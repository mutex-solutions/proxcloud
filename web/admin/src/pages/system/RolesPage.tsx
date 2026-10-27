import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { ChevronDown, ChevronRight, Pencil, Plus, RefreshCw, ShieldCheck, Trash2 } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
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
import { Skeleton } from "@/components/ui/skeleton"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"

type AdminRole = {
  id: string
  name: string
  description?: string
  permissions: string[]
  expandedPermissions: string[]
  builtIn: boolean
}

type PermissionMeta = { key: string; description: string }

const ROLES_PATH = "/admin/admin-roles"

// Server contract (internal/platform/admin/adminrole.go):
//   POST   /admin/admin-roles         { name, description, permissions[] }  name ^[A-Z][A-Z0-9_]*$, reserved built-ins refused
//   PUT    /admin/admin-roles/{id}    { description, permissions[] }        name is immutable — not accepted
//   DELETE /admin/admin-roles/{id}    refused while any admin user still holds the role
const NAME_RE = /^[A-Z][A-Z0-9_]*$/
const BUILT_IN = new Set(["SUPER_ADMIN", "ADMIN", "SUPPORT", "BILLING_ADMIN", "VIEWER"])

// area = the segment between admin: and the trailing :action (admin:billing_config:update → billing_config).
function areaOf(key: string) {
  return key.split(":")[1] ?? "other"
}
function pretty(area: string) {
  return area.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase())
}

type FormState = { name: string; description: string; permissions: Set<string> }
const emptyForm = (): FormState => ({ name: "", description: "", permissions: new Set() })

// Permission catalog columns — module scope, referentially stable.
const permColumns: ColumnDef<PermissionMeta, any>[] = [
  {
    id: "key",
    accessorFn: (p) => p.key,
    header: sortableHeader("Permission"),
    cell: ({ getValue }) => <span className="font-mono text-xs">{getValue()}</span>,
  },
  {
    id: "description",
    accessorFn: (p) => p.description,
    header: "Description",
    cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue()}</span>,
  },
]

// Grouped checkbox picker. form.permissions is the source of truth — any token not in the catalog
// (e.g. a stored "admin:user:*" wildcard on an edited role) rides along untouched so a save never
// silently drops it. ponytail: catalog offers the 58 concrete keys; wildcards are preserved, not editable.
function PermissionPicker({
  perms,
  form,
  setForm,
}: {
  perms: PermissionMeta[]
  form: FormState
  setForm: (f: FormState) => void
}) {
  const groups = useMemo(() => {
    const m = new Map<string, PermissionMeta[]>()
    for (const p of perms) {
      const a = areaOf(p.key)
      const list = m.get(a) ?? []
      list.push(p)
      m.set(a, list)
    }
    return [...m.entries()]
  }, [perms])

  const toggle = (key: string, on: boolean) => {
    const next = new Set(form.permissions)
    if (on) next.add(key)
    else next.delete(key)
    setForm({ ...form, permissions: next })
  }
  const toggleGroup = (keys: string[], on: boolean) => {
    const next = new Set(form.permissions)
    for (const k of keys) {
      if (on) next.add(k)
      else next.delete(k)
    }
    setForm({ ...form, permissions: next })
  }

  if (!perms.length) {
    return <p className="text-sm text-muted-foreground">No permissions available.</p>
  }

  return (
    <div className="max-h-[45vh] space-y-3 overflow-y-auto rounded-md border p-3">
      {groups.map(([area, items]) => {
        const keys = items.map((i) => i.key)
        const sel = keys.filter((k) => form.permissions.has(k)).length
        const groupChecked = sel === 0 ? false : sel === keys.length ? true : "indeterminate"
        return (
          <div key={area}>
            <label className="flex cursor-pointer items-center gap-2 border-b pb-1">
              <Checkbox checked={groupChecked} onCheckedChange={(v) => toggleGroup(keys, v === true)} />
              <span className="text-sm font-medium">{pretty(area)}</span>
              <span className="text-xs text-muted-foreground">
                {sel}/{keys.length}
              </span>
            </label>
            <div className="mt-1 grid gap-x-4 gap-y-1 sm:grid-cols-2">
              {items.map((p) => (
                <label key={p.key} className="flex cursor-pointer items-start gap-2 py-1">
                  <Checkbox
                    className="mt-0.5"
                    checked={form.permissions.has(p.key)}
                    onCheckedChange={(v) => toggle(p.key, v === true)}
                  />
                  <span className="min-w-0">
                    <span className="block text-sm leading-tight">{p.description}</span>
                    <span className="block font-mono text-xs text-muted-foreground">{p.key}</span>
                  </span>
                </label>
              ))}
            </div>
          </div>
        )
      })}
    </div>
  )
}

