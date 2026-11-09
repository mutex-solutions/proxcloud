import { useEffect } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { RefreshCw } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { StatusBadge } from "@/components/status-badge"
import { apiFetch } from "@/lib/api"
import { fmtDateTime } from "@/lib/format"
import type { SandboxBuildLogs, SandboxTemplateBuild } from "@/lib/types"
import { activeBuild, plainBuildText, sandboxTemplatePath } from "./sandbox-template-utils"

export function SandboxTemplateBuildDiagnostics({ pid, templateId, build }: { pid: string; templateId: string; build: SandboxTemplateBuild }) {
  const qc = useQueryClient()
  const logs = useQuery({
    queryKey: ["sandbox-template-logs", pid, templateId, build.buildID],
    queryFn: () => apiFetch<SandboxBuildLogs>(`${sandboxTemplatePath(pid, templateId, "builds", build.buildID, "logs")}?logsOffset=0`),
    refetchOnMount: "always",
    refetchInterval: (query) => {
      const data = query.state.data
      if (query.state.error) return false
      if (activeBuild(data?.status ?? build.status)) return 3000
      // A terminal worker may still be collecting its final log tail. Imported images
      // and old records without diagnostics do not have a worker archive to wait for.
      return data?.diagnostics && !data.diagnostics.archivedAt ? 3000 : false
    },
  })
  useEffect(() => {
    if (logs.data?.status && logs.data.status !== build.status) void qc.invalidateQueries({ queryKey: ["sandbox-templates", pid] })
  }, [logs.data?.status, build.status, qc, pid])
  const data = logs.data
  const diagnostics = data?.diagnostics ?? build.diagnostics
  const status = data?.status ?? build.status
  const lines = data?.logs ?? []
  return (
    <section className="grid min-w-0 gap-3 border-t pt-3" aria-label={`Diagnostics for ${build.buildID}`}>
      <div className="flex flex-wrap items-center gap-2">
        <StatusBadge status={status} />
        {(data?.step ?? build.step) && <Badge variant="outline">Phase: {plainBuildText(data?.step ?? build.step ?? "")}</Badge>}
        {diagnostics?.truncated && <Badge variant="outline">Logs truncated</Badge>}
        {diagnostics?.logsUnavailable && <Badge variant="outline">Logs unavailable</Badge>}
        <Button size="sm" variant="outline" className="ml-auto" disabled={logs.isFetching} onClick={() => void logs.refetch()}><RefreshCw aria-hidden="true" /> Refresh diagnostics</Button>
      </div>
      {data?.buildResources && <p className="text-xs text-muted-foreground">Builder: {data.buildResources.buildCpu} vCPU · {data.buildResources.buildMemoryGiB} GiB memory · {data.buildResources.buildDiskGiB} GiB disk</p>}
      {logs.isLoading && <p className="text-sm text-muted-foreground">Loading diagnostics…</p>}
      {logs.error && <p className="text-sm text-destructive" role="alert">Diagnostics could not be refreshed: {plainBuildText(logs.error.message)}{data ? " Last known details remain visible." : " Build metadata remains available above."}</p>}
      {(data?.reason ?? build.reason) && <p className="break-words text-sm text-destructive">{plainBuildText(data?.reason ?? build.reason ?? "")}</p>}
      {diagnostics ? <>
        {(diagnostics.podReason || diagnostics.podMessage) && <p className="break-words text-sm">{plainBuildText([diagnostics.podReason, diagnostics.podMessage].filter(Boolean).join(": "))}</p>}
        {!!diagnostics.stages?.length && <ul className="grid gap-2" aria-label="Build phases">
          {diagnostics.stages.map((stage, index) => <li key={`${stage.phase}-${index}`} className="grid gap-1 rounded-md border p-3 text-sm">
            <div className="flex flex-wrap gap-2"><span className="font-medium">{plainBuildText(stage.phase)}</span><StatusBadge status={stage.state} />{stage.exitCode !== undefined && <Badge variant={stage.exitCode === 0 ? "secondary" : "destructive"}>Exit code: {stage.exitCode}</Badge>}{stage.signal !== undefined && stage.signal !== 0 && <Badge variant="outline">Signal: {stage.signal}</Badge>}</div>
            {(stage.reason || stage.message) && <p className="break-words text-muted-foreground">{plainBuildText([stage.reason, stage.message].filter(Boolean).join(": "))}</p>}
            {(stage.cpu || stage.memory || stage.disk) && <p className="text-xs text-muted-foreground">Resources: {plainBuildText([stage.cpu && `CPU ${stage.cpu}`, stage.memory && `memory ${stage.memory}`, stage.disk && `disk ${stage.disk}`].filter(Boolean).join(" · "))}</p>}
          </li>)}
        </ul>}
        {!!diagnostics.warnings?.length && <ul className="grid gap-1 text-sm text-muted-foreground" aria-label="Diagnostic warnings">{diagnostics.warnings.map((warning, index) => <li key={index}>Warning: {plainBuildText(warning)}</li>)}</ul>}
        {diagnostics.logsUnavailable && <p className="text-sm text-muted-foreground">Some build output is unavailable.{!diagnostics.archivedAt ? " Capture will be retried while this panel is open." : " The archive is final; missing output cannot be recovered here."}</p>}
        {diagnostics.lossReason && <p className="text-sm text-muted-foreground">{plainBuildText(diagnostics.lossReason)}</p>}
        <p className="text-xs text-muted-foreground">{diagnostics.archivedAt ? `Archive finalized ${fmtDateTime(diagnostics.archivedAt)}.` : activeBuild(status) ? "Live capture; refreshes while this panel is open." : "Collecting final output; refreshes until the archive is finalized."}{diagnostics.lastCaptureAt ? ` Last capture ${fmtDateTime(diagnostics.lastCaptureAt)}.` : ""}</p>
      </> : !logs.isLoading && <p className="text-sm text-muted-foreground">Detailed phase diagnostics are not available for this build. Retained logs are shown below.</p>}
      <div className="grid gap-2"><h4 className="text-sm font-medium">Build logs</h4><pre className="max-h-80 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap break-words" aria-label="Build logs">{lines.length ? plainBuildText(lines.join("\n")) : logs.isLoading ? "Loading logs…" : "No logs kept for this build."}</pre></div>
      {!!diagnostics?.terminalTail?.length && <div className="grid gap-2"><h4 className="text-sm font-medium">Final log tail</h4><p className="text-xs text-muted-foreground">Final failure context retained separately when earlier output was truncated.</p><pre className="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap break-words" aria-label="Final log tail">{plainBuildText(diagnostics.terminalTail.join("\n"))}</pre></div>}
    </section>
  )
}
