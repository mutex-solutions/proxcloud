# C4 model change log

Every architecture change lands here in the same PR that changes the code and the
diagrams: what changed, why, and which diagrams were updated. Newest first. Format
and rules: [README.md](README.md#maintenance-rule).

## 2026-11-07: RustFS tenant buckets behind objectBackend

**What changed**

- `internal/cloud/client`: `Client` now wraps an `objectBackend` with `Caps()`. `cephBackend` keeps RGW;
  the new `rustfsBackend` embeds its plain-S3 data plane. New provider kind `rustfs`
  (`ExternalService.ObjectClient` picks the backend); methods renamed to neutral names.
- RustFS ownership lives in the project's canned policy, the project quota in the API (sum of bucket quotas),
  and scanner usage keeps the last value when the scanner cannot answer.
- Extra S3 keys on RustFS are IAM users granted through bucket policies with a bare-name principal.
- Bucket billing: new attribute `quota_gb` next to `size_gb`; the priced attribute is the billing basis.
- The installer registers the RustFS it deployed as a `rustfs` provider at stage 60.

**Why**

- Standard-tier sites had a deployed RustFS but no tenant bucket product.
- RustFS has no bucket owner and no user quota, so isolation and quota have to live on our side.
- Lab check RUSTFS-POLICY-1 on 1.0.4 showed user principals do work in bucket policies, but only by bare
  name, so grants could keep RGW's model instead of moving to identity policies. The ARN form saves and
  silently matches nothing, which is why the principal form is per backend.
- A tenant able to delete a bucket directly would leave its ARN behind and reach the next owner of the
  name, hence the Deny on the project key and the clean-up of other policies at create.
- Scanner usage is sometimes unavailable; writing 0 would bill nothing for that interval.

**Diagrams updated**: `c1-context.md`, `c2-containers.md`, `c3-components.md` (C3 managed services).

## 2026-11-07: P4 metering foundations (branch feat/p4-metering)

**What changed**

- Proxmox state sampler (C3c): Planned -> Implemented. `pveStateSync`, every minute: one
  `/cluster/resources` page per provider reconciles SERVER, IMAGE and FLOATING_IP rows of every
  project, carrying agent IPs and the GPU count from the cached row.
- Proxmox task poller (C3c): Planned -> Implemented. `pveTaskPoll`, every 20 seconds, per-node
  cursor, UPID dedupe; re-syncs the project, audits other users' tasks, and backs `LIST_EVENTS`
  (was an empty stub).
- VM identity: portal-created VMs carry a `pc-c-<unix>` tag. A row whose identity changed is
  archived and a new one inserted in the same pass, disks included, so a reused vmid gets a new
  row and bill item. The portal also archives a row older than its own create.
- Traffic (C3c): Planned -> Implemented. `metrics.PVEFetcher` reads public-NIC counters from
  `status/current` every minute into `nicCounter` with reset-aware deltas. `pve` is the new default
  source of a Proxmox provider.
- Public IP and image billing: Planned -> Implemented. FLOATING_IP rows derive from the allocations
  (one per allocation, deletable, which detaches the NIC); new `image` resource type on `size_gb`.
- GPU count comes from `hostpciN mapping=` on the full sync, tags being only the fallback.
- Rating engine: a started unit is a billed unit, `getTimeUnitsDiff` floors at 0, rows archived this
  cycle are fed back with `DeletedAt`, `OVERWRITE_TOTAL` is stored bare on the first charge too.
- Staleness policy (C3c): new. Last-known state for 30 minutes, then the provider's resources stop
  billing, the gap is written off (`SkipInterval`, new on the `billingapi` wire contract), and an
  `ERROR` log line and a `METERING_STALE` audit event fire once.
- Admin API refuses a rule pricing a usage attribute unless it is `OVERWRITE_TOTAL` on an hour or
  minute cadence (an unnamed apply method defaults to overwrite).
- Removed: the inert `load_balancer` price type (provider, catalog entry, seed rule).

**Why**

The cache was refreshed every 15 minutes and keyed on the vmid alone, so state changes outside
the portal lagged charges, and PVE's reuse of the lowest free vmid let a new VM inherit the old VM's
row and bill item. Hour rules lost the first partial hour and the last interval of every
resource. One page per provider instead of 3N calls makes a 60-second cadence affordable, and
the 2-minute grace exists because pmxcfs lags: without it a just-created VM would be archived and a
just-deleted one resurrected. The stamp replaced `meta: ctime` as the identity because a clone keeps
its template's ctime (verified on PVE 9.2), so ctime only separates VMs created from scratch. A
started hour bills in full so that a resource's bill is a simple count of clock hours; truncating
would make any VM younger than an hour free. The write-off exists because `NotEligibleForBilling`
alone only delays a charge: the watermark would stay and recovery would back-bill the whole outage.
Status/current was picked over a metric-server push because it needs nothing installed on the
nodes; its ceiling is the traffic between the last sample and a counter reset.

An external billing service must mirror the rating changes (`getTimeUnitsDiff`, the first
`OVERWRITE_TOTAL` charge, `SkipInterval`).

**Diagrams updated:** C3c, C3b table.

## 2026-11-07: tenant isolation moves to cluster firewall rules, EVPN gets platform IPAM

Found by the end-to-end lab drill on PVE 9.2.

**What changed**

- Tenant isolation is now a managed rule set in the cluster firewall (`proxcloud:isolation`
  rules, one ACCEPT per project) instead of per-vnet firewall rules. It also drops traffic
  from the pool to the nodes' management networks and a new `network.blockedCidrs`.
- On a zone without DHCP (EVPN, VLAN) the platform allocates each server's `net0` address,
  sets it through cloud-init and pins `ipfilter-net0` to the `/32`. The allocation rows live
  in `pvePublicIp` on NIC `net0`; only `net1` rows count against the `floatingIps` quota.
- A public NIC takes the default route; attach and detach answer `rebootRequired`.
- `ResolveVM` reads power state and config from the node, not from `cluster/resources`.
- A failed project deletion stays `DELETE_IN_PROGRESS` and is retried, and `DELETE
  /project/{id}/now` now actually deletes.

**Why**

- Vnet `FORWARD` rules only see frames switched inside one vnet bridge. Traffic between
  subnets is routed by the node, so with the old rules one tenant reached another, and
  through an exit node's leaked VRF route also the node management network and storage.
- PVE refuses `dhcp` on an EVPN zone: network create failed outright, and DHCP is simple-zone
  only. A static address also lets ipfilter pin the exact address, which closes the
  spoof-a-sibling ceiling.
- With a default route on both NICs, replies from a public address left through the overlay.
- `cluster/resources` lags a finished task by seconds, so a resize right after a stop
  restarted the VM.
- A rollback to `ENABLED` after a half-done teardown showed a broken project as healthy and
  was never retried.

**Diagrams updated**: `c4-code.md` (C4.1, C4.2, sequences).

## 2026-11-07: P6 installer and image pipelines

**What changed**

- Installer (C2c): Planned -> Implemented. `installer/` is one Ansible entrypoint,
  `make install INVENTORY=...`, in `allinone` mode: preflight, Proxmox identity, NAS stores,
  OS templates, k3s (`mgmt.placement: ssh | pve-vm`, always `--cluster-init`), the platform
  through ArgoCD Applications with inline values, the object store, and registration through
  the admin API. Day-2: `upgrade`, `templates`, `storage-rebench`, `add-node`, `remove-node`,
  `mgmt-scale`, `rustfs-upgrade`, `rustfs-rollback`. `split` mode stays Planned with P7.
- VM template pipeline (C2c): Planned -> Implemented. `images/build.py` (vendor image, Packer,
  smoke test, publish by tag, prune) runs from the installer and from
  `.github/workflows/vm-templates.yml`, which has no PR or fork trigger.
- Template discovery (C3b): new. The catalog comes from cluster tags (`pc-public`,
  `pc-img-*`, `pc-v-*`) in pool `pc-templates`; `config.templates` is now an optional pin or
  overlay. Server create clones the target node's own copy, so a local-disk site is no longer
  pinned to one node.
- `proxcloud-api admin-key create`: new. A non-interactive Admin-API key, minted through the
  database; the one writer path (`admin.NewAdminAPIKey`) is shared with the endpoint and the
  debug trigger.
- Chart: CloudNativePG and a new `RabbitmqCluster` are the defaults, the bitnami subcharts
  are opt-in (`values-dev.yaml`). `cnpg.keycloak` and `auth.*.discoveryUri` added.
- RustFS (C2): Planned -> Implemented as the internal object store; tenant buckets stay
  Planned. Platform-owned VMs (C2b): Planned -> Implemented, except the services cluster.
- New `ProxCloudImages` role and `images@pve!packer` token.

**Why**

- A first install took hand-run `pveum`, hand-made templates and psql seeds. The plan's
  v3.1 decision is one command with the easiest default for every choice, each overridable.
- Tags, not config, because a weekly rebuild changes template ids: a catalog in config goes
  stale on every run. The pool is the ownership proof, a tag alone is not.
- Per-node copies, because PVE clones across nodes only from shared to shared storage (the
  P3 lesson): with local VM disks, one template meant every VM on one node.
- `ProxCloudImages` uses `Sys.AccessNetwork` instead of `Sys.Modify` on `/`, and has no
  ACL on `/vms`, so the pipeline token cannot touch a tenant VM.
- k3s's bundled Traefik and ServiceLB replace MetalLB and nginx-gateway-fabric in `allinone`:
  one host needs no VIP pool, and every extra component is one more thing to pin and upgrade.
  They return with the services cluster.
- cert-manager is always deployed, not only for ACME, because the RabbitMQ operator's webhook
  needs it (operator v2.23).
- The operator's own Secret carries the RabbitMQ credentials, so there is one less secret
  to generate and rotate. The API reads it directly.
- The admin realm is separate from the customer realm: any login in the admin realm is an
  administrator, and the customer realm allows self-registration.

**Diagrams updated:** c1-context.md, c2-containers.md (C2b, C2c, tables), c3-components.md (C3b)

## 2026-11-07: console and server detail match what Proxmox serves

No component changed. The wizard honors the boot volume type, offers the project's own
images and sends one network; the serial console speaks the `termproxy` framing; PVE write
calls run on a context detached from the client connection, because a browser going away
mid-clone left a half-built VM. Fields the backend ignored were removed rather than kept
(fixed IP, DNS and gateway on networks, volume name, security group description, ether type).

## 2026-10-28: P3 storage foundation and honest UI (branch feat/p3-storage, PR TBD)

Summary of the P3 slices below (P3-1 to P3-10); the per-slice entries carry the detail.

**What changed**

- Capability detection (C3b): Planned -> Implemented. `pve/capability.go` derives each
  storage's type, sharing, content, snapshot/thin/live-migrate/replication support, tier,
  default class and per-node free space from the cluster; it runs every sync cycle, on admin
  Re-detect and on every profile save. Admin class override per storage; the installer's
  fio result is a suggestion only.
- Storage profiles are validated against detection (flags may only downgrade; parsed flags
  are also clamped to the detected capabilities on every read, so a profile written on
  provider create cannot claim more), gain a
  derived tier and class, a `label`, and `bootDisk` / `dbaas` caps. A capacity-class
  profile never takes boot disks, databases or snapshots.
- Server create: placement restricted to nodes where the profile's storage is active and
  keeps `storageHeadroomPct` (default 10 %) free; a template on shared storage clones to a
  picked node whose profile storage is shared too (PVE clones cross-node only shared to
  shared), a local template or a local profile pins to the template's node; refusals are
  409 with the reason.
- Volumes: add-volume free-space check, same-node attach gating (PVE's disk reassign needs
  both VMs on one node), `LIST_ATTACH_TARGETS`,
  `RETYPE` via `move_disk`, delete of detached disks; per-disk `profile` and
  `storage_class` reach billing.
- Client: `/init` `features` per provider and `platformConfiguration.hiddenServices`; the
  client hides actions the backend would reject and warns on capacity types and on server
  delete.
- Docs: `cloud-integration.md` (new *Storage capabilities and placement* section, provider
  document, accessors, sync job, create sequence), `configuration.md` (provider storage
  keys), `data-model.md`, `glossary.md`, `architecture.md`, the admin guide
  *Connecting a Proxmox VE Cluster* (new *Storage* section) and the client guides
  *Block Volumes* and *Working with Servers*.

**Why**

- Cross-node clones from a template on local storage, and cross-node attaches of local
  disks, failed inside Proxmox with raw 502s after the platform had already committed to a
  node.
- The scheduler ignored free space, so a nearly full pool was picked and the clone failed
  late (or filled the pool).
- The client offered actions every Proxmox request rejected or silently ignored (rebuild,
  rescue, set password, user-data, boot-time data volumes, ports, subnets, volume
  snapshots, attach to any server).
- Nothing stopped tenants from putting boot disks, databases or snapshots on the HDD NAS,
  whose RAIDZ HDD plus sync NFS gives single-disk IOPS, and operators could not price it
  apart.

**Diagrams updated:** c2-containers.md (storage tiers table and text), c3-components.md
(C3b capability detection, Scheduler, PVE service, Sync job, Provider config, Project
cloud handlers, Platform config, Billing resources, DBaaS rows), c4-code.md (sequence a:
create server)

## 2026-10-28: Capacity HDD (NFS) profile and capability gating (P3-9, S18)

**What changed**

- Storage profiles gain a derived `tier` (full only when every storage is Ceph) and
  `class` (the lowest effective class of their storages), an optional `label`, and the
  `bootDisk` / `dbaas` caps (`false` downgrades). A capacity-class profile never takes
  boot disks, databases or snapshots; a profile save claiming them is refused (400).
- API gates: server create on a profile without boot disks is 409; `RETYPE` of a boot
  disk onto one is 409; an image (server snapshot) of a server with an attached disk on
  capacity storage is 409; a database create whose storage class is named after a
  profile closed to databases is 409.
- The volume sync stamps each disk's own profile and storage class (`volume_type` now
  follows the disk's profile, not the VM's). The volume billing resource exposes
  `profile` and `storage_class`, so price rules can price "capacity" separately.
