#!/usr/bin/env bash
# Builds the Kubernetes node template with kubernetes-sigs/image-builder and gives every PVE node its own
# copy: CAPMOX clones to local storage only on the template's node.
#
#   PVE_URL=https://192.0.2.11:8006 PVE_TOKEN_ID='images@pve!packer' PVE_TOKEN_SECRET=... \
#   PVE_NODES="pve1 pve2 pve3" PVE_TEMPLATE_STORAGE=nas-templates PVE_BRIDGE=vmbr0 \
#   K8S_VERSION=1.35.10 K8S_OS=ubuntu-2604 images/k8s-node-template.sh
#
# PVE_BUILD_NETWORK_MODE=static attaches a private local NoCloud seed ISO; no DHCP or
# inbound seed HTTP server is required. The script default remains dhcp for compatibility.
# Static needs BUILD_IP (host CIDR), BUILD_GATEWAY, BUILD_NAMESERVER and xorriso.
# DHCP_RANGE starts optional host dnsmasq only in explicit dhcp mode; see images/README.md.
set -euo pipefail
umask 077
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
PVE_BUILD_NETWORK_MODE="${PVE_BUILD_NETWORK_MODE:-dhcp}"
export PVE_BUILD_NETWORK_MODE

: "${PVE_URL:?}" "${PVE_TOKEN_ID:?}" "${PVE_TOKEN_SECRET:?}" "${PVE_NODES:?}" "${PVE_TEMPLATE_STORAGE:?}" "${PVE_BRIDGE:?}" "${K8S_VERSION:?}"
# Validate before any API request; never echo a private VLAN value on failure.
PVE_BUILD_VLAN="${PVE_BUILD_VLAN:-0}"
if [[ ! "$PVE_BUILD_VLAN" =~ ^(0|[1-9][0-9]{0,3})$ ]] || (( PVE_BUILD_VLAN > 4094 )); then
  echo 'PVE_BUILD_VLAN must be 0 (untagged) or an integer from 1 to 4094' >&2
  exit 1
fi
PVE_ISO_STORAGE="${PVE_ISO_STORAGE:-$PVE_TEMPLATE_STORAGE}"
# The helper owns one allowlist for OS/seed/ISO selection and the immutable pin.
# Capture normally (not process substitution): validation failure must stop the shell.
selection="$(python3 "$SCRIPT_DIR/k8s-build-config.py" select)"
mapfile -t selected <<<"$selection"
[ "${#selected[@]}" = 6 ] || { echo 'Invalid reviewed image selection' >&2; exit 1; }
K8S_OS="${selected[0]}"
ISO_NAME="${selected[1]}"
ISO_URL="${selected[2]}"
ISO_SHA256="${selected[3]}"
readonly IMAGE_BUILDER_REF="${selected[4]}"
readonly IMAGE_BUILDER_COMMIT="${selected[5]}"
export K8S_OS
TEMPLATE_POOL="${TEMPLATE_POOL:-pc-templates}"
WORK="${WORK:-$HOME/.cache/proxcloud-image-builder}"
# Selection validated static inputs before any PVE operation, including downloads.
# Never fetch/reset/clean a shared existing checkout, or trust a SHA from the env.
if [ -e "$WORK/image-builder" ] || [ -L "$WORK/image-builder" ]; then
  python3 "$SCRIPT_DIR/k8s-build-config.py" verify-checkout --checkout "$WORK/image-builder"
fi
if [ "$PVE_BUILD_NETWORK_MODE" = static ]; then
  command -v xorriso >/dev/null || { echo 'Static build mode requires xorriso' >&2; exit 1; }
fi

name="$K8S_OS-kube-v$K8S_VERSION"
tag="pc-k8s-${K8S_VERSION//./-}"
os_tag="pc-k8s-os-$K8S_OS"
first="${PVE_NODES%% *}"
api="${PVE_URL%/}/api2/json"

pve() { curl -fsSk -H "Authorization: PVEAPIToken=$PVE_TOKEN_ID=$PVE_TOKEN_SECRET" "$@"; }

