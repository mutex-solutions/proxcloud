import { expect, test } from "@playwright/test"

const project = "/p/prj-aurora"

for (const viewport of [
  { name: "desktop", width: 1280, height: 800 },
  { name: "mobile", width: 390, height: 844 },
]) {
  for (const route of ["dashboard", "servers/new", "volumes"]) {
    test(`quota UI reflows on ${route} [${viewport.name}]`, async ({ page }) => {
      await page.setViewportSize(viewport)
      await page.goto(`${project}/${route}`)
      await page.waitForLoadState("networkidle")

      const documentOverflow = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
      )
      expect(documentOverflow).toBeLessThanOrEqual(1)
    })
  }
}

test("dashboard shows the full region GPU catalog with per-model capacity", async ({ page }) => {
  // prj-aurora has gpuCapacityVisible enabled, so the GPU tiles list the whole region catalog
  // (incl. models the project doesn't use) and no-limit models show region free/total.
  await page.goto(`${project}/dashboard`)
  await expect(page.getByText("Quota & usage")).toBeVisible()
  await expect(page.getByText("Instances", { exact: true })).toBeVisible()
  // Model with a project limit keeps its quota tile + a region-availability note.
  await expect(page.getByText("GPU / nvidia-a10", { exact: true })).toBeVisible()
  await expect(page.getByText("Project-wide custom quota", { exact: true })).toBeVisible()
  await expect(page.getByText("3 of 4 free in region", { exact: true })).toBeVisible()
  // A region model the project does NOT use is still listed (full catalog).
  await expect(page.getByText("GPU / nvidia-a100-80gb", { exact: true })).toBeVisible()
  await expect(page.getByText("Region availability").first()).toBeVisible()
  // A no-limit model shows region free/total (24 total, sold out) instead of "Unlimited".
  const l40s = page.getByRole("meter", { name: "GPU / nvidia-l40s quota" })
  await expect(l40s).toHaveAttribute("aria-valuemax", "24")
  await expect(l40s).toHaveAttribute("aria-valuetext", "24 used of 24")
})

test("dashboard applies wildcard GPU quota only when no exact model limit exists", async ({ page }) => {
  await page.goto("/p/prj-staging/dashboard")

  const exact = page.getByRole("meter", { name: "GPU / nvidia-a10 quota" })
  await expect(exact).toHaveAttribute("aria-valuemax", "1")
  await expect(exact).toHaveAttribute("aria-valuetext", "1 used of 1")

  const fallback = page.getByRole("meter", { name: "GPU / nvidia-l40s quota" })
  await expect(fallback).toHaveAttribute("aria-valuemax", "3")
  await expect(fallback).toHaveAttribute("aria-valuetext", "2 used of 3")
  await expect(page.getByText("Uses the project fallback quota (*)", { exact: true })).toBeVisible()
  await expect(page.getByText("GPU / Other models", { exact: true })).toHaveCount(0)
})

test("dashboard switches quota scope for multi-region projects", async ({ page }) => {
  await page.goto(`${project}/dashboard`)
  await page.getByLabel("Quota location").click()
  await page.getByRole("option", { name: "Da Nang (RegionTwo)" }).click()

  await expect(page.getByLabel("Quota location")).toContainText("Da Nang (RegionTwo)")
})

test("server wizard explains and blocks a flavor over quota", async ({ page }) => {
  await page.goto(`${project}/servers/new`)
  await page.getByRole("button", { name: /Ubuntu 24\.04/ }).click()
  await page.getByRole("button", { name: /g1\.a10/ }).click()
  await page.getByText("net-private", { exact: true }).locator("..").getByRole("checkbox").click()
  await page.getByLabel("Server name").fill("quota-test")

  await expect(page.getByText("This flavor exceeds the project quota")).toBeVisible()
  await expect(page.getByRole("button", { name: "Create server" })).toBeDisabled()
})

test("volume dialog blocks capacity over storage quota", async ({ page }) => {
  await page.goto(`${project}/volumes`)
  await page.getByRole("button", { name: "Create volume" }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Name").fill("quota-test")
  await dialog.getByLabel("Size (GB)").fill("900")

  await expect(dialog.getByText("This volume exceeds the project quota")).toBeVisible()
  await expect(dialog.getByRole("button", { name: "Create volume" })).toBeDisabled()
})

test("volume dialog checks the selected Cinder volume-type quota", async ({ page }) => {
  await page.goto(`${project}/volumes`)
  await page.getByRole("button", { name: "Create volume" }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Name").fill("typed-quota-test")
  await dialog.getByLabel("Size (GB)").fill("20")
  await dialog.getByLabel("Storage type").click()
  await page.getByRole("option", { name: "High IOPS SSD" }).click()

  await expect(dialog.getByText("This volume exceeds the project quota")).toBeVisible()
  await expect(dialog.getByText(/Volume type high-iops storage/)).toBeVisible()
  await expect(dialog.getByRole("button", { name: "Create volume" })).toBeDisabled()
})

test("volume dialog requires an enabled storage type before creating", async ({ page }) => {
  await page.goto(`${project}/volumes`)
  await page.getByRole("button", { name: "Create volume" }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Name").fill("typed-required-test")
  await dialog.getByLabel("Size (GB)").fill("10")

  // Two enabled types in the mock catalog → nothing is auto-selected and the
  // create button stays disabled until the user picks one.
  await expect(dialog.getByRole("button", { name: "Create volume" })).toBeDisabled()
  await dialog.getByLabel("Storage type").click()
  await page.getByRole("option", { name: "SSD", exact: true }).click()

  await expect(dialog.getByText("This volume fits the current quota snapshot.")).toBeVisible()
  await expect(dialog.getByRole("button", { name: "Create volume" })).toBeEnabled()
})
