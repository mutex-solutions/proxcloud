import { useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { ApiError, apiFetch } from "@/lib/api"
import type { SandboxTemplateActionResult, SandboxTemplateCapabilities } from "@/lib/types"
import { sandboxTemplatePath, templateNamePattern, validCommand, validImageSource, validRegistryCredentials, validRuntimeUser, validSize, validWorkdir } from "./sandbox-template-utils"

export function SandboxTemplateImport({ pid, capabilities, onClose, onImported }: {
  pid: string; capabilities: SandboxTemplateCapabilities; onClose: () => void; onImported: () => void
}) {
  const [source, setSource] = useState("")
  const [name, setName] = useState("")
  const [cpu, setCpu] = useState(String(capabilities.runtime.cpu))
  const [memory, setMemory] = useState(String(capabilities.runtime.memoryGiB))
  const [user, setUser] = useState("")
  const [workdir, setWorkdir] = useState("")
  const [start, setStart] = useState("")
  const [ready, setReady] = useState("")
  const [privateRegistry, setPrivateRegistry] = useState(false)
  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [pending, setPending] = useState(false)
  const [error, setError] = useState("")
  const mounted = useRef(true)
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  const clearCredentials = () => { setUsername(""); setPassword("") }
  const close = () => { clearCredentials(); onClose() }
  const enabled = capabilities.importEnabled && capabilities.trustedWorkerAvailable !== false
  const valid = enabled && validImageSource(source.trim()) && templateNamePattern.test(name.trim()) &&
    validSize(cpu, capabilities.runtime.maxCpu) && validSize(memory, capabilities.runtime.maxMemoryGiB) &&
    validCommand(start) && validCommand(ready) && validRuntimeUser(user) && validWorkdir(workdir) &&
    (!privateRegistry || validRegistryCredentials(username, password))

  // Deliberately not a React Query mutation: credentials must never enter its variables,
  // cache or retained error objects. Only a generic status-derived error reaches the UI.
  async function submit() {
    if (!valid || pending) return
    setPending(true); setError("")
    try {
      await apiFetch<SandboxTemplateActionResult>(sandboxTemplatePath(pid, "import"), {
        method: "POST",
        body: { source: source.trim(), name: name.trim(), cpuCount: Number(cpu), memoryMB: Number(memory) * 1024,
          ...(user ? { user } : {}), ...(workdir ? { workdir } : {}),
          ...(start ? { startCmd: start } : {}), ...(ready ? { readyCmd: ready } : {}),
          ...(privateRegistry ? { registry: { username, password } } : {}) },
      })
      // The server may finish after the dialog closes or the project changes. Invalidate the
      // captured project's list regardless; only component state and notifications need a mount.
      onImported()
      if (!mounted.current) return
      clearCredentials(); toast.success("Image import requested"); onClose()
    } catch (e) {
      if (!mounted.current) return
      setError(e instanceof ApiError && e.status === 409
        ? "Import conflicts with an existing name or active operation. Choose another name or retry later."
        : e instanceof ApiError && e.status === 503
          ? "Image import is unavailable or busy. Retry shortly."
          : "Image import failed. Check the digest, runtime settings and registry access, then retry.")
    } finally {
      if (mounted.current) setPending(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => !open && close()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Import image digest</DialogTitle>
          <DialogDescription>Import an immutable linux/amd64 image into this project. Image tags and other architectures are not supported. Closing this dialog clears credentials but does not stop an accepted import.</DialogDescription>
        </DialogHeader>
        <form className="grid gap-4" onSubmit={(e) => { e.preventDefault(); void submit() }}>
          <fieldset className="grid gap-4" disabled={pending || !enabled}>
            <div className="grid gap-2">
              <Label htmlFor="import-source">Image digest</Label>
              <Input id="import-source" value={source} onChange={(e) => setSource(e.target.value)} placeholder="registry.example.com/approved/image@sha256:…" autoComplete="off" spellCheck={false} required aria-describedby="import-digest-help" />
              <p id="import-digest-help" className="text-xs text-muted-foreground">Use repository@sha256 followed by 64 lowercase hexadecimal characters from an operator-approved registry. The server enforces approved origins and exact repositories for this project, platform and access; valid syntax does not grant permission. No URL schemes or tags.</p>
              {source && !validImageSource(source.trim()) && <p className="text-xs text-red-700 dark:text-red-400" role="status">Enter a repository and immutable sha256 digest, not a URL or image tag.</p>}
            </div>
            <div className="grid gap-2">
              <Label htmlFor="import-name">Template name</Label>
              <Input id="import-name" value={name} onChange={(e) => setName(e.target.value)} maxLength={63} placeholder="my-environment" required />
              <p className="text-xs text-muted-foreground">1–63 lowercase letters, digits, dots, underscores or hyphens; start with a letter or digit. The primary name is immutable.</p>
            </div>
            <fieldset className="grid gap-3 rounded-md border p-3">
              <legend className="px-1 text-sm font-medium">Runtime size</legend>
              <div className="grid grid-cols-2 gap-3">
                <div className="grid gap-2"><Label htmlFor="import-cpu">Runtime vCPU</Label><Input id="import-cpu" type="number" min={1} max={capabilities.runtime.maxCpu} step={1} value={cpu} onChange={(e) => setCpu(e.target.value)} required /></div>
                <div className="grid gap-2"><Label htmlFor="import-memory">Runtime memory (GiB)</Label><Input id="import-memory" type="number" min={1} max={capabilities.runtime.maxMemoryGiB} step={1} value={memory} onChange={(e) => setMemory(e.target.value)} required /></div>
              </div>
              <p className="text-xs text-muted-foreground">Location limits: {capabilities.runtime.maxCpu} vCPU / {capabilities.runtime.maxMemoryGiB} GiB. These size sandboxes, not the import worker.</p>
            </fieldset>
            <details className="rounded-md border p-3">
              <summary className="cursor-pointer text-sm font-medium">Runtime settings (optional)</summary>
              <div className="mt-3 grid gap-3">
                <div className="grid gap-2"><Label htmlFor="import-user">User</Label><Input id="import-user" value={user} onChange={(e) => setUser(e.target.value)} maxLength={32} placeholder="app" aria-describedby="import-user-help" /><p id="import-user-help" className="text-xs text-muted-foreground">Named users only (for example root or app); numeric UID and USER:GROUP are unsupported. Blank inherits the image user.</p>{!validRuntimeUser(user) && <p className="text-xs text-red-700 dark:text-red-400" role="status">Use a named user starting with a letter or underscore.</p>}</div>
                <div className="grid gap-2"><Label htmlFor="import-workdir">Working directory</Label><Input id="import-workdir" value={workdir} onChange={(e) => setWorkdir(e.target.value)} maxLength={4096} placeholder="/workspace" /></div>
                <div className="grid gap-2"><Label htmlFor="import-start">Start command</Label><Input id="import-start" value={start} onChange={(e) => setStart(e.target.value)} maxLength={32768} /></div>
                <div className="grid gap-2"><Label htmlFor="import-ready">Ready command</Label><Input id="import-ready" value={ready} onChange={(e) => setReady(e.target.value)} maxLength={32768} /></div>
              </div>
            </details>
            <div className="flex items-center gap-3"><Switch id="import-private" checked={privateRegistry} onCheckedChange={(checked) => { setPrivateRegistry(checked); clearCredentials() }} /><Label htmlFor="import-private">Private registry</Label></div>
            {privateRegistry && <div className="grid gap-3 rounded-md border p-3">
              <div className="grid gap-2"><Label htmlFor="import-username">Registry username</Label><Input id="import-username" autoComplete="off" value={username} maxLength={1024} onChange={(e) => setUsername(e.target.value)} required /></div>
              <div className="grid gap-2"><Label htmlFor="import-password">Registry password</Label><Input id="import-password" type="password" autoComplete="new-password" value={password} maxLength={32768} onChange={(e) => setPassword(e.target.value)} required /></div>
              <p className="text-xs text-muted-foreground">Sent only with this import request; not saved in browser storage. Cleared on close or success. Never put credentials in the image reference or commands.</p>
            </div>}
          </fieldset>
          {!enabled && <p className="text-sm text-muted-foreground" role="status">Digest import is disabled or its trusted worker is unavailable at this location.</p>}
          {error && <p className="text-sm text-red-700 dark:text-red-400" role="alert">{error}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={close}>Cancel</Button>
            <Button type="submit" className="text-black" disabled={!valid || pending}>{pending ? "Importing…" : "Import image"}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
