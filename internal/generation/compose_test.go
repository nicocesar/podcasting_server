package generation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

// musicInput is a three-movement plan totalling 25 minutes, matching the
// generation newMusicGeneration asks for.
const musicInput = `{"title":"The Long Room","summary":"Slow piano for a rainy evening.","movements":[` +
	`{"prompt":"warm rhodes, 60bpm, tape hiss","duration_ms":600000},` +
	`{"prompt":"strings enter, warmer, 60bpm","duration_ms":600000},` +
	`{"prompt":"fade to solo piano, 60bpm","duration_ms":300000}]}`

// fakeComposer stands in for internal/music. failures[i] fails call i+1
// (1-based) so a test can make a specific attempt blow up.
type fakeComposer struct {
	mu       sync.Mutex
	calls    int
	prompts  []string
	durs     []int
	failures map[int]error
	piece    []byte
}

func newFakeComposer() *fakeComposer {
	return &fakeComposer{failures: map[int]error{}, piece: []byte("MUSIC!")}
}

func (c *fakeComposer) Model() string { return "music-test" }

func (c *fakeComposer) Compose(_ context.Context, prompt string, durationMS int) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if err, ok := c.failures[c.calls]; ok {
		return nil, err
	}
	c.prompts = append(c.prompts, prompt)
	c.durs = append(c.durs, durationMS)
	return c.piece, nil
}

func musicRunner(st store.Store, api API, mus Composer, engines ...tts.Engine) *Runner {
	return NewRunner(Config{
		Store:          st,
		API:            api,
		Engines:        engines,
		Music:          mus,
		Model:          "claude-test",
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval:   5 * time.Millisecond,
		ComposeBackoff: time.Millisecond,
	})
}

func newMusicGeneration() store.Generation {
	return store.Generation{
		UserID: "alice", ID: "gen-music",
		Template: "ambient", Topic: "rain on a window, late evening",
		LengthMinutes: 25, Language: "en",
		Stage: store.GenResearching, Active: true, CreatedAt: time.Now().UTC(),
	}
}

func musicAPI() *fakeAPI {
	api := newFakeAPI()
	api.toolName = submitMusicToolName
	api.submissions = []string{musicInput}
	return api
}

// waitMusicStage is waitStage for the music generation's id, used where
// the run is kicked into the background rather than driven inline.
func waitMusicStage(t *testing.T, st store.Store, want string) store.Generation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g, err := st.GetGeneration(context.Background(), "alice", "gen-music")
		if err != nil {
			t.Fatal(err)
		}
		if g.Stage == want {
			return g
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("generation never reached %q", want)
	return store.Generation{}
}

