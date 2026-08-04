package fsstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

func TestAssetRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := s.PutAsset(ctx, "sfx/v1/lib/duck_quack-1500ms.mp3", "audio/mpeg", bytes.NewReader([]byte("quack"))); err != nil {
		t.Fatal(err)
	}
	r, ct, err := s.OpenAsset(ctx, "sfx/v1/lib/duck_quack-1500ms.mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "quack" {
		t.Errorf("asset bytes = %q, want %q", b, "quack")
	}
	// The content type is stored rather than guessed back from the
	// extension, so both backends return exactly what was written.
	if ct != "audio/mpeg" {
		t.Errorf("content type = %q, want audio/mpeg", ct)
	}
}

func TestOpenAssetMissingIsNotFound(t *testing.T) {
	// The ordinary cache miss. It has to be distinguishable from a real
	// failure, or every first render would look like an error.
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.OpenAsset(context.Background(), "sfx/v1/gen/nothing.mp3")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAssetKeysCannotEscapeTheAssetTree(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{
		"../escaped.mp3",
		"sfx/../../escaped.mp3",
		"/etc/passwd",
		"",
	} {
		t.Run(key, func(t *testing.T) {
			if err := s.PutAsset(ctx, key, "audio/mpeg", bytes.NewReader([]byte("x"))); err == nil {
				t.Errorf("PutAsset(%q) was allowed", key)
			}
			if _, _, err := s.OpenAsset(ctx, key); err == nil {
				t.Errorf("OpenAsset(%q) was allowed", key)
			}
		})
	}
	// Nothing may have been written outside root/assets.
	if _, err := os.Stat(filepath.Join(root, "escaped.mp3")); err == nil {
		t.Error("a traversal key wrote outside the assets tree")
	}
}

func TestDeleteUserLeavesAssetsAlone(t *testing.T) {
	// Assets belong to the station, not to a User. A User's departure must
	// not take the shared sound library with it.
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAsset(ctx, "sfx/v1/lib/duck_quack-1500ms.mp3", "audio/mpeg", bytes.NewReader([]byte("quack"))); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	r, _, err := s.OpenAsset(ctx, "sfx/v1/lib/duck_quack-1500ms.mp3")
	if err != nil {
		t.Fatalf("deleting a user removed a station asset: %v", err)
	}
	r.Close()
}

func TestGenerationPerformanceFieldsRoundTrip(t *testing.T) {
	// TargetLanguage and the performance meters carry real JSON tags, so they
	// ride the embedded Generation rather than needing a line in
	// generationRecord — unlike Cast and Trace. This is the test that says
	// so out loud, since the failure mode is silent data loss.
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	g := store.Generation{
		UserID: "alice", ID: "studio1",
		Template: "stories-v2", Topic: "a duck and a pig", LengthMinutes: 3,
		Language: "en", TargetLanguage: "es",
		DialogueRequests: 4, SFXGenerated: 2, SFXCacheHits: 7,
		Stage: store.GenResearching, Active: true, CreatedAt: time.Now().UTC(),
	}
	if err := s.PutGeneration(ctx, g); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGeneration(ctx, "alice", "studio1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetLanguage != "es" {
		t.Errorf("TargetLanguage = %q, want es", got.TargetLanguage)
	}
	if got.DialogueRequests != 4 || got.SFXGenerated != 2 || got.SFXCacheHits != 7 {
		t.Errorf("performance meters lost: %+v", got)
	}
}

func TestBeatTargetLanguageRoundTrips(t *testing.T) {
	// A recurring bilingual story has to stay bilingual on every firing.
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	b := store.Beat{
		UserID: "alice", ID: "beat1",
		Template: "stories-v2", Topic: "the farm", LengthMinutes: 3,
		Language: "en", TargetLanguage: "es", IntervalDays: 1,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.PutBeat(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBeat(ctx, "alice", "beat1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetLanguage != "es" {
		t.Errorf("TargetLanguage = %q, want es", got.TargetLanguage)
	}
}
