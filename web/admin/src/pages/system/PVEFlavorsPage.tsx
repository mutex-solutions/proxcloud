// PVEFlavorsPage — the Proxmox flavor catalog (Proxmox has no flavor concept;
// the platform curates one). Backed by GET/POST/PUT/DELETE /admin/flavors; the
// client wizard consumes it through the LIST_FLAVORS bulk action.

import { useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"

type PVEFlavor = {
  id?: string
  slug: string
  name?: string
  vcpus: number
  ramMb: number
  diskGb: number
  storageProfile?: string
  gpuModel?: string
  gpuCount?: number
  active: boolean
  orderNumber?: number
}

const LIST_PATH = "/admin/flavors"

const empty: PVEFlavor = { slug: "", name: "", vcpus: 1, ramMb: 1024, diskGb: 10, storageProfile: "", gpuModel: "", gpuCount: 0, active: true }

export default function PVEFlavorsPage() {
  const qc = useQueryClient()
  const { data, isLoading } = useAdminList<PVEFlavor>(LIST_PATH)
  const items = (data?.data ?? []).slice().sort((a, b) => (a.orderNumber ?? 0) - (b.orderNumber ?? 0) || a.slug.localeCompare(b.slug))

  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState(false)
  const [form, setForm] = useState<PVEFlavor>(empty)
  const [toDelete, setToDelete] = useState<PVEFlavor | null>(null)

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["admin-list", LIST_PATH] })

  const save = useMutation({
    mutationFn: () =>
      apiFetch(editing ? `${LIST_PATH}/${encodeURIComponent(form.slug)}` : LIST_PATH, {
        method: editing ? "PUT" : "POST",
        body: {
          ...form,
          gpuCount: form.gpuModel ? form.gpuCount || 1 : 0,
          gpuModel: form.gpuModel || undefined,
          storageProfile: form.storageProfile || undefined,
        },
      }),
    onSuccess: () => {
      toast.success(editing ? "Flavor updated" : "Flavor created")
      setOpen(false)
      invalidate()
    },
    onError: (e) => toast.error(e instanceof Error ? e.message : String(e)),
  })

  const del = useMutation({
    mutationFn: (slug: string) => apiFetch(`${LIST_PATH}/${encodeURIComponent(slug)}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Flavor deleted")
      setToDelete(null)
      invalidate()
    },
    onError: (e) => toast.error(e instanceof Error ? e.message : String(e)),
  })

  const num = (v: string) => (v === "" ? 0 : Number.parseInt(v, 10) || 0)

  return (
    <>
      <PageHeader
        title="Flavors (Proxmox)"
        description="The curated server sizes offered on Proxmox providers — Proxmox itself has no flavor concept."
        actions={
          <Button onClick={() => { setForm(empty); setEditing(false); setOpen(true) }}>Add flavor</Button>
        }
      />
      <Card>
        <CardContent className="pt-6">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Slug</TableHead>
                <TableHead>Name</TableHead>
                <TableHead>vCPU</TableHead>
                <TableHead>RAM</TableHead>
                <TableHead>Disk</TableHead>
                <TableHead>Storage profile</TableHead>
                <TableHead>GPU</TableHead>
                <TableHead>Status</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {isLoading ? (
                <TableRow><TableCell colSpan={9} className="text-center text-muted-foreground">Loading…</TableCell></TableRow>
              ) : items.length === 0 ? (
                <TableRow><TableCell colSpan={9} className="text-center text-muted-foreground">No flavors yet — the client wizard's Hardware step stays empty until one is active.</TableCell></TableRow>
              ) : (
                items.map((f) => (
                  <TableRow key={f.slug}>
                    <TableCell className="font-mono">{f.slug}</TableCell>
                    <TableCell>{f.name || f.slug}</TableCell>
                    <TableCell>{f.vcpus}</TableCell>
                    <TableCell>{Math.round(f.ramMb / 1024 * 10) / 10} GB</TableCell>
                    <TableCell>{f.diskGb} GB</TableCell>
                    <TableCell className="font-mono">{f.storageProfile || "—"}</TableCell>
                    <TableCell className="font-mono">{f.gpuModel ? `${f.gpuModel} ×${f.gpuCount ?? 1}` : "—"}</TableCell>
                    <TableCell>
                      <Badge variant={f.active ? "default" : "secondary"}>{f.active ? "Active" : "Hidden"}</Badge>
                    </TableCell>
                    <TableCell className="text-right">
                      <Button variant="ghost" size="sm" onClick={() => { setForm({ ...empty, ...f }); setEditing(true); setOpen(true) }}>Edit</Button>
                      <Button variant="ghost" size="sm" className="text-destructive" onClick={() => setToDelete(f)}>Delete</Button>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editing ? `Edit flavor ${form.slug}` : "Add flavor"}</DialogTitle>
            <DialogDescription>
              The slug rides VM tags and billing attributes — lowercase letters, digits, dashes.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div className="grid grid-cols-2 gap-4">
              <div className="grid gap-2">
                <Label htmlFor="fl-slug">Slug</Label>
                <Input id="fl-slug" className="font-mono" value={form.slug} disabled={editing} onChange={(e) => setForm({ ...form, slug: e.target.value })} placeholder="std-2" />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="fl-name">Display name</Label>
                <Input id="fl-name" value={form.name ?? ""} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Standard 2" />
              </div>
            </div>
            <div className="grid grid-cols-3 gap-4">
              <div className="grid gap-2">
                <Label htmlFor="fl-vcpus">vCPUs</Label>
                <Input id="fl-vcpus" type="number" min={1} value={form.vcpus} onChange={(e) => setForm({ ...form, vcpus: num(e.target.value) })} />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="fl-ram">RAM (MB)</Label>
                <Input id="fl-ram" type="number" min={128} value={form.ramMb} onChange={(e) => setForm({ ...form, ramMb: num(e.target.value) })} />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="fl-disk">Root disk (GB)</Label>
                <Input id="fl-disk" type="number" min={0} value={form.diskGb} onChange={(e) => setForm({ ...form, diskGb: num(e.target.value) })} />
              </div>
            </div>
            <div className="grid grid-cols-3 gap-4">
              <div className="grid gap-2">
                <Label htmlFor="fl-profile">Storage profile</Label>
                <Input id="fl-profile" className="font-mono" value={form.storageProfile ?? ""} onChange={(e) => setForm({ ...form, storageProfile: e.target.value })} placeholder="std" />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="fl-gpu">GPU model (alias)</Label>
                <Input id="fl-gpu" className="font-mono" value={form.gpuModel ?? ""} onChange={(e) => setForm({ ...form, gpuModel: e.target.value })} placeholder="nvidia-4090" />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="fl-gpun">GPU count</Label>
                <Input id="fl-gpun" type="number" min={0} value={form.gpuCount ?? 0} onChange={(e) => setForm({ ...form, gpuCount: num(e.target.value) })} disabled={!form.gpuModel} />
              </div>
            </div>
            <div className="flex items-center justify-between rounded-lg border p-3">
              <div>
                <Label className="text-sm font-medium">Active</Label>
                <div className="text-xs text-muted-foreground">Only active flavors show in the client wizard.</div>
              </div>
              <Switch checked={form.active} onCheckedChange={(on) => setForm({ ...form, active: on })} />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
            <Button onClick={() => save.mutate()} disabled={!form.slug.trim() || form.vcpus < 1 || form.ramMb < 128 || save.isPending}>
              {save.isPending ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete flavor</DialogTitle>
            <DialogDescription>
              Delete flavor "{toDelete?.slug}"? Existing servers keep billing by their stamped flavor; the wizard stops offering it.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => toDelete && del.mutate(toDelete.slug)} disabled={del.isPending}>
              {del.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
