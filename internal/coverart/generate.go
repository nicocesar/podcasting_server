package coverart

// Generated cover art (ADR 0020). An admin who
// creates a strand should not have to open a design tool first, so the
// backend draws the art the same way for everybody: cream field, a keyline
// box, one line icon, and the words set large and lowercase in a single
// accent colour. Two words in, a 3000px square out, ready for the feed.
//
// The look is a stacked logotype: each line is fitted to the same width
// independently, so "global" and "news" end up different sizes and the
// block reads as one mark rather than as a paragraph.

import (
	"bytes"
	"embed"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"sort"
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

//go:embed fonts/Poppins-ExtraBold.ttf
var fontFS embed.FS

// GenEdge is the size of the generated square. It matches the ceiling
// uploads are normalized to, so generated and uploaded art are
// interchangeable everywhere downstream.
const GenEdge = 3000

// Layout, as fractions of the canvas edge — the art has to hold up at
// 3000px in a feed and at 512 in a web card, so nothing here is absolute.
const (
	fInset      = 0.032  // keyline inset from the edge
	fKeyRadius  = 0.024  // keyline corner radius
	fKeyStroke  = 0.0030 // keyline stroke width
	fIconTop    = 0.141  // top of the icon frame
	fIconSize   = 0.222  // icon frame width and height
	fIconRadius = 0.052  // icon frame corner radius
	fIconStroke = 0.0038 // icon and glyph stroke width
	fGlyphInset = 0.225  // glyph inset inside the frame, as a fraction of the frame
	fTextWidth  = 0.78   // width every text line is fitted to
	fTextCenter = 0.605  // vertical centre of the cap-height block
	fTextTop    = 0.415  // the block may not climb above this (the icon is there)
	fTextBottom = 0.905  // nor below this
	fLineGap    = 0.014  // gap between stacked lines
	fMaxEm1     = 0.32   // em ceiling for a single line
	fMaxEm2     = 0.28   // ...for two lines
	fMaxEm3     = 0.21   // ...for three
)

// maxLines is how many lines the art will stack. Beyond that the words
// are too small to be a logotype, so Generate refuses instead.
const maxLines = 3

// accent is one of the colours the art can be drawn in. They are the
// design system's ink colours plus enough siblings that a canon of a dozen
// strands does not repeat itself.
type accent struct {
	Name string
	Hex  string
	col  color.RGBA
}

var accents = []accent{
	{Name: "blue", Hex: "#2b45c4"}, // riso blue, the house accent
	{Name: "red", Hex: "#bc3227"},  // ON AIR red
	{Name: "teal", Hex: "#0d8f74"},
	{Name: "violet", Hex: "#6d3fc9"},
	{Name: "ochre", Hex: "#b06d11"},
	{Name: "pink", Hex: "#b52a68"},
	{Name: "forest", Hex: "#2c7136"},
	{Name: "navy", Hex: "#1f3566"},
}

// bgCream is the paper the art is printed on: the app's card cream, warmed
// a shade so the keyline has something to sit against.
var bgCream = color.RGBA{0xf8, 0xf4, 0xea, 0xff}

func init() {
	for i := range accents {
		accents[i].col = mustHex(accents[i].Hex)
	}
}

func mustHex(s string) color.RGBA {
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b); err != nil {
		panic("coverart: bad accent hex " + s)
	}
	return color.RGBA{r, g, b, 0xff}
}

// AccentNames lists the accent colours by name, in palette order.
func AccentNames() []string {
	names := make([]string, len(accents))
	for i, a := range accents {
		names[i] = a.Name
	}
	return names
}

// Spec describes one piece of generated art. Only Text is required;
// leaving Accent and Icon empty derives both from the words, which is what
// makes "type two words, get a cover" work.
type Spec struct {
	// Text is the wording on the art — normally the strand's title. It is
	// lowercased and split into at most three stacked lines.
	Text string
	// Accent names a colour from AccentNames. Empty derives one from Text,
	// so the same strand always comes out the same colour.
	Accent string
	// Icon names a glyph from IconNames. Empty derives one from Text.
	Icon string
}

