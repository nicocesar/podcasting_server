# 28. Traffic says whether, the clock says when

Date: 2026-07-30

## Status

Proposed (reverses ADR 0016's firing mechanism while keeping the
property it was built to protect; gives ADR 0009's resume pass a
dependable driver; prerequisite for ADR 0029)

## Context

ADR 0016 gave Beats a clock made of traffic. There was no cron, no
queue, no ticker, and Cloud Run scales to zero, so the podcast client
became the scheduler: a feed poll fires that user's due Beats, detached
from the request. It rejected a scheduled tick endpoint explicitly, and
the rejection turned on one sentence — the tick would exist "so one
user's daily briefing can be made by a request nobody asked for, when
their podcast client is already making requests every hour."

That argument holds precisely because a Beat is per-User work with a
**natural rider**: the person who benefits is the person generating the
traffic. "A Beat whose owner never polls never fires" is a defensible
thing to say, because it is said to the same person.

ADR 0029 wants work with no rider. A Strand Agent's pass is
station-level: the person who benefits from it running is the Owner who
marked an Episode publishable, and the traffic that would drive it is
anonymous Strand Page browsing by strangers. Hanging one on the other
inverts the fairness — an Episode goes unaired because nobody browsed —
and produces a cold-start spiral, since an empty Strand attracts no
visitors to run the pass that would fill it. Worse, ADR 0018 notes that
anonymous egress is unmetered, unauthenticated and unrated-limited;
wiring a model-calling write path to it makes any crawler a spender.

Three of ADR 0016's four objections to a tick are weaker or absent for
this shape of work. The manual `gcloud` step remains real and small, and
the guarded route turns out to be smaller than ADR 0016 assumed — see
the credential below. The "dev-only equivalent so the feature exists on
a laptop" mostly evaporates when the endpoint *is* the interface and
`curl` is the dev story. The global `due_at` index was specific to
firing Beats and is not what a Strand pass queries.

Separately, ADR 0016's own consequences record two costs that traffic
firing cannot fix. A Beat's Generation "still runs with nothing keeping
the instance awake, so Cloud Run CPU throttling can stall it
mid-flight", and its only rescue is the next poll from a client that may
be hours away. And every replica's traffic can fire the same Beat, a
race ADR 0016 accepts rather than closes.

## Decision

### The Tick

A **Tick** is one authenticated request that does the work no request
asks for. Cloud Scheduler calls `POST /tick` hourly, carrying
`TICK_TOKEN` as a Bearer header. The same route accepts an admin
Session, which is simultaneously the laptop story, the "run a pass now"
button on the admin page, and the driver for ADR 0029's shadow mode.

**A shared secret, not OIDC, and not `ADMIN_TOKEN`.** Verifying a Google
identity token in-process means a JWKS fetch, an audience check and a
service-account allowlist, to guard a route whose worst abuse is making
an Episode arrive slightly early. This codebase already has the right
shape for a credential like this — a constant-time digest compare
against a configured secret — and already has the precedent for
admitting either that header or an admin Session on one route. Reusing
`ADMIN_TOKEN` is separately refused: that secret bootstraps user
provisioning and admin appointment, and a scheduler job has no business
holding it.

**API Keys are refused**, which is why the Session half of the route is
a Session and not the general authenticated-user check. A Tick is
unattended spend on a schedule, which is exactly what ADR 0010 keeps out
of a Generator credential's reach and exactly why ADR 0016 made the
Beats routes session-only. A leaked key that could make the station
spend on a timer would be a privilege escalation, not an over-broad
read.

A Tick claims a **bounded slice** of work and returns 200 promptly. It
does not drain the backlog. Cloud Scheduler enforces a request timeout
measured in minutes and retries non-2xx, and a retry that re-fires
Generations is the expensive failure mode rather than the cheap one, so
being behind must look like success and the next Tick catches up. Stated
as the invariant the handler implements: **a Tick answers non-2xx only
for a failure that happened before any Beat was fired.** Everything
after the first firing — one User's unreadable Beats, a failure writing
the status record — is recorded and answered 200. An hourly Tick that
does at most N units is self-healing; one that tries to finish is one
that eventually retry-storms the TTS bill.

