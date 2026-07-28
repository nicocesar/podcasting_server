package fsstore

// The public side on disk. Two global files beside invites.json — the
// canon and the airings — plus a follows.json per user, which sits next
// to that user's shares.json because a Follow is the third kind of
// reference in their feed.
//
//	root/
//	├── strands.json                the canon
//	├── airings.json                every airing, keyed by public id
//	├── strands/
//	│   └── tech-news/
//	│       ├── cover.jpg
//	│       └── cover_thumb.jpg
//	└── alice/
//	    └── follows.json            alice's follows (may be absent)

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const (
	strandsFile = "strands.json"
	airingsFile = "airings.json"
	followsFile = "follows.json"
	strandsDir  = "strands"
)

func (s *Store) strandDir(id string) string { return filepath.Join(s.root, strandsDir, id) }

// --- the canon ---

func (s *Store) readStrands() ([]store.Strand, error) {
	var out []store.Strand
	err := readJSON(filepath.Join(s.root, strandsFile), &out)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	return out, nil
}

func (s *Store) writeStrands(all []store.Strand) error {
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return writeJSON(filepath.Join(s.root, strandsFile), all)
}

func (s *Store) PutStrand(_ context.Context, st store.Strand) error {
	all, err := s.readStrands()
	if err != nil {
		return err
	}
	for i, have := range all {
		if have.ID == st.ID {
			all[i] = st
			return s.writeStrands(all)
		}
	}
	return s.writeStrands(append(all, st))
}

func (s *Store) GetStrand(_ context.Context, id string) (store.Strand, error) {
	all, err := s.readStrands()
	if err != nil {
		return store.Strand{}, err
	}
	for _, have := range all {
		if have.ID == id {
			return have, nil
		}
	}
	return store.Strand{}, store.ErrNotFound
}

func (s *Store) ListStrands(_ context.Context) ([]store.Strand, error) {
	return s.readStrands()
}

func (s *Store) DeleteStrand(_ context.Context, id string) error {
	all, err := s.readStrands()
	if err != nil {
		return err
	}
	kept := all[:0]
	for _, have := range all {
		if have.ID != id {
			kept = append(kept, have)
		}
	}
	if len(kept) == len(all) {
		return store.ErrNotFound
	}
	os.RemoveAll(s.strandDir(id))
	return s.writeStrands(kept)
}

func (s *Store) SetStrandCover(ctx context.Context, id, contentType string, full, thumb io.Reader) error {
	st, err := s.GetStrand(ctx, id)
	if err != nil {
		return err
	}
	dir := s.strandDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if st.CoverType != "" && coverFile(st.CoverType) != coverFile(contentType) {
		os.Remove(filepath.Join(dir, coverFile(st.CoverType)))
	}
	if _, err := writeAtomic(filepath.Join(dir, coverFile(contentType)), full); err != nil {
		return err
	}
	if _, err := writeAtomic(filepath.Join(dir, coverThumbFile), thumb); err != nil {
		return err
	}
	st.CoverType = contentType
	return s.PutStrand(ctx, st)
}

func (s *Store) OpenStrandCover(ctx context.Context, id string) (io.ReadCloser, string, error) {
	st, err := s.GetStrand(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if st.CoverType == "" {
		return nil, "", store.ErrNotFound
	}
	return openCover(filepath.Join(s.strandDir(id), coverFile(st.CoverType)), st.CoverType)
}

func (s *Store) OpenStrandCoverThumb(ctx context.Context, id string) (io.ReadCloser, string, error) {
	st, err := s.GetStrand(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if st.CoverType == "" {
		return nil, "", store.ErrNotFound
	}
	return openCover(filepath.Join(s.strandDir(id), coverThumbFile), "image/jpeg")
}

func openCover(path, contentType string) (io.ReadCloser, string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", store.ErrNotFound
		}
		return nil, "", err
	}
	return f, contentType, nil
}

// --- airings ---

func (s *Store) readAirings() ([]store.Airing, error) {
	var out []store.Airing
	err := readJSON(filepath.Join(s.root, airingsFile), &out)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	return out, nil
}

func (s *Store) writeAirings(all []store.Airing) error {
	return writeJSON(filepath.Join(s.root, airingsFile), all)
}

