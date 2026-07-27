# 21. A Strand remembers how its art was made

Date: 2026-07-27

## Status

Accepted

## Context

ADR 0020 gave the canon generated cover art: a `Spec` of `{Text, Accent,
Icon}` goes in, a stored PNG and its thumbnail come out. What it did not do
is keep the Spec. `store.Strand` holds `Title`, `Description`, `CoverType`,
`Retired`, `CreatedAt` — and nothing about the words on the art.

That has two consequences, and both of them are the admin page lying.

The first is that the art form is write-only. `renderAdminStrands` fills the
row with `ArtText: st.Title` unconditionally, and the accent and icon
`<select>`s always render their empty "from the words" option. So an admin
who draws "night talks" in amber with a moon, then reloads the page, is told
the art says *Night Talks* in a colour derived from the words. The fields do
not describe the strand; they describe what would happen if you pressed the
button again. Every reload silently discards the choice.

The second is that the server cannot tell a generated cover from an uploaded
one. `CoverType` is set either way — that is the point of ADR 0020, that the
two are interchangeable downstream — so nothing in the record distinguishes
"drawn from these words" from "this admin uploaded a file". As long as
wording and art were separate forms with separate buttons, that ignorance
was survivable: pressing *Redraw* was an explicit instruction to replace
whatever was there.

It stops being survivable the moment the four forms on a strand become one.
Four submit buttons per strand — save wording, redraw art, upload art,
retire — is the thing that makes the canon page feel scattered, and the
add-strand form already proves the merge works: it takes title, description,
art words, accent and icon and creates the strand *and* its art in one
submit. But a merged Save on an existing strand has to answer a question the
add form never faces: what does saving do to art that is already there? With
no Spec stored, the only available answers are "always redraw" — which
destroys an uploaded cover the first time somebody fixes a typo in a
description — or "never redraw", which makes the art fields decorative.

## Decision

`store.Strand` gains three fields, all optional and all `noindex`:

```go
ArtText string // words set on the art; empty means "the title"
Accent  string // named colour; empty means "derived from the words"
Icon    string // named icon; empty means "derived from the words"
```

Together they are the **Art Spec**: the record of how a Strand's current
cover was made. Three rules govern it.

**Generating writes the Spec.** Creating a Strand or redrawing its art
stores the `{Text, Accent, Icon}` that produced the PNG. The admin form then
renders from the record instead of from the title, so it shows what is
actually on the art, and the accent and icon selects show the chosen value
rather than resetting to "from the words".

**Uploading clears it.** A cover that came from a file was not made from
words, so the Spec is emptied. This is what finally makes the two cases
distinguishable: `CoverType != "" && ArtText == ""` is an uploaded cover.
The state is derived from what happened rather than asserted by a flag —
there is no way to set "this was uploaded" without an upload having
occurred, and no way for the two to disagree.

**Saving redraws only when the Spec changed.** One Save per strand handles
title, description and art together. The handler compares the submitted
Spec against the stored one and calls `coverart.Generate` only on a
difference. Editing a description leaves the art untouched; editing the
words on the art redraws it.

The empty Spec means exactly what today's behaviour means, so every strand
already in the canon is valid on read with no migration and no backfill. The
first Save on an old strand adopts the Spec its form was showing.

Retire, unretire and delete stay on their own routes. They change a
Strand's standing rather than its words, `delete` is destructive, and
folding them into the same button as a typo fix would be an accident waiting
to happen. On the page they move out of the form entirely, into the strand's
header row.

## Consequences

- The admin form tells the truth on reload. An accent chosen once stays
  chosen, and survives an unrelated edit to the description.
- One button per strand instead of four, with the destructive pair visibly
  elsewhere.
- Fixing a typo in a description no longer redraws a 3000 px PNG. That was
  never expensive — generation is local and free, ~0.4 s and no API call —
  but it did rewrite the stored cover and its thumbnail for no reason.
- An uploaded cover survives an edit to the wording. Before this it could
  not have, under any merged form.
- The Spec is a record of intent, not a cache. It is not re-rendered per
  request and it is not checked against the stored PNG; if the generator's
  constants change, existing art keeps its old look while its Spec describes
  the inputs, exactly as ADR 0020 established.
- Three more fields on a small, admin-written entity that is read on every
  strand page. They are `noindex`, so they cost storage and nothing else.
- An admin can still reach a state the form cannot express — uploading a
  file and then hand-editing nothing — and the honest reading of an empty
  Spec on a strand with a cover is "we did not draw this", which is what the
  disclosure says.
