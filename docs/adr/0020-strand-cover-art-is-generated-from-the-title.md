# 20. Strand cover art is generated from the title

Date: 2026-07-26

## Status

Accepted

## Context

A Strand is Dormant until it has cover art (ADR 0017): a podcast feed with
no `<itunes:image>` is broken in most clients, so nothing may air into a
Strand until art exists. The only way to get that art was
`POST /admin/strands/{id}/cover` — an upload.

That makes growing the canon a two-tool job. To add "Night Talks", an admin
has to leave the admin page, open a design tool, draw a 3000 px square that
matches the four covers already there, export it, come back and upload it.
The predictable outcome is a canon that stops growing, or one where the
fifth cover does not look like the first four.

The four covers we have share one recipe, and it is a recipe a program can
follow: a cream field, a thin rounded-rectangle keyline, one outlined line
icon in a rounded square, and the title set lowercase in a single accent
colour — each line fitted to the same width, so the words read as one
stacked logotype rather than as a sentence. The reference art is set in
Poppins ExtraBold.

## Decision

The backend draws Strand cover art from words. `internal/coverart` gains
`Generate(Spec) (Processed, error)`, returning the **same two derivatives**
an upload produces (ADR 0015), so everything downstream — storage, the feed,
`?s=thumb`, the web cards — is unchanged and generated art is
interchangeable with uploaded art.

A `Spec` is `{Text, Accent, Icon}`, and **only `Text` is required**:

- **Accent** defaults to one of eight named colours (the design system's
  riso blue and ON AIR red, plus siblings) chosen by hashing the words.
  Deterministic, so redrawing a title never silently changes its colour.
- **Icon** defaults to a keyword match over the words — "global news" finds
  a globe, "tech news" a chip — falling back to a waveform, which is true
  of every strand here. Thirteen icons ship as line work in a shared 0..100
  space, each auto-fitted to its frame by its own bounding box so a set
  drawn by hand still looks like a set.
- **Text** is lowercased and stacked: one or two words get a line each,
  three to six are balanced across at most three lines.

Drawing is done with `golang.org/x/image/vector` and a ~150-line stroker
rather than a 2D graphics dependency: every shape (line, arc, circle,
rounded rect) flattens to a polyline, and strokes are filled quads with a
disc at each vertex for round caps and joins. Type is set with
`golang.org/x/image/font/opentype` from an **embedded Poppins ExtraBold**
(OFL, 151 KB, `internal/coverart/fonts/`). Every dimension is a fraction of
the canvas edge, so the art holds up at 3000 px in a feed and at 512 in a
card.

On the admin canon page:

- **Creating a Strand generates its art**, so a title is enough to make one
  real and it is awake immediately.
- `POST /admin/strands/{id}/cover/generate` redraws it, taking optional
  `art_text`, `accent` and `icon` — the words on the art do not have to be
  the words in the feed.
- `GET /admin/strands/cover/preview` renders at 512 px without storing
  anything, and a small script keeps it pointed at the fields as the admin
  types.
- Upload stays, behind a disclosure. Generated art is the default, not the
  only option.

## Consequences

- The canon can grow from the admin page alone, and everything in it looks
  like it belongs to the same station.
- A title the generator cannot set (more than six words, or words too long
  to set large) **still creates the Strand**. It stays Dormant — the honest
  state for a feed with no image — and the page says so rather than losing
  the admin's typing to a 500.
- Cover art is now a design decision expressed in Go constants. Changing the
  look changes every strand created afterwards but **not** the ones already
  stored: art is generated once and stored, not rendered per request.
- A generated cover is a 3000 px PNG of flat colour, ~140 KB — smaller than
  most uploads, and the thumbnail path is unchanged.
- Rendering is ~0.4 s for a 3000 px square (mostly PNG encode) and ~30 ms
  for a preview. Both are admin-only, so no public path pays for it.
- The embedded font is a new binary asset in the repo, and the reason the
  look is reproducible: a system font would render differently per machine.
  Poppins is OFL, and `OFL.txt` ships beside it.
- The accent palette repeats after eight strands. Deliberate: a ninth
  strand sharing blue with the first is better than a colour nobody chose.
- Icon keyword matching is English and hand-written. A strand it does not
  recognize gets the waveform, and an admin who disagrees picks from the
  list.
