# Frontend-SPA (fas 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ersätt `dashboard.html` + `dashboard.js` med en React-SPA som bevarar allt
nuvarande beteende, ritad enligt designprojektet "Slskdarr Dashboard Design".

**Architecture:** Frontend-källkod i `web/`, byggd med Vite till
`internal/observ/web/dist/` som `go:embed` serverar. TanStack Query hanterar all
serverstate med polling; React Router hanterar navigation; CSS Modules med tokens
ur designen hanterar styling.

**Tech Stack:** React 19, TypeScript, Vite, React Router, TanStack Query, Vitest +
React Testing Library. Go 1.26 backend, oförändrat utom asset-handlern och en ny
read-only config-endpoint.

**Spec:** `docs/superpowers/specs/2026-07-20-frontend-spa-design.md`
**Issue:** #87

## Global Constraints

- **Språk i UI: engelska.** Inga svenska strängar i komponenter.
- **Inga användarvända strängar i komponenter.** Allt går genom `src/strings.ts`.
- **Datumformat: `sv-SE`** (`2026-07-20 14:32`) trots engelskt UI. All formatering
  går genom `src/format.ts`, aldrig inline `toLocaleString`.
- **All kod på engelska** — filnamn, komponentnamn, variabler, routes, kommentarer.
- **Node 22, Go 1.26.**
- **Inga nya typsnitt.** IBM Plex Sans + IBM Plex Mono, laddade som idag.
- **Ingen SSE i fas 1** (uppskjuten till #60). Polling via TanStack Query.
- **Inga charts i fas 1** (uppskjutna till #88). Översikt reflowas utan dem.
- **Commit efter varje task.** Conventional commits, referera `(#87)`.

---

## Filstruktur

**Nya filer:**

| Fil | Ansvar |
| --- | --- |
| `Makefile` | `ui`, `build`, `dev`, `test`-mål |
| `web/package.json`, `web/tsconfig.json`, `web/vite.config.ts` | Byggkonfiguration |
| `web/index.html` | HTML-skal, typsnittsladdning |
| `web/src/main.tsx` | Rot, QueryClientProvider, router |
| `web/src/strings.ts` | Alla användarvända strängar |
| `web/src/format.ts` | `formatBytes`, `percent`, `formatDateTime`, `formatTime` |
| `web/src/api/types.ts` | TypeScript-speglingar av Go-DTO:er |
| `web/src/api/client.ts` | `apiGet`, `apiPost` |
| `web/src/api/queries.ts` | TanStack Query-hooks |
| `web/src/styles/tokens.css`, `global.css` | Designtokens, reset |
| `web/src/components/*` | Återanvändbara komponenter |
| `web/src/routes/*` | En fil per vy |
| `internal/observ/config.go` | Read-only `/api/config` |
| `internal/observ/assets.go` | Asset-handler (ersätter `web.go`) |

**Raderas:** `internal/observ/web/dashboard.html`, `internal/observ/web/dashboard.js`,
`internal/observ/web.go`.

---

### Task 1: Byggkedja och skelett

**Files:**
- Create: `web/package.json`, `web/tsconfig.json`, `web/tsconfig.node.json`, `web/vite.config.ts`, `web/index.html`, `web/src/main.tsx`, `web/src/App.tsx`, `Makefile`, `internal/observ/web/dist/index.html`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: inget
- Produces: `make ui` bygger till `internal/observ/web/dist/`. `App` är default-export från `web/src/App.tsx`.

- [ ] **Step 1: Skapa Vite-projektets konfiguration**

`web/package.json`:

```json
{
  "name": "slskdarr-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "test": "vitest run",
    "test:watch": "vitest"
  },
  "dependencies": {
    "@tanstack/react-query": "^5.62.0",
    "react": "^19.0.0",
    "react-dom": "^19.0.0",
    "react-router-dom": "^7.1.0"
  },
  "devDependencies": {
    "@testing-library/jest-dom": "^6.6.0",
    "@testing-library/react": "^16.1.0",
    "@testing-library/user-event": "^14.5.0",
    "@types/react": "^19.0.0",
    "@types/react-dom": "^19.0.0",
    "@vitejs/plugin-react": "^4.3.0",
    "jsdom": "^25.0.0",
    "typescript": "^5.7.0",
    "vite": "^6.0.0",
    "vitest": "^2.1.0"
  }
}
```

`web/vite.config.ts` — bygger till Go-paketets `dist`, proxar API i dev:

```ts
// defineConfig comes from vitest/config, not vite — the plain Vite export does
// not type the `test` block and tsc will reject it.
import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// Build output lands inside internal/observ/web/ because go:embed cannot read
// files outside its own package directory.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/observ/web/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
      '/status': 'http://localhost:8080',
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/test-setup.ts',
  },
});
```

`web/tsconfig.json`:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "module": "ESNext",
    "moduleResolution": "bundler",
    "jsx": "react-jsx",
    "strict": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "noFallthroughCasesInSwitch": true,
    "noEmit": true,
    "skipLibCheck": true,
    "types": ["vitest/globals", "@testing-library/jest-dom"]
  },
  "include": ["src"],
  "references": [{ "path": "./tsconfig.node.json" }]
}
```

`web/tsconfig.node.json`:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "bundler",
    "composite": true,
    "strict": true,
    "noEmit": true,
    "skipLibCheck": true
  },
  "include": ["vite.config.ts"]
}
```

- [ ] **Step 2: Skapa HTML-skal och rot**

`web/index.html` — typsnitten laddas här, som i dagens `dashboard.html`:

```html
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>slskdarr</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:wght@400;500;600;700&family=IBM+Plex+Mono:wght@400;500;600&display=swap" rel="stylesheet">
</head>
<body>
<div id="root"></div>
<script type="module" src="/src/main.tsx"></script>
</body>
</html>
```

`web/src/main.tsx`:

```tsx
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
```

`web/src/App.tsx` — minimal tills Task 8 bygger skalet:

```tsx
export default function App() {
  return <div>slskdarr</div>;
}
```

`web/src/test-setup.ts`:

```ts
import '@testing-library/jest-dom/vitest';
```

- [ ] **Step 3: Placeholder-dist och gitignore**

`internal/observ/web/dist/index.html` — incheckad, så `go:embed` alltid hittar minst
en fil:

```html
<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>slskdarr</title></head>
<body style="font-family:sans-serif;background:#0b0d10;color:#dfe2e8;padding:2rem;">
<h1>Frontend not built</h1>
<p>Run <code>make ui</code> to build the web interface, then restart slskdarr.</p>
</body>
</html>
```

Lägg till i `.gitignore`:

```gitignore
# Vite build output; only the checked-in placeholder index.html is tracked.
/internal/observ/web/dist/*
!/internal/observ/web/dist/index.html
/web/node_modules/
```

- [ ] **Step 4: Skapa Makefile**

```makefile
.PHONY: ui build dev test clean

ui:
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
	git checkout internal/observ/web/dist/index.html
```

- [ ] **Step 5: Verifiera att bygget fungerar**

Run: `make ui`
Expected: `npm ci` installerar, `vite build` skriver `index.html` och `assets/*` till
`internal/observ/web/dist/`. Kommandot avslutas med exit 0.

Run: `ls internal/observ/web/dist/`
Expected: `index.html` och katalogen `assets`.

- [ ] **Step 6: Commit**

```bash
git add web/ Makefile .gitignore internal/observ/web/dist/index.html
git commit -m "build(web): Vite + React + TypeScript-skelett och Makefile (#87)"
```

---

### Task 2: Asset-handler i Go

Ersätter `web.go`. Måste servera SPA-fallback utan att fånga API-sökvägar.

**Files:**
- Create: `internal/observ/assets.go`, `internal/observ/assets_test.go`
- Delete: `internal/observ/web.go`
- Modify: `internal/observ/observ.go:363-364`

**Interfaces:**
- Consumes: `internal/observ/web/dist/` från Task 1
- Produces: `newAssetHandler() http.Handler` — registreras på `/` i muxen

- [ ] **Step 1: Skriv de fallerande testerna**

`internal/observ/assets_test.go`:

```go
package observ

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssetHandlerServesIndexAtRoot(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	newAssetHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
}

// Client-side routes must return the SPA shell so deep links and reloads work.
func TestAssetHandlerServesIndexForClientRoutes(t *testing.T) {
	for _, path := range []string{"/jobs", "/jobs/42", "/health", "/settings", "/peers"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)

		newAssetHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}
}

// A mistyped API path must 404, never fall through to HTML — an HTML body in
// response to a fetch() is a hostile thing to debug.
func TestAssetHandlerDoesNotSwallowAPIPaths(t *testing.T) {
	for _, path := range []string{"/api/nope", "/api/jobs/1/bogus"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)

		newAssetHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestAssetHandlerCachesHashedAssetsImmutably(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/does-not-exist.js", nil)

	newAssetHandler().ServeHTTP(rec, req)

	// Missing hashed assets must 404 rather than returning the SPA shell.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
```

- [ ] **Step 2: Kör testerna, verifiera att de fallerar**

Run: `go test ./internal/observ/ -run TestAssetHandler -v`
Expected: FAIL — `undefined: newAssetHandler`

- [ ] **Step 3: Implementera handlern**

`internal/observ/assets.go`:

```go
// Package observ: assets.go embeds and serves the built single-page app.
// Vite writes its output to web/dist; go:embed cannot read outside this
// package's directory, which is why the build output lands here rather than
// next to the frontend sources in web/.
package observ

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:web/dist
var distFS embed.FS

// newAssetHandler serves the SPA: hashed assets are cached forever, every
// unknown path returns index.html so client-side routes survive a reload, and
// /api paths are never swallowed — a mistyped API path must 404, not return
// HTML that a fetch() would then fail to parse.
func newAssetHandler() http.Handler {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		panic("observ: dist subtree missing: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")

		if strings.HasPrefix(path, "api/") {
			http.NotFound(w, r)
			return
		}

		// Hashed bundles: serve directly, cache forever, 404 if absent.
		if strings.HasPrefix(path, "assets/") {
			if _, err := fs.Stat(sub, path); err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
			return
		}

		// Any other real file (favicon and friends) is served as-is.
		if path != "" {
			if _, err := fs.Stat(sub, path); err == nil {
				files.ServeHTTP(w, r)
				return
			}
		}

		serveIndex(w, sub)
	})
}

func serveIndex(w http.ResponseWriter, sub fs.FS) {
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
```

