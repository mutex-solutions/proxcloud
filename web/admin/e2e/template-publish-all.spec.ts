import { expect, test, type Page } from "@playwright/test"
import { calls, navigate, recordCalls } from "./mock-calls.ts"

// "Publish on all nodes" of a Kubernetes node image: the per-node report with its failure states, the versions line,
// and the two rules of the dialog: it advances only when the report says so, and a refused publish is not repeated.
const digest = `sha256:${"c".repeat(64)}`
const PATH = "/admin/template-updates/publish-all"
const tuOn = { configured: true, integrationId: "int-gh", login: "proxcloud-ops", scopes: ["read:packages"], vmUpdatesEnabled: true, sandboxMirrorEnabled: false }
const providers = [{ id: "svc-proxmox-01", name: "Helium Proxmox", type: "CLOUD", status: "ACTIVE", config: { provider: "proxmox" } }]

const mixed = {
  digest, kubernetesVersion: "1.37.2", next: "wait", complete: false, nodesMissing: ["pve6"],
  versionsLine: '"1.37.2": "pve1=9210,pve5=9100"',
  nodes: [
    { node: "pve1", state: "published", vmid: 9210 },
    { node: "pve2", state: "smoke_running", vmid: 9211 },
    { node: "pve3", state: "missing" },
    { node: "pve4", state: "import_failed", code: "upload_failed", message: "The upload to the import storage failed" },
    { node: "pve5", state: "smoke_failed", vmid: 9214, code: "cloud_init_failed", message: "cloud-init reported an error" },
    { node: "pve6", state: "skipped", message: "Import storage nas-templates and disk storage local-zfs are not both active on this node" },
    { node: "pve7", state: "smoke_not_started", vmid: 9216, code: "smoke_network_unset", message: "Set the provider's smoke network first" },
  ],
}

async function start(page: Page, overrides: Record<string, unknown>) {
  await page.goto("/system/cloud-providers")
  await expect(page.getByRole("button", { name: "Add provider" })).toBeVisible()
  await recordCalls(page, { "GET /admin/template-updates/status": tuOn, "GET /admin/service": providers, ...overrides })
  await navigate(page, "/system/template-updates")
  await page.getByRole("row").filter({ hasText: "proxcloud-vm-k8s-1-37" }).getByRole("button", { name: "Publish on all nodes…" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByRole("button", { name: "Start" })).toBeDisabled()
  await dialog.getByRole("combobox", { name: "Proxmox provider" }).click()
  await page.getByRole("option", { name: "Helium Proxmox" }).click()
  await dialog.getByLabel("Type or paste the exact digest to confirm").fill(digest)
  await dialog.getByRole("button", { name: "Start" }).click()
  return dialog
}

test("the report lists every node with its reason and warns before the versions line", async ({ page }) => {
  const dialog = await start(page, { [`GET ${PATH}`]: mixed })
  const row = (node: string) => dialog.getByRole("row").filter({ hasText: node })
  await expect(row("pve1")).toContainText("Published")
  await expect(row("pve2")).toContainText("Smoke test running")
  await expect(row("pve3")).toContainText("Not imported, waits its turn")
  await expect(row("pve4")).toContainText("Import failed")
  await expect(row("pve4")).toContainText("upload_failed")
  await expect(row("pve5")).toContainText("cloud_init_failed")
  await expect(row("pve6")).toContainText("are not both active on this node")
  await expect(row("pve7")).toContainText("Smoke test not started")
  await expect(dialog.getByRole("alert")).toContainText("Nodes pve6 publish another Kubernetes version and not this one")
  await expect(dialog.getByRole("button", { name: '"1.37.2": "pve1=9210,pve5=9100"' })).toBeVisible()
  await expect(dialog.getByRole("status")).toContainText("Waiting for the running job")
  // "wait" never posts: a job of the provider is running.
  const gets = await calls(page, "GET", PATH)
  expect(gets[0].path).toContain(`serviceId=svc-proxmox-01&digest=${encodeURIComponent(digest)}`)
  expect(await calls(page, "POST", PATH)).toHaveLength(0)
})

test("an advance posts the confirmed digest once, and a refused publish stops the loop", async ({ page }) => {
  const refused = {
    digest, kubernetesVersion: "1.37.2", next: "wait", complete: false, nodesMissing: [],
    nodes: [{ node: "pve1", state: "publish_failed", vmid: 9210, code: "template_changed_since_smoke", message: "The template's configuration changed after its smoke test" }],
  }
  const dialog = await start(page, {
    [`GET ${PATH}`]: { digest, next: "advance", complete: false, nodesMissing: [], nodes: [{ node: "pve1", state: "ready_to_publish", vmid: 9210 }] },
    [`POST ${PATH}`]: refused,
  })
  await expect(dialog.getByRole("row").filter({ hasText: "pve1" })).toContainText("template_changed_since_smoke")
  await expect(dialog.getByRole("status")).toContainText("Stopped: a publish was refused.")
  await expect(dialog.getByRole("button", { name: "Try again" })).toBeVisible()
  const posts = await calls(page, "POST", PATH)
  expect(posts).toHaveLength(1)
  expect(posts[0].body).toEqual({ serviceId: "svc-proxmox-01", package: "proxcloud-vm-k8s-1-37", digest, confirmDigest: digest })
})

test("a provider that refuses is shown, not retried", async ({ page }) => {
  const dialog = await start(page, { [`GET ${PATH}`]: { apiError: { status: 409, message: "This artifact digest was withdrawn and can never be imported or published again" } } })
  await expect(dialog.getByRole("alert")).toContainText("This artifact digest was withdrawn")
  expect(await calls(page, "POST", PATH)).toHaveLength(0)
})

test("the mock provider reaches every node one step at a time", async ({ page }) => {
  const dialog = await start(page, {})
  await expect(dialog.getByRole("row").filter({ hasText: "pve1" })).toContainText("Importing")
  await expect(dialog.getByRole("row").filter({ hasText: "pve2" })).toContainText("Not imported, waits its turn")
  await expect.poll(() => calls(page, "POST", PATH)).toHaveLength(1)
})
