// Platform setup: GET /admin/setup/status and the setup script. The status is a short echo of the rules in
// internal/platform/admin/setupstatus.go, computed from the mock db so the page changes as the steps are saved. It is
// enough to show every state of the page; it is not a second copy of those rules (the live rows are placed only in
// connect, and the defaults the catalog step reads are the mock's own).
import { ApiError } from "@/lib/api"
import { on } from "../router"
import { db } from "../db"
import { mockPlatform, publishAllState } from "./platform"
import {
  detectedStorages, importedTemplates, proposedProfiles, seedFlavors, seedPricePlan, seedQuota, setupChecks, templateDiscovery, templatePlacement,
} from "../fixtures/setup"

type Doc = Record<string, any>

// State shared with the defaults and template mocks: the defaults items applied, and the template updates flag.
export const mockSetup = { applied: new Set<string>(), templateFlag: true }

const RANK: Record<string, number> = { done: 0, unknown: 1, attention: 2, todo: 3, blocked: 3 }
const ROLLUP = ["done", "unknown", "attention", "todo"]
const CHECK_STATE: Record<string, string> = { ok: "done", warn: "attention", fail: "todo" }

// PROXCLOUD_SELF_API_BASE_URL of the mock install.
const MOCK_API_BASE = "https://api.helium.example.com"

const item = (id: string, ok: boolean, done: string, todo: string) => ({ id, state: ok ? "done" : "todo", message: ok ? done : todo })
function step(id: string, done: string, items: Doc[]) {
  let worst = 0
  let summary = done
  for (const it of items) {
    if (RANK[it.state] > worst) {
      worst = RANK[it.state]
      summary = it.message
    }
  }
  return { id, state: ROLLUP[worst], summary, items }
}

export const isPve = (s: Doc) => s.config?.provider === "proxmox"

function catalogStep(p?: Doc) {
  const a = mockSetup.applied
  const items: Doc[] = [
    item("flavors", a.has("flavors"), "3 flavors in the catalog", "No flavor in the catalog: tenants cannot create a server"),
    item("pricePlan", a.has("pricePlan"), "An enabled public price plan with 1 server rules", "No enabled public price plan covers this provider: its usage is not billed"),
  ]
  if (p) {
    items.push(
      item("quota", a.has("quota"), "The provider's default project quota sets every limit", "The default project quota does not limit networks: new projects are unlimited there"),
      item("region", a.has("region"), "The provider's region is listed in the platform configuration", "The provider's region is not listed in the platform configuration"),
    )
  }
  const issuer = db.billingConfiguration
  items.push(issuer?.company?.businessName && issuer?.address?.country
    ? { id: "billingDetails", state: "done", message: `Invoices carry the details of ${issuer.company.businessName}` }
    : { id: "billingDetails", state: "attention", message: "The billing configuration lacks the business name, address or country that invoices carry" })
  return step("catalog", "Flavors, prices, quota and region are set", items)
}

function templatesStep(p: Doc, live: boolean) {
  const c = p.config ?? {}
  const images = live
    ? db.osImages.filter((g) => g.serviceId === p.id).reduce((n, g) => n + (g.images?.length ?? 0), 0)
    : Object.keys(c.templates ?? {}).length
  if (images > 0) return step("templates", `${images} OS images are published`, [{ id: "images", state: "done", message: `${images} OS images are published` }])
  if (!live) return step("templates", "", [{ id: "images", state: "unknown", message: "No template is pinned in the provider configuration; the templates on the cluster are counted with live=1" }])
  return step("templates", "", [
    { id: "images", state: "todo", message: "No OS image is published: tenants cannot create a server" },
    item("flag", mockSetup.templateFlag, "Template updates are on", "Template updates are off: set PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true and restart the API"),
    item("githubToken", db.integrations.some((i) => i.thirdParty === "GitHub"), "A GitHub token is saved", "No GitHub token is saved (System → Integrations → GitHub): image versions cannot be listed"),
    item("smokeNetwork", !!c.templateSmoke?.bridge, "The smoke-test network is set", "No smoke-test network (config.templateSmoke): imported templates cannot be tested"),
  ])
}

