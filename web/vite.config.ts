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
    // and dirty the working tree. outDir lives outside this project's root,
    // so Vite already defaults to leaving unrelated files alone — leave that
    // default in place instead of forcing a full wipe.
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
