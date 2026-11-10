import { expect, test, type Page } from "@playwright/test"

// Real current components, isolated synthetic in-memory state, no backend/cloud calls.
// Never point this config at an existing signed-in application.
const origin = "http://127.0.0.1:5274"
const externalRequests: string[] = []
const errors: string[] = []

async function seed(page: Page) {
  await page.evaluate(async () => {
    // Vite serves this existing mock-only module; production code is unchanged.
    const { db } = await import(/* @vite-ignore */ "/src/mocks/db.ts")
    const { setMockHandler } = await import(/* @vite-ignore */ "/src/lib/api.ts")
    const { dispatch } = await import(/* @vite-ignore */ "/src/mocks/router.ts")
    // Read-only capture: a future accidental Save/Approve click fails instead of
    // silently exercising a stateful mock mutation.
    setMockHandler(async (path: string, opts: { method?: string; body?: unknown; form?: unknown; rawBody?: unknown }) => {
      const method = (opts.method ?? (opts.body !== undefined || opts.form || opts.rawBody !== undefined ? "POST" : "GET")).toUpperCase()
      if (method !== "GET") {
        console.error(`[docs-capture] refused mock mutation: ${method} ${path}`)
        throw new Error(`Documentation capture refuses mock mutation: ${method} ${path}`)
      }
      return dispatch(path, opts)
    })
    const provider = db.services.find((row: { id: string }) => row.id === "svc-proxmox-01")!
    provider.name = "Example Proxmox"
    provider.status = "PUBLIC"
    provider.config = {
      provider: "proxmox", region: "pve", apiUrl: "https://pve.example.invalid:8006",
      tokenId: "proxcloud@pve!platform", poolPrefix: "pc-", insecureTls: false,
      regions: { pve: { displayName: "Example region" } },
      services: { compute: { pve: true }, image: { pve: true }, network: { pve: true }, volumev3: { pve: false } },
      capabilities: { detectedAt: "2026-11-10T08:00:00Z", storages: {
        "local-zfs": { type: "zfspool", tier: "standard", class: "performance", content: ["images", "rootdir"], snapshots: true, thin: true, replication: true, nodes: { "pve-demo-1": { active: true }, "pve-demo-2": { active: true } } },
        "shared-images": { type: "nfs", tier: "standard", class: "capacity", content: ["images", "iso", "import"], shared: true, thin: true, liveMigrate: true, nodes: { "pve-demo-1": { active: true }, "pve-demo-2": { active: true } } },
        "backup-store": { type: "nfs", tier: "standard", class: "capacity", content: ["backup"], shared: true, thin: true, nodes: { "pve-demo-1": { active: true }, "pve-demo-2": { active: true } } },
      } },
      storageClasses: { "local-zfs": { suggestedClass: "performance" }, "shared-images": { suggestedClass: "capacity" } },
      storageProfiles: { standard: { storages: { "*": "local-zfs" }, snapshots: true, thin: true } },
      network: { defaultZone: "example-zone", cidrPool: "192.0.2.0/24", isolation: "shared" },
      features: { components: [], enabledConsoleTypes: ["NOVNC"] },
    }
    db.services = [provider]
    db.pricePlans = [{ id: "pp-0001", name: "Example public rate card", enabled: true, accessMode: "PUBLIC", createdAt: "2026-11-01T09:00:00Z", serviceProviders: [{ serviceId: provider.id }] }]
    db.pricePlanRules = [
      { id: "rule-demo-cpu", pricePlanId: "pp-0001", name: "Compute: vCPU and RAM", resourceType: "instance", timeUnit: "hour", applyMethod: "ADD_TO_TOTAL", prices: [{ attributeName: "vcpus", tiers: [{ from: 0, value: "0.01" }] }, { attributeName: "ram_gb", tiers: [{ from: 0, value: "0.005" }] }], filters: [], modifiers: [] },
      { id: "rule-demo-storage", pricePlanId: "pp-0001", name: "Volume capacity", resourceType: "volume", timeUnit: "month", applyMethod: "ADD_TO_TOTAL", prices: [{ attributeName: "size_gb", tiers: [{ from: 0, value: "0.08" }] }], filters: [], modifiers: [] },
      { id: "rule-demo-ip", pricePlanId: "pp-0001", name: "Public IPv4", resourceType: "floating_ip", timeUnit: "hour", applyMethod: "ADD_TO_TOTAL", prices: [{ attributeName: "count", tiers: [{ from: 0, value: "0.003" }] }], filters: [], modifiers: [] },
    ]
    db.resourceTypes = [
      { resourceType: "instance", attributes: [{ name: "vcpus", type: "number" }, { name: "ram_gb", type: "number" }, { name: "gpu_count", type: "number" }, { name: "gpu_model", type: "string" }] },
      { resourceType: "volume", attributes: [{ name: "size_gb", type: "number" }] },
      { resourceType: "floating_ip", attributes: [{ name: "count", type: "number" }] },
    ]
    db.billingConfiguration.name = "Example billing configuration"
    db.billingConfiguration.company = { businessName: "Example Cloud", vatId: "EXAMPLE", financeEmail: "billing@example.invalid" }
    db.billingConfiguration.address = { country: "DE", city: "Example City", address: "1 Example Street", zipCode: "00000" }
    db.billingConfiguration.settings = { timeUnitLimits: { minute: 43200, hour: 720, month: 1 } }
    db.billingConfiguration.invoiceGatewayId = "int-invoice-demo"
    db.integrations = [{ id: "int-invoice-demo", name: "Example invoice gateway", thirdParty: "BUILT_IN_INVOICING" }, { id: "int-mail-demo", name: "Example mail gateway", thirdParty: "SMTP" }]
    db.billingConfiguration.mailGatewayId = "int-mail-demo"
    const bp = db.billingProfiles.find((row: { id: string }) => row.id === "bp-0001")!
    bp.fullName = "Example customer"; bp.firstName = "Example"; bp.lastName = "Customer"; bp.email = "customer@example.invalid"; bp.company = { businessName: "Example customer" }; bp.status = "NEW"
    db.validations = [{ id: "validation-demo-01", billingProfileId: bp.id, status: "PENDING", createdAt: "2026-11-10T07:30:00Z", billingProfile: { ...bp } }]
    db.taxes = [{ id: "tax-demo", name: "Example VAT", country: "DE", level: "ALL", accessMode: "PUBLIC", rateLevels: [{ level: 1, percentage: 19 }] }]
    db.savingsPlans = []
  })
}

