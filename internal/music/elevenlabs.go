// Package music composes instrumental audio through the ElevenLabs Music
// API. It is the audio half of the ambient template, standing where
// internal/tts stands for the spoken programs: the runner hands it a
// prompt and a duration, it hands back MP3 bytes.
//
// One vendor, one endpoint, no fallback chain. Unlike TTS — where a dead
// engine just means the next one in the chain voices the episode — there
// is no second music provider, so a missing key takes the whole ambient
// template off the chooser rather than failing at generation time.
package music

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Duration bounds from the /v1/music contract. A single call cannot
// exceed ten minutes, which is why longer tracks are composed as a chain
// of movements rather than one request.
const (
	MinDurationMS = 3_000
	MaxDurationMS = 600_000
)

// model is the current music model. Named here rather than per-call so a
// version bump is one edit and lands in the MusicModel meter.
const model = "music_v2"

// Client composes music through the official ElevenLabs REST API.
type Client struct {
	key     string
	model   string
	baseURL string
	http    *http.Client
}

// New returns a client reading ELEVENLABS_API_KEY. An empty key is an
// error rather than a silently dead client: the caller uses that error to
// hide the ambient template entirely.
func New(key string) (*Client, error) {
	if key == "" {
		return nil, fmt.Errorf("music: elevenlabs: ELEVENLABS_API_KEY not set")
	}
	return &Client{
		key:     key,
		model:   model,
		baseURL: "https://api.elevenlabs.io",
		// Composing ten minutes of music is far slower than voicing a
		// text chunk, so this is well above the TTS client's 120s. The
		// whole run stays bounded by the caller's ctx regardless.
		http: &http.Client{Timeout: 10 * time.Minute},
	}, nil
}

// Model reports the model composing requests use, for the meter.
func (c *Client) Model() string { return c.model }

// Compose renders one movement: a text prompt and a duration in, MP3
// bytes out.
func (c *Client) Compose(ctx context.Context, prompt string, durationMS int) ([]byte, error) {
	if prompt == "" {
		return nil, fmt.Errorf("music: empty prompt")
	}
	if durationMS < MinDurationMS || durationMS > MaxDurationMS {
		return nil, fmt.Errorf("music: duration %dms outside [%d, %d]", durationMS, MinDurationMS, MaxDurationMS)
	}
	body, err := json.Marshal(map[string]any{
		"prompt":          prompt,
		"music_length_ms": durationMS,
		"model_id":        c.model,
		// The ambient program is instrumental by definition; unrequested
		// vocals would also drag a language into a track that has none.
		"force_instrumental": true,
	})
	if err != nil {
		return nil, err
	}
	// mp3_44100_128 is not a preference. Movements are concatenated as raw
	// MP3 frames, and it is what every TTS engine emits too, so every
	// audio file the station produces stays one uniform format.
	url := c.baseURL + "/v1/music?output_format=mp3_44100_128"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Errors come back as JSON and the message is the useful part
		// (quota, plan, validation). Capped so a stray HTML error page
		// does not land whole in the logs.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("music: http %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(audio) == 0 {
		return nil, fmt.Errorf("music: empty response body")
	}
	return audio, nil
}
