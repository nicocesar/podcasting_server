package fsstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

func newStrandStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"alice", "bob"} {
		if err := s.UpsertUser(ctx, store.User{ID: id, Title: strings.ToUpper(id[:1]) + id[1:]}); err != nil {
			t.Fatal(err)
		}
	}
	return s, ctx
}

func mustEpisode(t *testing.T, s *Store, ctx context.Context, owner, slug string) {
	t.Helper()
	_, err := s.UpsertEpisode(ctx, store.Episode{
		OwnerID: owner, Slug: slug, Title: slug, PublishedAt: time.Now().UTC(),
	}, strings.NewReader("audio"))
	if err != nil {
		t.Fatal(err)
	}
}

func mustAir(t *testing.T, s *Store, ctx context.Context, id, owner, slug, strand string, at time.Time) store.Airing {
	t.Helper()
	a := store.Airing{ID: id, OwnerID: owner, Slug: slug, Strand: strand, AiredAt: at}
	if err := s.PutAiring(ctx, a); err != nil {
		t.Fatal(err)
	}
	return a
}

// TestStrandRoundTrip: the canon is the one piece of this feature an
// admin edits by hand, and every field of it shows up in a public feed.
func TestStrandRoundTrip(t *testing.T) {
	s, ctx := newStrandStore(t)
	want := store.Strand{
		ID: "tech-news", Title: "Tech News", Description: "What happened, briefly.",
		CoverType: "image/png", Retired: true,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.PutStrand(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetStrand(ctx, "tech-news")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != want.Title || got.Description != want.Description {
		t.Errorf("title/description lost: got %+v", got)
	}
	if got.CoverType != want.CoverType || !got.Retired {
		t.Errorf("cover type or retirement lost: got %+v", got)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}

	if _, err := s.GetStrand(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetStrand(missing) = %v, want ErrNotFound", err)
	}
}

// TestListStrandsOrdered: the canon renders as the discovery surface, so
// its order must not depend on insertion order.
func TestListStrandsOrdered(t *testing.T) {
	s, ctx := newStrandStore(t)
	for _, id := range []string{"music", "tech-news", "global-news"} {
		if err := s.PutStrand(ctx, store.Strand{ID: id, Title: id}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListStrands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, st := range all {
		ids = append(ids, st.ID)
	}
	want := []string{"global-news", "music", "tech-news"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ListStrands = %v, want %v", ids, want)
	}
}

// TestPutStrandReplaces: an admin editing a title must not fork the
// canon entry into two.
func TestPutStrandReplaces(t *testing.T) {
	s, ctx := newStrandStore(t)
	if err := s.PutStrand(ctx, store.Strand{ID: "music", Title: "Music"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStrand(ctx, store.Strand{ID: "music", Title: "Chillout"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListStrands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Title != "Chillout" {
		t.Fatalf("ListStrands = %+v, want one entry titled Chillout", all)
	}
}

// TestAiringByEpisodeAndStrand: the two lookups the whole public surface
// rests on — resolve a public id, and list a strand newest-first.
func TestAiringByEpisodeAndStrand(t *testing.T) {
	s, ctx := newStrandStore(t)
	now := time.Now().UTC()
	mustEpisode(t, s, ctx, "alice", "2026-07-25-morning")
	mustEpisode(t, s, ctx, "bob", "2026-07-25-morning") // same slug, different owner
	mustAir(t, s, ctx, "aaaa111111", "alice", "2026-07-25-morning", "tech-news", now.Add(-2*time.Hour))
	mustAir(t, s, ctx, "bbbb222222", "bob", "2026-07-25-morning", "tech-news", now.Add(-time.Hour))

	// The reason the opaque id exists: the same slug under two owners.
	got, err := s.GetAiringByEpisode(ctx, "bob", "2026-07-25-morning")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "bbbb222222" {
		t.Fatalf("GetAiringByEpisode returned %q, want bob's airing", got.ID)
	}

	list, err := s.ListAirings(ctx, "tech-news")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "bbbb222222" {
		t.Fatalf("ListAirings = %+v, want two, newest first", list)
	}
	if other, err := s.ListAirings(ctx, "music"); err != nil || len(other) != 0 {
		t.Fatalf("ListAirings(music) = %+v, %v; want empty", other, err)
	}
}

// TestDeleteEpisodeUnAirs: an Owner's delete propagates everywhere with
// no tombstone (ADR 0006), and since ADR 0018 the public surface is one
// of those places. If this regresses, deleted audio stays on the air.
func TestDeleteEpisodeUnAirs(t *testing.T) {
	s, ctx := newStrandStore(t)
	mustEpisode(t, s, ctx, "alice", "2026-07-25-morning")
	mustAir(t, s, ctx, "aaaa111111", "alice", "2026-07-25-morning", "tech-news", time.Now().UTC())

	if err := s.DeleteEpisode(ctx, "alice", "2026-07-25-morning"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAiring(ctx, "aaaa111111"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("airing survived the episode delete: %v", err)
	}
}

// TestDeleteUserUnAirs: deleting a user takes their airings off the air
// and leaves everyone else's alone.
func TestDeleteUserUnAirs(t *testing.T) {
	s, ctx := newStrandStore(t)
	mustEpisode(t, s, ctx, "alice", "ep1")
	mustEpisode(t, s, ctx, "bob", "ep2")
	mustAir(t, s, ctx, "aaaa111111", "alice", "ep1", "music", time.Now().UTC())
	mustAir(t, s, ctx, "bbbb222222", "bob", "ep2", "music", time.Now().UTC())

	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAiring(ctx, "aaaa111111"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted user's airing survived: %v", err)
	}
	if _, err := s.GetAiring(ctx, "bbbb222222"); err != nil {
		t.Fatalf("bob's airing should be untouched: %v", err)
	}
}

// TestUnAirRemovesTheAiring: an un-air is what makes a re-air mint a
// new id, so the old one must really be gone and stay gone.
func TestUnAirRemovesTheAiring(t *testing.T) {
	s, ctx := newStrandStore(t)
	mustEpisode(t, s, ctx, "alice", "ep1")
	mustAir(t, s, ctx, "aaaa111111", "alice", "ep1", "music", time.Now().UTC())
	if err := s.DeleteAiring(ctx, "aaaa111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAiring(ctx, "aaaa111111"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAiring after un-air = %v, want ErrNotFound", err)
	}
	if err := s.DeleteAiring(ctx, "aaaa111111"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteAiring = %v, want ErrNotFound", err)
	}
}

// TestFollowRoundTrip: a Follow is per (user, strand), so following the
// same strand twice edits one record rather than adding a second.
func TestFollowRoundTrip(t *testing.T) {
	s, ctx := newStrandStore(t)
	if err := s.PutFollow(ctx, store.Follow{UserID: "alice", Strand: "music"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFollow(ctx, store.Follow{UserID: "alice", Strand: "tech-news"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFollow(ctx, store.Follow{UserID: "alice", Strand: "music"}); err != nil {
		t.Fatal(err)
	}

	follows, err := s.ListFollows(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(follows) != 2 {
		t.Fatalf("ListFollows = %+v, want two", follows)
	}
	// Three PutFollows, two records: the repeat edited music rather than
	// adding a second one. With no Bar left to vary, the count is the
	// only evidence of that (ADR 0027).
	if follows[0].Strand != "music" || follows[1].Strand != "tech-news" {
		t.Errorf("follows are not the two strands, ordered: %+v", follows)
	}
	if follows[0].UserID != "alice" {
		t.Errorf("UserID not restored on read: %+v", follows[0])
	}

	if bobs, err := s.ListFollows(ctx, "bob"); err != nil || len(bobs) != 0 {
		t.Fatalf("ListFollows(bob) = %+v, %v; want empty", bobs, err)
	}
	if err := s.DeleteFollow(ctx, "alice", "music"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFollow(ctx, "alice", "music"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteFollow = %v, want ErrNotFound", err)
	}
}

// TestStrandsDirIsNotAUser: the canon's cover art lives in a directory
// beside the user directories, and ListUsers walks that same root.
func TestStrandsDirIsNotAUser(t *testing.T) {
	s, ctx := newStrandStore(t)
	if err := s.PutStrand(ctx, store.Strand{ID: "music", Title: "Music"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStrandCover(ctx, "music", "image/jpeg", strings.NewReader("full"), strings.NewReader("thumb")); err != nil {
		t.Fatal(err)
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.ID == strandsDir {
			t.Fatalf("the %q directory was listed as a user", strandsDir)
		}
	}
	if len(users) != 2 {
		t.Fatalf("ListUsers = %+v, want alice and bob only", users)
	}

	rc, contentType, err := s.OpenStrandCover(ctx, "music")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if contentType != "image/jpeg" {
		t.Errorf("cover content type = %q, want image/jpeg", contentType)
	}
	if _, _, err := s.OpenStrandCoverThumb(ctx, "music"); err != nil {
		t.Errorf("OpenStrandCoverThumb: %v", err)
	}
}

// TestDormantUntilCoverArt: SetStrandCover is what wakes a seeded strand
// up, and nothing else should.
func TestDormantUntilCoverArt(t *testing.T) {
	s, ctx := newStrandStore(t)
	seed := store.SeedStrands()[0]
	if err := s.PutStrand(ctx, seed); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetStrand(ctx, seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Dormant() {
		t.Fatal("a seeded strand must be dormant until an admin gives it art")
	}
	if err := s.SetStrandCover(ctx, seed.ID, "image/jpeg", strings.NewReader("full"), strings.NewReader("thumb")); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetStrand(ctx, seed.ID); err != nil {
		t.Fatal(err)
	}
	if got.Dormant() {
		t.Fatal("a strand with cover art must not be dormant")
	}
}
