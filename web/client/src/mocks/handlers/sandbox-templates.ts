// Stateful console-only mock. URL project scope is authoritative, including for
// reads. Credentials are inspected transiently and never copied into fixtures,
// query/mutation caches, diagnostics, errors or logs.
import { ApiError } from "@/lib/api"
import type { ProjectSandboxTemplate, SandboxTemplateCoverage } from "@/lib/types"
import { activeBuild, templateNamePattern, validCommand, validImageSource, validRegistryCredentials, validRuntimeUser, validWorkdir } from "@/pages/platform/sandbox-template-utils"
import { db } from "../db"
import type { MockSandboxTemplate } from "../fixtures/cloud"
import { on } from "../router"

const digest = `sha256:${"a".repeat(64)}`
const resourceDefaults = { buildCpu: 4, buildMemoryGiB: 16, buildDiskGiB: 80 }
const runs = new Map<string, number>()
const warms = new Map<string, { pulled: number; expiresAt: string }>()
const data = <T,>(value: T) => ({ data: value })
const flag = (name: string) => typeof window !== "undefined" && new URLSearchParams(window.location.search).has(name)
const busy = (t: ProjectSandboxTemplate) => activeBuild(t.status) || t.builds.some((b) => activeBuild(b.status))
function project(pid: string) {
  if (!db.projects.some((p) => p.id === pid)) throw new ApiError(404, 404, "Project not found")
  if (flag("template-manage-denied")) throw new ApiError(403, 403, "Project cloud resource management permission required")
}
function owned(pid: string, id: string) {
  project(pid)
  const t = db.sandboxTemplates.find((item) => item.projectId === pid && item.id === id)
  if (!t) throw new ApiError(404, 404, "Template not found")
  return t
}
function view(t: MockSandboxTemplate): ProjectSandboxTemplate {
  const { projectId: _private, ...safe } = structuredClone(t)
  return safe
}
function named(pid: string, name: string) {
  return db.sandboxTemplates.find((t) => t.projectId === pid && (t.name === name || t.aliases?.includes(name)))
}
function validName(value: unknown) {
  if (typeof value !== "string" || !templateNamePattern.test(value)) throw new ApiError(400, 400, "Use a valid template name")
  return value
}
function bodyObject(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ApiError(400, 400, "Object body required")
  return value as Record<string, unknown>
}
function integer(value: unknown, max: number, field: string) {
  if (typeof value !== "number" || !Number.isInteger(value) || value < 1 || value > max) throw new ApiError(400, 400, `${field} is outside location limits`)
  return value
}
function runtime(body: Record<string, unknown>) {
  const cpu = integer(body.cpuCount, 8, "Runtime vCPU")
  const memoryMB = integer(body.memoryMB, 28 * 1024, "Runtime memory")
  if (memoryMB < 1024 || memoryMB % 1024) throw new ApiError(400, 400, "Use whole GiB runtime memory")
  for (const name of ["startCmd", "readyCmd"] as const) if (body[name] !== undefined && (typeof body[name] !== "string" || !validCommand(body[name]))) throw new ApiError(400, 400, "Invalid startup command")
  return { cpu, memoryGiB: memoryMB / 1024 }
}
function freshName(pid: string, name: string) {
  if (name === "base" || named(pid, name)) throw new ApiError(409, 409, "Name or alias already exists")
}

on("GET /project/:pid/sandbox-templates/capabilities", ({ params }) => {
  project(params.pid)
  if (flag("capabilities-error")) throw new ApiError(503, 503, "Location capabilities unavailable")
  return data({
    importEnabled: params.pid !== "prj-staging", trustedWorkerAvailable: params.pid !== "prj-staging", warmingEnabled: params.pid !== "prj-staging",
    runtime: { cpu: 2, memoryGiB: 2, maxCpu: 8, maxMemoryGiB: 28 },
    build: { cpu: 4, memoryGiB: 16, diskGiB: 80, maxCpu: 12, maxMemoryGiB: 48, maxDiskGiB: 160, maxConcurrent: 2 },
  })
})

on("GET /project/:pid/sandbox-templates", ({ params }) => {
  project(params.pid)
  for (const t of db.sandboxTemplates.filter((t) => t.projectId === params.pid && activeBuild(t.status))) {
    const n = (runs.get(t.id) ?? 0) + 1
    runs.set(t.id, n)
    if (n >= 2) {
      t.status = t.name.includes("fail") ? "error" : "ready"
      t.ready = t.status === "ready" || t.ready
      t.builds[0].status = t.status
      t.builds[0].finishedAt = new Date().toISOString()
      if (t.status === "error") t.builds[0].reason = "A build step failed (exit 137, OOMKilled)"
    }
  }
  const templates = db.sandboxTemplates.filter((t) => t.projectId === params.pid).map(view)
  if (flag("older-active-build")) {
    const target = templates.find((t) => t.name === "web-app")
    if (target) target.builds.push({ buildID: "older-active-worker", status: "building", tags: [] })
  }
  return data(templates)
})

