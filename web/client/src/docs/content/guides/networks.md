# Networking

The **Network** group in the sidebar holds what connects your servers to each other and to the outside world: your private networks and the security groups that firewall them. Public addresses are attached to individual servers, as described below.

## Private networks

Start under **Network → Networks**, where you create the private networks your servers attach to. A project with no network yet needs one before its first server can be launched, and a server can only be launched onto a network your project owns.

Each network is a virtual network on the Proxmox VE cluster with an IPv4 subnet (more can be added, see below). Leave the range empty and ProxCloud picks a free one from the range your operator reserved for tenant networks, or ask for a specific CIDR inside that range. Address delivery depends on the zone: PVE DHCP is available on simple zones; VLAN/EVPN networks use platform-allocated static addresses passed through cloud-init instead. You do not need to add a competing DHCP server. Outbound NAT (SNAT) is a separate subnet setting and depends on the operator's routing and egress configuration.

![The Create network dialog](/docs-img/create-network.png)

Your networks can talk to each other. They are isolated from every other project's networks: traffic towards another tenant's range is dropped.

A network can only be deleted once no server has an interface on it.

## Reaching the internet

**Outbound** traffic uses the subnet's internet/SNAT setting where the operator offers it. A static address alone does not prove working gateway, DNS or firewall egress; contact the operator if the subnet has no working outbound route.

**Inbound** traffic needs a **public IP**: a server gets a second network interface on one of the public networks your operator has set up, with one public address from that network's pool. The address is pinned to the server by its firewall, so the server can't use any other address on that network. Your operator decides which public networks your project may use.

Attach one on the server's **Network** tab (**Public IP** card → pick a public network → **Attach public IP**); **Detach** removes the NIC and returns the address to the pool. Through the API this is the server action `ATTACH_PUBLIC_NETWORK` (`POST /api/v1/project/{id}/cloud/{serverId}/action`, data `networkId` = one of the networks listed by `GET /api/v1/project/{id}/public-networks`) and `DETACH_PUBLIC_NETWORK`. Deleting the server returns it too. The guest configures the change at its next boot, so **reboot the server** after attaching or detaching. While a server has a public NIC, its default route and outbound traffic go through the public address.

To spread inbound traffic across several servers behind one address, use a [load balancer](/docs/guides/load-balancers).

## Firewalling with security groups

**Network → Security groups** are the firewalls around your servers. Each group is a set of allow rules for inbound and outbound traffic; attach one or more to a server to control which ports and sources can reach it. Every project starts with a default group. A common first rule is allowing SSH (port 22) from your own address.

![A security group with an SSH rule](/docs-img/security-group-rules.png)

Security groups are enforced by the Proxmox VE cluster firewall, which is switched on for a server before its first boot.

## Subnets and routing

A network can hold several subnets (**Subnets** tab), and a server picks one at create. All networks and
subnets of a project route to each other without a router to set up; other projects cannot reach them. Each
subnet has its own outbound internet switch.
