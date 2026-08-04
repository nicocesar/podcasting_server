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
// It mixes in two phases, because it cannot place anything until it knows
// how loud everything is. First it measures — the narration, each effect,
// the bed — and then it builds one filter graph carrying the gains that
// measurement implies. Every level is relative to the narration in that
// same episode; levels.go holds the rules and the reasoning.
//
// What it still does not do: duck the bed under speech. A sidechain has
// four interacting parameters whose failure mode is audible pumping, and
// with the bed now sitting a measured distance under the narration rather
// than a hopeful one, a static offset is the honest version of the same
// idea. That is a decision, not a gap.
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

// Kind says how a part should be treated when levels are set.
type Kind int

const (
	// Speech is left exactly as the vendor returned it, and sets the
	// reference every other level is placed against. It is the zero value
	// so that a caller who forgets to say gets the untouched behaviour
	// rather than a processed one.
	//
	// Speech is never leveled part-by-part on purpose: eleven_v3 audio
	// tags are performance direction, and a story where "[whispers]
	// goodnight" is normalised to the same loudness as a shout is a story
	// whose direction has been thrown away.
	Speech Kind = iota
	// Effect is a sound effect, placed under the narration.
	Effect
	// Silence is a pause. It carries no audio — the graph generates it —
	// which is why MS rather than Audio says how long it is.
	Silence
)

// Part is one piece of the episode, in order.
type Part struct {
	// Audio is MP3 bytes. Every producer in this server pins
	// mp3_44100_128, so parts never need resampling to sit together.
	// Empty for Silence.
	Audio []byte
	// Label is for error messages only ("dialogue 2", "sfx duck_quack"),
	// so a failure names the piece that caused it.
	Label string
	// Kind decides whether this part's level is touched.
	Kind Kind
	// MS is how long a Silence lasts. Ignored for every other kind.
	MS int
}

// Result is a finished episode and the levels that produced it.
type Result struct {
	Audio []byte
	// Levels is what was measured and what was applied, for the trace.
	// The constants in this package were chosen from first principles
	// rather than by ear, so the numbers an episode actually used are the
	// evidence for changing them.
	Levels Levels
}

