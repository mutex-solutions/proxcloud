import { expect, test } from "@playwright/test"
import { calls, navigate, recordCalls } from "./mock-calls.ts"

// The operator's detail behind a tenant's "needs attention": the Machines dialog of a Kubernetes provider.
const svc = {
  id: "svc-k8s-01", name: "Managed Kubernetes AZ1", type: "CLOUD", status: "ACTIVE",
  config: {
    provider: "kamaji", region: "az1", regions: { az1: { name: "az1" } }, services: { kubernetes: { az1: true } },
    argocd: { namespace: "argocd", project: "proxcloud-k8s", chartRepo: "ghcr.io/mutex-solutions/charts", chartName: "proxmox-kamaji-cluster", chartVersion: "0.1.13" },
    cluster: { versions: { "1.35.10": "pve1=9001,pve2=9002" } },
  },
}
const clusters = { providerChartVersion: "0.1.13", clusters: [{ id: "pc-c-stuck", name: "staging", projectId: "prj-1", chartVersion: "0.1.13" }] }
const machines = {
  // A healthy v1beta2 object also carries conditions whose healthy state is False.
  proxmoxCluster: { conditions: [{ type: "ProxmoxAvailable", status: "True" }, { type: "Paused", status: "False", reason: "NotPaused" }] },
  machines: [
    {
      name: "pc-c-stuck-workers-pve1-abcde-1", nodeGroup: "workers", phase: "Running", nodeRef: "pc-c-stuck-workers-pve1-abcde-1",
      vmid: 141, proxmoxNode: "pve1", addresses: ["10.20.0.11"], attention: false,
      conditions: [
        { type: "NodeReady", status: "True" }, { type: "Paused", status: "False", reason: "NotPaused" },
        { type: "Deleting", status: "False", reason: "NotDeleting" }, { type: "Updating", status: "False", reason: "NotUpdating" },
      ],
    },
    {
      name: "pc-c-stuck-workers-pve2-fghij-2", nodeGroup: "workers", phase: "Provisioned", vmid: 142, proxmoxNode: "pve2",
      addresses: ["10.20.0.12"], code: "not_joined", attention: true,
      conditions: [{ type: "NodeHealthy", status: "Unknown", reason: "NodeDoesNotExist", message: "Waiting for a Node with spec.providerID proxmox://142 to exist" }],
      joinError: {
        errors: ["('scripts_user', RuntimeError('Runparts: 1 failures (runcmd) in 1 attempted'))"],
        logTail: "+ kubeadm join 192.168.140.101:6443 --token [MASKED] --discovery-token-ca-cert-hash [MASKED]\nerror execution phase preflight: couldn't validate the identity of the API Server: dial tcp 192.168.140.101:6443: i/o timeout",
      },
    },
    {
      name: "pc-c-stuck-batch-pve2-klmno-3", nodeGroup: "batch", phase: "Provisioning", addresses: [], code: "no_capacity", attention: true,
      conditions: [{ type: "VirtualMachineProvisioned", status: "False", reason: "VMProvisionFailed", message: "cannot reserve 8589934592B of memory on node pve2: 1024B available memory left" }],
    },
  ],
  // Two workers, one never joined: within the guard of 1, so Cluster API replaces it by itself. Two machines the host
  // had no room for: over the guard, nothing is replaced.
  healthChecks: [
    { nodeGroup: "workers", nodeStartupTimeoutSeconds: 1800, maxUnhealthy: 1, expectedMachines: 2, currentHealthy: 1, remediationsAllowed: 0 },
    { nodeGroup: "batch", nodeStartupTimeoutSeconds: 1800, maxUnhealthy: 1, expectedMachines: 2, currentHealthy: 0, remediationsAllowed: 0 },
  ],
}

async function open(page: import("@playwright/test").Page, machinesAnswer: unknown, more: Record<string, unknown> = {}) {
  await page.goto("/system/cloud-providers")
  await expect(page.getByRole("button", { name: "Add provider" })).toBeVisible()
  await recordCalls(page, {
    "GET /admin/service/svc-k8s-01": svc,
    "GET /admin/service/svc-k8s-01/k8s-clusters": clusters,
    "GET /admin/service/svc-k8s-01/k8s-clusters/pc-c-stuck/machines": machinesAnswer,
    ...more,
  })
  await navigate(page, "/system/cloud-providers/svc-k8s-01")
  await page.getByRole("button", { name: "Machines" }).click()
}

test("the Machines dialog shows each worker with the provider's own reason", async ({ page }) => {
  await open(page, machines)
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByText("Machines of staging")).toBeVisible()
  await expect.poll(() => calls(page, "GET", "/admin/service/svc-k8s-01/k8s-clusters/pc-c-stuck/machines")).toHaveLength(1)

  const row = (name: string) => dialog.getByRole("row").filter({ hasText: name })
  await expect(row("workers-pve1")).toContainText("ready")
  await expect(row("workers-pve1").getByText("Needs attention")).toHaveCount(0)
  // Paused=False, Deleting=False and Updating=False are the healthy state: no detail, and no provider warning.
  await expect(row("workers-pve1").getByRole("cell").nth(4)).toHaveText("—")
  await expect(dialog.getByText("Proxmox access of this cluster")).toHaveCount(0)
  await expect(row("workers-pve2")).toContainText("not_joined")
  await expect(row("workers-pve2")).toContainText("142 @ pve2")
  await expect(row("workers-pve2")).toContainText("Waiting for a Node with spec.providerID proxmox://142 to exist")
  await expect(row("workers-pve2").getByText("Needs attention")).toBeVisible()
  await expect(row("batch-pve2")).toContainText("cannot reserve 8589934592B of memory on node pve2")
})

