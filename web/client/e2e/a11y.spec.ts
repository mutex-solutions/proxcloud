// Axe accessibility matrix: every route × {light, dark}.
// The single documented exception is white-on-Blaze-Orange primary buttons
// (3.10:1 brand exception, mirroring mono's BRAND_ORANGE_A11Y) — excluded by
// node, never by disabling the color-contrast rule globally.
import { AxeBuilder } from "@axe-core/playwright"
import { expect, test } from "@playwright/test"
import { routes, themes } from "./routes.ts"

for (const theme of themes) {
  for (const route of routes) {
    test(`axe ${route.name} [${theme}]`, async ({ page }) => {
      await page.emulateMedia({ reducedMotion: "reduce" })
      await page.addInitScript((t) => localStorage.setItem("proxcloud.theme", t), theme)
      await page.goto(route.path)
      await page.waitForLoadState("networkidle")
      await page.waitForTimeout(500)

      const results = await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
        // Documented Blaze Orange brand exception: primary buttons only.
        .exclude('button[class*="bg-primary"]')
        .exclude('a[class*="bg-primary"]')
        .analyze()

      const violations = results.violations.map((v) => ({
        id: v.id,
        impact: v.impact,
        nodes: v.nodes.slice(0, 5).map((n) => n.target.join(" ")),
      }))
      expect(violations).toEqual([])
    })
  }
}
