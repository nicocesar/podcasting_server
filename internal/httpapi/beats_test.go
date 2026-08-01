package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// newsForm is a valid Briefing submission; cases override what they test.
func newsForm(over map[string]string) io.Reader {
	v := url.Values{
		"topic": {"fusion energy"}, "length": {"2"}, "freshness": {"7"},
		"language": {"en"}, "voice": {"female"},
	}
	for k, val := range over {
		v.Set(k, val)
	}
	return strings.NewReader(v.Encode())
}

const formType = "application/x-www-form-urlencoded"

// onlyBeat returns the user's single Beat, failing if there isn't exactly
// one.
func onlyBeat(t *testing.T, st *fsstore.Store, user string) store.Beat {
	t.Helper()
	beats, err := st.ListBeats(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	if len(beats) != 1 {
		t.Fatalf("want exactly 1 beat, got %d: %+v", len(beats), beats)
	}
	return beats[0]
}

// TestBeatCreatedWithTheFirstEpisode: ticking the box both starts an
// Episode now and leaves a Beat behind, with its clock anchored to now —
// the next one is a full interval away, not immediately due.
func TestBeatCreatedWithTheFirstEpisode(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1"}), formType)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	b := onlyBeat(t, st, "alice")
	if b.Topic != "fusion energy" || b.Template != "news" {
		t.Errorf("beat did not copy the request: %+v", b)
	}
	// The Briefing derives its cadence from the Freshness Window.
	if b.IntervalDays != 7 || b.FreshnessDays != 7 {
		t.Errorf("interval = %d, freshness = %d; want 7 and 7", b.IntervalDays, b.FreshnessDays)
	}
	if b.LastFiredAt.IsZero() {
		t.Error("the clock did not start: LastFiredAt is zero")
	}
	if b.Due(time.Now().UTC()) {
		t.Error("a brand-new beat is already due; it should be a week away")
	}
	waitAllSettled(t, st, "alice")
}

// TestNoBeatWithoutTheCheckbox: the default is still a one-off.
func TestNoBeatWithoutTheCheckbox(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.publishCreds(), newsForm(nil), formType)
	resp.Body.Close()

	beats, err := st.ListBeats(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(beats) != 0 {
		t.Fatalf("an unticked form left %d beats behind", len(beats))
	}
	waitAllSettled(t, st, "alice")
}

// TestTimelessCannotRecur: a Timeless topic has no Freshness Window, so
// it has no cadence either. The form hides the box, but the rule is the
// server's — a hand-posted form must be rejected, and nothing at all may
// be created by the rejected request.
func TestTimelessCannotRecur(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "0"}), formType)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "timeless topic") {
		t.Errorf("the error does not explain itself:\n%s", body)
	}
	beats, _ := st.ListBeats(context.Background(), "alice")
	gens, _ := st.ListGenerations(context.Background(), "alice")
	if len(beats) != 0 || len(gens) != 0 {
		t.Errorf("a rejected submission left %d beats and %d generations", len(beats), len(gens))
	}
}

// TestBeatCreationRequiresSession: a Beat is session-only, and this is
// where one is born. The /me/beats routes have always been s.session, but
// nothing creates a Beat there — the recur checkbox on the generate form
// does, and that route accepts an API Key. ADR 0016 claimed the invariant
// while this path quietly broke it, and every test above created its
// Beats with a Generator credential without anyone noticing.
//
// It matters more since ADR 0028 than it did before: a leaked key's Beats
// used to fire only when traffic happened to arrive, and now they fire on
// a clock.
func TestBeatCreationRequiresSession(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.publishCreds(),
		newsForm(map[string]string{"recur": "1"}), formType)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an API key created a Beat: status = %d, want 403\n%s", resp.StatusCode, body)
	}

	// Refused before anything exists — no Beat, and no Episode the owner
	// would be billed for on the way to being told no.
	beats, _ := st.ListBeats(ctx, "alice")
	if len(beats) != 0 {
		t.Errorf("a refused request left %d beats behind", len(beats))
	}
	gens, _ := st.ListGenerations(ctx, "alice")
	if len(gens) != 0 {
		t.Errorf("a refused request started %d generations", len(gens))
	}

	// The same key still publishes and still makes one-off Episodes: this
	// closes the Beat path, not the Publishing Contract.
	resp = do(t, "POST", ts.URL+"/me/generate/news", alice.publishCreds(), newsForm(nil), formType)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("a one-off with an API key: status = %d, want 201", resp.StatusCode)
	}
	waitAllSettled(t, st, "alice")
}

