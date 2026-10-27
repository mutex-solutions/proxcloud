# Turning Services On and Off

No OpenStack deployment runs every service, and not everything you run is something you want to sell. ProxCloud lets you flip service availability per region on each cloud provider — a service you switch off simply stops appearing in the client portal.

## Where the toggles are

Open **System > Cloud providers**, select the provider, and move to the **Services** tab. Every service shows a toggle for each region. Toggles don't take effect until you press **Save**.

## The services you can toggle

| Service | OpenStack component | What clients get in the portal |
|---|---|---|
| Compute | Nova | Servers — create, list, console |
| Block storage | Cinder | Volumes and snapshots |
| Networking | Neutron | Networks, routers, floating IPs, security groups |
| Load balancer | Octavia | Load balancers |
| Shared filesystems | Manila | File shares |
| Object storage | Swift API (Ceph RGW) | Buckets / containers |
| DNS | Designate | DNS zones |

On OpenStack 2026.1 (kolla-ansible), Octavia, Manila, and Ceph RGW-backed object storage work out of the box — but only switch them on in regions where the backing service is genuinely deployed.

## How far a toggle reaches

- **Per region** — enabling a service in one region makes it orderable only for clients in that region.
- **Hiding a service everywhere** — a service remains visible in the portal as long as it's enabled in *any* region of *any* provider. To take it away from all clients, disable it in every region across every cloud provider.

## What happens when you disable

- The matching section disappears from the client portal navigation for the affected regions.
- Clients can no longer create new resources of that type there.
- Resources that already exist are left alone and keep accruing charges until they're removed. Disabling only blocks new orders and hides the UI — it deletes nothing.