// resolved is a Spec with every blank filled in.
type resolved struct {
	lines []string
	col   color.RGBA
	icon  []polyline
}

func (s Spec) resolve() (resolved, error) {
	words := strings.Fields(strings.ToLower(s.Text))
	if len(words) == 0 {
		return resolved{}, fmt.Errorf("cover art needs at least one word")
	}
	lines, err := wrap(words)
	if err != nil {
		return resolved{}, err
	}

	col, err := accentColor(s.Accent, strings.Join(words, " "))
	if err != nil {
		return resolved{}, err
	}
	name := s.Icon
	if name == "" {
		name = iconFor(words)
	}
	glyph, ok := icons[name]
	if !ok {
		return resolved{}, fmt.Errorf("unknown icon %q", name)
	}
	return resolved{lines: lines, col: col, icon: glyph}, nil
}

// accentColor honours an explicit name, and otherwise picks from the
// palette by hashing the words — deterministic, so regenerating art for
// the same title does not change its colour.
func accentColor(name, key string) (color.RGBA, error) {
	if name != "" {
		for _, a := range accents {
			if a.Name == name {
				return a.col, nil
			}
		}
		return color.RGBA{}, fmt.Errorf("unknown accent %q", name)
	}
	h := fnv.New32a()
	h.Write([]byte(key))
	return accents[int(h.Sum32())%len(accents)].col, nil
}

// wrap stacks words into lines: one or two words get a line each (the
// stacked-logotype look), and more are balanced across up to maxLines by
// evening out their lengths.
func wrap(words []string) ([]string, error) {
	if len(words) <= 2 {
		return words, nil
	}
	if len(words) > 6 {
		return nil, fmt.Errorf("cover art takes at most six words, got %d", len(words))
	}
	// Try two lines, then three, and keep the first split whose longest
	// line is short enough to still be set large.
	for n := 2; n <= maxLines; n++ {
		if lines, ok := balance(words, n); ok {
			return lines, nil
		}
	}
	return nil, fmt.Errorf("those words will not fit on cover art; use a shorter title")
}

// balance splits words into exactly n lines, minimizing the longest line,
// and reports whether the result is set-able (no line longer than 14
// characters, past which the type would shrink below the icon's weight).
func balance(words []string, n int) ([]string, bool) {
	best, bestMax := []string(nil), 1<<30
	// Enumerate the n-1 break positions; with at most six words this is a
	// handful of combinations.
	var rec func(start, left int, acc []string)
	rec = func(start, left int, acc []string) {
		if left == 1 {
			cand := append(append([]string(nil), acc...), strings.Join(words[start:], " "))
			longest := 0
			for _, l := range cand {
				if len(l) > longest {
					longest = len(l)
				}
			}
			if longest < bestMax {
				best, bestMax = cand, longest
			}
			return
		}
		for end := start + 1; end <= len(words)-(left-1); end++ {
			rec(end, left-1, append(acc, strings.Join(words[start:end], " ")))
		}
	}
	rec(0, n, nil)
	return best, bestMax <= 14
}

// parsedFont is the embedded Poppins ExtraBold, parsed once. It is the
// closest practical match to the reference art: geometric, very heavy, and
// legible when set lowercase at this size.
var parsedFont = sync.OnceValues(func() (*sfnt.Font, error) {
	b, err := fontFS.ReadFile("fonts/Poppins-ExtraBold.ttf")
	if err != nil {
		return nil, err
	}
	return opentype.Parse(b)
})

// Generate draws the art and returns the same two derivatives an upload
// produces, so it can be handed straight to SetStrandCover.
func Generate(spec Spec) (Processed, error) {
	img, err := Render(spec, GenEdge)
	if err != nil {
		return Processed{}, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return Processed{}, err
	}
	thumb, err := makeThumb(img)
	if err != nil {
		return Processed{}, err
	}
	return Processed{Full: buf.Bytes(), FullType: "image/png", Thumb: thumb}, nil
}

