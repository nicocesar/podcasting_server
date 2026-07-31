package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// tickToken is the scheduler's shared secret in tests.
const tickToken = "test-tick-token"

// tick runs one pass as Cloud Scheduler would, and returns what it did.
func tick(t *testing.T, ts *httptest.Server) store.TickStatus {
	t.Helper()
	resp := do(t, "POST", ts.URL+"/tick", "bearer:"+tickToken, nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /tick: status = %d, want 200\n%s", resp.StatusCode, body)
	}
	var status store.TickStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decoding tick status: %v", err)
	}
	return status
}

// markSeen stamps a user's liveness directly, so a test can say "quiet for
// a week" without waiting one.
func markSeen(t *testing.T, st *fsstore.Store, id string, at time.Time) {
	t.Helper()
	if err := st.TouchUser(context.Background(), id, at); err != nil {
		t.Fatal(err)
	}
}

// TestTickSkipsDormantUser is the liveness gate itself: a due Beat whose
// owner has been quiet longer than the window does not fire, and one poll
// is enough to bring them back — which ADR 0028 promises in as many words.
func TestTickSkipsDormantUser(t *testing.T) {
	ts, st := newGeneratingServerTick(t, nil, generation.TickOptions{LivenessWindow: 48 * time.Hour})
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.publishCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "1"}), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	b := onlyBeat(t, st, "alice")
	b.LastFiredAt = time.Now().UTC().Add(-48 * time.Hour)
	b.LastSucceededAt = b.LastFiredAt
	if err := st.PutBeat(ctx, b); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ListGenerations(ctx, "alice")

	// Three days quiet against a two-day window.
	markSeen(t, st, "alice", time.Now().UTC().Add(-72*time.Hour))
	status := tick(t, ts)
	if status.LiveUsers != 0 {
		t.Errorf("LiveUsers = %d, want 0 — a dormant user was in the working set", status.LiveUsers)
	}
	if status.BeatsFired != 0 {
		t.Errorf("BeatsFired = %d, want 0", status.BeatsFired)
	}
	waitAllSettled(t, st, "alice")
	if after, _ := st.ListGenerations(ctx, "alice"); len(after) != len(before) {
		t.Fatalf("a dormant user's beat fired: %d → %d", len(before), len(after))
	}

	// Coming back is one poll.
	markSeen(t, st, "alice", time.Now().UTC())
	if status := tick(t, ts); status.BeatsFired != 1 {
		t.Errorf("after coming back: BeatsFired = %d, want 1 (%+v)", status.BeatsFired, status)
	}
	waitAllSettled(t, st, "alice")
}

// TestTickResumesRegardlessOfLiveness: the resume pass is deliberately not
// liveness gated — a stalled run has already been paid for, and dropping
// it because its owner has been quiet wastes the spend rather than saving
// it. Easy to break by moving ResumeAll inside the per-user loop.
func TestTickResumesRegardlessOfLiveness(t *testing.T) {
	ts, st := newGeneratingServerTick(t, nil, generation.TickOptions{LivenessWindow: time.Hour})
	createUser(t, ts, "alice")
	ctx := context.Background()

	// A run this process knows nothing about, exactly like one an evicted
	// instance left behind.
	stalled := store.Generation{
		UserID: "alice", ID: "stalled", Topic: "an interrupted run",
		Template: "news", LengthMinutes: 3, FreshnessDays: 1, Language: "en",
		Stage: store.GenResearching, Active: true, CreatedAt: time.Now().UTC(),
	}
	if err := st.PutGeneration(ctx, stalled); err != nil {
		t.Fatal(err)
	}

	// Dormant on purpose: nobody has polled for this account in a week.
	markSeen(t, st, "alice", time.Now().UTC().Add(-7*24*time.Hour))

	status := tick(t, ts)
	if status.LiveUsers != 0 {
		t.Fatalf("LiveUsers = %d, want 0 — the test needs a dormant owner", status.LiveUsers)
	}
	if status.Resumed < 1 {
		t.Errorf("Resumed = %d, want at least 1: a stalled run was abandoned "+
			"because its owner had been quiet", status.Resumed)
	}
	waitAllSettled(t, st, "alice")
}

