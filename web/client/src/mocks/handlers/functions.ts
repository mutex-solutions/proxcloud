// Functions (/project/:pid/functions): images only, internal endpoints only. A rollout settles on
// the second list read; an image with "fail" in its name does not become ready.
// Private registry (registryAuth): an image with "private" in its name is pulled only with
// credentials, and a password with "wrong" in it is refused by the registry.
// Scale: a function with "busy" in its name has load for every instance it may run, so it runs
// maxInstances of them; any other runs one.
// prj-aurora previews a location that runs functions; every other project one that does not.
// The mock keeps no secret value and no registry password: only names and when each was set.
import { ApiError } from "@/lib/api"
import { registryHost, serverHost } from "@/lib/format"
import type { CloudFunction, FunctionDeployment, FunctionLimits } from "@/lib/types"
import { PID } from "../fixtures/platform"
import { on } from "../router"

const DOMAIN = "sbx.example.com"
// The `functions` block of the sandbox service (GET /project/:pid/service).
export const FUNCTION_LIMITS: FunctionLimits = {
  domain: DOMAIN, minInstances: 0, maxMinInstances: 1, idleSeconds: 300, minIdleSeconds: 60, maxIdleSeconds: 3600,
  maxCpuMillis: 2000, maxMemoryMi: 4096, maxFunctions: 20, maxInstances: 5, defaultConcurrency: 50, maxConcurrency: 1000,
}

// pendingSetAt: the secret set a rollout in progress switches to. registryAuth is kept as a
// deployment records it, and its version is the time it was set.
type Fn = Omit<CloudFunction, "registryAuth" | "status"> & Pick<FunctionDeployment, "registryAuth"> & { projectId: string; deploying?: FunctionDeployment; reads: number; pendingSetAt?: Record<string, string>; wrongPassword?: boolean }
type Snapshot = Pick<FunctionDeployment, "image" | "port" | "cpuMillis" | "memoryMi" | "env" | "secretNames" | "registryAuth">
type SecretChanges = Record<string, string | null> | null
type RegistryAuthReq = { server?: string; username?: string; password?: string } | null
type Body = Partial<CloudFunction> & { secrets?: SecretChanges; registryAuth?: RegistryAuthReq } & Record<string, unknown>

const snapshotKeys = ["image", "port", "cpuMillis", "memoryMi", "env"] as const
const hex = (n: number) => Array.from({ length: n }, () => Math.floor(Math.random() * 16).toString(16)).join("")
const now = () => new Date().toISOString()
const snapshot = (s: Snapshot): Snapshot => structuredClone(Object.fromEntries([...snapshotKeys, "secretNames" as const, "registryAuth" as const].map((k) => [k, s[k]]))) as Snapshot

const deployments: FunctionDeployment[] = [
  { id: "fnd-0002", functionId: "fn-00000000000000a1", seq: 2, image: "registry.example.com/acme/hello-api:1.1", imageDigest: "sha256:" + "b".repeat(64), port: 8080, cpuMillis: 250, memoryMi: 256, env: { LOG_LEVEL: "info" }, secretNames: ["DB_PASSWORD"], state: "ACTIVE", createdAt: "2026-11-14T09:00:00Z", finishedAt: "2026-11-14T09:00:08Z" },
  { id: "fnd-0001", functionId: "fn-00000000000000a1", seq: 1, image: "registry.example.com/acme/hello-api:1.0", imageDigest: "sha256:" + "a".repeat(64), port: 8080, cpuMillis: 250, memoryMi: 256, state: "SUPERSEDED", createdAt: "2026-11-10T09:00:00Z", finishedAt: "2026-11-10T09:00:07Z" },
]
let fns: Fn[] = [
  {
    projectId: PID, reads: 0,
    id: "fn-00000000000000a1", name: "hello-api", image: "registry.example.com/acme/hello-api:1.1", port: 8080, cpuMillis: 250, memoryMi: 256,
    env: { LOG_LEVEL: "info" }, secretNames: ["DB_PASSWORD"], secretsSetAt: { DB_PASSWORD: "2026-11-14T09:00:00Z" },
    minInstances: 0, idleSeconds: 300, maxInstances: 1, concurrency: 50,
    host: `hello-api-3fa9c1.${DOMAIN}`, url: `https://hello-api-3fa9c1.${DOMAIN}`,
    instances: 0, suspended: false,
    currentDeployment: { id: "fnd-0002", seq: 2, image: "registry.example.com/acme/hello-api:1.1", deployedAt: "2026-11-14T09:00:08Z" },
    lastActiveAt: "2026-11-15T16:20:00Z", createdAt: "2026-11-10T09:00:00Z", updatedAt: "2026-11-14T09:00:08Z",
  },
]

