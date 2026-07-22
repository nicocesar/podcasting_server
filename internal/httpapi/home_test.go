package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestHomeOffersDashboardWhenSignedIn covers the nudge on "/": a signed-in
// browser is offered its dashboard on a timer, an anonymous one sees the
// plain landing page. Deliberately not a 302 — "/" must stay reachable
// while logged in, so the redirect is client-side and cancellable.
func TestHomeOffersDashboardWhenSignedIn(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")

	// Anonymous: no timer, no dashboard nudge.
	resp := do(t, "GET", ts.URL+"/", "", nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous status = %d", resp.StatusCode)
	}
	if strings.Contains(string(body), `id="resume"`) {
		t.Errorf("anonymous landing page offers a redirect:\n%s", body)
	}

	// Signed in: the offer is present, still a 200 rather than a redirect.
	resp = do(t, "GET", ts.URL+"/", alice.sessionCreds(), nil, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed-in status = %d, want 200 (not a redirect)", resp.StatusCode)
	}
	for _, want := range []string{`id="resume"`, `data-seconds="5"`, `href="/me"`, "Stay here"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("signed-in landing page missing %q", want)
		}
	}
	// The feed's title is the user's, so a stale session cannot leak
	// someone else's — cheap sanity that the right record was read.
	if !strings.Contains(string(body), "Briefings for alice") {
		t.Errorf("landing page did not name the signed-in feed:\n%s", body)
	}
}
