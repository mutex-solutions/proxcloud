// The Sandboxes block of the Managed services page (/platform) against the mock API (src/mocks/handlers/platform.ts):
// this release's BOM has the sandbox chart, so the settings offer sandboxes (?mock-platform=sandboxes, or no preset:
// nothing saved); nosandboxes is a release from before the offer, previous a platform such a release installed;
// sandboxready is a ready platform with a shared sandbox group and its disabled sandbox provider.
import { AxeBuilder } from "@axe-core/playwright"
import { expect, test, type Page } from "@playwright/test"
import { calls, recordCalls } from "./mock-calls.ts"

async function expectAccessible(page: Page) {
  await expect(page.locator("[data-sonner-toast]")).toHaveCount(0, { timeout: 10_000 })
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
    .exclude('button[class*="bg-primary"]')
    .exclude('a[class*="bg-primary"]')
    .analyze()
  expect(results.violations.map((v) => ({ id: v.id, nodes: v.nodes.slice(0, 5).map((n) => n.target.join(" ")) }))).toEqual([])
}

async function open(page: Page, preset = "") {
  await page.emulateMedia({ reducedMotion: "reduce" })
  await page.goto(`/platform${preset ? `?mock-platform=${preset}` : ""}`)
  await expect(page.getByRole("heading", { name: "Managed services", exact: true })).toBeVisible()
}

const fill = (page: Page, label: string, value: string) => page.getByLabel(label, { exact: true }).fill(value)

async function fillSettings(page: Page) {
  await fill(page, "Name", "lab")
  for (const g of ["system", "kamaji", "dbaas"]) await page.getByRole("checkbox", { name: `Group ${g}: node pve1` }).check()
  for (const [label, value] of [
    ["Bridge", "vmbr1"], ["Network", "10.90.0.0/24"], ["Gateway", "10.90.0.1"], ["DNS servers", "10.90.0.1"], ["API address", "10.90.0.10"],
    ["Node range", "10.90.0.20-10.90.0.60"], ["Load balancer range", "10.90.0.100-10.90.0.150"], ["Proxmox API URL", "https://10.90.0.2:8006"],
    ["Seed node", "pve1"], ["Seed address", "10.90.0.5"], ["Seed API sources", "192.0.2.10/32"], ["Control plane node", "pve1"], ["Boot store", "local-zfs"],
  ]) await fill(page, label, value)
}

const save = (page: Page) => page.getByRole("button", { name: /^Save (draft|settings)$/ }).click()
const givesUp = (page: Page) => page.getByRole("note", { name: "What a shared node group gives up" })

test("this release offers sandboxes: off until the operator chooses, in a new installation and on a ready platform", async ({ page }) => {
  for (const preset of ["", "ready"]) {
    await open(page, preset)
    await expect(page.getByRole("heading", { name: "Sandboxes", exact: true })).toBeVisible()
    await expect(page.getByRole("radio", { name: /^Off\./ })).toBeChecked()
    await expect(page.getByRole("radio", { name: /^Shared node group\./ })).toBeEnabled()
    await expect(page.getByRole("radio", { name: /^Separate sandbox cluster\./ })).toBeEnabled()
    await expect(page.getByLabel("Sandbox range", { exact: true })).toHaveCount(0)
  }
})

test("a release without the sandbox chart, and a platform that one installed, offer no sandboxes", async ({ page }) => {
  // previous: the platform stays on the BOM that installed it until its upgrade, and that BOM builds no sandboxes.
  for (const preset of ["nosandboxes", "previous"]) {
    await open(page, preset)
    await expect(page.getByRole("heading", { name: "Node groups" })).toBeVisible()
    await expect(page.getByRole("heading", { name: "Sandboxes", exact: true })).toHaveCount(0)
    await expect(page.getByRole("radio", { name: /Shared node group/ })).toHaveCount(0)
  }
})

