// Core DTO shapes from the ProxCloud API (optional fields may
// be absent — keep everything optional unless proven always-present).

export type Project = {
  id: string
  name: string
  status?: string
  organizationId?: string
  billingProfileId?: string
  memberships?: Array<{ sub?: string; roles?: string[] }>
  services?: Array<{ serviceId?: string; config?: Record<string, unknown> }>
  // false/absent = the client gets no external-network picker (the server auto-picks the pool for
  // public NICs); true = the client chooses.
  publicNetworksVisible?: boolean
  // false/absent = the dashboard does not show the region GPU capacity; true = it does.
  gpuCapacityVisible?: boolean
}

// GET /project/{id}/gpu-capacity — the region's cluster GPU capacity (available/total per model),
// only populated when the operator enabled it for the project.
export type GpuCapacityUsage = {
  visible: boolean
  region?: string
  capacity: Array<{ model: string; available: number; total: number }>
  warnings: string[]
}

export type Organization = {
  id: string
  name?: string
  billingProfileId?: string
  members?: Array<{ sub?: string; roles?: string[]; email?: string }>
}

export type BillingSummary = {
  id?: string
  status?: string
  currency?: string
  balance?: number
  accountCredit?: number
  promotionalCredit?: number
  currentMonthUsage?: number
  defaultCardId?: string
  hasBillingDetails?: boolean
  fullName?: string
  firstName?: string
  lastName?: string
  phone?: string
  address?: string
  city?: string
  county?: string
  country?: string
  zipCode?: string
  company?: boolean
  companyName?: string
  vatCode?: string
  // Extra invoice/billing e-mail recipients (every billing mail goes to `email` + these).
  contacts?: BillingContact[]
  // Payment settings: PREPAID (default; bills settle from credit) | POSTPAID (every issued bill
  // is charged to the saved card), and the preferred gateway preselected for deposits / new cards.
  paymentMode?: "PREPAID" | "POSTPAID"
  paymentGatewayId?: string
  [k: string]: unknown
}

export type BillingContact = { name?: string; email?: string }

// Payment gateway as listed by GET /payment/{bp}/gateway (no secrets): Stripe carries the
// Elements publicKey, Airwallex the SDK env ("demo" | "prod"), BankTransfer the operator-entered
// transfer instructions.
export type Gateway = {
  id?: string
  name?: string
  thirdParty?: string
  addCard?: boolean
  addFunds?: boolean
  minDeposit?: number | string
  metadata?: { publicKey?: string; env?: string; instructions?: string; minDeposit?: number }
}

export type CloudResource = {
  id: string
  type: string
  name?: string
  status?: string
  externalId?: string
  // Which cloud service/region the resource lives on — set on rows synced after the fields
  // were introduced; consumers must fall back to a project-level location when absent.
  serviceId?: string
  region?: string
  projectId?: string
  createdAt?: string
  info?: { createdAt?: string; updatedAt?: string }
  data?: Record<string, any>
  [k: string]: unknown
}

export type ProjectService = {
  id: string
  name?: string
  type?: string
  status?: string
  // Managed-Kubernetes (kamaji) services only: curated cluster versions, plus an optional
  // admin allowlist of node-group flavor ids (present and non-empty only when configured).
  kubernetesVersions?: string[]
  kubernetesFlavorIds?: string[]
  // Curated node-image variant names per version (e.g. {"1.35.4": ["nvidia"]}) — a node group
  // may pick one instead of the version's default image. Present only when configured.
  kubernetesImageVariants?: Record<string, string[]>
  // The provider's pinned platform (chart) version — feeds the opt-in "platform update" offer.
  kubernetesPlatformVersion?: string
  // Managed-database (dbaas) services only: the curated per-engine catalog the create
  // wizard offers, the provider's pinned platform (chart) version, the size limits the
  // create form validates against (0/absent = unbounded), and the optional StorageClass
  // allowlist (present only when configured; absent = the cluster default only).
  databaseEngines?: Record<string, DatabaseEngineOffer>
  databasePlatformVersion?: string
  databaseLimits?: DatabaseLimits
  databaseStorageClasses?: string[]
  [k: string]: unknown
}

export type DatabaseLimits = {
  maxCpu?: number
  maxMemoryGiB?: number
  maxStorageGiB?: number
}

export type DatabaseEngineOffer = {
  versions?: string[]
  default?: string
  // Allowed replica counts (e.g. [1, 3]); absent = 1 or 3.
  replicas?: number[]
  beta?: boolean
}

// Live quota values. `reserved` is capacity promised to in-flight
// operations, so consumers must include it when calculating current usage.
export type QuotaMetric = {
  used: number
  reserved: number
  // A negative limit means unlimited.
  limit: number
}

export type ProjectQuotaUsage = {
  serviceId: string
  region: string
  compute?: {
    instances?: QuotaMetric
    cores?: QuotaMetric
    ramMb?: QuotaMetric
  }
  storage?: {
    volumes?: QuotaMetric
    gigabytes?: QuotaMetric
    snapshots?: QuotaMetric
    perVolumeGigabytes?: QuotaMetric
    volumeTypes?: Record<string, {
      volumes?: QuotaMetric
      gigabytes?: QuotaMetric
      snapshots?: QuotaMetric
    }>
  }
  gpu: {
    limits: Record<string, number>
    usage: Record<string, number>
    // False means the cache could not provide a trustworthy usage snapshot.
    // Limits remain useful for display, but the UI must not treat missing usage as zero.
    usageAvailable: boolean
  }
  // A partial provider response still returns the metrics that were available.
  warnings: string[]
}

export type Location = {
  serviceId?: string
  region?: string
  displayName?: string
  resourceTypes?: string[]
  /** Backing provider: "proxmox" | "ceph-s3" | "rustfs" | "kamaji" | "dbaas". */
  provider?: string
  serviceName?: string
  [k: string]: unknown
}

export type CostInfo = {
  currentMonthCosts?: number
  forecastedMonthEndCosts?: number
  currentMonthCostsByType?: Record<string, number>
  dueAmount?: number
  balance?: number
  accountCredit?: number
  promotionalCredits?: number
  topResourcePrices?: Array<{
    // Backend emits currentCost; price is a legacy alias some callers still read.
    currentCost?: number
    price?: number
    resource?: { id?: string; type?: string; name?: string; createdAt?: string; data?: Record<string, any> }
  }>
  projects?: Record<string, CostInfo>
  [k: string]: unknown
}

export type Bill = {
  id: string
  status?: string
  invoiceNumber?: string
  netAmount?: number
  grossAmount?: number
  unpaidGrossAmount?: number
  invoiceCurrency?: string
  createdAt?: string
  dueAt?: string
  items?: Array<Record<string, any>>
  [k: string]: unknown
}

export type Transaction = {
  id: string
  status?: string
  amount?: number
  grossAmount?: number
  currency?: string
  createdAt?: string
  externalInvoiceId?: string
  [k: string]: unknown
}

export type CreditCard = {
  id: string
  panMasked?: string
  tokenExpirationDate?: string
  [k: string]: unknown
}

/** Providers that serve only storage products (no compute). */
export const isObjectProvider = (provider?: string) => provider === "ceph-s3" || provider === "rustfs" || provider === "truenas"
