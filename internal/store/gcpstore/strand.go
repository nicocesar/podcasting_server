package gcpstore

// The public side in Datastore and GCS. Every listing here filters on
// equality alone and sorts in Go, the same way ListEpisodes does, so
// none of it needs a composite index declared before it will run.

import (
	"context"
	"errors"
	"io"
	"sort"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/storage"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const (
	kindStrand = "Strand"
	kindAiring = "Airing"
	kindVouch  = "Vouch"
	kindFollow = "Follow"
)

func strandKey(id string) *datastore.Key { return datastore.NameKey(kindStrand, id, nil) }
func airingKey(id string) *datastore.Key { return datastore.NameKey(kindAiring, id, nil) }

func vouchKey(airingID, userID string) *datastore.Key {
	return datastore.NameKey(kindVouch, airingID+"/"+userID, nil)
}

func followKey(userID, strand string) *datastore.Key {
	return datastore.NameKey(kindFollow, userID+"/"+strand, nil)
}

func strandCoverObject(id string) string      { return "strands/" + id + "/cover" }
func strandCoverThumbObject(id string) string { return "strands/" + id + "/cover_thumb" }

// --- the canon ---

func (s *Store) PutStrand(ctx context.Context, st store.Strand) error {
	_, err := s.ds.Put(ctx, strandKey(st.ID), &st)
	return err
}

func (s *Store) GetStrand(ctx context.Context, id string) (store.Strand, error) {
	var st store.Strand
	if err := s.ds.Get(ctx, strandKey(id), &st); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.Strand{}, store.ErrNotFound
		}
		return store.Strand{}, err
	}
	st.ID = id
	return st, nil
}

func (s *Store) ListStrands(ctx context.Context) ([]store.Strand, error) {
	var out []store.Strand
	keys, err := s.ds.GetAll(ctx, datastore.NewQuery(kindStrand), &out)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		out[i].ID = k.Name
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if out == nil {
		out = []store.Strand{}
	}
	return out, nil
}

func (s *Store) DeleteStrand(ctx context.Context, id string) error {
	if _, err := s.GetStrand(ctx, id); err != nil {
		return err
	}
	if err := s.ds.Delete(ctx, strandKey(id)); err != nil {
		return err
	}
	for _, object := range []string{strandCoverObject(id), strandCoverThumbObject(id)} {
		if err := s.bucket.Object(object).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) SetStrandCover(ctx context.Context, id, contentType string, full, thumb io.Reader) error {
	st, err := s.GetStrand(ctx, id)
	if err != nil {
		return err
	}
	if err := s.writeObject(ctx, strandCoverObject(id), contentType, full); err != nil {
		return err
	}
	if err := s.writeObject(ctx, strandCoverThumbObject(id), "image/jpeg", thumb); err != nil {
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
	return s.openStrandObject(ctx, strandCoverObject(id), st.CoverType)
}

func (s *Store) OpenStrandCoverThumb(ctx context.Context, id string) (io.ReadCloser, string, error) {
	st, err := s.GetStrand(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if st.CoverType == "" {
		return nil, "", store.ErrNotFound
	}
	return s.openStrandObject(ctx, strandCoverThumbObject(id), "image/jpeg")
}

func (s *Store) openStrandObject(ctx context.Context, object, contentType string) (io.ReadCloser, string, error) {
	r, err := s.bucket.Object(object).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, "", store.ErrNotFound
		}
		return nil, "", err
	}
	return r, contentType, nil
}

// --- airings ---

func (s *Store) PutAiring(ctx context.Context, a store.Airing) error {
	_, err := s.ds.Put(ctx, airingKey(a.ID), &a)
	return err
}

func (s *Store) GetAiring(ctx context.Context, id string) (store.Airing, error) {
	var a store.Airing
	if err := s.ds.Get(ctx, airingKey(id), &a); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.Airing{}, store.ErrNotFound
		}
		return store.Airing{}, err
	}
	a.ID = id
	return a, nil
}

