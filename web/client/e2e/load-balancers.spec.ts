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

// Layer 4 options (v0.11.1): the protocol sits on the listener, with a connection limit and an idle timeout; a pool
// has session persistence, the PROXY version, a health check of five kinds with its figures, and its two timeouts.
test("a listener's protocol and limits and a pool's layer 4 options are saved as the API takes them", async ({ page }) => {
  await page.goto(`${SITE}/load-balancers`)
  await page.getByRole("link", { name: "web-lb" }).click()
  const pick = async (box: string, option: string) => {
    await page.getByRole("combobox", { name: box }).click()
    await page.getByRole("option", { name: option, exact: true }).click()
  }
  // A listener made before protocols existed has its pool's, and the pool follows the listener.
  const protocol = page.getByRole("combobox", { name: "Listener protocol" })
  await expect(protocol).toContainText("HTTP")
  await expect(page.getByTestId("pool-protocol-0")).toContainText("spoken to in HTTP")
  await protocol.click()
  await expect(page.getByRole("option")).toHaveText(["TCP", "HTTP"]) // what a load balancer serves today
  await page.getByRole("option", { name: "TCP", exact: true }).click()
  await expect(page.getByTestId("pool-protocol-0")).toContainText("spoken to in TCP")

  await page.getByRole("spinbutton", { name: "Connection limit" }).fill("500")
  await page.getByRole("spinbutton", { name: "Client idle timeout in seconds" }).fill("30")
  await pick("Algorithm", "Least connections")
  await pick("Session persistence", "Source address")
  await page.getByLabel("Kept for (s)").fill("600")
  await pick("PROXY protocol", "v1")
  await pick("Health check", "HTTPS GET")
  await expect(page.getByLabel("Path")).toHaveValue("/healthz")
  await page.getByLabel("Every (s)").fill("5")
  await page.getByLabel("Timeout (s)", { exact: true }).fill("2")
  await page.getByLabel("Up after").fill("3")
  await page.getByLabel("Down after").fill("4")
  await page.getByLabel("Connect timeout (s)").fill("1.1") // 1.1 * 1000 is not a whole number before it is rounded
  await page.getByLabel("Server idle timeout (s)").fill("30")
  await page.getByRole("button", { name: "Save configuration" }).click()
  await expect(page.getByText("Configuration saved")).toBeVisible()

  // Seconds on the page are milliseconds in the request; an empty figure is no field at all.
  const sent = await page.evaluate(() => (globalThis as { __lbConfig?: { listeners: object[]; pools: object[] } }).__lbConfig)
  expect(sent?.listeners[0]).toEqual({ port: 80, protocol: "tcp", pool: "web", sourceRanges: ["198.51.100.0/24"], connectionLimit: 500, timeoutClientData: 30000 })
  expect(sent?.pools[0]).toMatchObject({
    name: "web", protocol: "tcp", algorithm: "leastconn", proxyProtocol: true, proxyVersion: 1,
    sessionPersistence: { type: "source_ip", timeout: 600 }, timeoutMemberConnect: 1100, timeoutMemberData: 30000,
    healthCheck: { type: "https", path: "/healthz", interval: 5, timeout: 2, rise: 3, fall: 4 },
  })

  // The row gives them back.
  await page.getByRole("button", { name: "All" }).click()
  await page.getByRole("link", { name: "web-lb" }).click()
  await expect(page.getByRole("combobox", { name: "Listener protocol" })).toContainText("TCP")
  await expect(page.getByRole("spinbutton", { name: "Connection limit" })).toHaveValue("500")
  await expect(page.getByRole("spinbutton", { name: "Client idle timeout in seconds" })).toHaveValue("30")
  await expect(page.getByRole("combobox", { name: "Session persistence" })).toContainText("Source address")
  await expect(page.getByLabel("Kept for (s)")).toHaveValue("600")
  await expect(page.getByRole("combobox", { name: "PROXY protocol" })).toContainText("v1")
  await expect(page.getByRole("combobox", { name: "Health check" })).toContainText("HTTPS GET")
  await expect(page.getByLabel("Down after")).toHaveValue("4")
  await expect(page.getByLabel("Server idle timeout (s)")).toHaveValue("30")

  // No health check: its figures go, and every server reads "no check".
  await pick("Health check", "None")
  await expect(page.getByLabel("Every (s)")).toHaveCount(0)
  await expect(page.getByLabel("Check port")).toHaveCount(0)
  await pick("Session persistence", "None")
  await pick("PROXY protocol", "Off")
  await page.getByRole("button", { name: "Save configuration" }).click()
  await expect(page.getByText("no check")).toBeVisible()
  const cleared = await page.evaluate(() => (globalThis as { __lbConfig?: { pools: Array<Record<string, unknown>> } }).__lbConfig)
  expect(cleared?.pools[0].healthCheck).toEqual({ type: "none" })
  expect(cleared?.pools[0].sessionPersistence).toBeNull()
  expect(cleared?.pools[0].proxyProtocol).toBe(false)
  expect(cleared?.pools[0].proxyVersion).toBeUndefined()
})
