# C3: Components of `proxcloud-api`

The API is one Go binary. Packages follow a handler, service, repository layering
over the PostgreSQL document store ([ADR-0002](../../adr/0002-go-and-layered-architecture.md)).
This page groups the packages by area. Paths are relative to the repository root;
every Implemented path exists on the default branch.

Status legend and maintenance rule: [README.md](README.md).

## C3a. Identity and access

```mermaid
flowchart LR
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a
  classDef planned fill:#f2f2f2,stroke:#8a8a8a,stroke-dasharray:5 5,color:#555555

  req["HTTP request"]:::impl
  enforce["pkg/auth Enforce<br/>JWT, SigV4, pk.sk"]:::impl
  oidc["internal/oidc<br/>realm discovery"]:::impl
  rc["pkg/httpx RequestContext<br/>sub, email, email_verified"]:::impl
  acct["account<br/>POST /user, API keys"]:::impl
  users["user.Repo<br/>emailConfirmedAt"]:::impl
  org["org<br/>members, roles, audit"]:::impl
  inv["projectinvite"]:::impl
  rbac["rbac matcher"]:::impl
  adminapi["adminapi<br/>SigV4 public admin API"]:::impl
  mcp["mcp toolsets"]:::impl
  sso["Per-org SSO<br/>discover, JIT, enforce"]:::planned
  kcadm["Keycloak Admin API client"]:::planned

  req --> enforce
  oidc --> enforce
  enforce --> rc
  rc --> acct
  rc --> org
  rc --> inv
  rc --> mcp
  enforce --> adminapi
  acct --> users
  org -->|"verified email only"| users
  inv -->|"verified email only"| users
  org --> rbac
  acct -.-> sso
  sso -.-> kcadm
  sso -.-> org
```

| Component | Path | Responsibility | Status |
|---|---|---|---|
| Authenticator | `pkg/auth/auth.go` (`Enforce`, `verifyJWT`, `claimTrue`), `sigv4.go`, `sigv4sign.go`, `clientkey.go` | Validates OIDC JWTs per realm (bound to `azp`), AWS SigV4 for the admin API, and `pk.sk` personal access keys. Reads `email_verified` (fail closed: absent means false). | Implemented |
| Request context | `pkg/httpx/context.go` | Per-request principal: `Sub`, `Email`, `EmailVerified`, `Issuer`, `Azp`, `SigV4KeyID`. | Implemented |
| OIDC discovery | `internal/oidc/oidc.go` | Discovers the configured realms (customer, admin, admin API). | Implemented |
| Config | `internal/config/config.go` | koanf config: auth realms, ports `8080`/`8081`, `PROXCLOUD_*` env overrides. | Implemented |
| Account | `internal/platform/account/account.go`, `account_apikeys.go`, `dto.go` | `POST /user` creates or refreshes the user from the token claims; personal access keys. | Implemented |
| User | `internal/platform/user/user.go` (`FindVerifiedByEmail`, `EmailVerified`) | User store keyed by `sub`; `emailConfirmedAt` mirrors the IdP's `email_verified` for the current email. | Implemented |
| Organization | `internal/platform/org/` (`handler.go` `addMember`, `role_*.go`, `audit_handler.go`, `billing_handler.go`) | Organizations, members, custom roles, org audit log, billing-profile reads. Add-member resolves only IdP-verified emails. | Implemented |
| Project invites | `internal/platform/projectinvite/projectinvite.go` (`callerEmail`) | Token invites to a project. Invites match the caller only once their email is verified. | Implemented |
| RBAC | `internal/platform/rbac/` (`matcher.go`, `permission.go`, `roles.go`) | Permission kernel for org, project and admin permissions. | Implemented |
| Public admin API | `internal/platform/adminapi/` | `/admin-api/v1`: users, organizations, billing profiles, projects, bills, credits, service providers. | Implemented |
| MCP server | `internal/platform/mcp/` (`mcp.go`, `tools_client.go`, `tools_admin*.go`) | Client and admin toolsets, dispatched in-process through the app router. | Implemented |
| Audit | `internal/platform/audit/`, `pkg/audit/` | Audit events, field-level diffs, cursor paging. | Implemented |
| Per-org SSO | not yet in the tree | Org IdP and domain models, Keycloak Admin API client, email-first discovery, JIT membership, enforcement middleware. | Planned |