function enabled(pid: string) {
  if (pid !== PID) throw new ApiError(403, 403, "functions are not offered here")
}
// Where functions are off, reads and DELETE stay open, so what is left can be seen and removed;
// create, update, rollback and the actions are refused.
function owned(pid: string, id: string, change = false) {
  if (change) enabled(pid)
  const f = fns.find((x) => x.id === id && x.projectId === pid)
  if (!f) throw new ApiError(404, 404, "function not found")
  return f
}
function view(f: Fn): CloudFunction {
  const { projectId: _p, deploying: _d, reads: _r, pendingSetAt: _s, wrongPassword: _w, registryAuth: ra, ...v } = structuredClone(f)
  const status = f.suspended ? "SUSPENDED" : f.deploying ? "DEPLOYING" : !f.currentDeployment ? "FAILED" : f.instances > 0 ? "READY" : "IDLE"
  return { ...v, ...(ra ? { registryAuth: { server: ra.server, username: ra.username, setAt: ra.version } } : {}), status, ...(f.deploying ? { reason: "waiting for the new instance to become ready" } : {}) }
}
// What this release does not offer is refused by name, not ignored.
function checkOffered(b: Body) {
  if (b.exposure !== undefined && b.exposure !== "private") throw new ApiError(400, 400, "public endpoints are not offered")
  for (const [key, what] of [["gpu", "GPU functions"], ["domains", "custom hostnames"], ["source", "builds from source"]]) {
    if (b[key] != null) throw new ApiError(400, 400, `${what} are not offered`)
  }
}
// The machines pull from the host the image names: a DNS name, never an address or localhost.
function checkImage(image: string) {
  const host = registryHost(image).replace(/:\d+$/, "")
  if (!image || /\s/.test(image) || host === "localhost" || /^\d+(\.\d+){3}$/.test(host) || /[[:]/.test(host)) throw new ApiError(400, 400, "image: the registry must be a DNS name (not an IP address, not localhost)")
}
// registryAuth as a request gives it → as a snapshot keeps it. The password is dropped here.
function describeRegistry(r: NonNullable<RegistryAuthReq>, image: string): Snapshot["registryAuth"] {
  if (!r.password) throw new ApiError(400, 400, "registryAuth.password is required whenever registryAuth is set (it is write-only and cannot be read back)")
  if (!r.username) throw new ApiError(400, 400, "registryAuth.username is required (at most 255 characters)")
  const host = registryHost(image)
  return { server: r.server?.trim() || (host === "docker.io" ? "https://index.docker.io/v1/" : host), username: r.username, version: now() }
}
// The kubelet offers credentials only to the server they are stored for.
function checkRegistry(s: Snapshot) {
  if (!s.registryAuth) return
  const got = serverHost(s.registryAuth.server), want = registryHost(s.image)
  if (got !== want) throw new ApiError(400, 400, `the registry credentials are for ${got} but the image is pulled from ${want}: send registryAuth for that registry, or null to remove the credentials`)
}
// Secrets are merged: a string sets or replaces a name, null removes it, the rest stay. Returns
// the names and the set times, or undefined when the request changes nothing.
function mergeSecrets(f: Fn | undefined, given?: SecretChanges) {
  const setAt = { ...f?.secretsSetAt }
  let changed = false
  for (const [name, v] of Object.entries(given ?? {})) {
    if (v === null && !(name in setAt)) continue
    changed = true
    if (v === null) delete setAt[name]
    else setAt[name] = now()
  }
  if (!changed) return undefined
  const names = Object.keys(setAt).sort()
  return { setAt, snap: { secretNames: names.length ? names : undefined } }
}
function checkSecrets(s: Snapshot) {
  for (const name of s.secretNames ?? []) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name)) throw new ApiError(400, 400, `secrets: invalid environment variable name "${name}"`)
    if (name === "PORT") throw new ApiError(400, 400, "secret PORT is reserved (it is set from port)")
    if (name in (s.env ?? {})) throw new ApiError(400, 400, `${name} is both an environment variable and a secret`)
  }
}
const nextSeq = (f: Fn) => Math.max(0, ...deployments.filter((d) => d.functionId === f.id).map((d) => d.seq)) + 1
function deploy(f: Fn, s: Snapshot, rollbackOf?: number, setAt?: Record<string, string>) {
  if (f.deploying) throw new ApiError(409, 409, "a deployment of this function is already in progress")
  f.pendingSetAt = setAt
  f.deploying = { id: `fnd-${hex(8)}`, functionId: f.id, seq: nextSeq(f), ...snapshot(s), ...(rollbackOf ? { rollbackOf } : {}), state: "DEPLOYING", createdAt: now() }
  f.reads = 0
  delete f.lastError
  deployments.unshift(f.deploying)
}
const running = (f: Fn) => (f.name.includes("busy") ? f.maxInstances : 1)
function checkScale(s: { maxInstances: number; concurrency: number }) {
  if (!Number.isInteger(s.maxInstances) || s.maxInstances < 1 || s.maxInstances > FUNCTION_LIMITS.maxInstances) throw new ApiError(400, 400, `maxInstances must be between 1 and ${FUNCTION_LIMITS.maxInstances}`)
  if (!Number.isInteger(s.concurrency) || s.concurrency < 1 || s.concurrency > FUNCTION_LIMITS.maxConcurrency) throw new ApiError(400, 400, `concurrency must be between 1 and ${FUNCTION_LIMITS.maxConcurrency}`)
}
function settle(f: Fn) {
  const d = f.deploying
  if (!d || ++f.reads < 2) return
  delete f.deploying
  d.finishedAt = now()
  const setAt = f.pendingSetAt
  delete f.pendingSetAt
  const wrong = f.wrongPassword
  delete f.wrongPassword
  if (d.image.includes("fail") || wrong || (d.image.includes("private") && !d.registryAuth)) {
    d.state = "FAILED"
    // One of a fixed set of reasons: what the registry answered could repeat the credentials.
    d.message = d.registryAuth
      ? "image pull failed: the registry refused the credentials or does not have the image: check registryAuth (server, username, password) and the image reference"
      : "image pull failed: check the image reference and that the registry allows anonymous pulls"
    f.lastError = d.message
    if (!f.currentDeployment) f.instances = 0
    return
  }
  for (const old of deployments) if (old.functionId === f.id && old.state === "ACTIVE") old.state = "SUPERSEDED"
  d.state = "ACTIVE"
  // What the first ready instance ran: a rollback to this deployment deploys exactly that.
  d.imageDigest = d.image.split("@")[1] ?? `sha256:${hex(64)}`
  Object.assign(f, snapshot(d), { currentDeployment: { id: d.id, seq: d.seq, image: d.image, deployedAt: d.finishedAt }, instances: f.suspended ? 0 : running(f), updatedAt: d.finishedAt })
  if (setAt) f.secretsSetAt = d.secretNames ? setAt : undefined
}

