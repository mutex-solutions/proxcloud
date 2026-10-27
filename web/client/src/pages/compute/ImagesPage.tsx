// Images: "My images" = the project's own glance images (owner-filtered cache list,
// POST /project/{pid}/resource?type=IMAGE → CloudResource with data.image); "Public images" =
// the PUBLIC_IMAGES bulk action (flat glance image maps). Own images can be deleted.
// Upload: Go has NO plain IMAGE create (providers/write.go TypeImage = server snapshot only), so
// there is no "create then upload" flow — instead a per-row "Upload data" appears on images still
// in glance "queued" status and streams the raw file to POST /project/{pid}/image/{imageId}/upload.
import { useMemo, useRef, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { HardDrive, MoreHorizontal, RefreshCw, Trash2, Upload } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { LoadMore } from "@/components/load-more"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudCursorList, useCloudScope, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

const gb = (bytes?: number) => (bytes ? (bytes / 1073741824).toFixed(2) : "0.00")

function imageName(r: CloudResource): string {
  return (r.data?.image?.name as string) ?? r.name ?? r.id
}

// Public-catalog rows are flat glance image maps — no mutations, so the
// columns can live at module scope (referentially stable by construction).
const publicColumns: ColumnDef<Record<string, any>, any>[] = [
  {
    id: "name",
    accessorFn: (im) => String(im.name ?? im.id),
    header: sortableHeader("Name"),
    cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
  },
  {
    id: "os",
    accessorFn: (im) => [im.os_distro, im.os_version].filter(Boolean).join(" "),
    header: sortableHeader("OS"),
    cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
  },
  {
    id: "size",
    accessorFn: (im) => (im.size as number) ?? 0,
    header: sortableHeader("Size"),
    cell: ({ getValue }) => (
      <span className="text-sm tabular-nums text-muted-foreground">
        {getValue() ? `${gb(getValue())} GB` : "—"}
      </span>
    ),
  },
  {
    id: "status",
    accessorFn: (im) => (im.status as string) ?? "",
    header: sortableHeader("Status"),
    cell: ({ getValue }) => <StatusBadge status={getValue()} />,
  },
]

export default function ImagesPage() {
  const pid = useProjectId()
  const scope = useCloudScope(pid)
  const qc = useQueryClient()

  const {
    rows: mineData, isLoading: mineLoading, refetch: mineRefetch, isFetching: mineFetching,
    error: mineError, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCloudCursorList(pid, "IMAGE")
  const pub = useQuery({
    queryKey: ["bulk-action", pid, "PUBLIC_IMAGES", scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<{ result?: Record<string, any>[] }>(`/project/${pid}/cloud/action`, {
        method: "POST",
        body: { action: "PUBLIC_IMAGES" },
        cloud: scope,
      }),
    enabled: !!pid && !!scope,
    select: (d) => d?.result ?? [],
  })

  const [toDelete, setToDelete] = useState<CloudResource | null>(null)
  const uploadTarget = useRef<CloudResource | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)

  const upload = useMutation({
    // Raw body to the glance image (10GB guard server-side). imageId = the glance externalId.
    mutationFn: ({ r, file }: { r: CloudResource; file: File }) =>
      apiFetch(`/project/${pid}/image/${r.externalId ?? (r.data?.image?.id as string)}/upload`, {
        method: "POST",
        cloud: scope,
        rawBody: file,
        headers: { "Content-Type": "application/octet-stream" },
      }),
    onSuccess: (_d, { file }) => {
      toast.success(`Uploaded ${file.name}`)
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "IMAGE"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const pickUpload = (r: CloudResource) => {
    uploadTarget.current = r
    fileInput.current?.click()
  }

  const del = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}`, { method: "DELETE", cloud: scope }),
    onSuccess: (_d, r) => {
      toast.success(`Image "${imageName(r)}" deleted`)
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "IMAGE"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const uploadPending = upload.isPending
  const mineColumns = useMemo<ColumnDef<CloudResource, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => imageName(r),
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "status",
        accessorFn: (r) => (r.data?.image?.status as string) ?? r.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "size",
        accessorFn: (r) => (r.data?.image?.size as number) ?? 0,
        header: sortableHeader("Size"),
        cell: ({ getValue }) => (
          <span className="text-sm tabular-nums text-muted-foreground">
            {getValue() ? `${gb(getValue())} GB` : "—"}
          </span>
        ),
      },
      {
        id: "visibility",
        accessorFn: (r) => (r.data?.image?.visibility as string) ?? "",
        header: "Visibility",
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "created",
        accessorFn: (r) => r.info?.createdAt ?? (r.data?.image?.created_at as string) ?? r.createdAt ?? "",
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
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${imageName(r)}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  {(r.data?.image?.status as string) === "queued" && (
                    <DropdownMenuItem onClick={() => pickUpload(r)} disabled={uploadPending}>
                      <Upload className="size-4" /> Upload data
                    </DropdownMenuItem>
                  )}
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
    // pickUpload closes over refs only (stable); setToDelete is a stable setter.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [uploadPending],
  )

  return (
    <>
      <PageHeader
        title="Images"
        eyebrow="Compute"
        description="Your project's images and snapshots, plus the public OS catalog."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              void mineRefetch()
              void pub.refetch()
            }}
            disabled={mineFetching || pub.isFetching}
            aria-label="Refresh"
          >
            <RefreshCw className={mineFetching || pub.isFetching ? "size-4 animate-spin" : "size-4"} />
          </Button>
        }
      />

      <Tabs defaultValue="mine">
        <TabsList>
          <TabsTrigger value="mine">My images</TabsTrigger>
          <TabsTrigger value="public">Public images</TabsTrigger>
        </TabsList>

        <TabsContent value="mine" className="mt-4">
          {!mineLoading && !mineError && !mineData.length ? (
            <EmptyState
              icon={HardDrive}
              title="No images yet"
              hint="Server snapshots and images you upload will show up here."
            />
          ) : (
            <>
              <DataTable
                columns={mineColumns}
                data={mineData}
                isLoading={mineLoading}
                error={mineError as Error | null}
                pagination={false}
              />
              <LoadMore
                hasNextPage={hasNextPage}
                isFetching={isFetchingNextPage}
                onClick={() => void fetchNextPage()}
                count={mineData.length}
                noun="image"
              />
            </>
          )}
        </TabsContent>

        <TabsContent value="public" className="mt-4">
          {!pub.isLoading && !pub.error && !pub.data?.length ? (
            <EmptyState icon={HardDrive} title="No public images" hint="The region's public OS catalog is empty." />
          ) : (
            <DataTable
              columns={publicColumns}
              data={pub.data}
              isLoading={pub.isLoading}
              error={pub.error as Error | null}
              searchPlaceholder="Search images…"
              getRowId={(im) => String(im.id)}
            />
          )}
        </TabsContent>
      </Tabs>

      <input
        ref={fileInput}
        type="file"
        className="hidden"
        onChange={(e) => {
          const f = e.target.files?.[0]
          const r = uploadTarget.current
          e.target.value = ""
          if (!f || !r) return
          if (f.size > 10 * 1024 ** 3) {
            toast.error("Image is larger than 10 GB")
            return
          }
          upload.mutate({ r, file: f })
        }}
      />

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete image</DialogTitle>
            <DialogDescription>
              Delete image “{toDelete ? imageName(toDelete) : ""}”? This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => {
                if (toDelete) del.mutate(toDelete)
                setToDelete(null)
              }}
            >
              <Trash2 className="size-4" /> Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
