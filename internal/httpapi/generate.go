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
	cards := make([]programCard, 0, len(generation.TemplateIDs))
	for _, id := range generation.TemplateIDs {
		tpl, _ := generation.TemplateByID(id)
		cards = append(cards, programCard{ID: tpl.ID, Name: tpl.Name, Tagline: tpl.Tagline})
	}
	s.render(w, http.StatusOK, s.tmplPrograms, struct{ Programs []programCard }{cards})
}

// castOption is one returning-cast choice on the stories form: a story
// episode in the caller's feed (own or shared) with extracted characters.
type castOption struct {
	Value string // "owner/slug"
	Label string // episode title + character names
}

// generatePage is the template data for a per-template form.
type generatePage struct {
	Template    generation.Template
	Lengths     []int
	Freshness   []generation.FreshnessOption
	AgeRanges   []generation.AgeRangeOption
	CastOptions []castOption
	Languages   []tts.Voice // one entry per language
	Providers   []string    // engine names, chain order; "" (Auto) is added in the template
	Error       string
	Topic       string
}

// pageTemplate resolves the {template} path segment ("" → news, the
// pre-template URL shape). Unknown ids are a 404.
func (s *server) pageTemplate(w http.ResponseWriter, r *http.Request) (generation.Template, bool) {
	tpl, ok := generation.TemplateByID(r.PathValue("template"))
	if !ok {
		http.Error(w, "no such program", http.StatusNotFound)
	}
	return tpl, ok
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
	s.render(w, http.StatusOK, s.tmplGenerate, page)
}

const maxTopicLen = 2000

func (s *server) handleGenerateStart(w http.ResponseWriter, r *http.Request, u store.User) {
	tpl, ok := s.pageTemplate(w, r)
	if !ok {
		return
	}
	retry := func(msg string) {
		page, err := s.generatePage(r, u, tpl)
		if err != nil {
			s.fail(w, err)
			return
		}
		page.Error = msg
		page.Topic = r.FormValue("topic")
		s.render(w, http.StatusBadRequest, s.tmplGenerate, page)
	}

	topic := strings.TrimSpace(r.FormValue("topic"))
	if topic == "" || len(topic) > maxTopicLen {
		retry("The " + strings.ToLower(tpl.TopicLabel) + " is required, up to 2000 characters.")
		return
	}
	length, err := strconv.Atoi(r.FormValue("length"))
	if err != nil || !generation.ValidLength(length) {
		retry("Pick a length from the list.")
		return
	}
	freshness := 0
	if tpl.HasFreshness {
		freshness, err = strconv.Atoi(r.FormValue("freshness"))
		if err != nil || !generation.ValidFreshness(freshness) {
			retry("Pick a freshness window from the list.")
			return
		}
	}
	ageRange := ""
	if tpl.HasAgeRange {
		ageRange = r.FormValue("age")
		if !generation.ValidAgeRange(ageRange) {
			retry("Pick an age range from the list.")
			return
		}
	}
	language := r.FormValue("language")
	if _, ok := tts.VoiceFor(language, ""); !ok {
		retry("Pick a language from the list.")
		return
	}
	voice := r.FormValue("voice")
	if _, ok := tts.VoiceFor(language, voice); voice == "" || !ok {
		retry("Pick a voice from the list.")
		return
	}
	provider := r.FormValue("provider")
	if provider != "" && !slices.Contains(s.generator.EngineNames(), provider) {
		retry("Pick a voice provider from the list.")
		return
	}
	var cast []store.Character
	if tpl.HasCast {
		if ref := r.FormValue("cast"); ref != "" {
			owner, slug, ok := strings.Cut(ref, "/")
			if !ok || s.inFeed(r, u, owner, slug) != nil {
				retry("Pick a returning cast from the list.")
				return
			}
			ep, err := s.store.GetEpisode(r.Context(), owner, slug)
			if err != nil || ep.Template != "stories" || len(ep.Characters) == 0 {
				retry("Pick a returning cast from the list.")
				return
			}
			cast = ep.Characters
		}
	}

	id, err := randomHex(8)
	if err != nil {
		s.fail(w, err)
		return
	}
	g := store.Generation{
		UserID:         u.ID,
		ID:             id,
		Template:       tpl.ID,
		Topic:          topic,
		LengthMinutes:  length,
		FreshnessDays:  freshness,
		AgeRange:       ageRange,
		SaveCharacters: tpl.HasSaveCharacters && r.FormValue("save_characters") != "",
		Cast:           cast,
		Language:       language,
		Voice:          voice,
		Provider:       provider,
		Stage:          store.GenResearching,
		Active:         true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.store.PutGeneration(r.Context(), g); err != nil {
		s.fail(w, err)
		return
	}
	s.generator.Kick(g)

	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/me/generations/"+id, http.StatusSeeOther)
		return
	}
	s.writeJSON(w, http.StatusCreated, g)
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
}

func (s *server) generationView(g store.Generation) generationView {
	v := generationView{Generation: g}
	if tpl, ok := generation.TemplateByID(g.Template); ok {
		v.TemplateName = tpl.Name
	}
	switch g.Stage {
	case store.GenResearching:
		v.StageLabel = "Researching & writing"
	case store.GenVoicing:
		v.StageLabel = "Voicing"
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
		s.render(w, http.StatusOK, s.tmplGeneration, s.generationView(g))
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
