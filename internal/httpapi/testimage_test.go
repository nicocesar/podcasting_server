package httpapi

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// smallJPEG returns a valid w×h JPEG. Uploads in tests must be real
// images now that handleSetCover decodes them (coverart.Process).
func smallJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 120, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
