import { defineConfig } from '@playwright/test';

// test/boot/run.sh runs these tests against a booted appliance; see appliance.ts for the inputs. Traces, videos and
// screenshots stay off: they would record the typed password and the TOTP secret. So does the page snapshot a
// failure writes next to them, which this variable turns off.
process.env.PLAYWRIGHT_NO_COPY_PROMPT = '1';

export default defineConfig({
  testDir: '.',
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'list',
  // A test may wait for the built-in CA to issue the console's certificate first.
  timeout: 30 * 60_000,
  expect: { timeout: 30_000 },
  use: {
    browserName: 'chromium',
    trace: 'off',
    video: 'off',
    screenshot: 'off',
    actionTimeout: 30_000,
    navigationTimeout: 60_000,
  },
});