- [ ] **Step 4: Koppla in i muxen**

I `internal/observ/observ.go`, ersätt raderna:

```go
	mux.HandleFunc("/", dashboardHandler)
	mux.HandleFunc("/dashboard.js", dashboardJSHandler)
	return mux
```

med:

```go
	mux.Handle("/", newAssetHandler())
	return mux
```

- [ ] **Step 5: Radera den gamla handlern**

```bash
git rm internal/observ/web.go
```

- [ ] **Step 6: Kör testerna**

Run: `make ui && go test ./internal/observ/ -v`
Expected: PASS — samtliga tester, inklusive befintliga i `observ_test.go`,
`security_test.go`, `status_test.go`, `web_test.go`.

Om `web_test.go` refererar `dashboardHandler` eller `dashboardJSHandler`: uppdatera
de testerna till att gå via `newAssetHandler()`, eller radera dem om de enbart
testade att den gamla filen serverades.

- [ ] **Step 7: Commit**

```bash
git add internal/observ/
git commit -m "feat(observ): SPA-asset-handler med fallback och cache-headers (#87)"
```

---

### Task 3: Dockerfile med frontend-byggsteg

**Files:**
- Modify: `Dockerfile:1-7`

**Interfaces:**
- Consumes: `web/` och `make ui` från Task 1
- Produces: image där `dist` är byggd innan Go kompilerar

- [ ] **Step 1: Lägg till node-steget**

Ersätt rad 1–7 i `Dockerfile` med:

```dockerfile
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
```

Notera: `vite.config.ts` skriver till `../internal/observ/web/dist`, vilket från
`/web` blir `/internal/observ/web/dist` i node-steget.

- [ ] **Step 2: Bygg imagen**

Run: `docker build -t slskdarr:spa-test .`
Expected: Bygget lyckas. Node-steget kör `npm run build`; Go-steget kopierar in
`dist` och kompilerar.

- [ ] **Step 3: Verifiera att runtime-imagen inte innehåller Node**

Run: `docker run --rm --entrypoint /usr/local/bin/slskdarr slskdarr:spa-test --help`
Expected: binärens hjälptext, inget Node-relaterat i imagen (distroless har ingen
shell att inspektera med — att steget är `FROM gcr.io/distroless/static-debian12`
och bara kopierar binären räcker som garanti).

- [ ] **Step 4: Commit**

```bash
git add Dockerfile
git commit -m "build(docker): bygg frontend i eget steg före Go-kompilering (#87)"
```

---

### Task 4: Designtokens och global stil

**Files:**
- Create: `web/src/styles/tokens.css`, `web/src/styles/global.css`
- Modify: `web/src/main.tsx`

**Interfaces:**
- Produces: CSS-variabler tillgängliga globalt: `--bg`, `--panel`, `--border`,
  `--text`, `--text-muted`, `--accent`, `--active`, `--queued`, `--stalled`,
  `--orphaned`, `--done`, samt `-bg`-varianter och `--radius`, `--font-sans`,
  `--font-mono`.

- [ ] **Step 1: Skriv tokens**

`web/src/styles/tokens.css` — värdena kommer ur designprojektets `rootStyle`:

```css
:root {
  --bg: #0b0d10;
  --panel: #14171c;
  --panel-raised: #1a1f26;
  --border: #21252e;
  --border-subtle: #191d24;
  --text: #dfe2e8;
  --text-muted: #8a919d;
  --text-dim: #5f6672;

  --accent: #35c48f;
  --done: #35c48f;
  --done-bg: rgba(53, 196, 143, 0.13);
  --active: #4c8dff;
  --active-bg: rgba(76, 141, 255, 0.13);
  --queued: #a78bfa;
  --queued-bg: rgba(167, 139, 250, 0.13);
  --stalled: #e0a740;
  --stalled-bg: rgba(224, 167, 64, 0.13);
  --orphaned: #e5595d;
  --orphaned-bg: rgba(229, 89, 93, 0.13);
  --failed: #e5595d;
  --failed-bg: rgba(229, 89, 93, 0.13);

  --radius-sm: 8px;
  --radius: 11px;

  --font-sans: 'IBM Plex Sans', system-ui, sans-serif;
  --font-mono: 'IBM Plex Mono', ui-monospace, monospace;
}
```

- [ ] **Step 2: Skriv global stil**

`web/src/styles/global.css`:

```css
@import './tokens.css';

*, *::before, *::after { box-sizing: border-box; }

body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font-family: var(--font-sans);
  font-feature-settings: 'tnum' 1;
}

::selection { background: rgba(53, 196, 143, 0.3); }
::-webkit-scrollbar { width: 10px; height: 10px; }
::-webkit-scrollbar-thumb {
  background: #2a2e37;
  border-radius: 6px;
  border: 2px solid var(--bg);
}
::-webkit-scrollbar-thumb:hover { background: #363b45; }

input, select, button { font-family: inherit; }
```

- [ ] **Step 3: Importera i main.tsx**

Lägg till överst i `web/src/main.tsx`:

```tsx
import './styles/global.css';
```

- [ ] **Step 4: Verifiera**

Run: `cd web && npm run build`
Expected: Bygget lyckas, CSS bundlas.

- [ ] **Step 5: Commit**

```bash
git add web/src/styles/ web/src/main.tsx
git commit -m "feat(web): designtokens och global stil (#87)"
```

---

### Task 5: Strängkatalog och formatterare

Formatterarna måste replikera dagens beteende **exakt**, inklusive egenheter.

**Files:**
- Create: `web/src/strings.ts`, `web/src/format.ts`, `web/src/format.test.ts`

**Interfaces:**
- Produces:
  - `formatBytes(n: number | null | undefined): string`
  - `percent(done: number, total: number): number`
  - `formatDateTime(iso: string): string`
  - `formatTime(iso: string): string`
  - `formatShortTime(iso: string): string`
  - `formatScore(n: number): string`
  - `t` — strängkatalog, se nedan

- [ ] **Step 1: Skriv de fallerande testerna**

`web/src/format.test.ts`:

```ts
import { describe, expect, it } from 'vitest';
import { formatBytes, percent, formatDateTime, formatShortTime, formatScore } from './format';

describe('formatBytes', () => {
  it('returns "0 MB" for zero and nullish input', () => {
    expect(formatBytes(0)).toBe('0 MB');
    expect(formatBytes(null)).toBe('0 MB');
    expect(formatBytes(undefined)).toBe('0 MB');
  });

  it('formats megabytes with one decimal', () => {
    expect(formatBytes(1024 * 1024)).toBe('1.0 MB');
    expect(formatBytes(1536 * 1024)).toBe('1.5 MB');
  });

  // Matches the legacy dashboard: everything is expressed in MB, never scaled.
  it('does not scale to GB', () => {
    expect(formatBytes(5 * 1024 * 1024 * 1024)).toBe('5120.0 MB');
  });
});

describe('percent', () => {
  it('returns 0 when total is zero', () => {
    expect(percent(100, 0)).toBe(0);
  });

  it('rounds to nearest integer', () => {
    expect(percent(1, 3)).toBe(33);
    expect(percent(2, 3)).toBe(67);
  });

  // Deliberate change from the legacy dashboard, which let the bar overflow.
  it('clamps to 100', () => {
    expect(percent(150, 100)).toBe(100);
  });

  it('clamps negatives to 0', () => {
    expect(percent(-5, 100)).toBe(0);
  });
});

describe('date formatting', () => {
  // sv-SE is kept deliberately: ISO-like dates read better in a technical tool.
  it('formats a full timestamp in sv-SE', () => {
    expect(formatDateTime('2026-07-20T14:32:05Z')).toMatch(/2026-07-20/);
  });

  it('formats short time as HH:MM', () => {
    expect(formatShortTime('2026-07-20T14:32:05Z')).toMatch(/^\d{2}:\d{2}$/);
  });

  it('returns an em dash for empty input rather than "Invalid Date"', () => {
    expect(formatDateTime('')).toBe('—');
    expect(formatShortTime('')).toBe('—');
  });
});

describe('formatScore', () => {
  it('always shows two decimals', () => {
    expect(formatScore(1)).toBe('1.00');
    expect(formatScore(0.456)).toBe('0.46');
  });
});
```

- [ ] **Step 2: Kör testerna, verifiera att de fallerar**

Run: `cd web && npx vitest run src/format.test.ts`
Expected: FAIL — `Failed to resolve import "./format"`

- [ ] **Step 3: Implementera formatterarna**

`web/src/format.ts`:

```ts
// All user-facing formatting lives here so locale choices are made in one place.
// Dates use sv-SE regardless of UI language: ISO-like output (2026-07-20 14:32)
// is easier to scan in a technical tool than en-US ordering.
const DATE_LOCALE = 'sv-SE';

export function formatBytes(n: number | null | undefined): string {
  if (!n) return '0 MB';
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

export function percent(done: number, total: number): number {
  if (!total) return 0;
  const raw = Math.round((done / total) * 100);
  return Math.min(100, Math.max(0, raw));
}

export function formatDateTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString(DATE_LOCALE);
}

export function formatTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString(DATE_LOCALE);
}

export function formatShortTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString(DATE_LOCALE, { hour: '2-digit', minute: '2-digit' });
}

export function formatScore(n: number): string {
  return n.toFixed(2);
}
```

- [ ] **Step 4: Kör testerna**

Run: `cd web && npx vitest run src/format.test.ts`
Expected: PASS — samtliga tester.

