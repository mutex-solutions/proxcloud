# Block Volumes and Snapshots

Block storage gives your servers persistent disks that outlive any single boot. The **Storage** group in the sidebar covers block volumes and their snapshots, alongside file shares (mountable shared file systems) and object storage buckets — the latter has [its own guide](/docs/guides/object-storage).

## Volumes

A **volume** (under **Storage → Volumes**) is a block device you attach to a server. Every newly created server also boots from a root block volume. That root volume is deleted with its server, while additional data volumes are independent of the server lifecycle and can be reattached elsewhere.

Create a standalone data volume with **Create volume** — give it a name, a size in GB, and a storage class. Only classes enabled by the operator for the selected region are available. If there is only one class, it is selected automatically. The volume starts out empty.

Once it exists, a volume's actions menu lets you **attach it to a server**, **extend** it (grow the disk), **change its type**, or delete it. After attaching, format and mount it from inside the guest OS as you would any disk.

## Snapshots

A **snapshot** (under **Storage → Snapshots**) is a point-in-time copy of a volume. Take one before a risky change so you can roll back, or use it as the basis for a new volume. Snapshots capture the volume as it stands at the moment you create them, so quiesce or unmount the volume first if you need a fully consistent image.

## File shares

For storage that several servers mount at once, **Storage → File shares** provides shared file systems rather than single-attach block disks — useful when multiple machines need to read and write the same files concurrently.