async function ready(page: Page) {
  await page.evaluate(() => document.fonts.ready)
  await expect(page.locator("h1")).toBeVisible()
  await expect(page.locator("body")).not.toContainText("Sign in to continue")
}

async function open(page: Page, route: string, heading: string) {
  // SPA navigation preserves the isolated in-memory fixtures (reload resets them).
  await page.evaluate((path) => { history.pushState({}, "", path); dispatchEvent(new PopStateEvent("popstate")) }, route)
  await expect(page.getByRole("heading", { name: heading, exact: true })).toBeVisible()
  await ready(page)
}

async function shot(page: Page, filename: string) {
  await page.evaluate(() => document.fonts.ready)
  await page.mouse.move(0, 0)
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(1)
  await page.screenshot({ path: `public/docs-img/${filename}.png`, animations: "disabled", caret: "hide" })
}

test.beforeEach(async ({ page }) => {
  externalRequests.length = 0; errors.length = 0
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url())
    if (url.origin === origin && !url.pathname.startsWith("/api/") && !url.pathname.startsWith("/admin-api/")) return route.continue()
    externalRequests.push(url.origin + url.pathname)
    await route.abort("blockedbyclient")
  })
  await page.addInitScript(() => localStorage.setItem("proxcloud.theme", "light"))
  await page.clock.setFixedTime(new Date("2026-11-10T09:00:00Z"))
  page.on("pageerror", (error) => errors.push(error.stack ?? error.message))
  page.on("console", (msg) => { if (msg.type() === "error" || (msg.type() === "warning" && msg.text().includes("[mock]"))) errors.push(msg.text()) })
  await page.goto("/system/cloud-providers")
  await expect(page.getByRole("heading", { name: "Cloud providers", exact: true })).toBeVisible()
  await seed(page)
})

test.afterEach(async () => {
  expect(externalRequests, "No network request may leave the local mock origin").toEqual([])
  expect(errors, "No page/console errors or unhandled mock requests").toEqual([])
})

test("recapture the four published operator illustrations", async ({ page }) => {
  // Refresh the active query after fixture injection by navigating away and back.
  await open(page, "/system/price-plans", "Price plans")
  await expect(page.getByText("Example public rate card", { exact: true })).toBeVisible()
  await open(page, "/system/cloud-providers", "Cloud providers")
  await page.getByRole("button", { name: "Add provider", exact: true }).click()
  await expect(page.getByRole("dialog")).toBeVisible()
  await page.getByLabel("Display name", { exact: true }).fill("Example Proxmox")
  await page.getByLabel("API URL", { exact: true }).fill("https://pve.example.invalid:8006")
  await page.getByLabel("API token ID", { exact: true }).fill("proxcloud@pve!platform")
  await expect(page.getByLabel("Token secret", { exact: true })).toHaveValue("")
  await shot(page, "add-proxmox-provider")
  await page.keyboard.press("Escape")

  await open(page, "/system/cloud-providers/svc-proxmox-01", "Example Proxmox")
  await page.getByRole("tab", { name: "Storage", exact: true }).click()
  await expect(page.getByRole("cell", { name: /shared-images/ })).toBeVisible()
  await shot(page, "provider-storage")

  await open(page, "/system/price-plans", "Price plans")
  await page.getByRole("button", { name: "Create price plan", exact: true }).click()
  await expect(page.getByRole("dialog")).toBeVisible()
  await page.getByLabel("Name", { exact: true }).fill("Example standard pricing")
  await shot(page, "price-plans-create")
  await page.keyboard.press("Escape")
  await open(page, "/system/price-plans/pp-0001", "Example public rate card")
  await expect(page.getByRole("cell", { name: "Compute: vCPU and RAM", exact: true })).toBeVisible()
  await shot(page, "price-plan-rules")
})

