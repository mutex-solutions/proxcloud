# Launch Your First Server

This walkthrough takes you from an empty account to a running virtual machine. It assumes you've already [activated billing](/docs/getting-started/account-setup) — without an active profile the create buttons stay disabled.

## Step 1 — Have a project to work in

You're always inside a project, and your first sign-in gave you a default one, so you can start there. Open the project switcher at the top of the sidebar and either create a new project or select an existing one (you can also manage projects under **Organization → Projects**). Give a new project a name that tells you what it holds — `demo`, `staging`, a client name — and it becomes your active context, scoping the sidebar to it.

Projects also anchor teamwork and billing: members are added per project, and costs roll up per project. To pull colleagues in, see [Teammates and Invitations](/docs/guides/team-members).

## Step 2 — Line up what the server needs

A server is assembled from a few smaller pieces. You can create them as you go through the launch form, but it helps to know what they are:

- **Image** — the operating system the machine boots from. Browse **Compute → Images** to see what's available.
- **Flavor** — the compute size (vCPUs and RAM) and a suggested root-storage size. You pick this in the launch form.
- **Storage** — the root block-volume size and storage class. You can optionally add persistent data volumes at launch.
- **Network** — the private network the server attaches to. If the project has none yet, create one under **Network → Networks**.
- **Key pair** — the SSH key you'll log in with. Under **Compute → Key pairs**, upload a public key you already have or let the portal generate one and download the private half. You can't retrieve a generated private key later, so save it immediately.
- **Security group** — the firewall around the server. A default group usually exists; open **Network → Security groups** to allow the ports you need (for example SSH on 22).

## Step 3 — Launch the server

Go to **Compute → Servers** and click **Create server**. The launch form is a single page with numbered sections — work down it top to bottom:

1. Pick a **location** (region) and **availability zone**.
2. Choose the **image** to boot and a **flavor** for its compute size (vCPUs and RAM).
3. Under **Storage**, choose the root-volume size and storage class. Add persistent data volumes if the workload needs them.
4. Attach it to a **network**, and under **Access** select a **key pair** for SSH login.
5. Still under **Access**, tick a **security group** that opens the ports you'll use.
6. Leave **Assign floating IP** on (the default) to have a public address attached automatically shortly after the server comes up.
7. Give it a **name** and click **Create server**. ProxCloud provisions the machine on the underlying cloud; it moves through build states and settles on **ACTIVE** once it's up. (For what's happening behind that, see [How Provisioning Works](/docs/concepts/provisioning).)

The new server appears in the list right away, first in a **build** state:

## Step 4 — Reach it from the internet

If you left **Assign floating IP** on in the launch form, the server picks up a public address on its own shortly after going **ACTIVE** — skip to the SSH step.

To attach one by hand instead:

1. Under **Network → Floating IPs**, allocate a floating IP.
2. Associate that IP with your server.
3. SSH in with the private key from your key pair — for example `ssh -i mykey.pem <user>@<floating-ip>`. The default login user depends on the image you chose.

## Step 5 — Operate and tidy up

From the server's page you can run power actions — **Start**, **Stop**, **Reboot** — and, from **More actions**, delete it. Deleting releases the compute so it stops accruing charges; release the floating IP too under **Network → Floating IPs** if you no longer need it.

Deleting asks you to confirm, since it can't be undone:

You can drive these same actions from an AI assistant instead of the console — see [AI Agent Access](/docs/guides/ai-agents).

That's the full loop: project, building blocks, launch, connect, operate. The [task guides](/docs/guides/servers) go deeper on each resource type.
