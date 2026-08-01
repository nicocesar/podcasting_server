package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestEpisodePageOffersTheAirControl: the Episode Page used to be a
// download link and a way back, so the only place to put anything on the
// air was the Dashboard. It renders the same fragment now.
func TestEpisodePageOffersTheAirControl(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Smooth Lounge Vibe"}`, "MP3!").Body.Close()

	page := ts.URL + "/me/episodes/alice/ep1"
	resp, body := htmlPage(t, page, alice.sessionCreds())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("episode page: %d\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `action="/me/episodes/ep1/air"`) {
		t.Errorf("the owner's episode page offers no way to air it:\n%s", body)
	}
	// And it comes back here rather than bouncing to the Dashboard.
	if !strings.Contains(body, `value="/me/episodes/alice/ep1"`) {
		t.Errorf("the episode page's air form does not return to the episode page:\n%s", body)
	}

	// Airing from here lands back here.
	air := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air",
		url.Values{"strand": {"music"}, "return": {"/me/episodes/alice/ep1"}})
	air.Body.Close()
	if got := air.Header.Get("Location"); got != "/me/episodes/alice/ep1" {
		t.Errorf("airing from the episode page landed on %q", got)
	}

	// Now on the air, the same page offers the way back off it.
	_, body = htmlPage(t, page, alice.sessionCreds())
	if !strings.Contains(body, `action="/me/episodes/ep1/unair"`) {
		t.Errorf("an aired episode's page offers no way to take it off:\n%s", body)
	}
}

// TestCapabilityEpisodePageNeverAirs: /f/{token} is a place to listen,
// and whoever holds the link may not be the Owner at all. The control
// must not appear there even for the Owner's own browser — the address
// decides, not the reader.
func TestCapabilityEpisodePageNeverAirs(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Smooth Lounge Vibe"}`, "MP3!").Body.Close()

	page := feedBase(alice) + "/alice/ep1"
	for _, creds := range []string{"", alice.sessionCreds()} {
		_, body := htmlPage(t, page, creds)
		if !strings.Contains(body, "Smooth Lounge Vibe") {
			t.Fatalf("the capability episode page did not render at all:\n%s", body)
		}
		if strings.Contains(body, "/air") {
			t.Errorf("a capability URL offers the air control (creds %q):\n%s", creds, body)
		}
	}
}

// TestSharedEpisodePageNeverAirs: ADR 0006 lets anyone forward an
// Episode onward, ADR 0018 lets only its Owner air it. The Episode Page
// has to obey that as strictly as the Dashboard row does.
func TestSharedEpisodePageNeverAirs(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bobby")
	publishEpisode(t, ts, alice, "gift", `{"title":"A Gift"}`, "MP3!").Body.Close()
	share(t, ts, alice, "alice", "gift", "bobby").Body.Close()

	_, body := htmlPage(t, ts.URL+"/me/episodes/alice/gift", bob.sessionCreds())
	if !strings.Contains(body, "A Gift") {
		t.Fatalf("bob cannot read an episode shared with him:\n%s", body)
	}
	if strings.Contains(body, "/air") {
		t.Errorf("bob is offered the chance to air alice's episode:\n%s", body)
	}
}

// TestAirControlIsOneFragment guards the reason it was extracted: two
// copies of this markup would drift, and the copy that drifts is the one
// that stops matching what the server enforces.
func TestAirControlIsOneFragment(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Smooth Lounge Vibe"}`, "MP3!").Body.Close()

	dash := dashboardHTML(t, ts, alice.sessionCreds())
	_, page := htmlPage(t, ts.URL+"/me/episodes/alice/ep1", alice.sessionCreds())

	// Same control, same wording, same strand choices on both surfaces.
	for _, want := range []string{
		`action="/me/episodes/ep1/air"`,
		`Put on the air`,
		`<option value="music"`,
	} {
		if !strings.Contains(dash, want) {
			t.Errorf("the dashboard's air row is missing %q", want)
		}
		if !strings.Contains(page, want) {
			t.Errorf("the episode page's air row is missing %q", want)
		}
	}
}

