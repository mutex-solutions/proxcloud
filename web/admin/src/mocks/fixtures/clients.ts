// Client-area seed data: users, organizations, projects, billing profiles,
// cloud resources, bills, transactions, credits, validations. Everything is
// cross-referenced (bp -> org -> projects -> resources) so detail pages and
// joins render consistently.

type Doc = Record<string, any>

// ─── Users ────────────────────────────────────────────────────────────────────

export const users: Doc[] = [
  {
    id: "usr-0001",
    sub: "kc-7f3a1b2c-usr-0001",
    email: "alice.tran@acmerobotics.io",
    firstName: "Alice",
    lastName: "Tran",
    identities: [{ sub: "kc-7f3a1b2c-usr-0001", issuer: "https://auth.example.com/realms/proxcloud" }],
    createdAt: "2025-11-03T09:14:00Z",
  },
  {
    id: "usr-0002",
    sub: "kc-9d21e4aa-usr-0002",
    email: "bob.kowalski@northwindlabs.com",
    firstName: "Bob",
    lastName: "Kowalski",
    identities: [{ sub: "kc-9d21e4aa-usr-0002", issuer: "https://auth.example.com/realms/proxcloud" }],
    createdAt: "2026-01-18T15:42:00Z",
  },
  {
    id: "usr-0003",
    sub: "kc-4b77c9de-usr-0003",
    email: "carol.mendes@acmerobotics.io",
    firstName: "Carol",
    lastName: "Mendes",
    identities: [{ sub: "kc-4b77c9de-usr-0003", issuer: "https://auth.example.com/realms/proxcloud" }],
    createdAt: "2026-03-02T11:05:00Z",
  },
  {
    id: "usr-0004",
    sub: "kc-e1508f3b-usr-0004",
    email: "david.osei@heliumcompute.dev",
    firstName: "David",
    lastName: "Osei",
    identities: [{ sub: "kc-e1508f3b-usr-0004", issuer: "https://auth.example.com/realms/proxcloud" }],
    createdAt: "2026-07-06T08:27:00Z",
  },
]

// Keycloak credentials per user sub (user-management tab).
export const credentialsBySub: Record<string, Doc[]> = {
  "kc-7f3a1b2c-usr-0001": [
    { id: "cred-0001", sub: "kc-7f3a1b2c-usr-0001", type: "password", password: { configured: true }, createdAt: "2025-11-03T09:14:00Z" },
    { id: "cred-0002", sub: "kc-7f3a1b2c-usr-0001", type: "otp", totp: { verified: true, deviceName: "Pixel 9" }, createdAt: "2025-11-04T10:00:00Z" },
  ],
  "kc-9d21e4aa-usr-0002": [
    { id: "cred-0003", sub: "kc-9d21e4aa-usr-0002", type: "password", password: { configured: true }, createdAt: "2026-01-18T15:42:00Z" },
  ],
  "kc-4b77c9de-usr-0003": [
    { id: "cred-0004", sub: "kc-4b77c9de-usr-0003", type: "password", password: { configured: true }, createdAt: "2026-03-02T11:05:00Z" },
  ],
  "kc-e1508f3b-usr-0004": [
    { id: "cred-0005", sub: "kc-e1508f3b-usr-0004", type: "password", password: { configured: true }, createdAt: "2026-07-06T08:27:00Z" },
  ],
}

// ─── Organizations ────────────────────────────────────────────────────────────

export const organizations: Doc[] = [
  {
    id: "org-0001",
    name: "Acme Robotics",
    description: "Warehouse automation fleet - production and staging workloads.",
    billingProfileId: "bp-0001",
    memberCount: 2,
    projectCount: 2,
    createdAt: "2025-11-03T09:20:00Z",
  },
  {
    id: "org-0002",
    name: "Northwind Labs",
    description: "Research group running episodic GPU training jobs.",
    billingProfileId: "bp-0002",
    memberCount: 1,
    projectCount: 1,
    createdAt: "2026-01-18T16:00:00Z",
  },
]

