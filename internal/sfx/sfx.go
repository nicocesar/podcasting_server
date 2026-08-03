// Package sfx renders the sound effects a story asks for, and remembers
// them. It stands beside internal/tts and internal/music as the third
// audio source: the runner hands it a cue, it hands back MP3 bytes.
//
// The remembering is the point. A generated effect costs money and comes
// back slightly different every time it is generated, so a cue rendered
// once and stored is both cheaper and better: the duck in this week's
// story is the identical duck the listener met last week, which is what
// makes a returning cast feel like a returning cast rather than a
// coincidence of names.
//
// Two kinds of cue reach here. A Library cue is a name from a list
// somebody curated and listened to, and it is the same bytes for every
// listener forever. A freeform cue is whatever the story needed that the
// library did not have; it is keyed by the hash of its own text, so the
// second story to ask for "a rusty gate, slow" gets the first story's gate.
package sfx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/elevenlabs"
	"github.com/nicocesar/podcasting_server/internal/store"
)

// Duration bounds from the /v1/sound-generation contract.
const (
	MinDurationMS = 500
	MaxDurationMS = 30_000
)

// DefaultDurationMS is what a cue gets when the script does not say. Short
// on purpose: an effect in a story is punctuation, and the vendor's own
// estimate tends to run long enough to sit on the next line.
const DefaultDurationMS = 2000

// model is the sound-effects model. Named here so a version bump is one
// edit — and note that changing it changes what every cache key means, so
// bump cacheVersion with it.
const model = "eleven_text_to_sound_v2"

// cacheVersion is baked into every cache key. Rendering the same cue with
// a different model gives a different sound, so a model change has to miss
// the cache rather than silently serve the old vendor's take alongside the
// new one in the same episode.
const cacheVersion = "v1"

// Cue is one effect a story asked for.
type Cue struct {
	// Name is a library cue when it matches one, and is otherwise
	// treated as freeform text describing the sound.
	Name       string
	DurationMS int
}

// libraryCue is a curated effect: a stable name, and the prompt that
// renders it.
type libraryCue struct {
	Name       string
	Prompt     string
	DurationMS int
}

// Library is the curated list. The agent is shown these names and prefers
// them, because a named cue is one somebody has heard: it is the right
// length, it is not startling, and it will not surprise anybody at
// bedtime. Growing this list is listening work.
var Library = []libraryCue{
	{Name: "duck_quack", Prompt: "a single friendly duck quacking, close, dry, no echo", DurationMS: 1500},
	{Name: "pig_oink", Prompt: "a small pig oinking softly, close, warm", DurationMS: 1500},
	{Name: "cow_moo", Prompt: "a gentle cow mooing in a distant field", DurationMS: 2500},
	{Name: "sheep_baa", Prompt: "a soft sheep bleating, close", DurationMS: 1500},
	{Name: "barn_door", Prompt: "a wooden barn door creaking open slowly", DurationMS: 2500},
	{Name: "footsteps_soft", Prompt: "soft slow footsteps on straw and dry earth", DurationMS: 2500},
	{Name: "waddle", Prompt: "small webbed feet padding on wooden boards", DurationMS: 2000},
	{Name: "splash_small", Prompt: "a small gentle splash in a shallow puddle", DurationMS: 1500},
	{Name: "mud_squish", Prompt: "a soft wet squish in thick mud", DurationMS: 1500},
	{Name: "hay_rustle", Prompt: "dry hay rustling as something settles into it", DurationMS: 2500},
	{Name: "wind_soft", Prompt: "soft gentle breeze through long grass, calm", DurationMS: 4000},
	{Name: "birds_morning", Prompt: "distant morning birdsong, sparse and calm", DurationMS: 4000},
	{Name: "crickets_night", Prompt: "quiet crickets on a warm still night", DurationMS: 4000},
	{Name: "rain_gentle", Prompt: "light rain on a window, steady and soft", DurationMS: 4000},
	{Name: "door_knock", Prompt: "three soft knocks on a wooden door", DurationMS: 2000},
	{Name: "bell_small", Prompt: "one small bright hand bell, single ring", DurationMS: 2000},
	{Name: "page_turn", Prompt: "a page of a book turning slowly", DurationMS: 1500},
	{Name: "yawn_sleepy", Prompt: "a small sleepy yawn, gentle", DurationMS: 1500},
}

// LibraryNames returns the curated cue names, for the tool schema the
// agent casts from.
func LibraryNames() []string {
	out := make([]string, len(Library))
	for i, c := range Library {
		out[i] = c.Name
	}
	return out
}

