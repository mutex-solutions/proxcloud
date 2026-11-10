# Managed Kubernetes: NGINX Gateway Fabric and Gateway API

New managed Kubernetes clusters may opt into **NGINX Gateway Fabric**, not the
retired ingress-nginx controller. The software/chart is named
`nginx-gateway-fabric` (not `nginx-fabric-gateway`). This change does not migrate
existing Ingress objects, DNS, certificates, addresses or traffic.

## Source and version contract

| Component | Fixed source pin | Scope |
| --- | --- | --- |
| ProxCloud tenant cluster chart | `0.1.10` | New Gateway Fabric values/CRD carriers; existing Applications keep their own pin |
| NGINX Gateway Fabric | chart/software `2.8.1` | OCI `oci://ghcr.io/nginx/charts/nginx-gateway-fabric` |
| Gateway API | **Standard `v1.6.3`** | Ten CRDs plus safe-upgrade admission policy/binding in each opted-in tenant |
| Kubernetes floor | `1.32` in the supported `1.x` family | The controller chart's declared floor, not proof of every addon combination |
| CAAPH | repository BOM `v0.6.4` | Management-side HelmChartProxy delivery; actual deployment must be checked separately |

These pins were verified against official upstream metadata on 2026-11-10 UTC.
Latest is a dated observation, not a floating value. The actual NGF OCI chart and
images remain version-tag pinned rather than digest-certified by this change.
No public ProxCloud `0.1.10` publication or live tenant installation is implied.