on("POST /project/:pid/sandbox-templates/import", async ({ params, opts }) => {
  project(params.pid)
  const b = bodyObject(opts.body), name = validName(b.name)
  if (name === "slow-import") await new Promise((resolve) => setTimeout(resolve, 800))
  if (params.pid === "prj-staging") throw new ApiError(503, 503, "Import is disabled at this location")
  // This mock operator approves exact repositories, not arbitrary host/namespace
  // prefixes. Production policy is exclusively enforced by the backend.
  const approved = ["registry.example.com/approved/agent", `registry.example.com/templates/${params.pid}/base`]
  if (typeof b.source !== "string" || !validImageSource(b.source) || !approved.includes(b.source.split("@")[0])) throw new ApiError(403, 403, "Source is not operator-approved for this project")
  freshName(params.pid, name)
  if (b.registry) {
    const credentials = bodyObject(b.registry)
    if (typeof credentials.username !== "string" || typeof credentials.password !== "string" || !validRegistryCredentials(credentials.username, credentials.password)) throw new ApiError(400, 400, "Supply valid registry credentials")
  }
  if (name.includes("denied")) throw new ApiError(403, 403, "Source image pull verification failed")
  if (b.user !== undefined && (typeof b.user !== "string" || !validRuntimeUser(b.user))) throw new ApiError(400, 400, "Invalid runtime user")
  if (b.workdir !== undefined && (typeof b.workdir !== "string" || !validWorkdir(b.workdir))) throw new ApiError(400, 400, "Invalid working directory")
  const size = runtime(b), id = db.nextId("template"), buildID = db.nextId("import"), now = new Date().toISOString()
  db.sandboxTemplates.unshift({
    id, projectId: params.pid, name, aliases: [name], mode: "import", kind: "template", ...size,
    status: "building", ready: false, createdAt: now, updatedAt: now,
    builds: [{ buildID, mode: "import", status: "building", tags: ["default"], source: b.source, image: b.source, digest: b.source.split("@")[1],
      ...(typeof b.startCmd === "string" ? { startCmd: b.startCmd } : {}), ...(typeof b.readyCmd === "string" ? { readyCmd: b.readyCmd } : {}), createdAt: now }],
  })
  return data({ templateID: id, buildID, status: "building", aliases: [name], reused: false })
})

for (const method of ["POST", "DELETE"]) on(`${method} /project/:pid/sandbox-templates/:tid/aliases`, ({ params, opts }) => {
  const t = owned(params.pid, params.tid)
  if (busy(t) || flag("older-active-build")) throw new ApiError(409, 409, "Wait for the active worker before changing aliases")
  const requested = bodyObject(opts.body).aliases
  if (!Array.isArray(requested) || requested.length === 0) throw new ApiError(400, 400, "Provide aliases")
  let current = t.aliases ?? [t.name]
  for (const item of requested) {
    const name = validName(item)
    if (name === t.name && method === "DELETE") throw new ApiError(400, 400, "The primary name cannot be removed")
    if (method === "POST") {
      const conflict = named(params.pid, name)
      if (name === "base" || conflict && conflict.id !== t.id) throw new ApiError(409, 409, "Alias already belongs to another template")
      if (!current.includes(name)) current = [...current, name]
    } else current = current.filter((a) => a !== name)
  }
  if (current.length > 20) throw new ApiError(400, 400, "At most 19 additional aliases")
  t.aliases = current
  return data({ aliases: current })
})

for (const method of ["POST", "GET"]) on(`${method} /project/:pid/sandbox-templates/:tid/prepare`, ({ params, opts }) => {
  const t = owned(params.pid, params.tid)
  if (method === "POST" && opts.body !== undefined) throw new ApiError(400, 400, "Prepare takes no body")
  if (!t.ready) throw new ApiError(409, 409, "Wait for a ready artifact")
  const image = t.builds.find((b) => b.status === "ready")?.image ?? `registry.example.com/templates/${params.pid}/${t.id}@${digest}`
  if (params.pid === "prj-staging") return data({ enabled: false, image, pool: "cpu", pulled: 0, total: 0, ready: false })
  if (t.name.includes("warm-error")) throw new ApiError(503, 503, "Preparation is temporarily unavailable; retry shortly")
  const key = `${params.pid}/${t.id}`
  if (method === "POST") warms.set(key, { pulled: 0, expiresAt: new Date(Date.now() + 86400000).toISOString() })
  const warm = warms.get(key)
  if (warm && method === "GET") warm.pulled = Math.min(3, warm.pulled + 1)
  const pulled = warm?.pulled ?? 0
  return data<SandboxTemplateCoverage>({ enabled: true, image, pool: "cpu", pulled, total: 3, ready: !!warm && pulled === 3, ...(warm ? { expiresAt: warm.expiresAt } : {}) })
})

