# C4 model change log

Every architecture change lands here in the same PR that changes the code and the
diagrams: what changed, why, and which diagrams were updated. Newest first. Format
and rules: [README.md](README.md#maintenance-rule).

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
