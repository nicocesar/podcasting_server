// Package tts turns a generated Script into MP3 audio (ADR 0009). One
// narrow Engine interface, three implementations tried in chain order:
// edge-tts (free, unofficial Microsoft endpoint) first, Google Cloud TTS
// (official, billed per character) as the fallback, and ElevenLabs
// (official, billed, best quality) last — opt-in per Generation rather
// than a default, since it is the only engine that costs real money on
// the happy path. An episode is always voiced end-to-end by a single
// engine so the voice never changes mid-episode.
package tts

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Engine synthesizes one chunk of plain text into MP3 bytes.
type Engine interface {
	Name() string
	Synthesize(ctx context.Context, text string, v Voice) ([]byte, error)
}

// Voice is one curated voice per Language and Gender, resolved to each
// engine's own voice ID. The Language and Voice dropdowns on /me/generate
// map here.
type Voice struct {
	Language   string // BCP-47 primary tag, e.g. "en"
	Label      string // what the Language dropdown shows
	Gender     string // "female" or "male"
	Edge       string // edge-tts voice short name
	Google     string // Google Cloud TTS voice name
	GoogleLang string // Google language code, e.g. "en-US"
	Eleven     string // ElevenLabs voice ID

	// Spoken names for the sign-off credit, one per engine because the
	// same curated slot is a different persona on each: the "English
	// female" voice is Sonia on edge-tts and Amelia on ElevenLabs. These
	// are read aloud, so they are spelled for a speech engine rather than
	// copied from the voice ID — Google names its neural voices
	// "en-GB-Neural2-A", which is written out as "Neural Two A".
	EdgeName   string
	GoogleName string
	ElevenName string
}

// Voices is the curated list, in dropdown order. The first entry per
// Language is its default. The accents are a deliberate bit of
// personality: English speaks British, Spanish speaks Argentinian.
// Google has no es-AR locale, so its Spanish fallback is Latin American
// (es-US) — an accent shift when the edge-tts → Google fallback fires.
// ElevenLabs holds both accents, so it is the only engine that never
// shifts. Its IDs are shared-library voices, named in the comments
// because the ID alone says nothing about who you are hearing.
var Voices = []Voice{
	// Amelia - Enthusiastic and Expressive (British)
	{Language: "en", Label: "English", Gender: "female", Edge: "en-GB-SoniaNeural", Google: "en-GB-Neural2-A", GoogleLang: "en-GB", Eleven: "ZF6FPAbjXT4488VcRRnw",
		EdgeName: "Sonia", GoogleName: "Neural Two A", ElevenName: "Amelia"},
	// Christopher - Gentle and Trustworthy (British)
	{Language: "en", Label: "English", Gender: "male", Edge: "en-GB-RyanNeural", Google: "en-GB-Neural2-B", GoogleLang: "en-GB", Eleven: "G17SuINrv2H9FC6nvetn",
		EdgeName: "Ryan", GoogleName: "Neural Two B", ElevenName: "Christopher"},
	// Mariana - Intimate and Assertive (Argentinian)
	{Language: "es", Label: "Español", Gender: "female", Edge: "es-AR-ElenaNeural", Google: "es-US-Neural2-A", GoogleLang: "es-US", Eleven: "9rvdnhrYoXoUt4igKpBw",
		EdgeName: "Elena", GoogleName: "Neural Dos A", ElevenName: "Mariana"},
	// Juan - Rich, Soothing and Bassy (Argentinian)
	{Language: "es", Label: "Español", Gender: "male", Edge: "es-AR-TomasNeural", Google: "es-US-Neural2-B", GoogleLang: "es-US", Eleven: "dGjL92Li0y7ZUQ3MESQW",
		EdgeName: "Tomás", GoogleName: "Neural Dos B", ElevenName: "Juan"},
}

// Languages returns one Voice per Language, in dropdown order, for the
// Language dropdown.
func Languages() []Voice {
	seen := map[string]bool{}
	out := []Voice{}
	for _, v := range Voices {
		if !seen[v.Language] {
			seen[v.Language] = true
			out = append(out, v)
		}
	}
	return out
}

// VoiceFor resolves a Language and Gender to a curated Voice. An empty
// gender (Generations that predate the voice picker) gets the Language's
// default.
func VoiceFor(language, gender string) (Voice, bool) {
	var def *Voice
	for i := range Voices {
		v := &Voices[i]
		if v.Language != language {
			continue
		}
		if v.Gender == gender {
			return *v, true
		}
		if def == nil {
			def = v
		}
	}
	if gender == "" && def != nil {
		return *def, true
	}
	return Voice{}, false
}

