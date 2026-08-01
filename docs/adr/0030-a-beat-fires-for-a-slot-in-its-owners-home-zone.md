# 30. A Beat fires for a Slot, in its owner's Home Zone

Date: 2026-08-01

## Status

Proposed (extends ADR 0016's Beat with a time of day; depends on ADR
0028's clock, without which none of this is expressible; introduces the
first locale state the station has ever held)

## Context

"A briefing is worth having because it is there in the morning" is ADR
0016's own justification for Beats, and until now the station could not
say which morning, let alone which hour. A Beat had a cadence and nothing
else: fire, wait N days, fire again.

Worse, the cadence was measured from the wrong thing. `DueAt` was
`LastFiredAt.AddDate(0, 0, IntervalDays)` — the instant the last firing
*happened*. A daily Beat caught by the 07:15 Tick was next due at 07:15,
then 07:30, then 07:45: it ratcheted forward by up to one Tick every day
and never back, wandering hours through the clock over a month. Under ADR
0016 this was invisible, because traffic fired Beats at arbitrary moments
anyway and there was no expectation to violate. ADR 0028 gave the station
a dependable clock and turned an unremarkable property into a bug.

So a time of day is not a field this design adds to an existing model. It
is a different model, and the drift is the same problem seen from
underneath.

The station had also never stored anything about where anyone is. Every
timestamp is UTC, slugs are UTC dates, `pubDate` is UTC. Asking for seven
in the morning means introducing locale state, and the interesting
question is not how to store it but what it should do when its owner gets
on a plane.

## Decision

### The Slot, and the Anchor

A **Slot** is the firing a Beat intends: an instant it is *for*, as
distinct from the instant it happens. The **Anchor** (`Beat.AnchorAt`) is
the Slot the Beat last fired for, and the next Slot is measured from it.

`LastFiredAt` stays, unchanged in meaning, because "when was this meant to
run" and "when did it" are different questions and the trace answers both
— a firing records how far behind its Anchor it ran.

Measuring from the Anchor is the whole drift fix. A Beat caught at 07:13
has fired for the 07:00 Slot, so tomorrow is 07:00 again. This applies to
Beats with no time of day as well, which stop ratcheting for the same
reason.

### The time of day, on any cadence

**`Beat.FireAt`** is `"HH:MM"`, or empty for a **loose** Beat — which is
how every Beat behaved before this and remains the default. Any cadence
may carry one. The next Slot is calendar arithmetic in the zone, then the
wall clock re-set to `FireAt`:

    next := anchor.In(zone).AddDate(0, 0, IntervalDays)
    next = time.Date(next.Year(), next.Month(), next.Day(), h, m, 0, 0, zone)

Adding twenty-four hours would land at 08:00 the morning the clocks go
forward. Adding one calendar day and re-setting the clock lands at 07:00,
which is what "every morning at seven" means. The same arithmetic gives a
weekly Beat a stable weekday with no day-of-week field, because seven
calendar days from a Tuesday is a Tuesday.

### The Home Zone

**`User.HomeZone`** is an IANA name — `America/New_York`, never an offset
— so daylight saving is the zone database's problem. It is **home, and
deliberately not current**: it does not follow its owner abroad. A
briefing anchored to seven in the morning arrives at eight in the evening
in Tokyo, and is itself again on the flight home.

Following the traveller was the obvious alternative and is worse in two
ways. A zone change mid-cycle can only deliver two Episodes in one day or
none, and either answer is wrong some of the time — fly east overnight
and today's Slot has already passed, fly west and it has not happened
yet. And the station cannot see a traveller who only uses a podcast
client, so the feature would work for people who happen to open the web
and silently not for anyone else.

The station learns the zone **once**, from a hidden field the generate
form fills in from the browser. That is the whole onboarding: no
dropdown of three hundred and fifty entries, no blocking step. Every
later submission reports a zone too and every one is ignored, which is
what makes travel safe by construction rather than by care.

Changing it is a deliberate act on the Settings page. The Dashboard
offers it — when the browser disagrees with the stored zone *and* the
owner has at least one Anchored Beat, a banner says so and offers the
switch. An offer and never an action.

The route is **session-only**, and not folded into `PUT /me` with the
rest of the profile. Moving a Home Zone re-times every Anchored Beat, and
far enough west today's Slot has not happened yet and fires a second
time. That is unattended spend from a credential ADR 0010 keeps away from
it — the mistake the `recur` checkbox made for a year.

### The grace window

An Anchored Beat fires within **four hours** of its Slot and otherwise
skips it. A morning briefing is allowed to be late — a deploy, a cold
start, an owner outside the Liveness Window until mid-morning — and is
not allowed to arrive at bedtime calling itself the morning news.

