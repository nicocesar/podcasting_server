package tts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Edge synthesizes through Microsoft Edge's "Read Aloud" service. Free
// and good-quality, but unofficial: Microsoft rotates the protocol
// occasionally, which is why it is only ever the first engine in the
// chain, never the only one.
//
// The client is our own port of rany2/edge-tts 7.3.1 (the reference
// implementation every other library follows) rather than a Go wrapper:
// difyz9/edge-tts-go stopped at v1 tokens when Microsoft began rejecting
// them, and its errors dropped the HTTP status, so a rejected handshake
// surfaced only as "websocket: bad handshake". When the protocol rotates
// again, diff upstream's constants.py, drm.py and communicate.py against
// this file and edge_token.go.
type Edge struct{}

func NewEdge() Edge { return Edge{} }

func (Edge) Name() string { return "edge-tts" }

const (
	edgeChromiumFull  = "143.0.3650.75"
	edgeChromiumMajor = "143"
	edgeGECVersion    = "2-" + edgeChromiumFull

	edgeConnectTimeout = 10 * time.Second
	edgeReceiveTimeout = 60 * time.Second
)

// edgeWSSURL is a var only so tests can point it at a fake server.
var edgeWSSURL = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1?TrustedClientToken=" + edgeClientToken

var edgeHeaders = http.Header{
	"Pragma":          {"no-cache"},
	"Cache-Control":   {"no-cache"},
	"Origin":          {"chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"},
	"User-Agent":      {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + edgeChromiumMajor + ".0.0.0 Safari/537.36 Edg/" + edgeChromiumMajor + ".0.0.0"},
	"Accept-Encoding": {"gzip, deflate, br, zstd"},
	"Accept-Language": {"en-US,en;q=0.9"},
}

// edgeClockSkew corrects the token's clock (seconds) when Microsoft's
// Date header disagrees with ours; the token window is only five minutes.
var edgeClockSkew atomic.Int64

func (Edge) Synthesize(ctx context.Context, text string, v Voice) ([]byte, error) {
	ssml := edgeSSML(edgeVoiceName(v.Edge), text)
	conn, err := edgeDial(ctx)
	var hs *handshakeError
	if errors.As(err, &hs) && hs.status == http.StatusForbidden && hs.adjustSkew() {
		// A 403 may only mean our clock put us in the wrong token window.
		conn, err = edgeDial(ctx)
	}
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// Unblock reads promptly if the run is cancelled mid-stream.
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()

	now := edgeDate()
	config := "X-Timestamp:" + now + "\r\n" +
		"Content-Type:application/json; charset=utf-8\r\n" +
		"Path:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"true","wordBoundaryEnabled":"false"},"outputFormat":"audio-24khz-48kbitrate-mono-mp3"}}}}` + "\r\n"
	request := "X-RequestId:" + edgeID() + "\r\n" +
		"Content-Type:application/ssml+xml\r\n" +
		"X-Timestamp:" + now + "Z\r\n" + // the stray Z is Edge's own bug, mirrored
		"Path:ssml\r\n\r\n" + ssml
	for _, msg := range []string{config, request} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			return nil, fmt.Errorf("send: %w", err)
		}
	}

	var out []byte
	for {
		conn.SetReadDeadline(time.Now().Add(edgeReceiveTimeout))
		kind, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("receive: %w", err)
		}
		switch kind {
		case websocket.TextMessage:
			head, _, _ := bytes.Cut(data, []byte("\r\n\r\n"))
			switch path := edgeHeader(head, "Path"); path {
			case "turn.end":
				if len(out) == 0 {
					return nil, fmt.Errorf("no audio received")
				}
				return out, nil
			case "response", "turn.start", "audio.metadata":
			default:
				return nil, fmt.Errorf("unknown message path %q", path)
			}
		case websocket.BinaryMessage:
			// Two-byte big-endian header length, headers, then audio.
			if len(data) < 2 {
				return nil, fmt.Errorf("binary message too short")
			}
			n := int(binary.BigEndian.Uint16(data))
			if 2+n > len(data) {
				return nil, fmt.Errorf("binary header length %d exceeds message", n)
			}
			head, audio := data[2:2+n], data[2+n:]
			if p := edgeHeader(head, "Path"); p != "audio" {
				return nil, fmt.Errorf("binary message with path %q", p)
			}
			// The stream ends with an empty audio frame that has no
			// Content-Type; anything else must be MPEG with data.
			switch ct := edgeHeader(head, "Content-Type"); {
			case ct == "" && len(audio) == 0:
			case ct == "audio/mpeg" && len(audio) > 0:
				out = append(out, audio...)
			default:
				return nil, fmt.Errorf("unexpected audio frame: Content-Type %q, %d bytes", ct, len(audio))
			}
		}
	}
}

