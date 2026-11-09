import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { RefreshCw } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { apiFetch } from "@/lib/api"
import { fmtDateTime } from "@/lib/format"
import type { ProjectSandboxTemplate, SandboxTemplateCapabilities, SandboxTemplateCoverage } from "@/lib/types"
import { sandboxTemplatePath } from "./sandbox-template-utils"

const activeCoverage = (coverage?: SandboxTemplateCoverage) => !!coverage?.enabled && !!coverage.expiresAt && Date.parse(coverage.expiresAt) > Date.now()

export function SandboxTemplatePrepare({ pid, template, capabilities, onClose }: {
  pid: string; template: ProjectSandboxTemplate; capabilities?: SandboxTemplateCapabilities; onClose: () => void
}) {
  const qc = useQueryClient()
  const key = ["sandbox-template-coverage", pid, template.id]
  const enabled = !!capabilities?.warmingEnabled && template.ready
  const coverage = useQuery({
    queryKey: key,
    queryFn: () => apiFetch<SandboxTemplateCoverage>(sandboxTemplatePath(pid, template.id, "prepare")),
    enabled,
    refetchOnMount: "always",
    refetchInterval: (q) => enabled && activeCoverage(q.state.data) ? 5000 : false,
  })
  const prepare = useMutation({
    mutationFn: () => apiFetch<SandboxTemplateCoverage>(sandboxTemplatePath(pid, template.id, "prepare"), { method: "POST" }),
    onSuccess: (result) => qc.setQueryData(key, result),
  })
  const data = coverage.data
  const stale = !!data && !activeCoverage(data)
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-xl">
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
          {coverage.error && <p className="text-sm text-destructive" role="alert">Coverage is unavailable: {coverage.error.message}{data ? " Last known coverage is shown above and may be stale." : ""}</p>}
          {prepare.error && <p className="text-sm text-destructive" role="alert">{prepare.error.message}</p>}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Close</Button>
          <Button variant="outline" disabled={!enabled || coverage.isFetching || prepare.isPending} onClick={() => void coverage.refetch()}><RefreshCw aria-hidden="true" /> Refresh coverage</Button>
          <Button disabled={!enabled || prepare.isPending || data?.enabled === false} onClick={() => prepare.mutate()}>{prepare.isPending ? "Preparing…" : "Prepare image"}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