- `LIST_VOLUME_TYPES` returns `displayName`, `tier`, `class`, `bootDisk` and `dbaas`.
  Client volume create and retype label capacity types "HDD, not for databases" and
  warn; the admin profile editor flags capacity profiles.

**Why**

- The HDD NAS is the Standard tier's capacity store, never the hot VM-disk tier: RAIDZ
  HDD with sync NFS gives single-disk IOPS. Tenants must not land boot disks, databases
  or snapshots there, and operators need to price it apart.

**Diagrams updated:** c2-containers.md (storage tiers table and text), c3-components.md
(Provider config, Billing resources and DBaaS rows)

## 2026-10-28: Honest client UI and managed-service switch (P3-6, P3-7, P3-8)

**What changed**

- `GET /init/{projectId}` gains `features`: per attached provider id, the optional actions
  its backend serves (`cloudFeatures`). Proxmox serves `serverSnapshots` (image from a
  server) and `volumeRetype` (only with two or more storage profiles). No backend serves
  rebuild, rescue, set password, user-data, boot-time data volumes, ports, subnets or
  volume snapshots yet, so the client hides them.
- `platformConfiguration.hiddenServices` (service slugs) is dropped from the `/init` menu,
  so the client hides Kubernetes, Databases or Object storage entirely. The admin
  System > Platform page has the toggles; providers keep syncing.
