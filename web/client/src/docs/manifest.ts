// Docs sidebar manifest. Each page maps to src/docs/content/<slug>.md.
// Slugs mirror the public URL: /docs/<slug>. Titles are sentence case (design-system).
export type DocPage = { slug: string; title: string }
export type DocSection = { title: string; pages: DocPage[] }

export const docsTitle = "ProxCloud docs"

export const sections: DocSection[] = [
  {
    title: "Getting started",
    pages: [
      { slug: "getting-started/overview", title: "Meet the ProxCloud portal" },
      { slug: "getting-started/account-setup", title: "Signing in & activating billing" },
      { slug: "getting-started/first-server", title: "Launch your first server" },
    ],
  },
  {
    title: "Guides",
    pages: [
      { slug: "guides/servers", title: "Working with servers" },
      { slug: "guides/networks", title: "Networking" },
      { slug: "guides/load-balancers", title: "Load balancers" },
      { slug: "guides/volumes", title: "Volumes & snapshots" },
      { slug: "guides/object-storage", title: "Object storage buckets" },
      { slug: "guides/dns", title: "DNS zones" },
      { slug: "guides/team-members", title: "Teammates & invitations" },
      { slug: "guides/savings-plans", title: "Savings plans" },
      { slug: "guides/ai-agents", title: "AI agent access (MCP)" },
      { slug: "guides/sandboxes", title: "Sandboxes" },
    ],
  },
  {
    title: "Concepts",
    pages: [
      { slug: "concepts/billing-and-metering", title: "How metering & billing work" },
      { slug: "concepts/provisioning", title: "How provisioning works" },
      { slug: "concepts/identity", title: "How identity works" },
    ],
  },
  {
    title: "API",
    pages: [
      { slug: "api/sandboxes", title: "Sandbox API" },
      { slug: "api/e2b", title: "E2B API" },
    ],
  },
  {
    title: "Self-hosting",
    pages: [
      { slug: "self-hosting/overview", title: "Architecture & deployment" },
      { slug: "self-hosting/install", title: "Installing on Kubernetes" },
      { slug: "self-hosting/quickstart", title: "MicroK8s quickstart" },
      { slug: "self-hosting/sso", title: "Single sign-on" },
      { slug: "self-hosting/custom-ca", title: "Trusting a custom CA" },
      { slug: "self-hosting/backup", title: "Backup & recovery" },
    ],
  },
]

export const defaultSlug = sections[0].pages[0].slug
