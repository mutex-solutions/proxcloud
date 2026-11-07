# Sandbox cluster manifests

Platform setup of the **sandbox cluster**: the platform-owned Kamaji cluster (chart `proxmox-kamaji-cluster`, workers
in pool `proxcloud-sandbox` on their own vnet) where tenant sandboxes run as gVisor pods. The portal does not apply
these; they set up the runtime, the guardrail and the identity the portal's `sandbox` provider then works within
(the same split as `deploy/mgmt-cluster/` and `deploy/dbaas-cluster/`). Installer stage 55 applies them
(`installer/playbooks/55-sandbox-cluster.yml`); by hand, the order is below.

Design, lab measurements and the threat model: [`docs/managed-sandbox.md`](../../docs/managed-sandbox.md).

| File | What it does |
|---|---|
| `cilium-node-config.yaml` | Cilium socket LB limited to the host namespace on this cluster's nodes. A no-op while the chart runs Cilium without kube-proxy replacement; it keeps gVisor DNS working if that ever changes |
| `node-setup.yaml` | DaemonSet: installs gVisor (`runsc`, platform `systrap`) on every node; refuses a node with IPv6 |
| `runtimeclass.yaml` | RuntimeClass `gvisor` |
| `agent-sandbox/` | kubernetes-sigs/agent-sandbox v1.0.9, vendored (Sandbox CRD and controller); see its README |
| `network-policy.yaml` | Cilium cluster-wide guardrail for every tenant namespace: private ranges and TCP 25 denied, ingress from `sandbox-proxy` only |
| `rbac.yaml` | `proxcloud-system/proxcloud` ServiceAccount, its ClusterRole and a token Secret |
| `check-ipv4-only.sh` | The IPv6 precondition: Cilium IPv6 off, no IPv6 on any node |

## Preconditions

1. The cluster comes from the chart at 0.1.6 or later, so `NodeRestriction` is on (`kamajiControlPlane.
   admissionControllers`). Without it a worker's kubelet credential can read every Secret of the cluster.
2. The workers' vnet is in the provider's `network.sandboxCidrs`, so the PVE isolation rules drop tenant traffic to the
   workers and the workers' traffic to tenants and to the management networks (docs/managed-sandbox.md §1.2).
3. Single-stack IPv4 (`check-ipv4-only.sh`). Every rule of the guardrail names IPv4 ranges.
4. Outbound internet from the nodes: `node-setup` pulls from `gvisor.dev` and `storage.googleapis.com`.

## Apply order

```sh
export KUBECONFIG=<sandbox cluster admin kubeconfig>

# 0. Cilium: socket LB host-namespace only, read at agent start-up.
kubectl apply -f cilium-node-config.yaml
kubectl -n kube-system rollout restart ds/cilium && kubectl -n kube-system rollout status ds/cilium

# 1. Node runtime. Every node must report "node-setup done".
kubectl apply -f node-setup.yaml -f runtimeclass.yaml
kubectl -n sandbox-system rollout status ds/node-setup
kubectl -n sandbox-system logs -l app=node-setup --prefix | grep -E "done|REFUSED"

# 2. IPv4 only, before any tenant namespace exists.
./check-ipv4-only.sh

# 3. agent-sandbox, pinned.
kubectl apply --server-side -f agent-sandbox/sandbox-with-extensions.yaml
kubectl -n agent-sandbox-system rollout status deploy/agent-sandbox-controller

# 4. Guardrail and the portal's identity.
kubectl apply -f network-policy.yaml -f rbac.yaml
```

## Node runtime contract

Kept by `internal/cloud/sandbox` (`RuntimeHostDir`, `RuntimeMountPath`, `LabelTenant`):

- Tenant namespaces carry `proxcloud.io/tenant=true`; the guardrail selects on it.
- With envd on (SB5), sandbox pods mount `/opt/proxcloud-sandbox/bin` read-only at `/.proxcloud` and run
  `/.proxcloud/sandbox-init` as PID 1. The init container that installs envd and `sandbox-init` there comes with the
  runtime image in SB5; until then the provider runs sandboxes as plain gVisor pods.
- GPU placement uses `node-restriction.kubernetes.io/proxcloud-gpu` (SB9), the one label prefix a kubelet cannot set on
  its own Node once NodeRestriction is on.

## Attach to the portal

Build a kubeconfig from the `proxcloud-system/proxcloud-token` Secret (the recipe in the header of `deploy/mgmt-cluster/rbac.yaml`)
and register it as a `sandbox` provider. The admin API refuses the provider kind until SB3.
