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
import {
  useCloudCursorList, useCloudFeatures, useCloudList, useCloudScope, useProjectId, useProjectQuota,
} from "@/lib/hooks"
import { volumeCreateQuotaViolations } from "@/lib/quota"
import type { CloudResource } from "@/lib/types"

function vol(r: CloudResource): Record<string, any> {
  return (r.data?.volume as Record<string, any>) ?? {}
}
// Attachments live either at data.attachments (Go cache shape: {serverId, device, attachmentId})
// or data.volume.attachments (live shape: {server_id, device}). A Proxmox disk instead carries its
// VM in volume.vmid (= the server externalId) and is attached while its status is in-use.
function attachments(r: CloudResource): Array<{ serverId: string; device?: string }> {
  const v = vol(r)
  if (v.vmid != null && v.status === "in-use") return [{ serverId: String(v.vmid), device: v.slot as string }]
  const cache = (r.data?.attachments as Array<Record<string, any>>) ?? []
  const live = (v.attachments as Array<Record<string, any>>) ?? []
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
  const canRetype = useCloudFeatures(pid).has("volumeRetype")

  const { data: servers } = useCloudList(pid, "SERVER")

  const [createOpen, setCreateOpen] = useState(false)
  // serverId = the server externalId: a Proxmox disk is created on (and belongs to) a server.
  const [form, setForm] = useState({ size: "10", type: "", serverId: "" })
  const [extendTarget, setExtendTarget] = useState<CloudResource | null>(null)
  const [extendSize, setExtendSize] = useState("")
  const [deleteTarget, setDeleteTarget] = useState<CloudResource | null>(null)
  const [attachTarget, setAttachTarget] = useState<CloudResource | null>(null)
  const [attachServer, setAttachServer] = useState("")
  const [detachTarget, setDetachTarget] = useState<CloudResource | null>(null)
  const [retypeTarget, setRetypeTarget] = useState<CloudResource | null>(null)
  const [newType, setNewType] = useState("")

  const requestedSize = Number(form.size)
  // Aggregate and per-type volume quota checks use the selected admin-published type.
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
  // The servers this detached volume can attach to: its own and every server on its node
  // (Proxmox moves a disk between servers on the same node only).
  const attachTargets = useQuery({
    queryKey: ["volume-attach-targets", pid, attachTarget?.id],
    queryFn: async () => {
      const res = await apiFetch<{ result?: Array<{ externalId: string; name?: string; node?: string }> }>(
        `/project/${pid}/cloud/${attachTarget?.id}/action`,
        { method: "POST", cloud: scope, body: { action: "LIST_ATTACH_TARGETS" } },
      )
      return res.result ?? []
    },
    enabled: !!scope && !!attachTarget,
  })
  const soleVolumeType = volumeTypes.data?.length === 1 ? volumeTypes.data[0] : undefined
  // A capacity-class profile is HDD bulk storage: no boot disks, databases or snapshots (API-enforced).
  const isCapacityType = (name: string) =>
    (volumeTypes.data ?? []).some((t) => String(t.name) === name && t.extra_specs?.class === "capacity")
  const typeLabel = (t: Record<string, any>) =>
    String(t.displayName ?? t.name ?? t.id) + (t.extra_specs?.class === "capacity" ? " — HDD, not for databases" : "")
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
    // Go VOLUME ATTACH reads data.serverId as the server's externalId (NOT the cache id — no
    // resolveExtID).
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
        body: { action: "RETYPE", data: { newType: selectedRetypeVolumeType } },
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
            size: Number(form.size),
            type: selectedVolumeType,
            ...(form.serverId ? { serverId: form.serverId } : {}),
          },
        },
      })
    },
    onSuccess: () => {
      toast.success("Volume created")
      setCreateOpen(false)
      setForm({ size: "10", type: "", serverId: "" })
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
                  {canRetype && (
                    <DropdownMenuItem
                      onClick={() => {
                        setNewType("")
                        setRetypeTarget(r)
                      }}
                    >
                      Change type
                    </DropdownMenuItem>
                  )}
                  {/* The API refuses to delete an attached volume: detach it first. */}
                  {attachedCount(r) === 0 && (
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
    [servers, canRetype],
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
            <DialogDescription>
              A new disk, on one of your servers or standalone. It stays when its server is deleted, and can then be
              attached to another server on the same host.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
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
                  <span>{typeLabel(soleVolumeType)}</span>
                </div>
              ) : (
                <Select value={selectedCreateVolumeType} onValueChange={(type) => setForm({ ...form, type })}>
                  <SelectTrigger className="w-full" aria-label="Storage type">
                    <SelectValue placeholder="Select a storage type" />
                  </SelectTrigger>
                  <SelectContent>
                    {volumeTypes.data.map((type) => (
                      <SelectItem key={String(type.name)} value={String(type.name)}>
                        {typeLabel(type)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              {isCapacityType(selectedCreateVolumeType) ? <CapacityWarning /> : null}
            </div>
            <div className="grid gap-2">
              <Label>Server</Label>
              {
                <Select value={form.serverId || "none"} onValueChange={(v) => setForm({ ...form, serverId: v === "none" ? "" : v })}>
                  <SelectTrigger className="w-full" aria-label="Server">
                    <SelectValue placeholder="Select a server" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="none">No server (standalone)</SelectItem>
                    {(servers ?? [])
                      .filter((s) => !!s.externalId)
                      .map((s) => (
                        <SelectItem key={s.id} value={s.externalId as string}>
                          {(s.data?.server?.name as string) || s.name || s.externalId}
                        </SelectItem>
                      ))}
                  </SelectContent>
                </Select>
              }
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
            {attachTargets.isLoading ? (
              <p className="text-sm text-muted-foreground">Loading servers…</p>
            ) : attachTargets.isError ? (
              <p className="text-sm text-destructive">{(attachTargets.error as Error).message}</p>
            ) : !attachTargets.data?.length ? (
              <p className="text-sm text-muted-foreground">
                No server can use this volume: a volume only moves between servers on the same node.
              </p>
            ) : (
              <Select value={attachServer} onValueChange={setAttachServer}>
                <SelectTrigger>
                  <SelectValue placeholder="Select a server" />
                </SelectTrigger>
                <SelectContent>
                  {attachTargets.data.map((t) => (
                    <SelectItem key={t.externalId} value={t.externalId}>
                      {t.name || t.externalId}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
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
              {retypeTarget ? vol(retypeTarget).volume_type || "—" : ""}. The volume's data is moved to the new
              storage.
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
                  <span>{typeLabel(soleVolumeType)}</span>
                </div>
              ) : (
                <Select value={selectedRetypeVolumeType} onValueChange={setNewType}>
                  <SelectTrigger>
                    <SelectValue placeholder={volumeTypes.isLoading ? "Loading types…" : "Select a type"} />
                  </SelectTrigger>
                  <SelectContent>
                    {volumeTypes.data
                      .filter((t) => !(retypeTarget && vol(retypeTarget).bootable === true && t.extra_specs?.bootDisk === false))
                      .map((t) => (
                        <SelectItem key={String(t.id ?? t.name)} value={String(t.name ?? t.id)}>
                          {typeLabel(t)}
                        </SelectItem>
                      ))}
                  </SelectContent>
                </Select>
              )}
              {retypeUnchanged ? (
                <p className="text-xs text-muted-foreground">Choose a different enabled type to retype this volume.</p>
              ) : null}
              {isCapacityType(selectedRetypeVolumeType) ? <CapacityWarning /> : null}
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

// CapacityWarning is the honest label for a capacity-class (HDD NAS) storage type.
function CapacityWarning() {
  return (
    <Alert>
      <CircleAlert />
      <AlertTitle>HDD — not for databases</AlertTitle>
      <AlertDescription>
        Bulk data only: no boot disks, no databases, no snapshots (protect it with backups). The volume migrates with
        its server and survives a host failure, but the NAS is a single point of failure.
      </AlertDescription>
    </Alert>
  )
}
