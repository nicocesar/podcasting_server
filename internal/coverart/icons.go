package coverart

// The icon library. Every icon is line work drawn in a 0..100 square, the
// same weight and the same rounded-square frame, so a strand created from
// two words comes out looking like it belongs to the same set as the ones
// beside it.
//
// Icons are chosen from the words of the title (see iconFor), which is the
// whole point: the admin types "global news" and gets a globe without
// picking anything. When nothing matches, the fallback is a waveform —
// true of every strand here.

import (
	"math"
	"sort"
)

// icons maps an icon name to its glyph, in a 0..100 coordinate space with
// y pointing down. The frame around it is drawn by the renderer.
var icons = map[string][]polyline{
	"wave":  iconWave(),
	"mic":   iconMic(),
	"globe": iconGlobe(),
	"chip":  iconChip(),
	"note":  iconNote(),
	"book":  iconBook(),
	"chat":  iconChat(),
	"star":  iconStar(),
	"ball":  iconBall(),
	"flask": iconFlask(),
	"chart": iconChart(),
	"pin":   iconPin(),
	"film":  iconFilm(),
}

// defaultIcon is the fallback when no word in the title matches.
const defaultIcon = "wave"

// iconKeywords maps a single lowercase word to an icon. First match in
// title order wins, so "global news" finds the globe before "news" could
// mean anything else.
var iconKeywords = map[string]string{
	"world": "globe", "global": "globe", "earth": "globe", "planet": "globe",
	"international": "globe", "politics": "globe", "policy": "globe",
	"news": "globe", "headlines": "globe", "daily": "globe", "briefing": "globe",

	"tech": "chip", "technology": "chip", "ai": "chip", "code": "chip",
	"dev": "chip", "software": "chip", "robot": "chip", "robots": "chip",
	"machine": "chip", "computer": "chip", "computing": "chip", "future": "chip",
	"crypto": "chip", "cyber": "chip", "data": "chip",

	"music": "note", "song": "note", "songs": "note", "sound": "note",
	"sounds": "note", "album": "note", "albums": "note", "band": "note",
	"jazz": "note", "rock": "note", "beats": "note", "playlist": "note",

	"story": "book", "stories": "book", "book": "book", "books": "book",
	"read": "book", "reading": "book", "fiction": "book", "tales": "book",
	"tale": "book", "novel": "book", "poetry": "book", "history": "book",
	"literature": "book", "bedtime": "book",

	"talk": "chat", "talks": "chat", "interview": "chat", "interviews": "chat",
	"conversation": "chat", "conversations": "chat", "chat": "chat",
	"debate": "chat", "opinion": "chat", "voices": "chat", "questions": "chat",

	"radio": "mic", "show": "mic", "pod": "mic", "podcast": "mic",
	"live": "mic", "hour": "mic", "session": "mic", "sessions": "mic",
	"comedy": "mic", "standup": "mic", "host": "mic",

	"culture": "star", "art": "star", "arts": "star", "design": "star",
	"style": "star", "fashion": "star", "review": "star", "reviews": "star",
	"best": "star", "picks": "star", "magic": "star", "wonder": "star",

	"sport": "ball", "sports": "ball", "game": "ball", "games": "ball",
	"gaming": "ball", "football": "ball", "soccer": "ball", "match": "ball",
	"league": "ball", "team": "ball", "fitness": "ball",

	"science": "flask", "research": "flask", "health": "flask",
	"medicine": "flask", "biology": "flask", "chemistry": "flask",
	"physics": "flask", "space": "flask", "nature": "flask", "climate": "flask",
	"lab": "flask", "study": "flask",

	"business": "chart", "finance": "chart", "money": "chart",
	"market": "chart", "markets": "chart", "economy": "chart",
	"economics": "chart", "stocks": "chart", "startup": "chart",
	"startups": "chart", "work": "chart", "trends": "chart",

	"local": "pin", "city": "pin", "travel": "pin", "places": "pin",
	"neighborhood": "pin", "town": "pin", "streets": "pin", "maps": "pin",
	"guide": "pin", "here": "pin",

	"film": "film", "films": "film", "movie": "film", "movies": "film",
	"cinema": "film", "tv": "film", "video": "film", "screen": "film",
	"watch": "film", "series": "film",

	"audio": "wave", "voice": "wave", "signal": "wave", "frequency": "wave",
	"waves": "wave", "static": "wave", "airwaves": "wave", "noise": "wave",
}

