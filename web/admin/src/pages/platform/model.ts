// model.ts — the Managed services page's view of /api/v1/admin/platform (internal/platform/admin/platform.go and
// internal/platformlife): the answer shapes, the form and its mapping to a spec, the client-side copy of
// Installation.Validate, and what the operator does for each failure code (design §4.7). The API validates again and
// also checks what only it can read (the provider's networks), so this copy only saves a round trip.
import { ApiError } from "@/lib/api"

// replicas: the control plane's machines, 1 or 3, all on node; a spec without it means 1.
export type Machine = { node: string; replicas?: number; cores: number; memoryMiB: number; diskGiB: number }
export type NodeGroup = { name: string; nodes: string[]; count: number; cores: number; memoryMiB: number; diskGiB: number }
export type Spec = {
  pveApiUrl: string
  storage: { boot: string; iso: string; import: string }
  // mode: "bridge" (also what a spec without one means) or "routed" (ProxCloud makes the vnet named bridge). vlan: bridge mode only.
  network: { mode?: string; bridge: string; vlan?: number; prefix: string; gateway: string; dns: string[]; mtu?: number }
  seed: { mode?: string; pool?: string; node: string; address: string; cores: number; memoryMiB: number; diskGiB: number; sshKeys?: string[]; apiSources: string[] }
  clusters: { name?: string; roles?: string[]; endpoint: string; nodeRange: string; metallbRange: string; controlPlane: Machine; nodeGroups: NodeGroup[] }[]
  nodeImage?: { mode?: string; templates?: Record<string, number> }
  // Who gets the two providers; a spec without it means "none". Applied by a run when it differs from status.tenantsAttach.
  tenants?: { attach?: Attach }
  // Optional: the address the platform cluster's ArgoCD answers at over HTTPS, and the networks let in. May be set,
  // changed and cleared on an installed platform; the next apply makes it so.
  access?: { argocd?: { address: string; sources: string[] } }
  // Optional: where sandboxes run (internal/platformlife/sandbox.go). A spec without it keeps what is stored; "off"
  // switches them off. Offered only by a release whose BOM has the sandbox chart (bomOffersSandboxes).
  sandbox?: SandboxSpec
}
export type SandboxMachines = { nodes: string[]; count: number; cores: number; memoryMiB: number; diskGiB: number }
export type SandboxSpec = Partial<SandboxMachines> & {
  placement: "shared" | "separate" | "off"
  nodeRange?: string; dns?: string[]; proxyAddress?: string; domain?: string; tls?: { mode: string }
  system?: SandboxMachines // separate only: sandbox-proxy and the sandbox controller
}
export type Attach = "all" | "new" | "none"
export const ATTACH_LABEL: Record<Attach, string> = {
  all: "All projects: the ones that exist and every new one",
  new: "New projects only",
  none: "None: I attach the providers later",
}
// The choice a new installation starts with. A site with projects is live with tenants (so is one whose projects
// could not be counted): nothing changes for them unless the operator says so.
export const attachDefault = (projects?: number): Attach => (projects === 0 ? "all" : "new")
export type Installation = {
  id: string
  name: string
  serviceId: string
  layout: string
  spec: Spec
  status: {
    phase: string
    bom?: { release: string; sha256: string }
    seed?: { vmid?: number; node?: string; apiServer?: string; cdVolume?: string; tokenSecret?: string; readyAt?: string }
    // controlPlaneReplicas: the count above one a run handed to the cluster; absent means one
    clusters?: { name: string; endpoint?: string; kubernetesVersion?: string; argocdTokenExpiresAt?: string; controlPlaneReplicas?: number }[]
    components?: { name: string; cluster: string; version: string }[]
    providers?: { kubernetes?: string; database?: string; sandbox?: string }
    // What a run built for spec.sandbox; certificate is sandbox-proxy's self-signed one (PEM), for clients to trust.
    sandbox?: { placement: string; nodeRange: string; proxyAddress: string; count: number; certificate?: string; builtAt?: string }
    tenantsAttach?: Attach // the choice the last run applied
    argocdUrl?: string // where the platform cluster's ArgoCD answers, once a run saw its Service hold the address
    argocdSources?: string[] // the networks let in there, as the last run applied them (not the saved settings)
  }
  schedulerNote?: string // set while the job scheduler is off: what tenants miss and the command that turns it on
  lastRunId?: string
  rev: number
  credentials: { capi?: { tokenId: string }; csi?: { tokenId: string } }
  // In the answer of a save only: what the save wrote into the provider's network settings.
  siteChanges?: { blockedCidrs?: string[]; blockedCidrsRemoved?: string[]; zoneWithoutDhcp?: string; problem?: string }
  live?: { phase: string; components: { name: string; cluster: string; sync: string; health: string }[]; error?: string; code?: string }
}
export type Check = { id: string; status: "ok" | "warn" | "fail"; message: string }
// The last "Test the tenant path" of an installation (GET or POST …/tenant-path-probe; internal/platformlife/tenantprobe.go).
export type TenantProbe = {
  id: string; state: "running" | "passed" | "failed" | "error"; node: string; step?: string; startedAt: string; finishedAt?: string
  image?: string; template?: number; storage?: string // what the test VM is cloned from, and the store its disk goes to
  vmid?: number; source?: string; message: string; left?: string[]
  notes?: string[] // what Proxmox may keep by itself, which the cleanup cannot remove
  targets?: { address: string; port: number; expect: "open" | "closed"; result?: "open" | "refused" | "none" }[]
}
export type StageItem = { id: string; state: string; message?: string }
export type Stage = {
  id: string; state: string; retries: number; startedAt?: string; deadline?: string; finishedAt?: string
  progress?: string; items?: StageItem[]; code?: string; message?: string; receipt?: { vmid?: number; name?: string; cdVolume?: string }
}
export type Run = {
  id: string; kind: "apply" | "remove"; state: string; code?: string; message?: string; stages?: Stage[]
  requestedBy?: string; createdAt: string; startedAt?: string; finishedAt?: string
  bom?: BOMRef; fromBom?: BOMRef // fromBom: the run is an upgrade from that release to bom
  part?: "seed" // the run is the seed part of an upgrade that replaces machines: it stops after the seed's components
}
export type BOMRef = { release: string; sha256: string }
export type BOM = {
  release: string; sha256: string
  kubernetes: { version: string; nodeImage: { package: string; tag: string; digest: string } }
  seed: { image: { package: string; tag: string; digest: string } }
  components: { name: string; cluster: string; group?: string; feature?: string; chart: { version: string; digest?: string } }[]
}
// Whether the release an install runs with can build sandboxes: its BOM has the sandbox chart.
export const bomOffersSandboxes = (bom?: BOM) => !!bom?.components.some((c) => c.feature === "sandbox")
// One gate of POST /admin/service/{id}/sandbox/release-check.
export type SandboxGate = { name: string; ok: boolean; detail: string }
export type Plan = {
  installed?: { release: string; sha256: string } | null
  target: { release: string; sha256: string } // what an apply runs with
  newer?: { release: string; sha256: string } | null // this ProxCloud's own release, when the platform stays on an earlier one
  changes?: PlanChange[]
  blocking?: string[]
  upgrade?: Upgrade | null // the move to this ProxCloud's release, or the one in progress
  replacement?: Upgrade | null // next to an upgrade in progress toward an earlier ProxCloud's release: the move to this one's in its place
}
export type PlanChange = {
  cluster?: string; name: string; from?: string; to?: string; blocking?: string
  class?: "chart" | "record only" | "refused" | "site" | "seed" | "machines"; stage?: string; wave?: number; impact?: string
}
export type Upgrade = {
  from: BOMRef; to: BOMRef; allowed: boolean; inProgress?: boolean
  confirm?: string // what to type to start it
  changes: PlanChange[]; blocking: string[]
  // The seed-backup row, on a move that replaces the seed's k3s and has not started. When it is not ok, blocking
  // carries seed_backup_missing, which the operator answers with seedBackupTaken on the apply.
  seedBackup?: Check
  // On a move that replaces the platform cluster's machines: its two actions, "seed" then "platform". allowed, confirm
  // and blocking above are then those of the platform part. The rows named roll and roll/<unit> say what the
  // replacement does per unit, roll-vm/<unit> what it makes and deletes on the Proxmox site.
  parts?: UpgradePart[]
}
export type UpgradePart = { id: "seed" | "platform"; done: boolean; allowed: boolean; confirm?: string; blocking: string[] }

