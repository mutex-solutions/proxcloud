# DBaaS-cluster manifests for the ProxCloud `dbaas` provider

Operator-applied, one-time setup on the **DBaaS cluster** — the ops-pre-built Kubernetes
cluster that runs the full database operator suite and hosts every
customer database. ProxCloud does not apply these itself: they create the identity and
guardrails proxcloud then operates *within* (mirror of `deploy/mgmt-cluster/` for the kamaji
provider).

Full operator runbook: [`docs/managed-dbaas.md`](../../docs/managed-dbaas.md).

## Prerequisites

1. **A pre-built cluster** with the standard plumbing already in place — assumed
   pre-existing, NOT installed by anything here:
   - **MetalLB** — it is what turns the chart's `<id>-lb` Services into reachable endpoints
     (the whole connectivity model). One `IPAddressPool` on the dbaas services subnet (a
     routable subnet tenants reach over their normal egress), named in the provider config
     as `config.database.lbAddressPool`; every tenant-facing LB Service the chart renders
     carries `metallb.universe.tf/address-pool: <that pool>`. Size it with the address math
     below. No tenant network is involved anywhere.
   - **A CSI driver** with a default StorageClass — every database volume is a PVC
     (proxmox-csi on Proxmox).
   - **ArgoCD** on this cluster. Any standard install; the manifests here assume the
     `argocd` namespace.
2. **The database operator suite**, at PINNED versions — record the installed version next
   to the chart version you release, because CRD drift between operator and chart is the
   main compatibility risk (chart README, plan risk list):

   | operator | install namespace (LOAD-BEARING) | version guidance | engine |
   |---|---|---|---|
   | CloudNativePG | `cnpg-system` | **≥ 1.29.1** | postgresql + the ferretdb backend |
   | `plugin-barman-cloud` (+ cert-manager) | `cnpg-system` | **0.7.1** (the version the chart was exercised with); needs CNPG ≥ 1.26 and cert-manager for its operator↔plugin mTLS | postgresql/ferretdb **backups and restore** — provides `barmancloud.cnpg.io` `ObjectStore`; without it the Application renders an unknown kind and stays OutOfSync |
   | Percona Operator for MySQL (ps-operator) | `ps-operator` | current stable; verify served CRD version (`kubectl api-resources \| grep ps.percona.com`) | mysql |
   | mariadb-operator | `mariadb-operator` | **26.x** | mariadb |
   | valkey-operator | `valkey-operator-system` | **only if the valkey beta gate will open** — the official `valkey-io/valkey-operator` v0.4.x (CRD `valkey.io/ValkeyCluster`), Valkey 9.x only | valkey |
   | opensearch-k8s-operator | `opensearch-operator-system` | **3.0.2** — 3.x serves both api groups; the chart renders `opensearch.org/v1` (its webhooks refuse the deprecated `opensearch.opster.io`) | opensearch (+ Dashboards) |
   | Strimzi | `strimzi-system` | **1.1.0**, installed in **watch-all mode** (`watchAnyNamespace: true`) — REQUIRED, databases live in per-project `pc-*` namespaces the operator cannot enumerate up front. 1.x serves ONLY `kafka.strimzi.io/v1`. ⚠ `helm upgrade` does NOT upgrade Strimzi's CRDs — `kubectl apply` the new CRD bundle separately on every operator upgrade | kafka |
   | prometheus-operator CRDs (+ stack) | `monitoring` | optional — required only when databases run with `monitoring.enabled`; **REQUIRED (full kube-prometheus-stack) for the disk-autoscale leg** — it grows volumes off kubelet volume-stats (`kubelet_volume_stats_*`), and the provider then needs `config.metrics {source: prometheus, prometheus: {url: …}}` (same admin Metrics tab as compute providers) | all |
   | Vertical Pod Autoscaler (**autoscaler component only**) | `kube-system` (or upstream default) | install **only if the autoscale feature will be offered**. The **recommender is REQUIRED**; updater and admission-controller are NOT needed — every chart-rendered VPA is `updateMode: Off` (recommendation-only; proxcloud applies the scale-ups itself, a VPA must never evict DB pods) | all (autoscale feature) |

   The **namespace column is not a suggestion**: the chart's per-database NetworkPolicy
   (`templates/networkpolicy.yaml` rule 3) allowlists operator→pod traffic by exactly these
   namespace names — install an operator elsewhere and its probes/management traffic is
   silently denied. FerretDB needs no operator: the chart renders its frontend as a plain
   Deployment over a CNPG backend.
3. **Chart CI publishing `database-cluster`** to `ghcr.io/mutex-solutions/charts` at
   **pinned versions** — never `latest`. The chart source lives in this repo at
   `deploy/charts/database-cluster/` and is published by `.github/workflows/helm.yml` on a
   `database-cluster-v*` tag; see `deploy/charts/README.md`. The version proxcloud deploys is
   pinned per provider (`config.argocd.chartVersion`) and per existing database; the registry
   must keep every version a live database is pinned to.
