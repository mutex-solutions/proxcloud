import { defineConfig } from "@playwright/test"

// Opt-in publication capture, separate from the generic route gallery and normal tests.
export default defineConfig({
  testDir: "./docs-capture",
  outputDir: "./test-results/docs-capture",
  timeout: 45_000,
  retries: 0,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: "http://127.0.0.1:5274",
    viewport: { width: 1440, height: 1000 },
    deviceScaleFactor: 1,
    locale: "en-US",
    timezoneId: "UTC",
    colorScheme: "light",
    reducedMotion: "reduce",
    serviceWorkers: "block",
    trace: "off",
    video: "off",
  },
  webServer: {
    command: "VITE_MOCK=1 vite --config vite.docs.config.ts",
    url: "http://127.0.0.1:5274",
    reuseExistingServer: false,
    timeout: 30_000,
  },
})