// A run that replaces the platform cluster's machines stands in its roll: stage 7 counts the units as "step n of m".
export const rolling = (run: Run) => !!run.stages?.some((s) => s.id === "cluster" && s.state !== "done" && s.state !== "pending" && s.progress?.startsWith("step "))

// The react-query key every read of this page starts with: one invalidation re-reads them all.
export const PLATFORM_KEY = ["platform"]
export const CAPI_TOKEN = "capmox@pve!capi"
export const CSI_TOKEN = "csi@pve!csi"
export const GROUPS = ["system", "kamaji", "dbaas"] as const
export const GROUP_USE: Record<string, string> = {
  system: "the platform's own components",
  kamaji: "the control planes of tenant Kubernetes clusters",
  dbaas: "tenant databases",
}

// errors.details.code of a refusal ("platform_disabled", "run_active", …) and its problems.
export const errCode = (e: unknown) => (e instanceof ApiError ? (e.details?.code as string | undefined) : undefined)
// A 409 says the page's copy is behind (another session saved, or started a run): the caller reads it again.
export const isConflict = (e: unknown) => e instanceof ApiError && e.status === 409
export const errProblems = (e: unknown): string[] => (e instanceof ApiError && Array.isArray(e.details?.problems) ? e.details.problems : [])

export const runActive = (r?: Run) => !!r && ["queued", "running", "waiting", "cancelling"].includes(r.state)

// ── Form ──────────────────────────────────────────────────────────────────────

export type MachineForm = { cores: string; memoryGiB: string; diskGiB: string }
export type GroupForm = MachineForm & { nodes: string[]; count: string }
// Every field is the text of its input; lists are separated by commas, spaces or new lines.
export type Form = {
  name: string; pveApiUrl: string
  boot: string; iso: string; import: string
  networkMode: "bridge" | "routed"; bridge: string; vlan: string; prefix: string; gateway: string; dns: string; mtu: string
  seedNode: string; seedAddress: string; seed: MachineForm; sshKeys: string; apiSources: string
  endpoint: string; nodeRange: string; metallbRange: string
  cpNode: string; cpReplicas: string; cp: MachineForm
  groups: Record<string, GroupForm>
  attach: Attach | "" // "": not chosen yet; the settings step shows the default for the site
  argoAddress: string; argoSources: string // both empty: the platform cluster's ArgoCD gets no address
  sandbox: SandboxForm
}
// placement "": the spec holds no sandbox block and the form sends none (shown as Off). accept: the tick under the
// shared mode's text; it is not part of the spec, and a stored shared block counts as ticked.
export type SandboxForm = {
  placement: "" | "off" | "shared" | "separate"
  machines: GroupForm; system: GroupForm
  nodeRange: string; proxyAddress: string; domain: string; dns: string; accept: boolean
}

const group = (cores: string, mem: string): GroupForm => ({ nodes: [], count: "1", cores, memoryGiB: mem, diskGiB: "40" })
export const emptyForm: Form = {
  name: "", pveApiUrl: "", boot: "", iso: "local", import: "",
  networkMode: "bridge", bridge: "", vlan: "", prefix: "", gateway: "", dns: "", mtu: "",
  seedNode: "", seedAddress: "", seed: { cores: "2", memoryGiB: "6", diskGiB: "30" }, sshKeys: "", apiSources: "",
  endpoint: "", nodeRange: "", metallbRange: "",
  cpNode: "", cpReplicas: "1", cp: { cores: "2", memoryGiB: "4", diskGiB: "30" },
  groups: { system: group("4", "8"), kamaji: group("4", "8"), dbaas: group("4", "8") },
  attach: "", argoAddress: "", argoSources: "",
  sandbox: {
    placement: "", machines: { ...group("4", "8"), diskGiB: "40" }, system: { ...group("2", "4"), diskGiB: "30" },
    nodeRange: "", proxyAddress: "", domain: "", dns: "", accept: false,
  },
}

// ── Presets (internal/platformlife/preset.go holds the same figures for the API's "preset") ────────────────────────

// The first cluster chart that builds more than one control plane machine (kamaji.ControlPlaneReplicasChartVersion).
const REPLICAS_CHART = [0, 1, 14]
// Whether the release an install runs with can build three control plane machines: its cluster chart is new enough.
export function bomBuildsThree(bom?: BOM): boolean {
  const v = bom?.components.find((c) => c.cluster === "seed" && c.name === "pc-platform")?.chart.version ?? ""
  const p = v.replace(/^v/, "").split(/[-+]/)[0].split(".").map(Number)
  if (p.length !== 3 || p.some(Number.isNaN)) return false
  for (let i = 0; i < 3; i++) if (p[i] !== REPLICAS_CHART[i]) return p[i] > REPLICAS_CHART[i]
  return true
}

export type Preset = "homelab" | "standard" | "custom"
export const PRESETS: { id: Preset; title: string; text: string }[] = [
  { id: "homelab", title: "Homelab", text: "One Proxmox node or a small site. One control plane machine, one small machine per group, everything on one node. On a site with one node the platform network is one that node routes." },
  { id: "standard", title: "Standard", text: "Three control plane machines on one node when that node has the memory, two machines per group, spread over two nodes when the site has several." },
  { id: "custom", title: "Custom", text: "Nothing is filled in: every count, size and node is yours to set." },
]
// What a preset reads of the site: its nodes with their memory in MiB as Proxmox reports it (0 when not read), and
// whether the release can build three control plane machines.
export type SiteNode = { node: string; maxMiB: number; freeMiB: number }
export type Site = { nodes: SiteNode[]; three: boolean }

