package store

// The public side of the station: the Strand canon (ADR 0017), the
// Airings that put Episodes on it (ADR 0018), and the Vouches and
// Follows that decide what a Strand delivers into a Personal Feed
// (ADR 0019).

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

// SettleWindow is the day between Airing and eligibility, during which
// Vouches accumulate. When it closes the count is frozen and the
// delivery question is answered once (ADR 0019): nothing may be
// inserted into a listener's past, so a late Vouch shows but does not
// deliver.
const SettleWindow = 24 * time.Hour

// DeliveryHorizon bounds how far back a Follow reaches, so a new
// follower gets a month of backfill rather than the whole archive
// landing on their phone at once.
const DeliveryHorizon = 30 * 24 * time.Hour

// DefaultBar is the Bar a new Follow starts at: deliver what somebody
// vouched for. Zero is the firehose and is a deliberate choice, never a
// default that arrives by forgetting to set the field.
const DefaultBar = 1

// MaxBar is as high as a Bar goes. Beyond a handful of Vouches a Bar
// stops filtering and starts silencing, which unfollowing does better.
const MaxBar = 10

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

	// Settled closes the window Vouches accrue in, freezing
	// VouchesAtSettle. Delivery asks only about the frozen number, so a
	// later un-Vouch cannot retract an Episode from feeds that already
	// carry it: a feed that flaps is worse than one that is wrong.
	// Vouches keep accruing for display, so a Strand Page may show four
	// on an Episode that entered feeds at one.
	Settled         bool `json:"settled" datastore:"settled,noindex"`
	VouchesAtSettle int  `json:"vouches_at_settle" datastore:"vouches_at_settle,noindex"`
}

// Settleable reports whether the Airing's Vouch window has closed and
// its count has not yet been frozen. Settling needs no scheduler: as
// with Beats (ADR 0016), the work happens on the traffic that reads it.
func (a Airing) Settleable(now time.Time) bool {
	return !a.Settled && now.Sub(a.AiredAt) >= SettleWindow
}

// Delivers reports whether a follower with this Bar receives the
// Airing. Only a settled Airing delivers, and only inside the horizon —
// so a new follower gets a month of backfill, not the whole archive.
func (a Airing) Delivers(bar int, now time.Time) bool {
	if !a.Settled || now.Sub(a.AiredAt) > DeliveryHorizon {
		return false
	}
	return a.VouchesAtSettle >= bar
}

// Vouch is one signed-in User putting their name to one Aired Episode
// (ADR 0019). Public and attributed, and never for one's own Episode.
// Deliberately not a vote: there is no ranking, no score and no
// ordering by it. At the size of this station a Vouch says "this one is
// worth your time", and one of them carries further than a tally would.
type Vouch struct {
	AiringID string    `json:"airing" datastore:"airing_id"`
	UserID   string    `json:"user" datastore:"user_id"`
	At       time.Time `json:"at" datastore:"at,noindex"`
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

	// Bar is how many Vouches an Episode must carry to be delivered:
	// zero for everything Aired, one for whatever somebody vouched for,
	// higher for a Strand that has become noisy. Per Follow, so one
	// Strand can be a firehose for one listener and a trickle for
	// another.
	Bar int `json:"bar" datastore:"bar,noindex"`

	At time.Time `json:"at" datastore:"at,noindex"`
}

// ValidateBar reports why b is unacceptable as a Bar, or nil.
func ValidateBar(b int) error {
	if b < 0 || b > MaxBar {
		return fmt.Errorf("bar must be 0 to %d", MaxBar)
	}
	return nil
}
