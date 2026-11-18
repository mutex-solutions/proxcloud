import { expect, test } from "@playwright/test"

// The customer console defects of the production acceptance on v0.8.0 that the console decides (D9 to D15 and two
// of its UX findings). Runs on the mock app: src/mocks answers as a Proxmox site whose images show their display on
// the serial port, and prj-staging is a site without object storage and volume snapshots.
const P = "/p/prj-aurora"

test("D10: the role form states the name rule and holds a name the server would refuse", async ({ page }) => {
  await page.goto(`${P}/org/roles`)
  await page.getByRole("button", { name: /create role/i }).first().click()
  const name = page.getByLabel("Name", { exact: true })
  // The old example, "billing-viewer", is refused by the server's own rule.
  await expect(name).toHaveAttribute("placeholder", "BILLING_VIEWER")
  await expect(page.getByText("Starts with a letter; letters, digits and underscores only.")).toBeVisible()
  const create = page.getByRole("button", { name: "Create role", exact: true }).last()
  await name.fill("billing-viewer")
  await expect(name).toHaveAttribute("aria-invalid", "true")
  await expect(create).toBeDisabled()
  await name.fill("billing_viewer")
  await expect(name).toHaveAttribute("aria-invalid", "false")
  await expect(create).toBeEnabled()
})

test("D12: a backup asks before it starts", async ({ page }) => {
  await page.goto(`${P}/servers/res-server-001`)
  await page.getByRole("tab", { name: "Backups" }).click()
  await page.getByRole("button", { name: "Create backup" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByText("Back up this server now?")).toBeVisible()
  // Nothing has started: Cancel leaves no toast.
  await dialog.getByRole("button", { name: "Cancel" }).click()
  await expect(page.getByText("Backup started")).toHaveCount(0)
  await page.getByRole("button", { name: "Create backup" }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Create backup" }).click()
  await expect(page.getByText("Backup started")).toBeVisible()
})

test("D14: Rebuild offers the project's own images, as Create does", async ({ page }) => {
  await page.goto(`${P}/servers/res-server-001`)
  await page.getByRole("button", { name: /more actions/i }).click()
  await page.getByRole("menuitem", { name: "Rebuild" }).click()
  await page.getByRole("dialog").getByRole("combobox").click()
  await expect(page.getByText("Your images")).toBeVisible()
  await expect(page.getByRole("option", { name: "web-01-snapshot" })).toBeVisible()
})

test("D15: a page the menu hides is not reachable by its URL", async ({ page }) => {
  // The menu's answer is slow, as on a real site: a hidden page must wait for it and not start its own requests.
  await page.addInitScript(() => {
    ;(globalThis as { __mockLatency?: (call: string) => number | undefined }).__mockLatency = (call) =>
      call.startsWith("GET /init/") ? 1500 : undefined
  })
  for (const [path, label] of [["snapshots", "Snapshots"], ["s3-keys", "S3 access keys"]]) {
    await page.goto(`/p/prj-staging/${path}`)
    await expect(page.getByText(`${label} is not available in this project`)).toBeVisible()
    await expect(page.getByRole("link", { name: label, exact: true })).toHaveCount(0)
    // The hidden page never mounted: on production its requests went out and answered 400. The mock records every
    // call the app makes (src/mocks/router.ts).
    const calls = await page.evaluate(() => (globalThis as { __mockCalls?: string[] }).__mockCalls ?? [])
    expect(calls).toContain("GET /init/prj-staging")
    expect(calls.filter((c) => /\/s3-(keys|credentials)|\/resource\?type=/.test(c))).toEqual([])
  }
  // The same pages on a site that serves them.
  await page.goto(`${P}/snapshots`)
  await expect(page.getByRole("heading", { name: /snapshots/i }).first()).toBeVisible()
  await expect(page.getByText("is not available in this project")).toHaveCount(0)
})

test("D2: the Subnets tab shows the subnet of a network made before the row held subnetDetails", async ({ page }) => {
  await page.goto(`${P}/networks/res-network-001`)
  await page.getByRole("tab", { name: "Subnets" }).click()
  await expect(page.getByRole("cell", { name: "10.0.0.0/24", exact: true })).toBeVisible()
  await expect(page.getByRole("cell", { name: "10.0.0.1", exact: true })).toBeVisible()
  await expect(page.getByRole("checkbox", { name: "Outbound internet for 10.0.0.0/24" })).toBeChecked()
})

test("D9: the share dialog warns when the project's networks cannot reach the file server", async ({ page }) => {
  await page.goto(`${P}/file-shares`)
  await page.getByRole("button", { name: "Create share" }).click()
  await expect(page.getByTestId("share-warning")).toContainText("cannot reach the file server 192.0.2.100")
})

test("the Kubernetes form starts on the lowest offered version, without high availability", async ({ page }) => {
  await page.goto(`${P}/kubernetes`)
  await page.getByRole("button", { name: /create cluster/i }).first().click()
  const dialog = page.getByRole("dialog")
  // The mock offers 1.37.2 and 1.35.10.
  await expect(dialog.getByRole("combobox").filter({ hasText: "1.35.10" })).toBeVisible()
  await expect(dialog.getByRole("switch", { name: "High availability" })).not.toBeChecked()
})

test("the console opens on Serial for a serial display, says to press Enter, and ends with the server (D13)", async ({ page }) => {
  // The mock's serial socket: termproxy answers "OK" and then stays silent and OPEN, as the production one did when
  // the server restarted.
  await page.routeWebSocket(/mock-serial/, (ws) => {
    ws.send("OK")
  })
  await page.goto(`${P}/servers/res-server-001/console`)
  await expect(page.getByRole("tab", { name: "Serial" })).toHaveAttribute("aria-selected", "true")
  await expect(page.getByText("Connected", { exact: true })).toBeVisible()
  await expect(page.locator(".xterm-rows")).toContainText("Press Enter to see the login prompt")

  // The Graphical tab says why it shows one line.
  await page.getByRole("tab", { name: "Graphical (VNC)" }).click()
  await expect(page.getByTestId("serial-display-hint")).toBeVisible()
  await page.getByRole("tab", { name: "Serial" }).click()
  await expect(page.getByText("Connected", { exact: true })).toBeVisible()

  // The server restarts: it has been up for less time than the session.
  await page.evaluate(() => {
    ;(globalThis as { __mockServerUptime?: number }).__mockServerUptime = 0
  })
  await expect(page.getByText("The server was stopped or restarted, so this session has ended.")).toBeVisible({ timeout: 25_000 })
  await expect(page.getByText("Disconnected", { exact: true })).toBeVisible()
  await expect(page.getByRole("button", { name: "Reconnect" }).first()).toBeVisible()
})
