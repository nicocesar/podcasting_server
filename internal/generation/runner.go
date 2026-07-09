package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

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

	poll time.Duration

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
		store:   cfg.Store,
		api:     cfg.API,
		engines: cfg.Engines,
		model:   cfg.Model,
		log:     log,
		poll:    poll,
		running: make(map[string]bool),
	}
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
	g.Stage = store.GenFailed
	g.Active = false
	g.Error = cause.Error()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.store.PutGeneration(ctx, g); err != nil {
		r.log.Error("generation: could not record failure", "user", g.UserID, "id", g.ID, "err", err)
	}
}

// research drives the managed-agent session to a parsed Script. Resume-
// safe at every crash point: no session yet → create one; session idle
// with no reply → (re)send the task; agent already replied → parse it.
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
	}

	sent := false
	for {
		status, err := r.api.SessionStatus(ctx, g.SessionID)
		if err != nil {
			return g, fmt.Errorf("session status: %w", err)
		}
		switch status {
		case "terminated":
			return g, fmt.Errorf("agent session terminated")
		case "idle":
			msg, err := r.api.LastAgentMessage(ctx, g.SessionID)
			if err != nil {
				return g, fmt.Errorf("fetch agent output: %w", err)
			}
			if msg != "" {
				script, err := ParseScript(msg)
				if err != nil {
					return g, err
				}
				raw, err := json.Marshal(script)
				if err != nil {
					return g, err
				}
				g.Script = string(raw)
				g.Stage = store.GenVoicing
				return g, r.store.PutGeneration(ctx, g)
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

func (r *Runner) voiceAndPublish(ctx context.Context, g store.Generation) (store.Generation, error) {
	var script Script
	if err := json.Unmarshal([]byte(g.Script), &script); err != nil {
		return g, fmt.Errorf("stored script is corrupt: %w", err)
	}
	voice, ok := tts.VoiceFor(g.Language)
	if !ok {
		return g, fmt.Errorf("no voice for language %q", g.Language)
	}

	chunks := tts.Split(script.Script)
	g.Stage = store.GenVoicing
	g.VoicedChunks, g.TotalChunks = 0, len(chunks)
	if err := r.store.PutGeneration(ctx, g); err != nil {
		return g, err
	}
	mp3, err := tts.SynthesizeAll(ctx, r.engines, chunks, voice, func(done, total int) {
		g.VoicedChunks, g.TotalChunks = done, total
		if err := r.store.PutGeneration(ctx, g); err != nil {
			r.log.Warn("generation: progress checkpoint failed", "user", g.UserID, "id", g.ID, "err", err)
		}
	})
	if err != nil {
		return g, fmt.Errorf("voicing: %w", err)
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

	// The Script is checkpointed server-side, so the session — and with
	// it what Anthropic retains — can go. Best effort.
	if g.SessionID != "" {
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