// TestBeatCap: the guard against unattended spending, and it must fire
// before anything is created — otherwise the user pays for an Episode to
// be told they may not have the Beat that came with it.
func TestBeatCap(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	for i := range maxBeatsPerUser {
		resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
			newsForm(map[string]string{"recur": "1"}), formType)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("beat %d: status = %d, want 201", i, resp.StatusCode)
		}
	}
	before, _ := st.ListGenerations(context.Background(), "alice")

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1"}), formType)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("beat %d: status = %d, want 400\n%s", maxBeatsPerUser+1, resp.StatusCode, body)
	}
	beats, _ := st.ListBeats(context.Background(), "alice")
	if len(beats) != maxBeatsPerUser {
		t.Errorf("beats = %d, want %d", len(beats), maxBeatsPerUser)
	}
	if after, _ := st.ListGenerations(context.Background(), "alice"); len(after) != len(before) {
		t.Errorf("the rejected request still started a generation: %d → %d", len(before), len(after))
	}
	// A one-off is still fine at the cap: the limit is on Beats, not on
	// making episodes.
	resp = do(t, "POST", ts.URL+"/me/generate/news", alice.publishCreds(), newsForm(nil), formType)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("a one-off at the cap: status = %d, want 201", resp.StatusCode)
	}
	waitAllSettled(t, st, "alice")
}

// TestTickFiresDueBeatForLiveUser: the Tick is the scheduler now. Age the
// Beat's clock past its interval, mark the owner live, run a pass, and
// exactly one new Generation must appear — carrying the Beat's id and a
// window stretched to the real gap.
func TestTickFiresDueBeatForLiveUser(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "1"}), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	b := onlyBeat(t, st, "alice")
	// Two days quiet on a daily Beat.
	b.LastFiredAt = time.Now().UTC().Add(-48 * time.Hour)
	b.LastSucceededAt = b.LastFiredAt
	if err := st.PutBeat(ctx, b); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ListGenerations(ctx, "alice")

	markSeen(t, st, "alice", time.Now().UTC())
	status := tick(t, ts)
	if status.BeatsFired != 1 {
		t.Errorf("BeatsFired = %d, want 1 (%+v)", status.BeatsFired, status)
	}
	if status.LiveUsers != 1 {
		t.Errorf("LiveUsers = %d, want 1", status.LiveUsers)
	}

	// The Beat's own first Episode also carries its id — it is the first
	// thing the Beat produced — so take the newest match, which is the one
	// this pass started. ListGenerations is newest-first.
	fired := waitForGenerations(t, st, "alice", len(before)+1)
	var g store.Generation
	for _, cand := range fired {
		if cand.BeatID == b.ID {
			g = cand
			break
		}
	}
	if g.ID == "" {
		t.Fatalf("no generation carries the beat id %q: %+v", b.ID, fired)
	}
	if g.Topic != b.Topic {
		t.Errorf("topic = %q, want %q", g.Topic, b.Topic)
	}
	// Two days of ground uncovered, so the window widens from 1 to 2
	// rather than silently dropping a day of news.
	if g.FreshnessDays != 2 {
		t.Errorf("freshness = %d, want 2 (the real gap)", g.FreshnessDays)
	}
	if after := onlyBeat(t, st, "alice"); !after.LastFiredAt.After(b.LastFiredAt) {
		t.Error("the beat's clock did not advance")
	}
	waitAllSettled(t, st, "alice")
}

// TestTickFiresOnceUnderConcurrency: the hourly job and an admin pressing
// "run a pass now" can arrive at the same instant. Advancing the clock
// before kicking is the claim that makes that safe — six passes must not
// become six Episodes.
func TestTickFiresOnceUnderConcurrency(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
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
	markSeen(t, st, "alice", time.Now().UTC())

	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			r := do(t, "POST", ts.URL+"/tick", "bearer:"+tickToken, nil, "")
			r.Body.Close()
		})
	}
	wg.Wait()
	waitAllSettled(t, st, "alice")

	after, _ := st.ListGenerations(ctx, "alice")
	if len(after) != len(before)+1 {
		t.Errorf("six simultaneous ticks made %d generations, want 1 (total %d → %d)",
			len(after)-len(before), len(before), len(after))
	}
}

// TestBeatNotDueDoesNotFire: the common case — a Tick every hour must not
// produce an Episode every hour.
func TestBeatNotDueDoesNotFire(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "7"}), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")
	before, _ := st.ListGenerations(ctx, "alice")
	markSeen(t, st, "alice", time.Now().UTC())

	for range 3 {
		if status := tick(t, ts); status.BeatsFired != 0 {
			t.Errorf("a beat that is not due fired: %+v", status)
		}
	}
	waitAllSettled(t, st, "alice")

	if after, _ := st.ListGenerations(ctx, "alice"); len(after) != len(before) {
		t.Errorf("ticking a beat that is not due made %d extra generations", len(after)-len(before))
	}
}

