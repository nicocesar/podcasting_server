package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// htmlPage fetches a URL as a browser would. The Accept header matters:
// several handlers answer JSON without it, and an assertion that some
// markup is absent passes trivially against a document that was never
// markup at all.
func htmlPage(t *testing.T, url, creds string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	if v, ok := strings.CutPrefix(creds, "session:"); ok {
		req.AddCookie(&http.Cookie{Name: "session", Value: v})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestChromeOnEveryMemberPage: the bar is what replaced the per-page
// back-links, so a member page that renders without it has no way back
// at all.
func TestChromeOnEveryMemberPage(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	for _, path := range []string{"/me", "/me/settings", "/strands"} {
		_, body := htmlPage(t, ts.URL+path, alice.sessionCreds())
		for _, want := range []string{`class="chrome"`, `<a href="/me">Feed</a>`, `href="/strands"`, `href="/me/settings"`} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s is missing chrome %q", path, want)
			}
		}
	}
}

// TestChromeHidesAdminFromMembers: the Admin link is guarded
// server-side, so showing it would leak no access — but it would offer a
// door that answers 404, which is worse than no door.
func TestChromeHidesAdminFromMembers(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	chief := createAdmin(t, ts, "chief")

	_, member := htmlPage(t, ts.URL+"/me", alice.sessionCreds())
	if strings.Contains(member, `href="/admin"`) {
		t.Errorf("a member's chrome offers the admin index:\n%s", member)
	}
	_, boss := htmlPage(t, ts.URL+"/me", chief.sessionCreds())
	if !strings.Contains(boss, `href="/admin"`) {
		t.Errorf("an admin's chrome has no way to the admin index:\n%s", boss)
	}
}

// TestChromeStaysPublicOnCapabilityURLs is the rule that matters most
// here. A /f/{token} address is the whole credential and works for
// whoever holds it, so the bar must stay public there even when the
// browser happens to be carrying a session — otherwise a forwarded URL
// renders one particular member's navigation to somebody else.
func TestChromeStaysPublicOnCapabilityURLs(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	resp := publishEpisode(t, ts, alice, "2026-07-06-morning",
		`{"title":"Morning Update","duration_seconds":402}`, "FAKEMP3BYTES")
	resp.Body.Close()

	page := feedBase(alice) + "/alice/2026-07-06-morning"

	// Anonymous: the public bar, and nothing that assumes an account.
	_, anon := htmlPage(t, page, "")
	if strings.Contains(anon, `href="/me"`) {
		t.Errorf("a capability URL offers member navigation to an anonymous reader:\n%s", anon)
	}

	// Signed in as the feed's own owner, on their own capability URL:
	// still the public bar. The page is defined by the address, not by
	// who is looking at it.
	_, owner := htmlPage(t, page, alice.sessionCreds())
	if strings.Contains(owner, `href="/me"`) {
		t.Errorf("a capability URL rendered member navigation to a signed-in reader:\n%s", owner)
	}
	if !strings.Contains(owner, `class="chrome"`) {
		t.Errorf("a capability URL rendered no chrome at all:\n%s", owner)
	}
}

// TestAdminIndex: the door the chrome's Admin link opens, and a 404 for
// everyone else — the same shape adminUser gives every admin surface.
func TestAdminIndex(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	chief := createAdmin(t, ts, "chief")

	resp, body := htmlPage(t, ts.URL+"/admin", chief.sessionCreds())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin as an admin: %d\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"/admin/strands", "/admin/costs"} {
		if !strings.Contains(body, want) {
			t.Errorf("the admin index is missing %q", want)
		}
	}
	// Moderation is deliberately not here (ADR 0023): the takedown lives
	// on the Strand Page. A link to an index that was never built is
	// exactly the 404 this page exists to stop producing.
	if strings.Contains(body, "/admin/airings") {
		t.Errorf("the admin index links to /admin/airings, which does not exist:\n%s", body)
	}

	resp, _ = htmlPage(t, ts.URL+"/admin", alice.sessionCreds())
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /admin as a member: got %d, want 404", resp.StatusCode)
	}
}