Official evidence: [NGF release](https://github.com/nginx/nginx-gateway-fabric/releases/tag/v2.8.1),
[chart contract](https://github.com/nginx/nginx-gateway-fabric/tree/v2.8.1/charts/nginx-gateway-fabric),
[installation prerequisites](https://docs.nginx.com/nginx-gateway-fabric/install/helm/),
[Standard resources](https://github.com/kubernetes-sigs/gateway-api/tree/v1.6.3/config/crd/standard).

## C1 — Actors and ownership

- The platform operator owns the offered controller/chart pins and tenant CRD lifecycle.
- A project cloud manager chooses the curated `nginxGatewayFabric` boolean addon.
- The cluster administrator owns application `Gateway`, `HTTPRoute`/other supported
  routes, listener TLS Secrets and DNS. Installing the controller is **not** an
  application exposure request.
- Legacy ingress owners retain their existing release until a separately reviewed
  traffic migration/decommission. Ordinary metrics or certificate edits cannot
  uninstall it.

## C2 — Where resources are applied

```text
Client create or add-on edit
  -> authenticated project cloud-management / owned-cluster gate
  -> actual provider/cluster chart pin + Kubernetes version checks
  -> existing management-cluster ArgoCD Application
      -> tenant-scoped ClusterResourceSet + 11 pinned schema carrier Secrets
          -> Gateway API Standard resources in that TENANT cluster
      -> CAAPH HelmChartProxy (OCI NGF 2.8.1)
          -> NGF extension CRDs + controller + nginx GatewayClass in that TENANT
              -> user-created Gateway/Routes/TLS -> tenant LoadBalancer service
```

No management-cluster Gateway API CRD, GatewayClass, application Gateway or route is
rendered directly. No new ProxCloud management RBAC grant is needed: its existing
Application CRUD remains the write surface; CAPI/CAAPH use their established tenant
credentials. No PVE credentials, secret value or kubeconfig enters addon values.

## C3 — API, chart and compatibility rules

The modern menu is `metricsServer`, `certManager`, `nginxGatewayFabric`. Gateway
Fabric defaults **off**, matching the former optional controller policy. New create
accepts an older client's harmless `ingress:false` but rejects `ingress:true`.
A selected NGF addon requires provider chart `0.1.10+` before any infrastructure
reservation, and is revalidated by the service before management writes.

Existing clusters use their **actual Application pin**, not a newer provider pin.
Selecting NGF on chart `0.1.9` is refused rather than silently ignored. An explicit
platform update changes only the pin/ordinary existing values; it does not translate
Ingress objects or enable NGF. A chart change cannot move an enabled NGF cluster
below `0.1.10`.

`SET_ADDONS` merges only curated booleans into current known choices. Omitted
newer choices survive requests from an older client. Unknown request keys and
malformed stored known blocks are rejected; old operator-only/credential-push
entries are not round-tripped. A legacy raw `addons.ingress` block is preserved
verbatim. A request that changes that legacy boolean is refused with an explicit
migration/decommission requirement. Legacy ingress and NGF cannot both be enabled
through this path.

The retained legacy HelmChartProxy keeps its original name, release, namespace,
repository and `4.13.3` pin. This compatibility retention is **not** a claim that
retired ingress-nginx is safe or supported; it avoids destroying current traffic
as a side effect of an unrelated edit. Do not create new legacy installations.

## C4 — CRD delivery and readiness

The upstream NGF chart contains its own `gateway.nginx.org` extension CRDs, **not**
Gateway API Standard CRDs. The exact Standard resources are vendored under
`deploy/charts/proxmox-kamaji-cluster/files/gateway-api/v1.6.3`, from immutable
upstream commit `c0836133e24073fcdcdbb1713aba3cc73a89d0b5`, with per-file URL/size/SHA-256
provenance and Apache-2.0 license.

The aggregate Standard bundle exceeds one MiB. Each of its eleven source files is
carried in a separate resource-set Secret below Kubernetes' decoded Secret size
limit; the safe-upgrade file preserves both admission-policy documents. No remote
shell Job or runtime schema URL is introduced. The Gateway API carriers and CRS
have `Prune=false`, so disabling the controller cannot remove shared CRDs/routes.
CAPI resource-set removal does not itself delete applied tenant schemas either.
Schema retirement is a separate explicit operation, never addon cleanup.

**There is no strict cross-controller install order.** CRS and CAAPH reconcile
independently. If NGF's first Helm install runs before Standard CRDs are established,
Helm discovery fails and CAAPH retries. The reviewed CAAPH path installs again when
no release was recorded, or upgrades a failed release; eventual convergence is
not a readiness fence. NGF's Helm proxy waits for its release, but readiness still
requires observing CRS applied resources, successful Helm release/controller and
`GatewayClass/nginx` Accepted. No create response alone proves those conditions.

Helm installs missing NGF extension CRDs initially; it **does not upgrade them on
controller upgrades**. Before changing NGF/Gateway API pins, reconcile both CRD
families deliberately, review stored versions/admission policy and verify existing
route/controller compatibility. Do not delete a CRD to force an upgrade.

## Application example: users own listener, routes and TLS

After controller readiness, a cluster administrator may create these objects in
one application namespace. This example is documentation, not automatically applied:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: web
  namespace: application
spec:
  gatewayClassName: nginx
  listeners:
    - name: https
      hostname: app.example.invalid
      port: 443
      protocol: HTTPS
      tls:
        mode: Terminate
        certificateRefs:
          - kind: Secret
            name: web-tls
      allowedRoutes:
        namespaces:
          from: Same
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: web
  namespace: application
spec:
  parentRefs:
    - name: web
      sectionName: https
  hostnames: [app.example.invalid]
  rules:
    - backendRefs:
        - name: web
          port: 8080
```

The namespace, Service endpoints and `kubernetes.io/tls` Secret must already exist.
Do not place certificate private keys in Application/chart values. The controller's
internal Service is ClusterIP; a user Gateway creates the NGF data-plane
LoadBalancer (`externalTrafficPolicy: Local`) using the tenant's existing MetalLB
pool. No application Gateway, TLS issuer, DNS record or public listener is created
at addon installation. Same-namespace route attachment is intentional; broader
cross-namespace trust requires explicit Gateway API policy.

## Existing-cluster migration checklist

1. Inventory live IngressClasses/Ingress objects, annotations, rewrites, TCP/UDP
   listeners, certificates, DNS and assigned addresses; confirm owner and rollback.
2. Validate the target NGF/Standard feature support; not every ingress annotation
   has a Gateway API equivalent. Review listener routes and namespace trust.
3. Use a separate operator-approved staged plan with a test address/domain and
   explicit coexistence/traffic ownership outside the normal addon toggle path.
4. Validate TLS/SNI/redirects/headers/WebSocket/gRPC/session behavior, route status,
   healthy backend endpoints and rollback before a DNS/address cutover.
5. Remove legacy ingress only after explicit decommission acceptance. The normal
   create/edit/platform-update flows deliberately do not perform that cutover.

## Test and acceptance boundary

Native unit tests cover modern/legacy selection, chart pin and Kubernetes guards,
sparse-update preservation, unknown/malformed values, unchanged legacy release
identity and generated Standard schema hashes/sizes/tenant-scoped carriers. Helm
rendering is offline. These checks are not a live DNS/TLS/MetalLB/CAAPH convergence
or gateway traffic canary. Such acceptance requires an explicitly owned new tenant
fixture; existing clusters and forbidden network ranges remain untouched.
