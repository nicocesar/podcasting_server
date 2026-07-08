# 5. One Personal Feed per User replaces multi-Show

Date: 2026-07-07

## Status

Accepted (supersedes the Show Page half of ADR-0003; its Cover Art
rationale stands)

## Context

The server is going multi-user: private per-person feeds, plus sharing of
individual Episodes between Users. The single-user model organized content
into explicitly created Shows (ADR-0001..0003), each its own RSS feed. We
had to decide whether Shows survive into the multi-user world alongside
per-user feeds and sharing.

## Decision

Each User has exactly one **Personal Feed**; Shows disappear as a
user-facing concept. Identity follows: every User holds their own publish
token (used by their Generator) and read credential (used by their podcast
client), replacing the global Writer/Reader roles. An Episode's Owner is
the publish token that created it — ownership is enforced by the
credential, never asserted via an "on behalf of" parameter.

The Public Surface shrinks accordingly: no public page for a Personal Feed
(a feed is a person, and the product is privacy-first). Cover Art stays
unauthenticated at an unguessable per-user URL, for the podcast-client
compatibility reasons ADR-0003 documented.

## Considered Options

- **Keep multi-Show and add an aggregate per-user feed on top.** Rejected:
  three organizing concepts (Show, aggregate feed, shared episodes) before
  the first user arrives. Re-introducing multiple feeds per user later is
  an easy addition; removing a shipped concept is not.

## Consequences

- No user-facing show creation; a feed exists because the User exists.
- Users cannot split content into separate subscriptions (e.g. "morning
  news" vs "language practice") in v1.
- Parked v2 idea: content-extracted **Labels** could form public,
  ownerless show-like aggregations — deliberately not designed yet.
