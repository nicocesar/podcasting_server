package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nicocesar/podcasting_server/internal/audio"
	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

const (
	agentName       = "podcasting-generator"
	environmentName = "podcasting-generator-env"

	// researchTimeout bounds the agent session; runTimeout the whole
	// pipeline. A 60-minute episode is ~9k words of research + writing.
	researchTimeout = 30 * time.Minute
	runTimeout      = 45 * time.Minute
	pollInterval    = 5 * time.Second
)

type Config struct {
	Store store.Store
	API   API
	// Engines are tried in order per episode (edge-tts first, Google
	// fallback); see internal/tts.
	Engines []tts.Engine
	// Model powers the agent, e.g. "claude-sonnet-5".
	Model  string
	Logger *slog.Logger
	// PollInterval overrides how often the agent session is polled
	// (default 5s; tests shorten it).
	PollInterval time.Duration
	// DeleteSessions removes each agent session once its Episode is
	// published. Off by default: kept sessions stay inspectable in the
	// Anthropic Console, which is how the prompts get improved.
	DeleteSessions bool
}

// Runner drives Generations through their stages. It is the checkpointed
// in-process worker of ADR 0009: all state worth keeping is in the
// store.Generation record, so a restarted instance resumes from there
// (ResumeAll); only in-flight audio bytes are lost, and those are cheap
// to redo.
type Runner struct {
	store   store.Store
	api     API
	engines []tts.Engine
	model   string
	log     *slog.Logger

	poll           time.Duration
	deleteSessions bool

	mu      sync.Mutex
	running map[string]bool // "{user}/{id}" → a goroutine owns it
	agentID string          // cached after provision
	envID   string
}

func NewRunner(cfg Config) *Runner {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = pollInterval
	}
	return &Runner{
		store:          cfg.Store,
		api:            cfg.API,
		engines:        cfg.Engines,
		model:          cfg.Model,
		log:            log,
		poll:           poll,
		deleteSessions: cfg.DeleteSessions,
		running:        make(map[string]bool),
	}
}

// EngineNames lists the configured TTS engines in chain order, for the
// voice-provider dropdown on /me/generate. Only engines that actually
// initialized appear. Lock-free by design: engines is set once in
// NewRunner and never mutated (tts.Prefer copies rather than reorders).
func (r *Runner) EngineNames() []string {
	names := make([]string, len(r.engines))
	for i, e := range r.engines {
		names[i] = e.Name()
	}
	return names
}

// Bootstrap provisions the agent and environment (idempotent: pushing
// the repo's system prompt is a no-op unless it changed, else a new
// agent version) and resumes any Generation a previous instance left
// unfinished. Errors are logged, not fatal: provisioning is retried on
// the first Kick.
func (r *Runner) Bootstrap(ctx context.Context) {
	if err := r.provision(ctx); err != nil {
		r.log.Warn("generation: bootstrap provisioning failed (will retry on first use)", "err", err)
	}
	gens, err := r.store.ListActiveGenerations(ctx)
	if err != nil {
		r.log.Error("generation: resume scan failed", "err", err)
		return
	}
	for _, g := range gens {
		r.log.Info("generation: resuming", "user", g.UserID, "id", g.ID, "stage", g.Stage)
		r.Kick(g)
	}
}

// provision ensures the pre-baked agent + environment exist and caches
// their IDs for the process lifetime.
func (r *Runner) provision(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agentID != "" && r.envID != "" {
		return nil
	}
	agentID, err := r.api.EnsureAgent(ctx, agentName, r.model, systemPrompt)
	if err != nil {
		return fmt.Errorf("ensure agent: %w", err)
	}
	envID, err := r.api.EnsureEnvironment(ctx, environmentName)
	if err != nil {
		return fmt.Errorf("ensure environment: %w", err)
	}
	r.agentID, r.envID = agentID, envID
	r.log.Info("generation: provisioned", "agent", agentID, "environment", envID)
	return nil
}

// Kick starts (or resumes) the pipeline for g in a goroutine, unless one
// is already running it in this process. Concurrent replicas could in
// principle both resume the same Generation after a deploy; at this
// scale the worst case is duplicated work and a suffixed slug.
func (r *Runner) Kick(g store.Generation) {
	key := g.UserID + "/" + g.ID
	r.mu.Lock()
	if r.running[key] {
		r.mu.Unlock()
		return
	}
	r.running[key] = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.running, key)
			r.mu.Unlock()
		}()
		r.run(g)
	}()
}

