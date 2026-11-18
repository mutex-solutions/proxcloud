# Networking

The **Network** group in the sidebar holds what connects your servers to each other and to the outside world: your private networks and the security groups that firewall them. Public addresses are attached to individual servers, as described below.

## Private networks

Start under **Network → Networks**, where you create the private networks your servers attach to. A project with no network yet needs one before its first server can be launched, and a server can only be launched onto a network your project owns.

Each network is a virtual network on the Proxmox VE cluster with an IPv4 subnet (more can be added, see below). Leave the range empty and ProxCloud picks a free one from the range your operator reserved for tenant networks, or ask for a specific CIDR inside that range. Address delivery depends on the zone your operator set up: where the zone runs a DHCP server (some simple zones), servers get their address over DHCP; everywhere else (VLAN and EVPN zones, simple zones without DHCP) ProxCloud assigns each server a fixed address and passes it through cloud-init, and the network's details show DHCP as off. You do not need to add a competing DHCP server. Outbound NAT (SNAT) is a separate subnet setting and depends on the operator's routing and egress configuration.

Creating a network can take up to about 20 seconds: the request returns only after the new network has been applied on every node of the site, and the dialog says so while it waits. The network is usable as soon as it is listed.

![The Create network dialog](/docs-img/create-network.png)

*Illustration: current console UI with synthetic demo data; no live cloud operation was performed.*

Your networks can talk to each other. They are isolated from every other project's networks: traffic towards another tenant's range is dropped.

A network can only be deleted once no server has an interface on it.

## Reaching the internet

**Outbound** traffic uses the subnet's internet/SNAT setting where the operator offers it. A static address alone does not prove working gateway, DNS or firewall egress; contact the operator if the subnet has no working outbound route. Where the operator's own router serves the networks (VLANs), a network has no SNAT and no DHCP setting: it shows both as off, and outbound access is the router's.

**Inbound** traffic needs a **public IP**: a server gets a second network interface on one of the public networks your operator has set up, with one public address from that network's pool. The address is pinned to the server by its firewall, so the server can't use any other address on that network. Your operator decides which public networks your project may use.

Attach one on the server's **Network** tab (**Public IP** card → pick a public network → **Attach public IP**); **Detach** removes the NIC and returns the address to the pool. Through the API this is the server action `ATTACH_PUBLIC_NETWORK` (`POST /api/v1/project/{id}/cloud/{serverId}/action`, data `networkId` = one of the networks listed by `GET /api/v1/project/{id}/public-networks`) and `DETACH_PUBLIC_NETWORK`. Deleting the server returns it too. The guest configures the change at its next boot, so **reboot the server** after attaching or detaching. While a server has a public NIC, its default route and outbound traffic go through the public address.

To spread inbound traffic across several servers behind one address, use a [load balancer](/docs/guides/load-balancers).

## Firewalling with security groups

**Network → Security groups** are the firewalls around your servers. Each group is a list of rules that allow **inbound** traffic: a protocol, a destination port or port range, and the source address range. Attach one or more groups to a server; inbound traffic that no rule allows is dropped, and replies to connections the server opened need no rule. Every project starts with a default group. A common first rule is allowing SSH (port 22) from your own address.

A group named `k8s-<cluster id>` belongs to a Kubernetes cluster and firewalls its nodes ([Kubernetes](/docs/guides/kubernetes), "Firewall around the nodes"). You may change its rules; you cannot delete it or attach it to a server, and you cannot attach your own groups to a cluster's nodes.

What a security group does not do:

- **It does not filter outbound traffic.** A server may open any outbound connection. The API accepts a rule with direction `egress` and the console lists it as such, but it restricts nothing; the API's answer says so.
- **There are no deny rules, no rule order, no logging and no rule that names another group as the source.** The API refuses a rule that asks for one of these instead of storing a wider rule than you asked for.

The rule form asks for a **protocol** from a list (TCP, UDP, SCTP, ICMP, GRE, ESP, AH, or any), a **port** or port range for the protocols that have ports, the **source** as a CIDR (empty for anywhere), and an optional **description**, shown in the rule list. The direction is always ingress. The pencil on a rule opens the same form on that rule. Saving an edit is one action: the changed rule is added and then the old one is removed, so the traffic the rule allows is not interrupted. If someone else changes the group in between, you are told that both rules are there; delete the old one.

With no group ticked when you create a server, the group `default` applies. Unless you changed it, `default` allows traffic only from your project's own networks: nothing from outside reaches the server until a group allows it.

A rule's fields are checked before it is stored: the protocol is `tcp`, `udp`, `sctp`, `icmp`, `gre`, `esp` or `ah` (empty for any); a port range needs `tcp`, `udp` or `sctp` and ports from 1 to 65535; the remote is a CIDR such as `203.0.113.0/24` or a single address (empty for anywhere).

If someone else changes the group while you have its page open, deleting a rule from your page answers that the rules have changed and reloads the list; delete the rule again from the current list. Nothing is deleted by the refused attempt.

![A security group with an SSH rule](/docs-img/security-group-rules.png)

*Illustration: current console UI with synthetic demo data; no live cloud operation was performed.*

Security groups are enforced by the Proxmox VE cluster firewall, which is switched on for a server before its first boot.

## Subnets and routing

A network can hold several subnets (**Subnets** tab), and a server picks one at create. All networks and
subnets of a project route to each other without a router to set up; other projects cannot reach them. Each
subnet has its own outbound internet switch. The **Servers** tab of a network lists the servers with an interface
on it; it cannot be read while a host of the project's servers is down.
