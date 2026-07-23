package audio

import (
	"bytes"
	"testing"
)

// A minimal MPEG-1 Layer III frame: sync FF FB (MPEG1, L3, no CRC), byte
// 3 = 0x90 (128 kbps, 44.1 kHz, no padding), byte 4 = 0x00 (stereo). At
// 128k/44.1k a frame is 417 bytes. For MPEG1 stereo the side info is 32
// bytes, so a Xing/Info tag sits at offset 4+32 = 36; VBRI at a fixed 36.
const frameLen = 417

func makeFrame(tagAt36 string, fill byte) []byte {
	f := make([]byte, frameLen)
	for i := range f {
		f[i] = fill
	}
	f[0], f[1], f[2], f[3] = 0xFF, 0xFB, 0x90, 0x00
	// Zero the side info so nothing there looks like a false sync.
	for i := 4; i < 36; i++ {
		f[i] = 0
	}
	if tagAt36 != "" {
		copy(f[36:40], tagAt36)
	} else {
		// Ensure the tag slot is not accidentally a metadata signature.
		f[36], f[37], f[38], f[39] = 0x11, 0x22, 0x33, 0x44
	}
	return f
}

// id3Tag is a bare ID3v2 header plus a zero payload — no 0xFF, so the
// frame walker skips all of it looking for the next sync, exactly as it
// does with a real vendor tag.
func id3Tag(payload int) []byte {
	t := append([]byte("ID3"), 0x04, 0x00, 0x00)
	t = append(t, 0x00, 0x00, 0x00, byte(payload&0x7f)) // synchsafe size
	return append(t, make([]byte, payload)...)
}

func TestStripHeadersRemovesMetadata(t *testing.T) {
	audio := makeFrame("", 0x55) // a real (non-metadata) frame
	cases := []struct {
		name string
		in   []byte
	}{
		{"info header frame", append(makeFrame("Info", 0), append(clone(audio), clone(audio)...)...)},
		{"xing header frame", append(makeFrame("Xing", 0), append(clone(audio), clone(audio)...)...)},
		{"vbri header frame", append(makeFrame("VBRI", 0), append(clone(audio), clone(audio)...)...)},
		{"leading id3 tag", append(id3Tag(24), append(clone(audio), clone(audio)...)...)},
		{"id3 then info", concat(id3Tag(24), makeFrame("Info", 0), audio, audio)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := StripHeaders(tc.in)
			if err != nil {
				t.Fatalf("StripHeaders: %v", err)
			}
			if bytes.Contains(out, []byte("ID3")) {
				t.Error("ID3 tag survived")
			}
			for _, tag := range []string{"Info", "Xing", "VBRI"} {
				if bytes.Contains(out, []byte(tag)) {
					t.Errorf("%s header survived", tag)
				}
			}
			// Both real audio frames must remain, in order, byte-for-byte.
			if want := concat(audio, audio); !bytes.Equal(out, want) {
				t.Errorf("audio not preserved: got %d bytes, want %d", len(out), len(want))
			}
		})
	}
}

// TestStripHeadersConcatenatedSegments is the real scenario: two vendor
// parts, each an ID3 tag + Info header + audio, appended as raw bytes.
// The whole thing must reduce to just the audio frames of both parts.
func TestStripHeadersConcatenatedSegments(t *testing.T) {
	a1, a2, a3 := makeFrame("", 0x11), makeFrame("", 0x22), makeFrame("", 0x33)
	seg1 := concat(id3Tag(16), makeFrame("Info", 0), a1, a2)
	seg2 := concat(id3Tag(16), makeFrame("Info", 0), a3)
	out, err := StripHeaders(concat(seg1, seg2))
	if err != nil {
		t.Fatal(err)
	}
	if want := concat(a1, a2, a3); !bytes.Equal(out, want) {
		t.Fatalf("got %d bytes, want %d (the three audio frames only)", len(out), len(want))
	}
}

// TestStripHeadersIdempotent: a bare-frame stream is already normal, so
// stripping it again changes nothing — and never drops a real frame.
func TestStripHeadersIdempotent(t *testing.T) {
	bare := concat(makeFrame("", 0x11), makeFrame("", 0x22))
	out, err := StripHeaders(bare)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, bare) {
		t.Errorf("idempotent strip altered a bare stream: %d -> %d bytes", len(bare), len(out))
	}
}

// TestStripHeadersRejectsNonMP3: the sentinel test audio and any
// non-parseable bytes must error so the caller falls back to the
// original rather than publishing nothing.
func TestStripHeadersRejectsNonMP3(t *testing.T) {
	for _, in := range [][]byte{[]byte("MP3!"), []byte("MP3!MP3!MP3!"), nil, {0x00, 0x01, 0x02}} {
		if _, err := StripHeaders(in); err == nil {
			t.Errorf("want an error for %q, got nil", in)
		}
	}
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
