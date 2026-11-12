import { useMemo, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { CircleAlert, MoreHorizontal, Play, Plus, RefreshCw, RotateCw, Server, Square, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { LoadMore } from "@/components/load-more"
import { StatusBadge } from "@/components/status-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { apiFetch } from "@/lib/api"
import { useCloudCursorList, useCloudFeatures, useCloudScope, useProjectId } from "@/lib/hooks"
import { timeAgo } from "@/lib/format"
import type { CloudResource } from "@/lib/types"
import { Checkbox } from "@/components/ui/checkbox"
import { serverFlavor, serverIPs, serverName, serverStatus } from "./serverFields"

// Shown in every server delete dialog. Data volumes outlive the server and stay billed, unless ticked here.
// Backups do not outlive it, which is easy to miss in a sentence, so they get their own warning.
export function ServerDeleteVolumes({ checked, onChange }: { checked: boolean; onChange: (v: boolean) => void }) {
  const backups = useCloudFeatures(useProjectId()).has("serverBackups")
  return (
    <>
    {backups && (
      <Alert variant="destructive">
        <Trash2 />
        <AlertTitle>Its backups are deleted too</AlertTitle>
        <AlertDescription>
          Every backup of this server goes with it and none can be restored afterwards. If you may need an older
          state, restore it and copy what you need off the server first.
        </AlertDescription>
      </Alert>
    )}
    <Alert>
      <CircleAlert />
      <AlertTitle>Data volumes are kept</AlertTitle>
      <AlertDescription className="grid gap-2">
        <span>The boot disk goes with the server. Other volumes stay in this project, detached and still billed.</span>
        <label className="flex items-center gap-2">
          <Checkbox checked={checked} onCheckedChange={(v) => onChange(v === true)} />
          Also delete this server's volumes
        </label>
      </AlertDescription>
    </Alert>
    </>
  )
}

// Quick actions verified against internal/cloud/providers/write.go (TypeServer Action switch):
// START / STOP / SOFTREBOOT take no data; DELETE = DELETE /project/{pid}/cloud/{id} → 202.
type PendingRow = {
  id: string
  name: string
  action: "START" | "STOP" | "SOFTREBOOT" | "DELETE"
  label: string
}

const ACTION_VERB: Record<PendingRow["action"], string> = {
  START: "Start",
  STOP: "Stop",
  SOFTREBOOT: "Reboot",
  DELETE: "Delete",
}

export function ServersPage() {
  const pid = useProjectId()
  const scope = useCloudScope(pid)
  const navigate = useNavigate()
  const qc = useQueryClient()
  const {
    rows: all, isLoading, refetch, isFetching, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCloudCursorList(pid, "SERVER")
  // A load balancer's VM is managed on its own page; it still bills and counts as a server.
  const data = useMemo(() => all.filter((r) => !r.data?.server?.loadBalancerId), [all])
  const [pending, setPending] = useState<PendingRow | null>(null)

  const invalidate = () =>
    setTimeout(() => void qc.invalidateQueries({ queryKey: ["cloud", pid, "SERVER"] }), 1500)

  const act = useMutation({
    mutationFn: (p: { id: string; action: string }) =>
      apiFetch(`/project/${pid}/cloud/${p.id}/action`, {
        method: "POST",
        body: { action: p.action },
        cloud: scope,
      }),
    onSuccess: (_d, p) => {
      toast.success(`${ACTION_VERB[p.action as PendingRow["action"]] ?? p.action} requested`)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const [deleteVolumes, setDeleteVolumes] = useState(false)
  // The delete answers once the VM is destroyed, seconds later: until the list drops the row it
  // reads "Deleting", not the "Active" of the row's last read.
  const [deletingIds, setDeletingIds] = useState<Set<string>>(new Set())
  const markDeleting = (id: string, on: boolean) =>
    setDeletingIds((s) => {
      const next = new Set(s)
      if (on) next.add(id)
      else next.delete(id)
      return next
    })
  const del = useMutation({
    mutationFn: (id: string) =>
      apiFetch(`/project/${pid}/cloud/${id}${deleteVolumes ? "?deleteVolumes=true" : ""}`, { method: "DELETE", cloud: scope }),
    onMutate: (id) => markDeleting(id, true),
    onSuccess: () => toast.success("Server deletion requested"),
    onError: (e: Error) => toast.error(e.message),
    // Each delete clears its own mark once the list has been read again: by then the row is gone,
    // or, when the destroy failed partway, it shows the state the server is really in. Several
    // deletes in flight each carry their own id.
    onSettled: async (_d, _e, id) => {
      await qc.invalidateQueries({ queryKey: ["cloud", pid, "SERVER"] })
      markDeleting(id, false)
    },
  })

  const runPending = () => {
    if (!pending) return
    if (pending.action === "DELETE") del.mutate(pending.id)
    else act.mutate({ id: pending.id, action: pending.action })
    setPending(null)
  }

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => serverName(r),
        header: sortableHeader("Name"),
        cell: ({ row, getValue }) => (
          <Link
            className="inline-block py-1 font-medium hover:underline"
            to={`/p/${pid}/servers/${row.original.id}`}
            onClick={(e) => e.stopPropagation()}
          >
            {getValue()}
          </Link>
        ),
      },
      {
        id: "status",
        accessorFn: (r) => serverStatus(r) ?? "",
        header: sortableHeader("Status"),
        cell: ({ row, getValue }) => <StatusBadge status={deletingIds.has(row.original.id) ? "DELETING" : getValue()} />,
      },
      {
        id: "flavor",
        accessorFn: (r) => serverFlavor(r),
        header: "Flavor",
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue()}</span>,
      },
      {
        id: "ips",
        accessorFn: (r) => serverIPs(r).join(", "),
        header: "IP addresses",
        cell: ({ getValue }) => <span className="font-mono text-sm">{getValue() || "—"}</span>,
      },
      {
        id: "created",
        accessorFn: (r) => r.info?.createdAt ?? r.createdAt ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => (
          <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span>
        ),
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const r = row.original
          const name = serverName(r)
          const status = serverStatus(r)
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${name}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem
                    disabled={status === "ACTIVE"}
                    onClick={() => setPending({ id: r.id, name, action: "START", label: `start "${name}"` })}
                  >
                    <Play className="size-4" /> Start
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    disabled={status === "SHUTOFF"}
                    onClick={() => setPending({ id: r.id, name, action: "STOP", label: `stop "${name}"` })}
                  >
                    <Square className="size-4" /> Stop
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    onClick={() =>
                      setPending({ id: r.id, name, action: "SOFTREBOOT", label: `reboot "${name}"` })
                    }
                  >
                    <RotateCw className="size-4" /> Reboot
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem
                    variant="destructive"
                    onClick={() =>
                      setPending({
                        id: r.id,
                        name,
                        action: "DELETE",
                        label: `delete "${name}" — this cannot be undone`,
                      })
                    }
                  >
                    <Trash2 className="size-4" /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // pid from route params is stable per mount; setters are stable.
    [pid, deletingIds],
  )

  return (
    <>
      <PageHeader
        title="Servers"
        eyebrow="Compute"
        description="Virtual machines running in this project."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh">
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" asChild>
              <Link to={`/p/${pid}/servers/new`}>
                <Plus className="size-4" /> Create server
              </Link>
            </Button>
          </>
        }
      />

      {!isLoading && !data.length ? (
        <EmptyState
          icon={Server}
          title="No servers yet"
          hint="Create your first virtual machine — it will boot in this project's region."
          action={
            <Button asChild>
              <Link to={`/p/${pid}/servers/new`}>
                <Plus className="size-4" /> Create server
              </Link>
            </Button>
          }
        />
      ) : (
        <>
          <DataTable
            columns={columns}
            data={data}
            isLoading={isLoading}
            error={error as Error | null}
            pagination={false}
            onRowClick={(r) => navigate(`/p/${pid}/servers/${r.id}`)}
          />
          <LoadMore
            hasNextPage={hasNextPage}
            isFetching={isFetchingNextPage}
            onClick={() => void fetchNextPage()}
            count={data.length}
            noun="server"
          />
        </>
      )}

      <Dialog open={!!pending} onOpenChange={(o) => !o && setPending(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{pending ? ACTION_VERB[pending.action] : ""} server</DialogTitle>
            <DialogDescription>Are you sure you want to {pending?.label}?</DialogDescription>
          </DialogHeader>
          {pending?.action === "DELETE" && <ServerDeleteVolumes checked={deleteVolumes} onChange={setDeleteVolumes} />}
          <DialogFooter>
            <Button variant="outline" onClick={() => setPending(null)}>
              Cancel
            </Button>
            <Button
              variant={pending?.action === "DELETE" ? "destructive" : "default"}
              onClick={runPending}
            >
              {pending ? ACTION_VERB[pending.action] : "Confirm"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
