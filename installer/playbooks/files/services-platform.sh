#!/usr/bin/env bash
# Stage 50, second half: everything the services cluster runs. Idempotent, every step is an upgrade
# --install or an apply. Reads its settings from the environment (50-services-cluster.yml sets them).
set -euo pipefail

: "${KUBECONFIG:?}" "${METALLB_RANGE:?}" "${PVE_API_URL:?}" "${PVE_NODE_IPS:?}" "${PVE_PROXY_IP:?}"
: "${CAPI_TOKEN_ID:?}" "${CAPI_TOKEN_SECRET:?}" "${CSI_TOKEN_ID:?}" "${CSI_TOKEN_SECRET:?}" "${REGION:?}" "${STORAGE:?}"
: "${CILIUM_VERSION:?}" "${CERT_MANAGER_VERSION:?}" "${METALLB_VERSION:?}" "${CSI_VERSION:?}" "${ARGOCD_VERSION:?}" "${CNPG_VERSION:?}"
: "${CAPI_VERSION:?}" "${CAPMOX_VERSION:?}" "${IPAM_VERSION:?}" "${KAMAJI_CP_VERSION:?}" "${CAAPH_VERSION:?}"
: "${KAMAJI_IMAGE_TAG:?}" "${KAMAJI_ETCD_VERSION:?}" "${BARMAN_PLUGIN_VERSION:?}"
HERE="$(cd "$(dirname "$0")" && pwd)"

log() { echo "== $*"; }
up() { helm upgrade --install "$@" --wait --timeout 10m; }

log cni
up cilium cilium --repo https://helm.cilium.io --version "$CILIUM_VERSION" -n kube-system \
  --set operator.replicas=1 --set hubble.enabled=false --set ipam.mode=kubernetes

log cert-manager
up cert-manager cert-manager --repo https://charts.jetstack.io --version "$CERT_MANAGER_VERSION" \
  -n cert-manager --create-namespace --set crds.enabled=true

log metallb
up metallb metallb --repo https://metallb.github.io/metallb --version "$METALLB_VERSION" \
  -n metallb-system --create-namespace --set speaker.frr.enabled=false
kubectl label ns metallb-system pod-security.kubernetes.io/enforce=privileged --overwrite
kubectl apply -f - <<EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata: {name: services, namespace: metallb-system}
spec:
  addresses: ["$METALLB_RANGE"]
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata: {name: services, namespace: metallb-system}
spec:
  ipAddressPools: [services]
EOF

log proxmox-csi
kubectl create ns csi-proxmox --dry-run=client -o yaml | kubectl apply -f -
kubectl label ns csi-proxmox pod-security.kubernetes.io/enforce=privileged --overwrite
cat > /tmp/pc-csi.yaml <<EOF
features: {provider: capmox}
clusters:
  - url: ${PVE_API_URL%/}/api2/json
    insecure: true
    token_id: "$CSI_TOKEN_ID"
    token_secret: "$CSI_TOKEN_SECRET"
    region: "$REGION"
EOF
kubectl -n csi-proxmox create secret generic proxmox-csi-plugin --from-file=config.yaml=/tmp/pc-csi.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
rm -f /tmp/pc-csi.yaml
up proxmox-csi oci://ghcr.io/sergelogvinov/charts/proxmox-csi-plugin --version "$CSI_VERSION" -n csi-proxmox \
  --set existingConfigSecret=proxmox-csi-plugin \
  --set "storageClass[0].name=std" --set "storageClass[0].storage=$STORAGE" --set "storageClass[0].fstype=ext4" \
  --set "storageClass[0].reclaimPolicy=Delete" \
  --set-string 'storageClass[0].annotations.storageclass\.kubernetes\.io/is-default-class=true' \
  --set 'node.tolerations[0].operator=Exists'

# Tenant workers have no route to the management network, only to this subnet. Their proxmox-csi talks
# to the PVE API through this address, which forwards to the nodes.
log pve api forwarder
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata: {name: proxcloud-system}
---
apiVersion: v1
kind: Service
metadata: {name: pve-api, namespace: proxcloud-system}
spec:
  type: LoadBalancer
  loadBalancerIP: $PVE_PROXY_IP
  ports: [{name: api, port: 8006, targetPort: 8006}]
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: pve-api
  namespace: proxcloud-system
  labels: {kubernetes.io/service-name: pve-api}
addressType: IPv4
ports: [{name: api, port: 8006, protocol: TCP}]
endpoints:
$(for ip in $PVE_NODE_IPS; do echo "  - addresses: [$ip]"; done)
EOF

log cluster api
export PROXMOX_URL="$PVE_API_URL" PROXMOX_TOKEN="$CAPI_TOKEN_ID" PROXMOX_SECRET="$CAPI_TOKEN_SECRET"
export EXP_CLUSTER_RESOURCE_SET=true
if ! kubectl get deploy -n capi-system capi-controller-manager >/dev/null 2>&1; then
  clusterctl init --core "cluster-api:$CAPI_VERSION" --bootstrap "kubeadm:$CAPI_VERSION" \
    --control-plane "kamaji:$KAMAJI_CP_VERSION" --infrastructure "proxmox:$CAPMOX_VERSION" \
    --ipam "in-cluster:$IPAM_VERSION" --addon "helm:$CAAPH_VERSION" --wait-providers
fi

log kamaji
up kamaji-etcd kamaji-etcd --repo https://clastix.github.io/charts --version "$KAMAJI_ETCD_VERSION" \
  -n kamaji-system --create-namespace --set replicas=1 --set datastore.enabled=true --set datastore.name=default
up kamaji-crds kamaji-crds --repo https://clastix.github.io/charts --version 0.0.0+latest -n kamaji-system
up kamaji kamaji --repo https://clastix.github.io/charts --version 0.0.0+latest -n kamaji-system \
  --set image.tag="$KAMAJI_IMAGE_TAG" --set kamaji-etcd.deploy=false --set defaultDatastoreName=default

log argocd
up argocd oci://ghcr.io/argoproj/argo-helm/argo-cd --version "$ARGOCD_VERSION" -n argocd --create-namespace \
  --set dex.enabled=false --set notifications.enabled=false --set applicationSet.enabled=false \
  --set 'configs.params.server\.insecure=true'

log database operators
up cnpg cloudnative-pg --repo https://cloudnative-pg.github.io/charts --version "$CNPG_VERSION" -n cnpg-system --create-namespace
kubectl apply --server-side -f "https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/$BARMAN_PLUGIN_VERSION/manifest.yaml"

log registry
kubectl apply -f "$HERE/services-registry.yml"
kubectl -n proxcloud-registry rollout status deploy/pc-registry --timeout=180s

log guardrails
kubectl apply -f "$HERE/manifests/"
