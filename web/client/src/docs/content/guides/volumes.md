# Block Volumes and Snapshots

Block storage gives your servers extra disks beyond the root disk they boot from. The **Storage** group in the sidebar covers volumes alongside object storage buckets — the latter has [its own guide](/docs/guides/object-storage).

## Volumes

A **volume** (under **Storage → Volumes**) is a disk of one of your servers. On Proxmox VE a disk always belongs to a VM, so a volume is created on a server rather than as a free-standing disk: a create request carries a size in GB, a storage type, and the server it belongs to (`serverId`). The **Create volume** dialog does not ask for a server yet, so on a Proxmox provider create volumes through the API (`POST /api/v1/project/{id}/cloud` with type `VOLUME`). The storage types are the storage profiles your operator publishes for the region; their specs (shared, thin, snapshots, replication) are shown next to each name. If there is only one type, it is selected automatically. The volume starts out empty — format and mount it from inside the guest OS as you would any disk.

Once it exists, a volume's actions menu lets you:

- **Extend** it. Disks can only grow; Proxmox can't shrink a disk.
- **Detach** it. The disk stays with the server as an unused disk, keeps its data, and is still billed.
- **Attach** it again, to the same server or to another server in your project. Moving a disk to another server works when both servers are on the same node or the storage is shared.

Volumes are deleted together with their server. Delete the server only after you've copied off any data you still need.

## Snapshots

To capture a server, open its **Snapshots** tab and create a snapshot: ProxCloud clones the whole server into a project image that you can launch new servers from (see [Working with Servers](/docs/guides/servers)).

Per-volume snapshots are not available on Proxmox providers, so **Storage → Snapshots** stays empty there.
