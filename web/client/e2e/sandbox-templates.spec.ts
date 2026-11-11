import { expect, test, type Page } from "@playwright/test"

const project = "/p/prj-aurora"
const source = `registry.example.com/approved/agent@sha256:${"a".repeat(64)}`

async function templates(page: Page, pid = project) {
  await page.goto(`${pid}/sandboxes`)
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
  await expect(page.getByRole("button", { name: "Import image digest", exact: true }).first()).toBeVisible()
}
async function actions(page: Page, name = "web-app") {
  await page.getByRole("button", { name: `Actions for ${name}`, exact: true }).click()
}

test("imports an immutable digest and clears registry credentials after close", async ({ page }) => {
  await templates(page)
  await page.getByRole("button", { name: "Import image digest", exact: true }).first().click()
  const dialog = page.getByRole("dialog", { name: "Import image digest", exact: true })
  await dialog.getByLabel("Image digest", { exact: true }).fill(source)
  await dialog.getByLabel("Template name", { exact: true }).fill("import-agent")
  await dialog.getByLabel("Runtime vCPU", { exact: true }).fill("2")
  await dialog.getByLabel("Runtime memory (GiB)", { exact: true }).fill("4")
  await dialog.getByRole("switch", { name: "Private registry", exact: true }).click()
  await dialog.getByLabel("Registry username", { exact: true }).fill("caller-pull")
  await dialog.getByLabel("Registry password", { exact: true }).fill("ephemeral-test-only")
  await dialog.getByRole("button", { name: "Import image", exact: true }).click()
  await expect(dialog).not.toBeVisible()
  await expect(page.getByText("import-agent", { exact: true })).toBeVisible()
  await expect(page.getByText("ephemeral-test-only")).toHaveCount(0)
  const storage = await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))
  expect(storage).not.toContain("ephemeral-test-only")
  await page.getByRole("button", { name: "Import image digest", exact: true }).first().click()
  await expect(page.getByRole("dialog").getByLabel("Image digest", { exact: true })).toHaveValue("")
  await page.getByRole("dialog").getByRole("switch", { name: "Private registry", exact: true }).click()
  await expect(page.getByRole("dialog").getByLabel("Registry password", { exact: true })).toHaveValue("")
})

