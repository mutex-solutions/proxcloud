# Networking

The **Network** group in the sidebar holds what connects your servers to each other and to the outside world: your private networks and the security groups that firewall them. Public addresses are attached to individual servers, as described below.

## Private networks

Start under **Network → Networks**, where you create the private networks your servers attach to. A project with no network yet needs one before its first server can be launched, and a server can only be launched onto a network your project owns.

Each network is a virtual network on the Proxmox VE cluster with one IPv4 subnet. Leave the range empty and ProxCloud picks a free one from the range your operator reserved for tenant networks, or ask for a specific CIDR inside that range. DHCP and outbound NAT (SNAT) are on by default, so servers get an address and can reach the internet without further setup.

Your networks can talk to each other. They are isolated from every other project's networks: traffic towards another tenant's range is dropped.

A network can only be deleted once no server has an interface on it.

## Reaching the internet

**Outbound** traffic needs nothing extra: SNAT on the private network lets every server reach the internet.

**Inbound** traffic needs a **public NIC**. There are no floating IPs on Proxmox. Instead, a server gets a second network interface on one of the public networks your operator has set up, with one public address from that network's pool. The address is pinned to the server by its firewall, so the server can't use any other address on that network. Your operator decides which public networks your project may use.

The portal has no button for this yet. Attach a public NIC through the API with the server action `ATTACH_PUBLIC_NETWORK` (`POST /api/v1/project/{id}/cloud/{serverId}/action`, data `networkId` = one of the networks listed by `GET /api/v1/project/{id}/public-networks`). `DETACH_PUBLIC_NETWORK` removes the NIC and returns the address to the pool. Deleting the server returns it too.

## Firewalling with security groups

**Network → Security groups** are the firewalls around your servers. Each group is a set of allow rules for inbound and outbound traffic; attach one or more to a server to control which ports and sources can reach it. Every project starts with a default group. A common first rule is allowing SSH (port 22) from your own address.

Security groups are enforced by the Proxmox VE cluster firewall, which is switched on for a server before its first boot.
