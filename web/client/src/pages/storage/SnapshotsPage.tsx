import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Camera, Plus, RefreshCw, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { LoadMore } from "@/components/load-more"
import { StatusBadge } from "@/components/status-badge"
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
import { useCloudCursorList, useCloudList, useCloudScope, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

// The API stores the live snapshot under data.volumeSnapshot (data.snapshot is not used).
function snap(r: CloudResource): Record<string, any> {
  return (r.data?.volumeSnapshot as Record<string, any>) ?? (r.data?.snapshot as Record<string, any>) ?? {}
}

export default function SnapshotsPage() {
  const pid = useProjectId()
  const scope = useCloudScope(pid)
  const qc = useQueryClient()
  const {
    rows: data, isLoading, refetch, isFetching, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCloudCursorList(pid, "VOLUME_SNAPSHOT")
  const volumes = useCloudList(pid, "VOLUME")
  const volumesData = volumes.data

  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState("")
  const [volumeExtId, setVolumeExtId] = useState("")
  const [deleteTarget, setDeleteTarget] = useState<CloudResource | null>(null)

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["cloud", pid, "VOLUME_SNAPSHOT"] })

  const create = useMutation({
    mutationFn: () =>
      apiFetch(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: { type: "VOLUME_SNAPSHOT", data: { name, externalVolumeId: volumeExtId } },
      }),
    onSuccess: () => {
      toast.success("Snapshot created")
      setCreateOpen(false)
      setName("")
      setVolumeExtId("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const del = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}`, { method: "DELETE", cloud: scope }),
    onSuccess: () => {
      toast.success("Snapshot deletion requested")
      setDeleteTarget(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(() => {
    const volumeName = (extId?: string) => {
      if (!extId) return "—"
      const v = volumesData?.find((r) => r.externalId === extId)
      return (v?.data?.volume?.name as string) ?? v?.name ?? extId
    }
    return [
      {
        id: "name",
        accessorFn: (r) => snap(r).name || r.name || r.id,
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "status",
        accessorFn: (r) => (snap(r).status as string) ?? r.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "size",
        accessorFn: (r) => (snap(r).size as number) ?? null,
        header: sortableHeader("Size"),
        cell: ({ getValue }) => (
          <span className="text-sm tabular-nums">{getValue() != null ? `${getValue()} GB` : "—"}</span>
        ),
      },
      {
        id: "volume",
        accessorFn: (r) => volumeName(snap(r).volume_id as string),
        header: "Volume",
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue()}</span>,
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
        cell: ({ row }) => (
          <div className="text-right">
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={`Delete ${snap(row.original).name || row.original.id}`}
              onClick={() => setDeleteTarget(row.original)}
            >
              <Trash2 className="size-4" />
            </Button>
          </div>
        ),
      },
    ]
  }, [volumesData])

  return (
    <>
      <PageHeader
        title="Volume snapshots"
        eyebrow="Storage"
        description="Point-in-time copies of your block-storage volumes."
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void refetch()}
              disabled={isFetching}
              aria-label="Refresh snapshots"
            >
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create snapshot
            </Button>
          </>
        }
      />

      {!isLoading && !data.length ? (
        <EmptyState
          icon={Camera}
          title="No snapshots yet"
          hint="Snapshot a volume to capture its state at a point in time."
          action={
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create snapshot
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
            noun="snapshot"
          />
        </>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create snapshot</DialogTitle>
            <DialogDescription>Capture the current state of a volume.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label htmlFor="snap-name">Name</Label>
              <Input id="snap-name" value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label>Volume</Label>
              <Select value={volumeExtId} onValueChange={setVolumeExtId}>
                <SelectTrigger>
                  <SelectValue placeholder={volumes.data?.length ? "Select a volume" : "No volumes available"} />
                </SelectTrigger>
                <SelectContent>
                  {(volumes.data ?? [])
                    .filter((v) => !!v.externalId)
                    .map((v) => (
                      <SelectItem key={v.id} value={v.externalId as string}>
                        {(v.data?.volume?.name as string) ?? v.name ?? v.externalId}
                      </SelectItem>
                    ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => create.mutate()} disabled={!name || !volumeExtId || create.isPending}>
              {create.isPending ? "Creating…" : "Create snapshot"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete snapshot</DialogTitle>
            <DialogDescription>
              This permanently deletes {deleteTarget ? snap(deleteTarget).name || deleteTarget.id : ""}. This cannot
              be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => deleteTarget && del.mutate(deleteTarget)}
              disabled={del.isPending}
            >
              {del.isPending ? "Deleting…" : "Delete snapshot"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
