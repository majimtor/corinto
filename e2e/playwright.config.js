import { defineConfig, devices } from '@playwright/test';

// The browser tests run against a stack that is already up: `make e2e`
// starts the local one first. Its addresses are loopback ones because the
// session cookie of the local run is issued to no other host.
export default defineConfig({
  testDir: './tests',
  outputDir: './results',
  // One test at a time and no second try. The demo entry opens ten
  // sessions a minute for an address, and a test that passes at the second
  // attempt hides the first.
  workers: 1,
  retries: 0,
  forbidOnly: true,
  reporter: 'list',
  timeout: 90_000,
  use: {
    baseURL: process.env.PANEL_URL ?? 'http://127.0.0.1:8080',
    // The panel answers the address with no language in the language of
    // the browser, and the path is walked in Spanish (PLAN.md section 12).
    locale: 'es-ES',
  },
  projects: [
    // The whole path, with axe on every page.
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    // The demo entry alone: a session cookie that one engine discards is
    // found here.
    { name: 'firefox', use: { ...devices['Desktop Firefox'] }, testMatch: 'entry.spec.js' },
    { name: 'webkit', use: { ...devices['Desktop Safari'] }, testMatch: 'entry.spec.js' },
  ],
});
