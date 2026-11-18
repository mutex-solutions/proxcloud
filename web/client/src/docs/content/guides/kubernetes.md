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
| The machine runs but has not joined the cluster. | The server is up, and its node never registered. Most often the cluster's network has no router or gateway, or it cannot reach the cluster endpoint. | Check the network, then reduce the group's count by one and raise it again. |
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
