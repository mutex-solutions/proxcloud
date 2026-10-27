import type { LucideIcon } from "lucide-react"
import { CircuitBoard, Cpu, HardDrive, MemoryStick, Server, TriangleAlert } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import {
  Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle,
} from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import type { CloudScope } from "@/lib/api"
import type { GpuCapacityUsage, ProjectQuotaUsage, QuotaMetric } from "@/lib/types"
import { cn } from "@/lib/utils"

type QuotaOverviewProps = {
  data?: ProjectQuotaUsage
  gpuCapacity?: GpuCapacityUsage
  scope?: CloudScope
  scopeOptions?: Array<{ key: string; label: string; scope: CloudScope }>
  selectedScopeKey?: string
  onScopeChange?: (key: string) => void
  isLoading?: boolean
  error?: unknown
}

type MetricTileProps = {
  label: string
  metric?: QuotaMetric
  icon: LucideIcon
  format?: (value: number) => string
  details?: string[]
}

const countFormat = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 })

function formatCount(value: number): string {
  return countFormat.format(value)
}

function formatRam(value: number): string {
  if (Math.abs(value) < 1024 && value !== 0) return `${formatCount(value)} MiB`
  return `${formatCount(value / 1024)} GiB`
}

function formatGiB(value: number): string {
  return `${formatCount(value)} GiB`
}

function metricValues(metric?: QuotaMetric) {
  if (
    !metric ||
    !Number.isFinite(metric.used) ||
    !Number.isFinite(metric.reserved) ||
    !Number.isFinite(metric.limit)
  ) {
    return undefined
  }
  const used = Math.max(0, metric.used)
  const reserved = Math.max(0, metric.reserved)
  return { used, reserved, consumed: used + reserved, limit: metric.limit }
}

function compactMetric(label: string, metric?: QuotaMetric, format = formatCount): string | undefined {
  const values = metricValues(metric)
  if (!values) return undefined
  const limit = values.limit < 0 ? "Unlimited" : format(values.limit)
  return `${label} ${format(values.consumed)} / ${limit}`
}

function MetricTile({ label, metric, icon: Icon, format = formatCount, details = [] }: MetricTileProps) {
  const values = metricValues(metric)
  if (!values) {
    return (
      <div className="min-w-0 rounded-xl border bg-muted/20 p-4">
        <div className="flex items-center gap-2 text-sm font-medium">
          <Icon className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
          <span className="break-words">{label}</span>
        </div>
        <p className="mt-3 text-sm text-muted-foreground">Unavailable</p>
        {details.map((detail) => (
          <p key={detail} className="mt-1 break-words text-xs text-muted-foreground">{detail}</p>
        ))}
      </div>
    )
  }

  const { used, reserved, consumed, limit } = values
  const unlimited = limit < 0
  const ratio = unlimited ? 0 : limit > 0 ? consumed / limit : 1
  const percent = Math.min(Math.max(ratio * 100, 0), 100)
  const over = !unlimited && consumed > limit
  const exhausted = !unlimited && consumed >= limit
  const nearing = !unlimited && ratio >= 0.8
  const remaining = unlimited ? undefined : Math.max(limit - consumed, 0)
  const valueText = unlimited
    ? `${format(consumed)} used, unlimited`
    : `${format(consumed)} used of ${format(limit)}`

  return (
    <div className="min-w-0 rounded-xl border bg-muted/20 p-4">
      <div className="flex items-center gap-2 text-sm font-medium">
        <Icon className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
        <span className="break-words">{label}</span>
      </div>

      <div
        className={cn(
          "mt-3 font-mono text-lg font-semibold tabular-nums",
          exhausted ? "text-destructive" : nearing ? "text-warning-text" : undefined,
        )}
      >
        {format(consumed)}
        <span className="ml-1 text-sm font-normal text-muted-foreground">
          / {unlimited ? "Unlimited" : format(limit)}
        </span>
      </div>

      {unlimited ? (
        <div className="mt-3 h-1.5 rounded-full bg-muted" aria-label={`${label}: ${valueText}`} />
      ) : limit <= 0 ? (
        <div
          className="mt-3 h-1.5 rounded-full bg-destructive"
          role="status"
          aria-label={`${label}: ${valueText}`}
        />
      ) : (
        <div
          className="mt-3 h-1.5 overflow-hidden rounded-full bg-muted"
          role="meter"
          aria-label={`${label} quota`}
          aria-valuemin={0}
          aria-valuemax={limit}
          aria-valuenow={Math.min(consumed, limit)}
          aria-valuetext={valueText}
        >
          <div
            className={cn(
              "h-full rounded-full transition-[width]",
              exhausted ? "bg-destructive" : nearing ? "bg-warning" : "bg-primary",
            )}
            style={{ width: `${percent}%` }}
          />
        </div>
      )}

      <p className="mt-2 break-words text-xs text-muted-foreground">
        {over
          ? `${format(consumed - limit)} over quota`
          : unlimited
            ? `${format(consumed)} consumed`
            : `${format(remaining ?? 0)} remaining`}
        {reserved > 0 ? ` | ${format(used)} used + ${format(reserved)} reserved` : ""}
      </p>
      {details.map((detail) => (
        <p key={detail} className="mt-1 break-words text-xs text-muted-foreground">{detail}</p>
      ))}
    </div>
  )
}

