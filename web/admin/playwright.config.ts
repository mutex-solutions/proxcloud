import { defineConfig } from "@playwright/test"

// Runs against the mock app (no backend). `npm run dev:mock` is started
// automatically unless one is already listening on 5274.
// E2E_PORT moves the mock app off 5274 (a second checkout running its own).
const port = Number(process.env.E2E_PORT ?? 5274)

export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  retries: 0,
  workers: 4,
  reporter: [["list"]],
  use: {
    baseURL: `http://localhost:${port}`,
    viewport: { width: 1280, height: 800 },
    deviceScaleFactor: 1,
  },
  webServer: {
    command: `npm run dev:mock -- --port ${port}`,
    port,
    reuseExistingServer: true,
    timeout: 30_000,
  },
})
