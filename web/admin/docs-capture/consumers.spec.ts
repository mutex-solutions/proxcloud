import { expect, test } from "@playwright/test"

const pages = [
  "platform-admin/cloud/connect-a-cloud", "platform-admin/cloud/service-availability",
  "platform-admin/account-activation", "platform-admin/billing/price-plans",
  "platform-admin/billing/currency", "platform-admin/billing/invoicing",
  "platform-admin/billing/suspension", "platform-admin/billing/signup-credits",
  "platform-admin/billing/tax", "platform-admin/billing/savings-plans",
]

test("published admin documentation consumers load their current illustrations", async ({ page }) => {
  const errors: string[] = []
  const blocked: string[] = []
  page.on("pageerror", (error) => errors.push(error.message))
  page.on("console", (message) => { if (message.type() === "error") errors.push(message.text()) })
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url())
    if (url.origin === "http://127.0.0.1:5274" && !url.pathname.startsWith("/api/")) return route.continue()
    blocked.push(url.origin + url.pathname)
    await route.abort("blockedbyclient")
  })
  for (const slug of pages) {
    await page.goto(`/docs/${slug}`)
    await expect(page.locator("main h1")).toBeVisible()
    const images = page.locator('main img[src^="/docs-img/"]')
    await expect(images).not.toHaveCount(0)
    for (const image of await images.all()) {
      await image.scrollIntoViewIfNeeded()
      await expect.poll(() => image.evaluate((element: HTMLImageElement) => element.complete && element.naturalWidth === 1440 && element.naturalHeight === 1000)).toBe(true)
      expect(await image.getAttribute("alt")).toBeTruthy()
    }
  }
  expect(blocked).toEqual([])
  expect(errors).toEqual([])
})
