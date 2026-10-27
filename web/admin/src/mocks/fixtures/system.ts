// System-area seed data: cloud providers, catalog, price plans, taxes,
// savings plans, promotions, integrations, configuration, roles, keys,
// templates and menu items.

type Doc = Record<string, any>

// ─── Cloud providers (external services) ──────────────────────────────────────

export const services: Doc[] = [
  {
    id: "svc-openstack-01",
    name: "Helium OpenStack",
    type: "openstack",
    status: "ACTIVE",
    config: {
      provider: "openstack",
      identityUrl: "https://keystone.helium.example.com:5000/v3",
      regions: {
        "eu-central-1": { displayName: "EU Central (Frankfurt)" },
        "eu-west-1": { displayName: "EU West (Amsterdam)" },
      },
      services: {
        compute: { "eu-central-1": true, "eu-west-1": true },
        network: { "eu-central-1": true, "eu-west-1": true },
        volume: { "eu-central-1": true, "eu-west-1": true },
        image: { "eu-central-1": true, "eu-west-1": true },
        "load-balancer": { "eu-central-1": true, "eu-west-1": false },
        share: { "eu-central-1": true, "eu-west-1": false },
      },
      auth: { username: "proxcloud-admin", projectName: "admin", domainName: "Default" },
      features: {
        components: {
          "volume-backup": true,
          "volume-snapshot": true,
          "image-download": false,
          "instance-metrics": true,
        },
        volumeTypes: {
          "eu-central-1": [
            { name: "ssd", displayName: "NVMe SSD", enabled: true },
            { name: "hdd", displayName: "Capacity HDD", enabled: false },
          ],
          "eu-west-1": [{ name: "ssd", displayName: "NVMe SSD", enabled: true }],
        },
        consoleType: "NOVNC",
      },
      provisioning: {
        quota: { instances: 20, cores: 64, ram: 262144, volumes: 40, gigabytes: 4000, floatingips: 10 },
      },
      metrics: { source: "gnocchi" },
      availabilityZones: [
        { name: "az-1", displayName: "Zone 1", enabled: true },
        { name: "az-2", displayName: "Zone 2", enabled: true },
        { name: "az-maintenance", displayName: "", enabled: false },
      ],
    },
  },
  {
    id: "svc-ceph-01",
    name: "Ceph Object Store",
    type: "ceph-s3",
    status: "ACTIVE",
    config: {
      provider: "ceph-s3",
      s3Endpoint: "https://s3.helium.example.com",
      adminApiUrl: "https://rgw-admin.helium.example.com",
      s3WebsiteEndpoint: "https://s3-website.helium.example.com",
      region: "us-east-1",
      uidPrefix: "proxcloud-",
      defaultQuotaGiB: 500,
      regions: { "us-east-1": { displayName: "US East (object storage)" } },
      services: { "object-storage": { "us-east-1": true } },
    },
  },
  {
    id: "svc-dbaas-01",
    name: "Managed Databases AZ1",
    type: "CLOUD",
    status: "ACTIVE",
    config: {
      provider: "dbaas",
      region: "az1",
      regions: { az1: { name: "az1", country: "", displayName: "az1" } },
      services: { database: { az1: true } },
      argocd: {
        namespace: "argocd",
        project: "proxcloud-dbaas",
        chartRepo: "ghcr.io/mutex-solutions/charts",
        chartName: "database-cluster",
        chartVersion: "0.1.0",
      },
      database: {
        osServiceId: "svc-openstack-01",
        osProjectId: "db44a5000000000000000000000000aa",
        memberSubnetId: "5f0e1d2c-3b4a-5968-8776-655443322110",
        storageClasses: ["csi-cinder"],
        limits: { maxCpu: 32, maxMemoryGiB: 128, maxStorageGiB: 2048 },
        engines: {
          postgresql: { versions: ["16", "17", "18"], default: "17", replicas: [1, 3] },
          mysql: { versions: ["8.4"], default: "8.4", replicas: [1, 3] },
          mariadb: { versions: ["11.4"], default: "11.4", replicas: [1, 3] },
          valkey: { versions: ["8"], default: "8", replicas: [1, 3], beta: true },
          ferretdb: { versions: ["2"], default: "2", replicas: [1, 3] },
        },
      },
    },
  },
]

