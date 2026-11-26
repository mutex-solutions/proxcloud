// Functions in the operator console, against the mock API (src/mocks/handlers/platform.ts): the Functions tick under
// Sandboxes in the install settings, the Functions group and the function gates of the sandbox provider, and a
// project's function quota. Presets: functions (nothing saved, the BOM pins sandbox chart 0.2.0), functionsready (a
// shared group with the function side built, functions off on the provider), functionson (on, and a project attached).
// Under every other preset the BOM pins chart 0.1.0, as every BOM of this release does: nothing of functions is on
// any page and nothing of them is sent. The first tests are the forms' own rules, called in the page.
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

// The settings of a new installation with a sandbox group of the placement asked for, everything valid.
async function fillSettings(page: Page, placement: "shared" | "separate") {
  await fill(page, "Name", "lab")
  for (const g of ["system", "kamaji", "dbaas"]) await page.getByRole("checkbox", { name: `Group ${g}: node pve1` }).check()
  for (const [label, value] of [
    ["Bridge", "vmbr1"], ["Network", "10.90.0.0/24"], ["Gateway", "10.90.0.1"], ["DNS servers", "10.90.0.1"], ["API address", "10.90.0.10"],
    ["Node range", "10.90.0.20-10.90.0.60"], ["Load balancer range", "10.90.0.100-10.90.0.150"], ["Proxmox API URL", "https://10.90.0.2:8006"],
    ["Seed node", "pve1"], ["Seed address", "10.90.0.5"], ["Seed API sources", "192.0.2.10/32"], ["Control plane node", "pve1"], ["Boot store", "local-zfs"],
  ]) await fill(page, label, value)
  await page.getByRole("radio", { name: placement === "shared" ? /^Shared node group\./ : /^Separate sandbox cluster\./ }).check()
  await page.getByRole("checkbox", { name: "Group sandbox: node pve1" }).check()
  if (placement === "separate") await page.getByRole("checkbox", { name: "Group sandbox-system: node pve1" }).check()
  await fill(page, "Sandbox range", "10.90.0.64/28")
  await fill(page, "Proxy address", placement === "shared" ? "10.90.0.140" : "10.90.0.90")
  await fill(page, "Sandbox domain", "sbx.example.org")
}

const save = (page: Page) => page.getByRole("button", { name: /^Save (draft|settings)$/ }).click()
const adds = (page: Page) => page.getByRole("note", { name: "What functions add" })
const tick = (page: Page) => page.getByRole("checkbox", { name: /^Functions: I have read this/ })
const MODEL = "/src/pages/platform/model.ts", PROVIDER = "/src/pages/system/sandboxProvider.tsx"

// ── The forms' rules ──────────────────────────────────────────────────────────

