package httpapi

// The Spend page: what Anthropic actually charged, as a page rather than
// a download. /admin/costs and /admin/usage were byte-for-byte proxies of
// the Admin API, so reading them meant saving a file and opening it in
// something else — the dashboard said so out loud, which is a fair sign
// a surface is not finished.
//
// One page carries all three answers, because they are one question:
// what did this cost, which episodes caused it, and what did the tokens
// do. The three JSON representations are unchanged and still served —
// they are negotiated, not replaced — so anything that consumed them
// keeps working.
//
// No price table is introduced here (that decision stands): every dollar
// on this page comes from Anthropic's own billed figures.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// daySpend is one day's billed dollars, with the bar width the template
// draws. Percent is of the busiest day in the range, so the shape of the
// month is visible even when the amounts are pennies.
type daySpend struct {
	Day     string
	USD     float64
	Percent int
}

// modelUsage is one model's tokens across the whole range.
type modelUsage struct {
	Model  string
	Input  int64
	Output int64
}

type spendPage struct {
	Days     int
	TotalUSD float64
	ByDay    []daySpend
	Episodes []*episodeCost
	Models   []modelUsage
	// Pending counts episodes the cost report has not caught up with.
	// It lags a few hours, and a page that silently showed them as free
	// would be worse than one that says it does not know yet.
	Pending int
	// Workspace is the Anthropic workspace these numbers cover. Empty
	// means the whole organization — which the page says out loud,
	// because that is the difference between "what this server cost" and
	// "what everything on this key cost" (ADR 0024).
	Workspace string
	// Credits is the other meter: ElevenLabs' remaining balance, which
	// no Anthropic report knows about and which fails louder than an
	// overspend when it runs out.
	Credits creditView
	// Error is a reporting failure, shown on the page instead of a 502:
	// the admin can do nothing about an upstream hiccup except look
	// again later, and a blank error page tells them less than this.
	Error string
}

// handleAdminSpend renders the page a browser asked for. Everything else
// — curl, scripts, the tests — still gets the proxied cost report, so
// the JSON representation of this URL is unchanged.
func (s *server) handleAdminSpend(w http.ResponseWriter, r *http.Request) {
	if !wantsHTML(r) {
		s.handleAdminCosts(w, r)
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
	page := spendPage{Days: days, Workspace: s.workspaceID}
	// Read before the Anthropic branch below: the two vendors are
	// configured independently, and a server with no ANTHROPIC_ADMIN_KEY
	// still wants to know its music is about to stop.
	page.Credits = s.elevenCredits(r.Context())
	if s.adminAPI == nil {
		page.Error = "Cost reporting is not configured on this server. Set ANTHROPIC_ADMIN_KEY to read what Anthropic charged."
		s.render(w, r, http.StatusOK, s.tmplAdminSpend, page)
		return
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -days).Truncate(24 * time.Hour)

	ledger, err := s.adminAPI.fetchLedger(r.Context(), start, now, s.workspaceID)
	if err != nil {
		page.Error = "Could not read the cost report: " + err.Error()
		s.render(w, r, http.StatusOK, s.tmplAdminSpend, page)
		return
	}
	page.ByDay, page.TotalUSD = summarise(ledger)

	if page.Episodes, err = s.pricedEpisodes(r.Context(), ledger, start); err != nil {
		s.fail(w, err)
		return
	}
	for _, ep := range page.Episodes {
		if ep.Pricing != "reconciled" {
			page.Pending++
		}
	}

	// Tokens by model is the one thing the ledger cannot answer: it
	// buckets by token kind, which is what pricing needs. One more call.
	if page.Models, err = s.adminAPI.fetchModelUsage(r.Context(), start, now, s.workspaceID); err != nil {
		page.Error = "Could not read the usage report: " + err.Error()
	}
	s.render(w, r, http.StatusOK, s.tmplAdminSpend, page)
}

// summarise folds the ledger into one row per day, newest first, and the
// range total. Session fees count: they are real dollars on the bill.
func summarise(ledger map[string]*dayLedger) ([]daySpend, float64) {
	rows := make([]daySpend, 0, len(ledger))
	var total, peak float64
	for day, l := range ledger {
		sum := l.session
		for _, v := range l.dollars {
			sum += v
		}
		if sum == 0 {
			continue
		}
		rows = append(rows, daySpend{Day: day, USD: roundUSD(sum)})
		total += sum
		if sum > peak {
			peak = sum
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Day > rows[j].Day })
	if peak > 0 {
		for i := range rows {
			rows[i].Percent = int(rows[i].USD / peak * 100)
		}
	}
	return rows, roundUSD(total)
}

// fetchModelUsage totals input and output tokens per model over the
// range. Grouped by model rather than by day: the question this answers
// is which model did the work, not when.
func (a *anthropicAdmin) fetchModelUsage(ctx context.Context, start, end time.Time, workspace string) ([]modelUsage, error) {
	body, err := a.fetch(ctx, "/v1/organizations/usage_report/messages", url.Values{
		"bucket_width": {"1d"},
		"limit":        {"31"},
		"starting_at":  {start.Format(time.RFC3339)},
		"ending_at":    {end.Format(time.RFC3339)},
		"group_by[]":   {"model", "workspace_id"},
	})
	if err != nil {
		return nil, err
	}
	var usage struct {
		Data []struct {
			Results []struct {
				Model         string `json:"model"`
				WorkspaceID   string `json:"workspace_id"`
				UncachedInput int64  `json:"uncached_input_tokens"`
				CacheRead     int64  `json:"cache_read_input_tokens"`
				CacheCreation struct {
					Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
					Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
				Output int64 `json:"output_tokens"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, fmt.Errorf("usage report: %w", err)
	}
	byModel := map[string]*modelUsage{}
	for _, bucket := range usage.Data {
		for _, rec := range bucket.Results {
			if !inWorkspace(workspace, rec.WorkspaceID) {
				continue
			}
			name := rec.Model
			if name == "" {
				name = "(unattributed)"
			}
			m := byModel[name]
			if m == nil {
				m = &modelUsage{Model: name}
				byModel[name] = m
			}
			// Everything that went in counts as input here: cache reads
			// and cache writes are still context the model was given,
			// and splitting them four ways is the pricing question,
			// which the dollars above already answer.
			m.Input += rec.UncachedInput + rec.CacheRead +
				rec.CacheCreation.Ephemeral5m + rec.CacheCreation.Ephemeral1h
			m.Output += rec.Output
		}
	}
	out := make([]modelUsage, 0, len(byModel))
	for _, m := range byModel {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Input+out[i].Output > out[j].Input+out[j].Output
	})
	return out, nil
}
