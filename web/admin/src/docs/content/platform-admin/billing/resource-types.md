# Billable Resource Types

Every billable object ProxCloud tracks belongs to a resource type, and each resource type exposes a handful of attributes. A pricing rule (see [Price Plans](/docs/platform-admin/billing/price-plans)) always prices one attribute of one resource type — so this catalog is the definition of what you *can* put a price on.

## How usage turns into charges

ProxCloud continuously syncs resources out of your Proxmox VE clusters into its own cache. On each billing tick it rates every cached resource: the rule's price is multiplied by the current value of the priced attribute and added to the owning billing profile's bill. A resource therefore costs money for precisely as long as it exists — delete it and accrual stops at the next tick.

There are two kinds of attribute:

- **`existence`**: a boolean every resource type carries. Priced as 0 or 1, it produces a flat "per resource, per time unit" charge.
- **Numeric attributes**: capacity, count and usage metrics (RAM, vCPUs, disk GB, GPU count, size, object count, traffic, and so on). These enable proportional pricing: a bigger resource costs more, with no need for a rule per flavor.

String attributes such as `gpu_model`, `region` or `status` can't be priced, but a rule can filter on them. For example, a GPU rule can filter `gpu_model eq nvidia-4090` and price `gpu_count`.

## The catalog

Which types produce charges depends on the services enabled on your cloud providers (**System → Cloud providers**). A region with no object store, for example, contributes no buckets.

| Resource type | Comes from | Billable attributes |
|---|---|---|
| `instance` | Proxmox VE servers | existence, `vcpus`, `ram_mb`, `ram_gb`, `root_disk_gb`, `gpu_count` (filter on `gpu_model`, `instance_type`, `status`, …) |
| `instance_traffic` | Proxmox VE servers, usage from the provider's metrics source (a rule on it must be `OVERWRITE_TOTAL`, by the hour or minute) | `incoming/outgoing_public_traffic_mb`, `incoming/outgoing_private_traffic_mb`, `total_public_traffic_mb`, `total_private_traffic_mb`, `total_traffic_mb` |
| `volume` | Proxmox VE volumes | existence, `size` (GB) |
| `share` | TrueNAS NFS and SMB file shares | existence, `size_gb` (used), `quota_gb` (allocated), `protocol` |
| `bucket` | S3 buckets (Ceph RGW, RustFS) | existence, `size_gb` (used), `quota_gb` (allocated: the bucket quota, or the used size without one), `object_count`. Price `size_gb` or `quota_gb` to choose the billing basis |
| `kubernetes_cluster` | Managed Kubernetes | existence, `cp_replicas`, `node_groups` |
| `database_cluster` | Managed databases | existence, `replicas`, `vcpus_total`, `ram_gb_total`, `storage_gb_total`, `autoscale_enabled`, `read_endpoint_lb` (1 when a PostgreSQL read-only endpoint runs its own load balancer) |
| `floating_ip` | Public IPs allocated to Proxmox VE servers | existence |
| `image` | Project templates (images saved from a server) | `size_gb` |
| `server_snapshot` | Disk snapshots of Proxmox VE servers, one line per snapshot | `size_gb` (the server's provisioned disks) |
| `server_protection` | The Protection add-on of a server: the replica of its local disks | `size_gb`, `state` |
| `sandbox` | Sandboxes, per minute while running (price it with `minute` rules) | `vcpus`, `ram_gb`, `gpu_count` (filter on `gpu_model`, `template`) |
| `sandbox_snapshot` | A sandbox's stored filesystem (pause or snapshot) | `size_gb`, `kind` |

A Proxmox provider meters the traffic of public NICs out of the box; see [Connecting a Proxmox VE Cluster](/docs/platform-admin/cloud/connect-a-cloud). Networks, subnets, security groups, key pairs and catalog templates have no billing type: they are free plumbing.

Charges follow the cluster within a minute, including changes made outside the portal. An hourly rule bills every clock hour a resource existed in: created at 10:20 and deleted at 12:50 is three hours. If a cluster stops answering, billing carries on from the last known state for 30 minutes, then stops for that provider's resources until it answers again, and that gap is never back-billed. Look for the `METERING_STALE` event in the audit log.

## Deciding what to charge for

You don't need a rule for every type. In practice most operators bill for:

- instances, by vCPU, RAM and disk, plus a per-model GPU rule;
- volumes;
- object storage;
- managed clusters.

A resource type with no rule simply accrues nothing. The provider's **GPU** tab lists flavors that no enabled public rule prices, so a GPU flavor doesn't bill zero by accident.
