import { useMemo, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"
import { Cloud, RefreshCw } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { useAdminList } from "@/lib/hooks"
import { timeAgo } from "@/lib/format"

// GET /admin/cloud-resource (handler.go cloudResourcesAll) — single(list), NO paging:
// [{id, data, createdAt, externalId, info, region, type, serviceId, project:{id,name}}].
// The live object sits under data.<typeKey> (data.server / data.network / …).
export type CloudResourceRow = Record<string, any>

// The display name lives on the type-keyed object inside `data` (data.server.name, …);
// buckets are flat (data.bucketName).
export function resourceName(cr: CloudResourceRow): string {
  const d = (cr.data ?? {}) as Record<string, any>
  if (typeof d.bucketName === "string" && d.bucketName) return d.bucketName
  for (const v of Object.values(d)) {
    if (v && typeof v === "object" && typeof (v as any).name === "string" && (v as any).name) {
      return (v as any).name as string
    }
  }
  return (cr.externalId as string) || (cr.id as string) || "—"
}

export function resourceStatus(cr: CloudResourceRow): string | undefined {
  const d = (cr.data ?? {}) as Record<string, any>
  for (const v of Object.values(d)) {
    if (v && typeof v === "object" && typeof (v as any).status === "string" && (v as any).status) {
      return (v as any).status as string
    }
  }
  return undefined
}

export function resourceCreatedAt(cr: CloudResourceRow): string | undefined {
  return (cr.info?.createdAt as string) ?? (cr.createdAt as string)
}

const PAGE_SIZE = 25

// The cloud-resource type facet is now server-side (?type=), so the dropdown options are a fixed
// enum (they can't be derived from a single server page). Mirror internal/cloud resource types.
const RESOURCE_TYPES = [
  "SERVER", "VOLUME", "VOLUME_SNAPSHOT", "IMAGE", "NETWORK", "PORT", "ROUTER", "FLOATING_IP",
  "SECURITY_GROUP", "KEYPAIR", "SERVER_GROUP", "LOAD_BALANCER", "DNS_ZONE", "BARBICAN_SECRET",
  "STACK", "BUCKET", "SHARE",
]

export default function CloudResourcesPage() {
  const navigate = useNavigate()
  const [type, setType] = useState("ALL")
  const [pageIndex, setPageIndex] = useState(0)
  const listPath = `/admin/cloud-resource?limit=${PAGE_SIZE}&offset=${pageIndex * PAGE_SIZE}${
    type === "ALL" ? "" : `&type=${type}`
  }`
  const { data, isLoading, isError, error, refetch, isFetching } =
    useAdminList<CloudResourceRow>(listPath)
  const rows = data?.data ?? []
  const total = data?.paging?.total ?? 0

  const onTypeChange = (t: string) => {
    setType(t)
    setPageIndex(0) // a new facet resets to the first page
  }

  const columns = useMemo<ColumnDef<CloudResourceRow, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => `${resourceName(r)} ${r.externalId ?? ""}`,
        header: sortableHeader("Name"),
        cell: ({ row }) => {
          const r = row.original
          return (
            <div>
              <Link
                className="inline-block py-1 font-medium hover:underline"
                to={`/clients/cloud-resources/${r.id}`}
                onClick={(e) => e.stopPropagation()}
              >
                {resourceName(r)}
              </Link>
              <p className="font-mono text-xs text-muted-foreground">{r.externalId ?? "—"}</p>
            </div>
          )
        },
      },
      {
        id: "type",
        accessorFn: (r) => (r.type as string) ?? "",
        header: sortableHeader("Type"),
        cell: ({ getValue }) => (
          <span className="text-sm capitalize text-muted-foreground">
            {(getValue() as string)?.toLowerCase().replace(/_/g, " ") || "—"}
          </span>
        ),
      },
      {
        id: "project",
        accessorFn: (r) => r.project?.name ?? r.project?.id ?? "",
        header: sortableHeader("Project"),
        cell: ({ row }) => {
          const r = row.original
          return r.project?.id ? (
            <Link
              className="inline-block py-1 text-sm hover:underline"
              to={`/clients/projects/${r.project.id}`}
              onClick={(e) => e.stopPropagation()}
            >
              {r.project?.name ?? r.project.id}
            </Link>
          ) : (
            <span className="text-sm text-muted-foreground">—</span>
          )
        },
      },
      {
        id: "status",
        accessorFn: (r) => resourceStatus(r) ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue() || undefined} />,
      },
      {
        id: "region",
        accessorFn: (r) => (r.region as string) ?? "",
        header: sortableHeader("Region"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "created",
        accessorFn: (r) => resourceCreatedAt(r) ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span>,
      },
    ],
    // Helpers are module-scope.
    [],
  )

  return (
    <>
      <PageHeader
        title="Cloud resources"
        eyebrow="Clients"
        description="All cached cloud resources across every project."
        actions={
          <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh">
            <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
          </Button>
        }
      />

      {!isLoading && !isError && total === 0 ? (
        <EmptyState
          icon={Cloud}
          title="No cloud resources"
          hint="Resources appear here as projects provision infrastructure."
        />
      ) : (
        <DataTable
          columns={columns}
          data={rows}
          isLoading={isLoading}
          error={isError ? (error as Error) : null}
          onRowClick={(r) => r.id && navigate(`/clients/cloud-resources/${r.id}`)}
          getRowId={(r) => (r.id as string) ?? (r.externalId as string) ?? ""}
          pageSize={PAGE_SIZE}
          server={{ pageIndex, pageSize: PAGE_SIZE, total, onPageChange: setPageIndex }}
          toolbar={
            <Select value={type} onValueChange={onTypeChange}>
              <SelectTrigger className="h-9 w-48" aria-label="Filter by type">
                <SelectValue placeholder="All types" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="ALL">All types</SelectItem>
                {RESOURCE_TYPES.map((t) => (
                  <SelectItem key={t} value={t}>
                    {t.toLowerCase().replace(/_/g, " ")}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          }
        />
      )}
    </>
  )
}
