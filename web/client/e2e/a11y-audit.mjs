// One-off aggregator: run axe across the full route×theme matrix and print a
// compact violation summary (id → pages/nodes) for batch fixing.
import { chromium } from "@playwright/test"
import { AxeBuilder } from "@axe-core/playwright"
import { routes, themes } from "./routes.ts"

const browser = await chromium.launch()
const agg = new Map()

for (const theme of themes) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } })
  await ctx.addInitScript((t) => localStorage.setItem("proxcloud.theme", t), theme)
  const page = await ctx.newPage()
  for (const route of routes) {
    await page.goto(`http://localhost:5273${route.path}`, { waitUntil: "networkidle" })
    await page.waitForTimeout(400)
    const res = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
      .exclude('button[class*="bg-primary"]')
      .exclude('a[class*="bg-primary"]')
      .analyze()
    for (const v of res.violations) {
      const key = v.id
      if (!agg.has(key)) agg.set(key, { impact: v.impact, help: v.help, hits: [] })
      agg.get(key).hits.push(`${route.name}[${theme}] ${v.nodes.slice(0, 2).map((n) => n.target.join(" ")).join(" | ")}`)
    }
  }
  await ctx.close()
}
await browser.close()

for (const [id, info] of agg) {
  console.log(`\n=== ${id} (${info.impact}) — ${info.help} — ${info.hits.length} page-hits`)
  for (const h of info.hits.slice(0, 6)) console.log("   " + h)
  if (info.hits.length > 6) console.log(`   … +${info.hits.length - 6} more`)
}
console.log("\nTOTAL violation types:", agg.size)