// A preset fills counts, sizes and nodes into the form and leaves every address, name and store alone; each field
// stays editable afterwards. It is for a new installation: the page does not offer it on an installed platform, where
// it would resize and replace every machine. notes say what it could not do as its name promises.
export function applyPreset(preset: Preset, f: Form, site: Site): { form: Form; notes: string[] } {
  if (preset === "custom") return { form: f, notes: [] }
  const notes: string[] = []
  const names = site.nodes.map((n) => n.node)
  const seedNode = f.seedNode.trim() || names[0] || ""
  const home = preset === "homelab"
  const size = home ? { cores: "2", memoryGiB: "4" } : { cores: "4", memoryGiB: "8" }
  const others = names.filter((n) => n !== seedNode)
  const groups = Object.fromEntries(GROUPS.map((g, i) => {
    // Standard on several nodes: two nodes per group, each group starting one node further, so no node carries all.
    const all = [seedNode, ...others].filter(Boolean)
    const nodes = home || all.length < 2 ? all.slice(0, 1) : [all[i % all.length], all[(i + 1) % all.length]]
    return [g, { ...f.groups[g], ...size, nodes, count: home ? "1" : "2" }]
  }))
  let form: Form = {
    ...f, seedNode, cpNode: seedNode, cpReplicas: home ? "1" : "3",
    seed: { ...f.seed, cores: "2", memoryGiB: home ? "4" : "6" }, cp: { ...f.cp, cores: "2", memoryGiB: "4" }, groups,
  }
  if (!home) {
    const node = site.nodes.find((n) => n.node === seedNode)
    const need = plannedMemory(form)[seedNode]?.need ?? 0
    if (!site.three) {
      form = { ...form, cpReplicas: "1" }
      notes.push("One control plane machine, not three: the release this install runs with builds one. Three come with a release whose cluster chart is 0.1.14 or newer.")
    } else if (node && node.maxMiB > 0 && need > node.freeMiB) {
      form = { ...form, cpReplicas: "1" }
      notes.push(`One control plane machine, not three: with three, ${seedNode} would need ${gib(need)} of memory and has ${gib(node.freeMiB)} free.`)
    }
  }
  // One node and no bridge named yet: the network the node routes (the tenant path on one node).
  if (home && names.length === 1 && !f.bridge.trim()) form = { ...form, networkMode: "routed", bridge: "pcplat0", vlan: "" }
  // Sandboxes stay off unless they were switched on; then the preset sets the placement and the machines (preset.go).
  const sb = f.sandbox
  if (sb.placement === "shared" || sb.placement === "separate") {
    form = { ...form, sandbox: {
      ...sb, placement: home ? "shared" : "separate", accept: false,
      machines: { ...sb.machines, nodes: [seedNode].filter(Boolean), count: home ? "1" : "2", cores: home ? "2" : "4", memoryGiB: home ? "4" : "8", diskGiB: "40" },
      system: { ...sb.system, nodes: [seedNode].filter(Boolean), count: "1", cores: "2", memoryGiB: "4", diskGiB: "30" },
    } }
    notes.push(home
      ? "Sandboxes: a shared node group of one machine with 2 cores, which holds ONE sandbox of one vCPU. Read what shared gives up below, and take 4 cores for several."
      : "Sandboxes: a separate cluster with two sandbox machines and one system machine.")
  }
  return { form, notes }
}

// The memory in MiB the form places on each node, as the Check's capacity row counts it (Installation.PlannedMemoryMiB):
// `run` is what the machines take; `need` adds the one more machine a rolling update of the control plane makes, which
// the row fails on; `spare` adds one more machine of each group on each of its nodes as well, which the row warns on.
// cores are those of the machines that run.
export function plannedMemory(f: Form): Record<string, { cores: number; run: number; need: number; spare: number }> {
  const out: Record<string, { cores: number; run: number; need: number; spare: number }> = {}
  const add = (node: string, n: number, m: MachineForm, controlPlane = false, group = false) => {
    const cores = num(m.cores) || 0, mem = (num(m.memoryGiB) || 0) * 1024
    if (!node.trim()) return
    const at = (out[node.trim()] ??= { cores: 0, run: 0, need: 0, spare: 0 })
    at.cores += n * cores; at.run += n * mem; at.need += (n + (controlPlane ? 1 : 0)) * mem; at.spare += (n + (controlPlane || group ? 1 : 0)) * mem
  }
  add(f.seedNode, 1, f.seed)
  add(f.cpNode, num(f.cpReplicas) || 1, f.cp, true)
  const spread = (gf: GroupForm) => {
    const count = num(gf.count) || 0
    gf.nodes.forEach((n, i) => add(n, Math.floor(count / gf.nodes.length) + (i < count % gf.nodes.length ? 1 : 0), gf, false, true))
  }
  for (const g of GROUPS) spread(f.groups[g])
  // The sandbox machines are placed like a group's (Spec.machineGroups).
  if (f.sandbox.placement === "shared" || f.sandbox.placement === "separate") {
    spread(f.sandbox.machines)
    if (f.sandbox.placement === "separate") spread(f.sandbox.system)
  }
  return out
}
export const gib = (mib: number) => `${Number.isInteger(mib / 1024) ? mib / 1024 : (mib / 1024).toFixed(1)} GiB`

const words = (s: string) => s.split(/[\s,]+/).filter(Boolean)
const lines = (s: string) => s.split("\n").map((l) => l.trim()).filter(Boolean)
const num = (s: string) => (/^\d+$/.test(s.trim()) ? Number(s.trim()) : NaN)
const machine = (m: MachineForm) => ({ cores: num(m.cores), memoryMiB: num(m.memoryGiB) * 1024, diskGiB: num(m.diskGiB) })
const machineForm = (m: { cores: number; memoryMiB: number; diskGiB: number }): MachineForm =>
  ({ cores: String(m.cores), memoryGiB: String(m.memoryMiB / 1024), diskGiB: String(m.diskGiB) })

// What the form sends. The API fills the rest (seed mode and pool, cluster name and roles, node image mode); prev
// keeps the values of a stored spec the form does not show.
export function formToSpec(f: Form, prev?: Spec): Spec {
  const mtu = f.mtu.trim() ? num(f.mtu) : undefined
  // The mode is sent only when routed, and the tag only with a bridge: a spec stored before either field stays as it is.
  const routed = f.networkMode === "routed"
  const vlan = !routed && f.vlan.trim() ? num(f.vlan) : undefined
  return {
    ...prev,
    pveApiUrl: f.pveApiUrl.trim(),
    storage: { boot: f.boot.trim(), iso: f.iso.trim(), import: f.import.trim() },
    network: { ...(routed ? { mode: "routed" } : {}), bridge: f.bridge.trim(), ...(vlan ? { vlan } : {}), prefix: f.prefix.trim(), gateway: f.gateway.trim(), dns: words(f.dns), ...(mtu ? { mtu } : {}) },
    seed: { ...prev?.seed, node: f.seedNode.trim(), address: f.seedAddress.trim(), ...machine(f.seed), sshKeys: lines(f.sshKeys), apiSources: words(f.apiSources) },
    clusters: [{
      ...prev?.clusters?.[0],
      endpoint: f.endpoint.trim(), nodeRange: f.nodeRange.trim(), metallbRange: f.metallbRange.trim(),
      // The count is sent only above one: a spec stored before the field stays as it is, and the API reads none as one.
      controlPlane: { node: f.cpNode.trim(), ...(f.cpReplicas === "3" ? { replicas: 3 } : {}), ...machine(f.cp) },
      nodeGroups: GROUPS.map((name) => ({ name, nodes: f.groups[name].nodes, count: num(f.groups[name].count), ...machine(f.groups[name]) })),
    }],
    // "none" is what a spec without the key means: one stored before the choice existed stays as it is.
    ...(f.attach && (f.attach !== "none" || prev?.tenants) ? { tenants: { attach: f.attach } } : {}),
    // Both empty clears the block (JSON drops the key), also one a stored spec had.
    access: f.argoAddress.trim() || words(f.argoSources).length ? { argocd: { address: f.argoAddress.trim(), sources: words(f.argoSources) } } : undefined,
    sandbox: sandboxSpec(f.sandbox),
  }
}

