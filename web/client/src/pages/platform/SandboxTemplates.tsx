// Templates tab: the project's custom templates (built with the E2B SDK or CLI), their builds and logs, and delete
// (backend: e2b_console.go).
import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Copy, Layers, MoreHorizontal, ScrollText, Trash2 } from "lucide-react"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { apiFetch } from "@/lib/api"
import { fmtDateTime, timeAgo } from "@/lib/format"
import type { ProjectSandboxTemplate, SandboxTemplateBuild } from "@/lib/types"

async function copy(label: string, value: string) {
  try {
    await navigator.clipboard.writeText(value)
    toast.success(`${label} copied`)
  } catch {
    toast.error(`Couldn't copy ${label}. Select and copy it manually.`)
  }
}

function BuildLogs({ pid, t, b }: { pid: string; t: ProjectSandboxTemplate; b: SandboxTemplateBuild }) {
  const logs = useQuery({
    queryKey: ["sandbox-template-logs", pid, t.id, b.buildID],
    queryFn: () => apiFetch<{ status: string; reason?: string; logs: string[] }>(
      `/project/${pid}/sandbox-templates/${t.id}/builds/${b.buildID}/logs`),
    refetchInterval: (q) => (q.state.data?.status === "building" ? 3000 : false),
  })
  if (logs.isLoading) return <p className="text-sm text-muted-foreground">Loading logs…</p>
  if (logs.error) return <p className="text-sm text-destructive">{(logs.error as Error).message}</p>
  const lines = logs.data?.logs ?? []
  return (
    <pre className="max-h-80 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap" aria-label="Build logs">
      {lines.length ? lines.join("\n") : "No logs kept for this build."}
    </pre>
  )
}

