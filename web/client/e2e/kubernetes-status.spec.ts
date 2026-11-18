import { expect, test } from "@playwright/test"

// The provisioning status a tenant sees when a cluster does not come up (mock seeds in
// src/mocks/fixtures/cloud.ts): the reason per node group, and "Needs attention".
const base = "/p/prj-aurora/kubernetes"

test("the list marks the clusters that need attention, and only those", async ({ page }) => {
  await page.goto(base)
  const row = (name: string) => page.getByRole("row").filter({ hasText: name })
  await expect(row("staging").getByText("Needs attention")).toBeVisible()
  await expect(row("sandbox").getByText("Needs attention")).toBeVisible()
  await expect(row("prod")).toBeVisible()
  await expect(row("prod").getByText("Needs attention")).toHaveCount(0)
})

test("a stuck cluster says why per node group and what the tenant can do", async ({ page }) => {
  await page.goto(`${base}/res-pc-c-stuck`)
  await expect(page.getByText("Needs attention — provisioning is not making progress")).toBeVisible()
  // The page polls: the change to "Needs attention" is announced, and a step's state is not carried by its icon alone.
  await expect(page.getByRole("status").filter({ hasText: "Needs attention — provisioning is not making progress" })).toBeVisible()
  await expect(page.getByText(/delete and recreate the cluster, or contact the operator/).first()).toBeVisible()

  // The worker that was built and never joined: the lab case of 2026-11-17.
  const workers = page.getByRole("listitem").filter({ hasText: "Node group “workers”" })
  await expect(workers).toContainText("1/2 nodes ready")
  await expect(workers).toContainText("Needs attention: Node group “workers”")
  await expect(workers).toContainText("The machine runs but has not joined the cluster")
  // Past its bound the tenant is sent to the operator, not to a network check they cannot make.
  await expect(workers).toContainText("most likely it could not reach the cluster's endpoint. Contact the operator, who can read the join error of the machine and check the path from your network to the endpoint")
  await expect(workers).toContainText("use Replace on that machine, next to this node group's status in the table of node groups or in the list of instances below")
  await expect(workers).not.toContainText("reduce the count by one")
  await expect(workers).not.toContainText("has a router or gateway")
  await expect(workers).toContainText(/since 4\d min/)
  // Two machines the host had no room for.
  const batch = page.getByRole("listitem").filter({ hasText: "Node group “batch”" })
  await expect(batch).toContainText("2 machines: The host has no free capacity for this machine")
  // A group inside its bound is progress, not attention.
  const build = page.getByRole("listitem").filter({ hasText: "Node group “build”" })
  await expect(build).toContainText("Creating the virtual machine.")
  await expect(build).not.toContainText("Needs attention:")
  // Not joined inside its bound is a normal step: no diagnosis and nothing to do.
  const edge = page.getByRole("listitem").filter({ hasText: "Node group “edge”" })
  await expect(edge).toContainText("The machine runs but has not joined the cluster yet.")
  await expect(edge).not.toContainText("Contact the operator")

  const table = page.getByRole("table").filter({ hasText: "Node group" }).first()
  await expect(table.getByRole("columnheader", { name: "Status" })).toBeVisible()
  await expect(table.getByRole("row").filter({ hasText: "workers" }).getByText("Needs attention")).toBeVisible()
  await expect(table.getByRole("row").filter({ hasText: "build" }).getByText("Needs attention")).toHaveCount(0)
})

