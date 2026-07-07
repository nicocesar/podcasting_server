# 3. Cover Art and Show pages are public; content stays authenticated

Date: 2026-07-07

## Status

Accepted

## Context

Everything except `/healthz` required Basic auth. That broke Cover Art in
practice: podcast clients fetch artwork with a plain image loader, and
whether it attaches the feed's credentials is client-specific (AntennaPod
retries with credentials after a 401 by matching the stored image URL;
other readers may simply fail). Artwork is also the least sensitive thing
we serve.

Separately, a Show needs a human-facing HTML page — somewhere a browser
can land, see what the Show is, and find the feed URL.

## Decision

Introduce a **Public Surface**: a small set of unauthenticated GET
endpoints that expose a Show's identity but none of its content.

- `GET /` — bland landing page; lists nothing.
- `GET /shows/{show}` — the **Show Page**: Cover Art, title, description,
  and the feed URL with subscribe instructions. No Episode titles,
  summaries, or audio.
- `GET /shows/{show}/cover` — Cover Art, at the same URL the feed already
  advertises, so every client's artwork fetch works.
- `GET /static/*` — CSS for the pages.

Feeds, audio, and the entire Writer API remain behind Basic auth.

Pages are `html/template` files under `cmd/server/templates` (a `layout`
plus per-page templates and a `fragments/` directory), embedded with
`go:embed` and passed to the HTTP layer as an `fs.FS`. No HTMX yet — the
pages are static; fragments are where interactivity lands later.

## Consequences

- Artwork works in any podcast client, with no auth quirks.
- A Show's existence, title, description, and Cover Art are effectively
  public information for anyone who guesses its ID (`ai-news` is
  guessable). Nothing sensitive may go in a Show description. Episode
  content remains private.
- The root page enumerates nothing, so unguessable Show IDs stay unlisted.
- Public assets carry `Cache-Control` headers; a replaced cover can take
  up to an hour to propagate to clients.
- The mux catch-all now serves a styled HTML 404, so unmatched paths on
  known-method mismatches report 404 rather than 405.
