import { expect, test } from "@playwright/test"
import { calls, navigate, recordCalls } from "./mock-calls.ts"

// The operator's switch for the Proxmox VM firewall of one cluster's workers.
const svc = {
  id: "svc-k8s-01", name: "Managed Kubernetes AZ1", type: "CLOUD", status: "ACTIVE",
  config: {
    provider: "kamaji", region: "az1", regions: { az1: { name: "az1" } }, services: { kubernetes: { az1: true } },
    argocd: { namespace: "argocd", project: "proxcloud-k8s", chartRepo: "ghcr.io/mutex-solutions/charts", chartName: "proxmox-kamaji-cluster", chartVersion: "0.1.13" },
    cluster: { versions: { "1.35.10": "pve1=9001" } },
  },
}
const clusters = { providerChartVersion: "0.1.13", clusters: [{ id: "pc-c-open", name: "staging", projectId: "prj-1", chartVersion: "0.1.13" }] }
const worker = { vmid: 141, name: "pc-c-open-workers-pve1-abcde-1", node: "pve1", status: "running" }
const open = { clusterId: "pc-c-open", protected: false, groupName: "k8s-pc-c-open", enforced: true, workers: [{ ...worker, secured: false }] }
const closed = { ...open, protected: true, group: "g1a2b3c4", verified: true, workers: [{ ...worker, secured: true }] }
const path = "/admin/service/svc-k8s-01/k8s-clusters/pc-c-open/worker-firewall"

async function show(page: import("@playwright/test").Page, overrides: Record<string, unknown>) {
  await page.goto("/system/cloud-providers")
  await expect(page.getByRole("button", { name: "Add provider" })).toBeVisible()
  await recordCalls(page, { "GET /admin/service/svc-k8s-01": svc, "GET /admin/service/svc-k8s-01/k8s-clusters": clusters, ...overrides })
  await navigate(page, "/system/cloud-providers/svc-k8s-01")
  await page.getByRole("button", { name: "Firewall" }).click()
  return page.getByRole("dialog")
}

test("an unprotected cluster is protected by the operator and can be opened again", async ({ page }) => {
  const dialog = await show(page, { [`GET ${path}`]: open, [`POST ${path}`]: closed, [`DELETE ${path}`]: open })
  await expect(dialog.getByText("Worker firewall of staging")).toBeVisible()
  await expect(dialog.getByText("Not protected")).toBeVisible()
  await expect(dialog.getByRole("row").filter({ hasText: "141 @ pve1" })).toContainText("Off")
  await expect(dialog.getByRole("button", { name: "Remove the worker firewall" })).toHaveCount(0)

  await dialog.getByRole("button", { name: "Protect this cluster's workers" }).click()
  await expect.poll(() => calls(page, "POST", path)).toHaveLength(1)
  await expect(dialog.getByText("k8s-pc-c-open")).toBeVisible()
  await expect(dialog.getByRole("row").filter({ hasText: "141 @ pve1" })).toContainText("In place")

  await dialog.getByRole("button", { name: "Remove the worker firewall" }).click()
  await expect.poll(() => calls(page, "DELETE", path)).toHaveLength(1)
  await expect(dialog.getByText("Not protected")).toBeVisible()
})

test("a refused protect shows why and that the cluster is as it was", async ({ page }) => {
  const why = "After the firewall went on, pc-c-open-workers-pve1-abcde-1 did not renew the node Lease within 55s. The firewall was taken off again; the cluster is as it was"
  const dialog = await show(page, { [`GET ${path}`]: open, [`POST ${path}`]: { apiError: { status: 409, message: why } } })
  await dialog.getByRole("button", { name: "Protect this cluster's workers" }).click()
  await expect(dialog.getByRole("alert")).toContainText("taken off again")
  await expect(dialog.getByText("Not protected")).toBeVisible()
})

test("with the datacenter firewall off nothing can be protected", async ({ page }) => {
  const dialog = await show(page, { [`GET ${path}`]: { ...open, enforced: false } })
  await expect(dialog.getByRole("alert")).toContainText("datacenter firewall")
  await expect(dialog.getByRole("button", { name: "Protect this cluster's workers" })).toBeDisabled()
})
