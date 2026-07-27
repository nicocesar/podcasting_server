# 22. Actions carry their own return address

Date: 2026-07-27

## Status

Accepted

## Context

Every state change in the webapp is a form POST answered by a redirect, and
every one of those redirects goes to a **fixed** destination the handler
picks. `handleAir` and `handleUnair` send the browser to the Episode Page
via `redirectToEpisode`. Vouch and unvouch go to `/strands/{id}`. The six
admin strand actions all go to `/admin/strands`. The beat actions go to
`/me/beats`.

Fixed destinations are wrong in two different ways at once.

Sometimes they are the wrong *page*. Airing is a control on the dashboard —
it sits in a row alongside the player, the share box and the invite button —
but pressing it navigates to the Episode Page, which is a place to listen,
not the place you were working. Getting back is a link that returns you to
the top of the dashboard, so putting three episodes on the air means three
round trips through a page you did not ask for, each ending with a scroll
back down to where you were.

Sometimes they are the wrong *place on the right page*. Retiring the fourth
strand in the canon reloads `/admin/strands` at the top, and the strand you
just touched is somewhere below the fold, indistinguishable from the others.
Vouching for an episode reloads the strand page at the top for the same
reason. The work is correct; you just cannot see that it happened without
hunting for it.

There is also one destination that is not a page at all: `handleAdminUnair`
redirects to `/admin/airings`, and no such route is registered. The takedown
— the one power an admin has over another User's Airing — has always landed
on the catch-all 404.

The underlying mistake is that a handler is being asked a question it cannot
answer. `handleAir` knows an Episode went on the air; it does not and should
not know whether that instruction came from the dashboard, the Episode Page,
or somewhere not yet built.

## Decision

Every action form carries its own return address in a hidden field:

```html
<form method="post" action="/me/episodes/{{.Slug}}/air">
  <input type="hidden" name="return" value="/me#ep-{{.Slug}}">
  ...
```

The handler redirects to `return` when it is present and acceptable, and to
today's fixed destination otherwise. Acceptability is the existing
`localPath` helper, already written for the `?next=` parameter on login: it
keeps the redirect on this site, so a crafted `return` cannot turn a form
into an open redirect.

The anchor is the other half. Rows that an action can act on get a stable
id — `ep-{slug}` on the dashboard, `strand-{id}` on the canon page,
`airing-{id}` on a strand page — so the return address names a row and not
just a page. The browser does the scrolling.

Two things this deliberately is not.

It is **not the `Referer` header**. Referer requires no markup at all, which
is its whole appeal, but it is stripped by privacy settings and by
referrer-policy headers, it cannot carry an anchor, and it makes the return
path invisible: you cannot read a template and know where its button lands.
A hidden field is declared where the button is.

It is **not a fragment swap over `fetch`**. Re-rendering the touched row in
place would avoid the reload entirely, and the dashboard already speaks
`fetch` for share, unshare and revoke. But the air control was written as a
plain form on purpose — the comment above it says so — and it degrades today
to a `<select>` and a button with no JavaScript at all. Content-negotiating
an HTML fragment would mean maintaining both paths to keep that promise, for
a reload this project can afford. If the round trip ever becomes the
bottleneck, the return field is the fallback that a fragment swap would need
anyway, so this decision is a step toward that one rather than away from it.

`redirectToEpisode` is deleted; the Episode Page becomes a place air is
*offered* rather than a place air *sends you*.

## Consequences

- An action returns you to the row you touched, on the page you were
  reading. Airing three episodes is three presses without leaving the
  dashboard.
- POST-redirect-GET is unchanged, and so is the no-JavaScript guarantee. A
  browser with scripting off behaves identically.
- The same control can now live on more than one page without the handler
  knowing about either. This is what lets the air row become a fragment
  shared by the dashboard and the Episode Page.
- `/admin/airings` stops being referenced by a redirect that could never
  work. Where the takedown goes instead is ADR 0023.
- Every action form in the templates gains a line, and the value has to be
  right — a wrong `return` is a wrong landing, and nothing catches it but
  the eye. The fallback keeps a missing one harmless.
- `return` is attacker-controlled input on an authenticated POST. It is
  constrained to a local path by the same helper that guards login, and it
  is worth remembering that any new use of it inherits that requirement.
- Anchors put row ids in the URL bar. They name a slug or a strand id the
  viewer can already see on the page, so nothing leaks.
