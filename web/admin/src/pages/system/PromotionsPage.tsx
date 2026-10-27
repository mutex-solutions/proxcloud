import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { Column, ColumnDef } from "@tanstack/react-table"
import { Gift, MoreHorizontal, Plus, TicketPercent } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { fmtDate, fmtMoney } from "@/lib/format"
import { useAdminList } from "@/lib/hooks"

type PromotionCode = {
  id: string
  code?: string
  description?: string
  amount?: number | string
  status?: string
  validFrom?: string
  validUntil?: string
  creditValidityDuration?: string
  targetOrganizationIds?: string[]
}

type Organization = { id: string; name?: string }

type PromotionalCredit = {
  id: string
  billingProfileId?: string
  code?: string
  initialAmount?: number | string
  remainingAmount?: number | string
  expirationDate?: string
}

type BillingProfile = { id: string; email?: string; fullName?: string; firstName?: string; lastName?: string }

const CODES_PATH = "/admin/promotion-codes"

function bpLabel(bp: BillingProfile): string {
  return bp.email ?? bp.fullName ?? [bp.firstName, bp.lastName].filter(Boolean).join(" ") ?? bp.id
}

/** Sortable money value: normalizes the API's number-or-string amounts. */
function moneyNum(v: number | string | undefined): number {
  const n = typeof v === "string" ? parseFloat(v) : v
  return n == null || Number.isNaN(n) ? 0 : n
}

// ── Edit promotion code dialog ────────────────────────────────────────────────
// PUT /admin/promotion-codes/{id} — the update OVERWRITES code/description/amount/
// creditValidityDuration/validFrom/validUntil/targetOrganizationIds (omitted fields
// are dropped) and sets status only when supplied (promotioncode.go).

