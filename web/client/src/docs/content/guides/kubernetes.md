# Kubernetes clusters

**Platform → Kubernetes** lists your managed clusters. The control plane is hosted for you; the worker nodes are servers in your project, grouped in **node groups** (a flavor and a count each).

## Creating a cluster

The create form starts on the lowest Kubernetes version your operator offers, with **High availability** (three control-plane replicas instead of one) off. Pick a newer version or turn high availability on yourself; ask your operator which versions they have run on this site.

A first cluster needs two things from you: a **name** and, for its node group, a **flavor** (the size of each worker). The rest has a default:

- **Network.** The worker nodes run on one of your project's networks and get fixed addresses there, 24 of them reserved. When your project has exactly one network that can hold them, the form has it selected. When it has none, **Create a network for this cluster** makes one with the defaults of **Network → Networks** and selects it. A network that hands out addresses by DHCP cannot hold a cluster's nodes and is listed as not selectable; when every network of your project is such a network, a new one would be the same, and only your operator can change that.
- **Node group.** One group named `workers` with one node. Flavors that exceed your quota cannot be picked.
- **Add-ons, allowed API CIDRs and OIDC** are optional.

The network needs a way to the internet (the default of a new network), and the nodes must reach the cluster's own API address, which your operator provides.

## Status while a cluster is built

A new cluster reads **Pending**, then **Progressing**, then **Ready**. While it is being built, the cluster page shows a checklist: the control plane, one line per node group and the configuration sync. Each node group line says how many nodes are ready and what the platform is doing for the others:

| You read | Meaning | What you can do |
|---|---|---|
| Waiting for the control plane. | The workers start once the control plane is up. | Wait. |
| Preparing the machine. / Creating the virtual machine. / Starting the virtual machine. | The normal steps. With many nodes on slow disks this takes a while. | Wait. |
| Creating the virtual machine failed; retrying automatically. | A step failed and is tried again. | Wait. |
| The machine runs but has not joined the cluster yet. | The server is up and its node has not registered. In the first minutes this is a normal step. | Wait. |
| The machine runs but has not joined the cluster: most likely it could not reach the cluster's endpoint. | After 15 minutes with **Needs attention**: the server did not join and does not try again by itself. The likely cause is that it did not reach the cluster's endpoint. The path between your network and the endpoint is the operator's; you cannot check it, and the server's own error is shown to the operator only. | Contact the operator. Once the cause is repaired, use **Replace** on that server in the cluster's instance list ([Replace a node](#replace-a-node)). |
| The node joined but is not Ready. | The node registered and reports not Ready. | Check it with `kubectl describe node`. |
| The host has no free capacity for this machine, and it is not retried. | The host that should run the server is full. | Reduce the count or pick a smaller flavor. To retry, reduce the count by one and raise it again. |
| Creating the virtual machine failed and is not retried. | The platform gave up on this server. | Reduce the count by one and raise it again, or delete and recreate the cluster. |
| The platform cannot reach the virtualization provider. | A problem on the operator's side. | Contact the operator. |
| The control plane is taking longer than expected. | The hosted API server did not come up in 15 minutes. | Wait, delete and recreate, or contact the operator. |

## Replace a node

**Replace** deletes one server of the cluster and creates a new one in its place. The button is on every row of the cluster's instance list, and next to a node group's status for each server of the group that needs attention (such a server is not always in the instance list). The new server joins the cluster again. Use it for a server that did not join, after the operator repaired the cause, or for a node you want rebuilt.

- The node count of the group and your quota stay as they are, and the new server runs on the same host.
- A node that had joined is drained first. What is on its own disk is lost; persistent volumes are kept.
- For the last Ready node of a node group the console asks a second time: its pods have nowhere to run in that group until the new node is Ready, which takes several minutes.
- A machine whose server was never created has no row in the list: reduce the group's count by one and raise it again, or contact the operator.

On a site that has it turned on, the platform also replaces a server that did not join by itself, after about 30 minutes, in a node group of two or more servers. It does not when several servers of a group failed together: that points to a cause only the operator can repair.

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