// TestTickBudgetBounds: the budget truncates a pass, and — the half that
// matters — nothing is lost. A skipped firing did not advance its Beat's
// clock, so it is still due on the next pass.
func TestTickBudgetBounds(t *testing.T) {
	ts, st := newGeneratingServerTick(t, nil, generation.TickOptions{BeatBudget: 1})
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	for range 2 {
		resp := do(t, "POST", ts.URL+"/me/generate/news", alice.publishCreds(),
			newsForm(map[string]string{"recur": "1", "freshness": "1"}), formType)
		resp.Body.Close()
		waitAllSettled(t, st, "alice")
	}

	beats, err := st.ListBeats(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(beats) != 2 {
		t.Fatalf("got %d beats, want 2", len(beats))
	}
	for _, b := range beats {
		b.LastFiredAt = time.Now().UTC().Add(-48 * time.Hour)
		b.LastSucceededAt = b.LastFiredAt
		if err := st.PutBeat(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	markSeen(t, st, "alice", time.Now().UTC())

	first := tick(t, ts)
	if first.BeatsFired != 1 {
		t.Fatalf("first pass: BeatsFired = %d, want 1 (the budget)", first.BeatsFired)
	}
	if !first.Truncated {
		t.Error("first pass: Truncated = false, want true — the operator cannot see the backlog")
	}
	waitAllSettled(t, st, "alice")

	// Deferred, not lost.
	second := tick(t, ts)
	if second.BeatsFired != 1 {
		t.Errorf("second pass: BeatsFired = %d, want 1 — the truncated firing was lost", second.BeatsFired)
	}
	waitAllSettled(t, st, "alice")

	third := tick(t, ts)
	if third.BeatsFired != 0 {
		t.Errorf("third pass: BeatsFired = %d, want 0 — both beats are now fresh", third.BeatsFired)
	}
	waitAllSettled(t, st, "alice")
}

// TestFeedPollDoesNotFireBeats is the direct inverse of the test ADR 0016
// shipped with, and the one that proves the reversal actually happened.
// The old behaviour was asynchronous, so an immediate read would pass
// vacuously — waitAllSettled is what gives a firing time to appear.
func TestFeedPollDoesNotFireBeats(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.publishCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "1"}), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	b := onlyBeat(t, st, "alice")
	b.LastFiredAt = time.Now().UTC().Add(-48 * time.Hour)
	b.LastSucceededAt = b.LastFiredAt
	if err := st.PutBeat(ctx, b); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ListGenerations(ctx, "alice")

	u, _ := st.GetUser(ctx, "alice")
	for range 3 {
		r := do(t, "GET", ts.URL+"/f/"+u.FeedToken+"/feed.xml", "", nil, "")
		r.Body.Close()
	}
	waitAllSettled(t, st, "alice")

	if after, _ := st.ListGenerations(ctx, "alice"); len(after) != len(before) {
		t.Errorf("a feed poll fired %d generations: traffic is still the clock",
			len(after)-len(before))
	}
	if after := onlyBeat(t, st, "alice"); !after.LastFiredAt.Equal(b.LastFiredAt) {
		t.Error("a feed poll advanced the beat's clock")
	}
}

// TestFeedPollRecordsLiveness: what the poll does instead. The write is
// detached, so this waits for it rather than reading straight after.
func TestFeedPollRecordsLiveness(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	createUser(t, ts, "alice")
	ctx := context.Background()

	before, err := st.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !before.LastSeenAt.IsZero() {
		t.Fatalf("a freshly created user is already live: %v", before.LastSeenAt)
	}

	resp := do(t, "GET", ts.URL+"/f/"+before.FeedToken+"/feed.xml", "", nil, "")
	resp.Body.Close()

	after := waitForLiveness(t, st, "alice")
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("being seen looked like a profile edit: UpdatedAt %v → %v",
			before.UpdatedAt, after.UpdatedAt)
	}
}

