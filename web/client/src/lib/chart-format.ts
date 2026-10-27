/**
 * Formatters for the chart/KPI layer.
 *
 * `fmtMoney` (src/lib/format.ts) stays the source of truth for tables and
 * detail views (handles string/undefined inputs, em-dash fallback). The
 * helpers here are chart-oriented: compact axis/tooltip numbers and
 * sub-cent-aware money for usage-style metrics.
 */
import { fmtMoney } from "@/lib/format"

export { fmtMoney }

/** Compact number: 950 → "950", 1_234 → "1.2K", 3_400_000 → "3.4M", 1.1e9 → "1.1B". */
export function formatNumber(num: number): string {
  if (!Number.isFinite(num)) return "0"
  const abs = Math.abs(num)
  const sign = num < 0 ? "-" : ""
  if (abs >= 1_000_000_000) return sign + (abs / 1_000_000_000).toFixed(1) + "B"
  if (abs >= 1_000_000) return sign + (abs / 1_000_000).toFixed(1) + "M"
  if (abs >= 1_000) return sign + (abs / 1_000).toFixed(1) + "K"
  return num.toLocaleString("en-US")
}

/** USD currency: "$0.00" for zero/NaN, 4dp for sub-cent values ("$0.0004"). */
export function formatCurrency(value: number): string {
  if (Number.isNaN(value) || value === 0) return "$0.00"
  if (Math.abs(value) < 0.01) return `$${value.toFixed(4)}`
  return `$${value.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`
}

/**
 * Multi-currency money via Intl.NumberFormat, consistent with fmtMoney
 * (en-US locale, 2dp cap) but with sub-cent precision for tiny unit prices.
 */
export function formatMoney(value: number | string | undefined, currency = "USD"): string {
  const n = typeof value === "string" ? parseFloat(value) : value
  if (n === undefined || Number.isNaN(n)) return "—"
  const abs = Math.abs(n)
  if (abs > 0 && abs < 0.01) {
    return new Intl.NumberFormat("en-US", {
      style: "currency",
      currency,
      minimumFractionDigits: 4,
      maximumFractionDigits: 4,
    }).format(n)
  }
  return fmtMoney(n, currency)
}
