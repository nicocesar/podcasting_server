package httpapi

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// elevenStub stands in for ElevenLabs' /v1/user/subscription. calls
// counts requests, so the cache can be shown to work.
type elevenStub struct {
	*httptest.Server
	calls atomic.Int32
}

func newElevenStub(t *testing.T, body string) *elevenStub {
	t.Helper()
	stub := &elevenStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.calls.Add(1)
		if r.URL.Path != "/v1/user/subscription" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("xi-api-key") == "" {
			t.Error("request carried no xi-api-key")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(stub.Close)
	return stub
}

// newCreditServer builds a server whose ElevenLabs calls hit stub.
func newCreditServer(t *testing.T, stubURL string) *httptest.Server {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:             st,
		AdminToken:        adminToken,
		SessionSecret:     "test-session-secret",
		Assets:            os.DirFS("../../cmd/server"),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		ElevenLabsKey:     "xi-test-key",
		ElevenLabsBaseURL: stubURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// flat collapses HTML whitespace so an assertion can quote a sentence
// the template happens to wrap across lines.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// adminHTML fetches an admin page the way a browser does.
func adminHTML(t *testing.T, ts *httptest.Server, url, creds string) string {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	if v, ok := strings.CutPrefix(creds, "session:"); ok {
		req.AddCookie(&http.Cookie{Name: "session", Value: v})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d\n%s", url, resp.StatusCode, body)
	}
	return flat(string(body))
}

// subBody is one subscription payload: used of limit.
func subBody(used, limit int64) string {
	return fmt.Sprintf(`{"tier":"creator","status":"active","character_count":%d,`+
		`"character_limit":%d,"next_character_count_reset_unix":1788000000}`, used, limit)
}

// TestSpendPageShowsElevenLabsBalance: the balance an operator could
// previously only discover by reading a failed generation's trace.
func TestSpendPageShowsElevenLabsBalance(t *testing.T) {
	stub := newElevenStub(t, subBody(40_000, 100_000))
	ts := newCreditServer(t, stub.URL)
	admin := createAdmin(t, ts, "boss")

	page := adminHTML(t, ts, ts.URL+"/admin/costs", admin.sessionCreds())
	for _, want := range []string{
		"ElevenLabs credits",
		"60000",         // remaining
		"100000",        // limit
		"creator",       // tier
		"resets 29 Aug", // formatted reset date
	} {
		if !strings.Contains(page, want) {
			t.Errorf("spend page missing %q", want)
		}
	}
	// Plenty left: no alarm.
	if strings.Contains(page, "Running low") || strings.Contains(page, "Out of credit") {
		t.Error("healthy balance raised a warning")
	}
	// Credits are not dollars, and the page must not pretend otherwise.
	if strings.Contains(page, "$60000") {
		t.Error("credits rendered as dollars")
	}
}

func TestLowBalanceWarns(t *testing.T) {
	stub := newElevenStub(t, subBody(95_000, 100_000)) // 5% left
	ts := newCreditServer(t, stub.URL)
	admin := createAdmin(t, ts, "boss")

	spend := adminHTML(t, ts, ts.URL+"/admin/costs", admin.sessionCreds())
	if !strings.Contains(spend, "Running low") {
		t.Errorf("spend page did not warn:\n%s", spend)
	}
	// The warning also reaches the admin index, which is the page an
	// operator actually passes through.
	index := adminHTML(t, ts, ts.URL+"/admin", admin.sessionCreds())
	if !strings.Contains(index, "running low") {
		t.Errorf("admin index did not warn:\n%s", index)
	}
}

// TestExhaustedBalanceSaysWhatBreaks: the state behind the misleading
// 4xx. Music has no fallback and speech does, and the warning says so.
func TestExhaustedBalanceSaysWhatBreaks(t *testing.T) {
	stub := newElevenStub(t, subBody(100_000, 100_000))
	ts := newCreditServer(t, stub.URL)
	admin := createAdmin(t, ts, "boss")

	for _, url := range []string{ts.URL + "/admin/costs", ts.URL + "/admin"} {
		page := adminHTML(t, ts, url, admin.sessionCreds())
		if !strings.Contains(strings.ToLower(page), "out of credit") {
			t.Errorf("%s did not report exhaustion:\n%s", url, page)
		}
		if !strings.Contains(page, "Music cannot be composed") {
			t.Errorf("%s did not say music is the casualty", url)
		}
	}
}

// TestBalanceIsCached keeps admin page loads off the vendor's API.
func TestBalanceIsCached(t *testing.T) {
	stub := newElevenStub(t, subBody(10, 100_000))
	ts := newCreditServer(t, stub.URL)
	admin := createAdmin(t, ts, "boss")

	for range 4 {
		adminHTML(t, ts, ts.URL+"/admin/costs", admin.sessionCreds())
	}
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("hit ElevenLabs %d times for 4 page loads, want 1", n)
	}
}

// TestNoKeyExplainsItself: a server without ElevenLabs shows no meter
// and no alarm, just the reason.
func TestNoKeyExplainsItself(t *testing.T) {
	ts := newTestServer(t) // no ELEVENLABS_API_KEY
	admin := createAdmin(t, ts, "boss")

	page := adminHTML(t, ts, ts.URL+"/admin/costs", admin.sessionCreds())
	if !strings.Contains(page, "ELEVENLABS_API_KEY") {
		t.Errorf("unconfigured page does not say why:\n%s", page)
	}
	if strings.Contains(page, "Out of credit") {
		t.Error("an unread balance was reported as empty")
	}
}

// TestUnreadableBalanceIsNotZero: when the vendor errors, the page says
// it could not read the balance rather than showing an empty meter and
// crying wolf.
func TestUnreadableBalanceIsNotZero(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"detail":{"message":"boom"}}`)
	}))
	t.Cleanup(stub.Close)
	ts := newCreditServer(t, stub.URL)
	admin := createAdmin(t, ts, "boss")

	page := adminHTML(t, ts, ts.URL+"/admin/costs", admin.sessionCreds())
	if !strings.Contains(page, "Could not read the ElevenLabs balance") {
		t.Errorf("failed read not surfaced:\n%s", page)
	}
	if strings.Contains(page, "Out of credit") {
		t.Error("a failed read was reported as exhaustion")
	}
}
