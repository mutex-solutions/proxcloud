# Managed services from the operator console

How an operator builds, repairs and removes the platform that tenant Kubernetes clusters and tenant
databases run on, from the operator console (System → Managed services, `/platform`). The decisions
are in [ADR 0009](adr/0009-managed-services-platform-lifecycle.md); the routes are in
[api.md](api.md#managed-services-platform--apiv1adminplatform); the Proxmox roles and ACL rows are in
[proxmox-permissions.md](proxmox-permissions.md#managed-services).

> ## Status
>
> **Most of this page is not verified on a lab.** The code is covered by unit tests,
> renderer goldens, runner tests against a fake Proxmox and a fake Kubernetes API, and a mocked
> console. The install finished on one lab Proxmox VE cluster, on builds of main; what was run
> there and what was not is in [Not verified on a lab yet](#not-verified-on-a-lab-yet). It is off
> by default. v0.8.0 is the first release that carries it; one install was made on its published
> images (a lab, 2026-11-18) and one on those of the candidate v0.9.0-rc.2 (the same lab,
> 2026-11-19), none on those of v0.8.1, v0.8.2, v0.9.0, v0.9.1, v0.10.0, v0.10.1 or v0.10.2: see [Not implemented yet](#not-implemented-yet).

Code: `internal/platformlife` (the installation model, the preflight, the runner and its stages, the
renderers), `internal/platform/admin/platform.go` (the routes), `internal/cloud/pve/access.go` and
`setupscript.go` (the identities script), `deploy/platform/platform-bom.yaml` (what is installed),
`web/admin/src/pages/platform` (the page).

## 1. What it is

With the switch on, the console builds one **platform** on a Proxmox VE cluster:

- a **seed**: one VM in pool `proxcloud-seed` running k3s, with ArgoCD, cert-manager, Cluster API
  and CAPMOX. It builds the platform cluster and keeps its components at the versions of the BOM;
- a **platform cluster** (`pc-platform`, called the workload cluster in the code): a kubeadm
  cluster of VMs in pool `proxcloud-system`, with one control plane machine (or three on one node,
  [Presets and the control plane count](#presets-and-the-control-plane-count)) and three node groups:
  `system` (the platform's own components), `kamaji` (the control planes of tenant Kubernetes
  clusters) and `dbaas` (tenant databases). Every group but `system` is tainted, and each machine
  carries its group as the label `node-restriction.kubernetes.io/proxcloud-node-group`;
- the components of `deploy/platform/platform-bom.yaml`, each an ArgoCD Application on the seed.
  On the seed: `argocd` (a k3s HelmChart), `cert-manager`, `capi` and `pc-platform` (the cluster
  itself, from the chart `proxmox-kamaji-cluster` in `controlPlane.kind: kubeadm` mode). On the
  platform cluster: `cilium`, `cert-manager`, `metallb`, `argocd`, `cnpg`, `proxmox-csi`,
  `capi-tenant`, `kamaji-crds`, `kamaji-etcd`, `kamaji` and `platform`;
- two providers, `<name> Kubernetes` (`kamaji`) and `<name> Databases` (`dbaas`). Who gets them is a
  setting (`spec.tenants.attach`, from v0.9.0): every project, new projects only, or none. An
  installation saved before v0.9.0 has no such setting and means none, as before:
  `attachToNewProjects: false` and no project attached. The Kubernetes provider offers the BOM's
  Kubernetes version once its node image is published ([what a tenant can use](#what-a-tenant-can-use-after-the-install)).

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
| Tenant block volumes (proxmox-csi) | None on Compose: the compute provider holds no gateway token, also when the gateway listener is on (`--cluster-gateway`) | Through the CSI gateway |
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
| Permissions | `admin:service:read` to read the page, `admin:service:manage` for every button, the Check and the two kubeconfig downloads included. |
| A platform network | A free IPv4 network from /8 to /29, a gateway and one to three DNS servers. It must not overlap the provider's tenant address pool (`cidrPool`), a sandbox network or a public network. Either an existing bridge on every node used, with an optional VLAN tag on the machines' NICs (mode `bridge`), or, on a one-node site, a network ProxCloud makes and the node routes (mode `routed`): [The platform network: bridge, VLAN tag or routed](#the-platform-network-bridge-vlan-tag-or-routed). |
| Routes | The API (on Compose: the API container) reaches the Proxmox API, the seed address on 6443 and the cluster's API address on 6443. The seed and the cluster machines reach the Proxmox API URL you enter, `github.com` and `raw.githubusercontent.com` (at first boot the seed downloads the k3s binary from the first and the k3s install script from the second, both checked by sha256; GitHub redirects a release download to another host of its own, which must be reachable too and is not named in the BOM) and the registries of the BOM (`ghcr.io`, which serves every chart: ours and the third-party ones of the chart mirror, `ghcr.io/mutex-solutions/charts-mirror`; and the image registries those charts pull from). Tenant networks must reach the load balancer range and nothing else of the platform network. |
| Storage | A boot store that takes VM disks on every node used; an ISO store that is local to each node and takes ISOs (default `local`); an import store with `import` content on every node an image goes to (default: the provider's `importStorage`). The import store must not be the boot store: the portal token holds `PVEDatastoreAdmin` on the import store, which reaches every disk on it ([section 10](#10-security-boundaries)). The API refuses settings that name the same store for both; the form says so before it saves when you type the same store in both fields. An empty import store takes the provider's `importStorage`, so it is refused too when that is the boot store. The import store of the settings and the provider's `importStorage` must be one store: the script of step 2 is written for the store of the settings, and the `privileges:kubemint` check reads the provider's ([when the provider's import store is the boot store](#when-the-providers-import-store-is-the-boot-store)). |
| Capacity | The smallest machine is 2 cores, 4 GiB of memory and a 20 GiB disk. The smallest layout (seed, control plane, one machine per group) is five VMs. The review step adds up cores and memory per node, with the one more machine an update of the control plane makes and with one more of each group, next to the memory Proxmox reports as free on that node; the Check row `capacity` counts as the machines are counted when they are cloned ([Presets and the control plane count](#presets-and-the-control-plane-count)). |
| Root on one node | Once, for one pasted command: [the root command of step 2](#the-root-command-of-an-installation). It makes what only root can (roles, users, tokens, ACL rows, pools) and hands the two token secrets back in one line. The cluster firewall rule for the Proxmox API, the blocked networks and a new tenant zone without DHCP are no longer root steps: the install and the save of the settings do them with the portal token ([What ProxCloud does with the portal token](#what-proxcloud-does-with-the-portal-token)). |
| A router for a VLAN tenant zone | Only when the provider's tenant zone is a `vlan` zone. Proxmox gives a VLAN no gateway, no DHCP and no SNAT: a tenant network there is a bare layer 2 segment, and ProxCloud writes and reports it so (`dhcp: false`, `snat: false`; asking for either is a `400`). The site's own router must hold the first address of each tenant subnet (the `gateway` the network shows; servers and cluster workers get it as their default route) on that VLAN, route it to the load balancer range and to whatever else tenants may reach, and keep tenants apart. Without it a worker on such a network never reaches its control plane and the cluster stays `PROGRESSING`. An EVPN zone needs none of this. |
| A path from tenants to the platform network | Tenant cluster workers and database clients open TCP connections to the platform network. On a node that translates tenant traffic (a `simple` tenant zone, or an exit node of an EVPN zone) and also runs a platform machine, that connection is dropped unless the node itself routes the platform network. A one-node site with a `simple` tenant zone and the platform network on a VLAN behind the site's router is this case; a platform network of mode `routed` is not. The Check warns (`tenant-path`) and the install goes on, and **Test the tenant path** opens the path from a tenant network once the platform is installed: [The path from tenants to the platform](#the-path-from-tenants-to-the-platform). |
| A tenant network zone without DHCP | Only for tenant Kubernetes clusters, not for the install or for databases: a cluster's nodes get fixed addresses, so its network must be in an EVPN or a VLAN zone, or in a `simple` zone without a DHCP server. With a provider whose default zone (`config.network.defaultZone`) is a `simple` zone that ProxCloud created with DHCP, which is its default, a cluster create on its networks answers `400 Worker nodes need a network without DHCP (an EVPN or VLAN zone, or a simple zone without a DHCP server)`. The Check reads the zone in Proxmox, warns (`tenant-network`) when it has DHCP, and the install goes on; change the provider's default zone before you offer clusters ([managed-k8s.md](managed-k8s.md)). Not affected: a Kubernetes provider with shared workers (`cluster.shared`), which uses no tenant network, and a `simple` zone without DHCP. Such a zone is made in one of two ways: by hand in Proxmox before the first tenant network (a zone that exists is used as it is), or by ProxCloud when the zone's config says so, `"zones": {"<id>": {"kind": "simple", "dhcp": false}}` (from v0.8.1; it applies to a zone that does not exist yet, and an existing zone is never changed). Networks of such a zone answer `dhcp: false` and get no DHCP range; servers, cluster nodes and load balancers there get addresses the platform assigns. The key is refused on a zone of another kind (`400`, naming the key). The setup wizard keeps a zone's other keys when you change its kind there, so take `"dhcp"` out of the zone's config (Cloud providers) before you change such a zone to VLAN or EVPN. The key is not verified on a lab. |
| The datacenter firewall | On, with the provider's isolation rules written by a provider sync once the installation is saved: the Check verifies that tenant traffic to the seed, the API address and the node range is dropped. Saving the settings adds those addresses to the provider's blocked networks. With the firewall on, a node drops what does not come from its own network, so the seed and the platform machines need a rule to reach the Proxmox API: the install writes it (`pve-api-rule`). The rule names the platform machines' own addresses: [a site whose router translates them](#when-a-router-translates-the-platform-network) needs a rule of its own. |

The page also warns when the job scheduler is off, in three places with one text: **Before you
start**, the Check (row `scheduler`, a warning, absent while the scheduler runs) and the ready card.
With it off:

- no project is deleted: "Delete now" is refused with a `409` that says so, and a delete with the
  grace period is accepted but stays scheduled until the scheduler runs;
- the state of tenant clusters, databases and servers is refreshed only when an operator runs a
  provider sync;
- no usage is charged and no bill is made.

It does not block an install, and nothing on this page turns it on, because it also starts charging
usage on a timer: set prices first. The command, on the ProxCloud host of a Compose install
([Compose README](../deploy/compose/README.md#optional-features)):

```sh
./proxcloud-up.sh install --scheduler
```

On another kind of install, set `PROXCLOUD_JOBS_SCHEDULER_ENABLED=true` for the API.

The images a run imports (stage 2) are not smoke-tested automatically: the tests would clone beside
the build. Start the smoke test of an image from Template updates when the run has ended, before you
publish it ([template updates](template-updates.md)).

### VM id 9999 and the CSI volumes

proxmox-csi names every volume it creates `vm-9999-pvc-<uuid>`: 9999 is the plugin's default
`controllerVmID`, and neither the platform cluster nor a tenant cluster changes it. The platform's own
volumes carry these names (Kamaji etcd, the registry, every managed database), and so do the volumes
of tenant Kubernetes clusters. No VM 9999 is needed for this, and ProxCloud creates none. But to
Proxmox a disk named `vm-9999-…` belongs to VM 9999, whether that VM's config names the disk or not.

When a guest of your own has the id 9999 (on the first production site it was the owner's template),
the Check warns with the row `csi-vmid`, and so does **Test connection** on the provider's page. The
install goes on: the platform works either way. What must never be done on such a site:

- **Never destroy VM 9999 with "Destroy unreferenced disks owned by guest" ticked**
  (`qm destroy 9999 --destroy-unreferenced-disks 1`). Proxmox then also destroys the disks with a
  matching VM id on all enabled storages, which are the CSI volumes.
- Before you remove or move VM 9999 for any other reason, read `qm config 9999`: it must name no
  `vm-9999-pvc-` disk. A disk rescan (`qm disk rescan`) lists disks named after a VM as that VM's
  unused disks, and a plain destroy removes the unused disks of the config too.

The safe way is to leave VM 9999 as it is, and to give no new guest that id. This release does not change
the id the plugin uses. ProxCloud asks Proxmox for the lowest free id when it creates a VM, so on a
cluster with many guests, or with a datacenter `next-id` range that reaches 9999, a server of a tenant
can get it. Its own delete of a VM sets `destroy-unreferenced-disks`, except for a VM with id 9999:
that one is destroyed without the flag, so the CSI volumes stay.
Not tried on a lab: the two commands above against a VM 9999 with CSI volumes present, and the two
warnings against a real cluster (their tests use a fake Proxmox API). What the commands do is stated
from the `qm` manual (`--destroy-unreferenced-disks`: "destroy additionally all disks not referenced in
the config but with a matching VMID from all enabled storages"; `qm disk rescan`: "update disk sizes
and unused disk images"). The warning was added after the production acceptance of v0.8.0 found the
owner's template on that id.

### The path from tenants to the platform

Tenants open connections to the platform network: a tenant cluster's workers to their API address
(port 6443 on an address of the load balancer range), a tenant's servers to a managed database. On
one layout that path is closed for TCP although every range and firewall rule is right, and
nothing but the Check row `tenant-path` says so before a tenant finds it. Found on the first
production site (v0.8.0, 2026-11-18, one node): the control plane was ready in under a minute and
no worker ever joined.

**The rule.** A tenant's TCP connection to a platform address is dropped when both hold:

1. a Proxmox node translates it (SNAT). In a `simple` zone that is the node the tenant's VM runs
   on, for a network with outbound internet. In an EVPN zone it is an exit node. In a VLAN zone no
   node translates: the site's router does.
2. the translated packet enters the same node again through a bridge, because the platform machine
   that owns the destination address runs on that node and the way from the node to the platform
   network goes over something else (the site's router).

The node's connection tracking for bridges then sees the returning packet as a second connection
with the same reply as the first, and drops it. The Proxmox nftables firewall (`nftables: 1` in the
host firewall options) turns that tracking on for every bridge of the node, with or without
`firewall=1` on a NIC. A connection from the node itself or from the LAN passes; a tenant's TCP
connection never does (by the source UDP is not dropped; not tested). On the production site the
counters `insert_failed` and `drop` of `conntrack -S` grew by the same number with every attempt.
For the older iptables firewall Proxmox's SDN writes a rule of its own for this case; whether a
node on that firewall is free of the problem was not tested.

| Tenant zone | The platform machine that owns the address is on | Result |
|---|---|---|
| `simple`, one node, platform network a VLAN behind the site's router | the same node | **broken** (seen on the production site) |
| `simple`, several nodes | the tenant VM's node | **broken**. On another node it works, but the API address and a load balancer address move between the platform machines' nodes, so after a failover it breaks for other tenants |
| EVPN with SNAT on an exit node | the exit node | broken by the same rule; on another node it works. Not tested |
| VLAN zone, the site's router translates | any node | works: the node translates nothing (lab: workers joined on a VLAN zone) |
| any zone, the platform network routed by the node itself (the node holds its gateway) | the same node | works: the packet never leaves the node. Not tested |

The mechanism is stated from the kernel and Proxmox sources and fits the production counters and
captures; it was not reproduced on a lab, and the hook that drops was not captured.

**What you see when it is wrong.**

- A tenant cluster's control plane becomes ready and its endpoint answers from your workstation.
  The workers' VMs run and the node group stays at "The machine runs but has not joined the
  cluster" (`not_joined`); after 15 minutes it needs attention.
- On such a worker (console, or the guest agent), cloud-init's `kubeadm join` ended with a timeout
  on the API address, `https://<address>:6443`. From a tenant server on the same node
  `curl -k -m 5 https://<address>:6443/healthz` times out; the same command on the node answers.
- A tenant server cannot open a managed database's port, while the database reads healthy.
- On the node, `conntrack -S` shows `insert_failed` and `drop` growing during each attempt.

**Workers that did not join must be replaced.** `kubeadm join` runs once, at the first boot. A
worker that timed out stays "not joined" after the path is repaired. Replace it: the tenant
reduces the node group's count by one and raises it again. A count below one is refused, so a
group cannot go to zero and back: it is one such cycle per dead worker, and a group of one is
raised by one first and reduced once the new worker is ready (this needs quota for one more
worker). On the production site two dead workers of a group of two were replaced by 2 → 3 → 2,
twice: both times the scale-down removed a machine without a node, never a ready worker. Which
machine goes is Cluster API's choice ([managed-k8s.md](managed-k8s.md), scale-down); only that
order was run.

**What the Check reads.** The row `tenant-path` reads the provider's default zone in Proxmox (its
kind, the nodes a `simple` zone is limited to, an EVPN zone's exit nodes; for a zone that does not
exist yet, what the provider's config says), the nodes the settings place the platform's control
plane and node groups on, and whether such a node routes the platform network itself: the platform
bridge is a vnet of a `simple` zone with a subnet whose gateway is the platform gateway, or the
node's saved network configuration holds the platform gateway on that bridge. It **warns** when a
node that translates also hosts a platform machine and does not route the platform network, and
when Proxmox could not be read. It never fails, because it opens no connection: an `ok` row is not
proof that the path is open, and a warning stays after you added a node leg by hand (the row does
not see runtime addresses). It also warns on a node that routes the platform VLAN on a VLAN
interface of its own (the gateway on `vmbr0.140`, say): the row does not read that interface, and
by the rule above that layout should work (not tested). For a platform network of mode `routed`
the row is `ok`: before the install because the install makes the network with the gateway on the
node, after it because the row finds that vnet and its subnet.

**Test the tenant path.** The row reads; this test connects. It is a button in step 3 of an
installed platform (`POST …/installations/{id}/tenant-path-probe`), never part of **Check**,
because it writes to Proxmox and costs one VM clone and two SDN applies (each a network reload on
every node). What it does, with the portal token:

1. From the ProxCloud host it tries port 6443 and 5432 on the first 32 addresses of the load
   balancer range: a tenant cluster's API, a managed database. What opens is a target. With no
   target it stops (`409 no_target`) and creates nothing: make one database or cluster first.
   When a cluster API (6443) answers, only cluster APIs are targets: a managed database with
   `allowedCidrs` that admit the ProxCloud host and not the test network drops the test's
   connection, which looks like a closed path. With databases only, a `failed` says so.
2. It makes a network `tenant-path-probe` in the provider's default tenant zone, a free /28 from
   the end of `cidrPool`, with SNAT, as a tenant's network with outbound internet has it (in a
   VLAN zone Proxmox writes no SNAT, as for tenants).
3. On the node you choose (the control plane's node when you choose none; it must host a platform
   machine) it clones an Ubuntu or Debian image into the pool `pc-templates` as
   `pc-probe-<installation id>`, puts it on that network with a fixed address (the one after the
   gateway, over cloud-init) and starts it. The image is resolved as a tenant's server create
   resolves one, in the provider's catalog, from the copy on that node, and it must be this
   portal's own: a template that Template updates imported for this provider, or one the
   provider's `config.templates` names with its VM id. An import counts only when it finished
   and the template now at its node and VM id still has the import's name and source digest
   (the tag `pc-src-…`): Proxmox gives the VM id of a failed import, or of a template deleted by
   hand, to the next VM. Ubuntu before Debian, then the newest build (the `pc-v-` stamp,
   compared across images: a later build of Ubuntu 24.04 is taken before an earlier build of
   26.04). A published template in `pc-templates` that is neither is never cloned: on a Proxmox
   cluster that several portals share it can be another party's. The clone's disk goes to the
   store a tenant server's disk goes to on that node (the provider's default storage profile;
   without one, the template's own store). Without such an image on that node the test stops
   before step 2 (`409 no_os_template`, which names the published templates it did not take):
   import and publish one under Template updates. With the scheduler off nothing is published by
   itself. A site whose published images were all built outside Template updates (tagged by the
   image pipeline) gets this refusal until it imports one. Naming a template in
   `config.templates` with its VM id and node also satisfies the test, but it is not a setting
   of the test only: such an entry replaces the discovered image of that slug in the tenant
   catalog, with its copies on other nodes, so on a site with several nodes every tenant server
   of that image is then created from that one template on that one node. Prefer the import; pin
   only on a site with one node, or where that is what you want for tenants as well. The answer of the start and the result name the image, its template, the node and the
   store (`image`, `template`, `node`, `storage`).
4. Through the guest agent it opens one TCP connection to each target (at most 8) and one to the
   platform cluster's own API address, each with a 5 second limit.
5. It stops and destroys the VM, deletes the network and applies SDN: after a pass, after a
   failure, and after its 8 minute limit. What it could not remove is listed with the command to
   run. The next test first removes what an earlier one left (a ProxCloud restart in the middle of
   a test leaves the VM and the network until then). A test cut off that way reads as `error`
   12 minutes after its start, with what it may have left, and the button works again. The
   cleanup destroys a VM only when it has the test's name, is in `pc-templates`, is no template
   and carries the tag `pc-probe`; a VM with the name and without the tag (a clone cut off before
   it was tagged, or not the test's at all) is listed and left, and the next test does not start
   until you removed it. In a zone with a DHCP server Proxmox gives the VM's NIC an address of
   the range by itself, which the test does not use, and may keep that mapping on the node after
   the VM is gone (a line in `/etc/dnsmasq.d/<zone>/ethers` and in
   `/var/lib/misc/dnsmasq.<zone>.leases`). ProxCloud can neither read nor remove those files, so
   the result names them (`notes`) with what to delete; they do no harm while no other network
   uses that /28.

The result, read with `GET` on the same path:

| State | Meaning |
|---|---|
| `passed` | Every target opened from the tenant network (a refused connection counts: the address answered), and the platform's own API address did not answer, as the isolation rules want |
| `failed` | A target the ProxCloud host opens did not answer from the tenant network: the path is closed on that node (when only port 5432 was tried, a database's `allowedCidrs` can give the same result, and the message says so). Or the platform's own API address answered: tenants reach what the isolation rules should keep from them (see the Check row `isolation`) |
| `error` | The test could not be done (the clone failed, the guest agent never answered, the limit passed): nothing is known about the path |

One test covers one node. The path depends on the pair of nodes (where the tenant's VM runs,
where the platform machine that holds the address runs), so on a site with several nodes test
each node that hosts a platform machine, and again after a load balancer address moved. The test
is refused during a run of the installation (`409 run_active`), while another test runs
(`409 probe_running`), before the first install (`409 not_installed`: nothing listens yet), and
when the provider's default zone is a VLAN zone (`409 vlan_zone`: no node translates there, and a
test network would need its own VLAN and gateway on your router; test from a tenant's server).
The last test is kept (`GET …/tenant-path-probe`) until the next one. **Remove** deletes it with
the recorded status, and an install starts without one, so a platform installed anew answers
`404 no_probe` until its own first test (up to v0.9.0 it answered the removed platform's verdict,
which was about other machines; found on a lab on 2026-11-19, the fix is not run on a lab).
**Not verified on a lab:** no lab run of the test has ended with a verdict. Its one lab run
(2026-11-19, a `simple` zone with DHCP) ended `error`: the test then took the first published
Ubuntu template in slug order, which was a template of 2026-11-06 that this portal had not
imported, cloned it onto that template's own store, and waited for an address from the zone's
DHCP that never came (the guest agent answered, the NIC had no IPv4 address, and the zone's
dnsmasq logged no request; why was not determined, the VM is gone). Since then the test takes
only an image of this portal's own, clones it to the provider's disk store and gives the VM a
fixed address, so it no longer depends on a DHCP answer. That is covered by tests against a fake
Proxmox API and was not run on a lab; the lab test that would show it: with the simple zone as
the default zone and a tenant cluster running, start the test on a node that also holds a
template this portal did not import, and expect `image` to name the imported one and the state
`passed` or `failed`, not `error`. Its first lab verdict is to be the reproduction of the
production finding (`run-1` lab test "reproduce": expected `failed` from the node that holds the
address with a `simple` tenant zone, `passed` from another node).

**A new one-node site: let the node route the platform network.** Choose **Routed by the node**
in step 1 (`spec.network.mode: routed`) and ProxCloud makes the network at the install:
[The platform network: bridge, VLAN tag or routed](#the-platform-network-bridge-vlan-tag-or-routed).
The same network made by hand also works with mode `bridge`: a vnet of a `simple` SDN zone with
one subnet whose gateway is the platform gateway and with SNAT ticked (Datacenter → SDN in
Proxmox, then Apply), entered as the platform bridge. SNAT is for the platform machines' own way out: the node translates what they send
through its uplink, so the answers from the internet (image pulls during the install) come back
to the node. Without SNAT the site's router must have the route below; a route on the ProxCloud
host alone then leaves the platform machines without a return path from the internet and the
install fails at its image pulls. Proxmox puts
the gateway address on the node, so a tenant's packet is routed inside the node and is not
translated on the way (the zone's SNAT rule only matches what leaves through the node's uplink).
Consequences:

- The platform sees a tenant's real address. A database's `allowedCidrs` then hold tenant subnets,
  not the node's address ([managed-dbaas.md](managed-dbaas.md)).
- Everything outside the node (the ProxCloud host, your workstation) needs a route "platform
  network via the node's address", on the site's router or, with SNAT on the subnet, on the host
  itself: without it the API container reaches neither the seed nor the cluster's API address,
  and the run fails.
- It is a layout for **one node only**: a vnet of a `simple` zone is a separate network on each
  node, and the platform bridge must be one layer-2 segment
  ([What no check reads](#what-no-check-reads)).

The layout is not run anywhere yet: not on a lab, not on a production site.

**An existing site with the platform network on a VLAN: a leg of the node.** Give the node an
address of its own on the platform bridge and send tenant traffic for the platform network out
through it, so that the packet no longer passes the router and never enters the node a second
time. The address must be on the bridge device itself: by the source a veth pair plugged into the
bridge fails in the same way (not tested). The
commands below were run by hand, as root, on the one production site, at runtime, and the path
opened (a tenant server got an HTTP answer from the API address, `insert_failed` stopped growing,
the node's own route was unchanged). They are **not lab-verified**. Replace the four values: the
platform bridge (`plat140`), the platform network (`192.168.140.0/24`), a free address in it for
the node (`192.168.140.2`) and the provider's tenant pool, `cidrPool` (`10.100.0.0/16`). `140` is
a free routing table number, `1400` and `1401` free rule priorities.

```sh
# read first
ip -br addr show plat140; ip rule; ip route show table 140; sysctl net.ipv4.conf.all.rp_filter
arping -c 2 -I plat140 192.168.140.2        # expect no answer: the address is free
# apply
ip addr add 192.168.140.2/24 dev plat140 noprefixroute
ip route add 192.168.140.0/24 dev plat140 src 192.168.140.2 table 140
ip rule add pref 1400 from 10.100.0.0/16 to 192.168.140.0/24 lookup 140
ip rule add pref 1401 from 192.168.140.2 to 192.168.140.0/24 lookup 140
iptables -t nat -I POSTROUTING 1 -s 10.100.0.0/16 -d 192.168.140.0/24 -o plat140 -j SNAT --to-source 192.168.140.2
# verify
ip route get <a platform address>                  # unchanged: still through the router, the node's own path
ip route get <a platform address> from <a tenant server's address> iif <its vnet>    # "dev plat140 table 140"
conntrack -S | head -3                             # insert_failed no longer grows during the next line
# from a tenant server: curl -k -m 5 https://<the cluster's API address>:6443/healthz   -> an HTTP answer
# undo
iptables -t nat -D POSTROUTING -s 10.100.0.0/16 -d 192.168.140.0/24 -o plat140 -j SNAT --to-source 192.168.140.2
ip rule del pref 1401; ip rule del pref 1400
ip route flush table 140
ip addr del 192.168.140.2/24 dev plat140
```

- The rules select tenant sources only and the main routing table gets no route, so the node's
  own traffic, the LAN and your other VMs keep their paths. If a line is lost, the path is closed
  for tenants again and nothing else changes.
- The platform now sees the leg's address as the source of every tenant: a database's
  `allowedCidrs` must hold it (`192.168.140.2/32`), where they held the node's address before.
- **It does not last.** The lines are lost at a reboot of the node, and whether they survive an
  SDN Apply is not known: run the verify lines after each. The persistent form is below, and is
  not verified.
- On several nodes each node that hosts a platform machine needs its own leg and address. Not run.
- Then replace the workers that did not join (above).

**The leg in a persistent form. NOT lab-verified: do not rely on it before you tested it.**
ProxCloud does not write this file and no script of its own does: these are commands for you, as
root on the node, with the same four values as above. The file is one of your own in
`/etc/network/interfaces.d/` (the default `/etc/network/interfaces` of Proxmox reads that
directory; check that yours has the line `source /etc/network/interfaces.d/*`). It adds
`post-up` lines to the platform bridge, each written so that running it twice changes nothing.

```sh
cat > /etc/network/interfaces.d/pc-tenant-leg <<'LEG'
iface plat140
    post-up ip addr replace 192.168.140.2/24 dev plat140 noprefixroute
    post-up ip route replace 192.168.140.0/24 dev plat140 src 192.168.140.2 table 140
    post-up ip rule del pref 1400 2>/dev/null; ip rule add pref 1400 from 10.100.0.0/16 to 192.168.140.0/24 lookup 140
    post-up ip rule del pref 1401 2>/dev/null; ip rule add pref 1401 from 192.168.140.2 to 192.168.140.0/24 lookup 140
    post-up iptables -t nat -C POSTROUTING -s 10.100.0.0/16 -d 192.168.140.0/24 -o plat140 -j SNAT --to-source 192.168.140.2 2>/dev/null || iptables -t nat -I POSTROUTING 1 -s 10.100.0.0/16 -d 192.168.140.0/24 -o plat140 -j SNAT --to-source 192.168.140.2
LEG
ifreload -a
# verify, now and after every SDN Apply and reboot
ip -br addr show plat140; ip rule | grep -c 'lookup 140'      # the address; exactly 2 rules
iptables -t nat -S POSTROUTING | grep -c -- '--to-source 192.168.140.2'    # exactly 1
# undo: delete the file, then the undo block of the runtime commands above
rm /etc/network/interfaces.d/pc-tenant-leg
```

What is not known about this form, and what would settle it (a lab, on a throwaway VLAN vnet, never
on the bridge that carries a running platform):

- Whether ifupdown2 accepts a second `iface` stanza for an interface that Proxmox's SDN defines in
  `/etc/network/interfaces.d/sdn`, and merges the two. If it does not, the same five `post-up`
  lines go on an interface of your own (a `dummy` interface with `auto` and `iface … inet manual`,
  as the installer's `/etc/network/interfaces.d/proxcloud-sandbox-snat` does); that form does not
  fix the order at boot (the bridge must exist before the lines run).
- Whether a reload (`ifreload -a`, which every SDN Apply runs) removes an address it does not find
  in the configuration before `post-up` puts it back, which would cut tenant connections for a
  moment at each Apply.
- That no rule is doubled: run `pvesh set /cluster/sdn` twice and repeat the verify lines. Proxmox's
  own SDN rules are doubled at every Apply on a lab node (seen there), which is why each line above
  tests before it adds.
- The order at boot, and `rp_filter` in strict mode (rule 1401 is there for it; the production
  value was not read).

Until those are settled the runtime commands, run again after each reboot and checked after each
SDN Apply, are the form that was seen to work on the one production site.

For every other site (several nodes, tenants and platform machines on the same nodes) the layout
without a node in the path is the VLAN tenant zone with your own router (the row above). Not
decided and not tested: a connection-tracking zone for the platform bridge instead of the leg.

### The platform network: bridge, VLAN tag or routed

`spec.network.mode` says where the platform network comes from. It is part of the network block,
which is fixed after the first successful install. A settings record saved before the field
existed has no mode and means `bridge`, exactly as before.

**`bridge` (the default): an existing bridge.** The bridge (a Linux or OVS bridge, or an SDN vnet
you made) exists on every node the settings place a machine on, is one layer-2 segment across
them, and your router routes the network. The Check row `bridge` looks for it on each of those
nodes.

**A VLAN tag on a bridge (`spec.network.vlan`, 1 to 4094, mode `bridge` only).** Every NIC on
the bridge is tagged: the seed's (`tag=<n>` in its `net0`) and every platform machine's
(`networkDevices[].vlan` of the Cluster API provider, which Proxmox also writes as `tag=<n>`). So
a VLAN needs no bridge or vnet of its own, no SDN object and no SDN Apply; your switches and
router must carry and route the VLAN, which no check reads.

- It needs cluster chart `proxmox-kamaji-cluster` 0.1.14 or newer, the first that reads the tag.
  The platform's BOM names the chart: with an older one the Check row `spec` fails and says so,
  and nothing is rendered untagged. The BOM of this build names 0.1.16, so a new install can
  tag. An installation on an earlier BOM (0.8.x, chart 0.1.13) is refused a tag; its network is
  fixed anyway.
- For a tagged NIC Proxmox checks `SDN.Use` on `/sdn/zones/localnetwork/<bridge>/<tag>` (on
  `/sdn/zones/<zone>/<vnet>/<tag>` when the bridge is a vnet). The identities script grants the
  CAPI token its role on `/sdn/zones` with propagate, which reaches that path; the Check row
  `privileges:capi` reads the tag's own path, because a row of the token's own below `/sdn/zones`
  would replace what is passed down.
- An installed platform cannot get a tag: the network is fixed, and the tag is part of every
  machine's template.
- Not verified on a lab: the whole tag path (the privilege was read in Proxmox's code, not tried),
  and a tag on a bridge that is not VLAN-aware.

**`routed`: a network ProxCloud makes and the node routes. One Proxmox node only.** The bridge
field names a vnet that does not exist yet (a lower-case letter, then letters and digits, at most
8). The name must not be an interface of any node (`vmbr1`, `bond0`, `eno1`: the pattern admits
them): a vnet of that name would write a second configuration for the interface at the SDN Apply
and could cut the node or its VMs off. The Check row `bridge` and the run read every node's
network list and refuse such a name before anything is written; a node that does not answer
refuses it too. Whether Proxmox itself refuses it is not known (no lab test). The network must
also be a range no node is in: the row `network` fails when it overlaps a node's management
network or any address a node holds, other than the gateway itself. At the start of stage 3 the run makes, with the portal token and under the lock that
serializes every SDN change of the site:

| Object | What |
|---|---|
| SDN zone `pcplat` | `simple`, without a DHCP server, limited to the seed's node. Made once; a zone of that id of another kind stops the run |
| vnet `<bridge>` | In that zone, with the alias `proxcloud-platform:<installation id>`. A vnet of that name with another alias is never used or changed: the Check row `bridge` and the run refuse it (`platform_network_refused`) |
| subnet `<network>` | With the gateway of the settings, which Proxmox puts on the node, and **SNAT on** |
| SDN Apply | Before the seed is cloned. An Apply reloads the network configuration on every node of the cluster. The run applies once; when the node does not list the vnet among its bridges at a later attempt (the first Apply failed, or ProxCloud restarted before it), **Retry** applies again and creates nothing twice |

Why this layout: a tenant's packet to the platform is routed inside the node and never translated
on the way, so the rule of [The path from tenants to the platform](#the-path-from-tenants-to-the-platform)
does not apply, and the platform sees the tenant's real address.

- **SNAT is on, and that is not a setting.** It is the platform machines' way out: the node
  translates what they send through its uplink, so image pulls work without a route on your
  router. It only matches traffic that leaves through the uplink: not tenants' traffic to the
  platform, not the machines' calls to the Proxmox API on the node. A connection from a platform
  machine to an address that your router sends back into the same node through a bridge would meet
  the rule of the section above (not tested; nothing the platform does needs such a connection).
- **The route your site needs.** Everything outside the node reaches the platform network only
  through the node: the ProxCloud host (the seed's API, the cluster's API address, the load
  balancer range) and your workstation. Add the route "the platform network via the node's
  address" on your router, or on the ProxCloud host and each workstation
  (`ip route add 10.90.0.0/24 via <the node's address>`). No check can read it before the seed
  exists: without it stage 4 ends with `seed_api_unreachable`. Incoming connections are not
  translated, so the seed's API sources (`apiSources`) name the ProxCloud host's own address, as
  for a bridge.
- **The Proxmox API URL** is the node's own address, `https://<node address>:8006`, as for a
  bridge. A platform machine's call to it is delivered on the node itself. With the datacenter
  firewall on, the node must still accept port 8006 from the platform network
  ([What no check reads](#what-no-check-reads)).
- **The isolation rules are the same.** The seed, the API address and the node range must be in
  the provider's blocked networks and the load balancer range outside them; the cluster firewall
  drops tenant traffic to the blocked ones as it is forwarded between the vnets.
- **Several nodes.** A vnet of a `simple` zone is a separate network on each node, so the API
  refuses mode `routed` unless the seed, the control plane and every node group are on one node
  (`400 invalid_installation`), and it stays refused when you later add a node to a group: a
  platform installed in this mode stays on its node. A site that wants the platform on several
  nodes uses mode `bridge` (for tenants there: the VLAN tenant zone with your own router, or a leg
  per node).
- **Remove** deletes the subnet and the vnet after the seed, and the zone when no other vnet is in
  it, and applies SDN. It finds the vnet by its alias, not by the settings, so the vnet of a draft
  whose install failed and whose settings were then renamed or switched to mode `bridge` goes too.
  A vnet that is not the installation's stays. While the installation's vnet exists under one
  name, an install that names another is refused (`platform_network_refused`).

Mode `routed` is **not verified on a lab**: its tests use a fake Proxmox API. Not known until it
ran: that Proxmox forwards between a tenant vnet and this vnet with the nftables firewall on, that
the seed boots and pulls through the SNAT, that a tenant cluster's workers join, that the node
lists an applied vnet among its bridges for the portal token (when it does not, every attempt of
stage 3 applies SDN once more, which changes nothing applied), and whether Proxmox refuses a vnet
named like an interface.

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
  this release names 0.1.16, which keeps this; chart 0.1.11 gave up after 10.
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
nothing on the seed's pool; ProxCloud asks for no root password. Step 2 of the page shows one command
to paste into a root shell on one node:

```
GET /api/v1/admin/setup/proxmox-script?installation=<installation id>[&tokenId=<portal token>]
```

It is the [portal token script](proxmox-permissions.md#portal-token-script) followed by the
managed-services block, made for the stores of the saved settings and the provider's token id, without
`gpu`. Because the provider is saved, the portal token exists already: the first lines of the output
report it as existing, which is expected. Do not remove it; the saved provider uses its secret.
What the command does with the two platform tokens is in
[The root command of an installation](#the-root-command-of-an-installation). The same script without
an installation (`?managedServices=true&bootStorage=<store>&isoStorage=<store>`) is unchanged: it
prints each secret once and marks no token. The block:

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

### The root command of an installation

The command of step 2 (`?installation=<id>`) differs from the script above in three ways. None of
them was run on a Proxmox node ([Not verified on a lab yet](#not-verified-on-a-lab-yet)).

**It marks the two tokens.** A token it makes gets the comment `proxcloud:<installation id>`
(`pveum user token add … --comment`).

**It stops on a token that is not the installation's.** The ids `capmox@pve!capi` and `csi@pve!csi`
are fixed, so a token of that id may be there already and may be in use by something else (the first
production site had a `capmox@pve!capi` of an older Cluster API setup). Before it changes anything
the command reads both tokens. One that exists, does not carry this installation's comment, and
whose secret ProxCloud does not hold, stops it:

```
STOP: the token capmox@pve!capi exists on this cluster and was not made for this ProxCloud installation (pi-0a1b2c3d).
Something else may use it, so this command does not touch it. Look at what it may do (pveum acl list | grep capmox@pve)
and at who uses it. When nothing does, remove it and run this command again:
  pveum user token remove capmox@pve capi
Nothing was changed.
```

It never deletes or changes such a token: removing it is your decision and your command. A token
whose secret ProxCloud already holds (a site set up before tokens were marked, or with the secrets
entered by hand) is the installation's: the command adds the comment to it and goes on.

"Holds" means that the stored secret opens the token now. When the page asks for the command,
ProxCloud makes one read (`GET /version`) with each stored secret. Proxmox answering `401` means the
token was removed, or removed and made again for something else: the command then stops on it as on
any token it did not make. When Proxmox does not answer, or answers anything else, no command is
given (`502`), because a guess either way is wrong: try again. That Proxmox answers `401` to the
old secret of a token made again under the same id is assumed, not tried on a lab.

**It prints no secret.** The secrets of the tokens it made go into one line, printed last, also
when a later step of the command fails:

```
Paste this one line into step 2 of the Managed services page. It carries the new token secrets: treat it as a password.
PC-ENROLL:MTpwaS0wYTFiMmMzZDoxNzk1MDAwMDAwOjExMTExMTExLTEx…
```

The line is `PC-ENROLL:` plus base64 of `1:<installation id>:<unix time on the node>:<capi secret>:<csi secret>`;
a token the command kept has an empty field. base64 is an encoding, not encryption: whoever reads
the line reads the secrets. Paste it into **The PC-ENROLL line the command printed** and press
**Store the secrets** (`PUT …/credentials` with `{"enroll": "<line>"}`). The page checks the line
before it sends it (the whole line, of this installation, two token secrets or one), and the API
checks again:

| Answer | When |
|---|---|
| `400 invalid_enrollment` | Not such a line (cut short, a bare secret, another version), or a line of another installation. The answer does not quote the line |
| `400 enrollment_clock_ahead` | The node printed the line with a clock more than five minutes ahead of the portal's. Nothing is stored: stored, that time would make every line printed after the clock was set right a replay. Set the node's clock (`timedatectl`), then run the command again |
| `409 enrollment_replayed` | The line was stored before, or a newer one was: nothing changes. This is what keeps an old line from putting back the secret of a token that was made again since. "Newer" is the time the node printed the line, so two nodes whose clocks differ by more than the time between two runs can refuse a good line: run the command again |

Running the command again changes no token and prints "No token was made, so there is no line to
paste". A token it made earlier whose secret ProxCloud does not hold (the line was lost) is named with
the command that removes it: Proxmox shows a secret once. The two fields under **Enter the two
secrets by hand instead** remain for a token made another way.

The next step, not built: a command that sends the secrets to the portal itself
(`curl … | sh` with a short-lived single-use code). It needs a route without a session, a code store,
and a node that reaches the portal's URL over TLS; the pasted line needs none of them.

### What ProxCloud does with the portal token

Three hand steps of the first production install are done by the product, each with a privilege the
portal token already holds ([proxmox-permissions.md](proxmox-permissions.md)). No new privilege.

| What | When | Privilege | Detail |
|---|---|---|---|
| The cluster firewall rule for the Proxmox API | Stage 3 of an install, before the seed is cloned, and every apply; **Remove** deletes it. On an installed platform whose firewall does not hold the rule (one installed by 0.8.x, or a rule deleted or changed by hand) the plan of step 4 names it before the run writes it: entry `pve-api-rule`, kind `site` | `Sys.Modify` on `/` (as for the isolation rules) | One rule: `in ACCEPT tcp`, source the seed's address and the node range (as CIDRs; not the load balancer range), destination the host of the Proxmox API URL of the settings when it is an address (no destination for a name), the URL's port, comment `proxcloud:pve-api:<installation id>`. A rule with that comment that differs (the settings changed, or it was disabled by hand) is replaced; no other rule is read or touched, and the `management` IP set is not used. It is written with the datacenter firewall on or off. It is written in mode `routed` too: the node holds the gateway of the vnet, but the firewall's default management set is the network of the node's own address only (read in the Proxmox documentation, not tried) |
| The provider's blocked networks | Every save of the settings | none on Proxmox: it is the provider's own config. The next provider sync writes the `DROP` rules with `Sys.Modify` | The seed's address, the API address and the node range, as the exact CIDRs that no entry of `blockedCidrs` covers yet; never the load balancer range. What a save added is recorded beside the list (`config.network.platformBlockedCidrs`), and a later save takes off exactly the recorded entries that the settings no longer need (a corrected address or range), never an entry you wrote. The answer of the save names what was added and what was taken off (`siteChanges.blockedCidrs`, `blockedCidrsRemoved`), the page shows both, and the audit event of the save carries them. Three limits. A provider sync writes a narrower set of `DROP` rules only on a provider with a project; on one without, rules already written for a removed entry stay, and the `isolation` row names them for deletion by hand ([The isolation check and the provider sync](#the-isolation-check-and-the-provider-sync)). **Remove** takes nothing off: the record stays, and the next install needs the same addresses fenced. And the save compares with `blockedCidrs` only, not with the nodes' management networks, which it does not read: an address that a management network already covers gets an entry all the same (harmless; a site that passes the `network` row cannot have its load balancer range there) |
| A new tenant zone without DHCP | Every save of the settings | none at the save; `SDN.Allocate` when the zone is made, as before | When the provider's default zone is a `simple` zone, its config says nothing about DHCP and Proxmox has no such zone yet, the zone's config gets `"dhcp": false`, so the first tenant network creates the zone without a DHCP server. A zone that exists is never changed, and a config that says `"dhcp": true` stays. When the zones cannot be read nothing is written and the answer says so (`siteChanges.problem`) |

**When `capmox@pve!capi` or `csi@pve!csi` already exists** and the script is the one without an
installation, `pveum user token add` fails, the script prints
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
| Network | Where it comes from: an existing bridge (`mode: bridge`, the default; with an optional VLAN tag, 1 to 4094) or a vnet the install makes and the node routes (`mode: routed`, one node only; the bridge field then names the new vnet). Then the IPv4 network (/8 to /29), gateway, one to three DNS servers, optional MTU (1280 to 9000). [The three forms](#the-platform-network-bridge-vlan-tag-or-routed). |
| Seed | Node, address, cores, memory and disk; up to 8 SSH public keys (optional, for emergencies: ProxCloud never uses SSH); 1 to 16 IPv4 networks that may reach the seed's API (`apiSources`), at least the address ProxCloud reaches it from. |
| Start from | A preset: **Homelab**, **Standard** or **Custom**. It fills the nodes, counts and sizes below and nothing else; every field stays editable, and what is saved is the fields ([Presets and the control plane count](#presets-and-the-control-plane-count)). |
| API address | The cluster's API endpoint, held by kube-vip on a control plane machine. |
| Node range | `first-last`: the addresses Cluster API gives the machines. It must hold every machine, one more for the control plane's rolling update (so 2 addresses for one control plane machine, 4 for three), and one more per group and node. |
| Load balancer range | `first-last`: the MetalLB pool tenants reach (tenant cluster APIs, database addresses). It must be outside the provider's blocked networks and the nodes' management networks. |
| Control plane | Node, size and the number of machines: 1 (the default) or 3, all on that one node. From 1 to 3 can be saved on an installed platform; once a run has built three, 3 to 1 is refused. Three need a platform release whose cluster chart is 0.1.14 or newer, which this release's is: on an installation of an older one (0.8.x, until it is upgraded) the save refuses 3 (`400`), and the field says why. |
| Node groups | Exactly `system`, `kamaji` and `dbaas`. For each: 1 to 16 nodes, a count of at least one machine per chosen node (at most 64), and a size. One MachineDeployment is made per group and node. |
| Tenants: who gets managed Kubernetes and databases | `spec.tenants.attach`: `all` (the projects that exist and every new one), `new` (new projects only) or `none` (you attach the providers later). The form preselects `new` on a site that has projects, so that nothing changes for tenants who are already there, and `all` on a site with none; when the projects cannot be counted it preselects `new`. Can change after the install. [What a run does with it](#what-a-tenant-can-use-after-the-install). |
| Access: ArgoCD address and operator networks (optional) | `spec.access.argocd` `{address, sources[]}`: both or neither. The address is one IPv4 inside the load balancer range, the sources 1 to 16 IPv4 networks, never `/0`. With them the platform cluster's ArgoCD answers at `https://<address>` for those networks after the next apply. Can be set, changed and cleared after the install. [What it does and who can reach it](#an-address-for-the-platform-clusters-argocd). |

The gateway, the seed address, the API address, the node range and the load balancer range must not
share an address. The page checks these rules before saving; the API checks them again and adds what
only it can read (overlap with `cidrPool`, sandbox and public networks, and that no seed API source is
a tenant network).

Fixed after the first successful install (`409 fixed_fields`): the name, the network, the seed's node
and address, the API address, the control plane's node and the set of node groups. Sizes, counts, the
nodes of a group, the ranges, the stores, the SSH keys, the API sources and the ArgoCD address with
its operator networks can change; **Apply this release** applies them.

The control plane's node is fixed because every control plane machine is on it: another node means
each of them made anew on that node, which no lab has run. To move it, remove the installation and
install again. An installation on which a build of main before this rule saved another control
plane node, without applying it, cannot be put back through the API either, and its next apply
would build the control plane on the saved node: put the node back before you upgrade, or the
stored record needs a correction by hand. A group's nodes are not fixed. A node you add gets machines of the group. A node you
take away loses its machines of the group at the next apply: a volume on a store that is local to
that node stays there, and a pod that uses it does not start on another node. The API cannot refuse
that safely, because it does not record on which nodes a run built machines; it changes a group's
nodes only when the request names them. Not run on a lab.
The provider of an installation never changes. No route deletes a draft.

A save is refused during a run (`409 run_active`), and when someone else saved since the page read
the settings (`409 rev_conflict`: the page reads them again).

### When a router translates the platform network

The cluster firewall rule the install writes (`proxcloud:pve-api:<installation id>`) accepts TCP to
the Proxmox API port from the seed's address and the node range, to the API host of the settings.
It matches a packet only when the node sees those addresses as the source. That holds when the
platform network reaches the node untranslated: the node routes it (mode `routed`), or your router
forwards it without NAT.

When the router between the platform network and the node translates that traffic (masquerade or
SNAT toward the nodes' network), the node sees the router's address. The rule is then in place and
matches nothing, and the Check row `pve-api-rule` still reads `ok`: it reads the rule, not the
traffic. With the datacenter firewall on, add a rule of your own for the translated address
(Datacenter, Firewall, Add: direction in, action ACCEPT, protocol tcp, destination port the API
port, source the router's address on the nodes' network), or stop translating that path. ProxCloud
does not write or check that rule. A router's address inside the nodes' own network is usually
accepted already, because Proxmox's default management set is that network (read in Proxmox's
documentation, not tried).

Found on a lab on 2026-11-19, where a router masquerades the platform network: the rule was
written, the row read `ok`, and every call arrived from the router's address. Whether the rule
takes effect on an untranslated path with a firewall that drops was not run on a lab.

### Presets and the control plane count

**Presets.** "Start from" at the top of step 1 fills the form. A preset is not stored: the saved
settings name every node, count and size, and an apply never asks which preset they came from.
A preset is a start for a new installation: on an installed platform the page does not offer it,
because it would change every machine's size and so replace every machine.

| Preset | Control plane | Seed | Node groups | Platform network |
|---|---|---|---|---|
| **Homelab** | 1 machine, 2 cores, 4 GiB | 2 cores, 4 GiB | one machine per group, 2 cores, 4 GiB, everything on the seed's node | While no bridge is named: `routed`, vnet `pcplat0` ([why](#the-path-from-tenants-to-the-platform)), in the page on a site with one node, through the API when every machine of the spec is on one node (the API does not count the site's nodes). Otherwise unchanged |
| **Standard** | 3 machines on one node, 2 cores, 4 GiB each; 1 when the release builds one, or when the page reads less free memory on the node than three need | 2 cores, 6 GiB | two machines per group, 4 cores, 8 GiB; on a site with several nodes each group on two nodes | unchanged |
| **Custom** | nothing is filled in | | | |

The sizes are the form's own: Homelab is the smallest machine the API takes, Standard what the form
has always started from. Disk sizes are left as they are (30 GiB for the seed and the control
plane, 40 GiB for a group machine).

The API takes the same choice as `preset` (`homelab`, `standard`, `custom`) next to `spec` on a
create, or on an update of an installation that is not installed yet. On an installed platform the
API refuses `homelab` and `standard` (`409 fixed_fields`), as the page switches them off, and a
spec that leaves out the control plane or the node groups keeps the stored ones: nothing is filled
from a preset or a default on a platform that runs. Up to this fix the API took a preset there and
put its node, sizes and counts where the spec named none (found on a lab on 2026-11-19; the fix is
covered by tests, not run on a lab). On a draft a preset fills only what the spec leaves out: a value the caller gives always stays,
a node nobody names is the seed's, and the answer carries the whole spec. `standard` through the API
asks for three control plane machines where the installation's release builds them and for one
where it does not; it does not read the node's memory, so the Check row `capacity` then says
whether the node has room.

**How many control plane machines.** The count is the platform cluster's own control plane, not a
tenant's and not the seed. One is the default.

Three machines give:

- an API that keeps answering when one control plane VM is lost, stopped or replaced, and during an
  update of the control plane, which with one machine has a gap.

A lost control plane machine is not replaced by itself: the chart's MachineHealthCheck covers the
node groups, not the control plane. Replacing one by hand (deleting its Machine on the seed, after
which Cluster API makes a new one) was not run on a lab.

Three machines do **not** give protection against the loss of the Proxmox node or of its disk. All
three are on one node and on one store: the cluster chart pins the control plane's machine template
to a node, CAPMOX v0.9.2 has one template for the whole control plane, and Proxmox moves a clone to
another node only when the template and the new disk are on shared storage. A spec that names
several nodes for the control plane (`controlPlane.nodes`) is refused with that reason, also on a
site with shared storage: this release does not build it. Three etcd members also write three
times to the one store. On a store where a single write can take seconds (the guide's
[What no check reads](#what-no-check-reads) records one of 19.5 s) three may be less stable than one.

**The node needs memory for one machine more than the count.** A rolling update makes the new
machine before it removes an old one. CAPMOX refuses a clone when the node's memory, less the
configured memory of every VM and container on it, running or not, is smaller than the machine
asks for, and it never tries that machine again. A node that holds exactly its machines therefore
installs and then fails its first update for good. The Check row `capacity` fails before the first
install when a node has no room for its machines and the control plane's one more. A node group's
update makes one more machine of the group on each of its nodes as well; the row counts those too
and only **warns** when they do not fit. An upgrade that replaces the machines takes one unit at a
time and decides per unit, when its plan is read, whether the node has room for one more machine
of that unit or the machine is deleted first
([The platform cluster's machines](#the-platform-clusters-machines)); every other upgrade replaces
no machine (it stops with `unexpected_rollout` if it would). So the sum of all spares is never
needed at once, and not at the install. Homelab therefore passes the row on a node with 24 GiB that can be
reserved (five machines of 4 GiB and the control plane's spare), with a warning below 36 GiB.

**Changing the count.** From 1 to 3 on an installed platform: set it in step 1, save, and press
**Apply this release**. The run adds two machines and replaces none (the count is not part of the
machine template), and its stage 7 waits until all three run. Step 4 lists the count as a change
(`controlPlane.replicas: 1 → 3`). Where the control plane's node has no room for four machines
(three and the one more of an update), the save is still accepted, the Check row `capacity`
**fails**, step 4 shows that row's message with the numbers and **Apply this release** is off; an
apply started through the API stops at its first stage (`preflight_failed`) before it changes
anything. Free the memory or go back to 1, which is allowed for as long as no run built three. Do not combine it with an upgrade:
an upgrade run that finds a changed control plane stops with `unexpected_rollout`. A 3 that was
saved and not applied goes back to 1: the rule reads what a run built, not what is saved. Stage 7
records the count in the installation's status (`status.clusters[].controlPlaneReplicas`) just
before it hands three to the cluster, also when that run then fails; from then on 3 to 1 is
refused (`409 fixed_fields`): Cluster API would take two etcd members out one after the other, with
two members left one slow or lost member stops etcd, and no lab has run that step. A platform a
run built with three goes back to one only by Remove and a new install. On a platform whose
release builds one machine, 3 is refused at the save (`400 invalid_installation`), so it cannot be
stored where every apply would then fail.

**Sandboxes (`spec.sandbox`): built by the code, offered by no release yet.** The settings take
the block, the run builds it, and every installation is still refused it: the block needs a BOM
that names the sandbox chart, and no BOM does until the release that pins it. Until then the save
answers `400 invalid_installation` ("the platform's BOM … has no sandbox chart") and the Check's
`spec` row says the same. A spec without the block means "no sandboxes" on a new installation. On
an installed platform a PUT without the block keeps the stored one, so a console from before the
block never switches sandboxes off: off is the explicit `placement: off`. It is not the field
`layout`, which is about the two services of today and takes `shared` only.

```json
"sandbox": {
  "placement": "shared",
  "nodes": ["pve1"], "count": 1, "cores": 2, "memoryMiB": 4096, "diskGiB": 40,
  "nodeRange": "192.168.140.64/28",
  "dns": ["1.1.1.1"],
  "proxyAddress": "192.168.140.150",
  "domain": "sbx.example.org",
  "tls": {"mode": "selfsigned"},
  "system": {"nodes": ["pve2"], "count": 1, "cores": 2, "memoryMiB": 4096, "diskGiB": 30}
}
```

| Key | Meaning |
|---|---|
| `placement` | `shared`: a node group `sandbox` of the platform cluster. `separate`: a cluster of its own, `pc-sandbox`, whose control plane runs as pods on the `kamaji` group. `off`: no sandboxes; nothing else of the block is read. |
| `nodes`, `count`, `cores`, `memoryMiB`, `diskGiB` | The machines that run sandboxes, with a node group's bounds (2 cores, 4096 MiB, 20 GiB at least). |
| `nodeRange` | The machines' own addresses: **one aligned IPv4 CIDR** inside the platform network (`192.168.140.64/28`, not first-last). It holds neither the gateway, the seed, the API address nor an address of `nodeRange` or `metallbRange`, and it has an address for every sandbox machine and one more per node. |
| `dns` | Up to three resolvers of the sandbox machines; none takes the platform network's. |
| `proxyAddress` | Where sandbox-proxy answers. `shared`: an address of `metallbRange` (not ArgoCD's). `separate`: a host address of the platform network outside every range, which the sandbox cluster's own MetalLB announces. |
| `domain` | A sandbox answers at `https://<port>-<id>.<domain>`. |
| `tls.mode` | `selfsigned` (also what an absent block means): the run makes one certificate for `*.<domain>`, keeps its key sealed and shows the certificate in `status.sandbox.certificate`. `secret` (the operator's certificate) is refused at the save: no route stores one yet. |
| `system` | `separate` only, and required there: the machines for sandbox-proxy and the sandbox controller, which never share a machine with a sandbox. |

**How many sandboxes a machine holds.** By CPU a machine of n cores holds about n-1 sandboxes of
one vCPU: the node's own pods ask for about one core. A machine of 2 cores holds ONE (measured on
a node of 2 vCPU on 2026-11-21, where an idle sandbox took 35 to 50 MiB of memory). Take 4 cores
or more for several; the plan's `sandbox` row says the figure for the size chosen. The presets
fill a block the settings hold and never add one: Homelab `shared` with one machine of 2 cores,
4 GiB and 40 GiB (one sandbox: what a lab of 5 GiB nodes has room for), Standard `separate` with
two machines of 4 cores, 8 GiB and 40 GiB and a system machine of 2 cores, 4 GiB and 30 GiB.

**The range and the provider.** The provider fences the range as a sandbox network
(`config.network.sandboxCidrs`), and its isolation refuses a sandbox network that overlaps the
tenant pool or a blocked network. It answers that refusal to every network create of every tenant,
so one bad range would break tenant networks provider-wide. The save and the Check's `network` row
therefore refuse a range that overlaps `cidrPool`, a public network, an entry of `blockedCidrs` or
a management network, and a provider that already names another sandbox network. The save never
adds the range to `blockedCidrs`, and the Proxmox API rule never names it as a source.

**What a run writes.** In stage `cluster`, before the platform cluster's Application names the
pool: the Proxmox pool `proxcloud-sbx` and what opens it, both written by the portal token with the
rights the setup scripts gave it for tenant pools
([proxmox-permissions.md](proxmox-permissions.md#sandboxes-on-the-managed-platform)). `shared`: one
ACL row, `capmox@pve!capi` with `ProxCloudCAPI` on `/pool/proxcloud-sbx`; the group is then built
like the three others, with its machines in that pool, addresses from `nodeRange`, the taints
`proxcloud.io/sandbox` and `proxcloud.io/unsecured` in place of the group taint, and the
proxmox-csi node plugin kept off it. In stage `workload-platform` the run writes the Secret
`sandbox-system/sandbox-proxy-tls` and then syncs the sandbox chart (Application
`pc-platform-sandbox`), which installs runsc on the group's nodes and puts sandbox-proxy on the
`system` group. `separate`: a token `capmox@pve!pc-sandbox`, minted as a tenant cluster's with the
sandbox pool, the template pool, the platform's boot and ISO stores, `/nodes` and the platform
network's vnet in its scope; after the last wave the namespace `pc-sandbox` on the platform
cluster, the Secret `pc-sandbox-pve` with that token, and the Application `pc-sandbox` (the tenant
cluster chart, with the sandbox chart as its add-on). The stage is done when every machine of the
cluster is Running. A new sandbox machine takes sandboxes only once the provider sync has written
its VM firewall and taken the `unsecured` taint off.

**Changing it.** Nodes, count, sizes, `dns` and `domain` change with an apply. Fixed once a run
built it (`409 fixed_fields`): `placement`, which goes only to `off`, `nodeRange` and
`proxyAddress`. From `shared` to `separate`: `off`, apply, then `separate`. With `off` the run
deletes the Application (the sandbox chart's with everything it deployed, or the cluster's with
its machines), the group, and once the pool holds no VM the ACL row or the token and the pool.
**Remove** deletes `pc-sandbox` before anything of the platform cluster, because its machines
need the platform cluster's Cluster API and Kamaji, and the access and the pool at the end.

**Upgrades.** The sandbox chart is the one component an upgrade adds to an installed platform
(plan class `added`): it is rendered only for settings that ask for it, so for every other
installation the move changes nothing of it. A `shared` group rolls with the platform cluster's
machines as its last unit, and every sandbox on a replaced machine ends. An upgrade that replaces
the machines is refused while the group is asked for and not built (build it first, or leave the
block out until the upgrade is done), and **while `separate` exists or is asked for**: no run
upgrades `pc-sandbox` in place. Set `placement: off` and apply, make the upgrade, then set
`separate` again: the cluster is built new. Its cluster chart stays the version it was built with.

**What `shared` gives up.** Said in full, because it is the one choice here that changes what a
tenant's own workloads are exposed to. The install page prints the same list and saves `shared`
only with a tick under it.

Code that escapes gVisor in `shared` is root on a machine of the cluster that runs kamaji-etcd
(the datastore of every tenant's Kubernetes control plane), every tenant's API server,
CloudNativePG and every tenant database, ArgoCD and the tenants' Cluster API controllers. From
there:

- **The pod network is open to it.** The machine is a Cilium peer: it must exchange VXLAN with
  every platform machine, and it can send frames with any source identity to any pod. Cilium's
  network policies do not stop that. What protects the targets is their own authentication: client
  certificates on kamaji-etcd, passwords on the databases, tokens on the API servers.
- **The Cilium agent's credential on that machine** may list and watch pods, services, endpoints,
  nodes and every network policy of the cluster, and create and update CiliumIdentity and
  CiliumNode objects. Peers build their tunnel map from CiliumNode objects, so a forged one for
  another node pulls that node's pod traffic to the escaped machine: traffic between pods that is
  not encrypted with both ends authenticated can be read and forged, not only disturbed. By default
  the credential reads Secrets in `cilium-secrets` only.
- **The other credentials on the machine:** the kubelet's (held to its own Node and the Secrets of
  the pods bound there by NodeRestriction), the network guard's service account (it may read every
  pod's spec and every node's stats) and kube-proxy's.
- **No egress address of its own.** Sandboxes leave under the site's public address: abuse from a
  sandbox is abuse from your address.
- **What still holds:** the VM firewall of the machine (below) keeps the Proxmox API, the seed, the
  management networks, tenant networks and every address of the platform network that is not a
  machine of the cluster closed to it, and pins it to its own address. Of the cluster's own
  machines it reaches the pod tunnel, the health port, ICMP and the Kubernetes API port, nothing
  else. The portal's two identities on the cluster are narrowed by admission policies (below).
- **The portal token.** It holds its platform role on the sandbox pool, so a stolen portal token
  reaches a node of the platform cluster through a sandbox machine's console, guest agent or disks,
  where on `proxcloud-system` it holds `NoAccess`.

`separate` limits the same escape to the sandbox cluster: other tenants' sandboxes on it, its own
control plane's API and the internet. `shared` is the homelab choice: one operator, no outside
tenants, too little memory for a second control plane and a system machine. For a site with
outside tenants it is allowed, with this text read, the provider shown as "reduced isolation" on
the Ready card, and the release gates still required.

#### The sandbox provider

**What a run writes on the Proxmox provider.** In stage `cluster`, before a sandbox machine
exists, the run writes on the installation's Proxmox provider, in one write:

- `config.network.sandboxCidrs`: the one entry `nodeRange`. The next provider sync writes the
  isolation rules for it (tenant networks to and from the range, the range to the blocked and
  management networks).
- `config.sandbox`: `pool` (`proxcloud-sbx`), `nodes` (`[nodeRange]`), `dns`,
  `unsecuredMinAgeMinutes` (20), and what the platform network adds: `platformInstallationId` (the
  owner), `mode`, `bridge` and `vlan` (where the machines' NIC is: no vnet is looked up), and
  `deny` (the platform prefix, so that a platform network that is not RFC 1918 is as closed to a
  sandbox machine as one that is). `shared` adds `peers` and `kubeletSources`, both the cluster's
  `nodeRange`. `separate` adds `loadBalancers` and `proxy` (the proxy address, port 443).

In stage `register` it adds `secret.sandboxKubeconfig` (sealed): the identity
`proxcloud-sandbox-firewall` of the sandbox chart, which may read and patch Nodes, read the
RuntimeClass and ask the API server one dry-run question as a node identity (the NodeRestriction
gate), and nothing else. Until then the sync writes no VM firewall and logs that the kubeconfig is
missing, and every sandbox machine keeps its `unsecured` taint.

One provider fences one sandbox pool and one sandbox network. A `config.sandbox` that is not this
installation's (the installer's sandbox cluster has no owner key) is never overwritten: the save
and the Check's `network` row say so, and a run stops at `sandbox_refused` before it makes a
machine. Remove that sandbox cluster and its settings first.

**The VM firewall of a sandbox machine (layer 1).** The provider sync fences every VM of pool
`proxcloud-sbx`, and no other: a machine of the platform cluster is never selected, whatever its
bridge or tag. Both policies are DROP and every rule is an ACCEPT, so a missing rule closes more.

| | `shared` | `separate` |
|---|---|---|
| Cilium tunnel (UDP 8472), health (TCP 4240), ICMP | both ways with the sandbox range and the platform cluster's `nodeRange` | both ways inside the sandbox range |
| Kubernetes API | TCP 6443 to the API address, and to every address of the cluster's `nodeRange` | TCP 6443 and 8132 (konnectivity) to the control plane's address of `pc-sandbox` |
| Kubelet (TCP 10250) | from the cluster's `nodeRange` | inside the sandbox range |
| MetalLB memberlist (7946) | none | inside the sandbox range |
| sandbox-proxy | none: it runs on the `system` group | TCP 443 to the proxy address, from any source |
| DNS | UDP and TCP 53 to `dns` | the same |
| Internet | everything outside the private, CGNAT and link-local ranges, the platform prefix, the nodes' own addresses, the management and blocked networks and the tenant pool | the same |

- TCP 6443 to the whole `nodeRange` in `shared`: the Cilium agent, kube-proxy and the network
  guard reach the API through the `kubernetes` Service, which kube-proxy rewrites inside the VM to
  a control plane machine's own address before the VM firewall sees the packet. Which addresses
  are control plane machines changes with every roll.
- The proxy address takes any source in `separate`: there is no entry appliance on a managed
  platform, so a client's own address arrives. The address must not be reachable from a network
  you do not want sandboxes served to; that is your router's rule.
- The IP filter of each machine admits its own address once its Node has reported it (and in
  `separate` the proxy address, which any machine of the sandbox cluster may announce). A sandbox
  machine cannot answer for the API address, a platform machine or a MetalLB address.
- A NIC that is not on the platform bridge with the platform's VLAN tag gets an empty filter.

**The portal's identities on the cluster.** The sandbox chart makes two service accounts and the
run stores a kubeconfig for each: `proxcloud-sandbox` on the sandbox provider and
`proxcloud-sandbox-firewall` on the Proxmox provider. Both name the same API server (the gates
refuse otherwise): the platform cluster's API address with `shared`, the control plane of
`pc-sandbox` with `separate`, whose admin kubeconfig the run reads once from the Secret Kamaji
keeps and stores nowhere.

With `shared` the portal's identity holds, cluster-wide, only namespaces, RoleBindings, the right
to bind one ClusterRole (`proxcloud-sandbox-namespace`) and a read of Nodes. Everything else (pod
reads, logs, exec, Sandbox objects, network policies, quota) it holds only in a project namespace
`sb-<projectId>` where it has bound that role, which it does when it creates the namespace.
Admission policies of the chart hold it to `sb-*` namespaces labelled as a tenant's, to that one
role and to itself, hold the firewall identity to Nodes of the sandbox group, and hold every pod
of a tenant namespace to the gVisor RuntimeClass with no host access. The portal lists across
projects namespace by namespace there. A stolen token then reads, execs and writes inside `sb-*`
namespaces only. With `separate` the identities are the installer's cluster-wide ones: that
cluster runs sandboxes alone.

**The provider.** Stage `register` creates "\<name> Sandboxes" after the two tenant providers,
**DISABLED**: envd on with `domain`, the templates `base`, `code-interpreter-v1`, `python` and
`mcp-gateway`, each with the image this ProxCloud release pins by digest
(`deploy/templates/images.yaml`), the portal's default limits, an egress cap of 100 Mbit/s, the
abuse check in `alert` mode, rotation off, no sandbox registry, `mode` and
`proxmoxServiceId`. `spec.tenants.attach` is applied to it as to the two others, and recorded
apart (`status.sandboxAttach`): "all projects" waits while the provider is disabled, and that wait
does not make the next apply attach the two others again. A later apply adopts the row and keeps
what you changed (name, status, templates, limits); it writes the kubeconfig, `mode` and `domain`
again, which follow the settings. The pinned
images move with a ProxCloud release and no other way: the template update job of the installer
path does not run here. The sandbox machines pull `base` and `code-interpreter` ahead; a first
start of another template waits for its pull.

**Going live.** A run never sets the provider live. On its page under **Cloud providers**, choose
a status other than Disabled and save: the save runs the release gates and is refused while one
does not hold. The Ready card reads the same gates without changing anything (**Check the release
gates**).

| Gate | Holds when | On a `shared` cluster |
|---|---|---|
| `noderestriction` | the API server refuses a node identity a `node-restriction.kubernetes.io/` label on its own Node | the same |
| `isolation` | the cluster firewall holds the isolation rules for `nodeRange` | the same; with a bridged platform network those rules see routed traffic only, and the VM firewall is what holds inside the network |
| `fence` | every Node that may take a sandbox is a VM of the sandbox pool whose firewall reads back as written, pinned to the Node's address | first the RuntimeClass `gvisor` must select exactly the sandbox group (`node-restriction.kubernetes.io/proxcloud-node-group: sandbox`) and tolerate `proxcloud.io/sandbox` alone; then only that group's Nodes are asked. A class changed by hand fails the gate |
| `network-guard` | Cilium leaves the CNI config to the network guard and its heartbeat is fresh, on every schedulable node | on the sandbox group's Nodes alone, by the label a kubelet cannot set |

A new sandbox machine passes `fence` about 20 minutes after it joined (the join token on its disk
lives 15), once a provider sync has fenced it and taken its `unsecured` taint off. The gates are
checked again, cached for a minute, before every sandbox create.

**The job scheduler.** The sync that fences a sandbox machine, takes its taint off and puts it
back on a machine that lost its fence runs on the scheduler
(`PROXCLOUD_JOBS_SCHEDULER_ENABLED`), as do the minutely sandbox read and the abuse check. One
operator sync (`POST /api/v1/admin/job/external-service/sync`) fences once, and nothing watches
afterwards. So a platform's sandbox provider is refused "live" while the scheduler is off
(`409`), the Check's `scheduler` row says so while the settings ask for sandboxes, and the Ready
card repeats it. The same switch starts charging usage: the price rule for sandbox minutes holds a
placeholder rate until you set one (or 0). The installer's sandbox providers are not asked for
the scheduler.

**Switching sandboxes off.** The save of `placement: off` is refused (`409
sandbox_provider_live`) while the sandbox provider is not disabled, and so is the run: every
running sandbox would end without notice. Disable the provider, let its sandboxes end, then save
and apply. Once the machines are gone and the pool is empty, the run takes `config.sandbox`, the
`sandboxCidrs` entry and `secret.sandboxKubeconfig` off the Proxmox provider, deletes the pool's
firewall groups and IP sets from the cluster firewall, then the ACL row or the token and the
pool. The sandbox provider row stays, disabled, and is adopted when sandboxes come back; a Remove
disables it with the two others, and a new install does not give it its status back.

**The certificate.** With `separate` the run writes `sandbox-system/sandbox-proxy-tls` inside
`pc-sandbox` as soon as its control plane answers. The certificate is self-signed for
`*.<domain>`; the Ready card offers it for download, and a client adds it to its trust store
(`SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS` for the JavaScript SDK). `*.<domain>` must resolve to the
proxy address where clients are: a DNS zone, a resolver entry on a workstation, or a wildcard
service that maps a name to the address it contains.

**Not in this release's code:** an operator's own certificate (`tls.mode secret` is refused at the
save); waking a paused sandbox through sandbox-proxy (no wake URL is set); a sandbox registry,
custom template builds and template updates on the managed platform; a Check that an address of
the platform network outside the MetalLB range is free (the `sandbox` row reads the platform
cluster's Services only); the scheduler split into sandbox jobs and charging.

**Lab tests owed** (nothing of this section was run on a lab):

1. Install from Homelab on a one-node site and from Standard on a site with three nodes; the
   review's totals and the `capacity` row against what the nodes show.
2. A platform with three control plane machines on one node: stop one control plane VM and time
   until the API address answers again; a disk load on the store while watching kube-vip for
   "lost leadership"; the same with `vipLease` 60/40/5 and 15/10/2.
3. From 1 to 3 on a ready platform by an apply: two machines added, none replaced, stage 7 waits
   for three.
4. A node with room for the count and not for count + 1: the `capacity` row fails before the
   install; with the row ignored on a scratch cluster, a control plane rollout whose surge machine
   gets `VMProvisionFailed`, to confirm what the row is there to prevent. A node with room for the
   machines and not for a group's spare: the row warns, the install passes, and a change of that
   group's size shows what happens to the refused machine (whether worker repair replaces it).
7. From 1 to 3 where the apply fails after stage 7 recorded three: what the operator can do, since
   3 to 1 is then refused.
8. From 1 to 3 saved on a ready platform whose control plane node has no room for four machines:
   the plan lists `controlPlane.replicas` 1 to 3 and blocks with the row's numbers, the Check row
   `capacity` fails, an apply through the API fails `preflight_failed` at stage 1 with no machine
   made; back at 1 the Check is as before. And with room: the change is listed, nothing blocks.
5. The `capacity` row's count against CAPMOX's own (`cannot reserve ... of memory on node ...`) on
   a node with stopped VMs, containers and templates.
6. Out of this release, for the one that spreads the control plane: one template on a shared
   store, `allowedNodes` of three nodes and `storage` a shared store, and etcd's write times there.

### Step 2: Proxmox identities

[Section 3](#3-proxmox-identities). Paste the command into a root shell on one node, then paste its last
line (`PC-ENROLL:…`) into the page and press **Store the secrets**
([The root command of an installation](#the-root-command-of-an-installation)).

### Step 3: Check

`POST …/preflight` reads Proxmox with the portal token and the two stored tokens and changes nothing.
**Install** runs the same checks as its first stage.

| Check | Fails when | What to do |
|---|---|---|
| `flags` | Template updates are off, or no GitHub integration is saved | `--managed-services`; save a GitHub token |
| `spec` | The saved settings break a rule, for example an import store that is the boot store. Or they name a VLAN tag and the platform's BOM names a cluster chart older than 0.1.14, which cannot tag a NIC | Correct step 1 and save. For the tag: a bridge or vnet that carries the VLAN untagged, until a release whose BOM names chart 0.1.14 |
| `bridge` | Mode `bridge`: a node the settings place a machine on has no bridge or vnet of that name, or did not report its networks. Mode `routed`: Proxmox already has a vnet of that name that the installation did not make, or a node has an interface of that name, or a node's interfaces could not be read. On an installed platform (the network is fixed and the machines run on it) the same finding is a **warning**, so that no apply stops on a read that went wrong | Correct the bridge or the nodes in step 1; apply a vnet you made (Datacenter, SDN, Apply). Mode `routed`: name another vnet |
| `network` | The platform network overlaps a tenant, sandbox or public network, a seed API source is a tenant network, the load balancer range is inside a blocked or management network, or a node did not report its networks | Correct the network in step 1 |
| `isolation` | The seed, the API address or part of the node range is in no blocked or management network, or the cluster firewall's isolation rules are not the ones a sync writes | Press **Save settings** in step 1: the save adds the CIDRs the message names to the provider's blocked networks. By hand: add them to **Networks tenants must not reach** on the provider's page (`blockedCidrs`). The message lists the smallest ranges that are missing; one network that covers them does the same (`10.90.0.0/26` for a seed on `.5`, an API address on `.10` and nodes on `.20` to `.60`), as long as the load balancer range stays outside it. Then **Run a provider sync, then check again**: with the installation saved (step 1), the sync writes the provider's rules whether or not it has a project. Check that the datacenter firewall is on. Five messages name a cause no sync of a provider without a project repairs: `cidrPool … overlaps the management network …` (choose another `cidrPool`), `rules … are for another pool`, `rules … drop … to …, which this provider's cidrPool, blockedCidrs and sandboxCidrs do not`, `rules … have an ACCEPT …` or `have no ACCEPT …` and `rules … are not this provider's set …, and a project's rules for cidrPool … are in place` (another portal or provider shares the rules: give this provider its own `poolPrefix`; the provider's own older rules: delete them under Datacenter > Firewall and sync). ([More](#the-isolation-check-and-the-provider-sync)) |
| `tenant-network` | Never. It reads the provider's default zone in Proxmox and **warns** when the zone has DHCP, when it does not exist yet and is configured as a `simple` zone without `"dhcp": false` (ProxCloud then creates it with DHCP), or when the zones could not be read: a tenant Kubernetes cluster create on a network of such a zone is refused with a `400` (not on a Kubernetes provider with shared workers, `cluster.shared`, which uses no tenant network). The row is `ok` for a zone without DHCP and absent when the provider names no zone | Nothing for the install or for databases. Before tenants create clusters: give the provider an EVPN or a VLAN zone as its default zone, or a `simple` zone without DHCP (Cloud providers, `config.network.zones` and `defaultZone`; [Prerequisites](#2-prerequisites)) |
| `tenant-path` | Never, and absent when the provider names no zone. It reads the layout and **warns** when a node that translates tenant traffic (a `simple` default zone: the node of the tenant's VM; an EVPN default zone: its exit nodes) also hosts a platform machine and does not route the platform network itself, or when Proxmox could not be read: a tenant's TCP connection to the platform network is then dropped on that node, tenant Kubernetes workers never join and tenants cannot reach managed databases. It opens no connection: `ok` is not proof, and a leg added on a node by hand is not seen. It is `ok` for a platform network of mode `routed`. **Test the tenant path** below the rows opens the path on an installed platform. For a VLAN default zone that test is refused (`409 vlan_zone`), so the row names what to do instead: `curl -k -m 5 https://<a tenant cluster's API address>:6443/healthz` from a tenant's server | Nothing for the install. Before tenants create clusters or databases: one of the two layouts of [The path from tenants to the platform](#the-path-from-tenants-to-the-platform) |
| `csi-vmid` | Never. It **warns** when a guest of the cluster has VM id 9999, the id proxmox-csi names its volumes after, or when that could not be read | Nothing for the install. Never destroy VM 9999 with "Destroy unreferenced disks owned by guest" ([VM id 9999 and the CSI volumes](#vm-id-9999-and-the-csi-volumes)) |
| `sandbox` | Present while the settings ask for sandboxes. The portal token lacks, on `/pool/proxcloud-sbx`, `Pool.Allocate` or a privilege of `ProxCloudCAPI` (it holds them through its platform role on `/pool`), or with `separate` the mint role on the platform's boot or ISO store, `/nodes` or the platform network's zone or vnet; the provider's `poolPrefix` is the start of the pool's name; or the pool holds a VM without this installation's tag. The datacenter firewall is off (Proxmox then enforces no VM firewall, so no sandbox machine is ever opened to sandboxes); a LoadBalancer Service of the platform cluster other than `sandbox-system/sandbox-proxy` holds `proxyAddress`. A warning when the ACL row of a built `shared` group is gone. A sandbox pool or sandbox network of another owner on the provider fails the `network` row, and the `capacity` row warns for small `kamaji` machines while a `separate` cluster is not built (its control plane takes about 0.6 GiB there) | Run the managed-services script again as root on a node (it names no sandbox pool: it restores the rows of the portal token that these rights come from); move the foreign VM to another pool; an apply writes a lost row again; switch the datacenter firewall on (Datacenter, Firewall, Options), after reading what else it puts in force; choose a free address |
| `capacity` | Before the first install, when a node has less memory than the settings place on it; absent while the CAPI token is not stored. It reads the nodes and their VMs with the CAPI token and counts as CAPMOX counts before each clone: the node's memory less the configured memory of every VM that is not a template and every container on it, running or not (this installation's own machines are part of the plan and are not counted twice). Against that it puts the seed, the control plane machines plus one, and each group's machines on the node: the one more is what a rolling update of the control plane makes before it removes an old machine. A node that holds those and not one more machine of each group on it as well **warns** and does not stop the install. On an installed platform a finding about what a run already placed **warns**, and so does a node whose memory could not be read; the row **fails** there only when the settings ask for more control plane machines than a run built and their node has no room for them (the message names both counts). A VM the CAPI token cannot see is not counted, so `ok` is the scheduler's answer, not a promise that the node has the memory free | Smaller or fewer machines, one control plane machine in place of three, other nodes, or free memory on the node (a stopped VM counts with its configured memory). Then save and check again |
| `credentials` | A token secret is not stored | Run the root command of step 2 on a node and paste the `PC-ENROLL` line it prints |
| `machines` | Never, and absent while the installation has no VM. It reads the installation's VMs with the CAPI token and **warns** with one line per VM that is stopped or that does not start when its node boots (`onboot` off) or that the token could not read (the others are still listed, and an apply still sets `onboot` on them), or when the pools could not be read | A stopped VM: start it in Proxmox, or `qm start <vmid>` as root on its node ([After a Proxmox node reboots](#after-a-proxmox-node-reboots)). `onboot` off: **Apply this release** sets it at the end of stage 7, or `qm set <vmid> --onboot 1` |
| `privileges:capi`, `privileges:csi`, `privileges:kubemint` | The CAPI token, the CSI token or the portal token lacks a privilege on a path; the message names both. For a VLAN tag or a routed network `privileges:capi` also reads `SDN.Use` on the path of the machines' NIC (`/sdn/zones/localnetwork/<bridge>/<tag>`, `/sdn/zones/pcplat/<vnet>`) | Run the script again. `privileges:kubemint` reads every path a tenant cluster's token gets a row on, not `/storage` and `/sdn/zones` alone: a line on `/storage/<store>` or `/sdn/zones/<zone>` means a row of the portal's own on that path hides the mint role, and the script adds the role next to each such row ([why, and what the row gives](proxmox-permissions.md#a-row-of-the-portals-own-on-a-store-or-a-zone)); until then a cluster create that uses the path answers `409`, and, as for every failed row, an install or apply run stops at its first stage (`preflight_failed`). A site that had such rows before v0.8.1 passed this row then and fails it after the upgrade: run the script again before the next run. A row the portal's user holds through a group is not found by the script; the same section gives the row to add. On the provider's `importStorage`, when the settings name another import store, the script does not write that row ([what to do](#when-the-providers-import-store-is-the-boot-store)) |
| `mint-scope` | The portal token holds `User.Modify` or `Permissions.Modify` on `/`, `/access` or `/access/groups` (a stolen portal token can then create an API token for any user of the cluster, `root@pam` included) or `Permissions.Modify` on `/pool` (it can then write any role on every pool). A site set up before the group `proxcloud-kube` holds `ProxCloudKubeMint` on `/access/groups` and `/pool`. Or: `capmox@pve` or `csi@pve` is not in the group `proxcloud-kube`, and Proxmox refuses every token create or delete for it | Run the script again: it grants `ProxCloudKubeToken` on `/access/groups/proxcloud-kube`, puts both users in the group and removes the old rows ([the commands](proxmox-permissions.md#an-existing-site)). If it still fails on a privilege, another row grants it: `pveum acl list \| grep -F '<the portal user>'` on a node, then `pveum acl delete <path> --users <user> --roles <role>` (or `--tokens`) for a row on `/`, `/access`, `/access/groups` or `/pool` with a role other than `ProxCloudPlatform`. For a user outside the group the message gives the line: `pveum user modify <user> --groups proxcloud-kube --append 1` |
| `token-scope` | `capmox@pve!capi` or `csi@pve!csi` holds a privilege on `/pool`, `/storage` or `/vms` as a whole, which reaches every tenant's VMs or disks. The script writes no such row; an older setup may have, and the script removes none | Run the `pveum acl delete` command the message gives, as root on one node, then check again. When the portal token cannot read the row (one on `/`, for example) the message gives `pveum acl list` to find it: the row to remove is one on the named path itself, or on a parent of it (`/` included) with propagate 1, with a role other than `NoAccess`. The list is not empty on a site the script set up, and its rows on the platform pools and stores and on `/` without propagate stay. When no listed row is such a row, remove none: the token was made without privilege separation and holds its user's rights (the users keep `/pool` and `/storage`): run `pveum user token modify <user> <token name> --privsep 1` (`capmox@pve capi`, `csi@pve csi`), then the script again. Leave the tokens' rows on the platform pools and on the boot and ISO stores |
| `pool-fence` | The portal token holds a privilege on `proxcloud-system` or `proxcloud-seed`, or the CSI token on `proxcloud-seed` | Run the script again with the provider's token id: it writes the `NoAccess` rows on both pools, and a missing row is the usual cause (the portal token's `/pool` rows pass down to them). Only if the check still fails, remove the row granted to that user or token on `/pool/proxcloud-system` or `/pool/proxcloud-seed` itself. Do not remove its `/pool` rows: every tenant operation needs them |
| `iso-store-visibility` | The CAPI token sees an ISO store other than the one in the settings. CAPMOX puts each bootstrap ISO on the first ISO store its token sees | The message names the stores seen and, for each row of the token that reaches another ISO store, the command that removes it (`pveum acl delete <path> --tokens 'capmox@pve!capi' --roles <role>`, as root on one node); run them and check again, and `privileges:capi` must stay ok. A row the portal token cannot read is found with `pveum acl list \| grep -F 'capmox@pve!capi'`: it is on the other store's path or on a parent of it with propagate 1 (no listed row is such a row: the token lacks privilege separation, see `token-scope`). Or change the ISO store and run the script again. When the extra store is the boot store, the message gives no command: the token needs its row there, so name another boot store or take `iso` out of its content (the `boot-iso` row) |
| `boot-iso-grant` | `capmox@pve` may not upload and delete on the ISO store | Run the script again |
| `boot-iso` | The ISO store is shared, takes no ISOs, or is not the only ISO store of the boot storage | Name a node-local ISO store |
| `placement` | A node an image goes to lacks the import store with `import` content or the boot store | Change the nodes or the stores |
| `boot-store` | Proxmox has no storage of the boot store's name, or it takes no disk images. It **warns** when the store also holds disks of VM ids that are not the platform's (tenant servers, VMs of your own, or volumes a deleted VM left), or when its content could not be listed: `csi@pve!csi` holds `Datastore.Allocate` on the whole boot store and its secret is readable inside the platform cluster ([section 10](#10-security-boundaries)). The disks are listed with the CSI token, the only one that sees them all, and the platform's own machines are told apart with the CAPI token, so before both tokens are stored the row says the disks are not read yet. The ids it names are those in the volumes' names (`vm-<id>-*`), not guests: an id that no VM or container of the cluster holds (asked with `GET /cluster/nextid?vmid=<id>`, which answers for every guest whatever the token may read) is named with "no VM has this id: a leftover volume", and such a volume can be deleted by hand once you know whose it was (found on a lab on 2026-11-19: `vm-129-cloudinit` read as "VM 129"; the mark is not run on a lab). Not counted: the installation's own machines, the templates of the pool `pc-templates`, CSI volumes (`vm-<id>-pvc-*`). A VM of that pool that is no template is counted like any other VM: a clone that kept its template's pool, a VM put there by hand, and the VM of a template smoke test or of a tenant path test while it runs (up to this fix every member of the pool was skipped, so such VMs were not listed; found on a lab on 2026-11-19, the fix is not run on a lab) | A missing store: name an existing one in step 1 (`pvesm status`). No disk images: `pvesm set <store> --content <its content>,images`, which the message spells out. The warning: a store of its own for the platform, on ZFS `zfs create <pool>/pcplat && pvesm add zfspool pcplat --pool <pool>/pcplat --content images,rootdir --sparse 1`, then name it in step 1 and run the root command of step 2 again. The install works without it |
| `seed-backup` | Never. Present once the installation has a seed VM. `ok` with the archive's time and store when the portal sees a `vzdump` archive of the seed VM younger than 24 hours. On most stores it cannot see one: Proxmox lists a VM's backups only to a token that holds `VM.Backup` on that VM or `Datastore.Allocate` on the store, and answers the others with a shorter list, not an error; the portal token is kept off the seed VM on purpose. The portal therefore sees the seed's archives on the provider's import store (with its own token) and on the platform's ISO store (with `capmox@pve!capi`) only. For every other backup store the row says that the portal **cannot see** backups of the seed VM there, never that none exist, and names what to look for in Proxmox; it is then `ok` (for information), and a **warning** while an upgrade that replaces the seed's k3s waits to be started. It also **warns** when it could read every backup store of the node and none holds an archive younger than 24 hours ([The seed's k3s](#the-seeds-k3s)) | Back the seed VM up. Where the portal cannot see the archive, look for it in Proxmox, and confirm it when you start such an upgrade |
| `argocd-access` | Present while the settings name an ArgoCD address. It **fails** on an address outside the load balancer range or on operator networks the settings refuse (the `spec` row says the same). It **warns** when an operator network contains the address of a Proxmox node, with the address and the network, when the nodes' networks could not be read, and when a stored operator network has host bits (`192.168.33.5/24`: Kubernetes 1.36 refuses it on a new Service; the row names the form to save). `ok` otherwise, with the address and the networks | Name operator networks no node is in, or clear both fields. On a site where tenant traffic leaves under the node's address (SNAT) a tenant VM arrives from that address, so a network that contains it lets every tenant reach the ArgoCD login page ([who can reach it](#an-address-for-the-platform-clusters-argocd)) |
| `pve-api-rule` | Never. `ok` before the first install (the install writes the rule) and when the rule is in place; it **warns** on an installed platform whose rule is missing, changed or disabled, or when the cluster firewall rules could not be read. The message spells the rule out and says when the datacenter firewall is off. The row reads the rule, not the traffic: the rule's sources are the addresses of the seed and the platform machines as the node sees them, so on a site whose router translates the platform network on the way to the node (NAT) the row is `ok` while the rule matches nothing ([what to add](#when-a-router-translates-the-platform-network)) | Nothing before the install. Installed: **Apply this release** writes the rule again, and so does an upgrade run; the plan of step 4 lists the rule while it is missing (`pve-api-rule`, kind `site`) |
| `images` | A BOM image is on the template update blocklist | A new installation: install a newer ProxCloud release. An installed platform stays on its release and its images, and no release moves it to other images yet (see [Upgrade between BOMs](#upgrade-between-boms): a BOM with other images is refused): the message says which case applies, and an apply that must import that image stays blocked |

A check that cannot be read fails. A warning fails nothing: the page then says "No check failed" with the
number of warnings and not "Every check passed", the answer's `ok` stays `true`, and a run's first stage
lists the row as an item in state `warn` and goes on.

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

Shows the release and its components (`GET /platform/bom`; for a platform that stays on the
release that installed it, that release's: `GET /platform/bom?sha256=<installed>`), what is created (one seed VM, one
control plane and the worker machines, the two providers), the cores and memory per node, and for an
installed platform the plan (`GET …/plan`). **Install** (later **Apply this release**) starts a run
(`POST …/runs {kind: apply}`). You can leave the page; the run goes on.

The plan of an installed platform also names what the run writes on the Proxmox site outside the
platform's clusters. An upgrade that replaces the seed's k3s lists three such entries for the seed
VM ([The seed's k3s](#the-seeds-k3s)). Every plan can have the entry `pve-api-rule` (kind `site`, stage `seed-vm`): the
[cluster firewall rule for the Proxmox API](#what-proxcloud-does-with-the-portal-token), listed when the
route reads the cluster firewall and does not find the rule as the settings name it. That is the
case on a platform installed by 0.8.x, which never had the rule, and after the rule was deleted or
changed by hand. The entry spells the rule out; the run adds that one `ACCEPT` rule (and replaces
a rule with the same comment that differs) and touches nothing else in the firewall. It is in the
apply's list and in the upgrade's, since both runs write it, and it never blocks. When the
firewall rules cannot be read the entry is left out and the Check row `pve-api-rule` says so.
Found on the production upgrade of 2026-11-19 (0.8.0 to 0.9.0): the run wrote the rule and the
plan had not named it. The entry is not seen on a lab yet.

For a platform on another release than this ProxCloud's, the step also shows the move to this
release: **Upgrade to release X** with every change and what it does, or "The platform stays on
release Y" with the reasons, or "The upgrade to release X has not finished"
([Upgrade between BOMs](#upgrade-between-boms)).

### After the install

A ready installation shows one card. What it shows was read on a lab once (2026-11-18, the build
`dev-03bc9f3` of main on a platform installed by v0.8.0), at the API and with `kubectl` 1.35.10: the
four addresses and the two provider rows against the installation, the platform kubeconfig and the
two ArgoCD commands on both clusters. The page itself was not opened in a browser on the lab, and
the row "Offers no Kubernetes version yet" was not seen (the lab's provider offers one).

**Endpoints.** Four addresses, all on the platform network:

| Row | From | What answers there |
|---|---|---|
| Seed API | `status.seed.apiServer` | The seed's k3s, `https://<seed address>:6443`. The seed's firewall lets in only the seed API sources of the settings |
| Platform cluster API | `status.clusters[0].endpoint` | The cluster's API, `https://<API address>:6443` (kube-vip) |
| Load balancer range (MetalLB) | `spec.clusters[0].metallbRange` | The addresses of pool `services`: tenant database endpoints. Tenants reach them |
| Node range | `spec.clusters[0].nodeRange` | The platform's machines |

**Providers.** The two rows stage 10 registered, each with a link, and what a tenant cannot use
yet. The page reads this from the provider rows (`GET /admin/service`) and the installation. The
next section says what each line means and what its button does.

#### What a tenant can use after the install

A green install was not usable by a tenant up to v0.8.2: the Kubernetes provider offered no
version, so every cluster create was refused with "this provider offers no Kubernetes version yet",
and neither provider was attached to a project. On the first production install an operator typed
the version into the provider and attached both providers by hand. From v0.9.0 the install does
both, as far as it safely can. None of it is verified on a lab.

**The Kubernetes version.** A tenant cluster's workers are clones of a node image template, one per
Proxmox node, which the provider names per version in `config.cluster.versions`
(`"1.35.10": "pve1=9212,pve2=9213"`, [managed-k8s.md](managed-k8s.md)). The install imports the
BOM's node image on every node the platform cluster uses (stage 2), but it does not publish it:
Template updates publishes only a copy whose smoke test passed, and a run's imports are not
smoke-tested, because the tests would clone beside the build. That gate is kept. So:

- Stage 10 of every apply, and `POST …/kubernetes-version`, read the templates of the Proxmox
  cluster. When a node image of the BOM's Kubernetes version is **published on every node the
  platform cluster uses**, they add that version to the provider with the line Template updates shows
  for it (the newest published copy per node, including nodes the platform does not use). The
  stage's item `kubernetes-version` says what it found.
- A version the provider already names is yours. It is never overwritten and never removed, whatever
  it names, and other versions stay as they are. To change the line of a version you set, edit it on
  the provider's **Connection** tab.
- A run offers a version **once**. The installation records each version that was offered
  (`status.kubernetesOffered`), by a run or by `POST …/kubernetes-version`. When you delete that
  version from the provider afterwards, to stop offering it for new clusters while its template
  stays for the clusters that exist, an apply leaves it deleted; the stage's item says that the
  version was offered before. To offer it again, call `POST …/kubernetes-version` or copy the line
  from **Publish on all nodes** into the provider.
- *When you upgrade ProxCloud on a site that stays on a 0.8.x BOM:* the first apply after the
  upgrade adds the BOM's Kubernetes version to the provider when the provider does not name it and
  a node image of it is published on every node the platform cluster uses. That includes an image
  you published only to test it. Withdraw such a copy before the apply, or delete the version from
  the provider after it: it is then not added again.
- While no copy is published, the ready card says so under the Kubernetes provider, lists each node
  with the state of its copy (not imported, imported and waiting, smoke test running or failed and
  why, published), and offers one button, **Test, publish and offer Kubernetes <version>**. It runs
  [Publish on all nodes](template-updates.md) for the BOM's image, one smoke test or import at a
  time, on every node of the provider, and then calls `POST …/kubernetes-version`. Keep the page
  open: leaving it pauses, and the button goes on from where it stopped. It stops at the first step
  that fails and shows that node's message. A failed smoke test is not repeated by the button: run
  it again from the copy's row in Template updates.
- The smoke test needs the provider's smoke-test network (`config.templateSmoke`,
  [template-updates.md](template-updates.md)). Without one the node's line reads "Smoke test not
  started" with the reason; set the network on the Proxmox provider and press the button again.
- The version is written once the platform's own nodes publish the image. On a Proxmox cluster with
  more nodes, a copy published later on another node is not added to a version the provider already
  names: copy the line from **Publish on all nodes** into the provider, or let the card's button
  finish before it offers (it does: it offers only after the last node).

**The attachment.** `spec.tenants.attach` is applied by stage 10 through the paths an operator uses
by hand, and each change is in the audit log with the admin who started the run as its author and
the installation in its metadata:

| Choice | The attach policy of both providers | Projects that exist |
|---|---|---|
| `all` | on (`config.attachToNewProjects: true`) | One bulk attach per provider with scope "all" ([cloud-integration.md](cloud-integration.md)): audit action `BULK_ATTACH`. Its progress and its failed projects are on the provider's **Projects** tab. A run does not wait for it |
| `new` | on | untouched |
| `none`, or no setting | off | untouched |

A choice is applied **once**: stage 10 acts only when the setting differs from the one it applied
last (`status.tenantsAttach`). So an apply never attaches again a project you detached, and never
turns back on a policy you turned off; to have a run act again, change the setting. The choice is
recorded only when **both** providers took it. When one did and the other could not (a warning, see
below), the next apply applies it to both again: with `all`, a project you detached from the first
provider in between is attached again by that apply.

Saving `all` needs the permission `admin:project:update` besides `admin:service:manage`, as the bulk
attach on the provider's **Projects** tab does (`403` otherwise); the built-in roles ADMIN and
SUPER_ADMIN hold both. A run that has no requester is in the audit log as the system, with the run
in the entry's metadata (`requestedBy`). A run never
detaches a project: `none` after `all` turns the policy off and leaves every project attached. An
installation from before v0.9.0 has no setting, and its providers stay exactly as they are. When a
provider is disabled, a bulk attach of more than 5000 projects would be needed, or the attach fails
(the database, the start of the bulk attach), the stage's item `attach:<provider id>` says so as a
warning, the providers stay registered, the run still ends ready, and the next apply tries again. The card says which choice the settings hold and whether it is applied yet.

**The job scheduler.** One line under the providers says what tenants miss while it is off, with the
command ([Prerequisites](#2-prerequisites)).

**What a first tenant still needs.** With a project attached to both providers and a version
offered, the customer console's cluster form needs a name and a flavor. The rest has defaults:

- *Network.* A cluster's nodes need a network of the project without DHCP. The form preselects the
  project's network when exactly one is not listed with DHCP, offers **Create a network for this
  cluster** when the project has none (the network the Networks page makes with its defaults), and
  marks networks listed with DHCP; they stay selectable. The mark is the form's guess: it reads the
  stored network (the zone's DHCP and a DHCP range on the subnet together, as of the last sync),
  while the API decides on the zone alone and refuses with "Worker nodes need a network without
  DHCP". So in a zone with DHCP a network made without a range is not marked and its create is
  refused, and the form cannot tell before it makes a network whether the zone has DHCP: when the
  network it made comes back with DHCP it says so and names it, so that the tenant can delete it.
  When every network of the project has DHCP it says that a new one would be the same and sends the
  tenant to the operator: the zone is yours to change (the Check row `tenant-network`).
- *Not fixed by this release:* a flavor must exist in the catalog and fit the project's quota (the
  form disables the ones that do not); a price rule for clusters must exist or usage is billed as
  zero; the worker root disk is the provider's `rootVolumeGiB` (30); on a Compose install a tenant
  cluster has no block volumes ([section 9](#9-limits-on-the-compose-install)); the path from the
  tenant network to the platform's load balancer range must be open
  ([the path from tenants to the platform](#the-path-from-tenants-to-the-platform)); nothing
  publishes a Service of type LoadBalancer outside the tenant network.

**Platform cluster access.** **Download platform kubeconfig** (`GET …/platform-kubeconfig`) saves
`<name>-platform.kubeconfig`. It is described under
[Credentials](#8-credentials-and-the-seed-backup): one hour, read access without Secrets, and the
way into the cluster's ArgoCD.

**Open ArgoCD.** There are two ArgoCD instances and neither has an address by default: no Service
of type LoadBalancer, no ingress, no URL. The platform cluster's can be given one
([An address for the platform cluster's ArgoCD](#an-address-for-the-platform-clusters-argocd)); the
seed's never gets one. Without an address both serve plain HTTP inside their cluster, so the way in
is a port-forward from a machine that has `kubectl` and reaches the API address of the cluster. The
card prints the commands with the installation's file names:

| ArgoCD | What it holds | Kubeconfig | Local port |
|---|---|---|---|
| On the platform cluster | One Application per tenant cluster and tenant database | **Download platform kubeconfig** | 8080 |
| On the seed | The platform's own components, the cluster itself included | **Download seed kubeconfig** | 8081 |

```sh
export KUBECONFIG=./<name>-platform.kubeconfig     # or ./<name>-seed.kubeconfig
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo
kubectl -n argocd port-forward svc/argocd-server 8080:80      # 8081:80 for the seed
```

The first command prints the password of user `admin`; the second runs until you stop it. Then open
`http://localhost:8080` (or `:8081`). The Secret `argocd-initial-admin-secret` is the one ArgoCD
writes at its first start; ProxCloud sets no password. If someone deleted it or changed the
password in ArgoCD, the first command no longer gives a password that works, and ProxCloud cannot
recover one. Seen on a lab (2026-11-18), on the platform cluster and on the seed: the chart of this release
(argo-cd 10.14.0) names the Service `argocd-server` with ports 80 and 443 and writes that Secret,
the first command gave a password of 16 characters, and with the second `http://localhost:8080/`
answered `200` (ArgoCD v3.6.1). No login to either ArgoCD was made.

`admin` in an ArgoCD can deploy anything to the cluster it runs on, and the platform cluster's hosts
every tenant control plane. Use it to look and to sync again. What you change by hand on a
platform component, the next **Apply this release** writes back; a tenant Application is written
again by the provider's next sync.

#### An address for the platform cluster's ArgoCD

Off by default. A platform whose settings never name an address is rendered exactly as before this
feature: the same Application values, no change at the upgrade to this build.

Fill **ArgoCD address** and **Operator networks** under Access in step 1 (`spec.access.argocd`
`{address, sources[]}`), save, and press **Apply this release**. The plan lists the row
`access.argocd` with what it does. The apply then:

- makes the Service `argocd/argocd-server` on the platform cluster a `LoadBalancer` that asks
  MetalLB for that one address (`metallb.io/loadBalancerIPs`) and lets in only the operator networks
  (`loadBalancerSourceRanges`);
- leaves `server.insecure` out, so `argocd-server` serves HTTPS on port 443 with the self-signed
  certificate it generates (port 80 redirects). The browser warns once. There is no certificate
  authority of ProxCloud's and no cert-manager Certificate: an IP address gets no publicly trusted
  certificate either way. A domain with an ACME certificate is not built;
- restarts the platform cluster's ArgoCD (the change of `server.insecure` restarts its pods). Tenant
  clusters and databases keep running; their status in the consoles is stale for the restart. A
  later change of the address or of the networks alone restarts nothing;
- records `status.argocdUrl` once the Service holds the address. The ready card then shows the link.

**Which ArgoCD.** The platform cluster's only: the one with an Application per tenant cluster and
database. The seed's ArgoCD gets no address, by decision: its admin controls Cluster API and with it
every machine of the platform, and the seed has no load balancer. It stays behind the seed
kubeconfig and the port-forward above.

**Signing in.** With ArgoCD's own `admin` account. ProxCloud creates no ArgoCD account, stores no
ArgoCD password, puts none in any API answer and has no route that reveals one. Read the password as
before, with the one-hour platform kubeconfig (an audited download) and the first command of the
block above. Nothing rotates that password: it is the one ArgoCD wrote at its first start.

**What admin there means.** `admin` on this ArgoCD can deploy anything to the cluster that hosts
every tenant control plane and database: it is cluster-admin there in effect. Use it to look and to
sync again. What you change in ArgoCD by hand is undone by the next sync, because the Applications
heal themselves; a tenant Application is written again by its provider. For an everyday look that
changes nothing, the ready card's component list (`GET …/installations/{id}?live=1`) is enough.

**Who can reach the address.** The address is in the MetalLB pool, which tenants reach by design
(their cluster APIs and databases are there). Only `loadBalancerSourceRanges` stands between a
tenant VM and the ArgoCD login page. Two limits follow:

- On a site where tenant traffic leaves under a Proxmox node's address (SNAT, the routed and simple
  zone layouts), a tenant VM arrives from the node's address. An operator network that contains that
  address lets every tenant in, and a source range cannot tell the two apart. The Check row
  `argocd-access` warns when an operator network contains a node's address. On such a site name
  networks no node is in (a VPN range, a jump host as `/32`), or leave the address out.
- That the platform cluster enforces the source ranges (kube-proxy and Cilium as the BOM configures
  them) was seen on a lab (2026-11-20, v0.10.0-rc.1) for one layout: bridge mode, a VLAN tenant
  zone, no SNAT. With one operator address as the only source, the address answered that host and
  timed out for two Proxmox nodes, for the gateway host on the platform network and for a tenant VM
  that reached a tenant cluster's API at another address of the same pool in the same second. On a
  site where tenant traffic leaves under a node's address (the Check row warns there) this is
  **not verified on a lab**: do not set the address on such a production site.

**When the address is taken.** The pool is the tenants' pool, so the address may be held by a tenant
load balancer. MetalLB then gives the Service none, stage 9 waits and, at its deadline (20 minutes
in the wave), the run fails with `argocd_address_unavailable`; the message names the Service that
holds the address when one does. The platform keeps serving, and ArgoCD answers inside the cluster,
over HTTPS now: forward to port 443 (`port-forward svc/argocd-server 8080:443`, then
`https://localhost:8080`). Put a free address in the settings, or clear both fields, save and
**Retry**.

**Clearing it.** Empty both fields, save, apply: the Service is a `ClusterIP` again, MetalLB no
longer announces the address, ArgoCD restarts and serves plain HTTP inside the cluster as before,
and `status.argocdUrl` is cleared.

Run once on a lab (2026-11-20, v0.10.0-rc.1, an installed platform with two tenant servers and no
tenant cluster or database at the apply): the two spec refusals at save, the Check row warning and ok, the plan row, the apply (2
minutes 1 second; the address answered 63 seconds after the start), HTTPS with ArgoCD's self-signed
certificate from the operator address, the refusals above, the status fields, and clearing (1
minute 10 seconds; the Service was a `ClusterIP` again and nobody announced the address). Not run
on a lab: the sign-in, an address a tenant load balancer holds (`argocd_address_unavailable`), and a
site with SNAT ([Not verified on a lab yet](#not-verified-on-a-lab-yet)).

## 5. The stages of a run

One run per installation at a time. The page reads the run every 3 seconds.

| # | Stage (`id`) | What it does | Time limit |
|---|---|---|---|
| 1 | Check the settings and Proxmox (`validate`) | The checks of step 3, on the saved settings. | 5 min |
| 2 | Import the two images (`images`) | The seed image on the seed's node and the node image on the control plane's node and every node of a group, through template updates. An image whose template from an earlier import still exists on that node is not imported again; a template deleted by hand is imported again. An import that has just finished can show `checking`, with the VM it waits for, for a look or two: Proxmox lists a new template a moment after its task ends. That wait ends 5 minutes after the import's last step; a finished import whose VM is still no template in `pc-templates` then fails the stage. | No import moved for 60 min |
| 3 | Create the seed VM (`seed-vm`) | Takes a free VMID, clones the seed template into `proxcloud-seed` with the CAPI token, sets size and network (the template has no network device: Proxmox gives the new one its MAC address, and the CD's network config names that address), generates the seed's k3s server CA and tokens, uploads a NoCloud CD (`pcplat-<id>-…`) to the ISO store, puts it in place of the template's cloud-init drive and protects the VM, turns on the VM firewall (policy DROP; 6443, and 22 when SSH keys are set, from the API sources only) and starts the VM. | 30 min |
| 4 | Reach the seed (`seed-api`) | Waits until `https://<seed address>:6443/readyz` answers with a certificate of the CA made in stage 3. While it does not, the stage says whether the guest agent answers (`seed_not_booted`) or only the API does not (`seed_api_unreachable`). | 20 min |
| 5 | Secure the seed (`seed-identity`) | Reads the token of the service account `proxcloud-system/proxcloud-portal` with a bootstrap token, checks that it is cluster-admin, stores it encrypted, deletes the bootstrap token and its RBAC, then ejects the CD (the drive stays, empty: Proxmox removes no drive from a protected VM) and deletes it. The seed counts as ready from the moment its credential is stored; the stage is done only when the running seed's config no longer names the CD (an eject Proxmox took but left pending does not count, and the CD's file is kept until then) and no store lists it. | 10 min |
| 5a | Upgrade the seed's k3s (`seed-k3s`) | Reads the seed's node and records the k3s it runs (`status.seed.k3sVersion`). Skipped on every run but an upgrade whose BOM names a newer k3s: then a privileged Job on the seed replaces the k3s binary and restarts k3s ([Upgrade between BOMs](#the-seeds-k3s)). It is 5a so that stages 6 to 11 keep the numbers this guide, the codes and the messages use. | 20 min |
| 6 | Install the seed's components (`seed-platform`) | ArgoCD as a k3s HelmChart, the AppProject `proxcloud-platform` (BOM chart repositories only; the `default` project locked), then `cert-manager` and `capi` wave by wave. An Application counts when it is Synced and Healthy, compared against the chart version and values it has now, and its last sync operation ended `Succeeded`: ArgoCD shows Synced and Healthy from the resources alone while a hook of the sync still runs or fails. `capi` is done when, besides, the clusterctl inventory equals the BOM. The item of an Application names the resources and hooks its sync failed on, with their messages, and the stage's message repeats the first. Once an Application counts, its item has its state and no text: what ArgoCD still shows of an earlier attempt is left out. | 30 min |
| 7 | Build the platform cluster (`cluster`) | The CAPI token's Secret and the Application `pc-platform` on the seed; MachineDeployments of nodes no longer in a group are deleted; then waits for the control plane, the admin kubeconfig and every Machine Running. Before it writes anything it reads each node template: a template with a Proxmox cloud-init drive on another slot than `ide0` is refused (`machine_failed`, with the `qm set` command that removes the drive), because CAPMOX puts its own on `ide0` and a machine with both starts from the wrong one. A template without the drive stays usable everywhere: CAPMOX brings its own, and the template-updates smoke test gives its test VM one when the template has none, so such a template can still be tested and published for tenants. A template whose config the portal token may not read is not checked: the stage goes on and shows an item `template-<vmid>` in state `not checked`. A template that passed has the same item in state `ok`; one that Proxmox no longer knows has none (CAPMOX reports it on the machine). The items list each machine with its VMID (`no VM` until CAPMOX has cloned it) and, while it is not Running, the step CAPMOX is at with its error (the reason and message of the ProxmoxMachine's condition `VirtualMachineProvisioned`). CAPMOX calls a VM provisioned once it runs and has its addresses; for a machine that then does not join (kubeadm, the API address, etcd) the item carries the reason and message of the Machine's own condition `Ready` after `Cluster API:`. The message of the waiting stage repeats, in this order, what the ProxmoxCluster reports, the KubeadmControlPlane's condition `Available` while it is not `True` (`Control plane:`), the machines' errors and, last, the state of the Application `pc-platform` while its sync is not done (not while it is only Progressing, which it is for as long as machines are built). On a **Retry** that finds a ProxmoxCluster still without a condition, the run updates an annotation on it (`proxcloud.io/retried-by`) once, so CAPMOX opens the Proxmox API again at once instead of at the end of its backoff of up to 17 minutes; the 10 minutes count from then. Steps CAPMOX tries again (`CloningFailed`, `PoweringOnFailed`) are waited for. `VMProvisionFailed` and `TaskFailed` end the stage: CAPMOX does not go on with such a machine. Once every machine is Running the stage sets `onboot` on each VM of the platform that lacks it, with the CAPI token, so that the machines start again with their node ([After a Proxmox node reboots](#after-a-proxmox-node-reboots)); a VM it could not set does not fail the stage, and the Check's `machines` row names it. | 60 min |
| 8 | Reach the platform cluster (`workload-access`) | Creates the seed ArgoCD's service account on the cluster and a token for it (one year, renewed by any later apply when under 90 days are left), kept only in the seed's cluster Secret; writes the CSI token's Secret for proxmox-csi. | 10 min |
| 9 | Install the platform components (`workload-platform`) | The cluster's components wave by wave, each wave only when the one before is Synced and Healthy with its sync `Succeeded` (as in stage 6). After the first wave (Cilium) every node must be Ready with its group label. The platform's own volumes are recorded for Remove at every look of the stage, so also when it fails inside `kamaji-etcd`'s wave. | 20 min per wave |
| 10 | Create the two providers (`register`) | Creates the Kubernetes and Databases providers, or updates the ones this installation created before, with a kubeconfig of the cluster's `proxcloud-system/proxcloud` service account. Offers the BOM's Kubernetes version when its node image is published on the platform's nodes, and applies a changed `spec.tenants.attach` ([what a tenant can use](#what-a-tenant-can-use-after-the-install)). | 5 min |
| 11 | Finish (`done`) | Records the components and the BOM as installed. The phase becomes `ready`. | 5 min |

An upgrade (an apply that moves the platform to this release's BOM) is the same stages with the
target BOM, two more waits and one more check, and stage 5a when the BOM names a newer k3s: [Upgrade between BOMs](#upgrade-between-boms).

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
- If the API stops, the next start resumes the run. An API that is stopped cleanly (`docker compose
  restart api`, a pod that is terminated) cancels the run at the stop signal, before it drains its
  requests, and gives the run's job back as soon as the call the stage has in flight returns: the
  next start takes it at once and no attempt is spent. This holds only when that happens within the
  container's stop grace period (10 seconds by default under Compose, after which Docker kills the
  process); otherwise the stop is as a crash. The release was run once on a lab (2026-11-17, main
  cb2f052): `docker compose restart api` 33 ms into stage 8 of an apply on a ready platform, which
  had nothing new to write. The job was given back 24 ms after the stop signal, the new process
  went on with stage 8 0.9 seconds after the signal, the run succeeded with attempts 1 of 20, and
  nothing was created twice. Not run: a stop while a slow call is in flight, so how often the 10
  seconds are exceeded is not measured, and a stop in stages 2, 3 and 9 of a real install. After a
  crash, or when the release did not reach the database, the run resumes when the job's one-minute
  lease has run out, so up to a minute later, and an attempt is spent (seen on a lab before the
  release existed: a clean restart in stage 7 resumed by itself 61 seconds later, attempts 2 of 20,
  and nothing was created twice). If the job runs out of its 20 attempts (a
  crash during a run or a database error spends one; waits and a clean stop spend none), the run reads `failed` with `job_dead` and the stored phase stays
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
| `image_blocked` | 2 | A BOM image is on the template update blocklist | A new installation: install a newer ProxCloud release. An installed platform stays on its release and its images, and no release moves it to other images yet (see [Upgrade between BOMs](#upgrade-between-boms): a BOM with other images is refused): the message says which case applies, and an apply that must import that image stays blocked |
| `seed_clone_failed` | 3, Remove | The clone task failed or was refused; stage 2 recorded no seed template; the VM stays locked. Also, with nothing being cloned: Proxmox could not be read with the CAPI token while looking at the seed VM or the pool `proxcloud-seed` (unreachable, or the token is refused), in stage 3 on a ready seed or in `remove-cluster` and `remove-seed` | Fix the storage or the node the message names, **Retry**. For a read that failed: check that `capmox@pve!capi` still exists and the stored secret is current, then **Retry** or **Remove again** |
| `platform_network_refused` | 3, Remove | Mode `routed` only: Proxmox refused the zone, the vnet, the subnet or the SDN Apply (the message carries its error); a vnet of the settings' name exists and is not the installation's; the name is an interface of a node; the installation already made a vnet under another name; the zone `pcplat` exists as another kind; the vnet has a subnet that is not the settings'. While another network change of the site holds the SDN lock the stage waits | Correct what the message names in Proxmox (Datacenter, SDN), or name another vnet in step 1 (a draft can be changed), then **Retry** |
| `seed_config_refused` | 3, Remove | Proxmox refused a config change, the read of the VM's config after it, the CD upload, a firewall rule or the start (the message carries Proxmox's own error); the CD could not be rendered or built (`The seed's CD could not be rendered` or `… built`: for example Proxmox reports the seed's `net0` without a MAC address); at Remove, the VM is locked or carries another installation's tag | Fix what Proxmox's error names: the bridge, the boot or ISO store, the node, or a right of `capmox@pve!capi` (then run the script again), and **Retry**; it builds a new seed VM. A CD that could not be rendered or built is a defect, not a setting: nothing was stored and no CD was uploaded, Retry will not pass, report the message. A VM with another installation's tag is not deleted: look at it in Proxmox |
| `seed_not_booted` | 4 | The seed's guest agent did not answer in time: the VM did not boot, or its first boot failed (for example the k3s binary or its install script could not be downloaded, or one failed its checksum) | Open the seed VM's console in Proxmox. **Retry** builds a new seed |
| `seed_api_unreachable` | 4, 6 to 10, Remove, seed access, the page of a ready installation (`live.code`) | The seed is booted but its API does not answer from the API's host; or a seed call failed later (stages 8 to 10 read the cluster's admin kubeconfig from the seed); or the seed's API refused the stored credential or the request with a `401` or a `403` at five looks on end (a seed that restarts answers so for some seconds, with `Unauthorized`, `RBAC: … not found`, a plain `forbidden` or `not yet ready to handle request`, which the run waits out) | Add a route from the ProxCloud host to the platform network, or correct the seed API sources and save. **Retry** writes the seed's firewall rules again first. For a credential the seed keeps refusing: look on the seed VM's console whether k3s runs or keeps restarting (`systemctl status k3s`; a slow disk does that, see [What no check reads](#what-no-check-reads)), then **Retry**. For a ready installation whose seed stopped answering: [When the seed does not answer](#when-the-seed-does-not-answer) |
| `seed_tls_mismatch` | 4 | The server on the seed address presents a certificate of another CA | Another machine holds that address. Correct the seed address, **Retry** |
| `seed_identity_lost` | 4, 5, later | The run holds no credential for the seed; the seed refused the bootstrap token at five looks on end (it expires 3 hours after the seed was built); the seed did not report the service account as cluster-admin to the end of stage 5's time limit | **Retry**. It builds the seed again when it never became ready |
| `seed_cd_not_deleted` | 5, Remove | The CD could not be ejected or deleted, or Proxmox still lists it; or Proxmox took the eject but left it pending on the running seed (the message says so) | The CAPI token cannot delete on the ISO store: run the script again, **Retry**. For a pending eject: `qm pending <vmid>` on the seed's node shows it; stopping and starting the seed VM applies it, then **Retry** |
| `seed_backup_missing` | start, plan | An apply would start an upgrade that replaces the seed's k3s, and the portal saw no `vzdump` archive of the seed VM younger than 24 hours. On most sites it cannot see one: Proxmox shows the portal's tokens the seed's backups on the import store and the platform's ISO store only (row `seed-backup` of the check names the stores it cannot see into). `409`, no run was made | Check in Proxmox that there is an archive of **this** seed VM (its VMID, and the seed VM's name in the notes when it has notes), made after the seed VM was created and younger than 24 hours; take one if there is none (`vzdump <seed VMID>`). Then tick "I checked in Proxmox: seed VM … has a backup made after it was created and younger than 24 hours" in step 4 (`seedBackupTaken` by API) and start the upgrade again |
| `seed_k3s_failed` | 5a | An upgrade run only. The Job that replaces k3s failed: the etcd snapshot, the download (the seed must reach the BOM's k3s URL, as at its first boot), the sha256 check or a later line. Its script stops at the first failure, so a failure before the last two lines leaves k3s as it was. The message ends with the Job's last output; the Job is deleted. Also when the Job ended more than five minutes ago and the seed's node still reports the old k3s (the previous binary was put back by hand): the Job is deleted | Correct what the message names (most often the seed's way out to the download address), **Retry**: it makes a new Job |
| `seed_k3s_refused` | 5a | An upgrade run only. The seed's node reports a k3s that is newer than the BOM's or more than one minor older, or an older k3s than the one both BOMs name (the plan listed no `seed.k3s` row, so the run does not replace k3s): the record and the site disagree. Nothing was changed | The message names both versions. A seed that was changed by hand is not moved by a run |
| `seed_k3s_timeout` | 5a | An upgrade run only. The stage did not end in 20 minutes: the Job did not start or end, the node did not report the new version, or the seed's API did not come back after the restart. The platform cluster and every tenant keep serving; nothing is reconciled while the seed's API is away | If the seed's API answers: **Retry**. If it does not and `/usr/local/bin/k3s.prev` exists on the seed, put the previous k3s back over SSH with a key of `spec.seed.sshKeys`, with the command the message gives, then **Retry**. For a patch step it is `mv /usr/local/bin/k3s.prev /usr/local/bin/k3s && systemctl restart k3s`. For a minor step (as 1.35 to 1.36) it also restores the etcd snapshot the Job saved: stop k3s and the pods (`k3s-killall.sh`), put `k3s.prev` back, `k3s server --cluster-reset --cluster-reset-restore-path=<the snapshot>`, `systemctl start k3s` ([The seed's k3s](#the-seeds-k3s) has the whole command). Neither was tried on a lab |
| `seed_lost` | 3, Remove | A seed that had become ready is gone, or the VM at its VMID is no longer this installation's. It is not rebuilt: a new, empty seed would have no record of the cluster's machines | Remove with the option for a missing seed, then install again ([Remove](#remove)) |
| `component_unhealthy` | 6 to 9 | An Application is not Synced and Healthy in time (the message names it and its condition); or something could not be rendered or applied, in stage 7 the Application `pc-platform` | Download the seed kubeconfig to look closer, then **Retry**. ArgoCD does not sync an Application again by itself once its retries are spent; the run starts the sync of one whose last sync ended `Failed` or `Error`, once per run, so no manual sync is needed after the cause is fixed. When that sync ends `Failed` or `Error` too, the stage fails at once with what the sync failed on (the hook or resource and its message) instead of waiting for its time limit; **Retry** starts the sync once more. ArgoCD takes about 40 minutes to spend the ten retries of one sync, longer than the time limit of stage 6 or of a wave, so the stage does not wait for that either: once one sync has failed four times (about 8 minutes of ArgoCD's backoff), the last time in this run, and ArgoCD shows the hook or resource it failed on, the stage fails with that text while ArgoCD still tries. After a **Retry** the same sync gets one more attempt before the stage fails on it again |
| `capi_init_failed` | 6, 9 | clusterctl did not install the providers of the BOM; or a provider is installed at another version than the BOM names (provider upgrades are not in this release) | The same |
| `machine_failed` | 7 | CAPMOX gave a machine up: its ProxmoxMachine's condition `VirtualMachineProvisioned` is `False` with `VMProvisionFailed` (not enough memory on the node for its scheduler, no free VMID, the template not found, a VMID held by another VM) or `TaskFailed` (a Proxmox task failed: a clone, a resize, a config change). The message has the machine's name, its VMID and CAPMOX's text. Also: a node template has a Proxmox cloud-init drive on another slot than `ide0` (the message has the command), or its config could not be read for another reason than a missing permission | Correct what CAPMOX's text names. CAPMOX does not try that machine again, and **Retry** alone finds it as it was: delete the Machine on the seed with the seed kubeconfig (`kubectl -n pc-platform delete machine <name>`, the command is in the message), Cluster API makes a new one, then **Retry**. Do not delete the Machine when the text says the VM's name does not match (`expected VM name to match`): the VMID in the ProxmoxMachine then belongs to another VM, and CAPMOX deletes the VM at that VMID when its token may. The message then has no delete command and says so: look in Proxmox whose VM it is, and delete the Machine only once its ProxmoxMachine no longer names that VMID. For the template: run the `qm set` command of the message on that node, then **Retry**. The template then has no cloud-init drive, which is fine for its machines and for its smoke test ([template-updates.md](template-updates.md#smoke-test-templateupdatesmokev1)) |
| `capmox_auth_failed` | 7 | Proxmox refuses the CAPI token when CAPMOX uses it (a condition of the ProxmoxCluster or a ProxmoxMachine says so); or CAPMOX has set no condition on the ProxmoxCluster for 10 minutes, which is how a Proxmox API URL the seed cannot reach and a refused token both show, because CAPMOX opens the API before it writes anything | Check the Proxmox API URL as the platform network reaches it ([What no check reads](#what-no-check-reads)), paste the secret again, **Retry**. The Retry makes CAPMOX try at once (it updates the ProxmoxCluster), so a fix shows within its 10 minutes. CAPMOX's log on the seed says which it is: `kubectl -n capmox-system logs deploy/capmox-controller-manager` |
| `control_plane_timeout` | 7 | Not every machine is Running or the control plane is not initialized in 60 minutes. The items and the message carry the step each machine is at and CAPMOX's last error | **Retry**; the time limit starts again. Look at the machines with the seed kubeconfig |
| `workload_api_unreachable` | 8 to 10, Remove | The cluster's API does not answer from the API's host, or its kubeconfig or CA on the seed is not usable, or it refused the admin kubeconfig or the request with a `401` or `403` at five looks on end (as for `seed_api_unreachable`) | Add a route from the ProxCloud host to the API address, **Retry**. At Remove: restore the route and **Remove again**, or tick the option for a cluster that does not answer |
| `nodes_timeout` | 7, 9 | Not every node is Ready with its group label, or the cluster has another number of nodes than machines: after the first wave of stage 9, and in stage 7 between two units of a replacement of the machines ([The platform cluster's machines](#the-platform-clusters-machines)). The message names every node that is not Ready, and all of them when only the count is off | **Retry**. A Node that is not Ready and that no Machine owns (Cluster API removed its machine and gave up deleting the Node object) is deleted by the run at its next look, and the stage's items name it (`node/<name>`, state `deleted`): a run that stood in this wait on 0.10.1-rc.1 gets out with one Retry. A node that a Machine owns is never deleted: look at that machine with the seed kubeconfig |
| `provider_conflict` | 10 | A provider with the same name exists that is not this installation's, two rows carry its marker, or the provider validation refused the row | Correct or rename that provider under Cloud providers, **Retry** |
| `bom_unsupported` | start, plan | The installed BOM is not one this release carries: its own, or that of an earlier release from 0.8.0 on. A platform installed by a build of `main` (`dev-<sha>`) gets this after an upgrade | A platform moves only between BOMs a release carries ([Upgrade between BOMs](#upgrade-between-boms)). Go back to the ProxCloud release that installed the platform. Or, if no tenant cluster or database must be kept: delete them all (Remove refuses with `tenants_exist` otherwise), remove managed services, which deletes the platform cluster, and install again |
| `upgrade_refused` | start | An apply names this release's BOM as its target (`targetBom`) and the two BOMs differ in something this release does not move, or the target is a `-dev` BOM. `409`, no run is made; `details.reasons` has one line per entry, the same lines as the plan | Nothing is wrong with the platform: it stays on its BOM, and **Apply this release** keeps working. The move needs a later release ([Upgrade between BOMs](#upgrade-between-boms)) |
| `node_image_missing` | start, plan, 1 | The platform part of an upgrade that replaces machines, before its roll has begun: a node the cluster uses holds no template of the target BOM's node image (no finished import of this provider, node and digest whose template is still in `pc-templates`), or that could not be read. Nothing was changed | Run **Seed and node image** in step 4: it imports the image on every node. Then read the plan again |
| `seed_not_upgraded` | start, plan | The same part: the target BOM names another k3s and the seed's recorded k3s is not it yet | Run **Seed and node image** first |
| `capacity_short` | start, plan, 1, 7 | The control plane's node has no room for the one more machine its replacement makes first (memory by the count of the Check row `capacity`, own machines included, and one more full disk on the boot store), or that could not be read; the message has the numbers. In stage 7: the node of the step's unit had room when the plan was read and has none now. Nothing of that step was applied, every machine runs as before | Free memory or space on the node (a stopped VM counts with its configured memory), then read the plan again, or **Retry**. A control plane is never replaced by deleting its machine first |
| `drain_blocked` | start, plan, 1 | The same part: a PodDisruptionBudget on the platform cluster allows no disruption (`status.disruptionsAllowed` 0 with pods), so Cluster API's drain of the machine its pod is on would wait without end; the message names each as namespace/name. Also when the platform cluster's API could not be read | For a database: bump its chart to 0.5.2 or newer ([managed-dbaas.md](managed-dbaas.md#chart-pin--platform-update)), which sets no budget for one instance. For anything else: a second replica, or delete the budget. Then read the plan again |
| `platform_unhealthy` | start, plan, 1 | The same part: a Machine of the platform cluster is not Running, the count of Machines differs from the settings, a component's Application is not Healthy, or the seed could not be read. Unhealthy tenants do not block | Repair it (**Apply this release**, **Retry**), then read the plan again |
| `rollout_stalled` | 7 | A run that replaces machines: the items of the unit the roll stands at did not change for 30 minutes. The message has the machine's state and, for a machine that cannot be drained, Cluster API's text with the pod that blocks. The machines that run keep serving | Correct the cause, then **Resume** (Retry): the run goes on at the same unit and renders no unit back |
| `argocd_address_unavailable` | 9 | The settings name an ArgoCD address, and MetalLB did not give it to the Service `argocd/argocd-server` of the platform cluster before the wave's deadline: another Service holds it (the message names it), or it is not in MetalLB's pool. The platform keeps serving; ArgoCD answers inside the cluster | Put a free address of the load balancer range under Access in step 1, or clear both fields; save, then **Retry** |
| `bad_part` | start | `part` on `POST …/runs` is neither `seed` nor empty, or `seed` on an apply that is not an upgrade which replaces machines (`400`) | Send the part the plan lists |
| `unexpected_rollout` | 7 | An upgrade run only. The cluster chart of the target BOM would create a machine or bootstrap template, or change the control plane: Cluster API would replace machines. On a run that replaces machines: the render of the step would also replace machines outside the step's own unit. Nothing of the cluster (of that step) was synced; the Application `pc-platform` is left at the new chart version without automated sync. Also on such a run: the record says every unit is replaced, and the Application does not carry what the run renders (the run moves to another release than the one the machines were replaced for, the settings changed since, or the record was edited); nothing was applied | If machine sizes, nodes or node groups were changed in the settings together with the upgrade, put them back, save, **Retry**. A run to another release than the one the machines were replaced for: finish that upgrade first (**Retry**, an apply without a target), then start the new one. Otherwise the release is at fault: see [Upgrade between BOMs](#upgrade-between-boms), "When the upgrade cannot finish" |
| `tenants_unhealthy` | 7, 9 | An upgrade run only. After a wave of stage 9, and between two units of a replacement of machines in stage 7, something the tenants run on that was there when the upgrade first reached the platform cluster is not: a tenant control plane's Deployment has fewer available replicas than it asks for, a database has fewer Ready instance pods than `spec.instances`, a Deployment of the Kamaji or CloudNativePG operator is not available, a tenant's Application on the platform cluster's ArgoCD is not Healthy, or a TenantControlPlane is not Ready. The message names each. The stage waits for them up to the wave's 20 minutes (stage 9) or the unit's 30 minutes (stage 7) | Look at the tenant the message names. **Retry** goes on from the same wave and waits for it again: the list of tenants that were down before is taken once per upgrade (`status.tenantsDownBefore`), not per run, so a tenant a wave broke holds every run until it is healthy or deleted |
| `portal_changed` | any | ProxCloud was upgraded during the run and no longer carries the run's BOM. The stage the run stood at reads `failed` with this code | **Retry** |
| `job_dead` | any | The run's job ran out of attempts | **Retry**, or **Remove again** |
| `platform_disabled` | start | The switch, or template updates, is off | `--managed-services` |
| `stage_unavailable` | any | A run recorded by another release names a stage this one does not have | No action in this release |
| `tenants_exist` | Remove | Tenant clusters or databases still run on the platform (the message names up to five) | Delete them, **Remove again** |
| `sandbox_refused` | 7 | The settings ask for another sandbox placement than the one a run built, or the run's BOM has no sandbox chart. Nothing was changed | Set `placement: off`, apply, then the other placement; or leave `spec.sandbox` out |
| `sandbox_access_failed` | 7 | Proxmox refused pool `proxcloud-sbx`, the ACL row of the sandbox group or the token of the sandbox cluster. The portal token writes them with the rights the setup scripts gave it. Nothing of the cluster was changed | The Check's `sandbox` row names what is missing: run the managed-services script again as root on a node, then **Retry** |
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

A **Retry** after a failed upgrade goes on to the upgrade's target, never back to the BOM the
platform left ([Upgrade between BOMs](#upgrade-between-boms)).

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
   another one) and every `pcplat-<id>-*` CD; then every vnet with this installation's alias
   (a network of mode `routed`, also one the settings no longer name) with its subnet, and the
   zone `pcplat` when no other vnet is in it, with one SDN Apply;
6. wipes the stored secrets, the recorded status and the last tenant path test.

Afterwards the page prints the two `pveum user token remove` commands; ending the tokens is yours to
do. Remove leaves in place: the tokens, users, roles, ACL rows and pools of the script; the two
provider rows, disabled; the image templates in `pc-templates`; and the installation record in phase
`removed`, from which the same steps install again (both secrets must be pasted again).

A Remove deletes no rule of the cluster firewall, except the installation's own rule for the Proxmox
API, and takes none of the blocked networks a save added off the provider. The provider's `proxcloud:isolation` or
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
| `remove-secrets` | `secrets` | `deleted`: the stored token secrets, the seed's credential, the recorded status and the last tenant path test |

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
| The seed credential (a service account token, cluster-admin on the seed) | Encrypted in `platformSecret` | **Replace the seed credential**: creates the next token Secret (`proxcloud-portal-token-g<n>`), checks it, stores it, then deletes the older ones, which ends their tokens. Refused during a run. If it stops halfway, press it again; the next apply also removes token Secrets nothing stores. The audit log gets one entry, `ROTATE_CREDENTIAL` on the installation, with the names of the token Secret before and after (`fromSecret`, `toSecret`) and never a token. A rotation that stopped after it had stored the new credential gets that entry with outcome `FAILURE`. The old token does not end at that instant: the seed's API server remembers a token it has checked for some seconds (measured on a lab: the old token still answered 6 to 7 seconds after the rotation had returned). **A seed backup taken before a rotation is of no use after it**: the archive holds only the older token Secret, a seed restored from it refuses the stored credential, and ProxCloud cannot make a new one on a ready seed (the bootstrap token is gone). So run a backup by hand right after every rotation, as root on the seed's node: `vzdump <seed VMID> --storage <backup store> --mode snapshot --compress zstd` ([step 5](#when-the-seed-does-not-answer)). That archive holds the seed's secrets: read the warning under **Seed backup** below before you choose the store |
| The cluster's admin kubeconfig | On the seed only (Cluster API keeps it). A run reads it into memory, and so does a platform kubeconfig download, which never hands it out | Not stored by ProxCloud |
| Service account `kube-system/proxcloud-break-glass` on the cluster, with ClusterRole and ClusterRoleBinding `proxcloud-break-glass` and Role and RoleBinding `proxcloud-break-glass` in `argocd` | On the cluster. It has no token Secret; written by the first platform kubeconfig download and again by each one | Deleting the service account ends every token issued for it. The next download creates it again |
| The seed ArgoCD's token for the cluster | In the seed's cluster Secret only; valid one year | Renewed by any apply when under 90 days are left. The page shows the expiry and warns under 30 days |
| The providers' kubeconfig (service account `proxcloud` on the cluster) | In the two provider rows' encrypted secret | Written again by every apply |
| The platform cluster ArgoCD's `admin` password | On the cluster only, in the Secret `argocd-initial-admin-secret` ArgoCD wrote at its first start. ProxCloud never reads it into the portal, stores nothing for the ArgoCD address and creates no ArgoCD account; you read it with the platform kubeconfig | None by ProxCloud. Changing it in ArgoCD is yours to do and to keep |
| The seed's CA key, k3s token, bootstrap token | On the CD until stage 5 deletes it; the CA key is never stored by ProxCloud, the bootstrap token expires after 3 hours | None |

**Break-glass kubeconfig.** **Download seed kubeconfig** (`GET …/seed-kubeconfig`) asks the seed for
a one-hour token of the portal's service account and wraps it in a kubeconfig: full access to the
seed, and through the Secrets on it to the cluster's admin kubeconfig. The page saves it as a file.
The route itself answers JSON, `{"data": {"kubeconfig": "<the file's text>", "expiresAt": "<time>"}}`,
not the file: a script takes `.data.kubeconfig` (`jq -r .data.kubeconfig > seed.kubeconfig`). It needs
`admin:service:manage`, is written to the audit log as `BREAK_GLASS` (who, which installation, the
expiry; never the token) before it is handed out, and is refused when the audit log is not
available. Nothing in ProxCloud ends such a token before it expires: **Replace the seed credential**
deletes the stored credential's token Secrets, not a token issued this way (measured on a lab: a
kubeconfig downloaded before a rotation kept working after it). So rotating does not help when such a
file has leaked: it stays valid until its hour is over, and ProxCloud offers nothing that ends it
earlier. Treat the file as a root credential for one hour.

**Platform kubeconfig.** **Download platform kubeconfig** on the ready card
(`GET …/platform-kubeconfig`) is the same for the platform cluster, with less in it. ProxCloud reads
the cluster's admin kubeconfig from the seed into memory (a long-lived client certificate that Cluster
API issues; it is never handed out), writes the service account
`kube-system/proxcloud-break-glass` with its roles on the cluster, asks the cluster for a one-hour
token of that account and wraps the token in a kubeconfig. If the cluster grants a longer life than
the hour, nothing is handed out (`502`). Same permission, same audit entry (`BREAK_GLASS`, with
`operation: platform-kubeconfig`; the seed's has `seed-kubeconfig`), same refusal without the audit
log, same JSON answer.

What the token may do:

| Where | What | Why |
|---|---|---|
| The whole cluster (ClusterRole) | `get`, `list`, `watch` on namespaces, nodes, pods and their logs, services, endpoints, events, config maps, volumes and claims, service accounts, quotas, and on every resource of the API groups of workloads, networking, storage, CRDs, metrics, ArgoCD, Kamaji, Cluster API (without its bootstrap group), CloudNativePG, MetalLB, Cilium and cert-manager | The read steps of this guide and of [managed-k8s.md](managed-k8s.md): Applications, tenant control planes, machines, pods |
| Namespace `argocd` (Role) | `get` and `create` on `pods/portforward` (a WebSocket port-forward is authorized as `get`, a SPDY one as `create`); `get` on the Secret `argocd-initial-admin-secret` | The two ArgoCD commands. The Service and the pods that `kubectl port-forward svc/…` looks up first are read through the ClusterRole |

It reads no other Secret (a tenant cluster's admin kubeconfig and the providers' token are Secrets),
no join token of Cluster API's bootstrap group, and it changes, deletes and executes nothing. The
repair steps of the guides that write (`kubectl patch application`, `kubectl delete secret`) are
left out on purpose: they need the admin kubeconfig, which the seed kubeconfig reads
(`kubectl -n pc-platform get secret pc-platform-kubeconfig -o jsonpath='{.data.value}' | base64 -d`).

This is a limit on `kubectl`, not on the holder. The file opens the cluster's ArgoCD as `admin`, and
ArgoCD's own service accounts can change everything on the cluster. So whoever holds the file can,
through ArgoCD, do what a cluster admin can, for as long as the admin password is unchanged, which
is longer than the hour. Keep the file like an admin credential. To end a token before its hour,
delete the service account with the admin kubeconfig
(`kubectl -n kube-system delete serviceaccount proxcloud-break-glass`); the ArgoCD password is
changed in ArgoCD. Neither was run on a lab.

**Seed backup.** The seed holds what cannot be rebuilt from ProxCloud's database: the Cluster API
state of the cluster's machines and the cluster's CA and admin kubeconfig. Nothing backs it up for
you, and the page cannot see whether a job exists. It prints this command for the ready seed; run it
as root on one node with a backup store of your own:

```sh
pvesh create /cluster/backup --vmid <seed VMID> --storage <backup store> --schedule daily --mode snapshot --compress zstd --prune-backups keep-daily=7
```

`--compress zstd` and `--prune-backups keep-daily=7` are there because a job without them writes
one uncompressed archive a day and removes none, on a store that has no retention of its own
(measured on a lab, one `vzdump` by hand each time, for a seed that used 3.8 GiB of its 20 GiB
disk: with `--mode snapshot` only, 2 minutes 17 seconds and a 5.73 GiB archive; with `--mode
snapshot --compress zstd`, 1 minute 45 seconds and a 2.39 GiB archive; the seed stayed up both
times). Change the seven days to your own rule. Not run on a lab: the scheduled job itself and
`--prune-backups`.

> **Warning: the archive holds the seed's secrets.** Every archive of the seed carries its
> cluster-admin token and the token secret of `capmox@pve!capi`. The backup store must not be
> world-readable, and it must not be a shared export that other tenants or VMs can mount. `vzdump`
> does not protect the file: on the lab it wrote the archive with its default mode, 644, on an NFS
> export open to a whole subnet, so every user of the nodes and every client of that export could
> read it. On a production site's NFS store (2026-11-20) it wrote the archive mode 777 and its log
> mode 644. Check the mode of the files and who can mount the store before the first backup, and
> after every backup by hand make the archive and its log readable by root only, as root on the node:
> `chmod 600 <archive> <archive's .log>`, then `ls -l` both. Nothing in ProxCloud checks the store
> you choose.

Choose a backup store that is not the boot, ISO or import store of the settings,
and whose archives no API token other than your own (the operator's) can read. Proxmox lets a token
read an archive when it holds `Datastore.Allocate` on the store, or `Datastore.AllocateSpace` on
the store together with `VM.Backup` on the seed VM. So check both, as root on one node:

```sh
pveum user list                                  # every user
pveum user token list <user>                     # every token of that user
pveum user token permissions <user> <token name> --path /storage/<backup store>
pveum user token permissions <user> <token name> --path /vms/<seed VMID>
```

Run the last two for every token of every user, the portal token and the `csi@pve` and
`capmox@pve` tokens among them. They print the rights the token really has on that path, whatever
gives them. Reading the ACL rows of the store instead (`pveum acl list`) misses some: a row on
`/`, `/storage` or `/vms` with propagate 1 reaches the store or the seed, so does a row on
`/pool/<pool>` when the store or the seed VM is a member of that pool, and a row that names a user
or a group also holds for every token of that user (or of the group's members) made without
privilege separation. The two permission commands were run on the lab for single tokens; going
through every token of every user was not. On the lab the portal
token held `Datastore.AllocateSpace` on the backup store, and only the `NoAccess` row on
`proxcloud-seed` kept `VM.Backup` on the seed from it: do not remove that row. These checks cover API rights
only: on a file store (a directory, NFS, CIFS) the archive is also a file, which every user of the
nodes and every client of the export can read when the file's mode and the export allow it (on the
lab: mode 644 on an NFS export open to a whole subnet). Nothing in ProxCloud checks the store you
choose.

On a Proxmox cluster shared with other portals or tools no store may fit: on the lab, tokens of
another portal held `Datastore.Allocate` on every store that takes backups. Then take the store
the fewest tokens reach, and count every token that can read it as a holder of the seed's
credentials. On a site whose only backup store is `local` while the ISO or
import store is `local` too, the backup is within reach of ProxCloud's own tokens: `capmox@pve!capi` holds
`PVEDatastoreAdmin` on the ISO store and the portal token on the import store. That is the limit ADR
0009 A2 accepts for this milestone.

A restore from such a backup was run once on a lab (2026-11-17): the 2.39 GiB zstd archive above,
restored over the seed it was taken from three minutes earlier, in 2 minutes 8 seconds. The
sequence, what was seen and what was not run are in step 5 of
[When the seed does not answer](#when-the-seed-does-not-answer). Without a backup, a lost seed means Remove with the
option for a missing seed and a new install, which deletes the cluster's machines and volumes. The
same holds for a seed whose VM runs but whose Kubernetes cannot be repaired
([When the seed does not answer](#when-the-seed-does-not-answer)).

### When the seed does not answer

A seed that is down does not by itself change the record: the installation stays `ready` in the list,
in the setup status and in `GET …/installations/{id}`, and the plan shows no change, until a run is
started and fails (below). Only the live read
tells: the page reads the seed each time it loads, shows **Needs attention** and an alert with the
seed's address, VM and node, and `GET …/installations/{id}?live=1` answers `live.phase: degraded`
with `live.code: seed_api_unreachable` and the same text in `live.error`. Nothing sends a
notification, so a seed that went down is noticed when someone opens the page. The message tells two
cases apart: the API **does not answer** (the VM is stopped, unreachable, or runs without its
Kubernetes), or it **refuses the stored credential** (for some seconds after a start; when it stays,
k3s keeps restarting or the token Secret was deleted on the seed).

**What keeps running.** The platform cluster, the tenant control planes and the databases on it do
not need the seed to run (seen on a lab: with the seed's API down for 18 minutes the cluster's API
address kept answering and no machine changed). What stops is what the seed does: Cluster API
replaces no failed platform machine, the seed's ArgoCD syncs no platform component, and no run can
finish.

**What the buttons do meanwhile.**

- **Apply this release** and **Retry**: stage 3 writes the seed's firewall rules again and starts
  the VM when it is stopped; stage 4 then waits 20 minutes for the seed's API and fails
  `seed_api_unreachable` (or `seed_not_booted` when the guest agent does not answer either). A seed
  that had become ready is never rebuilt, and nothing is deleted. So an apply repairs a seed that
  was only stopped, and no other.
- **A run that fails sets the installation to `failed`** (an apply, a Retry or a Remove). The page
  then shows the failed run with its stage and code instead of the live read: the alert about the
  seed and the table of components are gone, because the page asks for the live read only while
  the stored phase is `ready` (`?live=1` itself still reads the seed and carries `live.code`, with
  `live.phase: failed`). Nothing else changed. After the seed is repaired, **Retry** brings the
  installation back to `ready`; reloading the page does not.
- **Download seed kubeconfig**, **Download platform kubeconfig** (it reaches the cluster through the
  seed) and **Replace the seed credential** answer `502`.
- **Remove managed services**: the seed VM exists, so the run asks Cluster API on it to delete the
  cluster, retries, and fails `seed_api_unreachable`. The option for a missing seed does not apply
  while the VM exists.

**What to check, in this order.** The seed's VMID and node are in the alert and in `status.seed`.

1. The VM runs: `qm status <vmid>` on its node. Start a stopped one (`qm start <vmid>`, or **Apply
   this release**).
2. The ProxCloud host reaches it: `curl -k https://<seed address>:6443/readyz`. A timeout is a route
   or a firewall (the seed API sources of the settings); `connection refused` means the VM answers
   and its Kubernetes does not listen; `401` means it runs (the request carries no token).
3. Inside the VM, as root on the node through the guest agent (no login is needed; the seed has a
   serial console, `qm terminal <vmid>`, but no password, so a login takes the SSH keys of the
   settings): `qm guest exec <vmid> -- systemctl is-active k3s`, `qm guest exec <vmid> -- journalctl -u k3s -n 50 --no-pager`
   and `qm guest exec <vmid> -- grep ' / ' /proc/mounts`.
4. A root filesystem line that ends in `emergency_ro` (or reads `ro`) means ext4 found an error and
   takes no more writes: k3s cannot start, and `mount -o remount,rw` does not clear it. The known
   cause is the image: a seed built from an Ubuntu 26.04 image exported before 2026-11-16 has a
   stale field in its ext4 superblock, and its root goes read-only at a reboot
   ([template updates, known issue](template-updates.md#known-issue-ubuntu-2604-images-exported-before-the-orphan-file-fix)).
   **Do not run a plain `e2fsck -f`, `-p` or `-y` on it**: before the field is zero the check
   deletes the file in inode 12 and leaves the field, so the fault comes back. The repair is the one
   of that section, on the Proxmox node, with the VM shut down. On ZFS the seed's partitions are
   `/dev/zvol/<pool>/vm-<vmid>-disk-0-part1` (root) and `-part13` (`/boot`); both are ext4.
   1. Test from the node (reads only; `AFFECTED` names a partition to repair):

      ```sh
      for d in /dev/zvol/<pool>/vm-<vmid>-disk-0-part1 /dev/zvol/<pool>/vm-<vmid>-disk-0-part13; do
        f=$(dd if=$d bs=1 skip=1116 count=4 2>/dev/null | od -An -tx4 | tr -d ' ')
        i=$(dd if=$d bs=1 skip=1664 count=4 2>/dev/null | od -An -tu4 | tr -d ' ')
        if [ $((0x$f & 0x1000)) -eq 0 ] && [ "$i" != 0 ]; then echo "AFFECTED $d s_orphan_file_inum=$i"; else echo "ok $d"; fi
      done
      ```

   2. Keep a way back: `zfs snapshot <pool>/vm-<vmid>-disk-0@before-repair`. Then
      `qm shutdown <vmid>` and wait until `qm status <vmid>` reads `stopped`.
   3. For each `AFFECTED` partition, these three commands in this order (why, and what each exit
      status means: the linked section):

      ```sh
      D=/dev/zvol/<pool>/vm-<vmid>-disk-0-part1     # then again with -part13
      e2fsck -E journal_only -p "$D"
      printf 'ssv orphan_file_inum 0\nclose -a\n' > /tmp/orphan-fix.cmd
      debugfs -w -f /tmp/orphan-fix.cmd "$D"
      [ "$(dd if="$D" bs=1 skip=1664 count=4 2>/dev/null | od -An -tu4 | tr -d ' ')" = 0 ] && e2fsck -f -p "$D"
      ```

      If the last `e2fsck` does not run, or exits 4 or more, stop: do not start the VM.
   4. Start the VM afterwards: `qm start <vmid>`. The seed's API answers again within about a
      minute (`curl -k https://<seed address>:6443/readyz` reads `401`). Then **Retry** when an
      apply or a Remove was started meanwhile and the installation reads `failed`, or reload the
      page until it reads the seed again when none was. The snapshot goes with the seed's disk at a
      Remove; delete it yourself (`zfs destroy <pool>/vm-<vmid>-disk-0@before-repair`) once the
      seed has run for a while.

   Run once on a lab, on 2026-11-17, on a seed on ZFS with e2fsprogs 1.47.2: both partitions were
   affected, every step exited 0, the file in inode 12 was kept, the seed's API answered 41 seconds
   after the start and all components read Synced and Healthy. Not run: a store other than ZFS
   (the partition devices are then the store's: an LVM volume needs `kpartx -a` first), and a root
   that reads `ro` for another reason, where the test prints `ok` and this repair does not apply.
5. If the seed cannot be repaired: restore the seed VM from your backup, over the same VMID. Stage 3
   sets protection on the seed, and Proxmox refuses a restore over a protected VM
   (`unable to restore VM <vmid> - - protection mode enabled`), so the protection comes off first.

   **Choose the archive first: it must be newer than the last replacement of the seed credential.**
   **Replace the seed credential** makes a new token Secret on the seed, stores its token and deletes
   the older Secrets. An archive from before holds only the older Secret, whose token ProxCloud no
   longer stores: the restored seed answers the stored credential with `401`, **Apply this release**
   fails `seed_api_unreachable` after five refusals, and nothing makes a new credential on a seed
   that had become ready (stage 5 uses the bootstrap token only while no credential is stored, and
   that token expired 3 hours after the seed was built). `qmrestore --force` has by then replaced
   the disk of the old seed, and what is left is step 6. This is read from the code, not tried on a
   lab. So compare the archive's time (`pvesm list <backup store> --vmid <vmid>`) with the last
   `ROTATE_CREDENTIAL` entry of the installation in the audit log, whatever its outcome, and take
   only an archive that is newer. The audit log misses a rotation made on a `dev-` build of main from
   before pull request #288 (no release has that older behaviour): there the rotation is logged as
   `CREATE` on the installation, not as `ROTATE_CREDENTIAL`. On an installation that ran such a
   build, also compare the generation in `status.seed.tokenSecret`
   (`proxcloud-portal-token-g<n>`) with the one the installation had when the archive was made.
   The newest archives may hold the fault you restore for; when no
   archive is both newer than that entry and sound, do not restore over the seed: step 6 applies.

   As root on the seed's node:

   ```sh
   qm config <vmid> | grep -E '^(net0|protection):'    # keep the output: the MAC to compare with
   cp /etc/pve/firewall/<vmid>.fw /root/<vmid>.fw.before-restore
   qm shutdown <vmid>                                    # qm stop <vmid> when the guest does not answer: its disk is replaced
   qm set <vmid> --protection 0
   qmrestore <archive> <vmid> --force 1 --storage <boot store>
   qm config <vmid> | grep -E '^(net0|protection):'
   diff /root/<vmid>.fw.before-restore /etc/pve/firewall/<vmid>.fw \
     || cp /root/<vmid>.fw.before-restore /etc/pve/firewall/<vmid>.fw    # missing or changed: put the saved one back
   qm set <vmid> --protection 1                          # only when the config shows no "protection: 1"
   pvesh get /pools/proxcloud-seed                       # the seed VM must still be a member
   qm start <vmid>
   ```

   `<archive>` is the archive's file (`pvesm list <backup store> --vmid <vmid>` names the volume,
   `pvesm path <volume>` prints its path); `<boot store>` is the boot store of the settings.
   - **The same VMID, and never `--unique`.** The installation records the seed by its VMID, its
     firewall file is `<vmid>.fw`, and the seed's network configuration and address go with the MAC
     of `net0`. `--unique` gives `net0` a new MAC; another VMID is a VM ProxCloud does not know.
   - **Check before the start**: `net0` carries the same MAC as before, the config reads
     `protection: 1` (set it back if the archive did not carry it), and `/etc/pve/firewall/<vmid>.fw`
     is there with policy DROP and a rule for 6443. If the file is missing or differs from the copy
     saved before the restore, copy the saved one back
     (`cp /root/<vmid>.fw.before-restore /etc/pve/firewall/<vmid>.fw`) before `qm start`: without it
     the seed's API on 6443 is open to its network until an apply writes the rules again. An apply
     does not set the protection again (stage 3 sets it when it attaches the first-boot CD only).
     The VM must also still be a member of the seed pool (`proxcloud-seed`): the `NoAccess` rows
     that keep the portal token and the `csi@pve` tokens from the seed and from its backups hold
     through that membership. On the lab Proxmox kept it over `--force`; if it is gone, put the VM
     back (`pveum pool modify proxcloud-seed --vms <vmid>`, not run on a lab) before the start.
   - **One config line changes and is harmless**: `vmgenid`. Proxmox gives a restored VM a new
     generation id, so a `diff` of the whole config before and after shows that line and no other.
     k3s, ArgoCD and Cluster API came back with it.
   - Then wait until `curl -k https://<seed address>:6443/readyz` reads `401`, and press
     **Apply this release**.
   - **Times to expect**, from the lab (a 20 GiB disk on ZFS on slow disks, the 2.39 GiB zstd
     archive on NFS): `qm shutdown` 14 seconds, `qmrestore` 2 minutes 8 seconds, `401` 45 seconds
     after `qm start`; the seed's API was away 3 minutes 32 seconds in all. The installation's live
     view read all 14 components Synced and Healthy 41 seconds after the `401`, and the apply took
     6 seconds. `qmrestore` also prints the rescan of every other store of the node; return code 0
     is the signal.
   - **What keeps working**: the platform cluster's API and the tenant clusters' APIs answered
     throughout. A break-glass kubeconfig issued before the backup was made still works on the
     restored seed until its hour is over (the same service account and signing key).
   - **Afterwards** delete `/root/<vmid>.fw.before-restore`: nothing removes it. It holds no secret.

   **Run once on a lab (2026-11-17, main cb2f052)**: a healthy seed restored over itself from an
   archive made three minutes earlier (`vzdump --mode snapshot --compress zstd` by hand), with no
   credential rotation and no machine change in between. Every step went as written above. The
   restored VM kept its MAC and address, `protection: 1` (it came back from the archive, so the
   `qm set --protection 1` was not needed), its firewall file (Proxmox wrote it from the archive,
   equal to the saved copy, so the `cp` did not run) and its pool. The stored seed credential
   worked, and Cluster API found its state equal to the cluster and wrote nothing: the four
   Machines kept their resourceVersions, no machine was replaced, no VM was cloned or deleted. The
   platform cluster and a tenant cluster with three workers were not touched (no pod restarted
   there), and **Apply this release** then succeeded. Seen before the sequence existed: with the
   VM shut down and still protected, `qmrestore … --force 1` ended with the message above and
   changed nothing.

   **Not run**: an archive older than a credential rotation (the `401` path above is read from the
   code) or older than a machine change (Cluster API meeting machines it does not know, or missing
   ones it does); `qm stop` on a guest that does not answer; `--unique`; a boot store other than
   ZFS; an archive made by the scheduled backup job, with or without pruning.
6. Without a backup the seed cannot be brought back: an apply never rebuilds a ready seed, because a
   new one would know nothing of the cluster's machines. What is left is the path of a lost seed:
   delete the tenant clusters and databases you can from their projects, delete the seed VM in
   Proxmox yourself (`qm set <vmid> --protection 0`, `qm stop <vmid>`, `qm destroy <vmid>`), then
   **Remove managed services** with the option for a missing seed ([Remove](#remove)), and install
   again. That deletes the platform cluster's machines and volumes, and with them every tenant
   control plane and database still on it.

Reboot or shut down a seed through the guest (`qm reboot`, `qm shutdown`), not with `qm stop`, and
put the backup job in place before you need it: it is the only way back that keeps the platform
cluster.

### After a Proxmox node reboots

Proxmox starts, at boot, only the VMs that have `onboot`. Up to v0.8.0 only the seed had it: after a
reboot of the node the platform cluster's machines stayed stopped until someone started them, and
with them every tenant control plane and database. This was seen on a production site on v0.8.0.

From v0.8.1 stage 7 of every install and of every **Apply this release** ends by setting `onboot`
on each VM of the platform that lacks it (the VMs of the pools `proxcloud-seed` and
`proxcloud-system` with the installation's tag), with the CAPI token, which holds
`VM.Config.Options` there. No chart and no BOM changes for it. A site installed on v0.8.0 gets it
at its first apply on v0.8.1; until then, and at any time, as root on the node:

```
qm set <vmid> --onboot 1
```

What this does not do, and what to do then:

- **A machine Cluster API replaced** (a changed node group, a rollout, a replaced worker) is a new
  VM without `onboot` until the next apply, when its node image template has none. The Check's
  `machines` row names every such VM. See "Start at boot from the template" below.
- **A stopped machine is not started by ProxCloud.** The `machines` row names it with its node;
  start it in Proxmox or with `qm start <vmid>`. Start the control plane machine before the
  others if you start them by hand.
- **No start order is set.** Proxmox takes `Sys.Modify` on `/` to change a VM's `startup` option,
  and the CAPI token does not hold it (the role is not widened for this). Without an order Proxmox
  starts the VMs one after another by VMID. To have the seed first, then the control plane, then
  the rest, as root: `qm set <vmid> --startup order=1` on the seed, `order=2` on the control
  plane machine, `order=3` on the others. A replaced machine loses it.
- **Tenant clusters' machines** are not touched by this: their VMs are made by the platform's
  CAPMOX in tenant pools. They take `onboot` from their node image template, and the provider sync
  sets it on the running workers that lack it (next paragraphs).

**Start at boot from the template.** From v0.9.0 a Kubernetes node image that **Template updates**
imports gets `onboot: 1` on the template itself. A Proxmox clone copies every option of its source
except the disks, MACs and snapshot fields it rewrites, and CAPMOX v0.9.2 has no field for `onboot`
and writes it nowhere (both read in code: `PVE/API2/Qemu.pm` on Proxmox VE 9.2, CAPMOX's
`internal/service/vmservice`). So every machine cloned from such a template has it from its first
boot: tenant workers, platform machines, and the replacement of either, with no call per VM. Stage
7's pass above stays as the backstop for machines made from older templates.

**Templates and workers from before v0.9.0.** A node image imported by v0.8.x has no `onboot`, and
neither has a worker cloned from it. The services sync (every 15 minutes; on demand
`POST /api/v1/admin/job/external-service/sync` on the operator API) closes both, per Proxmox
provider, with the portal's token and no new Proxmox privilege (`VM.Config.Options`, which its role
holds on `pc-templates` and on the tenant pools):

- **Node templates:** `onboot: 1` on every Kubernetes node template that a Template updates import
  of this portal for this provider still holds (the import's receipt: node, VM id, name and source
  digest), staged or published. Clones made after that have it. A **withdrawn** import (tag
  `pc-tu-withdrawn`) is neither read nor written: no new node group gets it. A cluster created on
  it before the withdraw keeps cloning from it (scale up, Replace, a health check's replacement),
  and such a worker has no `onboot` until the worker pass below sets it at the next sync.
- **Workers:** `onboot: 1` on every running worker VM of a tenant cluster of this portal that lacks
  it: a VM in a project's pool with the tag `pc-k8s-<cluster id>`, where that project has that
  cluster. A worker that is not running is left for a later sync, because CAPMOX still writes a new
  clone until its first start.

Both write only what lacks the option, so a sync that finds nothing to do writes nothing. A failed
write is logged (`imported templates start at boot and display`, `kubernetes workers start at boot`) and
tried again at the next sync. After an upgrade from v0.8.x, wait for one sync before a planned node
reboot, or check with `qm config <vmid> | grep onboot`.

What this does not cover:

- A template built by the installer's image pipeline (`images/build.py`), or any template no
  import receipt of this portal holds (the site's own, another portal's), is never written. To add
  it, as root on the node: `qm set <template vmid> --onboot 1`. Workers cloned from it are still
  covered by the sync's worker pass.
- Workers of a provider with `sharedWorkers` are not in a project's pool and are not written: they
  have `onboot` only from their template, or by `qm set <vmid> --onboot 1`.
- A portal whose scheduler is off runs no sync on its own: call the route above once after the
  upgrade. A sync of one project alone does not run these passes.
- A tenant worker that is stopped stays stopped until its node boots: nothing starts it.
- **Not tried on a lab:** that a clone made by CAPMOX carries the option, that Proxmox accepts
  `onboot` on a template, and that it skips a template when it starts the VMs of a booting node
  (it should: a template cannot be started). The template-updates smoke test sets `onboot: 0` on
  its own clone, as before.

Seen on a lab (2026-11-18, a platform installed by v0.8.0, then the build `dev-751a190` of main): the
`machines` row warned for the four machines without `onboot`, the apply set it with the CAPI token,
CAPMOX v0.9.2 had not taken it back 23 minutes later, and the row then read ok. Not verified on a
lab: a stopped machine, a machine the token cannot read, an install on this release from the start,
and a node reboot with `onboot` in place. The privileges (`VM.Config.Options` for `onboot`, and `Sys.Modify` on `/`
besides for `startup`) are read from Proxmox VE's API code (qemu-server, `PVE/API2/Qemu.pm`), and the seed's own `onboot` has been set with the same
token and role since the first install. That Proxmox starts VMs without a start order by VMID is from
its documentation as remembered, and was not checked in its code or on a node.

## 9. Limits on the Compose install

- **No block volumes for tenant Kubernetes clusters.** They come through the CSI gateway with a gateway token
  on the compute provider, and the Compose path writes no rows for one. Tenant clusters get no proxmox-csi
  StorageClass, also with the gateway listener on; the Kubernetes provider is registered with `rwxGiB: 0`, so no
  NAS dataset either.
- **The gateway listener is opt-in.** `proxcloud-up.sh install --cluster-gateway <address>` (v0.11.0 or later)
  serves the listener of the clusters' controllers on `<address>:8444` only, with an ECDSA key and a
  self-signed certificate for the address that the API makes on its first start, in its own volume
  ([compose README](../deploy/compose/README.md#optional-features)). It is for the clusters' load balancer
  controller (cluster chart 0.1.17, which the BOM of v0.11.0 pins for new tenant clusters; 0.1.19 from v0.11.1). A Kubernetes cluster gets
  one only with the listener on, the controller image the flag sets, and `config.cluster.loadBalancer` on the
  Kubernetes provider (a public network and a flavor; [managed-k8s.md](managed-k8s.md)). **A new route:** the platform
  cluster's nodes, on the platform network, must reach the portal host on `<address>:8444`; without it no controller
  reaches the gateway (its cluster page says it has not reported), and nothing else breaks.
- **No database backups.** The Databases provider is registered without a backup store, and with the
  egress fence on (`egressFence: true`), which allows no object store.
- **Database replicas.** PostgreSQL 17 and 18; three replicas are offered only when the `dbaas`
  group spans three or more Proxmox nodes, otherwise one.
- **No node image for tenants until it is published.** The install imports the node image and does
  not publish it, so the Kubernetes provider is registered with no version until the ready card's
  button, or you in Template updates, published it
  ([what a tenant can use](#what-a-tenant-can-use-after-the-install)). Until then every cluster
  create is refused with a `400` that says so
  (`this provider offers no Kubernetes version yet`), before anything is reserved or a token is made,
  and an upgrade of an existing cluster is refused the same way (`version is not offered by this
  provider`).
- **Tenant clusters need a default zone without DHCP** on the Proxmox provider (EVPN, VLAN, or a
  `simple` zone made without it: `"dhcp": false` in the zone's config, or made by hand). On a
  default zone with DHCP (a `simple` zone that ProxCloud created by its default) the Check warns
  (`tenant-network`), the install still registers and publishes the Kubernetes provider, and a
  cluster create on that zone's networks is refused with a `400`, unless the provider uses shared
  workers (`cluster.shared`) ([Prerequisites](#2-prerequisites)).
- **A database takes at least one whole CPU.** `cpu` is a whole number from 1 up (no maximum unless
  the provider sets one), and the default `dbaas` machine has 2 cores with a little requested by the
  platform's own pods, so it holds one database of the smallest size. Give the `dbaas` group more
  cores or machines for more.
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
- The platform cluster's ArgoCD has an address only when the settings name one
  (`spec.access.argocd`), and then for the named operator networks only, by
  `loadBalancerSourceRanges` on a MetalLB Service in the pool tenants reach. Its `admin` is in effect
  cluster-admin on the cluster that hosts every tenant control plane; the password is ArgoCD's
  initial secret and nothing rotates it. Where tenant traffic leaves under a node's address, an
  operator network that contains that address admits every tenant to the login page (the Check row
  `argocd-access` warns). That the source ranges are enforced on the platform cluster was seen on a
  lab for a layout without SNAT only. The seed's ArgoCD never has an address
  ([An address for the platform cluster's ArgoCD](#an-address-for-the-platform-clusters-argocd)).
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

## Upgrade between BOMs

One upgrade has run on a lab (2026-11-19, from the BOM of 0.8.0 to that of the candidate
`v0.9.0-rc.1`, on a platform without a tenant cluster or a database): one run of 72.8 seconds,
no machine replaced, no pod restarted. On the candidate `v0.9.0-rc.2` the same lab saw the answer
`409 bom_installed` and the plan that lists and blocks a control plane that does not fit. One
upgrade has run on a production site (2026-11-19: a single-node site with one tenant cluster and
one database, 0.8.0 to 0.9.0): one run of 68.6 seconds, nothing rolled, no tenant request failed
in 149 looks. A third ran on the lab on 2026-11-20, from the BOM of 0.9.1 to that of the candidate
`v0.10.0-rc.1`, with one tenant server and no tenant cluster or database: four changes, one run of
22.8 seconds, no machine replaced, no VM restarted. Nothing else in this section has run on a lab or on a production site. It says what
the code and its tests do; [the lab tests it still owes](#what-the-upgrade-still-owes-a-lab) are
listed at its end.

A ProxCloud upgrade does not change an installed platform. The platform stays on the BOM that
installed it, and **Apply this release** keeps running with that BOM. Moving the platform to the BOM
of the ProxCloud release that now runs is a separate run, an **upgrade**, which the operator starts.

### What this release moves, and what it refuses

The plan in step 4 (`GET …/plan`, `upgrade`) compares the installed BOM with this release's and
lists every difference. The upgrade is allowed when every difference is one of these:

| Entry | Kind | What the run does |
|---|---|---|
| `release` | record only | Nothing is synced. The record and the label `proxcloud.io/bom-release` of the platform's objects name the new release |
| `tenantCharts.kubernetes`, `tenantCharts.database` | record only | Stage 10 writes the chart on the two providers. New tenant clusters and databases get it; an existing one keeps its chart until its own chart upgrade |
| seed `capi`, platform cluster `capi-tenant` (chart `proxcloud-platform`) | chart | The Application moves to the new chart version. ArgoCD syncs, and runs the clusterctl hook again, only when the new chart renders an object differently; the hook then finds every provider at its version and changes nothing |
| platform cluster `platform` (chart `proxcloud-platform`) | chart | The Application moves to the new chart version. ArgoCD syncs what the new chart renders differently of provider RBAC, tenant AppProjects and the MetalLB pool |
| seed `pc-platform` (chart `proxmox-kamaji-cluster`) | chart | The Application moves to the new chart version. ArgoCD syncs the Cluster API objects of the platform cluster that the new chart renders differently, after the check that no machine is replaced (below). On a move that replaces machines it moves with the first step of the roll |
| `kubernetes.version` (the same, a newer patch or exactly one minor up), `kubernetes.nodeImage`, and `images.kubeVip` together with a new node image, when the target's cluster chart is 0.1.15 or newer and the installation uses the BOM's node image | machines | Cluster API replaces every machine of the platform cluster, ordered by the run ([The platform cluster's machines](#the-platform-clusters-machines)). The plan then begins with the line that there is no way back and has one row `roll/<unit>` per unit with what stops meanwhile, and one row `roll-vm/<unit>` of kind `site` with what is made and deleted on Proxmox |
| seed and platform cluster `cert-manager` (chart `cert-manager` of the chart mirror), in an upgrade with no `seed.k3s` and no `machines` row | chart | The Application moves to the new chart version in the component's wave (1 on both clusters). ArgoCD applies the CRDs in place (the chart renders them with `crds.enabled: true`; `v1` stays the only version) and removes what the new chart no longer renders. The controller, cainjector and webhook Deployments roll together, each keeping its old pod until the new one is Ready. The wave waits for the Application to be Synced and Healthy and for its sync to have Succeeded, which includes the chart's `startupapicheck` Job (a dry-run Certificate against the webhook). Certificates and their Secrets stay. No machine is replaced, nothing is written on Proxmox and no seed backup is asked for. See [A cert-manager move](#a-cert-manager-move) |
| `seed.k3s`, to a version that is newer by a patch, by a k3s build (`+k3s2` after `+k3s1`) or by exactly one minor | seed | Stage 5a replaces the k3s binary on the running seed VM and restarts k3s ([The seed's k3s](#the-seeds-k3s)). With it the plan lists four rows of kind `site`, what the stage writes on the seed: the Job, the etcd snapshot, the k3s config drop-in without leader election and the binary (its row gives the way back by hand) |

One more row can be in the plan that is not a difference of the two BOMs: `pve-api-rule`, kind
`site`, when the cluster firewall does not hold the rule for the Proxmox API
([step 4](#step-4-review-and-install)). It never refuses an upgrade.

The BOM after 0.10.0 is the first that names another k3s and another Kubernetes: k3s
`v1.36.6+k3s1` and Kubernetes 1.36.6 with its node image, one minor up from the `v1.35.10+k3s1` and
1.35.10 of every BOM before it. So every upgrade to it has the `seed.k3s` row, the `machines` rows
and the two parts, from whichever earlier BOM it starts
([Upgrade to Kubernetes 1.36.6](#upgrade-to-kubernetes-1366)). Until that BOM no upgrade between
released BOMs had either row. The rows and their stages have run in tests only, against a fake seed
and a fake Cluster API: **no such upgrade has run on a lab or on a site.** No third-party chart and
no Cluster API provider moves with it; two of the charts are then outside the Kubernetes versions
their own documents list.

The BOM of 0.10.2 (the release `v0.10.2`) moves cert-manager from v1.20.2 to v1.21.3 on the seed
and on the platform cluster (both entries list v1.20.2 in `upgradeFrom`), and nothing else: v1.21.3
lists Kubernetes 1.33 to 1.36. From the BOM of 0.10.1 or of 0.10.1-rc.2 the plan has three rows: the
release (record only) and `cert-manager` on the seed and on the platform cluster (chart, wave 1),
with no part and no seed, machines or site row; phrase `upgrade <name> to 0.10.2`. From 0.10.1-rc.1
it also has `pc-platform` 0.1.15 to 0.1.16 and `tenantCharts.kubernetes`, as the move to 0.10.1 had
(five rows). From the BOM of the candidate `0.10.2-rc.1`, which has the same pins, one row, the
release, record only. From every BOM on Kubernetes 1.35.10 (0.8.0 to 0.10.0 and their candidates)
the upgrade is refused: it would move k3s and Kubernetes too, and both cert-manager rows carry the
sentence of [A cert-manager move](#a-cert-manager-move). Such a platform upgrades to 0.10.1 with the
`v0.10.1` build first, both parts, then with this one. The plans are `PlanFor`'s and
`TestPreviousBOMsMoveCertManager`'s. The candidate `v0.10.2-rc.1` carried these pins first, as
`0.10.2-rc.1`; its drill on one lab (2026-11-21) ran the move from 0.10.1
([A cert-manager move](#a-cert-manager-move) has its figures); the moves from 0.10.1-rc.1 and
0.10.1-rc.2 were read, not run ([ci-release.md](ci-release.md#release-candidates)). Neither carries
an unfinished replacement of the machines: an installation that stands in one toward 0.10.1 or a
candidate of it goes on to that BOM with a Retry, and the move to this one is refused until the
replacement is finished.

The BOM of the candidate `v0.11.0-rc.1` (`0.11.0-rc.1`) has the pins of 0.10.2 but
`tenantCharts.kubernetes`: cluster chart `proxmox-kamaji-cluster` 0.1.17, which brings the load
balancer controller to new tenant clusters ([managed-k8s.md](managed-k8s.md)); the platform cluster
stays on 0.1.16. From the BOM of 0.10.2 or of 0.10.2-rc.1 the plan has two rows, the release and
`tenantCharts.kubernetes`, both record only, with no part and no seed, machines or site row; phrase
`upgrade <name> to 0.11.0-rc.1`. From 0.10.1 and 0.10.1-rc.2 it also has the two cert-manager rows
(four), from 0.10.1-rc.1 `pc-platform` 0.1.15 to 0.1.16 as well (five); from every BOM on Kubernetes
1.35.10 it is refused as above. The move from 0.10.2-rc.1 ran on one lab (two record-only rows, 39.6 s, no
Proxmox write; [ci-release.md](ci-release.md#release-candidates)).

The BOM of the candidate `v0.11.0-rc.2` (`0.11.0-rc.2`) has the pins of 0.11.0-rc.1: from that BOM the
plan has one row, the release, record only, phrase `upgrade <name> to 0.11.0-rc.2`; from every older
BOM it has the rows and refusals of 0.11.0-rc.1 above. The candidate corrects the load balancer after
the lab drill of `v0.11.0-rc.1`. The move from 0.11.0-rc.1 ran on one lab (2026-11-22): one record-only
row, 8.5 s, no VM restarted.

The BOM of the release `v0.11.0` (`0.11.0`) has the pins of both candidates: from 0.11.0-rc.1 and
0.11.0-rc.2 the plan has one row, the release, record only, phrase `upgrade <name> to 0.11.0`; from
0.10.2 and 0.10.2-rc.1 two rows, the release and `tenantCharts.kubernetes` 0.1.16 to 0.1.17, both
record only, with no part and no seed, machines or site row; from 0.10.1 and 0.10.1-rc.2 also the two
cert-manager rows (four), from 0.10.1-rc.1 `pc-platform` 0.1.15 to 0.1.16 as well (five); from every
BOM on Kubernetes 1.35.10 it is refused as above. The run writes chart 0.1.17 on the Kubernetes
provider, so clusters created after it can get the load balancer controller
([managed-k8s.md](managed-k8s.md)); it changes no existing cluster. No move to 0.11.0 itself has run on
a lab: the two candidates' runs, with these pins, did.

The BOM of the release `v0.11.1` (`0.11.1`) has the pins of 0.11.0 but
`tenantCharts.kubernetes`: cluster chart `proxmox-kamaji-cluster` 0.1.19, whose load balancer controller
watches the tenant's Services, nodes and TLS Secrets ([managed-k8s.md](managed-k8s.md), "Upgrade note for
v0.11.1"); the platform cluster stays on 0.1.16, and no component of the sandbox feature is in it. From
0.11.0, 0.11.0-rc.1 and 0.11.0-rc.2 the plan has two rows, the release and `tenantCharts.kubernetes`
0.1.17 to 0.1.19, both record only, with no part and no seed, machines or site row; from 0.10.2 and
0.10.2-rc.1 the same two rows (the chart from 0.1.16); from 0.10.1 and 0.10.1-rc.2 also the two
cert-manager rows (four), from 0.10.1-rc.1 `pc-platform` 0.1.15 to 0.1.16 as well (five); from every
BOM on Kubernetes 1.35.10 it is refused as above (`TestPreviousBOMsMoveCertManager`). The run writes
chart 0.1.19 on the Kubernetes provider and changes no existing cluster: a cluster made earlier gets UDP
and TLS from its Services when it is moved to the provider's chart. The phrase is `upgrade <name> to 0.11.1`,
and from each of its four candidates the plan has one row, the release, record only. No move to 0.11.1
itself has run on a lab: the four candidates' runs, with these pins, did (below).

The BOM of the candidate `v0.11.1-rc.1` (`0.11.1-rc.1`) is that BOM under the candidate's name: the
rows and refusals above, phrase `upgrade <name> to 0.11.1-rc.1`. The candidate exists for the lab drill
of the complete layer 4 load balancer ([ci-release.md](ci-release.md#release-candidates)). One move to
it ran on a lab, from 0.11.0 (2026-11-23): the plan had the two record-only rows, and the run replaced no
machine. Later builds keep it as a previous BOM: from 0.11.1-rc.1 the plan to a BOM with its pins has one
row, the release, record only.

The BOM of the candidate `v0.11.1-rc.2` (`0.11.1-rc.2`) has the pins of `0.11.1-rc.1`: the rows and
refusals above, phrase `upgrade <name> to 0.11.1-rc.2`, and from 0.11.1-rc.1 the release alone. One move
to it ran on a lab, from 0.11.1-rc.1 (2026-11-23): the plan had that one row, and the run replaced no
machine. Later builds keep it as a previous BOM: from 0.11.1-rc.2 the plan to a BOM with its pins has one
row, the release, record only.

The BOM of the candidate `v0.11.1-rc.3` (`0.11.1-rc.3`) has the same pins: the rows and refusals above,
phrase `upgrade <name> to 0.11.1-rc.3`, and from 0.11.1-rc.1 and 0.11.1-rc.2 the release alone. One move
to it ran on a lab, from 0.11.1-rc.2 (2026-11-24): the plan had that one row, and the run replaced no
machine. Later builds keep it as a previous BOM: from 0.11.1-rc.3 the plan to a BOM with its pins has one
row, the release, record only.

The BOM of the candidate `v0.11.1-rc.4` (`0.11.1-rc.4`) has the same pins: the rows and refusals above,
phrase `upgrade <name> to 0.11.1-rc.4`, and from 0.11.1-rc.1, 0.11.1-rc.2 and 0.11.1-rc.3 the release
alone. One move to it ran on a lab, from 0.11.1-rc.3 (2026-11-25): the plan had that one row, the run took
8.2 s and replaced no machine. Later builds keep it as a previous BOM: from 0.11.1-rc.4 the plan to a BOM
with its pins has one row, the release, record only.

"Moves" means that the Application's target is the new chart version and ArgoCD has compared the
cluster with it. ArgoCD starts a sync only for an Application that is out of sync, and a hook is
not part of the comparison: a chart version that renders the same objects as the one before is
recorded as moved with no sync and no hook run. In ArgoCD such an Application is Synced against
the new version while its last sync and its history still name the old one; that is the upgrade
having landed, not a failure. Seen on the lab on 2026-11-19 (0.8.0 to 0.9.0-rc.1): of the four
chart moves only `platform` was synced, and no clusterctl hook ran.

A chart of the product, and cert-manager, moves only to a version whose BOM entry lists the installed
version in `upgradeFrom`: the release checked that pair ([ci-release.md](ci-release.md#what-a-release-checks-about-its-bom-pairs);
for cert-manager the release read upstream's upgrade notes for that pair and compared the render,
`TestCertManagerMoveRenders`).

#### A cert-manager move

cert-manager is the one third-party chart an upgrade moves, on the seed and on the platform cluster,
and only in an upgrade that changes no k3s, Kubernetes version or node image. The seed's charts sync
in stage 6, after the seed's k3s (stage 5a), and the platform cluster's in stage 9, after the roll: in
one run the old chart would meet the new Kubernetes first. A release that moves cert-manager next to
such a change refuses the upgrade with one line per cert-manager row: "not supported by this release:
a third-party chart moves only in an upgrade that changes no k3s, Kubernetes version or node image,
and this one also changes …. First finish an upgrade to a release with this one's Kubernetes version
(…), k3s (…) and node image (…), with that release's build, then start this one". Upgrade with the
build of that release first (its BOM is the target an apply of that build offers), then with this one.

There is no way back: a platform does not go back to a BOM it left, and cert-manager documents no
downgrade. Its upgrade guide advises a backup of the cert-manager resources first; on the seed and on
the platform cluster:

```sh
kubectl get certificates,issuers,clusterissuers -A -o yaml > cert-manager-backup.yaml
```

Read the preflight row `certificates` before and after ([api.md](api.md)): it is `ok` when the three
cert-manager Deployments are available on both clusters and every Certificate is Ready, and a warning
that names each one that is not. It never fails and no run waits on it, so a Certificate that was not
Ready before the upgrade (an Issuer removed by hand) does not hold the run; fix it on its own. A
Certificate that is not Ready keeps the certificate its Secret holds until a new one is issued. The run's
own gate is the wave: if the chart's `startupapicheck` fails, the sync fails; the run starts it once
more, and when that one fails too the stage fails `component_unhealthy` with what the sync failed on.
Retry walks the waves again; cert-manager is recorded at its new version (`status.components`) only
after its wave passed.

**What holds the wave.** ArgoCD creates the chart's `startupapicheck` Job when the sync's hooks
start, not once the new webhook pod is Ready. On the lab it was created before the new webhook pod
was Ready on both clusters (on the seed 36 seconds before), and it checked the new webhook only
because its own image pull took longer. On a site that already has the v1.21.3 images the Job may
check the old webhook. The wave holds anyway: it waits until ArgoCD reports the three Deployments
rolled (the Application Healthy against the new chart, compared with the new source) and the sync,
the Job included, Succeeded.

**On a lab, one sample.** The drill of the candidate `v0.10.2-rc.1` (2026-11-21: one lab, a seed of
one VM, a platform cluster of four nodes on 1.36.6, one tenant cluster at 1.36.6 and one database,
probed every 2 seconds) moved a platform from the BOM of 0.10.1:

- **One run of 3 minutes 8 seconds**, no retry: stage `seed-platform` 98 seconds, stage
  `workload-platform` 84 seconds, every other stage under 4.1 seconds.
- **The rollout.** On each cluster the three new ReplicaSets appeared 9 to 11 seconds after ArgoCD's
  operation began, and their pods were Ready 20 to 53 seconds later (the images pulled from
  quay.io). Each Deployment kept a Ready pod in every look. The `startupapicheck` Job took 32 to 47
  seconds from created to succeeded. The CRDs stayed `v1` only (storedVersions `[v1]`), and the
  Role and RoleBinding `cert-manager-tokenrequest` were removed.
- **No Certificate issued again.** Each of the 11 (5 on the seed, 6 on the platform cluster) kept
  its revision, its Secret's serial and its notBefore, and was Ready in every look.
- **No webhook gap.** No server-side dry run against the webhooks of cert-manager or of Cluster API
  was refused, the CA bundles stayed as they were, and ArgoCD did not loop on Kamaji's Applications.
- **Tenants.** The tenant API and the database failed no look; no tenant pod restarted, no machine
  was replaced and nothing was written on Proxmox.
- **After the move.** A new Certificate was Ready in about 1 second on each cluster. A renewal of
  `capi-serving-cert` (`cmctl renew`) issued a new certificate in 1.5 to 1.7 seconds, and the
  cainjector put the new CA on Cluster API's webhook configuration and CRD within 3.4 seconds. A new
  tenant cluster and a new database were created and deleted.

**A seed of one VM is slow for about half a minute.** While the seed pulled and started the new
images its API answered every call slowly for about 30 seconds, cert-manager's and every other's
(up to 3.1 seconds, one client timeout of 4 seconds); nothing was refused. The platform cluster's
answers stayed under 1.1 seconds.

**Not drilled.** The installer path (stage 40 at `make upgrade`, stage 50 at the next `make
install`, and the two Certificates and the Issuer of plugin-barman-cloud on the services cluster);
the moves from 0.10.1-rc.1 and 0.10.1-rc.2; the refusal on a real platform on a BOM from 0.8.0 to
0.10.0 (a unit test only); a site that already has the v1.21.3 images; a failed `startupapicheck`,
`component_unhealthy` and the Retry after it; a seed of more than one node and a production site.
The figures are one lab's. There is no way back to try.

The rest of this subsection is the record of the moves to the BOM of 0.10.0 and the ones before
it, none of which replaced a machine. The plan to the BOM with Kubernetes 1.36.6 holds the same
rows and, in addition, the ones of [Upgrade to Kubernetes 1.36.6](#upgrade-to-kubernetes-1366).

From the BOM of 0.8.0, 0.8.1 or 0.8.2 to that of 0.10.0, the plan has these rows and no
other: `pc-platform` from cluster chart 0.1.13 to 0.1.15, `capi`, `capi-tenant` and `platform` from
platform chart 0.1.2 to 0.1.3, `tenantCharts.kubernetes` to 0.1.15, `tenantCharts.database` to 0.5.2,
and the release. All four
charts list the installed version in `upgradeFrom`, and the release's check found that chart
0.1.15 renders the platform cluster's control plane and templates as 0.1.13 and 0.1.14 did, so no machine is
replaced. After the upgrade the platform can go from 1 to 3 control plane machines, Replace works,
and new tenant clusters get the health check. From the BOM of 0.9.0, of one of its candidates or of 0.9.1 the plan has four changes:
`pc-platform` from 0.1.14 to 0.1.15, the two tenant charts and the release. A build of `main`, whose BOM is a `-dev` one,
refuses the move for that reason alone; the release `v0.10.0` allows it, as its candidate `v0.10.0-rc.1` did for a lab (the candidate's BOM is `0.10.0-rc.1`; run there from 0.9.1 on 2026-11-20, without a tenant cluster or database; from that BOM to `0.10.0` the plan has one change, the release, record only; no move to `0.10.0` itself was run); the releases `v0.9.0` and `v0.9.1` allowed the move to their BOM (chart 0.1.14), as the candidates
`v0.9.0-rc.1` and `v0.9.0-rc.2` did for a lab ([ci-release.md](ci-release.md#release-candidates)). From the BOM of 0.8.0 to
that of `v0.9.0-rc.1` it was run on a lab without tenants (2026-11-19): six changes, and no machine
replaced. Not run on a lab: the move from 0.8.1 or 0.8.2, the move to `0.9.0-rc.2`, to `0.9.0` or to `0.9.1`, three control
plane machines, and the health check on a cluster made before the upgrade. From the BOM of 0.9.0, or of
one of its candidates, to that of `0.9.1` the BOMs differ in the release line alone: the plan lists one
change, record only, and nothing is synced for it; not run on a lab either.

Every other difference refuses the whole upgrade, each with its own line in the plan and in the
`409 upgrade_refused` of a run. All lines begin with "not supported by this release":

| Entry | Why it is refused |
|---|---|
| `kubernetes.version` to an older version | "Kubernetes never goes back" |
| `kubernetes.version` more than one minor ahead | "one minor at a time: go through 1.<n+1> first" |
| `kubernetes.version`, `kubernetes.nodeImage` or `images.kubeVip` when the target's cluster chart is older than 0.1.15 | "the BOM names cluster chart X, which cannot order a rollout" |
| `kubernetes.nodeImage` on an installation with `nodeImage.mode: templates` | "this installation names its own node templates; the roll needs the BOM's image" |
| `seed.image` | Only a rebuilt seed would boot it, and a ready seed is never rebuilt |
| `seed.k3s` to an older version | "k3s never goes back" |
| `seed.k3s` more than one minor ahead | "one minor at a time: go through 1.<n+1> first" |
| `seed.k3s` at the same version with another sha256 or URL | A published k3s version does not change |
| `images.kubeVip` without a new node image | It is in the control plane's machine spec: the control plane machine would be replaced. kube-vip moves only together with a new node image, when the control plane is replaced anyway |
| `images.clusterctl`, `capi.seed.*`, `capi.workload.*` | The Cluster API providers are upgraded (`clusterctl upgrade`) only by a later release |
| A third-party chart but cert-manager: `argocd`, `cilium`, `metallb`, `cnpg`, `proxmox-csi`, `kamaji-crds`, `kamaji`, `kamaji-etcd`, even when its entry has `upgradeFrom` | Not in this release. Each restarts something tenants use (the CNI, the load balancer speaker, every database's operator, every tenant control plane's datastore), and none of those restarts has been measured |
| `cert-manager` in an upgrade that also has a `seed.k3s` or `machines` row | "a third-party chart moves only in an upgrade that changes no k3s, Kubernetes version or node image" ([A cert-manager move](#a-cert-manager-move)) |
| `cert-manager` at a version that does not list the installed one in `upgradeFrom`, or the same version with another digest | As for a chart of the product |
| A chart of the product at a version that does not list the installed one in `upgradeFrom`, or the same version with another digest | The release did not check that move |
| A component added or removed, or changed in its wave, install mode, namespace, node group, chart source or values | A run moves only a chart version |
| The target's `release` ends in `-dev` | No later build carries a development BOM, so the platform could afterwards be applied by that one build alone |

So a release whose cluster chart changes a machine or bootstrap template by itself (a NIC option
on the machines, a kubelet argument) still reaches new installs only: the roll replaces machines
for a new Kubernetes version or node image, and its check refuses any other template change next
to it. The release notes of such a release must say so.

### Upgrade to Kubernetes 1.36.6

**Run on one lab, and on one production site of one Proxmox node.** The candidate `v0.10.1-rc.1` was the first release with
this BOM ([ci-release.md](ci-release.md#release-candidates)). Its drill on 2026-11-20 (three Proxmox
nodes on one shared lab disk, one control plane machine, one tenant cluster and one database under a
write loop) ran part "seed" to the end and the control plane's unit of part "platform", and stopped
there: the old control plane machine's Node object stayed in the cluster and the run waited for a
node count that could not come true (`nodes_timeout`; corrected since, see
[The platform cluster's machines](#the-platform-clusters-machines)). The figures marked "one lab's
sample" below are that drill's and those of the second one: one run each on one lab, not a promise
for a site. The candidate `v0.10.1-rc.2` carries the corrections and cluster chart 0.1.16 in its
BOM, `0.10.1-rc.2`. Its drill on the same lab (2026-11-20, on the platform the first drill had left
after the control plane's unit) got out with one Retry, replaced the units `system`, `kamaji` and
`dbaas` with one more machine first, was paused once and resumed, and ended with every machine on
1.36.6 and no Node left behind. The release `v0.10.1` has the BOM `0.10.1`, with the pins of
`0.10.1-rc.2`; its code differs from that candidate's in the wait for the tenants between two
units, corrected after the second drill. One production site moved on `v0.10.1` (2026-11-20: one
Proxmox node, the platform's disks and its node template on one NVMe pool, one control plane
machine, one machine per group, one tenant cluster and one database of one instance); its figures
are below, marked "one-node site", and the corrected wait held there (after the `kamaji` unit the
run named the tenant control plane's Deployment while its TenantControlPlane still read Ready, and
stored the next step 33 seconds after the tenant API answered). Everything else is what the plan,
the code and its tests say, and is **not measured**: a unit replaced delete-first, stages 8 to 11 as
figures, a control plane of three, and the wait for the tenants between two units on a lab
([The platform cluster's machines](#the-platform-clusters-machines), point 5).

**What the BOM changes.** `kubernetes.version` 1.35.10 to 1.36.6, `kubernetes.nodeImage` to the
image built for 1.36.6 (`ubuntu-2604-k8s-1.36.6-20261116194043-72f0eaa3d0d4`, from the same source
commit and the same build chain as the 1.35.10 image it replaces), and `seed.k3s` `v1.35.10+k3s1` to
`v1.36.6+k3s1`. Next to them the BOMs `0.10.1-rc.2` and `0.10.1` name cluster chart 0.1.16 (for the platform
cluster and for new tenant clusters), which sets the node deletion timeout and replaces no machine
for it. Nothing else: no Cluster API provider, no clusterctl image, no kube-vip, no seed image.

**The plan, from every earlier BOM.** Nine of the eleven BOMs this release carries name 1.35.10
and `v1.35.10+k3s1`, so each is one minor from the target and the move is allowed from each. The
other two name 1.36.6 already and replace nothing: from `0.10.1-rc.1` the plan has three rows, the
release, the tenant chart and `pc-platform` 0.1.15 to 0.1.16; from `0.10.1-rc.2` one, the release,
record only. The plan
has, in this order: the row `roll` (there is no way back); `release`; `kubernetes.version` and
`kubernetes.nodeImage`, kind `machines`; `seed.k3s`, kind `seed`, with its four `site` rows
(`seed-k3s-job`: Job `proxcloud-system/k3s-upgrade-v1-36-6-k3s1`; `seed-etcd-snapshot`:
`pre-upgrade-v1.35.10-k3s1-<node>-<time>`; `seed-k3s-config`: the drop-in without leader election;
`seed-k3s-binary`, with the way back by hand across the minor); the chart rows of the starting BOM;
one row `roll/<unit>` per unit and one row `roll-vm/<unit>` per unit with a machine. With the control
plane and one machine each of `system`, `kamaji` and `dbaas` that is 19 rows from 0.10.0 or
0.10.0-rc.1 (`tenantCharts.kubernetes` and `pc-platform` 0.1.15 to 0.1.16), 20 from 0.9.0, 0.9.1 or
a candidate of 0.9.0 (the two tenant charts and
`pc-platform` 0.1.14 to 0.1.16), and 23 from 0.8.0, 0.8.1 or 0.8.2 (the two tenant charts,
`pc-platform` 0.1.13 to 0.1.16, and `capi`, `capi-tenant` and `platform` 0.1.2 to 0.1.3). (The
candidate `v0.10.1-rc.1`, whose BOM kept cluster chart 0.1.15, had 17 rows from 0.10.0.) From
each of the nine the cluster chart moves "with the first step of the roll": the release's check
renders the new chart with the old version and templates and finds the machines unchanged
([ci-release.md](ci-release.md#what-a-release-checks-about-its-bom-pairs)). The plan has two parts.
`seed`: phrase `upgrade <name> to <release>`. `platform`: blocked with `seed_not_upgraded` until the
seed's k3s is at `v1.36.6+k3s1`, then phrase `upgrade <name> to <release> and replace its machines`.
Only the move from 0.10.0 is drilled on a lab; the plans from the eight other BOMs were read
(`PlanFor`), not run.

**Before the platform part: databases made before database chart 0.5.2.** A database of one
instance on chart 0.5.1 or older has a PodDisruptionBudget that allows no eviction, and the plan's
part `platform` is then blocked with `drain_blocked`, which names it ("PodDisruptionBudget
<namespace>/<name>-primary allows no disruption ..."): Cluster API could never drain its machine. A
database keeps its chart when the provider's pin moves, so one made before 0.10.0 still has it
after the upgrades to 0.9.0 and 0.10.0 (seen on the production site, 2026-11-20). Read the platform
part's `blocking` before you start the seed part, and move each such database to 0.5.2 first with
the operator route ([managed-dbaas.md](managed-dbaas.md#chart-pin--platform-update)):

```text
# {id}: the Databases provider; {clusterId}: the database's Application name, as GET .../db-clusters lists it
POST /api/v1/admin/service/{id}/db-clusters/{clusterId}/bump-chart   # one database
POST /api/v1/admin/service/{id}/db-clusters/bump-chart               # every database of the provider
```

Then read the plan again. The bump changes only `enablePDB` to `false` for one instance: on
CloudNativePG 1.30.1 it removed the budget without restarting the database (the same pod, the same
postmaster start, no failed look), and `drain_blocked` left the plan at once. The database is still
down while its machine is replaced in the `dbaas` unit: take a backup of it first.

**What it does and what stops, step by step.** The sentences are the plan's; the sections
[The seed's k3s](#the-seeds-k3s) and [The platform cluster's machines](#the-platform-clusters-machines)
have the detail.

| Step | What the run does | What stops serving | How long |
|---|---|---|---|
| Part "seed", stage 2 | Imports the 1.36.6 node image as a template on every Proxmox node the cluster uses (a download of about 2.4 GiB), one node after another: the file is uploaded through the Proxmox API node, written to the import store, and imported into the node's boot store | Nothing by design. On a slow disk, or one the seed or the control plane shares with the import, the upload and the disk import can stall the etcd of the seed and of the platform cluster's control plane for seconds: the lab saw the seed's k3s restart once (39 s without its API), the platform API fail for up to 30 s, and single tenant API looks time out, all inside the minutes of an upload or import; the database port never failed. Choose a quiet hour | 11 to 19 minutes per node, 41 minutes for three nodes; the 19 include two uploads Proxmox answered with an error, which the import tried again by itself (the stage's item says which try runs) (one lab's sample, 2026-11-20). **No import when the node already has the BOM's node image as a published template** of Template updates: on the one-node site the stage took 0.08 seconds and wrote nothing on Proxmox |
| Part "seed", stage 5a | Saves an etcd snapshot on the seed, writes the k3s config drop-in that turns leader election off in three components, replaces the k3s binary, restarts k3s. k3s moves its bundled containerd from 2.2 to 2.3 with it | The seed's API. Reconciliation only: no machine is repaired, the seed's ArgoCD syncs nothing, no platform run goes on. The platform cluster, tenant clusters, tenant API servers and databases keep serving | The stage took 2 minutes 1 second and the seed's API was away for 43 seconds (one lab's sample, 2026-11-20) |
| Part "seed", stage 6 | The seed's charts are compared again, now on k3s 1.36 | Nothing | Not measured |
| Part "platform", step 1: control plane (one machine) | A second machine is cloned from the 1.36.6 template and joins; the old one is removed. etcd moves from 3.6.6 to 3.6.8 with the machine | The platform cluster's API, from the removal of the old etcd member until the new machine holds the API address: no tenant cluster or database operation, no repair, no failover, stale status in the consoles. Running tenant API servers, workloads and databases keep answering. **On a node whose disk the control plane shares with the clone, the API can be away during the clone too, before anything has joined** | Clone 6 minutes 5 seconds; the platform API away for up to 2 minutes 21 seconds during the clone on the control plane's own node (kube-vip let the address go, the API server restarted), then for 65 seconds at the hand-over, from the removal of the old etcd member until the new machine held the address (the kube-vip lease is 60 seconds); about 11 minutes for the whole unit. Tenant API: single looks failed, 13 seconds at most. Database: no failed look, no failed insert (one lab's sample, 2026-11-20). One-node site: clone 8 seconds, the platform API away 67 seconds at the hand-over and never during the clone, the unit 3 minutes 46 seconds |
| Step 2: `system` | The machine is replaced | The platform's operators (ArgoCD, Cluster API for tenants, CloudNativePG, MetalLB's controller, cert-manager, the CSI controller, CoreDNS) while they start on the new machine and pull their images; a MetalLB address may move | The operators were away for 3 to 5 minutes; the service addresses moved to another node in seconds, with no failed look at the platform API, a tenant API or the database. 8 minutes 50 seconds for the whole unit (one lab's sample, 2026-11-20) |
| Step 3: `kamaji` (one machine) | The machine is replaced; kamaji-etcd's disk follows | **Every tenant API server.** Tenant workloads keep running | **Tenant API servers away for 6 minutes 54 seconds.** During the unit's clone, on the node that also holds the control plane, single looks at the platform API and the tenant API failed for up to 10 seconds. Clone 6 minutes 7 seconds; 10 minutes 21 seconds for the whole unit (one lab's sample, 2026-11-20). One-node site: clone 7 seconds, the tenant API away 104 seconds |
| Step 4: `dbaas` | The machine is replaced; each database's disk follows | **Every database with one instance** on the machine | **The database away for 4 minutes 8 seconds**: a writer that inserted every 2 seconds failed 53 times and lost no row. 11 minutes 20 seconds for the whole unit (one lab's sample, 2026-11-20). One-node site: clone 6 seconds, the database away 75 seconds |
| Stages 8 to 11 | The platform cluster's charts are compared on 1.36, the providers are written, the record moves | Nothing | Not measured |

The figures are from two drills on one lab of three Proxmox nodes with one machine per unit
(`v0.10.1-rc.1` for the seed and the control plane, `v0.10.1-rc.2` for the three node groups), every
unit replaced with one more machine first. On the one-node site (2026-11-20, `v0.10.1`) the node
image was already a published template, each clone took 6 to 8 seconds (a full clone on the same
NVMe pool), the `system` unit had no gap, each unit took 2 minutes 44 seconds to 3 minutes 46
seconds and the whole part 13 minutes 14 seconds: the gaps were 67 seconds (platform API), 104
seconds (tenant API) and 75 seconds (database), and a clone stalled nothing. Another site's disks
and image pulls give other figures: announce more than these.

**Why the two stateful gaps are minutes, not seconds (known issue).** A new machine counts as Ready,
and Cluster API drains the old one, before the CSI node plugin of the new node has registered: the
plugin's pod is still pulling its image. The volume of kamaji-etcd, and of each database, is detached
from the old machine and then cannot be attached to the new one ("AttachVolume.Attach failed ...
CSINode ... does not contain driver", 8 or 9 times on the lab, 1.5 to 2.5 minutes of each gap); the
tenant API server meanwhile restarts with a growing back-off until kamaji-etcd answers. Nothing in
this release shortens it: the lever is `minReadySeconds` on the MachineDeployments, a change of the
cluster chart that needs a drill of its own.

**A clone is felt on the node that holds the control plane.** With one control plane machine, a
clone on its Proxmox node, sharing its disk, can stall the platform API: for up to 88 seconds
during the control plane's own clone on the lab (2 minutes 21 seconds together with a second stall),
for single looks of up to 10 seconds during the `kamaji` unit's clone on the same node, and for up
to 52 seconds on a tenant API during a Template updates smoke test there. Choose a quiet hour for
all three.

A unit on a node without room for one more machine is replaced delete-first: everything on it is
away for the whole clone and join (not measured). After step 1's new machine has joined etcd there
is no way back.

**The VM ids of the replaced machines are used again.** Proxmox gives a new machine the lowest free
id, so a new machine takes the id an old machine freed, possibly on another node and for another
unit (on the lab: 103 was the control plane on one node and became the `system` machine on another).
A backup job, a firewall rule or a note of yours that names a platform machine by its VM id points
at another machine after the roll: look at them before and after.

**The old machine templates stay on the seed.** After a roll the `ProxmoxMachineTemplate` objects of
the old version are still in namespace `pc-platform` on the seed, next to the new ones: the
Application `pc-platform` never prunes. Nothing refers to them and they hold no VM; they are
harmless and may stay. (The Proxmox templates of the old node image are another matter: Template
updates lists them, and they are yours to withdraw once no tenant cluster uses the old version.)

**The old machine's Node object.** Cluster API deletes a Machine's Node last, after the drain and
after the VM is gone, and gives up once the Machine has been in deletion for longer than
`nodeDeletionTimeoutSeconds`: 10 seconds by Cluster API's default, which the drain alone uses up.
With one control plane machine the delete falls into the gap in which the address moves, so on
cluster chart 0.1.15 the Node of the old control plane machine always stayed, NotReady, with no
Machine: five Nodes for four machines, the DaemonSets' rollouts held by a pod that could never be
ready, and a run that ended in `nodes_timeout` at every Retry. Two things correct it. Cluster chart
0.1.16 sets the timeout to 1800 seconds on the control plane and on every node group (Cluster API
puts it on the machines in place; no machine is replaced for it), so the delete is tried again
until the API answers. And the run, in the wait between two units and in stage 9, deletes a Node
that is not Ready and that no Machine owns: Cluster API joined it for a Machine of this cluster
(its annotations), that Machine is gone, and no Machine or ProxmoxMachine names the Node by
`status.nodeRef`, by its own name or by the provider id of its VM. The delete goes through the
platform cluster's API with the admin kubeconfig Cluster API keeps on the seed (no Proxmox call, no
new privilege) and only while the Node's uid is the one that was read. The stage's items name each
Node deleted (`node/<name>`, state `deleted`) and keep the item until the run ends, whatever the
stage writes afterwards, and the audit log has one entry per Node (action `DELETE` on the
installation, by the run, with the Node's name and the reason). The Check row `nodes` warns about
one before a run starts. **"Error deleting Node" during the hand-over is expected.** While the
platform API is away at the control plane's hand-over, the stage's item can read "<machine> (no VM):
old machine being removed: DeletingNode: Error deleting Node, please check controller logs for
errors": Cluster API's own condition, passed through. On the one-node site it showed for about 50
seconds and cleared by itself; once the API answered, Cluster API deleted the Node and the run had
none to delete. Do not delete that Node by hand. **A site that stands there after the control plane's unit** (the lab did,
on `v0.10.1-rc.1`: installation `failed`, `roll.step` 1, five Nodes) **gets out with one Retry** on
a release from `0.10.1-rc.2` on: the lab's Retry carried the unfinished roll to the new BOM, put
the timeout on the machines without replacing one, deleted the Node 41 seconds after the request
and stored step 2 after 62 seconds, with nothing to confirm and nothing done by hand. The
kube-vip lease stays at 60, 40 and 5 seconds: a shorter one would move the address sooner, but a
changed lease replaces every control plane machine (it is in the static pod), and a single control
plane loses its own address whenever its etcd is slow for longer than the renew deadline, which is
what the lab's disk did during the clone.

**Tenants keep their version.** No tenant cluster and no database is upgraded: a tenant cluster
stays on the Kubernetes version and the chart it has. Stage 10 adds 1.36.6 to the versions the
Kubernetes provider offers, next to the ones it has, **only once the 1.36.6 node image is published
on every node the provider uses**, and the run publishes nothing: the image it imported in stage 2
is `staged`, and a run's import is never smoke-tested by itself. Until then stage 10 ends with the
item `kubernetes-version` in state `warn` ("Kubernetes 1.36.6 is not offered to tenants: no node
image of it is published on ...") and a tenant's create at 1.36.6 is refused with `400` ("version
"1.36.6" is not offered by this provider"). To offer it: open **Template updates**, start the smoke
test of the new image on each node and publish it (the Ready card's button does all nodes; it needs
the provider's smoke network, and took 4.5 to 5.7 minutes per node on the lab), then **Apply this
release** once more, or wait for the provider's next sync: [What a tenant can use after the
install](#what-a-tenant-can-use-after-the-install) has the steps. Kamaji 26.11.2-edge takes tenant
versions up to 1.37.0 by its code; its document lists tenants up to 1.35 only. On the lab a tenant
cluster at 1.36.6 was created after the publish and was READY in 7 minutes 20 seconds.

**Two charts are outside their documented Kubernetes versions on 1.36.** The upgrade to 1.36.6
moves no third-party chart, so both stay at their pin:

- **cert-manager v1.20.2** (seed and platform cluster): its release table lists Kubernetes 1.32 to
  1.35. The first cert-manager that lists 1.36 is 1.21. This is new with the BOM of 0.10.1.
  **Closed by the BOM of 0.10.2**, which moves it to v1.21.3 (lists 1.33 to 1.36) in an upgrade
  of its own, after the one to 1.36.6 ([What this release moves](#what-this-release-moves-and-what-it-refuses)).
  A platform stays outside until it has made that move.
- **Cilium 1.18.16** (the platform cluster's CNI): its compatibility page lists 1.30 to 1.33, and
  says that other versions depend on Kubernetes' own compatibility. It was outside the list on
  1.35.10 already; 1.36 is one minor further. The first Cilium that lists 1.36 is 1.20.

No source says either breaks on 1.36. cert-manager v1.21.3 issued a new certificate and renewed
one on 1.36, on the seed and on the platform cluster, on the lab after its move (2026-11-21); v1.20.2
issued none there. Cilium 1.18.16 ran on every new node of the lab and of the one-node site, with
no fault seen. If one of them fails on your site, stop before the next part or unit: an upgrade to 1.36.6 cannot move either
chart (Cilium moves in no upgrade, cert-manager only in one that changes no k3s, Kubernetes version
or node image). Argo CD 3.6, Cluster API v1.14 (as management and as workload cluster) and
CloudNativePG 1.30 list 1.36. The Proxmox provider, the in-cluster IPAM provider, the Helm addon
provider, the Kamaji control plane provider, Kamaji, kamaji-etcd, MetalLB, proxmox-csi and kube-vip
state no Kubernetes version for their pin.

**Kubernetes 1.36 refuses a non-canonical address or CIDR in a new value.** From 1.36 the API
server validates addresses strictly by default (`StrictIPCIDRValidation`): an address with leading
zeros, or a CIDR with host bits set (`192.168.0.5/24` in place of `192.168.0.0/24`), is refused in
a built-in field. The product writes operator and tenant CIDRs into
`Service.spec.loadBalancerSourceRanges` on the platform cluster: a database's and a tenant
cluster's `allowedCidrs`, and the sources of the platform cluster's ArgoCD address. A value that is
already stored keeps working, on any later update of its object too (KEP-4858, ratcheting); only a
new or changed value, or an object that is created again, is refused, and its Application then
fails to sync. The product follows the same rule at its inputs: a database's or a tenant cluster's
`allowedCidrs`, a Kubernetes provider's `config.cluster.allowedCidrs` and
`spec.access.argocd.sources` refuse a new entry that is not canonical with the form to write
(`192.168.0.5/24 has host bits set: use 192.168.0.0/24`), and take an entry that is already stored
as it is, so a save that leaves the list alone is never refused. Stored values are not rewritten:
the Check row `argocd-access` warns about a stored source with host bits, a Kubernetes provider's
page names a stored default (a new tenant cluster gets it without the host bits), and a database
shows its list as stored. Before the platform part, read those lists for such values and correct
them through the product; it is a precaution, the roll itself breaks none. Not run against a 1.36
API server yet: the drill creates a database and a tenant cluster with an allow-list on the moved
platform.

**The installer path stays on 1.35.10.** `installer/playbooks/group_vars/all/versions.yml` keeps
k3s `v1.35.10+k3s1` and `services.k8s_version` 1.35.10: `make upgrade` would move a management
cluster of several servers, which runs Traefik, in place and without any of this upgrade's gates,
and nothing of that path was studied for 1.36. The kubectl in the clusterctl image therefore stays
1.35.10, one minor behind the 1.36 API servers it talks to, which is inside Kubernetes' skew rule
for kubectl and not for 1.37.

### The seed's k3s

An upgrade whose BOM names a newer k3s replaces it in place, in stage 5a, on the seed VM that
runs. The seed is not rebuilt and its disk is not touched.

**What is away.** The seed's API, while k3s restarts: 43 seconds on one lab on 2026-11-20 (one lab's
sample).
What stops meanwhile is reconciliation only: Cluster API replaces or repairs no machine, the
seed's ArgoCD syncs none of the platform's own components, and no platform run goes on. The
platform cluster, every tenant cluster, the tenant API servers and the databases keep serving:
they do not need the seed to run ([When the seed does not answer](#when-the-seed-does-not-answer)).
ArgoCD and Cluster API on the seed keep their state, which is in the seed's etcd on its disk; the
pods on the seed keep running through a k3s restart (k3s's own documentation). k3s rewrites the
add-ons it bundles (CoreDNS, metrics-server) at a version change; traefik, servicelb and
local-storage are disabled on the seed. From `v1.35.10+k3s1` to `v1.36.6+k3s1` the bundled CoreDNS,
metrics-server, etcd and Flannel are the same versions and containerd moves from 2.2.10 to 2.3.7
(k3s's release notes); whether the pods on the seed keep their restart counts across that containerd
step is not measured.

**What the stage does.** ProxCloud has no shell on the seed, so the stage makes one Job there,
`proxcloud-system/k3s-upgrade-<version>`, privileged and in the host's namespaces, with the BOM's
own clusterctl image (already on the seed; no other image or download source is added). The Job
runs these lines on the host and stops at the first one that fails:

```
k3s etcd-snapshot save --name pre-upgrade-<old version>
curl -fsSL --retry 5 -o /usr/local/bin/k3s.new <the BOM's k3s binary URL>
printf '%s  %s\n' <the BOM's sha256> /usr/local/bin/k3s.new | sha256sum -c -
chmod 0755 /usr/local/bin/k3s.new
mkdir -p /etc/rancher/k3s/config.yaml.d
printf '%s\n' '<the drop-in, one line>' > /etc/rancher/k3s/config.yaml.d/10-proxcloud-leader-elect.yaml.new
mv -f /etc/rancher/k3s/config.yaml.d/10-proxcloud-leader-elect.yaml.new /etc/rancher/k3s/config.yaml.d/10-proxcloud-leader-elect.yaml
cp -p /usr/local/bin/k3s /usr/local/bin/k3s.prev
mv -f /usr/local/bin/k3s.new /usr/local/bin/k3s
systemctl restart k3s
```

The download, checksum and mode lines are the ones of the seed's first boot. The install script is
**not** run again: the systemd unit and `/etc/rancher/k3s/config.yaml` stay as installed, which is
k3s's documented manual upgrade (the binary and a restart). A snapshot, download, checksum or
drop-in write that fails ends the Job before the binary is touched (`seed_k3s_failed`). The drop-in
is written after the checksum and before the binary: if a later line fails, the file stays, the
running k3s does not read it, and the next start of either binary takes it as intended. The stage is done when the
Job has ended, the seed's node reports the new version and is Ready; it then records the version,
and the snapshot the Job saved by its real name (k3s adds the node's name and a time to
`pre-upgrade-<old version>`): the path on the seed is `status.seed.preUpgradeSnapshot`, and the
finished stage carries it in its message and in the item `etcd-snapshot`, with the size and the
command that deletes it once the upgrade has held (k3s prunes no snapshot saved on demand). Then it
deletes the Job, whose log was the only other place with the name. Stage 6 follows unchanged and is the check that ArgoCD and Cluster API kept
their state: ArgoCD's repo server available, every Application Synced and Healthy, the clusterctl
inventory equal to the BOM.

**No leader election on the seed.** The drop-in
`/etc/rancher/k3s/config.yaml.d/10-proxcloud-leader-elect.yaml` holds one line:

```
{"kube-controller-manager-arg+":["leader-elect=false"],"kube-scheduler-arg+":["leader-elect=false"],"kube-cloud-controller-manager-arg+":["leader-elect=false"]}
```

A seed is one k3s server. With its embedded etcd, k3s still runs kube-controller-manager,
kube-scheduler and its cloud-controller-manager under a leader lease (it turns that off by itself
only for sqlite: `pkg/cluster/cluster.go` line 222 and `pkg/daemons/control/server.go` lines 135,
168 and 379 at `v1.36.6+k3s1`). A component that cannot renew its lease in time exits, and k3s with
it: that was seen on a lab seed during a backup and during a heavy apply, although no second server
exists that could take the lease. With `leader-elect=false` each of the three starts its
controllers at once and holds no lease, so a slow disk cannot end it that way. k3s reads
`config.yaml.d/*.yaml` after `config.yaml`, in alphabetical order, and a key that ends in `+` is
appended to the value before it (k3s's "Configuration File" document; `pkg/configfilearg/parser.go`
lines 226 to 292, the same in `v1.35.10+k3s1`), so an argument you set yourself in `config.yaml`
stays, and `config.yaml` is not rewritten. A new seed gets the same file at its first boot; an
existing seed gets it from the Job of the first upgrade that replaces its k3s, and it takes effect
with that restart. Limits: k3s's own controllers (`pkg/server/server.go`, `leader.RunOrDie`) keep
their lease and no argument turns it off, so this removes the exits that were seen, not every
possible one; a second k3s server must never join a seed that has this file; none of it has run on
a lab. To undo it by hand: delete the file and `systemctl restart k3s`.

**What stays on the seed.** The etcd snapshot under `/var/lib/rancher/k3s/server/db/snapshots`
(k3s prunes no snapshot saved on demand: delete old ones with `k3s etcd-snapshot delete`), the
drop-in and the previous binary as `/usr/local/bin/k3s.prev`. The plan lists them, and the Job,
before the run starts.

**No way back in the product.** Nothing restores the snapshot or puts the old binary back. If the
seed's API does not come back, the failure code `seed_k3s_timeout` and the plan's row `seed-k3s-binary` give the command that
does it by hand, over SSH with a key of `spec.seed.sshKeys`. Neither form has been tried on a lab.

- **A patch step** (the minor stays): `mv /usr/local/bin/k3s.prev /usr/local/bin/k3s && systemctl restart k3s`.
- **A minor step**, as `v1.35.10+k3s1` to `v1.36.6+k3s1`: the old binary alone is not enough. k3s's
  rollback document ("Rolling Back K3s", embedded etcd) says "If you cannot restore the database,
  you cannot roll back to a previous minor version", and gives this order: stop k3s and its pods
  with `k3s-killall.sh`, put the old binary back without starting it, restore the snapshot with
  `k3s server --cluster-reset --cluster-reset-restore-path=<snapshot>`, start k3s. The snapshot is
  the one the Job saved before it touched the binary:
  `/var/lib/rancher/k3s/server/db/snapshots/pre-upgrade-<old version>-<node>-<time>` (the plan's row
  `seed-etcd-snapshot` and the stage's first message name it; the time is the second of the save,
  which the run cannot read once the seed's API is away, so the command takes the newest file of
  that name). In one line, as the message gives it for `v1.35.10+k3s1`:

  ```
  S="$(ls -t /var/lib/rancher/k3s/server/db/snapshots/pre-upgrade-v1.35.10-k3s1-* | head -n 1)" && test -n "$S" && k3s-killall.sh && mv /usr/local/bin/k3s.prev /usr/local/bin/k3s && k3s server --cluster-reset --cluster-reset-restore-path="$S" && systemctl start k3s
  ```

  It stops before it changes anything when no such snapshot exists (`--cluster-reset` with an empty
  path would reset etcd without restoring it). `k3s server --cluster-reset` ends by itself with
  "Managed etcd cluster membership has been reset, restart without --cluster-reset flag now".
  `k3s-killall.sh` stops every pod on the seed; they start again with k3s. What was written on the
  seed after the snapshot is lost: that is the run's own progress since the Job began, and nothing
  of the platform cluster or a tenant. The restored etcd still holds the upgrade Job as it was while
  it ran; what k3s makes of it is not known (expected: its pod is found gone, the Job fails, the
  stage reports `seed_k3s_failed` and deletes it), so after the restore look at the run before
  **Retry**. The embedded etcd is 3.6.14 in both k3s versions, so the restore crosses no etcd
  version. The drop-in without leader election stays; k3s 1.35 reads it the same way.

If neither brings the seed back, the backup of the seed VM is the last net ([Credentials and the seed backup](#8-credentials-and-the-seed-backup)).

**The seed backup.** Such an upgrade starts only with a backup of the seed VM, and on most sites
you confirm it yourself: step 4 shows the reason `seed_backup_missing` and a box, "I checked in
Proxmox: seed VM … has a backup made after it was created and younger than 24 hours". Ticking it
lets the upgrade start, and the run and the audit log record that it was ticked. Before you tick
it, look in Proxmox (the seed VM's **Backup** tab, or the backup store's content) for an archive

- of **this** seed VM: its VMID, and, when the archive has notes, the seed VM's name in them
  (vzdump's default notes are the guest's name). Proxmox gives a freed VMID to the next VM and a
  store keeps the old archives, so a year-old archive of an earlier VM with the seed's VMID is not
  the seed's backup;
- made **after the seed VM was created** (the row names the time the seed became ready, when the
  record has it);
- **younger than 24 hours**.

The portal cannot do this for you on most stores. Proxmox lists a VM's backups only to a token that
holds `VM.Backup` on that VM or `Datastore.Allocate` on the store; to any other token it answers
with a list that leaves them out, not with an error. The portal token is kept off the seed VM on
purpose (the `NoAccess` row on the seed's pool, Check row `pool-fence`), and no token of the product
is given `VM.Backup` on it. So the check's row `seed-backup` (step 3) reads with the two tokens the
portal holds and sees the seed's archives where one of them manages the store: on the provider's
import store (the portal token) and on the platform's ISO store (`capmox@pve!capi`). An archive it
sees there, of this seed VM and younger than 24 hours, makes the row `ok` with its time and store,
and the upgrade then starts without the box. For every other backup store the row says that the
portal cannot see backups of the seed VM there: this is no sign that a backup is missing. The row
warns only while such an upgrade waits to be started, or when it could read every backup store of
the node and found no archive younger than 24 hours; it never fails, as a plain apply does not need
the backup. Neither of those two stores is where the seed's backup belongs
([Credentials and the seed backup](#8-credentials-and-the-seed-backup)), so expect to tick the box.

Seen on a lab (2026-11-20, v0.10.0-rc.1): with today's archive of the seed on the site's backup
store, the row of that build read "the stores read (…) hold none"; Proxmox had answered the portal
token with an empty list. The row described here is the correction; it has run in tests against a
fake Proxmox that filters the list as Proxmox does, not on a lab.

**Take the backup some minutes before you start.** On a busy node or a slow disk a snapshot backup
of the seed stalls the seed's disk for seconds; the seed's k3s then exits and systemd starts it
again, and the seed's API is away for about a minute (seen on the same lab, twice: during the
seed's own `vzdump`, and during an apply with no backup running). A run
that meets the restart retries its stage and goes on; that lab's run passed after two retries. The
same `vzdump` also left the etcd of another control plane VM on that node unhealthy until the
backup ended. So take the backup in a quiet hour, let it end, wait until the seed's API answers
again, and then start the upgrade: not the backup and the click in the same minute. On a one-node
site every VM shares the disk the backup reads.

### The platform cluster's machines

**Nothing in this section has run on a lab.** It says what the code and its tests do; every
duration is an expectation read from the manifests and marked "not measured" in the plan.

An upgrade whose BOM names another Kubernetes version, or another node image of the same version,
replaces every machine of the platform cluster. Cluster API replaces them (a new VM cloned from the
new image's template, the old one drained and deleted); the run orders it: the control plane first,
then one node group on one node at a time (`system`, `kamaji`, `dbaas`, then other groups by name;
inside a group its nodes by name). Nothing is upgraded in place.

**There is no way back.** Once the new control plane machine has joined, etcd has moved on and
kubeadm has no downgrade. The run restores nothing and has no rollback; Cancel, Retry and Remove do
not bring the old machines or the old version back. A step that fails leaves the machines that
still run serving, and Retry goes on at that step. If a database's volume does not follow its pod
to the new machine, the database stays down and the tenant's backups are the only net: take them
before you press the second button. Between "the new etcd member joined" and "the old member
removed" the two members need each other; a failure there is not repaired by the run.

**Two actions, in this order** (the plan's `parts`):

1. **Seed and node image** (`part: "seed"`). Stages 1 to 6 and no other: stage 2 imports the
   target's node image on every node the cluster uses, stage 5a moves the seed's k3s when the BOM
   names another, stage 6 moves the seed's own charts. No machine is replaced and no tenant is
   touched. The run ends succeeded with `status.targetBom` still set: the upgrade has started and
   the record names the BOM the platform left. Phrase: `upgrade <name> to <release>`, and the seed
   backup rule of [The seed's k3s](#the-seeds-k3s) when k3s moves.
2. **Platform cluster** (no `part`). The whole run, with the roll in stage 7. Phrase: `upgrade
   <name> to <release> and replace its machines`, asked of the run that begins the roll whether it
   starts the upgrade or continues it. Once the roll has begun every apply goes on without a phrase.

The second action is refused, in the plan, at `POST …/runs` (409) and again in stage 1 of the run,
until every pre-check passes: `node_image_missing` (a template of the target image on every node
used, which is how "the image is on the site" is known before any machine is touched),
`seed_not_upgraded`, `capacity_short`, `drain_blocked`, `platform_unhealthy`
([Failure codes](#6-failure-codes) has the fix of each). Tenants that are unhealthy before the
start do not block.

**One more machine first, or the old one deleted first.** A unit is replaced one machine at a time.
The control plane always gets one more machine first (a control plane of one cannot delete its only
etcd member first); a node without room for it refuses the upgrade (`capacity_short`). A node group
gets one more machine first where its node has room for one machine of that group; units go one at
a time, so one spare is needed at a time, never the sum the Check row `capacity` warns about. Where
the node has no room the unit is replaced **delete-first**: the old machine is deleted before the
new one exists. The plan says which, per unit; the run does what the plan it was confirmed with
said (`status.clusters[0].roll.deleteFirst`), and counts the node again before each step that needs
room: short now is `capacity_short` with nothing of the step applied.

**What stops, per unit** (the plan's `roll/<unit>` rows carry these sentences; the table in
[Upgrade to Kubernetes 1.36.6](#upgrade-to-kubernetes-1366) has what one lab measured):

| Unit | What stops while it is replaced |
|---|---|
| Control plane, one machine | A second machine is cloned and joins, then the old one is removed. From the removal of the old etcd member until the new machine holds the API address (the kube-vip lease, up to 60 seconds) the platform cluster's API does not answer: expected one to a few minutes. Meanwhile nothing on the platform can be created, changed, repaired or failed over: no tenant cluster or database operation, no restart of a failed tenant API server pod, no database failover, no ArgoCD sync, stale status in the consoles. Running tenant API servers, tenant workloads and databases are expected to keep answering: they are pods on the node groups, not on the control plane. Whether tenant API servers answer through the gap is for the lab to measure |
| Control plane, three machines | No API gap expected |
| `system` | The platform's operators restart; a MetalLB address may move to another node (seconds) |
| `kamaji`, one machine | **All tenant API servers are away** while their pods restart on the new machine, and kamaji-etcd moves with its disk. Tenant workloads keep running |
| `dbaas` | Every database with one instance on the machine is down while its pod moves and its disk is attached to the new machine; the plan counts them. Volumes are Proxmox disks on the node's store: the new machine is on the same Proxmox node, so the disk can follow |
| Any unit that is delete-first | Everything on the machine is away for the whole clone and join, and stays away if the new machine fails to join, until a Retry succeeds |

**What the run writes on the site** (rows `roll-vm/<unit>`, kind `site`): per machine one new VM
cloned on the unit's node from the target image's template, and the old VM deleted with its boot
disk. Volumes of proxmox-csi are separate disks and are kept. Only VMs that carry the
installation's tag (`pc-platform-<id>`) are deleted: no other VM on the node is touched.

**Stage 7 on such a run.** The order is made by the values the cluster chart is rendered with
(chart 0.1.15: a node group's `kubernetesVersions` and `deleteFirstOn`), not by pausing objects on
the cluster, which ArgoCD would put back. Step 1 renders the control plane at the target and every
group at the old version and template; each later step moves one MachineDeployment. Per step:

1. The stage records the step (`status.clusters[0].roll.step`) **before** it applies anything. The
   step is only ever raised, and every apply renders from it, so a Retry never puts a replaced unit
   back on the old version.
2. The rollout check reads ArgoCD's comparison of the step's render before anything of it syncs:
   at step 1 the KubeadmControlPlane and the control plane's machine template may differ, at a later
   step the templates of that step's MachineDeployment, and old templates to prune always. Anything
   else that is not Synced fails `unexpected_rollout` (a machine size edited together with the
   upgrade), with the step not synced.
3. The Application is applied and the stage waits until ArgoCD has synced exactly that render.
4. The unit is done when its owner (the KubeadmControlPlane or the MachineDeployment) has seen its
   last change and counts as many replicas, up-to-date, ready and available as it asks for, every
   Machine of the unit is at the target version, cloned from the target image's template and
   Running, and the unit has no Machine more. The
   stage's items name each machine with its VM, its unit and where it stands (not replaced yet,
   being cloned, cloned and joining, joined, old machine being removed with Cluster API's text and
   the pod that blocks a drain).
5. Then every node must be Ready with its group's label, and everything the tenants run on that
   was there before the first step must be there again (`tenants_unhealthy` while not), before the
   next unit's step is stored. The wait reads what the platform cluster's controller manager and
   kubelets write, which a unit does not take away together with the tenants: per tenant control
   plane the Deployment Kamaji runs it as (available replicas against replicas, its last change
   observed), per database its instance pods that are Ready against `spec.instances`, and every
   Deployment in the namespaces of the Kamaji and CloudNativePG operators. The health ArgoCD shows
   for a tenant's Application and the TenantControlPlane's own status are read as well, but are not
   enough alone: they are written by controllers the same drain evicts. On the drill of
   `v0.10.1-rc.2` they still said Healthy and Ready 76 seconds after the `kamaji` machine was
   drained, the next unit began, and its clone ran while the only tenant API server was away for
   another 5 minutes 38 seconds. What was down before the first step is noted once
   (`status.tenantsDownBefore`) and not waited for. The wait ends with the unit's 30 minutes; the
   message names what is still away. It is between two units, not between two machines of one
   MachineDeployment. This corrected wait has run on test doubles only, not on a lab.

During the control plane's step the platform cluster's API may not answer: that is waited for
(`workload_api_unreachable` on the waiting stage), not counted as an error. The stage fails with
`rollout_stalled` when no machine of the unit moved for 30 minutes. `status.clusters[0]
.kubernetesVersion` and `.templates` name the old version and templates until the last unit is
done.

**Pause and Resume.** Cancel is honoured between two looks and is called **Pause** on the page
while machines are replaced; a machine in flight is finished by Cluster API on its own, so a Pause
that is to stop before the next unit must be asked for before that unit's clone starts: once the
step is stored the run compares, syncs and clones within a minute. **Resume** (Retry) goes on at
the unit the record names and puts no replaced unit back. A paused cluster with the control plane
at the new version and node groups at the old one is a supported state (a kubelet may be up to
three minors older than the API server). There is no drain timeout: it would delete a machine
under a running database.

On the lab a Pause asked for right after the `system` unit ended the run as `cancelled` in 15
seconds, with nothing of the next unit applied and no clone started. The installation then reads
`failed` with `roll.step` 3 and the old `kubernetesVersion`, the plan allows the platform part
with nothing to confirm, and the Check is `ok`. Such a Pause falls into the wait for ArgoCD's
comparison of the next step, so the seed's Application `pc-platform` is left **OutOfSync and
without automated sync** until the Resume: while paused, ArgoCD neither applies the next step nor
puts back a hand edit of the cluster's objects. The mixed cluster served through the pause without
a failed look, and the Resume switched the automated sync back on at once and went on at step 3.

**Known gaps.** A MachineHealthCheck that replaces a machine of a unit not reached yet clones from
the old template; if Template updates has deleted that template since, the machine fails
(`machine_failed`). Keep the old node template until the upgrade has finished. A record edited by
hand on the seed can disagree with the stored step; the stage then waits or fails with a message
and does not guess.

### Before you start

- Before an upgrade that replaces machines: a dump or backup of every tenant database, the seed
  backup, and an announcement of what stops per unit (the plan's table).
- Take a backup of the seed VM (`vzdump`, [Credentials and the seed backup](#8-credentials-and-the-seed-backup))
  and a dump of the portal database. Take the seed backup some minutes before you start the upgrade
  and let it end first: on a busy node or a slow disk the backup can make the seed's k3s restart
  ([The seed's k3s](#the-seeds-k3s)); a run that meets the restart retries. The seed holds the Cluster API state of every machine of the
  platform cluster. Restoring both after a failed upgrade has not been tried. An upgrade that
  replaces the seed's k3s asks for the seed backup before it starts ([The seed's k3s](#the-seeds-k3s)).
- Do not change machine sizes, nodes or node groups in the same step: apply such a change before
  the upgrade or after it. Together with the upgrade it is refused in stage 7 (`unexpected_rollout`).
- Every tenant cluster and database keeps running. Tenants that are unhealthy before the upgrade
  do not stop it.

### Starting it

An upgrade that replaces the platform cluster's machines shows the card **Kubernetes upgrade to
release X** instead, with two actions and a button each
([The platform cluster's machines](#the-platform-clusters-machines)); by API the first is the same
`POST …/runs` with `"part":"seed"`, the second one without `part` and with the longer phrase.

Step 4 shows **Upgrade to release X** with the table of changes when the upgrade is allowed, and
the reasons when it is not. The button asks for the phrase `upgrade <installation name> to
<release>`. By API: `POST …/runs {"kind":"apply","targetBom":"<sha256 of this release's BOM>",
"confirm":"upgrade <name> to <release>"}` ([api.md](api.md)), with `"seedBackupTaken":true` for
the ticked seed backup box (what to check before ticking it: [The seed's k3s](#the-seeds-k3s)).

### What the run does

The stages of an apply, rendered from the target BOM:

- **Stages 1 to 5** find the images and the seed as they are when both BOMs name the same images.
  When the target names another node image, stage 2 imports it on every node the cluster uses.
- **Stage 5a** replaces the seed's k3s when the target BOM names a newer one
  ([The seed's k3s](#the-seeds-k3s)); otherwise it only records the version the seed runs.
- **Stage 6** applies the seed's ArgoCD HelmChart again (the same chart version, the new release
  label), then the seed's waves. `capi` at its new version passes when ArgoCD reports it Synced and
  Healthy against the new version, with the sync it started for it (if the render differs)
  `Succeeded`, and the clusterctl inventory equals the BOM. The stage shows where it stands as
  `step 2 of 2, wave 2`: the step counts the waves the stage has, the wave is the BOM's number,
  the one the plan shows.
- **Stage 7** makes the rollout check before it lets the cluster chart sync. The names of the
  machine and bootstrap templates carry a hash of their spec, so a chart that renders another
  template makes a new one and Cluster API replaces every machine that used the old one; a changed
  KubeadmControlPlane replaces the control plane machine. The stage therefore applies the
  Application `pc-platform` with the new chart version and **without** automated sync, waits until
  ArgoCD has compared the cluster with it, and reads the comparison (`status.resources`): a
  `ProxmoxMachineTemplate` or `KubeadmConfigTemplate` that is not Synced (and is not one ArgoCD
  would only prune, an old template), or a `KubeadmControlPlane` that is not Synced, fails the run
  with `unexpected_rollout`. Nothing was synced then. Otherwise the stage applies the Application
  as always, automated, waits until ArgoCD reports it Synced and Healthy against the new version or
  its last sync is of the new version, and goes on as in any apply. A new chart that renders every
  object as it is leaves the Application Synced with no sync at all (seen on the lab on 2026-11-19).
  There is also no sync to wait for when the new chart renders every object as it is and the Application
  is out of sync only for what it would prune (the machine templates of an earlier size: it never
  prunes them): ArgoCD starts no sync for that, so the stage passes when the comparison is of the
  new version and nothing but such objects is out of sync. That ArgoCD starts no sync then is read
  from its source, not seen on a lab.
  On a run that replaces machines the stage takes the roll instead, one unit per step, with this
  check per step ([The platform cluster's machines](#the-platform-clusters-machines)).
- **Stage 8** is unchanged.
- **Stage 9** notes which tenants are unhealthy before its first wave (once per upgrade, in
  `status.tenantsDownBefore`; a Retry keeps the note; a run that replaces machines takes it in
  stage 7, before its first step), then applies the waves. After
  each wave it waits until every component of the wave is Synced and Healthy against the new
  version, and until everything the tenants run on that was there at the start is there again
  (`tenants_unhealthy` while not): every tenant control plane's Deployment with its replicas
  available, every database with its instance pods Ready, the Deployments of the Kamaji and
  CloudNativePG operators available, the tenant Applications on the platform cluster's ArgoCD
  Healthy and every TenantControlPlane Ready. It is the same wait as between two units of a roll
  ([The platform cluster's machines](#the-platform-clusters-machines), point 5).
- **Stage 10** writes the tenant charts of the target BOM on the two providers.
- **Stage 11** records every component and sets `status.bom` to the target.

### What the record says meanwhile

| Field | During the upgrade | After the last stage |
|---|---|---|
| `status.bom` | The BOM the platform left: "last fully applied" | The target |
| `status.targetBom` | The target, from the start of the first run | Absent |
| `status.tenantsDownBefore` | From the first time a run reaches stage 9, or stage 7 on a run that replaces machines: what the tenants run on that was not there then, one text per object (`Deployment <namespace>/<name> (0 of 1 replicas available)`, `Database <namespace>/<name> (0 of 1 instances Ready)`, `Application <name> (Degraded)`, `TenantControlPlane <namespace>/<name> (NotReady)`, `Operator <namespace>/<name> (...)`) | Absent |
| `status.clusters[0].roll` | On an upgrade that replaces machines, from the moment stage 7 begins the roll: `step` (the unit being replaced, counted from 1, the control plane; one more than the number of units when all are done) and `deleteFirst` | Absent |
| `status.clusters[0].kubernetesVersion`, `.templates` | The old version and templates until the last unit is done | The target's |
| `status.components[]` | Each component at the version it runs: it moves when its wave (stage 7 for `pc-platform`) has passed | Every component at the target's version |
| The run's `bom`, `fromBom` | The target, and the BOM the platform left | The same |

The page shows it in the run card ("This run upgrades the platform from release A to release B")
and, after a run that did not succeed, in step 4 ("The upgrade to release B has not finished").

### Failure, Retry, Cancel and Remove

- A failed or cancelled upgrade run keeps `status.targetBom`. Every later apply, **Retry** and
  **Apply this release** alike, continues to the target without a new confirmation. No apply goes
  back to the BOM the platform left: an apply that names it as `targetBom` is `409 upgrade_refused`.
  Before this release a Retry would have run with the installed BOM again and written the old
  chart versions over the components already moved.
- Nothing is rolled back, by the run or by ArgoCD. The components whose wave passed stay at the new
  version, the others at the old one, and `status.components` says which.
- A tenant that a wave broke holds the upgrade in every run, a Retry included, until it is healthy
  again. A tenant that breaks for a reason of its own during the upgrade holds it in the same way:
  the run cannot tell the two apart. Repair it, or delete it; nothing in this release lets an
  upgrade pass an unhealthy tenant that was healthy at its start.
- A Cancel while stage 7 waits for ArgoCD's comparison leaves the Application `pc-platform` at the
  new chart version without automated sync and self-heal, as `unexpected_rollout` does. The next
  apply makes the check again and switches them back on.
- **Remove** works as always, and forgets the upgrade with the rest of the status.

**When the upgrade cannot finish.** `unexpected_rollout` without a change of the settings means the
release shipped a cluster chart that replaces machines, which its CI check should have refused. The
platform cluster runs as before, with the seed's `capi` chart already at the new version. Install
the ProxCloud release that corrects the chart and start the upgrade to its BOM: an upgrade to this
release's BOM is accepted over an unfinished target when both the installed BOM and the unfinished
target may move to it (each component stands at one of the two). Step 4 then shows two boxes: "The
upgrade to release X has not finished", and **Upgrade to release Z instead** with its own typed
phrase (the plan's `replacement`; by API the same `POST …/runs` with `targetBom` and `confirm` as
for any upgrade). **Apply this release** and **Retry** still continue to release X and fail at the
same check; only the second box leaves it. Until the corrected release is installed the platform
cannot be applied.

**A later release while the machines are being replaced.** Once the replacement has begun
(`status.clusters[0].roll`) and a unit is still to be replaced, a ProxCloud release whose BOM names
the same Kubernetes version, node image, kube-vip and k3s as the unfinished target, and differs
only in the product's own charts and the release, takes the target's place by itself: **Retry**
(an apply without `targetBom`, or one that names the new BOM) goes on under the new BOM with
nothing to confirm, `status.targetBom` becomes the new BOM, and the plan shows one upgrade in
progress whose `upgrade.carried` names the target it replaced. The step is kept. The units already
replaced render as they are, the units not reached render with the new cluster chart, and nothing
is rendered back. This is the way out for a site whose replacement stands at a defect of the
release that started it: install the corrected release, press Retry. It needs what any target
needs: the new chart lists both the installed version and the unfinished target's in `upgradeFrom`.
A later release that names another Kubernetes version, node image or k3s again is not taken while
the replacement runs: Retry goes on to the unfinished target (the newer ProxCloud embeds its BOM),
and an apply that names the new BOM is `409 upgrade_refused` with "the replacement of the platform
cluster's machines for release X has begun (step N) and release Z changes … again"; finish first,
then start the next upgrade. Before the replacement has begun, and after its last unit, nothing is
carried and the two boxes above apply.

The check reads what ArgoCD reports and has not run against a real ArgoCD. If on a site it fails
for a cluster chart that provably changes no template (compare `kubectl -n pc-platform get
proxmoxmachinetemplates,kubeadmconfigtemplates` with `helm template` of the new chart), the API's
environment variable `PROXCLOUD_PLATFORM_SKIP_ROLLOUT_GATE=true` (`PLATFORM_SKIP_ROLLOUT_GATE` in
the Compose `.env`) turns it off. With it off a chart that does change a template replaces every
machine of the platform cluster at once, control plane and all node groups together. On a run that
replaces machines the switch turns off the check of every step too: the order of the units is
kept (it is in the rendered values), but a machine size edited together with the upgrade is then
rolled with its step and not refused.

### What the upgrade still owes a lab

Lab only. One lab upgrade (2026-11-19, 0.8.0 to 0.9.0-rc.1, no tenants) ran U2 without its tenant
probes and the passing half of U3; U1, U4, U5, U6 and the refusing half of U3 have not run. Each
drill build must carry the BOM of the build before it
([ci-release.md](ci-release.md#upgrade-drill-builds)).

| Test | What it would show |
|---|---|
| U1 Record-only move: install from a release (v0.8.0, v0.8.1 or v0.8.2) with one tenant cluster and one database under a write loop; upgrade to a drill build whose BOM differs in `release` alone | Every stage done in seconds, no Application revision changed, `status.bom` moved; whether k3s's helm controller runs a Job for the seed ArgoCD when only the HelmChart's label changes |
| U2 Own-chart move: a drill build that bumps `proxcloud-platform` (and, when one exists that changes no template, `proxmox-kamaji-cluster`), with a probe of the tenant API and the database address once a second | The gaps, if any; that the clusterctl hook changes nothing; that `status.components` moves per wave |
| U3 The rollout check against the real ArgoCD: a drill build whose cluster chart changes a machine template value | That ArgoCD lists the new template as not Synced before any sync, that the run fails `unexpected_rollout` with no machine replaced, and that an unchanged chart leaves the KubeadmControlPlane Synced. On a platform that once changed a machine size (old templates left, the Application OutOfSync for them alone), a drill build whose cluster chart version is new and renders identically: that ArgoCD starts no sync and stage 7 passes all the same. Also that the result of the last sync carries its source (`status.operationState.syncResult.source`), which stage 7 waits on when the Application stays OutOfSync |
| U4 Failure and Retry: a drill build with a wrong chart digest for `platform`, so workload wave 3 fails; then the corrected build | No Application goes back to the old revision; with the corrected build step 4 shows **Upgrade to release Z instead**, and the later build's BOM is accepted over the unfinished target |
| U5 The tenant wait: stop a tenant control plane's pod during stage 9 | The run waits with `tenants_unhealthy` and goes on when it is Ready. Then let the wait run out (the run fails) and Retry with the tenant still down: the new run waits again |
| U6 Restore: after U4's failure, restore the seed's `vzdump` and the portal database dump | Whether that returns the platform to the BOM it left |
| U7 The seed's k3s: a drill build whose BOM differs in `seed.k3s` alone, on a platform with one tenant cluster and one database under a write loop, the tenant API and the database probed once a second | That the run ends succeeded; the gap of the seed's API in seconds; no gap on the tenant API or the database; the node at the target version; the etcd snapshot file on the seed; the drop-in `10-proxcloud-leader-elect.yaml` on the seed and `--leader-elect=false` in k3s's log lines "Running kube-controller-manager", "Running kube-scheduler" and "Running cloud-controller-manager"; no exit of k3s during the rest of the run; every Application of the seed Synced and Healthy and the clusterctl inventory unchanged afterwards; no Machine replaced. Also whether the Job's end is recorded once the kubelet is back (it restarts the k3s that runs it), and that the clusterctl image's `nsenter` enters the host as the Job asks |
| U8 The seed's k3s, failing: a throwaway build with a wrong sha256 for the binary, then the corrected build | `seed_k3s_failed` with k3s untouched (`k3s --version` on the seed, no `k3s.prev`), and Retry with the corrected build passes |
| U10 The roll, homelab shape (one node, one control plane machine): drill builds whose BOM names the next Kubernetes patch with a node image built for it; a tenant cluster serving a pod behind a LoadBalancer and a database under a write loop; probes once a second on the platform API, the tenant API and the database | That **Seed and node image** imports the template and that the platform part is blocked before it and allowed after; that the roll completes; the measured gap per unit (platform API; tenant API during the control plane's step and during the kamaji step; database during the dbaas step), to replace "not measured" in the plan and in this guide; every volume attached to the new machine; the owner's other VMs untouched (`qm list` before and after) |
| U11 Pause after the control plane, Resume an hour later | That the mixed-version cluster serves and the run goes on at the next unit |
| U12 The roll's refusals: a PodDisruptionBudget with no allowed disruption (before the start, and added mid-roll); memory too short for the control plane's one more machine, and for a group's | `drain_blocked` in the plan; `rollout_stalled` with the pod named; `capacity_short` with nothing changed; a delete-first unit with its gap |
| U13 A surge machine that fails to join, then Retry | `machine_failed` with the old machine still serving, and Retry continues at the same unit |
| U14 A machine size edited together with the upgrade; a chart-only upgrade | `unexpected_rollout` at the step, nothing of it synced; the chart-only upgrade still replaces nothing |
| U15 Three control plane machines | No API gap; that the kube-vip hand-over needs no change of the lease values |
| U16 A minor step: the BOM with k3s `v1.36.6+k3s1` and Kubernetes 1.36.6 is the first that carries one, so the drill of its candidate from the BOM of 0.10.0 is U7, U10 and U16 in one ([Upgrade to Kubernetes 1.36.6](#upgrade-to-kubernetes-1366)) | Whether the seed's k3s and the roll hold across a minor; every gap of that section's table, to replace "not measured"; the seed's pods across the containerd step; cert-manager v1.20.2 and Cilium 1.18.16 on 1.36; the Proxmox, IPAM, addon and Kamaji providers reconciling on 1.36; a new tenant cluster at 1.35.10 and one at 1.36.6, and a new database, on the 1.36 platform; a CIDR with host bits in `allowedCidrs` refused by the API server; the version skew during a pause; the way back of stage 5a with the snapshot restore, tried once on purpose with the command of the message (on a clone of the seed, or after the round): that the seed answers again on 1.35.10, what the upgrade Job's object does after the restore, and that **Retry** then moves the seed again |
| U9 The seed backup: start U7 without ticking the box, then with it; a backup on the import store or on the platform's ISO store | The `409 seed_backup_missing`, its sentence and the box in step 4; the `seed-backup` row saying "cannot see" for an archive on the site's own backup store (a directory store, a Proxmox Backup Server) and turning `ok` for one on the import or the ISO store; an old archive of an earlier VM with the seed's VMID not counting |

## Known issue: no PostgreSQL database on a node image built before 2026-11-16

The node image an earlier BOM named (`ubuntu-2604-k8s-1.35.10`, built 2026-11-14) runs Ubuntu 26.04 with kernel
7.0.0-43. Ubuntu 26.04 sets `kernel.apparmor_restrict_unprivileged_unconfined=1`, and since kernel
7.0.0-42 a container's processes then run under a stacked AppArmor label
(`cri-containerd.apparmor.d//&unconfined`) to which containerd's own profile allows no signal: the
processes of one container cannot signal each other ([Launchpad 2172640](https://bugs.launchpad.net/ubuntu/+source/linux/+bug/2172640)
for the symptom). The kernel change is intentional (it closes CVE-2026-90231), so no kernel that undoes it is
expected: the fix comes from containerd
([issue 14485](https://github.com/containerd/containerd/issues/14485),
[pull request 14487](https://github.com/containerd/containerd/pull/14487)).
PostgreSQL wakes its processes with signals, so a new database stops at "Configuring new PostgreSQL
instance" and stays `PENDING` (seen on a lab: 12 denied signals of `postgres` in the `dbaas` node's
`/var/log/audit/audit.log`). The controllers of the platform are Go programs and run.

- **The fix in the image.** A node image built from this release on sets that sysctl to `0`, the
  value of Ubuntu 24.04, in `/etc/sysctl.d/90-proxcloud-apparmor-stack.conf`; containers stay
  confined by containerd's profile. The export's cold-boot check refuses a Kubernetes image without
  it. **The BOM of v0.8.0 names an image with the fix**
  (`ubuntu-2604-k8s-1.35.10-20261116181221-72f0eaa3d0d4`; the file was read from the published
  disk). A database that comes up on it is not recorded from a lab. The setting
  and the cold-boot check go away again when the image carries a containerd release with the
  stacked-label rules: pull request 14487 was merged to containerd's main branch on 2026-11-16 and is
  in no release yet; the backport for 2.3 is pull request 14590, still open.
- **On a platform that runs now**, on every platform machine, as root: write the same file
  (`kernel.apparmor_restrict_unprivileged_unconfined = 0`), run `sysctl --system`, then restart the
  pods or reboot the machine: a running container keeps its stacked label. A machine that Cluster
  API replaces comes from the template again and needs it again. This is the workaround
  named in containerd pull request 14487 and its discussion (set the sysctl to 0, recreate the
  containers); nobody in the Launchpad report tested it, and it has not been run on our lab.
- **Not changed:** the seed image (the same kernel and k3s; no denial was measured there and its
  workloads are Go programs) and the tenant OS image `ubuntu-2604`, which keeps Ubuntu's default: a
  tenant who runs Docker or containerd on a 26.04 server meets the same bug and may set the same
  sysctl. Ubuntu 24.04 images are not affected.

## Not implemented yet

- **No release before v0.8.0.** The BOM of v0.8.0 is `0.8.0`, that of v0.8.1 is `0.8.1`, that of v0.8.2 is `0.8.2`, that of v0.9.0 is `0.9.0`, that of v0.9.1 is `0.9.1`, that of v0.10.0 is `0.10.0`, that of v0.10.1 is `0.10.1`, that of v0.10.2 is `0.10.2`, that of v0.11.0 is `0.11.0`, that of v0.11.1 is `0.11.1` (`main` carries a `-dev` one between releases; the candidates `v0.9.0-rc.1` and `v0.9.0-rc.2` carry `0.9.0-rc.1` and `0.9.0-rc.2`, release BOMs for a lab, and the candidates `v0.10.0-rc.1`, `v0.10.1-rc.1`, `v0.10.1-rc.2`, `v0.10.2-rc.1`, `v0.11.0-rc.1`, `v0.11.0-rc.2`, `v0.11.1-rc.1`, `v0.11.1-rc.2`, `v0.11.1-rc.3` and `v0.11.1-rc.4` carry `0.10.0-rc.1`, `0.10.1-rc.1`, `0.10.1-rc.2`, `0.10.2-rc.1`, `0.11.0-rc.1`, `0.11.0-rc.2`, `0.11.1-rc.1`, `0.11.1-rc.2`, `0.11.1-rc.3` and `0.11.1-rc.4`; v0.9.0 keeps both as previous BOMs, v0.9.1 keeps those and that of v0.9.0, v0.10.0 keeps that of v0.9.1 and that of the candidate `v0.10.0-rc.1` too, v0.10.1 keeps that of v0.10.0 and those of the candidates `v0.10.1-rc.1` and `v0.10.1-rc.2` as well, v0.10.2 keeps that of v0.10.1 and that of the candidate `v0.10.2-rc.1` too, v0.11.0 keeps that of v0.10.2 and those of the candidates `v0.11.0-rc.1` and `v0.11.0-rc.2` as well, and v0.11.1 keeps that of v0.11.0 and those of the candidates `v0.11.1-rc.1`, `v0.11.1-rc.2`, `v0.11.1-rc.3` and `v0.11.1-rc.4` too). `--managed-services` needs v0.8.0 or later: no
  earlier released image reads `PROXCLOUD_PLATFORM_ENABLED`, and the images of v0.8.0 exist once
  its tag's workflows have succeeded. For a test ahead of a release,
  `proxcloud-up.sh` takes a build of main as its version (`--version dev-<sha>`, `upgrade dev-<sha>`;
  CI publishes one per push to main) and prints that it is not for production
  ([deploy/compose/README.md](../deploy/compose/README.md)).
- **The third-party charts come from our mirror, and ArgoCD pulls them by version.** Every chart of the
  BOM has an exact version and a digest; the nine third-party ones are served by
  `ghcr.io/mutex-solutions/charts-mirror` ([ci-release.md](ci-release.md), "Chart mirror"), `kamaji`
  and `kamaji-crds` as version `26.11.2-edge`, packaged from the upstream tag of that name. CI
  compares the digests; ArgoCD does not, so a version replaced in the mirror between two checks
  would be installed.
- **An upgrade between BOMs moves the product's own charts only**
  ([Upgrade between BOMs](#upgrade-between-boms)). Until an operator starts one, an installed
  platform stays on the BOM that installed it. A release
  carries its own BOM and, from the release after v0.8.0 on, the BOM of each earlier release back
  to `0.8.0` (`deploy/platform/previous/`, byte for byte the file of that release's tag). After a
  ProxCloud upgrade the platform a release installed keeps working: the reads and the Check, an
  apply (**Apply this release**) and Remove. The apply runs with the installed BOM and takes every
  pin from it, the images, k3s, the charts and their digests, the Cluster API providers and the
  charts the two providers name: it repairs, applies changed settings and renews the seed ArgoCD's
  token, and it moves no component to the version of the newer release. Step 4 lists the contents
  of the installed release and the plan of the move to the newer one, allowed or refused.
  A BOM that changes the Kubernetes version, an image, k3s, a Cluster API provider or a third-party
  chart is refused: those moves are a later release. Provider (Cluster API) upgrades are refused as
  `capi_init_failed` where a run meets one.
  - **Seen on a lab once** (2026-11-18): a platform installed by the released v0.8.0, then the
    portal upgraded to the build `dev-751a190` of main. The plan had no change and named that
    build's BOM as `newer`, the Check passed, **Apply this release** succeeded in 6.6 seconds,
    `status.bom` stayed the 0.8.0 one and no Application of the seed was synced again. That
    build's own BOM pinned the same versions as 0.8.0, so the lab does not show an apply that
    keeps older pins while the build carries other ones, and the build was not the release
    v0.8.1 itself. The same platform was read the same day on the build `dev-03bc9f3` of main,
    which carries the BOMs of 0.8.0 and 0.8.1 as previous ones: the plan had no change, named
    that build's own BOM as `newer` (not 0.8.1), `status.bom` stayed the 0.8.0 one and both
    previous BOMs were answered by their hash; no apply was run there. What tests show: the file under `previous/` has the sha256 that
    a v0.8.0 platform records (`8eb87498…6600c6`), what the renderers of this build make of the
    0.8.0 BOM is byte for byte what v0.8.0 rendered from it (the goldens of the tag), and a run on
    fakes takes its images, Applications and provider bodies from the 0.8.0 BOM.
  - The pins come from the installed BOM; the code that renders them is the running release's. A
    release that changes a renderer changes what the next apply writes to an installed platform.
    `TestRenderGoldenOfPreviousBOMs` shows such a change as a diff in review.
  - A provider that the apply registers again gets the tenant chart versions of the installed
    BOM, not those of the newer release.
  Two consequences to know before an install is finished on a build that is not a release:
  - The installed BOM is recorded by the sha256 of the BOM file as that build carries it, in
    stage 11 only. Any later change of the file, a digest filled in for an entry that said
    `pending` or a comment, is another BOM. Only the BOMs of releases are kept as previous ones. A
    platform installed on a `-dev` build (a `dev-<sha>` build of `main`, `0.8.0-dev`,
    `0.9.0-dev`, `0.10.0-dev`, `0.11.0-dev` or `0.12.0-dev`) is therefore refused (`bom_unsupported`) by every later build whose BOM file
    differs, a release included: keep the build that installed it, or Remove and install again.
    Finish an install you want to keep only on a release.
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
  `proxmox-kamaji-cluster` 0.1.16, for the platform cluster (which, with one control plane machine
  and no VLAN tag, it renders as 0.1.12 to 0.1.15 do, but for the node deletion timeout) and
  for new tenant clusters (whose node group replicas it places by free capacity,
  [managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox)). A platform cluster built from the BOM that named 0.1.11 has a control
  plane node that registers the API address as its InternalIP (its kubelet has no `node-ip`;
  nothing failed on it with one control plane machine) and a kube-vip that gives up the API address
  when a lease renewal takes over 10 seconds. Chart 0.1.12 gives the kubelet the machine's own
  address and kube-vip a renew deadline of 40 seconds (several tries of at most 10 seconds each,
  see [What no check reads](#what-no-check-reads)). Those BOMs were of builds of main, which no
  release carries: an apply on the earlier platform is refused (`bom_unsupported`), and it gets the
  fixes by Remove and a new install. The same holds for a platform built from the BOM that named 0.1.12 and
  `proxcloud-platform` 0.1.1: its Kubernetes provider keeps `chartVersion` 0.1.12 (no placement by
  free capacity) until the operator sets it to 0.1.13 on the provider's page, and its provider
  service account cannot read Machines (no provisioning reason) until a Remove and a new install:
  the `platform` Application keeps the ClusterRole of chart 0.1.1 (self-heal), so re-applying
  `deploy/mgmt-cluster/rbac.yaml` there does not last.
- **`layout: separate`** (one cluster for Kubernetes, one for databases) is refused by the API.
- **`spec.sandbox`** is refused by the API of every release so far: no BOM names the sandbox chart
  yet. The code that builds either placement and registers its provider is in this release
  ([The sandbox provider](#the-sandbox-provider)); the release whose BOM pins the chart offers it.
- **No IPv6** on the platform network, and one platform network per installation. A VLAN tag needs
  a BOM that names cluster chart 0.1.14 or newer, as this build's does, and the network is fixed after the
  first install: a platform installed without a tag never gets one
  ([the three forms](#the-platform-network-bridge-vlan-tag-or-routed)).
- **No automated node leg** for an existing VLAN site: the commands are in
  [The path from tenants to the platform](#the-path-from-tenants-to-the-platform), by hand.
- **Control plane machines on one node only.** The count is 1 or 3, and all three are on one
  Proxmox node and one store. Machines on several nodes (`controlPlane.nodes`) are refused, and so is
  going from 3 back to 1 once a run has built three. Three need cluster chart 0.1.14 or newer, which this
  build's BOM names. On an installation of an earlier BOM (0.8.x) the save, the Check and the run
  refuse 3 until the platform is upgraded to this release's BOM.
- **kube-vip's lease values are those of one machine** (60, 40 and 5 seconds) also with three.
  They are chart values (`controlPlane.kubeadm.vipLease`); what suits three members on one store
  was not measured.
- **No sandbox node group**, and no sandbox cluster, on a platform of a released BOM (see
  `spec.sandbox` above). The sandbox machines have a range of their own (`spec.sandbox.nodeRange`),
  because they must not reach the Proxmox API: the range is left out of the API rule's sources and
  fenced as a sandbox network.
- **One installation on the page.** The API keeps one per Proxmox provider; the page shows the first.
- **No route deletes a draft or an installation record.**
- **No block volumes for tenant clusters and no database backups** on Compose
  ([section 9](#9-limits-on-the-compose-install)).
- **No per-namespace RBAC for the providers' service account** on the shared cluster.

## Not verified on a lab yet

**Sandboxes on the platform (`spec.sandbox`)** have run on no site: no BOM names the sandbox chart,
so no installation can save the block. What the code does was run against fakes and against the
two charts' renders only. A lab must still show, for each placement: the portal token making pool
`proxcloud-sbx` and writing the ACL row or minting the token (Proxmox accepting `Pool.Allocate`
for a `ProxCloudCAPI` row there is run for tenant pools, not for this one); CAPMOX giving a
sandbox machine its address from `nodeRange` and none from the cluster's range; the group built
on an installed platform with no other machine replaced; the sandbox chart Healthy with the
run's certificate; `pc-sandbox` built on the kamaji group and the memory its control plane takes
there; `off` taking all of it away, and Remove in the order it promises; a roll with a `shared`
group as its last unit; the mint on a platform network that is an SDN vnet.

**The sandbox provider and the fence on the platform network** were run against fakes only: a fake
Proxmox for the VM firewall (it stores rules and does not filter a packet), fake clusters for the
gates and the registration, and a model of the shared identity's RBAC for the portal's calls. A lab
must still show, for each placement:

1. After stage `cluster`: the provider row holds `config.sandbox` and the one `sandboxCidrs` entry,
   a provider sync writes the isolation rules for the range, and a tenant network create still
   works.
2. After stage `register`: the provider "… Sandboxes" exists DISABLED, a provider sync fences each
   sandbox VM (both policies DROP, the generation's group as its only rule, the IP filter pinned to
   its address) and takes the `unsecured` taint off after 20 minutes, and **no VM outside pool
   `proxcloud-sbx` changed** (compare `pvesh get /nodes/<n>/qemu/<id>/firewall/options` of a
   platform machine before and after).
3. `shared`, from a sandbox machine with its firewall in force: the Cilium agent stays Ready across
   a restart (the API through the `kubernetes` Service, which is TCP 6443 to a control plane
   machine), a pod on it reaches a pod on a `system` machine and back, `kubectl logs` and `exec`
   work (TCP 10250 from the control plane), image pulls and the runsc download work, and from the
   machine itself the Proxmox API, the seed, a MetalLB address other than through the pod network,
   and a management address do not answer.
4. `separate`: the same from a machine of `pc-sandbox`, plus a client outside the range reaching
   `https://<port>-<id>.<domain>` at the proxy address, and `sandbox-proxy` starting with the run's
   certificate.
5. The four gates on the Ready card: failing before the sync, holding after; then set the provider
   live, start `base` with the E2B SDK (`sudo` works, egress to the internet works, a private
   address and TCP 25 do not), and read the usage row.
6. `shared`: with the portal identity's kubeconfig, `kubectl get pods -A` and a read of a pod in
   `kube-system` are refused, a Sandbox with a privileged, runc or hostPath pod is refused, and a
   second project gets its own `sb-` namespace and RoleBinding.
7. Edit the RuntimeClass by hand (drop its `scheduling`): the `fence` gate fails and a sandbox
   create is refused within a minute.
8. With the scheduler off: "live" is refused with the message about the scheduler; one operator
   sync fences and untaints once.
9. `placement: off` with the provider live is refused; disabled, the apply removes the machines,
   the provider's sandbox settings, the firewall groups and IP sets (`pvesh get
   /cluster/firewall/groups` names none of `sbx…`), the access and the pool.
10. The datacenter firewall off: the Check's `sandbox` row fails.

**The address of the platform cluster's ArgoCD** (`spec.access.argocd`) was run once on a lab
(2026-11-20, v0.10.0-rc.1; bridge mode, a VLAN tenant zone, no SNAT): set on an installed platform
and applied, HTTPS from the operator address with a certificate warning, no answer for a tenant VM,
two Proxmox nodes and the gateway host of the platform network, and cleared again
([An address for the platform cluster's ArgoCD](#an-address-for-the-platform-clusters-argocd)).
A lab must still show: the sign-in with the password read through the platform kubeconfig; an
address already held by a tenant load balancer giving `argocd_address_unavailable` with the
platform still serving; tenant cluster and database status in the consoles across the restart of
ArgoCD with tenants on the platform; and a site where tenant traffic leaves under a node's address
(SNAT: the single-node and the routed layouts), where the Check row `argocd-access` warns and the
source ranges cannot tell a tenant from the node. For such a site the feature is not for production.

Of the prerequisites of v0.9.0 nothing was run on a Proxmox node; the tests run the root command
against a `pveum` that records its arguments, and the firewall rule against a fake Proxmox API. To
verify on a lab:

1. **The root command on an empty lab.** Paste it once: both tokens are made with the comment
   `proxcloud:<id>`, no secret is on the screen, the last line is `PC-ENROLL:…`, and the page stores
   it. This is where `pveum user token add --output-format json --comment` and
   `pveum user token list --output-format json` (the comment in its output) are first seen to behave
   as the command assumes. Paste the command again: no token changes, no line.
2. **A foreign token.** Make `capmox@pve!capi` by hand, then paste the command: it stops with the
   text of [The root command of an installation](#the-root-command-of-an-installation) and
   `pveum acl list` is as before. Remove the token with the printed command, paste again: it goes on.
3. **A site with tokens of v0.8.x** (no comment, secrets stored in ProxCloud): the command adds the
   comment and prints no line; the platform keeps working. Then remove `capmox@pve!capi` and make
   it again by hand: the page's command now has the stop text for it (Proxmox answered `401` to the
   stored secret).
4. **The pasted line twice**, and an older line after a newer one: `409 enrollment_replayed`. A
   line printed with the node's clock set an hour ahead: `400 enrollment_clock_ahead`, and after
   the clock is set right the next line is stored.
5. **The API rule, datacenter firewall on.** Install with the platform network on a VLAN other than
   the nodes': stage 3 lists the rule, `nft list ruleset` shows the accept, the seed reaches the
   Proxmox API (stage 6 and 7 pass). Whether Proxmox takes a comma list of CIDRs as the source and
   returns it as written (the rule would otherwise be rewritten at every apply) is read here. Delete
   the rule by hand, **Apply**: it is back. **Remove**: it is gone, and no other rule changed.
6. **The API rule in mode `routed`**, datacenter firewall on: the same, which settles whether the
   rule is needed there at all (it is written either way).
7. **Blocked networks.** Save a draft on a provider whose `blockedCidrs` is empty: the page lists
   what was added, a provider sync writes the `DROP` rules, the `isolation` row is `ok`, and a tenant
   server cannot open the seed, while it reaches a load balancer address.
   Then correct the seed address and save: the page lists the old entry as taken off, and on a
   provider with a project the next sync rewrites the rules; on one without, the `isolation` row
   names the rules to delete by hand.
8. **The tenant zone.** A provider whose default zone is a `simple` zone that Proxmox does not
   have: save the settings, create a tenant network, and `pvesh get /cluster/sdn/zones/<zone>` shows
   no `dhcp`; a tenant cluster on it becomes Ready. With a zone that exists with DHCP: nothing
   changes and `tenant-network` warns.
9. **The boot store row.** A boot store that also holds a VM of your own: the row warns and names
   the VM; on a store of its own it is `ok`.

The milestone run (M0) finished once on a lab Proxmox VE cluster: attempt 5, with one **Retry**, on
the code before the changes it led to. A later round (2026-11-17, main cb2f052) removed that
platform and installed again in one run without a Retry, on cluster chart 0.1.13 and platform chart
0.1.2; the Remove listed every deleted machine with its VMID. Not run on a lab: a cluster's `401`
and `403` tried again for five looks. The lab checks L0 to L11 of the design are not recorded as passed. Until they
are recorded here, treat every behaviour above as what the code and its tests say, not as
observed, except the figures this guide marks as seen on a lab. The upgrade between BOMs has its
own list ([What the upgrade still owes a lab](#what-the-upgrade-still-owes-a-lab)); one upgrade without tenants has run, the rest has not:

| Check | What it would show |
|---|---|
| U1 (v0.9.0) | A green install is usable: on a fresh install the ready card's button smoke-tests and publishes the node image on the platform's nodes and the Kubernetes provider then offers the BOM's version; a tenant of an attached project creates a cluster with a name and a flavor. Shows whether a node image template that already serves the platform's own machines passes the smoke test unchanged, and how long the test takes beside a running platform |
| U2 (v0.9.0) | An apply after U1 leaves a version line the operator edited as it is, and offers nothing twice: a version deleted from the provider after U1 stays deleted, and `POST …/kubernetes-version` brings it back |
| U3 (v0.9.0) | `spec.tenants.attach`: `all` on a site with projects attaches each one (Kubernetes and databases in the tenant menu) and the audit log names the run's author; `new` attaches a project created afterwards and no earlier one; an apply after a hand detach does not attach again |
| U4 (v0.9.0) | The customer console's cluster form on a project without a network: **Create a network for this cluster** makes one the cluster create accepts, in a `simple` zone without DHCP |
| U5 (v0.9.0) | The same form in a `simple` zone **with** DHCP: a network made with DHCP off is not marked and its cluster create is refused with "Worker nodes need a network without DHCP"; on a project without a network the form's button makes a network that comes back with DHCP, and the form names it; a network marked with DHCP whose flag is stale (the zone's DHCP was turned off, the scheduler is off) can still be picked and its create is accepted |
| L0 | The API container routes to the platform network |
| L1 | The script, the checks and the mint drill ([proxmox-permissions.md](proxmox-permissions.md#the-mint-grant-in-m1)) |
| L2 | The seed: its CA, the bootstrap token, the CD gone after stage 5, cloud-init off |
| L3 | The seed's components and the clusterctl inventory |
| L4 | The first kubeadm join of the BOM's node image, group labels and taints, kube-vip. With it: that a machine cloned from a template with its cloud-init drive on `ide0` starts from CAPMOX's ISO (`qm config <vmid>` shows one cloud-init medium, on `ide0`; in the machine `grep 'Using data from' /var/log/cloud-init.log`), and the texts of CAPMOX's conditions as stage 7 shows them |
| L5 | Placement of Kamaji and etcd on the `kamaji` group; the locked `default` project |
| L6 | The seed ArgoCD reaching the cluster with its token; a public OCI chart pulled without credentials |
| L7 | End to end: a tenant cluster and a tenant database, and the database egress fence. Run once, partly: the two project attaches, the placement of a database on the `dbaas` group and the first tenant network passed; no tenant cluster was created (no published node image, a `simple` default zone) and no database became healthy. A later round (2026-11-17, main cb2f052) created a tenant cluster on a network of a VLAN default zone, with a router for that VLAN set up outside the product: three workers were Ready 12 minutes after the create ([the node image's known issue](#known-issue-no-postgresql-database-on-a-node-image-built-before-2026-11-16)) |
| L8 | Resume after an API restart in stages 2, 3, 7 and 9; cancel and Retry. Run once on 2026-11-17, on a ready platform only: an API restart in stage 7 of an apply that had nothing new to write resumed by itself (61 seconds later, before the job was released at a clean stop), and a cancel between the two waves of stage 6 followed by **Retry** passed. Run once more the same day with the clean-stop release in place: a restart 33 ms into stage 8 of such an apply was given back at once and went on 0.9 seconds after the stop signal, attempts 1 of 20, nothing created twice ([section 5](#5-the-stages-of-a-run)). Not run: a restart in stages 2, 3 and 9 of a real install, and a stop with a slow call in flight |
| L9 | Failure drills: `seed_not_booted`, `machine_failed`, a removed grant, a template deleted by hand |
| L10 | Seed reboot, the backup job and a restore, Remove, and Remove with the seed gone. One clean reboot was run and **failed**: the seed's root filesystem came up in ext4 `emergency_ro` and its API never answered again. The cause was the image's stale ext4 superblock field, and the documented repair brought the seed back on 2026-11-17 ([When the seed does not answer](#when-the-seed-does-not-answer)); on a seed built from the fixed image two clean reboots then passed (same address and MAC, the pinned CA still valid, all components Synced and Healthy). One `vzdump` by hand with `--mode snapshot` passed, and one with `--compress zstd` (2.39 GiB in 1 minute 45 seconds); the scheduled job was not created and `--prune-backups` was not run. The restore was first refused by Proxmox over the protected seed; with the sequence of [step 5](#when-the-seed-does-not-answer) it was run once on 2026-11-17 and passed: a healthy seed restored over itself from a three-minute-old archive, Cluster API wrote nothing, the tenant cluster was not touched. Not run: an archive older than a credential rotation or a machine change, `qm stop` on a dead guest, `--unique`, a boot store other than ZFS, an archive of the scheduled job. Remove of a ready platform and Remove from a failed stage 7 passed |
| L11 | Seed credential rotation and the expiry of the break-glass kubeconfig. The rotation and that a break-glass kubeconfig outlives it were measured; the `401` after the hour was not |

[After the install](#after-the-install) was run on a lab once (2026-11-18, the build `dev-03bc9f3`
of main, `kubectl` 1.35.10), at the API and with `kubectl`: the platform kubeconfig against the
real cluster (a token of `kube-system/proxcloud-break-glass` whose claims give it one hour, no
client certificate), the audit row `BREAK_GLASS` with `operation: platform-kubeconfig` and no
token in it, `403` for a tenant and `401` without a token, what the token may do (the reads of the
table; refused on spot checks: Secrets in any namespace, `argocd-secret`, `pods/exec`, a delete, a
patch of an Application, Cluster API's bootstrap group, a port-forward outside `argocd`), the two
ArgoCD commands on the platform cluster and on the seed, and the name and ports of the
`argocd-server` Service and the Secret `argocd-initial-admin-secret` of chart argo-cd 10.14.0.
On that build the Role allowed only `create` on `pods/portforward`: `kubectl` was refused on its
first request (a WebSocket `GET`) and fell back to SPDY without a word. The Role has `get` and
`create` since; that a port-forward now opens on its first request is not run on a lab. Also not
run: the `401` after the hour, the card in a browser, a user with the support role, the `409`,
`502` and `503` answers of the route, the row "Offers no Kubernetes version yet", and a login to
either ArgoCD.

Nothing of the presets and the control plane count was run on a lab: no install from a preset, no
platform with three control plane machines, no change from 1 to 3, no Check row `capacity` against
a real node, and no rolling update on a node that is full. The tests owed are listed in
[Presets and the control plane count](#presets-and-the-control-plane-count).

Also not run: `onboot` set by stage 7 and a reboot of a node with it
([After a Proxmox node reboots](#after-a-proxmox-node-reboots)), the `machines` row against a real
cluster, a `simple` zone created from `"dhcp": false`, a tenant cluster or a load balancer on a
network of a `simple` zone without DHCP, the identities script on a site that already has the installer's tokens, a token
secret rotation, an install on a Proxmox cluster with a single node, and `--managed-services` on a
Compose host against a published release.

The row `tenant-path` and its guide ([The path from tenants to the platform](#the-path-from-tenants-to-the-platform))
were run on a lab in part (2026-11-18, the build `dev-03bc9f3` of main): the row read `ok` for a
VLAN default zone and warned for a `simple` default zone, with the texts of the code, and a warning
did not fail the Check. That is a read of the configuration; no path was measured (on that lab the
platform gateway is on a router, so the warning was the layout's, not a seen drop). Not run on a
lab: the two other `ok` cases (the node routes the platform network; the platform's machines are
on other nodes), an EVPN zone, the warning for a Proxmox that could not be read, and the row
against a path that is in fact closed. The mechanism is stated from
source and fits what the production site showed, the node leg was run by hand on that one site at
runtime only, and the layout with a platform network routed by the node was run nowhere.

The network modes are not verified on a lab either
([The platform network: bridge, VLAN tag or routed](#the-platform-network-bridge-vlan-tag-or-routed)):
mode `routed` (the zone, vnet and subnet the run makes, the install through its SNAT, forwarding
from a tenant vnet, Remove), the VLAN tag (the seed's NIC, the machines' NICs with chart 0.1.14,
the privilege on the tag's path, a bridge that is not VLAN-aware), the rows `bridge` and
`privileges:capi` against a real cluster, **Test the tenant path** (every step of it), and the
persistent form of the node leg.

The k3s upgrade of the seed is not verified on a lab either ([The seed's k3s](#the-seeds-k3s)):
no released BOM names a newer k3s, so it has run against a fake seed only. Open, and listed as
U7 to U9 in [What the upgrade still owes a lab](#what-the-upgrade-still-owes-a-lab): that a Job
which restarts the k3s it runs under ends and is reported as ended once the kubelet is back; that
`nsenter` of the clusterctl image enters the seed's host namespaces as the Job asks (the image has
the applet with these flags; that was read from the image, not run on a seed); how long the
seed's API is away; that the seed reaches the k3s download address (nothing checks it before the
Job, as nothing does before the first boot); what k3s does to its bundled CoreDNS and
metrics-server at the version change; the hand step that puts `k3s.prev` back, and across a minor the snapshot restore with it; that the
three components run without leader election after the Job's restart and after a first boot, and
that k3s then no longer exits under disk load; and whether the
corrected `seed-backup` row reads on a real site (the row of v0.10.0-rc.1 was seen on a lab on
2026-11-20 to report "hold none" for a store that held today's archive; the correction and the
`409 seed_backup_missing` have run in tests only).
