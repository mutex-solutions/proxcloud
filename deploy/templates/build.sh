#!/usr/bin/env bash
# Builds the built-in sandbox template images (docs/managed-sandbox.md §5.4 and §6.3). CI (workflow sandbox-templates)
# pushes them to ghcr and pins each digest in images.yaml.
#   PUSH=1 REGISTRY=ghcr.io/mutex-solutions TAG=20261106-abc1234 deploy/templates/build.sh [names...]
# Without PUSH the images stay local (docker load). Prints "<name> <ref>[@<digest>]" per image.
set -euo pipefail
cd "$(dirname "$0")"
REGISTRY=${REGISTRY:-ghcr.io/mutex-solutions}
TAG=${TAG:-dev}
PUSH=${PUSH:-0}
# e2b-dev/code-interpreter (Apache-2.0): the Jupyter server and E2B's FastAPI server on :49999.
CI_UPSTREAM=https://github.com/e2b-dev/code-interpreter.git#c49b78602c29e44d07e196604de515dfa6b12744
DESKTOP_UPSTREAM=https://github.com/e2b-dev/desktop.git#91f23acaab7312e847596acc44e6f7763f56917f
GPU_BASE=docker.io/pytorch/pytorch@sha256:7b324d212a4450795b49edba9949b7cdc72429148a64e974334bfe5774d51385

names=("$@")
[ ${#names[@]} -gt 0 ] || names=(base code-interpreter code-interpreter-gpu desktop codex opencode claude amp gemini node browser)
out=$( [ "$PUSH" = 1 ] && echo "--push" || echo "--load" )
declare -A built

build() { # name dockerfile-dir extra-args...
  local name=$1 dir=$2 ref meta
  shift 2
  ref="$REGISTRY/sandbox-$name:$TAG"
  meta=$(mktemp)
  docker buildx build --platform linux/amd64 $out --metadata-file "$meta" -f "$dir/Dockerfile" -t "$ref" "$@" . >&2
  if [ "$PUSH" = 1 ]; then
    local digest
    digest=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["containerimage.digest"])' "$meta")
    case "$digest" in sha256:*) ;; *) echo "no digest for $name" >&2; exit 1;; esac
    ref="$ref@$digest"
  fi
  rm -f "$meta"
  built[$name]="$ref"
  echo "$name $ref"
}

need_base() { [ -n "${built[base]:-}" ] || build base base >/dev/null; }

for n in "${names[@]}"; do
  case $n in
    base) build base base ;;
    code-interpreter) build code-interpreter code-interpreter --build-context "upstream=$CI_UPSTREAM" ;;
    code-interpreter-gpu) build code-interpreter-gpu code-interpreter --build-context "upstream=$CI_UPSTREAM" --build-arg "BASE_IMAGE=$GPU_BASE" ;;
    desktop) build desktop desktop --build-context "upstream=$DESKTOP_UPSTREAM" ;;
    codex|opencode|claude|amp|gemini) need_base; build "$n" "$n" --build-arg "BASE=${built[base]}" ;;
    node|browser) build "$n" "$n" ;;
    *) echo "unknown template $n" >&2; exit 2 ;;
  esac
done
