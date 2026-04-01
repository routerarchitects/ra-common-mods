# buildinfo

`buildinfo` exposes build metadata for Go services.

## What this module returns

- `GetVersion()` -> release version (expected from Git tag)
- `GetBuildTimestamp()` -> build timestamp (UTC)
- `GetCommitHash()` -> commit hash

The values should be injected at build time using `-ldflags -X`.

## Build-Time Injection (Required)

Use the fully-qualified symbol names when setting values:

- `github.com/routerarchitects/ra-common-mods/build-info.version`
- `github.com/routerarchitects/ra-common-mods/build-info.buildTimestamp`
- `github.com/routerarchitects/ra-common-mods/build-info.commitHash`

Example local build:

```bash
VERSION="$(git describe --tags 2>/dev/null || echo -n '')"
BUILD_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
COMMIT_HASH="$(git rev-parse --short=12 HEAD)"

go build -ldflags "-s -w \
  -X github.com/routerarchitects/ra-common-mods/build-info.version=${VERSION} \
  -X github.com/routerarchitects/ra-common-mods/build-info.buildTimestamp=${BUILD_TIMESTAMP} \
  -X github.com/routerarchitects/ra-common-mods/build-info.commitHash=${COMMIT_HASH}" ./cmd/my-service
```

## Usage

```go
import "github.com/routerarchitects/ra-common-mods/build-info"

ver := buildinfo.GetVersion()
ts := buildinfo.GetBuildTimestamp()
sha := buildinfo.GetCommitHash()
```

## Dockerfile

Use a multi-stage build and pass Git-derived values as build args.

```dockerfile
# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS builder
WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY . .

# Path to your main package, e.g. ./cmd/my-service
ARG APP_PATH=./cmd/my-service

# Injected from CI/CD (usually derived from Git metadata)
ARG VERSION=dev
ARG BUILD_TIMESTAMP=unknown
ARG COMMIT_HASH=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w \
  -X github.com/routerarchitects/ra-common-mods/build-info.version=${VERSION} \
  -X github.com/routerarchitects/ra-common-mods/build-info.buildTimestamp=${BUILD_TIMESTAMP} \
  -X github.com/routerarchitects/ra-common-mods/build-info.commitHash=${COMMIT_HASH}" \
  -o /out/app ${APP_PATH}

FROM gcr.io/distroless/static-debian12 AS runtime
COPY --from=builder /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
```

Example Docker build command:

```bash
docker build \
  --build-arg APP_PATH=./cmd/my-service \
  --build-arg VERSION="$(git describe --tags --exact-match 2>/dev/null || git rev-parse --short=12 HEAD)" \
  --build-arg BUILD_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --build-arg COMMIT_HASH="$(git rev-parse --short=12 HEAD)" \
  -t my-service:$(git rev-parse --short=12 HEAD) .
```

## Notes

- If `ldflags` are not provided, values may be empty/default.
- `git describe --tags --exact-match` returns a tag only when `HEAD` itself is tagged.
- Keep version source of truth as Git tag in your CI/CD pipeline.
