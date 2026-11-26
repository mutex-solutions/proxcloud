import { expect, test } from "@playwright/test"

// With the job scheduler off the API refuses a project's delete, with the grace period as well as "Delete now": no
// job would carry it out (lab drill of v0.11.1, R4-o2). The page shows the API's own words and the project stays.
const refusal = "This installation cannot delete a project yet: the job that deletes a project and its cloud resources runs only with the job scheduler, and the scheduler is off. Nothing was changed."

test("a scheduled delete the API refuses shows why and leaves the project as it is", async ({ page }) => {
  await page.goto("/p/prj-aurora/org/projects")
  await expect(page.getByRole("button", { name: "Actions for aurora-staging" })).toBeVisible()
  // The mock answers in the page: the two delete routes are refused as the API refuses them.
  await page.evaluate(async (message) => {
    const apiPath = "/src/lib/api.ts", routerPath = "/src/mocks/router.ts"
    const { ApiError, setMockHandler } = await import(/* @vite-ignore */ apiPath)
    const { dispatch } = await import(/* @vite-ignore */ routerPath)
    setMockHandler((path: string, opts: { method?: string }) =>
      opts.method === "DELETE" && /^\/project\/[^/]+(\/now)?$/.test(path)
        ? Promise.reject(new ApiError(409, 409, message))
        : dispatch(path, opts))
  }, refusal)
  await page.getByRole("button", { name: "Actions for aurora-staging" }).click()
  await page.getByRole("menuitem", { name: /Delete/ }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Schedule deletion" }).click()
  await expect(page.getByText(refusal)).toBeVisible()
  await expect(page.getByText("Project scheduled for deletion")).toHaveCount(0)
  // The dialog stays open on the project: nothing was scheduled.
  await expect(page.getByRole("dialog").getByRole("button", { name: "Schedule deletion" })).toBeEnabled()
})
