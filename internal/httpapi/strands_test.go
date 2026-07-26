package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// newStrandServer is a test server whose store the test can reach, so a
// canon can be seeded without an admin UI that does not exist yet.
func newStrandServer(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:         st,
		AdminToken:    adminToken,
		SessionSecret: "test-session-secret",
		Assets:        os.DirFS("../../cmd/server"),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, st
}

// putStrand seeds one canon entry. cover decides whether it is awake:
// a Strand without art is Dormant and takes no Airings.
func putStrand(t *testing.T, st store.Store, id string, cover bool, retired bool) {
	t.Helper()
	s := store.Strand{ID: id, Title: strings.ToUpper(id[:1]) + id[1:], Retired: retired}
	if cover {
		s.CoverType = "image/jpeg"
	}
	if err := st.PutStrand(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// airedEpisode publishes an episode, gives it a strand, and airs it,
// returning the public Airing id.
func airedEpisode(t *testing.T, ts *httptest.Server, st store.Store, a account, slug, strand string) string {
	t.Helper()
	resp := publishEpisode(t, ts, a, slug, `{"title":"`+slug+`"}`, "MP3!")
	resp.Body.Close()
	// 200 is a republish of the same slug (ADR 0002), which is a normal
	// way to arrive here a second time.
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusNoContent:
	default:
		t.Fatalf("publish %s: %d", slug, resp.StatusCode)
	}
	air := postForm(t, ts, a.sessionCreds(), "/me/episodes/"+slug+"/air", url.Values{"strand": {strand}})
	air.Body.Close()
	if air.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(air.Body)
		t.Fatalf("air %s: %d %s", slug, air.StatusCode, b)
	}
	airing, err := st.GetAiringByEpisode(context.Background(), a.ID, slug)
	if err != nil {
		t.Fatal(err)
	}
	return airing.ID
}

// postForm and deleteNoRedirect speak to the handlers without following
// the 303 they answer with — the shared do() uses the default client,
// which would chase the redirect and report the status of the page at
// the other end.
func postForm(t *testing.T, ts *httptest.Server, creds, path string, form url.Values) *http.Response {
	t.Helper()
	return sendNoRedirect(t, "POST", ts.URL+path, creds,
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
}

func deleteNoRedirect(t *testing.T, ts *httptest.Server, creds, path string) *http.Response {
	t.Helper()
	return sendNoRedirect(t, "DELETE", ts.URL+path, creds, nil, "")
}

func sendNoRedirect(t *testing.T, method, url, creds string, body io.Reader, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token, ok := strings.CutPrefix(creds, "bearer:"); ok {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if v, ok := strings.CutPrefix(creds, "session:"); ok {
		req.AddCookie(&http.Cookie{Name: "session", Value: v})
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestAirPutsAnEpisodeOnAStrand is the happy path: the Owner airs, an
// Airing record appears carrying the public id, and the Owner's chosen
// strand sticks to the Episode.
func TestAirPutsAnEpisodeOnAStrand(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")

	id := airedEpisode(t, ts, st, alice, "chillout-one", "music")
	if id == "" {
		t.Fatal("airing has no public id")
	}

	airing, err := st.GetAiring(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if airing.OwnerID != "alice" || airing.Slug != "chillout-one" || airing.Strand != "music" {
		t.Fatalf("airing = %+v", airing)
	}
	if airing.Settled || airing.VouchesAtSettle != 0 {
		t.Errorf("a fresh airing must be unsettled: %+v", airing)
	}
	ep, err := st.GetEpisode(context.Background(), "alice", "chillout-one")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Strand != "music" {
		t.Errorf("the owner's choice did not stick: episode strand = %q", ep.Strand)
	}

	// Listed on its strand, and nowhere else.
	list, err := st.ListAirings(context.Background(), "music")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAirings(music) = %+v, %v; want one", list, err)
	}
}

// TestAirRefusesDormantAndMissingStrands: a Strand without cover art
// renders a broken podcast feed, and a Retired one takes no new
// Airings. Neither may be aired into.
func TestAirRefusesDormantAndMissingStrands(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "dormant", false, false)
	putStrand(t, st, "retired", true, true)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Ep"}`, "MP3!").Body.Close()

	for _, tc := range []struct {
		name   string
		strand string
		want   int
	}{
		{"no cover art", "dormant", http.StatusUnprocessableEntity},
		{"retired", "retired", http.StatusUnprocessableEntity},
		{"does not exist", "nope", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air", url.Values{"strand": {tc.strand}})
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("air into %q: got %d, want %d", tc.strand, resp.StatusCode, tc.want)
			}
			if _, err := st.GetAiringByEpisode(context.Background(), "alice", "ep1"); err == nil {
				t.Fatal("the episode went on the air anyway")
			}
		})
	}
}

// TestAirNeedsAStrand: an Episode nothing in the canon fits cannot go
// public, because there would be nowhere for it to appear (ADR 0018).
func TestAirNeedsAStrand(t *testing.T) {
	ts, _ := newStrandServer(t)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Ep"}`, "MP3!").Body.Close()

	resp := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("air with no strand: got %d, want 422", resp.StatusCode)
	}
}

