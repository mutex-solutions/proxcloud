# ProxCloud customer console

The customer-facing React + TypeScript SPA. It uses Vite, Tailwind/shadcn components,
TanStack Query and OIDC authorization-code + PKCE; the matching Go API supplies
project-scoped cloud, billing, organization and sandbox endpoints.

## Local development

Use the repository's supported Node/npm toolchain (CI uses Node 24), then:

```sh
npm ci
npm run dev
```

For an isolated UI fixture, `npm run dev:mock` runs on port 5273. Mock data does not
prove API authorization, billing, PVE operations or an install is production-ready.
For the whole local platform use the root Docker Compose instructions in
[Development](../../docs/development.md).

Runtime API/OIDC configuration is served from `public/config.js` in development
and generated at container startup for deployments. Never put credentials in a
browser config or build-time frontend variable.

## Checks

```sh
npm run lint
npm run build
npx playwright install --only-shell chromium
npm run test:smoke
```

`test:smoke` opens the built login surface and checks for runtime errors; it is not
an authenticated cloud lifecycle test. `test:e2e`, `test:a11y` and `shots` are
separate Playwright tasks and may need their configured environment. See
[Testing](../../docs/testing.md).

Embedded help lives under `src/docs/content/`; keep its capability statements in
sync with server checks, especially managed sandbox saved-lifecycle holds. Do not
turn historical or untested behavior into customer promises.

> **Owner review:** `public/brand/logo.svg` and `public/brand/favicon.svg` are neutral
> placeholders. Final brand assets remain a release decision.
