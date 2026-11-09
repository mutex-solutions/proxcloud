import { useMemo } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { KeyRound, X } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { apiFetch } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"

type SsoRow = {
  organizationId: string
  organizationName?: string
  alias: string
  protocol: string
  providerName: string
  enabled: boolean
  enforceSso: boolean
  operatorDisabled: boolean
  domains: string[]
}

const PATH = "/admin/sso"

export default function SsoPage() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useAdminList<SsoRow>(PATH)
  const items = data?.data ?? []
  const refresh = () => qc.invalidateQueries({ queryKey: ["admin-list", PATH] })

  const toggle = useMutation({
    mutationFn: (r: SsoRow) =>
      apiFetch(`${PATH}/${r.organizationId}/${r.operatorDisabled ? "enable" : "disable"}`, { method: "POST" }),
    onSuccess: refresh,
    onError: (e) => toast.error((e as Error).message),
  })
  const release = useMutation({
    mutationFn: (domain: string) => apiFetch(`${PATH}/domains/${encodeURIComponent(domain)}`, { method: "DELETE" }),
    onSuccess: () => { toast.success("Domain released"); refresh() },
    onError: (e) => toast.error((e as Error).message),
  })

  const columns = useMemo<ColumnDef<SsoRow, any>[]>(
    () => [
      {
        id: "org",
        accessorFn: (r) => r.organizationName ?? r.organizationId,
        header: sortableHeader("Organization"),
      },
      {
        id: "provider",
        accessorFn: (r) => r.providerName,
        header: sortableHeader("Provider"),
        cell: ({ row }) => (
          <span>
            {row.original.providerName}{" "}
            <span className="font-mono text-xs uppercase text-muted-foreground">{row.original.protocol}</span>
          </span>
        ),
      },
      {
        id: "state",
        accessorFn: (r) => (r.operatorDisabled ? "stopped" : r.enabled ? "enabled" : "off"),
        header: "State",
        cell: ({ row: { original: r } }) => (
          <div className="flex flex-wrap gap-1">
            <Badge variant={r.operatorDisabled ? "destructive" : r.enabled ? "default" : "outline"}>
              {r.operatorDisabled ? "Stopped by operator" : r.enabled ? "Enabled" : "Off"}
            </Badge>
            {r.enforceSso && <Badge variant="outline">Enforced</Badge>}
          </div>
        ),
      },
      {
        id: "domains",
        accessorFn: (r) => r.domains.join(" "),
        header: "Verified domains",
        cell: ({ row: { original: r } }) => (
          <div className="flex flex-wrap gap-1">
            {r.domains.map((d) => (
              <span key={d} className="inline-flex items-center gap-1 rounded border px-1.5 py-0.5 font-mono text-xs">
                {d}
                <button
                  type="button"
                  aria-label={`Release ${d}`}
                  className="text-muted-foreground hover:text-foreground"
                  onClick={() => release.mutate(d)}
                >
                  <X className="size-3" />
                </button>
              </span>
            ))}
          </div>
        ),
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row: { original: r } }) => (
          <div className="text-right">
            <Button size="sm" variant="outline" disabled={toggle.isPending} onClick={() => toggle.mutate(r)}>
              {r.operatorDisabled ? "Lift stop" : "Disable"}
            </Button>
          </div>
        ),
      },
    ],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [toggle.isPending],
  )

  return (
    <>
      <PageHeader
        title="Organization SSO"
        eyebrow="System"
        description="Identity providers configured by organizations. Disabling one stops its logins right away and the organization cannot turn it back on."
      />
      {!isLoading && !error && items.length === 0 ? (
        <EmptyState icon={KeyRound} title="No organization SSO yet" hint="Organizations set this up under Organization, Single sign-on." />
      ) : (
        <DataTable
          columns={columns}
          data={items}
          isLoading={isLoading}
          error={error as Error | null}
          searchPlaceholder="Search organizations…"
          getRowId={(r) => r.alias}
        />
      )}
    </>
  )
}
