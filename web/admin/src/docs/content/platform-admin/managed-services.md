# Managed services

**System → Managed services** installs what tenant Kubernetes clusters and tenant databases run on: one *platform cluster* on your Proxmox VE cluster. The page builds it from one small VM, the *seed*, which creates the cluster's machines and keeps its components at the versions of your ProxCloud release.

It is optional. Tenants can create servers without it, and **Go live** in Platform setup never waits for it.

Reading the page needs read access to services; every button needs the permission to manage services.

## Before you start

| Needed | How |
|---|---|
| Managed services turned on | On the one-host install: `./proxcloud-up.sh install --managed-services`. This also turns on template updates. Until then the page shows only this command. |
| A saved Proxmox provider whose connection checks pass | [Platform setup](/docs/platform-admin/platform-setup), steps 1 and 2. |
| A GitHub token | **System → Integrations**. The two images are downloaded with it. |
| A network for the platform | One bridge, untagged, with a free address range and a route to it from the host ProxCloud runs on. |
| Root on one Proxmox node, once | To run the script of step 2. ProxCloud holds no root password. |

Tenant Kubernetes clusters also need a network zone without DHCP: the Proxmox provider's default zone must be an EVPN or a VLAN zone, or a simple zone without DHCP (`"dhcp": false` in the zone's settings before ProxCloud creates it, or a zone you made in Proxmox without DHCP). On a zone with DHCP (a simple zone that ProxCloud created by its default) the install works and databases work, but a cluster a tenant creates on its networks is refused. A Kubernetes provider with shared workers uses no tenant network and is not affected. Step 3 warns about it.

In a VLAN zone Proxmox provides no gateway, no DHCP and no SNAT: your own router must hold the first address of each tenant subnet on its VLAN and route it to the load balancer range. Without it a cluster's workers never join.

The page also warns when the job scheduler is off: tenant clusters and databases then update their status only when a provider sync runs, and no project is deleted: "Delete now" is refused and says so, and a delete with the grace period is accepted but stays scheduled until the scheduler runs.

No check of step 3 reads the following, so look before the first run:

- The seed and the machines reach the Proxmox API URL of the settings from the platform network. With the datacenter firewall on, a node accepts its API port only from its own management network unless you add the platform network there.
- Each node has memory left for its machines: the cluster manager counts the configured memory of every VM it sees on the node, running or stopped.
- The platform bridge is one network across the nodes, not a separate NAT network on each.
- The boot store has room for a full copy of every machine's disk, on disks that write fast: the seed and the control plane stop working when a write takes seconds.
- The Proxmox provider names its region.

## Step 1: Settings

Saving keeps a draft. Nothing is created on Proxmox until you press **Install**, and a draft can be changed as often as you like.

