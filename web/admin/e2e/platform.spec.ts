// Managed services page (/platform) against the mock API (src/mocks/handlers/platform.ts). The mock's first apply run
// fails at seed-api and later ones succeed; ?mock-platform=<state> starts the page in one state. A stage takes
// 1.5 seconds in the mock, so the tests that follow a run allow for that.
import { readFile } from "node:fs/promises"
import { AxeBuilder } from "@axe-core/playwright"
import { expect, test, type Page } from "@playwright/test"
import { calls, recordCalls } from "./mock-calls.ts"

const CAPI_SECRET = "11111111-2222-3333-4444-555555555555"
const CSI_SECRET = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

async function expectAccessible(page: Page) {
  await expect(page.locator("[data-sonner-toast]")).toHaveCount(0, { timeout: 10_000 }) // a toast is not part of the page
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
    .exclude('button[class*="bg-primary"]')
    .exclude('a[class*="bg-primary"]')
    .analyze()
  expect(results.violations.map((v) => ({ id: v.id, nodes: v.nodes.slice(0, 5).map((n) => n.target.join(" ")) }))).toEqual([])
}

const shot = (page: Page, name: string) => page.screenshot({ path: `e2e/shots/platform-${name}.png`, fullPage: true, animations: "disabled" })

async function open(page: Page, preset = "") {
  await page.emulateMedia({ reducedMotion: "reduce" })
  await page.goto(`/platform${preset ? `?mock-platform=${preset}` : ""}`)
  await expect(page.getByRole("heading", { name: "Managed services", exact: true })).toBeVisible()
}

async function fillSettings(page: Page) {
  const fill = (label: string, value: string) => page.getByLabel(label, { exact: true }).fill(value)
  await fill("Name", "lab")
  for (const g of ["system", "kamaji", "dbaas"]) await page.getByRole("checkbox", { name: `Group ${g}: node pve1` }).check()
  await page.getByRole("button", { name: "Use the lab profile" }).click()
  await fill("Bridge", "vmbr1")
  await fill("Network", "10.90.0.0/24")
  await fill("Gateway", "10.90.0.1")
  await fill("DNS servers", "10.90.0.1")
  await fill("API address", "10.90.0.10")
  await fill("Node range", "10.90.0.20-10.90.0.60")
  await fill("Load balancer range", "10.90.0.100-10.90.0.150")
  await fill("Proxmox API URL", "https://10.90.0.2:8006")
  await fill("Seed node", "pve1")
  await fill("Seed address", "10.90.0.5")
  await fill("Seed API sources", "192.0.2.10/32")
  await fill("Control plane node", "pve1")
  await fill("Boot store", "local-zfs")
}

test("with the switch off the page is blocked and names the install switch", async ({ page }) => {
  await open(page, "disabled")
  await expect(page.getByText("Managed services are off on this install")).toBeVisible()
  await expect(page.getByLabel("install command", { exact: true })).toHaveText("./proxcloud-up.sh install --managed-services")
  await expect(page.getByRole("button", { name: /Save draft|Install/ })).toHaveCount(0)
  await expectAccessible(page)
  await shot(page, "disabled")
})

test("the nav links to the page for an admin who reads providers", async ({ page }) => {
  await page.goto("/dashboard")
  await page.getByRole("link", { name: "Managed services", exact: true }).click()
  await expect(page).toHaveURL(/\/platform$/)
  await expect(page.getByRole("list", { name: "Prerequisites" })).toContainText("Managed services are on.")
  await expect(page.getByRole("list", { name: "Prerequisites" })).toContainText("The job scheduler is off")
})