func lookup(name string) (libraryCue, bool) {
	for _, c := range Library {
		if c.Name == name {
			return c, true
		}
	}
	return libraryCue{}, false
}

// Client renders cues and caches them in the store.
type Client struct {
	key     string
	baseURL string
	store   store.Store
	http    *http.Client
}

// New returns a client reading ELEVENLABS_API_KEY. An empty key is an
// error rather than a silently dead client, matching internal/music: the
// caller uses it to decide whether the template can run at all.
func New(key string, st store.Store) (*Client, error) {
	if key == "" {
		return nil, fmt.Errorf("sfx: elevenlabs: ELEVENLABS_API_KEY not set")
	}
	return &Client{
		key:     key,
		baseURL: "https://api.elevenlabs.io",
		store:   st,
		// A few seconds of effect renders fast; this is well under the
		// music client's ten minutes and above a plain HTTP default.
		http: &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// Result is one rendered cue and where it came from, so the caller can
// meter a paid render separately from a free one.
type Result struct {
	Audio    []byte
	CacheHit bool
}

// Render returns the audio for a cue, generating and storing it only on a
// miss. A store that fails to read or write is never fatal: the cache is
// an optimisation, and a story should not fail to publish because the
// bucket was briefly unhappy.
func (c *Client) Render(ctx context.Context, cue Cue) (Result, error) {
	prompt, dur := c.resolve(cue)
	key := CacheKey(cue.Name, prompt, dur)

	if c.store != nil {
		if r, _, err := c.store.OpenAsset(ctx, key); err == nil {
			b, err := io.ReadAll(r)
			r.Close()
			if err == nil && len(b) > 0 {
				return Result{Audio: b, CacheHit: true}, nil
			}
		}
	}

	b, err := c.generate(ctx, prompt, dur)
	if err != nil {
		return Result{}, err
	}
	if c.store != nil {
		// Best effort: a cue we could not store is a cue we pay for again
		// next time, which is a cost problem, not a correctness one.
		_ = c.store.PutAsset(ctx, key, "audio/mpeg", bytes.NewReader(b))
	}
	return Result{Audio: b}, nil
}

// resolve turns a cue into the prompt and duration to render it at,
// preferring the curated entry when the name matches one.
func (c *Client) resolve(cue Cue) (string, int) {
	dur := cue.DurationMS
	if lib, ok := lookup(cue.Name); ok {
		if dur <= 0 {
			dur = lib.DurationMS
		}
		return lib.Prompt, clampDuration(dur)
	}
	if dur <= 0 {
		dur = DefaultDurationMS
	}
	return strings.TrimSpace(cue.Name), clampDuration(dur)
}

func clampDuration(ms int) int {
	if ms < MinDurationMS {
		return MinDurationMS
	}
	if ms > MaxDurationMS {
		return MaxDurationMS
	}
	return ms
}

// CacheKey addresses a rendered cue by everything that decides how it
// sounds. A library cue keeps its name in the path so the stored tree is
// legible to a human poking at the bucket; a freeform cue is addressed by
// the hash of its prompt, since its text is arbitrary and may be long,
// oddly punctuated, or in another language.
func CacheKey(name, prompt string, durationMS int) string {
	if _, ok := lookup(name); ok {
		return fmt.Sprintf("sfx/%s/lib/%s-%dms.mp3", cacheVersion, name, durationMS)
	}
	sum := sha256.Sum256([]byte(normalize(prompt)))
	return fmt.Sprintf("sfx/%s/gen/%s-%dms.mp3", cacheVersion, hex.EncodeToString(sum[:8]), durationMS)
}

// normalize folds the differences that do not change the sound — casing
// and stray whitespace — so "A Rusty Gate" and "a rusty  gate" are one
// cached effect rather than two.
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func (c *Client) generate(ctx context.Context, prompt string, durationMS int) ([]byte, error) {
	if prompt == "" {
		return nil, fmt.Errorf("sfx: empty cue")
	}
	body, err := json.Marshal(map[string]any{
		"text":             prompt,
		"duration_seconds": float64(durationMS) / 1000,
		"model_id":         model,
	})
	if err != nil {
		return nil, err
	}
	// Same mp3_44100_128 pin as speech and music, so an effect drops into
	// the same mix as everything else without a resample.
	url := c.baseURL + "/v1/sound-generation?output_format=mp3_44100_128"
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
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, elevenlabs.HTTPError("sound-generation", resp.StatusCode, msg)
	}
	return io.ReadAll(resp.Body)
}
