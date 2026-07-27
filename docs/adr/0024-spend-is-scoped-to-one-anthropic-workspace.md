# 24. Spend is scoped to one Anthropic workspace

Date: 2026-07-27

## Status

Accepted

## Context

The Spend page put two numbers next to each other and implied they were
the same population. They were not.

The by-day bar came from `fetchLedger`, which sums Anthropic's cost
report — **the whole organisation's bill**. The by-episode list came from
`priceGeneration`, which prices only the tokens this server recorded on
its own Generation records. They agree only if this server is the org's
sole spender.

It wasn't. On 11 July the bar read $6.56 and the episodes totalled $1.52
— 77% of the day belonged to no episode. Broken down by description, the
day carried $3.45 of Claude Opus 4.6, a model this server never calls:
`GENERATION_MODEL` is unset on the service, so it runs the code default,
`claude-sonnet-5`. Grouping the usage report by `api_key_id` made it
plain: 3,334,995 tokens on `radio.nicocesar.com-generate`, all Sonnet 5,
and 730,242 tokens on **no API key at all**, all Opus 4.6 — OAuth-
authenticated work billing to the same organisation.

The damage was not only the missing rows. The effective rate is
`day_dollars[kind] / day_tokens[kind]`, and both halves were org-wide.
On a day when another workload ran a pricier model, every per-episode
dollar figure was computed at a blended rate matching no real model.
The episodes weren't just an incomplete subset of the bar; their
individual numbers were wrong.

Three ways to fix it, and the API decides which are possible:

| Report | `group_by` | Filters |
|---|---|---|
| `cost_report` | `description`, `workspace_id` | none accepted |
| `usage_report/messages` | `model`, `api_key_id`, `workspace_id`, `service_tier`, `context_window` | — |

**Dollars have no API-key dimension.** Tokens can be attributed to this
server's key exactly; dollars cannot be attributed to a key at all. That
rules out the obvious fix.

Subtracting doesn't work either — it makes the by-day total honest while
leaving every per-episode figure priced at the blended rate. Grouping
rates by model would fix the blend, but `store.Generation` records
`MusicModel` and not the text model, so there is nothing to join on, and
it still leaves the totals org-wide.

## Decision

Reporting is scoped to **one Anthropic workspace**: the one this server's
API key lives in, named by `ANTHROPIC_WORKSPACE_ID`.

`cost_report` is grouped by `description` **and** `workspace_id` —
description because `costKind` needs `cost_type` and `token_type`, which
group-by-workspace alone returns as null — and `usage_report` by
`workspace_id` alongside whatever it already grouped by. Rows from other
workspaces are dropped from both.

**Both halves of the rate are filtered identically.** That is the point:
the effective rate becomes this workspace's dollars over this workspace's
tokens. Filtering the numerator alone would be worse than filtering
neither, because it would look correct.

An empty `ANTHROPIC_WORKSPACE_ID` keeps every row, which is exactly what
every deployment did before, so nothing breaks by omission. But the page
**says which scope it is showing** — the workspace id when scoped, and a
warning when not. Silently org-wide is precisely how the discrepancy hid
for as long as it did; a number that might be measuring something other
than what its heading claims has to admit it.

The organisation's default workspace reports an empty id, so a configured
scope never matches it by accident.

The raw JSON proxies at `/admin/costs`, `/admin/costs/episodes` and
`/admin/usage` are unchanged: they are documented as verbatim passthrough
of Anthropic's reports, and a passthrough that quietly filtered would be
a different endpoint wearing the same name.

## Consequences

- The two halves of the page describe the same population, so the totals
  reconcile and the per-episode figures are priced at a real rate rather
  than a blend.
- Attribution is exact rather than inferred. No subtraction, no
  model-name heuristics, no "probably Claude Code" reasoning.
- It depends on operational hygiene the code cannot enforce: one
  workspace per workload, and this server's key inside it. Put another
  application's key in the same workspace and the numbers silently go
  wrong again — in exactly the way this ADR describes, with no error.
- `ANTHROPIC_WORKSPACE_ID` lives on the Cloud Run service like every
  other secret and env var, so it survives image-only deploys. A deploy
  cannot set it and cannot clear it.
- The default workspace cannot be named as a scope, because its rows
  carry an empty id and an empty scope means "everything". A deployment
  that wants scoping must create a real workspace — which is the
  arrangement worth having anyway.
- Recording the text model and the agent version on each Generation is
  still worth doing, but for a different question: comparing cost across
  prompt revisions. It is no longer needed for attribution.