4. **LB address pool** sized for the databases — see the address math below.
5. **Object store for backups (optional)** — any S3 endpoint (RustFS, Ceph RGW; plain `http`
   works), a bucket and an access/secret key pair, configured on the provider as
   `config.database.backup` + `secret.backupAccessKey` / `secret.backupSecretKey`. No change on
   the DB cluster beyond `plugin-barman-cloud` above; the per-database credential Secret is
   written by ProxCloud.

## Apply order

```sh
# 1. ProxCloud identity + least-privilege RBAC + SA token
kubectl apply -f rbac.yaml

# 2. AppProject guardrail (source/destination allowlist for proxcloud Applications)
kubectl apply -f appproject.yaml

# 3. Registry credential, so ArgoCD can pull the chart. Skip ONLY if you made the package
#    public. Fill in the placeholders first — see the header of the file.
kubectl apply -f repo-credential.yaml

# 4. Custom health checks — a MERGE PATCH onto the existing argocd-cm, NOT kubectl apply
kubectl -n argocd patch configmap argocd-cm --type merge --patch-file argocd-health.yaml

# 5. Restart the ArgoCD controllers so the new Lua health checks load.
#    By label, not by name: the statefulset is named after the Helm release.
kubectl -n argocd rollout restart statefulset \
  -l app.kubernetes.io/name=argocd-application-controller
```

Then assemble the proxcloud kubeconfig from the SA token (recipe in the comment block at the
bottom of `rbac.yaml`) and paste it as the provider secret when registering the `dbaas`
external service (admin UI → Cloud providers → Add provider → Database, or
`deploy/seed/external-service-dev.json`).

## What each file is

| File | Contents |
|---|---|
| `rbac.yaml` | `proxcloud-system` namespace, `proxcloud` ServiceAccount, ClusterRole/binding limited to exactly the verbs the dbaas leg of `internal/cloud/kamajik8s/client.go` uses (namespaces CRUD, secrets CRUD, services read, engine-CRD status reads), a namespaced Role for Application CRUD in `argocd`, the SA token Secret, and the kubeconfig recipe. Read its header for why the secrets grant is cluster-wide (RBAC cannot wildcard `pc-*` namespaces). |
| `appproject.yaml` | AppProject `proxcloud-dbaas`: sourceRepos = our OCI registry only, destinations = in-cluster `pc-*` only, empty (conservative, record-on-demand) clusterResourceWhitelist. |
| `repo-credential.yaml` | ArgoCD repository Secret for the (private) chart registry, with `enableOCI: true`. Its `url` must match `config.argocd.chartRepo` and the AppProject sourceRepos exactly — ArgoCD matches credentials by URL prefix. |
| `argocd-health.yaml` | argocd-cm patch: Lua health for CNPG Cluster / PerconaServerMySQL / MariaDB / Strimzi Kafka / OpenSearchCluster, plus a commented-out valkey placeholder. **Status fields must be validated during the live drill** — see the file header. |

## Verify

```sh
# RBAC is exactly scoped (yes / yes / no):
kubectl auth can-i list clusters.postgresql.cnpg.io -A --as=system:serviceaccount:proxcloud-system:proxcloud
kubectl auth can-i create applications.argoproj.io -n argocd --as=system:serviceaccount:proxcloud-system:proxcloud
kubectl auth can-i '*' '*' -A --as=system:serviceaccount:proxcloud-system:proxcloud

# AppProject present:
kubectl -n argocd get appproject proxcloud-dbaas

# Health customizations loaded:
kubectl -n argocd get cm argocd-cm -o jsonpath='{.data}' | grep -o 'resource\.customizations\.health\.[^"]*'
```

## LoadBalancer address math

Every customer database is **one LoadBalancer Service** (one address from the pool) — EXCEPT
kafka, which is **`instances + 1`**: Strimzi provisions one LB per broker (each broker must be
individually addressable) plus the external bootstrap LB (see the chart's
`templates/kafka/kafka.yaml` header), and a PostgreSQL database with the read-only endpoint
enabled is **2** (`<id>-ro-lb`), as is OpenSearch with Dashboards (`<id>-dash-lb`). Count those
when sizing the pool. An exhausted pool surfaces as the LB Service parked `<pending>` — the
Application stays Progressing and the database never goes READY, with nothing obviously wrong
on the k8s side; check the address pool first.

## Replica spread

With `instances > 1` the replicas of one database must land on different values of the node
label `topology.kubernetes.io/zone` — a **hard** anti-affinity (CNPG `podAntiAffinityType:
required`, a `requiredDuringSchedulingIgnoredDuringExecution` `podAntiAffinity` on the other
engines). Label the nodes with their zone, and keep at least as many zones as the largest
replica count you offer: a surplus replica stays Pending rather than sharing a zone. A site
with one node or one zone sets `config.database.scheduling.zoneKey` (chart value
`scheduling.zoneKey`) to a key the nodes do vary on, for example `kubernetes.io/hostname`.
