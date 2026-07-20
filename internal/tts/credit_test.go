package tts

import "testing"

func TestCredit(t *testing.T) {
	en, _ := VoiceFor("en", "female")
	es, _ := VoiceFor("es", "male")
	tests := []struct {
		name   string
		engine string
		voice  Voice
		want   string
	}{
		{"edge english", "edge-tts", en, "This episode was read by Sonia, from Microsoft Edge."},
		{"elevenlabs english", "elevenlabs", en, "This episode was read by Amelia, from Eleven Labs."},
		{"google english", "google-tts", en, "This episode was read by Neural Two A, from Google Cloud."},
		{"spanish is localized", "elevenlabs", es, "Este episodio fue narrado por Juan, de Eleven Labs."},
		{"spanish on edge", "edge-tts", es, "Este episodio fue narrado por Tomás, de Microsoft Edge."},
		// Rather than sign off with a slug the listener can't parse.
		{"unknown engine credits nothing", "festival", en, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Credit(tc.engine, tc.voice); got != tc.want {
				t.Errorf("Credit(%q) = %q, want %q", tc.engine, got, tc.want)
			}
		})
	}
}

// TestEveryVoiceCreditsOnEveryEngine is the real guard: a voice added to
// Voices without a spoken name for some engine would publish episodes
// that silently lose their sign-off, but only on the engine that happens
// to win the fallback chain that day.
func TestEveryVoiceCreditsOnEveryEngine(t *testing.T) {
	for _, v := range Voices {
		for engine := range providerNames {
			if Credit(engine, v) == "" {
				t.Errorf("no credit for %s/%s on %s", v.Language, v.Gender, engine)
			}
		}
	}
}

// TestCreditFormatPerLanguage catches a language added to Voices without
// a localized sign-off, which would otherwise ship a Spanish voice
// reading an English sentence.
func TestCreditFormatPerLanguage(t *testing.T) {
	for _, v := range Languages() {
		if _, ok := creditFormats[v.Language]; !ok {
			t.Errorf("language %q has no credit format", v.Language)
		}
	}
}

func TestByName(t *testing.T) {
	engines := []Engine{NewEdge()}
	if got := ByName(engines, "edge-tts"); got == nil {
		t.Error("ByName did not find edge-tts")
	}
	if got := ByName(engines, "elevenlabs"); got != nil {
		t.Errorf("ByName found %v for an absent engine", got)
	}
}
