package generation

// Beats: recurring Generations, fired by the Tick (ADR 0028) and gated on
// their owner's liveness. Traffic used to be the clock (ADR 0016); it now
// only records that somebody was here, and the Tick decides when to act on
// that. A Beat is still reached one owner at a time, never swept globally,
// so its due time stays a derived function rather than an indexed field.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// resumeTimeout bounds the store work a detached resume pass does.
// Generously above what a handful of reads needs: nothing is waiting on
// the goroutine, but it must not leak either.
const resumeTimeout = 30 * time.Second

// FireDue starts a Generation for each of one user's due Beats, at most
// budget of them. It reports how many it started and whether the budget
// stopped it with work still due.
//
// The budget is the Tick's bounded slice (ADR 0028). Firing only *starts*
// a Generation — fire claims the Beat, writes the checkpoint and hands
// off to Kick — so this bounds spend committed to, not episodes finished,
// which is both the number ADR 0016 worried about and the only quantity a
// request-shaped caller can bound. A budget of zero or less fires nothing
// and is not an error.
//
// What the budget skips is deferred, never lost: an unfired Beat's clock
// does not advance, so it is still due on the next pass and its window
// widens by the gap rule.
//
// A Beat that fails to fire is logged and skipped, and the first error is
// returned once the rest have had their turn: one bad Beat must not cost
// its owner the others.
func (r *Runner) FireDue(ctx context.Context, userID string, budget int) (fired int, truncated bool, err error) {
	if budget <= 0 {
		return 0, false, nil
	}
	beats, err := r.store.ListBeats(ctx, userID)
	if err != nil {
		return 0, false, err
	}
	var firstErr error
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
		// Checked here rather than at the top of the loop so truncation is
		// exact: it means a Beat that would have fired did not, never
		// merely that the list ran on past the budget.
		if fired >= budget {
			truncated = true
			break
		}
		started, err := r.fire(ctx, b, now)
		if err != nil {
			r.log.Error("beats: fire failed", "user", userID, "beat", b.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if started {
			fired++
		}
	}
	return fired, truncated, firstErr
}

// fire starts the Beat's Generation, advancing its clock first so the
// firing is claimed before any slow work begins. It reports whether a
// Generation was actually started: declining the claim and finding the
// Beat no longer due are both ordinary outcomes, not errors, but neither
// is a firing, and the Tick's budget and its status record both count
// firings.
//
// The claim has two layers. In this process, claimBeat serializes
// firings of one Beat and the Beat is re-read inside it, so concurrent
// Ticks — the hourly one and an admin pressing "run a pass now" —
// resolve to exactly one Episode. Across replicas it is the persisted
// clock alone, which is the same best-effort race Kick documents — and
// the same worst case: duplicated work and a suffixed slug from freeSlug.
func (r *Runner) fire(ctx context.Context, b store.Beat, now time.Time) (bool, error) {
	if !r.claimBeat(b) {
		return false, nil
	}
	defer r.releaseBeat(b)
	// Re-read under the claim: whoever held it before this may have just
	// fired, in which case the stored clock now says not due.
	fresh, err := r.store.GetBeat(ctx, b.UserID, b.ID)
	if err != nil {
		return false, err
	}
	if !fresh.Due(now) {
		return false, nil
	}
	b = fresh

	id, err := randomID()
	if err != nil {
		return false, err
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
		return false, err
	}
	if err := r.store.PutGeneration(ctx, g); err != nil {
		return false, err
	}
	r.Kick(g)
	return true, nil
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

// ResumeUser re-Kicks one user's unfinished Generations, detached from
// the request that asked for it.
//
// The attended surfaces call this — the Dashboard and the Beats page,
// where a person may be watching a stalled run and the next Tick is up to
// an hour away. It fires nothing: since ADR 0028 traffic does not fire
// Beats, and landing on a page no longer makes an overdue Beat un-overdue.
//
// Kick no-ops on anything this process is already running, so on a warm
// instance this costs one store read; on a fresh one it is what picks a
// run back up after Cloud Run reclaimed the instance that started it.
func (r *Runner) ResumeUser(userID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), resumeTimeout)
		defer cancel()

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
	}()
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
