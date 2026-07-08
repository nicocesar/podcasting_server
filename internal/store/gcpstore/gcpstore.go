// Package gcpstore is the production storage backend: user, episode, and
// share metadata in Datastore (Firestore in Datastore mode), audio and
// cover bytes in a GCS bucket, audio served via short-lived V4 signed URLs.
//
// Datastore layout: kind "User" keyed by user ID with an indexed
// cover_secret; kind "Episode" keyed by "{ownerID}/{slug}" with an indexed
// owner_id; kind "Share" keyed by "{userID}/{ownerID}/{slug}" with indexed
// user_id, owner_id, and slug. All queries are equality-only, so no
// composite indexes are required; sorting happens in memory.
//
// GCS layout: users/{ownerID}/{slug}.mp3 and users/{ownerID}/cover.
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
	kindUser     = "User"
	kindEpisode  = "Episode"
	kindShare    = "Share"
	kindInvite   = "Invite"
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

func userKey(id string) *datastore.Key { return datastore.NameKey(kindUser, id, nil) }

func episodeKey(ownerID, slug string) *datastore.Key {
	return datastore.NameKey(kindEpisode, ownerID+"/"+slug, nil)
}

func shareKey(userID, ownerID, slug string) *datastore.Key {
	return datastore.NameKey(kindShare, userID+"/"+ownerID+"/"+slug, nil)
}

func inviteKey(token string) *datastore.Key { return datastore.NameKey(kindInvite, token, nil) }

func audioObject(ownerID, slug string) string { return "users/" + ownerID + "/" + slug + ".mp3" }
func coverObject(ownerID string) string       { return "users/" + ownerID + "/cover" }

// --- users ---

func (s *Store) UpsertUser(ctx context.Context, u store.User) error {
	u.UpdatedAt = time.Now().UTC()
	id := u.ID
	_, err := s.ds.Put(ctx, userKey(id), &u)
	return err
}

// ignoreFieldMismatch drops datastore.ErrFieldMismatch: entities written
// before a schema change may carry properties the User struct no longer
// has (e.g. read_hash/cover_secret from before ADR 0008); they are noise,
// not errors.
func ignoreFieldMismatch(err error) error {
	var fm *datastore.ErrFieldMismatch
	if errors.As(err, &fm) {
		return nil
	}
	return err
}

func (s *Store) GetUser(ctx context.Context, id string) (store.User, error) {
	var u store.User
	if err := ignoreFieldMismatch(s.ds.Get(ctx, userKey(id), &u)); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.User{}, store.ErrNotFound
		}
		return store.User{}, err
	}
	u.ID = id
	return u, nil
}

func (s *Store) GetUserByFeedToken(ctx context.Context, token string) (store.User, error) {
	if token == "" {
		return store.User{}, store.ErrNotFound
	}
	var users []store.User
	q := datastore.NewQuery(kindUser).FilterField("feed_token", "=", token).Limit(1)
	keys, err := s.ds.GetAll(ctx, q, &users)
	if err = ignoreFieldMismatch(err); err != nil {
		return store.User{}, err
	}
	if len(users) == 0 {
		return store.User{}, store.ErrNotFound
	}
	users[0].ID = keys[0].Name
	return users[0], nil
}

