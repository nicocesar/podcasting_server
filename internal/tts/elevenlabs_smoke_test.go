package tts

import (
	"bytes"
	"context"
	"os"
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
