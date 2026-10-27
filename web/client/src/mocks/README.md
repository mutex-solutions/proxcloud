# Mock mode — frontend dev without a backend

Run the customer console fully in-browser: no Go API, no PostgreSQL, no
Keycloak. Auth is short-circuited and every API call is served from in-memory
fixtures.

```sh
npm run dev:mock     # Vite on http://localhost:5273 with VITE_MOCK=1
```

## How it works

- `main.tsx` — when `VITE_MOCK=1`, dynamically imports this module and calls
  `enableMocks()` before rendering. Production builds never include it.
- `enabled.ts` — the flag (`MOCK_ENABLED`) plus a canned always-authenticated
  OIDC session (`mockAuthState`). `lib/auth.tsx` swaps its provider/`useAuth`
  on this flag; nothing else in the app knows mocks exist.
- `router.ts` — a tiny `"METHOD /path/:param"` matcher with simulated latency.
  `lib/api.ts` short-circuits `apiFetchEnvelope`/`apiFetchRaw` through it via
  `setMockHandler`. Unhandled endpoints log a `[mock]` console warning and
  return an empty envelope (pages render their empty states, never crash).
- `db.ts` — the in-memory "database", seeded from `fixtures/`. Mutations
  (create/delete server, etc.) update it, so the UI behaves statefully within
  a session. Reload = reset.
- `fixtures/` — seed data, one file per domain. Edit these to change what the
  pages show.
- `handlers/` — endpoint implementations, one file per domain, registered in
  `index.ts`.

## Extending

New endpoint: add a fixture (if it needs data) and register a route in the
matching `handlers/*.ts`:

```ts
on("GET /project/:pid/thing", ({ params }) => ({ data: db.things }))
```

Simulate an error: `throw new ApiError(402, 402, "Insufficient funds")`.

The `[mock] no handler for …` console warnings are the to-do list: navigate the
app, watch the console, fill the gaps.
