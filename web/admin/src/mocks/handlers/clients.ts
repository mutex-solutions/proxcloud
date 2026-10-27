// Client-area endpoints: users, organizations, projects, billing profiles,
// cloud resources, bills, transactions, credits, validations, bank transfers.
import { ApiError } from "@/lib/api"
import { on } from "../router"
import { db } from "../db"

type Doc = Record<string, any>

const notFound = (what: string): never => {
  throw new ApiError(404, 404, `${what} not found.`)
}

const pdfResponse = (filename: string) =>
  new Response(new Blob([`%PDF-1.4\n% ProxCloud mock document: ${filename}\n%%EOF\n`], { type: "application/pdf" }), {
    status: 200,
    headers: { "Content-Disposition": `attachment; filename="${filename}"` },
  })

// ─── Users ────────────────────────────────────────────────────────────────────

on("GET /admin/user", () => ({ data: db.users }))

on("POST /admin/user", ({ opts }) => {
  const body = (opts.body ?? {}) as Doc
  const id = db.nextId("usr")
  const user: Doc = {
    id,
    sub: `kc-mock-${id}`,
    email: body.email,
    firstName: body.firstName,
    lastName: body.lastName,
    identities: [{ sub: `kc-mock-${id}`, issuer: "https://auth.example.com/realms/proxcloud" }],
    createdAt: new Date().toISOString(),
  }
  db.users.push(user)
  db.credentialsBySub[user.sub] = [
    { id: db.nextId("cred"), sub: user.sub, type: "password", password: { configured: true }, createdAt: user.createdAt },
  ]
  return { data: user }
})

on("GET /admin/user/:id", ({ params }) => ({
  data: db.users.find((u) => u.id === params.id) ?? notFound("User"),
}))

on("DELETE /admin/user/:id", ({ params }) => {
  db.users = db.users.filter((u) => u.id !== params.id)
  return { data: { ok: true } }
})

on("POST /admin/user/:id/impersonate", ({ params }) => {
  const u = db.users.find((x) => x.id === params.id) ?? notFound("User")
  return { data: { url: `https://console.example.com/?impersonate=${encodeURIComponent(u.sub)}` } }
})

// Keycloak user management (credentials + password reset).
on("GET /admin/user-management/credentials", ({ query }) => ({
  data: db.credentialsBySub[query.get("sub") ?? ""] ?? [],
}))

on("PUT /admin/user-management/password", () => ({ data: { ok: true } }))

on("DELETE /admin/user-management/credentials/:credentialId", ({ params, query }) => {
  const sub = query.get("sub") ?? ""
  db.credentialsBySub[sub] = (db.credentialsBySub[sub] ?? []).filter((c) => c.id !== params.credentialId)
  return { data: { ok: true } }
})

// ─── Organizations ────────────────────────────────────────────────────────────

on("GET /admin/organizations", () => ({ data: db.organizations }))

on("GET /admin/organizations/by-member/:sub", ({ params }) => ({
  data: db.organizations.filter((o) => (db.orgMembers[o.id] ?? []).some((m) => m.sub === params.sub)),
}))

on("GET /admin/organizations/:id", ({ params }) => {
  const org = db.organizations.find((o) => o.id === params.id) ?? notFound("Organization")
  const bp = db.billingProfiles.find((b) => b.id === org.billingProfileId)
  return {
    data: {
      ...org,
      memberCount: (db.orgMembers[org.id] ?? []).length,
      projectCount: db.projects.filter((p) => p.organizationId === org.id).length,
      billingProfile: bp
        ? { id: bp.id, name: bp.fullName, email: bp.email, status: bp.status, currency: bp.currency }
        : undefined,
    },
  }
})

on("PUT /admin/organizations/:id", ({ params, opts }) => {
  const org = db.organizations.find((o) => o.id === params.id) ?? notFound("Organization")
  const body = (opts.body ?? {}) as Doc
  if (body.name != null) org.name = body.name
  if (body.description != null) org.description = body.description
  return { data: org }
})

on("DELETE /admin/organizations/:id", ({ params }) => {
  if (db.projects.some((p) => p.organizationId === params.id)) {
    throw new ApiError(400, 400, "Organization still has projects.")
  }
  db.organizations = db.organizations.filter((o) => o.id !== params.id)
  return { data: { ok: true } }
})

