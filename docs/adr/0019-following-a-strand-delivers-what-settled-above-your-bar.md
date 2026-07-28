# 19. Following a Strand delivers what settled above your bar

Date: 2026-07-25

## Status

Accepted, amended by ADR 0027 (which strikes the Vouch, the Bar and
Settling; the Follow, the 30-day horizon and Mute-filters-everywhere
stand as decided here)

Originally: accepted (extends ADR 0006's reference model with a third
kind, and completes the Mute definition it left as a feed-only control)

## Context

ADR 0018 made Aired Episodes public and subscribable as a Strand Feed,
which is a second subscription in a listener's podcast client. A follow
that asks the listener to manage another feed is a link, not a follow —
the Personal Feed is meant to be the whole listening experience.

Delivering a whole Strand into a Personal Feed means unasked audio
auto-downloading to a phone, which is the exact hazard ADR 0006 built
Block and Mute for. Some bar is needed. The obvious bar is a vote count,
and the obvious problem is that this station is invite-only: with a
dozen Users, scores are 0, 1 and 2. Nothing is ever going to be sorted by
that number.

## Decision

### The Vouch, not the vote

A **Vouch** is one signed-in User putting their name to one Aired
Episode. Public and attributed by `User.Title`, like the Airing itself.
An Owner may not Vouch for their own Episode; at this scale that would
make every bar decorative.

It is deliberately not a vote and there is no ranking, no score, no
decay, and no hot/top/new. At a dozen Users a vouch is not a ranking
signal — it is one person saying "this one is worth your time", which
works better at N=12 than at N=12,000. The same integer scales up
untouched if the station ever grows.

### The Follow and the Bar

A **Follow** is a User's standing choice to have a Strand's Aired
Episodes delivered into their Personal Feed, with a **Bar**: the number
of Vouches an Episode must carry to be delivered. 0 is the firehose, 1
means "somebody vouched" and is the default, higher numbers are for a
Strand you find noisy.

This is a **third kind of reference**, alongside a User's own Episodes
and their Shares. It is not a Share: a Share has a Sharer who chose to
send it to you, and nobody chose here. Making the station the Sharer
would be a lie that gives Block something strange to do.

Unfollowing is the control. Block and Mute keep their existing meanings
and are not overloaded — but **Mute now filters everywhere a signed-in
User looks**, delivery and Strand Page alike, rather than only their
feed. Otherwise a followed Strand is a hole straight through it.

### Settling, and the frozen count

An Airing becomes eligible for delivery only once it has **settled** —
24 hours after Airing. At that moment its Vouch count is frozen onto the
Airing as `VouchesAtSettle`: one integer, written once, per Airing rather
than per follower. Delivery is `VouchesAtSettle >= Bar`.

Settling exists because the bar is evaluated after Airing, and RSS has no
way to say "this arrived today but happened on Tuesday". Vouches accrue
for a day, the decision is taken once, and nothing is ever inserted into
a listener's past.

Nothing is materialized. A Personal Feed remains a view: the User's own
Episodes, plus their Shares, plus every settled Airing in a followed
Strand whose frozen count clears that follower's Bar, within a **30-day
horizon**. Following costs zero storage per follower.

Settling needs no scheduler. As with Beats (ADR 0016), the work happens
when a Personal Feed is polled — Airings older than the window are
settled on the way past.

## Considered Options

- **Following as a second subscription.** Rejected: it already exists as
  the Strand Feed, and it makes the listener do the work.
- **Anonymous vouching.** Rejected: real volume, trivially gameable, and
  it would mean building IP heuristics to defend a number that means
  less than the signed-in one.
- **Inserting a late-vouched Episode at its true air date.** Rejected:
  clients sort by `pubDate`, so it lands buried under everything the
  listener has already scrolled past and is never seen.
- **Re-dating a late-vouched Episode to when it crossed the bar.**
  Rejected: Episodes are news-like and "date and time-of-day are
  meaningful". Presenting Tuesday's briefing as Thursday's is a lie about
  the news, not just about the file.
- **Materializing a Delivery record per (follower, Episode).** Rejected:
  the frozen count buys the same stability for one integer per Airing
  instead of one record per follower per Episode, and it would turn a
  GET on the feed into a write path across replicas.
- **Recomputing against the live Vouch count.** Rejected: an un-vouch
  would silently retract an Episode from feeds. A feed that flaps is
  worse than one that is wrong.
- **Following a User as well as a Strand.** Deferred, not rejected. It
  needs a public maker page to be honest about what you are signing up
  for, and Airing's attribution already carries its weight as
  accountability. Additive later: the Follow record grows a second kind.

## Consequences

- An Episode first vouched on day three never reaches a feed. It stays
  browsable and playable on its Strand Page. Correct for news-like
  content; worth revisiting if Story Time ever becomes the main event.
- Vouches keep accruing after settling, but only the frozen count
  decides delivery — so a Strand Page may show four Vouches on an
  Episode that entered feeds at one. Two numbers, one job each.
- Raising a Bar filters retroactively: last week's one-vouch Episodes
  leave the feed. Lowering it backfills within the horizon. The direction
  of surprise matches the direction of the request.
- A new follower gets 30 days of backfill, not the whole archive.
- Vouching is public and attributed, so who vouched is visible. There are
  no follower counts and no notifications: social pressure on a
  twelve-person station changes what people air.
- Owner delete, un-Airing and admin un-Air all remove an Episode from
  every follower's feed at once, since delivery is computed and not
  stored.
