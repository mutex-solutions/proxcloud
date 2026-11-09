# Load balancers

A load balancer gives your servers one public address and spreads traffic across them. Servers that stop answering are left out until they answer again.

## What you pay for

A load balancer runs on its own small VM in your project, so it costs what a server of that size costs, plus its disk, its public address and the traffic on that address, and it counts in your quota the same way. The VM does not appear under **Servers**. You will see it, its disk and its address on your bill.

## Create one

Under **Network → Load balancers**, press **Create load balancer** and pick a name, the network your servers are on, a public network and a size. The network must be one whose addresses the platform assigns, which is the usual kind.

The status reads `build` for a minute or two while the VM boots and installs what it needs, then `active`.

## Add listeners, pools and servers

- A **pool** is a set of your servers. Choose HTTP or TCP, and how to spread traffic: round robin, least connections, or by source address.
- Each server in a pool has a **port** and a **weight**. A server with twice the weight gets twice the traffic.
- A **listener** opens a port on the public address and forwards it to a pool.

Press **Save configuration**. The change reaches the load balancer within seconds. If HAProxy refuses it, the page shows why and the load balancer keeps running the last configuration that worked.

Only servers of this project, on the load balancer's own network and subnet, can be added. HTTP pools add an `X-Forwarded-For` header with the client's address. To serve HTTPS, use a TCP pool on port 443 and terminate TLS on your servers.

## Let the traffic in

The load balancer reaches your servers from its private address. The `default` security group already allows that. If a server uses other groups, open its port to that address.

## Health

Every server is checked every 3 seconds with a TCP connection. Two failures take it out, two successes bring it back. The page shows each server's state as `up` or `down`.

If you delete a server that is in a pool, it stays listed as `gone` and receives no traffic until you remove it.

## While a project is suspended

The load balancer's VM is paused with your servers, so it serves nothing and its configuration cannot change. You can still delete it. It comes back by itself when the project is resumed.

## Delete one

**Delete** removes the VM and its disk and gives the address back. Your servers are not touched. You cannot delete the VM or its address on their own, stop it, add a volume to it or make an image of it: change or delete the load balancer instead. While it is first being created, **Delete** asks you to try again a moment later.

Nothing updates the software on a running load balancer. To move to a current one, create a new load balancer and delete the old one. The new one gets a different address.
