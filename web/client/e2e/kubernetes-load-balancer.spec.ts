import { expect, test } from "@playwright/test"

// A cluster's load balancer (v0.11.0) on the mock app: prj-aurora's prod runs the controller in default mode with
// NGINX Gateway Fabric on; its balancer, on two appliances, carries two Services and refuses a third. staging has no
// controller.
const SITE = "/p/prj-aurora"

test("the cluster page says which Services get a public address, what it costs, and shows the balancer", async ({ page }) => {
  await page.goto(`${SITE}/kubernetes/res-pc-c-healthy`)
  const block = page.getByTestId("k8s-lb")
  await expect(block.getByTestId("k8s-lb-mode")).toContainText("get a public address of the platform's load balancer")
  await expect(block.getByTestId("k8s-lb-mode")).toContainText("proxcloud.io/internal")
  await expect(block.getByTestId("k8s-lb-cost")).toContainText("two appliance VMs, billed as servers")
  await expect(block.getByTestId("k8s-lb-cost")).toContainText("each Gateway takes a public address")
  await expect(block.getByTestId("k8s-lb-controller")).toContainText("last reported")
  await expect(block.getByText("VM 311")).toBeVisible()
  await expect(block.getByText("holds the address")).toHaveCount(1)
  const web = block.getByRole("row").filter({ hasText: "shop/web" })
  await expect(web).toContainText("203.0.113.21")
  await expect(web).toContainText("443")
  await expect(web).toContainText("3 of 3 up")
  // Policy Local: only the worker with a pod answers the health port.
  await expect(block.getByRole("row").filter({ hasText: "nginx-gateway/public-gateway-nginx" })).toContainText("1 of 3 up")
  await expect(block.getByRole("row").filter({ hasText: "infra/dns" })).toContainText("UDPNotSupported")

  await block.getByRole("button", { name: "Open the load balancer" }).click()
  await expect(page.getByTestId("lb-cluster")).toContainText("Services of Kubernetes cluster pc-c-healthy")
  await expect(page.getByTestId("lb-cluster").getByRole("row").filter({ hasText: "shop/web" })).toContainText("203.0.113.21")
  // Made by the cluster: nothing here edits or deletes it; the appliances can still be replaced.
  await expect(page.getByRole("button", { name: "Delete" })).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Save configuration" })).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Add pool" })).toHaveCount(0)
  await expect(page.getByTestId("lb-appliances").getByRole("button", { name: "Replace appliances" })).toBeEnabled()

  await page.getByRole("link", { name: "cluster pc-c-healthy" }).click()
  await expect(page.getByTestId("k8s-lb")).toBeVisible()
})

test("the cluster page lists the addresses the cluster keeps and releases one after a confirmation", async ({ page }) => {
  await page.goto(`${SITE}/kubernetes/res-pc-c-healthy`)
  const kept = page.getByTestId("k8s-lb-kept")
  await expect(kept).toContainText("still billed and counted in your quota")
  await expect(kept).toContainText("spec.loadBalancerIP")
  await expect(kept.getByText("203.0.113.30")).toBeVisible()
  await expect(kept.getByText("203.0.113.31")).toBeVisible()
  // No Service is on a kept address: it is in no row of the Services.
  await expect(page.getByTestId("k8s-lb").getByRole("row").filter({ hasText: "203.0.113.30" })).toHaveCount(0)

  // Cancel changes nothing.
  await kept.getByRole("button", { name: "Release 203.0.113.30" }).click()
  await expect(page.getByRole("dialog")).toContainText("goes back to the pool and stops being billed")
  await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click()
  await expect(kept.getByText("203.0.113.30")).toBeVisible()

  await kept.getByRole("button", { name: "Release 203.0.113.30" }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Release address" }).click()
  await expect(page.getByText("203.0.113.30 is being released")).toBeVisible()
  await expect(kept.getByText("203.0.113.30")).toHaveCount(0)
  await expect(kept.getByText("203.0.113.31")).toBeVisible()
})

test("the load balancer list names the cluster a balancer belongs to", async ({ page }) => {
  await page.goto(`${SITE}/load-balancers`)
  await expect(page.getByRole("row", { name: /k8s-5e1f0a2b3c4d/ })).toContainText("Kubernetes cluster pc-c-healthy")
  await expect(page.getByRole("row", { name: /web-lb/ })).not.toContainText("Kubernetes cluster")
})

test("adding NGINX Gateway Fabric to a cluster in default mode says each Gateway takes a public address", async ({ page }) => {
  await page.goto(`${SITE}/kubernetes/res-pc-c-healthy`)
  await page.getByRole("button", { name: "Manage add-ons" }).click()
  await expect(page.getByRole("dialog").getByTestId("ngf-public-address")).toContainText("each Gateway takes a public address")
})

test("a cluster without the controller keeps its private addresses and says so", async ({ page }) => {
  await page.goto(`${SITE}/kubernetes/res-pc-c-stuck`)
  await expect(page.getByTestId("k8s-lb-mode")).toContainText("not offered on this cluster")
  await expect(page.getByTestId("k8s-lb-cost")).toHaveCount(0)
  await expect(page.getByTestId("k8s-lb-controller")).toHaveCount(0)
  await page.getByRole("button", { name: "Manage add-ons" }).click()
  await expect(page.getByRole("dialog").getByTestId("ngf-public-address")).toHaveCount(0)
})
