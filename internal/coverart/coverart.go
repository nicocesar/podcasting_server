// Package coverart normalizes an uploaded cover image into the two
// derivatives the app serves: a full-size original capped at Apple's
// ceiling (kept for the RSS <itunes:image>), and a small thumbnail for
// the web cards. Decoding happens once, at upload time (ADR: eager
// resize) — the serve path stays a byte-copy.
package coverart

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"

	xdraw "golang.org/x/image/draw"
)

const (
	// maxFullEdge caps the longest edge of the stored original. Apple
	// Podcasts accepts cover art up to 3000 px square; anything larger
	// is wasted bytes and risks feed rejection.
	maxFullEdge = 3000
	// thumbEdge is the longest edge of the web thumbnail — enough to
	// stay crisp on a 2× retina card without shipping the full art.
	thumbEdge = 512
	// thumbQuality is the JPEG quality of the thumbnail.
	thumbQuality = 82
)

// Processed holds the two derivatives of an uploaded cover.
type Processed struct {
	// Full is the normalized original: proportionally downscaled to
	// maxFullEdge if it was larger, otherwise the input bytes verbatim.
	Full []byte
	// FullType is the MIME type of Full ("image/jpeg" or "image/png").
	FullType string
	// Thumb is the web thumbnail, always JPEG.
	Thumb []byte
}

// Process decodes the upload and produces both derivatives. contentType
// must be "image/jpeg" or "image/png" (the handler already enforces this).
// A decode failure returns an error so the upload PUT fails loudly rather
// than corrupting the feed later.
func Process(r io.Reader, contentType string) (Processed, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Processed{}, err
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Processed{}, fmt.Errorf("decode cover: %w", err)
	}

	full, fullType, err := normalizeFull(img, raw, contentType)
	if err != nil {
		return Processed{}, err
	}
	thumb, err := makeThumb(img)
	if err != nil {
		return Processed{}, err
	}
	return Processed{Full: full, FullType: fullType, Thumb: thumb}, nil
}

// normalizeFull returns the original bytes untouched when the image
// already fits within maxFullEdge; otherwise it downscales and re-encodes
// in the source format. Images are never upscaled.
func normalizeFull(img image.Image, raw []byte, contentType string) ([]byte, string, error) {
	b := img.Bounds()
	if b.Dx() <= maxFullEdge && b.Dy() <= maxFullEdge {
		return raw, contentType, nil
	}
	scaled := scaleToFit(img, maxFullEdge)
	var buf bytes.Buffer
	switch contentType {
	case "image/png":
		if err := png.Encode(&buf, scaled); err != nil {
			return nil, "", err
		}
	default:
		if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: 90}); err != nil {
			return nil, "", err
		}
		contentType = "image/jpeg"
	}
	return buf.Bytes(), contentType, nil
}

// makeThumb flattens the image onto white (so PNG transparency doesn't
// become black in JPEG), scales the longest edge down to thumbEdge, and
// encodes JPEG. Never upscales past the source.
func makeThumb(img image.Image) ([]byte, error) {
	flat := flattenWhite(img)
	thumb := scaleToFit(flat, thumbEdge)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: thumbQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// flattenWhite draws img over an opaque white background, discarding any
// alpha channel.
func flattenWhite(img image.Image) image.Image {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	xdraw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, xdraw.Src)
	xdraw.Draw(dst, dst.Bounds(), img, b.Min, xdraw.Over)
	return dst
}

// scaleToFit proportionally resizes img so its longest edge is at most
// maxEdge, using Catmull-Rom for quality. Images already within maxEdge
// are returned unchanged (no upscaling).
func scaleToFit(img image.Image, maxEdge int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxEdge && h <= maxEdge {
		return img
	}
	nw, nh := w, h
	if w >= h {
		nw = maxEdge
		nh = h * maxEdge / w
	} else {
		nh = maxEdge
		nw = w * maxEdge / h
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	return dst
}
