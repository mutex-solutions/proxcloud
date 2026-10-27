import { useMemo, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { MoreHorizontal, Plus, RefreshCw, Shield, Trash2 } from "lucide-react"
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
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudCursorList, useCloudScope, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

function sgName(r: CloudResource): string {
  return (r.data?.securityGroup?.name as string) ?? r.name ?? r.id
}
function sgDescription(r: CloudResource): string {
  return (r.data?.securityGroup?.description as string) ?? ""
}
function sgRuleCount(r: CloudResource): number | undefined {
  const rules = r.data?.securityGroup?.security_group_rules as unknown[] | undefined
  return rules?.length
}

export default function SecurityGroupsPage() {
  const pid = useProjectId()
  const scope = useCloudScope(pid)
  const navigate = useNavigate()
  const qc = useQueryClient()
  const {
    rows: data, isLoading, refetch, isFetching, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCloudCursorList(pid, "SECURITY_GROUP")

  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState("")
  const [description, setDescription] = useState("")
  const [toDelete, setToDelete] = useState<CloudResource | null>(null)

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["cloud", pid, "SECURITY_GROUP"] })

  const create = useMutation({
    mutationFn: () =>
      apiFetch(`/project/${pid}/cloud`, {
        method: "POST",
        body: { type: "SECURITY_GROUP", data: { name, description } },
        cloud: scope,
      }),
    onSuccess: () => {
      toast.success(`Security group "${name}" created`)
      setCreateOpen(false)
      setName("")
      setDescription("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const del = useMutation({
    mutationFn: (id: string) =>
      apiFetch(`/project/${pid}/cloud/${id}`, { method: "DELETE", cloud: scope }),
    onSuccess: () => {
      toast.success("Security group deletion requested")
      setToDelete(null)
      setTimeout(invalidate, 1500)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => sgName(r),
        header: sortableHeader("Name"),
        cell: ({ row, getValue }) => (
          <Link
            className="inline-block py-1 font-medium hover:underline"
            to={`/p/${pid}/security-groups/${row.original.id}`}
            onClick={(e) => e.stopPropagation()}
          >
            {getValue()}
          </Link>
        ),
      },
      {
        id: "description",
        accessorFn: (r) => sgDescription(r),
        header: "Description",
        cell: ({ getValue }) => (
          <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
      {
        id: "rules",
        accessorFn: (r) => sgRuleCount(r) ?? -1,
        header: sortableHeader("Rules"),
        cell: ({ row }) => (
          <span className="text-sm tabular-nums">{sgRuleCount(row.original) ?? "—"}</span>
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
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${sgName(r)}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => navigate(`/p/${pid}/security-groups/${r.id}`)}>
                    Manage rules
                  </DropdownMenuItem>
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
    // pid is stable per mount; setters/navigate are stable.
    [pid, navigate],
  )

  return (
    <>
      <PageHeader
        title="Security groups"
        eyebrow="Network"
        description="Firewall rule sets applied to servers and ports."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh">
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create security group
            </Button>
          </>
        }
      />

      {!isLoading && !error && !data.length ? (
        <EmptyState
          icon={Shield}
          title="No security groups yet"
          hint="Create a security group and add ingress/egress rules to control traffic."
          action={
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create security group
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
            onRowClick={(r) => navigate(`/p/${pid}/security-groups/${r.id}`)}
          />
          <LoadMore
            hasNextPage={hasNextPage}
            isFetching={isFetchingNextPage}
            onClick={() => void fetchNextPage()}
            count={data.length}
            noun="security group"
          />
        </>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create security group</DialogTitle>
            <DialogDescription>Rules can be added from the group's detail page after creation.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-2">
            <div className="grid gap-2">
              <Label htmlFor="sg-name">Name</Label>
              <Input id="sg-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="web-servers" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="sg-desc">Description</Label>
              <Input
                id="sg-desc"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="Allow HTTP/HTTPS traffic"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => create.mutate()} disabled={!name.trim() || create.isPending}>
              {create.isPending ? "Creating…" : "Create security group"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete security group</DialogTitle>
            <DialogDescription>
              Delete "{toDelete ? sgName(toDelete) : ""}" and all of its rules? This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => toDelete && del.mutate(toDelete.id)}
              disabled={del.isPending}
            >
              {del.isPending ? "Deleting…" : "Delete security group"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
