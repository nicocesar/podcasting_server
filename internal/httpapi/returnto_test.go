package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestAiringReturnsWhereTheFormAsked is the complaint this fixes: airing
// from the Dashboard used to navigate to the Episode Page, so putting
// three episodes on the air meant three round trips through a page
// nobody asked for, each ending with a scroll back down the log.
func TestAiringReturnsWhereTheFormAsked(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Smooth Lounge Vibe"}`, "MP3!").Body.Close()

	air := func(form url.Values) string {
		t.Helper()
		resp := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air", form)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("air: %d, want 303", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}

	got := air(url.Values{"strand": {"music"}, "return": {"/me?filter=mine#ep-ep1"}})
	if got != "/me?filter=mine#ep-ep1" {
		t.Errorf("air ignored the form's return address: got %q", got)
	}

	// Un-air honours it too, and the filter survives the round trip:
	// landing on an unfiltered log would lose the reader's place just as
	// surely as landing on another page.
	resp := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/unair",
		url.Values{"return": {"/me?filter=mine#ep-ep1"}})
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/me?filter=mine#ep-ep1" {
		t.Errorf("unair ignored the form's return address: got %q", got)
	}

	// No return field at all: the handler's own destination, unchanged.
	// Every form carries one, but a missing one must stay harmless.
	if got := air(url.Values{"strand": {"music"}}); got != "/me/episodes/alice/ep1" {
		t.Errorf("air without a return address: got %q, want the episode page", got)
	}
}

// TestReturnAddressStaysOnThisSite: `return` is attacker-controlled input
// on an authenticated POST, so it is constrained exactly as ?next= is on
// login — a path on this site, and nothing else.
func TestReturnAddressStaysOnThisSite(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Smooth Lounge Vibe"}`, "MP3!").Body.Close()

	for _, hostile := range []string{
		"https://evil.example/phish",
		"//evil.example/phish", // protocol-relative: leaves the site
		"http://evil.example",
		"evil.example",
		"/me\r\nX-Injected: yes", // a second header, not a path
		"",
	} {
		// Un-airing needs something on the air, and each pass consumes
		// it — otherwise the handler 404s before it ever redirects and
		// the assertion below would pass against no Location at all.
		postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air",
			url.Values{"strand": {"music"}}).Body.Close()

		resp := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/unair",
			url.Values{"return": {hostile}})
		resp.Body.Close()
		got := resp.Header.Get("Location")
		if got != "/me/episodes/alice/ep1" {
			t.Errorf("return %q: redirected to %q, want the safe fallback", hostile, got)
		}
		if resp.Header.Get("X-Injected") != "" {
			t.Errorf("return %q wrote a header of its own", hostile)
		}
	}
}

// TestTakedownReturnsToTheStrand: handleAdminUnair used to redirect to
// /admin/airings, a route that was never registered, so the one admin
// power over somebody else's Airing always ended on the 404 page. It
// lands on the Strand Page now — where ADR 0023 puts the control.
func TestTakedownReturnsToTheStrand(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	admin := createAdmin(t, ts, "chief")
	alice := createUser(t, ts, "alice")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	resp := postForm(t, ts, admin.sessionCreds(), "/admin/airings/"+id+"/unair", url.Values{})
	resp.Body.Close()
	got := resp.Header.Get("Location")
	if strings.Contains(got, "/admin/airings") {
		t.Fatalf("the takedown still redirects to /admin/airings, which does not exist: %q", got)
	}
	if got != "/strands/music" {
		t.Errorf("takedown landed on %q, want the strand page", got)
	}

	// And it is a page that exists — the whole point of the change.
	page, body := htmlPage(t, ts.URL+got, admin.sessionCreds())
	if page.StatusCode != http.StatusOK {
		t.Errorf("the takedown's destination answers %d:\n%s", page.StatusCode, body)
	}
}

// TestTakedownIsOnTheStrandPageForAdminsOnly: ADR 0023 puts the control
// on the Public Surface, so who sees it is the whole question. The page
// is readable by anyone; the button is not.
func TestTakedownIsOnTheStrandPageForAdminsOnly(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	admin := createAdmin(t, ts, "chief")
	alice := createUser(t, ts, "alice")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	page := ts.URL + "/strands/music"
	want := "/admin/airings/" + id + "/unair"

	_, boss := htmlPage(t, page, admin.sessionCreds())
	if !strings.Contains(boss, want) {
		t.Errorf("an admin reading the strand page is offered no takedown:\n%s", boss)
	}

	// The owner, a signed-in stranger, and nobody at all: no button.
	for name, creds := range map[string]string{
		"the episode's owner": alice.sessionCreds(),
		"anonymous":           "",
	} {
		_, body := htmlPage(t, page, creds)
		if !strings.Contains(body, "ep1") && !strings.Contains(body, "airing-"+id) {
			t.Fatalf("%s cannot read the strand page at all:\n%s", name, body)
		}
		if strings.Contains(body, want) {
			t.Errorf("%s is offered the takedown:\n%s", name, body)
		}
	}
}

// TestTakedownStaysOutOfThePublicCache: the strand page is Public
// Surface and is publicly cacheable for anonymous readers. An admin's
// rendering carries a button nobody else may use, so it must never be
// the copy a shared cache keeps.
func TestTakedownStaysOutOfThePublicCache(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	admin := createAdmin(t, ts, "chief")
	alice := createUser(t, ts, "alice")
	airedEpisode(t, ts, st, alice, "ep1", "music")

	resp, _ := htmlPage(t, ts.URL+"/strands/music", admin.sessionCreds())
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("an admin's strand page is cacheable: Cache-Control = %q", cc)
	}
	resp, _ = htmlPage(t, ts.URL+"/strands/music", "")
	if cc := resp.Header.Get("Cache-Control"); strings.Contains(cc, "no-store") {
		t.Errorf("the anonymous strand page lost its public cacheability: %q", cc)
	}
}
