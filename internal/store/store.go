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
	"time"
)

// ErrNotFound is returned by all backends when a Show, Episode, audio
// object, or cover does not exist.
var ErrNotFound = errors.New("not found")

// IDPattern constrains Show IDs and Slugs. They appear in URLs, file
// names, and Datastore key names, so they are kept deliberately boring:
// lowercase alphanumerics, dot, dash, underscore.
var IDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidID reports whether s is acceptable as a Show ID or Slug.
func ValidID(s string) bool { return IDPattern.MatchString(s) }

// Show is one podcast: one RSS feed, one subscription in a podcast client.
type Show struct {
	ID          string    `json:"id" datastore:"-"`
	Title       string    `json:"title" datastore:"title,noindex"`
	Description string    `json:"description,omitempty" datastore:"description,noindex"`
	Language    string    `json:"language,omitempty" datastore:"language,noindex"`
	CoverType   string    `json:"cover_type,omitempty" datastore:"cover_type,noindex"` // MIME type; empty means no Cover Art
	UpdatedAt   time.Time `json:"updated_at" datastore:"updated_at,noindex"`
}

// Episode is one playable item in a Show. Its identity within the Show is
// the Slug; publishing an existing Slug replaces the Episode.
type Episode struct {
	ShowID      string    `json:"-" datastore:"show_id"`
	Slug        string    `json:"slug" datastore:"-"`
	Title       string    `json:"title" datastore:"title,noindex"`
	Description string    `json:"description,omitempty" datastore:"description,noindex"`
	PublishedAt time.Time `json:"published_at" datastore:"published_at,noindex"`
	DurationSec int       `json:"duration_seconds,omitempty" datastore:"duration_seconds,noindex"`
	AudioSize   int64     `json:"audio_size,omitempty" datastore:"audio_size,noindex"`
	AudioType   string    `json:"audio_type,omitempty" datastore:"audio_type,noindex"`
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
	UpsertShow(ctx context.Context, s Show) error
	GetShow(ctx context.Context, id string) (Show, error)
	// ListShows returns all shows ordered by ID.
	ListShows(ctx context.Context) ([]Show, error)
	// DeleteShow removes the show, all its episodes, audio, and cover.
	DeleteShow(ctx context.Context, id string) error

	// UpsertEpisode stores audio and metadata, replacing any existing
	// episode with the same Slug, and returns the episode with AudioSize
	// filled in.
	UpsertEpisode(ctx context.Context, ep Episode, audio io.Reader) (Episode, error)
	GetEpisode(ctx context.Context, showID, slug string) (Episode, error)
	// ListEpisodes returns the show's episodes newest-first.
	ListEpisodes(ctx context.Context, showID string) ([]Episode, error)
	DeleteEpisode(ctx context.Context, showID, slug string) error

	OpenAudio(ctx context.Context, showID, slug string) (Audio, error)

	SetCover(ctx context.Context, showID, contentType string, r io.Reader) error
	// OpenCover returns the Cover Art bytes and their MIME type.
	OpenCover(ctx context.Context, showID string) (io.ReadCloser, string, error)
}
