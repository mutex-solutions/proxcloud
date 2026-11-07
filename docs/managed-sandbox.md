# Managed sandboxes (gVisor) — design notes and runbook

Sandboxes are short-lived containers for untrusted code (agents, code interpreters), run under gVisor (`runsc`) on a
Kubernetes cluster that belongs to the platform, with an E2B-compatible API in front. The feature is being ported from
an earlier implementation by the same owner (called **EE** below). It is not in the product yet: this file starts with
the lab spike (phase SB0) and grows into the operator runbook as the phases land.

> ## Status
>
> **Reachable by tenants through the native API, behind release gates (SB3).** SB1 ported the sandbox service; SB2a
> added the cluster (`deploy/sandbox-cluster/`, installer stages 10 and 55, isolation drops, section 1); SB2b layer 1
> (the VM firewall on every worker NIC, the `unsecured` taint, the sandbox's own egress address, section 2). SB3
> (section 3) mounts the routes, bills sandboxes per minute, adds the project quota and the suspension gate, deletes
> sandboxes on a suspension and on project deletion, and lets an operator create a sandbox provider, which goes live
> only while its three release gates hold. SB4 (section 4) publishes sandbox ports at
> `https://<port>-<id>.<domain>`: a platform HAProxy appliance passes TCP 443 to `sandbox-proxy`, where TLS ends with
> a wildcard certificate issued on the services cluster. SB5 (section 5) adds the E2B-compatible API at `/e2b` with
> project-scoped sandbox API keys, envd in every sandbox and the built-in templates `base` and `code-interpreter-v1`;
> what of E2B does not work is in [e2b-compat.md](e2b-compat.md). Not yet: template builds, snapshots and pause
> (SB6-SB7, so the wake endpoint has nothing to resume yet), MCP gateway (SB8), GPUs (SB9). Section 0 records the hand-built lab spike. Phase order
> and decisions: the sandbox plan (owner document, not in this repository).

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

## 1. The sandbox cluster (SB2a)

### 1.1 What builds it

| Piece | Where | What |
|---|---|---|
| Token and pool | installer stage 10, `sandbox.enabled` | pool `proxcloud-sandbox`; token `capmox@pve!sandbox` with the grants of a tenant cluster's token (token rows only, so `capmox@pve` keeps what it inherits), then a PVE check that it sees one `iso` store, the node-local one |
| Vnet | stage 55 | `sandbox.network.vnet` in an existing EVPN zone, subnet with SNAT and no DHCP, alias `proxcloud-sandbox` (no `st:` alias, so no project owns it) |
| Cluster | stage 55, `templates/sandbox-cluster.yml.j2` | an ArgoCD Application `pc-sandbox` of `proxmox-kamaji-cluster` at this checkout's chart version (0.1.8), namespace `pc-sandbox`, no project label: no portal sync lists it. No CSI, no StorageClass, no NAS dataset. Node group `sandbox` for sandboxes; with a sandbox domain also `sandbox-system` for sandbox-proxy (§4.5) |
| Release gate: NodeRestriction | stage 55 | the chart's `admissionControllers` include it; stage 55 reads the `KamajiControlPlane` and stops if it is missing |
| Runtime, guardrail, identity | `deploy/sandbox-cluster/` (README has the order) | `cilium-node-config.yaml`, `node-setup.yaml` (runsc 20261102.0, systrap), `runtimeclass.yaml`, agent-sandbox v1.0.9 vendored, `network-policy.yaml` (private ranges and TCP 25 denied), `rbac.yaml` (`proxcloud-system/proxcloud`, no Secret access) |
| Release gate: IPv4 only | `node-setup.yaml`, `check-ipv4-only.sh`, stage 55 | node-setup refuses a host with a global IPv6 address or an IPv6 default route; the check wants Cilium `enable-ipv6=false`, no IPv6 node address or pod CIDR, and node-setup Ready everywhere. Stage 55 stops before agent-sandbox and the guardrail when it fails |
| Layer 0: isolation drops | `internal/cloud/pve/network.go`, provider `network.sandboxCidrs` (stage 60 writes it) | section 1.2 |

Not in SB2a: the per-NIC VM firewall with `ipfilter` and the `unsecured` taint (SB2b), a dedicated SNAT address
(SB2b), the `sandbox-system` node group, `sandbox-proxy` and TLS (SB4, §4.5), the envd runtime image (SB5).

### 1.2 Layer 0: the sandbox network in the isolation rules

The provider's `network.sandboxCidrs` adds the sandbox subnet to the cluster firewall isolation set in both
directions ([C4.1](architecture/c4/c4-code.md#c41-tenant-network-isolation)): tenant pool to the sandbox, and sandbox
to the tenant pool, the node management networks and `blockedCidrs`. These rules see routed traffic, which is what
both paths of §0.3 are: a tenant VM reaching a worker through the shared VRF, and a worker reaching the NAS or a node
through the exit node's SNAT. They do not see traffic switched inside the sandbox vnet (HAProxy or node address
spoofing there is SB2b's `ipfilter`). An entry that overlaps the tenant pool or a blocked network (the management
networks included) is refused. The services network stays open to the workers here, because tenants need it; until
SB2b only the guardrail keeps a sandbox pod off it, so stage 55 refuses a services network outside the ranges the
guardrail denies.

Measured on the lab (2026-11-07) with the SB0 cluster's worker and a tenant VM of a portal stack on this branch,
packets in both directions, the provider without `sandboxCidrs`, with it, without it again, then with it:

| From → to | Without | With |
|---|---|---|
| Tenant VM → worker kubelet :10250, ssh :22 | open | dropped |
| Tenant VM → worker UDP 8472 (Cilium VXLAN), counted on the worker's tap | 5 of 5 arrive | 0 of 5 |
| Worker node namespace → NAS NFS :2049, TrueNAS API :443 | open | dropped |
| Worker node namespace → PVE API on a node that is not the exit node, ssh on another node | open | dropped |
| Worker node namespace → PVE API on the exit node | refused (RST) | dropped |
| Worker node namespace → management host ssh, object store | open | dropped |
| Worker node namespace → the tenant VM :22 | open | dropped |
| Worker node namespace → its own tenant control plane :6443, internet :443 | open | open |

"Dropped" is a 4 s timeout. The worker namespace probe is a privileged `hostNetwork` pod running `nsenter -t 1 -n`,
standing in for code that escaped gVisor. The cluster stayed healthy with the drops in place: node Ready, exec and logs
through konnectivity, cluster DNS and HTTPS from a gVisor sandbox. Each switch took effect within 20 s of a sync.

What layer 0 does not cover: the services cluster's subnet (the control plane endpoint lives there, so it stays
reachable, as it is for tenants), anything inside the sandbox vnet, and traffic to the internet.

### 1.3 Applied to the SB0 cluster

Stage 55 would add an Application to the lab's shared services cluster, so it did not run on the lab; its rendered
Application and the chart output were read (no CSI, no NFS objects, NodeRestriction in `admissionControllers`). The
manifests went to the SB0 cluster in the README's order: everything but `cilium-node-config.yaml` and the IPv6 refusal
in `node-setup.yaml` was already there unchanged. Results:

- `CiliumNodeConfig` merged into the agent's config (`bpf-lb-sock-hostns-only: true` in its config directory), but
  with socket LB off the agent reports `BPFSocketLBHostnsOnly: false`: a no-op on this chart, as §0.5 expected.
- The guardrail after the Cilium restart, from a gVisor sandbox: every private and link-local target dropped,
  `kubernetes` ClusterIP dropped, internet and cluster DNS open, TCP 25 dropped, 587 open.
- IPv6 check: passes on the worker; with a ULA address added on a dummy interface of the worker, node-setup refused the
  node (CrashLoopBackOff, the address in its log) and the check failed; with it removed, both passed again.

## 2. Layer 1: the VM firewall on the workers (SB2b)

Layer 0 (§1.2) sees routed traffic only. Layer 1 sees every frame a worker sends or receives, frames switched inside
the sandbox vnet included, and pins the worker's source address. The portal applies it, not the installer: the sync
job's pass over each Proxmox provider (`syncjob.reconcileSandboxWorkers`, `pve.StartSandboxPass`) fences every VM of
the provider's sandbox pool on every cycle, so a new worker, a replaced one and a hand edit all come back to it.

### 2.1 What a worker may do

Options on each worker: `enable`, `ipfilter`, `macfilter` on; `policy_in` and `policy_out` `DROP`; `dhcp`, `ndp`,
`radv` off. Every NIC gets `firewall=1`. The rules live in one cluster security group per generation, `sbx<pool
hash><content hash>`, which is the VM's only rule. Every rule in it ACCEPTs:

| Direction | What | Where |
|---|---|---|
| out | TCP to the API port and 8132 (konnectivity) | the sandbox control plane's address (the provider kubeconfig's server) |
| out | UDP and TCP 53 | `sandbox.dns` only |
| out, in | UDP 8472 (Cilium VXLAN), TCP 4240 and ICMP (Cilium health), TCP and UDP 7946 (MetalLB), TCP 10250 | the other sandbox workers (IP set `<group>-n`) |
| in | TCP 10250 | `sandbox.kubeletSources` (the services cluster's nodes) |
| in | TCP 443 (`config.sandbox.proxy.port`) | the entry appliance's vnet address only, to `sandbox-proxy`'s MetalLB address (`config.sandbox.proxy`, SB4) |
| out | anything | IP set `<group>-o`: `0.0.0.0/1` and `128.0.0.0/1` minus the private, CGNAT, link-local, multicast and reserved ranges, the management networks, every address a PVE node holds on any interface (uplinks, bonds and VLANs too), the sandbox's own SNAT address, `blockedCidrs`, the tenant pool, the sandbox subnet itself (its gateway is a PVE node) and the control plane address |

Nothing else comes in: there is no rule for a tenant, for the services cluster beyond the kubelet, or for anyone on the
internet, except the entry appliance, which may reach `sandbox-proxy`'s address on 443 (SB4, §4.3).

- **Why the kubelet between workers.** Kamaji starts the konnectivity server with the default proxy strategy, so
  exec and logs for a pod on one worker can leave through the agent on another. The kubelet still wants an API server
  credential.
- **Why all ACCEPT.** The policies drop, so no rule order matters and no partial state opens anything: a missing rule
  drops more. A changed rule set is a new generation. Its sets and group are built while nothing references them, each
  worker then gets the new reference before the old one is removed (for that moment it allows the union of two
  complete sets), and the old generation is deleted once no worker holds it. A generation that a worker uses and that
  differs from what the portal would build is not rewritten in place: the pass fails with `ErrSandboxDrift` and clears
  no worker for tenants. A PVE node that does not report its networks also stops the pass, so a new generation never
  denies less than the one the workers hold.
- **Why node addresses and the SNAT address.** The management networks are the bridges with a gateway; a node's PVE
  API and ssh also listen on its other addresses, and the SNAT address sits on a dummy interface PVE does not list. On
  a site where those are public, the internet set would have let a worker reach them.
- **Address pinning.** `ipfilter-net0` admits the worker's address and the MetalLB range (any worker may answer for a
  VIP). Before the worker's Node exists, the filter admits the whole worker range. The Node's `InternalIP` is the
  kubelet's to write, so it is trusted only while the worker still has its unsecured taint, and then pinned: an address
  the set already holds is never replaced by a later claim, and once the taint is off the Node is not read for it at
  all. The set counts as pinned only when it holds a single worker-range entry and that is a host: the range itself can
  end in a host (`.10-.20` is `.10/31 .12/30 .16/30 .20`). A NIC off the sandbox vnet gets an empty filter, which drops every
  IPv4 frame it sends. `macfilter` keeps the NIC's own MAC.
