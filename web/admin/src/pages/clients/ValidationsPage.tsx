import { useMemo, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { BadgeCheck, Check, RefreshCw, X } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { apiFetch } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"
import { fmtDateTime } from "@/lib/format"
import { profileName } from "./BillingProfilesPage"

// GET /admin/billing-profile/validations — PENDING billingProfileValidation docs, each joined
// with its shaped billing profile under `billingProfile` (billingprofile.go billingProfileValidations).
// POST /admin/billing-profile/validations/{validationId}/status/{APPROVED|REJECTED} flips the doc;
// the APPROVED branch activates the profile + sends the validation email server-side.
// Document upload/download is a vendor seam server-side, so no document link is rendered.
type ValidationRow = Record<string, any>

const LIST_PATH = "/admin/billing-profile/validations"

type PendingDecision = { id: string; status: "APPROVED" | "REJECTED"; who: string } | null

function validationWho(v: ValidationRow): string {
  const bp = (v.billingProfile ?? {}) as Record<string, any>
  return bp.id ? profileName(bp) : (v.billingProfileId as string) || (v.id as string)
}

export default function ValidationsPage() {
  const qc = useQueryClient()
  const { data, isLoading, isError, error, refetch, isFetching } = useAdminList<ValidationRow>(LIST_PATH)
  const rows = data?.data ?? []
  const [pending, setPending] = useState<PendingDecision>(null)

  const decide = useMutation({
    mutationFn: (p: { id: string; status: string }) =>
      apiFetch(`/admin/billing-profile/validations/${p.id}/status/${p.status}`, { method: "POST" }),
    onSuccess: (_d, p) => {
      toast.success(
        p.status === "APPROVED"
          ? "Validation approved — the billing profile has been activated"
          : "Validation rejected",
      )
      void qc.invalidateQueries({ queryKey: ["admin-list", LIST_PATH] })
    },
    // Surface the exact API error message (e.g. "Billing profile validation not found.").
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<ValidationRow, any>[]>(
    () => [
      {
        id: "profile",
        accessorFn: (v) => `${validationWho(v)} ${(v.billingProfile as Record<string, any>)?.email ?? ""}`,
        header: sortableHeader("Billing profile"),
        cell: ({ row }) => {
          const v = row.original
          const bp = (v.billingProfile ?? {}) as Record<string, any>
          const who = validationWho(v)
          return (
            <div>
              {bp.id ? (
                <Link
                  className="inline-block py-1 font-medium hover:underline"
                  to={`/clients/billing-profiles/${bp.id}`}
                >
                  {who}
                </Link>
              ) : (
                <span className="font-medium">{who}</span>
              )}
              <p className="text-xs text-muted-foreground">{bp.email ?? "—"}</p>
            </div>
          )
        },
      },
      {
        id: "status",
        accessorFn: (v) => (v.status as string) ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "submitted",
        accessorFn: (v) => (v.createdAt as string) ?? "",
        header: sortableHeader("Submitted"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDateTime(getValue())}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const v = row.original
          const who = validationWho(v)
          return (
            <div className="flex justify-end gap-2" onClick={(e) => e.stopPropagation()}>
              <Button
                variant="outline"
                size="sm"
                disabled={decide.isPending}
                onClick={() => setPending({ id: v.id, status: "APPROVED", who })}
              >
                <Check className="size-4" /> Approve
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="text-destructive-text hover:text-destructive-text"
                disabled={decide.isPending}
                onClick={() => setPending({ id: v.id, status: "REJECTED", who })}
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
        title="Validations"
        eyebrow="Clients"
        description="Pending billing-profile identity validations awaiting review."
        actions={
          <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh">
            <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
          </Button>
        }
      />

      {!isLoading && !isError && !rows.length ? (
        <EmptyState
          icon={BadgeCheck}
          title="No pending validations"
          hint="Validation requests appear here when clients submit identity documents and the validation flow is enabled."
        />
      ) : (
        <DataTable
          columns={columns}
          data={rows}
          isLoading={isLoading}
          error={isError ? (error as Error) : null}
          searchPlaceholder="Search validations…"
          getRowId={(v) => v.id}
          initialSorting={[{ id: "submitted", desc: true }]}
        />
      )}

      <Dialog open={!!pending} onOpenChange={(o) => !o && setPending(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{pending?.status === "APPROVED" ? "Approve validation" : "Reject validation"}</DialogTitle>
            <DialogDescription>
              {pending?.status === "APPROVED"
                ? `Approve the validation for ${pending?.who}? This activates the billing profile and notifies the client.`
                : `Reject the validation for ${pending?.who}? The client will need to submit again.`}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPending(null)}>
              Cancel
            </Button>
            <Button
              variant={pending?.status === "REJECTED" ? "destructive" : "default"}
              disabled={decide.isPending}
              onClick={() => {
                if (pending) decide.mutate({ id: pending.id, status: pending.status })
                setPending(null)
              }}
            >
              {pending?.status === "APPROVED" ? "Approve" : "Reject"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