function goLiveStep(p: Doc, steps: Doc[]) {
  const open = steps.filter((s) => s.id !== "catalog" && (s.state === "todo" || s.state === "blocked")).map((s) => s.id)
  const unread = steps.filter((s) => s.id !== "catalog" && s.state === "unknown").map((s) => s.id)
  const on = p.config?.attachToNewProjects !== false
  let attach = { id: "attachPolicy", state: "todo", message: "New projects are not attached to the provider yet (attach policy off)" }
  if (on) attach = open.length
    ? { id: "attachPolicy", state: "attention", message: `New projects are attached to the provider although these steps are open: ${open.join(", ")}` }
    : { id: "attachPolicy", state: "done", message: "New projects are attached to the provider" }
  else if (open.length) return { id: "golive", state: "blocked", summary: `Finish these steps first: ${open.join(", ")}`, items: [attach] }
  else if (unread.length) return { id: "golive", state: "unknown", summary: `These steps could not be checked: ${unread.join(", ")}`, items: [attach] }
  const items: Doc[] = [attach]
  if (steps.find((s) => s.id === "catalog")?.state === "todo") {
    items.push({ id: "catalog", state: "attention", message: "Catalog and pricing is not complete: usage that matches no rule is billed as zero" })
  }
  // The mock API runs without the job scheduler, like a plain Compose install.
  items.push({ id: "scheduler", state: "attention", message: "The job scheduler is off (PROXCLOUD_JOBS_SCHEDULER_ENABLED): nothing is synced or charged on a timer, so run one sync after going live" })
  return step("golive", attach.message, items)
}

// managedStep in setupstatus.go: the installation's phase, after go live and never one of its inputs.
function managedStep(p: Doc) {
  const in_ = mockPlatform.installations.find((i) => i.serviceId === p.id)
  const it = (state: string, message: string) => step("managed", message, [{ id: "installation", state, message }])
  if (!mockPlatform.enabled) return it("todo", "Managed services are off on this install (optional): turn them on with ./proxcloud-up.sh install --managed-services")
  if (!in_) return it("todo", "No managed-services installation yet (optional): Kubernetes and databases for tenants are set up on the Managed services page")
  const name = `Managed services ${in_.name}`
  switch (in_.status.phase) {
    case "ready": return it("done", `${name} are installed`)
    case "applying": return it("attention", `${name} are being installed`)
    case "removing": return it("attention", `${name} are being removed`)
    case "failed": return it("attention", `The last run of managed services ${in_.name} failed: open the Managed services page`)
    case "removed": return it("todo", `${name} were removed`)
    default: return it("todo", `${name} are a draft: finish and install them on the Managed services page`)
  }
}