// "" sends no block (the API keeps the stored one, and there is none); off is sent as the one word.
function sandboxSpec(sb: SandboxForm): SandboxSpec | undefined {
  if (sb.placement === "") return undefined
  if (sb.placement === "off") return { placement: "off" }
  const machines = (g: GroupForm): SandboxMachines => ({ nodes: g.nodes, count: num(g.count), ...machine(g) })
  const dns = words(sb.dns)
  return {
    placement: sb.placement, ...machines(sb.machines),
    nodeRange: sb.nodeRange.trim(), proxyAddress: sb.proxyAddress.trim(), domain: sb.domain.trim().toLowerCase(), tls: { mode: "selfsigned" },
    ...(dns.length ? { dns } : {}),
    ...(sb.placement === "separate" ? { system: machines(sb.system) } : {}),
  }
}

export function specToForm(name: string, s: Spec): Form {
  const c = s.clusters?.[0]
  const groups = { ...emptyForm.groups }
  for (const g of c?.nodeGroups ?? []) groups[g.name] = { ...machineForm(g), nodes: g.nodes ?? [], count: String(g.count) }
  return {
    name, pveApiUrl: s.pveApiUrl ?? "",
    boot: s.storage?.boot ?? "", iso: s.storage?.iso ?? "", import: s.storage?.import ?? "",
    networkMode: s.network?.mode === "routed" ? "routed" : "bridge", bridge: s.network?.bridge ?? "", vlan: s.network?.vlan ? String(s.network.vlan) : "",
    prefix: s.network?.prefix ?? "", gateway: s.network?.gateway ?? "",
    dns: (s.network?.dns ?? []).join(", "), mtu: s.network?.mtu ? String(s.network.mtu) : "",
    seedNode: s.seed?.node ?? "", seedAddress: s.seed?.address ?? "", seed: s.seed ? machineForm(s.seed) : emptyForm.seed,
    sshKeys: (s.seed?.sshKeys ?? []).join("\n"), apiSources: (s.seed?.apiSources ?? []).join(", "),
    endpoint: c?.endpoint ?? "", nodeRange: c?.nodeRange ?? "", metallbRange: c?.metallbRange ?? "",
    cpNode: c?.controlPlane?.node ?? "", cpReplicas: String(c?.controlPlane?.replicas ?? 1), cp: c?.controlPlane ? machineForm(c.controlPlane) : emptyForm.cp,
    groups,
    attach: s.tenants?.attach ?? "none",
    argoAddress: s.access?.argocd?.address ?? "", argoSources: (s.access?.argocd?.sources ?? []).join(", "),
    sandbox: sandboxForm(s.sandbox),
  }
}

function sandboxForm(sb?: SandboxSpec): SandboxForm {
  const e = emptyForm.sandbox
  if (!sb) return e
  if (sb.placement === "off") return { ...e, placement: "off" }
  const g = (m: Partial<SandboxMachines> | undefined, d: GroupForm): GroupForm =>
    m?.cores ? { ...machineForm(m as SandboxMachines), nodes: m.nodes ?? [], count: String(m.count ?? 1) } : d
  return {
    placement: sb.placement, machines: g(sb, e.machines), system: g(sb.system, e.system),
    nodeRange: sb.nodeRange ?? "", proxyAddress: sb.proxyAddress ?? "", domain: sb.domain ?? "", dns: (sb.dns ?? []).join(", "), accept: true,
  }
}

// ── Validation (Installation.Validate in internal/platformlife/installation.go) ──

const NAME = /^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$/
const NODE = /^[A-Za-z0-9]([-A-Za-z0-9]{0,61}[A-Za-z0-9])?$/
const STORAGE = /^[A-Za-z][A-Za-z0-9._-]{0,63}$/
const BRIDGE = /^[A-Za-z][A-Za-z0-9_.-]{0,14}$/
const VNET = /^[a-z][a-z0-9]{0,7}$/ // a Proxmox SDN vnet id
const DOMAIN = /^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]([-a-z0-9]{0,61}[a-z0-9])?$/
export const TOKEN_SECRET = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/

// The line the root command of step 2 prints: PC-ENROLL: plus base64 of "1:<installation>:<unix time>:<capi secret>:<csi secret>"
// (internal/platformlife/enroll.go; a token the command kept has an empty field). enrollProblem checks a pasted line
// before it is sent and says what is wrong with it, without quoting it; null when it can be sent. The API checks again,
// and alone knows whether the line was stored before.
export const ENROLL_PREFIX = "PC-ENROLL:"
export function enrollProblem(pasted: string, installationId: string): string | null {
  const line = pasted.replace(/\s+/g, "") // a terminal may wrap the line
  const whole = "This is not the PC-ENROLL line the command prints: copy the whole line, from PC-ENROLL: to its end."
  if (!line.startsWith(ENROLL_PREFIX)) return whole
  let f: string[]
  try {
    f = atob(line.slice(ENROLL_PREFIX.length)).split(":")
  } catch {
    return whole
  }
  if (f.length !== 5 || f[0] !== "1" || !/^[1-9][0-9]*$/.test(f[2]) || (!f[3] && !f[4]) || [f[3], f[4]].some((v) => v && !TOKEN_SECRET.test(v))) return whole
  if (f[1] !== installationId) return "This PC-ENROLL line is of another installation: run the command shown here, for this installation."
  return null
}

// An IPv4 address as a number, or NaN.
export function ip4(s: string): number {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s.trim())
  if (!m) return NaN
  const p = m.slice(1).map(Number)
  return p.some((n) => n > 255) ? NaN : ((p[0] * 256 + p[1]) * 256 + p[2]) * 256 + p[3]
}
// A CIDR as its first and last address, or undefined; strict wants the network address ("10.0.0.0/24", not ".5/24").
export function cidr4(s: string, strict = false): { first: number; last: number; bits: number } | undefined {
  const [a, b, ...rest] = s.trim().split("/")
  const addr = ip4(a ?? ""), bits = num(b ?? "")
  if (rest.length || Number.isNaN(addr) || Number.isNaN(bits) || bits > 32) return undefined
  const size = 2 ** (32 - bits)
  const first = Math.floor(addr / size) * size
  return strict && first !== addr ? undefined : { first, last: first + size - 1, bits }
}

