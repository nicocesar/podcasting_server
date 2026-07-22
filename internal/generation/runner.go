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
	// Music composes the audio for templates whose IsMusic is set. Nil
	// when ELEVENLABS_API_KEY is unset, which also takes those templates
	// off the chooser (Templates) rather than letting them fail late.
	Music Composer
	// Model powers the agent, e.g. "claude-sonnet-5".
	Model  string
	Logger *slog.Logger
	// PollInterval overrides how often the agent session is polled
	// (default 5s; tests shorten it).
	PollInterval time.Duration
	// ComposeBackoff is the base delay between retries of a failed
	// movement, scaled by attempt number (default 5s; tests shorten it).
	ComposeBackoff time.Duration
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
// Composer renders one movement of instrumental audio. Narrow on
// purpose: the runner needs exactly this much of internal/music, and a
// test needs exactly this much to fake.
type Composer interface {
	Compose(ctx context.Context, prompt string, durationMS int) ([]byte, error)
	Model() string
}

type Runner struct {
	store   store.Store
	api     API
	engines []tts.Engine
	music   Composer
	model   string
	log     *slog.Logger

	poll           time.Duration
	composeBackoff time.Duration
	deleteSessions bool

	mu       sync.Mutex
	running  map[string]bool   // "{user}/{id}" → a goroutine owns it
	agentIDs map[string]string // agent name → ID, cached after provision
	envID    string
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
	backoff := cfg.ComposeBackoff
	if backoff <= 0 {
		backoff = composeBackoff
	}
	return &Runner{
		store:          cfg.Store,
		api:            cfg.API,
		engines:        cfg.Engines,
		music:          cfg.Music,
		model:          cfg.Model,
		log:            log,
		poll:           poll,
		composeBackoff: backoff,
		deleteSessions: cfg.DeleteSessions,
		running:        make(map[string]bool),
		agentIDs:       make(map[string]string),
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

// Bootstrap provisions every template's agent and the environment
// (idempotent: pushing the repo's system prompts is a no-op unless one
// changed, else a new agent version) and resumes any Generation a
// previous instance left unfinished. Errors are logged, not fatal:
// provisioning is retried on the first Kick.
func (r *Runner) Bootstrap(ctx context.Context) {
	for _, id := range r.AvailableTemplates() {
		tpl, _ := TemplateByID(id)
		if err := r.provision(ctx, tpl); err != nil {
			r.log.Warn("generation: bootstrap provisioning failed (will retry on first use)",
				"template", id, "err", err)
		}
	}
	gens, err := r.store.ListActiveGenerations(ctx)
	if err != nil {
		r.log.Error("generation: resume scan failed", "err", err)
		return
	}
	for _, g := range gens {
		// Traced, not just logged: a run that was interrupted and picked
		// up by another instance explains gaps in everything that follows.
		r.trace(&g, store.LevelNotice, "run.resumed", "resuming after restart", "stage", g.Stage)
		r.Kick(g)
	}
}

// AvailableTemplates lists the templates this instance can actually
// produce, in chooser order. A music template without a configured
// Composer is dropped rather than offered: unlike TTS there is no
// fallback chain behind it, so it would take the request, spend an agent
// session, and only then discover it cannot make a sound.
func (r *Runner) AvailableTemplates() []string {
	ids := make([]string, 0, len(TemplateIDs))
	for _, id := range TemplateIDs {
		if tpl, ok := TemplateByID(id); ok && tpl.IsMusic && r.music == nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// provision ensures the template's pre-baked agent + the shared
// environment exist and caches their IDs for the process lifetime.
func (r *Runner) provision(ctx context.Context, tpl Template) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.envID == "" {
		envID, err := r.api.EnsureEnvironment(ctx, environmentName)
		if err != nil {
			return fmt.Errorf("ensure environment: %w", err)
		}
		r.envID = envID
	}
	if r.agentIDs[tpl.AgentName] == "" {
		agentID, err := r.api.EnsureAgent(ctx, tpl.AgentName, r.model, tpl.SystemPrompt, tpl.Tools)
		if err != nil {
			return fmt.Errorf("ensure agent: %w", err)
		}
		r.agentIDs[tpl.AgentName] = agentID
		r.log.Info("generation: provisioned", "agent", agentID, "template", tpl.ID, "environment", r.envID)
	}
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
			//
			// Cheap for the spoken programs, at least. A composed piece
			// pays the vendor per movement, which is why composeMovement
			// retries in place rather than letting a blip get this far.
			if tpl, ok := TemplateByID(g.Template); ok && tpl.IsMusic {
				g, err = r.composeAndPublish(ctx, g)
			} else {
				g, err = r.voiceAndPublish(ctx, g)
			}
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
	r.trace(&g, store.LevelError, "stage.failed", "failed", "stage", g.Stage, "err", cause)
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
		r.trace(g, store.LevelWarn, "session.usage_failed", "could not fetch session usage",
			"session", g.SessionID, "err", err)
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
	tpl, ok := TemplateByID(g.Template)
	if !ok {
		return g, fmt.Errorf("unknown generation template %q", g.Template)
	}
	if err := r.provision(ctx, tpl); err != nil {
		return g, err
	}
	ctx, cancel := context.WithTimeout(ctx, researchTimeout)
	defer cancel()

	if g.SessionID == "" {
		r.mu.Lock()
		agentID := r.agentIDs[tpl.AgentName]
		r.mu.Unlock()
		id, err := r.api.CreateSession(ctx, agentID, r.envID, "episode: "+g.Topic)
		if err != nil {
			return g, fmt.Errorf("create session: %w", err)
		}
		g.SessionID = id
		// Traced before the checkpoint, not after, so the console link is
		// on the record from the first write — research can run for half an
		// hour before the next PutGeneration.
		r.traceURL(&g, store.LevelInfo, "session.created", "session created",
			"https://platform.claude.com/workspaces/default/sessions/"+id,
			"session", id)
		if err := r.store.PutGeneration(ctx, g); err != nil {
			return g, err
		}
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
			use, err := r.api.LastToolUse(ctx, g.SessionID, tpl.SubmitToolName)
			if err != nil {
				return g, fmt.Errorf("fetch agent output: %w", err)
			}
			if tpl.IsMusic {
				// The composer has no legacy contract to fall back on and
				// no language round-trip: it either submitted a plan or it
				// has not submitted yet.
				if use != nil {
					var done bool
					g, done, err = r.judgeComposition(ctx, g, use)
					if done || err != nil {
						return g, err
					}
					break // rejected: keep polling for the resubmission
				}
				if !sent {
					if err := r.api.SendMessage(ctx, g.SessionID, tpl.TaskMessage(g, time.Now())); err != nil {
						return g, fmt.Errorf("send task: %w", err)
					}
					sent = true
				}
				break
			}
			if use != nil {
				// Assigned, not shadowed: judgeSubmission records why it
				// rejected a submission onto the Generation, and a := here
				// would drop those entries on the floor every time it did
				// not accept — which is exactly when they matter.
				var done bool
				g, done, err = r.judgeSubmission(ctx, g, use, &translationRequested)
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
						r.trace(&g, store.LevelNotice, "script.translation_requested",
							"script in wrong language, translating",
							"got", lang, "want", g.Language, "path", "legacy")
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
				task := tpl.TaskMessage(g, time.Now())
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
			r.trace(&g, store.LevelNotice, "script.rejected",
				"submission rejected, asking for a resubmission", "reason", perr)
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
		r.trace(&g, store.LevelNotice, "script.translation_requested",
			"script in wrong language, translating",
			"got", lang, "want", g.Language, "path", "tool")
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

// judgeComposition answers one submit_music call, mirroring
// judgeSubmission: accept and checkpoint, or reject with what to fix and
// let the caller keep polling. An already-answered call is re-judged
// without re-answering — the crash-recovery and awaiting-resubmission
// paths in one.
func (r *Runner) judgeComposition(ctx context.Context, g store.Generation, use *ToolUse) (store.Generation, bool, error) {
	comp, perr := ParseMusicSubmission(use.Input, g.LengthMinutes)
	if perr != nil {
		if !use.Answered {
			r.trace(&g, store.LevelNotice, "composition.rejected",
				"submission rejected, asking for a resubmission", "reason", perr)
			reject := "Rejected: " + perr.Error() + ". Fix that and call submit_music again with the full plan."
			if err := r.api.SendToolResult(ctx, g.SessionID, use.ID, reject, true); err != nil {
				return g, false, fmt.Errorf("reject submission: %w", err)
			}
		}
		return g, false, nil
	}
	if !use.Answered {
		if err := r.api.SendToolResult(ctx, g.SessionID, use.ID, "Composition received and accepted. You are done.", false); err != nil {
			return g, false, fmt.Errorf("acknowledge submission: %w", err)
		}
	}
	g, err := r.acceptComposition(ctx, g, comp)
	return g, true, err
}

// acceptComposition checkpoints the Composition into the same Script
// field the spoken programs use. Sharing the field is deliberate: Retry
// keys off Script being non-empty to decide whether a failed Generation
// resumes at voicing or goes back to the agent, and a composed piece
// wants exactly that behavior — the plan is the expensive part.
func (r *Runner) acceptComposition(ctx context.Context, g store.Generation, comp Composition) (store.Generation, error) {
	raw, err := json.Marshal(comp)
	if err != nil {
		return g, err
	}
	r.recordSessionUsage(ctx, &g)
	r.trace(&g, store.LevelInfo, "composition.accepted", "composition accepted",
		"title", comp.Title, "movements", len(comp.Movements), "millis", comp.TotalMS())
	g.Script = string(raw)
	g.Stage = store.GenVoicing
	return g, r.store.PutGeneration(ctx, g)
}

// acceptScript checkpoints the Script: the durable midpoint after which
// research is never repeated (ADR 0009).
func (r *Runner) acceptScript(ctx context.Context, g store.Generation, script Script) (store.Generation, error) {
	raw, err := json.Marshal(script)
	if err != nil {
		return g, err
	}
	r.recordSessionUsage(ctx, &g)
	r.trace(&g, store.LevelInfo, "script.accepted", "script accepted",
		"title", script.Title, "language", script.Language, "sources", len(script.Sources))
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
	// Both callbacks are invoked synchronously by SynthesizeAll from this
	// goroutine, so mutating g (counters and trace alike) needs no lock.
	mp3, engine, attempts, err := tts.SynthesizeAll(ctx, tts.Prefer(r.engines, g.Provider), chunks, voice, func(done, total int) {
		g.VoicedChunks, g.TotalChunks = done, total
		if err := r.store.PutGeneration(ctx, g); err != nil {
			r.trace(&g, store.LevelWarn, "progress.checkpoint_failed", "progress checkpoint failed", "err", err)
		}
	}, func(name string, err error) {
		// requested_provider is the point of this entry: it turns "a
		// fallback happened" into "the listener asked for X and got Y".
		r.trace(&g, store.LevelWarn, "tts.fallback", "tts engine failed, trying next",
			"engine", name, "requested_provider", g.Provider, "err", err)
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
	r.trace(&g, store.LevelInfo, "tts.selected", "episode voiced",
		"engine", engine, "requested_provider", g.Provider, "attempts", attempts, "chunks", len(chunks))

	// Sign off out loud with the voice and engine that actually read the
	// episode — on Auto that is whatever survived the fallback chain, and
	// this is the only trace a listener gets. Voiced by the winning engine
	// so the credit is in the same voice as the episode. Non-fatal: the
	// script is already synthesized and paid for, and losing the credit is
	// not worth losing the episode.
	if credit := tts.Credit(engine, voice); credit != "" {
		if e := tts.ByName(r.engines, engine); e != nil {
			outro, err := e.Synthesize(ctx, credit, voice)
			switch {
			case err != nil:
				r.trace(&g, store.LevelNotice, "tts.credit_failed", "credit outro failed, publishing without it",
					"engine", engine, "err", err)
			case len(outro) == 0:
				r.trace(&g, store.LevelNotice, "tts.credit_failed", "credit outro returned no audio",
					"engine", engine, "reason", "no audio")
			default:
				mp3 = append(mp3, outro...)
				g.TTSCharacters += utf8.RuneCountInString(credit)
			}
		}
	}

	g, ep, err := r.publishAudio(ctx, g, script.Title, script.Description(), mp3)
	if err != nil {
		return g, err
	}
	slug := ep.Slug

	// The cast extraction the form asked for. Non-fatal by design: the
	// Episode is already published, and the backfill button covers a
	// missed extraction.
	if g.SaveCharacters {
		chars, u, err := ExtractCharacters(ctx, r.api, script.Script)
		// Extraction burned real tokens either way; fold them into the
		// meters SessionsCount-free (statsLabel keys off sessions).
		g.InputTokens += u.InputTokens
		g.OutputTokens += u.OutputTokens
		g.CacheReadTokens += u.CacheReadTokens
		if err != nil {
			r.trace(&g, store.LevelWarn, "characters.extraction_failed", "character extraction failed",
				"episode", slug, "err", err)
		} else {
			ep.Characters = chars
			if err := r.store.UpdateEpisode(ctx, ep); err != nil {
				r.trace(&g, store.LevelWarn, "characters.save_failed", "could not save characters",
					"episode", slug, "count", len(chars), "err", err)
			} else {
				r.trace(&g, store.LevelInfo, "characters.extracted", "characters extracted",
					"episode", slug, "count", len(chars), "names", characterNames(chars))
			}
		}
	}

	return r.finish(ctx, g, slug)
}

// composeAttempts bounds the retries around a single movement. Each
// movement is paid for on success, so a transient failure on the last one
// must not throw away the movements already rendered — that is the whole
// reason this loop retries in place rather than failing the run.
const composeAttempts = 3

// composeBackoff is the base delay between those attempts. Generous: the
// failures worth retrying are upstream capacity blips, and the run has
// forty-five minutes to play with.
const composeBackoff = 5 * time.Second

// composeAndPublish is voiceAndPublish's counterpart for the templates
// whose audio is composed. Movements are rendered in order and appended
// as raw MP3 frames — the same concatenation the TTS path relies on,
// valid because the music client pins the identical mp3_44100_128 format.
func (r *Runner) composeAndPublish(ctx context.Context, g store.Generation) (store.Generation, error) {
	if r.music == nil {
		return g, fmt.Errorf("no music client configured")
	}
	var comp Composition
	if err := json.Unmarshal([]byte(g.Script), &comp); err != nil {
		return g, fmt.Errorf("stored composition is corrupt: %w", err)
	}
	if len(comp.Movements) == 0 {
		return g, fmt.Errorf("stored composition has no movements")
	}

	g.Stage = store.GenVoicing
	g.VoicedChunks, g.TotalChunks = 0, len(comp.Movements)
	g.MusicModel = r.music.Model()
	if err := r.store.PutGeneration(ctx, g); err != nil {
		return g, err
	}

	var mp3 []byte
	for i, m := range comp.Movements {
		piece, calls, err := r.composeMovement(ctx, &g, i, m)
		// Calls are metered even when the movement ultimately failed:
		// rejected attempts still cost, and run() persists g with the
		// failure record.
		g.MusicCalls += calls
		if err != nil {
			return g, fmt.Errorf("composing movement %d/%d: %w", i+1, len(comp.Movements), err)
		}
		g.MusicMillis += m.DurationMS
		mp3 = append(mp3, piece...)

		g.VoicedChunks = i + 1
		if err := r.store.PutGeneration(ctx, g); err != nil {
			r.trace(&g, store.LevelWarn, "progress.checkpoint_failed", "progress checkpoint failed", "err", err)
		}
	}
	r.trace(&g, store.LevelInfo, "music.composed", "piece composed",
		"model", g.MusicModel, "movements", len(comp.Movements),
		"millis", g.MusicMillis, "calls", g.MusicCalls)

	// No credit outro: tts.Credit names the engine and voice that read the
	// episode, and nothing here was read. A spoken sign-off would also be
	// the one voice in a track that is meant to have none.
	g, ep, err := r.publishAudio(ctx, g, comp.Title, comp.Description(), mp3)
	if err != nil {
		return g, err
	}
	return r.finish(ctx, g, ep.Slug)
}

// composeMovement renders one movement, retrying transient failures in
// place. It reports how many calls it made so the meter counts every
// request, not just the one that worked.
func (r *Runner) composeMovement(ctx context.Context, g *store.Generation, i int, m Movement) ([]byte, int, error) {
	var lastErr error
	for attempt := 1; attempt <= composeAttempts; attempt++ {
		piece, err := r.music.Compose(ctx, m.Prompt, m.DurationMS)
		if err == nil {
			return piece, attempt, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, attempt, lastErr
		}
		r.trace(g, store.LevelWarn, "music.retry", "movement failed, retrying",
			"movement", i+1, "attempt", attempt, "of", composeAttempts, "err", err)
		if attempt < composeAttempts {
			select {
			case <-ctx.Done():
				return nil, attempt, lastErr
			case <-time.After(time.Duration(attempt) * r.composeBackoff):
			}
		}
	}
	return nil, composeAttempts, lastErr
}

// publishAudio is the publish half both pipelines share: whatever
// produced the bytes — a voiced script or a composed piece — from here on
// an episode is an episode. Returns the stored Episode, whose Slug is the
// one that survived collision resolution.
func (r *Runner) publishAudio(ctx context.Context, g store.Generation, title, description string, mp3 []byte) (store.Generation, store.Episode, error) {
	g.Stage = store.GenPublishing
	if err := r.store.PutGeneration(ctx, g); err != nil {
		return g, store.Episode{}, err
	}
	slug, err := r.freeSlug(ctx, &g)
	if err != nil {
		return g, store.Episode{}, err
	}
	tpl, _ := TemplateByID(g.Template)
	ep := store.Episode{
		OwnerID:     g.UserID,
		Slug:        slug,
		Title:       title,
		Description: description,
		PublishedAt: time.Now().UTC(),
		AudioType:   "audio/mpeg",
		Template:    tpl.ID,
	}
	// Same courtesy the Publishing Contract extends (ADR 0004): estimate
	// the duration from the MP3 frames; failure is non-fatal.
	if d, err := audio.MP3Duration(bytes.NewReader(mp3)); err == nil {
		ep.DurationSec = int(d.Round(time.Second).Seconds())
	}
	ep, err = r.store.UpsertEpisode(ctx, ep, bytes.NewReader(mp3))
	if err != nil {
		return g, store.Episode{}, fmt.Errorf("publish: %w", err)
	}
	return g, ep, nil
}

// finish closes out a successful run: the agent's output is checkpointed
// server-side, so the session can go once the Episode is safe — but only
// when configured to, since kept sessions remain inspectable in the
// Anthropic Console for prompt work.
func (r *Runner) finish(ctx context.Context, g store.Generation, slug string) (store.Generation, error) {
	if r.deleteSessions && g.SessionID != "" {
		if err := r.api.DeleteSession(ctx, g.SessionID); err != nil {
			r.trace(&g, store.LevelWarn, "session.delete_failed", "could not delete session",
				"session", g.SessionID, "err", err)
		}
	}
	g.EpisodeSlug = slug
	g.Stage = store.GenDone
	g.Active = false
	return g, r.store.PutGeneration(ctx, g)
}

// ExtractCharacters distills a story script's cast for the HTTP layer's
// backfill endpoint, keeping the raw API out of httpapi.
func (r *Runner) ExtractCharacters(ctx context.Context, script string) ([]store.Character, error) {
	chars, _, err := ExtractCharacters(ctx, r.api, script)
	return chars, err
}

// freeSlug is YYYY-MM-DD-<topic slug>, suffixed -2, -3, … until free, so
// a new Generation never silently replaces an earlier Episode through
// the republish semantics of ADR 0002.
func (r *Runner) freeSlug(ctx context.Context, g *store.Generation) (string, error) {
	base := time.Now().UTC().Format("2006-01-02") + "-" + Slugify(g.Topic)
	slug := base
	for i := 2; ; i++ {
		_, err := r.store.GetEpisode(ctx, g.UserID, slug)
		if errors.Is(err, store.ErrNotFound) {
			if slug != base {
				// Worth surfacing: the listener asked for a topic they have
				// covered before, and the episode published under a name
				// they did not choose.
				r.trace(g, store.LevelNotice, "publish.slug_collision",
					"episode name was taken, published under a numbered slug",
					"base", base, "chosen", slug)
			}
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

// characterNames joins a cast for a trace detail field, where a compact
// string beats a structure the entry cannot hold anyway.
func characterNames(chars []store.Character) string {
	names := make([]string, len(chars))
	for i, c := range chars {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}
