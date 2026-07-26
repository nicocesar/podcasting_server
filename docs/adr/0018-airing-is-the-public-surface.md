# 18. Airing an Episode on its Strand is the only public surface

Date: 2026-07-25

## Status

Accepted (narrows ADR 0003: the landing page now lists Strands, though
Users and Personal Feeds remain unenumerable)

## Context

Nothing in this system has ever been readable without a capability. ADR
0003 built a Public Surface that "exposes a Show's identity but none of
its content" and noted approvingly that "the root page enumerates
nothing". ADR 0008 put every byte of audio inside the Feed Token
namespace. Making an Episode publicly audible reverses a decision taken
deliberately, twice, and so has to be taken just as deliberately.

Two readings of "public" were on the table and they are not the same
thing: reachable by anyone holding a link, or listed somewhere anyone can
browse. The first already exists — an Invite plays its Episode for anyone
holding it, unlimited, with no account (ADR 0014).

## Decision

**Airing** is the Owner's act of putting one of their Episodes on its
Strand, where anyone may hear it with no capability at all. Public means
aired; there is no unlisted-link form of public, because that is what a
Share and an Invite already are. An Episode with no Strand cannot be
Aired — there would be nowhere for it to go — which makes the canon
load-bearing rather than decorative.

Airing is **per-Episode and by hand**. There is deliberately no
Beat-level or account-level standing setting: Beats fire on feed polls
(ADR 0016), so a standing setting would mean an unattended bot publishing
unreviewed generated audio to the open internet under a User's name. The
invariant worth keeping while this surface is young is that a human heard
it before strangers could. A standing setting is additive later.

An Aired Episode is **attributed** to its Owner by their feed title
(`User.Title`), not by their username. Attribution is a prerequisite for
following a person at all, and retrofitting it would mean attributing old
Episodes nobody consented to attributing. It is also the cheapest
accountability there is. There is no public per-User page in this
release.

An **Airing is a record**, not a flag on the Episode:

    Airing:  ID (key, opaque) · OwnerID · Slug · Strand [indexed]
             · AiredAt [indexed]

The record exists so that the unsafe query is unwritable. Public reads
query Airings and then load the Episode by `(OwnerID, Slug)`; a private
Episode has no Airing, so it is not in the result set to be filtered out
— it is not in the table. A `public bool` on `Episode` would put the
whole archive one forgotten predicate away from being served. It also
mirrors `Share`, which is already a record meaning "this Episode appears
in that place, placed by someone, at some time".

The **ID is opaque but not secret**. It exists because Episode identity
is `(Owner, Slug)` and a Slug is unique only within its Owner's feed —
with slugs conventionally `YYYY-MM-DD-‹day-part›`, collisions across
Owners are the norm, so a public URL needs something else. Keeping the
username out of that URL is what preserves the property ADR 0006 relies
on: usernames are not enumerable, and so the zero-consent Share path
cannot be aimed at strangers. Guessing an Airing id grants nothing —
every Airing is listed on its Strand page — so ten random base32
characters, collision-checked, is enough. Un-Airing deletes the record;
re-Airing mints a new id, so links killed by an un-Air stay dead.

A Strand is served two ways from one query: a **Strand Page** (HTML,
with the Player) and a **Strand Feed** (RSS, no token, subscribable in
any podcast client). Both are reverse-chronological, the feed capped at
100 items. `itunes:block` stays set: a Strand Feed is reachable by URL
but not listed in Apple or Spotify directories, which is one attribute to
flip once there is a moderation story. Aired Episodes show **Strand**
cover art, not the Owner's Personal Feed art — which is behind a Feed
Token anyway, and is the wrong identity for the page.

The **admin may un-air any Airing** and may do nothing else: the Episode
survives untouched in its Owner's feed. An admin un-air sets a bar that
only an admin can clear, without which the Owner simply airs it again.
The Owner is told in plain language that the station removed it. There is
no user-facing report queue in this release.

## Considered Options

- **Public as an unlisted link.** Rejected: a second, weaker form of
  something Invites already do better, and it would leave "public"
  meaning two overlapping things.
- **Anonymous Airing.** Rejected: it kills the follow-a-person feature
  before it starts, and unattributed public audio is a moderation problem
  with nobody on the hook.
- **Username in the public URL.** Rejected: it publishes a directory of
  valid Share addresses. Shares land immediately with no approval and
  podcast clients auto-download, so unsolicited audio would arrive on
  strangers' phones with Block and Mute only able to react after the
  first one. ADR 0006 deferred the accept-inbox "until the user base
  opens beyond trusted circles"; an opaque id keeps that revisit a
  deliberate choice rather than one forced by a URL format.
- **Admin deletes the Episode on takedown.** Rejected as strictly more
  power than any takedown needs. What has to stop is the publicness.
- **A search box.** Rejected for now: Datastore offers no full-text
  query, and with four Strands and tens of Aired Episodes, browsing
  returns better results than searching. Revisit when one Strand holds
  more than a page.

## Consequences

- The public audio handler is the most dangerous code in this feature and
  must be new code, not an existing handler with auth relaxed. It takes
  an Airing id and nothing else; were it to accept `(owner, slug)` it
  would serve every private Episode to anyone who guessed a date.
- ADR 0003's "the root page enumerates nothing" no longer holds. Strands
  are enumerable; Users and Personal Feeds still are not.
- Un-Airing is best effort. Podcast clients cache and people download;
  what has been fetched is gone. The UI must not imply retraction.
- An Owner's delete removes the Airing along with everything else (ADR
  0006, no tombstone). An Owner's republish silently changes what the
  public hears, since an Airing refers to the Episode and not to a
  snapshot of it.
- Anonymous egress is unmetered and unauthenticated. Public pages and
  audio carry `Cache-Control`; a rate limiter is not in this release.
- One prolific Owner can flood a Strand, since ordering is purely
  chronological. This is the hole votes are meant to fill.
