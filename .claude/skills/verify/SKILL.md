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

```bash
# provision a user; response JSON has id + publish_token for basic auth
curl -s -X PUT -H "Authorization: Bearer verify-secret" \
  http://localhost:18432/admin/users/testuser

C="testuser:<publish_token>"
curl -s -u $C -H "Accept: text/html" http://localhost:18432/me          # dashboard HTML
curl -s -u $C -H "Accept: text/html" http://localhost:18432/me/generate # generate form
curl -s -u $C -d "topic=x&length=5&freshness=7&language=en&voice=female" \
  http://localhost:18432/me/generate                                    # 201 + JSON record
```

Handlers answer HTML only with `Accept: text/html`; otherwise JSON.

## Real TTS endpoints

`EDGE_TTS_SMOKE=1 go test ./internal/tts -run EdgeSmoke -v` hits the
real Microsoft endpoint (needs network, sandbox off). Google TTS needs
GCP application-default credentials — usually unverifiable locally.
