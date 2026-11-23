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

A worker runs `kubeadm join` once, at its first boot. When that fails the VM keeps running and nothing retries. For a machine in `not_joined` that needs attention the dialog shows, under the machine, **Join error, as the machine reports it**: the errors of cloud-init's result and the part of its output log that holds the join: from the last `kubeadm join` to the end of the log, where kubeadm's own message is (for example `dial tcp <endpoint>:6443: i/o timeout`), without the SSH host keys the image prints after it. A long join is shortened to its first line, the lines that name an error and the last lines. ProxCloud reads the two files at that moment through the Proxmox guest agent, with the token of the Proxmox provider that hosts the workers, and stores nothing. Join tokens, CA hashes and other credentials are masked before the text leaves the API. The tenant never sees it: their page says to contact you. When nothing can be read the dialog says why (the token is refused, the guest agent does not answer). At most five machines are read per opening of the dialog.

**Replace** on a machine's row deletes that Machine after one confirmation; Cluster API then makes a new one on the same Proxmox node, and it joins again. The node group's count and the project's quota do not change. A machine that has a node is drained first. The last Ready node of a node group asks a second time, because its pods have nowhere to run in the group until the new node is Ready. A tenant has the same action on the cluster's instance list and next to the status of a node group that needs attention. If the answer says the service account cannot delete Machines, apply `deploy/mgmt-cluster/rbac.yaml` again (or move the managed-services platform to platform chart 0.1.3). Repair the cause first: a new machine fails the same way otherwise.

**Automatic repair**, above the table, says what Cluster API does by itself. From cluster chart 0.1.14 each node group of two or more machines has a health check: a machine with no node after the startup timeout (30 minutes unless you set `config.cluster.nodeStartupTimeoutSeconds` on the provider; `0` turns it off, and the save refuses a value under 900 seconds, which would delete machines that are still being cloned) is replaced, but only while at most `config.cluster.repairMaxUnhealthy` machines of the group (1 by default) are not healthy. The save also refuses a `repairMaxUnhealthy` that is not a whole number of 1 or more and a `workerFirewall` that is not `true` or `false`. A note above the provider's tabs says when a stored value is ignored, and when the two repair keys are set on a provider whose chart is older than 0.1.14, where they do nothing. Know its limits:

