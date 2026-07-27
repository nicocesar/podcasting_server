package httpapi

import (
	"net/http"
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