wait_task() { # node upid; explicit checks also apply when caller handles failure.
  local up st status deadline=$((SECONDS + 600))
  up="$(printf %s "$2" | jq -sRr @uri)" || return 1
  while (( SECONDS < deadline )); do
    st="$(pve "$api/nodes/$1/tasks/$up/status")" || return 1
    status="$(jq -er '.data.status | select(.=="running" or .=="stopped")' <<<"$st")" || return 1
    if [ "$status" = stopped ]; then
      jq -e '.data.exitstatus=="OK"' <<<"$st" >/dev/null || { echo 'PVE build task failed (details retained in private PVE task log)' >&2; return 1; }
      return 0
    fi
    sleep 3
  done
  echo 'PVE build task did not finish within the wait budget' >&2
  return 1
}

vmid_of() { # template name on node -> one vmid, empty when absent; reads fail closed
  local rows
  rows="$(pve "$api/cluster/resources?type=vm")" || return 1
  jq -er '.data | type=="array"' <<<"$rows" >/dev/null || return 1
  jq -r --arg n "$1" --arg node "$2" '[.data[] | select(.name==$n and .node==$node and .template==1) | .vmid] | if length>1 then error("ambiguous template name") elif length==0 then "" else .[0] end' <<<"$rows"
}

wait_built_template() { # Only this successful build may wait for the cluster cache.
  local baseline="$1" id cfg members status deadline=$((SECONDS + 120))
  [ "$built" = 1 ] && [ -n "$RUN_DIR" ] && [ "$SEED_UPLOADED" = 0 ] || return 1
  while (( SECONDS < deadline )); do
    # Retry absence (including the old packer name/template flag), never read errors
    # or ambiguity. Pre-build/static lookups remain immediate and fail closed.
    id="$(vmid_of "$name" "$first")" || return 1
    if [ -n "$id" ]; then
      [[ "$id" =~ ^[1-9][0-9]*$ ]] || return 1
      jq -e --argjson id "$id" 'index($id)==null' <<<"$baseline" >/dev/null || {
        echo 'Build discovery refuses a preexisting VMID; publication refused' >&2; return 1;
      }
      members="$(pve "$api/pools/$TEMPLATE_POOL")" || return 1
      jq -e --argjson id "$id" --arg node "$first" '.data.members | type=="array" and ([.[] | select(.vmid==$id and .node==$node and .type=="qemu")] | length==1)' <<<"$members" >/dev/null || {
        echo 'Build template pool verification failed; publication refused' >&2; return 1;
      }
      cfg="$(pve "$api/nodes/$first/qemu/$id/config")" || return 1
      jq -e --arg n "$name" '.data | type=="object" and .name==$n and .template==1' <<<"$cfg" >/dev/null || {
        echo 'Build template identity verification failed; publication refused' >&2; return 1;
      }
      status="$(pve "$api/nodes/$first/qemu/$id/status/current")" || return 1
      jq -e '.data | type=="object" and .status=="stopped"' <<<"$status" >/dev/null || {
        echo 'Build template is not stopped; publication refused' >&2; return 1;
      }
      printf '%s\n' "$id"
      return 0
    fi
    sleep 3
  done
  echo 'Build template discovery timed out after 120s; publication refused' >&2
  return 1
}

clean_build_network() { # node vmid: CAPMOX creates net0 from the tenant's NetworkDevices spec.
  local cfg
  cfg="$(pve "$api/nodes/$1/qemu/$2/config")"
  jq -e '.data | type == "object"' <<<"$cfg" >/dev/null
  if jq -e '.data | has("net0")' <<<"$cfg" >/dev/null; then
    # Remove rather than untag: an untagged NIC could attach to an unauthorized
    # native network, and would require broader SDN.Use than the build VLAN.
    pve -X PUT "$api/nodes/$1/qemu/$2/config" -d delete=net0 \
      --data-urlencode "digest=$(jq -er '.data.digest' <<<"$cfg")" >/dev/null
    cfg="$(pve "$api/nodes/$1/qemu/$2/config")"
    jq -e '.data | type == "object" and (has("net0") | not)' <<<"$cfg" >/dev/null
  fi
}

