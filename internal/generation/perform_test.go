package generation

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/mix"
	"github.com/nicocesar/podcasting_server/internal/sfx"
	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

// fakeDialogue stands in for the ElevenLabs dialogue engine, recording
// what each request was cast to so a test can check that a Spanish line
// went to a Spanish voice.
type fakeDialogue struct {
	mu       sync.Mutex
	requests [][]tts.DialogueInput
	audio    []byte
	err      error
}

func (f *fakeDialogue) Name() string { return "elevenlabs" }

func (f *fakeDialogue) Synthesize(context.Context, string, tts.Voice) ([]byte, error) {
	return f.audio, f.err
}

func (f *fakeDialogue) SynthesizeDialogue(_ context.Context, in []tts.DialogueInput) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.requests = append(f.requests, in)
	return f.audio, nil
}

// fakeSFX counts renders and reports whichever cache outcome the test set.
type fakeSFX struct {
	mu       sync.Mutex
	cues     []string
	cacheHit bool
	audio    []byte
}

func (f *fakeSFX) Render(_ context.Context, cue sfx.Cue) (sfx.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cues = append(f.cues, cue.Name)
	return sfx.Result{Audio: f.audio, CacheHit: f.cacheHit}, nil
}

// tone renders real MP3 through ffmpeg. The mixer is not faked here: the
// point of this test is the whole performed path, and mixing is where it
// most recently went wrong.
func perfTone(t *testing.T, hz int) []byte {
	t.Helper()
	out, err := exec.Command(mix.Binary, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency="+strconv.Itoa(hz)+":duration=1",
		"-c:a", "libmp3lame", "-b:a", "128k", "-ar", "44100", "-ac", "2",
		"-f", "mp3", "pipe:1").Output()
	if err != nil {
		t.Fatalf("generating fixture: %v", err)
	}
	return out
}

func requirePerfFFmpeg(t *testing.T) {
	t.Helper()
	if mix.Available() {
		return
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		mix.Binary = p
		return
	}
	t.Skip("ffmpeg not available")
}

// storyInput is the farm story the original complaint was about, written
// the way this template is supposed to write it.
// It has to clear the length floor, or ParseStorySubmission rejects it and
// the canned agent resubmits the same thing forever.
var storyInput = mustMarshal(Story{
	Title: "Adventures on the Farm", Summary: "A duck and a pig move into an empty barn.",
	Language: "en", Bed: "soft music box, sleepy",
	Segments: []Segment{
		{Kind: SegSpeech, Speaker: "narrator", Lang: "en", Text: "The barn is empty. " + filler(70)},
		{Kind: SegSpeech, Speaker: "tutor", Lang: "es", Text: "Vacío."},
		{Kind: SegSFX, Cue: "duck_quack"},
		{Kind: SegSpeech, Speaker: "small_squeaky", Lang: "en", Text: "[excited] Quack! " + filler(70)},
		{Kind: SegPause, MS: 800},
		{Kind: SegSpeech, Speaker: "narrator", Lang: "en", Text: "[sleepy] Goodnight. " + filler(40)},
	},
})

// filler pads a segment to a plausible spoken length without inventing
// prose nobody reads. The words are counted, never asserted on.
func filler(words int) string {
	return strings.TrimSpace(strings.Repeat("and the sleepy barn was warm ", words/6))
}

func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func performRunner(st store.Store, api API, d *fakeDialogue, fx SFXRenderer, mus Composer) *Runner {
	return NewRunner(Config{
		Store:        st,
		API:          api,
		Engines:      []tts.Engine{d},
		SFX:          fx,
		Music:        mus,
		Model:        "claude-test",
		Host:         "radio.example.com",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: 5 * time.Millisecond,
	})
}

// storyComposer is a composer whose bed is real audio: the mixer measures
// the bed to lay it under the speech, so a placeholder byte string fails
// the run before the test reaches what it is about.
func storyComposer(t *testing.T) *fakeComposer {
	t.Helper()
	c := newFakeComposer()
	c.piece = perfTone(t, 220)
	return c
}

func newStoryGeneration() store.Generation {
	return store.Generation{
		UserID: "alice", ID: "gen-story",
		Template: "stories", Topic: "a duck and a pig",
		LengthMinutes: 2, Language: "en", TargetLanguage: "es",
		AgeRange: "2-4",
		Stage:    store.GenResearching, Active: true, CreatedAt: time.Now().UTC(),
	}
}

func storyAPI() *fakeAPI {
	api := newFakeAPI()
	api.toolName = submitStoryToolName
	api.submissions = []string{storyInput}
	return api
}

