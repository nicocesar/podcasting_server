package tts

import (
	"context"
	"fmt"
	"strings"
)

// DialogueEngine is the richer capability the multi-voice programs need,
// kept separate from Engine on purpose. Engine's contract is "one chunk of
// text, one voice", which every engine can honour; this one renders a run
// of turns in different voices as a single continuous take, matching
// prosody across the speaker changes. Only ElevenLabs can do it, so a
// template that needs it says so and fails loudly when the key is absent
// rather than silently degrading to a worse product (the whole point of
// the exercise was that the degraded product was the bug).
type DialogueEngine interface {
	Name() string
	SynthesizeDialogue(ctx context.Context, inputs []DialogueInput) ([]byte, error)
}

// DialogueInput is one turn: what is said, and who says it.
type DialogueInput struct {
	Text    string // may carry eleven_v3 audio tags, e.g. "[whispers] goodnight"
	VoiceID string
}

// MaxDialogueVoices and MaxDialogueChars are the vendor's limits on one
// Text-to-Dialogue request. The character cap is a reliability threshold
// rather than a hard rejection — past it the model starts truncating — so
// the packer aims well under it (see DialogueCharBudget).
const (
	MaxDialogueVoices = 10
	MaxDialogueChars  = 2000
)

// DialogueCharBudget is what the packer actually targets, leaving room for
// the audio tags the agent writes inline: tags are characters the vendor
// counts but the listener never hears, so a script measured at the limit
// arrives over it.
const DialogueCharBudget = 1800

// DialogueEngineOf finds the first engine in the chain that can render
// dialogue. Returns nil when none can.
func DialogueEngineOf(engines []Engine) DialogueEngine {
	for _, e := range engines {
		if d, ok := e.(DialogueEngine); ok {
			return d
		}
	}
	return nil
}

// Role is a part in a story, not a person. The agent casts from this
// closed list and the server resolves each role to a real voice, which is
// the same discipline the Strand canon uses: a model that cannot name a
// voice cannot hallucinate one, misspell one, or quietly re-cast the duck
// halfway through a story.
type Role struct {
	ID string
	// Hint is shown to the agent alongside the enum so it casts on
	// character rather than on the identifier.
	Hint string
}

// Roles is the canon, in the order the agent sees it.
var Roles = []Role{
	{ID: "narrator", Hint: "tells the story; warm, unhurried, the voice the listener hears most"},
	{ID: "tutor", Hint: "speaks only the language being practiced, as a native speaker would"},
	{ID: "child", Hint: "a young character, bright and curious"},
	{ID: "small_squeaky", Hint: "a small creature — a duckling, a mouse, a bug"},
	{ID: "big_gruff", Hint: "a large or gruff character — a bear, a tractor, a grumpy goat"},
	{ID: "warm_grownup", Hint: "a kind adult in the story — a farmer, a parent, a shopkeeper"},
	{ID: "silly", Hint: "a comic character, for the moments meant to get a laugh"},
}

// RoleIDs returns the canon as bare strings, for a JSON-schema enum.
func RoleIDs() []string {
	out := make([]string, len(Roles))
	for i, r := range Roles {
		out[i] = r.ID
	}
	return out
}

// ValidRole reports whether id is in the canon.
func ValidRole(id string) bool {
	for _, r := range Roles {
		if r.ID == id {
			return true
		}
	}
	return false
}

// storyVoice is one curated casting: a role, in a language, to a real
// ElevenLabs voice.
type storyVoice struct {
	Role     string
	Language string
	Eleven   string
	Name     string
}

