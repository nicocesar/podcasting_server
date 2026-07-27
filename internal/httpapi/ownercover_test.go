package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// setCover uploads a's Cover Art and returns the exact bytes stored, so
// a later fetch can be compared against them.
func setCover(t *testing.T, ts *httptest.Server, a account, w, h int) []byte {
	t.Helper()
	jpg := smallJPEG(t, w, h) // under the 3000px ceiling: stored verbatim
	resp := do(t, "PUT", ts.URL+"/me/image", a.publishCreds(), bytes.NewReader(jpg), "image/jpeg")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("set cover for %s: %d", a.ID, resp.StatusCode)
	}
	return jpg
}

// TestSharedEpisodeKeepsOwnerCover is the three-user chain of ADR 0025:
// nico makes an Episode, hands it to ldipenti, who forwards it to
// focagorda. In both recipients' feeds the item wears *nico's* art —
// the Owner's, not the Sharer's and not the reader's — because the art
// is provenance and only the Owner may set it (ADR 0006).
func TestSharedEpisodeKeepsOwnerCover(t *testing.T) {
	ts := newTestServer(t)
	nico := createUser(t, ts, "nico")
	ldipenti := createUser(t, ts, "ldipenti")
	focagorda := createUser(t, ts, "focagorda")

	// Three distinct covers, so a feed showing the wrong one is a test
	// failure rather than a coincidence.
	nicoArt := setCover(t, ts, nico, 300, 300)
	setCover(t, ts, ldipenti, 320, 320)
	setCover(t, ts, focagorda, 340, 340)

	resp := publishEpisode(t, ts, nico, "2026-07-27-morning",
		`{"title":"Nico Morning","duration_seconds":120}`, "FAKEMP3BYTES")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish: %d", resp.StatusCode)
	}

	// nico -> ldipenti -> focagorda. The second hop is open forwarding:
	// ldipenti shares an Episode they do not own (ADR 0006).
	for _, hop := range []struct {
		from account
		to   string
	}{{nico, "ldipenti"}, {ldipenti, "focagorda"}} {
		resp := share(t, ts, hop.from, "nico", "2026-07-27-morning", hop.to)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("share to %s: %d", hop.to, resp.StatusCode)
		}
	}

	for _, reader := range []account{ldipenti, focagorda} {
		body := fetchFeed(t, reader, "")
		// The item carries nico's art, addressed inside *this* reader's
		// own capability namespace — never nico's token.
		want := `href="` + ts.URL + "/f/" + feedToken(reader) + `/u/nico/cover"`
		if !strings.Contains(body, want) {
			t.Errorf("%s's feed missing owner cover %s:\n%s", reader.ID, want, body)
		}
		// The channel image stays the reader's own: their feed is still
		// their feed, whatever art the individual episodes wear.
		chanImg := `href="` + ts.URL + "/f/" + feedToken(reader) + `/cover"`
		if !strings.Contains(body, chanImg) {
			t.Errorf("%s's feed lost its channel image %s:\n%s", reader.ID, chanImg, body)
		}
		if strings.Contains(body, "/u/ldipenti/cover") {
			t.Errorf("%s's feed shows the Sharer's art, not the Owner's:\n%s", reader.ID, body)
		}

		// And the URL resolves, to nico's actual bytes.
		got, art := getBody(t, ts.URL+"/f/"+feedToken(reader)+"/u/nico/cover", "")
		if got.StatusCode != http.StatusOK {
			t.Fatalf("%s fetching nico's cover: %d", reader.ID, got.StatusCode)
		}
		if art != string(nicoArt) {
			t.Errorf("%s got %d bytes of cover, want nico's %d", reader.ID, len(art), len(nicoArt))
		}
	}
}

// TestOwnEpisodesCarryNoItemImage keeps the feed lean: an Owner's own
// Episodes already wear the channel's art, so repeating it per item
// would be bytes that say nothing.
func TestOwnEpisodesCarryNoItemImage(t *testing.T) {
	ts := newTestServer(t)
	nico := createUser(t, ts, "nico")
	setCover(t, ts, nico, 300, 300)

	resp := publishEpisode(t, ts, nico, "2026-07-27-solo",
		`{"title":"Solo","duration_seconds":60}`, "FAKEMP3BYTES")
	resp.Body.Close()

	body := fetchFeed(t, nico, "")
	if strings.Contains(body, "/u/nico/cover") {
		t.Errorf("own episode carries a redundant item image:\n%s", body)
	}
	// Exactly one image element, the channel's.
	if n := strings.Count(body, "<itunes:image"); n != 1 {
		t.Errorf("itunes:image count = %d, want 1 (channel only):\n%s", n, body)
	}
}

