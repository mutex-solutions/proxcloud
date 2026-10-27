import { useCallback, useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { Eye, FileText, Mail, MoreHorizontal, Pencil, Plus, Trash2, Undo2 } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
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
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { useAdminGet, useAdminList } from "@/lib/hooks"

type MessageTemplate = {
  id: string
  key?: string
  category?: string
  messageTitle?: string
  messageBody?: string
  disabled?: boolean
  systemTemplate?: boolean
}

type Placeholder = { key: string; description: string }
// GET /placeholders → { CATEGORY: [{key, description}], ... }
type PlaceholderMap = Record<string, Placeholder[]>

// Editor state — one dialog drives both create and edit.
// PUT update() only honors messageTitle/messageBody/disabled (key/category are
// server-immutable), so those are shown read-only in edit mode.
type Editor = {
  mode: "create" | "edit"
  id?: string
  key: string
  category: string
  subject: string
  body: string
  disabled: boolean
  systemTemplate: boolean
}

type PdfTemplate = {
  id: string
  name?: string
  description?: string
  type?: string
  content?: string
}

const MSG_PATH = "/admin/message-templates"
const PDF_PATH = "/admin/pdf-templates"

// ── Message templates ────────────────────────────────────────────────────────

function MessageTemplatesTab() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useAdminList<MessageTemplate>(MSG_PATH)
  const { data: placeholders } = useAdminGet<PlaceholderMap>(`${MSG_PATH}/placeholders`)
  const items = data?.data ?? []
  const categories = Object.keys(placeholders ?? {})

  const [editor, setEditor] = useState<Editor | null>(null)
  const [deleting, setDeleting] = useState<MessageTemplate | null>(null)

  const openCreate = () =>
    setEditor({ mode: "create", key: "", category: "", subject: "", body: "", disabled: false, systemTemplate: false })
  // Stable callback (setter only) so the column defs can memoize over it.
  const openEdit = useCallback(
    (t: MessageTemplate) =>
      setEditor({
        mode: "edit",
        id: t.id,
        key: t.key ?? "",
        category: t.category ?? "",
        subject: t.messageTitle ?? "",
        body: t.messageBody ?? "",
        disabled: t.disabled === true,
        systemTemplate: t.systemTemplate === true,
      }),
    [],
  )
  const patch = (p: Partial<Editor>) => setEditor((e) => (e ? { ...e, ...p } : e))

  const save = useMutation({
    // Create persists the whole body; PUT update only honors messageTitle/messageBody/
    // disabled — the extra keys are ignored server-side but sent for a full field set.
    mutationFn: (e: Editor) => {
      const payload = {
        key: e.key.trim(),
        category: e.category,
        messageTitle: e.subject,
        messageBody: e.body,
        disabled: e.disabled,
        systemTemplate: e.systemTemplate,
      }
      return e.mode === "create"
        ? apiFetch(MSG_PATH, { method: "POST", body: payload })
        : apiFetch(`${MSG_PATH}/${e.id}`, { method: "PUT", body: payload })
    },
    onSuccess: (_r, e) => {
      setEditor(null)
      qc.invalidateQueries({ queryKey: ["admin-list", MSG_PATH] })
      toast.success(e.mode === "create" ? "Template created" : "Template saved")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const del = useMutation({
    mutationFn: (t: MessageTemplate) => apiFetch(`${MSG_PATH}/${t.id}`, { method: "DELETE" }),
    onSuccess: () => {
      setDeleting(null)
      qc.invalidateQueries({ queryKey: ["admin-list", MSG_PATH] })
      toast.success("Template deleted")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const copyPlaceholder = (key: string) =>
    navigator.clipboard?.writeText(key).then(
      () => toast.success(`Copied ${key}`),
      () => toast.error("Copy failed"),
    )

  const canSave = editor !== null && (editor.mode === "edit" || editor.key.trim() !== "")

  const columns = useMemo<ColumnDef<MessageTemplate, any>[]>(
    () => [
      {
        id: "key",
        accessorFn: (t) => t.key ?? "",
        header: sortableHeader("Name"),
        cell: ({ row }) => {
          const t = row.original
          return (
            <span className="inline-flex flex-wrap items-center gap-2 font-mono text-xs">
              {t.key ?? "—"}
              {t.systemTemplate ? <Badge variant="secondary" className="font-sans">System</Badge> : null}
              {t.disabled ? <Badge variant="outline" className="font-sans">Disabled</Badge> : null}
            </span>
          )
        },
      },
      {
        id: "subject",
        accessorFn: (t) => t.messageTitle ?? "",
        header: sortableHeader("Subject"),
        cell: ({ getValue }) => <span className="text-sm">{getValue() || "—"}</span>,
      },
      {
        id: "category",
        accessorFn: (t) => t.category ?? "",
        header: sortableHeader("Category"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const t = row.original
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for template ${t.key ?? t.id}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => openEdit(t)}>
                    <Pencil className="size-4" /> Edit
                  </DropdownMenuItem>
                  <DropdownMenuItem variant="destructive" onClick={() => setDeleting(t)}>
                    <Trash2 className="size-4" /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // openEdit is a stable useCallback; setDeleting is a stable setter.
    [openEdit],
  )

  return (
    <>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          Email templates sent to customers. Subject and body use {"{{variable}}"} Mustache placeholders.
        </p>
        <Button size="sm" onClick={openCreate}>
          <Plus className="mr-1.5 size-4" /> Create template
        </Button>
      </div>

      {!isLoading && !error && items.length === 0 ? (
        <EmptyState icon={Mail} title="No message templates" hint="Create one, or system templates are seeded at startup." />
      ) : (
        <DataTable
          columns={columns}
          data={items}
          isLoading={isLoading}
          error={error as Error | null}
          searchPlaceholder="Search templates…"
          getRowId={(t) => t.id}
        />
      )}

      <Dialog open={!!editor} onOpenChange={(o) => !o && setEditor(null)}>
        <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>
              {editor?.mode === "create" ? "Create message template" : `Edit template ${editor?.key ?? ""}`}
            </DialogTitle>
            <DialogDescription>Subject and body are rendered as {"{{variable}}"} Mustache templates.</DialogDescription>
          </DialogHeader>

          {editor ? (
            <div className="space-y-3">
              {editor.mode === "create" ? (
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <div className="space-y-1.5">
                    <Label htmlFor="mt-key">Key</Label>
                    <Input
                      id="mt-key"
                      placeholder="e.g. welcome_email"
                      value={editor.key}
                      onChange={(e) => patch({ key: e.target.value })}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="mt-category">Category</Label>
                    <Select value={editor.category || undefined} onValueChange={(v) => patch({ category: v })}>
                      <SelectTrigger id="mt-category" className="w-full">
                        <SelectValue placeholder="Select a category" />
                      </SelectTrigger>
                      <SelectContent>
                        {categories.map((c) => (
                          <SelectItem key={c} value={c}>{c}</SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                </div>
              ) : (
                <p className="text-xs text-muted-foreground">
                  Key <span className="font-mono">{editor.key || "—"}</span>
                  {editor.category ? <> · category <span className="font-mono">{editor.category}</span></> : null}
                  {editor.systemTemplate ? <> · <span className="text-foreground">system template</span></> : null}
                </p>
              )}

              <div className="space-y-1.5">
                <Label htmlFor="mt-subject">Subject</Label>
                <Input id="mt-subject" value={editor.subject} onChange={(e) => patch({ subject: e.target.value })} />
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="mt-body">Body</Label>
                <Textarea
                  id="mt-body"
                  rows={10}
                  className="font-mono text-xs"
                  value={editor.body}
                  onChange={(e) => patch({ body: e.target.value })}
                />
                <p className="text-xs text-muted-foreground">Mustache syntax — insert a placeholder like {"{{firstName}}"}.</p>
              </div>

              {categories.length > 0 ? (
                <div className="space-y-1.5">
                  <Label>Placeholders</Label>
                  <div className="max-h-40 space-y-2 overflow-y-auto rounded-md border p-2">
                    {Object.entries(placeholders ?? {}).map(([cat, list]) => (
                      <div key={cat}>
                        <div className="text-xs font-medium text-muted-foreground">{cat}</div>
                        <div className="mt-1 flex flex-wrap gap-1">
                          {list.map((p) => (
                            <button
                              key={p.key}
                              type="button"
                              title={`${p.description} — click to copy`}
                              onClick={() => copyPlaceholder(p.key)}
                              className="rounded border bg-muted/40 px-1.5 py-0.5 font-mono text-[11px] hover:bg-muted"
                            >
                              {p.key}
                            </button>
                          ))}
                        </div>
                      </div>
                    ))}
                  </div>
                  <p className="text-xs text-muted-foreground">Click a placeholder to copy it to the clipboard.</p>
                </div>
              ) : null}

              <div className="flex items-center justify-between rounded-md border p-3">
                <div>
                  <Label htmlFor="mt-disabled">Disabled</Label>
                  <p className="text-xs text-muted-foreground">A disabled template is not sent.</p>
                </div>
                <Switch id="mt-disabled" checked={editor.disabled} onCheckedChange={(v) => patch({ disabled: v })} />
              </div>
            </div>
          ) : null}

          <DialogFooter>
            <Button variant="outline" onClick={() => setEditor(null)}>Cancel</Button>
            <Button onClick={() => editor && save.mutate(editor)} disabled={!canSave || save.isPending}>
              {save.isPending ? "Saving…" : editor?.mode === "create" ? "Create template" : "Save template"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete template</DialogTitle>
            <DialogDescription>
              Delete “{deleting?.key ?? deleting?.messageTitle}”? This cannot be undone.
              {deleting?.systemTemplate ? (
                <span className="mt-2 block text-destructive">
                  This looks like a system template — deleting it may break automated emails.
                </span>
              ) : null}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => deleting && del.mutate(deleting)} disabled={del.isPending}>
              {del.isPending ? "Deleting…" : "Delete template"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// ── PDF templates ────────────────────────────────────────────────────────────

function PdfTemplatesTab() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useAdminList<PdfTemplate>(PDF_PATH)
  const items = data?.data ?? []

  const [preview, setPreview] = useState<{ name: string; html: string } | null>(null)
  const [reverting, setReverting] = useState<PdfTemplate | null>(null)

  const runPreview = useMutation({
    // POST /{id}/preview takes the RAW template HTML as the body and returns the rendered
    // HTML string (dummy data), 200 even on a render error ("Template validation error: …").
    mutationFn: async (t: PdfTemplate) => {
      const html = await apiFetch<string>(`${PDF_PATH}/${t.id}/preview`, {
        method: "POST",
        rawBody: t.content ?? "",
        headers: { "Content-Type": "text/plain" },
      })
      return { name: t.name ?? t.id, html }
    },
    onSuccess: (res) => setPreview(res),
    onError: (e) => toast.error((e as Error).message),
  })

  const revert = useMutation({
    mutationFn: (t: PdfTemplate) => apiFetch(`${PDF_PATH}/${t.id}/revert-to-default`, { method: "POST", rawBody: "" }),
    onSuccess: () => {
      setReverting(null)
      qc.invalidateQueries({ queryKey: ["admin-list", PDF_PATH] })
      toast.success("Template reverted to default")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const columns = useMemo<ColumnDef<PdfTemplate, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (t) => t.name ?? "",
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue() || "—"}</span>,
      },
      {
        id: "type",
        accessorFn: (t) => t.type ?? "",
        header: sortableHeader("Type"),
        cell: ({ getValue }) => <Badge variant="outline">{getValue() || "—"}</Badge>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const t = row.original
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${t.name ?? t.id}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => runPreview.mutate(t)} disabled={runPreview.isPending}>
                    <Eye className="size-4" /> Preview
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => setReverting(t)}>
                    <Undo2 className="size-4" /> Revert to default
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // runPreview.mutate is stable; isPending drives the disabled state.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [runPreview.isPending],
  )

  if (!isLoading && !error && items.length === 0) {
    return <EmptyState icon={FileText} title="No PDF templates" hint="Invoice and statement layouts appear here." />
  }

  return (
    <>
      <DataTable
        columns={columns}
        data={items}
        isLoading={isLoading}
        error={error as Error | null}
        getRowId={(t) => t.id}
      />

      <Dialog open={!!preview} onOpenChange={(o) => !o && setPreview(null)}>
        <DialogContent className="sm:max-w-4xl">
          <DialogHeader>
            <DialogTitle>Preview — {preview?.name}</DialogTitle>
            <DialogDescription>Rendered with sample data.</DialogDescription>
          </DialogHeader>
          <iframe title="Template preview" sandbox="" srcDoc={preview?.html ?? ""} className="h-[70vh] w-full rounded-md border bg-white" />
        </DialogContent>
      </Dialog>

      <Dialog open={!!reverting} onOpenChange={(o) => !o && setReverting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revert to default</DialogTitle>
            <DialogDescription>
              Replace the content of “{reverting?.name}” with the built-in default? Custom changes are lost.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setReverting(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => reverting && revert.mutate(reverting)} disabled={revert.isPending}>
              {revert.isPending ? "Reverting…" : "Revert template"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

export default function TemplatesPage() {
  return (
    <>
      <PageHeader
        title="Templates"
        eyebrow="System"
        description="Email message templates and PDF document layouts."
      />
      <Tabs defaultValue="messages">
        <TabsList>
          <TabsTrigger value="messages">Message templates</TabsTrigger>
          <TabsTrigger value="pdf">PDF templates</TabsTrigger>
        </TabsList>
        <TabsContent value="messages" className="mt-4">
          <MessageTemplatesTab />
        </TabsContent>
        <TabsContent value="pdf" className="mt-4">
          <PdfTemplatesTab />
        </TabsContent>
      </Tabs>
    </>
  )
}
