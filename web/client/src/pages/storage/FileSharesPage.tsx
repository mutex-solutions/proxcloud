import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Copy, FolderTree, KeyRound, Maximize2, Plus, RefreshCw, ShieldCheck, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudList, useLocations, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

const share = (r: CloudResource) => (r.data?.share as Record<string, any>) ?? {}
const gb = (bytes: number) => `${(bytes / 2 ** 30).toFixed(bytes < 2 ** 30 ? 2 : 0)} GB`

function CopyLine({ text }: { text: string }) {
  return (
    <div className="flex items-center gap-2 rounded-md bg-muted p-2 font-mono text-xs break-all">
      <span className="flex-1">{text}</span>
      <Button
        variant="ghost"
        size="sm"
        aria-label="Copy"
        onClick={() => void navigator.clipboard.writeText(text).then(() => toast.success("Copied"))}
      >
        <Copy className="size-3" />
      </Button>
    </div>
  )
}

export default function FileSharesPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const { data, isLoading, refetch, isFetching } = useCloudList(pid, "SHARE")
  const locations = useLocations(pid).data ?? []
  const location = locations.find((l) => l.resourceTypes?.includes("SHARE"))
  const scope = { serviceId: location?.serviceId ?? "", region: location?.region ?? "" }
  const rows = data ?? []

  const [createOpen, setCreateOpen] = useState(false)
  const [form, setForm] = useState({ name: "", protocol: "NFS", sizeGb: "10" })
  const [details, setDetails] = useState<CloudResource | null>(null)
  const [password, setPassword] = useState<string | null>(null)
  const [resizing, setResizing] = useState<CloudResource | null>(null)
  const [newSize, setNewSize] = useState("")
  const [deleting, setDeleting] = useState<CloudResource | null>(null)

  const refresh = () => qc.invalidateQueries({ queryKey: ["cloud", pid, "SHARE"] }).then(() => refetch())
  const action = (r: CloudResource, name: string, body?: Record<string, unknown>) =>
    apiFetch<{ result?: { password?: string } }>(`/project/${pid}/cloud/${r.id}/action`, {
      method: "POST",
      body: { action: name, ...(body ? { data: body } : {}) },
    })

  const create = useMutation({
    mutationFn: () =>
      apiFetch<CloudResource>(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: {
          type: "SHARE",
          serviceId: scope.serviceId,
          data: { name: form.name.trim(), protocol: form.protocol, sizeGb: Number(form.sizeGb) },
        },
      }),
    onSuccess: (r) => {
      toast.success("File share created")
      setCreateOpen(false)
      setForm({ name: "", protocol: "NFS", sizeGb: "10" })
      setDetails(r)
      setPassword((share(r).password as string) || null)
      void refresh()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const resize = useMutation({
    mutationFn: () => action(resizing!, "EXTEND", { sizeGb: Number(newSize) }),
    onSuccess: () => {
      toast.success("Size updated")
      setResizing(null)
      void refresh()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const access = useMutation({
    mutationFn: (r: CloudResource) => action(r, "REFRESH_ACCESS"),
    onSuccess: () => {
      toast.success("Access updated with this project's current addresses")
      void refresh()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const reset = useMutation({
    mutationFn: (r: CloudResource) => action(r, "RESET_PASSWORD"),
    onSuccess: (res) => {
      setPassword(res.result?.password ?? null)
      toast.success("New password issued")
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const remove = useMutation({
    mutationFn: () => apiFetch(`/project/${pid}/cloud/${deleting!.id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("File share deleted")
      setDeleting(null)
      void refresh()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudResource>[]>(
    () => [
      { id: "name", header: sortableHeader("Name"), accessorFn: (r) => share(r).name ?? "" },
      { id: "protocol", header: "Protocol", accessorFn: (r) => share(r).protocol ?? "" },
      { id: "size", header: "Size", accessorFn: (r) => gb(share(r).sizeBytes ?? 0) },
      { id: "used", header: "Used", accessorFn: (r) => gb(share(r).usedBytes ?? 0) },
      { id: "created", header: "Created", accessorFn: (r) => timeAgo(r.createdAt) },
      {
        id: "actions",
        header: "",
        cell: ({ row }) => (
          <div className="flex justify-end gap-1">
            <Button variant="ghost" size="sm" onClick={() => { setPassword(null); setDetails(row.original) }}>
              Mount
            </Button>
            <Button variant="ghost" size="sm" aria-label="Resize" onClick={() => { setResizing(row.original); setNewSize(String(Math.round((share(row.original).sizeBytes ?? 0) / 2 ** 30))) }}>
              <Maximize2 className="size-4" />
            </Button>
            <Button variant="ghost" size="sm" aria-label="Refresh access" disabled={access.isPending} onClick={() => access.mutate(row.original)}>
              <ShieldCheck className="size-4" />
            </Button>
            <Button variant="ghost" size="sm" aria-label="Delete" onClick={() => setDeleting(row.original)}>
              <Trash2 className="size-4" />
            </Button>
          </div>
        ),
      },
    ],
    [access],
  )

  return (
    <>
      <PageHeader
        eyebrow="Storage"
        title="File shares"
        description="NFS and SMB shares on the HDD NAS, for shared files and backups. Not for databases."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
              <RefreshCw className="size-4" /> Refresh
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)} disabled={!location}>
              <Plus className="size-4" /> Create share
            </Button>
          </>
        }
      />
      {rows.length === 0 && !isLoading ? (
        <EmptyState icon={FolderTree} title="No file shares" hint="Create a share and mount it from your servers." />
      ) : (
        <DataTable columns={columns} data={rows} isLoading={isLoading} />
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create file share</DialogTitle>
            <DialogDescription>
              The share is open to this project's public IPs and networks, and follows them at the next sync. To pick up a
              new public IP right away, use the shield button on the share. Root on a client has no special rights on an
              NFS share.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3">
            <div className="grid gap-1">
              <Label htmlFor="sh-name">Name</Label>
              <Input id="sh-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="team-data" />
            </div>
            <div className="grid gap-1">
              <Label>Protocol</Label>
              <Select value={form.protocol} onValueChange={(v) => setForm({ ...form, protocol: v })}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="NFS">NFS (Linux)</SelectItem>
                  <SelectItem value="SMB">SMB (Windows)</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-1">
              <Label htmlFor="sh-size">Size (GB)</Label>
              <Input id="sh-size" type="number" min={1} value={form.sizeGb} onChange={(e) => setForm({ ...form, sizeGb: e.target.value })} />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>Cancel</Button>
            <Button disabled={create.isPending || !form.name.trim() || Number(form.sizeGb) < 1} onClick={() => create.mutate()}>
              {create.isPending ? "Creating…" : "Create"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!details} onOpenChange={(o) => !o && setDetails(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Mount {details ? share(details).name : ""}</DialogTitle>
            <DialogDescription>Run this on a server of this project.</DialogDescription>
          </DialogHeader>
          {details ? (
            <div className="grid gap-3">
              <CopyLine text={String(share(details).mount ?? "")} />
              {password ? (
                <div className="grid gap-1">
                  <Label>Password for {String(share(details).username ?? "")} (shown once)</Label>
                  <CopyLine text={password} />
                </div>
              ) : null}
              <p className="text-xs text-muted-foreground">
                Allowed: {((share(details).networks as string[]) ?? []).join(", ") || "none"}
              </p>
              {((share(details).sharedNat as string[]) ?? []).length > 0 ? (
                <p className="text-xs text-muted-foreground">
                  This site reaches the NAS through a NAT, so {(share(details).sharedNat as string[]).join(", ")} is also
                  allowed. Other projects behind that NAT share the address, and the address check does not separate this
                  share from them.
                </p>
              ) : null}
              {share(details).protocol === "SMB" ? (
                <Button variant="outline" size="sm" disabled={reset.isPending} onClick={() => reset.mutate(details)}>
                  <KeyRound className="size-4" /> Reset password
                </Button>
              ) : null}
            </div>
          ) : null}
        </DialogContent>
      </Dialog>

      <Dialog open={!!resizing} onOpenChange={(o) => !o && setResizing(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Resize {resizing ? share(resizing).name : ""}</DialogTitle>
            <DialogDescription>Billing may follow the allocated size. It cannot go below what is used.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-1">
            <Label htmlFor="sh-new">Size (GB)</Label>
            <Input id="sh-new" type="number" min={1} value={newSize} onChange={(e) => setNewSize(e.target.value)} />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setResizing(null)}>Cancel</Button>
            <Button disabled={resize.isPending || Number(newSize) < 1} onClick={() => resize.mutate()}>Apply</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {deleting ? share(deleting).name : ""}?</DialogTitle>
            <DialogDescription>Every file on this share is deleted. This cannot be undone.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>Delete share</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