func (s *Store) GetAiringByEpisode(ctx context.Context, ownerID, slug string) (store.Airing, error) {
	airings, err := s.ListAiringsByOwner(ctx, ownerID)
	if err != nil {
		return store.Airing{}, err
	}
	for _, a := range airings {
		if a.Slug == slug {
			return a, nil
		}
	}
	return store.Airing{}, store.ErrNotFound
}

func (s *Store) DeleteAiring(ctx context.Context, id string) error {
	if _, err := s.GetAiring(ctx, id); err != nil {
		return err
	}
	if err := s.ds.Delete(ctx, airingKey(id)); err != nil {
		return err
	}
	return s.deleteQuery(ctx, datastore.NewQuery(kindVouch).FilterField("airing_id", "=", id).KeysOnly())
}

func (s *Store) ListAirings(ctx context.Context, strand string) ([]store.Airing, error) {
	return s.listAirings(ctx, datastore.NewQuery(kindAiring).FilterField("strand", "=", strand))
}

func (s *Store) ListAiringsByOwner(ctx context.Context, ownerID string) ([]store.Airing, error) {
	return s.listAirings(ctx, datastore.NewQuery(kindAiring).FilterField("owner_id", "=", ownerID))
}

func (s *Store) listAirings(ctx context.Context, q *datastore.Query) ([]store.Airing, error) {
	var out []store.Airing
	keys, err := s.ds.GetAll(ctx, q, &out)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		out[i].ID = k.Name
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AiredAt.After(out[j].AiredAt) })
	if out == nil {
		out = []store.Airing{}
	}
	return out, nil
}

// --- vouches ---

func (s *Store) AddVouch(ctx context.Context, v store.Vouch) error {
	_, err := s.ds.Put(ctx, vouchKey(v.AiringID, v.UserID), &v)
	return err
}

func (s *Store) RemoveVouch(ctx context.Context, airingID, userID string) error {
	key := vouchKey(airingID, userID)
	var v store.Vouch
	if err := s.ds.Get(ctx, key, &v); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.ErrNotFound
		}
		return err
	}
	return s.ds.Delete(ctx, key)
}

func (s *Store) ListVouches(ctx context.Context, airingID string) ([]store.Vouch, error) {
	var out []store.Vouch
	q := datastore.NewQuery(kindVouch).FilterField("airing_id", "=", airingID)
	if _, err := s.ds.GetAll(ctx, q, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if out == nil {
		out = []store.Vouch{}
	}
	return out, nil
}

// --- follows ---

func (s *Store) PutFollow(ctx context.Context, f store.Follow) error {
	if _, err := s.GetUser(ctx, f.UserID); err != nil {
		return err
	}
	_, err := s.ds.Put(ctx, followKey(f.UserID, f.Strand), &f)
	return err
}

func (s *Store) DeleteFollow(ctx context.Context, userID, strand string) error {
	key := followKey(userID, strand)
	var f store.Follow
	if err := s.ds.Get(ctx, key, &f); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.ErrNotFound
		}
		return err
	}
	return s.ds.Delete(ctx, key)
}

func (s *Store) ListFollows(ctx context.Context, userID string) ([]store.Follow, error) {
	if _, err := s.GetUser(ctx, userID); err != nil {
		return nil, err
	}
	var out []store.Follow
	q := datastore.NewQuery(kindFollow).FilterField("user_id", "=", userID)
	if _, err := s.ds.GetAll(ctx, q, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Strand < out[j].Strand })
	if out == nil {
		out = []store.Follow{}
	}
	return out, nil
}

// deleteQuery removes every entity a keys-only query matches.
func (s *Store) deleteQuery(ctx context.Context, q *datastore.Query) error {
	keys, err := s.ds.GetAll(ctx, q, nil)
	if err != nil {
		return err
	}
	return s.deleteKeys(ctx, keys)
}