on("GET /admin/organizations/:id/members", ({ params }) => ({ data: db.orgMembers[params.id] ?? [] }))

on("POST /admin/organizations/:id/member", ({ params, opts }) => {
  const body = (opts.body ?? {}) as Doc
  const user = db.users.find((u) => u.id === body.userId || u.sub === body.userId) ?? notFound("User")
  const members = (db.orgMembers[params.id] ??= [])
  if (!members.some((m) => m.sub === user.sub)) {
    members.push({ sub: user.sub, firstName: user.firstName, lastName: user.lastName, email: user.email, role: body.role ?? "MEMBER" })
  }
  return { data: { ok: true } }
})

on("DELETE /admin/organizations/:id/member/:sub", ({ params }) => {
  const members = db.orgMembers[params.id] ?? []
  const target = members.find((m) => m.sub === params.sub)
  if (target?.role === "OWNER") throw new ApiError(400, 400, "Organization owners cannot be removed.")
  db.orgMembers[params.id] = members.filter((m) => m.sub !== params.sub)
  return { data: { ok: true } }
})

on("PUT /admin/organizations/:id/member/:sub/role", ({ params, opts }) => {
  const member = (db.orgMembers[params.id] ?? []).find((m) => m.sub === params.sub) ?? notFound("Member")
  member.role = ((opts.body ?? {}) as Doc).role ?? member.role
  return { data: { ok: true } }
})

// ─── Projects ─────────────────────────────────────────────────────────────────

const withOrg = (p: Doc): Doc => ({
  ...p,
  organization: db.organizations.find((o) => o.id === p.organizationId)
    ? { name: db.organizations.find((o) => o.id === p.organizationId)!.name }
    : undefined,
})

on("GET /admin/project", () => ({ data: db.projects.map(withOrg) }))

on("GET /admin/project/by-user", ({ query }) => {
  const sub = query.get("sub") ?? ""
  return { data: db.projects.filter((p) => (p.memberships ?? []).some((m: Doc) => m.sub === sub)) }
})

on("GET /admin/project/by-organization", ({ query }) => ({
  data: db.projects.filter((p) => p.organizationId === query.get("organizationId")),
}))

// Projects of a billing profile (raw docs).
on("GET /admin/project/:bpId/billing-profile", ({ params }) => ({
  data: db.projects.filter((p) => p.billingProfileId === params.bpId),
}))

on("GET /admin/project/:id", ({ params }) => ({
  data: db.projects.find((p) => p.id === params.id) ?? notFound("Project"),
}))

on("DELETE /admin/project/:id", ({ params }) => {
  db.projects = db.projects.filter((p) => p.id !== params.id)
  return { data: { ok: true } }
})

on("GET /admin/project/:id/resources/counts", ({ params }) => {
  const resources = db.cloudResources.filter((r) => r.projectId === params.id)
  const counts: Record<string, number> = { TOTAL: resources.length }
  for (const r of resources) counts[r.type] = (counts[r.type] ?? 0) + 1
  return { data: counts }
})

on("GET /admin/project/:id/gpu-usage", ({ params }) => ({
  data: {
    usage:
      params.id === "prj-0001"
        ? { "nvidia-a100-80gb": 1 }
        : params.id === "prj-0002"
          ? { "nvidia-l40s": 1 }
          : {},
    usageAvailable: params.id !== "prj-0002",
  },
}))

on("GET /admin/project/:id/members", ({ params }) => ({ data: db.projectMembers[params.id] ?? [] }))

on("POST /admin/project/:id/sync", () => ({ data: { ok: true } }))

on("PUT /admin/project/:id/quota", ({ params, opts }) => {
  const project = db.projects.find((p) => p.id === params.id) ?? notFound("Project")
  const quota = (opts.body ?? {}) as Doc
  const gpu = quota.gpu as Doc | undefined
  for (const [model, limit] of Object.entries(gpu ?? {})) {
    const canonical = model.trim() === "*" ? "*" : model.trim().toLowerCase().replaceAll("_", "-")
    if (model !== canonical || !Number.isSafeInteger(limit) || Number(limit) < 0) {
      throw new ApiError(400, 400, "GPU quota keys must be canonical and limits must be non-negative integers.")
    }
  }
  project.quota = quota
  return { data: project }
})