RUN_DIR=""
built=0
SEED_VOLUME=""
SEED_UPLOADED=0
DHCP_STARTED=0
KEEP_PRIVATE_RUN=0

seed_detached() {
  local rows ids id cfg
  rows="$(pve "$api/cluster/resources?type=vm")" || return 1
  jq -e '.data | type=="array" and all(.[]; type=="object" and (.type=="qemu" or .type=="lxc") and (.node|type=="string") and (.vmid|type=="number"))' <<<"$rows" >/dev/null || return 1
  ids="$(jq -r --arg node "$first" '.data[] | select(.node==$node and .type=="qemu") | .vmid' <<<"$rows")" || return 1
  # Read errors are not proof of detachment, including an empty/malformed resources response.
  for id in $ids; do
    [[ "$id" =~ ^[0-9]+$ ]] || return 1
    cfg="$(pve "$api/nodes/$first/qemu/$id/config")" || return 1
    jq -e --arg vol "$SEED_VOLUME" '.data | type=="object" and ([to_entries[] | select(.key|test("^(ide|sata|scsi)[0-9]+$")) | .value | startswith($vol+",")] | any | not)' <<<"$cfg" >/dev/null || return 1
  done
}

remove_seed() {
  local response task content
  [ "$SEED_UPLOADED" = 1 ] || return 0
  seed_detached || return 1
  [[ "$SEED_VOLUME" = "$PVE_ISO_STORAGE:iso/pc-build-seed-"*.iso ]] || return 1
  response="$(pve -X DELETE "$api/nodes/$first/storage/$PVE_ISO_STORAGE/content/$(printf %s "$SEED_VOLUME" | jq -sRr @uri)")" || return 1
  task="$(jq -er '.data | if .==null then "" elif type=="string" then . else error("bad delete response") end' <<<"$response")" || return 1
  if [ -n "$task" ]; then wait_task "$first" "$task" || return 1; fi
  content="$(pve "$api/nodes/$first/storage/$PVE_ISO_STORAGE/content?content=iso")" || return 1
  jq -e --arg vol "$SEED_VOLUME" '.data | type=="array" and ([.[] | select(.volid==$vol)] | length==0)' <<<"$content" >/dev/null || return 1
  SEED_UPLOADED=0
}

finish_run() {
  local status=$?
  trap - EXIT
  if [ "$DHCP_STARTED" = 1 ]; then sudo systemctl stop pc-image-dhcp || status=1; fi
  if ! remove_seed; then
    # Keep the private exact-volume note for manual owner-checked recovery; it is
    # not safe to delete seed media still attached to a surviving build VM.
    echo 'Sensitive build seed cleanup incomplete; publication refused. Review private run state.' >&2
    status=1
  elif [ -n "$RUN_DIR" ] && [ "$KEEP_PRIVATE_RUN" = 0 ]; then
    case "$RUN_DIR" in
      "$WORK"/run.*) [ "$(realpath -- "$RUN_DIR")" = "$RUN_DIR" ] && rm -rf -- "$RUN_DIR" ;;
      *) echo 'Refusing unexpected run cleanup path' >&2; status=1 ;;
    esac
  fi
  exit "$status"
}
trap finish_run EXIT

if [ "$PVE_BUILD_NETWORK_MODE" = static ]; then
  for node in $PVE_NODES; do
    existing="$(vmid_of "$name" "$node")"
    if [ -n "$existing" ]; then
      echo 'Static build refuses existing same-name templates without guest cleanup proof; use an explicitly reviewed new version/rebuild' >&2
      exit 1
    fi
  done
fi