// runToCompletion drives a generation and returns the final record.
func runToCompletion(t *testing.T, r *Runner, st store.Store, g store.Generation) store.Generation {
	t.Helper()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.run(g)
	out, err := st.GetGeneration(context.Background(), g.UserID, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestComposeAndPublish is the happy path end to end: agent plans, every
// movement renders, the pieces concatenate, an episode lands.
func TestComposeAndPublish(t *testing.T) {
	st := testStore(t)
	api := musicAPI()
	mus := newFakeComposer()
	r := musicRunner(st, api, mus)

	g := runToCompletion(t, r, st, newMusicGeneration())

	if g.Stage != store.GenDone {
		t.Fatalf("stage = %q, want done (error: %s)", g.Stage, g.Error)
	}
	if mus.calls != 3 {
		t.Errorf("compose calls = %d, want 3", mus.calls)
	}
	if got := strings.Join(mus.prompts, "|"); !strings.HasPrefix(got, "warm rhodes") {
		t.Errorf("movements rendered out of order: %q", got)
	}
	if want := []int{600_000, 600_000, 300_000}; !slices.Equal(mus.durs, want) {
		t.Errorf("durations = %v, want %v", mus.durs, want)
	}
	if g.MusicCalls != 3 {
		t.Errorf("MusicCalls = %d, want 3", g.MusicCalls)
	}
	if g.MusicMillis != 1_500_000 {
		t.Errorf("MusicMillis = %d, want 1500000", g.MusicMillis)
	}
	if g.MusicModel != "music-test" {
		t.Errorf("MusicModel = %q", g.MusicModel)
	}
	// The TTS meters belong to the spoken programs and must stay clean,
	// or every future cost query has to ask which kind of episode it is
	// looking at.
	if g.TTSCharacters != 0 || g.TTSAttempts != 0 || g.TTSEngine != "" {
		t.Errorf("TTS meters touched by a music generation: %+v", g)
	}
	if g.VoicedChunks != 3 || g.TotalChunks != 3 {
		t.Errorf("progress = %d/%d, want 3/3", g.VoicedChunks, g.TotalChunks)
	}

	ep, err := st.GetEpisode(context.Background(), "alice", g.EpisodeSlug)
	if err != nil {
		t.Fatalf("episode not published: %v", err)
	}
	if ep.Title != "The Long Room" {
		t.Errorf("title = %q", ep.Title)
	}
	if ep.Template != "ambient" {
		t.Errorf("template = %q", ep.Template)
	}
	if ep.Description != "Slow piano for a rainy evening." {
		t.Errorf("description = %q", ep.Description)
	}
	// Three movements appended as raw frames, and nothing else: no credit
	// outro, because there is no voice to credit.
	audio := readAudio(t, st, "alice", g.EpisodeSlug)
	if want := "MUSIC!MUSIC!MUSIC!"; audio != want {
		t.Errorf("audio = %q, want %q", audio, want)
	}
}

// TestComposeNeverVoices guards the routing: a music generation must not
// touch a TTS engine, which would both cost money and put a narrator on a
// track that is meant to have none.
func TestComposeNeverVoices(t *testing.T) {
	st := testStore(t)
	engine := &countingEngine{}
	r := musicRunner(st, musicAPI(), newFakeComposer(), engine)

	g := runToCompletion(t, r, st, newMusicGeneration())

	if g.Stage != store.GenDone {
		t.Fatalf("stage = %q, want done (error: %s)", g.Stage, g.Error)
	}
	if engine.calls != 0 {
		t.Errorf("TTS engine called %d times for a music generation", engine.calls)
	}
}

// TestComposeRetriesMovementInPlace is the money test: a transient
// failure on one movement must not throw away the movements already
// rendered and paid for.
func TestComposeRetriesMovementInPlace(t *testing.T) {
	st := testStore(t)
	mus := newFakeComposer()
	// Calls 1 and 2 render movements 1 and 2; call 3 (movement 3) fails
	// once, and the retry succeeds.
	mus.failures[3] = errors.New("upstream hiccup")
	r := musicRunner(st, musicAPI(), mus)

	g := runToCompletion(t, r, st, newMusicGeneration())

	if g.Stage != store.GenDone {
		t.Fatalf("stage = %q, want done (error: %s)", g.Stage, g.Error)
	}
	if mus.calls != 4 {
		t.Errorf("compose calls = %d, want 4 (3 movements + 1 retry)", mus.calls)
	}
	// Every request is metered, including the one that failed.
	if g.MusicCalls != 4 {
		t.Errorf("MusicCalls = %d, want 4", g.MusicCalls)
	}
	// Duration is metered per movement, not per call: the failed attempt
	// rendered nothing, so it must not inflate the composed total.
	if g.MusicMillis != 1_500_000 {
		t.Errorf("MusicMillis = %d, want 1500000 — a failed attempt inflated it", g.MusicMillis)
	}
	if audio := readAudio(t, st, "alice", g.EpisodeSlug); audio != "MUSIC!MUSIC!MUSIC!" {
		t.Errorf("audio = %q, want three movements", audio)
	}
}

// TestComposeFailsAfterExhaustingRetries: a movement that keeps failing
// fails the run, and the meters still record everything spent trying.
func TestComposeFailsAfterExhaustingRetries(t *testing.T) {
	st := testStore(t)
	mus := newFakeComposer()
	for i := 3; i <= 3+composeAttempts; i++ {
		mus.failures[i] = errors.New("upstream down")
	}
	r := musicRunner(st, musicAPI(), mus)

	g := runToCompletion(t, r, st, newMusicGeneration())

	if g.Stage != store.GenFailed {
		t.Fatalf("stage = %q, want failed", g.Stage)
	}
	if !strings.Contains(g.Error, "movement 3/3") {
		t.Errorf("error should name the movement that failed: %q", g.Error)
	}
	if g.MusicCalls != 2+composeAttempts {
		t.Errorf("MusicCalls = %d, want %d", g.MusicCalls, 2+composeAttempts)
	}
	// The two movements that did render are still counted — they were
	// paid for, and a retry will pay for them again.
	if g.MusicMillis != 1_200_000 {
		t.Errorf("MusicMillis = %d, want 1200000 (the two that rendered)", g.MusicMillis)
	}
	// The plan survives the failure, so Retry resumes at composing rather
	// than spending another agent session.
	if g.Script == "" {
		t.Error("composition checkpoint lost; a retry would re-run the agent")
	}
}

// TestRetryResumesAtComposing pins that the shared Script checkpoint does
// for a composed piece what it does for a script.
func TestRetryResumesAtComposing(t *testing.T) {
	st := testStore(t)
	g := newMusicGeneration()
	g.Stage = store.GenFailed
	g.Script = musicInput
	g.Error = "boom"
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	mus := newFakeComposer()
	r := musicRunner(st, musicAPI(), mus)

	out, err := r.Retry(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stage != store.GenVoicing {
		t.Fatalf("stage = %q, want voicing (the composing stage)", out.Stage)
	}
	// Retry kicks the run; let it land so its writes do not outlive the
	// test's temp dir.
	waitMusicStage(t, st, store.GenDone)
	if r.api.(*fakeAPI).sessions != 0 {
		t.Error("Retry started an agent session despite a stored composition")
	}
	if mus.calls != 3 {
		t.Errorf("compose calls = %d, want 3 — the stored plan should render as-is", mus.calls)
	}
}

// TestComposeRejectsCorruptCheckpoint: a plan that cannot be decoded must
// fail loudly rather than publish silence.
func TestComposeRejectsCorruptCheckpoint(t *testing.T) {
	st := testStore(t)
	g := newMusicGeneration()
	g.Stage = store.GenVoicing
	g.Script = `{"title":"T","summary":"S","movements":[]}`
	r := musicRunner(st, musicAPI(), newFakeComposer())

	out := runToCompletion(t, r, st, g)
	if out.Stage != store.GenFailed {
		t.Fatalf("stage = %q, want failed", out.Stage)
	}
	if !strings.Contains(out.Error, "no movements") {
		t.Errorf("error = %q", out.Error)
	}
}

// TestComposeWithoutClientFails covers the belt to AvailableTemplates'
// braces: a stored ambient generation must not panic when the instance
// has no music client.
func TestComposeWithoutClientFails(t *testing.T) {
	st := testStore(t)
	g := newMusicGeneration()
	g.Stage = store.GenVoicing
	g.Script = musicInput
	r := musicRunner(st, musicAPI(), nil)

	out := runToCompletion(t, r, st, g)
	if out.Stage != store.GenFailed {
		t.Fatalf("stage = %q, want failed", out.Stage)
	}
	if !strings.Contains(out.Error, "no music client") {
		t.Errorf("error = %q", out.Error)
	}
}

// TestAvailableTemplates: the ambient program is offered only when this
// instance can actually compose.
func TestAvailableTemplates(t *testing.T) {
	st := testStore(t)
	withMusic := musicRunner(st, musicAPI(), newFakeComposer())
	if !slices.Contains(withMusic.AvailableTemplates(), "ambient") {
		t.Error("ambient missing with a composer configured")
	}
	without := musicRunner(st, musicAPI(), nil)
	if slices.Contains(without.AvailableTemplates(), "ambient") {
		t.Error("ambient offered without a composer")
	}
	// The spoken programs are never affected either way.
	for _, ids := range [][]string{withMusic.AvailableTemplates(), without.AvailableTemplates()} {
		for _, want := range []string{"news", "stories"} {
			if !slices.Contains(ids, want) {
				t.Errorf("%s missing from %v", want, ids)
			}
		}
	}
}

// TestBootstrapSkipsUnavailableTemplates: no composer, no composer agent
// pushed to the platform.
func TestBootstrapSkipsUnavailableTemplates(t *testing.T) {
	st := testStore(t)
	api := newFakeAPI()
	musicRunner(st, api, nil, fakeEngine{name: "fake"}).Bootstrap(context.Background())
	if slices.Contains(api.agents, "podcasting-composer") {
		t.Errorf("provisioned the composer without a music client: %v", api.agents)
	}

	api2 := newFakeAPI()
	musicRunner(st, api2, newFakeComposer(), fakeEngine{name: "fake"}).Bootstrap(context.Background())
	if !slices.Contains(api2.agents, "podcasting-composer") {
		t.Errorf("composer not provisioned with a music client: %v", api2.agents)
	}
}

// TestComposerRejectionRoundTrip: a plan that breaks the duration ceiling
// is rejected back to the agent, which resubmits a valid one.
func TestComposerRejectionRoundTrip(t *testing.T) {
	st := testStore(t)
	api := musicAPI()
	bad := `{"title":"T","summary":"S","movements":[{"prompt":"p","duration_ms":1500000}]}`
	api.submissions = []string{bad, musicInput}
	mus := newFakeComposer()
	r := musicRunner(st, api, mus)

	g := runToCompletion(t, r, st, newMusicGeneration())

	if g.Stage != store.GenDone {
		t.Fatalf("stage = %q, want done (error: %s)", g.Stage, g.Error)
	}
	var sawRejection bool
	for _, res := range api.results["sess-1"] {
		if res.isError && strings.Contains(res.text, "submit_music") {
			sawRejection = true
		}
	}
	if !sawRejection {
		t.Errorf("agent was not told to resubmit on submit_music: %+v", api.results["sess-1"])
	}
	if mus.calls != 3 {
		t.Errorf("compose calls = %d, want 3 (only the accepted plan renders)", mus.calls)
	}
}

// TestAmbientTaskMessage: the composer is told the total in the unit its
// tool schema uses, since the durations have to add up to it.
func TestAmbientTaskMessage(t *testing.T) {
	tpl, _ := TemplateByID("ambient")
	msg := tpl.TaskMessage(newMusicGeneration(), time.Now())
	for _, want := range []string{"rain on a window", "25 minutes", "1500000", "English"} {
		if !strings.Contains(msg, want) {
			t.Errorf("task message missing %q:\n%s", want, msg)
		}
	}
}

// countingEngine records whether the TTS path was entered at all.
type countingEngine struct {
	mu    sync.Mutex
	calls int
}

func (e *countingEngine) Name() string { return "counting" }
func (e *countingEngine) Synthesize(context.Context, string, tts.Voice) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return []byte("MP3!"), nil
}

func readAudio(t *testing.T, st store.Store, owner, slug string) string {
	t.Helper()
	a, err := st.OpenAudio(context.Background(), owner, slug)
	if err != nil {
		t.Fatalf("read audio: %v", err)
	}
	if a.Content == nil {
		t.Fatal("no audio content")
	}
	defer a.Content.Close()
	b, err := io.ReadAll(a.Content)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
