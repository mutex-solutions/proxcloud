#!/usr/bin/env bash
# Release precondition of the sandbox cluster: no IPv6 anywhere a sandbox could use it. The guardrail
# (network-policy.yaml) names IPv4 ranges only, so an IPv6 path out of a sandbox would match none of its
# denies. Run with KUBECONFIG pointing at the sandbox cluster, after node-setup.yaml. Exit 1 on any finding.
set -euo pipefail
fail=0
no() { echo "FAIL: $*" >&2; fail=1; }

v6=$(kubectl -n kube-system get configmap cilium-config -o jsonpath='{.data.enable-ipv6}')
[ "$v6" = "false" ] || no "Cilium enable-ipv6 is '${v6:-unset}', want false: pods would get IPv6 addresses"

addrs=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.addresses[*]}{.address}{" "}{end}{"\n"}{end}')
grep -E '[0-9a-fA-F]*:[0-9a-fA-F:]+' <<<"$addrs" && no "a Node reports an IPv6 address"
cidrs=$(kubectl get nodes -o jsonpath='{range .items[*]}{.spec.podCIDRs[*]}{" "}{end}')
grep -q ':' <<<"$cidrs" && no "a Node has an IPv6 pod CIDR: $cidrs"

# node-setup refuses a host with a global IPv6 address or an IPv6 default route (its pod stays unready).
want=$(kubectl get nodes --no-headers | wc -l)
ready=$(kubectl -n sandbox-system get daemonset node-setup -o jsonpath='{.status.numberReady}')
[ "${ready:-0}" = "$want" ] || no "node-setup is Ready on ${ready:-0} of $want nodes (logs: kubectl -n sandbox-system logs -l app=node-setup --prefix)"

[ $fail = 0 ] && echo "ok: IPv4 only (Cilium enable-ipv6=false, no IPv6 on $want nodes)"
exit $fail
