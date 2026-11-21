# Load balancers

A load balancer gives your servers one public address and spreads traffic across them. Servers that stop answering are left out until they answer again.

## What you pay for

A load balancer runs on its own small VMs in your project: two with high availability (the default), one without. Each costs what a server of that size costs, plus its disk, and the load balancer adds its public address and the traffic on that address. It counts in your quota the same way: two servers and one public address with high availability. The VMs do not appear under **Servers**; the load balancer's page lists them. You will see them, their disks and the address on your bill.

## Create one

Under **Network → Load balancers**, press **Create load balancer** and pick a name, the network your servers are on, a public network and a size. The network must be one whose addresses the platform assigns, which is the usual kind. **High availability** is on unless you turn it off.

A load balancer needs three things from the site that only your operator can provide: a public network allowed for your project, a free public address in your quota, and the image the load balancer runs on. When one is missing, the page says which and **Create load balancer** is disabled, so you do not find out by a failed create. File shares have a similar notice when your project's networks cannot reach the file server.

The status reads `build` for a few minutes while the VMs boot and install what they need, then `active`.

## High availability

With high availability the load balancer runs on two VMs, called appliances. One holds the public address and carries the traffic; the other waits. When the active one fails, stops, or its HAProxy stops, the other takes the address over within a few seconds (about 4 seconds when a VM dies, under a second when one is stopped on purpose). Connections open at that moment are cut, and clients connect again.

The page lists both appliances, the node each runs on, and which one holds the address. The status reads:

- `active`: both answer and one holds the address.
- `degraded`: only one answers, or both hold the address because they cannot hear each other. The page says which. The load balancer keeps serving.
- `down`: neither answers.

Where both appliances run on the same node, which happens on a site with one node and on some networks, the page says so: the load balancer then survives the failure of an appliance, not the loss of the node.

A load balancer made before high availability existed, or with it turned off, runs on one VM. It cannot be changed into a pair: create a new load balancer (it gets a new address) and delete the old one.

### Replace appliances

**Replace appliances** builds both VMs again from the current image, one at a time, the standby first, so the address keeps answering. Traffic switches once, in about a second. Use it to get updated software, or to rebuild an appliance that is gone. It takes a few minutes per appliance, and the load balancer cannot be deleted while it runs.

## A Kubernetes cluster's load balancer

A load balancer named `k8s-…` whose row names a Kubernetes cluster belongs to that cluster: its Services of type `LoadBalancer` make it ([Kubernetes clusters](/docs/guides/kubernetes)). Its page lists those Services with their addresses and links to the cluster. It has no listeners, pools or **Delete** for you to change: change the cluster's Services instead, and it is deleted with the cluster. **Replace appliances** works as on any load balancer.

## Add listeners, pools and servers

- A **pool** is a set of your servers. Choose HTTP or TCP, and how to spread traffic: round robin, least connections, or by source address.
- Each server in a pool has a **port** and a **weight**. A server with twice the weight gets twice the traffic.
- A **listener** opens a port on the public address and forwards it to a pool. Under **Allowed sources** you can list the networks that may connect, such as `203.0.113.0/24`; empty means any address. Other clients are dropped before they reach the load balancer.
- **PROXY protocol v2** on a pool passes the client's address to your servers in a PROXY header. Turn it on only when your servers expect that header.

Press **Save configuration**. The change reaches the load balancer within seconds. If HAProxy refuses it, the page shows why and the load balancer keeps running the last configuration that worked.

Only servers of this project, on the load balancer's own network and subnet, can be added. HTTP pools add an `X-Forwarded-For` header with the client's address. To serve HTTPS, use a TCP pool on port 443 and terminate TLS on your servers.

## Let the traffic in

The load balancer reaches your servers from its private addresses (one per VM; a replace gives new ones). The `default` security group already allows that. If a server uses other groups, open its port to the load balancer's network.

## Health

Every server is checked every 3 seconds, by default with a TCP connection to its port. A pool can instead check with an HTTP `GET` on a path you choose (any 2xx or 3xx answer is healthy), and on another port than the one traffic goes to. Two failures take a server out, two successes bring it back. The page shows each server's state as `up` or `down`.

If you delete a server that is in a pool, it stays listed as `gone` and receives no traffic until you remove it.

## While a project is suspended

The load balancer's VMs are paused with your servers, so it serves nothing and its configuration cannot change. You can still delete it. It comes back by itself when the project is resumed.

## Delete one

**Delete** removes the VMs and their disks and gives the address back. Your servers are not touched. You cannot delete its VMs or its address on their own, stop them, add a volume to them or make an image of them: change or delete the load balancer instead. While it is first being created, **Delete** asks you to try again a moment later.

Nothing updates the software on a running load balancer by itself. With high availability, press **Replace appliances**. A load balancer on one VM has no replace: create a new load balancer and delete the old one; the new one gets a different address.
