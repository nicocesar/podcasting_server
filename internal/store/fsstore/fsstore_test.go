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

func TestShowLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.GetShow(ctx, "ai-news"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetShow on missing show: got %v, want ErrNotFound", err)
	}
	if err := s.UpsertShow(ctx, store.Show{ID: "ai-news", Title: "AI News"}); err != nil {
		t.Fatal(err)
	}
	sh, err := s.GetShow(ctx, "ai-news")
	if err != nil {
		t.Fatal(err)
	}
	if sh.Title != "AI News" || sh.ID != "ai-news" {
		t.Fatalf("unexpected show: %+v", sh)
	}
	shows, err := s.ListShows(ctx)
	if err != nil || len(shows) != 1 {
		t.Fatalf("ListShows: %v, %d shows", err, len(shows))
	}
	if err := s.DeleteShow(ctx, "ai-news"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetShow(ctx, "ai-news"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("show still present after delete: %v", err)
	}
}

func publish(t *testing.T, s *Store, slug, title, content string, at time.Time) store.Episode {
	t.Helper()
	ep, err := s.UpsertEpisode(context.Background(), store.Episode{
		ShowID:      "ai-news",
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
	if err := s.UpsertShow(ctx, store.Show{ID: "ai-news", Title: "AI News"}); err != nil {
		t.Fatal(err)
	}

	morning := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	noon := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	publish(t, s, "2026-07-06-morning", "Morning", "AUDIO-A", morning)
	publish(t, s, "2026-07-06-noon", "Noon", "AUDIO-BB", noon)

	eps, err := s.ListEpisodes(ctx, "ai-news")
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
	publish(t, s, "2026-07-06-morning", "Morning v2", "AUDIO-CCC", morning)
	eps, _ = s.ListEpisodes(ctx, "ai-news")
	if len(eps) != 2 {
		t.Fatalf("republish created a duplicate: %d episodes", len(eps))
	}
	audio, err := s.OpenAudio(ctx, "ai-news", "2026-07-06-morning")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Content.Close()
	b, _ := io.ReadAll(audio.Content)
	if string(b) != "AUDIO-CCC" {
		t.Fatalf("audio not replaced: %q", b)
	}
	ep, _ := s.GetEpisode(ctx, "ai-news", "2026-07-06-morning")
	if ep.Title != "Morning v2" || ep.AudioSize != int64(len("AUDIO-CCC")) {
		t.Fatalf("metadata not replaced: %+v", ep)
	}

	if err := s.DeleteEpisode(ctx, "ai-news", "2026-07-06-noon"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEpisode(ctx, "ai-news", "2026-07-06-noon"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: got %v, want ErrNotFound", err)
	}
}

func TestPublishToMissingShow(t *testing.T) {
	s := newStore(t)
	_, err := s.UpsertEpisode(context.Background(), store.Episode{ShowID: "nope", Slug: "x"}, strings.NewReader("a"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCover(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.UpsertShow(ctx, store.Show{ID: "ai-news", Title: "AI News"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenCover(ctx, "ai-news"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cover on fresh show: got %v, want ErrNotFound", err)
	}
	if err := s.SetCover(ctx, "ai-news", "image/png", strings.NewReader("PNGBYTES")); err != nil {
		t.Fatal(err)
	}
	rc, ct, err := s.OpenCover(ctx, "ai-news")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if ct != "image/png" || string(b) != "PNGBYTES" {
		t.Fatalf("got %q %q", ct, b)
	}
	// Show keeps its cover type on metadata reload.
	sh, _ := s.GetShow(ctx, "ai-news")
	if sh.CoverType != "image/png" {
		t.Fatalf("CoverType = %q", sh.CoverType)
	}
}
