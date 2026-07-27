package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

const oursWorkspace = "wrkspc_ours"

// twoWorkspaceUpstream bills two workloads on one organisation, at
// deliberately different per-token rates: ours is $1.00 for 1,000 input
// tokens ($0.001/token), the other workload is $50.00 for 5,000
// ($0.01/token). Blended org-wide that is $51.00 / 6,000 = $0.0085/token
// — so a scoped page and an unscoped one disagree on both the total and
// on every per-episode figure, which is what makes this test sharp.
func twoWorkspaceUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	today := time.Now().UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()["group_by[]"]
		if !contains(q, "workspace_id") {
			t.Errorf("%s was not grouped by workspace_id: %v", r.URL.Path, q)
		}
		switch r.URL.Path {
		case "/v1/organizations/cost_report":
			if !contains(q, "description") {
				t.Errorf("cost report lost its description grouping: %v", q)
			}
			// Amounts are cents.
			fmt.Fprintf(w, `{"data":[{"starting_at":%q,"results":[
			  {"currency":"USD","amount":"100","cost_type":"tokens",
			   "token_type":"uncached_input_tokens","workspace_id":%q},
			  {"currency":"USD","amount":"5000","cost_type":"tokens",
			   "token_type":"uncached_input_tokens","workspace_id":null}
			]}],"has_more":false}`, today, oursWorkspace)
		case "/v1/organizations/usage_report/messages":
			fmt.Fprintf(w, `{"data":[{"starting_at":%q,"results":[
			  {"uncached_input_tokens":1000,"output_tokens":0,"model":"claude-sonnet-5","workspace_id":%q},
			  {"uncached_input_tokens":5000,"output_tokens":0,"model":"claude-opus-4-6","workspace_id":null}
			]}],"has_more":false}`, today, oursWorkspace)
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
	}))
}

func contains(vals []string, want string) bool {
	for _, v := range vals {
		if v == want || strings.Contains(v, want) {
			return true
		}
	}
	return false
}

func newScopedServer(t *testing.T, upstream, workspace string) (*httptest.Server, *fsstore.Store) {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:                 st,
		AdminToken:            adminToken,
		SessionSecret:         "test-session-secret",
		Assets:                os.DirFS("../../cmd/server"),
		Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		AnthropicAdminKey:     "sk-ant-admin-test",
		AnthropicAdminBaseURL: upstream,
		AnthropicWorkspaceID:  workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, st
}

// TestSpendCountsOnlyOurWorkspace is the fix for the discrepancy that
// started this: the by-day bar was the whole organisation's bill while
// the by-episode list was only this server, so 77% of one day belonged
// to no episode. Scoped to one workspace, the two populations finally
// describe the same thing.
func TestSpendCountsOnlyOurWorkspace(t *testing.T) {
	upstream := twoWorkspaceUpstream(t)
	defer upstream.Close()
	ts, _ := newScopedServer(t, upstream.URL, oursWorkspace)
	admin := createAdmin(t, ts, "chief")

	_, body := htmlPage(t, ts.URL+"/admin/costs", admin.sessionCreds())
	if !strings.Contains(body, "$1.00") {
		t.Errorf("scoped spend is not $1.00 (our workspace alone)")
	}
	if strings.Contains(body, "$51.00") {
		t.Errorf("the page is still counting the other workspace's $50")
	}
	// The other workspace's model must not appear in tokens-by-model.
	if strings.Contains(body, "claude-opus-4-6") {
		t.Errorf("tokens by model leaked another workspace's model:\n%s", body)
	}
	if !strings.Contains(body, "claude-sonnet-5") {
		t.Errorf("our own model is missing from tokens by model")
	}
	if !strings.Contains(body, oursWorkspace) {
		t.Errorf("the page does not say which workspace it is scoped to")
	}
}

// TestUnscopedSpendSaysSo: an unset scope keeps the old org-wide
// behaviour, which is correct for a deployment that has not split
// workspaces — but silently org-wide is how the original discrepancy
// hid, so the page has to admit it.
func TestUnscopedSpendSaysSo(t *testing.T) {
	upstream := twoWorkspaceUpstream(t)
	defer upstream.Close()
	ts, _ := newScopedServer(t, upstream.URL, "")
	admin := createAdmin(t, ts, "chief")

	_, body := htmlPage(t, ts.URL+"/admin/costs", admin.sessionCreds())
	if !strings.Contains(body, "$51.00") {
		t.Errorf("an unscoped page should still total the whole organisation")
	}
	if !strings.Contains(body, "ANTHROPIC_WORKSPACE_ID") {
		t.Errorf("an unscoped page does not warn that it is org-wide:\n%s", body)
	}
}

// TestEpisodePricingUsesOurRate: the rate is dollars over tokens, and
// both halves must come from the same workspace. Priced against our own
// workspace an episode of 500 input tokens costs $0.50; priced against
// the blended organisation it would cost $4.25.
func TestEpisodePricingUsesOurRate(t *testing.T) {
	upstream := twoWorkspaceUpstream(t)
	defer upstream.Close()
	ts, st := newScopedServer(t, upstream.URL, oursWorkspace)
	admin := createAdmin(t, ts, "chief")

	ctx := context.Background()
	if err := st.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutGeneration(ctx, store.Generation{
		UserID: "alice", ID: "g1", Topic: "half our tokens", Stage: store.GenDone,
		InputTokens: 500,
		CreatedAt:   time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	_, body := htmlPage(t, ts.URL+"/admin/costs", admin.sessionCreds())
	if !strings.Contains(body, "$0.5000") {
		t.Errorf("the episode was not priced at our workspace's rate:\n%s", body)
	}
	if strings.Contains(body, "$4.2") {
		t.Error("the episode was priced at the blended organisation rate")
	}
}
