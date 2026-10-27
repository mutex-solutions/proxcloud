import { useMemo, useState } from "react"
import { Bar, BarChart, Cell, Pie, PieChart, XAxis, YAxis } from "recharts"
import { BarChart3 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { StatCard } from "@/components/stat-card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig,
} from "@/components/ui/chart"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { useBillingSummary, useOrgCostInfo, useProjectId, useProjects } from "@/lib/hooks"
import type { CostInfo } from "@/lib/types"
import { fmtMoney, fmtMoneyTight } from "@/lib/format"

const n = (v: unknown) => Number(v ?? 0)

// Stable entity→color slots: color follows the project / resource type, never
// its rank in this month's sorted list. Beyond 7 categories fold into Other.
const MAX_SLICES = 7
function slotColor(index: number): string {
  return index < 9 ? `var(--chart-${index + 1})` : "var(--chart-other)"
}
function typeLabel(key: string): string {
  const label = key.toLowerCase().replace(/_/g, " ")
  return label.charAt(0).toUpperCase() + label.slice(1)
}

export default function OrgBillingPage() {
  const pid = useProjectId()
  const { data: summary } = useBillingSummary(pid)
  const bp = summary?.id
  const { data, isLoading } = useOrgCostInfo(bp)
  const { data: projects } = useProjects()

  const [month, setMonth] = useState<"current" | "last">("current")
  const [proj, setProj] = useState("__all__")

  const currency = data?.currency ?? summary?.currency ?? "USD"
  const projName = (id: string) => projects?.find((p) => p.id === id)?.name || id

  const costByProject = data?.projects ?? {}
  const projectIds = Object.keys(costByProject)

  const totalsKey = month === "current" ? "currentMonthCosts" : "lastMonthCosts"
  const byTypeKey = month === "current" ? "currentMonthCostsByType" : "lastMonthCostsByType"

  // Selected scope: whole org (billing profile) or one project.
  const scope: CostInfo | undefined = proj === "__all__" ? data?.billingProfileCostInfo : costByProject[proj]
  const scopeTotal = n(scope?.[totalsKey])
  const byType = (scope?.[byTypeKey] as Record<string, number> | undefined) ?? {}

  // Stable color slots assigned over the SORTED id list (not spend rank).
  const projectColor = useMemo(() => {
    const sorted = [...projectIds].sort()
    return new Map(sorted.map((id, i) => [id, slotColor(i)]))
  }, [projectIds])

  // Per-project rows sorted by the selected month's spend (bar chart + table).
  const rows = useMemo(
    () =>
      projectIds
        .map((id) => ({
          id,
          name: projName(id),
          current: n(costByProject[id]?.currentMonthCosts),
          last: n(costByProject[id]?.lastMonthCosts),
          color: projectColor.get(id) ?? "var(--chart-other)",
        }))
        .sort((a, b) => (month === "current" ? b.current - a.current : b.last - a.last)),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [data, projects, month, projectColor],
  )
  const projBars = rows
    .map((r) => ({ name: r.name, value: month === "current" ? r.current : r.last, color: r.color }))
    .filter((r) => r.value > 0)

  const typeRows = useMemo(() => {
    const entries = Object.entries(byType)
      .map(([key, value]) => ({ key, value: n(value) }))
      .filter((r) => r.value > 0)
      .sort((a, b) => a.key.localeCompare(b.key))
    const head = entries.slice(0, MAX_SLICES)
    const rest = entries.slice(MAX_SLICES)
    const out = head.map((r, i) => ({ ...r, label: typeLabel(r.key), color: slotColor(i) }))
    if (rest.length) {
      out.push({
        key: "OTHER",
        label: "Other",
        value: rest.reduce((s, r) => s + r.value, 0),
        color: "var(--chart-other)",
      })
    }
    return out
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scope, byTypeKey])

  const barConfig = useMemo(
    () => ({ value: { label: "Spend" } }) satisfies ChartConfig,
    [],
  )
  const donutConfig = useMemo(
    () => Object.fromEntries(typeRows.map((r) => [r.key, { label: r.label, color: r.color }])) satisfies ChartConfig,
    [typeRows],
  )

  const topResources = (scope?.topResourcePrices ?? []).slice(0, 15)

  const orgThis = n(data?.billingProfileCostInfo?.currentMonthCosts)
  const orgLast = n(data?.billingProfileCostInfo?.lastMonthCosts)
  const delta = orgLast > 0 ? ((orgThis - orgLast) / orgLast) * 100 : null

  if (isLoading || !bp) {
    return (
      <>
        <PageHeader title="Organization billing" eyebrow="Organization" />
        <div className="grid gap-4 md:grid-cols-3">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
        <Skeleton className="mt-6 h-72" />
      </>
    )
  }

  if (!projectIds.length) {
    return (
      <>
        <PageHeader
          title="Organization billing"
          eyebrow="Organization"
          description="Per-project and per-resource spend across the organization."
        />
        <EmptyState icon={BarChart3} title="No billed usage yet" hint="Once projects accrue cost this month, per-project and per-resource breakdowns show here." />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Organization billing"
        eyebrow="Organization"
        description="Per-project and per-resource spend across the organization."
        actions={
          <div className="flex items-center gap-2 max-sm:flex-wrap">
            <div className="inline-flex gap-0.5 rounded-lg bg-muted p-0.5">
              <Button
                variant="ghost"
                size="sm"
                className={month === "current" ? "bg-background shadow-sm hover:bg-background" : "text-muted-foreground"}
                onClick={() => setMonth("current")}
              >
                This month
              </Button>
              <Button
                variant="ghost"
                size="sm"
                className={month === "last" ? "bg-background shadow-sm hover:bg-background" : "text-muted-foreground"}
                onClick={() => setMonth("last")}
              >
                Last month
              </Button>
            </div>
            <Select value={proj} onValueChange={setProj}>
              <SelectTrigger className="w-full sm:w-56" aria-label="Filter by project">
                <SelectValue placeholder="All projects" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="__all__">All projects</SelectItem>
                {rows.map((r) => (
                  <SelectItem key={r.id} value={r.id}>
                    {r.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        }
      />

      <div className="grid gap-4 md:grid-cols-3">
        <StatCard
          label={proj === "__all__" ? "All projects" : projName(proj)}
          numericValue={scopeTotal}
          format={{ style: "currency", currency }}
          hint={month === "current" ? "This month" : "Last month"}
        />
        <StatCard
          label="Org this month"
          numericValue={orgThis}
          format={{ style: "currency", currency }}
          hint={`${projectIds.length} project${projectIds.length === 1 ? "" : "s"} billed`}
        />
        <StatCard
          label="Org last month"
          numericValue={orgLast}
          format={{ style: "currency", currency }}
          hint={delta === null ? "No prior month" : undefined}
          trend={
            delta === null
              ? undefined
              : {
                  direction: delta >= 0 ? "up" : "down",
                  label: `${delta >= 0 ? "+" : ""}${delta.toFixed(0)}% this month`,
                  tone: delta >= 0 ? "negative" : "positive",
                }
          }
        />
      </div>

      <div className="mt-6 grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader className="border-b">
            <CardTitle className="text-base">Spend by project</CardTitle>
            <p className="text-sm text-muted-foreground">
              {month === "current" ? "This month, rated so far." : "Last month's invoiced usage."}
            </p>
          </CardHeader>
          <CardContent>
            {projBars.length ? (
              <ChartContainer config={barConfig} className="aspect-auto h-[280px] w-full">
                <BarChart data={projBars} layout="vertical" margin={{ left: 8, right: 16 }}>
                  <XAxis
                    type="number"
                    tickFormatter={(v) => fmtMoneyTight(Number(v), currency)}
                    tick={{ fontSize: 11 }}
                    tickLine={false}
                    axisLine={false}
                    tickCount={5}
                  />
                  <YAxis
                    type="category"
                    dataKey="name"
                    width={120}
                    tick={{ fontSize: 11 }}
                    tickLine={false}
                    axisLine={false}
                  />
                  <ChartTooltip
                    cursor={{ fill: "var(--muted)", fillOpacity: 0.5 }}
                    content={
                      <ChartTooltipContent
                        hideLabel
                        formatter={(value, _name, item) => (
                          <div className="flex w-full items-center justify-between gap-4">
                            <span className="text-muted-foreground">{item?.payload?.name}</span>
                            <span className="font-mono font-medium tabular-nums">
                              {fmtMoney(Number(value), currency)}
                            </span>
                          </div>
                        )}
                      />
                    }
                  />
                  <Bar dataKey="value" radius={[0, 4, 4, 0]} isAnimationActive={false}>
                    {projBars.map((r) => (
                      <Cell key={r.name} fill={r.color} />
                    ))}
                  </Bar>
                </BarChart>
              </ChartContainer>
            ) : (
              <p className="py-16 text-center text-sm text-muted-foreground">No spend in the selected month.</p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="border-b">
            <CardTitle className="text-base">Spend by resource type{proj === "__all__" ? "" : ` — ${projName(proj)}`}</CardTitle>
            <p className="text-sm text-muted-foreground">Where the selected scope's spend goes.</p>
          </CardHeader>
          <CardContent>
            {typeRows.length ? (
              <div className="flex flex-col items-center gap-4 sm:flex-row sm:gap-6">
                <div className="relative">
                  <ChartContainer config={donutConfig} className="aspect-square h-44">
                    <PieChart>
                      <ChartTooltip
                        content={
                          <ChartTooltipContent
                            hideLabel
                            formatter={(value, name) => (
                              <div className="flex w-full items-center justify-between gap-4">
                                <span className="text-muted-foreground">
                                  {donutConfig[name as string]?.label ?? name}
                                </span>
                                <span className="font-mono font-medium tabular-nums">
                                  {fmtMoney(Number(value), currency)}
                                </span>
                              </div>
                            )}
                          />
                        }
                      />
                      <Pie
                        data={typeRows}
                        dataKey="value"
                        nameKey="key"
                        innerRadius={55}
                        outerRadius={82}
                        paddingAngle={2}
                        strokeWidth={2}
                        isAnimationActive={false}
                      >
                        {typeRows.map((r) => (
                          <Cell key={r.key} fill={r.color} stroke="var(--card)" />
                        ))}
                      </Pie>
                    </PieChart>
                  </ChartContainer>
                  <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
                    <span className="text-xs text-muted-foreground">Total</span>
                    <span className="font-display text-lg font-semibold tabular-nums">
                      {fmtMoney(typeRows.reduce((s, r) => s + r.value, 0), currency)}
                    </span>
                  </div>
                </div>
                <div className="flex w-full flex-1 flex-col gap-1.5">
                  {typeRows.map((t) => (
                    <div key={t.key} className="flex items-center justify-between rounded bg-muted/40 px-2 py-1.5 text-xs">
                      <span className="flex items-center gap-2">
                        <span className="size-2 rounded-full" style={{ backgroundColor: t.color }} />
                        <span className="text-muted-foreground">{t.label}</span>
                      </span>
                      <span className="font-mono font-medium tabular-nums">{fmtMoney(t.value, currency)}</span>
                    </div>
                  ))}
                </div>
              </div>
            ) : (
              <p className="py-16 text-center text-sm text-muted-foreground">No spend in the selected month.</p>
            )}
          </CardContent>
        </Card>
      </div>

      <Card className="mt-6 overflow-hidden py-0">
        <div className="text-eyebrow border-b px-5 py-3">Cost by project</div>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Project</TableHead>
              <TableHead className="text-right">This month</TableHead>
              <TableHead className="text-right">Last month</TableHead>
              <TableHead className="text-right">Share</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => {
              const val = month === "current" ? r.current : r.last
              const share = scopeAllTotal(rows, month) > 0 ? (val / scopeAllTotal(rows, month)) * 100 : 0
              return (
                <TableRow key={r.id} className="cursor-pointer" onClick={() => setProj(r.id)}>
                  <TableCell className="font-medium">
                    <span className="flex items-center gap-2">
                      <span className="size-2 rounded-full" style={{ backgroundColor: r.color }} />
                      {r.name}
                    </span>
                  </TableCell>
                  <TableCell className="text-right font-mono tabular-nums">{fmtMoney(r.current, currency)}</TableCell>
                  <TableCell className="text-right font-mono tabular-nums">{fmtMoney(r.last, currency)}</TableCell>
                  <TableCell className="text-right text-sm text-muted-foreground tabular-nums">{share.toFixed(0)}%</TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </Card>

      <Card className="mt-6 overflow-hidden py-0">
        <div className="text-eyebrow border-b px-5 py-3">
          Top resources this month{proj === "__all__" ? "" : ` — ${projName(proj)}`}
        </div>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Resource</TableHead>
              <TableHead>Type</TableHead>
              <TableHead className="text-right">Cost</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {topResources.length ? (
              topResources.map((r, i) => (
                <TableRow key={i}>
                  <TableCell className="font-medium">{r.resource?.name ?? r.resource?.id ?? "—"}</TableCell>
                  <TableCell>
                    <Badge variant="secondary">{typeLabel(r.resource?.type ?? "—")}</Badge>
                  </TableCell>
                  <TableCell className="text-right font-mono tabular-nums">
                    {fmtMoney(n((r as Record<string, unknown>).currentCost ?? r.price), currency)}
                  </TableCell>
                </TableRow>
              ))
            ) : (
              <TableRow>
                <TableCell colSpan={3} className="py-6 text-center text-sm text-muted-foreground">
                  Nothing billed yet this month.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Card>
    </>
  )
}

// Sum of the selected month across all projects — the denominator for each project's share.
function scopeAllTotal(rows: Array<{ current: number; last: number }>, month: "current" | "last"): number {
  return rows.reduce((s, r) => s + (month === "current" ? r.current : r.last), 0)
}
