# Connecting a Proxmox VE Cluster

ProxCloud provisions and meters against one or more Proxmox VE clusters. You register each cluster as a *cloud provider* in the admin portal. One provider is one cluster, and one cluster is one region. ProxCloud drives the cluster through its REST API on port 8006 with a single API token. Tenancy is enforced on the platform side: every project gets its own resource pool, and ProxCloud checks ownership of every VM, network, firewall group and GPU mapping against its own database before it changes anything.

The **Add provider** dialog also offers three other provider kinds: `ceph-s3` (S3 object storage on Ceph RGW), `kamaji` (managed Kubernetes) and `dbaas` (managed databases). This page covers the `proxmox` kind.

## Before you start

You'll need:

- **The cluster's API URL**, e.g. `https://pve1.example.com:8006`.
- **A dedicated, privilege-separated API token**, such as `proxcloud@pve!platform`, holding the `ProxCloudPlatform` role. Do not use a `root@pam` token. The role, its ACL paths and the pool layout are defined in the repository document `docs/proxmox-permissions.md`. In short:
  - `ProxCloudPlatform` on `/` (no propagation), `/pool`, `/sdn` and `/storage`, and on `/mapping/pci` for GPU clusters.
  - Read-only `PVEAuditor` on `/nodes`.
  - `NoAccess` on the `proxcloud-system` pool.
- **The datacenter firewall switched on** (Datacenter → Firewall → Options). Without it, security groups and tenant network isolation are not enforced.
- **The nftables firewall on every node** (`nftables: 1` under Node → Firewall → Options). The legacy iptables firewall ignores the SDN vnet rules that isolate tenant networks.
- **A CIDR pool for tenant networks**, e.g. `10.100.0.0/16`. Every project subnet is carved from it.

## Registering the provider

Open **System → Cloud providers**, choose **Add provider**, and pick the **Proxmox VE** kind.

| Field | Description |
|---|---|
| Display name | A unique name; this is what clients see in the portal. |
| Region | The region name for this cluster, e.g. `pve`. It can't be changed after creation. |
| API URL | The cluster API endpoint, e.g. `https://pve1.example.com:8006`. |
| API token ID | The full token identity, e.g. `proxcloud@pve!platform`. |
| Token secret | The token's secret. It is encrypted at rest and never returned by the API. When you edit the provider later, leaving it blank keeps the stored secret. |
| TLS certificate fingerprint | Optional SHA-256 pin for a self-signed cluster certificate (`pvenode cert info` prints it). Empty means the system CA pool is used. |
| Skip TLS verification | Last resort. Prefer the fingerprint pin. |
| Pool prefix | Default `pc-`. Each project gets the pool `<prefix><projectId>`, which is its tenancy boundary. Don't give your management pool a name that starts with this prefix. |
| Tenant network isolation | `Shared zone + vnet firewall` (default) or `EVPN zone per project`. Both drop cross-tenant traffic with vnet firewall rules inside the CIDR pool; the EVPN mode also gives every project its own EVPN zone (VRF). |
| EVPN controller | Only for `EVPN zone per project`: the controller every project zone uses. Blank means the default zone's controller. |

Saving creates the provider as `PUBLIC` and enables the four services a Proxmox cluster serves in its region: `compute`, `network`, `volumev3` and `image`. There is no discovery round-trip. Traffic metrics start as `none` (see *Usage metrics* below).

## Test connection

**Test connection** is available both in the Add dialog and on the provider's **Connection** tab. It authenticates with the token and then runs these preflight checks. The test **fails** when:

- the datacenter firewall is disabled;
- any online node still runs the iptables firewall instead of nftables;
- a configured public network sits on a node's management bridge, meaning the bridge carries the node's own IP or default gateway.

It **warns** without failing when:

- the token belongs to `root@pam`;
- the token lacks a `ProxCloudPlatform` privilege. A cluster without GPUs can, for example, leave out the PCI mapping privileges on purpose.

On success it reports the Proxmox VE version and how many nodes, storages and PCI mappings the token can see.

<!-- screenshot: /docs-img/add-proxmox-provider.png — ProxCloud admin: the Add provider dialog with the Proxmox VE form and a Test connection result -->

## Proxmox resources

The provider's **Connection** tab has a **Proxmox resources** card with one JSON editor per configuration block. Each block saves on its own.