## C3b. Cloud / IaaS on Proxmox

```mermaid
flowchart TB
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a
  classDef planned fill:#f2f2f2,stroke:#8a8a8a,stroke-dasharray:5 5,color:#555555

  ph["project cloud handlers<br/>cloud_writes_pve, cloud_network_pve"]:::impl
  quota["quota gate<br/>cloud_quota_pve, gpuquota"]:::impl
  pip["public IP pool<br/>publicip_pve"]:::impl
  lifecycle["bootstrap and teardown"]:::impl
  es["externalservice proxmox.go<br/>network, profiles, quota"]:::impl
  svc["pve.Service<br/>write, volume, gpu, console"]:::impl
  net["pve network + sdn<br/>vnets, isolation, zones"]:::impl
  fwsvc["pve firewallsvc + firewall<br/>security groups, anti-spoof"]:::impl
  sch["pve.PickNode scheduler"]:::impl
  cli["pve.Client REST"]:::impl
  sync["syncjob + pve sync providers"]:::impl
  recon["providers.Reconcile"]:::impl
  repo["cloud.Repo<br/>resource cache"]:::impl
  lock["SDN apply lock"]:::impl
  cap["capability detection<br/>pve capability.go"]:::impl
  ha["replication + HA"]:::planned
  truenas["TrueNAS client"]:::planned

  ph --> quota
  ph --> pip
  ph --> es
  ph --> svc
  ph --> net
  ph -->|"serializes SDN apply"| lock
  lifecycle --> net
  lifecycle --> fwsvc
  svc --> sch
  svc --> fwsvc
  svc --> cli
  net --> cli
  fwsvc --> cli
  ph --> repo
  sync --> svc
  sync --> recon
  recon --> repo
  cap --> cli
  sync --> cap
  es --> cap
  ha -.-> cli
  ph -.-> truenas
```