- Client: server delete dialogs warn that every volume of the server is deleted, detached
  ones included; Create volume picks the server; Attach lists `LIST_ATTACH_TARGETS`;
  Delete shows only for detached volumes; the server Network tab shows the guest-reported
  addresses when the provider has no ports.

**Why**

- The client offered actions every Proxmox request rejected (or silently ignored, such as
  user-data and boot-time data volumes), and operators had no way to hide a managed service
  that is not ready to sell.

**Diagrams updated:** c3-components.md (Platform config and Project cloud handlers rows)

## 2026-10-28: Volume attach gating, retype and delete (P3-4, P3-5)

**What changed**

- Attach to a server on another node is refused with 409: qemu-server's disk reassign
  (`move_disk target-vmid=`) dies with "Both VMs need to be on the same node" even on shared
  storage. A target with no free disk slot is 409 too. New volume action
  `LIST_ATTACH_TARGETS` returns the servers on the volume's node (none while it is
  attached).
- `RETYPE` (`newType` = profile slug) moves the disk with `move_disk storage=<profile
  storage on the VM's node> delete=1`. The target must hold VM disks, be active on that
  node and keep the headroom free; otherwise 409. The old cache row is archived, the sync
  adds the new volid.
- `DELETE` of a volume destroys a detached disk (drops its `unusedN` entry); an attached
  one answers 409 "Detach the volume before deleting it". Volume create still needs
  `serverId` (400).
