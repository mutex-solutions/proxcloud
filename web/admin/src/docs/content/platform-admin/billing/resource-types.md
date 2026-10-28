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
| `instance_traffic` | Proxmox VE servers, usage from the provider's metrics source | `incoming/outgoing_public_traffic_mb`, `incoming/outgoing_private_traffic_mb`, `total_public_traffic_mb`, `total_private_traffic_mb`, `total_traffic_mb` |
| `volume` | Proxmox VE volumes | existence, `size` (GB) |
| `bucket` | Ceph RGW (S3) buckets | existence, `size_gb`, `object_count` |
| `kubernetes_cluster` | Managed Kubernetes | existence, `cp_replicas`, `node_groups` |
| `database_cluster` | Managed databases | existence, `replicas`, `vcpus_total`, `ram_gb_total`, `storage_gb_total`, `autoscale_enabled` |
| `floating_ip` | No Proxmox resource produces this type yet | existence |
| `load_balancer` | No Proxmox resource produces this type yet | existence |

Traffic is only metered when the provider has a metrics source configured. A Proxmox provider defaults to `none`; see [Connecting a Proxmox VE Cluster](/docs/platform-admin/cloud/connect-a-cloud). Networks, subnets, security groups, key pairs and templates have no billing type: they are free plumbing. Billing for public IP addresses and images is planned.

## Deciding what to charge for

You don't need a rule for every type. In practice most operators bill for:

- instances, by vCPU, RAM and disk, plus a per-model GPU rule;
- volumes;
- object storage;
- managed clusters.

A resource type with no rule simply accrues nothing. The provider's **GPU** tab lists flavors that no enabled public rule prices, so a GPU flavor doesn't bill zero by accident.
