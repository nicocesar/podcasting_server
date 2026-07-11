package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// newEpisodeCostServer is a cost-reporting test server that also hands
// back the store so tests can seed generations.
func newEpisodeCostServer(t *testing.T, upstream string) (*httptest.Server, *fsstore.Store) {
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
	return ts, st
}

func TestAdminEpisodeCostsReconcilesDollars(t *testing.T) {
	// The org's day, as both admin reports tell it. Amounts are cents.
	// Effective rates fall out as: input $0.002/1k, cache_read $0.20/1M,
	// cache_write $2.50/1M, output $10/1M — intro-priced Sonnet 5.
	today := time.Now().UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/organizations/cost_report":
			fmt.Fprintf(w, `{"data":[{"starting_at":%q,"results":[
				{"currency":"USD","amount":"0.2","cost_type":"tokens","token_type":"uncached_input_tokens"},
				{"currency":"USD","amount":"20","cost_type":"tokens","token_type":"cache_read_input_tokens"},
				{"currency":"USD","amount":"50","cost_type":"tokens","token_type":"cache_creation.ephemeral_5m_input_tokens"},
				{"currency":"USD","amount":"10","cost_type":"tokens","token_type":"output_tokens"},
				{"currency":"USD","amount":"2","cost_type":"session_usage"}
			]}],"has_more":false}`, today)
		case "/v1/organizations/usage_report/messages":
			fmt.Fprintf(w, `{"data":[{"starting_at":%q,"results":[
				{"uncached_input_tokens":1000,"cache_read_input_tokens":1000000,
				 "cache_creation":{"ephemeral_5m_input_tokens":200000,"ephemeral_1h_input_tokens":0},
				 "output_tokens":10000}
			]}],"has_more":false}`, today)
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	ts, st := newEpisodeCostServer(t, upstream.URL)

	ctx := context.Background()
	if err := st.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	// Half the org's tokens in every kind, and the day's only session
	// → cost = (0.002+0.20+0.50+0.10)/2 + 0.02 session fee = 0.421.
	if err := st.PutGeneration(ctx, store.Generation{
		UserID: "alice", ID: "gen1", Topic: "world cup", Stage: store.GenDone,
		SessionsCount: 1, InputTokens: 500, OutputTokens: 5000,
		CacheReadTokens: 500000, CacheWriteTokens: 100000,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// A generation with no tokens yet (still researching) prices at $0.
	if err := st.PutGeneration(ctx, store.Generation{
		UserID: "alice", ID: "gen2", Topic: "tango", Stage: store.GenResearching,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	resp := do(t, "GET", ts.URL+"/admin/costs/episodes?days=7", "bearer:"+adminToken, nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got struct {
		Days     int `json:"days"`
		Episodes []struct {
			ID           string             `json:"id"`
			CostUSD      *float64           `json:"cost_usd"`
			BreakdownUSD map[string]float64 `json:"breakdown_usd"`
			Pricing      string             `json:"pricing"`
		} `json:"episodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Days != 7 || len(got.Episodes) != 2 {
		t.Fatalf("days = %d, episodes = %d, want 7 and 2", got.Days, len(got.Episodes))
	}
	costs := map[string]float64{}
	for _, ep := range got.Episodes {
		if ep.Pricing != "reconciled" || ep.CostUSD == nil {
			t.Fatalf("episode %s: pricing = %q, cost = %v", ep.ID, ep.Pricing, ep.CostUSD)
		}
		costs[ep.ID] = *ep.CostUSD
	}
	if costs["gen1"] != 0.421 {
		t.Errorf("gen1 cost = %v, want 0.421", costs["gen1"])
	}
	if costs["gen2"] != 0 {
		t.Errorf("gen2 cost = %v, want 0", costs["gen2"])
	}
}

func TestAdminEpisodeCostsPendingWhenBillNotPosted(t *testing.T) {
	// Tokens consumed but no dollars posted yet: never invent a price.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[],"has_more":false}`)
	}))
	defer upstream.Close()
	ts, st := newEpisodeCostServer(t, upstream.URL)

	ctx := context.Background()
	if err := st.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutGeneration(ctx, store.Generation{
		UserID: "alice", ID: "gen1", Topic: "news", Stage: store.GenDone,
		SessionsCount: 1, OutputTokens: 5000, CacheWriteTokens: 100000,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	resp := do(t, "GET", ts.URL+"/admin/costs/episodes", "bearer:"+adminToken, nil, "")
	defer resp.Body.Close()
	var got struct {
		Episodes []struct {
			Pricing string   `json:"pricing"`
			CostUSD *float64 `json:"cost_usd"`
		} `json:"episodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Episodes) != 1 || got.Episodes[0].Pricing != "pending" || got.Episodes[0].CostUSD != nil {
		t.Errorf("episodes = %+v, want one pending with null cost", got.Episodes)
	}
}