- **IPv6.** No IPv6 rule: the policies drop it.
- **Spoofed ICMP errors.** PVE's nftables bridge chain accepts ICMP and ICMPv6 error types (destination unreachable,
  source quench, time exceeded) in `vm-out`/`vm-in` before the per-NIC checks, so `ipfilter` and the DROP policy never
  see them (lab, PVE 9.2). A worker could send them with any source to PVE host input and the other workers (routed ones
  only survive a tracked flow). Its `pre-vm-out` also accepts conntrack established/related before `ipfilter`, so a
  forged packet that matches a flow tracked on the node's bridge passes too. The host guard (§2.6) closes both: a host
  nftables chain on every node, ahead of PVE's, drops any IPv4 frame leaving a sandbox worker's tap whose source is
  outside its `ipfilter`.

### 2.2 The unsecured taint

The node group gives every worker `proxcloud.io/unsecured=true:NoSchedule` at join (`templates/sandbox-cluster.yml.j2`),
and NodeRestriction keeps the kubelet from removing it. The pass removes it only when all of these hold: the firewall
was read back in place in this pass, the filter is pinned to exactly the address the Node reports, and the Node is at
least `unsecuredMinAgeMinutes` old (default 20: CABPK's bootstrap token TTL is 15 minutes, and a token it refreshed
just before the Node registered lives a little longer; by then the join token on the worker's own disk is dead, §0.6).
A Node the pass cannot match to a VM of the pool keeps the taint. With the datacenter firewall off PVE enforces no VM
rule at all, so the pass still writes every worker's firewall but reads none back as in place
(`ErrSandboxUnenforced`).

The taint also comes back. A worker already cleared that a later pass fails on, finds not in place (the datacenter
firewall off included), or whose filter no longer admits exactly its Node's address (the pin lost, or the Node now
naming another address) gets `proxcloud.io/unsecured:NoSchedule` again, and the next pass treats it as new. The effect
is `NoSchedule`: pods already there keep running. DaemonSets that tolerate every
taint (Cilium, `node-setup`) run before; CoreDNS, the agent-sandbox controller, konnectivity's agent and MetalLB's
speaker schedule after, which is why stage 55 no longer waits for the controller.

### 2.3 Its own egress address (D14)

`sandbox.network.snat` is required with the sandbox on. Stage 55 puts it on the zone's exit node (one exit node, or
`exitnodes-primary`; root ssh to it through the inventory group `pve`) as an ifupdown2 dummy interface in
`/etc/network/interfaces.d/proxcloud-sandbox-snat`, whose `post-up` inserts `SNAT -s <subnet> -o <uplink>
--to-source <address>` above PVE's own rules, and whose `pre-up` (so the address is never up without it) drops every
new connection to the address in `INPUT`: otherwise the exit node's PVE API and ssh would answer on it, from the
internet and from the sandbox. Replies to the SNAT are not new connections. It then turns the subnet's PVE `snat` off and deletes the rules PVE left
behind. With the interface down the subnet's traffic leaves with its private source, which an uplink drops: it never
falls back to the node's address. `snat: node` keeps PVE's SNAT to the exit node's own address, for a site with one
public address that accepts sharing it with the nodes and the portal.

Measured on PVE 9.2: every SDN apply runs `ifreload`, which runs `post-up` again without `post-down`, so the rule is
added with `-C ... || -I`. PVE did not remove its own SNAT rules when `snat` went off, and every earlier apply had added
another copy (24 copies of the node-address rule for the sandbox subnet on the lab).

### 2.4 Permissions

