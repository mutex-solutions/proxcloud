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
