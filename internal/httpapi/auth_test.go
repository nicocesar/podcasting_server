package httpapi

// Tests for the credential split (ADR 0010): API Key lifecycle, session
// mechanics, Credential Management boundaries, and the Google OIDC
// forks against a faked token endpoint.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

const testGoogleClientID = "test-client-id"

// fakeIDToken builds an unsigned-but-shaped id_token whose sub is the
// authorization code — the exchange trusts the TLS channel, not the
// signature, so tests mint identities by picking codes.
func fakeIDToken(sub string) string {
	claims, _ := json.Marshal(map[string]any{
		"sub":            sub,
		"email":          sub + "@example.com",
		"email_verified": true,
		"aud":            testGoogleClientID,
		"iss":            "https://accounts.google.com",
		"exp":            time.Now().Add(time.Hour).Unix(),
	})
	return "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
}

// newGoogleTestServer is newTestServer with Google sign-in on, its token
// endpoint faked.
func newGoogleTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.FormValue("code") == "" {
			http.Error(w, "bad token request", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": fakeIDToken(r.FormValue("code"))})
	}))
	t.Cleanup(fake.Close)

	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:              st,
		AdminToken:         adminToken,
		SessionSecret:      "test-session-secret",
		GoogleClientID:     testGoogleClientID,
		GoogleClientSecret: "test-client-secret",
		GoogleTokenURL:     fake.URL,
		Assets:             os.DirFS("../../cmd/server"),
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// finishGoogle plays the browser's return from the consent screen: it
// lifts state and nonce off the 303 that started the flow and hits the
// callback with sub as the authorization code (= the Google identity).
// extraCookies stand in for whatever else the browser holds (a session,
// when linking).
func finishGoogle(t *testing.T, ts *httptest.Server, start *http.Response, sub string, extraCookies ...*http.Cookie) *http.Response {
	t.Helper()
	if start.StatusCode != http.StatusSeeOther {
		t.Fatalf("google start: got %d, want 303", start.StatusCode)
	}
	loc, err := url.Parse(start.Header.Get("Location"))
	if err != nil || loc.Host != "accounts.google.com" {
		t.Fatalf("google start location: %q (%v)", start.Header.Get("Location"), err)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/auth/google/callback?code="+url.QueryEscape(sub)+
		"&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	for _, c := range start.Cookies() {
		if c.Name == oauthNonceCookie {
			req.AddCookie(c)
		}
	}
	for _, c := range extraCookies {
		req.AddCookie(c)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func sessionFrom(t *testing.T, resp *http.Response) string {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c.Value
		}
	}
	t.Fatal("no session cookie in response")
	return ""
}