| Component | Path | Responsibility | Status |
|---|---|---|---|
| PVE client | `internal/cloud/pve/client.go`, `cluster.go`, `qemu.go`, `storage.go`, `sdn.go`, `firewall.go`, `pool.go`, `access.go`, `task.go`, `mapping.go`, `form.go` | Typed REST client: cluster resources, VMs, storage, SDN, cluster/VM firewall, pools, privilege checks, tasks, PCI mappings. Includes the preflight reads `DatacenterFirewallEnabled`, `NodesWithoutNFTables`, `ManagementBridges`. | Implemented |
| PVE service | `internal/cloud/pve/service.go`, `write.go`, `volume.go`, `gpu.go`, `console.go` | Project-scoped IaaS: pools, create/delete/resize/power/rename servers, project templates, volumes (add, extend, detach, attach with cross-node gating and attach targets, retype, delete detached), GPU inventory and slot picking, VNC/serial console. | Implemented |
| Networks and isolation | `internal/cloud/pve/network.go` (`CreateNetwork`, `ReconcileIsolation`, `EnsureProjectZone`, `ZoneIDFor`, `TeardownProjectNetworks`), `sdn.go` | Tenant vnets and subnets, ownership alias, cluster firewall isolation rules, per-project EVPN zones, staged SDN changes and apply. | Implemented |
| Firewall service | `internal/cloud/pve/firewallsvc.go` (`SecureVM`, `SetIPFilter`, `EnsureDefaultSecurityGroup`, `ServerSecurityGroups`, `DeleteProjectSecurityGroups`) | Security groups as cluster firewall groups, the project default group, VM firewall options, per-NIC ipfilter sets. | Implemented |
| Scheduler | `internal/cloud/pve/scheduler.go` (`PickNode`), `capability.go` (`StorageHasRoom`, `TemplateShared`) | Capacity-ranked node choice with CPU overcommit, storage-profile node restriction, free space on the profile's storage (with headroom), template locality and free GPU slots. | Implemented |
| Sync providers | `internal/cloud/pve/syncprovider.go`, `network.go` (`NetworkSyncProvider`), `firewallsvc.go` (`SecurityGroupSyncProvider`) | Map pool VMs, disks, templates, owned vnets and groups to cache rows. | Implemented |
| Reconcile | `internal/cloud/providers/providers.go` (`Reconcile`, `Deletable`), `compare.go`, `niche_sync.go`, `write.go` | Create, update and archive cache rows against the live list. | Implemented |
| Sync job | `internal/cloud/syncjob/job.go` (`syncProxmoxService`, `SyncOne`, `detectProxmoxCapabilities`) | 15-minute sync of every enabled project, plus on-demand single-project sync; each cycle also re-detects every Proxmox provider's storage capabilities. | Implemented |
| Resource cache | `internal/cloud/repo.go`, `resource.go`, `history.go` | The `cloudResource` table and its archive; the source for reads, quota usage and billing. | Implemented |
| Project cloud handlers | `internal/platform/project/cloud_writes.go`, `cloud_writes_pve.go` (`pveCreateServer`, `pveAction`, `pveDeleteResource`), `cloud_reads.go`, `clientcloud.go` | `/project/{id}/cloud` create, delete, actions and reads; Proxmox branch for servers, keypairs, volumes, security groups, images. `/init/{projectId}` returns the menu (minus `hiddenServices`) and per-provider `features` (`cloudFeatures`) that the client uses to hide actions the backend would reject. | Implemented |
| Network handlers | `internal/platform/project/cloud_network_pve.go` (`pveCreateNetwork`, `pveDeleteNetwork`, `pvePickCIDR`, `pvePublicNetworkAction`, `pveTeardownNetworks`) | Network create/delete under the SDN apply lock, CIDR pick, public NIC attach/detach. | Implemented |
| Quota | `internal/platform/project/cloud_quota_pve.go` (`checkPVEQuota`), `cloud_writes_pve.go` (`pveResizeDemand`, `enforcePVEGPUQuota`), `gpuquota.go`, `quota_usage.go` | Platform-side quota for servers, vCPUs, RAM, disk, networks, public IPs and GPUs; fails closed when usage cannot be read. | Implemented |
| Public IP pool | `internal/platform/project/publicip_pve.go` (`PublicIPRepo`, `pickPublicIP`) | Per public network IPv4 pool in the `pvePublicIp` table; the primary key is the race guard. | Implemented |
| Bootstrap | `internal/platform/project/bootstrap.go` (`BootstrapProxmoxOnto`) | Project pool, default security group, provider default quota seeded onto the project. | Implemented |
| Teardown | `internal/platform/project/teardown.go` (`TeardownProject`), `deletion.go` | Destroys pool VMs and the pool, vnets, EVPN zone, security groups, releases public IPs, archives cache rows. | Implemented |
| Provider config | `internal/platform/externalservice/proxmox.go` (`PVENetworkConfig`, `PVEStorageProfiles`, `PVECapabilities`, `PVEStorageClasses`, `PVEDefaultQuota`) | Typed view of a Proxmox provider: API endpoint and token, zones, `cidrPool`, isolation mode, public networks, storage profiles (with derived tier and class and the `bootDisk` / `dbaas` caps), detected storage capabilities and class overrides, the per-disk profile resolver for volume billing, GPU aliases, default quota. | Implemented |
| Admin provider tools | `internal/platform/admin/pveadmin.go` (`proxmoxAuth`, `tokenWarnings`, `checkNodeFirewalls`, `checkPublicNetworks`), `cloudadmin.go`, `pveflavors.go`, `projectquota.go` | Test connection with preflight, provider CRUD, flavor catalog, per-project quota edits. | Implemented |
| Flavor catalog | `internal/platform/pveflavor/pveflavor.go` | Admin-curated flavors for Proxmox, including GPU model and count and a storage profile. | Implemented |
| GPU | `internal/cloud/gpu.go`, `gpu_usage.go`, `internal/platform/project/gpucapacity.go` | GPU alias normalization, usage by project, capacity view. | Implemented |
| Capability detection | `internal/cloud/pve/capability.go` (`DetectStorage`, `classify`, `StorageHasRoom`, `TemplateShared`), `externalservice/proxmox.go` (`ValidatePVEStorageProfiles`, `DetectPVECapabilities`, `PVEEffectiveStorageClass`), `syncjob/job.go` (`detectProxmoxCapabilities`), `admin/pveadmin.go` (`proxmoxDetectStorage`, `proxmoxStorageClasses`, `validatePVEStorageProfiles`) | Per storage from `GET /storage` + `/cluster/resources type=storage`: type, shared, content, snapshots, thin, live migrate, replication, tier and a default class from a static type table, plus per-node active/avail. Persisted under `config.capabilities` every sync cycle and on admin Re-detect. Storage-profile saves are rejected when a storage is missing, inactive, lacks `images`, or the profile claims a flag the storage lacks. Admin class override (`config.storageClasses.<id>.class`) wins; the installer's `suggestedClass` is display-only. Feeds placement (active nodes, free space, template and clone-target locality), same-node attach gating and the capacity-class gates (boot disk, snapshots, DBaaS). HA readiness is still Planned. | Implemented |
| Replication and HA | not yet in the tree | `pvesr` jobs and HA node-affinity rules for protected VMs. | Planned |
| Metering samplers | not yet in the tree | Minute-level VM state sampling from `/cluster/resources`, task log polling, native NIC counters. | Planned |
| TrueNAS client | not yet in the tree | Shares, datasets and quotas through the TrueNAS WebSocket API. | Planned |
| VM metadata datasource | not yet in the tree | Per-VM cloud-init datasource served by the API. | Planned |

