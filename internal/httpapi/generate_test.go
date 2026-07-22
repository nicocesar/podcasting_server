package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
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

func (instantAPI) EnsureAgent(_ context.Context, name, _, _ string, _ []map[string]any) (string, error) {
	return "agent-" + name, nil
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
func (instantAPI) DeleteSession(context.Context, string) error              { return nil }
func (instantAPI) LastAgentMessage(context.Context, string) (string, error) { return "", nil }
func (instantAPI) LastToolUse(_ context.Context, sessionID, name string) (*generation.ToolUse, error) {
	// The deliverable differs by program: prose for the spoken ones, a
	// composition plan for the ambient one. A submission on the wrong
	// shape is rejected and resubmitted forever, so this has to match the
	// tool actually being polled for.
	input := []byte(`{"title":"Generated","summary":"A summary.","script":"Spoken words.","sources":[]}`)
	if name == "submit_music" {
		input = []byte(`{"title":"Composed","summary":"A summary.","movements":[{"prompt":"warm rhodes, 60bpm","duration_ms":300000}]}`)
	}
	return &generation.ToolUse{ID: sessionID + "-use-0", Input: input}, nil
}
func (instantAPI) SendToolResult(context.Context, string, string, string, bool) error { return nil }
func (instantAPI) CompleteJSON(context.Context, string, string, map[string]any, int) (string, generation.Usage, error) {
	return `{"characters":[{"name":"Lila","description":"A brave young fox."}]}`,
		generation.Usage{InputTokens: 20, OutputTokens: 10}, nil
}

type instantEngine struct{}

func (instantEngine) Name() string { return "instant" }
func (instantEngine) Synthesize(context.Context, string, tts.Voice) ([]byte, error) {
	return []byte("MP3!"), nil
}

// instantComposer is a music client that renders instantly, for the
// server variants that offer the ambient program.
type instantComposer struct{}

func (instantComposer) Model() string { return "music-test" }
func (instantComposer) Compose(context.Context, string, int) ([]byte, error) {
	return []byte("MUSIC!"), nil
}

// newGeneratingServer is newTestServer with the Generation feature on,
// and no music client — the default shape for an instance without an
// ElevenLabs key.
func newGeneratingServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newGeneratingServerWith(t, nil)
}