// Production (2026-11-18): the two workers that never joined were never among the project's servers, so the instance
// list had no row to press Replace on. The node group's own line names the machines that need attention.
test("a machine that never joined is replaced from its node group's line when the instance list is empty", async ({ page }) => {
  await page.goto(base)
  await expect(page.getByRole("row").filter({ hasText: "staging" })).toBeVisible()
  await page.evaluate(async () => {
    const apiPath = "/src/lib/api.ts", routerPath = "/src/mocks/router.ts"
    const { setMockHandler } = await import(/* @vite-ignore */ apiPath)
    const { dispatch } = await import(/* @vite-ignore */ routerPath)
    const w = window as unknown as { __actions: unknown[] }
    w.__actions = []
    setMockHandler(async (path: string, opts: { method?: string; body?: { action?: string } }) => {
      if (path.endsWith("/action") && opts.body?.action === "REPLACE_MACHINE") w.__actions.push(opts.body)
      return dispatch(path, opts)
    })
  })
  await page.getByRole("row").filter({ hasText: "staging" }).click()
  await expect(page.getByText("No worker instances yet.")).toBeVisible()
  const groups = page.getByRole("table").filter({ hasText: "Node group" }).first()
  const row = groups.getByRole("row").filter({ hasText: "workers" })
  await expect(row).toContainText("pc-c-stuck-workers-pve2-fghij-stuck")
  // Only a group with a machine past its bound names one: nothing to replace on a group that is still on its way.
  await expect(groups.getByRole("row").filter({ hasText: "edge" }).getByRole("button")).toHaveCount(0)
  await row.getByRole("button", { name: "Replace machine pc-c-stuck-workers-pve2-fghij-stuck" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toContainText("pc-c-stuck-workers-pve2-fghij-stuck")
  await dialog.getByRole("button", { name: "Replace", exact: true }).click()
  await expect(dialog).toHaveCount(0)
  expect(await page.evaluate(() => (window as unknown as { __actions: never[] }).__actions)).toEqual([
    { action: "REPLACE_MACHINE", data: { machine: "pc-c-stuck-workers-pve2-fghij-stuck", confirm: false } },
  ])
})

// Production finding R1: two workers that never joined took two scale-up and scale-down cycles to replace, because a
// group cannot go to zero. One action per machine now.
test("a node is replaced from the instance list, and the last Ready one asks first", async ({ page }) => {
  // main.tsx installs the mock at start: the recorder goes in after the first page has rendered, or it is overwritten.
  await page.goto(base)
  await expect(page.getByRole("row").filter({ hasText: "staging" })).toBeVisible()
  const posts: { action?: string; data?: Record<string, unknown> }[] = []
  await page.evaluate(async () => {
    const apiPath = "/src/lib/api.ts", routerPath = "/src/mocks/router.ts"
    const { setMockHandler } = await import(/* @vite-ignore */ apiPath)
    const { dispatch } = await import(/* @vite-ignore */ routerPath)
    const w = window as unknown as { __actions: unknown[] }
    w.__actions = []
    // The two workers of the group "workers", as the project's servers: one joined, one runs and never joined.
    const worker = (name: string, addr: string) => ({
      id: `res-${name}`, type: "SERVER", name, status: "ACTIVE", externalId: `vm-${name}`,
      data: { server: { id: `vm-${name}`, name, status: "ACTIVE", addresses: { workers: [{ addr, version: 4 }] } } },
    })
    setMockHandler(async (path: string, opts: { method?: string; body?: { action?: string } }) => {
      if (path.endsWith("/action") && opts.body?.action === "REPLACE_MACHINE") w.__actions.push(opts.body)
      const res = await dispatch(path, opts)
      if (path.includes("/resource?type=SERVER")) {
        return { ...res, data: [...(res.data as unknown[]), worker("pc-c-stuck-workers-pve1-abcde-ready", "10.20.0.11"), worker("pc-c-stuck-workers-pve2-fghij-stuck", "10.20.0.12")] }
      }
      return res
    })
  })
  await page.getByRole("row").filter({ hasText: "staging" }).click()
  const sent = async () => {
    posts.splice(0, posts.length, ...(await page.evaluate(() => (window as unknown as { __actions: never[] }).__actions)))
    return posts
  }

  // The machine that never joined: one confirmation, no warning, and the node count is not part of the request.
  await page.getByRole("button", { name: "Replace pc-c-stuck-workers-pve2-fghij-stuck" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toContainText("The node count and your quota stay as they are")
  await expect(dialog.getByRole("alert")).toHaveCount(0)
  await dialog.getByRole("button", { name: "Replace", exact: true }).click()
  await expect(dialog).toHaveCount(0)
  expect(await sent()).toEqual([{ action: "REPLACE_MACHINE", data: { machine: "pc-c-stuck-workers-pve2-fghij-stuck", confirm: false } }])

  // The only Ready node of the group: the server refuses, the dialog says why and asks again.
  await page.getByRole("button", { name: "Replace pc-c-stuck-workers-pve1-abcde-ready" }).click()
  await dialog.getByRole("button", { name: "Replace", exact: true }).click()
  await expect(dialog.getByRole("alert")).toContainText("the last Ready node of its node group")
  await dialog.getByRole("button", { name: "Replace anyway" }).click()
  await expect(dialog).toHaveCount(0)
  expect((await sent()).at(-1)).toEqual({ action: "REPLACE_MACHINE", data: { machine: "pc-c-stuck-workers-pve1-abcde-ready", confirm: true } })
  // The row click still opens the server; the button did not.
  await expect(page).toHaveURL(new RegExp(`${base}/res-pc-c-stuck$`))
})

test("a control plane that never came up needs attention; its workers only wait", async ({ page }) => {
  await page.goto(`${base}/res-pc-c-nocp`)
  const cp = page.getByRole("listitem").filter({ hasText: "Control plane" }).first()
  await expect(cp).toContainText("The control plane is taking longer than expected")
  const workers = page.getByRole("listitem").filter({ hasText: "Node group “workers”" })
  await expect(workers).toContainText("Waiting for the control plane.")
})

test("a healthy cluster shows no reason and no attention", async ({ page }) => {
  await page.goto(`${base}/res-pc-c-healthy`)
  await expect(page.getByRole("columnheader", { name: "Status" }).first()).toBeVisible()
  await expect(page.getByText("Needs attention")).toHaveCount(0)
  await expect(page.getByText("Working on it")).toHaveCount(0)
})

// Placement: a group scaled below the number of hosts it runs on empties a host.
test("the node group dialog warns when a scale-down empties a host", async ({ page }) => {
  await page.goto(`${base}/res-pc-c-healthy`)
  await page.getByRole("button", { name: "Edit node groups" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByRole("alert")).toHaveCount(0)
  await dialog.getByLabel("Nodes").fill("2")
  await expect(dialog.getByRole("alert")).toContainText("Node group “workers” runs on 3 hosts. With 2 nodes, every worker on at least one host is removed.")
  // Scaling up, or staying at one worker per host, warns about nothing.
  await dialog.getByLabel("Nodes").fill("5")
  await expect(dialog.getByRole("alert")).toHaveCount(0)
})
