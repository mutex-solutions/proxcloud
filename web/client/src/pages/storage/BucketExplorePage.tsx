// DOCUMENTED ESCAPE HATCH from the DataTable list-page pattern: this is a
// folder-navigation browser (stateful prefix drill-down, per-row navigation),
// so it keeps the bare <Table> primitives per the data-table contract.
import { Fragment, useRef, useState } from "react"
import { Link, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import {
  Download, File, FileArchive, FileAudio, FileCode, FileImage, FileText, FileVideo,
  Folder, FolderPlus, RefreshCw, Trash2, Upload,
} from "lucide-react"
import type { LucideIcon } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { Badge } from "@/components/ui/badge"
import {
  Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { fmtDateTime } from "@/lib/format"
import { useCloudResource, useCloudScope, useProjectId } from "@/lib/hooks"

type BucketObject = {
  name: string
  displayName?: string
  sizeInBytes?: number
  mimeType?: string
  directory?: boolean
  lastModified?: string
}

function fmtBytes(n?: number): string {
  if (n == null || Number.isNaN(n)) return "—"
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`
  return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`
}

// File-type icon from the mime type (fall back to the extension for the
// common archive/code cases object stores report as octet-stream).
function fileIcon(o: BucketObject): LucideIcon {
  if (o.directory) return Folder
  const mime = (o.mimeType ?? "").toLowerCase()
  const name = (o.displayName || o.name).toLowerCase()
  if (mime.startsWith("image/")) return FileImage
  if (mime.startsWith("video/")) return FileVideo
  if (mime.startsWith("audio/")) return FileAudio
  if (/(zip|gzip|x-tar|x-7z|x-rar|x-bzip)/.test(mime) || /\.(zip|gz|tgz|tar|7z|rar|bz2)$/.test(name))
    return FileArchive
  if (
    /(json|xml|javascript|html|css|x-sh|x-python|yaml)/.test(mime) ||
    /\.(js|jsx|ts|tsx|py|go|rs|rb|sh|ya?ml|json|xml|html|css|sql|tf)$/.test(name)
  )
    return FileCode
  if (mime.startsWith("text/") || /\.(txt|md|csv|log)$/.test(name)) return FileText
  return File
}

const COLS = ["Name", "Size", "Type", "Last modified"] as const

export default function BucketExplorePage() {
  const pid = useProjectId()
  const { resourceId = "" } = useParams()
  const scope = useCloudScope(pid)
  const qc = useQueryClient()
  const { data: bucket } = useCloudResource(pid, resourceId)
  const [prefix, setPrefix] = useState("") // "" = root; folders carry a trailing "/"
  const [folderOpen, setFolderOpen] = useState(false)
  const [folderName, setFolderName] = useState("")
  const [deleteTarget, setDeleteTarget] = useState<BucketObject | null>(null)
  // Going public exposes every object to the internet — always confirm first.
  // Going back to private is safe and applies immediately.
  const [confirmPublic, setConfirmPublic] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)

  const bucketLabel = (bucket?.data?.bucketName as string) || bucket?.externalId || "Bucket"

  const action = (name: string, data?: Record<string, unknown>) =>
    apiFetch<{ result?: unknown }>(`/project/${pid}/cloud/${resourceId}/action`, {
      method: "POST",
      cloud: scope,
      body: { action: name, ...(data ? { data } : {}) },
    })

  const objects = useQuery({
    queryKey: ["bucket-objects", pid, resourceId, prefix],
    queryFn: async () => {
      const res = await action("LIST_OBJECTS", prefix ? { folderName: prefix } : undefined)
      return (res.result as BucketObject[]) ?? []
    },
    enabled: !!pid && !!resourceId && !!scope,
  })

  const visibility = useQuery({
    queryKey: ["bucket-public", pid, resourceId],
    queryFn: async () => {
      const res = await action("IS_BUCKET_PUBLIC")
      return res.result === true
    },
    enabled: !!pid && !!resourceId && !!scope,
  })

  const invalidateObjects = () => {
    void qc.invalidateQueries({ queryKey: ["bucket-objects", pid, resourceId] })
    void qc.invalidateQueries({ queryKey: ["cloud-resource", pid, resourceId] })
  }

  const setPublic = useMutation({
    mutationFn: (pub: boolean) => action(pub ? "MAKE_BUCKET_PUBLIC" : "MAKE_BUCKET_PRIVATE"),
    onSuccess: (_d, pub) => {
      toast.success(pub ? "Bucket is now public" : "Bucket is now private")
      void qc.invalidateQueries({ queryKey: ["bucket-public", pid, resourceId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const createFolder = useMutation({
    mutationFn: () => action("CREATE_FOLDER", { folderName: prefix + folderName.trim() }),
    onSuccess: () => {
      toast.success("Folder created")
      setFolderOpen(false)
      setFolderName("")
      invalidateObjects()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const upload = useMutation({
    mutationFn: (file: File) =>
      apiFetch(
        `/project/${pid}/cloud/${resourceId}/upload-bucket-file?objectName=${encodeURIComponent(prefix + file.name)}`,
        {
          method: "POST",
          cloud: scope,
          rawBody: file,
          headers: { "Content-Type": file.type || "application/octet-stream" },
        }
      ),
    onSuccess: (_d, file) => {
      toast.success(`Uploaded ${file.name}`)
      invalidateObjects()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // Go DOWNLOAD action (cloud_writes.go) mints a short-lived token and returns the FULL public
  // download URL in result.url (GET /api/v1/download/{token} is whitelisted — the token is the auth).
  const download = useMutation({
    mutationFn: async (obj: BucketObject) => {
      const res = await action("DOWNLOAD", { objectName: obj.name })
      return (res.result as { url?: string } | undefined)?.url
    },
    onSuccess: (url) => {
      if (url) window.open(url, "_blank", "noopener")
      else toast.error("No download URL returned")
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const deleteObject = useMutation({
    mutationFn: (obj: BucketObject) => action("DELETE_OBJECT", { objectName: obj.name }),
    onSuccess: () => {
      toast.success("Object deleted")
      setDeleteTarget(null)
      invalidateObjects()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // Folder-path segments: "a/b/" → ["a", "b"]
  const segments = prefix.split("/").filter(Boolean)

  return (
    <>
      <PageHeader
        title={bucketLabel}
        eyebrow="Storage"
        breadcrumb={
          <Breadcrumb>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink asChild>
                  <Link to={`/p/${pid}/object-storage`}>Object storage</Link>
                </BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{bucketLabel}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        description="Browse, upload and manage the objects in this bucket."
        actions={
          <>
            <div className="mr-2 flex items-center gap-2">
              {visibility.data !== undefined && (
                <Badge variant={visibility.data ? "outline" : "secondary"}>
                  {visibility.data ? "Public" : "Private"}
                </Badge>
              )}
              <Switch
                checked={visibility.data === true}
                disabled={visibility.isLoading || setPublic.isPending}
                onCheckedChange={(v) => (v ? setConfirmPublic(true) : setPublic.mutate(false))}
                aria-label="Toggle public access"
              />
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void objects.refetch()}
              disabled={objects.isFetching}
              aria-label="Refresh objects"
            >
              <RefreshCw className={objects.isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button variant="outline" size="sm" onClick={() => setFolderOpen(true)}>
              <FolderPlus className="size-4" /> New folder
            </Button>
            <Button size="sm" onClick={() => fileInput.current?.click()} disabled={upload.isPending}>
              <Upload className="size-4" /> {upload.isPending ? "Uploading…" : "Upload file"}
            </Button>
            <input
              ref={fileInput}
              type="file"
              className="hidden"
              onChange={(e) => {
                const f = e.target.files?.[0]
                if (f) upload.mutate(f)
                e.target.value = ""
              }}
            />
          </>
        }
      />

      {/* Folder path inside the bucket — click a crumb to jump back up. */}
      <Breadcrumb className="mb-3">
        <BreadcrumbList>
          <BreadcrumbItem>
            {segments.length ? (
              <BreadcrumbLink asChild>
                <button type="button" onClick={() => setPrefix("")} className="inline-flex items-center gap-1.5">
                  <Folder className="size-3.5" /> {bucketLabel}
                </button>
              </BreadcrumbLink>
            ) : (
              <BreadcrumbPage className="inline-flex items-center gap-1.5">
                <Folder className="size-3.5" /> {bucketLabel}
              </BreadcrumbPage>
            )}
          </BreadcrumbItem>
          {segments.map((seg, i) => (
            <Fragment key={segments.slice(0, i + 1).join("/")}>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                {i === segments.length - 1 ? (
                  <BreadcrumbPage>{seg}</BreadcrumbPage>
                ) : (
                  <BreadcrumbLink asChild>
                    <button type="button" onClick={() => setPrefix(segments.slice(0, i + 1).join("/") + "/")}>
                      {seg}
                    </button>
                  </BreadcrumbLink>
                )}
              </BreadcrumbItem>
            </Fragment>
          ))}
        </BreadcrumbList>
      </Breadcrumb>

      {objects.isError ? (
        <div className="rounded-xl border bg-card py-10 text-center text-sm text-destructive">
          {(objects.error as Error).message}
        </div>
      ) : !objects.isLoading && !objects.data?.length ? (
        <EmptyState
          icon={Folder}
          title={prefix ? "This folder is empty" : "This bucket is empty"}
          hint="Upload a file or create a folder to get started."
          action={
            <Button onClick={() => fileInput.current?.click()}>
              <Upload className="size-4" /> Upload file
            </Button>
          }
        />
      ) : (
        <div className="overflow-hidden rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                {COLS.map((c) => (
                  <TableHead key={c}>{c}</TableHead>
                ))}
                <TableHead className="w-24" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {objects.isLoading
                ? Array.from({ length: 5 }).map((_, i) => (
                    <TableRow key={`skeleton-${i}`} className="hover:bg-transparent">
                      {Array.from({ length: COLS.length + 1 }).map((_, j) => (
                        <TableCell key={j}>
                          <Skeleton className="h-4 w-full max-w-32" />
                        </TableCell>
                      ))}
                    </TableRow>
                  ))
                : (objects.data ?? []).map((o) => {
                    const Icon = fileIcon(o)
                    return (
                      <TableRow key={o.name}>
                        <TableCell className="font-medium">
                          {o.directory ? (
                            <button
                              className="flex items-center gap-2 hover:underline"
                              onClick={() => setPrefix(o.name)}
                            >
                              <Icon className="size-4 shrink-0 text-primary/80" /> {o.displayName || o.name}
                            </button>
                          ) : (
                            <span className="flex items-center gap-2">
                              <Icon className="size-4 shrink-0 text-muted-foreground" /> {o.displayName || o.name}
                            </span>
                          )}
                        </TableCell>
                        <TableCell className="text-sm tabular-nums">
                          {o.directory ? "—" : fmtBytes(o.sizeInBytes)}
                        </TableCell>
                        <TableCell className="text-sm text-muted-foreground">
                          {o.directory ? "Folder" : o.mimeType || "—"}
                        </TableCell>
                        <TableCell className="text-sm text-muted-foreground">
                          {o.lastModified ? fmtDateTime(o.lastModified) : "—"}
                        </TableCell>
                        <TableCell>
                          <div className="flex justify-end gap-1">
                            {!o.directory && (
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                onClick={() => download.mutate(o)}
                                disabled={download.isPending}
                                aria-label={`Download ${o.displayName || o.name}`}
                              >
                                <Download className="size-4" />
                              </Button>
                            )}
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              onClick={() => setDeleteTarget(o)}
                              aria-label={`Delete ${o.displayName || o.name}`}
                            >
                              <Trash2 className="size-4" />
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                    )
                  })}
            </TableBody>
          </Table>
        </div>
      )}

      <Dialog open={folderOpen} onOpenChange={setFolderOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>New folder</DialogTitle>
            <DialogDescription>
              Create a folder {prefix ? `inside ${prefix}` : "at the bucket root"}.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="folder-name">Folder name</Label>
            <Input id="folder-name" value={folderName} onChange={(e) => setFolderName(e.target.value)} />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setFolderOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => createFolder.mutate()} disabled={!folderName.trim() || createFolder.isPending}>
              {createFolder.isPending ? "Creating…" : "Create folder"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={confirmPublic} onOpenChange={setConfirmPublic}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Make bucket public</DialogTitle>
            <DialogDescription>
              Anyone on the internet will be able to read every object in {bucketLabel}. You can make it
              private again at any time.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmPublic(false)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => {
                setPublic.mutate(true)
                setConfirmPublic(false)
              }}
            >
              Make public
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {deleteTarget?.directory ? "folder" : "object"}</DialogTitle>
            <DialogDescription>
              This permanently deletes {deleteTarget?.displayName || deleteTarget?.name}
              {deleteTarget?.directory ? " and everything inside it" : ""}. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => deleteTarget && deleteObject.mutate(deleteTarget)}
              disabled={deleteObject.isPending}
            >
              {deleteObject.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
