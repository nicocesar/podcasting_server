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
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
ENV PORT=8080
ENTRYPOINT ["/server"]
