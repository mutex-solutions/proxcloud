import { useMemo, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"
import { RefreshCw, Wallet } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader, sortableRightHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { useAdminList } from "@/lib/hooks"
import { fmtMoney, timeAgo } from "@/lib/format"
import { cn } from "@/lib/utils"

// GET /admin/billing-profile — shaped profile doc + computed financials
// (balance/accountCredit/promotionalCredit/currentMonth/lastMonth/forecastedMonthEnd as JSON numbers).
type BpRow = Record<string, any>

const PAGE_SIZE = 50

export function profileName(p: BpRow): string {
  const full = [p.firstName, p.lastName].filter(Boolean).join(" ")
  return (p.fullName as string) || full || (p.companyName as string) || (p.email as string) || (p.id as string)
}

/** Money cell: mono right-aligned, muted when zero, destructive when negative
 * (a negative balance is client debt — the row an operator scans for). */
function moneyCell(value: unknown, currency?: string) {
  const n = Number(value ?? 0)
  return (
    <div
      className={cn(
        "text-right font-mono text-sm tabular-nums",
        n === 0 && "text-muted-foreground",
        n < 0 && "text-destructive-text",
      )}
    >
      {fmtMoney(n, currency)}
    </div>
  )
}

export default function BillingProfilesPage() {
  const navigate = useNavigate()
  // BE-paged (offset): GET /admin/billing-profile?limit=&offset= → { data:[page], paging:{total} }.
  const [pageIndex, setPageIndex] = useState(0)
  const listPath = `/admin/billing-profile?limit=${PAGE_SIZE}&offset=${pageIndex * PAGE_SIZE}`
  const { data, isLoading, isError, error, refetch, isFetching } = useAdminList<BpRow>(listPath)
  const rows = data?.data ?? []
  const total = data?.paging?.total ?? 0

  const columns = useMemo<ColumnDef<BpRow, any>[]>(
    () => [
      {
        id: "client",
        accessorFn: (p) => `${profileName(p)} ${p.email ?? ""} ${p.companyName ?? ""}`,
        header: sortableHeader("Client"),
        cell: ({ row }) => {
          const p = row.original
          const company = p.companyName && p.companyName !== profileName(p) ? p.companyName : null
          return (
            <div>
              <Link
                to={`/clients/billing-profiles/${p.id}`}
                className="inline-block py-0.5 font-medium hover:underline"
                onClick={(e) => e.stopPropagation()}
              >
                {profileName(p)}
              </Link>
              <p className="text-xs text-muted-foreground">
                {[p.email, company].filter(Boolean).join(" · ") || "—"}
              </p>
            </div>
          )
        },
      },
      {
        id: "status",
        accessorFn: (p) => p.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "currency",
        accessorFn: (p) => p.currency ?? "",
        header: "Currency",
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "balance",
        accessorFn: (p) => Number(p.balance ?? 0),
        header: sortableRightHeader("Balance"),
        cell: ({ row }) => moneyCell(row.original.balance, row.original.currency),
      },
      {
        id: "currentMonth",
        accessorFn: (p) => Number(p.currentMonth ?? 0),
        header: sortableRightHeader("This month"),
        cell: ({ row }) => moneyCell(row.original.currentMonth, row.original.currency),
      },
      {
        id: "created",
        accessorFn: (p) => p.createdAt ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span>,
      },
    ],
    [],
  )

  return (
    <>
      <PageHeader
        title="Billing profiles"
        eyebrow="Clients"
        description="Customer billing profiles with live balances."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void refetch()}
            disabled={isFetching}
            aria-label="Refresh billing profiles"
          >
            <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
          </Button>
        }
      />

      {!isLoading && !isError && total === 0 ? (
        <EmptyState icon={Wallet} title="No billing profiles" hint="Profiles appear here once clients sign up." />
      ) : (
        <DataTable
          columns={columns}
          data={rows}
          isLoading={isLoading}
          error={isError ? (error as Error) : null}
          onRowClick={(p) => navigate(`/clients/billing-profiles/${p.id}`)}
          getRowId={(p) => p.id}
          pageSize={PAGE_SIZE}
          server={{ pageIndex, pageSize: PAGE_SIZE, total, onPageChange: setPageIndex }}
        />
      )}
    </>
  )
}
