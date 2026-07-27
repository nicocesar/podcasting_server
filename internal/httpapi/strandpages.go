package httpapi

// The public surface itself: the Strand index, a Strand Page, its Strand
// Feed, its cover art, and the audio of an Aired Episode (ADR 0018).
//
// Everything here is reachable with no capability at all, which makes it
// the only code in this server that hands audio to a stranger. Two rules
// hold it together:
//
//   - Every listing reads Airings, never Episodes. A private Episode has
//     no Airing, so it is not in the result set to be filtered out — it
//     is not in the table, and the query that would serve the whole
//     archive cannot be written by accident.
//   - The audio route resolves an Airing id and nothing else. Were it to
//     accept (owner, slug) it would serve every private Episode in the
//     system to anyone who guessed a date, and the slugs are dates.

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/feed"
	"github.com/nicocesar/podcasting_server/internal/store"
)

// publicCache is what an anonymous fetch of a page or a cover may keep.
// Egress here is unauthenticated and unmetered, so caching is most of
// the protection a rate limiter would give (ADR 0018).
const publicCache = "public, max-age=300"

// strandCard is one Strand on the index.
type strandCard struct {
	store.Strand
	Episodes int
}

// strandsPage is the template data for /strands.
type strandsPage struct {
	Strands []strandCard
}

// airedView is one Aired Episode on a Strand Page.
type airedView struct {
	Airing  store.Airing
	Episode store.Episode
	Author  string
	Vouches []store.Vouch
	// Vouched marks the signed-in viewer's own vouch, so the button can
	// say what pressing it will do.
	Vouched bool
	// Mine is true for the viewer's own Episode: a Vouch is never for
	// your own (ADR 0019), so the control is not offered at all.
	Mine   bool
	Player playerView
}

// strandPage is the template data for /strands/{strand}.
type strandPage struct {
	Strand    store.Strand
	Episodes  []airedView
	FeedURL   string
	SignedIn  bool
	Following bool
	Bar       int
}

