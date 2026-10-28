# Turning Services On and Off

Not everything a cluster can run is something you want to sell. ProxCloud lets you switch service availability per region on each cloud provider. When you switch a service off, it stops appearing in the client portal and the sync job stops reconciling it.

## Where the toggles are

Open **System > Cloud providers**, select the provider, and go to the **Services** tab. Every service shows a toggle for each region. Toggles don't take effect until you press **Save**.

The toggles live in the provider document under `config.services[<slug>][<region>]`. A provider with no `services` map at all counts as having every service on.

## The services you can toggle

| Provider kind | Service slug | What clients get in the portal |
|---|---|---|
| `proxmox` | `compute` | Servers and key pairs |
| `proxmox` | `image` | Images (OS templates and project templates) |
| `proxmox` | `volumev3` | Volumes and snapshots |
| `proxmox` | `network` | Private networks and security groups |
| `ceph-s3` | `object-store` | Object storage buckets and S3 access keys |
| `kamaji` | `kubernetes` | Managed Kubernetes clusters |
| `dbaas` | `database` | Managed database clusters |

A new Proxmox provider starts with `compute`, `network`, `volumev3` and `image` enabled in its region. Each of the other kinds starts with its single service enabled.

## How far a toggle reaches

- **Per region.** Enabling a service in one region makes it available only to clients in that region.
- **Hiding a service everywhere.** A service stays visible in the portal as long as it's enabled in *any* region of *any* provider. To take a managed service (Kubernetes, Databases, Object storage) away from all clients without touching the providers, turn it off under **System > Platform > Managed services shown to tenants**. That only hides it in the portal: the providers keep syncing, and existing resources keep running and billing.

## What happens when you disable

- The matching section disappears from the client portal navigation for the affected regions.
- Clients can no longer create new resources of that type there.
- The periodic sync honours the toggle:
  - On a Proxmox provider, turning off `compute` stops the sync of all its resources: servers, volumes, templates, networks and security groups. The other three Proxmox toggles only change what the portal shows.
  - Turning off `object-store`, `kubernetes` or `database` stops the sync of buckets, Kubernetes clusters or database clusters.
- Resources that already exist are left alone and keep accruing charges until they're removed. Disabling only blocks new orders and hides the UI. It deletes nothing.
