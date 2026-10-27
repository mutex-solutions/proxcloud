import { useMemo, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { Column, ColumnDef } from "@tanstack/react-table"
import { Check, Landmark, X } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { apiFetch } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"
import { fmtDateTime, fmtMoney } from "@/lib/format"

// GET /admin/bank-transfer?integrationId= — the transfers of ONE BankTransfer gateway integration
// (the route always filters by paymentGatewayId==integrationId, so a gateway must be selected).
// Approve/Reject settle via POST /admin/bank-transfer/{id}/approve | /reject (processAddFunds:
// approve credits the account, reject marks the transaction FAILED).
type Row = Record<string, any>

// The list returns raw docs (no _id→id shaping) — normalize the key.
function rowId(r: Row): string {
  return String(r.id ?? r._id ?? "")
}

type PendingDecision = { id: string; action: "approve" | "reject" } | null

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

export default function BankTransfersPage() {
  const qc = useQueryClient()
  const integrations = useAdminList<Row>("/admin/integrations")
  const gateways = (integrations.data?.data ?? []).filter((i) => i.thirdParty === "BankTransfer")
  const [selected, setSelected] = useState("")
  const gw = selected || gateways[0]?.id || ""

  const listPath = `/admin/bank-transfer?integrationId=${gw}`
  const transfers = useAdminList<Row>(listPath, !!gw)
  const rows = transfers.data?.data ?? []
  const [pending, setPending] = useState<PendingDecision>(null)

  const decide = useMutation({
    mutationFn: ({ id, action }: { id: string; action: "approve" | "reject" }) =>
      apiFetch(`/admin/bank-transfer/${id}/${action}`, { method: "POST" }),
    onSuccess: (_d, { action }) => {
      toast.success(action === "approve" ? "Transfer approved — funds credited" : "Transfer rejected")
      void qc.invalidateQueries({ queryKey: ["admin-list", listPath] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<Row, any>[]>(
    () => [
      {
        id: "date",
        accessorFn: (t) => (t.createdAt as string) ?? "",
        header: sortableHeader("Date"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDateTime(getValue())}</span>,
      },
      {
        id: "profile",
        accessorFn: (t) => (t.billingProfileId as string) ?? "",
        header: "Billing profile",
        cell: ({ getValue }) =>
          getValue() ? (
            <Link
              className="inline-block py-1 font-mono text-xs hover:underline"
              to={`/clients/billing-profiles/${getValue()}`}
            >
              {getValue()}
            </Link>
          ) : (
            <span className="font-mono text-xs text-muted-foreground">—</span>
          ),
      },
      {
        id: "amount",
        accessorFn: (t) => Number(t.amount ?? 0),
        header: sortableRightHeader("Amount"),
        cell: ({ row }) => (
          <div className="text-right font-mono text-sm tabular-nums">
            {fmtMoney(row.original.amount, row.original.currency)}
          </div>
        ),
      },
      {
        id: "reference",
        accessorFn: (t) => (t.referenceNumber as string) ?? "",
        header: "Reference",
        cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() || "—"}</span>,
      },
      {
        id: "status",
        accessorFn: (t) => (t.status as string) ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const t = row.original
          const id = rowId(t)
          if (t.status !== "PENDING") return null
          return (
            <div className="flex justify-end gap-2" onClick={(e) => e.stopPropagation()}>
              <Button
                variant="outline"
                size="sm"
                disabled={decide.isPending}
                onClick={() => setPending({ id, action: "approve" })}
              >
                <Check className="size-4" /> Approve
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="text-destructive-text hover:text-destructive-text"
                disabled={decide.isPending}
                onClick={() => setPending({ id, action: "reject" })}
              >
                <X className="size-4" /> Reject
              </Button>
            </div>
          )
        },
      },
    ],
    // decide.isPending drives the disabled state on the row buttons.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [decide.isPending],
  )

  return (
    <>
      <PageHeader
        title="Bank transfers"
        eyebrow="Clients"
        description="When a customer deposits via a Bank Transfer payment gateway, a pending transfer with a reference number appears here for an operator to approve (credits the account) or reject. Select a Bank Transfer gateway above; if none exists, install one under Integrations."
        actions={
          gateways.length > 1 ? (
            <Select value={gw} onValueChange={setSelected}>
              <SelectTrigger className="w-64" aria-label="Bank-transfer gateway">
                <SelectValue placeholder="Select a gateway" />
              </SelectTrigger>
              <SelectContent>
                {gateways.map((g) => (
                  <SelectItem key={g.id} value={g.id}>
                    {g.name ?? g.thirdParty ?? g.id}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : undefined
        }
      />

      {integrations.isError ? (
        <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">
          {(integrations.error as Error).message}
        </div>
      ) : !integrations.isLoading && !gw ? (
        <EmptyState
          icon={Landmark}
          title="Select a bank-transfer gateway"
          hint="No Bank Transfer gateway is configured. Install one under Integrations to start receiving bank-transfer deposits here."
        />
      ) : !integrations.isLoading && !transfers.isLoading && !transfers.isError && !rows.length ? (
        <EmptyState
          icon={Landmark}
          title="No pending transfers for this gateway"
          hint="When a customer deposits via this Bank Transfer gateway, a pending transfer with its reference number appears here to approve or reject."
        />
      ) : (
        <DataTable
          columns={columns}
          data={rows}
          isLoading={integrations.isLoading || transfers.isLoading}
          error={transfers.isError ? (transfers.error as Error) : null}
          searchPlaceholder="Search transfers…"
          getRowId={rowId}
          initialSorting={[{ id: "date", desc: true }]}
        />
      )}

      <Dialog open={!!pending} onOpenChange={(o) => !o && setPending(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{pending?.action === "approve" ? "Approve transfer" : "Reject transfer"}</DialogTitle>
            <DialogDescription>
              {pending?.action === "approve"
                ? "Approving settles the deposit — the client's account is credited. This cannot be undone."
                : "Rejecting marks the deposit transaction as failed. No credit is granted."}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPending(null)}>
              Cancel
            </Button>
            <Button
              variant={pending?.action === "reject" ? "destructive" : "default"}
              disabled={decide.isPending}
              onClick={() => {
                if (pending) decide.mutate(pending)
                setPending(null)
              }}
            >
              {pending?.action === "approve" ? "Approve" : "Reject"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
