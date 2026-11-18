import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  plugins: [react()],
  build: {
    // dist/index.html is the placeholder the Go build embeds when the console is not built; the build goes
    // beside it so that it never changes a tracked file.
    outDir: 'dist/app',
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['src/test-setup.ts'],
    coverage: {
      provider: 'v8',
      include: ['src/**/*.{ts,tsx}'],
      exclude: ['src/**/*.test.{ts,tsx}', 'src/test-setup.ts', 'src/api/schema.d.ts', 'src/main.tsx'],
      // Sonar reads the lcov paths from the repository root.
      reporter: ['text', ['lcov', { projectRoot: '..' }]],
      thresholds: { lines: 80, functions: 80, branches: 80, statements: 80 },
    },
  },
});
