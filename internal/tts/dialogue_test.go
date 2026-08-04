package tts

import (
	"strings"
	"testing"
)

func TestRoleVoiceResolvesPerLanguage(t *testing.T) {
	// The casting rule that fixes the original complaint: the same role in
	// two languages must resolve to two different voices, so a Spanish
	// line is not read by an English speaker.
	en, ok := RoleVoice("tutor", "en")
	if !ok {
		t.Fatal("no tutor voice for en")
	}
	es, ok := RoleVoice("tutor", "es")
	if !ok {
		t.Fatal("no tutor voice for es")
	}
	if en.Eleven == "" || es.Eleven == "" {
		t.Fatal("a role must resolve to a real ElevenLabs voice")
	}
	if en.Eleven == es.Eleven {
		t.Errorf("tutor resolves to the same voice (%s) in English and Spanish; "+
			"the practiced language would be read in the wrong accent", en.Eleven)
	}
}

func TestRoleVoiceCoversEveryRole(t *testing.T) {
	// Uncast roles fall back rather than failing, so every role in the
	// canon must produce a usable voice in every offered language. A gap
	// here reaches the vendor as an empty voice ID.
	for _, r := range Roles {
		for _, l := range Languages() {
			t.Run(r.ID+"/"+l.Language, func(t *testing.T) {
				v, ok := RoleVoice(r.ID, l.Language)
				if !ok {
					t.Fatalf("role %q has no voice in %q", r.ID, l.Language)
				}
				if v.Eleven == "" {
					t.Errorf("role %q in %q resolves to an empty ElevenLabs voice", r.ID, l.Language)
				}
			})
		}
	}
}

// The two languages the station actually tells stories in are cast by
// hand, every role, with no role left borrowing from roleFallback.
//
// TestRoleVoiceCoversEveryRole passes on the fallback alone, so it went on
// passing through the whole period when the duck sounded like the narrator.
// This is the test that notices: deleting a row from storyVoices is
// otherwise silent, because the fallback absorbs it and still returns a
// perfectly usable voice — just the wrong one.
func TestCuratedLanguagesCastEveryRole(t *testing.T) {
	for _, lang := range []string{"en", "es"} {
		for _, r := range Roles {
			t.Run(r.ID+"/"+lang, func(t *testing.T) {
				var found *storyVoice
				for i, sv := range storyVoices {
					if sv.Role == r.ID && sv.Language == lang {
						found = &storyVoices[i]
						break
					}
				}
				if found == nil {
					t.Fatalf("role %q has no curated %s voice, so it falls back to %q — "+
						"somebody has to listen and choose one", r.ID, lang, roleFallback[r.ID])
				}
				// A hand-copied ID that got truncated reaches the vendor
				// and fails the episode, and dialogue failures are not
				// skippable the way a sound effect is.
				if len(found.Eleven) < 15 || strings.ContainsAny(found.Eleven, " \t\n") {
					t.Errorf("role %q in %s has a malformed voice id %q", r.ID, lang, found.Eleven)
				}
				if strings.TrimSpace(found.Name) == "" {
					t.Errorf("role %q in %s has no voice name; the credit needs one to speak", r.ID, lang)
				}
			})
		}
	}
}

func TestNarratorVoiceCanSpeakTheCredit(t *testing.T) {
	// The performed pipeline voices the credit with the narrator's role
	// voice. Credit returns "" when the Voice has no spoken name for the
	// engine, and the caller treats that as "no credit" — so a role voice
	// missing ElevenName would drop attribution silently, on every episode.
	for _, l := range Languages() {
		t.Run(l.Language, func(t *testing.T) {
			v, ok := RoleVoice("narrator", l.Language)
			if !ok {
				t.Fatalf("no narrator voice for %q", l.Language)
			}
			if got := Credit("elevenlabs", v, "alice", "radio.example.com"); got == "" {
				t.Errorf("narrator voice in %q produces an empty credit", l.Language)
			}
		})
	}
}

func TestRoleVoiceRejectsAnInventedRole(t *testing.T) {
	// The enum is the guard: a model that cannot name a voice cannot
	// hallucinate one.
	if _, ok := RoleVoice("pato", "en"); ok {
		t.Error("an invented role resolved to a voice")
	}
}

func TestCastDialogueCastsEachTurnInItsOwnLanguage(t *testing.T) {
	turns := []DialogueTurn{
		{Role: "narrator", Language: "en", Text: "The barn is empty."},
		{Role: "tutor", Language: "es", Text: "Vacío."},
		{Role: "narrator", Language: "en", Text: "Empty."},
	}
	inputs, err := CastDialogue(turns)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 3 {
		t.Fatalf("got %d inputs, want 3", len(inputs))
	}
	if inputs[0].VoiceID == inputs[1].VoiceID {
		t.Error("the English and Spanish turns were cast to the same voice")
	}
	if inputs[0].VoiceID != inputs[2].VoiceID {
		t.Error("the narrator changed voice mid-story")
	}
}

func TestCastDialogueRejectsAnInventedRole(t *testing.T) {
	_, err := CastDialogue([]DialogueTurn{{Role: "duck", Language: "en", Text: "quack"}})
	if err == nil {
		t.Fatal("want an error for an unknown role")
	}
	if !strings.Contains(err.Error(), "duck") {
		t.Errorf("error should name the bad role, got %q", err)
	}
}

func TestCastDialogueEnforcesTheVoiceCeiling(t *testing.T) {
	// The vendor caps a request at ten distinct voices. Catching it here
	// means a caller learns its packing was too wide before paying for
	// the request.
	var turns []DialogueTurn
	for i := 0; i < MaxDialogueVoices+2; i++ {
		turns = append(turns, DialogueTurn{Role: "narrator", Language: "en", Text: "hello"})
	}
	// All one role, so all one voice: well inside the ceiling.
	if _, err := CastDialogue(turns); err != nil {
		t.Fatalf("many turns in one voice should be fine: %v", err)
	}
}

func TestStripAudioTags(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"a leading tag", "[whispers] goodnight", "goodnight"},
		{"several tags", "[excited][loudly] Quack! Quack!", "Quack! Quack!"},
		{"a tag in the middle", "the barn [gently] is empty", "the barn is empty"},
		{"no tags", "plain words", "plain words"},
		{"only a tag", "[sighs]", ""},
		{"an unclosed bracket", "[whispers goodnight", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripAudioTags(tc.in); got != tc.want {
				t.Errorf("StripAudioTags(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDialogueEngineOfFindsOnlyACapableEngine(t *testing.T) {
	// Edge is a plain Engine; it must not be mistaken for one that can
	// render dialogue, or a performed story would silently come back as a
	// single-voice reading.
	if d := DialogueEngineOf([]Engine{NewEdge()}); d != nil {
		t.Error("edge-tts was offered as a dialogue engine")
	}
	if d := DialogueEngineOf(nil); d != nil {
		t.Error("an empty chain produced a dialogue engine")
	}
	eleven, err := NewElevenLabs("test-key")
	if err != nil {
		t.Fatal(err)
	}
	if d := DialogueEngineOf([]Engine{NewEdge(), eleven}); d == nil {
		t.Error("ElevenLabs was not found in the chain")
	}
}
