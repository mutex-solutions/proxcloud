// Platform setup page (/setup) against the mock API. Request bodies are read from the recorded mock calls.
import { AxeBuilder } from "@axe-core/playwright"
import { expect, test, type Page } from "@playwright/test"
import { calls, recordCalls } from "./mock-calls.ts"

// A status answer with the given step states (every other step done).
const statusWith = (states: Record<string, string>, items: Record<string, unknown[]> = {}) => ({
  serviceId: "svc-proxmox-01", live: true,
  steps: ["connect", "network", "storage", "templates", "catalog", "golive"].map((id) => ({
    id, state: states[id] ?? "done", summary: `${id}: ${states[id] ?? "done"}`, items: items[id] ?? [],
  })),
})

// The axe check of e2e/a11y.spec.ts, for a state only an override reaches.
async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
    .exclude('button[class*="bg-primary"]')
    .exclude('a[class*="bg-primary"]')
    .analyze()
  expect(results.violations.map((v) => ({ id: v.id, nodes: v.nodes.slice(0, 5).map((n) => n.target.join(" ")) }))).toEqual([])
}

const noProvider = {
  serviceId: "", live: false,
  steps: [
    { id: "connect", state: "todo", summary: "No Proxmox provider is saved", items: [] },
    ...["network", "storage", "templates"].map((id) => ({ id, state: "blocked", summary: "Needs a saved Proxmox provider", items: [] })),
    { id: "catalog", state: "todo", summary: "No flavor in the catalog", items: [] },
    { id: "golive", state: "blocked", summary: "Needs a saved Proxmox provider", items: [] },
  ],
}

async function openStep(page: Page, step: string, overrides: Record<string, unknown> = {}) {
  await page.goto(`/setup?tab=${step}`)
  await expect(page.getByRole("heading", { name: "Platform setup" })).toBeVisible()
  await recordCalls(page, overrides)
  // Read the status again through the recorder, so overrides apply.
  await page.getByRole("button", { name: "Check again" }).click()
  await expect(page.getByRole("button", { name: "Check again" })).toBeEnabled()
}

test("the Dashboard card, the nav link and /onboarding lead to the setup page", async ({ page }) => {
  await page.goto("/dashboard")
  await expect(page.getByText("Go live", { exact: true })).toBeVisible()
  await expect(page.getByRole("link", { name: "Platform setup", exact: true })).toBeVisible()
  await page.getByRole("link", { name: "Open platform setup" }).click()
  await expect(page).toHaveURL(/\/setup$/)
  await page.goto("/onboarding")
  await expect(page).toHaveURL(/\/setup$/)
})

test("Prepare shows the script for the token, GPU and import store", async ({ page }) => {
  await page.goto("/setup")
  const script = page.getByLabel("setup script", { exact: true })
  await expect(script).toContainText("PC_TOKEN='proxcloud@pve!platform'")
  await expect(script).not.toContainText("/mapping/pci")
  await page.getByLabel("The cluster has GPUs to map").click()
  await page.getByLabel("Import store (optional)").fill("nas-templates")
  await expect(script).toContainText("'/mapping/pci'")
  await expect(script).toContainText("'/storage/nas-templates' 'PVEDatastoreAdmin'")
  await page.getByLabel("API token ID").fill("root@pam")
  await expect(page.getByText("tokenId must be user@realm!name")).toBeVisible()
})

test("Connect runs every check, pins the fingerprint on request and saves the provider held back", async ({ page }) => {
  await openStep(page, "connect", { "GET /admin/setup/status": noProvider })
  await page.getByLabel("API URL", { exact: true }).fill("https://192.0.2.11:8006")
  await page.getByLabel(/^Token secret/).fill("0b8e1f6c-secret")
  await page.getByRole("button", { name: "Run the checks" }).click()

  const rows = page.getByRole("list", { name: "Connection checks" })
  await expect(rows).toContainText("certificate signed by unknown authority")
  await expect(page.getByRole("alert").filter({ hasText: "The server presented this certificate" })).toContainText("pvenode cert info")
  await page.getByRole("button", { name: "It matches: pin this fingerprint" }).click()
  await expect(page.getByLabel(/TLS certificate fingerprint/)).toHaveValue(/^3A:9C:51/)
  await page.getByRole("button", { name: "Re-check" }).click()
  await expect(rows).toContainText("The datacenter firewall is on.")
  await expect(rows).toContainText("The pool pc-templates does not exist")

  const tests = await calls(page, "POST", "/admin/service/proxmox/auth")
  expect(tests.every((c) => c.path.includes("checks=all"))).toBe(true)
  expect(tests[0].body).toMatchObject({ apiUrl: "https://192.0.2.11:8006", tokenId: "proxcloud@pve!platform", apiUrls: [] })

  await page.getByRole("button", { name: "Save provider" }).click()
  await expect.poll(() => calls(page, "POST", "/admin/service")).toHaveLength(1)
  const [create] = await calls(page, "POST", "/admin/service")
  expect(create.body.config.attachToNewProjects).toBe(false)
  expect(create.body.config.fingerprint).toMatch(/^3A:9C:51/)
  expect(create.body.secret).toEqual({ tokenSecret: "0b8e1f6c-secret" })
})

