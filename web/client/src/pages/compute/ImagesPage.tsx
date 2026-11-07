// Images: "My images" = the project's own templates and server snapshots (cache list,
// POST /project/{pid}/resource?type=IMAGE → CloudResource with data.image); "Public images" =
// the project templates from the PUBLIC_IMAGES bulk action (flat image maps). Own images can be
// deleted. New images come from server snapshots, or from a URL a node downloads ("imageImport").
import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Download, HardDrive, MoreHorizontal, RefreshCw, Trash2 } from "lucide-react"
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
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudCursorList, useCloudFeatures, useCloudScope, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

const gb = (bytes?: number) => (bytes ? (bytes / 1073741824).toFixed(2) : "0.00")

function imageName(r: CloudResource): string {
  return (r.data?.image?.name as string) ?? r.name ?? r.id
}

// Public-catalog rows are flat template image maps — no mutations, so the
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

  const features = useCloudFeatures(pid)
  const [importing, setImporting] = useState(false)
  const [imp, setImp] = useState({ name: "", url: "", format: "qcow2", checksum: "" })
  const importImage = useMutation({
    mutationFn: () =>
      apiFetch(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: {
          type: "IMAGE",
          data: {
            name: imp.name.trim(),
            url: imp.url.trim(),
            format: imp.format,
            ...(imp.checksum.trim() ? { checksum: imp.checksum.trim(), checksumAlgorithm: "sha256" } : {}),
          },
        },
      }),
    onSuccess: () => {
      toast.success(`Image "${imp.name.trim()}" imported`)
      setImporting(false)
      setImp({ name: "", url: "", format: "qcow2", checksum: "" })
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "IMAGE"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const [toDelete, setToDelete] = useState<CloudResource | null>(null)
  const del = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}`, { method: "DELETE", cloud: scope }),
    onSuccess: (_d, r) => {
      toast.success(`Image "${imageName(r)}" deleted`)
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "IMAGE"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

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
    // setToDelete is a stable setter.
    [],
  )

  return (
    <>
      <PageHeader
        title="Images"
        eyebrow="Compute"
        description="Your project's images and snapshots, plus the project templates."
        actions={
          <>
            {features.has("imageImport") && (
              <Button size="sm" onClick={() => setImporting(true)}>
                <Download className="size-4" /> Import image
              </Button>
            )}
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
          </>
        }
      />

      <Dialog open={importing} onOpenChange={(o) => !importImage.isPending && setImporting(o)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Import image</DialogTitle>
            <DialogDescription>
              A public cloud image with cloud-init, in qcow2 or VMDK format. Downloading and importing can take minutes, and
              you can leave this page: the image appears when it is ready.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3">
            <div className="grid gap-2">
              <Label htmlFor="imp-name">Name</Label>
              <Input id="imp-name" value={imp.name} onChange={(e) => setImp({ ...imp, name: e.target.value })} placeholder="debian-13" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="imp-url">URL</Label>
              <Input id="imp-url" value={imp.url} onChange={(e) => setImp({ ...imp, url: e.target.value })} placeholder="https://example.com/image.qcow2" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="imp-format">Format</Label>
              <select
                id="imp-format"
                className="h-9 rounded-md border bg-transparent px-3 text-sm"
                value={imp.format}
                onChange={(e) => setImp({ ...imp, format: e.target.value })}
              >
                <option value="qcow2">qcow2</option>
                <option value="vmdk">VMDK</option>
              </select>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="imp-sum">SHA-256 checksum (optional)</Label>
              <Input id="imp-sum" value={imp.checksum} onChange={(e) => setImp({ ...imp, checksum: e.target.value })} />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" disabled={importImage.isPending} onClick={() => setImporting(false)}>
              Cancel
            </Button>
            <Button disabled={!imp.name.trim() || !imp.url.trim() || importImage.isPending} onClick={() => importImage.mutate()}>
              {importImage.isPending ? "Importing…" : "Import"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

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
              hint="Server snapshots will show up here."
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