export const orgMembers: Record<string, Doc[]> = {
  "org-0001": [
    { sub: "kc-7f3a1b2c-usr-0001", firstName: "Alice", lastName: "Tran", email: "alice.tran@acmerobotics.io", role: "OWNER" },
    { sub: "kc-4b77c9de-usr-0003", firstName: "Carol", lastName: "Mendes", email: "carol.mendes@acmerobotics.io", role: "MEMBER" },
  ],
  "org-0002": [
    { sub: "kc-9d21e4aa-usr-0002", firstName: "Bob", lastName: "Kowalski", email: "bob.kowalski@northwindlabs.com", role: "OWNER" },
  ],
}

// ─── Projects ─────────────────────────────────────────────────────────────────

export const projects: Doc[] = [
  {
    id: "prj-0001",
    name: "acme-production",
    status: "ENABLED",
    organizationId: "org-0001",
    billingProfileId: "bp-0001",
    memberships: [
      { sub: "kc-7f3a1b2c-usr-0001", role: "OWNER" },
      { sub: "kc-4b77c9de-usr-0003", role: "MEMBER" },
    ],
    services: [{ serviceId: "svc-proxmox-01", provider: "proxmox", region: "eu-central-1", pool: "pc-prj-0001" }],
    publicNetworkIds: null,
    publicNetworksVisible: true,
    quota: { gpu: { "nvidia-a100-80gb": 4 } },
    createdAt: "2025-11-03T09:25:00Z",
  },
  {
    id: "prj-0002",
    name: "acme-staging",
    status: "ENABLED",
    organizationId: "org-0001",
    billingProfileId: "bp-0001",
    memberships: [{ sub: "kc-7f3a1b2c-usr-0001", role: "OWNER" }],
    services: [{ serviceId: "svc-proxmox-01", provider: "proxmox", region: "eu-central-1", pool: "pc-prj-0002" }],
    publicNetworkIds: ["net-ext-01"],
    publicNetworksVisible: false,
    quota: { gpu: { "nvidia-l40s": 2 } },
    createdAt: "2026-02-11T13:40:00Z",
  },
  {
    id: "prj-0003",
    name: "northwind-research",
    status: "DISABLED",
    organizationId: "org-0002",
    billingProfileId: "bp-0002",
    memberships: [{ sub: "kc-9d21e4aa-usr-0002", role: "OWNER" }],
    services: [{ serviceId: "svc-ceph-01", provider: "ceph-s3", region: "us-east-1", rgwUid: "proxcloud-prj-0003" }],
    createdAt: "2026-01-19T10:12:00Z",
  },
]

export const projectMembers: Record<string, Doc[]> = {
  "prj-0001": [
    { id: "usr-0001", sub: "kc-7f3a1b2c-usr-0001", email: "alice.tran@acmerobotics.io", firstName: "Alice", lastName: "Tran" },
    { id: "usr-0003", sub: "kc-4b77c9de-usr-0003", email: "carol.mendes@acmerobotics.io", firstName: "Carol", lastName: "Mendes" },
  ],
  "prj-0002": [
    { id: "usr-0001", sub: "kc-7f3a1b2c-usr-0001", email: "alice.tran@acmerobotics.io", firstName: "Alice", lastName: "Tran" },
  ],
  "prj-0003": [
    { id: "usr-0002", sub: "kc-9d21e4aa-usr-0002", email: "bob.kowalski@northwindlabs.com", firstName: "Bob", lastName: "Kowalski" },
  ],
}

// ─── Billing profiles ─────────────────────────────────────────────────────────
// Summaries (GET /admin/billing-profile[…]) carry the computed financials; the
// raw docs (GET /admin/billing-profile/search) add bank/iban/taxConfiguration/
// projectProvisioningQuota/suspension override.

