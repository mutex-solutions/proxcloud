// Screenshot matrix: every route × {light, dark} against the mock app.
// Output: e2e/shots/<name>-<theme>.png — the gallery reviewed during the
// restyle. Not a pixel-diff suite; it asserts the page rendered (no auth
// wall, no crash) and captures the state of the world.
import { expect, test } from "@playwright/test"
import { routes, themes } from "./routes.ts"

// Desktop is the design target; mobile (iPhone-ish) is the reflow check —
// the page must render, navigate, and never force document-level horizontal
// scroll.
const viewports = [
  { name: "desktop", width: 1280, height: 800 },
  { name: "mobile", width: 390, height: 844 },
] as const

for (const vp of viewports) {
  for (const theme of themes) {
    for (const route of routes) {
      test(`shot ${route.name} [${theme}] [${vp.name}]`, async ({ page }) => {
        await page.setViewportSize({ width: vp.width, height: vp.height })
        await page.emulateMedia({ reducedMotion: "reduce" })
        await page.addInitScript((t) => localStorage.setItem("proxcloud.theme", t), theme)
        await page.goto(route.path)
        await page.waitForLoadState("networkidle")
        await page.evaluate(() => document.fonts.ready)
        // settle: mock latency + suspense chunks
        await page.waitForTimeout(500)

        const text = await page.evaluate(() => document.body.innerText)
        expect(text.length, "page should render content").toBeGreaterThan(40)
        if (!route.public) {
          expect(text, "should not hit the auth wall").not.toMatch(/sign in to continue/i)
        }
        // WCAG reflow floor: only inner containers (tables) may scroll
        // horizontally, never the document.
        const docOverflow = await page.evaluate(
          () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
        )
        expect(docOverflow, "no document-level horizontal scroll").toBeLessThanOrEqual(1)

        await page.screenshot({
          path: `e2e/shots/${route.name}-${theme}-${vp.name}.png`,
          animations: "disabled",
          fullPage: false,
        })
      })
    }
  }
}