- When more machines of a group than that are without a node, and always when all of them are, nothing is replaced. That is on purpose: the cause is then the site's, and new clones would fail the same way for ever. The box says so; replace the machines here after the repair.
- A group of one machine has no health check.
- Machines that are still starting count as not healthy, so the repair waits while a group is being scaled up by several machines.
- A cause that keeps hitting one machine of a larger group replaces it again after every timeout. Nothing stops that but the repair of the cause, or a longer timeout or `0` on the provider followed by **Apply settings** on that cluster.
- It does not react to a node that joined and went NotReady, nor to a stopped VM.
- A cluster made on an older chart gets the health check when you **Bump** it onto 0.1.14 or later, and it then acts at once on machines already past the timeout.
- The provider's two keys are the setting of every cluster on chart 0.1.14 or later: ProxCloud writes them into a cluster at every change it makes to that cluster, over what the cluster had. After you change them, **Apply settings** on a cluster's row (shown when the cluster is already on the provider's chart) writes them to that cluster now. There is no per-cluster setting.

None of this was run on a lab yet. Cluster chart 0.1.14 or later (this release pins 0.1.19 for new clusters) and platform chart 0.1.3 (which lets the portal delete a Machine and read the health checks) come with a managed-services platform installed from this release. Until the provider's chart version is 0.1.14 no cluster has a health check, and on a managed-services platform installed from an earlier release Replace answers that the service account cannot delete Machines, until the platform is upgraded to this release.

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

The provider's ClusterRole on the management cluster predates this view. On an installer-built site re-apply `deploy/mgmt-cluster/rbac.yaml`. A platform installed by **Managed services** from release 0.8.0 or later has the access (platform chart `proxcloud-platform` 0.1.2 or later); one installed from an earlier release keeps the old ClusterRole (its `platform` Application reverts a hand-applied one) until Remove and a new install. Until then tenants see the status as before, without reasons.

## Worker firewall

**Not verified on a lab yet.** Protect one cluster at a time and check a workload after each.

A worker VM is cloned by the Proxmox provider of Cluster API, which cannot switch the Proxmox firewall on for it. Without the firewall every port of a worker, the kubelet included, is open to the project's own networks, and a worker can send with any address of its network. Other tenants are kept out by the isolation rules either way.

**Firewall** on a cluster's row shows whether the cluster is protected and, per worker VM, whether its firewall is in place.

**Protect this cluster's workers** puts every running worker behind the Proxmox VM firewall: inbound traffic is dropped except what the cluster's security group allows, outbound stays open, and a worker may only send from the cluster's own worker and load balancer addresses. The security group is an ordinary group of the tenant's project named `k8s-<cluster id>`. It opens, between the cluster's workers, the pod network (UDP 8472), Cilium health (TCP 4240, ICMP), the kubelet (TCP 10250) and MetalLB (7946); from anywhere, the cluster's LoadBalancer addresses; from the project's networks, NodePorts (30000 to 32767). The tenant may change its rules.

The action refuses, and changes nothing, when the datacenter firewall is off or when a node's address is not one of the addresses reserved for the cluster. After writing, it watches for up to a minute that every node that was Ready still reports to its API server. If one does not, it takes the firewall off again and says so. The request answers only then.

What it does not check: traffic between pods on different nodes, and LoadBalancer Services. Look at a workload afterwards.

**Remove the worker firewall** switches the firewall off on every worker and deletes the group. It refuses, and changes nothing, while a worker VM is on a Proxmox node that is down: try again when the node is back or the worker has been replaced.

If Protect is cut short (the page shows an error after a minute because a proxy gave up, or the API restarted), open **Firewall** again to see the state. A firewall that stayed on without the check is not taken off by anything: look at the cluster's nodes, and use **Remove the worker firewall** if one is not Ready.

Things to know:

- **A new or replaced worker is open until the next provider sync**, up to 15 minutes. The sync brings every running worker of a protected cluster behind the firewall. Start a sync by hand to close the gap at once.
- **New clusters** are protected from their creation only with `config.cluster.workerFirewall: true` on the Kubernetes provider (API only, off by default). Existing clusters are never changed by an upgrade or by the sync.
- **Tenants cannot attach their own security group to a worker**, detach the cluster's, delete it, attach it to a server, or create a group whose name starts with `k8s-`. A group a tenant named `k8s-…` before 0.9.0 stays theirs unless the rest of the name is the id of one of their clusters.
- Not offered with shared workers.

The full table, the reasoning and the lab tests still owed are in `docs/managed-k8s.md`, "Worker firewall".

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

## Load balancer

**Not verified on a lab yet.** A Service of type `LoadBalancer` in a tenant cluster can get a public address of the platform's load balancer: two appliance VMs per cluster, built with the cluster's first such Service and kept until the cluster is deleted, one public address per Service. The tenant pays both.

On the provider's **Connection** tab, **Load balancer public network** and **Load balancer flavor id** set `config.cluster.loadBalancer`: a public network of the workers' Proxmox provider (a plain bridge with a gateway, not an SDN vnet) and an active flavor for the appliances. Both or neither; the save checks them. Empty: every cluster keeps MetalLB only.

A **new** cluster gets the load balancer controller in default mode when every gate holds: the portal has the gateway URL and the controller image (`PROXCLOUD_LB_CONTROLLER_IMAGE`), the provider pins cluster chart 0.1.17 or later, the workers are the project's own, and the provider's public network is allowed for the project. A Service without a class then gets a public address, and MetalLB serves class `proxcloud.io/internal`. Otherwise the cluster is made as before and the API log names the gate.

The **Load balancer** column of the clusters list shows each cluster's mode: Default, Class only or MetalLB only. The **Load balancer** button opens the cluster's view: when its controller last called the gateway and which protocol it speaks, the balancer's status, its appliances and its Services with their addresses.

### UDP, TLS and the controller of an earlier release

From v0.11.1 a cluster's Services can have UDP ports, end TLS with a certificate from a Secret, set options with `proxcloud.io/lb-*` annotations and keep an address. All of it is the work of the cluster's controller and the portal together, and **a cluster keeps the controller it was made with**:

- **UDP and TLS from Services need the cluster on chart 0.1.19.** The upgrade to v0.11.1 writes chart 0.1.19 on the provider (a record-only step: no cluster, VM or balancer is touched), so clusters created afterwards have everything. A cluster made by v0.11.0 keeps its controller of protocol 1: its UDP Services stay refused with `UDPNotSupported`, and none of its annotations but `proxcloud.io/lb-proxy-protocol` is read. That is deliberate: a Service that stood pending would otherwise get a public address, which is billed and counted in the tenant's quota, by your upgrade alone.
- **The dialog names it.** "It speaks protocol 1 of 2" with a note is a controller of an earlier release. **Bump** the cluster to the provider's chart on its row, or **Rotate the controller's credential**: either sets the controller's image to the portal's. The tenant can do the same with the platform update on the cluster's page, which says the same.
- **What a bump to 0.1.19 changes on the cluster:** the controller's image and one argument (`--watch=true`: it then learns of a changed Service, node or TLS Secret within seconds instead of every 10 s). No machine is replaced and the balancer's configuration does not change. The chart needs the controller image of v0.11.1 or later: do not bump or re-enable a cluster while the portal is rolled back to v0.11.0.
- **Health checks of UDP ports.** A worker that is down leaves in about 10 seconds. A port nothing listens on is often not seen under traffic, and a worker whose firewall drops in silence is never seen: the check is a datagram of random bytes and a ping. With `externalTrafficPolicy: Local` the check is the Service's health port, for UDP too.
- **Where a tenant's TLS key rests.** Sealed with the portal's encryption key in the portal's database, in a row of the cluster's balancer that no project lists, and in a root-only file on both appliances. Whoever holds a portal backup holds the sealed keys and, next to them, the key that opens them; a Proxmox backup, snapshot or clone of an appliance holds the file in clear. Keep both as you keep the provider secrets, and keep appliances out of backup jobs that leave the site. A portal without an encryption key takes no certificate (`TLSNotAvailable`).
- **Not the upstream controller's names.** Only `proxcloud.io/lb-*` annotations are read. A manifest written for the upstream cloud controller manager is renamed first; the tenant guide and `docs/managed-k8s.md` list the names and the seventeen places where a Service behaves differently on purpose.

- **Enable load balancer controller** gives an **existing** cluster the controller in class-only mode: only Services of class `proxcloud.io/lb` get a public address, and no MetalLB address moves. The cluster is pinned to the provider's chart version in the same apply, as a bump does. A gate that does not hold is refused with its name.
- On a cluster that has a controller the button **Rotate the controller's credential** mints a new one (the old token stops at once) and gives the cluster the portal's controller image and the provider's chart version, as a bump does. The mode stays.

The tenant's cluster page says which Services get a public address, what it costs, and that NGINX Gateway Fabric takes one public address per Gateway in default mode. The full rules are in `docs/managed-k8s.md`, "Load balancers of a cluster's Services".

