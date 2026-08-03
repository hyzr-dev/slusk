# Frontend stage: build the SPA before Go embeds it.
FROM node:22 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Build stage: static, cgo-free binary with the SPA embedded.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /internal/observ/web/dist ./internal/observ/web/dist
# Left at "dev" for a local `docker build`; deploy.yml passes the v* tag that
# triggered the build, so a running container can report which release it is.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/slusk ./cmd/slusk

# Runtime stage: distroless, non-root, fixed UID.
FROM gcr.io/distroless/static-debian12:nonroot

# ghcr.io links a package to a repository by this label, and a linked package
# inherits that repository's visibility. Without it the package sits loose in the
# org and stays private even when the repo is public, which surfaces as an auth
# error on `docker pull` rather than as a visibility problem.
LABEL org.opencontainers.image.source="https://github.com/hyzr-dev/slusk"
LABEL org.opencontainers.image.licenses="AGPL-3.0-or-later"

COPY --from=build /out/slusk /usr/local/bin/slusk
# The AGPL requires the licence text to travel with the work, and pushing an
# image conveys it. distroless has no package manager to carry a copyright file,
# so the plain text ships alongside the binary.
COPY --from=build /src/LICENSE /usr/local/share/slusk/LICENSE
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/slusk", "--config", "/config/config.toml"]

# distroless has no shell/curl/wget, so HEALTHCHECK execs the binary itself in
# --healthcheck mode: it hits its own /healthz over loopback and exits 0/1.
# /healthz reflects pipeline liveness (recent module attempts) and /readyz
# reports whether modules are succeeding; both are public probes that return no
# credentials or internal status. Private UI/API/metrics endpoints require the
# configured observ token. See pipeline.Runner.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/usr/local/bin/slusk", "--config", "/config/config.toml", "--healthcheck"]
