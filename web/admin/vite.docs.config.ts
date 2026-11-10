import { mergeConfig } from "vite"
import appConfig from "./vite.config"

// Screenshot-only server: current application components and mocks, no HMR runtime.
// Explicitly disable both HMR and Oxc refresh instrumentation for deterministic captures.
export default mergeConfig(appConfig, {
  oxc: { jsx: { refresh: false } },
  server: { host: "127.0.0.1", port: 5274, strictPort: true, hmr: false },
})
