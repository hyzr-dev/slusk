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
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/slskdarr ./cmd/slskdarr

# Runtime stage: distroless, non-root, fixed UID.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/slskdarr /usr/local/bin/slskdarr
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/slskdarr", "--config", "/config/config.toml"]

# distroless has no shell/curl/wget, so HEALTHCHECK execs the binary itself in
# --healthcheck mode: it hits its own /healthz over loopback and exits 0/1.
# /healthz reflects pipeline liveness (recent module attempts) and /readyz
# reports whether modules are succeeding; both are public probes that return no
# credentials or internal status. Private UI/API/metrics endpoints require the
# configured observ token. See pipeline.Runner.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/usr/local/bin/slskdarr", "--config", "/config/config.toml", "--healthcheck"]