// TestLivenessWriteIsCoarsened: polls inside the hour cost nothing. Driven
// through HTTP rather than by calling seen directly, which is what proves
// the check reads the User the handler already has rather than a fresh one.
func TestLivenessWriteIsCoarsened(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	createUser(t, ts, "alice")
	ctx := context.Background()

	recent := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	markSeen(t, st, "alice", recent)

	u, _ := st.GetUser(ctx, "alice")
	for range 3 {
		r := do(t, "GET", ts.URL+"/f/"+u.FeedToken+"/feed.xml", "", nil, "")
		r.Body.Close()
	}
	// Give a write that should not happen time to happen.
	time.Sleep(150 * time.Millisecond)

	got, err := st.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastSeenAt.Equal(recent) {
		t.Errorf("three polls inside the hour rewrote liveness: %v → %v", recent, got.LastSeenAt)
	}

	// Past the hour it moves.
	stale := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	markSeen(t, st, "alice", stale)
	r := do(t, "GET", ts.URL+"/f/"+u.FeedToken+"/feed.xml", "", nil, "")
	r.Body.Close()

	after := waitForLivenessAfter(t, st, "alice", stale)
	if !after.LastSeenAt.After(stale) {
		t.Errorf("a poll after two hours did not record liveness: still %v", after.LastSeenAt)
	}
}

// TestDashboardRecordsLiveness: the attended surfaces record liveness too,
// so somebody who reads on the web rather than in a podcast client does
// not silently fall dormant.
func TestDashboardRecordsLiveness(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	dashboardHTML(t, ts, alice.sessionCreds())

	waitForLiveness(t, st, "alice")
}

// TestManagementAPIDoesNotRecordLiveness: the JSON form of /me is the
// Management API, which an API Key reaches. A Generator polling its
// owner's account is not a sign that anybody is listening, and must not
// be able to hold their Beats open indefinitely.
func TestManagementAPIDoesNotRecordLiveness(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	for range 3 {
		resp := do(t, "GET", ts.URL+"/me", alice.publishCreds(), nil, "")
		resp.Body.Close()
	}
	time.Sleep(150 * time.Millisecond)

	u, err := st.GetUser(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !u.LastSeenAt.IsZero() {
		t.Errorf("the Management API recorded liveness (%v): a bot can now keep "+
			"its owner's beats firing forever", u.LastSeenAt)
	}
}

func TestTickAuth(t *testing.T) {
	ts, _ := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	admin := createAdmin(t, ts, "chief")

	cases := []struct {
		name  string
		creds string
		want  int
		why   string
	}{
		{"scheduler token", "bearer:" + tickToken, http.StatusOK,
			"the credential Cloud Scheduler carries"},
		{"admin session", admin.sessionCreds(), http.StatusOK,
			"the run-a-pass-now button"},
		{"regular session", alice.sessionCreds(), http.StatusUnauthorized,
			"a Beat's owner does not get to run the station's clock"},
		// 403 and not 401: s.session refuses a valid credential of the
		// wrong kind, which is exactly what ADR 0010 specifies.
		{"api key", alice.publishCreds(), http.StatusForbidden,
			"ADR 0010: a Generator credential must never make the station spend on a timer"},
		{"admin token", "bearer:" + adminToken, http.StatusUnauthorized,
			"ADMIN_TOKEN is not reused — a scheduler job must not hold the break-glass secret"},
		{"wrong token", "bearer:not-the-tick-token", http.StatusUnauthorized, ""},
		{"anonymous", "", http.StatusUnauthorized, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, "POST", ts.URL+"/tick", tc.creds, nil, "")
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d — %s", resp.StatusCode, tc.want, tc.why)
			}
		})
	}
}

