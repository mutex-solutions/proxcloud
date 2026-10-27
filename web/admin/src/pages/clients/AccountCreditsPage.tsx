import { useMemo, useState } from "react"
import { Link } from "react-router-dom"
import { useQuery } from "@tanstack/react-query"
import type { Column, ColumnDef } from "@tanstack/react-table"
import { Wallet } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { apiFetch } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"
import { fmtMoney, timeAgo } from "@/lib/format"
import { cn } from "@/lib/utils"

// GET /admin/account-credit?billingProfileId= — the credits of ONE billing profile (the route
// always filters by billingProfileId; there is no global list), returned as a bare {data:[…]}
// envelope (no paging). So this page scopes by a billing-profile picker.
type Row = Record<string, any>

function profileLabel(p: Row): string {
  const full = [p.firstName, p.lastName].filter(Boolean).join(" ")
  return (p.fullName as string) || full || (p.companyName as string) || (p.email as string) || (p.id as string)
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

/** Right-aligned mono money, muted when zero (a fully-consumed credit has
 * nothing left to spend — operators scan for the balances that still carry). */
function moneyCell(value: unknown, currency?: string) {
  const n = Number(value ?? 0)
  return (
    <div className={cn("text-right font-mono text-sm tabular-nums", n === 0 && "text-muted-foreground")}>
      {fmtMoney(n, currency)}
    </div>
  )
}

export default function AccountCreditsPage() {
  const profiles = useAdminList<Row>("/admin/billing-profile")
  const [selected, setSelected] = useState("")
  const bp = selected || profiles.data?.data?.[0]?.id || ""

  const credits = useQuery({
    queryKey: ["admin-account-credits", bp],
    queryFn: () => apiFetch<Row[]>(`/admin/account-credit?billingProfileId=${bp}`),
    enabled: !!bp,
  })

  const rows = credits.data ?? []

  const columns = useMemo<ColumnDef<Row, any>[]>(
    () => [
      {
        id: "profile",
        accessorFn: (c) => (c.billingProfileId as string) ?? bp,
        header: "Billing profile",
        cell: ({ getValue }) => (
          <Link
            className="inline-block py-1 font-mono text-xs hover:underline"
            to={`/clients/billing-profiles/${getValue()}`}
          >
            {getValue()}
          </Link>
        ),
      },
      {
        id: "credit",
        accessorFn: (c) => (c.id as string) ?? "",
        header: "Credit",
        cell: ({ getValue }) => <span className="font-mono text-xs text-muted-foreground">{getValue()}</span>,
      },
      {
        id: "amount",
        accessorFn: (c) => Number(c.amount ?? 0),
        header: sortableRightHeader("Amount"),
        cell: ({ row }) => moneyCell(row.original.amount, row.original.currency),
      },
      {
        id: "initial",
        accessorFn: (c) => Number(c.initialAmount ?? 0),
        header: sortableRightHeader("Initial"),
        cell: ({ row }) => moneyCell(row.original.initialAmount, row.original.currency),
      },
      {
        id: "currency",
        accessorFn: (c) => (c.currency as string) ?? "",
        header: "Currency",
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "created",
        accessorFn: (c) => (c.createdAt as string) ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span>,
      },
    ],
    [bp],
  )

  return (
    <>
      <PageHeader
        title="Account credits"
        eyebrow="Clients"
        description="Spendable credits per billing profile."
        actions={
          <Select value={bp} onValueChange={setSelected}>
            <SelectTrigger className="w-64" aria-label="Billing profile">
              <SelectValue placeholder="Select a billing profile" />
            </SelectTrigger>
            <SelectContent>
              {(profiles.data?.data ?? []).map((p) => (
                <SelectItem key={p.id} value={p.id}>
                  {profileLabel(p)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        }
      />

      {profiles.isError ? (
        <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">
          {(profiles.error as Error).message}
        </div>
      ) : !profiles.isLoading && !bp ? (
        <EmptyState icon={Wallet} title="No billing profiles" hint="Credits are scoped to a billing profile." />
      ) : !profiles.isLoading && !credits.isLoading && !credits.isError && !rows.length ? (
        <EmptyState
          icon={Wallet}
          title="No account credits"
          hint="Grant credits from the billing profile's Credits tab."
        />
      ) : (
        <DataTable
          columns={columns}
          data={rows}
          isLoading={profiles.isLoading || credits.isLoading}
          error={credits.isError ? (credits.error as Error) : null}
          searchPlaceholder="Search credits…"
          getRowId={(c) => c.id}
          initialSorting={[{ id: "created", desc: true }]}
        />
      )}
    </>
  )
}
