# Kubernetes clusters

**Platform → Kubernetes** lists your managed clusters. The control plane is hosted for you; the worker nodes are servers in your project, grouped in **node groups** (a flavor and a count each).

## Creating a cluster

The create form starts on the lowest Kubernetes version your operator offers, with **High availability** (three control-plane replicas instead of one) off. Pick a newer version or turn high availability on yourself; ask your operator which versions they have run on this site.

## Status while a cluster is built

A new cluster reads **Pending**, then **Progressing**, then **Ready**. While it is being built, the cluster page shows a checklist: the control plane, one line per node group and the configuration sync. Each node group line says how many nodes are ready and what the platform is doing for the others:

| You read | Meaning | What you can do |
|---|---|---|
| Waiting for the control plane. | The workers start once the control plane is up. | Wait. |
| Preparing the machine. / Creating the virtual machine. / Starting the virtual machine. | The normal steps. With many nodes on slow disks this takes a while. | Wait. |
| Creating the virtual machine failed; retrying automatically. | A step failed and is tried again. | Wait. |
| The machine runs but has not joined the cluster yet. | The server is up and its node has not registered. In the first minutes this is a normal step. | Wait. |
| The machine runs but has not joined the cluster: most likely it could not reach the cluster's endpoint. | After 15 minutes with **Needs attention**: the server did not join and does not try again. The likely cause is that it did not reach the cluster's endpoint; the console does not read the server's own error. The path between your network and the endpoint is the operator's; you cannot check it. | Contact the operator. Once the cause is repaired, reduce the group's count by one and raise it again: that replaces the server. A group cannot go to zero: with one server, raise the count by one, wait until the new server is ready, then reduce it by one. |
| The node joined but is not Ready. | The node registered and reports not Ready. | Check it with `kubectl describe node`. |
| The host has no free capacity for this machine, and it is not retried. | The host that should run the server is full. | Reduce the count or pick a smaller flavor. To retry, reduce the count by one and raise it again. |
| Creating the virtual machine failed and is not retried. | The platform gave up on this server. | Reduce the count by one and raise it again, or delete and recreate the cluster. |
| The platform cannot reach the virtualization provider. | A problem on the operator's side. | Contact the operator. |
| The control plane is taking longer than expected. | The hosted API server did not come up in 15 minutes. | Wait, delete and recreate, or contact the operator. |

## Needs attention

An amber **Needs attention** chip appears on the list and on the cluster page when a node group or the control plane made no progress for some time (15 to 90 minutes, depending on the step and on how many servers are built at once), or hit an error that is not retried. The **Status** column of the node group table shows which group it is.

Needs attention is not a final state. The cluster keeps its status, nothing is deleted, and the chip goes away by itself when the cluster recovers. You can wait, change the node group as its line says, delete and recreate the cluster, or contact the operator, who sees the full detail of every server.

While the platform works on a cluster the page refreshes itself every 15 seconds; a cluster that needs attention is refreshed once a minute.

## Node groups and hosts

The workers of a node group run on the hosts that hold the node image. When you create a cluster or raise a group's count, the platform puts each new worker on a host with enough free memory, CPU and disk, and spreads a group over several hosts when it can. Workers that already run stay where they are.

- If the new workers do not all fit, nothing is changed and you get a message such as *Node group "workers": only 2 of 5 new workers fit*. Reduce the count, pick a smaller flavor, or contact the operator.
- When you lower a count, a worker that never became ready is removed first; otherwise the host with the most workers of the group gives one up.
- Each worker's node carries the label `topology.kubernetes.io/zone` with its host, so you can spread or pin pods with it.
- On a site whose storage is local to each host, a volume lives on the host of the worker that created it. If you scale a group below the number of hosts it runs on, every worker on at least one host is removed, and pods that use a volume on that host cannot start elsewhere. The **Edit node groups** dialog warns you before you apply such a change.

## Firewall around the nodes

Your operator can put a cluster's nodes behind the platform firewall. The cluster page then shows a line **Worker firewall** with a security group named `k8s-<cluster id>`, which you also find under **Network → Security groups**.

With it, inbound traffic to a node is dropped unless a rule of that group allows it. The group starts with what the cluster needs between its own nodes (the pod network, health checks, the kubelet, MetalLB), your LoadBalancer addresses from anywhere, and NodePorts (30000 to 32767) from your project's networks. Outbound traffic is not filtered.

- The rules are yours to change. Add one when something outside the cluster must reach a port on a node directly, for example a `hostNetwork` service. Removing one of the rules the cluster started with can break the cluster.
- The kubelet port of a node is not reachable from your other servers.
- You cannot attach another security group to a node, detach or delete this one, attach it to a server, or create a group whose name starts with `k8s-`. The group is deleted with the cluster.
- A node that was just added or replaced gets the firewall within about 15 minutes.

A cluster page without that line has no firewall around its nodes: every port of a node is reachable from your project's own networks, and from nowhere else.
