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

- A **pool** is a set of your servers. Choose how to spread traffic: round robin, least connections, or by source address.
- Each server in a pool has a **port** and a **weight**. A server with twice the weight gets twice the traffic.
- A **listener** opens a port on the public address and forwards it to a pool. Its **protocol** is TCP or HTTP, and the pool speaks the same to your servers: a pool serves listeners of one protocol, so changing a listener's protocol changes its pool and the pool's other listeners with it. Under **Allowed sources** you can list the networks that may connect, such as `203.0.113.0/24`; empty means any address. Other clients are dropped before they reach the load balancer.
- A listener's **connection limit** is the most connections it holds open at a time; further clients wait until one closes. Empty means no limit of its own. The load balancer as a whole holds 20000. Its **idle timeout** closes a client's connection after that many seconds without data (60 when empty).
- **Session persistence** by source address keeps each client address on the server it first reached, until that address has made no new connection for the time you set (three hours when empty). The load balancer forgets these choices when you save a change and when the other appliance takes over, and clients are then spread afresh.
- **PROXY protocol** on a pool passes the client's address to your servers in a PROXY header, version 1 (a text line) or 2 (binary). Turn it on only when your servers expect that header, in that version.
- A pool's **connect timeout** is how long a server may take to accept a connection before the next server is tried (2 seconds when empty), and its **server idle timeout** closes a connection after that many seconds without data from the server (60 when empty). For long-lived connections such as a database or a websocket, raise both idle timeouts.

UDP listeners and TLS termination on the load balancer are not available yet.

Press **Save configuration**. The change reaches the load balancer within seconds. If HAProxy refuses it, the page shows why and the load balancer keeps running the last configuration that worked.

Only servers of this project, on the load balancer's own network and subnet, can be added. HTTP pools add an `X-Forwarded-For` header with the client's address. To serve HTTPS, use a TCP pool on port 443 and terminate TLS on your servers.

## Let the traffic in

The load balancer reaches your servers from its private addresses (one per VM; a replace gives new ones). The `default` security group already allows that. If a server uses other groups, open its port to the load balancer's network.

## Health

Every server is checked every 3 seconds, by default with a TCP connection to its port. Two failures take a server out, two successes bring it back. The page shows each server's state as `up` or `down`. A pool can check differently:

- **HTTP GET** or **HTTPS GET** on a path you choose: any 2xx or 3xx answer is healthy. The HTTPS check speaks TLS to the server and does not verify its certificate.
- **TLS hello**: the server is healthy when it answers a TLS hello with TLS. Use it for a TCP pool whose servers end TLS themselves. It does not look at the certificate.
- **None**: no check. Every server always gets traffic and reads `no check`; use it only when a check would disturb the service.
- On **another port** than the one traffic goes to.
- With your own figures: how often (**Every**, 1 to 300 seconds), how long an answer may take (**Timeout**), and how many checks in a row bring a server back (**Up after**) or take it out (**Down after**), 1 to 10 each.

If you delete a server that is in a pool, it stays listed as `gone` and receives no traffic until you remove it.

## While a project is suspended

The load balancer's VMs are paused with your servers, so it serves nothing and its configuration cannot change. You can still delete it. It comes back by itself when the project is resumed.

## Delete one

**Delete** removes the VMs and their disks and gives the address back. Your servers are not touched. You cannot delete its VMs or its address on their own, stop them, add a volume to them or make an image of them: change or delete the load balancer instead. While it is first being created, **Delete** asks you to try again a moment later.

Nothing updates the software on a running load balancer by itself. With high availability, press **Replace appliances**. A load balancer on one VM has no replace: create a new load balancer and delete the old one; the new one gets a different address.