// gpuMetrics builds the per-model GPU tiles. When the operator enabled GPU capacity for the
// project (gpuCapacity.visible), the tiles list the FULL region GPU catalog — every model the
// region offers, not only the ones this project already uses — and a model with no project limit
// shows the region's free/total capacity instead of a bare "Unlimited". A model WITH a project
// limit keeps its project quota bar (and notes region availability).
function gpuMetrics(data?: ProjectQuotaUsage, gpuCapacity?: GpuCapacityUsage): Array<{
  key: string
  label: string
  metric: QuotaMetric
  details: string[]
}> {
  const limits = data?.gpu?.limits ?? {}
  const usage = data?.gpu?.usage ?? {}
  const usageAvailable = data?.gpu?.usageAvailable !== false
  const projectUsed = (model: string) => (usageAvailable ? Number(usage[model] ?? 0) : Number.NaN)

  const capacityVisible = gpuCapacity?.visible === true
  const capacityByModel = new Map<string, { available: number; total: number }>()
  for (const entry of gpuCapacity?.capacity ?? []) {
    capacityByModel.set(entry.model, { available: entry.available, total: entry.total })
  }

  const explicit = new Set(Object.keys(limits).filter((model) => model !== "*"))
  const hasFallback = Object.prototype.hasOwnProperty.call(limits, "*")
  const fallbackLimit = Number(limits["*"])

  const models = [...new Set([
    ...(capacityVisible ? [...capacityByModel.keys()] : []),
    ...explicit,
    ...Object.keys(usage).filter((model) => model !== "*"),
  ])].sort((a, b) => a.localeCompare(b))

  const rows = models.map((model) => {
    const hasExact = explicit.has(model)
    const capacity = capacityVisible ? capacityByModel.get(model) : undefined

    // A model with a project limit (exact or the "*" fallback) keeps its project quota bar.
    if (hasExact || hasFallback) {
      const limit = hasExact ? Number(limits[model]) : fallbackLimit
      const details = [hasExact ? "Project-wide custom quota" : "Uses the project fallback quota (*)"]
      if (capacity) details.push(`${capacity.available} of ${capacity.total} free in region`)
      return {
        key: model,
        label: `GPU / ${model}`,
        metric: { used: projectUsed(model), reserved: 0, limit },
        details,
      }
    }

    // No project limit but capacity is visible → show the region's free/total for this model.
    if (capacity) {
      const inUse = Math.max(0, capacity.total - capacity.available)
      const yours = projectUsed(model)
      return {
        key: model,
        label: `GPU / ${model}`,
        metric: { used: inUse, reserved: 0, limit: capacity.total },
        details: [
          "Region availability · no project limit",
          Number.isFinite(yours) ? `You're using ${yours}` : "Your usage is unavailable",
        ],
      }
    }

    // No limit, no capacity → unlimited.
    return {
      key: model,
      label: `GPU / ${model}`,
      metric: { used: projectUsed(model), reserved: 0, limit: -1 },
      details: ["No GPU limit configured"],
    }
  })

  // The "*" catch-all only makes sense when we are NOT already listing the full catalog: it stands
  // in for models the project could use under the fallback but has no row for yet.
  if (hasFallback && !capacityVisible && !models.some((model) => !explicit.has(model))) {
    rows.push({
      key: "fallback:*",
      label: "GPU / Other models",
      metric: { used: 0, reserved: 0, limit: fallbackLimit },
      details: ["Fallback quota per unlisted GPU model (*)"],
    })
  }

  return rows
}

