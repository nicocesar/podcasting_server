package httpapi

// The /me/beats surface: the Beats a User has running, and the controls
// to edit, pause, or cancel one. Creating a Beat is not here — it is the
// checkbox on /me/generate, because a Beat is a Generation you asked to
// keep happening, and asking for it twice in two places would be two
// forms to keep in step.
//
// Session-only, like Credential Management (ADR 0010): a Beat spends
// money on its own schedule, so a leaked API Key must not be able to
// leave one behind.

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/store"
)

// maxBeatsPerUser bounds unattended spending. A Beat produces Episodes
// whether or not anyone listens, so the cheapest guard is a small number
// of them — see ADR 0016.
const maxBeatsPerUser = 5

// beatView is one row on the Beats page.
type beatView struct {
	store.Beat
	ProgramName string
	Cadence     string // "Every day"
	Status      string // "next in about 4 hours", "paused", …
	EditURL     string
	Failing     bool
}

// beatsPage is the template data for /me/beats.
type beatsPage struct {
	User  store.User
	Beats []beatView
	Max   int
	// ReturnTo is this page, so pausing or cancelling lands back on the
	// beat it touched rather than at the top of the list (ADR 0022).
	ReturnTo string
}

// newBeat builds the Beat a form asked to repeat. The request fields are
// copied, not referenced: every firing rebuilds an identical Generation,
// and nothing the User later deletes can hollow one out.
func newBeat(u store.User, tpl generation.Template, req generationRequest, id string, now time.Time) store.Beat {
	return store.Beat{
		UserID:         u.ID,
		ID:             id,
		Template:       tpl.ID,
		Topic:          req.Topic,
		LengthMinutes:  req.LengthMinutes,
		FreshnessDays:  req.FreshnessDays,
		AgeRange:       req.AgeRange,
		SaveCharacters: req.SaveCharacters,
		Cast:           req.Cast,
		Language:       req.Language,
		Voice:          req.Voice,
		Provider:       req.Provider,
		IntervalDays:   req.IntervalDays,
		FireAt:         req.FireAt,
		CreatedAt:      now,
	}
}


// beatCapError reports the message to show when the user may not have
// another Beat, or "" when they may.
func (s *server) beatCapError(r *http.Request, u store.User) string {
	beats, err := s.store.ListBeats(r.Context(), u.ID)
	if err != nil {
		return "Could not check your Beats. Try again."
	}
	if len(beats) >= maxBeatsPerUser {
		return fmt.Sprintf("You already have %d Beats, which is the limit. "+
			"Cancel one on the Beats page first.", maxBeatsPerUser)
	}
	return ""
}

// beatValues turns a stored Beat back into the form's values, so the edit
// page is the create page with everything filled in.
func beatValues(b store.Beat) generationRequest {
	return generationRequest{
		Topic:          b.Topic,
		LengthMinutes:  b.LengthMinutes,
		FreshnessDays:  b.FreshnessDays,
		AgeRange:       b.AgeRange,
		SaveCharacters: b.SaveCharacters,
		Cast:           b.Cast,
		Language:       b.Language,
		Voice:          b.Voice,
		Provider:       b.Provider,
		FireAt:         b.FireAt,
		Recur:          true,
		IntervalDays:   b.IntervalDays,
	}
}

// handleBeats lists the user's Beats, showing when each one is next due.
// Landing here no longer fires anything — since ADR 0028 that is the
// Tick's job, and a page a Beat's owner happens to open is not a clock.
// It does record that they were here, and revive a stalled run while
// somebody is watching.
func (s *server) handleBeats(w http.ResponseWriter, r *http.Request, u store.User) {
	views, err := s.beatViews(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, s.tmplBeats, beatsPage{User: u, Beats: views, Max: maxBeatsPerUser, ReturnTo: r.URL.RequestURI()})
	s.seen(u)
	s.resume(u)
}

