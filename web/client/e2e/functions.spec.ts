import { AxeBuilder } from "@axe-core/playwright"
import { expect, test, type Page } from "@playwright/test"

const path = "/p/prj-aurora/functions"

// The mock answers every call the page makes: a call without a handler fails the test that made it.
let unanswered: string[]
test.beforeEach(({ page }) => {
  unanswered = []
  page.on("console", (m) => { if (m.text().includes("[mock] no handler")) unanswered.push(m.text()) })
})
test.afterEach(() => expect(unanswered).toEqual([]))

// Everything the page keeps outside its form fields: the query cache (answers), the mutation cache
// (variables, answers, errors) and the browser's storage. A secret value or a registry password
// must be in none of them once it is saved. The client is found through the React tree, so the
// app exposes nothing for this.
const kept = (page: Page) => page.evaluate(() => {
  const root = document.getElementById("root") as unknown as Record<string, unknown>
  type Fiber = { memoizedProps?: { client?: any }; child?: Fiber; sibling?: Fiber }
  let client: any
  const walk = (f?: Fiber) => {
    for (; f && !client; f = f.sibling) {
      if (f.memoizedProps?.client?.getQueryCache) client = f.memoizedProps.client
      else walk(f.child)
    }
  }
  walk(root[Object.keys(root).find((k) => k.startsWith("__reactContainer$"))!] as Fiber)
  const queries = client.getQueryCache().getAll().map((q: any) => [q.queryKey, q.state.data])
  const mutations = client.getMutationCache().getAll().map((m: any) => [m.state.variables, m.state.data, m.state.error?.message])
  if (!queries.some(([key]: [unknown[]]) => key[0] === "functions")) throw new Error("the query cache was not found")
  return JSON.stringify([queries, mutations, { ...localStorage }, { ...sessionStorage }])
})

test("creates a function, deploys a new version and rolls back", async ({ page }) => {
  await page.goto(path)
  await page.getByRole("button", { name: "New function", exact: true }).click()
  const form = page.getByRole("dialog", { name: "New function", exact: true })
  const create = form.getByRole("button", { name: "Create function", exact: true })
  await form.getByLabel("Name", { exact: true }).fill("Bad_Name")
  await form.getByLabel("Image", { exact: true }).fill("ghcr.io/acme/echo:1")
  await expect(create).toBeDisabled()
  await form.getByLabel("Name", { exact: true }).fill("echo")
  await form.getByLabel("Environment variables", { exact: true }).fill("PORT=1")
  await expect(create).toBeDisabled()
  await form.getByLabel("Environment variables", { exact: true }).fill("GREETING=hi")
  await create.click()
  await expect(form).not.toBeVisible()

  const row = page.getByRole("row").filter({ hasText: "echo-" })
  await expect(row.getByText(/https:\/\/echo-[0-9a-f]{6}\.sbx\.example\.com/)).toBeVisible()
  // The next new function starts empty, not with this one's name, image and env.
  await page.getByRole("button", { name: "New function", exact: true }).click()
  await expect(form.getByLabel("Name", { exact: true })).toHaveValue("")
  await expect(form.getByLabel("Environment variables", { exact: true })).toHaveValue("")
  await form.getByRole("button", { name: "Cancel", exact: true }).click()
  await expect(form).not.toBeVisible()
  await expect(row.getByText("ready", { exact: true })).toBeVisible({ timeout: 15_000 })

  await page.getByRole("button", { name: "Actions for echo", exact: true }).click()
  await page.getByRole("menuitem", { name: "Deploy new version", exact: true }).click()
  const deploy = page.getByRole("dialog", { name: "Deploy new version of echo", exact: true })
  await expect(deploy.getByLabel("Environment variables", { exact: true })).toHaveValue("GREETING=hi")
  await deploy.getByLabel("Image", { exact: true }).fill("ghcr.io/acme/echo:2")
  await deploy.getByRole("button", { name: "Deploy", exact: true }).click()
  await expect(deploy).not.toBeVisible()
  await expect(row.getByText("#2 ·")).toBeVisible({ timeout: 15_000 })

  await page.getByRole("button", { name: "Actions for echo", exact: true }).click()
  await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
  const detail = page.getByRole("dialog", { name: "echo", exact: true })
  await detail.getByRole("tab", { name: "Deployments", exact: true }).click()
  await detail.getByRole("button", { name: "Roll back to deployment 1", exact: true }).click()
  await expect(detail.getByText("rollback to #1", { exact: true })).toBeVisible()
  // Deployed by tag, rolled back by the digest it ran: not by what the tag points at today.
  await expect(detail.getByRole("listitem").filter({ hasText: "rollback to #1" }).getByText(/^ghcr\.io\/acme\/echo@sha256:[0-9a-f]{64}$/)).toBeVisible()
  await detail.getByRole("tab", { name: "Logs", exact: true }).click()
  await expect(detail.getByLabel("Function logs")).toContainText("listening on :8080")
  await detail.getByRole("button", { name: "Close", exact: true }).first().click()
  await expect(row.getByText("#3 ·")).toBeVisible({ timeout: 15_000 })
})

