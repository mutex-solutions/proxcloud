import { useEffect, useRef, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Plus, Trash2 } from "lucide-react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import type { ProjectSandboxTemplate, SandboxTemplateActionResult, SandboxTemplateCapabilities } from "@/lib/types"
import { sandboxTemplatePath, templateNamePattern, validCommand, validSize } from "./sandbox-template-utils"

export function SandboxTemplateBuildDialog({ pid, template, capabilities, onClose, onBuilt }: {
  pid: string; template: ProjectSandboxTemplate; capabilities: SandboxTemplateCapabilities; onClose: () => void; onBuilt: (id: string) => void
}) {
  const qc = useQueryClient()
  const mounted = useRef(true)
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  const [name, setName] = useState("")
  const [cpu, setCpu] = useState(String(template.cpu))
  const [memory, setMemory] = useState(String(template.memoryGiB))
  const [buildCpu, setBuildCpu] = useState(String(capabilities.build.cpu))
  const [buildMemory, setBuildMemory] = useState(String(capabilities.build.memoryGiB))
  const [buildDisk, setBuildDisk] = useState(String(capabilities.build.diskGiB))
  const [steps, setSteps] = useState([""])
  const [start, setStart] = useState("")
  const [ready, setReady] = useState("")
  const valid = template.ready && templateNamePattern.test(name.trim()) && name.trim() !== template.name && !(template.aliases ?? []).includes(name.trim()) &&
    validSize(cpu, capabilities.runtime.maxCpu) && validSize(memory, capabilities.runtime.maxMemoryGiB) &&
    validSize(buildCpu, capabilities.build.maxCpu) && validSize(buildMemory, capabilities.build.maxMemoryGiB) && validSize(buildDisk, capabilities.build.maxDiskGiB) &&
    steps.length > 0 && steps.length <= 64 && steps.every((step) => !!step.trim() && validCommand(step)) && validCommand(start) && validCommand(ready)
  const build = useMutation({
    mutationFn: () => apiFetch<SandboxTemplateActionResult>(sandboxTemplatePath(pid, template.id, "builds"), {
      method: "POST", body: { name: name.trim(), cpuCount: Number(cpu), memoryMB: Number(memory) * 1024,
        steps: steps.map((command) => ({ type: "RUN", args: [command] })),
        buildCpu: Number(buildCpu), buildMemoryGiB: Number(buildMemory), buildDiskGiB: Number(buildDisk),
        ...(start ? { startCmd: start } : {}), ...(ready ? { readyCmd: ready } : {}) },
    }),
    onSuccess: (result) => {
      // Accepted work can outlive this dialog. Always refresh the captured project,
      // but never close a newer dialog or notify after a project switch.
      void qc.invalidateQueries({ queryKey: ["sandbox-templates", pid] })
      if (mounted.current) { toast.success("Template build requested"); onBuilt(result.templateID) }
    },
  })
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>New build</DialogTitle>
          <DialogDescription>Create a new template from a ready project template. Imports stay immutable: a build always needs a different name. The server selects the ready base artifact. Closing this dialog does not stop an accepted build.</DialogDescription>
        </DialogHeader>
        <form className="grid gap-4" onSubmit={(e) => { e.preventDefault(); if (valid && !build.isPending) build.mutate() }}>
          <fieldset className="grid gap-4" disabled={build.isPending || !template.ready}>
            <div className="grid gap-2"><Label htmlFor="build-base">Base template</Label><Input id="build-base" value={template.name} readOnly /></div>
            <div className="grid gap-2"><Label htmlFor="build-name">New template name</Label><Input id="build-name" value={name} onChange={(e) => setName(e.target.value)} maxLength={63} required /><p className="text-xs text-muted-foreground">1–63 lowercase letters, digits, dots, underscores or hyphens, starting with a letter or digit. Must differ from the base name and aliases.</p></div>
            <fieldset className="grid gap-3 rounded-md border p-3">
              <legend className="px-1 text-sm font-medium">Runtime size</legend>
              <div className="grid grid-cols-2 gap-3">
                <div className="grid gap-2"><Label htmlFor="build-runtime-cpu">Runtime vCPU</Label><Input id="build-runtime-cpu" type="number" min={1} max={capabilities.runtime.maxCpu} step={1} value={cpu} onChange={(e) => setCpu(e.target.value)} required /></div>
                <div className="grid gap-2"><Label htmlFor="build-runtime-memory">Runtime memory (GiB)</Label><Input id="build-runtime-memory" type="number" min={1} max={capabilities.runtime.maxMemoryGiB} step={1} value={memory} onChange={(e) => setMemory(e.target.value)} required /></div>
              </div>
              <p className="text-xs text-muted-foreground">Sizes future sandboxes only. Up to {capabilities.runtime.maxCpu} vCPU / {capabilities.runtime.maxMemoryGiB} GiB.</p>
            </fieldset>
            <fieldset className="grid gap-3 rounded-md border p-3">
              <legend className="px-1 text-sm font-medium">Builder resources</legend>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                <div className="grid gap-2"><Label htmlFor="build-cpu">Builder vCPU</Label><Input id="build-cpu" type="number" min={1} max={capabilities.build.maxCpu} step={1} value={buildCpu} onChange={(e) => setBuildCpu(e.target.value)} required /></div>
                <div className="grid gap-2"><Label htmlFor="build-memory">Builder memory (GiB)</Label><Input id="build-memory" type="number" min={1} max={capabilities.build.maxMemoryGiB} step={1} value={buildMemory} onChange={(e) => setBuildMemory(e.target.value)} required /></div>
                <div className="grid gap-2"><Label htmlFor="build-disk">Builder disk (GiB)</Label><Input id="build-disk" type="number" min={1} max={capabilities.build.maxDiskGiB} step={1} value={buildDisk} onChange={(e) => setBuildDisk(e.target.value)} required /></div>
              </div>
              <p className="text-xs text-muted-foreground">Independent of runtime size. Location limits: {capabilities.build.maxCpu} vCPU / {capabilities.build.maxMemoryGiB} GiB memory / {capabilities.build.maxDiskGiB} GiB disk; {capabilities.build.maxConcurrent} concurrent workers. Admission and availability are checked by the server.</p>
            </fieldset>
            <fieldset className="grid gap-3 rounded-md border p-3">
              <legend className="px-1 text-sm font-medium">RUN steps</legend>
              {steps.map((command, index) => <div key={index} className="grid gap-2">
                <Label htmlFor={`build-run-${index}`}>RUN command {index + 1}</Label>
                <div className="flex items-start gap-2"><Textarea id={`build-run-${index}`} className="min-h-20 min-w-0 font-mono text-xs" value={command} onChange={(e) => setSteps(steps.map((step, i) => i === index ? e.target.value : step))} placeholder="python -m pip install numpy" maxLength={32768} required /><Button type="button" size="icon-sm" variant="ghost" aria-label={`Remove RUN step ${index + 1}`} disabled={steps.length === 1} onClick={() => setSteps(steps.filter((_, i) => i !== index))}><Trash2 aria-hidden="true" /></Button></div>
              </div>)}
              <Button type="button" variant="outline" className="justify-self-start" disabled={steps.length >= 64} onClick={() => setSteps([...steps, ""])}><Plus aria-hidden="true" /> Add RUN step</Button>
              <p className="text-xs text-muted-foreground">Commands execute in order during the build, not at sandbox startup. Do not put passwords, tokens or registry credentials in commands; command text and output can be retained in build history.</p>
            </fieldset>
            <details className="rounded-md border p-3"><summary className="cursor-pointer text-sm font-medium">Startup settings (optional)</summary><div className="mt-3 grid gap-3">
              <div className="grid gap-2"><Label htmlFor="build-start">Start command</Label><Input id="build-start" value={start} onChange={(e) => setStart(e.target.value)} maxLength={32768} /></div>
              <div className="grid gap-2"><Label htmlFor="build-ready">Ready command</Label><Input id="build-ready" value={ready} onChange={(e) => setReady(e.target.value)} maxLength={32768} /></div>
              <p className="text-xs text-muted-foreground">Blank values inherit the base template's startup settings.</p>
            </div></details>
          </fieldset>
          {!template.ready && <p className="text-sm text-muted-foreground" role="status">The base template has no ready artifact.</p>}
          {build.error && <p className="text-sm text-red-700 dark:text-red-400" role="alert">{build.error.message}</p>}
          <DialogFooter><Button type="button" variant="outline" onClick={onClose}>Cancel</Button><Button type="submit" className="text-black" disabled={!valid || build.isPending}>{build.isPending ? "Building…" : "Start build"}</Button></DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