// GPU placement capacity per service (GET /admin/service/{id}/gpu-info).
export const gpuInfoByService: Record<string, Doc[]> = {
  "svc-openstack-01": [
    {
      region: "eu-central-1",
      gpus: [
        { name: "nvidia-a100-80gb", total: 16, inUse: 9 },
        { name: "nvidia-l40s", total: 24, inUse: 6 },
      ],
    },
    {
      region: "eu-west-1",
      gpus: [{ name: "nvidia-a100-80gb", total: 8, inUse: 8 }],
    },
  ],
  "svc-ceph-01": [],
}

export const unpricedFlavorsByService: Record<string, Doc[]> = {
  "svc-openstack-01": [
    { region: "eu-west-1", id: "flv-9001", name: "g1.h200.8", gpuModel: "NVIDIA-H200", gpuCount: 8, reason: "no gpu rule" },
  ],
  "svc-ceph-01": [],
}

export const volumeTypesByService: Record<string, Doc[]> = {
  "svc-openstack-01": [
    { region: "eu-central-1", volumeTypes: ["ssd", "hdd"] },
    { region: "eu-west-1", volumeTypes: ["ssd"] },
  ],
}

// serviceType -> region -> zones (GET /admin/service/{id}/availability-zones).
export const availabilityZonesByService: Record<string, Doc> = {
  "svc-openstack-01": {
    compute: { "eu-central-1": ["az-1", "az-2"], "eu-west-1": ["az-1"] },
    volume: { "eu-central-1": ["az-1"], "eu-west-1": ["az-1"] },
  },
}

export const shareProtocolsByService: Record<string, Doc[]> = {
  "svc-openstack-01": [
    { name: "NFS", displayName: "NFS", enabled: true },
    { name: "CEPHFS", displayName: "CephFS", enabled: false },
  ],
}

// POST /admin/service/openstack/auth — connection test with the stored creds.
export const openstackAuthResult: Doc = {
  services: [
    { type: "compute", name: "nova" },
    { type: "network", name: "neutron" },
    { type: "volumev3", name: "cinder" },
    { type: "image", name: "glance" },
    { type: "placement", name: "placement" },
  ],
  projects: [
    { id: "8c1f6a2d94e34b7c9f1d3e5a7b2c4d6e", name: "acme-production" },
    { id: "51b2e9c07aa14f0d8be6c3d2a1f09e88", name: "acme-staging" },
    { id: "adm1n000000000000000000000000000", name: "admin" },
  ],
  domains: [{ id: "default", name: "Default" }],
  roles: ["admin", "reader"],
  selectedProjectName: "admin",
}

// GET /admin/project-import/{serviceId} — importable Keystone projects.
export const importProjectsByService: Record<string, Doc[]> = {
  "svc-openstack-01": [
    { project: { id: "8c1f6a2d94e34b7c9f1d3e5a7b2c4d6e", name: "acme-production" }, proxcloudProjectId: "prj-0001", users: [] },
    { project: { id: "51b2e9c07aa14f0d8be6c3d2a1f09e88", name: "acme-staging" }, proxcloudProjectId: "prj-0002", users: [] },
    { project: { id: "3fa4b1c8d2e94f60a7b5c3d1e9f08a77", name: "legacy-analytics" }, users: [] },
  ],
}

// ─── Catalog ──────────────────────────────────────────────────────────────────

export const flavorCategories: Doc[] = [
  {
    id: "fc-0001",
    name: "General purpose",
    description: "Balanced vCPU and memory for web services and APIs.",
    orderNumber: 1,
    bareMetal: false,
    kubernetesFlavorCategory: false,
    flavors: [{ flavorName: "m1.small" }, { flavorName: "m1.medium" }, { flavorName: "m1.large" }],
    flavorAttributes: [],
  },
  {
    id: "fc-0002",
    name: "GPU accelerated",
    description: "A100 and L40S instances for training and inference.",
    orderNumber: 2,
    bareMetal: false,
    kubernetesFlavorCategory: false,
    flavors: [{ flavorName: "g1.a100.1" }, { flavorName: "g1.l40s.1" }],
    flavorAttributes: [],
  },
]