test("secrets are entered once and shown by name only", async ({ page }) => {
  test.slow() // four rollouts, each settling on a 5 s poll: right at the default 30 s
  const sentinel = "s3cr3t-sentinel-value"
  // The value is nowhere on the page: not as text and not in any input.
  const hidden = async () => {
    await expect(page.locator("body")).not.toContainText(sentinel)
    expect(await page.locator("input, textarea").evaluateAll((els) => els.map((e) => (e as HTMLInputElement).value))).not.toContain(sentinel)
  }
  await page.goto(path)
  await page.getByRole("button", { name: "New function", exact: true }).click()
  const form = page.getByRole("dialog", { name: "New function", exact: true })
  const create = form.getByRole("button", { name: "Create function", exact: true })
  await form.getByLabel("Name", { exact: true }).fill("vault")
  await form.getByLabel("Image", { exact: true }).fill("ghcr.io/acme/vault:1")
  await expect(form.getByLabel("Secret value", { exact: true })).toHaveAttribute("type", "password")
  await form.getByLabel("Secret name", { exact: true }).fill("PORT")
  await form.getByLabel("Secret value", { exact: true }).fill(sentinel)
  await expect(form.getByRole("button", { name: "Add secret", exact: true })).toBeDisabled()
  await form.getByLabel("Secret name", { exact: true }).fill("API_TOKEN")
  // Typed but not added: it would be left out of the function, so the function cannot be created yet.
  await expect(form.getByText("Not added yet")).toBeVisible()
  await expect(create).toBeDisabled()
  await form.getByRole("button", { name: "Add secret", exact: true }).click()
  await expect(create).toBeEnabled()
  await expect(form.getByRole("list", { name: "Secrets" }).getByText("API_TOKEN", { exact: true })).toBeVisible()
  await hidden()
  // A name cannot be both an environment variable and a secret.
  await form.getByLabel("Environment variables", { exact: true }).fill("API_TOKEN=plain")
  await expect(form.getByRole("alert")).toHaveText("API_TOKEN is both an environment variable and a secret")
  await expect(create).toBeDisabled()
  await form.getByLabel("Environment variables", { exact: true }).fill("MODE=prod")
  await create.click()
  await expect(form).not.toBeVisible()
  const row = page.getByRole("row").filter({ hasText: "vault-" })
  await expect(row.getByText("ready", { exact: true })).toBeVisible({ timeout: 15_000 })
  expect(await kept(page)).not.toContain(sentinel)

  // Deploy new version lists the secret by name, with an empty value field.
  await page.getByRole("button", { name: "Actions for vault", exact: true }).click()
  await page.getByRole("menuitem", { name: "Deploy new version", exact: true }).click()
  const deploy = page.getByRole("dialog", { name: "Deploy new version of vault", exact: true })
  await expect(deploy.getByRole("list", { name: "Secrets" }).getByText("API_TOKEN", { exact: true })).toBeVisible()
  await expect(deploy.getByLabel("Secret value", { exact: true })).toHaveValue("")
  await hidden()
  await deploy.getByRole("button", { name: "Cancel", exact: true }).click()

  // Details: replace one and add one in a single deployment.
  await page.getByRole("button", { name: "Actions for vault", exact: true }).click()
  await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
  const detail = page.getByRole("dialog", { name: "vault", exact: true })
  await detail.getByRole("tab", { name: "Secrets", exact: true }).click()
  const list = detail.getByRole("list", { name: "Secrets" })
  const save = detail.getByRole("button", { name: "Save and deploy", exact: true })
  await expect(list.getByText("API_TOKEN", { exact: true })).toBeVisible()
  await expect(detail.getByText("cannot be viewed again")).toBeVisible()
  await expect(save).toBeDisabled()
  await hidden()
  await detail.getByRole("button", { name: "Replace API_TOKEN", exact: true }).click()
  await expect(detail.getByLabel("Secret name", { exact: true })).toHaveValue("API_TOKEN")
  await expect(detail.getByLabel("Secret value", { exact: true })).toHaveValue("")
  await detail.getByLabel("Secret value", { exact: true }).fill(`${sentinel}-2`)
  await detail.getByRole("button", { name: "Replace secret", exact: true }).click()
  await expect(list.getByText("will be replaced", { exact: true })).toBeVisible()
  await detail.getByLabel("Secret name", { exact: true }).fill("DB_PASSWORD")
  await detail.getByLabel("Secret value", { exact: true }).fill(`${sentinel}-3`)
  await detail.getByRole("button", { name: "Add secret", exact: true }).click()
  await hidden()
  await save.click()
  await expect(list.getByText("DB_PASSWORD", { exact: true })).toBeVisible({ timeout: 15_000 })
  await hidden()
  expect(await kept(page)).not.toContain(sentinel)

  // Remove one: it is kept until saved, then gone.
  await detail.getByRole("button", { name: "Remove API_TOKEN", exact: true }).click()
  await expect(list.getByText("will be removed", { exact: true })).toBeVisible()
  await save.click()
  await expect(list.getByText("API_TOKEN", { exact: true })).toHaveCount(0, { timeout: 15_000 })

  // A rollback keeps the current secrets, not those of the earlier deployment.
  await detail.getByRole("tab", { name: "Deployments", exact: true }).click()
  await expect(detail.getByText("current secrets")).toBeVisible()
  await detail.getByRole("button", { name: "Roll back to deployment 1", exact: true }).click()
  await expect(detail.getByText("rollback to #1", { exact: true })).toBeVisible()
  await detail.getByRole("tab", { name: "Secrets", exact: true }).click()
  await expect(list.getByRole("listitem")).toHaveText([/^DB_PASSWORD/])
  await hidden()
  await detail.getByRole("button", { name: "Close", exact: true }).first().click()
  await expect(row.getByText("#4 ·")).toBeVisible({ timeout: 15_000 })
})

