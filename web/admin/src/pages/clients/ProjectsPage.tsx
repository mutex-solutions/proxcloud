import { useMemo, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { FolderKanban, MoreHorizontal, Pause, Play, RefreshCw } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { apiFetch } from "@/lib/api"
import { fmtDate } from "@/lib/format"
import { useAdminList } from "@/lib/hooks"

// GET /admin/project (clientarea_reads.go projectAdminList) — project doc shaped + joined
// `organization` + usedVcpus/usedRam/usedBlockStorage.
type Project = {
  id?: string
  name?: string
  status?: string
  organizationId?: string
  organization?: { name?: string }
  billingProfileId?: string
  createdAt?: string
}

const LIST_PATH = "/admin/project"
const PAGE_SIZE = 50

// Confirmable row actions: POST /admin/project/{id}/{status} (ENABLED|DISABLED, projectmut.go
// projectUpdateStatus) and POST /admin/project/{id}/sync (projectmut.go projectSync).
type PendingAction = { project: Project; kind: "ENABLED" | "DISABLED" | "sync" }

const actionCopy: Record<PendingAction["kind"], { title: string; verb: string; hint: string }> = {
  ENABLED: {
    title: "Enable project",
    verb: "Enable",
    hint: "Unpauses the project's servers and re-enables it.",
  },
  DISABLED: {
    title: "Disable project",
    verb: "Disable",
    hint: "Pauses every server in the project before disabling it.",
  },
  sync: {
    title: "Sync project",
    verb: "Sync",
    hint: "Reconciles the cached cloud resources against OpenStack.",
  },
}

export default function ProjectsPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  // BE-paged (offset): GET /admin/project?limit=&offset= → { data:[page], paging:{total} }.
  const [pageIndex, setPageIndex] = useState(0)
  const listPath = `${LIST_PATH}?limit=${PAGE_SIZE}&offset=${pageIndex * PAGE_SIZE}`
  const { data, isLoading, isFetching, error, refetch } = useAdminList<Project>(listPath)
  const [pending, setPending] = useState<PendingAction | null>(null)

  const projects = data?.data ?? []
  const total = data?.paging?.total ?? 0

  const runAction = useMutation({
    mutationFn: ({ project, kind }: PendingAction) =>
      apiFetch(
        kind === "sync" ? `/admin/project/${project.id}/sync` : `/admin/project/${project.id}/${kind}`,
        { method: "POST" },
      ),
    onSuccess: (_d, v) => {
      toast.success(v.kind === "sync" ? "Project synced" : `Project ${v.kind.toLowerCase()}`)
      setPending(null)
      // Paged list keys on the full (param-bearing) path, so invalidate the whole admin-list family.
      qc.invalidateQueries({ queryKey: ["admin-list"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<Project, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (p) => p.name ?? "",
        header: sortableHeader("Name"),
        cell: ({ row }) => {
          const p = row.original
          return p.id ? (
            <Link
              to={`/clients/projects/${p.id}`}
              className="inline-block py-1 font-medium hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              {p.name ?? "—"}
            </Link>
          ) : (
            <span className="font-medium">{p.name ?? "—"}</span>
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
        id: "organization",
        accessorFn: (p) => p.organization?.name ?? p.organizationId ?? "",
        header: sortableHeader("Organization"),
        cell: ({ row }) => {
          const p = row.original
          if (!p.organizationId) return <span className="text-sm text-muted-foreground">—</span>
          return (
            <Link
              to={`/clients/organizations/${p.organizationId}`}
              className={
                p.organization?.name
                  ? "inline-block py-1 text-sm hover:underline"
                  : "inline-block py-1 font-mono text-xs text-muted-foreground hover:underline"
              }
              onClick={(e) => e.stopPropagation()}
            >
              {p.organization?.name ?? p.organizationId}
            </Link>
          )
        },
      },
      {
        id: "billingProfile",
        accessorFn: (p) => p.billingProfileId ?? "",
        header: "Billing profile",
        cell: ({ row }) => {
          const bp = row.original.billingProfileId
          return bp ? (
            <Link
              to={`/clients/billing-profiles/${bp}`}
              className="inline-block py-1 font-mono text-xs text-muted-foreground hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              {bp}
            </Link>
          ) : (
            <span className="font-mono text-xs text-muted-foreground">—</span>
          )
        },
      },
      {
        id: "id",
        accessorFn: (p) => p.id ?? "",
        header: "ID",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
      {
        id: "created",
        accessorFn: (p) => p.createdAt ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDate(getValue())}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const p = row.original
          const enabled = (p.status ?? "").toUpperCase() === "ENABLED"
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${p.name ?? p.id}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  {enabled ? (
                    <DropdownMenuItem onClick={() => setPending({ project: p, kind: "DISABLED" })}>
                      <Pause className="size-4" /> Disable
                    </DropdownMenuItem>
                  ) : (
                    <DropdownMenuItem onClick={() => setPending({ project: p, kind: "ENABLED" })}>
                      <Play className="size-4" /> Enable
                    </DropdownMenuItem>
                  )}
                  <DropdownMenuItem onClick={() => setPending({ project: p, kind: "sync" })}>
                    <RefreshCw className="size-4" /> Sync
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // useState setters are stable; helpers are module-scope.
    [],
  )

  return (
    <>
      <PageHeader
        title="Projects"
        eyebrow="Clients"
        description="Client projects across every organization."
        actions={
          <Button variant="outline" size="sm" onClick={() => refetch()} disabled={isFetching} aria-label="Refresh">
            <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
          </Button>
        }
      />

      {!isLoading && !error && total === 0 ? (
        <EmptyState icon={FolderKanban} title="No projects yet" hint="Projects appear when clients create them." />
      ) : (
        <DataTable
          columns={columns}
          data={projects}
          isLoading={isLoading}
          error={error as Error | null}
          pageSize={PAGE_SIZE}
          onRowClick={(p) => p.id && navigate(`/clients/projects/${p.id}`)}
          getRowId={(p) => p.id ?? p.name ?? ""}
          server={{ pageIndex, pageSize: PAGE_SIZE, total, onPageChange: setPageIndex }}
        />
      )}

      {/* Action confirm */}
      <Dialog open={!!pending} onOpenChange={(o) => !o && setPending(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{pending ? actionCopy[pending.kind].title : ""}</DialogTitle>
            <DialogDescription>
              {pending ? (
                <>
                  {actionCopy[pending.kind].hint} Project: <span className="font-medium">{pending.project.name}</span>
                </>
              ) : null}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPending(null)}>
              Cancel
            </Button>
            <Button
              variant={pending?.kind === "DISABLED" ? "destructive" : "default"}
              disabled={runAction.isPending}
              onClick={() => pending && runAction.mutate(pending)}
            >
              {runAction.isPending ? "Working…" : pending ? actionCopy[pending.kind].verb : ""}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
