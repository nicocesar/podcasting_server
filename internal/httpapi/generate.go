package httpapi

// The /me/generate surface (ADR 0009): a form that starts a Generation,
// a progress page that watches it, and a retry for failed ones. The
// pipeline itself lives in internal/generation; these handlers only
// create, read, and re-arm Generation records.

import (
	"errors"
	"fmt"
	"net/http"
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

// generatePage is the template data for the form.
type generatePage struct {
	Lengths   []int
	Freshness []generation.FreshnessOption
	Voices    []tts.Voice
	Error     string
	Topic     string
}

func (s *server) handleGeneratePage(w http.ResponseWriter, r *http.Request, _ store.User) {
	s.render(w, http.StatusOK, s.tmplGenerate, generatePage{
		Lengths:   generation.Lengths,
		Freshness: generation.FreshnessOptions,
		Voices:    tts.Voices,
	})
}

const maxTopicLen = 500

func (s *server) handleGenerateStart(w http.ResponseWriter, r *http.Request, u store.User) {
	retry := func(msg string) {
		s.render(w, http.StatusBadRequest, s.tmplGenerate, generatePage{
			Lengths:   generation.Lengths,
			Freshness: generation.FreshnessOptions,
			Voices:    tts.Voices,
			Error:     msg,
			Topic:     r.FormValue("topic"),
		})
	}

	topic := strings.TrimSpace(r.FormValue("topic"))
	if topic == "" || len(topic) > maxTopicLen {
		retry("The topic is required, up to 500 characters.")
		return
	}
	length, err := strconv.Atoi(r.FormValue("length"))
	if err != nil || !generation.ValidLength(length) {
		retry("Pick a length from the list.")
		return
	}
	freshness, err := strconv.Atoi(r.FormValue("freshness"))
	if err != nil || !generation.ValidFreshness(freshness) {
		retry("Pick a freshness window from the list.")
		return
	}
	language := r.FormValue("language")
	if _, ok := tts.VoiceFor(language); !ok {
		retry("Pick a language from the list.")
		return
	}

	id, err := randomHex(8)
	if err != nil {
		s.fail(w, err)
		return
	}
	g := store.Generation{
		UserID:        u.ID,
		ID:            id,
		Topic:         topic,
		LengthMinutes: length,
		FreshnessDays: freshness,
		Language:      language,
		Stage:         store.GenResearching,
		Active:        true,
		CreatedAt:     time.Now().UTC(),
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

// generationView adds the display bits the progress page and dashboard
// need on top of the persisted record.
type generationView struct {
	store.Generation
	StageLabel string `json:"stage_label"`
	StatsLabel string `json:"stats_label,omitempty"` // human-readable meter summary
	EpisodeURL string `json:"episode_url,omitempty"`
}

func (s *server) generationView(g store.Generation) generationView {
	v := generationView{Generation: g}
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
