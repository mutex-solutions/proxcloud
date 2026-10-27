import { useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Cloud, Copy, RefreshCw, Trash2 } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { useAdminGet } from "@/lib/hooks"
import { fmtDateTime } from "@/lib/format"
import {
  resourceCreatedAt, resourceName, resourceStatus, type CloudResourceRow,
} from "./CloudResourcesPage"

// GET /admin/cloud-resource/{id} — the typed CloudResource doc, or {} when absent (httpx.Empty).
// GET /admin/cloud-resource/{id}/sync — live re-fetch + cache upsert, returns the refreshed doc
//   (cloudresourcemut.go registers sync as GET).
// DELETE /admin/cloud-resource/{id} — deletes the REAL OpenStack resource + archives the cache → 202.
export default function CloudResourceDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const path = `/admin/cloud-resource/${id}`
  const { data: res, isLoading, isError, error } = useAdminGet<CloudResourceRow>(path)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const sync = useMutation({
    mutationFn: () => apiFetch<CloudResourceRow>(`${path}/sync`),
    onSuccess: () => {
      toast.success("Resource synced from the cloud")
      void qc.invalidateQueries({ queryKey: ["admin-get", path] })
      void qc.invalidateQueries({ queryKey: ["admin-list", "/admin/cloud-resource"] })
    },
    // Surface the exact API message (404, or the 501 seam for unsynced types).
    onError: (e: Error) => toast.error(e.message),
  })

  const del = useMutation({
    mutationFn: () => apiFetch(path, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Delete accepted — the cloud resource is being removed")
      void qc.invalidateQueries({ queryKey: ["admin-list", "/admin/cloud-resource"] })
      navigate("/clients/cloud-resources")
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const crumbs = (
    <Breadcrumb>
      <BreadcrumbList>
        <BreadcrumbItem>
          <BreadcrumbLink asChild>
            <Link to="/clients/cloud-resources">Cloud resources</Link>
          </BreadcrumbLink>
        </BreadcrumbItem>
        <BreadcrumbSeparator />
        <BreadcrumbItem>
          <BreadcrumbPage className="font-mono text-xs">{id}</BreadcrumbPage>
        </BreadcrumbItem>
      </BreadcrumbList>
    </Breadcrumb>
  )

  if (isLoading) {
    return (
      <>
        <PageHeader title="Cloud resource" eyebrow="Clients" breadcrumb={crumbs} description="Loading…" />
        <Skeleton className="h-64" />
      </>
    )
  }
  if (isError) {
    return (
      <>
        <PageHeader title="Cloud resource" eyebrow="Clients" breadcrumb={crumbs} description="Failed to load." />
        <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">{(error as Error).message}</div>
      </>
    )
  }
  // Absent resource → the API returns an empty {} envelope.
  if (!res?.id) {
    return (
      <>
        <PageHeader
          title="Cloud resource"
          eyebrow="Clients"
          breadcrumb={crumbs}
          description="This resource does not exist."
        />
        <EmptyState
          icon={Cloud}
          title="Resource not found"
          hint="It may have been deleted or archived."
          action={
            <Button variant="outline" asChild>
              <Link to="/clients/cloud-resources">Back to cloud resources</Link>
            </Button>
          }
        />
      </>
    )
  }

  const name = resourceName(res)
  const status = resourceStatus(res)
  const type = (res.type as string | undefined)?.toLowerCase().replace(/_/g, " ") ?? "resource"

  return (
    <>
      <PageHeader
        title={name}
        eyebrow="Clients"
        breadcrumb={crumbs}
        description={`Cloud ${type} — cached copy of the live OpenStack object.`}
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => sync.mutate()} disabled={sync.isPending}>
              <RefreshCw className={sync.isPending ? "size-4 animate-spin" : "size-4"} /> Sync
            </Button>
            <Button variant="destructive" size="sm" onClick={() => setConfirmDelete(true)} disabled={del.isPending}>
              <Trash2 className="size-4" /> Delete
            </Button>
          </>
        }
      />

      <div className="grid gap-4">
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Details</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="grid gap-x-8 gap-y-3 text-sm sm:grid-cols-2 lg:grid-cols-3">
              <div>
                <dt className="text-eyebrow mb-1">Status</dt>
                <dd><StatusBadge status={status} /></dd>
              </div>
              <div>
                <dt className="text-eyebrow mb-1">Type</dt>
                <dd className="capitalize">{type}</dd>
              </div>
              <div>
                <dt className="text-eyebrow mb-1">External ID</dt>
                <dd className="break-all font-mono text-xs">{res.externalId ?? "—"}</dd>
              </div>
              <div>
                <dt className="text-eyebrow mb-1">Project</dt>
                <dd>
                  {res.projectId ? (
                    <Link className="break-all font-mono text-xs hover:underline" to={`/clients/projects/${res.projectId}`}>
                      {res.projectId}
                    </Link>
                  ) : (
                    "—"
                  )}
                </dd>
              </div>
              <div>
                <dt className="text-eyebrow mb-1">Service</dt>
                <dd className="break-all font-mono text-xs">{res.serviceId ?? "—"}</dd>
              </div>
              <div>
                <dt className="text-eyebrow mb-1">Region</dt>
                <dd>{res.region ?? "—"}</dd>
              </div>
              <div>
                <dt className="text-eyebrow mb-1">Created</dt>
                <dd>{fmtDateTime(resourceCreatedAt(res))}</dd>
              </div>
              <div>
                <dt className="text-eyebrow mb-1">Updated</dt>
                <dd>{fmtDateTime(res.updatedAt)}</dd>
              </div>
            </dl>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex flex-row items-center justify-between">
            <CardTitle className="text-base">Raw data</CardTitle>
            {res.data && Object.keys(res.data).length ? (
              <Button
                variant="outline"
                size="sm"
                onClick={() => {
                  void navigator.clipboard?.writeText(JSON.stringify(res.data, null, 2))
                  toast.success("Raw JSON copied")
                }}
              >
                <Copy className="size-4" /> Copy JSON
              </Button>
            ) : null}
          </CardHeader>
          <CardContent>
            {res.data && Object.keys(res.data).length ? (
              <pre className="max-h-[32rem] overflow-auto rounded-md bg-muted/40 p-4 font-mono text-xs leading-relaxed">
                {JSON.stringify(res.data, null, 2)}
              </pre>
            ) : (
              <p className="text-sm text-muted-foreground">No cached data for this resource.</p>
            )}
          </CardContent>
        </Card>
      </div>

      <Dialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete cloud resource</DialogTitle>
            <DialogDescription>
              This deletes the <span className="font-medium">real {type}</span> "{name}" on the cloud provider,
              not just the cached record. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmDelete(false)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={del.isPending}
              onClick={() => {
                del.mutate()
                setConfirmDelete(false)
              }}
            >
              <Trash2 className="size-4" /> {del.isPending ? "Deleting…" : "Delete resource"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
