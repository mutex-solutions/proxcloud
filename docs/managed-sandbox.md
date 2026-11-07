# Managed sandboxes (gVisor) — design notes and runbook

Sandboxes are short-lived containers for untrusted code (agents, code interpreters), run under gVisor (`runsc`) on a
Kubernetes cluster that belongs to the platform, with an E2B-compatible API in front. The feature is being ported from
an earlier implementation by the same owner (called **EE** below). It is not in the product yet: this file starts with
the lab spike (phase SB0) and grows into the operator runbook as the phases land.

> ## Status
>
> **Spike only.** No code, no provider kind, no route. Section 0 records what a hand-built sandbox cluster on Proxmox
> showed. Phase order and decisions: the sandbox plan (owner document, not in this repository).

## 0. SB0 lab spike (2026-11-07)

Built by hand on a nested PVE lab, outside any portal: one Kamaji cluster from `deploy/charts/proxmox-kamaji-cluster`
0.1.5, gVisor on its worker, agent-sandbox, one `Sandbox` with `runtimeClassName: gvisor`, exec through konnectivity, and
the EE Cilium guardrail, then packets from inside the sandbox.

### 0.1 What ran

| Piece | Version or setting |
|---|---|
| PVE | 9.2.26, nested (PVE VMs on a PVE host), EVPN zone with one exit node |
| Services cluster (management side) | CAPI v1.14.4, CABPK v1.14.4 (`--bootstrap-token-ttl=15m`), CAPMOX v0.9.2, CAAPH v0.6.4, Kamaji 26.11.2-edge, Kamaji CP provider v0.22.0, konnectivity server v0.35.0 |
| Cluster chart | `proxmox-kamaji-cluster` 0.1.5, rendered with `helm template` and applied with `kubectl` (no ArgoCD Application, so no portal sync sees it) |
| Tenant control plane | Kubernetes v1.35.10, LoadBalancer on the services cluster's MetalLB, konnectivity addon |
| Worker | 1 VM, 2 vCPU, 3 GiB, 30 GB, node image `ubuntu-2404-kube-v1.35.10` (Ubuntu 24.04.5, kernel 6.8, containerd 2.3.2) |
| Pool, network | pool `proxcloud-sandbox`; vnet `pcsbx` in the existing EVPN zone, its own /24 outside the portals' tenant pool, SNAT on, no DHCP, MTU 1450 |
| CNI and LB | Cilium 1.18.16 and MetalLB 0.16.2 from the chart (`kubeProxyReplacement: false`, kube-proxy from Kamaji) |
| CSI, NAS | none: `storageClasses: []`, no RWX dataset, no CSI token |
| CAPMOX token | `capmox@pve!sbx0`, minted by hand (§0.4) |
| gVisor | `runsc` 20261102.0 from the gVisor apt repo, platform `systrap`, installed by a `node-setup` DaemonSet |
| agent-sandbox | v1.0.9 `sandbox-with-extensions.yaml`, the EE vendored copy (upstream file plus a three-line header, upstream sha256 `4bf9ee5558e9a663…`), server-side apply |
| Guardrail | EE `network-policy.yaml`, label `proxcloud.io/tenant=true`, plus a port 25 deny |

Timings on this lab (nested, so slower than hardware):

| Step | Time |
|---|---|
| `kubectl apply` of the rendered chart → worker VM cloned | 70 s (CAPMOX waits for the TCP endpoint) |
| clone → VM started | 78 s |
| VM start → Node registered | about 50 s; bootstrap ISO detached and deleted 108 s after start |
| Whole cluster with Cilium and MetalLB pods Running | about 5 min |
| `node-setup` (apt install of runsc, containerd restart) | 49 s |
| agent-sandbox controller Ready | 6 s after apply |

### 0.2 Cold start and exec

`Sandbox` CR created → `Ready` condition, polled every 0.5 s, so each figure carries up to about 0.7 s of polling and
`kubectl` latency. Exec is a round trip of `kubectl exec … true` through the tenant API server and konnectivity.

| Case | Runs | Ready after | Exec round trip |
|---|---|---|---|
| gVisor, image cached (`python:3.12-slim`) | 5 | 3.4 – 4.2 s, median 4.1 s | 0.41 – 0.51 s |
| runc, image cached | 3 | 2.0 – 3.5 s, median 2.8 s | 0.33 – 0.38 s |
| gVisor, image not on the node (`node:22-slim`) | 1 | 17.6 s (pull) | 0.49 s |
| same image again | 1 | 4.4 s | 0.41 s |

