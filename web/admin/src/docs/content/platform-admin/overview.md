# Running the Platform

The operator's job ends where yours begins. Once ProxCloud is installed, wired to your identity provider, and pointed at your Proxmox VE clusters, the raw platform is running — but it isn't yet a business. Turning it into one is the platform administrator's work: deciding what you sell, setting its price, controlling how customers get switched on, and making sure the money actually gets collected. Everything below happens in the admin console — the same interface serving these docs.

## A suggested order of setup

Most deployments configure things in roughly this sequence. You can jump around, but pricing tends to come first because everything downstream depends on it.

1. **Pricing** — define your price plans and the resource types they charge for.
2. **Clouds** — register and tune your Proxmox VE clusters and S3 stores.
3. **Activation** — decide how a fresh sign-up becomes a usable account.
4. **Money automation** — invoicing, suspension of non-payers, and sign-up credits.
5. **Branding** — make the platform look like yours.

## Where things live in the console

### Money

Anything financial sits under the **System** group in the sidebar, spread across the **Price plans**, **Taxes**, **Savings plans**, and **Billing configuration** pages.

| What you're configuring | Console location | Guide |
|---|---|---|
| Price plans and their pricing rules | System → Price plans | [Price Plans](/docs/platform-admin/billing/price-plans) |
| The billable resource types and their attributes | Used while building rules | [Resource Types](/docs/platform-admin/billing/resource-types) |
| The platform currency | System → Billing configuration | [Currency](/docs/platform-admin/billing/currency) |
| Per-country tax rates | System → Taxes | [Tax Rates](/docs/platform-admin/billing/tax) |
| Invoice generation and gateways | System → Billing configuration, System → Integrations | [Invoicing](/docs/platform-admin/billing/invoicing) |
| Suspending accounts that stop paying | System → Billing configuration | [Automatic Suspension](/docs/platform-admin/billing/suspension) |
| Credit granted at sign-up | System → Billing configuration | [Sign-up Credits](/docs/platform-admin/billing/signup-credits) |
| Commitment-based discounts | System → Savings plans | [Savings Plans](/docs/platform-admin/billing/savings-plans) |

### Customers

How a new registration turns into an active, provisioned account is covered in [Account Activation](/docs/platform-admin/account-activation). Ongoing customer management happens under the **Client area** group: **Users**, **Organizations**, **Billing profiles**, **Validations**, and **Projects**.

Each project's detail page also carries a **public networks** allow-list. By default a project's servers can attach a public NIC to every public network (admin-designated bridge) its cloud provider declares. Restrict the list to a chosen subset when a customer should only get addresses from specific pools. The restriction is enforced when a public NIC is attached, and a network outside the list is rejected.

### Clouds

Your Proxmox VE clusters and other providers are managed from **System → Cloud providers**. That's where you register a cluster, shape its networks, storage profiles, templates and GPU mappings, and switch services (compute, networks, volumes, object storage, managed Kubernetes and databases) on or off. The Cloud Providers guides cover all of this.

### Look and settings

- **System → Platform** — general platform configuration.
- **System → Flavors (Proxmox)** — the flavor catalog (vCPU, RAM, disk, optional GPU and storage profile).
- **System → Instances** — the flavor tiers customers pick from.
- **System → Custom menu** — extra entries in the customer navigation.
- **System → Message templates** — the emails ProxCloud sends.
- **System → Admin permissions** and **System → API keys** — admin access control and the signed Admin API.
