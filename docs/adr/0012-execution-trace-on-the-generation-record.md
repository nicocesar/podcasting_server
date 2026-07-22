# 12. Execution trace on the Generation record

Date: 2026-07-22

## Status

Accepted.

## Context

A listener asked for the ElevenLabs voice provider. It returned HTTP 402
(the account is on a free plan, and free plans cannot use shared-library
voices over the API). The chain fell back to edge-tts, the episode
published, and nothing said so. Finding out took `gcloud logging read`
against Cloud Run.

The Generation record kept `TTSAttempts`, a counter — enough to know *a*
fallback happened, not which engine failed, why, or what the listener had
asked for. Everything else notable during a run had the same shape or
worse: the script-rejection path logged nothing at all, and a *successful*
character extraction logged nothing either.

Cloud Logging is a fine place for logs and a bad place for the operational
history of a specific record. It needs shell access, it is retention-
bound, and it cannot be linked to.

## Decision

Every Generation carries an append-only `Trace []TraceEntry`, written by
the runner as the run proceeds and read by an admin at
`GET /admin/generations/{user}/{id}`.

**One call per event.** `Runner.trace` logs (unchanged, so existing
`gcloud` habits keep working) *and* appends to the record. Two mechanisms
that can drift are worse than one that cannot.

**No store write per event.** The runner already checkpoints at every
stage boundary. Because each stage function returns its Generation even on
error and `fail()` persists that value, entries recorded during a doomed
stage still reach storage exactly once. A hard kill mid-research loses
in-memory entries; that is the same durability the rest of the checkpoint
model offers, and is not worth a bespoke flush.

**Every `TraceEntry` field is a scalar, and `Detail` is a JSON string.**
Datastore cannot store a slice or map nested inside a slice-of-structs.
A `map[string]any` here would pass every fsstore test and fail at Put time
in production. The string is the shape the storage layer forces, not a
preference.

**Four levels, and `notice` is the one that earns its keep.** A TTS
fallback that succeeded is not a failure, but an admin must be able to
tell a degraded run from a clean one. Without `notice` those are
indistinguishable, which is the exact gap that motivated this.

**Capped, evicting routine entries first.** 80 entries, truncated fields,
against a 1 MiB entity that `Script` already dominates. When full, the
oldest `info` goes before any `warn`/`error`: a long run emits many
routine events and must not push out the ones worth reading.
`TraceDropped` counts evictions so a truncated trace can say so.

**Admin is now `store.User.Admin`, not `ADMIN_TOKEN`.** A browser cannot
send an `Authorization` header on navigation, so a header-only credential
cannot have a page. Reporting endpoints (costs, usage, trace) moved to a
logged-in admin.

`ADMIN_TOKEN` survives on three tiers rather than being retired:

- **Token only** — `DELETE /admin/users/{user}` and password reset. Each
  is account takeover in a single call, and the token lives in Secret
  Manager and never touches a browser, so a stolen session cookie cannot
  reach them.
- **Token or admin session** — `GET /admin/users`,
  `PUT /admin/users/{user}` and `POST /admin/users/{user}/admin`. Listing
  is on this tier because it is how an operator finds the id to promote,
  and before the first admin exists the token is the only credential
  there is. The token path is what makes a fresh
  deployment bootstrappable: with no users at all, a session-authenticated
  "create the first user" is unreachable by construction. The session path
  is what lets an existing admin appoint a second one from the webapp
  instead of reaching for a shared secret in a terminal. That second case
  was missed in the first cut of this design and added after review.
- **Admin session only** — everything else under `/admin`.

The either-credential middleware uses `s.session`, not `s.auth`:
provisioning and appointment are credential management, which ADR 0010
keeps out of an API Key's reach. A leaked key that could appoint an admin
would be privilege escalation, not merely an over-broad read.

**Trace is `json:"-"`.** It carries raw upstream errors, session ids and
console links, and must never ride along on the owner-facing poll of
`/me/generations/{id}`.

## Consequences

- The motivating incident is now two entries on the record: `tts.fallback`
  (warn, naming the failed engine, the error, and the *requested*
  provider) followed by `tts.selected` (naming what actually voiced it).
- Three things that were invisible are now recorded: script rejections,
  successful character extraction, and slug collisions.
- A new generation template needs no tracing work. Event slugs are
  free-form strings, `Detail` is free-form JSON, and the renderer is
  generic — a template emits `r.trace(&g, "notice", "its.own.event", …)`
  and it displays. Nothing was added to the `Template` registry, per
  ADR 0011.
- Non-admins get 404 from `/admin/*`, not 403: the surface does not
  advertise itself.
- Traces can have holes. `PutGeneration` is a blind whole-entity
  overwrite, so if two replicas ever resume the same Generation (the known
  `Kick` race) one replica's entries are lost. Documented on the field.
- Generations created before this shipped have no trace, and the page says
  so rather than looking empty.
- Owner-visible traces were considered and deferred. The plumbing (a
  whitelist of re-worded, detail-free entries) is sketched but unbuilt;
  showing a listener "your requested voice provider was unavailable" is
  honest but invites a support conversation that is not yet wanted.
