// Billing seed data — keyed by the billing-profile id (BP_ID).
import type { Bill, BillingSummary, CostInfo, CreditCard, Transaction } from "@/lib/types"
import { BP_ID, PID } from "./platform"

export const billingSummary: BillingSummary = {
  id: BP_ID,
  status: "ACTIVE",
  currency: "USD",
  balance: 128.4,
  accountCredit: 50,
  promotionalCredit: 25,
  currentMonthUsage: 342.18,
  defaultCardId: "card-1",
  hasBillingDetails: true,
  fullName: "Dev User",
}

export const costInfo: CostInfo = {
  currentMonthCosts: 342.18,
  forecastedMonthEndCosts: 941.0,
  lastMonthCosts: 887.55,
  currentMonthCostsByType: { SERVER: 228.4, VOLUME: 48.9, LOAD_BALANCER: 32.5, FLOATING_IP: 12.38, BUCKET: 20.0 },
  lastMonthCostsByType: { SERVER: 610.2, VOLUME: 130.1, LOAD_BALANCER: 90.25, FLOATING_IP: 32.0, BUCKET: 25.0 },
  dueAmount: 0,
  balance: 128.4,
  accountCredit: 50,
  promotionalCredits: 25,
  topResourcePrices: [
    { currentCost: 96.2, price: 96.2, resource: { id: "res-server-004", type: "SERVER", name: "gpu-trainer", createdAt: "2026-06-12T09:30:00Z" } },
    { currentCost: 66.1, price: 66.1, resource: { id: "res-server-001", type: "SERVER", name: "web-01", createdAt: "2026-06-12T09:30:00Z" } },
    { currentCost: 32.5, price: 32.5, resource: { id: "res-load-balancer-001", type: "LOAD_BALANCER", name: "web-lb", createdAt: "2026-06-12T09:30:00Z" } },
  ],
}

export const orgCostInfo = {
  billingProfileCostInfo: costInfo,
  projects: {
    [PID]: costInfo,
    "prj-staging": { ...costInfo, currentMonthCosts: 58.3, forecastedMonthEndCosts: 160.0, lastMonthCosts: 149.9 },
  },
  currency: "USD",
}

export const bills: Bill[] = [
  {
    id: "bill-2026-06",
    status: "PAID",
    netAmount: 806.86,
    grossAmount: 887.55,
    unpaidGrossAmount: 0,
    invoiceCurrency: "USD",
    createdAt: "2026-07-01T00:00:00Z",
    dueAt: "2026-07-15T00:00:00Z",
    items: [
      { name: "web-01", resourceId: "res-server-001", projectId: PID, resourceType: "SERVER", netAmount: 180.5, currency: "USD" },
      { name: "gpu-trainer", resourceId: "res-server-004", projectId: PID, resourceType: "SERVER", netAmount: 429.7, currency: "USD" },
      { name: "data-vol-01", resourceId: "res-volume-001", projectId: PID, resourceType: "VOLUME", netAmount: 118.2, currency: "USD" },
      { name: "web-lb", resourceId: "res-load-balancer-001", projectId: PID, resourceType: "LOAD_BALANCER", netAmount: 78.46, currency: "USD" },
    ],
    adjustments: [],
    appliedPromotionalCredits: [{ code: "WELCOME25", amount: 25 }],
    appliedAccountCredits: [],
  },
  {
    id: "bill-2026-05",
    status: "SENT",
    netAmount: 512.0,
    grossAmount: 563.2,
    unpaidGrossAmount: 563.2,
    invoiceCurrency: "USD",
    createdAt: "2026-06-01T00:00:00Z",
    dueAt: "2026-06-15T00:00:00Z",
    items: [
      { name: "web-01", resourceId: "res-server-001", projectId: PID, resourceType: "SERVER", netAmount: 512.0, currency: "USD" },
    ],
    adjustments: [],
    appliedPromotionalCredits: [],
    appliedAccountCredits: [],
  },
]

// `billId` scopes a transaction to the bill it settled (deposits carry none).
export const collectTransactions: (Transaction & { billId?: string })[] = [
  { id: "txn-1001", status: "SUCCESS", amount: 887.55, grossAmount: 887.55, currency: "USD", createdAt: "2026-07-02T08:12:00Z", externalInvoiceId: "in_mock_1001", billId: "bill-2026-06" },
  { id: "txn-1000", status: "SUCCESS", amount: 400, grossAmount: 400, currency: "USD", createdAt: "2026-06-05T10:00:00Z", externalInvoiceId: "in_mock_1000" },
]

export const accountCreditTransactions: Transaction[] = [
  { id: "act-2001", status: "SUCCESS", amount: 50, grossAmount: 50, currency: "USD", createdAt: "2026-06-05T10:00:00Z" },
]

export const cards: CreditCard[] = [
  { id: "card-1", panMasked: "**** **** **** 4242", tokenExpirationDate: "2028-11-30", createdAt: "2026-05-01T00:00:00Z" },
  { id: "card-2", panMasked: "**** **** **** 5100", tokenExpirationDate: "2027-03-31", createdAt: "2026-06-10T00:00:00Z" },
]

export const paymentGateways = [
  // thirdParty matches the Go seed ("Stripe") — FundsPage gates new-card deposits on it.
  { id: "gw-stripe", thirdParty: "Stripe", addCard: true, addFunds: true, minDeposit: 10, metadata: { publicKey: "pk_test_mock" } },
]

export const savingsPlans = [
  {
    id: "sp-compute-12",
    name: "Compute savings plan",
    description: "Commit to a monthly compute spend and save up to 35%.",
    available: true,
    targets: [{ resourceType: "SERVER" }],
    // Discounts are stored as percentages (20 → 20%), matching the Go API.
    savingSchedule: [
      { durationMonths: 12, maxAmount: 10_000, noUpfrontTiers: [{ startAmount: 100, discount: 20 }, { startAmount: 1000, discount: 28 }], upfrontTiers: [{ startAmount: 100, discount: 30 }, { startAmount: 1000, discount: 35 }] },
      { durationMonths: 24, maxAmount: 10_000, noUpfrontTiers: [{ startAmount: 100, discount: 25 }], upfrontTiers: [{ startAmount: 100, discount: 38 }] },
    ],
  },
]

export const savingsContracts = [
  {
    id: "sc-1",
    savingsPlanId: "sp-compute-12",
    savingsPlanName: "Compute savings plan",
    status: "ACTIVE",
    durationMonths: 12,
    monthlyCommittedAmount: 300,
    discountRate: 20,
    paidUpfront: false,
    startDate: "2026-03-01",
    endDate: "2027-02-28",
  },
]

export const promoCredits = [
  { id: "pc-1", code: "WELCOME25", initialAmount: 25, remainingAmount: 12.5, expirationDate: "2026-12-31T23:59:59Z", createdAt: "2026-05-01T00:00:00Z" },
  { id: "pc-2", code: "LAUNCH-BONUS", initialAmount: 100, remainingAmount: 100, expirationDate: "9999-12-31T23:59:59Z", createdAt: "2026-07-01T00:00:00Z" },
]

export const countries = [
  { name: "Vietnam", cca2: "VN" },
  { name: "Singapore", cca2: "SG" },
  { name: "United States", cca2: "US" },
  { name: "Germany", cca2: "DE" },
]
