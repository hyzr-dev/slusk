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
