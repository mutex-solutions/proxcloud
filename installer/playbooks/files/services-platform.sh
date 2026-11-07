#!/usr/bin/env bash
# Stage 50, second half: everything the services cluster runs. Idempotent, every step is an upgrade
# --install or an apply. Reads its settings from the environment (50-services-cluster.yml sets them).
set -euo pipefail

: "${KUBECONFIG:?}" "${METALLB_RANGE:?}" "${PVE_API_URL:?}"
: "${CSI_TOKEN_ID:?}" "${CSI_TOKEN_SECRET:?}" "${REGION:?}" "${STORAGE:?}"
: "${CILIUM_VERSION:?}" "${CERT_MANAGER_VERSION:?}" "${METALLB_VERSION:?}" "${CSI_VERSION:?}" "${ARGOCD_VERSION:?}" "${CNPG_VERSION:?}"
: "${CAPI_VERSION:?}" "${CAPMOX_VERSION:?}" "${IPAM_VERSION:?}" "${KAMAJI_CP_VERSION:?}" "${CAAPH_VERSION:?}"
: "${KAMAJI_IMAGE_TAG:?}" "${KAMAJI_ETCD_VERSION:?}" "${BARMAN_PLUGIN_VERSION:?}"
BUNDLE="$(cd "$(dirname "$0")" && pwd)"
: "${CHART_REPO:?}"

log() { echo "== $*"; }
# A young API server drops connections while the CNI comes up: an install is retried, and is idempotent.
up() {
  for _ in 1 2 3 4; do
    helm upgrade --install "$@" --wait --timeout 10m && return 0
    sleep 30
  done
  return 1
}

log cni
up cilium cilium --repo https://helm.cilium.io --version "$CILIUM_VERSION" -n kube-system \
  --set operator.replicas=1 --set hubble.enabled=false --set ipam.mode=kubernetes

kubectl wait nodes --all --for=condition=Ready --timeout=600s

log cert-manager
up cert-manager cert-manager --repo https://charts.jetstack.io --version "$CERT_MANAGER_VERSION" \
  -n cert-manager --create-namespace --set crds.enabled=true

log metallb
up metallb metallb --repo https://metallb.github.io/metallb --version "$METALLB_VERSION" \
  -n metallb-system --create-namespace --set speaker.frr.enabled=false --set frrk8s.enabled=false
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

log cluster api
# Empty on purpose: a manager-level credential silently wins over every ProxmoxCluster.credentialsRef,
# which is what makes CAPMOX answer "not authorized to access endpoint" with the wrong token.
export PROXMOX_URL="" PROXMOX_TOKEN="" PROXMOX_SECRET=""
export EXP_CLUSTER_RESOURCE_SET=true
if ! kubectl get deploy -n capi-system capi-controller-manager >/dev/null 2>&1; then
  clusterctl init --core "cluster-api:$CAPI_VERSION" --bootstrap "kubeadm:$CAPI_VERSION" \
    --control-plane "kamaji:$KAMAJI_CP_VERSION" --infrastructure "proxmox:$CAPMOX_VERSION" \
    --ipam "in-cluster:$IPAM_VERSION" --addon "helm:$CAAPH_VERSION" --wait-providers
fi

log kamaji
up kamaji-crds kamaji-crds --repo https://clastix.github.io/charts --version 0.0.0+latest -n kamaji-system --create-namespace
up kamaji-etcd kamaji-etcd --repo https://clastix.github.io/charts --version "$KAMAJI_ETCD_VERSION" \
  -n kamaji-system --set replicas=1 --set datastore.enabled=true --set datastore.name=default
up kamaji kamaji --repo https://clastix.github.io/charts --version 0.0.0+latest -n kamaji-system \
  --set image.tag="$KAMAJI_IMAGE_TAG" --set kamaji-etcd.deploy=false --set defaultDatastoreName=default

log argocd
up argocd oci://ghcr.io/argoproj/argo-helm/argo-cd --version "$ARGOCD_VERSION" -n argocd --create-namespace \
  --set dex.enabled=false --set notifications.enabled=false --set applicationSet.enabled=false \
  --set 'configs.params.server\.insecure=true'

log database operators
up cnpg cloudnative-pg --repo https://cloudnative-pg.github.io/charts --version "$CNPG_VERSION" -n cnpg-system --create-namespace
kubectl apply --server-side -f "https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/$BARMAN_PLUGIN_VERSION/manifest.yaml"

log registry and charts
kubectl apply -f "$BUNDLE/registry.yml"
kubectl -n proxcloud-registry rollout status deploy/pc-registry --timeout=180s
kubectl -n proxcloud-registry port-forward svc/pc-registry 15000:5000 >/dev/null 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
sleep 3
for chart in "$BUNDLE"/charts/*; do
  helm package "$chart" -d /tmp/pc-charts >/dev/null
done
for pkg in /tmp/pc-charts/*.tgz; do
  helm push "$pkg" oci://127.0.0.1:15000/charts --plain-http
done
rm -rf /tmp/pc-charts

log guardrails
for f in "$BUNDLE"/manifests/*.yaml; do
  case "$f" in
    *health*) kubectl -n argocd patch configmap argocd-cm --type merge --patch-file "$f" ;;
    *) sed "s#ghcr.io/mutex-solutions/charts#$CHART_REPO#" "$f" | kubectl apply -f - ;;
  esac
done
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: proxcloud-charts-creds
  namespace: argocd
  labels: {argocd.argoproj.io/secret-type: repository}
stringData:
  type: helm
  name: proxcloud-charts
  url: $CHART_REPO
  enableOCI: "true"
  insecureOCIForceHttp: "true"
EOF
kubectl -n argocd rollout restart statefulset -l app.kubernetes.io/name=argocd-application-controller
