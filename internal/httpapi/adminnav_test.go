package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dashboardHTML fetches /me as a signed-in browser. The Accept header
// is the whole point: without it handleGetMe answers JSON, and a test
// asserting that some markup is absent would pass against a document
// that never contained markup at all.
func dashboardHTML(t *testing.T, ts *httptest.Server, creds string) string {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+"/me", nil)
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /me: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, "<html") {
		t.Fatalf("GET /me did not return the dashboard HTML:\n%s", body)
	}
	return body
}

// TestDashboardShowsAdminEntryToAdmins: an admin had no way into
// /admin/strands except by typing the URL, which meant the canon — the
// thing that decides what the whole public side can be about — was
// effectively hidden from the person who maintains it.
//
// The sidebar card that used to carry those links is gone: the chrome
// carries them on every page now, and a second copy on one page is the
// duplication that made the app feel bolted together. What matters is
// unchanged — an admin has a way in that is not the address bar.
func TestDashboardShowsAdminEntryToAdmins(t *testing.T) {
	ts := newTestServer(t)
	admin := createAdmin(t, ts, "chief")

	page := dashboardHTML(t, ts, admin.sessionCreds())
	if !strings.Contains(page, `href="/admin"`) {
		t.Errorf("an admin's dashboard offers no way into the admin surfaces:\n%s", page)
	}
}

// TestDashboardHidesAdminEntryFromUsers: the links are guarded
// server-side, so showing them would not leak access — but it would
// offer a door that answers 404, which is worse than no door.
func TestDashboardHidesAdminEntryFromUsers(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	page := dashboardHTML(t, ts, alice.sessionCreds())
	for _, unwanted := range []string{`href="/admin"`, "/admin/strands", "/admin/costs"} {
		if strings.Contains(page, unwanted) {
			t.Errorf("an ordinary user's dashboard offers %q", unwanted)
		}
	}

	// And the guard behind it still holds, card or no card.
	resp := do(t, "GET", ts.URL+"/admin/strands", alice.sessionCreds(), nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /admin/strands as a user: %d, want 404", resp.StatusCode)
	}
}
