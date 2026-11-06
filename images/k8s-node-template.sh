#!/usr/bin/env bash
# Builds the Kubernetes node template with kubernetes-sigs/image-builder and gives every PVE node its own
# copy: CAPMOX clones to local storage only on the template's node.
#
#   PVE_URL=https://192.0.2.11:8006 PVE_TOKEN_ID='images@pve!packer' PVE_TOKEN_SECRET=... \
#   PVE_NODES="pve1 pve2 pve3" PVE_TEMPLATE_STORAGE=nas-templates PVE_BRIDGE=vmbr0 \
#   K8S_VERSION=1.35.10 images/k8s-node-template.sh
#
# The image builder boots the Ubuntu installer, which needs DHCP on PVE_BRIDGE and a route back to the
# machine running this script. DHCP_RANGE=192.0.2.60,192.0.2.69 runs a dnsmasq for the build, on the
# interface of the default route, when the bridge has none. Needs: git make jq curl python3-venv, and
# dnsmasq with DHCP_RANGE.
set -euo pipefail

: "${PVE_URL:?}" "${PVE_TOKEN_ID:?}" "${PVE_TOKEN_SECRET:?}" "${PVE_NODES:?}" "${PVE_TEMPLATE_STORAGE:?}" "${PVE_BRIDGE:?}" "${K8S_VERSION:?}"
IMAGE_BUILDER_REF="${IMAGE_BUILDER_REF:-v0.1.55}"
ISO_NAME="${ISO_NAME:-ubuntu-24.04.4-live-server-amd64.iso}"
ISO_URL="${ISO_URL:-https://releases.ubuntu.com/noble/$ISO_NAME}"
ISO_SHA256="${ISO_SHA256:-e907d92eeec9df64163a7e454cbc8d7755e8ddc7ed42f99dbc80c40f1a138433}"
TEMPLATE_POOL="${TEMPLATE_POOL:-pc-templates}"
WORK="${WORK:-$HOME/.cache/proxcloud-image-builder}"

series="v${K8S_VERSION%.*}"
name="ubuntu-2404-kube-v$K8S_VERSION"
tag="pc-k8s-$(echo "$K8S_VERSION" | tr . -)"
first="${PVE_NODES%% *}"
api="${PVE_URL%/}/api2/json"

pve() { curl -fsSk -H "Authorization: PVEAPIToken=$PVE_TOKEN_ID=$PVE_TOKEN_SECRET" "$@"; }

wait_task() { # node upid
  local up
  up="$(printf %s "$2" | jq -sRr @uri)"
  while :; do
    st="$(pve "$api/nodes/$1/tasks/$up/status")"
    [ "$(jq -r .data.status <<<"$st")" = stopped ] && break
    sleep 3
  done
  [ "$(jq -r .data.exitstatus <<<"$st")" = OK ] || { echo "task failed: $st" >&2; return 1; }
}

vmid_of() { # template name on node -> vmid, empty when absent
  pve "$api/cluster/resources?type=vm" |
    jq -r --arg n "$1" --arg node "$2" '.data[] | select(.name==$n and .node==$node and .template==1) | .vmid' | head -1
}

if [ -z "$(vmid_of "$name" "$first")" ]; then
  pve -X POST "$api/nodes/$first/storage/$PVE_TEMPLATE_STORAGE/download-url" \
    --data-urlencode "url=$ISO_URL" -d content=iso -d "filename=$ISO_NAME" -d checksum-algorithm=sha256 -d "checksum=$ISO_SHA256" \
    >/dev/null 2>&1 || true # already there, or the build will say so

  [ -d "$WORK/image-builder" ] || git clone -q --depth 1 -b "$IMAGE_BUILDER_REF" https://github.com/kubernetes-sigs/image-builder "$WORK/image-builder"
  cd "$WORK/image-builder/images/capi"
  [ -d "$WORK/venv" ] || python3 -m venv "$WORK/venv"
  # shellcheck disable=SC1091
  source "$WORK/venv/bin/activate"
  export PATH="$PWD/.local/bin:$PATH"

  # Upstream's template predates the plugin's boot_iso block and has no pool field. The pool is what the
  # build token's ACL covers.
  TEMPLATE_POOL="$TEMPLATE_POOL" python3 - <<'PY'
