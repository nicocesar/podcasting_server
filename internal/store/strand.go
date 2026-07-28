package store

// The public side of the station: the Strand canon (ADR 0017), the
// Airings that put Episodes on it (ADR 0018), and the Follows that
// deliver a Strand into a Personal Feed (ADR 0019, ADR 0027).

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	StrandIDMinLen = 3
	StrandIDMaxLen = 32
	// StrandTitleMaxLen and StrandDescriptionMaxLen keep a canon entry
	// inside what a podcast client will render as a channel title and
	// summary without truncating mid-word.
	StrandTitleMaxLen       = 60
	StrandDescriptionMaxLen = 600
)

var strandIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// DeliveryHorizon bounds how far back a Follow reaches, so a new
// follower gets a month of backfill rather than the whole archive
// landing on their phone at once. It is the only bound on delivery
// besides Mute (ADR 0027).
const DeliveryHorizon = 30 * 24 * time.Hour

// StrandFeedItems caps a Strand Feed. Podcast clients re-read the whole
// document on every poll, so an uncapped public feed grows without
// bound and is fetched in full each time.
const StrandFeedItems = 100

// airingIDBytes is the entropy behind a public Airing id. The id is
// opaque but deliberately not secret — every Airing is listed on its
// Strand Page, so guessing one grants nothing that browsing would not.
// It exists to keep the Owner's username out of public URLs, which is
// what keeps usernames unenumerable and the zero-consent Share path
// (ADR 0006) unaimable at strangers.
const airingIDBytes = 6

var airingIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewAiringID mints the public identifier of an Airing: ten lowercase
// base32 characters, safe in a URL and in a podcast client's cache key.
func NewAiringID() (string, error) {
	buf := make([]byte, airingIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("airing id: %w", err)
	}
	return airingIDEncoding.EncodeToString(buf), nil
}

// Strand is one subject in the canon the station defines (ADR 0017).
// Users never coin one and neither does the classifier; an admin adds
// it, names it, and gives it cover art. The ID is immutable because it
// addresses the public feed, and renaming it would silently kill every
// subscription.
type Strand struct {
	ID          string `json:"id" datastore:"-"` // key; immutable
	Title       string `json:"title" datastore:"title,noindex"`
	Description string `json:"description,omitempty" datastore:"description,noindex"`

	// CoverType is the MIME type of the Strand's cover art; empty means
	// none yet, which keeps the Strand dormant. A podcast feed without
	// <itunes:image> is broken in most clients, so a Strand cannot be
	// Aired into until an admin has given it art.
	CoverType string `json:"cover_type,omitempty" datastore:"cover_type,noindex"`

	// ArtText, Accent and Icon are the Art Spec: the record of how this
	// Strand's cover was made (ADR 0021). Empty ArtText with a CoverType
	// set means the art came from a file rather than from words — an
	// upload clears the Spec, which is the only way the two are told
	// apart, since a generated cover and an uploaded one are otherwise
	// interchangeable by design (ADR 0020).
	//
	// Empty Accent or Icon mean "derive it from the words", exactly as
	// they do on the way into coverart.Spec. All three empty is what
	// every Strand created before this looks like, and it reads as
	// today's behaviour, so there is nothing to migrate.
	ArtText string `json:"art_text,omitempty" datastore:"art_text,noindex"`
	Accent  string `json:"accent,omitempty" datastore:"accent,noindex"`
	Icon    string `json:"icon,omitempty" datastore:"icon,noindex"`

	// Retired takes a Strand out of the canon without taking it off the
	// internet: no new Airings, no place in discovery, but its page and
	// feed keep serving whoever already subscribed. The only way a
	// Strand ends once anything has Aired on it.
	Retired bool `json:"retired,omitempty" datastore:"retired,noindex"`

	CreatedAt time.Time `json:"created_at" datastore:"created_at,noindex"`
}