on("PUT /admin/project/:id/gpu-capacity-visible", ({ params, opts }) => {
  const project = db.projects.find((p) => p.id === params.id) ?? notFound("Project")
  project.gpuCapacityVisible = !!(opts.body as Doc)?.gpuCapacityVisible
  return { data: project }
})

on("PUT /admin/project/:id/public-networks", ({ params, opts }) => {
  const project = db.projects.find((p) => p.id === params.id) ?? notFound("Project")
  const body = (opts.body ?? {}) as Doc
  project.publicNetworkIds = body.publicNetworkIds ?? null
  project.publicNetworksVisible = !!body.publicNetworksVisible
  return { data: project }
})

on("GET /admin/project/:id/external-service/:esId", ({ params }) => {
  const project = db.projects.find((p) => p.id === params.id) ?? notFound("Project")
  const svc = db.services.find((s) => s.id === params.esId) ?? notFound("Cloud provider")
  const services = (project.services ??= []) as Doc[]
  if (!services.some((s) => s.serviceId === svc.id)) {
    services.push(
      svc.config?.provider === "ceph-s3"
        ? { serviceId: svc.id, provider: "ceph-s3", region: svc.config.region, rgwUid: `${svc.config.uidPrefix ?? ""}${project.id}` }
        : { serviceId: svc.id, externalProjectId: db.nextId("os-project") },
    )
  }
  return { data: project }
})

// POST /admin/project/{id}/{ENABLED|DISABLED} — status update.
on("POST /admin/project/:id/:status", ({ params }) => {
  if (params.status !== "ENABLED" && params.status !== "DISABLED") notFound("Project action")
  const project = db.projects.find((p) => p.id === params.id) ?? notFound("Project")
  project.status = params.status
  return { data: project }
})

// Project membership manager (persists, cloud propagation is a seam upstream).
on("POST /admin/projects/manage", ({ opts }) => {
  const body = (opts.body ?? {}) as Doc
  const user = db.users.find((u) => u.id === body.userId || u.sub === body.userId) ?? notFound("User")
  const project = db.projects.find((p) => p.id === body.projectId) ?? notFound("Project")
  const memberships = (project.memberships ??= []) as Doc[]
  if (!memberships.some((m) => m.sub === user.sub)) memberships.push({ sub: user.sub, role: body.role ?? "MEMBER" })
  const members = (db.projectMembers[project.id] ??= [])
  if (!members.some((m) => m.sub === user.sub)) {
    members.push({ id: user.id, sub: user.sub, email: user.email, firstName: user.firstName, lastName: user.lastName })
  }
  return { data: { ok: true } }
})

on("POST /admin/projects/manage/remove", ({ opts }) => {
  const body = (opts.body ?? {}) as Doc
  const project = db.projects.find((p) => p.id === body.projectId) ?? notFound("Project")
  project.memberships = ((project.memberships ?? []) as Doc[]).filter((m) => m.sub !== body.sub)
  db.projectMembers[project.id] = (db.projectMembers[project.id] ?? []).filter((m) => m.sub !== body.sub)
  return { data: { ok: true } }
})

// ─── Cloud resources ──────────────────────────────────────────────────────────

on("GET /admin/cloud-resource", () => ({ data: db.cloudResources }))

on("GET /admin/cloud-resource/user/:userId", ({ params }) => {
  const ids = db.resourcesByUser[params.userId] ?? []
  return { data: db.cloudResources.filter((r) => ids.includes(r.id)) }
})

on("GET /admin/cloud-resource/project/:projectId", ({ params }) => ({
  data: db.cloudResources.filter((r) => r.projectId === params.projectId),
}))

on("GET /admin/cloud-resource/public-networks/:esId", () => ({ data: db.publicNetworks }))

on("GET /admin/cloud-resource/:id", ({ params }) => ({
  data: db.cloudResources.find((r) => r.id === params.id) ?? notFound("Cloud resource"),
}))

on("GET /admin/cloud-resource/:id/sync", ({ params }) => ({
  data: db.cloudResources.find((r) => r.id === params.id) ?? notFound("Cloud resource"),
}))

on("DELETE /admin/cloud-resource/:id", ({ params }) => {
  db.cloudResources = db.cloudResources.filter((r) => r.id !== params.id)
  return { data: { ok: true } }
})