export const liveFlavors: Doc[] = [
  { id: "flv-0001", name: "m1.small", vcpus: 2, ram: 4096, disk: 40 },
  { id: "flv-0002", name: "m1.medium", vcpus: 4, ram: 8192, disk: 80 },
  { id: "flv-0003", name: "m1.large", vcpus: 8, ram: 16384, disk: 160 },
  { id: "flv-0004", name: "g1.a100.1", vcpus: 12, ram: 98304, disk: 200 },
  { id: "flv-0005", name: "g1.l40s.1", vcpus: 8, ram: 65536, disk: 200 },
]

export const imageCategories: Doc[] = [
  { id: "ic-0001", name: "Linux distributions", description: "Base operating system images.", bareMetal: false },
  { id: "ic-0002", name: "Applications", description: "Preconfigured application stacks.", bareMetal: false },
]

export const imageGroups: Doc[] = [
  {
    id: "ig-0001",
    name: "Ubuntu",
    enabled: true,
    orderNumber: 1,
    categoryId: "ic-0001",
    description: "Ubuntu LTS releases.",
    groupLogoUrl: "",
    labels: [],
    images: [
      { name: "ubuntu-24.04-lts", version: "24.04", orderNumber: 0 },
      { name: "ubuntu-22.04-lts", version: "22.04", orderNumber: 1 },
    ],
  },
  {
    id: "ig-0002",
    name: "Debian",
    enabled: true,
    orderNumber: 2,
    categoryId: "ic-0001",
    description: "Debian stable.",
    groupLogoUrl: "",
    labels: [],
    images: [{ name: "debian-13", version: "13", orderNumber: 0 }],
  },
  {
    id: "ig-0003",
    name: "Docker host",
    enabled: false,
    orderNumber: 1,
    categoryId: "ic-0002",
    description: "Ubuntu with Docker Engine preinstalled.",
    groupLogoUrl: "",
    labels: [],
    images: [{ name: "docker-host-24.04", version: "1.2", orderNumber: 0 }],
  },
]

export const osImages: Doc[] = [
  {
    serviceId: "svc-openstack-01",
    serviceName: "Helium OpenStack",
    region: "eu-central-1",
    regionDisplayName: "EU Central (Frankfurt)",
    images: [
      { id: "img-0001", name: "ubuntu-24.04-lts", status: "ACTIVE", visibility: "public" },
      { id: "img-0002", name: "ubuntu-22.04-lts", status: "ACTIVE", visibility: "public" },
      { id: "img-0003", name: "debian-13", status: "ACTIVE", visibility: "public" },
      { id: "img-0004", name: "rocky-10", status: "ACTIVE", visibility: "public" },
      // private on purpose — exercises the "not public" binding warning.
      { id: "img-0005", name: "docker-host-24.04", status: "ACTIVE", visibility: "private" },
    ],
  },
  {
    serviceId: "svc-openstack-01",
    serviceName: "Helium OpenStack",
    region: "eu-west-1",
    regionDisplayName: "EU West (Amsterdam)",
    images: [
      { id: "img-0011", name: "ubuntu-24.04-lts", status: "ACTIVE", visibility: "public" },
      { id: "img-0012", name: "debian-13", status: "ACTIVE", visibility: "public" },
    ],
  },
]

export const metadataOptions: Doc[] = [
  {
    id: "meta-0001",
    key: "gpu_driver",
    displayName: "GPU driver",
    description: "NVIDIA driver branch installed on first boot.",
    type: "PREDEFINED_VALUES",
    options: [
      { value: "nvidia-550", displayName: "NVIDIA 550 (production)", enabled: true },
      { value: "nvidia-560", displayName: "NVIDIA 560 (new feature)", enabled: true },
    ],
    serviceIds: ["svc-openstack-01"],
    regions: ["eu-central-1", "eu-west-1"],
    userEditable: true,
    showInline: true,
    enabled: true,
  },
  {
    id: "meta-0002",
    key: "backup_retention_days",
    displayName: "Backup retention (days)",
    description: "How long automated volume backups are kept.",
    type: "NUMERIC_RANGE",
    numericRange: { min: 1, max: 90, unit: "days" },
    serviceIds: ["svc-openstack-01"],
    regions: [],
    userEditable: false,
    showInline: false,
    enabled: false,
  },
]

// ─── Custom menu ──────────────────────────────────────────────────────────────

