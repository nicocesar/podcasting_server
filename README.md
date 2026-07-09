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
- Each user has two secrets (ADR 0005/0008): a **Feed Token** — the
  capability URL their podcast client subscribes to, no password dialog —
  and a **publish token** (the Basic-auth password for their Generator
  and the `/me` Management API). Ownership is the credential: whoever
  publishes an episode owns it.
- An episode exists once, under its owner; shares are **references**
  (ADR 0006). The owner's republish or delete propagates to every feed.
- **Built-in Generation** (ADR 0009, optional): `/me/generate` turns a
  topic + length + freshness + language into an episode. A Claude Managed
  Agents session researches and writes the script — text only, no
  credential ever leaves the server — then the server voices it
  (edge-tts, Google Cloud TTS fallback) and publishes it. Enabled by
  setting `ANTHROPIC_API_KEY` (SETUP.md §11); progress is checkpointed
  and resumes across restarts.
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
# → {"id":"alice","publish_token":"...","feed_url":"http://localhost:8080/f/<token>/feed.xml"}
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
| `GET`/`POST /invites/{token}` | invite redemption page — the only way to join (ADR 0007) |
| `GET /static/*` | page assets |

Read side — the Feed Token namespace (ADR 0008; the URL is the key, no
other auth):

| Endpoint | Purpose |
|---|---|
| `GET /f/{token}` | subscribe page: cover, title, feed URL, QR, AntennaPod link |
| `GET /f/{token}/feed.xml` | Personal Feed RSS: own + shared episodes, newest-first |
| `GET /f/{token}/{owner}/{slug}.mp3` | audio (302 to signed URL in prod); episodes in this feed only |
| `GET /f/{token}/cover` | cover art |
| `GET /f/{token}/qr.png` | the feed URL as a scannable QR code |

Feed Variants (ADR 0005) — query params on `feed.xml` and `/me/feed`:
`?filter=mine`, `?filter=shared`, `?from=<owner>`, `?from=me`. Each RSS
item carries its owner in `<itunes:author>`.

Management API (publish token; always scoped to the caller):

| Endpoint | Purpose |
|---|---|
| `GET /me` | the **Dashboard** in a browser (invite links, share episodes, friend search); JSON for curl |
| `PUT /me` | feed settings (JSON: `title`, `description`, `language`) |
| `POST /me/feed-token` | reset the Feed Token: new feed URL, old one dies instantly |
| `GET /me/users?q=` | member search for the share box (self excluded, max 20 hits) |
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

The **Dashboard** at `https://HOST/me` is the browser home for all of the
above: open it, enter `username:publish-token` at the Basic-auth prompt,
and you get one-click invite links, per-episode "share with…" boxes with
member autocomplete, and pending-invite management. Members can find each
other by name there; nothing is searchable without credentials.

Admin — fallback provisioning and recovery (`Authorization: Bearer $ADMIN_TOKEN`):

| Endpoint | Purpose |
|---|---|
| `PUT /admin/users/{user}` | create a user; returns the feed URL and publish token **once** |
| `POST /admin/users/{user}/credentials` | rotate a user's lost secrets — new feed URL + publish token, content untouched |
| `GET /admin/users` | list users |
| `DELETE /admin/users/{user}` | delete a user, their episodes, and every reference to them |

When a user loses their once-shown credentials, rotate them — episodes,
shares, and the feed URL survive; only the secrets change (ADR 0007):

```sh
curl -H "Authorization: Bearer ${ADMIN_TOKEN}" -X POST \
  https://HOST/admin/users/alice/credentials
# → {"id":"alice","publish_token":"NEW...","feed_url":"https://HOST/f/NEWTOKEN/feed.xml"}
```

The old feed URL and publish token stop working immediately: the user
resubscribes their podcast app (scan the QR at the new `/f/{token}` page)
and updates `PODCAST_PUBLISH_CREDENTIALS` wherever their Generator runs.

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