test("provider form: the functions block is refused for each reason the API would make functions unavailable", async ({ page }) => {
  await open(page)
  const cases: Array<[Record<string, unknown>, string]> = [
    [{}, ""], // every number empty: the API's defaults
    [{ fnIdleSeconds: "0", fnMaxInstances: "0" }, ""], // 0 is the default too
    [{ fnMinIdleSeconds: "59" }, "Shortest idle time a function may set (s): at least 60."],
    [{ fnIdleSeconds: "30" }, "The default idle time must be between the shortest and the longest idle time."],
    [{ fnIdleSeconds: "4000" }, "The default idle time must be between the shortest and the longest idle time."],
    [{ fnIdleSeconds: "4000", fnMaxIdleSeconds: "7200" }, ""],
    [{ fnMaxCpuMillis: "249" }, "Max CPU per instance (millicores): at least 250."],
    [{ fnMaxMemoryMi: "255" }, "Max memory per instance (Mi): at least 256."],
    [{ fnMaxInstances: "100" }, ""],
    [{ fnMaxInstances: "101" }, "Most instances a function may scale to: at most 100."],
    [{ fnMaxFunctions: "1.5" }, "Functions per project, whatever its quota: a whole number, or empty for the default."],
    [{ fnQuotaVcpus: "-1" }, "vCPU: a whole number, or empty for the default."],
    [{ domain: "" }, "Functions need the sandbox domain: every function is served at <name>-<6 hex>.<sandbox domain>."],
    [{ domain: "Sbx.example.org" }, "Functions need the sandbox domain: every function is served at <name>-<6 hex>.<sandbox domain>."],
    // A block that is off is not checked by the API, so the form saves it: only a number must be a number.
    [{ fnEnabled: false, fnMinIdleSeconds: "5", domain: "" }, ""],
    [{ fnEnabled: false, fnMinIdleSeconds: "x" }, "Shortest idle time a function may set (s): a whole number, or empty for the default."],
  ]
  const got = await page.evaluate(async ([path, cs]) => {
    const m = await import(/* @vite-ignore */ path as string)
    const valid = { ...m.emptySandboxForm, name: "sbx", region: "az1", proxmoxServiceId: "svc-proxmox-01", kubeconfig: "k", templates: "base=img 1 1", domain: "sbx.example.org", fnEnabled: true }
    return (cs as Array<[Record<string, unknown>, string]>).map(([over]) => {
      const f = { ...valid, ...over }
      return [m.functionsFormError(f), m.sandboxFormValid(f)]
    })
  }, [PROVIDER, cases] as const)
  cases.forEach(([over, want], i) => {
    expect(got[i][0], JSON.stringify(over)).toBe(want)
    expect(got[i][1], `Save is offered only without an error: ${JSON.stringify(over)}`).toBe(want === "")
  })
})

test("provider form: no functions key unless one is stored or the form names one, and stored keys the form does not show stay", async ({ page }) => {
  await open(page)
  const got = await page.evaluate(async (path) => {
    const m = await import(/* @vite-ignore */ path)
    const svc = (functions?: unknown) => ({ name: "sbx", status: "DISABLED", config: { region: "az1", platformInstallationId: "pi-1", sandbox: { domain: "sbx.example.org", ...(functions ? { functions } : {}) } } })
    const block = (f: unknown) => m.sandboxConfigBlocks(f).sandbox.functions
    const untouched = m.sandboxFormFromService(svc())
    const stored = m.sandboxFormFromService(svc({ enabled: true, minInstances: 1, maxMinInstances: 1, idleSeconds: 120, maxInstances: 3, projectQuota: { functions: 10, vcpus: 4 }, somethingLater: "kept" }))
    return {
      untouchedHasKey: "functions" in m.sandboxConfigBlocks(untouched).sandbox,
      emptyHasKey: "functions" in m.sandboxConfigBlocks(m.emptySandboxForm).sandbox,
      installationId: untouched.installationId,
      switchedOn: block({ ...untouched, fnEnabled: true, fnMaxMinInstances: "1", fnMaxInstances: "8", fnQuotaRamMb: "4096" }),
      storedForm: [stored.fnEnabled, stored.fnMinInstances, stored.fnMaxMinInstances, stored.fnIdleSeconds, stored.fnMaxInstances, stored.fnMaxCpuMillis, stored.fnQuotaFunctions, stored.fnQuotaVcpus, stored.fnQuotaRamMb],
      storedBack: block(stored),
      storedCleared: block({ ...stored, fnEnabled: false, fnMinInstances: "0", fnIdleSeconds: "", fnQuotaFunctions: "", fnQuotaVcpus: "" }),
    }
  }, PROVIDER)
  expect(got.untouchedHasKey, "a provider without functions is saved as before").toBe(false)
  expect(got.emptyHasKey, "a new provider is created as before").toBe(false)
  expect(got.installationId).toBe("pi-1")
  expect(got.switchedOn).toEqual({ enabled: true, minInstances: 0, maxMinInstances: 1, maxInstances: 8, projectQuota: { ramMb: 4096 } })
  expect(got.storedForm).toEqual([true, "1", "1", "120", "3", "", "10", "4", ""])
  expect(got.storedBack).toEqual({ enabled: true, minInstances: 1, maxMinInstances: 1, idleSeconds: 120, maxInstances: 3, projectQuota: { functions: 10, vcpus: 4 }, somethingLater: "kept" })
  // Off is sent as off, an emptied number leaves the block (the API's default), an emptied quota leaves it whole.
  expect(got.storedCleared).toEqual({ enabled: false, minInstances: 0, maxMinInstances: 1, maxInstances: 3, somethingLater: "kept" })
})

