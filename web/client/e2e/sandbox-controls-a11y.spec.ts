import { AxeBuilder } from "@axe-core/playwright"
import { expect, test } from "@playwright/test"

for (const theme of ["light", "dark"]) {
  for (const width of [1280, 390]) {
    test(`sandbox template dialogs are accessible [${theme}, ${width}px]`, async ({ page }) => {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 900 })
      await page.emulateMedia({ reducedMotion: "reduce" })
      await page.addInitScript((value) => localStorage.setItem("proxcloud.theme", value), theme)
      await page.goto("/p/prj-aurora/sandboxes")
      await page.getByRole("tab", { name: "Templates", exact: true }).click()
      const check = async () => {
        const dialog = page.getByRole("dialog")
        await expect(dialog).toBeVisible()
        const results = await new AxeBuilder({ page })
          .include('[role="dialog"]')
          .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
          .analyze()
        // No contrast exceptions: owned buttons use accessible text or outlines.
        expect(results.violations.map((item) => ({ id: item.id, impact: item.impact, nodes: item.nodes.map((node) => node.target) }))).toEqual([])
        expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(1)
        expect(await dialog.evaluate((node) => node.scrollWidth - node.clientWidth)).toBeLessThanOrEqual(1)
      }
      const importer = page.getByRole("button", { name: "Import image digest", exact: true }).first()
      // Native disabled buttons cannot receive focus. Wait for the asynchronous
      // capabilities query before exercising real keyboard activation.
      await expect(importer).toBeEnabled()
      await importer.focus()
      await expect(importer).toBeFocused()
      await page.keyboard.press("Enter")
      await check()
      await page.getByRole("dialog").getByRole("switch", { name: "Private registry", exact: true }).click()
      await check()
      await page.keyboard.press("Escape")
      await expect(page.getByRole("dialog")).not.toBeVisible()
      await expect(importer).toBeFocused()
      for (const action of ["Manage aliases", "Prepare", "New build", "Builds and logs"]) {
        const opener = page.getByRole("button", { name: "Actions for web-app", exact: true })
        await expect(opener).toBeEnabled()
        await opener.focus()
        await expect(opener).toBeFocused()
        await page.keyboard.press("Enter")
        await page.getByRole("menuitem", { name: action, exact: true }).click()
        await check()
        if (action === "Manage aliases") {
          await page.getByRole("dialog").getByLabel("New alias", { exact: true }).fill("a11y-review")
          await page.getByRole("dialog").getByRole("button", { name: "Add alias", exact: true }).click()
          await check()
        }
        if (action === "Builds and logs") {
          await page.getByRole("dialog").getByRole("button", { name: "Diagnostics and logs", exact: true }).first().click()
          await expect(page.getByRole("dialog").getByLabel("Build logs")).toContainText("build step exited 137")
          await check()
        }
        await page.keyboard.press("Escape")
        await expect(page.getByRole("dialog")).not.toBeVisible()
        await expect(opener).toBeFocused()
      }
    })
  }
}