export const menuItems: Doc[] = [
  { id: "menu-0001", displayName: "Status page", url: "https://status.example.com", icon: "activity", renderMode: "OPEN_NEW_WINDOW", order: 1 },
  { id: "menu-0002", displayName: "Grafana", url: "https://grafana.example.com/d/project?var-project={projectId}", icon: "bar-chart", renderMode: "IFRAME", order: 2 },
]

export const menuPlaceholders: Record<string, string[]> = {
  url: ["{userEmail}", "{userSub}", "{projectId}", "{organizationId}"],
}

// ─── Templates ────────────────────────────────────────────────────────────────

export const messageTemplates: Doc[] = [
  {
    id: "mt-0001",
    key: "billing.invoice-created",
    category: "Billing",
    messageTitle: "Your ProxCloud invoice {invoiceNumber} is ready",
    messageBody: "Hello {firstName},\n\nYour invoice {invoiceNumber} for {amount} {currency} was issued on {issueDate}. You can download it from the billing section of the console.\n\nThe ProxCloud team",
    disabled: false,
    systemTemplate: true,
  },
  {
    id: "mt-0002",
    key: "billing.suspension-warning",
    category: "Billing",
    messageTitle: "Action needed: your account balance is {balance} {currency}",
    messageBody: "Hello {firstName},\n\nYour balance dropped below the configured threshold. Please add funds to avoid suspension of your projects.\n\nThe ProxCloud team",
    disabled: false,
    systemTemplate: true,
  },
  {
    id: "mt-0003",
    key: "account.welcome",
    category: "Account",
    messageTitle: "Welcome to ProxCloud Cloud",
    messageBody: "Hello {firstName},\n\nYour account is ready. Launch your first server from the console.\n\nThe ProxCloud team",
    disabled: true,
    systemTemplate: false,
  },
]

export const messagePlaceholders: Record<string, Array<{ key: string; description: string }>> = {
  "billing.invoice-created": [
    { key: "{firstName}", description: "Recipient first name" },
    { key: "{invoiceNumber}", description: "Invoice number" },
    { key: "{amount}", description: "Invoice total" },
    { key: "{currency}", description: "Invoice currency" },
    { key: "{issueDate}", description: "Issue date" },
  ],
  "billing.suspension-warning": [
    { key: "{firstName}", description: "Recipient first name" },
    { key: "{balance}", description: "Current balance" },
    { key: "{currency}", description: "Profile currency" },
  ],
  "account.welcome": [{ key: "{firstName}", description: "Recipient first name" }],
}

export const pdfTemplates: Doc[] = [
  {
    id: "pdf-0001",
    name: "Invoice",
    description: "Customer invoice statement.",
    type: "INVOICE",
    content: "<html><body><h1>Invoice {{.InvoiceNumber}}</h1><p>Billed to {{.CustomerName}} — total {{.Total}} {{.Currency}}.</p></body></html>",
  },
  {
    id: "pdf-0002",
    name: "Payment receipt",
    description: "Receipt for a successful deposit or charge.",
    type: "RECEIPT",
    content: "<html><body><h1>Receipt {{.ReceiptNumber}}</h1><p>Received {{.Amount}} {{.Currency}} on {{.Date}}.</p></body></html>",
  },
]

// ─── Integrations ─────────────────────────────────────────────────────────────

export const integrations: Doc[] = [
  {
    id: "int-stripe-01",
    name: "Stripe",
    description: "Card payments",
    thirdParty: "Stripe",
    config: { publicKey: "pk_test_mock", minDeposit: 10, sandbox: false, scan: true, callback: true },
    createdAt: "2025-11-01T12:00:00Z",
  },
  {
    id: "int-smtp-01",
    name: "Platform mail",
    description: "Transactional email",
    thirdParty: "SMTP",
    config: { domain: "smtp.eu.mailgun.org", port: 587, username: "postmaster@mg.example.com", fromName: "ProxCloud Cloud", fromEmail: "no-reply@example.com", starttls: true, noAuth: false },
    createdAt: "2025-11-01T12:05:00Z",
  },
  {
    id: "int-bank-01",
    name: "SEPA bank transfer",
    description: "Manual bank transfer deposits",
    thirdParty: "BankTransfer",
    config: { minDeposit: 50, bankTransferInstructions: "Example Cloud GmbH\nIBAN DE02120300000000202051\nBIC BYLADEM1001\nReference: your deposit reference number" },
    createdAt: "2026-02-01T09:00:00Z",
  },
]

