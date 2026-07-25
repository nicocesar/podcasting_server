package fsstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// TestBeatRoundTrip: every field a Beat carries must survive storage.
// A Beat rebuilds a whole Generation from its stored copy, so a field
// quietly lost here becomes an Episode produced with the wrong request —
// and nobody is watching when it happens. Cast is the specific trap: it
// is json:"-" and survives only through the beatRecord shadow.
func TestBeatRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	want := store.Beat{
		UserID: "alice", ID: "beat1",
		Template: "stories", Topic: "a dragon afraid of heights",
		LengthMinutes: 10, FreshnessDays: 7, AgeRange: "5-7",
		SaveCharacters: true,
		Cast:           []store.Character{{Name: "Lila", Description: "A brave young fox."}},
		Language:       "es", Voice: "male", Provider: "elevenlabs",
		IntervalDays: 3, Paused: true,
		LastFiredAt: now.Add(-48 * time.Hour), LastSucceededAt: now.Add(-72 * time.Hour),
		ConsecutiveFailures: 2, LastError: "all TTS engines failed",
		EpisodeCount: 11, CreatedAt: now.Add(-30 * 24 * time.Hour),
	}
	if err := s.PutBeat(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBeat(ctx, "alice", "beat1")
	if err != nil {
		t.Fatal(err)
	}
	// UpdatedAt is stamped by the store, so it is the one field that
	// legitimately differs.
	got.UpdatedAt = want.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the beat:\n got %+v\nwant %+v", got, want)
	}
}

func TestBeatListAndDelete(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertUser(ctx, store.User{ID: "bob", Title: "Bob"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, id := range []string{"b1", "b2"} {
		if err := s.PutBeat(ctx, store.Beat{
			UserID: "alice", ID: id, Template: "news", Topic: id,
			IntervalDays: 1, CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutBeat(ctx, store.Beat{
		UserID: "bob", ID: "b3", Template: "news", Topic: "b3",
		IntervalDays: 1, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	beats, err := s.ListBeats(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	// Newest-first, and strictly one user's — a Beat is examined on its
	// owner's traffic, so a list that leaked across users would fire
	// somebody else's.
	if len(beats) != 2 || beats[0].ID != "b2" || beats[1].ID != "b1" {
		t.Fatalf("ListBeats(alice) = %+v", beats)
	}

	if err := s.DeleteBeat(ctx, "alice", "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBeat(ctx, "alice", "b1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetBeat after delete: %v, want ErrNotFound", err)
	}
	if err := s.DeleteBeat(ctx, "alice", "b1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second DeleteBeat: %v, want ErrNotFound", err)
	}
	// Bob's beat is untouched by anything done to Alice's.
	if beats, err := s.ListBeats(ctx, "bob"); err != nil || len(beats) != 1 {
		t.Fatalf("ListBeats(bob) = %+v, %v", beats, err)
	}
}

// TestListBeatsNoDirectory: a user who has never made one lists cleanly
// rather than erroring on the missing directory.
func TestListBeatsNoDirectory(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	beats, err := s.ListBeats(ctx, "alice")
	if err != nil || len(beats) != 0 {
		t.Fatalf("ListBeats = %+v, %v", beats, err)
	}
}