- [ ] **Step 5: Skriv strängkatalogen**

`web/src/strings.ts` — översättning av samtliga svenska strängar i `dashboard.js`:

```ts
// Every user-facing string lives here. Components must never inline text.
// This is i18n preparation, not i18n — see #86.
export const t = {
  app: {
    name: 'slskdarr',
    tagline: 'Lidarr → Soulseek',
  },
  nav: {
    overview: 'Overview',
    jobs: 'Jobs',
    events: 'Events',
    peers: 'Peers',
    health: 'Health',
    settings: 'Settings',
  },
  status: {
    queued: 'Queued',
    active: 'Active',
    stalled: 'Stalled',
    done: 'Done',
    failed: 'Failed',
    orphaned: 'Orphaned',
  },
  state: {
    WANTED: 'Wanted',
    SELECTING: 'Selecting candidate',
    DOWNLOADING: 'Downloading',
    IMPORTING: 'Importing',
    DONE: 'Done',
    FAILED: 'Failed',
    CANCELLED: 'Cancelled',
  },
  candidateState: {
    NEW: 'Not tried',
    ACTIVE: 'In progress',
    SUCCEEDED: 'Succeeded',
    FAILED: 'Failed',
  },
  event: {
    search: 'Searched',
    search_fallback: 'Searched (fallback)',
    candidate_selected: 'Candidate selected',
    candidate_rejected: 'Candidate rejected',
    attempt_failed: 'Attempt failed',
    attempt_succeeded: 'Attempt succeeded',
    transfer_stalled: 'Transfer stalled',
    import_ok: 'Import completed',
    import_rejected: 'Import rejected',
    job_failed: 'Job failed',
  },
  columns: {
    album: 'Album / Artist',
    peer: 'Peer',
    progress: 'Progress',
    status: 'Status',
    id: 'ID',
    time: 'Time',
    job: 'Job',
    event: 'Event',
    detail: 'Detail',
    module: 'Module',
    lastRun: 'Last attempt / status',
    score: 'Score',
    succeeded: 'Succeeded',
    failed: 'Failed',
  },
  jobs: {
    searchPlaceholder: 'Search artist, album, peer…',
    allStatuses: 'All',
    empty: 'No jobs match the current filter.',
    back: '← Back',
    cancel: 'Cancel',
    retry: 'Retry',
    attemptHistory: 'Attempt history',
    events: 'Events',
    loading: 'Loading…',
    noAttempts: 'No attempts yet.',
    noEvents: 'No events.',
    cancelFailed: 'Could not cancel the job. It may already have finished.',
    retryFailed: 'Could not retry the job. Only failed jobs can be retried.',
    sleepingUntil: (time: string) => `Sleeping until ${time}`,
    candidates: (tried: number, max: number) => `${tried} of ${max} candidates tried`,
  },
  events: {
    filterPlaceholder: 'Filter events…',
    empty: 'No events.',
  },
  peers: {
    empty: 'No peers recorded yet.',
    noArtistHistory: 'No artist-specific history.',
    artistLine: (id: number, score: string, ok: number, fail: number) =>
      `Artist #${id} — score ${score}, ${ok} succeeded, ${fail} failed`,
  },
  health: {
    neverRun: 'Never run',
    consecutiveFailures: (n: number) => `${n} consecutive failures`,
  },
  settings: {
    readOnlyNotice:
      'Settings are read from the configuration file. Editing them here is planned — see issue #89.',
    lidarr: 'Lidarr',
    url: 'URL',
    apiKey: 'API key',
    apiKeyHidden: 'Configured (hidden)',
    apiKeyMissing: 'Not configured',
    reconcile: 'Reconcile',
    interval: 'Interval',
    concurrentDownloads: 'Concurrent downloads',
  },
} as const;
```

- [ ] **Step 6: Commit**

```bash
git add web/src/strings.ts web/src/format.ts web/src/format.test.ts
git commit -m "feat(web): strängkatalog och formatterare med sv-SE-datum (#87)"
```

---

### Task 6: API-typer och fetch-klient

**Files:**
- Create: `web/src/api/types.ts`, `web/src/api/client.ts`, `web/src/api/client.test.ts`

**Interfaces:**
- Produces:
  - `apiGet<T>(path: string): Promise<T>`
  - `apiPost(path: string): Promise<void>` — kastar `ApiError` vid icke-2xx
  - `ApiError` med `status: number`
  - Typerna `Job`, `JobDetail`, `AttemptDetail`, `TransferDetail`, `JobEvent`, `Peer`, `PeerArtist`, `StatusReport`, `ModuleStatus`, `AppConfig`

- [ ] **Step 1: Skriv typerna**

`web/src/api/types.ts` — speglar Go-DTO:erna exakt. Alla tidsfält är RFC 3339-strängar
(`2006-01-02T15:04:05Z07:00`), tomma när värdet saknas.

```ts
// Hand-written mirrors of the Go DTOs in internal/observ. Kept in one file so
// drift has a single place to be caught. See spec 2026-07-20.

export type JobStatus = 'queued' | 'active' | 'stalled' | 'done' | 'failed' | 'orphaned';
export type JobState =
  | 'WANTED' | 'SELECTING' | 'DOWNLOADING' | 'IMPORTING'
  | 'DONE' | 'FAILED' | 'CANCELLED';
export type CandidateState = 'NEW' | 'ACTIVE' | 'SUCCEEDED' | 'FAILED';

/** GET /api/jobs — internal/observ/observ.go jobDTO */
export interface Job {
  id: number;
  title: string;
  artist: string;
  status: JobStatus;
  peer: string;
  bytesDone: number;
  bytesTotal: number;
  updatedAt: string;
  state: JobState;
  candidatesTried: number;
  maxCandidates: number;
  failReason: string;
  nextAttemptAt: string;
  retries: number;
  notBefore: string;
}

/** internal/observ/jobdetail.go transferDetailDTO */
export interface TransferDetail {
  filename: string;
  state: string;
  bytesDone: number;
  bytesTotal: number;
  retries: number;
  lastProgressAt: string;
}

/** internal/observ/jobdetail.go attemptDetailDTO */
export interface AttemptDetail {
  id: number;
  username: string;
  fileCount: number;
  state: CandidateState;
  failReason: string;
  createdAt: string;
  updatedAt: string;
  transfers: TransferDetail[];
}

/** GET /api/jobs/{id}/detail — jobDetailDTO */
export interface JobDetail {
  id: number;
  title: string;
  artist: string;
  state: JobState;
  attempts: AttemptDetail[];
}

/** GET /api/events and /api/jobs/{id}/events — eventDTO */
export interface JobEvent {
  id: number;
  jobId: number;
  event: string;
  detail: string;
  createdAt: string;
}

/** internal/observ/peers.go peerArtistDTO */
export interface PeerArtist {
  artistId: number;
  successCount: number;
  failCount: number;
  lastSuccessAt: string;
  lastFailAt: string;
  score: number;
}

/** GET /api/peers — peerDTO */
export interface Peer {
  username: string;
  successCount: number;
  failCount: number;
  lastSuccessAt: string;
  lastFailAt: string;
  score: number;
  artists: PeerArtist[];
}

/** internal/observ/observ.go moduleStatusDTO */
export interface ModuleStatus {
  lastAttempt: string;
  lastCompleted: string;
  lastSuccess: string;
  lastErrorAt: string;
  lastError: string;
  consecutiveFailures: number;
  staleDeadline: string;
  live: boolean;
  ready: boolean;
}

/** GET /status */
export interface StatusReport {
  queued: number;
  active: number;
  stalled: number;
  orphaned: number;
  modules: Record<string, string>;
  moduleDetails: Record<string, ModuleStatus>;
}

/** GET /api/config — see Task 14. Secrets are never sent, only their presence. */
export interface AppConfig {
  lidarrUrl: string;
  lidarrApiKeyConfigured: boolean;
  reconcileInterval: string;
  maxConcurrentDownloads: number;
}
```

- [ ] **Step 2: Skriv de fallerande testerna för klienten**

`web/src/api/client.test.ts`:

```ts
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiGet, apiPost } from './client';

afterEach(() => vi.unstubAllGlobals());

describe('apiGet', () => {
  it('returns parsed JSON on success', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      new Response(JSON.stringify([{ id: 1 }]), { status: 200 }),
    ));
    await expect(apiGet('/api/jobs')).resolves.toEqual([{ id: 1 }]);
  });

  it('throws ApiError carrying the status on failure', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('nope', { status: 500 })));
    await expect(apiGet('/api/jobs')).rejects.toBeInstanceOf(ApiError);
  });
});

describe('apiPost', () => {
  it('resolves on 204', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    await expect(apiPost('/api/jobs/1/cancel')).resolves.toBeUndefined();
  });

  // The legacy dashboard ignored failures here entirely; we surface them.
  it('throws ApiError with the status on 409', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('conflict', { status: 409 })));
    await expect(apiPost('/api/jobs/1/retry')).rejects.toMatchObject({ status: 409 });
  });
});
```

- [ ] **Step 3: Kör testerna, verifiera att de fallerar**

Run: `cd web && npx vitest run src/api/client.test.ts`
Expected: FAIL — `Failed to resolve import "./client"`

- [ ] **Step 4: Implementera klienten**

`web/src/api/client.ts`:

```ts
// Auth needs no handling here: the server answers with WWW-Authenticate Basic,
// the browser then attaches ambient credentials to every same-origin request.
// See internal/observ/security.go.

export class ApiError extends Error {
  constructor(readonly status: number, message: string) {
    super(message);
    this.name = 'ApiError';
  }
}

export async function apiGet<T>(path: string): Promise<T> {
  const res = await fetch(path);
  if (!res.ok) {
    throw new ApiError(res.status, `GET ${path} failed with ${res.status}`);
  }
  return (await res.json()) as T;
}

