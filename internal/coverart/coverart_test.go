package coverart

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// solidJPEG encodes a w×h JPEG filled with one color.
func solidJPEG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// transparentPNG encodes a w×h fully-transparent PNG.
func transparentPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h)) // zero value = transparent
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dims(t *testing.T, b []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestProcessDownscalesLargeImage(t *testing.T) {
	in := solidJPEG(t, 4000, 2000, color.RGBA{200, 50, 50, 255})
	p, err := Process(bytes.NewReader(in), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if w, h := dims(t, p.Full); w != 3000 || h != 1500 {
		t.Errorf("full = %dx%d, want 3000x1500", w, h)
	}
	if w, h := dims(t, p.Thumb); w != 512 || h != 256 {
		t.Errorf("thumb = %dx%d, want 512x256", w, h)
	}
	if p.FullType != "image/jpeg" {
		t.Errorf("FullType = %q, want image/jpeg", p.FullType)
	}
	if len(p.Thumb) >= len(p.Full) {
		t.Errorf("thumb (%d) should be smaller than full (%d)", len(p.Thumb), len(p.Full))
	}
}

func TestProcessKeepsSmallOriginalVerbatim(t *testing.T) {
	in := solidJPEG(t, 300, 300, color.RGBA{10, 120, 200, 255})
	p, err := Process(bytes.NewReader(in), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Full, in) {
		t.Errorf("full should be the input bytes verbatim when under the ceiling")
	}
	// Thumb must not upscale past the 300px source.
	if w, h := dims(t, p.Thumb); w != 300 || h != 300 {
		t.Errorf("thumb = %dx%d, want 300x300 (no upscaling)", w, h)
	}
}

func TestProcessFlattensTransparencyToWhite(t *testing.T) {
	in := transparentPNG(t, 600, 600)
	p, err := Process(bytes.NewReader(in), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	img, format, err := image.Decode(bytes.NewReader(p.Thumb))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Errorf("thumb format = %q, want jpeg", format)
	}
	r, g, b, _ := img.At(10, 10).RGBA()
	if r < 0xf000 || g < 0xf000 || b < 0xf000 {
		t.Errorf("transparent pixel should be white, got r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
}

func TestProcessRejectsGarbage(t *testing.T) {
	if _, err := Process(bytes.NewReader([]byte("not an image")), "image/jpeg"); err == nil {
		t.Fatal("expected error decoding garbage bytes")
	}
}
