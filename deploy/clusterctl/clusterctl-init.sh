#!/bin/sh
# Entrypoint of the proxcloud-clusterctl image: brings the cluster's Cluster API providers to the wanted set.
#   clusterctl-init --core cluster-api:v1.14.4 --bootstrap kubeadm:v1.14.4 --infrastructure proxmox:v0.9.2 ...
# Arguments are clusterctl init's provider flags (--core, --bootstrap, --control-plane, --infrastructure, --ipam,
# --addon), each with NAME:VERSION. Read against the clusterctl inventory:
#   - every provider installed at its wanted version: nothing to do, exit 0;
#   - any provider installed at another version: refuse, install nothing, exit 1 (upgrades are a later release);
#   - otherwise one `clusterctl init` for the missing ones, from the local repository baked into the image.
set -eu

INVENTORY=providers.clusterctl.cluster.x-k8s.io
# Empty on purpose: a manager-level credential silently wins over every ProxmoxCluster.credentialsRef.
export PROXMOX_URL="" PROXMOX_TOKEN="" PROXMOX_SECRET=""

usage() {
  echo "usage: clusterctl-init {--core|--bootstrap|--control-plane|--infrastructure|--ipam|--addon} NAME:VERSION ..." >&2
  exit 2
}
[ "$#" -gt 0 ] || usage

# An absent inventory CRD is an empty cluster; any other kubectl failure stops here.
crd=$(kubectl get crd "$INVENTORY" --ignore-not-found -o name)
installed=""
if [ -n "$crd" ]; then
  installed=$(kubectl get "$INVENTORY" -A -o jsonpath='{range .items[*]}{.type} {.providerName} {.version}{"\n"}{end}')
fi

missing="" refused=""
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] || usage
  case "$1" in
    --core) type=CoreProvider ;;
    --bootstrap) type=BootstrapProvider ;;
    --control-plane) type=ControlPlaneProvider ;;
    --infrastructure) type=InfrastructureProvider ;;
    --ipam) type=IPAMProvider ;;
    --addon) type=AddonProvider ;;
    *) usage ;;
  esac
  # Only a-z 0-9 . : - (no blank, no newline), then exactly NAME:vX.Y.Z.
  case "$2" in *[!a-z0-9.:-]*) usage ;; esac
  printf '%s\n' "$2" | grep -Eqx '[a-z0-9-]+:v[0-9]+\.[0-9]+\.[0-9]+' || usage
  name=${2%%:*} want=${2#*:}
  have=$(printf '%s\n' "$installed" | awk -v t="$type" -v n="$name" '$1 == t && $2 == n { print $3 }' | tr '\n' ' ')
  have=${have% }
  if [ -z "$have" ]; then
    missing="$missing $1 $2"
  elif [ "$have" != "$want" ]; then
    refused="$refused
  $type $name: installed $have, wanted $want"
  fi
  shift 2
done

if [ -n "$refused" ]; then
  echo "refusing to change Cluster API provider versions (upgrades are not supported yet); nothing was installed:$refused" >&2
  exit 1
fi
if [ -z "$missing" ]; then
  echo "every wanted Cluster API provider is installed at its version; nothing to do"
  exit 0
fi
echo "installing:$missing"
# $missing holds only flags and NAME:VERSION words checked above, so splitting it is safe.
# shellcheck disable=SC2086
exec clusterctl init --config /opt/clusterctl/clusterctl.yaml $missing --wait-providers