// TestBeatPauseResumeCancel walks the management controls, including the
// two things a cancel must not do: touch the Episodes already published,
// or leave the Beat behind.
func TestBeatPauseResumeCancel(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "1"}), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")
	b := onlyBeat(t, st, "alice")
	episodesBefore, _ := st.ListEpisodes(ctx, "alice")
	if len(episodesBefore) == 0 {
		t.Fatal("the first episode never landed")
	}

	resp = do(t, "POST", ts.URL+"/me/beats/"+b.ID+"/pause", alice.sessionCreds(), nil, "")
	resp.Body.Close()
	if got := onlyBeat(t, st, "alice"); !got.Paused {
		t.Error("pause did not pause the beat")
	}

	// A paused beat is skipped however overdue it is.
	paused := onlyBeat(t, st, "alice")
	paused.LastFiredAt = time.Now().UTC().Add(-90 * 24 * time.Hour)
	if err := st.PutBeat(ctx, paused); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ListGenerations(ctx, "alice")
	u, _ := st.GetUser(ctx, "alice")
	r := do(t, "GET", ts.URL+"/f/"+u.FeedToken+"/feed.xml", "", nil, "")
	r.Body.Close()
	waitAllSettled(t, st, "alice")
	if after, _ := st.ListGenerations(ctx, "alice"); len(after) != len(before) {
		t.Errorf("a paused beat fired anyway")
	}

	// Resume re-phases the clock: coming back from a break gives a fresh
	// episode, not one stretched across the whole break.
	resp = do(t, "POST", ts.URL+"/me/beats/"+b.ID+"/resume", alice.sessionCreds(), nil, "")
	resp.Body.Close()
	got := onlyBeat(t, st, "alice")
	if got.Paused {
		t.Error("resume did not unpause the beat")
	}
	if got.Due(time.Now().UTC()) {
		t.Error("resume left the beat immediately due; the clock should have re-phased")
	}

	resp = do(t, "POST", ts.URL+"/me/beats/"+b.ID+"/cancel", alice.sessionCreds(), nil, "")
	resp.Body.Close()
	if beats, _ := st.ListBeats(ctx, "alice"); len(beats) != 0 {
		t.Errorf("cancel left %d beats", len(beats))
	}
	episodesAfter, _ := st.ListEpisodes(ctx, "alice")
	if len(episodesAfter) != len(episodesBefore) {
		t.Errorf("cancel changed the feed: %d episodes → %d", len(episodesBefore), len(episodesAfter))
	}
}

// TestBeatEditKeepsHistory: editing changes the future only. The clock,
// the episode count, and the window's anchor must survive, or retuning a
// topic's wording would re-cover a week you already heard.
func TestBeatEditKeepsHistory(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")
	ctx := context.Background()

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "1"}), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	b := onlyBeat(t, st, "alice")
	b.EpisodeCount = 11
	b.LastSucceededAt = time.Now().UTC().Add(-2 * time.Hour)
	if err := st.PutBeat(ctx, b); err != nil {
		t.Fatal(err)
	}

	resp = do(t, "POST", ts.URL+"/me/beats/"+b.ID, alice.sessionCreds(),
		newsForm(map[string]string{
			"recur": "1", "topic": "fusion energy, but the politics", "freshness": "7",
		}), formType)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusOK {
		t.Fatalf("edit: status = %d", resp.StatusCode)
	}

	got := onlyBeat(t, st, "alice")
	if got.Topic != "fusion energy, but the politics" {
		t.Errorf("topic = %q; the edit did not apply", got.Topic)
	}
	if got.IntervalDays != 7 || got.FreshnessDays != 7 {
		t.Errorf("cadence did not follow the new window: interval %d, freshness %d",
			got.IntervalDays, got.FreshnessDays)
	}
	if got.EpisodeCount != 11 {
		t.Errorf("episode count = %d, want 11", got.EpisodeCount)
	}
	if !got.LastSucceededAt.Equal(b.LastSucceededAt) || !got.LastFiredAt.Equal(b.LastFiredAt) {
		t.Error("the edit reset the clock")
	}
	if got.CreatedAt.IsZero() {
		t.Error("the edit lost the creation time")
	}
}

// TestBeatEditToTimelessRejected: the same rule as creation, on the edit
// path — and the Beat must be left exactly as it was.
func TestBeatEditToTimelessRejected(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "7"}), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")
	b := onlyBeat(t, st, "alice")

	resp = do(t, "POST", ts.URL+"/me/beats/"+b.ID, alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "0"}), formType)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", resp.StatusCode, body)
	}
	if got := onlyBeat(t, st, "alice"); got.FreshnessDays != 7 || got.IntervalDays != 7 {
		t.Errorf("a rejected edit changed the beat: %+v", got)
	}
}

