// Package mix assembles the parts of an episode into one MP3.
//
// It exists because concatenation stopped being enough. Everywhere else in
// this server, audio is joined by appending raw MP3 frames — legal only
// because every producer pins mp3_44100_128, and sufficient only as long
// as parts follow one another in time. A music bed does not: it plays
// underneath, which is overlap, and no amount of byte-appending produces
// overlap.
//
// So this package shells out to ffmpeg, and it is the only thing in the
// server that shells out to anything. The binary is a static build copied
// into the image (see Dockerfile); it needs no libc, no shell, and no
// package manager, which is what lets the distroless base stay as it is.
//
// What it deliberately does not do yet: loudness normalisation, ducking
// the bed under speech, and fading out at the end. Those were considered
// and deferred, so the levels here are whatever the vendors returned, with
// one fixed attenuation on the bed. That is a known gap, not an oversight
// — the first thing to revisit after hearing real episodes.
package mix

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/audio"
)

// BedGainDB is how far under the narration the music bed sits. A fixed
// attenuation rather than a sidechain duck: without ducking this has to be
// quiet enough to stay out of the way of the quietest line in the story,
// which is why it is well down rather than merely down.
const BedGainDB = -18

// Binary is where the static ffmpeg lands in the image. Overridable for
// local development, where ffmpeg is wherever the developer's package
// manager put it.
var Binary = envOr("FFMPEG_BINARY", "/ffmpeg")

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Available reports whether the ffmpeg binary can be found and run. Used
// to skip the mixing tests on a machine without it, and to fail a run
// early with a clear message rather than at the first exec.
func Available() bool {
	if _, err := os.Stat(Binary); err == nil {
		return true
	}
	_, err := exec.LookPath(Binary)
	return err == nil
}

// Part is one piece of the episode, in order.
type Part struct {
	// Audio is MP3 bytes. Every producer in this server pins
	// mp3_44100_128, so parts never need resampling to sit together.
	Audio []byte
	// Label is for error messages only ("dialogue 2", "sfx duck_quack"),
	// so a failure names the piece that caused it.
	Label string
}

// Mix concatenates parts in order and, when bed is non-empty, lays it
// underneath the whole result at BedGainDB, looped if it is shorter than
// the story and cut off when the story ends.
//
// Returns mp3_44100_128, the same format everything else in the server
// produces and expects.
func Mix(ctx context.Context, parts []Part, bed []byte) ([]byte, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("mix: no parts")
	}
	if !Available() {
		return nil, fmt.Errorf("mix: ffmpeg not found at %q", Binary)
	}

	dir, err := os.MkdirTemp("", "mix-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	// ffmpeg reads its inputs as files rather than from stdin: there are
	// several of them, and a bed that has to be read alongside the parts
	// rather than after them.
	inputs := make([]string, 0, len(parts)+1)
	for i, p := range parts {
		if len(p.Audio) == 0 {
			return nil, fmt.Errorf("mix: part %d (%s) is empty", i, p.Label)
		}
		path := filepath.Join(dir, fmt.Sprintf("part-%03d.mp3", i))
		if err := os.WriteFile(path, p.Audio, 0o600); err != nil {
			return nil, err
		}
		inputs = append(inputs, path)
	}
	// The bed is looped here, in Go, rather than by ffmpeg's -stream_loop.
	// That is not a stylistic preference: with -stream_loop the encoder
	// writes a correct, correctly-truncated file and then never exits,
	// because the input side goes on looping after the output is done.
	// The process hangs holding the whole run. Bounding it with -t or
	// atrim does not help — the output is right and the process still
	// hangs.
	//
	// Looping it here instead is both safe and cheap, and it is the trick
	// the rest of the server already runs on: MP3 frames concatenate as
	// raw bytes when the format matches, which it does, because every
	// producer pins mp3_44100_128. StripHeaders drops the per-copy ID3 and
	// Xing headers that would otherwise tell a decoder the bed ends after
	// the first repeat — the exact problem that function was written for.
	bedPath := ""
	if len(bed) > 0 {
		looped, err := loopBed(bed, parts)
		if err != nil {
			return nil, err
		}
		bedPath = filepath.Join(dir, "bed.mp3")
		if err := os.WriteFile(bedPath, looped, 0o600); err != nil {
			return nil, err
		}
	}
	out := filepath.Join(dir, "out.mp3")

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
	for _, in := range inputs {
		args = append(args, "-i", in)
	}
	if bedPath != "" {
		args = append(args, "-i", bedPath)
	}
	args = append(args, "-filter_complex", filterGraph(len(inputs), bedPath != ""), "-map", "[out]")
	// Pinned to match every other producer in the server.
	args = append(args, "-c:a", "libmp3lame", "-b:a", "128k", "-ar", "44100", "-ac", "2", out)

	cmd := exec.CommandContext(ctx, Binary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mix: ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	b, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("mix: ffmpeg produced no audio")
	}
	return b, nil
}

