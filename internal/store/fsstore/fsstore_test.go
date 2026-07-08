package fsstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func addUser(t *testing.T, s *Store, id string) {
	t.Helper()
	err := s.UpsertUser(context.Background(), store.User{
		ID:          id,
		Title:       id + "'s feed",
		CoverSecret: "secret-" + id,
		ReadHash:    "rh-" + id,
		PublishHash: "ph-" + id,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUserLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.GetUser(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetUser on missing user: got %v, want ErrNotFound", err)
	}
	addUser(t, s, "alice")
	u, err := s.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "alice" || u.Title != "alice's feed" {
		t.Fatalf("unexpected user: %+v", u)
	}
	// Credential hashes and the cover secret survive the round trip even
	// though they are hidden from API JSON.
	if u.ReadHash != "rh-alice" || u.PublishHash != "ph-alice" || u.CoverSecret != "secret-alice" {
		t.Fatalf("secrets lost on round trip: %+v", u)
	}
	if got, err := s.GetUserByCoverSecret(ctx, "secret-alice"); err != nil || got.ID != "alice" {
		t.Fatalf("GetUserByCoverSecret: %v, %+v", err, got)
	}
	if _, err := s.GetUserByCoverSecret(ctx, "wrong"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong secret: got %v, want ErrNotFound", err)
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers: %v, %d users", err, len(users))
	}
	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUser(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user still present after delete: %v", err)
	}
}

func publish(t *testing.T, s *Store, owner, slug, title, content string, at time.Time) store.Episode {
	t.Helper()
	ep, err := s.UpsertEpisode(context.Background(), store.Episode{
		OwnerID:     owner,
		Slug:        slug,
		Title:       title,
		PublishedAt: at,
	}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestEpisodeUpsertReplaceAndOrder(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addUser(t, s, "alice")

	morning := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	noon := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	publish(t, s, "alice", "2026-07-06-morning", "Morning", "AUDIO-A", morning)
	publish(t, s, "alice", "2026-07-06-noon", "Noon", "AUDIO-BB", noon)

	eps, err := s.ListEpisodes(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 || eps[0].Slug != "2026-07-06-noon" || eps[1].Slug != "2026-07-06-morning" {
		t.Fatalf("wrong order or count: %+v", eps)
	}
	if eps[0].AudioSize != int64(len("AUDIO-BB")) {
		t.Fatalf("wrong size: %d", eps[0].AudioSize)
	}

	// Republish same slug: replaces, no duplicate.
	publish(t, s, "alice", "2026-07-06-morning", "Morning v2", "AUDIO-CCC", morning)
	eps, _ = s.ListEpisodes(ctx, "alice")
	if len(eps) != 2 {
		t.Fatalf("republish created a duplicate: %d episodes", len(eps))
	}
	audio, err := s.OpenAudio(ctx, "alice", "2026-07-06-morning")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Content.Close()
	b, _ := io.ReadAll(audio.Content)
	if string(b) != "AUDIO-CCC" {
		t.Fatalf("audio not replaced: %q", b)
	}
	ep, _ := s.GetEpisode(ctx, "alice", "2026-07-06-morning")
	if ep.Title != "Morning v2" || ep.AudioSize != int64(len("AUDIO-CCC")) {
		t.Fatalf("metadata not replaced: %+v", ep)
	}

	if err := s.DeleteEpisode(ctx, "alice", "2026-07-06-noon"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEpisode(ctx, "alice", "2026-07-06-noon"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: got %v, want ErrNotFound", err)
	}
}

func TestPublishToMissingUser(t *testing.T) {
	s := newStore(t)
	_, err := s.UpsertEpisode(context.Background(), store.Episode{OwnerID: "nope", Slug: "x"}, strings.NewReader("a"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestSharesAndPropagation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addUser(t, s, "alice")
	addUser(t, s, "bob")
	addUser(t, s, "carol")
	at := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	publish(t, s, "alice", "2026-07-06-morning", "Morning", "AUDIO", at)

	// Sharing a missing episode fails.
	err := s.AddShare(ctx, store.Share{UserID: "bob", OwnerID: "alice", Slug: "nope", SharerID: "alice"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("share missing episode: got %v, want ErrNotFound", err)
	}

	// Alice shares to Bob; Bob forwards to Carol.
	if err := s.AddShare(ctx, store.Share{UserID: "bob", OwnerID: "alice", Slug: "2026-07-06-morning", SharerID: "alice", SharedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddShare(ctx, store.Share{UserID: "carol", OwnerID: "alice", Slug: "2026-07-06-morning", SharerID: "bob", SharedAt: at}); err != nil {
		t.Fatal(err)
	}

	// Re-sharing into the same feed keeps the first Sharer.
	if err := s.AddShare(ctx, store.Share{UserID: "carol", OwnerID: "alice", Slug: "2026-07-06-morning", SharerID: "alice", SharedAt: at}); err != nil {
		t.Fatal(err)
	}
	sh, err := s.GetShare(ctx, "carol", "alice", "2026-07-06-morning")
	if err != nil || sh.SharerID != "bob" {
		t.Fatalf("GetShare: %v, sharer %q (want bob)", err, sh.SharerID)
	}

	shares, err := s.ListShares(ctx, "bob")
	if err != nil || len(shares) != 1 || shares[0].UserID != "bob" || shares[0].OwnerID != "alice" {
		t.Fatalf("ListShares(bob): %v, %+v", err, shares)
	}

	// Bob removes it from his feed; Carol's reference is untouched.
	if err := s.RemoveShare(ctx, "bob", "alice", "2026-07-06-morning"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveShare(ctx, "bob", "alice", "2026-07-06-morning"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double remove: got %v, want ErrNotFound", err)
	}
	if _, err := s.GetShare(ctx, "carol", "alice", "2026-07-06-morning"); err != nil {
		t.Fatalf("carol's share vanished: %v", err)
	}

	// The owner's delete propagates to every referencing feed.
	if err := s.DeleteEpisode(ctx, "alice", "2026-07-06-morning"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetShare(ctx, "carol", "alice", "2026-07-06-morning"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("share survived episode delete: %v", err)
	}
}

func TestDeleteUserPropagates(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addUser(t, s, "alice")
	addUser(t, s, "bob")
	at := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	publish(t, s, "alice", "2026-07-06-morning", "Morning", "AUDIO", at)
	if err := s.AddShare(ctx, store.Share{UserID: "bob", OwnerID: "alice", Slug: "2026-07-06-morning", SharerID: "alice", SharedAt: at}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetShare(ctx, "bob", "alice", "2026-07-06-morning"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("share survived owner delete: %v", err)
	}
}

func TestInviteLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addUser(t, s, "alice")
	addUser(t, s, "bob")
	now := time.Date(2026, 7, 8, 8, 0, 0, 0, time.UTC)

	if _, err := s.GetInvite(ctx, "tok-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetInvite on missing invite: got %v, want ErrNotFound", err)
	}
	inv := store.Invite{
		Token: "tok-1", InviterID: "alice",
		OwnerID: "alice", Slug: "2026-07-08-morning",
		CreatedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour),
	}
	if err := s.AddInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if err := s.AddInvite(ctx, inv); err == nil {
		t.Fatal("duplicate token accepted")
	}
	if err := s.AddInvite(ctx, store.Invite{Token: "tok-2", InviterID: "alice", CreatedAt: now.Add(time.Hour), ExpiresAt: now.Add(8 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddInvite(ctx, store.Invite{Token: "tok-bob", InviterID: "bob", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// Listing is per-inviter, newest-first.
	invs, err := s.ListInvites(ctx, "alice")
	if err != nil || len(invs) != 2 || invs[0].Token != "tok-2" || invs[1].Token != "tok-1" {
		t.Fatalf("ListInvites(alice): %v, %+v", err, invs)
	}

	got, err := s.GetInvite(ctx, "tok-1")
	if err != nil || got.InviterID != "alice" || got.Slug != "2026-07-08-morning" {
		t.Fatalf("GetInvite: %v, %+v", err, got)
	}
	if !got.Redeemable(now) || got.Redeemable(now.Add(8*24*time.Hour)) {
		t.Fatalf("Redeemable wrong around expiry: %+v", got)
	}

	// Redemption is single-use.
	if err := s.RedeemInvite(ctx, "tok-1", "carol"); err != nil {
		t.Fatal(err)
	}
	if err := s.RedeemInvite(ctx, "tok-1", "dave"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second redemption: got %v, want ErrNotFound", err)
	}
	got, _ = s.GetInvite(ctx, "tok-1")
	if got.RedeemedBy != "carol" || got.Redeemable(now) {
		t.Fatalf("redeemed invite wrong: %+v", got)
	}

	// Revocation.
	if err := s.DeleteInvite(ctx, "tok-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteInvite(ctx, "tok-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: got %v, want ErrNotFound", err)
	}

	// Deleting a user removes the invites they minted, not others'.
	if err := s.DeleteUser(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInvite(ctx, "tok-bob"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob's invite survived his deletion: %v", err)
	}
	if _, err := s.GetInvite(ctx, "tok-1"); err != nil {
		t.Fatalf("alice's invite vanished with bob: %v", err)
	}
}

func TestCover(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addUser(t, s, "alice")
	if _, _, err := s.OpenCover(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cover on fresh user: got %v, want ErrNotFound", err)
	}
	if err := s.SetCover(ctx, "alice", "image/png", strings.NewReader("PNGBYTES")); err != nil {
		t.Fatal(err)
	}
	rc, ct, err := s.OpenCover(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if ct != "image/png" || string(b) != "PNGBYTES" {
		t.Fatalf("got %q %q", ct, b)
	}
	// The user keeps their cover type on metadata reload.
	u, _ := s.GetUser(ctx, "alice")
	if u.CoverType != "image/png" {
		t.Fatalf("CoverType = %q", u.CoverType)
	}
}
