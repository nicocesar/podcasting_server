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
# The one thing in this image that is not the server. internal/mix shells
# out to ffmpeg to lay a music bed under a story, which is overlap, and
# overlap is the one thing appending MP3 frames cannot do.
#
# These are hardened static PIE builds with no external dependencies, which
# is what makes them legal here: the base image stays distroless/static,
# there is still no libc, no shell and no package manager, the build stays
# CGO_ENABLED=0, and the container still runs as nonroot. Nothing about the
# image changes except that this file exists.
COPY --from=mwader/static-ffmpeg:8.1 /ffmpeg /ffmpeg
COPY --from=build /out/server /server
ENV PORT=8080
ENTRYPOINT ["/server"]