func TestAPIKeyLifecycle(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")

	// A key cannot mint or revoke keys — Credential Management is
	// session-only.
	resp := do(t, "POST", ts.URL+"/me/api-keys", alice.publishCreds(),
		strings.NewReader(`{"name":"escalation"}`), "application/json")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mint with API key: got %d, want 403", resp.StatusCode)
	}

	// A second named key works alongside the first; the listing shows
	// names but never plaintext or hashes.
	second := mintKey(t, ts, alice.Session, "cron-box")
	resp = do(t, "GET", ts.URL+"/me/api-keys", alice.sessionCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var keys []struct {
		KeyID string `json:"key_id"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(body, &keys); err != nil || len(keys) != 2 {
		t.Fatalf("list keys: %v %s", err, body)
	}
	if strings.Contains(string(body), strings.Split(second, "_")[2]) || strings.Contains(string(body), "hash") {
		t.Fatalf("key listing leaks secrets: %s", body)
	}

	// Both keys publish; revoking one kills it and leaves the other.
	for _, key := range []string{alice.Key, second} {
		resp := do(t, "GET", ts.URL+"/me", "bearer:"+key, nil, "")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("key %q: got %d, want 200", key, resp.StatusCode)
		}
	}
	secondID := strings.Split(second, "_")[1]

	// Another user cannot revoke it.
	resp = do(t, "DELETE", ts.URL+"/me/api-keys/"+secondID, bob.sessionCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user revoke: got %d, want 404", resp.StatusCode)
	}

	resp = do(t, "DELETE", ts.URL+"/me/api-keys/"+secondID, alice.sessionCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: got %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me", "bearer:"+second, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("revoked key still works: got %d, want 401", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("surviving key broken by sibling revoke: %d", resp.StatusCode)
	}
}

func TestPasswordChange(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	otherSession := login(t, ts, "alice", alice.Password)

	// Changing needs the current password.
	resp := do(t, "POST", ts.URL+"/me/password", alice.sessionCreds(),
		strings.NewReader(`{"current":"wrong","new":"a-new-long-password"}`), "application/json")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong current password: got %d, want 403", resp.StatusCode)
	}

	// Too-short new passwords are refused.
	resp = do(t, "POST", ts.URL+"/me/password", alice.sessionCreds(),
		strings.NewReader(fmt.Sprintf(`{"current":%q,"new":"short"}`, alice.Password)), "application/json")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short new password: got %d, want 400", resp.StatusCode)
	}

	// A real change logs out every other session, keeps this one (the
	// response re-issues the cookie), and leaves API keys alone.
	resp = do(t, "POST", ts.URL+"/me/password", alice.sessionCreds(),
		strings.NewReader(fmt.Sprintf(`{"current":%q,"new":"a-new-long-password"}`, alice.Password)), "application/json")
	fresh := sessionFrom(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change password: got %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me", "session:"+otherSession, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("other session survived password change: %d", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me", "session:"+fresh, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("re-issued session broken: %d", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("API key broken by password change: %d", resp.StatusCode)
	}
	login(t, ts, "alice", "a-new-long-password")
}

func TestLogoutEverywhere(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	req, _ := http.NewRequest("POST", ts.URL+"/me/logout-everywhere", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: alice.Session})
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout everywhere: got %d, want 303", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me", alice.sessionCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("session survived logout-everywhere: %d", resp.StatusCode)
	}
	// The API key is not a session; it survives.
	resp = do(t, "GET", ts.URL+"/me", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("API key killed by logout-everywhere: %d", resp.StatusCode)
	}
}

func TestLoginFailures(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	resp := do(t, "GET", ts.URL+"/login", "", nil, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login page: %d", resp.StatusCode)
	}
	for name, creds := range map[string][2]string{
		"wrong password": {"alice", "not-the-password"},
		"unknown user":   {"nobody", alice.Password},
	} {
		resp, err := noRedirect.PostForm(ts.URL+"/login", url.Values{
			"username": {creds[0]}, "password": {creds[1]},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, resp.StatusCode)
		}
		if len(resp.Cookies()) != 0 {
			t.Errorf("%s: a failed login set cookies", name)
		}
	}
}

func TestCrossOriginWriteRejected(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	post := func(origin string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/me/invites", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: alice.Session})
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("https://evil.example"); got != http.StatusForbidden {
		t.Errorf("cross-origin session write: got %d, want 403", got)
	}
	if got := post(ts.URL); got != http.StatusCreated {
		t.Errorf("same-origin session write: got %d, want 201", got)
	}
}

func TestGoogleLinkLoginUnlink(t *testing.T) {
	ts := newGoogleTestServer(t)
	alice := createUser(t, ts, "alice")

	// An unlinked Google identity cannot log in — no invite, no entry.
	start, err := noRedirect.Get(ts.URL + "/auth/google")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	resp := finishGoogle(t, ts, start, "google-sub-alice")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "invite") {
		t.Fatalf("unlinked google login: %d\n%s", resp.StatusCode, body)
	}

	// Linking requires a session, then the same identity logs in.
	req, _ := http.NewRequest("GET", ts.URL+"/auth/google?link=1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: alice.Session})
	start, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	cb := finishGoogle(t, ts, start, "google-sub-alice",
		&http.Cookie{Name: sessionCookie, Value: alice.Session})
	cb.Body.Close()
	if cb.StatusCode != http.StatusSeeOther || cb.Header.Get("Location") != "/me" {
		t.Fatalf("link callback: %d %q", cb.StatusCode, cb.Header.Get("Location"))
	}

	start, err = noRedirect.Get(ts.URL + "/auth/google")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	cb = finishGoogle(t, ts, start, "google-sub-alice")
	googleSession := sessionFrom(t, cb)
	cb.Body.Close()
	if cb.StatusCode != http.StatusSeeOther {
		t.Fatalf("google login after link: %d", cb.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me", "session:"+googleSession, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("google session on /me: %d", resp.StatusCode)
	}

	// A second account cannot claim the same Google identity.
	bob := createUser(t, ts, "bob")
	req, _ = http.NewRequest("GET", ts.URL+"/auth/google?link=1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: bob.Session})
	start, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	cb = finishGoogle(t, ts, start, "google-sub-alice",
		&http.Cookie{Name: sessionCookie, Value: bob.Session})
	cb.Body.Close()
	if cb.StatusCode != http.StatusConflict {
		t.Fatalf("double link: got %d, want 409", cb.StatusCode)
	}

	// Unlink works while a password remains.
	resp = do(t, "POST", ts.URL+"/me/google/unlink", alice.sessionCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unlink: got %d, want 204", resp.StatusCode)
	}
}

func TestGoogleRedemption(t *testing.T) {
	ts := newGoogleTestServer(t)
	alice := createUser(t, ts, "alice")
	inv := mintInvite(t, ts, alice, "")
	inviteURL := inv["url"].(string)

	// The invite page offers the Google fork.
	resp := do(t, "GET", inviteURL, "", nil, "")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "Join with Google") {
		t.Fatalf("invite page missing the Google fork:\n%s", page)
	}

	// "Join with Google" bounces via the consent screen and lands on the
	// Welcome page, logged in, with the invite spent.
	start, err := noRedirect.PostForm(inviteURL, url.Values{
		"username": {"dave"}, "method": {"google"},
	})
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	cb := finishGoogle(t, ts, start, "google-sub-dave")
	welcome, _ := io.ReadAll(cb.Body)
	daveSession := sessionFrom(t, cb)
	cb.Body.Close()
	if cb.StatusCode != 200 || !strings.Contains(string(welcome), "Welcome, dave") {
		t.Fatalf("google redemption: %d\n%s", cb.StatusCode, welcome)
	}
	resp = do(t, "GET", ts.URL+"/me", "session:"+daveSession, nil, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("dave's session: %d", resp.StatusCode)
	}
	// Spent, but this invite carried no Episode, so there is nothing
	// left for it to do: the page renders without a join form (ADR 0014).
	resp = do(t, "GET", inviteURL, "", nil, "")
	spent, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("spent invite: got %d, want 200", resp.StatusCode)
	}
	if strings.Contains(string(spent), `name="username"`) {
		t.Errorf("spent invite still offers the join form:\n%s", spent)
	}

	// dave is Google-only: unlinking his one Login is refused.
	resp = do(t, "POST", ts.URL+"/me/google/unlink", "session:"+daveSession, nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "password") {
		t.Fatalf("unlink last login: %d %s", resp.StatusCode, body)
	}
}
