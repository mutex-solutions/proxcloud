import { useMemo } from "react"
import { Area, AreaChart, CartesianGrid, Cell, Pie, PieChart, XAxis, YAxis } from "recharts"
import { CreditCard, Cpu, FolderKanban, Server, Users } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { StatCard } from "@/components/stat-card"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart"
import { Skeleton } from "@/components/ui/skeleton"
import { useAdminList, useAdminStats } from "@/lib/hooks"
import { fmtMoney, fmtMoneyTight } from "@/lib/format"
import { cn } from "@/lib/utils"

// Stable slot assignment: categories are sorted before colors are handed out,
// so a resource type keeps its hue across reloads; >7 fold into "Other".
const MAX_SLICES = 7
function slotColor(index: number): string {
  return index < 9 ? `var(--chart-${index + 1})` : "var(--chart-other)"
}
function typeLabel(key: string): string {
  const label = key.toLowerCase().replace(/_/g, " ")
  return label.charAt(0).toUpperCase() + label.slice(1)
}

// The /admin/stats money series are Record<currency, amount>; the dashboard
// trend chart shows the combined movement (mock/platform data is USD-major).
function sumTotals(total?: Record<string, number>): number {
  return Object.values(total ?? {}).reduce((a, b) => a + Number(b || 0), 0)
}

// GPU capacity (gpu-info) — one row block per cloud provider; hides itself when
// the provider reports no GPU devices.
type GpuRegionCapacity = { region: string; gpus: Array<{ name: string; total: number; inUse: number }> }

function ServiceGpuRows({ svc }: { svc: { id: string; name?: string } }) {
  const q = useAdminList<GpuRegionCapacity>(`/admin/service/${svc.id}/gpu-info`, !!svc.id)
  if (q.isLoading) return <Skeleton className="h-10" />
  const rows = (q.data?.data ?? []).flatMap((r) => r.gpus.map((g) => ({ ...g, region: r.region })))
  if (rows.length === 0) return null
  return (
    <div className="space-y-2">
      <p className="text-eyebrow">{svc.name ?? svc.id}</p>
      {rows.map((g) => {
        const pct = g.total ? Math.round((g.inUse / g.total) * 100) : 0
        return (
          <div key={`${g.region}-${g.name}`} className="flex items-center gap-3 text-sm">
            <span className="w-56 truncate font-mono text-xs">
              {g.name} <span className="text-muted-foreground">· {g.region}</span>
            </span>
            <div
              className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted"
              role="meter"
              aria-label={`${g.name} GPUs in use in ${g.region}`}
              aria-valuenow={pct}
              aria-valuemin={0}
              aria-valuemax={100}
            >
              <div
                className={cn(
                  "h-full rounded-full",
                  pct >= 100 ? "bg-destructive" : pct >= 85 ? "bg-warning" : "bg-primary",
                )}
                style={{ width: `${Math.min(pct, 100)}%` }}
              />
            </div>
            <span className="w-24 text-right font-mono text-xs tabular-nums text-muted-foreground">
              {g.inUse}/{g.total} used
            </span>
          </div>
        )
      })}
    </div>
  )
}

function GpuCapacityCard() {
  const services = useAdminList<{ id: string; name?: string }>("/admin/service")
  const list = services.data?.data ?? []
  return (
    <Card>
      <CardHeader className="border-b">
        <CardTitle className="flex items-center gap-2 text-base">
          <Cpu className="size-4 text-muted-foreground/70" aria-hidden="true" /> GPU capacity
        </CardTitle>
        <p className="text-sm text-muted-foreground">Accelerators in use per cloud provider region.</p>
      </CardHeader>
      <CardContent className="space-y-4">
        {services.isLoading ? (
          <Skeleton className="h-16" />
        ) : list.length === 0 ? (
          <p className="py-4 text-center text-sm text-muted-foreground">No cloud providers configured.</p>
        ) : (
          list.map((s) => <ServiceGpuRows key={s.id} svc={s} />)
        )}
      </CardContent>
    </Card>
  )
}

