package generation

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

// recordingEngine is a fake TTS engine that remembers every text it was
// asked to voice, so a test can assert not just which engine won but
// what it actually said. failOn makes it fail for matching text only,
// which is how the credit is failed without failing the script.
type recordingEngine struct {
	name   string
	err    error
	failOn func(string) bool

	mu     sync.Mutex
	spoken []string
}

func (e *recordingEngine) Name() string { return e.name }

func (e *recordingEngine) Synthesize(_ context.Context, text string, _ tts.Voice) ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	if e.failOn != nil && e.failOn(text) {
		return nil, fmt.Errorf("synthetic failure for %q", text)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spoken = append(e.spoken, text)
	return []byte("MP3!"), nil
}

func (e *recordingEngine) said() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.spoken...)
}

// last is the final thing this engine voiced — the credit, when there is
// one, since the sign-off is appended after the script.
func (e *recordingEngine) last() string {
	said := e.said()
	if len(said) == 0 {
		return ""
	}
	return said[len(said)-1]
}

// isCredit spots a sign-off in either supported language, so a test can
// assert an engine voiced no credit at all.
func isCredit(s string) bool {
	return strings.Contains(s, "read by") || strings.Contains(s, "narrado por")
}

// TestCreditOutro pins the sign-off to the engine that actually read the
// episode. The case that matters is the last one: when the requested
// provider fails and the chain falls back, the credit must name the
// engine that rescued the episode, not the one the user asked for.
// Getting that backwards would make the credit lie in exactly the
// situation it exists to expose.
func TestCreditOutro(t *testing.T) {
	tests := []struct {
		name        string
		language    string
		gender      string
		provider    string
		elevenFails bool
		wantEngine  string
		wantCredit  string
	}{
		{
			name: "auto takes the head of the chain", language: "en", gender: "female",
			wantEngine: "edge-tts",
			wantCredit: "This episode was read by Sonia, from Microsoft Edge.",
		},
		{
			name: "preferred provider is credited", language: "en", gender: "male", provider: "elevenlabs",
			wantEngine: "elevenlabs",
			wantCredit: "This episode was read by Christopher, from Eleven Labs.",
		},
		{
			name: "spanish signs off in spanish", language: "es", gender: "male", provider: "elevenlabs",
			wantEngine: "elevenlabs",
			wantCredit: "Este episodio fue narrado por Juan, de Eleven Labs.",
		},
		{
			name: "fallback credits the engine that rescued it", language: "es", gender: "female",
			provider: "elevenlabs", elevenFails: true,
			wantEngine: "edge-tts",
			wantCredit: "Este episodio fue narrado por Elena, de Microsoft Edge.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			api := newFakeAPI()
			// The runner rejects a script whose reported language differs
			// from the Generation's and asks the agent to translate, so a
			// Spanish case must hand back a Spanish script or it spins in
			// researching and never reaches voicing.
			if tc.language == "es" {
				api.submissions = []string{spanishInput}
			}
			edge := &recordingEngine{name: "edge-tts"}
			eleven := &recordingEngine{name: "elevenlabs"}
			if tc.elevenFails {
				eleven.err = fmt.Errorf("http 402: paid_plan_required")
			}
			// Registration order is the fallback chain: edge first, as in
			// main.go, with ElevenLabs reachable only by preference or
			// fallback.
			r := testRunner(st, api, edge, eleven)

			g := newGeneration()
			g.Language, g.Voice, g.Provider = tc.language, tc.gender, tc.provider
			if err := st.PutGeneration(context.Background(), g); err != nil {
				t.Fatal(err)
			}
			r.Kick(g)
			g = waitStage(t, st, store.GenDone)

			if g.TTSEngine != tc.wantEngine {
				t.Fatalf("TTSEngine = %q, want %q", g.TTSEngine, tc.wantEngine)
			}
			winner, loser := edge, eleven
			if tc.wantEngine == "elevenlabs" {
				winner, loser = eleven, edge
			}
			if got := winner.last(); got != tc.wantCredit {
				t.Errorf("credit = %q, want %q", got, tc.wantCredit)
			}
			for _, s := range loser.said() {
				if isCredit(s) {
					t.Errorf("%s voiced a credit it should not have: %q", loser.name, s)
				}
			}
			// The credit has to reach the published file, not just the
			// engine: every utterance returns "MP3!", so the audio is
			// exactly as long as the winner spoke.
			assertAudioLen(t, st, g.EpisodeSlug, 4*len(winner.said()))
		})
	}
}

// TestCreditOutroFailureIsNonFatal covers the deliberate choice to
// publish without a sign-off rather than lose an episode that is already
// synthesized and paid for.
func TestCreditOutroFailureIsNonFatal(t *testing.T) {
	st := testStore(t)
	edge := &recordingEngine{name: "edge-tts", failOn: isCredit}
	r := testRunner(st, newFakeAPI(), edge)

	g := newGeneration()
	g.Voice = "female"
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	for _, s := range edge.said() {
		if isCredit(s) {
			t.Fatalf("credit unexpectedly voiced: %q", s)
		}
	}
	// The script still published, without the sign-off appended.
	assertAudioLen(t, st, g.EpisodeSlug, 4*len(edge.said()))
}

// TestCreditSkippedForUnknownEngine guards the fallback in tts.Credit: an
// engine outside the curated table has no spoken name, and the episode
// should ship silent rather than sign off with a raw slug.
func TestCreditSkippedForUnknownEngine(t *testing.T) {
	st := testStore(t)
	unknown := &recordingEngine{name: "festival"}
	r := testRunner(st, newFakeAPI(), unknown)

	g := newGeneration()
	g.Voice = "female"
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	for _, s := range unknown.said() {
		if isCredit(s) {
			t.Errorf("unknown engine voiced a credit: %q", s)
		}
	}
}

func assertAudioLen(t *testing.T, st store.Store, slug string, want int) {
	t.Helper()
	a, err := st.OpenAudio(context.Background(), "alice", slug)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Content.Close()
	audio, err := io.ReadAll(a.Content)
	if err != nil {
		t.Fatal(err)
	}
	if len(audio) != want {
		t.Errorf("published audio = %d bytes, want %d", len(audio), want)
	}
}
