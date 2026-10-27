import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { Column, ColumnDef } from "@tanstack/react-table"
import { MoreHorizontal, PiggyBank, Plus, RefreshCw, ScrollText, Trash2 } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { fmtDate, fmtMoney } from "@/lib/format"
import { useAdminList } from "@/lib/hooks"

type Tier = { startAmount?: number | string; discount?: number | string }
type Schedule = {
  durationMonths?: number
  maxAmount?: number | string
  noUpfrontTiers?: Tier[]
  upfrontTiers?: Tier[]
}
type SavingsPlan = {
  id: string
  name?: string
  available?: boolean
  description?: string
  accessMode?: string
  targets?: Array<{ resourceType?: string }>
  savingSchedule?: Schedule[]
}
type SavingsContract = {
  id: string
  billingProfileId?: string
  savingsPlanId?: string
  savingsPlanName?: string
  status?: string
  durationMonths?: number
  monthlyCommittedAmount?: number | string
  startDate?: string
  endDate?: string
  billingProfile?: { email?: string; fullName?: string }
}
type ResourceType = { resourceType: string }
type BillingProfile = { id: string; email?: string; fullName?: string; firstName?: string; lastName?: string }

const PLANS_PATH = "/admin/savings-plans"
const CONTRACTS_PATH = "/admin/savings-contracts"

function bpLabel(bp: BillingProfile): string {
  return bp.email ?? bp.fullName ?? [bp.firstName, bp.lastName].filter(Boolean).join(" ") ?? bp.id
}

// One line per schedule: "12 mo: up to 10%" (max discount across both tier sets).
function tiersSummary(p: SavingsPlan): string {
  if (!p.savingSchedule?.length) return "—"
  return p.savingSchedule
    .map((s) => {
      const all = [...(s.noUpfrontTiers ?? []), ...(s.upfrontTiers ?? [])]
      const max = all.reduce((m, t) => Math.max(m, Number(t.discount ?? 0) || 0), 0)
      return `${s.durationMonths ?? "?"} mo: up to ${max}%`
    })
    .join(" · ")
}

function targetsSummary(p: SavingsPlan): string {
  const types = (p.targets ?? []).map((t) => t.resourceType).filter(Boolean)
  return types.length ? types.join(", ") : "—"
}

// ── Create plan dialog ────────────────────────────────────────────────────────
// POST /admin/savings-plans — body mirrors the Go savingsPlanReq (savingsplan.go):
// {name, available, description, accessMode, targets:[{resourceType}],
//  savingSchedule:[{durationMonths, maxAmount?, noUpfrontTiers|upfrontTiers:[{startAmount, discount}]}]}

type TierForm = { startAmount: string; discount: string }

