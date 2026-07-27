package coverart

// The generated cover art is line work: an outlined rounded rectangle, an
// outlined rounded square, and a glyph drawn inside it. Rather than carry
// a full 2D graphics dependency, every shape here is a polyline that gets
// stroked into filled polygons — one stroker handles lines, arcs, circles
// and rounded rects alike, because all of them flatten to points.

import (
	"image"
	"image/color"
	"math"

	"golang.org/x/image/vector"
)

// pt is a point in the coordinate space of whatever rasterizer is being
// filled, in pixels.
type pt struct{ x, y float64 }

// polyline is a flattened path. Closed paths join the last point back to
// the first; open ones get round caps, which is also what every join is,
// so the stroker only ever needs to know about discs and quads.
type polyline struct {
	pts    []pt
	closed bool
}

// stroke fills the outline of every polyline into z with the given stroke
// width. Each segment becomes a quad and each vertex a disc, so joins and
// caps come out round without any miter math.
//
// Every sub-polygon is emitted counter-clockwise on purpose: the vector
// rasterizer accumulates *signed* coverage and takes the absolute value,
// so a clockwise quad overlapping a counter-clockwise disc would cancel
// out and punch a hole at the join.
func stroke(z *vector.Rasterizer, pls []polyline, width float64) {
	h := width / 2
	if h <= 0 {
		return
	}
	for _, pl := range pls {
		n := len(pl.pts)
		if n == 0 {
			continue
		}
		if n == 1 {
			fillDisc(z, pl.pts[0], h)
			continue
		}
		segs := n - 1
		if pl.closed {
			segs = n
		}
		for i := 0; i < segs; i++ {
			fillSeg(z, pl.pts[i], pl.pts[(i+1)%n], h)
		}
		for _, p := range pl.pts {
			fillDisc(z, p, h)
		}
	}
}

// fillSeg emits the rectangle covering the segment a→b at half-width h.
func fillSeg(z *vector.Rasterizer, a, b pt, h float64) {
	dx, dy := b.x-a.x, b.y-a.y
	l := math.Hypot(dx, dy)
	if l == 0 {
		return
	}
	nx, ny := -dy/l*h, dx/l*h
	fillPoly(z, []pt{
		{a.x + nx, a.y + ny},
		{b.x + nx, b.y + ny},
		{b.x - nx, b.y - ny},
		{a.x - nx, a.y - ny},
	})
}

// fillDisc emits a polygon approximating the disc of radius h at c. The
// segment count follows the radius so small joins stay cheap and large
// caps stay smooth.
func fillDisc(z *vector.Rasterizer, c pt, h float64) {
	steps := int(h * 1.2)
	if steps < 10 {
		steps = 10
	}
	if steps > 64 {
		steps = 64
	}
	pts := make([]pt, steps)
	for i := range pts {
		a := 2 * math.Pi * float64(i) / float64(steps)
		pts[i] = pt{c.x + h*math.Cos(a), c.y + h*math.Sin(a)}
	}
	fillPoly(z, pts)
}

// fillPoly emits one closed sub-polygon, reversing it if needed so every
// sub-polygon in a drawing shares a winding direction.
func fillPoly(z *vector.Rasterizer, pts []pt) {
	if len(pts) < 3 {
		return
	}
	if signedArea(pts) < 0 {
		rev := make([]pt, len(pts))
		for i, p := range pts {
			rev[len(pts)-1-i] = p
		}
		pts = rev
	}
	z.MoveTo(float32(pts[0].x), float32(pts[0].y))
	for _, p := range pts[1:] {
		z.LineTo(float32(p.x), float32(p.y))
	}
	z.ClosePath()
}

func signedArea(pts []pt) float64 {
	var a float64
	for i, p := range pts {
		q := pts[(i+1)%len(pts)]
		a += p.x*q.y - q.x*p.y
	}
	return a / 2
}

// arcPts flattens an elliptical arc from angle a0 to a1 (radians, y down)
// into points, inclusive of both ends.
func arcPts(cx, cy, rx, ry, a0, a1 float64) []pt {
	span := math.Abs(a1 - a0)
	steps := int(span / (2 * math.Pi) * 64)
	if steps < 6 {
		steps = 6
	}
	pts := make([]pt, steps+1)
	for i := range pts {
		a := a0 + (a1-a0)*float64(i)/float64(steps)
		pts[i] = pt{cx + rx*math.Cos(a), cy + ry*math.Sin(a)}
	}
	return pts
}

// ellipse is a closed polyline around cx,cy.
func ellipse(cx, cy, rx, ry float64) polyline {
	p := arcPts(cx, cy, rx, ry, 0, 2*math.Pi)
	return polyline{pts: p[:len(p)-1], closed: true}
}

// roundRect is a closed polyline tracing a rounded rectangle. The radius
// is clamped to half the shorter side.
func roundRect(x0, y0, x1, y1, r float64) polyline {
	if w, h := x1-x0, y1-y0; r > w/2 {
		r = w / 2
	} else if r > h/2 {
		r = h / 2
	}
	if r < 0 {
		r = 0
	}
	var pts []pt
	// Corners in order, each arc sweeping a quarter turn clockwise in
	// screen space: top-left, top-right, bottom-right, bottom-left.
	pts = append(pts, arcPts(x0+r, y0+r, r, r, math.Pi, 1.5*math.Pi)...)
	pts = append(pts, arcPts(x1-r, y0+r, r, r, 1.5*math.Pi, 2*math.Pi)...)
	pts = append(pts, arcPts(x1-r, y1-r, r, r, 0, 0.5*math.Pi)...)
	pts = append(pts, arcPts(x0+r, y1-r, r, r, 0.5*math.Pi, math.Pi)...)
	return polyline{pts: pts, closed: true}
}

// strokeInto rasterizes polylines onto dst in col. The rasterizer covers
// box only, so a small shape does not pay for a canvas-sized coverage
// buffer; the polylines must already be in dst's coordinate space.
func strokeInto(dst *image.RGBA, box image.Rectangle, pls []polyline, width float64, col color.Color) {
	box = box.Intersect(dst.Bounds())
	if box.Empty() {
		return
	}
	z := vector.NewRasterizer(box.Dx(), box.Dy())
	local := make([]polyline, len(pls))
	for i, pl := range pls {
		lp := make([]pt, len(pl.pts))
		for j, p := range pl.pts {
			lp[j] = pt{p.x - float64(box.Min.X), p.y - float64(box.Min.Y)}
		}
		local[i] = polyline{pts: lp, closed: pl.closed}
	}
	stroke(z, local, width)
	z.Draw(dst, box, image.NewUniform(col), image.Point{})
}
