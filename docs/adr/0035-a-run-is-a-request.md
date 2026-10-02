# 35. A run is a request

Date: 2026-10-02

## Status

Accepted (supersedes the "CPU-always-allocated is the fix" consequence
of ADR 0016; keeps ADR 0028's Tick as the resume driver)

## Context

ADR 0016 recorded that a Generation runs detached from the request that
started it — `Kick` was a goroutine — so Cloud Run's CPU throttling could
stall it mid-flight, and named the remedy: "CPU-always-allocated is the
fix, and it is a service setting rather than a code change." The setting
was flipped on the service, by early August.

It worked, and it cost almost all of this project's Google bill.
Always-allocated CPU is instance-based billing: an instance is paid for
every second it is alive, idle or not. ADR 0028 then put a Tick on the
clock every fifteen minutes, and Cloud Run keeps an idle instance for up
to fifteen minutes after its last request — so the instance never went
away. The September invoice was one vCPU and 512 MiB, around the clock:
4.7 million vCPU-seconds over two months, about $45 a month, to serve a
handful of Generations a day and some feed polls. Request-based billing
for the same work fits inside the free tier.

Neither change was wrong alone. Together they bought a permanent warm
instance, which ADR 0016 and ADR 0028 both explicitly rejected as "a warm
instance around the clock" — it arrived by the side door.

## Decision

**A run happens inside a request.** With request-based billing, Cloud
Run allocates an instance CPU exactly while it has a request in flight.
So `Kick` no longer runs the pipeline itself: it POSTs
`/work/generations/{user}/{id}` to the service's own URL (`WORK_URL`) and
holds the request open, and that route's handler *is* the run. The run's
own 45-minute ceiling sits inside the service's request timeout, raised
to 60 minutes.

**It rides on `TICK_TOKEN`.** The station already has one credential for
its own unattended work, and this route can do strictly less with it
than `/tick` can — it only carries on a Generation someone already
started. No session path: nobody needs to press it.

**A lease in the store, because a request can land anywhere.** The
in-process `running` map stops being enough once a run can land on any
instance — and on a deploy, Cloud Run now lets the old revision finish
its in-flight runs while the new revision's Bootstrap resumes every
Active Generation. Without a cross-instance mark every deploy during a
run would pay for it twice. `Generation.LeaseUntil` is set when a
dispatched run starts, to the run ceiling plus slack; the route refuses
(409) while it is live. A failure clears it so a Retry is immediate. A
run killed outright leaves it to lapse, and the next Tick resumes it.
Two instances reading in the same instant can still both take it: that
is the race `Kick` has always documented, no longer the normal case.

**A failed dispatch is not run locally instead.** A goroutine run is the
stall this ADR exists to remove, and a dispatch that timed out cannot
tell whether its run started — falling back would risk paying twice. The
Generation stays Active and the next Tick re-Kicks it.

**Without `WORK_URL`, nothing changes.** A laptop, or a deployment that
has not set it, runs in a goroutine as before. That is also what makes
rollout safe: the code ships first, the variable second, the billing
mode last.

## Considered Options

- **Keep always-allocated CPU and tick hourly.** Rejected: podcast
  clients poll on their own timers and keep the instance just as warm;
  the saving is unknowable in advance and the warm instance stays.
- **min-instances=0 and accept the stalls**, resuming from checkpoints
  every Tick. Rejected: a voicing stage restarts from its first chunk,
  so stalls re-pay vendors, and episodes arrive hours late.
- **Cloud Tasks.** The standard answer, rejected on its ceiling: an HTTP
  task's dispatch deadline is thirty minutes, under the run's
  forty-five, and it adds a queue and an IAM binding to buy retries the
  Tick already gives.
- **Cloud Run Jobs** for the pipeline. Rejected for now: a second
  deployable, a second image entrypoint and a jobs API client to start
  each run, against one route.

## Consequences

- The station pays for CPU while something is happening and not
  otherwise. The fifteen-minute Tick costs a few milliseconds a pass.
- `WORK_URL` and the 60-minute timeout are manual service settings, like
  `TICK_TOKEN`: an image-only deploy cannot set them. SETUP.md carries
  the steps and the order.
- If the dispatching instance is reaped while the run's request is on a
  different one, the run's client goes with it. Whether Cloud Run keeps
  allocating CPU to a request whose client has gone is not documented;
  the worst case is a stall that the lease and the Tick recover, the
  same recovery ADR 0028 already relies on. At this station's traffic a
  run almost always lands on the instance that dispatched it.
- Dispatch failures are logged, not traced: the trace lives on the
  Generation, and the dispatcher's copy of it is stale by design.
