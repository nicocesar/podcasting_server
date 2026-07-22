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
	"unicode/utf8"
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

	// Login credentials (see CONTEXT.md "Credentials"). PasswordHash is
	// a bcrypt hash; empty means the account has no password Login.
	// GoogleSub is the linked Google identity ("sub" claim), indexed for
	// login lookup; empty means not linked. GoogleEmail is display only —
	// identity matching is strictly by sub. At least one Login exists
	// from Redemption onward.
	PasswordHash string `json:"-" datastore:"password_hash,noindex"`
	GoogleSub    string `json:"-" datastore:"google_sub"`
	GoogleEmail  string `json:"-" datastore:"google_email,noindex"`

	// Admin grants the /admin surface: cost reporting, user provisioning,
	// and the per-Generation execution trace. Appointed by the break-glass
	// POST /admin/users/{user}/admin, which is the only route still
	// guarded by ADMIN_TOKEN — it has to work before any admin exists.
	Admin bool `json:"admin,omitempty" datastore:"admin,noindex"`

	// CredentialVersion is stamped into every Session; bumping it (on
	// password change or "log out everywhere") kills all outstanding
	// sessions on their next request.
	CredentialVersion int64 `json:"-" datastore:"credential_version,noindex"`

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

// APIKey is a named, individually revocable credential a User mints for
// one Generator. It grants the Publishing Contract and the Management
// API, never Credential Management. The plaintext secret is shown once
// at minting; only its hex SHA-256 is stored. Wire form:
// "pods_{KeyID}_{secret}" as an Authorization: Bearer token.
type APIKey struct {
	UserID     string    `json:"-" datastore:"user_id"`
	KeyID      string    `json:"key_id" datastore:"-"` // unique; locates the record
	Name       string    `json:"name" datastore:"name,noindex"`
	SecretHash string    `json:"-" datastore:"secret_hash,noindex"`
	CreatedAt  time.Time `json:"created_at" datastore:"created_at,noindex"`
}

// Character is one recurring figure of a story Episode, extracted from
// the script so later Generations can bring the cast back. It lives on
// the canonical Episode: shares are references (ADR 0006), so anyone
// with the Episode in their feed can reuse its cast.
type Character struct {
	Name        string `json:"name" datastore:"name,noindex"`
	Description string `json:"description" datastore:"description,noindex"`
}

// Trace levels, ordered by how much they want an admin's attention.
// LevelNotice is the one that earns its keep: not a failure, but a run
// that quietly degraded — a TTS fallback that succeeded, a script that
// needed translating. Without it a degraded episode is indistinguishable
// from a clean one, which is the whole reason the trace exists.
const (
	LevelInfo   = "info"
	LevelNotice = "notice"
	LevelWarn   = "warn"
	LevelError  = "error"
)

// Trace caps. A Generation entity shares a 1 MiB Datastore budget with
// Script, which for a long episode is already the bulk of it, so the
// trace takes a deliberate ~76 KB slice at worst and truncates rather
// than letting a pathological run push the record over the limit.
const (
	MaxTraceEntries = 80
	MaxTraceMessage = 200
	MaxTraceDetail  = 512
	MaxTraceURL     = 200
)

// TraceEntry is one notable thing that happened during a Generation:
// enough for an admin to reconstruct a run — which TTS engine failed and
// why, whether a script was rejected, whether characters were extracted —
// without reaching for Cloud Logging.
//
// Every field is a scalar on purpose. Datastore cannot store a slice or
// map nested inside a slice-of-structs, so arbitrary key/values live in
// Detail as a compact JSON object rather than as a map. Adding a
// non-scalar field here fails at Put time against real Datastore while
// passing every fsstore test.
type TraceEntry struct {
	At      time.Time `json:"at" datastore:"at,noindex"`
	Level   string    `json:"level" datastore:"level,noindex"`
	Stage   string    `json:"stage,omitempty" datastore:"stage,noindex"`
	Event   string    `json:"event" datastore:"event,noindex"` // stable dotted slug, e.g. "tts.fallback"
	Message string    `json:"message" datastore:"message,noindex"`
	Detail  string    `json:"detail,omitempty" datastore:"detail,noindex"` // JSON object, or ""
	URL     string    `json:"url,omitempty" datastore:"url,noindex"`
}

