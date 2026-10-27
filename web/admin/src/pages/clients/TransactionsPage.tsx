import { useMemo, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { CreditCard, Download, RefreshCw } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader, sortableRightHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { useAdminList, useTabParam } from "@/lib/hooks"
import { fmtDateTime, fmtMoney } from "@/lib/format"

// Platform-wide transaction lists (the old admin's global Financial → Transactions):
//   GET /admin/account-credit-transactions   (all deposits)
//   GET /admin/collect-transactions          (all collect charges)
// The billing-profile picker is an OPTIONAL filter — "All profiles" by default so a deposit is
// visible without hunting for the right profile. The gateway re-sync is
// GET /admin/account-credit-transactions/{id}/sync (account-credit deposits only).
type Row = Record<string, any>

function profileLabel(p: Row): string {
  const full = [p.firstName, p.lastName].filter(Boolean).join(" ")
  return (p.fullName as string) || full || (p.companyName as string) || (p.email as string) || (p.id as string)
}

// Stream a receipt PDF: read the blob, name it from Content-Disposition, click a temp <a download>.
async function downloadResponse(resp: Response, fallback: string) {
  const blob = await resp.blob()
  const cd = resp.headers.get("content-disposition")
  const m = cd && (/filename\*=(?:UTF-8'')?"?([^";]+)"?/i.exec(cd) || /filename="?([^";]+)"?/i.exec(cd))
  const filename = m ? decodeURIComponent(m[1]) : fallback
  const url = URL.createObjectURL(blob)
  const a = document.createElement("a")
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

function BpCell({ id }: { id?: string }) {
  return id ? (
    <Link
      className="inline-block py-1 font-mono text-xs hover:underline"
      to={`/clients/billing-profiles/${id}`}
    >
      {id}
    </Link>
  ) : (
    <span className="font-mono text-xs text-muted-foreground">—</span>
  )
}

const PAGE_SIZE = 25

export default function TransactionsPage() {
  const qc = useQueryClient()
  const [tab, setTab] = useTabParam("credits")
  const profiles = useAdminList<Row>("/admin/billing-profile") // picker source — kept full (unpaged)
  const [selected, setSelected] = useState("") // "" = all profiles
  const [creditsPage, setCreditsPage] = useState(0)
  const [collectsPage, setCollectsPage] = useState(0)

  // Server-paged + server-filtered by the optional billing-profile picker (?billingProfileId=).
  const bpParam = selected ? `&billingProfileId=${selected}` : ""
  const credits = useAdminList<Row>(
    `/admin/account-credit-transactions?limit=${PAGE_SIZE}&offset=${creditsPage * PAGE_SIZE}${bpParam}`,
  )
  const collects = useAdminList<Row>(
    `/admin/collect-transactions?limit=${PAGE_SIZE}&offset=${collectsPage * PAGE_SIZE}${bpParam}`,
  )

  const creditRows = credits.data?.data ?? []
  const collectRows = collects.data?.data ?? []
  const creditTotal = credits.data?.paging?.total ?? 0
  const collectTotal = collects.data?.paging?.total ?? 0

  const onSelectBp = (v: string) => {
    setSelected(v === "all" ? "" : v)
    setCreditsPage(0) // a new profile filter resets both tabs to page 1
    setCollectsPage(0)
  }

  const sync = useMutation({
    mutationFn: (txnId: string) => apiFetch(`/admin/account-credit-transactions/${txnId}/sync`),
    onSuccess: () => {
      toast.success("Transaction re-synced with the gateway")
      void qc.invalidateQueries({ queryKey: ["admin-list"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const download = useMutation({
    mutationFn: async (txnId: string) => {
      const resp = await apiFetch<Response>(`/admin/collect-transactions/download/${txnId}`, { raw: true })
      if (!resp.ok) throw new Error((await resp.text()) || `Download failed (${resp.status})`)
      await downloadResponse(resp, `${txnId}.pdf`)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const creditColumns = useMemo<ColumnDef<Row, any>[]>(
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
        cell: ({ row }) => <BpCell id={row.original.billingProfileId} />,
      },
      {
        id: "status",
        accessorFn: (t) => (t.status as string) ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
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
        id: "externalId",
        accessorFn: (t) => (t.externalId as string) ?? "",
        header: "External ID",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          // Gateway re-sync only re-drives a PENDING deposit; settled rows get no dead button.
          if ((row.original.status as string) !== "PENDING") return null
          const busy = sync.isPending && sync.variables === row.original.id
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`Re-sync transaction ${row.original.id}`}
                disabled={sync.isPending}
                onClick={() => sync.mutate(row.original.id)}
              >
                <RefreshCw className={busy ? "size-4 animate-spin" : "size-4"} />
              </Button>
            </div>
          )
        },
      },
    ],
    // sync.isPending/variables drive the row buttons' disabled/spinner state.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [sync.isPending, sync.variables],
  )

  const collectColumns = useMemo<ColumnDef<Row, any>[]>(
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
        cell: ({ row }) => <BpCell id={row.original.billingProfileId} />,
      },
      {
        id: "status",
        accessorFn: (t) => (t.status as string) ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
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
        id: "gateway",
        accessorFn: (t) => (t.paymentGatewayId as string) ?? "",
        header: "Gateway",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
      {
        id: "externalId",
        accessorFn: (t) => (t.externalId as string) ?? "",
        header: "External ID",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => (
          <div className="text-right" onClick={(e) => e.stopPropagation()}>
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={`Download receipt for transaction ${row.original.id}`}
              disabled={download.isPending}
              onClick={() => download.mutate(row.original.id)}
            >
              <Download className="size-4" />
            </Button>
          </div>
        ),
      },
    ],
    // download.isPending drives the disabled state on the row buttons.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [download.isPending],
  )

  return (
    <>
      <PageHeader
        title="Transactions"
        eyebrow="Clients"
        description="Every deposit and collect transaction across all billing profiles. Filter by profile with the picker."
        actions={
          <Select value={selected || "all"} onValueChange={onSelectBp}>
            <SelectTrigger className="w-64" aria-label="Filter by billing profile">
              <SelectValue placeholder="All profiles" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All profiles</SelectItem>
              {(profiles.data?.data ?? []).map((p) => (
                <SelectItem key={p.id} value={p.id}>
                  {profileLabel(p)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        }
      />

      {credits.isError ? (
        <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">
          {(credits.error as Error).message}
        </div>
      ) : (
        <Tabs value={tab} onValueChange={setTab}>
          <TabsList>
            <TabsTrigger value="credits">
              Account credit{credits.data ? ` (${creditTotal})` : ""}
            </TabsTrigger>
            <TabsTrigger value="collects">
              Collect{collects.data ? ` (${collectTotal})` : ""}
            </TabsTrigger>
          </TabsList>

          <TabsContent value="credits" className="mt-4">
            {!credits.isLoading && creditTotal === 0 ? (
              <EmptyState
                icon={CreditCard}
                title="No deposit transactions"
                hint={selected ? "No deposits for this profile." : "Client deposits will show up here."}
              />
            ) : (
              <DataTable
                columns={creditColumns}
                data={creditRows}
                isLoading={credits.isLoading}
                getRowId={(t) => t.id}
                pageSize={PAGE_SIZE}
                server={{ pageIndex: creditsPage, pageSize: PAGE_SIZE, total: creditTotal, onPageChange: setCreditsPage }}
              />
            )}
          </TabsContent>

          <TabsContent value="collects" className="mt-4">
            {!collects.isLoading && collectTotal === 0 ? (
              <EmptyState
                icon={CreditCard}
                title="No collect transactions"
                hint={selected ? "No collect charges for this profile." : "Card charges for bills land here."}
              />
            ) : (
              <DataTable
                columns={collectColumns}
                data={collectRows}
                isLoading={collects.isLoading}
                error={collects.isError ? (collects.error as Error) : null}
                getRowId={(t) => t.id}
                pageSize={PAGE_SIZE}
                server={{ pageIndex: collectsPage, pageSize: PAGE_SIZE, total: collectTotal, onPageChange: setCollectsPage }}
              />
            )}
          </TabsContent>
        </Tabs>
      )}
    </>
  )
}