export const billingProfiles: Doc[] = [
  {
    id: "bp-0001",
    firstName: "Alice",
    lastName: "Tran",
    fullName: "Alice Tran",
    company: true,
    companyName: "Acme Robotics Inc.",
    vatCode: "US88-2213456",
    email: "billing@acmerobotics.io",
    phone: "+1 415 555 0134",
    address: "500 Harrison Street",
    city: "San Francisco",
    county: "CA",
    country: "US",
    zipCode: "94105",
    status: "ACTIVE",
    currency: "USD",
    balance: 128.4,
    accountCredit: 250.0,
    promotionalCredit: 12.5,
    currentMonth: 342.18,
    currentMonthUsage: 342.18,
    taxPayer: true,
    organizationId: "org-0001",
    pricePlanConfig: { pricePlanIds: ["pp-0002"], includePublicPricePlans: true },
    verifications: [
      { type: "EMAIL", status: "VERIFIED", createdAt: "2025-11-03T09:30:00Z" },
      { type: "PAYMENT_METHOD", status: "VERIFIED", createdAt: "2025-11-05T14:00:00Z" },
    ],
    validationStatus: "APPROVED",
    createdAt: "2025-11-03T09:22:00Z",
  },
  {
    id: "bp-0002",
    firstName: "Bob",
    lastName: "Kowalski",
    fullName: "Bob Kowalski",
    company: true,
    companyName: "Northwind Labs GmbH",
    vatCode: "DE314159265",
    email: "finance@northwindlabs.com",
    phone: "+49 30 555 0188",
    address: "Torstrasse 44",
    city: "Berlin",
    county: "",
    country: "DE",
    zipCode: "10119",
    status: "SUSPENDED",
    currency: "EUR",
    balance: -42.1,
    accountCredit: 0,
    promotionalCredit: 0,
    currentMonth: 96.75,
    currentMonthUsage: 96.75,
    taxPayer: true,
    organizationId: "org-0002",
    pricePlanConfig: { pricePlanIds: [], includePublicPricePlans: true },
    verifications: [{ type: "EMAIL", status: "VERIFIED", createdAt: "2026-01-18T16:10:00Z" }],
    validationStatus: "APPROVED",
    createdAt: "2026-01-18T16:05:00Z",
  },
  {
    id: "bp-0003",
    firstName: "David",
    lastName: "Osei",
    fullName: "David Osei",
    company: false,
    companyName: "",
    email: "david.osei@heliumcompute.dev",
    phone: "+44 20 7946 0555",
    address: "14 Finsbury Square",
    city: "London",
    county: "",
    country: "GB",
    zipCode: "EC2A 1AH",
    status: "NEW",
    currency: "USD",
    balance: 0,
    accountCredit: 0,
    promotionalCredit: 20,
    currentMonth: 0,
    currentMonthUsage: 0,
    taxPayer: false,
    pricePlanConfig: { pricePlanIds: [], includePublicPricePlans: true },
    verifications: [{ type: "EMAIL", status: "VERIFIED", createdAt: "2026-07-06T08:30:00Z" }],
    validationStatus: "PENDING",
    createdAt: "2026-07-06T08:29:00Z",
  },
]

// Raw profile docs — same ids, plus the fields the summary drops.
export const rawBillingProfiles: Doc[] = billingProfiles.map((bp) => ({
  ...bp,
  bank: bp.id === "bp-0001" ? "First Republic" : bp.id === "bp-0002" ? "Deutsche Bank" : "",
  iban: bp.id === "bp-0002" ? "DE89370400440532013000" : "",
  taxConfiguration:
    bp.id === "bp-0002"
      ? { disableAutomaticTaxCalculation: false, taxRuleId: "tax-0001" }
      : { disableAutomaticTaxCalculation: false },
  projectProvisioningQuota: bp.id === "bp-0001" ? { enabled: true, limit: 10 } : { enabled: false, limit: 0 },
  overwriteSuspension: bp.id === "bp-0002",
  suspensionConfiguration:
    bp.id === "bp-0002"
      ? { enabled: true, type: "BALANCE", suspendedAt: { balance: -40 }, notifications: [{ balance: -20 }] }
      : undefined,
  identityValidationId: bp.id === "bp-0003" ? "val-0001" : undefined,
}))