// storyVoices is the cast list. Every entry is a voice somebody listened
// to and chose; this is listening work, not programming work.
//
// Every offered language is cast for every role, which is what closes ADR
// 0032's "a duck can sound like the narrator" — the duck is small_squeaky,
// and until it was listed here it borrowed whatever roleFallback found.
// TestCuratedLanguagesCastEveryRole holds that line: adding a language to
// Voices without casting it here fails, because the dropdown is shared and
// a language offered to Story Time is a language it will try to perform.
//
// Roles may share a voice on purpose. In Spanish the gruff one and the
// squeaky one are both Agustin, and Italian and German each give the three
// young parts to one actor: eleven_v3 performs the audio tags the agent
// writes, so that is one actor playing several parts rather than a part
// being miscast.
//
// Italian and German were picked from the shared-voice library by usage,
// filtered on the metadata that matches each role — age for the young
// parts, "deep" for the gruff one, a storyteller description for the
// narrator. Every id was checked against text-to-dialogue before landing.
var storyVoices = []storyVoice{
	// English
	{Role: "narrator", Language: "en", Eleven: "ZF6FPAbjXT4488VcRRnw", Name: "Amelia"},
	{Role: "tutor", Language: "en", Eleven: "QMSGabqYzk8YAneQYYvR", Name: "Natalie"},
	{Role: "warm_grownup", Language: "en", Eleven: "G17SuINrv2H9FC6nvetn", Name: "Christopher"},
	{Role: "big_gruff", Language: "en", Eleven: "7JxUWWyYwXK8kmqmKEnT", Name: "Chuck"},
	{Role: "child", Language: "en", Eleven: "yZ3w1DL4ZAxIdOscv2t8", Name: "Wilf"},
	{Role: "small_squeaky", Language: "en", Eleven: "yZ3w1DL4ZAxIdOscv2t8", Name: "Wilf"},
	{Role: "silly", Language: "en", Eleven: "yZ3w1DL4ZAxIdOscv2t8", Name: "Wilf"},
	// Spanish
	{Role: "narrator", Language: "es", Eleven: "9rvdnhrYoXoUt4igKpBw", Name: "Mariana"},
	{Role: "tutor", Language: "es", Eleven: "bN1bDXgDIGX5lw0rtY2B", Name: "Melanie"},
	{Role: "warm_grownup", Language: "es", Eleven: "dGjL92Li0y7ZUQ3MESQW", Name: "Juan"},
	{Role: "big_gruff", Language: "es", Eleven: "D09EpJbk4um1HKSpeTSc", Name: "Agustin"},
	{Role: "child", Language: "es", Eleven: "D09EpJbk4um1HKSpeTSc", Name: "Agustin"},
	{Role: "small_squeaky", Language: "es", Eleven: "D09EpJbk4um1HKSpeTSc", Name: "Agustin"},
	{Role: "silly", Language: "es", Eleven: "D09EpJbk4um1HKSpeTSc", Name: "Agustin"},
	// Italian
	{Role: "narrator", Language: "it", Eleven: "MLpDWJvrjFIdb63xbJp8", Name: "Angelina"},
	{Role: "tutor", Language: "it", Eleven: "oVJbgLwL0s5pk9e2U6QH", Name: "Manuela"},
	{Role: "warm_grownup", Language: "it", Eleven: "uScy1bXtKz8vPzfdFsFw", Name: "Antonio"},
	{Role: "big_gruff", Language: "it", Eleven: "13Cuh3NuYvWOVQtLbRN8", Name: "Marco"},
	{Role: "child", Language: "it", Eleven: "t3hJ92dgZhDVtsff084B", Name: "Chris"},
	{Role: "small_squeaky", Language: "it", Eleven: "t3hJ92dgZhDVtsff084B", Name: "Chris"},
	{Role: "silly", Language: "it", Eleven: "t3hJ92dgZhDVtsff084B", Name: "Chris"},
	// German
	{Role: "narrator", Language: "de", Eleven: "7eVMgwCnXydb3CikjV7a", Name: "Lea"},
	{Role: "tutor", Language: "de", Eleven: "uvysWDLbKpA4XvpD3GI6", Name: "Leonie"},
	{Role: "warm_grownup", Language: "de", Eleven: "IWm8DnJ4NGjFI7QAM5lM", Name: "Stephan"},
	{Role: "big_gruff", Language: "de", Eleven: "czb8zR3V35utWZxvKd9a", Name: "Leo"},
	{Role: "child", Language: "de", Eleven: "aTTiK3YzK3dXETpuDE2h", Name: "Ben"},
	{Role: "small_squeaky", Language: "de", Eleven: "aTTiK3YzK3dXETpuDE2h", Name: "Ben"},
	{Role: "silly", Language: "de", Eleven: "aTTiK3YzK3dXETpuDE2h", Name: "Ben"},
}

// roleFallback is the gender each uncast role borrows from the curated
// Voices table, so the fallback at least varies with the part. With every
// offered language cast above, nothing reaches this in practice — it is
// the safety net that keeps an uncast role producing audio rather than an
// empty voice id, not the ordinary path.
var roleFallback = map[string]string{
	"narrator":      "female",
	"tutor":         "female",
	"child":         "female",
	"small_squeaky": "female",
	"big_gruff":     "male",
	"warm_grownup":  "male",
	"silly":         "male",
}

// RoleVoice resolves a role in a language to the voice that speaks it.
// Only the ElevenLabs fields are meaningful: dialogue is an ElevenLabs-only
// capability, and no other engine ever sees a role.
func RoleVoice(role, language string) (Voice, bool) {
	if !ValidRole(role) {
		return Voice{}, false
	}
	for _, sv := range storyVoices {
		if sv.Role == role && sv.Language == language {
			return Voice{
				Language:   language,
				Eleven:     sv.Eleven,
				ElevenName: sv.Name,
			}, true
		}
	}
	return VoiceFor(language, roleFallback[role])
}

// CastDialogue turns speaker roles into vendor voice IDs, resolving each
// turn in its own language — which is the point of the whole design: the
// narrator's English line and the tutor's Spanish one are cast separately,
// so the practiced word is spoken by somebody who actually speaks it.
//
// It also enforces the vendor's ten-voice ceiling here rather than at the
// HTTP boundary, so a caller learns its packing was too wide before it
// pays for the request.
func CastDialogue(turns []DialogueTurn) ([]DialogueInput, error) {
	inputs := make([]DialogueInput, 0, len(turns))
	seen := map[string]bool{}
	for i, t := range turns {
		v, ok := RoleVoice(t.Role, t.Language)
		if !ok {
			return nil, fmt.Errorf("turn %d: no voice for role %q in %q", i, t.Role, t.Language)
		}
		if v.Eleven == "" {
			return nil, fmt.Errorf("turn %d: role %q in %q has no ElevenLabs voice", i, t.Role, t.Language)
		}
		seen[v.Eleven] = true
		if len(seen) > MaxDialogueVoices {
			return nil, fmt.Errorf("dialogue uses more than %d distinct voices", MaxDialogueVoices)
		}
		inputs = append(inputs, DialogueInput{Text: t.Text, VoiceID: v.Eleven})
	}
	return inputs, nil
}

// DialogueTurn is one spoken turn before casting: the role and language
// the script asked for, not yet resolved to a vendor voice.
type DialogueTurn struct {
	Role     string
	Language string
	Text     string
}

// DialogueChars counts the characters a request will be billed and judged
// at, audio tags included — they are input the vendor processes even
// though nobody hears them.
func DialogueChars(turns []DialogueTurn) int {
	n := 0
	for _, t := range turns {
		n += len([]rune(t.Text))
	}
	return n
}

// audioTagRE-free by design: tags are left in the text on the way to
// ElevenLabs, which is the only consumer that understands them. StripAudioTags
// exists for everything else that has to deal with the same script —
// metering spoken length, and the episode description.
func StripAudioTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '[':
			depth++
		case r == ']' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
