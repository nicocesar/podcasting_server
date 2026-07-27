# 23. The takedown lives on the public page

Date: 2026-07-27

## Status

Accepted

## Context

ADR 0018 gave the station a takedown: an admin may end another User's
Airing, the Episode survives untouched in its Owner's Personal Feed, and
`AirBarred` stops the Owner simply airing it again. It is deliberately the
smallest power that works.

What ADR 0018 did not decide is where an admin *presses* it.
`handleAdminUnair` has always redirected to `/admin/airings`, a route that
was never registered, so the one admin action over someone else's content
has been answered by the catch-all 404 since the day it was written. The
takedown works — the Airing is deleted and the bar is set before the
redirect — but it has never had a home, and the 404 makes a successful
takedown look like a broken one.

The obvious repair is to build the page the redirect assumes: an admin index
of everything currently on the air, grouped by Strand, a button per row.
Nothing blocks it. `ListAirings` is per-Strand, but `renderAdminStrands`
already walks the canon calling it once per Strand for its counts, so an
overview needs no new store method.

The argument against it is that nobody would read it. A takedown is a
response to something you noticed, and you notice an Episode by encountering
it — on the Strand Page, where it is playable, credited, and sitting next to
its vouches. An admin index is a second inventory of the same Airings, at a
second address, that must be kept in step with the first and that you would
only ever open having already decided to act. The Strand Page is where the
judgement actually happens.

The cost of putting it there is that admin power appears on the Public
Surface. That surface is defined by what it serves with no credential at
all, and the Strand Page is the newest member of it (ADR 0018). A page in
that set rendering a control that only some viewers may use is a different
kind of page from the rest.

## Decision

The takedown renders inline on `/strands/{strand}`, on each aired Episode,
beside the vouch controls that are already viewer-dependent there. No
`/admin/airings` index is built, and the two dead redirects in
`handleAdminUnair` are replaced by the return address the form carries
(ADR 0022), so a takedown lands back on the Strand Page at the Airing it
just ended.

`strandPage` gains an admin flag alongside the `SignedIn` it already
carries. The button renders only for a signed-in admin; `POST
/admin/airings/{airing}/unair` keeps its `adminUser` guard, so the template
is a door and never the lock.

The caching question this raises is already answered. `handleStrandPage`
sets `Cache-Control: public` only for an anonymous viewer and `private,
no-store` for a signed-in one, because the page is *already* per-viewer —
vouch state and follow state differ between readers. An admin is by
definition signed in, so an admin-rendered response is `no-store` and cannot
enter a shared cache. The admin button rides on a distinction the page had
to make anyway.

The Strand Page therefore stays on the Public Surface as the set is defined
— reachable with no credential — while being explicit that what it *renders*
depends on who is reading. The **Public Surface** glossary entry is amended
to say so, because "reachable by anyone" and "identical for everyone" were
previously the same sentence and are no longer.

## Consequences

- The takedown stops 404-ing. That is the bug this fixes, and it would have
  been fixed by either option.
- The control is where the judgement is made: an admin who hears something
  that should not be public acts on the page they heard it on.
- No second inventory of Airings to build, render, or keep in step with the
  canon page.
- There is no single view of everything the station is currently publishing.
  `/strands` lists the Strands and each page lists its Airings, so the
  information is reachable in a few clicks but never on one screen. If
  moderation load ever makes that a real cost, an index can be added later
  without moving this control — the two are not exclusive, and this ADR
  declines to build the index now rather than forbidding it.
- The Public Surface now contains a page whose controls vary by viewer. The
  guard is server-side and unchanged; the risk is that a future edit to the
  Strand Page forgets the page is public and renders something for
  everybody. The `SignedIn` and admin flags are the seam to check.
- `/admin` lists the canon and Spend, and moderation is not one of its
  entries. An earlier sketch of the admin page had a row for it; the row
  is gone rather than pointing at a page that was not built.
