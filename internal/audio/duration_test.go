package audio

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// cbrMP3 builds a valid MPEG-1 Layer III stream: n CBR frames at
// 128 kbps / 44.1 kHz (417 bytes, 1152 samples ≈ 26.12 ms each).
func cbrMP3(n int) []byte {
	frame := make([]byte, 417)
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x00})
	return bytes.Repeat(frame, n)
}

func TestMP3Duration(t *testing.T) {
	const perFrame = time.Second * 1152 / 44100

	for _, n := range []int{1, 100, 383} {
		got, err := MP3Duration(bytes.NewReader(cbrMP3(n)))
		if err != nil {
			t.Fatalf("%d frames: %v", n, err)
		}
		want := time.Duration(n) * perFrame
		if diff := (got - want).Abs(); diff > 50*time.Millisecond {
			t.Errorf("%d frames: got %v, want ~%v", n, got, want)
		}
	}
}

func TestMP3DurationGarbageTail(t *testing.T) {
	in := append(cbrMP3(100), []byte("this is not an mpeg frame")...)
	got, err := MP3Duration(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := 100 * time.Second * 1152 / 44100
	if diff := (got - want).Abs(); diff > 50*time.Millisecond {
		t.Errorf("got %v, want ~%v", got, want)
	}
}

func TestMP3DurationRejectsNonMP3(t *testing.T) {
	for name, in := range map[string]string{
		"empty":   "",
		"garbage": strings.Repeat("definitely not audio ", 100),
	} {
		if _, err := MP3Duration(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
