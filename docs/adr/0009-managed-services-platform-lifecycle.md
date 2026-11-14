# 9. Managed-services platform: two provisioning paths, one lifecycle

## Status

Proposed (owner decisions of 2026-11-14 recorded below; not implemented yet).

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

### Seed cluster

A single k3s VM is the long-lived management cluster ("seed"). It runs Cluster API, the
Proxmox provider (CAPMOX) and ArgoCD, and no tenant workload. It creates and manages one
or more workload clusters. Each workload cluster's autoscaler asks the seed to scale its
node groups.

The seed's etcd is snapshotted to the NAS on a schedule. If the seed is lost, the workload
clusters keep running but cannot scale, repair nodes or upgrade until the seed is restored
on a fresh VM from the node template.

### Cluster layout is the operator's choice

The operator chooses one of:

- **Shared:** one workload cluster with a node group per service (`system`, `kamaji`,
  `dbaas`, `sandbox`). Each group is its own MachineDeployment with labels and taints, and
  each service is pinned to its group.
- **Separate:** one workload cluster per service.

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

### Delivery: Helm through ArgoCD

Every component is an ArgoCD `Application` whose source is the pinned OCI Helm chart. The
values come from Secrets and ConfigMaps that the provisioning path writes, so no Git
repository is needed.

- Stateless components sync automatically and self-heal.
- Stateful components (database clusters, Kamaji datastores, CSI) are never pruned
  automatically and sync in explicit waves.

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

## Open points for the implementation

- The exact schema of `platform-bom.yaml` and of `PlatformInstallation`.
- Whether the shared upgrade plan is a Go package the installer calls through the API, or
  a data file both sides interpret.
- How the seed VM is bootstrapped without SSH (cloud-init from the published node
  template) and how its first credentials reach the portal.
- The sandbox cluster as a node group of a shared cluster: gVisor nodes and their network
  isolation rules need their own review.
