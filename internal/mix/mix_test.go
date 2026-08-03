package mix

import (
	"context"
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
	t.Helper()
	path := filepath.Join(dir, strconv.Itoa(hz)+"-"+strconv.FormatFloat(seconds, 'f', 2, 64)+".mp3")
	cmd := exec.Command(Binary, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency="+strconv.Itoa(hz)+":duration="+strconv.FormatFloat(seconds, 'f', 3, 64),
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

func TestMixConcatenatesInOrder(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	a := tone(t, dir, 440, 2)
	b := tone(t, dir, 660, 1.5)

	out, err := Mix(context.Background(), []Part{{Audio: a, Label: "a"}, {Audio: b, Label: "b"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no audio")
	}
	// The parts run one after another, so the result is as long as both.
	// Tolerance is a frame or two: MP3 durations land on frame boundaries.
	if d := duration(t, dir, out); d < 3.4 || d > 3.7 {
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
	// runs the bed forever and the episode never ends.
	if d := duration(t, dir, out); d < 1.9 || d > 2.2 {
		t.Errorf("duration = %.3fs, want ~2.0s — the story's length, not the bed's", d)
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
	if len(out) == 0 {
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
	if string(dry) == string(wet) {
		t.Error("bedded output is byte-identical to the dry one; the bed never made it into the mix")
	}
}

func TestMixRejectsEmptyInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []Part
	}{
		{"no parts", nil},
		{"empty part", []Part{{Audio: nil, Label: "silent"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Mix(context.Background(), tc.parts, nil); err == nil {
				t.Error("want an error, got none")
			}
		})
	}
}

func TestFilterGraphAlwaysEndsAtOut(t *testing.T) {
	// Mix maps [out] unconditionally, so every shape of graph has to
	// produce that label — including the no-bed case, where there is
	// nothing to mix and the concat would otherwise be the last link.
	for _, tc := range []struct {
		name string
		n    int
		bed  bool
	}{
		{"one part, no bed", 1, false},
		{"several parts, no bed", 4, false},
		{"one part with bed", 1, true},
		{"several parts with bed", 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := filterGraph(tc.n, tc.bed)
			if !strings.HasSuffix(g, "[out]") {
				t.Errorf("graph does not end at [out]: %s", g)
			}
			if want := "concat=n=" + strconv.Itoa(tc.n); !strings.Contains(g, want) {
				t.Errorf("graph missing %q: %s", want, g)
			}
			if tc.bed && !strings.Contains(g, "normalize=0") {
				t.Errorf("bed mix must not normalize, or the story gets quieter: %s", g)
			}
		})
	}
}
