package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dashboard fetches /me as a logged-in browser would.
func dashboard(t *testing.T, ts *httptest.Server, a account, query string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/me"+query, nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: a.Session})
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /me%s: %d", query, resp.StatusCode)
	}
	return string(body)
}

// The Dashboard shows the whole Personal Feed, not only what this user
// published, and says of each shared Episode who made it and who passed
// it along.
func TestDashboardShowsSharedEpisodes(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")
	carol := createUser(t, ts, "carol")

	publishEpisode(t, ts, alice, "own-briefing", `{"title":"Morning Update"}`, "AUDIO").Body.Close()
	publishEpisode(t, ts, bobby, "fox-kettle", `{"title":"The Fox and the Kettle"}`, "AUDIO").Body.Close()

	// bobby shares to carol, who forwards to alice: the Owner and the
	// Sharer differ, and the credit line must name both.
	share(t, ts, bobby, "bobby", "fox-kettle", "carol").Body.Close()
	share(t, ts, carol, "bobby", "fox-kettle", "alice").Body.Close()

	page := dashboard(t, ts, alice, "")

	if !strings.Contains(page, "The Fox and the Kettle") {
		t.Error("dashboard omits an episode shared into the feed")
	}
	if !strings.Contains(page, "Morning Update") {
		t.Error("dashboard lost the user's own episode")
	}
	// "by @bobby · via @carol": creator first, then whoever handed it on.
	if !strings.Contains(page, "@bobby") || !strings.Contains(page, "@carol") {
		t.Errorf("credit line missing owner or sharer:\n%s", page)
	}
	if !strings.Contains(page, "episode shared") {
		t.Error("shared row is not marked for the tinted treatment")
	}
	if !strings.Contains(page, "2 shared") && !strings.Contains(page, "1 shared") {
		t.Errorf("header does not count the shared episodes:\n%s", page)
	}
}

// A creator sharing their own episode is credited once. "by @bobby ·
// via @bobby" is true but reads as a bug.
func TestDashboardCollapsesSelfShareCredit(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")

	publishEpisode(t, ts, bobby, "fox-kettle", `{"title":"The Fox and the Kettle"}`, "AUDIO").Body.Close()
	share(t, ts, bobby, "bobby", "fox-kettle", "alice").Body.Close()

	page := dashboard(t, ts, alice, "")
	if !strings.Contains(page, "@bobby") {
		t.Fatal("credit line missing the owner")
	}
	if strings.Contains(page, "via <strong>@bobby</strong>") {
		t.Errorf("owner credited twice on a direct share:\n%s", page)
	}
}

// A shared Episode offers forwarding and removal, and nothing that
// belongs to its Owner: no delete, no revoke, no character backfill.
func TestDashboardSharedRowActions(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")

	publishEpisode(t, ts, bobby, "fox-kettle", `{"title":"The Fox and the Kettle"}`, "AUDIO").Body.Close()
	share(t, ts, bobby, "bobby", "fox-kettle", "alice").Body.Close()

	// Assert on rendered markup, never on bare data- attribute names:
	// the page's own script mentions every one of them, so those
	// substrings are present whether or not a button was drawn.
	page := dashboard(t, ts, alice, "?filter=shared")
	if !strings.Contains(page, "Remove from my feed") {
		t.Error("shared row offers no way out of the feed")
	}
	if !strings.Contains(page, `<input class="share-to"`) ||
		!strings.Contains(page, "A link that plays this episode") {
		t.Error("shared row cannot be forwarded, which ADR 0006 permits")
	}
	if strings.Contains(page, "/characters") {
		t.Error("shared row offers the owner-only character backfill")
	}

	// The control belongs to shared rows alone: a feed of nothing but
	// your own episodes never offers to remove one.
	if own := dashboard(t, ts, bobby, ""); strings.Contains(own, "Remove from my feed") {
		t.Error("own episode offers Remove from my feed")
	}

	// And the endpoint the button drives actually removes it.
	resp := do(t, "DELETE", ts.URL+"/me/feed/bobby/fox-kettle", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unshare: got %d, want 204", resp.StatusCode)
	}
	if page := dashboard(t, ts, alice, ""); strings.Contains(page, "The Fox and the Kettle") {
		t.Error("removed share still on the dashboard")
	}
}

