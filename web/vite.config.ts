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