func (s *Store) ListUsers(ctx context.Context) ([]store.User, error) {
	var users []store.User
	keys, err := s.ds.GetAll(ctx, datastore.NewQuery(kindUser), &users)
	if err = ignoreFieldMismatch(err); err != nil {
		return nil, err
	}
	for i, k := range keys {
		users[i].ID = k.Name
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	if users == nil {
		users = []store.User{}
	}
	return users, nil
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	if _, err := s.GetUser(ctx, id); err != nil {
		return err
	}
	// Episode entities, shares in their feed, shares of their episodes
	// in other feeds (the owner's delete propagates; ADR 0006), and the
	// invites they minted (ADR 0007).
	for _, q := range []*datastore.Query{
		datastore.NewQuery(kindEpisode).FilterField("owner_id", "=", id).KeysOnly(),
		datastore.NewQuery(kindShare).FilterField("user_id", "=", id).KeysOnly(),
		datastore.NewQuery(kindShare).FilterField("owner_id", "=", id).KeysOnly(),
		datastore.NewQuery(kindInvite).FilterField("inviter_id", "=", id).KeysOnly(),
	} {
		keys, err := s.ds.GetAll(ctx, q, nil)
		if err != nil {
			return err
		}
		if err := s.deleteKeys(ctx, keys); err != nil {
			return err
		}
	}
	if err := s.ds.Delete(ctx, userKey(id)); err != nil {
		return err
	}
	// Audio and cover objects.
	it := s.bucket.Objects(ctx, &storage.Query{Prefix: "users/" + id + "/"})
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

func (s *Store) deleteKeys(ctx context.Context, keys []*datastore.Key) error {
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
	return nil
}

// --- episodes ---

func (s *Store) UpsertEpisode(ctx context.Context, ep store.Episode, audio io.Reader) (store.Episode, error) {
	if _, err := s.GetUser(ctx, ep.OwnerID); err != nil {
		return store.Episode{}, err
	}
	if ep.AudioType == "" {
		ep.AudioType = "audio/mpeg"
	}
	w := s.bucket.Object(audioObject(ep.OwnerID, ep.Slug)).NewWriter(ctx)
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
	if _, err := s.ds.Put(ctx, episodeKey(ep.OwnerID, ep.Slug), &ep); err != nil {
		return store.Episode{}, err
	}
	return ep, nil
}

func (s *Store) GetEpisode(ctx context.Context, ownerID, slug string) (store.Episode, error) {
	var ep store.Episode
	if err := s.ds.Get(ctx, episodeKey(ownerID, slug), &ep); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.Episode{}, store.ErrNotFound
		}
		return store.Episode{}, err
	}
	ep.OwnerID, ep.Slug = ownerID, slug
	return ep, nil
}

func (s *Store) ListEpisodes(ctx context.Context, ownerID string) ([]store.Episode, error) {
	if _, err := s.GetUser(ctx, ownerID); err != nil {
		return nil, err
	}
	var eps []store.Episode
	q := datastore.NewQuery(kindEpisode).FilterField("owner_id", "=", ownerID)
	keys, err := s.ds.GetAll(ctx, q, &eps)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		eps[i].Slug = strings.TrimPrefix(k.Name, ownerID+"/")
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

func (s *Store) DeleteEpisode(ctx context.Context, ownerID, slug string) error {
	if _, err := s.GetEpisode(ctx, ownerID, slug); err != nil {
		return err
	}
	if err := s.ds.Delete(ctx, episodeKey(ownerID, slug)); err != nil {
		return err
	}
	// The owner's delete propagates to every referencing feed (ADR 0006).
	q := datastore.NewQuery(kindShare).
		FilterField("owner_id", "=", ownerID).
		FilterField("slug", "=", slug).
		KeysOnly()
	keys, err := s.ds.GetAll(ctx, q, nil)
	if err != nil {
		return err
	}
	if err := s.deleteKeys(ctx, keys); err != nil {
		return err
	}
	err = s.bucket.Object(audioObject(ownerID, slug)).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return err
	}
	return nil
}

// --- shares ---

func (s *Store) AddShare(ctx context.Context, sh store.Share) error {
	if _, err := s.GetUser(ctx, sh.UserID); err != nil {
		return err
	}
	if _, err := s.GetEpisode(ctx, sh.OwnerID, sh.Slug); err != nil {
		return err
	}
	key := shareKey(sh.UserID, sh.OwnerID, sh.Slug)
	// Insert-if-absent: the first Sharer is kept.
	_, err := s.ds.RunInTransaction(ctx, func(tx *datastore.Transaction) error {
		var have store.Share
		err := tx.Get(key, &have)
		if err == nil {
			return nil
		}
		if !errors.Is(err, datastore.ErrNoSuchEntity) {
			return err
		}
		_, err = tx.Put(key, &sh)
		return err
	})
	return err
}

func (s *Store) GetShare(ctx context.Context, userID, ownerID, slug string) (store.Share, error) {
	var sh store.Share
	if err := s.ds.Get(ctx, shareKey(userID, ownerID, slug), &sh); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.Share{}, store.ErrNotFound
		}
		return store.Share{}, err
	}
	return sh, nil
}

func (s *Store) RemoveShare(ctx context.Context, userID, ownerID, slug string) error {
	if _, err := s.GetShare(ctx, userID, ownerID, slug); err != nil {
		return err
	}
	return s.ds.Delete(ctx, shareKey(userID, ownerID, slug))
}

