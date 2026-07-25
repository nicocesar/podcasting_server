package generation

// Beats: recurring Generations, fired by traffic rather than by a clock
// (ADR 0016). Nothing in this server ticks — an idle Cloud Run service
// has no process at all — so a Beat is examined when its owner's Personal
// Feed is polled or their Dashboard opened. The Heartbeat is that moment.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// heartbeatTimeout bounds the store work a heartbeat does. Generously
// above what a handful of reads and writes needs: the goroutine is
// detached from the request, so nothing is waiting on it, but it must not
// leak either.
const heartbeatTimeout = 30 * time.Second

// Heartbeat brings one user's Beats up to date and revives their stalled
// Generations. Callers hand it a user ID from a request they were already
// serving and let it run in a goroutine — it never touches the response,
// and the runs it starts are on their own Background context (see run),
// so the request finishing cannot cancel them.
//
// This is the whole scheduler. Its honest limits: a Beat whose owner
// never polls never fires, and an Episode it starts lands in the *next*
// poll, not the one that triggered it.
func (r *Runner) Heartbeat(userID string) {
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
	defer cancel()

	r.fireDueBeats(ctx, userID)
	r.resumeStalled(ctx, userID)
}

// fireDueBeats starts one Generation per due Beat.
func (r *Runner) fireDueBeats(ctx context.Context, userID string) {
	beats, err := r.store.ListBeats(ctx, userID)
	if err != nil {
		r.log.Error("beats: list failed", "user", userID, "err", err)
		return
	}
	now := time.Now().UTC()
	for _, b := range beats {
		if !b.Due(now) {
			continue
		}
		if _, ok := TemplateByID(b.Template); !ok {
			// A template retired from the registry, or one this instance
			// cannot produce. Leave the Beat alone rather than pausing it:
			// another instance may have the composer configured.
			continue
		}
		if err := r.fire(ctx, b, now); err != nil {
			r.log.Error("beats: fire failed", "user", userID, "beat", b.ID, "err", err)
		}
	}
}

// fire starts the Beat's Generation, advancing its clock first so the
// firing is claimed before any slow work begins.
//
// The claim has two layers. In this process, claimBeat serializes
// firings of one Beat and the Beat is re-read inside it, so the six
// simultaneous requests a browser and a podcast client can produce
// resolve to exactly one Episode. Across replicas it is the persisted
// clock alone, which is the same best-effort race Kick documents — and
// the same worst case: duplicated work and a suffixed slug from freeSlug.
func (r *Runner) fire(ctx context.Context, b store.Beat, now time.Time) error {
	if !r.claimBeat(b) {
		return nil
	}
	defer r.releaseBeat(b)
	// Re-read under the claim: whoever held it before this may have just
	// fired, in which case the stored clock now says not due.
	fresh, err := r.store.GetBeat(ctx, b.UserID, b.ID)
	if err != nil {
		return err
	}
	if !fresh.Due(now) {
		return nil
	}
	b = fresh

	id, err := randomID()
	if err != nil {
		return err
	}
	g := store.Generation{
		UserID:         b.UserID,
		ID:             id,
		BeatID:         b.ID,
		Template:       b.Template,
		Topic:          b.Topic,
		LengthMinutes:  b.LengthMinutes,
		FreshnessDays:  b.FreshnessDays,
		AgeRange:       b.AgeRange,
		SaveCharacters: b.SaveCharacters,
		Cast:           b.Cast,
		Language:       b.Language,
		Voice:          b.Voice,
		Provider:       b.Provider,
		Stage:          store.GenResearching,
		Active:         true,
		CreatedAt:      now,
	}
	// The window stretches to the ground actually uncovered since the last
	// Episode, so a Beat coming back from a quiet week covers the week
	// rather than the day. Only for templates whose cadence is the window
	// — for the others the Freshness Window is not a field at all.
	if tpl, ok := TemplateByID(b.Template); ok && tpl.DerivesInterval {
		g.FreshnessDays = b.GapDays(now)
	}
	// Traced at creation, like the cast on a hand-started Generation:
	// it explains months later why an Episode nobody asked for exists,
	// and why its window was the width it was.
	r.trace(&g, store.LevelInfo, "beat.fired", "started by a Beat",
		"beat", b.ID, "interval_days", b.IntervalDays,
		"freshness_days", g.FreshnessDays, "episode_count", b.EpisodeCount)

	b.LastFiredAt = now
	if err := r.store.PutBeat(ctx, b); err != nil {
		return err
	}
	if err := r.store.PutGeneration(ctx, g); err != nil {
		return err
	}
	r.Kick(g)
	return nil
}

// claimBeat takes the in-process right to fire b, or reports that
// somebody else already has it. It shares the runner's lock and mirrors
// the running map that guards Kick — the same problem one level up.
func (r *Runner) claimBeat(b store.Beat) bool {
	key := b.UserID + "/" + b.ID
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firing[key] {
		return false
	}
	r.firing[key] = true
	return true
}

func (r *Runner) releaseBeat(b store.Beat) {
	key := b.UserID + "/" + b.ID
	r.mu.Lock()
	delete(r.firing, key)
	r.mu.Unlock()
}

// resumeStalled re-Kicks the user's unfinished Generations. Kick no-ops
// on anything this process is already running, so on a warm instance this
// costs one store read; on a fresh one it is what picks a run back up
// after Cloud Run reclaimed the instance that started it. A Beat's
// Generation has no browser polling it, so this is its only rescue.
func (r *Runner) resumeStalled(ctx context.Context, userID string) {
	gens, err := r.store.ListGenerations(ctx, userID)
	if err != nil {
		r.log.Error("beats: resume scan failed", "user", userID, "err", err)
		return
	}
	for _, g := range gens {
		if g.Active {
			r.Kick(g)
		}
	}
}

// recordBeatOutcome folds a finished run back into the Beat that started
// it: a success clears the failure streak and moves the window's anchor
// forward, a failure counts toward the pause. Best effort and a no-op for
// a hand-started Generation — a bookkeeping write must never turn a
// published Episode into a failed one.
func (r *Runner) recordBeatOutcome(ctx context.Context, g store.Generation, cause error) {
	if g.BeatID == "" {
		return
	}
	b, err := r.store.GetBeat(ctx, g.UserID, g.BeatID)
	if err != nil {
		// Cancelled mid-run is normal, not an error worth shouting about.
		r.log.Info("beats: outcome not recorded", "user", g.UserID, "beat", g.BeatID, "err", err)
		return
	}
	if cause == nil {
		b.LastSucceededAt = time.Now().UTC()
		b.ConsecutiveFailures = 0
		b.LastError = ""
		b.EpisodeCount++
	} else {
		b.ConsecutiveFailures++
		b.LastError = cause.Error()
		if b.ConsecutiveFailures >= store.BeatFailureLimit {
			b.Paused = true
			r.log.Warn("beats: paused after repeated failures", "user", b.UserID, "beat", b.ID,
				"failures", b.ConsecutiveFailures, "err", cause)
		}
	}
	if err := r.store.PutBeat(ctx, b); err != nil {
		r.log.Error("beats: could not record outcome", "user", b.UserID, "beat", b.ID, "err", err)
	}
}

// randomID mints an unguessable Generation ID, the same shape the HTTP
// layer uses for a hand-started one.
func randomID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
