package httpapi

// GET /admin/costs and /admin/usage proxy Anthropic's Usage & Cost Admin
// API: real billed money (cost_report, daily × model) and token buckets
// (usage_report). The per-Generation meters answer "what did this episode
// consume"; these answer "what did Anthropic actually charge" — no price
// table to maintain in between. The usage report is passed through
// verbatim; the cost report has its amounts converted from cents (the
// upstream unit, despite each result saying "currency": "USD") to dollars.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type anthropicAdmin struct {
	key     string
	baseURL string
	http    *http.Client
}

// newAnthropicAdmin returns nil when no key is configured; the handlers
// answer 503 in that case.
func newAnthropicAdmin(key, baseURL string) *anthropicAdmin {
	if key == "" {
		return nil
	}
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &anthropicAdmin{
		key:     key,
		baseURL: baseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// handleAdminCosts serves the cost report: USD per day, grouped by
// description (which carries the model). Upstream amounts arrive in
// cents and are converted to dollars. ?days=N (1-31, default 30);
// ?page= forwards the API's pagination cursor.
func (s *server) handleAdminCosts(w http.ResponseWriter, r *http.Request) {
	q := url.Values{"group_by[]": {"description"}}
	s.adminAPI.proxy(w, r, "/v1/organizations/cost_report", 30, q, centsToDollars)
}

// handleAdminUsage serves daily token buckets grouped by model.
// ?days=N (1-31, default 7); ?page= forwards the pagination cursor.
func (s *server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	q := url.Values{"group_by[]": {"model"}}
	s.adminAPI.proxy(w, r, "/v1/organizations/usage_report/messages", 7, q, nil)
}

func (a *anthropicAdmin) proxy(w http.ResponseWriter, r *http.Request, path string, defaultDays int, q url.Values, transform func([]byte) ([]byte, error)) {
	if a == nil {
		http.Error(w, "cost reporting is not configured on this server (set ANTHROPIC_ADMIN_KEY)", http.StatusServiceUnavailable)
		return
	}
	days := defaultDays
	if v := r.FormValue("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 31 { // 31 = the API's max 1d buckets
			http.Error(w, "days must be 1-31", http.StatusBadRequest)
			return
		}
		days = n
	}
	now := time.Now().UTC()
	q.Set("bucket_width", "1d")
	q.Set("limit", "31")
	q.Set("starting_at", now.AddDate(0, 0, -days).Truncate(24*time.Hour).Format(time.RFC3339))
	q.Set("ending_at", now.Format(time.RFC3339))
	if page := r.FormValue("page"); page != "" {
		q.Set("page", page)
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.http.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("anthropic admin api: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if transform == nil || resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body) //nolint:errcheck // best effort once headers are sent
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("anthropic admin api: %v", err), http.StatusBadGateway)
		return
	}
	out, err := transform(body)
	if err != nil {
		http.Error(w, fmt.Sprintf("anthropic admin api: %v", err), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(out) //nolint:errcheck // best effort once headers are sent
}

// centsToDollars rewrites every "amount" field in the cost report from
// cents (the unit the upstream API bills in) to dollars, so the JSON
// means what its "currency": "USD" field implies.
func centsToDollars(body []byte) ([]byte, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	convertAmounts(doc)
	return json.Marshal(doc)
}

func convertAmounts(v any) {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			if k == "amount" {
				if s, ok := child.(string); ok {
					if cents, err := strconv.ParseFloat(s, 64); err == nil {
						node[k] = strconv.FormatFloat(cents/100, 'f', -1, 64)
					}
				}
				continue
			}
			convertAmounts(child)
		}
	case []any:
		for _, child := range node {
			convertAmounts(child)
		}
	}
}
