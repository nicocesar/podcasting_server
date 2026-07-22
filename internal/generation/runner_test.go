package generation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

const scriptInput = `{"title":"Fusion This Week","summary":"The state of fusion.","language":"en","script":"Hello. Fusion news. Goodbye.","sources":[{"title":"Igniter","url":"https://i.example","published":"2026-07-07"}]}`

const spanishInput = `{"title":"La fusión esta semana","summary":"El estado de la fusión.","language":"es","script":"Hola. Noticias de fusión. Adiós.","sources":[{"title":"Igniter","url":"https://i.example","published":"2026-07-07"}]}`

// legacyScriptReply is the pre-tool contract: the episode as a fenced
// json block in a chat message.
const legacyScriptReply = "Done.\n```json\n" + scriptInput + "\n```"

type toolResult struct {
	id      string
	text    string
	isError bool
}

// fakeAPI is an in-memory Managed Agents platform: sessions go idle
// immediately; once the task is sent the agent has called submit_episode
// with the first canned submission, and each rejected result advances to
// the next one (the last submission repeats). With legacyReplies set the
// agent predates the tool and answers each sent message with the next
// canned chat reply instead.
type fakeAPI struct {
	mu            sync.Mutex
	sessions      int
	sent          map[string][]string
	results       map[string][]toolResult
	deleted       []string
	submissions   []string
	legacyReplies []string
	agents        []string // names EnsureAgent was called with, in order
	completions   []string // canned CompleteJSON outputs, consumed in order
	completeErr   error
	// toolName is the tool the canned submissions arrive on, so the same
	// fake serves both the spoken programs (submit_episode) and the
	// ambient one (submit_music).
	toolName string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		sent:        map[string][]string{},
		results:     map[string][]toolResult{},
		submissions: []string{scriptInput},
		toolName:    submitToolName,
	}
}

func (f *fakeAPI) EnsureAgent(_ context.Context, name, _, _ string, _ []map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = append(f.agents, name)
	return "agent-" + name, nil
}
func (f *fakeAPI) EnsureEnvironment(context.Context, string) (string, error) { return "env-1", nil }

func (f *fakeAPI) CreateSession(context.Context, string, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions++
	return fmt.Sprintf("sess-%d", f.sessions), nil
}

func (f *fakeAPI) SendMessage(_ context.Context, sessionID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[sessionID] = append(f.sent[sessionID], text)
	return nil
}

func (f *fakeAPI) SessionStatus(context.Context, string) (string, error) { return "idle", nil }

func (f *fakeAPI) SessionUsage(context.Context, string) (Usage, error) {
	return Usage{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 10, CacheWriteTokens: 5}, nil
}

func (f *fakeAPI) LastAgentMessage(_ context.Context, sessionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.sent[sessionID])
	if n == 0 || len(f.legacyReplies) == 0 {
		return "", nil
	}
	if n > len(f.legacyReplies) {
		n = len(f.legacyReplies)
	}
	return f.legacyReplies[n-1], nil
}

func (f *fakeAPI) LastToolUse(_ context.Context, sessionID, name string) (*ToolUse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != f.toolName || len(f.submissions) == 0 || len(f.sent[sessionID]) == 0 {
		return nil, nil
	}
	// Each rejection makes the agent resubmit: the current call is the
	// one after the last rejected result.
	idx := 0
	for _, res := range f.results[sessionID] {
		if res.isError {
			idx++
		}
	}
	if idx >= len(f.submissions) {
		idx = len(f.submissions) - 1
	}
	use := &ToolUse{ID: fmt.Sprintf("%s-use-%d", sessionID, idx), Input: []byte(f.submissions[idx])}
	for _, res := range f.results[sessionID] {
		if res.id == use.ID {
			use.Answered = true
		}
	}
	return use, nil
}

func (f *fakeAPI) SendToolResult(_ context.Context, sessionID, toolUseID, text string, isError bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[sessionID] = append(f.results[sessionID], toolResult{id: toolUseID, text: text, isError: isError})
	return nil
}

func (f *fakeAPI) DeleteSession(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, sessionID)
	return nil
}

func (f *fakeAPI) CompleteJSON(context.Context, string, string, map[string]any, int) (string, Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completeErr != nil {
		return "", Usage{}, f.completeErr
	}
	if len(f.completions) == 0 {
		return `{"characters":[{"name":"Lila","description":"A brave young fox."}]}`, Usage{InputTokens: 20, OutputTokens: 10}, nil
	}
	out := f.completions[0]
	if len(f.completions) > 1 {
		f.completions = f.completions[1:]
	}
	return out, Usage{InputTokens: 20, OutputTokens: 10}, nil
}

