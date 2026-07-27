# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
#RUN --mount=type=cache,target=/go/pkg/mod go mod download
RUN go mod download
COPY . .

# with BUILDKIT (via artifact registry and cloudbuild.yaml
#RUN --mount=type=cache,target=/go/pkg/mod \
#    --mount=type=cache,target=/root/.cache/go-build \
#    CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/server ./cmd/server
# The build stamp. version.txt travels in the source as written; the
# commit and the build time are link-time only, so a local `go build`
# leaves them empty and the UI shows nothing rather than something made
# up. Empty args are fine: -X on an empty value is a no-op.
ARG COMMIT=""
ARG BUILT_AT=""
RUN BUILT="${BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -X main.commit=${COMMIT} -X main.builtAt=$BUILT" \
    -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
ENV PORT=8080
ENTRYPOINT ["/server"]