on("GET /admin/setup/status", ({ query }) => {
  const sid = query.get("serviceId") ?? ""
  const live = query.get("live") === "1"
  const p = sid ? db.services.find((s) => s.id === sid) : db.services.filter(isPve).sort((a, b) => String(a.name).localeCompare(String(b.name)))[0]
  if (sid && !p) throw new ApiError(404, 404, `Service not found: ${sid}`)
  if (!p) {
    const blocked = (id: string) => ({ id, state: "blocked", summary: "Needs a saved Proxmox provider", items: [] })
    return {
      data: {
        serviceId: "", live: false, seedUrl: MOCK_API_BASE,
        steps: [{ id: "connect", state: "todo", summary: "No Proxmox provider is saved", items: [] }, blocked("network"), blocked("storage"), blocked("templates"), catalogStep(), blocked("golive"), blocked("managed")],
      },
    }
  }
  const c = p.config ?? {}
  const n = c.network ?? {}
  const zone = n.zones?.[n.defaultZone]
  const profiles = Object.keys(c.storageProfiles ?? {}).sort()
  const detectedAt = c.capabilities?.detectedAt
  const checks = live ? setupChecks.map((ck) => ({ id: ck.id, state: CHECK_STATE[ck.status], message: ck.message })) : []
  const md = c.metadata ?? {}
  const seedUrl = String(md.url || MOCK_API_BASE).replace(/\/+$/, "")
  const seeds = md.forceIso === true
    ? { id: "seeds", state: "done", message: "Server seeds go on an ISO (config.metadata.forceIso)" }
    : md.forceIso === false || md.url
      ? { id: "seeds", state: "done", message: `Servers on a DHCP network fetch their seed from ${seedUrl}; the others get it on an ISO` }
      : { id: "seeds", state: "attention", message: `Servers on a DHCP network fetch their seed from ${seedUrl} unless ISO seeds are on (config.metadata.forceIso): tenant servers must reach it and trust its certificate, or they boot without their hostname, password and guest agent. Choose how servers get their seed` }
  const steps = [
    step("connect", `Provider ${p.name} is saved`, [{ id: "provider", state: "done", message: `Provider ${p.name} is saved` }, ...checks]),
    step("network", `Project networks use zone ${n.defaultZone} and ${n.cidrPool}`, [
      item("cidrPool", !!n.cidrPool, `Project networks are cut from ${n.cidrPool}`, "No address pool for project networks (config.network.cidrPool): creating a network fails"),
      item("zone", !!zone, `Default zone ${n.defaultZone} (${zone?.kind})`, "No default zone (config.network.defaultZone and zones): creating a network fails"),
      seeds,
    ]),
    step("storage", `Storage profiles are saved, default ${c.defaultStorageProfile}`, [
      item("detected", !!detectedAt, `Storage was detected on ${String(detectedAt).slice(0, 10)}`, "Storage was never detected (POST /admin/service/{id}/storage/detect)"),
      item("profiles", profiles.length > 0, `Storage profiles: ${profiles.join(", ")}`, "No storage profile is saved: servers and volumes have nowhere to go"),
      item("defaultProfile", profiles.includes(c.defaultStorageProfile), `Default storage profile ${c.defaultStorageProfile}`, "No default storage profile (config.defaultStorageProfile)"),
    ]),
    templatesStep(p, live),
    catalogStep(p),
  ]
  return { data: { serviceId: p.id, live, seedUrl, steps: [...steps, goLiveStep(p, steps), managedStep(p)] } }
})

