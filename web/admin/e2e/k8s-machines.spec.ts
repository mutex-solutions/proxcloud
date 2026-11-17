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
    },
    {
      name: "pc-c-stuck-batch-pve2-klmno-3", nodeGroup: "batch", phase: "Provisioning", addresses: [], code: "no_capacity", attention: true,
      conditions: [{ type: "VirtualMachineProvisioned", status: "False", reason: "VMProvisionFailed", message: "cannot reserve 8589934592B of memory on node pve2: 1024B available memory left" }],
    },
  ],
}

async function open(page: import("@playwright/test").Page, machinesAnswer: unknown) {
  await page.goto("/system/cloud-providers")
  await expect(page.getByRole("button", { name: "Add provider" })).toBeVisible()
  await recordCalls(page, {
    "GET /admin/service/svc-k8s-01": svc,
    "GET /admin/service/svc-k8s-01/k8s-clusters": clusters,
    "GET /admin/service/svc-k8s-01/k8s-clusters/pc-c-stuck/machines": machinesAnswer,
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
  await expect(row("workers-pve1").getByRole("cell").last()).toHaveText("—")
  await expect(dialog.getByText("Proxmox access of this cluster")).toHaveCount(0)
  await expect(row("workers-pve2")).toContainText("not_joined")
  await expect(row("workers-pve2")).toContainText("142 @ pve2")
  await expect(row("workers-pve2")).toContainText("Waiting for a Node with spec.providerID proxmox://142 to exist")
  await expect(row("workers-pve2").getByText("Needs attention")).toBeVisible()
  await expect(row("batch-pve2")).toContainText("cannot reserve 8589934592B of memory on node pve2")
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
