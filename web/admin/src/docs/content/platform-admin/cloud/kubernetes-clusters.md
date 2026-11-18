# Managed Kubernetes clusters

A Kubernetes provider's page (**System → Cloud providers**, the provider, **Connection**) lists every cluster it manages under **Managed clusters**, with the platform (chart) version each one is pinned to.

## Why a cluster is not there yet

A tenant sees a short reason per node group while a cluster is built, and an amber **Needs attention** chip when a node group or the control plane made no progress for a bounded time or hit an error that is not retried. The tenant gets a fixed text per reason code and never the provider's own message, which names nodes, VMIDs and storage.

**Machines** on a cluster's row shows you what is behind it: every worker Machine, read live from the management cluster, with

- its phase, its VMID and Proxmox node, and its addresses;
- the same reason code the tenant gets, and whether it needs attention;
- every condition that reports a finding, with the reason and message Cluster API and the Proxmox provider (CAPMOX) report: one that is not True, or for `Paused`, `Deleting`, `Updating`, `ScalingUp`, `ScalingDown`, `RollingOut` and `Remediating` (False when all is well) one that is True. Credentials are cut out of the messages.

A box above the table appears when the cluster's access to Proxmox itself is refused or unreachable, or the cluster is paused.

### A worker that did not join

A worker runs `kubeadm join` once, at its first boot. When that fails the VM keeps running and nothing retries. For a machine in `not_joined` that needs attention the dialog shows, under the machine, **Join error, as the machine reports it**: the errors of cloud-init's result and the end of its output log, where kubeadm's own message is (for example `dial tcp <endpoint>:6443: i/o timeout`). ProxCloud reads the two files at that moment through the Proxmox guest agent, with the token of the Proxmox provider that hosts the workers, and stores nothing. Join tokens, CA hashes and other credentials are masked before the text leaves the API. The tenant never sees it: their page says to contact you. When nothing can be read the dialog says why (the token is refused, the guest agent does not answer). At most five machines are read per opening of the dialog.

**Replace** on a machine's row deletes that Machine after one confirmation; Cluster API then makes a new one on the same Proxmox node, and it joins again. The node group's count and the project's quota do not change. A machine that has a node is drained first. The last Ready node of a node group asks a second time, because its pods have nowhere to run in the group until the new node is Ready. A tenant has the same action on the cluster's instance list. Repair the cause first: a new machine fails the same way otherwise.

**Automatic repair**, above the table, says what Cluster API does by itself. From cluster chart 0.1.14 each node group of two or more machines has a health check: a machine with no node after the startup timeout (30 minutes unless you set `config.cluster.nodeStartupTimeoutSeconds` on the provider; `0` turns it off) is replaced, but only while at most `config.cluster.repairMaxUnhealthy` machines of the group (1 by default) are not healthy. Know its limits:

- When more machines of a group than that are without a node, and always when all of them are, nothing is replaced. That is on purpose: the cause is then the site's, and new clones would fail the same way for ever. The box says so; replace the machines here after the repair.
- A group of one machine has no health check.
- Machines that are still starting count as not healthy, so the repair waits while a group is being scaled up by several machines.
- A cause that keeps hitting one machine of a larger group replaces it again after every timeout. Nothing stops that but the repair of the cause or the timeout set to `0`.
- It does not react to a node that joined and went NotReady, nor to a stopped VM.
- A cluster made on an older chart gets the health check when you **Bump** it onto 0.1.14 or later, and it then acts at once on machines already past the timeout.

None of this was run on a lab yet. Cluster chart 0.1.14 and platform chart 0.1.3 (which lets the portal delete a Machine and read the health checks) are not published at the time of writing: until the provider's chart version is 0.1.14 no cluster has a health check, and on a managed-services platform Replace answers that the service account cannot delete Machines.

| Code | Usual cause | What to do |
|---|---|---|
| `not_joined` | The VM runs and its node never registered. The code does not read why; the cause found so far is that the VM could not reach the API endpoint when it joined (`kubeadm join` timed out on the endpoint's address; it runs once). Then either the cluster's network has no router (a VLAN zone needs yours), or the Proxmox node drops the connection: a node that translates tenant traffic and also runs the platform machine with the endpoint address, without routing the platform network itself. The **Check** of Managed services warns of that layout (Tenant path). After the bound the tenant is told to contact you. | Read the join error under the machine. Repair the path ([Managed services](/docs/platform-admin/managed-services), "Tenant path"); the worker does not join by itself afterwards: **Replace** it here, or the tenant does on the cluster's page. In a group of two or more with one such machine, Cluster API replaces it by itself after the startup timeout (cluster chart 0.1.14 and later). |
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
- memory taken on a node is the larger of the configured memory of the running VMs ProxCloud's token can see and the memory the node itself reports as used, plus the VMs that do not run, so VMs it cannot see (the platform's own machines on a managed-services install) still take room. Their CPU is not counted. The node's figure includes the host and the ZFS ARC, so a node can read less free than the sum of its VMs says;
- running workers never move to another node; a scale-down removes a stuck worker first and empties a node last. Which worker of a node is removed is Cluster API's choice, and it can be the older one;
- workers that are decided and not cloned yet count, with their memory, CPU and root disk, so two creates in a row do not take the same free node or overfill a node's storage;
- a request that does not fit is refused with how many workers fit, and nothing is created.

Things to know:

- **Existing clusters keep the even split** until you set the provider's chart version to 0.1.13 and **Bump** each cluster. After the bump nothing moves until the cluster's node groups change.
- **A version published on fewer nodes moves workers.** At a cluster's next upgrade or node group edit, the workers on a node that no longer holds the image are deleted at once and rebuilt on the other nodes. Room for them is checked first.
- **A template that is gone.** A node whose template was removed gets no new worker even while **Kubernetes versions** still names it; when no node holds the image the request is refused with *no node still holds this version's image*. Correct the versions line.
- **One storage name per cluster.** A node where the cluster's worker storage is not active gets no worker.
- **Local volumes are not followed.** A tenant who scales a group below the number of nodes it runs on is warned in the console; ProxCloud does not read PersistentVolumes.