export async function apiPost(path: string): Promise<void> {
  const res = await fetch(path, { method: 'POST' });
  if (!res.ok) {
    throw new ApiError(res.status, `POST ${path} failed with ${res.status}`);
  }
}
```

- [ ] **Step 5: Kör testerna**

Run: `cd web && npx vitest run src/api/client.test.ts`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add web/src/api/
git commit -m "feat(web): API-typer och fetch-klient med felhantering (#87)"
```

---

### Task 7: Query-hooks

Polling-intervallen matchar dagens dashboard exakt: jobb och händelser 3 s, status
och peers 5 s.

**Files:**
- Create: `web/src/api/queries.ts`
- Modify: `web/src/App.tsx`

**Interfaces:**
- Produces: `useJobs()`, `useStatus()`, `useEvents()`, `usePeers()`,
  `useJobDetail(id)`, `useJobEvents(id)`, `useConfig()`, `useCancelJob()`,
  `useRetryJob()`, `queryKeys`

- [ ] **Step 1: Implementera hooks**

`web/src/api/queries.ts`:

```ts
import {
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { apiGet, apiPost } from './client';
import type { AppConfig, Job, JobDetail, JobEvent, Peer, StatusReport } from './types';

// Intervals match the legacy dashboard exactly so perceived freshness is
// unchanged by the migration.
const JOBS_INTERVAL = 3000;
const EVENTS_INTERVAL = 3000;
const STATUS_INTERVAL = 5000;
const PEERS_INTERVAL = 5000;

export const queryKeys = {
  jobs: ['jobs'] as const,
  status: ['status'] as const,
  events: ['events'] as const,
  peers: ['peers'] as const,
  config: ['config'] as const,
  jobDetail: (id: number) => ['jobs', id, 'detail'] as const,
  jobEvents: (id: number) => ['jobs', id, 'events'] as const,
};

export function useJobs() {
  return useQuery({
    queryKey: queryKeys.jobs,
    queryFn: () => apiGet<Job[]>('/api/jobs'),
    refetchInterval: JOBS_INTERVAL,
  });
}

export function useStatus() {
  return useQuery({
    queryKey: queryKeys.status,
    queryFn: () => apiGet<StatusReport>('/status'),
    refetchInterval: STATUS_INTERVAL,
  });
}

export function useEvents() {
  return useQuery({
    queryKey: queryKeys.events,
    queryFn: () => apiGet<JobEvent[]>('/api/events?limit=200'),
    refetchInterval: EVENTS_INTERVAL,
  });
}

export function usePeers() {
  return useQuery({
    queryKey: queryKeys.peers,
    queryFn: () => apiGet<Peer[]>('/api/peers'),
    refetchInterval: PEERS_INTERVAL,
  });
}

// Query keys include the job id, so a slow response for a previously viewed job
// can never overwrite the current one — this replaces the legacy dashboard's
// manual `detailJobId === id` guard.
export function useJobDetail(id: number) {
  return useQuery({
    queryKey: queryKeys.jobDetail(id),
    queryFn: () => apiGet<JobDetail>(`/api/jobs/${id}/detail`),
    refetchInterval: JOBS_INTERVAL,
  });
}

export function useJobEvents(id: number) {
  return useQuery({
    queryKey: queryKeys.jobEvents(id),
    queryFn: () => apiGet<JobEvent[]>(`/api/jobs/${id}/events`),
    refetchInterval: JOBS_INTERVAL,
  });
}

export function useConfig() {
  return useQuery({
    queryKey: queryKeys.config,
    queryFn: () => apiGet<AppConfig>('/api/config'),
    staleTime: Infinity, // config only changes when the file changes
  });
}

export function useCancelJob(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiPost(`/api/jobs/${id}/cancel`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.jobs });
      void qc.invalidateQueries({ queryKey: queryKeys.jobDetail(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.jobEvents(id) });
    },
  });
}

export function useRetryJob(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiPost(`/api/jobs/${id}/retry`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.jobs });
      // Retry wipes candidate history server-side, so the cached detail is stale.
      void qc.invalidateQueries({ queryKey: queryKeys.jobDetail(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.jobEvents(id) });
    },
  });
}
```

- [ ] **Step 2: Konfigurera QueryClient med "behåll senast kända data"**

Ersätt `web/src/App.tsx`:

```tsx
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

// The legacy dashboard never blanked the UI on a failed poll — it kept showing
// the last good response. keepPreviousData-style placeholderData plus a
// non-throwing error state reproduces that: `data` survives a failed refetch.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      placeholderData: (previous: unknown) => previous,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <div>slskdarr</div>
    </QueryClientProvider>
  );
}
```

- [ ] **Step 3: Verifiera att det bygger**

Run: `cd web && npm run build`
Expected: `tsc -b` passerar utan typfel, Vite bygger.

- [ ] **Step 4: Commit**

```bash
git add web/src/api/queries.ts web/src/App.tsx
git commit -m "feat(web): TanStack Query-hooks med polling och invalidering (#87)"
```

---

### Task 8: Appskal, sidebar och routing

**Files:**
- Create: `web/src/components/Layout.tsx`, `web/src/components/Layout.module.css`
- Modify: `web/src/App.tsx`

**Interfaces:**
- Consumes: `t` (Task 5), tokens (Task 4)
- Produces: `<Layout>` med sidebar och `<Outlet />`. Routes: `/`, `/jobs`,
  `/jobs/:id`, `/events`, `/peers`, `/health`, `/settings`

- [ ] **Step 1: Skriv Layout**

`web/src/components/Layout.tsx`:

```tsx
import { NavLink, Outlet } from 'react-router-dom';
import { t } from '../strings';
import styles from './Layout.module.css';

const NAV = [
  { to: '/', label: t.nav.overview, end: true },
  { to: '/jobs', label: t.nav.jobs, end: false },
  { to: '/events', label: t.nav.events, end: false },
  { to: '/peers', label: t.nav.peers, end: false },
  { to: '/health', label: t.nav.health, end: false },
  { to: '/settings', label: t.nav.settings, end: false },
];

export default function Layout() {
  return (
    <div className={styles.app}>
      <aside className={styles.sidebar}>
        <div className={styles.brand}>
          <div className={styles.brandMark}>sl</div>
          <div>
            <div className={styles.brandName}>{t.app.name}</div>
            <div className={styles.brandTagline}>{t.app.tagline}</div>
          </div>
        </div>
        <nav className={styles.nav}>
          {NAV.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => (isActive ? styles.navItemActive : styles.navItem)}
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
      </aside>
      <main className={styles.main}>
        <Outlet />
      </main>
    </div>
  );
}
```

`web/src/components/Layout.module.css`:

```css
.app { display: flex; min-height: 100vh; }

.sidebar {
  width: 236px;
  flex: 0 0 236px;
  background: #101317;
  border-right: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  height: 100vh;
  position: sticky;
  top: 0;
}

.brand { display: flex; align-items: center; gap: 11px; padding: 18px 18px 16px; }

.brandMark {
  width: 34px; height: 34px;
  border-radius: 9px;
  background: var(--accent);
  display: flex; align-items: center; justify-content: center;
  font-family: var(--font-mono);
  font-weight: 600; font-size: 14px;
  color: #08130d;
  letter-spacing: -0.5px;
}

.brandName { font-weight: 600; }
.brandTagline { font-size: 11px; color: var(--text-dim); }

.nav { padding: 6px 12px; display: flex; flex-direction: column; gap: 2px; }

.navItem, .navItemActive {
  display: flex; align-items: center;
  padding: 9px 12px;
  border-radius: var(--radius-sm);
  font-size: 13px;
  text-decoration: none;
  transition: background 0.12s;
}

.navItem { color: var(--text-muted); font-weight: 500; }
.navItem:hover { background: rgba(255, 255, 255, 0.03); }

.navItemActive {
  background: var(--panel-raised);
  color: #eef0f3;
  font-weight: 600;
  box-shadow: inset 2px 0 0 var(--accent);
}

.main { flex: 1; min-width: 0; padding: 22px 26px 48px; }
```

- [ ] **Step 2: Koppla in routern**

`web/src/App.tsx`:

```tsx
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter, Route, Routes } from 'react-router-dom';
import Layout from './components/Layout';
import Overview from './routes/Overview';
import Jobs from './routes/Jobs';
import JobDetail from './routes/JobDetail';
import Events from './routes/Events';
import Peers from './routes/Peers';
import Health from './routes/Health';
import Settings from './routes/Settings';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      placeholderData: (previous: unknown) => previous,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <Routes>
          <Route element={<Layout />}>
            <Route index element={<Overview />} />
            <Route path="jobs" element={<Jobs />} />
            <Route path="jobs/:id" element={<JobDetail />} />
            <Route path="events" element={<Events />} />
            <Route path="peers" element={<Peers />} />
            <Route path="health" element={<Health />} />
            <Route path="settings" element={<Settings />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </QueryClientProvider>
  );
}
```

- [ ] **Step 3: Skapa stubbar för alla routes så bygget går igenom**

Skapa sju filer i `web/src/routes/` med samma mönster, en per vy. Exempel
`web/src/routes/Overview.tsx`:

```tsx
export default function Overview() {
  return <h1>Overview</h1>;
}
```

Upprepa för `Jobs.tsx`, `JobDetail.tsx`, `Events.tsx`, `Peers.tsx`, `Health.tsx`,
`Settings.tsx` med respektive rubrik. De ersätts i Task 10–15.

- [ ] **Step 4: Verifiera**

Run: `cd web && npm run build`
Expected: Bygget lyckas.

- [ ] **Step 5: Commit**

```bash
git add web/src/
git commit -m "feat(web): appskal med sidebar och routing (#87)"
```

---

### Task 9: Delade komponenter

**Files:**
- Create: `web/src/components/StatusPill.tsx` + `.module.css`,
  `web/src/components/ProgressBar.tsx` + `.module.css`,
  `web/src/components/StatCard.tsx` + `.module.css`,
  `web/src/components/Table.module.css`,
  `web/src/components/PageHeading.tsx`,
  `web/src/components/StatusPill.test.tsx`

