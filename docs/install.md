# Install ProxCloud

`installer/` orchestrates the platform installation from a site inventory. Proxmox API access alone is **not** enough: the selected path also needs management-host SSH/sudo, reachable storage/network/DNS, an unused OS-build address and the dependencies below. Split services and sandboxes require additional prepared templates and network settings; defaults do not allocate safe site addresses for you.

```sh
cd installer
cp -r inventory/example inventory/mysite
$EDITOR inventory/mysite/hosts.yml inventory/mysite/group_vars/all/proxcloud.yml inventory/mysite/group_vars/all/vault.yml
make preflight INVENTORY=inventory/mysite
make install INVENTORY=inventory/mysite
```

The stages are designed for convergent re-runs, but `make install` is **not** a read-only check: changed pins/site values can reconcile resources, and some stages execute operational commands. Review the selected playbooks, inventory and migration gates before re-running on an existing site. A re-run preserves the Proxmox provider's console-set storage profiles, default profile and confirmed classes unless site group_vars override them; network, API URL and templates follow the site. Managed-provider registration and immutable machine templates have additional limits described under split mode.

**Back up `inventory/mysite/generated/`.** It holds the encryption key, database passwords and Proxmox token secrets. The encryption key cannot be recovered, and without it the stored provider secrets are lost. The directory is gitignored.

The BOM pins the broker-free chart/API at the stable release **v0.5.2** (after the
first stable `v0.4.0`); its tag publishes the images and app chart. Use
`images.build: true` (default) from a checkout containing the new core, or set
`images.build: false` once you have verified the `v0.5.2` chart and images are
pullable from your site.
`images.build: false` with an older Rabbit-dependent release is refused before
stage 40 writes secrets; the pin itself is not proof that images/packages exist.
No Git tag or release publication is performed by the installer. Public development
container images do not establish a public chart/release pin. A clean
`images.build: false` install from the released artifacts, and live installer
acceptance, have not been performed for this release.

**Current evidence (2026-11-10):** Debian 13 and Ubuntu 24.04 OS-template canaries
completed build, smoke and build-network cleanup on a development PVE 9.2 host.
The new Kubernetes static ISO seed mode has tests and full Packer configuration
validation, not a real ISO/reboot/Goss canary. These checks do not certify the
installer's split path end to end; dated lab reports below remain historical.

## What you need

| | |
|---|---|
| Installer host | Linux with `python3`, `openssl`, `docker` and `helm`; `make deps` creates a virtualenv with `ansible-core`. SSH to the management host(s). `docker` and `helm` are for `images.build`, the default. |
| Proxmox VE | 9.2 or newer, datacenter firewall on, nftables firewall on every node. For EVPN tenant networks, `frr-pythontools` on every node. |
| Management host | `mgmt.placement: ssh` (default): 1 or 3 Linux hosts with sudo, 4 vCPU and 8 GiB RAM or more, that are **not** Proxmox nodes. `mgmt.placement: pve-vm`: the installer creates the VMs. |
| Network | The management host reaches the Proxmox API, port 8006, and the internet (k3s, charts, images). OS-template builds use a reserved unused static address on the template bridge/access VLAN; the build host must reach its SSH port and the guest must reach working DNS/package repositories. DHCP is not required for OS templates. |
| Credential | A root-equivalent Proxmox credential for the install, in the vault. The portal itself runs on a least-privilege token the installer creates ([proxmox-permissions.md](proxmox-permissions.md)). |

## Stages