// handleStrandsIndex lists the canon. This is the narrowing of ADR 0003
// that ADR 0018 spells out: Strands are enumerable and are meant to be,
// while Users and Personal Feeds still are not.
func (s *server) handleStrandsIndex(w http.ResponseWriter, r *http.Request) {
	canon, err := s.store.ListStrands(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	cards := make([]strandCard, 0, len(canon))
	for _, st := range canon {
		// A Retired Strand keeps serving its subscribers but leaves
		// discovery, and a Strand with no art has nothing to show yet.
		if st.Dormant() {
			continue
		}
		airings, err := s.store.ListAirings(r.Context(), st.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		cards = append(cards, strandCard{Strand: st, Episodes: len(airings)})
	}
	w.Header().Set("Cache-Control", publicCache)
	s.render(w, http.StatusOK, s.tmplStrands, strandsPage{Strands: cards})
}

// handleStrandPage renders one Strand for a browser with no credentials.
func (s *server) handleStrandPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.strandOr404(w, r)
	if !ok {
		return
	}
	viewer, signedIn := s.sessionUser(r)
	views, err := s.airedViews(r, st, viewer, signedIn)
	if err != nil {
		s.fail(w, err)
		return
	}
	data := strandPage{
		Strand:   st,
		Episodes: views,
		FeedURL:  s.base(r) + "/strands/" + st.ID + "/feed.xml",
		SignedIn: signedIn,
	}
	if signedIn {
		if f, err := s.followOf(r, viewer, st.ID); err == nil {
			data.Following, data.Bar = true, f.Bar
		}
	}
	// Signed-in pages are per-viewer (mute, vouch state), so they must
	// not land in a shared cache.
	if signedIn {
		w.Header().Set("Cache-Control", "private, no-store")
	} else {
		w.Header().Set("Cache-Control", publicCache)
	}
	s.render(w, http.StatusOK, s.tmplStrand, data)
}

// airedViews loads a Strand's Airings and the Episodes behind them.
// Airings whose Episode has since vanished are skipped rather than
// rendered as holes: an Owner's delete propagates with no tombstone
// (ADR 0006), and a stale Airing is that delete arriving late.
func (s *server) airedViews(r *http.Request, st store.Strand, viewer store.User, signedIn bool) ([]airedView, error) {
	airings, err := s.store.ListAirings(r.Context(), st.ID)
	if err != nil {
		return nil, err
	}
	// Reading a Strand is the traffic settling rides on (ADR 0016's
	// pattern): an Airing whose day is up has its Vouch count frozen
	// here rather than by a scheduler nobody runs.
	airings = s.settleDue(r, airings)
	muted := map[string]bool{}
	if signedIn {
		for _, id := range viewer.Mutes {
			muted[id] = true
		}
	}
	views := make([]airedView, 0, len(airings))
	for _, a := range airings {
		if len(views) >= store.StrandFeedItems {
			break
		}
		// Mute means nothing from that Owner anywhere the viewer looks,
		// not only in their feed (ADR 0019).
		if muted[a.OwnerID] {
			continue
		}
		ep, err := s.store.GetEpisode(r.Context(), a.OwnerID, a.Slug)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		owner, err := s.store.GetUser(r.Context(), a.OwnerID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		vouches, err := s.store.ListVouches(r.Context(), a.ID)
		if err != nil {
			return nil, err
		}
		v := airedView{
			Airing:  a,
			Episode: ep,
			Author:  owner.Title,
			Vouches: vouches,
			Mine:    signedIn && a.OwnerID == viewer.ID,
			Player: playerView{
				AudioURL: "/strands/" + st.ID + "/" + a.ID + ".mp3",
				Title:    ep.Title,
				Seconds:  ep.DurationSec,
				// The Airing id keys the browser's resume position, so
				// an un-air and re-air starts the listener over rather
				// than resuming audio that may have changed.
				Key:      a.ID,
				CoverURL: "/strands/" + st.ID + "/cover",
			},
		}
		if signedIn {
			for _, vo := range vouches {
				if vo.UserID == viewer.ID {
					v.Vouched = true
					break
				}
			}
		}
		views = append(views, v)
	}
	return views, nil
}

// handleStrandFeed renders the Strand as public podcast RSS.
func (s *server) handleStrandFeed(w http.ResponseWriter, r *http.Request) {
	st, ok := s.strandOr404(w, r)
	if !ok {
		return
	}
	// Anonymous: a feed is fetched by a podcast client, which carries no
	// session, so there is no viewer to mute anything for.
	views, err := s.airedViews(r, st, store.User{}, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	base := s.base(r)
	items := make([]feed.Item, 0, len(views))
	for _, v := range views {
		items = append(items, feed.Item{
			Episode:      v.Episode,
			EnclosureURL: feed.StrandEnclosure(base, st.ID, v.Airing.ID),
			Author:       v.Author,
		})
	}
	body, err := feed.StrandRSS(st, items, base)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", publicCache)
	w.Write(body)
}

// handleStrandCover serves the Strand's art — the identity a podcast
// client shows, and the image the index cards use.
func (s *server) handleStrandCover(w http.ResponseWriter, r *http.Request) {
	art, contentType, err := s.openStrandArt(r, r.PathValue("strand"))
	if err != nil {
		s.fail(w, err)
		return
	}
	defer art.Close()
	w.Header().Set("Content-Type", contentType)
	// Same hour as a feed's Cover Art (ADR 0003): a replaced image may
	// take that long to reach clients.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	io.Copy(w, art)
}

func (s *server) openStrandArt(r *http.Request, id string) (io.ReadCloser, string, error) {
	if r.URL.Query().Get("s") == "thumb" {
		rc, ct, err := s.store.OpenStrandCoverThumb(r.Context(), id)
		if err == nil {
			return rc, ct, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, "", err
		}
		// Fall through: a cover stored before thumbnails existed.
	}
	return s.store.OpenStrandCover(r.Context(), id)
}

// handleStrandAudio is the most dangerous handler in this server: it
// hands MP3 bytes to anyone at all. It takes an Airing id and nothing
// else, so an Episode with no live Airing has no address here.
func (s *server) handleStrandAudio(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".mp3")
	if !ok {
		s.renderNotFound(w)
		return
	}
	airing, err := s.store.GetAiring(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The id alone decides; the strand in the path is decoration, and
	// must not be a second way in.
	if airing.Strand != r.PathValue("strand") {
		s.renderNotFound(w)
		return
	}
	w.Header().Set("Cache-Control", publicCache)
	s.serveAudio(w, r, airing.OwnerID, airing.Slug)
}

// strandOr404 resolves the path's Strand. A Retired one still answers —
// it keeps serving whoever already subscribed — but one that has never
// had art has nothing to serve.
func (s *server) strandOr404(w http.ResponseWriter, r *http.Request) (store.Strand, bool) {
	st, err := s.store.GetStrand(r.Context(), r.PathValue("strand"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && st.CoverType == "") {
		s.renderNotFound(w)
		return store.Strand{}, false
	}
	if err != nil {
		s.fail(w, err)
		return store.Strand{}, false
	}
	return st, true
}

func (s *server) followOf(r *http.Request, u store.User, strand string) (store.Follow, error) {
	follows, err := s.store.ListFollows(r.Context(), u.ID)
	if err != nil {
		return store.Follow{}, err
	}
	for _, f := range follows {
		if f.Strand == strand {
			return f, nil
		}
	}
	return store.Follow{}, store.ErrNotFound
}

// settleDue freezes the Vouch count of every Airing whose day is up.
// Settling needs no scheduler: as with Beats (ADR 0016), the work rides
// on the traffic that reads it.
func (s *server) settleDue(r *http.Request, airings []store.Airing) []store.Airing {
	now := time.Now().UTC()
	for i, a := range airings {
		if !a.Settleable(now) {
			continue
		}
		vouches, err := s.store.ListVouches(r.Context(), a.ID)
		if err != nil {
			s.log.Error("could not settle an airing", "airing", a.ID, "err", err)
			continue
		}
		a.Settled, a.VouchesAtSettle = true, len(vouches)
		if err := s.store.PutAiring(r.Context(), a); err != nil {
			s.log.Error("could not settle an airing", "airing", a.ID, "err", err)
			continue
		}
		airings[i] = a
	}
	return airings
}
