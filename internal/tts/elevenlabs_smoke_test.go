package tts

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestElevenLabsSmoke talks to the real ElevenLabs API and spends
// characters from the account quota, so it only runs on request:
// ELEVENLABS_SMOKE=1 go test ./internal/tts -run ElevenLabsSmoke -v
// A 402 here means the account is on the free tier, which refuses
// shared-library voices over the API — the curated voices need a paid
// plan, and until then the engine 402s every chunk and the chain falls
// back. Covers all four curated voices: the voice IDs are opaque
// strings, so a typo or a voice pulled from the library only shows up
// when it is actually requested.
func TestElevenLabsSmoke(t *testing.T) {
	key := os.Getenv("ELEVENLABS_API_KEY")
	if os.Getenv("ELEVENLABS_SMOKE") == "" || key == "" {
		t.Skip("set ELEVENLABS_SMOKE=1 and ELEVENLABS_API_KEY to hit the real API")
	}
	e, err := NewElevenLabs(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range Voices {
		t.Run(v.Language+"/"+v.Gender, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			b, err := e.Synthesize(ctx, "Hello from the podcasting server smoke test.", v)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("elevenlabs returned %d bytes for %s", len(b), v.Eleven)
			if len(b) < 1000 {
				t.Fatalf("suspiciously small audio: %d bytes", len(b))
			}
			// MP3 sanity: ID3 tag or an MPEG frame sync.
			if !bytes.HasPrefix(b, []byte("ID3")) && b[0] != 0xff {
				t.Fatalf("does not look like MP3: % x", b[:8])
			}
		})
	}
}

// TestElevenLabsDialogueSmoke renders one short two-voice take on
// DialogueModel, with stacked tags, through the same SynthesizeDialogue an
// episode uses. TestElevenLabsSmoke never reaches the dialogue model, so
// without this a model id the endpoint refuses only shows up as a failed
// story. Same opt-in: ELEVENLABS_SMOKE=1 go test ./internal/tts -run DialogueSmoke -v
func TestElevenLabsDialogueSmoke(t *testing.T) {
	key := os.Getenv("ELEVENLABS_API_KEY")
	if os.Getenv("ELEVENLABS_SMOKE") == "" || key == "" {
		t.Skip("set ELEVENLABS_SMOKE=1 and ELEVENLABS_API_KEY to hit the real API")
	}
	e, err := NewElevenLabs(key)
	if err != nil {
		t.Fatal(err)
	}
	narrator, ok := RoleVoice("narrator", "en")
	if !ok {
		t.Fatal("no English narrator")
	}
	duck, ok := RoleVoice("silly", "en")
	if !ok {
		t.Fatal("no English silly voice")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	b, err := e.SynthesizeDialogue(ctx, []DialogueInput{
		{Text: "[calm][thinking] And what did the duck say?", VoiceID: narrator.Eleven},
		{Text: "[giggles] Quack! [whispers] quack.", VoiceID: duck.Eleven},
	})
	if err != nil {
		t.Fatalf("%s: %v", DialogueModel, err)
	}
	t.Logf("%s returned %d bytes", DialogueModel, len(b))
	if len(b) < 1000 {
		t.Fatalf("suspiciously small audio: %d bytes", len(b))
	}
	if !bytes.HasPrefix(b, []byte("ID3")) && b[0] != 0xff {
		t.Fatalf("does not look like MP3: % x", b[:8])
	}
}

// TestElevenLabsNeedsKey guards the deliberate choice to fail fast on a
// missing key rather than register a dead engine in the dropdown.
func TestElevenLabsNeedsKey(t *testing.T) {
	if _, err := NewElevenLabs(""); err == nil {
		t.Fatal("expected an error for an empty key")
	}
}

// TestEveryVoiceHasElevenID catches a curated voice added to Voices
// without an ElevenLabs ID, which would fail only at synthesis time.
func TestEveryVoiceHasElevenID(t *testing.T) {
	for _, v := range Voices {
		if v.Eleven == "" {
			t.Errorf("voice %s/%s has no ElevenLabs ID", v.Language, v.Gender)
		}
	}
}

// TestElevenLabsVoicesAreInTheLibrary is the cheap half of the smoke
// test: it spends nothing, and it catches the failure that adding a
// language actually hits.
//
// A shared-library voice ID is not usable just because the shared-voices
// search returned it. Until the voice is added to the account's own
// library ("Add to my voices"), every request for it comes back 404
// voice_not_found — so a language can be fully curated here, pass every
// unit test, and then fail at synthesis for a reason nothing in the code
// can see. Story Time has no fallback to hide it: dialogue is
// ElevenLabs-only.
//
// It checks the curated dropdown voices and the whole story cast, since
// a role voice is just as opaque and just as absent:
//
//	ELEVENLABS_SMOKE=1 go test ./internal/tts -run VoicesAreInTheLibrary -v
func TestElevenLabsVoicesAreInTheLibrary(t *testing.T) {
	key := os.Getenv("ELEVENLABS_API_KEY")
	if os.Getenv("ELEVENLABS_SMOKE") == "" || key == "" {
		t.Skip("set ELEVENLABS_SMOKE=1 and ELEVENLABS_API_KEY to hit the real API")
	}
	ids := map[string]string{} // id -> who wants it
	for _, v := range Voices {
		ids[v.Eleven] = v.Language + "/" + v.Gender + " (" + v.ElevenName + ")"
	}
	for _, sv := range storyVoices {
		ids[sv.Eleven] = sv.Language + "/" + sv.Role + " (" + sv.Name + ")"
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for id, who := range ids {
		t.Run(who, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				"https://api.elevenlabs.io/v1/voices/"+id, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("xi-api-key", key)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
				t.Fatalf("voice %s for %s is not in the account library (%d: %s) — "+
					"add it from the shared library before this language ships",
					id, who, resp.StatusCode, strings.TrimSpace(string(body)))
			}
		})
	}
}
