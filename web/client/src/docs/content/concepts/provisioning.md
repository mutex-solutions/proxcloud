# How Provisioning Works

ProxCloud is a self-service and billing layer that sits in front of a Proxmox VE cluster. When you create a server, volume, or network in the portal, ProxCloud provisions it on the real cluster underneath and then keeps its own view of that resource in step with reality. This page explains that machinery — useful when you're wondering why a status changed, or how the console stays current.

## Projects map onto the cluster

Each ProxCloud project is backed by its own resource pool on the Proxmox VE cluster. ProxCloud manages that pool using its own **service credentials** (a PVE API token) — you don't need Proxmox accounts of your own for the platform to work. When you act in the portal, ProxCloud translates the request into Proxmox VE API calls made with those credentials, so the resource is created in the pool that belongs to your project.

## Keeping the console in sync

The state you see on a resource — a server starting, a volume attaching — reflects the real cluster:

- **Periodic sync.** A background job regularly polls the Proxmox VE API and reconciles ProxCloud's cached view against it, catching anything that drifted and reconstructing state after any gap. This is the source of truth.
- **No event feed.** ProxCloud syncs by polling the Proxmox VE API; there is no notification listener to set up, so a change made directly in Proxmox shows up after the next sync pass.

If a sync pass is missed, the next one reconciles the difference. You lose freshness in that window, never correctness. This is also why a change made directly in Proxmox — stopping or deleting a VM from the PVE web UI — still shows up in the portal.

## Activation and lifecycle

Provisioning is gated on an active account: because every resource meters into a bill, the platform won't provision billable resources until billing is activated (see [How Metering and Billing Work](/docs/concepts/billing-and-metering)). On a user's first sign-in ProxCloud creates their platform record and runs any sign-up or activation logic; from there the create, sync, and delete lifecycle applies to every resource type the platform mirrors.
