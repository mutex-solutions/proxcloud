import { expect, test } from "@playwright/test"

// The Workers column of a cluster's Services on the mock app. prj-aurora's prod has a Service whose health check is
// turned off (proxcloud.io/lb-enable-health-monitor: "false"): HAProxy reports each of its workers as "no check"
// and sends to every one, so the row counts none of them as up or as down.
const SITE = "/p/prj-aurora"

test("a Service with its health check off reads its workers as not checked, on the cluster's page and the balancer's", async ({ page }) => {
  await page.goto(`${SITE}/kubernetes/res-pc-c-healthy`)
  for (const id of ["k8s-lb", "lb-cluster"]) {
    const row = (service: string) => page.getByTestId(id).getByRole("row").filter({ hasText: service })
    await expect(row("infra/syslog")).toContainText("6514/TCP")
    await expect(row("infra/syslog")).toContainText("3, not checked")
    await expect(row("infra/syslog")).not.toContainText("0 of")
    await expect(row("infra/syslog")).not.toContainText("up")
    // A worker with a checked member counts by it; the one that has only the unchecked port is named apart.
    await expect(row("infra/relay")).toContainText("2 of 2 up, 1 not checked")
    // Control: the checked Services count as before, a worker that is down included.
    await expect(row("shop/web")).toContainText("3 of 3 up")
    await expect(row("shop/web")).not.toContainText("not checked")
    await expect(row("infra/dns")).toContainText("2 of 3 up")
    await expect(row("nginx-gateway/public-gateway-nginx")).toContainText("1 of 3 up")
    if (id === "k8s-lb") await page.getByTestId(id).getByRole("button", { name: "Open the load balancer" }).click()
  }
})
