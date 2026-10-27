import { Fragment, useCallback, useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { Boxes, ChevronDown, ChevronRight, Cpu, ImageIcon, MoreHorizontal, Plus, Tags, Trash2, TriangleAlert } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch, apiFetchEnvelope } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"

// ── shared types ─────────────────────────────────────────────────────────────

type FlavorRef = { flavorName?: string } & Record<string, unknown>

type FlavorCategory = {
  id: string
  name?: string
  description?: string
  orderNumber?: number
  bareMetal?: boolean
  kubernetesFlavorCategory?: boolean
  flavors?: FlavorRef[]
  flavorAttributes?: unknown[]
}

type LiveFlavor = { id: string; name: string; vcpus: number; ram: number; disk: number }

type ImageCategory = { id: string; name?: string; description?: string; bareMetal?: boolean }

type ImageBinding = { name?: string; version?: string; orderNumber?: number }

type ImageGroup = {
  id: string
  name?: string
  enabled?: boolean
  orderNumber?: number
  categoryId?: string
  description?: string
  groupLogoUrl?: string
  labels?: unknown[]
  images?: ImageBinding[]
}

type OsImagesLocation = {
  serviceId: string
  serviceName: string
  region: string
  regionDisplayName: string
  images: Array<{ id: string; name: string; status: string; visibility?: string }>
}

type MetaValueOption = { value?: string; displayName?: string; enabled?: boolean }

type MetaOption = {
  id: string
  key?: string
  displayName?: string
  description?: string
  type?: string
  options?: MetaValueOption[]
  numericRange?: { min?: number; max?: number; unit?: string }
  serviceIds?: string[]
  regions?: string[]
  userEditable?: boolean
  showInline?: boolean
  enabled?: boolean
}

const FLAVOR_PATH = "/admin/flavor-categories"
const FLAVOR_LIVE_PATH = "/admin/flavor-categories/flavors"
const IMAGE_CAT_PATH = "/admin/images/categories"
const IMAGE_GROUP_PATH = "/admin/images/groups"
const OS_IMAGES_PATH = "/admin/service/os-images"
const META_PATH = "/admin/instance-metadata-options"

function ErrorPanel({ error }: { error: unknown }) {
  return <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">{(error as Error).message}</div>
}

function dedupe(xs: string[]): string[] {
  return [...new Set(xs.filter(Boolean))]
}

// ── Flavor categories ────────────────────────────────────────────────────────

type FlavorForm = { name: string; description: string; orderNumber: string; bareMetal: boolean }