function CreatePlanDialog({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const { data: typesEnv } = useAdminList<ResourceType>("/admin/price-plan/resource-types")
  const resourceTypes = typesEnv?.data ?? []

  const [name, setName] = useState("")
  const [description, setDescription] = useState("")
  const [accessMode, setAccessMode] = useState("PUBLIC")
  const [available, setAvailable] = useState(true)
  const [targets, setTargets] = useState<string[]>([])
  // One schedule (add more via the API if ever needed).
  const [durationMonths, setDurationMonths] = useState("12")
  const [maxAmount, setMaxAmount] = useState("")
  const [paidUpfront, setPaidUpfront] = useState(false)
  const [tiers, setTiers] = useState<TierForm[]>([{ startAmount: "", discount: "" }])

  const setTier = (i: number, patch: Partial<TierForm>) =>
    setTiers((ts) => ts.map((t, j) => (j === i ? { ...t, ...patch } : t)))

  const create = useMutation({
    mutationFn: () => {
      const tierDocs = tiers.map((t) => ({ startAmount: parseFloat(t.startAmount), discount: parseFloat(t.discount) }))
      const schedule: Record<string, unknown> = {
        durationMonths: parseInt(durationMonths, 10),
        // paidUpfront on a contract selects the plan's upfrontTiers — store the tiers on the
        // matching side of the schedule (Go SavingsPlanSchedule has no paidUpfront field).
        [paidUpfront ? "upfrontTiers" : "noUpfrontTiers"]: tierDocs,
      }
      if (maxAmount !== "" && !Number.isNaN(parseFloat(maxAmount))) schedule.maxAmount = parseFloat(maxAmount)
      const body: Record<string, unknown> = {
        name,
        available,
        accessMode,
        targets: targets.map((rt) => ({ resourceType: rt })),
        savingSchedule: [schedule],
      }
      if (description.trim()) body.description = description.trim()
      return apiFetch(PLANS_PATH, { method: "POST", body })
    },
    onSuccess: () => {
      toast.success("Savings plan created")
      onSaved()
      onClose()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const tiersOk =
    tiers.length > 0 &&
    tiers.every(
      (t) =>
        t.startAmount !== "" &&
        !Number.isNaN(parseFloat(t.startAmount)) &&
        t.discount !== "" &&
        !Number.isNaN(parseFloat(t.discount))
    )
  const canSubmit =
    name.trim() && targets.length > 0 && durationMonths !== "" && parseInt(durationMonths, 10) > 0 && tiersOk

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] max-w-lg overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Create savings plan</DialogTitle>
          <DialogDescription>
            Commitment discount: clients commit to a monthly amount and earn the matching tier's discount.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="sp-name">Name</Label>
              <Input id="sp-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="Compute savings" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="sp-access">Access mode</Label>
              <Select value={accessMode} onValueChange={setAccessMode}>
                <SelectTrigger id="sp-access">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="PUBLIC">PUBLIC — everyone</SelectItem>
                  <SelectItem value="SCOPED">SCOPED — per profile</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="sp-desc">Description (optional)</Label>
            <Textarea id="sp-desc" value={description} onChange={(e) => setDescription(e.target.value)} rows={2} />
          </div>
          <div className="flex items-center gap-3">
            <Switch id="sp-available" checked={available} onCheckedChange={setAvailable} />
            <Label htmlFor="sp-available">Available for new contracts</Label>
          </div>

          <div className="grid gap-2">
            <Label>Target resource types</Label>
            <div className="flex flex-wrap gap-3">
              {resourceTypes.map((t) => (
                <label key={t.resourceType} className="flex items-center gap-1.5 text-sm">
                  <Checkbox
                    checked={targets.includes(t.resourceType)}
                    onCheckedChange={(v) =>
                      setTargets((ts) =>
                        v === true ? [...ts, t.resourceType] : ts.filter((x) => x !== t.resourceType)
                      )
                    }
                  />
                  {t.resourceType}
                </label>
              ))}
            </div>
          </div>

          <Separator />
          <p className="text-eyebrow">Saving schedule</p>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="sp-duration">Duration (months)</Label>
              <Input
                id="sp-duration"
                type="number"
                min="1"
                step="1"
                value={durationMonths}
                onChange={(e) => setDurationMonths(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="sp-max">Max amount (optional)</Label>
              <Input
                id="sp-max"
                type="number"
                min="0"
                step="any"
                value={maxAmount}
                onChange={(e) => setMaxAmount(e.target.value)}
              />
            </div>
          </div>
          <div className="flex items-center gap-3">
            <Checkbox
              id="sp-upfront"
              checked={paidUpfront}
              onCheckedChange={(v) => setPaidUpfront(v === true)}
            />
            <Label htmlFor="sp-upfront">Tiers apply to upfront-paid contracts</Label>
          </div>
          <div className="grid gap-2">
            <Label>Discount tiers — committed amount from, discount %</Label>
            {tiers.map((t, i) => (
              <div key={i} className="flex items-center gap-2">
                <Input
                  type="number"
                  step="any"
                  min="0.01"
                  className="w-36"
                  placeholder="From amount (0.01+)"
                  title="Start amount — must be non-zero"
                  aria-label={`Tier ${i + 1} start amount`}
                  value={t.startAmount}
                  onChange={(e) => setTier(i, { startAmount: e.target.value })}
                />
                <Input
                  type="number"
                  step="any"
                  min="0"
                  className="w-28"
                  placeholder="Discount %"
                  title="Discount percent"
                  aria-label={`Tier ${i + 1} discount percent`}
                  value={t.discount}
                  onChange={(e) => setTier(i, { discount: e.target.value })}
                />
                <Button
                  variant="ghost"
                  size="icon-sm"
                  aria-label={`Remove tier ${i + 1}`}
                  disabled={tiers.length === 1}
                  onClick={() => setTiers((ts) => ts.filter((_, j) => j !== i))}
                >
                  <Trash2 className="size-4 text-muted-foreground" />
                </Button>
              </div>
            ))}
            <Button
              variant="outline"
              size="sm"
              className="w-fit"
              onClick={() => setTiers((ts) => [...ts, { startAmount: "", discount: "" }])}
            >
              <Plus className="size-4" /> Add tier
            </Button>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => create.mutate()} disabled={!canSubmit || create.isPending}>
            Create savings plan
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ── Plan detail sheet ─────────────────────────────────────────────────────────

function TierList({ label, tiers }: { label: string; tiers?: Tier[] }) {
  if (!tiers?.length) return null
  return (
    <div>
      <p className="text-xs font-medium text-muted-foreground">{label}</p>
      <ul className="mt-1 space-y-0.5 font-mono text-sm tabular-nums">
        {tiers.map((t, i) => (
          <li key={i}>
            ≥ {fmtMoney(t.startAmount)} → {String(t.discount ?? 0)}%
          </li>
        ))}
      </ul>
    </div>
  )
}

function PlanDetailSheet({ plan, onClose }: { plan: SavingsPlan; onClose: () => void }) {
  // GET /admin/savings-contracts/savings-plan/{id} — contracts enriched with billingProfile.
  const contractsQ = useAdminList<SavingsContract>(`${CONTRACTS_PATH}/savings-plan/${plan.id}`)
  const contracts = contractsQ.data?.data ?? []

  return (
    <Sheet open onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="w-full overflow-y-auto sm:max-w-xl">
        <SheetHeader>
          <SheetTitle>{plan.name ?? plan.id}</SheetTitle>
          <SheetDescription>{plan.description || "Savings plan details and contracts."}</SheetDescription>
        </SheetHeader>
        <div className="space-y-6 px-4 pb-6">
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant={plan.accessMode === "SCOPED" ? "secondary" : "outline"}>{plan.accessMode ?? "PUBLIC"}</Badge>
            <StatusBadge status={plan.available ? "AVAILABLE" : "DISABLED"} />
            <span className="font-mono text-xs text-muted-foreground">{plan.id}</span>
          </div>

          <div>
            <p className="text-eyebrow">Targets</p>
            <p className="mt-1 text-sm text-muted-foreground">{targetsSummary(plan)}</p>
          </div>

          <div className="space-y-3">
            <p className="text-eyebrow">Saving schedule</p>
            {(plan.savingSchedule ?? []).map((s, i) => (
              <Card key={i} className="gap-2 p-3">
                <p className="text-sm">
                  {s.durationMonths ?? "?"} months
                  {s.maxAmount != null ? (
                    <span className="text-muted-foreground"> · max {fmtMoney(s.maxAmount)}</span>
                  ) : null}
                </p>
                <div className="grid grid-cols-2 gap-3">
                  <TierList label="No upfront" tiers={s.noUpfrontTiers} />
                  <TierList label="Paid upfront" tiers={s.upfrontTiers} />
                </div>
              </Card>
            ))}
            {!plan.savingSchedule?.length ? <p className="text-sm text-muted-foreground">No schedules.</p> : null}
          </div>

          <div className="space-y-2">
            <p className="text-eyebrow">Contracts on this plan</p>
            {contractsQ.isLoading ? (
              <Skeleton className="h-24" />
            ) : contractsQ.error ? (
              <p className="text-sm text-muted-foreground">{(contractsQ.error as Error).message}</p>
            ) : !contracts.length ? (
              <p className="text-sm text-muted-foreground">No contracts yet.</p>
            ) : (
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Billing profile</TableHead>
                      <TableHead className="text-right">Committed</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>End</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {contracts.map((c) => (
                      <TableRow key={c.id}>
                        <TableCell className="text-sm">
                          {c.billingProfile?.email ?? c.billingProfile?.fullName ?? (
                            <span className="font-mono">{c.billingProfileId ?? "—"}</span>
                          )}
                        </TableCell>
                        <TableCell className="text-right font-mono text-sm tabular-nums">{fmtMoney(c.monthlyCommittedAmount)}</TableCell>
                        <TableCell>
                          <StatusBadge status={c.status} />
                        </TableCell>
                        <TableCell className="text-sm text-muted-foreground">{fmtDate(c.endDate)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </Card>
            )}
          </div>
        </div>
      </SheetContent>
    </Sheet>
  )
}

// ── Create contract dialog ────────────────────────────────────────────────────
// POST /admin/savings-contracts/{billingProfileId} — body mirrors createSavingsContractReq
// (savingscontract.go): {savingsPlanId, durationMonths, monthlyCommittedAmount, paidUpfront,
// startDate: CURRENT_MONTH | NEXT_MONTH}.

function CreateContractDialog({ plans, onClose, onSaved }: { plans: SavingsPlan[]; onClose: () => void; onSaved: () => void }) {
  const profilesQ = useAdminList<BillingProfile>("/admin/billing-profile")
  const profiles = profilesQ.data?.data ?? []

  const [bpId, setBpId] = useState("")
  const [planId, setPlanId] = useState("")
  const [duration, setDuration] = useState("")
  const [amount, setAmount] = useState("")
  const [paidUpfront, setPaidUpfront] = useState(false)
  const [startDate, setStartDate] = useState("CURRENT_MONTH")

  const selectedPlan = plans.find((p) => p.id === planId)
  const durations = (selectedPlan?.savingSchedule ?? [])
    .map((s) => s.durationMonths)
    .filter((d): d is number => typeof d === "number")

  const create = useMutation({
    mutationFn: () =>
      apiFetch(`${CONTRACTS_PATH}/${bpId}`, {
        method: "POST",
        body: {
          savingsPlanId: planId,
          durationMonths: parseInt(duration, 10),
          monthlyCommittedAmount: parseFloat(amount),
          paidUpfront,
          startDate,
        },
      }),
    onSuccess: () => {
      toast.success("Savings contract created")
      onSaved()
      onClose()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const canSubmit = bpId && planId && duration && amount !== "" && !Number.isNaN(parseFloat(amount))

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create savings contract</DialogTitle>
          <DialogDescription>
            Commits a billing profile to a monthly amount — the discount rate comes from the plan's matching tier.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="sc-profile">Billing profile</Label>
            <Select value={bpId} onValueChange={setBpId}>
              <SelectTrigger id="sc-profile">
                <SelectValue placeholder={profilesQ.isLoading ? "Loading billing profiles…" : "Select a billing profile"} />
              </SelectTrigger>
              <SelectContent>
                {profiles.map((bp) => (
                  <SelectItem key={bp.id} value={bp.id}>
                    {bpLabel(bp)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="sc-plan">Savings plan</Label>
            <Select
              value={planId}
              onValueChange={(v) => {
                setPlanId(v)
                setDuration("")
              }}
            >
              <SelectTrigger id="sc-plan">
                <SelectValue placeholder="Select a plan" />
              </SelectTrigger>
              <SelectContent>
                {plans
                  .filter((p) => p.available)
                  .map((p) => (
                    <SelectItem key={p.id} value={p.id}>
                      {p.name ?? p.id}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="sc-duration">Duration</Label>
              <Select value={duration} onValueChange={setDuration} disabled={!selectedPlan}>
                <SelectTrigger id="sc-duration">
                  <SelectValue placeholder={selectedPlan ? "Select duration" : "Pick a plan first"} />
                </SelectTrigger>
                <SelectContent>
                  {durations.map((d) => (
                    <SelectItem key={d} value={String(d)}>
                      {d} months
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="sc-amount">Committed / month</Label>
              <Input
                id="sc-amount"
                type="number"
                min="0"
                step="any"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
                placeholder="100"
              />
            </div>
          </div>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="sc-start">Starts</Label>
              <Select value={startDate} onValueChange={setStartDate}>
                <SelectTrigger id="sc-start">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="CURRENT_MONTH">Current month</SelectItem>
                  <SelectItem value="NEXT_MONTH">Next month</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="flex items-end gap-3 pb-2">
              <Checkbox id="sc-upfront" checked={paidUpfront} onCheckedChange={(v) => setPaidUpfront(v === true)} />
              <Label htmlFor="sc-upfront">Paid upfront</Label>
            </div>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => create.mutate()} disabled={!canSubmit || create.isPending}>
            Create contract
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ── Page ──────────────────────────────────────────────────────────────────────

export default function SavingsPlansPage() {
  const qc = useQueryClient()
  const plansQ = useAdminList<SavingsPlan>(PLANS_PATH)
  const contractsQ = useAdminList<SavingsContract>(CONTRACTS_PATH)
  const plans = plansQ.data?.data ?? []
  const contracts = contractsQ.data?.data ?? []

  const invalidatePlans = () => qc.invalidateQueries({ queryKey: ["admin-list", PLANS_PATH] })
  const invalidateContracts = () => qc.invalidateQueries({ queryKey: ["admin-list", CONTRACTS_PATH] })

  const [createOpen, setCreateOpen] = useState(false)
  const [viewPlan, setViewPlan] = useState<SavingsPlan | null>(null)
  const [toDelete, setToDelete] = useState<SavingsPlan | null>(null)
  const [contractOpen, setContractOpen] = useState(false)

  const deletePlan = useMutation({
    mutationFn: (id: string) => apiFetch(`${PLANS_PATH}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Savings plan deleted")
      setToDelete(null)
      void invalidatePlans()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const planColumns = useMemo<ColumnDef<SavingsPlan, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (p) => p.name ?? p.id,
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "tiers",
        accessorFn: (p) => tiersSummary(p),
        header: "Discount tiers",
        enableSorting: false,
        cell: ({ getValue }) => <span className="text-sm tabular-nums">{getValue()}</span>,
      },
      {
        id: "targets",
        accessorFn: (p) => targetsSummary(p),
        header: "Target resource types",
        enableSorting: false,
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue()}</span>,
      },
      {
        id: "access",
        accessorFn: (p) => p.accessMode ?? "PUBLIC",
        header: sortableHeader("Access"),
        cell: ({ getValue }) => (
          <Badge variant={getValue() === "SCOPED" ? "secondary" : "outline"}>{getValue()}</Badge>
        ),
      },
      {
        id: "available",
        accessorFn: (p) => !!p.available,
        header: sortableHeader("Available"),
        cell: ({ row }) => <StatusBadge status={row.original.available ? "AVAILABLE" : "DISABLED"} />,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const p = row.original
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${p.name ?? p.id}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => setViewPlan(p)}>View details</DropdownMenuItem>
                  <DropdownMenuItem className="text-destructive" onClick={() => setToDelete(p)}>
                    Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // useState setters are stable; summary helpers are module-scope.
    []
  )

  const contractColumns = useMemo<ColumnDef<SavingsContract, any>[]>(
    () => [
      {
        id: "billingProfile",
        accessorFn: (c) => c.billingProfileId ?? "",
        header: sortableHeader("Billing profile"),
        cell: ({ getValue }) => <span className="font-mono text-sm">{getValue() || "—"}</span>,
      },
      {
        id: "plan",
        accessorFn: (c) => c.savingsPlanName ?? c.savingsPlanId ?? "",
        header: sortableHeader("Plan"),
        cell: ({ getValue }) => <span className="font-medium">{getValue() || "—"}</span>,
      },
      {
        id: "committed",
        accessorFn: (c) => Number(c.monthlyCommittedAmount ?? 0),
        header: sortableRightHeader("Committed / month"),
        cell: ({ row }) => (
          <div className="text-right font-mono text-sm tabular-nums">{fmtMoney(row.original.monthlyCommittedAmount)}</div>
        ),
      },
      {
        id: "status",
        accessorFn: (c) => c.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "start",
        accessorFn: (c) => c.startDate ?? "",
        header: sortableHeader("Start"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDate(getValue())}</span>,
      },
      {
        id: "end",
        accessorFn: (c) => c.endDate ?? "",
        header: sortableHeader("End"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDate(getValue())}</span>,
      },
    ],
    []
  )

  return (
    <>
      <PageHeader
        title="Savings plans"
        eyebrow="System"
        description="Commitment discounts and the contracts sold against them."
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                void plansQ.refetch()
                void contractsQ.refetch()
              }}
              disabled={plansQ.isFetching || contractsQ.isFetching}
              aria-label="Refresh"
            >
              <RefreshCw className={plansQ.isFetching || contractsQ.isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create savings plan
            </Button>
          </>
        }
      />

      <Tabs defaultValue="plans">
        <TabsList>
          <TabsTrigger value="plans">Plans</TabsTrigger>
          <TabsTrigger value="contracts">Contracts</TabsTrigger>
        </TabsList>

        <TabsContent value="plans" className="mt-4">
          {!plansQ.isLoading && !plansQ.error && !plans.length ? (
            <EmptyState
              icon={PiggyBank}
              title="No savings plans"
              hint="Create a plan clients can commit to for a discount."
              action={
                <Button onClick={() => setCreateOpen(true)}>
                  <Plus className="size-4" /> Create savings plan
                </Button>
              }
            />
          ) : (
            <DataTable
              columns={planColumns}
              data={plans}
              isLoading={plansQ.isLoading}
              error={plansQ.error ? (plansQ.error as Error) : null}
              searchPlaceholder="Search savings plans…"
              getRowId={(p) => p.id}
              onRowClick={(p) => setViewPlan(p)}
            />
          )}
        </TabsContent>

        <TabsContent value="contracts" className="mt-4">
          <div className="mb-4 flex justify-end">
            <Button size="sm" onClick={() => setContractOpen(true)} disabled={!plans.length}>
              <Plus className="size-4" /> Create contract
            </Button>
          </div>
          {!contractsQ.isLoading && !contractsQ.error && !contracts.length ? (
            <EmptyState
              icon={ScrollText}
              title="No savings contracts"
              hint="Contracts appear when clients commit to a plan — or create one for a billing profile here."
              action={
                plans.length ? (
                  <Button onClick={() => setContractOpen(true)}>
                    <Plus className="size-4" /> Create contract
                  </Button>
                ) : undefined
              }
            />
          ) : (
            <DataTable
              columns={contractColumns}
              data={contracts}
              isLoading={contractsQ.isLoading}
              error={contractsQ.error ? (contractsQ.error as Error) : null}
              searchPlaceholder="Search contracts…"
              getRowId={(c) => c.id}
            />
          )}
        </TabsContent>
      </Tabs>

      {createOpen ? <CreatePlanDialog onClose={() => setCreateOpen(false)} onSaved={() => void invalidatePlans()} /> : null}
      {viewPlan ? <PlanDetailSheet plan={viewPlan} onClose={() => setViewPlan(null)} /> : null}
      {contractOpen ? (
        <CreateContractDialog
          plans={plans}
          onClose={() => setContractOpen(false)}
          onSaved={() => void invalidateContracts()}
        />
      ) : null}

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete savings plan</DialogTitle>
            <DialogDescription>Delete "{toDelete?.name}"? Existing contracts keep their terms.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => toDelete && deletePlan.mutate(toDelete.id)}
              disabled={deletePlan.isPending}
            >
              Delete savings plan
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// Right-aligned sortable header for money columns (same local helper as the
// client billing pages — keeps the ghost-button sort affordance flush right).
function sortableRightHeader<TData>(label: string) {
  const Inner = sortableHeader<TData>(label)
  return function SortableRightHeader({ column }: { column: Column<TData, unknown> }) {
    return (
      <div className="text-right">
        <Inner column={column} />
      </div>
    )
  }
}