// ─── Billing profiles ─────────────────────────────────────────────────────────

on("GET /admin/billing-profile", () => ({ data: db.billingProfiles }))

// Exact-match raw-doc filter (organizationId / email).
on("GET /admin/billing-profile/search", ({ query }) => {
  let rows = db.rawBillingProfiles
  const orgId = query.get("organizationId")
  const email = query.get("email")
  if (orgId) rows = rows.filter((r) => r.organizationId === orgId)
  if (email) rows = rows.filter((r) => r.email === email)
  return { data: rows }
})

on("GET /admin/billing-profile/validations", () => ({
  data: db.validations.filter((v) => v.status === "PENDING"),
}))

on("POST /admin/billing-profile/validations/:id/status/:status", ({ params }) => {
  const v = db.validations.find((x) => x.id === params.id) ?? notFound("Billing profile validation")
  v.status = params.status
  const bp = db.billingProfiles.find((b) => b.id === v.billingProfileId)
  const raw = db.rawBillingProfiles.find((b) => b.id === v.billingProfileId)
  if (params.status === "APPROVED" && bp) bp.status = "ACTIVE"
  if (bp) bp.validationStatus = params.status
  if (raw) raw.validationStatus = params.status
  return { data: v }
})

on("GET /admin/billing-profile/financial/:id", ({ params }) => ({
  data: db.financialByBp[params.id] ?? {},
}))

on("PUT /admin/billing-profile/automatic-suspension/:id", ({ params, opts }) => {
  const raw = db.rawBillingProfiles.find((b) => b.id === params.id) ?? notFound("Billing profile")
  const body = (opts.body ?? {}) as Doc
  raw.overwriteSuspension = !!body.overwriteSuspension
  raw.suspensionConfiguration = body.suspensionConfiguration ?? undefined
  return { data: raw }
})

on("PUT /admin/billing-profile/tax-configuration/:id", ({ params, opts }) => {
  const raw = db.rawBillingProfiles.find((b) => b.id === params.id) ?? notFound("Billing profile")
  raw.taxConfiguration = (opts.body ?? {}) as Doc
  return { data: raw }
})

on("PUT /admin/billing-profile/project-provisioning-quota/:id", ({ params, opts }) => {
  const raw = db.rawBillingProfiles.find((b) => b.id === params.id) ?? notFound("Billing profile")
  raw.projectProvisioningQuota = (opts.body ?? {}) as Doc
  return { data: raw }
})

on("GET /admin/billing-profile/:id", ({ params }) => ({
  data: db.billingProfiles.find((b) => b.id === params.id) ?? notFound("Billing profile"),
}))

on("PUT /admin/billing-profile/:id", ({ params, opts }) => {
  const bp = db.billingProfiles.find((b) => b.id === params.id) ?? notFound("Billing profile")
  const raw = db.rawBillingProfiles.find((b) => b.id === params.id)
  const body = (opts.body ?? {}) as Doc
  Object.assign(bp, body)
  if (raw) Object.assign(raw, body)
  if (body.firstName != null || body.lastName != null) {
    bp.fullName = [bp.firstName, bp.lastName].filter(Boolean).join(" ")
    if (raw) raw.fullName = bp.fullName
  }
  return { data: bp }
})

on("POST /admin/billing-profile/:id/action/:target", ({ params }) => {
  const bp = db.billingProfiles.find((b) => b.id === params.id) ?? notFound("Billing profile")
  const raw = db.rawBillingProfiles.find((b) => b.id === params.id)
  bp.status = params.target
  if (raw) raw.status = params.target
  return { data: bp }
})

on("GET /admin/billing-profile/:id/cost-info", ({ params }) => ({
  data: db.costInfoByBp[params.id] ?? {
    dueAmount: 0,
    currentMonthCosts: 0,
    lastMonthCosts: 0,
    forecastedMonthEndCosts: 0,
    billingProfileCostInfo: { currentMonthCostsByType: {}, lastMonthCostsByType: {}, topResourcePrices: [] },
  },
}))

// ─── Suspension processes ─────────────────────────────────────────────────────

on("GET /admin/suspensions/:bpId", ({ params }) => ({ data: db.suspensionsByBp[params.bpId] ?? [] }))

// ─── Bills ────────────────────────────────────────────────────────────────────