gVisor adds about 1 s to a start. The pull dominates a first start, which is what template prepull (SB6a) is for.

**Syscall cost depends on the CPU limit.** A loop of 200,000 `write(2)` to `/dev/null` (Python):

| Runtime | CPU limit | Per call |
|---|---|---|
| runc | 500m | 5.7 µs (Python loop included) |
| gVisor systrap | 250m | 237 µs |
| gVisor systrap | 500m | 298 µs |
| gVisor systrap | 1000m | 35 µs |
| gVisor systrap | 1500m | 23 µs |

A pure CPU loop costs the same under both runtimes (1.73 s). Under systrap the sentry and its stub threads spend the
pod's own CFS quota, so a small limit is throttled hard. Sandbox sizes below 1 vCPU will feel broken for anything that
does I/O; the smallest size offered should be 1 vCPU, and the price per minute has to assume it. Nested virtualization
inflates every figure here; measure again on hardware before publishing numbers.

### 0.3 Network: packets from inside the sandbox

A Python probe ran TCP connects (3 s timeout), DNS and one HTTPS GET from a gVisor sandbox, first with no policy, then
with the guardrail, and from the worker's host network namespace (a privileged `hostPID` + `hostNetwork` pod standing
in for code that escaped gVisor). Targets are named by role; the lab addresses are in the lab notes.

| Target | Sandbox, no policy | Sandbox, guardrail | Node namespace |
|---|---|---|---|
| PVE API :8006 and ssh on a node that is not the EVPN exit node | open | blocked | open |
| PVE API on the exit node itself | refused (RST) | blocked | refused (RST) |
| NAS: NFS :2049, TrueNAS API :443 | open | blocked | open |
| Management host: ssh, k3s API, PowerDNS API | open | blocked | open |
| RustFS :9000, RGW :7480 | open | blocked | open |
| PVE API forwarder on the fake public network | open | blocked | open |
| Services cluster API | timeout | blocked | timeout |
| Its own tenant control plane :6443 | open | blocked | open |
| Another tenant's control plane :6443 | open | blocked | open |
| A tenant database LoadBalancer :5432 | open | blocked | open |
| A tenant VM's public address :22 | timeout | blocked | timeout |
| A VM on another project's vnet :22 | timeout | blocked | timeout |
| Its own node's kubelet :10250 | open | blocked | open |
| The outer host (the machine the nested PVE runs on) :22 | open | blocked | open |
| 169.254.169.254 | timeout (PVE has none) | blocked | timeout |
| `kubernetes` ClusterIP :443 | open | blocked | open |
| Internet: 1.1.1.1:443, HTTPS GET example.com | open, 200 | open, 200 | open, 200 |
| SMTP :25 / :587 (IPv4) | open / open | **blocked** / open | open / open |
| Cluster DNS | ok | ok | not its resolver |

"Blocked" is a timeout: Cilium drops, nothing answers. The guardrail is two `CiliumClusterwideNetworkPolicy` objects
(EE `sandbox-tenant-egress` with an added `egressDeny` on TCP 25, and `sandbox-tenant-ingress`) plus one namespace
policy standing in for EE's per-sandbox `<id>-egress` (internet minus the private ranges). It applied to a running
sandbox without a restart and held for every new one, after a node reset and after Cilium restarts.

What this shows:

- **Layer 2 (Cilium) works as in EE.** Every private, CGNAT and link-local destination is dropped, including the
  sandbox's own control plane and kubelet, while DNS and the internet stay open. The lab's "public" range is RFC1918, so
  the guardrail also blocks other tenants' public addresses there; on a real site those are ordinary internet.
- **Layer 1 does not exist yet, and it is needed.** From the node namespace everything the sandbox could reach before the
  policy stays reachable: the PVE API, the NAS, the management host, the object stores, the API forwarder, other
  tenants' control planes and database load balancers. The portal's cluster isolation rules match only the tenant CIDR
  pool, so a vnet outside it (as the plan wants for `pcsbx`) gets no drop rule at all. A gVisor escape on this cluster
  today is a foothold on the management network. This is SB2b (per-NIC VM firewall), and it blocks any tenant use.
