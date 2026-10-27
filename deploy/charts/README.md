# deploy/charts — every Helm chart this repo ships

Two charts live here:

- **`proxcloud`** — the application chart: the ProxCloud API, the customer and operator
  consoles, plus optionally bundled PostgreSQL, RabbitMQ and Keycloak. This is what
  `make deploy` installs and what CI publishes to `oci://ghcr.io/mutex-solutions/charts`
  at the release tag (see its own README).
- **`database-cluster`** — the Managed Database (DBaaS) chart: ONE chart, several engines
  (`values.engine` discriminator — postgresql / mysql / mariadb / valkey / ferretdb /
  opensearch / kafka) rendering the engine's operator CR plus an internal LB Service into the
  tenant network and a NetworkPolicy, once per customer database, on the ops-built DBaaS
  cluster. ProxCloud writes an ArgoCD `Application` whose values come from
  `internal/cloud/dbaas/values.go` (`BuildValues` — the values contract's other half; see the
  chart's `Chart.yaml` header for the change discipline). Cluster-side setup lives in
  `deploy/dbaas-cluster/`. NOTE: bare defaults deliberately do not render (`engine` is
  required) — template it with one of `examples/values-<engine>.yaml`, which is exactly what
  CI does.

## Publishing

`.github/workflows/helm.yml` gives each chart its own tag-driven release, because their
lifecycles are independent (a customer database pins a chart version; the app releases on its
own cadence). Both go to `oci://ghcr.io/mutex-solutions/charts`.

| chart | release tag | version source |
|---|---|---|
| `proxcloud` (app) | `vX.Y.Z` (shared with the image tags) | the tag |
| `database-cluster` | `database-cluster-vX.Y.Z` | `Chart.yaml` — the tag must match or the job fails |

Flow for `database-cluster`: bump `version:` in `Chart.yaml` in the PR that changes the chart,
merge, then cut the matching tag (`git tag database-cluster-v0.4.0 && git push origin
database-cluster-v0.4.0`). Publishing is idempotent — an already-published version is skipped.

ProxCloud pins the version per provider (`config.argocd.chartVersion`) and never resolves
`latest` — an existing database keeps its pin until something explicitly moves it.

## Working on it

```sh
helm lint deploy/charts/database-cluster -f deploy/charts/database-cluster/examples/values-postgresql.yaml
helm template test deploy/charts/database-cluster -n pc-demo \
  -f deploy/charts/database-cluster/examples/values-postgresql.yaml
```

`TestBuildValues` in `internal/cloud/dbaas/dbaas_test.go` pins every key the chart reads, and
is the guard against the two drifting apart.
