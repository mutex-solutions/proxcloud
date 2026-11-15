import { expect, test } from "@playwright/test"
import { calls, navigate, recordCalls } from "./mock-calls.ts"

test("provider Connection Save sends the failover URLs and the cluster CA", async ({ page }) => {
  await page.goto("/system/cloud-providers/svc-proxmox-01")
  await expect(page.getByLabel("API URL", { exact: true })).toBeVisible()
  await recordCalls(page)

  await page.getByLabel(/Other API URLs for failover/).fill("https://pve2.example:8006\nhttps://pve3.example:8006/")
  await page.getByLabel(/Cluster CA/).fill("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----")
  await page.getByRole("button", { name: "Save", exact: true }).click()

  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-proxmox-01")).toHaveLength(1)
  const [put] = await calls(page, "PUT", "/admin/service/svc-proxmox-01")
  expect(put.body.config.apiUrls).toEqual(["https://pve2.example:8006", "https://pve3.example:8006"])
  expect(put.body.config.caCert).toBe("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----")
  // The rest of the stored network block rides along.
  expect(put.body.config.network.publicNetworks).toHaveLength(1)
})

// The services cluster locks ArgoCD's `default` project (installer stage 50), so a Kubernetes provider must never
// fall back to it: a blank AppProject, or a stored provider without one, means the guardrail project proxcloud-k8s,
// as in kamaji.BuildApplication.
test("a new Kubernetes provider with a blank AppProject goes to proxcloud-k8s", async ({ page }) => {
  await page.goto("/system/cloud-providers")
  await expect(page.getByRole("button", { name: "Add provider" })).toBeVisible()
  await recordCalls(page)
  await page.getByRole("button", { name: "Add provider" }).click()
  await page.getByRole("button", { name: "Kubernetes", exact: true }).click()
  await page.getByLabel("Display name").fill("Managed Kubernetes AZ1")
  await page.getByLabel(/Management-cluster kubeconfig/).fill("apiVersion: v1\nkind: Config")
  await page.getByLabel("Chart version (pinned)").fill("0.1.0")
  await page.getByLabel(/Kubernetes versions/).fill("1.35.4=9001")
  await page.getByLabel("ArgoCD AppProject").fill("")
  await page.getByRole("button", { name: "Create provider" }).click()

  await expect.poll(() => calls(page, "POST", "/admin/service")).toHaveLength(1)
  const [post] = await calls(page, "POST", "/admin/service")
  expect(post.body.config.argocd.project).toBe("proxcloud-k8s")
})

test("a stored Kubernetes provider without an AppProject saves proxcloud-k8s", async ({ page }) => {
  const svc = {
    id: "svc-k8s-01", name: "Managed Kubernetes AZ1", type: "CLOUD", status: "ACTIVE",
    config: {
      provider: "kamaji", region: "az1", regions: { az1: { name: "az1" } }, services: { kubernetes: { az1: true } },
      argocd: { namespace: "argocd", chartRepo: "ghcr.io/mutex-solutions/charts", chartName: "proxmox-kamaji-cluster", chartVersion: "0.1.0" },
      cluster: { versions: { "1.35.4": "9001" } },
    },
  }
  await page.goto("/system/cloud-providers")
  await expect(page.getByRole("button", { name: "Add provider" })).toBeVisible()
  await recordCalls(page, { "GET /admin/service/svc-k8s-01": svc, "PUT /admin/service/svc-k8s-01": svc })
  await navigate(page, "/system/cloud-providers/svc-k8s-01")
  await expect(page.getByLabel("ArgoCD AppProject")).toHaveValue("proxcloud-k8s")
  await page.getByRole("button", { name: "Save", exact: true }).click()

  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-k8s-01")).toHaveLength(1)
  const [put] = await calls(page, "PUT", "/admin/service/svc-k8s-01")
  expect(put.body.config.argocd.project).toBe("proxcloud-k8s")
})
