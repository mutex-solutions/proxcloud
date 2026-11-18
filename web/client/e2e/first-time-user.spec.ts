import { expect, test } from "@playwright/test"

// What was left of the production acceptance on v0.8.0 for a first-time user: invitations that could be neither seen
// nor taken back (D11), a key-only server nobody could reach, menu entries a site cannot serve, the security group
// form, hard reboot, and a network create that sat on "Creating…". Runs on the mock app. prj-aurora is a site with
// mail and a public network; prj-staging has neither, and no image for a load balancer.
const SITE = "/p/prj-aurora"
const BARE = "/p/prj-staging"

test("D11: a pending invitation is listed and can be revoked", async ({ page }) => {
  await page.goto(`${SITE}/org/members`)
  const pending = page.getByTestId("pending-invites")
  await expect(pending.getByRole("cell", { name: "pending@example.com", exact: true })).toBeVisible()
  // Mail is configured here: the mail carries the link, the page does not.
  await expect(page.getByTestId("no-mail-notice")).toHaveCount(0)
  await expect(pending.getByRole("button", { name: "Copy link" })).toHaveCount(0)

  await pending.getByRole("button", { name: "Revoke the invitation of pending@example.com" }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Revoke invitation" }).click()
  await expect(page.getByText("Invitation revoked")).toBeVisible()
  await expect(pending.getByText("No pending invitations.")).toBeVisible()

  // A new invitation shows up in the list.
  await page.getByRole("button", { name: "Invite by email" }).click()
  await page.getByRole("textbox", { name: "Email" }).fill("new@example.com")
  await page.getByRole("button", { name: "Send invite" }).click()
  await expect(page.getByText("Invitation sent to new@example.com")).toBeVisible()
  await expect(pending.getByRole("cell", { name: "new@example.com", exact: true })).toBeVisible()
})

test("D11: on a site without mail the invitation link is shown to copy, and the page says no mail is sent", async ({ page }) => {
  await page.goto(`${BARE}/org/members`)
  await expect(page.getByTestId("no-mail-notice")).toContainText("This site sends no email")
  await page.getByRole("button", { name: "Invite by email" }).click()
  await page.getByRole("textbox", { name: "Email" }).fill("teammate@example.com")
  await page.getByRole("button", { name: "Send invite" }).click()
  // Not "Invitation sent": nothing was sent.
  const link = page.getByTestId("invite-link")
  await expect(link).toContainText("no email was sent")
  await expect(link).toContainText("/join-project?invite-token=")
  await expect(page.getByText("Invitation sent to")).toHaveCount(0)
  await page.getByRole("dialog").getByRole("button", { name: "Done" }).click()
  const row = page.getByTestId("pending-invites").getByRole("row", { name: /teammate@example.com/ })
  await expect(row.getByRole("button", { name: "Copy link" })).toBeVisible()
})

test("UX 1: the create form says a key-only server cannot be reached where there is no public network", async ({ page }) => {
  await page.goto(`${BARE}/servers/new`)
  const note = page.getByTestId("no-public-network")
  await expect(note).toContainText("a key pair alone only lets you in from another server on the same network")
  await expect(note).toContainText("Set password")
  await page.getByRole("button", { name: "Password", exact: true }).click()
  await expect(note).toContainText("Log in with this password in Console")
  // UX 8: what applies when no group is ticked.
  await expect(page.getByTestId("default-group-hint")).toContainText("allows traffic only from this project's own networks")

  // A site with a public network says nothing.
  await page.goto(`${SITE}/servers/new`)
  await expect(page.getByTestId("default-group-hint")).toBeVisible()
  await expect(page.getByTestId("no-public-network")).toHaveCount(0)
})

test("UX 1: the server page says how to get in, by what the site offers", async ({ page }) => {
  await page.goto(`${BARE}/servers/res-server-001`)
  const access = page.getByTestId("server-access")
  await expect(access).toContainText("This site gives this project no public network")
  await expect(access).toContainText("More actions → Set password, then More actions → Console")

  await page.goto(`${SITE}/servers/res-server-001`)
  await expect(page.getByTestId("server-access")).not.toContainText("no public network")
})

test("UX 10: hard reboot is offered and asks first", async ({ page }) => {
  await page.goto(`${SITE}/servers/res-server-001`)
  await page.getByRole("button", { name: /more actions/i }).click()
  await page.getByRole("menuitem", { name: "Hard reboot" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toContainText("unsaved data in the guest is lost")
  await dialog.getByRole("button", { name: /confirm|hard reboot/i }).last().click()
  await expect(page.getByText("Hard reboot requested")).toBeVisible()
  const calls = await page.evaluate(() => (globalThis as { __mockCalls?: string[] }).__mockCalls ?? [])
  expect(calls.some((c) => c.startsWith("POST /project/prj-aurora/cloud/res-server-001/action"))).toBe(true)
})

test("UX 3: the load balancer page says what the operator must enable instead of failing at create", async ({ page }) => {
  await page.goto(`${BARE}/load-balancers`)
  const warning = page.getByTestId("lb-site-warning")
  await expect(warning).toContainText("No public network is available to this project")
  await expect(warning).toContainText("debian-13, is not in this site's image catalog")
  for (const button of await page.getByRole("button", { name: "Create load balancer" }).all()) {
    await expect(button).toBeDisabled()
  }

  await page.goto(`${SITE}/load-balancers`)
  await expect(page.getByRole("button", { name: "Create load balancer" }).first()).toBeEnabled()
  await expect(page.getByTestId("lb-site-warning")).toHaveCount(0)
})

test("UX 3: the file shares page warns before the create dialog is opened", async ({ page }) => {
  await page.goto(`${SITE}/file-shares`)
  await expect(page.getByTestId("share-site-warning")).toContainText("cannot reach the file server 192.0.2.100")
})

test("UX 5: the rule form has a protocol list, a description, says its direction, and edits a rule in one action", async ({ page }) => {
  await page.goto(`${SITE}/security-groups`)
  await page.getByRole("link", { name: "web-ingress" }).click()
  await expect(page.getByRole("cell", { name: "HTTPS", exact: true })).toBeVisible()

  await page.getByRole("button", { name: "Add rule" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByTestId("rule-direction")).toContainText("Ingress (inbound)")
  // Protocol is a list of what the API takes; one without ports closes the port fields.
  await dialog.getByRole("combobox", { name: "Protocol" }).click()
  for (const name of ["TCP", "UDP", "SCTP", "ICMP", "GRE", "ESP", "AH", "Any protocol"]) {
    await expect(page.getByRole("option", { name, exact: true })).toBeVisible()
  }
  await page.getByRole("option", { name: "ICMP", exact: true }).click()
  await expect(dialog.getByLabel("Port from")).toBeDisabled()
  await dialog.getByRole("combobox", { name: "Protocol" }).click()
  await page.getByRole("option", { name: "TCP", exact: true }).click()
  await dialog.getByLabel("Port from").fill("22")
  await dialog.getByLabel(/Source/).fill("203.0.113.0/24")
  await dialog.getByLabel("Description (optional)").fill("ssh, office")
  await dialog.getByRole("button", { name: "Add rule" }).click()
  await expect(page.getByText("Rule added")).toBeVisible()
  const row = page.getByRole("row", { name: /ssh, office/ })
  await expect(row).toContainText("203.0.113.0/24")

  // A cancelled attempt leaves nothing in the form.
  await page.getByRole("button", { name: "Add rule" }).click()
  await page.getByRole("dialog").getByLabel("Port from").fill("9999")
  await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click()
  await page.getByRole("button", { name: "Add rule" }).click()
  await expect(page.getByRole("dialog").getByLabel("Port from")).toHaveValue("")
  await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click()

  // Edit: the form opens on the rule, and one save replaces it.
  await row.getByRole("button", { name: "Edit rule" }).click()
  const edit = page.getByRole("dialog")
  await expect(edit.getByLabel("Port from")).toHaveValue("22")
  await expect(edit.getByLabel("Description (optional)")).toHaveValue("ssh, office")
  await edit.getByLabel("Port from").fill("2222")
  await edit.getByRole("button", { name: "Save rule" }).click()
  await expect(page.getByText("Rule changed")).toBeVisible()
  await expect(page.getByRole("row", { name: /ssh, office/ })).toContainText("2222")
  await expect(page.getByRole("row", { name: /ssh, office/ })).toHaveCount(1)
})

test("UX 9: a network create says what it is waiting for", async ({ page }) => {
  // The create waits for the network to be applied on the nodes, as on a real site.
  await page.addInitScript(() => {
    ;(globalThis as { __mockLatency?: (call: string) => number | undefined }).__mockLatency = (call) =>
      call === "POST /project/prj-aurora/cloud" ? 2500 : undefined
  })
  await page.goto(`${SITE}/networks`)
  await page.getByRole("button", { name: /create network/i }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Name").fill("t-net")
  await dialog.getByRole("button", { name: "Create network" }).click()
  await expect(dialog.getByTestId("network-create-progress")).toContainText("applying it on every node")
  await expect(page.getByText('Network "t-net" created')).toBeVisible()
})
