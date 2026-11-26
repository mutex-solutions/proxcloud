import { expect, test } from "@playwright/test"
import { calls, navigate, recordCalls } from "./mock-calls.ts"

// With the job scheduler off the API refuses a project's delete, with the grace period as well as "Delete now": no
// job would carry it out (lab drill of v0.11.1, R4-o2). The page shows the API's own words and stays on the project.
const refusal = "This installation cannot delete a project yet: the job that deletes a project and its cloud resources runs only with the job scheduler, and the scheduler is off. Nothing was changed."

test("a scheduled delete the API refuses shows why, as Delete now does", async ({ page }) => {
  await page.goto("/clients/projects")
  await expect(page.getByRole("table")).toBeVisible()
  await recordCalls(page, {
    "DELETE /admin/project/prj-0001": { apiError: { status: 409, message: refusal } },
    "DELETE /admin/project/prj-0001/now": { apiError: { status: 409, message: refusal } },
  })
  await navigate(page, "/clients/projects/prj-0001")
  await page.getByRole("button", { name: "Delete project" }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Schedule deletion" }).click()
  await expect.poll(() => calls(page, "DELETE", "/admin/project/prj-0001")).toHaveLength(1)
  await expect(page.getByText(refusal)).toBeVisible()
  await expect(page.getByText("Project deletion scheduled")).toHaveCount(0)
  await expect(page).toHaveURL(/\/clients\/projects\/prj-0001$/)
})
