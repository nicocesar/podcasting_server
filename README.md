# podcasting_server

A private, multi-user podcast server. Every User has one private **Personal
Feed** of generated, news-like audio briefings, consumed from AntennaPod,
and can share individual episodes with other users. Episodes are produced
by a separate Generator service (not in this repo) and delivered through
the Publishing Contract below.

- Domain vocabulary: [CONTEXT.md](CONTEXT.md)
- Key decisions: [docs/adr/](docs/adr/)

## How it fits together

- **Cloud Run** runs the server; IAM allows unauthenticated, the app
  enforces HTTP Basic auth itself (the only scheme AntennaPod speaks).
- **Datastore** holds user/episode/share metadata; **GCS** holds audio and
  cover art. Audio downloads are 302 redirects to 15-minute signed URLs,
  so the server never streams audio.
- Each user has two credentials (ADR 0005): a **read credential**
  (`user:password`, for the phone) and a **publish token** (used as the
  Basic-auth password by their Generator and for the `/me` Management
  API). Ownership is the credential: whoever publishes an episode owns it.
- An episode exists once, under its owner; shares are **references**
  (ADR 0006). The owner's republish or delete propagates to every feed.
- HTML pages are `html/template` files under `cmd/server/templates`
  (layout + pages + `fragments/`), shipped in the binary via `go:embed` —
  editing them means rebuild + redeploy.

## Local development

```sh
make run    # filesystem backend in ./data, admin token "admin"
make test
```

Provision a user and publish:

```sh
curl -H "Authorization: Bearer admin" -X PUT localhost:8080/admin/users/alice
# → {"id":"alice","read_credentials":"alice:...","publish_token":"...","feed_url":...}
```

The filesystem backend (dev only) is read live — drop or edit files and
refresh the feed:

```
data/
├── alice/                        # user ID
│   ├── user.json                 # feed metadata + credential hashes
│   ├── shares.json               # episodes shared into alice's feed
│   ├── cover.jpg
│   ├── 2026-07-06-morning.mp3
│   └── 2026-07-06-morning.json   # episode metadata sidecar
└── bob/
```

## API

Public surface — no auth (ADR 0003/0005): a bland landing page, cover art
behind an unguessable per-user URL, static assets. Nothing about a user is
enumerable; feeds, episodes, and audio all require credentials.

| Endpoint | Purpose |
|---|---|
| `GET /` | bland landing page; lists nothing |
| `GET /covers/{secret}` | cover art (public so any podcast client's artwork fetch works) |
| `GET`/`POST /invites/{token}` | invite redemption page — the only way to join (ADR 0007) |
| `GET /static/*` | page assets |

Read side (read credential or publish token; only your own feed):

| Endpoint | Purpose |
|---|---|
| `GET /users/{user}` | subscribe page: cover, title, feed URL (login required) |
| `GET /users/{user}/feed.xml` | Personal Feed RSS: own + shared episodes, newest-first |
| `GET /users/{owner}/episodes/{slug}.mp3` | audio (302 to signed URL in prod); owner or share-holders only |

Feed Variants (ADR 0005) — query params on `feed.xml` and `/me/feed`:
`?filter=mine`, `?filter=shared`, `?from=<owner>`, `?from=me`. Each RSS
item carries its owner in `<itunes:author>`.

Management API (publish token; always scoped to the caller):

| Endpoint | Purpose |
|---|---|
| `GET /me` · `PUT /me` | feed settings (JSON: `title`, `description`, `language`) |
| `PUT /me/image` | upload cover art (body = JPEG or PNG bytes) |
| `GET /me/feed` | the feed as JSON, with provenance (`owner`, `sharer`) |
| `GET /me/episodes` | own episodes (JSON) |
| `PUT /me/episodes/{slug}` | publish an episode (see below) |
| `DELETE /me/episodes/{slug}` | delete own episode — removed from **every** feed |
| `POST /me/feed/{owner}/{slug}/share` | share to another user (JSON: `{"to":"bob"}`); forwarding allowed |
| `DELETE /me/feed/{owner}/{slug}` | remove a shared episode from my feed |
| `PUT`/`DELETE /me/blocks/{user}` | block/unblock a sharer (their shares are rejected) |
| `PUT`/`DELETE /me/mutes/{user}` | mute/unmute an owner (their episodes are hidden) |
| `POST /me/invites` | mint an invite link (single-use, 7-day expiry); optional payload `{"owner","slug"}` — that episode lands in the new feed as a share from you |
| `GET /me/invites` | list your invites with status (`pending`/`redeemed`/`expired`) |
| `DELETE /me/invites/{token}` | revoke a pending invite |

Growth is by invitation (ADR 0007): any user can mint an invite; the
invitee opens the link, picks a username, and gets credentials shown once.
There is no open signup and no email anywhere in the system.

Admin — fallback provisioning and recovery (`Authorization: Bearer $ADMIN_TOKEN`):

| Endpoint | Purpose |
|---|---|
| `PUT /admin/users/{user}` | create a user; returns credentials **once** (only hashes are stored) |
| `POST /admin/users/{user}/credentials` | rotate a user's lost credentials (content and feed URL untouched) |
| `GET /admin/users` | list users |
| `DELETE /admin/users/{user}` | delete a user, their episodes, and every reference to them |

User IDs and slugs match `^[a-z0-9][a-z0-9._-]*$`.

## The Publishing Contract

`PUT /me/episodes/{slug}` with `multipart/form-data`, authenticated with
the publishing user's token — publishing into someone else's feed is
inexpressible (ADR 0005):

- `metadata` — JSON: `title` (required), `description` (the full generated
  summary text; shown as show notes), `published_at` (RFC 3339, default
  now), `duration_seconds` (optional — when omitted the server estimates
  it from the MP3's frames; send it to override)
- `audio` — the MP3 bytes

```sh
curl -u "alice:PUBLISH_TOKEN" -X PUT \
  -F 'metadata={"title":"Morning Briefing — July 6","description":"..."};type=application/json' \
  -F 'audio=@briefing.mp3;type=audio/mpeg' \
  https://HOST/me/episodes/2026-07-06-morning
```

The slug is the episode's identity within its owner's feed: publishing an
existing slug **replaces** it (idempotent — an hourly cron can safely
retry), everywhere it is shared. Slug convention:
`YYYY-MM-DD-<daypart>[-suffix]` with day-parts `morning`, `noon`,
`evening`, `night` (convention only, not enforced).

## Deploy

One-time GCP setup: [SETUP.md](SETUP.md). After that:

```sh
make deploy   # Cloud Build: buildx + registry cache → Cloud Run
```

## Configuration (env vars)

| Var | Default | Meaning |
|---|---|---|
| `ADMIN_TOKEN` | — (required) | bearer token for `/admin` user provisioning |
| `STORAGE` | `fs` | `fs` (dev) or `gcp` |
| `DATA_DIR` | `./data` | fs backend root |
| `GCS_BUCKET` | — | required when `STORAGE=gcp` |
| `GCP_PROJECT` | auto-detect | Datastore project |
| `BASE_URL` | derived from request | external URL override for feed links |
| `PORT` | `8080` | listen port |
