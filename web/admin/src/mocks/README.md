# Mock mode — frontend dev without a backend

Run the admin console fully in-browser: no Go API, no PostgreSQL, no
Keycloak. Auth is short-circuited and every API call is served from in-memory
fixtures.

```sh
npm run dev:mock     # Vite on http://localhost:5274 with VITE_MOCK=1
```

## How it works

- `main.tsx` — when `VITE_MOCK=1`, dynamically imports this module and calls
  `enableMocks()` before rendering. Production builds never include it.
- `enabled.ts` — the flag (`MOCK_ENABLED`) plus a canned always-authenticated
  master-realm session (`mockAuthState`, admin@example.com / Ops Admin).
  `lib/auth.tsx` swaps its provider/`useAuth` on this flag; nothing else in
  the app knows mocks exist. `GET /admin/me` is mocked to a SUPER_ADMIN with
  the `admin:*` wildcard grant, so every nav item is visible. Append
  `?logged-out` to any URL to preview the login page.
- `router.ts` — a tiny `"METHOD /path/:param"` matcher with simulated latency.
  `lib/api.ts` short-circuits `apiFetchEnvelope`/`apiFetchRaw` through it via
  `setMockHandler`. Unhandled endpoints log a `[mock]` console warning and
  return an empty envelope (pages render their empty states, never crash).
- `db.ts` — the in-memory "database", seeded from `fixtures/`. Mutations
  (activate a billing profile, create a tax rate, etc.) update it, so the UI
  behaves statefully within a session. Reload = reset.
- `fixtures/` — seed data, one file per domain (`clients`, `system`, `audit`,
  `dashboard`). Edit these to change what the pages show.
- `handlers/` — endpoint implementations, one file per domain, registered in
  `index.ts`.

## Extending

New endpoint: add a fixture (if it needs data) and register a route in the
matching `handlers/*.ts`:

```ts
on("GET /admin/thing/:id", ({ params }) => ({ data: db.things.find((t) => t.id === params.id) }))
```

Simulate an error: `throw new ApiError(402, 402, "Insufficient funds")`.

The `[mock] no handler for …` console warnings are the to-do list: navigate the
app, watch the console, fill the gaps.