export type Problem = { field: string; message: string }

// validate lists what the API would refuse, each under the id of the input it is about.
export function validate(f: Form): Problem[] {
  const out: Problem[] = []
  const bad = (field: string, message: string) => out.push({ field, message })
  if (!NAME.test(f.name)) bad("name", "Name: lower-case letters, digits and dashes, at most 40 characters.")
  let url: URL | undefined
  try { url = new URL(f.pveApiUrl.trim()) } catch { /* not a URL */ }
  if (!url || url.protocol !== "https:" || !url.hostname || url.username || url.search || url.hash) bad("pveApiUrl", "Proxmox API URL: an https:// URL with a host.")
  for (const [field, label, v] of [["boot", "Boot store", f.boot], ["iso", "ISO store", f.iso], ["import", "Import store", f.import]] as const) {
    if ((field !== "import" || v.trim()) && !STORAGE.test(v.trim())) bad(field, `${label}: a Proxmox storage id.`)
  }
  // The portal token's role on the import store reaches every disk on it, so it must not be the boot store.
  if (f.import.trim() && f.import.trim() === f.boot.trim()) bad("import", "Import store: a store other than the boot store.")
  if (f.networkMode === "routed") {
    if (!VNET.test(f.bridge.trim())) bad("bridge", "Vnet name: a lower-case letter, then letters and digits, at most 8, such as pcplat0.")
    const placed = new Set([f.seedNode.trim(), f.cpNode.trim(), ...GROUPS.flatMap((g) => f.groups[g].nodes)].filter(Boolean))
    if (placed.size > 1) bad("networkMode", `A routed network is a layout for one Proxmox node, and the settings place machines on ${[...placed].sort().join(", ")}: put the seed, the control plane and every group on one node, or use an existing bridge.`)
  } else {
    if (!BRIDGE.test(f.bridge.trim())) bad("bridge", "Bridge: an interface name such as vmbr1.")
    if (f.vlan.trim() && !(num(f.vlan) >= 1 && num(f.vlan) <= 4094)) bad("vlan", "VLAN tag: 1 to 4094, or empty.")
  }

  const machineOK = (field: string, label: string, m: MachineForm) => {
    const c = num(m.cores), mem = num(m.memoryGiB), d = num(m.diskGiB)
    if (!(c >= 2 && c <= 64 && mem >= 4 && mem <= 256 && d >= 20 && d <= 2048)) bad(field, `${label}: 2 to 64 cores, 4 to 256 GiB of memory, 20 to 2048 GiB of disk.`)
  }
  const nodeOK = (field: string, label: string, v: string) => { if (!NODE.test(v.trim())) bad(field, `${label}: a Proxmox node name.`) }

  const dns = words(f.dns)
  if (dns.length < 1 || dns.length > 3 || dns.some((d) => Number.isNaN(ip4(d)))) bad("dns", "DNS servers: one to three IPv4 addresses.")
  if (f.mtu.trim() && !(num(f.mtu) >= 1280 && num(f.mtu) <= 9000)) bad("mtu", "MTU: 1280 to 9000, or empty.")
  nodeOK("seedNode", "Seed node", f.seedNode)
  machineOK("seed-cores", "Seed", f.seed)
  const keys = lines(f.sshKeys)
  if (keys.length > 8 || keys.some((k) => !/^(ssh-|ecdsa-|sk-)\S+ \S+/.test(k))) bad("sshKeys", "SSH keys: at most 8 public keys, one per line.")
  const sources = words(f.apiSources)
  if (sources.length < 1 || sources.length > 16 || sources.some((c) => !cidr4(c) || cidr4(c)!.bits === 0)) {
    bad("apiSources", "Seed API sources: 1 to 16 IPv4 networks such as 192.0.2.10/32, at least the address ProxCloud reaches the seed from.")
  }
  nodeOK("cpNode", "Control plane node", f.cpNode)
  machineOK("cp-cores", "Control plane", f.cp)
  if (!["1", "3"].includes(f.cpReplicas)) bad("cpReplicas", "Control plane machines: 1 or 3.")
  let machines = (num(f.cpReplicas) || 1) + 1, surge = 0 // the control plane machines and the one more a rolling update makes
  for (const g of GROUPS) {
    const gf = f.groups[g]
    const count = num(gf.count)
    if (gf.nodes.length < 1 || gf.nodes.length > 16) bad(`${g}-nodes`, `Group ${g}: choose 1 to 16 nodes.`)
    else if (gf.nodes.some((n) => !NODE.test(n)) || new Set(gf.nodes).size !== gf.nodes.length) bad(`${g}-nodes`, `Group ${g}: each node once, by its Proxmox name.`)
    if (!(count >= Math.max(1, gf.nodes.length) && count <= 64)) bad(`${g}-count`, `Group ${g}: at least one machine per chosen node, at most 64.`)
    machineOK(`${g}-cores`, `Group ${g}`, gf)
    machines += count || 0
    surge += gf.nodes.length
  }

  const net = cidr4(f.prefix, true)
  if (!net || net.bits < 8 || net.bits > 29) {
    bad("prefix", "Network: an IPv4 network from /8 to /29, such as 10.90.0.0/24.")
    return out // every address below is checked against it
  }
  const host = (field: string, label: string, v: string) => {
    const a = ip4(v)
    if (Number.isNaN(a) || a <= net.first || a >= net.last) { bad(field, `${label}: a host address inside ${f.prefix.trim()}.`); return NaN }
    return a
  }
  const range = (field: string, label: string, v: string) => {
    const parts = v.split("-")
    const first = ip4(parts[0] ?? ""), last = ip4(parts[1] ?? "")
    if (parts.length !== 2 || !(first > net.first && last < net.last && first <= last)) { bad(field, `${label}: first-last host addresses inside ${f.prefix.trim()}, such as 10.90.0.20-10.90.0.60.`); return undefined }
    if (last - first + 1 > 4096) { bad(field, `${label}: at most 4096 addresses.`); return undefined }
    return { first, last }
  }
  const points = [
    { field: "gateway", label: "Gateway", a: host("gateway", "Gateway", f.gateway) },
    { field: "seedAddress", label: "Seed address", a: host("seedAddress", "Seed address", f.seedAddress) },
    { field: "endpoint", label: "API address", a: host("endpoint", "API address", f.endpoint) },
  ]
  const ranges = [
    { field: "nodeRange", label: "Node range", r: range("nodeRange", "Node range", f.nodeRange) },
    { field: "metallbRange", label: "Load balancer range", r: range("metallbRange", "Load balancer range", f.metallbRange) },
  ]
  points.forEach((p, i) => {
    for (const q of points.slice(i + 1)) if (p.a === q.a) bad(q.field, `${q.label} and ${p.label.toLowerCase()} are the same address.`)
    for (const r of ranges) if (r.r && p.a >= r.r.first && p.a <= r.r.last) bad(p.field, `${p.label} is inside the ${r.label.toLowerCase()}.`)
  })
  const [nodes, lbs] = [ranges[0].r, ranges[1].r]
  if (nodes && lbs && nodes.first <= lbs.last && lbs.first <= nodes.last) bad("metallbRange", "The node range and the load balancer range overlap.")
  if (nodes && nodes.last - nodes.first + 1 < machines + surge) {
    bad("nodeRange", `Node range: it holds ${nodes.last - nodes.first + 1} addresses; ${machines} machines and ${surge} spare for updates need ${machines + surge}.`)
  }
  const argoSources = words(f.argoSources)
  if (f.argoAddress.trim() || argoSources.length) {
    const a = ip4(f.argoAddress)
    if (Number.isNaN(a) || (lbs && (a < lbs.first || a > lbs.last))) bad("argoAddress", "ArgoCD address: one free IPv4 address inside the load balancer range, or empty together with the operator networks.")
    if (argoSources.length < 1 || argoSources.length > 16 || argoSources.some((c) => !cidr4(c) || cidr4(c)!.bits === 0)) {
      bad("argoSources", "Operator networks: 1 to 16 IPv4 networks such as 192.0.2.0/24, never 0.0.0.0/0, or empty together with the address.")
    }
  }

  // Sandboxes (Spec.validateSandbox in internal/platformlife/sandbox.go).
  const sb = f.sandbox
  if (sb.placement === "shared" || sb.placement === "separate") {
    let need = 0
    const sbGroup = (id: string, label: string, g: GroupForm) => {
      const count = num(g.count)
      if (g.nodes.length < 1 || g.nodes.length > 16 || g.nodes.some((n) => !NODE.test(n)) || new Set(g.nodes).size !== g.nodes.length) bad(`${id}-nodes`, `${label}: choose 1 to 16 nodes, each once.`)
      if (!(count >= Math.max(1, g.nodes.length) && count <= 64)) bad(`${id}-count`, `${label}: at least one machine per chosen node, at most 64.`)
      machineOK(`${id}-cores`, label, g)
      need += (count || 0) + g.nodes.length
    }
    sbGroup("sbx", "Sandbox machines", sb.machines)
    if (sb.placement === "separate") sbGroup("sbxsys", "Sandbox system machines", sb.system)
    const r = cidr4(sb.nodeRange, true)
    if (!r || r.bits <= net.bits || r.bits > 30 || r.first < net.first || r.last > net.last) {
      bad("sbxRange", `Sandbox range: one network inside ${f.prefix.trim()}, from /${net.bits + 1} to /30, written as a CIDR such as 10.90.0.64/28 (not first-last).`)
    } else {
      if (r.first === net.first || r.last === net.last) bad("sbxRange", "Sandbox range: it holds the network or the broadcast address of the platform network.")
      for (const p of points) if (p.a >= r.first && p.a <= r.last) bad("sbxRange", `Sandbox range: the ${p.label.toLowerCase()} is inside it.`)
      for (const x of ranges) if (x.r && x.r.first <= r.last && r.first <= x.r.last) bad("sbxRange", `Sandbox range: it overlaps the ${x.label.toLowerCase()}. Sandbox machines take addresses no platform machine uses.`)
      if (r.last - r.first + 1 < need) bad("sbxRange", `Sandbox range: it holds ${r.last - r.first + 1} addresses; the sandbox machines and one spare per node need ${need}.`)
    }
    const proxy = ip4(sb.proxyAddress)
    if (Number.isNaN(proxy)) bad("sbxProxy", "Proxy address: an IPv4 address.")
    else if (sb.placement === "shared") {
      if (lbs && (proxy < lbs.first || proxy > lbs.last)) bad("sbxProxy", "Proxy address: with a shared node group, a free address inside the load balancer range.")
      if (f.argoAddress.trim() && ip4(f.argoAddress) === proxy) bad("sbxProxy", "Proxy address: the ArgoCD address is taken; each needs its own.")
    } else {
      const inRange = (x?: { first: number; last: number }) => !!x && proxy >= x.first && proxy <= x.last
      if (proxy <= net.first || proxy >= net.last || points.some((p) => p.a === proxy) || inRange(nodes) || inRange(lbs) || (r && inRange(r))) {
        bad("sbxProxy", `Proxy address: with a separate cluster, a free host address of ${f.prefix.trim()} outside the node range, the load balancer range and the sandbox range.`)
      }
    }
    if (!DOMAIN.test(sb.domain.trim().toLowerCase()) || sb.domain.trim().length > 200) bad("sbxDomain", "Sandbox domain: a DNS name such as sbx.example.org; sandboxes answer at <port>-<id>.<domain>.")
    const sdns = words(sb.dns)
    if (sdns.length > 3 || sdns.some((d) => Number.isNaN(ip4(d)))) bad("sbxDns", "Sandbox DNS servers: at most three IPv4 addresses, or empty for the platform network's.")
    if (sb.placement === "shared" && !sb.accept) bad("sbxAccept", "Tick the box under the text on what a shared node group gives up, or choose a separate cluster.")
  }
  return out
}

