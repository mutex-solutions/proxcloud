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
  await page.getByRole("radio", { name: /^Homelab/ }).check()
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

  // The first save says what it wrote into the provider, so that nobody adds the blocked networks by hand.
  await expect(page.getByRole("status").filter({ hasText: "The save also changed the provider" })).toContainText("10.90.0.5/32")
  // A corrected address: the entry the earlier save added for the old one is taken off, and the page says so.
  for (const [address, gone] of [["10.90.0.6", "10.90.0.5/32"], ["10.90.0.5", "10.90.0.6/32"]]) {
    await expect(page.getByRole("button", { name: "Save settings" })).toBeEnabled() // the save before it is done: the form was read again
    await page.getByLabel("Seed address", { exact: true }).fill(address)
    await page.getByRole("button", { name: "Save settings" }).click()
    await expect(page.getByRole("status").filter({ hasText: "The save also changed the provider" })).toContainText(`need them (an earlier save had added them): ${gone}`)
  }

  // One command as root for this installation: its stores come from the settings, and it prints no secret.
  const id = "pi-00000001" // the mock numbers its installations
  await expect(page.getByLabel("root command", { exact: true })).toContainText(`PC_INSTALL='${id}'`)
  await expect(page.getByLabel("root command", { exact: true })).toContainText("STOP: the token capmox@pve!capi exists on this cluster")
  await expect(page.getByLabel("root command", { exact: true })).toContainText("'/storage/local-zfs'")
  expect((await calls(page, "GET", "/admin/setup/proxmox-script")).at(-1)!.path).toMatch(new RegExp(`installation=${id}`))
  expect((await calls(page, "GET", "/admin/setup/proxmox-script")).at(-1)!.path).not.toMatch(/bootStorage|managedServices/)

  // Check fails until both secrets are stored, and says what to do.
  await page.getByRole("button", { name: "Check", exact: true }).click()
  await expect(page.getByRole("status").filter({ hasText: "checks failed" })).toBeVisible()
  await expect(page.getByRole("list", { name: "What to do for each failed check" })).toContainText("Run the root command of step 2 on a node and paste the PC-ENROLL line it prints.")
  await expectAccessible(page)
  await shot(page, "check-failed")

  // The command's last line carries both secrets. The page checks it before it is sent: a bare secret, a line cut
  // short and a line of another installation are refused with what is wrong, and nothing is sent.
  const enroll = (inst: string, at: number, capi = CAPI_SECRET, csi = CSI_SECRET) => `PC-ENROLL:${Buffer.from(`1:${inst}:${at}:${capi}:${csi}`).toString("base64")}`
  const line = page.getByLabel("The PC-ENROLL line the command printed")
  const store = page.getByRole("button", { name: "Store the secrets", exact: true })
  await expect(line).toHaveAttribute("type", "password")
  await expect(line).toHaveAttribute("autocomplete", "new-password") // not a login: no password manager offer
  await expect(store).toBeDisabled()
  for (const [bad, says] of [
    [CAPI_SECRET, /not the PC-ENROLL line the command prints/],
    [enroll(id, 1795000000).slice(0, -40), /not the PC-ENROLL line the command prints/],
    [enroll("pi-ffffffff", 1795000000), /of another installation/],
  ] as const) {
    await line.fill(bad)
    await expect(page.locator("#enroll-line-state")).toHaveText(says)
    await expect(store).toBeDisabled()
  }
  // As a terminal wraps it: white space inside the line is dropped.
  const good = enroll(id, 1795000000)
  await line.fill(`${good.slice(0, 50)} ${good.slice(50)}`)
  await store.click()
  await expect(page.getByRole("list", { name: "Stored token secrets" })).toContainText("capmox@pve!capi: stored. It is never shown again.")
  await expect(page.getByRole("list", { name: "Stored token secrets" })).toContainText("csi@pve!csi: stored. It is never shown again.")
  await expect(line).toHaveValue("")
  const [stored] = await calls(page, "PUT", /\/credentials$/)
  expect(stored.body).toEqual({ enroll: good })
  expect(await page.content()).not.toContain(CAPI_SECRET)
  expect(await page.content()).not.toContain(CSI_SECRET)
  expect(await page.content()).not.toContain(good)
  expect(await page.evaluate(() => JSON.stringify([{ ...localStorage }, { ...sessionStorage }]))).not.toContain(good)
  // With both secrets stored the command no longer stops on these tokens: they are this installation's.
  await expect(page.getByLabel("root command", { exact: true })).not.toContainText("STOP: the token")

  // The same line again is a replay: the API refuses it and the stored secrets stay.
  await line.fill(good)
  await store.click()
  await expect(page.getByRole("alert").filter({ hasText: "The secrets were not stored" })).toContainText("was stored already")

  // By hand, for a token made another way: the two fields are write-only as well.
  await page.getByText("Enter the two secrets by hand instead").click()
  const capi = page.getByLabel("Secret of capmox@pve!capi")
  await expect(capi).toHaveAttribute("type", "password")
  await capi.fill("not-a-secret")
  await expect(page.getByText(/Not a Proxmox token secret/)).toBeVisible()
  await expect(page.getByRole("button", { name: "Store these secrets" })).toBeDisabled()
  await capi.fill(CAPI_SECRET)
  await page.getByRole("button", { name: "Store these secrets" }).click()
  await expect(capi).toHaveValue("")
  expect((await calls(page, "PUT", /\/credentials$/)).at(-1)!.body).toEqual({ capi: { tokenId: "capmox@pve!capi", secret: CAPI_SECRET } })
  expect(await page.content()).not.toContain(CAPI_SECRET)

  await page.getByRole("button", { name: "Check again" }).click()
  await expect(page.getByRole("status").filter({ hasText: "No check failed. 1 warning does not stop the install" })).toBeVisible() // the scheduler row: the mock runs without it

  // Review, then Install: the first run of the mock fails at the seed's API.
  await expect(page.getByText("1 control plane machine and 3 worker machines in the pool")).toBeVisible()
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
  await expect(k8s).toContainText("The install imported the node image of Kubernetes v1.33.4 and did not publish it for tenants")
  await expect(k8s.getByRole("link", { name: "Or do each step in Template updates" })).toHaveAttribute("href", "/system/template-updates")
  await expect(k8s.getByRole("link", { name: "attach it on the provider's Projects tab" })).toHaveAttribute("href", /\/system\/cloud-providers\/svc-/)
  const dbs = providers.getByRole("listitem").filter({ hasText: "lab Databases" })
  await expect(dbs).toContainText("New projects do not get this provider, and the install attached it to no project")
  await expect(dbs).not.toContainText("Kubernetes version")
  // One row says what tenants miss without the job scheduler, with the command; nothing turns it on from here.
  await expect(page.getByLabel("Job scheduler", { exact: true })).toContainText("a tenant cannot delete a project")
  await expect(page.getByLabel("Job scheduler", { exact: true })).toContainText("./proxcloud-up.sh install --scheduler")

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
test("the platform ArgoCD gets an address: the settings, the plan, the link after the apply, and the clear", async ({ page }) => {
  await open(page, "ready")
  await recordCalls(page)
  const address = page.getByLabel("ArgoCD address (optional)", { exact: true }), sources = page.getByLabel("Operator networks", { exact: true })
  const sent = async () => (await calls(page, "PUT", /\/installations\/pi-/)).map((c) => c.body.spec.access?.argocd ?? null)
  // One of the two, an address outside the load balancer range, and everyone: refused in the form.
  await address.fill("10.90.0.150")
  await page.getByRole("button", { name: "Save settings" }).click() // refused in the form: problems show from the first Save
  await expect(page.getByText("Operator networks: 1 to 16 IPv4 networks such as 192.0.2.0/24, never 0.0.0.0/0").first()).toBeVisible()
  await sources.fill("0.0.0.0/0")
  await expect(page.getByText("Operator networks: 1 to 16 IPv4 networks").first()).toBeVisible()
  await sources.fill("192.0.2.0/24, 198.51.100.7/32")
  await expect(page.getByText("Operator networks: 1 to 16 IPv4 networks")).toHaveCount(0)
  await address.fill("10.90.0.99")
  await expect(page.getByText("ArgoCD address: one free IPv4 address inside the load balancer range").first()).toBeVisible()
  expect(await sent()).toEqual([])
  await address.fill("10.90.0.150")
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect.poll(sent).toEqual([{ address: "10.90.0.150", sources: ["192.0.2.0/24", "198.51.100.7/32"] }])
  // Saved and not applied: the plan lists it with what it does, and the card still shows the port-forward (to 443 after the apply).
  const row = page.getByRole("listitem").filter({ hasText: "access.argocd" })
  await expect(row).toContainText("new → https://10.90.0.150")
  await expect(row).toContainText("Only 192.0.2.0/24, 198.51.100.7/32 are let in")
  await expect(page.getByRole("link", { name: "Open the platform ArgoCD" })).toHaveCount(0)
  await expect(page.getByLabel("platform ArgoCD commands", { exact: true })).toContainText("port-forward svc/argocd-server 8080:443")
  // The apply records the address (set here: the mock has no apply of a settings change). The card links to it, says
  // how to sign in, and shows no password; the seed's ArgoCD keeps its port-forward.
  // The next save reads it.
  // The networks on the card are the ones that run applied, not the ones saved since.
  await mock(page, `Object.assign(mockPlatform.installations[0].status, { argocdUrl: "https://10.90.0.150", argocdSources: ["192.0.2.0/24", "198.51.100.7/32"] })`)
  await sources.fill("192.0.2.0/24")
  await page.getByRole("button", { name: "Save settings" }).click()
  const link = page.getByRole("link", { name: "Open the platform ArgoCD" })
  await expect(link).toHaveAttribute("href", "https://10.90.0.150")
  await expect(page.getByText("from 192.0.2.0/24, 198.51.100.7/32 only. The certificate is ArgoCD's own and self-signed")).toBeVisible()
  await expect(page.getByLabel("platform ArgoCD password command", { exact: true })).toHaveText(
    "export KUBECONFIG=./lab-platform.kubeconfig\nkubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo")
  await expect(page.getByText("What you change in ArgoCD by hand is undone by its next sync")).toBeVisible()
  await expect(page.getByLabel("seed ArgoCD commands", { exact: true })).toContainText("port-forward svc/argocd-server 8081:80")
  await expect(page.getByRole("listitem").filter({ hasText: "access.argocd" })).toContainText("Only 192.0.2.0/24 are let in")
  await expectAccessible(page)
  // Both fields emptied: the block is cleared, and the plan says the address goes.
  await address.fill("")
  await sources.fill("")
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect.poll(sent).toEqual([{ address: "10.90.0.150", sources: ["192.0.2.0/24", "198.51.100.7/32"] }, { address: "10.90.0.150", sources: ["192.0.2.0/24"] }, null])
  await expect(page.getByRole("listitem").filter({ hasText: "access.argocd" })).toContainText("https://10.90.0.150 → removed")
})

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

