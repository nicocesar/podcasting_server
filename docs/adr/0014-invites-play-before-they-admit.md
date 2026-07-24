# 14. Invites play before they admit

Date: 2026-07-23

## Status

Accepted (amends ADR 0007's 7-day invite TTL; closes the gap ADR 0013
left open; extends ADR 0006's forwarding rule past the membership edge)

## Context

Everything the server can hand someone is all-or-nothing. The Feed Token
is the whole Personal Feed; an Invite is a whole membership. There is no
way to give one person one Episode, which is the most ordinary thing a
user wants to do: *send grandma this one story*. ADR 0013 narrowed what
leaks by accident — signed-in pages no longer carry a capability — but
it explicitly left this open, because the fix is a policy decision, not
a URL change.

The nearest existing thing is an Invite: an unguessable token, minted by
any User, optionally carrying one Episode from their feed, expiring, and
revocable. The invite page already names the Episode it carries — *"…
invited you to join and hear **Sleepy Rabbits**"* — and then requires an
account before a single second of it plays. The capability is already
there; only the toll gate stands in front of it.

What made this worth a decision rather than a patch: an Episode reaching
someone with no account is the first time content leaves the membership
graph. Inside the graph, recipients are Users with Blocks, Mutes, and a
Sharer recorded against every entry (ADR 0006). A Guest has none of that.

## Decision

**An Invite plays before it admits.** The invite page leads with the
Player for the Episode it carries; joining the server is offered
underneath it, not in front of it. An Invite carrying no Episode is
unchanged — it is still a plain door.

**Any holder may mint one**, through the button the Dashboard already
has, on the same rule as ADR 0006: if the Episode is in your feed, you
may pass it on. The minter is recorded (`InviterID`), so every link out
has a name against it, exactly as every Share has a Sharer.

**One clock, thirty days.** Playing and joining expire together. ADR
0007 chose seven days when an Invite was purely a live door into the
system; a link that visibly still plays should still open, and the
alternative is a dead-end for someone who opened the link late. A
Redemption stops the *joining* — an Invite still admits exactly one
User — but not the playing: the link keeps working for its term.

**A Guest sees one Episode and no more than one Episode.** Title,
description, Cover Art, who sent it, the Player, and the join offer.
Deliberately absent: any download link, and anything at all about the
wider feed — no other titles, no feed URL, no subscribe box, no QR.
Guest audio and cover are served inside the invite's own namespace
(`/invites/{token}/…`), which sends `Referrer-Policy: no-referrer` like
every other capability surface (ADR 0013).

**The Owner can revoke any link to their Episode.** `Invite.owner_id`
becomes indexed, adding `ListEpisodeInvites(owner)` — every live link to
anything that Owner published, in one query for the whole Dashboard,
grouped by Slug by the caller. Each Episode row lists its live links —
who minted each one and when it dies — with a per-link kill. Indexed
now, while there is nothing to backfill: a Datastore single-property
index only covers entities written after it exists. `Slug` stays
unindexed; grouping in the handler is cheaper than a second filter.

**Block is untouched.** It keeps its current meaning — incoming only. If
Alice blocks Bob, Bob's existing links to Alice's Episodes keep working
until Alice revokes them from the list above. One lever, no hidden
cascade behind a word people reach for in a bad moment.

## Considered Options

- **A separate Guest Link concept.** Two clean domain terms, each with
  its own lifetime. Rejected: a second entity, a second listing UI, and
  a second revocation path, for two things that are indistinguishable to
  the person receiving one. Reusing the Invite also puts the listening
  moment at the top of the growth loop ADR 0006 named, instead of beside
  it.
- **A separate term implemented on the Invite record with a mode flag.**
  Rejected: it buys the vocabulary cost of two concepts and the
  implementation cost of one, and produces a term nobody can define
  without describing the table.
- **Stateless per-Episode capabilities** — `HMAC(secret, reader ‖ owner ‖
  slug)`, no storage at all. Elegant and briefly the front-runner, until
  Owner revocation: killing one link requires knowing it exists, which
  requires a record, and a derived token plus a revocation list is
  strictly worse than a stored token.
- **Owner-only minting.** The strongest consent story and the simplest
  rule. Rejected on the real case: a co-parent with the story in their
  feed should be able to send it to a grandparent without waiting on
  whoever generated it.
- **Any holder, no Owner override.** The purest reading of open
  forwarding. Rejected: it leaves the Owner with only `delete`, which
  vaporizes the Episode for every legitimate feed to kill one link.
- **Keeping the 7-day TTL for both.** Rejected as a dead-end: a page
  that plays but whose join button has silently expired is worse than a
  slightly wider door.
- **Play once, then dead.** Tightest exposure and the worst support
  burden — it breaks on a double-click, a link scanner, or any client
  that HEADs before it GETs.
- **Offering a download to Guests.** Rejected: it makes leaving the
  membership permanent and quietly defeats the Owner's delete, which is
  the backstop this whole model rests on. Anyone determined can still
  capture audio; we simply do not hand it over.
- **Membership stays the only way in** (no anonymous listening at all).
  Rejected — it is the status quo, and the status quo is what prompted
  the question.

## Consequences

- Content can now leave the membership graph. Bounded to one Episode,
  one link, 30 days, attributable to a named minter, and revocable by
  both that minter and the Owner — but it leaves, and that is new.
- Block and Mute do not reach Guests. They are user-to-user controls and
  a Guest is not a user; per-link revocation is the only lever that
  applies to them.
- The join window triples. An unredeemed Invite is a live door for 30
  days instead of 7, which is a real widening of ADR 0007's risk, taken
  knowingly in exchange for one comprehensible rule.
- Invites minted before this change keep their stored 7-day expiry;
  nothing is backfilled.
- `Invite.owner_id` changes from `noindex` to indexed. Records written
  before the change will not appear in `ListInvitesForEpisode` — an
  acceptable blind spot only because it applies to invites that all
  expire within a week of the deploy.
- The glossary changes: an Invite is no longer "a single-use token that
  admits exactly one new User", and **Guest** joins the language as
  someone who can hear one Episode and nothing else.
