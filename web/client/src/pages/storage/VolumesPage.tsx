import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { CircleAlert, HardDrive, MoreHorizontal, Plus, RefreshCw } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { LoadMore } from "@/components/load-more"
import { StatusBadge } from "@/components/status-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
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
import { useCloudCursorList, useCloudList, useCloudScope, useProjectId, useProjectQuota } from "@/lib/hooks"
import { volumeCreateQuotaViolations } from "@/lib/quota"
import type { CloudResource } from "@/lib/types"

function vol(r: CloudResource): Record<string, any> {
  return (r.data?.volume as Record<string, any>) ?? {}
}
// Attachments live either at data.attachments (Go cache shape: {serverId, device, attachmentId})
// or data.volume.attachments (live cinder shape: {server_id, device}).
function attachments(r: CloudResource): Array<{ serverId: string; device?: string }> {
  const cache = (r.data?.attachments as Array<Record<string, any>>) ?? []
  const live = (vol(r).attachments as Array<Record<string, any>>) ?? []
  const list = Array.isArray(cache) && cache.length ? cache : Array.isArray(live) ? live : []
  return list
    .map((a) => ({ serverId: (a.serverId as string) ?? (a.server_id as string) ?? "", device: a.device as string }))
    .filter((a) => a.serverId)
}
function attachedCount(r: CloudResource): number {
  return attachments(r).length
}
function isBootVolume(r: CloudResource): boolean {
  const value = vol(r).bootable ?? r.data?.bootable
  return value === true || (typeof value === "string" && value.toLowerCase() === "true")
}

