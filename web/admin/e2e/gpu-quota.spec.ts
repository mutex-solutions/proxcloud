import { expect, test } from "@playwright/test"

test("project quota tab shows GPU usage and editable per-model limits", async ({ page }) => {
  await page.goto("/clients/projects/prj-0001?tab=quota")
  await page.waitForLoadState("networkidle")

  await expect(page.getByRole("heading", { name: "GPU quota" })).toBeVisible()
  await expect(page.getByText("GPU devices in use")).toBeVisible()
  await expect(page.getByTestId("gpu-used-total")).toHaveText("1")
  await expect(page.getByTestId("gpu-status-nvidia-a100-80gb")).toContainText("3 remaining")
  await expect(page.getByText("nvidia-a100-80gb", { exact: true }).first()).toBeVisible()
  const limitInput = page.getByLabel("Limit for nvidia-a100-80gb")
  await expect(limitInput).toHaveValue("4")
  await limitInput.fill("")
  await expect(limitInput).toHaveAttribute("aria-invalid", "true")
  await expect(page.getByTestId("gpu-status-nvidia-a100-80gb")).toContainText("Invalid draft")
  await limitInput.fill("4")

  const documentOverflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  )
  expect(documentOverflow).toBeLessThanOrEqual(1)
})

test("project quota tab never fabricates remaining GPU capacity when usage is unavailable", async ({ page }) => {
  await page.goto("/clients/projects/prj-0002?tab=quota")
  await page.waitForLoadState("networkidle")

  const status = page.getByTestId("gpu-status-nvidia-l40s")
  await expect(status).toContainText("Unavailable")
  await expect(status).not.toContainText("remaining")
})

test("project quota tab saves a canonical per-model GPU limit", async ({ page }) => {
  await page.goto("/clients/projects/prj-0001?tab=quota")
  await page.waitForLoadState("networkidle")

  await page.getByLabel("GPU model").fill("NVIDIA_L40S")
  await page.getByLabel("Limit", { exact: true }).fill("2")
  await page.getByRole("button", { name: "Add limit" }).click()

  const limitInput = page.getByLabel("Limit for nvidia-l40s")
  await expect(limitInput).toHaveValue("2")
  await page.getByRole("button", { name: "Save quota" }).click()

  await expect(page.getByText("Project quota saved")).toBeVisible()
  await expect(limitInput).toHaveValue("2")
})
