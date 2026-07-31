package httpapi

// What the heartbeat became (ADR 0028). Traffic no longer fires anything:
// it says the owner is still listening, and the Tick decides when to act
// on that. The attended pages additionally revive a stalled run, because
// somebody is looking at one.

import (
	"context"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// livenessCoarsen is how stale an in-hand LastSeenAt has to be before a
// request rewrites it. A podcast client polls far more often than the
// Tick runs, and the Tick only ever asks a question measured in days, so
// anything finer is a write per poll for no answer that changes.
const livenessCoarsen = time.Hour

// livenessTimeout bounds the detached write. Generous for one read and
// one write; the point is only that the goroutine cannot leak.
const livenessTimeout = 10 * time.Second

// seen records that the User was here, which is the whole of what traffic
// now does.
//
// The staleness check reads the User the handler already resolved, so a
// poll inside the hour costs nothing at all: no store read, no goroutine,
// no write. That is what keeps the feed poll a read, which ADR 0027 went
// to some trouble to make it.
//
// The write itself is detached — a feed poll must not wait on it, and the
// request finishing must not cancel it. Unlike the old heartbeat there is
// no generator guard: liveness is recorded whether or not this instance
// can generate, so turning generation on later does not cost a day of
// everybody looking dormant.
func (s *server) seen(u store.User) {
	if time.Since(u.LastSeenAt) < livenessCoarsen {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), livenessTimeout)
		defer cancel()
		if err := s.store.TouchUser(ctx, u.ID, time.Now().UTC()); err != nil {
			s.log.Error("liveness: touch failed", "user", u.ID, "err", err)
		}
	}()
}

// resume re-Kicks the user's unfinished Generations, off the request path.
//
// The attended half of what the heartbeat used to be, and only for the
// attended surfaces: somebody is looking at this page, and a run Cloud Run
// stalled should not have to wait for the next Tick. The feed poll does
// not do this — a podcast client is not a person, and the Tick reaches
// every Active Generation hourly anyway.
func (s *server) resume(u store.User) {
	if s.generator == nil {
		return
	}
	s.generator.ResumeUser(u.ID)
}
