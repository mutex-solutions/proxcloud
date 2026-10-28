# Glossary

Domain terms as ProxCloud uses them, grounded in the entities in
`internal/platform/*` and their PostgreSQL tables. Where a term is easy to
confuse with a neighbor, that is called out explicitly.

## The core trio (read this first)

- **Organization** (`organization`) — the tenant / account boundary. It owns
  projects and members, and points at exactly one billing profile
  (`billingProfileId`). This is the "who is the customer" object.
- **Project** (`project`) — a workspace inside an organization that holds actual
  cloud resources and has its own embedded members. A project belongs to one
  organization and is billed against that organization's billing profile.
- **Billing profile** (`billingProfile`) — the "who pays and how" object: legal
  identity (name/company/VAT/address), currency, payment method, credit balance,
  suspension and price-plan configuration, and validation state. One organization
  → one billing profile.

So: **organization = the account, project = a workspace in it, billing profile =
its payment identity.** A resource lives in a project; the bill for it lands on
the organization's billing profile.

## Identity and membership

- **User** (`users`) — a person who has signed in, keyed by their IdP subject
  (`sub`). Holds profile fields, consent, and custom info. Created only after an
  explicit initialization call, not merely by presenting a token.
- **Member** — the link between a `sub` and an organization
  (`organization_members`) or, embedded, between a `sub` and a project
  (`Membership`). A member carries a **role**.
- **Role** — a member's permission set. Built-in roles are `OWNER`, `ADMIN`, and `MEMBER`;
  an organization can also define **custom roles** (`roleDefinition`) that name an
  explicit list of permissions. Policy resolves a caller's effective permissions
  from their role.
- **Billing-profile owner** — the single `sub` recorded on a billing profile (its
  payer). Distinct from an org/project *member*: being a member of an
  organization does not make you the payer, and the payer is one person while
  members are many. **User ≠ member ≠ billing-profile owner**: a user is the
  person, a member is that person's role in an org/project, the billing-profile
  owner is the one person financially responsible.

## Cloud

- **External service / cloud provider** (`externalService`) — a configured
  connection to a backing provider. Its `type` is `CLOUD` (a Proxmox VE cluster,
  a Ceph RGW, or a Kamaji/DBaaS management cluster), `CPANEL`, or `PAYMENT`; its
  encrypted `secret` holds credentials (never serialized). A `CLOUD` service's
  `config.provider` (`proxmox`, `ceph-s3`, `kamaji` or `dbaas`) and regions define
  where resources are created.
- **Region** — a named location within a cloud external service (from its config).
  One Proxmox cluster is one region. Resources, sync, and pricing scope are
  addressed per region.
- **Cloud resource** (`cloudResource`) — a locally cached record of one real
  cloud object (server, volume, network, security group, bucket, …), keyed by
  service + external id, kept current by sync and by ProxCloud's own write paths.
  This cache is what the console renders and what rating reads.
- **Pool** — the Proxmox resource pool a project owns (`<poolPrefix><projectId>`,
  default prefix `pc-`). Every project VM and template lives in it; it is the
  tenancy boundary on the cluster.
- **Vnet / SDN zone** — a project network is a Proxmox SDN vnet with one subnet
  carved from the provider's `cidrPool`, inside an SDN zone (one shared zone, or
  one EVPN zone per project).
- **Public network** — an operator-designated bridge with an IPv4 pool. A server
  reaches the internet inbound through a public NIC with one address from it;
  there are no floating IPs.
- **Storage profile** — a slug on the Proxmox provider mapping to a storage id per
  node, with capability flags validated against detection, a derived tier and
  class, and `bootDisk` / `dbaas` caps; flavors and volumes reference it.
- **Storage class** — `performance`, `standard` or `capacity` per Proxmox storage:
  detected from the storage type, overridable by the admin. A capacity-class
  profile (for example an HDD NFS share) never takes boot disks, databases or
  snapshots.
