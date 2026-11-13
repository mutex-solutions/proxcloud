# deploy/charts — every Helm chart this repo ships

Three charts live here:

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
- **`proxmox-kamaji-cluster`**: one tenant Kubernetes cluster on Proxmox VE (a Kamaji hosted
  control plane, CAPMOX worker VMs, Cilium, proxmox-csi and MetalLB), applied once per cluster
  on the services cluster through an ArgoCD `Application` that ProxCloud writes
  (`internal/cloud/kamaji`). The installer also uses it for the sandbox cluster (stage 55).
  Block volumes through the CSI gateway need chart 0.1.7 or later
  ([managed-k8s.md](../../docs/managed-k8s.md)).

## Publishing

`.github/workflows/helm.yml` gives each chart its own tag-driven release, because their
lifecycles are independent (a customer database pins a chart version; the app releases on its
own cadence). All three go to `oci://ghcr.io/mutex-solutions/charts`.

| chart | release tag | version source |
|---|---|---|
| `proxcloud` (app) | `vX.Y.Z` (shared with the image tags) | the tag |
| `database-cluster` | `database-cluster-vX.Y.Z` | `Chart.yaml` — the tag must match or the job fails |
| `proxmox-kamaji-cluster` | `proxmox-kamaji-cluster-vX.Y.Z` | `Chart.yaml` — the tag must match or the job fails |

The app chart/BOM selects the stable release `v0.5.2` (after the first stable
`v0.4.0`); its tag publishes the app chart and images. Local
rendering and a source pin are not proof that pull URLs exist. Use the
source-build path until the required release images and chart are verified
pullable; see [release readiness](../../docs/release-readiness.md).

For `database-cluster` and `proxmox-kamaji-cluster`, bump `version:` in `Chart.yaml`
in the PR that changes the chart, merge, then have a maintainer deliberately cut
the matching tag. The current source versions are database `0.5.1` and Kubernetes
`0.1.10`; they are independent of the app's `0.5.2`. Publishing is idempotent — an
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