// ── Sandboxes ─────────────────────────────────────────────────────────────────

// What a shared node group gives up, in the words of docs/managed-services.md ("Sandboxes"): shown in full before the
// tick, never shortened to a tooltip.
export const SHARED_GIVES_UP: string[] = [
  "Code that escapes a sandbox (a gVisor escape) is root on a machine of the platform cluster: the cluster that runs kamaji-etcd (the datastore of every tenant's Kubernetes control plane), every tenant's API server, CloudNativePG and every tenant database, and ArgoCD.",
  "The pod network is open to that machine. It is a Cilium peer and can send packets with any source to any pod; network policies do not stop it. What protects the targets then is their own authentication: client certificates on kamaji-etcd, passwords on the databases, tokens on the API servers.",
  "The Cilium agent's credential on that machine can create and change CiliumNode objects. A forged one pulls another node's pod traffic to the escaped machine: traffic between pods that is not encrypted with both ends authenticated can be read and forged.",
  "The machine also holds the kubelet's credential (its own Node and the Secrets of the pods on it), the network guard's (it reads every pod's spec) and kube-proxy's.",
  "Sandboxes have no address of their own to the internet: abuse from a sandbox leaves under your site's public address.",
  "What still holds: the Proxmox API, the seed, the management networks, tenant networks and every other address of the platform network stay closed to that machine by its VM firewall, and it cannot take another machine's address.",
  "A separate cluster limits the same escape to the sandbox cluster: other tenants' sandboxes on it, its own control plane's API and the internet. It costs a control plane (about 0.6 GiB on the kamaji group) and a system machine.",
]
// By CPU a sandbox machine of n cores holds n-1 sandboxes of one vCPU: the node's own pods ask for about one core.
export const sandboxRoom = (cores: string) => {
  const c = num(cores)
  if (!(c >= 2)) return ""
  return c === 2 ? "A machine of 2 cores holds ONE sandbox of one vCPU: take 4 cores or more for several." : `By CPU each machine holds about ${c - 1} sandboxes of one vCPU.`
}
export const GATE_LABEL: Record<string, string> = {
  noderestriction: "NodeRestriction", isolation: "Isolation rules", fence: "VM firewall of the sandbox machines", "network-guard": "Network guard", checker: "Gate checker", config: "Provider settings",
}

