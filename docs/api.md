# API Overview & Conventions

This is an orientation and conventions doc for contributors — not an exhaustive
endpoint reference. The admin console's docs (`/docs/reference/overview`, and the MCP
tool reference at `/docs/reference/mcp-server`) are hand-written markdown pages
(`web/admin/src/docs/content`); the route source (`internal/server/server.go` and each
package's `Routes`) is the authority. `/openapi.json` is a stub with no paths; the E2B
API publishes its own document at `/e2b/openapi.json`.

ProxCloud exposes three HTTP surfaces, all mounted on the same router
(`internal/server/server.go`):

| Surface | Base path | Auth | Style | Envelope |
|---------|-----------|------|-------|----------|
| Customer API | `/api/v1` | OIDC JWT (`auth.main` or `auth.admin` realm) **or** a customer's `pk.sk` personal access token | camelCase | `CustomHttpResponse` (`{data,errors,paging,...}`) |
| Admin API | `/admin-api/v1` | SigV4 **or** admin-api-realm JWT | snake_case | `{data}` / `{data,next_marker}` / `{error}` |
| MCP | `/mcp` | OIDC JWT **or** a `pk.sk` key (personal access token or admin hmac key) | MCP tools | JSON tool results |
| E2B-compatible sandboxes | `/e2b` | a project's sandbox API key (`X-API-Key`) | E2B's camelCase | E2B's `{code, message}` errors; [e2b-compat.md](e2b-compat.md), document at `/e2b/openapi.json` |

All four are described in `docs/auth.md`. The management port (`:8081`, separate
from the API port `:8080`) additionally serves health probes and, only with
`PROXCLOUD_JOBS_DEBUG_TRIGGERS` on, the unauthenticated debug triggers — see
`docs/configuration.md`. On demand in production, use `/api/v1/admin/job/*`
([jobs-scheduling.md](jobs-scheduling.md#manual-triggers)).

## Customer API — `/api/v1`

Authenticated with a customer (`auth.main`) JWT or that customer's personal access
token; operator-console JWTs reach `/api/v1/admin/**`. Responses use the `CustomHttpResponse`
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

Proxmox connection test: `POST /api/v1/admin/service/proxmox/auth`
(`admin:service:read`) tests a connection and stores nothing. The body takes
`apiUrl`, `apiUrls` (other nodes or a VIP to fail over to), `tokenId`,
`tokenSecret`, `caCert`, `fingerprint` and `insecureTls`. With
`?externalServiceId=<id>` and a blank `tokenSecret` it tests the stored provider
with its stored secret. For a caller with `admin:service:manage`, the fields the
body does carry then replace the stored ones for this test (an empty string
clears one), so an edited form can be tested before it is saved. A caller with
read access only always gets the stored connection: the stored secret is never
sent to an address such a caller chose. The route is a read and is not in the
admin audit log, except for this: a test that sends the stored secret with an
`apiUrl`, `apiUrls`, `caCert`, `fingerprint` or `insecureTls` other than the
stored one writes a `CONNECTION_TEST` event on the provider (`EXTERNAL_SERVICE`),
whatever the outcome, with the actor, the fields that differ and the addresses
tested. The secret itself is not in the event.

By default the first failed check is a `400` with the reason, and a pass is
`200 {version, nodes, storages, sdnZones, pciMappings, warnings}`. The installer
depends on this. With `?checks=all` a failed check is not an error: the answer
is `200` and also has `checks`, one row per check that could still run:

```json
{"data": {"version": "9.2.1", "warnings": [], "checks": [
  {"id": "connect", "status": "ok", "message": "Reached Proxmox VE 9.2.1."},
  {"id": "firewall-datacenter", "status": "fail", "message": "Proxmox preflight failed: the datacenter firewall is disabled, …"},
  {"id": "privileges:/pool", "status": "warn", "message": "Token lacks VM.Audit on /pool (see docs/proxmox-permissions.md)"}]}}
```

`status` is `ok`, `warn` (it works, but wants attention) or `fail`. A check with
several findings puts one on each line of `message`. Rows after `connect` or
`token` are missing when that one failed, because nothing else can run.

| `id` | Checks | Worst status |
|------|--------|--------------|
| `connect` | The address answers and its certificate is trusted | `fail` |
| `token` | Proxmox accepts the token; `warn` for a `root@pam` token | `fail` |
| `firewall-datacenter` | The datacenter firewall is on | `fail` |
| `firewall-nodes` | Every online node runs the nftables firewall | `fail` |
| `public-networks` | Stored provider: no public network on a management bridge | `fail` |
| `privileges:<path>` | The token holds the `ProxCloudPlatform` privileges on that ACL path (`/`, `/nodes`, `/pool`, `/sdn`, `/sdn/zones`, `/storage`, `/mapping/pci`). One row `privileges` when they cannot be read | `warn` |
| `store-reach` | Stored provider: tenant networks cannot reach the network stores | `warn` |
| `boot-iso` | Stored provider: the Kubernetes bootstrap ISO store is usable | `warn` |
| `version` | Proxmox VE 9.2 or later | `fail` |
| `template-pool` | The pool `pc-templates` exists | `warn` |
| `import-store` | Some storage takes `import` content. Stored provider with `importStorage`: that storage exists, takes it, and the token can delete files there | `warn` |
| `zone` | Stored provider: a default tenant network zone is configured, and Proxmox has no zone of that id with another type | `fail` |
| `cidr-pool` | Stored provider: `cidrPool` is an IPv4 range of /24 or larger; `warn` when it overlaps a management network of the nodes, or when a node's networks could not be read (that node was not compared) | `fail` |
| `storage-profile` | Stored provider: a default storage profile is configured and exists | `fail` |
| `frr` | Stored provider with an EVPN zone: `frr-pythontools` is installed on every online node | `warn` |

The rows from `version` down exist only in `checks`: they never turn the default
call into a `400` and never add to `warnings`.

With `?checks=all` a caller with `admin:service:manage` also gets `bridges`,
the bridges of every node as `{node, iface, type, cidr, management}`
(`management`: the bridge carries the node's own address or a default gateway,
so it cannot be a public network), and, when the connection was refused over the
certificate, `presentedFingerprint`: the SHA-256 of the certificate the server
presented, in the form `fingerprint` takes. Pinning it without comparing it to
`pvenode cert info` on the node is trust on first use.

Proxmox setup script: `GET /api/v1/admin/setup/proxmox-script`
(`admin:service:read`) answers, as `text/plain`, the shell script an operator
runs as root on a Proxmox node to create the portal token's role, pools, user,
token and ACLs. Query: `tokenId` (default `proxcloud@pve!platform`), `gpu`
(`true` adds the `/mapping/pci` ACL; default `false`) and `importStorage` (adds
`PVEDatastoreAdmin` on that store). A value that is not a plain id is a `400`.
The script and what it does are in
[proxmox-permissions.md](proxmox-permissions.md#portal-token-script).

Cloud service selection: the project cloud routes (`/project/{id}/cloud`, its
actions and deletes, `quota-usage`, `gpu-capacity`, `public-networks`, the S3
key routes) run against the service named by the `x-service-id` header (with
`x-region-id`), else `?serviceId=`. A named service the project is not attached
to is refused with `400 Project is not attached to service <id>` before any
provider is called. A provider registered after a project was created is not
attached to it until an operator attaches it (admin project page → Cloud
services → Attach provider). Without a named service the call goes to the
project's Proxmox service, else its first attached one.

Opening a project: `POST /api/v1/project/{id}/init` provisions an ENABLED
project on its services, and enables a DISABLED one nothing holds any more.
It answers `200 {}` only when the project is up. A project it leaves as it is
answers `409` with the reason in `errors.details`:

```json
{"errors": {"code": 409, "message": "This project is not active yet: …",
  "details": {"status": "DISABLED", "reason": "billing_profile_inactive", "billingProfileStatus": "NEW"}}}
```

| `reason` | Cause |
|----------|-------|
| `billing_profile_inactive` | The paying billing profile (the project's own, else the organization's) is not ACTIVE yet (NEW: billing details or a payment method missing). `billingProfileStatus` gives its status. |
| `billing_profile_suspended` | The paying billing profile is SUSPENDED (unpaid balance). |
| `billing_profile_missing` | No paying billing profile can be found. |
| `disabled_by_operator` | An operator disabled the project; only an operator enables it. |
| `scheduled_for_deletion` / `deletion_in_progress` / `deleted` | The project is on its way out. |
| `not_enabled` | Any other case where the project is not ENABLED. |

### Platform setup — `/api/v1/admin/setup`

Operator console surface for bringing a new install to its first tenant.

**Defaults.** `GET /api/v1/admin/setup/defaults?serviceId=` previews, and
`POST /api/v1/admin/setup/defaults` applies, the documents an install needs
before tenants can use a Proxmox provider. They are the files in `deploy/seed`,
compiled into the API (`deploy/seed/seed.go`), so no checkout and no
`seed-operator.py` run is needed. `seed-operator.py` is unchanged and still
works; unlike it, this route never updates a document that exists.

| Item | What an apply adds | Permission to apply |
|------|--------------------|---------------------|
| `flavors` | The default flavors whose slug is not in the catalog | `admin:service:manage` |
| `pricePlan` | The public rate card, enabled, when no enabled PUBLIC plan exists | `admin:price_plan:manage` + `admin:billing_config:read` |
| `quota` | The default project quota limits the provider does not set yet | `admin:service:manage` |
| `region` | The provider's region row in the platform configuration | `admin:platform_config:update` |

`flavors` and `pricePlan` need no provider. `quota` and `region` need
`serviceId` (a Proxmox provider) and answer `blocked` without one.

The preview needs all four reads (`admin:service:read`,
`admin:price_plan:read`, `admin:platform_config:read`,
`admin:billing_config:read`) and changes nothing:

```json
{"data": {"currency": "USD", "items": [
  {"id": "flavors", "state": "todo", "message": "3 of the 3 default flavors are missing", "preview": [{"slug": "s1-small", "vcpus": 1, "ramMb": 1024, "diskGb": 10}]},
  {"id": "pricePlan", "state": "todo", "message": "…", "preview": {"currency": "USD", "plan": {"name": "Public rate card", "enabled": true, "accessMode": "PUBLIC"}, "rules": []}},
  {"id": "quota", "state": "blocked", "message": "Needs a Proxmox provider (serviceId)"},
  {"id": "region", "state": "blocked", "message": "Needs a Proxmox provider (serviceId)"}
]}}
```

`state` is `todo` (an apply would write `preview`), `done` (nothing to add) or
`blocked` (the message says what is missing). `preview` is exactly what an
apply would write for the item.

The apply body is `{"serviceId": "…", "items": ["flavors", "pricePlan", "quota", "region"], "confirmRates": true}`.
`items` is required. Unknown fields and unknown items are refused with `400`.
It answers one result per requested item, with `state` `created`, `skipped`
(nothing was missing) or `blocked`.

- **Create-only.** An apply adds what is missing and never changes or deletes a
  document that exists. Applying twice leaves the same documents. One
  transaction under one advisory lock does the whole apply, so two operators
  applying at once get one price plan, and an apply that fails writes nothing.
- **Rates need a confirmation.** The plan is created enabled, so its rates are
  billed from the next charge run. `pricePlan` is refused with `400` unless the
  body carries `"confirmRates": true`. Show the previewed rules to the operator
  before sending it. The rates are amounts in the billing configuration's base
  currency; the preview says so when that is not the currency they were written
  in (USD), and refuses (`blocked`) when no base currency is set.
- **One public plan.** Every enabled PUBLIC plan is billed, so the default plan
  is skipped when one exists, whatever its name. With `serviceId`, only a plan
  that applies to that provider (no `serviceProviders`, or one naming it) counts:
  when every enabled PUBLIC plan is scoped to other providers the item is
  `blocked`, because the unscoped default would bill those a second time. Add
  the provider to one of those plans, or add a plan scoped to it. A plan with the
  default's name that is disabled or not public blocks the item instead of
  getting a twin.
- **Rules.** The plan gets the rules for `instance`, `instance_traffic`,
  `volume`, `floating_ip`, `image`, `server_snapshot` and `server_protection`.
  GPU rules are added only when a Proxmox provider maps GPUs (`config.gpus`),
  the `share` rule only when a TrueNAS provider exists, the `bucket` rule only
  when an object-store provider exists. The Kubernetes, database and sandbox
  rules of the file are not applied here. Rules are not added later to a plan
  that exists: after adding GPUs or a share provider, add those rules under
  System → Price plans, and check `GET /api/v1/admin/service/{id}/unpriced-flavors`.
- **Quota.** The stored default quota wins: only limits it does not set are
  added (a limit stored under a legacy key such as `instances`, or set to `-1`,
  counts as set), and the per-model `gpu` limits are kept.

**Status.** `GET /api/v1/admin/setup/status?serviceId=&live=1` says how far
the install is from its first tenant, for the Dashboard card and the setup
page. It needs the same four reads as the defaults preview and changes nothing.
`serviceId` names a Proxmox provider (`404` when there is none, `400` for
another type); without it the first Proxmox provider by name is used, and with
none saved every step that needs one is `blocked`.

```json
{"data": {"serviceId": "svc-1", "live": false, "steps": [
  {"id": "connect", "state": "done", "summary": "Provider lab is saved", "items": [{"id": "provider", "state": "done", "message": "Provider lab is saved"}]},
  {"id": "catalog", "state": "todo", "summary": "No flavor in the catalog: tenants cannot create a server", "items": []}
]}}
```

The steps, in order: `connect`, `network`, `storage`, `templates`, `catalog`,
`golive`. An item's `state` is `done`, `attention` (works, but look at it),
`todo`, `blocked` (needs another step first) or `unknown` (could not be read).
The worst item decides the step's state and its `summary`; a `blocked` item
makes the step `todo`, and a step is `blocked` only when it cannot start.

- **Without `live`** only the database is read: the provider, its
  `cidrPool` and default zone, storage detection, profiles and the default
  profile, flavors and the flavors no rule prices, the enabled PUBLIC plans
  that apply to the provider and their `instance` rules, the default quota
  (`done` only when it sets every limit the defaults item would add, so a quota
  with only per-model `gpu` limits is `todo`), the region row, the business name, address and country of the billing
  configuration, and the attach policy. Rows are not counted: the Compose seed
  writes the billing and platform configuration, and a fresh install shows no
  step done. `templates` is `unknown` unless the provider configuration pins a
  template.
- **With `live=1`** the cluster is asked too: the connection test, run as
  `POST /admin/service/proxmox/auth?checks=all&externalServiceId=` runs it for
  a read-only caller, every row as an item with the row's id (`ok` is `done`,
  `warn` is `attention`, `fail` is `todo`); the nodes with an active `import`
  store; the published OS images; and the imported templates by lifecycle
  state. `zone`, `cidr-pool`, `frr` and `store-reach` go to `network`,
  `storage-profile`, `import-store` and `boot-iso` to `storage`, every other row
  to `connect`. A row that asks what a database item asks takes that item's
  place and id: `zone` the item `zone`, `cidr-pool` the item `cidrPool`,
  `storage-profile` the item `defaultProfile`. A cluster that does not answer
  is a failed `connect` item, not a failed request, and the database items stay.
- **Go live** waits for `connect`, `network`, `storage` and `templates`
  (`blocked` until none is `todo`). While one of them is `unknown` (without
  `live`, `templates` is unless a template is pinned) go live is `unknown` too,
  not `todo`: ask again with `live=1` before turning on the attach policy.
  Without `live`, `connect` is `done` once the provider is saved; only `live=1`
  runs the connection checks. An open catalog and a job scheduler that is
  off (`PROXCLOUD_JOBS_SCHEDULER_ENABLED`) are `attention` items: they block
  nothing. An attach policy that is on while steps are open is `attention`.

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

- **client tools** (`clients` realm, or a client personal access token) — read/act
  on the caller's own projects and billing: resources and costs, servers (create,
  power actions, backups, snapshots, protection), volumes, Kubernetes clusters,
  databases, DNS records, load balancers (list), floating IPs, sandboxes, invites.
  See `tools_client.go`.
- **admin tools** (`master` realm or an Admin API `pk.sk` key) — the operator
  toolset. See the `tools_admin*.go` files.

Each tool maps to a real REST endpoint and is dispatched in-process through the
full router, so authorization, DTO shapes, and audit are identical to a direct
API call; `TestMCPToolRoutesExist` (`internal/server`) checks every tool's route
against the router. The tool catalog is documented at `/docs/reference/mcp-server`
in the admin console.

## Error model

| Status | Meaning |
|--------|---------|
| `400` | Bad request — malformed body, invalid query param, or a rejected argument. |
| `401` | Not authenticated — missing/invalid token (empty body, `WWW-Authenticate: Bearer`). |
| `403` | Authenticated but not authorized — e.g. wrong realm at the Admin API gate, or an MCP principal whose realm has no toolset. |
| `404` | Resource not found. |
| `409` | Conflict — e.g. creating a resource that already exists, or a project `/init` cannot bring up. |
| `501` | The endpoint exists but the capability is **not enabled / not configured in this deployment** — the code path drives a subsystem (cloud orchestration, an external provider) that this deployment has not wired up. It is a deliberate "not implemented here" response, not a runtime error. |

The customer API error envelope is `{"errors": {"code", "message"}}`; an error a
client can act on may also carry `errors.details` (an object, e.g. the project
`/init` 409 above). Clients must ignore details they do not know.

Unmatched routes and disallowed methods on the customer API still return the
`CustomHttpResponse` error envelope via `NotFoundHandler` / `MethodNotAllowedHandler`.
On the Admin API, errors use the `{ "error": { code, message } }` shape.
