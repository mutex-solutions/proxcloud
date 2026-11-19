import { expect, test, type Page } from "@playwright/test"
import { calls, navigate, recordCalls } from "./mock-calls.ts"

// Delete of a withdrawn template: offered on withdrawn rows only, confirmed in a dialog that names the VM id and the
// template's name, and a refusal stays in the dialog with the server's message.
const tuOn = { configured: true, integrationId: "int-gh", login: "proxcloud-ops", scopes: ["read:packages"], vmUpdatesEnabled: true, sandboxMirrorEnabled: false }
const row = (id: string, vmid: number, state: string, del: boolean) => ({
  id, package: "proxcloud-vm-ubuntu-2604-k8s", tag: `ubuntu-2604-k8s-1.35.10-${id}`, digest: `sha256:${"c".repeat(64)}`, os: "ubuntu-2604", variant: "kubernetes",
  kubernetesVersion: "1.35.10", buildVersion: id, slug: "ubuntu-2604-k8s-1.35.10", serviceId: "svc-proxmox-01", node: "pve1", vmid, templateName: `pc-ghcr-ubuntu-2604-k8s-1-35-10-${id}`,
  state, importedAt: "2026-11-14T10:00:00Z", requestedBy: "admin@example.com", publishedOnLine: 0,
  actions: { smoke: false, publish: false, withdraw: !del, delete: del, lastPublished: false },
})
const templates = { items: [row("old", 9201, "withdrawn", true), row("new", 9210, "staged", false)], smokes: [], vmUpdatesEnabled: true }
const PATH = "/admin/template-updates/templates/old"

async function open(page: Page, overrides: Record<string, unknown>) {
  await page.goto("/system/cloud-providers")
  await expect(page.getByRole("button", { name: "Add provider" })).toBeVisible()
  await recordCalls(page, { "GET /admin/template-updates/status": tuOn, "GET /admin/template-updates/templates": templates, ...overrides })
  await navigate(page, "/system/template-updates")
  await page.getByRole("tab", { name: "Imported templates" }).click()
  await expect(page.getByRole("row").filter({ hasText: "1.35.10-new" }).getByRole("button", { name: "Delete…" })).toHaveCount(0)
  await page.getByRole("row").filter({ hasText: "1.35.10-old" }).getByRole("button", { name: "Delete…" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toContainText("template VM 9201 (pc-ghcr-ubuntu-2604-k8s-1-35-10-old) on pve1")
  return dialog
}

test("a withdrawn template is deleted after the confirm", async ({ page }) => {
  const dialog = await open(page, { [`DELETE ${PATH}`]: { action: "delete", changed: [9201], complete: true } })
  expect(await calls(page, "DELETE", PATH)).toHaveLength(0)
  await dialog.getByRole("button", { name: "Delete template" }).click()
  await expect(dialog).toBeHidden()
  expect(await calls(page, "DELETE", PATH)).toHaveLength(1)
})

test("a refusal shows the server's message and keeps the dialog", async ({ page }) => {
  const message = 'Template 9201 on pve1 is still named by: cluster k8s-abc of project p1, node group workers. New workers would be cloned from it: move them to another image first'
  const dialog = await open(page, { [`DELETE ${PATH}`]: { apiError: { status: 409, message, details: { code: "template_in_use" } } } })
  await dialog.getByRole("button", { name: "Delete template" }).click()
  await expect(dialog.getByRole("alert")).toContainText(message)
  await expect(dialog.getByRole("button", { name: "Delete template" })).toBeEnabled()
})
