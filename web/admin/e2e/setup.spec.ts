// Platform setup page (/setup) against the mock API. Request bodies are read from the recorded mock calls.
import { expect, test, type Page } from "@playwright/test"
import { calls, recordCalls } from "./mock-calls.ts"

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
