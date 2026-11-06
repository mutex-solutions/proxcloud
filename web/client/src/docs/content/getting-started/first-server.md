# Launch Your First Server

This walkthrough takes you from an empty account to a running virtual machine. It assumes you've already [activated billing](/docs/getting-started/account-setup) — without an active profile the create buttons stay disabled.

## Step 1 — Have a project to work in

You're always inside a project, and your first sign-in gave you a default one, so you can start there. Open the project switcher at the top of the sidebar and either create a new project or select an existing one (you can also manage projects under **Organization → Projects**). Give a new project a name that tells you what it holds — `demo`, `staging`, a client name — and it becomes your active context, scoping the sidebar to it.

Projects also anchor teamwork and billing: members are added per project, and costs roll up per project. To pull colleagues in, see [Teammates and Invitations](/docs/guides/team-members).

## Step 2 — Line up what the server needs

A server is assembled from a few smaller pieces. You can create them as you go through the launch form, but it helps to know what they are:

- **Image** — the operating system the machine boots from: a template published by your operator, or one you saved from your own server. Browse **Compute → Images** to see what's available.
- **Flavor** — the compute size (vCPUs, RAM, optional GPU) and a default root-disk size. The flavor also decides which storage the root disk lands on. You pick this in the launch form.
- **Storage** — the root-disk size. It can't be smaller than the image requires.
- **Network** — one of your project's private networks. A server can only be launched onto a network your project owns, so if the project has none yet, create one under **Network → Networks** first.
- **Key pair** — the SSH key you'll log in with. Under **Compute → Key pairs**, upload a public key you already have or let the portal generate one and download the private half. You can't retrieve a generated private key later, so save it immediately.
- **Security group** — the firewall around the server. A default group usually exists; open **Network → Security groups** to allow the ports you need (for example SSH on 22).

## Step 3 — Launch the server

Go to **Compute → Servers** and click **Create server**. The launch form is a single page with numbered sections — work down it top to bottom:

1. Pick a **location** (region). ProxCloud chooses the Proxmox node for you.
2. Choose the **image** to boot and a **flavor** for its compute size.
3. Under **Storage**, set the root-disk size. Extra data disks are added after launch from **Storage → Volumes** (see [Block Volumes](/docs/guides/volumes)).
4. Attach it to one of your **networks**, and under **Access** select a **key pair** for SSH login.
5. Still under **Access**, tick a **security group** that opens the ports you'll use. Every project has a default group.
6. Give it a **name** and click **Create server**. ProxCloud clones the image into a new VM in your project, turns on its firewall before the first boot, and starts it. It moves through **BUILD** and settles on **ACTIVE** once it's up. (For what's happening behind that, see [How Provisioning Works](/docs/concepts/provisioning).)

The new server appears in the list right away, first in a **build** state:

## Step 4 — Reach it from the internet

A new server can already reach the internet: its private network gives it outbound access through SNAT. To reach it *from* the internet, it needs a **public NIC** — a second network interface on one of the public networks your operator has enabled for your project, carrying one public address. The address is pinned to the server by its firewall, so it can't be used by anything else.

There are no floating IPs on Proxmox, and the portal does not attach a public NIC for you yet. Attach one through the API with the server action `ATTACH_PUBLIC_NETWORK` (`POST /api/v1/project/{id}/cloud/{serverId}/action`, data `networkId` = one of the public networks listed by `GET /api/v1/project/{id}/public-networks`). `DETACH_PUBLIC_NETWORK` removes it again and returns the address to the pool. The NIC plugs in at once, but the guest configures it at its next boot, so **reboot the server** after attaching or detaching. While a server has a public NIC, its outbound traffic leaves through the public address.

Once the server has a public address:

1. Make sure one of its security groups allows SSH (port 22) from your address.
2. SSH in with the private key from your key pair — for example `ssh -i mykey.pem <user>@<public-ip>`. The default login user depends on the image you chose.

## Step 5 — Operate and tidy up

From the server's page you can run power actions — **Start**, **Stop**, **Reboot** — and, from **More actions**, delete it. Deleting destroys the VM together with its disks, so it stops accruing charges, and returns any public address to the pool.

Deleting asks you to confirm, since it can't be undone:

You can drive these same actions from an AI assistant instead of the console — see [AI Agent Access](/docs/guides/ai-agents).

That's the full loop: project, building blocks, launch, connect, operate. The [task guides](/docs/guides/servers) go deeper on each resource type.