// newGeneratingServerWith builds the generating server with an optional
// music client, so tests can cover the ambient program being offered and
// being absent.
func newGeneratingServerWith(t *testing.T, composer generation.Composer) *httptest.Server {
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
			Music:        composer,
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

// TestAmbientHiddenWithoutMusicClient: with no music client the ambient
// program is absent from the chooser and its URL 404s, rather than taking
// a request it cannot fulfil.
func TestAmbientHiddenWithoutMusicClient(t *testing.T) {
	ts := newGeneratingServer(t)
	alice := createUser(t, ts, "alice")

	resp := do(t, "GET", ts.URL+"/me/generate", alice.publishCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "/me/generate/ambient") {
		t.Errorf("ambient offered without a music client:\n%s", body)
	}

	// Hiding the card is not enough: the URL itself must not answer.
	resp = do(t, "GET", ts.URL+"/me/generate/ambient", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /me/generate/ambient = %d, want 404", resp.StatusCode)
	}
}

// TestAmbientFormOmitsVoiceFields: a composed piece has no narrator, so
// the voice and provider selects must not appear — a form that posts
// them would be asking for something the pipeline ignores.
func TestAmbientFormOmitsVoiceFields(t *testing.T) {
	ts := newGeneratingServerWith(t, instantComposer{})
	alice := createUser(t, ts, "alice")

	resp := do(t, "GET", ts.URL+"/me/generate", alice.publishCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "/me/generate/ambient") {
		t.Fatalf("ambient missing from the chooser:\n%s", body)
	}

	resp = do(t, "GET", ts.URL+"/me/generate/ambient", alice.publishCreds(), nil, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ambient form: %d\n%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "Voice provider") || strings.Contains(string(body), `name="voice"`) {
		t.Errorf("ambient form still offers voice fields:\n%s", body)
	}
	// The shared fields it does keep: the mood textarea and the length
	// and language selects.
	for _, want := range []string{"Mood", `name="length"`, `name="language"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("ambient form missing %q", want)
		}
	}
	// Freshness and the cast picker belong to the other programs.
	if strings.Contains(string(body), "Freshness") {
		t.Error("ambient form should not offer a freshness window")
	}
}

// TestAmbientSubmitWithoutVoice: the form posts no voice or provider, and
// the submission must still be accepted.
func TestAmbientSubmitWithoutVoice(t *testing.T) {
	ts := newGeneratingServerWith(t, instantComposer{})
	alice := createUser(t, ts, "alice")

	resp := do(t, "POST", ts.URL+"/me/generate/ambient", alice.publishCreds(),
		strings.NewReader(url.Values{
			"topic": {"rain on a window"}, "length": {"5"}, "language": {"en"},
		}.Encode()), "application/x-www-form-urlencoded")
	var g store.Generation
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if g.Voice != "" || g.Provider != "" {
		t.Errorf("music generation carries voice %q / provider %q", g.Voice, g.Provider)
	}
	waitSettled(t, ts, alice, g.ID)
}

// TestProgressPageWordingPerTemplate: the progress page must describe the
// program that was actually asked for. A composed piece has no script to
// research and no voice to record, and nothing about it is "timeless".
func TestProgressPageWordingPerTemplate(t *testing.T) {
	cases := []struct {
		name     string
		tpl      string
		form     url.Values
		want     []string
		unwanted []string
	}{
		{
			name: "ambient",
			tpl:  "ambient",
			form: url.Values{"topic": {"rain on a window"}, "length": {"5"}, "language": {"en"}},
			want: []string{"Composing your music", "Planning the composition", "Composing"},
			// The spoken pipeline's vocabulary, and the freshness clause
			// the ambient form never collects. ("script" alone would match
			// the page's own <script> tag.)
			unwanted: []string{"writing the script", "Researching", "Voicing", "timeless"},
		},
		{
			name:     "news keeps its wording",
			tpl:      "news",
			form:     url.Values{"topic": {"fusion"}, "length": {"5"}, "freshness": {"7"}, "language": {"en"}, "voice": {"female"}},
			want:     []string{"Generating an episode", "Researching &amp; writing the script", "Voicing", "last 7 days"},
			unwanted: []string{"Composing"},
		},
		{
			name:     "stories keeps its age range",
			tpl:      "stories",
			form:     url.Values{"topic": {"a dragon"}, "length": {"5"}, "age": {"5-7"}, "language": {"en"}, "voice": {"female"}},
			want:     []string{"Generating a story", "Writing the story", "for ages 5-7"},
			unwanted: []string{"timeless", "Composing"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newGeneratingServerWith(t, instantComposer{})
			alice := createUser(t, ts, "alice")

			resp := do(t, "POST", ts.URL+"/me/generate/"+tc.tpl, alice.publishCreds(),
				strings.NewReader(tc.form.Encode()), "application/x-www-form-urlencoded")
			var g store.Generation
			if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			// Ask for HTML: the progress page, not the JSON poll. The
			// page is a browser surface, so it authenticates by session
			// cookie rather than the publish token.
			req, _ := http.NewRequest("GET", ts.URL+"/me/generations/"+g.ID, nil)
			req.AddCookie(&http.Cookie{Name: "session", Value: alice.Session})
			req.Header.Set("Accept", "text/html")
			page, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(page.Body)
			page.Body.Close()

			for _, want := range tc.want {
				if !strings.Contains(string(body), want) {
					t.Errorf("progress page missing %q", want)
				}
			}
			for _, bad := range tc.unwanted {
				if strings.Contains(string(body), bad) {
					t.Errorf("progress page should not mention %q:\n%s", bad, body)
				}
			}
			// The POST kicked a real run; let it land before the subtest's
			// temp dir goes away underneath it.
			waitSettled(t, ts, alice, g.ID)
		})
	}
}

// waitSettled polls a generation until it reaches a terminal stage, so a
// test that starts one does not leave a goroutine writing into a temp
// directory that is being removed.
func waitSettled(t *testing.T, ts *httptest.Server, a account, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp := do(t, "GET", ts.URL+"/me/generations/"+id, a.publishCreds(), nil, "")
		var v struct {
			Stage string `json:"stage"`
		}
		json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if v.Stage == "done" || v.Stage == "failed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("generation never settled")
}

// TestLanguageLabelPerTemplate: the ambient form must not imply the
// language does anything to the audio, because it does not — it never
// reaches the Music API.
func TestLanguageLabelPerTemplate(t *testing.T) {
	ts := newGeneratingServerWith(t, instantComposer{})
	alice := createUser(t, ts, "alice")

	for tpl, want := range map[string]string{
		"ambient": "Title &amp; summary language",
		"news":    "Output language",
	} {
		resp := do(t, "GET", ts.URL+"/me/generate/"+tpl, alice.publishCreds(), nil, "")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), want) {
			t.Errorf("%s form: want label %q", tpl, want)
		}
	}
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

	// The chooser lists every program.
	resp := do(t, "GET", ts.URL+"/me/generate", alice.publishCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), "/me/generate/news") ||
		!strings.Contains(string(body), "/me/generate/stories") {
		t.Fatalf("chooser: %d\n%s", resp.StatusCode, body)
	}

	// The news form renders.
	resp = do(t, "GET", ts.URL+"/me/generate/news", alice.publishCreds(), nil, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Freshness") {
		t.Fatalf("form: %d\n%s", resp.StatusCode, body)
	}
	// The provider dropdown offers Auto plus the configured engines.
	if !strings.Contains(string(body), "Voice provider") || !strings.Contains(string(body), `<option value="instant">`) {
		t.Fatalf("provider dropdown missing:\n%s", body)
	}

	// Submitting starts a Generation and answers JSON (non-browser).
	resp = postGenerate(t, ts, alice, url.Values{
		"topic": {"fusion energy"}, "length": {"5"}, "freshness": {"7"}, "language": {"en"}, "voice": {"female"},
		"provider": {"instant"},
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
	if g.ID == "" || g.Stage != store.GenResearching || g.Provider != "instant" {
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
		{"topic": {strings.Repeat("a", 2001)}, "length": {"5"}, "freshness": {"7"}, "language": {"en"}, "voice": {"female"}},
		{"topic": {"x"}, "length": {"5"}, "freshness": {"7"}, "language": {"en"}, "voice": {"female"}, "provider": {"nope"}},
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

func postGenerateStories(t *testing.T, ts *httptest.Server, a account, form url.Values) *http.Response {
	t.Helper()
	return do(t, "POST", ts.URL+"/me/generate/stories", a.publishCreds(),
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
}

// generateStory drives one stories Generation to done and returns the
// published episode.
func generateStory(t *testing.T, ts *httptest.Server, a account, form url.Values) store.Episode {
	t.Helper()
	resp := postGenerateStories(t, ts, a, form)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("start story: %d %s", resp.StatusCode, b)
	}
	var g store.Generation
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if g.Template != "stories" {
		t.Fatalf("generation template = %q", g.Template)
	}
	waitGenerationDone(t, ts, a, g.ID)

	resp = do(t, "GET", ts.URL+"/me/generations/"+g.ID, a.publishCreds(), nil, "")
	var v struct {
		Stage       string `json:"stage"`
		Error       string `json:"error"`
		EpisodeSlug string `json:"episode_slug"`
	}
	json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if v.Stage != store.GenDone {
		t.Fatalf("story generation ended %q: %s", v.Stage, v.Error)
	}
	resp = do(t, "GET", ts.URL+"/me/episodes", a.publishCreds(), nil, "")
	var eps []store.Episode
	json.NewDecoder(resp.Body).Decode(&eps)
	resp.Body.Close()
	for _, ep := range eps {
		if ep.Slug == v.EpisodeSlug {
			return ep
		}
	}
	t.Fatalf("episode %q not listed", v.EpisodeSlug)
	return store.Episode{}
}

var storyForm = url.Values{
	"topic": {"a dragon afraid of heights"}, "length": {"2"}, "age": {"5-7"},
	"language": {"en"}, "voice": {"female"},
}

func TestGenerateStoriesFlow(t *testing.T) {
	ts := newGeneratingServer(t)
	alice := createUser(t, ts, "alice")
	bob := createUser(t, ts, "bob")

	// The stories form: age band and save-characters, no freshness, and no
	// cast picker while there is nothing to bring back.
	resp := do(t, "GET", ts.URL+"/me/generate/stories", alice.publishCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Listener age") ||
		!strings.Contains(page, "save_characters") {
		t.Fatalf("stories form: %d\n%s", resp.StatusCode, page)
	}
	if strings.Contains(page, "Freshness") || strings.Contains(page, "Returning characters") {
		t.Fatalf("stories form has foreign fields:\n%s", page)
	}

	// Unknown programs are a 404.
	resp = do(t, "GET", ts.URL+"/me/generate/nope", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown template: %d", resp.StatusCode)
	}

	// A story with save-characters ends up with its cast on the episode.
	form := url.Values{}
	maps.Copy(form, storyForm)
	form.Set("save_characters", "1")
	ep := generateStory(t, ts, alice, form)
	if ep.Template != "stories" || len(ep.Characters) == 0 || ep.Characters[0].Name != "Lila" {
		t.Fatalf("episode = %+v", ep)
	}

	// The cast picker now offers it…
	resp = do(t, "GET", ts.URL+"/me/generate/stories", alice.publishCreds(), nil, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Returning characters") || !strings.Contains(string(body), "Lila") {
		t.Fatalf("cast picker missing:\n%s", body)
	}

	// …and submitting with that cast is accepted.
	form = url.Values{}
	maps.Copy(form, storyForm)
	form.Set("cast", "alice/"+ep.Slug)
	generateStory(t, ts, alice, form)

	// Sharing the episode brings the cast to bob's picker too (characters
	// live on the canonical episode; ADR 0006).
	resp = do(t, "POST", ts.URL+"/me/feed/alice/"+ep.Slug+"/share", alice.publishCreds(),
		strings.NewReader(`{"to":"bob"}`), "application/json")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("share: %d", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/me/generate/stories", bob.publishCreds(), nil, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Lila") || !strings.Contains(string(body), "alice/"+ep.Slug) {
		t.Fatalf("shared cast missing from bob's picker:\n%s", body)
	}
}

func TestGenerateStoriesValidation(t *testing.T) {
	ts := newGeneratingServer(t)
	alice := createUser(t, ts, "alice")
	bad := []url.Values{
		{"topic": {"x"}, "length": {"2"}, "language": {"en"}, "voice": {"female"}},                                  // no age
		{"topic": {"x"}, "length": {"2"}, "age": {"0-99"}, "language": {"en"}, "voice": {"female"}},                 // bad age
		{"topic": {"x"}, "length": {"2"}, "age": {"5-7"}, "language": {"en"}, "voice": {"female"}, "cast": {"x"}},   // malformed cast
		{"topic": {"x"}, "length": {"2"}, "age": {"5-7"}, "language": {"en"}, "voice": {"female"}, "cast": {"a/b"}}, // cast not in feed
		{"topic": {""}, "length": {"2"}, "age": {"5-7"}, "language": {"en"}, "voice": {"female"}},                   // no idea
	}
	for i, form := range bad {
		resp := postGenerateStories(t, ts, alice, form)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("case %d: status = %d, want 400", i, resp.StatusCode)
		}
	}
}

func TestCharacterBackfill(t *testing.T) {
	ts := newGeneratingServer(t)
	alice := createUser(t, ts, "alice")

	// Published without the checkbox: no characters yet.
	ep := generateStory(t, ts, alice, storyForm)
	if len(ep.Characters) != 0 {
		t.Fatalf("characters before backfill = %+v", ep.Characters)
	}

	resp := do(t, "POST", ts.URL+"/me/episodes/"+ep.Slug+"/characters", alice.publishCreds(), nil, "")
	var got store.Episode
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(got.Characters) == 0 || got.Characters[0].Name != "Lila" {
		t.Fatalf("backfill: %d %+v", resp.StatusCode, got)
	}

	// A non-story episode has no cast to extract.
	news := url.Values{
		"topic": {"fusion"}, "length": {"2"}, "freshness": {"7"}, "language": {"en"}, "voice": {"female"},
	}
	resp = postGenerate(t, ts, alice, news)
	var g store.Generation
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	waitGenerationDone(t, ts, alice, g.ID)
	resp = do(t, "GET", ts.URL+"/me/generations/"+g.ID, alice.publishCreds(), nil, "")
	var v struct {
		EpisodeSlug string `json:"episode_slug"`
	}
	json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	resp = do(t, "POST", ts.URL+"/me/episodes/"+v.EpisodeSlug+"/characters", alice.publishCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("news backfill: %d, want 409", resp.StatusCode)
	}
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
