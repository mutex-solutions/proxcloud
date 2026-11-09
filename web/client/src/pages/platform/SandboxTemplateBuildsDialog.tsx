import { useState } from "react"
import { ScrollText } from "lucide-react"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { fmtDateTime } from "@/lib/format"
import type { ProjectSandboxTemplate } from "@/lib/types"
import { SandboxTemplateBuildDiagnostics } from "./SandboxTemplateBuildDiagnostics"
import { plainBuildText } from "./sandbox-template-utils"

export function SandboxTemplateBuildsDialog({ pid, template, onClose }: { pid: string; template: ProjectSandboxTemplate; onClose: () => void }) {
  const [expanded, setExpanded] = useState<string | null>(null)
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>{template.kind === "snapshot" ? "Snapshot" : "Builds"}: {template.name}</DialogTitle>
          <DialogDescription>{template.kind === "snapshot" ? `Taken from ${template.sourceSandbox || "a sandbox"}. Start a sandbox from it by name.` : "Newest first. Build tags select an artifact; template aliases are managed separately."}</DialogDescription>
        </DialogHeader>
        {template.ready && template.status !== "ready" && <p className="text-sm text-muted-foreground">An older successful build is still usable. The latest build is {template.status}.</p>}
        <ul className="grid gap-3">
          {template.builds.map((build) => <li key={build.buildID} className="grid min-w-0 gap-2 rounded-md border p-3">
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
              <StatusBadge status={build.status} />
              {build.mode === "import" && <Badge variant="outline">Imported</Badge>}
              {build.tags.map((tag) => <Badge key={tag} variant="secondary">Tag: {tag}</Badge>)}
              <span className="break-all font-mono text-xs text-muted-foreground">{build.buildID}</span>
              <span className="ml-auto text-xs text-muted-foreground">{fmtDateTime(build.createdAt)}{build.finishedAt && build.createdAt ? ` · ${Math.max(1, Math.round((Date.parse(build.finishedAt) - Date.parse(build.createdAt)) / 1000))} s` : ""}</span>
            </div>
            {build.reason && <p className="break-words text-sm text-red-700 dark:text-red-400">{plainBuildText(build.reason)}</p>}
            {build.buildResources && build.buildResources.buildCpu > 0 && <p className="text-xs text-muted-foreground">Builder: {build.buildResources.buildCpu} vCPU · {build.buildResources.buildMemoryGiB} GiB memory · {build.buildResources.buildDiskGiB} GiB disk</p>}
            {build.source && <p className="break-all font-mono text-xs text-muted-foreground">Source: {plainBuildText(build.source)}</p>}
            {build.image && <p className="break-all font-mono text-xs text-muted-foreground">Image: {plainBuildText(build.image)}</p>}
            {build.startCmd && <p className="break-words font-mono text-xs text-muted-foreground">start: {plainBuildText(build.startCmd)}</p>}
            {build.readyCmd && <p className="break-words font-mono text-xs text-muted-foreground">ready: {plainBuildText(build.readyCmd)}</p>}
            <div><Button variant="outline" size="sm" aria-expanded={expanded === build.buildID} onClick={() => setExpanded(expanded === build.buildID ? null : build.buildID)}><ScrollText aria-hidden="true" />{expanded === build.buildID ? "Hide diagnostics and logs" : "Diagnostics and logs"}</Button></div>
            {expanded === build.buildID && <SandboxTemplateBuildDiagnostics pid={pid} templateId={template.id} build={build} />}
          </li>)}
        </ul>
        {!template.builds.length && <p className="text-sm text-muted-foreground">No builds recorded.</p>}
        <DialogFooter><Button variant="outline" onClick={onClose}>Close</Button></DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
