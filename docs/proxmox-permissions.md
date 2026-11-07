# Proxmox VE permissions

ProxCloud needs no `root@pam` access on the cluster. The [installer](install.md) creates everything on this page (`installer/playbooks/10-pve-identity.yml`). This page defines four least-privilege roles, the users and API tokens that hold them, and the ACL paths they are granted on. The commands are for **PVE 9**. PVE 9 removed `VM.Monitor`; the guest-agent reads ProxCloud uses need `VM.GuestAgent.Audit` instead.

| Role | Held by | Used for |
|---|---|---|
| `ProxCloudPlatform` | `proxcloud@pve!platform` | the portal API (the token configured on the Proxmox provider) |
| `ProxCloudCAPI` | `capmox@pve!capi` | Cluster API provider for Proxmox (CAPMOX), services cluster |
| `ProxCloudCSI` | `csi@pve!csi` | `proxmox-csi-plugin` in the services cluster |
| `ProxCloudImages` | `images@pve!packer` | the image pipeline ([images/README.md](../images/README.md)) |

Every token is created with **privilege separation** (`--privsep 1`). A privilege-separated token's rights are the **intersection** of the user's ACLs and the token's ACLs, so each ACL below is granted to both the user and the token.

The admin **Test connection** button on the Proxmox provider reads the token's effective permissions (`GET /access/permissions`) and lists each `ProxCloudPlatform` privilege the token lacks, plus a warning for a `root@pam` token. Missing privileges are only reported as warnings. A cluster without GPUs can, for example, leave out `/mapping/pci`. The list it checks is `pve.PlatformPrivileges` in `internal/cloud/pve/access.go`. Keep it in sync with this page.

## Pools

| Pool | Contents |
|---|---|
| `pc-<projectId>` | one per project, created by the portal (the prefix is the provider's `poolPrefix`) |
| `pc-templates` | OS and k8s node templates |
| `proxcloud-system` | management VMs, the services cluster and its storage VMs. The platform token gets `NoAccess` here. Its name must **not** start with `poolPrefix`. |

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
| `Datastore.AllocateTemplate` | upload a server's seed CD (user-data or a password on a network without DHCP), and a customer image to the import store. Deleting a CD needs `Datastore.Allocate`, which also defines storages, so the role leaves it out: the portal then overwrites a removed server's CD with an empty one and the file stays. To have them removed, grant `Datastore.Allocate` on the seed store alone, next to `ProxCloudPlatform` on that path, because an ACL on a path replaces what its parent passes down. |
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

There is no ACL on `/vms`. VMs get their rights only through their pool, so a VM outside `pc-*`/`pc-templates` is invisible to the portal.

## ProxCloudCAPI

This follows the CAPMOX "RBAC with least privileges" setup, written as one explicit role (CAPMOX's own example grants built-in `PVEVMAdmin`).

Privileges: `Sys.Audit Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.GuestAgent.Audit VM.Migrate VM.PowerMgmt Datastore.AllocateSpace Datastore.Audit SDN.Use`

| Path | Role | Propagate |
|---|---|---|
| `/` | `ProxCloudCAPI` | 0 |
| `/nodes` | `ProxCloudCAPI` | 1 |
| `/pool/proxcloud-system` | `ProxCloudCAPI` | 1 |
| `/pool/pc-templates` | `PVETemplateUser` (built-in) | 1 |
| `/sdn/zones/<zone>/<vnet>` | `ProxCloudCAPI` | 0 |
| `/storage/<disk storage>` | `ProxCloudCAPI` | 0 |
| `/storage/<cloud-init ISO storage>` | `PVEDatastoreAdmin` (built-in) | 0 |

The ISO storage holds each new node's kubeadm join token until it has joined, and CAPMOX uploads to the first
`iso` store the token sees, in no fixed order: make it node-local (`local`, the installer's default), and give the
token no other store with `iso` content ([managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox),
"Bootstrap ISOs"). Installs before this rule gave `capmox@pve!capi` `ProxCloudCAPI` on all of `/storage` and the ISO
role on the NAS template store; stage 10 (run by `make install` and `make upgrade`; `make identity-check` previews it)
now takes both off the token after granting the two rows above, checks with PVE that the token sees no other `iso` store, and keeps
`/storage` on the user, which the tenant clusters' tokens need.

### Managed Kubernetes

Each tenant cluster runs under tokens the portal mints at create and revokes after the delete cascade (`MintClusterTokens`, `RevokeClusterTokens` in `internal/cloud/pve/access.go`):

| Token | Grants | Reaches |
|---|---|---|
| `capmox@pve!<clusterId>` | `ProxCloudCAPI` on the project pool, the boot store, the vnet, `/sdn/zones/localnetwork` and `/nodes`; `PVETemplateUser` on `pc-templates`; `PVEDatastoreAdmin` on the node-local ISO store (provider `isoStorage`, default `local`), which needs the same role for `capmox@pve` itself | CAPMOX on the services cluster |
| `csi@pve!<clusterId>` | `ProxCloudCSI` on the project pool and the stores of the cluster's StorageClasses. Minted only with the provider's `csiSharedStores`: **`Datastore.Allocate` on a store opens every disk on it, of any project** ([managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox)) | the tenant cluster, where a cluster-admin can read it |

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

These are the privileges from the `proxmox-csi-plugin` install guide. That guide grants them on `/`; here they are scoped to the services cluster.

Privileges: `VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit`

| Path | Role | Propagate |
|---|---|---|
| `/pool/proxcloud-system` | `ProxCloudCSI` | 1 |
| `/storage/<csi storage>` | `ProxCloudCSI` | 0 |

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

## pveum commands

Run these on any node (cluster config is shared). Replace the `<…>` placeholders.

```sh
# roles
pveum role add ProxCloudPlatform --privs "Sys.Audit Sys.Console Sys.Modify Pool.Allocate Pool.Audit VM.Allocate VM.Audit VM.Backup VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.Console VM.GuestAgent.Audit VM.GuestAgent.Unrestricted VM.Migrate VM.PowerMgmt VM.Replicate VM.Snapshot VM.Snapshot.Rollback SDN.Allocate SDN.Audit SDN.Use Datastore.AllocateSpace Datastore.AllocateTemplate Datastore.Audit Mapping.Audit Mapping.Use"
pveum role add ProxCloudCAPI --privs "Sys.Audit Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.GuestAgent.Audit VM.Migrate VM.PowerMgmt Datastore.AllocateSpace Datastore.Audit SDN.Use"
pveum role add ProxCloudCSI --privs "VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit"
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
