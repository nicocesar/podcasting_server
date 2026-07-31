package fsstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// seenAt provisions a user and stamps their liveness, which is how these
// tests say "this account has been quiet for a week" without waiting one.
func seenAt(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	addUser(t, s, id)
	if err := s.TouchUser(context.Background(), id, at); err != nil {
		t.Fatal(err)
	}
}

func TestListUsersSeenSince(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()

	seenAt(t, s, "recent", now.Add(-time.Hour))
	seenAt(t, s, "older", now.Add(-48*time.Hour))
	seenAt(t, s, "exactly", now.Add(-72*time.Hour))
	addUser(t, s, "never") // provisioned, never seen

	cutoff := now.Add(-72 * time.Hour)
	live, err := s.ListUsersSeenSince(ctx, cutoff, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Least-recently-seen first: this is the fairness rule when a Tick
	// runs out of budget, so the order is part of the contract.
	want := []string{"exactly", "older", "recent"}
	if len(live) != len(want) {
		t.Fatalf("got %d live users, want %d: %+v", len(live), len(want), live)
	}
	for i, id := range want {
		if live[i].ID != id {
			t.Errorf("position %d: got %q, want %q", i, live[i].ID, id)
		}
	}
}

// A never-seen user must be invisible at any cutoff. In gcpstore this is
// free — an entity without the property is not in its index — and fsstore
// has to reproduce it explicitly or the two backends disagree about who
// is live.
func TestListUsersSeenSinceExcludesNeverSeen(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addUser(t, s, "never")

	live, err := s.ListUsersSeenSince(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("zero cutoff returned never-seen users: %+v", live)
	}
}

func TestListUsersSeenSinceCutoffIsInclusive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	seenAt(t, s, "onthedot", cutoff)
	seenAt(t, s, "justbefore", cutoff.Add(-time.Second))

	live, err := s.ListUsersSeenSince(ctx, cutoff, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != "onthedot" {
		t.Fatalf("got %+v, want only onthedot", live)
	}
}

func TestListUsersSeenSinceLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()

	seenAt(t, s, "first", now.Add(-3*time.Hour))
	seenAt(t, s, "second", now.Add(-2*time.Hour))
	seenAt(t, s, "third", now.Add(-time.Hour))

	live, err := s.ListUsersSeenSince(ctx, now.Add(-24*time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	// The limit takes from the front of the ordering, so it keeps the
	// least-recently-seen rather than an arbitrary two.
	if len(live) != 2 || live[0].ID != "first" || live[1].ID != "second" {
		t.Fatalf("got %+v, want first and second", live)
	}
}

// Being seen is not an edit: TouchUser must leave UpdatedAt alone, or
// every feed poll would look like a profile change.
func TestTouchUserLeavesUpdatedAt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addUser(t, s, "alice")

	before, err := s.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !before.LastSeenAt.IsZero() {
		t.Fatalf("a fresh user is already live: %v", before.LastSeenAt)
	}

	seen := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	if err := s.TouchUser(ctx, "alice", seen); err != nil {
		t.Fatal(err)
	}

	after, err := s.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastSeenAt.Equal(seen) {
		t.Errorf("LastSeenAt: got %v, want %v", after.LastSeenAt, seen)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("UpdatedAt moved: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	// The rest of the record has to survive a write that touches one field.
	if after.Title != before.Title || after.FeedToken != before.FeedToken ||
		after.PasswordHash != before.PasswordHash || after.CredentialVersion != before.CredentialVersion {
		t.Errorf("TouchUser damaged the record: %+v -> %+v", before, after)
	}
}

func TestTouchUserMissing(t *testing.T) {
	err := newStore(t).TouchUser(context.Background(), "nobody", time.Now())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestTickStatusRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// Never having run is the interesting answer on a fresh deployment.
	if _, err := s.GetTickStatus(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTickStatus before any tick: got %v, want ErrNotFound", err)
	}

	want := store.TickStatus{
		At:         time.Now().UTC().Truncate(time.Second),
		DurationMS: 412,
		LiveUsers:  7,
		BeatsFired: 2,
		Resumed:    1,
		Truncated:  true,
		Trigger:    "scheduler",
		Error:      "alice: beats unreadable",
	}
	if err := s.PutTickStatus(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTickStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.At.Equal(want.At) {
		t.Errorf("At: got %v, want %v", got.At, want.At)
	}
	got.At, want.At = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// One record, not a log: the second write replaces the first.
	second := store.TickStatus{At: want.At.Add(time.Hour), Trigger: "admin", LiveUsers: 3}
	if err := s.PutTickStatus(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetTickStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Trigger != "admin" || got.LiveUsers != 3 || got.BeatsFired != 0 {
		t.Errorf("second write did not replace the first: %+v", got)
	}
}

// tick.json lives at the root beside invites.json, and ListUsers only
// descends into directories — so it must never be mistaken for a user.
func TestTickStatusIsNotAUser(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	addUser(t, s, "alice")
	if err := s.PutTickStatus(ctx, store.TickStatus{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].ID != "alice" {
		t.Fatalf("got %+v, want only alice", users)
	}
}