// Retry re-arms a failed Generation from its last completed checkpoint:
// a stored Script skips straight to voicing (research is never re-paid
// for a TTS failure); otherwise a fresh session starts over.
func (r *Runner) Retry(ctx context.Context, g store.Generation) (store.Generation, error) {
	if g.Stage != store.GenFailed {
		return g, fmt.Errorf("generation is not failed")
	}
	g.Error = ""
	g.Active = true
	g.VoicedChunks = 0
	if g.Script != "" {
		g.Stage = store.GenVoicing
	} else {
		g.Stage = store.GenResearching
		g.SessionID = "" // the old session produced nothing usable
	}
	if err := r.store.PutGeneration(ctx, g); err != nil {
		return g, err
	}
	r.Kick(g)
	return g, nil
}

func (r *Runner) run(g store.Generation) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	start := time.Now()

	var err error
	for err == nil {
		switch g.Stage {
		case store.GenResearching:
			g, err = r.research(ctx, g)
		case store.GenVoicing, store.GenPublishing:
			// One resumable unit: audio lives only in memory, so a
			// crash mid-publish restarts from voicing. The Script
			// checkpoint makes that cheap.
			g, err = r.voiceAndPublish(ctx, g)
		case store.GenDone:
			r.log.Info("generation: done",
				"user", g.UserID, "id", g.ID, "episode", g.EpisodeSlug,
				"took", time.Since(start).Round(time.Second).String())
			return
		default:
			return // failed (or unknown): nothing to drive
		}
	}
	r.fail(g, err)
}

// fail records the failure on its own context: the run context may be
// what just expired.
func (r *Runner) fail(g store.Generation, cause error) {
	r.log.Error("generation: failed", "user", g.UserID, "id", g.ID, "stage", g.Stage, "err", cause)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A failed research session still burned tokens; meter it before the
	// record freezes. (Research success meters in research; a session
	// that failed here is abandoned by Retry, so this counts it exactly
	// once.)
	if g.Stage == store.GenResearching && g.SessionID != "" {
		r.recordSessionUsage(ctx, &g)
	}
	g.Stage = store.GenFailed
	g.Active = false
	g.Error = cause.Error()
	if err := r.store.PutGeneration(ctx, g); err != nil {
		r.log.Error("generation: could not record failure", "user", g.UserID, "id", g.ID, "err", err)
	}
}

// recordSessionUsage folds the session's aggregate token consumption into
// the Generation's lifetime meters. Best effort: metering never fails a
// pipeline that otherwise worked.
func (r *Runner) recordSessionUsage(ctx context.Context, g *store.Generation) {
	u, err := r.api.SessionUsage(ctx, g.SessionID)
	if err != nil {
		r.log.Warn("generation: could not fetch session usage",
			"user", g.UserID, "id", g.ID, "session", g.SessionID, "err", err)
		return
	}
	g.SessionsCount++
	g.InputTokens += u.InputTokens
	g.OutputTokens += u.OutputTokens
	g.CacheReadTokens += u.CacheReadTokens
	g.CacheWriteTokens += u.CacheWriteTokens
}