**Interfaces:**
- Produces:
  - `<StatusPill status={JobStatus} state={JobState} />`
  - `<ProgressBar done={number} total={number} />`
  - `<StatCard label={string} value={number} />`
  - `<PageHeading>{children}</PageHeading>`
  - `Table.module.css` med klasserna `table`, `th`, `td`, `row`, `rowClickable`, `empty`

- [ ] **Step 1: Skriv det fallerande testet för StatusPill**

`web/src/components/StatusPill.test.tsx`:

```tsx
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import StatusPill from './StatusPill';

describe('StatusPill', () => {
  it('shows the translated state label', () => {
    render(<StatusPill status="active" state="DOWNLOADING" />);
    expect(screen.getByText('Downloading')).toBeInTheDocument();
  });

  // Legacy behaviour: an unrecognised state falls back to the coarser status
  // field, not to the raw state string. See dashboard.js:213.
  it('falls back to the status label for an unknown state', () => {
    render(<StatusPill status="queued" state={'FUTURE_STATE' as never} />);
    expect(screen.getByText('Queued')).toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Kör testet, verifiera att det fallerar**

Run: `cd web && npx vitest run src/components/StatusPill.test.tsx`
Expected: FAIL — `Failed to resolve import "./StatusPill"`

- [ ] **Step 3: Implementera StatusPill**

`web/src/components/StatusPill.tsx`:

```tsx
import type { JobState, JobStatus } from '../api/types';
import { t } from '../strings';
import styles from './StatusPill.module.css';

interface Props {
  status: JobStatus;
  state: JobState;
}

// An unknown state degrades to the coarser status label rather than showing a
// raw enum string — this two-level fallback is inherited from the legacy
// dashboard and is deliberate.
export default function StatusPill({ status, state }: Props) {
  const label = t.state[state] ?? t.status[status] ?? status;
  return <span className={`${styles.pill} ${styles[status] ?? ''}`}>{label}</span>;
}
```

`web/src/components/StatusPill.module.css`:

```css
.pill {
  display: inline-block;
  padding: 3px 9px;
  border-radius: 20px;
  font-size: 11px;
  font-weight: 600;
}

.queued { background: var(--queued-bg); color: var(--queued); }
.active { background: var(--active-bg); color: var(--active); }
.stalled { background: var(--stalled-bg); color: var(--stalled); }
.done { background: var(--done-bg); color: var(--done); }
.failed { background: var(--failed-bg); color: var(--failed); }
.orphaned { background: var(--orphaned-bg); color: var(--orphaned); }
```

- [ ] **Step 4: Kör testet**

Run: `cd web && npx vitest run src/components/StatusPill.test.tsx`
Expected: PASS

- [ ] **Step 5: Implementera övriga komponenter**

`web/src/components/ProgressBar.tsx`:

```tsx
import { percent } from '../format';
import styles from './ProgressBar.module.css';

export default function ProgressBar({ done, total }: { done: number; total: number }) {
  return (
    <div className={styles.bar}>
      <div className={styles.fill} style={{ width: `${percent(done, total)}%` }} />
    </div>
  );
}
```

`web/src/components/ProgressBar.module.css`:

```css
.bar { height: 5px; border-radius: 3px; background: #22262e; overflow: hidden; }
.fill { height: 100%; background: var(--accent); transition: width 0.3s; }
```

`web/src/components/StatCard.tsx`:

```tsx
import styles from './StatCard.module.css';

export default function StatCard({ label, value }: { label: string; value: number }) {
  return (
    <div className={styles.card}>
      <div className={styles.label}>{label}</div>
      <div className={styles.value}>{value}</div>
    </div>
  );
}
```

`web/src/components/StatCard.module.css`:

```css
.card {
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 14px 15px;
}
.label { font-size: 12px; color: var(--text-muted); }
.value { font-family: var(--font-mono); font-size: 28px; font-weight: 600; margin-top: 6px; }
```

`web/src/components/PageHeading.tsx`:

```tsx
import type { ReactNode } from 'react';

export default function PageHeading({ children }: { children: ReactNode }) {
  return <h1 style={{ margin: '0 0 12px', fontSize: 18, fontWeight: 600 }}>{children}</h1>;
}
```

`web/src/components/Table.module.css`:

```css
.table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12.5px;
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  overflow: hidden;
}

.th {
  text-align: left;
  padding: 9px 14px;
  font-size: 11px;
  text-transform: uppercase;
  color: #6b7280;
  border-bottom: 1px solid var(--border);
}