- **The NAS accepts that foothold as a node.** Traffic from the sandbox vnet leaves through the EVPN exit node, which
  SNATs it to its own node address. The NAS exports (templates, capacity, backup) admit the node subnet, so code that
  escapes gVisor can mount every VM disk on the NAS stores, every backup archive and the ISO store. No export rule can
  tell the two apart once the source is rewritten; the drop has to happen before the exit node (SB2b, and the sandbox
  CIDR in the isolation DROP set).
- Rejects from the exit node's own addresses show that "refused" is not "blocked": the packet got there.

**The other direction: tenant VMs to the sandbox workers.** The table above covers only traffic leaving a sandbox.
Traffic arriving at a sandbox worker from a tenant is not covered either, read from the lab configuration (not probed):

- The EVPN zone has one VRF per node, and on every node that VRF holds the routes of every tenant vnet and of the
  sandbox vnet together, so a tenant VM routes to the sandbox worker through its own gateway.
- The portal's isolation rules drop only tenant pool to tenant pool and tenant pool to the management ranges. The
  sandbox CIDR is in neither set, so nothing drops tenant to sandbox.
- The worker's NIC has no `firewall=1`, so no per-VM rule applies either.

So any tenant VM reaches a sandbox worker on every port: kubelet :10250, every NodePort, and Cilium's VXLAN UDP 8472.
A packet forged onto 8472 is decapsulated by Cilium as if it came from another node, with a source identity the sender
chooses, and can skip the sandbox ingress policy. The guardrail cannot defend against this; only a drop in front of the
worker can. The sandbox CIDR must be in the isolation DROP set in both directions, and the worker NIC needs
`firewall=1` with an inbound default of drop (SB2b).

**The guardrail is IPv4 only.** Every rule in it names IPv4 ranges (`0.0.0.0/0` minus the private ranges), so an IPv6
path out of a sandbox would match none of them. The sandbox cluster must run single-stack IPv4 (Cilium IPv6 off, no
IPv6 on the vnet). This is a precondition, not a default: SB2a has to check it, and adding IPv6 means writing the
IPv6 half of the guardrail first.

### 0.4 Exec goes through konnectivity

