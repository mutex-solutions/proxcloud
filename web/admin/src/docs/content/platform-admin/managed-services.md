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

The page also warns when the job scheduler is off: tenant clusters and databases then update their status only when a provider sync runs.

## Step 1: Settings

Saving keeps a draft. Nothing is created on Proxmox until you press **Install**, and a draft can be changed as often as you like.

- **Name.** Lower-case letters, digits and dashes. The two providers are named after it.
- **Node groups.** Three groups of worker machines: `system` (the platform's own components), `kamaji` (the control planes of tenant Kubernetes clusters) and `dbaas` (tenant databases). For each: the Proxmox nodes, the number of machines (at least one per chosen node) and their size. **Use the lab profile** sets the smallest size the installer accepts.
- **Platform network.** The bridge, the network, gateway and DNS; the address of the cluster's API; a range for the machines (count one spare per group and node, used during updates); a range for load balancers, which tenants reach; and the Proxmox API URL as machines on this network reach it.
- **Seed.** Its node, address and size, and the networks that may reach its API: at least the address ProxCloud reaches it from. SSH keys are optional and for emergencies only.
- **Control plane** and **storage.** One control plane machine; one boot store; one ISO store that is local to each node; optionally the store images are downloaded to.

The page checks the addresses as you save: everything inside the network, no overlaps, and a machine range that is large enough. ProxCloud then checks what only it knows, for example that the network does not overlap the address pool of tenant networks.

After the first successful install these are fixed: the name, the network, the seed's node and address, and the API address. To change them, remove the installation and install again.

## Step 2: Proxmox identities

Copy the script and run it as root on one node. It adds two users with one token each, a pool for the seed, and their permissions. It prints each token secret once.

Paste the two secrets and press **Store the secrets**. They are stored encrypted and are never shown again; to replace one, paste a new one. If you change the boot store or the ISO store, run the script again.

## Step 3: Check

Reads Proxmox with the stored tokens and changes nothing. Each failed check comes with what to do. **Install** runs the same checks first, so you cannot skip them.

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

- **Cancel the run** takes effect within about 15 seconds. A task Proxmox has started finishes first. Cancelling an installation deletes nothing. Cancelling a removal does not bring back what it had already deleted: the stage list shows how far it came, and **Remove again** goes on from there.
- **Retry** starts a new run. It looks at what exists and goes on from there. Nothing is deleted when an installation run fails, with one exception: a seed that never became ready is built again. **Retry** runs with the saved settings, so save a change first.
- A stage that did not work yet and is trying again shows why and what you can do meanwhile, before its time limit ends the run.
- If ProxCloud itself stopped during a run, the run is shown as failed with `job_dead`. **Retry** or **Remove managed services** goes on; until then the settings cannot be saved.

## When a run fails

The failed stage shows a code, a message and what to do.

| Code | What to do |
|---|---|
| `preflight_failed`, `privileges_missing`, `network_problem`, `iso_store_visibility` | Run the script again or correct the settings, then **Check**. |
| `isolation_not_enforced` | Add the networks named in the message to **Networks tenants must not reach** on the provider's page under **Cloud providers**. Then press **Check**: its isolation row links to that page and has the button **Run a provider sync, then check again**. Check too that the datacenter firewall is on. |
| `image_import_failed`, `image_import_stalled`, `image_blocked` | Open **Template updates** to see the import. A blocked image needs a newer ProxCloud release. |
| `seed_clone_failed`, `seed_config_refused` | Fix the storage or the node named in the message, then **Retry**. |
| `seed_not_booted` | Open the seed VM's console in Proxmox. **Retry** builds a new seed. |
| `seed_api_unreachable` | Add a route from the ProxCloud host to the platform network, or correct the seed API sources. |
| `seed_tls_mismatch` | Another server answers on the seed address. Correct the seed address in the settings, then **Retry**. |
| `seed_identity_lost` | **Retry**. It builds the seed again. |
| `seed_cd_not_deleted` | Run the script again, then **Retry**. |
| `seed_lost` | The seed VM is gone. Remove the installation with the option for a missing seed, then install again. |
| `component_unhealthy`, `capi_init_failed` | Download the seed kubeconfig to look closer, then **Retry**. |
| `machine_failed`, `capmox_auth_failed` | Check the boot store, the nodes, the Proxmox API URL and the token secret, then **Retry**. |
| `workload_api_unreachable` | Add a route from the ProxCloud host to the API address, then **Retry**. During a removal: restore the route and **Remove again**, or tick the option for a cluster that does not answer when it is beyond repair. |
| `provider_conflict` | Correct the provider named in the message under **Cloud providers**, then **Retry**. |
| `bom_unsupported` | The installed version is too old. Upgrade through the previous ProxCloud release first. |
| `tenants_exist` | Delete the tenant clusters and databases, then **Remove** again. |
| `machine_delete_stuck`, `volume_delete_failed` | Delete the VM or fix the storage named in the message, then **Remove** again. |
| a code ending in `_timeout`, `portal_changed`, `job_dead` | **Retry**, or **Remove again** for a removal. The time limits start again. |

## When it is ready

The page shows each component with its live state, read from the seed, and the two providers. They are created held back: new projects get them only after you attach them. Then:

1. Publish a Kubernetes node image for tenants in **Template updates** and enter its version on the Kubernetes provider. Until then tenants cannot create a cluster.
2. Add a backup job for the seed VM. The page prints the command. It cannot see whether a job exists.
3. Attach the providers to projects, and add price rules for clusters and databases.

After a ProxCloud upgrade, **Apply this release** in step 4 brings the components to the new versions. The review lists what changes and refuses a jump that is not supported.

## Seed access and removal

- **Download seed kubeconfig** gives full access to the seed for one hour, for looking closer when a component fails. The download is written to the audit log.
- **Replace the seed credential** gives ProxCloud a new credential for the seed and ends the old one.
- If someone else saved the settings while you were editing them, your **Save** is refused: the page reads the settings again and asks you to enter your change once more.
- **Remove managed services** deletes the platform cluster, its volumes and the seed VM and disables the two providers. You type `remove <name>` to confirm. It is refused while tenant clusters or databases exist. Tick the extra option only when the seed VM is already gone or the cluster no longer answers. ProxCloud then cannot see whether tenant clusters or databases still run there: any that do are deleted with the cluster's machines and volumes. Afterwards the page prints the two commands that end the tokens; they are yours to run.

## What this release does not do

- Tenant Kubernetes clusters get no block volumes on a one-host install.
- Tenant databases are not backed up.
- One shared cluster only. Separate clusters for Kubernetes and for databases come in a later release.
- The platform network is untagged: no VLAN.
