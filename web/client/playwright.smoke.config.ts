import { defineConfig } from "@playwright/test"

// Against the production build (`npm run build` first), not the mock dev server.
export default defineConfig({
  testDir: "./e2e-smoke",
  timeout: 30_000,
  reporter: [["list"]],
  use: { baseURL: "http://localhost:4173" },
  webServer: { command: "npm run preview -- --port 4173 --strictPort", port: 4173, timeout: 30_000 },
})
