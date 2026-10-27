import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { CalendarPlus, MoreHorizontal, PiggyBank, XCircle } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader, sortableRightHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { fmtDate, fmtMoney, fmtMoneyTight } from "@/lib/format"
import { useBillingSummary, useProjectId } from "@/lib/hooks"
import { cn } from "@/lib/utils"

type Tier = { startAmount?: number | string; discount?: number | string }
type Schedule = {
  durationMonths: number
  maxAmount?: number | string
  noUpfrontTiers?: Tier[]
  upfrontTiers?: Tier[]
}
type SavingsPlan = {
  id: string
  name?: string
  description?: string
  available?: boolean
  targets?: Array<{ resourceType?: string }>
  savingSchedule?: Schedule[]
}
type SavingsContract = {
  id: string
  savingsPlanId?: string
  savingsPlanName?: string
  status?: string
  durationMonths?: number
  monthlyCommittedAmount?: number | string
  discountRate?: number | string
  paidUpfront?: boolean
  startDate?: string
  endDate?: string
}

// Discount values are stored as percentages (e.g. 10 → 10%); shown raw.
function pct(v: number | string | undefined): string {
  if (v === undefined || v === null || v === "") return "—"
  return `${v}%`
}

// Merge the two tier ladders on their commit thresholds so a schedule renders
// as one readable table: commit | no-upfront discount | upfront discount.
function tierRows(s: Schedule): Array<{ start: number; noUpfront?: Tier["discount"]; upfront?: Tier["discount"] }> {
  const starts = [
    ...new Set([...(s.noUpfrontTiers ?? []), ...(s.upfrontTiers ?? [])].map((t) => Number(t.startAmount ?? 0))),
  ].sort((a, b) => a - b)
  return starts.map((start) => ({
    start,
    noUpfront: s.noUpfrontTiers?.find((t) => Number(t.startAmount ?? 0) === start)?.discount,
    upfront: s.upfrontTiers?.find((t) => Number(t.startAmount ?? 0) === start)?.discount,
  }))
}

