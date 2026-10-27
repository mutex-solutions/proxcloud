# Reselling Through Keystone Domains

ProxCloud supports a reseller arrangement built on Keystone domains. Each reseller gets a dedicated OpenStack domain, runs their own ProxCloud instance against it, and you invoice the reseller once a month for everything consumed inside that domain. Because domains isolate hard at the logical level, a reseller's admin user can only see and touch projects within their own domain.

## The shape of the model

1. You, the platform operator, create an isolated Keystone domain plus a domain-scoped admin user for the reseller.
2. The reseller stands up their own ProxCloud instance and connects it to your cloud in **Domain administrator** mode, then manages their own customers inside the domain.
3. You register that same domain in *your* ProxCloud as a **Private**, read-only cloud provider, so your parent instance can track and total up the reseller's consumption for billing.

## Creating the reseller's domain

```bash
openstack domain create reseller-acme --description "ACME reseller"
openstack user create acme-admin --domain reseller-acme --password <secret>
openstack role add --user acme-admin --domain reseller-acme admin
```

Give the reseller three things: your Keystone endpoint URL, the domain name, and the domain-admin credentials.

## Registering the domain in your ProxCloud

In your own admin portal, go to **System > Cloud providers** and add a provider pointed at the same Keystone endpoint:

| Field | Value |
|---|---|
| Visibility | `Private` — never expose it to ordinary clients. |
| Authentication mode | Domain administrator. |
| Domain name | The reseller's domain, e.g. `reseller-acme`. |

This registration is purely for tracking. It lets the sync engine inventory every project and resource in the reseller's domain so you can compute their consolidated bill. Treat it as read-only — the reseller's own ProxCloud instance is what actually manages the lifecycle.

## Marking the reseller's billing profile

Create a billing profile for the reseller under **Client area > Billing profiles**, switch on its reseller flag, and attach the private cloud provider you just registered. From then on the reseller gets one monthly bill aggregating all consumption across every project in their domain, rather than a stack of per-project invoices.

<!-- screenshot: /docs-img/domain-reseller-billing-profile.png — ProxCloud admin: billing profile detail with the reseller flag enabled and the private provider attached -->

## Who owns what

| Concern | Platform operator | Reseller |
|---|---|---|
| Keystone domain + domain admin | Creates | Uses |
| ProxCloud instance | Parent instance (tracking + reseller billing) | Own instance (their customers, pricing, branding) |
| End-customer billing | — | Reseller's ProxCloud |
| Reseller invoice | Monthly, aggregated per domain | Pays |
