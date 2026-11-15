// Managed services: /admin/platform (internal/platform/admin/platform.go), enough to walk the whole page without a
// cluster. The first apply run fails at "seed-api" with seed_api_unreachable and every later one succeeds, so Retry has
// something to do; the preflight fails "credentials" until both token secrets are stored. A run moves one stage per
// STAGE_MS of wall time. `?mock-platform=<preset>` in the URL starts the page in one state:
// disabled | draft | running | failed | ready | removing (none: enabled, nothing saved).
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
}

const fail = (status: number, code: string, message: string, details: Doc = {}) => new ApiError(status, status, message, { code, ...details })
const now = () => new Date().toISOString()

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
const activeRun = (id: string) => mockPlatform.runs.find((r) => r.installationId === id && !["succeeded", "failed", "cancelled"].includes(tick(r).state))

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
  in_.rev++
  return run
}

// tick moves a run to where the clock says it is, and its installation with it.
function tick(run: Doc): Doc {
  if (["succeeded", "failed", "cancelled"].includes(run.state)) return run
  const in_ = mockPlatform.installations.find((i) => i.id === run.installationId)!
  const end = (state: string, phase: string, code?: string, message?: string) => {
    Object.assign(run, { state, code, message, finishedAt: now() })
    in_.status.phase = phase
    in_.rev++
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
    if (i < at) Object.assign(s, { state: "done", finishedAt: t(i + 1) })
    else s.state = "running"
    if (s.id === "seed-identity" && s.state === "done") seedStatus(in_)
  }
  if (at >= run.stages.length) {
    if (run.kind === "remove") {
      end("succeeded", "removed")
      in_.status.seed = undefined
      delete mockPlatform.secrets[in_.id]
      for (const s of db.services) if (s.config?.platformInstallationId === in_.id) s.status = "DISABLED"
    } else {
      end("succeeded", "ready")
      ready(in_)
    }
  }
  return run
}

function ready(in_: Doc) {
  const provider = (kind: string, label: string) => {
    let svc = db.services.find((s) => s.config?.platformInstallationId === in_.id && s.config?.provider === kind)
    if (!svc) {
      svc = { id: db.nextId("svc"), name: `${in_.name} ${label}`, type: "CLOUD", status: "ACTIVE", config: { provider: kind, platformInstallationId: in_.id, attachToNewProjects: false } }
      db.services.push(svc)
    }
    return svc.id as string
  }
  seedStatus(in_)
  Object.assign(in_.status, {
    phase: "ready", bom: bomRef,
    clusters: [{ name: "pc-platform", namespace: "pc-platform", endpoint: in_.spec.clusters[0].endpoint, kubernetesVersion: bom.kubernetes.version, argocdTokenExpiresAt: new Date(Date.now() + 365 * 86_400_000).toISOString() }],
    components: bom.components.map((c) => ({ name: c.name, cluster: c.cluster, version: c.chart.version, managed: true })),
    providers: { kubernetes: provider("kamaji", "Kubernetes"), database: provider("dbaas", "Databases") },
  })
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
    row("isolation", true, "The platform addresses are blocked networks; the cluster firewall rules are written with the first tenant network.", ""),
    row("credentials", both, "Both platform tokens are stored.", `No secret is stored for ${[!sec.capi && "capmox@pve!capi", !sec.csi && "csi@pve!csi"].filter(Boolean).join(" and ")}.`),
    ...["capi", "csi"].map((t) => row(`privileges:${t}`, both, `The ${t} token holds its privileges.`, `The ${t} token could not be checked: no secret is stored.`)),
    row("privileges:kubemint", true, "The portal token holds the ProxCloudKubeMint privileges.", ""),
    row("pool-fence", true, "Neither the portal token nor csi@pve!csi reaches the seed's pool, and the portal token does not reach proxcloud-system.", ""),
    row("iso-store-visibility", both, `capmox@pve!capi sees one ISO store, ${iso}.`, "The ISO stores capmox@pve!capi sees could not be read: no secret is stored."),
    row("boot-iso-grant", true, `capmox@pve may upload and delete bootstrap ISOs on ${iso}.`, ""),
    row("boot-iso", true, `${iso} is node-local, takes ISOs, and is the only ISO store of the boot storage.`, ""),
    row("placement", true, "Every node an image goes to has the import and the boot store.", ""),
    row("images", true, "Neither BOM image is on the blocklist.", ""),
  ]
}

// The API's own rules, as far as the page's states need them: the name, and one rule only the server can know.
function refuse(name: string, spec: Doc) {
  const problems: string[] = []
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
  if (preset === "failed") tick(newRun(in_, "apply", Date.now() - 60_000))
  if (preset === "ready" || preset === "removing") {
    mockPlatform.applies = 1
    tick(newRun(in_, "apply", Date.now() - 3_600_000))
    if (preset === "removing") newRun(in_, "remove", Date.now() - 0.5 * STAGE_MS)
  }
}

on("GET /admin/platform/bom", () => ({ data: bom }))

