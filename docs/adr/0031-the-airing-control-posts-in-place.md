# 31. The airing control posts in place

Date: 2026-08-01

## Status

Accepted

Revisits the "not a fragment swap over `fetch`" passage of ADR 0022, which
deferred this decision until the reload became worth removing.

## Context

Pressing "Put on the air" is a POST answered by a 303, so the browser throws
the page away and fetches it again to change one line of it.

ADR 0022 made that reload land in the right place — the row you touched, on
the page you were reading — and explicitly left the reload itself alone. The
reasoning was that the control is a plain form on purpose, that it degrades
to a `<select>` and a button with no JavaScript at all, and that
content-negotiating a fragment would mean maintaining two paths to keep that
promise. That was the right call for a decision that cost one hidden field.

What has changed is what else is on the page. The Dashboard row now holds a
player, and airing an episode you are listening to stops the audio and loses
your position — the reload is not a flicker any more, it is an interruption
to the one thing the page is for. Everything else on that row already
avoids it: share, unshare and revoke all speak `fetch` and update in place.
Airing is the last control that navigates, and it is the one people press
most.

The maintenance objection was also aimed at the wrong shape. Writing the
row a second time in JavaScript would mean two renderers for one control,
which is exactly the drift the fragment was extracted to prevent. But the
server already renders that fragment from one `airRowView`, for two
different pages. Handing the same fragment back over the wire adds a
*caller*, not a second copy.

## Decision

The airing forms gain htmx attributes alongside the markup they already
have:

```html
<form method="post" action="/me/episodes/{{.Slug}}/air"
      hx-post="/me/episodes/{{.Slug}}/air"
      hx-target="closest .air-row" hx-swap="outerHTML">
```

`handleAir` and `handleUnair` are unchanged in what they do. Only the last
line differs: when the request carries `HX-Request`, they re-read the
Episode and render `airrow` on its own, with no layout around it; otherwise
they send the 303 to the return address, exactly as before.

Four things this commits to.

**The no-JavaScript path stays the contract.** `method` and `action` remain
on the form, the hidden `return` field remains in it, and the row htmx swaps
in carries a return address of its own. Strip the script and the control is
what ADR 0022 left behind, byte for byte. A test presses it both ways.

**The response is a re-read, not a patch.** The handler does not describe
what it just did; it rebuilds the row from the store through the same
`airRowView` the two pages build. What you are left looking at is the truth,
including a change that landed from somewhere else.

**The target is `closest .air-row`, not an id.** Two surfaces render this
fragment and a third may later; none of them has to agree about a name, and
a Dashboard of thirty rows needs no unique ids to swap the right one.

**A refusal comes back inside the control.** Airing can be refused for five
reasons, and a status code is a fine thing to navigate to and a useless
thing to swap — htmx ignores a 4xx, so the press would look like it did
nothing. An htmx press instead gets the control back with the reason on it,
at 200, still usable. A full-page post keeps its status code, because the
browser is leaving anyway and has nowhere to put a message.

htmx is vendored into `cmd/server/static/` and embedded like every other
asset. It loads only on the two pages with an airing control, through the
existing `scripts` block — nobody else pays for it.

## Consequences

- Airing no longer interrupts playback. Putting three episodes on the air
  is three presses, no reloads, and the player keeps running.
- The page fetched to change one row was the whole Dashboard: every
  episode, every strand, the feed. It is now the row.
- One vendored dependency, ~50KB, pinned in the repo rather than fetched
  from a CDN — no third party in the load path of a signed-in page, and no
  build step to keep it there.
- Refusals are now visible in two shapes, and the htmx one answers 200. The
  status code is no longer where a scripted client learns that airing
  failed; the airing routes are session-only (ADR 0010), so no API client
  is reading them.
- The fragment now has a third caller and a reason to stay self-contained
  that is enforced by a test: anything that reaches outside `airRowView`
  renders on two pages and breaks on the wire.
- Every other action in this app still reloads. This is the first
  in-place POST, not a policy that they all become one; the pattern is
  here if the next control earns it.
