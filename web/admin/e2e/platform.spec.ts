// Managed services page (/platform) against the mock API (src/mocks/handlers/platform.ts). The mock's first apply run
// fails at seed-api and later ones succeed; ?mock-platform=<state> starts the page in one state. A stage takes
// 1.5 seconds in the mock, so the tests that follow a run allow for that.
import { readFile } from "node:fs/promises"
import { AxeBuilder } from "@axe-core/playwright"
import { expect, test, type Page } from "@playwright/test"
import { calls, recordCalls } from "./mock-calls.ts"
import { themes } from "./routes.ts"

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

// The state in the page header, as its attribute and as its word.
const headerState = (page: Page) => page.locator("[data-platform-state]")

// mock runs fn against the mock's own state (src/mocks/handlers/platform.ts), as another session or the server would change it.
const MOCK = { state: "/src/mocks/handlers/platform.ts", router: "/src/mocks/router.ts" } // paths the dev server serves
async function mock(page: Page, body: string) {
  await page.evaluate(async ([src, paths]) => {
    const { mockPlatform } = await import(/* @vite-ignore */ paths.state)
    const { dispatch } = await import(/* @vite-ignore */ paths.router)
    await new Function("mockPlatform", "dispatch", `return (async () => { ${src} })()`)(mockPlatform, dispatch)
  }, [body, MOCK] as const)
}

