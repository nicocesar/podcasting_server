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
)

// backdateAired ages an Airing by hand. Time is the one thing these
// tests cannot wait for, and the horizon is now the only bound on
// delivery besides Mute (ADR 0027).
func backdateAired(t *testing.T, st store.Store, id string, age time.Duration) {
	t.Helper()
	a, err := st.GetAiring(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	a.AiredAt = time.Now().UTC().Add(-age)
	if err := st.PutAiring(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func feedXML(t *testing.T, ts *httptest.Server, a account) string {
	t.Helper()
	resp, err := http.Get(a.FeedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET feed.xml: %d", resp.StatusCode)
	}
	b := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(b)
}

// deliveryFixture: alice airs an episode on music and carol follows it.
// and carol follows the strand. Returns the airing id.
func deliveryFixture(t *testing.T, ts *httptest.Server, st store.Store, alice, carol account) string {
	t.Helper()
	id := airedEpisode(t, ts, st, alice, "lounge", "music")
	postForm(t, ts, carol.sessionCreds(), "/me/follows/music", url.Values{}).Body.Close()
	return id
}

// TestFollowDeliversIntoThePersonalFeed is the whole point of ADR 0019:
// following a strand puts what cleared your Bar into your own feed,
// with no second subscription.
func TestFollowDeliversIntoThePersonalFeed(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	carol := createUser(t, ts, "carol")
	id := deliveryFixture(t, ts, st, alice, carol)

	xml := feedXML(t, ts, carol)
	if !strings.Contains(xml, "lounge") {
		t.Fatalf("the followed episode is not in carol's feed:\n%s", xml)
	}
	// The enclosure is the public strand URL, not carol's feed token:
	// the audio is public, and /f/ authorises own-or-shared only.
	if !strings.Contains(xml, "/strands/music/"+id+".mp3") {
		t.Errorf("enclosure is not the public strand address:\n%s", xml)
	}
	// The GUID is unchanged, so this is the same item a podcast client
	// would see anywhere else (ADR 0008).
	if !strings.Contains(xml, "alice/lounge") {
		t.Errorf("GUID is not (owner, slug):\n%s", xml)
	}
	// Attributed by feed title, never by username.
	if !strings.Contains(xml, "<itunes:author>Briefings for alice</itunes:author>") {
		t.Errorf("delivered item is not credited to the owner's feed title:\n%s", xml)
	}

	// And the enclosure actually plays for someone holding nothing.
	resp, body := get(t, ts, "/strands/music/"+id+".mp3")
	if resp.StatusCode != http.StatusOK || body != "MP3!" {
		t.Errorf("the delivered enclosure does not play: %d %q", resp.StatusCode, body)
	}
}

// TestDeliveryStopsAtTheHorizon: a new follower gets a month of
// backfill, not the whole archive. With the Bar and Settling gone
// (ADR 0027) this is the only bound left on what a Follow drags in.
func TestDeliveryStopsAtTheHorizon(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	carol := createUser(t, ts, "carol")
	id := deliveryFixture(t, ts, st, alice, carol)

	// No settling window to wait out: it delivers the moment it airs.
	if !strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("a freshly aired episode did not deliver")
	}
	backdateAired(t, st, id, store.DeliveryHorizon-time.Hour)
	if !strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("an airing inside the horizon stopped delivering")
	}
	backdateAired(t, st, id, store.DeliveryHorizon+time.Hour)
	if strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("an airing past the horizon is still delivering")
	}
}

// TestUnAiringRemovesADelivery: delivery is computed, never stored, so
// taking an episode off the air takes it out of every follower's feed
// at once.
func TestUnAiringRemovesADelivery(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	carol := createUser(t, ts, "carol")
	deliveryFixture(t, ts, st, alice, carol)

	if !strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("not delivered to begin with")
	}
	postForm(t, ts, alice.sessionCreds(), "/me/episodes/lounge/unair", url.Values{}).Body.Close()
	if strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("an un-aired episode is still in a follower's feed")
	}
}

// TestMuteBeatsFollow: Mute means nothing from that Owner anywhere,
// including through a strand you follow (ADR 0019).
func TestMuteBeatsFollow(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	carol := createUser(t, ts, "carol")
	deliveryFixture(t, ts, st, alice, carol)

	if !strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("not delivered to begin with")
	}
	do(t, "PUT", ts.URL+"/me/mutes/alice", carol.sessionCreds(), nil, "").Body.Close()
	if strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("a muted owner's episode arrived through a followed strand")
	}
}

