package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

// instantAPI is a managed-agents platform whose agent has, conveniently,
// always already answered. The send/poll dance is covered in the
// generation package's own tests.
type instantAPI struct{}

func (instantAPI) EnsureAgent(context.Context, string, string, string) (string, error) {
	return "agent-1", nil
}
func (instantAPI) EnsureEnvironment(context.Context, string) (string, error) { return "env-1", nil }
func (instantAPI) CreateSession(context.Context, string, string, string) (string, error) {
	return "sess-1", nil
}
func (instantAPI) SendMessage(context.Context, string, string) error     { return nil }
func (instantAPI) SessionStatus(context.Context, string) (string, error) { return "idle", nil }
func (instantAPI) SessionUsage(context.Context, string) (generation.Usage, error) {
	return generation.Usage{InputTokens: 10, OutputTokens: 5}, nil
}
func (instantAPI) DeleteSession(context.Context, string) error { return nil }
func (instantAPI) LastAgentMessage(context.Context, string) (string, error) { return "", nil }
func (instantAPI) LastToolUse(_ context.Context, sessionID, _ string) (*generation.ToolUse, error) {
	return &generation.ToolUse{
		ID:    sessionID + "-use-0",
		Input: []byte(`{"title":"Generated","summary":"A summary.","script":"Spoken words.","sources":[]}`),
	}, nil
}
func (instantAPI) SendToolResult(context.Context, string, string, string, bool) error { return nil }

type instantEngine struct{}

func (instantEngine) Name() string { return "instant" }
func (instantEngine) Synthesize(context.Context, string, tts.Voice) ([]byte, error) {
	return []byte("MP3!"), nil
}

// newGeneratingServer is newTestServer with the Generation feature on.
func newGeneratingServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:         st,
		AdminToken:    adminToken,
		SessionSecret: "test-session-secret",
		Assets:        os.DirFS("../../cmd/server"),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Generator: generation.NewRunner(generation.Config{
			Store:        st,
			API:          instantAPI{},
			Engines:      []tts.Engine{instantEngine{}},
			Model:        "claude-test",
			Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			PollInterval: 5 * time.Millisecond,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func postGenerate(t *testing.T, ts *httptest.Server, a account, form url.Values) *http.Response {
	t.Helper()
	return do(t, "POST", ts.URL+"/me/generate", a.publishCreds(),
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
}

func TestGenerateDisabledWithoutRunner(t *testing.T) {
	ts := newTestServer(t) // no Generator
	alice := createUser(t, ts, "alice")
	resp := do(t, "GET", ts.URL+"/me/generate", alice.publishCreds(), nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestGenerateFlow(t *testing.T) {
	ts := newGeneratingServer(t)
	alice := createUser(t, ts, "alice")

	// The form renders.
	resp := do(t, "GET", ts.URL+"/me/generate", alice.publishCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Freshness") {
		t.Fatalf("form: %d\n%s", resp.StatusCode, body)
	}

	// Submitting starts a Generation and answers JSON (non-browser).
	resp = postGenerate(t, ts, alice, url.Values{
		"topic": {"fusion energy"}, "length": {"5"}, "freshness": {"7"}, "language": {"en"}, "voice": {"female"},
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("start: %d %s", resp.StatusCode, b)
	}
	var g store.Generation
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if g.ID == "" || g.Stage != store.GenResearching {
		t.Fatalf("generation = %+v", g)
	}

	// The progress endpoint reaches done; the episode is in the feed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp = do(t, "GET", ts.URL+"/me/generations/"+g.ID, alice.publishCreds(), nil, "")
		var v struct {
			Stage       string `json:"stage"`
			StageLabel  string `json:"stage_label"`
			EpisodeSlug string `json:"episode_slug"`
			Error       string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if v.Stage == store.GenDone {
			if v.EpisodeSlug == "" || v.StageLabel != "Published" {
				t.Fatalf("done view = %+v", v)
			}
			break
		}
		if v.Stage == store.GenFailed {
			t.Fatalf("generation failed: %s", v.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck at %q", v.Stage)
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp = do(t, "GET", ts.URL+"/me/episodes", alice.publishCreds(), nil, "")
	var eps []store.Episode
	if err := json.NewDecoder(resp.Body).Decode(&eps); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(eps) != 1 || eps[0].Title != "Generated" {
		t.Fatalf("episodes = %+v", eps)
	}

	// The progress page renders as HTML for logged-in browsers.
	req, _ := http.NewRequest("GET", ts.URL+"/me/generations/"+g.ID, nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: alice.Session})
	req.Header.Set("Accept", "text/html")
	htmlResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(htmlResp.Body)
	htmlResp.Body.Close()
	if htmlResp.StatusCode != http.StatusOK || !strings.Contains(string(page), "Generating an episode") {
		t.Fatalf("progress page: %d\n%s", htmlResp.StatusCode, page)
	}
}

func TestGenerateValidation(t *testing.T) {
	ts := newGeneratingServer(t)
	alice := createUser(t, ts, "alice")
	bad := []url.Values{
		{"topic": {""}, "length": {"5"}, "freshness": {"7"}, "language": {"en"}, "voice": {"female"}},
		{"topic": {"x"}, "length": {"7"}, "freshness": {"7"}, "language": {"en"}, "voice": {"female"}},
		{"topic": {"x"}, "length": {"5"}, "freshness": {"2"}, "language": {"en"}, "voice": {"female"}},
		{"topic": {"x"}, "length": {"5"}, "freshness": {"7"}, "language": {"fr"}, "voice": {"female"}},
		{"topic": {"x"}, "length": {"5"}, "freshness": {"7"}, "language": {"en"}, "voice": {"robot"}},
		{"topic": {"x"}, "length": {"5"}, "freshness": {"7"}, "language": {"en"}},
		{"topic": {strings.Repeat("a", 501)}, "length": {"5"}, "freshness": {"7"}, "language": {"en"}, "voice": {"female"}},
	}
	for i, form := range bad {
		resp := postGenerate(t, ts, alice, form)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("case %d: status = %d, want 400", i, resp.StatusCode)
		}
	}
}

func TestGenerationIsOwnerScoped(t *testing.T) {
	ts := newGeneratingServer(t)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")

	resp := postGenerate(t, ts, alice, url.Values{
		"topic": {"private"}, "length": {"2"}, "freshness": {"1"}, "language": {"en"}, "voice": {"male"},
	})
	var g store.Generation
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()

	resp = do(t, "GET", ts.URL+"/me/generations/"+g.ID, bob.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob sees alice's generation: %d", resp.StatusCode)
	}

	// Drain the pipeline before the test's TempDir is torn down.
	waitGenerationDone(t, ts, alice, g.ID)
}

func waitGenerationDone(t *testing.T, ts *httptest.Server, a account, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := do(t, "GET", ts.URL+"/me/generations/"+id, a.publishCreds(), nil, "")
		var v struct {
			Stage string `json:"stage"`
		}
		json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if v.Stage == store.GenDone || v.Stage == store.GenFailed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("generation never finished")
}