on("GET /admin/bill", () => ({ data: db.bills }))

on("GET /admin/bill/:bpId/billing-profile", ({ params }) => ({
  data: db.billOverviewsByBp[params.bpId] ?? [],
}))

on("GET /admin/bill/download/:billId", ({ params }) => ({
  data: pdfResponse(`bill-${params.billId}.pdf`),
}))

// ─── Transactions ─────────────────────────────────────────────────────────────

on("GET /admin/account-credit-transactions", () => ({ data: db.accountCreditTransactions }))

on("GET /admin/account-credit-transactions/:bpId/billing-profile", ({ params }) => ({
  data: db.accountCreditTransactions.filter((t) => t.billingProfileId === params.bpId),
}))

on("GET /admin/account-credit-transactions/:id/sync", ({ params }) => {
  const t = db.accountCreditTransactions.find((x) => x.id === params.id) ?? notFound("Transaction")
  if (t.status === "PENDING") t.status = "SUCCESS"
  return { data: t }
})

on("POST /admin/account-credit-transactions/refund/:id", ({ params }) => {
  const t = db.accountCreditTransactions.find((x) => x.id === params.id) ?? notFound("Transaction")
  t.status = "REFUNDED"
  return { data: t }
})

on("GET /admin/collect-transactions", () => ({ data: db.collectTransactions }))

on("GET /admin/collect-transactions/:bpId/billing-profile", ({ params }) => ({
  data: db.collectTransactions.filter((t) => t.billingProfileId === params.bpId),
}))

on("GET /admin/collect-transactions/download/:id", ({ params }) => ({
  data: pdfResponse(`receipt-${params.id}.pdf`),
}))

// ─── Account credits ──────────────────────────────────────────────────────────

on("GET /admin/account-credit", ({ query }) => ({
  data: db.accountCreditsByBp[query.get("billingProfileId") ?? ""] ?? [],
}))

on("POST /admin/account-credit/:bpId", ({ params, opts }) => {
  const bp = db.billingProfiles.find((b) => b.id === params.bpId) ?? notFound("Billing profile")
  const amount = Number(((opts.body ?? {}) as Doc).amount) || 0
  const credit = {
    id: db.nextId("acr"),
    billingProfileId: params.bpId,
    amount,
    initialAmount: amount,
    currency: bp.currency,
    createdAt: new Date().toISOString(),
  }
  ;(db.accountCreditsByBp[params.bpId] ??= []).push(credit)
  bp.accountCredit = (Number(bp.accountCredit) || 0) + amount
  return { data: credit }
})

// ─── Promotional credits ──────────────────────────────────────────────────────

on("GET /admin/promotional-credits/billing-profile/:bpId", ({ params }) => ({
  data: db.promoCreditsByBp[params.bpId] ?? [],
}))

on("POST /admin/promotional-credits", ({ opts }) => {
  const body = (opts.body ?? {}) as Doc
  const amount = Number(body.amount) || 0
  const days = Number(body.daysValidity) || 30
  const credit = {
    id: db.nextId("pcr"),
    billingProfileId: body.billingProfileId,
    code: "",
    initialAmount: amount,
    remainingAmount: amount,
    expirationDate: new Date(Date.now() + days * 86_400_000).toISOString(),
    createdAt: new Date().toISOString(),
  }
  ;(db.promoCreditsByBp[body.billingProfileId] ??= []).push(credit)
  return { data: credit }
})

on("DELETE /admin/promotional-credits/:id", ({ params }) => {
  for (const key of Object.keys(db.promoCreditsByBp)) {
    db.promoCreditsByBp[key] = db.promoCreditsByBp[key].filter((c) => c.id !== params.id)
  }
  return { data: { ok: true } }
})

// ─── Bank transfers ───────────────────────────────────────────────────────────

on("GET /admin/bank-transfer", ({ query }) => {
  const gw = query.get("integrationId")
  return { data: db.bankTransfers.filter((t) => !gw || t.integrationId === gw) }
})

on("POST /admin/bank-transfer/:id/:action", ({ params }) => {
  const t = db.bankTransfers.find((x) => x.id === params.id) ?? notFound("Bank transfer")
  t.status = params.action === "approve" ? "SUCCESS" : "REJECTED"
  return { data: t }
})