export const integrationStats: Doc[] = [
  { name: "Stripe", categories: ["Payment"], installed: true },
  { name: "Stripe", categories: ["Invoice"], installed: true },
  { name: "SMTP", categories: ["Mail"], installed: true },
  { name: "BankTransfer", categories: ["Payment"], installed: true },
]

// ─── Platform + billing configuration ─────────────────────────────────────────

export const platformConfiguration: Doc = {
  id: "platcfg-0001",
  name: "ProxCloud Cloud",
  language: "en",
  branding: { name: "ProxCloud", color: "#FF5C00", logo: "", faviconUrl: "" },
  dateConfiguration: { dateFormat: "DD/MM/YYYY" },
  projectProvisioningQuota: { enabled: true, limit: 5 },
  organizationProvisioningQuota: { enabled: false, limit: 0 },
  regions: [
    { serviceId: "svc-openstack-01", region: "eu-central-1", order: 1 },
    { serviceId: "svc-openstack-01", region: "eu-west-1", order: 2 },
    { serviceId: "svc-ceph-01", region: "us-east-1", order: 3 },
  ],
}

export const billingConfiguration: Doc = {
  id: "billcfg-0001",
  name: "Example Cloud Billing",
  address: { country: "DE", city: "Berlin", address: "Torstrasse 44" },
  company: { vatId: "DE318894123", businessName: "Example Cloud GmbH" },
  baseCurrency: "USD",
  mailGatewayId: "int-smtp-01",
  invoiceGatewayId: "int-stripe-01",
  settings: { timeUnitLimits: { minute: 60, hour: 24, month: 1 } },
  defaultConfiguration: true,
  promotionCodesEnabled: true,
  provisioningSettings: { promotionals: [{ amount: 20, daysValidity: 30 }] },
  autoActivationFlow: {
    autoActivationEnabled: true,
    kyc: "DISABLED",
    paymentMethod: "REQUIRED",
    paymentMethodCard: "ALTERNATIVE",
    paymentMethodDeposit: "ALTERNATIVE",
    minimumDepositAmount: 10,
    billingProfileValidation: "ALTERNATIVE",
  },
  suspensionConfiguration: {
    enabled: true,
    type: "BALANCE",
    suspendedAt: { balance: -100 },
    notifications: [{ balance: -25 }, { balance: -50 }],
  },
  savingsContractNotificationConfig: { sendExpiryNotification: true, reminderDaysBeforeExpiry: [30, 7] },
}

export const currencies: Doc[] = [
  { country: "United States", currency_name: "US Dollar", currency_code: "USD", numeric_code: "840" },
  { country: "Eurozone", currency_name: "Euro", currency_code: "EUR", numeric_code: "978" },
  { country: "United Kingdom", currency_name: "Pound Sterling", currency_code: "GBP", numeric_code: "826" },
  { country: "Switzerland", currency_name: "Swiss Franc", currency_code: "CHF", numeric_code: "756" },
  { country: "Japan", currency_name: "Yen", currency_code: "JPY", numeric_code: "392" },
]

export const countries: Doc[] = [
  { name: "Germany", cca2: "DE", cca3: "DEU", ccn3: 276 },
  { name: "United States", cca2: "US", cca3: "USA", ccn3: 840 },
  { name: "United Kingdom", cca2: "GB", cca3: "GBR", ccn3: 826 },
  { name: "France", cca2: "FR", cca3: "FRA", ccn3: 250 },
  { name: "Netherlands", cca2: "NL", cca3: "NLD", ccn3: 528 },
  { name: "Switzerland", cca2: "CH", cca3: "CHE", ccn3: 756 },
]

// ─── Price plans ──────────────────────────────────────────────────────────────

export const pricePlans: Doc[] = [
  {
    id: "pp-0001",
    name: "Public standard",
    enabled: true,
    accessMode: "PUBLIC",
    serviceProviders: [{ serviceId: "svc-openstack-01" }, { serviceId: "svc-ceph-01" }],
    createdAt: "2025-11-01T13:00:00Z",
  },
  {
    id: "pp-0002",
    name: "Enterprise reserved",
    enabled: true,
    accessMode: "PRIVATE",
    serviceProviders: [{ serviceId: "svc-openstack-01" }],
    createdAt: "2026-01-10T10:00:00Z",
  },
]

