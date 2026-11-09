import { defineConfig } from "@playwright/test"

// Against the production build (`npm run build` first), not the mock dev server.
export default defineConfig({
  testDir: "./e2e-smoke",
  timeout: 30_000,
  reporter: [["list"]],
  use: { baseURL: "http://localhost:4174" },
  webServer: { command: "npm run preview -- --port 4174 --strictPort", port: 4174, timeout: 30_000 },
})
