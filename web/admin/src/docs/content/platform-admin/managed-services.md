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

Tenant Kubernetes clusters also need a network zone without DHCP: the Proxmox provider's default zone must be an EVPN or a VLAN zone. On a simple zone the install works and databases work, but every cluster a tenant creates is refused. Step 3 warns about it.

The page also warns when the job scheduler is off: tenant clusters and databases then update their status only when a provider sync runs.

No check of step 3 reads the following, so look before the first run:

- The seed and the machines reach the Proxmox API URL of the settings from the platform network. With the datacenter firewall on, a node accepts its API port only from its own management network unless you add the platform network there.
- Each node has memory left for its machines: the cluster manager counts the configured memory of every VM it sees on the node, running or stopped.
- The platform bridge is one network across the nodes, not a separate NAT network on each.
- The boot store has room for a full copy of every machine's disk, on disks that write fast: the seed and the control plane stop working when a write takes seconds.
- The Proxmox provider names its region.

## Step 1: Settings

Saving keeps a draft. Nothing is created on Proxmox until you press **Install**, and a draft can be changed as often as you like.

- **Name.** Lower-case letters, digits and dashes. The two providers are named after it.
- **Node groups.** Three groups of worker machines: `system` (the platform's own components), `kamaji` (the control planes of tenant Kubernetes clusters) and `dbaas` (tenant databases). For each: the Proxmox nodes, the number of machines (at least one per chosen node) and their size. **Use the lab profile** sets the smallest size the installer accepts.
- **Platform network.** The bridge, the network, gateway and DNS; the address of the cluster's API; a range for the machines (count one spare per group and node, used during updates); a range for load balancers, which tenants reach; and the Proxmox API URL as machines on this network reach it.
- **Seed.** Its node, address and size, and the networks that may reach its API: at least the address ProxCloud reaches it from. SSH keys are optional and for emergencies only.
- **Control plane** and **storage.** One control plane machine; one boot store; one ISO store that is local to each node; optionally the store images are downloaded to, which must not be the boot store. Left empty it is the provider's import store; if that is the boot store the save is refused, and both the provider and this field must name another store.

The page checks the addresses as you save: everything inside the network, no overlaps, and a machine range that is large enough. ProxCloud then checks what only it knows, for example that the network does not overlap the address pool of tenant networks.

After the first successful install these are fixed: the name, the network, the seed's node and address, and the API address. To change them, remove the installation and install again.

## Step 2: Proxmox identities

Copy the script and run it as root on one node. It adds two users with one token each, a pool for the seed, and their permissions. It prints each token secret once.

Paste the two secrets and press **Store the secrets**. They are stored encrypted and are never shown again; to replace one, paste a new one. If you change the boot store or the ISO store, run the script again.

## Step 3: Check

Reads Proxmox with the stored tokens and changes nothing. Each failed check comes with what to do. **Install** runs the same checks first, so you cannot skip them. A **Warning** does not stop the install; the page then says "No check failed" and how many warnings there are.

- **Tenant network** (a warning). The provider's default zone is a simple zone, whose networks have DHCP. Tenants cannot create a Kubernetes cluster until the default zone is an EVPN or a VLAN zone; change it on the provider's page under **Cloud providers**.

- **Isolation.** The message lists the smallest address ranges missing from **Networks tenants must not reach**; one network that covers them works as well, as long as the load balancer range stays outside it. The firewall rules are then written by a provider sync, which the row's button **Run a provider sync, then check again** starts. No project or tenant network is needed for that, also when another ProxCloud uses the same Proxmox cluster: the sync does this for a provider whose installation is saved (step 1), and for no other. Five messages name a cause that no sync repairs while the provider has no project: the tenant address pool overlaps a management network (choose another pool on the provider's page); the rules in place are for another pool; the rules in place drop a network that this provider's settings do not name; the rules in place accept other sandbox endpoints than this provider's; or the rules in place are not this provider's set while a project's rules for the pool are in place (that project's sync maintains them). In the last four cases another ProxCloud or provider on the cluster uses the same pool prefix (give this provider its own), or the rules are this provider's own from before a change of its pool or its networks (delete the rules with its comment in Proxmox under Datacenter, Firewall, then run a provider sync; changed sandbox endpoints count as such a change). The sync also writes nothing while a Proxmox node does not report its networks, and nothing for a disabled provider: the message then says that the provider is disabled, and the fix is to enable it under **Cloud providers** and run a provider sync.
- **Token scope** and **ISO store visibility.** One of the two tokens holds a permission from an older setup that reaches more than the platform. The message gives the `pveum acl delete` command to run as root on one node; then press **Check** again. If `pveum acl list` shows no permission of the token on the path the message names, or above it with propagate, remove none (the token's permissions on the platform pools and stores are listed too, and stay): the token was made without privilege separation. Run the `pveum user token modify` command of the message, then the script again. If the ISO store message names the boot store, remove nothing: name another boot store, or take ISO images out of that store's content.

## Step 4: Review and install

Shows the release, its components, what is created and how many cores and how much memory each node needs. Compare that with the free memory of your nodes: the page does not read it.

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
| `isolation_not_enforced` | Add the networks named in the message to **Networks tenants must not reach** on the provider's page under **Cloud providers**. Then press **Check**: its isolation row links to that page and has the button **Run a provider sync, then check again**. Check too that the datacenter firewall is on. |
| `image_import_failed`, `image_import_stalled`, `image_blocked` | Open **Template updates** to see the import. A blocked image needs a newer ProxCloud release. |
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
| `bom_unsupported` | The platform was installed by another ProxCloud release, and no release upgrades an installed platform yet. Go back to the ProxCloud release that installed it. Or, if no tenant cluster or database must be kept, delete them all, then remove managed services and install again: **Remove** deletes the platform cluster. |
| `credentials_missing` | Paste both token secrets in step 2, then **Retry**, or **Remove again** for a removal. |
| `stage_unavailable` | The run was recorded by another ProxCloud release. No action in this release. |
| `tenants_exist` | Delete the tenant clusters and databases, then **Remove** again. |
| `machine_delete_stuck`, `volume_delete_failed` | Delete the VM or fix the storage named in the message, then **Remove** again. |
| a code ending in `_timeout`, `portal_changed`, `job_dead` | **Retry**, or **Remove again** for a removal. The time limits start again. |

## When it is ready

The page shows each component with its live state, read from the seed, and the two providers. They are created held back: new projects get them only after you attach them. Then:

1. Publish a Kubernetes node image for tenants in **Template updates** and enter its version on the Kubernetes provider. Until then tenants cannot create a cluster.
2. Add a backup job for the seed VM. The page prints the command. Use a backup store that is not the boot, ISO or import store of the settings. The page cannot see whether a job exists.
3. Attach the providers to projects, and add price rules for clusters and databases.

**Apply this release** in step 4 runs the stages again with the saved settings: it repairs what is missing, applies changed sizes and counts, and renews the seed's access to the platform cluster. This release cannot yet move an installed platform to the components of a newer ProxCloud release: after such an upgrade the review says so and the run is refused with `bom_unsupported`.

## Seed access and removal

- **Download seed kubeconfig** gives full access to the seed for one hour, for looking closer when a component fails. The download is written to the audit log.
- **Replace the seed credential** gives ProxCloud a new credential for the seed and ends the old one, a few seconds later. The audit log records it as `ROTATE_CREDENTIAL` with the names of the old and the new token Secret. It does not end a seed kubeconfig that was downloaded before: that file stays valid until its hour is over.
- If someone else saved the settings while you were editing them, your **Save** is refused: the page reads the settings again and asks you to enter your change once more.
- **Remove managed services** deletes the platform cluster, its volumes and the seed VM and disables the two providers. The next install enables the ones the removal disabled again; a provider you disabled yourself stays disabled. With the extra option and the seed VM gone, a machine that was still being copied is not deleted: the stage lists it, and you delete it in Proxmox. You type `remove <name>` to confirm. It is refused while tenant clusters or databases exist. The finished removal lists under each stage what it deleted (each machine with its VM, the volumes, the seed VM, the stored secrets) and what it could not check. Tick the extra option only when the seed VM is already gone or the cluster no longer answers. ProxCloud then cannot see whether tenant clusters or databases still run there: any that do are deleted with the cluster's machines and volumes. Afterwards the page prints the two commands that end the tokens; they are yours to run. The removal deletes no rule of the datacenter firewall: the provider's isolation rules stay, and the provider sync keeps them up to date while the removed installation is on record. If you later take the platform's addresses out of **Networks tenants must not reach** on a provider with no project, delete the rules with the provider's comment in Proxmox under Datacenter, Firewall, then run a provider sync.

## When the seed does not answer

The page reads the seed each time it loads. When the seed cannot be read, the page shows **Needs attention** and says whether the seed's address does not answer or refuses ProxCloud's credential, with the seed VM's number and its node. The installation still counts as ready everywhere else, and nothing sends a notification.

The platform cluster, tenant clusters and databases keep running without the seed. What stops: a failed platform machine is not replaced, and no run can finish.

1. In Proxmox, check that the seed VM runs. **Apply this release** starts a stopped seed; it never rebuilds one.
2. On the seed's node, as root: `qm guest exec <vmid> -- systemctl is-active k3s` and `qm guest exec <vmid> -- grep ' / ' /proc/mounts`.
3. If the line for `/` ends in `emergency_ro`, the seed's disk has a filesystem error and takes no writes. Snapshot the disk, shut the VM down, check the filesystem from the node (`e2fsck -f` on the disk's first partition) and start the VM. This repair has not been run on a lab.
4. If that does not help, restore the seed VM from your backup, then **Apply this release**.
5. Without a backup the seed cannot be brought back. Delete the seed VM in Proxmox, then use **Remove managed services** with the extra option and install again. This deletes the platform cluster with every tenant control plane and database on it.

While the seed does not answer, **Apply this release** waits 20 minutes and fails, **Remove managed services** fails without deleting the cluster, and the two seed buttons fail. Reboot a seed with `qm reboot`, never `qm stop`, and add the backup job before you need it.

## What this release does not do

- Tenant Kubernetes clusters get no block volumes on a one-host install.
- Tenant databases are not backed up.
- One shared cluster only. Separate clusters for Kubernetes and for databases come in a later release.
- The platform network is untagged: no VLAN.
