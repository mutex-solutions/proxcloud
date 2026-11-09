import { expect, test, type Page } from "@playwright/test"

const path = "/p/prj-aurora/sandboxes"
// Simulate same-SPA navigation, not page.goto: a reload would trivially clear
// local dialog state and would miss an old completion affecting the new project.
async function switchProject(page: Page, pid: string) {
  await page.evaluate((next) => {
    window.history.pushState({}, "", `/p/${next}/sandboxes`)
    window.dispatchEvent(new PopStateEvent("popstate"))
  }, pid)
}
async function templates(page: Page) {
  await page.goto(path)
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
}

test("collapsing diagnostics unmounts its panel and Escape restores action focus", async ({ page }) => {
  await templates(page)
  const trigger = page.getByRole("button", { name: "Actions for web-app", exact: true })
  await trigger.click()
  await page.getByRole("menuitem", { name: "Builds and logs", exact: true }).click()
  const dialog = page.getByRole("dialog")
  await dialog.getByRole("button", { name: "Diagnostics and logs", exact: true }).first().click()
  await expect(dialog.getByLabel("Build logs")).toBeVisible()
  await dialog.getByRole("button", { name: "Hide diagnostics and logs", exact: true }).click()
  await expect(dialog.getByLabel("Build logs")).toHaveCount(0)
  await page.keyboard.press("Escape")
  await expect(dialog).not.toBeVisible()
  await expect(trigger).toBeFocused()
})

test("same-SPA project switch clears private form memory and does not show another project's template", async ({ page }) => {
  await templates(page)
  await expect(page.getByText("tpl-web-app", { exact: true })).toBeVisible()
  await page.getByRole("button", { name: "Import image digest", exact: true }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByRole("switch", { name: "Private registry", exact: true }).click()
  await dialog.getByLabel("Registry username", { exact: true }).fill("caller-only")
  await dialog.getByLabel("Registry password", { exact: true }).fill("do-not-retain-project-secret")
  await switchProject(page, "prj-staging")
  await expect(page.getByRole("dialog")).not.toBeVisible()
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
  await expect(page.getByText("tpl-staging-app", { exact: true })).toBeVisible()
  await expect(page.getByText("tpl-web-app", { exact: true })).toHaveCount(0)
  await switchProject(page, "prj-aurora")
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
  await page.getByRole("button", { name: "Import image digest", exact: true }).first().click()
  await page.getByRole("dialog").getByRole("switch", { name: "Private registry", exact: true }).click()
  await expect(page.getByRole("dialog").getByLabel("Registry password", { exact: true })).toHaveValue("")
  await expect(page.getByRole("dialog").getByLabel("Registry username", { exact: true })).toHaveValue("")
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).not.toContain("do-not-retain-project-secret")
})

test("a late accepted import invalidates its captured project without affecting the new project's dialog", async ({ page }) => {
  await templates(page)
  await page.getByRole("button", { name: "Import image digest", exact: true }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Image digest", { exact: true }).fill(`registry.example.com/approved/agent@sha256:${"a".repeat(64)}`)
  await dialog.getByLabel("Template name", { exact: true }).fill("slow-import")
  await dialog.getByRole("button", { name: "Import image", exact: true }).click()
  await expect(dialog.getByRole("button", { name: "Importing…", exact: true })).toBeDisabled()
  await switchProject(page, "prj-staging")
  await expect(page.getByRole("dialog")).not.toBeVisible()
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
  await page.getByRole("button", { name: "Actions for web-app", exact: true }).click()
  await page.getByRole("menuitem", { name: "Manage aliases", exact: true }).click()
  const current = page.getByRole("dialog", { name: "Manage aliases", exact: true })
  await expect(current).toBeVisible()
  await current.getByLabel("New alias", { exact: true }).fill("staging-unsaved")
  // A positive completion marker in the old project's private fixture is safer
  // than a timed sleep; request bodies/secrets are never returned by this probe.
  await expect.poll(async () => page.evaluate(async () => {
    const mockModule = "/src/mocks/db.ts"
    const { db } = await import(/* @vite-ignore */ mockModule)
    return db.sandboxTemplates.some((t: { projectId: string; name: string }) => t.projectId === "prj-aurora" && t.name === "slow-import")
  })).toBe(true)
  await expect(current).toBeVisible()
  await expect(current.getByLabel("New alias", { exact: true })).toHaveValue("staging-unsaved")
  await expect(page.getByText("slow-import", { exact: true })).toHaveCount(0)
  await switchProject(page, "prj-aurora")
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
  await expect(page.getByText("slow-import", { exact: true })).toBeVisible()
})

test("all mock template routes bind to URL project despite spoofed body and headers", async ({ page }) => {
  await templates(page)
  const statuses = await page.evaluate(async () => {
    const mockModule = "/src/mocks/router.ts"
    const { dispatch } = await import(/* @vite-ignore */ mockModule)
    const root = "/project/prj-staging/sandbox-templates/tpl-web-app"
    const routes = [
      ["GET", `${root}/builds/build-ready/logs`, undefined],
      ["GET", `${root}/prepare`, undefined],
      ["POST", `${root}/prepare`, undefined],
      ["POST", `${root}/aliases`, { aliases: ["cross-project"], projectId: "prj-aurora" }],
      ["DELETE", `${root}/aliases`, { aliases: ["app-stable"] }],
      ["POST", `${root}/builds`, { name: "cross-project", projectId: "prj-aurora" }],
      ["DELETE", root, undefined],
    ] as const
    return Promise.all(routes.map(async ([method, path, body]) => {
      try { await dispatch(path, { method, body, headers: { "x-project-id": "prj-aurora", "x-service-id": "spoofed" } }); return 200 }
      catch (error) { return (error as { status: number }).status }
    }))
  })
  expect(statuses).toEqual([404, 404, 404, 404, 404, 404, 404])
})