func TestPerformAndPublish(t *testing.T) {
	requirePerfFFmpeg(t)
	st := testStore(t)
	d := &fakeDialogue{audio: perfTone(t, 440)}
	fx := &fakeSFX{audio: perfTone(t, 880)}
	mus := newFakeComposer()
	mus.piece = perfTone(t, 220)
	r := performRunner(st, storyAPI(), d, fx, mus)

	g := runToCompletion(t, r, st, newStoryGeneration())

	if g.Stage != store.GenDone {
		t.Fatalf("stage = %q, want done (error: %s)", g.Stage, g.Error)
	}

	// Consecutive speech is one request; effects and pauses break the run.
	// Plus one more for the credit, which leads the episode.
	if len(d.requests) != 4 {
		t.Errorf("dialogue requests = %d, want 4 (credit + 3 runs)", len(d.requests))
	}
	if len(fx.cues) != 1 || fx.cues[0] != "duck_quack" {
		t.Errorf("cues rendered = %v, want [duck_quack]", fx.cues)
	}
	if mus.calls != 1 {
		t.Errorf("bed compose calls = %d, want 1", mus.calls)
	}

	// The casting rule that the whole template exists for: the Spanish
	// line must not have gone out in the narrator's voice.
	var narrator, tutor string
	for _, req := range d.requests {
		for _, in := range req {
			switch {
			case strings.HasPrefix(in.Text, "The barn is empty."):
				narrator = in.VoiceID
			case in.Text == "Vacío.":
				tutor = in.VoiceID
			}
		}
	}
	if narrator == "" || tutor == "" {
		t.Fatal("did not find both the English and Spanish lines in the requests")
	}
	if narrator == tutor {
		t.Error("the Spanish line was cast to the narrator's voice")
	}

	ep, err := st.GetEpisode(context.Background(), "alice", g.EpisodeSlug)
	if err != nil {
		t.Fatalf("episode not published: %v", err)
	}
	if ep.Template != "stories" {
		t.Errorf("episode template = %q", ep.Template)
	}
	if ep.DurationSec <= 0 {
		t.Errorf("episode has no duration: the mix produced nothing measurable")
	}
}

// TestPerformProgressCountsTheSlowTail is the regression for a run that
// looked hung in production. Counting only pieces made the bar reach 100%
// and then sit there through the bed compose and the mix — the two
// slowest calls in the pipeline — which is indistinguishable from a hang
// and was reported as one.
func TestPerformProgressCountsTheSlowTail(t *testing.T) {
	requirePerfFFmpeg(t)
	st := testStore(t)
	d := &fakeDialogue{audio: perfTone(t, 440)}
	fx := &fakeSFX{audio: perfTone(t, 880)}
	mus := newFakeComposer()
	mus.piece = perfTone(t, 220)
	r := performRunner(st, storyAPI(), d, fx, mus)

	g := runToCompletion(t, r, st, newStoryGeneration())

	var story Story
	if err := json.Unmarshal([]byte(g.Script), &story); err != nil {
		t.Fatal(err)
	}
	pieces := len(Plan(story, tts.DialogueCharBudget))
	if want := pieces + bedAndMixSteps; g.TotalChunks != want {
		t.Errorf("TotalChunks = %d, want %d (%d pieces plus the bed and the mix)",
			g.TotalChunks, want, pieces)
	}
	// And the bar must actually arrive, not stop short of its own total.
	if g.VoicedChunks != g.TotalChunks {
		t.Errorf("progress finished at %d/%d", g.VoicedChunks, g.TotalChunks)
	}
}

func TestPerformRefusesWithoutADialogueEngine(t *testing.T) {
	// Resuming on an instance configured differently from the one that
	// took the request must fail loudly. The silent alternative — voicing
	// it flat in one voice — is the defect this template exists to fix.
	st := testStore(t)
	r := performRunner(st, storyAPI(), nil, &fakeSFX{}, newFakeComposer())
	r.engines = nil

	g := newStoryGeneration()
	g.Stage = store.GenVoicing
	g.Script = storyInput
	g = runToCompletion(t, r, st, g)

	if g.Stage != store.GenFailed {
		t.Fatalf("stage = %q, want failed", g.Stage)
	}
	if g.Error == "" {
		t.Error("failed without saying why")
	}
}

func TestPerformSkipsAnEffectItCannotRender(t *testing.T) {
	// An effect is punctuation. Losing one is not worth losing a story
	// that is already written and largely paid for.
	requirePerfFFmpeg(t)
	st := testStore(t)
	d := &fakeDialogue{audio: perfTone(t, 440)}
	mus := newFakeComposer()
	mus.piece = perfTone(t, 220)
	r := performRunner(st, storyAPI(), d, failingSFX{}, mus)

	g := runToCompletion(t, r, st, newStoryGeneration())

	if g.Stage != store.GenDone {
		t.Fatalf("a failed sound effect took down the whole story: %q / %s", g.Stage, g.Error)
	}
}

type failingSFX struct{}

func (failingSFX) Render(context.Context, sfx.Cue) (sfx.Result, error) {
	return sfx.Result{}, context.DeadlineExceeded
}