on("POST /project/:pid/sandbox-templates/:tid/builds", ({ params, opts }) => {
  const base = owned(params.pid, params.tid), b = bodyObject(opts.body), name = validName(b.name)
  if (!base.ready || busy(base) || flag("older-active-build")) throw new ApiError(409, 409, "Base is not ready or has an active worker")
  freshName(params.pid, name)
  const size = runtime(b), buildCpu = integer(b.buildCpu, 12, "Builder vCPU"), buildMemoryGiB = integer(b.buildMemoryGiB, 48, "Builder memory"), buildDiskGiB = integer(b.buildDiskGiB, 160, "Builder disk")
  if (!Array.isArray(b.steps) || b.steps.length < 1 || b.steps.length > 100 || b.steps.some((s: unknown) => {
    if (!s || typeof s !== "object") return true
    const step = s as { type?: unknown; args?: unknown[] }
    return step.type !== "RUN" || !Array.isArray(step.args) || step.args.length < 1 || step.args.length > 2 || typeof step.args[0] !== "string" || !step.args[0].trim() || !validCommand(step.args[0]) || (step.args[1] !== undefined && (typeof step.args[1] !== "string" || !validRuntimeUser(step.args[1])))
  })) throw new ApiError(400, 400, "Provide valid RUN steps")
  const id = db.nextId("template"), buildID = db.nextId("build"), now = new Date().toISOString()
  db.sandboxTemplates.unshift({ id, projectId: params.pid, name, kind: "template", mode: "build", aliases: [name], ...size,
    ready: false, status: "building", builds: [{ buildID, status: "building", mode: "build", source: base.name, tags: ["default"], buildResources: { buildCpu, buildMemoryGiB, buildDiskGiB }, createdAt: now }], createdAt: now, updatedAt: now })
  return data({ templateID: id, buildID, status: "building" })
})

on("GET /project/:pid/sandbox-templates/:tid/builds/:bid/logs", ({ params, query }) => {
  const t = owned(params.pid, params.tid), b = t.builds.find((entry) => entry.buildID === params.bid)
  if (!b) throw new ApiError(404, 404, "Build not found")
  const offset = Number(query.get("logsOffset") ?? 0)
  if (!Number.isSafeInteger(offset) || offset < 0) throw new ApiError(400, 400, "logsOffset must be non-negative")
  const resources = b.buildResources ?? resourceDefaults, failed = b.status === "error"
  const diagnostics = {
    revision: 2, stages: [
      { phase: "base", state: "terminated", reason: "Completed", exitCode: 0, cpu: "1", memory: "512Mi", disk: `${resources.buildDiskGiB}Gi` },
      { phase: "kaniko", state: activeBuild(b.status) ? "running" : "terminated", reason: failed ? "OOMKilled" : "Completed", exitCode: failed ? 137 : 0, signal: failed ? 9 : 0, cpu: String(resources.buildCpu), memory: `${resources.buildMemoryGiB}Gi`, disk: `${resources.buildDiskGiB}Gi` },
    ], warnings: failed ? ["A log segment could not be captured"] : [], truncated: failed, logsUnavailable: failed,
    lossReason: failed ? "Some output was unavailable after the build ended" : undefined,
    archivedAt: activeBuild(b.status) ? undefined : new Date().toISOString(), terminalTail: failed ? ["[kaniko] final stderr: allocation failed"] : [],
  }
  const allLogs = ["[base] Retrieving image manifest", "[kaniko] \u001b[36mINFO\u001b[0m Executing RUN step", "\u001b]8;;https://example.invalid\u0007literal <script>not HTML</script>\u001b]8;;\u0007", failed ? "[kaniko] ERROR build step exited 137" : "[push] Image artifact ready"]
  return data({ status: b.status, reason: b.reason, step: failed ? "kaniko" : b.step, logs: allLogs.slice(offset), nextLogsOffset: allLogs.length, buildResources: resources, diagnostics })
})

on("DELETE /project/:pid/sandbox-templates/:tid", ({ params }) => {
  const t = owned(params.pid, params.tid)
  if (busy(t) || flag("older-active-build")) throw new ApiError(409, 409, "Wait for the active worker before deleting; retry after it finishes")
  db.sandboxTemplates = db.sandboxTemplates.filter((item) => item.projectId !== params.pid || item.id !== t.id)
  return data({})
})