on("GET /project/:pid/functions", ({ params }) => {
  const mine = fns.filter((f) => f.projectId === params.pid)
  mine.forEach(settle)
  return { data: mine.map(view).sort((a, b) => a.name.localeCompare(b.name)) }
})
on("POST /project/:pid/functions", ({ params, opts }) => {
  enabled(params.pid)
  const b = opts.body as Body
  checkOffered(b)
  if (!b.name || !/^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/.test(b.name)) throw new ApiError(400, 400, "name must be 1-40 lowercase letters, digits or '-', starting with a letter and ending with a letter or digit")
  checkImage(b.image ?? "")
  if (fns.some((f) => f.projectId === params.pid && f.name === b.name)) throw new ApiError(409, 409, `a function named ${b.name} already exists in this project`)
  if (fns.filter((f) => f.projectId === params.pid).length >= FUNCTION_LIMITS.maxFunctions) throw new ApiError(429, 429, `a project may have at most ${FUNCTION_LIMITS.maxFunctions} functions`)
  const host = `${b.name}-${hex(6)}.${DOMAIN}`
  const f: Fn = {
    projectId: params.pid, reads: 0, id: `fn-${hex(16)}`, name: b.name, image: b.image!, port: b.port || 8080,
    cpuMillis: b.cpuMillis || 250, memoryMi: b.memoryMi || 256, env: b.env,
    // One instance unless asked otherwise: callers decide how many run under load, the owner pays.
    minInstances: b.minInstances ?? 0, idleSeconds: b.idleSeconds || FUNCTION_LIMITS.idleSeconds, maxInstances: b.maxInstances || 1, concurrency: b.concurrency || FUNCTION_LIMITS.defaultConcurrency,
    host, url: `https://${host}`, instances: 0, suspended: false, createdAt: now(), updatedAt: now(),
  }
  checkScale(f)
  const sec = mergeSecrets(undefined, b.secrets)
  checkSecrets({ ...f, ...sec?.snap })
  if (b.registryAuth) {
    f.registryAuth = describeRegistry(b.registryAuth, f.image)
    checkRegistry(f)
    f.wrongPassword = b.registryAuth.password!.includes("wrong")
  }
  fns.push(f)
  deploy(f, { ...f, ...sec?.snap }, undefined, sec?.setAt)
  return { data: view(f) }
})
on("GET /project/:pid/functions/:fn", ({ params }) => ({ data: view(owned(params.pid, params.fn)) }))
on("PATCH /project/:pid/functions/:fn", ({ params, opts }) => {
  const f = owned(params.pid, params.fn, true)
  const b = opts.body as Body
  checkOffered(b)
  if (b.name !== undefined && b.name !== f.name) throw new ApiError(400, 400, "a function cannot be renamed")
  if (b.image !== undefined) checkImage(b.image)
  const given = snapshotKeys.filter((k) => b[k] !== undefined)
  const scale = { maxInstances: b.maxInstances ?? f.maxInstances, concurrency: b.concurrency ?? f.concurrency }
  checkScale(scale)
  const sec = mergeSecrets(f, b.secrets)
  // registryAuth: an object sets or replaces (always a rollout), null removes, absent keeps.
  const reg = b.registryAuth !== undefined && !!(b.registryAuth || f.registryAuth)
  if (given.length || sec || reg) {
    const s = { ...snapshot(f), ...Object.fromEntries(given.map((k) => [k, b[k]])), ...sec?.snap } as Snapshot
    checkSecrets(s)
    if (reg) s.registryAuth = b.registryAuth ? describeRegistry(b.registryAuth, s.image) : undefined
    checkRegistry(s)
    if (f.deploying) throw new ApiError(409, 409, "a deployment of this function is already in progress")
    f.wrongPassword = !!b.registryAuth?.password?.includes("wrong")
    deploy(f, s, undefined, sec?.setAt)
  }
  f.minInstances = b.minInstances ?? f.minInstances
  f.idleSeconds = b.idleSeconds ?? f.idleSeconds
  f.maxInstances = scale.maxInstances
  f.concurrency = scale.concurrency
  if (f.instances > 0 && !f.deploying) f.instances = running(f) // applied without a deployment
  return { data: view(f) }
})
on("DELETE /project/:pid/functions/:fn", ({ params }) => {
  const f = owned(params.pid, params.fn)
  fns = fns.filter((x) => x !== f)
  return { data: undefined }
})
on("GET /project/:pid/functions/:fn/deployments", ({ params }) => {
  const f = owned(params.pid, params.fn)
  return { data: structuredClone(deployments.filter((d) => d.functionId === f.id).sort((a, b) => b.seq - a.seq)) }
})
on("POST /project/:pid/functions/:fn/rollback", ({ params, opts }) => {
  const f = owned(params.pid, params.fn, true)
  const b = (opts.body ?? {}) as { deploymentId?: string; registryAuth?: RegistryAuthReq }
  const d = deployments.find((x) => x.functionId === f.id && x.id === b.deploymentId)
  if (!d) throw new ApiError(404, 404, "deployment not found")
  if (d.state !== "SUPERSEDED") throw new ApiError(400, 400, "only a superseded deployment can be rolled back to")
  // Secrets and registry credentials are not versioned: a rollback runs with the current ones,
  // unless registryAuth comes with it (an object sets or replaces, null removes, as in an update).
  const clash = f.secretNames?.find((name) => name in (d.env ?? {}))
  if (clash) throw new ApiError(400, 400, `${clash} is an environment variable of that deployment and a secret now: remove the secret first`)
  // The bits that ran, not what the tag points at today: repository@digest.
  const image = d.imageDigest && !d.image.includes("@") ? `${d.image.replace(/:[^/:]*$/, "")}@${d.imageDigest}` : d.image
  const registryAuth = b.registryAuth === undefined ? f.registryAuth : b.registryAuth ? describeRegistry(b.registryAuth, image) : undefined
  const s = { ...d, image, secretNames: f.secretNames, registryAuth }
  checkRegistry(s)
  deploy(f, s, d.seq)
  f.wrongPassword = !!b.registryAuth?.password?.includes("wrong")
  return { data: view(f) }
})
on("POST /project/:pid/functions/:fn/action", ({ params, opts }) => {
  const f = owned(params.pid, params.fn, true)
  const action = (opts.body as { action?: string })?.action
  if (action !== "suspend" && action !== "resume") throw new ApiError(400, 400, "action must be suspend or resume")
  f.suspended = action === "suspend"
  if (f.suspended) f.instances = 0
  return { data: view(f) }
})
on("GET /project/:pid/functions/:fn/logs", ({ params }) => {
  const f = owned(params.pid, params.fn)
  return { data: { logs: f.instances > 0 ? `${now()} listening on :${f.port}\n${now()} GET / 200 1ms\n` : "" } }
})
