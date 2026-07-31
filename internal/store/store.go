// Package store defines the domain types and the storage interface the
// server is built against. Two implementations exist: a filesystem backend
// for local development (fsstore) and a Datastore+GCS backend for
// production (gcpstore).
package store

import (
	"context"
	"errors"
	"fmt"
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

// ValidID reports whether s is acceptable as a User ID or Slug. It is
// the loose rule: it gates lookups and login, and it is what every
// stored ID has always satisfied. Creating a *new* username is stricter
// (ValidateUsername) — but a name that could never be created must still
// be recognised as the key of an account made under the old rule.
func ValidID(s string) bool { return IDPattern.MatchString(s) }

// Username rules. A username is a person's public handle: it addresses
// them in Shares, appears in feed URLs, and sits one typo away from a
// route name. It is deliberately narrower than an ID — no dots, dashes,
// or underscores, a floor and a ceiling on length — and it may not be a
// Reserved word. These apply at creation only; ValidateUsername implies
// ValidID, so existing accounts are never invalidated by tightening them.
const (
	UsernameMinLen = 4
	UsernameMaxLen = 20
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9]+$`)

// reservedUsernames are handles no account may claim. Two kinds live
// here: path segments the router owns (so a username can never shadow or
// be mistaken for a route — keep this in step with the mux, which
// TestReservedCoversRoutes enforces), and roles or system identities a
// stranger should not be able to impersonate. Add freely; it is only
// ever consulted at account creation.
var reservedUsernames = func() map[string]bool {
	words := []string{
		// Router-owned top-level path segments.
		"admin", "api", "auth", "beats", "callback", "cover", "episode", "episodes",
		"f", "feed", "generate", "generations", "google", "healthz", "image",
		"invite", "invites", "login", "logout", "me", "settings", "share",
		"static", "strand", "strands", "tick", "usage", "user", "users",
		// Roles and system identities not to be impersonated.
		"about", "abuse", "anonymous", "everyone", "guest", "help", "host",
		"mail", "moderator", "mod", "null", "official", "owner", "postmaster",
		"root", "security", "staff", "support", "system", "webmaster", "www",
	}
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()

// Reserved reports whether s is a handle no account may claim. Exported
// so the router's own coverage test can check that every route segment
// is accounted for.
func Reserved(s string) bool { return reservedUsernames[s] }

// ValidateUsername reports why s is unacceptable as a new username, or
// nil if it is fine. The error text is safe to show a user: it names the
// rule broken, so the form can explain itself. The order matters — a
// blank or malformed name is reported as such before "taken" or
// "reserved", which would otherwise leak nonsense.
func ValidateUsername(s string) error {
	if len(s) < UsernameMinLen || len(s) > UsernameMaxLen {
		return fmt.Errorf("username must be %d to %d characters", UsernameMinLen, UsernameMaxLen)
	}
	if !usernamePattern.MatchString(s) {
		return errors.New("username may use only lowercase letters and digits")
	}
	if Reserved(s) {
		return errors.New("that username is reserved")
	}
	return nil
}

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

	// LastSeenAt is the liveness timestamp (ADR 0028): the Tick fires a
	// User's due Beats only if they were seen inside the Liveness Window.
	// Written on the feed poll and on the attended pages, coarsened so a
	// value already inside the hour is not rewritten.
	//
	// Deliberately not noindex, and the only property in this system
	// carrying an inequality filter — without the index there is no way
	// to ask who has been here lately except by reading every User.
	//
	// A timestamp and not a "work to do" flag, because level-triggered
	// state has no drain step and no clear step: re-running a Tick after
	// a crash is free, and no wakeup is ever lost.
	//
	// Zero means never seen, which is what every User written before ADR
	// 0028 looks like and what a freshly provisioned account looks like.
	// Both are correctly dormant until somebody actually shows up with
	// them; do not be tempted to stamp this at creation.
	LastSeenAt time.Time `json:"last_seen_at,omitzero" datastore:"last_seen_at"`

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

	// Strand is the subject the station placed this Episode in (ADR
	// 0017), or empty for a Strandless one — an Episode that fits
	// nothing in the canon, or arrived through the Publishing Contract
	// with nothing to read. Set on private Episodes too, so no public
	// endpoint may marshal this struct directly; public responses need
	// their own shape.
	Strand string `json:"strand,omitempty" datastore:"strand,noindex"`

	// AirBarred is set when an admin un-Airs the Episode (ADR 0018) and
	// cleared only by an admin. Without it a takedown is decorative:
	// the Owner would simply Air it again.
	AirBarred bool `json:"air_barred,omitempty" datastore:"air_barred,noindex"`
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

// Invite is an expiring token that does two things until it expires: it
// plays the one Episode it carries, and it admits one new User at its
// Redemption (delivering that Episode as a Share). The playing is
// unlimited and outlives the Redemption; the admitting happens once. An
// Invite carrying no Episode is a plain door. See docs/adr/0007 and 0014.
type Invite struct {
	Token     string `json:"token" datastore:"-"` // key; unguessable
	InviterID string `json:"inviter" datastore:"inviter_id"`

	// Optional payload: an Episode from the inviter's feed. OwnerID is
	// indexed so an Episode's Owner can find every live link to it and
	// revoke one without deleting the Episode (ADR 0014).
	OwnerID string `json:"owner,omitempty" datastore:"owner_id"`
	Slug    string `json:"slug,omitempty" datastore:"slug,noindex"`

	CreatedAt  time.Time `json:"created_at" datastore:"created_at,noindex"`
	ExpiresAt  time.Time `json:"expires_at" datastore:"expires_at,noindex"`
	RedeemedBy string    `json:"redeemed_by,omitempty" datastore:"redeemed_by,noindex"`
}

// Live reports whether the invite still plays its Episode at t. A spent
// invite is still live: Redemption closes the door, not the sound.
func (i Invite) Live(t time.Time) bool { return t.Before(i.ExpiresAt) }

// Redeemable reports whether the invite can still admit a user at t.
func (i Invite) Redeemable(t time.Time) bool {
	return i.RedeemedBy == "" && i.Live(t)
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

	// BeatID names the Beat that fired this Generation; empty for one a
	// User started by hand. Provenance only — the request fields above are
	// still frozen copies, so editing or cancelling the Beat never
	// disturbs a run already in flight.
	BeatID string `json:"beat_id,omitempty" datastore:"beat_id,noindex"`

	Stage string `json:"stage" datastore:"stage,noindex"`
	// Active indexes the resume scan: true until done or failed.
	Active bool   `json:"-" datastore:"active"`
	Error  string `json:"error,omitempty" datastore:"error,noindex"`
	// Dismissed means the User has read this failure and cleared it off
	// their Dashboard. A failed Generation is kept, not deleted — it is
	// still retryable and still carries the meters an admin bills from —
	// so this hides a row rather than dropping a record. Plain JSON
	// rather than json:"-" so the fs backend persists it from the
	// embedded struct without a line in generationRecord.
	Dismissed bool `json:"dismissed,omitempty" datastore:"dismissed,noindex"`

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

	// Music meters, for the templates whose audio is composed rather than
	// voiced. Separate fields rather than reusing the TTS ones: music is
	// billed by duration and has no characters, so folding it into
	// TTSCharacters would make that meter mean two different things
	// depending on the template. Same rule as above — raw counts, no
	// prices, and this is a different vendor's bill from Anthropic's.
	MusicMillis int    `json:"music_millis,omitempty" datastore:"music_millis,noindex"` // audio composed, summed over movements
	MusicCalls  int    `json:"music_calls,omitempty" datastore:"music_calls,noindex"`   // compose requests including retried ones
	MusicModel  string `json:"music_model,omitempty" datastore:"music_model,noindex"`

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

// MaxBeatGapDays caps the Freshness Window a catching-up Beat may ask
// for. It is the largest window the form offers, so a stretched window
// never asks the agent for something a User could not have asked for by
// hand.
const MaxBeatGapDays = 365

// BeatFailureLimit is how many consecutive failed firings pause a Beat.
// Above one because TTS fallbacks and agent hiccups are transient (ADR
// 0012); low enough that a genuinely broken Beat stops spending money
// within a few cycles.
const BeatFailureLimit = 3

// Beat is a Topic a User has the station cover on an ongoing basis: a
// saved Generation request re-run on a cadence, publishing a new Episode
// into their Personal Feed each time.
//
// The request fields are a frozen copy, not a pointer to the Generation
// that created it — same checkpoint philosophy as Generation.Cast (ADR
// 0011). A Beat rebuilds an identical request every firing, and pruning
// old Generations can never orphan one.
//
// A Beat has no clock of its own, and no stored due date either: DueAt
// is derived from LastFiredAt and IntervalDays every time it is asked.
// The Tick is what examines it (ADR 0028), and only for owners seen
// inside the Liveness Window — so a Beat is still reached one owner at a
// time, never swept globally, and a Beat nobody listens to still falls
// quiet.
type Beat struct {
	UserID string `json:"user" datastore:"user_id"`
	ID     string `json:"id" datastore:"-"` // unguessable; key is "{UserID}/{ID}"

	// The frozen request: the same fields /me/generate collects.
	Template       string      `json:"template,omitempty" datastore:"template,noindex"`
	Topic          string      `json:"topic" datastore:"topic,noindex"`
	LengthMinutes  int         `json:"length_minutes" datastore:"length_minutes,noindex"`
	FreshnessDays  int         `json:"freshness_days" datastore:"freshness_days,noindex"`
	AgeRange       string      `json:"age_range,omitempty" datastore:"age_range,noindex"`
	SaveCharacters bool        `json:"save_characters,omitempty" datastore:"save_characters,noindex"`
	Cast           []Character `json:"-" datastore:"cast,noindex"`
	Language       string      `json:"language" datastore:"language,noindex"`
	Voice          string      `json:"voice,omitempty" datastore:"voice,noindex"`
	Provider       string      `json:"provider,omitempty" datastore:"provider,noindex"`

	// IntervalDays is the cadence. For the news template it equals
	// FreshnessDays, so consecutive Episodes neither re-cover nor skip
	// ground; the other templates choose it from the offered options.
	IntervalDays int `json:"interval_days" datastore:"interval_days,noindex"`
	// Paused stops firing without losing the setup. Set by the User, or
	// by the runner after BeatFailureLimit consecutive failures.
	Paused bool `json:"paused,omitempty" datastore:"paused,noindex"`

	// LastFiredAt is the clock: it advances on every firing, successful or
	// not, so a failing Beat retries on cadence instead of hammering.
	// LastSucceededAt is what the Freshness Window stretches from, so the
	// Episode after a run of failures still covers the ground they missed.
	LastFiredAt         time.Time `json:"last_fired_at" datastore:"last_fired_at,noindex"`
	LastSucceededAt     time.Time `json:"last_succeeded_at" datastore:"last_succeeded_at,noindex"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty" datastore:"consecutive_failures,noindex"`
	LastError           string    `json:"last_error,omitempty" datastore:"last_error,noindex"`
	EpisodeCount        int       `json:"episode_count" datastore:"episode_count,noindex"`

	CreatedAt time.Time `json:"created_at" datastore:"created_at,noindex"`
	UpdatedAt time.Time `json:"updated_at" datastore:"updated_at,noindex"`
}

