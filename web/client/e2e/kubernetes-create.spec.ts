import { expect, test, type Page } from "@playwright/test"

// The first cluster of a project (mock seeds in src/mocks/fixtures/cloud.ts): the form asks a new tenant for a name
// and a flavor, and for nothing about the network that the project's state already answers.
const base = "/p/prj-aurora/kubernetes"

// record keeps every create the form sends; setup changes the mock's rows as another session would.
async function open(page: Page, setup = "") {
  // The mock is changed on a page that has not read the networks, then the route changes inside the app: a page load
  // would start the mock anew.
  await page.goto("/p/prj-aurora/databases")
  await expect(page.getByRole("heading", { name: "Databases" }).first()).toBeVisible()
  await page.evaluate(async (src) => {
    const apiPath = "/src/lib/api.ts", routerPath = "/src/mocks/router.ts", dbPath = "/src/mocks/db.ts"
    const { setMockHandler } = await import(/* @vite-ignore */ apiPath)
    const { dispatch } = await import(/* @vite-ignore */ routerPath)
    const { db } = await import(/* @vite-ignore */ dbPath)
    new Function("db", src)(db)
    const w = window as unknown as { __creates: unknown[] }
    w.__creates = []
    setMockHandler(async (path: string, opts: { method?: string; body?: { type?: string } }) => {
      if (path.endsWith("/cloud") && opts.method === "POST") w.__creates.push(opts.body)
      return dispatch(path, opts)
    })
  }, setup)
  await page.evaluate((u) => {
    history.pushState({}, "", u)
    dispatchEvent(new PopStateEvent("popstate"))
  }, base)
  await expect(page.getByRole("row").filter({ hasText: "prod" })).toBeVisible()
  await page.getByRole("button", { name: "Create cluster" }).first().click()
  return page.getByRole("dialog")
}
const creates = (page: Page) => page.evaluate(() => (window as unknown as { __creates: { type: string; data: Record<string, any> }[] }).__creates)

async function nameAndFlavor(page: Page) {
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Name", { exact: true }).first().fill("first")
  await dialog.getByRole("combobox").filter({ hasText: "Pick a flavor" }).click()
  await page.getByRole("option").filter({ hasNot: page.locator("[data-disabled]") }).first().click()
}

test("with one network that can hold the nodes, the form needs a name and a flavor only", async ({ page }) => {
  const dialog = await open(page)
  await expect(dialog.getByRole("combobox", { name: "Network" })).toContainText("net-private")
  await nameAndFlavor(page)
  await dialog.getByRole("button", { name: "Create cluster" }).click()
  await expect(page.getByText("Cluster \"first\" is being created")).toBeVisible()
  const [sent] = (await creates(page)).filter((c) => c.type === "KUBERNETES_CLUSTER")
  expect(sent.data.networkId).toBe("net-priv-1")
  expect(sent.data.version).toBe("1.35.10") // the lowest offered version is the default
})

test("a project without a network makes one from the form", async ({ page }) => {
  const dialog = await open(page, `db.cloud = db.cloud.filter((r) => r.type !== "NETWORK")`)
  await expect(dialog.getByText("This project has no network yet.")).toBeVisible()
  await nameAndFlavor(page)
  await dialog.getByRole("button", { name: "Create a network for this cluster" }).click()
  await expect(dialog.getByRole("combobox", { name: "Network" })).toContainText("first-net")
  await expect(dialog.getByText("This project has no network yet.")).toHaveCount(0)
  await dialog.getByRole("button", { name: "Create cluster" }).click()
  await expect(page.getByText("Cluster \"first\" is being created")).toBeVisible()
  const sent = await creates(page)
  expect(sent.map((c) => c.type)).toEqual(["NETWORK", "KUBERNETES_CLUSTER"])
  expect(sent[0].data).toEqual({ name: "first-net" })
  expect(sent[1].data.networkId).toBeTruthy()
})

test("a network with DHCP cannot be picked, and the form says who can change that", async ({ page }) => {
  const dialog = await open(page, `for (const r of db.cloud) if (r.type === "NETWORK" && r.name === "net-private") r.data.network.dhcp = true`)
  await expect(dialog.getByRole("status").filter({ hasText: "Every network of this project hands out addresses by DHCP" })).toContainText("ask the operator of this site")
  await expect(dialog.getByRole("button", { name: "Create a network for this cluster" })).toHaveCount(0)
  await dialog.getByRole("combobox", { name: "Network" }).click()
  await expect(page.getByRole("option", { name: /net-private \(has DHCP: not for cluster nodes\)/ })).toHaveAttribute("data-disabled", "")
})
