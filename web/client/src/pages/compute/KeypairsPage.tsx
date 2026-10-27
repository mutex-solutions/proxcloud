// Keypairs: list KEYPAIR cloud resources, create (data{name, publicKey?} per
// internal/cloud/providers/write.go TypeKeypair; a nova-generated private key comes back
// once as ephemeralData.privateKey) and delete with confirm.
import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Copy, Download, KeyRound, MoreHorizontal, Plus, RefreshCw, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { LoadMore } from "@/components/load-more"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudCursorList, useCloudScope, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

function keypairName(r: CloudResource): string {
  return (r.data?.keypair?.name as string) ?? r.name ?? r.id
}
function keypairFingerprint(r: CloudResource): string {
  return (r.data?.keypair?.fingerprint as string) ?? "—"
}

export default function KeypairsPage() {
  const pid = useProjectId()
  const scope = useCloudScope(pid)
  const qc = useQueryClient()
  const {
    rows: data, isLoading, refetch, isFetching, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCloudCursorList(pid, "KEYPAIR")

  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState("")
  const [publicKey, setPublicKey] = useState("")
  const [toDelete, setToDelete] = useState<CloudResource | null>(null)
  // The generated private key is returned exactly once (ephemeralData) — shown here.
  const [privateKey, setPrivateKey] = useState<{ name: string; key: string } | null>(null)

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["cloud", pid, "KEYPAIR"] })

  const create = useMutation({
    mutationFn: () =>
      apiFetch<CloudResource>(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: {
          type: "KEYPAIR",
          data: { name: name.trim(), ...(publicKey.trim() ? { publicKey: publicKey.trim() } : {}) },
        },
      }),
    onSuccess: (res) => {
      const kpName = name.trim()
      toast.success(`Key pair "${kpName}" created`)
      setCreateOpen(false)
      setName("")
      setPublicKey("")
      invalidate()
      const pk = (res?.ephemeralData as { privateKey?: string } | undefined)?.privateKey
      if (pk) setPrivateKey({ name: kpName, key: pk })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const del = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}`, { method: "DELETE", cloud: scope }),
    onSuccess: (_d, r) => {
      toast.success(`Key pair "${keypairName(r)}" deleted`)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const copyKey = async () => {
    if (!privateKey) return
    await navigator.clipboard.writeText(privateKey.key)
    toast.success("Private key copied to clipboard")
  }
  const downloadKey = () => {
    if (!privateKey) return
    const url = URL.createObjectURL(new Blob([privateKey.key], { type: "application/x-pem-file" }))
    const a = document.createElement("a")
    a.href = url
    a.download = `${privateKey.name}.pem`
    a.click()
    URL.revokeObjectURL(url)
  }

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => keypairName(r),
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "fingerprint",
        accessorFn: (r) => keypairFingerprint(r),
        header: "Fingerprint",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue()}</span>
        ),
      },
      {
        id: "created",
        accessorFn: (r) => r.info?.createdAt ?? r.createdAt ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => (
          <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span>
        ),
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const r = row.original
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${keypairName(r)}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem variant="destructive" onClick={() => setToDelete(r)}>
                    <Trash2 className="size-4" /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // useState setters are stable; helpers are module-scope.
    [],
  )

  return (
    <>
      <PageHeader
        title="Key pairs"
        eyebrow="Compute"
        description="SSH key pairs injected into servers at boot."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh">
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create key pair
            </Button>
          </>
        }
      />

      {!isLoading && !error && !data.length ? (
        <EmptyState
          icon={KeyRound}
          title="No key pairs yet"
          hint="Create a key pair to SSH into your servers — import your public key or let the cloud generate one."
          action={
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create key pair
            </Button>
          }
        />
      ) : (
        <>
          <DataTable
            columns={columns}
            data={data}
            isLoading={isLoading}
            error={error as Error | null}
            pagination={false}
          />
          <LoadMore
            hasNextPage={hasNextPage}
            isFetching={isFetchingNextPage}
            onClick={() => void fetchNextPage()}
            count={data.length}
            noun="key pair"
          />
        </>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create key pair</DialogTitle>
            <DialogDescription>
              Leave the public key empty to have a new key pair generated — the private key is shown only once.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label htmlFor="kp-name">Name</Label>
              <Input id="kp-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="my-key" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="kp-pub">Public key (optional)</Label>
              <Textarea
                id="kp-pub"
                value={publicKey}
                onChange={(e) => setPublicKey(e.target.value)}
                placeholder="ssh-ed25519 AAAA…"
                rows={4}
                className="max-h-40 font-mono text-xs"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => create.mutate()} disabled={!name.trim() || create.isPending}>
              {create.isPending ? "Creating…" : "Create key pair"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!privateKey} onOpenChange={(o) => !o && setPrivateKey(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Save your private key</DialogTitle>
            <DialogDescription>
              This is the only time the private key is shown — copy or download it now.
            </DialogDescription>
          </DialogHeader>
          <pre className="max-h-64 overflow-auto rounded-lg border bg-muted/50 p-3 font-mono text-xs whitespace-pre-wrap break-all">
            {privateKey?.key}
          </pre>
          <DialogFooter>
            <Button variant="outline" onClick={() => void copyKey()}>
              <Copy className="size-4" /> Copy
            </Button>
            <Button onClick={downloadKey}>
              <Download className="size-4" /> Download .pem
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete key pair</DialogTitle>
            <DialogDescription>
              Delete key pair “{toDelete ? keypairName(toDelete) : ""}”? This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => {
                if (toDelete) del.mutate(toDelete)
                setToDelete(null)
              }}
            >
              <Trash2 className="size-4" /> Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
