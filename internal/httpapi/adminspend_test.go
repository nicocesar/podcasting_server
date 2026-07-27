package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// spendUpstream fakes the two Admin API reports the Spend page reads.
func spendUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/organizations/cost_report":
			// Cents upstream: 116.3715c and 41.5c, so $1.163715 + $0.415.
			fmt.Fprint(w, `{"data":[
			  {"starting_at":"2026-07-10T00:00:00Z","results":[
			    {"currency":"USD","amount":"116.3715","cost_type":"tokens","token_type":"uncached_input_tokens"}]},
			  {"starting_at":"2026-07-11T00:00:00Z","results":[
			    {"currency":"USD","amount":"41.5","cost_type":"tokens","token_type":"output_tokens"}]}
			],"has_more":false}`)
		case "/v1/organizations/usage_report/messages":
			if r.URL.Query().Get("group_by[]") == "model" {
				fmt.Fprint(w, `{"data":[{"starting_at":"2026-07-10T00:00:00Z","results":[
				  {"model":"claude-opus-5","uncached_input_tokens":1000,"cache_read_input_tokens":200,"output_tokens":300},
				  {"model":"claude-sonnet-5","uncached_input_tokens":50,"output_tokens":10}
				]}],"has_more":false}`)
				return
			}
			fmt.Fprint(w, `{"data":[{"starting_at":"2026-07-10T00:00:00Z","results":[
			  {"uncached_input_tokens":1050,"cache_read_input_tokens":200,"output_tokens":310}
			]}],"has_more":false}`)
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
	}))
}

// TestSpendPageRendersForBrowsers: /admin/costs answered raw JSON, and
// the dashboard admitted as much in its own copy. A browser gets a page
// now.
func TestSpendPageRendersForBrowsers(t *testing.T) {
	upstream := spendUpstream(t)
	defer upstream.Close()
	ts := newCostReportingServer(t, upstream.URL)
	admin := createAdmin(t, ts, "chief")

	resp, body := htmlPage(t, ts.URL+"/admin/costs", admin.sessionCreds())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/costs: %d\n%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("a browser got %q, not a page", ct)
	}
	for _, want := range []string{
		"Spend",
		"$1.58",         // 116.3715c + 41.5c, converted from cents
		"2026-07-10",    // the by-day table
		"By episode",    //
		"claude-opus-5", // tokens by model
		"1200",          // opus input: uncached + cache read
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the spend page is missing %q", want)
		}
	}
}

// TestSpendKeepsItsJSONRepresentation: the page is negotiated, not a
// replacement. Anything that was reading this URL keeps its JSON.
func TestSpendKeepsItsJSONRepresentation(t *testing.T) {
	upstream := spendUpstream(t)
	defer upstream.Close()
	ts := newCostReportingServer(t, upstream.URL)
	admin := createAdmin(t, ts, "chief")

	resp := do(t, "GET", ts.URL+"/admin/costs", admin.sessionCreds(), nil, "")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("a non-browser got %q, want JSON", ct)
	}
}

// TestSpendPageSurvivesAnUpstreamFailure: an admin can do nothing about
// a 502 from Anthropic except look again later, and a blank error page
// tells them less than a page that says so.
func TestSpendPageSurvivesAnUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream is having a day", http.StatusBadGateway)
	}))
	defer upstream.Close()
	ts := newCostReportingServer(t, upstream.URL)
	admin := createAdmin(t, ts, "chief")

	resp, body := htmlPage(t, ts.URL+"/admin/costs", admin.sessionCreds())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/costs with a broken upstream: %d, want a page", resp.StatusCode)
	}
	if !strings.Contains(body, "Could not read the cost report") {
		t.Errorf("the page does not say what went wrong:\n%s", body)
	}
}

// TestSpendPageWithoutAKey: the server runs fine with no
// ANTHROPIC_ADMIN_KEY, and the page should say so rather than 503 at an
// admin who followed a link from /admin.
func TestSpendPageWithoutAKey(t *testing.T) {
	ts := newTestServer(t)
	admin := createAdmin(t, ts, "chief")

	resp, body := htmlPage(t, ts.URL+"/admin/costs", admin.sessionCreds())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/costs unconfigured: %d, want a page", resp.StatusCode)
	}
	if !strings.Contains(body, "ANTHROPIC_ADMIN_KEY") {
		t.Errorf("the page does not explain what is missing:\n%s", body)
	}
}
