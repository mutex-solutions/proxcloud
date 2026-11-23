import { expect, test } from "@playwright/test"

// A cluster's load balancer on the mock app: prj-aurora's prod runs the controller in default mode with NGINX Gateway
// Fabric on; its balancer, on two appliances, carries three Services (one ends TLS with a Secret's certificate, one
// is UDP) and refuses a fourth. staging has no controller.
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
  await expect(web).toContainText("443/HTTPS")
  await expect(web).toContainText("3 of 3 up")
  // A port that ends TLS names the Secret its certificate came from, and when that certificate expires.
  await expect(web.getByTestId("k8s-lb-cert")).toContainText("Secret shop/shop-tls (shop.example.com), expires")
  await expect(web.getByTestId("k8s-lb-cert")).toContainText("Expires in 1")
  // Policy Local: only the worker with a pod answers the health port.
  await expect(block.getByRole("row").filter({ hasText: "nginx-gateway/public-gateway-nginx" })).toContainText("1 of 3 up")
  const gw = block.getByRole("row").filter({ hasText: "nginx-gateway/public-gateway-nginx" })
  await expect(gw).toContainText("80/TCP")
  await expect(gw.getByTestId("k8s-lb-cert")).toHaveCount(0)
  await expect(gw.getByTestId("k8s-lb-udp-check")).toHaveCount(0)
  // A UDP port is served, its workers are checked, and the row says what the check cannot see.
  const dns = block.getByRole("row").filter({ hasText: "infra/dns" })
  await expect(dns).toContainText("203.0.113.23")
  await expect(dns).toContainText("53/UDP")
  await expect(dns).toContainText("2 of 3 up")
  await expect(dns.getByTestId("k8s-lb-udp-check")).toContainText("a closed port may not be seen under traffic")
  await expect(block.getByRole("row").filter({ hasText: "infra/sigtran" })).toContainText("ProtocolNotSupported")
  // The controller speaks the platform's protocol: nothing asks for an update.
  await expect(block.getByTestId("k8s-lb-old-controller")).toHaveCount(0)

  await block.getByRole("button", { name: "Open the load balancer" }).click()
  await expect(page.getByTestId("lb-cluster")).toContainText("Services of Kubernetes cluster pc-c-healthy")
  await expect(page.getByTestId("lb-cluster").getByRole("row").filter({ hasText: "shop/web" })).toContainText("203.0.113.21")
  // The balancer's own page reads the same members: the UDP Service's workers and their health, the port that ends
  // TLS by its protocol. The certificate's Secret is the cluster page's to name.
  await expect(page.getByTestId("lb-cluster").getByRole("row").filter({ hasText: "infra/dns" })).toContainText("53/UDP")
  await expect(page.getByTestId("lb-cluster").getByRole("row").filter({ hasText: "infra/dns" })).toContainText("2 of 3 up")
  await expect(page.getByTestId("lb-cluster").getByRole("row").filter({ hasText: "shop/web" })).toContainText("443/HTTPS")
  await expect(page.getByTestId("lb-cluster").getByTestId("k8s-lb-cert")).toHaveCount(0)
  // Made by the cluster: nothing here edits or deletes it; the appliances can still be replaced.
  await expect(page.getByRole("button", { name: "Delete" })).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Save configuration" })).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Add pool" })).toHaveCount(0)
  await expect(page.getByTestId("lb-appliances").getByRole("button", { name: "Replace appliances" })).toBeEnabled()

  await page.getByRole("link", { name: "cluster pc-c-healthy" }).click()
  await expect(page.getByTestId("k8s-lb")).toBeVisible()
})

test("a cluster whose controller is the one of an earlier release says what it does not serve and how to get it", async ({ page }) => {
  await page.addInitScript(() => {
    ;(globalThis as { __mockLBControllerProtocol?: number }).__mockLBControllerProtocol = 1
  })
  await page.goto(`${SITE}/kubernetes/res-pc-c-healthy`)
  const old = page.getByTestId("k8s-lb-old-controller")
  await expect(old).toContainText("protocol 1 of 2")
  await expect(old).toContainText("A Service with a UDP port is refused")
  await expect(old).toContainText("no TLS from a Secret")
  await expect(old).toContainText("Apply the platform update on this page")
  await expect(old).toContainText("chart 0.1.19 or later")
  await expect(old).toContainText("If the page offers no update, ask your operator")
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
