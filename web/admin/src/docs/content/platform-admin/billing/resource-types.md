# Billable Resource Types

Every billable object ProxCloud tracks belongs to a resource type, and each resource type exposes a handful of attributes. A pricing rule (see [Price Plans](/docs/platform-admin/billing/price-plans)) always prices one attribute of one resource type — so this catalog is the definition of what you *can* put a price on.

## How usage turns into charges

ProxCloud continuously syncs resources out of your Proxmox VE clusters into its own cache. On each billing tick it rates every cached resource: the rule's price is multiplied by the current value of the priced attribute and added to the owning billing profile's bill. A resource therefore costs money for precisely as long as it exists — delete it and accrual stops at the next tick.

There are two kinds of attribute:

- **`existence`** — a boolean every resource type carries. Priced as 0 or 1, it produces a flat "per resource, per time unit" charge. For the simplest types (networks, routers, floating IPs) it's the only thing you can bill.
- **Numeric attributes** — capacity and count metrics (RAM, vCPUs, disk GB, size, rule count, and so on). These enable proportional pricing: a bigger resource costs more, with no need for a rule per flavor.

## The catalog

Which types show up in your rule editor depends on the services enabled on your cloud providers (**System → Cloud providers**) — a region with no load balancing contributes no load balancer resources.

### Compute

| Resource type | Billable attributes |
|---|---|
| Instance | existence, RAM (MB/GB), vCPUs, disk (GB), network traffic counters (incoming/outgoing, public/private, totals) |

### Storage

| Resource type | Billable attributes |
|---|---|
| Volume | existence, size (GB) |
| Volume snapshot | existence, size (GB) |
| Volume backup | existence, size (GB) |
| Image | existence, size (MB/GB) |
| Object storage bucket | existence, size (MB/GB) |
| Shared file system | existence, size (GiB) |
| Share snapshot | existence |

### Network

| Resource type | Billable attributes |
|---|---|
| Network | existence |
| Router | existence |
| Subnet | existence, IP version |
| Port | existence |
| Floating IP | existence |
| Security group | existence, rule count |
| Load balancer | existence |

## Deciding what to charge for

You don't need a rule for every type. In practice most operators bill for instances (by vCPU/RAM/disk), volumes and their snapshots, floating IPs, load balancers, images, and object storage — and leave the free plumbing (networks, subnets, ports, security groups) unpriced. A resource type with no rule simply accrues nothing.
