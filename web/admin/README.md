# ProxCloud operator console

The operator-facing React + TypeScript SPA. It uses Vite, Tailwind/shadcn components,
TanStack Query and OIDC authorization-code + PKCE. The matching Go API enforces
operator permissions; hiding a menu item is not an authorization boundary.

## Local development

Use the repository's supported Node/npm toolchain (CI uses Node 24), then:

```sh
npm ci
npm run dev
```

For isolated UI fixtures, `npm run dev:mock` runs on port 5274. Mock data does not
prove live API permissions, billing, cloud mutations or install readiness. For the
whole local platform follow [Development](../../docs/development.md).

Runtime API/OIDC configuration is served from `public/config.js` in development
and generated at container startup for deployments. Browser configuration is
public: never put API secrets, SMTP credentials or cloud tokens there.

## Checks

```sh
npm run lint
npm run build
npx playwright install --only-shell chromium
npm run test:smoke
```

`test:smoke` opens the built login surface and checks for runtime errors; it is not
an authenticated admin acceptance test. `test:e2e`, `test:a11y` and `shots` are
separate Playwright tasks and may need their configured environment. See
[Testing](../../docs/testing.md).

Embedded operator/API help lives under `src/docs/content/`. Update it against the
actual route, configuration and capability checks, not historical feature plans.

## Published documentation screenshots

```sh
npx playwright test --config playwright.docs.config.ts
```

This **opt-in** capture suite is separate from the generic all-route `shots` gallery
and normal `test:e2e` task. It writes selected UI states directly to
`public/docs-img/` using the current components and synthetic in-memory fixtures.
The capture-only Vite config disables HMR/refresh instrumentation, binds loopback
on strict port 5274, and refuses to reuse another server. Playwright closes that
server after the run.

Captures use light theme, 1440 × 1000 CSS pixels, DPR 1, UTC, a fixed displayed
clock, local fonts and reduced motion. Readiness assertions replace arbitrary
sleep delays. External and real API requests are blocked, mock mutations are
refused, and console/page errors or missing mock handlers fail the run. The suite
opens/fills real controls without submitting provider, approval or financial
changes. Mock illustrations are not live PVE/API/billing acceptance evidence.

After recapture, visually review every changed image, check the rendered `/docs`
consumers, and update captions to describe the visible state. The add-provider
image shows the upper connection fields of a scrollable dialog, not all options.
Private keys, real customer data, internal endpoints and saved authentication
state must never be introduced into this fixture or public output.

> **Owner review:** `public/brand/logo.svg` and `public/brand/favicon.svg` are neutral
> placeholders. Final brand assets remain a release decision.
