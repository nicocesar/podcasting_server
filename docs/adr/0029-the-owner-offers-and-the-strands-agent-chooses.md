# 29. The Owner offers and the Strand's agent chooses

Date: 2026-07-30

## Status

**Draft — an idea under discussion, not a proposal.** Nothing here is
decided and nothing is being built.

It is written up as an ADR because the shape of the argument is
architectural — it would amend ADR 0018 on who Airs and ADR 0017 on one
Strand per Episode, take up the editorial pick ADR 0027 deferred, and
depend on ADR 0028 for firing. But the question underneath is not: it
changes what the station is *for*, not just how it works. Whether a
maker should have an editor between them and the air is a decision about
the experiment, and this record cannot settle it.

Two things would have to be true before it could be proposed:

- **Volume.** ADR 0027 rejected crowd curation on the evidence that this
  station has no abundance to curate, and that is still true. The
  argument for building anyway is that Beats *manufacture* abundance, so
  the honest test is whether Beat volume is actually about to rise.
- **Appetite.** ADR 0027's interviews found makers object to controls
  they did not ask for. An editor that can decline their work is a much
  larger control than a Vouch button was.

The shadow mode in the Decision below is deliberately the cheapest way to
answer the first without touching the public surface. It remains the
first thing to build if this is ever taken up.

## Context

A Strand today is a bucket. The canon has a title, a description and
cover art (ADR 0017), and an Episode lands in one because a small model
read it at generation time and an Owner confirmed or overrode the guess
when Airing (ADR 0018). Nothing about a Strand has taste. Two Episodes
sitting on `music` share a classification, not an editorial idea, and a
listener following the Strand gets whatever cleared the classifier in
reverse-chronological order.

ADR 0027 removed the Vouch after interviews, on the grounds that "crowd
curation is a solution to abundance" and this station has none. It left
one door open: "replacing the bar with a per-Strand editorial pick —
deferred, not rejected. If this station ever needs curation it will be
an admin naming what is worth hearing, which is the canon model it
already uses for Strands."

The volume argument has a wrinkle ADR 0027 did not have to weigh. A
station that generates on Beats *manufactures* abundance in a way a
station of hand-started Episodes does not. The question is not whether
there is abundance today — there is not — but whether the thing that
creates it is about to be turned up.

The obvious design is to hand Airing to a per-Strand agent outright:
each Strand gets a personality and a policy, sees recent Episodes, and
decides what goes on air. That inverts the wrong thing. Airing today
carries two separate acts in one click — the Owner consenting to be
public under their own name, and the placement of the Episode on a
Strand. ADR 0018 built the first deliberately and said so:

> There is deliberately no Beat-level or account-level standing setting:
> Beats fire on feed polls, so a standing setting would mean an
> unattended bot publishing unreviewed generated audio to the open
> internet under a User's name.

An Aired Episode is attributed to its Owner by `User.Title`, and ADR
0018 calls that "the cheapest accountability there is" — which stops
being true the moment a machine assigns it.

## Decision

### Publishable is consent; Airing is editorial

The Owner's gesture changes meaning. **Publishable** is an Owner
declaring that an Episode may be public under their name. It names no
Strand and guarantees no audience; it puts the Episode in the pool. It
is safe as a standing per-Beat setting, which per-Episode Airing was
not, because what is being agreed to in advance is publishability rather
than a specific public page.

**Airing stays what ADR 0018 defined** — the record that makes an
Episode publicly audible — but a Strand Agent may now create one, and
only for Episodes already marked Publishable. The invariant ADR 0018
was protecting survives intact: a human said yes before strangers could
hear it. What the human no longer does is pick the shelf.

Manual Airing by the Owner remains. It is strictly more consent than
Publishable, it is the path for an Episode that arrived through the
Publishing Contract, and it is what a Strand with no Agent still has.

### The Strand Agent

A **Strand Agent** is a principal, not a background function. It holds
an API Key under ADR 0010's split, scoped to one Strand, granting
exactly two capabilities: read candidate Dossiers, and create an Airing
on its own Strand. It cannot read private Episodes, cannot Air on
another Strand, and cannot widen itself — Credential Management stays
session-only. Airing is strictly more dangerous than publishing, so this
is its own capability rather than a ride on the existing Publishing
Contract grant.

