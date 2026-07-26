package httpapi

// The controls behind the public side: airing an Episode on its Strand,
// vouching for someone else's, and following a Strand into your own feed
// (ADR 0018, ADR 0019).
//
// Session-only, all of it, for the same reason Beats are (ADR 0010): a
// leaked API Key must not be able to put a User's audio in front of
// strangers, and it must not be able to put their name on someone
// else's. The Publishing Contract publishes into a private feed; going
// public is a decision a person makes in a browser.

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// awakeStrands is the canon an Episode may actually be aired into:
// everything neither Retired nor still waiting for its cover art. The
// Dashboard offers exactly these, so the picker can never present a
// choice the air handler would refuse.
func (s *server) awakeStrands(r *http.Request) ([]store.Strand, error) {
	canon, err := s.store.ListStrands(r.Context())
	if err != nil {
		return nil, err
	}
	awake := make([]store.Strand, 0, len(canon))
	for _, st := range canon {
		if !st.Dormant() {
			awake = append(awake, st)
		}
	}
	return awake, nil
}

// airingsBySlug indexes the User's live Airings, so a Dashboard of
// thirty episodes costs one query rather than thirty.
func (s *server) airingsBySlug(r *http.Request, u store.User) (map[string]store.Airing, error) {
	airings, err := s.store.ListAiringsByOwner(r.Context(), u.ID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.Airing, len(airings))
	for _, a := range airings {
		out[a.Slug] = a
	}
	return out, nil
}

// handleAir puts one of the caller's own Episodes on a Strand. The
// Strand comes from the form when the Owner overrides what the station
// chose, and from the Episode otherwise — the station proposes, the
// Owner disposes (ADR 0017).
func (s *server) handleAir(w http.ResponseWriter, r *http.Request, u store.User) {
	slug := r.PathValue("slug")
	ep, err := s.store.GetEpisode(r.Context(), u.ID, slug)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Only the Owner airs. The route is scoped to the caller's own
	// episodes, so a Sharer cannot launder someone else's work onto the
	// public surface by forwarding it to themselves first (ADR 0006
	// lets anyone forward; ADR 0018 does not let anyone air).
	if ep.AirBarred {
		http.Error(w, "the station removed this episode from the air", http.StatusForbidden)
		return
	}

	strand := r.FormValue("strand")
	if strand == "" {
		strand = ep.Strand
	}
	if strand == "" {
		http.Error(w, "this episode has no strand yet — choose one to air it", http.StatusUnprocessableEntity)
		return
	}
	st, err := s.store.GetStrand(r.Context(), strand)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such strand", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if st.Dormant() {
		http.Error(w, "that strand is not taking episodes", http.StatusUnprocessableEntity)
		return
	}

	// Already on the air: re-airing would mint a new public id and kill
	// the old links, so moving strands is an un-air the Owner asks for
	// deliberately rather than a side effect of pressing air twice.
	if have, err := s.store.GetAiringByEpisode(r.Context(), u.ID, slug); err == nil {
		if have.Strand == strand {
			s.redirectToEpisode(w, r, u, slug)
			return
		}
		http.Error(w, "this episode is already on another strand — take it off the air first", http.StatusConflict)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}

	id, err := store.NewAiringID()
	if err != nil {
		s.fail(w, err)
		return
	}
	airing := store.Airing{
		ID: id, OwnerID: u.ID, Slug: slug,
		Strand: strand, AiredAt: time.Now().UTC(),
	}
	if err := s.store.PutAiring(r.Context(), airing); err != nil {
		s.fail(w, err)
		return
	}
	// The Owner's choice sticks to the Episode, so the next time they
	// look at it the station is not still proposing something else.
	if ep.Strand != strand {
		ep.Strand = strand
		if err := s.store.UpdateEpisode(r.Context(), ep); err != nil {
			s.log.Error("could not record the owner's strand choice",
				"user", u.ID, "episode", slug, "strand", strand, "err", err)
		}
	}
	s.redirectToEpisode(w, r, u, slug)
}

