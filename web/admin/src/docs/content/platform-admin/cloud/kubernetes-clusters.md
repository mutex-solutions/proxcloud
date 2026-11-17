# Managed Kubernetes clusters

A Kubernetes provider's page (**System → Cloud providers**, the provider, **Connection**) lists every cluster it manages under **Managed clusters**, with the platform (chart) version each one is pinned to.

## Why a cluster is not there yet

A tenant sees a short reason per node group while a cluster is built, and an amber **Needs attention** chip when a node group or the control plane made no progress for a bounded time or hit an error that is not retried. The tenant gets a fixed text per reason code and never the provider's own message, which names nodes, VMIDs and storage.

**Machines** on a cluster's row shows you what is behind it: every worker Machine, read live from the management cluster, with

- its phase, its VMID and Proxmox node, and its addresses;
- the same reason code the tenant gets, and whether it needs attention;
- every condition that is not True, with the reason and message Cluster API and the Proxmox provider (CAPMOX) report. Credentials are cut out of the messages.

A box above the table appears when the cluster's access to Proxmox itself is refused or unreachable.

| Code | Usual cause | What to do |
|---|---|---|
| `not_joined` | The VM runs and its kubelet cannot reach the API endpoint: the cluster's network has no router or gateway. | Fix the network with the tenant; the tenant then reduces the count by one and raises it again. |
| `no_capacity` | CAPMOX could not reserve memory on the node that holds the image. It does not retry. | Free capacity or publish the image on more nodes; the tenant scales the group down by one and up again. |
| `vm_create_failed` | A clone or a Proxmox task failed for good (`VMProvisionFailed`, `TaskFailed`). | Read the message, correct the cause; the machine is replaced when the tenant scales down by one and up again. |
| `vm_create_retrying` | A clone or power-on failed and is retried. | Nothing, unless it stays: then the message says what Proxmox refuses. |
| `provider_access_refused` | Proxmox refuses the cluster's token, or CAPMOX never opened the Proxmox API. | Check the token and that the management cluster reaches the Proxmox API. |
| `control_plane_stalled` | The hosted control plane did not come up in 15 minutes. | `kubectl get tcp -n pc-<projectId>` on the management cluster. |

Needs attention clears by itself when the cluster recovers; nothing is deleted or retried on its account.

## If the dialog says the service account cannot read Machines

The provider's ClusterRole on the management cluster predates this view. Re-apply `deploy/mgmt-cluster/rbac.yaml` (an install made by **Managed services** gets it with platform chart `proxcloud-platform` 0.1.2). Until then tenants see the status as before, without reasons.
