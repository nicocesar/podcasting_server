package coverart

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"reflect"
	"testing"
)

func TestGenerateProducesBothDerivatives(t *testing.T) {
	p, err := Generate(Spec{Text: "tech news"})
	if err != nil {
		t.Fatal(err)
	}
	if p.FullType != "image/png" {
		t.Errorf("FullType = %q, want image/png", p.FullType)
	}
	if w, h := dims(t, p.Full); w != GenEdge || h != GenEdge {
		t.Errorf("full = %dx%d, want %d square", w, h, GenEdge)
	}
	if w, h := dims(t, p.Thumb); w != 512 || h != 512 {
		t.Errorf("thumb = %dx%d, want 512 square", w, h)
	}
	// The thumb goes through the same path an upload does, so it is JPEG.
	if _, format, err := image.Decode(bytes.NewReader(p.Thumb)); err != nil || format != "jpeg" {
		t.Errorf("thumb: format %q, err %v", format, err)
	}
}

// TestGenerateIsDeterministic: regenerating art for a title must not
// quietly change its colour, or a strand's identity would drift every time
// somebody touched the admin page.
func TestGenerateIsDeterministic(t *testing.T) {
	a, err := Render(Spec{Text: "global news"}, 256)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(Spec{Text: "global news"}, 256)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Error("the same words produced different art")
	}
	// Different words, different mark — at minimum a different colour or
	// icon, so two strands do not look like the same one.
	c, err := Render(Spec{Text: "night talks"}, 256)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Pix, c.Pix) {
		t.Error("different words produced identical art")
	}
}

// TestRenderUsesTheChosenAccent: an explicit colour has to actually be the
// ink on the page, not just an accepted parameter.
func TestRenderUsesTheChosenAccent(t *testing.T) {
	img, err := Render(Spec{Text: "music", Accent: "red", Icon: "note"}, 512)
	if err != nil {
		t.Fatal(err)
	}
	want := mustHex("#bc3227")
	var found bool
	for y := 0; y < 512 && !found; y++ {
		for x := 0; x < 512; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if uint8(r>>8) == want.R && uint8(g>>8) == want.G && uint8(b>>8) == want.B {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("no pixel is the accent colour, so nothing was drawn in it")
	}
	// The corners are paper: the art must not bleed to the edge.
	for _, p := range []image.Point{{0, 0}, {511, 0}, {0, 511}, {511, 511}} {
		if got := color.RGBAModel.Convert(img.At(p.X, p.Y)).(color.RGBA); got != bgCream {
			t.Errorf("corner %v = %v, want the cream field", p, got)
		}
	}
}

// TestIconChosenFromWords is the feature in one line: the words pick the
// picture, so an admin who types two words picks nothing at all.
func TestIconChosenFromWords(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"global news", "globe"},
		{"tech news", "chip"},
		{"music", "note"},
		{"bedtime stories", "book"},
		{"late night talks", "chat"},
		{"the science hour", "flask"},
		{"unmatchable gibberish", defaultIcon},
	} {
		spec := Spec{Text: c.text}
		r, err := spec.resolve()
		if err != nil {
			t.Fatalf("%q: %v", c.text, err)
		}
		if !reflect.DeepEqual(r.icon, icons[c.want]) {
			t.Errorf("%q chose the wrong icon, want %s", c.text, c.want)
		}
	}
}

// TestWrapStacksWords: one or two words get a line each — the stacked
// logotype the reference art uses — and longer titles are balanced.
func TestWrapStacksWords(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"music", []string{"music"}},
		{"tech news", []string{"tech", "news"}},
		{"late night talks", []string{"late night", "talks"}},
	} {
		spec := Spec{Text: c.in}
		r, err := spec.resolve()
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if !reflect.DeepEqual(r.lines, c.want) {
			t.Errorf("%q wrapped to %q, want %q", c.in, r.lines, c.want)
		}
	}
}