The portal token keeps its inherited roles everywhere except the sandbox pool, where stage 10 gives `proxcloud@pve`
and `proxcloud@pve!platform` the role `ProxCloudSandboxGuard` (`Pool.Audit VM.Audit VM.Config.Network`). An ACL on a
path replaces what the parent passes down, so on the pool and its VMs the token loses `ProxCloudPlatform` and
`ProxCloudKubeMint` from `/pool`: no guest agent (`VM.GuestAgent.Unrestricted`), console, disk, power, snapshot, backup
or ACL rights on a VM that runs tenant code. The CSI tokens and the services cluster's own CAPMOX token get nothing
there: `csi@pve` as a user row (its tokens are privilege-separated, so the row caps them), and `capmox@pve!capi` as a
token row — the sandbox cluster runs through `capmox@pve!sandbox`, so the services CAPMOX token never manages the pool,
and through `/pool` it would otherwise inherit `ProxCloudCAPI` (clone, config, power, migrate) on the workers. It is a
token row, not a user row, because `capmox@pve` owns the sandbox token and a user `NoAccess` would cap that too. Stage
10 reads each token's effective rights from PVE — on the pool and on every worker VM, where a stray per-VM ACL or a
group grant would hand back rights the pool-level read misses — and stops unless the portal holds the guard role alone
and the CSI and services-CAPMOX tokens hold nothing. The groups and IP sets are cluster firewall objects, written with
the `Sys.Modify` the token holds on `/`.

The pass reads the sandbox cluster with the `proxcloud-system/proxcloud-firewall` service account
(`deploy/sandbox-cluster/rbac.yaml`: nodes get, list, patch). Stage 55 turns its token into
`generated/sandbox-firewall.kubeconfig`, and stage 60 stores it as the provider's `secret.sandboxKubeconfig`, next to
`config.sandbox` (`pool`, `nodes`, `loadBalancers`, `dns`, `kubeletSources`, `snat`, `unsecuredMinAgeMinutes`).

### 2.5 Measured on the lab (2026-11-07)