// postAirHTMX presses the control the way the browser does once htmx is
// loaded: the same form, the same body, plus the header htmx sets on
// everything it sends.
func postAirHTMX(t *testing.T, ts *httptest.Server, creds, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := strings.CutPrefix(creds, "session:"); ok {
		req.AddCookie(&http.Cookie{Name: "session", Value: v})
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

// TestAiringOverHTMXSwapsTheControl: pressing the button used to reload
// the whole page to change one line of it. An htmx press gets that line
// back on its own — no layout, no redirect — and it is the same fragment
// the two pages render, so the swapped-in control cannot disagree with
// the one it replaced.
func TestAiringOverHTMXSwapsTheControl(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Smooth Lounge Vibe"}`, "MP3!").Body.Close()

	resp, body := postAirHTMX(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air",
		url.Values{"strand": {"music"}, "return": {"/me/episodes/alice/ep1"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an htmx press was answered %d, so htmx swaps nothing:\n%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Location") != "" {
		t.Errorf("an htmx press still redirects: %q", resp.Header.Get("Location"))
	}
	// The control itself, and only the control: a layout here would swap
	// a whole page into the middle of the one the reader is looking at.
	if strings.Contains(body, "<!doctype html") || strings.Contains(body, `class="chrome"`) {
		t.Errorf("the air fragment came back wrapped in the layout:\n%s", body)
	}
	if !strings.Contains(body, `class="air-row"`) {
		t.Errorf("the response is not the air row:\n%s", body)
	}
	// And it comes back in the state the press put it in: on the air,
	// offering the way back off.
	if !strings.Contains(body, `action="/me/episodes/ep1/unair"`) {
		t.Errorf("the swapped-in control does not know the episode is now aired:\n%s", body)
	}
	if !strings.Contains(body, "On air in") {
		t.Errorf("the swapped-in control does not say it is on the air:\n%s", body)
	}
	// The row that replaced this one still knows where a JavaScript-less
	// press would land, so the fragment stays usable if htmx goes away.
	if !strings.Contains(body, `value="/me/episodes/alice/ep1"`) {
		t.Errorf("the swapped-in control lost its return address:\n%s", body)
	}

	// Taking it off the air swaps the same way, back to the picker.
	resp, body = postAirHTMX(t, ts, alice.sessionCreds(), "/me/episodes/ep1/unair",
		url.Values{"return": {"/me/episodes/alice/ep1"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an htmx un-air was answered %d:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `action="/me/episodes/ep1/air"`) {
		t.Errorf("un-airing over htmx did not give the picker back:\n%s", body)
	}
}

// TestAiringWithoutHTMXStillRedirects: htmx is an enhancement, not the
// contract. A press from a browser that never loaded it — or from curl —
// gets the 303 it always got, to the address the form carried (ADR 0022).
func TestAiringWithoutHTMXStillRedirects(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Smooth Lounge Vibe"}`, "MP3!").Body.Close()

	resp := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air",
		url.Values{"strand": {"music"}, "return": {"/me#ep-ep1"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a plain press was answered %d, not a redirect", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/me#ep-ep1" {
		t.Errorf("a plain press landed on %q", got)
	}
}

// TestRefusedHTMXAiringSaysWhy: a refusal used to be a whole error page,
// which is a fine thing to navigate to and a useless thing to swap. Over
// htmx the reader stays where they are, so the reason has to arrive
// inside the control — and the control has to still be usable.
func TestRefusedHTMXAiringSaysWhy(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Smooth Lounge Vibe"}`, "MP3!").Body.Close()

	resp, body := postAirHTMX(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air",
		url.Values{"strand": {"no-such-strand"}, "return": {"/me/episodes/alice/ep1"}})
	// 200, or htmx drops the response on the floor and the press looks
	// like it did nothing at all.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a refused htmx press was answered %d, so nothing is swapped in:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "no such strand") {
		t.Errorf("the refusal does not say why:\n%s", body)
	}
	// Still the picker: the episode was not aired, and the reader can try
	// again without reloading.
	if !strings.Contains(body, `action="/me/episodes/ep1/air"`) {
		t.Errorf("a refusal left the reader without a control:\n%s", body)
	}
	if strings.Contains(body, "On air in") {
		t.Errorf("a refused press claims the episode is on the air:\n%s", body)
	}
}
