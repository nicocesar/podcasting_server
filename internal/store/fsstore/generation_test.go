package fsstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

func TestGenerationTemplateFieldsRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	g := store.Generation{
		UserID: "alice", ID: "story1",
		Template: "stories", Topic: "a dragon", LengthMinutes: 2,
		AgeRange: "5-7", SaveCharacters: true,
		Cast:     []store.Character{{Name: "Lila", Description: "A brave young fox."}},
		Language: "en", Stage: store.GenResearching, Active: true,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.PutGeneration(ctx, g); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGeneration(ctx, "alice", "story1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Template != "stories" || got.AgeRange != "5-7" || !got.SaveCharacters {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if len(got.Cast) != 1 || got.Cast[0] != g.Cast[0] {
		t.Fatalf("cast = %+v", got.Cast) // Cast is json:"-": needs its record shadow
	}
}

func TestGenerationRoundTrip(t *testing.T) {
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

	g := store.Generation{
		UserID: "alice", ID: "abc123",
		Topic: "fusion", LengthMinutes: 5, FreshnessDays: 7, Language: "en",
		Stage: store.GenResearching, Active: true,
		SessionID: "sess-9", Script: `{"title":"t"}`,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.PutGeneration(ctx, g); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetGeneration(ctx, "alice", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "sess-9" || got.Script != `{"title":"t"}` || !got.Active || got.Stage != store.GenResearching {
		t.Fatalf("round trip lost fields: %+v", got)
	}

	// The hidden fields stay hidden from API JSON but survive storage;
	// the generations dir must not pollute the episode list.
	if eps, err := s.ListEpisodes(ctx, "alice"); err != nil || len(eps) != 0 {
		t.Fatalf("episodes = %v, %v", eps, err)
	}

	// Active scan sees alice's, not bob's finished one.
	done := g
	done.ID, done.Stage, done.Active = "zzz", store.GenDone, false
	done.UserID = "bob"
	if err := s.PutGeneration(ctx, done); err != nil {
		t.Fatal(err)
	}
	active, err := s.ListActiveGenerations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].UserID != "alice" || active[0].ID != "abc123" {
		t.Fatalf("active = %+v", active)
	}

	if _, err := s.GetGeneration(ctx, "alice", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	gens, err := s.ListGenerations(ctx, "bob")
	if err != nil || len(gens) != 1 || gens[0].Stage != store.GenDone {
		t.Fatalf("bob's generations = %v, %v", gens, err)
	}
}