func (s *Store) ListShares(ctx context.Context, userID string) ([]store.Share, error) {
	if _, err := s.GetUser(ctx, userID); err != nil {
		return nil, err
	}
	var shares []store.Share
	q := datastore.NewQuery(kindShare).FilterField("user_id", "=", userID)
	if _, err := s.ds.GetAll(ctx, q, &shares); err != nil {
		return nil, err
	}
	if shares == nil {
		shares = []store.Share{}
	}
	return shares, nil
}

// --- invites ---

func (s *Store) AddInvite(ctx context.Context, inv store.Invite) error {
	token := inv.Token
	_, err := s.ds.Put(ctx, inviteKey(token), &inv)
	return err
}

func (s *Store) GetInvite(ctx context.Context, token string) (store.Invite, error) {
	var inv store.Invite
	if err := s.ds.Get(ctx, inviteKey(token), &inv); err != nil {
		if errors.Is(err, datastore.ErrNoSuchEntity) {
			return store.Invite{}, store.ErrNotFound
		}
		return store.Invite{}, err
	}
	inv.Token = token
	return inv, nil
}

func (s *Store) ListInvites(ctx context.Context, inviterID string) ([]store.Invite, error) {
	var invs []store.Invite
	q := datastore.NewQuery(kindInvite).FilterField("inviter_id", "=", inviterID)
	keys, err := s.ds.GetAll(ctx, q, &invs)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		invs[i].Token = k.Name
	}
	sort.Slice(invs, func(i, j int) bool { return invs[i].CreatedAt.After(invs[j].CreatedAt) })
	if invs == nil {
		invs = []store.Invite{}
	}
	return invs, nil
}

func (s *Store) DeleteInvite(ctx context.Context, token string) error {
	if _, err := s.GetInvite(ctx, token); err != nil {
		return err
	}
	return s.ds.Delete(ctx, inviteKey(token))
}

// RedeemInvite claims the invite in a transaction, enforcing single use.
func (s *Store) RedeemInvite(ctx context.Context, token, userID string) error {
	_, err := s.ds.RunInTransaction(ctx, func(tx *datastore.Transaction) error {
		key := inviteKey(token)
		var inv store.Invite
		if err := tx.Get(key, &inv); err != nil {
			if errors.Is(err, datastore.ErrNoSuchEntity) {
				return store.ErrNotFound
			}
			return err
		}
		if inv.RedeemedBy != "" {
			return store.ErrNotFound
		}
		inv.RedeemedBy = userID
		_, err := tx.Put(key, &inv)
		return err
	})
	return err
}

// --- audio & cover ---

// OpenAudio returns a short-lived V4 signed URL. On Cloud Run the SDK
// signs via the IAM signBlob API, which requires the service account to
// hold roles/iam.serviceAccountTokenCreator on itself (see SETUP.md).
func (s *Store) OpenAudio(ctx context.Context, ownerID, slug string) (store.Audio, error) {
	ep, err := s.GetEpisode(ctx, ownerID, slug)
	if err != nil {
		return store.Audio{}, err
	}
	u, err := s.bucket.SignedURL(audioObject(ownerID, slug), &storage.SignedURLOptions{
		Scheme:  storage.SigningSchemeV4,
		Method:  http.MethodGet,
		Expires: time.Now().Add(signedURLTTL),
	})
	if err != nil {
		return store.Audio{}, err
	}
	return store.Audio{RedirectURL: u, Size: ep.AudioSize, ContentType: ep.AudioType}, nil
}

func (s *Store) SetCover(ctx context.Context, userID, contentType string, r io.Reader) error {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	w := s.bucket.Object(coverObject(userID)).NewWriter(ctx)
	w.ContentType = contentType
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	u.CoverType = contentType
	return s.UpsertUser(ctx, u)
}

func (s *Store) OpenCover(ctx context.Context, userID string) (io.ReadCloser, string, error) {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	if u.CoverType == "" {
		return nil, "", store.ErrNotFound
	}
	r, err := s.bucket.Object(coverObject(userID)).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, "", store.ErrNotFound
		}
		return nil, "", err
	}
	return r, u.CoverType, nil
}
