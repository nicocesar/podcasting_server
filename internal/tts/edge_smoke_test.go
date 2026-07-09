package tts

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

// TestEdgeSmoke talks to the real Microsoft endpoint, so it only runs on
// request: EDGE_TTS_SMOKE=1 go test ./internal/tts -run EdgeSmoke -v
// Run it when generation starts failing at voicing — if this fails, the
// edge-tts protocol has rotated and the Google fallback is carrying the
// service.
func TestEdgeSmoke(t *testing.T) {
	if os.Getenv("EDGE_TTS_SMOKE") == "" {
		t.Skip("set EDGE_TTS_SMOKE=1 to hit the real endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	v, _ := VoiceFor("en")
	b, err := NewEdge().Synthesize(ctx, "Hello from the podcasting server smoke test.", v)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("edge-tts returned %d bytes", len(b))
	if len(b) < 1000 {
		t.Fatalf("suspiciously small audio: %d bytes", len(b))
	}
	// MP3 sanity: ID3 tag or an MPEG frame sync.
	if !bytes.HasPrefix(b, []byte("ID3")) && b[0] != 0xff {
		t.Fatalf("does not look like MP3: % x", b[:8])
	}
}