// Mix concatenates parts in order and, when bed is non-empty, lays it
// underneath at a measured distance below the narration, looped to cover
// the story and faded in and out around it.
//
// Returns mp3_44100_128, the same format everything else in the server
// produces and expects.
func Mix(ctx context.Context, parts []Part, bed []byte) (Result, error) {
	parts = renderable(parts)
	if len(parts) == 0 {
		return Result{}, fmt.Errorf("mix: no parts")
	}
	if !Available() {
		return Result{}, fmt.Errorf("mix: ffmpeg not found at %q", Binary)
	}

	dir, err := os.MkdirTemp("", "mix-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	// ffmpeg reads its inputs as files rather than from stdin: there are
	// several of them, and a bed that has to be read alongside the parts
	// rather than after them.
	//
	// Silence has no file. It is generated in the graph by anullsrc, which
	// means input indices stop matching part indices, so each part records
	// the input it became — or -1 for the ones that became a filter.
	input := make([]int, len(parts))
	var inputs []string
	for i, p := range parts {
		if p.Kind == Silence {
			input[i] = -1
			continue
		}
		if len(p.Audio) == 0 {
			return Result{}, fmt.Errorf("mix: part %d (%s) is empty", i, p.Label)
		}
		path := filepath.Join(dir, fmt.Sprintf("part-%03d.mp3", i))
		if err := os.WriteFile(path, p.Audio, 0o600); err != nil {
			return Result{}, err
		}
		input[i] = len(inputs)
		inputs = append(inputs, path)
	}

	story, err := totalDuration(parts)
	if err != nil {
		return Result{}, fmt.Errorf("mix: measuring story: %w", err)
	}

	levels, gains := measure(ctx, parts, input, inputs, bed, dir)

	// The tail and the fades only make sense over a bed, and only when the
	// measurement they were computed against held up.
	tail := time.Duration(0)
	if len(bed) > 0 && levels.Measured {
		tail = BedTail
	}

	bedPath := ""
	if len(bed) > 0 {
		looped, err := loopBed(bed, story+tail)
		if err != nil {
			return Result{}, err
		}
		bedPath = filepath.Join(dir, "bed.mp3")
		if err := os.WriteFile(bedPath, looped, 0o600); err != nil {
			return Result{}, err
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
	graph := filterGraph(parts, input, len(inputs), bedPath != "", story, tail, levels, gains)
	args = append(args, "-filter_complex", graph, "-map", "[out]")
	// Pinned to match every other producer in the server.
	args = append(args, "-c:a", "libmp3lame", "-b:a", "128k", "-ar", "44100", "-ac", "2", out)

	if _, err := run(ctx, args...); err != nil {
		return Result{}, fmt.Errorf("mix: %w", err)
	}

	b, err := os.ReadFile(out)
	if err != nil {
		return Result{}, err
	}
	if len(b) == 0 {
		return Result{}, fmt.Errorf("mix: ffmpeg produced no audio")
	}
	return Result{Audio: b, Levels: levels}, nil
}

// renderable drops the parts that would contribute nothing. A pause of no
// length is not an error worth failing an episode over, but it must not
// reach the graph either: anullsrc with a zero duration produces an empty
// stream, and concat refuses one.
func renderable(parts []Part) []Part {
	out := make([]Part, 0, len(parts))
	for _, p := range parts {
		if p.Kind == Silence && p.MS <= 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// measure listens to everything before anything is placed, and returns
// both the record for the trace and the per-part gains the graph needs.
//
// Nothing here is fatal. A measurement that fails leaves the mixer doing
// what it did before it could measure: no gains, a flat bed, no fades. The
// alternative is failing an episode that is already written and paid for
// over a meter, which is the same trade the rest of this pipeline already
// refuses to make for a missing bed or an effect that would not render.
func measure(ctx context.Context, parts []Part, input []int, inputs []string, bed []byte, dir string) (Levels, []float64) {
	gains := make([]float64, len(parts))

	speech, err := measureSpeech(ctx, parts, input, inputs)
	if err != nil {
		return unmeasured("could not measure the narration: " + err.Error()), gains
	}
	if !speech.usable() {
		return unmeasured(fmt.Sprintf("narration measured %.1f LUFS, below the %.0f floor; levels left alone",
			speech.LUFS, SilenceFloorLUFS)), gains
	}

	levels := Levels{
		Measured:   true,
		SpeechLUFS: speech.LUFS,
		SpeechPeak: speech.Peak,
		SpeechMean: speech.Mean,
		SpeechMax:  speech.Max,
	}

	for i, p := range parts {
		if p.Kind != Effect {
			continue
		}
		m, err := measureFile(ctx, inputs[input[i]], "volumedetect")
		if err != nil {
			// One effect that will not measure is left at the level it
			// arrived at, which is the old behaviour for every effect.
			continue
		}
		gains[i] = effectGain(speech, m)
		levels.EffectGains = append(levels.EffectGains, gains[i])
	}

	levels.BedGain = BedGainDB
	if len(bed) > 0 {
		// The bed is measured before it is looped: a loop of the same
		// music measures the same, and this way the meter reads a minute
		// instead of however many minutes the story turned out to be.
		path := filepath.Join(dir, "bed-measure.mp3")
		if err := os.WriteFile(path, bed, 0o600); err == nil {
			if m, err := measureFile(ctx, path, "loudnorm=print_format=json"); err == nil {
				levels.BedLUFS = m.LUFS
				levels.BedGain = bedGain(speech, m)
			}
		}
	}

	levels.FinalGain, levels.FinalClamp = finalGain(speech)
	return levels, gains
}

// measureSpeech meters the narration as one continuous thing rather than
// part by part. Concatenating first is what makes the number comparable to
// the bed's: loudness is gated, so measuring runs separately and averaging
// them would weight a two-word line the same as a two-minute scene.
func measureSpeech(ctx context.Context, parts []Part, input []int, inputs []string) (measurement, error) {
	var idx []int
	for i, p := range parts {
		if p.Kind == Speech {
			idx = append(idx, input[i])
		}
	}
	if len(idx) == 0 {
		return measurement{}, fmt.Errorf("the story has no speech to measure against")
	}

	var b strings.Builder
	for _, i := range idx {
		fmt.Fprintf(&b, "[%d:a]%s[m%d];", i, commonFormat, i)
	}
	for _, i := range idx {
		fmt.Fprintf(&b, "[m%d]", i)
	}
	// volumedetect passes its audio through untouched, so both meters read
	// the same signal in one run. Speech is measured both ways because the
	// bed is compared to it in LUFS and the effects in plain dB.
	fmt.Fprintf(&b, "concat=n=%d:v=0:a=1,volumedetect,loudnorm=print_format=json", len(idx))

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "info"}
	for _, in := range inputs {
		args = append(args, "-i", in)
	}
	args = append(args, "-filter_complex", b.String(), "-f", "null", "-")

	stderr, err := run(ctx, args...)
	if err != nil {
		return measurement{}, err
	}
	loud, err := parseLoudness(stderr)
	if err != nil {
		return measurement{}, err
	}
	vol, err := parseVolume(stderr)
	if err != nil {
		return measurement{}, err
	}
	return loud.merge(vol), nil
}

// measureFile meters one file with one analysis filter.
func measureFile(ctx context.Context, path, filter string) (measurement, error) {
	stderr, err := run(ctx, "-nostdin", "-hide_banner", "-loglevel", "info",
		"-i", path, "-af", filter, "-f", "null", "-")
	if err != nil {
		return measurement{}, err
	}
	if strings.HasPrefix(filter, "loudnorm") {
		return parseLoudness(stderr)
	}
	return parseVolume(stderr)
}

// run executes ffmpeg and returns its stderr, which is where it puts both
// its errors and everything the analysis filters report.
func run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, Binary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stderr.String(), fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(lastLines(stderr.String(), 5)))
	}
	return stderr.String(), nil
}

// lastLines trims a log down to something that fits in an error message.
// The analysis filters are chatty and the useful part is always at the end.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// loopBed repeats the bed until it covers cover, and one repeat past that
// so amix never runs out of music before the last line. Trimming back to
// the exact length is amix's job (duration=first).
//
// cover includes the tail: the bed now plays on after the last word, and a
// bed looped only to the length of the speech would fall silent exactly
// where the ending is supposed to be.
func loopBed(bed []byte, cover time.Duration) ([]byte, error) {
	bedDur, err := audio.MP3Duration(bytes.NewReader(bed))
	if err != nil {
		return nil, fmt.Errorf("mix: measuring bed: %w", err)
	}
	if bedDur <= 0 {
		return nil, fmt.Errorf("mix: bed has no duration")
	}
	repeats := int(cover/bedDur) + 2
	if repeats > maxBedRepeats {
		// A pathologically short bed under a long story would otherwise
		// build a very large buffer. Capping it means the music stops
		// early rather than the server falling over, which is the better
		// of two bad outcomes.
		repeats = maxBedRepeats
	}
	looped := bytes.Repeat(bed, repeats)
	// StripHeaders drops the per-copy ID3 and Xing headers that would
	// otherwise tell a decoder the bed ends after the first repeat — the
	// exact problem that function was written for.
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
// Silence is counted from its length rather than its bytes, having none.
func totalDuration(parts []Part) (time.Duration, error) {
	var total time.Duration
	for i, p := range parts {
		if p.Kind == Silence {
			total += time.Duration(p.MS) * time.Millisecond
			continue
		}
		d, err := audio.MP3Duration(bytes.NewReader(p.Audio))
		if err != nil {
			return 0, fmt.Errorf("part %d (%s): %w", i, p.Label, err)
		}
		total += d
	}
	return total, nil
}

// commonFormat is the shape every stream is put into before it meets
// another one. The parts are resampled because concat refuses inputs that
// disagree, and a cached sound effect rendered months ago is exactly the
// input most likely to disagree.
const commonFormat = "aresample=44100,aformat=sample_fmts=fltp:channel_layouts=stereo"

// filterGraph builds the whole mix: each part in order at the level
// measurement decided, optionally under a bed that fades in, plays past the
// last word, and fades away.
func filterGraph(parts []Part, input []int, n int, bed bool, story, tail time.Duration, levels Levels, gains []float64) string {
	var b strings.Builder
	for i, p := range parts {
		switch {
		case p.Kind == Silence:
			// A source filter, so a pause costs no input file and no
			// silent MP3 to carry around.
			fmt.Fprintf(&b, "anullsrc=r=44100:cl=stereo:d=%.3f,%s[a%d];",
				float64(p.MS)/1000, commonFormat, i)
		case gains[i] != 0:
			fmt.Fprintf(&b, "[%d:a]%s,volume=%.2fdB[a%d];", input[i], commonFormat, gains[i], i)
		default:
			fmt.Fprintf(&b, "[%d:a]%s[a%d];", input[i], commonFormat, i)
		}
	}
	for i := range parts {
		fmt.Fprintf(&b, "[a%d]", i)
	}
	fmt.Fprintf(&b, "concat=n=%d:v=0:a=1", len(parts))
	if tail > 0 {
		// Padding the story rather than extending the bed is what lets the
		// music outlast the last word: the padded story is still the first
		// input to amix, so it still decides the length, and the bed is
		// still cut back to it.
		fmt.Fprintf(&b, ",apad=pad_dur=%.3f", tail.Seconds())
	}
	b.WriteString("[story];")

	if !bed {
		// One more link so the -map target is always [out], whether or
		// not there is a bed.
		b.WriteString("[story]anull")
		writeFinal(&b, levels)
		return b.String()
	}

	fmt.Fprintf(&b, "[%d:a]%s,volume=%.2fdB", n, commonFormat, levels.BedGain)
	if levels.Measured {
		in, out := fades(story + tail)
		if in > 0 {
			fmt.Fprintf(&b, ",afade=t=in:st=0:d=%.3f", in.Seconds())
		}
		if out > 0 {
			// Positioned to land exactly on the end of the padded story,
			// which is where amix will cut the bed off anyway.
			fmt.Fprintf(&b, ",afade=t=out:st=%.3f:d=%.3f", (story + tail - out).Seconds(), out.Seconds())
		}
	}
	b.WriteString("[bed];")

	// Both inputs are finite files, so this is the ordinary case ffmpeg
	// handles well. duration=first says the story decides the length: the
	// bed was looped past the end on purpose, and this is what cuts it
	// back. normalize=0 keeps amix from scaling both inputs down to make
	// room — without it, adding a bed makes the narration quieter.
	b.WriteString("[story][bed]amix=inputs=2:duration=first:dropout_transition=0:normalize=0")
	writeFinal(&b, levels)
	return b.String()
}

// writeFinal applies the one gain that moves the finished episode onto
// TargetLUFS and closes the graph at [out], which -map always expects to
// find whether or not there was a bed to mix.
// It is always appended to a chain, never started fresh, so it brings its
// own separator.
func writeFinal(b *strings.Builder, levels Levels) {
	if !levels.Measured || levels.FinalGain == 0 {
		b.WriteString("[out]")
		return
	}
	fmt.Fprintf(b, ",volume=%.2fdB[out]", levels.FinalGain)
}