export const resourceTypes: Doc[] = [
  {
    resourceType: "SERVER",
    attributes: [
      { type: "number", name: "vcpus" },
      { type: "number", name: "ram" },
      { type: "number", name: "gpu" },
      { type: "number", name: "traffic", isUsage: true },
    ],
  },
  { resourceType: "VOLUME", attributes: [{ type: "number", name: "size" }] },
  { resourceType: "OBJECT_STORAGE", attributes: [{ type: "number", name: "storage", isUsage: true }] },
  { resourceType: "FLOATING_IP", attributes: [{ type: "number", name: "count" }] },
]

export const pricePlanRules: Doc[] = [
  {
    id: "rule-0001",
    pricePlanId: "pp-0001",
    name: "Compute vCPU + RAM",
    resourceType: "SERVER",
    timeUnit: "hour",
    applyMethod: "SUM",
    prices: [
      { attributeName: "vcpus", tiers: [{ from: 0, to: 0, value: 0.012 }] },
      { attributeName: "ram", tiers: [{ from: 0, to: 0, value: 0.0000045 }] },
    ],
    filters: null,
    modifiers: null,
  },
  {
    id: "rule-0002",
    pricePlanId: "pp-0001",
    name: "GPU A100",
    resourceType: "SERVER",
    timeUnit: "hour",
    applyMethod: "SUM",
    prices: [{ attributeName: "gpu", tiers: [{ from: 0, to: 0, value: 1.85 }] }],
    filters: { gpuModel: "NVIDIA-A100-80GB" },
    modifiers: null,
  },
  {
    id: "rule-0003",
    pricePlanId: "pp-0001",
    name: "Block storage",
    resourceType: "VOLUME",
    timeUnit: "month",
    applyMethod: "SUM",
    prices: [{ attributeName: "size", tiers: [{ from: 0, to: 0, value: 0.08 }] }],
    filters: null,
    modifiers: null,
  },
  {
    id: "rule-0004",
    pricePlanId: "pp-0002",
    name: "Reserved compute",
    resourceType: "SERVER",
    timeUnit: "hour",
    applyMethod: "SUM",
    prices: [{ attributeName: "vcpus", tiers: [{ from: 0, to: 0, value: 0.009 }] }],
    filters: null,
    modifiers: null,
  },
]

export const adjustmentRules: Doc[] = [
  {
    id: "adj-0001",
    name: "Volume discount",
    enabled: true,
    description: "10 percent off once monthly spend passes 1000.",
    pricePlanId: "pp-0001",
    targets: null,
    tiers: [{ startAmount: 1000, modifier: { operator: "SUBTRACT", asPercentage: true, value: 10 } }],
  },
]

export const ruleUsage: Record<string, Doc> = {
  "rule-0001": { openBillsCount: 2, totalAppliedAmount: 431.2 },
  "rule-0002": { openBillsCount: 1, totalAppliedAmount: 121.7 },
  "rule-0003": { openBillsCount: 2, totalAppliedAmount: 31.18 },
  "rule-0004": { openBillsCount: 0, totalAppliedAmount: 0 },
}

export const adjRuleUsage: Record<string, Doc> = {
  "adj-0001": { openBillsCount: 0, totalAdjustmentsAmount: 0 },
}

// ─── Taxes ────────────────────────────────────────────────────────────────────

export const taxes: Doc[] = [
  { id: "tax-0001", name: "Germany VAT", country: "DE", level: "ALL", accessMode: "PUBLIC", rateLevels: [{ level: 1, percentage: 19 }] },
  { id: "tax-0002", name: "UK VAT", country: "GB", level: "ALL", accessMode: "PUBLIC", rateLevels: [{ level: 1, percentage: 20 }] },
  { id: "tax-0003", name: "US B2B exempt", country: "US", level: "BUSINESS_ONLY", accessMode: "PUBLIC", rateLevels: [{ level: 1, percentage: 0 }] },
]

// ─── Savings plans ────────────────────────────────────────────────────────────

