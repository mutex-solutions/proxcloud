import { useMemo } from "react"
import { useNavigate } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Download, FileText, Receipt } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader, sortableRightHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { LoadMore } from "@/components/load-more"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { fmtDate, fmtDateTime, fmtMoney } from "@/lib/format"
import { useBillingSummary, useCursorList, useProjectId } from "@/lib/hooks"
import type { Bill, Transaction } from "@/lib/types"
import { cn } from "@/lib/utils"

// Fetch an authed endpoint as a blob and trigger a browser download.
export async function downloadPdf(path: string, filename: string) {
  const resp = await apiFetch<Response>(path, { raw: true })
  if (!resp.ok) throw new Error(`Download failed (${resp.status})`)
  const blob = await resp.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement("a")
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}


export default function HistoryPage() {
  const pid = useProjectId()
  const { data: summary, isLoading } = useBillingSummary(pid)
  const bp = summary?.id

  return (
    <>
      <PageHeader
        title="Billing history"
        eyebrow="Billing"
        description="Bills and payment transactions on this billing profile."
      />
      {isLoading || !bp ? (
        <Skeleton className="h-64" />
      ) : (
        <Tabs defaultValue="bills">
          <TabsList>
            <TabsTrigger value="bills">Bills</TabsTrigger>
            <TabsTrigger value="transactions">Transactions</TabsTrigger>
            <TabsTrigger value="account-credits">Account credits</TabsTrigger>
          </TabsList>
          <TabsContent value="bills" className="mt-4">
            <BillsTab pid={pid} bp={bp} />
          </TabsContent>
          <TabsContent value="transactions" className="mt-4">
            <TransactionsTab bp={bp} />
          </TabsContent>
          <TabsContent value="account-credits" className="mt-4">
            <AccountCreditsTab bp={bp} />
          </TabsContent>
        </Tabs>
      )}
    </>
  )
}