// ── What the operator does ────────────────────────────────────────────────────

const RERUN = "Run the root command of step 2 again, then press Check."
// The field is "Networks tenants must not reach" on the provider's page; the Check step links there and has the sync button.
const BLOCKED = "Press Save settings in step 1: the save adds the installation's own addresses to the provider's blocked networks (or add the networks named in the message, or one network that covers them, to “Networks tenants must not reach” on the provider's page under Cloud providers)"
const ISOLATION = `${BLOCKED}. Then press Check in step 3: its isolation row runs the provider sync that writes the rules. Check too that the datacenter firewall is on.`

// One row of the preflight (POST …/preflight) → what fixes it.
const CHECK_ACTION: Record<string, string> = {
  flags: "Turn the switches on with ./proxcloud-up.sh install --managed-services, and save a GitHub token under Integrations.",
  spec: "Correct the settings in step 1 and save.",
  network: "Correct the platform network in step 1 and save.",
  bridge: "Name a bridge or vnet that exists on every node the settings use, or change the nodes, in step 1. For a routed network, name a vnet Proxmox does not have yet.",
  isolation: `${BLOCKED}, then run a provider sync: for a provider with a saved installation it writes the rules, with or without a project. Check too that the datacenter firewall is on. No sync helps when the message says that the tenant address pool overlaps a management network (choose another pool on the provider's page), or that the rules in place are for another pool, drop a network this provider does not name, accept other sandbox endpoints than this provider's, or are not this provider's set while a project's rules for the pool are in place (another ProxCloud or provider on this Proxmox cluster uses the same pool prefix: give this provider its own; if the rules are this provider's own older ones, delete them in Proxmox under Datacenter, Firewall, then sync).`,
  credentials: "Run the root command of step 2 on a node and paste the PC-ENROLL line it prints.",
  "boot-store": "Name a store that exists and takes disk images as the boot store in step 1, or run the command of the message as root on a node. A warning about other VMs' disks does not stop the install: the message says what a store of its own for the platform avoids, and the command that adds one.",
  "seed-backup": "Back the seed VM up in Proxmox (vzdump). On most stores ProxCloud cannot see the seed's backups: Proxmox does not show them to its tokens. An upgrade that replaces the seed's k3s asks you to check in Proxmox for a backup of this seed VM from the last 24 hours and to confirm it with a box in step 4.",
  "pve-api-rule": "Nothing to do before the install: Install and every Apply write this cluster firewall rule. On an installed platform, press Apply.",
  privileges: RERUN,
  "mint-scope": "Either the portal token can create API tokens for every user of the Proxmox cluster, root@pam included, or write any role on every pool (an older setup granted that on /access/groups and /pool), or capmox@pve or csi@pve is not in the group proxcloud-kube, so Proxmox refuses their tokens. The message says which. Run the identities script again (step 2) as root on one Proxmox node: it moves the grant to the group, puts both users in it and removes the old rows. If its last lines say NOT FINISHED, run it for each token it names. Then press Check.",
  "pool-fence": "A token reaches a platform pool it must not, usually because its NoAccess row there is missing. Run the identities script again (step 2), then press Check. Only if it still fails, remove the permission granted to that token on the pool itself in Proxmox (Datacenter, Permissions). Do not remove its permissions on /pool.",
  "token-scope": "A platform token holds a permission that reaches every tenant, left by an older setup. Run the pveum acl delete command of the message as root on one Proxmox node, then press Check. If pveum acl list shows no permission of the token on the path the message names, or above it with propagate, remove none: the token was made without privilege separation. Run the pveum user token modify command of the message, then the identities script again. Do not remove the token's permissions on the platform pools or on the boot and ISO stores.",
  "iso-store-visibility": `${CAPI_TOKEN} must see exactly one ISO store. Run the pveum acl delete commands of the message as root on one Proxmox node, or name the right ISO store in step 1 and run the script again. Then press Check. If the message names the boot store, remove nothing: name another boot store in step 1, or take ISO images out of that store's content in Proxmox.`,
  "boot-iso-grant": RERUN,
  "boot-iso": "Name a store that is local to each node and takes ISO files as the ISO store in step 1.",
  placement: "Every node used needs the import store and the boot store. Change the nodes or the stores in step 1.",
  capacity: "A node has less memory than the settings place on it, counted with the one more machine an update of the control plane makes. In step 1 use smaller or fewer machines, one control plane machine in place of three, or other nodes; or free memory on the node in Proxmox (a stopped VM counts with its configured memory). Then save and press Check.",
  images: "A platform image is blocked. For a new installation, install a newer ProxCloud release. An installed platform stays on its release's images: no release changes them yet.",
}
export const checkAction = (id: string) => CHECK_ACTION[id.split(":")[0]] ?? "Read the message, correct it, then press Check."