// beatViews renders the user's Beats for display, newest-first as the
// store returns them.
func (s *server) beatViews(r *http.Request, u store.User) ([]beatView, error) {
	beats, err := s.store.ListBeats(r.Context(), u.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	loc, zoneErr := store.LoadZone(u.HomeZone)
	if zoneErr != nil {
		loc = time.UTC
	}
	views := make([]beatView, 0, len(beats))
	for _, b := range beats {
		tpl, _ := generation.TemplateByID(b.Template)
		views = append(views, beatView{
			Beat:        b,
			ProgramName: tpl.Name,
			Cadence:     cadenceLabel(b.IntervalDays),
			Status:      beatStatus(b, now, loc, u.HomeZone != ""),
			EditURL:     "/me/beats/" + b.ID + "/edit",
			Failing:     b.ConsecutiveFailures > 0,
		})
	}
	return views, nil
}

// cadenceLabel names an interval, falling back to a plain day count for a
// stored Beat whose cadence is no longer on the menu — a news Beat takes
// its cadence from the Freshness Window, which offers spans the interval
// list does not.
func cadenceLabel(days int) string {
	for _, o := range generation.IntervalOptions {
		if o.Days == days {
			return o.Label
		}
	}
	switch days {
	case 90:
		return "Every 3 months"
	case 365:
		return "Every year"
	}
	return fmt.Sprintf("Every %d days", days)
}

// beatStatus is the one line under a Beat that says where it stands.
//
// An Anchored Beat gets a wall time, because that is what its owner
// asked for and a vague span would be a worse answer than the exact one.
// A loose Beat keeps the vague span, because it has no time of day to be
// precise about.
func beatStatus(b store.Beat, now time.Time, loc *time.Location, hasZone bool) string {
	if b.Paused {
		if b.ConsecutiveFailures >= store.BeatFailureLimit {
			return fmt.Sprintf("paused after %d failures", b.ConsecutiveFailures)
		}
		return "paused"
	}
	if b.FireAt != "" && !hasZone {
		// Reachable only by clearing a Home Zone that Anchored Beats were
		// relying on. Said plainly, because the Beat has silently stopped.
		return "waiting for a timezone — set one in Settings"
	}
	if b.Due(now, loc) {
		return "due now"
	}
	next := b.NextAt(now, loc)
	if next.IsZero() {
		return "not scheduled"
	}
	if b.FireAt == "" {
		return "next in about " + roughDuration(next.Sub(now))
	}
	return "next " + whenWords(next, now, loc)
}

// whenWords names an Anchored firing the way a person would: the day it
// falls on and the time of day, read in the owner's own zone.
func whenWords(next, now time.Time, loc *time.Location) string {
	at := next.In(loc)
	clock := at.Format("15:04")
	today := now.In(loc)
	days := int(at.Truncate(24*time.Hour).Sub(today.Truncate(24*time.Hour)) / (24 * time.Hour))
	switch {
	case at.YearDay() == today.YearDay() && at.Year() == today.Year():
		return "today at " + clock
	case days <= 1:
		return "tomorrow at " + clock
	case days < 7:
		return at.Format("Monday") + " at " + clock
	default:
		return at.Format("2 Jan") + " at " + clock
	}
}

// roughDuration is a human span for a due time — deliberately vague,
// because a loose Beat has no time of day, only a cadence.
func roughDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", max(int(d.Minutes()), 1))
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// beatOf resolves the {id} path segment against the caller, 404ing on
// anything that is not theirs.
func (s *server) beatOf(w http.ResponseWriter, r *http.Request, u store.User) (store.Beat, bool) {
	b, err := s.store.GetBeat(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.renderNotFound(w, r)
			return b, false
		}
		s.fail(w, err)
		return b, false
	}
	return b, true
}

// handleBeatEdit renders the generate form filled in from the Beat,
// pointed at the Beat's own URL.
func (s *server) handleBeatEdit(w http.ResponseWriter, r *http.Request, u store.User) {
	b, ok := s.beatOf(w, r, u)
	if !ok {
		return
	}
	tpl, ok := generation.TemplateByID(b.Template)
	if !ok {
		s.renderNotFound(w, r)
		return
	}
	page, err := s.generatePage(r, u, tpl)
	if err != nil {
		s.fail(w, err)
		return
	}
	page.Values = beatValues(b)
	page.Beat = &b
	s.render(w, r, http.StatusOK, s.tmplGenerate, page)
}