- **Name.** Lower-case letters, digits and dashes. The two providers are named after it.
- **Start from.** Three choices that fill in the nodes, counts and sizes for you, for a new installation (on an installed platform they are switched off: a preset would resize and replace its machines). **Homelab** is for one Proxmox node or a small site: one control plane machine and one small machine per group, everything on one node; on a site with one node it also chooses a platform network routed by that node. **Standard** is three control plane machines on one node and two machines per group, spread over two nodes when the site has several; when the node has too little free memory for three, or the release builds one, it takes one and says so. **Custom** fills in nothing. A choice only fills the form: every field stays yours to change, and what is saved is the fields.
- **Node groups.** Three groups of worker machines: `system` (the platform's own components), `kamaji` (the control planes of tenant Kubernetes clusters) and `dbaas` (tenant databases). For each: the Proxmox nodes, the number of machines (at least one per chosen node) and their size. All of it can change later.
- **Platform network.** Where it comes from, then the network, gateway and DNS. **An existing bridge** is one that exists on every node used and that your router routes; an optional VLAN tag (1 to 4094) puts every machine's NIC on that VLAN of the bridge, so the VLAN needs no bridge of its own (this needs a platform release whose cluster chart is 0.1.14 or newer: the Check says so when it is older). **Routed by the node** is for a site with one Proxmox node: you name a new vnet and the install makes it, with the gateway on the node and SNAT. Tenant traffic to the platform then never leaves the node. Everything outside the node (the ProxCloud host, your workstation) needs a route to the platform network through the node's address, on your router or on those hosts; without it the install stops when it cannot reach the seed. The seed, the control plane and every group must then be on that one node. Routed mode is not verified on a lab. The choice is fixed after the first install. Then: the address of the cluster's API; a range for the machines (count one spare per group and node, used during updates); a range for load balancers, which tenants reach; and the Proxmox API URL as machines on this network reach it.
- **Seed.** Its node, address and size, and the networks that may reach its API: at least the address ProxCloud reaches it from. SSH keys are optional and for emergencies only.
- **Control plane.** Its node, its size and the number of machines: 1 or 3. This is the platform cluster's own control plane, not a tenant's. Three machines keep its API up when one of their VMs is lost or is replaced during an update. They all run on the one node you name and on one store, so they do not protect against the loss of that node or of its disk, and three write more to that store than one. The node needs memory for one machine more than the count, because an update makes the new machine before it removes an old one; a machine that does not fit is refused and not tried again, and the **Capacity** check tells you before the install. You can go from 1 to 3 later (save, then **Apply this release**); a 3 you saved and did not apply goes back to 1, and once an apply has built three, back to 1 is not possible without removing the installation. Machines on several nodes are not built in this release. Three need a platform release whose cluster chart is 0.1.14 or newer; the field says so when the release is older. Not verified on a lab.
- **Storage.** One boot store; one ISO store that is local to each node; optionally the store images are downloaded to, which must not be the boot store. Left empty it is the provider's import store; if that is the boot store the save is refused, and both the provider and this field must name another store.

The page checks the addresses as you save: everything inside the network, no overlaps, and a machine range that is large enough. ProxCloud then checks what only it knows, for example that the network does not overlap the address pool of tenant networks.

After the first successful install these are fixed: the name, the network, the seed's node and address, and the API address. To change them, remove the installation and install again.

## Step 2: Proxmox identities

Copy the script and run it as root on one node. It adds two users with one token each, a pool for the seed, and their permissions. It prints each token secret once.

Paste the two secrets and press **Store the secrets**. They are stored encrypted and are never shown again; to replace one, paste a new one. If you change the boot store or the ISO store, run the script again.

## Step 3: Check

Reads Proxmox with the stored tokens and changes nothing. Each failed check comes with what to do. **Install** runs the same checks first, so you cannot skip them. A **Warning** does not stop the install; the page then says "No check failed" and how many warnings there are.

- **Tenant network** (a warning). The provider's default zone has DHCP in Proxmox (a simple zone that ProxCloud created has), or will be created with it. Tenants cannot create a Kubernetes cluster on its networks until the default zone is an EVPN or a VLAN zone, or a simple zone without DHCP; change it on the provider's page under **Cloud providers**. A Kubernetes provider with shared workers is not affected.
- **Tenant path** (a warning). A Proxmox node translates tenant traffic (the provider's default zone is a simple zone, or an EVPN zone with this node as exit node), runs a platform machine too, and does not route the platform network itself. A tenant's TCP connection to the platform network is then dropped on that node: the workers of tenant Kubernetes clusters run and never join, and tenants cannot reach managed databases. The install is not affected. Two layouts are supported: on a new one-node site, a platform network the node routes (a vnet of a simple zone whose subnet has the platform gateway); on an existing site, a leg of the node on the platform bridge, added by hand as root (run on one production site, not lab-verified, lost at a reboot). The steps are in `docs/managed-services.md`, "The path from tenants to the platform". The check reads the configuration only: it opens no connection, a passing row is not proof, and the warning stays after you added the leg. For a platform network routed by the node the row passes. Workers that did not join are not retried: the tenant reduces the node group's count by one and raises it again, once per worker (a count below one is refused: a group of one is raised by one first and reduced when the new worker is ready).
- **Bridge.** A node the settings place a machine on has no bridge or vnet of the name in step 1, or, for a network routed by the node, Proxmox already has a vnet of that name or a node has an interface of that name (a vnet named like `vmbr1` would take that interface over, so choose a name no node has). Correct the bridge or the nodes; apply a vnet you made in Proxmox (Datacenter, SDN, Apply). On an installed platform the same finding is a warning.
- **Test the tenant path** (a button below the checks, on an installed platform). The checks read; this test connects, and it is the only one that changes anything in Proxmox. It makes a small network in the tenant zone and a small VM on the node you choose, connects from that VM to what listens in the load balancer range (a tenant cluster's API, a database), and removes both again. It costs one VM clone and two SDN applies and takes a few minutes, so it never runs with **Check**. It needs something to connect to (create one database or cluster first) and a published Ubuntu or Debian template on that node (publish one in **Template updates**; with the scheduler off nothing is published by itself). **Passed**: the path is open from that node. **Failed**: the ProxCloud host reaches the address and the tenant network does not, so workers on that node never join and tenants cannot reach databases; or the VM reached the platform's own API address, which tenants must not. **Error**: the test could not be done and nothing is known. One test covers one node: on a site with several nodes, test each node that runs a platform machine. With a VLAN zone as the provider's default zone the test is refused: no Proxmox node translates tenant traffic there, and a test network would need its own VLAN on your router. If the cleanup could not remove something, the page lists it with the command. A test that was cut off by a ProxCloud restart shows as an error 12 minutes after its start; test again, and the next test first removes what the earlier one left. When only a database answered and the test VM could not open it, the database's allowed networks may be the reason, and the message says so. Not verified on a lab.
- **Capacity.** A node has less memory than the settings place on it. The count includes the one more machine an update of the control plane makes. When that fits and one more machine of each group on the node does not, the check is a warning and the install goes on: you need that room later, when you change a group's size. The count is made as the machines are counted when they are cloned: every VM and container on the node counts with its configured memory, running or not. Use smaller or fewer machines, one control plane machine in place of three, or other nodes, or free memory on the node; then save and check again. On an installed platform this is a warning.
- **Machines** (a warning). A VM of the platform is stopped, or does not start when its Proxmox node boots. The message names each VM with its node and the command. Start a stopped VM in Proxmox; **Apply this release** sets start at boot on every machine. After a reboot of a Proxmox node, run this check: machines that were built before this release start only once an apply has run on it.

- **Isolation.** The message lists the smallest address ranges missing from **Networks tenants must not reach**; one network that covers them works as well, as long as the load balancer range stays outside it. The firewall rules are then written by a provider sync, which the row's button **Run a provider sync, then check again** starts. No project or tenant network is needed for that, also when another ProxCloud uses the same Proxmox cluster: the sync does this for a provider whose installation is saved (step 1), and for no other. Five messages name a cause that no sync repairs while the provider has no project: the tenant address pool overlaps a management network (choose another pool on the provider's page); the rules in place are for another pool; the rules in place drop a network that this provider's settings do not name; the rules in place accept other sandbox endpoints than this provider's; or the rules in place are not this provider's set while a project's rules for the pool are in place (that project's sync maintains them). In the last four cases another ProxCloud or provider on the cluster uses the same pool prefix (give this provider its own), or the rules are this provider's own from before a change of its pool or its networks (delete the rules with its comment in Proxmox under Datacenter, Firewall, then run a provider sync; changed sandbox endpoints count as such a change). The sync also writes nothing while a Proxmox node does not report its networks, and nothing for a disabled provider: the message then says that the provider is disabled, and the fix is to enable it under **Cloud providers** and run a provider sync.
- **Token scope** and **ISO store visibility.** One of the two tokens holds a permission from an older setup that reaches more than the platform. The message gives the `pveum acl delete` command to run as root on one node; then press **Check** again. If `pveum acl list` shows no permission of the token on the path the message names, or above it with propagate, remove none (the token's permissions on the platform pools and stores are listed too, and stay): the token was made without privilege separation. Run the `pveum user token modify` command of the message, then the script again. If the ISO store message names the boot store, remove nothing: name another boot store, or take ISO images out of that store's content.

## Step 4: Review and install

Shows the release, its components, what is created and, for each node, the cores and memory its machines take, the memory with the one more machine an update of the control plane makes, the memory with one more of each group as well, and the memory Proxmox reports as free on that node now. "That is not enough" marks a node with less free than the machines and the control plane's one more need. Free memory is what is not in use at this moment; the **Capacity** check in step 3 is the one that decides.

**Install** starts a run. You can leave the page; the run goes on.

## While a run is going

The page lists the stages and reads the run every few seconds:

1. Check the settings and Proxmox
2. Import the two images
3. Create the seed VM
4. Reach the seed
5. Secure the seed
6. Install the seed's components
7. Build the platform cluster
8. Reach the platform cluster
9. Install the platform components
10. Create the two providers
11. Finish

- **Cancel the run** usually takes effect within about 15 seconds, and up to 5 minutes while a stage pauses after repeated errors. A task Proxmox has started finishes first. Cancelling an installation deletes nothing. Cancelling a removal does not bring back what it had already deleted: the stage list shows how far it came, and **Remove again** goes on from there.
- **Retry** starts a new run. It looks at what exists and goes on from there. Nothing is deleted when an installation run fails, with one exception: a seed that never became ready is built again. **Retry** runs with the saved settings, so save a change first.
- A stage that did not work yet and is trying again shows why and what you can do meanwhile, before its time limit ends the run. A cluster that has just started (the seed after a restart) refuses ProxCloud's credential for some seconds; the stage says so and tries again, and fails only when it is refused five times on end (about four minutes).
- If ProxCloud itself stopped during a run, the run is shown as failed with `job_dead`. **Retry** or **Remove managed services** goes on; until then the settings cannot be saved.

## When a run fails

The failed stage shows a code, a message and what to do.

| Code | What to do |
|---|---|
| `preflight_failed` | Open the failed checks of the stage. Run the script again or correct the settings, then **Check**. |
| `platform_network_refused` | Only for a network routed by the node: Proxmox refused the new vnet, or its name or the zone `pcplat` is taken by something else. Correct what the message names in Proxmox (Datacenter, SDN) or name another vnet in the settings, then **Retry**. |
| `isolation_not_enforced` | Add the networks named in the message to **Networks tenants must not reach** on the provider's page under **Cloud providers**. Then press **Check**: its isolation row links to that page and has the button **Run a provider sync, then check again**. Check too that the datacenter firewall is on. |
| `image_import_failed`, `image_import_stalled`, `image_blocked` | Open **Template updates** to see the import. A blocked image needs a newer ProxCloud release on a new installation; an installed platform stays on its release's images, and no release changes them yet. |
| `seed_clone_failed` | Fix the storage or the node named in the message, then **Retry**. |
| `seed_config_refused` | The message carries Proxmox's own error: fix the bridge, the store, the node or the token right it names, then **Retry**. A CD that could not be rendered or built is a defect: report the message. |
| `seed_not_booted` | Open the seed VM's console in Proxmox. **Retry** builds a new seed. |
| `seed_api_unreachable` | Add a route from the ProxCloud host to the platform network, or correct the seed API sources. When the message says the cluster's API refuses the credential: it does so for some seconds after it starts; if the run failed on it, look on the seed VM's console whether its Kubernetes keeps restarting (a slow disk does that), then **Retry**. |
| `seed_tls_mismatch` | Another server answers on the seed address. Correct the seed address in the settings, then **Retry**. |
| `seed_identity_lost` | **Retry**. It builds the seed again. |
| `seed_cd_not_deleted` | Run the script again, then **Retry**. |
| `seed_lost` | The seed VM is gone. Remove the installation with the option for a missing seed, then install again. |
| `component_unhealthy`, `capi_init_failed` | The component's row shows what its sync failed on, for example a job and its message. Download the seed kubeconfig to look closer, then **Retry**. The run starts a failed sync again itself, once; no manual sync is needed. |
| `machine_failed` | The message carries the cluster manager's own text: correct what it names (memory on the node, the boot store, the template). The machine is not tried again on its own: download the seed kubeconfig, run the `kubectl … delete machine` command of the message, then **Retry**. If the text says a VM's name does not match, do not delete the machine: its number belongs to another VM. If the message names a node template with a cloud-init drive, run its `qm set` command on that node, then **Retry**; the template then has no cloud-init drive, and its smoke test in Template updates adds one to its own test VM. |
| `capmox_auth_failed` | Proxmox refused the token, or the seed cannot reach the Proxmox API URL of the settings. Check the URL as the platform network reaches it and paste the token secret again, then **Retry**. The message names the log that says which. |
| `workload_api_unreachable` | Add a route from the ProxCloud host to the API address, then **Retry**. When the message says the cluster's API refuses the credential: it does so for some seconds after it starts; if the run failed on it, **Retry**. During a removal: restore the route and **Remove again**, or tick the option for a cluster that does not answer when it is beyond repair. |
| `provider_conflict` | Correct the provider named in the message under **Cloud providers**, then **Retry**. |
| `upgrade_refused` | The upgrade to this ProxCloud's platform release is not possible: step 4 lists why. The platform stays on its release and **Apply this release** keeps working. |
| `unexpected_rollout` | An upgrade would replace machines of the platform cluster. Nothing was synced. If you changed machine sizes or node groups together with the upgrade, put them back and **Retry**; otherwise install the ProxCloud release that corrects it: step 4 then shows **Upgrade to release X instead**. |
| `tenants_unhealthy` | During an upgrade a tenant cluster or database that was healthy before is not. Look at the one the message names. **Retry** waits for it again: the upgrade goes on only when it is healthy or deleted. |
| `bom_unsupported` | The platform was installed by a ProxCloud whose platform release this one does not carry (a development build, or a release from before 0.8.0), and a platform moves only between releases this ProxCloud carries. Go back to the ProxCloud that installed it. Or, if no tenant cluster or database must be kept, delete them all, then remove managed services and install again: **Remove** deletes the platform cluster. |
| `credentials_missing` | Paste both token secrets in step 2, then **Retry**, or **Remove again** for a removal. |
| `stage_unavailable` | The run was recorded by another ProxCloud release. No action in this release. |
| `tenants_exist` | Delete the tenant clusters and databases, then **Remove** again. |
| `machine_delete_stuck`, `volume_delete_failed` | Delete the VM or fix the storage named in the message, then **Remove** again. |
| a code ending in `_timeout`, `portal_changed`, `job_dead` | **Retry**, or **Remove again** for a removal. The time limits start again. |

## When it is ready

The page shows each component with its live state, read from the seed. Below that:

- **Endpoints.** The address of the seed's API, the address of the platform cluster's API, the load balancer range and the range of the machines. All are on the platform network; the seed's API answers only for the seed API sources of the settings.
- **Providers.** The two providers with a link each, and what tenants cannot use yet. The install sets no Kubernetes version on the Kubernetes provider, so the row says "Offers no Kubernetes version yet" until you enter one. Both providers are created held back: the row says so until you turn on the policy for new projects on the provider's **Projects** tab. Projects that existed before are attached on that tab too; the page does not read whether they are.
- **Platform cluster access.** **Download platform kubeconfig** saves a kubeconfig that works for one hour. With `kubectl` it reads the cluster, without its Secrets, and changes nothing. It also opens the cluster's ArgoCD, whose admin can change everything on the cluster, so keep the file like an admin credential. The download is written to the audit log.
- **Open ArgoCD.** There are two, one on the platform cluster (every tenant cluster and database) and one on the seed (the platform's own components). This release gives neither an address. The page prints two commands for each: one prints the password of user `admin`, the other forwards a local port; then open `http://localhost:8080` (platform cluster) or `http://localhost:8081` (seed). Run them on a machine that has `kubectl` and reaches the API address shown under Endpoints. The platform cluster's needs the platform kubeconfig, the seed's the seed kubeconfig. What you change by hand on a component, the next **Apply this release** writes back.

Then:

1. Publish a Kubernetes node image for tenants in **Template updates** and enter the line it prints for the image (`<version>=<node image id>`) on the Kubernetes provider's **Connection** tab. Until then tenants cannot create a cluster.
2. Add a backup job for the seed VM. The page prints the command: a daily snapshot, compressed, of which seven are kept. The backup holds the seed's credentials: use a backup store that is not the boot, ISO or import store of the settings, and that no API token other than your own can read. The operator guide (docs/managed-services.md, "Credentials and the seed backup") gives the checks. The page cannot see whether a job exists.
3. Attach the providers to projects on each provider's **Projects** tab, and add price rules for clusters and databases.

**Apply this release** in step 4 runs the stages again with the saved settings: it repairs what is missing, applies changed sizes and counts, and renews the seed's access to the platform cluster. A platform stays on the platform release that installed it until you upgrade it: after a ProxCloud upgrade the review lists that release's contents, and **Apply this release** runs with them. A platform installed by a development build is not kept this way: after an upgrade its run is refused with `bom_unsupported`.

## Upgrading to a newer platform release

After a ProxCloud upgrade, step 4 compares the installed platform release with the one this ProxCloud carries and shows one of three things. None of it has run on a lab yet.

- **Upgrade to release X**, with a table of what differs and what the run does for each entry. This release moves a platform only when the two releases differ in ProxCloud's own charts. Tenant clusters and databases keep running. The button asks you to type `upgrade <name> to <release>`. Take a backup of the seed VM and a dump of the portal database first, and do not change machine sizes or node groups in the same step.
- **The platform stays on release Y**, with the reasons. A release that changes the Kubernetes version, an image, k3s, a Cluster API provider or a third-party chart cannot be moved to yet. Nothing is wrong with the platform, and **Apply this release** keeps working.
- **The upgrade to release X has not finished**: a run toward it started and did not succeed. **Retry** and **Apply this release** both continue to release X. Nothing goes back to release Y, which the record names until a run has succeeded. When release X belongs to an earlier ProxCloud than the one installed now, a second box, **Upgrade to release Z instead**, offers the move to this ProxCloud's release in its place: use it when the upgrade to X cannot finish.

The run has the same stages as an install. It adds a check before the platform cluster's chart is synced (it fails with `unexpected_rollout` instead of replacing machines) and, between the steps of stage 9, a wait until the tenants that were healthy before are healthy again (`tenants_unhealthy` while they are not). The operator guide (docs/managed-services.md, "Upgrade between BOMs") has the details and what to do when an upgrade cannot finish.

## Seed access and removal

- **Download seed kubeconfig** gives full access to the seed for one hour, for looking closer when a component fails. The download is written to the audit log.
- **Replace the seed credential** gives ProxCloud a new credential for the seed and ends the old one, a few seconds later. The audit log records it as `ROTATE_CREDENTIAL` with the names of the old and the new token Secret, also when it stopped after the new credential was stored (outcome `FAILURE`; press it again). It does not end a seed kubeconfig that was downloaded before: that file stays valid until its hour is over.
- If someone else saved the settings while you were editing them, your **Save** is refused: the page reads the settings again and asks you to enter your change once more.
- **Remove managed services** deletes the platform cluster, its volumes and the seed VM and disables the two providers. The next install enables the ones the removal disabled again; a provider you disabled yourself stays disabled. With the extra option and the seed VM gone, a machine that was still being copied is not deleted: the stage lists it, and you delete it in Proxmox. You type `remove <name>` to confirm. It is refused while tenant clusters or databases exist. The finished removal lists under each stage what it deleted (each machine with its VM, the volumes, the seed VM, the stored secrets) and what it could not check. Tick the extra option only when the seed VM is already gone or the cluster no longer answers. ProxCloud then cannot see whether tenant clusters or databases still run there: any that do are deleted with the cluster's machines and volumes. Afterwards the page prints the two commands that end the tokens; they are yours to run. The removal deletes no rule of the datacenter firewall: the provider's isolation rules stay, and the provider sync keeps them up to date while the removed installation is on record. If you later take the platform's addresses out of **Networks tenants must not reach** on a provider with no project, delete the rules with the provider's comment in Proxmox under Datacenter, Firewall, then run a provider sync.

## When the seed does not answer

The page reads the seed each time it loads. When the seed cannot be read, the page shows **Needs attention** and says whether the seed's address does not answer or refuses ProxCloud's credential, with the seed VM's number and its node. The installation still counts as ready everywhere else, and nothing sends a notification.

The platform cluster, tenant clusters and databases keep running without the seed. What stops: a failed platform machine is not replaced, and no run can finish.

1. In Proxmox, check that the seed VM runs. **Apply this release** starts a stopped seed; it never rebuilds one.
2. On the seed's node, as root: `qm guest exec <vmid> -- systemctl is-active k3s` and `qm guest exec <vmid> -- grep ' / ' /proc/mounts`.
3. If the line for `/` ends in `emergency_ro`, the seed's disk takes no writes. The known cause is a defect of the Ubuntu 26.04 images exported before 2026-11-16. Do not run a plain `e2fsck -f` on it: that deletes a file and leaves the fault. Snapshot the disk, shut the VM down and run the three-command repair of the operator guide (docs/managed-services.md, "When the seed does not answer", step 4) on the root partition and on `/boot`, then start the VM. Then press **Retry** if an apply was started meanwhile and failed, or reload the page if none was. This repair was run on a lab on 2026-11-17 and brought the seed back.
4. If that does not help, restore the seed VM from your backup. Take only an archive made after the last time **Replace the seed credential** was used (the audit log has a `ROTATE_CREDENTIAL` entry for each): a seed restored from an older one refuses ProxCloud's credential and cannot be brought back, so run a backup by hand after every replacement. The seed is protected and Proxmox refuses a restore over a protected VM, so on the seed's node, as root: `cp /etc/pve/firewall/<vmid>.fw /root/<vmid>.fw.before-restore`, `qm shutdown <vmid>`, `qm set <vmid> --protection 0`, `qmrestore <archive> <vmid> --force 1 --storage <boot store>`. Keep the same VMID and never add `--unique`: the seed's MAC and address must stay. Before the start check `qm config <vmid>` (the same `net0` MAC, `protection: 1`; set it back with `qm set <vmid> --protection 1` if it is missing) and that `/etc/pve/firewall/<vmid>.fw` exists and equals the copy; if it is missing or differs, copy the saved file back before the start. Then `qm start <vmid>` and **Apply this release**. This restore has not been run on a lab; only Proxmox's refusal over the protected seed was seen.
5. Without a backup the seed cannot be brought back. Delete the seed VM in Proxmox, then use **Remove managed services** with the extra option and install again. This deletes the platform cluster with every tenant control plane and database on it.

While the seed does not answer, **Apply this release** waits 20 minutes and fails, **Remove managed services** fails without deleting the cluster, and the two seed buttons fail. A run that fails sets the installation to **Failed**: the page then shows the failed run instead of the seed's live state, and after the seed is repaired **Retry** brings it back to ready. Reboot a seed with `qm reboot`, never `qm stop`, and add the backup job before you need it.

## What this release does not do

- Tenant Kubernetes clusters get no block volumes on a one-host install.
- Tenant databases are not backed up.
- One shared cluster only. Separate clusters for Kubernetes and for databases come in a later release.
- The platform network is untagged: no VLAN.
