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
  await expect(workers).toContainText("most likely it could not reach the cluster's endpoint. Contact the operator, who can check the path from your network to the endpoint")
  await expect(workers).toContainText("A group of one cannot go to zero")
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

// Workers behind the Proxmox firewall: the cluster page names their security group and links to it.
test("a cluster whose workers are behind the firewall shows their security group", async ({ page }) => {
  await page.goto(`${base}/res-pc-c-healthy`)
  await expect(page.getByText("Worker firewall: inbound traffic to the nodes is dropped")).toBeVisible()
  await page.getByRole("button", { name: "k8s-pc-c-healthy" }).click()
  await expect(page).toHaveURL(/\/security-groups\//)
  // A cluster without the group says nothing about a firewall.
  await page.goto(`${base}/res-pc-c-stuck`)
  await expect(page.getByText("Needs attention — provisioning is not making progress")).toBeVisible()
  await expect(page.getByText("Worker firewall")).toHaveCount(0)
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
