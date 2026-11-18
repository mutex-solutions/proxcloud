// Managed services: /admin/platform (internal/platform/admin/platform.go), enough to walk the whole page without a
// cluster. The first apply run fails at "seed-api" with seed_api_unreachable and every later one succeeds, so Retry has
// something to do; the preflight fails "credentials" until both token secrets are stored. A run moves one stage per
// STAGE_MS of wall time. Like the API, `rev` moves only when the settings are saved, and every write refuses a body
// field the API's structs do not have. `?mock-platform=<preset>` in the URL starts the page in one state:
// disabled | draft | isolation (the isolation check fails until a provider sync) | norunner (runs answer 409) |
// running | waiting (stage 4 is trying again) | failed | dead (the run's job died: the stored phase stays applying) |
// ready | previous (ready, installed by the release before this one: the plan names a newer release and why this one does not move the platform to it) |
// upgrade (the same, and the move is allowed: the plan lists it, the run wants the typed confirmation) |
// replace (the same, with an unfinished upgrade toward the release of an earlier ProxCloud: the plan also offers the move to this one's in its place) |
// onenode (nothing saved, and the provider has one node, pve1, with 32 GiB of which 24 are free: the presets on a small site) |
// onechart (nothing saved, and this release's cluster chart builds one control plane machine) | degraded (one component out of sync) | simplezone (the check warns: no tenant cluster on a simple zone) | silent (the seed does not answer) | unread (the live read fails) | removing
// (none: enabled, nothing saved).
import { ApiError } from "@/lib/api"
import { on } from "../router"
import { db } from "../db"

type Doc = Record<string, any>

const STAGE_MS = 1500
const APPLY = ["validate", "images", "seed-vm", "seed-api", "seed-identity", "seed-platform", "cluster", "workload-access", "workload-platform", "register", "done"]
const REMOVE = ["remove-cluster", "remove-seed", "remove-secrets"]
const PROGRESS: Record<string, string> = {
  images: "2 of 2 images imported on pve1",
  "seed-vm": "VM cloned, starting",
  cluster: "machines 3/4 running, control plane 1/1",
  "workload-platform": "wave 1: metallb Healthy, proxmox-csi Progressing",
  "remove-cluster": "machines 2/4 deleted",
}
const SERVICE = "svc-proxmox-01"

const preset = typeof window === "undefined" ? "" : (new URLSearchParams(window.location.search).get("mock-platform") ?? "")

export const mockPlatform = {
  enabled: preset !== "disabled",
  installations: [] as Doc[],
  secrets: {} as Record<string, { capi?: boolean; csi?: boolean }>, // which secrets are stored; never their values
  runs: [] as Doc[],
  applies: 0,
  synced: false, // a provider sync ran (handlers/setup.ts): the isolation preset's check passes after it
  probes: {} as Record<string, Doc>, // the last tenant path test per installation; it ends two reads after its start
}

const fail = (status: number, code: string, message: string, details: Doc = {}) => new ApiError(status, status, message, { code, ...details })
const now = () => new Date().toISOString()
const TERMINAL = ["succeeded", "failed", "cancelled"]

// The API decodes every write with DisallowUnknownFields: strict refuses a key its struct does not have. A shape names
// the keys; true is a value, an object a nested struct, [shape] a list of them.
type Shape = { [k: string]: true | Shape | [Shape] }
function strict(v: unknown, shape: Shape, at = "") {
  if (v === null || typeof v !== "object") return
  for (const [k, x] of Object.entries(v)) {
    const want = shape[k]
    if (!want) throw new ApiError(400, 400, `Invalid request body: unknown field "${at}${k}"`)
    if (Array.isArray(want)) for (const item of Array.isArray(x) ? x : []) strict(item, want[0], `${at}${k}.`)
    else if (want !== true) strict(x, want, `${at}${k}.`)
  }
}
const MACHINE: Shape = { node: true, nodes: true, replicas: true, cores: true, memoryMiB: true, diskGiB: true }
const INSTALLATION: Shape = {
  name: true, preset: true, serviceId: true, provisionedBy: true, layout: true, rev: true,
  spec: {
    sandbox: { placement: true },
    pveApiUrl: true, storage: { boot: true, iso: true, import: true },
    network: { mode: true, bridge: true, vlan: true, prefix: true, gateway: true, dns: true, mtu: true },
    seed: { mode: true, pool: true, node: true, address: true, cores: true, memoryMiB: true, diskGiB: true, sshKeys: true, apiSources: true },
    clusters: [{ name: true, roles: true, endpoint: true, nodeRange: true, metallbRange: true, controlPlane: MACHINE,
      nodeGroups: [{ name: true, nodes: true, count: true, cores: true, memoryMiB: true, diskGiB: true }] }],
    nodeImage: { mode: true, templates: true },
  },
}
const TOKEN: Shape = { tokenId: true, secret: true }

