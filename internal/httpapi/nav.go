package httpapi

// The chrome: one navigation bar on every HTML page, so a reader is
// never more than a click from their Dashboard, the canon, or their
// settings. Before this, each page hand-rolled a back-link to one
// hard-coded parent and lateral movement was impossible — the only way
// from a Strand Page to your own feed was the browser's back button.
//
// Two things decide what the bar shows, and they are deliberately not
// the same question:
//
//   - Who is reading. Resolved once by the auth middleware and memoed on
//     the request, so building the bar usually costs no extra store read.
//   - Where they are reading it. A page inside a capability namespace
//     (/f/{token}, /invites/{token}) gets the public bar even when the
//     viewer happens to be signed in, because those URLs are the whole
//     credential and are meant to work for whoever holds the link.
//     Rendering somebody's member links there would offer navigation to
//     a reader who is not that somebody.

import (
	"context"
	"net/http"

	"github.com/nicocesar/podcasting_server/internal/store"
)

type ctxKey int

const (
	// navUserKey memoes the User the auth middleware already resolved.
	navUserKey ctxKey = iota
	// navCapabilityKey marks a request served inside a capability
	// namespace, where the bar stays public regardless of session.
	navCapabilityKey
)

// withNavUser memoes a resolved User for the rest of the request.
func withNavUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, navUserKey, u)
}

// withCapabilityScope marks the request as served from a capability URL.
func withCapabilityScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, navCapabilityKey, true)
}

// navView is what the bar renders from. It carries no page state: the
// bar is the same on every page, which is the point of it.
type navView struct {
	SignedIn bool
	Admin    bool
	// Title and UserID name the station on the identity link. Empty when
	// signed out, where that link is the site itself.
	Title  string
	UserID string
	// Generate reports whether the studio is switched on at all. The
	// link is hidden rather than shown-and-broken when it is not.
	Generate bool
}

// navFor builds the bar for one request. It never fails: a request with
// no session, or one inside a capability namespace, gets the public bar.
func (s *server) navFor(r *http.Request) navView {
	if r == nil {
		return navView{}
	}
	if capability, _ := r.Context().Value(navCapabilityKey).(bool); capability {
		return navView{}
	}
	u, ok := r.Context().Value(navUserKey).(store.User)
	if !ok {
		// Not behind auth middleware — a public page a signed-in reader
		// may still be looking at, like a Strand Page. This is the only
		// path that costs a store read, and only for a request that
		// actually carries a session cookie.
		if u, ok = s.sessionUser(r); !ok {
			return navView{}
		}
	}
	return navView{
		SignedIn: true,
		Admin:    u.Admin,
		Title:    u.Title,
		UserID:   u.ID,
		Generate: s.generator != nil,
	}
}

// buildStamp is the three-part answer to "what exactly am I running":
// the release someone chose, the commit it was cut from, and when the
// image was built. Only the release is guaranteed — a local `go build`
// has no commit and no build time, and says so by omitting them rather
// than inventing values.
type buildStamp struct {
	Version string
	Commit  string
	// BuiltAt is RFC3339 UTC, for the <time datetime> attribute. The
	// browser turns it into "2 days ago" and a local-time tooltip: the
	// reader's clock is the only one that can do that correctly, and a
	// server-rendered relative time is stale the moment it is cached.
	BuiltAt string
}

// Known reports whether there is anything worth showing. False for a
// working tree, where a version stamp would be theatre.
func (b buildStamp) Known() bool { return b.Version != "" || b.Commit != "" }

func (s *server) buildStamp() buildStamp {
	return buildStamp{Version: s.version, Commit: s.commit, BuiltAt: s.builtAt}
}

// airRowView is everything the airing control needs, and nothing else.
// It is self-contained on purpose: the Dashboard and the Episode Page
// both render the same fragment from one of these, so the two surfaces
// cannot drift into disagreeing about what airing means or who may do
// it. Built only for an Episode its viewer owns.
type airRowView struct {
	Slug      string
	AirBarred bool
	// OnAir is the live Airing, or nil when the Episode is private.
	OnAir *store.Airing
	// SuggestedStrand is where the picker starts — what the station
	// chose at generation time, when it chose anything. The station
	// proposes, the Owner disposes (ADR 0017).
	SuggestedStrand string
	// Strands is the canon this Episode may be aired into: awake ones
	// only, since a dormant or retired Strand takes nothing. Empty means
	// the control does not appear at all.
	Strands []store.Strand
	// ReturnTo is where pressing the button lands, anchor and all. The
	// two surfaces pass different values; the handler honours whichever
	// it is given (ADR 0022).
	ReturnTo string
	// Error is what went wrong on the press that produced this row —
	// only ever set when the row is re-rendered in place for htmx. A
	// full-page post still gets a plain status code, because there the
	// browser is leaving anyway and has nowhere to put a message.
	Error string
}

// pageView is what every HTML response executes against: the bar, and
// the page's own data untouched underneath it. Keeping the page data
// whole under .Page is what lets every content template go on using the
// dot it always used — only layout.html knows this wrapper exists.
type pageView struct {
	Nav  navView
	Page any
}
