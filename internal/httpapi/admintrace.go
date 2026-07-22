package httpapi

// The admin execution-trace view: what actually happened inside one
// Generation, read off the record instead of out of Cloud Logging.
//
// It exists because a silent TTS fallback — a listener asked for one
// voice provider, the chain quietly used another — was only ever visible
// through `gcloud logging read`. The trace is persisted by the runner
// (internal/generation/trace.go); this is the surface that shows it.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/store"
)

// adminTraceRow is one trace entry, rendered. Detail arrives as a JSON
// object (a Datastore-imposed shape, see store.TraceEntry) and is flattened
// back into sorted k=v chips for display.
type adminTraceRow struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Stage   string    `json:"stage,omitempty"`
	Event   string    `json:"event"`
	Message string    `json:"message"`
	URL     string    `json:"url,omitempty"`
	Chips   []string  `json:"detail,omitempty"`
	// Notable drives the row styling: warn and error earn the ON-AIR red,
	// notice the accent, info stays quiet.
	Notable bool `json:"notable"`
}

type adminGenerationView struct {
	store.Generation
	TemplateName string          `json:"template_name,omitempty"`
	StatsLabel   string          `json:"stats_label,omitempty"`
	Trace        []adminTraceRow `json:"trace"`
	TraceDropped int             `json:"trace_dropped,omitempty"`
	// Worst is the highest level present, so the page can say at a glance
	// whether this run degraded.
	Worst string `json:"worst"`
}

// handleAdminGeneration renders one Generation's execution trace. Content
// negotiated like the owner-facing progress page: HTML for a browser,
// the same payload as JSON for anything scripted.
func (s *server) handleAdminGeneration(w http.ResponseWriter, r *http.Request, _ store.User) {
	g, err := s.store.GetGeneration(r.Context(), r.PathValue("user"), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	v := adminGenerationView{
		Generation:   g,
		StatsLabel:   statsLabel(g),
		Trace:        make([]adminTraceRow, 0, len(g.Trace)),
		TraceDropped: g.TraceDropped,
		Worst:        store.LevelInfo,
	}
	if tpl, ok := generation.TemplateByID(g.Template); ok {
		v.TemplateName = tpl.Name
	}
	for _, e := range g.Trace {
		notable := e.Level == store.LevelWarn || e.Level == store.LevelError
		v.Trace = append(v.Trace, adminTraceRow{
			At: e.At, Level: e.Level, Stage: e.Stage, Event: e.Event,
			Message: e.Message, URL: e.URL, Chips: detailChips(e.Detail),
			Notable: notable,
		})
		if levelRank(e.Level) > levelRank(v.Worst) {
			v.Worst = e.Level
		}
	}

	if wantsHTML(r) {
		s.render(w, http.StatusOK, s.tmplAdminGeneration, v)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

func levelRank(level string) int {
	switch level {
	case store.LevelError:
		return 3
	case store.LevelWarn:
		return 2
	case store.LevelNotice:
		return 1
	}
	return 0
}

// detailChips flattens the Detail JSON object into sorted "k=v" strings.
// Unparseable detail is shown raw rather than dropped: a malformed chip
// is still evidence, and silently hiding it would defeat the point.
func detailChips(detail string) []string {
	if detail == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(detail), &m); err != nil {
		return []string{detail}
	}
	chips := make([]string, 0, len(m))
	for k, val := range m {
		chips = append(chips, k+"="+chipValue(val))
	}
	sort.Strings(chips)
	return chips
}

func chipValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// Whole numbers are counts (attempts, chunks); don't render "2.00".
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