export const mockSpec: Doc = {
  pveApiUrl: "https://10.90.0.2:8006",
  storage: { boot: "local-zfs", iso: "local", import: "local" },
  network: { bridge: "vmbr1", prefix: "10.90.0.0/24", gateway: "10.90.0.1", dns: ["10.90.0.1"] },
  seed: { mode: "vm", pool: "proxcloud-seed", node: "pve1", address: "10.90.0.5", cores: 2, memoryMiB: 4096, diskGiB: 30, apiSources: ["192.0.2.10/32"] },
  clusters: [{
    name: "pc-platform", roles: ["kamaji", "dbaas"], endpoint: "10.90.0.10", nodeRange: "10.90.0.20-10.90.0.60", metallbRange: "10.90.0.100-10.90.0.150",
    controlPlane: { node: "pve1", replicas: 1, cores: 2, memoryMiB: 4096, diskGiB: 30 },
    nodeGroups: ["system", "kamaji", "dbaas"].map((name) => ({ name, nodes: ["pve1"], count: 1, cores: 2, memoryMiB: 4096, diskGiB: 40 })),
  }],
  nodeImage: { mode: "bom" },
}

const bom = {
  schemaVersion: 1, release: "0.7.0", sha256: "9f2c4e1ab7d05566c1e0a4b3f8d7e6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9",
  kubernetes: { version: "v1.33.4", nodeImage: { package: "proxcloud-vm-k8s-node", tag: "v1.33.4-1", digest: "sha256:4b1d7c0e9a2f55e1c3a8b6d4f2e0a1c3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f3e5" } },
  seed: { image: { package: "proxcloud-vm-seed", tag: "0.7.0", digest: "sha256:a7c3e5b1d9f20c4e6a8b0d2f4c6e8a0b2d4f6c8e0a2b4d6f8c0e2a4b6d8f0c2e" } },
  components: [
    { name: "argo-cd", cluster: "seed", wave: 0, group: "", chart: { version: "8.3.0", digest: "sha256:11aa22bb33cc44dd55ee66ff77008899aabbccddeeff00112233445566778899" } },
    { name: "cluster-api-operator", cluster: "seed", wave: 1, group: "", chart: { version: "0.22.0", digest: "sha256:22bb33cc44dd55ee66ff77008899aabbccddeeff0011223344556677889900aa" } },
    { name: "metallb", cluster: "workload", wave: 1, group: "all", chart: { version: "0.15.2", digest: "sha256:33cc44dd55ee66ff77008899aabbccddeeff0011223344556677889900aabb11" } },
    { name: "proxmox-csi", cluster: "workload", wave: 1, group: "all", chart: { version: "0.3.12", digest: "sha256:44dd55ee66ff77008899aabbccddeeff0011223344556677889900aabb1122cc" } },
    { name: "kamaji", cluster: "workload", wave: 2, group: "kamaji", chart: { version: "1.0.0", digest: "sha256:55ee66ff77008899aabbccddeeff0011223344556677889900aabb1122cc33dd" } },
    { name: "cloudnative-pg", cluster: "workload", wave: 2, group: "dbaas", chart: { version: "0.26.0", digest: "sha256:66ff77008899aabbccddeeff0011223344556677889900aabb1122cc33dd44ee" } },
    // The cluster chart, last so that the rows above keep their places: 0.1.14 builds one or three control plane machines.
    { name: "pc-platform", cluster: "seed", wave: 3, group: "", chart: { version: "0.1.14", digest: "sha256:77008899aabbccddeeff0011223344556677889900aabb1122cc33dd44ee55ff" } },
  ],
}
const bomRef = { release: bom.release, sha256: bom.sha256 }
// The preset "previous": the release that installed the platform, which this one carries as a previous BOM.
// Its cluster chart is the one before 0.1.14: it builds one control plane machine.
const previous = { ...bom, release: "0.6.0", sha256: "1e0d9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d",
  components: bom.components.map((c) => (c.name === "pc-platform" ? { ...c, chart: { ...c.chart, version: "0.1.13" } } : c)) }
if (preset === "onechart") bom.components = previous.components
if (preset === "onenode") db.proxmoxAuthResult.nodes = [{ node: "pve1", status: "online", maxmem: 32 * 2 ** 30, mem: 8 * 2 ** 30 }]

const find = (id: string) => {
  if (!mockPlatform.enabled) throw fail(404, "platform_disabled", "Managed services are off on this site: set PROXCLOUD_PLATFORM_ENABLED=true")
  const in_ = mockPlatform.installations.find((i) => i.id === id)
  if (!in_) throw fail(404, "not_found", "installation not found")
  return in_
}
const view = (in_: Doc) => {
  const sec = mockPlatform.secrets[in_.id] ?? {}
  return { ...in_, credentials: { ...(sec.capi ? { capi: { tokenId: "capmox@pve!capi" } } : {}), ...(sec.csi ? { csi: { tokenId: "csi@pve!csi" } } : {}) } }
}
const activeRun = (id: string) => mockPlatform.runs.find((r) => r.installationId === id && !TERMINAL.includes(tick(r).state))

function seedStatus(in_: Doc) {
  if (in_.status.seed?.readyAt) return
  in_.status.seed = { vmid: 9100, node: in_.spec.seed.node, generation: 1, apiServer: `https://${in_.spec.seed.address}:6443`, tokenSecret: "proxcloud-portal-token-g1", k3sVersion: "v1.33.4+k3s1", readyAt: now() }
}

