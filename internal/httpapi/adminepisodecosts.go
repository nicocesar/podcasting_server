package httpapi

// GET /admin/costs/episodes prices each Generation in dollars while
// keeping the costs-no-price-table decision: for every day and token
// type, the effective rate is the org's billed dollars (cost report) ÷
// the org's tokens (usage report) — Anthropic's own realized price, so
// introductory pricing, price changes, and model switches are reflected
// automatically. An episode then pays for exactly the tokens its meters
// recorded, at those rates. The day's small "session_usage" platform fee
// is split across that day's episodes by sessions_count (approximate if
// other managed-agent traffic shares the org). Days the cost report has
// not posted yet — it lags a few hours — come back as "pending" with a
// null cost. TTS is a different vendor's bill and stays raw characters.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

const costDay = "2006-01-02"

// dayLedger is one day of the org's bill: dollars and tokens per token
// kind, from which the day's effective $/token falls out.
type dayLedger struct {
	dollars map[string]float64 // kind → billed $
	tokens  map[string]int64   // kind → org-wide tokens
	session float64            // session_usage $ (no token denominator)
}

// costKind buckets both admin reports into the four meters a Generation
// stores. The cache_creation.* variants (5m and 1h TTLs) blend into one
// cache_write rate because the Generation meter does not distinguish.
func costKind(costType, tokenType string) string {
	if costType == "session_usage" {
		return "session"
	}
	switch {
	case tokenType == "uncached_input_tokens":
		return "input"
	case tokenType == "output_tokens":
		return "output"
	case tokenType == "cache_read_input_tokens":
		return "cache_read"
	case strings.HasPrefix(tokenType, "cache_creation"):
		return "cache_write"
	}
	return "" // web search, unknown lines: not episode token spend
}