// TestBeatsAreSessionOnly: a Beat spends money unattended, so an API Key
// must not be able to leave one running or reach the management surface
// (ADR 0010's rule for Credential Management, applied here for the same
// reason).
func TestBeatsAreSessionOnly(t *testing.T) {
	ts, _ := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	for _, path := range []string{"/me/beats", "/me/beats/whatever/edit"} {
		resp := do(t, "GET", ts.URL+path, alice.publishCreds(), nil, "")
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s with an API key = 200; should need a session", path)
		}
	}
	resp := do(t, "POST", ts.URL+"/me/beats/whatever/cancel", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Error("an API key could cancel a beat")
	}
}

// TestBeatsPageShowsTheBeat: the tab has to actually describe what is
// running — program, cadence, and where it stands.
func TestBeatsPageShowsTheBeat(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	resp := do(t, "POST", ts.URL+"/me/generate/news", alice.sessionCreds(),
		newsForm(map[string]string{"recur": "1", "freshness": "7"}), formType)
	resp.Body.Close()
	waitAllSettled(t, st, "alice")

	resp = do(t, "GET", ts.URL+"/me/beats", alice.sessionCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"fusion energy", "The Briefing", "Every week", "next in about"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("beats page missing %q:\n%s", want, body)
		}
	}
	waitAllSettled(t, st, "alice")
}

// TestDashboardLinksToBeatsWhenEmpty: the checkbox that makes a Beat is
// at the bottom of a form you have to already be filling in, so without a
// standing link on the Dashboard the whole feature is invisible to anyone
// who has never made one.
func TestDashboardLinksToBeatsWhenEmpty(t *testing.T) {
	ts, _ := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	// The Dashboard is the HTML face of /me; without the Accept header the
	// same route answers JSON.
	req, _ := http.NewRequest("GET", ts.URL+"/me", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: alice.Session})
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{`href="/me/beats"`, "Nothing on a beat yet"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("dashboard with no beats is missing %q", want)
		}
	}
}

// TestStoriesBeatPicksItsOwnCadence: Story Time has no Freshness Window,
// so it chooses an interval instead — and an invalid one is rejected.
func TestStoriesBeatPicksItsOwnCadence(t *testing.T) {
	ts, st := newGeneratingServerStore(t, nil)
	alice := createUser(t, ts, "alice")

	form := func(over map[string]string) io.Reader {
		v := url.Values{
			"topic": {"a dragon afraid of heights"}, "length": {"2"},
			"age": {"5-7"}, "language": {"en"}, "voice": {"female"},
		}
		for k, val := range over {
			v.Set(k, val)
		}
		return strings.NewReader(v.Encode())
	}

	resp := do(t, "POST", ts.URL+"/me/generate/stories", alice.sessionCreds(),
		form(map[string]string{"recur": "1", "interval": "5"}), formType)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an off-menu interval: status = %d, want 400", resp.StatusCode)
	}

	resp = do(t, "POST", ts.URL+"/me/generate/stories", alice.sessionCreds(),
		form(map[string]string{"recur": "1", "interval": "1"}), formType)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	b := onlyBeat(t, st, "alice")
	if b.IntervalDays != 1 {
		t.Errorf("interval = %d, want 1", b.IntervalDays)
	}
	// A story is never news: nothing derived a window for it.
	if b.FreshnessDays != 0 {
		t.Errorf("freshness = %d on a story beat, want 0", b.FreshnessDays)
	}
	waitAllSettled(t, st, "alice")
}

// waitForGenerations waits until the user has at least n generations,
// returning them. A Tick is synchronous but Kick is not, and a generate
// request answers before its run finishes, so a test cannot simply read
// straight after the response.
func waitForGenerations(t *testing.T, st *fsstore.Store, user string, n int) []store.Generation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		gens, err := st.ListGenerations(context.Background(), user)
		if err != nil {
			t.Fatal(err)
		}
		if len(gens) >= n {
			return gens
		}
		time.Sleep(10 * time.Millisecond)
	}
	gens, _ := st.ListGenerations(context.Background(), user)
	t.Fatalf("waited for %d generations, have %d", n, len(gens))
	return nil
}

// waitAllSettled lets every in-flight run finish, so a test never leaves
// a goroutine writing into a temp directory being torn down. It also
// gives anything already in flight time to have done whatever it was
// going to — which is what makes "nothing fired" assertions mean
// something rather than passing vacuously.
func waitAllSettled(t *testing.T, st *fsstore.Store, user string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		gens, err := st.ListGenerations(context.Background(), user)
		if err != nil {
			t.Fatal(err)
		}
		active := false
		for _, g := range gens {
			if g.Active {
				active = true
			}
		}
		if !active {
			// One more beat of grace for a pass that has read the beats
			// but not yet written a Generation.
			time.Sleep(50 * time.Millisecond)
			gens, _ = st.ListGenerations(context.Background(), user)
			for _, g := range gens {
				if g.Active {
					active = true
				}
			}
			if !active {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("generations never settled")
}
