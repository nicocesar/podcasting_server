package httpapi

// The Home Zone: the IANA zone a Beat's Anchor is read in (ADR 0030).
//
// Home, and deliberately not current. The station learns it once, from
// the browser that first asked for a time of day, and changes it only
// when its owner says so. A phone reporting Asia/Tokyo means its owner is
// travelling, not that their mornings have moved — and following the
// traveller would mean a zone change mid-cycle, which can only ever
// deliver two Episodes in one day or none.

import (
	"net/http"
	"strings"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// ensureHomeZone gives a User their first Home Zone from what the browser
// reported, and returns the User as it now stands.
//
// Only ever the first. A User who already has one keeps it, which is what
// makes travel safe: the generate form reports a zone on every submission
// and this ignores all but the earliest.
func (s *server) ensureHomeZone(r *http.Request, u store.User, zone string) (store.User, error) {
	if u.HomeZone != "" || zone == "" {
		return u, nil
	}
	if err := store.ValidateHomeZone(zone); err != nil {
		// A browser talking nonsense is not worth failing a request over;
		// the form has already refused an Anchor without a usable zone.
		s.log.Warn("home zone: browser reported an unusable zone", "user", u.ID, "zone", zone)
		return u, nil
	}
	u.HomeZone = zone
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		return u, err
	}
	return u, nil
}

// handleSetTimezone changes the Home Zone, which is the deliberate act
// the Dashboard's banner offers when the browser disagrees with it.
//
// Session-only, and not folded into PUT /me with the rest of the profile.
// PUT /me takes an API Key, and moving somebody's Home Zone re-times
// every Anchored Beat they own — westward far enough and today's Anchor
// has not happened yet, so it fires a second time. That is unattended
// spend from a Generator credential, which ADR 0010 and ADR 0016 keep out
// of reach, and which the recur checkbox got wrong for a year.
func (s *server) handleSetTimezone(w http.ResponseWriter, r *http.Request, u store.User) {
	zone := strings.TrimSpace(r.FormValue("timezone"))
	if err := store.ValidateHomeZone(zone); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.HomeZone = zone
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/me/settings"), http.StatusSeeOther)
}

// zoneBanner is the traveller's prompt on the Dashboard: an offer to
// change the Home Zone when the browser is somewhere else. An offer and
// never an action — the station does not move somebody's morning because
// their laptop woke up in another country.
//
// The comparison happens in the browser, because the browser is the only
// side that knows its own zone and a GET carries nothing that would tell
// the server. So this ships the stored zone and lets homezone.js decide
// whether to reveal anything, which also means a reader with JS off is
// never shown a banner they could not have acted on.
type zoneBanner struct {
	// Home is the stored zone. Empty means none is set, and there is
	// nothing to disagree with.
	Home string
	// Anchored is how many of the User's Beats actually care about a
	// zone. Zero means a disagreement is real but harmless — a traveller
	// with no Anchored Beat has nothing to decide — so the banner stays
	// out of the way.
	Anchored int
	// ReturnTo brings the reader back to the Dashboard after the change,
	// per ADR 0022.
	ReturnTo string
}

// Relevant reports whether the banner is worth rendering at all, before
// the browser has had its say.
func (z zoneBanner) Relevant() bool { return z.Home != "" && z.Anchored > 0 }

func zoneBannerFor(u store.User, beats []beatView, returnTo string) zoneBanner {
	z := zoneBanner{Home: u.HomeZone, ReturnTo: returnTo}
	for _, b := range beats {
		if b.FireAt != "" {
			z.Anchored++
		}
	}
	return z
}
