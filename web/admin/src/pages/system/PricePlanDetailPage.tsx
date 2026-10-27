import { useMemo, useState } from "react"
import { Link, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { BarChart3, MoreHorizontal, Pencil, Plus, Ruler, SlidersHorizontal, Trash2 } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
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
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { fmtMoney } from "@/lib/format"
import { useAdminGet, useAdminList } from "@/lib/hooks"

type PricePlan = {
  id: string
  name?: string
  enabled?: boolean
  accessMode?: string
  serviceProviders?: Array<{ serviceId?: string }>
}

type PriceTier = { from?: number | string; to?: number | string; value?: number | string }
type RulePrice = { attributeName?: string; tiers?: PriceTier[] }
type Rule = {
  id: string
  name?: string
  resourceType?: string
  timeUnit?: string
  applyMethod?: string
  prices?: RulePrice[]
  filters?: unknown
  modifiers?: unknown
}

type ResourceType = {
  resourceType: string
  attributes?: Array<{ type: string; name: string; isUsage?: boolean }>
}

// Price adjustment rule — mirrors the Go priceAdjustmentRule DTO
// (priceadjustmentrule_repo.go): tiers[{startAmount, modifier:{operator add|subtract,
// asPercentage, value}}].
type AdjModifier = { operator?: string; asPercentage?: boolean; value?: number | string }
type AdjTier = { startAmount?: number | string; modifier?: AdjModifier }
type AdjRule = {
  id: string
  name?: string
  enabled?: boolean
  description?: string
  pricePlanId?: string
  targets?: unknown
  tiers?: AdjTier[]
}

const TIME_UNITS = ["minute", "hour", "month"]

function priceBasis(rule: Rule): string {
  if (!rule.prices?.length) return "—"
  return rule.prices
    .map((p) => {
      const v = p.tiers?.[0]?.value
      return `${p.attributeName ?? "?"} @ ${v ?? "—"}`
    })
    .join(", ")
}

function adjTiersSummary(rule: AdjRule): string {
  if (!rule.tiers?.length) return "—"
  return rule.tiers
    .map((t) => {
      const m = t.modifier
      const sign = m?.operator === "subtract" ? "−" : "+"
      return `≥ ${t.startAmount ?? 0}: ${sign}${m?.value ?? 0}${m?.asPercentage ? "%" : ""}`
    })
    .join(" · ")
}

// ── Usage dialogs ─────────────────────────────────────────────────────────────

function UsageDialog({ ruleId, ruleName, onClose }: { ruleId: string; ruleName?: string; onClose: () => void }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ["rule-usage", ruleId],
    queryFn: () =>
      apiFetch<{ openBillsCount?: number; totalAppliedAmount?: number | string }>(
        `/admin/price-plan/rule/${ruleId}/usage`
      ),
  })
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Rule usage</DialogTitle>
          <DialogDescription>{ruleName}</DialogDescription>
        </DialogHeader>
        {isLoading ? (
          <Skeleton className="h-16" />
        ) : error ? (
          <div className="rounded-lg border bg-muted/40 p-3 text-sm text-muted-foreground">{(error as Error).message}</div>
        ) : (
          <div className="grid grid-cols-2 gap-4">
            <div>
              <p className="text-sm text-muted-foreground">Open bills</p>
              <p className="font-display text-2xl font-semibold tabular-nums">{data?.openBillsCount ?? 0}</p>
            </div>
            <div>
              <p className="text-sm text-muted-foreground">Total applied</p>
              <p className="font-display text-2xl font-semibold tabular-nums">{fmtMoney(data?.totalAppliedAmount)}</p>
            </div>
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}

// GET /admin/price-adjustment-rules/{id}/usage → {openBillsCount, totalAdjustmentsAmount}
function AdjUsageDialog({ ruleId, ruleName, onClose }: { ruleId: string; ruleName?: string; onClose: () => void }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ["adj-rule-usage", ruleId],
    queryFn: () =>
      apiFetch<{ openBillsCount?: number; totalAdjustmentsAmount?: number | string }>(
        `/admin/price-adjustment-rules/${ruleId}/usage`
      ),
  })
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Adjustment rule usage</DialogTitle>
          <DialogDescription>{ruleName}</DialogDescription>
        </DialogHeader>
        {isLoading ? (
          <Skeleton className="h-16" />
        ) : error ? (
          <div className="rounded-lg border bg-muted/40 p-3 text-sm text-muted-foreground">{(error as Error).message}</div>
        ) : (
          <div className="grid grid-cols-2 gap-4">
            <div>
              <p className="text-sm text-muted-foreground">Open bills</p>
              <p className="font-display text-2xl font-semibold tabular-nums">{data?.openBillsCount ?? 0}</p>
            </div>
            <div>
              <p className="text-sm text-muted-foreground">Total adjustments</p>
              <p className="font-display text-2xl font-semibold tabular-nums">{fmtMoney(data?.totalAdjustmentsAmount)}</p>
            </div>
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}

// ── Rule edit dialog ──────────────────────────────────────────────────────────
// PUT /admin/price-plan/rule/{id} overwrites name/prices/filters/modifiers/resourceType/
// applyMethod/timeUnit and DROPS omitted keys — so unchanged fields are sent back as-is.
// Editable here: name, time unit, and the first-tier value of each price.

function EditRuleDialog({ rule, onClose, onSaved }: { rule: Rule; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(rule.name ?? "")
  const [timeUnit, setTimeUnit] = useState(rule.timeUnit ?? "hour")
  const [values, setValues] = useState<string[]>((rule.prices ?? []).map((p) => String(p.tiers?.[0]?.value ?? "")))

  const save = useMutation({
    mutationFn: () => {
      const prices = (rule.prices ?? []).map((p, i) => {
        const v = parseFloat(values[i])
        const tiers = p.tiers?.length
          ? [{ ...p.tiers[0], value: v }, ...p.tiers.slice(1)]
          : [{ value: v }]
        return { ...p, tiers }
      })
      return apiFetch(`/admin/price-plan/rule/${rule.id}`, {
        method: "PUT",
        body: {
          name,
          timeUnit,
          resourceType: rule.resourceType,
          applyMethod: rule.applyMethod,
          prices,
          filters: rule.filters,
          modifiers: rule.modifiers,
        },
      })
    },
    onSuccess: () => {
      toast.success("Rule updated")
      onSaved()
      onClose()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const valuesOk = values.every((v) => v !== "" && !Number.isNaN(parseFloat(v)))

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Edit rule</DialogTitle>
          <DialogDescription>
            Resource type ({rule.resourceType ?? "—"}), filters and modifiers are kept unchanged.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="er-name">Name</Label>
            <Input id="er-name" value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="er-unit">Time unit</Label>
            <Select value={timeUnit} onValueChange={setTimeUnit}>
              <SelectTrigger id="er-unit">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {TIME_UNITS.map((u) => (
                  <SelectItem key={u} value={u}>
                    {u}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {(rule.prices ?? []).map((p, i) => (
            <div key={i} className="grid gap-2">
              <Label htmlFor={`er-price-${i}`}>{p.attributeName ?? `Price ${i + 1}`} — first-tier price</Label>
              <Input
                id={`er-price-${i}`}
                type="number"
                step="any"
                value={values[i] ?? ""}
                onChange={(e) => setValues((vs) => vs.map((v, j) => (j === i ? e.target.value : v)))}
              />
            </div>
          ))}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => save.mutate()} disabled={!name.trim() || !valuesOk || save.isPending}>
            Save rule
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ── Price adjustment rule create/edit dialog ──────────────────────────────────
// POST /admin/price-adjustment-rules · PUT /admin/price-adjustment-rules/{id}
// body: {name, enabled, description, pricePlanId, tiers:[{startAmount,
// modifier:{operator: "add"|"subtract", asPercentage, value}}]} — pricePlanId is
// required by the validator on both create and update.

type AdjTierForm = { startAmount: string; operator: string; asPercentage: boolean; value: string }

function toTierForms(rule?: AdjRule): AdjTierForm[] {
  const tiers = rule?.tiers
  if (!tiers?.length) return [{ startAmount: "", operator: "add", asPercentage: true, value: "" }]
  return tiers.map((t) => ({
    startAmount: String(t.startAmount ?? ""),
    operator: t.modifier?.operator ?? "add",
    asPercentage: t.modifier?.asPercentage ?? false,
    value: String(t.modifier?.value ?? ""),
  }))
}

function AdjRuleDialog({
  planId,
  rule,
  onClose,
  onSaved,
}: {
  planId: string
  rule?: AdjRule
  onClose: () => void
  onSaved: () => void
}) {
  const [name, setName] = useState(rule?.name ?? "")
  const [description, setDescription] = useState(rule?.description ?? "")
  const [enabled, setEnabled] = useState(rule?.enabled ?? true)
  const [tiers, setTiers] = useState<AdjTierForm[]>(toTierForms(rule))

  const setTier = (i: number, patch: Partial<AdjTierForm>) =>
    setTiers((ts) => ts.map((t, j) => (j === i ? { ...t, ...patch } : t)))

  const save = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = {
        name,
        enabled,
        description,
        pricePlanId: planId,
        tiers: tiers.map((t) => ({
          startAmount: parseFloat(t.startAmount),
          modifier: { operator: t.operator, asPercentage: t.asPercentage, value: parseFloat(t.value) },
        })),
      }
      if (rule?.targets !== undefined) body.targets = rule.targets // preserved on edit
      return rule
        ? apiFetch(`/admin/price-adjustment-rules/${rule.id}`, { method: "PUT", body })
        : apiFetch("/admin/price-adjustment-rules", { method: "POST", body })
    },
    onSuccess: () => {
      toast.success(rule ? "Adjustment rule updated" : "Adjustment rule created")
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
        t.value !== "" &&
        !Number.isNaN(parseFloat(t.value))
    )

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] max-w-lg overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{rule ? "Edit adjustment rule" : "Create adjustment rule"}</DialogTitle>
          <DialogDescription>
            Tiered surcharge or discount applied on top of this plan's charges.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="ar-name">Name</Label>
              <Input id="ar-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="Volume discount" />
            </div>
            <div className="flex items-end gap-3 pb-1">
              <Switch id="ar-enabled" checked={enabled} onCheckedChange={setEnabled} />
              <Label htmlFor="ar-enabled">Enabled</Label>
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="ar-desc">Description (optional)</Label>
            <Input id="ar-desc" value={description} onChange={(e) => setDescription(e.target.value)} />
          </div>
          <div className="grid gap-2">
            <div className="text-eyebrow">Tiers — applied from the given usage amount upwards</div>
            {tiers.map((t, i) => (
              <div key={i} className="flex flex-wrap items-center gap-2">
                <Input
                  type="number"
                  step="any"
                  className="w-24 font-mono"
                  placeholder="0.01"
                  aria-label="Start amount"
                  value={t.startAmount}
                  onChange={(e) => setTier(i, { startAmount: e.target.value })}
                />
                <Select value={t.operator} onValueChange={(v) => setTier(i, { operator: v })}>
                  <SelectTrigger className="w-28" aria-label="Operator">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="add">Add</SelectItem>
                    <SelectItem value="subtract">Subtract</SelectItem>
                  </SelectContent>
                </Select>
                <Input
                  type="number"
                  step="any"
                  className="w-24 font-mono"
                  placeholder="10"
                  aria-label="Modifier value"
                  value={t.value}
                  onChange={(e) => setTier(i, { value: e.target.value })}
                />
                <label className="flex items-center gap-1.5 text-sm">
                  <Checkbox checked={t.asPercentage} onCheckedChange={(v) => setTier(i, { asPercentage: v === true })} />
                  %
                </label>
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="Remove tier"
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
              onClick={() => setTiers((ts) => [...ts, { startAmount: "", operator: "add", asPercentage: true, value: "" }])}
            >
              <Plus className="size-4" /> Add tier
            </Button>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => save.mutate()} disabled={!name.trim() || !tiersOk || save.isPending}>
            {rule ? "Save rule" : "Create rule"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ── Price adjustment rules tab ────────────────────────────────────────────────

function AdjustmentRulesTab({ planId }: { planId: string }) {
  const qc = useQueryClient()
  const listPath = `/admin/price-adjustment-rules/price-plan/${planId}`
  const { data, isLoading, error } = useAdminList<AdjRule>(listPath, !!planId)
  const rules = data?.data ?? []
  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin-list", listPath] })

  const [createOpen, setCreateOpen] = useState(false)
  const [toEdit, setToEdit] = useState<AdjRule | null>(null)
  const [toDelete, setToDelete] = useState<AdjRule | null>(null)
  const [usageRule, setUsageRule] = useState<AdjRule | null>(null)

  const deleteRule = useMutation({
    mutationFn: (id: string) => apiFetch(`/admin/price-adjustment-rules/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Adjustment rule deleted")
      setToDelete(null)
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<AdjRule, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => r.name ?? r.id,
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "description",
        accessorFn: (r) => r.description ?? "",
        header: "Description",
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "enabled",
        accessorFn: (r) => (r.enabled ? "ENABLED" : "DISABLED"),
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "tiers",
        accessorFn: (r) => adjTiersSummary(r),
        header: "Modifier tiers",
        enableSorting: false,
        cell: ({ getValue }) => <span className="font-mono text-sm tabular-nums">{getValue()}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const rule = row.original
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${rule.name ?? rule.id}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => setUsageRule(rule)}>
                    <BarChart3 className="size-4" /> Usage
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => setToEdit(rule)}>
                    <Pencil className="size-4" /> Edit
                  </DropdownMenuItem>
                  <DropdownMenuItem variant="destructive" onClick={() => setToDelete(rule)}>
                    <Trash2 className="size-4" /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // useState setters are stable; helpers are module scope.
    [],
  )

  return (
    <>
      <div className="mb-4 flex justify-end">
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          <Plus className="size-4" /> Create adjustment rule
        </Button>
      </div>

      {!isLoading && !error && !rules.length ? (
        <EmptyState
          icon={SlidersHorizontal}
          title="No price adjustment rules"
          hint="Add a tiered surcharge or discount on top of this plan's charges."
          action={
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create adjustment rule
            </Button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={rules}
          isLoading={isLoading}
          error={error as Error | null}
          getRowId={(r) => r.id}
        />
      )}

      {createOpen ? (
        <AdjRuleDialog planId={planId} onClose={() => setCreateOpen(false)} onSaved={() => void invalidate()} />
      ) : null}
      {toEdit ? (
        <AdjRuleDialog planId={planId} rule={toEdit} onClose={() => setToEdit(null)} onSaved={() => void invalidate()} />
      ) : null}

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete adjustment rule</DialogTitle>
            <DialogDescription>Delete "{toDelete?.name}"? It stops applying to future charges immediately.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => toDelete && deleteRule.mutate(toDelete.id)}
              disabled={deleteRule.isPending}
            >
              Delete rule
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {usageRule ? <AdjUsageDialog ruleId={usageRule.id} ruleName={usageRule.name} onClose={() => setUsageRule(null)} /> : null}
    </>
  )
}

// ── Page ──────────────────────────────────────────────────────────────────────

export default function PricePlanDetailPage() {
  const { id = "" } = useParams()
  const qc = useQueryClient()

  const { data: plan, isLoading: planLoading, error: planError } = useAdminGet<PricePlan>(`/admin/price-plan/${id}`, !!id)
  const rulesPath = `/admin/price-plan/${id}/rule`
  const { data: rulesEnv, isLoading: rulesLoading } = useAdminList<Rule>(rulesPath, !!id)
  const rules = rulesEnv?.data ?? []

  const { data: typesEnv } = useAdminList<ResourceType>("/admin/price-plan/resource-types")
  const resourceTypes = typesEnv?.data ?? []

  const invalidateRules = () => qc.invalidateQueries({ queryKey: ["admin-list", rulesPath] })

  // Add-rule dialog state
  const [addOpen, setAddOpen] = useState(false)
  const [name, setName] = useState("")
  const [resourceType, setResourceType] = useState("")
  const [attribute, setAttribute] = useState("")
  const [timeUnit, setTimeUnit] = useState("hour")
  const [price, setPrice] = useState("")

  const selectedType = resourceTypes.find((t) => t.resourceType === resourceType)

  const createRule = useMutation({
    mutationFn: () =>
      apiFetch("/admin/price-plan/rule", {
        method: "POST",
        body: {
          name,
          resourceType,
          timeUnit,
          pricePlanId: id,
          prices: [{ attributeName: attribute, tiers: [{ value: parseFloat(price) }] }],
        },
      }),
    onSuccess: () => {
      toast.success("Rule added")
      setAddOpen(false)
      setName("")
      setResourceType("")
      setAttribute("")
      setPrice("")
      void invalidateRules()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const [toDelete, setToDelete] = useState<Rule | null>(null)
  const deleteRule = useMutation({
    mutationFn: (ruleId: string) => apiFetch(`/admin/price-plan/rule/${ruleId}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Rule deleted")
      setToDelete(null)
      void invalidateRules()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const [usageRule, setUsageRule] = useState<Rule | null>(null)
  const [editRule, setEditRule] = useState<Rule | null>(null)

  const ruleColumns = useMemo<ColumnDef<Rule, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => r.name ?? r.id,
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "type",
        accessorFn: (r) => r.resourceType ?? "",
        header: sortableHeader("Resource type"),
        cell: ({ getValue }) => <span className="text-sm">{getValue() || "—"}</span>,
      },
      {
        id: "unit",
        accessorFn: (r) => r.timeUnit ?? "",
        header: sortableHeader("Time unit"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "basis",
        accessorFn: (r) => priceBasis(r),
        header: "Price basis",
        enableSorting: false,
        cell: ({ getValue }) => <span className="font-mono text-sm tabular-nums">{getValue()}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const rule = row.original
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${rule.name ?? rule.id}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => setUsageRule(rule)}>
                    <BarChart3 className="size-4" /> Usage
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => setEditRule(rule)}>
                    <Pencil className="size-4" /> Edit rule
                  </DropdownMenuItem>
                  <DropdownMenuItem variant="destructive" onClick={() => setToDelete(rule)}>
                    <Trash2 className="size-4" /> Delete rule
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // useState setters are stable; helpers are module scope.
    [],
  )

  const canSubmit = name.trim() && resourceType && attribute && price !== "" && !Number.isNaN(parseFloat(price))

  const providerCount = plan?.serviceProviders?.length ?? 0

  return (
    <>
      <PageHeader
        title={plan?.name ?? "Price plan"}
        eyebrow="System"
        breadcrumb={
          <Breadcrumb>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink asChild>
                  <Link to="/system/price-plans">Price plans</Link>
                </BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{plan?.name ?? id}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        description="Pricing rules and price adjustments charged against this plan's resources."
        actions={
          <Button size="sm" onClick={() => setAddOpen(true)}>
            <Plus className="size-4" /> Add rule
          </Button>
        }
      />

      {planLoading ? (
        <Skeleton className="mb-6 h-10" />
      ) : planError ? (
        <div className="mb-6 rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">
          {(planError as Error).message}
        </div>
      ) : plan ? (
        <div className="mb-6 flex flex-wrap items-center gap-2">
          <Badge variant={plan.accessMode === "SCOPED" ? "secondary" : "outline"}>{plan.accessMode ?? "PUBLIC"}</Badge>
          <StatusBadge status={plan.enabled ? "ENABLED" : "DISABLED"} />
          {providerCount > 0 ? (
            <Badge variant="outline">
              {providerCount} service provider{providerCount === 1 ? "" : "s"}
            </Badge>
          ) : null}
          <span className="font-mono text-xs text-muted-foreground">{plan.id}</span>
        </div>
      ) : null}

      <Tabs defaultValue="rules">
        <TabsList>
          <TabsTrigger value="rules">Rules</TabsTrigger>
          <TabsTrigger value="adjustments">Price adjustments</TabsTrigger>
        </TabsList>

        <TabsContent value="rules" className="mt-4">
          {!rulesLoading && !rules.length ? (
            <EmptyState
              icon={Ruler}
              title="No rules yet"
              hint="Add a rule to start rating a resource type against this plan."
              action={
                <Button onClick={() => setAddOpen(true)}>
                  <Plus className="size-4" /> Add rule
                </Button>
              }
            />
          ) : (
            <DataTable
              columns={ruleColumns}
              data={rules}
              isLoading={rulesLoading}
              searchPlaceholder="Search rules…"
              getRowId={(r) => r.id}
            />
          )}
        </TabsContent>

        <TabsContent value="adjustments" className="mt-4">
          <AdjustmentRulesTab planId={id} />
        </TabsContent>
      </Tabs>

      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add rule</DialogTitle>
            <DialogDescription>Price one resource attribute per time unit.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label htmlFor="rule-name">Name</Label>
              <Input id="rule-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="Instance vCPU hourly" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="rule-type">Resource type</Label>
              <Select
                value={resourceType}
                onValueChange={(v) => {
                  setResourceType(v)
                  setAttribute("")
                }}
              >
                <SelectTrigger id="rule-type">
                  <SelectValue placeholder="Select a resource type" />
                </SelectTrigger>
                <SelectContent>
                  {resourceTypes.map((t) => (
                    <SelectItem key={t.resourceType} value={t.resourceType}>
                      {t.resourceType}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="rule-attr">Priced attribute</Label>
              <Select value={attribute} onValueChange={setAttribute} disabled={!selectedType}>
                <SelectTrigger id="rule-attr">
                  <SelectValue placeholder={selectedType ? "Select an attribute" : "Pick a resource type first"} />
                </SelectTrigger>
                <SelectContent>
                  {(selectedType?.attributes ?? []).map((a) => (
                    <SelectItem key={a.name} value={a.name}>
                      {a.name} ({a.type})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="grid gap-2">
                <Label htmlFor="rule-unit">Time unit</Label>
                <Select value={timeUnit} onValueChange={setTimeUnit}>
                  <SelectTrigger id="rule-unit">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {TIME_UNITS.map((u) => (
                      <SelectItem key={u} value={u}>
                        {u}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="rule-price">Price per unit</Label>
                <Input
                  id="rule-price"
                  type="number"
                  step="any"
                  min="0"
                  value={price}
                  onChange={(e) => setPrice(e.target.value)}
                  placeholder="0.01"
                />
              </div>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => createRule.mutate()} disabled={!canSubmit || createRule.isPending}>
              Add rule
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete rule</DialogTitle>
            <DialogDescription>Delete "{toDelete?.name}"? Charging stops for this rule immediately.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => toDelete && deleteRule.mutate(toDelete.id)}
              disabled={deleteRule.isPending}
            >
              Delete rule
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {usageRule ? <UsageDialog ruleId={usageRule.id} ruleName={usageRule.name} onClose={() => setUsageRule(null)} /> : null}
      {editRule ? (
        <EditRuleDialog rule={editRule} onClose={() => setEditRule(null)} onSaved={() => void invalidateRules()} />
      ) : null}
    </>
  )
}