// Dormant reports whether the Strand cannot yet be Aired into: it has
// no cover art, or it has been Retired.
func (s Strand) Dormant() bool { return s.CoverType == "" || s.Retired }

// ValidateStrandID reports why s is unacceptable as a new Strand id, or
// nil if it is fine. The text is safe to show an admin: it names the
// rule broken. Ids share the top-level path namespace with usernames,
// so the reserved list applies here too.
func ValidateStrandID(s string) error {
	if len(s) < StrandIDMinLen || len(s) > StrandIDMaxLen {
		return fmt.Errorf("strand id must be %d to %d characters", StrandIDMinLen, StrandIDMaxLen)
	}
	if !strandIDPattern.MatchString(s) {
		return errors.New("strand id may use only lowercase letters, digits and single dashes between them")
	}
	if Reserved(s) {
		return errors.New("that strand id is reserved")
	}
	return nil
}

// ValidateStrand reports why the canon entry is unacceptable, or nil.
// Cover art is not checked: a Strand is created without it and stays
// Dormant until an admin uploads one.
func ValidateStrand(s Strand) error {
	if err := ValidateStrandID(s.ID); err != nil {
		return err
	}
	if s.Title == "" || len(s.Title) > StrandTitleMaxLen {
		return fmt.Errorf("strand title must be 1 to %d characters", StrandTitleMaxLen)
	}
	if len(s.Description) > StrandDescriptionMaxLen {
		return fmt.Errorf("strand description must be at most %d characters", StrandDescriptionMaxLen)
	}
	return nil
}

// SeedStrands is the canon a fresh install starts with, so a new
// deployment is not dead on arrival. All four are Dormant until an
// admin gives them cover art. A deployment that is not ours is expected
// to retire these and name its own — which is why the canon is stored
// data rather than a constant in this file.
func SeedStrands() []Strand {
	return []Strand{
		{ID: "tech-news", Title: "Tech News", Description: "What happened in technology, briefly."},
		{ID: "music", Title: "Music", Description: "Instrumental pieces, composed to order."},
		{ID: "stories", Title: "Stories", Description: "Told for listening, mostly at bedtime."},
		{ID: "global-news", Title: "Global News", Description: "The wider world, briefly."},
	}
}

// Airing is the Owner's act of putting one Episode on its Strand, where
// anyone may hear it with no capability at all (ADR 0018). It is a
// record rather than a flag on the Episode so that the public queries
// read Airings and never Episodes: a private Episode has no Airing, so
// it is not in the result set to be filtered out — it is not in the
// table, and the query that would serve the whole archive is unwritable.
type Airing struct {
	ID      string `json:"id" datastore:"-"` // key; opaque, not secret
	OwnerID string `json:"owner" datastore:"owner_id"`
	Slug    string `json:"slug" datastore:"slug,noindex"`

	// Strand and AiredAt are indexed: together they are the Strand Page
	// and Strand Feed query, which is the only query the public makes.
	Strand  string    `json:"strand" datastore:"strand"`
	AiredAt time.Time `json:"aired_at" datastore:"aired_at"`
}

// Delivers reports whether a follower receives the Airing: everything
// Aired on a followed Strand, inside the horizon — so a new follower
// gets a month of backfill, not the whole archive (ADR 0027).
func (a Airing) Delivers(now time.Time) bool {
	return now.Sub(a.AiredAt) <= DeliveryHorizon
}

// Follow is a User's standing choice to have a Strand's Aired Episodes
// delivered into their Personal Feed (ADR 0019) — the third kind of
// reference a feed holds, after the User's own Episodes and their
// Shares. Unlike a Share it has no Sharer, because nobody chose to send
// it; unfollowing is the control, and Block and Mute keep their
// existing meanings rather than being overloaded to do this job.
type Follow struct {
	UserID string `json:"-" datastore:"user_id"`
	Strand string `json:"strand" datastore:"strand"`

	At time.Time `json:"at" datastore:"at,noindex"`
}
