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
`{type: NETWORK, data: {name, cidr?, enableDhcp?, snat?}}`. `snat` defaults to
`true`, except in a `vlan` zone, where Proxmox does no DHCP and no SNAT: both
flags default to `false` there, the answer says `dhcp: false` and `snat: false`,
and `true` for either is a `400` (so are the actions `ADD_SUBNET` and `SET_SNAT`
with `snat: true`). The `gateway` of such a network is the address the site's
router holds.

`enableDhcp` follows the zone as Proxmox has it, read at the create: it defaults
to `true` only where the zone runs a DHCP server (a `simple` zone with
`dhcp: dnsmasq`, which is how ProxCloud creates one unless its config says
`"dhcp": false`). In every other zone (EVPN, VLAN, a `simple` zone without a
DHCP server) it defaults to `false`, no DHCP range is written on the subnet, and
`enableDhcp: true` is a `400` that says the zone has no DHCP server; servers
there get a fixed address the platform assigns. The `dhcp` a network answers, at
the create, after a subnet action and at every sync, is `true` only when its
subnet has a DHCP range and its zone runs a DHCP server. Up to v0.8.0 it
followed the zone's configured kind and the subnet alone, so a network in a
`simple` zone made without DHCP, or in an EVPN zone, answered `dhcp: true`; a
sync corrects such a row. The DHCP range those subnets still carry is left in
place: it does nothing without a DHCP server in the zone, and removing it would
take an SDN apply on a running site.

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
- `UPDATE_RULE`, data `ruleId` (the `id` from `LIST_RULES`) and the fields of
  `ADD_RULE`: the edit of a rule, as a delete and an add in one action. The new
  rule passes the same checks as in `ADD_RULE`, so an edit cannot store what an
  add refuses. Proxmox has no call that swaps a group rule for another, so the
  API adds the new rule first and then deletes the old one: there is no moment
  without either, and a refused add leaves the old rule as it was. A `ruleId`
  that no rule has any more is a `409` and nothing is written. When the group
  changes between the add and the delete the answer is a `409` whose message
  says that the new rule was added and the old one is still there; the cache row
  is refreshed, so the list shows both. An edit that changes nothing writes
  nothing. A rule made in Proxmox that holds what `ADD_RULE` cannot express (an
  action other than ACCEPT, a source port, a port list, a macro, an ICMP type,
  a remote on the near side) is not edited: the answer is a `409` that says to
  delete it and add a new one, because the edit would drop those settings. Not
  verified on a lab.

Load balancer readiness: `POST /api/v1/project/{id}/cloud/action` with
`{"action":"LIST_LOAD_BALANCER_WARNINGS"}` on a Proxmox service answers
`result`: a list of sentences, one for each thing the site lacks for a load
balancer and only the operator can change: no public network available to the
project, no public address left in the project's quota (`floatingIps`), the
appliance image (`config.lbImage`, default `debian-13`) not in the image
catalog. An empty list means the create is not refused for one of these. A
catalog that cannot be read is not reported as a missing image. The console
shows the list on the load balancers page and disables Create while it is not
empty. `LIST_SHARE_WARNINGS` on a TrueNAS service is the same question for file
shares; the console shows its answer on the file shares page and in the create
dialog, and leaves Create enabled.

Project invitations (`/api/v1/project-invites`, customer realm):

| Call | Who | Answer |
|---|---|---|
| `POST /invite` `{email, projectId}` | `project:manage_members` on the project | `202` with the invitation: `id`, `email`, `projectId`, `expiresAt` (24 hours), `mailConfigured`, `inviteUrl` |
| `GET /project/{projectId}` | a project admin sees every pending invitation of the project; another member of the project sees the ones they sent | `{mailConfigured, invites: [...]}`, each invitation as above |
| `DELETE /project/{projectId}/{inviteId}` | the same rule as the list | `200`; the link stops working and a queued mail is cancelled. An id the caller may not see is a `404`, as an id that does not exist |
| `GET /mine` | the invitee | the pending invitations of the caller's verified email |
| `GET /{token}`, `POST /accept/{token}`, `POST /decline/{token}` | the invitee | accept and decline need an account whose email the identity provider has verified as the invited one |

