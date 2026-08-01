package httpapi

// The /me/generate surface (ADR 0009): a form that starts a Generation,
// a progress page that watches it, and a retry for failed ones. The
// pipeline itself lives in internal/generation; these handlers only
// create, read, and re-arm Generation records.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

// generating gates the Generation endpoints on the feature being
// configured (ANTHROPIC_API_KEY at boot).
func (s *server) generating(h authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, u store.User) {
		if s.generator == nil {
			http.Error(w, "episode generation is not configured on this server", http.StatusServiceUnavailable)
			return
		}
		h(w, r, u)
	}
}

// programCard is one template on the chooser page.
type programCard struct {
	ID      string
	Name    string
	Tagline string
}

// handleGenerateChooser renders "what's on the program?": one card per
// template, each linking to its own form page.
func (s *server) handleGenerateChooser(w http.ResponseWriter, r *http.Request, _ store.User) {
	ids := s.generator.AvailableTemplates()
	cards := make([]programCard, 0, len(ids))
	for _, id := range ids {
		tpl, _ := generation.TemplateByID(id)
		cards = append(cards, programCard{ID: tpl.ID, Name: tpl.Name, Tagline: tpl.Tagline})
	}
	s.render(w, r, http.StatusOK, s.tmplPrograms, struct{ Programs []programCard }{cards})
}

// castOption is one returning-cast choice on the stories form: a story
// episode in the caller's feed (own or shared) with extracted characters.
type castOption struct {
	Value string // "owner/slug"
	Label string // episode title + character names
}

// generatePage is the template data for a per-template form. The same
// page serves three jobs: a fresh form, a form redisplayed with an error,
// and the edit form of an existing Beat.
type generatePage struct {
	Template    generation.Template
	Lengths     []int
	Freshness   []generation.FreshnessOption
	Intervals   []generation.IntervalOption
	AgeRanges   []generation.AgeRangeOption
	CastOptions []castOption
	Languages   []tts.Voice // one entry per language
	Providers   []string    // engine names, chain order; "" (Auto) is added in the template
	Error       string

	// Values prefills every control. Zero values select nothing, which
	// leaves the browser on each list's first option — the behaviour the
	// form had before it could be prefilled at all.
	Values generationRequest
	// Beat is set only when editing one: it redirects the form at the
	// Beat's own URL and changes what the button says.
	Beat *store.Beat
}

// generationRequest is the set of fields /me/generate collects, parsed
// and validated in one place. A Generation is built from it, and so is
// the Beat that repeats it.
type generationRequest struct {
	Topic          string
	LengthMinutes  int
	FreshnessDays  int
	AgeRange       string
	SaveCharacters bool
	Cast           []store.Character
	CastRef        string // "owner/slug" the Cast came from; trace only
	Language       string
	Voice          string
	Provider       string

	// Recur asks for a Beat, and IntervalDays is its cadence — equal to
	// FreshnessDays for a template that derives it.
	Recur        bool
	IntervalDays int
}

// pageTemplate resolves the {template} path segment ("" → news, the
// pre-template URL shape). Unknown ids are a 404.
func (s *server) pageTemplate(w http.ResponseWriter, r *http.Request) (generation.Template, bool) {
	tpl, ok := generation.TemplateByID(r.PathValue("template"))
	if !ok || !slices.Contains(s.generator.AvailableTemplates(), tpl.ID) {
		// Hiding the chooser card is not enough: a template this instance
		// cannot produce must 404 on its own URL too, or a bookmark walks
		// straight past the filter.
		http.Error(w, "no such program", http.StatusNotFound)
		return tpl, false
	}
	return tpl, true
}

// castOptions lists the reusable casts for the stories form: story
// episodes in u's feed — own and shared in, since characters live on the
// canonical Episode (ADR 0006) — that have an extracted cast.
func (s *server) castOptions(r *http.Request, u store.User) ([]castOption, error) {
	entries, err := s.feedEntries(r, u, "", "")
	if err != nil {
		return nil, err
	}
	opts := []castOption{}
	for _, e := range entries {
		if e.Template != "stories" || len(e.Characters) == 0 {
			continue
		}
		names := make([]string, len(e.Characters))
		for i, c := range e.Characters {
			names[i] = c.Name
		}
		opts = append(opts, castOption{
			Value: e.OwnerID + "/" + e.Slug,
			Label: e.Title + " — " + strings.Join(names, ", "),
		})
	}
	return opts, nil
}

