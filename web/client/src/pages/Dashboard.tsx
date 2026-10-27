import { useMemo, useState } from "react"
import { Link } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"
import { Cell, Pie, PieChart } from "recharts"
import {
  ArrowUpRight, BookOpen, CalendarClock, Receipt, Server, UserPlus, Wallet,
  type LucideIcon,
} from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, MoneyCell } from "@/components/data-table"
import { QuotaOverview } from "@/components/quota-overview"
import { StatCard } from "@/components/stat-card"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from "@/components/ui/chart"
import { Skeleton } from "@/components/ui/skeleton"
import {
  useBillingSummary, useCostInfo, useFeatures, useLocations, useProject, useProjectGpuCapacity,
  useProjectId, useProjectQuota, useUIMenu,
} from "@/lib/hooks"
import { fmtMoney, timeAgo } from "@/lib/format"
import type { CostInfo } from "@/lib/types"

// Stable slot assignment: sort categories so a service keeps its color across
// months/filters; >7 categories fold into "Other".
const MAX_SLICES = 7
type TopResourcePrice = NonNullable<CostInfo["topResourcePrices"]>[number]

function serviceLabel(key: string): string {
  const label = key.toLowerCase().replace(/_/g, " ")
  return label.charAt(0).toUpperCase() + label.slice(1)
}

// Common next steps, gated like the sidebar: while the menu/features queries
// load nothing is hidden; once loaded, a disabled service or absent feature
// drops its action. Pure links — no new endpoints.
function QuickActions({ pid }: { pid: string }) {
  const { data: init } = useUIMenu(pid)
  const { data: features } = useFeatures()
  const items = init?.menu?.items
  const featureSet = features ? new Set(features) : undefined

  const actions: Array<{ to: string; label: string; hint: string; icon: LucideIcon; external?: boolean }> = [
    ...(items && items["compute"]?.enabled !== true
      ? []
      : [{ to: `/p/${pid}/servers/new`, label: "Launch a server", hint: "Create a VM", icon: Server }]),
    ...(featureSet && !featureSet.has("billing")
      ? []
      : [{ to: `/p/${pid}/billing/funds`, label: "Add funds", hint: "Top up your balance", icon: Wallet }]),
    { to: `/p/${pid}/org/members`, label: "Invite a teammate", hint: "Organization members", icon: UserPlus },
    { to: "/docs", label: "Read the docs", hint: "Guides and how-tos", icon: BookOpen, external: true },
  ]

  return (
    <div className="mb-6">
      <div className="text-eyebrow mb-2">Quick actions</div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {actions.map((a) => (
          <Link
            key={a.to}
            to={a.to}
            target={a.external ? "_blank" : undefined}
            rel={a.external ? "noopener noreferrer" : undefined}
            className="group flex items-center gap-3 rounded-xl border bg-card p-3 shadow-sm transition-colors hover:bg-accent/50"
          >
            <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border bg-muted/50 text-muted-foreground">
              <a.icon className="size-4" strokeWidth={1.5} />
            </span>
            <span className="min-w-0">
              <span className="block truncate text-sm font-medium">{a.label}</span>
              <span className="block truncate text-xs text-muted-foreground">{a.hint}</span>
            </span>
            <ArrowUpRight className="ml-auto size-4 shrink-0 text-muted-foreground/50 transition-colors group-hover:text-foreground" />
          </Link>
        ))}
      </div>
    </div>
  )
}

