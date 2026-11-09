# Docs screenshot backlog

> All screenshots were removed for the ProxCloud rebrand (they showed the previous product). Recapture every item below on the ProxCloud lab, save under `web/{client,admin}/public/docs-img/`, and re-add the `![...](/docs-img/...)` line to the listed page. Mask lab addresses before capturing.
>
> Recaptured on the lab so far: `create-server`, `servers-list`, `server-detail`, `create-volume`, `volumes-list`, `create-network`, `security-group-rules` (customer docs), `add-proxmox-provider`, `provider-storage`, `price-plans-create`, `price-plan-rules` (operator docs).

## admin-billing

- [ ] `/docs-img/billing-configuration-activation.png` — Billing configuration page, Activation tab, showing the auto-activation switch and the constraint selectors (KYC, payment method, deposit, validation) — page: `administrators-manual/account-activation.md`
- [ ] `/docs-img/validations-queue.png` — Validations page listing pending billing profile validations with Approve and Reject buttons — page: `administrators-manual/account-activation.md`
- [ ] `/docs-img/billing-profile-activate.png` — Billing profile detail page showing the Activate action on a non-active profile — page: `administrators-manual/account-activation.md`
- [x] `/docs-img/price-plans-create.png` — Price plans page with the create dialog open, showing name, access mode and enabled fields — page: `administrators-manual/billing/price-plans.md`
- [x] `/docs-img/price-plan-rules.png` — Price plan detail page listing pricing rules with resource type, priced attribute and time unit columns — page: `platform-admin/billing/price-plans.md` (captured on the live "Public rate card" plan, 2026-07-09)
- [ ] `/docs-img/price-plan-rule-create.png` — Rule creation dialog with a resource type selected and its attribute list expanded — page: `platform-admin/billing/price-plans.md` (Add-rule dialog, resource type `instance`, attributes expanded; dialog cancelled, no write)
- [ ] `/docs-img/billing-configuration-base-currency.png` — Billing configuration page, Business details tab, with the Base currency dropdown open — page: `administrators-manual/billing/currencies.md`
- [ ] `/docs-img/taxes-add-tax.png` — Taxes page with the add-tax dialog open, showing name, rate and country fields — page: `administrators-manual/billing/tax-rules.md`
- [ ] `/docs-img/billing-configuration-invoice-gateway.png` — Billing configuration page, Business details tab, showing the Invoice gateway selector — page: `administrators-manual/billing/configure-invoicing.md`
- [ ] `/docs-img/suspension-balance.png` — Billing configuration Settings tab with Automatic suspension enabled, type BALANCE, showing notification limits and the suspend-at balance — page: `administrators-manual/billing/automated-suspension.md`
- [ ] `/docs-img/suspension-due-date.png` — Billing configuration Settings tab with Automatic suspension type DUE_DATE, showing day-based notification limits and the suspend-at days — page: `administrators-manual/billing/automated-suspension.md`
- [ ] `/docs-img/provisioning-promotional-credits.png` — Billing configuration Activation tab, Provisioning promotional credits section with a credit row (amount and days valid) filled in — page: `administrators-manual/billing/promotional-credits.md`
- [ ] `/docs-img/savings-plan-targets.png` — Savings plan creation form showing the target resource selection and duration — page: `administrators-manual/billing/savings-plans.md`
- [ ] `/docs-img/savings-plan-discount-tiers.png` — Discount tier rows (start amount and discount percent) for a savings plan schedule — page: `administrators-manual/billing/savings-plans.md`

## admin-cloud-provider-settings

- [ ] `/docs-img/add-cloud-provider-form.png` — ProxCloud admin: System > Cloud providers > Add provider dialog with the General and Configuration sections filled in
- [ ] `/docs-img/enable-disable-services-tab.png` — ProxCloud admin: cloud provider detail, Services tab with per-region toggles for compute, network, images and volumes

## client-storage

Storage-guide shots, captured 2026-07-13 against throwaway demo resources (a volume, its snapshot, and a bucket) that were deleted right after.

- [x] `/docs-img/create-volume.png` — The Create volume dialog (name, size in GB, optional type/AZ) — page: `guides/volumes.md`
- [ ] `/docs-img/volume-attach.png` — Volume `docs-demo-vol` (ceph-ssd1) attached to the `docs-demo` server, shown on the server detail Volumes tab (VM IP blurred) — page: `guides/volumes.md`
- [ ] `/docs-img/volume-snapshot.png` — The Snapshots list showing `docs-demo-snap` (a snapshot of `docs-demo-vol`) — page: `guides/volumes.md`
- [ ] `/docs-img/create-bucket.png` — Create bucket dialog showing the globally-unique-name note and object-lock option — page: `guides/object-storage.md`
- [ ] `/docs-img/bucket-objects.png` — Bucket object explorer with a folder and two uploaded files, and the Private toggle — page: `guides/object-storage.md`
- [ ] `/docs-img/s3-access-keys.png` — S3 access keys page: project credentials + additional scoped keys. **Access key IDs replaced with AWS example values, the secret left masked, and the S3 endpoint hostname blurred (both the Endpoint field and the CLI example) — never publish real keys/endpoints** — page: `guides/object-storage.md`
- [ ] `/docs-img/bucket-settings.png` — Bucket settings dialog (General tab: versioning/object-lock/quota) with the General/Website/Access/Lifecycle/Policy tab bar — page: `guides/object-storage.md`