func (s *server) generatePage(r *http.Request, u store.User, tpl generation.Template) (generatePage, error) {
	page := generatePage{
		Template:  tpl,
		Lengths:   generation.Lengths,
		Freshness: generation.FreshnessOptions,
		Intervals: generation.IntervalOptions,
		AgeRanges: generation.AgeRanges,
		Languages: tts.Languages(),
		Providers: s.generator.EngineNames(),
	}
	if tpl.HasCast {
		opts, err := s.castOptions(r, u)
		if err != nil {
			return page, err
		}
		page.CastOptions = opts
	}
	return page, nil
}

func (s *server) handleGeneratePage(w http.ResponseWriter, r *http.Request, u store.User) {
	tpl, ok := s.pageTemplate(w, r)
	if !ok {
		return
	}
	page, err := s.generatePage(r, u, tpl)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, s.tmplGenerate, page)
}

const maxTopicLen = 2000

// parseGenerationForm validates the submitted form against the template's
// field flags. It returns the parsed request, or a message to show the
// user with the form redisplayed — the caller decides how to render it,
// because the same parse serves both starting a Generation and editing a
// Beat.
func (s *server) parseGenerationForm(r *http.Request, u store.User, tpl generation.Template) (generationRequest, string) {
	var req generationRequest

	req.Topic = strings.TrimSpace(r.FormValue("topic"))
	if req.Topic == "" || len(req.Topic) > maxTopicLen {
		return req, "The " + strings.ToLower(tpl.TopicLabel) + " is required, up to 2000 characters."
	}
	length, err := strconv.Atoi(r.FormValue("length"))
	if err != nil || !generation.ValidLength(length) {
		return req, "Pick a length from the list."
	}
	req.LengthMinutes = length
	if tpl.HasFreshness {
		freshness, err := strconv.Atoi(r.FormValue("freshness"))
		if err != nil || !generation.ValidFreshness(freshness) {
			return req, "Pick a freshness window from the list."
		}
		req.FreshnessDays = freshness
	}
	if tpl.HasAgeRange {
		req.AgeRange = r.FormValue("age")
		if !generation.ValidAgeRange(req.AgeRange) {
			return req, "Pick an age range from the list."
		}
	}
	req.Language = r.FormValue("language")
	if _, ok := tts.VoiceFor(req.Language, ""); !ok {
		return req, "Pick a language from the list."
	}
	// A composed piece has no narrator, so the form does not offer these
	// and they stay empty on the Generation — nothing downstream resolves
	// a Voice for it.
	if !tpl.IsMusic {
		req.Voice = r.FormValue("voice")
		if _, ok := tts.VoiceFor(req.Language, req.Voice); req.Voice == "" || !ok {
			return req, "Pick a voice from the list."
		}
		req.Provider = r.FormValue("provider")
		if req.Provider != "" && !slices.Contains(s.generator.EngineNames(), req.Provider) {
			return req, "Pick a voice provider from the list."
		}
	}
	req.SaveCharacters = tpl.HasSaveCharacters && r.FormValue("save_characters") != ""

	if tpl.HasCast {
		if ref := r.FormValue("cast"); ref != "" {
			req.CastRef = ref
			owner, slug, ok := strings.Cut(ref, "/")
			if !ok || s.inFeed(r, u, owner, slug) != nil {
				return req, "Pick a returning cast from the list."
			}
			ep, err := s.store.GetEpisode(r.Context(), owner, slug)
			if err != nil || ep.Template != "stories" || len(ep.Characters) == 0 {
				return req, "Pick a returning cast from the list."
			}
			req.Cast = ep.Characters
		}
	}

	// The Beat half. The checkbox is hidden client-side for a Timeless
	// briefing, but that is a convenience; this is the rule.
	req.Recur = r.FormValue("recur") != ""
	if req.Recur {
		switch {
		case tpl.DerivesInterval && req.FreshnessDays == 0:
			return req, "A timeless topic isn't tied to the news, so there's nothing " +
				"to keep covering. Pick a freshness window, or uncheck the box."
		case tpl.DerivesInterval:
			req.IntervalDays = req.FreshnessDays
		default:
			interval, err := strconv.Atoi(r.FormValue("interval"))
			if err != nil || !generation.ValidInterval(interval) {
				return req, "Pick how often it should repeat."
			}
			req.IntervalDays = interval
		}
	}
	return req, ""
}

