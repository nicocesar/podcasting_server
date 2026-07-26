package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// get fetches a URL with no credentials at all — the way a stranger or a
// podcast client arrives.
func get(t *testing.T, ts *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	resp, err := noRedirect.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestStrandPageAndIndexAreAnonymous: the whole point of ADR 0018 — a
// browser with no capability sees the strand and its episodes.
func TestStrandPageAndIndexAreAnonymous(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	airedEpisode(t, ts, st, alice, "chillout-one", "music")

	resp, body := get(t, ts, "/strands")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /strands: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "/strands/music") {
		t.Errorf("the index does not link the strand:\n%s", body)
	}

	resp, body = get(t, ts, "/strands/music")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /strands/music: %d", resp.StatusCode)
	}
	for _, want := range []string{"chillout-one", "Briefings for alice", "/strands/music/feed.xml"} {
		if !strings.Contains(body, want) {
			t.Errorf("strand page is missing %q:\n%s", want, body)
		}
	}
	// Attribution is the feed title, never the username, because the
	// username is also the Share address (ADR 0018).
	if strings.Contains(body, ">alice<") {
		t.Errorf("the strand page exposes the raw username:\n%s", body)
	}
}

// TestPrivateEpisodesAreNotOnTheStrand is the load-bearing privacy test.
// A published-but-never-aired Episode must be invisible and unfetchable
// on the public surface, however you ask for it.
func TestPrivateEpisodesAreNotOnTheStrand(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")

	// One aired, one strictly private — and the private one even carries
	// the same strand on the Episode, which is the trap: Stranding runs
	// on every Episode, aired or not.
	airedEpisode(t, ts, st, alice, "public-one", "music")
	publishEpisode(t, ts, alice, "secret-one", `{"title":"Secret"}`, "SECRET!").Body.Close()
	ep, err := st.GetEpisode(context.Background(), "alice", "secret-one")
	if err != nil {
		t.Fatal(err)
	}
	ep.Strand = "music"
	if err := st.UpdateEpisode(context.Background(), ep); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, ts, "/strands/music")
	if strings.Contains(page, "secret-one") || strings.Contains(page, "Secret") {
		t.Fatalf("a private episode is listed on the strand page:\n%s", page)
	}
	_, xml := get(t, ts, "/strands/music/feed.xml")
	if strings.Contains(xml, "secret-one") {
		t.Fatalf("a private episode is in the strand feed:\n%s", xml)
	}

	// And there is no address that would serve its audio.
	for _, path := range []string{
		"/strands/music/secret-one.mp3",
		"/strands/music/alice/secret-one.mp3",
	} {
		resp, body := get(t, ts, path)
		if resp.StatusCode == http.StatusOK && strings.Contains(body, "SECRET") {
			t.Fatalf("%s served private audio", path)
		}
	}
}