// The script the API generates (pve.PlatformSetupScript), shortened: the same inputs and the same refusal.
on("GET /admin/setup/proxmox-script", ({ query }) => {
  const tokenId = query.get("tokenId") || "proxcloud@pve!platform"
  const store = query.get("importStorage") ?? ""
  const m = /^([A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z][A-Za-z0-9._-]*)!([A-Za-z][A-Za-z0-9._-]*)$/.exec(tokenId)
  const bad = !m ? "tokenId must be user@realm!name, of letters, digits, dot, dash and underscore"
    : store && !/^[A-Za-z][A-Za-z0-9._-]*$/.test(store) ? "importStorage must be a storage id: a letter, then letters, digits, dot, dash and underscore" : ""
  // installation=<id>: the root command of that installation (pve.EnrollScript), for its own stores.
  const enroll = mockPlatform.installations.find((i) => i.id === query.get("installation"))
  if (query.get("installation") && !enroll) return { data: new Response(JSON.stringify({ errors: { code: 404, message: "installation not found" } }), { status: 404 }) }
  const held = mockPlatform.secrets[enroll?.id ?? ""] ?? {}
  const managed = !!enroll || ["true", "1"].includes(query.get("managedServices") ?? "")
  const boot = enroll?.spec.storage.boot ?? query.get("bootStorage") ?? "", iso = enroll?.spec.storage.iso ?? (query.get("isoStorage") || "local")
  const badManaged = managed && !/^[A-Za-z][A-Za-z0-9._-]*$/.test(boot) ? "bootStorage must be a storage id: a letter, then letters, digits, dot, dash and underscore" : ""
  if (bad || badManaged) return { data: new Response(JSON.stringify({ errors: { code: 400, message: bad || badManaged } }), { status: 400 }) }
  const grants = ["'/' 'ProxCloudPlatform' 0", "'/nodes' 'PVEAuditor' 1", "'/pool' 'ProxCloudPlatform' 1", "'/pool/proxcloud-system' 'NoAccess' 1",
    "'/sdn' 'ProxCloudPlatform' 1", "'/storage' 'ProxCloudPlatform' 1"]
  if (query.get("gpu") === "true") grants.push("'/mapping/pci' 'ProxCloudPlatform' 1")
  if (store) grants.push(`'/storage/${store}' 'PVEDatastoreAdmin' 0`)
  const script = [
    "# ProxCloud: the Proxmox VE identity of the portal token.",
    "# Paste into a root shell on one node (the cluster shares it). Safe to run again.",
    "(", "set -eu", `PC_USER='${m![1]}'`, `PC_TOKEN_NAME='${m![2]}'`, `PC_TOKEN='${tokenId}'`, "",
    "pveum role add 'ProxCloudPlatform' 2>/dev/null || true",
    "pveum role modify 'ProxCloudPlatform' --append 1 --privs 'Sys.Audit Pool.Allocate VM.Allocate (mock: shortened)'",
    "pveum pool add 'pc-templates' --comment 'ProxCloud templates' 2>/dev/null || true",
    "pveum pool add 'proxcloud-system' --comment 'ProxCloud management (not tenant)' 2>/dev/null || true",
    `pveum user add "$PC_USER" --comment 'ProxCloud portal' 2>/dev/null || true`,
    `pveum user token add "$PC_USER" "$PC_TOKEN_NAME" --privsep 1`,
    `grant() { pveum acl modify "$1" --users "$PC_USER" --roles "$2" --propagate "$3"; pveum acl modify "$1" --tokens "$PC_TOKEN" --roles "$2" --propagate "$3"; }`,
    ...grants.map((g) => `grant ${g}`), "",
    `pveum user token permissions "$PC_USER" "$PC_TOKEN_NAME"`, "pvenode cert info",
    // pve.ManagedSetupScript, shortened: the two identities of managed services and their pool.
    ...(managed ? [
      "", "# Managed services: the machines' and the volumes' identities.",
      "pveum role add 'ProxCloudCAPI' 2>/dev/null || true", "pveum role add 'ProxCloudCSI' 2>/dev/null || true", "pveum role add 'ProxCloudKubeMint' 2>/dev/null || true",
      "pveum pool add 'proxcloud-seed' --comment 'ProxCloud seed' 2>/dev/null || true",
      "pveum user add 'capmox@pve' 2>/dev/null || true", "pveum user add 'csi@pve' 2>/dev/null || true",
      ...(enroll ? [
        `PC_INSTALL='${enroll.id}'`, `PC_MARK='proxcloud:${enroll.id}'`,
        ...(held.capi ? [] : [`[ "$PC_CAPI" != foreign ] || { echo 'STOP: the token capmox@pve!capi exists on this cluster and was not made for this ProxCloud installation (${enroll.id}).'; exit 1; }`]),
        ...(held.csi ? [] : [`[ "$PC_CSI" != foreign ] || { echo 'STOP: the token csi@pve!csi exists on this cluster and was not made for this ProxCloud installation (${enroll.id}).'; exit 1; }`]),
        `PC_CAPI_SECRET=$(pc_token 'capmox@pve' 'capi' "$PC_CAPI")`, `PC_CSI_SECRET=$(pc_token 'csi@pve' 'csi' "$PC_CSI")`,
        `printf 'PC-ENROLL:%s\\n' "$(printf '1:%s:%s:%s:%s' "$PC_INSTALL" "$(date +%s)" "$PC_CAPI_SECRET" "$PC_CSI_SECRET" | base64 -w0)"`,
      ] : ["pveum user token add 'capmox@pve' 'capi' --privsep 1", "pveum user token add 'csi@pve' 'csi' --privsep 1"]),
      `pveum acl modify '/storage/${boot}' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI'`,
      `pveum acl modify '/storage/${iso}' --tokens 'capmox@pve!capi' --roles 'PVEDatastoreAdmin'`,
      "pveum user token permissions 'capmox@pve' 'capi'", "pveum user token permissions 'csi@pve' 'csi'",
    ] : []),
    ")", "",
  ].join("\n")
  return { data: new Response(script, { headers: { "Content-Type": "text/plain" } }) }
})

// Storage detection stores config.capabilities and answers it with the proposal (nothing of the proposal is stored).
on("POST /admin/service/:id/storage/detect", ({ params }) => {
  const svc = db.services.find((s) => s.id === params.id)
  if (!svc) throw new ApiError(404, 404, `Service not found: ${params.id}`)
  const capabilities = { detectedAt: new Date().toISOString(), storages: structuredClone(detectedStorages) }
  ;(svc.config ??= {}).capabilities = capabilities
  return { data: { ...capabilities, proposedProfiles: structuredClone(proposedProfiles) } }
})

