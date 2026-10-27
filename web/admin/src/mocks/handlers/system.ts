// System-area endpoints: cloud providers, catalog, configuration, pricing,
// taxes, savings plans, promotions, integrations, roles, keys, templates, menu.
import { ApiError } from "@/lib/api"
import { on } from "../router"
import { db } from "../db"

type Doc = Record<string, any>

const notFound = (what: string): never => {
  throw new ApiError(404, 404, `${what} not found.`)
}

// Deep-merge plain objects (PUT /admin/service/{id} merges config).
function deepMerge(target: Doc, patch: Doc): Doc {
  for (const [k, v] of Object.entries(patch)) {
    if (v && typeof v === "object" && !Array.isArray(v) && target[k] && typeof target[k] === "object" && !Array.isArray(target[k])) {
      deepMerge(target[k], v as Doc)
    } else {
      target[k] = v
    }
  }
  return target
}

// ─── Cloud providers (external services) ──────────────────────────────────────

on("GET /admin/service", () => ({ data: db.services }))

on("POST /admin/service", ({ opts }) => {
  const body = (opts.body ?? {}) as Doc
  const svc = {
    id: db.nextId("svc"),
    name: body.name ?? "New provider",
    type: body.config?.provider === "ceph-s3" || body.type === "ceph-s3" ? "ceph-s3" : "openstack",
    status: "ACTIVE",
    config: body.config ?? body,
  }
  db.services.push(svc)
  db.gpuInfoByService[svc.id] = []
  db.unpricedFlavorsByService[svc.id] = []
  return { data: svc }
})

// Live glance images across providers (register before /admin/service/:id).
on("GET /admin/service/os-images", () => ({ data: db.osImages }))

// Connection test with the stored credentials.
on("POST /admin/service/openstack/auth", () => ({ data: db.openstackAuthResult }))

on("GET /admin/service/:id", ({ params }) => ({
  data: db.services.find((s) => s.id === params.id) ?? notFound("Cloud provider"),
}))

on("PUT /admin/service/:id", ({ params, opts }) => {
  const svc = db.services.find((s) => s.id === params.id) ?? notFound("Cloud provider")
  const body = (opts.body ?? {}) as Doc
  if (body.name != null) svc.name = body.name
  if (body.config) deepMerge((svc.config ??= {}), body.config as Doc)
  return { data: svc }
})

on("DELETE /admin/service/:id", ({ params }) => {
  db.services = db.services.filter((s) => s.id !== params.id)
  return { data: { ok: true } }
})

on("POST /admin/service/:id/discover", ({ params }) => ({
  data: db.services.find((s) => s.id === params.id) ?? notFound("Cloud provider"),
}))

on("GET /admin/service/:id/gpu-info", ({ params }) => ({ data: db.gpuInfoByService[params.id] ?? [] }))

// Per-service live catalog (the kamaji-provider form's flavor/image pickers).
on("GET /admin/service/:id/os-images", () => ({ data: db.osImages }))
on("GET /admin/service/:id/os-flavors", () => ({ data: db.liveFlavors }))

on("GET /admin/service/:id/unpriced-flavors", ({ params }) => ({
  data: db.unpricedFlavorsByService[params.id] ?? [],
}))

on("PUT /admin/service/:id/metrics-config", ({ params, opts }) => {
  const svc = db.services.find((s) => s.id === params.id) ?? notFound("Cloud provider")
  const body = (opts.body ?? {}) as Doc
  const metrics = ((svc.config ??= {}).metrics ??= {}) as Doc
  metrics.source = body.source ?? metrics.source
  if (body.prometheus) metrics.prometheus = body.prometheus
  return { data: svc }
})

on("POST /admin/service/:id/metrics-test", () => ({
  data: { ok: true, trafficSeries: 42, monthStartSeries: 39, warnings: [] },
}))

on("GET /admin/service/:id/volume/types", ({ params }) => ({
  data: db.volumeTypesByService[params.id] ?? [],
}))

on("PUT /admin/service/:id/volume/types", ({ params, opts }) => {
  const svc = db.services.find((s) => s.id === params.id) ?? notFound("Cloud provider")
  const features = ((svc.config ??= {}).features ??= {}) as Doc
  features.volumeTypes = opts.body ?? {}
  return { data: svc }
})

on("GET /admin/service/:id/availability-zones", ({ params }) => ({
  data: db.availabilityZonesByService[params.id] ?? {},
}))