// IconNames lists every icon that can be named explicitly, sorted so the
// admin picker has a stable order.
func IconNames() []string {
	names := make([]string, 0, len(icons))
	for n := range icons {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// iconFor picks an icon from the title's words, first match winning, and
// falls back to defaultIcon.
func iconFor(words []string) string {
	for _, w := range words {
		if name, ok := iconKeywords[w]; ok {
			return name
		}
	}
	return defaultIcon
}

// bow is an arc from (x0,y0) to (x1,y1) bulging sag units to the right of
// the direction of travel — a parabola, close enough for line art and far
// cheaper to reason about than a real Bézier.
func bow(x0, y0, x1, y1, sag float64) polyline {
	const steps = 20
	dx, dy := x1-x0, y1-y0
	l := math.Hypot(dx, dy)
	if l == 0 {
		return polyline{pts: []pt{{x0, y0}}}
	}
	nx, ny := -dy/l, dx/l
	pts := make([]pt, steps+1)
	for i := range pts {
		t := float64(i) / steps
		// 4t(1-t) peaks at 1 in the middle and is 0 at both ends.
		off := sag * 4 * t * (1 - t)
		pts[i] = pt{x0 + dx*t + nx*off, y0 + dy*t + ny*off}
	}
	return pts2pl(pts)
}

func pts2pl(pts []pt) polyline { return polyline{pts: pts} }

func line(x0, y0, x1, y1 float64) polyline {
	return polyline{pts: []pt{{x0, y0}, {x1, y1}}}
}

func iconWave() []polyline {
	heights := []float64{16, 28, 40, 48, 40, 28, 16}
	var pls []polyline
	for i, h := range heights {
		x := 20 + float64(i)*10
		pls = append(pls, line(x, 50-h/2, x, 50+h/2))
	}
	return pls
}

func iconMic() []polyline {
	return []polyline{
		roundRect(40, 16, 60, 52, 10),
		pts2pl(arcPts(50, 44, 19, 19, 0, math.Pi)),
		line(50, 63, 50, 78),
		line(36, 80, 64, 80),
	}
}

func iconGlobe() []polyline {
	const cx, cy, r = 50, 50, 33
	return []polyline{
		ellipse(cx, cy, r, r),
		ellipse(cx, cy, 14, r),
		line(cx-r, cy, cx+r, cy),
		bow(cx-27, cy-17, cx+27, cy-17, 6),
		bow(cx-27, cy+17, cx+27, cy+17, -6),
	}
}

func iconChip() []polyline {
	pls := []polyline{
		roundRect(24, 24, 76, 76, 8),
		roundRect(40, 40, 60, 60, 3),
	}
	for _, at := range []float64{36, 50, 64} {
		pls = append(pls,
			line(14, at, 24, at),
			line(76, at, 86, at),
			line(at, 14, at, 24),
			line(at, 76, at, 86),
		)
	}
	return pls
}

func iconNote() []polyline {
	return []polyline{
		ellipse(33, 70, 10, 8.5),
		ellipse(63, 63, 10, 8.5),
		line(43, 70, 43, 32),
		line(73, 63, 73, 25),
		// The beam, as an outlined parallelogram.
		polyline{pts: []pt{{43, 32}, {73, 25}, {73, 36}, {43, 43}}, closed: true},
		// Ticks either side, like the reference art: a tall one outside, a
		// short one in, mirrored around the note.
		line(8, 44, 8, 62),
		line(18, 48, 18, 58),
		line(88, 37, 88, 55),
		line(80, 41, 80, 51),
	}
}

func iconBook() []polyline {
	return []polyline{
		line(50, 34, 50, 78),
		// Two facing pages, each a bowed outline meeting at the spine.
		polyline{pts: append(
			bow(50, 34, 16, 40, 3).pts,
			append([]pt{{16, 72}}, bow(16, 72, 50, 78, -3).pts...)...,
		)},
		polyline{pts: append(
			bow(50, 34, 84, 40, -3).pts,
			append([]pt{{84, 72}}, bow(84, 72, 50, 78, 3).pts...)...,
		)},
		line(24, 50, 43, 47),
		line(24, 60, 43, 57),
		line(57, 47, 76, 50),
		line(57, 57, 76, 60),
	}
}

func iconChat() []polyline {
	return []polyline{
		roundRect(14, 22, 64, 54, 10),
		polyline{pts: []pt{{26, 54}, {26, 66}, {38, 54}}},
		roundRect(40, 46, 86, 74, 10),
		polyline{pts: []pt{{74, 74}, {74, 84}, {64, 74}}},
	}
}

// sparkle is a four-pointed star: long points on the axes, pinched in
// between.
func sparkle(cx, cy, outer, inner float64) polyline {
	var pts []pt
	for i := 0; i < 8; i++ {
		a := math.Pi / 2 * float64(i) / 2
		r := outer
		if i%2 == 1 {
			r = inner
		}
		pts = append(pts, pt{cx + r*math.Cos(a), cy + r*math.Sin(a)})
	}
	return polyline{pts: pts, closed: true}
}

func iconStar() []polyline {
	return []polyline{
		sparkle(46, 52, 32, 9),
		sparkle(78, 24, 14, 4),
	}
}

func iconBall() []polyline {
	return []polyline{
		ellipse(50, 50, 32, 32),
		bow(50, 18, 50, 82, 16),
		bow(50, 18, 50, 82, -16),
	}
}

func iconFlask() []polyline {
	return []polyline{
		line(38, 18, 62, 18),
		polyline{pts: []pt{{43, 18}, {43, 40}, {25, 74}, {75, 74}, {57, 40}, {57, 18}}},
		line(31, 62, 69, 62),
		ellipse(44, 54, 3.5, 3.5),
		ellipse(56, 66, 4.5, 4.5),
	}
}

func iconChart() []polyline {
	return []polyline{
		line(18, 78, 82, 78),
		roundRect(24, 54, 38, 78, 3),
		roundRect(43, 38, 57, 78, 3),
		roundRect(62, 24, 76, 78, 3),
	}
}

func iconPin() []polyline {
	arc := arcPts(50, 42, 24, 24, math.Pi*0.78, math.Pi*2.22)
	return []polyline{
		polyline{pts: append(arc, pt{50, 84})},
		ellipse(50, 40, 9, 9),
	}
}

func iconFilm() []polyline {
	pls := []polyline{
		roundRect(14, 26, 86, 74, 8),
		line(32, 26, 32, 74),
		line(68, 26, 68, 74),
	}
	for _, y := range []float64{35, 50, 65} {
		pls = append(pls,
			roundRect(19, y-5, 27, y+5, 2),
			roundRect(73, y-5, 81, y+5, 2),
		)
	}
	return pls
}
