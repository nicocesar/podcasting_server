# 26. The ElevenLabs balance is on the Spend page, in credits

Date: 2026-07-27

## Status

Accepted

## Context

Spend (ADR 0024) answers "what did this cost" from Anthropic's billed
dollars. ElevenLabs is the other meter running, and nothing surfaced it
anywhere. When the balance ran out, the only sign was a 4xx buried in a
failed Generation's trace — the last place an operator looks and the
last moment the information is useful.

It fails differently from an overspend, too. An Anthropic overrun is a
bill; an empty ElevenLabs balance is an outage with two halves:

- **Music stops completely.** There is no second composer (`internal/music`
  says so out loud), so the ambient template simply cannot run.
- **Speech degrades quietly.** TTS has a fallback chain, so episodes keep
  being voiced — by a different engine, without anyone being told.

The errors were actively misleading. Out-of-credit answers **401 with
`detail.status: "quota_exceeded"`** and no `code` field, so ADR 0025's
error work read it as a generic 401 and advised "check
ELEVENLABS_API_KEY" — sending the operator after a typo in a key that
was fine.

## Decision

The Spend page grows a second meter: **ElevenLabs credits**, read from
`GET /v1/user/subscription`, showing remaining of limit, a bar, the
tier, and when the allowance resets.

**Credits, not dollars.** ElevenLabs reports a character allowance that
both speech and music draw on, and no price. Converting it to currency
would mean a price table — the thing this project has already declined
to keep for Anthropic tokens. The page says so, so nobody adds one
later thinking it was an oversight.

**Warnings, not just a number.** Below 10% remaining
(`LowRemainingFraction`), or on a dead subscription status, the Spend
page and the **admin index** both say so — the index because it is the
page an operator actually passes through. Exhausted gets its own
wording, naming both halves of the failure.

The balance is cached for 5 minutes, failures included, so admin page
loads do not become a round trip to another vendor (or a queue of
timeouts when that vendor is down). A failed read renders as "could not
read the balance", never as a balance of zero: an unknown balance must
not cry wolf.

The error classifier learns the two distinctions this uncovered:

- `quota_exceeded` (however it is spelled — `code`, `status`, or the
  message text) means **top up the plan**, not check the key.
- `missing_permissions` means the key is **valid but unscoped**. The
  first real call to `/v1/user/subscription` failed exactly this way:
  the deployed key had no `user_read` permission.

## Consequences

- An operator sees the balance before it stops the station, on the page
  they already visit.
- **The key needs the `user_read` permission.** Without it the section
  renders an error that says precisely that, and the rest of the server
  is unaffected — speech and music never needed the scope.
- One more vendor call on admin pages, at most every 5 minutes.
- The threshold is a constant, not configuration. If 10% proves wrong
  for the plan's shape, changing it is a one-line edit, and a knob
  nobody tunes is worse than a number in the code.
- Credit spent per Episode is not attributed the way dollars are in
  `pricedEpisodes`. The account total is the question that was actually
  being asked. *(Superseded the same day — see the addendum.)*

## Addendum, same day: per-episode ElevenLabs, in units not dollars

The by-episode table's one Cost column had only ever meant Anthropic,
which was invisible until the balance ran dry: an ambient episode showed
its agent cost of a few cents and nothing about the music that actually
failed. The column is now labelled **Anthropic**, and a second column,
**ElevenLabs**, reports what the episode drew from the allowance —
characters for a voiced episode, composed duration for an ambient one,
blank for the free engines that voice most of them.

Both numbers were already stored per Generation (`TTSCharacters`,
`MusicMillis`, `MusicCalls`); only the display was missing.

The second column is **not** dollars, and cannot honestly be. ElevenLabs
sells a monthly allowance rather than billing per request, so there is
no per-episode invoice to quote. A figure would have to be (plan price ÷
plan credits) × credits used — a price table, and a misleading one: an
allowance costs the same whether it is spent or not, so a quiet month
would report near-zero "cost" against an unchanged bill.
