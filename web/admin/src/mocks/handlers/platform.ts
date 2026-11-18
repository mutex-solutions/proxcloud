// Managed services: /admin/platform (internal/platform/admin/platform.go), enough to walk the whole page without a
// cluster. The first apply run fails at "seed-api" with seed_api_unreachable and every later one succeeds, so Retry has
// something to do; the preflight fails "credentials" until both token secrets are stored. A run moves one stage per
// STAGE_MS of wall time. Like the API, `rev` moves only when the settings are saved, and every write refuses a body
// field the API's structs do not have. `?mock-platform=<preset>` in the URL starts the page in one state:
// disabled | draft | isolation (the isolation check fails until a provider sync) | norunner (runs answer 409) |
// running | waiting (stage 4 is trying again) | failed | dead (the run's job died: the stored phase stays applying) |
// ready | previous (ready, installed by the release before this one: the plan names a newer release and why this one does not move the platform to it) |
// upgrade (the same, and the move is allowed: the plan lists it, the run wants the typed confirmation) |
// replace (the same, with an unfinished upgrade toward the release of an earlier ProxCloud: the plan also offers the move to this one's in its place) | degraded (one component out of sync) | simplezone (the check warns: no tenant cluster on a simple zone) | silent (the seed does not answer) | unread (the live read fails) | removing
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

// "Publish on all nodes" per "<provider>/<digest>": the step each node is at (handlers/setup.ts moves it). It lives
// here because this module is loaded first and reads it when a run registers the providers.
export const publishAllState: Record<string, Record<string, number>> = {}

