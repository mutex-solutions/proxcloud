import { expect, test } from "@playwright/test"
import { calls, navigate, recordCalls } from "./mock-calls.ts"

// The operator's side of a cluster's load balancer (v0.11.0): the provider's config.cluster.loadBalancer fields, each
// cluster's mode in the list, and the Load balancer dialog with "Enable load balancer controller".
const svc = {
  id: "svc-k8s-01", name: "Managed Kubernetes AZ1", type: "CLOUD", status: "ACTIVE",
  config: {
    provider: "kamaji", region: "az1", regions: { az1: { name: "az1" } }, services: { kubernetes: { az1: true } },
    argocd: { namespace: "argocd", project: "proxcloud-k8s", chartRepo: "ghcr.io/mutex-solutions/charts", chartName: "proxmox-kamaji-cluster", chartVersion: "0.1.17" },
    cluster: { versions: { "1.36.6": "pve1=9001" }, loadBalancer: { publicNetworkId: "vmbr1", flavorId: "lb-small" }, workerFirewall: true },
  },
}
const clusters = {
  providerChartVersion: "0.1.17",
  clusters: [
    { id: "pc-c-new", name: "prod", projectId: "prj-1", chartVersion: "0.1.17", loadBalancer: "default" },
    { id: "pc-c-old", name: "staging", projectId: "prj-1", chartVersion: "0.1.16" },
  ],
}
const view = {
  controllerSince: "2026-11-20T08:00:00Z", controllerLastSeen: new Date(Date.now() - 30000).toISOString(), balancerId: "res-lb-1",
  balancer: {
    status: "ACTIVE",
    appliances: [{ serverId: "311", node: "pve2", role: "MASTER", holdsVip: true, state: "up" }, { serverId: "312", node: "pve3", role: "BACKUP", state: "up" }],
    cluster: { services: [{ uid: "u1", namespace: "shop", name: "web", vip: "203.0.113.21", state: "ready" }] },
  },
}
const lbPath = (id: string) => `/admin/service/svc-k8s-01/k8s-clusters/${id}/load-balancer`

async function open(page: import("@playwright/test").Page, overrides: Record<string, unknown> = {}) {
  await page.goto("/system/cloud-providers")
  await expect(page.getByRole("button", { name: "Add provider" })).toBeVisible()
  await recordCalls(page, { "GET /admin/service/svc-k8s-01": svc, "GET /admin/service/svc-k8s-01/k8s-clusters": clusters, ...overrides })
  await navigate(page, "/system/cloud-providers/svc-k8s-01")
}

test("the provider's load balancer fields round-trip and are sent as a pair", async ({ page }) => {
  await open(page, { "PUT /admin/service/svc-k8s-01": svc })
  await expect(page.getByLabel(/Load balancer public network/)).toHaveValue("vmbr1")
  await expect(page.getByLabel("Load balancer flavor id")).toHaveValue("lb-small")
  // One without the other cannot be saved.
  await page.getByLabel("Load balancer flavor id").fill("")
  await expect(page.getByText("Set both, or clear both.")).toBeVisible()
  await expect(page.getByRole("button", { name: "Save", exact: true })).toBeDisabled()
  await page.getByLabel("Load balancer flavor id").fill("lb-medium")
  await page.getByRole("button", { name: "Save", exact: true }).click()
  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-k8s-01")).toHaveLength(1)
  const [put] = await calls(page, "PUT", "/admin/service/svc-k8s-01")
  expect(put.body.config.cluster.loadBalancer).toEqual({ publicNetworkId: "vmbr1", flavorId: "lb-medium" })
  // A key the form does not edit rides along.
  expect(put.body.config.cluster.workerFirewall).toBe(true)

  // Both cleared: no load balancer.
  await page.getByLabel(/Load balancer public network/).fill("")
  await page.getByLabel("Load balancer flavor id").fill("")
  await page.getByRole("button", { name: "Save", exact: true }).click()
  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-k8s-01")).toHaveLength(2)
  const [, cleared] = await calls(page, "PUT", "/admin/service/svc-k8s-01")
  expect(cleared.body.config.cluster.loadBalancer).toBeUndefined()
})

test("a cluster with a controller shows its mode, last call and balancer; Run again rotates", async ({ page }) => {
  await open(page, { [`GET ${lbPath("pc-c-new")}`]: view, [`POST ${lbPath("pc-c-new")}`]: { clusterId: "pc-c-new" } })
  await expect(page.getByRole("row").filter({ hasText: "prod" })).toContainText("Default")
  await expect(page.getByRole("row").filter({ hasText: "staging" })).toContainText("MetalLB only")
  await page.getByRole("button", { name: "Load balancer of prod" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toContainText("Default mode")
  await expect(dialog.getByTestId("lb-controller-seen")).toContainText("last called the gateway")
  await expect(dialog).toContainText("holds the addresses")
  await expect(dialog.getByRole("row").filter({ hasText: "shop/web" })).toContainText("203.0.113.21")
  await dialog.getByRole("button", { name: "Rotate the controller's credential" }).click()
  await expect.poll(() => calls(page, "POST", lbPath("pc-c-new"))).toHaveLength(1)
  await expect(page.getByText("A new credential was minted")).toBeVisible()
})

test("Enable load balancer controller on an existing cluster, and a gate that does not hold", async ({ page }) => {
  const why = "No load balancer controller: the provider runs sharedWorkers: the controller needs the project's own workers"
  await open(page, { [`GET ${lbPath("pc-c-old")}`]: null, [`POST ${lbPath("pc-c-old")}`]: { apiError: { status: 409, message: why } } })
  await page.getByRole("button", { name: "Load balancer of staging" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toContainText("No controller")
  await expect(dialog).toContainText("class-only mode")
  await expect(dialog.getByTestId("lb-controller-seen")).toHaveCount(0)
  await dialog.getByRole("button", { name: "Enable load balancer controller" }).click()
  await expect(dialog.getByRole("alert")).toContainText("sharedWorkers")
  expect(await calls(page, "POST", lbPath("pc-c-old"))).toHaveLength(1)
})
