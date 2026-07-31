package httpapi

// POST /tick: the clock (ADR 0028). Cloud Scheduler calls it hourly with
// TICK_TOKEN; an admin can call it from the admin page, which doubles as
// the laptop story.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/store"
)

// tickTimeout bounds one pass. The work is bounded store I/O — the
// Generations it starts run on their own contexts and outlive it — so
// minutes is generous, and Cloud Scheduler's own deadline is the real
// ceiling anyway.
const tickTimeout = 2 * time.Minute

// hasTickToken reports whether the request carries TICK_TOKEN, compared
// as a digest in constant time — the same shape as hasAdminToken.
//
// The tickEnabled guard is the one thing hasAdminToken does not need:
// New refuses an empty AdminToken, so its digest is never the digest of
// "". TICK_TOKEN is optional, and without this an unset one would make
// the empty Bearer token a valid credential.
func (s *server) hasTickToken(r *http.Request) bool {
	if !s.tickEnabled {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], s.tickHash[:]) == 1
}

// tickAuth admits TICK_TOKEN or a logged-in admin, mirroring
// adminOrToken. The token path is the scheduler; the session path is an
// admin pressing "run a pass now", and is what makes the feature exist on
// a laptop with no scheduler at all.
//
// Deliberately s.session and not s.auth: a Tick is unattended spend on a
// timer, which is exactly what ADR 0010 keeps out of a Generator
// credential's reach and why ADR 0016 made the Beats routes session-only.
// A leaked API Key that could make the station spend hourly would be a
// privilege escalation, not an over-broad read.
func (s *server) tickAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.hasTickToken(r) {
			h(w, r)
			return
		}
		s.session(func(w http.ResponseWriter, r *http.Request, u store.User) {
			if !u.Admin {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		})(w, r)
	}
}

func (s *server) handleTick(w http.ResponseWriter, r *http.Request) {
	opt := s.tickOptions
	opt.Trigger = generation.TriggerAdmin
	if s.hasTickToken(r) {
		opt.Trigger = generation.TriggerScheduler
	}

	if s.generator == nil {
		// Not a 503. A Tick with nothing it could possibly do is not a
		// failure, and answering non-2xx would have Cloud Scheduler retry
		// forever against an instance that has no generation configured.
		//
		// The status is still recorded, and that is the whole point: the
		// admin card answers "is the clock reaching us", and a deployment
		// whose scheduler is working perfectly must not be told no Tick
		// has ever run just because generation is switched off. That is a
		// different problem, and it says so in the error.
		status := store.TickStatus{
			At:      time.Now().UTC(),
			Trigger: opt.Trigger,
			Error:   "generation is not configured on this server",
		}
		ctx, cancel := context.WithTimeout(context.Background(), tickTimeout)
		defer cancel()
		if err := s.store.PutTickStatus(ctx, status); err != nil {
			s.log.Error("tick: status write failed", "err", err)
		}
		s.tickResponse(w, r, status)
		return
	}

	// Background, not the request context: a Scheduler client timeout must
	// not cancel a pass between claiming a Beat and writing its Generation.
	ctx, cancel := context.WithTimeout(context.Background(), tickTimeout)
	defer cancel()

	status, err := s.generator.Tick(ctx, opt)
	if err != nil {
		// Only reached when nothing was fired, so the retry this provokes
		// is free. Every failure after the first firing is inside status.
		s.fail(w, err)
		return
	}
	s.tickResponse(w, r, status)
}

// tickStale is how long after a Tick the admin page starts warning.
// Comfortably more than the hourly interval, so one skipped run — a cold
// start that timed out, a redeploy — is not an alarm, but a scheduler job
// that was never created or has quietly stopped is.
const tickStale = 3 * time.Hour

// tickView is the operator's answer to "is the clock reaching us". It
// exists because nothing else on any surface would say: with no scheduler
// job the station looks perfectly healthy and simply never fires a Beat
// or resumes a stalled run.
type tickView struct {
	// Configured reports whether TICK_TOKEN is set. False is not an error
	// — an admin can still press the button — but it does mean no
	// scheduler is calling this deployment.
	Configured bool
	// Ever is false before the first Tick, which on a fresh deployment is
	// the interesting answer.
	Ever bool
	// At is RFC3339 UTC for a <time datetime> attribute; the browser turns
	// it into "12 minutes ago" against the reader's own clock, which is
	// the only one that can do it correctly.
	At    string
	Stale bool

	LiveUsers  int
	TotalUsers int
	BeatsFired int
	Resumed    int
	Truncated  bool
	Error      string
}

func (s *server) tickView(r *http.Request) tickView {
	v := tickView{Configured: s.tickEnabled, Stale: true}

	status, err := s.store.GetTickStatus(r.Context())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("tick: status read failed", "err", err)
		}
		return v
	}
	v.Ever = true
	v.At = status.At.UTC().Format(time.RFC3339)
	v.Stale = time.Since(status.At) > tickStale
	v.LiveUsers = status.LiveUsers
	v.BeatsFired = status.BeatsFired
	v.Resumed = status.Resumed
	v.Truncated = status.Truncated
	v.Error = status.Error

	// The denominator is computed here rather than stored on the record:
	// it changes about monthly, and making every Tick pay a full user scan
	// to keep it fresh would be a poor trade for one number on one page.
	if users, err := s.store.ListUsers(r.Context()); err == nil {
		v.TotalUsers = len(users)
	}
	return v
}

// tickResponse answers a browser with a redirect back where it came from
// (ADR 0022) and everyone else — Cloud Scheduler, curl — with the status
// as JSON.
func (s *server) tickResponse(w http.ResponseWriter, r *http.Request, status store.TickStatus) {
	if wantsHTML(r) {
		http.Redirect(w, r, returnTo(r, "/admin"), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(status); err != nil {
		s.log.Error("tick: encode failed", "err", err)
	}
}