on("PUT /admin/service/:id/availability-zones", ({ params, opts }) => {
  const svc = db.services.find((s) => s.id === params.id) ?? notFound("Cloud provider")
  ;(svc.config ??= {}).availabilityZones = opts.body ?? []
  return { data: svc }
})

on("GET /admin/service/:id/share/protocols", ({ params }) => ({
  data: db.shareProtocolsByService[params.id] ?? [],
}))

on("PUT /admin/service/:id/share/protocols", ({ params, opts }) => {
  db.shareProtocolsByService[params.id] = (opts.body ?? []) as Doc[]
  return { data: { ok: true } }
})

// Project imports (Keystone projects vs ProxCloud projects).
on("GET /admin/project-import/:serviceId", ({ params }) => ({
  data: db.importProjectsByService[params.serviceId] ?? [],
}))

on("POST /admin/project-import/bulk-import/:serviceId", ({ params, opts }) => {
  const rows = db.importProjectsByService[params.serviceId] ?? []
  for (const entry of (opts.body ?? []) as Doc[]) {
    const row = rows.find((r) => r.project?.id === entry.project?.id)
    if (!row || row.proxcloudProjectId) continue
    const project = {
      id: db.nextId("prj"),
      name: entry.project.name,
      status: "ENABLED",
      services: [{ serviceId: params.serviceId, externalProjectId: entry.project.id }],
      memberships: [],
      createdAt: new Date().toISOString(),
    }
    db.projects.push(project)
    row.proxcloudProjectId = project.id
  }
  return { data: { ok: true } }
})

// ─── Catalog: flavor categories ───────────────────────────────────────────────

on("GET /admin/flavor-categories", () => ({ data: db.flavorCategories }))
on("GET /admin/flavor-categories/flavors", () => ({ data: db.liveFlavors }))

on("POST /admin/flavor-categories", ({ opts }) => {
  const item = { id: db.nextId("fc"), ...((opts.body ?? {}) as Doc) }
  db.flavorCategories.push(item)
  return { data: item }
})

on("PUT /admin/flavor-categories/:id", ({ params, opts }) => {
  const item = db.flavorCategories.find((c) => c.id === params.id) ?? notFound("Flavor category")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/flavor-categories/:id", ({ params }) => {
  db.flavorCategories = db.flavorCategories.filter((c) => c.id !== params.id)
  return { data: { ok: true } }
})

// ─── Catalog: image categories + groups ───────────────────────────────────────

on("GET /admin/images/categories", () => ({ data: db.imageCategories }))

on("POST /admin/images/categories", ({ opts }) => {
  const item = { id: db.nextId("ic"), ...((opts.body ?? {}) as Doc) }
  db.imageCategories.push(item)
  return { data: item }
})

on("PUT /admin/images/categories/:id", ({ params, opts }) => {
  const item = db.imageCategories.find((c) => c.id === params.id) ?? notFound("Image category")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/images/categories/:id", ({ params }) => {
  db.imageCategories = db.imageCategories.filter((c) => c.id !== params.id)
  db.imageGroups = db.imageGroups.filter((g) => g.categoryId !== params.id)
  return { data: { ok: true } }
})

on("GET /admin/images/categories/:id/groups", ({ params }) => ({
  data: db.imageGroups.filter((g) => g.categoryId === params.id),
}))

on("POST /admin/images/groups", ({ opts }) => {
  const item = { id: db.nextId("ig"), ...((opts.body ?? {}) as Doc) }
  db.imageGroups.push(item)
  return { data: item }
})

on("PUT /admin/images/groups/:id", ({ params, opts }) => {
  const item = db.imageGroups.find((g) => g.id === params.id) ?? notFound("Image group")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/images/groups/:id", ({ params }) => {
  db.imageGroups = db.imageGroups.filter((g) => g.id !== params.id)
  return { data: { ok: true } }
})

// ─── Catalog: instance metadata options ───────────────────────────────────────

on("GET /admin/instance-metadata-options", () => ({ data: db.metadataOptions }))

on("POST /admin/instance-metadata-options", ({ opts }) => {
  const item = { id: db.nextId("meta"), enabled: true, ...((opts.body ?? {}) as Doc) }
  db.metadataOptions.push(item)
  return { data: item }
})

