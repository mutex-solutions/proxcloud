// Session-authenticated project templates and their immutable image artifacts.
import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Copy, Hammer, Import, Layers, MoreHorizontal, RefreshCw, ScrollText, Tags, Trash2, Zap } from "lucide-react"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { ApiError, apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import type { ProjectSandboxTemplate, SandboxTemplateCapabilities } from "@/lib/types"
import { SandboxTemplateImport } from "./SandboxTemplateImport"
import { SandboxTemplateAliases } from "./SandboxTemplateAliases"
import { SandboxTemplatePrepare } from "./SandboxTemplatePrepare"
import { SandboxTemplateBuildDialog } from "./SandboxTemplateBuildDialog"
import { SandboxTemplateBuildsDialog } from "./SandboxTemplateBuildsDialog"
import { activeBuild, sandboxTemplatePath } from "./sandbox-template-utils"

function fmtSize(bytes?: number) {
  if (!bytes) return "—"
  if (bytes < 1024 * 1024) return `${Math.max(1, Math.round(bytes / 1024))} KB`
  if (bytes < 1024 ** 3) return `${(bytes / 1024 ** 2).toFixed(1)} MB`
  return `${(bytes / 1024 ** 3).toFixed(2)} GB`
}
async function copy(value: string) {
  try { await navigator.clipboard.writeText(value); toast.success("Template name copied") }
  catch { toast.error("Couldn't copy template name — select and copy it manually") }
}
type TemplateAction = { type: "builds" | "aliases" | "prepare" | "build"; template: ProjectSandboxTemplate }
const templateBusy = (template: ProjectSandboxTemplate) => activeBuild(template.status) || template.builds.some((build) => activeBuild(build.status))

// Key all local state by project too: no open dialogs, pending form state or credentials
// may carry across a project switch, even when this tab is used outside SandboxesPage.
export function TemplatesTab({ pid }: { pid: string }) { return <ProjectTemplates key={pid} pid={pid} /> }

function ProjectTemplates({ pid }: { pid: string }) {
  const qc = useQueryClient()
  const mounted = useRef(true)
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  const list = useQuery({
    queryKey: ["sandbox-templates", pid],
    queryFn: () => apiFetch<ProjectSandboxTemplate[]>(sandboxTemplatePath(pid)),
    enabled: !!pid,
    refetchInterval: (query) => query.state.data?.some((template) => templateBusy(template)) ? 5000 : false,
  })
  const capabilities = useQuery({
    queryKey: ["sandbox-template-capabilities", pid],
    queryFn: () => apiFetch<SandboxTemplateCapabilities>(sandboxTemplatePath(pid, "capabilities")),
    enabled: !!pid,
  })
  const opener = useRef<HTMLButtonElement | null>(null)
  const rememberOpener = useCallback((event: React.FocusEvent<HTMLButtonElement>) => { opener.current = event.currentTarget }, [])
  const restoreFocus = () => requestAnimationFrame(() => { if (opener.current?.isConnected) opener.current.focus() })
  const closeAction = () => { setAction(null); restoreFocus() }
  const closeImport = () => { setImportOpen(false); restoreFocus() }
  const [importOpen, setImportOpen] = useState(false)
  const [action, setAction] = useState<TemplateAction | null>(null)
  const [toDelete, setToDelete] = useState<ProjectSandboxTemplate | null>(null)
  const invalidate = () => { void qc.invalidateQueries({ queryKey: ["sandbox-templates", pid] }) }
  const del = useMutation({
    mutationFn: (template: ProjectSandboxTemplate) => apiFetch(sandboxTemplatePath(pid, template.id), { method: "DELETE" }),
    onSuccess: (_, template) => {
      invalidate()
      if (!mounted.current) return
      toast.success(`${template.kind === "snapshot" ? "Snapshot" : "Template"} deleted`)
      setToDelete((current) => current?.id === template.id ? null : current)
      restoreFocus()
    },
    onError: (error: Error) => { if (mounted.current) toast.error(error.message) },
  })
  const { isPending: deleting, reset: resetDelete } = del
  const caps = capabilities.isError ? undefined : capabilities.data
  const canImport = !!caps?.importEnabled && caps.trustedWorkerAvailable !== false
  const selected = action && (list.data?.find((template) => template.id === action.template.id) ?? action.template)
  const columns = useMemo<ColumnDef<ProjectSandboxTemplate, any>[]>(() => [
    {
      id: "name", accessorFn: (template) => template.name, header: sortableHeader("Name"),
      cell: ({ row, getValue }) => <div className="grid gap-1"><span className="font-medium">{getValue()}</span><span className="font-mono text-xs text-muted-foreground">{row.original.id}</span>{!!row.original.aliases?.length && <div className="flex flex-wrap gap-1" aria-label="Aliases">{row.original.aliases.filter((alias) => alias !== row.original.name).map((alias) => <Badge key={alias} variant="outline">Alias: {alias}</Badge>)}</div>}</div>,
    },
    { id: "kind", accessorFn: (template) => template.mode === "import" ? "Imported" : template.kind === "snapshot" ? "Snapshot" : "Template", header: sortableHeader("Type"), cell: ({ getValue }) => <Badge variant="secondary">{getValue()}</Badge> },
    {
      id: "status", accessorFn: (template) => template.ready ? "ready" : template.status, header: sortableHeader("Status"),
      cell: ({ row, getValue }) => <div className="grid"><StatusBadge status={getValue()} />{row.original.ready && row.original.status !== "ready" && <span className="text-xs text-muted-foreground">Latest build: {row.original.status}</span>}</div>,
    },
    {
      id: "size", accessorFn: (template) => template.cpu, header: sortableHeader("Runtime size"),
      cell: ({ row }) => <span className="text-sm">{row.original.cpu} vCPU · {row.original.memoryGiB} GiB{row.original.gpu && <Badge className="ml-2" variant="outline">{row.original.gpu.count}× {row.original.gpu.model.replace(/^nvidia-/, "").toUpperCase()}</Badge>}</span>,
    },
    { id: "stored", accessorFn: (template) => template.sizeBytes ?? 0, header: sortableHeader("Stored"), cell: ({ row }) => <span className="text-sm text-muted-foreground">{row.original.kind === "snapshot" ? fmtSize(row.original.sizeBytes) : "—"}</span> },
    { id: "builds", accessorFn: (template) => template.builds.length, header: sortableHeader("Builds"), cell: ({ getValue }) => <span className="text-sm">{getValue()}</span> },
    { id: "updated", accessorFn: (template) => template.updatedAt ?? template.createdAt ?? "", header: sortableHeader("Updated"), cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() ? timeAgo(getValue()) : "—"}</span> },
    {
      id: "actions", header: () => null, enableSorting: false,
      cell: ({ row }) => {
        const template = row.original
        const busy = templateBusy(template)
        return <div className="flex justify-end"><DropdownMenu>
          <DropdownMenuTrigger asChild><Button variant="ghost" size="icon-sm" onFocus={rememberOpener} aria-label={`Actions for ${template.name}`}><MoreHorizontal aria-hidden="true" /></Button></DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={() => setAction({ type: "builds", template })}><ScrollText aria-hidden="true" /> Builds and logs</DropdownMenuItem>
            <DropdownMenuItem onSelect={() => setAction({ type: "aliases", template })} disabled={busy}><Tags aria-hidden="true" /> Manage aliases</DropdownMenuItem>
            <DropdownMenuItem onSelect={() => setAction({ type: "prepare", template })}><Zap aria-hidden="true" /> Prepare</DropdownMenuItem>
            <DropdownMenuItem onSelect={() => setAction({ type: "build", template })} disabled={!caps || !template.ready || busy}><Hammer aria-hidden="true" /> New build</DropdownMenuItem>
            <DropdownMenuItem onSelect={() => void copy(template.name)}><Copy aria-hidden="true" /> Copy name</DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" disabled={deleting} onSelect={() => { resetDelete(); setToDelete(template) }}><Trash2 aria-hidden="true" /> Delete</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu></div>
      },
    },
  ], [caps, deleting, resetDelete, rememberOpener, setAction, setToDelete])
  const importButton = <Button className="text-black" onFocus={rememberOpener} onClick={() => setImportOpen(true)} disabled={!canImport}><Import aria-hidden="true" /> Import image digest</Button>

  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <p className="max-w-2xl text-sm text-muted-foreground">Project templates, imported image digests and named snapshots. Start a sandbox by primary name or alias. Build tags select an artifact; aliases are separate names for the template.</p>
        <div className="flex flex-wrap gap-2"><Button variant="outline" disabled={list.isFetching || capabilities.isFetching} onClick={() => { void list.refetch(); void capabilities.refetch() }} aria-label="Refresh templates"><RefreshCw aria-hidden="true" /></Button>{importButton}</div>
      </div>
      {capabilities.isLoading ? <p className="text-xs text-muted-foreground" role="status">Loading location capabilities…</p>
        : capabilities.error ? <p className="text-sm text-red-700 dark:text-red-400" role="alert">Location capabilities are unavailable. Import and new builds are disabled; existing details remain accessible. <Button variant="outline" size="sm" onClick={() => void capabilities.refetch()}>Retry capabilities</Button></p>
          : !canImport ? <p className="text-xs text-muted-foreground" role="status">Digest import is disabled or its trusted worker is unavailable at this location.</p> : null}
      {list.data?.some((template) => templateBusy(template)) && <p className="text-xs text-muted-foreground">Templates with active builds are busy. Alias changes and new builds are disabled until the active operation finishes.</p>}
      {list.data && list.data.length === 0 ? <EmptyState icon={Layers} title="No templates yet" hint="Import an immutable image digest here, or build a template with the E2B SDK or CLI." action={importButton} />
        : <DataTable columns={columns} data={list.data ?? []} isLoading={list.isLoading} error={list.error as Error | null} />}
      {importOpen && caps && <SandboxTemplateImport pid={pid} capabilities={caps} onClose={closeImport} onImported={invalidate} />}
      {selected && action?.type === "builds" && <SandboxTemplateBuildsDialog key={selected.id} pid={pid} template={selected} onClose={closeAction} />}
      {selected && action?.type === "aliases" && <SandboxTemplateAliases key={selected.id} pid={pid} template={selected} onClose={closeAction} />}
      {selected && action?.type === "prepare" && <SandboxTemplatePrepare key={selected.id} pid={pid} template={selected} capabilities={caps} onClose={closeAction} />}
      {selected && action?.type === "build" && caps && <SandboxTemplateBuildDialog key={selected.id} pid={pid} template={selected} capabilities={caps} onClose={closeAction} onBuilt={() => { closeAction(); invalidate() }} />}
      <Dialog open={!!toDelete} onOpenChange={(open) => { if (!open) { setToDelete(null); del.reset(); restoreFocus() } }}>
        <DialogContent>
          <DialogHeader><DialogTitle>Delete {toDelete?.kind === "snapshot" ? "snapshot" : "template"}?</DialogTitle><DialogDescription>{toDelete ? `"${toDelete.name}" and all its aliases and builds are removed. New sandboxes can no longer start from it; running ones keep running.` : ""}</DialogDescription></DialogHeader>
          {del.error && <p className="text-sm text-red-700 dark:text-red-400" role="alert">{del.error instanceof ApiError && del.error.status === 409 ? "An active worker still uses this template. Nothing was deleted. Retry after the worker finishes." : del.error.message}</p>}
          <DialogFooter><Button variant="outline" onClick={() => { setToDelete(null); del.reset(); restoreFocus() }}>Cancel</Button><Button variant="outline" className="text-red-700 dark:text-red-400" disabled={del.isPending} onClick={() => toDelete && del.mutate(toDelete)}>{del.isPending ? "Deleting…" : "Delete"}</Button></DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