function FlavorCategoriesTab() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useAdminList<FlavorCategory>(FLAVOR_PATH)
  const items = data?.data ?? []
  const liveQ = useAdminList<LiveFlavor>(FLAVOR_LIVE_PATH)
  const liveNames = dedupe((liveQ.data?.data ?? []).map((f) => f.name))

  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<FlavorCategory | null>(null)
  const [form, setForm] = useState<FlavorForm>({ name: "", description: "", orderNumber: "0", bareMetal: false })
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [deleting, setDeleting] = useState<FlavorCategory | null>(null)

  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin-list", FLAVOR_PATH] })

  const openCreate = () => {
    setEditing(null)
    setForm({ name: "", description: "", orderNumber: "0", bareMetal: false })
    setSelected(new Set())
    setOpen(true)
  }
  const openEdit = useCallback((c: FlavorCategory) => {
    setEditing(c)
    setForm({
      name: c.name ?? "",
      description: c.description ?? "",
      orderNumber: String(c.orderNumber ?? 0),
      bareMetal: c.bareMetal === true,
    })
    setSelected(new Set((c.flavors ?? []).map((f) => f.flavorName ?? "").filter(Boolean)))
    setOpen(true)
  }, [])

  // union of live nova names + already-selected names (so an offline region's flavor still shows).
  const flavorChoices = dedupe([...liveNames, ...selected])

  const save = useMutation({
    mutationFn: () => {
      // preserve any richer flavor sub-doc already stored for a selected name; else store {flavorName}.
      const existingByName = new Map((editing?.flavors ?? []).map((f) => [f.flavorName ?? "", f]))
      const flavors: FlavorRef[] = [...selected].map((flavorName) => existingByName.get(flavorName) ?? { flavorName })
      const body = {
        name: form.name,
        description: form.description,
        orderNumber: Number(form.orderNumber) || 0,
        bareMetal: form.bareMetal,
        // PUT overwrites all 7 fields — preserve the flags/attributes the form does not surface.
        kubernetesFlavorCategory: editing?.kubernetesFlavorCategory ?? false,
        flavorAttributes: editing?.flavorAttributes ?? [],
        flavors,
      }
      return editing
        ? apiFetch(`${FLAVOR_PATH}/${editing.id}`, { method: "PUT", body })
        : apiFetch(FLAVOR_PATH, { method: "POST", body })
    },
    onSuccess: () => {
      setOpen(false)
      invalidate()
      toast.success(editing ? "Flavor category updated" : "Flavor category created")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: (id: string) => apiFetch(`${FLAVOR_PATH}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      setDeleting(null)
      invalidate()
      toast.success("Flavor category deleted")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const columns = useMemo<ColumnDef<FlavorCategory, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (c) => c.name ?? "",
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue() || "—"}</span>,
      },
      {
        id: "order",
        accessorFn: (c) => c.orderNumber ?? 0,
        header: sortableHeader("Order"),
        cell: ({ getValue }) => <span className="tabular-nums">{getValue()}</span>,
      },
      {
        id: "flavors",
        accessorFn: (c) => (c.flavors ?? []).length,
        header: sortableHeader("Flavors"),
        cell: ({ getValue }) => <span className="tabular-nums">{getValue()}</span>,
      },
      {
        id: "bareMetal",
        accessorFn: (c) => (c.bareMetal ? "Yes" : "No"),
        header: "Bare metal",
        cell: ({ getValue }) => <span className="text-muted-foreground">{getValue()}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const c = row.original
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label="Flavor category actions">
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => openEdit(c)}>Edit</DropdownMenuItem>
                  <DropdownMenuItem className="text-destructive" onClick={() => setDeleting(c)}>
                    Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // useState setters are stable; openEdit is useCallback'd.
    [openEdit],
  )

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <Button onClick={openCreate}>
          <Plus className="size-4" /> Create flavor category
        </Button>
      </div>
      {!isLoading && !error && items.length === 0 ? (
        <EmptyState
          icon={Cpu}
          title="No flavor categories"
          hint="Flavor categories drive the server-create wizard's Hardware list."
          action={<Button onClick={openCreate}>Create flavor category</Button>}
        />
      ) : (
        <DataTable
          columns={columns}
          data={data?.data}
          isLoading={isLoading}
          error={error ? (error as Error) : null}
          searchPlaceholder="Search flavor categories…"
          getRowId={(c) => c.id}
        />
      )}

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{editing ? "Edit flavor category" : "Create flavor category"}</DialogTitle>
            <DialogDescription>Pick the live Nova flavors this category exposes to clients.</DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="fc-name">Name</Label>
              <Input id="fc-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="fc-order">Order number</Label>
              <Input id="fc-order" type="number" value={form.orderNumber} onChange={(e) => setForm({ ...form, orderNumber: e.target.value })} />
            </div>
            <div className="space-y-1.5 sm:col-span-2">
              <Label htmlFor="fc-desc">Description</Label>
              <Textarea id="fc-desc" value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} />
            </div>
            <label className="flex items-center gap-2 text-sm sm:col-span-2">
              <Switch checked={form.bareMetal} onCheckedChange={(v) => setForm({ ...form, bareMetal: v === true })} />
              Bare metal category
            </label>
            <div className="space-y-1.5 sm:col-span-2">
              <Label>Flavors</Label>
              {liveQ.isLoading ? (
                <Skeleton className="h-24" />
              ) : flavorChoices.length === 0 ? (
                <p className="text-sm text-muted-foreground">No live flavors available from the cloud provider.</p>
              ) : (
                <div className="max-h-52 space-y-1 overflow-y-auto rounded-md border p-2">
                  {flavorChoices.map((name) => (
                    <label key={name} className="flex items-center gap-2 text-sm">
                      <Checkbox
                        checked={selected.has(name)}
                        onCheckedChange={(v) => {
                          const next = new Set(selected)
                          if (v === true) next.add(name)
                          else next.delete(name)
                          setSelected(next)
                        }}
                      />
                      <span className="font-mono">{name}</span>
                    </label>
                  ))}
                </div>
              )}
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
            <Button onClick={() => save.mutate()} disabled={save.isPending || !form.name}>
              {save.isPending ? "Saving…" : editing ? "Save changes" : "Create flavor category"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete flavor category</DialogTitle>
            <DialogDescription>
              Delete “{deleting?.name}”? Servers can no longer be created from its flavors.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.id)} disabled={remove.isPending}>
              {remove.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// ── Image categories (with expandable groups) ────────────────────────────────

type ImageCatForm = { name: string; description: string; bareMetal: boolean }

function CategoryGroups({ categoryId }: { categoryId: string }) {
  const q = useQuery({
    queryKey: ["image-groups", categoryId],
    queryFn: async () => (await apiFetchEnvelope<ImageGroup[]>(`${IMAGE_CAT_PATH}/${categoryId}/groups`)).data ?? [],
  })
  if (q.isLoading) return <Skeleton className="h-16" />
  if (q.error) return <ErrorPanel error={q.error} />
  const groups = q.data ?? []
  if (groups.length === 0) return <p className="px-2 py-1 text-sm text-muted-foreground">No image groups in this category.</p>
  return (
    <div className="flex flex-wrap gap-2 px-2 py-1">
      {groups.map((g) => (
        <Badge key={g.id} variant={g.enabled ? "secondary" : "outline"} className={g.enabled ? undefined : "text-muted-foreground"}>
          {g.name ?? g.id} · {(g.images ?? []).length} images
        </Badge>
      ))}
    </div>
  )
}

function ImageCategoriesTab() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useAdminList<ImageCategory>(IMAGE_CAT_PATH)
  const items = data?.data ?? []

  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<ImageCategory | null>(null)
  const [form, setForm] = useState<ImageCatForm>({ name: "", description: "", bareMetal: false })
  const [deleting, setDeleting] = useState<ImageCategory | null>(null)
  const [expanded, setExpanded] = useState<Set<string>>(new Set())

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["admin-list", IMAGE_CAT_PATH] })
    qc.invalidateQueries({ queryKey: ["image-groups"] })
  }

  const openCreate = () => {
    setEditing(null)
    setForm({ name: "", description: "", bareMetal: false })
    setOpen(true)
  }
  const openEdit = (c: ImageCategory) => {
    setEditing(c)
    setForm({ name: c.name ?? "", description: c.description ?? "", bareMetal: c.bareMetal === true })
    setOpen(true)
  }

  const save = useMutation({
    mutationFn: () => {
      const body = { name: form.name, description: form.description, bareMetal: form.bareMetal }
      return editing
        ? apiFetch(`${IMAGE_CAT_PATH}/${editing.id}`, { method: "PUT", body })
        : apiFetch(IMAGE_CAT_PATH, { method: "POST", body })
    },
    onSuccess: () => {
      setOpen(false)
      invalidate()
      toast.success(editing ? "Image category updated" : "Image category created")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: (id: string) => apiFetch(`${IMAGE_CAT_PATH}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      setDeleting(null)
      invalidate()
      toast.success("Image category deleted")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const toggleExpand = (id: string) => {
    const next = new Set(expanded)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    setExpanded(next)
  }

  if (isLoading) return <Skeleton className="h-64 rounded-xl" />
  if (error) return <ErrorPanel error={error} />

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <Button onClick={openCreate}>
          <Plus className="size-4" /> Create image category
        </Button>
      </div>
      {items.length === 0 ? (
        <EmptyState
          icon={Tags}
          title="No image categories"
          hint="Image categories group the OS images shown in the create wizard."
          action={<Button onClick={openCreate}>Create image category</Button>}
        />
      ) : (
        /* Expand/collapse rows — DataTable can't express these, so the bare
           Table stays, on the same card surface DataTable uses. */
        <div className="overflow-hidden rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-10" />
                <TableHead>Name</TableHead>
                <TableHead>Description</TableHead>
                <TableHead>Bare metal</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((c) => (
                <Fragment key={c.id}>
                  <TableRow>
                    <TableCell>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        aria-label={expanded.has(c.id) ? "Collapse image groups" : "Expand image groups"}
                        aria-expanded={expanded.has(c.id)}
                        onClick={() => toggleExpand(c.id)}
                      >
                        {expanded.has(c.id) ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
                      </Button>
                    </TableCell>
                    <TableCell className="font-medium">{c.name ?? "—"}</TableCell>
                    <TableCell className="text-muted-foreground">{c.description ?? "—"}</TableCell>
                    <TableCell className="text-muted-foreground">{c.bareMetal ? "Yes" : "No"}</TableCell>
                    <TableCell className="text-right">
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon-sm" aria-label="Image category actions">
                            <MoreHorizontal className="size-4" />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => openEdit(c)}>Edit</DropdownMenuItem>
                          <DropdownMenuItem className="text-destructive" onClick={() => setDeleting(c)}>
                            Delete
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                  {expanded.has(c.id) ? (
                    <TableRow className="hover:bg-transparent">
                      <TableCell />
                      <TableCell colSpan={4} className="bg-muted/30">
                        <CategoryGroups categoryId={c.id} />
                      </TableCell>
                    </TableRow>
                  ) : null}
                </Fragment>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editing ? "Edit image category" : "Create image category"}</DialogTitle>
            <DialogDescription>Deleting a category cascades to all its image groups.</DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-1 gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="ic-name">Name</Label>
              <Input id="ic-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ic-desc">Description</Label>
              <Textarea id="ic-desc" value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} />
            </div>
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={form.bareMetal} onCheckedChange={(v) => setForm({ ...form, bareMetal: v === true })} />
              Bare metal category
            </label>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
            <Button onClick={() => save.mutate()} disabled={save.isPending || !form.name}>
              {save.isPending ? "Saving…" : editing ? "Save changes" : "Create image category"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete image category</DialogTitle>
            <DialogDescription>
              Delete “{deleting?.name}”? All image groups in this category are deleted too.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.id)} disabled={remove.isPending}>
              {remove.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// ── Image groups (grouped per category) ──────────────────────────────────────

type GroupForm = { name: string; categoryId: string; enabled: boolean; orderNumber: string }

function ImageGroupsTab() {
  const qc = useQueryClient()
  const catsQ = useAdminList<ImageCategory>(IMAGE_CAT_PATH)
  const cats = catsQ.data?.data ?? []
  const catIds = cats.map((c) => c.id)
  const osImagesQ = useAdminList<OsImagesLocation>(OS_IMAGES_PATH)
  const glanceNames = useMemo(
    () => dedupe((osImagesQ.data?.data ?? []).flatMap((l) => (l.images ?? []).map((i) => i.name))),
    [osImagesQ.data],
  )
  // The client's create-server list is tenant-scoped: only PUBLIC Glance images reach it.
  // Bindings to any other visibility silently vanish client-side — warn here instead.
  const publicNames = useMemo(
    () =>
      new Set(
        (osImagesQ.data?.data ?? []).flatMap((l) =>
          (l.images ?? []).filter((i) => (i.visibility ?? "public") === "public").map((i) => i.name),
        ),
      ),
    [osImagesQ.data],
  )
  const hiddenFromClients = (name?: string) => !!name && glanceNames.includes(name) && !publicNames.has(name)

  const groupsQ = useQuery({
    queryKey: ["image-groups", "by-category", catIds.join(",")],
    enabled: cats.length > 0,
    queryFn: async () =>
      Promise.all(
        cats.map(async (cat) => ({
          cat,
          groups: (await apiFetchEnvelope<ImageGroup[]>(`${IMAGE_CAT_PATH}/${cat.id}/groups`)).data ?? [],
        })),
      ),
  })

  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<ImageGroup | null>(null)
  const [form, setForm] = useState<GroupForm>({ name: "", categoryId: "", enabled: true, orderNumber: "0" })
  const [rows, setRows] = useState<ImageBinding[]>([])
  const [deleting, setDeleting] = useState<ImageGroup | null>(null)

  const invalidate = () => qc.invalidateQueries({ queryKey: ["image-groups"] })

  const openCreate = () => {
    setEditing(null)
    setForm({ name: "", categoryId: cats[0]?.id ?? "", enabled: true, orderNumber: "0" })
    setRows([])
    setOpen(true)
  }
  const openEdit = (g: ImageGroup) => {
    setEditing(g)
    setForm({
      name: g.name ?? "",
      categoryId: g.categoryId ?? "",
      enabled: g.enabled !== false,
      orderNumber: String(g.orderNumber ?? 0),
    })
    setRows((g.images ?? []).map((i) => ({ name: i.name ?? "", version: i.version ?? "", orderNumber: i.orderNumber ?? 0 })))
    setOpen(true)
  }

  const buildBody = (f: GroupForm, imgs: ImageBinding[], base: ImageGroup | null) => ({
    name: f.name,
    categoryId: f.categoryId,
    enabled: f.enabled,
    orderNumber: Number(f.orderNumber) || 0,
    // PUT overwrites all fields — preserve the ones the form does not surface.
    description: base?.description,
    groupLogoUrl: base?.groupLogoUrl,
    labels: base?.labels ?? [],
    images: imgs
      .filter((r) => (r.name ?? "").trim() !== "")
      .map((r, i) => ({ name: r.name, version: r.version ?? "", orderNumber: r.orderNumber ?? i })),
  })

  const save = useMutation({
    mutationFn: () => {
      const body = buildBody(form, rows, editing)
      return editing
        ? apiFetch(`${IMAGE_GROUP_PATH}/${editing.id}`, { method: "PUT", body })
        : apiFetch(IMAGE_GROUP_PATH, { method: "POST", body })
    },
    onSuccess: () => {
      setOpen(false)
      invalidate()
      toast.success(editing ? "Image group updated" : "Image group created")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const toggle = useMutation({
    mutationFn: (g: ImageGroup) => {
      const body = buildBody(
        { name: g.name ?? "", categoryId: g.categoryId ?? "", enabled: !g.enabled, orderNumber: String(g.orderNumber ?? 0) },
        g.images ?? [],
        g,
      )
      return apiFetch(`${IMAGE_GROUP_PATH}/${g.id}`, { method: "PUT", body })
    },
    onSuccess: (_d, g) => {
      invalidate()
      toast.success(g.enabled ? "Image group disabled" : "Image group enabled")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: (id: string) => apiFetch(`${IMAGE_GROUP_PATH}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      setDeleting(null)
      invalidate()
      toast.success("Image group deleted")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  if (catsQ.isLoading || (cats.length > 0 && groupsQ.isLoading)) return <Skeleton className="h-64 rounded-xl" />
  if (catsQ.error) return <ErrorPanel error={catsQ.error} />
  if (groupsQ.error) return <ErrorPanel error={groupsQ.error} />

  // Select options: live glance names ∪ names already on the editing group (offline regions still show).
  const imageChoices = dedupe([...glanceNames, ...rows.map((r) => r.name ?? "")])

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <Button onClick={openCreate} disabled={cats.length === 0}>
          <Plus className="size-4" /> Create image group
        </Button>
      </div>
      {cats.length === 0 ? (
        <EmptyState icon={Boxes} title="No image groups" hint="Create an image category first — groups hang off categories." />
      ) : (
        <div className="space-y-6">
          {/* Grouped per category — one table per section, so the bare Table
              stays (a single DataTable would flatten the grouping). */}
          {(groupsQ.data ?? []).map(({ cat, groups }) => (
            <div key={cat.id}>
              <h3 className="text-eyebrow mb-2">{cat.name ?? cat.id}</h3>
              {groups.length === 0 ? (
                <p className="text-sm text-muted-foreground">No groups in this category.</p>
              ) : (
                <div className="overflow-hidden rounded-xl border bg-card">
                  <Table>
                    <TableHeader>
                      <TableRow className="hover:bg-transparent">
                        <TableHead>Name</TableHead>
                        <TableHead>Order</TableHead>
                        <TableHead>Images</TableHead>
                        <TableHead>Enabled</TableHead>
                        <TableHead className="w-12" />
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {groups.map((g) => (
                        <TableRow key={g.id}>
                          <TableCell className="font-medium">{g.name ?? "—"}</TableCell>
                          <TableCell className="tabular-nums">{g.orderNumber ?? 0}</TableCell>
                          <TableCell className="tabular-nums">{(g.images ?? []).length}</TableCell>
                          <TableCell>
                            <Switch
                              checked={g.enabled !== false}
                              onCheckedChange={() => toggle.mutate(g)}
                              aria-label={`Toggle ${g.name ?? g.id}`}
                            />
                          </TableCell>
                          <TableCell className="text-right">
                            <DropdownMenu>
                              <DropdownMenuTrigger asChild>
                                <Button variant="ghost" size="icon-sm" aria-label="Image group actions">
                                  <MoreHorizontal className="size-4" />
                                </Button>
                              </DropdownMenuTrigger>
                              <DropdownMenuContent align="end">
                                <DropdownMenuItem onClick={() => openEdit(g)}>Edit</DropdownMenuItem>
                                <DropdownMenuItem className="text-destructive" onClick={() => setDeleting(g)}>
                                  Delete
                                </DropdownMenuItem>
                              </DropdownMenuContent>
                            </DropdownMenu>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{editing ? "Edit image group" : "Create image group"}</DialogTitle>
            <DialogDescription>Bind live Glance images clients can launch from this group.</DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="ig-name">Name</Label>
              <Input id="ig-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ig-cat">Category</Label>
              <Select value={form.categoryId} onValueChange={(v) => setForm({ ...form, categoryId: v })}>
                <SelectTrigger id="ig-cat" className="w-full">
                  <SelectValue placeholder="Select a category" />
                </SelectTrigger>
                <SelectContent>
                  {cats.map((c) => (
                    <SelectItem key={c.id} value={c.id}>{c.name ?? c.id}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ig-order">Order number</Label>
              <Input id="ig-order" type="number" value={form.orderNumber} onChange={(e) => setForm({ ...form, orderNumber: e.target.value })} />
            </div>
            <label className="flex items-center gap-2 self-end pb-2 text-sm">
              <Switch checked={form.enabled} onCheckedChange={(v) => setForm({ ...form, enabled: v === true })} />
              Enabled
            </label>
            <div className="space-y-2 sm:col-span-2">
              <Label>Image bindings</Label>
              {rows.map((row, i) => (
                <div key={i} className="space-y-1">
                  <div className="flex gap-2">
                    <Select value={row.name ?? ""} onValueChange={(v) => setRows(rows.map((x, j) => (j === i ? { ...x, name: v } : x)))}>
                      <SelectTrigger className="flex-1" aria-label="Glance image">
                        <SelectValue placeholder="Glance image" />
                      </SelectTrigger>
                      <SelectContent>
                        {imageChoices.map((n) => (
                          <SelectItem key={n} value={n}>
                            {hiddenFromClients(n) ? `${n} (not public)` : n}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <Input
                      className="w-28"
                      placeholder="Version"
                      aria-label="Version"
                      value={row.version ?? ""}
                      onChange={(e) => setRows(rows.map((x, j) => (j === i ? { ...x, version: e.target.value } : x)))}
                    />
                    <Input
                      className="w-20"
                      type="number"
                      placeholder="Order"
                      aria-label="Order"
                      value={String(row.orderNumber ?? 0)}
                      onChange={(e) => setRows(rows.map((x, j) => (j === i ? { ...x, orderNumber: Number(e.target.value) || 0 } : x)))}
                    />
                    <Button variant="ghost" size="icon" aria-label="Remove image binding" onClick={() => setRows(rows.filter((_, j) => j !== i))}>
                      <Trash2 className="size-4" />
                    </Button>
                  </div>
                  {hiddenFromClients(row.name) ? (
                    <p className="text-xs text-amber-600 dark:text-amber-500">
                      <TriangleAlert className="mr-1.5 inline size-3.5 align-[-2px]" />
                      Not public in Glance — clients cannot see or launch this image. Run{" "}
                      <code className="rounded bg-muted px-1">openstack image set --public</code> to publish it.
                    </p>
                  ) : null}
                </div>
              ))}
              <Button
                variant="outline"
                size="sm"
                onClick={() => setRows([...rows, { name: "", version: "", orderNumber: rows.length }])}
                disabled={imageChoices.length === 0}
              >
                <Plus className="size-4" /> Add image
              </Button>
              {osImagesQ.isLoading ? <p className="text-xs text-muted-foreground">Loading live Glance images…</p> : null}
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
            <Button onClick={() => save.mutate()} disabled={save.isPending || !form.name || !form.categoryId}>
              {save.isPending ? "Saving…" : editing ? "Save changes" : "Create image group"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete image group</DialogTitle>
            <DialogDescription>Delete “{deleting?.name}”? Clients can no longer launch its images.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.id)} disabled={remove.isPending}>
              {remove.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// ── Instance metadata options ────────────────────────────────────────────────

const META_TYPES = ["PREDEFINED_VALUES", "TEXT", "NUMERIC_RANGE"] as const

// Sentence-case labels for the enum codes sent to the API.
const META_TYPE_LABELS: Record<string, string> = {
  PREDEFINED_VALUES: "Predefined values",
  TEXT: "Text",
  NUMERIC_RANGE: "Numeric range",
}

type MetaOptRow = { value: string; displayName: string; enabled: boolean }
type MetaForm = {
  key: string
  displayName: string
  description: string
  type: string
  options: MetaOptRow[]
  min: string
  max: string
  unit: string
  userEditable: boolean
  showInline: boolean
}

const emptyMetaForm = (): MetaForm => ({
  key: "",
  displayName: "",
  description: "",
  type: "PREDEFINED_VALUES",
  options: [{ value: "", displayName: "", enabled: true }],
  min: "0",
  max: "0",
  unit: "",
  userEditable: false,
  showInline: false,
})

function MetadataOptionsTab() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useAdminList<MetaOption>(META_PATH)
  const items = data?.data ?? []
  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin-list", META_PATH] })

  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<MetaOption | null>(null)
  const [form, setForm] = useState<MetaForm>(emptyMetaForm())
  const [deleting, setDeleting] = useState<MetaOption | null>(null)

  const openCreate = () => {
    setEditing(null)
    setForm(emptyMetaForm())
    setOpen(true)
  }
  const openEdit = useCallback((o: MetaOption) => {
    setEditing(o)
    setForm({
      key: o.key ?? "",
      displayName: o.displayName ?? "",
      description: o.description ?? "",
      type: o.type ?? "PREDEFINED_VALUES",
      options:
        (o.options ?? []).length > 0
          ? (o.options ?? []).map((x) => ({ value: x.value ?? "", displayName: x.displayName ?? "", enabled: x.enabled !== false }))
          : [{ value: "", displayName: "", enabled: true }],
      min: String(o.numericRange?.min ?? 0),
      max: String(o.numericRange?.max ?? 0),
      unit: o.numericRange?.unit ?? "",
      userEditable: o.userEditable === true,
      showInline: o.showInline === true,
    })
    setOpen(true)
  }, [])

  const save = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = {
        key: form.key,
        displayName: form.displayName,
        description: form.description,
        type: form.type,
        userEditable: form.userEditable,
        showInline: form.showInline,
      }
      if (form.type === "PREDEFINED_VALUES") {
        body.options = form.options
          .filter((o) => o.value.trim() !== "")
          .map((o) => ({ value: o.value.trim(), displayName: o.displayName.trim() || o.value.trim(), enabled: o.enabled }))
      }
      if (form.type === "NUMERIC_RANGE") {
        body.numericRange = { min: Number(form.min) || 0, max: Number(form.max) || 0, unit: form.unit || undefined }
      }
      // PUT overwrites all mutable fields — preserve the appliesTo scoping the form does not surface.
      if (editing?.serviceIds) body.serviceIds = editing.serviceIds
      if (editing?.regions) body.regions = editing.regions
      return editing
        ? apiFetch(`${META_PATH}/${editing.id}`, { method: "PUT", body })
        : apiFetch(META_PATH, { method: "POST", body })
    },
    onSuccess: () => {
      setOpen(false)
      invalidate()
      toast.success(editing ? "Metadata option updated" : "Metadata option created")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  // enabled Switch → disable = soft DELETE, enable = POST /{id}/reactivate.
  const toggle = useMutation({
    mutationFn: ({ opt, on }: { opt: MetaOption; on: boolean }) =>
      on
        ? apiFetch(`${META_PATH}/${opt.id}/reactivate`, { method: "POST", body: {} })
        : apiFetch(`${META_PATH}/${opt.id}`, { method: "DELETE" }),
    onSuccess: (_d, v) => {
      invalidate()
      toast.success(v.on ? "Option reactivated" : "Option disabled")
    },
    onError: (e) => toast.error((e as Error).message),
  })
  const toggleOption = toggle.mutate

  // permanent delete (must be disabled first — the API 400s an active option).
  const remove = useMutation({
    mutationFn: (id: string) => apiFetch(`${META_PATH}/${id}?permanent=true`, { method: "DELETE" }),
    onSuccess: () => {
      setDeleting(null)
      invalidate()
      toast.success("Metadata option deleted")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const columns = useMemo<ColumnDef<MetaOption, any>[]>(
    () => [
      {
        id: "key",
        accessorFn: (o) => o.key ?? "",
        header: sortableHeader("Key"),
        cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() || "—"}</span>,
      },
      {
        id: "displayName",
        accessorFn: (o) => o.displayName ?? "",
        header: sortableHeader("Display name"),
        cell: ({ getValue }) => <span>{getValue() || "—"}</span>,
      },
      {
        id: "type",
        accessorFn: (o) => o.type ?? "",
        header: sortableHeader("Type"),
        cell: ({ getValue }) => (
          <span className="text-muted-foreground">{META_TYPE_LABELS[getValue() as string] ?? getValue() ?? "—"}</span>
        ),
      },
      {
        id: "enabled",
        accessorFn: (o) => (o.enabled === true ? 1 : 0),
        header: sortableHeader("Enabled"),
        cell: ({ row }) => {
          const o = row.original
          return (
            <Switch
              checked={o.enabled === true}
              onCheckedChange={(on) => toggleOption({ opt: o, on })}
              aria-label={`Toggle ${o.key ?? o.id}`}
            />
          )
        },
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const o = row.original
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label="Metadata option actions">
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => openEdit(o)}>Edit</DropdownMenuItem>
                  <DropdownMenuItem className="text-destructive" onClick={() => setDeleting(o)}>
                    Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // useState setters are stable; openEdit is useCallback'd, mutate is stable.
    [openEdit, toggleOption],
  )

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <Button onClick={openCreate}>
          <Plus className="size-4" /> Create metadata option
        </Button>
      </div>
      {!isLoading && !error && items.length === 0 ? (
        <EmptyState
          icon={ImageIcon}
          title="No instance metadata options"
          hint="Metadata options let clients tag servers with curated key/value pairs."
          action={<Button onClick={openCreate}>Create metadata option</Button>}
        />
      ) : (
        <DataTable
          columns={columns}
          data={data?.data}
          isLoading={isLoading}
          error={error ? (error as Error) : null}
          searchPlaceholder="Search metadata options…"
          getRowId={(o) => o.id}
        />
      )}

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{editing ? "Edit metadata option" : "Create metadata option"}</DialogTitle>
            <DialogDescription>Keys may not start with hw:, os_ or proxcloud_.</DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="mo-key">Key</Label>
              <Input id="mo-key" placeholder="owner" className="font-mono" value={form.key} onChange={(e) => setForm({ ...form, key: e.target.value })} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="mo-display">Display name</Label>
              <Input id="mo-display" value={form.displayName} onChange={(e) => setForm({ ...form, displayName: e.target.value })} />
            </div>
            <div className="space-y-1.5 sm:col-span-2">
              <Label htmlFor="mo-desc">Description</Label>
              <Textarea id="mo-desc" value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} />
            </div>
            <div className="space-y-1.5 sm:col-span-2">
              <Label htmlFor="mo-type">Type</Label>
              <Select value={form.type} onValueChange={(v) => setForm({ ...form, type: v })}>
                <SelectTrigger id="mo-type" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {META_TYPES.map((t) => (
                    <SelectItem key={t} value={t}>{META_TYPE_LABELS[t] ?? t}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {form.type === "PREDEFINED_VALUES" ? (
              <div className="space-y-2 sm:col-span-2">
                <Label>Options</Label>
                {form.options.map((o, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <Input
                      placeholder="Value"
                      aria-label="Option value"
                      className="font-mono"
                      value={o.value}
                      onChange={(e) => setForm({ ...form, options: form.options.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)) })}
                    />
                    <Input
                      placeholder="Display name"
                      aria-label="Option display name"
                      value={o.displayName}
                      onChange={(e) => setForm({ ...form, options: form.options.map((x, j) => (j === i ? { ...x, displayName: e.target.value } : x)) })}
                    />
                    <label className="flex shrink-0 items-center gap-1 text-xs">
                      <Checkbox
                        checked={o.enabled}
                        onCheckedChange={(v) => setForm({ ...form, options: form.options.map((x, j) => (j === i ? { ...x, enabled: v === true } : x)) })}
                      />
                      On
                    </label>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label="Remove option"
                      onClick={() => setForm({ ...form, options: form.options.filter((_, j) => j !== i) })}
                      disabled={form.options.length === 1}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </div>
                ))}
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setForm({ ...form, options: [...form.options, { value: "", displayName: "", enabled: true }] })}
                >
                  <Plus className="size-4" /> Add option
                </Button>
              </div>
            ) : null}
            {form.type === "NUMERIC_RANGE" ? (
              <div className="grid grid-cols-3 gap-2 sm:col-span-2">
                <div className="space-y-1.5">
                  <Label htmlFor="mo-min">Min</Label>
                  <Input id="mo-min" type="number" value={form.min} onChange={(e) => setForm({ ...form, min: e.target.value })} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="mo-max">Max</Label>
                  <Input id="mo-max" type="number" value={form.max} onChange={(e) => setForm({ ...form, max: e.target.value })} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="mo-unit">Unit</Label>
                  <Input id="mo-unit" value={form.unit} onChange={(e) => setForm({ ...form, unit: e.target.value })} />
                </div>
              </div>
            ) : null}
            <div className="flex gap-6 sm:col-span-2">
              <label className="flex items-center gap-2 text-sm">
                <Switch checked={form.userEditable} onCheckedChange={(v) => setForm({ ...form, userEditable: v === true })} />
                User editable
              </label>
              <label className="flex items-center gap-2 text-sm">
                <Switch checked={form.showInline} onCheckedChange={(v) => setForm({ ...form, showInline: v === true })} />
                Show inline
              </label>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
            <Button onClick={() => save.mutate()} disabled={save.isPending || !form.key || !form.displayName}>
              {save.isPending ? "Saving…" : editing ? "Save changes" : "Create option"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete metadata option</DialogTitle>
            <DialogDescription>
              Permanently delete “{deleting?.key}”? Disable it first if it is still active. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.id)} disabled={remove.isPending}>
              {remove.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

export default function CatalogPage() {
  return (
    <>
      <PageHeader
        title="Catalog"
        eyebrow="System"
        description="Flavor categories, image groups and instance metadata offered to clients."
      />
      <Tabs defaultValue="flavors">
        {/* 4 triggers overflow narrow viewports — the wrapper scrolls horizontally
            instead of wrapping/clipping (thin scrollbar as the affordance). */}
        <div className="-mx-1 overflow-x-auto px-1 pb-1">
          <TabsList className="w-max">
            <TabsTrigger value="flavors">Flavor categories</TabsTrigger>
            <TabsTrigger value="categories">Image categories</TabsTrigger>
            <TabsTrigger value="groups">Image groups</TabsTrigger>
            <TabsTrigger value="metadata">Instance metadata</TabsTrigger>
          </TabsList>
        </div>
        <TabsContent value="flavors" className="mt-4">
          <FlavorCategoriesTab />
        </TabsContent>
        <TabsContent value="categories" className="mt-4">
          <ImageCategoriesTab />
        </TabsContent>
        <TabsContent value="groups" className="mt-4">
          <ImageGroupsTab />
        </TabsContent>
        <TabsContent value="metadata" className="mt-4">
          <MetadataOptionsTab />
        </TabsContent>
      </Tabs>
    </>
  )
}
