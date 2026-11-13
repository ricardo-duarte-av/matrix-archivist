# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# Pure Go build: goolm (no libolm) + modernc SQLite (no CGO) → easy multi-arch.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags goolm -ldflags "-s -w -X main.version=${VERSION}" -o /out/archivist ./cmd/archivist

FROM alpine:3.22
RUN apk add --no-cache git openssh-client ca-certificates tzdata \
 && adduser -D -H -u 1000 archivist \
 && mkdir -p /data /config && chown archivist:archivist /data
COPY --from=build /out/archivist /usr/local/bin/archivist
USER archivist
VOLUME ["/data"]
ENTRYPOINT ["archivist", "-config", "/config/config.yaml"]
