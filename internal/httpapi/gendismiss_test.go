package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// plantFailedGeneration puts a failed Generation straight into the store.
// Driving a real run to failure would mean an engine rigged to break;
// what these tests are about is what the Dashboard does with a failure
// once it exists, not how it got one.
func plantFailedGeneration(t *testing.T, st *fsstore.Store, userID, id, topic string) {
	t.Helper()
	err := st.PutGeneration(context.Background(), store.Generation{
		UserID:    userID,
		ID:        id,
		Topic:     topic,
		Stage:     store.GenFailed,
		Error:     "the agent gave up",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// dismiss presses the Dashboard's Dismiss button.
func dismiss(t *testing.T, ts *httptest.Server, a account, id string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/me/generations/"+id+"/dismiss",
		strings.NewReader(url.Values{"return": {"/me"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "session", Value: a.Session})
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A failed Generation is not done, so nothing ever filtered it out of the
// Dashboard's in-flight panel: one bad run sat there for good. Dismissing
// is the way off, and it must not take the record with it — the run is
// still retryable and still carries the meters an admin bills from.
func TestDismissedGenerationLeavesTheDashboard(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	plantFailedGeneration(t, st, alice.ID, "gen-dead", "a topic that went nowhere")

	if page := dashboard(t, ts, alice, ""); !strings.Contains(page, "a topic that went nowhere") {
		t.Fatalf("failed generation missing from the dashboard before dismissal:\n%s", page)
	}

	resp := dismiss(t, ts, alice, "gen-dead")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dismiss = %d, want 303", resp.StatusCode)
	}

	if page := dashboard(t, ts, alice, ""); strings.Contains(page, "a topic that went nowhere") {
		t.Errorf("dismissed generation still on the dashboard:\n%s", page)
	}

	// Dismissing hides a row; it does not delete a run.
	g, err := st.GetGeneration(context.Background(), alice.ID, "gen-dead")
	if err != nil {
		t.Fatalf("dismissed generation was destroyed: %v", err)
	}
	if g.Stage != store.GenFailed || !g.Dismissed {
		t.Errorf("after dismiss: stage=%q dismissed=%v, want failed/true", g.Stage, g.Dismissed)
	}
}

// Dismissal survives the round trip through the store. Dismissed carries
// a plain JSON tag precisely so the fs backend persists it from the
// embedded struct; a json:"-" would have needed a line in
// generationRecord and silently forgotten the flag without one.
func TestDismissalPersists(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	plantFailedGeneration(t, st, alice.ID, "gen-dead", "a topic that went nowhere")
	dismiss(t, ts, alice, "gen-dead").Body.Close()

	// Straight from disk, not from any in-process cache.
	gens, err := st.ListGenerations(context.Background(), alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 1 {
		t.Fatalf("generations = %d, want 1", len(gens))
	}
	if !gens[0].Dismissed {
		t.Error("Dismissed did not survive the store round trip")
	}
}

// Pressing Dismiss twice — a double-submitted form, a reloaded POST —
// lands on the Dashboard rather than on an error page.
func TestDismissIsIdempotent(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	plantFailedGeneration(t, st, alice.ID, "gen-dead", "a topic that went nowhere")
	dismiss(t, ts, alice, "gen-dead").Body.Close()

	resp := dismiss(t, ts, alice, "gen-dead")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("second dismiss = %d, want 303", resp.StatusCode)
	}
}

// Only a failure can be dismissed. Hiding a run that is still going would
// mean a Generation spending real money with no row on the Dashboard
// saying so.
func TestInFlightGenerationCannotBeDismissed(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	// Active deliberately left false. Opening the Dashboard runs the
	// resume pass, which re-Kicks every Active Generation it finds — so
	// an Active fixture would have the runner pick this record up and
	// drive it for real, which is not what is under test here. The gate
	// being tested reads Stage, not Active.
	err := st.PutGeneration(context.Background(), store.Generation{
		UserID:    alice.ID,
		ID:        "gen-live",
		Topic:     "a topic still cooking",
		Stage:     store.GenResearching,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := dismiss(t, ts, alice, "gen-live")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("dismissing an in-flight run = %d, want 409", resp.StatusCode)
	}

	if page := dashboard(t, ts, alice, ""); !strings.Contains(page, "a topic still cooking") {
		t.Errorf("in-flight generation left the dashboard:\n%s", page)
	}
}

// Retrying a dismissed run puts it back on the Dashboard. Without this
// the row would be hidden while the run was live and spending money —
// the one state where the panel most needs to say something.
func TestRetryUndismisses(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	plantFailedGeneration(t, st, alice.ID, "gen-dead", "a topic that went nowhere")
	dismiss(t, ts, alice, "gen-dead").Body.Close()

	resp := do(t, "POST", ts.URL+"/me/generations/gen-dead/retry", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry = %d, want 200", resp.StatusCode)
	}
	waitGenerationDone(t, ts, alice, "gen-dead")

	g, err := st.GetGeneration(context.Background(), alice.ID, "gen-dead")
	if err != nil {
		t.Fatal(err)
	}
	if g.Dismissed {
		t.Error("a retried generation is still dismissed — it would run invisibly")
	}
}

// One user's dismissal is not another's: the handler resolves the
// Generation under the caller's own ID, so a guessed id belonging to
// somebody else is simply not found.
func TestDismissIsScopedToTheOwner(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")

	plantFailedGeneration(t, st, alice.ID, "gen-dead", "alice's dead run")

	resp := dismiss(t, ts, bobby, "gen-dead")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bobby dismissing alice's run = %d, want 404", resp.StatusCode)
	}

	g, err := st.GetGeneration(context.Background(), alice.ID, "gen-dead")
	if err != nil {
		t.Fatal(err)
	}
	if g.Dismissed {
		t.Error("bobby dismissed alice's generation")
	}
}