// Pending identity validations (GET /admin/billing-profile/validations).
export const validations: Doc[] = [
  {
    id: "val-0001",
    billingProfileId: "bp-0003",
    status: "PENDING",
    createdAt: "2026-07-08T12:41:00Z",
    billingProfile: {
      id: "bp-0003",
      firstName: "David",
      lastName: "Osei",
      fullName: "David Osei",
      email: "david.osei@heliumcompute.dev",
      status: "NEW",
      currency: "USD",
    },
  },
]

// Suspension (dunning) processes per billing profile.
export const suspensionsByBp: Record<string, Doc[]> = {
  "bp-0002": [
    { id: "susp-0001", status: "OPEN", createdAt: "2026-06-28T02:00:00Z", updatedAt: "2026-07-05T02:00:00Z" },
    { id: "susp-0000", status: "SUCCESS", createdAt: "2026-04-14T02:00:00Z", updatedAt: "2026-04-21T02:00:00Z" },
  ],
}

// Cost dashboards (GET /admin/billing-profile/{id}/cost-info).
export const costInfoByBp: Record<string, Doc> = {
  "bp-0001": {
    dueAmount: 0,
    currentMonthCosts: 342.18,
    lastMonthCosts: 918.02,
    forecastedMonthEndCosts: 964.4,
    billingProfileCostInfo: {
      currentMonthCostsByType: { SERVER: 296.4, VOLUME: 31.18, OBJECT_STORAGE: 14.6 },
      lastMonthCostsByType: { SERVER: 802.1, VOLUME: 84.52, OBJECT_STORAGE: 31.4 },
      topResourcePrices: [
        {
          resource: { id: "cr-0001", name: "prod-api-01", type: "SERVER", createdAt: "2026-02-11T14:00:00Z" },
          currentCost: 148.2,
          forecastedCost: 417.9,
        },
        {
          resource: { id: "cr-0002", name: "prod-gpu-train-01", type: "SERVER", createdAt: "2026-05-19T08:00:00Z" },
          currentCost: 121.7,
          forecastedCost: 343.1,
        },
        {
          resource: { id: "cr-0003", name: "prod-data", type: "VOLUME", createdAt: "2026-02-11T14:05:00Z" },
          currentCost: 31.18,
          forecastedCost: 87.9,
        },
      ],
    },
  },
  "bp-0002": {
    dueAmount: 42.1,
    currentMonthCosts: 96.75,
    lastMonthCosts: 402.33,
    forecastedMonthEndCosts: 272.8,
    billingProfileCostInfo: {
      currentMonthCostsByType: { OBJECT_STORAGE: 96.75 },
      lastMonthCostsByType: { SERVER: 344.1, OBJECT_STORAGE: 58.23 },
      topResourcePrices: [
        {
          resource: { id: "cr-0006", name: "northwind-datasets", type: "BUCKET", createdAt: "2026-01-19T10:30:00Z" },
          currentCost: 96.75,
          forecastedCost: 272.8,
        },
      ],
    },
  },
  "bp-0003": {
    dueAmount: 0,
    currentMonthCosts: 0,
    lastMonthCosts: 0,
    forecastedMonthEndCosts: 0,
    billingProfileCostInfo: { currentMonthCostsByType: {}, lastMonthCostsByType: {}, topResourcePrices: [] },
  },
}

// Financial summaries (GET /admin/billing-profile/financial/{id}).
export const financialByBp: Record<string, Doc> = {
  "bp-0001": {
    currency: "USD",
    totalCredit: 250.0,
    totalPromotionalCredit: 12.5,
    currentMonthUsage: 342.18,
    totalSuccessfulBillTransactions: 8,
    totalSuccessfulAddFundsTransactions: 5,
    numberOfTransactionsLastMonth: 3,
  },
  "bp-0002": {
    currency: "EUR",
    totalCredit: 0,
    totalPromotionalCredit: 0,
    currentMonthUsage: 96.75,
    totalSuccessfulBillTransactions: 5,
    totalSuccessfulAddFundsTransactions: 2,
    numberOfTransactionsLastMonth: 1,
  },
  "bp-0003": {
    currency: "USD",
    totalCredit: 0,
    totalPromotionalCredit: 20,
    currentMonthUsage: 0,
    totalSuccessfulBillTransactions: 0,
    totalSuccessfulAddFundsTransactions: 0,
    numberOfTransactionsLastMonth: 0,
  },
}

