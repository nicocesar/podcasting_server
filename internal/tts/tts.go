// Package tts turns a generated Script into MP3 audio (ADR 0009). One
// narrow Engine interface, two implementations: edge-tts (free, unofficial
// Microsoft endpoint) tried first, Google Cloud TTS (official, billed per
// character) as the fallback. An episode is always voiced end-to-end by a
// single engine so the voice never changes mid-episode.
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
}

// Voices is the curated list, in dropdown order. The first entry per
// Language is its default. Spanish speaks Argentinian on edge-tts; Google
// has no es-AR locale, so its fallback is Latin American (es-US).
var Voices = []Voice{
	{Language: "en", Label: "English", Gender: "female", Edge: "en-US-AriaNeural", Google: "en-US-Neural2-F", GoogleLang: "en-US"},
	{Language: "en", Label: "English", Gender: "male", Edge: "en-US-GuyNeural", Google: "en-US-Neural2-D", GoogleLang: "en-US"},
	{Language: "es", Label: "Español", Gender: "female", Edge: "es-AR-ElenaNeural", Google: "es-US-Neural2-A", GoogleLang: "es-US"},
	{Language: "es", Label: "Español", Gender: "male", Edge: "es-AR-TomasNeural", Google: "es-US-Neural2-B", GoogleLang: "es-US"},
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
// part). progress is called after each chunk with (done, total). It also
// reports which engine completed the episode ("" on failure) and how many
// engines were tried — attempts > 1 means a fallback fired, which the
// caller meters rather than letting it pass silently.
func SynthesizeAll(ctx context.Context, engines []Engine, chunks []string, v Voice, progress func(done, total int)) (mp3 []byte, engine string, attempts int, err error) {
	var lastErr error
	for _, e := range engines {
		attempts++
		audio, err := synthesizeWith(ctx, e, chunks, v, progress)
		if err == nil {
			return audio, e.Name(), attempts, nil
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
