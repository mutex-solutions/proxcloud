import { expect, test, type Page } from "@playwright/test"
import path from "node:path"

const project = "/p/prj-aurora"
const image = (name: string) => path.resolve("public/docs-img", `${name}.png`)

// These are test-only API fixtures. No application DOM or feature controls are fabricated.
// All visible data is synthetic; only the actual current React components render it.
async function demo(page: Page, extra: "default" | "billing" | "invites" = "default") {
  await page.clock.setFixedTime(new Date("2026-11-10T10:00:00Z"))
  await page.addInitScript(() => localStorage.setItem("proxcloud.theme", "light"))
  await page.goto("/")
  await expect(page.getByText("aurora-production", { exact: true }).first()).toBeVisible()
  await page.evaluate(async (variant) => {
    const apiPath = "/src/lib/api.ts", routerPath = "/src/mocks/router.ts", dbPath = "/src/mocks/db.ts", platformPath = "/src/mocks/fixtures/platform.ts", billingPath = "/src/mocks/fixtures/billing.ts", peoplePath = "/src/mocks/fixtures/people.ts"
    const [{ setMockHandler, ApiError }, { dispatch }, { db }, platform, billing, people] = await Promise.all([
      import(/* @vite-ignore */ apiPath), import(/* @vite-ignore */ routerPath), import(/* @vite-ignore */ dbPath), import(/* @vite-ignore */ platformPath), import(/* @vite-ignore */ billingPath), import(/* @vite-ignore */ peoplePath),
    ])
    const createdAt = "2026-11-09T09:30:00Z"
    const server = (id: string, vmid: string, name: string, status: string, ip: string, vcpus = 2, ram = 4096) => ({
      id, externalId: vmid, name, status, projectId: "prj-aurora", serviceId: "svc-proxmox-1", region: "pve", type: "SERVER", createdAt,
      info: { createdAt, updatedAt: "2026-11-10T09:50:00Z" },
      data: { server: { id: vmid, name, status, addresses: { "demo-network": [{ addr: ip, version: 4 }] }, flavor: { original_name: "s1-medium", vcpus, ram, disk: 30 }, created: createdAt } },
    })
    db.cloud = [
      server("res-server-001", "100", "demo-web", "ACTIVE", "192.0.2.10"),
      server("res-server-002", "101", "demo-worker", "SHUTOFF", "192.0.2.11", 4, 8192),
      { id: "res-network-001", externalId: "vdemo", name: "demo-network", type: "NETWORK", status: "ACTIVE", projectId: "prj-aurora", serviceId: "svc-proxmox-1", region: "pve", createdAt, data: { network: { id: "vdemo", name: "demo-network", status: "ACTIVE", shared: false, subnets: ["192.0.2.0/24"], cidr: "192.0.2.0/24", gateway: "192.0.2.1", snat: true, dhcp: false, zone: "demo-zone" } } },
      { id: "res-keypair-001", name: "demo-key", type: "KEYPAIR", status: "ACTIVE", projectId: "prj-aurora", data: { keypair: { name: "demo-key", fingerprint: "SHA256:documentation-example" } } },
      { id: "res-security-group-001", externalId: "gdemo", name: "demo-ssh", type: "SECURITY_GROUP", status: "ACTIVE", projectId: "prj-aurora", data: { securityGroup: { id: "gdemo", name: "demo-ssh", description: "Documentation example — restricted SSH", security_group_rules: [{ id: "demo-rule-ssh", direction: "ingress", ethertype: "IPv4", protocol: "tcp", port_range: "22", remote_ip_prefix: "198.51.100.0/24" }] } } },
      { id: "res-volume-001", externalId: "demo-store:100/vm-100-disk-0.qcow2", name: "demo-web-boot", type: "VOLUME", status: "IN-USE", projectId: "prj-aurora", createdAt, data: { volume: { name: "demo-web-boot", size: 30, status: "in-use", volume_type: "standard", vmid: 100, slot: "scsi0", bootable: true } } },
      { id: "res-volume-002", externalId: "demo-store:100/vm-100-disk-1.qcow2", name: "demo-data", type: "VOLUME", status: "IN-USE", projectId: "prj-aurora", createdAt, data: { volume: { name: "demo-data", size: 100, status: "in-use", volume_type: "standard", vmid: 100, slot: "scsi1", bootable: false } } },
      { id: "res-volume-003", externalId: "demo-store:102/vm-102-disk-0.qcow2", name: "demo-detached", type: "VOLUME", status: "AVAILABLE", projectId: "prj-aurora", createdAt, data: { volume: { name: "demo-detached", size: 50, status: "available", volume_type: "standard", bootable: false } } },
      { id: "res-bucket-001", externalId: "demo-assets", name: "demo-assets", type: "BUCKET", status: "ACTIVE", projectId: "prj-aurora", serviceId: "svc-ceph-1", createdAt, data: { bucketName: "demo-assets", objectCount: 3, sizeInGb: "0.2", storageBackend: "CEPH_S3" } },
    ]
    db.projects[0].name = "docs-demo"
    db.projects[0].gpuCapacityVisible = false
    db.projects[0].publicNetworksVisible = false
    db.projects = [db.projects[0]]
    platform.locations.splice(0, platform.locations.length,
      { serviceId: "svc-proxmox-1", region: "pve", displayName: "Demo PVE", resourceTypes: ["SERVER", "KEYPAIR", "IMAGE", "VOLUME", "VOLUME_SNAPSHOT", "NETWORK", "SECURITY_GROUP"], provider: "proxmox", serviceName: "Demo PVE" },
      { serviceId: "svc-ceph-1", region: "s3", displayName: "Demo Object Storage", resourceTypes: ["BUCKET"], provider: "ceph-s3", serviceName: "Demo S3" },
    )
    platform.organizations[0].name = "Example Organization"
    if (variant === "billing") Object.assign(billing.billingSummary, { status: "NEW", hasBillingDetails: false, fullName: "" })
    if (variant === "invites") people.myInvites.push({ token: "documentation-invitation", projectId: "prj-demo", projectName: "docs-demo", expiresAt: "2026-11-17T10:00:00Z" })
    const types = [
      { id: "standard", name: "standard", displayName: "SSD", default: true, extra_specs: { class: "standard", bootDisk: true } },
      { id: "capacity", name: "capacity", displayName: "Capacity HDD", extra_specs: { class: "capacity", bootDisk: false } },
    ]
    const knownReads = [/^\/project$/, /^\/organizations$/, /^\/organizations\/self-service$/, /^\/project-invites\/mine$/, /^\/project-invites\/[^/]+$/, /^\/init\/[^/]+$/, /^\/features$/, /^\/project\/[^/]+\/(?:resource-types|service|public-networks|quota-usage|gpu-capacity|billing|cost-info)$/, /^\/project\/[^/]+\/cloud\/[^/]+$/, /^\/bill\/[^/]+(?:\/cost-info)?$/, /^\/savings-plans$/, /^\/savings-contracts\/[^/]+(?:\/[^/]+\/eligible)?$/, /^\/billing-profile(?:\/countries)?$/, /^\/payment\/[^/]+\/gateway$/, /^\/card\/[^/]+$/, /^\/flavor-categories$/, /^\/groups\/flavors$/, /^\/groups\/images$/, /^\/image-groups$/, /^\/image-categories$/]
    const safeCollections = new Set(["LIST_FLAVORS", "PUBLIC_IMAGES", "LIST_VOLUME_TYPES", "LIST_TYPES"])
    const safeActions = new Set(["LIST_SECURITY_GROUPS", "LIST_EVENTS", "LIST_BACKUPS", "LIST_RULES", "GET_SERVERS", "LIST_OBJECTS", "GET_SETTINGS", "IS_BUCKET_PUBLIC"])
    setMockHandler(async (requestPath: string, opts: { method?: string; body?: any }) => {
      const pathname = requestPath.split("?")[0]
      const method = (opts.method ?? (opts.body === undefined ? "GET" : "POST")).toUpperCase()
      if (method === "GET" && pathname === "/project-invites/documentation-invitation") return { data: { email: "dev@example.com", projectId: "prj-aurora", projectName: "docs-demo", expiresAt: "2026-11-17T10:00:00Z" } }
      if (method === "GET" && /^\/init\//.test(pathname)) return { data: { ...platform.uiMenu, features: { "svc-proxmox-1": ["serverSnapshots", "volumeRetype"] } } }
      if (method === "GET" && /^\/project\/[^/]+\/public-networks$/.test(pathname)) return { data: [] }
      if (method === "POST" && /^\/project\/[^/]+\/cloud\/action$/.test(pathname)) {
        const action = opts.body?.action
        if (!safeCollections.has(action)) { console.error("[docs-capture] refused unknown collection action"); throw new ApiError(403, 403, "Documentation capture refused unknown action") }
        if (action === "LIST_VOLUME_TYPES" || action === "LIST_TYPES") return { data: { result: types } }
        if (action === "LIST_FLAVORS") return { data: { result: [
          { externalId: "demo-small", data: { id: "demo-small", name: "s1-small", vcpus: 1, ram: 2048, disk: 20 } },
          { externalId: "demo-medium", data: { id: "demo-medium", name: "s1-medium", vcpus: 2, ram: 4096, disk: 40 } },
        ] } }
        if (action === "PUBLIC_IMAGES") return { data: { result: [{ id: "debian-13", name: "Debian 13", os_distro: "debian", min_disk: 3, status: "active" }, { id: "ubuntu-2404", name: "Ubuntu 24.04", os_distro: "ubuntu", min_disk: 3, status: "active" }] } }
      }
      if (method === "POST" && /^\/project\/[^/]+\/cloud\/[^/]+\/action$/.test(pathname)) {
        if (!safeActions.has(opts.body?.action)) { console.error("[docs-capture] refused resource mutation"); throw new ApiError(403, 403, "Documentation capture refused mutation") }
        if (opts.body?.action === "LIST_BACKUPS") return { data: { result: [] } }
      } else if (method === "POST" && /^\/project\/[^/]+\/resource$/.test(pathname)) {
        // Read-only list endpoint; fixtures return PVE-shaped resources.
      } else if (method === "POST" && /^\/project\/[^/]+\/cloud\/action$/.test(pathname)) {
        // Collection reads validated above.
      } else if (method === "POST" && /^\/user$/.test(pathname)) {
        return { data: {} }
      } else if (!(method === "GET" && knownReads.some((pattern) => pattern.test(pathname)))) {
        console.error(`[docs-capture] refused unhandled ${method} ${pathname}`)
        throw new ApiError(403, 403, "Documentation capture refused an unhandled request")
      }
      return dispatch(requestPath, opts)
    })
  }, extra)
}

async function open(page: Page, route: string) {
  await page.evaluate((next) => {
    history.pushState({}, "", next)
    dispatchEvent(new PopStateEvent("popstate"))
  }, route)
}

async function capture(page: Page, name: string, fullPage = false) {
  await page.evaluate(() => document.fonts.ready)
  await expect(page.getByText("docs-demo", { exact: true }).first()).toBeVisible()
  if (fullPage) {
    // Screenshot the actual page at its natural content height. Resizing before the
    // capture keeps sticky navigation at the top instead of stitching it mid-wizard.
    const height = await page.evaluate(() => document.documentElement.scrollHeight)
    await page.setViewportSize({ width: 1440, height })
    await page.evaluate(() => window.scrollTo(0, 0))
    await expect(page.getByRole("button", { name: "Create server", exact: true })).toBeInViewport()
  }
  await page.mouse.move(1430, 990)
  await page.screenshot({ path: image(name), fullPage, animations: "disabled", caret: "hide" })
}

test.beforeEach(async ({ page, context }) => {
  const problems: string[] = []
  page.on("pageerror", (error) => problems.push(error.message))
  page.on("console", (message) => { if (message.type() === "error" || /\[mock\] (?:no handler|unknown)/.test(message.text())) problems.push(message.text()) })
  await context.route("**/*", async (route) => {
    const url = new URL(route.request().url())
    if (url.origin === "http://127.0.0.1:5273" && !url.pathname.startsWith("/api/")) return route.continue()
    if (url.protocol === "data:" || url.protocol === "blob:") return route.continue()
    problems.push(`Blocked non-local request: ${url.origin}${url.pathname}`)
    await route.abort("blockedbyclient")
  })
  ;(page as Page & { docsProblems: string[] }).docsProblems = problems
})
test.afterEach(async ({ page }) => {
  expect((page as Page & { docsProblems: string[] }).docsProblems, "No console/page errors or unhandled/external requests").toEqual([])
})

test("render documentation consumers with all screenshot images loaded", async ({ page }) => {
  for (const slug of ["getting-started/first-server", "guides/networks", "guides/volumes", "guides/team-members"]) {
    await page.goto(`/docs/${slug}`)
    const illustrations = page.locator('img[src^="/docs-img/"]')
    await expect(illustrations.first()).toBeVisible()
    await expect.poll(async () => illustrations.evaluateAll((images) => images.every((image) => image instanceof HTMLImageElement && image.complete && image.naturalWidth > 0))).toBe(true)
    await expect(page.getByText(/synthetic (?:demo data|invitation)/).first()).toBeVisible()
  }
})

test("capture a synthetic pending project invitation", async ({ page }) => {
  await demo(page, "invites")
  await open(page, "/join-project?invite-token=documentation-invitation")
  await expect(page.getByRole("button", { name: /Accept invite/ })).toBeVisible()
  await expect(page.getByText("docs-demo", { exact: true })).toBeVisible()
  await page.evaluate(() => document.fonts.ready)
  await page.screenshot({ path: image("join-project-accept"), animations: "disabled", caret: "hide" })
})

test("recapture existing client documentation images with synthetic current PVE UI", async ({ page }) => {
  await demo(page)
  await open(page, `${project}/servers`)
  await expect(page.getByRole("heading", { name: "Servers", exact: true })).toBeVisible()
  await expect(page.getByText("demo-web", { exact: true })).toBeVisible()
  await capture(page, "servers-list")
  await open(page, `${project}/servers/res-server-001`)
  await expect(page.getByRole("heading", { name: "demo-web", exact: true })).toBeVisible()
  await expect(page.getByText("192.0.2.10", { exact: true }).first()).toBeVisible()
  await capture(page, "server-detail")
  await open(page, `${project}/servers/new`)
  await page.getByRole("button", { name: /Debian 13/ }).click()
  await page.getByRole("button", { name: /s1-medium/ }).click()
  await page.getByText("demo-network", { exact: true }).locator("..").getByRole("checkbox").click()
  await page.getByLabel("Server name", { exact: true }).fill("demo-api")
  await expect(page.getByRole("button", { name: "Create server", exact: true })).toBeEnabled()
  await capture(page, "create-server", true)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await open(page, `${project}/networks`)
  await page.getByRole("button", { name: "Create network", exact: true }).first().click()
  await page.getByRole("dialog").getByLabel("Name", { exact: true }).fill("demo-network-2")
  await capture(page, "create-network")
  await page.keyboard.press("Escape")
  await open(page, `${project}/security-groups/res-security-group-001`)
  await expect(page.getByRole("heading", { name: "demo-ssh", exact: true })).toBeVisible()
  await expect(page.getByRole("cell", { name: "198.51.100.0/24", exact: true })).toBeVisible()
  await expect(page.getByRole("cell", { name: "22", exact: true })).toBeVisible()
  await capture(page, "security-group-rules")
  await open(page, `${project}/volumes`)
  await expect(page.getByText("demo-web-boot", { exact: true })).toBeVisible()
  await expect(page.getByText("demo-data", { exact: true })).toBeVisible()
  await capture(page, "volumes-list")
  await page.getByRole("button", { name: "Create volume", exact: true }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Name", { exact: true }).fill("demo-bulk-data")
  await dialog.getByLabel("Storage type", { exact: true }).click()
  await page.getByRole("option", { name: /Capacity HDD/ }).click()
  await dialog.getByLabel("Server", { exact: true }).click()
  await page.getByRole("option", { name: "demo-web", exact: true }).click()
  await expect(dialog.getByText("HDD — not for databases", { exact: true })).toBeVisible()
  await capture(page, "create-volume")
})
