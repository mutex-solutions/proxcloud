import { useMemo, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { Column, ColumnDef } from "@tanstack/react-table"
import { Check, Download, Landmark, X } from "lucide-react"
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
import { fmtDate, fmtDateTime, fmtMoney } from "@/lib/format"

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

// GET /admin/bank-transfer/{id}/proof → the customer's proof-of-payment file (streamed) → download.
async function downloadProof(id: string, fileName: string) {
  const resp = await apiFetch<Response>(`/admin/bank-transfer/${id}/proof`, { raw: true })
  if (!resp.ok) throw new Error(`Download failed (${resp.status})`)
  const url = URL.createObjectURL(await resp.blob())
  const a = document.createElement("a")
  a.href = url
  a.download = fileName || `bank-transfer-${id}-proof`
  a.click()
  URL.revokeObjectURL(url)
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
        // The transfer doc stores the GROSS amount the customer must wire (grossAmount, not amount).
        id: "amount",
        accessorFn: (t) => Number(t.grossAmount ?? 0),
        header: sortableRightHeader("Amount"),
        cell: ({ row }) => (
          <div className="text-right font-mono text-sm tabular-nums">
            {fmtMoney(row.original.grossAmount, row.original.currency)}
          </div>
        ),
      },
      {
        id: "reference",
        accessorFn: (t) => (t.referenceNumber == null ? "" : String(t.referenceNumber)),
        header: "Reference",
        cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() || "—"}</span>,
      },
      {
        // Set when the customer paid a specific bill — approval settles that bill automatically.
        // The invoice number is stamped at deposit time; older transfers only carry the bill id.
        id: "bill",
        accessorFn: (t) => (t.invoiceNumber as string) ?? (t.billId as string) ?? "",
        header: "Invoice",
        cell: ({ row }) =>
          row.original.invoiceNumber ? (
            <span className="text-sm" title={row.original.billId}>{row.original.invoiceNumber}</span>
          ) : (
            <span className="font-mono text-xs text-muted-foreground">{row.original.billId || "—"}</span>
          ),
      },
      {
        // The customer's "I have transferred" step: when they reported it, their bank's reference,
        // a note, and the proof file (streamed from /admin/bank-transfer/{id}/proof).
        id: "submitted",
        accessorFn: (t) => (t.submittedAt as string) ?? "",
        header: sortableHeader("Submitted"),
        cell: ({ row }) => {
          const t = row.original
          if (!t.submittedAt) return <span className="text-sm text-muted-foreground">—</span>
          return (
            <div className="text-sm">
              <div>{fmtDateTime(t.submittedAt)}</div>
              {t.transferredAt ? (
                <div className="text-xs text-muted-foreground">transferred {fmtDate(t.transferredAt)}</div>
              ) : null}
            </div>
          )
        },
      },
      {
        id: "bankReference",
        accessorFn: (t) => (t.bankReference as string) ?? "",
        header: "Bank ref",
        cell: ({ row }) => (
          <div className="max-w-56">
            <div className="font-mono text-xs">{row.original.bankReference || "—"}</div>
            {row.original.note ? (
              <div className="truncate text-xs text-muted-foreground" title={row.original.note}>{row.original.note}</div>
            ) : null}
          </div>
        ),
      },
      {
        id: "proof",
        header: "Proof",
        enableSorting: false,
        cell: ({ row }) => {
          const t = row.original
          const proof = t.proof as { fileName?: string } | undefined
          if (!proof) return <span className="text-sm text-muted-foreground">—</span>
          return (
            <Button
              variant="ghost"
              size="sm"
              className="h-7 px-2 text-xs"
              onClick={(e) => {
                e.stopPropagation()
                downloadProof(rowId(t), proof.fileName ?? "").catch((err: Error) => toast.error(err.message))
              }}
            >
              <Download className="size-3.5" /> {proof.fileName || "Download"}
            </Button>
          )
        },
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
        description="When a customer deposits via a Bank Transfer payment gateway, a pending transfer with a reference number appears here. Once the customer reports the transfer (date, bank reference, proof) the Submitted column fills in and the finance e-mail is notified; approve (credits the account and settles the linked bill) or reject. Select a Bank Transfer gateway above; if none exists, install one under Integrations."
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