function newRun(in_: Doc, kind: string, startedAt = Date.now()): Doc {
  const failAt = kind === "apply" && ++mockPlatform.applies === 1 ? "seed-api" : ""
  const run = {
    id: db.nextId("run"), installationId: in_.id, kind, bom: bomRef, requestedBy: "admin@example.com", state: "queued", createdAt: new Date(startedAt).toISOString(),
    stages: (kind === "apply" ? APPLY : REMOVE).map((id) => ({ id, state: "pending", retries: 0 })),
    t0: startedAt, failAt,
  }
  mockPlatform.runs.unshift(run)
  in_.lastRunId = run.id
  in_.status.phase = kind === "apply" ? "applying" : "removing"
  return run
}

// tick moves a run to where the clock says it is, and its installation with it.
function tick(run: Doc): Doc {
  if (TERMINAL.includes(run.state) || run.dead) return run
  const in_ = mockPlatform.installations.find((i) => i.id === run.installationId)!
  const end = (state: string, phase: string, code?: string, message?: string) => {
    Object.assign(run, { state, code, message, finishedAt: now() })
    in_.status.phase = phase
  }
  if (run.cancelRequested && Date.now() >= run.cancelAt) {
    for (const s of run.stages) if (s.state === "running") s.state = "pending"
    end("cancelled", "failed")
    return run
  }
  const at = Math.floor((Date.now() - run.t0) / STAGE_MS)
  run.state = "running"
  run.startedAt ??= new Date(run.t0).toISOString()
  for (const [i, s] of run.stages.entries()) {
    const t = (n: number) => new Date(run.t0 + n * STAGE_MS).toISOString()
    if (i > at) break
    Object.assign(s, { startedAt: t(i), deadline: t(i + 1800), progress: PROGRESS[s.id] })
    if (s.id === "validate") s.items = checks(in_).map((c) => ({ id: c.id, state: c.status, message: c.message }))
    if (s.id === "seed-vm") s.receipt = { vmid: 9100, name: `pc-seed-${in_.id}`, generation: 1 }
    if (s.id === run.failAt) {
      Object.assign(s, { state: "failed", finishedAt: t(i + 1), retries: 3, code: "seed_api_unreachable", message: `The seed's API at ${in_.spec.seed.address}:6443 did not answer in time: dial tcp: i/o timeout.` })
      end("failed", "failed", s.code, s.message)
      return run
    }
    if (s.id === run.waitAt) { // the stage keeps trying: the runner stores why on the waiting stage
      Object.assign(s, { state: "waiting", retries: 2, code: "seed_api_unreachable", message: `The seed's API at ${in_.spec.seed.address}:6443 does not answer yet: dial tcp: i/o timeout.` })
      run.state = "waiting"
      return run
    }
    if (i < at) Object.assign(s, { state: "done", finishedAt: t(i + 1) })
    else s.state = "running"
    if (s.id === "seed-identity" && s.state === "done") seedStatus(in_)
  }
  if (at >= run.stages.length) {
    if (run.kind === "remove") {
      end("succeeded", "removed")
      in_.status = { phase: "removed", providers: in_.status.providers } // what was built is forgotten; the provider rows stay
      delete mockPlatform.secrets[in_.id]
      for (const s of db.services) if (s.config?.platformInstallationId === in_.id) s.status = "DISABLED"
    } else {
      end("succeeded", "ready")
      ready(in_)
      if (run.fromBom) { // an upgrade ends here: the record moves to the target
        in_.status.bom = run.bom
        delete in_.status.targetBom
      }
    }
  }
  return run
}

// settleDead ends a run whose job died, as the API does before the next run starts or when the run is cancelled.
function settleDead(in_: Doc) {
  for (const run of mockPlatform.runs) {
    if (run.installationId !== in_.id || !run.dead || TERMINAL.includes(run.state)) continue
    Object.assign(run, { state: "failed", code: "job_dead", finishedAt: now() })
    in_.status.phase = "failed"
  }
}

function ready(in_: Doc) {
  const provider = (kind: string, label: string) => {
    let svc = db.services.find((s) => s.config?.platformInstallationId === in_.id && s.config?.provider === kind)
    if (!svc) {
      svc = { id: db.nextId("svc"), name: `${in_.name} ${label}`, type: "CLOUD", status: "ACTIVE", config: { provider: kind, platformInstallationId: in_.id, attachToNewProjects: false, ...(kind === "kamaji" ? { cluster: { versions: {} } } : {}) } }
      db.services.push(svc)
    }
    return svc.id as string
  }
  seedStatus(in_)
  Object.assign(in_.status, {
    phase: "ready", bom: bomRef,
    clusters: [{ name: "pc-platform", namespace: "pc-platform", endpoint: in_.spec.clusters[0].endpoint, kubernetesVersion: bom.kubernetes.version, ...(in_.spec.clusters[0].controlPlane.replicas > 1 ? { controlPlaneReplicas: in_.spec.clusters[0].controlPlane.replicas } : {}), argocdTokenExpiresAt: new Date(Date.now() + 365 * 86_400_000).toISOString() }],
    components: bom.components.map((c) => ({ name: c.name, cluster: c.cluster, version: c.chart.version, managed: true })),
    providers: { kubernetes: provider("kamaji", "Kubernetes"), database: provider("dbaas", "Databases") },
  })
}