- New `pve.ConflictError`, `Service.AttachTargets`, `Service.RetypeVolume`,
  `Service.DeleteVolume`; handlers map `ConflictError` to 409 (`pveVolumeErr`).

**Why**

- Cross-node attach of a local disk failed inside PVE with a raw 502, there was no way to
  learn which servers were eligible, and detached disks could neither be deleted nor moved
  to another volume type.

**Diagrams updated:** c2-containers.md (storage tiers text), c3-components.md (PVE service
row)

## 2026-10-28: Template placement and capacity-aware scheduling (P3-1, P3-2)

**What changed**

- Server create reads live storage capabilities and the template's config. A template whose
  volumes (disks, EFI/TPM state, cloud-init and ISO drives) all sit on shared storage is
  full-cloned with `target=<picked node>` and `storage=<profile storage on that node>`
  only when that profile storage is shared as well: qemu-server's clone dies with "can't
  clone to non-shared storage" for a cross-node clone onto local storage. A template with
  any volume on local storage, or a local profile, is pinned to the template's node; when
  the storage profile is not active there the create answers 409 with the reason instead
  of a 502 at clone time.
- Scheduler (`PickNode`) skips a node whose profile storage is inactive or would keep less
  than `config.storageHeadroomPct` (default 10 % of its capacity) free after the root disk.
  Data-volume create (`AddVolume`) runs the same check on the server's node and answers 409.
