# Working with Servers

Servers are the virtual machines at the center of most projects. This guide covers the compute resources that surround them — key pairs and images — and the day-to-day operations you'll run on a machine once it exists. If you've never launched one, start with [Launch Your First Server](/docs/getting-started/first-server).

Everything here lives under the **Compute** group in the sidebar.

## Launching and sizing

Create a server from **Compute → Servers**. Each machine is a Proxmox VE virtual machine cloned from an **image** (the operating system to boot), sized by a **flavor** (vCPUs, RAM, optional GPU, and a default root-disk size), attached to one of your project's **networks**, given a **key pair** for SSH login (injected by cloud-init), and protected by one or more **security groups**. Pick a flavor that matches the workload, then set the root-disk size; it can't go below what the image needs. The flavor decides which storage the root disk lives on, and the root disk is deleted with the server. ProxCloud places the server on a host where that storage has room for the root disk; if no host has room, or the image can only be cloned on a host where the flavor's storage isn't available, the create is refused with the reason and nothing is created. Try another flavor or ask your operator. Add data disks after launch from **Storage → Volumes** (see [Block Volumes](/docs/guides/volumes)).

There are no floating IPs on Proxmox. To make a server reachable from the internet, give it a public NIC as described in [Networking](/docs/guides/networks).

## Power actions

A server's page exposes the lifecycle controls:

- **Reboot** — asks the guest OS to restart cleanly.
- **Stop / Start** — power the server off and on. A stopped server keeps its disks and network attachments.
- From **More actions**: **Rename**, **Resize** (move to another flavor), **Console (VNC)** for a browser console, and **Delete**.

Actions the cloud behind your project can't perform are not shown. On Proxmox that means there is no rebuild, rescue or set-password action and no cloud-init user-data field yet; with password login the password is set on the image's default account. The server's **Network** tab lists the addresses the guest reports.

When you're done with a machine, **delete** it so it stops consuming compute and accruing charges. Deleting destroys the VM together with all of its disks, including volumes you detached from it, so copy off anything you want to keep first.

## Key pairs

SSH access is by key pair, managed under **Compute → Key pairs**. Either upload the public half of a key you already hold, or have the portal generate a pair and download the private key. A generated private key is shown once and cannot be retrieved afterward, so save it the moment it appears.

## Images

**Compute → Images** lists the machine images you can boot from — both the templates the operator publishes and any you own. Choose one when launching a server; the image determines the OS and the default login user.

To save a server as your own image, open the server's **Snapshots** tab and create a snapshot. ProxCloud clones the server into a template that belongs to your project; it appears under **Compute → Images** and can be used to launch new servers. Deleting the image removes the template.

## Keeping the console in sync

The status you see on a server reflects the real state on the Proxmox VE cluster. It is kept current by a background sync every 15 minutes, by every action you take in ProxCloud, and by a live refresh when you open a server. A change made directly in Proxmox appears after the next sync. For the mechanics, see [How Provisioning Works](/docs/concepts/provisioning).

## User data and metadata

The create wizard takes cloud-init user data (a `#cloud-config` or a script) and any instance metadata the
operator offers. Inside the server, read the metadata with `cloud-init query ds.meta_data.meta`.

## Set password

Sets a user's password in the running server through its guest agent. Without a username it is the image's
default user (`debian`, `ubuntu`, ...).

## Rebuild

Replaces the boot disk with a fresh copy of an image, losing everything on it. The server keeps its addresses,
firewall and data volumes. Delete its snapshots first.

## When a host fails

If the host of a server goes down, the server shows **Unknown**. It keeps its addresses and volumes, you are not charged for
the server itself while it lasts, and actions on it are refused until the host is back. It then shows as stopped: start it
again. Other servers in the project are not affected. To have a server restart on another host by itself, enable
**Protection**.

## Snapshots

The **Snapshots** tab saves the state of the server's disks and rolls back to it, losing what was written
since. It protects against mistakes, not against disk or node failure. Local ZFS rolls back to the newest
snapshot only: delete newer ones first. A server with a disk on capacity storage cannot be snapshotted.

**Images** on the same page are something else: a full copy of the server's disk, to create new servers from.

## Public IP

On the server's **Network** tab, attach a public IP from a public network the project may use, then reboot:
the guest brings the address up at boot, and it becomes the default route. Detach gives the address back.

## Spread groups

Give servers the same spread group in the create wizard to place them on different nodes where possible.
With the strict option, creating one fails rather than share a node. Placement happens once, at create.