// DueAt is when the Beat next wants to fire. A Beat with no firing yet
// is anchored to its creation: the form that created it already produced
// the first Episode.
func (b Beat) DueAt() time.Time {
	from := b.LastFiredAt
	if from.IsZero() {
		from = b.CreatedAt
	}
	return from.AddDate(0, 0, b.IntervalDays)
}

// Due reports whether the Beat should fire at t.
func (b Beat) Due(t time.Time) bool {
	return !b.Paused && b.IntervalDays > 0 && !t.Before(b.DueAt())
}

// GapDays is the Freshness Window the next firing should use: the ground
// actually uncovered since the last Episode this Beat published, never
// narrower than the cadence and never wider than the widest window the
// form offers. A Beat whose owner stopped polling for ten days comes
// back with one Episode covering the ten days, not one covering today.
func (b Beat) GapDays(t time.Time) int {
	from := b.LastSucceededAt
	if from.IsZero() {
		from = b.CreatedAt
	}
	days := int(t.Sub(from) / (24 * time.Hour))
	return min(max(days, b.IntervalDays), MaxBeatGapDays)
}

// TickStatus is what the last Tick did (ADR 0028). Exactly one exists —
// this is a signal, not a log.
//
// It exists because a deployment can be perfectly healthy and have no
// scheduler job pointed at it, and that failure is invisible from every
// other surface: Beats simply never fire, and stalled Generations are
// never resumed until the process restarts. This is the record the admin
// page reads to say when the clock last reached us.
//
// Every field is noindex. Nothing queries this; one key reads it.
type TickStatus struct {
	// At is when the pass started, DurationMS how long it took.
	At         time.Time `json:"at" datastore:"at,noindex"`
	DurationMS int64     `json:"duration_ms" datastore:"duration_ms,noindex"`

	// LiveUsers is how many Users the Liveness Window admitted — the
	// Tick's entire working set, since a dormant User's Beats are never
	// examined at all.
	LiveUsers int `json:"live_users" datastore:"live_users,noindex"`

	// BeatsFired counts Generations the Tick started; Resumed counts
	// Active Generations it re-Kicked, most of which were already running
	// and no-oped.
	BeatsFired int `json:"beats_fired" datastore:"beats_fired,noindex"`
	Resumed    int `json:"resumed" datastore:"resumed,noindex"`

	// Truncated means the Beat budget ran out. The firings it skipped are
	// deferred, not lost: their clocks did not advance, so they are still
	// due next Tick and their windows widen by the gap rule.
	Truncated bool `json:"truncated,omitempty" datastore:"truncated,noindex"`

	// Trigger is who asked: "scheduler" or "admin".
	Trigger string `json:"trigger" datastore:"trigger,noindex"`

	// Error is the first failure the pass survived. A Tick that fired
	// anything reports success by design, so without this the failure
	// would be visible only in the logs.
	Error string `json:"error,omitempty" datastore:"error,noindex"`
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
	// ListUsersSeenSince returns the Users whose LastSeenAt is at or
	// after cutoff — the liveness gate the Tick fires Beats behind (ADR
	// 0028) — least-recently-seen first, at most limit of them. A limit
	// of zero or less means no limit.
	//
	// Named for the cutoff rather than for "live" because which cutoff
	// counts as live is the caller's policy, not the store's.
	//
	// The ordering is the fairness rule for a Tick that runs out of
	// budget: the User closest to falling dormant is served before the
	// one who has been polling all afternoon. A User who has never been
	// seen at all is never returned, whatever the cutoff.
	ListUsersSeenSince(ctx context.Context, cutoff time.Time, limit int) ([]User, error)
	// TouchUser records that the User was seen at t, and nothing else.
	//
	// Deliberately not UpsertUser: that writes a whole User struct, so a
	// liveness write racing a profile edit would put back the title the
	// reader happened to be holding. This writes the one field, and
	// leaves UpdatedAt alone — being seen is not an edit.
	TouchUser(ctx context.Context, userID string, t time.Time) error
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
	// ListEpisodeInvites returns every invite carrying an Episode owned
	// by ownerID, newest-first — whoever minted it. It answers "who has
	// a live link to something of mine", so the Owner can revoke one
	// (ADR 0014). One call covers a whole Dashboard; callers group by
	// Slug themselves.
	ListEpisodeInvites(ctx context.Context, ownerID string) ([]Invite, error)
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

	// PutBeat stores or replaces a Beat.
	PutBeat(ctx context.Context, b Beat) error
	GetBeat(ctx context.Context, userID, id string) (Beat, error)
	// ListBeats returns the user's beats newest-first. There is still
	// deliberately no all-users variant and no due-date query: the Tick
	// reaches Beats through the liveness gate, one owner at a time, so a
	// Beat's due time stays derived rather than stored and indexed (ADR
	// 0028, keeping ADR 0016's shape for a new reason).
	ListBeats(ctx context.Context, userID string) ([]Beat, error)
	DeleteBeat(ctx context.Context, userID, id string) error

	// PutTickStatus records the outcome of a Tick, replacing the previous
	// one. There is one record, not a history: the question it answers is
	// "did the clock reach us recently", and a log of that is a bigger
	// feature than the signal is worth.
	PutTickStatus(ctx context.Context, ts TickStatus) error
	// GetTickStatus returns the last Tick, ErrNotFound when none has ever
	// run — which on a fresh deployment is the interesting answer.
	GetTickStatus(ctx context.Context) (TickStatus, error)

	// --- the public side: strands, airings, follows ---

	// PutStrand stores or replaces a canon entry. The ID is immutable
	// by convention, not by this call: it addresses the public feed, so
	// the admin layer never offers a rename (ADR 0017).
	PutStrand(ctx context.Context, s Strand) error
	GetStrand(ctx context.Context, id string) (Strand, error)
	// ListStrands returns the whole canon ordered by ID, retired
	// entries included; callers filter. Retirement is a flag, never a
	// deletion, once anything has aired.
	ListStrands(ctx context.Context) ([]Strand, error)
	// DeleteStrand removes a canon entry outright. Only ever called for
	// a Strand that has never been Aired into — a mistake made five
	// minutes ago; the caller checks.
	DeleteStrand(ctx context.Context, id string) error
	// SetStrandCover persists the normalized full-size art and its web
	// thumbnail, the same pair internal/coverart produces for a feed.
	SetStrandCover(ctx context.Context, id, contentType string, full, thumb io.Reader) error
	OpenStrandCover(ctx context.Context, id string) (io.ReadCloser, string, error)
	OpenStrandCoverThumb(ctx context.Context, id string) (io.ReadCloser, string, error)

	// PutAiring stores or replaces an Airing.
	PutAiring(ctx context.Context, a Airing) error
	// GetAiring resolves the public identifier. This is the only way
	// the public surface reaches an Episode: no Airing, no bytes.
	GetAiring(ctx context.Context, id string) (Airing, error)
	// GetAiringByEpisode finds the live Airing of one Episode, so the
	// Owner's Dashboard can say whether it is on the air. ErrNotFound
	// when it is private.
	GetAiringByEpisode(ctx context.Context, ownerID, slug string) (Airing, error)
	// DeleteAiring un-Airs. A re-Air mints a new identifier, so links
	// killed by an un-Air stay dead.
	DeleteAiring(ctx context.Context, id string) error
	// ListAirings returns one Strand's Airings newest-first — the
	// Strand Page, the Strand Feed, and the delivery query all read
	// this and nothing else.
	ListAirings(ctx context.Context, strand string) ([]Airing, error)
	// ListAiringsByOwner returns an Owner's Airings newest-first, for
	// their own view of what they have on the air.
	ListAiringsByOwner(ctx context.Context, ownerID string) ([]Airing, error)

	// PutFollow stores or replaces a User's Follow of a Strand.
	PutFollow(ctx context.Context, f Follow) error
	DeleteFollow(ctx context.Context, userID, strand string) error
	// ListFollows returns the User's Follows ordered by Strand.
	ListFollows(ctx context.Context, userID string) ([]Follow, error)

	OpenAudio(ctx context.Context, ownerID, slug string) (Audio, error)

	// SetCover persists both the normalized full-size Cover Art and its
	// web thumbnail (produced by internal/coverart). contentType is the
	// MIME type of the full image; the thumbnail is always JPEG.
	SetCover(ctx context.Context, userID, contentType string, full, thumb io.Reader) error
	// OpenCover returns the full-size Cover Art bytes and their MIME type.
	OpenCover(ctx context.Context, userID string) (io.ReadCloser, string, error)
	// OpenCoverThumb returns the web thumbnail (always image/jpeg), or
	// ErrNotFound when the user has no thumbnail (e.g. a cover uploaded
	// before thumbnails existed — callers fall back to OpenCover).
	OpenCoverThumb(ctx context.Context, userID string) (io.ReadCloser, string, error)
}