// newGeneration builds the Generation a request asks for. beatID is empty
// for one a User started by hand.
func newGeneration(u store.User, tpl generation.Template, req generationRequest, id, beatID string, now time.Time) store.Generation {
	g := store.Generation{
		UserID:         u.ID,
		ID:             id,
		BeatID:         beatID,
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
		Stage:          store.GenResearching,
		Active:         true,
		CreatedAt:      now,
	}
	// Traced here rather than in the runner because it happens exactly
	// once, at creation: a resumed run would re-emit it on every restart.
	// CastRef is the source episode, which the frozen Cast itself loses.
	if len(req.Cast) > 0 {
		g.AppendTrace(store.TraceEntry{
			At: now, Level: store.LevelInfo, Stage: g.Stage,
			Event: "cast.reused", Message: "reusing a returning cast",
			Detail: castDetail(req.CastRef, req.Cast),
		})
	}
	return g
}

// castDetail renders the trace detail for a reused cast: the source
// episode ref, which the frozen Cast on the Generation itself does not
// keep, plus who came back.
func castDetail(ref string, chars []store.Character) string {
	names := make([]string, len(chars))
	for i, c := range chars {
		names[i] = c.Name
	}
	b, err := json.Marshal(map[string]any{
		"source": ref, "count": len(chars), "names": strings.Join(names, ", "),
	})
	if err != nil {
		return ""
	}
	return string(b)
}