export function QuotaOverview({
  data,
  gpuCapacity,
  scope,
  scopeOptions = [],
  selectedScopeKey,
  onScopeChange,
  isLoading = false,
  error,
}: QuotaOverviewProps) {
  const region = data?.region || scope?.region
  const storageDetails = [
    compactMetric("Volumes", data?.storage?.volumes),
    compactMetric("Snapshots", data?.storage?.snapshots),
    data?.storage?.perVolumeGigabytes
      ? (() => {
          const values = metricValues(data.storage?.perVolumeGigabytes)
          if (!values) return undefined
          return `Max per volume ${values.limit < 0 ? "Unlimited" : formatGiB(values.limit)}`
        })()
      : undefined,
  ].filter((detail): detail is string => !!detail)
  const gpu = gpuMetrics(data, gpuCapacity)
  const warnings = data?.warnings ?? []

  return (
    <Card className="min-w-0">
      <CardHeader className="border-b">
        <CardTitle className="text-base">Quota &amp; usage</CardTitle>
        <CardDescription>Current consumption, including resources reserved by in-flight operations.</CardDescription>
        <CardAction>
          {scopeOptions.length > 1 && selectedScopeKey && onScopeChange ? (
            <Select value={selectedScopeKey} onValueChange={onScopeChange}>
              <SelectTrigger className="w-36 sm:w-64" aria-label="Quota location">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {scopeOptions.map((option) => (
                  <SelectItem key={option.key} value={option.key}>{option.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : (
            <Badge
              variant="outline"
              className="max-w-36 truncate sm:max-w-64"
              title={region ? `Region: ${region}` : "No region"}
            >
              {region ? `Region: ${region}` : "No region"}
            </Badge>
          )}
        </CardAction>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4" aria-label="Loading quota usage">
            {[0, 1, 2, 3].map((i) => <Skeleton key={i} className="h-32" />)}
          </div>
        ) : !scope ? (
          <p className="rounded-lg border border-dashed p-5 text-sm text-muted-foreground">
            Quota usage is unavailable until a cloud location is attached to this project.
          </p>
        ) : error ? (
          <p className="rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive" role="alert">
            Could not load quota usage{error instanceof Error && error.message ? `: ${error.message}` : "."}
          </p>
        ) : !data ? (
          <p className="rounded-lg border border-dashed p-5 text-sm text-muted-foreground">
            Quota usage is currently unavailable for this region.
          </p>
        ) : (
          <>
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
              <MetricTile label="Instances" metric={data.compute?.instances} icon={Server} />
              <MetricTile label="vCPU" metric={data.compute?.cores} icon={Cpu} />
              <MetricTile label="RAM" metric={data.compute?.ramMb} icon={MemoryStick} format={formatRam} />
              <MetricTile
                label="Block storage"
                metric={data.storage?.gigabytes}
                icon={HardDrive}
                format={formatGiB}
                details={storageDetails}
              />
              {gpu.map((item) => (
                <MetricTile
                  key={item.key}
                  label={item.label}
                  metric={item.metric}
                  icon={CircuitBoard}
                  details={item.details}
                />
              ))}
            </div>

            {warnings.length > 0 ? (
              <div className="mt-4 flex gap-3 rounded-lg border border-warning/40 bg-warning/10 p-3 text-sm text-warning-text" role="status">
                <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
                <div>
                  <p className="font-medium">Some quota data may be incomplete.</p>
                  <ul className="mt-1 list-disc space-y-1 pl-4">
                    {warnings.map((warning, index) => <li key={`${warning}:${index}`}>{warning}</li>)}
                  </ul>
                </div>
              </div>
            ) : null}
          </>
        )}
      </CardContent>
    </Card>
  )
}