// ─── Template updates (the subset the setup page and the Template updates page read) ───

const templates = structuredClone(importedTemplates)
const github = () => db.integrations.find((i) => i.thirdParty === "GitHub")

on("GET /admin/template-updates/status", () => ({
  data: {
    configured: !!github(), integrationId: github()?.id, login: github() ? "proxcloud-ops" : undefined, scopes: github() ? ["read:packages"] : undefined,
    vmUpdatesEnabled: mockSetup.templateFlag, sandboxMirrorEnabled: false,
  },
}))
on("GET /admin/template-updates", () => ({ data: templateDiscovery }))
on("GET /admin/template-updates/history", () => ({ data: { items: [], jobs: {} } }))
on("GET /admin/template-updates/placement/:serviceId", () => ({ data: templatePlacement }))
on("GET /admin/template-updates/templates", () => ({ data: { items: templates, smokes: [], vmUpdatesEnabled: mockSetup.templateFlag } }))

on("POST /admin/template-updates/vm", ({ opts }) => {
  const b = (opts.body ?? {}) as Doc
  if (!mockSetup.templateFlag) throw new ApiError(409, 409, "vm_updates_disabled")
  if (b.confirmDigest !== b.digest) throw new ApiError(400, 400, "confirmDigest must repeat digest")
  const line = templateDiscovery.vm.find((l: Doc) => l.package === b.package)
  const slug = String(b.package).replace("proxcloud-vm-", "")
  templates.push({
    id: db.nextId("tpl"), package: b.package, tag: line?.latest?.tag ?? "", digest: b.digest, os: slug, variant: "os", buildVersion: "", slug,
    serviceId: b.serviceId, node: b.node, state: "importing", importedAt: new Date().toISOString(), requestedBy: "admin@example.com",
    publishedOnLine: 0, actions: { smoke: false, publish: false, withdraw: false, lastPublished: false },
  })
  return { data: { id: db.nextId("rcpt"), payload: b, step: "queued" } }
})

on("POST /admin/template-updates/templates/:id/publish", ({ params, opts }) => {
  const t = templates.find((x: Doc) => x.id === params.id)
  if (!t) throw new ApiError(404, 404, "Template not found")
  if ((opts.body as Doc | undefined)?.confirmDigest !== t.digest) throw new ApiError(400, 400, "confirmDigest must repeat the template's digest")
  if (!t.actions.publish) throw new ApiError(409, 409, "Publish needs a passing smoke test of this template")
  t.state = "published_default"
  t.actions = { ...t.actions, publish: false, withdraw: true }
  return { data: { complete: true, providers: [] } }
})

on("DELETE /admin/template-updates/templates/:id", ({ params }) => {
  const i = templates.findIndex((x: Doc) => x.id === params.id)
  if (i < 0) throw new ApiError(404, 404, "No template import with that id")
  if (!templates[i].actions.delete) throw new ApiError(409, 409, "Only a withdrawn template can be deleted: withdraw it first")
  templates.splice(i, 1)
  return { data: { action: "delete", changed: [], complete: true } }
})