// episodeCost is one Generation priced at the day's effective rates.
type episodeCost struct {
	User          string           `json:"user"`
	ID            string           `json:"id"`
	Topic         string           `json:"topic"`
	Stage         string           `json:"stage"`
	EpisodeSlug   string           `json:"episode_slug,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	SessionsCount int              `json:"sessions_count"`
	Tokens        map[string]int64 `json:"tokens"`
	TTSEngine     string           `json:"tts_engine,omitempty"`
	TTSCharacters int              `json:"tts_characters,omitempty"`
	// The ElevenLabs meters, in their own units. They carry no dollars
	// and never will from here: ElevenLabs sells a monthly allowance,
	// not per-request billing, so a figure in this row would be our
	// arithmetic rather than their invoice (ADR 0026).
	MusicMillis int    `json:"music_millis,omitempty"`
	MusicCalls  int    `json:"music_calls,omitempty"`
	MusicModel  string `json:"music_model,omitempty"`
	// The performed-story meters. A story draws on the same allowance
	// three separate ways at once, which is why these are here rather
	// than folded into the two above.
	DialogueRequests int                `json:"dialogue_requests,omitempty"`
	SFXGenerated     int                `json:"sfx_generated,omitempty"`
	SFXCacheHits     int                `json:"sfx_cache_hits,omitempty"`
	CostUSD          *float64           `json:"cost_usd"` // null while pending
	BreakdownUSD     map[string]float64 `json:"breakdown_usd,omitempty"`
	// Pricing is "reconciled" (all consumed token kinds had a posted
	// rate for the day) or "pending" (the cost report has not caught
	// up; retry later).
	Pricing string `json:"pricing"`
}

func (s *server) handleAdminEpisodeCosts(w http.ResponseWriter, r *http.Request) {
	if s.adminAPI == nil {
		http.Error(w, "cost reporting is not configured on this server (set ANTHROPIC_ADMIN_KEY)", http.StatusServiceUnavailable)
		return
	}
	days := 30
	if v := r.FormValue("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 31 { // 31 = the API's max 1d buckets
			http.Error(w, "days must be 1-31", http.StatusBadRequest)
			return
		}
		days = n
	}
	now := time.Now().UTC()
	start := now.AddDate(0, 0, -days).Truncate(24 * time.Hour)

	ledger, err := s.adminAPI.fetchLedger(r.Context(), start, now, s.workspaceID)
	if err != nil {
		http.Error(w, fmt.Sprintf("anthropic admin api: %v", err), http.StatusBadGateway)
		return
	}

	episodes, err := s.pricedEpisodes(r.Context(), ledger, start)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"days":     days,
		"episodes": episodes,
	})
}

// pricedEpisodes prices every Generation since start at its day's
// effective rates. Shared by the JSON endpoint and the Spend page, so
// the page can never quote a different number than the API does.
func (s *server) pricedEpisodes(ctx context.Context, ledger map[string]*dayLedger, start time.Time) ([]*episodeCost, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var episodes []*episodeCost
	byDay := map[string][]*episodeCost{}
	for _, u := range users {
		gens, err := s.store.ListGenerations(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		for _, g := range gens {
			if g.CreatedAt.Before(start) {
				continue
			}
			ep := priceGeneration(g, ledger)
			episodes = append(episodes, ep)
			if ep.Pricing == "reconciled" {
				byDay[g.CreatedAt.UTC().Format(costDay)] = append(byDay[g.CreatedAt.UTC().Format(costDay)], ep)
			}
		}
	}
	allocateSessionFees(ledger, byDay)

	// Newest first, across users.
	sort.Slice(episodes, func(i, j int) bool {
		return episodes[i].CreatedAt.After(episodes[j].CreatedAt)
	})
	return episodes, nil
}

// ElevenLabs is what this Generation drew from the ElevenLabs
// allowance, in ElevenLabs' own units: characters for speech, composed
// duration for music, a count for generated effects. Empty when the
// episode owes them nothing — the free engines voice most episodes, and
// that is the answer to "why is this row blank".
//
// Every kind that drew is listed. This used to be a switch returning the
// first match, which was right while a template was either voiced or
// composed and never both. A performed story is both and more: it
// reported its bed's duration and silently hid the dialogue characters
// that dominate its bill, so a two-minute story with speech, effects and
// music read as a flat "1m 00s".
//
// Deliberately not dollars, and deliberately not summed. The Cost column
// beside it is Anthropic's posted figure; this is a meter reading. The
// units genuinely differ and this page does not invent a rate to convert
// between them (ADR 0026), so they sit side by side instead.
func (e *episodeCost) ElevenLabs() string {
	var parts []string
	if e.TTSEngine == "elevenlabs" && e.TTSCharacters > 0 {
		s := fmt.Sprintf("%s chars", commas(int64(e.TTSCharacters)))
		// Requests, because dialogue is billed per call as well as per
		// character, and the packer's budget decides how many a story costs.
		if e.DialogueRequests > 0 {
			s += " · " + plural(e.DialogueRequests, "take")
		}
		parts = append(parts, s)
	}
	if e.MusicMillis > 0 {
		s := formatDuration(e.MusicMillis)
		// The unit word only when something else shares the cell. A
		// composed piece is still a bare duration, as it has always been:
		// the column header names the vendor, a duration reads as a
		// duration, and the extra word was wide enough to push this table
		// into a horizontal scroll on a phone. It earns its width only
		// once a character count is sitting next to it.
		if len(parts) > 0 {
			s += " music"
		}
		if e.MusicCalls > 1 {
			s += fmt.Sprintf(" · %d calls", e.MusicCalls)
		}
		parts = append(parts, s)
	}
	if e.SFXGenerated > 0 {
		parts = append(parts, plural(e.SFXGenerated, "effect"))
	}
	// Cache hits cost nothing, which is exactly why they are worth
	// showing: this is the number that says the cue library is working,
	// and a story where it stays at zero is a story to go look at.
	if e.SFXCacheHits > 0 {
		parts = append(parts, fmt.Sprintf("%d cached", e.SFXCacheHits))
	}
	// A performed story lists three or four meters where a voiced one
	// listed a single number. That is long enough to widen the table past
	// a phone, and this table already clips its topics to stop a
	// horizontal scrollbar carrying the cost column out of sight — so the
	// cell is allowed to wrap in CSS (.spend-meter) and grow downward
	// instead. Kept as a plain string with ordinary spaces: the wrapping
	// is presentation, and burying non-breaking spaces in here would make
	// every reader and test carry that knowledge invisibly.
	return strings.Join(parts, " · ")
}

// formatDuration renders composed audio: seconds under a minute,
// minutes above it. No unit word — the column header names the vendor
// and a duration reads as a duration, while the extra word was wide
// enough to push the table into a horizontal scroll on a phone.
func formatDuration(ms int) string {
	sec := ms / 1000
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm %02ds", sec/60, sec%60)
}

// commas groups thousands: a six-figure character count is unreadable
// otherwise, and these numbers are read against a plan's allowance.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Dollars is the cost as a person reads it, or "" while the day is
// still pending. The template cannot do this itself: CostUSD is a
// *float64 so that "not priced yet" is distinguishable from "free", and
// handing that pointer to printf renders "$%!f(*float64=0x...)" — which
// is exactly what production showed on every reconciled row.
//
// A method, so it stays out of the JSON: the API's shape is unchanged.
func (e *episodeCost) Dollars() string {
	if e.CostUSD == nil {
		return ""
	}
	return strconv.FormatFloat(*e.CostUSD, 'f', 4, 64)
}

// priceGeneration prices one Generation's meters at its creation day's
// effective rates. Any consumed kind without a posted rate makes the
// whole episode "pending" — a partial dollar figure would mislead.
func priceGeneration(g store.Generation, ledger map[string]*dayLedger) *episodeCost {
	ep := &episodeCost{
		User: g.UserID, ID: g.ID, Topic: g.Topic, Stage: g.Stage,
		EpisodeSlug: g.EpisodeSlug, CreatedAt: g.CreatedAt,
		SessionsCount: g.SessionsCount,
		Tokens: map[string]int64{
			"input":       g.InputTokens,
			"output":      g.OutputTokens,
			"cache_read":  g.CacheReadTokens,
			"cache_write": g.CacheWriteTokens,
		},
		TTSEngine: g.TTSEngine, TTSCharacters: g.TTSCharacters,
		MusicMillis: g.MusicMillis, MusicCalls: g.MusicCalls, MusicModel: g.MusicModel,
		DialogueRequests: g.DialogueRequests,
		SFXGenerated:     g.SFXGenerated, SFXCacheHits: g.SFXCacheHits,
		BreakdownUSD: map[string]float64{},
		Pricing:      "reconciled",
	}
	day := ledger[g.CreatedAt.UTC().Format(costDay)]
	total := 0.0
	for kind, tok := range ep.Tokens {
		if tok == 0 {
			continue
		}
		if day == nil || day.tokens[kind] == 0 || day.dollars[kind] == 0 {
			ep.Pricing = "pending"
			ep.BreakdownUSD = nil
			return ep
		}
		cost := float64(tok) * day.dollars[kind] / float64(day.tokens[kind])
		ep.BreakdownUSD[kind] = roundUSD(cost)
		total += cost
	}
	total = roundUSD(total)
	ep.CostUSD = &total
	return ep
}

// allocateSessionFees splits each day's session_usage dollars across
// that day's reconciled episodes, weighted by sessions_count.
func allocateSessionFees(ledger map[string]*dayLedger, byDay map[string][]*episodeCost) {
	for day, eps := range byDay {
		l := ledger[day]
		if l == nil || l.session == 0 {
			continue
		}
		var sessions int
		for _, ep := range eps {
			sessions += ep.SessionsCount
		}
		if sessions == 0 {
			continue
		}
		for _, ep := range eps {
			share := l.session * float64(ep.SessionsCount) / float64(sessions)
			if share == 0 {
				continue
			}
			ep.BreakdownUSD["session_share"] = roundUSD(share)
			total := roundUSD(*ep.CostUSD + share)
			ep.CostUSD = &total
		}
	}
}

func roundUSD(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// fetchLedger pulls the cost report and the usage report for the range
// and folds both into per-day dollars and tokens per kind. One page of
// 1d buckets covers the 31-day maximum, so no pagination loop.
func (a *anthropicAdmin) fetchLedger(ctx context.Context, start, end time.Time, workspace string) (map[string]*dayLedger, error) {
	q := url.Values{
		"bucket_width": {"1d"},
		"limit":        {"31"},
		"starting_at":  {start.Format(time.RFC3339)},
		"ending_at":    {end.Format(time.RFC3339)},
		// Both reports are grouped the same way and filtered the same
		// way. The effective rate is this workspace's dollars over this
		// workspace's tokens; mixing scopes between numerator and
		// denominator is exactly the blend that made per-episode dollars
		// wrong when the organization ran more than one workload.
		"group_by[]": {"workspace_id"},
	}
	qc := url.Values{"group_by[]": {"description", "workspace_id"}}
	maps.Copy(qc, q)
	qc["group_by[]"] = []string{"description", "workspace_id"}
	costBody, err := a.fetch(ctx, "/v1/organizations/cost_report", qc)
	if err != nil {
		return nil, err
	}
	usageBody, err := a.fetch(ctx, "/v1/organizations/usage_report/messages", q)
	if err != nil {
		return nil, err
	}

	ledger := map[string]*dayLedger{}
	at := func(day string) *dayLedger {
		l := ledger[day]
		if l == nil {
			l = &dayLedger{dollars: map[string]float64{}, tokens: map[string]int64{}}
			ledger[day] = l
		}
		return l
	}

	// Cost report: amounts arrive in cents (see centsToDollars).
	var costs struct {
		Data []struct {
			StartingAt time.Time `json:"starting_at"`
			Results    []struct {
				Amount      string `json:"amount"`
				CostType    string `json:"cost_type"`
				TokenType   string `json:"token_type"`
				WorkspaceID string `json:"workspace_id"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(costBody, &costs); err != nil {
		return nil, fmt.Errorf("cost report: %w", err)
	}
	for _, bucket := range costs.Data {
		day := bucket.StartingAt.UTC().Format(costDay)
		for _, rec := range bucket.Results {
			if !inWorkspace(workspace, rec.WorkspaceID) {
				continue
			}
			cents, err := strconv.ParseFloat(rec.Amount, 64)
			if err != nil {
				continue
			}
			switch kind := costKind(rec.CostType, rec.TokenType); kind {
			case "":
			case "session":
				at(day).session += cents / 100
			default:
				at(day).dollars[kind] += cents / 100
			}
		}
	}

	// Usage report: token counts per bucket; results sum across any
	// grouping the API applies.
	var usage struct {
		Data []struct {
			StartingAt time.Time `json:"starting_at"`
			Results    []struct {
				UncachedInput int64 `json:"uncached_input_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				CacheCreation struct {
					Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
					Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
				Output      int64  `json:"output_tokens"`
				WorkspaceID string `json:"workspace_id"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(usageBody, &usage); err != nil {
		return nil, fmt.Errorf("usage report: %w", err)
	}
	for _, bucket := range usage.Data {
		l := at(bucket.StartingAt.UTC().Format(costDay))
		for _, rec := range bucket.Results {
			if !inWorkspace(workspace, rec.WorkspaceID) {
				continue
			}
			l.tokens["input"] += rec.UncachedInput
			l.tokens["cache_read"] += rec.CacheRead
			l.tokens["cache_write"] += rec.CacheCreation.Ephemeral5m + rec.CacheCreation.Ephemeral1h
			l.tokens["output"] += rec.Output
		}
	}
	return ledger, nil
}

// inWorkspace reports whether a report row belongs to the workspace the
// server is scoped to. An unset scope keeps every row, which is the
// org-wide behaviour every deployment had before workspaces were split.
// The organization's default workspace reports an empty id, so an
// explicit scope never matches it by accident.
func inWorkspace(scope, rowID string) bool {
	return scope == "" || scope == rowID
}

// fetch GETs one Admin API resource and returns the body, erroring on
// any non-200 so callers never parse an error envelope as a report.
func (a *anthropicAdmin) fetch(ctx context.Context, path string, q url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %s: %s", path, resp.Status, snippet)
	}
	return io.ReadAll(resp.Body)
}