A caller who is neither a project admin nor a member of the project gets the
`404` of a project that does not exist on the list and the revoke.
`mailConfigured` is true when the invitation mail is handed to a mail gateway: a
gateway is set (the SMTP integration or `PROXCLOUD_MAIL_SMTP_HOST` with
`PROXCLOUD_MAIL_FROM`) and the durable invite outbox is off (it queues, and no
dispatcher sends from it). When it is false no mail was sent, and `inviteUrl` is
how the invitation reaches the invitee: the inviter copies it. `inviteUrl` is in
every answer, also when `mailConfigured` is true: the send is best effort, so a
set gateway does not mean the mail arrived (a wrong password, a disabled
template, an invitation made before the gateway was set). The link alone
does not let anyone join: accept matches the caller's verified email to the
invited one. An invitee can also sign in with the invited address without the
link; the console then shows the invitation on its start page (`GET /mine`). On
a site without mail the identity provider cannot send a verification mail
either, so the operator marks the address as verified in Keycloak
(Users, the user, "Email verified") for an account that registered itself.
An invitation made before v0.9.0 has no inviter stored: only a project admin
sees it.

Server power actions: `START`, `STOP`, `REBOOT` (asks the guest to restart) and
`HARDREBOOT` (resets the VM without the guest's help, for a guest that hangs;
the console offers it under More actions). `SHOW_CONSOLE_OUTPUT` answers an
empty string on Proxmox: the Proxmox API has no call that returns a VM's past
serial output, so there is no console log tail. Use the serial console.

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
| `csi-vmid` | No guest has VM id 9999, the id proxmox-csi names its volumes after (`vm-9999-pvc-<uuid>`). In use: the message says never to destroy that VM with "Destroy unreferenced disks owned by guest", and it is in `warnings` too. An id that could not be read is a `warn` row only | `warn` |
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
without `managedServices=true` is a `400`. `installation=<id>` answers the root
command of that managed-services installation instead: the stores are the
installation's, `tokenId` defaults to its provider's, and the command marks the
two platform tokens with the comment `proxcloud:<id>`, stops before any change
on one that exists without that comment and whose secret the portal does not
hold (a stored secret counts only when one read with it succeeds: a `401` from
Proxmox means not held, and a Proxmox that does not answer is a `502` with no
command), prints no secret, and ends with one line `PC-ENROLL:…` that carries the
secrets of the tokens it made (see the credentials route). It goes without
`managedServices` and the three storage parameters (`400`); an unknown id is a
`404`. A value that is not a plain id is a
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
    "progress": {"code": "not_joined", "since": "2026-11-17T09:20:00Z", "machines": 1, "attention": true,
      "machineNames": ["pc-c-1-workers-pve2-x7k2p-9d4tq"]}}]}}
```

`progress.machineNames` lists at most five machines of the group that need attention, by the name `REPLACE_MACHINE`
takes; it is absent when none does or none is made yet.

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

`/api/v1/admin/service/{id}/k8s-clusters/{clusterId}/worker-firewall` is the Proxmox VM firewall of one cluster's
workers ([managed-k8s.md](managed-k8s.md#worker-firewall); not verified on a lab). `GET` (`admin:service:read`) reports
and writes nothing; `POST` protects the workers and answers after it has watched the nodes, up to a minute, or `409`
with the reason when it refused or took the firewall off again; `DELETE` removes the firewall and the cluster's
security group, or answers `409` and changes nothing when a worker is on a node that is down or another server uses
the group (both need the provider manage permission). A `POST` cut short by a proxy timeout goes on to its end: read
the state with `GET`. `verified` is true only in the answer of the `POST` that put the firewall on and watched the
nodes; it is false on every `GET` and on a `POST` to a cluster that was already protected.

```json
{"clusterId": "pc-c-0a1b2c3d", "protected": true, "group": "g1a2b3c4", "groupName": "k8s-pc-c-0a1b2c3d", "enforced": true,
 "verified": true, "workers": [{"vmid": 141, "name": "pc-c-0a1b2c3d-workers-pve1-abcde-1", "node": "pve1", "status": "running", "secured": true}]}