test("from an empty form to a failed run, then Retry and Cancel", async ({ page }) => {
  test.setTimeout(90_000)
  await open(page)
  await recordCalls(page)

  // The form mirrors the API's rules: nothing is sent while a setting is wrong.
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByRole("alert").filter({ hasText: "settings need correcting" })).toBeVisible()
  await expect(page.getByLabel("Name", { exact: true })).toHaveAttribute("aria-invalid", "true")
  expect(await calls(page, "POST", "/admin/platform/installations")).toHaveLength(0)
  await expect(page.getByRole("radio", { name: /Separate clusters/ })).toBeDisabled()

  await fillSettings(page)
  await page.getByLabel("Gateway", { exact: true }).fill("10.90.0.30")
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByText("Gateway is inside the node range.").first()).toBeVisible()
  await page.getByLabel("Node range", { exact: true }).fill("10.90.0.20-10.90.0.24")
  await expect(page.getByText(/Node range: it holds 5 addresses; 5 machines and 3 spare for updates need 8/).first()).toBeVisible()
  await page.getByLabel("Gateway", { exact: true }).fill("10.100.0.1")
  await page.getByLabel("Network", { exact: true }).fill("10.100.0.0/24")
  await expect(page.getByText(/API address: a host address inside 10.100.0.0\/24/).first()).toBeVisible()

  // What only the API knows comes back as its problems.
  for (const [label, v] of [["API address", "10.100.0.10"], ["Node range", "10.100.0.20-10.100.0.60"], ["Load balancer range", "10.100.0.100-10.100.0.150"], ["Seed address", "10.100.0.5"]]) {
    await page.getByLabel(label, { exact: true }).fill(v)
  }
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByRole("alert").filter({ hasText: "The settings were not saved" })).toContainText("overlaps cidrPool 10.100.0.0/16")

  await fillSettings(page)
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByText("2. Proxmox identities")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect(created.body).toMatchObject({ name: "lab", serviceId: "svc-proxmox-01", layout: "shared" })
  expect(created.body.spec.seed).toMatchObject({ node: "pve1", cores: 2, memoryMiB: 4096, apiSources: ["192.0.2.10/32"] })
  expect(created.body.spec.clusters[0].nodeGroups.map((g: { name: string; count: number }) => [g.name, g.count])).toEqual([["system", 1], ["kamaji", 1], ["dbaas", 1]])
  await shot(page, "draft")

  // The script is the managed one for the saved stores.
  await expect(page.getByLabel("identities script", { exact: true })).toContainText("pveum user token add 'capmox@pve' 'capi'")
  expect((await calls(page, "GET", "/admin/setup/proxmox-script")).at(-1)!.path).toMatch(/managedServices=true&bootStorage=local-zfs&isoStorage=local/)

  // Check fails until both secrets are stored, and says what to do.
  await page.getByRole("button", { name: "Check", exact: true }).click()
  await expect(page.getByRole("status").filter({ hasText: "checks failed" })).toBeVisible()
  await expect(page.getByRole("list", { name: "What to do for each failed check" })).toContainText("Paste both token secrets in step 2.")
  await expectAccessible(page)
  await shot(page, "check-failed")

  // The two secrets are write-only: sent once, then gone from the page.
  const capi = page.getByLabel("Secret of capmox@pve!capi")
  await expect(capi).toHaveAttribute("type", "password")
  await capi.fill("not-a-secret")
  await expect(page.getByText(/Not a Proxmox token secret/)).toBeVisible()
  await expect(page.getByRole("button", { name: "Store the secrets" })).toBeDisabled()
  await capi.fill(CAPI_SECRET)
  await page.getByLabel("Secret of csi@pve!csi").fill(CSI_SECRET)
  await page.getByRole("button", { name: "Store the secrets" }).click()
  await expect(page.getByText("Stored. It is never shown again; paste a new one to replace it.")).toHaveCount(2)
  await expect(capi).toHaveValue("")
  await expect(page.getByLabel("Secret of csi@pve!csi")).toHaveValue("")
  const [stored] = await calls(page, "PUT", /\/credentials$/)
  expect(stored.body).toEqual({ capi: { tokenId: "capmox@pve!capi", secret: CAPI_SECRET }, csi: { tokenId: "csi@pve!csi", secret: CSI_SECRET } })
  expect(await page.content()).not.toContain(CAPI_SECRET)
  expect(await page.content()).not.toContain(CSI_SECRET)
  expect(await page.evaluate(() => JSON.stringify([{ ...localStorage }, { ...sessionStorage }]))).not.toContain(CAPI_SECRET)

  await page.getByRole("button", { name: "Check again" }).click()
  await expect(page.getByRole("status").filter({ hasText: "Every check passed." })).toBeVisible()

  // Review, then Install: the first run of the mock fails at the seed's API.
  await expect(page.getByText("1 control plane and 3 worker machines in the pool")).toBeVisible()
  await expect(page.getByRole("list", { name: "Cores and memory per node" })).toContainText("pve1: 10 cores and 20 GiB of memory")
  await page.getByRole("button", { name: "Install", exact: true }).click()
  const stages = page.getByRole("list", { name: "Stages" })
  await expect(stages.locator('[data-stage="validate"]')).toContainText("Running")
  await expect(page.getByRole("button", { name: "Save settings" })).toHaveCount(0) // no edits during a run
  await expect(stages.locator('[data-stage="seed-api"]')).toContainText("Failed", { timeout: 15_000 })
  await expect(stages.locator('[data-stage="validate"]')).toContainText("Done")
  await expect(stages.locator('[data-stage="seed-api"]')).toContainText("seed_api_unreachable")
  await expect(stages.locator('[data-stage="seed-api"]')).toContainText("Add a route from the ProxCloud host to the platform network")
  await expect(stages.locator('[data-stage="seed-identity"]')).toContainText("Not started")
  await expect(page.getByText("Nothing is deleted when a run fails.")).toBeVisible()
  await expectAccessible(page)
  await shot(page, "run-failed")

  // Retry starts a new apply; Cancel ends it cancelled and offers Retry again.
  await page.getByRole("button", { name: "Retry" }).click()
  await expect(stages.getByText("Running")).toBeVisible()
  await shot(page, "run-progress")
  await page.getByRole("button", { name: "Cancel the run" }).click()
  await expect(page.getByText("The run was cancelled. Nothing was deleted.")).toBeVisible({ timeout: 15_000 })
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible()
  expect((await calls(page, "POST", /\/runs$/)).map((c) => c.body)).toEqual([{ kind: "apply" }, { kind: "apply" }])
  expect(await calls(page, "POST", /\/runs\/[^/]+\/cancel$/)).toHaveLength(1)
})