// research drives the managed-agent session to a Script in the requested
// language, delivered through the submit_episode tool: the agent calls
// it, the session blocks awaiting our tool result, and the result either
// accepts (ack, checkpoint) or rejects with what to fix (the agent then
// resubmits). Resume-safe at every crash point: no session yet → create
// one; session idle with no submission → (re)send the task; submission
// pending → judge it; accepted-but-uncheckpointed → judge it again, the
// answered ack is simply not re-sent. A script that comes back in the
// wrong language (research sources often set the agent's tongue) gets
// one rejection round before the pipeline gives up.
func (r *Runner) research(ctx context.Context, g store.Generation) (store.Generation, error) {
	if err := r.provision(ctx); err != nil {
		return g, err
	}
	ctx, cancel := context.WithTimeout(ctx, researchTimeout)
	defer cancel()

	if g.SessionID == "" {
		id, err := r.api.CreateSession(ctx, r.agentID, r.envID, "episode: "+g.Topic)
		if err != nil {
			return g, fmt.Errorf("create session: %w", err)
		}
		g.SessionID = id
		if err := r.store.PutGeneration(ctx, g); err != nil {
			return g, err
		}
		r.log.Info("generation: session created",
			"user", g.UserID, "id", g.ID, "session", id,
			"trace", "https://platform.claude.com/workspaces/default/sessions/"+id)
	}

	sent := false
	translationRequested := false
	for {
		status, err := r.api.SessionStatus(ctx, g.SessionID)
		if err != nil {
			return g, fmt.Errorf("session status: %w", err)
		}
		switch status {
		case "terminated":
			return g, fmt.Errorf("agent session terminated")
		case "idle":
			use, err := r.api.LastToolUse(ctx, g.SessionID, submitToolName)
			if err != nil {
				return g, fmt.Errorf("fetch agent output: %w", err)
			}
			if use != nil {
				g, done, err := r.judgeSubmission(ctx, g, use, &translationRequested)
				if done || err != nil {
					return g, err
				}
				break // rejected: keep polling for the resubmission
			}
			// No tool call. A fenced message is a session from before
			// the submit_episode tool (in flight across the deploy),
			// still following the legacy contract; anything else is
			// narration, not a delivery.
			msg, err := r.api.LastAgentMessage(ctx, g.SessionID)
			if err != nil {
				return g, fmt.Errorf("fetch agent output: %w", err)
			}
			if script, perr := ParseScript(msg); msg != "" && perr == nil {
				// An unreported language (older agent version) is taken
				// on faith; a reported mismatch gets a translation pass.
				if lang := PrimaryTag(script.Language); lang != "" && lang != g.Language {
					if !translationRequested {
						r.log.Info("generation: script in wrong language, translating",
							"user", g.UserID, "id", g.ID, "got", lang, "want", g.Language)
						if err := r.api.SendMessage(ctx, g.SessionID, translateMessage(g.Language)); err != nil {
							return g, fmt.Errorf("request translation: %w", err)
						}
						translationRequested = true
					}
					// Requested already: the latest message is still the
					// untranslated one — keep polling for the new reply.
					break
				}
				return r.acceptScript(ctx, g, script)
			} else if strings.Contains(msg, "```json") {
				return g, perr // a legacy delivery that is broken JSON
			}
			if !sent {
				task := userMessage(g.Topic, g.LengthMinutes, g.FreshnessDays, g.Language, time.Now())
				if err := r.api.SendMessage(ctx, g.SessionID, task); err != nil {
					return g, fmt.Errorf("send task: %w", err)
				}
				sent = true
			}
		}
		select {
		case <-ctx.Done():
			return g, fmt.Errorf("research timed out: %w", ctx.Err())
		case <-time.After(r.poll):
		}
	}
}

// judgeSubmission answers one submit_episode call: accept (ack + Script
// checkpoint, done=true) or reject with what to fix (done=false, the
// caller keeps polling for the resubmission). An already-answered call
// is re-judged without re-answering — that is the crash-recovery path
// (accepted but not yet checkpointed) and the already-rejected path
// (waiting for the resubmission) in one.
func (r *Runner) judgeSubmission(ctx context.Context, g store.Generation, use *ToolUse, translationRequested *bool) (store.Generation, bool, error) {
	script, perr := ParseSubmission(use.Input)
	if perr != nil {
		if !use.Answered {
			reject := "Rejected: " + perr.Error() + ". Fix that and call submit_episode again with the full episode."
			if err := r.api.SendToolResult(ctx, g.SessionID, use.ID, reject, true); err != nil {
				return g, false, fmt.Errorf("reject submission: %w", err)
			}
		}
		return g, false, nil
	}
	// An unreported language is taken on faith; a reported mismatch gets
	// one rejection round (research sources often set the agent's tongue).
	if lang := PrimaryTag(script.Language); lang != "" && lang != g.Language {
		if use.Answered {
			// This is the submission we already rejected (possibly before
			// a restart): the resubmission is still coming.
			*translationRequested = true
			return g, false, nil
		}
		if *translationRequested {
			return g, false, fmt.Errorf("script is still in %q after a translation round (want %q)", lang, g.Language)
		}
		r.log.Info("generation: script in wrong language, translating",
			"user", g.UserID, "id", g.ID, "got", lang, "want", g.Language)
		if err := r.api.SendToolResult(ctx, g.SessionID, use.ID, wrongLanguageResult(g.Language), true); err != nil {
			return g, false, fmt.Errorf("request translation: %w", err)
		}
		*translationRequested = true
		return g, false, nil
	}
	if !use.Answered {
		if err := r.api.SendToolResult(ctx, g.SessionID, use.ID, "Episode received and accepted. You are done.", false); err != nil {
			return g, false, fmt.Errorf("acknowledge submission: %w", err)
		}
	}
	g, err := r.acceptScript(ctx, g, script)
	return g, true, err
}