// Setup checklist row — the status dot carries the color, the neutral label
// carries the information (design-system convention; keeps colored copy off the page).
// Literal classes so Tailwind emits them: status-dot-ok status-dot-warn
function SetupRow({ label, ok }: { label: string; ok?: boolean }) {
  return (
    <div className="flex items-center justify-between border-b py-2.5 text-sm last:border-0">
      <span>{label}</span>
      <span className="inline-flex items-center gap-1.5">
        <span className={cn("status-dot", ok ? "status-dot-ok" : "status-dot-warn")} />
        <span className="text-muted-foreground">{ok ? "Configured" : "Not set up"}</span>
      </span>
    </div>
  )
}

const moneyConfig = {
  billed: { label: "Billed", color: "var(--chart-3)" },
  collected: { label: "Collected", color: "var(--chart-1)" },
} satisfies ChartConfig

const signupConfig = {
  users: { label: "New users", color: "var(--chart-2)" },
  billingProfiles: { label: "Billing profiles", color: "var(--chart-4)" },
} satisfies ChartConfig

export function DashboardPage() {
  const { data, isLoading } = useAdminStats()
  const resources = useAdminList<{ id?: string; type?: string }>("/admin/cloud-resource")

  // Billed (invoices raised) vs collected (payments received), last 12 months.
  const money = useMemo(() => {
    const byKey = new Map<string, { label: string; billed: number; collected: number }>()
    for (const b of data?.insights?.bills ?? []) {
      const label = `${b.year}-${String(b.month).padStart(2, "0")}`
      byKey.set(label, { label, billed: sumTotals(b.total), collected: 0 })
    }
    for (const p of data?.insights?.payments ?? []) {
      const label = `${p.year}-${String(p.month).padStart(2, "0")}`
      const row = byKey.get(label) ?? { label, billed: 0, collected: 0 }
      row.collected = sumTotals(p.total)
      byKey.set(label, row)
    }
    return [...byKey.values()].sort((a, b) => a.label.localeCompare(b.label))
  }, [data?.insights?.bills, data?.insights?.payments])

  // New users + new billing profiles, last 30 days.
  const signups = useMemo(() => {
    const byKey = new Map<string, { label: string; users: number; billingProfiles: number }>()
    for (const d of data?.insights?.newUsers ?? []) {
      const label = `${String(d.month).padStart(2, "0")}-${String(d.day).padStart(2, "0")}`
      byKey.set(label, { label, users: d.count, billingProfiles: 0 })
    }
    for (const d of data?.insights?.newBillingProfiles ?? []) {
      const label = `${String(d.month).padStart(2, "0")}-${String(d.day).padStart(2, "0")}`
      const row = byKey.get(label) ?? { label, users: 0, billingProfiles: 0 }
      row.billingProfiles = d.count
      byKey.set(label, row)
    }
    return [...byKey.values()]
  }, [data?.insights?.newUsers, data?.insights?.newBillingProfiles])

  // Cloud resources by type — donut slices with stable (sorted-key) colors.
  const byType = useMemo(() => {
    const counts = new Map<string, number>()
    for (const r of resources.data?.data ?? []) {
      const key = r.type ?? "UNKNOWN"
      counts.set(key, (counts.get(key) ?? 0) + 1)
    }
    const entries = [...counts.entries()].sort(([a], [b]) => a.localeCompare(b))
    const head = entries.slice(0, MAX_SLICES)
    const rest = entries.slice(MAX_SLICES)
    const rows = head.map(([key, value], i) => ({
      key,
      label: typeLabel(key),
      value,
      color: slotColor(i),
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
  }, [resources.data?.data])

  const donutConfig = useMemo(
    () =>
      Object.fromEntries(byType.map((r) => [r.key, { label: r.label, color: r.color }])) satisfies ChartConfig,
    [byType],
  )
  const resourceTotal = byType.reduce((s, r) => s + r.value, 0)

  return (
    <>
      <PageHeader
        title="Dashboard"
        eyebrow="Overview"
        description="Platform-wide activity summary."
      />

      {isLoading ? (
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
          <StatCard label="Users" numericValue={data?.users ?? 0} icon={Users} hint="Registered accounts" />
          <StatCard label="Projects" numericValue={data?.projects ?? 0} icon={FolderKanban} hint="Across all organizations" />
          <StatCard
            label="Cloud resources"
            numericValue={data?.cloudResources ?? 0}
            icon={Server}
            hint="Servers, volumes, buckets…"
          />
          <StatCard label="Transactions" numericValue={data?.transactions ?? 0} icon={CreditCard} hint="Payments processed" />
        </div>
      )}

      <div className="mt-6 grid gap-4 lg:grid-cols-3">
        <Card>
          <CardHeader className="border-b">
            <CardTitle className="text-base">Platform setup</CardTitle>
            <p className="text-sm text-muted-foreground">One-time configuration checklist.</p>
          </CardHeader>
          <CardContent>
            {isLoading ? (
              <div className="space-y-3 py-1">
                {[0, 1, 2, 3, 4].map((i) => (
                  <Skeleton key={i} className="h-6" />
                ))}
              </div>
            ) : (
              <>
                <SetupRow label="Cloud provider" ok={data?.cloudProviderConfigured} />
                <SetupRow label="Billing" ok={data?.billingConfigured} />
                <SetupRow label="Branding" ok={data?.brandingConfigured} />
                <SetupRow label="Mail gateway" ok={data?.mailGatewayConfigured} />
                <SetupRow label="Price plan" ok={data?.pricePlanConfigured} />
              </>
            )}
          </CardContent>
        </Card>

        <Card className="lg:col-span-2">
          <CardHeader className="border-b">
            <CardTitle className="text-base">Billed vs collected</CardTitle>
            <p className="text-sm text-muted-foreground">Invoices raised and payments received, last 12 months.</p>
          </CardHeader>
          <CardContent>
            {isLoading ? (
              <Skeleton className="h-56" />
            ) : money.length ? (
              <ChartContainer config={moneyConfig} className="aspect-auto h-56 w-full">
                <AreaChart data={money} margin={{ left: 4, right: 8, top: 4 }}>
                  <defs>
                    <linearGradient id="fillBilled" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%" stopColor="var(--chart-3)" stopOpacity={0.3} />
                      <stop offset="95%" stopColor="var(--chart-3)" stopOpacity={0.02} />
                    </linearGradient>
                    <linearGradient id="fillCollected" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%" stopColor="var(--chart-1)" stopOpacity={0.3} />
                      <stop offset="95%" stopColor="var(--chart-1)" stopOpacity={0.02} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid strokeDasharray="3 3" vertical={false} stroke="var(--border)" />
                  <XAxis dataKey="label" tickLine={false} axisLine={false} tick={{ fontSize: 11 }} tickMargin={8} minTickGap={28} />
                  <YAxis
                    tickLine={false}
                    axisLine={false}
                    tick={{ fontSize: 11 }}
                    width={48}
                    tickFormatter={(v) => fmtMoneyTight(Number(v))}
                  />
                  <ChartTooltip
                    content={
                      <ChartTooltipContent
                        indicator="line"
                        formatter={(value, name, item) => (
                          <div className="flex w-full items-center justify-between gap-4">
                            <span className="flex items-center gap-1.5">
                              <span
                                className="h-2.5 w-1 shrink-0 rounded-[2px]"
                                style={{ backgroundColor: item.color }}
                              />
                              <span className="text-muted-foreground">
                                {moneyConfig[name as keyof typeof moneyConfig]?.label ?? name}
                              </span>
                            </span>
                            <span className="font-mono font-medium tabular-nums">{fmtMoney(Number(value))}</span>
                          </div>
                        )}
                      />
                    }
                  />
                  <ChartLegend content={<ChartLegendContent />} />
                  <Area
                    dataKey="billed"
                    type="monotone"
                    stroke="var(--chart-3)"
                    strokeWidth={2}
                    fill="url(#fillBilled)"
                    isAnimationActive={false}
                  />
                  <Area
                    dataKey="collected"
                    type="monotone"
                    stroke="var(--chart-1)"
                    strokeWidth={2}
                    fill="url(#fillCollected)"
                    isAnimationActive={false}
                  />
                </AreaChart>
              </ChartContainer>
            ) : (
              <p className="py-16 text-center text-sm text-muted-foreground">No billing activity yet.</p>
            )}
          </CardContent>
        </Card>
      </div>

      <div className="mt-4 grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader className="border-b">
            <CardTitle className="text-base">Cloud resources by type</CardTitle>
            <p className="text-sm text-muted-foreground">Everything provisioned across client projects.</p>
          </CardHeader>
          <CardContent>
            {resources.isLoading ? (
              <Skeleton className="h-44" />
            ) : byType.length ? (
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
                                <span className="font-mono font-medium tabular-nums">{Number(value)}</span>
                              </div>
                            )}
                          />
                        }
                      />
                      <Pie
                        data={byType}
                        dataKey="value"
                        nameKey="key"
                        innerRadius={55}
                        outerRadius={82}
                        paddingAngle={2}
                        strokeWidth={2}
                        isAnimationActive={false}
                      >
                        {byType.map((r) => (
                          <Cell key={r.key} fill={r.color} stroke="var(--card)" />
                        ))}
                      </Pie>
                    </PieChart>
                  </ChartContainer>
                  {/* Donut center total: HTML, wearing text tokens. */}
                  <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
                    <span className="text-xs text-muted-foreground">Total</span>
                    <span className="font-display text-lg font-semibold tabular-nums">{resourceTotal}</span>
                  </div>
                </div>
                <div className="flex w-full flex-1 flex-col gap-1.5">
                  {byType.map((r) => (
                    <div
                      key={r.key}
                      className="flex items-center justify-between rounded bg-muted/40 px-2 py-1.5 text-xs"
                    >
                      <span className="flex items-center gap-2">
                        <span className="size-2 rounded-full" style={{ backgroundColor: r.color }} />
                        <span className="text-muted-foreground">{r.label}</span>
                      </span>
                      <span className="font-medium tabular-nums">{r.value}</span>
                    </div>
                  ))}
                </div>
              </div>
            ) : (
              <p className="py-16 text-center text-sm text-muted-foreground">No cloud resources provisioned yet.</p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="border-b">
            <CardTitle className="text-base">New signups</CardTitle>
            <p className="text-sm text-muted-foreground">Users and billing profiles created, last 30 days.</p>
          </CardHeader>
          <CardContent>
            {isLoading ? (
              <Skeleton className="h-56" />
            ) : signups.length ? (
              <ChartContainer config={signupConfig} className="aspect-auto h-56 w-full">
                <AreaChart data={signups} margin={{ left: 4, right: 8, top: 4 }}>
                  <defs>
                    <linearGradient id="fillUsers" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%" stopColor="var(--chart-2)" stopOpacity={0.3} />
                      <stop offset="95%" stopColor="var(--chart-2)" stopOpacity={0.02} />
                    </linearGradient>
                    <linearGradient id="fillBps" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%" stopColor="var(--chart-4)" stopOpacity={0.3} />
                      <stop offset="95%" stopColor="var(--chart-4)" stopOpacity={0.02} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid strokeDasharray="3 3" vertical={false} stroke="var(--border)" />
                  <XAxis dataKey="label" tickLine={false} axisLine={false} tick={{ fontSize: 11 }} tickMargin={8} minTickGap={28} />
                  <YAxis tickLine={false} axisLine={false} tick={{ fontSize: 11 }} width={32} allowDecimals={false} />
                  <ChartTooltip content={<ChartTooltipContent indicator="line" />} />
                  <ChartLegend content={<ChartLegendContent />} />
                  <Area
                    dataKey="billingProfiles"
                    type="monotone"
                    stroke="var(--chart-4)"
                    strokeWidth={2}
                    fill="url(#fillBps)"
                    isAnimationActive={false}
                  />
                  <Area
                    dataKey="users"
                    type="monotone"
                    stroke="var(--chart-2)"
                    strokeWidth={2}
                    fill="url(#fillUsers)"
                    isAnimationActive={false}
                  />
                </AreaChart>
              </ChartContainer>
            ) : (
              <p className="py-16 text-center text-sm text-muted-foreground">No signups in the last 30 days.</p>
            )}
          </CardContent>
        </Card>
      </div>

      <div className="mt-4">
        <GpuCapacityCard />
      </div>
    </>
  )
}
