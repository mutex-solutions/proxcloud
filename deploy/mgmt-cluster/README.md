# Management-cluster manifests for the ProxCloud `kamaji` provider

Operator-applied, one-time setup on the **Kamaji management cluster** (the cluster running
the Kamaji operator + cluster-api-operator — infra-ops `kamaji-cluster-az1`, or a dev
equivalent). ProxCloud does not apply these itself: they create the identity and guardrails
proxcloud then operates *within*.

Full operator runbook: [`docs/managed-k8s.md`](../../docs/managed-k8s.md).

## Prerequisites

1. **Kamaji stack live** — Kamaji operator, cluster-api-operator with the KamajiControlPlane
   provider, CAPMOX and the in-cluster IPAM provider, CAAPH. (Already true on `kamaji-cluster-az1`; a dev mgmt cluster needs the
   same four charts — plan §3.0.)
2. **ArgoCD installed** on this cluster (plan D3). Any standard install; the manifests here
   assume the `argocd` namespace.
3. **Chart CI publishing the kamaji cluster chart** (`config.argocd.chartName`, and its mirrored images) to
   `ghcr.io/mutex-solutions/charts` at **pinned versions** — never `latest`/`0.0.0+latest`.
   The chart source lives in this repo at `deploy/charts/` and is published by
   `.github/workflows/helm.yml` on a release tag; see `deploy/charts/README.md`. The
   version proxcloud deploys is pinned per provider (`config.argocd.chartVersion`) and per
   existing cluster; the registry must keep every version a live cluster is pinned to.

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
#    By label, not by name: the statefulset is named after the Helm release, so on
#    kamaji-cluster-az1 it is `kamaji-argocd-application-controller`, not `argocd-...`.
kubectl -n argocd rollout restart statefulset \
  -l app.kubernetes.io/name=argocd-application-controller
```

Then assemble the proxcloud kubeconfig from the SA token (recipe in the comment block at the
bottom of `rbac.yaml`) and paste it as the provider secret when registering the `kamaji`
external service (admin UI → Cloud providers → Add provider, or
`deploy/seed/external-service-dev.json`).

## What each file is

| File | Contents |
|---|---|
| `rbac.yaml` | `proxcloud-system` namespace, `proxcloud` ServiceAccount, ClusterRole/binding limited to exactly the verbs `internal/cloud/kamajik8s/client.go` uses, a namespaced Role for Application CRUD in `argocd`, the SA token Secret, and the kubeconfig recipe. Read its header for why the secrets grant is cluster-wide (RBAC cannot wildcard `pc-*` namespaces). |
| `appproject.yaml` | AppProject `proxcloud-k8s`: sourceRepos = our OCI registry only, destinations = in-cluster `pc-*` only, empty (conservative) clusterResourceWhitelist. |
| `repo-credential.yaml` | ArgoCD repository Secret for the (private) chart registry, with `enableOCI: true`. Its `url` must match `config.argocd.chartRepo` and the AppProject sourceRepos exactly — ArgoCD matches credentials by URL prefix. |
| `argocd-health.yaml` | argocd-cm patch: Lua health for TenantControlPlane / Cluster / MachineDeployment. **Status fields must be validated during the live drill** (plan §3.0) — see the file header. |

## Verify

```sh
# RBAC is exactly scoped (yes / yes / no):
kubectl auth can-i list tenantcontrolplanes.kamaji.clastix.io -A --as=system:serviceaccount:proxcloud-system:proxcloud
kubectl auth can-i create applications.argoproj.io -n argocd --as=system:serviceaccount:proxcloud-system:proxcloud
kubectl auth can-i '*' '*' -A --as=system:serviceaccount:proxcloud-system:proxcloud

# AppProject present:
kubectl -n argocd get appproject proxcloud-k8s

# Health customizations loaded:
kubectl -n argocd get cm argocd-cm -o jsonpath='{.data}' | grep -o 'resource\.customizations\.health\.[^"]*'
```