// ─── Cloud resources ──────────────────────────────────────────────────────────

export const cloudResources: Doc[] = [
  {
    id: "cr-0001",
    type: "SERVER",
    externalId: "0a1b2c3d-1111-4a5b-8c9d-000000000001",
    region: "eu-central-1",
    projectId: "prj-0001",
    project: { id: "prj-0001", name: "acme-production" },
    data: { server: { name: "prod-api-01", status: "ACTIVE", flavor: "m1.large", addresses: ["10.0.12.4", "185.44.12.9"] } },
    info: { createdAt: "2026-02-11T14:00:00Z" },
    createdAt: "2026-02-11T14:00:00Z",
  },
  {
    id: "cr-0002",
    type: "SERVER",
    externalId: "0a1b2c3d-2222-4a5b-8c9d-000000000002",
    region: "eu-central-1",
    projectId: "prj-0001",
    project: { id: "prj-0001", name: "acme-production" },
    data: {
      server: {
        name: "prod-gpu-train-01",
        status: "ACTIVE",
        flavor: {
          id: "flv-gpu-a100-1",
          name: "g1.a100.1",
          extra_specs: { "pci_passthrough:alias": "nvidia-a100-80gb:1" },
        },
        addresses: ["10.0.12.17"],
      },
    },
    info: { createdAt: "2026-05-19T08:00:00Z" },
    createdAt: "2026-05-19T08:00:00Z",
  },
  {
    id: "cr-0003",
    type: "VOLUME",
    externalId: "0a1b2c3d-3333-4a5b-8c9d-000000000003",
    region: "eu-central-1",
    projectId: "prj-0001",
    project: { id: "prj-0001", name: "acme-production" },
    data: { volume: { name: "prod-data", status: "IN-USE", size: 500 } },
    info: { createdAt: "2026-02-11T14:05:00Z" },
    createdAt: "2026-02-11T14:05:00Z",
  },
  {
    id: "cr-0004",
    type: "SERVER",
    externalId: "0a1b2c3d-4444-4a5b-8c9d-000000000004",
    region: "eu-west-1",
    projectId: "prj-0002",
    project: { id: "prj-0002", name: "acme-staging" },
    data: { server: { name: "staging-api-01", status: "SHUTOFF", flavor: "m1.medium", addresses: ["10.1.4.8"] } },
    info: { createdAt: "2026-02-12T09:30:00Z" },
    createdAt: "2026-02-12T09:30:00Z",
  },
  {
    id: "cr-0005",
    type: "FLOATING_IP",
    externalId: "0a1b2c3d-5555-4a5b-8c9d-000000000005",
    region: "eu-central-1",
    projectId: "prj-0001",
    project: { id: "prj-0001", name: "acme-production" },
    data: { floatingIp: { name: "185.44.12.9", status: "ACTIVE" } },
    createdAt: "2026-02-11T14:02:00Z",
  },
  {
    id: "cr-0006",
    type: "BUCKET",
    externalId: "northwind-datasets",
    region: "us-east-1",
    projectId: "prj-0003",
    project: { id: "prj-0003", name: "northwind-research" },
    data: { bucketName: "northwind-datasets", usageBytes: 812_000_000_000 },
    createdAt: "2026-01-19T10:30:00Z",
  },
]

// Owner mapping for /admin/cloud-resource/user/{userId}.
export const resourcesByUser: Record<string, string[]> = {
  "usr-0001": ["cr-0001", "cr-0002", "cr-0003", "cr-0005"],
  "usr-0002": ["cr-0006"],
  "usr-0003": ["cr-0004"],
  "usr-0004": [],
}

