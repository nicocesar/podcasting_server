.PHONY: run test build docker deploy deploy-preflight

# Local development: filesystem backend in ./data, throwaway admin token.
run:
	ADMIN_TOKEN=admin \
	STORAGE=fs DATA_DIR=./data \
	go run ./cmd/server

test:
	go test ./...

build:
	go build ./...

docker:
	docker buildx build -t podcasting_server .

# .gcloudignore is a denylist written against the files that existed the
# day it was written; a `credentials.json` dropped in the root tomorrow
# would upload in silence. So ask gcloud what it is about to send and
# refuse on anything credential-shaped. Matching is on the path, never on
# contents — this is a guard rail, not a secret scanner, and it must not
# trip over source files that merely talk about credentials.
deploy-preflight:
	@found=$$(gcloud meta list-files-for-upload | grep -iE '(^|/)\.env($$|\.)|(^|/)client_secret|credential[^/]*\.json$$|-key\.json$$|(^|/)(sa|token)\.json$$|\.(pem|p12)$$|(^|/)id_rsa|^(data|\.claude|\.agents)/'); \
	if [ -n "$$found" ]; then \
		echo "deploy-preflight: refusing to upload these to Cloud Build:" >&2; \
		echo "$$found" | sed 's/^/  /' >&2; \
		echo "Exclude them in .gcloudignore (or delete them) and try again." >&2; \
		exit 1; \
	fi

# Build and deploy via Cloud Build (see cloudbuild.yaml and SETUP.md).
# Manual submits carry no git context, so feed SHORT_SHA from local git:
# it tags the image, is linked into the binary as the commit, and GET
# /version then reports the deployed commit. _BUILT_AT rides along for
# the dashboard's build stamp. Trigger builds get SHORT_SHA for free and
# fall back to the builder's clock for the timestamp.
deploy: deploy-preflight
	gcloud builds submit --config cloudbuild.yaml \
		--substitutions=SHORT_SHA=$$(git rev-parse --short HEAD),_BUILT_AT=$$(date -u +%Y-%m-%dT%H:%M:%SZ)