## C3c. Metering and billing

```mermaid
flowchart LR
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a
  classDef planned fill:#f2f2f2,stroke:#8a8a8a,stroke-dasharray:5 5,color:#555555

  cron["scheduler + lock"]:::impl
  sync["servicesSync 15m"]:::impl
  mj["metricsjob<br/>traffic counters"]:::impl
  prom["metrics.Prometheus"]:::impl
  repo["cloud.Repo"]:::impl
  mrepo["metrics.Repo"]:::impl
  bp["billingprovider<br/>native or external"]:::impl
  fan["chargefanout"]:::impl
  bj["billingjob"]:::impl
  br["billingresource providers"]:::impl
  price["pricing engine"]:::impl
  bill["billing<br/>bills, suspension, reminders"]:::impl
  pay["payment<br/>Stripe"]:::impl
  susp["suspend and resume<br/>Proxmox VMs"]:::impl
  samplers["Proxmox state and task samplers"]:::planned
  airwallex["Airwallex gateway"]:::planned

  cron --> sync
  cron --> mj
  cron --> bp
  sync --> repo
  mj --> prom
  mj --> mrepo
  bp --> fan
  bp --> bj
  fan --> bj
  bj --> br
  br --> repo
  br --> mrepo
  bj --> price
  price --> bill
  bill --> pay
  bill --> susp
  samplers -.-> repo
  pay -.-> airwallex
```

