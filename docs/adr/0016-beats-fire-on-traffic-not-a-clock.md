# 16. Beats fire on traffic, not on a clock

Date: 2026-07-24

## Status

Accepted (extends ADR 0009's checkpointed worker; relies on ADR 0002's
republish semantics and ADR 0010's session-only rule)

## Context

Every Episode so far has been hand-started: fill in a form, get one
Episode. A briefing is worth having because it is there in the morning,
and nothing in the product could say "keep covering this."

The record for that is easy. The firing is not. This server has no cron,
no queue, no ticker: durability is a checkpoint on the Generation and a
resume scan at boot (ADR 0009), and Cloud Run scales to zero, so at six
in the morning there is very likely no process at all. Every other part
of the system is driven by a request that is already happening.

The existing pipeline gets away with having no clock by accident: the
progress page polls every 2.5 seconds, so a browser-started Generation
always has a request in flight keeping the instance awake. Anything
unattended loses that.

## Decision

A **Beat** is a Topic a User has the station cover on an ongoing basis: a
frozen copy of a Generation request, plus a cadence, plus a clock. It is
created by a checkbox on the generate form — the same form, because a
Beat is a Generation you asked to keep happening — and managed on
`/me/beats`.

**Traffic is the clock.** There is no scheduler. Three handlers call a
heartbeat for the user they have already resolved: the feed poll
(`GET /f/{token}/feed.xml`), the Dashboard, and the Beats page. The
heartbeat lists that one user's Beats, fires the due ones, and returns.
It runs detached from the request, so no response waits on it, and the
runs it starts are on their own Background context, so a finished request
cannot cancel one.

This makes the podcast client the scheduler. A client polling hourly is
the only heartbeat that arrives while its owner is asleep, which is
exactly when a morning briefing has to be made.

**The cadence is the Freshness Window, where there is one.** A Briefing
fires as often as its window is wide, so consecutive Episodes neither
re-cover nor skip ground, and a Timeless briefing cannot be a Beat at all
— no window, no cadence, no offer. The programs with no window (Story
Time, The Long Room) choose an interval from a list whose day values are
the Freshness Window's, so the domain keeps one vocabulary of durations.

**A gap widens the window rather than multiplying the Episodes.** A daily
Beat whose owner stopped polling for ten days fires once, with the window
stretched to the ten days and capped at a year. Ten firings would be ten
agent sessions researching the same twenty-four hours.

**The heartbeat also resumes.** It re-Kicks the user's Active
Generations. Kick already no-ops on anything this process is running, so
on a warm instance this is one store read; on a fresh one it is what
picks up a run that Cloud Run reclaimed the instance out from under. A
Beat's Generation has no browser polling it, so this is its only rescue.

**Failures pause the Beat, not the user's attention.** Nobody is watching
a progress page to press Retry, so the Beat counts consecutive failures
and pauses itself at three, showing the last error. Transient TTS
fallbacks and agent hiccups (ADR 0012) must not pause a healthy Beat, and
a permanently broken one must not spend forever.

**Five Beats per User**, checked before anything is created. Unattended
spending is the real risk here and the server has no cost model to
reason with; a small number is the honest guard.

**Session-only.** A Beat spends money on its own schedule, so a leaked
API Key must not be able to leave one running — the same reasoning that
put Credential Management behind a session in ADR 0010.

## Considered Options

- **Cloud Scheduler calling an authenticated tick endpoint.** Rejected:
  the standard answer, and here it buys a manual `gcloud` step, an
  OIDC-guarded route, a dev-only equivalent so the feature exists on a
  laptop, and a global `due_at` Datastore index — all so one user's daily
  briefing can be made by a request nobody asked for, when their podcast
  client is already making requests every hour.
- **An in-process ticker with `--no-cpu-throttling` and min-instances=1.**
  Rejected: it pays for a warm instance around the clock, and every
  replica would tick, making the `Kick` race worse rather than better.
- **Catching up fully — one Episode per missed interval.** Rejected: ten
  near-identical Episodes, ten agent sessions and ten TTS bills, all
  queued on one instance.
- **A separate "create a Beat" form.** Rejected: two forms with the same
  fields and the same validation, kept in step by hand.
- **A pointer from the Beat to the Generation that created it.** Rejected
  for the reason ADR 0011 froze the cast onto the Generation: a Beat must
  rebuild an identical request years later, and pruning old Generations
  must not be able to hollow one out.

## Consequences

- A Beat whose owner never polls never fires. The feature is worth
  exactly as much as the listening is real, which is a defensible thing
  for a private podcast to say, but it is not a scheduler and must not be
  described as one.
- An Episode a heartbeat starts lands in the *next* poll, not the one
  that triggered it. A daily briefing is therefore up to one poll
  interval late, every day.
- A Beat's Generation still runs with nothing keeping the instance awake,
  so Cloud Run CPU throttling can stall it mid-flight. The heartbeat's
  resume pass recovers it from its last checkpoint, at the cost of up to
  one poll interval of latency per interruption. If this proves to thrash
  in practice, CPU-always-allocated is the fix, and it is a service
  setting rather than a code change.
- Firing is claimed in-process before the slow work, and the Beat's clock
  is written before the Generation. Two replicas can still both fire the
  same Beat — the race Kick already documents — and the outcome is the
  same: duplicated work and a `-2` suffixed Slug, never a replaced
  Episode (ADR 0002).
- The Freshness Window on a Generation is no longer always one of the
  values the form offers: a catching-up Beat can ask for any width up to
  a year. Anything reading `FreshnessDays` must treat it as a number of
  days, not a menu choice.
- The glossary gains **Beat**, and finally gains **Generation Template**,
  which ADR 0011 introduced without writing down.