test("Connect of a saved provider shows the live checks", async ({ page }) => {
  await page.goto("/setup?tab=connect")
  const items = page.getByRole("list", { name: "Checks of this step" })
  await expect(items).toContainText("Provider Helium Proxmox is saved")
  await expect(items).toContainText("The pool pc-templates does not exist")
  await expect(page.getByRole("tab", { name: /2\. Connect \(Needs attention\)/ })).toBeVisible()
  await expect(page.getByRole("link", { name: "Change the connection on the provider page" })).toHaveAttribute("href", "/system/cloud-providers/svc-proxmox-01")
})

const storedPublic = [{ id: "net-ext-01", name: "Internet", cidr: "203.0.113.0/24", gateway: "203.0.113.1", ranges: ["203.0.113.10-203.0.113.200"] }]

test("Network save sends the whole block and keeps the stored public networks", async ({ page }) => {
  await page.goto("/setup?tab=network")
  await expect(page.getByRole("button", { name: "Save network" })).toBeEnabled()
  await expect(page.getByLabel("Project range (cidrPool)")).toBeDisabled()
  await recordCalls(page)
  await page.getByLabel("Networks tenants must not reach").fill("198.51.100.0/24")
  await page.getByRole("button", { name: "Save network" }).click()

  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-proxmox-01")).toHaveLength(1)
  const [put] = await calls(page, "PUT", "/admin/service/svc-proxmox-01")
  const net = put.body.config.network
  expect(Object.keys(put.body.config)).toEqual(["network"])
  expect(net.publicNetworks).toEqual(storedPublic)
  expect(net).toMatchObject({ defaultZone: "overlay", cidrPool: "10.100.0.0/16", isolation: "shared", blockedCidrs: ["198.51.100.0/24"] })
  expect(net.zones.overlay).toMatchObject({ kind: "evpn", peers: ["10.0.0.1"], exitNodes: ["pve1"] })
})

test("Network adds a public network on a bridge that is not the management bridge", async ({ page }) => {
  await page.goto("/setup?tab=network")
  await expect(page.getByRole("button", { name: "Save network" })).toBeEnabled()
  await recordCalls(page)
  await page.getByRole("combobox", { name: "Bridge" }).click()
  await expect(page.getByRole("option")).toHaveText(["None", "vmbr1"])
  await page.getByRole("option", { name: "vmbr1" }).click()
  await page.getByLabel("Range (CIDR)").fill("198.51.100.0/24")
  await page.getByLabel("Gateway").fill("198.51.100.1")
  await page.getByRole("button", { name: "Save network" }).click()

  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-proxmox-01")).toHaveLength(1)
  const [put] = await calls(page, "PUT", "/admin/service/svc-proxmox-01")
  expect(put.body.config.network.publicNetworks).toEqual([
    ...storedPublic,
    { id: "vmbr1", name: "Internet", cidr: "198.51.100.0/24", gateway: "198.51.100.1" },
  ])
})

test("Storage saves the confirmed proposals with the default and the backup store", async ({ page }) => {
  await page.goto("/setup?tab=storage")
  await expect(page.getByRole("button", { name: "Detect storage" })).toBeVisible()
  await recordCalls(page)
  await page.getByRole("button", { name: "Detect storage" }).click()
  await expect(page.getByLabel("Add profile capacity")).toBeChecked()
  await expect(page.getByLabel("pvesm set command", { exact: true })).toHaveText("pvesm set local --content iso,vztmpl,backup,import")
  await page.getByRole("combobox", { name: "Default profile" }).click()
  await page.getByRole("option", { name: "std" }).click()
  await page.getByRole("combobox", { name: "Backup store (backup content)" }).click()
  await page.getByRole("option", { name: "local" }).click()
  await page.getByRole("button", { name: "Save storage" }).click()

  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-proxmox-01")).toHaveLength(1)
  const [put] = await calls(page, "PUT", "/admin/service/svc-proxmox-01")
  expect(put.body.config).toEqual({
    storageProfiles: {
      capacity: { storages: { "*": "nas-capacity" }, shared: true, liveMigrate: true, label: "Capacity HDD (NFS)" },
      std: { storages: { "*": "local-zfs" }, shared: false, snapshots: true, thin: true, liveMigrate: false, replication: false },
    },
    defaultStorageProfile: "std",
    backupStorage: "local",
  })
})