| Component | Path | Responsibility | Status |
|---|---|---|---|
| Scheduler | `internal/platform/scheduler/scheduler.go`, `internal/platform/lock/shedlock.go` | Cron specs (charge every minute, hour and month; sync every 15 minutes; dunning, collect, bill send) run once across pods. | Implemented |
| Operator job triggers | `internal/platform/job/job.go` | `/api/v1/admin/job/*` triggers for charge, metrics, sync, collect. | Implemented |
| Metrics | `internal/cloud/metrics/` (`prometheus.go`, `service.go`, `repo.go`), `internal/cloud/metricsjob/job.go` | Month-to-date VM traffic from a Prometheus-compatible source, per provider (opt-in; Proxmox defaults to none). | Implemented |
| Billing resources | `internal/cloud/billingresource/` (`billingresource.go` `GetBillingResources`, `server.go`, `volume.go`, `floatingip.go`, `bucket.go`, `loadbalancer.go`, `kubernetescluster.go`, `databasecluster.go`, `catalog.go`) | Turn cache rows into billable units with typed attributes; servers yield an instance unit plus a traffic unit; volumes carry `profile` and `storage_class` for per-profile price rules. | Implemented |
| Billing provider | `internal/platform/billingprovider/billingprovider.go` | Native (rate in process) or external (resolve here, rate in a separate billing service). | Implemented |
| Billing job | `internal/platform/billingjob/billingjob.go` (`Charge`, `ChargeProfileByID`), `remote.go` | Per active billing profile and provider: resolve billing resources, pick price plans, rate onto the current bill. | Implemented |
| Charge fan-out | `internal/platform/chargefanout/chargefanout.go` | One RabbitMQ message per profile, off by default. | Implemented |
| Billing client and callbacks | `internal/platform/billingclient/`, `internal/platform/billingcallback/`, `pkg/billingapi/` | Wire contract and callbacks for an external billing service. | Implemented |
| Pricing | `internal/platform/pricing/` (`engine.go`, `charge.go`, `planselect.go`, `bill_build.go`, `adjustments.go`, `tax.go`, `fx.go`, `settle.go`) | Rating engine, bill items, savings and price adjustments, tax, currency, settlement. | Implemented |
| Billing | `internal/platform/billing/` (`balance.go`, `sendbill.go`, `suspension_job.go`, `activationservice.go`, `reminder.go`, `savings.go`, `billpdf.go`) | Billing profiles, balance, bill finalization, PDF, dunning and suspension, reminders, savings contracts. | Implemented |
| Proxmox suspend/resume | `cmd/api/main.go` (`billingCloudSuspender`) | Suspends and resumes a suspended profile's Proxmox VMs. | Implemented |
| Payment | `internal/platform/payment/` (`stripe.go`, `addfunds.go`, `collect.go`, `registercard.go`, `scanner.go`) | Stripe deposits, cards, collect, transaction scanner; bank transfer settlement. | Implemented |
| Proxmox native samplers | not yet in the tree | Minute-level state and task sampling, native NIC counters, VM identity stamp. | Planned |
| Public IP and image billing | not yet in the tree | Billing units for allocated public IPs and project templates. | Planned |
| Protected VM billing | not yet in the tree | Billing unit for replicated, HA-protected VMs. | Planned |
| Airwallex gateway | not yet in the tree | Second payment gateway. | Planned |

## C3d. Platform services

```mermaid
flowchart LR
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a
  classDef planned fill:#f2f2f2,stroke:#8a8a8a,stroke-dasharray:5 5,color:#555555

  router["internal/server<br/>app and mgmt routers"]:::impl
  admin["admin<br/>/api/v1/admin/**"]:::impl
  pcfg["platformconfig"]:::impl
  feat["feature"]:::impl
  cat["catalog"]:::impl
  es["externalservice"]:::impl
  mail["mail"]:::impl
  msg["message templates"]:::impl
  sse["sse"]:::impl
  promo["promotion"]:::impl
  pg["internal/pgdoc"]:::impl
  amqp["internal/amqp"]:::impl

  router --> admin
  router --> pcfg
  router --> feat
  router --> cat
  router --> promo
  router --> sse
  admin --> es
  admin --> msg
  msg --> mail
  admin --> pg
  es --> pg
  router --> amqp
```

