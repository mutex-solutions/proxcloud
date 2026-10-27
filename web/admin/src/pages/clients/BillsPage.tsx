import { useMemo, useState } from "react"
import { Link } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"
import { Download, Receipt, RefreshCw } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader, sortableRightHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { apiFetch } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"
import { fmtMoney, timeAgo } from "@/lib/format"

// GET /admin/bill — every bill doc (shaped, money as numbers) + the joined `billingProfile`.
// The raw doc carries items[].netAmount (scale-16 running nets); the list net = their sum.
// (Gross/unpaid are only computed by the per-bill overview endpoints, not stored on the doc.)
type BillRow = Record<string, any>

function billNet(b: BillRow): number {
  const items = (b.items as Array<Record<string, any>>) ?? []
  return items.reduce((sum, it) => {
    const n = parseFloat(String(it?.netAmount ?? 0))
    return sum + (Number.isNaN(n) ? 0 : n)
  }, 0)
}

function billClient(b: BillRow): string {
  const bp = (b.billingProfile as Record<string, any>) ?? {}
  return (
    bp.fullName ||
    [bp.firstName, bp.lastName].filter(Boolean).join(" ") ||
    bp.email ||
    (b.billingProfileId as string) ||
    ""
  )
}

const PAGE_SIZE = 50

export default function BillsPage() {
  // BE-paged (offset): GET /admin/bill?limit=&offset= → { data:[page], paging:{total} }.
  const [pageIndex, setPageIndex] = useState(0)
  const listPath = `/admin/bill?limit=${PAGE_SIZE}&offset=${pageIndex * PAGE_SIZE}`
  const { data, isLoading, isError, error, refetch, isFetching } = useAdminList<BillRow>(listPath)
  const rows = data?.data ?? []
  const total = data?.paging?.total ?? 0
  const [downloading, setDownloading] = useState<string | null>(null)

  // GET /admin/bill/download/{billId} → statement PDF (streamed) → blob download.
  const download = async (billId: string) => {
    setDownloading(billId)
    try {
      const resp = await apiFetch<Response>(`/admin/bill/download/${billId}`, { raw: true })
      if (!resp.ok) throw new Error(`Download failed (${resp.status})`)
      const blob = await resp.blob()
      const url = URL.createObjectURL(blob)
      const a = document.createElement("a")
      a.href = url
      a.download = `bill-${billId}.pdf`
      a.click()
      URL.revokeObjectURL(url)
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setDownloading(null)
    }
  }

  const columns = useMemo<ColumnDef<BillRow, any>[]>(
    () => [
      {
        id: "bill",
        accessorFn: (b) => (b.id as string) ?? "",
        header: "Bill",
        cell: ({ getValue }) => <span className="font-mono text-xs">{getValue()}</span>,
      },
      {
        id: "client",
        accessorFn: (b) => billClient(b),
        header: sortableHeader("Client"),
        cell: ({ row }) => {
          const b = row.original
          return b.billingProfileId ? (
            <Link
              className="inline-block py-1 text-sm font-medium hover:underline"
              to={`/clients/billing-profiles/${b.billingProfileId}`}
              onClick={(e) => e.stopPropagation()}
            >
              {billClient(b) || "—"}
            </Link>
          ) : (
            <span className="text-sm text-muted-foreground">—</span>
          )
        },
      },
      {
        id: "status",
        accessorFn: (b) => (b.status as string) ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "net",
        accessorFn: (b) => billNet(b),
        header: sortableRightHeader("Net"),
        cell: ({ row }) => (
          <div className="text-right font-mono text-sm tabular-nums">
            {fmtMoney(billNet(row.original), row.original.invoiceCurrency ?? "USD")}
          </div>
        ),
      },
      {
        id: "created",
        accessorFn: (b) => (b.createdAt as string) ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const b = row.original
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`Download statement for bill ${b.id}`}
                disabled={downloading === b.id}
                onClick={() => void download(b.id)}
              >
                <Download className="size-4" />
              </Button>
            </div>
          )
        },
      },
    ],
    // download closes over setDownloading (stable) but reads `downloading` for the
    // disabled state — keep it fresh.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [downloading],
  )

  return (
    <>
      <PageHeader
        title="Bills"
        eyebrow="Clients"
        description="Every bill on the platform, newest first."
        actions={
          <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh">
            <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
          </Button>
        }
      />

      {!isLoading && !isError && total === 0 ? (
        <EmptyState icon={Receipt} title="No bills yet" hint="Bills appear once usage charging runs." />
      ) : (
        <DataTable
          columns={columns}
          data={rows}
          isLoading={isLoading}
          error={isError ? (error as Error) : null}
          getRowId={(b) => b.id}
          initialSorting={[{ id: "created", desc: true }]}
          pageSize={PAGE_SIZE}
          server={{ pageIndex, pageSize: PAGE_SIZE, total, onPageChange: setPageIndex }}
        />
      )}
    </>
  )
}