export const savingsPlans: Doc[] = [
  {
    id: "sp-0001",
    name: "Compute savings 12m",
    available: true,
    description: "Commit to monthly compute spend for 12 months and save up to 18 percent.",
    accessMode: "PUBLIC",
    targets: [{ resourceType: "SERVER" }],
    savingSchedule: [
      {
        durationMonths: 12,
        maxAmount: 10000,
        noUpfrontTiers: [
          { startAmount: 100, discount: 5 },
          { startAmount: 1000, discount: 12 },
        ],
        upfrontTiers: [
          { startAmount: 100, discount: 8 },
          { startAmount: 1000, discount: 18 },
        ],
      },
    ],
  },
  {
    id: "sp-0002",
    name: "Storage savings 24m",
    available: false,
    description: "Long-term object storage commitment.",
    accessMode: "PRIVATE",
    targets: [{ resourceType: "OBJECT_STORAGE" }],
    savingSchedule: [
      {
        durationMonths: 24,
        maxAmount: 5000,
        noUpfrontTiers: [{ startAmount: 50, discount: 10 }],
        upfrontTiers: [{ startAmount: 50, discount: 15 }],
      },
    ],
  },
]

export const savingsContracts: Doc[] = [
  {
    id: "sc-0001",
    billingProfileId: "bp-0001",
    savingsPlanId: "sp-0001",
    savingsPlanName: "Compute savings 12m",
    status: "ACTIVE",
    durationMonths: 12,
    monthlyCommittedAmount: 500,
    startDate: "2026-03-01T00:00:00Z",
    endDate: "2027-03-01T00:00:00Z",
    billingProfile: { email: "billing@acmerobotics.io", fullName: "Alice Tran" },
  },
]

// ─── Promotion codes ──────────────────────────────────────────────────────────

export const promotionCodes: Doc[] = [
  {
    id: "code-0001",
    code: "WELCOME25",
    description: "Sign-up promotion, 25 USD of credit.",
    amount: 25,
    status: "ACTIVE",
    validFrom: "2026-01-01T00:00:00Z",
    validUntil: "2026-12-31T23:59:59Z",
    creditValidityDuration: "P60D",
    targetOrganizationIds: [],
  },
  {
    id: "code-0002",
    code: "ACME50",
    description: "Retention credit for Acme Robotics.",
    amount: 50,
    status: "EXPIRED",
    validFrom: "2026-02-01T00:00:00Z",
    validUntil: "2026-04-30T23:59:59Z",
    creditValidityDuration: "P30D",
    targetOrganizationIds: ["org-0001"],
  },
]

// ─── Admin roles + permissions ────────────────────────────────────────────────

export const availablePermissions: Doc[] = [
  { key: "admin:user:read", description: "List and inspect platform users" },
  { key: "admin:user:manage", description: "Create, delete and impersonate users" },
  { key: "admin:project:read", description: "List and inspect projects" },
  { key: "admin:project:manage", description: "Enable, disable, sync and delete projects" },
  { key: "admin:organization:read", description: "List and inspect organizations" },
  { key: "admin:organization:manage", description: "Edit organizations and memberships" },
  { key: "admin:billing_profile:read", description: "List and inspect billing profiles" },
  { key: "admin:billing_profile:manage", description: "Activate, suspend and edit billing profiles" },
  { key: "admin:bill:read", description: "List bills and download statements" },
  { key: "admin:transaction:read", description: "List transactions and bank transfers" },
  { key: "admin:transaction:manage", description: "Approve transfers, refund and re-sync transactions" },
  { key: "admin:cloud_resource:read", description: "List cached cloud resources" },
  { key: "admin:cloud_resource:manage", description: "Sync and delete cloud resources" },
  { key: "admin:platform_config:read", description: "Read the platform configuration" },
  { key: "admin:platform_config:manage", description: "Edit branding, quotas and regions" },
  { key: "admin:billing_config:read", description: "Read the billing configuration" },
  { key: "admin:billing_config:manage", description: "Edit the billing configuration" },
  { key: "admin:price_plan:read", description: "List price plans and rules" },
  { key: "admin:price_plan:manage", description: "Edit price plans, rules and adjustments" },
  { key: "admin:tax:read", description: "List tax rates" },
  { key: "admin:tax:manage", description: "Edit tax rates" },
  { key: "admin:savings_plan:read", description: "List savings plans and contracts" },
  { key: "admin:savings_plan:manage", description: "Edit savings plans and contracts" },
  { key: "admin:promotional_credit:manage", description: "Manage promotion codes and promotional credits" },
  { key: "admin:menu:manage", description: "Manage custom client-menu entries" },
  { key: "admin:message_template:read", description: "List message and PDF templates" },
  { key: "admin:message_template:manage", description: "Edit message and PDF templates" },
  { key: "admin:flavor_category:manage", description: "Manage the instance catalog" },
  { key: "admin:service:read", description: "List cloud providers" },
  { key: "admin:service:manage", description: "Connect and configure cloud providers" },
  { key: "admin:integration:read", description: "List integrations" },
  { key: "admin:integration:manage", description: "Install and configure integrations" },
  { key: "admin:permission:read", description: "List admin roles" },
  { key: "admin:permission:manage", description: "Edit admin roles" },
  { key: "admin:hmac_key:manage", description: "Manage machine API keys" },
  { key: "admin:audit:read", description: "Read the audit log" },
]