// The capacity row (internal/platformlife/capacity.go), with the nodes' free memory in place of the scheduler's count:
// a node without room for its machines and the control plane's one more of an update fails the row before the first
// install and warns after it; one without room for the groups' spare machines as well warns. Absent while the CAPI
// token is not stored.
function capacity(in_: Doc, stored: boolean): Doc[] {
  if (!stored) return []
  const planned: Record<string, number> = {}, spare: Record<string, number> = {}
  const add = (node: string, n: number, mib: number) => { planned[node] = (planned[node] ?? 0) + n * mib }
  const c = in_.spec.clusters[0]
  add(in_.spec.seed.node, 1, in_.spec.seed.memoryMiB)
  add(c.controlPlane.node, (c.controlPlane.replicas || 1) + 1, c.controlPlane.memoryMiB)
  for (const g of c.nodeGroups) g.nodes.forEach((n: string, i: number) => {
    add(n, Math.floor(g.count / g.nodes.length) + (i < g.count % g.nodes.length ? 1 : 0), g.memoryMiB)
    spare[n] = (spare[n] ?? 0) + g.memoryMiB
  })
  const short: string[] = [], tight: string[] = []
  for (const [node, mib] of Object.entries(planned).sort()) {
    const at = (db.proxmoxAuthResult.nodes as Doc[]).find((n) => n.node === node)
    const room = at ? Math.floor((at.maxmem - at.mem) / 2 ** 20) : 0
    if (mib > room) short.push(`Node ${node}: the settings place ${mib / 1024} GiB of memory there, and ${room / 1024} GiB can be reserved.`)
    else if (mib + (spare[node] ?? 0) > room) tight.push(`Node ${node} holds the machines (${mib / 1024} GiB of ${room / 1024} GiB), and not the spare machine of every group on it as well (${(spare[node] ?? 0) / 1024} GiB more): a change that replaces a group's machines there needs that room, or the new machine is refused and not tried again.`)
  }
  if (!short.length && tight.length) return [{ id: "capacity", status: "warn", message: tight.join("\n") }]
  if (!short.length) return [{ id: "capacity", status: "ok", message: "Every node has the memory for its machines and their spares during an update." }]
  return [{ id: "capacity", status: in_.status.bom ? "warn" : "fail", message: `${short.join("\n")}\nA machine that does not fit is refused and not tried again, at the install or at the first update of the control plane.` }]
}

function checks(in_: Doc): Doc[] {
  const sec = mockPlatform.secrets[in_.id] ?? {}
  const both = !!sec.capi && !!sec.csi
  const iso = in_.spec.storage.iso
  const row = (id: string, ok: boolean, done: string, todo: string) => ({ id, status: ok ? "ok" : "fail", message: ok ? done : todo })
  return [
    row("flags", true, "Managed services and VM template updates are on, and a GitHub integration is saved.", ""),
    row("spec", true, "The installation is complete and consistent.", ""),
    row("network", true, "The platform network is apart from every tenant, sandbox and public network, and tenants can reach the MetalLB range.", ""),
    row("isolation", preset !== "isolation" || mockPlatform.synced, "The platform addresses are blocked networks; the cluster firewall rules are written by the next provider sync (with this installation saved it needs no project), or with the first tenant network.",
      "The seed address 10.90.0.5 and the API address 10.90.0.10 are not among the provider's blocked networks: add 10.90.0.5/32 and 10.90.0.10/32."),
    ...(preset === "simplezone" ? [{ id: "tenant-network", status: "warn", message: "The provider's default zone pcm0 has DHCP in Proxmox. A tenant Kubernetes cluster needs a network without DHCP, so a cluster create on a network of this zone is refused (400, \"Worker nodes need a network without DHCP\") until the default zone is an EVPN or a VLAN zone (config.network.zones and defaultZone, under Cloud providers). A Kubernetes provider with shared workers (cluster.shared) uses no tenant network and is not affected. The install and managed databases do not need it." }] : []),
    ...capacity(in_, both),
    row("credentials", both, "Both platform tokens are stored.", `No secret is stored for ${[!sec.capi && "capmox@pve!capi", !sec.csi && "csi@pve!csi"].filter(Boolean).join(" and ")}.`),
    ...["capi", "csi"].map((t) => row(`privileges:${t}`, both, `The ${t} token holds its privileges.`, `The ${t} token could not be checked: no secret is stored.`)),
    row("privileges:kubemint", true, "The portal token holds the ProxCloudKubeMint privileges.", ""),
    row("mint-scope", true, "The portal token may create tokens for the users of the group proxcloud-kube alone, and both are in it.", ""),
    row("pool-fence", true, "Neither the portal token nor csi@pve!csi reaches the seed's pool, and the portal token does not reach proxcloud-system.", ""),
    row("token-scope", true, "Neither platform token holds anything on /pool, /storage or /vms as a whole.", ""),
    row("iso-store-visibility", both, `capmox@pve!capi sees one ISO store, ${iso}.`, "The ISO stores capmox@pve!capi sees could not be read: no secret is stored."),
    row("boot-iso-grant", true, `capmox@pve may upload and delete bootstrap ISOs on ${iso}.`, ""),
    row("boot-iso", true, `${iso} is node-local, takes ISOs, and is the only ISO store of the boot storage.`, ""),
    row("placement", true, "Every node an image goes to has the import and the boot store.", ""),
    row("images", true, "Neither BOM image is on the blocklist.", ""),
  ]
}