// TestStrandAudioNeedsItsOwnAiring: the id is the whole authorisation,
// and the strand in the path must not be a second way in.
func TestStrandAudioNeedsItsOwnAiring(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	putStrand(t, st, "stories", true, false)
	alice := createUser(t, ts, "alice")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	resp, body := get(t, ts, "/strands/music/"+id+".mp3")
	if resp.StatusCode != http.StatusOK || body != "MP3!" {
		t.Fatalf("audio on its own strand: %d %q", resp.StatusCode, body)
	}
	// Same id, wrong strand.
	resp, _ = get(t, ts, "/strands/stories/"+id+".mp3")
	if resp.StatusCode == http.StatusOK {
		t.Fatal("audio served under the wrong strand")
	}
	// Gone once un-aired.
	if err := st.DeleteAiring(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	resp, _ = get(t, ts, "/strands/music/"+id+".mp3")
	if resp.StatusCode == http.StatusOK {
		t.Fatal("audio still served after the un-air")
	}
}

// TestStrandFeedIsSubscribable: what a podcast client actually needs —
// blocked from directories, per-item attribution, opaque enclosure URLs
// with no username in them, and a GUID that matches the personal feed's
// so one episode is not two items.
func TestStrandFeedIsSubscribable(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	resp, body := get(t, ts, "/strands/music/feed.xml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET feed.xml: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{
		"<itunes:block>Yes</itunes:block>",
		"/strands/music/cover",
		"/strands/music/" + id + ".mp3",
		"<itunes:author>Briefings for alice</itunes:author>",
		"alice/ep1", // the GUID, unchanged from the personal feed
	} {
		if !strings.Contains(body, want) {
			t.Errorf("feed is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/f/") {
		t.Errorf("a public feed leaks a feed-token URL:\n%s", body)
	}
}

// TestDormantStrandHasNoPublicFace: a strand with no cover art would
// render a broken feed, so it does not exist publicly yet.
func TestDormantStrandHasNoPublicFace(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "dormant", false, false)

	for _, path := range []string{"/strands/dormant", "/strands/dormant/feed.xml"} {
		resp, _ := get(t, ts, path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, resp.StatusCode)
		}
	}
	_, index := get(t, ts, "/strands")
	if strings.Contains(index, "/strands/dormant") {
		t.Error("a dormant strand is listed on the index")
	}
}

// TestRetiredStrandKeepsServing: retirement takes a Strand out of
// discovery, not off the internet — the people already subscribed keep
// their feed (ADR 0017).
func TestRetiredStrandKeepsServing(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	airedEpisode(t, ts, st, alice, "ep1", "music")
	putStrand(t, st, "music", true, true) // retire it

	if resp, _ := get(t, ts, "/strands/music/feed.xml"); resp.StatusCode != http.StatusOK {
		t.Errorf("a retired strand's feed died: %d", resp.StatusCode)
	}
	if resp, _ := get(t, ts, "/strands/music"); resp.StatusCode != http.StatusOK {
		t.Errorf("a retired strand's page died: %d", resp.StatusCode)
	}
	_, index := get(t, ts, "/strands")
	if strings.Contains(index, "/strands/music") {
		t.Error("a retired strand is still in discovery")
	}
}

// TestMuteReachesTheStrandPage: Mute means nothing from that Owner
// anywhere the viewer looks, not only in their feed — the completion of
// ADR 0006's definition that ADR 0019 spells out.
func TestMuteReachesTheStrandPage(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bobby")
	airedEpisode(t, ts, st, alice, "ep1", "music")

	// Anonymous and un-muted viewers see it.
	if _, body := get(t, ts, "/strands/music"); !strings.Contains(body, "ep1") {
		t.Fatal("the episode is not on the page to begin with")
	}
	resp := do(t, "PUT", ts.URL+"/me/mutes/alice", bob.sessionCreds(), nil, "")
	resp.Body.Close()

	seen := do(t, "GET", ts.URL+"/strands/music", bob.sessionCreds(), nil, "")
	defer seen.Body.Close()
	body, _ := io.ReadAll(seen.Body)
	if strings.Contains(string(body), "ep1") {
		t.Fatalf("a muted owner's episode is on the strand page:\n%s", body)
	}
}

// TestSettlingRidesOnTraffic: reading a Strand freezes the Vouch count
// of anything whose day is up, with no scheduler anywhere (ADR 0016's
// pattern, ADR 0019's rule).
func TestSettlingRidesOnTraffic(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bobby")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	// Bob vouches, then the airing is backdated past its settling day.
	postForm(t, ts, bob.sessionCreds(), "/me/vouches/"+id, url.Values{}).Body.Close()
	airing, err := st.GetAiring(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	airing.AiredAt = time.Now().UTC().Add(-store.SettleWindow - time.Hour)
	if err := st.PutAiring(context.Background(), airing); err != nil {
		t.Fatal(err)
	}

	get(t, ts, "/strands/music") // the traffic that settles it

	airing, err = st.GetAiring(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !airing.Settled || airing.VouchesAtSettle != 1 {
		t.Fatalf("airing = settled %v with %d vouches; want settled with 1",
			airing.Settled, airing.VouchesAtSettle)
	}

	// A later vouch is displayed but does not change the frozen count:
	// feeds must not flap (ADR 0019).
	carol := createUser(t, ts, "carol")
	postForm(t, ts, carol.sessionCreds(), "/me/vouches/"+id, url.Values{}).Body.Close()
	get(t, ts, "/strands/music")
	airing, _ = st.GetAiring(context.Background(), id)
	if airing.VouchesAtSettle != 1 {
		t.Errorf("the frozen count moved to %d after settling", airing.VouchesAtSettle)
	}
	if v, _ := st.ListVouches(context.Background(), id); len(v) != 2 {
		t.Errorf("the later vouch was not recorded for display: %+v", v)
	}
}
