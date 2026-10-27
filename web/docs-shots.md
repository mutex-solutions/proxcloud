# Docs screenshot backlog

> All screenshots were removed for the ProxCloud rebrand (they showed the previous product). Recapture every item below on the ProxCloud lab, save under `web/{client,admin}/public/docs-img/`, and re-add the `![...](/docs-img/...)` line to the listed page.

## admin-billing

- [ ] `/docs-img/billing-configuration-activation.png` — Billing configuration page, Activation tab, showing the auto-activation switch and the constraint selectors (KYC, payment method, deposit, validation) — page: `administrators-manual/account-activation.md`
- [ ] `/docs-img/validations-queue.png` — Validations page listing pending billing profile validations with Approve and Reject buttons — page: `administrators-manual/account-activation.md`
- [ ] `/docs-img/billing-profile-activate.png` — Billing profile detail page showing the Activate action on a non-active profile — page: `administrators-manual/account-activation.md`
- [ ] `/docs-img/price-plans-create.png` — Price plans page with the create dialog open, showing name, access mode and enabled fields — page: `administrators-manual/billing/price-plans.md`
- [ ] `/docs-img/price-plan-rules.png` — Price plan detail page listing pricing rules with resource type, priced attribute and time unit columns — page: `platform-admin/billing/price-plans.md` (captured on the live "Public rate card" plan, 2026-07-09)
- [ ] `/docs-img/price-plan-rule-create.png` — Rule creation dialog with a resource type selected and its attribute list expanded — page: `platform-admin/billing/price-plans.md` (Add-rule dialog, resource type `instance`, attributes expanded; dialog cancelled, no write)
- [ ] `/docs-img/billing-configuration-base-currency.png` — Billing configuration page, Business details tab, with the Base currency dropdown open — page: `administrators-manual/billing/currencies.md`
- [ ] `/docs-img/taxes-add-tax.png` — Taxes page with the add-tax dialog open, showing name, rate and country fields — page: `administrators-manual/billing/tax-rules.md`
- [ ] `/docs-img/billing-configuration-invoice-gateway.png` — Billing configuration page, Business details tab, showing the Invoice gateway selector — page: `administrators-manual/billing/configure-invoicing.md`
- [ ] `/docs-img/suspension-balance.png` — Billing configuration Settings tab with Automatic suspension enabled, type BALANCE, showing notification limits and the suspend-at balance — page: `administrators-manual/billing/automated-suspension.md`
- [ ] `/docs-img/suspension-due-date.png` — Billing configuration Settings tab with Automatic suspension type DUE_DATE, showing day-based notification limits and the suspend-at days — page: `administrators-manual/billing/automated-suspension.md`
- [ ] `/docs-img/provisioning-promotional-credits.png` — Billing configuration Activation tab, Provisioning promotional credits section with a credit row (amount and days valid) filled in — page: `administrators-manual/billing/promotional-credits.md`
- [ ] `/docs-img/savings-plan-targets.png` — Savings plan creation form showing the target resource selection and duration — page: `administrators-manual/billing/savings-plans.md`
- [ ] `/docs-img/savings-plan-discount-tiers.png` — Discount tier rows (start amount and discount percent) for a savings plan schedule — page: `administrators-manual/billing/savings-plans.md`

## admin-openstack-settings

- [ ] `/docs-img/add-openstack-cloud-form.png` — ProxCloud admin: System > Cloud providers > Add provider dialog with the General and Configuration sections filled in
- [skip: no discovered-regions/country view exists in current admin UI] `/docs-img/add-openstack-cloud-regions.png` — ProxCloud admin: Discovered regions list after a successful Connect, showing region name and country fields
- [ ] `/docs-img/enable-disable-services-tab.png` — ProxCloud admin: cloud provider detail, Services tab with per-region toggles for compute, volumes, load balancer, shares, object store, DNS
- [ ] `/docs-img/instance-metrics-feature-toggle.png` — ProxCloud admin: cloud provider detail, Features section with the instance metrics toggle enabled
- [skip: instance-metrics feature toggle is OFF on live provider; no charts rendered] `/docs-img/instance-metrics-client-charts.png` — client portal: server detail page showing the CPU/memory/network charts rendered from Gnocchi data
- [ ] `/docs-img/domain-reseller-provider.png` — ProxCloud admin: cloud provider form configured with Private visibility and Domain administrator mode for a reseller domain
- [skip: no reseller billing profile exists; profile detail has no reseller flag UI] `/docs-img/domain-reseller-billing-profile.png` — ProxCloud admin: billing profile detail with the reseller flag enabled and the private provider attached
- [ ] `/docs-img/flavor-categories-list.png` — ProxCloud admin: System > Instances page listing flavor categories with their assigned flavors
- [ ] `/docs-img/flavor-categories-client-tabs.png` — client portal: create-server Hardware section showing one tab per flavor category
- [ ] `/docs-img/custom-menu-item-form.png` — ProxCloud admin: System > Custom menu page with the Add menu item form (display name, URL, icon)
- [ ] `/docs-img/custom-menu-item-client-sidebar.png` — client portal: sidebar showing the custom menu section with the configured Support link and icon
- [skip: no Keycloak admin console credentials provided] `/docs-img/customize-keycloak-theme-realm.png` — Keycloak admin console: Realm settings > Themes with the custom proxcloud theme selected as login theme
- [ ] `/docs-img/customize-keycloak-theme-login.png` — branded client login page rendered with the custom Keycloak theme
- [removed] `/docs-img/marketing-events-gtm-trigger.png` — conversion-tracking.md page DELETED 2026-07-09 (GTM/dataLayer feature has no frontend implementation); shot obsolete

