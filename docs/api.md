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

Deleting a project: `DELETE /api/v1/project/{id}/now` (and the operator's
`DELETE /api/v1/admin/project/{id}/now`) only flags the project; the deletion job
of the scheduler tears its cloud resources down and removes it. While the scheduler
is off (`PROXCLOUD_JOBS_SCHEDULER_ENABLED`, and no debug triggers) the call answers
`409` and changes nothing, instead of leaving the project `DELETE_IN_PROGRESS`.

Networks on a Proxmox provider: `POST /api/v1/project/{id}/cloud` with
`{type: NETWORK, data: {name, cidr?, enableDhcp?, snat?}}`. Both flags default to
`true`, except in a `vlan` zone, where Proxmox does no DHCP and no SNAT: they
default to `false`, the answer says `dhcp: false` and `snat: false`, and `true`
for either is a `400` (so are the actions `ADD_SUBNET` and `SET_SNAT` with
`snat: true`). The `gateway` of such a network is the address the site's router
holds.

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

Security group rules on a Proxmox provider: actions on
`POST /api/v1/project/{id}/cloud/{resourceId}/action`, where the resource is a
`SECURITY_GROUP` (`internal/platform/project/cloud_writes_pve.go`). A group is an
allow list for inbound traffic. Nothing here is lab-verified on PVE 9.2 in this
form; the tests run against a fake Proxmox.

- `LIST_RULES`: the rules, from the cache row. Each has `id`, `direction`
  (`ingress` or `egress`), `protocol`, `port_range` (`22` or `8000:8100`),
  `remote_ip_prefix` (the source of an ingress rule, the destination of an egress
  rule), `description` and `pos`. The `id` is
  `<group>-<position>-<fingerprint of the rule's content>`: it names that rule at
  that position and stops matching when the rule moves or changes.
- `ADD_RULE`, data `direction` (`ingress`, the default, or `egress`), `protocol`
  (`tcp`, `udp`, `sctp`, `icmp`, `gre`, `esp`, `ah`; absent or empty for any),
  `portRangeMin` and `portRangeMax` (whole numbers from 1 to 65535, destination
  ports; they need `tcp`, `udp` or `sctp`), `remoteIpPrefix` (a CIDR without host
  bits, or one address; absent for anywhere), `description`. Every rule is an
  allow rule. Any other key is a `400` that names it (`remoteGroupId`, `action`,
  `log`, `priority` and the rest: the provider cannot honour them, and dropping
  one would store a wider rule than asked), and so is a value that fails the
  checks above. What only Proxmox refuses comes back as a `400` with its text.
  An egress rule is stored with the CIDR as its destination and **restricts
  nothing**, because every server's outbound policy is ACCEPT: the answer then
  carries `result.warning` saying so.
- `DELETE_RULE`, data `ruleId` (the `id` from `LIST_RULES`). Without `ruleId` the
  answer is `400`; `pos` is no longer taken, because a position names whatever
  rule sits there now. When no rule of the group has that id any more (a rule was
  added or deleted since the list was read, or the id is of the shape
  `<group>-<position>` that v0.8.0 listed) the answer is `409`, nothing is
  deleted and the cache row is refreshed: list the rules again and repeat the
  delete with the current id. The delete also sends Proxmox the digest of the
  rule list it was checked against, so a change between the check and the delete
  is refused by Proxmox and answered `409` too.