// The import store the API fills in when none is named: the provider's.
const importOf = (serviceId: string, spec: Doc) => spec?.storage?.import || db.services.find((s) => s.id === serviceId)?.config?.importStorage || "local"

// The API's own rules, as far as the page's states need them: the name, a rule only the server can know, and the two
// of Validate (internal/platformlife/installation.go) the form cannot or does not check.
function refuse(name: string, spec: Doc, serviceId: string) {
  const problems: string[] = []
  const imp = importOf(serviceId, spec)
  if (imp === spec?.storage?.boot) problems.push(`spec.storage.import "${imp}" is the boot store: use another store with import content, the one the provider's importStorage names (an empty spec.storage.import takes it)`)
  if (spec?.seed?.mode === "existing") problems.push("spec.seed.mode existing comes in a later release; use vm")
  if (!/^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$/.test(name ?? "")) problems.push(`name "${name}": lower-case letters, digits and dashes, at most 40`)
  const cp = spec?.clusters?.[0]?.controlPlane ?? {}
  if (![undefined, 0, 1, 3].includes(cp.replicas)) problems.push(`spec.clusters[0].controlPlane.replicas ${cp.replicas}: 1 or 3 (etcd needs an odd number of members, and this release builds no more than 3)`)
  if (cp.nodes?.length && JSON.stringify(cp.nodes) !== JSON.stringify([cp.node])) problems.push("spec.clusters[0].controlPlane.nodes: every control plane machine goes to one Proxmox node, controlPlane.node. Machines on several nodes need the node image template and the control plane disks on shared storage (CAPMOX v0.9.2 clones one template, and Proxmox moves a clone to another node only from and to shared storage), and this release does not build that: leave controlPlane.nodes out")
  if (spec?.sandbox) problems.push("spec.sandbox: the sandbox service on the platform comes in a later release; leave spec.sandbox out")
  if (String(spec?.network?.prefix ?? "").startsWith("10.100.")) problems.push(`the platform network ${spec.network.prefix} overlaps cidrPool 10.100.0.0/16`)
  if (problems.length) throw fail(400, "invalid_installation", "The installation is not valid", { problems })
}

function create(name: string, spec: Doc): Doc {
  const t = now()
  const in_ = { id: `pi-${(mockPlatform.installations.length + 1).toString(16).padStart(8, "0")}`, name, serviceId: SERVICE, provisionedBy: "portal", layout: "shared",
    spec: structuredClone(spec), status: { phase: "draft" }, createdBy: "admin@example.com", createdAt: t, updatedAt: t, rev: 1 } as Doc
  mockPlatform.installations.push(in_)
  return in_
}

if (preset && !["disabled", "onenode", "onechart"].includes(preset)) {
  const in_ = create("lab", mockSpec)
  if (preset !== "draft") mockPlatform.secrets[in_.id] = { capi: true, csi: true }
  if (preset === "running") { mockPlatform.applies = 1; newRun(in_, "apply", Date.now() - 6.5 * STAGE_MS) }
  if (preset === "waiting") { mockPlatform.applies = 1; newRun(in_, "apply", Date.now() - 6.5 * STAGE_MS).waitAt = "seed-api" }
  if (preset === "failed") tick(newRun(in_, "apply", Date.now() - 60_000))
  if (preset === "dead") {
    mockPlatform.applies = 1
    tick(newRun(in_, "apply", Date.now() - 3.5 * STAGE_MS)).dead = true
    in_.status.seed = { vmid: 9100, node: "pve1", generation: 1, cdVolume: "local:iso/pc-seed-pi-00000001.iso" }
  }
  if (["ready", "previous", "upgrade", "replace", "degraded", "silent", "unread", "removing"].includes(preset)) {
    mockPlatform.applies = 1
    tick(newRun(in_, "apply", Date.now() - 3_600_000))
    if (["previous", "upgrade", "replace"].includes(preset)) in_.status.bom = { release: previous.release, sha256: previous.sha256 }
    if (preset === "replace") in_.status.targetBom = { release: "0.6.5", sha256: "5".repeat(64) }
    if (preset === "removing") newRun(in_, "remove", Date.now() - 0.5 * STAGE_MS)
  }
}

on("GET /admin/platform/bom", ({ query }) => {
  const sha = query.get("sha256")
  const found = [bom, previous].find((b) => !sha || b.sha256 === sha)
  if (!found) throw fail(404, "bom_unsupported", "This release does not embed a BOM with that sha256.")
  return { data: found }
})

on("GET /admin/platform/installations", () => {
  if (!mockPlatform.enabled) throw fail(404, "platform_disabled", "Managed services are off on this site: set PROXCLOUD_PLATFORM_ENABLED=true")
  mockPlatform.runs.forEach(tick)
  return { data: mockPlatform.installations.map(view) }
})

