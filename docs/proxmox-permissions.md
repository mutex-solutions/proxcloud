# Proxmox VE permissions

ProxCloud needs no `root@pam` access on the cluster. The [installer](install.md) creates everything on this page (`installer/playbooks/10-pve-identity.yml`). This page defines eight least-privilege roles, the users and API tokens that hold them, and the ACL paths they are granted on. The commands are for **PVE 9**. PVE 9 removed `VM.Monitor`; the guest-agent reads ProxCloud uses need `VM.GuestAgent.Audit` instead.

| Role | Held by | Used for |
|---|---|---|
| `ProxCloudPlatform` | `proxcloud@pve!platform` | the portal API (the token configured on the Proxmox provider) |
| `ProxCloudCAPI` | `capmox@pve!capi` | Cluster API provider for Proxmox (CAPMOX), services cluster, or the seed and workload cluster of [managed services](#managed-services) |
| `ProxCloudCSI` | `csi@pve!csi`, `csi@pve!gateway` | `proxmox-csi-plugin` in the services (or managed-services workload) cluster; the CSI gateway of the portal API (split mode) |
| `ProxCloudImages` | `images@pve!packer` | the image pipeline ([images/README.md](../images/README.md)) |
| `ProxCloudSandboxGuard` | `proxcloud@pve!platform`, on pool `proxcloud-sandbox` only | the sync job's VM firewall on the sandbox workers (`sandbox.enabled`) |
| `ProxCloudKubeMint` (`Permissions.Modify`) | `proxcloud@pve!platform` (`mode: split`, or the [managed-services script](#managed-services)) | granting the per-cluster tokens of managed Kubernetes their roles ([below](#managed-kubernetes)) |
| `ProxCloudKubeToken` (`User.Modify`) | `proxcloud@pve!platform`, on `/access/groups/proxcloud-kube` only (same sites) | creating and deleting those tokens, for the two users in the group `proxcloud-kube` (`capmox@pve`, `csi@pve`) and no other user ([the mint grant](#the-mint-grant-in-m1)) |
| `ProxCloudSandboxEntry` | `proxcloud@pve!platform`, on pool `proxcloud-sandbox-entry` only | the sync job keeping the sandbox entry appliance: firewall and HAProxy config through the guest agent (stage 56) |

Every token is created with **privilege separation** (`--privsep 1`). A privilege-separated token's rights are the **intersection** of the user's ACLs and the token's ACLs, so each ACL below is granted to both the user and the token.

The admin **Test connection** button on the Proxmox provider reads the token's effective permissions (`GET /access/permissions`) and lists each `ProxCloudPlatform` privilege the token lacks, plus a warning for a `root@pam` token, and one (`mint-scope`) for a token that holds `User.Modify` or `Permissions.Modify` on `/`, `/access` or `/access/groups`, or `Permissions.Modify` on `/pool`, where the product needs neither ([the mint grant](#the-mint-grant-in-m1)). Missing privileges are only reported as warnings. A cluster without GPUs can, for example, leave out `/mapping/pci`. The list it checks is `pve.PlatformPrivileges` in `internal/cloud/pve/access.go`. Keep it in sync with this page. The setup checks of the same test (`?checks=all`, [api.md](api.md)) read more with the same token: the pool `pc-templates`, and for an EVPN zone each node's package list (`GET /nodes/{node}/apt/versions`) to find `frr-pythontools`. That last read is expected to work with `PVEAuditor` on `/nodes` but is not lab-verified; when it is refused the check says so and stays a warning.

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

**SERVER, VOLUME and IMAGE rows need no such visibility.** The grants on this page give the portal token no ACL on
`/vms` or `/access` and `NoAccess` on the fenced pools, so it can never show that it sees every guest. A cached row
whose VM is no longer in its project pool is retired when `GET /cluster/nextid?vmid=<id>` says the VMID is free:
Proxmox answers that from the cluster's whole guest list for any authenticated user, so no grant is needed and a VM
the token cannot see still counts. A VMID that a VM of another project took (the token reads it, under another
SMBIOS UUID) counts as free too. Before v0.8.0 the sync asked for propagated `VM.Audit` on `/vms`, `/pool` and
every ACL path under them and for `Sys.Audit` on `/access`, which these grants never satisfy: no row was ever
retired and the warning below came at every sync (lab finding F60). When the warning names a SERVER or VOLUME row
now, a guest still holds that row's VMID and the token does not see it in the project pool; the text names the path
(`/pool/<pool>` when the token lost `VM.Audit` on the project pool, else `/vms/<id>`).
[vm-inventory-safety.md](vm-inventory-safety.md) has the rule and the repair. Do not add a `/vms` ACL or remove a
fence row for it.

If a sync warns **provider visibility unverified / cached inventory preserved** for a NETWORK row:

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
| `proxcloud-sbx` | [managed services](#managed-services) with `spec.sandbox` only: the sandbox machines of the managed platform, a node group of the platform cluster or the workers of its own cluster `pc-sandbox`. Created and deleted by the portal token; no script names it ([sandboxes on the managed platform](#sandboxes-on-the-managed-platform)). Its name must **not** start with `poolPrefix`. |
| `proxcloud-sandbox` | `sandbox.enabled` only: the sandbox cluster's workers ([managed-sandbox.md](managed-sandbox.md)). No project owns it, so nothing in it is synced or billed to a tenant. Its name must **not** start with `poolPrefix`. |
| `proxcloud-sandbox-entry` | `sandbox.entry.public.bridge` set: the sandbox entry appliance (HAProxy, stage 56). The platform token holds only `ProxCloudSandboxEntry` there. Its name must **not** start with `poolPrefix`. |

## ProxCloudPlatform

| Privilege | Why |
|---|---|
| `Sys.Audit` | cluster status, node PCI scan, datacenter firewall options (the preflight) |
| `Sys.Modify` | cluster firewall security groups and tenant isolation rules (`/cluster/firewall/groups` and `/rules` check `Sys.Modify` on `/`); with managed services also the one rule that lets an installation's seed and node range reach the Proxmox API (comment `proxcloud:pve-api:<installation id>`, written by the install and every apply, deleted by Remove). No privilege was added for it |
| `Sys.Console` | HA resources and rules of protected servers (`/cluster/ha/*` checks it on `/`) |
| `Pool.Allocate`, `Pool.Audit` | create, read, fill and delete project pools |
| `VM.Allocate`, `VM.Clone` | clone templates into a project pool, delete VMs, convert snapshots to images |
| `VM.Audit` | read VM config, status, snapshots and firewall |
| `VM.Config.CDROM`, `.CPU`, `.Cloudinit`, `.Disk`, `.HWType`, `.Memory`, `.Network`, `.Options` | flavor, cloud-init, disks, GPU `hostpciN`, NICs, VM firewall/ipset, tags |
| `VM.Backup` | server backups: vzdump, restore and deleting an archive all check it on the VM, through its pool. The same check hides an archive of a VM that is gone from this token, which is why backups go with their server. On the backup store, `Datastore.AllocateSpace` (below) is the other half |
| `VM.Console` | VNC and serial console |
| `VM.GuestAgent.Audit` | `network-get-interfaces`, which the sync uses to read VM IPs |
| `VM.GuestAgent.Unrestricted` | `set-user-password`: the server's Set password action. PVE checks nothing narrower for it. Also `exec` and `file-write` on a load balancer's own VM, which is how its config gets there, and `file-read` of two cloud-init files (`/run/cloud-init/result.json`, `/var/log/cloud-init-output.log`) of a managed Kubernetes worker that did not join, for the operator's Machines view only ([managed-k8s.md](managed-k8s.md#a-worker-that-does-not-join)). PVE 9 takes `VM.GuestAgent.FileRead` or this privilege for `file-read` (read in `PVE/API2/Qemu/Agent.pm` as remembered, not re-read for this change; not tried on a lab): nothing was added to the role for it. |
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
| `/pool/proxcloud-sandbox` | `ProxCloudSandboxGuard` (`Pool.Audit VM.Audit VM.Config.Network`) | 1 | user and token rows. It replaces, on that pool and its VMs, the `ProxCloudPlatform` the token inherits from `/pool`: the sync reads the workers and sets their NIC flag, firewall options, rules and IP filters, and can do nothing else to them (no guest agent, console, disk, power, snapshot or ACL rights). The cluster security group and IP sets it references are written with `Sys.Modify` on `/`. Stage 10 reads the token's effective rights there and stops unless they are exactly these three ([managed-sandbox.md](managed-sandbox.md#24-permissions)) |
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
| `/sdn/zones/localnetwork/<bridge>/<tag>` | `SDN.Use` (in `ProxCloudCAPI`) | only for a NIC with a VLAN tag on a node bridge |
| `/storage/<disk storage>` | `ProxCloudCAPI` | 0 |
| `/storage/<cloud-init ISO storage>` | `PVEDatastoreAdmin` (built-in) | 0 |

A NIC with a VLAN tag (managed services with `spec.network.vlan`) is checked by Proxmox on the tag's own path
(`PVE::GuestHelpers`, read in the code of pve-manager 9.2, not tried). The managed-services script grants the `capi`
token `ProxCloudCAPI` on `/sdn/zones` with propagate, which reaches that path and the vnet of a routed platform
network (`/sdn/zones/pcplat/<vnet>`); the Check row `privileges:capi` reads the exact path, because a row of the
token's own below `/sdn/zones` would replace what is passed down.

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
| `csi@pve!<clusterId>` | No longer minted. A cluster made by an earlier release with `csiSharedStores` may still hold one (`ProxCloudCSI` on its pool and stores, **`Datastore.Allocate` on a store opens every disk on it, of any project**); moving the cluster onto the CSI gateway, or deleting it, revokes it: the portal lists the tokens of `csi@pve` first and sends the delete only when the cluster's id is among them ([managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox)) | the tenant cluster, where a cluster-admin can read it |
| `capmox@pve!sandbox` | the same grants as `capmox@pve!<clusterId>`, on pool `proxcloud-sandbox` and the sandbox vnet (`/sdn/zones/<zone>/<vnet>`), the services boot store and the node-local ISO store. Created by stage 10 with `sandbox.enabled`, token rows only (`sandbox_acls`), plus a `PVEDatastoreAdmin` row for `capmox@pve` when `sandbox.iso_storage` is not `services.iso_storage`; stage 10 then checks with PVE that it sees one `iso` store | CAPMOX on the services cluster, for the sandbox cluster |

`/sdn/zones/localnetwork` is there because PVE checks a clone against the NICs of the template, which sit on a node bridge (`Permission check failed (/sdn/zones/localnetwork/vmbr0, SDN.Use)`). The token also has it for every other node bridge, so it stays on the management side. It needs no `/`: go-proxmox's `GET /cluster/status` answers 403 and is ignored.

**The `403` on `GET /cluster/status` in the Proxmox access log is expected.** The caller is CAPMOX with the cluster's token, not the portal and not the CSI. CAPMOX v0.9.2 asks go-proxmox v0.8.2 for a cluster handle before it looks a VM up, checks that a VM id is taken, or deletes a VM (`pkg/proxmox/goproxmox/api_client.go`: `FindVMResource`, `CheckID`, `DeleteVM`), and `Client.Cluster` (`cluster.go`) reads `/cluster/status` first to fill in the cluster's name and quorum, which CAPMOX never uses. The library's own comment: "requires (/, Sys.Audit), do not error out if no access to still get the cluster". The calls CAPMOX does use on that handle, `/cluster/resources` and `/cluster/nextid`, need nothing on `/` and answer with what the token may see. So the line appears a few times per machine made or deleted (12 times in the life of a two-worker cluster on the lab) and nothing degrades. `Sys.Audit` on `/` is **not** granted to make it go away: it would let every tenant cluster's token read the Proxmox cluster's membership and node addresses, and whatever else Proxmox gives for `Sys.Audit` on `/`. The same lab run showed `403` twice on a worker's `qemu/<vmid>/status/current`, at a clone start and at the delete; the machines came up and were removed, and that read was not traced further.

A token's rights are its user's intersected with its own, so `capmox@pve` holds the same role on `/pool`, `/storage` and `/sdn/zones`, `csi@pve` on `/pool` and `/storage`, and the portal token additionally needs the right to mint and grant. `make install` with `mode: split` sets this (stage 10, `split_acls`):

```sh
pveum role add ProxCloudKubeMint  --privs "Permissions.Modify"
pveum role add ProxCloudKubeToken --privs "User.Modify"
# the only users the portal may create and delete tokens for
pveum group add proxcloud-kube --comment "ProxCloud: users the portal mints cluster tokens for"
pveum user modify capmox@pve --groups proxcloud-kube --append 1
pveum user modify csi@pve    --groups proxcloud-kube --append 1
grant /access/groups/proxcloud-kube ProxCloudKubeToken 0 proxcloud@pve platform
grant /storage       ProxCloudKubeMint 1 proxcloud@pve platform
grant /sdn/zones     ProxCloudKubeMint 1 proxcloud@pve platform
grant /sdn/zones     ProxCloudPlatform 1 proxcloud@pve platform
grant /nodes         ProxCloudKubeMint 0 proxcloud@pve platform
# capmox@pve itself, for the cluster tokens: their boot stores, and the ISO store (a create checks this one)
pveum acl modify /storage       --users capmox@pve --roles ProxCloudCAPI     --propagate 1
pveum acl modify /storage/local --users capmox@pve --roles PVEDatastoreAdmin --propagate 0
```

An ACL on a path replaces what the parent passes down for that user or token. The `/sdn/zones` mint grant on its own leaves the portal token without `SDN.*` under `/sdn/zones`, so every network create fails with `Permission check failed (/sdn/zones, SDN.Allocate)` and no NIC can attach, hence the second `/sdn/zones` line. `/storage` already holds `ProxCloudPlatform` directly. `/pool` gets no mint grant: on `/pool/<pool>` PVE accepts `Pool.Allocate` for a role whose privileges the caller holds there itself, with propagate, and `ProxCloudPlatform` on `/pool` holds those of `ProxCloudCAPI` and `PVETemplateUser`, the two roles the portal writes on a pool ([the mint grant](#the-mint-grant-in-m1)). `/nodes/<node>` does not inherit the mint grant (propagate 0), by design. After any ACL change, read the result with `pveum user token permissions proxcloud@pve platform --path <path>`, not the ACL list; Test connection warns when the portal token lacks a privilege on `/sdn/zones`.

`Permissions.Modify` is a big right: a stolen portal token can grant itself anything on those paths. `User.Modify` is granted on the group's path and never on `/access/groups`: there PVE takes it for every user of the cluster, `root@pam` included ([the mint grant](#the-mint-grant-in-m1)). Without these grants a cluster create is refused, or the provider runs in `sharedWorkers` mode (docs/managed-k8s.md): the tenant reads `409 Could not mint the cluster's Proxmox tokens: the operator must fix a permission of the portal on Proxmox first…`, and PVE's own `Permission check failed (<path>, <privileges>)` goes to the operator, in the API log and in the audit event of the refused create ([a row of the portal's own on a store or a zone](#a-row-of-the-portals-own-on-a-store-or-a-zone)). This list ran on a real cluster (PVE 9.2) with no other grant: create, scale-out, delete, a refused create. That run predates the group: its `User.Modify` sat on `/access/groups`, and the grant on `/access/groups/proxcloud-kube` has not run on a cluster yet. It also held `ProxCloudKubeMint` on `/pool`; a create without that row has not run on a cluster yet either. Without `User.Modify`, `POST /access/users/{user}/token/{name}` answers 403 even for the token's own user, and `PUT /access/acl` wants `Permissions.Modify` on `/sdn/zones/...` and `/nodes`, and `Permissions.Modify` or `Datastore.Allocate` on `/storage/...`; on `/pool/<pool>` it wants `Permissions.Modify`, or `Pool.Allocate` and every privilege of the role written.

#### A row of the portal's own on a store or a zone

An ACL row on a path replaces, for that user or token, everything its parents pass down. The mint role sits on `/storage` and `/sdn/zones` with propagate, so it reaches every store and zone **except** one where `proxcloud@pve` or its token holds a row of its own with another role. On such a path PVE refuses the cluster token's row (`403 Permission check failed (/storage/<store>, Permissions.Modify|Datastore.Allocate)`) and every cluster create that boots from that store fails. This happened on a production site on v0.8.0 (2026-11-17): it had older rows `ProxCloudPlatform` on `/storage/nvme-u2` and two other stores, written before the managed-services script existed; every tenant cluster create answered `502`, and the Check was green because it read `/storage` alone.

Since v0.8.1:

- **The script and stage 10 repeat the mint role next to each such row.** They read the ACL list as root and, for every store (`/storage/<store>`), zone (`/sdn/zones/<zone>`) and vnet (`/sdn/zones/<zone>/<vnet>`) where the portal's user or its token holds a row with a role other than `ProxCloudKubeMint`, write `ProxCloudKubeMint` there **for that one alone**: for the user where the user holds the row, for the token where the token does. Never for the other of the two: a mint row for a principal that has no row there would itself replace what that principal inherits (its `ProxCloudPlatform` from `/storage`), and the portal could then no longer allocate a disk on that store. The new row propagates when any of that principal's rows on the path does, and not otherwise, for the same reason one level down. A row on a bridge of `localnetwork` (`/sdn/zones/localnetwork/<bridge>`) or below a vnet gets none: a mint writes on `/sdn/zones/localnetwork` itself and on a vnet, never there, and the role is `Permissions.Modify`. Running either again changes nothing more: stage 10 writes nothing, and the script runs the same `pveum acl modify` lines, which leave the same rows. A row that reaches the portal's user through a **group** is not found; the Check still fails on the path, and the row to add is then `pveum acl modify <path> --users <portal user> --roles ProxCloudKubeMint --propagate 0` next to a user row with any role of its own.
- **The Check reads the paths a mint writes** (`privileges:kubemint`, `pve.KubeMintPrivileges`): `/pool/pc-templates`, `/nodes`, `/sdn/zones/localnetwork`, every zone of the provider (`/sdn/zones/<zone>`, which stands in for its vnets), and `/storage/<store>` for the provider's ISO store, its import store and the store, on every node, of every storage profile that can hold a boot disk, next to the parents it read before. A failed line names the path and says to run the script again; a row that names a single store or zone also says, once, why the parent's grant does not reach it and which lines the script does not fix (a row held through a group, and [the provider's import store when the settings name another](managed-services.md#when-the-providers-import-store-is-the-boot-store)). **A failed row stops a run**: after the upgrade to v0.8.1, on a site with such rows, an install or apply run of the managed services stops at its first stage (`preflight_failed`; a remove run has no such stage) until the script has run again; before v0.8.1 the same site passed the Check and failed every tenant cluster create instead.
- **A refused create says so** (the `409` above) and leaves an audit event with outcome `FAILURE`; the operator's audit page shows PVE's refusal under it.

**What each added row lets a stolen portal token do.** `Permissions.Modify` on that one store or zone: write any role there, `Administrator` and `PVEDatastoreAdmin` included, for itself or any other user or token, which on a store opens every disk on it, of any tenant and of the platform ([the mint grant](#the-mint-grant-in-m1), the bullet on `/storage`). That is exactly what the row on `/storage` or `/sdn/zones` already gives on every store and zone that has no row of its own, and what the script was always meant to give on this one: a site without such rows gets no new row and no new right. It is still a widening against the site as it stood: before the row, the stolen token could not grant itself anything on that store. It also covers stores no tenant cluster boots from (a backup store with a row of the portal's, for example): the script runs on a node and does not know the provider's storage profiles. To keep the mint role off such a store, delete the row after the script (`pveum acl delete /storage/<store> --users|--tokens '<id>' --roles ProxCloudKubeMint`); the Check does not read a store that is in no boot profile and is neither the ISO nor the import store, and a script run writes the row again. Removing the site's own row from the store instead (so that it inherits from `/storage` again) gives the same rights as the added row, with one row fewer.

None of this has run on a cluster. The role resolution is tested against a model of PVE's (`TestMintWithAnOwnRowOnTheBootStore` in `internal/cloud/pve`, `test_a_row_on_a_single_store_or_zone_gets_the_mint_role` in `installer/tests`), which reads `PVE::AccessControl::roles` the way this section describes it; that a row without propagate on a zone leaves its vnets inheriting from `/sdn/zones` is from the PVE source, not from a lab.

**When the token is revoked.** Not by the delete: CAPMOX needs the token to delete the workers. The delete queues a durable job (`kamaji.release/v1`), which the API runs with or without the job scheduler. It revokes the token at its first look after the cascade has finished: no Application, TenantControlPlane, MachineDeployment or Machine of the cluster on the management cluster, and no VM named `<clusterId>-…` in the project's pool. Its first look comes within seconds of the delete, the next ones 15 seconds apart, growing to 5 minutes, for 24 hours. While the management cluster or Proxmox does not answer, or a worker may still exist, the token stays. The provider sync's sweep is the backstop and does the same for a cluster whose marker is older than 30 minutes; without the scheduler it runs only when an operator starts a provider sync ([managed-k8s.md](managed-k8s.md#deletion-and-orphan-finalization)). The job's timing is not verified on a lab. The token's secret is only in a Secret that the cascade deletes.

The revoke removes the token's own ACL rows first, then the token (`DeleteAPIToken`): one `PUT /access/acl` with `delete=1` per row whose principal is exactly that token, on the paths the mint wrote them on, so with the privileges the portal token already mints with (`Permissions.Modify` there) and no new one. The rows are read with `GET /access/acl`, which shows the portal token the rows on paths it may modify. PVE stops honouring a deleted token's ACL entries at once, but leaves them in `user.cfg` until the next write of that file, no longer lists them, and until then prints `user config - ignore invalid acl token '<token>'` at every `pveum`, `qm` and `pvesh` call, once per row: up to v0.10.1-rc.2 the release deleted the token alone, and the rows of the last tenant cluster deleted stayed (seen on the lab, six lines). They are harmless (token names are never reused) and go at the next write of `user.cfg`, which any `pveum` change makes. A row that cannot be read or removed does not hold the revoke: the token is deleted all the same. The removal of the rows is tested against a model of PVE that keeps a deleted token's rows (`TestRevokeRemovesTheTokensOwnRows`), not on a cluster.

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

Managed Kubernetes and databases from the operator console ([ADR 0009](adr/0009-managed-services-platform-lifecycle.md)) run on a platform of their own: a seed VM (k3s with Cluster API, CAPMOX and ArgoCD) in pool `proxcloud-seed`, and a workload cluster in `proxcloud-system`. The portal never holds root on Proxmox, so the operator creates two more tokens with the same script: `GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage=<store>&isoStorage=<store>` adds the block below to the portal token script. `bootStorage` (required) holds the boot disks of the seed and the workload VMs and the volumes of their `std` StorageClass; `isoStorage` (default `local`) is the node-local store of their bootstrap ISOs and the seed's CD. Both are refused unless they are plain storage ids, like `importStorage`. The block adds the roles `ProxCloudCAPI`, `ProxCloudCSI`, `ProxCloudKubeMint` and `ProxCloudKubeToken`, the pool `proxcloud-seed`, the users `capmox@pve` and `csi@pve` with the privilege-separated tokens `capi` and `csi`, the group `proxcloud-kube` with those two users in it, the rows of the table below, the lines that take an earlier script's mint grant off `/access/groups` and `/pool` ([an existing site](#an-existing-site)), and ends with what both tokens hold on both platform pools. The two secrets are printed once; when a token exists already the script says so, prints no secret and still writes the rows. The Managed services page shows this script in its step 2 and takes the two secrets; the durable job of the API then uses them ([managed-services.md](managed-services.md#3-proxmox-identities)).

<!-- generated: the lines GET /api/v1/admin/setup/proxmox-script?gpu=true&managedServices=true&bootStorage=local-lvm&isoStorage=local adds -->
```sh
# managed services: roles, the seed pool, the users and tokens of CAPMOX and proxmox-csi on the platform, and
# their ACLs; the portal token may then mint the tokens of tenant clusters. Both secrets are printed once: store them
pveum role add 'ProxCloudCAPI' 2>/dev/null || true
pveum role modify 'ProxCloudCAPI' --append 1 --privs 'Sys.Audit Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.GuestAgent.Audit VM.Migrate VM.PowerMgmt Datastore.AllocateSpace Datastore.Audit SDN.Use'
pveum role add 'ProxCloudCSI' 2>/dev/null || true
pveum role modify 'ProxCloudCSI' --append 1 --privs 'VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit'
pveum role add 'ProxCloudKubeMint' 2>/dev/null || true
pveum role modify 'ProxCloudKubeMint' --append 1 --privs 'Permissions.Modify'
pveum role add 'ProxCloudKubeToken' 2>/dev/null || true
pveum role modify 'ProxCloudKubeToken' --append 1 --privs 'User.Modify'
pveum pool add 'proxcloud-seed' --comment 'ProxCloud seed cluster (not tenant)' 2>/dev/null || true
pveum user add 'capmox@pve' --comment 'ProxCloud CAPI' 2>/dev/null || true
pveum user token add 'capmox@pve' 'capi' --privsep 1 || echo 'The token capmox@pve!capi exists already: its secret was printed when it was created.'
pveum user add 'csi@pve' --comment 'ProxCloud CSI' 2>/dev/null || true
pveum user token add 'csi@pve' 'csi' --privsep 1 || echo 'The token csi@pve!csi exists already: its secret was printed when it was created.'
# the only users the portal token may create and delete tokens for. Its grant sits on this group, never on
# /access/groups, which reaches every user of the cluster (root@pam too)
pveum group add 'proxcloud-kube' --comment 'ProxCloud: users the portal mints cluster tokens for' 2>/dev/null || true
pveum user modify 'capmox@pve' --groups 'proxcloud-kube' --append 1
pveum user modify 'csi@pve' --groups 'proxcloud-kube' --append 1
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
grant '/access/groups/proxcloud-kube' 'ProxCloudKubeToken' 0
grant '/storage' 'ProxCloudKubeMint' 1
grant '/sdn/zones' 'ProxCloudKubeMint' 1
grant '/sdn/zones' 'ProxCloudPlatform' 1
grant '/nodes' 'ProxCloudKubeMint' 0
grant '/pool/proxcloud-seed' 'NoAccess' 1
# a row of this user or token on a single store, zone or vnet replaces what /storage and /sdn/zones pass down there, the
# mint role too, and a tenant cluster on that store is then refused. The mint role is written next to each such row,
# for the one that holds the row (a row for the other would replace what that one inherits), propagated as the row is
PC_SHADOW=$(pveum acl list --output-format json | perl -MJSON::PP -e 'my ($u, $t) = @ARGV; my %p; for (@{ decode_json(join "", <STDIN>) }) { next if $_->{roleid} eq "ProxCloudKubeMint"; my $f = $_->{type} eq "user" && $_->{ugid} eq $u ? "--users" : $_->{type} eq "token" && $_->{ugid} eq $t ? "--tokens" : next; next unless $_->{path} =~ m{^/storage/[^/]+$|^/sdn/zones/[^/]+$} || ($_->{path} =~ m{^/sdn/zones/([^/]+)/[^/]+$} && $1 ne "localnetwork"); $p{"$f $_->{ugid}"}{$_->{path}} ||= $_->{propagate} ? 1 : 0 } for my $k (sort keys %p) { print "$k $p{$k}{$_} $_\n" for sort keys %{ $p{$k} } }' "$PC_USER" "$PC_TOKEN")
printf '%s\n' "$PC_SHADOW" | while read -r PC_FLAG PC_ID PC_PROP PC_PATH; do
  [ -z "$PC_PATH" ] || pveum acl modify "$PC_PATH" "$PC_FLAG" "$PC_ID" --roles 'ProxCloudKubeMint' --propagate "$PC_PROP" </dev/null
done
# a site set up before the group: the mint role sat on /access/groups and /pool and held User.Modify. These remove
# exactly this token's rows, after the grant on the group above, and change nothing on a new site
pveum acl delete '/access/groups' --users "$PC_USER" --roles 'ProxCloudKubeMint'
pveum acl delete '/access/groups' --tokens "$PC_TOKEN" --roles 'ProxCloudKubeMint'
pveum acl delete '/pool' --users "$PC_USER" --roles 'ProxCloudKubeMint'
pveum acl delete '/pool' --tokens "$PC_TOKEN" --roles 'ProxCloudKubeMint'
# User.Modify leaves the role for every holder at once, so only when no other portal token still creates tokens
# through a row on /access/groups: run this script for each of them, the last run takes the privilege out
PC_LEFT=$(pveum acl list --output-format json | perl -MJSON::PP -e 'print map { "  $_->{type} $_->{ugid}\n" } grep { $_->{path} eq "/access/groups" && $_->{roleid} eq "ProxCloudKubeMint" } @{ decode_json(join "", <STDIN>) }')
[ -n "$PC_LEFT" ] || pveum role modify 'ProxCloudKubeMint' --privs 'Permissions.Modify'
pc_left() { # said last, when this subshell ends, below everything the script prints
  echo 'NOT FINISHED: ProxCloudKubeMint still holds User.Modify, because these still hold that role on /access/groups:'
  echo "$PC_LEFT"
  echo 'Each is another portal token, or its user, that creates cluster tokens through that row and would stop if the role lost'
  echo 'User.Modify now. Run this script for each such token (tokenId=<its id>), or make upgrade for the token of an installer site;'
  echo 'the last run takes User.Modify out of the role. Until then a token listed here whose user is listed too can create an API'
  echo 'token for any user of the cluster, root@pam included. A row no portal uses: pveum acl delete /access/groups --users <user>'
  echo '(or --tokens <token id>, in single quotes) --roles ProxCloudKubeMint, then this script again.'
}
[ -z "$PC_LEFT" ] || trap pc_left EXIT
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
| `proxcloud@pve!platform` (user and token) | `/access/groups/proxcloud-kube` (no propagate) | `ProxCloudKubeToken` | Create and delete tokens for the members of that group, `capmox@pve` and `csi@pve`. Never on `/access/groups`: there it reaches every user ([the mint grant](#the-mint-grant-in-m1)). |
| `proxcloud@pve!platform` (user and token) | `/storage`, `/sdn/zones` (propagate), `/nodes` (no propagate) | `ProxCloudKubeMint` | No row on `/pool`: there the portal's `ProxCloudPlatform` is enough. Grant the tenant clusters' tokens their roles ([Managed Kubernetes](#managed-kubernetes)), with `ProxCloudPlatform` repeated on `/sdn/zones`. |
| `proxcloud@pve!platform` (user and token) | `/storage/<importStorage>` (no propagate), only with `importStorage` | `ProxCloudKubeMint` | The `PVEDatastoreAdmin` row there replaces what `/storage` passes down, and a tenant cluster's ISO or boot store may be that store (`local` on a site without a NAS): without it PVE refuses the cluster token's row there and the cluster create fails. Stage 10 writes the same row (`nas_mint_acls`). |
| `proxcloud@pve` or `proxcloud@pve!platform`, whichever holds the row | every `/storage/<store>`, `/sdn/zones/<zone>` and `/sdn/zones/<zone>/<vnet>` (not a bridge of `localnetwork`) where it already holds a row of its own with another role (propagate as that row) | `ProxCloudKubeMint` | Not in `pve.ManagedACLs`: found in the ACL list when the script or stage 10 runs, on a site that wrote such rows itself or with an older setup. The row replaces the mint role the parent passes down ([a row of the portal's own](#a-row-of-the-portals-own-on-a-store-or-a-zone)). |

The other rows (`/`, `/nodes`, `/pool/pc-templates`, `/sdn/zones`, the two `/storage` rows of `capmox@pve!capi`, and `/pool` and `/storage` for both users) are those of [ProxCloudCAPI](#proxcloudcapi) and [ProxCloudCSI](#proxcloudcsi).

The portal checks a site against the same tables (`internal/cloud/pve/access.go`), each with a client on that token: `MissingPrivileges` with `CAPIPrivileges`, `CSIPrivileges` and `KubeMintPrivileges` (which reads the paths a mint writes and not their parents alone: the template pool, `/nodes`, `/sdn/zones/localnetwork`, each zone of the provider, and the ISO store, the import store and every boot store of the provider's storage profiles; a failed line on a single store or zone says that a row of the portal's own hides the mint role there); `ISOVisibilityProblem`, which asks PVE per `iso` store whether the CAPI token sees it and wants exactly `isoStorage` (CAPMOX puts each bootstrap ISO on the first `iso` store its token sees, as stage 10 checks for the services cluster); and `CheckBootISOGrant` for `capmox@pve`. They are the rows `privileges:capi`, `privileges:csi`, `privileges:kubemint`, `iso-store-visibility` and `boot-iso-grant` of the page's Check step and of a run's first stage, next to `mint-scope`, which fails when the portal token holds `User.Modify` or `Permissions.Modify` on `/`, `/access` or `/access/groups` or `Permissions.Modify` on `/pool` (`HeldPrivileges` with `KubeMintForbidden`; Test connection warns on the same), or when `capmox@pve` or `csi@pve` is not in the group `proxcloud-kube` (`KubeMintGroupProblems`: PVE then refuses every token create or delete for that user), `pool-fence`, which fails when the portal token holds any privilege on `/pool/proxcloud-system` or `/pool/proxcloud-seed`, or `csi@pve!csi` on `/pool/proxcloud-seed`, and `token-scope`, which fails when `capmox@pve!capi` or `csi@pve!csi` holds any privilege on `/pool`, `/storage` or `/vms` as a whole (no row of the tables above gives one; an older setup wrote some, the script removes no row, and stage 10 removes only the CAPI token's old rows on `/storage` and on the NAS template store, so the message gives the `pveum acl delete` command; a token made without privilege separation fails it on its user's `/pool` and `/storage` rows and has no row of its own on those paths or above them with propagate to remove (its rows on the platform pools and stores stay), and the message then gives `pveum user token modify … --privsep 1`). A row of the two tokens on a single tenant pool or store is not compared with the tables ([managed-services.md](managed-services.md#step-3-check)).

### Sandboxes on the managed platform

With `spec.sandbox` ([managed-services.md](managed-services.md#presets-and-the-control-plane-count); no release
offers it yet) the sandbox machines go to a pool of their own, `proxcloud-sbx`, in both placements. Not
`proxcloud-system`: the provider sync writes the VM firewall of a sandbox machine with the portal token, which
holds `NoAccess` there.

**No new role, no new privilege, and no row for root to add.** The portal token makes the pool and writes what
opens it, with rights the two scripts above already grant it for tenant pools:

| What the run does (portal token) | Call | The right it rests on | Granted by |
|---|---|---|---|
| create and delete the pool | `POST /pools`, `DELETE /pools/proxcloud-sbx` | `Pool.Allocate` on `/pool` | the portal token script (`ProxCloudPlatform` on `/pool`, propagate) |
| `shared`: one row, `capmox@pve!capi` with `ProxCloudCAPI` on `/pool/proxcloud-sbx` (propagate), and its removal | `PUT /access/acl` | below `/pool` PVE takes `Pool.Allocate` in place of `Permissions.Modify` for a role whose every privilege the caller holds there with propagate: `ProxCloudPlatform` holds all 19 of `ProxCloudCAPI` | the portal token script, as for a tenant pool ([the mint grant](#the-mint-grant-in-m1)) |
| `separate`: the token `capmox@pve!pc-sandbox` and its removal | `POST`, `DELETE /access/users/capmox@pve/token/pc-sandbox` | `User.Modify` on `/access/groups/proxcloud-kube` | the managed-services script |
| `separate`: that token's rows: `ProxCloudCAPI` on `/pool/proxcloud-sbx`, `/storage/<bootStorage>`, `/nodes`, `/sdn/zones/localnetwork` and, where the platform network is an SDN vnet, `/sdn/zones/<zone>/<vnet>`; `PVETemplateUser` on `/pool/pc-templates`; `PVEDatastoreAdmin` on `/storage/<isoStorage>` | `PUT /access/acl` | `Pool.Allocate` below `/pool`; `ProxCloudKubeMint` (`Permissions.Modify`) on `/storage`, `/sdn/zones` (propagate) and `/nodes` | both scripts, exactly as for a tenant cluster's token |

What each principal then holds on `/pool/proxcloud-sbx`:

| Principal | Holds | Why |
|---|---|---|
| `proxcloud@pve!platform` | `ProxCloudPlatform`, inherited from `/pool` | It reads the sandbox machines and writes their VM firewall, as on a tenant pool. This is the difference from the installer's `proxcloud-sandbox`, where root narrows the portal to `ProxCloudSandboxGuard`: here the portal must hold `Pool.Allocate` on the pool to write the row at all, and root is not asked a second time. A stolen portal token can do to a sandbox machine what it can do to a tenant's VM (console, guest agent, disks, power). **With `shared` that machine is a node of the platform cluster**: the token so reaches the position of code that escaped a sandbox there (a kubelet's and a Cilium agent's credential on the cluster that runs every tenant's control plane), which `NoAccess` on `proxcloud-system` keeps it from on every other platform node. With `separate` it reaches the sandbox cluster alone. |
| `capmox@pve!capi` | `shared`: `ProxCloudCAPI` (the one row). `separate` and no sandboxes: nothing | The platform's own Cluster API builds the group's machines. Its user `capmox@pve` holds the role on `/pool` already; a privilege-separated token holds the intersection, so the row is what opens the pool, and its removal closes it. |
| `capmox@pve!pc-sandbox` | `separate`: `ProxCloudCAPI` | The sandbox cluster's own token, in one Secret on the platform cluster (`pc-sandbox/pc-sandbox-pve`). It never sits on a sandbox machine. |
| `csi@pve!csi` | nothing | Its rows are on `proxcloud-system` and the boot store. No volume is attached to a sandbox machine: the proxmox-csi node plugin is kept off the group, and `pc-sandbox` has no CSI. |
| `csi@pve` (user) | `ProxCloudCSI`, inherited from `/pool` | No `NoAccess` row as on the installer's sandbox pool: only root could write one (PVE takes `NoAccess` from a caller with `Permissions.Modify` alone), and on a console-managed site no `csi@pve` token holds a row on `/pool` that would reach the pool. A site that made `csi@pve!gateway` by hand with `/pool` can cap it: `pveum acl modify /pool/proxcloud-sbx --users csi@pve --roles NoAccess --propagate 1`, after an apply made the pool. |

**An existing site** needs nothing run again when its Check passes: the row `sandbox` reads, with the portal token,
exactly the rights of the first table on the paths the run writes (and with `shared` on an installed group, with
`capmox@pve!capi`, the row itself). It fails on a site whose scripts ran before these rows existed, whose rows were
changed since, or that gave the portal's user or token a row of its own on `/pool/proxcloud-sbx`, on the platform's
boot or ISO store or on the platform network's zone (such a row replaces what the parent passes down). The fix it
names is the root step of every such case: **run the managed-services script again as root on a node**
(`GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage=<store>&isoStorage=<store>`, or step 2 of
the Managed services page). It writes the scripts' rows again, the mint role next to a row of the portal's own
included; a row on `/pool/proxcloud-sbx` itself it does not touch: delete it
(`pveum acl delete /pool/proxcloud-sbx --tokens '<the portal token>' --roles <role>`, and `--users` for the user's).
An apply that meets the refusal stops at `sandbox_access_failed` with nothing of the cluster changed.

Not run on a lab. The `ProxCloudCAPI` row on a pool is the write every tenant cluster create makes on its own pool,
and [the mint grant](#the-mint-grant-in-m1) says how far Proxmox taking `Pool.Allocate` for it is verified; it has not
been made on this pool, and no site has built sandboxes this way.

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

### What the fence hides from the portal: the seed's backups

Proxmox lists a backup to a token only when the token may read it: `Datastore.Allocate` on the store, or
`Datastore.AllocateSpace` on the store together with `VM.Backup` on the VM the archive is of. A token that holds
neither still gets `200` for the store's content: the list simply leaves those archives out
(`PVE::Storage::check_volume_access`, run for every row of the listing). The `NoAccess` row on `proxcloud-seed` takes
`VM.Backup` on the seed VM from the portal token, `ProxCloudCAPI` and `ProxCloudCSI` do not carry it, and no
privilege is added for this. So the product's tokens see a backup of the seed VM in two places only, each because the
token manages the store: `/storage/<importStorage>` (`PVEDatastoreAdmin`, the portal token) and the platform's ISO
store (`PVEDatastoreAdmin`, `capmox@pve!capi`). The Check row `seed-backup` reads with both and, for every other
backup store, says that the portal cannot see backups of the seed VM there; it never reports "none" for a store whose
listing Proxmox may have filtered. An upgrade that replaces the seed's k3s therefore asks the operator to confirm the
backup ([managed-services.md](managed-services.md#the-seeds-k3s)). Seen on a PVE 9.2 lab on 2026-11-20: the portal
token got `{"data":[]}` for a store that held an archive of the seed made minutes before. Do not grant `VM.Backup`
on the seed's pool to make the row green: with `Datastore.AllocateSpace` on a store it lets the token restore the
seed's archive, which holds the seed's cluster-admin token and the `capmox@pve!capi` secret.

### An existing site

Stage 10 only adds rows. On a site installed before these rows, `make upgrade` adds the narrow rows and leaves the broad ones: `capmox@pve!capi` keeps `ProxCloudCAPI` on `/pool`, and `csi@pve!csi` keeps `ProxCloudCSI` on `/pool` and `/storage`. The new rows grant nothing the old ones did not, so nothing breaks and nothing narrows until an operator removes the old token rows, after stage 10 has run with this release (the last `delete` only when every StorageClass of the services cluster is on `services.storage`, as the installer makes it):

```sh
pveum acl delete /pool    --tokens 'capmox@pve!capi' --roles ProxCloudCAPI
pveum acl delete /pool    --tokens 'csi@pve!csi'     --roles ProxCloudCSI
pveum acl delete /storage --tokens 'csi@pve!csi'     --roles ProxCloudCSI
pveum user token permissions capmox@pve capi --path /pool/proxcloud-system
pveum user token permissions csi@pve csi --path /storage/<services.storage>
```

**The mint grant.** Until this change `ProxCloudKubeMint` held `User.Modify` and `Permissions.Modify` and sat on `/access/groups`, where it let the portal token create an API token for every user of the cluster, `root@pam` included, and on `/pool`, where it let the token write any role on every pool ([below](#the-mint-grant-in-m1)). Neither path removes rows in general, but both remove exactly these four: the role on `/access/groups` and on `/pool`, for the portal's user and for its token. On an installer site, `make upgrade` (stage 10, `mode: split`) creates the group `proxcloud-kube`, puts `capmox@pve` and `csi@pve` in it, creates `ProxCloudKubeToken` and grants it on the group, then deletes its four old rows, then takes `User.Modify` out of `ProxCloudKubeMint`, and last checks with PVE that the token holds neither privilege on `/access/groups` and `/pool`. On a site set up with the console's script, run the managed-services script again as root (`GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage=<store>&…`, or copy it from the Managed services page): it does the same in the same order. The new grant is written before the old rows and the old privilege go, so the portal token creates and deletes cluster tokens throughout, and a run that stops half way leaves the old grant, and minting, in place.

**More than one portal token on the cluster.** The role belongs to the cluster: taking `User.Modify` out of it changes it for every holder. A second portal token (another provider or portal, under the same user or another one) that was set up before the group still creates tokens through its own `ProxCloudKubeMint` row on `/access/groups` and has no grant on the group, so it would answer 403 on every cluster create and release. The script and stage 10 therefore take the privilege out of the role only when no other user or token holds the role on `/access/groups`. Otherwise they leave the role as it is and say `NOT FINISHED` with the list: the script as its last output, stage 10 as a failed task it does not stop on. Run the script for each token named (`tokenId=<its id>`), or `make upgrade` for an installer's `proxcloud@pve!platform`; the last run finds no other row and takes the privilege out. In between:

- a token whose user's row is already gone (a second token of the same user) keeps minting through the user's new row on the group and reaches the two service users only;
- a token whose user and token rows are both still there (another portal's) is as wide as before, and its own `mint-scope` check fails until its script has run.

A row no portal uses is removed by hand (`pveum acl delete /access/groups --users <user> --roles ProxCloudKubeMint`, or `--tokens '<id>'`), then the script or `make upgrade` again.

By hand, in this order (use the provider's token id if it is not `proxcloud@pve!platform`):

```sh
pveum group add proxcloud-kube --comment "ProxCloud: users the portal mints cluster tokens for"
pveum user modify capmox@pve --groups proxcloud-kube --append 1
pveum user modify csi@pve    --groups proxcloud-kube --append 1
pveum role add ProxCloudKubeToken --privs "User.Modify"
pveum acl modify /access/groups/proxcloud-kube --users proxcloud@pve             --roles ProxCloudKubeToken --propagate 0
pveum acl modify /access/groups/proxcloud-kube --tokens 'proxcloud@pve!platform' --roles ProxCloudKubeToken --propagate 0
# before anything is removed: must list User.Modify. A row on the group's own path replaces what /access/groups
# passes down, so this reads the new row and not the old one
pveum user token permissions proxcloud@pve platform --path /access/groups/proxcloud-kube
pveum acl delete /access/groups --users proxcloud@pve             --roles ProxCloudKubeMint
pveum acl delete /access/groups --tokens 'proxcloud@pve!platform' --roles ProxCloudKubeMint
pveum acl delete /pool          --users proxcloud@pve             --roles ProxCloudKubeMint
pveum acl delete /pool          --tokens 'proxcloud@pve!platform' --roles ProxCloudKubeMint
# the next line only when this prints nothing: every row it prints is another portal token's (see above)
pveum acl list | grep -F ProxCloudKubeMint | grep -F ' /access/groups '
pveum role modify ProxCloudKubeMint --privs "Permissions.Modify"
# must list neither User.Modify nor Permissions.Modify
pveum user token permissions proxcloud@pve platform --path /access/groups
# must list Pool.Allocate and not Permissions.Modify
pveum user token permissions proxcloud@pve platform --path /pool
# must still list User.Modify
pveum user token permissions proxcloud@pve platform --path /access/groups/proxcloud-kube
```

`pveum` marks a privilege with `(*)` only when it is held with propagate; the group row is not, so `User.Modify` shows there without the mark (the API returns it with value 0). Read the names, not the marks or the values.

Until that has run, the preflight row `mint-scope` fails and Test connection warns. The old grant was live for as long as the site ran it, so also look for tokens nobody on your side created: `pveum user token list root@pam`, and the same for every other administrator account.

### The mint grant in M1

The portal token creates one `capmox@pve!<clusterId>` token per tenant cluster, grants it its roles, and deletes it with the cluster. Two roles carry that, on these rows, each for the portal's user and for its token, next to the portal's own role on `/pool`:

| Path | Role | Propagate | What the product does with it |
|---|---|---|---|
| `/access/groups/proxcloud-kube` | `ProxCloudKubeToken` (`User.Modify`) | 0 | `POST` and `DELETE /access/users/capmox@pve/token/<clusterId>`; `DELETE /access/users/csi@pve/token/<clusterId>` for a cluster from before the CSI gateway; `GET /access/users/capmox@pve` (the boot ISO check and `mint-scope`) and `GET /access/users/csi@pve` (`mint-scope`) |
| `/storage`, `/sdn/zones` | `ProxCloudKubeMint` (`Permissions.Modify`) | 1 | `PUT /access/acl` for the cluster token on the boot and ISO stores, `/sdn/zones/localnetwork` and the cluster's vnet |
| `/pool` | none for minting: `ProxCloudPlatform`, which the portal holds there for its own work | 1 | `PUT /access/acl` for the cluster token on the tenant pool (`ProxCloudCAPI`) and on `pc-templates` (`PVETemplateUser`). On `/pool/<pool>` PVE accepts `Pool.Allocate` in place of `Permissions.Modify` when the caller holds every privilege of the role there itself, with propagate; `ProxCloudPlatform` holds all 19 of `ProxCloudCAPI` and both of `PVETemplateUser` |
| `/nodes` | `ProxCloudKubeMint` | 0 | `PUT /access/acl` for the cluster token on `/nodes` |
| `/storage/<importStorage>` | `ProxCloudKubeMint` | 0 | the same, where the portal's own `PVEDatastoreAdmin` row shadows `/storage` |
| a store, a zone or a vnet (not a bridge of `localnetwork`) where the site gave the portal's user or token a row of its own | `ProxCloudKubeMint`, for the holder of that row | as that row | the same, where that row shadows `/storage` or `/sdn/zones`; written by the script and by stage 10 from the ACL list, on such a site only ([a row of the portal's own](#a-row-of-the-portals-own-on-a-store-or-a-zone)) |

The group `proxcloud-kube` holds `capmox@pve` and `csi@pve` and nothing else: not `proxcloud@pve`, not `images@pve`, no operator account. The group has no ACL row of its own, so membership gives the two users no right.

**Why a group.** PVE checks a token create, update or delete for user X with `userid-group` (`PVE::RPCEnvironment::exec_api2_perm_check`, read at `libpve-access-control` 9.2.1): the caller passes with `User.Modify` on `/access/groups` itself, **for every user of the cluster, whatever its groups**; otherwise it needs `User.Modify` on `/access/groups/<g>` for a group X is in, and a user in no group, such as `root@pam`, is never reached. A token is never "its own user" for this check (`proxcloud@pve!platform` is not `proxcloud@pve`). Until this change the portal held both privileges of `ProxCloudKubeMint` on `/access/groups`, and the lab confirmed what that means ([L1 results](#l1-results)): the portal token created a token for `root@pam`.

What a stolen portal token can do with each row:

- **on `/access/groups/proxcloud-kube` (`User.Modify`):** create privilege-separated tokens for `capmox@pve` and `csi@pve` (a new token holds nothing until it is granted a role, and never more than its user holds); delete any token of the two users, or change its expiry and comment, the platform's `capmox@pve!capi`, `csi@pve!csi`, `csi@pve!gateway` and the sandbox cluster's token included; read the two users and set `enable`, `expire`, name, email, comment and keys on them; unlock their TFA. That stops managed Kubernetes and the platform; it does not raise the token's rights by itself. It can **not** do any of this to a user outside the group (`root@pam`, `proxcloud@pve`, `images@pve`, an operator: 403), put another user into the group (PVE wants the target to be in one of the caller's groups already, and the portal holds no `Group.Allocate`), create or delete a user or set a password (`Realm.AllocateUser`), or write an ACL row under `/access` (no `Permissions.Modify` there; `ProxCloudKubeToken` has none).
- **on `/pool` (no mint row; `ProxCloudPlatform`):** on a pool where it holds `Pool.Allocate`, which is every tenant pool and `pc-templates` and not `proxcloud-system`, `proxcloud-seed` or the sandbox pools (its row there is `NoAccess` or a role without it), write a role made only of privileges it holds there itself, with propagate, for any user or token. A token it minted for `capmox@pve` so gets at most `ProxCloudCAPI` on tenant pools, which is nothing the stolen token could not do itself. It can **not** write a row on `/pool` itself (PVE takes `Pool.Allocate` only below `/pool`), write `Administrator`, `NoAccess` or any role with a privilege it lacks, or give a minted token a row that reaches a platform pool: such a token has no row on `/pool`, so it holds nothing on `proxcloud-system` and `proxcloud-seed`. Until 2026-11-16 the portal held `ProxCloudKubeMint` on `/pool` too, which allowed all three; the lab measured the first with that row ([L1 results](#l1-results)). That the two writes of the product pass without the row is read from the PVE source (`PVE::RPCEnvironment::check_perm_modify`, `PVE::API2::ACL` `update_acl`, `libpve-access-control` 9.2.1) and has not run on a cluster yet.
- **on `/storage`:** grant its own user and token `PVEDatastoreAdmin` on `/storage/<bootStorage>`. No pool row stands in the way, because PVE checks a disk against its store alone once the caller holds `Datastore.Allocate` there ([what the seed pool does not fence](#what-the-seed-pool-does-not-fence)). With `VM.Config.Disk` and `VM.GuestAgent.Unrestricted` on the tenant pools it then attaches any disk of that store, the seed's, a workload VM's or another tenant's, to a tenant VM and reads it through the guest agent: the seed's cluster-admin token and the `capmox@pve!capi` secret are on the seed's disk. A8 accepts it for M1 with the rest of the mint grant (a stolen portal token "can grant itself anything on those paths"), and M1 has no row that closes it.
- **on `/sdn/zones` and `/nodes`:** any role, `Administrator` included, with propagate, for itself or any other user or token, on the zones and vnets and on every node.
- **on a single store or zone where the script repeated the mint role (v0.8.1):** what the bullets on `/storage` and `/sdn/zones` say, on that path. Before the script ran there the site's own row kept the stolen token from granting itself anything on it; the row closes that accidental gap in the token's reach, which is why it is named here ([what each added row lets a stolen portal token do](#a-row-of-the-portals-own-on-a-store-or-a-zone)).

The rows on `/storage`, `/sdn/zones` and `/nodes` are what is left of the risk the owner accepted for M1 on 2026-11-15 ([ADR 0009](adr/0009-managed-services-platform-lifecycle.md#a8-risks-accepted-for-the-first-milestone), A8), and they stay: PVE has no narrower right than `Permissions.Modify` for writing a role on `/nodes`, `/sdn/zones/...` or (without `Datastore.Allocate`) `/storage/...`, and with it the holder may write any role, `Administrator` included, with propagate, for any principal. (On `/pool/<pool>` PVE also accepts `Pool.Allocate` for a role whose privileges the caller holds there, which is why the `/pool` row is gone; `/storage/<store>` has the same with `Datastore.Allocate`, which is itself the right that opens every disk of a store, so it is no way out there.) Two things stay open inside it and are stated here, not closed:

- `Administrator` on `/nodes` or `/storage` for the stolen token itself: what `Sys.Modify` and the other node privileges then reach on a host was not analysed.
- The disruption listed for the group row.

**PVE versions.** `libpve-access-control` 9.2.1 (2026-11-13) made creating a token *without* privilege separation for another user, regenerating another user's token secret, and any update that leaves such a token without privilege separation require `Realm.AllocateUser` on the realm and `Permissions.Modify` on `/`, which the portal holds on no site. Before 9.2.1, `User.Modify` through the group check was enough for all three. For a site still on the old grant that is one call to full root (`privsep=0` for `root@pam`), or the takeover of any existing token by regenerating its secret. With the grant on the group it is limited to `capmox@pve` and `csi@pve`, and there it still matters: a `privsep=0` token holds what that user holds (`ProxCloudCAPI` or `ProxCloudCSI` on `/pool` and `/storage` and the rows of the tables above, so `ProxCloudCAPI` on both platform pools), and a regenerated `capmox@pve!capi` or `csi@pve!gateway` secret is that token's rights. Both are more than a stolen portal token reaches on 9.2.1 now that the `/pool` row is gone. **Managed Kubernetes therefore needs `libpve-access-control` 9.2.1 or later on every node** (`pveversion -v | grep access-control`); the product does not check the package version. This paragraph is from the PVE source and changelog; none of it was tried on the lab, and the lab record does not say which `libpve-access-control` the lab ran. The product itself only ever sends `privsep=1`.

**Detection.** The preflight row `mint-scope` fails, and Test connection on the provider warns (`mint-scope`), while the portal token holds `User.Modify` or `Permissions.Modify` on `/`, `/access` or `/access/groups`, or `Permissions.Modify` on `/pool`; the message says to run the script again. The preflight row also fails when `capmox@pve` or `csi@pve` is not in the group, with the `pveum user modify` line that puts it back: PVE then answers 403 to every token create or delete for that user although the privileges read complete. Installer stage 10 asserts the two paths for `proxcloud@pve!platform` in split mode after it has moved the grant, and names every other user or token that still holds `ProxCloudKubeMint` on `/access/groups`. [An existing site](#an-existing-site) has the commands.

The structural fix for the rows that stay is to narrow the `/storage` grant to the stores tenant clusters use, with the platform's disks on a store outside them; a site whose Kubernetes provider runs in `sharedWorkers` mode needs no mint grant at all.

Lab check L1 is the drill for it on PVE 9.2. **The run of 2026-11-16 was a part of it:** it created no VM and ran the read-only rows, the `/pool` grant and the token-create probes; the two disk-attach steps are still to run ([results](#l1-results)). As designed, L1 uses three scratch VMs on one node and the boot store, in `proxcloud-seed`, `proxcloud-system` and a tenant pool, destroyed afterwards. The checks above must be green; `pveum user token permissions` for the portal token must be empty on both platform pools and `csi@pve csi` is empty on `proxcloud-seed` but holds `Datastore.Allocate` on `/storage/<bootStorage>`; `capmox@pve capi` must see one ISO store. The CSI token attaches the seed-pool VM's disk to the `proxcloud-system` one (`PUT /nodes/<node>/qemu/<vmid>/config` with `scsi9=<bootStorage>:vm-<seed-pool vmid>-disk-0`), which PVE is expected to allow. Then a drill with the portal token, reverted afterwards: grant `capmox@pve` `ProxCloudPlatform` on `/pool` and read its effective rights on `/pool/proxcloud-seed`, which must stay `ProxCloudCAPI`; grant its own user and token `PVEDatastoreAdmin` on `/storage/<bootStorage>`, read `pveum user token permissions proxcloud@pve platform --path /storage/<bootStorage>`, and attach the seed-pool VM's disk to the tenant one, which PVE is expected to allow; and try `POST /access/users/root@pam/token/x`, to learn whether PVE lets it mint for `root@pam`. After this change the first and the last of these must answer 403 ([the re-drill](#the-re-drill-still-to-run)).

#### L1 results

> **Measured on 2026-11-16 on the lab (`pve-manager/9.2.26`, three nested nodes), with the grant as it was before this change: `ProxCloudKubeMint` (`User.Modify Permissions.Modify`) on `/access/groups` and on `/pool`.** The portal token there was `proxcloud@pve!m0`. Rows marked "not run" were not part of that run. Nothing has been measured with the grant on `/access/groups/proxcloud-kube`, or without the row on `/pool`, yet: the re-drill after this change is still to run.

| Step of the drill | Expected | Measured (2026-11-16, PVE 9.2.26, old grant) |
|---|---|---|
| `pveum user token permissions` for the portal token on `/pool/proxcloud-system` and `/pool/proxcloud-seed` | empty | empty on both |
| `pveum user token permissions csi@pve csi` on `/pool/proxcloud-seed` | empty | empty |
| `csi@pve!csi` on `/storage/<bootStorage>` | holds `Datastore.Allocate` | its rows are `ProxCloudCSI` on `/pool/proxcloud-system` and on the boot store (`local-zfs`); nothing on `/`, `/pool`, `/storage`, `/vms`, `/sdn/zones`, `/nodes` |
| ISO stores `capmox@pve!capi` sees | one, `isoStorage` | one, `local` |
| CSI token attaches the seed-pool VM's disk to the `proxcloud-system` VM | allowed (the open point of [what the seed pool does not fence](#what-the-seed-pool-does-not-fence)) | not run |
| Portal token grants `capmox@pve` `ProxCloudPlatform` on `/pool` (`PUT /access/acl`); effective rights of `capmox@pve` and `capmox@pve!capi` on `/pool/proxcloud-seed` and `/pool/proxcloud-system` | still `ProxCloudCAPI` | grant accepted (HTTP 200); `ProxCloudCAPI` (19 privileges) on both pools for both, unchanged; `capmox@pve` on `/pool` itself rose to 33. Reverted with the portal token (HTTP 200); the ACL list was byte-equal to the one before (102 rows) |
| Portal token grants itself `PVEDatastoreAdmin` on `/storage/<bootStorage>`, then attaches the seed-pool VM's disk to the tenant VM | allowed (accepted in A8) | not run |
| Portal token calls `POST /access/users/root@pam/token/l1probe` (privilege separated) | unknown: the drill was to find out | **allowed: HTTP 200, token `root@pam!l1probe` created**, privilege separated, no ACL row. The same call for `capmox@pve` and for `proxcloud@pve`: HTTP 200, created. All three removed as root at once; token lists and ACL list back to the state before |
| The same without privilege separation (`privsep=0`, full root) | not part of the drill | **not tried**: no full-root credential was created, even for a moment |
| Portal token's effective privileges on `/nodes` | `PVEAuditor` and `Permissions.Modify` (the `ProxCloudKubeMint` row, propagate 0) | the 7 privileges of `PVEAuditor` only, no `Permissions.Modify`, though the row `/nodes proxcloud@pve!m0 ProxCloudKubeMint` exists. Explained from the PVE source afterwards, not measured again: a privilege held without propagate comes back with value 0 (`PVE::RPCEnvironment::permissions` maps each privilege to its propagate bit, and `pveum` prints it without `(*)`), so a reading that keeps only true values drops it. The ACL write on `/nodes` of every cluster create on that lab passes, which needs the privilege. The re-drill reads the names, not the values |

#### The re-drill, still to run

Not run yet. It needs `libpve-access-control` 9.2.1 or later on every node (`pveversion -v | grep access-control`), the lab's two portal tokens moved (the script for `proxcloud@pve!m0` and `make upgrade` for `proxcloud@pve!platform`, in either order), and a copy of `pveum acl list` and of the token lists taken before. Each call is made with the portal token unless it says root.

| Step | Expected |
|---|---|
| After the first of the two migrations | it ends with `NOT FINISHED` naming the other token; `pveum role list` still shows `User.Modify` in `ProxCloudKubeMint`; a cluster create with either token works |
| After the second | no `NOT FINISHED`; `ProxCloudKubeMint` is `Permissions.Modify` alone; `mint-scope` and `privileges:kubemint` are ok for both providers |
| `pveum user token permissions` for the portal token on `/access/groups`, `/access/groups/proxcloud-kube`, `/pool`, `/nodes` (root) | neither privilege; `User.Modify` without `(*)`; `Pool.Allocate` and no `Permissions.Modify`; `Permissions.Modify` without `(*)` |
| `POST /access/users/root@pam/token/l1probe`, the same for `proxcloud@pve` and `images@pve` | 403, no token |
| `POST` then `DELETE /access/users/capmox@pve/token/l1probe`, the same for `csi@pve` | 200 and 200 |
| `POST /access/users/capmox@pve/token/l1probe0` with `privsep=0` | 403. If it answers 200, delete the token as root at once: the node is older than 9.2.1 |
| `PUT /access/users/root@pam` with `groups=proxcloud-kube&append=1` | 403 |
| `PUT /access/acl` `path=/access/groups roles=ProxCloudKubeToken` for its own token | 403 |
| `PUT /access/acl` `path=/pool roles=ProxCloudPlatform users=capmox@pve` (200 on 2026-11-16), and `roles=Administrator` | 403 both |
| `PUT /access/acl` `path=/pool/<a tenant pool> roles=ProxCloudCAPI tokens=capmox@pve!l1probe` (create the token first) | 200 |
| The same with `roles=Administrator`, and `roles=ProxCloudCAPI` on `path=/pool/proxcloud-system` | 400 ("requires 'Permissions.Modify' or superset of privileges"); 403 |
| Create one tenant cluster in the console, then delete it | both succeed; `capmox@pve!<clusterId>` exists in between and is gone after |
| As root `pveum user modify csi@pve --groups ''`, then the Check step | `mint-scope` fails and names `csi@pve`; then `pveum user modify csi@pve --groups proxcloud-kube --append 1` and it passes |
| The two disk-attach steps of L1 | allowed (they are the open points of A8 and of the seed pool), not run on 2026-11-16 |

Afterwards: delete every `l1probe` token (as root if the portal token's delete fails), and compare `pveum acl list` with the copy: the only differences are the four old `ProxCloudKubeMint` rows per portal token gone and the two `ProxCloudKubeToken` rows per portal token on `/access/groups/proxcloud-kube` added.

## pveum commands

Run these on any node (cluster config is shared). Replace the `<…>` placeholders.

```sh
# roles
pveum role add ProxCloudPlatform --privs "Sys.Audit Sys.Console Sys.Modify Pool.Allocate Pool.Audit VM.Allocate VM.Audit VM.Backup VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.Console VM.GuestAgent.Audit VM.GuestAgent.Unrestricted VM.Migrate VM.PowerMgmt VM.Replicate VM.Snapshot VM.Snapshot.Rollback SDN.Allocate SDN.Audit SDN.Use Datastore.AllocateSpace Datastore.AllocateTemplate Datastore.Audit Mapping.Audit Mapping.Use"
pveum role add ProxCloudCAPI --privs "Sys.Audit Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.GuestAgent.Audit VM.Migrate VM.PowerMgmt Datastore.AllocateSpace Datastore.Audit SDN.Use"
pveum role add ProxCloudCSI --privs "VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit"
pveum role add ProxCloudKubeMint --privs "Permissions.Modify"                               # mode: split
pveum role add ProxCloudKubeToken --privs "User.Modify"                                     # mode: split
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
