import { useEffect, useRef, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Plus, Trash2 } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { apiFetch } from "@/lib/api"
import type { ProjectSandboxTemplate } from "@/lib/types"
import { sandboxTemplatePath, templateNamePattern } from "./sandbox-template-utils"

export function SandboxTemplateAliases({ pid, template, onClose }: { pid: string; template: ProjectSandboxTemplate; onClose: () => void }) {
  const qc = useQueryClient()
  const mounted = useRef(true)
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  const [aliases, setAliases] = useState((template.aliases ?? []).filter((a) => a !== template.name))
  const [name, setName] = useState("")
  const [confirm, setConfirm] = useState<{ alias: string; remove: boolean } | null>(null)
  const update = useMutation({
    mutationFn: ({ alias, remove }: { alias: string; remove: boolean }) => apiFetch<{ aliases: string[] }>(sandboxTemplatePath(pid, template.id, "aliases"), { method: remove ? "DELETE" : "POST", body: { aliases: [alias] } }),
    onSuccess: (result) => {
      void qc.invalidateQueries({ queryKey: ["sandbox-templates", pid] })
      if (!mounted.current) return
      setAliases(result.aliases.filter((a) => a !== template.name)); setName(""); setConfirm(null)
      toast.success("Template aliases updated")
    },
  })
  const valid = templateNamePattern.test(name.trim()) && name.trim() !== template.name && !aliases.includes(name.trim()) && aliases.length < 19
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Manage aliases</DialogTitle>
          <DialogDescription>Aliases are extra names for this project's template. They are not build tags: tags select a build; aliases select a template.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2"><Label htmlFor="alias-primary">Primary name (immutable)</Label><Input id="alias-primary" value={template.name} readOnly /><p className="text-xs text-muted-foreground">The primary name cannot be renamed or removed here.</p></div>
          <ul className="grid gap-2" aria-label="Template aliases">
            {aliases.map((alias) => <li key={alias} className="flex items-center justify-between gap-2 rounded-md border p-2"><Badge variant="secondary" className="break-all">{alias}</Badge><Button variant="ghost" size="sm" disabled={update.isPending || !!confirm} aria-label={`Remove alias ${alias}`} onClick={() => { update.reset(); setConfirm({ alias, remove: true }) }}><Trash2 aria-hidden="true" /> Remove</Button></li>)}
          </ul>
          {!aliases.length && <p className="text-sm text-muted-foreground">No additional aliases.</p>}
          <form className="grid gap-2" onSubmit={(e) => { e.preventDefault(); if (valid && !update.isPending) { update.reset(); setConfirm({ alias: name.trim(), remove: false }) } }}>
            <Label htmlFor="alias-name">New alias</Label>
            <div className="flex flex-wrap gap-2"><Input className="min-w-0 flex-1 basis-40" id="alias-name" value={name} onChange={(e) => setName(e.target.value)} maxLength={63} disabled={update.isPending || !!confirm} placeholder="my-environment-stable" aria-describedby="alias-help" /><Button type="submit" variant="outline" disabled={!valid || update.isPending || !!confirm}><Plus aria-hidden="true" /> Add alias</Button></div>
            <p id="alias-help" className="text-xs text-muted-foreground">1–63 lowercase letters, digits, dots, underscores or hyphens, starting with a letter or digit. Up to 19 additional aliases. Names must be unique within this project and not built-in names.</p>
          </form>
          {confirm && <div className="grid gap-3 rounded-md border p-3" role="group" aria-label="Confirm alias change">
            <p className="text-sm">{confirm.remove ? `Remove alias "${confirm.alias}"? New sandboxes will no longer resolve this name.` : `Add alias "${confirm.alias}" to ${template.name}?`}</p>
            <div className="flex flex-wrap gap-2"><Button size="sm" variant="outline" className={confirm.remove ? "text-red-700 dark:text-red-400" : undefined} disabled={update.isPending} onClick={() => update.mutate(confirm)}>{update.isPending ? "Saving…" : confirm.remove ? "Confirm remove" : "Confirm add"}</Button><Button size="sm" variant="outline" disabled={update.isPending} onClick={() => { setConfirm(null); update.reset() }}>Cancel change</Button></div>
          </div>}
          {update.error && <p className="text-sm text-red-700 dark:text-red-400" role="alert">{update.error.message}</p>}
        </div>
        <DialogFooter><Button variant="outline" onClick={onClose}>Close</Button></DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
