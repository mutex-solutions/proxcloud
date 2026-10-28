import { useEffect, useMemo, useState } from "react"
import { useNavigate } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Database, FolderOpen, MoreHorizontal, Plus, RefreshCw, Settings } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { LoadMore } from "@/components/load-more"
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
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudCursorList, useProjectId } from "@/lib/hooks"
import { isS3Location, useBucketLocations } from "@/lib/objectstore"
import type { CloudResource, Location } from "@/lib/types"
import { BucketSettingsDialog } from "./BucketSettingsDialog"

export function bucketName(r: CloudResource): string {
  return (r.data?.bucketName as string) || r.externalId || r.name || r.id
}

// sizeInGb may arrive as a number or a decimal string — tolerate both (and a legacy {$numberDecimal} wrapper).
export function bucketGb(r: CloudResource): string {
  const v = r.data?.sizeInGb
  if (v == null) return "—"
  if (typeof v === "object") {
    const d = (v as Record<string, unknown>).$numberDecimal
    return d != null ? `${d} GB` : "—"
  }
  return `${v} GB`
}

export default function BucketsPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const navigate = useNavigate()
  const {
    rows: data, isLoading, refetch, isFetching, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCloudCursorList(pid, "BUCKET")
  // Each attached S3 service/region holds its own bucket set — the user picks which one to create in.
  const { locations } = useBucketLocations(pid)

  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState("")
  const [locKey, setLocKey] = useState("")
  const [objectLock, setObjectLock] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<CloudResource | null>(null)
  const [settingsTarget, setSettingsTarget] = useState<CloudResource | null>(null)

  // Key the store picker by serviceId+region, NOT the array index — useLocations returns the API array
  // with no stable order, so an index could point at a different backend between renders and create the
  // bucket on the wrong store.
  const locKeyOf = (l: Location) => `${l.serviceId ?? ""}::${l.region ?? ""}`
  const selectedLoc = locations.find((l) => locKeyOf(l) === locKey) ?? locations[0]
  const s3Selected = isS3Location(selectedLoc)
  const multipleStores = locations.length > 1

  useEffect(() => {
    if (locations.length && !locations.some((l) => locKeyOf(l) === locKey)) {
      setLocKey(locKeyOf(locations[0]))
    }
  }, [locations, locKey])

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["cloud", pid, "BUCKET"] })

  const create = useMutation({
    mutationFn: () => {
      // Target the CHOSEN store explicitly, not whichever location happens to be first. Fail fast on an
      // incomplete location — empty x-service-id/x-region-id headers could resolve the wrong store.
      if (!selectedLoc?.serviceId || !selectedLoc.region) {
        return Promise.reject(new Error("Select a storage location first"))
      }
      return apiFetch(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: { serviceId: selectedLoc.serviceId, region: selectedLoc.region },
        body: {
          type: "BUCKET",
          data: { bucketName: name, ...(s3Selected && objectLock ? { objectLockEnabled: true } : {}) },
        },
      })
    },
    onSuccess: () => {
      toast.success("Bucket created")
      setCreateOpen(false)
      setName("")
      setObjectLock(false)
      invalidate()
    },
    // 409 = the name is taken globally (S3 bucket names are not per-project).
    onError: (e: Error) => toast.error(e.message),
  })

  const del = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Bucket deletion requested")
      setDeleteTarget(null)
      invalidate()
    },
    // The store refuses to delete a non-empty bucket — surface the API error.
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => bucketName(r),
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "objects",
        accessorFn: (r) => (r.data?.objectCount as number) ?? 0,
        header: sortableHeader("Objects"),
        cell: ({ getValue }) => <span className="text-sm tabular-nums">{getValue()}</span>,
      },
      {
        id: "size",
        accessorFn: (r) => bucketGb(r),
        header: "Size",
        cell: ({ getValue }) => <span className="text-sm tabular-nums text-muted-foreground">{getValue()}</span>,
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
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${bucketName(r)}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => navigate(`/p/${pid}/object-storage/${r.id}`)}>
                    <FolderOpen className="size-4" /> Browse
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => setSettingsTarget(r)}>
                    <Settings className="size-4" /> Settings
                  </DropdownMenuItem>
                  <DropdownMenuItem variant="destructive" onClick={() => setDeleteTarget(r)}>
                    Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    [navigate, pid],
  )

  return (
    <>
      <PageHeader
        title="Object storage"
        eyebrow="Storage"
        description={
          multipleStores
            ? "Buckets for storing objects and files. Each S3 location holds its own buckets — a bucket lives in one of them."
            : "Buckets for storing objects and files."
        }
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh buckets">
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)} disabled={!locations.length}>
              <Plus className="size-4" /> Create bucket
            </Button>
          </>
        }
      />

      {!isLoading && !data.length ? (
        <EmptyState
          icon={Database}
          title="No buckets yet"
          hint="Create a bucket to store objects and files."
          action={
            <Button onClick={() => setCreateOpen(true)} disabled={!locations.length}>
              <Plus className="size-4" /> Create bucket
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
            onRowClick={(r) => navigate(`/p/${pid}/object-storage/${r.id}`)}
          />
          <LoadMore
            hasNextPage={hasNextPage}
            isFetching={isFetchingNextPage}
            onClick={() => void fetchNextPage()}
            count={data.length}
            noun="bucket"
          />
        </>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create bucket</DialogTitle>
            <DialogDescription>
              {s3Selected
                ? "S3 bucket names are globally unique — if a name is taken you will need another one."
                : "Bucket names must be unique within this project."}
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4">
            {multipleStores ? (
              <div className="grid gap-2">
                <Label>Storage</Label>
                <Select value={locKey} onValueChange={setLocKey}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {locations.map((l) => (
                      <SelectItem key={locKeyOf(l)} value={locKeyOf(l)}>
                        S3 (Ceph){l.serviceName ? ` — ${l.serviceName}` : ""} ({l.region})
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-xs text-muted-foreground">
                  Each location holds a separate set of buckets. A bucket cannot be moved between them.
                </p>
              </div>
            ) : null}

            <div className="grid gap-2">
              <Label htmlFor="bucket-name">Bucket name</Label>
              <Input id="bucket-name" value={name} onChange={(e) => setName(e.target.value)} />
            </div>

            {s3Selected ? (
              <div className="flex items-start gap-2">
                <Checkbox
                  id="object-lock"
                  checked={objectLock}
                  onCheckedChange={(v) => setObjectLock(v === true)}
                  className="mt-0.5"
                />
                <div className="grid gap-0.5">
                  <Label htmlFor="object-lock" className="font-normal">
                    Enable object lock
                  </Label>
                  <p className="text-xs text-muted-foreground">
                    Protects objects from deletion for a retention period, and turns on versioning. This can only be
                    chosen now — it cannot be enabled later.
                  </p>
                </div>
              </div>
            ) : null}
          </div>

          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => create.mutate()} disabled={!name.trim() || !selectedLoc || create.isPending}>
              {create.isPending ? "Creating…" : "Create bucket"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete bucket</DialogTitle>
            <DialogDescription>
              This deletes {deleteTarget ? bucketName(deleteTarget) : ""}. The bucket must be empty — delete its
              objects first.
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
              {del.isPending ? "Deleting…" : "Delete bucket"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {settingsTarget ? (
        <BucketSettingsDialog
          pid={pid}
          resourceId={settingsTarget.id}
          bucketName={bucketName(settingsTarget)}
          open={!!settingsTarget}
          onOpenChange={(o) => !o && setSettingsTarget(null)}
        />
      ) : null}
    </>
  )
}
