# 6. Shares are references to one canonical Episode; any recipient may share onward

Date: 2026-07-07

## Status

Accepted

## Context

Users can share individual Episodes with other Users, and a Personal Feed
mixes own and shared content. We had to decide what a shared Episode *is*
(copy vs reference), how much consent stands between a Share and the
recipient's feed (which podcast clients auto-download to a phone), and
whether recipients can share onward.

## Decision

An Episode exists once, under its Owner; a Share places a **reference**
into the recipient's Personal Feed, addressed by username and landing
immediately — no inbox or approval step. The Owner's republish (ADR-0002
replace) propagates to every referencing feed, and the Owner's delete
removes the Episode from every feed, with no tombstone.

Any User with the Episode in their feed may Share it onward. Provenance is
therefore first-class: each feed entry records an immutable **Owner**
(first publisher) and a **Sharer** (who placed it in *this* feed), which
can differ. Recipient controls compensate for the zero-consent share path:
remove any shared Episode from my feed, **Block** a Sharer (their Shares
never reach me again), and **Mute** an Owner (their Episodes never appear,
whoever forwards them).

## Considered Options

- **Copy at share time.** Rejected: duplicates storage per share, and
  recipients keep content the Owner regrets sharing — wrong default for a
  privacy-first product.
- **Owner-only sharing (no forwarding).** Rejected: forwarding is the
  growth loop of the product, and the Owner keeps the ultimate control —
  delete vaporizes the Episode from every feed it was forwarded to.
- **Accept-inbox before a Share lands.** Rejected for v1: friction on the
  exact loop meant to be sticky, and podcast clients offer no surface for
  pending shares. Revisit if the user base opens beyond trusted circles.

## Consequences

- The RSS GUID derives from (Owner, Slug), so a User's own
  `2026-07-07-morning` and a shared one coexist in one feed; ADR-0002
  semantics hold per Owner.
- An Owner's delete silently vanishes Episodes from recipients' feeds —
  acceptable; clients that already downloaded keep the audio regardless.
- Block/Mute machinery is required at v1, not deferred: direct-to-feed
  sharing plus auto-downloading clients means unwanted audio lands on
  phones otherwise.