// handshakeError keeps what gorilla's "bad handshake" throws away: the
// HTTP status and the start of the body Microsoft answered with.
type handshakeError struct {
	status int
	body   string
	date   string
	err    error
}

func (e *handshakeError) Error() string {
	msg := fmt.Sprintf("handshake rejected: HTTP %d", e.status)
	if e.body != "" {
		msg += ": " + e.body
	}
	return msg
}

func (e *handshakeError) Unwrap() error { return e.err }

// adjustSkew moves our token clock to the server's Date header and
// reports whether it could.
func (e *handshakeError) adjustSkew() bool {
	server, err := http.ParseTime(e.date)
	if err != nil {
		return false
	}
	edgeClockSkew.Add(server.Unix() - edgeNow())
	return true
}

func edgeNow() int64 { return time.Now().Unix() + edgeClockSkew.Load() }

func edgeDial(ctx context.Context) (*websocket.Conn, error) {
	url := edgeWSSURL +
		"&ConnectionId=" + edgeID() +
		"&Sec-MS-GEC=" + edgeToken(edgeNow()) +
		"&Sec-MS-GEC-Version=" + edgeGECVersion
	header := edgeHeaders.Clone()
	header.Set("Cookie", "muid="+edgeID()+";")
	dialer := websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  edgeConnectTimeout,
		EnableCompression: true,
	}
	conn, resp, err := dialer.DialContext(ctx, url, header)
	if err == nil {
		return conn, nil
	}
	if resp == nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return nil, &handshakeError{
		status: resp.StatusCode,
		body:   strings.TrimSpace(string(body)),
		date:   resp.Header.Get("Date"),
		err:    err,
	}
}

func edgeHeader(head []byte, name string) string {
	for line := range bytes.SplitSeq(head, []byte("\r\n")) {
		k, v, ok := bytes.Cut(line, []byte(":"))
		if ok && string(bytes.TrimSpace(k)) == name {
			return string(bytes.TrimSpace(v))
		}
	}
	return ""
}

// edgeID is 32 uppercase hex characters, the shape of both the request
// ids and the MUID cookie.
func edgeID() string {
	var b [16]byte
	rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// edgeDate is the JavaScript Date.toString() form Edge sends.
func edgeDate() string {
	return time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")
}

var shortVoiceRE = regexp.MustCompile(`^([a-z]{2,})-([A-Z]{2,})-(.+Neural)$`)

// edgeVoiceName expands "en-GB-SoniaNeural" to the long form Edge itself
// sends; names with a sub-region ("zh-CN-liaoning-XiaobeiNeural") move
// it into the locale.
func edgeVoiceName(short string) string {
	m := shortVoiceRE.FindStringSubmatch(short)
	if m == nil {
		return short
	}
	lang, region, name := m[1], m[2], m[3]
	if sub, rest, ok := strings.Cut(name, "-"); ok {
		region += "-" + sub
		name = rest
	}
	return fmt.Sprintf("Microsoft Server Speech Text to Speech Voice (%s-%s, %s)", lang, region, name)
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func edgeSSML(voice, text string) string {
	// Control characters other than tab and newlines break the service.
	text = strings.Map(func(r rune) rune {
		if r < 32 && r != '\t' && r != '\n' && r != '\r' {
			return ' '
		}
		return r
	}, text)
	return "<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='en-US'>" +
		"<voice name='" + voice + "'>" +
		"<prosody pitch='+0Hz' rate='+0%' volume='+0%'>" +
		xmlEscaper.Replace(text) +
		"</prosody></voice></speak>"
}