```

`GET /api/v1/admin/service/{id}/k8s-clusters/{clusterId}/machines` (`admin:service:read`) is the operator's detail,
read live from the management cluster:

```json
{"proxmoxCluster": {"conditions": [{"type": "ProxmoxAvailable", "status": "True"}]},
 "machines": [{"name": "pc-c-1-workers-pve2-x7k2p-9d4tq", "nodeGroup": "workers", "phase": "Provisioned",
   "lastUpdated": "2026-11-17T09:20:00Z", "nodeRef": "", "vmid": 142, "proxmoxNode": "pve2",
   "addresses": ["10.20.0.12"], "code": "not_joined", "attention": true, "ready": false, "failureMessage": "",
   "conditions": [{"type": "NodeHealthy", "status": "Unknown", "reason": "NodeDoesNotExist",
     "message": "Waiting for a Node with spec.providerID proxmox://142 to exist", "lastTransitionTime": "2026-11-17T09:20:00Z"}],
   "joinError": {"errors": ["('scripts_user', RuntimeError('Runparts: 1 failures (runcmd) in 1 attempted'))"],
     "logTail": "+ kubeadm join 192.168.140.101:6443 --token [MASKED] ...\ndial tcp 192.168.140.101:6443: i/o timeout"}}],
 "healthChecks": [{"nodeGroup": "workers", "nodeStartupTimeoutSeconds": 1800, "maxUnhealthy": 1,
   "expectedMachines": 2, "currentHealthy": 1, "remediationsAllowed": 0}]}
