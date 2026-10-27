// Dashboard aggregates: /admin/me identity and /admin/stats (KPIs + insight
// series). The series are generated relative to "now" so the charts always
// show a recent window.

type Doc = Record<string, any>

export const adminMe: Doc = {
  sub: "mock-admin-0001",
  email: "admin@example.com",
  firstName: "Ops",
  lastName: "Admin",
  role: "SUPER_ADMIN",
  // The admin:* wildcard grant expands to every permission (AdminShell's
  // hasPermission understands it), so all nav items are visible.
  permissions: ["admin:*"],
}

// Last 12 calendar months, oldest first.
function monthsBack(n: number): Array<{ year: number; month: number }> {
  const out: Array<{ year: number; month: number }> = []
  const d = new Date()
  for (let i = n - 1; i >= 0; i--) {
    const m = new Date(d.getFullYear(), d.getMonth() - i, 1)
    out.push({ year: m.getFullYear(), month: m.getMonth() + 1 })
  }
  return out
}

// Last n days, oldest first.
function daysBack(n: number): Array<{ year: number; month: number; day: number }> {
  const out: Array<{ year: number; month: number; day: number }> = []
  const now = Date.now()
  for (let i = n - 1; i >= 0; i--) {
    const d = new Date(now - i * 86_400_000)
    out.push({ year: d.getFullYear(), month: d.getMonth() + 1, day: d.getDate() })
  }
  return out
}

// Deterministic pseudo-random so the dashboard is stable across reloads.
function wave(i: number, base: number, amp: number): number {
  return Math.round((base + amp * Math.sin(i * 1.7) + (amp / 2) * Math.cos(i * 0.9)) * 100) / 100
}

const paymentMonths = monthsBack(12).map((m, i) => ({ ...m, total: { USD: wave(i, 900, 350), EUR: wave(i, 220, 90) } }))
const billMonths = monthsBack(12).map((m, i) => ({ ...m, total: { USD: wave(i, 1050, 380), EUR: wave(i, 260, 110) } }))
const newUserDays = daysBack(30).map((d, i) => ({ ...d, count: Math.max(0, Math.round(1.6 + 1.4 * Math.sin(i * 0.8))) }))
const newBpDays = daysBack(30).map((d, i) => ({ ...d, count: Math.max(0, Math.round(0.9 + Math.sin(i * 0.5))) }))

export const adminStats: Doc = {
  users: 4,
  projects: 3,
  cloudResources: 6,
  transactions: 5,
  cloudProviderConfigured: true,
  billingConfigured: true,
  brandingConfigured: true,
  mailGatewayConfigured: true,
  pricePlanConfigured: true,
  insights: {
    bills: billMonths,
    payments: paymentMonths,
    newUsers: newUserDays,
    newBillingProfiles: newBpDays,
  },
}
