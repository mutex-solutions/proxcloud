# 9. Managed-services platform: two provisioning paths, one lifecycle

## Status

Accepted, as amended on 2026-11-15 (owner decisions of 2026-11-14 and 2026-11-15). The
review that merges the amendments is the owner's sign-off. Not implemented yet.

The Decision below is the proposal of 2026-11-14, kept as written. Where an amendment
changes a section, the section ends with a note, and
[Amendments (2026-11-15)](#amendments-2026-11-15) holds the decision in force.

## Context

The portal alone (API, consoles, Keycloak, PostgreSQL) now installs with one command on a
single Docker host (`deploy/compose`). Managed Kubernetes, managed databases and sandboxes
need more: a Cluster API management cluster, one or more Kubernetes clusters on Proxmox
VMs, and the operators that run on them (Kamaji, CloudNativePG, MetalLB, proxmox-csi, the
sandbox stack).

Today only the Ansible installer builds that platform (`make install`, mode `split`,
stages 50 to 60). It works, but an operator who started from the one-host install has no
way to add managed services without the installer, and there is no defined answer to
these questions:

- Which versions of the platform components belong to a ProxCloud release?
- How is an upgrade ordered, checked and rolled back?
- What may a patch release change, and what needs a minor release?
- How do a site built by the installer and a site built from the portal stay the same
  thing to operate?

## Decision

### Two provisioning paths that converge

Both paths stay supported.

- **Installer path.** The operator provisions from a workstation with `make install`. It
  fits high-availability and larger sites.
- **In-platform path.** The operator presses "Enable managed services" in the operator
  console. A durable job in the portal provisions the platform.

Only the bootstrap differs: who creates the first VM and the first cluster. From the
moment ArgoCD runs and its Applications are applied, both paths share one specification
and one implementation:

- Both read the same platform bill of materials (below).
- Both end by installing the same ArgoCD Applications from the same pinned charts with
  the same values schema. Neither applies a chart outside ArgoCD after bootstrap.
- Both write a `PlatformInstallation` record in the portal: BOM version, the version of
  every component, cluster endpoints, node groups, and which path provisioned it.
- A site built by the installer shows the same status and upgrade screen in the console.
  A site built from the portal can be upgraded with `make upgrade`. The button and the
  make target run the same ordered plan and the same preflight.

*Amended: sites built before this lifecycle stay observe-only
([A9](#a9-existing-installer-sites-stay-observe-only)).*

### Seed cluster

A single k3s VM is the long-lived management cluster ("seed"). It runs Cluster API, the
Proxmox provider (CAPMOX) and ArgoCD, and no tenant workload. It creates and manages one
or more workload clusters. Each workload cluster's autoscaler asks the seed to scale its
node groups.

The seed's etcd is snapshotted to the NAS on a schedule. If the seed is lost, the workload
clusters keep running but cannot scale, repair nodes or upgrade until the seed is restored
on a fresh VM from the node template.

*Amended: the seed's image and pool ([A2](#a2-seed-vm-from-a-native-template-in-its-own-pool)),
its ArgoCD and cert-manager ([A4](#a4-seed-argocd-and-cert-manager)), which Cluster API
manages what ([A5](#a5-the-workload-cluster-keeps-its-own-argocd-and-tenant-cluster-api)),
its backup and restore ([A7](#a7-seed-backup-by-a-pve-backup-job)).*

### Cluster layout is the operator's choice

The operator chooses one of:

- **Shared:** one workload cluster with a node group per service (`system`, `kamaji`,
  `dbaas`, `sandbox`). Each group is its own MachineDeployment with labels and taints, and
  each service is pinned to its group.
- **Separate:** one workload cluster per service.

*Amended: the first milestone builds the shared layout without a `sandbox` group, and the
sandbox's shape is open until the third milestone
([Scope of the first milestone](#scope-of-the-first-milestone),
[open points](#open-points-for-the-implementation)).*

### One version source: the platform BOM

Every ProxCloud release ships `platform-bom.yaml`. It lists the exact version of every
component, never a range or `latest`:

- k3s, cert-manager, Cluster API core and kubeadm providers, CAPMOX, ArgoCD;
- the workload cluster's Kubernetes version and the published node template digest;
- Kamaji, CloudNativePG and the other database operators, MetalLB, proxmox-csi, the
  sandbox charts, the sandbox registry;
- each chart pinned by OCI digest.

The installer's `versions.yml` is generated from this file, or a test keeps the two equal.

A component version changes only through a ProxCloud release. The release is tested end to
end on a lab before its tag is pushed.

*Amended: digests of charts in HTTP repositories, verification by CI, development BOMs
([A6](#a6-chart-digests-verified-by-ci-until-a-mirror-exists)).*

### Delivery: Helm through ArgoCD

Every component is an ArgoCD `Application` whose source is the pinned OCI Helm chart. The
values come from Secrets and ConfigMaps that the provisioning path writes, so no Git
repository is needed.

- Stateless components sync automatically and self-heal.
- Stateful components (database clusters, Kamaji datastores, CSI) are never pruned
  automatically and sync in explicit waves.

*Amended: values inline ([A1](#a1-helm-values-inline)), the Cluster API providers
([A3](#a3-cluster-api-providers-by-a-clusterctl-job)), the seed's ArgoCD
([A4](#a4-seed-argocd-and-cert-manager)).*

### Upgrade

1. A new ProxCloud release is deployed. The portal compares its BOM with the installed one
   and shows the component differences.
2. A preflight job must pass before anything changes:
   - each component's allowed upgrade paths (no skipped versions);
   - deprecated or removed APIs still in use, checked against the target versions;
   - the Cluster API contract;
   - fresh backups;
   - cluster health.
3. The upgrade runs in ordered waves, each gated on health, with pause and resume:
   cert-manager, Cluster API core, CAPMOX, ArgoCD, the workload cluster's Kubernetes
   version (rolling), then the operators.
4. Stateless components can be rolled back to the previous BOM. Stateful operators and
   Kubernetes itself are forward-only, as upstream; the preflight and the backups protect
   them.

*Amended: the order of step 3 ([A10](#a10-upgrade-order-is-the-boms-wave-order)).*

### Compatibility rules

- A **patch** release may change component patch versions only: no CRD storage-version
  change, no Kubernetes minor, no operator minor.
- A **minor** release may change component minors, and the workload cluster's Kubernetes
  version by one minor.
- A breaking component change (a CRD removal, a Cluster API contract change, a datastore
  migration) needs an explicit migration step in its BOM entry and a lab upgrade test from
  the previous minor.
- The portal supports the platform BOM of its own minor and the one before. The portal is
  always upgraded first and the platform after.

### Tenant resources

Tenant Kubernetes clusters and database instances are upgraded by their owners from the
portal, within the versions the BOM's operators support. A platform upgrade never changes
a tenant cluster's Kubernetes version or a database's major version.

## Consequences

- An operator can start with the one-host install and add managed services from the
  console, without the installer.
- The provisioning logic of installer stages 50 to 60 has to exist a second time, in the
  portal's job. The shared specification (BOM, Application set, upgrade plan, preflight)
  limits the duplication to the bootstrap.
- ArgoCD becomes a required component of every managed-services site.
- Every component bump needs a ProxCloud release, including security patches of a single
  operator. Patch releases have to stay cheap.
- The seed is a single VM. Losing it stops scaling and repair until it is restored, so its
  backup and restore need a tested runbook.
- Supporting two platform versions per portal minor adds test combinations.

*Amended: [Consequences of the amendments](#consequences-of-the-amendments).*

## Amendments (2026-11-15)

The owner decided these on 2026-11-14 and 2026-11-15, or they follow from those decisions,
after the design of the in-platform path ("P3: Enable managed services") was checked
against the code. Each amendment says
what changes in the Decision and why. Everything else above stands.

### A1. Helm values inline

**Change.** An Application carries its non-secret Helm values inline
(`spec.source.helm.valuesObject`). Secret values never appear in an Application: the
provisioning path writes them into a Secret, and the chart refers to that Secret by name.
This replaces "the values come from Secrets and ConfigMaps that the provisioning path
writes".

**Why.** ArgoCD has no Helm values source that reads a Secret or a ConfigMap. Its sources
are inline values and values files in the chart or in a Git repository, and a Git
repository is ruled out. The Kubernetes and Databases providers already build their
Applications this way (`kamaji.BuildApplication`, `dbaas.BuildApplication`), so tenant and
platform Applications have one shape.

### A2. Seed VM from a native template in its own pool

**Change.** A seed the portal builds (`seed.mode: vm`) is a full clone of the published
native OS image (`proxcloud-vm-ubuntu-2604`), not of the Kubernetes node template. The BOM
pins the image by digest, and the portal imports it through Template updates like a node
image. The VM lives in a new pool, `proxcloud-seed`, and is created with the platform's
CAPI token `capmox@pve!capi`. The portal token and `csi@pve` have NoAccess on that pool.
The workload clusters' VMs stay in `proxcloud-system`, where the installer puts them. This
replaces "restored on a fresh VM from the node template" and the open point's "cloud-init
from the published node template".

On the installer path the seed is the mgmt k3s VM that stage 25 clones from a vendor base
template into `proxcloud-system` (`pc.pve.system_pool`). It stays there when the installer
provisions through the portal's API in the second milestone (`seed.mode: existing`), and
`csi@pve!csi` holds `ProxCloudCSI` there, `VM.Config.Disk` included. The pool fence does
not apply to installer seeds until they are moved into `proxcloud-seed`, which the design
leaves to adoption (design §6).

**Why.** The native image is 6 GiB against 20 GiB and carries no kubeadm or kubelet units
next to k3s's own. The seed holds admin rights over every platform cluster, so it must be
out of reach of the workload's CSI token, which needs `VM.Config.Disk` on
`proxcloud-system` and which the providers' service account on the workload can read. A
pool of its own keeps that token off the seed's VM: it cannot read or change the VM's
configuration. Creating the seed with the portal token would need rights for that token on
the pool, `VM.GuestAgent.Unrestricted` among them, which would give the portal root on the
seed.

**Limit.** The pool fences the seed's VM, not its disk. The disk is a volume on the boot
store, which is also the store of the `std` StorageClass, and `csi@pve!csi` keeps
`ProxCloudCSI` on `/storage/<boot>`. That role holds `Datastore.Allocate`, and PVE's
volume check accepts any volume on a store for a principal with `Datastore.Allocate` there,
whoever owns the volume; that is how proxmox-csi attaches its `vm-9999-pvc-*` volumes
(`docs/proxmox-permissions.md`: it "opens every disk on it"). Whoever holds the CSI token
can therefore attach the seed's disk to a workload VM and read the k3s datastore, the
server CA key, the secrets-encryption key, the CAPI token and the portal's service-account
token. The first milestone accepts this ([A8](#a8-risks-accepted-for-the-first-milestone)).
The fix is a boot store for the seed, and a backup store for its backups
([A7](#a7-seed-backup-by-a-pve-backup-job)), where `csi@pve` holds nothing.

### A3. Cluster API providers by a clusterctl Job

**Change.** On the seed and on each workload cluster, the Cluster API providers are
installed by a `clusterctl` Job, run as an ArgoCD Sync hook of a new pinned chart of ours,
`proxcloud-platform`. Its image, `proxcloud-clusterctl`, is built by GitHub Actions with
clusterctl and kubectl built from upstream source pinned by sha256 and the provider
manifests of the BOM baked in as a local repository; the BOM pins the image by digest. The Job installs only the providers
that are missing, does nothing when the installed set equals the BOM, and refuses when an
installed provider has another version: provider upgrades come with the upgrade run.

**Why.** It is the tool and the provider inventory installer sites already have
(`clusterctl init` in stage 50), so both paths stay one thing to operate. The image digest
pins every provider manifest, and nothing calls GitHub at run time. The
cluster-api-operator would add a component and an inventory that installer sites do not
have.

### A4. Seed ArgoCD and cert-manager

**Change.** The seed's ArgoCD is the one component installed outside an Application,
because it is what applies them: a k3s `HelmChart` resource that the portal creates through
the Kubernetes API, never a file in the k3s manifests directory. cert-manager on the seed is
an Application pinned by the BOM, in the shape installer stage 40 uses today, and runs
before the Cluster API Job. Stage 40 moves its HelmChart from the manifests directory to the
API when the installer converges, so both paths own the seed's ArgoCD and cert-manager the
same way.

**Why.** k3s re-applies every file in its manifests directory at each start, which would
undo an upgrade made through the API. cert-manager is a Cluster API prerequisite; as an
Application the BOM pins and upgrades it like every other component, and `clusterctl` does
not install its own when one exists.

### A5. The workload cluster keeps its own ArgoCD and tenant Cluster API

**Change.** The seed's Cluster API manages the platform's workload clusters only. A
workload cluster that runs Kamaji or the database operators has its own ArgoCD and its own
Cluster API (core, kubeadm bootstrap, the Kamaji control plane, CAPMOX, IPAM and the Helm
add-on provider), as the services cluster of an installer site has today. Tenant Kubernetes
clusters and databases are Applications of that ArgoCD and Cluster API objects of that
cluster. The seed's ArgoCD installs the platform components into the workload cluster; the
seed itself runs Cluster API, CAPMOX, ArgoCD and cert-manager, and nothing of a tenant.

**Why.** The Kubernetes and Databases providers (`internal/cloud/kamaji`,
`internal/cloud/dbaas`) create tenant Applications and Cluster API objects in the cluster
they are registered with, next to Kamaji and CloudNativePG. Keeping that keeps where the
providers create tenant objects, and their registration model (one provider row per
service, as installer stage 60 writes it), unchanged. The providers gain configuration for
node-group placement and the database egress fence, and the registered bodies carry it
(design §4.2 and §4.4). Tenant objects never reach the seed, so its set of components
stays small and fixed, and a lost seed does not stop tenant clusters from scaling or
repairing.

### A6. Chart digests verified by CI until a mirror exists

**Change.** The BOM records an exact version and a digest for every chart: for a chart in
an OCI registry its manifest digest, for a chart in an HTTP Helm repository the `digest` of
that version in the repository's `index.yaml`. ArgoCD still pulls by version. A GitHub
Actions job (`bom-verify`) pulls every chart and image of the BOM anonymously at every BOM
change and at the release tag, and fails on any difference. A version that is not exact
(Kamaji's `0.0.0+latest`) or a digest not yet known (`pending`) is allowed only in a BOM
whose release ends in `-dev`; the release gate on the tag refuses both. Our packages
`proxmox-kamaji-cluster`, `database-cluster`, `proxcloud-platform` and
`proxcloud-clusterctl` are, like everything public, built and published by GitHub Actions
only. Since 2026-11-16 all four are published and public on GHCR (`bom-verify` reads them
anonymously), so pulls need no credentials. This replaces "each
chart pinned by OCI digest" and qualifies "never a range or `latest`" for development
BOMs.

**Why.** The installer pulls cilium, cert-manager, MetalLB, CloudNativePG and Kamaji from
HTTP Helm repositories (`services-platform.sh`), which serve no OCI digest. That is the
installer's choice, not always an upstream limit: cert-manager's install guide calls its
OCI chart, `oci://quay.io/jetstack/charts/cert-manager`, the source of truth and its HTTP
repository a legacy one that updates hours later. Wherever upstream publishes a chart in an
OCI registry, the BOM may take it from there and record its manifest digest. The installer
installs Kamaji as chart `0.0.0+latest`, pinned by image tag only. ArgoCD's target revision
for a chart is a version, and the portal cannot look up third-party digests
(`internal/templateupdates` talks only to our GHCR organisation). Until a mirror under our
GHCR organisation serves the checked charts, a chart changed upstream behind the same
version is caught by CI, not at pull time; that gap is stated, not hidden. The flow that
publishes that mirror, `ghcr.io/mutex-solutions/charts-mirror`, exists since 2026-11-16
(`docs/ci-release.md`, "Chart mirror"), and since its first publication that day the BOM
takes all nine third-party charts from it at an exact version and manifest digest, Kamaji
included. ArgoCD still pulls by version, so a version replaced in the mirror is caught by
CI, not at pull time.

### A7. Seed backup by a PVE backup job

**Change.** The seed is backed up as a whole VM by a Proxmox VE backup job to the site's
backup storage. The console prints the one root command that creates the job and shows
attention until a backup job covers the seed. Restoring the seed means restoring that VM
backup. This replaces "the seed's etcd is snapshotted to the NAS on a schedule" and
"restored on a fresh VM from the node template".

**Why.** k3s sends etcd snapshots off the VM only to S3-compatible storage, which would make
an object store a prerequisite of managed services. A VM backup keeps the k3s token, the
server CA, the datastore and the Cluster API state together, so a restored seed answers
with the certificate the portal pins. Creating a backup job needs `Sys.Modify` on `/`
together with backup rights on the seed: the CAPI and CSI tokens hold no `Sys.Modify`, and
the portal token holds nothing on the seed's pool, so the operator runs the command as
root. etcd snapshots to an object store can follow if a site needs them.

### A8. Risks accepted for the first milestone

**Change.** Managed Kubernetes keeps `ProxCloudKubeMint` (`Permissions.Modify`) for the
portal token on `/pool`, `/storage`, `/sdn/zones` and `/nodes`, and `ProxCloudKubeToken`
(`User.Modify`) on `/access/groups/proxcloud-kube`, a group that holds `capmox@pve` and
`csi@pve` and no one else. *(Corrected on 2026-11-16: this said `ProxCloudKubeMint`
(`User.Modify Permissions.Modify`) on `/access/groups`, `/pool`, `/storage`, `/sdn/zones`
and `/nodes`; see "Correction" below.)*
The portal token itself has NoAccess on both platform pools, but a stolen portal token can
grant a role on `/pool` to a principal it can mint a token for, and that grant reaches the
platform pools for every principal without a row of its own there. The first milestone
narrows this: the token rows of `capmox@pve!capi` are narrowed from `/pool` to the two
platform pools, those of `csi@pve!csi` from `/pool` and `/storage` to
`/pool/proxcloud-system` and `/storage/<boot>`, and `capmox@pve` and `csi@pve` get explicit
user rows on the platform pools (in PVE the more specific path wins). The rest is accepted
for the first milestone. The lab measures which principals a stolen token could still
raise, and whether PVE lets it mint a token for `root@pam`, and
`docs/proxmox-permissions.md` records the result.

The `csi@pve!csi` row on `/storage/<boot>` keeps `Datastore.Allocate` on the store that
holds the seed's disk, so the CSI token can reach that disk
([A2](#a2-seed-vm-from-a-native-template-in-its-own-pool), "Limit"). That is accepted for
the first milestone too.

**Correction (2026-11-16).** The risk accepted above was described as a role on `/pool`
for "a principal it can mint a token for". The grant reached further than that sentence
says. PVE takes `User.Modify` on `/access/groups` itself for every user of the cluster,
whatever its groups, and only on `/access/groups/<group>` for the members of one group
(`PVE::RPCEnvironment`, the `userid-group` check). The lab drill of 2026-11-16
(`pve-manager/9.2.26`) confirmed it: with the grant as written here the portal token
created an API token for `root@pam` (`POST /access/users/root@pam/token/…`, HTTP 200),
privilege separated and without an ACL row, and the same for `capmox@pve` and
`proxcloud@pve`. A token without privilege separation, which for `root@pam` is full root,
was not tried; by the PVE source the same grant allowed it before `libpve-access-control`
9.2.1 (2026-11-13) and is refused from that version on. Minting for `root@pam` was neither
described nor accepted here, so the owner decided on 2026-11-16 to close it before the
release:

- `User.Modify` moves to its own role on `/access/groups/proxcloud-kube`. The portal token
  creates and deletes tokens for `capmox@pve` and `csi@pve` only; for `root@pam`, its own
  user and every other user PVE answers 403, and it cannot add a user to the group.
- `ProxCloudKubeMint` loses `User.Modify`, and its row under `/access` is gone, so the
  token holds no `Permissions.Modify` there.
- The setup script and installer stage 10 remove exactly the rows the earlier versions
  wrote on `/access/groups`; the preflight (`mint-scope`), the connection test and stage 10
  report a site that still holds them.

What stays accepted for the first milestone, unchanged: `Permissions.Modify` on `/pool`,
`/storage`, `/sdn/zones` and `/nodes`, with which a stolen portal token writes any role
there for itself or for a token it minted under `capmox@pve` or `csi@pve` (such a
`capmox@pve` token reaches `ProxCloudCAPI` on both platform pools, the user's own rows
being its ceiling), and the power to delete or expire the tokens of those two users and to
disable them. The grant on the group has not run on a lab yet;
`docs/proxmox-permissions.md` has the drill results and what is still to measure.

**Why.** The structural fix, scoping the mint grant to a parent pool of the tenant pools
(nested pools in PVE), renames every tenant pool and is a change of its own. Sites whose
tenant workers run in the shared mode (`kamaji.ClusterDefaults.Shared`) need no mint grant
at all. `docs/proxmox-permissions.md` already says that a stolen portal token can grant
itself anything on those paths; this amendment makes the acceptance explicit and bounds it
to the first milestone. The workload's proxmox-csi needs `Datastore.Allocate` on the store
of the `std` StorageClass to attach its volumes, and the first milestone has one boot store
for the platform (design §8.5). A store of the seed's own is a change of its own.

### A9. Existing installer sites stay observe-only

**Change.** Sites the installer built before this lifecycle existed are recorded in the
portal with their versions and shown in the console, but they are not upgraded through this
lifecycle, neither by the button nor by `make upgrade`. Their Helm-built services clusters
are not migrated in place to Applications; they join fully when rebuilt. Sites the
installer builds after it provisions through the portal's API are full members from the
start. This narrows "a site built by the installer shows the same status and upgrade
screen".

**Why.** On those sites the components were applied with Helm, not as Applications of a
pinned BOM. Moving them in place needs its own migration and lab test; recording them gives
status now without that risk.

### A10. Upgrade order is the BOM's wave order

**Change.** Step 3 of the upgrade runs the components in the BOM's wave order, cluster by
cluster: on the seed its ArgoCD, cert-manager, the Cluster API providers (one component,
the clusterctl Job of [A3](#a3-cluster-api-providers-by-a-clusterctl-job)), then the
workload cluster's Kubernetes version (rolling); then the workload cluster's components in
their waves. Install uses the same order. This replaces "cert-manager, Cluster API core,
CAPMOX, ArgoCD, the workload cluster's Kubernetes version (rolling), then the operators":
the seed's ArgoCD moves first.

**Why.** The seed's ArgoCD applies cert-manager and the Cluster API Job
([A4](#a4-seed-argocd-and-cert-manager)), so install needs it first, and the upgrade runs
the same wave executor as install (design §1 and §7). Upgrading it first also means every
later component is synced by the ArgoCD version its BOM was tested with. The BOM's wave
numbers are then the only place the order is written. The upgrade run itself comes with
the first release that changes the BOM.

### Scope of the first milestone

These owner decisions set the scope. Apart from the sandbox, they do not change a decision
above.

- The in-platform path comes first, behind a flag that stays off until the path has run on
  a lab. The installer provisions through the portal's API in the second milestone.
- Layout: shared only, one workload cluster with the groups `system`, `kamaji` and `dbaas`.
  The separate layout comes later; the data model holds a list of clusters from the start.
- The sandbox's shape is decided in the third milestone, so the `sandbox` group of the
  shared layout above is open until then (see the open points).
- On a Docker Compose portal there are no tenant block volumes (no CSI gateway listener)
  and no database backups. The console lists both in a note on the ready installation's
  card. *(Corrected on 2026-11-16: this said "shows both as warnings"; the page shows an
  informational note, not a warning.)*

### Consequences of the amendments

- Every site runs Cluster API twice, on the seed and on each workload cluster, both from the
  same clusterctl image.
- The operator creates the seed's backup job; the console warns until it exists.
- A chart changed upstream behind the same version is caught by CI, not by ArgoCD, until
  the mirror exists.
- The mint-grant escalation stays open in the first milestone, documented and measured.
  *(Corrected on 2026-11-16: on `/pool`, `/storage`, `/sdn/zones` and `/nodes`. Minting a
  token for a user other than `capmox@pve` and `csi@pve` is closed, see
  [A8](#a8-risks-accepted-for-the-first-milestone).)*
- The CSI token can reach the seed's disk through the shared boot store in the first
  milestone, documented.

## Open points for the implementation

Resolved on 2026-11-15, except the sandbox's shape, which stays open until the third
milestone. Each original question is kept, followed by its answer and where the detail
is. "Design" is the P3 design of the in-platform path ("Enable managed services"); the
files it names land with its implementation.

- The exact schema of `platform-bom.yaml` and of `PlatformInstallation`.

  **Answer.** The BOM is `deploy/platform/platform-bom.yaml`, `schemaVersion: 1`: the
  release; the Kubernetes version and node image (package, tag, digest); the seed image and
  k3s (version, binary and install script by sha256); the clusterctl image; the Cluster API
  providers of the seed and of the workload; the tenant charts the providers pin; and one
  entry per component (name, cluster, wave, install kind, namespace, chart with version and
  digest, node group, stateful, `upgradeFrom`). The API image embeds it,
  `internal/platformlife` parses and validates it, and the installer reads the same file.
  `PlatformInstallation` is a portal document (collection `platformInstallation`) with a
  spec (layout, PVE API address, storage, network, seed, clusters and node groups, node
  image) and a status (phase, installed BOM, seed, clusters, components, volumes,
  providers); its credentials sit in a separate collection that no route returns. Design
  §3.1 to §3.3.
- Whether the shared upgrade plan is a Go package the installer calls through the API, or
  a data file both sides interpret.

  **Answer.** A Go package, `internal/platformlife`, called through the admin API by the
  console and by the installer, as stage 60 already calls the API. The BOM is the only data
  both sides read, and a test keeps the installer's `versions.yml` equal to it. The plan
  needs live state (Application health, Cluster API conditions, the clusterctl inventory)
  that the portal reads, a second interpreter in Ansible would drift, and the portal is
  always upgraded first, so its interpreter is the newest. Design §0 and §7.
- How the seed VM is bootstrapped without SSH (cloud-init from the published node
  template) and how its first credentials reach the portal.

  **Answer.** Image and pool: [A2](#a2-seed-vm-from-a-native-template-in-its-own-pool). The
  clone gets a NoCloud CD, `pcplat-<id>-g<n>.iso`, with k3s pinned by sha256, a server CA
  the portal generated, and a one-shot bootstrap manifest whose bootstrap token lives 3
  hours and can read one ServiceAccount token Secret. Cloud-init applies it once and then
  disables itself. The portal holds the token and the CA certificate before the VM boots:
  it pins TLS to that CA from the first call, waits for `/readyz`, reads and stores the
  ServiceAccount token, deletes the bootstrap objects, then ejects and deletes the CD.
  Nothing calls back from the seed and no command runs in the guest. Design §4.2 (stages 3
  to 5), §4.3 and §5.
- The sandbox cluster as a node group of a shared cluster: gVisor nodes and their network
  isolation rules need their own review.

  **Answer.** Open: the owner decides the sandbox's shape in the third milestone, and the
  first milestone has no `sandbox` group. The design recommends against gVisor workers
  joining the shared cluster as a node group: they would share the CNI overlay and the API
  server with kamaji-etcd and the databases. It recommends a Kamaji-hosted sandbox cluster
  whose control plane runs in the `kamaji` group, with its gVisor workers in their own
  pool, vnet and firewall, as installer stage 55 builds it today. That shape needs
  per-group address ranges first. Design §8 and its question 1.
