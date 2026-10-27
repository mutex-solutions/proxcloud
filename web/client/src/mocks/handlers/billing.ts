// Billing: summary, cost info, bills, transactions, cards, funds, savings, credits.
import { on } from "../router"
import { db } from "../db"
import {
  accountCreditTransactions, billingSummary, bills, collectTransactions,
  costInfo, countries, orgCostInfo, paymentGateways, savingsPlans,
} from "../fixtures/billing"

on("GET /project/:pid/billing", () => ({ data: billingSummary }))
on("GET /project/:pid/cost-info", () => ({ data: costInfo }))
on("GET /bill/:bp/cost-info", () => ({ data: orgCostInfo }))

on("GET /bill/:bp", () => ({ data: bills }))
on("GET /bill/:bp/:billId", ({ params }) => ({ data: bills.find((b) => b.id === params.billId) }))
on("GET /bill/:bp/download/:billId/statement", () => ({ data: new Response(new Blob(["%PDF-1.4 mock"], { type: "application/pdf" })) }))

on("GET /collect-transactions", () => ({ data: collectTransactions }))
on("GET /collect-transactions/:bp/bill/:billId", ({ params }) => ({
  data: collectTransactions.filter((t) => t.billId === params.billId),
}))
on("GET /collect-transactions/:bp/download/:txnId", () => ({ data: new Response(new Blob(["%PDF-1.4 mock"], { type: "application/pdf" })) }))
on("GET /account-credit-transactions", () => ({ data: accountCreditTransactions }))

on("POST /payment/:bp/bill/:billId/pay", ({ params }) => {
  const bill = bills.find((b) => b.id === params.billId)
  if (bill) {
    bill.status = "PAID"
    bill.unpaidGrossAmount = 0
  }
  return { data: {} }
})

// Cards + funds (Stripe flows return canned intents; confirm callbacks no-op)
on("GET /card/:bp", () => ({ data: db.cards }))
on("POST /card/:bp/add", () => ({ data: { transactionId: "txn-mock-add", externalPaymentId: "pi_mock", metadata: { client_secret: "seti_mock_secret" } } }))
on("POST /card/:bp/:cardId/default", () => ({ data: {} }))
on("DELETE /card/:cardId", ({ params }) => {
  db.cards = db.cards.filter((c) => c.id !== params.cardId)
  return { data: {} }
})
on("GET /payment/:bp/gateway", () => ({ data: paymentGateways }))
on("POST /payment/deposit/:bp/card", ({ opts }) => ({
  data: { status: "SUCCESS", grossAmount: (opts.body as { amount?: number })?.amount ?? 0 },
}))
on("POST /payment/deposit/:bp", () => ({ data: { transactionId: "txn-mock-deposit", externalPaymentId: "pi_mock", metadata: "pi_mock_client_secret" } }))
on("GET /callbacks/payment/stripe/card/confirm/:txnId", () => ({ data: {} }))
on("GET /callbacks/payment/stripe/funds/confirm/:txnId", () => ({ data: {} }))

on("GET /billing-profile", () => ({ data: [billingSummary] }))
on("GET /billing-profile/countries", () => ({ data: countries }))
on("PUT /billing-profile/:bp", () => ({ data: {} }))

// Savings plans + contracts
on("GET /savings-plans", () => ({ data: savingsPlans }))
on("GET /savings-contracts/:bp", () => ({ data: db.savingsContracts }))
on("GET /savings-contracts/:bp/:planId/eligible", () => ({ data: true }))
on("POST /savings-contracts/:bp", ({ opts }) => {
  const body = opts.body as Record<string, any>
  db.savingsContracts.push({
    id: db.nextId("sc"),
    savingsPlanId: body.savingsPlanId,
    savingsPlanName: "Compute savings plan",
    status: "ACTIVE",
    durationMonths: body.durationMonths,
    monthlyCommittedAmount: body.monthlyCommittedAmount,
    discountRate: 0.2,
    paidUpfront: body.paidUpfront,
    startDate: "2026-08-01",
    endDate: "2027-07-31",
  })
  return { data: {} }
})
on("DELETE /savings-contracts/:bp/:id", ({ params }) => {
  db.savingsContracts = db.savingsContracts.filter((c) => c.id !== params.id)
  return { data: {} }
})
on("POST /savings-contracts/:bp/:id/extend", () => ({ data: {} }))

// Promotional credits
on("GET /promotional-credits/:bp", () => ({ data: db.promoCredits }))
on("POST /promotion/:bp/code", ({ query }) => {
  db.promoCredits.push({
    id: db.nextId("pc"),
    code: query.get("code") ?? "MOCK-CODE",
    initialAmount: 20,
    remainingAmount: 20,
    expirationDate: "2026-12-31T23:59:59Z",
    createdAt: new Date().toISOString(),
  })
  return { data: {} }
})
