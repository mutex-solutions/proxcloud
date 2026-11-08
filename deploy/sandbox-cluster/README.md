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
| `node-setup.yaml` | DaemonSet: installs gVisor (`runsc`, platform `systrap`, `allow-suid` unless `RUNSC_ALLOW_SUID=false` so `sudo` works in sandboxes; security effect in docs/managed-sandbox.md §1.4) on every node; refuses a node with IPv6 |
| `runtimeclass.yaml` | RuntimeClass `gvisor` |
| `agent-sandbox/` | kubernetes-sigs/agent-sandbox v1.0.9, vendored (Sandbox CRD and controller); see its README |
| `network-policy.yaml` | Cilium cluster-wide guardrail for every tenant namespace: private ranges and TCP 25 denied, ingress from `sandbox-proxy` only |
| `rbac.yaml` | `proxcloud-system/proxcloud` ServiceAccount, its ClusterRole and a token Secret; `proxcloud-system/proxcloud-firewall` (nodes get, list, patch; impersonating only the node `proxcloud-noderestriction-probe` for the NodeRestriction release gate) for the Proxmox provider's sync, which fences the workers and takes their `unsecured` taint off |
| `check-ipv4-only.sh` | The IPv6 precondition: Cilium IPv6 off, no IPv6 on any node |
| `registry.yaml` | The sandbox registry's side (docs/managed-sandbox.md §6): `registry-trust` puts the registry CA under `/etc/containerd/certs.d/<address>/` on every node, from the ConfigMap `sandbox-registry` stage 55 writes; `sandbox-registry-users`, the identity of the account sync on the services cluster (namespaces `list`, and only the pull and build Secrets it writes). The template prepull DaemonSet is rendered by stage 55 from `deploy/templates/images.yaml`, and the template build namespace `sbx-build` (its policy, the registry CA, the portal's and the sync's Roles there) from `installer/playbooks/templates/sandbox-builds.yml.j2` (docs/managed-sandbox.md §7) |
| `sandbox-proxy.yaml` | `sandbox-proxy` (two replicas on the `sandbox-system` nodes only, TLS ends there), its LoadBalancer Service on one MetalLB address, and `sandbox-cert-sync`: the identity that may get and patch `sandbox-proxy-tls` alone, for the copy from the services cluster (docs/managed-sandbox.md §4) |

## Preconditions

1. The cluster comes from the chart at 0.1.6 or later, so `NodeRestriction` is on (`kamajiControlPlane.
   admissionControllers`). Without it a worker's kubelet credential can read every Secret of the cluster.
2. The workers' vnet is in the provider's `network.sandboxCidrs`, so the PVE isolation rules drop tenant traffic to the
   workers and the workers' traffic to tenants and to the management networks (docs/managed-sandbox.md §1.2).
3. Single-stack IPv4 (`check-ipv4-only.sh`). Every rule of the guardrail names IPv4 ranges.
4. The workers join with the `proxcloud.io/unsecured:NoSchedule` taint, and only the Proxmox provider's sync takes it
   off, once the worker's VM firewall is in place and the worker is 15 minutes old (docs/managed-sandbox.md §2). Until
   then only DaemonSets that tolerate every taint run there (Cilium, `node-setup`): CoreDNS and the agent-sandbox
   controller wait, so do not wait for them after step 3.
5. Outbound internet from the nodes: `node-setup` pulls from `gvisor.dev` and `storage.googleapis.com`.

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

# 5. sandbox-proxy (installer stage 56 does this, plus the certificate and the entry appliance). It runs only on nodes
#    labelled node-restriction.kubernetes.io/proxcloud-system by CAPI (stage 55's sandbox-system node group, chart
#    value machineLabels), never on a sandbox worker. Its config, wake token and an empty certificate Secret first;
#    cert-sync on the services cluster fills the certificate. PROXCLOUD_WAKE_URL is where the proxy wakes paused
#    autoResume sandboxes; the workers' firewall must let it reach that address (Proxmox provider config.sandbox.portal).
kubectl -n sandbox-system create configmap sandbox-proxy-config --from-literal=SANDBOX_DOMAIN=example-sandbox.net \
  --from-literal=PROXCLOUD_WAKE_URL=https://api.example.org/api/v1/internal/sandboxes
kubectl -n sandbox-system create secret generic sandbox-proxy-wake --from-literal=PROXCLOUD_WAKE_TOKEN=<proxyWakeToken>
kubectl -n sandbox-system create secret generic sandbox-proxy-tls
kubectl apply -f sandbox-proxy.yaml
kubectl -n sandbox-system set image deploy/sandbox-proxy proxy=ghcr.io/mutex-solutions/proxcloud-sandbox-proxy:<release>
kubectl -n sandbox-system annotate svc sandbox-proxy metallb.io/loadBalancerIPs=<proxy address>
```

## Node runtime contract

Kept by `internal/cloud/sandbox` (`RuntimeHostDir`, `RuntimeMountPath`, `LabelTenant`):

- Tenant namespaces carry `proxcloud.io/tenant=true`; the guardrail selects on it.
- With envd on (`config.sandbox.envd` and a `domain`, SB5), sandbox pods mount `/opt/proxcloud-sandbox/bin` read-only at
  `/.proxcloud` and run `/.proxcloud/sandbox-init` as PID 1. `node-setup.yaml`'s init container installs envd and
  `sandbox-init` there from the `proxcloud-sandbox-runtime` image (stage 55 pins its tag; by hand, `kubectl -n
  sandbox-system set image ds/node-setup sandbox-runtime=ghcr.io/mutex-solutions/proxcloud-sandbox-runtime:<release>`).
  Without envd the provider runs sandboxes as plain gVisor pods.
- GPU placement uses `node-restriction.kubernetes.io/proxcloud-gpu` (SB9), the one label prefix a kubelet cannot set on
  its own Node once NodeRestriction is on.

## Attach to the portal

Build a kubeconfig from the `proxcloud-system/proxcloud-token` Secret (the recipe in the header of `deploy/mgmt-cluster/rbac.yaml`)
and register it as a `sandbox` provider with `config.sandbox.proxmoxServiceId` naming the Proxmox provider of the workers.
Store it `DISABLED` first: it goes live only while its release gates hold (NodeRestriction, the isolation drops, the
worker fence; [managed-sandbox.md §3](../../docs/managed-sandbox.md#36-release-gates-how-the-portal-checks-each)).