## client

- [ ] `/docs-img/client-portal-overview.png` — Project dashboard after login, with the full sidebar (Compute, Storage, Network, Platform, Billing, Organization) visible — page: `client-manual.md`
- [skip: profile already activated; form only shown before first deposit] `/docs-img/billing-details-form.png` — Billing > Funds page showing the billing details form (name, phone, address, country fields) before first deposit — page: `client-manual/account-activation.md`
- [ ] `/docs-img/deposit-funds.png` — Deposit dialog on Billing > Funds with amount entered and the card payment element visible — page: `client-manual/account-activation.md`
- [ ] `/docs-img/savings-plans-catalog.png` — Billing > Savings plans page showing the published plans with duration options and "up to X% upfront / no upfront" tier summaries — page: `client-manual/savings-plans.md`
- [skip: no published savings plans; purchase form unreachable] `/docs-img/savings-plans-purchase.png` — Purchase form with duration selected, monthly committed amount filled in, start-month selector and the "Pay upfront" checkbox — page: `client-manual/savings-plans.md`
- [skip: no contracts exist; section is empty duplicate of catalog shot] `/docs-img/savings-contracts-table.png` — Active savings contracts table showing committed/month, discount %, end date, and the Extend/Cancel actions — page: `client-manual/savings-plans.md`
- [ ] `/docs-img/invite-member.png` — Organization > Members page with the email invite field and Send invite button, plus the existing-member "Add to project" picker — page: `client-manual/user-invitation.md`
- [skip: needs a live invite token; sending an invite is a write] `/docs-img/join-project-accept.png` — The /join-project screen showing the pending invitation with Accept invite and Decline buttons — page: `client-manual/user-invitation.md`
- [ ] `/docs-img/admin-console-first-login.png` — ProxCloud admin console right after first sign-in on a fresh install — page: `operators-manual/kubernetes-install.md`

## client-compute

VM lifecycle shots for the "Launch Your First Server" walkthrough, captured 2026-07-13 against a throwaway demo server that was deleted right after.

- [ ] `/docs-img/create-server-form.png` — The Create server form (single 8-section page) filled in: image, flavor, network, Assign floating IP on, a security group and a name — page: `getting-started/first-server.md`
- [ ] `/docs-img/server-building.png` — Servers list filtered to the demo server showing the amber **Build** status right after launch — page: `getting-started/first-server.md`
- [ ] `/docs-img/server-active.png` — Servers list filtered to the demo server showing the green **Active** status with its IP addresses assigned (VM IPs blurred) — page: `getting-started/first-server.md`
- [ ] `/docs-img/server-detail-actions.png` — Server detail page with the Start/Stop/Reboot header buttons and the More actions menu open (Delete highlighted); VM IPs and the account email (top-right) blurred — page: `getting-started/first-server.md`
- [ ] `/docs-img/server-delete-confirm.png` — The "Delete server" confirmation dialog ("This cannot be undone.") — page: `getting-started/first-server.md`

## client-storage

Storage-guide shots, captured 2026-07-13 against throwaway demo resources (a volume, its snapshot, and a bucket) that were deleted right after.

- [ ] `/docs-img/create-volume.png` — The Create volume dialog (name, size in GB, optional type/AZ) — page: `guides/volumes.md`
- [ ] `/docs-img/volume-attach.png` — Volume `docs-demo-vol` (ceph-ssd1) attached to the `docs-demo` server, shown on the server detail Volumes tab (VM IP blurred) — page: `guides/volumes.md`
- [ ] `/docs-img/volume-snapshot.png` — The Snapshots list showing `docs-demo-snap` (a snapshot of `docs-demo-vol`) — page: `guides/volumes.md`
- [ ] `/docs-img/create-bucket.png` — Create bucket dialog showing the Swift/S3-(Ceph) backend picker, globally-unique-name note and object-lock option — page: `guides/object-storage.md`
- [ ] `/docs-img/bucket-objects.png` — Bucket object explorer with a folder and two uploaded files, and the Private toggle — page: `guides/object-storage.md`
- [ ] `/docs-img/s3-access-keys.png` — S3 access keys page: project credentials + additional scoped keys. **Access key IDs replaced with AWS example values, the secret left masked, and the S3 endpoint hostname blurred (both the Endpoint field and the CLI example) — never publish real keys/endpoints** — page: `guides/object-storage.md`
- [ ] `/docs-img/bucket-settings.png` — Bucket settings dialog (General tab: versioning/object-lock/quota) with the General/Website/Access/Lifecycle/Policy tab bar — page: `guides/object-storage.md`
