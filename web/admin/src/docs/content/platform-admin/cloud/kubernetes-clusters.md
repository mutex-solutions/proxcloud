# Managed Kubernetes clusters

A Kubernetes provider's page (**System → Cloud providers**, the provider, **Connection**) lists every cluster it manages under **Managed clusters**, with the platform (chart) version each one is pinned to.

## Why a cluster is not there yet

A tenant sees a short reason per node group while a cluster is built, and an amber **Needs attention** chip when a node group or the control plane made no progress for a bounded time or hit an error that is not retried. The tenant gets a fixed text per reason code and never the provider's own message, which names nodes, VMIDs and storage.

**Machines** on a cluster's row shows you what is behind it: every worker Machine, read live from the management cluster, with

- its phase, its VMID and Proxmox node, and its addresses;
- the same reason code the tenant gets, and whether it needs attention;
- every condition that reports a finding, with the reason and message Cluster API and the Proxmox provider (CAPMOX) report: one that is not True, or for `Paused`, `Deleting`, `Updating`, `ScalingUp`, `ScalingDown`, `RollingOut` and `Remediating` (False when all is well) one that is True. Credentials are cut out of the messages.

A box above the table appears when the cluster's access to Proxmox itself is refused or unreachable, or the cluster is paused.

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

The provider's ClusterRole on the management cluster predates this view. On an installer-built site re-apply `deploy/mgmt-cluster/rbac.yaml`. A platform installed by **Managed services** from this release has the access (platform chart `proxcloud-platform` 0.1.2); one installed from an earlier release keeps the old ClusterRole (its `platform` Application reverts a hand-applied one) until Remove and a new install. Until then tenants see the status as before, without reasons.

## Where a node group's workers go

A node image exists per Proxmox node (the provider's **Kubernetes versions**, `node=vmid,node=vmid`), and a worker is cloned on the node of its template. On a site without shared storage, publish the image on every node that should run workers: **Template updates** has **Publish on all nodes** for that.

From cluster chart 0.1.13 ProxCloud decides how many workers of a group each node runs, instead of the chart's even split:

- a new worker goes to a node that holds the image (the template is still on that node), has the cluster's storage with room for the disk (your storage headroom), stays under your CPU overcommit and has the memory free;
- among those, the node with the fewest workers of that group, then the most free memory;
- memory taken on a node is the larger of the configured memory of the VMs ProxCloud's token can see (running or stopped) and the memory the node itself reports as used, so VMs it cannot see (the platform's own machines on a managed-services install) still take room. Their CPU is not counted;
- running workers never move to another node; a scale-down removes a stuck worker first and empties a node last. Which worker of a node is removed is Cluster API's choice, and it can be the older one;
- workers that are decided and not cloned yet count, with their memory, CPU and root disk, so two creates in a row do not take the same free node or overfill a node's storage;
- a request that does not fit is refused with how many workers fit, and nothing is created.

Things to know:

- **Existing clusters keep the even split** until you set the provider's chart version to 0.1.13 and **Bump** each cluster. After the bump nothing moves until the cluster's node groups change.
- **A version published on fewer nodes moves workers.** At a cluster's next upgrade or node group edit, the workers on a node that no longer holds the image are deleted at once and rebuilt on the other nodes. Room for them is checked first.
- **A template that is gone.** A node whose template was removed gets no new worker even while **Kubernetes versions** still names it; when no node holds the image the request is refused with *no node still holds this version's image*. Correct the versions line.
- **One storage name per cluster.** A node where the cluster's worker storage is not active gets no worker.
- **Local volumes are not followed.** A tenant who scales a group below the number of nodes it runs on is warned in the console; ProxCloud does not read PersistentVolumes.
