package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

const adminToken = "test-admin-token"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:      st,
		AdminToken: adminToken,
		// The real embedded assets, via the filesystem.
		Assets: os.DirFS("../../cmd/server"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// do sends a request. creds is "" (no auth), "bearer:<token>" (admin), or
// "user:secret" (basic auth).
func do(t *testing.T, method, url, creds string, body io.Reader, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token, ok := strings.CutPrefix(creds, "bearer:"); ok {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if creds != "" {
		user, pass, _ := strings.Cut(creds, ":")
		req.SetBasicAuth(user, pass)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// account is what the admin provisioning endpoint hands back once.
type account struct {
	ID              string `json:"id"`
	ReadCredentials string `json:"read_credentials"`
	PublishToken    string `json:"publish_token"`
	FeedURL         string `json:"feed_url"`
	CoverURL        string `json:"cover_url"`
}

// publishCreds is the basic-auth form of the account's publish token.
func (a account) publishCreds() string { return a.ID + ":" + a.PublishToken }

func createUser(t *testing.T, ts *httptest.Server, id string) account {
	t.Helper()
	resp := do(t, "PUT", ts.URL+"/admin/users/"+id, "bearer:"+adminToken,
		strings.NewReader(`{"title":"Briefings for `+id+`"}`), "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create user %s: %d %s", id, resp.StatusCode, b)
	}
	var a account
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	return a
}

func publishEpisode(t *testing.T, ts *httptest.Server, a account, slug, metadata, audio string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("metadata", metadata); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("audio", slug+".mp3")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte(audio))
	mw.Close()
	return do(t, "PUT", ts.URL+"/me/episodes/"+slug, a.publishCreds(), &buf, mw.FormDataContentType())
}

func share(t *testing.T, ts *httptest.Server, from account, owner, slug, to string) *http.Response {
	t.Helper()
	return do(t, "POST", ts.URL+"/me/feed/"+owner+"/"+slug+"/share", from.publishCreds(),
		strings.NewReader(`{"to":"`+to+`"}`), "application/json")
}

func fetchFeed(t *testing.T, ts *httptest.Server, a account, params string) string {
	t.Helper()
	resp := do(t, "GET", ts.URL+"/users/"+a.ID+"/feed.xml"+params, a.ReadCredentials, nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("feed %s%s: %d %s", a.ID, params, resp.StatusCode, body)
	}
	return string(body)
}

func TestProvisioning(t *testing.T) {
	ts := newTestServer(t)

	// Admin endpoints reject everything but the admin token.
	for _, creds := range []string{"", "bearer:wrong-token"} {
		resp := do(t, "GET", ts.URL+"/admin/users", creds, nil, "")
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("admin with creds %q: got %d, want 401", creds, resp.StatusCode)
		}
	}

	alice := createUser(t, ts, "alice")
	if alice.ID != "alice" || !strings.HasPrefix(alice.ReadCredentials, "alice:") ||
		alice.PublishToken == "" || !strings.Contains(alice.FeedURL, "/users/alice/feed.xml") {
		t.Fatalf("unexpected account: %+v", alice)
	}

	// Creating the same user again must not rotate credentials.
	resp := do(t, "PUT", ts.URL+"/admin/users/alice", "bearer:"+adminToken, strings.NewReader("{}"), "application/json")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("recreate user: got %d, want 409", resp.StatusCode)
	}

	// GET /me works with the publish token, not the read credential, and
	// never leaks credential hashes.
	resp = do(t, "GET", ts.URL+"/me", alice.publishCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"id": "alice"`) && !strings.Contains(string(body), `"id":"alice"`) {
		t.Fatalf("GET /me: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "hash") || strings.Contains(string(body), alice.PublishToken) {
		t.Errorf("GET /me leaks secrets: %s", body)
	}
	resp = do(t, "GET", ts.URL+"/me", alice.ReadCredentials, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("GET /me with read credential: got %d, want 403", resp.StatusCode)
	}
}

func TestAuthBoundaries(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")

	cases := []struct {
		name, method, path, creds string
		want                      int
	}{
		{"no creds on feed", "GET", "/users/alice/feed.xml", "", 401},
		{"bad creds on feed", "GET", "/users/alice/feed.xml", "alice:wrong", 401},
		{"read cred reads feed", "GET", "/users/alice/feed.xml", alice.ReadCredentials, 200},
		{"publish token reads feed", "GET", "/users/alice/feed.xml", alice.publishCreds(), 200},
		{"bob cannot read alice's feed", "GET", "/users/alice/feed.xml", bob.ReadCredentials, 404},
		{"read cred cannot publish", "PUT", "/me", alice.ReadCredentials, 403},
		{"no creds on healthz ok", "GET", "/healthz", "", 200},
	}
	for _, c := range cases {
		resp := do(t, c.method, ts.URL+c.path, c.creds, nil, "")
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: got %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

func TestPublicSurface(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	resp := do(t, "PUT", ts.URL+"/me/image", alice.publishCreds(), strings.NewReader("JPEG"), "image/jpeg")
	resp.Body.Close()
	resp = publishEpisode(t, ts, alice, "2026-07-06-morning",
		`{"title":"Secret Episode Title","description":"Secret summary."}`, "MP3")
	resp.Body.Close()

	coverPath := strings.TrimPrefix(alice.CoverURL, ts.URL)

	// Public Surface: landing page, cover by secret, static assets. No
	// per-user pages without credentials (ADR 0005).
	for path, want := range map[string]int{
		"/":                 200,
		coverPath:           200,
		"/covers/wrong":     404,
		"/static/style.css": 200,
		"/users/alice":      401,
		"/no/such/path":     404,
	} {
		resp := do(t, "GET", ts.URL+path, "", nil, "")
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s without creds: got %d, want %d", path, resp.StatusCode, want)
		}
	}

	// The landing page enumerates nothing.
	resp = do(t, "GET", ts.URL+"/", "", nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "alice") {
		t.Errorf("landing page lists users:\n%s", body)
	}

	// The authenticated user page shows identity and feed URL, never
	// episode data.
	resp = do(t, "GET", ts.URL+"/users/alice", alice.ReadCredentials, nil, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	for _, want := range []string{"Briefings for alice", "/users/alice/feed.xml", coverPath} {
		if !strings.Contains(page, want) {
			t.Errorf("user page missing %q:\n%s", want, page)
		}
	}
	for _, leak := range []string{"Secret Episode Title", "Secret summary", "2026-07-06-morning"} {
		if strings.Contains(page, leak) {
			t.Errorf("user page leaks episode data %q:\n%s", leak, page)
		}
	}

	// Content stays authenticated.
	for _, path := range []string{
		"/users/alice/feed.xml",
		"/users/alice/episodes/2026-07-06-morning.mp3",
	} {
		resp := do(t, "GET", ts.URL+path, "", nil, "")
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("GET %s without creds: got %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestPublishFeedAndDownload(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	meta := `{"title":"Morning Briefing","description":"The news.","published_at":"2026-07-06T08:00:00Z","duration_seconds":300}`
	resp := publishEpisode(t, ts, alice, "2026-07-06-morning", meta, "FAKEMP3BYTES")
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("publish: %d %s", resp.StatusCode, b)
	}
	var ep struct {
		Owner     string `json:"owner"`
		Slug      string `json:"slug"`
		AudioSize int64  `json:"audio_size"`
	}
	json.NewDecoder(resp.Body).Decode(&ep)
	resp.Body.Close()
	if ep.Owner != "alice" || ep.Slug != "2026-07-06-morning" || ep.AudioSize != int64(len("FAKEMP3BYTES")) {
		t.Fatalf("unexpected episode: %+v", ep)
	}

	// Republish same slug → 200 (replace), not a new episode.
	resp = publishEpisode(t, ts, alice, "2026-07-06-morning", meta, "NEWBYTES")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("republish: got %d, want 200", resp.StatusCode)
	}

	// Feed contains the item exactly once, with enclosure, guid, author.
	xml := fetchFeed(t, ts, alice, "")
	for _, want := range []string{
		"<title>Morning Briefing</title>",
		`/users/alice/episodes/2026-07-06-morning.mp3"`,
		`length="8"`, // len("NEWBYTES")
		`type="audio/mpeg"`,
		`isPermaLink="false"`,
		"alice/2026-07-06-morning",
		"<itunes:author>alice</itunes:author>",
		"<itunes:duration>300</itunes:duration>",
		"Mon, 06 Jul 2026 08:00:00 +0000",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("feed missing %q\n%s", want, xml)
		}
	}
	if strings.Count(xml, "<item>") != 1 {
		t.Errorf("feed should have exactly 1 item:\n%s", xml)
	}

	// Download the audio.
	resp = do(t, "GET", ts.URL+"/users/alice/episodes/2026-07-06-morning.mp3", alice.ReadCredentials, nil, "")
	audio, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(audio) != "NEWBYTES" {
		t.Fatalf("download: %d %q", resp.StatusCode, audio)
	}

	// Range requests must work (podcast clients resume downloads).
	req, _ := http.NewRequest("GET", ts.URL+"/users/alice/episodes/2026-07-06-morning.mp3", nil)
	user, pass, _ := strings.Cut(alice.ReadCredentials, ":")
	req.SetBasicAuth(user, pass)
	req.Header.Set("Range", "bytes=3-")
	rangeResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	partial, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || string(partial) != "BYTES" {
		t.Fatalf("range: %d %q", rangeResp.StatusCode, partial)
	}
}