- There is no rule update: `UPDATE_RULE` is a `400`. Delete and add.

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
| `mint-scope` | Only when it applies: the token holds `User.Modify` or `Permissions.Modify` on `/`, `/access` or `/access/groups` (it could create an API token for any user, `root@pam` included) or `Permissions.Modify` on `/pool` (it could write any role on every pool). The message says what to run | `warn` |
| `store-reach` | Stored provider: tenant networks cannot reach the network stores | `warn` |
| `boot-iso` | Stored provider: the Kubernetes bootstrap ISO store is usable | `warn` |
| `version` | Proxmox VE 9.2 or later | `fail` |
| `template-pool` | The pool `pc-templates` exists. When it does not, the first template import creates it (`Pool.Allocate` on `/pool`); `pveum pool add pc-templates` is only for a token without that privilege | `warn` |
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
`PVEDatastoreAdmin` on that store). `managedServices=true` adds the identities
of managed services: the CAPI and CSI users and tokens, pool `proxcloud-seed`,
their roles and ACLs, with `bootStorage` (required) and `isoStorage` (default
`local`), and with `importStorage` the mint role on that store too; either store
without `managedServices=true` is a `400`. A value that is not a plain id is a
`400`. The script and what it does are in
[proxmox-permissions.md](proxmox-permissions.md#portal-token-script) and
[Managed services](proxmox-permissions.md#managed-services).

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

Managed Kubernetes status: a `KUBERNETES_CLUSTER` resource's `data.cluster` carries, next to `status`, two optional
fields that say why a cluster is not there yet. Both are absent on a settled cluster, and on an install whose
management-cluster RBAC predates them.

```json
{"cluster": {"status": "PROGRESSING",
  "attention": {"code": "control_plane_stalled", "since": "2026-11-17T09:12:00Z"},
  "node_groups": [{"name": "workers", "count": 2, "ready_replicas": 1, "phase": "ScalingUp",
    "progress": {"code": "not_joined", "since": "2026-11-17T09:20:00Z", "machines": 1, "attention": true}}]}}
```

`progress.code` is one of `provider_access_refused`, `no_capacity`, `vm_create_failed`, `vm_create_retrying`,
`waiting_control_plane`, `creating_vm`, `starting_vm`, `not_joined`, `node_not_ready`, `provisioning`;
`attention.code` is `control_plane_stalled` or `provider_access_refused`. `attention: true` means no progress for a
bounded time or an error that is not retried; it clears by itself. `status` is unchanged by either.
`cluster.attention` carries control-plane and provider codes only. A worker that stalls is reported on
`node_groups[].progress.attention` and leaves `cluster.attention` absent, so a client that asks "does this cluster
need attention" reads both: `cluster.attention` is present, or any `node_groups[].progress.attention` is true.
Both are evaluated at a provider sync, not when the resource is read: with the default 15-minute sync a flag can
show up to one interval after its bound has passed. The codes, their
texts and the bounds: [managed-k8s.md](managed-k8s.md#provisioning-status-reason-and-needs-attention).

Managed Kubernetes placement: on a cluster whose chart is 0.1.13 or later, `node_groups[i].placement` (`{"pve1": 2,
"pve3": 1}`) says how many replicas of the group each Proxmox node runs. It is decided by the server at create,
`SET_NODE_GROUPS` and `UPGRADE`, and is never read from a request; neither is a node group's `imageId`, which is
ignored from v0.8.0 (the image comes from the provider's version matrix and the group's `imageVariant`). The node
names are the ones `image_id` already carried. Create, `SET_NODE_GROUPS` and `UPGRADE` can answer `409` when the new
workers do not fit or the cluster's storage is on no node that holds the image, and `503` when Proxmox cannot be read:
[managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox).
A node group of `SET_NODE_GROUPS` has the shape of a node group at create: `rootVolumeGiB` may be left out when the
Kubernetes provider has a default root disk size (`config.cluster.rootVolumeGiB`), and a new group then gets the
larger of that default and the flavor's disk. A group that already runs keeps the root disk size it has when the
field is left out. Without a provider default a group without it answers `400`, at create too.

`GET /api/v1/admin/service/{id}/k8s-clusters/{clusterId}/machines` (`admin:service:read`) is the operator's detail,
read live from the management cluster:

```json
{"proxmoxCluster": {"conditions": [{"type": "ProxmoxAvailable", "status": "True"}]},
 "machines": [{"name": "pc-c-1-workers-pve2-x7k2p-9d4tq", "nodeGroup": "workers", "phase": "Provisioned",
   "lastUpdated": "2026-11-17T09:20:00Z", "nodeRef": "", "vmid": 142, "proxmoxNode": "pve2",
   "addresses": ["10.20.0.12"], "code": "not_joined", "attention": true, "failureMessage": "",
   "conditions": [{"type": "NodeHealthy", "status": "Unknown", "reason": "NodeDoesNotExist",
     "message": "Waiting for a Node with spec.providerID proxmox://142 to exist", "lastTransitionTime": "2026-11-17T09:20:00Z"}]}]}
```

`conditions` joins the Machine's and its ProxmoxMachine's. Messages have credentials cut out and are clipped to 400
characters. `404` for a cluster that is not this provider's; `502` with "re-apply deploy/mgmt-cluster/rbac.yaml" when
the service account may not read Machines.

Publishing a node image on every node: `GET /api/v1/admin/template-updates/publish-all?serviceId=…&digest=sha256:…`
(`admin:service:read`) reports one artifact's state on every node of a Proxmox provider, and
`POST /api/v1/admin/template-updates/publish-all` with `{"serviceId","package","digest","confirmDigest"}`
(`admin:service:manage`) advances it one step: it publishes what passed its smoke test and starts at most one import
or smoke test.

```json
{"digest": "sha256:…", "kubernetesVersion": "1.35.10",
 "nodes": [{"node": "pve1", "state": "published", "vmid": 9210},
           {"node": "pve2", "state": "smoke_queued", "vmid": 9211},
           {"node": "pve3", "state": "missing"},
           {"node": "pve4", "state": "skipped", "code": "node_down", "message": "The node is not online"}],
 "versionsLine": "\"1.35.10\": \"pve1=9210\"", "complete": false, "nodesMissing": [], "next": "wait",
 "actions": ["published pve1/9210", "smoke test queued pve2/9211"]}
```

`complete` is false while a node is down (`code` `node_down`): its copy cannot be read and is not in `versionsLine`.
`next` is `advance` (post again), `wait` (a job of the provider runs), `blocked` or `done`. `blocked` means a step
failed on a node: the node's `message` says what to do. An advance never repeats a smoke test or an import
(`smoke_failed`, `smoke_not_started`, `import_failed`), so posting again changes nothing until that is done. A
`publish_failed` is this post's own refused publish, and the next post tries that publish again.

Errors, each with a `code` in `details`: `409` `busy` (another advance of that provider runs), `digest_withdrawn`,
`vm_updates_disabled`, `provider_missing`, `provider_disabled` or `provider_incomplete`; `400` `confirm_digest` (the
POST without a package or with a `confirmDigest` that is not the digest; a malformed digest); `502` `pve_unreadable`
(the provider's nodes, VMs or storage could not be read; nothing was changed); `503` `pve_unwired`. An advance that
has to start an import can also answer what the import does, without a `code`: `429` (GitHub rate limit), `502`
(GitHub), `409` (the integration is not configured) or `400` (the artifact is refused).

When an advance has already published a node and then cannot start its next job, it answers `200` with the report,
`actions`, and `"error": {"code", "message"}` (`rate_limited`, a lifecycle code, or `next_step_failed`), so what it
did is shown and audited; `next` is then `advance`. The states and the rules:
[template-updates.md](template-updates.md#publish-on-all-nodes).

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
{"data": {"serviceId": "svc-1", "live": false, "seedUrl": "https://api.example.com", "steps": [
  {"id": "connect", "state": "done", "summary": "Provider lab is saved", "items": [{"id": "provider", "state": "done", "message": "Provider lab is saved"}]},
  {"id": "catalog", "state": "todo", "summary": "No flavor in the catalog: tenants cannot create a server", "items": []}
]}}
```

The steps, in order: `connect`, `network`, `storage`, `templates`, `catalog`,
`golive`, `managed`. `managed` is optional and read from the database only: the
phase of the provider's managed-services installation (`todo` with the switch
off, without an installation, for a draft or a removed one; `attention` during
a run and after a failed one; `done` when ready). It is computed after `golive`
and is not one of its inputs. An item's `state` is `done`, `attention` (works, but look at it),
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
  state. `zone`, `cidr-pool`, `frr`, `public-networks` and `store-reach` go to
  `network`, `storage-profile`, `import-store` and `boot-iso` to `storage`,
  every other row to `connect`. A row that asks what a database item asks
  takes that item's place and id: `zone` the item `zone`, `cidr-pool` the item `cidrPool`,
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
- **Server seeds.** `network` has the item `seeds`: `done` once the operator
  chose how servers get their seed, that is `config.metadata.forceIso` is
  `true`, or is `false` with a seed URL, or `config.metadata.url` is set;
  `attention` otherwise, saying that servers on a DHCP network fetch their seed
  from the API base URL unless ISO seeds are on, and must reach it and trust its
  certificate. With no seed URL at all it stays `attention`, `forceIso: false`
  included, and says every seed goes on an ISO. It never
  holds go live back, and nothing is probed: only the operator knows what tenant
  servers reach. `seedUrl` is where a URL seed is fetched from:
  `config.metadata.url`, else the API's own base URL
  (`PROXCLOUD_SELF_API_BASE_URL`); `""` means none, so every seed goes on an ISO
  ([Server seeds](cloud-integration.md#server-seeds)).

### Managed-services platform — `/api/v1/admin/platform`

**BOM.** `GET /api/v1/admin/platform/bom` answers the platform bill of
materials this release carries (`deploy/platform/platform-bom.yaml`, compiled
into the API; [ADR 0009](adr/0009-managed-services-platform-lifecycle.md)):
`release`, the Kubernetes version and node image, the seed image and k3s
release with their sha256, the Cluster API providers, the tenant charts, and
every component with its cluster, wave, namespace, node group and chart
(`repo`, `name`, `version`, `digest`). `sha256` is the hash of the file, the
key a run records it by. It needs `admin:service:read` and changes nothing.

The file pins exact versions only. A digest is the chart's OCI manifest
digest, or for an `https://` repository the `digest` of that version in its
`index.yaml`. A `-dev` release (main) may carry `pending` digests for
the project's own packages and for a chart at a moving `0.0.0+` version
(today none: the third-party charts come from
`ghcr.io/mutex-solutions/charts-mirror` at exact versions, and a chart of
that mirror may never be `pending`); the validation refuses `pending` and
`0.0.0+` in any other release. A run imports the BOM's two images and builds the
seed from it; nothing else installs from the BOM yet.

**Installations.** With `PROXCLOUD_PLATFORM_ENABLED=true` (default off; off,
these routes answer `404` with `errors.details.code` `platform_disabled`) an
installation records what "Enable managed services" builds on one Proxmox
provider (`internal/platformlife`, collection `platformInstallation`, one per
`serviceId`). Reads need `admin:service:read`, everything else
`admin:service:manage`. Refusals carry `errors.details.code` and, where there
are several, `errors.details.problems`.

- `GET|POST /installations`: list; create `{name, serviceId, provisionedBy?,
  layout?, spec}` in phase `draft`. `spec` has `pveApiUrl`, `storage {boot,
  iso, import}` (import defaults to the provider's `importStorage`), `network
  {bridge, prefix, gateway, dns[], mtu}`, `seed {mode, pool, node, address,
  cores, memoryMiB, diskGiB, sshKeys[], apiSources[]}`, one cluster `{name,
  roles, endpoint, nodeRange, metallbRange, controlPlane, nodeGroups[]}` with
  exactly the groups `system`, `kamaji` and `dbaas`, and `nodeImage {mode,
  templates}`. A spec that breaks a rule, or overlaps the provider's
  `cidrPool`, `sandboxCidrs` or public networks, or puts the MetalLB range in
  `blockedCidrs`, is a `400 invalid_installation`; so is a `storage.import`
  equal to `storage.boot` (the portal token's role on the import store would
  reach the platform's disks), also when the empty `import` took that store
  from the provider. `layout: separate`, `provisionedBy: installer`
  and `seed.mode: existing` are refused until a later release.
- `GET|PUT /installations/{id}`: the PUT replaces `name` and `spec` and names
  the `rev` it read (`409 rev_conflict` otherwise). It is refused during a run
  (`409 run_active`) and for a fixed field (`409 fixed_fields`): `serviceId`
  always; once a run succeeded also `provisionedBy` (but a stored `installer`
  may become `portal`), `layout`, `name`,
  `spec.network`, the seed's mode, pool, node and address, and the cluster's
  name, endpoint and node groups. A draft can be corrected; no route deletes
  one.
- `PUT /installations/{id}/credentials`: `{capi: {tokenId, secret}, csi:
  {tokenId, secret}}`, either or both, write-only. The ids must be
  `capmox@pve!capi` and `csi@pve!csi` (the managed-services script's), the
  secrets UUIDs. They are sealed with the data key in `platformSecret`; without
  `PROXCLOUD_ENCRYPTION_DEFAULT_KEY` the route answers `409 no_encryption_key`.
  No route returns a secret: an installation shows `credentials.{capi,csi}.tokenId`.
- `POST /installations/{id}/preflight`: read-only, not audited; `{ok, checks}`
  with rows `flags`, `spec`, `network`, `isolation`, `tenant-network`, `credentials`,
  `privileges:capi`, `privileges:csi`, `privileges:kubemint`, `mint-scope`, `pool-fence`,
  `token-scope`, `iso-store-visibility`, `boot-iso-grant`, `boot-iso`, `placement` and
  `images`. A row's `status` is `ok`, `fail` or `warn`; `ok` in the answer is
  false only when a row failed. `tenant-network` never fails. It reads the provider's default
  zone in Proxmox: `warn` when that zone has DHCP, when it does not exist yet and is
  configured as a `simple` zone (ProxCloud creates one with DHCP), or when the zones
  could not be read; a tenant Kubernetes cluster create on a network of such a zone is
  refused with a `400`, except on a Kubernetes provider with shared workers
  (`cluster.shared`), which uses no tenant network. It is `ok` for a zone without
  DHCP, and absent when the provider names no zone.
  `isolation` fails, naming the CIDRs to add to `blockedCidrs`, when
  the seed, the API VIP or the node range is outside the blocked and management
  networks, and when the cluster firewall's isolation rules are not the ones the
  sync writes (a provider sync writes them without a project only for a provider
  that has an installation, in any phase, so `POST /installations` comes before
  the sync and the sync before this check; it writes none for
  a `cidrPool` over a management network, for rules in place for another pool
  for rules in place that drop something the provider's set does not or accept
  other sandbox endpoints, and for rules that are not the provider's set while
  a project's rules for the pool are in place, which the message names; and
  none for a provider with status `DISABLED`, which the message says); with no
  tenant network on the whole Proxmox cluster yet it passes. `mint-scope` fails
  when the portal token holds `User.Modify` or `Permissions.Modify` on `/`,
  `/access` or `/access/groups`, where it could create an API token for any user
  (`root@pam` included), or `Permissions.Modify` on `/pool`, and says to run the
  identities script again; and when `capmox@pve` or `csi@pve` is not in the group
  `proxcloud-kube`, with the `pveum user modify` line that puts it there. `token-scope` fails
  when `capmox@pve!capi` or `csi@pve!csi` holds a privilege on `/pool`, `/storage`
  or `/vms`, and names the `pveum acl delete` command, or which row of
  `pveum acl list` is the one to remove. A row that cannot be read fails.
- `GET /installations/{id}/plan`: the installed BOM against this release's
  (`installed`, `target`, `changes`, `blocking`); a version change not listed in
  `upgradeFrom` and the removal of a stateful component block, and an installed
  BOM this release does not carry blocks with `bom_unsupported` (no release
  upgrades between BOMs yet).

- `GET /installations/{id}?live=1`: the installation with `live`
  (`phase`, `components[] {name, cluster, sync, health}`, `error`, `code`),
  read from the seed's ArgoCD for this request and never stored. `live.phase`
  is `degraded` when the stored phase is `ready` and the seed does not answer,
  lists no Application, or one is not Synced and Healthy. When the seed could
  not be read, `live.code` is `seed_api_unreachable` and `live.error` says
  whether its API does not answer or refuses the stored credential, with the
  seed's address, VMID and node. The stored phase stays `ready`: a seed that
  went down shows only here.
- `POST /installations/{id}/runs` `{kind: apply|remove, confirm}`: `202 {runId}`. A run
  and its durable job (`platform.run/v1`, key `platform-run-<runId>`) are
  written in one transaction; `409 run_active` while another run of the
  installation is not finished, `409 bom_unsupported` when the installed BOM is
  not one this release carries. Without the runner (it starts only when
  `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true` too) the route answers
  `409 platform_disabled`; the reads below still work. A plain remove is
  `{"kind":"remove"}`. `confirm` is optional, belongs to a remove only and asks
  for more: when it is sent it must be `remove <installation name>`
  (`400 invalid_confirmation` otherwise, also on an apply). With it, a remove whose seed VM is gone deletes the VMs of pool
  `proxcloud-system` tagged `pc-platform-<id>` and the recorded volumes, and a
  remove whose workload cluster does not answer goes on without checking for
  tenants. Without it the first fails `seed_lost` and the second is retried,
  then fails `workload_api_unreachable`. A remove started again after an
  earlier one finished the cluster part and deleted the seed VM needs no
  confirmation.
- `GET /installations/{id}/runs[/{runId}]`: the newest runs, or one, with
  their stages (`state`, `retries`, `refused`, `deadline`, `items`, `code`,
  `message`, `receipt`). A run whose job ran out of attempts reads `failed` with code
  `job_dead`; a cancel not honoured yet reads `cancelling`. A run that fails
  before its stages run again (`portal_changed`, a provider that is gone) sets
  the stage it stood at `failed` with the run's code. `items` are
  `{id, state, message}`: the checks of stage 1, the Applications of stages 6
  and 9, the node templates (`template-<vmid>`: `ok` or `not checked`) and
  machines of stage 7, and in a remove what was deleted, left or not checked
  (`tenants`, one item per machine, `cluster`, `volume-<name>`, `volumes`,
  `vm-<vmid>`, `cds`, `secrets`). A stage keeps its items when it is done.
  A Kubernetes `401` or `403` from the seed or the platform cluster is counted
  in `retries` and tried again, because an API server answers so while it
  starts; `refused` is how many looks on end ended so (absent when none), and
  the fifth fails the stage with its code and the server's text.
- `POST /installations/{id}/runs/{runId}/cancel`: the runner stops at its next
  claim or write; the run ends `cancelled` and the installation `failed`.
  Nothing is deleted.

- `GET /installations/{id}/seed-kubeconfig` (`admin:service:manage`, although
  a GET): `{kubeconfig, expiresAt}`, a kubeconfig of the seed with a one-hour
  token of its `proxcloud-portal` service account (cluster-admin). Nothing
  stores it. The audit entry (`BREAK_GLASS`) has the actor, the installation
  and the expiry, never the token, and is written before the answer; `409
  no_seed` without a ready seed, `502` when the seed issues no token, `503`
  when the API has no audit service (no token is asked for). The answer
  carries `Cache-Control: no-store`.
- `POST /installations/{id}/seed-credential/rotate`: creates the service
  account's next token Secret (`proxcloud-portal-token-g<n+1>`), checks that
  its token is cluster-admin, stores it, then deletes the older token Secrets,
  which revokes their tokens. `409 run_active` during a run, `409 no_seed`
  without a ready seed. A rotation that stops before the new token is stored
  deletes the Secret it created. A `502` means the rotation did not finish:
  `status.seed.tokenSecret` names the stored credential, and a second call,
  or stage 6 of the next apply, deletes every other token Secret. A rotation
  that finished writes one audit entry of its own, action `ROTATE_CREDENTIAL`
  on the installation, with `fromSecret` and `toSecret` (the two Secret
  names) and never a token; the generic `CREATE` entry of an admin `POST` is
  not written for this route. A rotation that answered `502` after the new
  credential was stored writes the same entry with `outcome: FAILURE` and both
  names (older token Secrets may still be on the seed); a `502` that stored
  nothing, and every refusal, writes none.

An apply runs `validate`, `images`, `seed-vm`, `seed-api`, `seed-identity`,
`seed-platform`, `cluster`, `workload-access`, `workload-platform`, `register`
and `done`; a remove runs `remove-cluster`, `remove-seed` and
`remove-secrets`, and fails `tenants_exist` while tenant clusters or databases
run on the platform. Stage 10 (`register`) creates the Kubernetes and Databases
providers, or adopts the rows that carry `config.platformInstallationId`; on
an adopted row it rewrites `config.argocd`, `config.namespacePlacement`,
`config.cluster.pveApiUrl`, `config.database.{egressFence,lbAddressPool,storageClasses}`,
`config.database.engines.postgresql.replicas` (`[1]` when the dbaas group
spans fewer than three PVE nodes) and the kubeconfig, and leaves the name, the
status and every other key. A body the provider validation refuses fails
`provider_conflict`. A
remove sets both providers `DISABLED` and does not delete them; a later apply
adopts them and leaves them `DISABLED`. A seed of `mode: existing` is refused when
the installation is saved and by the `spec` check. A run's job is never replayed through
`/admin/durable-jobs` (`409`): a new run is the retry. None of the stages after
`seed-identity` has run against a real cluster yet.

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