// A green install is usable with one button: the node image the install imported is smoke-tested and published on
// each node, and only then is its Kubernetes version added to the provider. Until then the card names each node.
test("the ready card publishes the install's node image and offers its Kubernetes version", async ({ page }) => {
  await open(page, "ready")
  await recordCalls(page)
  const k8s = page.getByRole("list", { name: "Providers" }).getByRole("listitem").filter({ hasText: "lab Kubernetes" })
  const nodes = k8s.getByRole("list", { name: "Node image per node" })
  await expect(nodes.getByRole("listitem").filter({ hasText: "pve1" })).toContainText("Not imported, waits its turn")
  await expect(nodes.getByRole("listitem").filter({ hasText: "pve2" })).toContainText("Not imported, waits its turn")
  // Asked before anything is published, the API offers nothing and names the nodes.
  const refused = await page.evaluate(async (paths) => {
    const { dispatch } = await import(/* @vite-ignore */ paths.router)
    const { mockPlatform } = await import(/* @vite-ignore */ paths.state)
    return dispatch(`/admin/platform/installations/${mockPlatform.installations[0].id}/kubernetes-version`, { method: "POST", body: {} })
      .then(() => "ok", (e: { status: number; details?: { code?: string; missingNodes?: string[] } }) => `${e.status} ${e.details?.code} ${e.details?.missingNodes}`)
  }, MOCK)
  expect(refused).toBe("409 node_image_unpublished pve1,pve2")

  await k8s.getByRole("button", { name: "Test, publish and offer Kubernetes v1.33.4" }).click()
  await expect(k8s).toContainText("Offers Kubernetes v1.33.4.", { timeout: 20_000 })
  await expect(page.getByText("Tenants can now create clusters with Kubernetes v1.33.4")).toBeVisible()
  await expect(k8s).not.toContainText("no Kubernetes version")
  const advances = await calls(page, "POST", "/admin/template-updates/publish-all")
  expect(advances.length).toBeGreaterThan(0)
  expect(await calls(page, "POST", /\/kubernetes-version$/)).toHaveLength(1)
})