test("capture current billing, savings and service controls", async ({ page }) => {
  await open(page, "/system/billing-configuration", "Billing configuration")
  await page.getByRole("tab", { name: "Activation", exact: true }).click()
  await expect(page.getByLabel("Auto-activation enabled")).toBeVisible()
  await shot(page, "billing-configuration-activation")

  await page.getByRole("tab", { name: "Business details", exact: true }).click()
  await page.getByLabel("Base currency", { exact: true }).click()
  await expect(page.getByRole("option", { name: /EUR/ })).toBeVisible()
  await shot(page, "billing-configuration-base-currency")
  await page.keyboard.press("Escape")
  await page.getByLabel("Invoice gateway", { exact: true }).scrollIntoViewIfNeeded()
  await shot(page, "billing-configuration-invoice-gateway")

  await page.getByRole("tab", { name: "Settings", exact: true }).click()
  await page.getByText("Automatic suspension", { exact: true }).scrollIntoViewIfNeeded()
  await shot(page, "suspension-balance")
  await page.getByRole("combobox").filter({ hasText: "Balance" }).click()
  await page.getByRole("option", { name: /due date/i }).click()
  // Current UI displays both units: clear the inactive balance inputs and fill
  // days through real controls so the illustration is not a contradictory policy.
  const settings = page.getByRole("tabpanel", { name: "Settings", exact: true })
  const suspensionInputs = settings.locator("input[type=number]")
  await suspensionInputs.nth(0).fill("")
  await suspensionInputs.nth(1).fill("10")
  await suspensionInputs.nth(2).fill("")
  await suspensionInputs.nth(3).fill("3")
  await suspensionInputs.nth(4).fill("")
  await suspensionInputs.nth(5).fill("7")
  await expect(suspensionInputs.nth(1)).toHaveValue("10")
  await shot(page, "suspension-due-date")

  await open(page, "/clients/validations", "Validations")
  await expect(page.getByText("customer@example.invalid", { exact: true })).toBeVisible()
  await shot(page, "validations-queue")

  await open(page, "/system/price-plans/pp-0001", "Example public rate card")
  await page.getByRole("button", { name: "Add rule", exact: true }).first().click()
  await expect(page.getByRole("dialog", { name: "Add rule", exact: true })).toBeVisible()
  await page.getByLabel("Name", { exact: true }).fill("Example CPU price")
  await page.getByLabel("Resource type", { exact: true }).click()
  await page.getByRole("option", { name: "instance", exact: true }).click()
  await page.getByLabel("Priced attribute", { exact: true }).click()
  await expect(page.getByRole("option", { name: "vcpus (number)", exact: true })).toBeVisible()
  await shot(page, "price-plan-rule-create")
  await page.keyboard.press("Escape"); await page.keyboard.press("Escape")

  await open(page, "/system/taxes", "Taxes")
  await page.getByRole("button", { name: "Create tax rate", exact: true }).click()
  await page.getByLabel("Name", { exact: true }).fill("Example VAT")
  await page.getByLabel("Percentage", { exact: true }).fill("19")
  await page.getByLabel("Country (optional)", { exact: true }).fill("DE")
  await shot(page, "taxes-add-tax")
  await page.keyboard.press("Escape")

  await open(page, "/system/savings-plans", "Savings plans")
  await page.getByRole("button", { name: "Create savings plan", exact: true }).click()
  await page.getByLabel("Name", { exact: true }).fill("Example compute savings")
  await page.getByLabel("Description (optional)").fill("Synthetic example — not a live offer")
  await page.getByText("instance", { exact: true }).click()
  await page.getByLabel("Tier 1 start amount", { exact: true }).fill("100")
  await page.getByLabel("Tier 1 discount percent", { exact: true }).fill("5")
  await shot(page, "savings-plan-targets")
  await page.getByRole("button", { name: "Add tier", exact: true }).click()
  await page.getByLabel("Tier 2 start amount", { exact: true }).fill("1000")
  await page.getByLabel("Tier 2 discount percent", { exact: true }).fill("10")
  await page.getByText("Discount tiers — committed amount from, discount %", { exact: true }).scrollIntoViewIfNeeded()
  await shot(page, "savings-plan-discount-tiers")
  await page.keyboard.press("Escape")

  await open(page, "/system/cloud-providers/svc-proxmox-01", "Example Proxmox")
  await page.getByRole("tab", { name: "Services", exact: true }).click()
  await expect(page.getByRole("switch", { name: "compute in pve", exact: true })).toBeVisible()
  await shot(page, "enable-disable-services-tab")
})
