import { defineConfig, devices } from '@playwright/test';

const baseURL = process.env.MAGENTO_BASE_URL ?? 'http://localhost';

export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  expect: {
    timeout: 15_000,
  },
  retries: process.env.CI ? 1 : 0,
  workers: 1,          // single worker — only one Magento instance per run
  reporter: [
    ['line'],
    ['json', { outputFile: 'playwright-report/results.json' }],
  ],
  use: {
    baseURL,
    headless: true,
    screenshot: 'only-on-failure',
    video: 'off',
    // Accept self-signed certs on local Docker stacks
    ignoreHTTPSErrors: true,
    // Extra time for Magento's PHP rendering
    navigationTimeout: 30_000,
    actionTimeout: 15_000,
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  // Fail fast on first error in CI
  ...(process.env.CI ? { maxFailures: 3 } : {}),
});
