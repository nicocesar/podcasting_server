package httpapi

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// newCostReportingServer is a test server with the Admin API pointed at a
// fake upstream that records the query it received.
func newCostReportingServer(t *testing.T, upstream string) *httptest.Server {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:                 st,
		AdminToken:            adminToken,
		Assets:                os.DirFS("../../cmd/server"),
		Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		AnthropicAdminKey:     "sk-ant-admin-test",
		AnthropicAdminBaseURL: upstream,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestAdminCostsProxiesTheCostReport(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/organizations/cost_report" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-ant-admin-test" {
			t.Errorf("x-api-key = %q", got)
		}
		q := r.URL.Query()
		if q.Get("bucket_width") != "1d" || q.Get("starting_at") == "" || q.Get("ending_at") == "" {
			t.Errorf("query = %v", q)
		}
		if got := q["group_by[]"]; len(got) != 1 || got[0] != "description" {
			t.Errorf("group_by = %v", got)
		}
		// The upstream API reports amounts in cents.
		fmt.Fprint(w, `{"data":[{"starting_at":"2026-07-10T00:00:00Z","results":[{"currency":"USD","amount":"116.3715","description":"Claude Sonnet 5 Usage - Input Tokens, Cache Write","model":"claude-sonnet-5"}]}],"has_more":false}`)
	}))
	defer upstream.Close()
	ts := newCostReportingServer(t, upstream.URL)

	resp := do(t, "GET", ts.URL+"/admin/costs?days=7", "bearer:"+adminToken, nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "claude-sonnet-5") {
		t.Errorf("body = %s", body)
	}
	// Amounts are converted from the upstream's cents to dollars.
	if !strings.Contains(string(body), `"amount":"1.163715"`) {
		t.Errorf("amount not converted to dollars: %s", body)
	}

	// Guarded like every other /admin endpoint.
	resp = do(t, "GET", ts.URL+"/admin/costs", "", nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	// The API caps 1d reports at 31 buckets; reject out-of-range early.
	resp = do(t, "GET", ts.URL+"/admin/costs?days=90", "bearer:"+adminToken, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("days=90 status = %d, want 400", resp.StatusCode)
	}
}

func TestAdminUsageProxiesTheUsageReport(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/organizations/usage_report/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.URL.Query()["group_by[]"]; len(got) != 1 || got[0] != "model" {
			t.Errorf("group_by = %v", got)
		}
		fmt.Fprint(w, `{"data":[],"has_more":false}`)
	}))
	defer upstream.Close()
	ts := newCostReportingServer(t, upstream.URL)

	resp := do(t, "GET", ts.URL+"/admin/usage", "bearer:"+adminToken, nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestAdminCostsUnconfigured(t *testing.T) {
	ts := newTestServer(t) // no ANTHROPIC_ADMIN_KEY
	resp := do(t, "GET", ts.URL+"/admin/costs", "bearer:"+adminToken, nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}
