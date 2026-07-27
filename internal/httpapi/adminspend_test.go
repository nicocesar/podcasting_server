package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// spendUpstream fakes the two Admin API reports the Spend page reads.
func spendUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	// Today, because a Generation is priced at its own day's rates: a
	// ledger for some other day reconciles nothing and every row comes
	// back "pending", which is how a formatting bug in the priced cell
	// stayed invisible to these tests until production showed it.
	today := time.Now().UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/organizations/cost_report":
			// Cents upstream: 116.3715c and 41.5c, so $1.163715 + $0.415.
			fmt.Fprintf(w, `{"data":[
			  {"starting_at":%q,"results":[
			    {"currency":"USD","amount":"116.3715","cost_type":"tokens","token_type":"uncached_input_tokens"},
			    {"currency":"USD","amount":"41.5","cost_type":"tokens","token_type":"output_tokens"}]}
			],"has_more":false}`, today)
		case "/v1/organizations/usage_report/messages":
			if r.URL.Query().Get("group_by[]") == "model" {
				fmt.Fprintf(w, `{"data":[{"starting_at":%q,"results":[
				  {"model":"claude-opus-5","uncached_input_tokens":1000,"cache_read_input_tokens":200,"output_tokens":300},
				  {"model":"claude-sonnet-5","uncached_input_tokens":50,"output_tokens":10}
				]}],"has_more":false}`, today)
				return
			}
			fmt.Fprintf(w, `{"data":[{"starting_at":%q,"results":[
			  {"uncached_input_tokens":1050,"cache_read_input_tokens":200,"output_tokens":310}
			]}],"has_more":false}`, today)
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
		"$1.58",                               // 116.3715c + 41.5c, converted from cents
		time.Now().UTC().Format("2006-01-02"), // the by-day table
		"By episode",                          //
		"claude-opus-5",                       // tokens by model
		"1200",                                // opus input: uncached + cache read
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

// TestSpendPageRendersRealRows is the case the first pass of these tests
// missed entirely: every assertion above ran against a server with no
// generations, so the by-episode table was always empty and a formatting
// bug in its cells could not be seen. It shipped one — the cost is a
// *float64 and printf was handed the pointer, so production showed
// "$%!f(*float64=0x...)" in every priced row.
func TestSpendPageRendersRealRows(t *testing.T) {
	upstream := spendUpstream(t)
	defer upstream.Close()
	ts, st := newEpisodeCostServer(t, upstream.URL)
	admin := createAdmin(t, ts, "chief")

	ctx := context.Background()
	if err := st.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	long := "A very long episode topic that will not fit in a narrow column without being cut off somewhere"
	for _, g := range []store.Generation{
		{UserID: "alice", ID: "gen1", Topic: "world cup", Stage: store.GenDone,
			InputTokens: 500, OutputTokens: 100,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{UserID: "alice", ID: "gen2", Topic: long, Stage: store.GenDone,
			InputTokens: 100, OutputTokens: 10,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	} {
		if err := st.PutGeneration(ctx, g); err != nil {
			t.Fatal(err)
		}
	}

	_, body := htmlPage(t, ts.URL+"/admin/costs", admin.sessionCreds())

	// The bug: a pointer reaching printf.
	if strings.Contains(body, "%!f") || strings.Contains(body, "*float64") {
		t.Errorf("a cost cell rendered a Go formatting error:\n%s", body)
	}
	// The topic actually appears, and carries its full text for the
	// tooltip so a clipped cell is still readable.
	for _, want := range []string{"world cup", long, `title="` + long + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the by-episode table is missing %q", want)
		}
	}
	// And a real dollar figure, not a pointer and not an empty cell.
	if !strings.Contains(body, "$0.") {
		t.Errorf("no priced row rendered a dollar amount:\n%s", body)
	}
}

// TestSpendEpisodeCellShape pins the two things the screenshot caught:
// a priced cell holds a dollar figure and not a Go pointer, and a topic
// cell carries its full text in a title so clipping it stays readable.
func TestSpendEpisodeCellShape(t *testing.T) {
	upstream := spendUpstream(t)
	defer upstream.Close()
	ts, st := newEpisodeCostServer(t, upstream.URL)
	admin := createAdmin(t, ts, "chief")

	ctx := context.Background()
	if err := st.UpsertUser(ctx, store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	topic := "A very long episode topic that will not fit in a narrow column at all"
	if err := st.PutGeneration(ctx, store.Generation{
		UserID: "alice", ID: "g1", Topic: topic, Stage: store.GenDone,
		InputTokens: 500, OutputTokens: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	_, body := htmlPage(t, ts.URL+"/admin/costs", admin.sessionCreds())

	want := `<td class="spend-topic" title="` + topic + `">` + topic
	if !strings.Contains(body, want) {
		t.Errorf("the topic cell is not clippable-with-tooltip:\n%s", want)
	}
	// A priced row: "$" followed by digits, not a formatting verb.
	priced := regexp.MustCompile(`\$[0-9]+\.[0-9]{4}`)
	if !priced.MatchString(body) {
		t.Errorf("no episode row rendered a dollar figure in $0.0000 form")
	}
}