// loopBed repeats the bed until it covers the story, and one repeat past
// that so amix never runs out of music before the last line. Trimming back
// to the exact length is amix's job (duration=first).
func loopBed(bed []byte, parts []Part) ([]byte, error) {
	story, err := totalDuration(parts)
	if err != nil {
		return nil, fmt.Errorf("mix: measuring story: %w", err)
	}
	bedDur, err := audio.MP3Duration(bytes.NewReader(bed))
	if err != nil {
		return nil, fmt.Errorf("mix: measuring bed: %w", err)
	}
	if bedDur <= 0 {
		return nil, fmt.Errorf("mix: bed has no duration")
	}
	repeats := int(story/bedDur) + 2
	if repeats > maxBedRepeats {
		// A pathologically short bed under a long story would otherwise
		// build a very large buffer. Capping it means the music stops
		// early rather than the server falling over, which is the better
		// of two bad outcomes.
		repeats = maxBedRepeats
	}
	looped := bytes.Repeat(bed, repeats)
	stripped, err := audio.StripHeaders(looped)
	if err != nil {
		// Same call the publish path makes and the same policy: leaving
		// the audio un-normalized is survivable, failing the episode over
		// it is not.
		return looped, nil
	}
	return stripped, nil
}

// maxBedRepeats bounds the loop above. A three-second bed under the
// longest plausible episode is well inside this.
const maxBedRepeats = 512

// totalDuration sums the parts the way the published episode will be
// measured, reusing the same frame walker that fills in DurationSec.
func totalDuration(parts []Part) (time.Duration, error) {
	var total time.Duration
	for i, p := range parts {
		d, err := audio.MP3Duration(bytes.NewReader(p.Audio))
		if err != nil {
			return 0, fmt.Errorf("part %d (%s): %w", i, p.Label, err)
		}
		total += d
	}
	return total, nil
}

// filterGraph builds the concat of n speech/effect inputs, optionally with
// input n as a bed mixed underneath.
//
// The parts are resampled into a common format before concat because
// concat refuses inputs that disagree, and a cached sound effect rendered
// months ago is exactly the input most likely to disagree.
func filterGraph(n int, bed bool) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "[%d:a]aresample=44100,aformat=sample_fmts=fltp:channel_layouts=stereo[a%d];", i, i)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "[a%d]", i)
	}
	fmt.Fprintf(&b, "concat=n=%d:v=0:a=1[speech];", n)

	if !bed {
		// One more link so the -map target is always [out], whether or
		// not there is a bed.
		b.WriteString("[speech]anull[out]")
		return b.String()
	}
	fmt.Fprintf(&b, "[%d:a]aresample=44100,aformat=sample_fmts=fltp:channel_layouts=stereo,volume=%ddB[bed];",
		n, BedGainDB)
	// Both inputs are finite files, so this is the ordinary case ffmpeg
	// handles well. duration=first says the story decides the length: the
	// bed was looped past the end on purpose, and this is what cuts it
	// back. normalize=0 keeps amix from scaling both inputs down to make
	// room — without it, adding a bed makes the narration quieter.
	b.WriteString("[speech][bed]amix=inputs=2:duration=first:dropout_transition=0:normalize=0[out]")
	return b.String()
}