.thSortable { cursor: pointer; user-select: none; }
.thSortable:hover { color: #a3aab5; }

.td { padding: 10px 14px; border-bottom: 1px solid var(--border-subtle); }

.rowClickable { cursor: pointer; }
.rowClickable:hover { background: #181c22; }

.empty { padding: 20px 14px; color: var(--text-muted); font-size: 12.5px; }

.mono { font-family: var(--font-mono); }
```

- [ ] **Step 6: Kör hela testsviten och bygget**

Run: `cd web && npm test && npm run build`
Expected: PASS, bygget lyckas.

- [ ] **Step 7: Commit**

```bash
git add web/src/components/
git commit -m "feat(web): delade komponenter — pill, progress, statuskort, tabellstil (#87)"
```

---

### Task 10: Översiktsvyn

Charts utgår (#88). Vyn består av statuskort, aktiva jobb och modulhälsa.

**Files:**
- Modify: `web/src/routes/Overview.tsx`
- Create: `web/src/routes/Overview.module.css`

**Interfaces:**
- Consumes: `useJobs`, `useStatus` (Task 7), `StatCard`, `ProgressBar`, `Table.module.css` (Task 9)

- [ ] **Step 1: Implementera vyn**

`web/src/routes/Overview.tsx`:

```tsx
import { useNavigate } from 'react-router-dom';
import { useJobs } from '../api/queries';
import type { JobStatus } from '../api/types';
import PageHeading from '../components/PageHeading';
import ProgressBar from '../components/ProgressBar';
import StatCard from '../components/StatCard';
import table from '../components/Table.module.css';
import { formatBytes } from '../format';
import { t } from '../strings';
import styles from './Overview.module.css';

// The legacy dashboard omitted the failed card even though it counted the
// status; showing it is a deliberate fix (#87).
const CARDS: JobStatus[] = ['queued', 'active', 'stalled', 'done', 'failed'];

export default function Overview() {
  const navigate = useNavigate();
  const { data: jobs = [] } = useJobs();

  const counts = Object.fromEntries(
    CARDS.map((s) => [s, jobs.filter((j) => j.status === s).length]),
  ) as Record<JobStatus, number>;

  // Overview deliberately ignores the Jobs view's search and status filters.
  const active = jobs.filter((j) => j.status === 'active');

  return (
    <>
      <PageHeading>{t.nav.overview}</PageHeading>

      <div className={styles.cards}>
        {CARDS.map((s) => (
          <StatCard key={s} label={t.status[s]} value={counts[s]} />
        ))}
      </div>

      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.album}</th>
            <th className={table.th}>{t.columns.peer}</th>
            <th className={table.th}>{t.columns.progress}</th>
          </tr>
        </thead>
        <tbody>
          {active.length === 0 ? (
            <tr>
              <td className={table.empty} colSpan={3}>{t.jobs.empty}</td>
            </tr>
          ) : (
            active.map((j) => (
              <tr
                key={j.id}
                className={table.rowClickable}
                onClick={() => navigate(`/jobs/${j.id}`)}
              >
                <td className={table.td}>
                  <div>{j.title}</div>
                  <div className={styles.sub}>{j.artist}</div>
                </td>
                <td className={table.td}>{j.peer || '—'}</td>
                <td className={table.td}>
                  <div className={styles.bytes}>
                    {formatBytes(j.bytesDone)} / {formatBytes(j.bytesTotal)}
                  </div>
                  <ProgressBar done={j.bytesDone} total={j.bytesTotal} />
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </>
  );
}
```

`web/src/routes/Overview.module.css`:

```css
.cards {
  display: grid;
  grid-template-columns: repeat(5, 1fr);
  gap: 13px;
  margin-bottom: 16px;
}

.sub { font-size: 11px; color: var(--text-muted); margin-top: 2px; }
.bytes { font-size: 11px; color: #7c828d; margin-bottom: 3px; }
```

- [ ] **Step 2: Verifiera**

Run: `cd web && npm run build`
Expected: Bygget lyckas.

- [ ] **Step 3: Commit**

```bash
git add web/src/routes/Overview.tsx web/src/routes/Overview.module.css
git commit -m "feat(web): översiktsvy med statuskort och aktiva jobb (#87)"
```

---

### Task 11: Jobbvyn med sökning och statusfilter

Filtersemantiken måste matcha dagens exakt: skiftlägesokänslig delsträngsmatchning
över `#id`, titel, artist och peer, kombinerad med exakt statusmatchning.

**Files:**
- Modify: `web/src/routes/Jobs.tsx`
- Create: `web/src/routes/Jobs.module.css`, `web/src/routes/jobFilter.ts`, `web/src/routes/jobFilter.test.ts`

**Interfaces:**
- Produces: `matchesFilters(job: Job, search: string, status: string): boolean`

- [ ] **Step 1: Skriv de fallerande testerna för filtret**

`web/src/routes/jobFilter.test.ts`:

```ts
import { describe, expect, it } from 'vitest';
import type { Job } from '../api/types';
import { matchesFilters } from './jobFilter';

const job = {
  id: 42,
  title: 'Kind of Blue',
  artist: 'Miles Davis',
  peer: 'someuser',
  status: 'active',
} as Job;

describe('matchesFilters', () => {
  it('matches everything when both filters are empty', () => {
    expect(matchesFilters(job, '', '')).toBe(true);
  });

  it('matches case-insensitively across title, artist and peer', () => {
    expect(matchesFilters(job, 'miles', '')).toBe(true);
    expect(matchesFilters(job, 'BLUE', '')).toBe(true);
    expect(matchesFilters(job, 'someuser', '')).toBe(true);
  });

  // The id is searchable with its # prefix, as in the legacy dashboard.
  it('matches the id including the hash prefix', () => {
    expect(matchesFilters(job, '#42', '')).toBe(true);
    expect(matchesFilters(job, '42', '')).toBe(true);
  });

  it('treats the search term as one substring, not as separate words', () => {
    expect(matchesFilters(job, 'Blue Miles', '')).toBe(false);
  });

  it('matches status exactly', () => {
    expect(matchesFilters(job, '', 'active')).toBe(true);
    expect(matchesFilters(job, '', 'queued')).toBe(false);
  });

  it('requires both filters to match', () => {
    expect(matchesFilters(job, 'miles', 'queued')).toBe(false);
  });
});
```

- [ ] **Step 2: Kör testerna, verifiera att de fallerar**

Run: `cd web && npx vitest run src/routes/jobFilter.test.ts`
Expected: FAIL — `Failed to resolve import "./jobFilter"`

- [ ] **Step 3: Implementera filtret**

`web/src/routes/jobFilter.ts`:

```ts
import type { Job } from '../api/types';

// Semantics preserved from the legacy dashboard: the whole search term is one
// case-insensitive substring matched against a haystack of "#id title artist
// peer". No tokenisation, no per-field targeting.
export function matchesFilters(job: Job, search: string, status: string): boolean {
  if (status && job.status !== status) return false;
  if (!search) return true;

  const haystack = `#${job.id} ${job.title} ${job.artist} ${job.peer}`.toLowerCase();
  return haystack.includes(search.toLowerCase());
}
```

- [ ] **Step 4: Kör testerna**

Run: `cd web && npx vitest run src/routes/jobFilter.test.ts`
Expected: PASS

- [ ] **Step 5: Implementera vyn**

`web/src/routes/Jobs.tsx`:

```tsx
import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useJobs } from '../api/queries';
import type { JobStatus } from '../api/types';
import PageHeading from '../components/PageHeading';
import ProgressBar from '../components/ProgressBar';
import StatusPill from '../components/StatusPill';
import table from '../components/Table.module.css';
import { formatBytes } from '../format';
import { t } from '../strings';
import { matchesFilters } from './jobFilter';
import styles from './Jobs.module.css';

const STATUSES: JobStatus[] = ['queued', 'active', 'stalled', 'done', 'failed'];

export default function Jobs() {
  const navigate = useNavigate();
  const { data: jobs = [] } = useJobs();
  const [search, setSearch] = useState('');
  const [status, setStatus] = useState('');

  const filtered = jobs.filter((j) => matchesFilters(j, search, status));

  return (
    <>
      <PageHeading>{t.nav.jobs}</PageHeading>

      <div className={styles.controls}>
        <input
          className={styles.input}
          type="text"
          placeholder={t.jobs.searchPlaceholder}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <select
          className={styles.select}
          value={status}
          onChange={(e) => setStatus(e.target.value)}
        >
          <option value="">{t.jobs.allStatuses}</option>
          {STATUSES.map((s) => (
            <option key={s} value={s}>{t.status[s]}</option>
          ))}
        </select>
      </div>

      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.id}</th>
            <th className={table.th}>{t.columns.status}</th>
            <th className={table.th}>{t.columns.album}</th>
            <th className={table.th}>{t.columns.peer}</th>
            <th className={table.th}>{t.columns.progress}</th>
          </tr>
        </thead>
        <tbody>
          {filtered.length === 0 ? (
            <tr>
              <td className={table.empty} colSpan={5}>{t.jobs.empty}</td>
            </tr>
          ) : (
            filtered.map((j) => (
              <tr
                key={j.id}
                className={table.rowClickable}
                onClick={() => navigate(`/jobs/${j.id}`)}
              >
                <td className={`${table.td} ${table.mono}`}>#{j.id}</td>
                <td className={table.td}><StatusPill status={j.status} state={j.state} /></td>
                <td className={table.td}>
                  <div>{j.title}</div>
                  <div className={styles.sub}>{j.artist}</div>
                </td>
                <td className={table.td}>{j.peer || '—'}</td>
                <td className={table.td}>
                  {j.bytesTotal > 0 && (
                    <div className={styles.bytes}>
                      {formatBytes(j.bytesDone)} / {formatBytes(j.bytesTotal)}
                    </div>
                  )}
                  <ProgressBar done={j.bytesDone} total={j.bytesTotal} />
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </>
  );
}
```

`web/src/routes/Jobs.module.css`:

```css
.controls { display: flex; gap: 8px; margin-bottom: 12px; }

.input, .select {
  padding: 8px 10px;
  border-radius: var(--radius-sm);
  background: #13161b;
  border: 1px solid #23272f;
  color: var(--text);
  font-size: 12.5px;
}

.input { width: 260px; }
.sub { font-size: 11px; color: var(--text-muted); margin-top: 2px; }
.bytes { font-size: 11px; color: #7c828d; margin-bottom: 3px; }
```

- [ ] **Step 6: Kör tester och bygg**

Run: `cd web && npm test && npm run build`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add web/src/routes/Jobs.tsx web/src/routes/Jobs.module.css web/src/routes/jobFilter.ts web/src/routes/jobFilter.test.ts
git commit -m "feat(web): jobbvy med sökning och statusfilter (#87)"
```

---

### Task 12: Jobbdetaljvyn

Den mest beteendetäta vyn. Tre saker måste bevaras: tre-nivåers fallback i headern,
att retry-knappen visas baserat på både live-jobbet och det cachade detaljsvaret,
och att `notBefore` i det förflutna inte visas.

**Files:**
- Modify: `web/src/routes/JobDetail.tsx`
- Create: `web/src/routes/JobDetail.module.css`

**Interfaces:**
- Consumes: `useJobs`, `useJobDetail`, `useJobEvents`, `useCancelJob`, `useRetryJob`

- [ ] **Step 1: Implementera vyn**

`web/src/routes/JobDetail.tsx`:

```tsx
import { Link, useParams } from 'react-router-dom';
import {
  useCancelJob,
  useJobDetail,
  useJobEvents,
  useJobs,
  useRetryJob,
} from '../api/queries';
import PageHeading from '../components/PageHeading';
import StatusPill from '../components/StatusPill';
import table from '../components/Table.module.css';
import { formatBytes, formatDateTime, formatShortTime } from '../format';
import { t } from '../strings';
import styles from './JobDetail.module.css';

export default function JobDetail() {
  const id = Number(useParams().id);
  const { data: jobs = [] } = useJobs();
  const { data: detail, isLoading: detailLoading } = useJobDetail(id);
  const { data: events, isLoading: eventsLoading } = useJobEvents(id);
  const cancel = useCancelJob(id);
  const retry = useRetryJob(id);

  const job = jobs.find((j) => j.id === id);

  // Retry is offered when either source reports FAILED — the polled list and
  // the detail response can disagree briefly, and both are authoritative enough.
  const isFailed = job?.state === 'FAILED' || detail?.state === 'FAILED';

  // A notBefore in the past has no display relevance.
  const sleepingUntil =
    job?.notBefore && new Date(job.notBefore) > new Date() ? job.notBefore : '';

  return (
    <>
      <Link to="/jobs" className={styles.back}>{t.jobs.back}</Link>

      {/* Three-tier fallback: live job, then cached detail, then loading. The
          middle tier keeps the page useful after a job ages out of /api/jobs. */}
      {job ? (
        <>
          <PageHeading>{job.title}</PageHeading>
          <div className={styles.meta}>
            <StatusPill status={job.status} state={job.state} />
            <span>{job.artist}</span>
            <span>{job.peer || '—'}</span>
            <span className={table.mono}>
              {formatBytes(job.bytesDone)} / {formatBytes(job.bytesTotal)}
            </span>
            {sleepingUntil && (
              <span className={styles.sleeping}>
                {t.jobs.sleepingUntil(formatShortTime(sleepingUntil))}
              </span>
            )}
          </div>
          {(job.status === 'queued' || job.status === 'failed') && job.maxCandidates > 0 && (
            <div className={styles.candidates}>
              {t.jobs.candidates(job.candidatesTried, job.maxCandidates)}
            </div>
          )}
          {job.failReason && <div className={styles.failReason}>{job.failReason}</div>}
        </>
      ) : detail ? (
        <>
          <PageHeading>{detail.title}</PageHeading>
          <div className={styles.meta}>
            <span>{detail.artist}</span>
            <span>{t.state[detail.state] ?? detail.state}</span>
          </div>
        </>
      ) : (
        <PageHeading>{t.jobs.loading}</PageHeading>
      )}

      <div className={styles.actions}>
        <button
          className={styles.action}
          disabled={cancel.isPending}
          onClick={() => cancel.mutate()}
        >
          {t.jobs.cancel}
        </button>
        {isFailed && (
          <button
            className={styles.action}
            disabled={retry.isPending}
            onClick={() => retry.mutate()}
          >
            {t.jobs.retry}
          </button>
        )}
      </div>

      {/* The legacy dashboard silently ignored failed actions; we surface them. */}
      {cancel.isError && <div className={styles.error}>{t.jobs.cancelFailed}</div>}
      {retry.isError && <div className={styles.error}>{t.jobs.retryFailed}</div>}

      <h2 className={styles.section}>{t.jobs.attemptHistory}</h2>
      {detailLoading && !detail ? (
        <div className={styles.placeholder}>{t.jobs.loading}</div>
      ) : !detail?.attempts.length ? (
        <div className={styles.placeholder}>{t.jobs.noAttempts}</div>
      ) : (
        detail.attempts.map((a) => (
          <div key={a.id} className={styles.attempt}>
            <div>
              <strong>{a.username}</strong>{' '}
              {t.candidateState[a.state] ?? a.state}
              {a.failReason && ` (${a.failReason})`}
            </div>
            <div className={styles.attemptMeta}>
              {formatDateTime(a.createdAt)} — {a.fileCount} files
            </div>
            {a.transfers.map((tr) => (
              <div key={tr.filename} className={styles.transfer}>
                {tr.filename} — {tr.state} {formatBytes(tr.bytesDone)} /{' '}
                {formatBytes(tr.bytesTotal)}
                {tr.retries > 0 && ` (${tr.retries} retries)`}
              </div>
            ))}
          </div>
        ))
      )}

      <h2 className={styles.section}>{t.jobs.events}</h2>
      {eventsLoading && !events ? (
        <div className={styles.placeholder}>{t.jobs.loading}</div>
      ) : !events?.length ? (
        <div className={styles.placeholder}>{t.jobs.noEvents}</div>
      ) : (
        <table className={table.table}>
          <thead>
            <tr>
              <th className={table.th}>{t.columns.time}</th>
              <th className={table.th}>{t.columns.event}</th>
              <th className={table.th}>{t.columns.detail}</th>
            </tr>
          </thead>
          <tbody>
            {events.map((e) => (
              <tr key={e.id}>
                <td className={`${table.td} ${table.mono}`}>{formatDateTime(e.createdAt)}</td>
                <td className={table.td}>{t.event[e.event as keyof typeof t.event] ?? e.event}</td>
                <td className={table.td}>{e.detail}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
```

`web/src/routes/JobDetail.module.css`:

```css
.back { color: var(--active); text-decoration: none; font-size: 12.5px; }
.back:hover { text-decoration: underline; }

.meta {
  display: flex; align-items: center; gap: 12px;
  font-size: 12.5px; color: var(--text-muted);
  margin-bottom: 10px;
}

.sleeping { color: var(--stalled); }
.candidates { font-size: 12px; color: var(--text-muted); margin-bottom: 8px; }
.failReason { font-size: 12.5px; color: var(--failed); margin-bottom: 10px; }

.actions { display: flex; gap: 8px; margin-bottom: 12px; }

.action {
  padding: 6px 12px;
  border-radius: 7px;
  border: 1px solid rgba(229, 89, 93, 0.28);
  background: rgba(229, 89, 93, 0.09);
  color: var(--orphaned);
  font-size: 12px;
  cursor: pointer;
}
.action:disabled { opacity: 0.5; cursor: default; }

.error {
  padding: 9px 12px;
  border-radius: var(--radius-sm);
  background: var(--failed-bg);
  color: var(--failed);
  font-size: 12.5px;
  margin-bottom: 12px;
}

.section { font-size: 14px; font-weight: 600; margin: 22px 0 10px; }
.placeholder { color: var(--text-muted); font-size: 12.5px; }

.attempt {
  padding: 10px 0;
  border-bottom: 1px solid var(--border-subtle);
  font-size: 12.5px;
}
.attempt:last-child { border-bottom: none; }
.attemptMeta { font-size: 11.5px; color: var(--text-muted); margin-top: 3px; }
.transfer {
  font-size: 11.5px;
  color: #9aa0aa;
  padding: 3px 0 3px 12px;
  font-family: var(--font-mono);
}
```

- [ ] **Step 2: Verifiera**

Run: `cd web && npm run build`
Expected: Bygget lyckas.

- [ ] **Step 3: Commit**

```bash
git add web/src/routes/JobDetail.tsx web/src/routes/JobDetail.module.css
git commit -m "feat(web): jobbdetaljvy med försökshistorik och felhantering (#87)"
```

---

### Task 13: Händelse- och peers-vyerna

Designen saknar dessa vyer, men de finns i dagens dashboard och har fungerande
endpoints. Att utelämna dem vore en funktionsregression.

**Files:**
- Modify: `web/src/routes/Events.tsx`, `web/src/routes/Peers.tsx`
- Create: `web/src/routes/Peers.module.css`

- [ ] **Step 1: Implementera händelsevyn**

`web/src/routes/Events.tsx`:

```tsx
import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useEvents } from '../api/queries';
import PageHeading from '../components/PageHeading';
import table from '../components/Table.module.css';
import { formatDateTime } from '../format';
import { t } from '../strings';
import styles from './Jobs.module.css';

export default function Events() {
  const { data: events = [] } = useEvents();
  const [filter, setFilter] = useState('');

  // Same substring semantics as the legacy dashboard, over the raw event code,
  // the detail text and the job id.
  const filtered = events.filter((e) =>
    !filter ||
    `${e.event} ${e.detail} ${e.jobId}`.toLowerCase().includes(filter.toLowerCase()),
  );

  return (
    <>
      <PageHeading>{t.nav.events}</PageHeading>

      <div className={styles.controls}>
        <input
          className={styles.input}
          type="text"
          placeholder={t.events.filterPlaceholder}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
      </div>

      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.time}</th>
            <th className={table.th}>{t.columns.job}</th>
            <th className={table.th}>{t.columns.event}</th>
            <th className={table.th}>{t.columns.detail}</th>
          </tr>
        </thead>
        <tbody>
          {filtered.length === 0 ? (
            <tr><td className={table.empty} colSpan={4}>{t.events.empty}</td></tr>
          ) : (
            filtered.map((e) => (
              <tr key={e.id}>
                <td className={`${table.td} ${table.mono}`}>{formatDateTime(e.createdAt)}</td>
                <td className={`${table.td} ${table.mono}`}>
                  <Link to={`/jobs/${e.jobId}`} className={styles.link}>#{e.jobId}</Link>
                </td>
                <td className={table.td}>{t.event[e.event as keyof typeof t.event] ?? e.event}</td>
                <td className={table.td}>{e.detail}</td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </>
  );
}
```

Lägg till i `web/src/routes/Jobs.module.css`:

```css
.link { color: var(--active); text-decoration: none; }
.link:hover { text-decoration: underline; }
```

- [ ] **Step 2: Implementera peers-vyn**

Sorteringen måste bete sig som idag: klick på aktiv kolumn växlar riktning, klick på
ny kolumn börjar alltid fallande.

`web/src/routes/Peers.tsx`:

```tsx
import { Fragment, useState } from 'react';
import { usePeers } from '../api/queries';
import type { Peer } from '../api/types';
import PageHeading from '../components/PageHeading';
import table from '../components/Table.module.css';
import { formatScore } from '../format';
import { t } from '../strings';
import styles from './Peers.module.css';

type SortKey = 'score' | 'successCount' | 'failCount';

export default function Peers() {
  const { data: peers = [] } = usePeers();
  const [sortKey, setSortKey] = useState<SortKey>('score');
  const [desc, setDesc] = useState(true);
  const [expanded, setExpanded] = useState<string | null>(null);

  // Clicking the active column toggles direction; a new column always starts
  // descending. Sort is non-mutating so the API order breaks ties stably.
  function sortBy(key: SortKey) {
    if (key === sortKey) {
      setDesc((d) => !d);
    } else {
      setSortKey(key);
      setDesc(true);
    }
  }

  const sorted = [...peers].sort((a, b) => {
    const d = (a[sortKey] || 0) - (b[sortKey] || 0);
    return desc ? -d : d;
  });

  const header = (key: SortKey, label: string) => (
    <th className={`${table.th} ${table.thSortable}`} onClick={() => sortBy(key)}>
      {label}
    </th>
  );

  return (
    <>
      <PageHeading>{t.nav.peers}</PageHeading>

      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.peer}</th>
            {header('score', t.columns.score)}
            {header('successCount', t.columns.succeeded)}
            {header('failCount', t.columns.failed)}
          </tr>
        </thead>
        <tbody>
          {sorted.length === 0 ? (
            <tr><td className={table.empty} colSpan={4}>{t.peers.empty}</td></tr>
          ) : (
            sorted.map((p: Peer) => (
              // A keyed Fragment is required here: the shorthand <> cannot take
              // a key, and each peer renders two sibling rows.
              <Fragment key={p.username}>
                <tr
                  className={table.rowClickable}
                  onClick={() => setExpanded(expanded === p.username ? null : p.username)}
                >
                  <td className={table.td}>{p.username}</td>
                  <td className={`${table.td} ${table.mono}`}>{formatScore(p.score)}</td>
                  <td className={`${table.td} ${table.mono}`}>{p.successCount}</td>
                  <td className={`${table.td} ${table.mono}`}>{p.failCount}</td>
                </tr>
                {expanded === p.username && (
                  <tr className={styles.detailRow}>
                    <td className={table.td} colSpan={4}>
                      {p.artists.length === 0 ? (
                        <div className={styles.artist}>{t.peers.noArtistHistory}</div>
                      ) : (
                        p.artists.map((a) => (
                          <div key={a.artistId} className={styles.artist}>
                            {t.peers.artistLine(
                              a.artistId,
                              formatScore(a.score),
                              a.successCount,
                              a.failCount,
                            )}
                          </div>
                        ))
                      )}
                    </td>
                  </tr>
                )}
              </Fragment>
            ))
          )}
        </tbody>
      </table>
    </>
  );
}
```

`web/src/routes/Peers.module.css`:

```css
.detailRow { background: #0e1115; }
.artist {
  font-size: 11.5px;
  color: #9aa0aa;
  padding: 3px 0 3px 12px;
  font-family: var(--font-mono);
}
```

- [ ] **Step 3: Verifiera**

Run: `cd web && npm run build`
Expected: Bygget lyckas.

- [ ] **Step 4: Commit**

```bash
git add web/src/routes/Events.tsx web/src/routes/Peers.tsx web/src/routes/Peers.module.css web/src/routes/Jobs.module.css
git commit -m "feat(web): händelse- och peers-vyer (#87)"
```

---

### Task 14: Hälsovy

**Files:**
- Modify: `web/src/routes/Health.tsx`
- Create: `web/src/routes/Health.module.css`

- [ ] **Step 1: Implementera vyn**

`web/src/routes/Health.tsx`:

```tsx
import { useStatus } from '../api/queries';
import PageHeading from '../components/PageHeading';
import table from '../components/Table.module.css';
import { formatTime } from '../format';
import { t } from '../strings';
import styles from './Health.module.css';

export default function Health() {
  const { data: status } = useStatus();
  const modules = status?.moduleDetails ?? {};
  const names = Object.keys(modules).sort();

  return (
    <>
      <PageHeading>{t.nav.health}</PageHeading>

      <table className={table.table}>
        <thead>
          <tr>
            <th className={table.th}>{t.columns.module}</th>
            <th className={table.th}>{t.columns.lastRun}</th>
          </tr>
        </thead>
        <tbody>
          {names.map((name) => {
            const m = modules[name];
            const label = m.lastAttempt ? formatTime(m.lastAttempt) : t.health.neverRun;
            return (
              <tr key={name}>
                <td className={table.td}>{name}</td>
                <td
                  className={`${table.td} ${m.ready ? '' : styles.unhealthy}`}
                  title={m.lastError}
                >
                  {label}
                  {m.consecutiveFailures > 0 &&
                    ` (${t.health.consecutiveFailures(m.consecutiveFailures)})`}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}
```

`web/src/routes/Health.module.css`:

```css
.unhealthy { color: var(--orphaned); font-weight: 600; }
```

- [ ] **Step 2: Verifiera och commit**

Run: `cd web && npm run build`
Expected: Bygget lyckas.

```bash
git add web/src/routes/Health.tsx web/src/routes/Health.module.css
git commit -m "feat(web): hälsovy med modulstatus (#87)"
```

---

### Task 15: Config-endpoint och inställningsvy

Läsvy. API-nyckeln skickas aldrig — bara huruvida den är konfigurerad.

**Files:**
- Create: `internal/observ/config.go`, `internal/observ/config_test.go`
- Modify: `internal/observ/observ.go` (registrera endpointen), `web/src/routes/Settings.tsx`
- Create: `web/src/routes/Settings.module.css`

**Interfaces:**
- Produces: `GET /api/config` → `AppConfig` (se `web/src/api/types.ts`, Task 6)

- [ ] **Step 1: Skriv det fallerande testet**

`internal/observ/config_test.go`:

```go
package observ

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigHandlerNeverLeaksTheAPIKey(t *testing.T) {
	cfg := AppConfig{
		LidarrURL:              "http://lidarr:8686",
		lidarrAPIKey:           "super-secret-value",
		ReconcileInterval:      "5m",
		MaxConcurrentDownloads: 3,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)

	newConfigHandler(func() AppConfig { return cfg }).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Fatal("response body leaked the Lidarr API key")
	}

	var got struct {
		LidarrURL              string `json:"lidarrUrl"`
		LidarrAPIKeyConfigured bool   `json:"lidarrApiKeyConfigured"`
		MaxConcurrentDownloads int    `json:"maxConcurrentDownloads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.LidarrAPIKeyConfigured {
		t.Error("lidarrApiKeyConfigured = false, want true")
	}
	if got.LidarrURL != "http://lidarr:8686" {
		t.Errorf("lidarrUrl = %q", got.LidarrURL)
	}
}

func TestConfigHandlerReportsMissingAPIKey(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)

	newConfigHandler(func() AppConfig { return AppConfig{} }).ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), `"lidarrApiKeyConfigured":true`) {
		t.Error("reported a configured key when none was set")
	}
}
```

- [ ] **Step 2: Kör testet, verifiera att det fallerar**

Run: `go test ./internal/observ/ -run TestConfigHandler -v`
Expected: FAIL — `undefined: AppConfig`

- [ ] **Step 3: Implementera endpointen**

`internal/observ/config.go`:

```go
// Package observ: config.go serves the settings view's read-only view of the
// running configuration. Secrets are never sent — only whether they are set.
// Writable configuration is deliberately out of scope; see issue #89.
package observ

import (
	"encoding/json"
	"net/http"
)

// AppConfig is the subset of configuration the settings view displays. The
// Lidarr API key is unexported so it can never be marshalled by accident.
type AppConfig struct {
	LidarrURL              string `json:"lidarrUrl"`
	ReconcileInterval      string `json:"reconcileInterval"`
	MaxConcurrentDownloads int    `json:"maxConcurrentDownloads"`

	lidarrAPIKey string
}

// NewAppConfig builds the display config, keeping the API key out of the
// marshalled surface.
func NewAppConfig(lidarrURL, lidarrAPIKey, reconcileInterval string, maxConcurrent int) AppConfig {
	return AppConfig{
		LidarrURL:              lidarrURL,
		ReconcileInterval:      reconcileInterval,
		MaxConcurrentDownloads: maxConcurrent,
		lidarrAPIKey:           lidarrAPIKey,
	}
}

// ConfigFunc produces the current display configuration.
type ConfigFunc func() AppConfig

func newConfigHandler(config ConfigFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := config()
		resp := struct {
			AppConfig
			LidarrAPIKeyConfigured bool `json:"lidarrApiKeyConfigured"`
		}{c, c.lidarrAPIKey != ""}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}
```

- [ ] **Step 4: Registrera endpointen**

I `internal/observ/observ.go`, lägg till parametern `config ConfigFunc` i
`NewServerWithReadiness`-signaturen och registrera före `mux.Handle("/", …)`:

```go
	mux.Handle("/api/config", newConfigHandler(config))
```

Uppdatera anropsplatsen i `cmd/slskdarr` så den skickar in en `ConfigFunc` byggd med
`NewAppConfig` från den inlästa TOML-konfigurationen.

- [ ] **Step 5: Kör Go-testerna**

Run: `go test ./... `
Expected: PASS

- [ ] **Step 6: Implementera inställningsvyn**

`web/src/routes/Settings.tsx`:

```tsx
import { useConfig } from '../api/queries';
import PageHeading from '../components/PageHeading';
import { t } from '../strings';
import styles from './Settings.module.css';

export default function Settings() {
  const { data: config } = useConfig();

  return (
    <>
      <PageHeading>{t.nav.settings}</PageHeading>
      <div className={styles.notice}>{t.settings.readOnlyNotice}</div>

      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.lidarr}</h2>
        <Field label={t.settings.url} value={config?.lidarrUrl ?? '—'} />
        <Field
          label={t.settings.apiKey}
          value={
            config?.lidarrApiKeyConfigured
              ? t.settings.apiKeyHidden
              : t.settings.apiKeyMissing
          }
        />
      </section>

      <section className={styles.group}>
        <h2 className={styles.groupTitle}>{t.settings.reconcile}</h2>
        <Field label={t.settings.interval} value={config?.reconcileInterval ?? '—'} />
        <Field
          label={t.settings.concurrentDownloads}
          value={String(config?.maxConcurrentDownloads ?? '—')}
        />
      </section>
    </>
  );
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className={styles.field}>
      <label className={styles.label}>{label}</label>
      <input className={styles.input} value={value} disabled readOnly />
    </div>
  );
}
```

`web/src/routes/Settings.module.css`:

```css
.notice {
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--active-bg);
  color: var(--active);
  font-size: 12.5px;
  margin-bottom: 16px;
}

