export function fmtMoney(v: number | string | undefined, currency = "USD"): string {
  if (v === undefined || v === null) return "—"
  const n = typeof v === "string" ? parseFloat(v) : v
  if (Number.isNaN(n)) return "—"
  // A charge under half a cent keeps two significant digits ($0.0034), not a $0.00 that reads as
  // free. Float noise from a subtraction (and -0) is zero, never "-$0.00".
  const v0 = Math.abs(n) < 1e-6 ? 0 : n
  const tiny = v0 !== 0 && Math.abs(v0) < 0.005
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency,
    ...(tiny ? { maximumSignificantDigits: 2 } : { maximumFractionDigits: 2 }),
  }).format(v0)
}

// Bytes in the largest 1024-step unit that keeps a whole digit: 512 B, 3.4 KB, 1.2 GB.
export function fmtBytes(n: number): string {
  const units = ["B", "KB", "MB", "GB", "TB"]
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${i === 0 ? n : n.toFixed(1).replace(/\.0$/, "")} ${units[i]}`
}

// Month-end forecast label: always an estimate (current cost + what the resources running now still
// accrue at their current rates); "approximate" when the API held part of it at its current cost
// (a type it could not rate, a usage total, stale metering, or no projection at all).
export function forecastLabel(
  cost: { forecastedMonthEndCosts?: number; forecastApproximate?: boolean } | undefined,
  currency = "USD",
): string {
  const approx = cost?.forecastApproximate ? " (approximate)" : ""
  return `Est. month end ${fmtMoney(cost?.forecastedMonthEndCosts, currency)}${approx}`
}

// Tooltip text explaining the forecast, including why it is approximate when it is.
export function forecastExplanation(
  cost: { forecastApproximateReasons?: string[]; forecastUnratedTypes?: string[] } | undefined,
): string {
  const base =
    "Estimate: this month's cost so far plus what the resources running now will add until the end of " +
    "the billing month (UTC) at their current rates. Excludes taxes, discounts and future changes."
  const why: string[] = []
  for (const r of cost?.forecastApproximateReasons ?? []) {
    if (r === "unrated") {
      const types = (cost?.forecastUnratedTypes ?? []).join(", ")
      why.push(`some resources could not be priced${types ? ` (${types})` : ""} and are held at their current cost`)
    } else if (r === "usage") why.push("usage such as traffic is held at its month-to-date total")
    else if (r === "stale") why.push("a provider's metering is stale")
    else if (r === "unavailable") why.push("no projection is available, so it shows the current cost")
    else if (r === "external_billing") why.push("bills are rated by an external billing service")
  }
  return why.length ? `${base} Approximate: ${why.join("; ")}.` : base
}

// Money without noise: drops the ".00" tail on whole amounts (tier chips,
// axis ticks) but keeps cents whenever they carry information.
export function fmtMoneyTight(v: number | string | undefined, currency = "USD"): string {
  if (v === undefined || v === null) return "—"
  const n = typeof v === "string" ? parseFloat(v) : v
  if (Number.isNaN(n)) return "—"
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency,
    maximumFractionDigits: Number.isInteger(n) ? 0 : 2,
  }).format(n)
}

export function fmtDate(v: string | number | Date | undefined): string {
  if (!v) return "—"
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return "—"
  return new Intl.DateTimeFormat("en-GB", { day: "2-digit", month: "short", year: "numeric" }).format(d)
}

export function fmtDateTime(v: string | number | Date | undefined): string {
  if (!v) return "—"
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return "—"
  return new Intl.DateTimeFormat("en-GB", {
    day: "2-digit", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit",
  }).format(d)
}

export function timeAgo(v: string | number | Date | undefined): string {
  if (!v) return "—"
  const d = new Date(v).getTime()
  if (Number.isNaN(d)) return "—"
  const s = Math.floor((Date.now() - d) / 1000)
  if (s < 60) return "just now"
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const days = Math.floor(h / 24)
  if (days < 30) return `${days}d ago`
  return fmtDate(v)
}

// status → dot flavor used across resource tables
export function statusKind(status?: string): "ok" | "warn" | "error" | "muted" {
  const s = (status ?? "").toUpperCase()
  if (["ACTIVE", "ENABLED", "SUCCESS", "PAID", "RUNNING", "ONLINE", "AVAILABLE", "IN-USE", "UP"].includes(s)) return "ok"
  if (["ERROR", "FAILED", "SUSPENDED", "DISABLED", "DOWN", "DELETED", "REJECTED"].includes(s)) return "error"
  if (["BUILD", "PENDING", "CREATING", "DELETING", "PAUSED", "SHUTOFF", "PENDING_CREATE", "OPEN", "SENT", "NEW"].includes(s)) return "warn"
  // Compound VERB_PHASE states (CREATE_COMPLETE, UPDATE_FAILED, PENDING_UPDATE, DEGRADED, …).
  if (s.endsWith("_FAILED")) return "error"
  if (s === "SUSPEND_COMPLETE") return "warn" // settled, but paused — not healthy-green
  if (s.endsWith("_COMPLETE")) return "ok"
  if (s.endsWith("_IN_PROGRESS") || s.startsWith("PENDING") || s === "DEGRADED") return "warn"
  return "muted"
}
