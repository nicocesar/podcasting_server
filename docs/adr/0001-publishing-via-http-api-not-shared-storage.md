# Publishing happens through the server's HTTP API, not shared storage

The Generator (a separate, still-fuzzy service) needs to deliver Episodes.
We decided it publishes via an authenticated HTTP API
(`PUT /shows/{show}/episodes/{slug}`) instead of writing directly to
Datastore/GCS. The server is the single owner of the storage schema and its
invariants (slug uniqueness, metadata validity, show-must-exist); the
Generator stays a dumb HTTP client with no GCP credentials or SDKs, and the
same contract works unchanged against the local filesystem backend and
production.

## Considered Options

- **Shared storage**: Generator writes MP3s to GCS and entities to
  Datastore itself. Rejected: two codebases coupled to one storage schema,
  no single place to enforce invariants, and the Generator would need GCP
  credentials and different code paths for local development.

## Consequences

- Uploads pass through Cloud Run, which caps HTTP/1 request bodies at
  32 MB. Fine for news-summary MP3s (~1 MB/minute at 128 kbps); if episodes
  ever outgrow this, add a signed-upload-URL endpoint rather than reverting
  to shared storage.