// TestOwnAiredEpisodeIsNotDeliveredTwice: alice follows the strand she
// airs into. Her episode is already hers; it must not also arrive as a
// delivery.
func TestOwnAiredEpisodeIsNotDeliveredTwice(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	airedEpisode(t, ts, st, alice, "lounge", "music")
	postForm(t, ts, alice.sessionCreds(), "/me/follows/music", url.Values{}).Body.Close()

	if n := strings.Count(feedXML(t, ts, alice), "<item>"); n != 1 {
		t.Fatalf("alice's feed has %d items, want exactly 1", n)
	}
}

// TestShareWinsOverDelivery: if an episode is both shared to you and
// delivered by a strand, it appears once, credited to the person who
// chose to send it.
func TestShareWinsOverDelivery(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	carol := createUser(t, ts, "carol")
	deliveryFixture(t, ts, st, alice, carol)
	share(t, ts, alice, "alice", "lounge", "carol").Body.Close()

	xml := feedXML(t, ts, carol)
	// Counted as items, not as "alice/lounge": that string is in the
	// GUID and again inside a feed-token enclosure URL.
	if n := strings.Count(xml, "<item>"); n != 1 {
		t.Fatalf("carol's feed has %d items, want exactly 1:\n%s", n, xml)
	}
	// Shared, so it is addressed in carol's own namespace.
	if !strings.Contains(xml, "/f/") || strings.Contains(xml, "/strands/music/") {
		t.Errorf("the share did not win the enclosure:\n%s", xml)
	}
}

// TestFollowedRowOnTheDashboard: a delivered row is credited to the
// strand and carries none of the owner's controls — the trap being that
// "not shared" used to mean "mine".
func TestFollowedRowOnTheDashboard(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	carol := createUser(t, ts, "carol")
	id := deliveryFixture(t, ts, st, alice, carol)

	page := dashboardHTML(t, ts, carol.sessionCreds())
	if !strings.Contains(page, "from the") || !strings.Contains(page, `href="/strands/music"`) {
		t.Fatalf("no strand credit on the followed row:\n%s", page)
	}
	if !strings.Contains(page, "Briefings for alice") {
		t.Error("the followed row is not credited to the owner's feed title")
	}
	if !strings.Contains(page, "/strands/music/"+id+".mp3") {
		t.Error("the player does not use the public audio address")
	}
	// None of the owner's controls, and nothing that would 404.
	for _, unwanted := range []string{
		"/me/episodes/lounge/air",
		"/me/episodes/lounge/characters",
	} {
		if strings.Contains(page, unwanted) {
			t.Errorf("a followed row offers %q", unwanted)
		}
	}
	// The delivered row is carol's only row, so the share box must not
	// be rendered at all. Checked by the input's class rather than the
	// data-share attribute, which also appears in the page's script.
	if strings.Contains(page, `class="share-to"`) {
		t.Error("a followed row offers the share box")
	}
	// And the JS must be told to skip it, or wiring throws on the first
	// missing control and takes every later row with it.
	if !strings.Contains(page, `data-followed="1"`) {
		t.Error("the followed row is not marked for the script to skip")
	}
}

// TestFollowedFeedVariant: same feed, narrower view (ADR 0005).
func TestFollowedFeedVariant(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	carol := createUser(t, ts, "carol")
	deliveryFixture(t, ts, st, alice, carol)
	publishEpisode(t, ts, carol, "mine-only", `{"title":"Mine Only"}`, "MP3!").Body.Close()

	resp := do(t, "GET", ts.URL+"/me/feed?filter=followed", carol.sessionCreds(), nil, "")
	defer resp.Body.Close()
	b := make([]byte, 8192)
	n, _ := resp.Body.Read(b)
	body := string(b[:n])
	if !strings.Contains(body, "lounge") {
		t.Errorf("filter=followed dropped the delivered episode:\n%s", body)
	}
	if strings.Contains(body, "mine-only") {
		t.Errorf("filter=followed included carol's own episode:\n%s", body)
	}
}
