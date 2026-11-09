// Projects, organizations, invites, menu/features, search, user bootstrap.
import { on } from "../router"
import { db } from "../db"
import { features, ORG_ID, PID, organizations, uiMenu } from "../fixtures/platform"
import { mockProfile } from "../enabled"
import { myInvites } from "../fixtures/people"

// Page-URL preview flags (mock runs in-browser): "/?no-projects" previews the
// "/" onboarding states without touching fixtures — combine with "no-org"
// (create-org flow) or "invited" (pending-invite flow).
const pageFlag = (name: string) =>
  typeof window !== "undefined" && new URLSearchParams(window.location.search).has(name)

on("GET /project", () => ({ data: pageFlag("no-projects") ? [] : db.projects }))
on("POST /project", ({ opts }) => {
  const body = opts.body as { name?: string; organizationId?: string }
  const project = {
    id: db.nextId("prj"),
    name: body.name ?? "new-project",
    status: "ACTIVE",
    organizationId: body.organizationId ?? ORG_ID,
    billingProfileId: "bp-acme-1",
    memberships: [],
    services: [{ serviceId: "svc-proxmox-1" }],
  }
  db.projects.push(project)
  return { data: project }
})
on("POST /project/:pid/init", () => ({ data: {} }))
on("POST /project/:pid/rename", ({ params, opts }) => {
  const p = db.projects.find((x) => x.id === params.pid)
  if (p) p.name = (opts.body as { name?: string })?.name ?? p.name
  return { data: p }
})
on("DELETE /project/:pid", ({ params }) => {
  const p = db.projects.find((x) => x.id === params.pid)
  if (p) p.status = "SCHEDULED_FOR_DELETION"
  return { data: {} }
})
on("DELETE /project/:pid/cancel", ({ params }) => {
  const p = db.projects.find((x) => x.id === params.pid)
  if (p) p.status = "ACTIVE"
  return { data: {} }
})
on("DELETE /project/:pid/now", ({ params }) => {
  db.projects = db.projects.filter((x) => x.id !== params.pid)
  return { data: {} }
})
on("PUT /project/:pid/organization", () => ({ data: {} }))
on("POST /project/:pid/billing/:bpId", () => ({ data: {} }))
on("GET /project/:pid/service", () => ({
  data: [
    { id: "svc-proxmox-1", name: "ProxCloud Compute", type: "CLOUD", status: "ACTIVE" },
    { id: "svc-ceph-1", name: "ProxCloud S3", type: "CLOUD", status: "ACTIVE" },
    {
      id: "svc-sandbox-1", name: "ProxCloud Sandboxes", type: "CLOUD", status: "ACTIVE", sandboxE2B: true,
      sandboxTemplates: [{ name: "base", image: "registry.example.com/approved/base@sha256:" + "a".repeat(64), cpu: 2, memoryGiB: 2 }],
      sandboxLimits: { maxCpu: 8, maxMemoryGiB: 28, egressMbps: 100, maxTimeoutSeconds: 86400 },
      sandboxGpuModels: ["nvidia-a10"],
    },
    {
      id: "svc-dbaas-1",
      name: "ProxCloud DBaaS",
      type: "CLOUD",
      status: "ACTIVE",
      databasePlatformVersion: "0.1.0",
      databaseLimits: { maxCpu: 32, maxMemoryGiB: 128, maxStorageGiB: 2048 },
      databaseStorageClasses: ["proxmox-csi", "proxmox-csi-nvme"],
      databaseEngines: {
        postgresql: { versions: ["16", "17", "18"], default: "17", replicas: [1, 3] },
        mysql: { versions: ["8.4"], replicas: [1, 3] },
        mariadb: { versions: ["11.4"], replicas: [1, 3] },
        valkey: { versions: ["8"], replicas: [1, 3], beta: true },
        ferretdb: { versions: ["2"], replicas: [1, 3] },
        opensearch: { versions: ["3.3.0"], default: "3.3.0", replicas: [1, 3] },
        kafka: { versions: ["4.2.0", "4.2.1", "4.3.0"], default: "4.3.0", replicas: [3] },
      },
    },
  ],
}))

