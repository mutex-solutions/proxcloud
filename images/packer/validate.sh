#!/usr/bin/env bash
# Full expression/plugin validation with documentation-only values; never runs a VM build.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
export PKR_VAR_proxmox_url=https://pve.invalid/api2/json
export PKR_VAR_token_id=builder@pve!validation
export PKR_VAR_token_secret=validation-only-not-a-credential
export PKR_VAR_node=test-node
export PKR_VAR_base_vmid=9000
export PKR_VAR_vmid=9001
export PKR_VAR_name=validation-image
export PKR_VAR_tags=validation
export PKR_VAR_storage=test-store
export PKR_VAR_bridge=test-bridge
export PKR_VAR_build_ip=192.0.2.40/24
export PKR_VAR_build_gateway=192.0.2.1
export PKR_VAR_nameserver=192.0.2.1

packer init .
# Both branches must be evaluated: syntax-only validation misses unknown functions
# hidden behind a nonzero VLAN branch.
for vlan in 0 29; do
  for vars in os/*.pkrvars.hcl; do
    printf '\nValidate %s with fixture VLAN %s\n' "$vars" "$vlan"
    PKR_VAR_vlan="$vlan" packer validate -var-file="$vars" .
  done
done