export function DashboardPage() {
  const pid = useProjectId()
  const { project } = useProject(pid)
  const { data: locations, isLoading: locationsLoading } = useLocations(pid)
  const [quotaScopeKey, setQuotaScopeKey] = useState<string>()
  const quotaScopeOptions = useMemo(
    () =>
      (locations ?? [])
        .filter(
          (location) =>
            location.provider !== "ceph-s3" &&
            !!location.serviceId &&
            !!location.region &&
            (!location.resourceTypes ||
              location.resourceTypes.includes("SERVER") ||
              location.resourceTypes.includes("VOLUME")),
        )
        .map((location) => ({
          key: `${location.serviceId}/${location.region}`,
          label: location.displayName || [location.serviceName, location.region].filter(Boolean).join(" / "),
          scope: { serviceId: location.serviceId!, region: location.region! },
        })),
    [locations],
  )
  const selectedQuotaScope =
    quotaScopeOptions.find((option) => option.key === quotaScopeKey) ?? quotaScopeOptions[0]
  const quotaScope = selectedQuotaScope?.scope
  const quota = useProjectQuota(pid, quotaScope)
  const gpuCapacity = useProjectGpuCapacity(pid, quotaScope, project?.gpuCapacityVisible === true)
  const { data: cost, isLoading: costLoading } = useCostInfo(pid)
  const { data: summary } = useBillingSummary(pid)

  const currency = (summary?.currency as string) ?? "USD"
  const projectCost = cost?.projects?.[pid] ?? cost

  const topResourceColumns = useMemo<ColumnDef<TopResourcePrice>[]>(
    () => [
      {
        id: "resource",
        accessorFn: (row) => row.resource?.name ?? row.resource?.id ?? "",
        header: "Resource",
        enableSorting: false,
        cell: ({ row }) => {
          const resource = row.original.resource
          const label = resource?.name ?? resource?.id ?? "—"
          return resource?.type === "SERVER" && resource.id ? (
            <Link
              className="font-medium hover:underline"
              to={`/p/${pid}/servers/${resource.id}`}
            >
              {label}
            </Link>
          ) : (
            <span className="font-medium">{label}</span>
          )
        },
      },
      {
        id: "type",
        accessorFn: (row) => row.resource?.type ?? "",
        header: "Type",
        enableSorting: false,
        cell: ({ row }) => (
          <Badge variant="secondary">{serviceLabel(row.original.resource?.type ?? "—")}</Badge>
        ),
      },
      {
        id: "created",
        accessorFn: (row) => row.resource?.createdAt ?? "",
        header: "Created",
        enableSorting: false,
        cell: ({ row }) => (
          <span className="text-muted-foreground">{timeAgo(row.original.resource?.createdAt)}</span>
        ),
      },
      {
        id: "cost",
        accessorFn: (row) => row.currentCost ?? row.price ?? 0,
        header: () => <div className="text-right">Cost</div>,
        enableSorting: false,
        cell: ({ getValue }) => <MoneyCell value={getValue<number>()} currency={currency} />,
      },
    ],
    [currency, pid],
  )

  const byService = useMemo(() => {
    const entries = Object.entries(projectCost?.currentMonthCostsByType ?? {}).sort(([a], [b]) =>
      a.localeCompare(b),
    )
    const head = entries.slice(0, MAX_SLICES)
    const rest = entries.slice(MAX_SLICES)
    const rows = head.map(([key, value], i) => ({
      key,
      label: serviceLabel(key),
      value,
      color: `var(--chart-${i + 1})`,
    }))
    if (rest.length) {
      rows.push({
        key: "OTHER",
        label: "Other",
        value: rest.reduce((s, [, v]) => s + v, 0),
        color: "var(--chart-other)",
      })
    }
    return rows
  }, [projectCost?.currentMonthCostsByType])

  const donutConfig = useMemo(
    () =>
      Object.fromEntries(
        byService.map((r) => [r.key, { label: r.label, color: r.color }]),
      ) satisfies ChartConfig,
    [byService],
  )

  const monthTotal = byService.reduce((s, r) => s + r.value, 0)

  return (
    <>
      <PageHeader
        title={project?.name ?? "Dashboard"}
        eyebrow="Overview"
        description="Live usage, balance and this month's spend for this project."
      />

      {/* First thing on the page: what the user came to DO. Stats read; actions act. */}
      <QuickActions pid={pid} />

      {costLoading ? (
        <div className="grid gap-4 md:grid-cols-3">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      ) : (
        <div className="grid gap-4 @3xl:grid-cols-3 @container md:grid-cols-3">
          <StatCard
            label="Balance"
            numericValue={summary?.balance ?? 0}
            format={{ style: "currency", currency }}
            hint={`Account credit ${fmtMoney(summary?.accountCredit, currency)} · Promo ${fmtMoney(summary?.promotionalCredit, currency)}`}
            icon={Wallet}
          />
          <StatCard
            label="This month"
            numericValue={projectCost?.currentMonthCosts ?? 0}
            format={{ style: "currency", currency }}
            hint={`Forecast ${fmtMoney(projectCost?.forecastedMonthEndCosts, currency)}`}
            icon={CalendarClock}
            trend={
              projectCost?.forecastedMonthEndCosts && projectCost?.lastMonthCosts
                ? {
                    direction:
                      Number(projectCost.forecastedMonthEndCosts) >= Number(projectCost.lastMonthCosts)
                        ? "up"
                        : "down",
                    label: "vs last month",
                    // Rising spend is a caution signal, not a win.
                    tone:
                      Number(projectCost.forecastedMonthEndCosts) >= Number(projectCost.lastMonthCosts)
                        ? ("negative" as const)
                        : ("positive" as const),
                  }
                : undefined
            }
          />
          <StatCard
            label="Due now"
            numericValue={projectCost?.dueAmount ?? 0}
            format={{ style: "currency", currency }}
            hint={summary?.status ? `Billing profile ${String(summary.status).toLowerCase()}` : undefined}
            icon={Receipt}
          />
        </div>
      )}

      <div className="mt-6">
        <QuotaOverview
          data={quota.data}
          gpuCapacity={project?.gpuCapacityVisible ? gpuCapacity.data : undefined}
          scope={quotaScope}
          scopeOptions={quotaScopeOptions}
          selectedScopeKey={selectedQuotaScope?.key}
          onScopeChange={setQuotaScopeKey}
          isLoading={locationsLoading || quota.isLoading || (project?.gpuCapacityVisible === true && gpuCapacity.isLoading)}
          error={quota.error}
        />
      </div>

      <div className="mt-6 grid gap-4 xl:grid-cols-2">
        <Card className="min-w-0">
          <CardHeader className="border-b">
            <CardTitle className="text-base">Cost by service</CardTitle>
            <p className="text-sm text-muted-foreground">This month, rated so far.</p>
          </CardHeader>
          <CardContent className="flex flex-1 items-center">
            {byService.length > 0 ? (
              <div className="flex w-full flex-col items-center gap-4 sm:flex-row sm:gap-6">
                <div className="relative">
                  <ChartContainer config={donutConfig} className="aspect-square h-44">
                    <PieChart>
                      <ChartTooltip
                        content={<ChartTooltipContent hideLabel formatter={(value, name) => (
                          <div className="flex w-full items-center justify-between gap-4">
                            <span className="text-muted-foreground">{donutConfig[name as string]?.label ?? name}</span>
                            <span className="font-mono font-medium tabular-nums">{fmtMoney(Number(value), currency)}</span>
                          </div>
                        )} />}
                      />
                      <Pie
                        data={byService}
                        dataKey="value"
                        nameKey="key"
                        innerRadius={55}
                        outerRadius={82}
                        paddingAngle={2}
                        strokeWidth={2}
                        isAnimationActive={false}
                      >
                        {byService.map((r) => (
                          <Cell key={r.key} fill={r.color} stroke="var(--card)" />
                        ))}
                      </Pie>
                    </PieChart>
                  </ChartContainer>
                  {/* Donut center total: HTML, wearing text tokens. */}
                  <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
                    <span className="text-xs text-muted-foreground">Total</span>
                    <span className="font-display text-lg font-semibold tabular-nums">
                      {fmtMoney(monthTotal, currency)}
                    </span>
                  </div>
                </div>
                <div className="flex w-full flex-1 flex-col gap-1.5">
                  {byService.map((r) => (
                    <div
                      key={r.key}
                      className="flex items-center justify-between rounded bg-muted/40 px-2 py-1.5 text-xs"
                    >
                      <div className="flex items-center gap-2">
                        <span className="size-2 rounded-full" style={{ backgroundColor: r.color }} />
                        <span className="text-muted-foreground">{r.label}</span>
                      </div>
                      <span className="font-medium tabular-nums">{fmtMoney(r.value, currency)}</span>
                    </div>
                  ))}
                </div>
              </div>
            ) : (
              <p className="w-full py-6 text-center text-sm text-muted-foreground">No usage recorded yet this month.</p>
            )}
          </CardContent>
        </Card>

        <Card className="min-w-0">
          <CardHeader className="border-b">
            <CardTitle className="text-base">Top resources</CardTitle>
            <p className="text-sm text-muted-foreground">What's driving this month's spend.</p>
          </CardHeader>
          <CardContent className="min-w-0">
            <DataTable
              columns={topResourceColumns}
              data={projectCost?.topResourcePrices}
              isLoading={costLoading}
              empty="Nothing billed yet."
              pageSize={5}
              skeletonRows={5}
              getRowId={(row) =>
                `${row.resource?.type ?? "resource"}:${
                  row.resource?.id ?? row.resource?.name ?? row.currentCost ?? row.price ?? "unknown"
                }`
              }
            />
          </CardContent>
        </Card>
      </div>
    </>
  )
}