A Strand gains an **Editorial Policy**: admin-managed prose describing
what this Strand takes and what it turns away. It is a **second field,
not the Description**. The Description is listener-facing copy that
ships in the RSS channel and must not drift into prompt-speak, and the
Policy is an instruction that must stay under the station's control —
ADR 0017 already holds that the canon is the station's, and a
user-editable Policy would be a prompt-injection surface aimed at the
publishing path.

### The Dossier

Scoring every Episode against every Strand with the script in hand is
`episodes × strands` inference that grows every time an admin adds to
the canon. Instead, one **Dossier** is written per Episode at generation
time, beside the existing `strand.classified` stage and on the same
small model: language, duration, content type, a short synopsis, and
whatever else policies turn out to ask about.

Agents read Dossiers, never audio and never scripts. An editor picking
from a slush pile reads coverage, not manuscripts — and it is what makes
a public candidate URL unnecessary, since the Agent has no reason to
want the bytes.

Hard criteria live in code, not in the prompt. Length, language and
content type are a pre-filter over the candidate query, so the model is
only ever asked the taste question, and only about Episodes that already
qualify.

### The Consideration

Every decision is a record: **Consideration**, keyed by `(Strand,
Episode)`, carrying the outcome — aired, declined, blocked — and a
one-line reason.

    Consideration:  Strand · OwnerID · Slug · Outcome
                    · Reason · At

It is doing four jobs at once. It is the idempotency key, so a Tick
retried or overlapping (ADR 0028) cannot mint two Airings for one
Episode on one Strand. It is the audit trail for an Owner asking why
their Episode was not picked up. It records an admin un-Air as
`blocked`, so the pass does not cheerfully re-Air something that was
taken down. And it makes the time window a query bound rather than a
rule: "the last X hours or N Episodes" decides what a pass looks at, not
what it is permitted to have an opinion about, so a Strand added next
month can still reach into the back catalogue.

### Overlap is allowed; exclusivity is a Strand's own policy

An Episode may be Aired on more than one Strand. `Airing` is already a
record carrying its own Strand rather than a flag on the Episode (ADR
0018), so nothing structural forbids it; only `GetAiringByEpisode`
assumes a single live Airing and must return a set.

ADR 0017's "exactly one Strand or none" narrows to describe the
classifier's placement, which stays one and is now a hint on the Dossier
rather than a fact about the Episode. The argument ADR 0017 made — at
this volume a classifier would smear every Episode across every Strand —
is about a classifier, and weakens when each Strand is applying its own
editorial policy. This is the one-way door in the decision: one to many
is additive, many back to one would be a migration across everything
ever Aired.

The duplicate a listener would see is a **rendering** problem, and the
Personal Feed is already a computed view: it dedupes by `(Owner, Slug)`.
Two Strand Pages showing the same Episode is not a defect; it is what a
good pick looks like from two angles.

If a Strand wants exclusivity it says so **in its own Editorial Policy**
— "not already Aired elsewhere" — which is one clause and one pre-filter
query. No flag on the Owner's side, no arbitration between Strands.

### Shadow first

The first deployment airs nothing. Agents run on the Tick, write
Considerations with reasons, and create no Airings. Comparing a week of
that against what Owners actually Aired by hand answers the three open
questions — whether the taste is any good, what a pass costs, and
whether a Strand's Policy is a coherent editorial idea or a vibe — before
the public surface is touched. Then one Strand goes live.

## Considered Options

- **The Agent Airs whatever it likes, with no Owner gesture.** The
  original shape, and rejected: it publishes audio under a User's name
  that the User never offered, which is a consent defect rather than a
  policy tradeoff, and it hollows out the attribution ADR 0018 relies on
  for accountability.
- **The Owner keeps placing; the Agent only ranks or highlights.**
  Rejected: an ordering signal over a handful of Episodes is the Vouch
  again, wearing a model instead of a crowd, and ADR 0027 is the record
  of how that lands.