func (s *Store) PutAiring(_ context.Context, a store.Airing) error {
	all, err := s.readAirings()
	if err != nil {
		return err
	}
	for i, have := range all {
		if have.ID == a.ID {
			all[i] = a
			return s.writeAirings(all)
		}
	}
	return s.writeAirings(append(all, a))
}

func (s *Store) GetAiring(_ context.Context, id string) (store.Airing, error) {
	all, err := s.readAirings()
	if err != nil {
		return store.Airing{}, err
	}
	for _, have := range all {
		if have.ID == id {
			return have, nil
		}
	}
	return store.Airing{}, store.ErrNotFound
}

func (s *Store) GetAiringByEpisode(_ context.Context, ownerID, slug string) (store.Airing, error) {
	all, err := s.readAirings()
	if err != nil {
		return store.Airing{}, err
	}
	for _, have := range all {
		if have.OwnerID == ownerID && have.Slug == slug {
			return have, nil
		}
	}
	return store.Airing{}, store.ErrNotFound
}

func (s *Store) DeleteAiring(ctx context.Context, id string) error {
	all, err := s.readAirings()
	if err != nil {
		return err
	}
	kept := all[:0]
	for _, have := range all {
		if have.ID != id {
			kept = append(kept, have)
		}
	}
	if len(kept) == len(all) {
		return store.ErrNotFound
	}
	return s.writeAirings(kept)
}

func (s *Store) ListAirings(_ context.Context, strand string) ([]store.Airing, error) {
	return s.listAirings(func(a store.Airing) bool { return a.Strand == strand })
}

func (s *Store) ListAiringsByOwner(_ context.Context, ownerID string) ([]store.Airing, error) {
	return s.listAirings(func(a store.Airing) bool { return a.OwnerID == ownerID })
}

func (s *Store) listAirings(keep func(store.Airing) bool) ([]store.Airing, error) {
	all, err := s.readAirings()
	if err != nil {
		return nil, err
	}
	var out []store.Airing
	for _, a := range all {
		if keep(a) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AiredAt.After(out[j].AiredAt) })
	return out, nil
}

// removeAirings drops every airing matching drop. It is how an Owner's
// delete reaches the public surface (ADR 0006: the delete propagates
// everywhere, with no tombstone).
func (s *Store) removeAirings(drop func(store.Airing) bool) error {
	all, err := s.readAirings()
	if err != nil {
		return err
	}
	kept := all[:0]
	for _, a := range all {
		if !drop(a) {
			kept = append(kept, a)
		}
	}
	if len(kept) == len(all) {
		return nil
	}
	return s.writeAirings(kept)
}

// --- follows ---

func (s *Store) readFollows(userID string) ([]store.Follow, error) {
	var out []store.Follow
	err := readJSON(filepath.Join(s.userDir(userID), followsFile), &out)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	for i := range out {
		out[i].UserID = userID
	}
	return out, nil
}

func (s *Store) writeFollows(userID string, all []store.Follow) error {
	sort.Slice(all, func(i, j int) bool { return all[i].Strand < all[j].Strand })
	return writeJSON(filepath.Join(s.userDir(userID), followsFile), all)
}

func (s *Store) PutFollow(ctx context.Context, f store.Follow) error {
	if _, err := s.GetUser(ctx, f.UserID); err != nil {
		return err
	}
	all, err := s.readFollows(f.UserID)
	if err != nil {
		return err
	}
	for i, have := range all {
		if have.Strand == f.Strand {
			all[i] = f
			return s.writeFollows(f.UserID, all)
		}
	}
	return s.writeFollows(f.UserID, append(all, f))
}

func (s *Store) DeleteFollow(_ context.Context, userID, strand string) error {
	all, err := s.readFollows(userID)
	if err != nil {
		return err
	}
	kept := all[:0]
	for _, have := range all {
		if have.Strand != strand {
			kept = append(kept, have)
		}
	}
	if len(kept) == len(all) {
		return store.ErrNotFound
	}
	return s.writeFollows(userID, kept)
}

func (s *Store) ListFollows(ctx context.Context, userID string) ([]store.Follow, error) {
	if _, err := s.GetUser(ctx, userID); err != nil {
		return nil, err
	}
	return s.readFollows(userID)
}