on("POST /admin/platform/installations", ({ opts }) => {
  const b = (opts.body ?? {}) as Doc
  if (!mockPlatform.enabled) find("")
  strict(b, INSTALLATION)
  refuse(b.name, b.spec, b.serviceId)
  if (mockPlatform.installations.some((i) => i.serviceId === b.serviceId)) throw fail(409, "installation_exists", "this provider already has an installation")
  // What the API fills in: the seed's mode and pool, the cluster's name and roles, the node image mode, and the import
  // store of the provider when none is named.
  return { data: view(create(b.name, {
    ...b.spec,
    storage: { ...b.spec.storage, import: importOf(b.serviceId, b.spec) },
    seed: { mode: "vm", pool: "proxcloud-seed", ...b.spec.seed },
    clusters: (b.spec.clusters ?? []).map((c: Doc) => ({ name: "pc-platform", roles: ["kamaji", "dbaas"], ...c, controlPlane: { ...c.controlPlane, replicas: c.controlPlane?.replicas || 1 } })),
    nodeImage: { mode: "bom" },
  })) }
})

on("GET /admin/platform/installations/:id", ({ params, query }) => {
  const in_ = find(params.id)
  mockPlatform.runs.forEach(tick)
  if (query.get("live") !== "1") return { data: view(in_) }
  if (in_.status.phase !== "ready") return { data: { ...view(in_), live: { phase: in_.status.phase, components: [] } } }
  if (preset === "unread") throw new ApiError(500, 500, "The installation's secrets could not be read")
  if (preset === "silent") return { data: { ...view(in_), live: { phase: "degraded", components: [], code: "seed_api_unreachable", error: "The seed's API at https://10.90.0.5:6443 does not answer. The record still says ready: nothing stores this state. The platform cluster and what runs on it keep running, but an apply or a Remove waits on the seed and then fails. Check that seed VM 9100 runs on pve1 and open its console (qm terminal 9100): the guide's section 'When the seed does not answer' has the steps." } } }
  const components = bom.components.map((c) => ({ name: c.name, cluster: c.cluster, sync: "Synced", health: "Healthy" }))
  if (preset === "degraded") Object.assign(components[2], { sync: "OutOfSync", health: "Progressing" })
  return { data: { ...view(in_), live: { phase: preset === "degraded" ? "degraded" : "ready", components } } }
})

on("PUT /admin/platform/installations/:id", ({ params, opts }) => {
  const in_ = find(params.id)
  const b = (opts.body ?? {}) as Doc
  strict(b, INSTALLATION)
  if (b.rev !== in_.rev) throw fail(409, "rev_conflict", "the installation changed since it was read: read it again")
  if (activeRun(in_.id)) throw fail(409, "run_active", "a run of this installation is not finished: wait for it or cancel it")
  refuse(b.name, b.spec, in_.serviceId)
  // ControlPlaneCountProblems in internal/platformlife/installation.go: the previous release's chart builds one.
  if ((b.spec.clusters?.[0]?.controlPlane?.replicas || 1) > 1 && in_.status.bom?.sha256 === previous.sha256) {
    const why = `spec.clusters: 3 control plane machines: the platform's BOM (release ${previous.release}) names a cluster chart which builds one; more need chart 0.1.14 or newer, so keep 1 until the platform is on a release that has it`
    throw fail(400, "invalid_installation", "The installation is not valid", { problems: [why] })
  }
  if (in_.status.bom) { // FixedChanges in internal/platformlife/installation.go
    const same = (a: unknown, c: unknown) => JSON.stringify(a) === JSON.stringify(c)
    const [old, next] = [in_.spec.clusters[0], b.spec.clusters?.[0] ?? {}]
    const fixed = [
      b.name !== in_.name && "name",
      !same(b.spec.network, in_.spec.network) && "spec.network",
      ...["mode", "pool", "node", "address"].map((k) => b.spec.seed?.[k] !== in_.spec.seed[k] && `spec.seed.${k}`),
      b.spec.clusters?.length !== in_.spec.clusters.length && "spec.clusters",
      next.name !== old.name && "spec.clusters[0].name",
      next.endpoint !== old.endpoint && "spec.clusters[0].endpoint",
      !same((next.nodeGroups ?? []).map((g: Doc) => g.name), old.nodeGroups.map((g: Doc) => g.name)) && "spec.clusters[0].nodeGroups",
    ].filter(Boolean) as string[]
    if (fixed.length) throw fail(409, "fixed_fields", `These fields cannot change once the platform was installed (serviceId never): ${fixed.join(", ")}`, { problems: fixed })
  }
  { // ControlPlaneShrink in internal/platformlife/installation.go: against what a run built, not the stored spec
    const [had, wants] = [in_.status.clusters?.[0]?.controlPlaneReplicas || 1, b.spec.clusters?.[0]?.controlPlane?.replicas || 1]
    if (wants < had) {
      const why = `spec.clusters[0].controlPlane.replicas: a run built this control plane with ${had} machines, and it does not go from ${had} machines to ${wants} in this release. Taking etcd members out leaves two for a while, where one slow or lost member stops the platform's API, and no lab has run that step. Keep ${had}; to get a platform with one, remove the installation and install again.`
      throw fail(409, "fixed_fields", why, { problems: [why] })
    }
  }
  Object.assign(in_, { name: b.name, spec: structuredClone(b.spec), rev: in_.rev + 1, updatedAt: now() })
  return { data: view(in_) }
})