| Stage | Playbook | What it does |
|---|---|---|
| 00 | `00-preflight.yml` | Version, firewall, FRR, reach, refuses a Proxmox node as management host, detects storage and proposes profiles, pins the API certificate, fio class suggestion. |
| 05 | `05-storage.yml` | With a NAS: creates `nas-templates`, `nas-capacity`, `nas-backup` over NFS. |
| 10 | `10-pve-identity.yml` | Roles, users, privilege-separated tokens, ACLs and pools. |
| 25 | `25-mgmt-vms.yml` | `placement: pve-vm` only: the management VMs. |
| 20 | `20-templates.yml` | OS templates through [images/build.py](../images/README.md), from the first management host. |
| 30 | `30-mgmt-cluster.yml` | k3s, always with `--cluster-init`. |
| 35 | `35-local-images.yml` | `images.build`: builds images and chart from this checkout, into a registry on the management cluster. A tag already there is not rebuilt. |
| 40 | `40-mgmt-platform.yml` | Read-only existing-site migration gate, then ArgoCD and Applications: cert-manager, CloudNativePG, ProxCloud (API, consoles, Keycloak, PostgreSQL durable jobs). No new RabbitMQ. Scheduler/debug off unless explicitly enabled; workers always finish queued work. |
| 45 | `45-object-store.yml` | The object store, see below. |
| 50 | `50-services-cluster.yml` | `mode: split`: Cluster API and CAPMOX on the mgmt cluster, then the services cluster and its platform. See "Split mode" below. |
| 55 | `55-sandbox-cluster.yml` | `mode: split` with `sandbox.enabled`: the sandbox cluster's vnet, the cluster from the kamaji chart, then `deploy/sandbox-cluster` on it. See "Sandbox cluster" below. |
| 55 | `55-sandbox-registry.yml` | `sandbox.registry.address` set: the sandbox registry (Zot on the services cluster, its bucket on the object store), the images of `deploy/templates/images.yaml` copied in and pre-pulled on every sandbox node. Also run by `make upgrade`. See "Sandbox registry" below. |
| 56 | `56-sandbox-edge.yml` | `sandbox.enabled`: the public entry of sandbox ports: `sandbox-proxy` and its MetalLB address, the wildcard certificate on the services cluster and its copy, the DNS record and TSIG key on PowerDNS, the HAProxy entry appliance. Also run by `make upgrade`. See "Sandbox ports" below. |
| 60 | `60-register.yml` | Logins, an Admin-API key, the Proxmox provider, fio suggestions; on the first run `deploy/seed/seed-operator.py` (platform and billing configuration, flavors, public price plan, quota). Prints the URLs. |

Stage 40 no longer installs, waits for, upgrades or removes the RabbitMQ operator.
Existing brokers/operators/PVCs remain for recovery; new sites need none.
**Before upgrading an existing site**, complete the
[stop-producers, drain/reconcile, snapshot and retain runbook](postgres-jobs-migration.md),
including live Argo `Prune=false,Delete=false` protection. Set
`proxcloud.jobs_migration` with your explicit retain/cutover decision and evidence;
stage 40 fails before applying anything if the required proof/protection is missing,
or cluster discovery is forbidden/unavailable. It never deletes or patches the broker.
cert-manager stays for TLS/ingress and the CSI gateway. k3s's bundled Traefik and
ServiceLB serve the portal on the management host's ports 80 and 443. MetalLB and
the services cluster arrive with split mode.

## Options

[`installer/playbooks/group_vars/all/defaults.yml`](../installer/playbooks/group_vars/all/defaults.yml) documents every option; [`versions.yml`](../installer/playbooks/group_vars/all/versions.yml) pins every version. Override them under `proxcloud:` in the site's `group_vars`; the two are deep-merged. The ones most sites touch:

