# API Overview & Conventions

This is an orientation and conventions doc for contributors — not an exhaustive
endpoint reference. For the interactive, always-current endpoint reference, open
the admin console at `/docs/api` (and the MCP tool reference at `/docs/api/mcp`).

ProxCloud exposes three HTTP surfaces, all mounted on the same router
(`internal/server/server.go`):

| Surface | Base path | Auth | Style | Envelope |
|---------|-----------|------|-------|----------|
| Customer API | `/api/v1` | OIDC JWT (`clients` realm) | camelCase | `CustomHttpResponse` (`{data,errors,paging,...}`) |
| Admin API | `/admin-api/v1` | SigV4 **or** admin-api-realm JWT | snake_case | `{data}` / `{data,next_marker}` / `{error}` |
| MCP | `/mcp` | OIDC JWT **or** `pk.sk` API key | MCP tools | JSON tool results |

All three are described in `docs/auth.md`. The management port (`:8081`, separate
from the API port `:8080`) additionally serves health probes and operator debug
triggers — see `docs/configuration.md`.

## Customer API — `/api/v1`

Authenticated with a `clients`-realm JWT. Responses use the `CustomHttpResponse`
envelope (`pkg/httpx/response.go`):

```json
{ "data": { ... }, "errors": null, "paging": null, "redirectUrl": null, "authRedirect": null }
```

- **Null fields are omitted** on the wire (only non-null fields serialize). An
  empty/`null` data response is `{}`.
- Field names are **camelCase**.
- A single error is an object (not an array): `{ "errors": { "code": <int>, "message": "<key>" } }`.
- A few endpoints return a bare object/list with no envelope (e.g.
  `account/details`, some affiliate reads) via `Raw`.

### Pagination

Two paging shapes coexist under the `paging` field:

- **Offset list** — `paging: { limit, offset, total }` (`List` / `Page`).
- **Cursor list** — `paging: { limit, offset, total, nextMarker?, prevMarker? }`
  (`CursorList`); pass the returned `nextMarker` back to page forward.

### Route groups

Each domain registers its own `Routes()` under `/api/v1` (see `AppRouter` in
`internal/server/server.go`). Major groups:

| Group | Purpose |
|-------|---------|
| `account` | Authenticated user's account/profile |
| `organizations` | Organizations, custom roles, org audit log |
| `billing-profile` / `bill` | Billing summaries, bills, payments, cards |
| `project` | Projects and their cloud resources (servers, volumes, networks, security groups, keypairs, images (project templates), buckets, public-IP attach) |
| `platform-configuration` | Public platform config (login bootstrap) |
| `features` | Available feature set |
| `promotion` / `affiliate` | Deposit promos, affiliate program |
| `catalog` | Cloud catalog (flavor categories, image groups) |
| `order` / `project-invites` | Orders and project invitations |
| `admin/**` | Operator console surface (permission-gated) |
| `streaming` | SSE real-time event stream (`/events/{projectId}`) |
| `callbacks`, `payments`, `webhooks` | Inbound webhooks (public whitelist) |

Project public-network policy: `GET /api/v1/project/{id}/public-networks` lists
the external (public) networks the project is allowed to use, and
`PUT /api/v1/admin/project/{id}/public-networks` (operator console surface) sets
the per-project allow-list — a `null`/absent `publicNetworkIds` means all
public networks. There are no floating IPs on Proxmox: public reachability is the
server action `ATTACH_PUBLIC_NETWORK` / `DETACH_PUBLIC_NETWORK` on
`POST /api/v1/project/{id}/cloud/{resourceId}/action` (body data `networkId`). It
adds (or removes) a second NIC (`net1`) on the admin-designated bridge with one
address from that network's pool, pinned by the VM firewall's ipfilter
(`internal/platform/project/cloud_network_pve.go`). The network must be in the
project's allow-list. Attaching again while the NIC is plugged answers 409 and
leaves it alone; an address held by a NIC that never got plugged is plugged.

## Admin API — `/admin-api/v1`

A machine-to-machine surface (`internal/platform/adminapi`). Authenticated with an
`hmac_keys` SigV4 signature or an admin-api-realm JWT (see the gate in
`docs/auth.md`). Distinct conventions from the customer API:

- **snake_case** field names, non-null fields only.
- **Entity** responses: `{ "data": { ... } }` (`200`, or `201` on create).
- **List** responses: `{ "data": [ ... ], "next_marker": "<id>" }` (`next_marker`
  omitted on the last page).
- **Error** responses: `{ "error": { "code": "<CODE>", "message": "<text>" } }`.

### Keyset pagination

Lists are keyset-paged by `_id` (`adminapi.go`):

- Query params: `marker` (the `_id` to start at, inclusive) and `limit`
  (`1..500`, default `50`).
- The server fetches `limit + 1` rows; if an extra row exists, its `_id` is
  returned as `next_marker` and the extra row is trimmed.
- To page: issue the request, then repeat with `marker=<next_marker>` until the
  response omits `next_marker`.

```
GET /admin-api/v1/users?limit=50
  -> { "data": [ ...50 users... ], "next_marker": "665f...e21" }
GET /admin-api/v1/users?limit=50&marker=665f...e21
  -> { "data": [ ...next page... ] }        # no next_marker => last page
```

A non-numeric `limit` is `400`; `limit > 500` is `400`.

### Route groups

Registered in `Handler.Routes` (`adminapi.go`):

| Resource | Routes |
|----------|--------|
| `users` | list, create, get, delete |
| `organizations` | list, create, get, update; members list/add/remove/set-role |
| `billing_profiles` | list, create, get, update; activate / suspend / resume |
| `projects` | list, create, get; provision |
| `bills` | list, get |
| `account_credits` | list, create, get, delete |
| `service_providers` | list, get |

## MCP — `/mcp`

A stateless streamable-HTTP Model Context Protocol endpoint
(`internal/platform/mcp`). The principal's realm (or an admin API key) selects a
toolset:

- **client tools** (`clients` realm) — read/act on the caller's own projects and
  billing (list projects/servers/volumes/networks/security groups/images, get
  cost/billing, run server power actions, create
  and delete volumes, invite users). See `tools_client.go`.
- **admin tools** (`master` realm or `pk.sk` API key) — the operator toolset. See
  `tools_admin.go`.

Each tool maps to a real REST endpoint and is dispatched in-process through the
full router, so authorization, DTO shapes, and audit are identical to a direct
API call. Browse the live tool catalog at `/docs/api/mcp` in the admin console.

## Error model

| Status | Meaning |
|--------|---------|
| `400` | Bad request — malformed body, invalid query param, or a rejected argument. |
| `401` | Not authenticated — missing/invalid token (empty body, `WWW-Authenticate: Bearer`). |
| `403` | Authenticated but not authorized — e.g. wrong realm at the Admin API gate, or an MCP principal whose realm has no toolset. |
| `404` | Resource not found. |
| `409` | Conflict — e.g. creating a resource that already exists. |
| `501` | The endpoint exists but the capability is **not enabled / not configured in this deployment** — the code path drives a subsystem (cloud orchestration, an external provider) that this deployment has not wired up. It is a deliberate "not implemented here" response, not a runtime error. |

Unmatched routes and disallowed methods on the customer API still return the
`CustomHttpResponse` error envelope via `NotFoundHandler` / `MethodNotAllowedHandler`.
On the Admin API, errors use the `{ "error": { code, message } }` shape.