function EditCodeDialog({ code: pc, onClose, onSaved }: { code: PromotionCode; onClose: () => void; onSaved: () => void }) {
  const orgsQ = useAdminList<Organization>("/admin/organizations")
  const orgs = orgsQ.data?.data ?? []

  const [code, setCode] = useState(pc.code ?? "")
  const [description, setDescription] = useState(pc.description ?? "")
  const [amount, setAmount] = useState(pc.amount != null ? String(pc.amount) : "")
  const [status, setStatus] = useState(pc.status ?? "ACTIVE")
  const [validFrom, setValidFrom] = useState(typeof pc.validFrom === "string" ? pc.validFrom.slice(0, 10) : "")
  const [validUntil, setValidUntil] = useState(typeof pc.validUntil === "string" ? pc.validUntil.slice(0, 10) : "")
  const [validity, setValidity] = useState(pc.creditValidityDuration ?? "")
  const [targetOrgIds, setTargetOrgIds] = useState<string[]>(pc.targetOrganizationIds ?? [])

  const save = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = { code: code.trim(), amount: parseFloat(amount), status }
      if (description.trim()) body.description = description.trim()
      if (validFrom) body.validFrom = `${validFrom}T00:00:00Z`
      if (validUntil) body.validUntil = `${validUntil}T23:59:59Z`
      if (validity.trim()) body.creditValidityDuration = validity.trim()
      if (targetOrgIds.length) body.targetOrganizationIds = targetOrgIds
      return apiFetch(`${CODES_PATH}/${pc.id}`, { method: "PUT", body })
    },
    onSuccess: () => {
      toast.success("Promotion code updated")
      onSaved()
      onClose()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Edit promotion code</DialogTitle>
          <DialogDescription>Fields left empty are cleared on the code.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="ec-code">Code</Label>
              <Input id="ec-code" value={code} onChange={(e) => setCode(e.target.value)} className="font-mono" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="ec-amount">Amount</Label>
              <Input
                id="ec-amount"
                type="number"
                min="0"
                step="any"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
              />
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="ec-desc">Description (optional)</Label>
            <Input id="ec-desc" value={description} onChange={(e) => setDescription(e.target.value)} />
          </div>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="ec-status">Status</Label>
              <Select value={status} onValueChange={setStatus}>
                <SelectTrigger id="ec-status">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="ACTIVE">ACTIVE</SelectItem>
                  <SelectItem value="DISABLED">DISABLED</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="ec-validity">Credit validity (ISO-8601, e.g. P30D)</Label>
              <Input
                id="ec-validity"
                value={validity}
                onChange={(e) => setValidity(e.target.value)}
                placeholder="Never expires when empty"
              />
            </div>
          </div>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="ec-from">Valid from (optional)</Label>
              <Input id="ec-from" type="date" value={validFrom} onChange={(e) => setValidFrom(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="ec-until">Valid until (optional)</Label>
              <Input id="ec-until" type="date" value={validUntil} onChange={(e) => setValidUntil(e.target.value)} />
            </div>
          </div>
          <div className="grid gap-2">
            <Label>Target organizations — none selected means anyone can redeem</Label>
            {orgsQ.isLoading ? (
              <Skeleton className="h-8" />
            ) : !orgs.length ? (
              <p className="text-sm text-muted-foreground">No organizations.</p>
            ) : (
              <div className="flex max-h-40 flex-col gap-1.5 overflow-y-auto rounded-md border p-2">
                {orgs.map((o) => (
                  <label key={o.id} className="flex items-center gap-2 text-sm">
                    <Checkbox
                      checked={targetOrgIds.includes(o.id)}
                      onCheckedChange={(v) =>
                        setTargetOrgIds((ids) => (v === true ? [...ids, o.id] : ids.filter((x) => x !== o.id)))
                      }
                    />
                    {o.name ?? o.id}
                  </label>
                ))}
              </div>
            )}
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            onClick={() => save.mutate()}
            disabled={!code.trim() || !amount || Number.isNaN(parseFloat(amount)) || save.isPending}
          >
            Save promotion code
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ── Promotion codes tab ───────────────────────────────────────────────────────

function PromotionCodesTab() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useAdminList<PromotionCode>(CODES_PATH)
  const codes = data?.data ?? []
  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin-list", CODES_PATH] })

  const [createOpen, setCreateOpen] = useState(false)
  const [code, setCode] = useState("")
  const [amount, setAmount] = useState("")
  const [validFrom, setValidFrom] = useState("")
  const [validUntil, setValidUntil] = useState("")
  const [validityDays, setValidityDays] = useState("")

  const createCode = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = { code, amount: parseFloat(amount) }
      if (validFrom) body.validFrom = `${validFrom}T00:00:00Z`
      if (validUntil) body.validUntil = `${validUntil}T23:59:59Z`
      // Credit validity is an ISO-8601 duration on the API (e.g. P30D).
      if (validityDays) body.creditValidityDuration = `P${parseInt(validityDays, 10)}D`
      return apiFetch(CODES_PATH, { method: "POST", body })
    },
    onSuccess: () => {
      toast.success("Promotion code created")
      setCreateOpen(false)
      setCode("")
      setAmount("")
      setValidFrom("")
      setValidUntil("")
      setValidityDays("")
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const [toEdit, setToEdit] = useState<PromotionCode | null>(null)
  const [toDelete, setToDelete] = useState<PromotionCode | null>(null)
  const deleteCode = useMutation({
    mutationFn: (id: string) => apiFetch(`${CODES_PATH}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Promotion code deleted")
      setToDelete(null)
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<PromotionCode, any>[]>(
    () => [
      {
        id: "code",
        accessorFn: (c) => c.code ?? "",
        header: sortableHeader("Code"),
        cell: ({ getValue }) => <span className="font-mono font-medium">{getValue() || "—"}</span>,
      },
      {
        id: "amount",
        accessorFn: (c) => moneyNum(c.amount),
        header: sortableRightHeader("Amount"),
        cell: ({ row }) => (
          <div className="text-right font-mono text-sm tabular-nums">{fmtMoney(row.original.amount)}</div>
        ),
      },
      {
        id: "status",
        accessorFn: (c) => c.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ row }) => <StatusBadge status={row.original.status} />,
      },
      {
        id: "validFrom",
        accessorFn: (c) => c.validFrom ?? "",
        header: sortableHeader("Valid from"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDate(getValue())}</span>,
      },
      {
        id: "validUntil",
        accessorFn: (c) => c.validUntil ?? "",
        header: sortableHeader("Valid until"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDate(getValue())}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const c = row.original
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label="Promotion code actions">
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => setToEdit(c)}>Edit</DropdownMenuItem>
                  <DropdownMenuItem className="text-destructive" onClick={() => setToDelete(c)}>
                    Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // useState setters are stable; helpers are module-scope.
    [],
  )

  return (
    <>
      <div className="mb-4 flex justify-end">
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          <Plus className="size-4" /> Create promotion code
        </Button>
      </div>

      {!isLoading && !error && !codes.length ? (
        <EmptyState
          icon={TicketPercent}
          title="No promotion codes"
          hint="Create a code clients can redeem for promotional credit."
          action={
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create promotion code
            </Button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={codes}
          isLoading={isLoading}
          error={error ? (error as Error) : null}
          searchPlaceholder="Search promotion codes…"
          getRowId={(c) => c.id}
        />
      )}

      {toEdit ? <EditCodeDialog code={toEdit} onClose={() => setToEdit(null)} onSaved={() => void invalidate()} /> : null}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create promotion code</DialogTitle>
            <DialogDescription>Clients redeem the code for promotional credit in the base currency.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="grid gap-2">
                <Label htmlFor="pc-code">Code</Label>
                <Input
                  id="pc-code"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  placeholder="WELCOME25"
                  className="font-mono"
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="pc-amount">Amount</Label>
                <Input
                  id="pc-amount"
                  type="number"
                  min="0"
                  step="any"
                  value={amount}
                  onChange={(e) => setAmount(e.target.value)}
                  placeholder="25"
                />
              </div>
            </div>
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="grid gap-2">
                <Label htmlFor="pc-from">Valid from (optional)</Label>
                <Input id="pc-from" type="date" value={validFrom} onChange={(e) => setValidFrom(e.target.value)} />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="pc-until">Valid until (optional)</Label>
                <Input id="pc-until" type="date" value={validUntil} onChange={(e) => setValidUntil(e.target.value)} />
              </div>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="pc-validity">Credit validity in days (optional)</Label>
              <Input
                id="pc-validity"
                type="number"
                min="1"
                step="1"
                value={validityDays}
                onChange={(e) => setValidityDays(e.target.value)}
                placeholder="Never expires when empty"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button
              onClick={() => createCode.mutate()}
              disabled={!code.trim() || !amount || Number.isNaN(parseFloat(amount)) || createCode.isPending}
            >
              Create promotion code
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete promotion code</DialogTitle>
            <DialogDescription>Delete "{toDelete?.code}"? Clients can no longer redeem it.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => toDelete && deleteCode.mutate(toDelete.id)}
              disabled={deleteCode.isPending}
            >
              Delete promotion code
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// ── Promotional credits tab ───────────────────────────────────────────────────
// There is no platform-wide credits list on the API — credits are read per billing
// profile (GET /admin/promotional-credits/billing-profile/{id}), so a profile is
// selected first.

function PromotionalCreditsTab() {
  const qc = useQueryClient()
  const [bpId, setBpId] = useState("")

  const profilesQ = useAdminList<BillingProfile>("/admin/billing-profile")
  const profiles = profilesQ.data?.data ?? []

  const creditsPath = `/admin/promotional-credits/billing-profile/${bpId}`
  const creditsQ = useAdminList<PromotionalCredit>(creditsPath, !!bpId)
  const credits = creditsQ.data?.data ?? []
  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin-list", creditsPath] })

  const [grantOpen, setGrantOpen] = useState(false)
  const [amount, setAmount] = useState("")
  const [daysValidity, setDaysValidity] = useState("365")

  const grant = useMutation({
    mutationFn: () =>
      apiFetch("/admin/promotional-credits", {
        method: "POST",
        body: { billingProfileId: bpId, amount: parseFloat(amount), daysValidity: parseInt(daysValidity, 10) },
      }),
    onSuccess: () => {
      toast.success("Promotional credit granted")
      setGrantOpen(false)
      setAmount("")
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const [toDelete, setToDelete] = useState<PromotionalCredit | null>(null)
  const deleteCredit = useMutation({
    mutationFn: (id: string) => apiFetch(`/admin/promotional-credits/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Promotional credit deleted")
      setToDelete(null)
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<PromotionalCredit, any>[]>(
    () => [
      {
        id: "billingProfile",
        accessorFn: (c) => c.billingProfileId ?? bpId,
        header: "Billing profile",
        cell: ({ getValue }) => <span className="font-mono text-sm">{getValue()}</span>,
      },
      {
        id: "code",
        accessorFn: (c) => c.code ?? "",
        header: sortableHeader("Code"),
        cell: ({ getValue }) => <span className="font-mono text-sm">{getValue() || "—"}</span>,
      },
      {
        id: "amount",
        accessorFn: (c) => moneyNum(c.initialAmount),
        header: sortableRightHeader("Amount"),
        cell: ({ row }) => (
          <div className="text-right font-mono text-sm tabular-nums">{fmtMoney(row.original.initialAmount)}</div>
        ),
      },
      {
        id: "remaining",
        accessorFn: (c) => moneyNum(c.remainingAmount),
        header: sortableRightHeader("Remaining"),
        cell: ({ row }) => (
          <div className="text-right font-mono text-sm tabular-nums">{fmtMoney(row.original.remainingAmount)}</div>
        ),
      },
      {
        id: "expires",
        accessorFn: (c) => c.expirationDate ?? "",
        header: sortableHeader("Expires"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDate(getValue())}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => (
          <div className="text-right">
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon-sm" aria-label="Promotional credit actions">
                  <MoreHorizontal className="size-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem className="text-destructive" onClick={() => setToDelete(row.original)}>
                  Delete
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        ),
      },
    ],
    // bpId is the fallback owner label when the API omits billingProfileId.
    [bpId],
  )

  return (
    <>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <div className="w-full max-w-sm">
          <Label htmlFor="credits-bp" className="sr-only">
            Billing profile
          </Label>
          <Select value={bpId} onValueChange={setBpId}>
            <SelectTrigger id="credits-bp">
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
        <Button size="sm" onClick={() => setGrantOpen(true)} disabled={!bpId}>
          <Plus className="size-4" /> Grant credit
        </Button>
      </div>

      {!bpId ? (
        <EmptyState
          icon={Gift}
          title="Pick a billing profile"
          hint="Promotional credits are stored per billing profile — select one to list its credits."
        />
      ) : !creditsQ.isLoading && !creditsQ.error && !credits.length ? (
        <EmptyState
          icon={Gift}
          title="No promotional credits"
          hint="Grant a credit to this billing profile — it is consumed before account credit."
          action={
            <Button onClick={() => setGrantOpen(true)}>
              <Plus className="size-4" /> Grant credit
            </Button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={credits}
          isLoading={creditsQ.isLoading}
          error={creditsQ.error ? (creditsQ.error as Error) : null}
          searchPlaceholder="Search promotional credits…"
          getRowId={(c) => c.id}
        />
      )}

      <Dialog open={grantOpen} onOpenChange={setGrantOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Grant promotional credit</DialogTitle>
            <DialogDescription>The credit lands on the selected billing profile and expires after the validity window.</DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="gc-amount">Amount</Label>
              <Input
                id="gc-amount"
                type="number"
                min="0"
                step="any"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
                placeholder="25"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="gc-days">Valid for (days)</Label>
              <Input
                id="gc-days"
                type="number"
                min="1"
                step="1"
                value={daysValidity}
                onChange={(e) => setDaysValidity(e.target.value)}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setGrantOpen(false)}>
              Cancel
            </Button>
            <Button
              onClick={() => grant.mutate()}
              disabled={
                !amount ||
                Number.isNaN(parseFloat(amount)) ||
                !daysValidity ||
                Number.isNaN(parseInt(daysValidity, 10)) ||
                grant.isPending
              }
            >
              Grant credit
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete promotional credit</DialogTitle>
            <DialogDescription>
              Remove this credit ({fmtMoney(toDelete?.remainingAmount)} remaining)? The balance drops immediately.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => toDelete && deleteCredit.mutate(toDelete.id)}
              disabled={deleteCredit.isPending}
            >
              Delete credit
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

export default function PromotionsPage() {
  return (
    <>
      <PageHeader
        title="Promotions"
        eyebrow="System"
        description="Redeemable promotion codes and per-profile promotional credits."
      />
      <Tabs defaultValue="codes">
        <TabsList>
          <TabsTrigger value="codes">Promotion codes</TabsTrigger>
          <TabsTrigger value="credits">Promotional credits</TabsTrigger>
        </TabsList>
        <TabsContent value="codes" className="mt-4">
          <PromotionCodesTab />
        </TabsContent>
        <TabsContent value="credits" className="mt-4">
          <PromotionalCreditsTab />
        </TabsContent>
      </Tabs>
    </>
  )
}

/** Right-aligned variant of sortableHeader for numeric (amount) columns. */
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