on("GET /organizations", () => ({ data: pageFlag("no-org") ? [] : organizations }))
on("GET /organizations/self-service", () => ({ data: { canCreateOrganization: true } }))
on("POST /organizations", ({ opts }) => ({
  data: { id: db.nextId("org"), name: (opts.body as { name?: string })?.name ?? "new-org" },
}))
on("PUT /organizations/:orgId", () => ({ data: {} }))
on("DELETE /organizations/:orgId", () => ({ data: {} }))

on("GET /project-invites/mine", () => ({
  data: pageFlag("invited")
    ? [
        { token: "tok-aurora", projectId: PID, projectName: "aurora-production", expiresAt: new Date(Date.now() + 7 * 86400_000).toISOString() },
        { token: "tok-staging", projectId: "prj-staging", projectName: "aurora-staging", expiresAt: new Date(Date.now() + 3 * 86400_000).toISOString() },
      ]
    : myInvites,
}))
// Any token resolves to a valid invite except the literal "missing" (previews
// the invite-not-found state on /join/missing).
on("GET /project-invites/:token", ({ params }) => ({
  data:
    params.token === "missing"
      ? {}
      : {
          email: mockProfile.email,
          projectId: PID,
          projectName: "aurora-production",
          expiresAt: new Date(Date.now() + 7 * 86400_000).toISOString(),
        },
}))
on("POST /project-invites/accept/:token", () => ({ data: {} }))
on("POST /project-invites/decline/:token", () => ({ data: {} }))
on("POST /project-invites/invite", () => ({ data: {} }))

// uiMenu + an admin Custom Menu item so /p/:pid/more/status-page renders the
// embedded-iframe chrome in dev (same-origin docs page stands in for the
// operator's status page; exercises {{project.id}} substitution).
on("GET /init/:pid", () => ({
  data: {
    ...uiMenu,
    features: { "svc-proxmox-1": ["serverSnapshots", "volumeRetype"] },
    menu: {
      ...uiMenu.menu,
      items: {
        ...uiMenu.menu.items,
        "status-page": {
          enabled: true,
          newMenuItem: true,
          displayName: "Status page",
          order: 1,
          renderMode: "IFRAME",
          url: `${window.location.origin}/docs?project={{project.id}}`,
        },
      },
    },
  },
}))
on("GET /features", () => ({ data: features }))
on("POST /user", () => ({ data: {} }))
on("POST /user/custom-info/:key", () => ({ data: {} }))
on("DELETE /user/custom-info/:key", () => ({ data: {} }))
on("GET /user/api-keys", () => ({
  data: [{ id: "pk" + "a".repeat(32), description: "terraform", createdAt: "2026-07-01T00:00:00Z" }],
}))
on("POST /user/api-keys", () => ({
  data: { id: "pk" + "b".repeat(32), token: "pk" + "b".repeat(32) + ".sk" + "c".repeat(40), description: "new", createdAt: "2026-07-23T00:00:00Z" },
}))
on("DELETE /user/api-keys/:id", () => ({ data: {} }))

on("GET /search/:pid", () => {
  const servers = db.cloud
    .filter((r) => r.type === "SERVER")
    .map((r) => ({
      type: "SERVER",
      data: {
        id: r.id,
        name: r.name,
        status: r.status,
        ipv4: r.data?.server?.addresses?.["net-private"]?.[0]?.addr,
        flavor: r.data?.server?.flavor?.original_name,
        region: "RegionOne",
      },
    }))
  const projects = db.projects.map((p) => ({ type: "PROJECT", data: { id: p.id, name: p.name, status: p.status } }))
  return { data: [...servers, ...projects] }
})
