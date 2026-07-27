.PHONY: run test build docker deploy

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

# Build and deploy via Cloud Build (see cloudbuild.yaml and SETUP.md).
# Manual submits carry no git context, so feed SHORT_SHA from local git:
# it tags the image, is linked into the binary as the commit, and GET
# /version then reports the deployed commit. _BUILT_AT rides along for
# the dashboard's build stamp. Trigger builds get SHORT_SHA for free and
# fall back to the builder's clock for the timestamp.
deploy:
	gcloud builds submit --config cloudbuild.yaml \
		--substitutions=SHORT_SHA=$$(git rev-parse --short HEAD),_BUILT_AT=$$(date -u +%Y-%m-%dT%H:%M:%SZ)