on("GET /admin/platform/installations", () => {
  if (!mockPlatform.enabled) throw fail(404, "platform_disabled", "Managed services are off on this site: set PROXCLOUD_PLATFORM_ENABLED=true")
  mockPlatform.runs.forEach(tick)
  return { data: mockPlatform.installations.map(view) }
})

on("POST /admin/platform/installations", ({ opts }) => {
  const b = (opts.body ?? {}) as Doc
  if (!mockPlatform.enabled) find("")
  refuse(b.name, b.spec)
  if (mockPlatform.installations.some((i) => i.serviceId === b.serviceId)) throw fail(409, "installation_exists", "this provider already has an installation")
  return { data: view(create(b.name, { ...b.spec, seed: { mode: "vm", pool: "proxcloud-seed", ...b.spec.seed }, nodeImage: { mode: "bom" } })) }
})

on("GET /admin/platform/installations/:id", ({ params, query }) => {
  const in_ = find(params.id)
  mockPlatform.runs.forEach(tick)
  if (query.get("live") !== "1") return { data: view(in_) }
  const components = in_.status.phase === "ready" ? bom.components.map((c) => ({ name: c.name, cluster: c.cluster, sync: "Synced", health: "Healthy" })) : []
  return { data: { ...view(in_), live: { phase: in_.status.phase, components } } }
})

on("PUT /admin/platform/installations/:id", ({ params, opts }) => {
  const in_ = find(params.id)
  const b = (opts.body ?? {}) as Doc
  if (activeRun(in_.id)) throw fail(409, "run_active", "a run of this installation is not finished")
  if (b.rev !== in_.rev) throw fail(409, "rev_conflict", "the installation changed since it was read")
  refuse(b.name, b.spec)
  if (in_.status.bom) {
    const fixed = [
      b.name !== in_.name && "name",
      JSON.stringify(b.spec.network) !== JSON.stringify(in_.spec.network) && "spec.network",
      b.spec.seed.address !== in_.spec.seed.address && "spec.seed.address",
    ].filter(Boolean) as string[]
    if (fixed.length) throw fail(409, "fixed_fields", `These fields cannot change once the platform was installed (serviceId never): ${fixed.join(", ")}`, { problems: fixed })
  }
  Object.assign(in_, { name: b.name, spec: structuredClone(b.spec), rev: in_.rev + 1, updatedAt: now() })
  return { data: view(in_) }
})

on("PUT /admin/platform/installations/:id/credentials", ({ params, opts }) => {
  const in_ = find(params.id)
  const b = (opts.body ?? {}) as Doc
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

on("GET /admin/platform/installations/:id/plan", ({ params }) => {
  const in_ = find(params.id)
  return { data: { installed: in_.status.bom ?? null, target: bomRef, changes: [], blocking: [] } }
})

on("POST /admin/platform/installations/:id/runs", ({ params, opts }) => {
  const in_ = find(params.id)
  const b = (opts.body ?? {}) as Doc
  if (b.kind !== "apply" && b.kind !== "remove") throw fail(400, "invalid_kind", "kind is apply or remove")
  if (b.confirm && (b.kind !== "remove" || b.confirm !== `remove ${in_.name}`)) throw fail(400, "invalid_confirmation", `confirm must be "remove ${in_.name}"`)
  if (activeRun(in_.id)) throw fail(409, "run_active", "a run of this installation is not finished")
  return { data: { runId: newRun(in_, b.kind).id } }
})

const publicRun = (run: Doc) => {
  const { t0: _t0, failAt: _failAt, cancelAt: _cancelAt, ...out } = tick(run)
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
  if (["succeeded", "failed", "cancelled"].includes(tick(run).state)) throw fail(409, "run_finished", "the run is finished")
  Object.assign(run, { cancelRequested: true, cancelAt: Date.now() + STAGE_MS }) // honoured at the runner's next wake-up
  return { data: publicRun(run) }
})

on("GET /admin/platform/installations/:id/seed-kubeconfig", ({ params }) => {
  const in_ = find(params.id)
  if (!in_.status.seed?.readyAt) throw fail(409, "no_seed", "the installation has no ready seed")
  const kubeconfig = `apiVersion: v1\nkind: Config\nclusters:\n- name: pc-seed\n  cluster:\n    server: ${in_.status.seed.apiServer}\nusers:\n- name: pc-seed\n  user:\n    token: mock-break-glass-token\n`
  return { data: { kubeconfig, expiresAt: new Date(Date.now() + 3_600_000).toISOString() } }
})

on("POST /admin/platform/installations/:id/seed-credential/rotate", ({ params }) => {
  const in_ = find(params.id)
  if (activeRun(in_.id)) throw fail(409, "run_active", "a run of this installation is not finished")
  if (!in_.status.seed?.readyAt) throw fail(409, "no_seed", "the installation has no ready seed")
  const n = Number(/g(\d+)$/.exec(in_.status.seed.tokenSecret)?.[1] ?? 1) + 1
  in_.status.seed = { ...in_.status.seed, generation: n, tokenSecret: `proxcloud-portal-token-g${n}` }
  in_.rev++
  return { data: view(in_) }
})