existing="$(vmid_of "$name" "$first")"
if [ -z "$existing" ]; then
  mkdir -p -- "$WORK"
  WORK="$(realpath -- "$WORK")"
  RUN_DIR="$(mktemp -d "$WORK/run.XXXXXXXX")"
  chmod 0700 "$RUN_DIR"
  if [ -e "$WORK/image-builder" ] || [ -L "$WORK/image-builder" ]; then
    checkout="$WORK/image-builder"
  else
    # Clone only into the private run; never replace/update a shared cached tree.
    checkout="$RUN_DIR/checkout"
    git clone -q --depth 1 --single-branch -b "$IMAGE_BUILDER_REF" https://github.com/kubernetes-sigs/image-builder "$checkout" >"$RUN_DIR/checkout.log" 2>&1
  fi
  # Reverify origin/pin/cache, then export only pinned object bytes. Do not copy
  # hidden working-tree changes or the cached .git config/hooks into the build.
  python3 "$SCRIPT_DIR/k8s-build-config.py" export-checkout --checkout "$checkout" --run "$RUN_DIR"
  pve -X POST "$api/nodes/$first/storage/$PVE_ISO_STORAGE/download-url" \
    --data-urlencode "url=$ISO_URL" -d content=iso -d "filename=$ISO_NAME" -d checksum-algorithm=sha256 -d "checksum=$ISO_SHA256" \
    >/dev/null 2>&1 || true # already there, or the build will say so

  cd "$RUN_DIR/image-builder/images/capi"
  [ -d "$WORK/venv" ] || python3 -m venv "$WORK/venv"
  # shellcheck disable=SC1091
  source "$WORK/venv/bin/activate"
  export PATH="$PWD/.local/bin:$PATH"

  # Render password-bearing files privately rather than upstream's tee-to-log helper.
  TEMPLATE_POOL="$TEMPLATE_POOL" PVE_ISO_STORAGE="$PVE_ISO_STORAGE" \
    ISO_NAME="$ISO_NAME" ISO_URL="$ISO_URL" ISO_SHA256="$ISO_SHA256" \
    python3 "$SCRIPT_DIR/k8s-build-config.py" prepare --capi "$PWD" --run "$RUN_DIR"

  if [ "$PVE_BUILD_NETWORK_MODE" = static ]; then
    SEED_VOLUME="$(<"$RUN_DIR/seed-volume")"
    seed_name="$(<"$RUN_DIR/seed-name")"
    command -v xorriso >/dev/null || { echo 'Static build mode requires xorriso' >&2; exit 1; }
    xorriso -as mkisofs -quiet -volid cidata -joliet -rock -output "$RUN_DIR/$seed_name" \
      "$RUN_DIR/seed/user-data" "$RUN_DIR/seed/meta-data" "$RUN_DIR/seed/network-config" >/dev/null 2>&1
    # Prove absence before claiming exact-name ownership. An interrupted upload may
    # still create the file, so track it before the transfer, but never an old object.
    content="$(pve "$api/nodes/$first/storage/$PVE_ISO_STORAGE/content?content=iso")"
    jq -e --arg vol "$SEED_VOLUME" '.data | type=="array" and ([.[] | select(.volid==$vol)] | length==0)' <<<"$content" >/dev/null
    SEED_UPLOADED=1
    upid="$(pve -X POST "$api/nodes/$first/storage/$PVE_ISO_STORAGE/upload" \
      -F content=iso -F "filename=@$RUN_DIR/$seed_name" | jq -er .data)"
    wait_task "$first" "$upid"
    pve "$api/nodes/$first/storage/$PVE_ISO_STORAGE/content?content=iso" |
      jq -e --arg vol "$SEED_VOLUME" '[.data[] | select(.volid==$vol)] | length==1' >/dev/null
  fi

  if [ -n "${DHCP_RANGE:-}" ]; then
    iface="$(ip -o -4 route show default | awk '{print $5}')"
    sudo systemd-run --unit=pc-image-dhcp --collect /usr/sbin/dnsmasq --no-daemon --port=0 --bind-interfaces --interface="$iface" \
      "--dhcp-range=$DHCP_RANGE,2h" "--dhcp-option=3,${DHCP_GATEWAY:?with DHCP_RANGE}" --dhcp-option=6,1.1.1.1 --no-resolv
    DHCP_STARTED=1
  fi

  vars="$RUN_DIR/vars.json"
  # Proxmox plugin represents an untagged adapter with an empty string, not tag=0.
  vlan_tag=""; if (( PVE_BUILD_VLAN )); then vlan_tag="$PVE_BUILD_VLAN"; fi
  # The workflows serialize writers. Still exclude every VMID already visible on
  # the build node, regardless of name/pool, before this make may create a VM.
  baseline_rows="$(pve "$api/cluster/resources?type=vm")"
  jq -e '.data | type=="array" and all(.[]; type=="object" and (.node|type=="string") and (.vmid|type=="number"))' <<<"$baseline_rows" >/dev/null
  baseline_ids="$(jq -c --arg node "$first" '[.data[] | select(.node==$node) | .vmid]' <<<"$baseline_rows")"
  echo 'Starting image-builder; detailed output is retained only in private per-run state'
  if ! PROXMOX_URL="$api" PROXMOX_USERNAME="$PVE_TOKEN_ID" PROXMOX_TOKEN="$PVE_TOKEN_SECRET" PROXMOX_NODE="$first" \
    PROXMOX_ISO_POOL="$PVE_ISO_STORAGE" PROXMOX_BRIDGE="$PVE_BRIDGE" PROXMOX_VLAN="$vlan_tag" PROXMOX_STORAGE_POOL="$PVE_TEMPLATE_STORAGE" \
    ISO_FILE="$PVE_ISO_STORAGE:iso/$ISO_NAME" PACKER_VAR_FILES="$vars" \
    PACKER_LOG=0 PACKER_LOG_PATH= PACKER_DEBUG=0 DEBUG= \
    IB_VERSION="$IMAGE_BUILDER_REF-$IMAGE_BUILDER_COMMIT" \
    PACKER_BIN="$(command -v packer)" \
    make --silent -o set-ssh-password "build-proxmox-$K8S_OS" >"$RUN_DIR/build.log" 2>&1; then
    echo 'Image-builder failed; detailed output remains in private run state (not public logs)' >&2
    KEEP_PRIVATE_RUN=1
    exit 1
  fi
  # Sensitive local media must be detached and the exact uploaded seed deleted
  # before *any* clone or pc-k8s publication. Packer success alone is insufficient.
  remove_seed || { echo 'Sensitive build seed cleanup incomplete; publication refused' >&2; exit 1; }
  built=1