export default function RolesPage() {
  const qc = useQueryClient()
  const rolesQ = useAdminList<AdminRole>(ROLES_PATH)
  const permsQ = useAdminList<PermissionMeta>("/admin/admin-permissions/available-permissions")
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  const roles = rolesQ.data?.data ?? []
  const perms = permsQ.data?.data ?? []
  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin-list", ROLES_PATH] })

  // null = closed; { role: null } = create; { role } = edit that role.
  const [dialog, setDialog] = useState<{ role: AdminRole | null } | null>(null)
  const [form, setForm] = useState<FormState>(emptyForm())
  const [toDelete, setToDelete] = useState<AdminRole | null>(null)

  const editing = dialog?.role ?? null

  const openCreate = () => {
    setForm(emptyForm())
    setDialog({ role: null })
  }
  const openEdit = (role: AdminRole) => {
    setForm({ name: role.name, description: role.description ?? "", permissions: new Set(role.permissions ?? []) })
    setDialog({ role })
  }

  const createRole = useMutation({
    mutationFn: () =>
      apiFetch(ROLES_PATH, {
        method: "POST",
        body: { name: form.name, description: form.description, permissions: [...form.permissions] },
      }),
    onSuccess: () => {
      toast.success("Role created")
      setDialog(null)
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const updateRole = useMutation({
    mutationFn: (id: string) =>
      apiFetch(`${ROLES_PATH}/${id}`, {
        method: "PUT",
        body: { description: form.description, permissions: [...form.permissions] },
      }),
    onSuccess: () => {
      toast.success("Role updated")
      setDialog(null)
      void invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const deleteRole = useMutation({
    mutationFn: (id: string) => apiFetch(`${ROLES_PATH}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Role deleted")
      setToDelete(null)
      void invalidate()
    },
    // Refused if any admin user is still assigned the role — surface the API message.
    onError: (e: Error) => toast.error(e.message),
  })

  const nameValid = NAME_RE.test(form.name) && !BUILT_IN.has(form.name)
  const canSubmit = form.permissions.size > 0 && (editing ? true : nameValid)
  const saving = createRole.isPending || updateRole.isPending
  const submit = () => (editing ? updateRole.mutate(editing.id) : createRole.mutate())

  return (
    <>
      <PageHeader
        title="Admin roles"
        eyebrow="System"
        description="Built-in operator roles plus custom roles you define, and the permission catalog."
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void rolesQ.refetch()}
              disabled={rolesQ.isFetching}
              aria-label="Refresh"
            >
              <RefreshCw className={rolesQ.isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={openCreate} disabled={permsQ.isLoading}>
              <Plus className="size-4" /> Create role
            </Button>
          </>
        }
      />
      <Tabs defaultValue="roles">
        <TabsList>
          <TabsTrigger value="roles">Roles</TabsTrigger>
          <TabsTrigger value="permissions">Permission catalog</TabsTrigger>
        </TabsList>

        <TabsContent value="roles" className="mt-4">
          {rolesQ.isLoading ? (
            <Skeleton className="h-64" />
          ) : rolesQ.error ? (
            <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">
              {(rolesQ.error as Error).message}
            </div>
          ) : roles.length === 0 ? (
            <EmptyState
              icon={ShieldCheck}
              title="No admin roles"
              action={
                <Button onClick={openCreate}>
                  <Plus className="size-4" /> Create role
                </Button>
              }
            />
          ) : (
            <div className="space-y-3">
              {roles.map((role) => {
                const open = expanded[role.id] === true
                return (
                  <Card key={role.id}>
                    <CardContent className="p-4">
                      <div className="flex items-center justify-between gap-4 max-sm:flex-wrap">
                        <div className="flex min-w-0 items-center gap-3">
                          <ShieldCheck className="size-5 shrink-0 text-muted-foreground/60" />
                          <div className="min-w-0">
                            <div className="flex items-center gap-2">
                              <p className="truncate font-medium">{role.name}</p>
                              {role.builtIn ? (
                                <Badge variant="outline">Built-in</Badge>
                              ) : (
                                <Badge variant="secondary">Custom</Badge>
                              )}
                            </div>
                            <p className="text-sm text-muted-foreground">
                              {role.description ? `${role.description} · ` : ""}
                              {role.expandedPermissions?.length ?? 0} permissions
                            </p>
                          </div>
                        </div>
                        <div className="flex shrink-0 items-center gap-1">
                          {!role.builtIn && (
                            <>
                              <Button
                                variant="ghost"
                                size="icon"
                                aria-label={`Edit role ${role.name}`}
                                onClick={() => openEdit(role)}
                              >
                                <Pencil className="size-4 text-muted-foreground" />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon"
                                aria-label={`Delete role ${role.name}`}
                                onClick={() => setToDelete(role)}
                              >
                                <Trash2 className="size-4 text-muted-foreground" />
                              </Button>
                            </>
                          )}
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() => setExpanded({ ...expanded, [role.id]: !open })}
                          >
                            {open ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
                            {open ? "Hide permissions" : "Show permissions"}
                          </Button>
                        </div>
                      </div>
                      {open ? (
                        <div className="mt-3 flex flex-wrap gap-1 border-t pt-3">
                          {(role.expandedPermissions ?? []).map((p) => (
                            <Badge key={p} variant="secondary" className="font-mono text-xs">
                              {p}
                            </Badge>
                          ))}
                        </div>
                      ) : null}
                    </CardContent>
                  </Card>
                )
              })}
            </div>
          )}
        </TabsContent>

        <TabsContent value="permissions" className="mt-4">
          <DataTable
            columns={permColumns}
            data={perms}
            isLoading={permsQ.isLoading}
            error={permsQ.error as Error | null}
            searchPlaceholder="Search permissions…"
            getRowId={(p) => p.key}
          />
        </TabsContent>
      </Tabs>

      <Dialog open={dialog !== null} onOpenChange={(o) => !o && setDialog(null)}>
        <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{editing ? `Edit role ${editing.name}` : "Create role"}</DialogTitle>
            <DialogDescription>
              {editing
                ? "The role name is fixed. Update its description and permissions."
                : "Define a custom operator role and the permissions it grants."}
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label htmlFor="role-name">Name</Label>
              <Input
                id="role-name"
                value={form.name}
                disabled={!!editing}
                onChange={(e) => setForm({ ...form, name: e.target.value.toUpperCase() })}
                placeholder="SUPPORT_TIER2"
                aria-invalid={!editing && form.name !== "" && !nameValid}
              />
              <p className="text-xs text-muted-foreground">A–Z, 0–9, _; must start with a letter.</p>
              {!editing && form.name !== "" && BUILT_IN.has(form.name) ? (
                <p className="text-xs text-destructive">{form.name} is a reserved built-in role name.</p>
              ) : null}
            </div>
            <div className="grid gap-2">
              <Label htmlFor="role-desc">Description</Label>
              <Textarea
                id="role-desc"
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
                placeholder="What this role is for"
                rows={2}
              />
            </div>
            <div className="grid gap-2">
              <Label>Permissions ({form.permissions.size} selected)</Label>
              <PermissionPicker perms={perms} form={form} setForm={setForm} />
            </div>
          </div>

          <DialogFooter>
            <Button variant="outline" onClick={() => setDialog(null)}>
              Cancel
            </Button>
            <Button onClick={submit} disabled={!canSubmit || saving}>
              {editing ? "Save changes" : "Create role"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete role</DialogTitle>
            <DialogDescription>
              Delete "{toDelete?.name}"? A role still assigned to any admin user will refuse to delete.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => toDelete && deleteRole.mutate(toDelete.id)}
              disabled={deleteRole.isPending}
            >
              Delete role
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