- `PVEStorageProfile.Nodes` + `pve.ActiveStorageNodes` replaced by
  `PVEStorageProfile.Placement(caps)` (node -> storage state); new `pve.StorageHasRoom`,
  `pve.TemplateShared`, `pve.ErrStorageFull`; `AddVolume` takes the headroom.

**Why**

- Cross-node creates from a local template failed inside PVE with a raw 502, and the
  scheduler ignored free space, so a full pool was picked and the clone failed late.

**Diagrams updated:** c2-containers.md (storage tiers text), c3-components.md (Scheduler
row), c4-code.md (server create sequence)

## 2026-10-28: Storage capability detection (P3-3, P3-10 API part)

**What changed**

- Capability detection (C3b): Planned -> Implemented. New `internal/cloud/pve/capability.go`
  derives per storage, from `GET /storage` and `GET /cluster/resources?type=storage`: type,
  shared, content, snapshots, thin, live migrate, replication (`zfspool` only), tier
  (`full` for rbd/cephfs, else `standard`), a default class from a static type table, and
  per-node active/total/avail.
- Provider config gains `config.capabilities` (written by detection: every services-sync
  cycle, the new admin `POST /admin/service/{id}/storage/detect`, and every storage-profile
  save) and `config.storageClasses.<id>` (`class` = admin override, set through the new
  `PUT /admin/service/{id}/storage-classes`; `suggestedClass` = installer fio result, shown
  only).
- Behaviour change: a `storageProfiles` save on a Proxmox provider is validated against a
  live detection (storage exists, active on its node(s), has `images` content, flags only
  downgraded). A cluster that cannot be reached rejects the save.
- Behaviour change: `PVEStorageProfile.Nodes` takes the active storage map; `*` now means
  "nodes where the storage is active" instead of "all nodes", and explicit node entries are
  dropped when their storage is inactive there. A server create on a profile with no active
  storage answers 409 instead of trying a node that cannot hold the disk.
- Admin UI: provider Storage tab (capabilities, active nodes, detected/suggested class,
  override select, Re-detect button).

**Why**