const RETRY = "Press Retry. The time limits start again."
// A failure code of a run or a stage (design §4.7 and internal/platformlife/run.go) → what the operator does.
const CODE_ACTION: Record<string, string> = {
  preflight_failed: "Open the failed checks of this stage. Run the identities script again or correct the settings, then press Check.",
  isolation_not_enforced: ISOLATION,
  image_import_failed: "Open Template updates to see the import, then Retry. If it keeps failing, install a newer ProxCloud release.",
  image_import_stalled: "Open Template updates to see the import, then Retry. If it keeps failing, install a newer ProxCloud release.",
  image_blocked: "This image is blocked. For a new installation, install a newer ProxCloud release. An installed platform stays on its release's images: no release changes them yet.",
  seed_clone_failed: "Fix the storage or the node named in the message, then Retry.",
  platform_network_refused: "The routed platform network could not be made or removed. The message carries the error from Proxmox, or names the vnet or zone that is in the way: correct it in Proxmox (Datacenter, SDN) or name another vnet in step 1, then Retry.",
  seed_config_refused: "The message carries the error from Proxmox: fix the bridge, the store, the node or the token right it names, then Retry. If the message says the seed's CD could not be rendered or built, Retry will not pass: report the message.",
  seed_not_booted: "Open the seed VM's console in Proxmox. Retry builds a new seed when this one never became ready.",
  seed_api_unreachable: "Add a route from the ProxCloud host to the platform network, or correct the seed API sources. Retry writes the seed's firewall rules again first. When the message says the cluster's API refuses the credential: it does so for some seconds after it starts; if the run failed on it, look on the seed VM's console whether its Kubernetes keeps restarting, then Retry.",
  seed_tls_mismatch: "Another server answers on the seed address. Correct the seed address in the settings, then Retry.",
  seed_identity_lost: "Press Retry. It builds the seed again.",
  seed_cd_not_deleted: `${CAPI_TOKEN} cannot delete on the ISO store. Run the identities script again, then Retry.`,
  seed_lost: "The seed VM is gone or is no longer this installation's. It is not built again on its own. Remove the installation with the option for a missing seed, then install again.",
  component_unhealthy: "The message names the component and what its sync failed on. Download the seed kubeconfig to look closer, then Retry.",
  capi_init_failed: "The cluster manager did not start on the seed. The message names what its sync failed on. Download the seed kubeconfig to look closer, then Retry.",
  machine_failed: "Correct what the message names. The machine is not tried again on its own: download the seed kubeconfig, run the kubectl delete machine command of the message, then Retry. If the message says a VM's name does not match, do not delete the machine. If it names a node template, run its qm set command on that node, then Retry.",
  capmox_auth_failed: `Proxmox refused ${CAPI_TOKEN}, or the seed cannot reach the Proxmox API URL of the settings. Check the URL as the platform network reaches it and paste the token secret again, then Retry.`,
  workload_api_unreachable: "Add a route from the ProxCloud host to the API address, then Retry. When the message says the cluster's API refuses the credential: it does so for some seconds after it starts; if the run failed on it, Retry.",
  bom_unsupported: "The platform was installed by a ProxCloud whose platform release this one does not carry (a development build, or a release from before 0.8.0), and a platform moves only between releases this ProxCloud carries. Go back to the ProxCloud that installed it. Or, if no tenant cluster or database must be kept, delete them all, then remove managed services and install again: Remove deletes the platform cluster.",
  upgrade_refused: "This ProxCloud cannot move the platform to its own platform release: step 4 lists why. The platform stays on its release, and Apply this release keeps working.",
  unexpected_rollout: "The upgrade would replace machines of the platform cluster that it is not there to replace, so nothing of that step was synced. If machine sizes, nodes or node groups were changed together with the upgrade, put them back, save, then Retry. Otherwise install the ProxCloud release that corrects it: step 4 then offers the upgrade to that release in place of this one.",
  argocd_address_unavailable: "MetalLB did not give the platform cluster's ArgoCD the address of the settings; the message names the Service that holds it when one does. The platform keeps serving. Put a free address of the load balancer range under Access in step 1 (or clear both fields), save, then Retry.",
  "argocd-access": "Under Access in step 1, name operator networks that no Proxmox node is in, or clear both fields. On a site where tenant traffic leaves under the node's address, a tenant VM reaches the ArgoCD login page from a network that contains that address.",
  node_image_missing: "A node the platform cluster uses holds no template of the release's node image. Run “Seed and node image” in step 4 first: it imports the image on every node and replaces no machine.",
  capacity_short: "A node has no room for the one more machine a replacement makes first. Nothing was changed. Free memory or space on the node the message names (a stopped VM counts with its configured memory), then read the plan again or Retry.",
  drain_blocked: "A PodDisruptionBudget on the platform cluster allows no disruption, so the machine its pod is on could never be drained. Nothing was changed. For a database, bump its chart to 0.5.2 or newer (the databases guide has the step); for anything else give it a second replica or delete the budget. Then read the plan again.",
  platform_unhealthy: "The replacement of the machines starts only on a platform whose machines all run and whose components are all Healthy. Nothing was changed. Repair what the message names (Apply this release, then Retry), then read the plan again.",
  seed_not_upgraded: "The seed's k3s is not at the release's version yet. Run “Seed and node image” in step 4 first.",
  rollout_stalled: "No machine of the unit moved for 30 minutes. The message names the machine and, when a pod cannot be evicted, the pod. The machines that run keep serving. Correct the cause (for a pod held by a PodDisruptionBudget: give it a second replica or remove the budget), then Resume: the run goes on at the same unit.",
  tenants_unhealthy: "A tenant cluster or database that was healthy before the upgrade is not. Look at the one the message names. Retry goes on from the same step and waits for it again: the upgrade does not go on until it is healthy or deleted.",
  seed_backup_missing: "The upgrade replaces k3s on the seed VM, and ProxCloud saw no backup of that VM from the last 24 hours; on most sites it cannot see one. Check in Proxmox that there is a backup of this seed VM, made after the seed VM was created and younger than 24 hours (vzdump makes one), tick the box in step 4, then start the upgrade again.",
  seed_k3s_failed: "The seed runs its k3s as before. The message ends with the last lines of the upgrade: a download that failed means the seed cannot reach the release's k3s address. Correct it, then Retry.",
  seed_k3s_refused: "The seed runs another k3s than this upgrade can start from, and nothing was changed. The message names both versions.",
  seed_k3s_timeout: "The platform cluster and the tenants keep running. While the stage is still trying there is nothing to do. If it failed and the seed's API stays away, the message names the command that puts the previous k3s back. Then Retry.",
  provider_conflict: "A provider row of this installation was refused. Read the message, correct that provider under Cloud providers, then Retry.",
  machine_delete_stuck: "Delete the VM named in the message in Proxmox, then start Remove again.",
  tenants_exist: "Tenant clusters or databases still run on the platform. Delete them, then start Remove again.",
  volume_delete_failed: `Check ${CSI_TOKEN} and the storage named in the message, then start Remove again.`,
  credentials_missing: "Run the root command of step 2 and paste the PC-ENROLL line it prints, then Retry.",
  stage_unavailable: "This release cannot run this stage for this installation.",
  platform_disabled: "Turn managed services on with ./proxcloud-up.sh install --managed-services.",
  portal_changed: RETRY,
  job_dead: RETRY,
}
// A removal has no Retry: its button is "Remove again", and a cluster that does not answer has the forced option.
export function codeAction(code: string, kind: Run["kind"] = "apply") {
  if (kind === "remove" && code === "workload_api_unreachable") {
    return "Restore the route from the ProxCloud host to the API address and press Remove again. If the cluster is beyond repair, tick the option for a cluster that does not answer."
  }
  const action = CODE_ACTION[code] ?? (code.endsWith("_timeout") ? RETRY : "Read the message, correct it, then Retry.")
  return kind === "remove" ? action.replace(/\bRetry\b/g, "Remove again") : action
}

// The guide, the failure codes and the messages number an apply's stages 1 to 11. The k3s stage came later, between
// 5 and 6: it is 5a, so every other stage keeps its number.
export function stageNumbers(stages: Stage[]): string[] {
  let n = 0
  return stages.map((s) => (s.id === "seed-k3s" ? `${n}a` : String(++n)))
}

export const STAGE_TITLE: Record<string, string> = {
  validate: "Check the settings and Proxmox",
  images: "Import the two images",
  "seed-vm": "Create the seed VM",
  "seed-api": "Reach the seed",
  "seed-identity": "Secure the seed",
  "seed-k3s": "Upgrade the seed's k3s",
  "seed-platform": "Install the seed's components",
  cluster: "Build the platform cluster",
  "workload-access": "Reach the platform cluster",
  "workload-platform": "Install the platform components",
  register: "Create the two providers",
  done: "Finish",
  "remove-cluster": "Delete the platform cluster",
  "remove-seed": "Delete the seed",
  "remove-secrets": "Forget the stored secrets",
}