export default function SavingsPage() {
  const pid = useProjectId()
  const { data: summary, isLoading } = useBillingSummary(pid)
  const bp = summary?.id
  const currency = summary?.currency ?? "USD"
  const [purchasing, setPurchasing] = useState<SavingsPlan | null>(null)

  const { data: plans, isLoading: plansLoading, error: plansError } = useQuery({
    queryKey: ["savings-plans", bp],
    queryFn: () => apiFetch<SavingsPlan[]>(`/savings-plans?billingProfileId=${bp}`),
    enabled: !!bp,
  })

  if (isLoading || !bp) {
    return (
      <>
        <PageHeader title="Savings plans" eyebrow="Billing" />
        <Skeleton className="h-64" />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Savings plans"
        eyebrow="Billing"
        description="Commit to monthly usage in exchange for a discount on matching resources."
      />

      {plansLoading ? (
        <Skeleton className="h-40" />
      ) : plansError ? (
        <ErrorPanel message={(plansError as Error).message} />
      ) : !plans?.length ? (
        <EmptyState icon={PiggyBank} title="No savings plans available" hint="Plans published by the operator appear here." />
      ) : (
        <div className="grid items-start gap-4 md:grid-cols-2">
          {plans.map((p) => (
            <PlanCard key={p.id} plan={p} currency={currency} onPurchase={() => setPurchasing(p)} />
          ))}
        </div>
      )}

      <ContractsSection bp={bp} currency={currency} />

      {purchasing ? (
        <PurchaseDialog
          bp={bp}
          plan={purchasing}
          currency={currency}
          onOpenChange={(o) => !o && setPurchasing(null)}
        />
      ) : null}
    </>
  )
}

function PlanCard({ plan, currency, onPurchase }: { plan: SavingsPlan; currency: string; onPurchase: () => void }) {
  return (
    <Card className="gap-0 overflow-hidden py-0">
      <div className="flex items-start justify-between gap-4 border-b px-5 py-4">
        <div className="min-w-0">
          <div className="text-eyebrow mb-1">Savings plan</div>
          <h3 className="font-display text-base font-semibold">{plan.name ?? plan.id}</h3>
          {plan.description ? <p className="mt-1 text-sm text-muted-foreground">{plan.description}</p> : null}
        </div>
        <Button size="sm" className="shrink-0" onClick={onPurchase}>Purchase</Button>
      </div>
      <CardContent className="space-y-5 px-5 py-4">
        {plan.targets?.length ? (
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="text-eyebrow mr-1">Applies to</span>
            {plan.targets.map((t, i) => (
              <Badge key={i} variant="secondary">{t.resourceType ?? "any resource"}</Badge>
            ))}
          </div>
        ) : null}

        {plan.savingSchedule?.length ? (
          plan.savingSchedule.map((s, i) => (
            <div key={i}>
              <div className="mb-2 flex items-baseline justify-between gap-2">
                <span className="text-eyebrow">{s.durationMonths}-month term</span>
                {s.maxAmount !== undefined ? (
                  <span className="text-xs text-muted-foreground tabular-nums">
                    up to {fmtMoneyTight(s.maxAmount, currency)}/mo
                  </span>
                ) : null}
              </div>
              <div className="overflow-hidden rounded-lg border">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b bg-muted/40 text-xs text-muted-foreground">
                      <th className="px-3 py-1.5 text-left font-medium">Monthly commit</th>
                      <th className="px-3 py-1.5 text-right font-medium">No upfront</th>
                      <th className="px-3 py-1.5 text-right font-medium">Paid upfront</th>
                    </tr>
                  </thead>
                  <tbody>
                    {tierRows(s).map((row) => (
                      <tr key={row.start} className="border-b last:border-0">
                        <td className="px-3 py-1.5 font-mono tabular-nums">{fmtMoneyTight(row.start, currency)}+</td>
                        <td className="px-3 py-1.5 text-right font-mono tabular-nums">{pct(row.noUpfront)}</td>
                        <td className="px-3 py-1.5 text-right font-mono font-medium text-primary-text tabular-nums">
                          {pct(row.upfront)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          ))
        ) : (
          <p className="text-sm text-muted-foreground">No discount schedule published.</p>
        )}
      </CardContent>
    </Card>
  )
}

// Purchase: eligibility is checked first (GET .../{planId}/eligible → boolean), then
// POST /savings-contracts/{bp} {savingsPlanId, durationMonths, monthlyCommittedAmount, paidUpfront, startDate}.
function PurchaseDialog({
  bp, plan, currency, onOpenChange,
}: {
  bp: string
  plan: SavingsPlan
  currency: string
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const schedules = plan.savingSchedule ?? []
  const [duration, setDuration] = useState(schedules[0] ? String(schedules[0].durationMonths) : "")
  const [amount, setAmount] = useState("")
  const [upfront, setUpfront] = useState(false)
  const [start, setStart] = useState("CURRENT_MONTH")

  const { data: eligible, isLoading: eligibleLoading } = useQuery({
    queryKey: ["savings-eligible", bp, plan.id],
    queryFn: () => apiFetch<boolean>(`/savings-contracts/${bp}/${plan.id}/eligible`),
  })

  const purchase = useMutation({
    mutationFn: () =>
      apiFetch(`/savings-contracts/${bp}`, {
        method: "POST",
        body: {
          savingsPlanId: plan.id,
          durationMonths: Number(duration),
          monthlyCommittedAmount: Number(amount),
          paidUpfront: upfront,
          startDate: start,
        },
      }),
    onSuccess: () => {
      toast.success(`Savings contract created for ${plan.name ?? "plan"}`)
      void qc.invalidateQueries({ queryKey: ["savings-contracts", bp] })
      onOpenChange(false)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const amountNum = Number(amount)
  const invalid = !duration || !Number.isFinite(amountNum) || amountNum <= 0

  // Preview the tier the entered commitment would land in for the chosen term.
  const schedule = schedules.find((s) => String(s.durationMonths) === duration)
  const ladder = (upfront ? schedule?.upfrontTiers : schedule?.noUpfrontTiers) ?? []
  const matchedTier = [...ladder]
    .sort((a, b) => Number(a.startAmount ?? 0) - Number(b.startAmount ?? 0))
    .filter((t) => Number.isFinite(amountNum) && amountNum >= Number(t.startAmount ?? 0))
    .pop()

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Purchase {plan.name ?? "savings plan"}</DialogTitle>
          <DialogDescription>
            Commit to a monthly amount — matching usage gets the tier discount on each bill.
          </DialogDescription>
        </DialogHeader>

        {eligible === false ? (
          <p className="text-sm text-destructive">
            This billing profile already has an active contract for this plan.
          </p>
        ) : null}

        <div className="space-y-4">
          <div>
            <Label className="mb-1.5 block" htmlFor="sp-duration">Duration</Label>
            <Select value={duration} onValueChange={setDuration}>
              <SelectTrigger id="sp-duration" className="w-full">
                <SelectValue placeholder="Select duration" />
              </SelectTrigger>
              <SelectContent>
                {schedules.map((s) => (
                  <SelectItem key={s.durationMonths} value={String(s.durationMonths)}>
                    {s.durationMonths} months
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div>
            <Label className="mb-1.5 block" htmlFor="sp-amount">Monthly committed amount</Label>
            <div className="relative">
              <Input
                id="sp-amount"
                className="h-11 pr-14 font-mono text-lg tabular-nums md:text-lg"
                type="number"
                inputMode="decimal"
                min={0}
                placeholder="0.00"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
              />
              <span className="pointer-events-none absolute top-1/2 right-3 -translate-y-1/2 font-mono text-xs text-muted-foreground">
                {currency}/mo
              </span>
            </div>
            {matchedTier ? (
              <p className="mt-1.5 text-xs text-muted-foreground">
                Qualifies for the <span className="font-medium text-primary-text">{pct(matchedTier.discount)}</span>{" "}
                {upfront ? "upfront" : "no-upfront"} tier.
              </p>
            ) : null}
          </div>
          <div>
            <Label className="mb-1.5 block" htmlFor="sp-start">Starts</Label>
            <Select value={start} onValueChange={setStart}>
              <SelectTrigger id="sp-start" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="CURRENT_MONTH">This month</SelectItem>
                <SelectItem value="NEXT_MONTH">Next month</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="flex items-center gap-2">
            <Checkbox id="upfront" checked={upfront} onCheckedChange={(v) => setUpfront(v === true)} />
            <Label htmlFor="upfront">Pay upfront (upfront tier discounts; cannot be cancelled)</Label>
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button
            onClick={() => purchase.mutate()}
            disabled={invalid || purchase.isPending || eligibleLoading || eligible === false}
          >
            {purchase.isPending ? "Purchasing…" : "Purchase"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ContractsSection({ bp, currency }: { bp: string; currency: string }) {
  const qc = useQueryClient()
  const [cancelling, setCancelling] = useState<SavingsContract | null>(null)
  const [extending, setExtending] = useState<SavingsContract | null>(null)

  const { data: contracts, isLoading, error } = useQuery({
    queryKey: ["savings-contracts", bp],
    queryFn: () => apiFetch<SavingsContract[]>(`/savings-contracts/${bp}`),
  })

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["savings-contracts", bp] })

  const cancel = useMutation({
    // DELETE /savings-contracts/{bp}/{contractId} — non-upfront ACTIVE contracts only.
    mutationFn: (id: string) => apiFetch(`/savings-contracts/${bp}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Contract cancelled")
      setCancelling(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const extend = useMutation({
    // POST .../extend — pushes endDate out by the contract's own duration.
    mutationFn: (id: string) => apiFetch(`/savings-contracts/${bp}/${id}/extend`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Contract extended")
      setExtending(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<SavingsContract, any>[]>(
    () => [
      {
        id: "plan",
        accessorFn: (c) => c.savingsPlanName ?? c.savingsPlanId ?? "",
        header: sortableHeader("Plan"),
        cell: ({ row }) => {
          const c = row.original
          const active = c.status === "ACTIVE"
          return (
            <span className={cn("font-medium", !active && "text-muted-foreground")}>
              {c.savingsPlanName ?? c.savingsPlanId ?? "—"}
              {c.paidUpfront ? <Badge variant="secondary" className="ml-2">Upfront</Badge> : null}
            </span>
          )
        },
      },
      {
        id: "committed",
        accessorFn: (c) => Number(c.monthlyCommittedAmount ?? 0),
        header: sortableRightHeader("Committed / month"),
        cell: ({ row }) => (
          <div className="text-right font-mono tabular-nums">
            {fmtMoney(row.original.monthlyCommittedAmount, currency)}
          </div>
        ),
      },
      {
        id: "discount",
        accessorFn: (c) => Number(c.discountRate ?? 0),
        header: sortableRightHeader("Discount"),
        cell: ({ row }) => (
          <div className="text-right font-mono tabular-nums">{pct(row.original.discountRate)}</div>
        ),
      },
      {
        id: "status",
        accessorFn: (c) => c.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "term",
        accessorFn: (c) => c.startDate ?? "",
        header: sortableHeader("Term"),
        cell: ({ row }) => (
          <span className="text-sm text-muted-foreground tabular-nums">
            {fmtDate(row.original.startDate)} → {fmtDate(row.original.endDate)}
          </span>
        ),
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const c = row.original
          const active = c.status === "ACTIVE"
          // Ended contracts have no actions — no point rendering an empty menu.
          if (!active) return null
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={`Actions for ${c.savingsPlanName ?? c.savingsPlanId ?? "contract"}`}
                  >
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => setExtending(c)}>
                    <CalendarPlus className="size-4" /> Extend
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    variant="destructive"
                    onClick={() => setCancelling(c)}
                    disabled={c.paidUpfront === true}
                  >
                    <XCircle className="size-4" /> Cancel contract
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    [currency],
  )

  return (
    <div className="mt-8">
      <h2 className="mb-3 font-display text-lg font-semibold">Your contracts</h2>
      {!isLoading && !error && !contracts?.length ? (
        <EmptyState icon={PiggyBank} title="No savings contracts" hint="Purchase a plan above to start saving." />
      ) : (
        <DataTable
          columns={columns}
          data={contracts}
          isLoading={isLoading}
          error={error as Error | null}
          getRowId={(c) => c.id}
        />
      )}

      <Dialog open={!!cancelling} onOpenChange={(o) => !o && setCancelling(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Cancel contract</DialogTitle>
            <DialogDescription>
              Cancel the {cancelling?.savingsPlanName ?? ""} contract? Its discount stops applying to future bills.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCancelling(null)}>Keep contract</Button>
            <Button
              variant="destructive"
              onClick={() => cancelling && cancel.mutate(cancelling.id)}
              disabled={cancel.isPending}
            >
              {cancel.isPending ? "Cancelling…" : "Cancel contract"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!extending} onOpenChange={(o) => !o && setExtending(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Extend contract</DialogTitle>
            <DialogDescription>
              Extend the {extending?.savingsPlanName ?? ""} contract by {extending?.durationMonths ?? "—"} months
              (new end date {extending?.endDate ? fmtDate(extending.endDate) : "—"} + {extending?.durationMonths ?? 0} months)?
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setExtending(null)}>Cancel</Button>
            <Button onClick={() => extending && extend.mutate(extending.id)} disabled={extend.isPending}>
              {extend.isPending ? "Extending…" : "Extend contract"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}


function ErrorPanel({ message }: { message: string }) {
  return <div className="rounded-md border bg-muted/40 p-4 text-sm text-muted-foreground">{message}</div>
}
