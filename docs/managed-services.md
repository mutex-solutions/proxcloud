# Managed services from the operator console

How an operator builds, repairs and removes the platform that tenant Kubernetes clusters and tenant
databases run on, from the operator console (System → Managed services, `/platform`). The decisions
are in [ADR 0009](adr/0009-managed-services-platform-lifecycle.md); the routes are in
[api.md](api.md#managed-services-platform--apiv1adminplatform); the Proxmox roles and ACL rows are in
[proxmox-permissions.md](proxmox-permissions.md#managed-services).

> ## Status
>
> **Nothing on this page is verified on a lab.** The code is on main and covered by unit tests,
> renderer goldens, runner tests against a fake Proxmox and a fake Kubernetes API, and a mocked
> console. No run of the managed path has finished against a real Proxmox VE cluster yet: see
> [Not verified on a lab yet](#not-verified-on-a-lab-yet). It is off by default, and the release that
> carries it (v0.8.0) is not cut: see [Not implemented yet](#not-implemented-yet).

Code: `internal/platformlife` (the installation model, the preflight, the runner and its stages, the
renderers), `internal/platform/admin/platform.go` (the routes), `internal/cloud/pve/access.go` and
`setupscript.go` (the identities script), `deploy/platform/platform-bom.yaml` (what is installed),
`web/admin/src/pages/platform` (the page).

## 1. What it is

With the switch on, the console builds one **platform** on a Proxmox VE cluster:

- a **seed**: one VM in pool `proxcloud-seed` running k3s, with ArgoCD, cert-manager, Cluster API
  and CAPMOX. It builds the platform cluster and keeps its components at the versions of the BOM;
- a **platform cluster** (`pc-platform`, called the workload cluster in the code): a kubeadm
  cluster of VMs in pool `proxcloud-system`, with one control plane machine and three node groups:
  `system` (the platform's own components), `kamaji` (the control planes of tenant Kubernetes
  clusters) and `dbaas` (tenant databases). Every group but `system` is tainted, and each machine
  carries its group as the label `node-restriction.kubernetes.io/proxcloud-node-group`;
- the components of `deploy/platform/platform-bom.yaml`, each an ArgoCD Application on the seed.
  On the seed: `argocd` (a k3s HelmChart), `cert-manager`, `capi` and `pc-platform` (the cluster
  itself, from the chart `proxmox-kamaji-cluster` in `controlPlane.kind: kubeadm` mode). On the
  platform cluster: `cilium`, `cert-manager`, `metallb`, `argocd`, `cnpg`, `proxmox-csi`,
  `capi-tenant`, `kamaji-crds`, `kamaji-etcd`, `kamaji` and `platform`;
- two providers, `<name> Kubernetes` (`kamaji`) and `<name> Databases` (`dbaas`), registered with
  `attachToNewProjects: false`, so no project gets them until an operator attaches them.

The work is done by a durable job of the API (`platform.run/v1`). It holds a lease, writes its
intent before every effect on Proxmox or Kubernetes and the result after it, and each stage first
looks at what exists. A run therefore goes on after an API restart, and **Retry** continues from
what is there.

### This path or the installer

| | Console (this page) | Installer, `proxcloud.mode: split` ([install.md](install.md#split-mode-managed-kubernetes-and-databases)) |
|---|---|---|
| Starts from | A running ProxCloud with a saved Proxmox provider, for example the [Compose install](../deploy/compose/README.md) | A site inventory, a management host with SSH, a root-equivalent Proxmox credential in the vault |
| Root on Proxmox | Once, by hand, for the identities script. ProxCloud stores no root credential | Held by the installer for the whole install |
| Management cluster | The seed VM, built by the job | The mgmt k3s cluster the installer builds |
| Node images | The BOM's two images, imported through [template updates](template-updates.md) | Templates you build first (`images/k8s-node-template.sh`) |
| Tenant block volumes (proxmox-csi) | None on Compose: the API has no CSI gateway listener there | Through the CSI gateway |
| Database backups | None: the database provider is registered without a backup store | To the installer's object store |
| Sandboxes | No | Yes ([managed-sandbox.md](managed-sandbox.md)) |
| Lab evidence | None yet | See [install.md](install.md) and [managed-k8s.md](managed-k8s.md) for what ran |

Use the console path for a site that was not installed by the installer and wants tenant Kubernetes
and databases without a management host. Use the installer when you need block volumes for tenant
clusters, database backups or sandboxes, or when the site is already an installer site.

The two paths do not meet in this release. A site whose services cluster the installer built is not
shown or adopted by the page, and the installer does not write an installation record
([Not implemented yet](#not-implemented-yet)). Do not run both on one Proxmox cluster without
reading [section 3](#3-proxmox-identities): they share the pool `proxcloud-system` and the token ids
`capmox@pve!capi` and `csi@pve!csi`.

## 2. Prerequisites

| Needed | Detail |
|---|---|
| The switch | `PROXCLOUD_PLATFORM_ENABLED=true` and `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true` on the API, read at start. On Compose: `./proxcloud-up.sh install --managed-services`, which sets both ([Compose README](../deploy/compose/README.md#optional-features)). With the first off, the installation routes answer `404 platform_disabled` and the page shows only the command. With only the second off, the page and its settings work, the `flags` check fails and no run starts (`409 platform_disabled`). |
| An encryption key | `PROXCLOUD_ENCRYPTION_DEFAULT_KEY`. Without it the token secrets cannot be stored (`409 no_encryption_key`). The Compose install generates one. |
| A Proxmox provider | Saved, with its connection checks passing (Platform setup, steps 1 and 2). One installation per Proxmox provider; the page shows the installation of the first one. |
| A GitHub integration | System → Integrations. Template updates finds and downloads the two images through it. |
| Permissions | `admin:service:read` to read the page, `admin:service:manage` for every button, the Check and the seed kubeconfig download included. |
| A platform network | One bridge that exists on every node used, untagged (no VLAN), with a free IPv4 network from /8 to /29, a gateway and one to three DNS servers. It must not overlap the provider's tenant address pool (`cidrPool`), a sandbox network or a public network. |
| Routes | The API (on Compose: the API container) reaches the Proxmox API, the seed address on 6443 and the cluster's API address on 6443. The seed and the cluster machines reach the Proxmox API URL you enter, `github.com` and `raw.githubusercontent.com` (at first boot the seed downloads the k3s binary from the first and the k3s install script from the second, both checked by sha256; GitHub redirects a release download to another host of its own, which must be reachable too and is not named in the BOM) and the registries of the BOM (`ghcr.io`, which serves every chart: ours and the third-party ones of the chart mirror, `ghcr.io/mutex-solutions/charts-mirror`; and the image registries those charts pull from). Tenant networks must reach the load balancer range and nothing else of the platform network. |
| Storage | A boot store that takes VM disks on every node used; an ISO store that is local to each node and takes ISOs (default `local`); an import store with `import` content on every node an image goes to (default: the provider's `importStorage`). The import store must not be the boot store: the portal token holds `PVEDatastoreAdmin` on the import store, which reaches every disk on it ([section 10](#10-security-boundaries)). The API refuses settings that name the same store for both; the form says so before it saves when you type the same store in both fields. An empty import store takes the provider's `importStorage`, so it is refused too when that is the boot store. The import store of the settings and the provider's `importStorage` must be one store: the script of step 2 is written for the store of the settings, and the `privileges:kubemint` check reads the provider's ([when the provider's import store is the boot store](#when-the-providers-import-store-is-the-boot-store)). |
| Capacity | The smallest machine is 2 cores, 4 GiB of memory and a 20 GiB disk. The smallest layout (seed, control plane, one machine per group) is five VMs. The review step adds up cores and memory per node; it does not read what is free. |
| Root on one node | Once, to run the identities script. |
| The datacenter firewall | On, with the provider's isolation rules written by a provider sync once the installation is saved: the Check verifies that tenant traffic to the seed, the API address and the node range is dropped. |

The page also warns when the job scheduler is off: tenant clusters and databases then update their
status only when a provider sync runs. It does not block an install.

### What no check reads

The checks of step 3 do not see the following. Each one shows only in stage 4, 7 or 9, so look
before the first run:

- **The seed reaches the Proxmox API URL.** Until stage 7 only the ProxCloud host calls Proxmox.
  CAPMOX calls it from the seed, with the seed's address on the platform network as the source.
  With the datacenter firewall on, a node accepts port 8006 only from its `management` IP set
  (by default the node's own network): add the platform network to it, or enter a URL the platform
  network reaches. A URL that is not reached ends stage 7 with `capmox_auth_failed` after 10 minutes.
- **Memory on each node for CAPMOX's scheduler.** CAPMOX clones a machine only when its memory fits
  into the node's total memory minus the configured memory of every VM its token sees on that node,
  running or stopped (the seed and the platform's other machines). A machine that does not fit ends
  stage 7 with `machine_failed` and CAPMOX's text.
- **One layer-2 segment for the platform bridge across the nodes.** The cluster's API address is
  announced by ARP from the control plane's node. A bridge that is a separate NAT network on each
  node (no port between the nodes) leaves the machines of the other nodes unable to join.
- **Space on the boot store for full clones.** Every machine is a full raw clone of its disk size.
  A ZFS store without `sparse` reserves the whole size for each.
- **Disk latency.** The seed (k3s with embedded etcd) and the control plane write to etcd. An fsync
  that takes seconds makes k3s exit and start again (seen on a lab: writes of 6.2 and 6.9 s), and
  can cost the control plane its leader: with a write of 19.5 s kube-vip gave up the API address
  five times and the controllers lost their leases with it. Put the boot store on disks that sync
  in milliseconds (etcd asks for a 99th percentile under 10 ms). The load is highest in stage 7:
  CAPMOX clones the machines of all node groups at the same moment, as full clones on the store the
  seed's and the control plane's disks are on, and nothing in CAPMOX v0.9.2 or in the cluster chart
  spaces them out. A seed that restarts then is tried again by the run (see
  [How long-running states look](#how-long-running-states-look)); a store that cannot take the
  clones of every group at once next to a running etcd is too slow for this path. From cluster
  chart 0.1.12 kube-vip tries to renew its lease for 40 seconds before it gives up the API address
  (lease 60 s, renew deadline 40 s, retry 5 s). Its client cuts every request at 10 seconds, so
  the address is kept when one try in those 40 seconds gets through in under 10: a stall of about
  half a minute is survived, an etcd whose every write takes over 10 seconds is not. The BOM of
  this release names 0.1.12; chart 0.1.11 gave up after 10.
- **The MTU of the platform network.** Cilium tunnels pod traffic between the machines at the
  machines' MTU. When the bridge's real path MTU between two nodes is below 1500 (the bridge is
  itself carried by an overlay), set the settings' MTU to it.
- **The provider's region.** The Proxmox provider must name its region (`config.region`), or hold
  exactly one: the region becomes a label of every machine and the storage plugin's configuration.

Platform setup and the Dashboard show an optional step `managed` after Go live
(`GET /api/v1/admin/setup/status`). It only reports the installation's phase and links to the page;
Go live never waits for it.

## 3. Proxmox identities

The portal token cannot create users or roles or grant itself the rows below, and it must hold
nothing on the seed's pool; ProxCloud asks for no root password. Step 2 of the page shows a script to
run as root on one node:

```
GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage=<store>&isoStorage=<store>[&tokenId=<portal token>][&importStorage=<store>]
```

It is the [portal token script](proxmox-permissions.md#portal-token-script) followed by the
managed-services block. The page fills in the stores of the saved settings and the provider's token
id, and leaves `gpu` out. Because the provider is saved, the portal token exists already: the first
lines of the output report it as existing, which is expected. Do not remove it; the saved provider
uses its secret. Only the two secrets printed for `capmox@pve!capi` and `csi@pve!csi` are pasted in
step 2. The block:

- creates the roles `ProxCloudCAPI`, `ProxCloudCSI`, `ProxCloudKubeMint` and `ProxCloudKubeToken` when
  missing and appends their privileges (a re-run adds privileges and removes none, with one
  exception below);
- creates the pool `proxcloud-seed` (`proxcloud-system` and `pc-templates` come from the portal
  token part);
- creates the users `capmox@pve` and `csi@pve` and one privilege-separated token each,
  `capmox@pve!capi` and `csi@pve!csi`, and prints each secret once;
- creates the group `proxcloud-kube` and puts those two users in it: they are the only users the
  portal token may create and delete tokens for;
- writes the ACL rows of `pve.ManagedACLs`: the CAPI token on `/pool/proxcloud-system` and
  `/pool/proxcloud-seed` (not `/pool`), the CSI token on `/pool/proxcloud-system` and the boot store,
  `NoAccess` on `/pool/proxcloud-seed` for `csi@pve` and for the portal user and token, and
  `ProxCloudKubeToken` (`User.Modify`) for the portal user and token on
  `/access/groups/proxcloud-kube`, and `ProxCloudKubeMint` (`Permissions.Modify`) on `/storage`,
  `/sdn/zones` and `/nodes`, not on `/pool`. The full list and the reasons are in
  [proxmox-permissions.md](proxmox-permissions.md#managed-services);
- removes what an earlier version of this script wrote and this one no longer does: the
  `ProxCloudKubeMint` rows of the portal user and token on `/access/groups` and on `/pool`, and
  `User.Modify` in that role. On `/access/groups` that privilege let the portal token create an API
  token for every user of the cluster, `root@pam` included; on `/pool` the role let it write any
  role on every pool. Nothing else is removed, and on a new site these lines change nothing. When
  another portal token of the cluster still holds the role on `/access/groups`, the script leaves
  `User.Modify` in the role, so that token keeps working, and ends with `NOT FINISHED` and the
  list: run the script for each token named
  ([more than one portal token](proxmox-permissions.md#an-existing-site));
- ends with `pveum user token permissions` for both tokens on both platform pools.

**The token ids are fixed.** `PUT …/installations/{id}/credentials` refuses any id but
`capmox@pve!capi` and `csi@pve!csi`, and any secret that is not a UUID. The secrets are write-only:
they are sealed with the encryption key in the collection `platformSecret`, no route returns them,
and an installation shows only which ids are stored. To replace one, paste a new one.

**When `capmox@pve!capi` or `csi@pve!csi` already exists**, `pveum user token add` fails, the script prints
`The token … exists already: its secret was printed when it was created.` and goes on with the
ACL rows. Proxmox does not show a secret again. Either paste the secret you kept, or remove that
token (never the portal token) and run the script again:

```sh
pveum user token remove capmox@pve capi
pveum user token remove csi@pve csi
```

Removing a token ends its secret for everything that uses it. On a site where the installer's
services cluster uses the same two tokens, that cluster's CAPMOX and proxmox-csi stop working until
they get the new secrets. Paste the existing secrets instead where they are kept. On an installer
site that is `inventory/<site>/generated/token-capi` and `generated/token-csi`, in any mode, but
only for a token stage 10 created itself (the token did not exist when stage 10 first ran) or whose
secret is in the vault (`capi_token_secret`, `csi_token_secret`). Only `split` mode fails when the
two files are missing. A token that existed before the installer ran, or was made by hand or by
this script, is in no file: its secret is wherever the output was kept, and nowhere on Proxmox.

**Run the script again** after changing the boot store or the ISO store in the settings: the rows on
`/storage/<boot>` and `/storage/<iso>` are written for the stores in the query. The script does not
remove the rows of the old stores, nor any other row: one an older setup gave the two tokens on
`/pool` or `/storage` stays, and the `token-scope` check of step 3 fails on it until it is removed by
hand.

### When the provider's import store is the boot store

A provider set up with `importStorage` equal to the boot store of the settings (one store for
everything) cannot be installed as it is. With the import store left empty the save is refused,
naming a store you did not type. With another import store in the settings only, the save passes,
but the script writes `ProxCloudKubeMint` on that store alone, `privileges:kubemint` fails on
`/storage/<the provider's store>`, and running the script again does not change that. Do this
instead:

1. Pick a second store with `import` content on the seed's node and on every node of the node
   groups (`pvesm set <store> --content <its content>,import` adds it).
2. Set the provider's `importStorage` to that store (`config.importStorage`; **Import store** in the
   console's setup).
3. Leave the import store of the settings empty, or type the same store, and save.
4. Run the script of step 2 again. It now carries `importStorage=<that store>`.
5. Remove the rows the provider's first script wrote on the boot store. The script does not, and
   while they stay the portal token still holds `Datastore.Allocate` on every platform disk:

   ```sh
   pveum acl delete /storage/<boot> --roles PVEDatastoreAdmin --users proxcloud@pve --tokens 'proxcloud@pve!platform'
   pveum acl delete /storage/<boot> --roles ProxCloudKubeMint --users proxcloud@pve --tokens 'proxcloud@pve!platform'
   ```

   The second line is for a site where an earlier managed-services script ran with the boot store
   as `importStorage`. Use the provider's token id if it is not `proxcloud@pve!platform`.

A site with a boot store such as `local-lvm` and `local` for ISOs and imports is not affected: the
ISO store may be the import store.

The mint grant (`ProxCloudKubeToken` and `ProxCloudKubeMint`) gives the portal token more than it had: read
[the mint grant in M1](proxmox-permissions.md#the-mint-grant-in-m1) before running the script.

## 4. The setup steps

The page shows **Before you start** (the switch, the provider, template updates, the GitHub token,
the scheduler) and four numbered steps. Steps 2 to 4 open once the settings are saved.

### Step 1: Settings

Saving keeps a draft (`POST` or `PUT /installations`). Nothing is created on Proxmox until
**Install**.

| Field | Rule |
|---|---|
| Name | Lower-case letters, digits and dashes, at most 40. The two providers are named after it. |
| Proxmox API URL | `https://` with a host: the Proxmox API as machines on the platform network reach it. CAPMOX and the tenant clusters' providers use it. |
| Boot store, ISO store, import store | Storage ids. The ISO store defaults to `local`; an empty import store takes the provider's `importStorage`. The import store may not be the boot store; if the provider's `importStorage` is, an empty field is refused as well ([what to do](#when-the-providers-import-store-is-the-boot-store)). |
| Network | Bridge, IPv4 network (/8 to /29), gateway, one to three DNS servers, optional MTU (1280 to 9000). |
| Seed | Node, address, cores, memory and disk; up to 8 SSH public keys (optional, for emergencies: ProxCloud never uses SSH); 1 to 16 IPv4 networks that may reach the seed's API (`apiSources`), at least the address ProxCloud reaches it from. |
| API address | The cluster's API endpoint, held by kube-vip on the control plane machine. |
| Node range | `first-last`: the addresses Cluster API gives the machines. It must hold every machine, one more for the control plane's rolling update, and one more per group and node. |
| Load balancer range | `first-last`: the MetalLB pool tenants reach (tenant cluster APIs, database addresses). It must be outside the provider's blocked networks and the nodes' management networks. |
| Control plane | One machine: node and size. |
| Node groups | Exactly `system`, `kamaji` and `dbaas`. For each: 1 to 16 nodes, a count of at least one machine per chosen node (at most 64), and a size. One MachineDeployment is made per group and node. **Use the lab profile** sets 2 cores and 4 GiB everywhere. |

The gateway, the seed address, the API address, the node range and the load balancer range must not
share an address. The page checks these rules before saving; the API checks them again and adds what
only it can read (overlap with `cidrPool`, sandbox and public networks, and that no seed API source is
a tenant network).

Fixed after the first successful install (`409 fixed_fields`): the name, the network, the seed's node
and address, the API address and the set of node groups. Sizes, counts, the nodes of a group, the
ranges, the stores, the SSH keys and the API sources can change; **Apply this release** applies them.
The provider of an installation never changes. No route deletes a draft.

A save is refused during a run (`409 run_active`), and when someone else saved since the page read
the settings (`409 rev_conflict`: the page reads them again).

### Step 2: Proxmox identities

[Section 3](#3-proxmox-identities). Paste the two secrets and press **Store the secrets**.

### Step 3: Check

`POST …/preflight` reads Proxmox with the portal token and the two stored tokens and changes nothing.
**Install** runs the same checks as its first stage.

| Check | Fails when | What to do |
|---|---|---|
| `flags` | Template updates are off, or no GitHub integration is saved | `--managed-services`; save a GitHub token |
| `spec` | The saved settings break a rule, for example an import store that is the boot store | Correct step 1 and save |
| `network` | The platform network overlaps a tenant, sandbox or public network, a seed API source is a tenant network, the load balancer range is inside a blocked or management network, or a node did not report its networks | Correct the network in step 1 |
| `isolation` | The seed, the API address or part of the node range is in no blocked or management network, or the cluster firewall's isolation rules are not the ones a sync writes | Add the CIDRs the message names to **Networks tenants must not reach** on the provider's page (`blockedCidrs`). The message lists the smallest ranges that are missing; one network that covers them does the same (`10.90.0.0/26` for a seed on `.5`, an API address on `.10` and nodes on `.20` to `.60`), as long as the load balancer range stays outside it. Then **Run a provider sync, then check again**: with the installation saved (step 1), the sync writes the provider's rules whether or not it has a project. Check that the datacenter firewall is on. Five messages name a cause no sync of a provider without a project repairs: `cidrPool … overlaps the management network …` (choose another `cidrPool`), `rules … are for another pool`, `rules … drop … to …, which this provider's cidrPool, blockedCidrs and sandboxCidrs do not`, `rules … have an ACCEPT …` or `have no ACCEPT …` and `rules … are not this provider's set …, and a project's rules for cidrPool … are in place` (another portal or provider shares the rules: give this provider its own `poolPrefix`; the provider's own older rules: delete them under Datacenter > Firewall and sync). ([More](#the-isolation-check-and-the-provider-sync)) |
| `credentials` | A token secret is not stored | Paste it in step 2 |
| `privileges:capi`, `privileges:csi`, `privileges:kubemint` | The CAPI token, the CSI token or the portal token lacks a privilege on a path; the message names both | Run the script again. `privileges:kubemint` on `/storage/<store>`: that is the provider's `importStorage`; when the settings name another import store the script does not write that row ([what to do](#when-the-providers-import-store-is-the-boot-store)) |
| `mint-scope` | The portal token holds `User.Modify` or `Permissions.Modify` on `/`, `/access` or `/access/groups` (a stolen portal token can then create an API token for any user of the cluster, `root@pam` included) or `Permissions.Modify` on `/pool` (it can then write any role on every pool). A site set up before the group `proxcloud-kube` holds `ProxCloudKubeMint` on `/access/groups` and `/pool`. Or: `capmox@pve` or `csi@pve` is not in the group `proxcloud-kube`, and Proxmox refuses every token create or delete for it | Run the script again: it grants `ProxCloudKubeToken` on `/access/groups/proxcloud-kube`, puts both users in the group and removes the old rows ([the commands](proxmox-permissions.md#an-existing-site)). If it still fails on a privilege, another row grants it: `pveum acl list \| grep -F '<the portal user>'` on a node, then `pveum acl delete <path> --users <user> --roles <role>` (or `--tokens`) for a row on `/`, `/access`, `/access/groups` or `/pool` with a role other than `ProxCloudPlatform`. For a user outside the group the message gives the line: `pveum user modify <user> --groups proxcloud-kube --append 1` |
| `token-scope` | `capmox@pve!capi` or `csi@pve!csi` holds a privilege on `/pool`, `/storage` or `/vms` as a whole, which reaches every tenant's VMs or disks. The script writes no such row; an older setup may have, and the script removes none | Run the `pveum acl delete` command the message gives, as root on one node, then check again. When the portal token cannot read the row (one on `/`, for example) the message gives `pveum acl list` to find it: the row to remove is one on the named path itself, or on a parent of it (`/` included) with propagate 1, with a role other than `NoAccess`. The list is not empty on a site the script set up, and its rows on the platform pools and stores and on `/` without propagate stay. When no listed row is such a row, remove none: the token was made without privilege separation and holds its user's rights (the users keep `/pool` and `/storage`): run `pveum user token modify <user> <token name> --privsep 1` (`capmox@pve capi`, `csi@pve csi`), then the script again. Leave the tokens' rows on the platform pools and on the boot and ISO stores |
| `pool-fence` | The portal token holds a privilege on `proxcloud-system` or `proxcloud-seed`, or the CSI token on `proxcloud-seed` | Run the script again with the provider's token id: it writes the `NoAccess` rows on both pools, and a missing row is the usual cause (the portal token's `/pool` rows pass down to them). Only if the check still fails, remove the row granted to that user or token on `/pool/proxcloud-system` or `/pool/proxcloud-seed` itself. Do not remove its `/pool` rows: every tenant operation needs them |
| `iso-store-visibility` | The CAPI token sees an ISO store other than the one in the settings. CAPMOX puts each bootstrap ISO on the first ISO store its token sees | The message names the stores seen and, for each row of the token that reaches another ISO store, the command that removes it (`pveum acl delete <path> --tokens 'capmox@pve!capi' --roles <role>`, as root on one node); run them and check again, and `privileges:capi` must stay ok. A row the portal token cannot read is found with `pveum acl list \| grep -F 'capmox@pve!capi'`: it is on the other store's path or on a parent of it with propagate 1 (no listed row is such a row: the token lacks privilege separation, see `token-scope`). Or change the ISO store and run the script again. When the extra store is the boot store, the message gives no command: the token needs its row there, so name another boot store or take `iso` out of its content (the `boot-iso` row) |
| `boot-iso-grant` | `capmox@pve` may not upload and delete on the ISO store | Run the script again |
| `boot-iso` | The ISO store is shared, takes no ISOs, or is not the only ISO store of the boot storage | Name a node-local ISO store |
| `placement` | A node an image goes to lacks the import store with `import` content or the boot store | Change the nodes or the stores |
| `images` | A BOM image is on the template update blocklist | Install a newer ProxCloud release |

A check that cannot be read fails.

#### The isolation check and the provider sync

The rules are the provider's own: `DROP` from the tenant pool (`cidrPool`) to itself and to the
blocked and management networks, with the comment `proxcloud:isolation` (pool prefix `pc-`) or
`proxcloud:isolation/<poolPrefix>`. They do not depend on any project, and two things write them:
a provider sync (the scheduler's, **Run a provider sync, then check again**, or
`POST /api/v1/admin/job/external-service/sync`), and the first tenant network before it goes live.

A sync writes them without a project only for a provider that has a managed-services installation,
in any phase, the saved draft of step 1 and a `removed` installation included, and only with `PROXCLOUD_PLATFORM_ENABLED=true`.
For every other provider the sync does what it did before: a project's sync writes the rules, and
a provider with no project gets no firewall request at all. So the order is: save the installation
(step 1), run a provider sync, then check. Saving never waits for this check: step 1 reads the
provider's settings only, not the firewall.

A provider sync skips a provider whose status is `DISABLED`, in every part and without a log line,
so no sync writes or repairs its rules. The check's message then starts with `the provider is
disabled`: enable the provider under **Cloud providers**, run a provider sync, then check again.

That provider-level part of the sync, the one that needs no project, has no tenant waiting for it
and none of its own to protect. It only ever adds `DROP`s, and only to a set that no project's sync
maintains: it refuses in the cases below, logs
the reason as a warning on every cycle, and the check gives the same sentence. A sync that skipped
this part because the SDN lock was busy or could not be read says so in the log too and tries again
on the next cycle.

- **A node did not report its networks.** The missing node may be the one whose management network
  the pool overlaps, and the `DROP` to its network would be missing. Nothing is written until every
  node answers. A tenant's network create is not held up by this, as before.

- **`cidrPool` overlaps a management network of the nodes.** With no tenant yet those rules would
  only cut the nodes off each other. Choose another `cidrPool`. A project's sync and a network
  create still write the rules for such a pool, as before.
- **The rules in place are for another pool.** The rules carry the pool prefix only, so two portals
  with the same prefix (`pc-` is the default), or two providers of one portal on the same cluster,
  share one rule set, and whoever writes replaces the other's `DROP`s. A provider with no project
  therefore leaves rules for another pool alone: give it its own `poolPrefix`. The same message
  appears after a `cidrPool` change on a provider with no project. No sync applies the new pool
  then: delete the provider's rules (the ones with its comment) under Datacenter > Firewall and run
  a provider sync, or let its first tenant network rewrite them.
- **The rules in place drop something this provider's set does not.** Same pool, but a blocked
  network, a sandbox network or a management network in the rules is in none of this provider's
  `DROP`s (an equal or a wider network covers it). The other portal or provider with the same prefix
  may depend on that `DROP`, so nothing is removed and nothing is written: give this provider its
  own `poolPrefix`. A set that keeps every `DROP` in place and adds some is written, unless one of
  the next two cases applies. The same
  message appears when `blockedCidrs` or `sandboxCidrs` of a provider with no project was narrowed:
  the wider rules stay until they are deleted by hand as above, or until a project's sync rewrites
  them.
- **The rules in place accept other sandbox endpoints.** The `ACCEPT` rules for the sandbox
  endpoints (registry, build bucket, portal wake) carry the same comment and sit above the sandbox
  `DROP`s. When the ones in place are not exactly this provider's, nothing is written: removing one
  would cut the other portal's or provider's sandbox network off its endpoint, and adding one would
  open an address that its rules drop. Give this provider its own `poolPrefix`. The same message
  appears when the sandbox endpoints of a provider with no project changed, or when one of its
  `ACCEPT` rules was deleted by hand: delete the provider's rules as above and run a provider sync.
- **A project's rules for the pool are in place and the set is not this provider's.** A project's
  sync writes the provider-wide set on every cycle, whatever is in place. So while the cluster
  firewall holds a project's `ACCEPT` rules (`proxcloud:isolation:<project>`) with a source in this
  `cidrPool`, the provider-level part changes nothing, neither a wider set nor a missing or
  disabled rule. If the project is this provider's, its own sync has already written the set in
  the same cycle and this case does not show. If it is a project of another portal or provider
  with the same prefix, a write here would be undone at that project's next sync and done again,
  on every cycle, with the other's sandbox gate failing in between and its projects' `ACCEPT`s
  deleted and added again each time: give this provider its own `poolPrefix`. A project of another
  prefix on the same `cidrPool` gives the same message (an `ACCEPT` does not say whose project it
  is): delete this provider's rules as above and run a provider sync, which writes a missing set.
  Not seen: a portal or provider with the same prefix and pool whose projects have no network yet.
  The two then replace each other's set on every cycle until its first tenant network.

A provider that has projects still rewrites the rules from its projects' sync and at a network
create, whatever is in place, as before this check existed: two such providers with one prefix
replace each other's rules every cycle, and the check of the one that lost says so.

While no rule of this provider exists and no tenant network exists on the whole Proxmox cluster, the
check passes and says that the next provider sync or the first tenant network writes them. A vnet does not
say which portal made it. So on a cluster that a second portal shares, the other portal's tenant
networks end that case and the check fails with `0 in place, N wanted` until one provider sync has
run; the saved installation is enough for it, no project or tenant network is needed.

### Step 4: Review and install

Shows the release and its components (`GET /platform/bom`), what is created (one seed VM, one
control plane and the worker machines, the two providers), the cores and memory per node, and for an
installed platform the plan (`GET …/plan`). **Install** (later **Apply this release**) starts a run
(`POST …/runs {kind: apply}`). You can leave the page; the run goes on.

## 5. The stages of a run

One run per installation at a time. The page reads the run every 3 seconds.

| # | Stage (`id`) | What it does | Time limit |
|---|---|---|---|
| 1 | Check the settings and Proxmox (`validate`) | The checks of step 3, on the saved settings. | 5 min |
| 2 | Import the two images (`images`) | The seed image on the seed's node and the node image on the control plane's node and every node of a group, through template updates. An image whose template from an earlier import still exists on that node is not imported again; a template deleted by hand is imported again. An import that has just finished can show `checking`, with the VM it waits for, for a look or two: Proxmox lists a new template a moment after its task ends. That wait ends 5 minutes after the import's last step; a finished import whose VM is still no template in `pc-templates` then fails the stage. | No import moved for 60 min |
| 3 | Create the seed VM (`seed-vm`) | Takes a free VMID, clones the seed template into `proxcloud-seed` with the CAPI token, sets size and network (the template has no network device: Proxmox gives the new one its MAC address, and the CD's network config names that address), generates the seed's k3s server CA and tokens, uploads a NoCloud CD (`pcplat-<id>-…`) to the ISO store, puts it in place of the template's cloud-init drive and protects the VM, turns on the VM firewall (policy DROP; 6443, and 22 when SSH keys are set, from the API sources only) and starts the VM. | 30 min |
| 4 | Reach the seed (`seed-api`) | Waits until `https://<seed address>:6443/readyz` answers with a certificate of the CA made in stage 3. While it does not, the stage says whether the guest agent answers (`seed_not_booted`) or only the API does not (`seed_api_unreachable`). | 20 min |
| 5 | Secure the seed (`seed-identity`) | Reads the token of the service account `proxcloud-system/proxcloud-portal` with a bootstrap token, checks that it is cluster-admin, stores it encrypted, deletes the bootstrap token and its RBAC, then ejects the CD (the drive stays, empty: Proxmox removes no drive from a protected VM) and deletes it. The seed counts as ready from the moment its credential is stored; the stage is done only when the running seed's config no longer names the CD (an eject Proxmox took but left pending does not count, and the CD's file is kept until then) and no store lists it. | 10 min |
| 6 | Install the seed's components (`seed-platform`) | ArgoCD as a k3s HelmChart, the AppProject `proxcloud-platform` (BOM chart repositories only; the `default` project locked), then `cert-manager` and `capi` wave by wave. An Application counts when it is Synced and Healthy, compared against the chart version and values it has now, and its last sync operation ended `Succeeded`: ArgoCD shows Synced and Healthy from the resources alone while a hook of the sync still runs or fails. `capi` is done when, besides, the clusterctl inventory equals the BOM. The item of an Application names the resources and hooks its sync failed on, with their messages, and the stage's message repeats the first. Once an Application counts, its item has its state and no text: what ArgoCD still shows of an earlier attempt is left out. | 30 min |
| 7 | Build the platform cluster (`cluster`) | The CAPI token's Secret and the Application `pc-platform` on the seed; MachineDeployments of nodes no longer in a group are deleted; then waits for the control plane, the admin kubeconfig and every Machine Running. Before it writes anything it reads each node template: a template with a Proxmox cloud-init drive on another slot than `ide0` is refused (`machine_failed`, with the `qm set` command that removes the drive), because CAPMOX puts its own on `ide0` and a machine with both starts from the wrong one. A template whose config the portal token may not read is not checked: the stage goes on and shows an item `template-<vmid>` in state `not checked`. A template that passed has the same item in state `ok`; one that Proxmox no longer knows has none (CAPMOX reports it on the machine). The items list each machine with its VMID (`no VM` until CAPMOX has cloned it) and, while it is not Running, the step CAPMOX is at with its error (the reason and message of the ProxmoxMachine's condition `VirtualMachineProvisioned`). CAPMOX calls a VM provisioned once it runs and has its addresses; for a machine that then does not join (kubeadm, the API address, etcd) the item carries the reason and message of the Machine's own condition `Ready` after `Cluster API:`. The message of the waiting stage repeats, in this order, what the ProxmoxCluster reports, the KubeadmControlPlane's condition `Available` while it is not `True` (`Control plane:`), the machines' errors and, last, the state of the Application `pc-platform` while its sync is not done (not while it is only Progressing, which it is for as long as machines are built). On a **Retry** that finds a ProxmoxCluster still without a condition, the run updates an annotation on it (`proxcloud.io/retried-by`) once, so CAPMOX opens the Proxmox API again at once instead of at the end of its backoff of up to 17 minutes; the 10 minutes count from then. Steps CAPMOX tries again (`CloningFailed`, `PoweringOnFailed`) are waited for. `VMProvisionFailed` and `TaskFailed` end the stage: CAPMOX does not go on with such a machine. | 60 min |
| 8 | Reach the platform cluster (`workload-access`) | Creates the seed ArgoCD's service account on the cluster and a token for it (one year, renewed by any later apply when under 90 days are left), kept only in the seed's cluster Secret; writes the CSI token's Secret for proxmox-csi. | 10 min |
| 9 | Install the platform components (`workload-platform`) | The cluster's components wave by wave, each wave only when the one before is Synced and Healthy with its sync `Succeeded` (as in stage 6). After the first wave (Cilium) every node must be Ready with its group label. The platform's own volumes are recorded for Remove at every look of the stage, so also when it fails inside `kamaji-etcd`'s wave. | 20 min per wave |
| 10 | Create the two providers (`register`) | Creates the Kubernetes and Databases providers, or updates the ones this installation created before, with a kubeconfig of the cluster's `proxcloud-system/proxcloud` service account. | 5 min |
| 11 | Finish (`done`) | Records the components and the BOM as installed. The phase becomes `ready`. | 5 min |

A Remove has three stages: Delete the platform cluster (`remove-cluster`, 45 min), Delete the seed
(`remove-seed`, 30 min), Forget the stored secrets (`remove-secrets`, 5 min).

### How long-running states look

- **Running**: the stage is at work. Stages 2, 6, 7 and 9 show a progress line or items (images,
  Applications, machines) that change as they move.
- **Waiting**, shown with **Trying again. It did not work yet**: the stage looked, found its
  condition not met or met a passing error, and gave the job back. It looks again after 15 seconds;
  after a passing error (Proxmox or a cluster unreachable, a 5xx, or a cluster's API that refuses
  the run's credential, see below) the pause doubles up
  to 5 minutes and the retry counter goes up. The row shows the code the stage would fail with, the message, what
  you can do meanwhile and `must finish by`. This is normal during stages 2, 4, 7 and 9: an import,
  a boot, a kubeadm join and a chart sync take minutes.
- A stage fails when its time limit passes (with the code it was waiting on, or `<stage>_timeout`
  when it had none), after 40 passing errors, or at once on a refusal: a Proxmox 4xx other than 429, a
  Kubernetes 4xx other than 401, 403, 409 and 429 (for 401 and 403 see below), a failed Proxmox task, a
  machine CAPMOX gave up, a sync that failed again after the run had started it.
- **A cluster's API that has just started** (the seed's k3s after a restart, the platform cluster's
  API server) refuses a good credential for some seconds, until it has read its Secrets and RBAC
  objects: with a `401`, which is all an API server says of a token it cannot check; with a `403`
  of the RBAC authorizer, with or without its list of roles it does not find (`RBAC: [clusterrole…
  "cluster-admin" not found, …]`); with a `403 … not yet ready to handle request` of an admission
  plugin. No text tells these from a token that was revoked, a role or binding that was deleted, a
  quota or a namespace that is being deleted, and the server's `/readyz` answers a token it does
  not know with the same `401`. So every Kubernetes `401` and `403`, from the seed and from the
  platform cluster, is first a passing error: the message starts with "The cluster's API refuses
  the run's credential, as an API server does for some seconds after it starts" and carries the
  server's text. When five looks on end are refused (3 minutes 45 seconds or more, with the pauses
  above) the refusal is taken as real: the stage fails with its code (`seed_api_unreachable`,
  `workload_api_unreachable`, `component_unhealthy`, …) and "The cluster's API refused the run's
  credential at 5 looks on end:" with the server's text. A look that ends another way starts the
  count again. In stage 5 the same holds for the bootstrap token (`401`); a service account the
  seed does not report as cluster-admin is waited for until the stage's 10 minutes end.
- **Skipped**: the stage had nothing to do (for example `remove-cluster` when no seed ever became
  ready).
- The installation's phase is `applying` or `removing` during a run, then `ready`, `removed` or
  `failed`. A ready installation is shown as **Needs attention** (`degraded`, computed when the page
  loads, never stored) when the seed does not answer, lists no Application, or one is not Synced and
  Healthy.
- If the API stops, the next start resumes the run. If the job runs out of its 20 attempts (a
  crash during a run or a database error spends one; waits spend none), the run reads `failed` with `job_dead` and the stored phase stays
  `applying` or `removing` until **Retry** or **Remove** settles it; settings cannot be saved until
  then.

Messages are fixed text plus Proxmox, Cluster API or ArgoCD text clipped to 400 characters, with
every secret the run holds replaced by `[redacted]`.

## 6. Failure codes

The failed stage shows a code, a message and what to do. The same is shown, without alarm, while a
stage is still trying.

| Code | Stage | Cause | Operator action |
|---|---|---|---|
| `preflight_failed` | 1, or before it | One or more checks failed (the items list them); or the installation's Proxmox provider is gone or its connection is not valid | Fix what the failed checks name ([step 3](#step-3-check)), press **Check**, then **Retry** |
| `isolation_not_enforced` | 1 | `isolation` is the only failed check | Add the named CIDRs to the provider's blocked networks, run a provider sync, check that the datacenter firewall is on, **Retry** |
| `credentials_missing` | 3 to 5, 7, 8, Remove | A token secret is not stored (the message names which) | Paste it in step 2, then **Retry** or **Remove again** |
| `image_import_failed` | 2 | Proxmox could not be read; a node lacks the import or boot store; an import failed or was refused. Also: an import is finished but the VM at its VMID is not a template in `pc-templates` on its node (moved to another pool or node, or out of the portal token's view), so the stage can neither use it nor call it deleted; the message names the VM | Open Template updates to see the import, then **Retry**. If it keeps failing, install a newer release. For a template that was moved: put it back in `pc-templates` on its node, or delete it so that it is imported again, then **Retry** |
| `image_import_stalled` | 2 | No import moved for an hour | Open Template updates to see the import, then **Retry**. If it keeps failing, install a newer release |
| `image_blocked` | 2 | A BOM image is on the template update blocklist | Install a newer ProxCloud release |
| `seed_clone_failed` | 3, Remove | The clone task failed or was refused; stage 2 recorded no seed template; the VM stays locked. Also, with nothing being cloned: Proxmox could not be read with the CAPI token while looking at the seed VM or the pool `proxcloud-seed` (unreachable, or the token is refused), in stage 3 on a ready seed or in `remove-cluster` and `remove-seed` | Fix the storage or the node the message names, **Retry**. For a read that failed: check that `capmox@pve!capi` still exists and the stored secret is current, then **Retry** or **Remove again** |
| `seed_config_refused` | 3, Remove | Proxmox refused a config change, the read of the VM's config after it, the CD upload, a firewall rule or the start (the message carries Proxmox's own error); the CD could not be rendered or built (`The seed's CD could not be rendered` or `… built`: for example Proxmox reports the seed's `net0` without a MAC address); at Remove, the VM is locked or carries another installation's tag | Fix what Proxmox's error names: the bridge, the boot or ISO store, the node, or a right of `capmox@pve!capi` (then run the script again), and **Retry**; it builds a new seed VM. A CD that could not be rendered or built is a defect, not a setting: nothing was stored and no CD was uploaded, Retry will not pass, report the message. A VM with another installation's tag is not deleted: look at it in Proxmox |
| `seed_not_booted` | 4 | The seed's guest agent did not answer in time: the VM did not boot, or its first boot failed (for example the k3s binary or its install script could not be downloaded, or one failed its checksum) | Open the seed VM's console in Proxmox. **Retry** builds a new seed |
| `seed_api_unreachable` | 4, 6 to 10, Remove, seed access | The seed is booted but its API does not answer from the API's host; or a seed call failed later (stages 8 to 10 read the cluster's admin kubeconfig from the seed); or the seed's API refused the stored credential or the request with a `401` or a `403` at five looks on end (a seed that restarts answers so for some seconds, with `Unauthorized`, `RBAC: … not found`, a plain `forbidden` or `not yet ready to handle request`, which the run waits out) | Add a route from the ProxCloud host to the platform network, or correct the seed API sources and save. **Retry** writes the seed's firewall rules again first. For a credential the seed keeps refusing: look on the seed VM's console whether k3s runs or keeps restarting (`systemctl status k3s`; a slow disk does that, see [What no check reads](#what-no-check-reads)), then **Retry** |
| `seed_tls_mismatch` | 4 | The server on the seed address presents a certificate of another CA | Another machine holds that address. Correct the seed address, **Retry** |
| `seed_identity_lost` | 4, 5, later | The run holds no credential for the seed; the seed refused the bootstrap token at five looks on end (it expires 3 hours after the seed was built); the seed did not report the service account as cluster-admin to the end of stage 5's time limit | **Retry**. It builds the seed again when it never became ready |
| `seed_cd_not_deleted` | 5, Remove | The CD could not be ejected or deleted, or Proxmox still lists it; or Proxmox took the eject but left it pending on the running seed (the message says so) | The CAPI token cannot delete on the ISO store: run the script again, **Retry**. For a pending eject: `qm pending <vmid>` on the seed's node shows it; stopping and starting the seed VM applies it, then **Retry** |
| `seed_lost` | 3, Remove | A seed that had become ready is gone, or the VM at its VMID is no longer this installation's. It is not rebuilt: a new, empty seed would have no record of the cluster's machines | Remove with the option for a missing seed, then install again ([Remove](#remove)) |
| `component_unhealthy` | 6 to 9 | An Application is not Synced and Healthy in time (the message names it and its condition); or something could not be rendered or applied, in stage 7 the Application `pc-platform` | Download the seed kubeconfig to look closer, then **Retry**. ArgoCD does not sync an Application again by itself once its retries are spent; the run starts the sync of one whose last sync ended `Failed` or `Error`, once per run, so no manual sync is needed after the cause is fixed. When that sync ends `Failed` or `Error` too, the stage fails at once with what the sync failed on (the hook or resource and its message) instead of waiting for its time limit; **Retry** starts the sync once more. ArgoCD takes about 40 minutes to spend the ten retries of one sync, longer than the time limit of stage 6 or of a wave, so the stage does not wait for that either: once one sync has failed four times (about 8 minutes of ArgoCD's backoff), the last time in this run, and ArgoCD shows the hook or resource it failed on, the stage fails with that text while ArgoCD still tries. After a **Retry** the same sync gets one more attempt before the stage fails on it again |
| `capi_init_failed` | 6, 9 | clusterctl did not install the providers of the BOM; or a provider is installed at another version than the BOM names (provider upgrades are not in this release) | The same |
| `machine_failed` | 7 | CAPMOX gave a machine up: its ProxmoxMachine's condition `VirtualMachineProvisioned` is `False` with `VMProvisionFailed` (not enough memory on the node for its scheduler, no free VMID, the template not found, a VMID held by another VM) or `TaskFailed` (a Proxmox task failed: a clone, a resize, a config change). The message has the machine's name, its VMID and CAPMOX's text. Also: a node template has a Proxmox cloud-init drive on another slot than `ide0` (the message has the command), or its config could not be read for another reason than a missing permission | Correct what CAPMOX's text names. CAPMOX does not try that machine again, and **Retry** alone finds it as it was: delete the Machine on the seed with the seed kubeconfig (`kubectl -n pc-platform delete machine <name>`, the command is in the message), Cluster API makes a new one, then **Retry**. Do not delete the Machine when the text says the VM's name does not match (`expected VM name to match`): the VMID in the ProxmoxMachine then belongs to another VM, and CAPMOX deletes the VM at that VMID when its token may. The message then has no delete command and says so: look in Proxmox whose VM it is, and delete the Machine only once its ProxmoxMachine no longer names that VMID. For the template: run the `qm set` command of the message on that node, then **Retry** |
| `capmox_auth_failed` | 7 | Proxmox refuses the CAPI token when CAPMOX uses it (a condition of the ProxmoxCluster or a ProxmoxMachine says so); or CAPMOX has set no condition on the ProxmoxCluster for 10 minutes, which is how a Proxmox API URL the seed cannot reach and a refused token both show, because CAPMOX opens the API before it writes anything | Check the Proxmox API URL as the platform network reaches it ([What no check reads](#what-no-check-reads)), paste the secret again, **Retry**. The Retry makes CAPMOX try at once (it updates the ProxmoxCluster), so a fix shows within its 10 minutes. CAPMOX's log on the seed says which it is: `kubectl -n capmox-system logs deploy/capmox-controller-manager` |
| `control_plane_timeout` | 7 | Not every machine is Running or the control plane is not initialized in 60 minutes. The items and the message carry the step each machine is at and CAPMOX's last error | **Retry**; the time limit starts again. Look at the machines with the seed kubeconfig |
| `workload_api_unreachable` | 8 to 10, Remove | The cluster's API does not answer from the API's host, or its kubeconfig or CA on the seed is not usable, or it refused the admin kubeconfig or the request with a `401` or `403` at five looks on end (as for `seed_api_unreachable`) | Add a route from the ProxCloud host to the API address, **Retry**. At Remove: restore the route and **Remove again**, or tick the option for a cluster that does not answer |
| `nodes_timeout` | 9 | Not every node is Ready with its group label after the first wave | **Retry** |
| `provider_conflict` | 10 | A provider with the same name exists that is not this installation's, two rows carry its marker, or the provider validation refused the row | Correct or rename that provider under Cloud providers, **Retry** |
| `bom_unsupported` | start, plan | The installed BOM is not the one this release carries | No release upgrades between BOMs ([Not implemented yet](#not-implemented-yet)). Go back to the ProxCloud release that installed the platform. Or, if no tenant cluster or database must be kept: delete them all (Remove refuses with `tenants_exist` otherwise), remove managed services, which deletes the platform cluster, and install again |
| `portal_changed` | any | ProxCloud was upgraded during the run and no longer carries the run's BOM. The stage the run stood at reads `failed` with this code | **Retry** |
| `job_dead` | any | The run's job ran out of attempts | **Retry**, or **Remove again** |
| `platform_disabled` | start | The switch, or template updates, is off | `--managed-services` |
| `stage_unavailable` | any | A run recorded by another release names a stage this one does not have | No action in this release |
| `tenants_exist` | Remove | Tenant clusters or databases still run on the platform (the message names up to five) | Delete them, **Remove again** |
| `machine_delete_stuck` | Remove | Cluster API is still deleting machines after 45 minutes, or (missing seed) a tagged VM could not be stopped or deleted | Delete the VM the message names in Proxmox, **Remove again** |
| `volume_delete_failed` | Remove | A recorded volume could not be deleted with the CSI token | Check `csi@pve!csi` and the storage the message names, **Remove again** |
| `<stage>_timeout` | any | A stage's time limit passed while it had stored no code, for example `validate_timeout` | **Retry** |

A failed permission, network or ISO store check has no code of its own: it ends stage 1 as
`preflight_failed` with the check among its items.

## 7. Retry, cancel and Remove

**Retry** starts a new run of the saved settings (save a change first). Every stage looks at what
exists, so finished work is not repeated and the time limits start again. Nothing is deleted when an
apply fails, with one exception: a seed that never became ready (its credential was never stored in
stage 5) is deleted and built again as the next generation, with a new CA and a new CD. A seed that
became ready is never deleted or rebuilt by an apply: a stage 5 that failed after storing the
credential, for example with `seed_cd_not_deleted`, keeps the seed, and Retry goes on with the
bootstrap objects and the CD.

**Cancel the run** sets a flag the runner reads at its next claim and before every write: usually
within about 15 seconds, and up to 5 minutes while a stage pauses after repeated errors. A Proxmox
task that was started finishes first. The run ends `cancelled` and the installation `failed`.
Cancelling an apply deletes nothing. Cancelling a Remove does not bring back what it deleted.

### Remove

**Remove managed services** asks you to type `remove <name>`. It is not offered for a draft, for a
removed installation or during a run. The typed text is the page's own safeguard: the page sends
`{"kind":"remove"}` to `POST …/installations/{id}/runs`, and that is the whole body of a plain
Remove for an API caller too. The API field `confirm` is not a confirmation of every Remove: it
is the option described below, and a body that carries it asks for that option.
The run:

1. refuses with `tenants_exist` while the cluster's ArgoCD holds an Application labelled
   `app.kubernetes.io/managed-by=proxcloud` (a tenant cluster or database). A cluster that never
   could hold one is not asked for long: when its control plane never came up, an API that does
   not answer is neither retried nor needs the option below. That no provider is recorded is not
   taken for that: stage 10 records them only after both are registered, and the first is public
   as soon as its row exists;
2. sets the installation's providers `DISABLED` (the rows stay) and notes on each row the status it
   had (`config.platformStatusBeforeRemove`). The next install gives a row with that note its
   status back in stage 10. A provider you disabled yourself has no note and stays disabled;
3. deletes the Application `pc-platform` on the seed without a cascade, then the Cluster API
   Cluster, and waits until Cluster API has deleted every machine. When the seed showed no Cluster
   before the Application was deleted, the run looks once more before it goes on: a sync that was
   running may have created one in between;
4. deletes the platform's own volumes with the CSI token, by the exact handles recorded in stage 9
   (read again from the cluster when it answers);
5. deletes the seed VM (only when its VMID, pool and name are this installation's and no tag names
   another one) and every `pcplat-<id>-*` CD;
6. wipes the stored secrets and the recorded status.

Afterwards the page prints the two `pveum user token remove` commands; ending the tokens is yours to
do. Remove leaves in place: the tokens, users, roles, ACL rows and pools of the script; the two
provider rows, disabled; the image templates in `pc-templates`; and the installation record in phase
`removed`, from which the same steps install again (both secrets must be pasted again).

A Remove deletes no rule of the cluster firewall. The provider's `proxcloud:isolation` or
`proxcloud:isolation/<poolPrefix>` rules stay, and because the installation record stays, the
provider sync keeps maintaining them without a project, as before the Remove
([The isolation check and the provider sync](#the-isolation-check-and-the-provider-sync)); no
setting returns the provider to the behaviour of one without an installation. If you then take
the platform's addresses out of `blockedCidrs` on a provider with no project, the sync removes no
`DROP` and logs a warning on every cycle: delete the provider's rules under Datacenter > Firewall
and run a provider sync, which writes the narrower set.

A Remove that fails or is cancelled keeps what it deleted; **Remove again** goes on from there.

The stages of a Remove list what happened, and the list stays on the finished run:

| Stage | Item | States |
|---|---|---|
| `remove-cluster` | `tenants` | `none`: the cluster was asked and holds no tenant cluster or database. `not checked`: its control plane never came up, it did not answer and the option let the run go on, or the seed VM is gone |
| | one per machine, by its name | `deleting` while Cluster API deletes it, then `deleted`, with its VMID (`no VM` when CAPMOX had not cloned it). Cluster API deletes the VM, its disk and its bootstrap ISO; the run does not read Proxmox to confirm it |
| | `cluster` | `gone`: the seed lists no cluster and no machine any more. Also when the seed VM is gone and the installation records no cluster and no volume: the seed was not asked, and the item says that an earlier Remove deleted them or none was built |
| | `volume-<name>` | `deleted`, with the volume's handle, component and claim; `not found` when the store no longer listed a recorded volume |
| | `volumes` | `none recorded`: stage 9 never recorded a volume, so none was deleted. `not checked`: see the option |
| | `vm-<vmid>` (seed gone) | `deleted`: a tagged VM the run deleted itself. `left`: a VM of the pool it did not delete |
| `remove-seed` | `vm-<vmid>` | `gone`: Proxmox no longer has the seed VM; it was deleted with its disk and firewall rules, by this run or an earlier Remove. `left`: the VM at that VMID is not this installation's seed |
| | `cds` | `gone`: no `pcplat-<id>-*` CD is on the ISO store |
| `remove-secrets` | `secrets` | `deleted`: the stored token secrets, the seed's credential and the recorded status |

**The option "Go on even when the seed VM is gone or the platform cluster does not answer"** sends
the typed text to the API as well: `{"kind":"remove","confirm":"remove <name>"}` (another text is
refused with `400 invalid_confirmation`). It changes two cases:

- **The seed VM is gone** (or the VM at its VMID is no longer this installation's). Without the
  option the run fails `seed_lost`. With it, Cluster API cannot be asked, so the run deletes, with
  the CAPI token, every VM of pool `proxcloud-system` that carries the tag `pc-platform-<id>`, each
  after its bootstrap ISO (`user-data-<vmid>.iso` on the ISO store, which holds the machine's
  kubeadm data), then the recorded volumes. CAPMOX tags a VM only after its clone: a machine that
  was still being cloned when the seed was lost has no tag and is **not** deleted. The stage lists
  such VMs (named `pc-platform-…`, in the pool, with no installation's tag) as items in state
  `left`; delete them in Proxmox.
- **The cluster does not answer** whether tenants still run on it. Without the option the run
  retries, then fails `workload_api_unreachable`. With it, the run goes on without the check. It
  then cannot read the platform's volumes again either and deletes the recorded ones; when none
  was recorded, the stage shows an item `volumes` in state `not checked`: look on the boot store
  for `vm-*-pvc-*` disks of the platform's own components.

In both cases ProxCloud cannot see tenant clusters or databases on the platform: any that exist lose
their control planes and volumes with the machines. Tenant worker VMs live in the projects' pools
and are not deleted by this; delete those clusters from the projects first where you can.

## 8. Credentials and the seed backup

| Credential | Where it is | Rotation |
|---|---|---|
| `capmox@pve!capi`, `csi@pve!csi` | Encrypted in `platformSecret`; the first also in a Secret on the seed, the second in a Secret on the cluster | Create a new token secret in Proxmox (remove the token, run the script again), paste it in step 2, then **Apply this release**: stages 7 and 8 write the Secrets again. Not verified on a lab |
| The seed credential (a service account token, cluster-admin on the seed) | Encrypted in `platformSecret` | **Replace the seed credential**: creates the next token Secret (`proxcloud-portal-token-g<n>`), checks it, stores it, then deletes the older ones, which ends their tokens. Refused during a run. If it stops halfway, press it again; the next apply also removes token Secrets nothing stores |
| The cluster's admin kubeconfig | On the seed only (Cluster API keeps it). A run reads it into memory | Not stored by ProxCloud |
| The seed ArgoCD's token for the cluster | In the seed's cluster Secret only; valid one year | Renewed by any apply when under 90 days are left. The page shows the expiry and warns under 30 days |
| The providers' kubeconfig (service account `proxcloud` on the cluster) | In the two provider rows' encrypted secret | Written again by every apply |
| The seed's CA key, k3s token, bootstrap token | On the CD until stage 5 deletes it; the CA key is never stored by ProxCloud, the bootstrap token expires after 3 hours | None |

**Break-glass kubeconfig.** **Download seed kubeconfig** (`GET …/seed-kubeconfig`) asks the seed for
a one-hour token of the portal's service account and wraps it in a kubeconfig: full access to the
seed, and through the Secrets on it to the cluster's admin kubeconfig. The page saves it as a file.
The route itself answers JSON, `{"data": {"kubeconfig": "<the file's text>", "expiresAt": "<time>"}}`,
not the file: a script takes `.data.kubeconfig` (`jq -r .data.kubeconfig > seed.kubeconfig`). It needs
`admin:service:manage`, is written to the audit log as `BREAK_GLASS` (who, which installation, the
expiry; never the token) before it is handed out, and is refused when the audit log is not
available. Nothing in ProxCloud ends such a token before it expires: **Replace the seed credential**
deletes the stored credential's token Secrets, not a token issued this way. Treat the file as a root
credential for one hour.

**Seed backup.** The seed holds what cannot be rebuilt from ProxCloud's database: the Cluster API
state of the cluster's machines and the cluster's CA and admin kubeconfig. Nothing backs it up for
you, and the page cannot see whether a job exists. It prints this command for the ready seed; run it
as root on one node with a backup store of your own:

```sh
pvesh create /cluster/backup --vmid <seed VMID> --storage <backup store> --schedule daily --mode snapshot
```

The backup contains the seed's cluster-admin token and the secret of `capmox@pve!capi`. Choose a
backup store that is not the boot, ISO or import store of the settings, and on which neither the
portal token nor any `csi@pve` or `capmox@pve` token holds `Datastore.Allocate`. Check each with
`pveum user token permissions <user> <token name> --path /storage/<backup store>`. Nothing in
ProxCloud checks the store you choose. On a site whose only backup store is `local` while the ISO or
import store is `local` too, the backup is within reach of those tokens: `capmox@pve!capi` holds
`PVEDatastoreAdmin` on the ISO store and the portal token on the import store. That is the limit ADR
0009 A2 accepts for this milestone.

A restore from such a backup has not been run. Without a backup, a lost seed means Remove with the
option for a missing seed and a new install, which deletes the cluster's machines and volumes.

## 9. Limits on the Compose install

- **No block volumes for tenant Kubernetes clusters.** They come through the CSI gateway, a listener
  of the API that the Compose stack does not run. The script therefore writes no rows for a gateway
  token. Tenant clusters get no proxmox-csi StorageClass; the Kubernetes provider is registered with
  `rwxGiB: 0`, so no NAS dataset either.
- **No database backups.** The Databases provider is registered without a backup store, and with the
  egress fence on (`egressFence: true`), which allows no object store.
- **Database replicas.** PostgreSQL 17 and 18; three replicas are offered only when the `dbaas`
  group spans three or more Proxmox nodes, otherwise one.
- **No node image for tenants yet.** The Kubernetes provider is registered with no versions. Publish
  a Kubernetes node image in Template updates and enter its version on the provider before tenants
  can create a cluster.
- **The API container must route to the platform network** (the seed and the API address, 6443).
  Nothing checks this before stage 4.
- **One host.** The job and its state live in the one API container and its PostgreSQL. While the
  host is down, tenant clusters and databases keep running; nothing reconciles the platform's
  record.

The page shows the first two and the single shared cluster under **What this release does not do**.

## 10. Security boundaries

In short; the full account is in [proxmox-permissions.md](proxmox-permissions.md#managed-services)
and ADR 0009, A2 and A8.

- The portal token holds `NoAccess` on `proxcloud-system` and `proxcloud-seed`: with its own token
  ProxCloud cannot read or change the configuration of a platform VM. This fences the VM, not its
  disks: when the import store is also the boot store, the portal token holds `Datastore.Allocate`
  there (`PVEDatastoreAdmin`) and can reach every platform disk on it, the seed's included. The API
  therefore refuses settings whose import store is the boot store (the `spec` check and stage 1
  repeat the rule). It compares the two names in the settings only: it does not read the portal
  token's rows on the boot store, so a `PVEDatastoreAdmin` row already there (written for a
  provider whose `importStorage` was or is the boot store) is not caught and stays until it is
  removed by hand
  ([step 2](#when-the-providers-import-store-is-the-boot-store)).
- The job uses the CAPI token for the platform VMs. That token holds `ProxCloudCAPI` on `/` (that
  path only), on `/nodes`, on `/sdn/zones` (`SDN.Use` on every zone and vnet, tenant ones included),
  on those two pools and on the boot store; `PVETemplateUser` on `pc-templates`; and
  `PVEDatastoreAdmin` on the ISO store, which reaches every volume on it.
- The preflight reads, with each of the two tokens, that it holds nothing on `/pool`, `/storage` and
  `/vms` as a whole (`token-scope`), that the CAPI token sees one ISO store (`iso-store-visibility`)
  and that the CSI token holds nothing on the seed's pool (`pool-fence`). It does not compare the
  tokens' rows one by one with the script's: a row on a single tenant pool, on a single store that
  takes no ISOs, or on another path (`/access`, `/mapping`, `/sdn` outside the zones) is not reported.
  `pveum acl list` on a node shows them all.
- The seed's API is behind its VM firewall (the API sources only) and the datacenter isolation rules
  (tenant networks cannot reach the seed, the API address or the node range). ProxCloud talks to it
  only over TLS pinned to the CA it made for that seed.
- Tenant control planes run in `pc-*` namespaces placed on the `kamaji` group, tenant databases in
  `pc-db-*` namespaces on the `dbaas` group, by the namespace annotations of the
  `PodNodeSelector` and `PodTolerationRestriction` admission plugins. Database pods reach only their
  own namespace, DNS and the API server.
- Both ArgoCDs have their `default` project locked; the seed's project may source only the BOM's
  chart repositories.
- The portal token creates and deletes API tokens for `capmox@pve` and `csi@pve` only
  (`ProxCloudKubeToken`, `User.Modify` on `/access/groups/proxcloud-kube`). A site set up before
  that group holds the grant on `/access/groups`, where the lab measured that the portal token can
  create a token for `root@pam` (2026-11-16, PVE 9.2.26); the `mint-scope` check fails there until
  the script has run again
  ([the mint grant](proxmox-permissions.md#the-mint-grant-in-m1)). The nodes need
  `libpve-access-control` 9.2.1 or later: before it the same grant also allowed a token without
  privilege separation for those two users, and a new secret for their existing tokens.
- The portal token holds no `Permissions.Modify` on `/pool`: it writes a cluster token's row on a
  tenant pool with its own `ProxCloudPlatform` rights, cannot write on `/pool` itself or on the
  platform pools, and cannot hand out a privilege it lacks. A site set up before 2026-11-16 holds
  `ProxCloudKubeMint` on `/pool` until the script has run again (`mint-scope`).
- **Accepted for this milestone, not closed:** `ProxCloudKubeMint` (`Permissions.Modify` on
  `/storage`, `/sdn/zones` and `/nodes`, and on the import store's own path) lets a stolen portal
  token grant any role on those paths, to itself or to any other user or token, which reaches
  the seed's disk through the boot store; with `ProxCloudKubeToken` it can delete or expire
  the tokens of those two users and disable the users; the CSI token's
  `Datastore.Allocate` on the boot store reaches every disk on it, the seed's included, and so does
  the portal token's when it holds `PVEDatastoreAdmin` on the boot store (the settings may not name
  the boot store as the import store; a row made for the provider's `importStorage` is not read); the providers' service account
  reads every Secret on the cluster, the CSI token's among them; CAPMOX and proxmox-csi reach the
  Proxmox API without verifying its certificate; ArgoCD pulls charts by version, and their digests
  are compared in CI, not at pull time.

## Not implemented yet

- **No released version.** The BOM is `0.8.0-dev`. `--managed-services` needs v0.8.0, which is not
  tagged, so no released image reads `PROXCLOUD_PLATFORM_ENABLED`. For a test before the release,
  `proxcloud-up.sh` takes a build of main as its version (`--version dev-<sha>`, `upgrade dev-<sha>`;
  CI publishes one per push to main) and prints that it is not for production
  ([deploy/compose/README.md](../deploy/compose/README.md)).
- **The third-party charts come from our mirror, and ArgoCD pulls them by version.** Every chart of the
  BOM has an exact version and a digest; the nine third-party ones are served by
  `ghcr.io/mutex-solutions/charts-mirror` ([ci-release.md](ci-release.md), "Chart mirror"), `kamaji`
  and `kamaji-crds` as version `26.11.2-edge`, packaged from the upstream tag of that name. CI
  compares the digests; ArgoCD does not, so a version replaced in the mirror between two checks
  would be installed.
- **No upgrade between BOMs.** A release carries one BOM. An apply on an installed platform runs
  with the installed BOM, which must be the one the release carries: it repairs, applies changed
  settings and renews the seed ArgoCD's token. After a ProxCloud upgrade that changes the BOM,
  `POST …/runs` answers `409 bom_unsupported` and the plan is blocked, until a release carries the
  previous BOMs. Provider (Cluster API) upgrades are refused as `capi_init_failed`.
  Two consequences to know before an install is finished on a build that is not a release:
  - The installed BOM is recorded by the sha256 of the BOM file as that build carries it, in
    stage 11 only. Any later change of the file, a digest filled in for an entry that said
    `pending` or a comment, is another BOM. A platform installed on a `-dev` build is therefore
    refused (`bom_unsupported`) by every later build whose BOM file differs: keep the build that
    installed it, or Remove and install again. Finish an install you want to keep only on a build
    whose BOM has no `pending` digest.
  - An installation that never finished has no recorded BOM. **Retry** after a ProxCloud upgrade
    runs with the new build's BOM on whatever the failed run left on the seed and the cluster,
    without the comparison an installed platform gets. Nothing checks that step from one BOM to the
    other; when the failed run had passed stage 6, Remove first if the new BOM changes a chart or
    provider version.
- **Remove does not delete the seed's other Applications** (ArgoCD, cert-manager and `capi` on the
  seed) one by one, as the design's step 6 describes. They go with the seed VM. With the seed kept
  (a Remove that stops before `remove-seed`), they stay.
- **`seed.mode: existing` and `provisionedBy: installer` are not implemented.** The API refuses
  both in a create or an update (`400 invalid_installation`), and the `spec` check fails for a
  record stored with either before that. A draft can change both; an installed record stored with
  `provisionedBy: installer` is corrected by a PUT that sends `provisionedBy: portal`, the one
  change of that field the API accepts after a run succeeded. Nothing in the installer writes an installation record,
  and a site with an installer-built services cluster is not shown on the page.
- **The cluster chart's fixes of 0.1.12 reach a new install only.** The BOM names
  `proxmox-kamaji-cluster` 0.1.12, for the platform cluster and for new tenant clusters (whose
  output did not change). A platform cluster built from the BOM that named 0.1.11 has a control
  plane node that registers the API address as its InternalIP (its kubelet has no `node-ip`;
  nothing failed on it with one control plane machine) and a kube-vip that gives up the API address
  when a lease renewal takes over 10 seconds. Chart 0.1.12 gives the kubelet the machine's own
  address and kube-vip a renew deadline of 40 seconds (several tries of at most 10 seconds each,
  see [What no check reads](#what-no-check-reads)). No release upgrades between BOMs: an apply on
  the earlier platform is refused (`bom_unsupported`), and it gets 0.1.12 by Remove and a new
  install.
- **`layout: separate`** (one cluster for Kubernetes, one for databases) is refused by the API.
- **No VLAN** on the platform network, and no IPv6.
- **One control plane machine.** The settings take no control plane count.
- **No sandbox node group.**
- **One installation on the page.** The API keeps one per Proxmox provider; the page shows the first.
- **No route deletes a draft or an installation record.**
- **No block volumes for tenant clusters and no database backups** on Compose
  ([section 9](#9-limits-on-the-compose-install)).
- **No per-namespace RBAC for the providers' service account** on the shared cluster.

## Not verified on a lab yet

The milestone run (M0) finished once on a lab Proxmox VE cluster: attempt 5, with one **Retry**, on
the code before the changes it led to. Those changes have not run on a lab: a cluster's `401` and
`403` tried again for five looks, the items of a Remove, and cluster chart 0.1.12 (`node-ip`,
kube-vip's lease). The lab checks L0 to L11 of the design are not recorded as passed. Until they
are recorded here, treat every behaviour above as what the code and its tests say, not as
observed, except the figures this guide marks as seen on a lab:

| Check | What it would show |
|---|---|
| L0 | The API container routes to the platform network |
| L1 | The script, the checks and the mint drill ([proxmox-permissions.md](proxmox-permissions.md#the-mint-grant-in-m1)) |
| L2 | The seed: its CA, the bootstrap token, the CD gone after stage 5, cloud-init off |
| L3 | The seed's components and the clusterctl inventory |
| L4 | The first kubeadm join of the BOM's node image, group labels and taints, kube-vip. With it: that a machine cloned from a template with its cloud-init drive on `ide0` starts from CAPMOX's ISO (`qm config <vmid>` shows one cloud-init medium, on `ide0`; in the machine `grep 'Using data from' /var/log/cloud-init.log`), and the texts of CAPMOX's conditions as stage 7 shows them |
| L5 | Placement of Kamaji and etcd on the `kamaji` group; the locked `default` project |
| L6 | The seed ArgoCD reaching the cluster with its token; a public OCI chart pulled without credentials |
| L7 | End to end: a tenant cluster and a tenant database, and the database egress fence |
| L8 | Resume after an API restart in stages 2, 3, 7 and 9; cancel and Retry |
| L9 | Failure drills: `seed_not_booted`, `machine_failed`, a removed grant, a template deleted by hand |
| L10 | Seed reboot, the backup job and a restore, Remove, and Remove with the seed gone |
| L11 | Seed credential rotation and the expiry of the break-glass kubeconfig |

Also not run: the identities script on a site that already has the installer's tokens, a token
secret rotation, an install on a Proxmox cluster with a single node, and `--managed-services` on a
Compose host against a published release.