fi

if [ "$built" = 1 ]; then
  src="$(wait_built_template "$baseline_ids")"
else
  src="$(vmid_of "$name" "$first")"
fi
[ -n "$src" ] || { echo "no template $name on $first" >&2; exit 1; }
# image-builder has finished its in-guest tests on the tagged build network. Clean
# the source BEFORE cloning/publication, and also heal older per-node copies.
clean_build_network "$first" "$src"
images=""
for node in $PVE_NODES; do
  if [ "$node" = "$first" ]; then
    id="$src" # Keep the source identity just verified, not a second cache lookup.
  else
    id="$(vmid_of "$name" "$node")"
  fi
  if [ -z "$id" ]; then
    id="$(pve "$api/cluster/nextid" | jq -r .data)"
    upid="$(pve -X POST "$api/nodes/$first/qemu/$src/clone" -d "newid=$id" -d "name=$name" -d "target=$node" \
      -d "storage=$PVE_TEMPLATE_STORAGE" -d "pool=$TEMPLATE_POOL" -d full=1 -d format=raw | jq -r .data)"
    wait_task "$first" "$upid"
    pve -X POST "$api/nodes/$node/qemu/$id/template" >/dev/null
  fi
  clean_build_network "$node" "$id"
  pve -X PUT "$api/nodes/$node/qemu/$id/config" --data-urlencode "tags=proxcloud;pc-k8s;$tag;$os_tag" >/dev/null
  images="$images${images:+,}$node=$id"
done
# What the kamaji provider takes as this version's image (config.cluster.versions).
echo "$K8S_VERSION=$images"