// Production finding K1: two workers ran and never joined, and nothing told the operator why without a shell on the
// node. The dialog carries what cloud-init in the guest says, with the join token and the CA hash masked by the API.
test("a machine that did not join shows the guest's join error, and the repair state of each group", async ({ page }) => {
  await open(page, machines)
  const dialog = page.getByRole("dialog")
  const stuck = dialog.getByRole("row").filter({ hasText: "workers-pve2" })
  await expect(stuck).toContainText("Join error, as the machine reports it")
  await expect(stuck).toContainText("Runparts: 1 failures (runcmd)")
  await expect(stuck.getByLabel("End of the cloud-init log of pc-c-stuck-workers-pve2-fghij-2")).toContainText("dial tcp 192.168.140.101:6443: i/o timeout")
  await expect(dialog.getByRole("row").filter({ hasText: "workers-pve1" })).not.toContainText("Join error")

  const repair = dialog.getByText("Automatic repair").locator("..")
  await expect(repair.getByRole("listitem").filter({ hasText: "workers" })).toContainText("1 of 2 machines healthy")
  await expect(repair.getByRole("listitem").filter({ hasText: "workers" })).toContainText("after 30 min")
  await expect(repair.getByRole("listitem").filter({ hasText: "workers" })).not.toContainText("nothing is replaced")
  await expect(repair.getByRole("listitem").filter({ hasText: "batch" })).toContainText("so nothing is replaced: find the cause, then replace the machines here")
})

test("a cluster without a health check says that nothing is replaced by itself", async ({ page }) => {
  await open(page, { ...machines, healthChecks: [] })
  await expect(page.getByRole("dialog").getByText("Automatic repair").locator("..")).toContainText("No node group of this cluster has a health check")
})

test("one machine is replaced after a confirmation; the last Ready node of a group asks twice", async ({ page }) => {
  const replacePath = (name: string) => `/admin/service/svc-k8s-01/k8s-clusters/pc-c-stuck/machines/${name}/replace`
  await open(page, machines, {
    [`POST ${replacePath("pc-c-stuck-workers-pve2-fghij-2")}`]: { result: "REPLACING" },
    [`POST ${replacePath("pc-c-stuck-workers-pve1-abcde-1")}`]: { apiError: { status: 409, message: "This machine is the last Ready node of its node group", details: { reason: "last_ready_node" } } },
  })
  const dialog = page.getByRole("dialog")
  await dialog.getByRole("button", { name: "Replace pc-c-stuck-workers-pve2-fghij-2" }).click()
  const ask = dialog.getByRole("alertdialog")
  await expect(ask).toContainText("The node group's count and the project's quota do not change")
  await expect(ask).not.toContainText("last Ready node")
  // Nothing is sent before the confirmation.
  expect(await calls(page, "POST", replacePath("pc-c-stuck-workers-pve2-fghij-2"))).toHaveLength(0)
  await ask.getByRole("button", { name: "Replace machine" }).click()
  await expect(ask).toHaveCount(0)
  expect((await calls(page, "POST", replacePath("pc-c-stuck-workers-pve2-fghij-2"))).map((c) => c.body)).toEqual([{ confirm: false }])

  await dialog.getByRole("button", { name: "Replace pc-c-stuck-workers-pve1-abcde-1" }).click()
  await ask.getByRole("button", { name: "Replace machine" }).click()
  await expect(ask).toContainText("This is the last Ready node of its node group")
  await ask.getByRole("button", { name: "Replace anyway" }).click()
  expect((await calls(page, "POST", replacePath("pc-c-stuck-workers-pve1-abcde-1"))).map((c) => c.body)).toEqual([{ confirm: false }, { confirm: true }])
})

test("an install whose RBAC is older says which file to apply", async ({ page }) => {
  await open(page, { apiError: { status: 502, message: "The ProxCloud service account cannot read Machines: re-apply deploy/mgmt-cluster/rbac.yaml" } })
  await expect(page.getByRole("dialog").getByRole("alert")).toContainText("re-apply deploy/mgmt-cluster/rbac.yaml")
})

test("a paused cluster and a refused token show as the provider's finding", async ({ page }) => {
  await open(page, {
    proxmoxCluster: { conditions: [
      { type: "ProxmoxAvailable", status: "False", reason: "ProxmoxUnreachable", message: "401 authentication failure" },
      { type: "Paused", status: "True", reason: "Paused", message: "Cluster spec.paused is set to true" },
      { type: "Deleting", status: "False", reason: "NotDeleting" },
    ] },
    machines: [],
  })
  const box = page.getByRole("dialog").getByText("Proxmox access of this cluster").locator("..")
  await expect(box).toContainText("401 authentication failure")
  await expect(box).toContainText("Cluster spec.paused is set to true")
  await expect(box).not.toContainText("NotDeleting")
})

// The provider's repair setting (timeout, guard, off) reaches a cluster at its next change. A cluster already on the
// provider's chart has no bump to wait for: Apply settings writes it now, through the same route.
test("a cluster on the provider's chart can take the provider's settings without a bump", async ({ page }) => {
  await open(page, machines, { "POST /admin/service/svc-k8s-01/k8s-clusters/pc-c-stuck/bump-chart": { chartVersion: "0.1.13" } })
  await page.keyboard.press("Escape")
  await expect(page.getByRole("button", { name: "Bump to 0.1.13" })).toHaveCount(0)
  await page.getByRole("button", { name: "Apply the provider's settings to staging" }).click()
  await expect.poll(() => calls(page, "POST", "/admin/service/svc-k8s-01/k8s-clusters/pc-c-stuck/bump-chart")).toHaveLength(1)
})
