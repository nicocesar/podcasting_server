package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

// newDispatchingServer is the deployed shape since ADR 0035: Kick hands
// every run to POST /work on the service itself.
func newDispatchingServer(t *testing.T) (*httptest.Server, *fsstore.Store) {
	t.Helper()
	return newServerTick(t, generation.Config{
		Engines:   []tts.Engine{instantEngine{}},
		WorkToken: tickToken,
	}, generation.TickOptions{})
}

// stalledRun is an Active Generation no process is running, the shape a
// killed instance leaves behind.
func stalledRun(t *testing.T, st *fsstore.Store, lease time.Time) store.Generation {
	t.Helper()
	g := store.Generation{
		UserID: "alice", ID: "stalled", Topic: "an interrupted run",
		Template: "news", LengthMinutes: 3, FreshnessDays: 1, Language: "en",
		Stage: store.GenResearching, Active: true, CreatedAt: time.Now().UTC(),
		LeaseUntil: lease,
	}
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestDispatchedRunCompletes: a Generation started from the form runs to
// the end through the service's own /work route, not a goroutine — the
// lease it leaves on the record is the proof, since only that route
// takes one.
func TestDispatchedRunCompletes(t *testing.T) {
	ts, st := newDispatchingServer(t)
	alice := createUser(t, ts, "alice")

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(), newsForm(nil), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	gens, err := st.ListGenerations(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 1 {
		t.Fatalf("want 1 generation, got %d", len(gens))
	}
	g := gens[0]
	if g.Stage != store.GenDone {
		t.Fatalf("stage = %q (error %q), want done", g.Stage, g.Error)
	}
	if g.LeaseUntil.IsZero() {
		t.Error("run finished without a lease: it ran in a goroutine, not inside a /work request")
	}
}

// TestWorkRespectsLease: a run another instance holds is left alone —
// the deploy case, where the old revision finishes its in-flight run
// while the new one's Bootstrap resumes everything Active — and the same
// run is picked up once the lease has lapsed.
func TestWorkRespectsLease(t *testing.T) {
	ts, st := newDispatchingServer(t)
	createUser(t, ts, "alice")
	ctx := context.Background()

	stalledRun(t, st, time.Now().UTC().Add(time.Hour))
	resp := do(t, "POST", ts.URL+"/work/generations/alice/stalled", "bearer:"+tickToken, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("leased run: status = %d, want 409", resp.StatusCode)
	}
	if g, _ := st.GetGeneration(ctx, "alice", "stalled"); g.Stage != store.GenResearching {
		t.Fatalf("leased run was driven anyway: stage = %q", g.Stage)
	}

	stalledRun(t, st, time.Now().UTC().Add(-time.Minute))
	resp = do(t, "POST", ts.URL+"/work/generations/alice/stalled", "bearer:"+tickToken, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lapsed lease: status = %d, want 200", resp.StatusCode)
	}
	// The response is the end of the run, so no waiting.
	if g, _ := st.GetGeneration(ctx, "alice", "stalled"); g.Stage != store.GenDone {
		t.Errorf("after a 200 the run should be over: stage = %q (error %q)", g.Stage, g.Error)
	}
}

// TestWorkNeedsTickToken: the route starts spend, so it takes the
// scheduler's credential and nothing else — not a missing one, not a
// wrong one, not a user's session.
func TestWorkNeedsTickToken(t *testing.T) {
	ts, st := newDispatchingServer(t)
	alice := createUser(t, ts, "alice")
	stalledRun(t, st, time.Time{})

	for name, creds := range map[string]string{
		"none":    "",
		"wrong":   "bearer:not-the-token",
		"admin":   "bearer:" + adminToken,
		"session": alice.sessionCreds(),
	} {
		resp := do(t, "POST", ts.URL+"/work/generations/alice/stalled", creds, nil, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
	if g, _ := st.GetGeneration(context.Background(), "alice", "stalled"); g.Stage != store.GenResearching {
		t.Errorf("an unauthorized request drove the run: stage = %q", g.Stage)
	}
}