test("a run ends ready: live components, providers, seed credential and the break-glass download", async ({ page }) => {
  test.setTimeout(60_000)
  await open(page, "running")
  await expect(page.getByRole("list", { name: "Stages" }).locator('[data-stage="cluster"]')).toContainText("machines 3/4 running, control plane 1/1")
  await expect(page.getByText("Managed services “lab”", { exact: true })).toBeVisible({ timeout: 20_000 })
  await recordCalls(page)

  const components = page.getByRole("table", { name: "Components and their live state" })
  await expect(components.getByRole("row").filter({ hasText: "kamaji" })).toContainText("Synced")
  await expect(components.getByRole("row").filter({ hasText: "kamaji" })).toContainText("Healthy")
  await expect(page.getByRole("link", { name: "lab Kubernetes" })).toHaveAttribute("href", /\/system\/cloud-providers\/svc-/)
  await expect(page.getByRole("link", { name: "lab Databases" })).toBeVisible()
  await expect(page.getByLabel("backup command", { exact: true })).toContainText("pvesh create /cluster/backup --vmid 9100")
  await expect(page.getByText("Tenant databases are not backed up.")).toBeVisible()
  await expect(page.getByText("Tenant Kubernetes clusters get no block volumes on a one-host install.")).toBeVisible()

  // Fields the first install fixed are read-only; the others still save.
  await expect(page.getByLabel("Name", { exact: true })).toBeDisabled()
  await expect(page.getByLabel("Network", { exact: true })).toBeDisabled()
  await expect(page.getByLabel("Seed address", { exact: true })).toBeDisabled()
  await expect(page.getByLabel("Node range", { exact: true })).toBeEnabled()
  await expect(page.getByRole("button", { name: "Apply this release" })).toBeEnabled()
  await expectAccessible(page)
  await shot(page, "ready")

  await page.getByRole("button", { name: "Replace the seed credential" }).click()
  await expect(page.getByText(/ProxCloud now uses a new seed credential/)).toBeVisible()
  expect(await calls(page, "POST", /\/seed-credential\/rotate$/)).toHaveLength(1)

  // The kubeconfig is a file: it is never in the page.
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download seed kubeconfig" }).click()])
  expect(download.suggestedFilename()).toBe("lab-seed.kubeconfig")
  expect(await readFile(await download.path(), "utf8")).toContain("token: mock-break-glass-token")
  await expect(page.getByText(/Downloaded\. It works until/)).toBeVisible()
  expect(await page.content()).not.toContain("mock-break-glass-token")
})

