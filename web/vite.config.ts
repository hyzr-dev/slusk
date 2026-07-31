// defineConfig comes from vitest/config, not vite — the plain Vite export does
// not type the `test` block and tsc will reject it.
import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// The dev server has no backend of its own — /api and /status are proxied to a
// running slskdarr, normally the testenv lab. The lab publishes the observ
// listener on 9090 and requires a bearer token, so the proxy injects the
// Authorization header and the browser never sees an auth prompt.
//
// Both values are read from Node's process.env rather than import.meta.env or a
// VITE_ prefix: vite.config.ts runs in Node, so the token stays on the dev
// server instead of being baked into the client bundle. The default token is the
// lab's fixed value from testenv/render_config.py, which makes `make dev` work
// against a running lab with no configuration. Point SLSKDARR_DEV_API elsewhere
// when verifying a worktree that serves the backend on another port.
const apiTarget = process.env.SLSKDARR_DEV_API ?? 'http://localhost:9090';
const apiToken = process.env.SLSKDARR_DEV_TOKEN ?? 'slskdarr-pr-lab-observ-token-0001';

// Origin must be rewritten alongside Host. changeOrigin only rewrites Host, so
// a browser POST through this proxy still carries `Origin: http://localhost:5173`
// while the backend believes it is serving 9090. ProtectPrivateEndpoints'
// same-origin check (issue #279, internal/observ/security.go) then rejects every
// browser-initiated mutation with 403 — search, job creation, retry, cancel,
// config writes. Production is unaffected, because there the SPA is served from
// the same origin as the API; it is only this proxy that splits them.
//
// A bearer-authenticated client may omit Origin but cannot override a
// conflicting one, so stripping it would work too. Setting it to the target is
// the more honest of the two: after proxying, the request really is same-origin.
const backendProxy = {
  target: apiTarget,
  changeOrigin: true,
  headers: { Authorization: `Bearer ${apiToken}`, Origin: apiTarget },
};

// Build output lands inside internal/observ/web/ because go:embed cannot read
// files outside its own package directory.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/observ/web/dist',
    // dist/placeholder.html is a tracked git file that go:embed needs present
    // even before the frontend is ever built. emptyOutDir: true wipes the
    // whole outDir first, which would delete placeholder.html on every build
    // and dirty the working tree. Keep it false — but that means Vite alone
    // does not clean up stale content-hashed bundles between local builds,
    // which go:embed would otherwise bake into every binary. The `ui` Make
    // target removes dist/assets and dist/index.html (sparing
    // placeholder.html) before invoking Vite, so local builds stay clean
    // without touching this setting. Do not flip this back to true.
    emptyOutDir: false,
  },
  server: {
    proxy: {
      '/api': backendProxy,
      '/status': backendProxy,
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/test-setup.ts',
    // Vitest defaults to 5s per test. Several Settings.test.tsx cases drive
    // fake timers through the save-then-restart-poll sequence and exceed that
    // under a full parallel run — on a loaded machine or a CI runner they time
    // out while passing comfortably in isolation. This raises the wall-clock
    // allowance only; no assertion is relaxed and no test is skipped. A test
    // that genuinely hangs still fails, just 15s later.
    testTimeout: 15_000,
  },
});