type fakeEngine struct {
	name string
	err  error
}

func (e fakeEngine) Name() string { return e.name }
func (e fakeEngine) Synthesize(context.Context, string, tts.Voice) ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	return []byte("MP3!"), nil
}

func testStore(t *testing.T) store.Store {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertUser(context.Background(), store.User{ID: "alice", Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	return st
}

func testRunner(st store.Store, api API, engines ...tts.Engine) *Runner {
	return NewRunner(Config{
		Store:        st,
		API:          api,
		Engines:      engines,
		Model:        "claude-test",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: 5 * time.Millisecond,
	})
}

func newGeneration() store.Generation {
	return store.Generation{
		UserID: "alice", ID: "gen1",
		Topic: "Fusion Energy!", LengthMinutes: 5, FreshnessDays: 7, Language: "en",
		Stage: store.GenResearching, Active: true, CreatedAt: time.Now().UTC(),
	}
}

// waitStage polls until the generation reaches a terminal stage.
func waitStage(t *testing.T, st store.Store, want string) store.Generation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g, err := st.GetGeneration(context.Background(), "alice", "gen1")
		if err != nil {
			t.Fatal(err)
		}
		if g.Stage == want {
			return g
		}
		if g.Stage == store.GenDone || g.Stage == store.GenFailed {
			t.Fatalf("generation ended at %q (error %q), want %q", g.Stage, g.Error, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("generation never reached %q", want)
	return store.Generation{}
}

func TestPipelineHappyPath(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	if g.Active {
		t.Error("done generation still active")
	}
	wantSlug := time.Now().UTC().Format("2006-01-02") + "-fusion-energy"
	if g.EpisodeSlug != wantSlug {
		t.Errorf("slug = %q, want %q", g.EpisodeSlug, wantSlug)
	}
	ep, err := st.GetEpisode(context.Background(), "alice", wantSlug)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Title != "Fusion This Week" {
		t.Errorf("title = %q", ep.Title)
	}
	if !strings.Contains(ep.Description, "The state of fusion.") || !strings.Contains(ep.Description, "https://i.example") {
		t.Errorf("description = %q", ep.Description)
	}
	a, err := st.OpenAudio(context.Background(), "alice", wantSlug)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Content.Close()
	audio, _ := io.ReadAll(a.Content)
	if string(audio) != "MP3!" {
		t.Errorf("audio = %q", audio)
	}
	// Sessions are kept by default so prompts can be improved from the
	// Console traces.
	if len(api.deleted) != 0 {
		t.Errorf("deleted sessions = %v, want none", api.deleted)
	}
	// The task carried the request parameters.
	task := api.sent["sess-1"][0]
	for _, want := range []string{"Fusion Energy!", "750 spoken words", "last 7 days", "English"} {
		if !strings.Contains(task, want) {
			t.Errorf("task missing %q:\n%s", want, task)
		}
	}
	// The submission was acknowledged, unblocking the session.
	if res := api.results["sess-1"]; len(res) != 1 || res[0].isError {
		t.Errorf("tool results = %+v, want a single accepting ack", res)
	}
	// The meters recorded what the run consumed.
	if g.SessionsCount != 1 || g.InputTokens != 100 || g.OutputTokens != 40 ||
		g.CacheReadTokens != 10 || g.CacheWriteTokens != 5 {
		t.Errorf("session meters = %d sessions, %d/%d/%d/%d tokens",
			g.SessionsCount, g.InputTokens, g.OutputTokens, g.CacheReadTokens, g.CacheWriteTokens)
	}
	wantChars := utf8.RuneCountInString("Hello. Fusion news. Goodbye.")
	if g.TTSEngine != "fake" || g.TTSAttempts != 1 || g.TTSCharacters != wantChars {
		t.Errorf("tts meters = engine %q, %d attempts, %d chars (want fake, 1, %d)",
			g.TTSEngine, g.TTSAttempts, g.TTSCharacters, wantChars)
	}
}

// TestStoriesPipeline: a stories Generation provisions the storyteller
// agent (not the news one), sends the story task with the returning cast,
// and — SaveCharacters — extracts the cast onto the published Episode.
func TestStoriesPipeline(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration()
	g.Template = "stories"
	g.FreshnessDays = 0
	g.AgeRange = "5-7"
	g.SaveCharacters = true
	g.Cast = []store.Character{{Name: "Grandpa Bear", Description: "Slow and warm."}}
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	if len(api.agents) != 1 || api.agents[0] != "podcasting-storyteller" {
		t.Errorf("agents ensured = %v, want [podcasting-storyteller]", api.agents)
	}
	task := api.sent["sess-1"][0]
	for _, want := range []string{"aged 5 to 7", "Returning characters", "Grandpa Bear"} {
		if !strings.Contains(task, want) {
			t.Errorf("task missing %q:\n%s", want, task)
		}
	}
	ep, err := st.GetEpisode(context.Background(), "alice", g.EpisodeSlug)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Template != "stories" {
		t.Errorf("episode template = %q", ep.Template)
	}
	if len(ep.Characters) != 1 || ep.Characters[0].Name != "Lila" {
		t.Errorf("episode characters = %+v", ep.Characters)
	}
	// Extraction tokens joined the meters (100+20 in, 40+10 out), without
	// counting as a session.
	if g.SessionsCount != 1 || g.InputTokens != 120 || g.OutputTokens != 50 {
		t.Errorf("meters = %d sessions, %d/%d tokens", g.SessionsCount, g.InputTokens, g.OutputTokens)
	}
}

// A failed extraction never fails the pipeline: the Episode is already
// published, and the dashboard's backfill button covers the gap.
func TestCharacterExtractionFailureIsNonFatal(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	api.completeErr = errors.New("model overloaded")
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration()
	g.Template = "stories"
	g.AgeRange = "all"
	g.SaveCharacters = true
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	ep, err := st.GetEpisode(context.Background(), "alice", g.EpisodeSlug)
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.Characters) != 0 {
		t.Errorf("characters = %+v, want none", ep.Characters)
	}
}

// News Generations — including legacy records with no Template — keep
// using the original agent.
func TestNewsPipelineKeepsItsAgent(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration() // Template empty: a pre-template record
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	waitStage(t, st, store.GenDone)

	if len(api.agents) != 1 || api.agents[0] != "podcasting-generator" {
		t.Errorf("agents ensured = %v, want [podcasting-generator]", api.agents)
	}
}

// Bootstrap warms every template's agent so the first request of either
// kind pays no provisioning latency.
func TestBootstrapProvisionsAllTemplates(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	r := testRunner(st, api, fakeEngine{name: "fake"})
	r.Bootstrap(context.Background())

	want := map[string]bool{"podcasting-generator": true, "podcasting-storyteller": true}
	for _, name := range api.agents {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Errorf("agents not provisioned: %v (got %v)", want, api.agents)
	}
}

func TestDeleteSessionsOptIn(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	r := NewRunner(Config{
		Store:          st,
		API:            api,
		Engines:        []tts.Engine{fakeEngine{name: "fake"}},
		Model:          "claude-test",
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval:   5 * time.Millisecond,
		DeleteSessions: true,
	})
	g := newGeneration()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	waitStage(t, st, store.GenDone)
	if len(api.deleted) != 1 || api.deleted[0] != "sess-1" {
		t.Errorf("deleted sessions = %v, want [sess-1]", api.deleted)
	}
}

// TestWrongLanguageGetsTranslated: the agent researches in Spanish and
// submits a Spanish script; the runner rejects the submission with a
// translation instruction and voices the English resubmission.
func TestWrongLanguageGetsTranslated(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	api.submissions = []string{spanishInput, scriptInput}
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration() // Language: "en"
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	if msgs := api.sent["sess-1"]; len(msgs) != 1 {
		t.Fatalf("messages sent = %d, want 1 (the task; translation goes via the tool result)", len(msgs))
	}
	res := api.results["sess-1"]
	if len(res) != 2 || !res[0].isError || res[1].isError {
		t.Fatalf("tool results = %+v, want a rejection then an ack", res)
	}
	for _, want := range []string{"Translate", "English"} {
		if !strings.Contains(res[0].text, want) {
			t.Errorf("rejection missing %q:\n%s", want, res[0].text)
		}
	}
	// The stored Script is the translated one.
	if !strings.Contains(g.Script, "Fusion This Week") {
		t.Errorf("stored script is not the translation: %s", g.Script)
	}
	ep, err := st.GetEpisode(context.Background(), "alice", g.EpisodeSlug)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Title != "Fusion This Week" {
		t.Errorf("episode title = %q, want the translated title", ep.Title)
	}
}

// TestLegacySessionStillLands: a session created before the
// submit_episode tool (in flight across the deploy) answers with the old
// fenced-block contract and must still produce its Episode.
func TestLegacySessionStillLands(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	api.submissions = nil // the pinned agent version has no tool
	api.legacyReplies = []string{legacyScriptReply}
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)
	if !strings.Contains(g.Script, "Fusion This Week") {
		t.Errorf("stored script = %s", g.Script)
	}
}

func TestSlugCollisionGetsSuffix(t *testing.T) {
	st := testStore(t)
	slug := time.Now().UTC().Format("2006-01-02") + "-fusion-energy"
	_, err := st.UpsertEpisode(context.Background(), store.Episode{
		OwnerID: "alice", Slug: slug, Title: "Existing", PublishedAt: time.Now().UTC(),
	}, strings.NewReader("old"))
	if err != nil {
		t.Fatal(err)
	}

	r := testRunner(st, newFakeAPI(), fakeEngine{name: "fake"})
	g := newGeneration()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	if g.EpisodeSlug != slug+"-2" {
		t.Fatalf("slug = %q, want %q", g.EpisodeSlug, slug+"-2")
	}
	if ep, err := st.GetEpisode(context.Background(), "alice", slug); err != nil || ep.Title != "Existing" {
		t.Fatalf("existing episode was disturbed: %v %v", ep, err)
	}
}

func TestTTSFailureThenRetrySkipsResearch(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()

	broken := testRunner(st, api, fakeEngine{name: "edge", err: errors.New("protocol rotated")})
	g := newGeneration()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	broken.Kick(g)
	g = waitStage(t, st, store.GenFailed)

	if !strings.Contains(g.Error, "voicing") {
		t.Errorf("error = %q, want a voicing failure", g.Error)
	}
	if g.Script == "" {
		t.Fatal("script checkpoint lost on TTS failure")
	}

	// A retry — here after a "restart", i.e. a fresh Runner with a
	// working engine — resumes from the Script, never re-researching.
	fixed := testRunner(st, api, fakeEngine{name: "fake"})
	g, err := fixed.Retry(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if g.Stage != store.GenVoicing {
		t.Fatalf("retry stage = %q, want voicing", g.Stage)
	}
	g = waitStage(t, st, store.GenDone)
	if api.sessions != 1 {
		t.Errorf("sessions created = %d, want 1 (research must not repeat)", api.sessions)
	}
	// The failed voicing attempt is metered, not hidden: two engine
	// attempts total, but the session was only paid for (and counted)
	// once, and characters count only the successful voicing.
	if g.TTSAttempts != 2 {
		t.Errorf("tts attempts = %d, want 2 (failure + retry)", g.TTSAttempts)
	}
	if g.SessionsCount != 1 || g.InputTokens != 100 {
		t.Errorf("session meters after retry = %d sessions, %d input tokens (want 1, 100)",
			g.SessionsCount, g.InputTokens)
	}
	if wantChars := utf8.RuneCountInString("Hello. Fusion news. Goodbye."); g.TTSCharacters != wantChars {
		t.Errorf("tts characters = %d, want %d (counted once)", g.TTSCharacters, wantChars)
	}
}

func TestEngineFallback(t *testing.T) {
	st := testStore(t)
	r := testRunner(st, newFakeAPI(),
		fakeEngine{name: "edge", err: errors.New("down")},
		fakeEngine{name: "google"},
	)
	g := newGeneration()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)
	if g.EpisodeSlug == "" {
		t.Fatal("no episode published")
	}
	if g.TTSEngine != "google" || g.TTSAttempts != 2 {
		t.Errorf("tts meters = engine %q, %d attempts (want google, 2)", g.TTSEngine, g.TTSAttempts)
	}
}

func TestProviderPreferenceReordersEngines(t *testing.T) {
	st := testStore(t)
	r := testRunner(st, newFakeAPI(),
		fakeEngine{name: "edge"},
		fakeEngine{name: "google"},
	)
	g := newGeneration()
	g.Provider = "google" // prefer the engine that is second in the chain
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)
	if g.TTSEngine != "google" || g.TTSAttempts != 1 {
		t.Errorf("tts meters = engine %q, %d attempts (want google, 1)", g.TTSEngine, g.TTSAttempts)
	}
}
