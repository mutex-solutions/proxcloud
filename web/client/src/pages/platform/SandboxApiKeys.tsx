// API keys for the E2B-compatible sandbox API (/e2b), plus the "Use with the E2B SDK" setup panel. A key belongs to this
// project and acts as its creator; only a hint of it is kept after it is shown once (backend: e2b_keys.go).
import { useId, useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Copy, KeyRound, Plus, Trash2 } from "lucide-react"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { apiFetch } from "@/lib/api"
import { config } from "@/lib/config"
import { timeAgo } from "@/lib/format"
import type { SandboxApiKey } from "@/lib/types"

async function copy(label: string, value: string) {
  try {
    await navigator.clipboard.writeText(value)
    toast.success(`${label} copied`)
  } catch {
    toast.error(`Couldn't copy ${label}. Select and copy it manually.`)
  }
}

function Snippet({ label, text }: { label: string; text: string }) {
  return (
    <div className="relative">
      <pre className="overflow-auto rounded-md bg-muted p-3 pr-12 font-mono text-xs">{text}</pre>
      <Button variant="ghost" size="icon-sm" className="absolute top-1.5 right-1.5" onClick={() => void copy(label, text)} aria-label={`Copy ${label}`}>
        <Copy className="size-4" aria-hidden="true" />
      </Button>
    </div>
  )
}

// The E2B API lives next to /api/v1 on the same host.
export function e2bApiUrl(apiUrl = config.apiUrl): string {
  return apiUrl.replace(/\/api\/v1\/?$/, "") + "/e2b"
}