test("Remove asks for the typed name and sends the confirmation only for the forced path", async ({ page }) => {
  test.setTimeout(60_000)
  await open(page, "ready")
  await recordCalls(page)
  await page.getByRole("button", { name: "Remove managed services" }).click()
  const dialog = page.getByRole("dialog")
  const remove = dialog.getByRole("button", { name: "Remove", exact: true })
  await expect(remove).toBeDisabled()
  await dialog.getByLabel(/Type remove lab to confirm/).fill("remove la")
  await expect(remove).toBeDisabled()
  await dialog.getByLabel(/Type remove lab to confirm/).fill("remove lab")
  await expect(remove).toBeEnabled()
  await expectAccessible(page)
  await shot(page, "remove-dialog")
  await remove.click()
  await expect(dialog).toHaveCount(0)
  expect((await calls(page, "POST", /\/runs$/)).map((c) => c.body)).toEqual([{ kind: "remove" }])

  const stages = page.getByRole("list", { name: "Stages" })
  await expect(page.getByText("Removal", { exact: true })).toBeVisible()
  await expect(stages.locator('[data-stage="remove-cluster"]')).toContainText(/Running|Done/)
  await expect(page.getByRole("button", { name: "Remove managed services" })).toBeDisabled()
  await shot(page, "removing")
  await expect(page.getByText("Managed services “lab” were removed")).toBeVisible({ timeout: 20_000 })
  await expect(page.getByLabel("token removal commands", { exact: true })).toContainText("pveum user token remove capmox@pve capi")
  await expect(page.getByLabel("token removal commands", { exact: true })).toContainText("pveum user token remove csi@pve csi")
  await expect(page.getByRole("button", { name: "Remove managed services" })).toHaveCount(0)
  await shot(page, "removed")
})

test("the forced removal carries the typed confirmation", async ({ page }) => {
  await open(page, "ready")
  await recordCalls(page)
  await page.getByRole("button", { name: "Remove managed services" }).click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel(/Type remove lab to confirm/).fill("remove lab")
  await dialog.getByRole("checkbox", { name: /Go on even when the seed VM is gone/ }).check()
  await dialog.getByRole("button", { name: "Remove", exact: true }).click()
  await expect(dialog).toHaveCount(0)
  expect((await calls(page, "POST", /\/runs$/)).map((c) => c.body)).toEqual([{ kind: "remove", confirm: "remove lab" }])
})

test("a removal in progress and a failed run read from a fresh page load", async ({ page }) => {
  await open(page, "removing")
  await expect(page.getByText("Removal", { exact: true })).toBeVisible()
  await expect(page.getByRole("button", { name: "Cancel the run" })).toBeEnabled()
  await expectAccessible(page)

  await open(page, "failed")
  await expect(page.getByRole("group", { name: "Failure" })).toContainText("seed_api_unreachable")
  await expect(page.getByRole("button", { name: "Retry" })).toBeEnabled()
  await expect(page.getByRole("button", { name: "Save settings" })).toBeEnabled() // a draft that never succeeded can be corrected
  await expect(page.getByLabel("Name", { exact: true })).toBeEnabled()
})

test("an admin without admin:service:manage reads the page and changes nothing", async ({ page }) => {
  await page.route(/\/src\/mocks\/fixtures\/dashboard\.ts/, async (route) => {
    const resp = await route.fetch()
    await route.fulfill({ response: resp, body: (await resp.text()).replace('permissions: ["admin:*"]', 'permissions: ["admin:service:read"]') })
  })
  await open(page, "ready")
  await expect(page.getByText("Changing anything needs the permission admin:service:manage.")).toBeVisible()
  for (const name of ["Save settings", "Store the secrets", "Check", "Apply this release", "Download seed kubeconfig", "Replace the seed credential", "Remove managed services"]) {
    await expect(page.getByRole("button", { name, exact: true })).toBeDisabled()
  }
})
