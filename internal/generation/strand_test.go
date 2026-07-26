package generation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// capturingAPI records what the classifier asked for, so the tests can
// assert on the schema rather than only on the answer.
type capturingAPI struct {
	*fakeAPI
	model   string
	prompt  string
	schema  map[string]any
	calls   int
	answer  string
	failErr error
}

func newCapturingAPI(answer string) *capturingAPI {
	return &capturingAPI{fakeAPI: newFakeAPI(), answer: answer}
}

func (c *capturingAPI) CompleteJSON(_ context.Context, model, prompt string, schema map[string]any, _ int) (string, Usage, error) {
	c.calls++
	c.model, c.prompt, c.schema = model, prompt, schema
	if c.failErr != nil {
		return "", Usage{InputTokens: 20, OutputTokens: 5}, c.failErr
	}
	return `{"strand":"` + c.answer + `"}`, Usage{InputTokens: 20, OutputTokens: 5}, nil
}

func testCanon() []store.Strand {
	return []store.Strand{
		{ID: "tech-news", Title: "Tech News", Description: "What happened in technology."},
		{ID: "music", Title: "Music"},
		{ID: "stories", Title: "Stories", Description: "Told for listening."},
	}
}

func schemaEnum(t *testing.T, schema map[string]any) []string {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties: %#v", schema)
	}
	field, ok := props["strand"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no strand property: %#v", props)
	}
	enum, ok := field["enum"].([]string)
	if !ok {
		t.Fatalf("strand property has no string enum: %#v", field)
	}
	return enum
}