export function ApiKeysTab({ pid, e2b }: { pid: string; e2b: boolean }) {
  const qc = useQueryClient()
  const nameId = useId()
  const keyId = useId()
  const keys = useQuery({
    queryKey: ["sandbox-api-keys", pid],
    queryFn: () => apiFetch<SandboxApiKey[]>(`/project/${pid}/sandbox-api-keys`),
    enabled: !!pid,
  })
  const invalidate = () => void qc.invalidateQueries({ queryKey: ["sandbox-api-keys", pid] })

  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState("")
  const [created, setCreated] = useState<string | null>(null) // the full key, shown once
  const [toRevoke, setToRevoke] = useState<SandboxApiKey | null>(null)

  const closeCreate = () => { setCreateOpen(false); setName(""); setCreated(null) }
  const create = useMutation({
    mutationFn: () => apiFetch<{ key: string }>(`/project/${pid}/sandbox-api-keys`, { method: "POST", body: name.trim() ? { name: name.trim() } : {} }),
    onSuccess: (r) => { setCreated(r.key); invalidate() },
    onError: (e: Error) => toast.error(e.message),
  })
  const revoke = useMutation({
    mutationFn: (k: SandboxApiKey) => apiFetch(`/project/${pid}/sandbox-api-keys/${k.id}`, { method: "DELETE" }),
    onSuccess: () => { toast.success("API key revoked"); setToRevoke(null); invalidate() },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<SandboxApiKey, any>[]>(
    () => [
      { id: "name", accessorFn: (k) => k.name, header: sortableHeader("Name"), cell: ({ getValue }) => <span className="font-medium">{getValue() || "—"}</span> },
      { id: "hint", accessorFn: (k) => k.hint, header: "Key", cell: ({ getValue }) => <span className="font-mono text-xs">{getValue()}</span> },
      { id: "created", accessorFn: (k) => k.createdAt, header: sortableHeader("Created"), cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span> },
      { id: "lastUsed", accessorFn: (k) => k.lastUsedAt ?? "", header: sortableHeader("Last used"), cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() ? timeAgo(getValue()) : "Never"}</span> },
      {
        id: "actions", header: () => null, enableSorting: false,
        cell: ({ row }) => (
          <div className="flex justify-end">
            <Button variant="ghost" size="sm" onClick={() => setToRevoke(row.original)} aria-label={`Revoke API key ${row.original.name || row.original.hint}`}>
              <Trash2 aria-hidden="true" /> Revoke
            </Button>
          </div>
        ),
      },
    ],
    [],
  )

  const env = `export E2B_API_URL=${e2bApiUrl()}\nexport E2B_API_KEY=<your key>`
  const py = `from e2b import Sandbox\n\nsbx = Sandbox.create("base")\nprint(sbx.commands.run("echo hi").stdout)\nsbx.kill()`

  return (
    <div className="grid gap-4">
      <div className="flex items-center justify-between gap-4">
        <p className="text-sm text-muted-foreground">Keys for the E2B-compatible API. They belong to this project and act as you.</p>
        <Button onClick={() => setCreateOpen(true)}><Plus aria-hidden="true" /> New API key</Button>
      </div>
      {keys.data && keys.data.length === 0 ? (
        <EmptyState icon={KeyRound} title="No API keys" hint="Create a key to use the stock E2B SDK with this project." />
      ) : (
        <DataTable columns={columns} data={keys.data ?? []} isLoading={keys.isLoading} error={keys.error as Error | null} />
      )}

      <details className="rounded-md border p-4" open>
        <summary className="cursor-pointer text-sm font-medium">Use with the E2B SDK</summary>
        <div className="mt-3 grid gap-3">
          {!e2b && (
            <p className="text-sm text-muted-foreground" role="status">
              This project's sandbox location does not offer the E2B API yet: calls with a key answer 503 until your
              operator turns it on.
            </p>
          )}
          <Snippet label="environment variables" text={env} />
          <Snippet label="Python example" text={py} />
          <p className="text-xs text-muted-foreground">Templates: base, and code-interpreter-v1 for the e2b-code-interpreter SDK, where offered.</p>
        </div>
      </details>

      <Dialog open={createOpen} onOpenChange={(o) => !o && closeCreate()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{created ? "API key created" : "New API key"}</DialogTitle>
            <DialogDescription>
              {created ? "Copy it now. You won't see this key again." : "Name it after what will use it, so you can revoke the right one later."}
            </DialogDescription>
          </DialogHeader>
          {created ? (
            <div className="grid gap-2">
              <Label htmlFor={keyId}>API key</Label>
              <div className="flex items-center gap-2">
                <Input id={keyId} readOnly value={created} className="font-mono text-xs" onFocus={(e) => e.currentTarget.select()} />
                <Button variant="outline" size="icon-sm" onClick={() => void copy("API key", created)} aria-label="Copy API key">
                  <Copy className="size-4" aria-hidden="true" />
                </Button>
              </div>
              <p className="text-xs text-muted-foreground" role="status">Store it somewhere safe. Only a short hint stays visible afterwards.</p>
            </div>
          ) : (
            <div className="grid gap-2">
              <Label htmlFor={nameId}>Name (optional)</Label>
              <Input id={nameId} value={name} maxLength={100} onChange={(e) => setName(e.target.value)} placeholder="ci-runner" />
            </div>
          )}
          <DialogFooter>
            {created ? (
              <Button onClick={closeCreate}>Done</Button>
            ) : (
              <>
                <Button variant="outline" onClick={closeCreate}>Cancel</Button>
                <Button disabled={create.isPending} onClick={() => create.mutate()}>{create.isPending ? "Creating…" : "Create key"}</Button>
              </>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toRevoke} onOpenChange={(o) => !o && setToRevoke(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke API key?</DialogTitle>
            <DialogDescription>
              {toRevoke ? `"${toRevoke.name || toRevoke.hint}" stops working immediately. Anything using it gets 401 errors.` : ""}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToRevoke(null)}>Cancel</Button>
            <Button variant="destructive" disabled={revoke.isPending} onClick={() => toRevoke && revoke.mutate(toRevoke)}>
              {revoke.isPending ? "Revoking…" : "Revoke"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