test("install settings: which BOM offers functions, what is sent, and what the form refuses", async ({ page }) => {
  await open(page)
  const got = await page.evaluate(async (path) => {
    const m = await import(/* @vite-ignore */ path)
    const bom = (version?: string) => ({ components: [{ name: "pc-platform", cluster: "seed", chart: { version: "0.1.14" } }, ...(version ? [{ name: "sandbox", cluster: "workload", feature: "sandbox", chart: { version } }] : [])] })
    const sandbox = { ...m.emptyForm.sandbox, placement: "shared", accept: true, machines: { ...m.emptyForm.sandbox.machines, nodes: ["pve1"] }, nodeRange: "10.90.0.64/28", proxyAddress: "10.90.0.140", domain: "sbx.example.org" }
    const form = (functions: Record<string, unknown>) => ({ ...m.emptyForm, prefix: "10.90.0.0/24", metallbRange: "10.90.0.100-10.90.0.150", sandbox: { ...sandbox, functions: { ...sandbox.functions, ...functions } } })
    const sent = (functions: Record<string, unknown>) => m.formToSpec(form(functions)).sandbox
    const refused = (functions: Record<string, unknown>) => m.validate(form({ enabled: true, ...functions })).filter((p: { field: string }) => p.field.startsWith("sbxFn")).map((p: { field: string }) => p.field)
    const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"
    return {
      offers: ["", "0.1.0", "0.1.9", "0.2.0", "v0.2.0", "0.2.0-rc.1", "0.10.0", "1.0.0", "main"].map((v) => [v, m.bomOffersFunctions(bom(v || undefined))]),
      three: m.bomBuildsThree(bom()),
      untouched: "functions" in sent({}),
      on: sent({ enabled: true, portalURL: " https://cloud.example.org ", portalCA: `\n${pem}\n` }).functions,
      onNoCA: sent({ enabled: true, portalURL: "https://cloud.example.org" }).functions,
      storedOff: sent({ enabled: false, stored: true, portalURL: "https://cloud.example.org" }).functions,
      readBack: [undefined, { enabled: false }, { enabled: true, portalURL: "https://p.example", portalCA: pem }].map((functions) =>
        m.specToForm("lab", { sandbox: { placement: "shared", ...(functions ? { functions } : {}) } }).sandbox.functions),
      offIsNotChecked: m.validate(form({ enabled: false, portalURL: "nonsense" })).some((p: { field: string }) => p.field.startsWith("sbxFn")),
      urls: ["https://cloud.example.org", "https://cloud.example.org/", "https://10.90.0.2:8443", "", "cloud.example.org", "http://cloud.example.org", "https://cloud.example.org/api", "https://cloud.example.org?x=1", "https://user:pw@cloud.example.org"]
        .map((portalURL) => [portalURL, refused({ portalURL })]),
      cas: ["", pem, `${pem}\n${pem}`, "MIIB", "-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----"].map((portalCA) => refused({ portalURL: "https://cloud.example.org", portalCA })),
      shared: m.functionsGiveUp("shared"), separate: m.functionsGiveUp("separate"), pem,
    }
  }, MODEL)
  // The first chart with the function objects is 0.2.0: every BOM on main pins an older one.
  expect(got.offers).toEqual([["", false], ["0.1.0", false], ["0.1.9", false], ["0.2.0", true], ["v0.2.0", true], ["0.2.0-rc.1", true], ["0.10.0", true], ["1.0.0", true], ["main", false]])
  expect(got.three, "the cluster chart's own rule is unchanged").toBe(true)
  expect(got.untouched, "no tick and nothing stored: the block is the one of a release without functions").toBe(false)
  expect(got.on).toEqual({ enabled: true, portalURL: "https://cloud.example.org", portalCA: got.pem })
  expect(got.onNoCA).toEqual({ enabled: true, portalURL: "https://cloud.example.org" })
  expect(got.storedOff, "a stored block is switched off in words: a spec without it would keep it on").toEqual({ enabled: false })
  expect(got.readBack).toEqual([
    { enabled: false, portalURL: "", portalCA: "", stored: false },
    { enabled: false, portalURL: "", portalCA: "", stored: true },
    { enabled: true, portalURL: "https://p.example", portalCA: got.pem, stored: true },
  ])
  expect(got.offIsNotChecked).toBe(false)
  expect(got.urls).toEqual([
    ["https://cloud.example.org", []], ["https://cloud.example.org/", []], ["https://10.90.0.2:8443", []],
    ["", ["sbxFnUrl"]], ["cloud.example.org", ["sbxFnUrl"]], ["http://cloud.example.org", ["sbxFnUrl"]], ["https://cloud.example.org/api", ["sbxFnUrl"]],
    ["https://cloud.example.org?x=1", ["sbxFnUrl"]], ["https://user:pw@cloud.example.org", ["sbxFnUrl"]],
  ])
  expect(got.cas).toEqual([[], [], [], ["sbxFnCa"], ["sbxFnCa"]])
  // The three losses the text must name, in each placement's own words.
  for (const lines of [got.shared, got.separate]) {
    expect(lines.join(" ")).toContain("is not encrypted")
    expect(lines.join(" ")).toContain("Callers cost the tenant money")
    expect(lines.join(" ")).toContain("the registry passwords of every function that runs there")
  }
  expect(got.shared[0]).toContain("On a shared node group")
  expect(got.shared[1]).toContain("the platform cluster's etcd")
  expect(got.separate[0]).toContain("A separate cluster keeps an escape away from the platform cluster, but not from other tenants' functions")
  expect(got.separate[1]).toContain("the Kamaji datastore on the platform cluster")
})

