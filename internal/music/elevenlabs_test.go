package music

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNewNeedsKey(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("want an error for an empty key, got nil")
	}
	if _, err := New("k"); err != nil {
		t.Fatalf("New with a key: %v", err)
	}
}

// testClient points a real client at ts. The baseURL field is unexported,
// which is why this test lives in the package.
func testClient(ts *httptest.Server) *Client {
	c, _ := New("test-key")
	c.baseURL = ts.URL
	return c
}

// TestComposeRequest pins the wire contract. The output format especially:
// movements are concatenated as raw MP3 frames and played back-to-back
// with TTS audio elsewhere in the station, so a format drift here is a
// corrupt file rather than a loud failure.
func TestComposeRequest(t *testing.T) {
	var gotPath, gotQuery, gotKey, gotType string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotKey = r.Header.Get("xi-api-key")
		gotType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Write([]byte("ID3reallymusic"))
	}))
	defer ts.Close()

	audio, err := testClient(ts).Compose(context.Background(), "warm rhodes, 60bpm", 300_000)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if string(audio) != "ID3reallymusic" {
		t.Errorf("audio = %q", audio)
	}
	if gotPath != "/v1/music" {
		t.Errorf("path = %q, want /v1/music", gotPath)
	}
	if gotQuery != "output_format=mp3_44100_128" {
		t.Errorf("query = %q, want output_format=mp3_44100_128", gotQuery)
	}
	if gotKey != "test-key" {
		t.Errorf("xi-api-key = %q", gotKey)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q", gotType)
	}
	if gotBody["prompt"] != "warm rhodes, 60bpm" {
		t.Errorf("prompt = %v", gotBody["prompt"])
	}
	if gotBody["music_length_ms"] != float64(300_000) {
		t.Errorf("music_length_ms = %v", gotBody["music_length_ms"])
	}
	if gotBody["model_id"] != model {
		t.Errorf("model_id = %v, want %v", gotBody["model_id"], model)
	}
	// The ambient program is instrumental by definition; a vocal sneaking
	// into a sleep track is the failure this pins.
	if gotBody["force_instrumental"] != true {
		t.Errorf("force_instrumental = %v, want true", gotBody["force_instrumental"])
	}
}

// TestComposeRejectsBadDuration keeps an out-of-range movement from
// costing a round trip: the vendor would reject it anyway.
func TestComposeRejectsBadDuration(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer ts.Close()
	c := testClient(ts)

	for _, ms := range []int{0, MinDurationMS - 1, MaxDurationMS + 1} {
		if _, err := c.Compose(context.Background(), "x", ms); err == nil {
			t.Errorf("duration %d: want an error, got nil", ms)
		}
	}
	if _, err := c.Compose(context.Background(), "", MinDurationMS); err == nil {
		t.Error("empty prompt: want an error, got nil")
	}
	if called {
		t.Error("client made a request despite invalid input")
	}
}

func TestComposeSurfacesErrorBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"detail":"quota exhausted"}`))
	}))
	defer ts.Close()

	_, err := testClient(ts).Compose(context.Background(), "x", MinDurationMS)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "402") || !strings.Contains(err.Error(), "quota exhausted") {
		t.Errorf("error should carry the status and the body: %v", err)
	}
}

// TestComposeErrorBodyCapped keeps a stray HTML error page out of the
// logs whole.
func TestComposeErrorBodyCapped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(strings.Repeat("x", 4000)))
	}))
	defer ts.Close()

	_, err := testClient(ts).Compose(context.Background(), "x", MinDurationMS)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if len(err.Error()) > 700 {
		t.Errorf("error body not capped: %d chars", len(err.Error()))
	}
}

// TestComposeRejectsEmptyBody: a 200 with no bytes would otherwise append
// nothing and silently shorten the piece.
func TestComposeRejectsEmptyBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()

	if _, err := testClient(ts).Compose(context.Background(), "x", MinDurationMS); err == nil {
		t.Fatal("want an error for an empty body, got nil")
	}
}

// TestComposeSmoke hits the real API. Skipped unless ELEVENLABS_SMOKE=1
// and a key are set, since it composes real audio and costs real money.
func TestComposeSmoke(t *testing.T) {
	key := os.Getenv("ELEVENLABS_API_KEY")
	if os.Getenv("ELEVENLABS_SMOKE") != "1" || key == "" {
		t.Skip("set ELEVENLABS_SMOKE=1 and ELEVENLABS_API_KEY to run")
	}
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	audio, err := c.Compose(context.Background(),
		"Soft ambient piano, slow 60bpm, warm tape saturation, no percussion.", 10_000)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(audio) < 1000 {
		t.Fatalf("suspiciously small audio: %d bytes", len(audio))
	}
	// Same shape check the TTS smoke tests use: an ID3 tag or a frame sync.
	if !(strings.HasPrefix(string(audio), "ID3") || audio[0] == 0xff) {
		t.Errorf("not MP3: first bytes %x", audio[:4])
	}
	t.Logf("composed %d bytes", len(audio))
}