on("PUT /admin/platform/installations/:id/credentials", ({ params, opts }) => {
  const in_ = find(params.id)
  const b = (opts.body ?? {}) as Doc
  strict(b, { capi: TOKEN, csi: TOKEN })
  const uuid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/
  const problems = [["capi", "capmox@pve!capi"], ["csi", "csi@pve!csi"]].flatMap(([k, want]) =>
    !b[k] ? [] : b[k].tokenId !== want ? [`tokenId must be ${want}`] : !uuid.test(b[k].secret ?? "") ? [`the secret of ${want} is not a PVE token secret (a UUID)`] : [])
  if (!b.capi && !b.csi) problems.push("give capi, csi or both")
  if (problems.length) throw fail(400, "invalid_credentials", "The credentials are not valid", { problems })
  const sec = (mockPlatform.secrets[in_.id] ??= {})
  if (b.capi) sec.capi = true
  if (b.csi) sec.csi = true
  return { data: view(in_) } // the answer names the stored token ids, never a secret
})

on("POST /admin/platform/installations/:id/preflight", ({ params }) => {
  const rows = checks(find(params.id))
  return { data: { ok: rows.every((c) => c.status !== "fail"), checks: rows } }
})

// "Test the tenant path": refused before the first install, then running for two reads and passed.
on("POST /admin/platform/installations/:id/tenant-path-probe", ({ params, opts }) => {
  const in_ = find(params.id)
  const b = (opts.body ?? {}) as Doc
  strict(b, { node: true })
  if (!in_.status.bom) throw fail(409, "not_installed", "The platform is not installed yet, so nothing listens on the platform network and the path cannot be tested. Install first; until then the Check row tenant-path reads the layout.")
  const c = in_.spec.clusters[0]
  const lb = String(c.metallbRange).split("-")[0]
  const probe = { id: in_.id, state: "running", node: b.node || c.controlPlane.node, template: 9200, startedAt: now(), step: "cloning the test VM", reads: 0,
    targets: [{ address: lb, port: 6443, expect: "open" }, { address: c.endpoint, port: 6443, expect: "closed" }], message: "The test is running." }
  mockPlatform.probes[in_.id] = probe
  return { data: { ...probe } }
})
on("GET /admin/platform/installations/:id/tenant-path-probe", ({ params }) => {
  const p = mockPlatform.probes[find(params.id).id]
  if (!p) throw fail(404, "no_probe", "The tenant path of this installation was not tested yet.")
  if (p.state === "running" && ++p.reads >= 2) {
    Object.assign(p, { state: "passed", step: "", finishedAt: now(), vmid: 131, source: "10.100.255.242",
      targets: p.targets.map((t: Doc) => ({ ...t, result: t.expect === "open" ? "open" : "none" })),
      message: `The path is open: from a tenant network on ${p.node} (address 10.100.255.242, translated by the node as a tenant's is) a connection opened to ${p.targets[0].address}:6443, and the platform's own API address did not answer, as the isolation rules want. This holds for ${p.node}; a tenant on another node takes another path.` })
  }
  return { data: { ...p } }
})

on("GET /admin/platform/installations/:id/plan", ({ params }) => {
  const in_ = find(params.id)
  const installed = in_.status.bom ?? null
  if (installed?.sha256 === previous.sha256) {
    const pending = in_.status.targetBom
    return { data: { installed, target: pending ?? installed, newer: bomRef, changes: [], blocking: [], upgrade: upgradePlan(in_), ...(stale(in_) ? { replacement: upgradePlan(in_, true) } : {}) } }
  }
  return { data: { installed, target: bomRef, changes: [], blocking: [] } }
})

// The plan of the move from the previous release to this one, as platformlife.Classify answers it: the preset
// "upgrade" differs in the product's own charts alone, "previous" also in a third-party chart, which refuses it.
// stale: an upgrade toward another release than this one's has started and not finished. With `replace` the plan is
// the move to this release in its place.
const stale = (in_: Doc) => !!in_.status.targetBom && in_.status.targetBom.sha256 !== bom.sha256
function upgradePlan(in_: Doc, replace = false) {
  const release = { name: "release", from: previous.release, to: bom.release, class: "record only", impact: "Nothing is synced for it: the platform's record and the labels of its objects name the new release." }
  const chart = { cluster: "workload", name: "platform", from: "0.1.2", to: "0.1.3", class: "chart", stage: "workload-platform", wave: 3, impact: "ArgoCD syncs the chart's objects. No pod of a tenant restarts." }
  const cilium = { cluster: "workload", name: "cilium", from: "1.18.15", to: "1.18.16", class: "refused", blocking: "not supported by this release: a third-party chart is upgraded only by a later release (this one moves the product's own charts)" }
  const refused = preset !== "upgrade" && preset !== "replace"
  const inProgress = !!in_.status.targetBom && !replace
  return {
    from: { release: previous.release, sha256: previous.sha256 }, to: inProgress ? in_.status.targetBom : bomRef, allowed: !refused, ...(inProgress ? { inProgress } : {}),
    ...(refused || inProgress ? {} : { confirm: `upgrade ${in_.name} to ${bom.release}` }),
    changes: refused ? [release, chart, cilium] : [release, chart],
    blocking: refused ? [`workload/cilium: ${cilium.blocking}`] : [],
  }
}

