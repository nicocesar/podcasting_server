# The Slug is the Episode's identity; republishing a Slug replaces the Episode

An Episode is identified within its Show by a caller-chosen Slug (by
convention `YYYY-MM-DD-<day-part>[-suffix]`, e.g. `2026-07-06-morning`).
Publishing to an existing Slug is an idempotent upsert that replaces the
Episode's audio and metadata — there are no server-generated episode IDs
and no duplicate detection beyond the Slug itself. We chose this because
the Generator is an unattended hourly job: retries and double-fires must
not create duplicate feed entries, and predictable URLs make the system
debuggable with curl alone.

## Consequences

- Two distinct episodes can never share a Slug; "another morning episode"
  needs a new Slug (e.g. `-update1`). Day-parts are convention, not schema.
- The RSS GUID is derived from (show, slug), so a replaced Episode keeps
  its GUID — podcast clients that already downloaded it will not re-download
  the replacement. Acceptable for a single-user server; republish is meant
  for fixing bad generations, not versioning.