- **Capability detection** — reading each Proxmox storage's type, sharing,
  content, snapshot/thin support and per-node free space from the cluster
  (`config.capabilities`); the basis for profile validation and placement.
- **Template** — a VM that servers are fully cloned from: an admin OS template
  from the provider config, or a project template made from a tenant VM (shown
  as an image).

## Pricing and billing

- **Price plan** (`pricePlan`) — a named, enable-able collection of pricing rules,
  optionally scoped to specific service providers.
- **Price plan rule** (`pricePlanRule`) — within a plan, prices one **resource
  type** over a **time unit** (minute/hour/month), with per-attribute graduated
  tier **prices**, plus optional **filters** and **modifiers**.
- **Price adjustment rule** (`priceAdjustmentRule`) — a plan-level surcharge or
  discount applied on top of rule output, via targeted tiers (e.g. volume
  discounts).
- **Resource type** — the kind of billable thing a rule applies to (e.g.
  instance, volume, bucket). Maps a cloud resource to the rules that
  price it.
- **Billable attribute** — a named, measurable quantity of a resource that a rule
  puts a price on (e.g. vCPUs, RAM, disk GB, hours). The rule's `prices` are
  keyed by attribute.
- **Bill** (`bill`) — a per-period statement for a billing profile, with line
  items, tax, and a lifecycle (`OPEN → SENT → PAID`). Finalized monthly, then
  collectable and dunnable.
- **Transaction** — a money movement on a billing profile: card charges
  (`creditCardTransaction`), account-credit movements
  (`accountCreditTransaction`), collections (`collectTransaction`), bank transfers
  (`bankTransfer`).
- **Account credit** (`accountCredit`) — the billing profile's prepaid balance
  that bills and usage draw down; topped up by deposits, promotions, and refunds.
- **Savings plan** (`savingsPlan`) / **contract** (`savingsContract`) — a
  commitment offering (a plan definition) and a customer's active instance of it
  (a contract, with tiers, schedule, and an expiry that jobs remind on and
  expire).
- **Promotional credit** (`promotionalCredit`) / **promo code**
  (`promotionCode`, redeemed via `promotionCodeRedemption`) — granted or
  code-redeemed credit added to a billing profile's balance.

## Lifecycle

- **Activation** — bringing a billing profile (and its projects) into service:
  enabling projects, provisioning memberships, bootstrapping projects onto the
  cloud providers,
  applying sign-up/provisioning credits (`billing.ActivationService`).
- **Suspension** (`suspension`) — taking a billing profile out of service for
  non-payment or policy: suspending its projects' VMs and disabling the
  projects; reversed on resume. Driven by dunning and by admin action.
- **Validation (KYC)** (`identityValidation`) — the identity/know-your-customer
  check on a billing profile; an approved validation is a precondition for
  activation.

## Catalog and platform

- **Flavor** (`flavor`) — an admin-curated server size for Proxmox (vCPUs, RAM,
  disk, optional GPU model/count and storage profile); Proxmox has no flavors of
  its own.
- **Flavor category** (`flavorCategory`) — an operator-defined grouping of
  compute flavors for presentation in the console (alongside image groups /
  categories for OS images).
- **HMAC key** (`hmac_keys`) — an access-key/secret pair (`pk…`/`sk…`) used to
  sign requests to the public admin API with AWS SigV4. Minted by an operator;
  the secret is shown once.
- **MCP** — Model Context Protocol; the assistant-facing tool surface (see
  [ADR-0006](adr/0006-mcp-in-process-dispatch.md)) that dispatches in-process
  through the same auth and policy as the REST API.
- **Realm** — an identity partition in the OIDC issuer. ProxCloud uses separate
  realms/clients per audience: the customer console (`clients`), the operator
  admin console, and the public admin API. See
  [ADR-0003](adr/0003-auth-as-oauth2-resource-server.md).
