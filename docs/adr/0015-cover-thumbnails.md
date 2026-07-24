# 15. Cover uploads are resized into a full image and a web thumbnail

Date: 2026-07-24

## Status

Accepted

## Context

Each program has one Cover Art image, uploaded via `PUT /me/image` (JPEG or
PNG, 8 MB cap) and stored as raw bytes. The **same bytes** were served
everywhere: the RSS `<itunes:image>` (`/f/{token}/cover`) and every web
card — the dashboard, public Show page, episode page, invite page, the
settings preview, and the player's media-session artwork.

That single copy can't satisfy both consumers. Apple and Spotify *want* a
large square cover (1400–3000 px) for the feed, but the web renders the
cover as a small card. So a big upload is correct for the feed and wasteful
for every page, and nothing capped a monster upload — a 12000 px file went
straight into storage and into the feed, risking rejection.

## Decision

Decode each upload **once, at upload time**, and store **two** derivatives:

- **Full** — the normalized original: proportionally downscaled so its
  longest edge is ≤ 3000 px (Apple's ceiling), source format kept. Smaller
  uploads are stored verbatim. Never upscaled. This is what RSS keeps
  serving.
- **Thumb** — longest edge ≤ 512 px, re-encoded JPEG q82, transparency
  flattened onto white. This is what the web cards load.

Decoding and scaling live in a new `internal/coverart` package
(`Process`), using stdlib `image/jpeg`,`image/png` plus
`golang.org/x/image/draw` (Catmull-Rom). The `store.Store` interface gains
a two-image `SetCover(ctx, userID, contentType, full, thumb)` and an
`OpenCoverThumb`; both `fsstore` (`cover_thumb.jpg`) and `gcpstore`
(`users/{id}/cover_thumb`) implement them.

The thumbnail is addressed by a **query param on the existing handlers**:
`GET …/cover?s=thumb` serves the thumb, the bare URL serves the full image.
A missing thumb **falls back to the full image**, never a 404. Web
templates carry a `?s=thumb` on their `<img src>` and `data-cover`; RSS is
untouched. A one-shot `server backfill-thumbs` subcommand regenerates both
derivatives for covers uploaded before thumbnails existed.

Aspect ratio is deliberately **not** touched — no square-crop, no pad.
Enforcing square covers would silently discard or letterbox uploaded art
and belongs to its own decision (with a UI cropper), not to a resize pass.

## Consequences

- Web pages ship a few-KB thumbnail instead of a multi-MB original; the
  feed still advertises the large image podcast clients expect.
- Storage holds two objects per cover. `DeleteUser` in `gcpstore` already
  sweeps everything under `users/{id}/`, so the thumb is cleaned up with
  no extra code.
- A bad or undecodable image fails loudly on the upload `PUT` (400) instead
  of silently breaking the feed's `<itunes:image>` later.
- Backfill **re-normalizes** existing originals: an old cover above 3000 px
  is downscaled when backfilled. Intended (it also protects the feed), but
  it does rewrite stored originals — the pass is idempotent and safe to
  re-run.
- PNG transparency becomes **white** in the thumbnail (JPEG has no alpha).
  Cover art is effectively never transparent, so this is accepted rather
  than carrying PNG thumbnails.
- Non-square uploads remain non-square (and technically non-compliant),
  just smaller. Square enforcement is left as a follow-up.
- New dependency `golang.org/x/image`, in the same `golang.org/x` family
  already vendored.
