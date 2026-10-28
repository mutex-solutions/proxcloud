// Docs sidebar manifest. Each page maps to src/docs/content/<slug>.md.
// Slugs mirror the public URL: /docs/<slug>. Titles are sentence case
// (design-system convention); proper nouns and initialisms keep their casing.
export type DocPage = { slug: string; title: string }
export type DocSection = { title: string; pages: DocPage[] }

export const docsTitle = "ProxCloud Admin Docs"

export const sections: DocSection[] = [
  {
    title: "Running the platform",
    pages: [
      { slug: "platform-admin/overview", title: "Overview" },
      { slug: "platform-admin/account-activation", title: "Account activation" },
    ],
  },
  {
    title: "Cloud providers",
    pages: [
      { slug: "platform-admin/cloud/connect-a-cloud", title: "Connecting Proxmox VE" },
      { slug: "platform-admin/cloud/service-availability", title: "Service availability" },
    ],
  },
  {
    title: "Billing configuration",
    pages: [
      { slug: "platform-admin/billing/price-plans", title: "Price plans" },
      { slug: "platform-admin/billing/resource-types", title: "Resource types" },
      { slug: "platform-admin/billing/currency", title: "Platform currency" },
      { slug: "platform-admin/billing/tax", title: "Tax rates" },
      { slug: "platform-admin/billing/invoicing", title: "Invoicing" },
      { slug: "platform-admin/billing/suspension", title: "Automatic suspension" },
      { slug: "platform-admin/billing/signup-credits", title: "Sign-up credits" },
      { slug: "platform-admin/billing/savings-plans", title: "Savings plans" },
    ],
  },
  {
    title: "Platform settings",
    pages: [
      { slug: "platform-admin/settings/flavor-categories", title: "Flavor categories" },
      { slug: "platform-admin/settings/custom-menu", title: "Custom menu items" },
      { slug: "platform-admin/settings/login-branding", title: "Login branding" },
    ],
  },
  {
    title: "Admin API reference",
    pages: [
      { slug: "reference/overview", title: "Overview" },
      { slug: "reference/authentication", title: "Authentication" },
      { slug: "reference/users", title: "Users" },
      { slug: "reference/organizations", title: "Organizations" },
      { slug: "reference/projects", title: "Projects" },
      { slug: "reference/billing-profiles", title: "Billing profiles" },
      { slug: "reference/bills", title: "Bills" },
      { slug: "reference/account-credits", title: "Account credits" },
      { slug: "reference/service-providers", title: "Service providers" },
      { slug: "reference/mcp-server", title: "MCP server" },
    ],
  },
]

export const defaultSlug = sections[0].pages[0].slug
