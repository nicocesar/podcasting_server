// Package gcpstore is the production storage backend: episode and show
// metadata in Datastore (Firestore in Datastore mode), audio and cover
// bytes in a GCS bucket, audio served via short-lived V4 signed URLs.
//
// Datastore layout: kind "Show" keyed by show ID; kind "Episode" keyed by
// "{showID}/{slug}" with an indexed show_id field. Episodes are filtered
// by show_id and sorted in memory, so no composite indexes are required.
//
// GCS layout: shows/{showID}/{slug}.mp3 and shows/{showID}/cover.
package gcpstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const (
	kindShow    = "Show"
	kindEpisode = "Episode"
	signedURLTTL = 15 * time.Minute
)

type Store struct {
	ds     *datastore.Client
	bucket *storage.BucketHandle
}

// New connects to Datastore and GCS using application default credentials.
// projectID may be empty to auto-detect (metadata server on Cloud Run).
func New(ctx context.Context, projectID, bucket string) (*Store, error) {
	if projectID == "" {
		projectID = datastore.DetectProjectID
	}
	ds, err := datastore.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("gcpstore: datastore: %w", err)
	}
	gcs, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcpstore: storage: %w", err)
	}
	return &Store{ds: ds, bucket: gcs.Bucket(bucket)}, nil
}

func showKey(id string) *datastore.Key { return datastore.NameKey(kindShow, id, nil) }

func episodeKey(showID, slug string) *datastore.Key {
	return datastore.NameKey(kindEpisode, showID+"/"+slug, nil)
}

func audioObject(showID, slug string) string { return "shows/" + showID + "/" + slug + ".mp3" }
func coverObject(showID string) string       { return "shows/" + showID + "/cover" }

func (s *Store) UpsertShow(ctx context.Context, sh store.Show) error {
	sh.UpdatedAt = time.Now().UTC()
	id := sh.ID
	_, err := s.ds.Put(ctx, showKey(id), &sh)
	return err
}

func (s *Store) GetShow(ctx context.Context, id string) (store.Show, error) {
	var sh store.Show
	if err := s.ds.Get(ctx, showKey(id), &sh); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.Show{}, store.ErrNotFound
		}
		return store.Show{}, err
	}
	sh.ID = id
	return sh, nil
}

func (s *Store) ListShows(ctx context.Context) ([]store.Show, error) {
	var shows []store.Show
	keys, err := s.ds.GetAll(ctx, datastore.NewQuery(kindShow), &shows)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		shows[i].ID = k.Name
	}
	sort.Slice(shows, func(i, j int) bool { return shows[i].ID < shows[j].ID })
	if shows == nil {
		shows = []store.Show{}
	}
	return shows, nil
}

func (s *Store) DeleteShow(ctx context.Context, id string) error {
	if _, err := s.GetShow(ctx, id); err != nil {
		return err
	}
	// Episode entities.
	q := datastore.NewQuery(kindEpisode).FilterField("show_id", "=", id).KeysOnly()
	keys, err := s.ds.GetAll(ctx, q, nil)
	if err != nil {
		return err
	}
	for len(keys) > 0 {
		batch := keys
		if len(batch) > 500 {
			batch = keys[:500]
		}
		if err := s.ds.DeleteMulti(ctx, batch); err != nil {
			return err
		}
		keys = keys[len(batch):]
	}
	if err := s.ds.Delete(ctx, showKey(id)); err != nil {
		return err
	}
	// Audio and cover objects.
	it := s.bucket.Objects(ctx, &storage.Query{Prefix: "shows/" + id + "/"})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.bucket.Object(attrs.Name).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			return err
		}
	}
}

func (s *Store) UpsertEpisode(ctx context.Context, ep store.Episode, audio io.Reader) (store.Episode, error) {
	if _, err := s.GetShow(ctx, ep.ShowID); err != nil {
		return store.Episode{}, err
	}
	if ep.AudioType == "" {
		ep.AudioType = "audio/mpeg"
	}
	w := s.bucket.Object(audioObject(ep.ShowID, ep.Slug)).NewWriter(ctx)
	w.ContentType = ep.AudioType
	n, err := io.Copy(w, audio)
	if err != nil {
		w.Close()
		return store.Episode{}, err
	}
	if err := w.Close(); err != nil {
		return store.Episode{}, err
	}
	ep.AudioSize = n
	if _, err := s.ds.Put(ctx, episodeKey(ep.ShowID, ep.Slug), &ep); err != nil {
		return store.Episode{}, err
	}
	return ep, nil
}

func (s *Store) GetEpisode(ctx context.Context, showID, slug string) (store.Episode, error) {
	var ep store.Episode
	if err := s.ds.Get(ctx, episodeKey(showID, slug), &ep); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.Episode{}, store.ErrNotFound
		}
		return store.Episode{}, err
	}
	ep.ShowID, ep.Slug = showID, slug
	return ep, nil
}

func (s *Store) ListEpisodes(ctx context.Context, showID string) ([]store.Episode, error) {
	if _, err := s.GetShow(ctx, showID); err != nil {
		return nil, err
	}
	var eps []store.Episode
	q := datastore.NewQuery(kindEpisode).FilterField("show_id", "=", showID)
	keys, err := s.ds.GetAll(ctx, q, &eps)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		eps[i].Slug = strings.TrimPrefix(k.Name, showID+"/")
	}
	sort.Slice(eps, func(i, j int) bool {
		if !eps[i].PublishedAt.Equal(eps[j].PublishedAt) {
			return eps[i].PublishedAt.After(eps[j].PublishedAt)
		}
		return eps[i].Slug > eps[j].Slug
	})
	if eps == nil {
		eps = []store.Episode{}
	}
	return eps, nil
}

func (s *Store) DeleteEpisode(ctx context.Context, showID, slug string) error {
	if _, err := s.GetEpisode(ctx, showID, slug); err != nil {
		return err
	}
	if err := s.ds.Delete(ctx, episodeKey(showID, slug)); err != nil {
		return err
	}
	err := s.bucket.Object(audioObject(showID, slug)).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return err
	}
	return nil
}

// OpenAudio returns a short-lived V4 signed URL. On Cloud Run the SDK
// signs via the IAM signBlob API, which requires the service account to
// hold roles/iam.serviceAccountTokenCreator on itself (see SETUP.md).
func (s *Store) OpenAudio(ctx context.Context, showID, slug string) (store.Audio, error) {
	ep, err := s.GetEpisode(ctx, showID, slug)
	if err != nil {
		return store.Audio{}, err
	}
	u, err := s.bucket.SignedURL(audioObject(showID, slug), &storage.SignedURLOptions{
		Scheme:  storage.SigningSchemeV4,
		Method:  http.MethodGet,
		Expires: time.Now().Add(signedURLTTL),
	})
	if err != nil {
		return store.Audio{}, err
	}
	return store.Audio{RedirectURL: u, Size: ep.AudioSize, ContentType: ep.AudioType}, nil
}

func (s *Store) SetCover(ctx context.Context, showID, contentType string, r io.Reader) error {
	sh, err := s.GetShow(ctx, showID)
	if err != nil {
		return err
	}
	w := s.bucket.Object(coverObject(showID)).NewWriter(ctx)
	w.ContentType = contentType
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
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
	r, err := s.bucket.Object(coverObject(showID)).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, "", store.ErrNotFound
		}
		return nil, "", err
	}
	return r, sh.CoverType, nil
}