// acceptScript checkpoints the Script: the durable midpoint after which
// research is never repeated (ADR 0009).
func (r *Runner) acceptScript(ctx context.Context, g store.Generation, script Script) (store.Generation, error) {
	raw, err := json.Marshal(script)
	if err != nil {
		return g, err
	}
	r.recordSessionUsage(ctx, &g)
	g.Script = string(raw)
	g.Stage = store.GenVoicing
	return g, r.store.PutGeneration(ctx, g)
}

func (r *Runner) voiceAndPublish(ctx context.Context, g store.Generation) (store.Generation, error) {
	var script Script
	if err := json.Unmarshal([]byte(g.Script), &script); err != nil {
		return g, fmt.Errorf("stored script is corrupt: %w", err)
	}
	voice, ok := tts.VoiceFor(g.Language, g.Voice)
	if !ok {
		return g, fmt.Errorf("no voice for language %q", g.Language)
	}

	chunks := tts.Split(script.Script)
	g.Stage = store.GenVoicing
	g.VoicedChunks, g.TotalChunks = 0, len(chunks)
	if err := r.store.PutGeneration(ctx, g); err != nil {
		return g, err
	}
	mp3, engine, attempts, err := tts.SynthesizeAll(ctx, tts.Prefer(r.engines, g.Provider), chunks, voice, func(done, total int) {
		g.VoicedChunks, g.TotalChunks = done, total
		if err := r.store.PutGeneration(ctx, g); err != nil {
			r.log.Warn("generation: progress checkpoint failed", "user", g.UserID, "id", g.ID, "err", err)
		}
	})
	// Attempts accumulate even on failure (run() persists g with the
	// failure record); characters count only what the winning engine
	// actually voiced.
	g.TTSAttempts += attempts
	if err != nil {
		return g, fmt.Errorf("voicing: %w", err)
	}
	g.TTSEngine = engine
	for _, chunk := range chunks {
		g.TTSCharacters += utf8.RuneCountInString(chunk)
	}

	g.Stage = store.GenPublishing
	if err := r.store.PutGeneration(ctx, g); err != nil {
		return g, err
	}
	slug, err := r.freeSlug(ctx, g.UserID, g.Topic)
	if err != nil {
		return g, err
	}
	ep := store.Episode{
		OwnerID:     g.UserID,
		Slug:        slug,
		Title:       script.Title,
		Description: script.Description(),
		PublishedAt: time.Now().UTC(),
		AudioType:   "audio/mpeg",
	}
	// Same courtesy the Publishing Contract extends (ADR 0004): estimate
	// the duration from the MP3 frames; failure is non-fatal.
	if d, err := audio.MP3Duration(bytes.NewReader(mp3)); err == nil {
		ep.DurationSec = int(d.Round(time.Second).Seconds())
	}
	if _, err := r.store.UpsertEpisode(ctx, ep, bytes.NewReader(mp3)); err != nil {
		return g, fmt.Errorf("publish: %w", err)
	}

	// The Script is checkpointed server-side, so the session can go once
	// the Episode is safe — but only when configured to: kept sessions
	// remain inspectable in the Anthropic Console for prompt work.
	if r.deleteSessions && g.SessionID != "" {
		if err := r.api.DeleteSession(ctx, g.SessionID); err != nil {
			r.log.Warn("generation: could not delete session", "session", g.SessionID, "err", err)
		}
	}

	g.EpisodeSlug = slug
	g.Stage = store.GenDone
	g.Active = false
	return g, r.store.PutGeneration(ctx, g)
}

// freeSlug is YYYY-MM-DD-<topic slug>, suffixed -2, -3, … until free, so
// a new Generation never silently replaces an earlier Episode through
// the republish semantics of ADR 0002.
func (r *Runner) freeSlug(ctx context.Context, userID, topic string) (string, error) {
	base := time.Now().UTC().Format("2006-01-02") + "-" + Slugify(topic)
	slug := base
	for i := 2; ; i++ {
		_, err := r.store.GetEpisode(ctx, userID, slug)
		if errors.Is(err, store.ErrNotFound) {
			return slug, nil
		}
		if err != nil {
			return "", err
		}
		if i > 100 {
			return "", fmt.Errorf("no free slug near %q", base)
		}
		slug = base + "-" + strconv.Itoa(i)
	}
}