// TestClassifyStrandEnumIsTheCanon is the load-bearing test of ADR 0017.
// The canon travels in the schema as an enum, not in the prompt as a
// suggestion, which is what makes coining a Strand structurally
// impossible rather than merely discouraged. If this regresses, the
// closed canon quietly becomes free text.
func TestClassifyStrandEnumIsTheCanon(t *testing.T) {
	api := newCapturingAPI("music")
	got, _, err := ClassifyStrand(context.Background(), api, testCanon(), StrandRequest{Title: "Smooth Lounge Vibe"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "music" {
		t.Fatalf("ClassifyStrand = %q, want music", got)
	}
	enum := schemaEnum(t, api.schema)
	want := []string{"tech-news", "music", "stories", StrandNone}
	if strings.Join(enum, ",") != strings.Join(want, ",") {
		t.Fatalf("schema enum = %v, want %v", enum, want)
	}
	if api.model != strandModel {
		t.Errorf("classified on %q, want the small model %q", api.model, strandModel)
	}
}

// TestClassifyStrandNone: "none" is an answer, not a failure. The pile
// of Strandless Episodes is the evidence for growing the canon, so it
// must come back cleanly rather than as an error.
func TestClassifyStrandNone(t *testing.T) {
	api := newCapturingAPI(StrandNone)
	got, usage, err := ClassifyStrand(context.Background(), api, testCanon(), StrandRequest{Title: "Something odd"})
	if err != nil {
		t.Fatalf("none must not be an error: %v", err)
	}
	if got != "" {
		t.Fatalf("ClassifyStrand = %q, want empty", got)
	}
	if usage.InputTokens == 0 {
		t.Error("usage must be reported even when nothing fits — the call was paid for")
	}
}

// TestClassifyStrandRejectsOffCanon: the enum should make this
// unreachable, but a strand id outside the canon would be a dangling
// reference on a public Episode, so it is refused rather than stored.
func TestClassifyStrandRejectsOffCanon(t *testing.T) {
	api := newCapturingAPI("hardboiled")
	got, _, err := ClassifyStrand(context.Background(), api, testCanon(), StrandRequest{Title: "The Big Sleep"})
	if err == nil {
		t.Fatal("an off-canon strand must be an error")
	}
	if got != "" {
		t.Fatalf("ClassifyStrand = %q, want empty on an off-canon answer", got)
	}
	if !strings.Contains(err.Error(), "hardboiled") {
		t.Errorf("the error should name the offending id: %v", err)
	}
}

// TestClassifyStrandEmptyCanon: a fresh install has no canon until an
// admin builds one. Paying for a classification against nothing would
// be pure waste.
func TestClassifyStrandEmptyCanon(t *testing.T) {
	api := newCapturingAPI("music")
	got, usage, err := ClassifyStrand(context.Background(), api, nil, StrandRequest{Title: "Anything"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("ClassifyStrand = %q, want empty", got)
	}
	if api.calls != 0 {
		t.Errorf("called the model %d times against an empty canon, want 0", api.calls)
	}
	if usage != (Usage{}) {
		t.Errorf("usage = %+v, want zero", usage)
	}
}

// TestClassifyStrandFailureReportsUsage: a failed call still cost
// tokens, and the meters count false starts rather than hiding them.
func TestClassifyStrandFailureReportsUsage(t *testing.T) {
	api := newCapturingAPI("music")
	api.failErr = errors.New("upstream is down")
	_, usage, err := ClassifyStrand(context.Background(), api, testCanon(), StrandRequest{Title: "x"})
	if err == nil {
		t.Fatal("want an error")
	}
	if usage.InputTokens == 0 {
		t.Error("a failed classification still burned tokens; they must be reported")
	}
}

// TestStrandPromptCoversScriptlessEpisodes: an ambient episode has a
// Topic and a title and no script at all, and it is the case the
// station most obviously wants sorted ("chillout" → the music strand).
func TestStrandPromptCoversScriptlessEpisodes(t *testing.T) {
	api := newCapturingAPI("music")
	_, _, err := ClassifyStrand(context.Background(), api, testCanon(), StrandRequest{
		Title:    "Smooth Lounge Vibe",
		Topic:    "chillout",
		Template: "The Long Room",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Smooth Lounge Vibe", "chillout", "The Long Room", "tech-news", "Told for listening"} {
		if !strings.Contains(api.prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, api.prompt)
		}
	}
	if strings.Contains(api.prompt, "Script:") {
		t.Error("prompt claims to carry a script when there is none")
	}
}

// TestStrandPromptTruncatesScript: the subject is settled in the first
// few hundred words, and a sixty-minute script is thousands.
func TestStrandPromptTruncatesScript(t *testing.T) {
	api := newCapturingAPI("stories")
	long := strings.Repeat("é", strandScriptRunes*2) // multibyte: a byte-wise cut would corrupt it
	_, _, err := ClassifyStrand(context.Background(), api, testCanon(), StrandRequest{Title: "A long one", Script: long})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(api.prompt, "Script:") {
		t.Fatal("prompt should carry the script")
	}
	if got := strings.Count(api.prompt, "é"); got != strandScriptRunes {
		t.Fatalf("script contributed %d runes, want %d", got, strandScriptRunes)
	}
}

// seedCanon puts a canon in the store so the runner has something to
// sort into. Cover art is irrelevant here: dormancy gates Airing, not
// Stranding, so art arriving later must not require re-classification.
func seedCanon(t *testing.T, st store.Store, strands ...store.Strand) {
	t.Helper()
	for _, s := range strands {
		if err := st.PutStrand(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
}

func traceHas(g store.Generation, event string) bool {
	for _, e := range g.Trace {
		if e.Event == event {
			return true
		}
	}
	return false
}

// TestPipelineStrandsTheEpisode: the wiring end to end. A finished
// Generation sorts its Episode into the canon, stores the Strand on the
// Episode itself, and folds the classifier's tokens into the meters
// rather than hiding them.
func TestPipelineStrandsTheEpisode(t *testing.T) {
	st := testStore(t)
	seedCanon(t, st,
		store.Strand{ID: "tech-news", Title: "Tech News"},
		store.Strand{ID: "music", Title: "Music"},
	)
	api := newFakeAPI()
	api.completions = []string{`{"strand":"tech-news"}`}
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	ep, err := st.GetEpisode(context.Background(), "alice", g.EpisodeSlug)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Strand != "tech-news" {
		t.Fatalf("episode strand = %q, want tech-news", ep.Strand)
	}
	if ep.AirBarred {
		t.Error("a freshly published episode must not be barred from airing")
	}
	if !traceHas(g, "strand.classified") {
		t.Error("stranding left no trace entry")
	}
	// The session meters are 100/40 before stranding; the classifier's
	// 20/10 lands on top of them without touching SessionsCount, which
	// statsLabel keys off.
	if g.SessionsCount != 1 || g.InputTokens != 120 || g.OutputTokens != 50 {
		t.Errorf("meters = %d sessions, %d in, %d out; want 1, 120, 50",
			g.SessionsCount, g.InputTokens, g.OutputTokens)
	}
}

// TestStrandingFailureIsNonFatal: the Episode is the thing that took an
// agent session and a full TTS run to make. Losing it over a failed
// classification would be absurd — the Owner can pick a Strand by hand
// when they Air it.
func TestStrandingFailureIsNonFatal(t *testing.T) {
	st := testStore(t)
	seedCanon(t, st, store.Strand{ID: "tech-news", Title: "Tech News"})
	api := newFakeAPI()
	api.completeErr = errors.New("upstream is down")
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	ep, err := st.GetEpisode(context.Background(), "alice", g.EpisodeSlug)
	if err != nil {
		t.Fatalf("the episode must survive a failed stranding: %v", err)
	}
	if ep.Strand != "" {
		t.Errorf("episode strand = %q, want empty after a failed classification", ep.Strand)
	}
	if !traceHas(g, "strand.failed") {
		t.Error("a failed stranding must say so in the trace")
	}
}

// TestStrandingSkipsRetiredStrands: a Retired Strand accepts no new
// Airings, so classifying into one would put an Episode somewhere it
// can never go. With nothing else in the canon there is nothing to ask
// about, and the call must not be made at all.
func TestStrandingSkipsRetiredStrands(t *testing.T) {
	st := testStore(t)
	seedCanon(t, st, store.Strand{ID: "tech-news", Title: "Tech News", Retired: true})
	api := newFakeAPI()
	api.completions = []string{`{"strand":"tech-news"}`}
	r := testRunner(st, api, fakeEngine{name: "fake"})

	g := newGeneration()
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.Kick(g)
	g = waitStage(t, st, store.GenDone)

	ep, err := st.GetEpisode(context.Background(), "alice", g.EpisodeSlug)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Strand != "" {
		t.Fatalf("episode landed in the retired strand %q", ep.Strand)
	}
	// Nothing live to sort into: the classifier's tokens never appear.
	if g.InputTokens != 100 {
		t.Errorf("input tokens = %d, want 100 — the model should not have been asked", g.InputTokens)
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short", "hola", 10, "hola"},
		{"exact", "hola", 4, "hola"},
		{"cut", "hola", 2, "ho"},
		{"multibyte", "ééé", 2, "éé"},
		{"empty", "", 5, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateRunes(tc.in, tc.n); got != tc.want {
				t.Fatalf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}
