package generation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/tts"
)

// speech is a shorthand for the segment kind under test.
func speech(role, lang, text string) Segment {
	return Segment{Kind: SegSpeech, Speaker: role, Lang: lang, Text: text}
}

// farmStory is the story the old template got wrong, written the way the
// new one should write it: English narration with the Spanish words in
// their own segments, spoken by the tutor.
func farmStory() Story {
	return Story{
		Title:    "Adventures on the Farm",
		Summary:  "A duck and a pig move into an empty barn.",
		Language: "en",
		Bed:      "soft music box, sleepy, warm",
		Segments: []Segment{
			speech("narrator", "en", "On the farm there is a big red barn. Today the barn is empty."),
			speech("tutor", "es", "Vacío."),
			speech("narrator", "en", "[gently] Empty. Vacío."),
			{Kind: SegSFX, Cue: "duck_quack"},
			speech("small_squeaky", "en", "[excited] Quack! Quack!"),
			speech("tutor", "es", "El pato."),
			{Kind: SegPause, MS: 800},
			speech("narrator", "en", "[sleepy] Goodnight, little one."),
		},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseStorySubmissionAcceptsCodeSwitching(t *testing.T) {
	// The whole point of the template: a story that is deliberately in two
	// languages must be accepted, where the old contract's single
	// episode-level language tag would have demanded it be translated into
	// one.
	st, err := ParseStorySubmission(mustJSON(t, farmStory()), 0, "en", "es")
	if err != nil {
		t.Fatalf("bilingual story rejected: %v", err)
	}
	if got := st.Languages(); len(got) != 2 || got[0] != "en" || got[1] != "es" {
		t.Errorf("Languages() = %v, want [en es]", got)
	}
}

func TestParseStorySubmissionRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Story)
		base   string
		target string
		want   string // substring of the rejection the agent reads
	}{
		{
			name:   "a third language",
			mutate: func(s *Story) { s.Segments[1].Lang = "fr" },
			base:   "en", target: "es",
			want: "may only use",
		},
		{
			name:   "a speaker outside the cast",
			mutate: func(s *Story) { s.Segments[0].Speaker = "pato" },
			base:   "en", target: "es",
			want: "not one of the available voices",
		},
		{
			name:   "the practiced language never appearing",
			mutate: func(s *Story) { s.Segments = s.Segments[:1] },
			base:   "en", target: "es",
			want: "meant to practice it",
		},
		{
			name:   "a sound effect with no cue",
			mutate: func(s *Story) { s.Segments[3].Cue = "" },
			base:   "en", target: "es",
			want: "sound effect with no cue",
		},
		{
			name:   "a pause outside the bounds",
			mutate: func(s *Story) { s.Segments[6].MS = 60000 },
			base:   "en", target: "es",
			want: "outside the allowed",
		},
		{
			name:   "an unknown segment kind",
			mutate: func(s *Story) { s.Segments[0].Kind = "jingle" },
			base:   "en", target: "es",
			want: "which is not one of",
		},
		{
			name:   "a speech segment with no language",
			mutate: func(s *Story) { s.Segments[0].Lang = "" },
			base:   "en", target: "es",
			want: "does not say what language",
		},
		{
			name:   "no segments at all",
			mutate: func(s *Story) { s.Segments = nil },
			base:   "en", target: "es",
			want: "no segments",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := farmStory()
			tc.mutate(&st)
			_, err := ParseStorySubmission(mustJSON(t, st), 0, tc.base, tc.target)
			if err == nil {
				t.Fatal("want a rejection, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("rejection = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseStorySubmissionMonolingualIsFine(t *testing.T) {
	// No target language: an ordinary one-language story must still pass,
	// and must not be asked to practice anything.
	st := farmStory()
	st.Segments = []Segment{
		speech("narrator", "en", "Once upon a time."),
		speech("child", "en", "Hello!"),
	}
	if _, err := ParseStorySubmission(mustJSON(t, st), 0, "en", ""); err != nil {
		t.Fatalf("monolingual story rejected: %v", err)
	}
}

func TestParseStorySubmissionRejectsAStub(t *testing.T) {
	// The floor exists because acceptance is unrecoverable: the story is
	// checkpointed and Retry resumes from it rather than from the agent.
	st := farmStory()
	st.Segments = []Segment{speech("narrator", "en", "The story goes here.")}
	_, err := ParseStorySubmission(mustJSON(t, st), 5, "en", "")
	if err == nil || !strings.Contains(err.Error(), "spoken words") {
		t.Fatalf("want a length rejection, got %v", err)
	}
}

func TestSpokenWordsIgnoresAudioTags(t *testing.T) {
	// Audio tags are direction the vendor performs and the listener never
	// hears, so they must not count toward the length the user asked for —
	// otherwise a heavily-directed story is silently short.
	st := Story{Segments: []Segment{
		speech("narrator", "en", "[whispers][gently] one two three"),
		{Kind: SegSFX, Cue: "duck_quack"},
	}}
	if got := st.SpokenWords(); got != 3 {
		t.Errorf("SpokenWords() = %d, want 3", got)
	}
}

func TestPlanGroupsSpeechAndBreaksAtEffects(t *testing.T) {
	pieces := Plan(farmStory(), tts.DialogueCharBudget)

	var kinds []string
	for _, p := range pieces {
		kinds = append(kinds, p.Kind)
	}
	want := []string{SegSpeech, SegSFX, SegSpeech, SegPause, SegSpeech}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("piece kinds = %v, want %v", kinds, want)
	}
	// Consecutive speech is one request, so the vendor can match prosody
	// across the speaker change — the reason for using dialogue at all.
	if n := len(pieces[0].Turns); n != 3 {
		t.Errorf("first dialogue run has %d turns, want 3", n)
	}
	// Each turn keeps its own language, which is what lets the server cast
	// the Spanish line to a Spanish voice.
	if pieces[0].Turns[1].Language != "es" {
		t.Errorf("turn 2 language = %q, want es", pieces[0].Turns[1].Language)
	}
}

func TestPlanSplitsALongRunAtASegmentBoundary(t *testing.T) {
	line := strings.Repeat("word ", 40) // ~200 chars
	st := Story{}
	for i := 0; i < 20; i++ {
		st.Segments = append(st.Segments, speech("narrator", "en", line))
	}
	pieces := Plan(st, 500)

	if len(pieces) < 2 {
		t.Fatalf("a 4000-char run produced %d pieces; it must be split", len(pieces))
	}
	for i, p := range pieces {
		if p.Kind != SegSpeech {
			t.Fatalf("piece %d is %s, want all speech", i, p.Kind)
		}
		if n := tts.DialogueChars(p.Turns); n > 500 && len(p.Turns) > 1 {
			t.Errorf("piece %d is %d chars, over the 500 budget with %d turns to split", i, n, len(p.Turns))
		}
	}
	// Nothing may be dropped on the floor by the packing.
	total := 0
	for _, p := range pieces {
		total += len(p.Turns)
	}
	if total != 20 {
		t.Errorf("packed %d turns, want all 20", total)
	}
}

func TestPlanKeepsAnOversizedSegmentWhole(t *testing.T) {
	// A single segment past the budget cannot be split without cutting a
	// sentence in half. One over-long request beats a mangled one.
	long := strings.Repeat("x", 3000)
	pieces := Plan(Story{Segments: []Segment{speech("narrator", "en", long)}}, 500)
	if len(pieces) != 1 || len(pieces[0].Turns) != 1 {
		t.Fatalf("got %d pieces, want the oversized segment kept whole", len(pieces))
	}
}

func TestSpokenScriptStripsDirection(t *testing.T) {
	// What the Strand classifier and character extraction read. A
	// classifier reading "[giggling]" is reading something no listener
	// hears.
	got := spokenScript(farmStory())
	if strings.Contains(got, "[") || strings.Contains(got, "]") {
		t.Errorf("spoken script still carries audio tags: %q", got)
	}
	if !strings.Contains(got, "Vacío") {
		t.Error("spoken script dropped the Spanish segments; they are part of the story")
	}
}

func TestStoriesV2MessageNamesBothLanguages(t *testing.T) {
	tpl, ok := TemplateByID("stories-v2")
	if !ok {
		t.Fatal("stories-v2 is not registered")
	}
	if !tpl.NeedsDialogue || !tpl.HasTargetLanguage {
		t.Error("stories-v2 must declare NeedsDialogue and HasTargetLanguage")
	}
	if !CarriesCast("stories-v2") {
		t.Error("stories-v2 episodes must be able to carry a returning cast")
	}
}