- Profile flags were admin-declared and unchecked, so a profile could claim shared or
  replication on local storage, or map a storage that is disabled on a node, and placement
  failed late at clone time. Detection gives the honest baseline the P3-4 attach gating and
  P3-9 caps enforcement build on.

**Diagrams updated:** c2-containers.md (storage tiers), c3-components.md (C3b diagram and
table), c4-code.md (server create sequence)

## 2026-10-28: Remove OpenStack remnants

**What changed**

- Dead OpenStack code paths are deleted: the gophercloud dependency, the Swift half
  of the object-store client, the `OS_*` / `openstack.*` config keys (the default
  region is now `PROXCLOUD_DEFAULT_REGION` / `proxcloud.default-region`, default
  `default`), the stale `/api/v1/notifications/` public-path entry, and the
  OpenStack-only client, admin and mgmt routes (`/debug/cloud`, metadata, service
  auth and details, discovery, project imports, unassociated projects, placement,
  reseller, availability zones, share protocols, VHI-ostor, Gnocchi granularity,
  volume-type writes) with their MCP tools and admin UI tabs.
- Persisted names now match the backend: table `gnocchiMetrics` is `usageMetrics`,
  job `gnocchiMetricsFetch` is `usageMetricsFetch`, download types
  `OPENSTACK_SWIFT_BUCKET` / `OPENSTACK_CONSOLE` are `OBJECT` / `CONSOLE`
  (`OPENSTACK_S3_OBJECT` is gone), the Gnocchi metrics source and the two Ceilometer
  Prometheus schemas are removed (only `libvirt-exporter` remains, pending a Proxmox
  schema).
- Public Admin API: `configuration.cloud.provider` reports the real provider kind
  (`PROXMOX`, `CEPH_S3`, `KAMAJI`, `DBAAS`) with a `proxmox.pool_prefix` block;
  `provisioned_services[]` carries `provider`, `region` and `pool` instead of the
  `openstack` block, and the `openstack_project_id` filter is gone.
- Managed K8s and DBaaS (C3e): the create-path stubs are renamed
  `cloud_kamaji_create.go` / `cloud_dbaas_create.go`; Cinder CSI, Octavia
  annotations, keystone application credentials, Glance image ids and neutron RBAC
  plumbing are removed, with one `TODO(P7)` at each seam naming the Proxmox piece
  (CAPMOX, proxmox-csi, MetalLB). Read, sync and delete are unchanged.
- CI: the rebrand gate also fails on OpenStack-era terms (this file and
  `countries.json` excluded).

**Why**

- **Misleading operators and API clients.** Dead paths and OpenStack wire fields
  (a fixed `OPENSTACK` provider value, empty `openstack` blocks, admin tabs and MCP
  tools that changed nothing on Proxmox) suggested features that do not exist and
  hid the real provider, pool and region. Names now match the Proxmox backend.
- **No installed base.** There are no deployed installs, so tables, jobs and enum
  values are renamed outright, with no compatibility shims.

**Diagrams updated:** c3-components.md (C3e stub file names). No diagram shape
changed.

## 2026-10-28: PR #16 tenant isolation, VM firewall, quota hardening

**What changed**

- Tenant networks (C2b, C3b, C4.1): every vnet now carries managed forward rules
  that accept the project's own subnets and drop anything else headed into the
  provider's `cidrPool`. New isolation mode `evpn-per-project` gives each project
  its own EVPN zone (id `ZoneIDFor(projectId)`); `shared` stays the default.
- Network create (C4 sequence b): the CIDR is picked under the platform `sdn-apply`
  lock, must sit inside `cidrPool` and must not overlap any subnet on the cluster.
  Isolation rules are written before the SDN apply. `cidrPool` is now required on
  Proxmox providers (breaking).
- VM firewall (C4.2, sequence a): `firewall=1` on every NIC, ipfilter and macfilter
  anti-spoof, inbound default DROP, a per-project `default` security group created at
  bootstrap. Cloned NICs beyond `net0` are removed. The firewall is set before the
  first boot; a failure destroys the VM. Server create now requires one of the
  project's networks (breaking).
- Public IPs (C2b, C3b, C4.2): new public IP pool per public network (`pvePublicIp`
  table) with a `floatingIps` quota; the address is pinned by ipfilter; the
  management bridge is refused.