Nothing is lost by skipping, which is what makes it affordable: ADR
0016's gap rule widens the next Freshness Window over the ground the
skipped Episode would have covered. Skipping also costs no write. The
Slot is recomputed from the Anchor on every pass rather than stored, so
an abandoned morning simply is not due, and tomorrow comes round on its
own.

A loose Beat has no time of day to be late for and ignores the grace
entirely, exactly as before.

### Zone data

`time/tzdata` is imported blank in `cmd/server`. `distroless/static` does
ship zone data, but that is a property of the base image rather than of
this program, and a Beat's morning should not depend on what the
Dockerfile happens to sit on.

## Considered Options

- **Follow the traveller: update the zone from the browser on every
  visit.** Rejected above — the mid-cycle double-fire or gap, and the
  silent failure for anyone who does not open the web. It is also the
  option that cannot be undone by an inattentive user: a zone that moves
  on its own has no moment at which anybody agreed to it.
- **A fixed UTC offset instead of a zone name.** Rejected: it breaks
  twice a year, silently, and the failure is a briefing arriving an hour
  early every morning until somebody notices and does arithmetic.
- **A time of day only on daily Beats.** Rejected once the Anchor
  arithmetic existed: it covers the case that prompted this and forbids
  "every Friday at bedtime" for no saving, and it would have left the
  drift unfixed for loose Beats — leaving `DueAt` meaning two different
  things depending on a field, which is how bugs get written.
- **An explicit weekday or day-of-month picker.** Rejected as a calendar
  UI: the 31st in February, the fifth Monday, changing the weekday
  mid-cycle. Anchor arithmetic gives a stable weekday for free, and the
  station has four programs.
- **Always fire, however late.** Rejected: an Anchor that fires fourteen
  hours late is not an Anchor, and after a long absence the "morning"
  briefing arrives at nine in the evening.
- **Fire only in the Tick containing the Slot.** Rejected as brittle: one
  deploy at 07:00, or one slow cold start, silently costs the day's
  briefing with nothing anywhere saying why.
- **Storing the next Slot rather than the last one.** Rejected: it makes
  every read a write path, and every change to `FireAt`, the cadence or
  the Home Zone a migration of stored state. Deriving forward from the
  Anchor keeps `Due` a pure function, which is what lets a skipped
  morning cost nothing.
- **Denormalising the zone onto each Beat.** Tempting, since a Beat
  already freezes its whole request (ADR 0011), and it would keep `Due` a
  method with no arguments. Rejected because a Home Zone is one fact
  about a person, and copying it per Beat means a zone change is a fan-out
  write that can half-fail, leaving two of somebody's Beats in different
  countries.

## Consequences

- `Beat.Due` and `Beat.DueAt` take a `*time.Location`, so every caller
  must have resolved the owner first. `FireDue` takes a whole `User`
  rather than an id, which the Tick already had in hand.
- An Anchored Beat whose owner has no Home Zone does not fire, and says
  so on the Beats page. Reachable only by clearing a zone that Anchored
  Beats depend on; reading the Anchor in UTC instead would put somebody's
  morning briefing in the middle of their night, which is worse than a
  stall that explains itself.
- **The migration is a fallback, not a backfill.** A Beat with no Anchor
  inherits `LastFiredAt` and freezes where it had drifted to. No writes,
  no deploy-time pass, no new failure mode — but existing loose Beats do
  change behaviour, acquiring whatever time of day they had wandered to.
  Strictly more predictable than the drift, and correctable by setting a
  real time.
- The Tick interval is now load-bearing in a way it was not. An Anchor is
  honoured to within one Tick, so an hourly Tick makes an "07:00" Beat
  arrive anywhere up to 07:59. Fifteen minutes is what makes the feature
  mean what it says.
- Anchored Beats concentrate on popular hours, so the Beat budget will be
  hit by a herd long before an even spread would reach it. Harmless at
  this size — truncation defers fifteen minutes, well inside the grace —
  and the first thing to revisit if the station grows.
- One morning a year, an Anchor between 02:00 and 03:00 fires an hour
  early: Go resolves a wall time the spring-forward skips backward into
  the old offset. Accepted rather than special-cased, and asserted in a
  test so a change in Go's behaviour is noticed rather than discovered.
- Episode slugs still use the **UTC** date. A 07:00 Anchor in New York is
  11:00 UTC and dates correctly, but an evening Anchor is past midnight
  UTC and takes tomorrow's date in its slug and filename. Pre-existing,
  made more visible by Anchors, and left alone: fixing it means changing
  what a slug means for every Episode already published.
- `cmd/server` grows the embedded zone database, around 450KB of binary.