func TestSharingForwardingAndPropagation(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")
	carol := createUser(t, ts, "carol")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning",
		`{"title":"Alice Morning","published_at":"2026-07-06T08:00:00Z"}`, "ALICE-AUDIO")
	resp.Body.Close()
	resp = publishEpisode(t, ts, bob, "2026-07-06-morning",
		`{"title":"Bob Morning","published_at":"2026-07-06T09:00:00Z"}`, "BOB-AUDIO")
	resp.Body.Close()

	// Carol cannot share what is not in her feed.
	resp = share(t, ts, carol, "alice", "2026-07-06-morning", "bob")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("share from outside own feed: got %d, want 404", resp.StatusCode)
	}

	// Alice shares to Bob (owner share) → Bob forwards to Carol.
	resp = share(t, ts, alice, "alice", "2026-07-06-morning", "bob")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("share: got %d, want 201", resp.StatusCode)
	}
	resp = share(t, ts, bob, "alice", "2026-07-06-morning", "carol")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("forward: got %d, want 201", resp.StatusCode)
	}

	// Bob's feed mixes his own and Alice's episode; his own slug and
	// Alice's identical slug coexist with distinct GUIDs.
	xml := fetchFeed(t, ts, bob, "")
	for _, want := range []string{
		"<title>Alice Morning</title>", "<title>Bob Morning</title>",
		"alice/2026-07-06-morning", "bob/2026-07-06-morning",
		"<itunes:author>alice</itunes:author>", "<itunes:author>bob</itunes:author>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("bob's feed missing %q\n%s", want, xml)
		}
	}
	if strings.Count(xml, "<item>") != 2 {
		t.Errorf("bob's feed should have 2 items:\n%s", xml)
	}

	// Feed Variants (ADR 0005).
	if xml := fetchFeed(t, ts, bob, "?filter=mine"); strings.Contains(xml, "Alice Morning") || !strings.Contains(xml, "Bob Morning") {
		t.Errorf("filter=mine wrong:\n%s", xml)
	}
	if xml := fetchFeed(t, ts, bob, "?filter=shared"); !strings.Contains(xml, "Alice Morning") || strings.Contains(xml, "Bob Morning") {
		t.Errorf("filter=shared wrong:\n%s", xml)
	}
	if xml := fetchFeed(t, ts, bob, "?from=alice"); !strings.Contains(xml, "Alice Morning") || strings.Contains(xml, "Bob Morning") {
		t.Errorf("from=alice wrong:\n%s", xml)
	}
	if xml := fetchFeed(t, ts, bob, "?from=me"); strings.Contains(xml, "Alice Morning") || !strings.Contains(xml, "Bob Morning") {
		t.Errorf("from=me wrong:\n%s", xml)
	}

	// The JSON feed listing carries provenance: owner and sharer differ
	// on a forwarded episode.
	resp = do(t, "GET", ts.URL+"/me/feed?filter=shared", carol.publishCreds(), nil, "")
	var entries []struct {
		Owner  string `json:"owner"`
		Slug   string `json:"slug"`
		Sharer string `json:"sharer"`
	}
	json.NewDecoder(resp.Body).Decode(&entries)
	resp.Body.Close()
	if len(entries) != 1 || entries[0].Owner != "alice" || entries[0].Sharer != "bob" {
		t.Fatalf("carol's provenance wrong: %+v", entries)
	}

	// Carol can download Alice's audio through the share; Bob's own
	// episode stays invisible to her.
	resp = do(t, "GET", ts.URL+"/users/alice/episodes/2026-07-06-morning.mp3", carol.ReadCredentials, nil, "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "ALICE-AUDIO" {
		t.Fatalf("carol download shared audio: %d %q", resp.StatusCode, b)
	}
	resp = do(t, "GET", ts.URL+"/users/bob/episodes/2026-07-06-morning.mp3", carol.ReadCredentials, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("carol downloads unshared audio: got %d, want 404", resp.StatusCode)
	}

	// Carol removes the share from her feed; audio access goes with it.
	resp = do(t, "DELETE", ts.URL+"/me/feed/alice/2026-07-06-morning", carol.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove share: got %d, want 204", resp.StatusCode)
	}
	if xml := fetchFeed(t, ts, carol, ""); strings.Contains(xml, "Alice Morning") {
		t.Errorf("removed share still in carol's feed:\n%s", xml)
	}

	// The owner's delete propagates: Bob's feed loses the episode too.
	resp = do(t, "DELETE", ts.URL+"/me/episodes/2026-07-06-morning", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("owner delete: got %d, want 204", resp.StatusCode)
	}
	if xml := fetchFeed(t, ts, bob, ""); strings.Contains(xml, "Alice Morning") {
		t.Errorf("deleted episode still in bob's feed:\n%s", xml)
	}
}

