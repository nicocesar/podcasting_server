package tts

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSplitStripsCiteTags(t *testing.T) {
	chunks := Split(`The rover landed <cite index="3-1">on Tuesday morning</cite> near the crater.`)
	if len(chunks) != 1 || chunks[0] != "The rover landed on Tuesday morning near the crater." {
		t.Fatalf("chunks = %q", chunks)
	}
}

func TestStripTagsKeepsProse(t *testing.T) {
	cases := map[string]string{
		`plain text stays`:                         "plain text stays",
		`<b>bold</b> and <cite index="0">c</cite>`: "bold and c",
		`self-closing<br/> too`:                    "self-closing too",
		`math like 5 < 10 survives`:                "math like 5 < 10 survives",
	}
	for in, want := range cases {
		if got := stripTags(in); got != want {
			t.Errorf("stripTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitShortTextIsOneChunk(t *testing.T) {
	chunks := Split("Hello there.\n\nSecond paragraph.")
	if len(chunks) != 1 || !strings.Contains(chunks[0], "Second paragraph.") {
		t.Fatalf("chunks = %q", chunks)
	}
}

func TestSplitRespectsLimit(t *testing.T) {
	para := strings.Repeat("This is a sentence about podcasts. ", 40) // ~1.4KB
	text := strings.Join([]string{para, para, para, para}, "\n\n")    // ~5.6KB
	chunks := Split(text)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > maxChunkBytes {
			t.Errorf("chunk %d is %d bytes", i, len(c))
		}
		if strings.TrimSpace(c) == "" {
			t.Errorf("chunk %d is blank", i)
		}
	}
	joined := strings.Join(chunks, " ")
	if strings.Count(joined, "sentence about podcasts") != 160 {
		t.Errorf("lost text: %d sentences", strings.Count(joined, "sentence about podcasts"))
	}
}

func TestSplitHardCutsMonsterSentence(t *testing.T) {
	chunks := Split(strings.Repeat("abcdefghij", 1000)) // 10KB, no boundaries
	if len(chunks) < 3 {
		t.Fatalf("expected hard cuts, got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > maxChunkBytes {
			t.Fatalf("chunk of %d bytes", len(c))
		}
	}
}

type stubEngine struct {
	name  string
	fails int // fail this many calls, then succeed
	calls int
}

func (s *stubEngine) Name() string { return s.name }
func (s *stubEngine) Synthesize(context.Context, string, Voice) ([]byte, error) {
	s.calls++
	if s.calls <= s.fails {
		return nil, errors.New("boom")
	}
	return []byte(s.name), nil
}

func TestSynthesizeAllFallsBackFromChunkZero(t *testing.T) {
	// Primary dies on its second chunk; the whole episode must be
	// re-voiced by the fallback so the voice never changes mid-episode.
	primary := &stubEngine{name: "edge", fails: 2}
	fallback := &stubEngine{name: "google"}
	var last int
	var failed string
	out, engine, attempts, err := SynthesizeAll(context.Background(), []Engine{primary, fallback},
		[]string{"one", "two", "three"}, Voice{}, func(done, total int) { last = done },
		func(engine string, err error) { failed = engine + ": " + err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	// The rescued failure must still be reported, not swallowed.
	if !strings.Contains(failed, "edge") || !strings.Contains(failed, "boom") {
		t.Fatalf("onFail reported %q", failed)
	}
	if string(out) != "googlegooglegoogle" {
		t.Fatalf("out = %q", out)
	}
	if fallback.calls != 3 || last != 3 {
		t.Fatalf("fallback calls = %d, last progress = %d", fallback.calls, last)
	}
	if engine != "google" || attempts != 2 {
		t.Fatalf("engine = %q, attempts = %d (want google, 2)", engine, attempts)
	}
}

func TestSynthesizeAllAllEnginesFail(t *testing.T) {
	_, engine, attempts, err := SynthesizeAll(context.Background(),
		[]Engine{&stubEngine{name: "edge", fails: 99}}, []string{"one"}, Voice{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "edge") {
		t.Fatalf("err = %v", err)
	}
	if engine != "" || attempts != 1 {
		t.Fatalf("engine = %q, attempts = %d (want \"\", 1)", engine, attempts)
	}
}

func TestPrefer(t *testing.T) {
	edge := &stubEngine{name: "edge"}
	google := &stubEngine{name: "google"}
	chain := []Engine{edge, google}

	names := func(es []Engine) string {
		out := []string{}
		for _, e := range es {
			out = append(out, e.Name())
		}
		return strings.Join(out, ",")
	}

	cases := []struct {
		name string
		want string
	}{
		{"", "edge,google"},           // auto: chain untouched
		{"edge", "edge,google"},       // already first
		{"google", "google,edge"},     // moved to front, rest as fallback
		{"elevenlabs", "edge,google"}, // unknown engine: preference, not a demand
	}
	for _, c := range cases {
		if got := names(Prefer(chain, c.name)); got != c.want {
			t.Errorf("Prefer(%q) = %s, want %s", c.name, got, c.want)
		}
	}
	// The shared chain must never be reordered in place.
	if names(chain) != "edge,google" {
		t.Fatalf("input mutated: %s", names(chain))
	}
}

func TestVoiceFor(t *testing.T) {
	if v, ok := VoiceFor("en", "male"); !ok || v.Edge != "en-GB-RyanNeural" {
		t.Fatalf("en/male = %+v, %v", v, ok)
	}
	// Empty gender (records that predate the voice picker) gets the
	// language's default.
	if v, ok := VoiceFor("es", ""); !ok || v.Gender != "female" {
		t.Fatalf("es default = %+v, %v", v, ok)
	}
	if _, ok := VoiceFor("en", "robot"); ok {
		t.Fatal("made up a gender")
	}
	if _, ok := VoiceFor("xx", "female"); ok {
		t.Fatal("made up a voice")
	}
}