// ── The install settings ──────────────────────────────────────────────────────

test("a BOM that pins sandbox chart 0.1.0 offers nothing of functions, on any page", async ({ page }) => {
  for (const preset of ["sandboxes", "sandboxready"]) {
    await open(page, preset)
    if (preset === "sandboxes") await page.getByRole("radio", { name: /^Shared node group\./ }).check()
    await expect(page.getByRole("note", { name: "What a shared node group gives up" })).toBeVisible()
    await expect(adds(page)).toHaveCount(0)
    await expect(tick(page)).toHaveCount(0)
    await expect(page.getByLabel("Portal URL", { exact: true })).toHaveCount(0)
  }
  // The sandbox provider such a platform built: no Functions group, no function gate, and a save without the key.
  await page.goto("/system/cloud-providers/svc-sandbox-01?mock-platform=sandboxready")
  await expect(page.getByLabel("Sandbox domain", { exact: true })).toHaveValue("sbx.example.org")
  await recordCalls(page)
  await expect(page.getByRole("group", { name: "Functions" })).toHaveCount(0)
  await page.getByRole("button", { name: "Check release gates" }).click()
  await expect(page.getByRole("list", { name: "Release gates" }).getByRole("listitem")).toHaveCount(4)
  await expect(page.getByRole("list", { name: "Function gates" })).toHaveCount(0)
  await page.getByRole("button", { name: "Save", exact: true }).click()
  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-sandbox-01")).toHaveLength(1)
  expect("functions" in (await calls(page, "PUT", "/admin/service/svc-sandbox-01"))[0].body.config.sandbox).toBe(false)
  // And a project: no function quota.
  await page.goto("/clients/projects/prj-0001?tab=quota&mock-platform=sandboxready")
  await expect(page.getByRole("heading", { name: "Resource quota" })).toBeVisible()
  await expect(page.getByRole("heading", { name: "Function quota" })).toHaveCount(0)
})

