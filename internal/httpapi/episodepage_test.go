package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// feedBase is the capability namespace of a feed: the feed URL minus
// "/feed.xml". Episode Pages and enclosures hang off it.
func feedBase(a account) string { return strings.TrimSuffix(a.FeedURL, "/feed.xml") }

func getBody(t *testing.T, url, creds string) (*http.Response, string) {
	t.Helper()
	resp := do(t, "GET", url, creds, nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// TestEpisodePage covers the browser half of ADR 0013: one address
// serves two representations — "{slug}.mp3" is the enclosure, the bare
// "{slug}" is a readable page with an inline Player — and both obey the
// same visibility rule as the feed.
func TestEpisodePage(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning",
		`{"title":"Morning Update","description":"What happened overnight.","duration_seconds":402}`,
		"FAKEMP3BYTES")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish: %d", resp.StatusCode)
	}

	page := feedBase(alice) + "/alice/2026-07-06-morning"

	resp, body := getBody(t, page, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("episode page: %d, want 200\n%s", resp.StatusCode, body)
	}
	// The markup the Player is built from. preload="none" is what keeps
	// a long Dashboard at zero audio bytes, and data-seconds is what
	// lets the scrubber render before any byte is fetched — both are
	// load-bearing, not decoration.
	for _, want := range []string{
		`src="/f/` + feedToken(alice) + `/alice/2026-07-06-morning.mp3"`,
		`preload="none"`,
		`data-seconds="402"`,
		`data-key="alice/2026-07-06-morning"`,
		"Morning Update",
		"What happened overnight.",
		"player.js",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("episode page missing %q:\n%s", want, body)
		}
	}
	// The page must not invite anyone to pass its URL on: it carries the
	// Feed Token, so a copy-link button would be a foot-gun (ADR 0013).
	if strings.Contains(body, "data-copy") {
		t.Errorf("episode page offers a copy-link button:\n%s", body)
	}

	// The enclosure still serves audio, unchanged by the split.
	resp, body = getBody(t, page+".mp3", "")
	if resp.StatusCode != http.StatusOK || body != "FAKEMP3BYTES" {
		t.Fatalf("enclosure: %d %q", resp.StatusCode, body)
	}

	// Slugs that do not exist, and paths that are not slugs at all.
	for _, bad := range []string{"/alice/nope", "/alice/NOT_A_SLUG", "/nobody/2026-07-06-morning"} {
		resp, _ := getBody(t, feedBase(alice)+bad, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404", bad, resp.StatusCode)
		}
	}
}

// TestEpisodePageFollowsSharing checks the page is reachable exactly
// where the audio is: a stranger's feed cannot render someone else's
// Episode until it has been Shared into that feed.
func TestEpisodePageFollowsSharing(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning", `{"title":"Alice Morning"}`, "A")
	resp.Body.Close()

	// Bob's own token, Alice's episode: not shared, so it does not exist.
	inBobsFeed := feedBase(bob) + "/alice/2026-07-06-morning"
	resp, _ = getBody(t, inBobsFeed, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unshared episode page: got %d, want 404", resp.StatusCode)
	}
	resp, _ = getBody(t, inBobsFeed+".mp3", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unshared enclosure: got %d, want 404", resp.StatusCode)
	}

	resp = share(t, ts, alice, "alice", "2026-07-06-morning", "bob")
	resp.Body.Close()

	resp, body := getBody(t, inBobsFeed, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shared episode page: got %d, want 200\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Alice Morning") {
		t.Errorf("shared episode page does not name the episode:\n%s", body)
	}
	// The audio it points at is inside Bob's namespace, not Alice's:
	// his token is the only one he holds.
	if !strings.Contains(body, `src="/f/`+feedToken(bob)+`/alice/2026-07-06-morning.mp3"`) {
		t.Errorf("shared episode page points outside the reader's feed:\n%s", body)
	}
}

// TestDashboardCarriesPlayers checks the Dashboard grew a Player per
// Episode row and a link to each Episode Page.
func TestDashboardCarriesPlayers(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning",
		`{"title":"Morning Update","duration_seconds":402}`, "A")
	resp.Body.Close()

	// /me answers browsers with HTML and everything else with JSON, so
	// the Accept header is what selects the Dashboard.
	req, _ := http.NewRequest("GET", ts.URL+"/me", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: alice.Session})
	req.Header.Set("Accept", "text/html")
	htmlResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(htmlResp.Body)
	htmlResp.Body.Close()
	body := string(raw)
	if htmlResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard: %d", htmlResp.StatusCode)
	}
	for _, want := range []string{
		`href="/me/episodes/alice/2026-07-06-morning"`,
		`src="/me/episodes/alice/2026-07-06-morning.mp3"`,
		`<audio controls preload="none"`,
		`data-seconds="402"`,
		"player.js",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q:\n%s", want, body)
		}
	}
	// A signed-in listener must never be one Ctrl-C away from handing
	// over their whole feed, so nothing they click or play on the
	// Dashboard is a capability URL. The subscribe box still shows the
	// feed URL — that one is the point, and it is deliberate.
	if strings.Contains(body, `href="/f/`+feedToken(alice)+`/alice/`) ||
		strings.Contains(body, `src="/f/`+feedToken(alice)+`/alice/`) {
		t.Errorf("dashboard puts a capability URL on an episode:\n%s", body)
	}
}

