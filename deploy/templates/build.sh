#!/usr/bin/env bash
# Builds the built-in sandbox template images (docs/managed-sandbox.md §5.4).
#   PUSH=1 REGISTRY=ghcr.io/mutex-solutions TAG=20261108-abc1234 deploy/templates/build.sh [names...]
# Without PUSH the images stay local (docker load). Prints "<name> <ref>[@<digest>]" per image; put the pushed
# reference (with its digest) into the sandbox provider's config.sandbox.templates.
set -euo pipefail
cd "$(dirname "$0")"
REGISTRY=${REGISTRY:-ghcr.io/mutex-solutions}
TAG=${TAG:-dev}
PUSH=${PUSH:-0}
# e2b-dev/code-interpreter (Apache-2.0): the Jupyter server and E2B's FastAPI server on :49999.
CI_UPSTREAM=https://github.com/e2b-dev/code-interpreter.git#c49b78602c29e44d07e196604de515dfa6b12744

names=("$@")
[ ${#names[@]} -gt 0 ] || names=(base code-interpreter)
out=$( [ "$PUSH" = 1 ] && echo "--push" || echo "--load" )

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
  echo "$name $ref"
}

for n in "${names[@]}"; do
  case $n in
    base) build base base ;;
    code-interpreter) build code-interpreter code-interpreter --build-context "upstream=$CI_UPSTREAM" ;;
    *) echo "unknown template $n (SB5 ships base and code-interpreter)" >&2; exit 2 ;;
  esac
done
