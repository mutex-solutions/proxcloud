# Block Volumes and Snapshots

Block storage gives your servers extra disks beyond the root disk they boot from. The **Storage** group in the sidebar covers volumes alongside object storage buckets — the latter has [its own guide](/docs/guides/object-storage).

## Volumes

A **volume** (under **Storage → Volumes**) is a disk of one of your servers. On Proxmox VE a disk always belongs to a VM, so a volume is created on a server rather than as a free-standing disk: the **Create volume** dialog asks for a name, a size in GB, a storage type and the server the volume belongs to. The storage types are the storage profiles your operator publishes for the region; their specs (shared, thin, snapshots, replication) are shown next to each name. If there is only one type, it is selected automatically. The disk is created on the storage of the server's host, so if that storage is too full the dialog shows "Not enough free space" and nothing is created; pick a smaller size or another type. The volume starts out empty — format and mount it from inside the guest OS as you would any disk.

![The Create volume dialog with a capacity type and its warning](/docs-img/create-volume.png)

A type labelled **HDD, not for databases** (for example *Capacity HDD (NFS)*) is bulk storage on a shared HDD NAS. It suits large data that is read and written in big chunks. It never holds a server's boot disk or a managed database, and you cannot snapshot a server into an image while such a volume is attached (detach it first, and protect the data with backups instead). The volume moves with its server and survives a host failure, but the NAS itself is a single point of failure. Your operator may price it differently from the other types.

Once it exists, a volume's actions menu lets you:

- **Extend** it. Disks can only grow; Proxmox can't shrink a disk.
- **Detach** it. The disk stays with the server as an unused disk, keeps its data, and is still billed.
- **Attach** it again, to the same server or to another server in your project. Moving a disk to another server works only when both servers run on the same node; ProxCloud lists only the servers it can attach to and refuses the others with the reason.
- **Change type** (retype) it, which moves the disk to another storage type on its server's node. The disk keeps its data and the old copy is removed once the move succeeds. The action shows only when your operator publishes more than one storage type.

![The Volumes list with a boot disk and a data disk](/docs-img/volumes-list.png)
- **Delete** it once it is detached. An attached volume must be detached first.

A server's volumes, attached or detached, are deleted together with the server; the server's delete dialog warns about this. Delete the server only after you've copied off any data you still need.

## Snapshots

To capture a server, open its **Snapshots** tab and create a snapshot: ProxCloud clones the whole server into a project image that you can launch new servers from (see [Working with Servers](/docs/guides/servers)).

Per-volume snapshots are not available on Proxmox providers, so **Storage → Snapshots** is hidden there.