export default function VolumesPage() {
  const pid = useProjectId()
  const scope = useCloudScope(pid)
  const qc = useQueryClient()
  const {
    rows: data, isLoading, refetch, isFetching, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCloudCursorList(pid, "VOLUME")
  const projectQuota = useProjectQuota(pid, scope)

  const { data: servers } = useCloudList(pid, "SERVER")

  const [createOpen, setCreateOpen] = useState(false)
  const [form, setForm] = useState({ name: "", size: "10", type: "", availabilityZone: "" })
  const [extendTarget, setExtendTarget] = useState<CloudResource | null>(null)
  const [extendSize, setExtendSize] = useState("")
  const [deleteTarget, setDeleteTarget] = useState<CloudResource | null>(null)
  const [attachTarget, setAttachTarget] = useState<CloudResource | null>(null)
  const [attachServer, setAttachServer] = useState("")
  const [detachTarget, setDetachTarget] = useState<CloudResource | null>(null)
  const [retypeTarget, setRetypeTarget] = useState<CloudResource | null>(null)
  const [newType, setNewType] = useState("")
  const [migrationPolicy, setMigrationPolicy] = useState<"never" | "on-demand">("never")

  const requestedSize = Number(form.size)
  // Aggregate and per-type Cinder quota checks use the selected admin-published type.
  const quotaCheckPending = createOpen && projectQuota.isLoading
  const storageQuotaUnavailable =
    createOpen && !projectQuota.isLoading && (!!projectQuota.error || !projectQuota.data?.storage)

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["cloud", pid, "VOLUME"] })

  const serverName = (extId: string) => {
    const s = (servers ?? []).find((x) => x.externalId === extId)
    return (s?.data?.server?.name as string) || s?.name || extId
  }

  // Volume types are the strict live/admin intersection returned by LIST_VOLUME_TYPES.
  const volumeTypes = useQuery({
    queryKey: ["volume-types", pid, scope?.serviceId, scope?.region],
    queryFn: async () => {
      const res = await apiFetch<{ result?: Array<Record<string, any>> }>(
        `/project/${pid}/cloud/action`,
        { method: "POST", cloud: scope, body: { action: "LIST_VOLUME_TYPES" } }
      )
      return res.result ?? []
    },
    enabled: !!scope && (createOpen || !!retypeTarget),
  })
  const soleVolumeType = volumeTypes.data?.length === 1 ? volumeTypes.data[0] : undefined
  const availableVolumeTypeNames = useMemo(
    () => new Set((volumeTypes.data ?? []).map((type) => String(type.name))),
    [volumeTypes.data],
  )
  const resolveVolumeType = (type: string) =>
    String(soleVolumeType?.name ?? "") || (availableVolumeTypeNames.has(type) ? type : "")
  const selectedCreateVolumeType = resolveVolumeType(form.type)
  const selectedRetypeVolumeType = resolveVolumeType(newType)
  const currentRetypeVolumeType = retypeTarget ? String(vol(retypeTarget).volume_type ?? "") : ""
  const retypeUnchanged = !!selectedRetypeVolumeType && selectedRetypeVolumeType === currentRetypeVolumeType
  const createQuotaViolations = volumeCreateQuotaViolations(
    projectQuota.data,
    requestedSize,
    selectedCreateVolumeType,
  )

  const attach = useMutation({
    // Go VOLUME ATTACH reads data.serverId as the nova server UUID (NOT the cache id — it is
    // passed straight to nova, no resolveExtID).
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}/action`, {
        method: "POST",
        cloud: scope,
        body: { action: "ATTACH", data: { serverId: attachServer } },
      }),
    onSuccess: () => {
      toast.success("Volume attached")
      setAttachTarget(null)
      setAttachServer("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const detach = useMutation({
    mutationFn: ({ r, serverId }: { r: CloudResource; serverId: string }) =>
      apiFetch(`/project/${pid}/cloud/${r.id}/action`, {
        method: "POST",
        cloud: scope,
        body: { action: "DETACH", data: { serverId } },
      }),
    onSuccess: () => {
      toast.success("Volume detached")
      setDetachTarget(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const retype = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}/action`, {
        method: "POST",
        cloud: scope,
        body: { action: "RETYPE", data: { newType: selectedRetypeVolumeType, migrationPolicy } },
      }),
    onSuccess: () => {
      toast.success("Retype requested")
      setRetypeTarget(null)
      setNewType("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const create = useMutation({
    mutationFn: async () => {
      if (!Number.isInteger(requestedSize) || requestedSize <= 0) {
        throw new Error("Volume size must be a positive whole number.")
      }
      const latestQuota = await projectQuota.refetch()
      const selectedVolumeType = selectedCreateVolumeType.trim()
      const latestViolations = volumeCreateQuotaViolations(latestQuota.data, requestedSize, selectedVolumeType)
      if (latestViolations.length > 0) {
        throw new Error(latestViolations.map((violation) => violation.message).join(" "))
      }
      return apiFetch(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: {
          type: "VOLUME",
          data: {
            name: form.name,
            size: Number(form.size),
            type: selectedVolumeType,
            ...(form.availabilityZone ? { availabilityZone: form.availabilityZone } : {}),
          },
        },
      })
    },
    onSuccess: () => {
      toast.success("Volume created")
      setCreateOpen(false)
      setForm({ name: "", size: "10", type: "", availabilityZone: "" })
      invalidate()
      void qc.invalidateQueries({ queryKey: ["project-quota", pid] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const extend = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}/action`, {
        method: "POST",
        cloud: scope,
        body: { action: "EXTEND", data: { size: Number(extendSize) } },
      }),
    onSuccess: () => {
      toast.success("Extend requested")
      setExtendTarget(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const del = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}`, { method: "DELETE", cloud: scope }),
    onSuccess: () => {
      toast.success("Volume deletion requested")
      setDeleteTarget(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => vol(r).name || r.name || r.id,
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "status",
        accessorFn: (r) => (vol(r).status as string) ?? r.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "size",
        accessorFn: (r) => (vol(r).size as number) ?? null,
        header: sortableHeader("Size"),
        cell: ({ getValue }) => (
          <span className="text-sm tabular-nums">{getValue() != null ? `${getValue()} GB` : "—"}</span>
        ),
      },
      {
        id: "type",
        accessorFn: (r) => (vol(r).volume_type as string) || "",
        header: "Type",
        cell: ({ getValue }) => (
          <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>
        ),
      },
      {
        id: "attached",
        accessorFn: (r) =>
          attachments(r)
            .map((a) => serverName(a.serverId))
            .join(", "),
        header: "Attached to",
        cell: ({ getValue }) => (
          <span className="block max-w-48 truncate text-sm text-muted-foreground" title={getValue() || undefined}>
            {getValue() || "—"}
          </span>
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
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label="Volume actions">
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  {attachedCount(r) === 0 && (
                    <DropdownMenuItem
                      onClick={() => {
                        setAttachServer("")
                        setAttachTarget(r)
                      }}
                    >
                      Attach to server
                    </DropdownMenuItem>
                  )}
                  {attachedCount(r) > 0 && !isBootVolume(r) && (
                    <DropdownMenuItem onClick={() => setDetachTarget(r)}>Detach</DropdownMenuItem>
                  )}
                  <DropdownMenuItem
                    onClick={() => {
                      setExtendSize(String((vol(r).size as number) ?? ""))
                      setExtendTarget(r)
                    }}
                  >
                    Extend
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    onClick={() => {
                      setNewType("")
                      setMigrationPolicy("never")
                      setRetypeTarget(r)
                    }}
                  >
                    Change type
                  </DropdownMenuItem>
                  {!(attachedCount(r) > 0 && isBootVolume(r)) && (
                    <DropdownMenuItem variant="destructive" onClick={() => setDeleteTarget(r)}>
                      Delete
                    </DropdownMenuItem>
                  )}
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // useState setters are stable; helpers are module-scope. serverName reads
    // the servers list, so the "Attached to" column re-derives when it loads.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [servers],
  )

  return (
    <>
      <PageHeader
        title="Volumes"
        eyebrow="Storage"
        description="Block-storage volumes in this project."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh">
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create volume
            </Button>
          </>
        }
      />

      {!isLoading && !data.length ? (
        <EmptyState
          icon={HardDrive}
          title="No volumes yet"
          hint="Create a block-storage volume to attach to your servers."
          action={
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create volume
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
            noun="volume"
          />
        </>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Create volume</DialogTitle>
            <DialogDescription>A new block-storage volume in this project's region.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label htmlFor="vol-name">Name</Label>
              <Input id="vol-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="vol-size">Size (GB)</Label>
              <Input
                id="vol-size"
                type="number"
                min={1}
                step={1}
                value={form.size}
                onChange={(e) => setForm({ ...form, size: e.target.value })}
              />
            </div>
            <div className="grid gap-2">
              <Label>Storage type</Label>
              {volumeTypes.isLoading ? (
                <p className="text-sm text-muted-foreground">Loading enabled storage types…</p>
              ) : volumeTypes.isError ? (
                <Alert variant="destructive">
                  <CircleAlert />
                  <AlertTitle>Storage types could not be loaded</AlertTitle>
                  <AlertDescription className="flex flex-wrap items-center gap-3">
                    The block-storage catalog is temporarily unavailable.
                    <Button type="button" variant="outline" size="sm" onClick={() => void volumeTypes.refetch()}>
                      Try again
                    </Button>
                  </AlertDescription>
                </Alert>
              ) : !volumeTypes.data?.length ? (
                <Alert variant="destructive">
                  <CircleAlert />
                  <AlertTitle>No storage type is enabled</AlertTitle>
                  <AlertDescription>An administrator must enable a volume type for this region.</AlertDescription>
                </Alert>
              ) : soleVolumeType ? (
                <div className="flex h-9 items-center gap-2 rounded-md border bg-muted/30 px-3 text-sm">
                  <HardDrive className="size-4 text-muted-foreground" />
                  <span>{String(soleVolumeType.displayName ?? soleVolumeType.name)}</span>
                </div>
              ) : (
                <Select value={selectedCreateVolumeType} onValueChange={(type) => setForm({ ...form, type })}>
                  <SelectTrigger className="w-full" aria-label="Storage type">
                    <SelectValue placeholder="Select a storage type" />
                  </SelectTrigger>
                  <SelectContent>
                    {volumeTypes.data.map((type) => (
                      <SelectItem key={String(type.name)} value={String(type.name)}>
                        {String(type.displayName ?? type.name)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            </div>
            <div className="grid gap-2">
              <Label htmlFor="vol-az">Availability zone (optional)</Label>
              <Input
                id="vol-az"
                value={form.availabilityZone}
                onChange={(e) => setForm({ ...form, availabilityZone: e.target.value })}
              />
            </div>
            {quotaCheckPending ? (
              <p className="text-sm text-muted-foreground">Checking the current storage quota…</p>
            ) : createQuotaViolations.length > 0 ? (
              <Alert variant="destructive">
                <CircleAlert />
                <AlertTitle>This volume exceeds the project quota</AlertTitle>
                <AlertDescription>
                  <ul className="list-disc space-y-1 pl-4">
                    {createQuotaViolations.map((violation) => (
                      <li key={`${violation.resource}-${violation.message}`}>{violation.message}</li>
                    ))}
                  </ul>
                </AlertDescription>
              </Alert>
            ) : storageQuotaUnavailable ? (
              <Alert>
                <CircleAlert />
                <AlertTitle>Live storage quota is unavailable</AlertTitle>
                <AlertDescription>
                  The API will still validate this request when you create the volume.
                </AlertDescription>
              </Alert>
            ) : projectQuota.data && requestedSize > 0 ? (
              <p className="text-sm text-muted-foreground">This volume fits the current quota snapshot.</p>
            ) : null}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button
              onClick={() => create.mutate()}
              disabled={
                !form.name ||
                !scope ||
                volumeTypes.isLoading ||
                volumeTypes.isError ||
                !selectedCreateVolumeType ||
                !Number.isFinite(requestedSize) ||
                !Number.isInteger(requestedSize) ||
                requestedSize <= 0 ||
                quotaCheckPending ||
                createQuotaViolations.length > 0 ||
                create.isPending
              }
            >
              {create.isPending ? "Creating…" : "Create volume"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!extendTarget} onOpenChange={(o) => !o && setExtendTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Extend volume</DialogTitle>
            <DialogDescription>
              New size for {extendTarget ? vol(extendTarget).name || extendTarget.id : ""} — must be larger than the
              current size.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="extend-size">New size (GB)</Label>
            <Input
              id="extend-size"
              type="number"
              min={1}
              value={extendSize}
              onChange={(e) => setExtendSize(e.target.value)}
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setExtendTarget(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => extendTarget && extend.mutate(extendTarget)}
              disabled={!Number(extendSize) || extend.isPending}
            >
              {extend.isPending ? "Extending…" : "Extend volume"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!attachTarget} onOpenChange={(o) => !o && setAttachTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Attach volume</DialogTitle>
            <DialogDescription>
              Attach {attachTarget ? vol(attachTarget).name || attachTarget.id : ""} to a server.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label>Server</Label>
            <Select value={attachServer} onValueChange={setAttachServer}>
              <SelectTrigger>
                <SelectValue placeholder="Select a server" />
              </SelectTrigger>
              <SelectContent>
                {(servers ?? [])
                  .filter((s) => !!s.externalId)
                  .map((s) => (
                    <SelectItem key={s.id} value={s.externalId as string}>
                      {(s.data?.server?.name as string) || s.name || s.externalId}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAttachTarget(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => attachTarget && attach.mutate(attachTarget)}
              disabled={!attachServer || attach.isPending}
            >
              {attach.isPending ? "Attaching…" : "Attach volume"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!detachTarget} onOpenChange={(o) => !o && setDetachTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Detach volume</DialogTitle>
            <DialogDescription>
              Detach {detachTarget ? vol(detachTarget).name || detachTarget.id : ""} from{" "}
              {detachTarget ? attachments(detachTarget).map((a) => serverName(a.serverId)).join(", ") : ""}? The
              server loses access to the volume's data.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDetachTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => {
                if (!detachTarget) return
                const first = attachments(detachTarget)[0]
                if (first) detach.mutate({ r: detachTarget, serverId: first.serverId })
              }}
              disabled={detach.isPending}
            >
              {detach.isPending ? "Detaching…" : "Detach volume"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!retypeTarget} onOpenChange={(o) => !o && setRetypeTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Change volume type</DialogTitle>
            <DialogDescription>
              Retype {retypeTarget ? vol(retypeTarget).name || retypeTarget.id : ""} — current type:{" "}
              {retypeTarget ? vol(retypeTarget).volume_type || "—" : ""}.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label>New type</Label>
              {volumeTypes.isLoading ? (
                <p className="text-sm text-muted-foreground">Loading enabled storage types…</p>
              ) : volumeTypes.isError ? (
                <Alert variant="destructive">
                  <CircleAlert />
                  <AlertTitle>Storage types could not be loaded</AlertTitle>
                  <AlertDescription className="flex flex-wrap items-center gap-3">
                    The block-storage catalog is temporarily unavailable.
                    <Button type="button" variant="outline" size="sm" onClick={() => void volumeTypes.refetch()}>
                      Try again
                    </Button>
                  </AlertDescription>
                </Alert>
              ) : !volumeTypes.data?.length ? (
                <Alert variant="destructive">
                  <CircleAlert />
                  <AlertTitle>No storage type is enabled</AlertTitle>
                  <AlertDescription>An administrator must enable a volume type for this region.</AlertDescription>
                </Alert>
              ) : soleVolumeType ? (
                <div className="flex h-9 items-center gap-2 rounded-md border bg-muted/30 px-3 text-sm">
                  <HardDrive className="size-4 text-muted-foreground" />
                  <span>{String(soleVolumeType.displayName ?? soleVolumeType.name)}</span>
                </div>
              ) : (
                <Select value={selectedRetypeVolumeType} onValueChange={setNewType}>
                  <SelectTrigger>
                    <SelectValue placeholder={volumeTypes.isLoading ? "Loading types…" : "Select a type"} />
                  </SelectTrigger>
                  <SelectContent>
                    {volumeTypes.data.map((t) => (
                      <SelectItem key={String(t.id ?? t.name)} value={String(t.name ?? t.id)}>
                        {String(t.displayName ?? t.name ?? t.id)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              {retypeUnchanged ? (
                <p className="text-xs text-muted-foreground">Choose a different enabled type to retype this volume.</p>
              ) : null}
            </div>
            <div className="grid gap-2">
              <Label>Migration policy</Label>
              <Select value={migrationPolicy} onValueChange={(v) => setMigrationPolicy(v as "never" | "on-demand")}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="never">never — fail if migration is required</SelectItem>
                  <SelectItem value="on-demand">on-demand — migrate the data if required</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRetypeTarget(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => retypeTarget && retype.mutate(retypeTarget)}
              disabled={
                !selectedRetypeVolumeType ||
                retypeUnchanged ||
                volumeTypes.isLoading ||
                volumeTypes.isError ||
                retype.isPending
              }
            >
              {retype.isPending ? "Requesting…" : "Change type"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete volume</DialogTitle>
            <DialogDescription>
              This permanently deletes {deleteTarget ? vol(deleteTarget).name || deleteTarget.id : ""} and its data.
              This cannot be undone.
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
              {del.isPending ? "Deleting…" : "Delete volume"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