// Publish on all nodes: each POST moves the first unpublished node one step (import, smoke test, publish), one job at
// a time like the server; the GET reads the same state.
const publishAllSteps = ["missing", "importing", "smoke_running", "ready_to_publish", "published"]
function publishAllReport(serviceId: string, digest: string, act: boolean) {
  const st = (publishAllState[`${serviceId}/${digest}`] ??= Object.fromEntries(templatePlacement.nodes.map((n: Doc) => [n.node, 0])))
  const first = Object.keys(st).find((n) => st[n] < 4)
  if (act && first) st[first]++
  const nodes = Object.keys(st).map((node, i) => ({ node, state: publishAllSteps[st[node]], vmid: st[node] > 1 ? 9300 + i : undefined }))
  const line = templateDiscovery.vm.find((l: Doc) => l.latest?.digest === digest)
  const pub = nodes.filter((n) => n.state === "published")
  return {
    digest, kubernetesVersion: line?.kubernetesVersion, nodes, nodesMissing: [], complete: pub.length === nodes.length,
    versionsLine: line?.kubernetesVersion && pub.length ? `"${line.kubernetesVersion}": "${pub.map((n) => `${n.node}=${n.vmid}`).join(",")}"` : undefined,
    next: pub.length === nodes.length ? "done" : "advance",
  }
}
on("GET /admin/template-updates/publish-all", ({ query }) => ({ data: publishAllReport(query.get("serviceId") ?? "", query.get("digest") ?? "", false) }))
on("POST /admin/template-updates/publish-all", ({ opts }) => {
  const b = (opts.body ?? {}) as Doc
  if (!mockSetup.templateFlag) throw new ApiError(409, 409, "vm_updates_disabled")
  if (b.confirmDigest !== b.digest) throw new ApiError(400, 400, "Confirm the exact digest to publish")
  return { data: publishAllReport(b.serviceId, b.digest, true) }
})

// The no-provision save replaces the top-level config keys the body carries, like PUT /admin/service/{id}.
on("PUT /admin/service/:id/update", ({ params, opts }) => {
  const svc = db.services.find((s) => s.id === params.id)
  if (!svc) throw new ApiError(404, 404, `Service not found: ${params.id}`)
  Object.assign((svc.config ??= {}), ((opts.body ?? {}) as Doc).config ?? {})
  return { data: svc }
})

// ─── Install defaults: the preview and the create-only apply (POST refuses pricePlan without confirmRates) ───

function defaultsItems(serviceId: string) {
  const a = mockSetup.applied
  const p = db.services.find((s) => s.id === serviceId && isPve(s))
  const currency = db.billingConfiguration?.baseCurrency ?? "USD"
  const item = (id: string, todo: string, done: string, preview: unknown, needsProvider = false) =>
    needsProvider && !p ? { id, state: "blocked", message: "Needs a Proxmox provider (serviceId)" }
      : a.has(id) ? { id, state: "done", message: done } : { id, state: "todo", message: todo, preview }
  return [
    item("flavors", "3 of the 3 default flavors are missing", "The 3 default flavors exist", seedFlavors),
    item("pricePlan", `No enabled public price plan: "Public rate card" with ${seedPricePlan.rules.length} rules would be added, enabled, in ${currency}`,
      'The enabled public price plan "Public rate card" exists; the default plan is not added, because every enabled public plan is billed', { ...seedPricePlan, currency }),
    item("quota", "The default project quota does not limit networks; limits already set are kept", "The default project quota sets every limit",
      { ...seedQuota, ...(p?.config?.provisioning?.quota ?? {}) }, true),
    item("region", `Region ${p?.config?.region ?? "pve"} is not listed in the platform configuration`, "The provider has a region row in the platform configuration",
      { serviceId, region: p?.config?.region ?? "pve", order: 0 }, true),
  ]
}

on("GET /admin/setup/defaults", ({ query }) => ({
  data: { currency: db.billingConfiguration?.baseCurrency ?? "USD", items: defaultsItems(query.get("serviceId") ?? "") },
}))

on("POST /admin/setup/defaults", ({ opts }) => {
  const b = (opts.body ?? {}) as Doc
  const want: string[] = b.items ?? []
  if (!want.length) throw new ApiError(400, 400, "items is required: any of flavors, pricePlan, quota, region")
  if (want.includes("pricePlan") && b.confirmRates !== true) {
    throw new ApiError(400, 400, "confirmRates must be true to apply pricePlan: the default plan is created enabled and its rates are billed. Review them in GET /admin/setup/defaults first")
  }
  const items = defaultsItems(b.serviceId ?? "").filter((i) => want.includes(i.id)).map((i) => {
    if (i.state === "todo") mockSetup.applied.add(i.id)
    return { id: i.id, state: i.state === "todo" ? "created" : i.state === "done" ? "skipped" : "blocked", message: i.message }
  })
  return { data: { items } }
})

// The operator job trigger answers 202 with no body.
on("POST /admin/job/external-service/sync", () => {
  mockPlatform.synced = true
  return { data: undefined }
})
