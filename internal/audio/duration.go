// Package audio derives metadata from uploaded audio. The Publishing
// Contract lets a dumb publisher omit duration_seconds; the server fills
// it in from the MP3 itself (docs/adr/0004).
package audio

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tcolgate/mp3"
)

// MP3Duration walks the MPEG frames of r and sums their durations. It is
// exact for CBR and VBR alike and never decodes audio. A truncated or
// garbage tail yields the duration of the frames before it; input with
// no parseable frames at all is an error.
func MP3Duration(r io.Reader) (total time.Duration, err error) {
	// The frame walker sees external input; a panic on a malformed
	// header must become "estimation failed", not a crash.
	defer func() {
		if p := recover(); p != nil {
			total, err = 0, fmt.Errorf("mp3 frame walk panicked: %v", p)
		}
	}()

	d := mp3.NewDecoder(r)
	var (
		frame   mp3.Frame
		skipped int
		frames  int
	)
	for {
		if err := d.Decode(&frame, &skipped); err != nil {
			switch {
			case frames > 0:
				return total, nil
			case errors.Is(err, io.EOF):
				return 0, errors.New("no MP3 frames found")
			default:
				return 0, err
			}
		}
		frames++
		total += frame.Duration()
	}
}