The SB0 cluster, with the pass run from the management host every 20 s with the portal's own PVE token (`go test
-tags sandboxlive ./internal/cloud/syncjob/ -run TestSandboxFenceLive`) and the firewall service account. No portal
stack: the provider config was a file. Probes as in §1.2 (TCP connect, 4 s timeout, "dropped" is a timeout), with no
layer 0 rule for the sandbox subnet in place, so the right column is layer 1 alone.

Permissions, `pveum user token permissions proxcloud@pve platform`: on `/pool/proxcloud-sandbox` and `/vms/<worker>`,
35 privileges before and `Pool.Audit VM.Audit VM.Config.Network` after; unchanged (35, or 33 on `/`) on a project pool
and its VMs, the sandbox vnet, the boot store and `/`. `csi@pve!gateway` and `!csi`: nothing on the worker after,
unchanged on a project pool.

| From → to | Before | After |
|---|---|---|
| Worker node namespace → NAS NFS, TrueNAS API | open | dropped |
| → PVE API on its own node, ssh on another node; PVE API on the exit node | open; refused | dropped |
| → the vnet gateway's PVE API and ssh (a PVE node's own address) | refused | dropped |
| → management host ssh, k3s API, PowerDNS API | open | dropped |
| → CSI gateways on the management host (two portals) | refused | dropped |
| → RustFS, RGW, the outer host | open | dropped |
| → DBaaS load balancers :5432 (two), another tenant cluster's control plane | open | dropped |
| → services cluster API, a services node's kubelet; the PVE API forwarder | timeout; refused | dropped |
| → a tenant VM's public and project addresses, 169.254.169.254 | timeout | dropped |
| → its own control plane :6443 and :8132 | open | open |
| → internet :443, DNS, HTTPS GET | open, ok, 200 | open, ok, 200 |
| Tenant VM → worker kubelet, ssh; a NodePort | open; refused | dropped |
| Tenant VM → worker UDP 8472, counted on the worker's tap | 5 of 5 | 0 of 5 |

No tenant load balancer appliance existed on the lab during the run: the tenant VM's public and project addresses
stood in for one (an appliance is a tenant VM with a public NIC).

Spoofing, raw UDP to 8472 from worker 1's namespace to worker 2, counted on worker 2's tap: with its own source
address 5 of 5; with another worker's address 0 of 5 (5 of 5 with `ipfilter` turned off by hand, which the pass turned
back on 8 s later); with the vnet gateway, a tenant address or a MetalLB address 0 of 5 (the last two are also refused
by worker 2's inbound rules).

Second worker: a node group with the taint added to the running cluster, the pass looping. From the PVE task log:
CAPMOX's clone and config at 16:00:50, the portal's `firewall=1` on its NIC at 16:01:04, CAPMOX's start at 16:01:06, so
the VM never ran without its firewall. Its Node registered at 16:01:54 through it, the filter was pinned to its address
at 16:02:08, and Cilium health reported 2/2 nodes and endpoints reachable over VXLAN. The pass took the taint off at
16:17:15 (Node 15 min 21 s old); konnectivity's agent and MetalLB's speaker started there, and a gVisor pod pinned to
it resolved names through CoreDNS on the first worker (VXLAN), fetched HTTPS (200), was refused the NAS, its control
plane and the first worker's kubelet by the guardrail, and answered 6 of 6 execs with two konnectivity agents up.

Generation switch: adding the kubelet rule made a new group; both workers moved to it in one pass, the old group and
its sets were deleted, and the cluster stayed Ready with exec working.

Egress (D14), SYNs from the worker to 1.1.1.1:443 seen on the uplink: before, source = the exit node's address; after
the interface file on the exit node and `snat` off on the subnet, source = the dedicated address, HTTPS 200.

Review fixes, the same pass with the fixed build (2026-11-07, ~16:50 UTC): from the management host the SNAT address
answered PVE API and ssh (open) before the `INPUT` guard and dropped both after, with the exit node's own address still
open and the worker's HTTPS still 200; `ifreload -a` left one copy of the guard and of the SNAT rule. A pass with the
cluster as it was changed nothing. Worker 2's pinned address deleted from its filter by hand: the next pass logged it
no longer fenced, put the taint back and the worker range in the filter; the pass after pinned its own address again
and took the taint off. Datacenter firewall turned off: one pass tainted both workers (`ErrSandboxUnenforced`); turned
back on, the next pass cleared both, nodes Ready, HTTPS 200.

Not run here: stages 55 and 60 (they would add an Application to the lab's shared services cluster). The exit node file
and the subnet change were made by hand with the content stage 55 writes, and the provider config came from a file.

## 2.6 Layer 1b: the host anti-spoof guard

Layer 1's `ipfilter` pins the source of every frame a worker sends — except ICMP errors and conntrack hits. PVE's nftables bridge firewall
(`table bridge proxmox-firewall-guests`) has a chain `allow-icmp` that its `vm-out` (prerouting) and `vm-in`
(postrouting) base chains `jump` *before* `iifname vmap @vm-map-out` reaches the per-guest `ipfilter` and `macfilter`
drops. `allow-icmp` accepts the ICMP error types unconditionally (v4 destination-unreachable, source-quench,
time-exceeded; v6 destination-unreachable, packet-too-big, time-exceeded, parameter-problem). So a worker can send an
ICMP error with any forged source and neither `ipfilter` nor the `DROP` policy sees it. On the lab a worker reached PVE
host input on the vnet gateway with the gateway's own address, another worker's address, a management address and an
arbitrary external address as source; routed ones to another worker only survived a tracked flow, but the host and node
addresses were reachable off-path. The same chain's `pre-vm-out` also accepts `ct state established,related` before
`ipfilter`, so a forged-source packet of any protocol that matches a flow already tracked on the node's bridge (for
example one between two other workers on that node) passes untouched. (Upstream Proxmox behaviour, PVE 9.2; a bug
report is drafted in PR #116, not filed.)

The guard closes it on the host, since the VM firewall cannot. On every node that can host a sandbox worker (the pool
spans the cluster, so every `pve` node) the installer places `/usr/local/sbin/sandbox-icmp-guard.py` and a systemd
service plus a one-minute timer (`proxcloud-sandbox-icmp-guard`). The script reads, from PVE's own cluster config
(`/etc/pve`), every sandbox-pool worker's NICs on the sandbox vnet (from `/etc/pve/nodes/*/qemu-server`, whichever node
holds the config) and that NIC's `ipfilter-net<N>` set — the worker's pinned source addresses, kept correct by the
portal's sync — and installs a host nftables chain. Every node pins every worker: a rule matches the tap's name, so it
is inert where the tap is absent and live the moment a worker migrates or is HA-restarted onto the node.

```
table bridge proxcloud-sandbox {
  chain antispoof {
    type filter hook prerouting priority -300; policy accept;   # before PVE's vm-out (priority 0)
    iifname { <sandbox taps> } icmpv6 type { … } counter drop    # IPv4-only cluster: any v6 error is spoofed
    iifname "<tap>" ip saddr { <ipfilter nomatch entries> } counter drop      # only if the set has `!` entries
    iifname "<tap>" ip saddr != { <this worker's ipfilter> } counter drop
  }
}
```

Because the chain is a base chain at an earlier priority with `policy accept`, a frame with a pinned source continues
to PVE's chain — an `accept` verdict there is not terminal for the hook. The guard mirrors `ipfilter` (a worker's own
address plus the MetalLB range, which any worker may answer for; `!` nomatch entries excluded), so it drops nothing
PVE's anti-spoof would allow; it only stops PVE's early accepts from skipping that check. An entry that is not an IPv4
address or CIDR (an alias; the portal never writes one) is left out and reported on stderr, which drops more, never
less, and cannot make the generated ruleset invalid. A NIC with no `ipfilter` set yet (the sync has not fenced the
worker) gets what PVE gives it, no pin, except that every ICMP error from it is dropped; an empty set drops every IPv4
frame, as PVE does. The counters reset every run (the table is replaced atomically each minute). The timer re-runs the script
every minute; a worker that just joined, moved node or had its firewall rebuilt is pinned well within its 20-minute
unsecured taint, so it never runs tenant code before the guard covers it. It is the installer's and not the sync's
because the portal's PVE token reaches the API only, never a node shell.

Measured on the lab (2026-11-08), raw ICMP errors (and raw ICMPv6) from a worker's node namespace to the vnet gateway
(PVE host input, a node address) and to another worker, five per type and source, counted on the receiving tap:

| Source on the forged error | To PVE host input, before the guard | After the guard |
|---|---|---|
| the worker's own address | arrives | arrives |
| a MetalLB address in its `ipfilter` | arrives | arrives |
| another worker's address | arrives | dropped |
| the vnet gateway | arrives | dropped |
| a management address | arrives | dropped |
| an arbitrary external address | arrives | dropped |
| any ICMPv6 error | — | dropped |

The cluster stayed healthy with the guard in place: both workers Ready, the `keep` gVisor sandbox Running, worker-to-
worker ping (ICMP echo, not an error type) 0% loss.

With the all-source pin (review follow-up, same day): the ICMP drill above gave the same result, and 80 forged UDP and
TCP packets (sources another worker, the gateway, a management address, an external address) from worker 1 to the
gateway all hit the guard's drop counter on its tap. The established-flow bypass itself needs two workers on one node,
which the lab (one per node) does not have; the guard drops those frames by source before PVE's chain runs, whatever
their conntrack state. The script, env file, service and timer are installed on all three nodes (pve1 included, which
hosts no worker today), and the generated ruleset passes `nft -c` with nomatch and alias entries.

Not run here: the installer stage (it would touch the lab's shared services cluster); its guard tasks were applied by
hand with the files and content stage 55 writes. Stage 10's per-VM checks see worker VMs only on a re-run after stage
55 built the cluster.

## 3. Tenant API, billing, quota, suspension and release gates (SB3)

### 3.1 The native API

On the project cloud routes (`/api/v1/project/{id}/...`), for a project attached to a sandbox provider:

| Call | What |
|---|---|
| `POST /cloud` `{type: SANDBOX, data: {template, name?, cpu?, memoryGiB?, timeoutSeconds?, gpu?, env?, metadata?, network?}}` | Create. A create that names no service goes to the project's first enabled sandbox provider. 200 with the row; the sandbox is `PENDING` until its pod is Ready |
| `POST /resource?type=SANDBOX` | List, read through the cluster (a sandbox goes `RUNNING` in seconds and ends at its time limit) |
| `DELETE /cloud/{id}` | Delete the Sandbox CR (its pod goes with it); the row ends now (`DELETED` with its end) and the next read archives it |
| `POST /cloud/{id}/action` `EXEC {cmd \| command[], stdin?, timeoutSeconds?}` | Run one command and wait: `{stdout, stderr, exitCode, truncated}` |
| `GET_LOGS {tailLines?}` | The newest lines of its output (default 200, at most 2000) |
| `GET_ACCESS` | How to reach its ports (SB4): `{domain, host: "{port}-<id>.<domain>", trafficAccessToken?}`, read from the live sandbox; 409 when the provider publishes no ports (`config.sandbox.domain` unset) |
| `SET_TIMEOUT {timeoutSeconds}` | The end moves to now plus that (keepalive or shorten), within the provider's maximum |
| `REFRESH` | The live state |

Errors: 409 for a DISABLED or deleting project, 403 for a SUSPENDED billing profile, 503 while the provider is
`DISABLED` (what runs may finish: logs, delete), while a release gate does not hold or the cluster is unreachable, 429 over the project quota or the cluster ResourceQuota, 400 for a bad request,
404 for a sandbox of another project (every action and delete needs a row the project owns, and the service checks
the sandbox's project labels again).

### 3.2 Billing

`sandbox` is billed per minute from the moment the sandbox was Ready (`runningSince`, never the create: an image pull
is not the customer's) to its end, rounded up to whole minutes, at least one. The engine bills every clock minute an
item spans, so the billed window starts at the minute `runningSince` falls in and is that many minutes long: from
`runningSince` itself, a 39 s sandbox started at :07 would span two clock minutes. Attributes: `vcpus`, `ram_gb`,
`gpu_count`, `gpu_model`, `template`, `status`, `display_name`. The start is recorded by any read that sees the pod
Ready: the minutely `sandboxStateSync` (at :10, before the charge at :30), the client list, an `EXEC`, a delete. An
ended row is `DELETED` with its end and archived; the charge rates archived rows of the cycle to their end. A
sandbox can start and end between two reads: a row that ends without ever being read Ready is billed from the last
read that saw it pending (`seenPendingAt`, else its create, marked `runningSinceAssumed`), so a short sandbox is
never free and one that never started pays at most about a minute. `sandbox_snapshot` is per stored GB-hour (`size_gb`); nothing writes one before SB7. The seed's rules for both
are named `PLACEHOLDER ...` until a rate is signed. The worker VMs sit in the platform's pool and bill nobody.

### 3.3 Quota (D10)

Project quota keys `sandboxes`, `sandboxVcpus`, `sandboxRamMb`, `sandboxGpus`, on the project's quota document (admin
Projects → Quota). Each absent key falls back to the provider's `config.sandbox.projectQuota`, then to 5 sandboxes,
16 vCPU, 32768 MB, 0 GPU: never to unlimited. A create is checked against the sandboxes the cluster runs for the
project (not the cache), and the namespace ResourceQuota carries the same numbers as the backstop for two creates at
once. The provider's per-sandbox limits (`config.sandbox.limits`: 8 vCPU, 32 GiB, 1 GPU by default) apply first.

### 3.4 Suspension (D9)

Create, `EXEC` and `SET_TIMEOUT` pass `refuseNewUsage` (`cloudAction`'s allow-list from #84 refuses `EXEC`,
`SET_TIMEOUT` and `REFRESH` for a DISABLED project) and then the billing profile check, which covers the moment between a
profile's suspension and the walk that disables its projects. List, `GET_LOGS` and delete stay open. On a suspension
(and an operator's disable through the same walk) `billingCloudSuspender` deletes every running sandbox of the
project and ends every row (also one whose sandbox was already gone, since a DISABLED project is not synced);
snapshots are kept (none exist before SB7); a resume brings none back. An operator's disable deletes them once, with
no retry pass. A create already past the gate when the profile is suspended can survive until the next dunning pass;
it is billed. A provider set `DISABLED` stops new usage (create, `EXEC`, `SET_TIMEOUT`) but deletes nothing.

### 3.5 Project deletion

Teardown handles sandboxes first: every row's sandbox is deleted, the row ended and archived, then the project's
`sb-<project>` namespace is deleted. Each sandbox is read before it goes, so its archived row carries its start. A
failure on either keeps the project `DELETE_IN_PROGRESS`, so the deletion job retries. A deleting project is not
charged, so the time between the delete request and the teardown is not billed.

### 3.6 Release gates: how the portal checks each

A sandbox provider names the Proxmox provider of its workers (`config.sandbox.proxmoxServiceId`): that provider's
sync fences them (section 2), and the gates read through its PVE token and its sandbox kubeconfig (the
`proxcloud-firewall` service account). Both kubeconfigs must name the same API server. The provider can be stored
`DISABLED` at any time; create or update leaving it live answers 409 with each failing gate, and
`POST /admin/service/{id}/sandbox/release-check` (the provider page's "Check release gates") shows all three. Every
create, `EXEC` and `SET_TIMEOUT` checks them again (one verdict per provider kept for a minute; no checker or any
failure refuses). Fail closed is platform-wide: one PVE node that does not report its networks, a firewall
generation roll or a Node address change fails a gate, and every create and exec on that provider is refused until
the next check passes (up to a minute after it holds again).

| Gate | How the portal checks it |
|---|---|
| NodeRestriction on the sandbox cluster's API server | Impersonating the node `system:node:proxcloud-noderestriction-probe` (group `system:nodes`), it asks the API server for two dry-run creates of that node's own Node: plain, which the Node authorizer allows (so the identity works), then with a `node-restriction.kubernetes.io/` label, which only NodeRestriction refuses (`is not allowed to set the following labels`). Holds only when the first passes and the second is refused that way. Admission runs on dry runs; nothing is stored. The firewall service account may impersonate that one user and that group, nothing else (`rbac.yaml`), and a ValidatingAdmissionPolicy in the same file refuses that identity every write that is not a dry run (Kubernetes 1.30+), so a leaked token cannot register the probe Node |
| Isolation DROP for the sandbox CIDR | With the portal's token: the datacenter firewall is on, and this portal's rules in the cluster firewall are exactly, and enabled, the set `ReconcileIsolation` writes with `network.sandboxCidrs` (tenant pool to the sandbox network, sandbox network to the pool and the blocked networks, `forward` and `in`) |
| The SB2b fence on every worker | Read-only with the portal's token: PVE enforces VM firewalls, the current generation's IP sets and group hold exactly what the pass writes, and every Node without the `unsecured` taint (a tainted one takes no tenant pod, and NodeRestriction keeps the kubelet from dropping the taint) is a VM of the sandbox pool whose firewall reads back as the pass leaves it, with its filter pinned to the address the Node reports. At least one worker must be open |

### 3.7 Measured on the lab (2026-11-08)

A compose stack on this branch (scheduler off, jobs by trigger), its Proxmox provider with the SB0 cluster's pool,
`sandboxCidrs` and the firewall service account's kubeconfig, and a sandbox provider stored `DISABLED`; two tenant
projects. The portal reached the sandbox API server through a lab host forward.

- The stack's own fence pass changed nothing on the workers (same generation as the SB2b hand pass).
- Release check: isolation held, fence held ("2 worker(s) open ... pinned"), NodeRestriction did not: the firewall
  service account had no impersonation right, because applying this branch's `rbac.yaml` to the SB0 cluster was
  refused by the session's permission check. The same probe with the cluster admin's credential: the plain dry run
  passed, the labelled one was refused by NodeRestriction (`node "proxcloud-noderestriction-probe" is not allowed to set
  the following labels`). One of the stack's sandbox DROP rules disabled by hand: the isolation gate failed (`rule 12
  ... is disabled`); re-enabled. The provider page in a browser showed the three answers.
- Going live (`PUT status PUBLIC`): 409 with the NodeRestriction reason; the row stayed `DISABLED`.
- Attach to both projects: `sb-<project>` with the tenant label and the ResourceQuota (5 pods, 16 CPU, the lower of the
  provider cap and the quota for memory). A sandbox create: 503 "not available" (fail closed on the gate), nothing
  on the cluster. Profile suspended: project `DISABLED`, create 409, list 200; resumed: `ENABLED`, create 503 again.
  The other tenant: 404 on the project. Project deleted now: its namespace gone.
- Not run: anything with a running sandbox (console create, exec, logs, timeout, delete, quota refusal, suspension
  deleting a running sandbox, the minutely charge, a cross-tenant action on a sandbox id). They wait for `rbac.yaml`
  on the SB0 cluster; the service-level create, exec, logs, timeout and delete ran there in SB1.

After review (same day): the dry-run-only policy passed a server-side dry run on the SB0 cluster (v1.35); applying
it was refused by the session's permission check, so its effect is not measured. The fixes below are covered by
`test/integration/sandbox_test.go` (handler on Postgres, in-memory sandbox cluster) and
`cmd/api/suspension_walk_test.go`; still not run live.

### 3.8 Measured on the lab, with the RBAC applied (2026-11-08)

The same kind of stack from main after #117 (scheduler off, jobs by trigger), its own pool prefix, the SB0 cluster with
this repo's `rbac.yaml` applied; two tenants. Fixes from this run are in the same PR and were measured again on it.

- Release check in the operator console: all three gates held. Status set `PUBLIC` in the console: stored.
- Client console (headless Chromium): create (Running in about a minute, ends in 1 h), Run command (`6*7` = 42, kernel
  `4.19.0-gvisor`, uid 0), Logs, Extend by 1 hour (ends in 2 h), delete; the quota refusal shows its message.
- API: create, `EXEC` (`cmd` with exit code 3 kept; `command` with `stdin`), `GET_LOGS` (a line written to the
  container's stdout), `SET_TIMEOUT` (moved the end; 99999 s: 400 out of range). The other tenant: 404 on `EXEC`,
  `GET_LOGS` and delete of that id in its own project, 404 on the project. Quota `sandboxes: 2` with two running: 429;
  `sandboxVcpus: 2`: 429, and the namespace ResourceQuota followed the change.
- Profile suspended with two running: both deleted, rows `DELETED` with their end, project `DISABLED`, create 409;
  resumed: `ENABLED`, a create runs. Project deleted now with a running sandbox: namespace gone, row archived with its
  start and end. Provider `DISABLED` in the console: create, `EXEC`, `SET_TIMEOUT` 503, `GET_LOGS` and delete work.
- Billing: a 39 s sandbox no read saw running (the controller removed it at its limit) was archived by
  `run-sandbox-state` with an assumed start and billed; a 9 min one and a 4 min 50 s one were rated while running and
  after their end.

Bugs found and fixed:

- Every ended sandbox paid one minute too many (the 39 s one 2, 9 min 10, 4 min 50 s 6), for two reasons: the window
  started mid-minute, so it spanned an extra clock minute; and a row deleted after a charge's cycle timestamp but
  before that charge ran was rated up to its end with the watermark left at the cycle, so the next charge rated that
  minute again (the same holds for hourly resources). Now: 37 s read running, 1 minute; 2 min 51 s, 3 minutes; a 30 s
  one nothing read, 1 minute.
- A create naming no service, with the project's sandbox provider `DISABLED`, went to the Proxmox provider and
  answered 400 "SANDBOX is not supported on a Proxmox provider"; now 503 like the create naming it.
- `ReconcileIsolation` compared rules without their `enable` flag, so a rule disabled by hand was never repaired. Now
  a disabled DROP (or project ACCEPT) counts as missing: one of the stack's DROPs disabled by hand failed the
  isolation gate (`rule 12 ... is disabled`), and the next sync put it back enabled and the gate held.


## 4. Public entry, TLS and wake (SB4)

A port inside a running sandbox is reached at `https://<port>-<sandboxId>.<domain>`, one DNS label under the sandbox
domain, so a single wildcard name and certificate cover every sandbox:

```
client ──TCP 443──► entry appliance (HAProxy VM: public NIC + NIC on the sandbox vnet, TCP passthrough)
                    └──► sandbox-proxy's MetalLB address (sandbox cluster, sandbox-system)
                         TLS ends here (*.<domain>), route <port>-<id> → the ready pod's IP:<port>
```

### 4.1 sandbox-proxy

`cmd/sandbox-proxy` and `internal/sandboxproxy`, ported from EE with its routing unchanged: it looks the pod up by
`proxcloud.io/sandbox-id` (Running, Ready, not being deleted; hits and misses cached two seconds), forwards plain HTTP
to `<podIP>:<port>` with streaming and WebSocket upgrades, refuses envd's control-plane paths (`/init`, `/freeze`, ...)
on port 49983, and on every other port wants the sandbox's traffic token in `e2b-traffic-access-token` when the
sandbox was created with `allowPublicTraffic: false`. EE's trust of `X-Forwarded-Host` is gone: nothing sits between
the client and the proxy that could set it.

New here (D15): the proxy terminates TLS itself. `SANDBOX_TLS_DIR` holds `tls.crt` and `tls.key` from the mounted
Secret `sandbox-proxy-tls`; the files are read again at most once a minute on a handshake (`pkg/certfile`, shared with
the CSI gateway), so a renewal is picked up without a restart, and a broken write keeps the last good certificate.
Until the Secret holds a certificate the proxy waits (alive on `/healthz`, not ready on `/readyz`), so MetalLB answers
for the address only from a node with a ready proxy (`externalTrafficPolicy: Local`). Manifest:
`deploy/sandbox-cluster/sandbox-proxy.yaml` (two replicas on the system nodes only, §4.5; a `pods: list` ClusterRole
and nothing else). Image:
`deploy/sandbox-proxy.Dockerfile`, published by CI as `proxcloud-sandbox-proxy` next to the other images; the sandbox
nodes pull it from the registry over the internet, never from the mgmt registry they cannot reach.

### 4.2 The entry appliance

The load balancer appliance of P8-1 with one TCP listener (`pve.SandboxEntryConfig` renders it with `RenderHAProxy`):
443 passed through to `sandbox-proxy`'s address, so the appliance never holds the certificate. Installer stage 56
builds the VM (`sandbox.entry`): a clone of the stock image in its own pool `proxcloud-sandbox-entry`, `net0` on the
public bridge with the default route, `net1` on the sandbox vnet, each NIC's address pinned (`ipfilter-net0/1`), inbound
DROP. The Proxmox provider's sync (`syncjob.reconcileSandboxWorkers` → `pve.Service.SandboxEntry`) then keeps it in
line every cycle: firewall options, the listener as the only inbound rule (`SetVMInboundPorts`), and HAProxy's config
through the guest agent (`PushHAProxy`, which installs HAProxy over the public NIC first and disables sshd), pushed
only when the file's hash differs or HAProxy is not running. It touches a VM only when it carries the tag
`proxcloud-sandbox-entry` and sits in the entry pool (`config.sandbox.proxy.entry.pool`, stage 60 writes it), so a
recycled vmid or a tenant's VM that its owner tagged (the portal's `/pool` role reaches those) is never written to.

The workers admit the appliance alone: with `config.sandbox.proxy` (`address`, `port`, `entry.address`) the layer-1
group gets `in TCP <port> from <entry address> to <proxy address>` (§2.1), and the rule set refuses a proxy address
outside the MetalLB range or an entry address that is a worker's, a load balancer's or outside the subnet. A provider
without it keeps the generation it had. The workers' `-o` set excludes the whole sandbox subnet, so no worker can open
a connection to the appliance; anti-spoofing on both sides keeps a worker from taking its address.

Grants (stage 10, `docs/proxmox-permissions.md`): on the entry pool the portal holds `ProxCloudSandboxEntry`
(`Pool.Audit VM.Audit VM.Config.Network VM.GuestAgent.Audit VM.GuestAgent.Unrestricted`), replacing the platform role
it inherits from `/pool` (no power, disk, console or clone on a VM that faces the internet); `csi@pve` and
`capmox@pve!capi` get `NoAccess` there.

Limits, as for every P8-1 appliance: one VM, no standby (a single point of failure for every sandbox's ports), security
updates by hand, and no client address at the proxy (TCP passthrough without the PROXY protocol). Idle connections
stay open for an hour (P8-1's 60 s would cut a quiet WebSocket or SSE stream); sandbox-proxy closes a connection that
sends no request headers within 10 s. Without
`sandbox.entry.public.bridge` there is no appliance: forward TCP 443 to `sandbox.proxy.address` yourself (a site that
publishes sandboxes over a VPN only does that, D4).

### 4.3 Certificate and DNS

cert-manager on the **services cluster** issues `*.<domain>` (stage 56, `templates/sandbox-edge.yml.j2`, namespace
`pc-sandbox`), and `sandbox-proxy cert-sync` (a Deployment there) copies `tls.crt` and `tls.key` every minute to
`sandbox-system/sandbox-proxy-tls` with a sandbox-cluster credential that may get and patch that one Secret. Only a
complete, matching pair that is valid now and covers `*.<domain>` is copied, so an expired or wrong certificate (for
example a stale `issuer: secret` Secret) never replaces the one the proxy serves; cert-sync logs why. The issuer, the ACME account key and the DNS update key never reach the cluster
that runs tenant code: code that escapes a sandbox can read at most the certificate's private key on its node (§4.5).

`sandbox.tls.issuer`:

| Issuer | What stage 56 does |
|---|---|
| `acme` (default) | DNS-01 through PowerDNS with RFC2136. Stage 56 creates a TSIG key (`hmac-sha256`) on PowerDNS and lets it update the sandbox zone alone, and only from the services network (`TSIG-ALLOW-DNSUPDATE`, `ALLOW-DNSUPDATE-FROM` zone metadata), keeps it in `generated/sandbox-tsig.json` and gives it to an ACME `Issuer`. Needs `dnsupdate=yes` in `pdns.conf` (checked through the API) and the zone delegated publicly. `acme.ca_bundle` takes a private ACME server's CA |
| `secret` | Your `kubernetes.io/tls` Secret named `sandbox.tls.secret` in `pc-sandbox` on the services cluster; cert-sync copies it whenever it changes |
| `selfsigned` | Test sites without a public domain: a CA of its own (kept in `generated/sandbox-ca.crt`) issues the wildcard |

DNS: with `sandbox.powerdns.api_url` stage 56 points `*.<domain>` at the appliance's public address (the zone must
exist on PowerDNS). Without it, write that record yourself.

`dnsupdate=yes` is server-wide, and PowerDNS's `allow-dnsupdate-from` defaults to `127.0.0.0/8,::1`: any local process
on the PowerDNS host could then update any zone, tenant zones included, without a key. Set `allow-dnsupdate-from=`
(empty) in `pdns.conf` alongside it; the sandbox zone's own `ALLOW-DNSUPDATE-FROM` and TSIG metadata then decide alone.

The domain: every sandbox host is a sibling under `*.<domain>`, so one tenant's page can set cookies for
`.<domain>` that reach every other tenant's sandbox. Use a registrable domain of its own (`example-sandbox.net`, not
`sbx.example.org` under the portal's domain, whose cookies it could then also toss), and add it to the Public Suffix
List so browsers treat each host as its own site.

### 4.4 Wake and the tokens a tenant needs

`POST /api/v1/internal/sandboxes/{id}/wake` is how `sandbox-proxy` asks the portal to resume a paused sandbox (E2B
autoResume) when traffic finds no pod. It is public in `pkg/auth`; the bearer must be some sandbox provider's
`secret.proxyWakeToken`, compared before anything about the sandbox is read. Stage 56 generates the token
(`generated/sandbox-wake-token`, put it into the sandbox provider's `secret.proxyWakeToken`) and gives it to the proxy,
but not the portal's address yet: nothing is paused before SB7, so stage 56 leaves `PROXCLOUD_WAKE_URL` unset and the
proxy answers 502 "not found" for an unknown host at once, without calling the portal. SB7 sets it. With it set, the
endpoint answers 404 for a sandbox it cannot resume, and a proxy that cannot reach the portal answers 503 with
`Retry-After` after five seconds.

`GET_ACCESS` (native action, a read) returns the sandbox's host pattern `{port}-<id>.<domain>` and its traffic token
(and envd's token once envd runs, SB5), read from the live CR: the tokens are never on the cache row or in the
create answer.

### 4.5 What a compromised worker can reach here

The proxy pods hold the wildcard key and the wake token, and a kubelet may read the Secrets of the pods bound to its
node. Code that escapes gVisor on a node with a proxy pod could read both: impersonate every sandbox host to clients
that reach it, and ask the portal to wake sandboxes. So the proxy never shares a node with tenant code:

- Stage 55 adds the node group `sandbox-system` when a sandbox domain is set (`sandbox.system_nodes`, default 2 small
  workers, addresses from `sandbox.network.nodes`). Its Machines carry `node-restriction.kubernetes.io/proxcloud-system`
  (chart value `machineLabels`), which CAPI copies onto the Node from the management side; NodeRestriction keeps any
  kubelet from setting it, so a compromised worker cannot pass itself off as a system node. Its taint
  `proxcloud.io/sandbox-system:NoSchedule` keeps sandboxes off (they tolerate nothing but the GPU taint), and MetalLB's
  speaker tolerates it (chart value `proxmox.metallb.speakerTolerations`) so the proxy's address is announced there.
- The proxy requires that label (required node affinity, no preferred fallback) and tolerates that taint alone. With no
  system node it stays Pending; it never lands on a sandbox worker.
- The system workers sit in the sandbox pool: the sync fences them like the others (§2), and they join with the
  unsecured taint, so the proxy starts once the portal's sync has run (after stage 60; stage 56 then says so and checks
  the proxy on its next run).

Rotating means a new certificate (`kubectl -n pc-sandbox delete secret sandbox-wildcard-tls` on the services cluster)
and a new wake token in both places.

### 4.6 Measured on the lab (2026-11-08)

On the SB0 cluster, with no portal stack: stage 56 of this branch ran from the installer host against a split-mode copy
of the lab site (the SB0 cluster's admin kubeconfig standing in for stage 55's, the services objects in a scratch
namespace), the fence pass and the entry reconcile as a live test binary with the portal's own PVE token, two
sandboxes through `sandbox.Service`, and every request from the workstation through a lab host forward to the
appliance's public address. The lab has no public domain: a lab zone on PowerDNS, first `selfsigned`, then `acme`
against Pebble (Let's Encrypt's ACME test server) checking DNS-01 against the lab PowerDNS.

- Stage 56, first run: zone check, wildcard record (`*.<domain>` answered with the appliance's address), the VM (two
  NICs, both pinned, inbound DROP), the proxy on its MetalLB address waiting for its certificate, the issuer,
  certificate and cert-sync. A second run changed nothing but the copied manifests.
- The fence pass moved both workers to a new generation with `in tcp 443 from <appliance> to <proxy address>` as the
  only addition. The entry reconcile's first pass found no HAProxy and started its install (`not ready`), the second
  pushed the config: HAProxy active, the listener as the VM's only inbound rule, sshd off.
- Requests, TLS verified against the issuer's root only (system trust refuses the chain):

  | Request | Answer |
  |---|---|
  | tenant A's host, A's token | 200, the page A's sandbox serves on 8000 |
  | A's host, no token | 403 token missing |
  | A's host, tenant B's token | 403 token invalid |
  | B's host, A's token | 403 token invalid |
  | B's host, B's token | 200, B's page |
  | A's envd port (no envd before SB5) | 502 port not open |
  | a sandbox that does not exist | 503 after 5 s: the wake call cannot reach the lab portal from the workers (wake is now off until SB7: 502 at once) |

- Reload: the wildcard Secret deleted on the services cluster at 19:16:16; cert-manager reissued, cert-sync copied it at
  19:16:46, and the proxy served the new serial by 19:17:04, with no restart. Switching the issuer from `selfsigned` to
  `acme` reissued through Pebble (order valid, so Pebble found the TXT record that only the RFC2136 solver could have
  written with the TSIG key; PowerDNS refused an unsigned update to the zone); both replicas served the new chain (leaf, Pebble intermediate, verified against Pebble's root)
  within three minutes of the copy.
- Fenced: from a worker's node namespace the appliance (443) is dropped. Back on the workers' old generation (the pass
  with a provider config without `proxy`) the same request from the workstation times out.

Not run here: stage 10's entry pool rows (the grant was refused by the session's permission check; the appliance ran
in a scratch pool that inherits the portal's `/pool` role instead) and stage 60's; the wake endpoint against a portal
(its tests run on Postgres: `test/integration/sandbox_test.go`); `GET_ACCESS` through a portal (same test). The proxy
image was built on the workstation and loaded into each node's containerd; the workers cannot reach a lab registry,
and none is configured for them.

Review follow-up, same day, on the SB0 cluster: a third MachineDeployment shaped as stage 55 now renders
`sandbox-system` (template label `node-restriction.kubernetes.io/proxcloud-system`, taints `unsecured` and
`sandbox-system`). CAPI v1.14 put the label on the Machine and on the Node within a minute of the join; the kubelet's
own `--node-labels` did not carry it. Three replicas with the proxy's scheduling rules (plus a toleration for
`unsecured`, since no fence pass ran for that node) all landed on the system node, though the spread constraint
prefers the other two workers; a gVisor pod pinned to the system node stayed Pending ("untolerated taint"). The
manifest passed `kubectl apply --dry-run=server`. MetalLB's speaker ran on the two old workers only (SB0's MetalLB
values predate `speakerTolerations`), which is why stage 55 now sets it. The node group and the probes were removed
afterwards. Not repeated end to end: the full entry path with the proxy on a system node, and wake (now off).

## 5. The E2B-compatible API (SB5)

The stock E2B SDKs (`e2b`, `e2b-code-interpreter`) work against a sandbox provider that runs envd under a sandbox
domain. Feature by feature, what works, what differs and what does not: [e2b-compat.md](e2b-compat.md).

```
SDK ──REST, X-API-Key──► proxcloud-api /e2b/*  (E2B_API_URL=https://<api host>/e2b)
 │                          └── same gate, quota and create as the native API ──► Sandbox CR on the sandbox cluster
 └──HTTPS──► https://<port>-<id>.<domain> ──► entry appliance ──► sandbox-proxy (section 4) ──► pod:49983 (envd)
```

### 5.1 Keys

A sandbox API key is `e2b_` and 40 hex digits. It belongs to one project and acts as the member who created it; every
call checks again that this member may manage cloud resources in the project, so removing the member stops the key.
Only its SHA-256 is stored (table `sandbox_api_keys`, the hash is the id): a database read never yields a usable key.
The client console's Sandboxes > API keys tab, or `GET`/`POST`/`DELETE /api/v1/project/{id}/sandbox-api-keys[/{keyId}]`,
lists (name, hint, created, last used), mints (the key is shown once) and revokes keys; a revoked key answers 401 at
once. A PAT (`pk.sk`) is not accepted here: it reaches every project of its user (D8).

### 5.2 One door more, the same checks

`/e2b/*` is public in `pkg/auth`; its own gate resolves the key. Create, `POST .../timeout`, and `refreshes` or
`connect` that move the end later call the native `sandboxUsageGate` (a deleting or DISABLED project 409, a suspended
billing profile 403, a DISABLED provider or a release gate that does not hold 503) and the create carries the project
quota (429), then creates the sandbox and its row the native API does, so billing, suspension, teardown and the
cross-project checks are the same. List, get, logs and kill stay open, as on the native API. A create returns once the
sandbox is ready (envd up, the template's ready command passed), at most 50 s, and records the start on the row then,
so a sandbox only the E2B API ever touched is billed from that moment. `test/integration/sandbox_e2b_test.go` covers
each gate on each of these routes, and fails with the gate calls taken out.

### 5.3 envd in the sandbox

With `config.sandbox.envd: true` and a `domain` (the admin form's "Sandbox domain" and "Run envd";
installer stage 60 sets both when `sandbox.domain` is set), every sandbox mounts `/opt/proxcloud-sandbox/bin` from its node read-only at `/.proxcloud`
and runs `/.proxcloud/sandbox-init` as PID 1: it creates the template's user if the image has none, starts envd
(`-isnotfc`, port 49983), posts envd's `/init` over localhost with the sandbox's env, user, workdir and access token,
runs the template's start command (as root with `startAsRoot`), marks the pod ready once the ready command exits 0,
restarts envd if it dies, and relays ports bound to localhost to the pod address. A restarted envd has no access token
until its `/init`, and envd without a token lets every request through, so sandbox-init first drops the ready marker
(the readiness probe turns the pod NotReady and sandbox-proxy routes only to Ready pods), waits 5 s (probe period plus
the proxy's 2 s lookup cache), starts envd, and puts the marker back only once `/init` succeeded. The two binaries come
from `proxcloud-sandbox-runtime` (`deploy/sandbox-runtime.Dockerfile`: envd built unmodified from e2b-dev/infra
`e85b9f5`, Apache-2.0, notice in the image), which `node-setup`'s init container copies to every node (stage 55 pins its
tag, and leaves the init container out on a site without a sandbox domain).

### 5.4 Built-in templates

Until template builds (SB6) the provider offers only the templates of its `config.sandbox.templates`. The E2B SDKs ask
for `base` (`Sandbox.create()`) and `code-interpreter-v1` (`e2b_code_interpreter.Sandbox.create()`).
`deploy/templates/build.sh` builds both (`PUSH=1 REGISTRY=ghcr.io/mutex-solutions TAG=<tag>`, prints each reference
with its digest; SB6a moves this into a CI workflow):

| Template | Image | Config |
|---|---|---|
| `base` | `sandbox-base`: E2B's base image plus user `user` with passwordless sudo | `"user": "user", "workdir": "/home/user"`, 1-2 vCPU / 1 GiB |
| `code-interpreter-v1` | `sandbox-code-interpreter`: Jupyter and E2B's code-interpreter server on :49999 (Python, JavaScript, Bash kernels) | `"startCmd": "/root/.jupyter/start-up.sh", "startAsRoot": true, "readyCmd": "curl -fsS localhost:49999/health", "user": "user", "workdir": "/home/user"`, 2 vCPU / 2 GiB |

The code interpreter's start counts towards the create's 50 s: at 1 vCPU on the nested lab it took 37-40 s on an idle
node and missed the limit when its node was busy, so offer it at 2 vCPU.

### 5.5 Measured on the lab (2026-11-08)

A compose stack on this branch (`pc-sb5`, scheduler off, jobs by trigger) with its own Proxmox provider (pool prefix
`pc5-`, the SB0 workers' pool, `config.sandbox.proxy` naming its entry), a sandbox provider with envd under a lab
domain, two tenants with a key each, the SDKs on the workstation. The SB0 cluster (two workers, 2 vCPU / 3 GiB each)
got the runtime installed by the runtime image run as node-setup's init container would, the template and proxy images
loaded into each node's containerd (the workers reach no lab registry), a `sandbox-proxy` with a lab CA's wildcard
certificate and an entry appliance VM shaped as stage 56 builds it in a scratch pool; the stack's sync fenced the
workers with the entry rule and pushed the appliance's HAProxy config. Lab deviations: no `sandbox-system` node group
on SB0, so the proxy ran on a worker; the client reached the appliance through a lab host forward.

- Release check: all three gates held; provider set `PUBLIC`.
- `tests/e2b-compat/compat.py` with the stock `e2b` 2.63.0 and `e2b-code-interpreter` 2.11.9: all checks pass. Create of
  `base` in about 9 s; commands as `user` in `/home/user` with `envVars`, streamed output, exit code, background process
  listed and killed; `python3` run; files written, read, listed, binary round trip; a port served at
  `https://8000-<id>.<domain>` through the appliance and the proxy; `set_timeout`, `get_info` with metadata, list by
  metadata, `connect` from a second client; `allow_internet_access=False` blocks egress, the default reaches the
  internet, 169.254.169.254 stays unreachable; kill, and the port answers 502 afterwards. Code interpreter: create in 37 s,
  `run_code` result `42`, stdout and stderr, `ZeroDivisionError`.
- Other project's key: `connect` 404, list empty, kill refused (the sandbox kept running). Revoked key: 401 on list and
  create at once, the other project's key unaffected; no key or a PAT: 401.
- Provider `DISABLED`: E2B create 503, list 200; back to `PUBLIC`.
- Client console (headless browser): API keys tab, key shown once, listed by hint only, works (200), revoked (401).
  Admin console: the provider's "Sandbox domain" field and "Run envd" switch.
- MCP client toolset as the tenant: `create_sandbox`, `list_sandboxes` (RUNNING), `exec_in_sandbox` (`42`,
  `4.19.0-gvisor`), `get_sandbox_logs`, `set_sandbox_timeout`, `delete_sandbox`. On the compose stack the admin and client
  realms are one, so the check ran with the admin issuer moved off it for the run.
- The keys table holds hashes, names and hints only. Ended E2B sandboxes are archived with their start.
- Found: another drill fencing the same pool with a different provider config rolled the workers to its generation
  while this ran; the fence gate then failed (503 on create) until this stack's sync ran again. Two portals must not
  fence one sandbox pool.
