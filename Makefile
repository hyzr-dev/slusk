.PHONY: ui build dev test clean lab-up lab-reset lab-down lab-destroy

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

lab-up:
	./testenv/lab.sh up

lab-reset:
	./testenv/lab.sh reset

lab-down:
	./testenv/lab.sh down

lab-destroy:
	./testenv/lab.sh destroy
