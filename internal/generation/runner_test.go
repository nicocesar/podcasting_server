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

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

const scriptReply = "Done.\n```json\n" +
	`{"title":"Fusion This Week","summary":"The state of fusion.","language":"en","script":"Hello. Fusion news. Goodbye.","sources":[{"title":"Igniter","url":"https://i.example","published":"2026-07-07"}]}` +
	"\n```"

const spanishReply = "Listo.\n```json\n" +
	`{"title":"La fusión esta semana","summary":"El estado de la fusión.","language":"es","script":"Hola. Noticias de fusión. Adiós.","sources":[{"title":"Igniter","url":"https://i.example","published":"2026-07-07"}]}` +
	"\n```"

// fakeAPI is an in-memory Managed Agents platform: sessions go idle
// immediately and answer each sent message with the next canned reply
// (the last reply repeats when messages outnumber replies).
type fakeAPI struct {
	mu       sync.Mutex
	sessions int
	sent     map[string][]string
	deleted  []string
	replies  []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{sent: map[string][]string{}, replies: []string{scriptReply}}
}

func (f *fakeAPI) EnsureAgent(context.Context, string, string, string) (string, error) {
	return "agent-1", nil
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

func (f *fakeAPI) LastAgentMessage(_ context.Context, sessionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.sent[sessionID])
	if n == 0 {
		return "", nil
	}
	if n > len(f.replies) {
		n = len(f.replies)
	}
	return f.replies[n-1], nil
}

func (f *fakeAPI) DeleteSession(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, sessionID)
	return nil
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
// replies with a Spanish script; the runner asks for a translation in the
// same session and voices the English reply that follows.
func TestWrongLanguageGetsTranslated(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	api.replies = []string{spanishReply, scriptReply}
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration() // Language: "en"
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	msgs := api.sent["sess-1"]
	if len(msgs) != 2 {
		t.Fatalf("messages sent = %d, want 2 (task + translation)", len(msgs))
	}
	for _, want := range []string{"Translate", "English"} {
		if !strings.Contains(msgs[1], want) {
			t.Errorf("translation request missing %q:\n%s", want, msgs[1])
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
}