// An Episode shared today ranks by when it arrived, not when it aired.
// Ranking a shared back-catalogue episode by its air date would bury it
// where its recipient would never see it turn up.
func TestDashboardRanksSharesByArrival(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")

	// alice's own episode aired yesterday; bobby's aired long ago but
	// reaches her now.
	publishEpisode(t, ts, alice, "own-briefing",
		`{"title":"Morning Update","published_at":"2026-07-23T09:00:00Z"}`, "AUDIO").Body.Close()
	publishEpisode(t, ts, bobby, "fox-kettle",
		`{"title":"The Fox and the Kettle","published_at":"2020-04-12T09:00:00Z"}`, "AUDIO").Body.Close()
	share(t, ts, bobby, "bobby", "fox-kettle", "alice").Body.Close()

	page := dashboard(t, ts, alice, "")
	shared := strings.Index(page, "The Fox and the Kettle")
	own := strings.Index(page, "Morning Update")
	if shared < 0 || own < 0 {
		t.Fatalf("dashboard missing an episode (shared=%d own=%d)", shared, own)
	}
	if shared > own {
		t.Error("a 2020 episode shared just now sorted below yesterday's own episode")
	}
}

// The Feed Variants, as the log's own filter. The counts follow what is
// actually shown.
func TestDashboardFilter(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")

	publishEpisode(t, ts, alice, "own-briefing", `{"title":"Morning Update"}`, "AUDIO").Body.Close()
	publishEpisode(t, ts, bobby, "fox-kettle", `{"title":"The Fox and the Kettle"}`, "AUDIO").Body.Close()
	share(t, ts, bobby, "bobby", "fox-kettle", "alice").Body.Close()

	mine := dashboard(t, ts, alice, "?filter=mine")
	if !strings.Contains(mine, "Morning Update") || strings.Contains(mine, "The Fox and the Kettle") {
		t.Error("?filter=mine did not restrict the log to own episodes")
	}

	shared := dashboard(t, ts, alice, "?filter=shared")
	if !strings.Contains(shared, "The Fox and the Kettle") || strings.Contains(shared, "Morning Update") {
		t.Error("?filter=shared did not restrict the log to shared episodes")
	}
	if !strings.Contains(shared, "all shared") {
		t.Errorf("an all-shared log should say so:\n%s", shared)
	}

	// A filter the API does not define falls back to everything rather
	// than showing an empty log.
	all := dashboard(t, ts, alice, "?filter=nonsense")
	if !strings.Contains(all, "Morning Update") || !strings.Contains(all, "The Fox and the Kettle") {
		t.Error("an unknown filter should behave as no filter")
	}
}

// Regression: live Invite links are grouped by owner/slug, never by the
// bare slug. A shared Episode that happens to share a slug with one of
// your own must not inherit your links — or the Revoke button beside
// them, which would kill a link to a different episode entirely.
func TestDashboardLinksDoNotCrossSlugCollision(t *testing.T) {
	ts := newTestServer(t)
	alice := createUser(t, ts, "alice")
	bobby := createUser(t, ts, "bobby")

	// The same slug in two different namespaces.
	publishEpisode(t, ts, alice, "news", `{"title":"Alice Nightly News"}`, "AUDIO").Body.Close()
	publishEpisode(t, ts, bobby, "news", `{"title":"Bobby Nightly News"}`, "AUDIO").Body.Close()
	share(t, ts, bobby, "bobby", "news", "alice").Body.Close()

	// A live link to alice's own "news".
	resp := do(t, "POST", ts.URL+"/me/invites", alice.publishCreds(),
		strings.NewReader(`{"owner":"alice","slug":"news"}`), "application/json")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint invite: %d %s", resp.StatusCode, body)
	}

	// Count the rendered link rows, not the string "data-revoke", which
	// the page's own script also mentions.
	page := dashboard(t, ts, alice, "")
	if n := strings.Count(page, "<li data-token="); n != 1 {
		t.Errorf("expected exactly one revocable link row, got %d", n)
	}
	// And it must sit inside alice's own card, not the shared one. Cards
	// are siblings, so splitting on the card open tag isolates each.
	for card := range strings.SplitSeq(page, `<div class="episode`) {
		if strings.Contains(card, "Bobby Nightly News") && strings.Contains(card, "data-token=") {
			t.Error("invite link rendered on the shared episode with the colliding slug")
		}
		if strings.Contains(card, "Alice Nightly News") && !strings.Contains(card, "data-token=") {
			t.Error("invite link lost from the episode it actually belongs to")
		}
	}
}