// TestAirIsSessionOnly: the Publishing Contract publishes into a private
// feed. A leaked API Key must not be able to put a User's audio in front
// of strangers (ADR 0018).
func TestAirIsSessionOnly(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Ep"}`, "MP3!").Body.Close()

	resp := postForm(t, ts, alice.publishCreds(), "/me/episodes/ep1/air", url.Values{"strand": {"music"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("air with an API key: got %d, want 403", resp.StatusCode)
	}
	if _, err := st.GetAiringByEpisode(context.Background(), "alice", "ep1"); err == nil {
		t.Fatal("an API key put an episode on the air")
	}
}

// TestOnlyTheOwnerAirs: ADR 0006 lets anyone forward an Episode onward,
// but ADR 0018 lets only its Owner air it. Bob has alice's episode in
// his feed and still cannot make it public.
func TestOnlyTheOwnerAirs(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bobby")
	publishEpisode(t, ts, alice, "ep1", `{"title":"Ep"}`, "MP3!").Body.Close()
	share(t, ts, alice, "alice", "ep1", "bobby").Body.Close()

	resp := postForm(t, ts, bob.sessionCreds(), "/me/episodes/ep1/air", url.Values{"strand": {"music"}})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("a sharer aired someone else's episode")
	}
	if _, err := st.GetAiringByEpisode(context.Background(), "alice", "ep1"); err == nil {
		t.Fatal("alice's episode went on the air without her")
	}
}

// TestReAiringToAnotherStrandIsRefused: re-airing mints a new public id
// and kills the old links, so moving strands has to be deliberate.
func TestReAiringToAnotherStrandIsRefused(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	putStrand(t, st, "stories", true, false)
	alice := createUser(t, ts, "alice")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	resp := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air", url.Values{"strand": {"stories"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-air onto another strand: got %d, want 409", resp.StatusCode)
	}
	airing, err := st.GetAiring(context.Background(), id)
	if err != nil || airing.Strand != "music" {
		t.Fatalf("the original airing moved or vanished: %+v, %v", airing, err)
	}

	// Airing again onto the same strand is a no-op, not an error, and
	// keeps the id the listeners already have.
	same := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air", url.Values{"strand": {"music"}})
	defer same.Body.Close()
	if same.StatusCode != http.StatusSeeOther {
		t.Fatalf("re-air onto the same strand: got %d, want 303", same.StatusCode)
	}
	if again, _ := st.GetAiringByEpisode(context.Background(), "alice", "ep1"); again.ID != id {
		t.Errorf("the public id changed on a no-op re-air: %q → %q", id, again.ID)
	}
}

// TestUnAirThenReAirMintsANewID: links killed by an un-air stay dead
// (ADR 0018).
func TestUnAirThenReAirMintsANewID(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	first := airedEpisode(t, ts, st, alice, "ep1", "music")

	off := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/unair", url.Values{})
	off.Body.Close()
	if off.StatusCode != http.StatusSeeOther {
		t.Fatalf("unair: got %d, want 303", off.StatusCode)
	}
	if _, err := st.GetAiring(context.Background(), first); err == nil {
		t.Fatal("the airing survived the un-air")
	}

	second := airedEpisode(t, ts, st, alice, "ep1", "music")
	if second == first {
		t.Fatal("re-airing reused the dead public id")
	}
}

// TestAdminUnAirBarsReAiring: without the bar a takedown is decorative —
// the Owner just airs it again (ADR 0018). The Episode itself must
// survive: what had to stop was the publicness.
func TestAdminUnAirBarsReAiring(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	admin := createAdmin(t, ts, "chief")
	alice := createUser(t, ts, "alice")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	resp := postForm(t, ts, admin.sessionCreds(), "/admin/airings/"+id+"/unair", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin unair: got %d, want 303", resp.StatusCode)
	}
	if _, err := st.GetAiring(context.Background(), id); err == nil {
		t.Fatal("the airing survived the takedown")
	}
	ep, err := st.GetEpisode(context.Background(), "alice", "ep1")
	if err != nil {
		t.Fatalf("the episode must survive a takedown: %v", err)
	}
	if !ep.AirBarred {
		t.Fatal("the takedown did not bar re-airing")
	}

	again := postForm(t, ts, alice.sessionCreds(), "/me/episodes/ep1/air", url.Values{"strand": {"music"}})
	defer again.Body.Close()
	if again.StatusCode != http.StatusForbidden {
		t.Fatalf("re-air after a takedown: got %d, want 403", again.StatusCode)
	}
}

