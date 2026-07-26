package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestDashboardOffersAiring: with an awake strand in the canon, an own
// episode gets a picker and a button, defaulting to whatever the station
// chose for it.
func TestDashboardOffersAiring(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	putStrand(t, st, "stories", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Ep One"}`, "MP3!").Body.Close()

	// The station sorted it into stories at generation time.
	ep, err := st.GetEpisode(context.Background(), "alice", "ep1")
	if err != nil {
		t.Fatal(err)
	}
	ep.Strand = "stories"
	if err := st.UpdateEpisode(context.Background(), ep); err != nil {
		t.Fatal(err)
	}

	page := dashboardHTML(t, ts, alice.sessionCreds())
	if !strings.Contains(page, `action="/me/episodes/ep1/air"`) {
		t.Fatalf("no air control on the dashboard:\n%s", page)
	}
	if !strings.Contains(page, `<option value="stories" selected>`) {
		t.Errorf("the picker does not default to the station's choice:\n%s", page)
	}
	if strings.Contains(page, "On air in") {
		t.Error("a private episode is shown as on air")
	}
}

// TestDashboardShowsOnAirState: once aired, the row says so in words and
// offers the way back off, not another way on.
func TestDashboardShowsOnAirState(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	airedEpisode(t, ts, st, alice, "ep1", "music")

	page := dashboardHTML(t, ts, alice.sessionCreds())
	if !strings.Contains(page, "On air in") || !strings.Contains(page, `href="/strands/music"`) {
		t.Fatalf("the dashboard does not show the on-air state:\n%s", page)
	}
	if !strings.Contains(page, `action="/me/episodes/ep1/unair"`) {
		t.Error("no way to take it off the air")
	}
	if strings.Contains(page, `action="/me/episodes/ep1/air"`) {
		t.Error("an aired episode still offers the air control")
	}
}

// TestDashboardHidesAiringWithoutACanon: a station whose strands are all
// dormant offers nothing, rather than a picker with no options that
// would 422 on submit.
func TestDashboardHidesAiringWithoutACanon(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "asleep", false, false) // no cover art
	putStrand(t, st, "gone", true, true)     // retired
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Ep One"}`, "MP3!").Body.Close()

	page := dashboardHTML(t, ts, alice.sessionCreds())
	if strings.Contains(page, `action="/me/episodes/ep1/air"`) {
		t.Fatal("the dashboard offers airing with no awake strand to air into")
	}
	if strings.Contains(page, "asleep") || strings.Contains(page, "gone") {
		t.Error("a dormant or retired strand is offered as a choice")
	}
}

// TestDashboardNeverOffersToAirAShare: ADR 0006 lets anyone forward an
// Episode onward; ADR 0018 lets only its Owner air it. The control must
// not appear on a row that is not yours, or the button lies.
func TestDashboardNeverOffersToAirAShare(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bobby")
	publishEpisode(t, ts, alice, "gift", `{"title":"A Gift"}`, "MP3!").Body.Close()
	share(t, ts, alice, "alice", "gift", "bobby").Body.Close()

	page := dashboardHTML(t, ts, bob.sessionCreds())
	if !strings.Contains(page, "A Gift") {
		t.Fatal("the shared episode is not on bob's dashboard at all")
	}
	if strings.Contains(page, `/me/episodes/gift/air`) {
		t.Fatal("bob is offered the chance to air alice's episode")
	}
}

// TestDashboardShowsTheStationsBar: an admin takedown must be legible to
// the owner, not a button that silently stops working (ADR 0018).
func TestDashboardShowsTheStationsBar(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	admin := createAdmin(t, ts, "chief")
	alice := createUser(t, ts, "alice")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	postForm(t, ts, admin.sessionCreds(), "/admin/airings/"+id+"/unair", url.Values{}).Body.Close()

	page := dashboardHTML(t, ts, alice.sessionCreds())
	if !strings.Contains(page, "The station took this off the air") {
		t.Fatalf("the owner is not told their episode was taken down:\n%s", page)
	}
	if strings.Contains(page, `action="/me/episodes/ep1/air"`) {
		t.Error("a barred episode still offers the air control")
	}
}

// TestAirFromTheDashboardReachesTheStrand is the whole loop as a person
// walks it: press the button, and it is on the public page.
func TestAirFromTheDashboardReachesTheStrand(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Smooth Lounge Vibe"}`, "MP3!").Body.Close()

	resp := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air", url.Values{"strand": {"music"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("air: %d, want 303", resp.StatusCode)
	}

	_, strandPage := get(t, ts, "/strands/music")
	if !strings.Contains(strandPage, "Smooth Lounge Vibe") {
		t.Fatalf("the episode is not on the strand page:\n%s", strandPage)
	}
	if !strings.Contains(dashboardHTML(t, ts, alice.sessionCreds()), "On air in") {
		t.Error("the dashboard does not reflect the airing it just did")
	}
}