export const mockPlatform = {
  enabled: preset !== "disabled",
  installations: [] as Doc[],
  secrets: {} as Record<string, { capi?: boolean; csi?: boolean; enrolledAt?: number }>, // which secrets are stored (and when the last enrollment line was printed); never their values
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
const MACHINE: Shape = { node: true, cores: true, memoryMiB: true, diskGiB: true }
const INSTALLATION: Shape = {
  name: true, serviceId: true, provisionedBy: true, layout: true, rev: true,
  spec: {
    pveApiUrl: true, storage: { boot: true, iso: true, import: true },
    network: { mode: true, bridge: true, vlan: true, prefix: true, gateway: true, dns: true, mtu: true },
    seed: { mode: true, pool: true, node: true, address: true, cores: true, memoryMiB: true, diskGiB: true, sshKeys: true, apiSources: true },
    clusters: [{ name: true, roles: true, endpoint: true, nodeRange: true, metallbRange: true, controlPlane: MACHINE,
      nodeGroups: [{ name: true, nodes: true, count: true, cores: true, memoryMiB: true, diskGiB: true }] }],
    nodeImage: { mode: true, templates: true },
    tenants: { attach: true },
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
    controlPlane: { node: "pve1", cores: 2, memoryMiB: 4096, diskGiB: 30 },
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
  ],
}
const bomRef = { release: bom.release, sha256: bom.sha256 }
// The preset "previous": the release that installed the platform, which this one carries as a previous BOM.
const previous = { ...bom, release: "0.6.0", sha256: "1e0d9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d" }

const SCHEDULER_NOTE = "The job scheduler is off. Until it is on, a tenant cannot delete a project, the state of clusters, databases and servers is refreshed only when an operator runs a provider sync, and no usage is charged or billed. Turn it on where ProxCloud is installed: ./proxcloud-up.sh install --scheduler on a Compose install, PROXCLOUD_JOBS_SCHEDULER_ENABLED=true for the API elsewhere. It starts charging usage on a timer, so set the prices first."

const find = (id: string) => {
  if (!mockPlatform.enabled) throw fail(404, "platform_disabled", "Managed services are off on this site: set PROXCLOUD_PLATFORM_ENABLED=true")
  const in_ = mockPlatform.installations.find((i) => i.id === id)
  if (!in_) throw fail(404, "not_found", "installation not found")
  return in_
}
const view = (in_: Doc) => {
  const sec = mockPlatform.secrets[in_.id] ?? {}
  // The mock API runs without the job scheduler, like a plain Compose install.
  return { ...in_, schedulerNote: SCHEDULER_NOTE, credentials: { ...(sec.capi ? { capi: { tokenId: "capmox@pve!capi" } } : {}), ...(sec.csi ? { csi: { tokenId: "csi@pve!csi" } } : {}) } }
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
  const k8s = provider("kamaji", "Kubernetes"), dbs = provider("dbaas", "Databases")
  // Stage 10: a changed attach choice is applied once, and the BOM's version is offered where its image is published.
  const choice = in_.spec.tenants?.attach ?? "none"
  if (choice !== (in_.status.tenantsAttach ?? "none")) {
    for (const s of db.services) if (s.id === k8s || s.id === dbs) s.config.attachToNewProjects = choice !== "none"
    in_.status.tenantsAttach = choice
  }
  offerVersion(in_, k8s)
  Object.assign(in_.status, {
    phase: "ready", bom: bomRef,
    clusters: [{ name: "pc-platform", namespace: "pc-platform", endpoint: in_.spec.clusters[0].endpoint, kubernetesVersion: bom.kubernetes.version, argocdTokenExpiresAt: new Date(Date.now() + 365 * 86_400_000).toISOString() }],
    components: bom.components.map((c) => ({ name: c.name, cluster: c.cluster, version: c.chart.version, managed: true })),
    providers: { kubernetes: k8s, database: dbs },
  })
}

// offerVersion is platformlife.NodeImageLine and OfferVersion over the mock's publish-all state: the BOM's version is
// added to the Kubernetes provider when every node publishes the node image, and never over a version the row names.
function offerVersion(in_: Doc, serviceId: string) {
  const st = publishAllState[`${in_.serviceId}/${bom.kubernetes.nodeImage.digest}`] ?? {}
  const nodes = Object.keys(st)
  const missing = nodes.length ? nodes.filter((n) => st[n] < 4) : [in_.spec.clusters[0].controlPlane.node]
  const line = nodes.map((n, i) => `${n}=${9300 + i}`).join(",")
  const svc = db.services.find((s) => s.id === serviceId)!
  let versions = (svc.config.cluster?.versions ?? {}) as Doc
  const offered = !missing.length && !(bom.kubernetes.version in versions)
  if (offered) {
    // A new list and a new row, as an API answer is: the page compares what it reads with what it has.
    versions = { ...versions, [bom.kubernetes.version]: line }
    db.services = db.services.map((s) => (s === svc ? { ...s, config: { ...s.config, cluster: { ...s.config.cluster, versions } } } : s))
  }
  return { version: bom.kubernetes.version, line, missing, offered, versions }
}

function checks(in_: Doc): Doc[] {
  const sec = mockPlatform.secrets[in_.id] ?? {}
  const both = !!sec.capi && !!sec.csi
  const iso = in_.spec.storage.iso
  const row = (id: string, ok: boolean, done: string, todo: string) => ({ id, status: ok ? "ok" : "fail", message: ok ? done : todo })
  return [
    row("flags", true, "Managed services and VM template updates are on, and a GitHub integration is saved.", ""),
    { id: "scheduler", status: "warn", message: SCHEDULER_NOTE },
    row("spec", true, "The installation is complete and consistent.", ""),
    row("network", true, "The platform network is apart from every tenant, sandbox and public network, and tenants can reach the MetalLB range.", ""),
    row("isolation", preset !== "isolation" || mockPlatform.synced, "The platform addresses are blocked networks; the cluster firewall rules are written by the next provider sync (with this installation saved it needs no project), or with the first tenant network.",
      "The seed address 10.90.0.5 and the API address 10.90.0.10 are not among the provider's blocked networks: add 10.90.0.5/32 and 10.90.0.10/32."),
    ...(preset === "simplezone" ? [{ id: "tenant-network", status: "warn", message: "The provider's default zone pcm0 has DHCP in Proxmox. A tenant Kubernetes cluster needs a network without DHCP, so a cluster create on a network of this zone is refused (400, \"Worker nodes need a network without DHCP\") until the default zone is an EVPN or a VLAN zone (config.network.zones and defaultZone, under Cloud providers). A Kubernetes provider with shared workers (cluster.shared) uses no tenant network and is not affected. The install and managed databases do not need it." }] : []),
    row("credentials", both, "Both platform tokens are stored.", `No secret is stored for ${[!sec.capi && "capmox@pve!capi", !sec.csi && "csi@pve!csi"].filter(Boolean).join(" and ")}: run the root command of step 2 on a node and paste the PC-ENROLL line it prints.`),
    ...["capi", "csi"].map((t) => row(`privileges:${t}`, both, `The ${t} token holds its privileges.`, `The ${t} token could not be checked: no secret is stored.`)),
    row("privileges:kubemint", true, "The portal token holds the ProxCloudKubeMint privileges.", ""),
    row("mint-scope", true, "The portal token may create tokens for the users of the group proxcloud-kube alone, and both are in it.", ""),
    row("pool-fence", true, "Neither the portal token nor csi@pve!csi reaches the seed's pool, and the portal token does not reach proxcloud-system.", ""),
    row("token-scope", true, "Neither platform token holds anything on /pool, /storage or /vms as a whole.", ""),
    row("iso-store-visibility", both, `capmox@pve!capi sees one ISO store, ${iso}.`, "The ISO stores capmox@pve!capi sees could not be read: no secret is stored."),
    row("boot-iso-grant", true, `capmox@pve may upload and delete bootstrap ISOs on ${iso}.`, ""),
    row("boot-iso", true, `${iso} is node-local, takes ISOs, and is the only ISO store of the boot storage.`, ""),
    row("placement", true, "Every node an image goes to has the import and the boot store.", ""),
    row("boot-store", true, `The boot store ${in_.spec.storage.boot} exists and takes disk images.${both ? " It holds no disk of a VM that is not the platform's." : " Whether it also holds other VMs' disks is read once the token csi@pve!csi is stored."}`, ""),
    row("pve-api-rule", true, in_.status.bom ? "The cluster firewall lets the seed and the node range reach the Proxmox API." : "Install writes the cluster firewall rule that lets the seed and the node range reach the Proxmox API, before the seed is made. Nothing to do by hand.", ""),
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

if (preset && preset !== "disabled") {
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
  const made = create(b.name, {
    ...b.spec,
    storage: { ...b.spec.storage, import: importOf(b.serviceId, b.spec) },
    seed: { mode: "vm", pool: "proxcloud-seed", ...b.spec.seed },
    clusters: (b.spec.clusters ?? []).map((c: Doc) => ({ name: "pc-platform", roles: ["kamaji", "dbaas"], ...c })),
    nodeImage: { mode: "bom" },
  })
  // What the first save writes into the provider (the API adds the exact CIDRs of the node range too; the mock names two).
  return { data: { ...view(made), siteChanges: { blockedCidrs: [`${made.spec.seed.address}/32`, `${made.spec.clusters[0].endpoint}/32`] } } }
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
  // A changed seed address: its old /32, which an earlier save added, goes and the new one comes (the API does the
  // same for the API address and the node range).
  const [was, is] = [in_.spec.seed.address, b.spec.seed?.address]
  Object.assign(in_, { name: b.name, spec: structuredClone(b.spec), rev: in_.rev + 1, updatedAt: now() })
  return { data: was === is ? view(in_) : { ...view(in_), siteChanges: { blockedCidrs: [`${is}/32`], blockedCidrsRemoved: [`${was}/32`] } } }
})

on("PUT /admin/platform/installations/:id/credentials", ({ params, opts }) => {
  const in_ = find(params.id)
  const b = (opts.body ?? {}) as Doc
  strict(b, { capi: TOKEN, csi: TOKEN, enroll: true })
  const sec = (mockPlatform.secrets[in_.id] ??= {})
  if (b.enroll) { // platformlife.ParseEnrollLine and Store.SaveEnrollment
    const no = (code: string, status: number, message: string) => fail(status, code, message)
    const malformed = no("invalid_enrollment", 400, "this is not the PC-ENROLL line the root command of step 2 prints: copy the whole line, from PC-ENROLL: to its end")
    if (b.capi || b.csi) throw fail(400, "invalid_credentials", "The credentials are not valid", { problems: ["enroll goes without capi and csi"] })
    if (!String(b.enroll).startsWith("PC-ENROLL:")) throw malformed
    let f: string[]
    try { f = atob(String(b.enroll).slice(10)).split(":") } catch { throw malformed }
    const isSecret = (v: string) => /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/.test(v)
    if (f.length !== 5 || f[0] !== "1" || !(Number(f[2]) > 0) || (!f[3] && !f[4]) || [f[3], f[4]].some((v) => v && !isSecret(v))) throw malformed
    if (f[1] !== in_.id) throw no("invalid_enrollment", 400, "this PC-ENROLL line is of another installation: run the root command shown in step 2 of this installation")
    if (Number(f[2]) > Date.now() / 1000 + 300) throw no("enrollment_clock_ahead", 400, "this PC-ENROLL line was printed by a node whose clock is ahead of the portal's by more than 5 minutes: nothing was changed. Set the node's clock (timedatectl shows it; chrony keeps it), then run the root command of step 2 again")
    if (Number(f[2]) <= (sec.enrolledAt ?? 0)) throw no("enrollment_replayed", 409, "this PC-ENROLL line was stored already, or a newer one was: nothing was changed. For new secrets run the root command of step 2 again")
    sec.enrolledAt = Number(f[2])
    if (f[3]) sec.capi = true
    if (f[4]) sec.csi = true
    return { data: view(in_) }
  }
  const uuid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/
  const problems = [["capi", "capmox@pve!capi"], ["csi", "csi@pve!csi"]].flatMap(([k, want]) =>
    !b[k] ? [] : b[k].tokenId !== want ? [`tokenId must be ${want}`] : !uuid.test(b[k].secret ?? "") ? [`the secret of ${want} is not a PVE token secret (a UUID)`] : [])
  if (!b.capi && !b.csi) problems.push("give capi, csi or both")
  if (problems.length) throw fail(400, "invalid_credentials", "The credentials are not valid", { problems })
  if (b.capi) sec.capi = true
  if (b.csi) sec.csi = true
  return { data: view(in_) } // the answer names the stored token ids, never a secret
})

on("POST /admin/platform/installations/:id/kubernetes-version", ({ params }) => {
  const in_ = find(params.id)
  const id = in_.status.providers?.kubernetes
  if (!id || !in_.status.bom) throw fail(409, "no_provider", "This installation has no Kubernetes provider yet: install it first.")
  const r = offerVersion(in_, id)
  if (r.missing.length) {
    throw fail(409, "node_image_unpublished", `No node image of Kubernetes ${r.version} is published on ${r.missing.join(", ")}. Publish ${bom.kubernetes.nodeImage.tag} there (Template updates, Publish on all nodes), then offer the version again.`,
      { version: r.version, missingNodes: r.missing })
  }
  return { data: { version: r.version, line: r.line, offered: r.offered, serviceId: id, versions: r.versions } }
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