// TestTickAuthWithoutToken: an unset TICK_TOKEN must not turn the empty
// Bearer token into a valid credential, and must still leave the admin
// session working. hasAdminToken needs no such guard only because New
// refuses an empty AdminToken.
func TestTickAuthWithoutToken(t *testing.T) {
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:         st,
		AdminToken:    adminToken,
		SessionSecret: "test-session-secret",
		Assets:        os.DirFS("../../cmd/server"),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		// TickToken deliberately unset.
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	admin := createAdmin(t, ts, "chief")

	for _, creds := range []string{"bearer:", "bearer: ", ""} {
		resp := do(t, "POST", ts.URL+"/tick", creds, nil, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("creds %q: status = %d, want 401 — an unset TICK_TOKEN authorised the empty token",
				creds, resp.StatusCode)
		}
	}

	resp := do(t, "POST", ts.URL+"/tick", admin.sessionCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin session with no TICK_TOKEN: status = %d, want 200", resp.StatusCode)
	}
}

// TestTickWithoutGeneratorStillRecords: a deployment with no
// ANTHROPIC_API_KEY answers 200 — retrying forever would be worse — and
// still records the pass. The admin card's question is "is the clock
// reaching us", and telling an operator whose scheduler works perfectly
// that no Tick has ever run would send them hunting the wrong fault.
func TestTickWithoutGeneratorStillRecords(t *testing.T) {
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:         st,
		AdminToken:    adminToken,
		TickToken:     tickToken,
		SessionSecret: "test-session-secret",
		Assets:        os.DirFS("../../cmd/server"),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Generator deliberately nil.
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	status := tick(t, ts)
	if status.Error == "" {
		t.Error("a tick with no generator did not say why it did nothing")
	}

	got, err := st.GetTickStatus(context.Background())
	if err != nil {
		t.Fatalf("the pass was not recorded, so /admin will claim the clock "+
			"has never reached this deployment: %v", err)
	}
	if got.Trigger != generation.TriggerScheduler {
		t.Errorf("Trigger = %q, want %q", got.Trigger, generation.TriggerScheduler)
	}
}

// TestTickFromBrowserRedirects: the admin page's button is a plain form,
// so it must land back on /admin rather than on a page of JSON (ADR 0022).
func TestTickFromBrowserRedirects(t *testing.T) {
	ts, _ := newGeneratingServerStore(t, nil)
	admin := createAdmin(t, ts, "chief")

	req, err := http.NewRequest("POST", ts.URL+"/tick",
		strings.NewReader("return=%2Fadmin"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", formType)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "session", Value: admin.Session})
	req.Header.Set("Origin", ts.URL)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/admin" {
		t.Errorf("Location = %q, want /admin", got)
	}
}

// TestTickStatusOnAdminPage: the signal that a deployment with no
// scheduler job is otherwise missing. Also the only test that catches a
// template field renamed on one side only.
func TestTickStatusOnAdminPage(t *testing.T) {
	ts, _ := newGeneratingServerStore(t, nil)
	admin := createAdmin(t, ts, "chief")

	before := adminHTML(t, ts, ts.URL+"/admin", admin.sessionCreds())
	if !strings.Contains(before, "No tick has ever run") {
		t.Errorf("a deployment that has never ticked does not say so:\n%s", before)
	}

	tick(t, ts)

	after := adminHTML(t, ts, ts.URL+"/admin", admin.sessionCreds())
	if strings.Contains(after, "No tick has ever run") {
		t.Error("the admin page still says no tick has ever run")
	}
	if !strings.Contains(after, "listeners inside the liveness window") {
		t.Errorf("the tick card did not render its counts:\n%s", after)
	}
}

// waitForLiveness polls for the detached liveness write to land.
func waitForLiveness(t *testing.T, st *fsstore.Store, id string) store.User {
	t.Helper()
	return waitForLivenessAfter(t, st, id, time.Time{})
}

func waitForLivenessAfter(t *testing.T, st *fsstore.Store, id string, after time.Time) store.User {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		u, err := st.GetUser(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if u.LastSeenAt.After(after) {
			return u
		}
		if time.Now().After(deadline) {
			t.Fatalf("liveness never recorded for %q (LastSeenAt %v, want after %v)",
				id, u.LastSeenAt, after)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
