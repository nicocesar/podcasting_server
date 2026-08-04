package mix

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests need a real ffmpeg, which is the point: the thing worth
// testing here is the filter graph, and a fake would only prove that the
// string we build is the string we expected to build. They skip on a
// machine without one, the same gate the live-vendor smoke tests use.
//
// The arithmetic that decides how loud an episode is lives in levels.go and
// is tested in levels_test.go without ffmpeg, so the rules survive on a
// machine where everything here skips.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if Available() {
		return
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		Binary = p
		return
	}
	t.Skip("ffmpeg not available")
}

// tone renders a test signal directly with ffmpeg, so the fixtures are
// generated rather than checked in and are guaranteed to be the
// mp3_44100_128 every producer in the server pins.
func tone(t *testing.T, dir string, hz int, seconds float64) []byte {
	return toneAt(t, dir, hz, seconds, 0)
}

// toneAt is tone at a chosen level, for the tests that are about levels.
//
// The gain is applied rather than assumed because lavfi's sine is not a
// full-scale signal — it arrives around -18 dBFS — so a fixture's absolute
// level is not something to reason about from the source. What these tests
// assert is always a relationship between two fixtures, which the gain
// difference does pin down exactly.
func toneAt(t *testing.T, dir string, hz int, seconds, gainDB float64) []byte {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%d-%.2f-%.1f.mp3", hz, seconds, gainDB))
	cmd := exec.Command(Binary, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency="+strconv.Itoa(hz)+":duration="+strconv.FormatFloat(seconds, 'f', 3, 64),
		"-af", fmt.Sprintf("volume=%.2fdB", gainDB),
		"-c:a", "libmp3lame", "-b:a", "128k", "-ar", "44100", "-ac", "2", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating fixture: %v: %s", err, out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// duration reads back what ffprobe says, which is the only opinion that
// matters for an enclosure a podcast client will scrub through.
func duration(t *testing.T, dir string, b []byte) float64 {
	t.Helper()
	path := filepath.Join(dir, "probe.mp3")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	probe := "ffprobe"
	if p := filepath.Join(filepath.Dir(Binary), "ffprobe"); fileExists(p) {
		probe = p
	}
	out, err := exec.Command(probe, "-v", "error", "-show_entries", "format=duration",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Skipf("ffprobe not available: %v", err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parsing duration %q: %v", out, err)
	}
	return d
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// loudnessOf meters a finished episode the way a podcast platform would.
// It runs the package's own measurement path, so these tests also cover the
// stderr parsing against whatever ffmpeg actually prints rather than
// against a fixture of what it printed once.
func loudnessOf(t *testing.T, dir string, b []byte) measurement {
	t.Helper()
	path := filepath.Join(dir, "loudness.mp3")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := measureFile(context.Background(), path, "loudnorm=print_format=json")
	if err != nil {
		t.Fatalf("measuring loudness: %v", err)
	}
	return m
}

// peakIn is the loudest sample inside one window of the output, which is
// how a test asks "how loud is it right here" — during an effect, during
// the narration, or after the last word while the bed fades.
func peakIn(t *testing.T, dir string, b []byte, start, length float64) float64 {
	t.Helper()
	path := filepath.Join(dir, "window.mp3")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, err := run(context.Background(), "-nostdin", "-hide_banner", "-loglevel", "info",
		"-ss", strconv.FormatFloat(start, 'f', 3, 64),
		"-t", strconv.FormatFloat(length, 'f', 3, 64),
		"-i", path, "-af", "volumedetect", "-f", "null", "-")
	if err != nil {
		t.Fatalf("measuring window: %v", err)
	}
	m, err := parseVolume(stderr)
	if err != nil {
		t.Fatalf("parsing window: %v", err)
	}
	return m.Max
}

func TestMixConcatenatesInOrder(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	a := tone(t, dir, 440, 2)
	b := tone(t, dir, 660, 1.5)

	out, err := Mix(context.Background(), []Part{{Audio: a, Label: "a"}, {Audio: b, Label: "b"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Audio) == 0 {
		t.Fatal("no audio")
	}
	// The parts run one after another, so the result is as long as both.
	// Tolerance is a frame or two: MP3 durations land on frame boundaries.
	if d := duration(t, dir, out.Audio); d < 3.4 || d > 3.7 {
		t.Errorf("duration = %.3fs, want ~3.5s (2.0 + 1.5)", d)
	}
}

func TestMixBedStopsWithTheStory(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	speech := tone(t, dir, 440, 2)
	// Deliberately shorter than the speech: the bed has to loop to cover
	// it, and deliberately looped forever, so something has to cut it off.
	bed := tone(t, dir, 220, 1)

	out, err := Mix(context.Background(), []Part{{Audio: speech, Label: "speech"}}, bed)
	if err != nil {
		t.Fatal(err)
	}
	// amix duration=first is what stops it. Without that, -stream_loop -1
	// runs the bed forever and the episode never ends. The story now
	// decides the length plus a fixed tail, not the bed's looped length —
	// which for a two-second fixture is most of the file and for a real
	// story is three seconds on the end of several minutes.
	want := 2.0 + BedTail.Seconds()
	if d := duration(t, dir, out.Audio); d < want-0.2 || d > want+0.2 {
		t.Errorf("duration = %.3fs, want ~%.1fs — the story's length plus the tail, not the bed's", d, want)
	}
}

// A single part with a bed is the shape that used to hang forever: with
// the bed looped infinitely, amix went on pulling music frames after the
// speech ended and ffmpeg never terminated. It reproduced at one part and
// not at two, which is exactly the kind of thing that reaches production.
// The deadline is the assertion — a regression does not fail this test, it
// stops it from ever finishing, so the test has to bound its own patience.
func TestMixSinglePartWithBedTerminates(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	speech := tone(t, dir, 440, 2)
	bed := tone(t, dir, 220, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := Mix(ctx, []Part{{Audio: speech, Label: "only"}}, bed)
	if err != nil {
		t.Fatalf("single part with a bed did not render: %v", err)
	}
	if len(out.Audio) == 0 {
		t.Fatal("no audio")
	}
}

func TestMixBedIsAudible(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	speech := tone(t, dir, 440, 2)
	bed := tone(t, dir, 220, 1)

	dry, err := Mix(context.Background(), []Part{{Audio: speech, Label: "speech"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wet, err := Mix(context.Background(), []Part{{Audio: speech, Label: "speech"}}, bed)
	if err != nil {
		t.Fatal(err)
	}
	// The bed has to actually reach the output. It would not if amix were
	// left to normalize, which silently scales every input down instead of
	// adding one underneath.
	if string(dry.Audio) == string(wet.Audio) {
		t.Error("bedded output is byte-identical to the dry one; the bed never made it into the mix")
	}
}

func TestMixRejectsEmptyInput(t *testing.T) {
	// Needs the binary even though nothing here reaches it: without one,
	// every case would "pass" on the ffmpeg-not-found error instead of on
	// the emptiness it is supposed to be testing.
	requireFFmpeg(t)
	for _, tc := range []struct {
		name  string
		parts []Part
	}{
		{"no parts", nil},
		{"empty part", []Part{{Audio: nil, Label: "silent"}}},
		{"only a zero-length pause", []Part{{Kind: Silence, MS: 0, Label: "nothing"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Mix(context.Background(), tc.parts, nil); err == nil {
				t.Error("want an error, got none")
			}
		})
	}
}

// The finished file has to land where a podcast player expects it,
// otherwise Story Time is the episode listeners reach for the volume knob
// on. The narration fixture is deliberately nowhere near the target, so
// passing means the gain was computed rather than inherited.
func TestMixHitsTargetLoudness(t *testing.T) {
	// Both directions: a narration that has to come up and one that has to
	// come down both land in the same place, which is what makes an
	// episode play at the same volume as whatever preceded it in a queue.
	for _, tc := range []struct {
		name   string
		gainDB float64
	}{
		{"quiet narration is brought up", -3},
		{"loud narration is brought down", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireFFmpeg(t)
			dir := t.TempDir()
			narration := toneAt(t, dir, 440, 3, tc.gainDB)

			out, err := Mix(context.Background(), []Part{{Audio: narration, Label: "narration"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !out.Levels.Measured {
				t.Fatalf("levels were not measured: %s", out.Levels.Note)
			}
			if out.Levels.FinalClamp != "" {
				t.Fatalf("gain was clamped by %q; the fixture is outside the range this test is about",
					out.Levels.FinalClamp)
			}
			got := loudnessOf(t, dir, out.Audio)
			if got.LUFS < TargetLUFS-1.5 || got.LUFS > TargetLUFS+1.5 {
				t.Errorf("finished episode measured %.2f LUFS, want ~%.0f", got.LUFS, TargetLUFS)
			}
			// And it must not have bought that loudness by clipping.
			if got.Peak > TruePeakCeilingDB+0.5 {
				t.Errorf("true peak %.2f dBFS is above the %.0f ceiling", got.Peak, TruePeakCeilingDB)
			}
		})
	}
}

// ADR 0032's stated worry, made into a test: "a generated effect that comes
// back hot will be loud in a story meant for a sleeping child". The effect
// fixture is 20dB above the narration, which is well past anything
// ElevenLabs would plausibly return, and it still must not end up louder
// than the narrator.
func TestMixHoldsAHotEffectUnderTheNarration(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	speech := toneAt(t, dir, 440, 2, -25)
	hot := toneAt(t, dir, 900, 1, -5)

	// The fixture really is hot to begin with, or this test proves nothing.
	if before := loudnessOf(t, dir, hot).Peak - loudnessOf(t, dir, speech).Peak; before < 15 {
		t.Fatalf("fixture effect is only %.1fdB above the narration; the test cannot show anything", before)
	}

	out, err := Mix(context.Background(), []Part{
		{Kind: Speech, Audio: speech, Label: "before"},
		{Kind: Effect, Audio: hot, Label: "thunder"},
		{Kind: Speech, Audio: speech, Label: "after"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The effect occupies 2.0-3.0s, between the two speech parts.
	narration := peakIn(t, dir, out.Audio, 0.2, 1.5)
	effect := peakIn(t, dir, out.Audio, 2.15, 0.7)
	if effect > narration+1.5 {
		t.Errorf("effect peaks at %.1f dBFS against narration at %.1f dBFS; it should not be louder than the narrator",
			effect, narration)
	}
}

// A pause the agent writes has to become silence a listener hears. It was
// validated, planned, and then discarded for the whole life of the
// performed program.
func TestMixSilenceAddsDuration(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	a := tone(t, dir, 440, 1.5)
	b := tone(t, dir, 660, 1.5)

	without, err := Mix(context.Background(), []Part{
		{Audio: a, Label: "a"}, {Audio: b, Label: "b"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Mix(context.Background(), []Part{
		{Audio: a, Label: "a"},
		{Kind: Silence, MS: 1000, Label: "pause"},
		{Audio: b, Label: "b"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	gap := duration(t, dir, with.Audio) - duration(t, dir, without.Audio)
	if gap < 0.9 || gap > 1.15 {
		t.Errorf("a 1000ms pause added %.3fs to the episode, want ~1.0s", gap)
	}
}

// The music carries on after the last word instead of stopping dead with
// it, and only when there is music to carry on.
func TestMixBedOutlastsTheStory(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	speech := tone(t, dir, 440, 3)
	bed := tone(t, dir, 220, 2)

	dry, err := Mix(context.Background(), []Part{{Audio: speech, Label: "speech"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wet, err := Mix(context.Background(), []Part{{Audio: speech, Label: "speech"}}, bed)
	if err != nil {
		t.Fatal(err)
	}

	if d := duration(t, dir, dry.Audio); d > 3.2 {
		t.Errorf("no bed, so nothing should play on after the last word: %.3fs, want ~3.0s", d)
	}
	tail := duration(t, dir, wet.Audio) - duration(t, dir, dry.Audio)
	if want := BedTail.Seconds(); tail < want-0.25 || tail > want+0.25 {
		t.Errorf("bed tail = %.3fs, want ~%.1fs", tail, want)
	}
}

// The bed fades away rather than being cut off. Measured in the tail, where
// the speech has ended and the only thing left is the music going quiet.
func TestMixBedFadesOut(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	speech := tone(t, dir, 440, 4)
	bed := tone(t, dir, 220, 2)

	out, err := Mix(context.Background(), []Part{{Audio: speech, Label: "speech"}}, bed)
	if err != nil {
		t.Fatal(err)
	}
	// Story is 4s, tail runs 4.0-7.0s. Early tail against late tail.
	early := peakIn(t, dir, out.Audio, 4.1, 0.5)
	late := peakIn(t, dir, out.Audio, 6.3, 0.5)
	if late >= early {
		t.Errorf("bed is not fading: %.1f dBFS early in the tail, %.1f dBFS late — want the later one quieter",
			early, late)
	}
}

// The guard that matters most. Narration that measures as silence is a
// broken render, not a quiet one, and "correcting" it means multiplying
// whatever noise is in there by a very large number. The mixer has to
// notice and leave everything alone instead.
func TestMixLeavesLevelsAloneWhenNarrationIsSilence(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	silent := toneAt(t, dir, 440, 2, -90)

	out, err := Mix(context.Background(), []Part{{Audio: silent, Label: "narration"}}, nil)
	if err != nil {
		t.Fatalf("a silent story should still publish, not fail: %v", err)
	}
	if out.Levels.Measured {
		t.Errorf("narration measured %.1f LUFS and was treated as usable", out.Levels.SpeechLUFS)
	}
	if out.Levels.Note == "" {
		t.Error("falling back to unmeasured levels has to say why, or a flat episode is a mystery")
	}
	if out.Levels.FinalGain != 0 {
		t.Errorf("applied %.1fdB of gain to something it could not measure", out.Levels.FinalGain)
	}
	if got := loudnessOf(t, dir, out.Audio); got.LUFS > SilenceFloorLUFS {
		t.Errorf("silence came out at %.1f LUFS; it was amplified", got.LUFS)
	}
}

func TestFilterGraphAlwaysEndsAtOut(t *testing.T) {
	// Mix maps [out] unconditionally, so every shape of graph has to
	// produce that label — including the no-bed case, where there is
	// nothing to mix and the concat would otherwise be the last link, and
	// the unmeasured case, where there is no final gain to close it with.
	for _, tc := range []struct {
		name     string
		parts    []Part
		bed      bool
		measured bool
	}{
		{"one part, no bed", []Part{{Label: "a"}}, false, true},
		{"several parts, no bed", []Part{{Label: "a"}, {Label: "b"}, {Label: "c"}}, false, true},
		{"one part with bed", []Part{{Label: "a"}}, true, true},
		{"several parts with bed", []Part{{Label: "a"}, {Label: "b"}}, true, true},
		{"unmeasured, no bed", []Part{{Label: "a"}}, false, false},
		{"unmeasured with bed", []Part{{Label: "a"}}, true, false},
		{"with a pause", []Part{{Label: "a"}, {Kind: Silence, MS: 500}, {Label: "b"}}, true, true},
		{"with an effect", []Part{{Label: "a"}, {Kind: Effect, Label: "fx"}}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := make([]int, len(tc.parts))
			gains := make([]float64, len(tc.parts))
			n := 0
			for i, p := range tc.parts {
				if p.Kind == Silence {
					input[i] = -1
					continue
				}
				input[i] = n
				n++
			}
			levels := unmeasured("test")
			tail := time.Duration(0)
			if tc.measured {
				levels = Levels{Measured: true, BedGain: -18, FinalGain: -2}
				if tc.bed {
					tail = BedTail
				}
			}
			g := filterGraph(tc.parts, input, n, tc.bed, 10*time.Second, tail, levels, gains)

			if !strings.HasSuffix(g, "[out]") {
				t.Errorf("graph does not end at [out]: %s", g)
			}
			if want := "concat=n=" + strconv.Itoa(len(tc.parts)); !strings.Contains(g, want) {
				t.Errorf("graph missing %q: %s", want, g)
			}
			if tc.bed && !strings.Contains(g, "normalize=0") {
				t.Errorf("bed mix must not normalize, or the story gets quieter: %s", g)
			}
			// An unmeasured mix keeps the old behaviour exactly: no gain
			// on the end, and no fades it has no reference for.
			if !tc.measured && strings.Contains(g, "afade") {
				t.Errorf("unmeasured graph should not fade: %s", g)
			}
			if tc.measured && tc.bed && !strings.Contains(g, "afade=t=out") {
				t.Errorf("a measured bed should fade out: %s", g)
			}
			if tc.measured && tc.bed && !strings.Contains(g, "apad") {
				t.Errorf("a measured bed should outlast the story: %s", g)
			}
		})
	}
}