```

`conditions` joins the Machine's and its ProxmoxMachine's. Messages have credentials cut out and are clipped to 400
characters. `404` for a cluster that is not this provider's; `502` with "re-apply deploy/mgmt-cluster/rbac.yaml" when
the service account may not read Machines.

`joinError` is present only for a machine with `code` `not_joined` and `attention` true, on at most five machines
of one answer: what cloud-init in the worker says, read at this request through the Proxmox guest agent
(`errors` of `/run/cloud-init/result.json`, `logTail` the last 40 lines of `/var/log/cloud-init-output.log`) and
never stored. kubeadm bootstrap tokens, CA hashes, certificate keys and other credentials are replaced by
`[MASKED]` or `[REDACTED]`. The read permission is enough for it: a caller with `admin:service:read` makes the portal
read these two files in the worker. `readError` instead says in general words why nothing could be read. `healthChecks` are
the node groups' MachineHealthChecks (cluster chart 0.1.14 and later), `[]` when there is none or the service
account may not read them. No tenant route carries either field.

`POST /api/v1/admin/service/{id}/k8s-clusters/{clusterId}/machines/{machine}/replace` (`admin:service:manage`),
body none or `{"confirm": true}`: deletes that worker Machine, so that Cluster API makes a new one on the same
Proxmox node; the node group's count, placement and the project's quota are not touched. `200`
`{"result": "REPLACING"}`. `409` with `details.reason` `last_ready_node` when the machine is the only Ready node of
its node group and `confirm` is not true; `409` when it is already being removed; `404` for a machine that is not a
worker of this cluster; `502` when the service account may not delete Machines, with what to apply
(`deploy/mgmt-cluster/rbac.yaml`, or platform chart 0.1.3). A tenant has the same as the
cluster action `REPLACE_MACHINE` with `{"machine": "<name>", "confirm": false}` on
`POST /api/v1/project/{pid}/cloud/{id}/action`, with the same answers (`{"result": "REPLACING"}`; the `502` there says to contact the operator); the machine's
name is the name of its server in the project, and `node_groups[].progress.machineNames` has the names of the
machines that need attention. [managed-k8s.md](managed-k8s.md#a-worker-that-does-not-join) has the
limits.

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
  `network`, `storage-profile`, `import-store`, `boot-iso` and `csi-vmid` to `storage`,
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
With `?sha256=<hash>` it answers the BOM with that hash that the release
carries, its own or the one of an earlier release from 0.8.0 on
(`deploy/platform/previous/`), which a platform that release installed stays
on; `404 bom_unsupported` when the release carries none with that hash.

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
  layout?, preset?, spec}` in phase `draft`. `preset` (`homelab`, `standard` or `custom`; also on the PUT of an
  installation that is not installed yet) fills what
  `spec` leaves out before the checks and is not stored: sizes, counts, the three node groups, the control plane's
  node (the seed's) and its count (`standard`: 3 where the installation's release builds three, else 1), and for
  `homelab` with every machine of the spec on one node and no bridge named a `routed` network on the vnet
  `pcplat0` ([the table](managed-services.md#presets-and-the-control-plane-count)). An unknown preset is a
  `400 invalid_installation`. On an installed platform a preset other than `custom` is a `409 fixed_fields`
  (below). `spec` has `pveApiUrl`, `storage {boot,
  iso, import}` (import defaults to the provider's `importStorage`), `network
  {mode, bridge, vlan, prefix, gateway, dns[], mtu}` (`mode`: `bridge`, also what a spec without it means, or
  `routed`, where `bridge` names a vnet the install makes and every machine is on one node; `vlan`: 1 to 4094, mode
  `bridge` only, and only with a BOM whose cluster chart is 0.1.14 or newer), `seed {mode, pool, node, address,
  cores, memoryMiB, diskGiB, sshKeys[], apiSources[]}`, one cluster `{name,
  roles, endpoint, nodeRange, metallbRange, controlPlane, nodeGroups[]}` with
  exactly the groups `system`, `kamaji` and `dbaas`, `nodeImage {mode,
  templates}`, and from v0.9.0 the optional `tenants {attach}`: who gets the two providers, `all` (the projects that
  exist and every new one), `new` (new projects only) or `none`. A spec without it means `none`, as before v0.9.0,
  and stays valid. Stage 10 of a run applies a choice once, when it differs from `status.tenantsAttach` (the one
  applied last): the attach policy of both providers, and for `all` one bulk attach of every project per provider.
  A create or a PUT whose spec says `all` also needs `admin:project:update` (`403` otherwise), as the bulk attach
  does. `status.kubernetesOffered` lists the Kubernetes versions that were offered on the Kubernetes provider: a run
  offers a version once.
  An installation is answered with `schedulerNote` while the job scheduler is off: the text of the Check row
  `scheduler`. A spec that breaks a rule, or overlaps the provider's
  `cidrPool`, `sandboxCidrs` or public networks, or puts the MetalLB range in
  `blockedCidrs`, is a `400 invalid_installation`; so is a `storage.import`
  equal to `storage.boot` (the portal token's role on the import store would
  reach the platform's disks), also when the empty `import` took that store
  from the provider. `layout: separate`, `provisionedBy: installer`
  and `seed.mode: existing` are refused until a later release. `controlPlane` is `{node, replicas, cores, memoryMiB,
  diskGiB}`: `replicas` is 1 or 3 (a spec without it means 1, and the answer names it), all machines on `node`; 3
  need a BOM whose cluster chart is 0.1.14 or newer (the preflight's `spec` row). `controlPlane.nodes` (machines on
  several nodes) is refused unless it is `[node]`, and `spec.sandbox {placement: shared|separate}`, the place for the
  sandbox service's placement, is refused until the release that builds it.
- `GET|PUT /installations/{id}`: the PUT replaces `name` and `spec` and names
  the `rev` it read (`409 rev_conflict` otherwise). It is refused during a run
  (`409 run_active`) and for a fixed field (`409 fixed_fields`): `serviceId`
  always; once a run succeeded also `provisionedBy` (but a stored `installer`
  may become `portal`), `layout`, `name`,
  `spec.network`, the seed's mode, pool, node and address, and the cluster's
  name, endpoint, `controlPlane.node` and node groups (their names: a group's `nodes`, count and size can change).
  Once a run succeeded a `preset` other than `custom` is the same `409 fixed_fields` (`problems`: `preset`), with
  every spec, and nothing is stored: a preset fills a draft. On such a platform a spec that leaves out
  `clusters`, a cluster's `controlPlane` or its `nodeGroups` keeps the stored ones (missing means keep, never
  a default); a `controlPlane` or a group that is sent is taken as sent and checked whole.
  A control plane count below the one a run built (`status.clusters[].controlPlaneReplicas`,
  written by stage 7 before it hands a count above one to the cluster; absent means one) is the same
  `409 fixed_fields`, with the reason as its message: 3 to 1. From 1 to 3 is saved, and a 3 no run built goes back to
  1. A count above one on an installation whose BOM names a cluster chart older than 0.1.14 is a
  `400 invalid_installation`; `controlPlane.nodes` equal to `[node]` is accepted and not stored. A draft can be corrected; no route deletes
  one. The answer of a POST or PUT may carry `siteChanges`: what the save wrote
  into the provider's `config.network`, so that nobody has to. `blockedCidrs`
  are the CIDRs added to the provider's `blockedCidrs` (the seed's address, the
  API address and the node range, as far as no entry covered them; never the
  MetalLB range). They are recorded in `config.network.platformBlockedCidrs`,
  and `blockedCidrsRemoved` are the recorded entries a later save took off
  because the settings no longer need them; an entry the operator wrote is
  never removed, and a Remove takes nothing off. `zoneWithoutDhcp` names the default
  tenant zone whose config now says `"dhcp": false`: a `simple` zone whose
  config said nothing about DHCP and that Proxmox does not have yet (a zone
  that exists is never changed). `problem` says what could not be done. The
  audit event of the save carries both under `providerNetwork`.
- `PUT /installations/{id}/credentials`: `{capi: {tokenId, secret}, csi:
  {tokenId, secret}}`, either or both, write-only. The ids must be
  `capmox@pve!capi` and `csi@pve!csi` (the managed-services script's), the
  secrets UUIDs. They are sealed with the data key in `platformSecret`; without
  `PROXCLOUD_ENCRYPTION_DEFAULT_KEY` the route answers `409 no_encryption_key`.
  No route returns a secret: an installation shows `credentials.{capi,csi}.tokenId`.
  The other body is `{enroll: "PC-ENROLL:…"}`, alone: the line the root command
  printed, base64 of `1:<installation id>:<unix time>:<capi secret>:<csi secret>`
  (a token the command kept has an empty field). A line that is not one, or of
  another installation, is a `400 invalid_enrollment`; a line whose time is more
  than five minutes ahead of the portal's clock is a `400
  enrollment_clock_ahead`; a line stored before, or
  printed no later than the one stored last, is a `409 enrollment_replayed` and
  changes nothing. No answer quotes the line.
- `POST /installations/{id}/preflight`: read-only, not audited; `{ok, checks}`
  with rows `flags`, `scheduler` (a `warn` that says what tenants miss and the command that turns the job scheduler
  on; absent while it runs), `spec`, `network`, `bridge`, `isolation`, `tenant-network`, `tenant-path`, `csi-vmid`, `credentials`, `machines`, `capacity`,
  `privileges:capi`, `privileges:csi`, `privileges:kubemint`, `mint-scope`, `pool-fence`,
  `token-scope`, `iso-store-visibility`, `boot-iso-grant`, `boot-iso`, `placement`, `boot-store`,
  `pve-api-rule` and `images`. `boot-store` fails when Proxmox has no storage of
  the boot store's name or it takes no disk images, and warns when the store also
  holds disks of VMs that are not the platform's (listed with the CSI token, once both tokens are stored; of the
  pool `pc-templates` only the templates are left out, a VM of that pool that is no template is listed).
  `pve-api-rule` never fails: `ok` before the first install and when the cluster
  firewall holds the installation's rule (comment `proxcloud:pve-api:<id>`), `warn`
  when an installed platform's rule is missing. It reads the rule, not the traffic: the rule's sources are the
  platform machines' addresses as the node sees them, and the message says that a site whose router translates
  them needs a rule for the translated address. A row's `status` is `ok`, `fail` or `warn`; `ok` in the answer is
  false only when a row failed. `csi-vmid` never fails: it warns when a guest has VM id 9999
  (see the connection test's row of that id). `tenant-network` never fails. It reads the provider's default
  zone in Proxmox: `warn` when that zone has DHCP, when it does not exist yet and is
  configured as a `simple` zone without `"dhcp": false` (ProxCloud then creates it with DHCP), or when the zones
  could not be read; a tenant Kubernetes cluster create on a network of such a zone is
  refused with a `400`, except on a Kubernetes provider with shared workers
  (`cluster.shared`), which uses no tenant network. It is `ok` for a zone without
  DHCP, and absent when the provider names no zone.
  `capacity` is absent while the CAPI token is not stored. It fails before the first install when a node's memory,
  less the configured memory of the other VMs and containers on it as the CAPI token lists them, is smaller than what
  the settings place there with one spare machine for the control plane. It is `warn` when that fits and one spare
  machine per group and node on top does not, on an installed platform, and when the nodes could not be read.
  `bridge` fails when a node the settings use has no bridge or vnet of that name, or (mode `routed`) when a vnet of
  that name exists and is not the installation's; on an installed platform the same finding is `warn`.
  `tenant-path` never fails either and is absent in the same case. It reads the layout (the default zone's kind,
  the nodes it translates tenant traffic on, the nodes of the platform machines, and whether such a node holds the
  platform gateway on the platform bridge) and is `warn` when a node that translates also hosts a platform machine
  without routing the platform network, or when Proxmox could not be read; `ok` for mode `routed`. It opens no
  connection, so `ok` does not prove that tenants reach the platform; `…/tenant-path-probe` below does, and for a VLAN default
  zone, where that probe is refused, the row names the check from a tenant's server ([managed-services.md](managed-services.md#the-path-from-tenants-to-the-platform)).
  `machines` never fails either, and is absent while the installation has no VM. It reads, with the CAPI
  token, the VMs of the pools `proxcloud-seed` and `proxcloud-system` that carry the installation's tag:
  `warn` with one line per VM that is stopped (start it in Proxmox, `qm start <vmid>`) or that has no
  `onboot` (the next apply sets it, or `qm set <vmid> --onboot 1`) or that the token could not read (the
  other VMs are still listed), or when the pools could not be read.
  `isolation` fails, naming the CIDRs missing from `blockedCidrs` (a save of
  the installation adds them), when
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
- `POST /installations/{id}/tenant-path-probe` `{node?}`: starts the tenant path test and answers `202` with the
  probe in state `running`; `GET` on the same path (`admin:service:read`) answers the installation's last probe
  (`404 no_probe` before the first). The one check that writes to Proxmox, so it is audited like every installation
  write and never part of the preflight: a throwaway network in the provider's default tenant zone (a free /28 from
  the end of `cidrPool`, with SNAT), a throwaway VM `pc-probe-<id>` cloned from an Ubuntu or Debian image
  on `node` (default: the control plane's node; it must host a platform machine) into `pc-templates`, with a fixed
  address in every zone. The image is resolved in the provider's catalog as a tenant's server create resolves one, and
  only one of this portal's own is taken: a template that Template updates imported for this provider (a finished import whose
  template at that node and VM id still has the import's name and source digest), or one that
  `config.templates` names with its VM id; the clone's disk goes to the store of the provider's default storage
  profile on that node. TCP connections
  through the guest agent, then both removed, on every path, with an 8 minute limit. A probe still `running` 12 minutes
  after its start was cut off by an API restart: `GET` answers it as `error` with what it may have left, and a new
  start is taken. Refused with `409` and a code
  before anything is created: `not_installed`, `no_tenant_zone`, `vlan_zone` (the default zone is a VLAN zone: no node translates there),
  `invalid_node`, `probe_running`, `run_active`,
  `no_os_template` (no such image on that node; the message names the published templates that are not this portal's), `no_target` (from the ProxCloud host nothing answers on port 6443 or 5432 of
  the first 32 addresses of the load balancer range; when a 6443 answers, 5432 listeners are not targets). The probe: `{id, state, node, image, template, storage, requestedBy,
  startedAt, finishedAt, step, vmid, vnet, source, targets[], message, left[], notes[]}`. `image` is the catalog
  image the VM is cloned from, `template` its template on `node`, `storage` the store the clone's disk goes to
  (absent: the template's own); the `202` answer carries them already. `notes[]` is what Proxmox may keep by itself
  and the cleanup cannot remove: the DHCP mapping of the test VM's NIC in a zone with a DHCP server. `state` is `running`, `passed`
  (every target opened or was refused, and the platform's own API address did not answer), `failed` (a target the
  host opens gave no answer from the tenant network, or the platform's API address answered) or `error` (the test
  could not be done: nothing is known). `targets[]` are `{address, port, expect: open|closed, result:
  open|refused|none}`; `left[]` names what the cleanup could not remove, with the command. One probe tests one node
  ([managed-services.md](managed-services.md#the-path-from-tenants-to-the-platform)). Not verified on a lab.
- `GET /installations/{id}/plan[?target=<sha256>]`: what an apply does
  (`installed`, `target`, `newer`, `changes`, `blocking`, `upgrade`). `target` is
  the BOM an apply without `targetBom` runs with: this release's for a new
  installation (`changes` then lists every component), the installed one for an
  installed platform (`changes` is empty), and the target of an upgrade that has
  started. `newer` is this release's own BOM (`release`, `sha256`) when the
  installed platform is on another one. An installed BOM this release does not
  carry (its own and those of earlier releases from 0.8.0 on; not one of a build
  of `main`) blocks with `bom_unsupported`.
  `upgrade` is the plan of the move to this release's BOM; it is absent when the
  platform is on it. `?target=` names the BOM to plan for and takes the same
  values as `targetBom` below (`400 invalid_target` otherwise).
  While an upgrade toward the BOM of an earlier release is in progress,
  `upgrade` is that move and `replacement`, of the same shape, is the move to
  this release's BOM in its place (with its own `allowed`, `blocking` and
  `confirm`); it is absent otherwise.
  `upgrade` is `{from, to, allowed, inProgress, confirm, changes, blocking}`:
  - `changes[]` lists every difference of the two BOMs:
    `{cluster, name, from, to, class, stage, wave, impact, blocking}`. `class` is
    `chart` (the run syncs a new chart version in `stage`, at `wave`),
    `record only` (nothing of the platform is synced for it) or `refused`.
    `impact` says what the change restarts or rolls; a refused change has
    `blocking`, its reason, instead.
  - `allowed` is false when one change is refused or the target is a `-dev` BOM,
    with each reason in `blocking`. This release moves a platform only when the
    BOMs differ in `release`, `tenantCharts` and the versions of the product's
    own charts (`proxcloud-platform` as `capi`, `capi-tenant` and `platform`;
    `proxmox-kamaji-cluster` as `pc-platform`), each new version listing the old
    one in `upgradeFrom`. Everything else is refused: `kubernetes.version`,
    `kubernetes.nodeImage`, `seed.image`, `seed.k3s`, `images.kubeVip`,
    `images.clusterctl`, every `capi.*` provider, every third-party chart, and a
    component that is added, removed or changed in anything but its chart
    version.
  - `confirm` is the phrase a run that starts the upgrade must carry, present
    when it is allowed and not started. `inProgress` is true once a run toward
    `to` has started and none has succeeded (`status.targetBom`).

- `GET /installations/{id}?live=1`: the installation with `live`
  (`phase`, `components[] {name, cluster, sync, health}`, `error`, `code`),
  read from the seed's ArgoCD for this request and never stored. `live.phase`
  is `degraded` when the stored phase is `ready` and the seed does not answer,
  lists no Application, or one is not Synced and Healthy. When the seed could
  not be read, `live.code` is `seed_api_unreachable` and `live.error` says
  whether its API does not answer or refuses the stored credential, with the
  seed's address, VMID and node. The stored phase stays `ready`: a seed that
  went down shows only here.
- `POST /installations/{id}/runs` `{kind: apply|remove, confirm, targetBom}`: `202 {runId}`. A run
  and its durable job (`platform.run/v1`, key `platform-run-<runId>`) are
  written in one transaction; `409 run_active` while another run of the
  installation is not finished, `409 bom_unsupported` when the installed BOM is
  not one this release carries. An apply on an installed platform runs with
  the installed BOM (the run's `bom`), also when this release's own is another.
  `targetBom` makes an apply an upgrade. It is the sha256 of this release's own
  BOM (`400 invalid_target` for any other value, and on a remove), and `confirm`
  must then be the plan's `upgrade.confirm`, `upgrade <installation name> to
  <release>` (`400 invalid_confirmation`). A move the plan does not allow is
  `409 upgrade_refused` with `details.reasons`, the plan's `upgrade.blocking`.
  The run's `bom` is the target and `fromBom` the installed BOM; the installation
  records the target as `status.targetBom`. From then until a run succeeds,
  every apply (Retry) continues to that target without `targetBom` or `confirm`,
  and the installed BOM as `targetBom` is `409 upgrade_refused`: nothing goes
  back. `status.bom` names the BOM the platform left until the last stage of a
  run has passed; `status.components` say per component what has moved. A later
  release may replace a target that was not reached: its own BOM as `targetBom`
  is accepted when both the installed BOM and the unfinished target may move to
  it. Without the runner (it starts only when
  `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true` too) the route answers
  `409 platform_disabled`; the reads below still work. A plain remove is
  `{"kind":"remove"}`. `confirm` is optional, belongs to a remove only and asks
  for more: when it is sent it must be `remove <installation name>`
  (`400 invalid_confirmation` otherwise, also on an apply without `targetBom`). With it, a remove whose seed VM is gone deletes the VMs of pool
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
  An upgrade run has `fromBom`; its codes `unexpected_rollout` (stage 7) and
  `tenants_unhealthy` (stage 9) are in
  [managed-services.md](managed-services.md#upgrade-between-boms).
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
  carries `Cache-Control: no-store`. The entry's `operation` is
  `seed-kubeconfig`.
- `GET /installations/{id}/platform-kubeconfig` (`admin:service:manage`,
  although a GET): `{kubeconfig, expiresAt}`, a kubeconfig of the platform
  cluster with a one-hour token of its service account
  `kube-system/proxcloud-break-glass`. The route reads the cluster's admin
  kubeconfig and CA from the seed, writes that account, a ClusterRole and a
  Role in `argocd` with their bindings on the cluster (all named
  `proxcloud-break-glass`; written again by each call), and asks the cluster
  for the token. The admin kubeconfig is never returned. The token reads the
  cluster without its Secrets and may forward a port in `argocd` and read
  `argocd-initial-admin-secret` there; it changes nothing
  ([managed-services.md](managed-services.md#8-credentials-and-the-seed-backup)
  has the rules and what they do not limit). Nothing stores it. Audit entry,
  order, `no-store` and `503` as for `seed-kubeconfig`, with `operation:
  platform-kubeconfig`. `409 no_seed` without a ready seed; `409 no_cluster`
  when the seed holds no admin kubeconfig or CA of the cluster yet; `502
  seed_api_unreachable` when the seed does not answer; `502
  workload_api_unreachable` when the cluster refuses a write or the token, or
  grants the token for longer than the hour.
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
- `POST /installations/{id}/kubernetes-version` (v0.9.0; no body): offers the Kubernetes version of the installed BOM
  on the installation's Kubernetes provider. It reads the templates of the Proxmox cluster, publishes nothing and
  overwrites nothing. When a node image of that version is published on every node the platform cluster uses, the
  answer is `{version, line, offered, serviceId, versions}`: `line` is the value written to
  `config.cluster.versions[version]` (the newest published copy per node, `pve1=9212,pve2=9213`), `offered` is false
  when the provider already names that version (its line is kept, whatever it is), `versions` is the provider's map
  afterwards. `409 node_image_unpublished` with `details.version` and `details.missingNodes` when a node lacks a
  published copy; `409 no_provider` before the first install; `409 bom_unsupported` when this release does not carry
  the installed BOM; `502 pve_unreadable` when Proxmox did not list its templates. An answered call records the
  version in `status.kubernetesOffered`. Stage 10 of an apply does the same for a version not recorded there, so a
  version removed from the provider afterwards is offered again by this route only. A change is in the audit log with the provider's versions before and after. Not verified on a lab
  ([managed-services.md](managed-services.md#what-a-tenant-can-use-after-the-install)).

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
