# 27. A follow delivers the whole strand

Date: 2026-07-28

## Status

Accepted (amends ADR 0019: strikes the Vouch, the Bar and Settling, and
keeps the Follow, the 30-day horizon, and Mute filtering everywhere)

## Context

ADR 0019 built a bar for Strand delivery out of Vouches, on the reasoning
that delivering a whole Strand into a Personal Feed means unasked audio
auto-downloading to a phone, and that some bar is needed.

Talking to the people actually using the station says otherwise. The
Vouch is noise to them: a control they are asked to operate, on behalf of
a filter they did not ask for, over a volume of episodes that does not
need filtering. ADR 0019 already named the tension — at a dozen Users a
count is 0, 1 or 2 — and answered it by making the number mean something
other than a score. The interviews say the number does not need to mean
anything at all, because there is nothing here to curate yet.

Crowd curation is a solution to abundance. This station does not have
abundance. It has four Strands and a handful of makers, and the controls
that actually carry weight are the ones aimed at a person: unfollowing,
Mute, Block, and the admin takedown.

## Decision

**The Vouch is removed.** No entity, no store methods, no routes, no
controls on the Strand Page. Existing Vouch records are orphaned rather
than migrated; nothing reads them.

**The Bar is removed.** A Follow carries no number. Following a Strand
delivers every Aired Episode on it into the follower's Personal Feed,
within the same 30-day horizon, minus the follower's own Episodes and
minus any Owner they have Muted.

**Settling is removed.** It existed only to freeze a Vouch count before
the delivery question was answered once. With no count there is no
question, and a 24-hour hold with nothing behind it is an embargo nobody
asked for. `Delivers` becomes a horizon check:

    func (a Airing) Delivers(now time.Time) bool {
        return now.Sub(a.AiredAt) <= DeliveryHorizon
    }

**What ADR 0019 keeps.** The Follow as the third kind of reference a
Personal Feed holds, distinct from a Share because nobody chose to send
it. The 30-day horizon, so a new follower gets a month rather than the
archive. Unfollowing as the control, with Block and Mute not overloaded
to do that job — and Mute still filtering everywhere a signed-in User
looks, delivery and Strand Page alike.

## Considered Options

- **Keeping the Bar with a default of zero.** Rejected: a control every
  Follow carries and no Follow uses is worse than no control. It would
  also keep `ValidateBar`, the number input, and the retroactive-filter
  semantics alive to serve nobody.
- **Keeping Settling as a delivery embargo.** Rejected: it would be a new
  feature wearing an old feature's code. A day between Airing and
  delivery is defensible — it gives a takedown time to land before audio
  reaches a phone — but that is a decision to take on its own merits,
  with its own name, not by declining to delete something.
- **Keeping the Vouch as display only, with no delivery effect.**
  Rejected: the interviews object to the control, not to its wiring. A
  button that demonstrably does nothing is more noise, not less.
- **Suppressing the one-time backfill.** Rejected: gating delivery on a
  deploy timestamp is code written to be deleted, guarding against a
  handful of episodes arriving at once on a twelve-person station.
- **Replacing the bar with a per-Strand editorial pick.** Deferred, not
  rejected. If this station ever needs curation it will be an admin
  naming what is worth hearing, which is the canon model it already uses
  for Strands (ADR 0017) — not a crowd.

## Consequences

- **A one-time backfill.** Airings inside the horizon that never settled,
  or settled below their followers' Bar, deliver on the first feed poll
  after deploy. At this size that is a handful of episodes arriving at
  once in existing followers' clients. Accepted, not mitigated.
- Aired Episodes now reach followers immediately instead of a day later.
  The grace period a takedown used to have before audio hit a phone is
  gone; the takedown itself is unchanged and still removes the Episode
  from every feed at once, since delivery is computed and not stored.
- A Strand Page has one fewer per-viewer control, but is still per-viewer
  through Mute and follow state, so `private, no-store` on a signed-in
  rendering stands unchanged and ADR 0023's reasoning still holds — its
  argument now rests on those two rather than on vouch state.
- Delivery no longer writes. `settleDue` made a feed poll and a Strand
  Page read into write paths across replicas; both are now pure reads.
  ADR 0016's ride-on-traffic pattern still stands for Beats, which is
  where it started. (No longer true as of ADR 0028: Beats moved to a
  scheduled Tick, and traffic now records liveness rather than firing
  anything. Delivery being a pure read is unaffected.)
- The horizon is now the only bound on what a Follow drags in besides
  Mute, so it carries more weight than it used to and is tested directly.
- Nothing takes the Vouch's place. If the station grows into needing
  curation, this ADR is the record that the crowd-sourced version was
  tried on paper and rejected on evidence.