func TestBlockAndMute(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")
	carol := createUser(t, ts, "carol")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning", `{"title":"Alice Morning"}`, "A")
	resp.Body.Close()
	resp = share(t, ts, alice, "alice", "2026-07-06-morning", "bob")
	resp.Body.Close()

	// Carol blocks Bob: Bob's shares are rejected at share time; Alice
	// can still reach her (block targets the Sharer, ADR 0006).
	resp = do(t, "PUT", ts.URL+"/me/blocks/bob", carol.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("block: got %d, want 204", resp.StatusCode)
	}
	resp = share(t, ts, bob, "alice", "2026-07-06-morning", "carol")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked share: got %d, want 403", resp.StatusCode)
	}
	resp = share(t, ts, alice, "alice", "2026-07-06-morning", "carol")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("share from unblocked owner: got %d, want 201", resp.StatusCode)
	}

	// Carol mutes Alice: the already-shared episode disappears from her
	// feed (mute targets the Owner, whoever forwarded it).
	resp = do(t, "PUT", ts.URL+"/me/mutes/alice", carol.publishCreds(), nil, "")
	resp.Body.Close()
	if xml := fetchFeed(t, ts, carol, ""); strings.Contains(xml, "Alice Morning") {
		t.Errorf("muted owner still in feed:\n%s", xml)
	}

	// Unmute: it comes back — mute hides, it does not remove.
	resp = do(t, "DELETE", ts.URL+"/me/mutes/alice", carol.publishCreds(), nil, "")
	resp.Body.Close()
	if xml := fetchFeed(t, ts, carol, ""); !strings.Contains(xml, "Alice Morning") {
		t.Errorf("unmuted owner missing from feed:\n%s", xml)
	}

	// Unblock Bob and his shares land again.
	resp = do(t, "DELETE", ts.URL+"/me/blocks/bob", carol.publishCreds(), nil, "")
	resp.Body.Close()
	resp = publishEpisode(t, ts, bob, "2026-07-06-noon", `{"title":"Bob Noon"}`, "B")
	resp.Body.Close()
	resp = share(t, ts, bob, "bob", "2026-07-06-noon", "carol")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("share after unblock: got %d, want 201", resp.StatusCode)
	}
}