test("secrets typed for a function that goes away are not offered for the next one", async ({ page }) => {
  await page.goto(path)
  await page.getByRole("button", { name: "New function", exact: true }).click()
  const form = page.getByRole("dialog", { name: "New function", exact: true })
  await form.getByLabel("Name", { exact: true }).fill("other")
  await form.getByLabel("Image", { exact: true }).fill("ghcr.io/acme/other:1")
  await form.getByRole("button", { name: "Create function", exact: true }).click()
  await expect(page.getByRole("row").filter({ hasText: "other-" }).getByText("ready", { exact: true })).toBeVisible({ timeout: 15_000 })

  await page.getByRole("button", { name: "Actions for hello-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
  const detail = page.getByRole("dialog", { name: "hello-api", exact: true })
  await detail.getByRole("tab", { name: "Secrets", exact: true }).click()
  await detail.getByLabel("Secret name", { exact: true }).fill("STALE_TOKEN")
  await detail.getByLabel("Secret value", { exact: true }).fill("typed-for-hello-api")
  // Typed but not added: saving now would leave it out.
  await detail.getByRole("button", { name: "Remove DB_PASSWORD", exact: true }).click()
  await expect(detail.getByRole("button", { name: "Save and deploy", exact: true })).toBeDisabled()
  await detail.getByRole("button", { name: "Add secret", exact: true }).click()
  await expect(detail.getByRole("button", { name: "Save and deploy", exact: true })).toBeEnabled()

  // Someone else deletes the function (the mock API, called the way the app calls it): the
  // dialog goes with it at the next poll, without having been closed.
  const api = "/src/lib/api.ts"
  await page.evaluate(async (api) => (await import(api)).apiFetch("/project/prj-aurora/functions/fn-00000000000000a1", { method: "DELETE" }), api)
  await expect(detail).not.toBeVisible({ timeout: 15_000 })

  await page.getByRole("button", { name: "Actions for other", exact: true }).click()
  await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
  const next = page.getByRole("dialog", { name: "other", exact: true })
  await next.getByRole("tab", { name: "Secrets", exact: true }).click()
  await expect(next.getByLabel("Secret name", { exact: true })).toBeVisible()
  await expect(next.getByText("STALE_TOKEN")).toHaveCount(0)
  await expect(next.getByRole("button", { name: "Save and deploy", exact: true })).toBeDisabled()
})

test("private registry credentials are entered once and shown without the password", async ({ page }) => {
  test.slow() // five rollouts, each settling on a 5 s poll
  const sentinel = "r3g1stry-sentinel-pass"
  // The password is nowhere on the page: not as text and not in any input.
  const hidden = async () => {
    await expect(page.locator("body")).not.toContainText(sentinel)
    expect((await page.locator("input, textarea").evaluateAll((els) => els.map((e) => (e as HTMLInputElement).value))).join("\n")).not.toContain(sentinel)
  }
  await page.goto(path)
  await page.getByRole("button", { name: "New function", exact: true }).click()
  const form = page.getByRole("dialog", { name: "New function", exact: true })
  const create = form.getByRole("button", { name: "Create function", exact: true })
  await form.getByLabel("Name", { exact: true }).fill("priv")
  // The server defaults to the registry of the image; Docker Hub has its own address.
  await form.getByLabel("Image", { exact: true }).fill("acme/private-api:1")
  await expect(form.getByLabel("Registry server", { exact: true })).toHaveAttribute("placeholder", "https://index.docker.io/v1/")
  await form.getByLabel("Image", { exact: true }).fill("ghcr.io/acme/private-api:1")
  await expect(form.getByLabel("Registry server", { exact: true })).toHaveAttribute("placeholder", "ghcr.io")
  await expect(form.getByLabel("Registry password", { exact: true })).toHaveAttribute("type", "password")
  // Nothing verifies the credentials but a registry that is asked with them: the section says so.
  await expect(form.getByRole("group", { name: "Private registry", exact: true })).toContainText("The platform does not verify them, and a deployment that works does not prove them")
  await expect(create).toBeEnabled() // credentials are optional
  await form.getByLabel("Registry username", { exact: true }).fill("bot")
  await expect(form.getByRole("alert")).toContainText("Enter the username and the password")
  await expect(create).toBeDisabled()
  await form.getByLabel("Registry password", { exact: true }).fill(sentinel)
  await create.click()
  await expect(form).not.toBeVisible()
  const row = page.getByRole("row").filter({ hasText: "priv-" })
  await expect(row.getByText("ready", { exact: true })).toBeVisible({ timeout: 15_000 })
  await hidden()
  expect(await kept(page)).not.toContain(sentinel)

  // Deploy new version shows the server and the user, and no password field until Replace.
  const openDeploy = async () => {
    await page.getByRole("button", { name: "Actions for priv", exact: true }).click()
    await page.getByRole("menuitem", { name: "Deploy new version", exact: true }).click()
  }
  await openDeploy()
  const deploy = page.getByRole("dialog", { name: "Deploy new version of priv", exact: true })
  const go = deploy.getByRole("button", { name: "Deploy", exact: true })
  await expect(deploy.getByText("ghcr.io · bot", { exact: true })).toBeVisible()
  await expect(deploy.getByText("A rollback uses the current credentials")).toBeVisible()
  await expect(deploy.getByLabel("Registry password", { exact: true })).toHaveCount(0)
  await hidden()
  // Replace: the password is needed again, and a wrong one fails the rollout without being echoed.
  await deploy.getByRole("button", { name: "Replace registry credentials", exact: true }).click()
  await expect(deploy.getByLabel("Registry username", { exact: true })).toHaveValue("bot")
  await expect(deploy.getByLabel("Registry password", { exact: true })).toHaveValue("")
  await expect(go).toBeDisabled()
  await deploy.getByRole("button", { name: "Keep registry credentials", exact: true }).click()
  await expect(go).toBeEnabled()
  await deploy.getByRole("button", { name: "Replace registry credentials", exact: true }).click()
  await deploy.getByLabel("Registry username", { exact: true }).fill("bot2")
  await deploy.getByLabel("Registry password", { exact: true }).fill(`wrong-${sentinel}`)
  await go.click()
  await expect(deploy).not.toBeVisible()
  await expect(row.getByText("last deploy failed", { exact: true })).toBeVisible({ timeout: 15_000 })

  // The function keeps the credentials it had; the reason names no registry output.
  await page.getByRole("button", { name: "Actions for priv", exact: true }).click()
  await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
  const detail = page.getByRole("dialog", { name: "priv", exact: true })
  await expect(detail.getByRole("status")).toContainText("the registry refused the credentials or does not have the image")
  const shown = detail.locator("dd").filter({ hasText: "ghcr.io" }).last()
  await expect(shown).toContainText("· bot · password set")
  await detail.getByRole("tab", { name: "Deployments", exact: true }).click()
  await expect(detail.getByText("current secrets and registry credentials")).toBeVisible()
  await hidden()
  // No API response carries the password either.
  const api = "/src/lib/api.ts"
  const bodies = await page.evaluate(async (api) => {
    const { apiFetch } = await import(api)
    const list = await apiFetch("/project/prj-aurora/functions")
    const id = list.find((f: { name: string }) => f.name === "priv").id
    return JSON.stringify([list, await apiFetch(`/project/prj-aurora/functions/${id}`), await apiFetch(`/project/prj-aurora/functions/${id}/deployments`)])
  }, api)
  expect(bodies).toContain('"username":"bot2"') // the failed deployment records who, not the password
  expect(bodies).not.toContain(sentinel)
  expect(bodies).not.toContain('"password"') // no such key in any shape
  await detail.getByRole("button", { name: "Close", exact: true }).first().click()

  // An image in another registry is refused while the credentials are for this one...
  await openDeploy()
  await deploy.getByLabel("Image", { exact: true }).fill("quay.io/acme/api:2")
  await go.click()
  await expect(page.getByText("the registry credentials are for ghcr.io but the image is pulled from quay.io")).toBeVisible()
  // ...until they are replaced or removed in the same deploy. Replace does not carry the old
  // registry over: the server is left to the new image.
  await deploy.getByRole("button", { name: "Replace registry credentials", exact: true }).click()
  await expect(deploy.getByLabel("Registry server", { exact: true })).toHaveValue("")
  await expect(deploy.getByLabel("Registry server", { exact: true })).toHaveAttribute("placeholder", "quay.io")
  await expect(deploy.getByLabel("Registry username", { exact: true })).toHaveValue("bot")
  await deploy.getByRole("button", { name: "Keep registry credentials", exact: true }).click()
  await deploy.getByRole("button", { name: "Remove registry credentials", exact: true }).click()
  await expect(deploy.getByText("will be removed", { exact: true })).toBeVisible()
  await go.click()
  await expect(deploy).not.toBeVisible()
  await expect(row.getByText("#3 ·")).toBeVisible({ timeout: 15_000 })
  await openDeploy()
  await expect(deploy.getByLabel("Registry server", { exact: true })).toHaveAttribute("placeholder", "quay.io")
  await expect(deploy.getByLabel("Registry username", { exact: true })).toHaveValue("")
  await hidden()

  // Back to the private image, with credentials. Deployment #3 is in another registry than
  // they are for: it can be rolled back to only without them, and the button says so.
  await deploy.getByLabel("Image", { exact: true }).fill("ghcr.io/acme/private-api:1")
  await deploy.getByLabel("Registry username", { exact: true }).fill("bot")
  await deploy.getByLabel("Registry password", { exact: true }).fill(sentinel)
  await go.click()
  await expect(deploy).not.toBeVisible()
  await expect(row.getByText("#4 ·")).toBeVisible({ timeout: 15_000 })
  await page.getByRole("button", { name: "Actions for priv", exact: true }).click()
  await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
  await detail.getByRole("tab", { name: "Deployments", exact: true }).click()
  await expect(detail.getByRole("button", { name: "Roll back to deployment 1", exact: true })).toBeVisible()
  await expect(detail.getByText("In another registry than the stored credentials (ghcr.io): rolling back removes them.")).toBeVisible()
  await detail.getByRole("button", { name: "Roll back to deployment 3 without registry credentials", exact: true }).click()
  await expect(detail.getByRole("listitem").filter({ hasText: "rollback to #3" }).getByText("serving", { exact: true })).toBeVisible({ timeout: 15_000 })
  await expect(detail.getByText("In another registry than the stored credentials")).toHaveCount(0)
  await detail.getByRole("tab", { name: "Overview", exact: true }).click()
  await expect(detail.locator("dd").filter({ hasText: "ghcr.io" })).toHaveCount(0)
  await hidden()
})

test("suspends, resumes and deletes a function", async ({ page }) => {
  await page.goto(path)
  const row = page.getByRole("row").filter({ hasText: "hello-api" })
  await expect(row.getByText("idle", { exact: true })).toBeVisible()
  await page.getByRole("button", { name: "Actions for hello-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Suspend", exact: true }).click()
  await expect(row.getByText("suspended", { exact: true })).toBeVisible()
  await page.getByRole("button", { name: "Actions for hello-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Resume", exact: true }).click()
  await expect(row.getByText("idle", { exact: true })).toBeVisible()
  await page.getByRole("button", { name: "Actions for hello-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Delete", exact: true }).click()
  await page.getByRole("dialog", { name: "Delete function?", exact: true }).getByRole("button", { name: "Delete", exact: true }).click()
  await expect(page.getByText("No functions", { exact: true })).toBeVisible()
})

test("a failed deployment keeps the function on its current one", async ({ page }) => {
  await page.goto(path)
  await page.getByRole("button", { name: "Actions for hello-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Deploy new version", exact: true }).click()
  const deploy = page.getByRole("dialog")
  await deploy.getByLabel("Image", { exact: true }).fill("ghcr.io/acme/will-fail:9")
  await deploy.getByRole("button", { name: "Deploy", exact: true }).click()
  const row = page.getByRole("row").filter({ hasText: "hello-api" })
  await expect(row.getByText("last deploy failed", { exact: true })).toBeVisible({ timeout: 15_000 })
  await expect(row.getByText("#2 ·")).toBeVisible()
})

test("is hidden where functions are not enabled", async ({ page }) => {
  // The service of prj-staging carries no `functions` block: no menu entry, and the URL opens no page.
  await page.goto("/p/prj-staging/functions")
  await expect(page.getByText("Functions is not available in this project", { exact: true })).toBeVisible()
  await expect(page.getByRole("link", { name: "Sandboxes", exact: true })).toBeVisible()
  await expect(page.getByRole("link", { name: "Functions", exact: true })).toHaveCount(0)
  await expect(page.getByRole("button", { name: "New function", exact: true })).toHaveCount(0)
  // The page never mounted: it asked the API for nothing.
  expect((await page.evaluate(() => (globalThis as { __mockCalls?: string[] }).__mockCalls ?? [])).filter((c) => c.includes("/functions"))).toEqual([])
  await page.goto(path)
  await expect(page.getByRole("link", { name: "Functions", exact: true })).toBeVisible()
})


test("a new function runs one instance unless asked otherwise, and the maximum and the requests per instance are set and changed", async ({ page }) => {
  await page.goto(path)
  await page.getByRole("button", { name: "New function", exact: true }).click()
  const form = page.getByRole("dialog", { name: "New function", exact: true })
  const create = form.getByRole("button", { name: "Create function", exact: true })
  await form.getByLabel("Name", { exact: true }).fill("busy-api")
  await form.getByLabel("Image", { exact: true }).fill("ghcr.io/acme/busy:1")
  // One instance at most and 50 requests each unless asked otherwise; the location allows 5.
  await expect(form.getByLabel("Maximum instances", { exact: true })).toHaveValue("1")
  await expect(form.getByLabel("Requests per instance", { exact: true })).toHaveValue("50")
  await expect(form.getByText("anyone who can reach the URL can drive the function to its maximum", { exact: false })).toBeVisible()
  await form.getByLabel("Maximum instances", { exact: true }).fill("6")
  await expect(create).toBeDisabled()
  await form.getByLabel("Maximum instances", { exact: true }).fill("3")
  await form.getByLabel("Requests per instance", { exact: true }).fill("0")
  await expect(create).toBeDisabled()
  await form.getByLabel("Requests per instance", { exact: true }).fill("10")
  await expect(form.getByText("one instance for every 10 requests in flight", { exact: false })).toBeVisible()
  await create.click()
  await expect(form).not.toBeVisible()

  // The mock gives a function named busy load for every instance it may run.
  const row = page.getByRole("row").filter({ hasText: "busy-api-" })
  await expect(row.getByText("3 instances", { exact: true })).toBeVisible({ timeout: 15_000 })
  await page.getByRole("button", { name: "Actions for busy-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
  const detail = page.getByRole("dialog", { name: "busy-api", exact: true })
  await expect(detail.getByText("3 running · up to 3, one for every 10 requests in flight", { exact: true })).toBeVisible()
  await detail.getByRole("button", { name: "Close", exact: true }).first().click()

  // Changing the maximum deploys nothing: the deployment stays #1.
  await page.getByRole("button", { name: "Actions for busy-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Deploy new version", exact: true }).click()
  const deploy = page.getByRole("dialog", { name: "Deploy new version of busy-api", exact: true })
  await expect(deploy.getByLabel("Maximum instances", { exact: true })).toHaveValue("3")
  await expect(deploy.getByLabel("Requests per instance", { exact: true })).toHaveValue("10")
  await deploy.getByLabel("Maximum instances", { exact: true }).fill("2")
  await deploy.getByRole("button", { name: "Deploy", exact: true }).click()
  await expect(deploy).not.toBeVisible()
  await expect(row.getByText("2 instances", { exact: true })).toBeVisible()
  await page.getByRole("button", { name: "Actions for busy-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
  await expect(detail.getByText("2 running · up to 2, one for every 10 requests in flight", { exact: true })).toBeVisible()
  await expect(detail.getByText(/^#1, deployed/)).toBeVisible()

  // A function created without a maximum has one: the API's default too, not only the form's.
  const api = "/src/lib/api.ts"
  const made = await page.evaluate(async (api) => (await import(api)).apiFetch("/project/prj-aurora/functions", { method: "POST", body: { name: "busy-default", image: "ghcr.io/acme/busy:1" } }), api)
  expect(made.maxInstances).toBe(1)
})

test("a name that could be a sandbox host, and an image on an address or on localhost, are refused in the form", async ({ page }) => {
  await page.goto(path)
  await page.getByRole("button", { name: "New function", exact: true }).click()
  const form = page.getByRole("dialog", { name: "New function", exact: true })
  const create = form.getByRole("button", { name: "Create function", exact: true })
  const name = form.getByLabel("Name", { exact: true })
  const image = form.getByLabel("Image", { exact: true })
  await image.fill("ghcr.io/acme/echo:1")
  // A host that starts with a digit is a sandbox's (<port>-<id>): a function name never does.
  for (const bad of ["8080-api", "-api", "api-", "Api"]) {
    await name.fill(bad)
    await expect(create).toBeDisabled()
  }
  // <name>-<6 hex> must fit the 40 + 7 characters the host label has room for: the field takes no more.
  await expect(name).toHaveAttribute("maxlength", "40")
  await name.fill("api-2")
  await expect(create).toBeEnabled()
  // The platform's machines pull from the host the image names: only a DNS name is taken.
  for (const bad of ["10.0.0.5/acme/echo:1", "10.0.0.5:5000/acme/echo:1", "localhost/echo:1", "localhost:5000/echo:1", "[fd00::1]:5000/echo:1"]) {
    await image.fill(bad)
    await expect(form.getByText("The registry must be named by a DNS name", { exact: false })).toBeVisible()
    await expect(create).toBeDisabled()
  }
  for (const good of ["acme/echo:1", "registry.example.com:5000/acme/echo@sha256:" + "a".repeat(64)]) {
    await image.fill(good)
    await expect(create).toBeEnabled()
  }
})

test("the notes say who can call a function, who pays, what is billed, and what a tag, the certificate, a cookie and a secret mean", async ({ page }) => {
  await page.goto(path)
  const notes = page.getByRole("region", { name: "Before you rely on a function", exact: true })
  for (const [term, text] of [
    ["Who can call it", "There is no login in front of it"],
    ["Who can call it", "the six characters after the name are not a secret"],
    ["Callers cost you", "Whoever sends the requests, this project pays for the instances that run"],
    ["Callers cost you", "A new function runs one instance at most"],
    ["What is billed", "every running period is rounded up to whole minutes"],
    ["What is billed", "includes the idle wait before the function stops"],
    ["What is billed", "the larger of the two sizes while a deployment replaces one with the other"],
    ["What is billed", "is stopped within about two minutes and bills nothing"],
    ["Image tags", "a later instance can run another image than the first one"],
    ["Image tags", "A rollback always deploys the image that deployment ran, by its digest"],
    ["The certificate", "a client must trust the platform's certificate authority"],
    ["Cookies", "Every function of every project is served under sbx.example.com"],
    ["Cookies", "Never set a cookie with that parent as its Domain"],
    ["Secrets and logs", "A function can read its secrets, so it can print them"],
    ["Secrets and logs", "read-only members too"],
    ["Secrets and logs", "could read the secrets of the functions running there"],
  ]) {
    await expect(notes.locator("div").filter({ has: page.getByText(term, { exact: true }) }).last()).toContainText(text)
  }
  // The same, where a value is typed and where it is read.
  await page.getByRole("button", { name: "New function", exact: true }).click()
  const form = page.getByRole("dialog", { name: "New function", exact: true })
  await expect(form.getByText("A tag can move", { exact: false })).toBeVisible()
  await expect(form.getByText("can print them: keep them out of its output", { exact: false })).toBeVisible()
  await expect(form.getByText("Members who manage this project's resources can read these values", { exact: false })).toBeVisible()
  await form.getByRole("button", { name: "Cancel", exact: true }).click()
  await page.getByRole("button", { name: "Actions for hello-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Logs", exact: true }).click()
  const detail = page.getByRole("dialog", { name: "hello-api", exact: true })
  await expect(detail.getByText("Every member who can view this project's resources can read them: a function should never print its secrets.", { exact: false })).toBeVisible()
  await detail.getByRole("tab", { name: "Overview", exact: true }).click()
  await expect(detail.getByText("Internal, with no login in front of it: whoever reaches the address can call it.", { exact: true })).toBeVisible()
  await expect(detail.getByText("https://hello-api-3fa9c1.sbx.example.com", { exact: true }).first()).toBeVisible()
  await expect(detail.getByRole("button", { name: "Copy the URL", exact: true })).toBeVisible()
})

test("what this release does not offer is not on the page, and the API refuses it by name", async ({ page }) => {
  await page.goto(path)
  await page.getByRole("button", { name: "New function", exact: true }).click()
  const form = page.getByRole("dialog", { name: "New function", exact: true })
  await expect(form.getByLabel("Image", { exact: true })).toBeVisible()
  await expect(form.getByRole("tab")).toHaveCount(0) // no "From GitHub"
  for (const label of ["GPU", "Access", "Custom domain (optional)", "GitHub account"]) await expect(form.getByLabel(label, { exact: true })).toHaveCount(0)
  await form.getByRole("button", { name: "Cancel", exact: true }).click()
  await page.getByRole("button", { name: "Actions for hello-api", exact: true }).click()
  await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
  const detail = page.getByRole("dialog", { name: "hello-api", exact: true })
  await expect(detail.getByRole("tab")).toHaveText(["Overview", "Secrets", "Deployments", "Logs"])
  await expect(page.getByText("GitHub connections")).toHaveCount(0)

  const api = "/src/lib/api.ts"
  const refused = await page.evaluate(async (api) => {
    const { apiFetch } = await import(api)
    const base = { name: "later", image: "ghcr.io/acme/later:1" }
    const answers: string[] = []
    for (const extra of [{ exposure: "public" }, { gpu: { model: "any", count: 1 } }, { domains: ["shop.example.com"] }, { source: { type: "github", repo: "acme/later" } }]) {
      answers.push(await apiFetch("/project/prj-aurora/functions", { method: "POST", body: { ...base, ...extra } }).then(() => "created", (e: { status: number; message: string }) => `${e.status} ${e.message}`))
    }
    answers.push(await apiFetch("/project/prj-aurora/functions/fn-00000000000000a1", { method: "PATCH", body: { exposure: "public" } }).then(() => "updated", (e: { status: number; message: string }) => `${e.status} ${e.message}`))
    // Where functions are off, nothing is created or changed; reads and DELETE stay open for what is left.
    answers.push(await apiFetch("/project/prj-staging/functions", { method: "POST", body: base }).then(() => "created", (e: { status: number; message: string }) => `${e.status} ${e.message}`))
    answers.push(await apiFetch("/project/prj-staging/functions/fn-00000000000000a1", { method: "PATCH", body: { port: 9090 } }).then(() => "updated", (e: { status: number; message: string }) => `${e.status} ${e.message}`))
    answers.push(await apiFetch("/project/prj-staging/functions").then((l: unknown[]) => `listed ${l.length}`, (e: { status: number; message: string }) => `${e.status} ${e.message}`))
    answers.push(await apiFetch("/project/prj-staging/functions/fn-00000000000000a1", { method: "DELETE" }).then(() => "deleted", (e: { status: number; message: string }) => `${e.status} ${e.message}`))
    return answers
  }, api)
  expect(refused).toEqual([
    "400 public endpoints are not offered", "400 GPU functions are not offered", "400 custom hostnames are not offered", "400 builds from source are not offered",
    "400 public endpoints are not offered",
    "403 functions are not offered here", "403 functions are not offered here", "listed 0", "404 function not found",
  ])
})

test("the guide and the API reference resolve through the docs manifest", async ({ page }) => {
  for (const [slug, title] of [["guides/functions", "Functions"], ["api/functions", "Functions API"]]) {
    await page.goto(`/docs/${slug}`)
    await expect(page.getByRole("heading", { level: 1, name: title, exact: true })).toBeVisible()
    await expect(page.getByText("Page not found", { exact: true })).toHaveCount(0)
    await expect(page.getByRole("link", { name: title, exact: true })).toBeVisible()
  }
})

for (const theme of ["light", "dark"]) {
  test(`the page and its dialogs are accessible [${theme}]`, async ({ page }) => {
    await page.emulateMedia({ reducedMotion: "reduce" })
    await page.addInitScript((value) => localStorage.setItem("proxcloud.theme", value), theme)
    await page.goto(path)
    await expect(page.getByRole("row").filter({ hasText: "hello-api" })).toBeVisible()
    const check = async (scope?: string) => {
      const axe = new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
        // The documented brand exception of e2e/a11y.spec.ts: primary buttons only.
        .exclude('button[class*="bg-primary"]')
      if (scope) axe.include(scope)
      const results = await axe.analyze()
      expect(results.violations.map((v) => ({ id: v.id, impact: v.impact, nodes: v.nodes.slice(0, 5).map((n) => n.target.join(" ")) }))).toEqual([])
    }
    await check()
    await page.getByRole("button", { name: "New function", exact: true }).click()
    const form = page.getByRole("dialog", { name: "New function", exact: true })
    await form.getByLabel("Name", { exact: true }).fill("a11y")
    await form.getByLabel("Image", { exact: true }).fill("10.0.0.5/echo:1") // the error state of the image
    await form.getByLabel("Secret name", { exact: true }).fill("TOKEN") // the "not added yet" state
    await form.getByLabel("Registry username", { exact: true }).fill("bot") // the alert of the registry
    await expect(form.getByRole("alert")).toBeVisible()
    await check('[role="dialog"]')
    await form.getByRole("button", { name: "Cancel", exact: true }).click()
    await page.getByRole("button", { name: "Actions for hello-api", exact: true }).click()
    await page.getByRole("menuitem", { name: "Details and deployments", exact: true }).click()
    const detail = page.getByRole("dialog", { name: "hello-api", exact: true })
    for (const tab of ["Overview", "Secrets", "Deployments", "Logs"]) {
      await detail.getByRole("tab", { name: tab, exact: true }).click()
      if (tab === "Deployments") await expect(detail.getByRole("button", { name: "Roll back to deployment 1", exact: true })).toBeVisible()
      if (tab === "Logs") await expect(detail.getByLabel("Function logs")).toContainText("No log lines")
      await check('[role="dialog"]')
    }
    await detail.getByRole("button", { name: "Close", exact: true }).first().click()
    await page.getByRole("button", { name: "Actions for hello-api", exact: true }).click()
    await page.getByRole("menuitem", { name: "Delete", exact: true }).click()
    await expect(page.getByRole("dialog", { name: "Delete function?", exact: true })).toBeVisible()
    await check('[role="dialog"]')
  })
}
