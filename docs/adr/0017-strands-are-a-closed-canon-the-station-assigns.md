# 17. Strands are a closed canon the station assigns, one per Episode

Date: 2026-07-25

## Status

Accepted

## Context

Episodes needed a way to be grouped by subject, so that listeners could
find more of what they liked and eventually follow a subject rather than
a person. The obvious shape is tags — free text, coined by whoever
publishes. Three facts about this system argue against the obvious shape.

Datastore has no full-text search, and every Episode text field is
`noindex`. "Search" over free text is either an equality index on a
normalized string — which is browsing, not searching — or a second
service. The station is invite-only with no open signup, so the corpus is
small: free text at this volume produces singletons and three spellings
of the same idea, never the critical mass that makes tag systems
self-organize. And the grouping was wanted as the basis of a *public*
surface, where a squatted or misspelled label is not merely untidy.

The word also mattered. "Sequence" implies order, which this is not.
"Topic" is already the free-text subject a User submits to start a
Generation. "Station" already means the whole product, in the glossary,
in template copy, and in ADR 0016. "Tag" collides with 36 existing uses —
BCP-47 language tags, ID3 tags, struct tags — in the packages where this
code would live.

## Decision

A **Strand** is the one subject an Episode belongs to, drawn from a fixed
canon the station defines. An Episode has exactly one Strand or none.

The canon is **stored data, not a code constant**, managed from an
admin-only page: id, title, description, cover art. A deployment of this
software that is not ours picks its own strands without forking. A fresh
install with an empty canon is seeded with four — `tech-news`, `music`,
`stories`, `global-news` — dormant until an admin gives each one cover
art, because a podcast feed without `<itunes:image>` is broken in most
clients. Cover uploads reuse `internal/coverart`.

A Strand's **id is immutable**; it addresses the public feed, and
renaming it would silently kill every subscription. Title, description
and art are editable at will. A Strand with Airings is never deleted,
only **retired**: it stops accepting new Airings and leaves discovery,
while its page and feed keep serving the people already subscribed.
Deletion is available only for a Strand that has never been aired into.

**Stranding** is the station reading a finished Episode and placing it:
one schema-constrained `/v1/messages` call on a small model, at
generation time, for every Episode whether or not it will ever be public.
The canon is expressed as an `enum` in the JSON schema, so inventing a
Strand is structurally impossible rather than discouraged by prompt. The
call also admits an explicit "none".

The station proposes and the Owner disposes: the Strand can be set or
changed by the Owner when Airing (ADR 0018), which is also how an Episode
that arrived through the Publishing Contract — no Script, nothing to
read — gets one.

## Considered Options

- **Free text, global namespace.** Rejected: normalization rules we would
  get wrong at least once, squatting on a public surface, and a graveyard
  of one-episode strands at this corpus size. Decisive argument was
  reversibility — fixed to free is deleting a validation, free to fixed
  is a migration across every Episode anyone ever published.
- **A model that may coin new Strands.** Rejected: this is free text with
  extra steps, and a fluent, confident, high-volume tagger is a *worse*
  free-text tagger than a human. Three noir episodes classified
  independently yield `noir`, `hardboiled` and `crime-fiction`, each
  defensible. The pile of Strandless Episodes is better evidence for
  growing the canon than inference is, and it costs nothing to collect.
  Propose-and-approve — the model suggests a name into an admin queue
  when nothing fits — remains available later as an additive change.
- **Many Strands per Episode.** Rejected: at this volume most Episodes
  would land in most Strands and no Strand would be coherent enough to be
  worth following. One to many is additive; many to one forces a winner
  to be picked for every Episode ever classified.
- **The canon as a Go constant.** Rejected once third-party installs were
  considered: a constant means forking to choose your own subjects. The
  admin page also carries the cover-art upload that Strand feeds require
  regardless.
- **Stranding at Airing time rather than at generation.** Rejected: the
  saving is one small call against a bill that already includes a
  managed-agent session and full TTS, the Owner would wait on a model call
  mid-click, and the "none" pile would be biased toward Episodes someone
  already wanted to air.

## Consequences

- `Episode` gains a `Strand`. It is set on private Episodes too, so no
  public endpoint may marshal the `Episode` struct directly — public
  responses need their own shape.
- The classifier's schema is built per call from the live canon. Adding a
  Strand changes the behaviour of every subsequent Stranding with no
  deploy.
- Stranding is a trace stage (`strand.classified`) and its tokens land in
  the existing Generation meters. A Stranding failure never fails a
  Generation: the Episode is simply Strandless, and the Owner picks at
  Airing.
- Republishing a Slug does not re-Strand. Content is replaced; the
  subject stays what it was unless the Owner changes it. Silently moving
  someone's aired Episode to another Strand would be worse than a stale
  classification.
- Retirement means the canon only grows. Ids are never reused.
