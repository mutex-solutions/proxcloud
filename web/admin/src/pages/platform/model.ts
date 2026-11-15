// model.ts — the Managed services page's view of /api/v1/admin/platform (internal/platform/admin/platform.go and
// internal/platformlife): the answer shapes, the form and its mapping to a spec, the client-side copy of
// Installation.Validate, and what the operator does for each failure code (design §4.7). The API validates again and
// also checks what only it can read (the provider's networks), so this copy only saves a round trip.
import { ApiError } from "@/lib/api"

export type Machine = { node: string; cores: number; memoryMiB: number; diskGiB: number }
export type NodeGroup = { name: string; nodes: string[]; count: number; cores: number; memoryMiB: number; diskGiB: number }
export type Spec = {
  pveApiUrl: string
  storage: { boot: string; iso: string; import: string }
  network: { bridge: string; prefix: string; gateway: string; dns: string[]; mtu?: number }
  seed: { mode?: string; pool?: string; node: string; address: string; cores: number; memoryMiB: number; diskGiB: number; sshKeys?: string[]; apiSources: string[] }
  clusters: { name?: string; roles?: string[]; endpoint: string; nodeRange: string; metallbRange: string; controlPlane: Machine; nodeGroups: NodeGroup[] }[]
  nodeImage?: { mode?: string; templates?: Record<string, number> }
}
export type Installation = {
  id: string
  name: string
  serviceId: string
  layout: string
  spec: Spec
  status: {
    phase: string
    bom?: { release: string; sha256: string }
    seed?: { vmid?: number; node?: string; cdVolume?: string; tokenSecret?: string; readyAt?: string }
    clusters?: { name: string; endpoint?: string; kubernetesVersion?: string; argocdTokenExpiresAt?: string }[]
    components?: { name: string; cluster: string; version: string }[]
    providers?: { kubernetes?: string; database?: string }
  }
  lastRunId?: string
  rev: number
  credentials: { capi?: { tokenId: string }; csi?: { tokenId: string } }
  live?: { phase: string; components: { name: string; cluster: string; sync: string; health: string }[]; error?: string }
}
export type Check = { id: string; status: "ok" | "warn" | "fail"; message: string }
export type StageItem = { id: string; state: string; message?: string }
export type Stage = {
  id: string; state: string; retries: number; startedAt?: string; deadline?: string; finishedAt?: string
  progress?: string; items?: StageItem[]; code?: string; message?: string; receipt?: { vmid?: number; name?: string; cdVolume?: string }
}
export type Run = {
  id: string; kind: "apply" | "remove"; state: string; code?: string; message?: string; stages?: Stage[]
  requestedBy?: string; createdAt: string; startedAt?: string; finishedAt?: string
}
export type BOM = {
  release: string; sha256: string
  kubernetes: { version: string; nodeImage: { package: string; tag: string; digest: string } }
  seed: { image: { package: string; tag: string; digest: string } }
  components: { name: string; cluster: string; group?: string; chart: { version: string; digest?: string } }[]
}
export type Plan = {
  installed?: { release: string; sha256: string } | null
  target: { release: string; sha256: string }
  changes?: { cluster?: string; name: string; from?: string; to?: string; blocking?: string }[]
  blocking?: string[]
}

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
export const errProblems = (e: unknown): string[] => (e instanceof ApiError && Array.isArray(e.details?.problems) ? e.details.problems : [])

export const runActive = (r?: Run) => !!r && ["queued", "running", "waiting", "cancelling"].includes(r.state)

// ── Form ──────────────────────────────────────────────────────────────────────

export type MachineForm = { cores: string; memoryGiB: string; diskGiB: string }
export type GroupForm = MachineForm & { nodes: string[]; count: string }
// Every field is the text of its input; lists are separated by commas, spaces or new lines.
export type Form = {
  name: string; pveApiUrl: string
  boot: string; iso: string; import: string
  bridge: string; prefix: string; gateway: string; dns: string; mtu: string
  seedNode: string; seedAddress: string; seed: MachineForm; sshKeys: string; apiSources: string
  endpoint: string; nodeRange: string; metallbRange: string
  cpNode: string; cp: MachineForm
  groups: Record<string, GroupForm>
}

