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
> images (a lab, 2026-11-18), none on those of v0.8.1 or v0.8.2: see [Not implemented yet](#not-implemented-yet).

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
| Permissions | `admin:service:read` to read the page, `admin:service:manage` for every button, the Check and the two kubeconfig downloads included. |
| A platform network | A free IPv4 network from /8 to /29, a gateway and one to three DNS servers. It must not overlap the provider's tenant address pool (`cidrPool`), a sandbox network or a public network. Either an existing bridge on every node used, with an optional VLAN tag on the machines' NICs (mode `bridge`), or, on a one-node site, a network ProxCloud makes and the node routes (mode `routed`): [The platform network: bridge, VLAN tag or routed](#the-platform-network-bridge-vlan-tag-or-routed). |
| Routes | The API (on Compose: the API container) reaches the Proxmox API, the seed address on 6443 and the cluster's API address on 6443. The seed and the cluster machines reach the Proxmox API URL you enter, `github.com` and `raw.githubusercontent.com` (at first boot the seed downloads the k3s binary from the first and the k3s install script from the second, both checked by sha256; GitHub redirects a release download to another host of its own, which must be reachable too and is not named in the BOM) and the registries of the BOM (`ghcr.io`, which serves every chart: ours and the third-party ones of the chart mirror, `ghcr.io/mutex-solutions/charts-mirror`; and the image registries those charts pull from). Tenant networks must reach the load balancer range and nothing else of the platform network. |
| Storage | A boot store that takes VM disks on every node used; an ISO store that is local to each node and takes ISOs (default `local`); an import store with `import` content on every node an image goes to (default: the provider's `importStorage`). The import store must not be the boot store: the portal token holds `PVEDatastoreAdmin` on the import store, which reaches every disk on it ([section 10](#10-security-boundaries)). The API refuses settings that name the same store for both; the form says so before it saves when you type the same store in both fields. An empty import store takes the provider's `importStorage`, so it is refused too when that is the boot store. The import store of the settings and the provider's `importStorage` must be one store: the script of step 2 is written for the store of the settings, and the `privileges:kubemint` check reads the provider's ([when the provider's import store is the boot store](#when-the-providers-import-store-is-the-boot-store)). |
| Capacity | The smallest machine is 2 cores, 4 GiB of memory and a 20 GiB disk. The smallest layout (seed, control plane, one machine per group) is five VMs. The review step adds up cores and memory per node; it does not read what is free. |
| Root on one node | Once, for one pasted command: [the root command of step 2](#the-root-command-of-an-installation). It makes what only root can (roles, users, tokens, ACL rows, pools) and hands the two token secrets back in one line. The cluster firewall rule for the Proxmox API, the blocked networks and a new tenant zone without DHCP are no longer root steps: the install and the save of the settings do them with the portal token ([What ProxCloud does with the portal token](#what-proxcloud-does-with-the-portal-token)). |
| A router for a VLAN tenant zone | Only when the provider's tenant zone is a `vlan` zone. Proxmox gives a VLAN no gateway, no DHCP and no SNAT: a tenant network there is a bare layer 2 segment, and ProxCloud writes and reports it so (`dhcp: false`, `snat: false`; asking for either is a `400`). The site's own router must hold the first address of each tenant subnet (the `gateway` the network shows; servers and cluster workers get it as their default route) on that VLAN, route it to the load balancer range and to whatever else tenants may reach, and keep tenants apart. Without it a worker on such a network never reaches its control plane and the cluster stays `PROGRESSING`. An EVPN zone needs none of this. |
| A path from tenants to the platform network | Tenant cluster workers and database clients open TCP connections to the platform network. On a node that translates tenant traffic (a `simple` tenant zone, or an exit node of an EVPN zone) and also runs a platform machine, that connection is dropped unless the node itself routes the platform network. A one-node site with a `simple` tenant zone and the platform network on a VLAN behind the site's router is this case; a platform network of mode `routed` is not. The Check warns (`tenant-path`) and the install goes on, and **Test the tenant path** opens the path from a tenant network once the platform is installed: [The path from tenants to the platform](#the-path-from-tenants-to-the-platform). |
| A tenant network zone without DHCP | Only for tenant Kubernetes clusters, not for the install or for databases: a cluster's nodes get fixed addresses, so its network must be in an EVPN or a VLAN zone, or in a `simple` zone without a DHCP server. With a provider whose default zone (`config.network.defaultZone`) is a `simple` zone that ProxCloud created with DHCP, which is its default, a cluster create on its networks answers `400 Worker nodes need a network without DHCP (an EVPN or VLAN zone, or a simple zone without a DHCP server)`. The Check reads the zone in Proxmox, warns (`tenant-network`) when it has DHCP, and the install goes on; change the provider's default zone before you offer clusters ([managed-k8s.md](managed-k8s.md)). Not affected: a Kubernetes provider with shared workers (`cluster.shared`), which uses no tenant network, and a `simple` zone without DHCP. Such a zone is made in one of two ways: by hand in Proxmox before the first tenant network (a zone that exists is used as it is), or by ProxCloud when the zone's config says so, `"zones": {"<id>": {"kind": "simple", "dhcp": false}}` (from v0.8.1; it applies to a zone that does not exist yet, and an existing zone is never changed). Networks of such a zone answer `dhcp: false` and get no DHCP range; servers, cluster nodes and load balancers there get addresses the platform assigns. The key is refused on a zone of another kind (`400`, naming the key). The setup wizard keeps a zone's other keys when you change its kind there, so take `"dhcp"` out of the zone's config (Cloud providers) before you change such a zone to VLAN or EVPN. The key is not verified on a lab. |
| The datacenter firewall | On, with the provider's isolation rules written by a provider sync once the installation is saved: the Check verifies that tenant traffic to the seed, the API address and the node range is dropped. Saving the settings adds those addresses to the provider's blocked networks. With the firewall on, a node drops what does not come from its own network, so the seed and the platform machines need a rule to reach the Proxmox API: the install writes it (`pve-api-rule`). |

The page also warns when the job scheduler is off: tenant clusters and databases then update their
status only when a provider sync runs, and no project is deleted: "Delete now" is refused with a
`409` that says so, and a delete with the grace period is accepted but stays scheduled until the
scheduler runs ([Compose README](../deploy/compose/README.md#optional-features)). It does not
block an install.

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
   machine) it clones a published Ubuntu or Debian template of the catalog into the pool
   `pc-templates` as `pc-probe-<installation id>`, puts it on that network (with a fixed address,
   or by DHCP where the zone has a DHCP server, as a tenant's server gets its address) and
   starts it. Without such a template on that node it stops before step 2
   (`409 no_os_template`): publish one under Template updates. With the scheduler off nothing is
   published by itself.
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
   until you removed it.

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
**Not verified on a lab:** the test's tests use a fake Proxmox API. Its first lab run is the
reproduction of the production finding (`run-1` lab test "reproduce": expected `failed` from the
node that holds the address with a `simple` tenant zone, `passed` from another node).

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
  and nothing is rendered untagged. **The BOM of this build names 0.1.13** (0.1.14 is not
  published yet), so a tag is refused until a release whose BOM names 0.1.14.
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
  this release names 0.1.13, which keeps this; chart 0.1.11 gave up after 10.
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
| The cluster firewall rule for the Proxmox API | Stage 3 of an install, before the seed is cloned, and every apply; **Remove** deletes it | `Sys.Modify` on `/` (as for the isolation rules) | One rule: `in ACCEPT tcp`, source the seed's address and the node range (as CIDRs; not the load balancer range), destination the host of the Proxmox API URL of the settings when it is an address (no destination for a name), the URL's port, comment `proxcloud:pve-api:<installation id>`. A rule with that comment that differs (the settings changed, or it was disabled by hand) is replaced; no other rule is read or touched, and the `management` IP set is not used. It is written with the datacenter firewall on or off. It is written in mode `routed` too: the node holds the gateway of the vnet, but the firewall's default management set is the network of the node's own address only (read in the Proxmox documentation, not tried) |
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
| `tenant-path` | Never, and absent when the provider names no zone. It reads the layout and **warns** when a node that translates tenant traffic (a `simple` default zone: the node of the tenant's VM; an EVPN default zone: its exit nodes) also hosts a platform machine and does not route the platform network itself, or when Proxmox could not be read: a tenant's TCP connection to the platform network is then dropped on that node, tenant Kubernetes workers never join and tenants cannot reach managed databases. It opens no connection: `ok` is not proof, and a leg added on a node by hand is not seen. It is `ok` for a platform network of mode `routed`. **Test the tenant path** below the rows opens the path on an installed platform | Nothing for the install. Before tenants create clusters or databases: one of the two layouts of [The path from tenants to the platform](#the-path-from-tenants-to-the-platform) |
| `csi-vmid` | Never. It **warns** when a guest of the cluster has VM id 9999, the id proxmox-csi names its volumes after, or when that could not be read | Nothing for the install. Never destroy VM 9999 with "Destroy unreferenced disks owned by guest" ([VM id 9999 and the CSI volumes](#vm-id-9999-and-the-csi-volumes)) |
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
| `boot-store` | Proxmox has no storage of the boot store's name, or it takes no disk images. It **warns** when the store also holds disks of VMs that are not the platform's (tenant servers, or VMs of your own), or when its content could not be listed: `csi@pve!csi` holds `Datastore.Allocate` on the whole boot store and its secret is readable inside the platform cluster ([section 10](#10-security-boundaries)). The disks are listed with the CSI token, the only one that sees them all, and the platform's own machines are told apart with the CAPI token, so before both tokens are stored the row says the disks are not read yet. Not counted: the installation's own machines, the templates of the pool `pc-templates`, CSI volumes (`vm-<id>-pvc-*`) | A missing store: name an existing one in step 1 (`pvesm status`). No disk images: `pvesm set <store> --content <its content>,images`, which the message spells out. The warning: a store of its own for the platform, on ZFS `zfs create <pool>/pcplat && pvesm add zfspool pcplat --pool <pool>/pcplat --content images,rootdir --sparse 1`, then name it in step 1 and run the root command of step 2 again. The install works without it |
| `pve-api-rule` | Never. `ok` before the first install (the install writes the rule) and when the rule is in place; it **warns** on an installed platform whose rule is missing, changed or disabled, or when the cluster firewall rules could not be read. The message spells the rule out and says when the datacenter firewall is off | Nothing before the install. Installed: **Apply this release** writes the rule again |
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
yet. The page reads this from the provider rows (`GET /admin/service`):

- *The Kubernetes provider offers no version.* The install registers it with an empty
  `config.cluster.versions`, so every cluster create is refused with "this provider offers no
  Kubernetes version yet". ProxCloud does not fill it in. Do two things: publish a Kubernetes node
  image in **Template updates**, then enter the line it prints for the image
  (`<version>=<node image id>`, [managed-k8s.md](managed-k8s.md)) on the provider's **Connection** tab. The row turns to "Offers Kubernetes <versions>" once one is set.
- *Neither provider is attached to a project.* Both are created with
  `attachToNewProjects: false`. On each provider's **Projects** tab, turn the policy on for new
  projects and attach the existing ones. The card shows the policy only: whether an existing project
  is attached it does not read.

**Platform cluster access.** **Download platform kubeconfig** (`GET …/platform-kubeconfig`) saves
`<name>-platform.kubeconfig`. It is described under
[Credentials](#8-credentials-and-the-seed-backup): one hour, read access without Secrets, and the
way into the cluster's ArgoCD.

**Open ArgoCD.** There are two ArgoCD instances and v0.8.x gives neither an address: no Service of
type LoadBalancer, no ingress, no URL (planned for v0.10.0, for the platform cluster's only). Both
serve plain HTTP inside their cluster, so the way in is a port-forward from a machine that has
`kubectl` and reaches the API address of the cluster. The card prints the commands with the
installation's file names:

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
| 7 | Build the platform cluster (`cluster`) | The CAPI token's Secret and the Application `pc-platform` on the seed; MachineDeployments of nodes no longer in a group are deleted; then waits for the control plane, the admin kubeconfig and every Machine Running. Before it writes anything it reads each node template: a template with a Proxmox cloud-init drive on another slot than `ide0` is refused (`machine_failed`, with the `qm set` command that removes the drive), because CAPMOX puts its own on `ide0` and a machine with both starts from the wrong one. A template without the drive stays usable everywhere: CAPMOX brings its own, and the template-updates smoke test gives its test VM one when the template has none, so such a template can still be tested and published for tenants. A template whose config the portal token may not read is not checked: the stage goes on and shows an item `template-<vmid>` in state `not checked`. A template that passed has the same item in state `ok`; one that Proxmox no longer knows has none (CAPMOX reports it on the machine). The items list each machine with its VMID (`no VM` until CAPMOX has cloned it) and, while it is not Running, the step CAPMOX is at with its error (the reason and message of the ProxmoxMachine's condition `VirtualMachineProvisioned`). CAPMOX calls a VM provisioned once it runs and has its addresses; for a machine that then does not join (kubeadm, the API address, etcd) the item carries the reason and message of the Machine's own condition `Ready` after `Cluster API:`. The message of the waiting stage repeats, in this order, what the ProxmoxCluster reports, the KubeadmControlPlane's condition `Available` while it is not `True` (`Control plane:`), the machines' errors and, last, the state of the Application `pc-platform` while its sync is not done (not while it is only Progressing, which it is for as long as machines are built). On a **Retry** that finds a ProxmoxCluster still without a condition, the run updates an annotation on it (`proxcloud.io/retried-by`) once, so CAPMOX opens the Proxmox API again at once instead of at the end of its backoff of up to 17 minutes; the 10 minutes count from then. Steps CAPMOX tries again (`CloningFailed`, `PoweringOnFailed`) are waited for. `VMProvisionFailed` and `TaskFailed` end the stage: CAPMOX does not go on with such a machine. Once every machine is Running the stage sets `onboot` on each VM of the platform that lacks it, with the CAPI token, so that the machines start again with their node ([After a Proxmox node reboots](#after-a-proxmox-node-reboots)); a VM it could not set does not fail the stage, and the Check's `machines` row names it. | 60 min |
| 8 | Reach the platform cluster (`workload-access`) | Creates the seed ArgoCD's service account on the cluster and a token for it (one year, renewed by any later apply when under 90 days are left), kept only in the seed's cluster Secret; writes the CSI token's Secret for proxmox-csi. | 10 min |
| 9 | Install the platform components (`workload-platform`) | The cluster's components wave by wave, each wave only when the one before is Synced and Healthy with its sync `Succeeded` (as in stage 6). After the first wave (Cilium) every node must be Ready with its group label. The platform's own volumes are recorded for Remove at every look of the stage, so also when it fails inside `kamaji-etcd`'s wave. | 20 min per wave |
| 10 | Create the two providers (`register`) | Creates the Kubernetes and Databases providers, or updates the ones this installation created before, with a kubeconfig of the cluster's `proxcloud-system/proxcloud` service account. | 5 min |
| 11 | Finish (`done`) | Records the components and the BOM as installed. The phase becomes `ready`. | 5 min |

An upgrade (an apply that moves the platform to this release's BOM) is the same eleven stages with the
target BOM, two more waits and one more check: [Upgrade between BOMs](#upgrade-between-boms).

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
| `seed_lost` | 3, Remove | A seed that had become ready is gone, or the VM at its VMID is no longer this installation's. It is not rebuilt: a new, empty seed would have no record of the cluster's machines | Remove with the option for a missing seed, then install again ([Remove](#remove)) |
| `component_unhealthy` | 6 to 9 | An Application is not Synced and Healthy in time (the message names it and its condition); or something could not be rendered or applied, in stage 7 the Application `pc-platform` | Download the seed kubeconfig to look closer, then **Retry**. ArgoCD does not sync an Application again by itself once its retries are spent; the run starts the sync of one whose last sync ended `Failed` or `Error`, once per run, so no manual sync is needed after the cause is fixed. When that sync ends `Failed` or `Error` too, the stage fails at once with what the sync failed on (the hook or resource and its message) instead of waiting for its time limit; **Retry** starts the sync once more. ArgoCD takes about 40 minutes to spend the ten retries of one sync, longer than the time limit of stage 6 or of a wave, so the stage does not wait for that either: once one sync has failed four times (about 8 minutes of ArgoCD's backoff), the last time in this run, and ArgoCD shows the hook or resource it failed on, the stage fails with that text while ArgoCD still tries. After a **Retry** the same sync gets one more attempt before the stage fails on it again |
| `capi_init_failed` | 6, 9 | clusterctl did not install the providers of the BOM; or a provider is installed at another version than the BOM names (provider upgrades are not in this release) | The same |
| `machine_failed` | 7 | CAPMOX gave a machine up: its ProxmoxMachine's condition `VirtualMachineProvisioned` is `False` with `VMProvisionFailed` (not enough memory on the node for its scheduler, no free VMID, the template not found, a VMID held by another VM) or `TaskFailed` (a Proxmox task failed: a clone, a resize, a config change). The message has the machine's name, its VMID and CAPMOX's text. Also: a node template has a Proxmox cloud-init drive on another slot than `ide0` (the message has the command), or its config could not be read for another reason than a missing permission | Correct what CAPMOX's text names. CAPMOX does not try that machine again, and **Retry** alone finds it as it was: delete the Machine on the seed with the seed kubeconfig (`kubectl -n pc-platform delete machine <name>`, the command is in the message), Cluster API makes a new one, then **Retry**. Do not delete the Machine when the text says the VM's name does not match (`expected VM name to match`): the VMID in the ProxmoxMachine then belongs to another VM, and CAPMOX deletes the VM at that VMID when its token may. The message then has no delete command and says so: look in Proxmox whose VM it is, and delete the Machine only once its ProxmoxMachine no longer names that VMID. For the template: run the `qm set` command of the message on that node, then **Retry**. The template then has no cloud-init drive, which is fine for its machines and for its smoke test ([template-updates.md](template-updates.md#smoke-test-templateupdatesmokev1)) |
| `capmox_auth_failed` | 7 | Proxmox refuses the CAPI token when CAPMOX uses it (a condition of the ProxmoxCluster or a ProxmoxMachine says so); or CAPMOX has set no condition on the ProxmoxCluster for 10 minutes, which is how a Proxmox API URL the seed cannot reach and a refused token both show, because CAPMOX opens the API before it writes anything | Check the Proxmox API URL as the platform network reaches it ([What no check reads](#what-no-check-reads)), paste the secret again, **Retry**. The Retry makes CAPMOX try at once (it updates the ProxmoxCluster), so a fix shows within its 10 minutes. CAPMOX's log on the seed says which it is: `kubectl -n capmox-system logs deploy/capmox-controller-manager` |
| `control_plane_timeout` | 7 | Not every machine is Running or the control plane is not initialized in 60 minutes. The items and the message carry the step each machine is at and CAPMOX's last error | **Retry**; the time limit starts again. Look at the machines with the seed kubeconfig |
| `workload_api_unreachable` | 8 to 10, Remove | The cluster's API does not answer from the API's host, or its kubeconfig or CA on the seed is not usable, or it refused the admin kubeconfig or the request with a `401` or `403` at five looks on end (as for `seed_api_unreachable`) | Add a route from the ProxCloud host to the API address, **Retry**. At Remove: restore the route and **Remove again**, or tick the option for a cluster that does not answer |
| `nodes_timeout` | 9 | Not every node is Ready with its group label after the first wave | **Retry** |
| `provider_conflict` | 10 | A provider with the same name exists that is not this installation's, two rows carry its marker, or the provider validation refused the row | Correct or rename that provider under Cloud providers, **Retry** |
| `bom_unsupported` | start, plan | The installed BOM is not one this release carries: its own, or that of an earlier release from 0.8.0 on. A platform installed by a build of `main` (`dev-<sha>`) gets this after an upgrade | A platform moves only between BOMs a release carries ([Upgrade between BOMs](#upgrade-between-boms)). Go back to the ProxCloud release that installed the platform. Or, if no tenant cluster or database must be kept: delete them all (Remove refuses with `tenants_exist` otherwise), remove managed services, which deletes the platform cluster, and install again |
| `upgrade_refused` | start | An apply names this release's BOM as its target (`targetBom`) and the two BOMs differ in something this release does not move, or the target is a `-dev` BOM. `409`, no run is made; `details.reasons` has one line per entry, the same lines as the plan | Nothing is wrong with the platform: it stays on its BOM, and **Apply this release** keeps working. The move needs a later release ([Upgrade between BOMs](#upgrade-between-boms)) |
| `unexpected_rollout` | 7 | An upgrade run only. The cluster chart of the target BOM would create a machine or bootstrap template, or change the control plane: Cluster API would replace machines. Nothing of the cluster was synced; the Application `pc-platform` is left at the new chart version without automated sync | If machine sizes, nodes or node groups were changed in the settings together with the upgrade, put them back, save, **Retry**. Otherwise the release is at fault: see [Upgrade between BOMs](#upgrade-between-boms), "When the upgrade cannot finish" |
| `tenants_unhealthy` | 9 | An upgrade run only. After a wave, a tenant cluster or database that was healthy when the run reached stage 9 is not: its Application on the platform cluster's ArgoCD is not Healthy, or its TenantControlPlane is not Ready. The message names them. The stage waits for them up to the wave's 20 minutes | Look at the tenant the message names. **Retry** goes on from the same wave and waits for it again: the list of tenants that were down before is taken once per upgrade (`status.tenantsDownBefore`), not per run, so a tenant a wave broke holds every run until it is healthy or deleted |
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
6. wipes the stored secrets and the recorded status.

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
| The seed credential (a service account token, cluster-admin on the seed) | Encrypted in `platformSecret` | **Replace the seed credential**: creates the next token Secret (`proxcloud-portal-token-g<n>`), checks it, stores it, then deletes the older ones, which ends their tokens. Refused during a run. If it stops halfway, press it again; the next apply also removes token Secrets nothing stores. The audit log gets one entry, `ROTATE_CREDENTIAL` on the installation, with the names of the token Secret before and after (`fromSecret`, `toSecret`) and never a token. A rotation that stopped after it had stored the new credential gets that entry with outcome `FAILURE`. The old token does not end at that instant: the seed's API server remembers a token it has checked for some seconds (measured on a lab: the old token still answered 6 to 7 seconds after the rotation had returned). **A seed backup taken before a rotation is of no use after it**: the archive holds only the older token Secret, a seed restored from it refuses the stored credential, and ProxCloud cannot make a new one on a ready seed (the bootstrap token is gone). So run a backup by hand right after every rotation, as root on the seed's node: `vzdump <seed VMID> --storage <backup store> --mode snapshot --compress zstd` ([step 5](#when-the-seed-does-not-answer)). That archive holds the seed's secrets: read the warning under **Seed backup** below before you choose the store |
| The cluster's admin kubeconfig | On the seed only (Cluster API keeps it). A run reads it into memory, and so does a platform kubeconfig download, which never hands it out | Not stored by ProxCloud |
| Service account `kube-system/proxcloud-break-glass` on the cluster, with ClusterRole and ClusterRoleBinding `proxcloud-break-glass` and Role and RoleBinding `proxcloud-break-glass` in `argocd` | On the cluster. It has no token Secret; written by the first platform kubeconfig download and again by each one | Deleting the service account ends every token issued for it. The next download creates it again |
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
> read it. Check the mode of the files and who can mount the store before the first backup, and
> again after a backup by hand. Nothing in ProxCloud checks the store you choose.

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
  CAPMOX in tenant pools. They have `onboot` only when their node image template has it (next
  paragraph); the others must be started in Proxmox after a node reboot.

**Start at boot from the template.** From v0.9.0 a Kubernetes node image that **Template updates**
imports gets `onboot: 1` on the template itself. A Proxmox clone copies every option of its source
except the disks, MACs and snapshot fields it rewrites, and CAPMOX v0.9.2 has no field for `onboot`
and writes it nowhere (both read in code: `PVE/API2/Qemu.pm` on Proxmox VE 9.2, CAPMOX's
`internal/service/vmservice`). So every machine cloned from such a template has it from its first
boot: tenant workers, platform machines, and the replacement of either, with no call per VM. Stage
7's pass above stays as the backstop for machines made from older templates. What it does not
cover:

- A template imported before v0.9.0, or built by the installer's image pipeline
  (`images/build.py`), has no `onboot`; machines cloned from it get none. To add it, as root on the
  node: `qm set <template vmid> --onboot 1`. Machines that exist already keep what they have
  (`qm set <vmid> --onboot 1` on each, or for platform machines the next apply).
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
  can create a cluster. Until then every cluster create is refused with a `400` that says so
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

Nothing in this section has run on a lab or on a production site. It says what the code and its
tests do; [the lab tests it still owes](#what-the-upgrade-still-owes-a-lab) are listed at its end.

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
| seed `capi`, platform cluster `capi-tenant` (chart `proxcloud-platform`) | chart | ArgoCD syncs the chart and runs its clusterctl hook, which finds every provider at its version and changes nothing |
| platform cluster `platform` (chart `proxcloud-platform`) | chart | ArgoCD syncs provider RBAC, tenant AppProjects and the MetalLB pool |
| seed `pc-platform` (chart `proxmox-kamaji-cluster`) | chart | ArgoCD syncs the platform cluster's Cluster API objects, after the check that no machine is replaced (below) |

A chart of the product moves only to a version whose BOM entry lists the installed version in
`upgradeFrom`: the release checked that pair ([ci-release.md](ci-release.md#what-a-release-checks-about-its-bom-pairs)).

Every other difference refuses the whole upgrade, each with its own line in the plan and in the
`409 upgrade_refused` of a run. All lines begin with "not supported by this release":

| Entry | Why it is refused |
|---|---|
| `kubernetes.version` | Every machine of the platform cluster is replaced: the Kubernetes upgrade of a later release |
| `kubernetes.nodeImage` | A new node image replaces every machine |
| `seed.image` | Only a rebuilt seed would boot it, and a ready seed is never rebuilt |
| `seed.k3s` (version or a checksum) | No stage upgrades k3s on a running seed |
| `images.kubeVip` | It is in the control plane's machine spec: the control plane machine would be replaced |
| `images.clusterctl`, `capi.seed.*`, `capi.workload.*` | The Cluster API providers are upgraded (`clusterctl upgrade`) only by a later release |
| A third-party chart: `argocd`, `cert-manager`, `cilium`, `metallb`, `cnpg`, `proxmox-csi`, `kamaji-crds`, `kamaji`, `kamaji-etcd` | Not in this release. Each restarts something tenants use (the CNI, the load balancer speaker, every database's operator, every tenant control plane's datastore), and none of those restarts has been measured |
| A chart of the product at a version that does not list the installed one in `upgradeFrom`, or the same version with another digest | The release did not check that move |
| A component added or removed, or changed in its wave, install mode, namespace, node group, chart source or values | A run moves only a chart version |
| The target's `release` ends in `-dev` | No later build carries a development BOM, so the platform could afterwards be applied by that one build alone |

So a release that changes a machine or bootstrap template (a NIC option on the machines, kube-vip,
a kubelet argument) reaches new installs only, and an installed platform stays on its BOM until a
release rolls machines. The release notes of such a release must say so.

### Before you start

- Take a backup of the seed VM (`vzdump`, [Credentials and the seed backup](#8-credentials-and-the-seed-backup))
  and a dump of the portal database. The seed holds the Cluster API state of every machine of the
  platform cluster. Restoring both after a failed upgrade has not been tried.
- Do not change machine sizes, nodes or node groups in the same step: apply such a change before
  the upgrade or after it. Together with the upgrade it is refused in stage 7 (`unexpected_rollout`).
- Every tenant cluster and database keeps running. Tenants that are unhealthy before the upgrade
  do not stop it.

### Starting it

Step 4 shows **Upgrade to release X** with the table of changes when the upgrade is allowed, and
the reasons when it is not. The button asks for the phrase `upgrade <installation name> to
<release>`. By API: `POST …/runs {"kind":"apply","targetBom":"<sha256 of this release's BOM>",
"confirm":"upgrade <name> to <release>"}` ([api.md](api.md)).

### What the run does

The eleven stages of an apply, rendered from the target BOM:

- **Stages 1 to 5** find the images and the seed as they are: both BOMs name the same images.
- **Stage 6** applies the seed's ArgoCD HelmChart again (the same chart version, the new release
  label), then the seed's waves. `capi` at its new version passes when ArgoCD reports it Synced and
  Healthy against the new version with its sync `Succeeded`, and the clusterctl inventory equals
  the BOM.
- **Stage 7** makes the rollout check before it lets the cluster chart sync. The names of the
  machine and bootstrap templates carry a hash of their spec, so a chart that renders another
  template makes a new one and Cluster API replaces every machine that used the old one; a changed
  KubeadmControlPlane replaces the control plane machine. The stage therefore applies the
  Application `pc-platform` with the new chart version and **without** automated sync, waits until
  ArgoCD has compared the cluster with it, and reads the comparison (`status.resources`): a
  `ProxmoxMachineTemplate` or `KubeadmConfigTemplate` that is not Synced (and is not one ArgoCD
  would only prune, an old template), or a `KubeadmControlPlane` that is not Synced, fails the run
  with `unexpected_rollout`. Nothing was synced then. Otherwise the stage applies the Application
  as always, automated, waits until the last sync is of the new version, and goes on as in any apply.
  There is no sync to wait for when the new chart renders every object as it is and the Application
  is out of sync only for what it would prune (the machine templates of an earlier size: it never
  prunes them): ArgoCD starts no sync for that, so the stage passes when the comparison is of the
  new version and nothing but such objects is out of sync. That ArgoCD starts no sync then is read
  from its source, not seen on a lab.
- **Stage 8** is unchanged.
- **Stage 9** notes which tenants are unhealthy before its first wave (once per upgrade, in
  `status.tenantsDownBefore`; a Retry keeps the note), then applies the waves. After
  each wave it waits until every component of the wave is Synced and Healthy against the new
  version, and until every tenant that was healthy at the start is healthy again: the tenant
  Applications on the platform cluster's ArgoCD must be Healthy and every TenantControlPlane Ready
  (`tenants_unhealthy` while not).
- **Stage 10** writes the tenant charts of the target BOM on the two providers.
- **Stage 11** records every component and sets `status.bom` to the target.

### What the record says meanwhile

| Field | During the upgrade | After the last stage |
|---|---|---|
| `status.bom` | The BOM the platform left: "last fully applied" | The target |
| `status.targetBom` | The target, from the start of the first run | Absent |
| `status.tenantsDownBefore` | From the first time a run reaches stage 9: the tenants that were not healthy then | Absent |
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

The check reads what ArgoCD reports and has not run against a real ArgoCD. If on a site it fails
for a cluster chart that provably changes no template (compare `kubectl -n pc-platform get
proxmoxmachinetemplates,kubeadmconfigtemplates` with `helm template` of the new chart), the API's
environment variable `PROXCLOUD_PLATFORM_SKIP_ROLLOUT_GATE=true` (`PLATFORM_SKIP_ROLLOUT_GATE` in
the Compose `.env`) turns it off. With it off a chart that does change a template replaces every
machine of the platform cluster at once, control plane and all node groups together.

### What the upgrade still owes a lab

Lab only; none of it has run. Each drill build must carry the BOM of the build before it
([ci-release.md](ci-release.md#upgrade-drill-builds)).

| Test | What it would show |
|---|---|
| U1 Record-only move: install from a release (v0.8.0, v0.8.1 or v0.8.2) with one tenant cluster and one database under a write loop; upgrade to a drill build whose BOM differs in `release` alone | Every stage done in seconds, no Application revision changed, `status.bom` moved; whether k3s's helm controller runs a Job for the seed ArgoCD when only the HelmChart's label changes |
| U2 Own-chart move: a drill build that bumps `proxcloud-platform` (and, when one exists that changes no template, `proxmox-kamaji-cluster`), with a probe of the tenant API and the database address once a second | The gaps, if any; that the clusterctl hook changes nothing; that `status.components` moves per wave |
| U3 The rollout check against the real ArgoCD: a drill build whose cluster chart changes a machine template value | That ArgoCD lists the new template as not Synced before any sync, that the run fails `unexpected_rollout` with no machine replaced, and that an unchanged chart leaves the KubeadmControlPlane Synced. On a platform that once changed a machine size (old templates left, the Application OutOfSync for them alone), a drill build whose cluster chart version is new and renders identically: that ArgoCD starts no sync and stage 7 passes all the same. Also that the result of the last sync carries its source (`status.operationState.syncResult.source`), which stage 7 waits on when the Application stays OutOfSync |
| U4 Failure and Retry: a drill build with a wrong chart digest for `platform`, so workload wave 3 fails; then the corrected build | No Application goes back to the old revision; with the corrected build step 4 shows **Upgrade to release Z instead**, and the later build's BOM is accepted over the unfinished target |
| U5 The tenant wait: stop a tenant control plane's pod during stage 9 | The run waits with `tenants_unhealthy` and goes on when it is Ready. Then let the wait run out (the run fails) and Retry with the tenant still down: the new run waits again |
| U6 Restore: after U4's failure, restore the seed's `vzdump` and the portal database dump | Whether that returns the platform to the BOM it left |

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

- **No release before v0.8.0.** The BOM of v0.8.0 is `0.8.0`, that of v0.8.1 is `0.8.1`, that of v0.8.2 is `0.8.2` (`main` carries a `-dev` one between releases, now `0.9.0-dev`). `--managed-services` needs v0.8.0 or later: no
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
    platform installed on a `-dev` build (a `dev-<sha>` build of `main`, `0.8.0-dev` or
    `0.9.0-dev`) is therefore refused (`bom_unsupported`) by every later build whose BOM file
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
  `proxmox-kamaji-cluster` 0.1.13, for the platform cluster (which it renders as 0.1.12 does) and
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
- **No IPv6** on the platform network, and one platform network per installation. A VLAN tag needs
  a BOM that names cluster chart 0.1.14, which this build's does not
  ([the three forms](#the-platform-network-bridge-vlan-tag-or-routed)).
- **No automated node leg** for an existing VLAN site: the commands are in
  [The path from tenants to the platform](#the-path-from-tenants-to-the-platform), by hand.
- **One control plane machine.** The settings take no control plane count.
- **No sandbox node group**, and no sandbox cluster made by the install. Both layouts of a later
  release fit the saved settings without a change of format. Separate: one more entry in
  `spec.clusters[]`; the API rule's sources, the blocked networks and the boot-store nodes are
  already computed over every cluster. Shared (a node group on the platform cluster): the group
  gets an optional address range of its own, because sandbox nodes must not reach the Proxmox API;
  that range is then left out of the API rule's sources and fenced as a sandbox network. Until
  then the rule's source is the whole node range, which is right while every machine in it is a
  platform machine.
- **One installation on the page.** The API keeps one per Proxmox provider; the page shows the first.
- **No route deletes a draft or an installation record.**
- **No block volumes for tenant clusters and no database backups** on Compose
  ([section 9](#9-limits-on-the-compose-install)).
- **No per-namespace RBAC for the providers' service account** on the shared cluster.

## Not verified on a lab yet

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
own list ([What the upgrade still owes a lab](#what-the-upgrade-still-owes-a-lab)); none of it has run:

| Check | What it would show |
|---|---|
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