function BuildsDialog({ pid, t, onClose }: { pid: string; t: ProjectSandboxTemplate | null; onClose: () => void }) {
  const [open, setOpen] = useState<string | null>(null)
  return (
    <Dialog open={!!t} onOpenChange={(o) => { if (!o) { setOpen(null); onClose() } }}>
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Builds: {t?.name}</DialogTitle>
          <DialogDescription>
            Newest first. Tags point at the build a sandbox starts from (default when no tag is given).
          </DialogDescription>
        </DialogHeader>
        <ul className="grid max-h-[60vh] gap-3 overflow-auto">
          {t?.builds.map((b) => (
            <li key={b.buildID} className="grid gap-2 rounded-md border p-3">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <StatusBadge status={b.status} />
                {b.tags.map((tg) => <Badge key={tg} variant="secondary">{tg}</Badge>)}
                <span className="font-mono text-xs text-muted-foreground">{b.buildID}</span>
                <span className="ml-auto text-xs text-muted-foreground">
                  {fmtDateTime(b.createdAt)}
                  {b.finishedAt && b.createdAt
                    ? ` · ${Math.max(1, Math.round((Date.parse(b.finishedAt) - Date.parse(b.createdAt)) / 1000))} s`
                    : ""}
                </span>
              </div>
              {b.reason && <p className="text-sm text-destructive">{b.reason}</p>}
              {b.from && <p className="font-mono text-xs text-muted-foreground">from: {b.from}</p>}
              {b.startCmd && <p className="font-mono text-xs text-muted-foreground">start: {b.startCmd}</p>}
              <div>
                <Button variant="outline" size="sm" aria-expanded={open === b.buildID}
                  onClick={() => setOpen(open === b.buildID ? null : b.buildID)}>
                  <ScrollText aria-hidden="true" /> {open === b.buildID ? "Hide logs" : "Logs"}
                </Button>
              </div>
              {t && open === b.buildID && <BuildLogs pid={pid} t={t} b={b} />}
            </li>
          ))}
        </ul>
        <DialogFooter>
          <Button variant="outline" onClick={() => { setOpen(null); onClose() }}>Close</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function TemplatesTab({ pid }: { pid: string }) {
  const qc = useQueryClient()
  const list = useQuery({
    queryKey: ["sandbox-templates", pid],
    queryFn: () => apiFetch<ProjectSandboxTemplate[]>(`/project/${pid}/sandbox-templates`),
    enabled: !!pid,
    refetchInterval: (q) => (q.state.data?.some((t) => t.status === "building" || t.status === "waiting") ? 5000 : false),
  })
  const [shown, setShown] = useState<ProjectSandboxTemplate | null>(null)
  const [toDelete, setToDelete] = useState<ProjectSandboxTemplate | null>(null)
  const del = useMutation({
    mutationFn: (t: ProjectSandboxTemplate) => apiFetch(`/project/${pid}/sandbox-templates/${t.id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Template deleted")
      setToDelete(null)
      void qc.invalidateQueries({ queryKey: ["sandbox-templates", pid] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<ProjectSandboxTemplate, any>[]>(
    () => [
      {
        id: "name", accessorFn: (t) => t.name, header: sortableHeader("Name"),
        cell: ({ row, getValue }) => (
          <div className="grid">
            <span className="font-medium">{getValue()}</span>
            <span className="font-mono text-xs text-muted-foreground">{row.original.id}</span>
          </div>
        ),
      },
      {
        id: "status", accessorFn: (t) => (t.ready ? "ready" : t.status), header: sortableHeader("Status"),
        cell: ({ row, getValue }) => (
          <div className="grid">
            <StatusBadge status={getValue()} />
            {row.original.ready && row.original.status !== "ready" && (
              <span className="text-xs text-muted-foreground">Latest build: {row.original.status}</span>
            )}
          </div>
        ),
      },
      {
        id: "size", accessorFn: (t) => t.cpu, header: sortableHeader("Size"),
        cell: ({ row }) => <span className="text-sm">{row.original.cpu} vCPU · {row.original.memoryGiB} GiB</span>,
      },
      { id: "builds", accessorFn: (t) => t.builds.length, header: sortableHeader("Builds"), cell: ({ getValue }) => <span className="text-sm">{getValue()}</span> },
      { id: "updated", accessorFn: (t) => t.updatedAt ?? t.createdAt ?? "", header: sortableHeader("Updated"), cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() ? timeAgo(getValue()) : "—"}</span> },
      {
        id: "actions", header: () => null, enableSorting: false,
        cell: ({ row }) => (
          <div className="flex justify-end">
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${row.original.name}`}>
                  <MoreHorizontal aria-hidden="true" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => setShown(row.original)}><ScrollText aria-hidden="true" /> Builds and logs</DropdownMenuItem>
                <DropdownMenuItem onSelect={() => void copy("Template name", row.original.name)}><Copy aria-hidden="true" /> Copy name</DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem variant="destructive" onSelect={() => setToDelete(row.original)}><Trash2 aria-hidden="true" /> Delete</DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        ),
      },
    ],
    [],
  )

  return (
    <div className="grid gap-4">
      <p className="text-sm text-muted-foreground">
        Templates you build with the E2B SDK or CLI (<code className="font-mono text-xs">Template.build</code>). Only this
        project sees them. Start a sandbox from one by its name.
      </p>
      {list.data && list.data.length === 0 ? (
        <EmptyState icon={Layers} title="No templates yet" hint="Build one with the E2B SDK: Template().from_image(…) or from_dockerfile(…), then Template.build(template, alias). See the API keys tab for setup." />
      ) : (
        <DataTable columns={columns} data={list.data ?? []} isLoading={list.isLoading} error={list.error as Error | null} />
      )}

      <BuildsDialog pid={pid} t={shown && (list.data?.find((t) => t.id === shown.id) ?? shown)} onClose={() => setShown(null)} />

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete template?</DialogTitle>
            <DialogDescription>
              {toDelete ? `"${toDelete.name}" and all its builds are removed. New sandboxes can no longer start from it; running ones keep running.` : ""}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>Cancel</Button>
            <Button variant="destructive" disabled={del.isPending} onClick={() => toDelete && del.mutate(toDelete)}>
              {del.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
