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