// mintInvite mints an invite for a; payload is "" or "owner/slug".
func mintInvite(t *testing.T, ts *httptest.Server, a account, payload string) map[string]any {
	t.Helper()
	body := "{}"
	if payload != "" {
		owner, slug, _ := strings.Cut(payload, "/")
		body = `{"owner":"` + owner + `","slug":"` + slug + `"}`
	}
	resp := do(t, "POST", ts.URL+"/me/invites", a.publishCreds(), strings.NewReader(body), "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mint invite: %d %s", resp.StatusCode, b)
	}
	var inv map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		t.Fatal(err)
	}
	return inv
}

// redeem posts a username to the invite URL and returns the response.
func redeem(t *testing.T, url, username string) *http.Response {
	t.Helper()
	resp, err := http.PostForm(url, map[string][]string{"username": {username}})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestInviteRedemption(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	resp := publishEpisode(t, ts, alice, "2026-07-08-morning", `{"title":"Morning Update"}`, "AUDIO")
	resp.Body.Close()

	// Minting with a payload not in your feed fails.
	resp = do(t, "POST", ts.URL+"/me/invites", alice.publishCreds(),
		strings.NewReader(`{"owner":"alice","slug":"nope"}`), "application/json")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("mint with dead payload: got %d, want 404", resp.StatusCode)
	}

	inv := mintInvite(t, ts, alice, "alice/2026-07-08-morning")
	url := inv["url"].(string)
	if inv["status"] != "pending" || inv["inviter"] != "alice" {
		t.Fatalf("unexpected invite: %+v", inv)
	}

	// The redemption page is public and shows inviter + episode title;
	// a wrong token is a plain 404.
	resp = do(t, "GET", url, "", nil, "")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "alice") || !strings.Contains(string(page), "Morning Update") {
		t.Fatalf("invite page: %d\n%s", resp.StatusCode, page)
	}
	resp = do(t, "GET", ts.URL+"/invites/no-such-token", "", nil, "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("bad token: got %d, want 404", resp.StatusCode)
	}

	// Invalid and taken usernames re-render the form without burning
	// the invite.
	resp = redeem(t, url, "Bad Name!")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid username: got %d, want 400", resp.StatusCode)
	}
	resp = redeem(t, url, "alice")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("taken username: got %d, want 409", resp.StatusCode)
	}

	// Redemption creates the user, shows credentials once, and the
	// payload is already in the new feed as a share from the inviter.
	resp = redeem(t, url, "carol")
	welcome, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("redeem: %d\n%s", resp.StatusCode, welcome)
	}
	w := string(welcome)
	for _, want := range []string{"Welcome, carol", "carol:", "/users/carol/feed.xml", "Morning Update"} {
		if !strings.Contains(w, want) {
			t.Fatalf("welcome page missing %q:\n%s", want, w)
		}
	}
	// Extract the credentials from the page to prove they work.
	readCreds := w[strings.Index(w, "carol:") : strings.Index(w, "carol:")+len("carol:")+32]
	carol := account{ID: "carol", ReadCredentials: readCreds}
	xml := fetchFeed(t, ts, carol, "")
	if !strings.Contains(xml, "Morning Update") || !strings.Contains(xml, "<itunes:author>alice</itunes:author>") {
		t.Fatalf("carol's feed missing payload:\n%s", xml)
	}
	resp = do(t, "GET", ts.URL+"/users/alice/episodes/2026-07-08-morning.mp3", readCreds, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("carol downloads payload audio: got %d, want 200", resp.StatusCode)
	}

	// Single use: the spent invite is a 404 for both GET and POST.
	resp = do(t, "GET", url, "", nil, "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("spent invite page: got %d, want 404", resp.StatusCode)
	}
	resp = redeem(t, url, "dave")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("spent invite redeem: got %d, want 404", resp.StatusCode)
	}

	// The inviter sees who redeemed it.
	resp = do(t, "GET", ts.URL+"/me/invites", alice.publishCreds(), nil, "")
	var views []map[string]any
	json.NewDecoder(resp.Body).Decode(&views)
	resp.Body.Close()
	if len(views) != 1 || views[0]["status"] != "redeemed" || views[0]["redeemed_by"] != "carol" {
		t.Fatalf("invite list: %+v", views)
	}
}

