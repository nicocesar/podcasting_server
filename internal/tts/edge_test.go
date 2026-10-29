package tts

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

// fakeEdge points the client at an httptest server for the duration of
// the test.
func fakeEdge(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := edgeWSSURL
	edgeWSSURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/edge/v1?TrustedClientToken=x"
	t.Cleanup(func() { edgeWSSURL = old; edgeClockSkew.Store(0) })
}

func audioFrame(contentType string, audio []byte) []byte {
	head := "X-RequestId:abc\r\nPath:audio"
	if contentType != "" {
		head += "\r\nContent-Type:" + contentType
	}
	head += "\r\n"
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(head)))
	return append(append(frame, head...), audio...)
}

// The production failure that prompted the port: a rejected handshake
// must say which status Microsoft answered, not just "bad handshake".
// A 403 is retried once after a clock-skew correction; that is all.
func TestEdgeHandshakeErrorKeepsStatus(t *testing.T) {
	var attempts atomic.Int32
	fakeEdge(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "token rejected", http.StatusForbidden)
	})
	_, err := NewEdge().Synthesize(context.Background(), "hi", Voice{Edge: "en-GB-SoniaNeural"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403: token rejected") {
		t.Fatalf("err = %v, want HTTP 403 with body", err)
	}
	if n := attempts.Load(); n != 2 {
		t.Errorf("attempts = %d, want 2 (one skew-corrected retry)", n)
	}
}

func TestEdgeHandshakeOtherStatusNotRetried(t *testing.T) {
	var attempts atomic.Int32
	fakeEdge(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := NewEdge().Synthesize(context.Background(), "hi", Voice{Edge: "en-GB-SoniaNeural"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("err = %v, want HTTP 429", err)
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

// What the real service does: config + SSML in, then turn.start, audio
// frames, an empty trailing frame without Content-Type, and turn.end.
func TestEdgeStreamsAudioUntilTurnEnd(t *testing.T) {
	var gotURL, gotSSML, gotCookie string
	fakeEdge(t, func(w http.ResponseWriter, r *http.Request) {
		gotURL, gotCookie = r.URL.RawQuery, r.Header.Get("Cookie")
		conn, err := edgeUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for range 2 {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if strings.Contains(string(msg), "Path:ssml") {
				gotSSML = string(msg)
			}
		}
		conn.WriteMessage(websocket.TextMessage, []byte("Path:turn.start\r\n\r\n{}"))
		conn.WriteMessage(websocket.BinaryMessage, audioFrame("audio/mpeg", []byte{0xff, 0xf3, 1}))
		conn.WriteMessage(websocket.BinaryMessage, audioFrame("audio/mpeg", []byte{2, 3}))
		conn.WriteMessage(websocket.BinaryMessage, audioFrame("", nil))
		conn.WriteMessage(websocket.TextMessage, []byte("Path:turn.end\r\n\r\n{}"))
	})
	audio, err := NewEdge().Synthesize(context.Background(), "Fish & <chips>", Voice{Edge: "en-GB-SoniaNeural"})
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "\xff\xf3\x01\x02\x03" {
		t.Errorf("audio = % x", audio)
	}
	if !strings.Contains(gotURL, "Sec-MS-GEC-Version=2-") {
		t.Errorf("query %q lacks a v2 token version", gotURL)
	}
	if !strings.HasPrefix(gotCookie, "muid=") {
		t.Errorf("cookie = %q, want muid", gotCookie)
	}
	for _, want := range []string{
		"Microsoft Server Speech Text to Speech Voice (en-GB, SoniaNeural)",
		"Fish &amp; &lt;chips&gt;",
	} {
		if !strings.Contains(gotSSML, want) {
			t.Errorf("SSML lacks %q:\n%s", want, gotSSML)
		}
	}
}

func TestEdgeNoAudioIsAnError(t *testing.T) {
	fakeEdge(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := edgeUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.ReadMessage()
		conn.ReadMessage()
		conn.WriteMessage(websocket.TextMessage, []byte("Path:turn.end\r\n\r\n{}"))
	})
	if _, err := NewEdge().Synthesize(context.Background(), "hi", Voice{Edge: "en-GB-SoniaNeural"}); err == nil {
		t.Fatal("want error when no audio arrives")
	}
}

func TestEdgeVoiceName(t *testing.T) {
	for short, want := range map[string]string{
		"en-GB-SoniaNeural":            "Microsoft Server Speech Text to Speech Voice (en-GB, SoniaNeural)",
		"fil-PH-AngeloNeural":          "Microsoft Server Speech Text to Speech Voice (fil-PH, AngeloNeural)",
		"zh-CN-liaoning-XiaobeiNeural": "Microsoft Server Speech Text to Speech Voice (zh-CN-liaoning, XiaobeiNeural)",
		"Microsoft Server Speech Text to Speech Voice (en-US, AvaNeural)": "Microsoft Server Speech Text to Speech Voice (en-US, AvaNeural)",
	} {
		if got := edgeVoiceName(short); got != want {
			t.Errorf("edgeVoiceName(%q) = %q, want %q", short, got, want)
		}
	}
}

func TestEdgeSSMLReplacesControlCharacters(t *testing.T) {
	got := edgeSSML("v", "a\x00b\tc\nd\x1fe")
	if !strings.Contains(got, ">a b\tc\nd e<") {
		t.Errorf("SSML = %q", got)
	}
}

// The client sends Edge's chrome-extension Origin, which gorilla's
// default same-origin check would reject.
var edgeUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