const group = (cores: string, mem: string): GroupForm => ({ nodes: [], count: "1", cores, memoryGiB: mem, diskGiB: "40" })
export const emptyForm: Form = {
  name: "", pveApiUrl: "", boot: "", iso: "local", import: "",
  bridge: "", prefix: "", gateway: "", dns: "", mtu: "",
  seedNode: "", seedAddress: "", seed: { cores: "2", memoryGiB: "6", diskGiB: "30" }, sshKeys: "", apiSources: "",
  endpoint: "", nodeRange: "", metallbRange: "",
  cpNode: "", cp: { cores: "2", memoryGiB: "4", diskGiB: "30" },
  groups: { system: group("4", "8"), kamaji: group("4", "8"), dbaas: group("4", "8") },
}

// The lab profile of design §8.3: the smallest machines the API takes, one per group.
export function labProfile(f: Form): Form {
  const small = (g: GroupForm): GroupForm => ({ ...g, count: String(Math.max(1, g.nodes.length)), cores: "2", memoryGiB: "4" })
  return { ...f, seed: { ...f.seed, cores: "2", memoryGiB: "4" }, cp: { ...f.cp, cores: "2", memoryGiB: "4" },
    groups: Object.fromEntries(GROUPS.map((g) => [g, small(f.groups[g])])) }
}

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
  return {
    ...prev,
    pveApiUrl: f.pveApiUrl.trim(),
    storage: { boot: f.boot.trim(), iso: f.iso.trim(), import: f.import.trim() },
    network: { bridge: f.bridge.trim(), prefix: f.prefix.trim(), gateway: f.gateway.trim(), dns: words(f.dns), ...(mtu ? { mtu } : {}) },
    seed: { ...prev?.seed, node: f.seedNode.trim(), address: f.seedAddress.trim(), ...machine(f.seed), sshKeys: lines(f.sshKeys), apiSources: words(f.apiSources) },
    clusters: [{
      ...prev?.clusters?.[0],
      endpoint: f.endpoint.trim(), nodeRange: f.nodeRange.trim(), metallbRange: f.metallbRange.trim(),
      controlPlane: { node: f.cpNode.trim(), ...machine(f.cp) },
      nodeGroups: GROUPS.map((name) => ({ name, nodes: f.groups[name].nodes, count: num(f.groups[name].count), ...machine(f.groups[name]) })),
    }],
  }
}

export function specToForm(name: string, s: Spec): Form {
  const c = s.clusters?.[0]
  const groups = { ...emptyForm.groups }
  for (const g of c?.nodeGroups ?? []) groups[g.name] = { ...machineForm(g), nodes: g.nodes ?? [], count: String(g.count) }
  return {
    name, pveApiUrl: s.pveApiUrl ?? "",
    boot: s.storage?.boot ?? "", iso: s.storage?.iso ?? "", import: s.storage?.import ?? "",
    bridge: s.network?.bridge ?? "", prefix: s.network?.prefix ?? "", gateway: s.network?.gateway ?? "",
    dns: (s.network?.dns ?? []).join(", "), mtu: s.network?.mtu ? String(s.network.mtu) : "",
    seedNode: s.seed?.node ?? "", seedAddress: s.seed?.address ?? "", seed: s.seed ? machineForm(s.seed) : emptyForm.seed,
    sshKeys: (s.seed?.sshKeys ?? []).join("\n"), apiSources: (s.seed?.apiSources ?? []).join(", "),
    endpoint: c?.endpoint ?? "", nodeRange: c?.nodeRange ?? "", metallbRange: c?.metallbRange ?? "",
    cpNode: c?.controlPlane?.node ?? "", cp: c?.controlPlane ? machineForm(c.controlPlane) : emptyForm.cp,
    groups,
  }
}

// ── Validation (Installation.Validate in internal/platformlife/installation.go) ──

const NAME = /^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$/
const NODE = /^[A-Za-z0-9]([-A-Za-z0-9]{0,61}[A-Za-z0-9])?$/
const STORAGE = /^[A-Za-z][A-Za-z0-9._-]{0,63}$/
const BRIDGE = /^[A-Za-z][A-Za-z0-9_.-]{0,14}$/
export const TOKEN_SECRET = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/

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
  if (!BRIDGE.test(f.bridge.trim())) bad("bridge", "Bridge: an interface name such as vmbr1.")

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
  let machines = 2, surge = 0 // the control plane and its rolling surge
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
  return out
}

// ── What the operator does ────────────────────────────────────────────────────

const RERUN = "Run the identities script again (step 2), then press Check."
const ISOLATION = "Add the networks named in the message to the provider's blocked networks (Cloud providers, the provider's network settings), run a provider sync, and check that the datacenter firewall is on."

