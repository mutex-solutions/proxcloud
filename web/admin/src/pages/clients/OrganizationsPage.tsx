import { useMemo, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { Building2, Plus, RefreshCw } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader, sortableRightHeader } from "@/components/data-table"
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
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { apiFetch } from "@/lib/api"
import { fmtDate } from "@/lib/format"
import { useAdminList } from "@/lib/hooks"

// GET /admin/organizations (organization.go orgToDto) — the org doc shaped (_id→id) +
// memberCount / projectCount / billingProfile?.
type Org = {
  id?: string
  name?: string
  description?: string
  billingProfileId?: string
  memberCount?: number
  projectCount?: number
  createdAt?: string
}

const LIST_PATH = "/admin/organizations"
const PAGE_SIZE = 50

export default function OrganizationsPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  // BE-paged (offset): GET /admin/organizations?limit=&offset= → { data:[page], paging:{total} }.
  const [pageIndex, setPageIndex] = useState(0)
  const listPath = `${LIST_PATH}?limit=${PAGE_SIZE}&offset=${pageIndex * PAGE_SIZE}`
  const { data, isLoading, isFetching, error, refetch } = useAdminList<Org>(listPath)
  const [createOpen, setCreateOpen] = useState(false)
  const [form, setForm] = useState({ name: "", description: "" })

  const orgs = data?.data ?? []
  const total = data?.paging?.total ?? 0

  // POST /admin/organizations (organization.go organizationCreate) — plain branch: {name, description}.
  const createOrg = useMutation({
    mutationFn: () =>
      apiFetch(LIST_PATH, {
        method: "POST",
        body: { name: form.name, ...(form.description ? { description: form.description } : {}) },
      }),
    onSuccess: () => {
      toast.success("Organization created")
      setCreateOpen(false)
      setForm({ name: "", description: "" })
      qc.invalidateQueries({ queryKey: ["admin-list"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<Org, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (o) => o.name ?? "",
        header: sortableHeader("Name"),
        cell: ({ row }) => {
          const o = row.original
          return (
            <div className="min-w-0">
              {o.id ? (
                <Link
                  to={`/clients/organizations/${o.id}`}
                  className="inline-block py-0.5 font-medium hover:underline"
                  onClick={(e) => e.stopPropagation()}
                >
                  {o.name ?? "—"}
                </Link>
              ) : (
                <span className="font-medium">{o.name ?? "—"}</span>
              )}
              {o.description ? (
                <p className="max-w-72 truncate text-xs text-muted-foreground" title={o.description}>
                  {o.description}
                </p>
              ) : null}
            </div>
          )
        },
      },
      {
        id: "id",
        accessorFn: (o) => o.id ?? "",
        header: "ID",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
      {
        id: "members",
        accessorFn: (o) => o.memberCount ?? 0,
        header: sortableRightHeader("Members"),
        cell: ({ getValue }) => (
          <div className={`text-right text-sm tabular-nums${getValue() ? "" : " text-muted-foreground"}`}>
            {getValue()}
          </div>
        ),
      },
      {
        id: "projects",
        accessorFn: (o) => o.projectCount ?? 0,
        header: sortableRightHeader("Projects"),
        cell: ({ getValue }) => (
          <div className={`text-right text-sm tabular-nums${getValue() ? "" : " text-muted-foreground"}`}>
            {getValue()}
          </div>
        ),
      },
      {
        id: "billingProfile",
        accessorFn: (o) => o.billingProfileId ?? "",
        header: "Billing profile",
        cell: ({ row }) => {
          const bp = row.original.billingProfileId
          return bp ? (
            <Link
              to={`/clients/billing-profiles/${bp}`}
              className="inline-block py-1 font-mono text-xs text-muted-foreground hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              {bp}
            </Link>
          ) : (
            <span className="font-mono text-xs text-muted-foreground">—</span>
          )
        },
      },
      {
        id: "created",
        accessorFn: (o) => o.createdAt ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDate(getValue())}</span>,
      },
    ],
    // useState setters are stable; helpers are module-scope.
    [],
  )

  return (
    <>
      <PageHeader
        title="Organizations"
        eyebrow="Clients"
        description="Client organizations and their memberships."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => refetch()} disabled={isFetching} aria-label="Refresh">
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create organization
            </Button>
          </>
        }
      />

      {!isLoading && !error && total === 0 ? (
        <EmptyState
          icon={Building2}
          title="No organizations yet"
          hint="Organizations appear when clients sign up."
          action={
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create organization
            </Button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={orgs}
          isLoading={isLoading}
          error={error as Error | null}
          pageSize={PAGE_SIZE}
          onRowClick={(o) => o.id && navigate(`/clients/organizations/${o.id}`)}
          getRowId={(o) => o.id ?? o.name ?? ""}
          server={{ pageIndex, pageSize: PAGE_SIZE, total, onPageChange: setPageIndex }}
        />
      )}

      {/* Create organization */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create organization</DialogTitle>
            <DialogDescription>
              Creates an empty organization. Add members and a billing profile from its detail page.
            </DialogDescription>
          </DialogHeader>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault()
              createOrg.mutate()
            }}
          >
            <div className="grid gap-2">
              <Label htmlFor="org-name">Name</Label>
              <Input
                id="org-name"
                autoComplete="off"
                required
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="org-desc">Description</Label>
              <Input
                id="org-desc"
                autoComplete="off"
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
              />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setCreateOpen(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={createOrg.isPending || !form.name}>
                {createOrg.isPending ? "Creating…" : "Create organization"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </>
  )
}
