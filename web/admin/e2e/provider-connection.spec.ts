import { expect, test } from "@playwright/test"
import { calls, recordCalls } from "./mock-calls.ts"

test("provider Connection Save sends the failover URLs and the cluster CA", async ({ page }) => {
  await page.goto("/system/cloud-providers/svc-proxmox-01")
  await expect(page.getByLabel("API URL", { exact: true })).toBeVisible()
  await recordCalls(page)

  await page.getByLabel(/Other API URLs for failover/).fill("https://pve2.example:8006\nhttps://pve3.example:8006/")
  await page.getByLabel(/Cluster CA/).fill("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----")
  await page.getByRole("button", { name: "Save", exact: true }).click()

  await expect.poll(() => calls(page, "PUT", "/admin/service/svc-proxmox-01")).toHaveLength(1)
  const [put] = await calls(page, "PUT", "/admin/service/svc-proxmox-01")
  expect(put.body.config.apiUrls).toEqual(["https://pve2.example:8006", "https://pve3.example:8006"])
  expect(put.body.config.caCert).toBe("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----")
  // The rest of the stored network block rides along.
  expect(put.body.config.network.publicNetworks).toHaveLength(1)
})