function BillsTab({ pid, bp }: { pid: string; bp: string }) {
  const navigate = useNavigate()
  const {
    rows: bills, isLoading, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCursorList<Bill>(["bills", bp], `/bill/${bp}`)

  const columns = useMemo<ColumnDef<Bill, any>[]>(
    () => [
      {
        id: "created",
        accessorFn: (b) => b.createdAt ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm">{fmtDate(getValue())}</span>,
      },
      {
        id: "status",
        accessorFn: (b) => b.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "net",
        accessorFn: (b) => b.netAmount ?? 0,
        header: sortableRightHeader("Net"),
        cell: ({ row }) => (
          <div className="text-right font-mono tabular-nums">
            {fmtMoney(row.original.netAmount, row.original.invoiceCurrency)}
          </div>
        ),
      },
      {
        id: "gross",
        accessorFn: (b) => b.grossAmount ?? 0,
        header: sortableRightHeader("Gross"),
        cell: ({ row }) => (
          <div className="text-right font-mono tabular-nums">
            {fmtMoney(row.original.grossAmount, row.original.invoiceCurrency)}
          </div>
        ),
      },
      {
        id: "unpaid",
        accessorFn: (b) => b.unpaidGrossAmount ?? 0,
        header: sortableRightHeader("Unpaid"),
        // A settled bill's $0.00 is non-information — mute it so open balances pop.
        cell: ({ row }) => {
          const unpaid = Number(row.original.unpaidGrossAmount ?? 0)
          return (
            <div className={cn("text-right font-mono tabular-nums", unpaid === 0 && "text-muted-foreground")}>
              {fmtMoney(row.original.unpaidGrossAmount, row.original.invoiceCurrency)}
            </div>
          )
        },
      },
    ],
    [],
  )

  if (!isLoading && !error && !bills.length) {
    return <EmptyState icon={FileText} title="No bills yet" hint="Bills appear here once usage is charged." />
  }
  return (
    <>
      <DataTable
        columns={columns}
        data={bills}
        isLoading={isLoading}
        error={error as Error | null}
        pagination={false}
        onRowClick={(b) => navigate(`/p/${pid}/billing/history/bills/${b.id}`)}
        getRowId={(b) => b.id}
      />
      <LoadMore
        hasNextPage={hasNextPage}
        isFetching={isFetchingNextPage}
        onClick={() => void fetchNextPage()}
        count={bills.length}
        noun="bill"
      />
    </>
  )
}

function TransactionsTab({ bp }: { bp: string }) {
  // Keyset-paged (BE-driven): GET /collect-transactions?billingProfileId=&limit=&after=
  // → newest-first pages walked via "Load more". (The /collect-transactions/{id} path
  // route is the single-by-id read.)
  const {
    rows: txns, isLoading, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCursorList<Transaction>(["collect-transactions", bp], `/collect-transactions?billingProfileId=${bp}`)

  const columns = useMemo<ColumnDef<Transaction, any>[]>(
    () => [
      {
        id: "date",
        accessorFn: (t) => t.createdAt ?? "",
        header: sortableHeader("Date"),
        cell: ({ getValue }) => <span className="text-sm">{fmtDateTime(getValue())}</span>,
      },
      {
        id: "status",
        accessorFn: (t) => t.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "amount",
        accessorFn: (t) => t.grossAmount ?? 0,
        header: sortableRightHeader("Amount"),
        cell: ({ row }) => (
          <div className="text-right font-mono tabular-nums">
            {fmtMoney(row.original.grossAmount, row.original.currency)}
          </div>
        ),
      },
      {
        id: "externalId",
        accessorFn: (t) => (t.externalId as string) ?? t.externalInvoiceId ?? "",
        header: "External ID",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
      {
        id: "receipt",
        header: () => <div className="text-right">Receipt</div>,
        enableSorting: false,
        cell: ({ row }) => {
          const t = row.original
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="Download receipt"
                onClick={() =>
                  downloadPdf(
                    `/collect-transactions/${bp}/download/${t.id}`,
                    `${t.externalInvoiceId ?? t.id}.pdf`,
                  ).catch((e: Error) => toast.error(e.message))
                }
              >
                <Download className="size-4" />
              </Button>
            </div>
          )
        },
      },
    ],
    [bp],
  )

  if (!isLoading && !error && !txns.length) {
    return <EmptyState icon={Receipt} title="No transactions yet" hint="Deposits and bill payments appear here." />
  }
  return (
    <>
      <DataTable
        columns={columns}
        data={txns}
        isLoading={isLoading}
        error={error as Error | null}
        pagination={false}
        getRowId={(t) => t.id}
      />
      <LoadMore
        hasNextPage={hasNextPage}
        isFetching={isFetchingNextPage}
        onClick={() => void fetchNextPage()}
        count={txns.length}
        noun="transaction"
      />
    </>
  )
}

// Account-credit transactions (deposits / refunds) — GET /account-credit-transactions?billingProfileId=.
// ponytail: no per-row receipt download — the Go download route is a deliberate 501 seam
// (external invoice provider not implemented); add the button when the seam goes live.
function AccountCreditsTab({ bp }: { bp: string }) {
  const {
    rows: txns, isLoading, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCursorList<Transaction>(
    ["account-credit-transactions", bp],
    `/account-credit-transactions?billingProfileId=${bp}`,
  )

  const columns = useMemo<ColumnDef<Transaction, any>[]>(
    () => [
      {
        id: "date",
        accessorFn: (t) => t.createdAt ?? "",
        header: sortableHeader("Date"),
        cell: ({ getValue }) => <span className="text-sm">{fmtDateTime(getValue())}</span>,
      },
      {
        id: "status",
        accessorFn: (t) => t.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "amount",
        accessorFn: (t) => t.amount ?? 0,
        header: sortableRightHeader("Amount"),
        cell: ({ row }) => (
          <div className="text-right font-mono tabular-nums">
            {fmtMoney(row.original.amount, row.original.currency)}
          </div>
        ),
      },
      {
        id: "gross",
        accessorFn: (t) => t.grossAmount ?? 0,
        header: sortableRightHeader("Gross"),
        cell: ({ row }) => (
          <div className="text-right font-mono tabular-nums">
            {fmtMoney(row.original.grossAmount, row.original.currency)}
          </div>
        ),
      },
      {
        id: "currency",
        accessorFn: (t) => t.currency ?? "",
        header: "Currency",
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "externalId",
        accessorFn: (t) => (t.externalId as string) ?? t.externalInvoiceId ?? "",
        header: "External ID",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
    ],
    [],
  )

  if (!isLoading && !error && !txns.length) {
    return (
      <EmptyState icon={Receipt} title="No account-credit transactions" hint="Deposits and refunds appear here." />
    )
  }
  return (
    <>
      <DataTable
        columns={columns}
        data={txns}
        isLoading={isLoading}
        error={error as Error | null}
        pagination={false}
        getRowId={(t) => t.id}
      />
      <LoadMore
        hasNextPage={hasNextPage}
        isFetching={isFetchingNextPage}
        onClick={() => void fetchNextPage()}
        count={txns.length}
        noun="transaction"
      />
    </>
  )
}