Hourly, not every fifteen minutes. Nothing in ADR 0029 needs sub-hour
latency, a Beat's cadence is measured in days, and a scale-to-zero
service pays a cold start per Tick. The interval is configuration.

The **bounded slice is counted in Beat firings**, defaulting to twenty.
Firing only *starts* a Generation — it claims the Beat, writes the
checkpoint and hands off — so this bounds spend committed to rather than
episodes finished, which is the number ADR 0016's concern was actually
about, and it is the only quantity a request-shaped handler can bound at
all. Twenty is several fully-loaded Users' worth of a whole day's Beats
inside one hour: far above any steady state this station has, and low
enough that a bug making every Beat look due costs twenty Episodes
rather than the corpus.

### Liveness, not a work flag

Traffic no longer fires anything. It records that somebody was here:
`User.LastSeenAt`, written on the feed poll and on the attended pages,
coarsened so a value already inside the hour is not rewritten. The Tick
fires due Beats only for Users seen inside the **Liveness Window**,
which defaults to seven days.

Seven and not two, because the two failures are not the same size. A
window too short stops a person's briefing for having their phone off
over a weekend, silently, with nothing anywhere saying why — a product
failure that presents as the feature simply not working. A window too
long spends a few more days on an account that was going to be noticed
anyway. The generous side of that trade is the cheap side, and the
window is configuration in either direction.

This keeps the property ADR 0016 was actually buying. That ADR's real
concern was named plainly — "unattended spending is the real risk here
and the server has no cost model to reason with" — and traffic firing
answered it by making a User who stops listening stop spending,
automatically. Liveness gating preserves that exactly, while moving
execution somewhere it can survive. Traffic says whether work is
warranted; the clock says when it runs.

A timestamp, not a "work to do" flag. A flag is edge-triggered, and
edge-triggered state is a queue with none of a queue's guarantees: two
polls before one Tick lose a wakeup, a Tick that dies mid-drain has
either cleared work it did not do or will do work twice, and the clear
step has no obviously correct place. A timestamp is level-triggered —
idempotent, no drain, no clear, and re-running a Tick after a crash is
free. It also matches how this system already prefers to hold state:
"nothing is materialized, a Personal Feed remains a view" (ADR 0027). A
flag is materialized intent; a timestamp plus a due check is a view.

### What the heartbeat becomes

The heartbeat shrinks to the liveness write. Firing moves to the Tick.
The **Kick pass stays on the attended surfaces** — the Dashboard and the
Beats page, where a human may be watching a stalled Generation and an
hour is too long to wait — and comes off the feed poll, which returns to
being a read with one coarsened write. The Tick runs the same Kick pass
unconditionally every hour, which is the first dependable driver ADR
0009's checkpoint-and-resume has ever had.

### Station work

The Strand pass (ADR 0029) runs on the Tick and is **not** liveness
gated. Dormancy is a per-User question and only Beats have it. The
Strand pass is guarded by its own candidates: no Episodes marked
publishable since the last pass means no model calls and no spend, with
no extra state to consult.

The Tick is for station-level work and for User work behind a liveness
gate. It is not a general-purpose cron, and the absence of one has been
doing useful design work — ADR 0016's constraint is why nothing in this
system quietly polls. A new periodic job needs its own argument, not a
free ride on this route.

## Considered Options

- **Keep Beats on traffic; clock only the Strand pass.** The cautious
  option, and rejected once liveness gating existed: it leaves two
  firing mechanisms to reason about, and it leaves ADR 0016's stall
  consequence unfixed for exactly the work most likely to stall — an
  unattended Generation with no browser polling it. The spending
  property was the only reason to keep Beats on traffic, and the
  timestamp keeps it.
- **A "work to do" flag set by traffic and drained by the Tick.** The
  shape this decision started as. Rejected for the level-versus-edge
  reasoning above: it is at-least-once delivery hand-rolled on
  Datastore, and it buys nothing a timestamp does not.