// External (router:external) networks per cloud provider.
export const publicNetworks: Doc[] = [
  { id: "net-ext-01", name: "public" },
  { id: "net-ext-02", name: "public-ipv6" },
]

// ─── Bills ────────────────────────────────────────────────────────────────────

export const bills: Doc[] = [
  {
    id: "bill-2026-07-0001",
    billingProfileId: "bp-0001",
    billingProfile: { id: "bp-0001", fullName: "Alice Tran", firstName: "Alice", lastName: "Tran", email: "billing@acmerobotics.io" },
    status: "OPEN",
    invoiceCurrency: "USD",
    items: [
      { netAmount: 296.4, resourceType: "SERVER" },
      { netAmount: 31.18, resourceType: "VOLUME" },
      { netAmount: 14.6, resourceType: "OBJECT_STORAGE" },
    ],
    createdAt: "2026-07-01T00:10:00Z",
  },
  {
    id: "bill-2026-06-0001",
    billingProfileId: "bp-0001",
    billingProfile: { id: "bp-0001", fullName: "Alice Tran", firstName: "Alice", lastName: "Tran", email: "billing@acmerobotics.io" },
    status: "PAID",
    invoiceCurrency: "USD",
    items: [
      { netAmount: 802.1, resourceType: "SERVER" },
      { netAmount: 84.52, resourceType: "VOLUME" },
      { netAmount: 31.4, resourceType: "OBJECT_STORAGE" },
    ],
    createdAt: "2026-06-01T00:10:00Z",
  },
  {
    id: "bill-2026-06-0002",
    billingProfileId: "bp-0002",
    billingProfile: { id: "bp-0002", fullName: "Bob Kowalski", firstName: "Bob", lastName: "Kowalski", email: "finance@northwindlabs.com" },
    status: "UNPAID",
    invoiceCurrency: "EUR",
    items: [
      { netAmount: 344.1, resourceType: "SERVER" },
      { netAmount: 58.23, resourceType: "OBJECT_STORAGE" },
    ],
    createdAt: "2026-06-01T00:10:00Z",
  },
]

// Per-profile bill financial overviews (GET /admin/bill/{bpId}/billing-profile).
export const billOverviewsByBp: Record<string, Doc[]> = {
  "bp-0001": [
    {
      id: "bill-2026-07-0001",
      status: "OPEN",
      totalAmount: 342.18,
      totalInvoiceAmount: 342.18,
      unpaidAmount: 0,
      dueAt: "2026-07-15T00:00:00Z",
      currency: "USD",
      invoiceCurrency: "USD",
    },
    {
      id: "bill-2026-06-0001",
      status: "PAID",
      totalAmount: 918.02,
      totalInvoiceAmount: 918.02,
      unpaidAmount: 0,
      dueAt: "2026-06-15T00:00:00Z",
      currency: "USD",
      invoiceCurrency: "USD",
    },
  ],
  "bp-0002": [
    {
      id: "bill-2026-06-0002",
      status: "UNPAID",
      totalAmount: 402.33,
      totalInvoiceAmount: 402.33,
      unpaidAmount: 42.1,
      dueAt: "2026-06-15T00:00:00Z",
      currency: "EUR",
      invoiceCurrency: "EUR",
    },
  ],
  "bp-0003": [],
}

// ─── Transactions ─────────────────────────────────────────────────────────────

export const accountCreditTransactions: Doc[] = [
  {
    id: "act-0001",
    billingProfileId: "bp-0001",
    status: "SUCCESS",
    amount: 250,
    currency: "USD",
    externalId: "pi_3PxA1b2C3d4E5f6G",
    paymentGatewayId: "int-stripe-01",
    createdAt: "2026-06-20T17:05:00Z",
  },
  {
    id: "act-0002",
    billingProfileId: "bp-0002",
    status: "SUCCESS",
    amount: 100,
    currency: "EUR",
    externalId: "SEPA-2026-004411",
    paymentGatewayId: "int-bank-01",
    createdAt: "2026-05-30T09:12:00Z",
  },
  {
    id: "act-0003",
    billingProfileId: "bp-0001",
    status: "PENDING",
    amount: 500,
    currency: "USD",
    externalId: "pi_3PzQ9w8V7u6T5s4R",
    paymentGatewayId: "int-stripe-01",
    createdAt: "2026-07-10T21:44:00Z",
  },
]

