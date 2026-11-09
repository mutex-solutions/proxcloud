import { useEffect, useRef } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { RefreshCw } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { apiFetch } from "@/lib/api"
import { fmtDateTime } from "@/lib/format"
import type { ProjectSandboxTemplate, SandboxTemplateCapabilities, SandboxTemplateCoverage } from "@/lib/types"
import { sandboxTemplatePath, templateAccessDenied } from "./sandbox-template-utils"

const activeCoverage = (coverage?: SandboxTemplateCoverage) => !!coverage?.enabled && !!coverage.expiresAt && Date.parse(coverage.expiresAt) > Date.now()

export function SandboxTemplatePrepare({ pid, template, capabilities, onClose, onCloseAutoFocus, onAccessDenied }: {
  pid: string; template: ProjectSandboxTemplate; capabilities?: SandboxTemplateCapabilities; onClose: () => void; onCloseAutoFocus?: (event: Event) => void; onAccessDenied?: () => void
}) {
  const qc = useQueryClient()
  const mounted = useRef(true)
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  const key = ["sandbox-template-coverage", pid, template.id]
  const enabled = !!capabilities?.warmingEnabled && template.ready
  const coverage = useQuery({
    queryKey: key,
    queryFn: async () => {
      try { return await apiFetch<SandboxTemplateCoverage>(sandboxTemplatePath(pid, template.id, "prepare")) }
      catch (error) { if (templateAccessDenied(error)) onAccessDenied?.(); throw error }
    },
    enabled,
    retry: (attempt, error) => !templateAccessDenied(error) && attempt < 1,
    refetchOnMount: "always",
    refetchInterval: (q) => enabled && !templateAccessDenied(q.state.error) && activeCoverage(q.state.data) ? 5000 : false,
  })
  const prepare = useMutation({
    // Cancel the older observer before admitting new intent. React Query's
    // cancellation fence prevents an ignored/slow transport response from
    // overwriting the POST result with a pre-prepare coverage snapshot.
    onMutate: () => qc.cancelQueries({ queryKey: key, exact: true }),
    mutationFn: () => {
      if (!mounted.current || templateAccessDenied(coverage.error)) throw new Error("Preparation is no longer authorized")
      return apiFetch<SandboxTemplateCoverage>(sandboxTemplatePath(pid, template.id, "prepare"), { method: "POST" })
    },
    onSuccess: (result) => { if (mounted.current) qc.setQueryData(key, result) },
    onError: (error) => { if (templateAccessDenied(error)) onAccessDenied?.() },
  })
  const denied = templateAccessDenied(coverage.error) || templateAccessDenied(prepare.error)
  useEffect(() => { if (denied) onAccessDenied?.() }, [denied, onAccessDenied])
  if (denied) return <p role="alert">Project cloud resource management permission required.</p>
  const data = coverage.data
  const stale = !!data && !activeCoverage(data)
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent onCloseAutoFocus={onCloseAutoFocus} className="max-h-[90dvh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Prepare: {template.name}</DialogTitle>
          <DialogDescription>Request a temporary image pre-pull on the eligible runtime pool. Coverage is a point-in-time observation, not a startup-time guarantee or SLA.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-3" aria-live="polite">
          {!capabilities ? <p className="text-sm text-muted-foreground">Location capabilities are unavailable. Preparation cannot be requested.</p>
            : !capabilities.warmingEnabled ? <p className="text-sm text-muted-foreground">Image preparation is disabled at this location.</p>
              : !template.ready ? <p className="text-sm text-muted-foreground">This template has no ready artifact. Wait for a successful build or import before preparing it.</p>
                : coverage.isLoading ? <p className="text-sm text-muted-foreground">Loading coverage…</p> : null}
          {data && <div className="grid gap-3 rounded-md border p-4">
            <p className="text-lg font-medium">Coverage: {data.pulled}/{data.total} nodes</p>
            {!data.enabled ? <p className="text-sm text-muted-foreground">Preparation is disabled; coverage is not available.</p>
              : stale ? <p className="text-sm text-muted-foreground">No active preparation, or the preparation has expired. Shown coverage is stale; prepare again to refresh intent.</p>
                : data.total === 0 ? <p className="text-sm text-muted-foreground">No eligible nodes are available. 0/0 is not ready coverage.</p>
                  : <p className="text-sm text-muted-foreground">{data.ready ? "Image pulled on the current eligible nodes." : "Waiting for image pulls on eligible nodes."} Coverage refreshes while this dialog is open and preparation is active.</p>}
            <dl className="grid gap-2 text-sm">
              <div><dt className="text-muted-foreground">Pool</dt><dd>{data.pool || "Unavailable"}</dd></div>
              <div><dt className="text-muted-foreground">Image</dt><dd className="break-all font-mono text-xs">{data.image || "Unavailable"}</dd></div>
              <div><dt className="text-muted-foreground">Preparation expires</dt><dd>{data.expiresAt && Date.parse(data.expiresAt) > 0 ? fmtDateTime(data.expiresAt) : "No active preparation"}</dd></div>
            </dl>
          </div>}
          {coverage.error && <p className="text-sm text-red-700 dark:text-red-400" role="alert">Coverage is unavailable: {coverage.error.message}{data ? " Last known coverage is shown above and may be stale." : ""}</p>}
          {prepare.error && <p className="text-sm text-red-700 dark:text-red-400" role="alert">{prepare.error.message}</p>}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Close</Button>
          <Button variant="outline" className="transition-colors dark:bg-background dark:text-foreground dark:hover:bg-background dark:hover:text-foreground" disabled={!enabled || coverage.isFetching || prepare.isPending} onClick={() => void coverage.refetch()}><RefreshCw aria-hidden="true" /> Refresh coverage</Button>
          <Button className="text-black" disabled={!enabled || prepare.isPending || data?.enabled === false} onClick={() => prepare.mutate()}>{prepare.isPending ? "Preparing…" : "Prepare image"}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
