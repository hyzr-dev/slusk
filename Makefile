.PHONY: ui build dev test clean

ui:
	rm -rf internal/observ/web/dist/assets internal/observ/web/dist/index.html
	cd web && npm ci && npm run build

build: ui
	go build -o slskdarr ./cmd/slskdarr

dev:
	cd web && npm run dev

test:
	go test ./...
	cd web && npm test

clean:
	rm -rf internal/observ/web/dist
	git checkout internal/observ/web/dist/placeholder.html
