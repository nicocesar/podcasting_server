# podcasting_server

A private, single-user podcast server. It hosts generated, news-like audio
briefings as podcast feeds for AntennaPod. Episodes are produced by a
separate Generator service (not in this repo) and delivered through the
Publishing Contract below.

- Domain vocabulary: [CONTEXT.md](CONTEXT.md)
- Key decisions: [docs/adr/](docs/adr/)

## How it fits together

- **Cloud Run** runs the server; IAM allows unauthenticated, the app
  enforces HTTP Basic auth itself (the only scheme AntennaPod speaks).
- **Datastore** holds show/episode metadata; **GCS** holds audio and cover
  art. Audio downloads are 302 redirects to 15-minute signed URLs, so the
  server never streams audio.
- Two credentials (`user:password`): **Reader** (the phone; feeds and
  audio) and **Writer** (the Generator and you; publishing + management).
  Cover art and per-show HTML pages are public (ADR 0003).
- HTML pages are `html/template` files under `cmd/server/templates`
  (layout + pages + `fragments/`), shipped in the binary via `go:embed` —
  editing them means rebuild + redeploy.

## Local development

```sh
make run    # filesystem backend in ./data, creds reader:reader / writer:writer
make test
```

The filesystem backend (dev only) is read live — drop or edit files and
refresh the feed:

```
data/
├── ai-news/                      # show ID
│   ├── show.json                 # {"title": ..., "description": ...}
│   ├── cover.jpg
│   ├── 2026-07-06-morning.mp3
│   └── 2026-07-06-morning.json   # episode metadata sidecar
└── markets/
```

## API

Public surface — no auth (see [ADR 0003](docs/adr/0003-public-surface-cover-and-show-page.md)):

| Endpoint | Purpose |
|---|---|
| `GET /` | bland landing page; lists nothing |
| `GET /shows/{show}` | show page: cover, title, description, feed URL — no episode data |
| `GET /shows/{show}/cover` | cover art (public so any podcast client's artwork fetch works) |
| `GET /static/*` | page assets |

A show's identity is public for anyone who guesses its ID, so keep show
descriptions non-sensitive. Episode titles, summaries, and audio stay
private.

Read side (Reader or Writer credentials):

| Endpoint | Purpose |
|---|---|
| `GET /shows/{show}/feed.xml` | podcast RSS, all episodes newest-first |
| `GET /shows/{show}/episodes/{slug}.mp3` | audio (302 to signed URL in prod) |

Write side (Writer credentials):

| Endpoint | Purpose |
|---|---|
| `PUT /shows/{show}` | create/update a show (JSON: `title`, `description`, `language`) |
| `PUT /shows/{show}/image` | upload cover art (body = JPEG or PNG bytes) |
| `PUT /shows/{show}/episodes/{slug}` | publish an episode (see below) |
| `GET /shows` · `GET /shows/{show}/episodes` | list (JSON) |
| `DELETE /shows/{show}` · `DELETE .../episodes/{slug}` | delete |

Show IDs and slugs match `^[a-z0-9][a-z0-9._-]*$`.

## The Publishing Contract

`PUT /shows/{show}/episodes/{slug}` with `multipart/form-data`:

- `metadata` — JSON: `title` (required), `description` (the full generated
  summary text; shown as show notes), `published_at` (RFC 3339, default
  now), `duration_seconds` (optional — when omitted the server estimates
  it from the MP3's frames; send it to override)
- `audio` — the MP3 bytes

```sh
curl -u "generator:PASSWORD" -X PUT \
  -F 'metadata={"title":"Morning Briefing — July 6","description":"..."};type=application/json' \
  -F 'audio=@briefing.mp3;type=audio/mpeg' \
  https://HOST/shows/ai-news/episodes/2026-07-06-morning
```

The slug is the episode's identity: publishing an existing slug **replaces**
it (idempotent — an hourly cron can safely retry). The show must already
exist; a typo'd show ID fails with 404 instead of silently creating a feed.
Slug convention: `YYYY-MM-DD-<daypart>[-suffix]` with day-parts `morning`,
`noon`, `evening`, `night` (convention only, not enforced).

## Deploy

One-time GCP setup: [SETUP.md](SETUP.md). After that:

```sh
make deploy   # Cloud Build: buildx + registry cache → Cloud Run
```

## Configuration (env vars)

| Var | Default | Meaning |
|---|---|---|
| `READER_CREDENTIALS` | — (required) | `user:password` for the read side |
| `WRITER_CREDENTIALS` | — (required) | `user:password` for the write side |
| `STORAGE` | `fs` | `fs` (dev) or `gcp` |
| `DATA_DIR` | `./data` | fs backend root |
| `GCS_BUCKET` | — | required when `STORAGE=gcp` |
| `GCP_PROJECT` | auto-detect | Datastore project |
| `BASE_URL` | derived from request | external URL override for feed links |
| `PORT` | `8080` | listen port |
