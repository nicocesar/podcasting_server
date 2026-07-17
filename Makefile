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
# cloudbuild.yaml stamps it into version.txt and tags the image with it,
# and GET /version then reports the deployed commit. Trigger builds get
# SHORT_SHA for free; this only matters for `make deploy`.
deploy:
	gcloud builds submit --config cloudbuild.yaml \
		--substitutions=SHORT_SHA=$$(git rev-parse --short HEAD)
