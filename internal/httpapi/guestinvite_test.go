package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mintEpisodeLink creates an Invite carrying one Episode and returns its
// public URL — the "Send as a link" button on the Dashboard.
func mintEpisodeLink(t *testing.T, ts *httptest.Server, a account, owner, slug string) string {
	t.Helper()
	resp := do(t, "POST", ts.URL+"/me/invites", a.publishCreds(),
		strings.NewReader(`{"owner":"`+owner+`","slug":"`+slug+`"}`), "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mint link: %d %s", resp.StatusCode, b)
	}
	var v struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.URL
}

// TestGuestHearsOneEpisode is the heart of ADR 0014: someone with no
// account plays the Episode an Invite carries, and can learn nothing
// else from the link they were given.
func TestGuestHearsOneEpisode(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning",
		`{"title":"Sleepy Rabbits","description":"A bedtime story.","duration_seconds":402}`,
		"AUDIOBYTES")
	resp.Body.Close()
	resp = do(t, "PUT", ts.URL+"/me/image", alice.publishCreds(), strings.NewReader("JPEGBYTES"), "image/jpeg")
	resp.Body.Close()

	url := mintEpisodeLink(t, ts, alice, "alice", "2026-07-06-morning")

	// The page leads with the episode and its player — no credential
	// of any kind was presented.
	resp, body := getBody(t, url, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("guest page: %d\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"Sleepy Rabbits", "A bedtime story.", "alice",
		`<audio controls preload="none"`, `data-seconds="402"`, "/audio.mp3", "player.js",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("guest page missing %q:\n%s", want, body)
		}
	}
	// ...and nothing about the wider feed, nor a way to keep the audio.
	for _, forbidden := range []string{"/f/", "feed.xml", "qr.png", "antennapod", "download"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("guest page leaks %q:\n%s", forbidden, body)
		}
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("guest page Referrer-Policy = %q, want no-referrer", got)
	}

	// The audio plays, seekably, still with no account.
	resp, audio := getBody(t, url+"/audio.mp3", "")
	if resp.StatusCode != http.StatusOK || audio != "AUDIOBYTES" {
		t.Fatalf("guest audio: %d %q", resp.StatusCode, audio)
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("guest audio sends a referrer")
	}
	resp, cover := getBody(t, url+"/cover", "")
	if resp.StatusCode != http.StatusOK || cover != "JPEGBYTES" {
		t.Fatalf("guest cover: %d %q", resp.StatusCode, cover)
	}
	// A capability URL must never sit in a shared cache.
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("guest cover Cache-Control = %q, want private", cc)
	}

	// The token unlocks that Episode and no other.
	resp = publishEpisode(t, ts, alice, "2026-07-07-morning", `{"title":"Second"}`, "OTHER")
	resp.Body.Close()
	resp, body = getBody(t, url, "")
	if strings.Contains(body, "Second") {
		t.Errorf("guest page exposes another episode:\n%s", body)
	}

	// A made-up token is a plain 404, like any other missing page.
	resp, _ = getBody(t, ts.URL+"/invites/deadbeefdeadbeefdeadbeefdeadbeef/audio.mp3", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown invite audio: got %d, want 404", resp.StatusCode)
	}
}

// TestOwnerRevokesAnotherMembersLink covers the lever ADR 0014 gives an
// Owner: Bob may forward Alice's Episode, and Alice may kill his link
// without deleting the Episode for everyone.
func TestOwnerRevokesAnotherMembersLink(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning", `{"title":"Sleepy Rabbits"}`, "AUDIO")
	resp.Body.Close()
	resp = share(t, ts, alice, "alice", "2026-07-06-morning", "bob")
	resp.Body.Close()

	// Bob has it in his feed, so he may pass it on (ADR 0006).
	url := mintEpisodeLink(t, ts, bob, "alice", "2026-07-06-morning")
	resp, _ = getBody(t, url, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bob's link: %d", resp.StatusCode)
	}
	token := url[strings.LastIndex(url, "/")+1:]

	// Alice sees it on her own episode row, attributed to Bob.
	req, _ := http.NewRequest("GET", ts.URL+"/me", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: alice.Session})
	req.Header.Set("Accept", "text/html")
	htmlResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(htmlResp.Body)
	htmlResp.Body.Close()
	if !strings.Contains(string(raw), `data-token="`+token+`"`) ||
		!strings.Contains(string(raw), "<strong>bob</strong>") {
		t.Fatalf("alice's dashboard does not show bob's link:\n%s", raw)
	}

	// A third member has no business revoking it.
	carol := createUser(t, ts, "carol")
	resp = do(t, "DELETE", ts.URL+"/me/invites/"+token, carol.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("carol revoking someone else's link: got %d, want 404", resp.StatusCode)
	}

	// Alice can, because the Episode is hers.
	resp = do(t, "DELETE", ts.URL+"/me/invites/"+token, alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("alice revoking bob's link: got %d, want 204", resp.StatusCode)
	}
	resp, _ = getBody(t, url, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("revoked link still plays: got %d", resp.StatusCode)
	}
	// ...and the Episode itself is untouched, in her feed and in Bob's.
	if xml := fetchFeed(t, bob, ""); !strings.Contains(xml, "Sleepy Rabbits") {
		t.Errorf("revoking a link removed the episode from bob's feed")
	}
}

// TestOwnerDeleteReachesGuests: the backstop the whole model leans on —
// an Owner's delete kills a Guest link, because the link is a reference
// resolved server-side (ADR 0006).
func TestOwnerDeleteReachesGuests(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning", `{"title":"Sleepy Rabbits"}`, "AUDIO")
	resp.Body.Close()
	url := mintEpisodeLink(t, ts, alice, "alice", "2026-07-06-morning")

	resp = do(t, "DELETE", ts.URL+"/me/episodes/2026-07-06-morning", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete episode: %d", resp.StatusCode)
	}

	resp, body := getBody(t, url+"/audio.mp3", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("guest audio survives the owner's delete: %d %q", resp.StatusCode, body)
	}
	// The page itself survives as a bare invite — the payload silently
	// vanishes, exactly as a dead Share does.
	resp, body = getBody(t, url, "")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "Sleepy Rabbits") {
		t.Errorf("deleted episode still named on the guest page: %d\n%s", resp.StatusCode, body)
	}
}