test("shared: what functions add is on the page in full before the tick, and the tick sends the portal URL and CA", async ({ page }) => {
  await open(page, "functions")
  await recordCalls(page)
  await expect(adds(page), "nothing of functions while sandboxes are off").toHaveCount(0)
  await fillSettings(page, "shared")

  // Every loss is text on the page while the tick is still off, and it comes before the tick in the reading order.
  const note = adds(page)
  await expect(tick(page)).not.toBeChecked()
  await expect(note.getByRole("listitem")).toHaveCount(5)
  await expect(note).toContainText("On a shared node group, code that escapes a sandbox or a function (a gVisor escape) also reads the secrets of other tenants' functions")
  await expect(note).toContainText("a plain Kubernetes Secret in the platform cluster's etcd, and this release sets up no encryption for it")
  await expect(note).toContainText("anyone who reaches the proxy address keeps a function awake")
  await expect(note).toContainText("the tenant is billed for every minute")
  await expect(note).toContainText("What it adds to what a shared node group gives up")
  expect(await page.evaluate(() => {
    const items = [...document.querySelectorAll('[aria-label="What functions add"] li')]
    const box = document.getElementById("sbxFunctions")!
    return items.length > 0 && items.every((li) => !!(li.compareDocumentPosition(box) & Node.DOCUMENT_POSITION_FOLLOWING))
  }), "every line of the text precedes the tick").toBe(true)
  // The text on what a shared group gives up for sandboxes stays, with its own tick.
  await expect(page.getByRole("note", { name: "What a shared node group gives up" }).getByRole("listitem")).toHaveCount(7)
  await expect(page.getByLabel("Portal URL", { exact: true }), "no field of functions before the tick").toHaveCount(0)
  await expectAccessible(page)

  await tick(page).check()
  await fill(page, "Portal URL", "http://cloud.example.org/api")
  await fill(page, "Portal CA (PEM, optional)", "not a certificate")
  await page.getByRole("checkbox", { name: /^I have read this, and sandboxes/ }).check()
  await save(page)
  await expect(page.getByText("Portal URL: the https:// address sandbox-proxy reaches this ProxCloud at").first()).toBeVisible()
  await expect(page.getByText("Portal CA: one or more PEM certificates").first()).toBeVisible()
  await expect(page.getByLabel("Portal URL", { exact: true })).toHaveAttribute("aria-invalid", "true")
  await expect(page.getByLabel("Portal CA (PEM, optional)", { exact: true })).toHaveAttribute("aria-invalid", "true")
  expect(await calls(page, "POST", "/admin/platform/installations")).toHaveLength(0)
  await expectAccessible(page)

  const pem = "-----BEGIN CERTIFICATE-----\nMIIBmockPortalCA\n-----END CERTIFICATE-----"
  await fill(page, "Portal URL", "https://cloud.example.org")
  await fill(page, "Portal CA (PEM, optional)", pem)
  await save(page)
  await expect(page.getByText("Settings saved")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect(created.body.spec.sandbox).toEqual({
    placement: "shared", nodes: ["pve1"], count: 1, cores: 4, memoryMiB: 8192, diskGiB: 40,
    nodeRange: "10.90.0.64/28", proxyAddress: "10.90.0.140", domain: "sbx.example.org", tls: { mode: "selfsigned" },
    functions: { enabled: true, portalURL: "https://cloud.example.org", portalCA: pem },
  })
  // What is stored reads back ticked, with its fields.
  await expect(tick(page)).toBeChecked()
  await expect(page.getByLabel("Portal URL", { exact: true })).toHaveValue("https://cloud.example.org")
})

test("separate: the text is the separate cluster's, and a tick made under one placement does not carry to the other", async ({ page }) => {
  await open(page, "functions")
  await recordCalls(page)
  await fillSettings(page, "separate")
  const note = adds(page)
  await expect(note).toContainText("A separate cluster keeps an escape away from the platform cluster, but not from other tenants' functions")
  await expect(note).toContainText("the sandbox cluster's etcd, which is the Kamaji datastore on the platform cluster")
  await expect(note).not.toContainText("On a shared node group")
  await tick(page).check()
  await fill(page, "Portal URL", "https://cloud.example.org")
  await expect(page.getByText("The sandbox machines' firewall is opened to this address.")).toBeVisible()

  // Shared has another text: the tick is asked for again under it, and nothing of functions is sent until then.
  await page.getByRole("radio", { name: /^Shared node group\./ }).check()
  await expect(note).toContainText("On a shared node group")
  await expect(tick(page)).not.toBeChecked()
  await page.getByRole("radio", { name: /^Separate sandbox cluster\./ }).check()
  await expect(tick(page)).not.toBeChecked()
  await save(page)
  await expect(page.getByText("Settings saved")).toBeVisible()
  const [created] = await calls(page, "POST", "/admin/platform/installations")
  expect("functions" in created.body.spec.sandbox, "an unticked box on a new installation sends no functions key").toBe(false)
})

test("a preset that sets the placement clears the Functions tick", async ({ page }) => {
  await open(page, "functions")
  await fill(page, "Seed node", "pve1")
  await page.getByRole("radio", { name: /^Separate sandbox cluster\./ }).check()
  await tick(page).check()
  await page.getByRole("radio", { name: /^Homelab/ }).check()
  await expect(page.getByRole("radio", { name: /^Shared node group\./ })).toBeChecked()
  await expect(adds(page)).toContainText("On a shared node group")
  await expect(tick(page)).not.toBeChecked()
})

test("built: the tick stays through Off and back, and taking it away or switching sandboxes off is refused while a function exists", async ({ page }) => {
  await open(page, "functionsready")
  await recordCalls(page)
  await expect(tick(page)).toBeChecked()
  await expect(page.getByLabel("Portal URL", { exact: true })).toHaveValue("https://cloud.example.org")
  const refused = page.getByRole("alert").filter({ hasText: "The settings were not saved" })

  // Off, then back: the text is the same one, and a cleared tick would be saved as "functions off".
  await page.getByRole("radio", { name: /^Off\./ }).check()
  await expect(adds(page)).toHaveCount(0)
  await save(page)
  await expect(refused).toContainText("Functions are on for this installation: take the Functions tick away and apply first")
  await page.getByRole("radio", { name: /^Shared node group\./ }).check()
  await page.getByRole("checkbox", { name: /^I have read this, and sandboxes/ }).check()
  await expect(tick(page)).toBeChecked()

  await tick(page).uncheck()
  await save(page)
  await expect(refused).toContainText("1 function of this installation's sandbox provider exists")
  const puts = await calls(page, "PUT", /\/admin\/platform\/installations\/pi-/)
  expect(puts.at(-1)!.body.spec.sandbox.functions, "off is sent in words, not by leaving the block out").toEqual({ enabled: false })
  expect(puts.at(-1)!.body.spec.sandbox.placement).toBe("shared")
})

test("ready: the function gates are apart from the release gates, and the reduced isolation mark names functions", async ({ page }) => {
  await open(page, "functionsready")
  await recordCalls(page)
  const item = page.getByRole("listitem", { name: "Sandbox provider" })
  await expect(item).toContainText("Reduced isolation: sandboxes and functions run on a shared node group of the platform cluster, and an escape there reads the secrets of other tenants' functions.")

  await item.getByRole("button", { name: "Check the release gates" }).click()
  // The provider's own gates are counted alone: a function gate never keeps the provider from going live.
  await expect(item.getByRole("list", { name: "Release gates" }).getByRole("listitem")).toHaveCount(4)
  await expect(item.getByRole("status").first()).toContainText("1 of 4 gates do not hold")
  const fn = item.getByRole("list", { name: "Function gates" })
  await expect(fn.getByRole("listitem")).toHaveCount(5)
  await expect(fn.getByRole("listitem").filter({ hasText: "A sandbox-proxy replica reported in the last 90 seconds" })).toContainText("no sandbox-proxy replica of this provider has reported")
  await expect(item.getByRole("status", { name: "Functions" })).toContainText("Functions: 1 of 5 do not hold, so functions cannot be switched on yet. This does not keep the provider from going live.")

  await item.getByRole("button", { name: "Check the release gates" }).click()
  await expect(item.getByRole("status").first()).toContainText("Every gate holds. Set the provider live on its page")
  // The price row warns and refuses nothing.
  await expect(fn.getByRole("listitem").filter({ hasText: "Price rule for functions" })).toContainText("functions run and bill nothing")
  await expect(item.getByRole("status", { name: "Functions" })).toContainText("Functions: every gate holds. Switch them on on the provider's page, in the Functions group.")
  expect(await calls(page, "PUT", /\/admin\/service\//), "the page never switches functions on").toHaveLength(0)
  await expectAccessible(page)
})

// ── The sandbox provider ──────────────────────────────────────────────────────

test("provider: the Functions group, its gates, a block the API would not take, and what a save sends", async ({ page }) => {
  await page.emulateMedia({ reducedMotion: "reduce" })
  await page.goto("/system/cloud-providers/svc-sandbox-01?mock-platform=functionsready")
  const group = page.getByRole("group", { name: "Functions" })
  await expect(group).toBeVisible()
  await recordCalls(page)
  const on = group.getByRole("switch", { name: "Offer serverless functions on this cluster" })
  await expect(on).not.toBeChecked()
  await expect(group).toContainText("Reduced isolation: this provider runs on a shared node group, where an escape reads the secrets of other tenants' functions.")
  await expect(group).toContainText("https://<name>-<6 hex>.sbx.example.org")
  await expect(group.getByLabel("Most instances a function may scale to")).toHaveAttribute("placeholder", "5")

  // The gates: the function rows are a list of their own, under the sentence that says what they decide.
  await page.getByRole("button", { name: "Check release gates" }).click()
  await expect(page.getByRole("list", { name: "Release gates" }).getByRole("listitem")).toHaveCount(4)
  const fn = page.getByRole("list", { name: "Function gates" })
  await expect(fn.getByRole("listitem")).toHaveCount(5)
  await expect(page.getByText("is accepted only while these hold. They do not decide whether the provider goes live.")).toBeVisible()
  await expect(fn.getByRole("listitem").filter({ hasText: "A sandbox-proxy replica reported in the last 90 seconds" }).getByLabel("does not hold")).toBeVisible()
  await expect(fn.getByRole("listitem").filter({ hasText: "Price rule for functions" }).getByLabel("warning")).toBeVisible()
  await expect(fn.getByRole("listitem").filter({ hasText: "Job scheduler" }).getByLabel("holds")).toBeVisible()

  // A block the API would store and then refuse to serve is not saved.
  const saveButton = page.getByRole("button", { name: "Save", exact: true })
  await on.check()
  await group.getByLabel("Shortest idle time a function may set (s)").fill("30")
  await expect(group.getByRole("alert")).toHaveText("Shortest idle time a function may set (s): at least 60.")
  await expect(saveButton).toBeDisabled()
  await group.getByLabel("Shortest idle time a function may set (s)").fill("")
  await expect(group.getByRole("alert")).toHaveCount(0)

  // While a gate does not hold the API refuses the switch, in its own words.
  await page.evaluate(async () => {
    const { ApiError, setMockHandler } = await import(/* @vite-ignore */ "/src/lib/api.ts" as string)
    const { dispatch } = await import(/* @vite-ignore */ "/src/mocks/router.ts" as string)
    const w = window as unknown as { __calls: unknown[]; __refuse: boolean }
    w.__refuse = true
    setMockHandler((path: string, opts: { method?: string; body?: unknown }) => {
      w.__calls.push({ method: (opts.method ?? "GET").toUpperCase(), path, body: opts.body })
      if (w.__refuse && opts.method === "PUT") return Promise.reject(new ApiError(409, 409, "Functions cannot be switched on: no sandbox-proxy replica of this provider has reported in the last 90 seconds"))
      return dispatch(path, opts)
    })
  })
  await saveButton.click()
  await expect(page.getByText("Functions cannot be switched on: no sandbox-proxy replica of this provider has reported in the last 90 seconds")).toBeVisible()
  await page.evaluate(() => { (window as unknown as { __refuse: boolean }).__refuse = false })

  await group.getByLabel("Most instances a function may scale to").fill("3")
  await group.getByLabel("Highest minimum a tenant may set").click()
  await page.getByRole("option", { name: "1: a tenant may keep a function always on" }).click()
  await group.getByLabel("Functions", { exact: true }).fill("10")
  await expectAccessible(page)
  await saveButton.click()
  await expect(page.getByText("Saved", { exact: true })).toBeVisible()
  const puts = await calls(page, "PUT", "/admin/service/svc-sandbox-01")
  expect(puts.at(-1)!.body.config.sandbox.functions).toEqual({ enabled: true, minInstances: 0, maxMinInstances: 1, maxInstances: 3, projectQuota: { functions: 10 } })
  expect(puts.at(-1)!.body.secret, "the wake token is the installation's: the form never sends one").toBeUndefined()
})

// ── A project's function quota ────────────────────────────────────────────────

test("project: the function quota is a field of its own, blank falls back to the provider's default", async ({ page }) => {
  await page.goto("/clients/projects/prj-0001?tab=quota&mock-platform=functionson")
  await expect(page.getByRole("heading", { name: "Function quota" })).toBeVisible()
  await recordCalls(page)
  const functions = page.getByLabel("Functions", { exact: true })
  await expect(functions).toHaveAttribute("placeholder", "Provider default (10)") // the provider names 10
  await expect(page.getByLabel("Function vCPUs", { exact: true })).toHaveAttribute("placeholder", "Provider default (8)") // built in
  await expect(page.getByLabel("Function RAM (MiB)", { exact: true })).toHaveAttribute("placeholder", "Provider default (16384)")

  await functions.fill("1.5")
  await expect(functions).toHaveAttribute("aria-invalid", "true")
  await expect(page.getByRole("button", { name: "Save function quota" })).toBeDisabled()
  await functions.fill("4")
  await page.getByLabel("Function RAM (MiB)", { exact: true }).fill("2048")
  await page.getByRole("button", { name: "Save function quota" }).click()
  await expect(page.getByText("Function quota saved")).toBeVisible()
  const [put] = await calls(page, "PUT", "/admin/project/prj-0001/function-quota")
  expect(put.body).toEqual({ functions: 4, ramMb: 2048 })
  // Never a key of the resource quota: a key there would make the project's Proxmox quota a document of its own.
  expect(await calls(page, "PUT", "/admin/project/prj-0001/quota")).toHaveLength(0)
  await expect(functions).toHaveValue("4")
  await expectAccessible(page)

  // Saving the resource quota leaves the function quota out of its body.
  await page.getByRole("button", { name: "Save resource quota" }).click()
  await expect(page.getByText("Project quota saved")).toBeVisible()
  const [quota] = await calls(page, "PUT", "/admin/project/prj-0001/quota")
  expect(Object.keys(quota.body).some((k) => /function/i.test(k))).toBe(false)
})

// ── The operator docs ─────────────────────────────────────────────────────────

test("the operator docs have the functions page, linked from the settings page and the resource types", async ({ page }) => {
  await page.emulateMedia({ reducedMotion: "reduce" })
  await page.goto("/docs/platform-admin/cloud/sandboxes")
  await expect(page.getByRole("heading", { name: "Functions on the sandbox provider", level: 1 })).toBeVisible()
  await expect(page.getByRole("heading", { name: "What functions add to what sandboxes give up" })).toBeVisible()
  await expect(page.getByRole("heading", { name: "Switching functions off" })).toBeVisible()
  await expectAccessible(page)
  for (const from of ["/docs/platform-admin/managed-services", "/docs/platform-admin/billing/resource-types"]) {
    await page.goto(from)
    await page.getByRole("main").getByRole("link", { name: "Functions on the sandbox provider" }).first().click()
    await expect(page).toHaveURL(/\/docs\/platform-admin\/cloud\/sandboxes$/)
  }
})
