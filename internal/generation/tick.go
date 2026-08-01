package generation

// The Tick: the one request that does the work nobody asked for (ADR
// 0028). Traffic says whether work is warranted — it writes a liveness
// timestamp and nothing else — and the clock says when it runs.
//
// A method on Runner rather than its own package, because everything a
// pass does is already Runner's job and Runner already holds the store
// and the logger. When ADR 0029's Strand pass lands, the Tick starts
// doing station work that is not per-user generation, and that is the
// moment to reconsider extracting an internal/tick with a narrow
// interface. Not before.

import (
	"context"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const (
	// DefaultLivenessWindow is how recently a User must have been seen for
	// their Beats to fire.
	//
	// Seven days, and generous on purpose: the two failure modes are not
	// the same size. Too short stops somebody's briefing for having their
	// phone off over a weekend, silently, with nothing anywhere saying
	// why — a product failure that presents as the feature simply not
	// working. Too long spends a few more days on an account that was
	// going to be noticed anyway.
	DefaultLivenessWindow = 7 * 24 * time.Hour

	// DefaultBeatBudget caps the Beat firings one pass starts. Several
	// fully-loaded Users' worth of a whole day's Beats inside one hour:
	// far above any steady state this station has, and low enough that a
	// bug making every Beat look due costs twenty Episodes rather than the
	// corpus.
	DefaultBeatBudget = 20

	// DefaultUserScanLimit caps the liveness query, which the Beat budget
	// does not: without it a station with a hundred thousand live Users
	// would read all of them every hour to fire twenty Beats.
	DefaultUserScanLimit = 500

	// TriggerScheduler and TriggerAdmin are who asked, recorded on the
	// status so an operator can tell the hourly job from their own button.
	TriggerScheduler = "scheduler"
	TriggerAdmin     = "admin"
)

// TickOptions is the Tick's policy, all of it configuration. A zero value
// takes the defaults.
type TickOptions struct {
	LivenessWindow time.Duration
	BeatBudget     int
	UserScanLimit  int
	Trigger        string
}

func (o TickOptions) withDefaults() TickOptions {
	if o.LivenessWindow <= 0 {
		o.LivenessWindow = DefaultLivenessWindow
	}
	if o.BeatBudget <= 0 {
		o.BeatBudget = DefaultBeatBudget
	}
	if o.UserScanLimit <= 0 {
		o.UserScanLimit = DefaultUserScanLimit
	}
	if o.Trigger == "" {
		o.Trigger = TriggerScheduler
	}
	return o
}

// Tick does the work no request asks for: it resumes stalled Generations
// everywhere, and fires the due Beats of Users seen inside the Liveness
// Window, up to a bounded slice.
//
// It returns an error only for a failure that happened *before* any Beat
// was fired. Everything after that is folded into the returned status and
// reported as success — because the caller is Cloud Scheduler, a non-2xx
// is a retry, and a retry that re-fires Generations is the expensive
// failure mode rather than the cheap one. Being behind must look like
// success, and the next Tick catches up.
func (r *Runner) Tick(ctx context.Context, opt TickOptions) (store.TickStatus, error) {
	opt = opt.withDefaults()
	start := time.Now().UTC()
	status := store.TickStatus{At: start, Trigger: opt.Trigger}

	// The resume pass first, unconditionally and not liveness gated. A
	// stalled run has been dead for up to an hour, which makes it the most
	// latency-sensitive work here; and doing it first means the firings
	// below are not immediately re-Kicked by the same pass.
	//
	// A resume failure must not cost the hour's firings, so it is recorded
	// and stepped over rather than returned.
	resumed, err := r.ResumeAll(ctx)
	if err != nil {
		r.log.Error("tick: resume pass failed", "err", err)
		status.Error = err.Error()
	}
	status.Resumed = resumed

	// The one failure that legitimately answers non-2xx: nothing has been
	// fired, so a Scheduler retry is free and correct.
	users, err := r.store.ListUsersSeenSince(ctx, start.Add(-opt.LivenessWindow), opt.UserScanLimit)
	if err != nil {
		status.DurationMS = time.Since(start).Milliseconds()
		if status.Error == "" {
			status.Error = err.Error()
		}
		r.putTickStatus(ctx, status)
		return status, err
	}
	status.LiveUsers = len(users)

	remaining := opt.BeatBudget
	for _, u := range users {
		if remaining <= 0 {
			status.Truncated = true
			break
		}
		// A dying context ends the pass cleanly with an accurate record,
		// rather than as a run of logged cancellations.
		if ctx.Err() != nil {
			status.Truncated = true
			break
		}
		fired, truncated, err := r.FireDue(ctx, u, remaining)
		remaining -= fired
		status.BeatsFired += fired
		if truncated {
			status.Truncated = true
		}
		if err != nil {
			// One user's bad day is not the pass's: continue, never return.
			r.log.Error("tick: firing failed", "user", u.ID, "err", err)
			if status.Error == "" {
				status.Error = err.Error()
			}
		}
	}

	status.DurationMS = time.Since(start).Milliseconds()
	r.putTickStatus(ctx, status)
	r.log.Info("tick: done",
		"trigger", status.Trigger, "live_users", status.LiveUsers,
		"beats_fired", status.BeatsFired, "resumed", status.Resumed,
		"truncated", status.Truncated, "duration_ms", status.DurationMS,
		"err", status.Error)
	return status, nil
}

// putTickStatus records the pass, best effort. Losing the operator's
// dashboard line is never worth failing a Tick that fired something, for
// the retry that would follow.
func (r *Runner) putTickStatus(ctx context.Context, status store.TickStatus) {
	if err := r.store.PutTickStatus(ctx, status); err != nil {
		r.log.Error("tick: status write failed", "err", err)
	}
}
