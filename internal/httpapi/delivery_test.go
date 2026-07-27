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

// settleAired backdates an Airing past the settling window so the next
// read freezes its Vouch count. Time is the one thing these tests
// cannot wait for.
func settleAired(t *testing.T, st store.Store, id string) {
	t.Helper()
	a, err := st.GetAiring(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	a.AiredAt = time.Now().UTC().Add(-store.SettleWindow - time.Hour)
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

// deliveryFixture: alice airs an episode on music, bobby vouches for it,
// and carol follows the strand. Returns the airing id.
func deliveryFixture(t *testing.T, ts *httptest.Server, st store.Store, alice, bobby, carol account, bar string) string {
	t.Helper()
	id := airedEpisode(t, ts, st, alice, "lounge", "music")
	postForm(t, ts, bobby.sessionCreds(), "/me/vouches/"+id, url.Values{}).Body.Close()
	settleAired(t, st, id)
	postForm(t, ts, carol.sessionCreds(), "/me/follows/music", url.Values{"bar": {bar}}).Body.Close()
	return id
}

// TestFollowDeliversIntoThePersonalFeed is the whole point of ADR 0019:
// following a strand puts what cleared your Bar into your own feed,
// with no second subscription.
func TestFollowDeliversIntoThePersonalFeed(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")
	carol := createUser(t, ts, "carol")
	id := deliveryFixture(t, ts, st, alice, bobby, carol, "1")

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

// TestDeliveryRespectsTheBar: the Bar is the whole reason a Follow is
// not a firehose.
func TestDeliveryRespectsTheBar(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")
	carol := createUser(t, ts, "carol")
	deliveryFixture(t, ts, st, alice, bobby, carol, "1")

	if !strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("one vouch did not clear a bar of one")
	}
	// Raising the Bar filters retroactively — you asked for less.
	postForm(t, ts, carol.sessionCreds(), "/me/follows/music", url.Values{"bar": {"2"}}).Body.Close()
	if strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("raising the bar to two did not drop a one-vouch episode")
	}
	// Zero is the firehose.
	postForm(t, ts, carol.sessionCreds(), "/me/follows/music", url.Values{"bar": {"0"}}).Body.Close()
	if !strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("a bar of zero did not take an aired episode")
	}
}

// TestUnsettledIsNotDelivered: nothing may be inserted into a
// listener's past, so an Airing delivers only after its day is up.
func TestUnsettledIsNotDelivered(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")
	carol := createUser(t, ts, "carol")

	id := airedEpisode(t, ts, st, alice, "lounge", "music")
	postForm(t, ts, bobby.sessionCreds(), "/me/vouches/"+id, url.Values{}).Body.Close()
	postForm(t, ts, carol.sessionCreds(), "/me/follows/music", url.Values{"bar": {"1"}}).Body.Close()

	if strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("an airing delivered before it settled")
	}
	// The feed poll is also what settles it (ADR 0016's pattern).
	settleAired(t, st, id)
	if !strings.Contains(feedXML(t, ts, carol), "lounge") {
		t.Fatal("a settled airing above the bar did not deliver")
	}
	a, err := st.GetAiring(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Settled || a.VouchesAtSettle != 1 {
		t.Errorf("the feed poll did not settle the airing: %+v", a)
	}
}

// TestUnAiringRemovesADelivery: delivery is computed, never stored, so
// taking an episode off the air takes it out of every follower's feed
// at once.
func TestUnAiringRemovesADelivery(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")
	carol := createUser(t, ts, "carol")
	deliveryFixture(t, ts, st, alice, bobby, carol, "1")

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
	bobby := createUser(t, ts, "bobby")
	carol := createUser(t, ts, "carol")
	deliveryFixture(t, ts, st, alice, bobby, carol, "1")

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
	bobby := createUser(t, ts, "bobby")
	id := airedEpisode(t, ts, st, alice, "lounge", "music")
	postForm(t, ts, bobby.sessionCreds(), "/me/vouches/"+id, url.Values{}).Body.Close()
	settleAired(t, st, id)
	postForm(t, ts, alice.sessionCreds(), "/me/follows/music", url.Values{"bar": {"1"}}).Body.Close()

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
	bobby := createUser(t, ts, "bobby")
	carol := createUser(t, ts, "carol")
	deliveryFixture(t, ts, st, alice, bobby, carol, "1")
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
	bobby := createUser(t, ts, "bobby")
	carol := createUser(t, ts, "carol")
	id := deliveryFixture(t, ts, st, alice, bobby, carol, "1")

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
	bobby := createUser(t, ts, "bobby")
	carol := createUser(t, ts, "carol")
	deliveryFixture(t, ts, st, alice, bobby, carol, "1")
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
