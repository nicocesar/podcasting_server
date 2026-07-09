# 9. Built-in Generation: the agent writes the Script; the server voices and publishes

Date: 2026-07-08

## Status

Accepted

## Context

Users want an Episode from a Topic (plus Target Length, Freshness Window,
and Language) without operating their own Generator. Research and
scriptwriting are delegated to Claude Managed Agents, whose sessions run
in sandboxes with two hard properties: no secret storage (anything handed
to a session lands in Anthropic-side session history) and no
file-download API (a file produced in the sandbox can only leave if the
agent itself uploads it somewhere). Handing each session the user's
publish token — a long-lived credential — was therefore unacceptable, and
"pull the MP3 from the backend" is not a thing the platform offers.

## Decision

The managed agent produces **only text**: the Script (title, summary,
spoken text, sources) as structured output in its final message, fetched
from the session's persisted event history. The server does everything
with side effects: text-to-speech, MP3 assembly, and publication through
the same internal path as the Publishing Contract, so the invariant "all
content enters via the contract" survives with the server acting as the
User's Generator.

TTS lives behind one narrow interface (script chunks in, MP3 out) with
two implementations: `edge-tts-go` primary (free, unofficial Microsoft
endpoint) and Google Cloud TTS as automatic fallback (official, billed
per character, authenticated by the existing service account — no new
secret).

Orchestration is a checkpointed in-process worker: each Generation
persists its stage, agent session id, Script, and TTS progress. Progress
polling from the live progress page keeps the Cloud Run instance alive;
after a restart or redeploy any instance resumes unfinished Generations
from their checkpoints, and user-visible retry restarts from the last
completed stage (a TTS failure never re-pays for research).

The Agent and Environment are provisioned by an idempotent startup
bootstrap: looked up by name, created if missing, and re-versioned
whenever the repo's embedded system prompt differs from the latest remote
version.

## Consequences

- No credential of any kind — publish token, TTS key, upload capability —
  ever leaves the server or transits Anthropic session history.
- The server grows a TTS subsystem; audio character is bounded by TTS
  vendors, and the unofficial edge-tts protocol breaks periodically
  (hence the official fallback behind the shared interface).
- The agent can never iterate on audio (no music, mixing, multi-voice
  production in the sandbox). Doing that later means revisiting this ADR;
  the considered alternative was sandbox rendering with a one-time signed
  GCS upload URL per Generation.
- Sessions can be deleted after publish — the Script is checkpointed
  server-side — minimizing what Anthropic retains.
- Anthropic model tokens dominate per-Episode cost (~$0.10–0.70 on
  Sonnet); TTS and compute are noise. No quotas for now; Generation
  records make adding caps a count query later.