// handleBeatUpdate replaces the Beat's request. It changes the future
// only: Episodes already published stay as they are, and a Generation
// already in flight keeps the frozen copy it started with.
func (s *server) handleBeatUpdate(w http.ResponseWriter, r *http.Request, u store.User) {
	b, ok := s.beatOf(w, r, u)
	if !ok {
		return
	}
	tpl, ok := generation.TemplateByID(b.Template)
	if !ok {
		s.renderNotFound(w, r)
		return
	}
	req, msg := s.parseGenerationForm(r, u, tpl)
	if msg == "" && !req.Recur {
		// Unchecking the box on an edit is ambiguous — it could mean
		// "stop" or be a slip — so say what the two real options are
		// rather than silently deleting something.
		msg = "Leave the box checked to keep this Beat. To stop it, " +
			"pause or cancel it on the Beats page."
	}
	if msg != "" {
		page, err := s.generatePage(r, u, tpl)
		if err != nil {
			s.fail(w, err)
			return
		}
		page.Error = msg
		page.Values = req
		page.Beat = &b
		s.render(w, r, http.StatusBadRequest, s.tmplGenerate, page)
		return
	}

	// An Anchor added on an edit needs a zone, exactly as one added on
	// creation does.
	if req.FireAt != "" {
		var err error
		if u, err = s.ensureHomeZone(r, u, req.BrowserZone); err != nil {
			s.fail(w, err)
			return
		}
	}
	updated := newBeat(u, tpl, req, b.ID, b.CreatedAt)
	// The history and the clock survive an edit: retuning the wording of
	// a Topic is not a reason to re-cover a week you already heard.
	//
	// The Anchor survives too, and a changed FireAt needs no re-phasing:
	// the next occurrence re-reads the wall clock from the Beat, so a
	// 07:00 Anchor moved to 09:00 simply comes round at 09:00 next time.
	updated.AnchorAt = b.AnchorAt
	updated.LastFiredAt = b.LastFiredAt
	updated.LastSucceededAt = b.LastSucceededAt
	updated.EpisodeCount = b.EpisodeCount
	updated.Paused = b.Paused
	updated.ConsecutiveFailures = b.ConsecutiveFailures
	updated.LastError = b.LastError
	if err := s.store.PutBeat(r.Context(), updated); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/me/beats"), http.StatusSeeOther)
}

// handleBeatPause stops a Beat without losing it.
func (s *server) handleBeatPause(w http.ResponseWriter, r *http.Request, u store.User) {
	b, ok := s.beatOf(w, r, u)
	if !ok {
		return
	}
	b.Paused = true
	if err := s.store.PutBeat(r.Context(), b); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/me/beats"), http.StatusSeeOther)
}

// handleBeatResume restarts a Beat, re-phasing its clock to now. Coming
// back from a pause should give a fresh Episode, not one stretched over
// the whole holiday — and it clears a failure streak, since resuming is
// the User saying they have dealt with whatever broke.
func (s *server) handleBeatResume(w http.ResponseWriter, r *http.Request, u store.User) {
	b, ok := s.beatOf(w, r, u)
	if !ok {
		return
	}
	now := time.Now().UTC()
	b.Paused = false
	// The Anchor re-phases too, or a Beat paused for a month would come
	// back with a month-old Anchor and be due the instant it resumed. For
	// an Anchored Beat this reads as "start counting from today", so the
	// next one lands at its own time of day tomorrow rather than now.
	b.AnchorAt = now
	b.LastFiredAt = now
	b.LastSucceededAt = now
	b.ConsecutiveFailures = 0
	b.LastError = ""
	if err := s.store.PutBeat(r.Context(), b); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/me/beats"), http.StatusSeeOther)
}

// handleBeatCancel removes the Beat. Episodes it already published are
// ordinary Episodes and stay in the feed; a Generation still running is
// left to finish, since its request was frozen at firing time.
func (s *server) handleBeatCancel(w http.ResponseWriter, r *http.Request, u store.User) {
	if _, ok := s.beatOf(w, r, u); !ok {
		return
	}
	if err := s.store.DeleteBeat(r.Context(), u.ID, r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/me/beats"), http.StatusSeeOther)
}