| Block | What it holds |
|---|---|
| `network` | `defaultZone` and `zones` (SDN zones by kind: `evpn`, `vlan` or `simple`, with per-zone settings such as peers, exit nodes and, for `evpn` zones, `mtu` (default 1450)). Also `cidrPool` (required), `isolation`, `evpnController` and `publicNetworks`. |
| `storageProfiles` | Named storage profiles mapping nodes to a Proxmox storage (`"*"` for every node), with capability flags: `shared`, `snapshots`, `thin`, `liveMigrate`, `replication`. Flavors and volumes refer to a profile by its slug. |
| `templates` | The OS template catalog clients create servers from: `{slug: {vmid, node, name, osDistro, minDiskGb}}`. |
| `gpus` | GPU PCI mappings by mapping id, each with the model `alias` used everywhere GPU-related, e.g. `{"<mapping-id>": {"alias": "nvidia-4090"}}`. |

### Public networks

Projects have no floating IPs. A server becomes reachable from outside when it gets a public NIC: a second interface on an operator-designated bridge, with one address allocated from that network's pool and pinned by the VM firewall. Declare those bridges under `network.publicNetworks`:

```json
"publicNetworks": [
  {
    "id": "vmbr1",
    "name": "Internet",
    "cidr": "203.0.113.0/24",
    "gateway": "203.0.113.1",
    "ranges": ["203.0.113.10-203.0.113.200"],
    "exclude": ["203.0.113.53"]
  }
]
```

`ranges` and `exclude` are optional; without `ranges` the whole CIDR is the pool. Never use the management bridge. **Test connection** rejects a public network on a node's management bridge. The traffic-metrics job also uses these bridge ids to tell public traffic from private traffic. Which public networks a given project may use is set on the project's detail page (see [Running the Platform](/docs/platform-admin/overview)).

## Default quota

The provider's **Quota** tab sets the default quota seeded into each new project at bootstrap: `servers`, `vcpus`, `ramMb`, `diskGb`, `networks`, `floatingIps` (public addresses) and `gpu` (the any-model GPU limit). A project's own quota can then be changed on its **Quota** tab.

## Usage metrics

The **Metrics** tab chooses where the hourly traffic-billing job reads per-VM network usage:

- `none` is the default for a Proxmox provider. The job skips the provider, so no traffic is metered.
- `prometheus` reads a Prometheus-compatible endpoint (Prometheus, Mimir, VictoriaMetrics or Thanos). Set the endpoint URL and the metric schema, plus headers, credentials or a custom CA if needed. The only schema today is `libvirt-exporter`, a legacy layout that doesn't match Proxmox VMs yet, so a Proxmox provider meters no traffic until a Proxmox schema is added.

Always run **Test** after you change the source. A misconfigured source doesn't fail loudly; ingestion stops and traffic goes unbilled. The same settings are available through `PUT /api/v1/admin/service/{id}/metrics-config`, `POST /api/v1/admin/service/{id}/metrics-test`, and the admin MCP tools `set_metrics_config` and `test_metrics_config`.

## After the provider is saved

- **Bootstrap.** When a billing profile is activated (or an admin provisions a project), ProxCloud bootstraps each project onto the provider. It creates the project's pool and default security group and seeds the default quota. See [Account Activation](/docs/platform-admin/account-activation).
- **Sync.** A periodic sync, every 15 minutes, keeps ProxCloud's cache of each project's servers, volumes, networks, security groups and templates in step with the cluster, so billing accrual reflects what exists. VMs outside the project pools are invisible to ProxCloud.
- **Services.** Every service is available by default. Adjust that on the provider's **Services** tab; see [Service Availability](/docs/platform-admin/cloud/service-availability).
- **Tabs.** A Proxmox provider has the Connection, Services, Configuration, Features, Quota, GPU, Metrics and Advanced tabs. The **Advanced** tab's Danger zone is where a provider is deleted.

## GPU capacity and quotas

GPUs are exposed through Proxmox PCI resource mappings. List each mapping under the `gpus` block with its model alias, e.g. `nvidia-a6000`. That alias is the shared vocabulary for pricing rules (`gpu_model`), flavors (System → Flavors (Proxmox)), this capacity view and project quotas.

The provider's **GPU** tab shows capacity per model: the devices in the mappings for that alias, and how many of them are attached to VMs. The same tab lists **unpriced flavors**, meaning flavors that match no enabled public price rule and would therefore bill zero. The dashboard shows the same capacity as a per-model usage bar.

### Per-project GPU quota

On a project's **Quota** tab, set per-model device limits (`nvidia-a6000 → 4`, or `*` as a catch-all). ProxCloud enforces them when servers are created or resized through the client portal or API. A request that would exceed the limit is rejected with a clear error. No entry means unlimited.
