# Vendored Gateway API Standard resources

Unmodified schema sources from Kubernetes SIG Network Gateway API **v1.6.3**,
immutable commit `c0836133e24073fcdcdbb1713aba3cc73a89d0b5`.

- Origin: <https://github.com/kubernetes-sigs/gateway-api/tree/v1.6.3/config/crd/standard>
- Eleven source files: ten CRDs and one two-document safe-upgrade admission policy/binding.
- Per-file immutable source URLs, decoded bytes and SHA-256 are in `provenance.json`.
- Upstream Apache-2.0 terms are retained in `LICENSE` and original file notices.
- The tenant chart renders one resource-set Secret per YAML file, selected by one
  tenant-scoped ClusterResourceSet when `addons.nginxGatewayFabric.enabled=true`.
- These are data files, never executed by the build or downloaded during a cluster create.
- Do not use `kubectl apply` client-side last-applied annotations on the large CRDs.
  The pinned CAPI Reconcile resource-set implementation uses GET/CREATE or a
  resourceVersion-guarded merge patch, not kubectl client-side apply.
- Update Standard and NGINX extension CRDs only through a separately reviewed
  compatibility/lifecycle change; removing a controller must not delete CRDs and routes.
