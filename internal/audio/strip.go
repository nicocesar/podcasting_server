package audio

import (
	"bytes"
	"errors"
	"io"

	"github.com/tcolgate/mp3"
)

// StripHeaders rewrites an MP3 into a bare stream of audio frames: every
// ID3 tag and every Xing/Info/VBRI header frame removed, the real frames
// kept in order and byte-for-byte.
//
// It exists because the pipeline builds an episode by concatenating the
// MP3s of its parts — voiced chunks, the spoken credit, music movements
// (internal/tts, internal/generation) — as raw bytes. That is only safe
// when the parts carry no framing metadata. edge-tts output is bare
// frames, so it always worked; ElevenLabs (speech *and* music) prefixes
// each part with an ID3 tag and an Info/Xing header that declares that
// part's own duration. Appended together, the file still announces the
// length of its first part, and players that trust the header — most do —
// stop there, never reaching the credit (or, for a multi-part episode,
// anything past the first part). The bytes are all present; they are just
// beyond the advertised end.
//
// Normalizing the whole assembled blob to bare frames — the shape
// edge-tts already produced — makes every player play to the true end.
// Input with no parseable frames is returned unchanged with an error, so
// a caller can safely fall back to the original bytes (tests voice with
// sentinel non-MP3 audio, and a real stream should never publish empty).
func StripHeaders(data []byte) (out []byte, err error) {
	// The frame walker sees vendor bytes; a panic on a malformed header
	// must become an error, not a crash — same contract as MP3Duration.
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, errors.New("mp3 frame walk panicked")
		}
	}()

	d := mp3.NewDecoder(bytes.NewReader(data))
	var (
		frame  mp3.Frame
		skip   int
		frames int
	)
	out = make([]byte, 0, len(data))
	for {
		if derr := d.Decode(&frame, &skip); derr != nil {
			// EOF, or a garbage tail after real frames: keep what parsed.
			// Zero frames means this was never MP3 to begin with.
			if frames == 0 {
				return nil, errors.New("no MP3 frames found")
			}
			return out, nil
		}
		buf, rerr := io.ReadAll(frame.Reader())
		if rerr != nil {
			return nil, rerr
		}
		frames++
		if isMetadataFrame(&frame, buf) {
			// A silent Xing/Info/VBRI header frame, one per concatenated
			// part. Dropping it is what removes the false duration; it
			// carries no audio and no bit-reservoir dependency.
			continue
		}
		out = append(out, buf...)
	}
}

// isMetadataFrame reports whether buf is a Xing/Info/VBRI header frame
// rather than audio. The tag sits at a fixed offset — after the header,
// optional CRC, and side information for Xing/Info; at a constant 36 for
// VBRI — so the check is exact rather than a scan, which keeps it from
// ever mistaking real audio that happens to contain those four bytes.
func isMetadataFrame(f *mp3.Frame, buf []byte) bool {
	if len(buf) >= 40 && string(buf[36:40]) == "VBRI" {
		return true
	}
	sideLen, err := f.SideInfoLength()
	if err != nil {
		return false
	}
	off := 4 // frame header
	if f.Header().Protection() {
		off += 2 // 16-bit CRC follows the header when present
	}
	off += sideLen
	if len(buf) < off+4 {
		return false
	}
	tag := string(buf[off : off+4])
	return tag == "Xing" || tag == "Info"
}
