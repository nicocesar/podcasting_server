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
deploy:
	gcloud builds submit --config cloudbuild.yaml