| Component | Path | Responsibility | Status |
|---|---|---|---|
| Routers | `internal/server/server.go` | App router (`:8080`) and management router (`:8081`). | Implemented |
| Admin | `internal/platform/admin/` | All operator routes: users, organizations, projects, providers, price plans, billing config, transactions, integrations, admin roles, HMAC keys, audit. | Implemented |
| External services | `internal/platform/externalservice/` (`externalservice.go`, `proxmox.go`, `kamaji.go`, `dbaas.go`, `service.go`, `repo.go`) | Provider records (Proxmox, Ceph S3, Kamaji, DBaaS) with encrypted secrets. | Implemented |
| Platform config | `internal/platform/platformconfig/` | Branding, regions, login and provisioning settings; `hiddenServices` hides managed services from every tenant menu. | Implemented |
| Feature flags | `internal/platform/feature/handler.go` | Feature set the SPAs query. | Implemented |
| Catalog | `internal/platform/catalog/catalog.go` | Flavor categories, image groups. | Implemented |
| Mail and templates | `internal/platform/mail/` (`smtp.go`, `service.go`), `internal/platform/message/` | SMTP delivery, templated messages. | Implemented |
| SSE | `internal/platform/sse/` | Per-project event stream. | Implemented |
| Promotion, affiliate | `internal/platform/promotion/`, `internal/platform/affiliate/` | Promotion codes, affiliate config. | Implemented |
| Order | `internal/platform/order/order.go` | Order read by id. | Implemented |
| CRM | `internal/platform/crm/crm.go` | CRM sync interface with a no-op provider. | Implemented |
| Paging | `internal/platform/paging/` | Cursor paging. | Implemented |
| Data store | `internal/pgdoc/` | PostgreSQL JSONB document API used by every repository. | Implemented |
| Broker | `internal/amqp/amqp.go` | RabbitMQ connection and reconnect. | Implemented |
| Health | `internal/health/` | Liveness and readiness. | Implemented |

## C3e. Managed services and object storage

```mermaid
flowchart LR
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a
  classDef planned fill:#f2f2f2,stroke:#8a8a8a,stroke-dasharray:5 5,color:#555555

  ph["project handlers<br/>cloud_kamaji, cloud_dbaas, cloud_bucket_settings"]:::impl
  kam["kamaji<br/>values, sync"]:::impl
  dba["dbaas<br/>values, sync, autoscale"]:::impl
  k8s["kamajik8s<br/>K8s API client"]:::impl
  obj["client objectstore<br/>Ceph RGW backend"]:::impl
  ccred["cephcred"]:::impl
  argo["ArgoCD on the management cluster"]:::impl
  rgw["Ceph RGW"]:::impl
  pvek8s["Managed K8s create on Proxmox"]:::planned
  pvedb["DBaaS create on Proxmox"]:::planned
  rustfs["RustFS backend"]:::planned

  ph --> kam
  ph --> dba
  ph --> obj
  kam --> k8s
  dba --> k8s
  k8s --> argo
  obj --> ccred
  obj --> rgw
  kam -.-> pvek8s
  dba -.-> pvedb
  obj -.-> rustfs
```

| Component | Path | Responsibility | Status |
|---|---|---|---|
| Managed Kubernetes | `internal/cloud/kamaji/`, `internal/cloud/kamajik8s/`, `internal/platform/project/cloud_kamaji.go` | Cluster values builder, Kubernetes API client, sync, read and delete of Kamaji clusters through ArgoCD `Application` objects. | Implemented |
| Managed K8s create on Proxmox | `internal/platform/project/cloud_kamaji_create.go` (stub) | Credential mint, IP range carving and worker placement on Proxmox. Create currently fails with an explicit error. | Planned |
| DBaaS | `internal/cloud/dbaas/`, `internal/platform/project/cloud_dbaas.go`, `deploy/charts/database-cluster` | Database cluster values, sync, autoscale, read and delete. Create refuses a storage class named after a Proxmox storage profile closed to databases (capacity class). | Implemented |
| DBaaS create on Proxmox | `internal/platform/project/cloud_dbaas_create.go` (stub) | Database endpoints on a routed services subnet through MetalLB. Create currently fails with an explicit error. | Planned |
| Object store client | `internal/cloud/client/client.go`, `objectstore.go`, `objectstore_ceph.go`, `objectstore_ceph_policy.go`, `objectstore_ceph_settings.go` | Bucket and object operations, policies, settings on Ceph RGW. | Implemented |
| Ceph credentials | `internal/cloud/cephcred/` | Per-project RGW user and S3 keys, secret encrypted at rest. | Implemented |
| Bucket settings | `internal/platform/project/cloud_bucket_settings.go` | Bucket actions (versioning, object lock, quota, lifecycle, CORS, tags, policy, per-key grants) and project S3 keys. | Implemented |
| RustFS backend | not yet in the tree | Second object-store backend behind the same bucket surface. | Planned |