test("a shared node group: the machines, the range, what it gives up in full, and the tick", async ({ page }) => {
  await open(page, "sandboxes")
  await recordCalls(page)
  await fillSettings(page)
  await expect(page.getByRole("heading", { name: "Sandboxes", exact: true })).toBeVisible()
  // Off is where a new installation starts: no machine, no field, and nothing of a sandbox is sent.
  await expect(page.getByRole("radio", { name: /^Off\./ })).toBeChecked()
  await expect(page.getByLabel("Sandbox range", { exact: true })).toHaveCount(0)
  await expect(givesUp(page)).toHaveCount(0)

  await page.getByRole("radio", { name: /^Shared node group\./ }).check()
  // The losses are on the page as text, every one, before anything can be saved.
  const note = givesUp(page)
  await expect(note.getByRole("listitem")).toHaveCount(7)
  await expect(note).toContainText("root on a machine of the platform cluster")
  await expect(note).toContainText("kamaji-etcd")
  await expect(note).toContainText("A forged one pulls another node's pod traffic")
  await expect(note).toContainText("abuse from a sandbox leaves under your site's public address")
  await expect(note).toContainText("A separate cluster limits the same escape")
  await expect(page.getByLabel("Sandboxes and the job scheduler")).toContainText("also starts charging usage")
  await expect(page.getByRole("group", { name: "Group sandbox-system" })).toHaveCount(0)

  await page.getByRole("checkbox", { name: "Group sandbox: node pve1" }).check()
  await page.getByRole("group", { name: "Group sandbox", exact: true }).getByLabel("Group sandbox: Cores").fill("2")
  await expect(page.getByRole("group", { name: "Group sandbox", exact: true })).toContainText("A machine of 2 cores holds ONE sandbox")
  await fill(page, "Sandbox range", "10.90.0.64-10.90.0.79") // first-last, as the node range is written
  await fill(page, "Proxy address", "10.90.0.90") // outside the load balancer range
  await fill(page, "Sandbox domain", "sbx")
  await save(page)
  await expect(page.getByText("Sandbox range: one network inside 10.90.0.0/24, from /25 to /30, written as a CIDR").first()).toBeVisible()
  await expect(page.getByText("Proxy address: with a shared node group, a free address inside the load balancer range.").first()).toBeVisible()
  await expect(page.getByText("Sandbox domain: a DNS name such as sbx.example.org").first()).toBeVisible()
  await expect(page.getByText("Tick the box under the text on what a shared node group gives up").first()).toBeVisible()
  await expect(page.getByLabel("Sandbox range", { exact: true })).toHaveAttribute("aria-invalid", "true")
  expect(await calls(page, "POST", "/admin/platform/installations")).toHaveLength(0)

  await fill(page, "Sandbox range", "10.90.0.32/28") // inside the node range
  await expect(page.getByText("Sandbox range: it overlaps the node range.").first()).toBeVisible()
  await fill(page, "Sandbox range", "10.90.0.64/28")
  await fill(page, "Proxy address", "10.90.0.140")
  await fill(page, "Sandbox domain", "sbx.example.org")
  await save(page)
  expect(await calls(page, "POST", "/admin/platform/installations"), "not saved without the tick").toHaveLength(0)
  await expectAccessible(page)
  await page.getByRole("checkbox", { name: /I have read this/ }).check()
  await save(page)
  await expect(page.getByText("Settings saved")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect(created.body.spec.sandbox).toEqual({
    placement: "shared", nodes: ["pve1"], count: 1, cores: 2, memoryMiB: 8192, diskGiB: 40,
    nodeRange: "10.90.0.64/28", proxyAddress: "10.90.0.140", domain: "sbx.example.org", tls: { mode: "selfsigned" },
  })
  // What is stored reads back as chosen and ticked.
  await expect(page.getByRole("radio", { name: /^Shared node group\./ })).toBeChecked()
  await expect(page.getByRole("checkbox", { name: /I have read this/ })).toBeChecked()
})

test("a separate cluster: a system group of its own, a proxy address outside every range, and no tick", async ({ page }) => {
  await open(page, "sandboxes")
  await recordCalls(page)
  await fillSettings(page)
  await page.getByRole("radio", { name: /^Separate sandbox cluster\./ }).check()
  await expect(givesUp(page)).toHaveCount(0)
  await expect(page.getByRole("group", { name: "Group sandbox-system" })).toBeVisible()
  await page.getByRole("checkbox", { name: "Group sandbox: node pve1" }).check()
  await page.getByRole("checkbox", { name: "Group sandbox-system: node pve1" }).check()
  await fill(page, "Sandbox range", "10.90.0.64/28")
  await fill(page, "Proxy address", "10.90.0.140") // the load balancer range is the platform cluster's
  await fill(page, "Sandbox domain", "sbx.example.org")
  await fill(page, "DNS servers of the sandbox machines (optional)", "1.1.1.1, 9.9.9.9")
  await save(page)
  await expect(page.getByText("Proxy address: with a separate cluster, a free host address of 10.90.0.0/24 outside the node range, the load balancer range and the sandbox range.").first()).toBeVisible()
  await fill(page, "Proxy address", "10.90.0.90")
  await save(page)
  await expect(page.getByText("Settings saved")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect(created.body.spec.sandbox).toEqual({
    placement: "separate", nodes: ["pve1"], count: 1, cores: 4, memoryMiB: 8192, diskGiB: 40,
    nodeRange: "10.90.0.64/28", proxyAddress: "10.90.0.90", domain: "sbx.example.org", tls: { mode: "selfsigned" }, dns: ["1.1.1.1", "9.9.9.9"],
    system: { nodes: ["pve1"], count: 1, cores: 2, memoryMiB: 4096, diskGiB: 30 },
  })
})

test("a preset fills the sandbox machines once sandboxes are switched on, and never switches them on", async ({ page }) => {
  await open(page, "sandboxes")
  await fill(page, "Seed node", "pve1")
  await page.getByRole("radio", { name: /^Standard/ }).check()
  await expect(page.getByRole("radio", { name: /^Off\./ })).toBeChecked()
  await page.getByRole("radio", { name: /^Separate sandbox cluster\./ }).check()
  await page.getByRole("radio", { name: /^Homelab/ }).check()
  // Homelab: a shared group of one small machine, and the tick is asked for again.
  await expect(page.getByRole("radio", { name: /^Shared node group\./ })).toBeChecked()
  await expect(page.getByRole("checkbox", { name: /I have read this/ })).not.toBeChecked()
  const group = page.getByRole("group", { name: "Group sandbox", exact: true })
  await expect(group.getByLabel("Group sandbox: machines")).toHaveValue("1")
  await expect(group.getByLabel("Group sandbox: Cores")).toHaveValue("2")
  await expect(group.getByRole("checkbox", { name: "Group sandbox: node pve1" })).toBeChecked()
  await expect(page.getByText("which holds ONE sandbox of one vCPU")).toBeVisible()
  await page.getByRole("radio", { name: /^Standard/ }).check()
  await expect(page.getByRole("radio", { name: /^Separate sandbox cluster\./ })).toBeChecked()
  await expect(group.getByLabel("Group sandbox: machines")).toHaveValue("2")
  await expect(page.getByRole("group", { name: "Group sandbox-system" }).getByLabel("Group sandbox-system: machines")).toHaveValue("1")
})

test("ready: the sandbox provider, its release gates, the certificate, and what is fixed", async ({ page }) => {
  await open(page, "sandboxready")
  await recordCalls(page)
  const item = page.getByRole("listitem", { name: "Sandbox provider" })
  await expect(item.getByRole("link", { name: "lab Sandboxes" })).toHaveAttribute("href", "/system/cloud-providers/svc-sandbox-01")
  await expect(item).toContainText("Disabled: no tenant can start a sandbox yet. A run never sets it live.")
  await expect(item).toContainText("Reduced isolation: sandboxes run on a shared node group of the platform cluster.")
  await expect(item).toContainText("The job scheduler is off, so this provider cannot be set live")

  // The gates are read, never set: the first check finds the machine not fenced yet.
  await item.getByRole("button", { name: "Check the release gates" }).click()
  const gates = item.getByRole("list", { name: "Release gates" })
  await expect(gates.getByRole("listitem")).toHaveCount(4)
  await expect(gates.getByRole("listitem").filter({ hasText: "VM firewall of the sandbox machines" })).toContainText("no worker is open to sandboxes yet")
  await expect(item.getByRole("status")).toContainText("1 of 4 gates do not hold")
  await item.getByRole("button", { name: "Check the release gates" }).click()
  await expect(item.getByRole("status")).toContainText("Every gate holds. Set the provider live on its page")
  expect(await calls(page, "POST", "/admin/service/svc-sandbox-01/sandbox/release-check")).toHaveLength(2)
  expect(await calls(page, "PUT", /\/admin\/service\//), "the page never sets the provider live").toHaveLength(0)

  const download = page.waitForEvent("download")
  await item.getByRole("button", { name: "Download the certificate" }).click()
  expect((await download).suggestedFilename()).toBe("lab-sandbox.crt")
  await expectAccessible(page)

  // What a run built: the placement goes only to Off, the range and the proxy address stay.
  await expect(page.getByRole("radio", { name: /^Shared node group\./ })).toBeChecked()
  await expect(page.getByRole("radio", { name: /^Separate sandbox cluster\./ })).toBeDisabled()
  await expect(page.getByLabel("Sandbox range", { exact: true })).toBeDisabled()
  await expect(page.getByLabel("Proxy address", { exact: true })).toBeDisabled()
  await expect(page.getByRole("radio", { name: /^Off\./ })).toBeEnabled()
})

test("off is refused while the sandbox provider is live, and saved once it is disabled", async ({ page }) => {
  await open(page, "sandboxready")
  await recordCalls(page)
  // Another session set the provider live.
  await page.evaluate(async () => {
    const { db } = await import(/* @vite-ignore */ "/src/mocks/db.ts" as string)
    db.services.find((s: { id: string }) => s.id === "svc-sandbox-01").status = "PUBLIC"
  })
  await page.getByRole("radio", { name: /^Off\./ }).check()
  await expect(page.getByLabel("Sandbox range", { exact: true })).toHaveCount(0)
  await save(page)
  const refused = page.getByRole("alert").filter({ hasText: "The settings were not saved" })
  await expect(refused).toContainText('The sandbox provider "lab Sandboxes" is PUBLIC')
  await expect(refused).toContainText("Set the provider DISABLED under Cloud providers")
  await page.evaluate(async () => {
    const { db } = await import(/* @vite-ignore */ "/src/mocks/db.ts" as string)
    db.services.find((s: { id: string }) => s.id === "svc-sandbox-01").status = "DISABLED"
  })
  await save(page)
  await expect(page.getByText("Settings saved")).toBeVisible()
  const puts = await calls(page, "PUT", /\/admin\/platform\/installations\/pi-/)
  expect(puts.at(-1)!.body.spec.sandbox).toEqual({ placement: "off" })
})
