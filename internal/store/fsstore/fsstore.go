// Package fsstore is the local-development storage backend: a plain
// directory tree that is read live, so episodes can be added or edited
// with a text editor and show up on the next feed request.
//
// Layout:
//
//	root/
//	├── ai-news/                    show ID
//	│   ├── show.json               show metadata
//	│   ├── cover.jpg               cover art (name depends on type)
//	│   ├── 2026-07-06-morning.mp3
//	│   └── 2026-07-06-morning.json episode metadata sidecar
//	└── markets/
package fsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const showFile = "show.json"

type Store struct {
	root string
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("fsstore: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) showDir(id string) string { return filepath.Join(s.root, id) }

func (s *Store) UpsertShow(_ context.Context, sh store.Show) error {
	dir := s.showDir(sh.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sh.UpdatedAt = time.Now().UTC()
	return writeJSON(filepath.Join(dir, showFile), sh)
}

func (s *Store) GetShow(_ context.Context, id string) (store.Show, error) {
	var sh store.Show
	if err := readJSON(filepath.Join(s.showDir(id), showFile), &sh); err != nil {
		return store.Show{}, err
	}
	sh.ID = id // directory name is canonical
	return sh, nil
}

func (s *Store) ListShows(ctx context.Context) ([]store.Show, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	shows := []store.Show{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sh, err := s.GetShow(ctx, e.Name())
		if err != nil {
			continue // directory without show.json: not a show
		}
		shows = append(shows, sh)
	}
	sort.Slice(shows, func(i, j int) bool { return shows[i].ID < shows[j].ID })
	return shows, nil
}

func (s *Store) DeleteShow(ctx context.Context, id string) error {
	if _, err := s.GetShow(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(s.showDir(id))
}

func (s *Store) UpsertEpisode(ctx context.Context, ep store.Episode, audio io.Reader) (store.Episode, error) {
	if _, err := s.GetShow(ctx, ep.ShowID); err != nil {
		return store.Episode{}, err
	}
	dir := s.showDir(ep.ShowID)
	if ep.AudioType == "" {
		ep.AudioType = "audio/mpeg"
	}

	size, err := writeAtomic(filepath.Join(dir, ep.Slug+".mp3"), audio)
	if err != nil {
		return store.Episode{}, err
	}
	ep.AudioSize = size
	if err := writeJSON(filepath.Join(dir, ep.Slug+".json"), ep); err != nil {
		return store.Episode{}, err
	}
	return ep, nil
}

func (s *Store) GetEpisode(_ context.Context, showID, slug string) (store.Episode, error) {
	return s.readEpisode(showID, slug)
}

func (s *Store) readEpisode(showID, slug string) (store.Episode, error) {
	path := filepath.Join(s.showDir(showID), slug+".json")
	var ep store.Episode
	if err := readJSON(path, &ep); err != nil {
		return store.Episode{}, err
	}
	// File names are canonical; sidecar content may be hand-written and
	// missing fields, so fill in what we can observe.
	ep.ShowID, ep.Slug = showID, slug
	if ep.AudioType == "" {
		ep.AudioType = "audio/mpeg"
	}
	if fi, err := os.Stat(filepath.Join(s.showDir(showID), slug+".mp3")); err == nil {
		ep.AudioSize = fi.Size()
		if ep.PublishedAt.IsZero() {
			ep.PublishedAt = fi.ModTime().UTC()
		}
	}
	return ep, nil
}

func (s *Store) ListEpisodes(ctx context.Context, showID string) ([]store.Episode, error) {
	if _, err := s.GetShow(ctx, showID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.showDir(showID))
	if err != nil {
		return nil, err
	}
	eps := []store.Episode{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == showFile || !strings.HasSuffix(name, ".json") {
			continue
		}
		ep, err := s.readEpisode(showID, strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		eps = append(eps, ep)
	}
	sort.Slice(eps, func(i, j int) bool {
		if !eps[i].PublishedAt.Equal(eps[j].PublishedAt) {
			return eps[i].PublishedAt.After(eps[j].PublishedAt)
		}
		return eps[i].Slug > eps[j].Slug
	})
	return eps, nil
}

func (s *Store) DeleteEpisode(_ context.Context, showID, slug string) error {
	dir := s.showDir(showID)
	errJSON := os.Remove(filepath.Join(dir, slug+".json"))
	errMP3 := os.Remove(filepath.Join(dir, slug+".mp3"))
	if os.IsNotExist(errJSON) && os.IsNotExist(errMP3) {
		return store.ErrNotFound
	}
	for _, err := range []error{errJSON, errMP3} {
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Store) OpenAudio(_ context.Context, showID, slug string) (store.Audio, error) {
	f, err := os.Open(filepath.Join(s.showDir(showID), slug+".mp3"))
	if err != nil {
		if os.IsNotExist(err) {
			return store.Audio{}, store.ErrNotFound
		}
		return store.Audio{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return store.Audio{}, err
	}
	return store.Audio{
		Content:     f,
		Size:        fi.Size(),
		ModTime:     fi.ModTime(),
		ContentType: "audio/mpeg",
	}, nil
}

func coverFile(contentType string) string {
	switch contentType {
	case "image/png":
		return "cover.png"
	default:
		return "cover.jpg"
	}
}

func (s *Store) SetCover(ctx context.Context, showID, contentType string, r io.Reader) error {
	sh, err := s.GetShow(ctx, showID)
	if err != nil {
		return err
	}
	if sh.CoverType != "" && coverFile(sh.CoverType) != coverFile(contentType) {
		os.Remove(filepath.Join(s.showDir(showID), coverFile(sh.CoverType)))
	}
	if _, err := writeAtomic(filepath.Join(s.showDir(showID), coverFile(contentType)), r); err != nil {
		return err
	}
	sh.CoverType = contentType
	return s.UpsertShow(ctx, sh)
}

func (s *Store) OpenCover(ctx context.Context, showID string) (io.ReadCloser, string, error) {
	sh, err := s.GetShow(ctx, showID)
	if err != nil {
		return nil, "", err
	}
	if sh.CoverType == "" {
		return nil, "", store.ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.showDir(showID), coverFile(sh.CoverType)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", store.ErrNotFound
		}
		return nil, "", err
	}
	return f, sh.CoverType, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store.ErrNotFound
		}
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = writeAtomic(path, strings.NewReader(string(b)+"\n"))
	return err
}

// writeAtomic writes r to a temp file in the target directory and renames
// it into place, so a crashed upload never leaves a truncated file where
// the feed can see it.
func writeAtomic(path string, r io.Reader) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	return n, os.Rename(tmp.Name(), path)
}