- **An "exclusive content" checkbox on the Owner's side.** Rejected: it
  asks the maker to arbitrate between editorial policies they have never
  read, and every control on the publish path is a tax on the person
  this station exists to serve. ADR 0027 is recent evidence about
  controls nobody asked for.
- **First-come-first-served for a contested exclusive.** Rejected:
  there is no meaningful "first". The winner would be decided by canon
  iteration order and would be the same Strand every time — a hardcoded
  priority laundered through arrival time. If priority is wanted it
  should be named.
- **A budget or auction between Strands.** Rejected as a scarcity
  mechanism for a system with no scarcity: four Strands means every
  auction has one bidder, and it would add a currency, a refill rule and
  a story for a Strand that runs dry mid-week, all to arbitrate
  contention that has not happened yet.
- **A public, no-auth URL per publishable Episode for Agents to read.**
  Rejected: it recreates the unlisted-public tier ADR 0018 ruled out —
  "public means aired; there is no unlisted-link form of public" — and
  makes the whole pool readable *before* any decision is taken. It also
  adds a second path to episode bytes, when ADR 0018 holds that the
  public audio handler is the most dangerous code in the feature. The
  decoupling it was reaching for is delivered by a scoped API Key, and
  the Dossier means no Agent ever needs the audio.
- **Every Agent reads every candidate's full script.** Rejected on cost
  and on shape: it multiplies inference by the size of the canon, and
  penalises exactly the growth ADR 0017 designed the canon to allow.
- **Keeping strictly one Strand per Episode and arbitrating.** Rejected
  because every arbitration mechanism above was worse than the duplicate
  it prevents, and the duplicate is fixed by deduping a view.
- **Waiting until there is real abundance.** The strongest objection,
  and the reason for shadow mode rather than for deferral. At four
  Strands and a handful of makers an Agent declining one Episode in four
  is theatre with a token bill. Shadow mode costs a week and answers
  whether the editorial idea is real before any of it is load-bearing.

## Consequences

- ADR 0017's one-Strand-per-Episode no longer holds for the public
  surface. `Episode.Strand` becomes the classifier's hint;
  `GetAiringByEpisode` returns a set, and every caller assuming one live
  Airing — the Owner's controls, the un-Air path, the Episode page —
  must be found and changed. Un-Airing is per-Airing: taking an Episode
  off `chillout` leaves it on `music`.
- The Personal Feed gains a dedupe by `(Owner, Slug)`, which it did not
  need when an Episode could reach it from at most one Strand.
- Publishable is a new standing consent, and standing consents age
  badly. It must be visible and revocable in one place, and revoking it
  must be defined against Airings that already exist — the proposal is
  that it stops future Airings and leaves current ones to the existing
  un-Air control, so revocation is never silently retroactive.
- An admin un-Air must write a `blocked` Consideration in the same
  transaction as deleting the Airing, or the next pass undoes the
  takedown. This is the sharpest failure mode in the design and deserves
  a test that asserts it directly.
- Episode text is untrusted input to the editorial call. A synopsis in a
  Dossier can attempt to instruct the Agent, and the Agent holds a
  credential that can publish. Structured output over a fixed decision
  schema is the containment, exactly as ADR 0017 used an `enum` to make
  inventing a Strand structurally impossible.
- Cost moves from one classifier call per Episode to one classifier call
  plus one Dossier plus one editorial call per surviving candidate per
  Strand. The pre-filter is what keeps the last term small, and shadow
  mode is what measures it before it is real.
- A Strand with no Agent or no Policy simply never Airs anything on its
  own, and depends on manual Airing. Nothing breaks; the canon can be
  adopted a Strand at a time.
- Owners can be told why an Episode was passed over, which is new and
  is a support surface as much as a feature. There is no appeal path in
  this release.
- The Airing rate is now set by machines, and ADR 0018's note that one
  prolific Owner can flood a Strand becomes a note that one enthusiastic
  Policy can. The Consideration record is where a per-pass Airing cap
  would go if it is needed.