// One row of the preflight (POST …/preflight) → what fixes it.
const CHECK_ACTION: Record<string, string> = {
  flags: "Turn the switches on with ./proxcloud-up.sh install --managed-services, and save a GitHub token under Integrations.",
  spec: "Correct the settings in step 1 and save.",
  network: "Correct the platform network in step 1 and save.",
  isolation: ISOLATION,
  credentials: "Paste both token secrets in step 2.",
  privileges: RERUN,
  "pool-fence": "A token reaches a pool it must not. Remove that permission in Proxmox (Datacenter, Permissions).",
  "iso-store-visibility": `${CAPI_TOKEN} must see exactly one ISO store. Remove its permission on the others, or name the right ISO store in step 1 and run the script again.`,
  "boot-iso-grant": RERUN,
  "boot-iso": "Name a store that is local to each node and takes ISO files as the ISO store in step 1.",
  placement: "Every node used needs the import store and the boot store. Change the nodes or the stores in step 1.",
  images: "A platform image is blocked. Install a newer ProxCloud release.",
}
export const checkAction = (id: string) => CHECK_ACTION[id.split(":")[0]] ?? "Read the message, correct it, then press Check."

const RETRY = "Press Retry. The time limits start again."
// A failure code of a run or a stage (design §4.7 and internal/platformlife/run.go) → what the operator does.
const CODE_ACTION: Record<string, string> = {
  preflight_failed: "Open the failed checks of this stage. Run the identities script again or correct the settings, then press Check.",
  privileges_missing: RERUN,
  network_problem: "Correct the platform network in the settings, then press Check.",
  iso_store_visibility: CHECK_ACTION["iso-store-visibility"],
  isolation_not_enforced: ISOLATION,
  image_import_failed: "Open Template updates to see the import, then Retry. If it keeps failing, install a newer ProxCloud release.",
  image_import_stalled: "Open Template updates to see the import, then Retry. If it keeps failing, install a newer ProxCloud release.",
  image_blocked: "This image is blocked. Install a newer ProxCloud release.",
  seed_clone_failed: "Fix the storage or the node named in the message, then Retry.",
  seed_config_refused: "Fix the storage or the node named in the message, then Retry.",
  seed_not_booted: "Open the seed VM's console in Proxmox. Retry builds a new seed when this one never became ready.",
  seed_api_unreachable: "Add a route from the ProxCloud host to the platform network, or correct the seed API sources. Retry writes the seed's firewall rules again first.",
  seed_tls_mismatch: "Another server answers on the seed address. Correct the address.",
  seed_identity_lost: "Press Retry. It builds the seed again.",
  seed_cd_not_deleted: `${CAPI_TOKEN} cannot delete on the ISO store. Run the identities script again, then Retry.`,
  seed_lost: "The seed VM is gone or is no longer this installation's. It is not built again on its own. Remove the installation with the option for a missing seed, then install again.",
  component_unhealthy: "The message names the component and its condition. Download the seed kubeconfig to look closer, then Retry.",
  capi_init_failed: "The cluster manager did not start on the seed. Download the seed kubeconfig to look closer, then Retry.",
  machine_failed: "Check the boot store, the nodes and the Proxmox API URL in the settings, then Retry.",
  capmox_auth_failed: `Proxmox refused ${CAPI_TOKEN}. Check the Proxmox API URL and paste the token secret again, then Retry.`,
  workload_api_unreachable: "Add a route from the ProxCloud host to the API address.",
  bom_unsupported: "The installed version is too old for this release. Upgrade through the previous ProxCloud release first.",
  provider_conflict: "A provider row of this installation was refused. Read the message, correct that provider under Cloud providers, then Retry.",
  machine_delete_stuck: "Delete the VM named in the message in Proxmox, then start Remove again.",
  tenants_exist: "Tenant clusters or databases still run on the platform. Delete them, then start Remove again.",
  volume_delete_failed: `Check ${CSI_TOKEN} and the storage named in the message, then start Remove again.`,
  credentials_missing: "Paste both token secrets in step 2, then Retry.",
  stage_unavailable: "This release cannot run this stage for this installation.",
  platform_disabled: "Turn managed services on with ./proxcloud-up.sh install --managed-services.",
  portal_changed: RETRY,
  job_dead: RETRY,
}
export const codeAction = (code: string) => CODE_ACTION[code] ?? (code.endsWith("_timeout") ? RETRY : "Read the message, correct it, then Retry.")

export const STAGE_TITLE: Record<string, string> = {
  validate: "Check the settings and Proxmox",
  images: "Import the two images",
  "seed-vm": "Create the seed VM",
  "seed-api": "Reach the seed",
  "seed-identity": "Secure the seed",
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
