import { expect, test } from "@playwright/test"

// A load balancer with high availability (v0.11.0): two appliances, the one holding the address, what a one-node site
// means for it, and the replace. Runs on the mock app: prj-aurora's web-lb has both appliances on node pve1.
const SITE = "/p/prj-aurora"

test("the create dialog offers high availability, on by default, with what it costs", async ({ page }) => {
  await page.goto(`${SITE}/load-balancers`)
  await page.getByRole("button", { name: "Create load balancer" }).first().click()
  const dialog = page.getByRole("dialog")
  const ha = dialog.getByRole("switch", { name: "High availability" })
  await expect(ha).toBeChecked()
  await expect(dialog.getByTestId("lb-ha-cost")).toContainText("Both are billed as servers")
  await ha.click()
  await expect(ha).not.toBeChecked()
  await expect(dialog.getByTestId("lb-ha-cost")).toContainText("One appliance VM")
})

test("a balancer on two appliances shows them, the one holding the address, and the one-node limit", async ({ page }) => {
  await page.goto(`${SITE}/load-balancers`)
  await expect(page.getByRole("row", { name: /web-lb/ })).toContainText("Two appliances")
  await page.getByRole("link", { name: "web-lb" }).click()
  const block = page.getByTestId("lb-appliances")
  await expect(block).toContainText("VM 301")
  await expect(block).toContainText("VM 302")
  await expect(block.getByText("holds the address")).toHaveCount(1)
  await expect(page.getByTestId("lb-one-node")).toContainText("survives an appliance failure, not the loss of the node")
  // What the pair adds to a pool and a listener.
  await expect(page.getByRole("combobox", { name: "Health check" })).toContainText("HTTP GET")
  await expect(page.getByLabel("Path")).toHaveValue("/healthz")
  await expect(page.getByRole("textbox", { name: "Allowed sources" })).toHaveValue("198.51.100.0/24")

  await block.getByRole("button", { name: "Replace appliances" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toContainText("the standby first")
  await dialog.getByRole("button", { name: "Replace appliances" }).click()
  await expect(page.getByText("Replacing the appliances, the standby first")).toBeVisible()
  await expect(page.getByTestId("lb-replacing")).toBeVisible()
  await expect(block.getByRole("button", { name: "Replace appliances" })).toBeDisabled()
  const calls = await page.evaluate(() => (globalThis as { __mockCalls?: string[] }).__mockCalls ?? [])
  expect(calls.some((c) => /^POST \/project\/prj-aurora\/cloud\/res-load-balancer-001\/action/.test(c))).toBe(true)
})
