---
name: verify
description: Build, run, drive, and cleanly stop this server for end-to-end verification.
---

# Verifying the podcasting server

Run everything from a scratch dir (`$SC`) outside the repo; the server
needs no repo-relative paths at runtime.

## Start (pidfile — never pkill/pgrep by pattern later)

```bash
go build -o $SC/server ./cmd/server
STORAGE=fs DATA_DIR=$SC/data ADMIN_TOKEN=verify-secret PORT=18432 \
  $SC/server > $SC/server.log 2>&1 &
echo $! > $SC/server.pid
sleep 1 && curl -s http://localhost:18432/version   # readiness check
```

Add `ANTHROPIC_API_KEY=sk-ant-fake` to enable the /me/generate surface;
a fake key is fine for form/validation checks (the pipeline fails at
research with a logged 401, which is expected).

## Stop

```bash
kill $(cat $SC/server.pid); rm -rf $SC/data $SC/server.pid
```

Do NOT `pkill -f`/`pgrep -f` a path substring: the pattern matches the
shell running the check itself, so it always "finds" a process and can
kill your own shell (exit 144).

## Drive

Two credentials (ADR 0010): browsers log in for a `session` cookie; agents
use a Bearer API key minted from a session.

```bash
# provision a user; response JSON has id + a temporary password
curl -s -X PUT -H "Authorization: Bearer verify-secret" \
  http://localhost:18432/admin/users/testuser

# log in (303) and capture the session cookie
curl -s -c $SC/jar -d "username=testuser&password=<password>" \
  -o /dev/null http://localhost:18432/login

# mint an API key with the session; response JSON has key ("pods_...")
curl -s -b $SC/jar -H 'Content-Type: application/json' \
  -d '{"name":"verify"}' http://localhost:18432/me/api-keys

K="Authorization: Bearer <pods_... key>"
curl -s -b $SC/jar -H "Accept: text/html" http://localhost:18432/me      # dashboard HTML (session)
curl -s -H "$K" http://localhost:18432/me                                # JSON (API key)
curl -s -b $SC/jar -d "topic=x&length=5&freshness=7&language=en&voice=female" \
  http://localhost:18432/me/generate                                     # 201 + JSON record
```

Handlers answer HTML only with `Accept: text/html`; otherwise JSON.
Credential management (`/me/api-keys`, `/me/password`, `/me/feed-token`)
is session-only — an API key gets 403 there. Set `GOOGLE_CLIENT_ID` +
`GOOGLE_CLIENT_SECRET` to render the Google buttons (the full OIDC flow
needs a real client; skip it locally).

## Real TTS endpoints

`EDGE_TTS_SMOKE=1 go test ./internal/tts -run EdgeSmoke -v` hits the
real Microsoft endpoint (needs network, sandbox off). Google TTS needs
GCP application-default credentials — usually unverifiable locally.
