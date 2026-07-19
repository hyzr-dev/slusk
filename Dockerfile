# Build stage: static, cgo-free binary.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/slskdarr ./cmd/slskdarr

# Runtime stage: distroless, non-root, fixed UID.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/slskdarr /usr/local/bin/slskdarr
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/slskdarr", "--config", "/config/config.toml"]

# distroless has no shell/curl/wget, so HEALTHCHECK execs the binary itself in
# --healthcheck mode: it hits its own /healthz over loopback and exits 0/1.
# /healthz reflects pipeline liveness (recent module attempts), while /readyz
# separately reports whether modules are succeeding; see pipeline.Runner.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/usr/local/bin/slskdarr", "--config", "/config/config.toml", "--healthcheck"]
