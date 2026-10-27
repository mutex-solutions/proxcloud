export function fmtMoney(v: number | string | undefined, currency = "USD"): string {
  if (v === undefined || v === null) return "—"
  const n = typeof v === "string" ? parseFloat(v) : v
  if (Number.isNaN(n)) return "—"
  return new Intl.NumberFormat("en-US", { style: "currency", currency, maximumFractionDigits: 2 }).format(n)
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
  if (["BUILD", "PENDING", "CREATING", "PAUSED", "SHUTOFF", "PENDING_CREATE", "OPEN", "SENT", "NEW"].includes(s)) return "warn"
  // Compound OpenStack states — Heat (CREATE_COMPLETE, UPDATE_FAILED, …) and
  // Octavia (PENDING_UPDATE, DEGRADED) report VERB_PHASE strings.
  if (s.endsWith("_FAILED")) return "error"
  if (s === "SUSPEND_COMPLETE") return "warn" // settled, but paused — not healthy-green
  if (s.endsWith("_COMPLETE")) return "ok"
  if (s.endsWith("_IN_PROGRESS") || s.startsWith("PENDING") || s === "DEGRADED") return "warn"
  return "muted"
}
