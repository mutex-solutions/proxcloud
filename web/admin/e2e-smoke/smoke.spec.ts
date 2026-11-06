import { expect, test } from "@playwright/test"

// Opens the production build: a broken bundle (react and react-dom on different versions, a chunk
// that throws at load) passes tsc and vite build and still renders a white page.
test("the built console renders its login page", async ({ page }) => {
  const errors: string[] = []
  page.on("pageerror", (e) => errors.push(e.message))
  await page.goto("/")
  await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible()
  expect(errors).toEqual([])
})
