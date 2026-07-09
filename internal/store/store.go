// Package store defines the domain types and the storage interface the
// server is built against. Two implementations exist: a filesystem backend
// for local development (fsstore) and a Datastore+GCS backend for
// production (gcpstore).
package store

import (
	"context"
	"errors"
	"io"
	"regexp"
	"slices"
	"time"
)

// ErrNotFound is returned by all backends when a User, Episode, Share,
// audio object, or cover does not exist.
var ErrNotFound = errors.New("not found")

// IDPattern constrains User IDs and Slugs. They appear in URLs, file
// names, and Datastore key names, so they are kept deliberately boring:
// lowercase alphanumerics, dot, dash, underscore.
var IDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidID reports whether s is acceptable as a User ID or Slug.
func ValidID(s string) bool { return IDPattern.MatchString(s) }

// User is a person with an account: exactly one Personal Feed, a publish
// token (their Generator), and a Feed Token (their podcast client). See
// docs/adr/0005 and 0008.
type User struct {
	ID string `json:"id" datastore:"-"`

	// Personal Feed presentation.
	Title       string `json:"title" datastore:"title,noindex"`
	Description string `json:"description,omitempty" datastore:"description,noindex"`
	Language    string `json:"language,omitempty" datastore:"language,noindex"`
	CoverType   string `json:"cover_type,omitempty" datastore:"cover_type,noindex"` // MIME type; empty means no Cover Art

	// FeedToken is the capability that IS the read side: the feed,
	// audio, and cover are served under /f/{FeedToken}/ with no other
	// authentication (ADR 0008). Stored as-is — it must be displayed
	// back to its owner — and replaced wholesale on rotation.
	FeedToken string `json:"-" datastore:"feed_token"`

	// PublishHash is hex SHA-256 of "user:token" for the publish token.
	// Plaintext is never stored.
	PublishHash string `json:"-" datastore:"publish_hash,noindex"`

	// Blocks: users whose Shares are rejected at share time.
	// Mutes: owners whose Episodes are hidden from this feed at render
	// time, whoever shared them. See docs/adr/0006.
	Blocks []string `json:"blocks,omitempty" datastore:"blocks,noindex"`
	Mutes  []string `json:"mutes,omitempty" datastore:"mutes,noindex"`

	UpdatedAt time.Time `json:"updated_at" datastore:"updated_at,noindex"`
}

// Blocked reports whether sharer is on the user's block list.
func (u User) Blocked(sharer string) bool { return slices.Contains(u.Blocks, sharer) }

// Muted reports whether owner is on the user's mute list.
func (u User) Muted(owner string) bool { return slices.Contains(u.Mutes, owner) }

// Episode is one playable item. It exists once, under its Owner — the
// User whose publish token created it — and is referenced by any number
// of Personal Feeds. Identity is (OwnerID, Slug); publishing an existing
// Slug replaces the Episode everywhere it is referenced (ADR 0002/0006).
type Episode struct {
	OwnerID     string    `json:"owner" datastore:"owner_id"`
	Slug        string    `json:"slug" datastore:"-"`
	Title       string    `json:"title" datastore:"title,noindex"`
	Description string    `json:"description,omitempty" datastore:"description,noindex"`
	PublishedAt time.Time `json:"published_at" datastore:"published_at,noindex"`
	DurationSec int       `json:"duration_seconds,omitempty" datastore:"duration_seconds,noindex"`
	AudioSize   int64     `json:"audio_size,omitempty" datastore:"audio_size,noindex"`
	AudioType   string    `json:"audio_type,omitempty" datastore:"audio_type,noindex"`
}

// Share is a reference placing one Episode into one User's Personal
// Feed. The Sharer is whoever placed it there and may differ from the
// Owner, since any recipient may share onward (ADR 0006).
type Share struct {
	UserID   string    `json:"-" datastore:"user_id"`      // recipient feed
	OwnerID  string    `json:"owner" datastore:"owner_id"` // episode owner
	Slug     string    `json:"slug" datastore:"slug"`
	SharerID string    `json:"sharer" datastore:"sharer_id,noindex"`
	SharedAt time.Time `json:"shared_at" datastore:"shared_at,noindex"`
}

// Invite is a single-use, expiring token that admits one new User at its
// Redemption; it may carry one Episode from the inviter's feed, delivered
// as a Share on redemption. See docs/adr/0007.
type Invite struct {
	Token     string `json:"token" datastore:"-"` // key; unguessable
	InviterID string `json:"inviter" datastore:"inviter_id"`

	// Optional payload: an Episode from the inviter's feed.
	OwnerID string `json:"owner,omitempty" datastore:"owner_id,noindex"`
	Slug    string `json:"slug,omitempty" datastore:"slug,noindex"`

	CreatedAt  time.Time `json:"created_at" datastore:"created_at,noindex"`
	ExpiresAt  time.Time `json:"expires_at" datastore:"expires_at,noindex"`
	RedeemedBy string    `json:"redeemed_by,omitempty" datastore:"redeemed_by,noindex"`
}

// Redeemable reports whether the invite can still admit a user at t.
func (i Invite) Redeemable(t time.Time) bool {
	return i.RedeemedBy == "" && t.Before(i.ExpiresAt)
}

// Generation stages. A Generation is Active until it reaches done or
// failed; failed ones may be retried from their last completed stage.
const (
	GenResearching = "researching" // agent session: research + Script
	GenVoicing     = "voicing"     // TTS over the Script
	GenPublishing  = "publishing"  // storing the Episode
	GenDone        = "done"
	GenFailed      = "failed"
)