- **Riding the Strand pass on Strand Page traffic.** Rejected: the
  cold-start spiral, the inverted fairness of an Owner's Episode
  depending on strangers' browsing, and the connection it would make
  between unmetered anonymous requests and model spend.
- **No liveness gate — the Tick fires every due Beat.** Rejected: it
  discards the one guard ADR 0016 had against unattended spending, on a
  server that still has no cost model, in favour of punctuality nobody
  asked for.
- **An in-process ticker with min-instances=1.** Rejected in ADR 0016
  and still rejected: a warm instance around the clock, and every
  replica ticking makes the firing race worse rather than better.
- **Cloud Tasks or Pub/Sub for the work queue.** Rejected: more
  infrastructure than one route, to gain retry semantics that a
  recurring Tick over level-triggered state already has.
- **An OIDC token from a dedicated service account**, which is how this
  ADR originally read and is the standard answer for Cloud Scheduler.
  Rejected on proportion: a JWKS-backed signature check, an audience
  check and a service-account allowlist, all new code on the auth path,
  to protect a route that starts work the station was going to do
  anyway. A shared secret compared as a digest is the same guarantee
  against the same threat — someone who does not have the secret — and
  the codebase already has that primitive and the route shape that uses
  it. Revisit if the Tick ever grows a capability that a leaked secret
  could turn into something worse than early Episodes.
- **Fifteen-minute Ticks.** Rejected as Beat-shaped thinking applied to
  a daily cadence: four times the cold starts to make a briefing that is
  a day old arrive forty-five minutes sooner.

## Consequences

- A global index appears after all, on liveness and only on liveness.
  ADR 0016 counted an index as a cost of the tick endpoint and it is now
  paid, but not the one it named: a Beat's due time stays a derived
  function of its own fields, never stored and never indexed, because
  the liveness gate means Beats are still examined one owner at a time.
  `LastSeenAt` is the first property in this system carrying an
  inequality filter; a single-property index serves it, so there is
  still no composite index and no `index.yaml`.
- Every User that exists today has no `LastSeenAt`, and an entity
  without the property is absent from its index. The first Tick after
  deploy therefore fires for nobody, and each User rejoins on their next
  poll. The rollout is self-quieting rather than a thundering herd, and
  a newly provisioned account is correctly dormant until somebody
  actually shows up with it.
- A User who stops polling for longer than the Liveness Window stops
  generating, which is ADR 0016's behaviour made explicit and tunable
  rather than emergent. Coming back is one poll: the next Tick sees them
  live again. A Beat that was due throughout is subject to ADR 0016's
  gap rule and fires once with a widened window, not once per missed
  interval.
- Beats are late by up to one Tick rather than up to one poll interval.
  For most clients this is an improvement; for an aggressively polling
  one it is a regression of under an hour on content measured in days.
- Liveness gating stops dormant *accounts*, not zombie *clients*. A
  phone in a drawer still polls, so spend still correlates with polling
  and only ever correlated with listening by proxy. This is no worse
  than ADR 0016 and no better, and must not be described as more.
- The double-fire race narrows but does not close: a Tick that times out
  after firing and is retried can fire again. The existing in-process
  claim still applies, and the backstop is unchanged — duplicated work
  and a `-2` suffixed Slug, never a replaced Episode (ADR 0002).
- The service is never fully cold, since something calls it every hour.
  ADR 0016's observation that "at six in the morning there is very
  likely no process at all" stops being true.
- Deploying now includes a one-time scheduler job and a `TICK_TOKEN`,
  both manual and outside the image-only deploy path. A deployment
  without them degrades badly and silently: Beats never fire, *and*
  a Generation that Cloud Run stalls is never resumed until the process
  next restarts, since the Tick is now the only driver of the resume
  pass outside boot. Nothing else in the system would show this. Hence
  the operator-visible signal on the admin page — when the last Tick
  landed, what it did, and a warning when the answer is "never".
- All unattended spend now originates at one route, so a station-level
  daily ceiling becomes a single check in a single place, against meters
  that already exist. Not decided here, but newly cheap, and the answer
  to ADR 0016's complaint that the server "has no cost model to reason
  with".
