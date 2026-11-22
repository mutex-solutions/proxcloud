import { expect, test } from "@playwright/test"

// TLS on a load balancer (v0.11.1): a project's certificates, the picker on a TLS or HTTPS listener, and the
// warnings before a certificate expires. Runs on the mock app: prj-aurora holds shop-2027 (months left) and api-2026
// (twelve days left).
const SITE = "/p/prj-aurora"

test("the certificates page lists what each certificate says, warns before one expires, and takes an upload", async ({ page }) => {
  await page.goto(`${SITE}/certificates`)
  const shop = page.getByRole("row", { name: /shop-2027/ })
  await expect(shop).toContainText("shop.example.org, www.shop.example.org")
  await expect(shop).toContainText("ECDSA P-256")
  await expect(shop).toContainText("No listener")
  await expect(shop).not.toContainText("Expires in")
  await expect(page.getByRole("row", { name: /api-2026/ })).toContainText("Expires in 12 days")
  await expect(page.getByTestId("cert-replace-note")).toContainText("upload the new one")

  await page.getByRole("button", { name: "Upload certificate" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toContainText("never shown again")
  const upload = dialog.getByRole("button", { name: "Upload", exact: true })
  await expect(upload).toBeDisabled()
  await dialog.getByLabel("Name").fill("new-site")
  await dialog.getByLabel("Certificate chain", { exact: true }).fill("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
  // What the API refuses is said as the API says it, and the dialog stays for the correction.
  await dialog.getByLabel("Private key", { exact: true }).fill("not a key")
  await upload.click()
  await expect(page.getByText("The private key is not PEM text with one key in it")).toBeVisible()
  await dialog.getByLabel("Private key", { exact: true }).fill("-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n")
  await upload.click()
  await expect(page.getByText("Certificate new-site uploaded")).toBeVisible()
  const made = page.getByRole("row", { name: /new-site/ })
  await expect(made).toContainText("new.example.org")
  // The key is on the page nowhere once the dialog closed.
  await expect(page.getByText("BEGIN PRIVATE KEY")).toHaveCount(0)

  await made.getByRole("button", { name: "Delete certificate new-site" }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Delete" }).click()
  await expect(page.getByText("Certificate deleted")).toBeVisible()
  await expect(page.getByRole("row", { name: /new-site/ })).toHaveCount(0)
})

test("an HTTPS listener takes certificates in order, and one in use is not deleted", async ({ page }) => {
  await page.goto(`${SITE}/load-balancers`)
  await page.getByRole("link", { name: "web-lb" }).click()
  const pick = async (box: string, option: RegExp | string) => {
    await page.getByRole("combobox", { name: box }).click()
    await page.getByRole("option", { name: option, exact: typeof option === "string" }).click()
  }
  await expect(page.getByTestId("listener-certificates-0")).toHaveCount(0) // an HTTP listener has none
  await pick("Listener protocol", "HTTPS")
  const certs = page.getByTestId("listener-certificates-0")
  await expect(certs).toContainText("An HTTPS listener needs a certificate")
  await expect(certs).toContainText("X-Forwarded-Proto")
  await expect(page.getByTestId("pool-protocol-0")).toContainText("spoken to in HTTP") // HTTP behind HTTPS
  await pick("Add a certificate", /api-2026/)
  await expect(certs).toContainText("Expires in 12 days") // the warning sits where the certificate is used
  await pick("Add a certificate", /shop-2027/)
  await expect(certs.getByText("default")).toHaveCount(1)
  // The first certificate is the default: take it off, and the next one is.
  await certs.getByRole("button", { name: "Take certificate api-2026 off the listener" }).click()
  await expect(certs).not.toContainText("api-2026")
  await expect(certs).toContainText("default")
  await page.getByRole("spinbutton", { name: "Listener port" }).fill("443")
  await page.getByRole("button", { name: "Save configuration" }).click()
  await expect(page.getByText("Configuration saved")).toBeVisible()
  const sent = await page.evaluate(() => (globalThis as { __lbConfig?: { listeners: object[]; pools: Array<Record<string, unknown>> } }).__lbConfig)
  expect(sent?.listeners[0]).toEqual({ port: 443, protocol: "https", pool: "web", sourceRanges: ["198.51.100.0/24"], certificates: ["c0ffee00000000000000000000000001"] })
  expect(sent?.pools[0].protocol).toBe("http")

  // On the Certificates page (reached in the app: the mock keeps what was saved) it reads as used, and stays.
  await page.getByRole("link", { name: "Certificates" }).click()
  const shop = page.getByRole("row", { name: /shop-2027/ })
  await expect(shop).toContainText("web-lb")
  await shop.getByRole("button", { name: "Delete certificate shop-2027" }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Delete" }).click()
  await expect(page.getByText("This certificate is used by a listener of load balancer web-lb")).toBeVisible()
  await expect(page.getByRole("row", { name: /shop-2027/ })).toBeVisible()

  // Back to plain HTTP: the certificates leave the request with the protocol.
  await page.getByRole("link", { name: "Load balancers" }).click()
  await page.getByRole("link", { name: "web-lb" }).click()
  await expect(page.getByRole("combobox", { name: "Listener protocol" })).toContainText("HTTPS")
  await pick("Listener protocol", "HTTP")
  await page.getByRole("button", { name: "Save configuration" }).click()
  const lastSent = () => page.evaluate(() => (globalThis as { __lbConfig?: { listeners: Array<Record<string, unknown>> } }).__lbConfig?.listeners[0])
  await expect.poll(async () => (await lastSent())?.protocol).toBe("http") // the toast of the save before may still show
  expect((await lastSent())?.certificates).toBeUndefined()
})

test("a TLS listener speaks plain TCP to its pool", async ({ page }) => {
  await page.goto(`${SITE}/load-balancers`)
  await page.getByRole("link", { name: "web-lb" }).click()
  await page.getByRole("combobox", { name: "Listener protocol" }).click()
  await page.getByRole("option", { name: "TLS", exact: true }).click()
  await expect(page.getByTestId("pool-protocol-0")).toContainText("spoken to in TCP")
  await expect(page.getByTestId("listener-certificates-0")).toContainText("plain TCP")
})