// Generation is one User-requested production of an Episode from a Topic
// (ADR 0009): research and writing delegated to a managed agent, voicing
// and publishing done by the server. The record doubles as the checkpoint
// the pipeline resumes from after a restart — Script is the durable
// midpoint, so a failure after it never repeats the research.
type Generation struct {
	UserID string `json:"user" datastore:"user_id"`
	ID     string `json:"id" datastore:"-"` // unguessable; key is "{UserID}/{ID}"

	// The request, as submitted on /me/generate.
	Topic         string `json:"topic" datastore:"topic,noindex"`
	LengthMinutes int    `json:"length_minutes" datastore:"length_minutes,noindex"`
	FreshnessDays int    `json:"freshness_days" datastore:"freshness_days,noindex"`
	Language      string `json:"language" datastore:"language,noindex"`

	Stage string `json:"stage" datastore:"stage,noindex"`
	// Active indexes the resume scan: true until done or failed.
	Active bool   `json:"-" datastore:"active"`
	Error  string `json:"error,omitempty" datastore:"error,noindex"`

	// Checkpoints.
	SessionID    string `json:"-" datastore:"session_id,noindex"` // managed-agent session
	Script       string `json:"-" datastore:"script,noindex"`     // agent output JSON; empty until researched
	VoicedChunks int    `json:"voiced_chunks" datastore:"voiced_chunks,noindex"`
	TotalChunks  int    `json:"total_chunks" datastore:"total_chunks,noindex"`
	EpisodeSlug  string `json:"episode_slug,omitempty" datastore:"episode_slug,noindex"`

	CreatedAt time.Time `json:"created_at" datastore:"created_at,noindex"`
	UpdatedAt time.Time `json:"updated_at" datastore:"updated_at,noindex"`
}

// Audio is how a backend hands episode audio to the HTTP layer. Exactly
// one of RedirectURL or Content is set: production redirects the client to
// a short-lived signed URL, local development serves the file directly.
type Audio struct {
	RedirectURL string
	Content     io.ReadSeekCloser
	Size        int64
	ModTime     time.Time
	ContentType string
}

// Store is the storage backend behind the HTTP layer. The server is the
// only writer; see docs/adr/0001.
type Store interface {
	UpsertUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, id string) (User, error)
	// GetUserByFeedToken resolves the capability URL to its owner.
	GetUserByFeedToken(ctx context.Context, token string) (User, error)
	// ListUsers returns all users ordered by ID.
	ListUsers(ctx context.Context) ([]User, error)
	// DeleteUser removes the user, their episodes, audio, cover, the
	// shares in their feed, every share of their episodes in other
	// feeds, and the invites they minted.
	DeleteUser(ctx context.Context, id string) error

	// UpsertEpisode stores audio and metadata, replacing any existing
	// episode with the same (OwnerID, Slug), and returns the episode
	// with AudioSize filled in.
	UpsertEpisode(ctx context.Context, ep Episode, audio io.Reader) (Episode, error)
	GetEpisode(ctx context.Context, ownerID, slug string) (Episode, error)
	// ListEpisodes returns the owner's episodes newest-first.
	ListEpisodes(ctx context.Context, ownerID string) ([]Episode, error)
	// DeleteEpisode removes the episode and every Share referencing it,
	// in any feed (the owner's delete propagates; ADR 0006).
	DeleteEpisode(ctx context.Context, ownerID, slug string) error

	// AddShare places the reference in the recipient's feed. If the same
	// episode is already shared into that feed, the existing Share (and
	// its Sharer) is kept.
	AddShare(ctx context.Context, sh Share) error
	GetShare(ctx context.Context, userID, ownerID, slug string) (Share, error)
	RemoveShare(ctx context.Context, userID, ownerID, slug string) error
	// ListShares returns the shares in the user's feed.
	ListShares(ctx context.Context, userID string) ([]Share, error)

	AddInvite(ctx context.Context, inv Invite) error
	GetInvite(ctx context.Context, token string) (Invite, error)
	// ListInvites returns the invites minted by inviterID, newest-first.
	ListInvites(ctx context.Context, inviterID string) ([]Invite, error)
	DeleteInvite(ctx context.Context, token string) error
	// RedeemInvite atomically claims the invite for userID, enforcing
	// single use: ErrNotFound if the token does not exist or is already
	// redeemed. Expiry is the caller's check (Redeemable).
	RedeemInvite(ctx context.Context, token, userID string) error

	// PutGeneration stores or replaces the Generation checkpoint.
	PutGeneration(ctx context.Context, g Generation) error
	GetGeneration(ctx context.Context, userID, id string) (Generation, error)
	// ListGenerations returns the user's generations newest-first.
	ListGenerations(ctx context.Context, userID string) ([]Generation, error)
	// ListActiveGenerations returns every unfinished generation across
	// all users — the resume scan after a restart (ADR 0009).
	ListActiveGenerations(ctx context.Context) ([]Generation, error)

	OpenAudio(ctx context.Context, ownerID, slug string) (Audio, error)

	SetCover(ctx context.Context, userID, contentType string, r io.Reader) error
	// OpenCover returns the Cover Art bytes and their MIME type.
	OpenCover(ctx context.Context, userID string) (io.ReadCloser, string, error)
}
