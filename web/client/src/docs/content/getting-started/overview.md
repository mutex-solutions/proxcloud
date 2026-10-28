# Meet the ProxCloud Portal

ProxCloud is a self-service cloud console. From it you launch and run cloud infrastructure, watch what you're spending, and share the work with your team. This page is a quick orientation to how the portal is laid out before you dive into specific tasks.

The organizing idea is the **project**. Nearly everything you create — servers, disks, networks, buckets, bills — belongs to one project. When you sign in you land inside a project (or create one), and the sidebar exposes every resource and setting scoped to that project.

## Moving between projects and pages

- A **project switcher** sits at the top of the sidebar. Use it to jump between the projects you're a member of. Because resources, members, and billing are all per-project, switching changes the entire context around you.
- The **Dashboard** gives you a quick summary of the project you're in: how many resources you have and what happened recently.
- A **More** menu may surface extra links your cloud operator has published — support desks, status pages, and the like.

## The resource map

Everything the portal manages falls into a handful of groups. The table below is the map; the task guides go into each area in depth. An entry only shows up when your operator has enabled that service in your region.

| Group | Where in the sidebar | What it's for |
|-------|----------------------|---------------|
| Compute | Servers, Key pairs, Images | Run virtual machines, keep your SSH key pairs, and use or save machine images |
| Storage | Volumes, Snapshots, Object storage, S3 access keys | Add disks to your servers, keep S3 buckets, and manage the keys that reach them |
| Network | Networks, Security groups | Build private networks and firewall traffic |
| Platform | Kubernetes, Databases | Managed Kubernetes and database clusters, when your operator offers them |
| Billing | Savings plans, Promotional credits, Funds, Cards, Billing history | Deposits, payment cards, committed-use discounts, invoices, and past bills |
| Organization | Members, Projects, Audit log, Roles, Settings | Team membership, project administration, and the activity trail |
| Account | Account | Your own profile and preferences |

## Where to head next

- Brand-new here? Go to [Signing In and Activating Billing](/docs/getting-started/account-setup). You need an active billing profile before the portal will let you build anything.
- Ready to build? [Launch Your First Server](/docs/getting-started/first-server) walks the whole flow end to end.
- Cutting the bill on steady workloads? See [Savings Plans](/docs/guides/savings-plans).
- Bringing in colleagues? [Teammates and Invitations](/docs/guides/team-members) covers it.