// handleUnair takes the caller's own Episode off the air. Best effort by
// nature: whatever has been fetched is gone, and podcast clients keep
// what they downloaded (ADR 0018).
func (s *server) handleUnair(w http.ResponseWriter, r *http.Request, u store.User) {
	slug := r.PathValue("slug")
	airing, err := s.store.GetAiringByEpisode(r.Context(), u.ID, slug)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteAiring(r.Context(), airing.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.redirectToEpisode(w, r, u, slug)
}

// handleAdminUnair is the takedown (ADR 0018). It is the only power an
// admin has over someone else's Airing, and deliberately the smallest
// one that works: the Episode itself survives untouched in its Owner's
// feed, because what had to stop was the publicness. It bars re-airing,
// without which the Owner simply airs it again and the takedown is
// decorative.
func (s *server) handleAdminUnair(w http.ResponseWriter, r *http.Request, _ store.User) {
	airing, err := s.store.GetAiring(r.Context(), r.PathValue("airing"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteAiring(r.Context(), airing.ID); err != nil {
		s.fail(w, err)
		return
	}
	ep, err := s.store.GetEpisode(r.Context(), airing.OwnerID, airing.Slug)
	if err != nil {
		// The airing is already gone, which is the part that mattered.
		s.log.Error("un-aired an episode that is no longer there",
			"owner", airing.OwnerID, "episode", airing.Slug, "err", err)
		http.Redirect(w, r, "/admin/airings", http.StatusSeeOther)
		return
	}
	ep.AirBarred = true
	if err := s.store.UpdateEpisode(r.Context(), ep); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin/airings", http.StatusSeeOther)
}

// handleVouch puts the caller's name to an Aired Episode. Never their
// own: at the size of this station, self-vouching past a Bar of one
// would make every Bar decorative (ADR 0019).
func (s *server) handleVouch(w http.ResponseWriter, r *http.Request, u store.User) {
	airing, err := s.store.GetAiring(r.Context(), r.PathValue("airing"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if airing.OwnerID == u.ID {
		http.Error(w, "you cannot vouch for your own episode", http.StatusForbidden)
		return
	}
	err = s.store.AddVouch(r.Context(), store.Vouch{
		AiringID: airing.ID, UserID: u.ID, At: time.Now().UTC(),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/strands/"+airing.Strand, http.StatusSeeOther)
}

// handleUnvouch takes the caller's name back off. It does not retract
// anything already delivered: a settled Airing carries a frozen count,
// so feeds do not flap when someone changes their mind (ADR 0019).
func (s *server) handleUnvouch(w http.ResponseWriter, r *http.Request, u store.User) {
	airing, err := s.store.GetAiring(r.Context(), r.PathValue("airing"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.RemoveVouch(r.Context(), airing.ID, u.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/strands/"+airing.Strand, http.StatusSeeOther)
}

// handleFollow starts or adjusts a Follow. The same handler sets the Bar,
// because raising it is the same act as choosing it.
func (s *server) handleFollow(w http.ResponseWriter, r *http.Request, u store.User) {
	strand := r.PathValue("strand")
	st, err := s.store.GetStrand(r.Context(), strand)
	if err != nil {
		s.fail(w, err)
		return
	}
	// A Retired Strand keeps serving the people already subscribed but
	// takes no new followers — it is on its way out.
	if st.Retired {
		http.Error(w, "that strand is closed to new followers", http.StatusUnprocessableEntity)
		return
	}

	bar := store.DefaultBar
	if raw := r.FormValue("bar"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "bar must be a whole number", http.StatusBadRequest)
			return
		}
		if err := store.ValidateBar(n); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bar = n
	}
	err = s.store.PutFollow(r.Context(), store.Follow{
		UserID: u.ID, Strand: st.ID, Bar: bar, At: time.Now().UTC(),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/strands/"+st.ID, http.StatusSeeOther)
}

// handleUnfollow stops the deliveries. Unfollowing is the control here —
// Block and Mute keep their own meanings rather than being overloaded to
// do this job (ADR 0019).
func (s *server) handleUnfollow(w http.ResponseWriter, r *http.Request, u store.User) {
	strand := r.PathValue("strand")
	if err := s.store.DeleteFollow(r.Context(), u.ID, strand); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/strands/"+strand, http.StatusSeeOther)
}

// redirectToEpisode sends the browser back to the Episode Page it acted
// from — the signed-in address, never the capability one, so a
// listener's address bar never holds the key to their whole feed.
func (s *server) redirectToEpisode(w http.ResponseWriter, r *http.Request, u store.User, slug string) {
	http.Redirect(w, r, "/me/episodes/"+u.ID+"/"+slug, http.StatusSeeOther)
}