test("accepted import refreshes its project even after the dialog is closed", async ({ page }) => {
  await templates(page)
  await page.getByRole("button", { name: "Import image digest", exact: true }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Image digest", { exact: true }).fill(source)
  await dialog.getByLabel("Template name", { exact: true }).fill("slow-import")
  await dialog.getByRole("button", { name: "Import image", exact: true }).click()
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click()
  await expect(dialog).not.toBeVisible()
  await expect(page.getByText("slow-import", { exact: true })).toBeVisible()
})

test("validates digest and keeps an import error retryable", async ({ page }) => {
  await templates(page)
  await page.getByRole("button", { name: "Import image digest", exact: true }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Image digest", { exact: true }).fill("registry.example.com/approved/agent:latest")
  await dialog.getByLabel("Template name", { exact: true }).fill("invalid-agent")
  await expect(dialog.getByRole("button", { name: "Import image", exact: true })).toBeDisabled()
  await dialog.getByLabel("Image digest", { exact: true }).fill(source)
  await dialog.getByLabel("Template name", { exact: true }).fill("import-denied")
  await dialog.getByRole("button", { name: "Import image", exact: true }).click()
  await expect(dialog.getByRole("alert")).toBeVisible()
  await expect(dialog.getByRole("button", { name: "Import image", exact: true })).toBeEnabled()
})

test("manages aliases without mutating primary name or tags", async ({ page }) => {
  await templates(page)
  await actions(page)
  await page.getByRole("menuitem", { name: "Manage aliases", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "Manage aliases", exact: true })
  await expect(dialog.getByLabel("Primary name (immutable)", { exact: true })).toHaveValue("web-app")
  await expect(dialog.getByLabel("Primary name (immutable)", { exact: true })).toHaveAttribute("readonly", "")
  await dialog.getByLabel("New alias", { exact: true }).fill("review-agent")
  await dialog.getByRole("button", { name: "Add alias", exact: true }).click()
  await dialog.getByRole("button", { name: "Confirm add", exact: true }).click()
  await expect(dialog.getByRole("button", { name: "Remove alias review-agent", exact: true })).toBeVisible()
  await dialog.getByRole("button", { name: "Remove alias review-agent", exact: true }).click()
  await dialog.getByRole("button", { name: "Confirm remove", exact: true }).click()
  await expect(dialog.getByRole("button", { name: "Remove alias review-agent", exact: true })).toHaveCount(0)
})

test("prepares image and observes fresh N/M coverage", async ({ page }) => {
  await templates(page)
  await actions(page)
  await page.getByRole("menuitem", { name: "Prepare", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "Prepare: web-app", exact: true })
  await dialog.getByRole("button", { name: "Prepare image", exact: true }).click()
  await expect(dialog.getByText("Coverage: 3/3 nodes", { exact: true })).toBeVisible({ timeout: 20000 })
  await expect(dialog.getByText(/not.*guarantee|best-effort/i).first()).toBeVisible()
  await dialog.getByRole("button", { name: "Close", exact: true }).first().click()
  await expect(dialog).not.toBeVisible()
})

test("accepted prepare fences an older GET and keeps polling active intent", async ({ page }) => {
  await templates(page)
  await page.evaluate(async () => {
    const apiPath = "/src/lib/api.ts", routerPath = "/src/mocks/router.ts"
    const { setMockHandler } = await import(/* @vite-ignore */ apiPath)
    const { dispatch } = await import(/* @vite-ignore */ routerPath)
    const control = { reads: 0, posted: false, released: false, release: () => {} }
    ;(window as unknown as { coverageRace: typeof control }).coverageRace = control
    setMockHandler((path: string, opts: { method?: string }) => {
      if (!path.endsWith("/prepare")) return dispatch(path, opts)
      if (opts.method === "POST") { control.posted = true; return dispatch(path, opts) }
      control.reads++
      if (control.reads === 1) return new Promise((resolve) => {
        control.release = () => { control.released = true; resolve({ data: { enabled: true, image: "old-image", pool: "cpu", pulled: 0, total: 3, ready: false } }) }
      })
      return dispatch(path, opts)
    })
  })
  await actions(page)
  await page.getByRole("menuitem", { name: "Prepare", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "Prepare: web-app", exact: true })
  await expect.poll(() => page.evaluate(() => (window as unknown as { coverageRace: { reads: number } }).coverageRace.reads)).toBe(1)
  await dialog.getByRole("button", { name: "Prepare image", exact: true }).click()
  await expect(dialog.getByText("Coverage: 0/3 nodes", { exact: true })).toBeVisible()
  await expect(dialog.getByText("Waiting for image pulls on eligible nodes.", { exact: false })).toBeVisible()
  await page.evaluate(() => (window as unknown as { coverageRace: { release: () => void } }).coverageRace.release())
  await expect.poll(() => page.evaluate(() => (window as unknown as { coverageRace: { reads: number } }).coverageRace.reads), { timeout: 10000 }).toBeGreaterThan(1)
  await expect(dialog.getByText("Coverage: 3/3 nodes", { exact: true })).toBeVisible({ timeout: 20000 })
  await expect(dialog.getByText(/No active preparation, or the preparation has expired/)).toHaveCount(0)
})

test("shows disabled location capabilities without fake success", async ({ page }) => {
  await templates(page, "/p/prj-staging")
  await expect(page.getByRole("button", { name: "Import image digest", exact: true }).first()).toBeDisabled()
  await actions(page)
  await page.getByRole("menuitem", { name: "Prepare", exact: true }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByText(/disabled|not enabled|not available/i).first()).toBeVisible()
  await expect(dialog.getByRole("button", { name: "Prepare image", exact: true })).toBeDisabled()
})

test("starts a build with resources independent from runtime size", async ({ page }) => {
  await templates(page)
  await actions(page)
  await page.getByRole("menuitem", { name: "New build", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "New build", exact: true })
  await dialog.getByLabel("New template name", { exact: true }).fill("agent-child")
  await dialog.getByLabel("Runtime vCPU", { exact: true }).fill("1")
  await dialog.getByLabel("Runtime memory (GiB)", { exact: true }).fill("1")
  await dialog.getByLabel("Builder vCPU", { exact: true }).fill("4")
  await dialog.getByLabel("Builder memory (GiB)", { exact: true }).fill("24")
  await dialog.getByLabel("Builder disk (GiB)", { exact: true }).fill("120")
  await dialog.getByLabel("RUN command 1", { exact: true }).fill("echo setup-complete")
  await dialog.getByRole("button", { name: "Start build", exact: true }).click()
  await expect(dialog).not.toBeVisible()
  await expect(page.getByText("agent-child", { exact: true })).toBeVisible()
})

test("shows phase diagnostics, resource budgets, termination reason and log warnings", async ({ page }) => {
  await templates(page)
  await actions(page)
  await page.getByRole("menuitem", { name: "Builds and logs", exact: true }).click()
  const dialog = page.getByRole("dialog")
  await dialog.getByRole("button", { name: /Logs|Diagnostics/ }).first().click()
  await expect(dialog.getByText("OOMKilled", { exact: false }).first()).toBeVisible()
  await expect(dialog.getByText(/137/).first()).toBeVisible()
  await expect(dialog.getByText(/log.*unavailable|could not be captured|Some output/i).first()).toBeVisible()
  await expect(dialog.getByText(/truncat|archive limit/i).first()).toBeVisible()
  await expect(dialog.getByLabel("Build logs")).toContainText("build step exited 137")
  await expect(dialog.getByLabel("Build logs")).not.toContainText("\u001b")
  await expect(dialog.getByLabel("Build logs")).toContainText("literal <script>not HTML</script>")
  await expect(dialog.getByLabel("Build logs").locator("script, a")).toHaveCount(0)
})

test("an older active build keeps mutation controls busy even when latest has failed", async ({ page }) => {
  await page.goto(`${project}/sandboxes?older-active-build`)
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
  await actions(page)
  await expect(page.getByRole("menuitem", { name: "Manage aliases", exact: true })).toBeDisabled()
  await expect(page.getByRole("menuitem", { name: "New build", exact: true })).toBeDisabled()
  await expect(page.getByRole("menuitem", { name: "Builds and logs", exact: true })).toBeEnabled()
})

test("active-worker delete stays open and retryable, never a build cancellation", async ({ page }) => {
  await page.goto(`${project}/sandboxes?older-active-build`)
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
  await actions(page)
  await page.getByRole("menuitem", { name: "Delete", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "Delete template?", exact: true })
  await dialog.getByRole("button", { name: "Delete", exact: true }).click()
  await expect(dialog.getByRole("alert")).toContainText("Nothing was deleted")
  await expect(dialog.getByRole("button", { name: "Delete", exact: true })).toBeEnabled()
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click()
  await expect(dialog).not.toBeVisible()
  await expect(page.getByText("web-app", { exact: true })).toBeVisible()
})

test("capability failure disables new work but keeps history readable", async ({ page }) => {
  await page.goto(`${project}/sandboxes?capabilities-error`)
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
  await expect(page.getByRole("button", { name: "Retry capabilities", exact: true })).toBeVisible()
  await expect(page.getByRole("button", { name: "Import image digest", exact: true }).first()).toBeDisabled()
  await actions(page)
  await expect(page.getByRole("menuitem", { name: "New build", exact: true })).toBeDisabled()
  await page.getByRole("menuitem", { name: "Builds and logs", exact: true }).click()
  await expect(page.getByRole("dialog")).toBeVisible()
})

test("console reads require project management permission", async ({ page }) => {
  await page.goto(`${project}/sandboxes?template-manage-denied`)
  await page.getByRole("tab", { name: "Templates", exact: true }).click()
  await expect(page.getByRole("alert").filter({ hasText: "Project cloud resource management permission required" })).toBeVisible()
  await expect(page.getByText("tpl-web-app", { exact: true })).toHaveCount(0)
})

test("sandbox guide and both API references resolve through the target manifest", async ({ page }) => {
  for (const [slug, title] of [["guides/sandboxes", "Sandboxes"], ["api/e2b", "E2B API"], ["api/sandboxes", "Sandbox API"]]) {
    await page.goto(`/docs/${slug}`)
    await expect(page.getByRole("heading", { level: 1, name: title, exact: true })).toBeVisible()
    await expect(page.getByText("Page not found", { exact: true })).toHaveCount(0)
  }
})

test("new controls reflow on a narrow viewport", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await templates(page)
  await page.getByRole("button", { name: "Import image digest", exact: true }).first().click()
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(1)
  await expect(page.getByRole("dialog").getByRole("button", { name: "Import image", exact: true })).toBeVisible()
})