// maxChunkBytes keeps each synthesis request under Google's 5000-byte
// input limit with headroom; edge-tts has no such limit but chunking also
// drives the progress checkpoint, so both engines get the same pieces.
const maxChunkBytes = 3000

// tagRE matches any XML/HTML-like tag — the research agent cites its web
// sources as <cite index="...">…</cite>, which belongs in the stored
// Script but must never be spoken. Requiring a letter after < (or </)
// leaves literal comparisons like "5 < 10" alone.
var tagRE = regexp.MustCompile(`</?[a-zA-Z][^<>]*>`)

// stripTags removes markup wrappers, keeping the prose inside them.
func stripTags(text string) string {
	return tagRE.ReplaceAllString(text, "")
}

// Split cuts the script into synthesis chunks, preferring paragraph
// boundaries, then sentence boundaries, then a hard cut. No chunk exceeds
// maxChunkBytes. Markup is stripped first: chunks hold only speakable
// text, so byte limits and character metering see what is actually voiced.
func Split(text string) []string {
	text = stripTags(text)
	chunks := []string{}
	current := ""
	flush := func() {
		if s := strings.TrimSpace(current); s != "" {
			chunks = append(chunks, s)
		}
		current = ""
	}
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if len(current)+2+len(para) <= maxChunkBytes {
			if current != "" {
				current += "\n\n"
			}
			current += para
			continue
		}
		flush()
		if len(para) <= maxChunkBytes {
			current = para
			continue
		}
		for _, piece := range splitSentences(para) {
			if len(current)+1+len(piece) > maxChunkBytes {
				flush()
			}
			if current != "" {
				current += " "
			}
			current += piece
		}
		flush()
	}
	flush()
	return chunks
}

// splitSentences breaks an oversized paragraph on sentence ends, hard-
// cutting any single sentence that still exceeds the limit.
func splitSentences(s string) []string {
	pieces := []string{}
	rest := s
	for rest != "" {
		cut := len(rest)
		if cut > maxChunkBytes {
			cut = maxChunkBytes
			if i := strings.LastIndexAny(rest[:cut], ".!?"); i > 0 {
				cut = i + 1
			}
		}
		pieces = append(pieces, strings.TrimSpace(rest[:cut]))
		rest = strings.TrimSpace(rest[cut:])
	}
	return pieces
}

// Prefer returns engines with the named engine first and the rest in
// their original order as fallback. Empty or unknown name returns engines
// unchanged: the provider choice is a preference, not a demand, so a
// Generation naming an engine that didn't initialize still voices with
// the default chain. Always returns a fresh slice — the input is shared
// across concurrent generations and must never be reordered in place.
func Prefer(engines []Engine, name string) []Engine {
	if name == "" {
		return engines
	}
	out := make([]Engine, 0, len(engines))
	for _, e := range engines {
		if e.Name() == name {
			out = append([]Engine{e}, out...)
		} else {
			out = append(out, e)
		}
	}
	return out
}

// SynthesizeAll voices every chunk with one engine, falling through to
// the next engine from chunk zero on any failure (voice consistency over
// partial progress — chunks are cheap, the Script was the expensive
// part). progress is called after each chunk with (done, total). onFail
// is called with each engine that fails, even when a later engine
// rescues the episode — otherwise the only trace of a fallback is the
// attempts meter, with the actual error discarded. It also reports which
// engine completed the episode ("" on failure) and how many engines were
// tried — attempts > 1 means a fallback fired, which the caller meters
// rather than letting it pass silently.
func SynthesizeAll(ctx context.Context, engines []Engine, chunks []string, v Voice, progress func(done, total int), onFail func(engine string, err error)) (mp3 []byte, engine string, attempts int, err error) {
	var lastErr error
	for _, e := range engines {
		attempts++
		audio, err := synthesizeWith(ctx, e, chunks, v, progress)
		if err == nil {
			return audio, e.Name(), attempts, nil
		}
		if onFail != nil {
			onFail(e.Name(), err)
		}
		lastErr = fmt.Errorf("%s: %w", e.Name(), err)
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no TTS engine configured")
	}
	return nil, "", attempts, lastErr
}

func synthesizeWith(ctx context.Context, e Engine, chunks []string, v Voice, progress func(done, total int)) ([]byte, error) {
	var out []byte
	for i, chunk := range chunks {
		b, err := e.Synthesize(ctx, chunk, v)
		if err != nil {
			return nil, fmt.Errorf("chunk %d/%d: %w", i+1, len(chunks), err)
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("chunk %d/%d: engine returned no audio", i+1, len(chunks))
		}
		out = append(out, b...)
		if progress != nil {
			progress(i+1, len(chunks))
		}
	}
	return out, nil
}
