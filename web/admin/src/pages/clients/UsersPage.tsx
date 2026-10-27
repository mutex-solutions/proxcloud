import { useMemo, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { MoreHorizontal, Plus, RefreshCw, Trash2, Users } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useAdminList } from "@/lib/hooks"


// GET /admin/user (handler.go listRaw "users") — raw user docs, shaped _id→id.
export type AdminUser = {
  id?: string
  sub?: string
  email?: string
  firstName?: string
  lastName?: string
  createdAt?: string
}

const LIST_PATH = "/admin/user"
const PAGE_SIZE = 50

export function userDisplayName(u: AdminUser): string {
  return [u.firstName, u.lastName].filter(Boolean).join(" ") || "—"
}

export default function UsersPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  // BE-paged (offset): GET /admin/user?limit=&offset= → { data:[page], paging:{total} }.
  const [pageIndex, setPageIndex] = useState(0)
  const listPath = `${LIST_PATH}?limit=${PAGE_SIZE}&offset=${pageIndex * PAGE_SIZE}`
  const { data, isLoading, isFetching, error, refetch } = useAdminList<AdminUser>(listPath)
  const [createOpen, setCreateOpen] = useState(false)
  const [form, setForm] = useState({ email: "", firstName: "", lastName: "" })
  const [projectIds, setProjectIds] = useState<string[]>([])
  const [toDelete, setToDelete] = useState<AdminUser | null>(null)

  // GET /admin/project (clientarea_reads.go projectAdminList) — for the create-dialog invite picker.
  const projects = useAdminList<{ id?: string; name?: string }>("/admin/project", createOpen)

  const users = data?.data ?? []
  const total = data?.paging?.total ?? 0

  // Paged list keys on the full (param-bearing) path, so invalidate the whole admin-list family.
  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin-list"] })

  // POST /admin/user (user.go userCreate) — body {firstName, lastName, email, projectIds?}.
  // projectIds fan out as best-effort project invites server-side.
  const createUser = useMutation({
    mutationFn: () =>
      apiFetch(LIST_PATH, {
        method: "POST",
        body: {
          email: form.email,
          firstName: form.firstName,
          lastName: form.lastName,
          ...(projectIds.length ? { projectIds } : {}),
        },
      }),
    onSuccess: () => {
      toast.success("User created")
      setCreateOpen(false)
      setForm({ email: "", firstName: "", lastName: "" })
      setProjectIds([])
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // DELETE /admin/user/{id} (user.go userDelete).
  const deleteUser = useMutation({
    mutationFn: (id: string) => apiFetch(`/admin/user/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("User deleted")
      setToDelete(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<AdminUser, any>[]>(
    () => [
      {
        id: "email",
        accessorFn: (u) => u.email ?? "",
        header: sortableHeader("Email"),
        cell: ({ row }) => {
          const u = row.original
          return u.id ? (
            <Link
              to={`/clients/users/${u.id}`}
              className="inline-block py-1 font-medium hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              {u.email ?? "—"}
            </Link>
          ) : (
            <span className="font-medium">{u.email ?? "—"}</span>
          )
        },
      },
      {
        id: "name",
        accessorFn: (u) => userDisplayName(u),
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="text-sm">{getValue()}</span>,
      },
      {
        id: "sub",
        accessorFn: (u) => u.sub ?? "",
        header: "Sub",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
      {
        id: "created",
        accessorFn: (u) => u.createdAt ?? "",
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
          const u = row.original
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${u.email ?? u.sub}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem variant="destructive" onClick={() => setToDelete(u)}>
                    <Trash2 className="size-4" /> Delete user
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
        title="Users"
        eyebrow="Clients"
        description="Every registered client account on the platform."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => refetch()} disabled={isFetching} aria-label="Refresh">
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create user
            </Button>
          </>
        }
      />

      {!isLoading && !error && total === 0 ? (
        <EmptyState
          icon={Users}
          title="No users yet"
          hint="Create the first user to get started."
          action={
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create user
            </Button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={users}
          isLoading={isLoading}
          error={error as Error | null}
          pageSize={PAGE_SIZE}
          onRowClick={(u) => u.id && navigate(`/clients/users/${u.id}`)}
          getRowId={(u) => u.id ?? u.sub ?? u.email ?? ""}
          server={{ pageIndex, pageSize: PAGE_SIZE, total, onPageChange: setPageIndex }}
        />
      )}

      {/* Create user */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Create user</DialogTitle>
            <DialogDescription>The user is created without a password; set one from their detail page.</DialogDescription>
          </DialogHeader>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault()
              createUser.mutate()
            }}
          >
            <div className="grid gap-2">
              <Label htmlFor="user-email">Email</Label>
              <Input
                id="user-email"
                type="email"
                autoComplete="off"
                required
                value={form.email}
                onChange={(e) => setForm({ ...form, email: e.target.value })}
              />
            </div>
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="grid gap-2">
                <Label htmlFor="user-first">First name</Label>
                <Input
                  id="user-first"
                  autoComplete="off"
                  value={form.firstName}
                  onChange={(e) => setForm({ ...form, firstName: e.target.value })}
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="user-last">Last name</Label>
                <Input
                  id="user-last"
                  autoComplete="off"
                  value={form.lastName}
                  onChange={(e) => setForm({ ...form, lastName: e.target.value })}
                />
              </div>
            </div>
            <div className="grid gap-2">
              <Label>Invite to projects (optional)</Label>
              {(projects.data?.data ?? []).length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  {projects.isLoading ? "Loading projects…" : "No projects available."}
                </p>
              ) : (
                <div className="max-h-36 space-y-1.5 overflow-y-auto rounded-md border p-2">
                  {(projects.data?.data ?? []).map((p) =>
                    p.id ? (
                      <label key={p.id} className="flex cursor-pointer items-center gap-2 text-sm">
                        <Checkbox
                          checked={projectIds.includes(p.id)}
                          onCheckedChange={(c) =>
                            setProjectIds((prev) =>
                              c === true ? [...prev, p.id!] : prev.filter((x) => x !== p.id),
                            )
                          }
                        />
                        <span>{p.name ?? p.id}</span>
                      </label>
                    ) : null,
                  )}
                </div>
              )}
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setCreateOpen(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={createUser.isPending || !form.email}>
                {createUser.isPending ? "Creating…" : "Create user"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Delete confirm */}
      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete user</DialogTitle>
            <DialogDescription>
              This permanently deletes {toDelete?.email ?? "this user"}. Users still attached to projects cannot be
              deleted.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={deleteUser.isPending}
              onClick={() => toDelete?.id && deleteUser.mutate(toDelete.id)}
            >
              {deleteUser.isPending ? "Deleting…" : "Delete user"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