import json, os
path = 'packer/proxmox/packer.json.tmpl'
doc = json.load(open(path))
b = doc['builders'][0]
b['pool'] = os.environ['TEMPLATE_POOL']
if 'boot_iso' not in b:
    b['boot_iso'] = {'iso_file': b.pop('iso_file'), 'unmount': True}
    for k in ('iso_url', 'iso_checksum', 'iso_storage_pool', 'unmount_iso'):
        b.pop(k, None)
# Nested or slow nodes need more than upstream's 10s to come back from the reboot: Ansible then hangs on
# a dead proxy connection instead of retrying.
for p in doc['provisioners']:
    if p.get('pause_before') == '10s':
        p['pause_before'] = '90s'
json.dump(doc, open(path, 'w'), indent=2)
PY

  if [ -n "${DHCP_RANGE:-}" ]; then
    iface="$(ip -o -4 route show default | awk '{print $5}')"
    sudo systemd-run --unit=pc-image-dhcp --collect /usr/sbin/dnsmasq --no-daemon --port=0 --bind-interfaces --interface="$iface" \
      "--dhcp-range=$DHCP_RANGE,2h" "--dhcp-option=3,${DHCP_GATEWAY:?with DHCP_RANGE}" --dhcp-option=6,1.1.1.1 --no-resolv
    trap 'sudo systemctl stop pc-image-dhcp' EXIT
  fi

  vars="$(mktemp)"
  cat >"$vars" <<EOF
{"kubernetes_semver":"v$K8S_VERSION","kubernetes_series":"$series","kubernetes_deb_version":"$K8S_VERSION-1.1","kubernetes_rpm_version":"$K8S_VERSION",
 "disk_format":"raw","cores":"2","sockets":"2","memory":"4096","disk_size":"20G","cpu_type":"host"}
EOF
  PROXMOX_URL="$api" PROXMOX_USERNAME="$PVE_TOKEN_ID" PROXMOX_TOKEN="$PVE_TOKEN_SECRET" PROXMOX_NODE="$first" \
    PROXMOX_ISO_POOL="$PVE_TEMPLATE_STORAGE" PROXMOX_BRIDGE="$PVE_BRIDGE" PROXMOX_STORAGE_POOL="$PVE_TEMPLATE_STORAGE" \
    ISO_FILE="$PVE_TEMPLATE_STORAGE:iso/$ISO_NAME" PACKER_VAR_FILES="$vars" make build-proxmox-ubuntu-2404
fi

src="$(vmid_of "$name" "$first")"
[ -n "$src" ] || { echo "no template $name on $first" >&2; exit 1; }
images=""
for node in $PVE_NODES; do
  id="$(vmid_of "$name" "$node")"
  if [ -z "$id" ]; then
    id="$(pve "$api/cluster/nextid" | jq -r .data)"
    upid="$(pve -X POST "$api/nodes/$first/qemu/$src/clone" -d "newid=$id" -d "name=$name" -d "target=$node" \
      -d "storage=$PVE_TEMPLATE_STORAGE" -d "pool=$TEMPLATE_POOL" -d full=1 -d format=raw | jq -r .data)"
    wait_task "$first" "$upid"
    pve -X POST "$api/nodes/$node/qemu/$id/template" >/dev/null
  fi
  pve -X PUT "$api/nodes/$node/qemu/$id/config" -d "tags=proxcloud;pc-k8s;$tag" >/dev/null
  images="$images${images:+,}$node=$id"
done
# What the kamaji provider takes as this version's image (config.cluster.versions).
echo "$K8S_VERSION=$images"