const templatesTodo = statusWith({ templates: "todo", golive: "blocked" }, {
  templates: [{ id: "images", state: "todo", message: "No OS image is published: tenants cannot create a server" }],
})
const tuOn = { configured: true, login: "proxcloud-ops", scopes: ["read:packages"], vmUpdatesEnabled: true, sandboxMirrorEnabled: false }

test("Templates with the flag off show the installer command", async ({ page }) => {
  await openStep(page, "templates", {
    "GET /admin/setup/status": templatesTodo,
    "GET /admin/template-updates/status": { configured: false, vmUpdatesEnabled: false, sandboxMirrorEnabled: false },
  })
  await expect(page.getByLabel("installer command", { exact: true })).toHaveText("./proxcloud-up.sh install --template-updates")
  await expect(page.getByText("Needs template updates turned on and a GitHub token.")).toBeVisible()
  await expect(page.getByLabel("Personal access token (classic)")).toBeVisible()
})

test("Templates import every picked image on every picked node, then publish all that passed once each", async ({ page }) => {
  await openStep(page, "templates", { "GET /admin/setup/status": templatesTodo, "GET /admin/template-updates/status": tuOn })
  await expect(page.getByRole("checkbox", { name: /Kubernetes|k8s/ })).toHaveCount(0)
  await page.getByRole("checkbox", { name: /Ubuntu 24\.04/ }).click()
  await page.getByRole("checkbox", { name: /Debian 13/ }).click()
  await page.getByRole("checkbox", { name: "pve1" }).click()
  await page.getByRole("checkbox", { name: "pve2" }).click()
  await page.getByRole("combobox", { name: "Template disk storage" }).click()
  await expect(page.getByRole("option")).toHaveText(["local-zfs"])
  await page.getByRole("option", { name: "local-zfs" }).click()
  await expectAccessible(page)
  await page.getByRole("button", { name: "Import 4 templates" }).click()

  await expect.poll(() => calls(page, "POST", "/admin/template-updates/vm")).toHaveLength(4)
  const imports = await calls(page, "POST", "/admin/template-updates/vm")
  for (const c of imports) {
    expect(c.body.confirmDigest).toBe(c.body.digest)
    expect(c.body).toMatchObject({ serviceId: "svc-proxmox-01", importStorage: "nas-templates", diskStorage: "local-zfs" })
  }
  expect(imports.map((c) => `${c.body.package}@${c.body.node}`).sort()).toEqual([
    "proxcloud-vm-debian-13@pve1", "proxcloud-vm-debian-13@pve2", "proxcloud-vm-ubuntu-2404@pve1", "proxcloud-vm-ubuntu-2404@pve2",
  ])
  await expect(page.getByRole("table", { name: "Imported templates" })).toContainText("Importing")

  await page.getByRole("button", { name: "Publish all that passed (2)" }).click()
  const publishPath = /\/admin\/template-updates\/templates\/.+\/publish$/
  await expect(page.getByRole("button", { name: "Publish all that passed (0)" })).toBeDisabled()
  const published = await calls(page, "POST", publishPath)
  expect(published.map((c) => c.path).sort()).toEqual([
    "/admin/template-updates/templates/tpl-debian/publish", "/admin/template-updates/templates/tpl-ubuntu/publish",
  ])
  expect(published.map((c) => c.body.confirmDigest).sort()).toEqual([`sha256:${"a".repeat(64)}`, `sha256:${"b".repeat(64)}`])
})

test("Templates save the smoke-test network", async ({ page }) => {
  await openStep(page, "templates", { "GET /admin/setup/status": templatesTodo, "GET /admin/template-updates/status": tuOn })
  await page.getByLabel("Bridge", { exact: true }).fill("vmbr0")
  await page.getByLabel("VLAN tag").fill("29")
  await page.getByRole("button", { name: "Save smoke-test network" }).click()
  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-proxmox-01/update")).toHaveLength(1)
  const [put] = await calls(page, "PUT", "/admin/service/svc-proxmox-01/update")
  expect(put.body).toEqual({ config: { templateSmoke: { bridge: "vmbr0", vlan: 29 } } })
})

