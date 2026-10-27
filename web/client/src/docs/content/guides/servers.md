# Working with Servers

Servers are the virtual machines at the center of most projects. This guide covers the compute resources that surround them — server groups, key pairs, and images — and the day-to-day operations you'll run on a machine once it exists. If you've never launched one, start with [Launch Your First Server](/docs/getting-started/first-server).

Everything here lives under the **Compute** group in the sidebar.

## Launching and sizing

Create a server from **Compute → Servers**. Each machine is built from an **image** (the operating system to boot), a **flavor** (its vCPU, RAM, and suggested root-storage size), a **root volume**, a **network** to attach to, a **key pair** for SSH login, and one or more **security groups** for firewalling. Pick a flavor that matches the workload, then choose the root-volume size and one of the storage classes published by the operator. The image boots from this block volume; the root volume is deleted with the server. You can also add data volumes during launch. Data volumes are preserved when the server is deleted so they can be attached elsewhere. The launch form also carries an **Assign floating IP** option, on by default — leave it on and a public IP is attached automatically shortly after the server becomes active.

## Power actions

A server's page exposes the lifecycle controls:

- **Soft reboot** — asks the guest OS to restart cleanly.
- **Hard reboot** — power-cycles the machine when the guest is unresponsive.
- **Stop / Start** — power the instance off and on. A stopped server keeps its disks and network attachments.

When you're done with a machine, **delete** it so it stops consuming compute and accruing charges.

## Server groups

**Compute → Server groups** lets you bundle instances under a scheduling policy — for example keeping members of a group apart on separate hosts for resilience, or together for locality. Create the group first, then launch servers into it.

## Key pairs

SSH access is by key pair, managed under **Compute → Key pairs**. Either upload the public half of a key you already hold, or have the portal generate a pair and download the private key. A generated private key is shown once and cannot be retrieved afterward, so save it the moment it appears.

## Images

**Compute → Images** lists the machine images you can boot from — both those the operator publishes and any you own. Choose one when launching a server; the image determines the OS and the default login user.

## Keeping the console in sync

The status you see on a server reflects the real state on the underlying cloud, kept current by a background sync and by live event notifications. A machine you reboot or delete directly on OpenStack shows the change here too, usually within seconds. For the mechanics, see [How Provisioning Works](/docs/concepts/provisioning).