// The choice of who gets the two providers: preselected by whether the site has projects, sent with the settings,
// applied by the run, and shown on the card as pending when it was changed after the install.
test("the settings choose who gets the providers, and a run applies the choice", async ({ page }) => {
  test.setTimeout(90_000)
  await open(page)
  await recordCalls(page)
  // The mock site has projects: it is live with tenants, so nothing changes for the projects that exist.
  await expect(page.getByRole("radio", { name: /^New projects only/ })).toBeChecked()
  await expect(page.getByText(/so “New projects only” is preselected/)).toBeVisible()
  await fillSettings(page)
  await page.getByRole("radio", { name: /^All projects/ }).check()
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByText("Settings saved")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect((created.body as { spec: { tenants?: { attach?: string } } }).spec.tenants).toEqual({ attach: "all" })
  await expect(page.getByRole("radio", { name: /^All projects/ })).toBeChecked()

  // A stored installation without the key means none, and a save that leaves it alone does not add it.
  await open(page, "ready")
  await recordCalls(page)
  await expect(page.getByRole("radio", { name: /^None: I attach the providers later/ })).toBeChecked()
  const providers = page.getByRole("list", { name: "Providers" })
  await expect(providers.getByRole("listitem").filter({ hasText: "lab Databases" })).toContainText("New projects do not get this provider")
  await page.getByRole("radio", { name: /^New projects only/ }).check()
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect(page.getByText("Settings saved")).toBeVisible()
  const [saved] = await calls(page, "PUT", /\/admin\/platform\/installations\/pi-[0-9a-f]+$/)
  expect((saved.body as { spec: { tenants?: { attach?: string } } }).spec.tenants).toEqual({ attach: "new" })
  await expect(page.getByText("That is not applied yet: press “Apply this release” below.")).toBeVisible()

  await page.getByRole("button", { name: "Apply this release" }).click()
  await expect(providers.getByRole("listitem").filter({ hasText: "lab Databases" })).toContainText("New projects get this provider.", { timeout: 40_000 })
  await expect(providers.getByRole("listitem").filter({ hasText: "lab Kubernetes" })).toContainText("New projects get this provider.")
  await expect(page.getByText("That is not applied yet")).toHaveCount(0)
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

test("an upgrade that replaces the seed's k3s: the plan says what is away and written, and asks for a backup of the seed", async ({ page }) => {
  await open(page, "upgradek3s")
  await recordCalls(page)
  const plan = page.getByRole("table", { name: /What differs between release 0\.6\.0 and release 0\.7\.0/ })
  await expect(plan.getByRole("row", { name: /seed\.k3s v1\.35\.10\+k3s1 v1\.35\.11\+k3s1 seed \(stage seed-k3s\).*The seed's API is away for about a minute.*the databases keep serving/ })).toBeVisible()
  await expect(plan.getByRole("row", { name: /seed-etcd-snapshot .*pre-upgrade-v1\.35\.10-k3s1.* site \(stage seed-k3s\)/ })).toBeVisible()
  await expect(plan.getByRole("row", { name: /seed-k3s-binary .*k3s\.prev/ })).toBeVisible()
  await expect(page.getByText(/Seed backup \(needed before this upgrade\):.*The portal cannot see backups of seed VM 9100 on store nas-backup/)).toBeVisible()
  const start = page.getByRole("button", { name: "Upgrade to release 0.7.0" })
  await expect(start).toBeDisabled()
  await expectAccessible(page)
  await page.getByLabel("I checked in Proxmox: seed VM 9100 has a backup made after it was created and younger than 24 hours").check()
  await start.click()
  const dialog = page.getByRole("dialog", { name: /Upgrade “lab” to release 0\.7\.0/ })
  await dialog.getByLabel(/to confirm/).fill("upgrade lab to 0.7.0")
  await dialog.getByRole("button", { name: "Upgrade", exact: true }).click()
  await expect(page.getByText(/This run upgrades the platform from release 0\.6\.0 to release 0\.7\.0/)).toBeVisible()
  await expect(page.locator('[data-stage="seed-k3s"]')).toContainText("5a. Upgrade the seed's k3s")
  await expect(page.locator('[data-stage="seed-platform"]')).toContainText("6. Install the seed's components")
  expect((await calls(page, "POST", /\/runs$/)).map((c) => c.body)).toEqual([{ kind: "apply", targetBom: expect.stringMatching(/^[0-9a-f]{64}$/), confirm: "upgrade lab to 0.7.0", seedBackupTaken: true }])
})

test("an upgrade that replaces the platform cluster's machines: two actions, what stops per unit, and the longer phrase", async ({ page }) => {
  await open(page, "upgradek8s")
  await recordCalls(page)
  await expect(page.getByRole("heading", { name: "Kubernetes upgrade to release 0.7.0" })).toBeVisible()
  await expect(page.getByRole("note")).toContainText("There is no way back once the new control plane machine has joined")
  const actions = page.getByRole("list", { name: "The two actions of the Kubernetes upgrade" })
  await expect(actions.locator('[data-part="platform"]')).toContainText('node_image_missing: node pve1 has no template of node image')
  await expect(actions.getByRole("button", { name: "Replace the machines" })).toBeDisabled()
  const units = page.getByRole("table", { name: /The machines the upgrade replaces, in order/ })
  await expect(units.getByRole("row", { name: /1\. controlPlane 1\.35\.10 1\.35\.11 one more machine first .*the platform cluster's API does not answer.*Not measured on a lab yet/ })).toBeVisible()
  await expect(units.getByRole("row", { name: /4\. dbaas@pve1 .* old machine deleted first .*deleted BEFORE the new one exists/ })).toBeVisible()
  await expect(page.getByRole("button", { name: /^Upgrade to release/ })).toHaveCount(0)
  await expectAccessible(page)
  await shot(page, "upgrade-k8s")
  // The seed part: the plain phrase, a run that stops after the seed's components.
  await actions.getByRole("button", { name: "Seed and node image" }).click()
  let dialog = page.getByRole("dialog", { name: /Upgrade “lab” to release 0\.7\.0/ })
  await dialog.getByLabel(/to confirm/).fill("upgrade lab to 0.7.0")
  await dialog.getByRole("button", { name: "Upgrade", exact: true }).click()
  await expect(page.getByText(/It is the first of two actions/)).toBeVisible()
  await expect(page.locator('[data-stage="cluster"]')).toHaveCount(0)
  await expect(headerState(page)).toHaveAttribute("data-platform-state", "ready", { timeout: 30_000 })
  // Done: the platform part is offered, with the phrase that names the machines.
  await expect(actions.locator('[data-part="seed"]')).toContainText("Seed and node image (done)")
  await actions.getByRole("button", { name: "Replace the machines" }).click()
  dialog = page.getByRole("dialog", { name: /Upgrade “lab” to release 0\.7\.0/ })
  await expect(dialog).toContainText("There is no way back")
  const go = dialog.getByRole("button", { name: "Upgrade", exact: true })
  await dialog.getByLabel(/to confirm/).fill("upgrade lab to 0.7.0")
  await expect(go).toBeDisabled()
  await dialog.getByLabel(/to confirm/).fill("upgrade lab to 0.7.0 and replace its machines")
  await go.click()
  await expect(page.locator('[data-stage="cluster"]')).toBeVisible()
  expect((await calls(page, "POST", /\/runs$/)).map((c) => c.body)).toEqual([
    { kind: "apply", targetBom: expect.stringMatching(/^[0-9a-f]{64}$/), confirm: "upgrade lab to 0.7.0", part: "seed" },
    { kind: "apply", confirm: "upgrade lab to 0.7.0 and replace its machines" },
  ])
})

test("an upgrade that finished in another session: the refusal bom_installed reads as a plain message", async ({ page }) => {
  await open(page, "upgrade")
  await page.getByRole("button", { name: "Upgrade to release 0.7.0" }).click()
  const dialog = page.getByRole("dialog", { name: /Upgrade “lab” to release 0\.7\.0/ })
  await dialog.getByLabel(/to confirm/).fill("upgrade lab to 0.7.0")
  await mock(page, "const b = (await dispatch('/admin/platform/bom', {})).data; mockPlatform.installations[0].status.bom = { release: b.release, sha256: b.sha256 }")
  await dialog.getByRole("button", { name: "Upgrade", exact: true }).click()
  await expect(page.getByRole("alert").filter({ hasText: "The platform is already on this release" })).toContainText("no run was started")
  await expect(page.getByText(/targetBom|The run did not start/)).toHaveCount(0)
  await expect(page.getByRole("heading", { name: /Upgrade to release/ })).toHaveCount(0) // the page read the plan again
})

test("an unfinished upgrade toward an earlier ProxCloud's release: the review offers the move to this one's in its place", async ({ page }) => {
  await open(page, "replace")
  await recordCalls(page)
  await expect(page.getByRole("heading", { name: "The upgrade to release 0.6.5 has not finished" })).toBeVisible()
  await expect(page.getByRole("heading", { name: "Upgrade to release 0.7.0 instead" })).toBeVisible()
  await expect(page.getByText(/in place of the unfinished upgrade to release 0\.6\.5/)).toBeVisible()
  await expectAccessible(page)
  await page.getByRole("button", { name: "Upgrade to release 0.7.0" }).click()
  const dialog = page.getByRole("dialog", { name: /Upgrade “lab” to release 0\.7\.0/ })
  await dialog.getByLabel(/to confirm/).fill("upgrade lab to 0.7.0")
  await dialog.getByRole("button", { name: "Upgrade", exact: true }).click()
  await expect(page.getByText(/This run upgrades the platform from release 0\.6\.0 to release 0\.7\.0/)).toBeVisible()
  expect((await calls(page, "POST", /\/runs$/)).map((c) => c.body)).toEqual([{ kind: "apply", targetBom: expect.stringMatching(/^[0-9a-f]{64}$/), confirm: "upgrade lab to 0.7.0" }])
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
  await expect(page.getByRole("status").filter({ hasText: "No check failed. 1 warning does not stop the install" })).toBeVisible() // the scheduler row: the mock runs without it
  expect(await calls(page, "POST", "/admin/job/external-service/sync")).toHaveLength(1)
  expect(await calls(page, "POST", /\/preflight$/)).toHaveLength(2)
})

test("a warning of the check does not read as a pass and does not stop the install", async ({ page }) => {
  await open(page, "simplezone")
  await page.getByRole("button", { name: "Check", exact: true }).click()
  await expect(page.getByRole("status").filter({ hasText: "No check failed. 2 warnings do not stop the install" })).toBeVisible()
  await expect(page.getByRole("list", { name: "Connection checks" })).toContainText("a cluster create on a network of this zone is refused")
  // The other warning: what tenants miss while the job scheduler is off, with the command that turns it on.
  await expect(page.getByRole("list", { name: "Connection checks" })).toContainText("a tenant cannot delete a project")
  await expect(page.getByRole("list", { name: "Connection checks" })).toContainText("./proxcloud-up.sh install --scheduler")
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

test("the network is an existing bridge, with an optional VLAN tag, or a vnet the install makes", async ({ page }) => {
  await open(page)
  await recordCalls(page)
  await fillSettings(page)
  // An existing bridge is the default, and a spec without a tag names neither a mode nor a VLAN.
  await expect(page.getByRole("radio", { name: /An existing bridge/ })).toBeChecked()
  await page.getByLabel("VLAN tag (optional)", { exact: true }).fill("4095")
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByText("VLAN tag: 1 to 4094, or empty.").first()).toBeVisible()
  expect(await calls(page, "POST", "/admin/platform/installations")).toHaveLength(0)

  // Routed: the bridge field names the vnet, and the tag has no meaning.
  await page.getByRole("radio", { name: /Routed by the node/ }).check()
  await expect(page.getByLabel("VLAN tag (optional)", { exact: true })).toHaveCount(0)
  // The bridge's name does not become the vnet's: a vnet named like a live interface would take it over.
  await expect(page.getByLabel("Vnet name", { exact: true })).toHaveValue("")
  await page.getByLabel("Vnet name", { exact: true }).fill("Platform-Net")
  await expect(page.getByText(/Vnet name: a lower-case letter/).first()).toBeVisible()
  await page.getByLabel("Vnet name", { exact: true }).fill("pcplat0")
  await page.getByLabel("Seed node", { exact: true }).fill("pve2")
  await expect(page.getByText(/A routed network is a layout for one Proxmox node, and the settings place machines on pve1, pve2/).first()).toBeVisible()
  await expectAccessible(page)
  await page.getByLabel("Seed node", { exact: true }).fill("pve1")
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByText("2. Proxmox identities")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect(created.body.spec.network).toEqual({ mode: "routed", bridge: "pcplat0", prefix: "10.90.0.0/24", gateway: "10.90.0.1", dns: ["10.90.0.1"] })
  await expect(page.getByRole("radio", { name: /Routed by the node/ })).toBeChecked()

  // Back to a bridge with a tag, in the saved draft.
  await page.getByRole("radio", { name: /An existing bridge/ }).check()
  await expect(page.getByLabel("Bridge", { exact: true })).toHaveValue("")
  await page.getByLabel("Bridge", { exact: true }).fill("vmbr1")
  await page.getByLabel("VLAN tag (optional)", { exact: true }).fill("140")
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect.poll(async () => (await calls(page, "PUT", /\/installations\/pi-/)).length).toBe(1)
  const [saved] = await calls(page, "PUT", /\/installations\/pi-/)
  expect(saved.body.spec.network).toEqual({ bridge: "vmbr1", vlan: 140, prefix: "10.90.0.0/24", gateway: "10.90.0.1", dns: ["10.90.0.1"] })
})

test("the tenant path is tested on demand, on an installed platform only", async ({ page }) => {
  await open(page, "draft")
  await expect(page.getByRole("button", { name: "Check", exact: true })).toBeVisible()
  await expect(page.getByRole("button", { name: "Test the tenant path" })).toHaveCount(0)

  await open(page, "ready")
  await recordCalls(page)
  const test_ = page.getByRole("region", { name: "Test of the tenant path" })
  await expect(test_).toContainText("It costs one VM clone and two SDN applies")
  // Check does not start it.
  await page.getByRole("button", { name: "Check", exact: true }).click()
  await expect(page.getByRole("status").filter({ hasText: /check/i }).first()).toBeVisible()
  expect(await calls(page, "POST", /\/tenant-path-probe$/)).toHaveLength(0)

  await test_.getByRole("button", { name: "Test the tenant path" }).click()
  await expect(test_.getByRole("status")).toContainText("Running on pve1")
  await expect(test_.getByRole("button", { name: "Testing…" })).toBeDisabled()
  await expect(test_.getByRole("status")).toContainText("The path is open", { timeout: 15_000 })
  await expect(test_.getByRole("list", { name: "Connections of the last test" })).toContainText("the platform's own API address: tenants must not reach it")
  const [started] = await calls(page, "POST", /\/tenant-path-probe$/)
  expect(started.body).toEqual({ node: "pve1" })
  await expectAccessible(page)
  await shot(page, "tenant-path-test")
})

// The addresses and stores every preset test types in; a preset fills the nodes, counts and sizes.
// Both token secrets, typed by hand (the PC-ENROLL line has its own test).
async function storeSecrets(page: Page) {
  await page.getByText("Enter the two secrets by hand instead").click()
  await page.getByLabel("Secret of capmox@pve!capi").fill(CAPI_SECRET)
  await page.getByLabel("Secret of csi@pve!csi").fill(CSI_SECRET)
  await page.getByRole("button", { name: "Store these secrets" }).click()
  const stored = page.getByRole("list", { name: "Stored token secrets" })
  await expect(stored).toContainText("capmox@pve!capi: stored. It is never shown again.")
  await expect(stored).toContainText("csi@pve!csi: stored. It is never shown again.")
}

async function fillAddresses(page: Page, bridge = "vmbr1") {
  const fill = (label: string, value: string) => page.getByLabel(label, { exact: true }).fill(value)
  await fill("Name", "lab")
  if (bridge) await fill("Bridge", bridge)
  for (const [label, v] of [["Network", "10.90.0.0/24"], ["Gateway", "10.90.0.1"], ["DNS servers", "10.90.0.1"], ["API address", "10.90.0.10"], ["Node range", "10.90.0.20-10.90.0.60"],
    ["Load balancer range", "10.90.0.100-10.90.0.150"], ["Proxmox API URL", "https://10.90.0.2:8006"], ["Seed address", "10.90.0.5"], ["Seed API sources", "192.0.2.10/32"], ["Boot store", "local-zfs"]]) await fill(label, v)
}
const sizes = (g: { name: string; nodes: string[]; count: number; cores: number; memoryMiB: number }) => [g.name, g.nodes, g.count, g.cores, g.memoryMiB]

test("the Homelab preset: one control plane machine and one small machine per group on one node", async ({ page }) => {
  await open(page)
  await recordCalls(page)
  await expect(page.getByRole("checkbox", { name: "Group system: node pve1" })).toBeVisible() // the nodes are read
  await expect(page.getByRole("radio", { name: /^Custom/ })).toBeChecked()
  await page.getByRole("radio", { name: /^Homelab/ }).check()
  // Filled in, and still the form's own fields: the nodes, the counts, the sizes, the count of the control plane.
  await expect(page.getByLabel("Seed node", { exact: true })).toHaveValue("pve1")
  await expect(page.getByLabel("Control plane node", { exact: true })).toHaveValue("pve1")
  await expect(page.getByLabel("Control plane machines", { exact: true })).toHaveValue("1")
  await expect(page.getByLabel("Group dbaas: machines")).toHaveValue("1")
  await expect(page.getByLabel("Group dbaas: Memory (GiB)")).toHaveValue("4")
  // The site has two nodes, so the network stays an existing bridge.
  await expect(page.getByRole("radio", { name: /An existing bridge/ })).toBeChecked()
  await fillAddresses(page)
  await expectAccessible(page)
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByText("2. Proxmox identities")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect(created.body.preset).toBeUndefined() // the form sends the fields, not the choice
  expect(created.body.spec.clusters[0].controlPlane).toEqual({ node: "pve1", cores: 2, memoryMiB: 4096, diskGiB: 30 })
  expect(created.body.spec.clusters[0].nodeGroups.map(sizes)).toEqual([["system", ["pve1"], 1, 2, 4096], ["kamaji", ["pve1"], 1, 2, 4096], ["dbaas", ["pve1"], 1, 2, 4096]])
  expect(created.body.spec.network.mode).toBeUndefined()
  // The review counts the machines and their spares against what the node has free.
  await expect(page.getByText("1 control plane machine and 3 worker machines in the pool")).toBeVisible()
  await expect(page.getByRole("list", { name: "Cores and memory per node" })).toContainText("pve1: 10 cores and 20 GiB of memory; 24 GiB with the one more machine an update of the control plane makes, 36 GiB with one more of each group as well. The node has 112 GiB free of 128 GiB.")
})

test("the Standard preset: three control plane machines on one node, two machines per group over two nodes", async ({ page }) => {
  await open(page)
  await recordCalls(page)
  await expect(page.getByRole("checkbox", { name: "Group system: node pve1" })).toBeVisible()
  await page.getByRole("radio", { name: /^Standard/ }).check()
  await expect(page.getByLabel("Control plane machines", { exact: true })).toHaveValue("3")
  await expect(page.getByText(/do not survive the loss of that node or of its disk/)).toBeVisible()
  await expect(page.getByRole("list", { name: "What the preset changed" })).toHaveCount(0)
  await fillAddresses(page)
  // Everything stays editable: one group gets a third machine.
  await page.getByLabel("Group kamaji: machines").fill("3")
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByText("2. Proxmox identities")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect(created.body.spec.seed).toMatchObject({ node: "pve1", cores: 2, memoryMiB: 6144 })
  expect(created.body.spec.clusters[0].controlPlane).toEqual({ node: "pve1", replicas: 3, cores: 2, memoryMiB: 4096, diskGiB: 30 })
  expect(created.body.spec.clusters[0].nodeGroups.map(sizes)).toEqual([["system", ["pve1", "pve2"], 2, 4, 8192], ["kamaji", ["pve2", "pve1"], 3, 4, 8192], ["dbaas", ["pve1", "pve2"], 2, 4, 8192]])
  // pve1: seed 6, control plane 3 x 4, system 8, kamaji 8, dbaas 8 = 42 GiB running; 46 with the control plane's spare, 70 with the three groups' too.
  await expect(page.getByText("3 control plane machines on pve1 and 7 worker machines in the pool")).toBeVisible()
  const load = page.getByRole("list", { name: "Cores and memory per node" })
  await expect(load).toContainText("pve1: 20 cores and 42 GiB of memory; 46 GiB with the one more machine an update of the control plane makes, 70 GiB with one more of each group as well. The node has 112 GiB free of 128 GiB.")
  await expect(load).toContainText("pve2: 16 cores and 32 GiB of memory; 32 GiB with the one more machine an update of the control plane makes, 56 GiB with one more of each group as well.")
  // With both secrets stored the Check has its capacity row.
  await storeSecrets(page)
  await page.getByRole("button", { name: "Check", exact: true }).click()
  await expect(page.getByRole("status").filter({ hasText: "No check failed. 1 warning does not stop the install" })).toBeVisible() // the scheduler row: the mock runs without it
  await expect(page.getByText("Every node has the memory for its machines and their spares during an update.")).toBeVisible()
})

test("on one small node: Standard keeps one control plane machine, Homelab makes a network the node routes and passes the Check, and the page says what does not fit", async ({ page }) => {
  await open(page, "onenode")
  await recordCalls(page)
  await expect(page.getByRole("checkbox", { name: "Group system: node pve1" })).toBeVisible()
  // Standard: three control plane machines would need more than the node has free, and the page says so.
  await page.getByRole("radio", { name: /^Standard/ }).check()
  await expect(page.getByLabel("Control plane machines", { exact: true })).toHaveValue("1")
  await expect(page.getByRole("list", { name: "What the preset changed" })).toContainText("One control plane machine, not three: with three, pve1 would need 70 GiB of memory and has 24 GiB free.")
  await expect(page.getByLabel("Group system: machines")).toHaveValue("2")
  // Homelab on a site with one node: the routed network, with its vnet named.
  await page.getByRole("radio", { name: /^Homelab/ }).check()
  await expect(page.getByRole("list", { name: "What the preset changed" })).toHaveCount(0)
  await expect(page.getByRole("radio", { name: /Routed by the node/ })).toBeChecked()
  await expect(page.getByLabel("Vnet name", { exact: true })).toHaveValue("pcplat0")
  await fillAddresses(page, "")
  await page.getByRole("button", { name: "Save draft" }).click()
  await expect(page.getByText("2. Proxmox identities")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect(created.body.spec.network).toEqual({ mode: "routed", bridge: "pcplat0", prefix: "10.90.0.0/24", gateway: "10.90.0.1", dns: ["10.90.0.1"] })
  // 24 GiB with the control plane's spare on a node with 24 free: the smallest preset installs on the smallest site.
  // The groups' spares (36 GiB) do not fit, which the review says and the Check warns about.
  const load = page.getByRole("list", { name: "Cores and memory per node" })
  await expect(load).toContainText("24 GiB with the one more machine an update of the control plane makes, 36 GiB with one more of each group as well. The node has 24 GiB free of 32 GiB. That holds the install")
  await expect(load.getByText("That is not enough.")).toHaveCount(0)
  await storeSecrets(page)
  await page.getByRole("button", { name: "Check", exact: true }).click()
  await expect(page.getByRole("status").filter({ hasText: "No check failed. 2 warnings do not stop the install" })).toBeVisible() // the capacity row and the scheduler row
  await expect(page.getByText("Node pve1 holds the machines (24 GiB of 24 GiB), and not the spare machine of every group on it as well (12 GiB more)")).toBeVisible()
  // One machine more in a group no longer fits: the review says it, and the Check refuses it.
  await page.getByLabel("Group system: machines").fill("2")
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect(load).toContainText("28 GiB with the one more machine an update of the control plane makes, 40 GiB with one more of each group as well. The node has 24 GiB free of 32 GiB. That is not enough.")
  await page.getByRole("button", { name: "Check again", exact: true }).click()
  await expect(page.getByRole("status").filter({ hasText: "1 of " })).toContainText("checks failed")
  await expect(page.getByText("Node pve1: the settings place 28 GiB of memory there, and 24 GiB can be reserved.")).toBeVisible()
  await expect(page.getByRole("list", { name: "What to do for each failed check" })).toContainText("one control plane machine in place of three")
  await expectAccessible(page)
})

test("Custom fills nothing, and a release that builds one control plane machine does not offer three", async ({ page }) => {
  await open(page)
  await expect(page.getByRole("checkbox", { name: "Group system: node pve1" })).toBeVisible()
  await page.getByLabel("Group system: machines").fill("5")
  await page.getByRole("radio", { name: /^Standard/ }).check()
  await page.getByRole("radio", { name: /^Custom/ }).check()
  await expect(page.getByLabel("Group system: machines")).toHaveValue("2") // Custom changes nothing: Standard's fill stays
  await expect(page.getByLabel("Control plane machines", { exact: true })).toHaveValue("3")

  // A release whose cluster chart builds one machine: Standard fills one and says why.
  await open(page, "onechart")
  await expect(page.getByRole("checkbox", { name: "Group system: node pve1" })).toBeVisible()
  await page.getByRole("radio", { name: /^Standard/ }).check()
  await expect(page.getByLabel("Control plane machines", { exact: true })).toHaveValue("1")
  await expect(page.getByRole("list", { name: "What the preset changed" })).toContainText("the release this install runs with builds one")

  // A platform installed by the release before this one, whose cluster chart builds one machine.
  await open(page, "previous")
  const count = page.getByLabel("Control plane machines", { exact: true })
  await expect(count).toHaveValue("1")
  await expect(page.getByText(/This release builds one: three need a release whose cluster chart is 0\.1\.14 or newer\./)).toBeVisible()
  await expect(count.getByRole("option", { name: "3, on this one node" })).toBeDisabled()
  // Installed: a preset would resize and replace every machine, so none is offered.
  for (const name of [/^Homelab/, /^Standard/, /^Custom/]) await expect(page.getByRole("radio", { name })).toBeDisabled()
  await expect(page.getByText("A preset is a start for a new installation.")).toBeVisible()
  // The control plane's node is fixed with the install (the API answers 409 fixed_fields): another node is every machine anew.
  await expect(page.getByLabel("Control plane node", { exact: true })).toBeDisabled()
  // The API refuses three for this platform at the save, for a client that sends it anyway.
  const answer = await page.evaluate(async (paths) => {
    const { dispatch } = await import(/* @vite-ignore */ paths.router)
    const { mockPlatform } = await import(/* @vite-ignore */ paths.state)
    const in_ = mockPlatform.installations[0]
    const spec = structuredClone(in_.spec)
    spec.clusters[0].controlPlane.replicas = 3
    return dispatch(`/admin/platform/installations/${in_.id}`, { method: "PUT", body: { name: in_.name, rev: in_.rev, spec } })
      .then(() => "ok", (e: { status: number; message: string }) => `${e.status} ${e.message}`)
  }, MOCK)
  expect(answer).toMatch(/^400 /)
})

test("an installed control plane grows from one machine to three, and does not go back once a run built three", async ({ page }) => {
  await open(page, "ready")
  await recordCalls(page)
  const count = page.getByLabel("Control plane machines", { exact: true })
  const puts = async () => (await calls(page, "PUT", /\/installations\/pi-/)).map((c) => c.body.spec.clusters[0].controlPlane.replicas ?? 1)
  await expect(count).toHaveValue("1")
  await count.selectOption("3")
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect.poll(puts).toEqual([3])
  await expect(page.getByText("3 control plane machines on pve1 and 3 worker machines in the pool")).toBeVisible()
  // Saved and not applied: the platform still has one machine, so the count goes back to one.
  await expect(count.getByRole("option", { name: "1", exact: true })).toBeEnabled()
  await expect(page.getByText("so it stays at three.")).toHaveCount(0)
  await count.selectOption("1")
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect.poll(puts).toEqual([3, 1])
  await expect(page.getByText("1 control plane machine and 3 worker machines in the pool")).toBeVisible()
  // An apply hands three to the cluster and records it in the status before the sync. The mock has no apply of a
  // settings change, so the record is set here; the next save reads it.
  await page.evaluate(async (paths) => {
    const { mockPlatform } = await import(/* @vite-ignore */ paths.state)
    mockPlatform.installations[0].status.clusters[0].controlPlaneReplicas = 3
  }, MOCK)
  await count.selectOption("3")
  await page.getByRole("button", { name: "Save settings" }).click()
  await expect.poll(puts).toEqual([3, 1, 3])
  await expect(page.getByText("3 control plane machines on pve1 and 3 worker machines in the pool")).toBeVisible() // the save is done
  await expect(count.getByRole("option", { name: "1", exact: true })).toBeDisabled()
  await expect(page.getByText("A run built this platform with three, so it stays at three.")).toBeVisible()
  // The API's own refusal, for a client that sends it anyway.
  const answer = await page.evaluate(async (paths) => {
    const { dispatch } = await import(/* @vite-ignore */ paths.router)
    const { mockPlatform } = await import(/* @vite-ignore */ paths.state)
    const in_ = mockPlatform.installations[0]
    const spec = structuredClone(in_.spec)
    spec.clusters[0].controlPlane.replicas = 1
    return dispatch(`/admin/platform/installations/${in_.id}`, { method: "PUT", body: { name: in_.name, rev: in_.rev, spec } })
      .then(() => "ok", (e: { status: number; message: string }) => `${e.status} ${e.message}`)
  }, MOCK)
  expect(answer).toMatch(/^409 .*does not go from 3 machines to 1 in this release/)
})
