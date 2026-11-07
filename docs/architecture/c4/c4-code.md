# C4: Code

Code-level views of the components where a mistake costs the most: tenant network
isolation, the VM firewall and public IPs, quota enforcement, and the billing
rating path. Each section lists the key functions with `file:line` (a snapshot,
see the [maintenance rule](README.md#maintenance-rule)), then explains the
invariants the code holds. Four sequence diagrams follow.

Status legend: [README.md](README.md). Everything on this page is **Implemented**.

## C4.1 Tenant network isolation

| Symbol | Location | Role |
|---|---|---|
| `pveCreateNetwork` | `internal/platform/project/cloud_network_pve.go:78` | `POST /project/{id}/cloud {type:NETWORK}` on a Proxmox provider |
| `lockSDN`, `withSDNLock` | `cloud_network_pve.go:38`, `:54` | Platform-wide `sdn-apply` lock (PostgreSQL); busy returns 409, a crashed holder expires after 3 minutes |
| `pvePickCIDR` | `cloud_network_pve.go:255` | Requested CIDR must sit inside `cidrPool`, be `/29` or larger and overlap nothing; blank picks the first free `/24` |
| `Service.SubnetCIDRs` | `internal/cloud/pve/network.go:298` | Every subnet on the cluster (all zones, all projects, foreign vnets), read under the lock |
| `Service.CreateNetwork` | `network.go:157` | Stages vnet `v<5 base36>` with alias `st:<projectId>:<name>`, then its subnet (gateway, DHCP range, SNAT) |
| `ZoneIDFor` | `network.go:341` | Per-project EVPN zone id: `p` + 7 base36 chars of `sha256(projectId)` (8 chars, the PVE limit) |
| `Service.EnsureProjectZone` | `network.go:353` | Stages the project zone from the default EVPN zone template; next free VRF VNI (from 10000); refuses an id collision with another owner |
| `isolationRules`, `projectIsolationRules`, `Service.isolationOwner` | `network.go` | This portal's provider-wide DROPs, one project's ACCEPTs, and the comment that marks the DROPs as this portal's (below) |
| `Service.ReconcileIsolation` | `network.go` | Brings the cluster firewall to the rule set: rebuilds this portal's DROPs when they are missing or the networks changed, then this project's ACCEPTs over all its subnets. Called on every network change and, per project, from the sync job |
| `Service.deleteClusterRule` | `network.go` | Deletes a rule by identity: fresh position, sent with the list's digest, so a list edited in between can never cost another rule |
| `Job.reconcileIsolation` | `internal/cloud/syncjob/job.go` | Per sync cycle and project, under the `sdn-apply` lock; a busy lock skips to the next cycle |
| `Client.ManagementCIDRs` | `internal/cloud/pve/cluster.go` | The networks of the bridges carrying a node's own IP or gateway; always blocked |
| `Service.ApplyAndWait` | `network.go:553` | `PUT /cluster/sdn` and wait for the task |
| `pveDeleteNetwork` | `cloud_network_pve.go:175` | Refuses while a pool VM NIC uses the vnet; deletes, re-reconciles, stages an empty project zone for removal, applies once (PVE accepts both in one apply) |
| `Client.AddClusterFirewallRule`, `DeleteClusterFirewallRule` | `internal/cloud/pve/firewall.go` | Cluster firewall REST calls |

**Isolation modes** (`config.network.isolation` on the provider):

- `shared` (default): every tenant vnet sits in the provider's default zone, one
  routing domain. The rule set below drops traffic between tenants.
- `evpn-per-project`: each project gets its own EVPN zone (its own VRF) with id
  `ZoneIDFor(projectId)`. The same rule set still applies, because exit nodes leak
  every VRF into the default table and the VRF alone is not a wall.

**Managed rule set in the cluster firewall**, top to bottom (first match wins):

1. Two per project, comment `proxcloud:isolation:<projectId>`: `FORWARD ACCEPT` and `IN
   ACCEPT`, both `source=<project subnets> dest=<project subnets>` (absent when the project
   has no subnets, since an empty field means "any").
2. `FORWARD DROP source=<cidrPool> dest=<cidrPool>`.
3. `IN DROP source=<cidrPool> dest=<cidrPool>`: a gateway address lives on the node, so
   pinging another tenant's gateway is input, not forward. The project's own `IN ACCEPT`
   keeps its own gateways reachable.
4. `FORWARD DROP source=<cidrPool> dest=<management networks, blockedCidrs, sandboxCidrs>`.
5. `IN DROP source=<cidrPool> dest=<management networks, blockedCidrs, sandboxCidrs>`: an exit
   node's own addresses are reached through input, not forward.
6. Only with `network.sandboxCidrs` (the sandbox cluster's worker vnet): `FORWARD DROP` and `IN
   DROP`, `source=<sandboxCidrs> dest=<cidrPool, management networks, blockedCidrs>`. Sandbox
   workers leave through an exit node that SNATs them to a node address, which the NAS exports and
   the management hosts admit, so their drop must happen here, before the rewrite. An entry that
   overlaps `cidrPool` or a blocked network (the management networks included) is refused (the
   reconcile fails and writes nothing).

**Layer 1, the sandbox workers' VM firewall** (`internal/cloud/pve/sandboxfw.go`, `syncjob/sandboxfw.go`, provider
`config.sandbox`). The rules above see routed traffic only. Every VM of the sandbox pool also gets `firewall=1` on each
NIC, VM options `policy_in`/`policy_out` `DROP` with `ipfilter` and `macfilter`, and as its only rule a reference to a
cluster security group `sbx<pool hash><content hash>` whose rules all ACCEPT: its control plane's API and konnectivity
ports, DNS to the configured resolvers, VXLAN, Cilium health, MetalLB memberlist and the kubelet between workers (IP set
`<group>-n`), the kubelet from `kubeletSources`, with `config.sandbox.proxy` TCP 443 from the entry appliance's vnet
address to `sandbox-proxy`'s MetalLB address alone (SB4), with `config.sandbox.registry` TCP out to the sandbox
registry's address and port alone (SB6a), with `config.sandbox.buildStorage` TCP out to the template build bucket's
address and port alone (SB6b), and the internet (IP set `<group>-o`: everything minus the private
ranges, the management networks, every node address, the sandbox's SNAT address, `blockedCidrs`, `cidrPool`, the
sandbox subnet and the control plane). With every
rule an ACCEPT under DROP policies, no order or partial write opens anything; a changed set is a new generation that
workers move to before the old one is deleted. `ipfilter-net0` is the worker range until its Node joins, then pinned
to the Node's address once (read only while the worker is tainted, never followed after) plus the MetalLB range. The
sync job applies it every cycle and only then takes the `proxcloud.io/unsecured` taint off a worker at least 20 minutes
old; a cleared worker found unfenced (the datacenter firewall off included) gets the taint back. Details:
[managed-sandbox.md §2](../../managed-sandbox.md#2-layer-1-the-vm-firewall-on-the-workers-sb2b).

**Layer 1b, the host anti-spoof guard** (installer `sandbox-icmp-guard.py` + a systemd timer on every `pve` node).
PVE's bridge firewall accepts ICMP error types and conntrack established/related before the per-NIC `ipfilter`, so a
worker can forge their source. The guard installs a host nftables chain (`table bridge proxcloud-sandbox`, prerouting
priority -300, before PVE's) that drops any IPv4 frame leaving a worker's tap whose source is not in that worker's
`ipfilter` set, read from PVE's cluster config each run; every node pins every worker, so a migrated one is covered. It is the installer's and not the sync's: the portal's PVE token reaches the API, never a node shell.

**Who owns the DROPs.** A cluster normally has one portal, which owns its DROPs outright: a
changed `cidrPool` or `blockedCidrs` replaces them and leaves none behind. Their comment is
`proxcloud:isolation` for the default pool prefix `pc-` (clusters already running need no
migration) and `proxcloud:isolation/<poolPrefix>` for any other. A portal only rewrites DROPs
carrying its own comment, and only moves ACCEPTs inside its own pool, because those are the only
ones its new DROPs can shadow. So a second portal on the cluster, with another pool prefix, can
never remove the DROPs protecting the first one's tenants, whatever its pool. What this does not
give two portals with different pools is isolation between them: add each portal's pool to the
other's `blockedCidrs`.

These are cluster rules, not vnet rules, because traffic between subnets is routed by
the node: on PVE 9.2 with `proxmox-firewall`, vnet `FORWARD` rules only see frames
switched inside one vnet bridge, and a routed packet never hits them. Traffic inside
one vnet is switched and needs no rule. There is deliberately no blanket
`DROP source=<cidrPool>`: it would also drop egress to the internet through SNAT.

Addresses: PVE offers DHCP on simple zones only (`dhcp` on an EVPN zone is refused). On a
zone without DHCP the platform allocates the server's `net0` address when it clones the VM
(`VnetStaticPool`, `PublicIPRepo.Allocate`, a row on NIC `net0`), writes it to `ipconfig0`,
and pins `ipfilter-net0` to that `/32`. On a DHCP zone the set is the subnet plus `0.0.0.0`,
because `proxmox-firewall` checks ipfilter before `dhcp=1` and would drop DISCOVER.

```mermaid
flowchart LR
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a

  subgraph pa["Project A"]
    va1["vnet A1<br/>subnet in cidrPool"]:::impl
    va2["vnet A2<br/>subnet in cidrPool"]:::impl
  end
  subgraph pb["Project B"]
    vb1["vnet B1<br/>subnet in cidrPool"]:::impl
  end
  inet["Internet via SNAT"]:::impl

  mgmt["Node management networks,<br/>blockedCidrs"]:::impl
  sbx["Sandbox workers<br/>sandboxCidrs"]:::impl

  va1 <-->|"ACCEPT: own subnets"| va2
  va1 -.-x|"DROP: dest in cidrPool"| vb1
  va1 -.-x|"DROP"| mgmt
  va1 -.-x|"DROP"| sbx
  sbx -.-x|"DROP: to cidrPool, mgmt"| mgmt
  sbx -.-x|"layer 1, VM firewall: DROP but own CP, DNS, workers, internet"| mgmt
  va1 -->|"egress allowed"| inet
  vb1 -->|"egress allowed"| inet
```

Invariants:

- The CIDR pick and the vnet staging happen under the same lock, so two concurrent
  creates cannot land on one range, and `SubnetCIDRs` also sees staged networks.
- The rule set is reconciled **before** the SDN apply. If reconcile or apply fails, the
  vnet is unstaged and the apply re-run. New DROPs go in before old rules go, so every
  step fails closed.
- Every sync cycle reconciles each project, so a project made before a rule change gets it
  within 15 minutes and a rule deleted or disabled by hand comes back (the compare includes
  `enable`; so does the lookup of a rule to delete, so a disabled copy goes and its enabled twin stays). An
  up-to-date cluster takes no write.
- A node that is down does not block a network change: its networks are left out of the check,
  DROPs already in place stay until it is back, and the project's ACCEPTs are still set.
- Rules are deleted by identity with the list's digest. PVE has no rule ids and refuses a
  stale digest, so an operator or a second portal editing the list cannot make a delete hit
  the wrong rule.
- The test connection (`internal/platform/admin/pveadmin.go:47`) refuses a cluster
  whose datacenter firewall is off or where a node still runs the legacy iptables
  firewall (it ignores FORWARD rules), so the rules above are actually enforced. It warns about
  each network store (NFS, CIFS, GlusterFS, iSCSI, external Ceph monitors) whose server is outside
  the dropped networks: a vnet's NAT gives its
  traffic a node's address, which a NAS export to the nodes admits.
- Verified live on PVE 9.2 in both isolation modes, on one node and across nodes: same
  project reaches its other vnets, another tenant and the management network are dropped,
  SNAT egress works, a spoofed source address is dropped. Verified again with packets for the
  gateway rules, the sync reconcile (rules removed by hand, traffic state before and after) and
  two portals with different pools on one cluster.
- Outside this rule set: a public NIC is bridged straight to the uplink, so no cluster rule
  applies and the VM reaches whatever the uplink routes. Filtering that is the operator's edge
  (see Public networks in the operator docs).

## C4.2a Load balancers

| Symbol | Location | Role |
|---|---|---|
| `Handler.pveCreateLoadBalancer`, `lbPlugPublic` | `internal/platform/project/cloud_lb_pve.go` | Row first, then a server build with no group and no seed; public NIC before first boot |
| `Handler.pveBuildServer` | `internal/platform/project/cloud_writes_pve.go` | The server create shared with `pveCreateServer`; `pveAppliance` is what only the platform can set |
| `Handler.pveLBApply`, `lbApplyLocked` | `cloud_lb_pve.go` | One balancer in line with its row: members re-resolved, config hash compared, pushed when it differs |
| `Handler.ReconcileLoadBalancers` | `cloud_lb_pve.go`, via `syncjob.WithLoadBalancers` | The pass on every sync; a stopped appliance is started here |
| `Handler.lbMemberAddress` | `cloud_lb_pve.go` | The ownership check of a member: pool, stamp, allocation on the balancer's network and subnet |
| `Handler.pveApplianceGuard`, `lbOwnerOf` | `cloud_lb_pve.go` | 409 on a tenant's direct action on the VM, its disk or its address; the VM's own tag decides, the balancer's record when it has no row yet |
| `refuseAppliance` | `lb.go`, in `AddVolume`, `AttachVolume`, `AttachTargets`, `CreateImage` | A request cannot name the appliance as a target |
| `Service.undoCreate`, `VMLeftError` | `internal/cloud/pve/write.go` | A failed create destroys its VM; its addresses go back only when PVE confirms it is gone. Every server create |
| `LBSpec.Validate`, `RenderHAProxy` | `internal/cloud/pve/lb.go` | Request checks; the config, safe on any stored row |
| `Service.PushHAProxy`, `SetVMInboundPorts`, `LBHealth` | `lb.go` | Install if missing, `haproxy -c`, swap, reload, swap back if the reload fails; listener ports on the VM firewall; `show stat` |
| `Client.GuestStart`, `GuestRun`, `GuestWriteFile` | `internal/cloud/pve/agent.go` | Guest agent `exec` and `file-write`; only ever used on a VM with the `pc-lb-` tag |
| `PublicIPRepo.AllocateBeforeRow` | `internal/platform/project/publicip_pve.go` | A public address taken during the build, which `orphanIPs` must not read as a reused vmid |

## C4.2 VM firewall, default security group and public IPs

| Symbol | Location | Role |
|---|---|---|
| `Service.CreateServer` | `internal/cloud/pve/write.go:68` | Clone, configure, resize, firewall, start |
| `nicFirewallParams`, `extraNICKeys` | `internal/cloud/pve/firewallsvc.go:193`, `:211` | `firewall=1` on every NIC; drop every cloned NIC beyond `net0` and its `ipconfigN` |
| `vmFirewallOptions` | `firewallsvc.go:243` | `enable=1 ipfilter=1 macfilter=1 dhcp=1 policy_in=DROP policy_out=ACCEPT` |
| `Service.SecureVM` | `firewallsvc.go:257` | `ipfilter-netN` = the vnet's subnets for each NIC on a tenant vnet, then the VM firewall options |
| `Service.SetIPFilter` | `firewallsvc.go:295` | Makes an ipfilter set hold exactly the given CIDRs; a changed set is dropped and rebuilt (fail closed) |
| `defaultGroupRules`, `EnsureDefaultSecurityGroup` | `firewallsvc.go:351`, `:361` | Project `default` group: ingress from `cidrPool` only (other tenants are already dropped by the isolation rules) |
| `Service.ServerSecurityGroups` | `firewallsvc.go:386` | Resolve the user's group names or ids to owned groups; none given means the default group |
| `pvePublicNetworkAction` | `internal/platform/project/cloud_network_pve.go:315` | `ATTACH_PUBLIC_NETWORK` / `DETACH_PUBLIC_NETWORK` on a server |
| `PublicIPRepo.Allocate`, `allocatePublicIP`, `pickPublicIP` | `internal/platform/project/publicip_pve.go:75`, `:101`, `:125` | Lowest free address of the network's pool; insert with primary key `serviceId/networkId/ip`, retry on a duplicate |
| `Client.ManagementBridges` | `internal/cloud/pve/cluster.go:85` | Bridges carrying a node's own IP or gateway; a public network on one is refused |

Invariants:

- A server must sit on one of the project's own networks. The template's NIC is
  replaced by `net0` on the tenant vnet; any other cloned NIC is removed, so a
  template made from a VM with a public NIC cannot hand out that VM's bridge or
  ipfilter. A clone also inherits the source's firewall rules, so they are cleared before
  the chosen groups are added.
- The firewall is configured **before the first boot**. If adding a group or
  `SecureVM` fails, the VM is destroyed rather than started open.
- A NIC on a bridge that is not a tenant vnet gets an empty ipfilter set, which
  drops its IPv4 egress until the public IP pool pins an address to it.
- Public attach order: quota check (`floatingIps`), allocate, `SecureVM`,
  `ipfilter-net1` = the address `/32`, then plug `net1` with `firewall=1`. Detach
  empties the ipfilter set first, unplugs, and only then returns the address.
  Server delete and project teardown release the server's or project's addresses.
- A public NIC takes the default route: a static `net0` loses its gateway while one is
  attached, because replies from the public address must not leave through the overlay.
  The guest picks up `ipconfig1` and the route change at its next boot, so attach and
  detach answer `rebootRequired: true`.

## C4.3 Quota enforcement

| Symbol | Location | Role |
|---|---|---|
| `pveQuotaLimits` | `internal/platform/project/cloud_quota_pve.go:40` | The project's quota, else the provider default (`config.provisioning.quota`) |
| `Handler.enforcePVEQuota` | `cloud_quota_pve.go:49` | Gate a demand against the limits |
| `checkPVEQuota` | `cloud_quota_pve.go:70` | The pure gate: reads usage only for limited keys that are requested; a usage read error returns 503 |
| `Handler.pveQuotaUsage` | `cloud_quota_pve.go:103` | Sums the project's cache rows on the provider: servers, vCPUs, RAM, networks, volume GB |
| `pveResizeDemand` | `internal/platform/project/cloud_writes_pve.go:591` | Resize demand = new flavor minus cached flavor for vCPUs, RAM and boot disk |
| `Handler.enforcePVEGPUQuota` | `cloud_writes_pve.go:605` | GPU quota per model; a resize frees the replaced flavor's GPUs first |
| `BootstrapProxmoxOnto` | `internal/platform/project/bootstrap.go:96` | Seeds the provider default quota onto a project that has none |

Keys: `servers`, `vcpus`, `ramMb`, `diskGb`, `networks`, `floatingIps`, plus the
`gpu` map. Sandbox keys (`sandboxes`, `sandboxVcpus`, `sandboxRamMb`, `sandboxGpus`) live on the same
document but follow C4.5: an absent one falls back to the sandbox provider's default, never to unlimited. A key absent from a non-empty quota is unlimited; with no project quota
and no provider default, everything is unlimited. `floatingIps` usage is counted
from the `pvePublicIp` table, not the cache.

| Gate point | Demand |
|---|---|
| Server create | `servers 1`, flavor vCPUs and RAM, root disk (the user's root size when given) |
| Server resize | `pveResizeDemand` (shrinks count as 0) |
| Volume create / extend | size, or the growth |
| Network create | `networks 1` |
| Public IP attach (new address) | `floatingIps 1` |

```mermaid
flowchart TB
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a

  start["create / resize / attach request"]:::impl
  limits["pveQuotaLimits<br/>project quota, else provider default"]:::impl
  any{"key limited and requested?"}
  usage["read usage<br/>cache rows or pvePublicIp"]:::impl
  err["503 usage unavailable"]:::impl
  cmp{"used + demand > limit?"}
  deny["409 Quota exceeded"]:::impl
  ok["proceed"]:::impl

  start --> limits --> any
  any -->|"no"| ok
  any -->|"yes"| usage
  usage -->|"read error"| err
  usage --> cmp
  cmp -->|"yes"| deny
  cmp -->|"no"| ok
```

## C4.4 Billing rating path

| Symbol | Location | Role |
|---|---|---|
| `MinutelyChargeSpec` and siblings | `internal/platform/scheduler/scheduler.go:20` | Charge crons (minute, hour, month) |
| `nativeProvider.Charge` | `internal/platform/billingprovider/billingprovider.go:72` | In-process loop, or RabbitMQ fan-out when enabled |
| `chargeCycle` | `internal/platform/billingjob/billingjob.go:326` | The cycle of a pass: a minute or hour pass on the 1st at 00:00 closes the month before |
| `Service.Charge`, `ChargeProfileByID` | `internal/platform/billingjob/billingjob.go:56`, `:104` | Per active billing profile |
| `Service.chargeBillingResource` | `billingjob.go:128` | Per provider: resolve resources, select plans and rules, rate |
| `Service.billingResources` | `billingjob.go:252` | Every active project of the profile bound to the provider |
| `GetBillingResources`, `stampResourceValues` | `internal/cloud/billingresource/billingresource.go:26`, `:55` | Cache rows to billing resources via the type registry; stamps region, service id, createdAt |
| Registry | `cmd/api/main.go:407` | Server, volume, floating IP, K8s cluster, DB cluster, bucket. A load balancer bills as the VM, disk and address it is made of |
| `ServerProvider.GetBillingInformation` | `internal/cloud/billingresource/server.go:34` | `instance` unit (flavor, GPU) plus `instance_traffic` unit; not billed in DELETED, ERROR, UNKNOWN, BUILD |
| `SelectPricePlansForService`, `ApplicableRules` | `internal/platform/pricing/planselect.go:46`, `:60` | Profile plans plus public plans for the provider, filtered by time unit |
| `ChargeBillingResources` | `internal/platform/pricing/charge.go:26` | Under the profile lock: current bill, created on demand, rate each resource, adjustments, save |
| `Engine.ApplyPricePlanRules` | `internal/platform/pricing/engine.go:30` | Filters, tiers, modifiers per rule |
| `SaveChargingToBill`, `getTimeUnitsDiff` | `internal/platform/pricing/bill_build.go:28`, `:155` | Bill item upsert; units elapsed since the last rating |
| `Service.billAdjuster` | `billingjob.go:165` | Savings-contract discounts and price-adjustment rules after rating |

```mermaid
flowchart LR
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a

  cron["charge cron"]:::impl
  bj["billingjob<br/>per profile, per provider"]:::impl
  cache["cloud.Repo rows"]:::impl
  brp["billingresource registry"]:::impl
  plans["price plans and rules"]:::impl
  eng["pricing.Engine"]:::impl
  bill["current bill<br/>items and adjustments"]:::impl

  cron --> bj
  bj --> brp
  brp --> cache
  bj --> plans
  brp -->|"BillingResource"| eng
  plans --> eng
  eng -->|"rule results"| bill
```

Public IPs from the Proxmox pool and project templates are not billing units yet
(Planned); they are bounded by quota (`floatingIps`, `diskGb`) in the meantime.

---

## C4.5 Sandboxes: the usage path and the release gates

| Symbol | Location | Role |
|---|---|---|
| `Handler.sandboxBillingGate` | `internal/platform/project/cloud_sandbox.go` | `refuseNewUsage` (deleting or DISABLED, so an operator's disable and a billing suspension alike), then the billing profile not SUSPENDED |
| `Handler.sandboxReleaseGate` | `cloud_sandbox.go` | The three release gates, cached per provider for a minute, fail closed |
| `ExternalService.SandboxProjectQuota` | `internal/platform/externalservice/sandbox.go` | Per key: the project's value, else `config.sandbox.projectQuota`, else 5 sandboxes / 16 vCPU / 32768 MB / 0 GPU |
| `ProjectQuota.check` | `internal/cloud/sandbox/quota.go` | Live sandboxes plus the new one against the project quota (429); the provider's per-sandbox limits apply first |
| `Job.SandboxReleaseGates` | `internal/cloud/syncjob/sandboxgate.go` | NodeRestriction, isolation and fence, read through the Proxmox provider of the workers (`config.sandbox.proxmoxServiceId`) |
| `probeNodeRestriction` | `sandboxgate.go` | Two dry-run Node creates impersonating a node: plain must pass, with a `node-restriction.kubernetes.io/` label must be refused by NodeRestriction |
| `Service.CheckSandboxIsolation` | `internal/cloud/pve/network.go` | Datacenter firewall on and this portal's isolation DROPs exactly those `ReconcileIsolation` writes for `sandboxCidrs` |
| `Service.CheckSandboxFence` | `internal/cloud/pve/sandboxfw.go` | Read-only: the current generation's sets and group exist unchanged, every untainted Node is a pool VM whose firewall reads back in place and whose filter is pinned to its address |
| `Handler.DeleteProjectSandboxes` | `cloud_sandbox.go` | D9: called by `billingCloudSuspender` on suspend; deletes every live sandbox and ends its row |
| `Handler.teardownSandboxes` | `cloud_sandbox.go` | Project deletion: delete, end and archive every sandbox row, then `DeleteProjectNamespace` |

Allowed while the project is DISABLED: list, `GET_LOGS`, delete. Refused: create, `EXEC`, `SET_TIMEOUT`
(the allow-list of `cloudAction` and the gate above), `REFRESH`.

```mermaid
flowchart TB
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a

  req["create / EXEC / SET_TIMEOUT"]:::impl
  susp{"refuseNewUsage, profile SUSPENDED?"}
  r409["409 / 403"]:::impl
  gates{"release gates hold?<br/>(cached 1 min)"}
  r503["503 not available"]:::impl
  quota{"create: live usage + new > project quota?"}
  r429["429 quota exceeded"]:::impl
  ok["Sandbox CR / exec / patch"]:::impl
  nr["NodeRestriction<br/>dry-run Node as a node"]:::impl
  iso["isolation<br/>cluster firewall DROPs"]:::impl
  fen["fence<br/>VM firewall, pinned filter"]:::impl

  req --> susp
  susp -->|"yes"| r409
  susp -->|"no"| gates
  gates -->|"no"| r503
  gates -->|"yes"| quota
  quota -->|"yes"| r429
  quota -->|"no"| ok
  gates --- nr
  gates --- iso
  gates --- fen
```

## Sequence (a): create server

```mermaid
sequenceDiagram
  autonumber
  participant SPA as web/client
  participant H as project handlers
  participant PS as pve.Service
  participant PVE as Proxmox API
  participant R as cloud.Repo
  participant SJ as syncjob
  participant BJ as billingjob

  SPA->>H: POST /project/{id}/cloud type SERVER
  H->>H: flavor, template, project network required
  H->>H: GPU quota, then enforcePVEQuota (503 if usage unreadable)
  H->>PVE: storage detection (storage + cluster resources), template config
  H->>H: profile refuses boot disks (capacity class or bootDisk false): 409
  H->>H: profile nodes = nodes where its storage is active (409 when none)
  H->>H: local template or local profile storage: pin to the template node (409 when the profile is not active there)
  H->>PVE: nodes, cluster resources (vm)
  H->>H: PickNode (capacity, allowed nodes, free GPUs, profile storage free >= disk + headroom)
  H->>PS: ServerSecurityGroups (ensure default group)
  H->>PS: CreateServer
  PS->>PVE: NextID, full clone (target node, profile storage) into project pool, wait
  PS->>PVE: read config
  PS->>PVE: set config: cores, memory, tags, net0 on vnet with firewall=1, drop extra NICs
  PS->>PVE: resize boot disk
  PS->>PVE: add security group rules
  PS->>PVE: SecureVM: ipfilter-net0, firewall options
  alt firewall step failed
    PS->>PVE: destroy VM
    PS-->>H: error (502)
  end
  PS->>PVE: start VM
  PS-->>H: cache row
  H->>R: insert row, audit
  H-->>SPA: 200 resource
  SJ->>PVE: every 15 min, pool VMs
  SJ->>R: Reconcile rows
  BJ->>R: every minute, GetBillingResources
  BJ->>BJ: instance and traffic units, rate, bill item
```

## Sequence (b): create network

```mermaid
sequenceDiagram
  autonumber
  participant SPA as web/client
  participant H as pveCreateNetwork
  participant L as sdn-apply lock
  participant PS as pve.Service
  participant PVE as Proxmox API
  participant R as cloud.Repo

  SPA->>H: POST cloud type NETWORK, name, cidr optional
  H->>H: enforcePVEQuota networks 1
  H->>L: lock (409 if busy)
  H->>PS: SubnetCIDRs
  PS->>PVE: list vnets and subnets, all projects
  H->>H: pvePickCIDR inside cidrPool, no overlap
  alt isolation evpn-per-project
    H->>PS: EnsureProjectZone ZoneIDFor(projectId)
  else shared
    H->>PS: EnsureZone default zone
  end
  H->>PS: CreateNetwork: stage vnet and subnet
  H->>PS: ReconcileIsolation (cluster firewall rules)
  PS->>PVE: reconcile cluster firewall rules
  opt reconcile failed
    H->>PS: unstage vnet
    H->>L: unlock
    H-->>SPA: 502, stop
  end
  H->>PS: ApplyAndWait
  PS->>PVE: PUT /cluster/sdn, wait task
  opt apply failed
    H->>PS: unstage vnet, reconcile, apply again
    H->>L: unlock
    H-->>SPA: 502, stop
  end
  H->>L: unlock
  H->>R: insert NETWORK row, audit
  H-->>SPA: 200 resource
```

## Sequence (c): project teardown

Triggered either by the operator's delete-project-now action (the `Teardown`
callback wired in `cmd/api/main.go`, run in the background) or by the scheduled
deletion job (`internal/platform/project/deletion.go`, through the
`projectTeardownDeleter` adapter in `cmd/api/main.go`). Both run `TeardownProject`
(`internal/platform/project/teardown.go:76`). Every Proxmox leg is idempotent, so a
failed teardown reports incomplete and can be re-run.

```mermaid
sequenceDiagram
  autonumber
  participant OP as Operator
  participant T as TeardownProject
  participant PS as pve.Service
  participant PVE as Proxmox API
  participant L as sdn-apply lock
  participant IP as PublicIPRepo
  participant R as cloud.Repo

  OP->>T: deletion job sweep (flagged by delete now, or past its grace window)
  T->>T: sandboxes first (delete, end and archive the rows, drop sb-<project>), then managed K8s and DBaaS rows
  T->>PS: TeardownProjectPool
  PS->>PVE: stop and destroy every VM in the pool, delete pool
  T->>L: lock
  T->>PS: TeardownProjectNetworks
  PS->>PVE: remove subnets, vnets (staged), project isolation rule
  T->>PS: DeleteProjectZoneIfEmpty (staged)
  opt anything staged
    T->>PS: ApplyAndWait
  end
  T->>PS: DeleteProjectSecurityGroups
  T->>L: unlock
  T->>IP: ReleaseProject
  T->>R: archive the project's rows
  T->>T: mark project DELETED (or stay DELETE_IN_PROGRESS for the next sweep)
```

## Sequence (d): per-org SSO login

The guard runs in `pkg/auth.Enforce` before any handler, so a refused identity never gets a
ProxCloud user, whichever route it calls first. Keycloak runs before ProxCloud sees anything, so
an IdP asserting a foreign address does get a Keycloak account; `SweepForeignUsers` removes those.

```mermaid
sequenceDiagram
  autonumber
  participant U as User
  participant SPA as web/client
  participant API as proxcloud-api
  participant KC as Keycloak customer realm
  participant IDP as Tenant IdP

  U->>SPA: enter email
  SPA->>API: POST /api/v1/auth/sso/discover
  API-->>SPA: alias of the IdP for a verified domain, nothing else
  SPA->>KC: authorize with kc_idp_hint, login_hint
  KC->>IDP: OIDC or SAML brokering
  IDP-->>KC: assertion with email and groups
  KC->>KC: stock first-broker-login, no silent account linking
  KC-->>SPA: tokens with idp, idp_groups, email_verified, scope proxcloud-sso
  SPA->>API: POST /user
  API->>API: guard: IdP active, verified email in its org's domains, account not bound elsewhere
  alt refused
    API-->>SPA: 403 sso_* and no user row
    SPA-->>U: explains why, offers sign out
  else accepted
    API->>API: bind user to the IdP, JIT membership, role from group mapping, never admin
    API-->>SPA: user
    SPA->>API: later requests
    API->>API: guard again, plus enforce-SSO and break-glass
  end
```
