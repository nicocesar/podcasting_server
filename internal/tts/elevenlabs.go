package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ElevenLabs synthesizes through the official ElevenLabs REST API. The
// best-sounding of the three engines and the only one that keeps the
// Argentinian accent for Spanish (Google falls back to es-US), but it is
// billed per character and the curated voices come from ElevenLabs'
// shared library, which their free tier refuses over the API with a 402.
// It is therefore last in the chain: opt in per Generation from the voice
// provider dropdown, and let the free engines carry the default path.
type ElevenLabs struct {
	key   string
	model string
	http  *http.Client
}

// elevenLabsModel is the multilingual model — one model voices both
// English and Spanish, so the engine never has to switch models with the
// language.
const elevenLabsModel = "eleven_multilingual_v2"

// NewElevenLabs returns an engine reading ELEVENLABS_API_KEY. An empty
// key is an error rather than a silently dead engine: it would otherwise
// appear in the provider dropdown and fail every chunk.
func NewElevenLabs(key string) (*ElevenLabs, error) {
	if key == "" {
		return nil, fmt.Errorf("tts: elevenlabs: ELEVENLABS_API_KEY not set")
	}
	return &ElevenLabs{
		key:   key,
		model: elevenLabsModel,
		// Generous: a 3000-byte chunk takes several seconds to render
		// and the whole episode is already bounded by the caller's ctx.
		http: &http.Client{Timeout: 120 * time.Second},
	}, nil
}

func (*ElevenLabs) Name() string { return "elevenlabs" }

func (e *ElevenLabs) Synthesize(ctx context.Context, text string, v Voice) ([]byte, error) {
	if v.Eleven == "" {
		return nil, fmt.Errorf("no elevenlabs voice for %s/%s", v.Language, v.Gender)
	}
	body, err := json.Marshal(map[string]string{
		"text":     text,
		"model_id": e.model,
	})
	if err != nil {
		return nil, err
	}
	// mp3_44100_128 matches what the other engines emit, so chunks from a
	// fallback-free episode concatenate into a uniform file.
	url := "https://api.elevenlabs.io/v1/text-to-speech/" + v.Eleven + "?output_format=mp3_44100_128"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", e.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Errors come back as JSON, and the message is the useful part
		// (quota exhausted, free plan, unknown voice). Capped: a stray
		// HTML error page should not land whole in the logs.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("elevenlabs: http %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return io.ReadAll(resp.Body)
}
