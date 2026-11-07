# Managed sandboxes (gVisor) — design notes and runbook

Sandboxes are short-lived containers for untrusted code (agents, code interpreters), run under gVisor (`runsc`) on a
Kubernetes cluster that belongs to the platform, with an E2B-compatible API in front. The feature is being ported from
an earlier implementation by the same owner (called **EE** below). It is not in the product yet: this file starts with
the lab spike (phase SB0) and grows into the operator runbook as the phases land.

> ## Status
>
> **Not reachable by tenants.** The sandbox service is ported (SB1) with no route, and the admin API refuses the
> `sandbox` provider kind. SB2a adds the cluster itself: manifests in `deploy/sandbox-cluster/`, installer stages 10
> and 55, and isolation drops for the sandbox network (section 1). SB2b adds layer 1: the VM firewall on every NIC
> of the workers, kept by the portal's sync job, the `unsecured` taint until it is in place, and the sandbox's own
> egress address (section 2). The billing, quota and suspension gates (SB3) are still missing. Section 0 records the
> hand-built lab spike. Phase order and decisions: the sandbox plan (owner document, not in this repository).

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
| Cluster | stage 55, `templates/sandbox-cluster.yml.j2` | an ArgoCD Application `pc-sandbox` of `proxmox-kamaji-cluster` at this checkout's chart version (0.1.7), namespace `pc-sandbox`, no project label: no portal sync lists it. No CSI, no StorageClass, no NAS dataset. One node group `sandbox`, no taint (the SB1 code sets no toleration on CPU sandboxes) |
| Release gate: NodeRestriction | stage 55 | the chart's `admissionControllers` include it; stage 55 reads the `KamajiControlPlane` and stops if it is missing |
| Runtime, guardrail, identity | `deploy/sandbox-cluster/` (README has the order) | `cilium-node-config.yaml`, `node-setup.yaml` (runsc 20261102.0, systrap), `runtimeclass.yaml`, agent-sandbox v1.0.9 vendored, `network-policy.yaml` (private ranges and TCP 25 denied), `rbac.yaml` (`proxcloud-system/proxcloud`, no Secret access) |
| Release gate: IPv4 only | `node-setup.yaml`, `check-ipv4-only.sh`, stage 55 | node-setup refuses a host with a global IPv6 address or an IPv6 default route; the check wants Cilium `enable-ipv6=false`, no IPv6 node address or pod CIDR, and node-setup Ready everywhere. Stage 55 stops before agent-sandbox and the guardrail when it fails |
| Layer 0: isolation drops | `internal/cloud/pve/network.go`, provider `network.sandboxCidrs` (stage 60 writes it) | section 1.2 |

Not in SB2a: the per-NIC VM firewall with `ipfilter` and the `unsecured` taint (SB2b), a dedicated SNAT address
(SB2b), the `sandbox-system` node group, `sandbox-proxy` and TLS (SB4), the envd runtime image (SB5).

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
| out | anything | IP set `<group>-o`: `0.0.0.0/1` and `128.0.0.0/1` minus the private, CGNAT, link-local, multicast and reserved ranges, the management networks, every address a PVE node holds on any interface (uplinks, bonds and VLANs too), the sandbox's own SNAT address, `blockedCidrs`, the tenant pool, the sandbox subnet itself (its gateway is a PVE node) and the control plane address |

Nothing else comes in: there is no rule for a tenant, for the services cluster beyond the kubelet, or for anyone on the
internet. The sandbox entry (HAProxy appliance to `sandbox-proxy`) gets its rule with SB4.

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
  only survive a tracked flow). The host ICMP guard (§2.6) closes it: a host nftables chain on every node, ahead of
  PVE's, drops a forged-source error leaving a sandbox worker's tap.

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

## 2.6 Layer 1b: the host ICMP anti-spoof guard

Layer 1's `ipfilter` pins the source of every frame a worker sends — except ICMP. PVE's nftables bridge firewall
(`table bridge proxmox-firewall-guests`) has a chain `allow-icmp` that its `vm-out` (prerouting) and `vm-in`
(postrouting) base chains `jump` *before* `iifname vmap @vm-map-out` reaches the per-guest `ipfilter` and `macfilter`
drops. `allow-icmp` accepts the ICMP error types unconditionally (v4 destination-unreachable, source-quench,
time-exceeded; v6 destination-unreachable, packet-too-big, time-exceeded, parameter-problem). So a worker can send an
ICMP error with any forged source and neither `ipfilter` nor the `DROP` policy sees it. On the lab a worker reached PVE
host input on the vnet gateway with the gateway's own address, another worker's address, a management address and an
arbitrary external address as source; routed ones to another worker only survived a tracked flow, but the host and node
addresses were reachable off-path. (Upstream Proxmox behaviour, PVE 9.2; a bug report is filed separately.)

The guard closes it on the host, since the VM firewall cannot. On every node that can host a sandbox worker (the pool
spans the cluster, so every `pve` node) the installer places `/usr/local/sbin/sandbox-icmp-guard.py` and a systemd
service plus a one-minute timer (`proxcloud-sandbox-icmp-guard`). The script reads, from PVE's own cluster config
(`/etc/pve`), each local sandbox-pool worker's NICs on the sandbox vnet and that NIC's `ipfilter-net<N>` set — the
worker's pinned source addresses, kept correct by the portal's sync — and installs a host nftables chain:

```
table bridge proxcloud-sandbox {
  chain antispoof {
    type filter hook prerouting priority -300; policy accept;   # before PVE's vm-out (priority 0)
    iifname { <sandbox taps> } icmpv6 type { … } drop            # IPv4-only cluster: any v6 error is spoofed
    iifname "<tap>" meta l4proto icmp icmp type { … } ip saddr != { <this worker's ipfilter> } drop
  }
}
```

Because the chain is a base chain at an earlier priority with `policy accept`, a non-matching frame (a legitimate
own-source error, or any non-ICMP) continues to PVE's chain — an `accept` verdict there is not terminal for the hook.
Only a forged-source error is dropped. The guard mirrors `ipfilter` exactly (a worker's own address plus the MetalLB
range, which any worker may answer for), so it drops nothing PVE's anti-spoof would allow. The timer re-runs the script
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
worker ping (ICMP echo, not an error type) 0% loss. Not run here: the installer stage (it would touch the lab's shared
services cluster); the guard script was deployed to both worker nodes and run by hand with the content stage 55 writes.
