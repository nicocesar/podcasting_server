# 11. Generation Templates: registry in code, one agent per persona, cast on the Episode

Date: 2026-07-19

## Status

Accepted (extends ADR 0009's agent model and ADR 0006's share semantics)

## Context

The built-in Generation (ADR 0009) shipped with one implicit shape: a
news-briefing agent driven by topic + freshness + length. We want other
kinds of episodes — first, children's stories — with different personas
and different form fields, without duplicating the pipeline. Stories also
raise a continuity wish: characters that can return in later episodes.

## Decision

A **Generation Template** is a code-defined registry entry
(`internal/generation/template.go`): program branding for the UI, its own
platform agent (name + system prompt + tools), its form-field flags, and
a task-message builder. The pipeline (script → TTS → publish) stays
shared; `/me/generate` becomes a program chooser and each template gets
`/me/generate/{template}`. The bare `POST /me/generate` remains the news
alias, and a Generation with no `Template` field resolves to news, so
records and clients from before this ADR keep working.

**One platform agent per persona.** Each template's system prompt versions
independently on the platform (ADR 0009's push-on-boot applies per agent).
The news agent's configuration is byte-identical to before, so this change
creates no new version of it.

**Characters live on the canonical Episode.** The stories template can
extract its episode's cast (name + description) onto the Episode record —
never a per-user library — so anyone with the episode in their feed, own
or shared (ADR 0006: shares are references), can pick it as the returning
cast of a new story. The chosen cast is frozen onto the Generation at
submit time, checkpoint-style: a resumed Generation rebuilds the identical
task even if the source episode has since vanished.

**Extraction is not an agent session.** It is one schema-constrained
`/v1/messages` call on a small model (`claude-haiku-4-5`): the script is
already written and there is nothing to research. It runs after publish
when the form's "save characters" checkbox asked for it (non-fatal — the
episode is already out), and on demand from the dashboard's backfill
button. Its tokens fold into the Generation's meters without counting as
a session.

## Considered Options

- **One agent, persona in the task message.** Rejected: prompts stop
  versioning independently, and every persona change re-versions the
  single agent under in-flight sessions of the other kind.
- **User-editable templates (data, not code).** Rejected as premature: a
  template builder means validation, versioning, and a prompt-injection
  surface before a third template even exists.
- **Character memory in a platform memory store.** Rejected: memory
  stores are workspace-scoped (needs per-user store lifecycle), split
  user data across two systems, and put a live platform call in the way
  of rendering a form. The Datastore/fsstore already holds everything
  else the product owns.
- **Per-program feeds.** Rejected for now: ADR 0005's single Personal
  Feed stands; the `Template` field on episodes is the groundwork if a
  kid-device feed ever earns its own ADR.

## Consequences

- Template #3 is one registry entry plus its prompt, a chooser card
  appears automatically, and the form renders from its field flags.
- The submit_episode contract is shared; a template needing different
  submission fields would carry its own tool schema in the registry.
- The stories agent (`podcasting-storyteller`) appears on the platform at
  version 1 on the first boot after this change.
