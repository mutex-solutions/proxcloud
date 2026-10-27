// Route manifest for validation: every admin route from src/App.tsx, with
// mock-fixture params resolved (ids live in src/mocks/fixtures). Drives the
// screenshot matrix, the axe matrix, and the nothing-skipped tracking during
// the restyle.
export type RouteEntry = {
  /** Path with mock fixture ids inlined (see src/mocks/fixtures). */
  path: string
  name: string
  hasTable?: boolean
  hasChart?: boolean
  hasDialogs?: boolean
  /** Public route (no auth chrome). */
  public?: boolean
}

export const routes: RouteEntry[] = [
  { path: "/", name: "home", hasChart: true },
  { path: "/dashboard", name: "dashboard", hasChart: true },

  // Client area
  { path: "/clients/users", name: "users", hasTable: true, hasDialogs: true },
  { path: "/clients/users/usr-0001", name: "user-detail", hasTable: true, hasDialogs: true },
  { path: "/clients/organizations", name: "organizations", hasTable: true },
  { path: "/clients/organizations/org-0001", name: "organization-detail", hasTable: true },
  { path: "/clients/billing-profiles", name: "billing-profiles", hasTable: true },
  { path: "/clients/billing-profiles/bp-0001", name: "billing-profile-detail", hasTable: true, hasDialogs: true },
  { path: "/clients/projects", name: "projects", hasTable: true, hasDialogs: true },
  { path: "/clients/projects/prj-0001", name: "project-detail", hasTable: true, hasDialogs: true },
  { path: "/clients/projects/prj-0001?tab=quota", name: "project-detail-quota", hasTable: true },
  { path: "/clients/bills", name: "bills", hasTable: true },
  { path: "/clients/transactions", name: "transactions", hasTable: true, hasDialogs: true },
  { path: "/clients/account-credits", name: "account-credits", hasTable: true, hasDialogs: true },
  { path: "/clients/bank-transfers", name: "bank-transfers", hasTable: true, hasDialogs: true },
  { path: "/clients/validations", name: "validations", hasTable: true, hasDialogs: true },
  { path: "/clients/cloud-resources", name: "cloud-resources", hasTable: true },
  { path: "/clients/cloud-resources/cr-0001", name: "cloud-resource-detail" },

  // Billing setup
  { path: "/system/price-plans", name: "price-plans", hasTable: true, hasDialogs: true },
  { path: "/system/price-plans/pp-0001", name: "price-plan-detail", hasTable: true, hasDialogs: true },
  { path: "/system/taxes", name: "taxes", hasTable: true, hasDialogs: true },
  { path: "/system/savings-plans", name: "savings-plans", hasTable: true, hasDialogs: true },
  { path: "/system/promotions", name: "promotions", hasTable: true, hasDialogs: true },

  // Platform
  { path: "/system/cloud-providers", name: "cloud-providers", hasTable: true, hasDialogs: true },
  { path: "/system/cloud-providers/svc-openstack-01", name: "cloud-provider-detail", hasTable: true, hasDialogs: true },
  { path: "/system/catalog", name: "catalog", hasTable: true, hasDialogs: true },
  { path: "/system/templates", name: "templates", hasDialogs: true },
  { path: "/system/integrations", name: "integrations", hasDialogs: true },
  { path: "/system/configuration", name: "configuration", hasDialogs: true },
  { path: "/system/billing-configuration", name: "billing-configuration", hasDialogs: true },
  { path: "/system/menu", name: "menu", hasTable: true, hasDialogs: true },
  { path: "/system/roles", name: "roles", hasTable: true, hasDialogs: true },
  { path: "/system/hmac-keys", name: "hmac-keys", hasTable: true, hasDialogs: true },
  { path: "/audit", name: "audit", hasTable: true, hasDialogs: true },

  // Public docs
  { path: "/docs", name: "docs", public: true },
  { path: "/docs/platform-admin/billing/price-plans", name: "docs-article", public: true },
]

export const themes = ["light", "dark"] as const
export type Theme = (typeof themes)[number]
