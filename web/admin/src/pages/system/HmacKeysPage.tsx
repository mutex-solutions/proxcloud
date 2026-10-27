import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { Check, Copy, KeyRound, MoreHorizontal, Plus, Trash2 } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { apiFetch } from "@/lib/api"
import { fmtDateTime } from "@/lib/format"
import { useAdminList } from "@/lib/hooks"

// The list strips secretKey server-side; the id IS the public key ("pk…").
type HmacKey = {
  id: string
  description?: string
  createdAt?: string
  updatedAt?: string
}

// The generate response carries the plaintext secret ONCE.
type GeneratedKey = { id: string; secretKey: string; description?: string; createdAt?: string }

const PATH = "/admin/hmac-keys"

function CopyField({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div>
      <Label className="text-xs text-muted-foreground">{label}</Label>
      <div className="mt-1 flex items-center gap-2">
        <code className="min-w-0 flex-1 truncate rounded-md border bg-muted/50 px-2 py-1.5 font-mono text-xs">{value}</code>
        <Button
          variant="outline"
          size="icon"
          className="size-8 shrink-0"
          onClick={() => {
            void navigator.clipboard.writeText(value)
            setCopied(true)
            setTimeout(() => setCopied(false), 1500)
          }}
          aria-label={`Copy ${label}`}
        >
          {copied ? <Check className="size-4 text-primary" /> : <Copy className="size-4" />}
        </Button>
      </div>
    </div>
  )
}

export default function HmacKeysPage() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useAdminList<HmacKey>(PATH)
  const items = data?.data ?? []

  const [deleting, setDeleting] = useState<HmacKey | null>(null)
  const [genOpen, setGenOpen] = useState(false)
  const [description, setDescription] = useState("")
  const [generated, setGenerated] = useState<GeneratedKey | null>(null)

  const remove = useMutation({
    mutationFn: (id: string) => apiFetch(`${PATH}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      setDeleting(null)
      qc.invalidateQueries({ queryKey: ["admin-list", PATH] })
      toast.success("HMAC key deleted")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const generate = useMutation({
    mutationFn: () => apiFetch<GeneratedKey>(PATH, { method: "POST", body: { description } }),
    onSuccess: (key) => {
      setGenOpen(false)
      setDescription("")
      setGenerated(key) // reveal the secret once
      qc.invalidateQueries({ queryKey: ["admin-list", PATH] })
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const columns = useMemo<ColumnDef<HmacKey, any>[]>(
    () => [
      {
        id: "id",
        accessorFn: (k) => k.id,
        header: sortableHeader("Key ID (public)"),
        cell: ({ getValue }) => <span className="font-mono text-xs">{getValue()}</span>,
      },
      {
        id: "description",
        accessorFn: (k) => k.description ?? "",
        header: "Description",
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "created",
        accessorFn: (k) => k.createdAt ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDateTime(getValue())}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => (
          <div className="text-right">
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon-sm" aria-label={`Actions for key ${row.original.id}`}>
                  <MoreHorizontal className="size-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem variant="destructive" onClick={() => setDeleting(row.original)}>
                  <Trash2 className="size-4" /> Delete
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        ),
      },
    ],
    // useState setters are stable; formatters are module scope.
    [],
  )

  return (
    <>
      <PageHeader
        title="API keys"
        eyebrow="System"
        description="SigV4 access-key pairs for the machine-to-machine admin API (/admin-api/v1). The secret is shown once at creation and never again."
        actions={
          <Button size="sm" onClick={() => setGenOpen(true)}>
            <Plus className="size-4" /> Generate key
          </Button>
        }
      />
      {!isLoading && !error && items.length === 0 ? (
        <EmptyState
          icon={KeyRound}
          title="No API keys yet"
          hint="Generate a key pair to authenticate scripts against the public admin API with AWS SigV4."
          action={
            <Button onClick={() => setGenOpen(true)}>
              <Plus className="size-4" /> Generate key
            </Button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={items}
          isLoading={isLoading}
          error={error as Error | null}
          searchPlaceholder="Search keys…"
          getRowId={(k) => k.id}
        />
      )}

      {/* Generate dialog */}
      <Dialog open={genOpen} onOpenChange={(o) => !o && setGenOpen(false)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Generate API key</DialogTitle>
            <DialogDescription>
              Mints an access-key / secret-key pair for signing admin-API requests. The secret is shown once —
              save it immediately.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="desc">Description (optional)</Label>
            <Input
              id="desc"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="e.g. billing export script"
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setGenOpen(false)}>Cancel</Button>
            <Button onClick={() => generate.mutate()} disabled={generate.isPending}>
              {generate.isPending ? "Generating…" : "Generate key"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Reveal-once dialog */}
      <Dialog open={!!generated} onOpenChange={(o) => !o && setGenerated(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Save your API key</DialogTitle>
            <DialogDescription>
              This is the only time the secret key is shown. Copy both values now — they can't be retrieved later.
            </DialogDescription>
          </DialogHeader>
          {generated ? (
            <div className="space-y-3">
              <CopyField label="Access key ID" value={generated.id} />
              <CopyField label="Secret key" value={generated.secretKey} />
            </div>
          ) : null}
          <DialogFooter>
            <Button onClick={() => setGenerated(null)}>I've saved it</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete dialog */}
      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete API key</DialogTitle>
            <DialogDescription>
              Delete key <span className="font-mono">{deleting?.id}</span>? Admin-API clients signing with it will
              be locked out immediately.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.id)} disabled={remove.isPending}>
              {remove.isPending ? "Deleting…" : "Delete key"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
