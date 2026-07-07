# Podcasting Server

A private, single-user podcast server: it hosts generated, news-like audio
briefings as podcast feeds consumed from a phone podcast client. Episodes
are produced elsewhere; this context only stores, lists, and serves them.

## Language

**Show**:
A single podcast: one RSS feed, one subscription in a podcast client. Has a
title, description, optional Cover Art, and a set of Episodes. Shows are
created explicitly, never as a side effect of publishing.
_Avoid_: feed, channel, topic

**Episode**:
One playable item in a Show: an MP3 plus its metadata (title, description
holding the full generated summary text, publication time, optional
duration). Episodes never expire; they are news-like, so date and
time-of-day are meaningful. Identity within a Show is its Slug.
_Avoid_: item, track, file

**Slug**:
The unique identifier of an Episode within its Show; a free-form string, by
convention `YYYY-MM-DD-<day-part>` with optional suffixes (e.g.
`2026-07-06-morning-update1`). Publishing an existing Slug replaces that
Episode.
_Avoid_: episode id, filename

**Day-part**:
A conventional time-of-day label used in Slugs: `morning`, `noon`,
`evening`, `night`. A naming convention only — the server does not validate
or enumerate it.
_Avoid_: edition, period

**Generator**:
The external actor (a separate, future service) that produces Episodes and
publishes them through the Publishing Contract. Out of scope here except
for the contract it must honor.
_Avoid_: producer, worker, cron job

**Publishing Contract**:
The agreed interface through which the Generator delivers Episodes and is
the only way content enters the system; the server owns all storage.
_Avoid_: upload API, ingestion

**Management API**:
The owner-facing operations for administering Shows: create/update a Show,
set Cover Art, list, and delete Shows or Episodes. Distinct from the
read-side endpoints the podcast client consumes.
_Avoid_: admin panel, backoffice

**Cover Art**:
The single image associated with a Show, displayed by podcast clients.
Part of the Public Surface: served without authentication, because podcast
clients fetch artwork outside their authenticated feed session.
_Avoid_: artwork, thumbnail, logo

**Public Surface**:
The unauthenticated GET endpoints: the landing page, Show Pages, Cover
Art, and static assets. Exposes a Show's identity (title, description,
Cover Art) but never its content — feeds, Episodes, and audio stay behind
credentials. The landing page lists nothing, so Show IDs are not
enumerable.
_Avoid_: public site, anonymous access

**Show Page**:
The public HTML page for one Show (`/shows/{show}`): Cover Art, title,
description, and subscribe instructions with the feed URL. Shows no
Episode data.
_Avoid_: landing page (that is `/`), show site

**Reader**:
The credential/role that may only consume feeds, Episodes, and Cover Art
(the phone's podcast client).
_Avoid_: subscriber, listener account

**Writer**:
The credential/role that may publish and manage content (the Generator and
the owner).
_Avoid_: admin, publisher
