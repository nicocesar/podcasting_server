# 4. The server estimates episode duration when the publisher omits it

Date: 2026-07-07

## Status

Accepted

## Context

The Publishing Contract trusted `metadata.duration_seconds` verbatim, and
the feed omitted `<itunes:duration>` when it was missing. In practice this
put a correctness burden on the publisher: stale or copy-pasted metadata
produced feeds claiming "3 seconds" for a 2-minute file, which podcast
clients display until the download reveals the truth. The Generator should
be as dumb as possible.

## Decision

When `duration_seconds` is absent (or 0) in a publish request, the server
estimates it by walking the uploaded MP3's MPEG frames and summing their
durations — exact for CBR and VBR, no audio decoding. An explicit
`duration_seconds` always overrides the estimate.

Implementation: `internal/audio.MP3Duration` using
`github.com/tcolgate/mp3` (tiny pure-Go frame walker, zero transitive
dependencies). The walk is panic-guarded because it parses external input.

Estimation failure is non-fatal: the episode publishes without a
`<itunes:duration>` tag (legal RSS; clients show the duration after
download). A publisher error should never block a briefing from going out.

## Consequences

- This carves one exception into ADR 0001's "the server never inspects
  audio": a single derived field, computed at publish time, before the
  bytes reach storage.
- Publish requests read the audio twice (frame walk, then upload) — fine
  at news-briefing sizes, all in-memory or from the multipart temp file.
- A zero-duration episode can no longer be expressed; sending
  `duration_seconds: 0` means "estimate it". Not a real loss.
- Non-MP3 uploads still publish (the contract already assumes MP3), just
  without a duration.