func TestInviteRevocationAndExpiry(t *testing.T) {
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:      st,
		AdminToken: adminToken,
		Assets:     os.DirFS("../../cmd/server"),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")

	// Revocation: only the inviter, only while pending.
	inv := mintInvite(t, ts, alice, "")
	token := inv["token"].(string)
	resp := do(t, "DELETE", ts.URL+"/me/invites/"+token, bob.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("revoke someone else's invite: got %d, want 404", resp.StatusCode)
	}
	resp = do(t, "DELETE", ts.URL+"/me/invites/"+token, alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: got %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/invites/"+token, "", nil, "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("revoked invite page: got %d, want 404", resp.StatusCode)
	}

	// A redeemed invite cannot be revoked (the record stays).
	inv = mintInvite(t, ts, alice, "")
	resp = redeem(t, inv["url"].(string), "carol")
	resp.Body.Close()
	resp = do(t, "DELETE", ts.URL+"/me/invites/"+inv["token"].(string), alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("revoke redeemed invite: got %d, want 409", resp.StatusCode)
	}

	// Expiry: an expired token is indistinguishable from a missing one.
	expired := store.Invite{
		Token: "expired-token", InviterID: "alice",
		CreatedAt: time.Now().Add(-8 * 24 * time.Hour),
		ExpiresAt: time.Now().Add(-24 * time.Hour),
	}
	if err := st.AddInvite(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		resp := do(t, method, ts.URL+"/invites/expired-token", "", strings.NewReader("username=dave"), "application/x-www-form-urlencoded")
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("%s expired invite: got %d, want 404", method, resp.StatusCode)
		}
	}
	// It still shows up for the inviter, flagged.
	resp = do(t, "GET", ts.URL+"/me/invites", alice.publishCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"status":"expired"`) && !strings.Contains(string(body), `"status": "expired"`) {
		t.Fatalf("expired invite not flagged:\n%s", body)
	}
}

func TestDashboardAndUserSearch(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	createUser(t, ts, "bob")
	createUser(t, ts, "bonnie")
	resp := publishEpisode(t, ts, alice, "2026-07-08-morning", `{"title":"Morning Update"}`, "AUDIO")
	resp.Body.Close()

	// curl-style GET /me keeps returning JSON.
	resp = do(t, "GET", ts.URL+"/me", alice.publishCreds(), nil, "")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("GET /me default content type: %q", ct)
	}
	resp.Body.Close()

	// A browser (Accept: text/html) gets the Dashboard with the feed
	// URL, the episode, and the invite button.
	req, _ := http.NewRequest("GET", ts.URL+"/me", nil)
	user, pass, _ := strings.Cut(alice.publishCreds(), ":")
	req.SetBasicAuth(user, pass)
	req.Header.Set("Accept", "text/html")
	htmlResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(htmlResp.Body)
	htmlResp.Body.Close()
	page := string(body)
	if htmlResp.StatusCode != 200 || !strings.Contains(htmlResp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("dashboard: %d %q", htmlResp.StatusCode, htmlResp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"Morning Update", "/users/alice/feed.xml", "mk-invite", "share-to"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}

	// Member search: substring match, self excluded, auth required.
	resp = do(t, "GET", ts.URL+"/me/users?q=bo", alice.publishCreds(), nil, "")
	var hits []struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&hits)
	resp.Body.Close()
	if len(hits) != 2 || hits[0].ID != "bob" || hits[1].ID != "bonnie" {
		t.Fatalf("search bo: %+v", hits)
	}
	resp = do(t, "GET", ts.URL+"/me/users?q=alice", alice.publishCreds(), nil, "")
	json.NewDecoder(resp.Body).Decode(&hits)
	resp.Body.Close()
	if len(hits) != 0 {
		t.Fatalf("search must exclude self: %+v", hits)
	}
	resp = do(t, "GET", ts.URL+"/me/users?q=bo", "", nil, "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated search: got %d, want 401", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me/users?q=bo", alice.ReadCredentials, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("read-credential search: got %d, want 403", resp.StatusCode)
	}
}

func TestCredentialRotation(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	resp := publishEpisode(t, ts, alice, "2026-07-08-morning", `{"title":"Keeper"}`, "AUDIO")
	resp.Body.Close()

	resp = do(t, "POST", ts.URL+"/admin/users/alice/credentials", "bearer:"+adminToken, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %d", resp.StatusCode)
	}
	var rotated account
	json.NewDecoder(resp.Body).Decode(&rotated)
	resp.Body.Close()

	// Old credentials are dead, new ones work, content survived.
	resp = do(t, "GET", ts.URL+"/users/alice/feed.xml", alice.ReadCredentials, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("old read creds: got %d, want 401", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("old publish token: got %d, want 401", resp.StatusCode)
	}
	rotated.ID = "alice"
	if xml := fetchFeed(t, ts, rotated, ""); !strings.Contains(xml, "Keeper") {
		t.Fatalf("episodes lost on rotation:\n%s", xml)
	}
	// Rotating an unknown user is a 404.
	resp = do(t, "POST", ts.URL+"/admin/users/nobody/credentials", "bearer:"+adminToken, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("rotate unknown user: got %d, want 404", resp.StatusCode)
	}
}

// cbrMP3 builds a valid MPEG-1 Layer III stream: n CBR frames at
// 128 kbps / 44.1 kHz (417 bytes, 1152 samples ≈ 26.12 ms each).
func cbrMP3(n int) string {
	frame := make([]byte, 417)
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x00})
	return strings.Repeat(string(frame), n)
}

func TestPublishEstimatesDuration(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	// 383 frames ≈ 10.0 s. No duration_seconds → the server estimates.
	resp := publishEpisode(t, ts, alice, "2026-07-07-noon",
		`{"title":"Estimated"}`, cbrMP3(383))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("publish: %d %s", resp.StatusCode, b)
	}
	var ep struct {
		DurationSec int   `json:"duration_seconds"`
		AudioSize   int64 `json:"audio_size"`
	}
	json.NewDecoder(resp.Body).Decode(&ep)
	resp.Body.Close()
	if ep.DurationSec != 10 {
		t.Errorf("estimated duration: got %d, want 10", ep.DurationSec)
	}
	if ep.AudioSize != 383*417 {
		t.Errorf("audio must be stored whole after estimation: got %d, want %d", ep.AudioSize, 383*417)
	}

	// An explicit duration_seconds overrides the estimate.
	resp = publishEpisode(t, ts, alice, "2026-07-07-noon",
		`{"title":"Explicit","duration_seconds":42}`, cbrMP3(383))
	json.NewDecoder(resp.Body).Decode(&ep)
	resp.Body.Close()
	if ep.DurationSec != 42 {
		t.Errorf("explicit duration: got %d, want 42", ep.DurationSec)
	}

	// Unparseable audio publishes fine, just without a duration.
	resp = publishEpisode(t, ts, alice, "2026-07-07-evening",
		`{"title":"Garbage"}`, "FAKEMP3BYTES")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("garbage publish: %d", resp.StatusCode)
	}
	ep.DurationSec = 0 // the field is omitempty; a stale value must not pass
	json.NewDecoder(resp.Body).Decode(&ep)
	resp.Body.Close()
	if ep.DurationSec != 0 {
		t.Errorf("garbage duration: got %d, want 0", ep.DurationSec)
	}
}

func TestPublishValidation(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	// Invalid slug → 400.
	resp := publishEpisode(t, ts, alice, "Bad_Slug!", `{"title":"x"}`, "a")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid slug: got %d, want 400", resp.StatusCode)
	}

	// Missing title → 400.
	resp = publishEpisode(t, ts, alice, "2026-07-06-morning", `{"description":"no title"}`, "a")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing title: got %d, want 400", resp.StatusCode)
	}
}

func TestCoverRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	resp := do(t, "PUT", ts.URL+"/me/image", alice.publishCreds(), strings.NewReader("JPEGBYTES"), "image/jpeg")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set cover: %d", resp.StatusCode)
	}

	// The feed advertises the cover at its unguessable URL.
	coverPath := strings.TrimPrefix(alice.CoverURL, ts.URL)
	if xml := fetchFeed(t, ts, alice, ""); !strings.Contains(xml, coverPath) {
		t.Errorf("feed missing itunes:image %s:\n%s", coverPath, xml)
	}

	resp = do(t, "GET", alice.CoverURL, "", nil, "")
	img, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(img) != "JPEGBYTES" || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("get cover: %d %q %q", resp.StatusCode, img, resp.Header.Get("Content-Type"))
	}

	// Wrong content type rejected.
	resp = do(t, "PUT", ts.URL+"/me/image", alice.publishCreds(), strings.NewReader("GIF"), "image/gif")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("gif cover: got %d, want 415", resp.StatusCode)
	}
}

func TestUpdateMeKeepsCover(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	resp := do(t, "PUT", ts.URL+"/me/image", alice.publishCreds(), strings.NewReader("JPEG"), "image/jpeg")
	resp.Body.Close()

	// Re-PUT the feed settings; cover must survive.
	resp = do(t, "PUT", ts.URL+"/me", alice.publishCreds(),
		strings.NewReader(`{"title":"Alice v2"}`), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-put me: %d", resp.StatusCode)
	}
	var u struct {
		Title     string `json:"title"`
		CoverType string `json:"cover_type"`
	}
	json.NewDecoder(resp.Body).Decode(&u)
	resp.Body.Close()
	if u.Title != "Alice v2" || u.CoverType != "image/jpeg" {
		t.Fatalf("cover dropped on update: %+v", u)
	}
}
