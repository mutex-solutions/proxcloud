# deploy/charts — every Helm chart this repo ships

Five charts live here:

- **`proxcloud`** — the application chart: the ProxCloud API, the customer and operator
  consoles, a CloudNativePG `Cluster` and optionally Keycloak. No RabbitMQ is
  required or deployed by default; legacy broker resources are explicit migration
  retention only. The
  [installer](../../docs/install.md) deploys it through ArgoCD with the operators in place;
  `make deploy` installs it directly, and CI publishes it to
  `oci://ghcr.io/mutex-solutions/charts` at the release tag (see its own README).
- **`database-cluster`** — the Managed Database (DBaaS) chart: ONE chart, several engines
  (`values.engine` discriminator — postgresql / mysql / mariadb / valkey / ferretdb /
  opensearch / kafka) rendering the engine's operator CR plus a LoadBalancer Service (MetalLB address pool) and a
  NetworkPolicy, once per customer database, on the ops-built DBaaS
  cluster. ProxCloud writes an ArgoCD `Application` whose values come from
  `internal/cloud/dbaas/values.go` (`BuildValues` — the values contract's other half; see the
  chart's `Chart.yaml` header for the change discipline). Cluster-side setup lives in
  `deploy/dbaas-cluster/`. NOTE: bare defaults deliberately do not render (`engine` is
  required) — template it with one of `examples/values-<engine>.yaml`, which is exactly what
  CI does.
- **`proxcloud-platform`**: ProxCloud's own objects on the managed-services platform
  ([ADR 0009](../../docs/adr/0009-managed-services-platform-lifecycle.md)); nothing applies it
  yet. One part per component of the platform BOM that ADR 0009 describes:
  `clusterctl.enabled` (components `capi` on the seed and `capi-tenant` on the workload
  cluster) renders `clusterctl init` as an ArgoCD Sync hook Job from the
  `proxcloud-clusterctl` image, pinned by digest; `platform.enabled` (component `platform`)
  renders on the workload cluster the provider service account and RBAC of
  `deploy/mgmt-cluster/` and `deploy/dbaas-cluster/` (the objects both define, once), the
  CiliumNetworkPolicy grant of the database egress fence, both tenant AppProjects, a locked
  `default` AppProject, the MetalLB pool `services` and the tenant chart repository entry
  (no credentials). Bare defaults do not render; `examples/values-<component>.yaml` do.
  `internal/platformlife/chart_test.go` pins each render and keeps the RBAC and AppProjects
  equal to the files of those two directories.
- **`proxmox-kamaji-cluster`**: one tenant Kubernetes cluster on Proxmox VE (a Kamaji hosted
  control plane, CAPMOX worker VMs, Cilium, proxmox-csi and MetalLB), applied once per cluster
  on the services cluster through an ArgoCD `Application` that ProxCloud writes
  (`internal/cloud/kamaji`). The installer also uses it for the sandbox cluster (stage 55).
  Block volumes through the CSI gateway need chart 0.1.7 or later
  ([managed-k8s.md](../../docs/managed-k8s.md)). From 0.1.11, `controlPlane.kind: kubeadm`
  renders the platform's own workload cluster of managed services instead
  ([ADR 0009](../../docs/adr/0009-managed-services-platform-lifecycle.md)): a
  `KubeadmControlPlane` on one VM with kube-vip, as installer stage 50 builds the services
  cluster, and no tenant add-ons, CSI controller or Gateway API carriers. Tenant clusters keep
  the default `kamaji`, whose output is unchanged. 0.1.12 changes kubeadm mode only: the control
  plane's kubelet gets the machine's own address as `node-ip` (it registered the kube-vip
  address), and kube-vip's lease is 60 s with a renew deadline of 40 s, so it tries a renewal
  again for 40 s before it gives up the API address. Each request is still cut at 10 s by
  kube-vip's client: a stall of about half a minute is survived, a write that takes over 10 s at
  every try is not. 0.1.13 reads a node group's `machinePlacement` (`<node>: <replicas>`), which
  the portal writes from each node's free memory, CPU and storage; a group without the key is
  split evenly as before, so an existing cluster renders the same objects until its node groups
  change. A map whose sum is not `machineCount`, or that names a node without an image, fails the
  render. 0.1.14 reads `machineHealthCheck`: with `nodeStartupTimeoutSeconds` above
  0, one `MachineHealthCheck` per node group of two or more machines replaces a worker that
  registered no Node in that time, while at most `maxUnhealthy` machines of the group (a number,
  capped at the group's count minus one) are not healthy. The default is 0: no object, and the
  same render as 0.1.13 ([managed-k8s.md](../../docs/managed-k8s.md#a-worker-that-does-not-join)).
  The same 0.1.14 reads `proxmox.network.vlan` (1 to 4094): the control plane's and every node
  group's machine template gets `networkDevices[].vlan`, which the Cluster API provider writes as
  `tag=<n>` on the NIC. Without the value the render is what 0.1.13 gave, so an existing cluster
  keeps its machine templates. The portal writes it only for a platform network with a VLAN tag
  ([managed-services.md](../../docs/managed-services.md#the-platform-network-bridge-vlan-tag-or-routed)).
  0.1.15 reads two optional keys of a node group, both for the platform cluster's upgrade run and
  both on the `MachineDeployment` only (no machine or bootstrap template changes its name):
  `kubernetesVersions` (`<node>: <version>`) gives the MachineDeployment of that node another
  Kubernetes version than the cluster's, and `deleteFirstOn` (a list of nodes) renders
  `rollout.strategy.rollingUpdate` with `maxSurge: 0` and `maxUnavailable: 1` for those nodes, so
  a machine is deleted before its replacement is cloned where a node has no room for one more.
  Without the keys the render is what 0.1.14 gave. Published and pinned by the platform BOM.
  0.1.16 reads `nodeDeletionTimeoutSeconds` (default 1800) and renders it as
  `deletion.nodeDeletionTimeoutSeconds` of the `KubeadmControlPlane`'s machine template (kubeadm
  mode) and of every `MachineDeployment`'s: how long Cluster API keeps trying to delete the Node of
  a Machine it removes, counted from the start of the Machine's deletion. Cluster API's default of
  10 seconds left the Node of a replaced control plane machine in the cluster for good when the
  control plane has one machine (the delete falls into the gap in which the address moves). Cluster
  API puts the value on the machines in place and no machine or bootstrap template changes its
  name: no machine is replaced for it. A negative value renders nothing, which is the render of
  0.1.15 ([managed-services.md](../../docs/managed-services.md#the-platform-clusters-machines)).
  0.1.17 reads `loadBalancer` (kamaji mode only): with `secretName` it renders the cluster's load
  balancer controller next to its control plane (`templates/lb-controller.yaml`, the portal's
  `proxcloud-api` image, the admin kubeconfig and the gateway Secret) with an egress NetworkPolicy,
  and with `default: true` MetalLB serves only class `proxcloud.io/internal`. NGINX Gateway Fabric's
  Service class is never set. Without the block the render is what 0.1.16 gave, so an existing
  cluster bumped to it changes nothing ([managed-k8s.md](../../docs/managed-k8s.md)). Published from
  its tag; the platform BOM pins it for new tenant clusters (`tenantCharts.kubernetes`), while the platform
  cluster (`pc-platform`) stays on 0.1.16.

- **`proxcloud-sandbox`**: the sandbox runtime of the managed platform, 0.1.0. `mode: shared` runs it on a
  node group `sandbox` of the platform cluster, `mode: separate` on the platform-owned sandbox cluster, where cluster
  chart 0.1.18's `addons.sandbox` installs it. It carries `deploy/sandbox-cluster`'s node-setup, telemetry, guardrail,
  agent-sandbox (a copy of the vendored file) and sandbox-proxy, with placement by mode, the RuntimeClass `gvisor`
  with `scheduling`, a CiliumNodeConfig, the portal's two identities and the admission policies that narrow them in
  shared mode. Bare defaults do not render (`proxy.address`, `proxy.domain`); `examples/values-{shared,separate}.yaml`
  do. `internal/cloud/sandbox/chart_test.go` pins both renders and keeps what it shares with `deploy/sandbox-cluster`
  equal; `test/policy` runs its policies through the API server's CEL. Not pinned by the platform BOM yet.

  0.1.18 of `proxmox-kamaji-cluster` reads three optional keys of a node group: `pool` (its Proxmox pool instead of
  `proxmox.pool`), `addresses` (its own ranges: an `InClusterIPPool` `<release>-<group>` with the network's prefix and
  gateway, and `net0` on it with `defaultIPv4: false`) and `dnsServers` (on `net0`), each part of the group's machine
  template; and `addons.sandbox` (the sandbox chart in mode separate, through CAAPH). Without them the render is what
  0.1.17 gave.

## Publishing

`.github/workflows/helm.yml` gives each chart its own tag-driven release, because their
lifecycles are independent (a customer database pins a chart version; the app releases on its
own cadence). All five go to `oci://ghcr.io/mutex-solutions/charts`.

| chart | release tag | version source |
|---|---|---|
| `proxcloud` (app) | `vX.Y.Z` (shared with the image tags) | the tag |
| `database-cluster` | `database-cluster-vX.Y.Z` | `Chart.yaml` — the tag must match or the job fails |
| `proxmox-kamaji-cluster` | `proxmox-kamaji-cluster-vX.Y.Z` | `Chart.yaml` — the tag must match or the job fails |
| `proxcloud-platform` | `proxcloud-platform-vX.Y.Z` | `Chart.yaml` — the tag must match or the job fails |
| `proxcloud-sandbox` | `proxcloud-sandbox-vX.Y.Z` | `Chart.yaml` — the tag must match or the job fails |

The app chart/BOM selects the stable release `v0.10.2` (after the first stable
`v0.4.0`); its tag publishes the app chart and images. Local
rendering and a source pin are not proof that pull URLs exist. Use the
source-build path until the required release images and chart are verified
pullable; see [release readiness](../../docs/release-readiness.md).

For `database-cluster`, `proxmox-kamaji-cluster` and `proxcloud-platform`, bump `version:` in `Chart.yaml`
in the PR that changes the chart, merge, then have a maintainer deliberately cut
the matching tag. The current source versions are database `0.5.2`, Kubernetes `0.1.18` (not published yet), platform
`0.1.3` and sandbox `0.1.0` (not published yet). All three are published and pinned by the platform
BOM (database `0.5.2`: no
disruption budget for a PostgreSQL of one instance; Kubernetes `0.1.17` for new tenant clusters: the load
balancer controller, and `0.1.16` for the platform cluster: the node deletion
timeout of the control plane's and the node groups' machines, on top of the version and the
delete-first rollout per node of a node group of `0.1.15`, on top of the node groups' MachineHealthCheck, the
VLAN tag and the control plane count of the platform cluster of `0.1.14`; platform `0.1.3`: `delete`
on Machines and read access to MachineHealthChecks for the provider's service account);
they are independent of the app's `0.10.2`. Publishing is idempotent — an
already-published version is skipped, so a reused version does not update an old
artifact. These are release operations, not commands to run during normal setup.
See [CI/release operations](../../docs/ci-release.md) for full verification.

ProxCloud pins the version per provider (`config.argocd.chartVersion` on the database and the
Kubernetes provider) and never resolves `latest`: an existing database or cluster keeps its pin
until something explicitly moves it.

## Working on it

```sh
helm lint deploy/charts/database-cluster -f deploy/charts/database-cluster/examples/values-postgresql.yaml
helm template test deploy/charts/database-cluster -n pc-demo \
  -f deploy/charts/database-cluster/examples/values-postgresql.yaml
```

`TestBuildValues` in `internal/cloud/dbaas/dbaas_test.go` pins every key the chart reads, and
is the guard against the two drifting apart.
