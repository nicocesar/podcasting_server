package tts

import "fmt"

// The sign-off credit names the voice and engine that actually read the
// episode, out loud, at the end of the audio. It exists because the
// engine is chosen at synthesis time by a fallback chain: on Auto an
// episode can come back in a different voice than the last one, and the
// only other trace is the TTSEngine field on the Generation. Speaking it
// makes the fallback audible to whoever is listening.
//
// It is voiced by the winning engine, as one more chunk appended after
// the script, so it is always the same voice the episode was read in.

// providerNames are the engine names as they should be *heard*. The
// internal names are slugs ("edge-tts", "elevenlabs") and a speech engine
// reads them badly, so each one gets a spoken spelling.
var providerNames = map[string]string{
	"edge-tts":   "Microsoft Edge",
	"google-tts": "Google Cloud",
	"elevenlabs": "Eleven Labs",
}

// creditFormats is the sign-off per language, with two verbs: the voice
// name and the provider name. An episode is read in one language, and a
// Spanish voice reading an English sentence sounds like a bug, so the
// credit is localized alongside the voice rather than fixed in English.
var creditFormats = map[string]string{
	"en": "This episode was read by %s, from %s.",
	"es": "Este episodio fue narrado por %s, de %s.",
}

// SpokenName is the persona name for this voice on the given engine, or
// "" if the engine is unknown to the curated table.
func (v Voice) SpokenName(engine string) string {
	switch engine {
	case "edge-tts":
		return v.EdgeName
	case "google-tts":
		return v.GoogleName
	case "elevenlabs":
		return v.ElevenName
	}
	return ""
}

// Credit returns the spoken sign-off for an episode read by engine in
// voice v. It returns "" when either the engine or the voice is missing
// a name: an episode with no credit is better than one that signs off
// with a raw slug like "en-GB-Neural2-A from edge-tts".
func Credit(engine string, v Voice) string {
	name, provider := v.SpokenName(engine), providerNames[engine]
	if name == "" || provider == "" {
		return ""
	}
	format, ok := creditFormats[v.Language]
	if !ok {
		format = creditFormats["en"]
	}
	return fmt.Sprintf(format, name, provider)
}

// ByName finds a configured engine by its Name(), for callers that learn
// which engine won only after synthesis and need it again to voice the
// credit in the same voice.
func ByName(engines []Engine, name string) Engine {
	for _, e := range engines {
		if e.Name() == name {
			return e
		}
	}
	return nil
}