| Option | Default | Choices |
|---|---|---|
| `mgmt.placement` | `ssh` | `ssh`, `pve-vm` |
| `ingress.domain` | `<mgmt ip>.sslip.io` | Your domain; the portal uses `app.`, `admin.`, `api.`, `id.`, `s3.` under it |
| `ingress.tls` | `none` | `none`, `acme` (Let's Encrypt, needs public DNS), or the name of a TLS Secret |
| `images.build` | `true` | Builds images and chart from this checkout and pushes them to a registry on the management cluster. `false` pulls the release pinned in `versions.yml` from `images.registry` (for that, a release must be published) |
| `images.pull_secret` | none | `{username, password}` for a private registry |
| `network.kind` | `evpn` | `evpn`, `simple` (one node), `vlan` |
| `object_store.mode` | `vm` | `vm`, `truenas-app`, `external`, `ceph-rgw` |
| `templates.per_node` | `auto` | `auto`: a copy per node unless the default profile is on shared storage |
| `storage_profiles`, `storage_classes` | detected | Override detection; confirm a fio suggestion |
| `jobs.scheduler_enabled`, `jobs.debug_triggers` | `false`, `false` | Gates for new scheduled work/debug routes, **not** a pause of existing durable jobs. Enable scheduling deliberately after cutover. |
| `jobs.worker_concurrency`, `jobs.poll_interval`, `jobs.lease_duration` | `2`, `1s`, `1m` | Per-pod worker bounds: `1..16`, `100ms..1m`, `10s..10m`. |
| `database.instances`, `database.status_read_replica` | `1`, `false` | Replica opt-in for history/metrics only, requires at least two instances. Queue/finance use `-rw`. |
| `jobs_migration.*` | unset | Existing-site explicit retain/cutover, old producers/consumers stopped, drain/reconciliation and snapshot evidence; [runbook](postgres-jobs-migration.md). |

### Proxmox endpoints and cluster CA (stage 60)

`pve.api_url` is the installer's endpoint; `pve.portal_api_url` is the portal's
primary endpoint, falling back to `api_url`. They are not interchangeable when the
installer uses a bastion/forward. To register failover, explicitly supply **every
additional portal-reachable** endpoint under `pve.portal_api_urls`. Stage 60 copies
the list, in order, into the existing provider field `config.apiUrls`; it does not
discover, probe or replace addresses with the installer-side endpoint.

```yaml
proxcloud:
  pve:
    api_url: https://installer-pve.example.org:8006
    portal_api_url: https://pve1.example.org:8006
    portal_api_urls:
      - https://pve2.example.org:8006
      - https://pve3.example.org:8006
    # PUBLIC certificate only, obtained and verified through your trusted operator channel.
    # Never paste a private key. This maps to config.caCert, NOT an ignored config.clusterCA key.
    cluster_ca: "{{ lookup('ansible.builtin.file', inventory_dir ~ '/pve-root-ca.pem') }}"
```

The public PVE cluster CA is normally available to an authorized operator as
`/etc/pve/pve-root-ca.pem`. Do not copy a node's private key or use an unverified
network download as your trust source. The installer only validates the supplied
certificate bundle locally (PEM parsing, certificate-only blocks, at most 64 KiB);
it does not prove that a particular endpoint presents a chain issued by that CA.
The existing PVE client verifies the certificate chain against `config.caCert`
without checking the hostname, for PVE node IPs/VIPs. `caCert` takes precedence over
`fingerprint`, but `insecureTls: true` bypasses both; explicitly supplying a CA
therefore also writes `insecureTls: false`. An expired, wrong or non-signing CA
still fails the real connection rather than falling back to a fingerprint or
insecure TLS. An empty, malformed, missing-file or private-key input stops payload
resolution before a provider POST/PUT; it never selects another trust mode.

**Existing-site migration / retry rules:**

1. Back up the site's configuration and generated secrets through the existing
   private backup process. Inspect the registered provider's public connection
   fields in the admin console; do not export its encrypted credentials.
2. With both new inputs omitted or `null`, stage 60 keeps installed `apiUrls`,
   `caCert`, `fingerprint` and `insecureTls` exactly, including absent keys (system-CA
   verification). A fresh site with no CA retains stage 00's pinned leaf
   fingerprint. Existing `pve.fingerprint` and `portal_api_url` inputs still work;
   one leaf pin generally cannot verify different nodes' certificates.
3. For CA-backed failover, supply the verified public CA and the additional
   portal-reachable URLs. `portal_api_urls: []` deliberately clears only the
   additional list. `cluster_ca: ''` is invalid, **not** a request to erase trust;
   to leave installed trust unchanged, omit it or use `null`. Trust-mode removal
   remains an explicit admin-console action, not an installer default.
4. Plan/run stage 60 only under your normal change authorization: the complete
   playbook also manages logins, seeds and other providers and performs its existing
   live connection test. `make upgrade` is **not** a registration-only preview; it
   runs other stages too. This change has only offline test/render/syntax evidence,
   not fresh-install, failover, restore or live migration acceptance.
5. Stage 60 requires a unique matching name identifying a valid `CLOUD`/`proxmox`
   row before and after creation. A duplicate name or collision with another
   provider kind stops registration; resolve it in the admin console rather than
   letting the installer repurpose a row. On retry it uses the same row, sends no
   `secret` on its configuration PUT and keeps rotated `tokenId`, service toggles,
   detected capabilities and unrelated config. Only connection and the previously
   installer-owned network/storage/template fields are reconciled; an unchanged
   payload skips the PUT. This is sequential rerun safety, not cross-process CAS.

Offline regression commands (no endpoint or private inventory required):

```sh
installer/.venv/bin/python installer/tests/test_registration.py
installer/.venv/bin/python installer/tests/test_filters.py
```

### Access over plain HTTP

`ingress.tls: none` is for a lab or a management network you trust. A browser has no WebCrypto outside a secure context, so the consoles log in without PKCE and the installer stops requiring it on the Keycloak clients; credentials cross the network in the clear. Use `acme` or a TLS Secret on anything else.

### Placement of the management cluster

Never on a Proxmox node: k3s and `pve-firewall` fight over FORWARD and AppArmor, and evictions read the ZFS ARC as used memory. Preflight refuses it.

```yaml
proxcloud:
  mgmt:
    placement: pve-vm
    vm: {servers: 1, ips: [192.0.2.30/24], gateway: 192.0.2.1}
```

The VMs go into pool `proxcloud-system`, which the platform token cannot touch. Three servers need three Proxmox nodes (etcd members on different nodes).

### Storage classes

Preflight detects each storage's type and proposes profiles: `std` on ZFS, `ceph` on RBD, `capacity` on an NFS store with `images` content. An NFS store that also holds `iso` or `import` content is the template store, not a profile. With hosts in the inventory group `pve`, it also runs fio on a 4 GiB scratch volume per storage and suggests a class (`generated/storage-classes.json`):

| p99 sync-write latency | Class |
|---|---|
| under 1 ms | `performance` |
| 1 to 5 ms | `standard` |
| over 5 ms, or under 2,000 randread IOPS | `capacity` |

This is only a suggestion. It lands as `suggestedClass` on the provider, and the operator confirms it in the admin console or with `storage_classes`. The thresholds are `preflight.fio_*`. A NAS's cache can flatter randread, so latency decides first.

### Object store

| Mode | What happens |
|---|---|
| `vm` | A RustFS VM in `proxcloud-system`, cloned from the Debian template, with an XFS data disk on `nas-capacity` when present (qcow2, so the VM can be snapshotted), else on the default profile's storage. |
| `truenas-app` | The TrueNAS community catalog app on the NAS pool, driven through the TrueNAS REST API (`vault.truenas_api_key`). |
| `external`, `ceph-rgw` | Nothing is deployed. `ceph-rgw` registers `object_store.{endpoint,access_key,secret_key,region}` as the `ceph-s3` provider. Its key belongs to an RGW admin user with caps `users=*;buckets=*;usage=*`. Preflight refuses blank values and stage 60 tests the endpoint and key through the API before storing anything. A re-run writes the corrected endpoint or rotated key to the provider. `region` is what tenants see and send, so use the zonegroup's `api_name` or keep `us-east-1`; it is read only when the provider is created. |

For `vm` and `truenas-app`, the public endpoint is `s3.<domain>` through the management ingress with `/rustfs/admin`, `/minio/admin` and the console blocked. The root credentials are generated, FTP, SFTP and WebDAV are off, and the internal buckets `cnpg-backups` and `etcd-snapshots` get versioning, Object Lock GOVERNANCE and a user limited to the bucket.

Not automated yet: the off-site copy, OTLP metrics, and pointing CloudNativePG and k3s backups at those buckets.

## Day 2

| Command | What it does |
|---|---|
| `make upgrade` | After bumping `versions.yml`: stages 00, 10 (Proxmox identity), 30 (k3s), 35 (images), 40 (chart, operators), 55 (registry: copies the newly pinned sandbox images in), 56 (sandbox edge) and 60. It does not run stage 50 or stage 55 (cluster): run those playbooks yourself when a pin they use changed (CAPI, CAPMOX, kamaji, the sandbox runtime image). |
| `make identity-check` | What stage 10 would grant, align and take away, writing nothing. Run it before an upgrade that changes grants. |
| `make templates` | Build new template versions now. Keeps two per OS and node. |
| `make storage-rebench` | Re-run fio. Never changes the class of a live volume. |
| `make add-node NODE=pve4` | Checks the node (version, empty, chrony, latency), drops the QDevice when the member count turns odd, joins, FRR, SDN apply, its template copies, fio, re-detect. Safe to re-run on a member. |
| `make remove-node NODE=pve3` | Refuses while guests remain, destroys the node's template copies; a second run with `ARGS='-e pc_node_is_off=true'` deletes the member. |
| `make mgmt-scale` | After adding servers to the inventory or raising `mgmt.vm.servers`: joins them to etcd. |
| `make rustfs-upgrade VERSION=x.y.z` | Read the release notes and advisories first, and add the release digest to `versions.yml`. Stops RustFS, takes a VM snapshot, installs the pinned binary, verifies. |
| `make rustfs-rollback` | Rolls the VM back to that snapshot, binary and data together. Anything written since is lost. |

`add-node` needs SSH from the node to an existing member (`pvecm add --use_ssh`). On a 2-node cluster it does not set up the QDevice for you: it needs a machine outside the cluster.

## Per-organization SSO: restrict Keycloak's egress

If you enable per-organization SSO, Keycloak calls URLs tenants choose, and a DNS name checked when it was saved can
point at an internal address later. No check in the API can close that. Allow Keycloak egress to DNS, its database and
the public internet on 443 only, and deny the pod, service, node and management networks, the Proxmox, TrueNAS and
database addresses, and `169.254.0.0/16`. A `NetworkPolicy` with `ipBlock` `0.0.0.0/0` and an `except` list does it on
a CNI that enforces policies. The installer does not apply one, since the ranges are specific to the site.

## Credentials

- **Installer's admin key:** `proxcloud-api admin-key create -description installer`, run in the API pod by stage 60, mints an Admin-API SigV4 key directly in PostgreSQL. It needs database access and no login, which is what makes it usable before the first operator exists. Delete it from the admin console (API keys) if you do not want to keep it, and remove `generated/admin-api-key.json`.
- **Operator login:** user `keycloak.operator.username` in realm `proxcloud-admin`, password in `generated/operator-password`. That realm has no self-registration, because every login in it is an administrator.
- **Customers:** realm `proxcloud`, with self-registration on.

## Split mode: managed Kubernetes and databases

`proxcloud.mode: split` adds stage 50: Cluster API and CAPMOX on the mgmt cluster build a kubeadm **services cluster** of PVE VMs, which then runs Kamaji, its own Cluster API, ArgoCD, MetalLB, proxmox-csi and the database operators. Stage 60 registers the Kubernetes and database providers. Tenant clusters get persistent volumes from `services.kamaji.rwx_gib` (a NAS dataset per cluster) and from proxmox-csi through the CSI gateway, which stage 40 turns on in split mode: see [managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox). Stage 60 only registers these two providers when they do not exist yet, so `services.kamaji.*` and the chart version count at the first registration, and a re-run changes nothing on them. To change one later, see "Changing a setting later" in [managed-k8s.md](managed-k8s.md#2-provider-setup-admin--cloud-providers--add-provider--kubernetes). `services.iso_storage` (default `local`) receives each new node's kubeadm join token until the node joins: preflight refuses a shared store there, and on an install from before this rule `make upgrade` re-runs stage 10, which narrows the services token to that store and then checks with PVE that it is the only ISO store the token sees (`make identity-check` shows the changes first), see "Bootstrap ISOs" in [managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox).

1. Build the node templates **separately** (`images/k8s-node-template.sh`, see [images/README.md](../images/README.md#kubernetes-node-templates) and the [static quickstart](quickstart.md#d-managed-kubernetes-databases-and-sandboxes-split)). The GitHub workflow defaults `network_mode=static` and reads cluster values from Repository Secrets; direct script use must explicitly set `PVE_BUILD_NETWORK_MODE=static` or it selects legacy DHCP. Static mode uses a private NoCloud seed ISO, `BUILD_IP`/gateway/DNS and an explicit SSH host; it needs xorriso/OpenSSL and scoped ISO upload/delete access, not a DHCP server or inbound runner HTTP route. Use separate `PVE_TEMPLATE_STORAGE` (`images`) and `PVE_ISO_STORAGE` (`iso`) when needed. Stage 50 only consumes the resulting template IDs; it does not run this builder or consume its Actions Secrets. The static path still requires real ISO/reboot/Goss acceptance for the selected Kubernetes version.
2. Set `proxcloud.services` in the site's group_vars ([defaults.yml](../installer/playbooks/group_vars/all/defaults.yml) documents it): the network, `templates`, `control_plane`, one `workers` entry per PVE node a zone should exist on, and `kamaji.version_images`. A database with replicas needs a worker per replica, on different nodes. `object_store.mode` must be `vm` or `truenas-app`, because database backups go to the installer's own bucket. Preflight checks all of this.
3. `make install`.

The services subnet must be reachable from tenant networks (it holds the API endpoints and database addresses) and must reach the PVE API. Stage 10 grants what per-cluster tokens need, see [proxmox-permissions.md](proxmox-permissions.md#managed-kubernetes): read that first, since it gives the portal token `Permissions.Modify`.

**What ran where:** `50-services-cluster.yml`'s template and `files/services-platform.sh` ran on a lab through hand-written drivers, against a site already installed in `allinone`. Preflight in split mode and stage 60's registration ran on that lab against that cluster, and a project attached to both providers. The playbook itself and stage 10's split grants have not run end to end on a clean site.

Stage 50 cannot change the machines of a running cluster: a `ProxmoxMachineTemplate` is immutable, so a changed size, template or pool in `proxcloud.services` fails the apply. Moving an `allinone` site with a hand-built services cluster onto this stage means rebuilding that cluster.

## Sandbox cluster (split mode)

`proxcloud.sandbox.enabled: true` with `mode: split` builds the platform's sandbox cluster, where tenant code runs
under gVisor ([managed-sandbox.md](managed-sandbox.md)). Tenants reach it through the sandbox provider stage 60
registers, once the operator sets it live (step 2).

1. Set `proxcloud.sandbox.network` ([defaults.yml](../installer/playbooks/group_vars/all/defaults.yml)): a subnet outside
   `network.cidr_pool`, its gateway, the worker range (`nodes`), the cluster's LoadBalancer range (`metallb`) and DNS
   servers that are not on the management network. Stage 55 refuses a subnet overlapping the tenant pool, the
   services network, `network.blocked_cidrs` or `network.public`, an address outside the subnet, a VNI
   (`sandbox.network.tag`) another vnet holds, and a services network outside the private, CGNAT and link-local ranges
   the sandbox guardrail denies (a sandbox pod would reach the tenants' control planes there). The portal refuses a
   subnet overlapping a node's management network. With `network.isolation: evpn` set `sandbox.network.zone` to an
   EVPN zone made for the sandbox: there is no shared zone, and a tenant's zone would put the workers in its VRF.
   Set `sandbox.network.snat` to the sandbox's own egress address: an IPv4 address on the exit node's uplink network
   that the uplink routes to that node and nothing else uses, so abuse reports and blocklists name it and not the
   nodes or the portal. The zone needs one exit node (or `exitnodes-primary`), and that node must be in the inventory
   group `pve` with root ssh. `snat: node` shares the exit node's own address instead.
2. `make install`, or stages 10, 55 and 60 on an installed site. Stage 10 creates pool `proxcloud-sandbox` and token
   `capmox@pve!sandbox` ([proxmox-permissions.md](proxmox-permissions.md#managed-kubernetes)). Stage 55 creates the vnet
   (`sandbox.network.vnet`, in the EVPN zone `sandbox.network.zone`, default the provider's default zone), applies an
   ArgoCD Application of the kamaji chart this checkout pushed (NodeRestriction on, checked), waits for the workers,
   keeps `generated/sandbox.kubeconfig`, and applies `deploy/sandbox-cluster` in its README's order, stopping if the
   cluster has IPv6. It also puts `sandbox.network.snat` on the exit node (`/etc/network/interfaces.d/proxcloud-sandbox-snat`),
   turns PVE's own SNAT off for the subnet, and keeps `generated/sandbox-firewall.kubeconfig` (nodes only). Stage 10
   also gives the portal token the `ProxCloudSandboxGuard` role on the sandbox pool and checks it. Stage 60 writes the
   subnet to the Proxmox provider's `network.sandboxCidrs` and the workers to `config.sandbox` with that kubeconfig;
   the next sync adds the isolation drops and the workers' VM firewall, and takes the `proxcloud.io/unsecured` taint
   off each worker once it is fenced and 20 minutes old (`sandbox.unsecured_min_age_minutes`). Stage 55 also keeps
   the portal service account's `generated/sandbox-portal.kubeconfig`, and stage 60 registers the sandbox provider
   `<provider name> Sandboxes` with it, stored `DISABLED` (templates and limits from `sandbox.templates` and
   `sandbox.limits`; a site's `templates` replace the default `python` one). Once the taint is off, open it under Cloud
   providers, run "Check release gates" and set the status to `PUBLIC`. A re-run leaves an existing sandbox provider
   alone, also one renamed in the console.
3. The portal creates the default EVPN zone with the first tenant network. On a first install stage 55 therefore says
   SKIPPED; create a network, then run stage 55 again (and stage 60 if the provider was registered before
   `sandbox.enabled`). Until stage 60 and one sync have run, the workers have no isolation drops and no VM firewall,
   and they keep the taint, so nothing but platform DaemonSets runs on them (CoreDNS and the agent-sandbox
   controller wait too).

**What ran where:** stage 55 has not run on a lab (it would add an Application to the lab's shared services cluster).
Its template was rendered and the chart output read; the manifests and the IPv6 check ran against the hand-built SB0
cluster; the isolation drops ran through a portal on the lab ([managed-sandbox.md](managed-sandbox.md)). The VM
firewall pass ran on the lab with the portal token against the SB0 cluster, the guard role was granted by hand with
stage 10's rows, and the exit node's SNAT file was written by hand with the content stage 55 renders
([managed-sandbox.md §2.5](managed-sandbox.md#25-measured-on-the-lab-2026-11-07)). Stages 10, 55 and 60 themselves
have only been syntax-checked.

## Sandbox registry (stage 55)

The sandbox nodes pull templates and `sandbox-proxy` from the platform's own registry (the envd runtime image still from `images.registry`), each project with an account
that reads its own images and the built-in ones only ([managed-sandbox.md §6](managed-sandbox.md#6-the-sandbox-registry-sb6a)).

1. `sandbox.registry.address`: one address of `services.network.metallb`, unused. The stage refuses an address
   outside that range.
2. The bucket `sandbox.registry.bucket` (default `pc-sandbox-registry`) goes on the object store of stage 45:
   - **Standard** (`object_store.mode: vm` or `truenas-app`): RustFS. The stage creates the bucket and a user
     `<bucket>-svc` limited to it with the RustFS root key (`generated/rustfs-*`).
   - **Full** (`object_store.mode: ceph-rgw`): RGW. `object_store.access_key`/`secret_key` must be an RGW user with
     `users=*` caps (the same key the ceph-s3 provider uses); the stage creates the user `<bucket>-svc` through the
     admin ops API, and that user creates and owns the bucket. `object_store.region` is the zonegroup's `api_name`.
   - `external`: the store's own key creates and keeps the bucket.
   The bucket has no Object Lock (Zot's garbage collection deletes unreferenced blobs); its secret is
   `generated/sandbox-registry-s3-secret`. A second bucket, `sandbox.registry.builds_bucket` (default
   `pc-sandbox-builds`), gets the same treatment for custom template builds (secret
   `generated/sandbox-builds-s3-secret`). Build pods fetch from it, so the store's endpoint must be an IPv4 address with
   its port; the stage refuses a name.
3. `make install`, or stages 55 (both), 56 and 60 on an installed site. Stage 55 (registry) runs Zot in
   `sandbox.registry.namespace` with a CA of its own, writes `generated/sandbox-registry-{admin,gc,pull-nodes,key}`
   (the accounts and the key project passwords derive from), copies each pinned image and the release's
   `proxcloud-sandbox-proxy` in (skopeo on the first mgmt host; `images.pull_secret` if ghcr needs a login) and
   applies `deploy/sandbox-cluster/registry.yaml`, the prepull and the build namespace `sbx-build`
   (`templates/sandbox-builds.yml.j2`: its egress policy, the registry CA, the portal's and the sync's rights there)
   on the sandbox cluster. Stage 56 runs sandbox-proxy from the copy. Stage 60 gives the Proxmox provider
   `config.sandbox.registry` and `config.sandbox.buildStorage` (the workers' firewall then admits those two addresses
   and ports) and, when it first registers the sandbox provider, the templates with `pin:<name>` resolved to the copies,
   `templateRegistry`, `imageAdoption` (`sandbox.registry.adoption`), the build bucket, the registry CA,
   `registryPrune` (`sandbox.registry.prune`, default `on`) and the `gc` account
   ([managed-sandbox.md §7](managed-sandbox.md#7-custom-templates-sb6b)).
4. Check: after the next sync, `kubectl -n sandbox-system get pods -l app=template-prepull` on the sandbox cluster
   shows one Running pod per node; a re-run of stage 55 reports the count.
5. New template images: merge the `sandbox-templates` workflow's pull request, then `make upgrade`. The portal moves
   the templates to the new digests once every node pre-pulled them (`imageAdoption: on`).

The registry sync invokes `sandbox-proxy registry-users`, so its image must contain that subcommand; an
older incompatible image stops with "unknown subcommand". The Docker workflow now builds five images, including
`proxcloud-sandbox-proxy` and `proxcloud-sandbox-runtime`, and development tags are public. **Stage 35 still builds
only API/web/admin**, and the BOM selects the stable release `v0.5.2`; this source pin does not prove its sandbox images were published or are pullable. On a source-build site,
explicitly pin `sandbox.proxy.image` and `sandbox.runtime_image` to compatible, pullable development tags or digests;
do not assume `<images.registry>/…:<release>` exists. A complete split/sandbox installation from coordinated public
release artifacts has not been accepted yet.

An existing sandbox provider keeps its templates: set `Template registry` (`<address>/t`), `Built-in image updates`
and the templates' images (the `library/` copies) in the console, and for custom templates "Custom template builds"
(endpoint, bucket and keys from `generated/`), "Prune unused template images", the gc account
(`generated/sandbox-registry-gc`) and the registry CA. Upgrading from SB6a replaces the `push` account with per-project
build accounts; nothing used `push` before SB6b.

**What ran where:** stage 55 (registry) ran on the lab against the SB0 cluster from a scratch site, with RustFS and
then RGW as the backend; stage 60's new keys and the release proxy copy did not run there
([managed-sandbox.md §6.6](managed-sandbox.md#66-measured-on-the-lab-2026-11-07)). SB6b's additions to stages 55 and
60 have not run on the lab: running stage 55 there was refused by the session's permission check
([§7.6](managed-sandbox.md#76-measured-on-the-lab-2026-11-08)).

## Sandbox ports (stage 56)

Sandbox ports answer at `https://<port>-<sandboxId>.<sandbox.domain>` ([managed-sandbox.md §4](managed-sandbox.md#4-public-entry-tls-and-wake-sb4)).

1. Give the sandbox a domain of its own (`sandbox.domain`, e.g. `example-sandbox.net`): a registrable domain apart
   from the portal's, on the Public Suffix List if you can, since every tenant's sandbox shares its cookie site
   (managed-sandbox.md §4.3). A zone on PowerDNS, delegated from its parent, with `dnsupdate=yes` and
   `allow-dnsupdate-from=` (empty) in `pdns.conf`, `sandbox.powerdns.api_url` and `.nameserver` set and
   `pdns_api_key` in the vault. Without PowerDNS set `sandbox.tls.issuer: secret` (your `kubernetes.io/tls` Secret
   for `*.<domain>` in `pc-sandbox` on the services cluster, `sandbox.tls.secret`) and write `*.<domain>` yourself.
2. `sandbox.proxy.address`: one address of `sandbox.network.metallb`. `sandbox.entry`: the appliance's address on the
   sandbox vnet (outside the gateway, the workers and the MetalLB range) and its public bridge, address (CIDR) and
   gateway. No `entry.public.bridge`: no appliance; forward TCP 443 to `sandbox.proxy.address` yourself.
3. Stages 10, 55, 56 and 60 (or `make install`). Stage 10 creates pool `proxcloud-sandbox-entry` with the
   `ProxCloudSandboxEntry` role for the portal. Stage 55 adds the node group `sandbox-system`
   (`sandbox.system_nodes`, its addresses from `sandbox.network.nodes`): sandbox-proxy runs there alone. Stage 56
   writes the record and, for `acme`, the TSIG key (`generated/sandbox-tsig.json`), builds the appliance (VM
   `sandbox.entry.vmid`), applies the issuer, the certificate and cert-sync on the services cluster and `sandbox-proxy`
   on the sandbox cluster. It generates `generated/sandbox-wake-token`: put it into the sandbox provider's
   `secret.proxyWakeToken` (the proxy calls the wake endpoint at `sandbox.proxy.wake_url` to resume paused sandboxes). Stage 60 gives the Proxmox provider
   `config.sandbox.proxy`; the next sync fences the system nodes, opens the workers to the appliance and installs
   HAProxy's config on it. Run stage 56 again after that sync: it then waits until the proxy serves.
4. Check: `curl https://8000-<id>.<domain>/` with a sandbox serving on 8000 (`GET_ACCESS` gives its host and token).

**What ran where:** stage 56 ran on the lab against the SB0 cluster (a split-mode copy of the lab site, the SB0
cluster's kubeconfig in place of stage 55's, the services objects in a scratch namespace), with `selfsigned` and then
`acme` against an ACME test server (DNS-01 through the lab PowerDNS); stage 60's and stage 10's new rows did not run
there ([managed-sandbox.md §4.6](managed-sandbox.md#46-measured-on-the-lab-2026-11-08)).

