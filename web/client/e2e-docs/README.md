# Client documentation screenshots

Opt-in capture of the current client React UI using **synthetic in-memory API fixtures only**.
These images illustrate controls and layout, not a live provider, payment, tenant or runtime acceptance.

```sh
cd web/client
npm ci --include=dev
NODE_ENV=development npx playwright test -c playwright.docs.config.ts
```

The dedicated config starts a fresh mock-mode Vite server bound to `127.0.0.1:5273`
(`reuseExistingServer: false`, strict port), desktop 1440×1000, DPR1, light theme,
`en-US`/UTC and fixed display time. The tall create-server wizard uses its actual
content height so fixed navigation stays at the top. The capture suite is outside
normal `e2e/` discovery and does not add screenshot writes to ordinary test runs.

Safety and reproducibility:

- Only localhost assets are allowed; `/api/` and every non-local network request are blocked.
- Current application components/dialogs are used, with no fabricated DOM or injected screenshot text.
- The test-only API hook provides PVE-shaped resources, documentation-only IPv4 addresses,
  synthetic identifiers and `example.com` users; it refuses unknown operations/mutations.
- Each screenshot waits for meaningful rendered content and font readiness; animations are disabled.
- Page/console errors, unhandled mock routes and blocked external requests fail the suite.
- Production app code, general-purpose mock handlers, package manifest and lockfile are unchanged.

The parent inventory in `web/docs-shots.md` and `web/docs-image-audit.json` records
published filenames/routes/coverage. Keep user-facing captions explicit that these
are mock/demo captures, not evidence that a cloud or financial operation succeeded.