test("Catalog shows the rates before Apply, and Apply sends the rate confirmation", async ({ page }) => {
  await page.goto("/setup?tab=catalog")
  const rates = page.getByRole("table", { name: "Rates" })
  await expect(rates).toContainText("Compute: CPU + RAM components")
  await expect(rates).toContainText("vcpus: 0.008")
  await expect(page.getByText("g1.h200.8 in eu-west-1: no gpu rule")).toBeVisible()
  const apply = page.getByRole("button", { name: "Apply 4 items" })
  await expect(apply).toBeDisabled()
  await recordCalls(page)
  await page.getByText("I reviewed these rates.").click()
  await apply.click()

  await expect.poll(() => calls(page, "POST", "/admin/setup/defaults")).toHaveLength(1)
  const [post] = await calls(page, "POST", "/admin/setup/defaults")
  expect(post.body).toEqual({ serviceId: "svc-proxmox-01", items: ["flavors", "pricePlan", "quota", "region"], confirmRates: true })
  await expect(page.getByRole("list", { name: "Applied" })).toContainText("created")
})

test("Catalog without the price plan needs no rate confirmation", async ({ page }) => {
  await page.goto("/setup?tab=catalog")
  await expect(page.getByRole("table", { name: "Rates" })).toBeVisible()
  await recordCalls(page)
  await page.getByRole("checkbox", { name: "Apply: Public price plan" }).click()
  await expect(page.getByText("I reviewed these rates.")).toHaveCount(0)
  await page.getByRole("button", { name: "Apply 3 items" }).click()
  await expect.poll(() => calls(page, "POST", "/admin/setup/defaults")).toHaveLength(1)
  const [post] = await calls(page, "POST", "/admin/setup/defaults")
  expect(post.body).toEqual({ serviceId: "svc-proxmox-01", items: ["flavors", "quota", "region"], confirmRates: false })
})

const goLiveItems = [
  { id: "attachPolicy", state: "todo", message: "New projects are not attached to the provider yet (attach policy off)" },
  { id: "catalog", state: "attention", message: "Catalog and pricing is not complete: usage that matches no rule is billed as zero" },
  { id: "scheduler", state: "attention", message: "The job scheduler is off (PROXCLOUD_JOBS_SCHEDULER_ENABLED)" },
]

test("Go live waits for steps 2 to 5, warns without blocking when the scheduler is off, then attaches and syncs", async ({ page }) => {
  await page.goto("/setup?tab=golive")
  const goLive = page.getByRole("button", { name: "Go live", exact: true })
  // The mock install has storage open.
  await expect(page.getByText("Finish first: 4. Storage.")).toBeVisible()
  await expect(goLive).toBeDisabled()

  // A step the cluster could not answer for keeps it disabled too.
  await recordCalls(page, { "GET /admin/setup/status": statusWith({ templates: "unknown", golive: "unknown" }) })
  await page.getByRole("button", { name: "Check again" }).click()
  await expect(page.getByText("Some steps could not be checked: use Check again.")).toBeVisible()
  await expect(goLive).toBeDisabled()

  await recordCalls(page, { "GET /admin/setup/status": statusWith({ connect: "attention", catalog: "todo", golive: "todo" }, { golive: goLiveItems }) })
  await page.getByRole("button", { name: "Check again" }).click()
  await expect(page.getByText("Finish the catalog, or go live without it.")).toBeVisible()
  await expect(goLive).toBeDisabled()
  await page.getByText("Go live without a complete catalog").click()
  await expect(goLive).toBeEnabled()
  await expect(page.getByLabel("scheduler command", { exact: true })).toHaveText("./proxcloud-up.sh install --scheduler")
  await expectAccessible(page)

  await goLive.click()
  await expect(page.getByRole("list", { name: "Go live results" })).toContainText("Existing projects")
  const all = await page.evaluate(() => (window as unknown as { __calls: { method: string; path: string; body?: any }[] }).__calls)
  const writes = all.filter((c) => c.method !== "GET").map((c) => `${c.method} ${c.path}`)
  expect(writes.slice(0, 3)).toEqual([
    "PUT /admin/service/svc-proxmox-01/attach-policy",
    "POST /admin/job/external-service/sync",
    "POST /admin/service/svc-proxmox-01/bulk-attach/preview",
  ])
  expect(all.find((c) => c.path.endsWith("/attach-policy"))?.body).toEqual({ attachToNewProjects: true })
  expect(all.find((c) => c.path.endsWith("/bulk-attach/preview"))?.body).toEqual({ scope: { mode: "all" } })
})

test("Next is a static card that links ADR 0009", async ({ page }) => {
  await page.goto("/setup?tab=next")
  await expect(page.getByRole("link", { name: /ADR 0009/ })).toHaveAttribute("href", /docs\/adr\/0009-managed-services-platform-lifecycle\.md$/)
  await expect(page.getByRole("button", { name: /Enable managed services/ })).toBeDisabled()
})
