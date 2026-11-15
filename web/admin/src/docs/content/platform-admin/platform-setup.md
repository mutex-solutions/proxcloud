# Platform setup

**System → Platform setup** takes a new install from an empty portal to the first tenant server. It has eight steps. Each step shows its state, read from the portal and, when you press **Check again**, from the Proxmox VE cluster itself:

| State | Meaning |
|---|---|
| Done | Every check of the step passed. |
| Needs attention | It works, with a warning you should read (for example, no GPU mapping on a cluster without GPUs). It does not block going live. |
| To do | Something is missing or failed. |
| Unknown | The portal could not read the cluster. Fix the connection and check again; an unknown step is never shown as done. |

The page needs read access to services, price plans, the platform configuration and the billing configuration. Without those four, the Dashboard card and the menu entry are hidden.

## 1. Prepare Proxmox

Done by hand, as root, on one node of the cluster. Enter the API token ID you want (default `proxcloud@pve!platform`), say whether the cluster has GPUs to map, and name the store for image imports if you have one. The page then shows one script to paste into a root shell. It creates the `ProxCloudPlatform` role, the pools `pc-templates` and `proxcloud-system`, the user, its token and the ACLs, and it can be run again safely. The token secret is printed once: keep it for the next step. The last lines print what the token holds and the node's certificate fingerprint.

The portal holds no root credential and never asks for one. Turn on the two firewalls yourself in the Proxmox UI: **Datacenter → Firewall → Options → Firewall** and, on each node, **Firewall → Options → nftables**.

## 2. Connect

Enter the API URL, the token ID and secret, and how to trust the certificate. Pin the fingerprint the page shows only after comparing it with `pvenode cert info` on the node. **Test** runs every check at once and lists one row per check: reachability and TLS, the token, both firewalls, the privileges on each ACL path, Proxmox VE 9.2 or later, and the pool `pc-templates`.

**Pool prefix** (default `pc-`): every project gets the pool `<prefix><projectId>`. If another portal already uses this cluster, give this one its own prefix and its own tenant network range: the cluster firewall rules that isolate tenants are owned per prefix, and two portals with the same prefix rewrite each other's rules.

**Save** creates the provider but does not attach new projects to it yet; that happens at **Go live**. You can save with failed checks; the step stays open until they pass.

## 3. Network

The tenant network settings, as a form: the zone kind (simple, VLAN or EVPN) and its id, the CIDR range tenant subnets are carved from (set it once: subnets outside a later range lose the rule that separates them), the exit nodes, an optional public network on a bridge that is not any node's management bridge, and the networks tenants must not reach. Saving keeps every other stored network setting.

**Server seeds** says how a new server on a network with DHCP gets its hostname, password and SSH keys. A server that cannot get them boots with the template's hostname, no password and no guest agent, and nothing reports it, so the step needs attention until you choose (it does not block going live):

- **By URL from** the API address shown: tenant servers download their seed from it. Choose this only when they can reach that address and trust its certificate.
- **On an ISO**: the seed goes on a CD in the server. Choose this for a LAN install or a self-signed certificate, for example the one-host install with TLS mode `internal`.

Servers on a network without DHCP always get an ISO. **Save server seeds** changes only this choice; the other seed settings of the provider are kept.

## 4. Storage

**Detect** lists the cluster's storages with a proposed storage profile for each. Confirm the profiles, pick the default, the store for image imports (it needs the `import` content type) and the store for server backups. The page explains how to add NAS stores to Proxmox and how to connect TrueNAS for tenant file shares.

## 5. OS templates

Done as soon as the cluster has at least one published OS template, for example from `images/build.py`. Otherwise the step walks you through importing the published images: the template updates switch of the install (`./proxcloud-up.sh install --template-updates` on the one-host install), a GitHub token with `read:packages`, the smoke-test network, the OS and nodes to import, and **Publish all that passed**, which publishes every template whose smoke test passed. Nothing is published without that click.

## 6. Catalog and pricing

Shows the default flavors, quota and price rules before anything is created. **Apply** asks you to confirm the rates: they become invoices. Only missing items are created; an existing public price plan is never replaced. The step also lists flavors that no price rule matches, which would be billed as zero.

## 7. Go live

Enabled when steps 2 to 5 are done and step 6 is done or knowingly skipped, on a status just read from the cluster. It attaches new projects to the provider, runs one provider sync and attaches the projects created meanwhile. When the job scheduler is off, the step says so: nothing syncs on a timer and no charge runs until you turn it on (`./proxcloud-up.sh install --scheduler` on the one-host install). It does not block going live.

## 8. Next: managed services

Managed Kubernetes, managed databases and sandboxes need a platform cluster. Enabling them from the console is planned; see the architecture decision record 0009 in the repository.