on("POST /admin/platform/installations/:id/runs", ({ params, opts }) => {
  const in_ = find(params.id)
  if (preset === "norunner") throw fail(409, "platform_disabled", "Runs need the platform runner: set PROXCLOUD_PLATFORM_ENABLED=true and PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true")
  const b = (opts.body ?? {}) as Doc
  strict(b, { kind: true, confirm: true, targetBom: true })
  if (b.kind !== "apply" && b.kind !== "remove") throw fail(400, "invalid_kind", "kind is apply or remove")
  // An apply that names this release as its target is an upgrade: allowed as the plan says, with the plan's phrase.
  const up = b.kind === "apply" && b.targetBom && (!in_.status.targetBom || stale(in_)) ? upgradePlan(in_, stale(in_)) : null
  if (b.targetBom && (b.kind !== "apply" || b.targetBom !== bom.sha256)) throw fail(400, "invalid_target", "targetBom must be the sha256 of this build's own BOM")
  if (up && !up.allowed) throw fail(409, "upgrade_refused", `upgrade_refused: ${up.blocking.join("; ")}`, { reasons: up.blocking })
  if (up ? b.confirm !== up.confirm : b.confirm && (b.kind !== "remove" || b.confirm !== `remove ${in_.name}`)) throw fail(400, "invalid_confirmation", `confirm must be "remove ${in_.name}" on a remove, or the plan's phrase on an upgrade`)
  settleDead(in_) // a run whose job died is finished first
  if (activeRun(in_.id)) throw fail(409, "run_active", "a run of this installation is not finished")
  const run = newRun(in_, b.kind)
  // An upgrade, started or continued: the run goes from the installed release to the target, which the record keeps
  // until the run has succeeded (tick).
  if (up) in_.status.targetBom = bomRef
  if (b.kind === "apply" && in_.status.targetBom) run.fromBom = in_.status.bom
  return { data: { runId: run.id } }
})

const publicRun = (run: Doc) => {
  const { t0: _t0, failAt: _failAt, cancelAt: _cancelAt, waitAt: _waitAt, dead, ...out } = tick(run)
  if (dead && !TERMINAL.includes(out.state)) return { ...out, state: "failed", code: "job_dead" } // the view only: nothing is stored yet
  return out.cancelRequested && out.state !== "cancelled" ? { ...out, state: "cancelling" } : out
}
const findRun = (id: string, runId: string) => {
  const run = mockPlatform.runs.find((r) => r.id === runId && r.installationId === find(id).id)
  if (!run) throw fail(404, "not_found", "run not found")
  return run
}

on("GET /admin/platform/installations/:id/runs", ({ params }) => ({ data: mockPlatform.runs.filter((r) => r.installationId === find(params.id).id).map(publicRun) }))
on("GET /admin/platform/installations/:id/runs/:runId", ({ params }) => ({ data: publicRun(findRun(params.id, params.runId)) }))

on("POST /admin/platform/installations/:id/runs/:runId/cancel", ({ params }) => {
  const run = findRun(params.id, params.runId)
  if (run.dead) {
    settleDead(find(params.id))
    return { data: publicRun(run) }
  }
  if (TERMINAL.includes(tick(run).state)) throw fail(409, "run_finished", "the run is finished")
  Object.assign(run, { cancelRequested: true, cancelAt: Date.now() + STAGE_MS }) // honoured at the runner's next wake-up
  return { data: publicRun(run) }
})

on("GET /admin/platform/installations/:id/seed-kubeconfig", ({ params }) => {
  const in_ = find(params.id)
  if (!in_.status.seed?.readyAt) throw fail(409, "no_seed", "the installation has no ready seed")
  const kubeconfig = `apiVersion: v1\nkind: Config\nclusters:\n- name: pc-seed\n  cluster:\n    server: ${in_.status.seed.apiServer}\nusers:\n- name: pc-seed\n  user:\n    token: mock-break-glass-token\n`
  return { data: { kubeconfig, expiresAt: new Date(Date.now() + 3_600_000).toISOString() } }
})

// As the API: a token of the break-glass account on the platform cluster, never the admin kubeconfig the seed holds.
on("GET /admin/platform/installations/:id/platform-kubeconfig", ({ params }) => {
  const in_ = find(params.id)
  if (!in_.status.seed?.readyAt) throw fail(409, "no_seed", "this installation has no ready seed")
  const endpoint = in_.status.clusters?.[0]?.endpoint
  if (!endpoint) throw fail(409, "no_cluster", "this installation has no platform cluster yet")
  const kubeconfig = `apiVersion: v1\nkind: Config\nclusters:\n- name: pc-platform\n  cluster:\n    server: https://${endpoint}:6443\nusers:\n- name: pc-platform\n  user:\n    token: mock-platform-break-glass-token\n`
  return { data: { kubeconfig, expiresAt: new Date(Date.now() + 3_600_000).toISOString() } }
})

on("POST /admin/platform/installations/:id/seed-credential/rotate", ({ params }) => {
  const in_ = find(params.id)
  if (activeRun(in_.id)) throw fail(409, "run_active", "a run of this installation is not finished")
  if (!in_.status.seed?.readyAt) throw fail(409, "no_seed", "the installation has no ready seed")
  const n = Number(/g(\d+)$/.exec(in_.status.seed.tokenSecret)?.[1] ?? 1) + 1
  in_.status.seed = { ...in_.status.seed, generation: n, tokenSecret: `proxcloud-portal-token-g${n}` }
  return { data: view(in_) }
})