on("PUT /admin/instance-metadata-options/:id", ({ params, opts }) => {
  const item = db.metadataOptions.find((m) => m.id === params.id) ?? notFound("Metadata option")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("POST /admin/instance-metadata-options/:id/reactivate", ({ params }) => {
  const item = db.metadataOptions.find((m) => m.id === params.id) ?? notFound("Metadata option")
  item.enabled = true
  return { data: item }
})

// Soft delete disables; ?permanent=true removes.
on("DELETE /admin/instance-metadata-options/:id", ({ params, query }) => {
  if (query.get("permanent") === "true") {
    db.metadataOptions = db.metadataOptions.filter((m) => m.id !== params.id)
  } else {
    const item = db.metadataOptions.find((m) => m.id === params.id) ?? notFound("Metadata option")
    item.enabled = false
  }
  return { data: { ok: true } }
})

// ─── Custom menu ──────────────────────────────────────────────────────────────

on("GET /admin/menu", () => ({ data: [...db.menuItems].sort((a, b) => (a.order ?? 0) - (b.order ?? 0)) }))
on("GET /admin/menu/placeholders", () => ({ data: db.menuPlaceholders }))

on("POST /admin/menu", ({ opts }) => {
  const item = { id: db.nextId("menu"), ...((opts.body ?? {}) as Doc) }
  db.menuItems.push(item)
  return { data: item }
})

on("PUT /admin/menu/reorder", ({ opts }) => {
  const ids = (opts.body ?? []) as string[]
  ids.forEach((id, i) => {
    const item = db.menuItems.find((m) => m.id === id)
    if (item) item.order = i + 1
  })
  return { data: { ok: true } }
})

on("PUT /admin/menu/:id", ({ params, opts }) => {
  const item = db.menuItems.find((m) => m.id === params.id) ?? notFound("Menu item")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/menu/:id", ({ params }) => {
  db.menuItems = db.menuItems.filter((m) => m.id !== params.id)
  return { data: { ok: true } }
})

// ─── Templates ────────────────────────────────────────────────────────────────

on("GET /admin/message-templates", () => ({ data: db.messageTemplates }))
on("GET /admin/message-templates/placeholders", () => ({ data: db.messagePlaceholders }))

on("POST /admin/message-templates", ({ opts }) => {
  const item = { id: db.nextId("mt"), systemTemplate: false, ...((opts.body ?? {}) as Doc) }
  db.messageTemplates.push(item)
  return { data: item }
})

on("PUT /admin/message-templates/:id", ({ params, opts }) => {
  const item = db.messageTemplates.find((t) => t.id === params.id) ?? notFound("Message template")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/message-templates/:id", ({ params }) => {
  db.messageTemplates = db.messageTemplates.filter((t) => t.id !== params.id)
  return { data: { ok: true } }
})

on("GET /admin/pdf-templates", () => ({ data: db.pdfTemplates }))

on("PUT /admin/pdf-templates/:id", ({ params, opts }) => {
  const item = db.pdfTemplates.find((t) => t.id === params.id) ?? notFound("PDF template")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

// Renders the posted template body with sample values.
on("POST /admin/pdf-templates/:id/preview", ({ opts }) => {
  const raw = typeof opts.rawBody === "string" ? opts.rawBody : ""
  const html = raw
    .replaceAll("{{.InvoiceNumber}}", "INV-2026-07-0001")
    .replaceAll("{{.ReceiptNumber}}", "RCP-2026-07-0001")
    .replaceAll("{{.CustomerName}}", "Alice Tran")
    .replaceAll("{{.Amount}}", "342.18")
    .replaceAll("{{.Total}}", "342.18")
    .replaceAll("{{.Currency}}", "USD")
    .replaceAll("{{.Date}}", "11 Jul 2026")
  return { data: html || "<html><body><p>Empty template.</p></body></html>" }
})

on("POST /admin/pdf-templates/:id/revert-to-default", ({ params }) => {
  const item = db.pdfTemplates.find((t) => t.id === params.id) ?? notFound("PDF template")
  item.content = `<html><body><h1>${item.name}</h1><p>Default template restored.</p></body></html>`
  return { data: item }
})

// ─── Integrations ─────────────────────────────────────────────────────────────

on("GET /admin/integrations", () => ({ data: db.integrations }))
on("GET /admin/integrations/stats", () => ({ data: db.integrationStats }))

on("POST /admin/integrations", ({ opts }) => {
  const body = (opts.body ?? {}) as Doc
  const item = { id: db.nextId("int"), createdAt: new Date().toISOString(), ...body }
  db.integrations.push(item)
  const stat = db.integrationStats.find((s) => s.name === body.thirdParty)
  if (stat) stat.installed = true
  return { data: item }
})

on("POST /admin/integrations/healthcheck/:id", () => ({ data: { ok: true } }))

on("PUT /admin/integrations/:id", ({ params, opts }) => {
  const item = db.integrations.find((i) => i.id === params.id) ?? notFound("Integration")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/integrations/:id", ({ params }) => {
  const removed = db.integrations.find((i) => i.id === params.id)
  db.integrations = db.integrations.filter((i) => i.id !== params.id)
  if (removed && !db.integrations.some((i) => i.thirdParty === removed.thirdParty)) {
    for (const stat of db.integrationStats) if (stat.name === removed.thirdParty) stat.installed = false
  }
  return { data: { ok: true } }
})

// ─── Platform + billing configuration ─────────────────────────────────────────

on("GET /admin/platform-configuration/current", () => ({ data: db.platformConfiguration }))

on("PUT /admin/platform-configuration/:id", ({ opts }) => {
  Object.assign(db.platformConfiguration, (opts.body ?? {}) as Doc)
  return { data: db.platformConfiguration }
})

on("PUT /admin/platform-configuration/:id/regions", ({ opts }) => {
  db.platformConfiguration.regions = (opts.body ?? []) as Doc[]
  return { data: db.platformConfiguration }
})

on("GET /admin/billing/configuration/current", () => ({ data: db.billingConfiguration }))
on("GET /admin/billing/configuration/currencies", () => ({ data: db.currencies }))
on("GET /admin/billing/configuration/countries", () => ({ data: db.countries }))

on("POST /admin/billing/configuration", ({ opts }) => {
  Object.assign(db.billingConfiguration, (opts.body ?? {}) as Doc)
  db.billingConfiguration.id ??= db.nextId("billcfg")
  return { data: db.billingConfiguration }
})

on("PUT /admin/billing/configuration/:id", ({ opts }) => {
  Object.assign(db.billingConfiguration, (opts.body ?? {}) as Doc)
  return { data: db.billingConfiguration }
})

// ─── Price plans ──────────────────────────────────────────────────────────────

on("GET /admin/price-plan", () => ({ data: db.pricePlans }))
on("GET /admin/price-plan/resource-types", () => ({ data: db.resourceTypes }))

on("POST /admin/price-plan", ({ opts }) => {
  const item = { id: db.nextId("pp"), createdAt: new Date().toISOString(), ...((opts.body ?? {}) as Doc) }
  db.pricePlans.push(item)
  return { data: item }
})

on("POST /admin/price-plan/clone", ({ opts }) => {
  const body = (opts.body ?? {}) as Doc
  const ids: string[] = body.pricePlanIds ?? (body.sourcePricePlanId ? [body.sourcePricePlanId] : [])
  const cloned = ids.flatMap((sourceId) => {
    const src = db.pricePlans.find((p) => p.id === sourceId)
    if (!src) return []
    const copy = { ...structuredClone(src), id: db.nextId("pp"), name: `${src.name} (copy)`, createdAt: new Date().toISOString() }
    db.pricePlans.push(copy)
    const rules = db.pricePlanRules.filter((r) => r.pricePlanId === sourceId)
    for (const r of rules) db.pricePlanRules.push({ ...structuredClone(r), id: db.nextId("rule"), pricePlanId: copy.id })
    return [{ sourcePricePlanId: sourceId, newPricePlanId: copy.id, newPricePlanName: copy.name, rulesCloned: rules.length }]
  })
  return { data: { clonedPricePlans: cloned } }
})

on("POST /admin/price-plan/rule", ({ opts }) => {
  const item = { id: db.nextId("rule"), ...((opts.body ?? {}) as Doc) }
  db.pricePlanRules.push(item)
  return { data: item }
})

on("PUT /admin/price-plan/rule/:ruleId", ({ params, opts }) => {
  const item = db.pricePlanRules.find((r) => r.id === params.ruleId) ?? notFound("Price rule")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/price-plan/rule/:ruleId", ({ params }) => {
  db.pricePlanRules = db.pricePlanRules.filter((r) => r.id !== params.ruleId)
  return { data: { ok: true } }
})

on("GET /admin/price-plan/rule/:ruleId/usage", ({ params }) => ({
  data: db.ruleUsage[params.ruleId] ?? { openBillsCount: 0, totalAppliedAmount: 0 },
}))

on("GET /admin/price-plan/:id", ({ params }) => ({
  data: db.pricePlans.find((p) => p.id === params.id) ?? notFound("Price plan"),
}))

on("PUT /admin/price-plan/:id", ({ params, opts }) => {
  const item = db.pricePlans.find((p) => p.id === params.id) ?? notFound("Price plan")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/price-plan/:id", ({ params }) => {
  db.pricePlans = db.pricePlans.filter((p) => p.id !== params.id)
  db.pricePlanRules = db.pricePlanRules.filter((r) => r.pricePlanId !== params.id)
  return { data: { ok: true } }
})

on("GET /admin/price-plan/:id/rule", ({ params }) => ({
  data: db.pricePlanRules.filter((r) => r.pricePlanId === params.id),
}))

// Price adjustment rules.
on("GET /admin/price-adjustment-rules/price-plan/:planId", ({ params }) => ({
  data: db.adjustmentRules.filter((r) => r.pricePlanId === params.planId),
}))

on("POST /admin/price-adjustment-rules", ({ opts }) => {
  const item = { id: db.nextId("adj"), enabled: true, ...((opts.body ?? {}) as Doc) }
  db.adjustmentRules.push(item)
  return { data: item }
})

on("GET /admin/price-adjustment-rules/:id/usage", ({ params }) => ({
  data: db.adjRuleUsage[params.id] ?? { openBillsCount: 0, totalAdjustmentsAmount: 0 },
}))

on("PUT /admin/price-adjustment-rules/:id", ({ params, opts }) => {
  const item = db.adjustmentRules.find((r) => r.id === params.id) ?? notFound("Adjustment rule")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/price-adjustment-rules/:id", ({ params }) => {
  db.adjustmentRules = db.adjustmentRules.filter((r) => r.id !== params.id)
  return { data: { ok: true } }
})

// ─── Taxes ────────────────────────────────────────────────────────────────────

on("GET /admin/tax", () => ({ data: db.taxes }))

on("POST /admin/tax", ({ opts }) => {
  const item = { id: db.nextId("tax"), ...((opts.body ?? {}) as Doc) }
  db.taxes.push(item)
  return { data: item }
})

on("PUT /admin/tax/:id", ({ params, opts }) => {
  const item = db.taxes.find((t) => t.id === params.id) ?? notFound("Tax rate")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/tax/:id", ({ params }) => {
  db.taxes = db.taxes.filter((t) => t.id !== params.id)
  return { data: { ok: true } }
})

// ─── Savings plans + contracts ────────────────────────────────────────────────

on("GET /admin/savings-plans", () => ({ data: db.savingsPlans }))

on("POST /admin/savings-plans", ({ opts }) => {
  const item = { id: db.nextId("sp"), available: true, ...((opts.body ?? {}) as Doc) }
  db.savingsPlans.push(item)
  return { data: item }
})

on("PUT /admin/savings-plans/:id", ({ params, opts }) => {
  const item = db.savingsPlans.find((p) => p.id === params.id) ?? notFound("Savings plan")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/savings-plans/:id", ({ params }) => {
  db.savingsPlans = db.savingsPlans.filter((p) => p.id !== params.id)
  return { data: { ok: true } }
})

on("GET /admin/savings-contracts", () => ({ data: db.savingsContracts }))

on("GET /admin/savings-contracts/savings-plan/:planId", ({ params }) => ({
  data: db.savingsContracts.filter((c) => c.savingsPlanId === params.planId),
}))

on("POST /admin/savings-contracts/:bpId", ({ params, opts }) => {
  const body = (opts.body ?? {}) as Doc
  const bp = db.billingProfiles.find((b) => b.id === params.bpId) ?? notFound("Billing profile")
  const plan = db.savingsPlans.find((p) => p.id === body.savingsPlanId) ?? notFound("Savings plan")
  const months = Number(body.durationMonths) || 12
  const start = new Date()
  const end = new Date(start.getFullYear(), start.getMonth() + months, start.getDate())
  const contract = {
    id: db.nextId("sc"),
    billingProfileId: params.bpId,
    savingsPlanId: plan.id,
    savingsPlanName: plan.name,
    status: "ACTIVE",
    durationMonths: months,
    monthlyCommittedAmount: body.monthlyCommittedAmount ?? 0,
    startDate: start.toISOString(),
    endDate: end.toISOString(),
    billingProfile: { email: bp.email, fullName: bp.fullName },
  }
  db.savingsContracts.push(contract)
  return { data: contract }
})

on("DELETE /admin/savings-contracts/:id", ({ params }) => {
  db.savingsContracts = db.savingsContracts.filter((c) => c.id !== params.id)
  return { data: { ok: true } }
})

// ─── Promotion codes ──────────────────────────────────────────────────────────

on("GET /admin/promotion-codes", () => ({ data: db.promotionCodes }))

on("POST /admin/promotion-codes", ({ opts }) => {
  const item = { id: db.nextId("code"), status: "ACTIVE", ...((opts.body ?? {}) as Doc) }
  db.promotionCodes.push(item)
  return { data: item }
})

on("PUT /admin/promotion-codes/:id", ({ params, opts }) => {
  const item = db.promotionCodes.find((c) => c.id === params.id) ?? notFound("Promotion code")
  Object.assign(item, (opts.body ?? {}) as Doc)
  return { data: item }
})

on("DELETE /admin/promotion-codes/:id", ({ params }) => {
  db.promotionCodes = db.promotionCodes.filter((c) => c.id !== params.id)
  return { data: { ok: true } }
})

// ─── Admin roles + permissions ────────────────────────────────────────────────

on("GET /admin/admin-roles", () => ({ data: db.adminRoles }))
on("GET /admin/admin-permissions/available-permissions", () => ({ data: db.availablePermissions }))

// Expand admin:* / admin:<area>:* grants the way the API's ExpandPatterns does.
function expandPermissions(patterns: string[]): string[] {
  const all = db.availablePermissions.map((p) => p.key as string)
  const out = new Set<string>()
  for (const pat of patterns) {
    if (pat === "admin:*") for (const k of all) out.add(k)
    else if (pat.endsWith(":*")) {
      const prefix = pat.slice(0, -1) // keep the trailing colon
      for (const k of all) if (k.startsWith(prefix)) out.add(k)
    } else out.add(pat)
  }
  return [...out]
}

on("POST /admin/admin-roles", ({ opts }) => {
  const body = (opts.body ?? {}) as Doc
  const permissions: string[] = body.permissions ?? []
  const role = {
    id: db.nextId("role"),
    name: body.name,
    description: body.description ?? "",
    permissions,
    expandedPermissions: expandPermissions(permissions),
    builtIn: false,
  }
  db.adminRoles.push(role)
  return { data: role }
})

on("PUT /admin/admin-roles/:id", ({ params, opts }) => {
  const role = db.adminRoles.find((r) => r.id === params.id) ?? notFound("Role")
  const body = (opts.body ?? {}) as Doc
  if (body.description != null) role.description = body.description
  if (body.permissions) {
    role.permissions = body.permissions
    role.expandedPermissions = expandPermissions(body.permissions)
  }
  return { data: role }
})

on("DELETE /admin/admin-roles/:id", ({ params }) => {
  const role = db.adminRoles.find((r) => r.id === params.id) ?? notFound("Role")
  if (role.builtIn) throw new ApiError(400, 400, "Built-in roles cannot be deleted.")
  db.adminRoles = db.adminRoles.filter((r) => r.id !== params.id)
  return { data: { ok: true } }
})

// ─── HMAC (SigV4) API keys ────────────────────────────────────────────────────

on("GET /admin/hmac-keys", () => ({ data: db.hmacKeys }))

on("POST /admin/hmac-keys", ({ opts }) => {
  const body = (opts.body ?? {}) as Doc
  const suffix = Math.random().toString(36).slice(2, 12).toUpperCase().padEnd(10, "X")
  const key = {
    id: `AKSTRA${suffix}`,
    description: body.description ?? "",
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
  }
  db.hmacKeys.push(key)
  // The secret is returned once at creation and never stored.
  return { data: { ...key, secretKey: `sk_${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}` } }
})

on("DELETE /admin/hmac-keys/:id", ({ params }) => {
  db.hmacKeys = db.hmacKeys.filter((k) => k.id !== params.id)
  return { data: { ok: true } }
})
