---
name: news-briefing
description: Generate a spoken news-briefing MP3 for a topic and publish it as a podcast Episode. Args: "<topic>" <duration> [freshness]
disable-model-invocation: true
---

# News Briefing

Produce a news briefing on a topic — research, script, synthesize, publish —
ending as an Episode in the publishing user's Personal Feed via the
Publishing Contract (README.md, "The Publishing Contract").

Arguments: `"<topic>" <duration> [freshness]`
- `duration` — target length, e.g. `10m`.
- `freshness` — the window: how far back stories may date, e.g. `36h`, `3d`, `2w`. Default `1w`.

## 1. Preflight

- `PODCAST_HOST` and `PODCAST_PUBLISH_CREDENTIALS` (`user:publish-token`,
  from provisioning) must be set in the environment; stop and name
  whichever is missing.
- `edge-tts` and `ffprobe` must be on PATH (`pipx install edge-tts`, or run
  via `uvx edge-tts`).
- The credentials must work:
  `curl -sfu "$PODCAST_PUBLISH_CREDENTIALS" "$PODCAST_HOST/me/episodes"`.
  On 401/403, stop and report the credentials as the problem — the target
  feed is implied by them; there is nothing else to configure.

Done when env, tools, and credentials all check out. Keep the episode
list; step 5 uses it.

## 2. Research

The window is now minus freshness; the word budget is duration in minutes
× 165 (the en-US-AndrewNeural voice speaks ~165 wpm).

WebSearch the topic from several angles. For each candidate story record
headline, source, URL, publication date, and the facts worth airing — the
URLs feed the show notes in step 5.

Freshness rule: a date the search result states clearly is trusted; a
missing or ambiguous date is settled by WebFetching the article; a story
whose date can't be confirmed inside the window is dropped.

Sparse window: if the kept stories can't honestly fill the budget, don't
pad and don't widen the window — write a shorter briefing and tell the
user how far short it fell and why.

Done when every kept story has a confirmed in-window date and together they
carry enough material to fill the word budget — or the window is known to
be sparse.

## 3. Script

One English-speaking news anchor. Intro gives the date and topic in a
sentence or two; stories follow in order of importance, each
self-contained; a one-line outro closes.

Write for the ear: continuous prose only — no headings, bullets, or URLs;
acronyms expanded on first mention; numbers, symbols, and dates written as
they are spoken.

Target the word budget. Save the script to the scratchpad as `script.txt`.

Done when the script is within ±10% of the word budget (or of the sparse
target from step 2) and reads aloud cleanly.

## 4. Synthesize

```sh
edge-tts -v en-US-AndrewNeural -f script.txt --write-media briefing.mp3 \
  || { rm -f briefing.mp3; \
       edge-tts -v en-US-AndrewNeural -f script.txt --write-media briefing.mp3; }
```

edge-tts can die mid-stream (`ServerTimeoutError`) and leave a truncated
MP3 that still plays — trust the exit status, not the file. On failure,
delete the file and retry once, as above.

Measure with `ffprobe` and note the actual duration — report it as-is, no
re-synthesis loop.

Done when ffprobe reads a valid duration from `briefing.mp3`.

## 5. Publish

Slug: `YYYY-MM-DD-<day-part>` from the current local time — `morning`
(before 12), `noon` (12–16), `evening` (17–21), `night` otherwise. If step
1's episode list already has that slug, suffix `-update1` (then `-update2`,
…) — publishing an existing slug silently replaces it.

Title: `<Topic> Briefing — <Month D>`. Description (the show notes), in
this order: a `Sources:` list on top — one line per story with headline,
outlet, date, and URL — then the heading `Transcript` followed by the full
script. Omit `duration_seconds`; the server estimates it from the MP3
frames.

```sh
cat sources.txt script.txt > description.txt
jq -n --arg t "$TITLE" --rawfile d description.txt '{title:$t, description:$d}' > metadata.json
curl -sfu "$PODCAST_PUBLISH_CREDENTIALS" -X PUT \
  -F 'metadata=<metadata.json;type=application/json' \
  -F 'audio=@briefing.mp3;type=audio/mpeg' \
  "$PODCAST_HOST/me/episodes/<slug>"
```

Done when the PUT succeeds. Report the title, slug, measured duration, and
the feed URL — read it from `feed_url` in
`curl -sfu "$PODCAST_PUBLISH_CREDENTIALS" "$PODCAST_HOST/me"`.
