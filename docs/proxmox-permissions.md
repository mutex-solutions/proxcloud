# Proxmox VE permissions

ProxCloud needs no `root@pam` access on the cluster. The [installer](install.md) creates everything on this page (`installer/playbooks/10-pve-identity.yml`). This page defines seven least-privilege roles, the users and API tokens that hold them, and the ACL paths they are granted on. The commands are for **PVE 9**. PVE 9 removed `VM.Monitor`; the guest-agent reads ProxCloud uses need `VM.GuestAgent.Audit` instead.

| Role | Held by | Used for |
|---|---|---|
| `ProxCloudPlatform` | `proxcloud@pve!platform` | the portal API (the token configured on the Proxmox provider) |
| `ProxCloudCAPI` | `capmox@pve!capi` | Cluster API provider for Proxmox (CAPMOX), services cluster, or the seed and workload cluster of [managed services](#managed-services) |
| `ProxCloudCSI` | `csi@pve!csi`, `csi@pve!gateway` | `proxmox-csi-plugin` in the services (or managed-services workload) cluster; the CSI gateway of the portal API (split mode) |
| `ProxCloudImages` | `images@pve!packer` | the image pipeline ([images/README.md](../images/README.md)) |
| `ProxCloudSandboxGuard` | `proxcloud@pve!platform`, on pool `proxcloud-sandbox` only | the sync job's VM firewall on the sandbox workers (`sandbox.enabled`) |
| `ProxCloudKubeMint` | `proxcloud@pve!platform` (`mode: split`, or the [managed-services script](#managed-services)) | minting and revoking the per-cluster tokens of managed Kubernetes ([below](#managed-kubernetes)) |
| `ProxCloudSandboxEntry` | `proxcloud@pve!platform`, on pool `proxcloud-sandbox-entry` only | the sync job keeping the sandbox entry appliance: firewall and HAProxy config through the guest agent (stage 56) |

Every token is created with **privilege separation** (`--privsep 1`). A privilege-separated token's rights are the **intersection** of the user's ACLs and the token's ACLs, so each ACL below is granted to both the user and the token.

The admin **Test connection** button on the Proxmox provider reads the token's effective permissions (`GET /access/permissions`) and lists each `ProxCloudPlatform` privilege the token lacks, plus a warning for a `root@pam` token. Missing privileges are only reported as warnings. A cluster without GPUs can, for example, leave out `/mapping/pci`. The list it checks is `pve.PlatformPrivileges` in `internal/cloud/pve/access.go`. Keep it in sync with this page. The setup checks of the same test (`?checks=all`, [api.md](api.md)) read more with the same token: the pool `pc-templates`, and for an EVPN zone each node's package list (`GET /nodes/{node}/apt/versions`) to find `frr-pythontools`. That last read is expected to work with `PVEAuditor` on `/nodes` but is not lab-verified; when it is refused the check says so and stays a warning.

## Inventory visibility repair

The NETWORK reconciler does **not** treat PVE's `200 []` as proof of deletion.
The SDN API silently omits vnets without `SDN.Audit` or `SDN.Allocate` on
`/sdn/zones/<zone>/<vnet>`. Before changing cached inventory, ProxCloud checks
self effective privileges with the **configured provider token** and re-reads
missing vnets/subnets by ID. It checks the namespace, zone and exact vnet paths;
a narrower ACL can shadow a working parent grant. No new read-wide or root
permission is granted automatically. It never queries another user's privileges.

PVE9's actual missing-object response is **HTTP400** with
`errors.vnet = "could not find vnet '<id>'"` (or `errors.subnet`). PVE returns
that same response for an object moved into a zone hidden from the token. Before
accepting absence, ProxCloud therefore also proves **complete SDN visibility**:
self `Sys.Audit` on `/access` makes `GET /access/acl` unfiltered; propagated
`SDN.Audit` or `SDN.Allocate` must hold on `/sdn/zones` and every SDN ACL shadow
boundary. ACL snapshots and self privileges are rechecked around by-id reads.
No role/group translator, root ticket, alternate token or filtered ACL list is
accepted as evidence. A token lacking this existing audit/read evidence retains
rows with a warning; portal-requested explicit deletion remains available. An
operator can repair intended read privileges deliberately, never via auto-grant.

If a sync warns **provider visibility unverified / cached inventory preserved**:

1. Keep the cached NETWORK rows and share exports. Do not manually delete them to
   silence the warning; they preserve ownership, billing history and NAS access.
2. In the provider's Test connection, check effective SDN rights. With the portal
   token (not a root ticket), read `GET /access/permissions?path=/sdn/zones`, then
   `/sdn/zones/<cached-zone>` and `/sdn/zones/<cached-zone>/<cached-vnet>`. Omit
   `userid`. Each path must include `SDN.Audit` or `SDN.Allocate`.
3. Repair the intended user **and** privilege-separated token ACLs using the
   existing roles. In split mode, the mint-only `/sdn/zones` ACL must include
   `ProxCloudPlatform` too; inspect more-specific `NoAccess`/non-propagating grants.
   Do not add root or a broad new privilege just to bypass this check.
4. Read `GET /cluster/sdn/vnets/<vnet>` with that same token. The alias must name
   the expected project. A forbidden/unknown answer is not disappearance, even
   if an empty list or a masked404 accompanies it. Legacy cached rows without a
   zone fail safely: restore visibility so a complete normal list can fill it;
   if it never returns, an operator must repair the missing identity metadata.
5. Run project sync again. Live networks keep their original rows; the next
   unchanged pass should report zero changes. A truly vanished vnet with proven
   privileges and by-id absence archives normally, and its CIDRs leave exports.

A **multiple provider owners** error is a separate per-project NAS conflict.
Keep only one attached provider for a given NAS API backend + parent dataset,
using a single canonical hostname even when the NAS has aliases/proxies. Different
`snatSources`, credentials or a disabled duplicate are not separate owners.
Choose the canonical provider, preserve/reassociate existing share rows as needed,
then detach the redundant binding (do not delete datasets to repair configuration).
Alternatively give intentional separate providers disjoint parent datasets. Retry
sync/refresh afterward. Unrelated projects with a single owner are not blocked.

## Pools

| Pool | Contents |
|---|---|
| `pc-<projectId>` | one per project, created by the portal (the prefix is the provider's `poolPrefix`) |
| `pc-templates` | OS and k8s node templates |
| `proxcloud-system` | management VMs, the services cluster and its storage VMs. The platform token gets `NoAccess` here. Its name must **not** start with `poolPrefix`. |
| `proxcloud-seed` | [managed services](#managed-services) from the console only: the seed VM (k3s with Cluster API, CAPMOX and ArgoCD). Only `capmox@pve!capi` holds a role there; the portal token and every `csi@pve` token get `NoAccess`. That fences the seed VM, not its disks on the boot store ([what the seed pool does not fence](#what-the-seed-pool-does-not-fence)). Its name must **not** start with `poolPrefix`. |
| `proxcloud-sandbox` | `sandbox.enabled` only: the sandbox cluster's workers ([managed-sandbox.md](managed-sandbox.md)). No project owns it, so nothing in it is synced or billed to a tenant. Its name must **not** start with `poolPrefix`. |
| `proxcloud-sandbox-entry` | `sandbox.entry.public.bridge` set: the sandbox entry appliance (HAProxy, stage 56). The platform token holds only `ProxCloudSandboxEntry` there. Its name must **not** start with `poolPrefix`. |

## ProxCloudPlatform

| Privilege | Why |
|---|---|
| `Sys.Audit` | cluster status, node PCI scan, datacenter firewall options (the preflight) |
| `Sys.Modify` | cluster firewall security groups and tenant isolation rules (`/cluster/firewall/groups` and `/rules` check `Sys.Modify` on `/`) |
| `Sys.Console` | HA resources and rules of protected servers (`/cluster/ha/*` checks it on `/`) |
| `Pool.Allocate`, `Pool.Audit` | create, read, fill and delete project pools |
| `VM.Allocate`, `VM.Clone` | clone templates into a project pool, delete VMs, convert snapshots to images |
| `VM.Audit` | read VM config, status, snapshots and firewall |
| `VM.Config.CDROM`, `.CPU`, `.Cloudinit`, `.Disk`, `.HWType`, `.Memory`, `.Network`, `.Options` | flavor, cloud-init, disks, GPU `hostpciN`, NICs, VM firewall/ipset, tags |
| `VM.Backup` | server backups: vzdump, restore and deleting an archive all check it on the VM, through its pool. The same check hides an archive of a VM that is gone from this token, which is why backups go with their server. On the backup store, `Datastore.AllocateSpace` (below) is the other half |
| `VM.Console` | VNC and serial console |
| `VM.GuestAgent.Audit` | `network-get-interfaces`, which the sync uses to read VM IPs |
| `VM.GuestAgent.Unrestricted` | `set-user-password`: the server's Set password action. PVE checks nothing narrower for it. Also `exec` and `file-write` on a load balancer's own VM, which is how its config gets there. |
| `VM.Migrate` | migrate and rebalance |
| `VM.PowerMgmt` | start, stop, reboot, suspend and resume |
| `VM.Replicate` | replication jobs of protected servers |
| `VM.Snapshot`, `VM.Snapshot.Rollback` | server snapshots |
| `SDN.Allocate`, `SDN.Audit` | vnets, subnets, per-project EVPN zones, SDN apply |
| `SDN.Use` | attach NICs to vnets and public bridges |
| `Datastore.AllocateSpace`, `Datastore.Audit` | VM disks and volumes, storage listing |
| `Datastore.AllocateTemplate` | upload a server's seed CD and customer images. Seed cleanup requires a separate complete global read view and owned artifact journal; absent either, the CD remains with a durable operator warning. Once proven unreferenced after its grace, a denied delete can overwrite it with an empty CD. Physical deletion needs `Datastore.Allocate` on the seed store alone, next to the existing roles on that path (a child ACL replaces parent roles). Never grant it cluster-wide just for cleanup. See [server-seed-safety.md](server-seed-safety.md). |
| `Mapping.Audit`, `Mapping.Use` | GPU PCI mappings |

An existing site adds the two newest with `pveum role modify ProxCloudPlatform --append 1 --privs "VM.GuestAgent.Unrestricted Datastore.AllocateTemplate"`. Without them Test connection warns, and the two actions answer 409 and name the missing privilege. `VM.Backup` is added the same way; without it the backup actions answer 409 naming it, before a running server is stopped. For image import grant `pveum acl modify /storage/<importStorage> --roles PVEDatastoreAdmin --users proxcloud@pve --tokens 'proxcloud@pve!platform' --propagate 0`.

ACLs:

| Path | Role | Propagate | Note |
|---|---|---|---|
| `/` | `ProxCloudPlatform` | **0** | only `Sys.*` matter here. Without propagation they do not reach `/nodes` (no node network or power rights; `/nodes` gets read-only `PVEAuditor` below). |
| `/nodes` | `PVEAuditor` | 1 | read-only `Sys.Audit` per node: node firewall options (the nftables preflight), node networks (the management-bridge guard), the task logs of all users (the metering task poller; without it only the token's own tasks are visible) |
| `/pool` | `ProxCloudPlatform` | 1 | covers every `pc-*` pool created later, and `pc-templates` |
| `/pool/proxcloud-system` | `NoAccess` | 1 | the more specific path wins, and `NoAccess` cancels all other roles |
| `/sdn` | `ProxCloudPlatform` | 1 | zones, vnets, controllers, apply |
| `/storage` | `ProxCloudPlatform` | 1 | or one ACL per storage the provider's profiles use |
| `/mapping/pci` | `ProxCloudPlatform` | 1 | GPU clusters only |
| `/storage/<importStorage>` | `PVEDatastoreAdmin` | 0 | image import only: PVE wants `Datastore.Allocate` to delete any file that is not a backup, and the import store is the one place the portal needs that. Without it an imported image works, the uploaded file stays on the store, and Test connection warns. The installer adds it with a NAS. It shadows the `/storage` grant on that store, so with minted cluster tokens add `ProxCloudKubeMint` on the same path too |

There is no ACL on `/vms`. VMs get their rights only through their pool, so a VM outside `pc-*`/`pc-templates` is invisible to the portal. The sandbox pool (`sandbox.enabled`) and the sandbox entry pool are the other exceptions to `/pool`: the workers run tenant code and the entry appliance faces the internet, so the portal holds only what its sync needs there.

| Path | Role | Propagate | Note |
|---|---|---|---|
| `/pool/proxcloud-sandbox` | `ProxCloudSandboxGuard` (`Pool.Audit VM.Audit VM.Config.Network`) | 1 | user and token rows. It replaces, on that pool and its VMs, the `ProxCloudPlatform` and `ProxCloudKubeMint` the token inherits from `/pool`: the sync reads the workers and sets their NIC flag, firewall options, rules and IP filters, and can do nothing else to them (no guest agent, console, disk, power, snapshot or ACL rights). The cluster security group and IP sets it references are written with `Sys.Modify` on `/`. Stage 10 reads the token's effective rights there and stops unless they are exactly these three ([managed-sandbox.md](managed-sandbox.md#24-permissions)) |
| `/pool/proxcloud-sandbox` | `NoAccess` | 1 | user `csi@pve`: its tokens hold `ProxCloudCSI` on `/pool` for tenant clusters, and nothing of theirs is in this pool. A user row: the CSI tokens are privilege-separated, so capping the user caps them |
| `/pool/proxcloud-sandbox` | `NoAccess` | 1 | token `capmox@pve!capi`: the services cluster's CAPMOX holds `ProxCloudCAPI` on `/pool` and would inherit it on the workers, but the sandbox cluster runs through `capmox@pve!sandbox`, so it never manages this pool. A token row, not a user row: `capmox@pve` owns the sandbox token, which the user `NoAccess` would cap too |
| `/pool/proxcloud-sandbox-entry` | `ProxCloudSandboxEntry` (`Pool.Audit VM.Audit VM.Config.Network VM.GuestAgent.Audit VM.GuestAgent.Unrestricted`) | 1 | user and token rows, with an entry appliance (`sandbox.entry.public.bridge`). It replaces the platform role from `/pool` on the VM that faces the internet: the sync sets its firewall and pushes HAProxy's config through the agent, and cannot power, resize, console into or clone it |
| `/pool/proxcloud-sandbox-entry` | `NoAccess` | 1 | user `csi@pve` and token `capmox@pve!capi`, as on the sandbox pool |

Lab, PVE 9.2: before, the platform token held all 35 privileges on a worker (`/vms/<id>`) and `capmox@pve!capi` the 19 of `ProxCloudCAPI`; after, the three above for the platform token and nothing for the CAPI and CSI tokens, on the pool and on each worker VM (stage 10 checks both), with project pools and their VMs, the sandbox vnet, the stores and `/` unchanged.

## ProxCloudCAPI

This follows the CAPMOX "RBAC with least privileges" setup, written as one explicit role (CAPMOX's own example grants built-in `PVEVMAdmin`).

Privileges: `Sys.Audit Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.GuestAgent.Audit VM.Migrate VM.PowerMgmt Datastore.AllocateSpace Datastore.Audit SDN.Use`

| Path | Role | Propagate |
|---|---|---|
| `/` | `ProxCloudCAPI` | 0 |
| `/nodes` | `ProxCloudCAPI` | 1 |
| `/pool/proxcloud-system` | `ProxCloudCAPI` | 1 |
| `/pool/proxcloud-seed` | `ProxCloudCAPI` | 1 (managed services only) |
| `/pool/pc-templates` | `PVETemplateUser` (built-in) | 1 |
| `/sdn/zones/<zone>/<vnet>` | `ProxCloudCAPI` | 0 |
| `/storage/<disk storage>` | `ProxCloudCAPI` | 0 |
| `/storage/<cloud-init ISO storage>` | `PVEDatastoreAdmin` (built-in) | 0 |

This is the minimum for the services cluster alone. What stage 10 grants in split mode (`split_acls`) is wider for the
user, because the per-cluster tokens minted under `capmox@pve` need it (below): `ProxCloudCAPI` on all of `/pool`,
`/storage` and `/sdn/zones` (propagated). The `capi` token holds `/pool/proxcloud-system` only, never `/pool`, and the
user holds its own row there too, so a `/pool` grant written for `capmox@pve` later does not reach the services VMs
([managed services](#managed-services)). The token keeps `/sdn/zones` and the two `/storage` rows. Installs before this
rule gave the `capi` token `/pool` itself; stage 10 only adds rows, so that one stays until it is removed by hand
([below](#an-existing-site)). With `sandbox.enabled` the `capi` token gets `NoAccess` on the sandbox pools.

The ISO storage holds each new node's kubeadm join token until it has joined, and CAPMOX uploads to the first
`iso` store the token sees, in no fixed order: make it node-local (`local`, the installer's default), and give the
token no other store with `iso` content ([managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox),
"Bootstrap ISOs"). Installs before this rule gave `capmox@pve!capi` `ProxCloudCAPI` on all of `/storage` and the ISO
role on the NAS template store; stage 10 (run by `make install` and `make upgrade`; `make identity-check` previews it)
now takes both off the token after granting the two rows above, checks with PVE that the token sees no other `iso` store, and keeps
`/storage` on the user, which the tenant clusters' tokens need.

### Managed Kubernetes

Each tenant cluster runs under a token the portal mints at create and revokes after the delete cascade (`MintClusterTokens`, `RevokeClusterTokens` in `internal/cloud/pve/access.go`):

| Token | Grants | Reaches |
|---|---|---|
| `capmox@pve!<clusterId>` | `ProxCloudCAPI` on the project pool, the boot store, the vnet, `/sdn/zones/localnetwork` and `/nodes`; `PVETemplateUser` on `pc-templates`; `PVEDatastoreAdmin` on the node-local ISO store (provider `isoStorage`, default `local`), which needs the same role for `capmox@pve` itself | CAPMOX on the services cluster |
| `csi@pve!gateway` | `ProxCloudCSI` on `/pool` and `/storage` (propagate), one token for every cluster. Created by the installer, not minted | the portal API only (the CSI gateway, below) |
| `csi@pve!<clusterId>` | No longer minted. A cluster made by an earlier release with `csiSharedStores` may still hold one (`ProxCloudCSI` on its pool and stores, **`Datastore.Allocate` on a store opens every disk on it, of any project**); moving the cluster onto the CSI gateway, or deleting it, revokes it ([managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox)) | the tenant cluster, where a cluster-admin can read it |
| `capmox@pve!sandbox` | the same grants as `capmox@pve!<clusterId>`, on pool `proxcloud-sandbox` and the sandbox vnet (`/sdn/zones/<zone>/<vnet>`), the services boot store and the node-local ISO store. Created by stage 10 with `sandbox.enabled`, token rows only (`sandbox_acls`), plus a `PVEDatastoreAdmin` row for `capmox@pve` when `sandbox.iso_storage` is not `services.iso_storage`; stage 10 then checks with PVE that it sees one `iso` store | CAPMOX on the services cluster, for the sandbox cluster |

`/sdn/zones/localnetwork` is there because PVE checks a clone against the NICs of the template, which sit on a node bridge (`Permission check failed (/sdn/zones/localnetwork/vmbr0, SDN.Use)`). The token also has it for every other node bridge, so it stays on the management side. It needs no `/`: go-proxmox's `GET /cluster/status` answers 403 and is ignored.

A token's rights are its user's intersected with its own, so `capmox@pve` holds the same role on `/pool`, `/storage` and `/sdn/zones`, `csi@pve` on `/pool` and `/storage`, and the portal token additionally needs the right to mint and grant. `make install` with `mode: split` sets this (stage 10, `split_acls`):

```sh
pveum role add ProxCloudKubeMint --privs "User.Modify Permissions.Modify"
grant /access/groups ProxCloudKubeMint 1 proxcloud@pve platform
grant /pool          ProxCloudKubeMint 1 proxcloud@pve platform
grant /storage       ProxCloudKubeMint 1 proxcloud@pve platform
grant /sdn/zones     ProxCloudKubeMint 1 proxcloud@pve platform
grant /sdn/zones     ProxCloudPlatform 1 proxcloud@pve platform
grant /nodes         ProxCloudKubeMint 0 proxcloud@pve platform
# capmox@pve itself, for the cluster tokens: their boot stores, and the ISO store (a create checks this one)
pveum acl modify /storage       --users capmox@pve --roles ProxCloudCAPI     --propagate 1
pveum acl modify /storage/local --users capmox@pve --roles PVEDatastoreAdmin --propagate 0
```

An ACL on a path replaces what the parent passes down for that user or token. The `/sdn/zones` mint grant on its own leaves the portal token without `SDN.*` under `/sdn/zones`, so every network create fails with `Permission check failed (/sdn/zones, SDN.Allocate)` and no NIC can attach, hence the second `/sdn/zones` line. `/pool` and `/storage` already hold `ProxCloudPlatform` directly. `/nodes/<node>` does not inherit the mint grant (propagate 0), by design. After any ACL change, read the result with `pveum user token permissions proxcloud@pve platform --path <path>`, not the ACL list; Test connection warns when the portal token lacks a privilege on `/sdn/zones`.

`Permissions.Modify` is a big right: a stolen portal token can grant itself anything on those paths. Without these grants a cluster create fails with PVE's `Permission check failed`, or the provider runs in `sharedWorkers` mode (docs/managed-k8s.md). This list ran on a real cluster (PVE 9.2) with no other grant: create, scale-out, delete, a refused create. Without it, `POST /access/users/{user}/token/{name}` answers 403 even for the token's own user, and `PUT /access/acl` wants `Permissions.Modify` on `/sdn/zones/...` and `/nodes`, and `Permissions.Modify` or `Datastore.Allocate` on `/storage/...`.

PVE stops honouring a deleted token's ACL entries at once, but leaves them in `user.cfg` until the next write of that file. Until then `pveum` prints `ignore invalid acl token`, which is harmless; token names are never reused.

## ProxCloudCSI

These are the privileges from the `proxmox-csi-plugin` install guide. That guide grants them on `/`; here they are scoped.

Privileges: `VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit`

| Token | Path | Role | Propagate | Note |
|---|---|---|---|---|
| `csi@pve!csi` | `/pool/proxcloud-system` | `ProxCloudCSI` | 1 | the services cluster's own disks (installs before [managed services](#managed-services) granted `/pool`) |
| `csi@pve!csi` | `/storage/<csi storage>` | `ProxCloudCSI` | 0 | `services.storage`, the `std` StorageClass (installs before granted `/storage`) |
| `csi@pve!gateway` | `/pool` | `ProxCloudCSI` | 1 | the workers of every tenant cluster sit in project pools under it |
| `csi@pve!gateway` | `/storage` | `ProxCloudCSI` | 1 | the stores of the clusters' StorageClasses are chosen after stage 10 |

The user `csi@pve` holds the same role on `/pool` and `/storage`, since a privilege-separated token gets the intersection, and its own row on `/pool/proxcloud-system`.

### The CSI gateway token

`csi@pve!gateway` is the only PVE credential tenant block volumes use (`internal/cloud/csigw`). Tenant clusters' proxmox-csi
controllers present a per-cluster credential to the gateway, never a PVE token, and the gateway sends on only the calls
it allows. Its rights are broad on purpose: `Datastore.Allocate` on a store reaches every disk there, and the gateway's
registry, not PVE, decides which disk and which VM a cluster may touch. Keep the token on the portal API alone.

The gateway does not read pools with this token. `ProxCloudCSI` has no `Pool.Audit`, and without it `/cluster/resources`
leaves out a VM's `pool` field (seen on PVE 9.2), so the gateway reads which VMs are a cluster's workers with the
portal's own token, which also never sees `proxcloud-system`.

The installer (split mode) creates the token in stage 10 and writes it to the compute provider in stage 60
(`config.csiGatewayTokenId`, `secret.csiGatewaySecret`). By hand:

```sh
pveum user token add csi@pve gateway --privsep 1
pveum acl modify /pool    --tokens 'csi@pve!gateway' --roles ProxCloudCSI --propagate 1
pveum acl modify /storage --tokens 'csi@pve!gateway' --roles ProxCloudCSI --propagate 1
pveum user token permissions csi@pve gateway --path /pool/<a project pool>
pveum user token permissions csi@pve gateway --path /storage/<a store>
```

Neither path holds another ACL for this token, so nothing is shadowed. The user's own `/pool` and `/storage` grants must
stay: removing them leaves the token with nothing.

## ProxCloudImages

Privileges: `Sys.Audit Sys.AccessNetwork Pool.Allocate Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.Console VM.GuestAgent.Audit VM.PowerMgmt Datastore.Allocate Datastore.AllocateSpace Datastore.AllocateTemplate Datastore.Audit SDN.Use`

| Path | Role | Propagate | Note |
|---|---|---|---|
| `/pool/pc-templates` | `ProxCloudImages` | 1 | the only VMs it can touch |
| `/nodes` | `ProxCloudImages` | 1 | `Sys.AccessNetwork` is what `download-url` asks for on a node; without it, that call wants `Sys.Modify` on `/` |
| `/storage` | `ProxCloudImages` | 1 | or one ACL per template and import store |
| `/sdn/zones/localnetwork` | `ProxCloudImages` | 1 | `SDN.Use` on the node bridges |

`VM.Console` is for image-builder: Packer types the Ubuntu installer's boot command over the VM's console.

There is no ACL on `/vms`, so the token holds `VM.Config.*` only on VMs already in the pool. `build.py` therefore creates a VM empty, with just `pool`, and configures it in a second call.

## Portal token script

For a portal that was not set up by the installer (the [Compose install](../deploy/compose/README.md), or a cluster moving off `root@pam`), this script creates what the portal token needs and nothing else: the `ProxCloudPlatform` role, the pools `pc-templates` and `proxcloud-system`, the user, its privilege-separated token and the ACLs of the table above. Paste it into a root shell on one node. It can be run again: the role's privileges are appended, never replaced, and a pool, user or token that exists is left alone. It ends with the token's effective permissions and the certificate fingerprints of the node, to compare with what the provider form shows.

The operator console's API generates it: `GET /api/v1/admin/setup/proxmox-script` (`admin:service:read`) answers it as text. `tokenId` (default `proxcloud@pve!platform`) names the token, `gpu=true` adds the `/mapping/pci` ACL, and `importStorage=<storage>` adds `PVEDatastoreAdmin` on that store for image import. A `tokenId` or `importStorage` with anything but letters, digits, dot, dash and underscore is refused with a 400. The script below is the answer to `?gpu=true`; a test keeps the two equal.

<!-- generated: GET /api/v1/admin/setup/proxmox-script?gpu=true -->
```sh
# ProxCloud: the Proxmox VE identity of the portal token.
# Paste into a root shell on one node (the cluster shares it). Safe to run again.
(
set -eu
PC_USER='proxcloud@pve'
PC_TOKEN_NAME='platform'
PC_TOKEN='proxcloud@pve!platform'

# role: created empty when missing, then the privileges are appended (a re-run adds new ones, removes none)
pveum role add 'ProxCloudPlatform' 2>/dev/null || true
pveum role modify 'ProxCloudPlatform' --append 1 --privs 'Sys.Audit Sys.Console Sys.Modify Pool.Allocate Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.Backup VM.Console VM.GuestAgent.Audit VM.GuestAgent.Unrestricted VM.Migrate VM.PowerMgmt VM.Replicate VM.Snapshot VM.Snapshot.Rollback SDN.Allocate SDN.Audit SDN.Use Datastore.AllocateSpace Datastore.AllocateTemplate Datastore.Audit Mapping.Audit Mapping.Use'

# pools: OS templates, and the management VMs the token must not reach
pveum pool add 'pc-templates' --comment 'ProxCloud templates' 2>/dev/null || true
pveum pool add 'proxcloud-system' --comment 'ProxCloud management (not tenant)' 2>/dev/null || true

# user and privilege-separated token. The secret is printed once: store it
pveum user add "$PC_USER" --comment 'ProxCloud portal' 2>/dev/null || true
pveum user token add "$PC_USER" "$PC_TOKEN_NAME" --privsep 1 || echo "The token $PC_TOKEN exists already: its secret was printed when it was created."

# ACLs, for the user and for the token: a privilege-separated token holds the intersection of the two
grant() { # path role propagate
  pveum acl modify "$1" --users "$PC_USER" --roles "$2" --propagate "$3"
  pveum acl modify "$1" --tokens "$PC_TOKEN" --roles "$2" --propagate "$3"
}
grant '/' 'ProxCloudPlatform' 0
grant '/nodes' 'PVEAuditor' 1
grant '/pool' 'ProxCloudPlatform' 1
grant '/pool/proxcloud-system' 'NoAccess' 1
grant '/sdn' 'ProxCloudPlatform' 1
grant '/storage' 'ProxCloudPlatform' 1
grant '/mapping/pci' 'ProxCloudPlatform' 1

# what the token really holds
pveum user token permissions "$PC_USER" "$PC_TOKEN_NAME"
# the certificate fingerprint to pin on the provider: pve-ssl.pem, or pveproxy-ssl.pem when the node has its own
pvenode cert info
)
```

The script does not turn on the datacenter firewall or the nftables firewall of the nodes, and adds no `import` content to a storage: those change a running cluster, and are done in the Proxmox UI ([quickstart.md](quickstart.md)). Without `managedServices` it creates none of the other identities on this page (CAPI, CSI, images).

## Managed services

Managed Kubernetes and databases from the operator console ([ADR 0009](adr/0009-managed-services-platform-lifecycle.md)) run on a platform of their own: a seed VM (k3s with Cluster API, CAPMOX and ArgoCD) in pool `proxcloud-seed`, and a workload cluster in `proxcloud-system`. The portal never holds root on Proxmox, so the operator creates two more tokens with the same script: `GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage=<store>&isoStorage=<store>` adds the block below to the portal token script. `bootStorage` (required) holds the boot disks of the seed and the workload VMs and the volumes of their `std` StorageClass; `isoStorage` (default `local`) is the node-local store of their bootstrap ISOs and the seed's CD. Both are refused unless they are plain storage ids, like `importStorage`. The block adds the roles `ProxCloudCAPI`, `ProxCloudCSI` and `ProxCloudKubeMint`, the pool `proxcloud-seed`, the users `capmox@pve` and `csi@pve` with the privilege-separated tokens `capi` and `csi`, the rows of the table below, and ends with what both tokens hold on both platform pools. The two secrets are printed once; when a token exists already the script says so, prints no secret and still writes the rows. The Managed services page shows this script in its step 2 and takes the two secrets; the durable job of the API then uses them ([managed-services.md](managed-services.md#3-proxmox-identities)).

<!-- generated: the lines GET /api/v1/admin/setup/proxmox-script?gpu=true&managedServices=true&bootStorage=local-lvm&isoStorage=local adds -->
```sh
# managed services: roles, the seed pool, the users and tokens of CAPMOX and proxmox-csi on the platform, and
# their ACLs; the portal token may then mint the tokens of tenant clusters. Both secrets are printed once: store them
pveum role add 'ProxCloudCAPI' 2>/dev/null || true
pveum role modify 'ProxCloudCAPI' --append 1 --privs 'Sys.Audit Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.GuestAgent.Audit VM.Migrate VM.PowerMgmt Datastore.AllocateSpace Datastore.Audit SDN.Use'
pveum role add 'ProxCloudCSI' 2>/dev/null || true
pveum role modify 'ProxCloudCSI' --append 1 --privs 'VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit'
pveum role add 'ProxCloudKubeMint' 2>/dev/null || true
pveum role modify 'ProxCloudKubeMint' --append 1 --privs 'User.Modify Permissions.Modify'
pveum pool add 'proxcloud-seed' --comment 'ProxCloud seed cluster (not tenant)' 2>/dev/null || true
pveum user add 'capmox@pve' --comment 'ProxCloud CAPI' 2>/dev/null || true
pveum user token add 'capmox@pve' 'capi' --privsep 1 || echo 'The token capmox@pve!capi exists already: its secret was printed when it was created.'
pveum user add 'csi@pve' --comment 'ProxCloud CSI' 2>/dev/null || true
pveum user token add 'csi@pve' 'csi' --privsep 1 || echo 'The token csi@pve!csi exists already: its secret was printed when it was created.'
pveum acl modify '/' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 0
pveum acl modify '/' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 0
pveum acl modify '/nodes' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/nodes' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/proxcloud-system' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/proxcloud-system' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/proxcloud-seed' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/proxcloud-seed' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/pc-templates' --users 'capmox@pve' --roles 'PVETemplateUser' --propagate 1
pveum acl modify '/pool/pc-templates' --tokens 'capmox@pve!capi' --roles 'PVETemplateUser' --propagate 1
pveum acl modify '/sdn/zones' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/sdn/zones' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/storage' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/storage/local-lvm' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 0
pveum acl modify '/storage/local' --users 'capmox@pve' --roles 'PVEDatastoreAdmin' --propagate 0
pveum acl modify '/storage/local' --tokens 'capmox@pve!capi' --roles 'PVEDatastoreAdmin' --propagate 0
pveum acl modify '/pool' --users 'csi@pve' --roles 'ProxCloudCSI' --propagate 1
pveum acl modify '/pool/proxcloud-system' --users 'csi@pve' --roles 'ProxCloudCSI' --propagate 1
pveum acl modify '/pool/proxcloud-system' --tokens 'csi@pve!csi' --roles 'ProxCloudCSI' --propagate 1
pveum acl modify '/pool/proxcloud-seed' --users 'csi@pve' --roles 'NoAccess' --propagate 1
pveum acl modify '/storage' --users 'csi@pve' --roles 'ProxCloudCSI' --propagate 1
pveum acl modify '/storage/local-lvm' --tokens 'csi@pve!csi' --roles 'ProxCloudCSI' --propagate 0
grant '/access/groups' 'ProxCloudKubeMint' 1
grant '/pool' 'ProxCloudKubeMint' 1
grant '/storage' 'ProxCloudKubeMint' 1
grant '/sdn/zones' 'ProxCloudKubeMint' 1
grant '/sdn/zones' 'ProxCloudPlatform' 1
grant '/nodes' 'ProxCloudKubeMint' 0
grant '/pool/proxcloud-seed' 'NoAccess' 1
# what the platform tokens hold on the platform pools: the CAPI token ProxCloudCAPI on both, the CSI token nothing on the
# seed VM (its disks on the boot store stay open to the CSI token: docs/proxmox-permissions.md, "What the seed pool does not fence")
pveum user token permissions 'capmox@pve' 'capi' --path '/pool/proxcloud-system'
pveum user token permissions 'capmox@pve' 'capi' --path '/pool/proxcloud-seed'
pveum user token permissions 'csi@pve' 'csi' --path '/pool/proxcloud-system'
pveum user token permissions 'csi@pve' 'csi' --path '/pool/proxcloud-seed'
```

The rows are `pve.ManagedACLs` (`internal/cloud/pve/access.go`). Installer stage 10 writes the same rows in split mode (`split_acls`), without the `proxcloud-seed` ones and with the two of the CSI gateway's token, which has no listener on Compose; a test keeps the two equal.

| Principal | Path | Role | Why |
|---|---|---|---|
| `capmox@pve!capi` (token) | `/pool/proxcloud-system`, `/pool/proxcloud-seed` | `ProxCloudCAPI` | Not `/pool`: the token never needs a tenant pool. |
| `capmox@pve` (user) | `/pool/proxcloud-system`, `/pool/proxcloud-seed` | `ProxCloudCAPI` | The same rights it inherits from `/pool`, as a more specific row: a `/pool` grant written for this user later does not reach the platform VMs. The user keeps `/pool` for the tenant clusters' tokens. |
| `csi@pve!csi` (token) | `/pool/proxcloud-system`; `/storage/<bootStorage>` | `ProxCloudCSI` | Not `/pool` and `/storage`. |
| `csi@pve` (user) | `/pool/proxcloud-system`; `/pool/proxcloud-seed` | `ProxCloudCSI`; `NoAccess` | The first as for `capmox@pve`; the second caps every `csi@pve` token off the seed VM, the CSI gateway's included, but not off its disks ([below](#what-the-seed-pool-does-not-fence)). |
| `proxcloud@pve!platform` (user and token) | `/pool/proxcloud-seed` | `NoAccess` | As on `proxcloud-system`. |
| `proxcloud@pve!platform` (user and token) | `/access/groups`, `/pool`, `/storage`, `/sdn/zones` (propagate), `/nodes` (no propagate) | `ProxCloudKubeMint` | Mint and grant the tenant clusters' tokens ([Managed Kubernetes](#managed-kubernetes)), with `ProxCloudPlatform` repeated on `/sdn/zones`. |
| `proxcloud@pve!platform` (user and token) | `/storage/<importStorage>` (no propagate), only with `importStorage` | `ProxCloudKubeMint` | The `PVEDatastoreAdmin` row there replaces what `/storage` passes down, and a tenant cluster's ISO or boot store may be that store (`local` on a site without a NAS): without it PVE refuses the cluster token's row there and the cluster create fails. Stage 10 writes the same row (`nas_mint_acls`). |

The other rows (`/`, `/nodes`, `/pool/pc-templates`, `/sdn/zones`, the two `/storage` rows of `capmox@pve!capi`, and `/pool` and `/storage` for both users) are those of [ProxCloudCAPI](#proxcloudcapi) and [ProxCloudCSI](#proxcloudcsi).

The portal checks a site against the same tables (`internal/cloud/pve/access.go`), each with a client on that token: `MissingPrivileges` with `CAPIPrivileges`, `CSIPrivileges` and `KubeMintPrivileges` (which checks the import store apart); `ISOVisibilityProblem`, which asks PVE per `iso` store whether the CAPI token sees it and wants exactly `isoStorage` (CAPMOX puts each bootstrap ISO on the first `iso` store its token sees, as stage 10 checks for the services cluster); and `CheckBootISOGrant` for `capmox@pve`. They are the rows `privileges:capi`, `privileges:csi`, `privileges:kubemint`, `iso-store-visibility` and `boot-iso-grant` of the page's Check step and of a run's first stage, next to `pool-fence`, which fails when the portal token holds any privilege on `/pool/proxcloud-system` or `/pool/proxcloud-seed`, or `csi@pve!csi` on `/pool/proxcloud-seed`, and `token-scope`, which fails when `capmox@pve!capi` or `csi@pve!csi` holds any privilege on `/pool`, `/storage` or `/vms` as a whole (no row of the tables above gives one; an older setup wrote some, the script removes no row, and stage 10 removes only the CAPI token's old rows on `/storage` and on the NAS template store, so the message gives the `pveum acl delete` command). A row of the two tokens on a single tenant pool or store is not compared with the tables ([managed-services.md](managed-services.md#step-3-check)).

### What the seed pool does not fence

PVE checks a disk against two places: the VM that owns it (`VM.Config.Disk` there, with `Datastore.Audit` on the
store) or the store alone. `Datastore.Allocate` on a store passes the check for every volume on it, whatever VM owns
it; it is how proxmox-csi attaches its `vm-9999-pvc-*` volumes to other VMs, and the reason the role carries it. The
`NoAccess` rows on `proxcloud-seed` close the first way for the portal and every `csi@pve` token. The second stays
open: the seed's disk is cloned onto `bootStorage`, where `csi@pve!csi` holds `ProxCloudCSI`, and so is every other
VM disk on that store, tenant ones included when tenants use it. Whoever holds that token (the workload's proxmox-csi
Secret, which the providers' service account can read, or anyone who takes over the workload) can attach the seed's
disk to a workload VM, where the token holds `VM.Config.Disk`, and read the seed's k3s datastore with its cluster-admin
token and the `capmox@pve!capi` secret; or delete the disk while the seed is stopped. The portal token holds
`Datastore.Allocate` on the boot store too when `importStorage` is that store: the platform API refuses an
installation whose import store is its boot store, comparing the two names in the settings only (a row on the boot
store made for another installation, for a provider's earlier `importStorage` or by hand is not read; [removing it](managed-services.md#when-the-providers-import-store-is-the-boot-store)). M1 accepts the rest
([ADR 0009](adr/0009-managed-services-platform-lifecycle.md#a2-seed-vm-from-a-native-template-in-its-own-pool), A2
"Limit", and A8). The fix is a boot store of the seed's own, and a store for its backups, where no `csi@pve` token
holds a role (refused when it is `bootStorage` or `importStorage`); M1 gives the platform one boot store, so that is a
change of its own. Lab check L1 measures it.

### An existing site

Stage 10 only adds rows. On a site installed before these rows, `make upgrade` adds the narrow rows and leaves the broad ones: `capmox@pve!capi` keeps `ProxCloudCAPI` on `/pool`, and `csi@pve!csi` keeps `ProxCloudCSI` on `/pool` and `/storage`. The new rows grant nothing the old ones did not, so nothing breaks and nothing narrows until an operator removes the old token rows, after stage 10 has run with this release (the last `delete` only when every StorageClass of the services cluster is on `services.storage`, as the installer makes it):

```sh
pveum acl delete /pool    --tokens 'capmox@pve!capi' --roles ProxCloudCAPI
pveum acl delete /pool    --tokens 'csi@pve!csi'     --roles ProxCloudCSI
pveum acl delete /storage --tokens 'csi@pve!csi'     --roles ProxCloudCSI
pveum user token permissions capmox@pve capi --path /pool/proxcloud-system
pveum user token permissions csi@pve csi --path /storage/<services.storage>
```

### The mint grant in M1

`ProxCloudKubeMint` gives the portal token `User.Modify` on `/access/groups` and `Permissions.Modify` on `/access/groups`, `/pool`, `/storage`, `/sdn/zones` (all propagated) and `/nodes` (that path only). With `Permissions.Modify` on a path, PVE lets the holder write any role there, with propagate, for any user or token, itself included. A stolen portal token can therefore:

- **on `/pool`:** mint a token for an existing principal and grant it a role on `/pool`, which reaches every pool where that principal has no row of its own. `NoAccess` keeps the portal token off both platform pools, and the user rows above keep `capmox@pve` and `csi@pve` to their roles there. Any other user of the site without a row on the platform pools is not covered. The owner accepted this for M1 on 2026-11-15 ([ADR 0009](adr/0009-managed-services-platform-lifecycle.md#a8-risks-accepted-for-the-first-milestone), A8), documented here and measured on the lab.
- **on `/storage`:** grant its own user and token `PVEDatastoreAdmin` on `/storage/<bootStorage>`. No pool row stands in the way, because PVE checks a disk against its store alone once the caller holds `Datastore.Allocate` there ([what the seed pool does not fence](#what-the-seed-pool-does-not-fence)). With `VM.Config.Disk` and `VM.GuestAgent.Unrestricted` on the tenant pools it then attaches any disk of that store, the seed's, a workload VM's or another tenant's, to a tenant VM and reads it through the guest agent: the seed's cluster-admin token and the `capmox@pve!capi` secret are on the seed's disk. A8 accepts it for M1 with the rest of the mint grant (a stolen portal token "can grant itself anything on those paths"), and M1 has no row that closes it.
- **on `/sdn/zones` and `/nodes`:** the same, for the zones and vnets and for every node.

The structural fix is to grant the mint role on a parent pool of the tenant pools (nested pools), which renames every tenant pool, and to narrow the `/storage` grant to the stores tenant clusters use, with the platform's disks on a store outside them; a site whose Kubernetes provider runs in `sharedWorkers` mode needs no mint grant at all.

Lab check L1 is to measure it on PVE 9.2. It uses three scratch VMs on one node and the boot store, in `proxcloud-seed`, `proxcloud-system` and a tenant pool, destroyed afterwards. The checks above are green; `pveum user token permissions` for the portal token is empty on both platform pools and `csi@pve csi` is empty on `proxcloud-seed` but holds `Datastore.Allocate` on `/storage/<bootStorage>`; `capmox@pve capi` sees one ISO store. The CSI token attaches the seed-pool VM's disk to the `proxcloud-system` one (`PUT /nodes/<node>/qemu/<vmid>/config` with `scsi9=<bootStorage>:vm-<seed-pool vmid>-disk-0`), which PVE is expected to allow. Then a drill with the portal token, reverted afterwards: grant `capmox@pve` `ProxCloudPlatform` on `/pool` and read its effective rights on `/pool/proxcloud-seed`, which must stay `ProxCloudCAPI`; grant its own user and token `PVEDatastoreAdmin` on `/storage/<bootStorage>`, read `pveum user token permissions proxcloud@pve platform --path /storage/<bootStorage>`, and attach the seed-pool VM's disk to the tenant one, which PVE is expected to allow; and try `POST /access/users/root@pam/token/x`, to learn whether PVE lets it mint for `root@pam`.

#### L1 results

> **Not yet measured on the lab.** L1 has not run. No row below is a result; each stays "not measured" until the drill has run and its output is recorded here.

| Step of the drill | Expected | Measured |
|---|---|---|
| `pveum user token permissions proxcloud@pve platform` on `/pool/proxcloud-system` and `/pool/proxcloud-seed` | empty | not measured |
| `pveum user token permissions csi@pve csi` on `/pool/proxcloud-seed` | empty | not measured |
| `csi@pve!csi` on `/storage/<bootStorage>` | holds `Datastore.Allocate` | not measured |
| ISO stores `capmox@pve!capi` sees | one, `isoStorage` | not measured |
| CSI token attaches the seed-pool VM's disk to the `proxcloud-system` VM | allowed (the open point of [what the seed pool does not fence](#what-the-seed-pool-does-not-fence)) | not measured |
| Portal token grants `capmox@pve` `ProxCloudPlatform` on `/pool`; effective rights of `capmox@pve` on `/pool/proxcloud-seed` | still `ProxCloudCAPI` | not measured |
| Portal token grants itself `PVEDatastoreAdmin` on `/storage/<bootStorage>`, then attaches the seed-pool VM's disk to the tenant VM | allowed (accepted in A8) | not measured |
| Portal token calls `POST /access/users/root@pam/token/x` | unknown: the drill is to find out | not measured |

## pveum commands

Run these on any node (cluster config is shared). Replace the `<…>` placeholders.

```sh
# roles
pveum role add ProxCloudPlatform --privs "Sys.Audit Sys.Console Sys.Modify Pool.Allocate Pool.Audit VM.Allocate VM.Audit VM.Backup VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.Console VM.GuestAgent.Audit VM.GuestAgent.Unrestricted VM.Migrate VM.PowerMgmt VM.Replicate VM.Snapshot VM.Snapshot.Rollback SDN.Allocate SDN.Audit SDN.Use Datastore.AllocateSpace Datastore.AllocateTemplate Datastore.Audit Mapping.Audit Mapping.Use"
pveum role add ProxCloudCAPI --privs "Sys.Audit Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.GuestAgent.Audit VM.Migrate VM.PowerMgmt Datastore.AllocateSpace Datastore.Audit SDN.Use"
pveum role add ProxCloudCSI --privs "VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit"
pveum role add ProxCloudKubeMint --privs "User.Modify Permissions.Modify"                   # mode: split
pveum role add ProxCloudSandboxGuard --privs "Pool.Audit VM.Audit VM.Config.Network"          # sandbox.enabled
pveum role add ProxCloudSandboxEntry --privs "Pool.Audit VM.Audit VM.Config.Network VM.GuestAgent.Audit VM.GuestAgent.Unrestricted"  # entry appliance
pveum role add ProxCloudImages --privs "Sys.Audit Sys.AccessNetwork Pool.Allocate Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.Console VM.GuestAgent.Audit VM.PowerMgmt Datastore.Allocate Datastore.AllocateSpace Datastore.AllocateTemplate Datastore.Audit SDN.Use"

# pools
pveum pool add pc-templates --comment "ProxCloud templates"
pveum pool add proxcloud-system --comment "ProxCloud management (not tenant)"

# users + privilege-separated tokens (the secret is printed once; store it)
pveum user add proxcloud@pve --comment "ProxCloud portal"
pveum user add capmox@pve --comment "ProxCloud CAPI"
pveum user add csi@pve --comment "ProxCloud CSI"
pveum user add images@pve --comment "ProxCloud image pipeline"
pveum user token add proxcloud@pve platform --privsep 1
pveum user token add capmox@pve capi --privsep 1
pveum user token add csi@pve csi --privsep 1
pveum user token add images@pve packer --privsep 1

# helper: grant to user AND token (privsep = intersection)
grant() { # path role propagate user token
  pveum acl modify "$1" --users "$4" --roles "$2" --propagate "$3"
  pveum acl modify "$1" --tokens "$4!$5" --roles "$2" --propagate "$3"
}

# platform
grant /                       ProxCloudPlatform 0 proxcloud@pve platform
grant /nodes                  PVEAuditor        1 proxcloud@pve platform
grant /pool                   ProxCloudPlatform 1 proxcloud@pve platform
grant /pool/proxcloud-system  NoAccess          1 proxcloud@pve platform
grant /sdn                    ProxCloudPlatform 1 proxcloud@pve platform
grant /storage                ProxCloudPlatform 1 proxcloud@pve platform
grant /mapping/pci            ProxCloudPlatform 1 proxcloud@pve platform

# CAPI (services cluster)
grant /                        ProxCloudCAPI     0 capmox@pve capi
grant /nodes                   ProxCloudCAPI     1 capmox@pve capi
grant /pool/proxcloud-system   ProxCloudCAPI     1 capmox@pve capi
grant /pool/pc-templates       PVETemplateUser   1 capmox@pve capi
grant /sdn/zones/<zone>/<vnet> ProxCloudCAPI     0 capmox@pve capi
grant /storage/<disk-storage>  ProxCloudCAPI     0 capmox@pve capi
grant /storage/local           PVEDatastoreAdmin 0 capmox@pve capi   # node-local ISO store: join tokens

# CSI
grant /pool/proxcloud-system  ProxCloudCSI 1 csi@pve csi
grant /storage/<csi-storage>  ProxCloudCSI 0 csi@pve csi
# CSI gateway (managed Kubernetes block volumes)
pveum user token add csi@pve gateway --privsep 1
grant /pool                   ProxCloudCSI 1 csi@pve gateway
grant /storage                ProxCloudCSI 1 csi@pve gateway

# images
grant /pool/pc-templates        ProxCloudImages 1 images@pve packer
grant /nodes                    ProxCloudImages 1 images@pve packer
grant /storage                  ProxCloudImages 1 images@pve packer
grant /sdn/zones/localnetwork   ProxCloudImages 1 images@pve packer

# check what a token really has
pveum user token permissions proxcloud@pve platform
```

Templates must be members of `pc-templates`, or neither the platform nor CAPMOX can clone them: `pveum pool modify pc-templates --vms <id>[,<id>…]`.

## Moving a cluster off `root@pam`

1. Run the commands above and move the existing templates into `pc-templates`.
2. On the Proxmox provider, set the token ID to `proxcloud@pve!platform` and paste the new secret.
3. Click **Test connection**. It must pass with no "Token lacks …" warnings, apart from any you leave out on purpose (`/mapping/pci` without GPUs).
4. Create a project network and a server, open its console, snapshot it, and delete it.
5. Revoke the old token: `pveum user token remove root@pam <tokenid>`.

## Known ceilings

- `Sys.Console` on `/` is what PVE asks for to write HA resources and rules. The ACL does not propagate, so it does not reach node consoles under `/nodes`. A cluster that does not sell protected servers can leave it out, along with `VM.Replicate`; `ENABLE_PROTECTION` then answers with the 403.
- An existing install needs `pveum role modify ProxCloudPlatform --append 1 --privs "Sys.Console VM.Replicate"` before protected servers work.
- `Sys.Modify` on `/` is needed for cluster security groups and isolation rules. The same privilege also allows editing datacenter options and the datacenter firewall. Proxmox has no narrower privilege for firewall groups.
- `SDN.Use` on `/sdn` also covers the node bridges, the management bridge included. The portal refuses the management bridge as a public network (test connection and attach). For a tighter setup, grant `SDN.Use` only on `/sdn/zones/<zone>` and `/sdn/zones/localnetwork/<public bridge>`.

## Verified on a PVE 9.2 lab

- A `pc-*` pool created after the `/pool` ACL inherits it, and VMs inherit their pool's rights.
- `NoAccess` on `/pool/proxcloud-system` overrides the propagated `/pool` grant: the token gets `Permission check failed` on a VM in that pool and does not see it.
- `GET /access/permissions?path=/pool` called with a privsep token returns the inherited user∩token set. A correctly set up token gets no warnings from **Test connection**.
- `GET /nodes/{node}/firewall/options` reports `nftables` as 0/1 and is readable with `PVEAuditor` on `/nodes`.
- VNC console through `vncproxy` + `vncwebsocket` works with a token (`RFB` banner through the portal's proxy). `termproxy` mints a ticket with a token too.
- The whole lifecycle ran on this token alone: SDN zones, vnets and apply, cluster firewall rules and groups, VM clone, config, power, resize and delete, disks across storages, pools.

Not run yet, because the lab has no GPU and no services cluster:

- Hot-adding a mapped GPU (`hostpciN: mapping=…`) with `Mapping.Use` and `VM.Config.HWType`, without root.
- CAPMOX and `proxmox-csi-plugin` with the scoped paths above instead of `/`.
