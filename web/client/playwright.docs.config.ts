import { defineConfig } from "@playwright/test"

// Opt-in documentation capture only: never included by the normal e2e suite.
// A fresh localhost-only mock server avoids accidentally capturing another app.
export default defineConfig({
  testDir: "./e2e-docs",
  testMatch: "docs-screenshots.spec.ts",
  timeout: 45_000,
  retries: 0,
  workers: 1,
  reporter: [["list"]],
  outputDir: "./test-results/docs-capture",
  use: {
    baseURL: "http://127.0.0.1:5273",
    viewport: { width: 1440, height: 1000 },
    deviceScaleFactor: 1,
    colorScheme: "light",
    locale: "en-US",
    timezoneId: "UTC",
    reducedMotion: "reduce",
    serviceWorkers: "block",
    trace: "off",
    screenshot: "off",
    video: "off",
  },
  webServer: {
    command: "npm run dev:mock -- --host 127.0.0.1 --strictPort",
    url: "http://127.0.0.1:5273",
    reuseExistingServer: false,
    timeout: 45_000,
  },
})
