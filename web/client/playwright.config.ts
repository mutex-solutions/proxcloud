import { defineConfig } from "@playwright/test"

// Runs against the mock app (no backend). `npm run dev:mock` is started
// automatically unless one is already listening on 5273.
export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  retries: 0,
  workers: 4,
  reporter: [["list"]],
  use: {
    baseURL: "http://localhost:5273",
    viewport: { width: 1280, height: 800 },
    deviceScaleFactor: 1,
  },
  webServer: {
    command: "npm run dev:mock",
    port: 5273,
    reuseExistingServer: true,
    timeout: 30_000,
  },
})