// Render draws the art at an arbitrary edge size. Small sizes are for the
// admin preview; the stored art is GenEdge.
func Render(spec Spec, edge int) (*image.RGBA, error) {
	if edge < 64 {
		return nil, fmt.Errorf("cover art edge %d is too small", edge)
	}
	r, err := spec.resolve()
	if err != nil {
		return nil, err
	}
	e := float64(edge)
	dst := image.NewRGBA(image.Rect(0, 0, edge, edge))
	xdraw.Draw(dst, dst.Bounds(), image.NewUniform(bgCream), image.Point{}, xdraw.Src)

	drawKeyline(dst, e, r.col)
	drawIcon(dst, e, r.icon, r.col)
	if err := drawText(dst, e, r.lines, r.col); err != nil {
		return nil, err
	}
	return dst, nil
}

// drawKeyline strokes the outer rounded rectangle. The straight runs are
// plain rectangle fills and only the four corners need a rasterizer, which
// keeps a 3000px canvas from allocating a 3000px coverage buffer.
func drawKeyline(dst *image.RGBA, e float64, col color.RGBA) {
	in := fInset * e
	rad := fKeyRadius * e
	w := fKeyStroke * e
	x0, y0, x1, y1 := in, in, e-in, e-in
	src := image.NewUniform(col)
	h := w / 2

	fill := func(a, b, c, d float64) {
		rect := image.Rect(int(a+0.5), int(b+0.5), int(c+0.5), int(d+0.5))
		xdraw.Draw(dst, rect, src, image.Point{}, xdraw.Src)
	}
	fill(x0+rad, y0-h, x1-rad, y0+h) // top
	fill(x0+rad, y1-h, x1-rad, y1+h) // bottom
	fill(x0-h, y0+rad, x0+h, y1-rad) // left
	fill(x1-h, y0+rad, x1+h, y1-rad) // right

	pad := w + 2
	for _, c := range []struct{ cx, cy, a0, a1 float64 }{
		{x0 + rad, y0 + rad, 180, 270},
		{x1 - rad, y0 + rad, 270, 360},
		{x1 - rad, y1 - rad, 0, 90},
		{x0 + rad, y1 - rad, 90, 180},
	} {
		arc := polyline{pts: arcPts(c.cx, c.cy, rad, rad, deg(c.a0), deg(c.a1))}
		box := image.Rect(
			int(c.cx-rad-pad), int(c.cy-rad-pad),
			int(c.cx+rad+pad), int(c.cy+rad+pad))
		strokeInto(dst, box, []polyline{arc}, w, col)
	}
}

func deg(d float64) float64 { return d * 3.14159265358979323846 / 180 }

// drawIcon strokes the icon frame and its glyph, both inside one
// rasterizer over the frame's box.
func drawIcon(dst *image.RGBA, e float64, glyph []polyline, col color.RGBA) {
	size := fIconSize * e
	x0 := (e - size) / 2
	y0 := fIconTop * e
	w := fIconStroke * e

	pls := []polyline{roundRect(x0, y0, x0+size, y0+size, fIconRadius*e)}
	// Each glyph is fitted to the frame's inner square by its own bounding
	// box rather than by its nominal 0..100 space: an icon drawn a little
	// small or a little off-centre still lands the same size as its
	// neighbours, which is the only way a set drawn by hand looks like a set.
	inner := size * (1 - 2*fGlyphInset)
	bx0, by0, bx1, by1 := glyphBounds(glyph)
	gw, gh := bx1-bx0, by1-by0
	scale := inner / max(gw, gh)
	// Centre the fitted box in the frame.
	ox := x0 + size/2 - (bx0+gw/2)*scale
	oy := y0 + size/2 - (by0+gh/2)*scale
	for _, pl := range glyph {
		mapped := make([]pt, len(pl.pts))
		for i, p := range pl.pts {
			mapped[i] = pt{ox + p.x*scale, oy + p.y*scale}
		}
		pls = append(pls, polyline{pts: mapped, closed: pl.closed})
	}
	pad := w + 2
	box := image.Rect(int(x0-pad), int(y0-pad), int(x0+size+pad), int(y0+size+pad))
	strokeInto(dst, box, pls, w, col)
}

