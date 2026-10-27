import type * as React from "react"
import NumberFlow from "@number-flow/react"
import type { Format } from "@number-flow/react"
import { ArrowDownRight, ArrowUpRight } from "lucide-react"
import type { LucideIcon } from "lucide-react"

import { Card } from "@/components/ui/card"
import { Sparkline } from "@/components/sparkline"
import { cn } from "@/lib/utils"

interface StatCardProps {
  label: string
  value?: React.ReactNode
  /** When set, the value is rendered as an animated NumberFlow instead of `value`. */
  numericValue?: number
  /**
   * Intl.NumberFormat options forwarded to NumberFlow
   * (NumberFlow's `Format` — a subset of Intl.NumberFormatOptions,
   * e.g. { style: "currency", currency: "USD" }).
   */
  format?: Format
  icon?: LucideIcon
  /** `tone` decouples color from direction: a rising cost is "negative" even
   * though the arrow points up. Defaults to up=positive / down=negative. */
  trend?: { direction: "up" | "down"; label: string; tone?: "positive" | "negative" | "neutral" }
  hint?: string
  sparkline?: number[]
  className?: string
}

export function StatCard({
  label,
  value,
  numericValue,
  format,
  icon: Icon,
  trend,
  hint,
  sparkline,
  className,
}: StatCardProps) {
  return (
    <Card className={cn("gap-2 py-5", className)}>
      <div className="flex items-center justify-between gap-2 px-5">
        <span className="text-sm font-medium text-muted-foreground">{label}</span>
        {Icon && (
          <span className="rounded-md bg-primary/10 p-1.5 text-primary">
            <Icon className="size-4" aria-hidden="true" />
          </span>
        )}
      </div>
      <div className="px-5">
        <div className="text-2xl font-bold tracking-tight tabular-nums">
          {numericValue !== undefined ? (
            <NumberFlow value={numericValue} format={format} />
          ) : (
            value
          )}
        </div>
        {(trend || hint) && (
          <div className="mt-1 flex items-center gap-1.5 text-xs">
            {trend && (
              <span
                className={cn(
                  "inline-flex items-center gap-0.5 font-medium",
                  (trend.tone ?? (trend.direction === "up" ? "positive" : "negative")) === "positive"
                    ? "text-success-text"
                    : (trend.tone ?? "negative") === "negative"
                      ? "text-warning-text"
                      : "text-muted-foreground"
                )}
              >
                {trend.direction === "up" ? (
                  <ArrowUpRight className="size-3.5" aria-hidden="true" />
                ) : (
                  <ArrowDownRight className="size-3.5" aria-hidden="true" />
                )}
                {trend.label}
              </span>
            )}
            {trend && hint && <span aria-hidden className="text-muted-foreground/60">·</span>}
            {hint && <span className="text-muted-foreground">{hint}</span>}
          </div>
        )}
      </div>
      {sparkline && sparkline.length > 1 && (
        <div className="mt-auto px-5">
          <Sparkline points={sparkline} fill />
        </div>
      )}
    </Card>
  )
}
