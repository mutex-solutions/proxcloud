import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { BadgePercent, Gift } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader, sortableRightHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { fmtDate, fmtMoney } from "@/lib/format"
import { useBillingSummary, useProjectId } from "@/lib/hooks"

type PromoCredit = {
  id: string
  code?: string
  initialAmount?: number | string
  remainingAmount?: number | string
  expirationDate?: string
  createdAt?: string
}

// The API uses a far-future sentinel (9999-01-01) for never-expiring credits.
function expiry(v?: string): string {
  if (!v) return "Never"
  const d = new Date(v)
  if (Number.isNaN(d.getTime()) || d.getFullYear() >= 9999) return "Never"
  return fmtDate(v)
}


// Remaining/initial as a compact meter: muted track, primary fill.
function RemainingMeter({ remaining, initial }: { remaining: number; initial: number }) {
  const ratio = initial > 0 ? Math.min(Math.max(remaining / initial, 0), 1) : 0
  return (
    <div
      className="h-1.5 w-20 overflow-hidden rounded-full bg-muted"
      role="meter"
      aria-valuemin={0}
      aria-valuemax={initial}
      aria-valuenow={remaining}
      aria-label="Remaining credit"
    >
      <div className="h-full rounded-full bg-primary" style={{ width: `${ratio * 100}%` }} />
    </div>
  )
}

export default function CreditsPage() {
  const pid = useProjectId()
  const { data: summary, isLoading } = useBillingSummary(pid)
  const bp = summary?.id
  const currency = summary?.currency ?? "USD"

  const { data: credits, isLoading: creditsLoading, error } = useQuery({
    queryKey: ["promotional-credits", bp],
    queryFn: () => apiFetch<PromoCredit[]>(`/promotional-credits/${bp}`),
    enabled: !!bp,
  })

  const columns = useMemo<ColumnDef<PromoCredit, any>[]>(
    () => [
      {
        id: "code",
        accessorFn: (c) => c.code ?? "",
        header: sortableHeader("Code"),
        cell: ({ getValue }) => <span className="font-mono text-sm">{getValue() || "—"}</span>,
      },
      {
        id: "initial",
        accessorFn: (c) => Number(c.initialAmount ?? 0),
        header: sortableRightHeader("Granted"),
        cell: ({ row }) => (
          <div className="text-right font-mono text-muted-foreground tabular-nums">
            {fmtMoney(row.original.initialAmount, currency)}
          </div>
        ),
      },
      {
        id: "remaining",
        accessorFn: (c) => Number(c.remainingAmount ?? 0),
        header: sortableRightHeader("Remaining"),
        cell: ({ row }) => {
          const initial = Number(row.original.initialAmount ?? 0)
          const remaining = Number(row.original.remainingAmount ?? 0)
          return (
            <div className="flex items-center justify-end gap-3">
              <RemainingMeter remaining={remaining} initial={initial} />
              <span className="font-mono font-medium tabular-nums">
                {fmtMoney(row.original.remainingAmount, currency)}
              </span>
            </div>
          )
        },
      },
      {
        id: "expires",
        accessorFn: (c) => c.expirationDate ?? "",
        header: sortableHeader("Expires"),
        cell: ({ row }) => (
          <span className="text-sm text-muted-foreground">{expiry(row.original.expirationDate)}</span>
        ),
      },
      {
        id: "granted",
        accessorFn: (c) => c.createdAt ?? "",
        header: sortableHeader("Granted on"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDate(getValue())}</span>,
      },
    ],
    [currency],
  )

  if (isLoading || !bp) {
    return (
      <>
        <PageHeader title="Promotional credits" eyebrow="Billing" />
        <Skeleton className="h-64" />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Promotional credits"
        eyebrow="Billing"
        description="Credits granted by promotions — they settle bills before account credit."
      />

      <div className="grid items-start gap-4 lg:grid-cols-3">
        <div className="min-w-0 lg:col-span-2">
          {!creditsLoading && !error && !credits?.length ? (
            <EmptyState
              icon={Gift}
              title="No promotional credits"
              hint="Redeem a promo code to receive credit."
            />
          ) : (
            <DataTable
              columns={columns}
              data={credits}
              isLoading={creditsLoading}
              error={error as Error | null}
              getRowId={(c) => c.id}
            />
          )}
        </div>

        <RedeemCard pid={pid} bp={bp} />
      </div>
    </>
  )
}

// Same redeem endpoint as FundsPage — POST /promotion/{bp}/code?code= (both surfaces keep it).
function RedeemCard({ pid, bp }: { pid: string; bp: string }) {
  const qc = useQueryClient()
  const [code, setCode] = useState("")
  const redeem = useMutation({
    mutationFn: () =>
      apiFetch(`/promotion/${bp}/code?code=${encodeURIComponent(code.trim())}`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Promo code redeemed")
      setCode("")
      void qc.invalidateQueries({ queryKey: ["promotional-credits", bp] })
      void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  return (
    <Card className="gap-4">
      <CardHeader className="border-b [.border-b]:pb-4">
        <CardTitle className="flex items-center gap-2 text-base">
          <BadgePercent className="size-4 text-primary" /> Redeem a promo code
        </CardTitle>
        <CardDescription>Promo codes mint promotional credit onto this profile.</CardDescription>
      </CardHeader>
      <CardContent className="flex gap-2">
        <Input
          placeholder="Promo code"
          aria-label="Promo code"
          autoComplete="off"
          className="font-mono"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && code.trim() && !redeem.isPending) redeem.mutate()
          }}
        />
        <Button onClick={() => redeem.mutate()} disabled={!code.trim() || redeem.isPending}>
          {redeem.isPending ? "Redeeming…" : "Redeem"}
        </Button>
      </CardContent>
    </Card>
  )
}
