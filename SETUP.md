# One-time GCP setup

Everything below happens once per project. Day-to-day deploys are just
`make deploy`.

```sh
export PROJECT_ID=$(gcloud config get-value project)
export REGION=us-central1
export BUCKET=${PROJECT_ID}-podcast-audio
export SA=podcasting-server@${PROJECT_ID}.iam.gserviceaccount.com
```

## 1. Enable APIs

```sh
gcloud services enable \
  run.googleapis.com \
  cloudbuild.googleapis.com \
  datastore.googleapis.com \
  storage.googleapis.com \
  secretmanager.googleapis.com \
  iamcredentials.googleapis.com
```

## 2. Datastore database

Firestore in **Datastore mode** (one per project; skip if it exists):

```sh
gcloud firestore databases create --location=${REGION} --type=datastore-mode
```

No composite indexes are needed — the server filters episodes by `show_id`
(a built-in single-property index) and sorts in memory.

## 3. Bucket

```sh
gcloud storage buckets create gs://${BUCKET} \
  --location=${REGION} --uniform-bucket-level-access
```

The bucket stays private; listeners get 15-minute signed URLs.

## 4. Service account and roles

```sh
gcloud iam service-accounts create podcasting-server

# Datastore + bucket access
gcloud projects add-iam-policy-binding ${PROJECT_ID} \
  --member=serviceAccount:${SA} --role=roles/datastore.user
gcloud storage buckets add-iam-policy-binding gs://${BUCKET} \
  --member=serviceAccount:${SA} --role=roles/storage.objectAdmin

# Sign V4 URLs without a key file: the SA needs signBlob on itself
gcloud iam service-accounts add-iam-policy-binding ${SA} \
  --member=serviceAccount:${SA} --role=roles/iam.serviceAccountTokenCreator
```

## 5. Credentials secrets

Format is `user:password`. Reader is for AntennaPod, Writer for the
Generator and your own curl.

```sh
printf 'nico:%s' "$(openssl rand -hex 16)" | \
  gcloud secrets create podcast-reader-credentials --data-file=-
printf 'generator:%s' "$(openssl rand -hex 24)" | \
  gcloud secrets create podcast-writer-credentials --data-file=-

for s in podcast-reader-credentials podcast-writer-credentials; do
  gcloud secrets add-iam-policy-binding $s \
    --member=serviceAccount:${SA} --role=roles/secretmanager.secretAccessor
done
```

Read them back when configuring clients:

```sh
gcloud secrets versions access latest --secret=podcast-reader-credentials
```

## 6. Cloud Build permissions

The default Cloud Build service account needs to deploy Cloud Run and act
as the runtime SA:

```sh
export CB_SA=$(gcloud projects describe ${PROJECT_ID} \
  --format='value(projectNumber)')@cloudbuild.gserviceaccount.com
gcloud projects add-iam-policy-binding ${PROJECT_ID} \
  --member=serviceAccount:${CB_SA} --role=roles/run.admin
gcloud iam service-accounts add-iam-policy-binding ${SA} \
  --member=serviceAccount:${CB_SA} --role=roles/iam.serviceAccountUser
```

## 7. First deploy

```sh
make deploy
gcloud run services describe podcasting-server \
  --region=${REGION} --format='value(status.url)'
```

`BASE_URL` is not required: the server derives feed URLs from the request's
`Host` and `X-Forwarded-Proto` headers. Set it as an env var only if you
put the service behind a custom domain that rewrites Host.

## 8. Create your first show

```sh
export URL=$(gcloud run services describe podcasting-server --region=${REGION} --format='value(status.url)')
export WRITER=$(gcloud secrets versions access latest --secret=podcast-writer-credentials)

curl -u "${WRITER}" -X PUT "${URL}/shows/ai-news" \
  -H 'Content-Type: application/json' \
  -d '{"title":"AI News","description":"Hourly AI briefings","language":"en"}'
curl -u "${WRITER}" -X PUT "${URL}/shows/ai-news/image" \
  -H 'Content-Type: image/jpeg' --data-binary @cover.jpg
```

## 9. Publish a test episode

Any MP3 works; if you don't have one handy, generate a 3-second tone:

```sh
ffmpeg -f lavfi -i "sine=frequency=440:duration=3" -q:a 9 test.mp3
```

Publish it through the Publishing Contract (`PUT` is idempotent — re-running
replaces the episode, exactly what the Generator will do):

```sh
curl -u "${WRITER}" -X PUT \
  -F 'metadata={"title":"Test episode","description":"Publishing smoke test.","duration_seconds":3};type=application/json' \
  -F 'audio=@test.mp3;type=audio/mpeg' \
  "${URL}/shows/ai-news/episodes/$(date +%F)-test"
```

Verify it end to end — the episode in the feed, then the audio download
(in prod this is a 302 to a signed GCS URL, hence `-L`):

```sh
export READER=$(gcloud secrets versions access latest --secret=podcast-reader-credentials)

curl -u "${READER}" "${URL}/shows/ai-news/feed.xml"
curl -u "${READER}" -L -o /dev/null -w '%{http_code} %{size_download} bytes\n' \
  "${URL}/shows/ai-news/episodes/$(date +%F)-test.mp3"
```

Clean up when done:

```sh
curl -u "${WRITER}" -X DELETE "${URL}/shows/ai-news/episodes/$(date +%F)-test"
```

## 10. Subscribe in AntennaPod

Add podcast by RSS address: `${URL}/shows/ai-news/feed.xml` — AntennaPod
will get a 401, prompt for username/password, and store the reader
credentials for both feed refreshes and episode downloads.

The show also has a public page (cover, description, feed URL — no
episode data) at `${URL}/shows/ai-news`, handy for grabbing the feed URL
on a new device.
