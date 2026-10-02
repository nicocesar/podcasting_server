package httpapi

// POST /work/generations/{user}/{id}: a Generation's run, held inside a
// request (ADR 0035). The runner's own Kick is the only caller. Cloud Run
// allocates CPU to an instance while it has a request in flight, so a run
// that is the request keeps its CPU without the service paying for an
// always-allocated instance around the clock.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// workAuth admits TICK_TOKEN and nothing else. The token is the one the
// station already gives its own unattended work (ADR 0028), and this route
// can do strictly less with it than /tick can: it only carries on a
// Generation somebody already started. No session path — nobody needs to
// press this, and an admin who wants a stalled run moving has "run a pass
// now".
func (s *server) workAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hasTickToken(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *server) handleWork(w http.ResponseWriter, r *http.Request) {
	if s.generator == nil {
		http.Error(w, "generation is not configured on this server", http.StatusServiceUnavailable)
		return
	}
	// The lease read and write on their own short context; the run itself
	// is on the runner's. Neither is the request's: a dispatcher that gives
	// up must not cancel a run between two checkpoints.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ran, err := s.generator.WorkLeased(ctx, r.PathValue("user"), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		s.fail(w, err)
	case !ran:
		// Finished, or running somewhere else. Either way there is
		// nothing for this request to do, and the dispatcher knows the
		// difference does not matter to it.
		w.WriteHeader(http.StatusConflict)
	default:
		w.WriteHeader(http.StatusOK)
	}
}