// glyphBounds is the bounding box of an icon's points, ignoring stroke
// width (which is the same for every icon, so it cancels out).
func glyphBounds(glyph []polyline) (x0, y0, x1, y1 float64) {
	first := true
	for _, pl := range glyph {
		for _, p := range pl.pts {
			if first {
				x0, y0, x1, y1 = p.x, p.y, p.x, p.y
				first = false
				continue
			}
			x0, y0 = min(x0, p.x), min(y0, p.y)
			x1, y1 = max(x1, p.x), max(y1, p.y)
		}
	}
	return x0, y0, x1, y1
}

// measureEm is the size the reference face is measured at. Advances scale
// linearly with size, so one measurement per line is enough to solve for
// the size that fits the target width.
const measureEm = 1000

// drawText sets each line at its own size, fitted to the same width, and
// stacks the cap-height boxes tightly around the block centre.
func drawText(dst *image.RGBA, e float64, lines []string, col color.RGBA) error {
	f, err := parsedFont()
	if err != nil {
		return err
	}
	ref, err := opentype.NewFace(f, &opentype.FaceOptions{Size: measureEm, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return err
	}
	defer ref.Close()
	refCap := f26(ref.Metrics().CapHeight)

	target := fTextWidth * e
	maxEm := map[int]float64{1: fMaxEm1, 2: fMaxEm2, 3: fMaxEm3}[len(lines)] * e

	sizes := make([]float64, len(lines))
	widths := make([]float64, len(lines))
	depths := make([]float64, len(lines))
	for i, l := range lines {
		bounds, adv := font.BoundString(ref, l)
		if adv <= 0 {
			return fmt.Errorf("cover art text %q has no width", l)
		}
		size := target / f26(adv) * measureEm
		if size > maxEm {
			size = maxEm
		}
		sizes[i] = size
		widths[i] = f26(adv) * size / measureEm
		// How far this line's glyphs actually reach below the baseline, at
		// this line's own size. Measured rather than guessed from the
		// letters, because each line is set at a different size and a "g"
		// over a big line needs more room than over a small one.
		depths[i] = max(0, f26(bounds.Max.Y)) * size / measureEm
	}

	// Stack: cap heights, plus whatever the line above hangs below its
	// baseline, plus a constant gap. Lines with nothing below the baseline
	// end up as tight as the reference art.
	gaps := make([]float64, len(lines)-1)
	for i := range gaps {
		gaps[i] = depths[i] + fLineGap*e
	}
	caps := make([]float64, len(lines))
	block := 0.0
	for i, s := range sizes {
		caps[i] = refCap * s / measureEm
		block += caps[i]
	}
	for _, g := range gaps {
		block += g
	}

	// Keep the block inside its zone, shrinking everything together if a
	// long stack would otherwise reach the icon.
	zoneTop, zoneBot := fTextTop*e, fTextBottom*e
	if zone := zoneBot - zoneTop; block > zone {
		k := zone / block
		for i := range sizes {
			sizes[i] *= k
			widths[i] *= k
			caps[i] *= k
		}
		for i := range gaps {
			gaps[i] *= k
		}
		block = zone
	}
	top := fTextCenter*e - block/2
	if top < zoneTop {
		top = zoneTop
	}

	y := top
	for i, l := range lines {
		face, err := opentype.NewFace(f, &opentype.FaceOptions{
			Size: sizes[i], DPI: 72, Hinting: font.HintingNone,
		})
		if err != nil {
			return err
		}
		baseline := y + caps[i]
		d := &font.Drawer{
			Dst:  dst,
			Src:  image.NewUniform(col),
			Face: face,
			Dot: fixed.Point26_6{
				X: fixed.Int26_6((e - widths[i]) / 2 * 64),
				Y: fixed.Int26_6(baseline * 64),
			},
		}
		d.DrawString(l)
		face.Close()
		y = baseline
		if i < len(gaps) {
			y += gaps[i]
		}
	}
	return nil
}

func f26(v fixed.Int26_6) float64 { return float64(v) / 64 }

// IconKeywordsFor reports which words steer the icon choice, for the admin
// page's help text. Only a sample is useful, so it returns them grouped by
// icon and sorted.
func IconKeywordsFor(icon string) []string {
	var out []string
	for w, i := range iconKeywords {
		if i == icon {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}