async function open(page: Page, preset = "", theme = "") {
  if (theme) await page.addInitScript((t) => localStorage.setItem("proxcloud.theme", t), theme)
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
  await page.getByLabel("Import store (optional)", { exact: true }).fill("local-zfs") // the boot store
  await expect(page.getByText("Import store: a store other than the boot store.").first()).toBeVisible()
  await page.getByLabel("Import store (optional)", { exact: true }).fill("")
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
  expect((await calls(page, "GET", "/admin/setup/proxmox-script")).at(-1)!.path).toMatch(/importStorage=/) // the API filled the provider's import store

  // Check fails until both secrets are stored, and says what to do.
  await page.getByRole("button", { name: "Check", exact: true }).click()
  await expect(page.getByRole("status").filter({ hasText: "checks failed" })).toBeVisible()
  await expect(page.getByRole("list", { name: "What to do for each failed check" })).toContainText("Paste both token secrets in step 2.")
  await expectAccessible(page)
  await shot(page, "check-failed")

  // The two secrets are write-only: sent once, then gone from the page.
  const capi = page.getByLabel("Secret of capmox@pve!capi")
  await expect(capi).toHaveAttribute("type", "password")
  await expect(capi).toHaveAttribute("autocomplete", "new-password") // not a login: no password manager offer
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
  await expect(page.locator(":focus")).toHaveText("Installation run") // the pressed button is gone: focus follows to the run
  await expect(page.getByRole("status").filter({ hasText: /^Stage \d+ of 11: / })).toHaveCount(1)
  await expect(page.getByRole("button", { name: "Save settings" })).toHaveCount(0) // no edits during a run
  await expect(stages.locator('[data-stage="seed-api"]')).toContainText("Failed", { timeout: 15_000 })
  await expect(stages.locator('[data-stage="validate"]')).toContainText("Done")
  await expect(stages.locator('[data-stage="seed-api"]')).toContainText("seed_api_unreachable")
  await expect(stages.locator('[data-stage="seed-api"]')).toContainText("Add a route from the ProxCloud host to the platform network")
  await expect(stages.locator('[data-stage="seed-identity"]')).toContainText("Not started")
  await expect(page.getByText("Nothing is deleted when a run fails.")).toBeVisible()
  await expect(page.getByRole("status").filter({ hasText: "Installation run: failed. Add a route from the ProxCloud host" })).toHaveCount(1)
  await expect(headerState(page)).toHaveText("Failed")
  await expectAccessible(page)
  await shot(page, "run-failed")

  // Retry runs with what is saved, so it waits for unsaved settings; Check and Install do too.
  await page.getByLabel("Seed API sources", { exact: true }).fill("192.0.2.0/24")
  await expect(page.getByRole("button", { name: "Retry" })).toBeDisabled()
  await expect(page.getByText("Save the settings first: Retry runs with what is saved.")).toBeVisible()
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect(page.getByText("Settings saved")).toBeVisible()
  await expect(page.getByRole("button", { name: "Retry" })).toBeEnabled()

  // Retry starts a new apply; Cancel ends it cancelled and offers Retry again.
  await page.getByRole("button", { name: "Retry" }).click()
  await expect(stages.getByText("Running")).toBeVisible()
  await shot(page, "run-progress")
  await expect(page.getByText(/up to 5 minutes while a stage pauses after repeated errors/)).toBeVisible()
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
  await expect(page.getByLabel("backup command", { exact: true })).toContainText("pvesh create /cluster/backup --vmid 9100 --storage <backup store> --schedule daily --mode snapshot --compress zstd --prune-backups keep-daily=7")
  await expect(page.getByText(/a backup store that is not the boot, ISO or import store of the settings/)).toBeVisible()
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

test("the ready card: endpoints, the providers' gap, the platform kubeconfig and the two ArgoCD commands", async ({ page }) => {
  await open(page, "ready")
  await recordCalls(page)

  const endpoints = page.getByRole("list", { name: "Endpoints" })
  await expect(endpoints.getByRole("listitem").filter({ hasText: "Seed API" })).toContainText("https://10.90.0.5:6443")
  await expect(endpoints.getByRole("listitem").filter({ hasText: "Platform cluster API" })).toContainText("https://10.90.0.10:6443")
  await expect(endpoints.getByRole("listitem").filter({ hasText: "Load balancer range (MetalLB)" })).toContainText("10.90.0.100-10.90.0.150")
  await expect(endpoints.getByRole("listitem").filter({ hasText: "Node range" })).toContainText("10.90.0.20-10.90.0.60")

  // A green install leaves the Kubernetes provider with no version and both providers attached to no project: the
  // card says so and links to where each is put right.
  const providers = page.getByRole("list", { name: "Providers" })
  const k8s = providers.getByRole("listitem").filter({ hasText: "lab Kubernetes" })
  await expect(k8s).toContainText("Offers no Kubernetes version yet, so no tenant can create a cluster.")
  await expect(k8s.getByRole("link", { name: "Publish a Kubernetes node image" })).toHaveAttribute("href", "/system/template-updates")
  await expect(k8s.getByRole("link", { name: "enter its version on the provider's Connection tab" })).toHaveAttribute("href", /\/system\/cloud-providers\/svc-/)
  await expect(k8s.getByRole("link", { name: "Attach it on the provider's Projects tab" })).toHaveAttribute("href", /\/system\/cloud-providers\/svc-/)
  const dbs = providers.getByRole("listitem").filter({ hasText: "lab Databases" })
  await expect(dbs).toContainText("New projects do not get this provider")
  await expect(dbs).not.toContainText("Kubernetes version")

  // ArgoCD has no address: the exact commands, one pair for each cluster, each with its own kubeconfig file and port.
  await expect(page.getByLabel("platform ArgoCD commands", { exact: true })).toHaveText(
    "export KUBECONFIG=./lab-platform.kubeconfig\nkubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo\nkubectl -n argocd port-forward svc/argocd-server 8080:80")
  await expect(page.getByLabel("seed ArgoCD commands", { exact: true })).toHaveText(
    "export KUBECONFIG=./lab-seed.kubeconfig\nkubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo\nkubectl -n argocd port-forward svc/argocd-server 8081:80")
  await expectAccessible(page)

  // The platform kubeconfig is a file of its own route, and like the seed's it is never in the page.
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download platform kubeconfig" }).click()])
  expect(download.suggestedFilename()).toBe("lab-platform.kubeconfig")
  const file = await readFile(await download.path(), "utf8")
  expect(file).toContain("token: mock-platform-break-glass-token")
  expect(file).toContain("server: https://10.90.0.10:6443")
  await expect(page.getByText(/Downloaded\. It works until/)).toBeVisible()
  expect(await page.content()).not.toContain("mock-platform-break-glass-token")
  expect(await calls(page, "GET", /\/platform-kubeconfig$/)).toHaveLength(1)
  expect(await calls(page, "GET", /\/seed-kubeconfig$/)).toHaveLength(0)
})

// The providers are read again when a run ends (it registers them), and a provider row the operator completed before
// (a version, the attach policy) is adopted as it is: the card then has nothing to warn about.
test("the ready card stops warning once the Kubernetes provider has a version and is attached", async ({ page }) => {
  test.setTimeout(60_000)
  await open(page, "running")
  await mock(page, `
    const { db } = await import("/src/mocks/db.ts")
    db.services.push({ id: "svc-k8s-kept", name: "lab Kubernetes", type: "CLOUD", status: "ACTIVE", config: {
      provider: "kamaji", platformInstallationId: mockPlatform.installations[0].id, attachToNewProjects: true, cluster: { versions: { "1.35.10": "9000" } } } })`)
  const k8s = page.getByRole("list", { name: "Providers" }).getByRole("listitem").filter({ hasText: "lab Kubernetes" })
  await expect(k8s).toContainText("Offers Kubernetes 1.35.10.", { timeout: 20_000 })
  await expect(k8s).toContainText("New projects get this provider.")
  await expect(k8s).not.toContainText("no Kubernetes version")
  await expect(page.getByRole("list", { name: "Providers" }).getByRole("listitem").filter({ hasText: "lab Databases" })).toContainText("New projects do not get this provider")
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
  await expect(page.locator(":focus")).toHaveText("Removal")
  await shot(page, "removing")
  await expect(page.getByText("Managed services “lab” were removed")).toBeVisible({ timeout: 20_000 })
  await expect(page.getByLabel("token removal commands", { exact: true })).toContainText("pveum user token remove capmox@pve capi")
  await expect(page.getByLabel("token removal commands", { exact: true })).toContainText("pveum user token remove csi@pve csi")
  await expect(page.getByRole("button", { name: "Remove managed services" })).toHaveCount(0)
  // The header follows the stored phase, not the live answer of the ready page; and what was built is forgotten, so
  // the settings are a draft again.
  await expect(headerState(page)).toHaveText("Removed")
  await expect(page.getByLabel("Name", { exact: true })).toBeEnabled()
  await expect(page.getByRole("button", { name: "Install", exact: true })).toBeEnabled()
  await expect(page.getByRole("button", { name: "Apply this release" })).toHaveCount(0)
  await expect(page.getByText(/Changes from the installed release/)).toHaveCount(0)
  await shot(page, "removed")
})

test("a platform installed by the release before stays on it: the review says so and lists its contents", async ({ page }) => {
  await open(page, "previous")
  await expect(page.getByRole("heading", { name: "The platform stays on release 0.6.0" })).toBeVisible()
  await expect(page.getByText(/carries the platform release 0\.7\.0 and does not move this platform to it/)).toBeVisible()
  await expect(page.getByRole("list", { name: "Why the upgrade is refused" })).toContainText("workload/cilium: not supported by this release")
  await expect(page.getByRole("button", { name: /^Upgrade to release/ })).toHaveCount(0)
  await expect(page.getByText(/^Release 0\.6\.0 with Kubernetes/)).toBeVisible()
  await expect(page.getByText("Changes from the installed release 0.6.0")).toBeVisible()
  await expect(page.getByRole("button", { name: "Apply this release" })).toBeEnabled()
  await expectAccessible(page)
})

test("an upgrade the plan allows: the changes, the typed confirmation, the run and the record", async ({ page }) => {
  test.setTimeout(90_000) // two runs: one that fails at stage 4 and one of all eleven stages
  await open(page, "upgrade")
  await recordCalls(page)
  await mock(page, "mockPlatform.applies = 0") // the next apply fails at seed-api, the one after it succeeds
  const posted = async () => (await calls(page, "POST", /\/runs$/)).map((c) => c.body)
  await expect(page.getByRole("heading", { name: "Upgrade to release 0.7.0" })).toBeVisible()
  const plan = page.getByRole("table", { name: /What differs between release 0\.6\.0 and release 0\.7\.0/ })
  await expect(plan.getByRole("row", { name: /platform cluster: platform 0\.1\.2 0\.1\.3 chart \(stage workload-platform, wave 3\)/ })).toBeVisible()
  await expectAccessible(page)
  await page.getByRole("button", { name: "Upgrade to release 0.7.0" }).click()
  const dialog = page.getByRole("dialog", { name: /Upgrade “lab” to release 0\.7\.0/ })
  await expect(dialog.getByText(/Retry continues to release 0\.7\.0/)).toBeVisible()
  const go = dialog.getByRole("button", { name: "Upgrade", exact: true })
  await expect(go).toBeDisabled()
  await dialog.getByLabel(/to confirm/).fill("upgrade lab to 0.7.0")
  await expectAccessible(page)
  await go.click()
  await expect(page.getByText(/This run upgrades the platform from release 0\.6\.0 to release 0\.7\.0/)).toBeVisible()
  expect(await posted()).toEqual([{ kind: "apply", targetBom: expect.stringMatching(/^[0-9a-f]{64}$/), confirm: "upgrade lab to 0.7.0" }])
  // The run fails: the record keeps the release it left, the review says the upgrade has not finished, and Retry
  // goes on without a target or a confirmation.
  await expect(headerState(page)).toHaveAttribute("data-platform-state", "failed", { timeout: 20_000 })
  await expect(page.getByText(/Retry continues to release 0\.7\.0/)).toBeVisible()
  await expect(page.getByRole("heading", { name: "The upgrade to release 0.7.0 has not finished" })).toBeVisible()
  await shot(page, "upgrade-failed")
  await page.getByRole("button", { name: "Retry" }).click()
  await expect.poll(async () => (await posted()).length).toBe(2)
  expect((await posted())[1]).toEqual({ kind: "apply" })
  await expect(headerState(page)).toHaveAttribute("data-platform-state", "ready", { timeout: 40_000 })
  await expect(page.getByRole("heading", { name: /Upgrade to release|has not finished|stays on release/ })).toHaveCount(0)
})

test("a re-apply from ready that fails shows Failed in the header", async ({ page }) => {
  await open(page, "ready")
  await expect(headerState(page)).toHaveText("Ready")
  await mock(page, "mockPlatform.applies = 0") // the mock's next apply fails at seed-api
  await page.getByRole("button", { name: "Apply this release" }).click()
  await expect(headerState(page)).toHaveText("Installing")
  await expect(page.getByRole("group", { name: "Failure" })).toContainText("seed_api_unreachable", { timeout: 15_000 })
  await expect(headerState(page)).toHaveText("Failed")
})

test("a cancelled removal says what stays deleted and offers Remove again", async ({ page }) => {
  await open(page, "removing")
  await page.getByRole("button", { name: "Cancel the run" }).click()
  await expect(page.getByText("The removal was cancelled. What it had already deleted stays deleted")).toBeVisible({ timeout: 15_000 })
  await expect(page.getByText("A removal that stops keeps what it already deleted. Remove again goes on from there.")).toBeVisible()
  await expect(page.getByText(/Nothing (was|is) deleted/)).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Remove again" })).toBeEnabled()
  await expect(page.getByRole("button", { name: "Retry" })).toHaveCount(0)
})

test("a run whose job died offers Retry and Remove, and the page stops asking", async ({ page }) => {
  await open(page, "dead")
  const failure = page.getByRole("group", { name: "Failure" })
  await expect(failure).toContainText("job_dead")
  await expect(failure).toContainText("Press Retry. The time limits start again.")
  await expect(page.getByText("local:iso/pc-seed-pi-00000001.iso")).toBeVisible()
  await expect(page.getByRole("button", { name: "Cancel the run" })).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Remove managed services" })).toBeEnabled()
  // What the API refuses until the run is settled stays locked, with the reason.
  await expect(page.getByRole("button", { name: "Save settings" })).toBeDisabled()
  await expect(page.getByText("The run above ended without finishing.")).toBeVisible()
  await expectAccessible(page)

  await page.waitForTimeout(500) // the one re-read at the run's end
  await recordCalls(page)
  await page.waitForTimeout(4000) // longer than one poll interval
  expect(await calls(page, "GET", "/admin/platform/installations")).toHaveLength(0)
  expect(await calls(page, "GET", /\/runs\//)).toHaveLength(0)

  await page.getByRole("button", { name: "Retry" }).click()
  await expect(page.getByRole("list", { name: "Stages" }).getByText("Running")).toBeVisible()
  expect((await calls(page, "POST", /\/runs$/)).map((c) => c.body)).toEqual([{ kind: "apply" }])
})

test("a stage that is trying again shows why and what to do", async ({ page }) => {
  await open(page, "waiting")
  const stage = page.getByRole("list", { name: "Stages" }).locator('[data-stage="seed-api"]')
  await expect(stage).toContainText("Waiting")
  const why = stage.getByRole("group", { name: "Trying again" })
  await expect(why).toContainText("seed_api_unreachable")
  await expect(why).toContainText("does not answer yet")
  await expect(why).toContainText("Add a route from the ProxCloud host to the platform network")
  await expect(stage.getByRole("group", { name: "Failure" })).toHaveCount(0)
  await expect(page.getByRole("status").filter({ hasText: "Stage 4 of 11: Reach the seed, trying again." })).toHaveCount(1)
  await expect(page.getByRole("button", { name: "Cancel the run" })).toBeEnabled()
  await expectAccessible(page)
})

test("a save that lost against another session reads the settings again and says so", async ({ page }) => {
  await open(page, "draft")
  const url = page.getByLabel("Proxmox API URL", { exact: true })
  await url.fill("https://10.90.0.3:8006")
  await expect(page.getByRole("button", { name: "Check", exact: true })).toBeDisabled()
  await mock(page, "const i = mockPlatform.installations[0]; i.rev++; i.spec.pveApiUrl = 'https://10.90.0.9:8006'")
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect(page.getByRole("alert").filter({ hasText: "Someone else changed these settings" })).toBeVisible()
  await expect(url).toHaveValue("https://10.90.0.9:8006")
  await expect(page.getByRole("button", { name: "Check", exact: true })).toBeEnabled() // the form is clean again
  await expect(page.getByText("Save the settings first")).toHaveCount(0)
  await expectAccessible(page)

  await url.fill("https://10.90.0.3:8006")
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect(page.getByText("Settings saved")).toBeVisible()
  await expect(page.getByRole("alert").filter({ hasText: "Someone else changed these settings" })).toHaveCount(0)
  await expect(url).toHaveValue("https://10.90.0.3:8006")
})

test("a refusal because another session started a run shows that run", async ({ page }) => {
  await open(page, "ready")
  await expect(page.getByRole("table", { name: "Components and their live state" })).toBeVisible()
  await mock(page, "await dispatch('/admin/platform/installations/pi-00000001/runs', { method: 'POST', body: { kind: 'apply' } })")
  await page.getByRole("button", { name: "Replace the seed credential" }).click()
  await expect(page.getByRole("list", { name: "Stages" })).toBeVisible()
  await expect(page.getByRole("button", { name: "Cancel the run" })).toBeEnabled()
  await expect(headerState(page)).toHaveText("Installing")
})

test("without the runner Install is refused and the forms stay", async ({ page }) => {
  await open(page, "norunner")
  await page.getByRole("button", { name: "Install", exact: true }).click()
  await expect(page.getByRole("alert").filter({ hasText: "The run did not start" })).toContainText("Runs need the platform runner")
  await expect(page.getByRole("button", { name: "Save settings" })).toBeEnabled()
})

test("a failed isolation check names the field, links to the provider and runs the sync", async ({ page }) => {
  await open(page, "isolation")
  await recordCalls(page)
  await page.getByRole("button", { name: "Check", exact: true }).click()
  const todo = page.getByRole("list", { name: "What to do for each failed check" })
  await expect(todo).toContainText("“Networks tenants must not reach” on the provider's page")
  await expect(todo.getByRole("link", { name: "Open the provider's page" })).toHaveAttribute("href", "/system/cloud-providers/svc-proxmox-01")
  await expectAccessible(page)
  await todo.getByRole("button", { name: "Run a provider sync, then check again" }).click()
  await expect(page.getByRole("status").filter({ hasText: "Every check passed." })).toBeVisible()
  expect(await calls(page, "POST", "/admin/job/external-service/sync")).toHaveLength(1)
  expect(await calls(page, "POST", /\/preflight$/)).toHaveLength(2)
})

test("a warning of the check does not read as a pass and does not stop the install", async ({ page }) => {
  await open(page, "simplezone")
  await page.getByRole("button", { name: "Check", exact: true }).click()
  await expect(page.getByRole("status").filter({ hasText: "No check failed. 1 warning does not stop the install" })).toBeVisible()
  await expect(page.getByRole("list", { name: "Connection checks" })).toContainText("a cluster create on a network of this zone is refused")
  await expect(page.getByRole("list", { name: "What to do for each failed check" })).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Install", exact: true })).toBeEnabled()
})

test("the ready card: out of sync, a silent seed, and a live read that fails", async ({ page }) => {
  await open(page, "degraded")
  await expect(headerState(page)).toHaveText("Needs attention")
  await expect(page.getByRole("table", { name: "Components and their live state" }).getByRole("row").filter({ hasText: "metallb" })).toContainText("OutOfSync")

  await open(page, "silent")
  const silent = page.getByRole("alert").filter({ hasText: "The seed could not be read" })
  await expect(silent).toContainText("qm terminal 9100") // the VM, its node and the console, not only "did not answer"
  await expect(silent.getByRole("link", { name: /When the seed does not answer/ })).toHaveAttribute("href", "/docs/platform-admin/managed-services")
  await expect(headerState(page)).toHaveText("Needs attention")

  await open(page, "unread")
  await expect(page.getByRole("alert").filter({ hasText: "The live state could not be read" })).toContainText("secrets could not be read")
  await expect(page.getByRole("list", { name: "Components the last run installed" })).toContainText("kamaji 1.0.0, on the platform cluster")
  await expect(headerState(page)).toHaveText("Ready") // the stored phase
  await expectAccessible(page)
})

test("the mock refuses what the API refuses: an unknown field, a fixed field after install, and two rules of the settings", async ({ page }) => {
  await open(page, "ready")
  const answers = await page.evaluate(async (paths) => {
    const { dispatch } = await import(/* @vite-ignore */ paths.router)
    const { mockPlatform } = await import(/* @vite-ignore */ paths.state)
    const in_ = mockPlatform.installations[0]
    const put = (body: unknown) => dispatch(`/admin/platform/installations/${in_.id}`, { method: "PUT", body })
      .then(() => "ok", (e: { status: number; message: string; details?: { problems?: string[] } }) => `${e.status} ${e.message}${e.details?.problems ? ": " + e.details.problems.join("; ") : ""}`)
    const spec = (change: (s: { seed: Record<string, unknown>; storage: Record<string, unknown> }) => void) => { const s = structuredClone(in_.spec); change(s); return s }
    return [
      await put({ name: in_.name, rev: in_.rev, spec: spec((s) => { s.seed.vmid = 1 }) }),
      await put({ name: in_.name, rev: in_.rev, spec: spec((s) => { s.seed.node = "pve2" }) }),
      await put({ name: in_.name, rev: in_.rev + 1, spec: in_.spec }),
      await dispatch(`/admin/platform/installations/${in_.id}/runs`, { method: "POST", body: { kind: "apply", force: true } }).then(() => "ok", (e: { status: number }) => String(e.status)),
      String(in_.rev),
      await put({ name: in_.name, rev: in_.rev, spec: spec((s) => { s.storage.boot = "local" }) }), // the import store, typed
      await put({ name: in_.name, rev: in_.rev, spec: spec((s) => { s.storage.boot = "local"; s.storage.import = "" }) }), // and filled in by the API
      await put({ name: in_.name, rev: in_.rev, spec: spec((s) => { s.seed.mode = "existing" }) }),
    ]
  }, MOCK)
  expect(answers[0]).toBe('400 Invalid request body: unknown field "spec.seed.vmid"')
  expect(answers[1]).toMatch(/^409 .*spec\.seed\.node/)
  expect(answers[2]).toMatch(/^409 the installation changed since it was read/)
  expect(answers[3]).toBe("400")
  expect(answers[4]).toBe("1") // a run, a rotation and a refused save leave rev alone
  expect(answers[5]).toMatch(/^400 .*spec\.storage\.import "local" is the boot store/)
  expect(answers[6]).toBe(answers[5])
  expect(answers[7]).toMatch(/^400 .*spec\.seed\.mode existing comes in a later release/)
})

// Error and run states of the page, in both themes: the route matrix of a11y.spec.ts loads each state but opens none.
for (const theme of themes) {
  test(`axe on the error, run and removal states [${theme}]`, async ({ page }) => {
    test.setTimeout(90_000)
    await open(page, "", theme)
    await page.getByRole("button", { name: "Save draft" }).click()
    await expect(page.getByRole("alert").filter({ hasText: "settings need correcting" })).toBeVisible()
    await page.getByLabel("SSH public keys (optional)").fill("not a key")
    await expect(page.getByLabel("SSH public keys (optional)")).toHaveAttribute("aria-describedby", "sshKeys-hint sshKeys-error")
    await expect(page.locator("#sshKeys-error")).toHaveText("SSH keys: at most 8 public keys, one per line.")
    await expectAccessible(page)

    await open(page, "isolation")
    await page.getByRole("button", { name: "Check", exact: true }).click()
    await expect(page.getByRole("status").filter({ hasText: "checks failed" })).toBeVisible()
    await expectAccessible(page)

    await open(page, "running")
    await expect(page.getByRole("list", { name: "Stages" })).toBeVisible()
    await expectAccessible(page)

    await open(page, "failed")
    await expect(page.getByRole("group", { name: "Failure" })).toBeVisible()
    await expectAccessible(page)

    await open(page, "ready")
    await page.getByRole("button", { name: "Remove managed services" }).click()
    const dialog = page.getByRole("dialog")
    await expect(dialog).toContainText("any that do are deleted with the cluster's machines and volumes")
    await dialog.getByLabel(/Type remove lab to confirm/).fill("remove lab")
    await expectAccessible(page)
    await dialog.getByRole("button", { name: "Remove", exact: true }).click()
    await expect(page.getByRole("list", { name: "Stages" })).toBeVisible()
    await expectAccessible(page)
    await expect(page.getByText("Managed services “lab” were removed")).toBeVisible({ timeout: 20_000 })
    await expectAccessible(page)
  })
}

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
  for (const name of ["Save settings", "Store the secrets", "Check", "Apply this release", "Download platform kubeconfig", "Download seed kubeconfig", "Replace the seed credential", "Remove managed services"]) {
    await expect(page.getByRole("button", { name, exact: true })).toBeDisabled()
  }
})