// TestOwnerWithoutCoverFallsBack: an Owner who never uploaded art adds
// no item image, so the reader's channel art stands. A shared Episode
// must never render coverless.
func TestOwnerWithoutCoverFallsBack(t *testing.T) {
	ts := newTestServer(t)
	nico := createUser(t, ts, "nico") // no cover
	ldipenti := createUser(t, ts, "ldipenti")
	setCover(t, ts, ldipenti, 300, 300)

	resp := publishEpisode(t, ts, nico, "2026-07-27-bare",
		`{"title":"Bare","duration_seconds":60}`, "FAKEMP3BYTES")
	resp.Body.Close()
	resp = share(t, ts, nico, "nico", "2026-07-27-bare", "ldipenti")
	resp.Body.Close()

	body := fetchFeed(t, ldipenti, "")
	if strings.Contains(body, "/u/nico/cover") {
		t.Errorf("coverless owner still got an item image:\n%s", body)
	}
	if n := strings.Count(body, "<itunes:image"); n != 1 {
		t.Errorf("itunes:image count = %d, want 1 (channel only):\n%s", n, body)
	}
}

// TestOwnerCoverNeedsAShare is the authorisation: the route exposes a
// stranger's art to nobody. Holding a Feed Token lets you see the art
// behind Episodes in *that* feed, and nothing else.
func TestOwnerCoverNeedsAShare(t *testing.T) {
	ts := newTestServer(t)
	nico := createUser(t, ts, "nico")
	setCover(t, ts, nico, 300, 300)
	stranger := createUser(t, ts, "stranger")

	resp, _ := getBody(t, ts.URL+"/f/"+feedToken(stranger)+"/u/nico/cover", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("stranger reading nico's cover: %d, want 404", resp.StatusCode)
	}

	// A muted Owner is invisible too, art included, even though the
	// Share technically exists.
	victim := createUser(t, ts, "victim")
	pub := publishEpisode(t, ts, nico, "2026-07-27-muted",
		`{"title":"Muted","duration_seconds":60}`, "FAKEMP3BYTES")
	pub.Body.Close()
	sh := share(t, ts, nico, "nico", "2026-07-27-muted", "victim")
	sh.Body.Close()
	mute := do(t, "PUT", ts.URL+"/me/mutes/nico", victim.publishCreds(), nil, "")
	mute.Body.Close()
	if mute.StatusCode != http.StatusNoContent && mute.StatusCode != http.StatusOK {
		t.Fatalf("mute: %d", mute.StatusCode)
	}

	resp, _ = getBody(t, ts.URL+"/f/"+feedToken(victim)+"/u/nico/cover", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("muted owner's cover: %d, want 404", resp.StatusCode)
	}
}

// TestSharedEpisodePageShowsOwnerCover: the web surfaces agree with the
// feed. On the signed-in address the URL carries no capability.
func TestSharedEpisodePageShowsOwnerCover(t *testing.T) {
	ts := newTestServer(t)
	nico := createUser(t, ts, "nico")
	ldipenti := createUser(t, ts, "ldipenti")
	setCover(t, ts, nico, 300, 300)
	setCover(t, ts, ldipenti, 320, 320)

	resp := publishEpisode(t, ts, nico, "2026-07-27-page",
		`{"title":"Page","duration_seconds":60}`, "FAKEMP3BYTES")
	resp.Body.Close()
	resp = share(t, ts, nico, "nico", "2026-07-27-page", "ldipenti")
	resp.Body.Close()

	// Signed in: /me/u/{owner}/cover, no Feed Token in the markup.
	_, page := getBody(t, ts.URL+"/me/episodes/nico/2026-07-27-page", ldipenti.sessionCreds())
	if !strings.Contains(page, "/me/u/nico/cover") {
		t.Errorf("session episode page missing owner cover:\n%s", page)
	}
	if strings.Contains(page, feedToken(ldipenti)) {
		t.Errorf("session page leaked the Feed Token:\n%s", page)
	}

	// The dashboard row agrees.
	dash := dashboard(t, ts, ldipenti, "")
	if !strings.Contains(dash, "/me/u/nico/cover") {
		t.Errorf("dashboard row missing owner cover:\n%s", dash)
	}

	// And the capability address serves it too.
	got, art := getBody(t, ts.URL+"/me/u/nico/cover", ldipenti.sessionCreds())
	if got.StatusCode != http.StatusOK || len(art) == 0 {
		t.Fatalf("GET /me/u/nico/cover: %d (%d bytes)", got.StatusCode, len(art))
	}
}
