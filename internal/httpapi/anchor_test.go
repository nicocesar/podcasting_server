package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// anchoredForm is a Beat submission that asks for a time of day.
func anchoredForm(fireAt, zone string) io.Reader {
	return newsForm(map[string]string{
		"recur": "1", "freshness": "1",
		"fire_at": fireAt, "browser_zone": zone,
	})
}

// TestGenerateFormOffersATime: the controls have to actually be on the
// page. Worth asserting rather than eyeballing, because the form only
// renders where generation is configured — which a laptop usually is not.
func TestGenerateFormOffersATime(t *testing.T) {
	ts, _ := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	page := signedInPage(t, ts.URL+"/me/generate/news", alice.sessionCreds())
	for _, want := range []string{
		`id="fire_at"`,      // the time picker
		`name="fire_at"`,    // and it submits under the name the server reads
		`type="time"`,       // a real time input, not a free-text box
		`id="browser_zone"`, // the hidden field beat.js fills in
		`name="browser_zone"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the generate form is missing %s", want)
		}
	}
	// The stale promise ADR 0028 made false must not have crept back.
	if strings.Contains(page, "catch up when your podcast app checks the feed") {
		t.Error("the form still tells people beats fire on a feed poll")
	}
}

// TestSettingsOffersTheZoneOnceItMatters: a listener with nothing on a
// clock is never asked what country they consider home.
func TestSettingsOffersTheZoneOnceItMatters(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	if page := signedInPage(t, ts.URL+"/me/settings", alice.sessionCreds()); strings.Contains(page, "Home timezone") {
		t.Error("settings offers a timezone to somebody who has no use for one")
	}

	u, _ := st.GetUser(ctx, "alice")
	u.HomeZone = "America/New_York"
	if err := st.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	page := signedInPage(t, ts.URL+"/me/settings", alice.sessionCreds())
	if !strings.Contains(page, "Home timezone") || !strings.Contains(page, "America/New_York") {
		t.Errorf("settings does not show the stored zone:\n%s", page)
	}
}

// TestAnchorSetOnCreation: the time of day and the zone both land, and
// the zone comes from the browser rather than a dropdown.
func TestAnchorSetOnCreation(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		anchoredForm("07:00", "America/New_York"), formType)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201\n%s", resp.StatusCode, body)
	}

	b := onlyBeat(t, st, "alice")
	if b.FireAt != "07:00" {
		t.Errorf("FireAt = %q, want 07:00", b.FireAt)
	}
	u, err := st.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.HomeZone != "America/New_York" {
		t.Errorf("HomeZone = %q, want America/New_York", u.HomeZone)
	}
	waitAllSettled(t, st, "alice")
}

// TestAnchorRefusedWithoutAZone: an Anchor is a wall time, and without a
// zone it is not an instant. Refused on the form rather than guessed at,
// because guessing means UTC and UTC is somebody's 3am.
func TestAnchorRefusedWithoutAZone(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		anchoredForm("07:00", ""), formType)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "timezone") {
		t.Errorf("the error does not explain itself:\n%s", body)
	}
	if beats, _ := st.ListBeats(ctx, "alice"); len(beats) != 0 {
		t.Errorf("a refused submission left %d beats", len(beats))
	}

	// A loose Beat needs no zone at all and still goes through.
	resp = do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "1"}), formType)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a loose beat was refused: status = %d", resp.StatusCode)
	}
	waitAllSettled(t, st, "alice")
}

func TestAnchorRejectsNonsense(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	for _, tc := range []struct{ fireAt, zone string }{
		{"25:00", "America/New_York"},
		{"7am", "America/New_York"},
		{"07:00", "Mars/Olympus"},
		{"07:00", "Local"}, // the server's zone, which is nobody's morning
	} {
		resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
			anchoredForm(tc.fireAt, tc.zone), formType)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("fire_at=%q zone=%q: status = %d, want 400", tc.fireAt, tc.zone, resp.StatusCode)
		}
	}
	if beats, _ := st.ListBeats(context.Background(), "alice"); len(beats) != 0 {
		t.Errorf("rejected submissions left %d beats", len(beats))
	}
}

// TestTravelDoesNotMoveTheHomeZone is the decision this feature turns on.
// The form reports the browser's zone on every submission; only the first
// one is ever taken.
func TestTravelDoesNotMoveTheHomeZone(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		anchoredForm("07:00", "America/New_York"), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	// A week later, from Tokyo.
	resp = do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		anchoredForm("07:00", "Asia/Tokyo"), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	u, err := st.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.HomeZone != "America/New_York" {
		t.Errorf("HomeZone = %q — travelling moved somebody's morning", u.HomeZone)
	}
}

// TestChangingTheHomeZoneIsDeliberate: the one way it moves is the owner
// saying so, and an API Key may not do it — moving a zone westward can
// make today's Anchor un-happen and fire a second time, which is
// unattended spend (ADR 0010, ADR 0016).
func TestChangingTheHomeZoneIsDeliberate(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		anchoredForm("07:00", "America/New_York"), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	resp = do(t, "POST", ts.URL+"/me/timezone", alice.publishCreds(),
		strings.NewReader("timezone=Asia/Tokyo"), formType)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("an API key moved the home zone: status = %d, want 403", resp.StatusCode)
	}
	if u, _ := st.GetUser(ctx, "alice"); u.HomeZone != "America/New_York" {
		t.Fatalf("HomeZone = %q after an API key tried to move it", u.HomeZone)
	}

	// The owner may.
	req, err := http.NewRequest("POST", ts.URL+"/me/timezone",
		strings.NewReader("timezone=Asia/Tokyo&return=%2Fme%2Fsettings"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", formType)
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(&http.Cookie{Name: "session", Value: alice.Session})
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("owner changing their zone: status = %d, want 303", resp.StatusCode)
	}
	if u, _ := st.GetUser(ctx, "alice"); u.HomeZone != "Asia/Tokyo" {
		t.Errorf("HomeZone = %q, want Asia/Tokyo", u.HomeZone)
	}
}

// TestAnchoredBeatFiresForItsSlot drives the whole feature through the
// Tick, and is the test that would catch the drift coming back: the Beat
// fires for a Slot on the minute, and the Anchor it records is that Slot
// rather than the ragged instant the Tick happened to run.
//
// Deterministic whatever time the suite runs at: the Anchor is placed a
// fixed distance behind now rather than at a wall-clock hour, so "inside
// the grace window" is a fact about the fixture and not about the day.
func TestAnchoredBeatFiresForItsSlot(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	loc, err := store.LoadZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// One hour ago, local — comfortably inside the four-hour grace, and
	// still inside it whichever side of midnight that lands.
	slotClock := time.Now().In(loc).Add(-time.Hour).Format("15:04")

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		anchoredForm(slotClock, "America/New_York"), formType)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201\n%s", resp.StatusCode, body)
	}
	waitAllSettled(t, st, "alice")
	markSeen(t, st, "alice", time.Now().UTC())

	b := onlyBeat(t, st, "alice")
	b.AnchorAt = time.Now().UTC().Add(-49 * time.Hour) // two slots ago
	b.LastSucceededAt = b.AnchorAt
	if err := st.PutBeat(ctx, b); err != nil {
		t.Fatal(err)
	}

	if status := tick(t, ts); status.BeatsFired != 1 {
		t.Fatalf("BeatsFired = %d, want 1 — a beat inside its grace window did not fire (%+v)",
			status.BeatsFired, status)
	}
	waitAllSettled(t, st, "alice")

	after := onlyBeat(t, st, "alice")
	if got := after.AnchorAt.In(loc).Format("15:04"); got != slotClock {
		t.Errorf("anchor recorded as %s, want %s — it recorded the firing, not the slot, "+
			"which is the drift bug returning", got, slotClock)
	}
	if !after.LastFiredAt.After(after.AnchorAt) {
		t.Error("LastFiredAt should be the later of the two: the slot is when it meant to run")
	}
}

// TestAnchoredBeatPastGraceIsSkipped: the other half of the decision. Far
// enough past its Slot, the firing is abandoned rather than delivered
// hours late — and abandoning writes nothing, so tomorrow comes round on
// its own.
func TestAnchoredBeatPastGraceIsSkipped(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	loc, err := store.LoadZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// Six hours ago, against a four-hour grace.
	slotClock := time.Now().In(loc).Add(-6 * time.Hour).Format("15:04")

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		anchoredForm(slotClock, "America/New_York"), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")
	markSeen(t, st, "alice", time.Now().UTC())

	b := onlyBeat(t, st, "alice")
	b.AnchorAt = time.Now().UTC().Add(-54 * time.Hour)
	b.LastSucceededAt = b.AnchorAt
	if err := st.PutBeat(ctx, b); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ListGenerations(ctx, "alice")

	if status := tick(t, ts); status.BeatsFired != 0 {
		t.Errorf("a beat six hours past its slot fired anyway: %+v", status)
	}
	waitAllSettled(t, st, "alice")

	if gens, _ := st.ListGenerations(ctx, "alice"); len(gens) != len(before) {
		t.Error("a skipped beat started a generation")
	}
	if after := onlyBeat(t, st, "alice"); !after.AnchorAt.Equal(b.AnchorAt) {
		t.Error("a skipped beat wrote to its anchor — skipping is meant to cost no write")
	}
}

// TestAnchoredBeatWaitsForAZone: clearing a Home Zone that Anchored Beats
// depend on must stop them rather than firing them in UTC, which would
// put somebody's morning briefing in the middle of their night.
func TestAnchoredBeatWaitsForAZone(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		anchoredForm("07:00", "America/New_York"), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	// Reach behind the UI: the form will not create this state, but a
	// cleared zone can.
	u, _ := st.GetUser(ctx, "alice")
	u.HomeZone = ""
	if err := st.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	b := onlyBeat(t, st, "alice")
	b.AnchorAt = time.Now().UTC().Add(-72 * time.Hour)
	if err := st.PutBeat(ctx, b); err != nil {
		t.Fatal(err)
	}
	markSeen(t, st, "alice", time.Now().UTC())
	before, _ := st.ListGenerations(ctx, "alice")

	if status := tick(t, ts); status.BeatsFired != 0 {
		t.Errorf("an anchored beat with no zone fired: %+v", status)
	}
	waitAllSettled(t, st, "alice")
	if gens, _ := st.ListGenerations(ctx, "alice"); len(gens) != len(before) {
		t.Error("an anchored beat with no zone started a generation")
	}

	// And the Beats page says why, rather than showing it as healthy.
	page := beatsHTML(t, ts, alice.sessionCreds())
	if !strings.Contains(page, "waiting for a timezone") {
		t.Errorf("the beats page does not explain the stall:\n%s", page)
	}
}

// TestLooseBeatNeedsNoZone: the old shape still works untouched, for
// anyone who never wants a time of day.
func TestLooseBeatNeedsNoZone(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "1"}), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	if u, _ := st.GetUser(ctx, "alice"); u.HomeZone != "" {
		t.Errorf("a loose beat set a home zone: %q", u.HomeZone)
	}
	b := onlyBeat(t, st, "alice")
	b.AnchorAt = time.Now().UTC().Add(-48 * time.Hour)
	b.LastSucceededAt = b.AnchorAt
	if err := st.PutBeat(ctx, b); err != nil {
		t.Fatal(err)
	}
	markSeen(t, st, "alice", time.Now().UTC())

	if status := tick(t, ts); status.BeatsFired != 1 {
		t.Errorf("a loose beat did not fire: %+v", status)
	}
	waitAllSettled(t, st, "alice")
}

func beatsHTML(t *testing.T, ts *httptest.Server, creds string) string {
	t.Helper()
	return signedInPage(t, ts.URL+"/me/beats", creds)
}

// signedInPage fetches a page the way a browser does, collapsing
// whitespace so an assertion can quote a sentence the template wraps.
func signedInPage(t *testing.T, url, creds string) string {
	t.Helper()
	resp, body := htmlPage(t, url, creds)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d\n%s", url, resp.StatusCode, body)
	}
	return flat(body)
}

// TestBeatStatusNamesTheTime: an Anchored Beat's one line has to say the
// hour its owner asked for, in their own zone — a vague "next in about 14
// hours" would be a worse answer than the exact one they chose.
func TestBeatStatusNamesTheTime(t *testing.T) {
	loc, err := store.LoadZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// A Tuesday, so the weekday branch is deterministic.
	now := time.Date(2026, 7, 7, 9, 0, 0, 0, loc)

	cases := []struct {
		name  string
		beat  store.Beat
		want  string
		hasTZ bool
	}{
		{
			name:  "later today",
			beat:  store.Beat{IntervalDays: 1, FireAt: "21:00", AnchorAt: time.Date(2026, 7, 6, 21, 0, 0, 0, loc)},
			want:  "next today at 21:00",
			hasTZ: true,
		},
		{
			name:  "tomorrow morning",
			beat:  store.Beat{IntervalDays: 1, FireAt: "07:00", AnchorAt: time.Date(2026, 7, 7, 7, 0, 0, 0, loc)},
			want:  "next tomorrow at 07:00",
			hasTZ: true,
		},
		{
			name:  "later in the week names the day",
			beat:  store.Beat{IntervalDays: 7, FireAt: "07:00", AnchorAt: time.Date(2026, 7, 3, 7, 0, 0, 0, loc)},
			want:  "next Friday at 07:00",
			hasTZ: true,
		},
		{
			name:  "a paused beat says so before anything else",
			beat:  store.Beat{IntervalDays: 1, FireAt: "07:00", Paused: true},
			want:  "paused",
			hasTZ: true,
		},
		{
			name:  "an anchored beat with no zone explains the stall",
			beat:  store.Beat{IntervalDays: 1, FireAt: "07:00", AnchorAt: now.Add(-48 * time.Hour)},
			want:  "waiting for a timezone — set one in Settings",
			hasTZ: false,
		},
		{
			name:  "a loose beat keeps the vague span",
			beat:  store.Beat{IntervalDays: 7, AnchorAt: time.Date(2026, 7, 6, 9, 0, 0, 0, loc)},
			want:  "next in about 6 days",
			hasTZ: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := beatStatus(tc.beat, now, loc, tc.hasTZ); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
