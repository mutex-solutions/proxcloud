// Org members/roles/audit + project members + account profile.
import type { Paging } from "@/lib/api"
import { on } from "../router"
import { db } from "../db"
import { accountAuditEvents, accountDetails, orgAuditEvents, permissionMeta } from "../fixtures/people"

// Cursor paging like the Go API: ?limit=&after=<last id> → paging.nextMarker.
function pageByCursor<T extends { id?: string }>(items: T[], query: URLSearchParams) {
  const limit = Math.max(1, Number(query.get("limit")) || 50)
  const after = query.get("after")
  const start = after ? items.findIndex((e) => e.id === after) + 1 : 0
  const page = items.slice(start, start + limit)
  const nextMarker = start + limit < items.length ? page[page.length - 1]?.id : undefined
  return { data: page, ...(nextMarker ? { paging: { nextMarker } as unknown as Paging } : {}) }
}

on("GET /organizations/:orgId/members", () => ({ data: db.orgMembers }))
on("POST /organizations/:orgId/member", ({ opts }) => {
  const body = opts.body as { email?: string; role?: string }
  db.orgMembers.push({ sub: db.nextId("user"), firstName: "Invited", lastName: "User", email: body.email ?? "", role: body.role ?? "MEMBER" })
  return { data: {} }
})
on("PUT /organizations/:orgId/member/:sub/role", ({ params, opts }) => {
  const m = db.orgMembers.find((x) => x.sub === params.sub)
  if (m) m.role = (opts.body as { role?: string })?.role ?? m.role
  return { data: {} }
})
on("DELETE /organizations/:orgId/member/:sub", ({ params }) => {
  db.orgMembers = db.orgMembers.filter((x) => x.sub !== params.sub)
  return { data: {} }
})

on("GET /organizations/:orgId/roles", () => ({ data: db.orgRoles }))
on("GET /organizations/:orgId/roles/permissions", () => ({ data: permissionMeta }))
on("POST /organizations/:orgId/roles", ({ opts }) => {
  const body = opts.body as { name?: string; description?: string; permissions?: string[] }
  db.orgRoles.push({ id: db.nextId("role"), name: body.name ?? "custom-role", description: body.description ?? "", permissions: body.permissions ?? [], expandedPermissions: body.permissions ?? [], builtIn: false })
  return { data: {} }
})
on("PUT /organizations/:orgId/roles/:roleId", () => ({ data: {} }))
on("DELETE /organizations/:orgId/roles/:roleId", ({ params }) => {
  db.orgRoles = db.orgRoles.filter((r) => r.id !== params.roleId)
  return { data: {} }
})

on("GET /organizations/:orgId/audit", ({ query }) => pageByCursor(orgAuditEvents, query))
on("GET /organizations/:orgId/audit/export", () => ({
  data: new Response(new Blob(["id,timestamp,action,outcome\nevt-1,2026-07-10T09:15:00Z,server.start,SUCCESS\n"], { type: "text/csv" })),
}))

on("GET /project/:pid/members", () => ({ data: db.projectMembers }))
on("POST /project/:pid/members", () => ({ data: {} }))
on("PUT /project/:pid/members/:sub/role", ({ params, opts }) => {
  const m = db.projectMembers.find((x) => x.sub === params.sub)
  if (m) m.role = (opts.body as { role?: string })?.role ?? m.role
  return { data: {} }
})
on("DELETE /project/:pid/members", ({ query }) => {
  const sub = query.get("sub")
  db.projectMembers = db.projectMembers.filter((x) => x.sub !== sub)
  return { data: {} }
})

on("GET /account/details", () => ({ data: accountDetails }))
on("GET /account/audit", ({ query }) => pageByCursor(accountAuditEvents, query))
on("POST /account/name", ({ opts }) => {
  const body = opts.body as { firstName?: string; lastName?: string }
  accountDetails.firstName = body.firstName ?? accountDetails.firstName
  accountDetails.lastName = body.lastName ?? accountDetails.lastName
  return { data: accountDetails }
})
