import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { ArrowDown, ArrowUp, Pencil, Plus, RefreshCw, SquareMenu, Trash2 } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { useAdminGet, useAdminList } from "@/lib/hooks"

type MenuItem = {
  id: string
  displayName?: string
  url?: string
  icon?: string
  renderMode?: string
  order?: number
}

const LIST_PATH = "/admin/menu"

// RenderMode enum on the API (RenderMode): IFRAME | OPEN_NEW_WINDOW.
const RENDER_MODES = [
  { value: "IFRAME", label: "Iframe (embedded)" },
  { value: "OPEN_NEW_WINDOW", label: "Open in new window" },
]

type FormState = {
  displayName: string
  url: string
  icon: string
  renderMode: string
  order: string
}

const emptyForm: FormState = { displayName: "", url: "", icon: "", renderMode: "IFRAME", order: "0" }

function toBody(f: FormState) {
  return {
    displayName: f.displayName.trim(),
    url: f.url.trim(),
    icon: f.icon.trim(),
    renderMode: f.renderMode,
    order: parseInt(f.order, 10) || 0,
  }
}

function MenuItemForm({ form, setForm }: { form: FormState; setForm: (f: FormState) => void }) {
  return (
    <div className="grid gap-4">
      <div className="grid gap-2">
        <Label htmlFor="mi-name">Display name</Label>
        <Input
          id="mi-name"
          value={form.displayName}
          onChange={(e) => setForm({ ...form, displayName: e.target.value })}
          placeholder="Documentation"
        />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="mi-url">URL</Label>
        <Input
          id="mi-url"
          value={form.url}
          onChange={(e) => setForm({ ...form, url: e.target.value })}
          placeholder="https://docs.example.com?project=project.id"
          className="font-mono"
        />
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="mi-icon">Icon</Label>
          <Input
            id="mi-icon"
            value={form.icon}
            onChange={(e) => setForm({ ...form, icon: e.target.value })}
            placeholder="icon name or css class"
          />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="mi-order">Order</Label>
          <Input
            id="mi-order"
            type="number"
            min="0"
            step="1"
            value={form.order}
            onChange={(e) => setForm({ ...form, order: e.target.value })}
          />
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="mi-render">Render mode</Label>
        <Select value={form.renderMode} onValueChange={(v) => setForm({ ...form, renderMode: v })}>
          <SelectTrigger id="mi-render">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {RENDER_MODES.map((m) => (
              <SelectItem key={m.value} value={m.value}>
                {m.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </div>
  )
}

export default function MenuPage() {
  const qc = useQueryClient()
  const { data, isLoading, error, refetch, isFetching } = useAdminList<MenuItem>(LIST_PATH)
  const placeholdersQ = useAdminGet<Record<string, string[]>>(`${LIST_PATH}/placeholders`)

  const items = useMemo(
    () => [...(data?.data ?? [])].sort((a, b) => (a.order ?? 0) - (b.order ?? 0)),
    [data],
  )

  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin-list", LIST_PATH] })

  const [createOpen, setCreateOpen] = useState(false)
  const [createForm, setCreateForm] = useState<FormState>(emptyForm)
  const [editing, setEditing] = useState<MenuItem | null>(null)
  const [editForm, setEditForm] = useState<FormState>(emptyForm)
  const [toDelete, setToDelete] = useState<MenuItem | null>(null)

  const createItem = useMutation({
    mutationFn: () => apiFetch(LIST_PATH, { method: "POST", body: toBody(createForm) }),
    onSuccess: () => {
      toast.success("Menu item created")
      setCreateOpen(false)
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const updateItem = useMutation({
    mutationFn: (id: string) => apiFetch(`${LIST_PATH}/${id}`, { method: "PUT", body: toBody(editForm) }),
    onSuccess: () => {
      toast.success("Menu item updated")
      setEditing(null)
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const deleteItem = useMutation({
    mutationFn: (id: string) => apiFetch(`${LIST_PATH}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Menu item deleted")
      setToDelete(null)
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // PUT /admin/menu/reorder takes the full id list in the new order (the API sets order = index).
  const reorder = useMutation({
    mutationFn: (ids: string[]) => apiFetch(`${LIST_PATH}/reorder`, { method: "PUT", body: ids }),
    onSuccess: () => void invalidate(),
    onError: (e: Error) => {
      toast.error(e.message)
      void invalidate()
    },
  })

  const move = (index: number, dir: -1 | 1) => {
    const ids = items.map((i) => i.id)
    const j = index + dir
    if (j < 0 || j >= ids.length) return
    ;[ids[index], ids[j]] = [ids[j], ids[index]]
    reorder.mutate(ids)
  }

  const openCreate = () => {
    setCreateForm({ ...emptyForm, order: String(items.length) })
    setCreateOpen(true)
  }

  const openEdit = (m: MenuItem) => {
    setEditForm({
      displayName: m.displayName ?? "",
      url: m.url ?? "",
      icon: m.icon ?? "",
      renderMode: m.renderMode ?? "IFRAME",
      order: String(m.order ?? 0),
    })
    setEditing(m)
  }

  const formValid = (f: FormState) => f.displayName.trim() !== "" && f.url.trim() !== ""

  const placeholders = placeholdersQ.data ?? {}

  return (
    <>
      <PageHeader
        title="Menu"
        eyebrow="System"
        description={'Custom links added here appear in the client console\'s "More" section.'}
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void refetch()}
              disabled={isFetching}
              aria-label="Refresh"
            >
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={openCreate}>
              <Plus className="size-4" /> Create menu item
            </Button>
          </>
        }
      />

      <div className="space-y-6">
        {isLoading ? (
          <Skeleton className="h-64" />
        ) : error ? (
          <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">{(error as Error).message}</div>
        ) : !items.length ? (
          <EmptyState
            icon={SquareMenu}
            title="No custom menu items yet"
            hint="Add a link and it shows up in the client console's More section."
            action={
              <Button onClick={openCreate}>
                <Plus className="size-4" /> Create menu item
              </Button>
            }
          />
        ) : (
          <Card className="overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-28">Order</TableHead>
                  <TableHead>Display name</TableHead>
                  <TableHead>URL</TableHead>
                  <TableHead>Icon</TableHead>
                  <TableHead>Render mode</TableHead>
                  <TableHead className="w-24" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((m, i) => (
                  <TableRow key={m.id}>
                    <TableCell>
                      <div className="flex items-center gap-1">
                        <span className="w-6 font-mono text-sm tabular-nums">{m.order ?? 0}</span>
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={`Move ${m.displayName ?? "item"} up`}
                          disabled={i === 0 || reorder.isPending}
                          onClick={() => move(i, -1)}
                        >
                          <ArrowUp className="size-4 text-muted-foreground" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={`Move ${m.displayName ?? "item"} down`}
                          disabled={i === items.length - 1 || reorder.isPending}
                          onClick={() => move(i, 1)}
                        >
                          <ArrowDown className="size-4 text-muted-foreground" />
                        </Button>
                      </div>
                    </TableCell>
                    <TableCell className="font-medium">{m.displayName ?? "—"}</TableCell>
                    <TableCell className="max-w-md truncate font-mono text-xs">{m.url ?? "—"}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">{m.icon || "—"}</TableCell>
                    <TableCell>
                      <Badge variant="outline">
                        {RENDER_MODES.find((r) => r.value === m.renderMode)?.label ?? m.renderMode ?? "—"}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={`Edit ${m.displayName ?? "item"}`}
                          onClick={() => openEdit(m)}
                        >
                          <Pencil className="size-4 text-muted-foreground" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={`Delete ${m.displayName ?? "item"}`}
                          onClick={() => setToDelete(m)}
                        >
                          <Trash2 className="size-4 text-muted-foreground" />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        )}

        <Card>
          <CardHeader>
            <CardTitle className="text-eyebrow">URL variables you can use</CardTitle>
          </CardHeader>
          <CardContent>
            {placeholdersQ.isLoading ? (
              <Skeleton className="h-16" />
            ) : !Object.keys(placeholders).length ? (
              <p className="text-sm text-muted-foreground">No placeholders available.</p>
            ) : (
              <div className="space-y-3">
                <p className="text-sm text-muted-foreground">
                  These tokens in a menu URL are replaced with the signed-in user's context when the link opens.
                </p>
                <div className="flex flex-wrap gap-2">
                  {Object.entries(placeholders).flatMap(([group, fields]) =>
                    (fields ?? []).map((f) => (
                      <code key={`${group}.${f}`} className="rounded bg-muted px-2 py-1 font-mono text-xs">
                        {group}.{f}
                      </code>
                    )),
                  )}
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create menu item</DialogTitle>
            <DialogDescription>The item appears in the client console's More section.</DialogDescription>
          </DialogHeader>
          <MenuItemForm form={createForm} setForm={setCreateForm} />
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => createItem.mutate()} disabled={!formValid(createForm) || createItem.isPending}>
              Create menu item
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!editing} onOpenChange={(o) => !o && setEditing(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Edit menu item</DialogTitle>
            <DialogDescription>Changes show in the client console immediately.</DialogDescription>
          </DialogHeader>
          <MenuItemForm form={editForm} setForm={setEditForm} />
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditing(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => editing && updateItem.mutate(editing.id)}
              disabled={!formValid(editForm) || updateItem.isPending}
            >
              Save changes
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete menu item</DialogTitle>
            <DialogDescription>
              Delete "{toDelete?.displayName}"? It disappears from the client console's More section.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => toDelete && deleteItem.mutate(toDelete.id)}
              disabled={deleteItem.isPending}
            >
              Delete menu item
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
