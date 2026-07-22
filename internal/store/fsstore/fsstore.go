// Package fsstore is the local-development storage backend: a plain
// directory tree that is read live, so episodes can be added or edited
// with a text editor and show up on the next feed request.
//
// Layout:
//
//	root/
//	├── invites.json                all invites, keyed by token
//	├── alice/                      user ID
//	│   ├── user.json               user + feed metadata
//	│   ├── shares.json             shares in alice's feed (may be absent)
//	│   ├── cover.jpg               cover art (name depends on type)
//	│   ├── 2026-07-06-morning.mp3
//	│   └── 2026-07-06-morning.json episode metadata sidecar
//	└── bob/
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

const (
	userFile    = "user.json"
	sharesFile  = "shares.json"
	invitesFile = "invites.json"
	apiKeysFile = "apikeys.json"
)

type Store struct {
	root string
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("fsstore: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) userDir(id string) string { return filepath.Join(s.root, id) }

// --- users ---

func (s *Store) UpsertUser(_ context.Context, u store.User) error {
	dir := s.userDir(u.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	u.UpdatedAt = time.Now().UTC()
	return writeJSON(filepath.Join(dir, userFile), newUserRecord(u))
}

// userRecord persists the fields User hides from API JSON (the secrets
// carry json:"-" so they never leak through handlers).
type userRecord struct {
	store.User
	FeedToken         string `json:"feed_token"`
	PasswordHash      string `json:"password_hash,omitempty"`
	GoogleSub         string `json:"google_sub,omitempty"`
	GoogleEmail       string `json:"google_email,omitempty"`
	CredentialVersion int64  `json:"credential_version,omitempty"`
}

func (r userRecord) user(id string) store.User {
	u := r.User
	u.ID = id // directory name is canonical
	u.FeedToken = r.FeedToken
	u.PasswordHash = r.PasswordHash
	u.GoogleSub = r.GoogleSub
	u.GoogleEmail = r.GoogleEmail
	u.CredentialVersion = r.CredentialVersion
	return u
}

func newUserRecord(u store.User) userRecord {
	return userRecord{
		User:              u,
		FeedToken:         u.FeedToken,
		PasswordHash:      u.PasswordHash,
		GoogleSub:         u.GoogleSub,
		GoogleEmail:       u.GoogleEmail,
		CredentialVersion: u.CredentialVersion,
	}
}

func (s *Store) GetUser(_ context.Context, id string) (store.User, error) {
	var r userRecord
	if err := readJSON(filepath.Join(s.userDir(id), userFile), &r); err != nil {
		return store.User{}, err
	}
	return r.user(id), nil
}

func (s *Store) GetUserByFeedToken(ctx context.Context, token string) (store.User, error) {
	if token == "" {
		return store.User{}, store.ErrNotFound
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	for _, u := range users {
		if u.FeedToken == token {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (s *Store) GetUserByGoogleSub(ctx context.Context, sub string) (store.User, error) {
	if sub == "" {
		return store.User{}, store.ErrNotFound
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	for _, u := range users {
		if u.GoogleSub == sub {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (s *Store) ListUsers(ctx context.Context) ([]store.User, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	users := []store.User{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		u, err := s.GetUser(ctx, e.Name())
		if err != nil {
			continue // directory without user.json: not a user
		}
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	return users, nil
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	if _, err := s.GetUser(ctx, id); err != nil {
		return err
	}
	// Their episodes disappear from every other feed (ADR 0006), and
	// their pending invites stop being doors into the system.
	if err := s.removeShares(ctx, func(sh store.Share) bool { return sh.OwnerID == id }); err != nil {
		return err
	}
	if err := s.removeInvites(func(inv store.Invite) bool { return inv.InviterID == id }); err != nil && err != store.ErrNotFound {
		return err
	}
	if err := s.removeAPIKeys(func(k store.APIKey) bool { return k.UserID == id }); err != nil && err != store.ErrNotFound {
		return err
	}
	return os.RemoveAll(s.userDir(id))
}

// --- api keys ---

// apiKeyRecord persists the fields APIKey hides from API JSON (owner and
// secret hash carry json:"-" so they never leak through handlers).
type apiKeyRecord struct {
	store.APIKey
	UserID     string `json:"user_id"`
	SecretHash string `json:"secret_hash"`
}

// API keys live in one root-level file like invites: revocation and
// lookup by KeyID need the whole set anyway.
func (s *Store) readAPIKeys() ([]store.APIKey, error) {
	var recs []apiKeyRecord
	err := readJSON(filepath.Join(s.root, apiKeysFile), &recs)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	keys := make([]store.APIKey, len(recs))
	for i, r := range recs {
		keys[i] = r.APIKey
		keys[i].UserID = r.UserID
		keys[i].SecretHash = r.SecretHash
	}
	return keys, nil
}

func (s *Store) writeAPIKeys(keys []store.APIKey) error {
	recs := make([]apiKeyRecord, len(keys))
	for i, k := range keys {
		recs[i] = apiKeyRecord{APIKey: k, UserID: k.UserID, SecretHash: k.SecretHash}
	}
	return writeJSON(filepath.Join(s.root, apiKeysFile), recs)
}

func (s *Store) PutAPIKey(ctx context.Context, k store.APIKey) error {
	if _, err := s.GetUser(ctx, k.UserID); err != nil {
		return err
	}
	keys, err := s.readAPIKeys()
	if err != nil {
		return err
	}
	for _, have := range keys {
		if have.KeyID == k.KeyID {
			return fmt.Errorf("fsstore: duplicate api key id")
		}
	}
	return s.writeAPIKeys(append(keys, k))
}

func (s *Store) GetAPIKey(_ context.Context, keyID string) (store.APIKey, error) {
	keys, err := s.readAPIKeys()
	if err != nil {
		return store.APIKey{}, err
	}
	for _, k := range keys {
		if k.KeyID == keyID {
			return k, nil
		}
	}
	return store.APIKey{}, store.ErrNotFound
}

func (s *Store) ListAPIKeys(ctx context.Context, userID string) ([]store.APIKey, error) {
	if _, err := s.GetUser(ctx, userID); err != nil {
		return nil, err
	}
	keys, err := s.readAPIKeys()
	if err != nil {
		return nil, err
	}
	mine := []store.APIKey{}
	for _, k := range keys {
		if k.UserID == userID {
			mine = append(mine, k)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].CreatedAt.After(mine[j].CreatedAt) })
	return mine, nil
}

func (s *Store) DeleteAPIKey(_ context.Context, keyID string) error {
	return s.removeAPIKeys(func(k store.APIKey) bool { return k.KeyID == keyID })
}

// removeAPIKeys drops every key matching drop; ErrNotFound when none
// matched.
func (s *Store) removeAPIKeys(drop func(store.APIKey) bool) error {
	keys, err := s.readAPIKeys()
	if err != nil {
		return err
	}
	kept := keys[:0]
	for _, k := range keys {
		if !drop(k) {
			kept = append(kept, k)
		}
	}
	if len(kept) == len(keys) {
		return store.ErrNotFound
	}
	return s.writeAPIKeys(kept)
}

// --- episodes ---

func (s *Store) UpsertEpisode(ctx context.Context, ep store.Episode, audio io.Reader) (store.Episode, error) {
	if _, err := s.GetUser(ctx, ep.OwnerID); err != nil {
		return store.Episode{}, err
	}
	dir := s.userDir(ep.OwnerID)
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

func (s *Store) UpdateEpisode(_ context.Context, ep store.Episode) error {
	have, err := s.readEpisode(ep.OwnerID, ep.Slug)
	if err != nil {
		return err
	}
	ep.AudioSize = have.AudioSize // audio is untouched; keep what is on disk
	return writeJSON(filepath.Join(s.userDir(ep.OwnerID), ep.Slug+".json"), ep)
}

func (s *Store) GetEpisode(_ context.Context, ownerID, slug string) (store.Episode, error) {
	return s.readEpisode(ownerID, slug)
}

func (s *Store) readEpisode(ownerID, slug string) (store.Episode, error) {
	path := filepath.Join(s.userDir(ownerID), slug+".json")
	var ep store.Episode
	if err := readJSON(path, &ep); err != nil {
		return store.Episode{}, err
	}
	// File names are canonical; sidecar content may be hand-written and
	// missing fields, so fill in what we can observe.
	ep.OwnerID, ep.Slug = ownerID, slug
	if ep.AudioType == "" {
		ep.AudioType = "audio/mpeg"
	}
	if fi, err := os.Stat(filepath.Join(s.userDir(ownerID), slug+".mp3")); err == nil {
		ep.AudioSize = fi.Size()
		if ep.PublishedAt.IsZero() {
			ep.PublishedAt = fi.ModTime().UTC()
		}
	}
	return ep, nil
}

func (s *Store) ListEpisodes(ctx context.Context, ownerID string) ([]store.Episode, error) {
	if _, err := s.GetUser(ctx, ownerID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.userDir(ownerID))
	if err != nil {
		return nil, err
	}
	eps := []store.Episode{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == userFile || name == sharesFile || !strings.HasSuffix(name, ".json") {
			continue
		}
		ep, err := s.readEpisode(ownerID, strings.TrimSuffix(name, ".json"))
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

func (s *Store) DeleteEpisode(ctx context.Context, ownerID, slug string) error {
	dir := s.userDir(ownerID)
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
	// The owner's delete propagates to every referencing feed (ADR 0006).
	return s.removeShares(ctx, func(sh store.Share) bool {
		return sh.OwnerID == ownerID && sh.Slug == slug
	})
}

// --- shares ---

func (s *Store) readShares(userID string) ([]store.Share, error) {
	var shares []store.Share
	err := readJSON(filepath.Join(s.userDir(userID), sharesFile), &shares)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	for i := range shares {
		shares[i].UserID = userID
	}
	return shares, nil
}

func (s *Store) writeShares(userID string, shares []store.Share) error {
	return writeJSON(filepath.Join(s.userDir(userID), sharesFile), shares)
}

func (s *Store) AddShare(ctx context.Context, sh store.Share) error {
	if _, err := s.GetUser(ctx, sh.UserID); err != nil {
		return err
	}
	if _, err := s.GetEpisode(ctx, sh.OwnerID, sh.Slug); err != nil {
		return err
	}
	shares, err := s.readShares(sh.UserID)
	if err != nil {
		return err
	}
	for _, have := range shares {
		if have.OwnerID == sh.OwnerID && have.Slug == sh.Slug {
			return nil // already in the feed; the first Sharer is kept
		}
	}
	return s.writeShares(sh.UserID, append(shares, sh))
}

func (s *Store) GetShare(ctx context.Context, userID, ownerID, slug string) (store.Share, error) {
	shares, err := s.readShares(userID)
	if err != nil {
		return store.Share{}, err
	}
	for _, sh := range shares {
		if sh.OwnerID == ownerID && sh.Slug == slug {
			return sh, nil
		}
	}
	return store.Share{}, store.ErrNotFound
}

func (s *Store) RemoveShare(_ context.Context, userID, ownerID, slug string) error {
	shares, err := s.readShares(userID)
	if err != nil {
		return err
	}
	kept := shares[:0]
	for _, sh := range shares {
		if sh.OwnerID != ownerID || sh.Slug != slug {
			kept = append(kept, sh)
		}
	}
	if len(kept) == len(shares) {
		return store.ErrNotFound
	}
	return s.writeShares(userID, kept)
}

func (s *Store) ListShares(ctx context.Context, userID string) ([]store.Share, error) {
	if _, err := s.GetUser(ctx, userID); err != nil {
		return nil, err
	}
	return s.readShares(userID)
}

// removeShares drops every share matching drop from every user's feed.
func (s *Store) removeShares(ctx context.Context, drop func(store.Share) bool) error {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		shares, err := s.readShares(u.ID)
		if err != nil {
			return err
		}
		kept := shares[:0]
		for _, sh := range shares {
			if !drop(sh) {
				kept = append(kept, sh)
			}
		}
		if len(kept) != len(shares) {
			if err := s.writeShares(u.ID, kept); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- invites ---

func (s *Store) readInvites() ([]store.Invite, error) {
	var invs []store.Invite
	err := readJSON(filepath.Join(s.root, invitesFile), &invs)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	return invs, nil
}

func (s *Store) writeInvites(invs []store.Invite) error {
	return writeJSON(filepath.Join(s.root, invitesFile), invs)
}

func (s *Store) AddInvite(_ context.Context, inv store.Invite) error {
	invs, err := s.readInvites()
	if err != nil {
		return err
	}
	for _, have := range invs {
		if have.Token == inv.Token {
			return fmt.Errorf("fsstore: duplicate invite token")
		}
	}
	return s.writeInvites(append(invs, inv))
}

func (s *Store) GetInvite(_ context.Context, token string) (store.Invite, error) {
	invs, err := s.readInvites()
	if err != nil {
		return store.Invite{}, err
	}
	for _, inv := range invs {
		if inv.Token == token {
			return inv, nil
		}
	}
	return store.Invite{}, store.ErrNotFound
}

func (s *Store) ListInvites(_ context.Context, inviterID string) ([]store.Invite, error) {
	invs, err := s.readInvites()
	if err != nil {
		return nil, err
	}
	mine := []store.Invite{}
	for _, inv := range invs {
		if inv.InviterID == inviterID {
			mine = append(mine, inv)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].CreatedAt.After(mine[j].CreatedAt) })
	return mine, nil
}

func (s *Store) DeleteInvite(_ context.Context, token string) error {
	return s.removeInvites(func(inv store.Invite) bool { return inv.Token == token })
}

func (s *Store) RedeemInvite(_ context.Context, token, userID string) error {
	invs, err := s.readInvites()
	if err != nil {
		return err
	}
	for i, inv := range invs {
		if inv.Token == token && inv.RedeemedBy == "" {
			invs[i].RedeemedBy = userID
			return s.writeInvites(invs)
		}
	}
	return store.ErrNotFound
}

// removeInvites drops every invite matching drop; ErrNotFound when none
// matched.
func (s *Store) removeInvites(drop func(store.Invite) bool) error {
	invs, err := s.readInvites()
	if err != nil {
		return err
	}
	kept := invs[:0]
	for _, inv := range invs {
		if !drop(inv) {
			kept = append(kept, inv)
		}
	}
	if len(kept) == len(invs) {
		return store.ErrNotFound
	}
	return s.writeInvites(kept)
}

// --- generations ---

// Generations live in a "generations/" subdirectory of the user, one JSON
// file each; ListEpisodes already skips directories, so they never look
// like episode sidecars.
func (s *Store) generationPath(userID, id string) string {
	return filepath.Join(s.userDir(userID), "generations", id+".json")
}

func (s *Store) PutGeneration(ctx context.Context, g store.Generation) error {
	if _, err := s.GetUser(ctx, g.UserID); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.generationPath(g.UserID, g.ID)), 0o755); err != nil {
		return err
	}
	g.UpdatedAt = time.Now().UTC()
	return writeJSON(s.generationPath(g.UserID, g.ID), newGenerationRecord(g))
}

// generationRecord persists the fields Generation hides from API JSON
// (the session ID and Script carry json:"-" so they never leak through
// handlers).
type generationRecord struct {
	store.Generation
	Active       bool               `json:"active"`
	SessionID    string             `json:"session_id,omitempty"`
	Script       string             `json:"script,omitempty"`
	Cast         []store.Character  `json:"cast,omitempty"`
	Trace        []store.TraceEntry `json:"trace,omitempty"`
	TraceDropped int                `json:"trace_dropped,omitempty"`
}

func newGenerationRecord(g store.Generation) generationRecord {
	return generationRecord{
		Generation: g,
		Active:     g.Active, SessionID: g.SessionID, Script: g.Script, Cast: g.Cast,
		Trace: g.Trace, TraceDropped: g.TraceDropped,
	}
}

func (r generationRecord) generation(userID, id string) store.Generation {
	g := r.Generation
	g.UserID, g.ID = userID, id // file name is canonical
	g.Active, g.SessionID, g.Script = r.Active, r.SessionID, r.Script
	g.Cast = r.Cast
	g.Trace, g.TraceDropped = r.Trace, r.TraceDropped
	return g
}

func (s *Store) GetGeneration(_ context.Context, userID, id string) (store.Generation, error) {
	var r generationRecord
	if err := readJSON(s.generationPath(userID, id), &r); err != nil {
		return store.Generation{}, err
	}
	return r.generation(userID, id), nil
}

func (s *Store) ListGenerations(ctx context.Context, userID string) ([]store.Generation, error) {
	if _, err := s.GetUser(ctx, userID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.userDir(userID), "generations"))
	if os.IsNotExist(err) {
		return []store.Generation{}, nil
	}
	if err != nil {
		return nil, err
	}
	gens := []store.Generation{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		g, err := s.GetGeneration(ctx, userID, strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		gens = append(gens, g)
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i].CreatedAt.After(gens[j].CreatedAt) })
	return gens, nil
}

func (s *Store) ListActiveGenerations(ctx context.Context) ([]store.Generation, error) {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	active := []store.Generation{}
	for _, u := range users {
		gens, err := s.ListGenerations(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		for _, g := range gens {
			if g.Active {
				active = append(active, g)
			}
		}
	}
	return active, nil
}

// --- audio & cover ---

func (s *Store) OpenAudio(_ context.Context, ownerID, slug string) (store.Audio, error) {
	f, err := os.Open(filepath.Join(s.userDir(ownerID), slug+".mp3"))
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

func (s *Store) SetCover(ctx context.Context, userID, contentType string, r io.Reader) error {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.CoverType != "" && coverFile(u.CoverType) != coverFile(contentType) {
		os.Remove(filepath.Join(s.userDir(userID), coverFile(u.CoverType)))
	}
	if _, err := writeAtomic(filepath.Join(s.userDir(userID), coverFile(contentType)), r); err != nil {
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
	f, err := os.Open(filepath.Join(s.userDir(userID), coverFile(u.CoverType)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", store.ErrNotFound
		}
		return nil, "", err
	}
	return f, u.CoverType, nil
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