export const collectTransactions: Doc[] = [
  {
    id: "clt-0001",
    billingProfileId: "bp-0001",
    status: "SUCCESS",
    amount: 918.02,
    currency: "USD",
    externalId: "ch_3PwB2c3D4e5F6g7H",
    paymentGatewayId: "int-stripe-01",
    createdAt: "2026-06-15T00:20:00Z",
  },
  {
    id: "clt-0002",
    billingProfileId: "bp-0002",
    status: "FAILED",
    amount: 402.33,
    currency: "EUR",
    externalId: "ch_3PvC3d4E5f6G7h8I",
    paymentGatewayId: "int-stripe-01",
    createdAt: "2026-06-15T00:22:00Z",
  },
  {
    id: "clt-0003",
    billingProfileId: "bp-0001",
    status: "PENDING",
    amount: 120.5,
    currency: "USD",
    externalId: "ch_3PxD4e5F6g7H8i9J",
    paymentGatewayId: "int-stripe-01",
    createdAt: "2026-06-16T00:20:00Z",
  },
  {
    id: "clt-0004",
    billingProfileId: "bp-0001",
    status: "PENDING",
    amount: 64,
    currency: "USD",
    paymentGatewayId: "int-stripe-01",
    createdAt: "2026-06-17T00:20:00Z",
  },
]

// ─── Account credits (spendable balances per profile) ─────────────────────────

export const accountCreditsByBp: Record<string, Doc[]> = {
  "bp-0001": [
    { id: "acr-0001", billingProfileId: "bp-0001", amount: 250, initialAmount: 250, currency: "USD", createdAt: "2026-06-20T17:06:00Z" },
    { id: "acr-0002", billingProfileId: "bp-0001", amount: 0, initialAmount: 100, currency: "USD", createdAt: "2026-03-08T10:00:00Z" },
  ],
  "bp-0002": [
    { id: "acr-0003", billingProfileId: "bp-0002", amount: 0, initialAmount: 100, currency: "EUR", createdAt: "2026-05-30T09:13:00Z" },
  ],
  "bp-0003": [],
}

// ─── Promotional credits ──────────────────────────────────────────────────────

export const promoCreditsByBp: Record<string, Doc[]> = {
  "bp-0001": [
    {
      id: "pcr-0001",
      billingProfileId: "bp-0001",
      code: "WELCOME25",
      initialAmount: 25,
      remainingAmount: 12.5,
      expirationDate: "2026-08-15T00:00:00Z",
      createdAt: "2026-06-16T12:00:00Z",
    },
  ],
  "bp-0002": [],
  "bp-0003": [
    {
      id: "pcr-0002",
      billingProfileId: "bp-0003",
      code: "SIGNUP20",
      initialAmount: 20,
      remainingAmount: 20,
      expirationDate: "2026-08-05T00:00:00Z",
      createdAt: "2026-07-06T08:30:00Z",
    },
  ],
}

// ─── Bank transfers (pending deposits awaiting operator review) ───────────────

export const bankTransfers: Doc[] = [
  {
    id: "bt-0001",
    integrationId: "int-bank-01",
    billingProfileId: "bp-0002",
    amount: 150,
    currency: "EUR",
    referenceNumber: "PROXCLOUD-2026-07-0031",
    status: "PENDING",
    createdAt: "2026-07-09T11:20:00Z",
  },
  {
    id: "bt-0002",
    integrationId: "int-bank-01",
    billingProfileId: "bp-0002",
    amount: 100,
    currency: "EUR",
    referenceNumber: "PROXCLOUD-2026-05-0018",
    status: "SUCCESS",
    createdAt: "2026-05-30T08:44:00Z",
  },
]