func (s *server) handleGenerateStart(w http.ResponseWriter, r *http.Request, u store.User) {
	tpl, ok := s.pageTemplate(w, r)
	if !ok {
		return
	}
	req, msg := s.parseGenerationForm(r, u, tpl)
	if req.Recur {
		// A Beat is session-only, and this is where one is born. The
		// /me/beats routes have always been s.session, but a Beat is not
		// created there — it is created by the recur checkbox on this
		// form, which rides the generate route and so accepts an API Key.
		// ADR 0016 stated the invariant and this path quietly broke it.
		//
		// The reasoning is ADR 0010's and ADR 0016's together: a Beat
		// spends money on its own schedule, so a leaked Generator
		// credential must not be able to leave one running. That matters
		// more since ADR 0028 than it did before — Beats used to fire only
		// when traffic happened to arrive, and now they fire on a clock.
		//
		// Refused the way s.session refuses, rather than as a form error:
		// the caller is a program, and a rendered HTML form is not an
		// answer it can use.
		if _, ok := s.bearerUser(r); ok {
			http.Error(w, "a Beat requires a browser session, not an API key", http.StatusForbidden)
			return
		}
	}
	if msg == "" && req.Recur {
		// Checked before anything is created, so the cap is reported on the
		// form rather than after an Episode already exists.
		msg = s.beatCapError(r, u)
	}
	if msg != "" {
		s.retryGenerate(w, r, u, tpl, req, msg)
		return
	}

	now := time.Now().UTC()
	id, err := randomHex(8)
	if err != nil {
		s.fail(w, err)
		return
	}
	var beatID string
	if req.Recur {
		beatID, err = randomHex(8)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	g := newGeneration(u, tpl, req, id, beatID, now)
	if err := s.store.PutGeneration(r.Context(), g); err != nil {
		s.fail(w, err)
		return
	}
	if req.Recur {
		// The clock starts now: this Generation is the Beat's first
		// Episode, so the next one is due a full interval from here.
		b := newBeat(u, tpl, req, beatID, now)
		b.LastFiredAt = now
		if err := s.store.PutBeat(r.Context(), b); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.generator.Kick(g)

	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/me/generations/"+id, http.StatusSeeOther)
		return
	}
	s.writeJSON(w, http.StatusCreated, g)
}

// retryGenerate redisplays the form with an error, keeping what was
// filled in so a rejected submission never costs the user their typing.
func (s *server) retryGenerate(w http.ResponseWriter, r *http.Request, u store.User, tpl generation.Template, req generationRequest, msg string) {
	page, err := s.generatePage(r, u, tpl)
	if err != nil {
		s.fail(w, err)
		return
	}
	page.Error = msg
	page.Values = req
	s.render(w, r, http.StatusBadRequest, s.tmplGenerate, page)
}

// handleEpisodeCharacters backfills the cast of one of the caller's own
// story episodes: the extraction the "save characters" checkbox would
// have run, from the Generation's stored Script.
func (s *server) handleEpisodeCharacters(w http.ResponseWriter, r *http.Request, u store.User) {
	slug := r.PathValue("slug")
	ep, err := s.store.GetEpisode(r.Context(), u.ID, slug)
	if err != nil {
		s.fail(w, err)
		return
	}
	if ep.Template != "stories" {
		http.Error(w, "not a story episode", http.StatusConflict)
		return
	}
	// The script lives on the Generation that published this slug.
	gens, err := s.store.ListGenerations(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	scriptText := ""
	for _, g := range gens {
		if g.EpisodeSlug != slug || g.Script == "" {
			continue
		}
		var script generation.Script
		if err := json.Unmarshal([]byte(g.Script), &script); err == nil {
			scriptText = script.Script
			break
		}
	}
	if scriptText == "" {
		http.Error(w, "no script on record for this episode", http.StatusNotFound)
		return
	}
	chars, err := s.generator.ExtractCharacters(r.Context(), scriptText)
	if err != nil {
		s.log.Error("character backfill failed", "owner", u.ID, "slug", slug, "err", err)
		http.Error(w, "character extraction failed", http.StatusBadGateway)
		return
	}
	ep.Characters = chars
	if err := s.store.UpdateEpisode(r.Context(), ep); err != nil {
		s.fail(w, err)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return
	}
	s.writeJSON(w, http.StatusOK, ep)
}

// generationView adds the display bits the progress page and dashboard
// need on top of the persisted record.
type generationView struct {
	store.Generation
	TemplateName string `json:"template_name"` // program display name
	StageLabel   string `json:"stage_label"`
	StatsLabel   string `json:"stats_label,omitempty"` // human-readable meter summary
	EpisodeURL   string `json:"episode_url,omitempty"`

	// Progress-page wording, resolved per template so the page describes
	// the program the listener actually asked for rather than the news
	// briefing the pipeline was first written for.
	ProgressTitle string      `json:"progress_title,omitempty"`
	Stages        []stageStep `json:"stages,omitempty"`
	Detail        string      `json:"detail,omitempty"` // the meta line's middle clause
}

// stageStep is one entry in the progress checklist. Key is the stage id
// the page's script keys off; only Label varies by template.
type stageStep struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

func (s *server) generationView(g store.Generation) generationView {
	v := generationView{Generation: g}
	tpl, _ := generation.TemplateByID(g.Template)
	v.TemplateName = tpl.Name
	v.ProgressTitle = tpl.ProgressTitle
	v.Stages = []stageStep{
		{store.GenResearching, tpl.PlanStage},
		{store.GenVoicing, tpl.AudioStage},
		{store.GenPublishing, "Publishing"},
		{store.GenDone, "In your feed"},
	}
	v.Detail = generationDetail(g, tpl)
	switch g.Stage {
	case store.GenResearching:
		v.StageLabel = tpl.PlanStage
	case store.GenVoicing:
		v.StageLabel = tpl.AudioStage
		// For music the count is movements rather than text chunks, which
		// is the honest unit either way: pieces of audio still to make.
		if g.TotalChunks > 0 {
			v.StageLabel += " (" + strconv.Itoa(g.VoicedChunks) + "/" + strconv.Itoa(g.TotalChunks) + ")"
		}
	case store.GenPublishing:
		v.StageLabel = "Publishing"
	case store.GenDone:
		v.StageLabel = "Published"
	case store.GenFailed:
		v.StageLabel = "Failed"
	default:
		v.StageLabel = g.Stage
	}
	if g.EpisodeSlug != "" {
		v.EpisodeURL = "/me"
	}
	v.StatsLabel = statsLabel(g)
	return v
}

// generationDetail renders the meta line's middle clause: whatever
// distinguishes this request beyond topic and length. Driven by the
// template's form flags rather than its id, so a template that collects
// neither an age range nor a freshness window — the ambient one — simply
// has nothing to say here, instead of claiming to be "timeless".
func generationDetail(g store.Generation, tpl generation.Template) string {
	switch {
	case tpl.HasAgeRange:
		if g.AgeRange == "" || g.AgeRange == "all" {
			return "for all ages"
		}
		return "for ages " + g.AgeRange
	case tpl.HasFreshness:
		if g.FreshnessDays > 0 {
			return "last " + strconv.Itoa(g.FreshnessDays) + " days"
		}
		return "timeless"
	}
	return ""
}

// statsLabel renders the Generation's meters (raw counts; dollars live on
// /admin/costs) into one line for the progress page. Empty until the
// first meter lands.
func statsLabel(g store.Generation) string {
	var parts []string
	if g.SessionsCount > 0 {
		s := fmt.Sprintf("%d in / %d out tokens", g.InputTokens, g.OutputTokens)
		if g.CacheReadTokens > 0 {
			s += fmt.Sprintf(" (+%d cached)", g.CacheReadTokens)
		}
		s += fmt.Sprintf(" · %d session", g.SessionsCount)
		if g.SessionsCount > 1 {
			s += "s"
		}
		parts = append(parts, s)
	}
	if g.TTSAttempts > 0 {
		s := fmt.Sprintf("%d chars", g.TTSCharacters)
		if g.TTSEngine != "" {
			s += " via " + g.TTSEngine
		}
		if g.TTSAttempts > 1 {
			s += fmt.Sprintf(" (%d engine attempts)", g.TTSAttempts)
		}
		parts = append(parts, s)
	}
	if g.MusicCalls > 0 {
		// Minutes, not milliseconds: this line is read by a person, and
		// the duration is the thing that costs.
		s := fmt.Sprintf("%.0f min composed", float64(g.MusicMillis)/60000)
		if g.MusicModel != "" {
			s += " via " + g.MusicModel
		}
		if g.MusicCalls > 0 {
			s += fmt.Sprintf(" (%d call", g.MusicCalls)
			if g.MusicCalls > 1 {
				s += "s"
			}
			s += ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

func (s *server) loadGeneration(w http.ResponseWriter, r *http.Request, u store.User) (store.Generation, bool) {
	g, err := s.store.GetGeneration(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			s.fail(w, err)
		}
		return store.Generation{}, false
	}
	return g, true
}

// handleGeneration answers browsers with the progress page and everything
// else (the page's own polling included) with JSON.
func (s *server) handleGeneration(w http.ResponseWriter, r *http.Request, u store.User) {
	g, ok := s.loadGeneration(w, r, u)
	if !ok {
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		s.render(w, r, http.StatusOK, s.tmplGeneration, s.generationView(g))
		return
	}
	s.writeJSON(w, http.StatusOK, s.generationView(g))
}

func (s *server) handleGenerationRetry(w http.ResponseWriter, r *http.Request, u store.User) {
	g, ok := s.loadGeneration(w, r, u)
	if !ok {
		return
	}
	g, err := s.generator.Retry(r.Context(), g)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/me/generations/"+g.ID, http.StatusSeeOther)
		return
	}
	s.writeJSON(w, http.StatusOK, s.generationView(g))
}

// handleGenerationDismiss clears a failed Generation off the caller's
// Dashboard. The record survives — still retryable at its own URL, still
// carrying the meters an admin bills from (ADR 0011) — so this is a read
// receipt, not a delete.
//
// Only a failure can be dismissed. An in-flight run would come back on
// the next poll anyway, and letting one be hidden would mean a
// Generation still spending money with no row saying so.
func (s *server) handleGenerationDismiss(w http.ResponseWriter, r *http.Request, u store.User) {
	g, ok := s.loadGeneration(w, r, u)
	if !ok {
		return
	}
	if g.Stage != store.GenFailed {
		http.Error(w, "only a failed generation can be dismissed", http.StatusConflict)
		return
	}
	// Already dismissed is success, not a conflict: a double-submitted
	// form should land on the Dashboard, not on an error page.
	if !g.Dismissed {
		g.Dismissed = true
		if err := s.store.PutGeneration(r.Context(), g); err != nil {
			s.fail(w, err)
			return
		}
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, returnTo(r, "/me"), http.StatusSeeOther)
		return
	}
	s.writeJSON(w, http.StatusOK, s.generationView(g))
}
