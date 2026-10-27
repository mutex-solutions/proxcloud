// Route manifest for validation: every client route, with mock-fixture params
// resolved. Drives the screenshot matrix, the axe matrix, and the
// nothing-skipped tracking during the restyle.
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

const P = "/p/prj-aurora"

export const routes: RouteEntry[] = [
  { path: "/", name: "home" },
  { path: `${P}/dashboard`, name: "dashboard", hasChart: true },

  // Compute
  { path: `${P}/servers`, name: "servers", hasTable: true, hasDialogs: true },
  { path: `${P}/servers/new`, name: "servers-new" },
  { path: `${P}/servers/res-server-001`, name: "server-detail", hasTable: true, hasDialogs: true },
  { path: `${P}/server-groups`, name: "server-groups", hasTable: true, hasDialogs: true },
  { path: `${P}/keypairs`, name: "keypairs", hasTable: true, hasDialogs: true },
  { path: `${P}/images`, name: "images", hasTable: true, hasDialogs: true },

  // Storage
  { path: `${P}/volumes`, name: "volumes", hasTable: true, hasDialogs: true },
  { path: `${P}/snapshots`, name: "snapshots", hasTable: true, hasDialogs: true },
  { path: `${P}/object-storage`, name: "buckets", hasTable: true, hasDialogs: true },
  { path: `${P}/object-storage/res-bucket-001`, name: "bucket-explore", hasTable: true, hasDialogs: true },
  { path: `${P}/s3-keys`, name: "s3-keys", hasTable: true, hasDialogs: true },
  { path: `${P}/shares`, name: "shares", hasTable: true, hasDialogs: true },

  // Network
  { path: `${P}/networks`, name: "networks", hasTable: true, hasDialogs: true },
  { path: `${P}/networks/res-network-001`, name: "network-detail", hasTable: true, hasDialogs: true },
  { path: `${P}/routers`, name: "routers", hasTable: true, hasDialogs: true },
  { path: `${P}/ports`, name: "ports", hasTable: true, hasDialogs: true },
  { path: `${P}/floating-ips`, name: "floating-ips", hasTable: true, hasDialogs: true },
  { path: `${P}/security-groups`, name: "security-groups", hasTable: true, hasDialogs: true },
  { path: `${P}/security-groups/res-security-group-001`, name: "security-group-detail", hasTable: true, hasDialogs: true },
  { path: `${P}/load-balancers`, name: "load-balancers", hasTable: true, hasDialogs: true },
  { path: `${P}/dns`, name: "dns-zones", hasTable: true, hasDialogs: true },
  { path: `${P}/dns/res-dns-zone-001`, name: "dns-zone-detail", hasTable: true, hasDialogs: true },

  // Platform
  { path: `${P}/stacks`, name: "stacks", hasTable: true, hasDialogs: true },
  { path: `${P}/secrets`, name: "secrets", hasTable: true, hasDialogs: true },

  // Billing
  { path: `${P}/billing/savings`, name: "billing-savings", hasTable: true, hasDialogs: true },
  { path: `${P}/billing/credits`, name: "billing-credits", hasTable: true, hasDialogs: true },
  { path: `${P}/billing/funds`, name: "billing-funds", hasDialogs: true },
  { path: `${P}/billing/cards`, name: "billing-cards", hasTable: true, hasDialogs: true },
  { path: `${P}/billing/history`, name: "billing-history", hasTable: true },
  { path: `${P}/billing/history/bills/bill-2026-06`, name: "bill-detail", hasTable: true },

  // Custom menu
  { path: `${P}/more/status-page`, name: "more-custom" },

  // Organization
  { path: `${P}/org/billing`, name: "org-billing", hasChart: true, hasTable: true },
  { path: `${P}/org/members`, name: "org-members", hasTable: true, hasDialogs: true },
  { path: `${P}/org/projects`, name: "org-projects", hasTable: true, hasDialogs: true },
  { path: `${P}/org/audit`, name: "org-audit", hasTable: true },
  { path: `${P}/org/roles`, name: "org-roles", hasTable: true, hasDialogs: true },
  { path: `${P}/org/settings`, name: "org-settings", hasDialogs: true },
  { path: `${P}/account`, name: "account", hasTable: true },

  // Join flow + public docs
  { path: "/join-project", name: "join-project" },
  { path: "/docs", name: "docs", public: true },
  // Slug must match src/docs/manifest.ts — a bad slug renders the 404 state.
  { path: "/docs/getting-started/first-server", name: "docs-article", public: true },
]

export const themes = ["light", "dark"] as const
export type Theme = (typeof themes)[number]