// TestAdminUnAirIsAdminOnly: an ordinary user must not be able to reach
// into someone else's publishing decisions.
func TestAdminUnAirIsAdminOnly(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bobby")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	resp := postForm(t, ts, bob.sessionCreds(), "/admin/airings/"+id+"/unair", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-admin takedown: got %d, want 404", resp.StatusCode)
	}
	if _, err := st.GetAiring(context.Background(), id); err != nil {
		t.Fatal("a non-admin took an episode off the air")
	}
}

// TestVouchNotForYourOwn: at a dozen users, self-vouching past a Bar of
// one would make every Bar decorative (ADR 0019).
func TestVouchNotForYourOwn(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	resp := postForm(t, ts, alice.sessionCreds(), "/me/vouches/"+id, url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("self-vouch: got %d, want 403", resp.StatusCode)
	}
	if v, _ := st.ListVouches(context.Background(), id); len(v) != 0 {
		t.Fatalf("self-vouch recorded: %+v", v)
	}
}

// TestVouchIsIdempotentOverHTTP: one person, one name, however many
// times they press the button.
func TestVouchIsIdempotentOverHTTP(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bobby")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	for range 3 {
		resp := postForm(t, ts, bob.sessionCreds(), "/me/vouches/"+id, url.Values{})
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("vouch: got %d, want 303", resp.StatusCode)
		}
	}
	vouches, err := st.ListVouches(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(vouches) != 1 || vouches[0].UserID != "bobby" {
		t.Fatalf("ListVouches = %+v, want one from bobby", vouches)
	}

	del := deleteNoRedirect(t, ts, bob.sessionCreds(), "/me/vouches/"+id)
	del.Body.Close()
	if del.StatusCode != http.StatusSeeOther {
		t.Fatalf("unvouch: got %d, want 303", del.StatusCode)
	}
	if v, _ := st.ListVouches(context.Background(), id); len(v) != 0 {
		t.Fatalf("vouch survived removal: %+v", v)
	}
}

// TestFollowSetsAndAdjustsTheBar: the same call starts a Follow and
// changes its Bar, because raising it is the same act as choosing it.
func TestFollowSetsAndAdjustsTheBar(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")

	resp := postForm(t, ts, alice.sessionCreds(), "/me/follows/music", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("follow: got %d, want 303", resp.StatusCode)
	}
	follows, err := st.ListFollows(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(follows) != 1 || follows[0].Bar != store.DefaultBar {
		t.Fatalf("ListFollows = %+v, want one at the default bar", follows)
	}

	raise := postForm(t, ts, alice.sessionCreds(), "/me/follows/music", url.Values{"bar": {"3"}})
	raise.Body.Close()
	follows, _ = st.ListFollows(context.Background(), "alice")
	if len(follows) != 1 || follows[0].Bar != 3 {
		t.Fatalf("after raising: %+v, want a single follow at bar 3", follows)
	}

	bad := postForm(t, ts, alice.sessionCreds(), "/me/follows/music", url.Values{"bar": {"99"}})
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("out-of-range bar: got %d, want 400", bad.StatusCode)
	}

	del := deleteNoRedirect(t, ts, alice.sessionCreds(), "/me/follows/music")
	del.Body.Close()
	if follows, _ = st.ListFollows(context.Background(), "alice"); len(follows) != 0 {
		t.Fatalf("follow survived unfollow: %+v", follows)
	}
}

// TestFollowRefusesRetiredStrand: a Retired Strand keeps serving the
// people already subscribed but takes no new followers.
func TestFollowRefusesRetiredStrand(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "gone", true, true)
	alice := createUser(t, ts, "alice")

	resp := postForm(t, ts, alice.sessionCreds(), "/me/follows/gone", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("follow a retired strand: got %d, want 422", resp.StatusCode)
	}
}

// TestDeleteEpisodeTakesItOffTheAir: the Owner's delete propagates
// everywhere with no tombstone (ADR 0006), and the public surface is now
// one of those places — over HTTP, not only in the store.
func TestDeleteEpisodeTakesItOffTheAir(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	id := airedEpisode(t, ts, st, alice, "ep1", "music")

	resp := do(t, "DELETE", ts.URL+"/me/episodes/ep1", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", resp.StatusCode)
	}
	if _, err := st.GetAiring(context.Background(), id); err == nil {
		t.Fatal("deleted audio is still on the air")
	}
}