// TestGenerateRefusesWhatItCannotSet: the caller needs an error it can put
// on the page, because the alternative is art with type too small to read.
func TestGenerateRefusesWhatItCannotSet(t *testing.T) {
	for _, c := range []struct {
		name string
		spec Spec
	}{
		{"empty", Spec{Text: "   "}},
		{"too many words", Spec{Text: "one two three four five six seven"}},
		{"words too long to set", Spec{Text: "extraordinarily circumlocutory broadcasting"}},
		{"unknown accent", Spec{Text: "music", Accent: "chartreuse"}},
		{"unknown icon", Spec{Text: "music", Icon: "unicorn"}},
	} {
		if _, err := Generate(c.spec); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
}

// TestPaletteAndIconsAreNamed: the admin page offers these by name, so
// every name it offers has to resolve.
func TestPaletteAndIconsAreNamed(t *testing.T) {
	if len(AccentNames()) != len(accents) {
		t.Errorf("AccentNames dropped an accent")
	}
	for _, name := range AccentNames() {
		if _, err := Render(Spec{Text: "sample", Accent: name}, 128); err != nil {
			t.Errorf("accent %q: %v", name, err)
		}
	}
	for _, name := range IconNames() {
		if _, err := Render(Spec{Text: "sample", Icon: name}, 128); err != nil {
			t.Errorf("icon %q: %v", name, err)
		}
	}
}

// TestStrokeJoinsAreSolid guards the winding rule in stroke(): the
// rasterizer takes the absolute value of signed coverage, so a sub-polygon
// wound the other way would cancel its neighbour and leave a hole at the
// join. A thick closed square is the clearest case — every pixel on the
// stroke's centre line must be inked.
func TestStrokeJoinsAreSolid(t *testing.T) {
	dst := image.NewRGBA(image.Rect(0, 0, 100, 100))
	ink := color.RGBA{0, 0, 0, 0xff}
	strokeInto(dst, dst.Bounds(), []polyline{
		{pts: []pt{{20, 20}, {80, 20}, {80, 80}, {20, 80}}, closed: true},
	}, 10, ink)
	for _, p := range []image.Point{
		{20, 20}, {80, 20}, {80, 80}, {20, 80}, // corners, where quads meet discs
		{50, 20}, {80, 50}, {50, 80}, {20, 50}, // mid-edges
	} {
		if _, _, _, a := dst.At(p.X, p.Y).RGBA(); a < 0xff00 {
			t.Errorf("hole in the stroke at %v (alpha %d)", p, a>>8)
		}
	}
	// ...and the middle stays empty: this is an outline, not a fill.
	if _, _, _, a := dst.At(50, 50).RGBA(); a != 0 {
		t.Errorf("the outline was filled in (alpha %d at the centre)", a>>8)
	}
}

// TestDumpSamples writes art to a directory for eyeballing. It is how the
// layout constants were calibrated against the reference covers, and stays
// here because the only real test of a logotype is looking at it:
//
//	ART_OUT=/tmp/art go test ./internal/coverart -run DumpSamples
func TestDumpSamples(t *testing.T) {
	out := os.Getenv("ART_OUT")
	if out == "" {
		t.Skip("set ART_OUT to a directory to dump sample art")
	}
	write := func(name string, spec Spec, edge int) {
		img, err := Render(spec, edge)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f, err := os.Create(out + "/" + name + ".png")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		spec Spec
	}{
		{"tech_news", Spec{Text: "tech news", Accent: "blue"}},
		{"music", Spec{Text: "music", Accent: "red"}},
		{"global_news", Spec{Text: "global news", Accent: "teal"}},
		{"stories", Spec{Text: "stories", Accent: "violet"}},
		{"late_night_talks", Spec{Text: "late night talks"}},
		{"auto_science", Spec{Text: "science hour"}},
	} {
		write(c.name, c.spec, 1242)
	}
	for _, name := range IconNames() {
		write("icon_"+name, Spec{Text: name, Icon: name, Accent: "blue"}, 620)
	}
}