// TestMyEpisodeIsSessionOnly covers the signed-in twin of the capability
// routes: same page, same audio, authorised by the session cookie, with
// no Feed Token in the URL to leak.
func TestMyEpisodeIsSessionOnly(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning",
		`{"title":"Alice Morning","duration_seconds":402}`, "AUDIOBYTES")
	resp.Body.Close()

	page := ts.URL + "/me/episodes/alice/2026-07-06-morning"

	// The owner, signed in: page and audio, with URLs that stay under /me.
	resp, body := getBody(t, page, alice.sessionCreds())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("own episode page: %d\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"Alice Morning",
		`src="/me/episodes/alice/2026-07-06-morning.mp3"`,
		"Back to your dashboard",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("session episode page missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/f/"+feedToken(alice)+"/alice/") {
		t.Errorf("session episode page leaks a capability URL:\n%s", body)
	}
	// ...and none of the capability-URL warnings, which would be a lie here.
	if strings.Contains(body, "key to the whole feed") {
		t.Errorf("session episode page warns about a capability it does not have:\n%s", body)
	}

	resp, body = getBody(t, page+".mp3", alice.sessionCreds())
	if resp.StatusCode != http.StatusOK || body != "AUDIOBYTES" {
		t.Fatalf("own episode audio: %d %q", resp.StatusCode, body)
	}

	// Another member, signed in, with no Share: it does not exist.
	for _, url := range []string{page, page + ".mp3"} {
		resp, _ := getBody(t, url, bob.sessionCreds())
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s as a stranger: got %d, want 404", url, resp.StatusCode)
		}
	}

	// No session at all: the browser is sent to log in, never served.
	resp, _ = getBody(t, page, "")
	if resp.StatusCode == http.StatusOK {
		t.Errorf("episode page served without a session")
	}

	// Once shared, the same URL works for Bob under his own session.
	resp = share(t, ts, alice, "alice", "2026-07-06-morning", "bob")
	resp.Body.Close()
	resp, body = getBody(t, page, bob.sessionCreds())
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Alice Morning") {
		t.Fatalf("shared episode page for bob: %d\n%s", resp.StatusCode, body)
	}
}

// TestSessionCoverKeepsTokenOutOfPages checks the last capability URL a
// signed-in page used to carry: its Cover Art. The image is the same
// bytes either way; only the address and the cache posture differ.
func TestSessionCoverKeepsTokenOutOfPages(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	resp := do(t, "PUT", ts.URL+"/me/image", alice.publishCreds(), strings.NewReader("JPEGBYTES"), "image/jpeg")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("set cover: %d", resp.StatusCode)
	}

	resp, body := getBody(t, ts.URL+"/me/image", alice.sessionCreds())
	if resp.StatusCode != http.StatusOK || body != "JPEGBYTES" {
		t.Fatalf("GET /me/image: %d %q", resp.StatusCode, body)
	}
	// A URL behind a session must never land in a shared cache.
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("GET /me/image: Cache-Control = %q, want private", cc)
	}
	resp, _ = getBody(t, ts.URL+"/me/image", "")
	if resp.StatusCode == http.StatusOK {
		t.Errorf("cover served without a session")
	}

	// The Dashboard shows the cover without minting a capability for it.
	req, _ := http.NewRequest("GET", ts.URL+"/me", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: alice.Session})
	req.Header.Set("Accept", "text/html")
	htmlResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(htmlResp.Body)
	htmlResp.Body.Close()
	if !strings.Contains(string(raw), `src="/me/image"`) {
		t.Errorf("dashboard does not use the session cover URL")
	}
	if strings.Contains(string(raw), "/f/"+feedToken(alice)+"/cover") {
		t.Errorf("dashboard still fetches its cover through the Feed Token")
	}
}

// TestCapabilityPagesSendNoReferrer guards the leak that a link inside
// an episode description would otherwise cause: the Referer header would
// carry the Feed Token — the whole feed — to the destination site.
func TestCapabilityPagesSendNoReferrer(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	resp := publishEpisode(t, ts, alice, "2026-07-06-morning", `{"title":"Morning"}`, "A")
	resp.Body.Close()

	for _, path := range []string{
		"", "/feed.xml", "/alice/2026-07-06-morning", "/alice/2026-07-06-morning.mp3",
	} {
		resp, _ := getBody(t, feedBase(alice)+path, "")
		if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("GET /f/…%s: Referrer-Policy = %q, want no-referrer", path, got)
		}
	}
}

// feedToken pulls the capability out of the account's feed URL.
func feedToken(a account) string {
	parts := strings.Split(strings.TrimSuffix(a.FeedURL, "/feed.xml"), "/f/")
	return parts[len(parts)-1]
}