- Test connection (C2 trust boundaries): refuses a cluster with the datacenter
  firewall off or a node on the legacy iptables firewall; warns on missing token
  privileges and on `root@pam` tokens. Least-privilege roles documented in
  `docs/proxmox-permissions.md`.
- Quota (C3b, C4.3): the provider default quota is seeded onto new projects; the
  gate fails closed (503) when usage cannot be read; resize, volume create and
  extend, and the boot disk are counted.
- Teardown (C4 sequence c): project teardown also removes the project's vnets,
  isolation rules, EVPN zone and security groups, and releases its public IPs. The
  scheduled deletion job now runs the same teardown (it used the old cloud write
  path, which never reached Proxmox resources).
- Identity (C3a, C2 trust boundaries): `email_verified` is read from the token
  and mirrored to `emailConfirmedAt`; org add-member and project invites match
  IdP-verified emails only.

**Why**

- **Cross-tenant reachability.** All tenant vnets shared one SDN zone with no rules
  between them, so a tenant VM could reach another tenant's VMs. The zone routes
  between its vnets; only firewall rules (or a separate VRF plus rules) stop it.
- **Spoofing.** Without `firewall=1`, ipfilter and macfilter, a VM could send with
  any source address, including another tenant's or a public address it did not
  hold. A template made from a VM with a public NIC would also have handed that NIC
  and its address filter to every clone.
- **Unbounded and untracked resources.** Public addresses had no pool and no record
  of who held them, and a failed usage read let a create through. Without a default
  quota a new project could consume the whole cluster. Quota now bounds what is not
  yet billed (public IPs, templates) and fails closed.
- **Account takeover through unverified email.** Anyone can register an account with
  an email address they do not own. Matching people by unverified email let such an
  account be added to an organization or claim a project invite meant for the real
  owner.
- **Silent misconfiguration.** With the datacenter firewall off, or a node on the
  iptables firewall, every rule above is silently unenforced; the preflight turns
  that into an error.

**Diagrams updated:** c1-context.md, c2-containers.md (C2b, trust boundaries),
c3-components.md (C3a, C3b), c4-code.md (C4.1 to C4.3, sequences a, b, c).

## 2026-10-28: Initial C4 model (baseline: initial ProxCloud release)

**What changed**

- Baseline of the model as of the initial ProxCloud release: a multi-tenant
  private-cloud portal and billing system for Proxmox VE. One Go API
  (`/api/v1`, SigV4 `/admin-api/v1`, `/mcp`, SSE, management port), customer and
  operator React consoles, PostgreSQL (JSONB document store), RabbitMQ, Keycloak,
  Helm charts and GitHub Actions CI publishing to ghcr.io.
- The IaaS backend is Proxmox VE (`internal/cloud/pve`): pools per project, full
  clones, SDN networks, cluster-firewall security groups, storage profiles, GPU PCI
  mappings. Tenant buckets run on Ceph RGW.
- Code base rebranded to ProxCloud (module, `PROXCLOUD_*` config, image names), with a
  CI gate that rejects old brand strings.
- Planned elements drawn from the roadmap: installer, Proxmox native metering
  samplers, HA and replication, TrueNAS and RustFS for the Standard storage tier,
  Proxmox Backup Server, per-org SSO, managed Kubernetes and DBaaS create on Proxmox,
  VM and node template pipelines, Airwallex.

**Removed (not drawn)**

- The OpenStack compute, network and identity backend; only the Ceph RGW bucket
  client survives in `internal/cloud/client`.
- The OpenStack notification relay (notifier); resource state now comes from the
  15-minute Proxmox sync.
- OpenStack-only managed-service legs: managed Kubernetes and DBaaS create on
  OpenStack are stubs that fail with an explicit error until the Proxmox versions land.
- Gnocchi as a metrics source; traffic metering uses an optional
  Prometheus-compatible source, off by default for Proxmox providers.

**Why**

- ProxCloud targets Proxmox VE private clouds. Keeping the OpenStack paths would
  have meant shipping code with no backend to run against; the bucket surface was
  backend-agnostic and stayed.

**Diagrams updated:** all four files created.