.group {
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 16px;
  margin-bottom: 14px;
}

.groupTitle { font-size: 13px; font-weight: 600; margin: 0 0 12px; }
.field { display: flex; align-items: center; gap: 12px; margin-bottom: 10px; }
.label { width: 180px; font-size: 12.5px; color: var(--text-muted); }

.input {
  flex: 1;
  padding: 8px 10px;
  border-radius: var(--radius-sm);
  background: #13161b;
  border: 1px solid #23272f;
  color: var(--text-muted);
  font-size: 12.5px;
}
```

- [ ] **Step 7: Verifiera och commit**

Run: `make test`
Expected: PASS på både Go- och frontend-sidan.

```bash
git add internal/observ/config.go internal/observ/config_test.go internal/observ/observ.go cmd/ web/src/routes/Settings.tsx web/src/routes/Settings.module.css
git commit -m "feat(observ): read-only config-endpoint och inställningsvy (#87)"
```

---

### Task 16: Radera den gamla dashboarden

**Files:**
- Delete: `internal/observ/web/dashboard.html`, `internal/observ/web/dashboard.js`
- Modify: `internal/observ/web_test.go` (om den fortfarande refererar de gamla handlarna)

- [ ] **Step 1: Verifiera att inget refererar filerna**

Run: `grep -rn "dashboard.html\|dashboard.js\|dashboardHandler\|dashboardJSHandler" --include="*.go" --include="*.md" .`
Expected: Endast träffar i specen och planen (dokumentation av historiken). Träffar i
Go-kod betyder att Task 2 inte är helt genomförd.

- [ ] **Step 2: Radera filerna**

```bash
git rm internal/observ/web/dashboard.html internal/observ/web/dashboard.js
```

- [ ] **Step 3: Kör hela testsviten**

Run: `make test`
Expected: PASS

- [ ] **Step 4: Manuell rökttest**

Run: `make build && ./slskdarr --config config.toml`

Öppna `http://localhost:8080` och verifiera:
- Sidan laddar, sidebaren visar sex poster.
- Översikt visar fem statuskort och aktiva jobb.
- Jobb-vyn filtrerar på både sökfält och statusväljare.
- Klick på en jobbrad går till `/jobs/:id`; **ladda om sidan** och verifiera att
  detaljvyn fortfarande visas (SPA-fallbacken fungerar).
- `/api/nope` i adressfältet ger 404, inte HTML.
- Händelser, Peers, Hälsa och Inställningar renderar.

- [ ] **Step 5: Commit**

```bash
git commit -m "refactor(observ): radera vanilla-JS-dashboarden (#87)"
```

---

## Efter planen

När samtliga tasks är gröna: kör `superpowers:finishing-a-development-branch` för att
avgöra hur `feat/frontend-spa` ska integreras.

Kvarstående arbete spåras i **#88** (charts), **#89** (skrivbar config), **#86**
(i18n) och **#60** (SSE).