The tenant API server runs with `--egress-selector-config-file` and a `konnectivity-server` sidecar. With an iptables
`DROP` on the worker for the agent's port (8132) to the control plane address and the agent pod restarted, `kubectl
exec` and `kubectl logs` both failed with `No agent available` while `kubectl get pods` kept working. So exec, logs and
the startup probe path never need a route from the services cluster to the sandbox vnet, and Layer 1 can drop it.

Do not repeat that drill as written: the exec used to remove the rule went through the tunnel it cut. Make such a rule
remove itself (`(sleep 60; iptables -D …) &`), or plan on a VM reset, which is what restored this one.

The EE ServiceAccount and ClusterRole, renamed to `proxcloud-system/proxcloud`, could create and list Sandboxes and exec
into one, and were refused secrets. `kubectl auth can-i create pods/exec` answers `no` for it while
`--subresource=exec` answers `yes` and the exec works: read the second form.

### 0.5 What changed from EE

| EE | Here | Why |
|---|---|---|
| `cilium-node-config.yaml` (`bpf-lb-sock-hostns-only`) is a prerequisite | **not applied** | The chart runs Cilium without kube-proxy replacement, so socket LB (`bpf-lb-sock`) is off and nothing tries to translate inside gVisor's own network stack, which a socket hook cannot see into. Cilium still translates ClusterIPs per packet in BPF at the pod's veth, which every gVisor packet crosses, so cluster DNS and ClusterIPs worked from a gVisor pod without the file. It becomes required if the sandbox cluster ever turns on kube-proxy replacement, which brings socket LB back (see §0.6, Gateway). |
| `node-setup` init container copies envd and `sandbox-init` from a private registry image; pull secret created by hand | removed for the spike | The runtime image comes with SB5 (built in this repository's CI). Without it a sandbox is a plain pod, which is enough to test the runtime and the network. |
| runsc platform left at the default | `platform = "systrap"` written to `runsc.toml` | Make the choice explicit; systrap needs no `/dev/kvm` in the worker, which a nested or a cloud VM may not have. |
| NVIDIA toolkit and nvproxy branch | left out | The lab has no GPU (SB9). |
| Labels `…/tenant`, namespaces `…-system`, SA name | `proxcloud.io/tenant`, `sandbox-system`, `proxcloud-system/proxcloud` | Rebrand only. |
| Guardrail without a port 25 rule | second `egressDeny` entry: `0.0.0.0/0` TCP 25 | Plan D13. Verified: 25 dropped, 587 open. |
| Node image | the stock image-builder image already ships a `gvisor` runtime handler and `imports = ["/etc/containerd/conf.d/*.toml"]`, and no `runsc` | Same as EE's images, so `node-setup` works unchanged. No node image variant is needed for CPU nodes. |
| CAPMOX writes the bootstrap ISO to the shared ISO store | the cluster token sees only the node-local `local` dir store, so the ISO goes there | See §0.6. |

### 0.6 Open questions answered

**Can CAPMOX use a different ISO store per cluster, and does it detach the ISO after join?**
There is no field for it in CAPMOX v0.9.2 (`ProxmoxCluster`, `ProxmoxMachine`). Its injector calls go-proxmox v0.8.2
`VirtualMachine.CloudInit` without the `WithCloudInitStorage` option, which then takes the first enabled storage with
`iso` content in `GET /nodes/{node}/storage`. PVE lists only the storages the caller has rights on, so **the store is
chosen by the cluster token's ACL**: give the token `PVEDatastoreAdmin` on a node-local store with `iso` content and
nothing on the shared one. On the lab the ISO was uploaded to the node's `local` dir store and never touched the NAS.
Because a token's rights are its user's intersected with its own, `capmox@pve` needed the same role on that store; it was
added next to the user's inherited `ProxCloudCAPI` there so nothing else it had on that path was shadowed, and checked
with `pveum user token permissions` and `pveum user permissions`. If the token sees two `iso` stores, the pick is not
stable: PVE builds that listing in Perl hash order, which changes between calls, so the store alternates (PR #101). One
`iso` store per cluster token is the only reliable setting.
CAPMOX does detach and delete it: once the Machine is Available with a NodeRef it sets `ide0: none,media=cdrom` and
deletes `user-data-<vmid>.iso` (that delete needs `Datastore.Allocate`, which `PVEDatastoreAdmin` has). Measured: the
ISO existed for 111 s. This is the grant SB-pre (PR #101) gives every cluster token, exercised here by CAPMOX itself
through a real join.

**What is the CABPK bootstrap token's TTL?** 15 minutes (`--bootstrap-token-ttl=15m` on CABPK here, which is also its
default). The token Secret in the tenant's `kube-system` was created when the Machine was, expired 15 min later and was
then removed. The node joined about 3 min in, so the token stayed valid for about 12 min after join. Two places still hold
it: the ISO (gone after 111 s) and `/var/lib/cloud/instance/user-data.txt` on the node's own disk, which stays. Code that
escapes gVisor in those first minutes can read a valid join token and join a fake node. Keep tenant pods off a new node
until the token has expired, which the plan's `proxcloud.io/unsecured` taint can cover by being removed no earlier than
the TTL.

**Does NodeRestriction stop a node from changing its own labels and taints?** Only if it is on, and **on Kamaji tenant
control planes it is off**: Kamaji's default `admissionControllers` list (copied to every `TenantControlPlane`) has no
`NodeRestriction`, and the chart does not set it. With the node's own kubelet credential, on the chart as it is:

| Change to its own Node | NodeRestriction off (chart today) | NodeRestriction on |
|---|---|---|
| plain label, e.g. `proxcloud.io/tenant=b` | allowed | allowed |
| `node-restriction.kubernetes.io/…` label | **allowed** | refused: `is not allowed to modify labels` |
| `node-role.kubernetes.io/control-plane` label | **allowed** | refused |
| add a taint | **allowed** | refused: `is not allowed to modify taints` |
| remove a taint an admin set | **allowed** | refused |

It was turned on for the spike cluster by setting `KamajiControlPlane.spec.admissionControllers` to Kamaji's list plus
`NodeRestriction`; the API server rolled in about a minute and the table's right column was then measured.

Labels and taints are the small part. The Node authorizer lets any node credential write Node objects and create pods;
NodeRestriction is what limits that to the node's own object and to mirror pods without secrets. Without it, a node's
own kubelet credential can:

- change or delete **other** nodes (their labels, taints, status), and
- create a pod bound to itself that mounts any Secret or uses any service account in any namespace. The Node authorizer
  grants a node read access to the secrets of pods bound to it, so the kubelet can then read that Secret.

So one compromised worker reads every Secret in the cluster, one pod at a time. In a shared sandbox cluster those
secrets belong to other tenants and to the platform (`proxcloud-system`), so the step crosses tenants. **NodeRestriction
is a release gate for the sandbox cluster**, and it affects every managed Kubernetes cluster as well: the chart change
is small and needs its own PR. Any placement decision (dedicated nodes, `sandbox-system` nodes) must use labels under
`node-restriction.kubernetes.io/`, the only prefix a kubelet cannot set on itself once NodeRestriction is on; a plain
label like `proxcloud.io/tenant` can be claimed by the node it is meant to keep tenants off.

**Does Cilium's Gateway API work with the chart's Cilium config?** No. With `gatewayAPI.enabled: true` alone the
operator logs `Gateway API support requires either kube-proxy-replacement or enable-node-port enabled` and the
`GatewayClass` stays `Unknown`. Adding `nodePort.enabled: true` gets the class Accepted and a `Gateway` Programmed with a
MetalLB address (the L2 announcement reached the EVPN fabric), but kube-proxy installs a `REJECT` for the Gateway's
endpoint-less Service, so every connection is refused, from the node and from the exit node. A Cilium Gateway on this
cluster needs kube-proxy replacement, which means dropping Kamaji's kube-proxy addon, and then socket LB, which brings
back EE's `bpf-lb-sock-hostns-only` for gVisor. gVisor cluster DNS still worked with `nodePort` on. Gateway API CRDs
v1.3.0 were installed for the test; the Cilium values went back to the chart's afterwards.

**D15, as recorded in the sandbox plan after this spike:** TLS ends inside `sandbox-proxy` itself, a Go TLS listener
serving the wildcard certificate from a Secret and reloading it when the Secret changes. cert-manager on the services
cluster issues the certificate and syncs the Secret into the sandbox cluster. No Gateway, no ingress controller and no
separate terminator Deployment, and the chart's Cilium stays as it is. The plan's author chose this; the owner has not
reviewed it yet.

Not tested here: Zot reloading `accessControl`, nvproxy on Pascal.

### 0.7 Other things the spike showed

- A sandbox that does not fit stays `Ready=False` with reason `DependenciesNotReady` and a `Pending` pod (`Insufficient
  cpu`), indefinitely. EE's create waits 50 s and answers 503; the capacity check before create (EE `capacity.go`) is
  what turns that into a clear answer, so it ports with SB1.
- System pods take 1.3 vCPU of requests on a 2 vCPU worker (Cilium, CoreDNS ×2, konnectivity, MetalLB, agent-sandbox,
  node-setup), so one 1 vCPU sandbox filled it. Size sandbox workers well above 2 vCPU, or keep system pods on the
  `sandbox-system` group.
- `node-setup` survived a hard reset of the worker: runsc and the containerd drop-in persist, and the DaemonSet rerun is
  a no-op.
- The tenant control plane's address comes from the services cluster's MetalLB pool, like any tenant cluster; the
  sandbox vnet needs a route to it (here, the EVPN exit node and SNAT).

### 0.8 Consequences for the next phases

Release gates (no tenant code on the sandbox cluster until all hold):

1. **SB2b, in both directions.** Without a VM-level firewall on the workers, a node-level foothold reaches the PVE API,
   the NAS (as a node address, through the exit node's SNAT) and the management host, and every tenant VM reaches the
   workers on any port, VXLAN included (§0.3). The sandbox CIDR goes into the isolation DROP set both ways, and the
   worker NIC gets `firewall=1` with inbound drop by default.
2. **NodeRestriction on the sandbox control plane**, with placement on `node-restriction.kubernetes.io/` labels only
   (§0.6). Add it to the chart's `KamajiControlPlane` for every cluster, as its own fix.
3. **Single-stack IPv4** on the sandbox cluster and vnet while the guardrail names only IPv4 ranges (§0.3).

Other consequences:

4. **SB-pre** (PR #101) mints cluster tokens on the node-local ISO store; §0.6 is a live CAPMOX run of that grant. The
   sandbox cluster's token gets the same, with exactly one `iso` store visible.
5. **D15**: TLS ends in `sandbox-proxy` (Go listener, wildcard Secret from cert-manager on the services cluster), not in
   a Cilium Gateway or a separate terminator (§0.6).
6. **D10 and pricing**: smallest sandbox 1 vCPU under systrap.
7. SB2a does not need EE's `cilium-node-config.yaml` on this chart, as long as kube-proxy replacement stays off (§0.5).