// Episode is one playable item. It exists once, under its Owner — the
// User whose API Key created it — and is referenced by any number
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

	// Template is the Generation Template that produced the episode
	// ("news", "stories"); empty for uploads and pre-template episodes.
	Template string `json:"template,omitempty" datastore:"template,noindex"`
	// Characters is the extracted cast of a story episode; empty until
	// the owner runs extraction (checkbox at generation, or backfill).
	Characters []Character `json:"characters,omitempty" datastore:"characters,noindex"`
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
	// Template names the Generation Template ("news", "stories"); empty
	// means news, the only template that existed before the field.
	Template      string `json:"template,omitempty" datastore:"template,noindex"`
	Topic         string `json:"topic" datastore:"topic,noindex"`
	LengthMinutes int    `json:"length_minutes" datastore:"length_minutes,noindex"`
	FreshnessDays int    `json:"freshness_days" datastore:"freshness_days,noindex"`
	// AgeRange is the stories listener age band ("2-4", "5-7", "8-12",
	// "all"); empty for templates without the field.
	AgeRange string `json:"age_range,omitempty" datastore:"age_range,noindex"`
	// SaveCharacters asks the pipeline to extract the cast onto the
	// published Episode after publishing (stories only).
	SaveCharacters bool `json:"save_characters,omitempty" datastore:"save_characters,noindex"`
	// Cast is the returning cast frozen at submit time, so a resumed
	// Generation rebuilds the identical task message even if the source
	// episode has since been deleted or unshared (same checkpoint
	// philosophy as Script).
	Cast     []Character `json:"-" datastore:"cast,noindex"`
	Language string      `json:"language" datastore:"language,noindex"`
	Voice    string      `json:"voice,omitempty" datastore:"voice,noindex"` // "female" or "male"; empty predates the voice picker
	// Provider is the preferred TTS engine name ("edge-tts",
	// "google-tts", "elevenlabs"); empty = auto (default chain order).
	// Preference only —
	// TTSEngine below records which engine actually voiced the episode.
	Provider string `json:"provider,omitempty" datastore:"provider,noindex"`

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

	// Meters: what this Generation consumed, as lifetime totals across
	// retries — false starts cost real money and are counted, not
	// hidden. Raw counts only; dollars come from Anthropic's Cost API
	// (GET /admin/costs), never from a price table here.
	SessionsCount    int    `json:"sessions_count,omitempty" datastore:"sessions_count,noindex"`
	InputTokens      int64  `json:"input_tokens,omitempty" datastore:"input_tokens,noindex"`
	OutputTokens     int64  `json:"output_tokens,omitempty" datastore:"output_tokens,noindex"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty" datastore:"cache_read_tokens,noindex"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty" datastore:"cache_write_tokens,noindex"`
	TTSEngine        string `json:"tts_engine,omitempty" datastore:"tts_engine,noindex"`         // engine that voiced the published episode
	TTSCharacters    int    `json:"tts_characters,omitempty" datastore:"tts_characters,noindex"` // runes synthesized by the winning engine
	TTSAttempts      int    `json:"tts_attempts,omitempty" datastore:"tts_attempts,noindex"`     // engines tried; >1 per voicing means a fallback fired

	// Trace is the execution record: what happened during this run, for
	// admin eyes. json:"-" because it carries raw upstream error strings,
	// session ids and console links — it must never ride along on the
	// owner-facing poll of /me/generations/{id}. Admin surfaces opt in
	// explicitly. TraceDropped counts entries evicted at the cap, so a
	// truncated trace can say so instead of quietly looking complete.
	//
	// Caveat for whoever debugs from this: the runner is the sole writer,
	// but PutGeneration is a blind whole-entity overwrite, so if two
	// replicas ever resume the same Generation (the known Kick race) one
	// replica's entries are lost. A trace with holes is possible.
	Trace        []TraceEntry `json:"-" datastore:"trace,noindex"`
	TraceDropped int          `json:"-" datastore:"trace_dropped,noindex"`

	CreatedAt time.Time `json:"created_at" datastore:"created_at,noindex"`
	UpdatedAt time.Time `json:"updated_at" datastore:"updated_at,noindex"`
}

// AppendTrace adds one entry, truncating its strings and enforcing the
// entry cap. When full it evicts the oldest info entry rather than the
// oldest entry outright: a long run emits many routine events, and the
// warn/error entries that motivated the trace must not be the ones pushed
// out by them. Only when nothing routine is left does it drop the oldest
// of any level.
func (g *Generation) AppendTrace(e TraceEntry) {
	e.Message = truncate(e.Message, MaxTraceMessage)
	e.Detail = truncate(e.Detail, MaxTraceDetail)
	e.URL = truncate(e.URL, MaxTraceURL)
	g.Trace = append(g.Trace, e)
	for len(g.Trace) > MaxTraceEntries {
		i := g.evictIndex()
		g.Trace = append(g.Trace[:i], g.Trace[i+1:]...)
		g.TraceDropped++
	}
}

// evictIndex picks the entry to drop: the oldest info, else the oldest.
func (g *Generation) evictIndex() int {
	for i, e := range g.Trace {
		if e.Level == LevelInfo {
			return i
		}
	}
	return 0
}

// truncate cuts s to at most n bytes without splitting a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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
	// GetUserByGoogleSub resolves a Google identity to its User —
	// strictly by sub, never by email.
	GetUserByGoogleSub(ctx context.Context, sub string) (User, error)
	// ListUsers returns all users ordered by ID.
	ListUsers(ctx context.Context) ([]User, error)
	// DeleteUser removes the user, their episodes, audio, cover, the
	// shares in their feed, every share of their episodes in other
	// feeds, the invites they minted, and their API keys.
	DeleteUser(ctx context.Context, id string) error

	// PutAPIKey stores an API key record (the secret already hashed).
	PutAPIKey(ctx context.Context, k APIKey) error
	// GetAPIKey resolves a key by its KeyID, whoever owns it.
	GetAPIKey(ctx context.Context, keyID string) (APIKey, error)
	// ListAPIKeys returns the user's keys newest-first.
	ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error)
	DeleteAPIKey(ctx context.Context, keyID string) error

	// UpsertEpisode stores audio and metadata, replacing any existing
	// episode with the same (OwnerID, Slug), and returns the episode
	// with AudioSize filled in.
	UpsertEpisode(ctx context.Context, ep Episode, audio io.Reader) (Episode, error)
	// UpdateEpisode replaces the episode's metadata, keeping its audio;
	// ErrNotFound if no episode exists at (OwnerID, Slug).
	UpdateEpisode(ctx context.Context, ep Episode) error
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