const allPermissionKeys = availablePermissions.map((p) => p.key as string)

export const adminRoles: Doc[] = [
  {
    id: "role-0001",
    name: "SUPER_ADMIN",
    description: "Full, unrestricted access to every admin capability.",
    permissions: ["admin:*"],
    expandedPermissions: allPermissionKeys,
    builtIn: true,
  },
  {
    id: "role-0002",
    name: "ADMIN",
    description: "Day-to-day platform administration without permission management.",
    permissions: allPermissionKeys.filter((k) => !k.startsWith("admin:permission")),
    expandedPermissions: allPermissionKeys.filter((k) => !k.startsWith("admin:permission")),
    builtIn: true,
  },
  {
    id: "role-0003",
    name: "SUPPORT",
    description: "Read access to client data for support engineers.",
    permissions: [
      "admin:user:read",
      "admin:project:read",
      "admin:organization:read",
      "admin:billing_profile:read",
      "admin:bill:read",
      "admin:transaction:read",
      "admin:cloud_resource:read",
    ],
    expandedPermissions: [
      "admin:user:read",
      "admin:project:read",
      "admin:organization:read",
      "admin:billing_profile:read",
      "admin:bill:read",
      "admin:transaction:read",
      "admin:cloud_resource:read",
    ],
    builtIn: true,
  },
  {
    id: "role-0004",
    name: "BILLING_ADMIN",
    description: "Manage billing configuration, price plans, taxes and promotions.",
    permissions: [
      "admin:billing_profile:*",
      "admin:bill:read",
      "admin:transaction:*",
      "admin:billing_config:*",
      "admin:price_plan:*",
      "admin:tax:*",
      "admin:savings_plan:*",
      "admin:promotional_credit:manage",
    ],
    expandedPermissions: allPermissionKeys.filter(
      (k) =>
        k.startsWith("admin:billing") ||
        k.startsWith("admin:bill:") ||
        k.startsWith("admin:transaction") ||
        k.startsWith("admin:price_plan") ||
        k.startsWith("admin:tax") ||
        k.startsWith("admin:savings_plan") ||
        k.startsWith("admin:promotional_credit"),
    ),
    builtIn: true,
  },
  {
    id: "role-0005",
    name: "VIEWER",
    description: "Read-only access across the console.",
    permissions: allPermissionKeys.filter((k) => k.endsWith(":read")),
    expandedPermissions: allPermissionKeys.filter((k) => k.endsWith(":read")),
    builtIn: true,
  },
  {
    id: "role-0006",
    name: "NOC_OPERATOR",
    description: "Custom role for the network operations rotation.",
    permissions: ["admin:cloud_resource:*", "admin:service:read", "admin:project:read", "admin:audit:read"],
    expandedPermissions: [
      "admin:cloud_resource:read",
      "admin:cloud_resource:manage",
      "admin:service:read",
      "admin:project:read",
      "admin:audit:read",
    ],
    builtIn: false,
  },
]

// ─── HMAC (SigV4) API keys ────────────────────────────────────────────────────

export const hmacKeys: Doc[] = [
  { id: "AKSTRA7Q2M4E8R1T6Y9U", description: "Billing exporter (finance cron)", createdAt: "2026-03-14T09:00:00Z", updatedAt: "2026-03-14T09:00:00Z" },
  { id: "AKSTRA3W5X8C1V4B7N2M", description: "Terraform provider - platform CI", createdAt: "2026-05-02T16:30:00Z", updatedAt: "2026-05-02T16:30:00Z" },
]
