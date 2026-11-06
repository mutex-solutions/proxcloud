// Server detail — Overview / Console log plus Network / Security / Volumes /
// Events / Snapshots tabs. Action names + data keys verified against
// internal/platform/project/cloud_writes.go (cloudAction + clusterAction) and
// internal/cloud/providers/write.go (WriteService.Action TypeServer/TypeVolume):
//   RENAME{name} · RESIZE{flavorId} + CONFIRMRESIZE/REVERTRESIZE · REBUILD{imageId} ·
//   SET_PASSWORD{password} · RESCUE (result = generated password) / UNRESCUE ·
//   REMOTECONTROL (result = remote_console map with .url) ·
//   ADD/REMOVE_SECURITY_GROUP{name} · ATTACH_PORT/DETACH_PORT{portId} ·
//   volume ATTACH/DETACH act on the VOLUME resource with data{serverId: <server externalId>}.
import { useEffect, useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import {
  Camera, Globe, HardDrive, History, MoreVertical, Play, Plus, RotateCw, Square, Trash2, X,
} from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { StatusBadge } from "@/components/status-badge"
import {
  Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch, type CloudScope } from "@/lib/api"
import { fmtDateTime, timeAgo } from "@/lib/format"
import { useCloudFeatures, useCloudList, useCloudResource, useCloudScope, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"
import { ServerProtection } from "./ServerProtection"
import { ServerDeleteVolumes, serverFlavor, serverIPs, serverName, serverStatus } from "./ServersPage"

const ACTION_LABELS: Record<string, string> = {
  START: "Start", STOP: "Stop", SOFTREBOOT: "Reboot", RESIZE: "Resize", RENAME: "Rename", REBUILD: "Rebuild",
}

type PendingAction = { action: string; title: string; label: string; destructive?: boolean } | null
type FormDialog = "rename" | "resize" | "rebuild" | "password" | "delete" | null

type Flavor = {
  externalId?: string
  data?: { id?: string; name?: string; vcpus?: number; ram?: number; disk?: number }
}
type TemplateImage = Record<string, any>

// POST /project/{pid}/cloud/{rid}/action → envelope data = {result}.
function actionFetch<T = unknown>(
  pid: string,
  resourceId: string,
  scope: CloudScope | undefined,
  action: string,
  data?: Record<string, unknown>,
) {
  return apiFetch<{ result?: T }>(`/project/${pid}/cloud/${resourceId}/action`, {
    method: "POST",
    body: data ? { action, data } : { action },
    cloud: scope,
  })
}

export function ServerDetailPage() {
  const pid = useProjectId()
  const { resourceId = "" } = useParams()
  const navigate = useNavigate()
  const scope = useCloudScope(pid)
  // Rebuild / rescue / set password / ports / snapshots show only when the provider serves them.
  const features = useCloudFeatures(pid)
  const qc = useQueryClient()
  const { data: server, isLoading } = useCloudResource(pid, resourceId)
  const [pending, setPending] = useState<PendingAction>(null)
  const [dialog, setDialog] = useState<FormDialog>(null)
  const [renameVal, setRenameVal] = useState("")
  const [resizeFlavor, setResizeFlavor] = useState("")
  const [rebuildImage, setRebuildImage] = useState("")
  const [passwordVal, setPasswordVal] = useState("")
  const [passwordUser, setPasswordUser] = useState("")
  // Hide the confirm/revert-resize panel as soon as one is clicked, so a second click can't
  // fire a confirm at an already-confirmed server (which the API rejects with 409). The cached status can lag
  // the real state for a while (notification/sync delay), so relying on it alone leaves the
  // buttons live too long. Reset when the status finally moves off VERIFY_RESIZE, or on error.
  const [resizeActed, setResizeActed] = useState(false)
  // Re-arm the panel once the server leaves VERIFY_RESIZE (a later resize shows it again).
  // Kept above any early return so the hook order is stable.
  useEffect(() => {
    if (!server || serverStatus(server) !== "VERIFY_RESIZE") setResizeActed(false)
  }, [server])

  const invalidate = () => {
    setTimeout(() => {
      void qc.invalidateQueries({ queryKey: ["cloud-resource", pid, resourceId] })
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "SERVER"] })
    }, 1500)
  }

  const act = useMutation({
    mutationFn: (p: { action: string; data?: Record<string, unknown> }) =>
      actionFetch(pid, resourceId, scope, p.action, p.data),
    onSuccess: (d, p) => {
      if (p.action === "RESCUE" && typeof d?.result === "string" && d.result) {
        // RESCUE returns the generated admin password (cloud_writes.go RESCUE case).
        toast.success(`Rescue started — admin password: ${d.result}`, { duration: 30000 })
      } else if (p.action === "SET_PASSWORD") {
        toast.success(`Password set for user ${(d?.result as { username?: string } | undefined)?.username ?? "(default)"}`)
      } else {
        toast.success(`${ACTION_LABELS[p.action] ?? "Action"} requested`)
      }
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // Console opens OUR page (it renders noVNC against ProxCloud's proxied WebSocket and mints its own
  // token on mount) — the PVE console endpoint is never handed to the browser, so it stays
  // off the public internet. New tab: a console wants its own window.
  const openConsole = () =>
    window.open(`/p/${pid}/servers/${resourceId}/console`, "_blank", "noopener")

  const [deleteVolumes, setDeleteVolumes] = useState(false)
  const del = useMutation({
    mutationFn: () =>
      apiFetch(`/project/${pid}/cloud/${resourceId}${deleteVolumes ? "?deleteVolumes=true" : ""}`, { method: "DELETE", cloud: scope }),
    onSuccess: () => {
      toast.success("Server deletion requested")
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "SERVER"] })
      navigate(`/p/${pid}/servers`)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // Resize flavors + rebuild images load lazily when their dialog opens.
  const flavors = useQuery({
    queryKey: ["bulk-action", pid, "LIST_FLAVORS", scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<{ result?: Flavor[] }>(`/project/${pid}/cloud/action`, {
        method: "POST",
        body: { action: "LIST_FLAVORS" },
        cloud: scope,
      }),
    enabled: !!pid && !!scope && dialog === "resize",
    select: (d) => d?.result ?? [],
  })
  const images = useQuery({
    queryKey: ["bulk-action", pid, "PUBLIC_IMAGES", scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<{ result?: TemplateImage[] }>(`/project/${pid}/cloud/action`, {
        method: "POST",
        body: { action: "PUBLIC_IMAGES" },
        cloud: scope,
      }),
    enabled: !!pid && !!scope && dialog === "rebuild",
    select: (d) => d?.result ?? [],
  })

  if (isLoading || !server) {
    return (
      <>
        <PageHeader title="Server" eyebrow="Compute" />
        <div className="grid gap-4">
          <div className="flex items-center gap-3">
            <Skeleton className="h-5 w-24" />
            <Skeleton className="h-4 w-64" />
          </div>
          <Skeleton className="h-9 w-full max-w-xl" />
          <Skeleton className="h-64" />
        </div>
      </>
    )
  }

  const s = server.data?.server ?? {}
  const status = serverStatus(server)
  const ext = server.externalId ?? ""
  const name = serverName(server)

  const closeForm = () => setDialog(null)

  return (
    <>
      <PageHeader
        title={name}
        eyebrow="Compute"
        breadcrumb={
          <Breadcrumb>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink asChild>
                  <Link to={`/p/${pid}/servers`}>Servers</Link>
                </BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{name}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        description={`${serverFlavor(server)} — ${serverIPs(server).join(", ") || "no addresses"}`}
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setPending({ action: "START", title: "Start server", label: "start this server" })}
              disabled={status === "ACTIVE"}
            >
              <Play className="size-4" /> Start
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setPending({ action: "STOP", title: "Stop server", label: "stop this server" })}
              disabled={status === "SHUTOFF"}
            >
              <Square className="size-4" /> Stop
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setPending({ action: "SOFTREBOOT", title: "Reboot server", label: "reboot this server" })}
            >
              <RotateCw className="size-4" /> Reboot
            </Button>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline" size="sm">
                  <MoreVertical className="size-4" /> More actions
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem
                  onClick={() => {
                    setRenameVal(name)
                    setDialog("rename")
                  }}
                >
                  Rename
                </DropdownMenuItem>
                <DropdownMenuItem
                  onClick={() => {
                    setResizeFlavor("")
                    setDialog("resize")
                  }}
                >
                  Resize
                </DropdownMenuItem>
                {features.has("serverRebuild") && (
                  <DropdownMenuItem
                    onClick={() => {
                      setRebuildImage("")
                      setDialog("rebuild")
                    }}
                  >
                    Rebuild
                  </DropdownMenuItem>
                )}
                {!features.has("serverRescue") ? null : status === "RESCUE" ? (
                  <DropdownMenuItem
                    onClick={() => setPending({ action: "UNRESCUE", title: "Unrescue server", label: "take this server out of rescue mode" })}
                  >
                    Unrescue
                  </DropdownMenuItem>
                ) : (
                  <DropdownMenuItem
                    onClick={() => setPending({ action: "RESCUE", title: "Rescue server", label: "put this server into rescue mode" })}
                  >
                    Rescue
                  </DropdownMenuItem>
                )}
                {features.has("serverPassword") && (
                  <DropdownMenuItem
                    onClick={() => {
                      setPasswordVal("")
                      setDialog("password")
                    }}
                  >
                    Set password
                  </DropdownMenuItem>
                )}
                <DropdownMenuItem onClick={openConsole}>Console (VNC)</DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem variant="destructive" onClick={() => setDialog("delete")}>
                  <Trash2 className="size-4" /> Delete
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </>
        }
      />

      <div className="mb-4 flex items-center gap-3">
        <StatusBadge status={status} />
        <span className="font-mono text-xs text-muted-foreground">{server.externalId}</span>
      </div>

      {status === "VERIFY_RESIZE" && !resizeActed ? (
        <div className="mb-4 flex flex-wrap items-center gap-3 rounded-md border bg-muted p-3 text-sm">
          <span>The resize is waiting for verification.</span>
          <Button
            size="sm"
            onClick={() => {
              setResizeActed(true)
              act.mutate({ action: "CONFIRMRESIZE" }, { onError: () => setResizeActed(false) })
            }}
            disabled={act.isPending}
          >
            Confirm resize
          </Button>
          <Button
            size="sm"
            variant="outline"
            onClick={() => {
              setResizeActed(true)
              act.mutate({ action: "REVERTRESIZE" }, { onError: () => setResizeActed(false) })
            }}
            disabled={act.isPending}
          >
            Revert resize
          </Button>
        </div>
      ) : null}

      <Tabs defaultValue="overview">
        {/* 7 triggers overflow narrow viewports — the wrapper scrolls horizontally
            instead of wrapping/clipping (thin scrollbar as the affordance). */}
        <div className="-mx-1 overflow-x-auto px-1 pb-1">
          <TabsList className="w-max">
            <TabsTrigger value="overview">Overview</TabsTrigger>
            <TabsTrigger value="network">Network</TabsTrigger>
            <TabsTrigger value="security">Security</TabsTrigger>
            <TabsTrigger value="volumes">Volumes</TabsTrigger>
            <TabsTrigger value="protection">Protection</TabsTrigger>
            {features.has("serverEvents") && <TabsTrigger value="events">Events</TabsTrigger>}
            {features.has("serverDiskSnapshots") && <TabsTrigger value="disk-snapshots">Snapshots</TabsTrigger>}
            {features.has("serverSnapshots") && <TabsTrigger value="snapshots">Images</TabsTrigger>}
            {features.has("consoleLog") && <TabsTrigger value="logs">Console log</TabsTrigger>}
          </TabsList>
        </div>

        <TabsContent value="overview" className="mt-4">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Details</CardTitle>
            </CardHeader>
            <CardContent>
              <dl className="grid gap-x-8 gap-y-3 text-sm sm:grid-cols-2">
                <Row k="Status" v={<StatusBadge status={status} />} />
                <Row k="Flavor" v={serverFlavor(server)} />
                <Row k="IP addresses" v={<span className="font-mono">{serverIPs(server).join(", ") || "—"}</span>} />
                <Row k="Created" v={fmtDateTime((s.created as string) ?? server.info?.createdAt ?? server.createdAt)} />
                {features.has("spreadGroups") && s.spreadGroup ? <Row k="Spread group" v={String(s.spreadGroup)} /> : null}
              </dl>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="network" className="mt-4">
          {features.has("ports") ? (
            <NetworkTab pid={pid} resourceId={resourceId} ext={ext} scope={scope} />
          ) : (
            <div className="grid gap-4">
              <AddressesTab server={server} />
              {features.has("publicIp") && (
                <PublicIpCard pid={pid} resourceId={resourceId} server={server} scope={scope} />
              )}
            </div>
          )}
        </TabsContent>

        <TabsContent value="security" className="mt-4">
          <SecurityTab pid={pid} resourceId={resourceId} scope={scope} />
        </TabsContent>

        <TabsContent value="protection" className="mt-4">
          <ServerProtection pid={pid} resourceId={resourceId} />
        </TabsContent>

        <TabsContent value="volumes" className="mt-4">
          <VolumesTab pid={pid} ext={ext} scope={scope} />
        </TabsContent>

        <TabsContent value="events" className="mt-4">
          <EventsTab pid={pid} resourceId={resourceId} scope={scope} />
        </TabsContent>

        {features.has("serverDiskSnapshots") && (
          <TabsContent value="disk-snapshots" className="mt-4">
            <DiskSnapshotsTab pid={pid} resourceId={resourceId} scope={scope} />
          </TabsContent>
        )}

        {features.has("serverSnapshots") && (
          <TabsContent value="snapshots" className="mt-4">
            <SnapshotsTab pid={pid} resourceId={resourceId} ext={ext} scope={scope} />
          </TabsContent>
        )}

        <TabsContent value="logs" className="mt-4">
          <ConsoleLog pid={pid} resourceId={resourceId} />
        </TabsContent>
      </Tabs>

      {/* Confirm dialog for quick + rescue actions */}
      <Dialog open={!!pending} onOpenChange={(o) => !o && setPending(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{pending?.title ?? "Confirm action"}</DialogTitle>
            <DialogDescription>Are you sure you want to {pending?.label}?</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPending(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => {
                if (pending) act.mutate({ action: pending.action })
                setPending(null)
              }}
            >
              {pending?.title ?? "Confirm"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Rename */}
      <Dialog open={dialog === "rename"} onOpenChange={(o) => !o && closeForm()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Rename server</DialogTitle>
            <DialogDescription>Set a new name for this server.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="rename-input">Name</Label>
            <Input id="rename-input" value={renameVal} onChange={(e) => setRenameVal(e.target.value)} />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={closeForm}>
              Cancel
            </Button>
            <Button
              disabled={!renameVal.trim()}
              onClick={() => {
                act.mutate({ action: "RENAME", data: { name: renameVal.trim() } })
                closeForm()
              }}
            >
              Rename
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Resize */}
      <Dialog open={dialog === "resize"} onOpenChange={(o) => !o && closeForm()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Resize server</DialogTitle>
            <DialogDescription>
              Pick a new flavor. A running server restarts to apply it. A disk can grow but never shrink.
            </DialogDescription>
          </DialogHeader>
          {flavors.isLoading ? (
            <Skeleton className="h-9" />
          ) : (
            <Select value={resizeFlavor} onValueChange={setResizeFlavor}>
              <SelectTrigger className="w-full">
                <SelectValue placeholder="Select a flavor" />
              </SelectTrigger>
              <SelectContent>
                {(flavors.data ?? [])
                  .filter((f) => !!f.externalId)
                  .map((f) => (
                    <SelectItem key={f.externalId} value={f.externalId!}>
                      {f.data?.name ?? f.externalId} · {f.data?.vcpus ?? "?"} vCPU ·{" "}
                      {f.data?.ram ? `${Math.round(f.data.ram / 1024)} GB` : "?"}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={closeForm}>
              Cancel
            </Button>
            <Button
              disabled={!resizeFlavor}
              onClick={() => {
                act.mutate({ action: "RESIZE", data: { flavorId: resizeFlavor } })
                closeForm()
              }}
            >
              Resize
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Rebuild */}
      <Dialog open={dialog === "rebuild"} onOpenChange={(o) => !o && closeForm()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Rebuild server</DialogTitle>
            <DialogDescription>
              Reinstall this server from an image. All data on the root disk will be lost.
            </DialogDescription>
          </DialogHeader>
          {images.isLoading ? (
            <Skeleton className="h-9" />
          ) : (
            <Select value={rebuildImage} onValueChange={setRebuildImage}>
              <SelectTrigger className="w-full">
                <SelectValue placeholder="Select an image" />
              </SelectTrigger>
              <SelectContent>
                {(images.data ?? [])
                  .filter((im) => !!im.id)
                  .map((im) => (
                    <SelectItem key={String(im.id)} value={String(im.id)}>
                      {String(im.name ?? im.id)}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={closeForm}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={!rebuildImage}
              onClick={() => {
                act.mutate({ action: "REBUILD", data: { imageId: rebuildImage } })
                closeForm()
              }}
            >
              Rebuild
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Set password */}
      <Dialog open={dialog === "password"} onOpenChange={(o) => !o && closeForm()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Set password</DialogTitle>
            <DialogDescription>
              The guest agent sets it in the running server. Without a username it is the image's default user.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="password-user">Username (optional)</Label>
            <Input
              id="password-user"
              value={passwordUser}
              onChange={(e) => setPasswordUser(e.target.value)}
              autoComplete="off"
            />
            <Label htmlFor="password-input">New password</Label>
            <Input
              id="password-input"
              type="password"
              value={passwordVal}
              onChange={(e) => setPasswordVal(e.target.value)}
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={closeForm}>
              Cancel
            </Button>
            <Button
              disabled={!passwordVal}
              onClick={() => {
                act.mutate({
                  action: "SET_PASSWORD",
                  data: { password: passwordVal, ...(passwordUser.trim() ? { username: passwordUser.trim() } : {}) },
                })
                setPasswordVal("")
                setPasswordUser("")
                closeForm()
              }}
            >
              Set password
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete */}
      <Dialog open={dialog === "delete"} onOpenChange={(o) => !o && closeForm()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete server</DialogTitle>
            <DialogDescription>
              Are you sure you want to delete "{name}"? This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <ServerDeleteVolumes checked={deleteVolumes} onChange={setDeleteVolumes} />
          <DialogFooter>
            <Button variant="outline" onClick={closeForm}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={del.isPending}
              onClick={() => {
                del.mutate()
                closeForm()
              }}
            >
              <Trash2 className="size-4" /> Delete server
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

function Row({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4 border-b pb-2 last:border-0 sm:border-0 sm:pb-0">
      <dt className="text-muted-foreground">{k}</dt>
      <dd className="text-right">{v}</dd>
    </div>
  )
}

// ── Addresses (NIC/IP) panel: the guest-reported addresses from the server row, grouped by
// network. Shown instead of the ports tab on providers without ports (Proxmox). ──
function AddressesTab({ server }: { server: CloudResource }) {
  const addrs = (server.data?.server?.addresses ?? {}) as Record<string, Array<{ addr?: string; version?: number }>>
  const rows = Object.entries(addrs).flatMap(([net, list]) =>
    (Array.isArray(list) ? list : []).filter((a) => a?.addr).map((a) => ({ net, addr: a.addr as string, version: a.version })),
  )
  if (!rows.length) {
    return (
      <p className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
        No addresses reported yet. They appear once the server is running with its guest agent.
      </p>
    )
  }
  return (
    <Card className="overflow-hidden py-0">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Network</TableHead>
            <TableHead>Address</TableHead>
            <TableHead>Version</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r) => (
            <TableRow key={`${r.net}-${r.addr}`}>
              <TableCell>{r.net}</TableCell>
              <TableCell className="font-mono text-sm">{r.addr}</TableCell>
              <TableCell className="text-sm text-muted-foreground">{r.version ? `IPv${r.version}` : "—"}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Card>
  )
}

// ── Network tab: the server's ports (live list scoped by deviceId) + attach/detach. ──
function NetworkTab({
  pid, resourceId, ext, scope,
}: { pid: string; resourceId: string; ext: string; scope?: CloudScope }) {
  const qc = useQueryClient()
  const ports = useCloudList(pid, "PORT", `&deviceId=${ext}`)
  const allPorts = useCloudList(pid, "PORT")
  const [attachId, setAttachId] = useState("")

  const invalidate = () =>
    setTimeout(() => void qc.invalidateQueries({ queryKey: ["cloud", pid, "PORT"] }), 1500)

  const attach = useMutation({
    // clusterAction TypeServer ATTACH_PORT reads data{portId} (resolved via extID — a live port id passes through).
    mutationFn: (portId: string) => actionFetch(pid, resourceId, scope, "ATTACH_PORT", { portId }),
    onSuccess: () => {
      toast.success("Port attach requested")
      setAttachId("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const detach = useMutation({
    mutationFn: (portId: string) => actionFetch(pid, resourceId, scope, "DETACH_PORT", { portId }),
    onSuccess: () => {
      toast.success("Port detach requested")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const freePorts = (allPorts.data ?? []).filter((p) => !(p.data?.port?.device_id as string))

  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <Select value={attachId} onValueChange={setAttachId}>
          <SelectTrigger className="w-72">
            <SelectValue placeholder={freePorts.length ? "Select an unattached port" : "No unattached ports"} />
          </SelectTrigger>
          <SelectContent>
            {freePorts.map((p) => (
              <SelectItem key={p.id} value={p.id}>
                {(p.data?.port?.name as string) || p.id}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button
          size="sm"
          disabled={!attachId || attach.isPending}
          onClick={() => attach.mutate(attachId)}
        >
          <Plus className="size-4" /> Attach port
        </Button>
      </div>

      {ports.isLoading ? (
        <Skeleton className="h-40" />
      ) : !ports.data?.length ? (
        <p className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
          No ports attached to this server.
        </p>
      ) : (
        <Card className="overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Port</TableHead>
                <TableHead>MAC address</TableHead>
                <TableHead>Fixed IPs</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="w-24" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {ports.data.map((p) => {
                const port = (p.data?.port ?? {}) as Record<string, any>
                const ips = ((port.fixed_ips as Array<{ ip_address?: string }>) ?? [])
                  .map((ip) => ip.ip_address)
                  .filter(Boolean)
                  .join(", ")
                return (
                  <TableRow key={p.id}>
                    <TableCell className="font-mono text-xs">{(port.name as string) || p.id}</TableCell>
                    <TableCell className="font-mono text-xs">{(port.mac_address as string) ?? "—"}</TableCell>
                    <TableCell className="font-mono text-sm">{ips || "—"}</TableCell>
                    <TableCell>
                      <StatusBadge status={port.status as string} />
                    </TableCell>
                    <TableCell className="text-right">
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={detach.isPending}
                        onClick={() => detach.mutate(p.id)}
                      >
                        <X className="size-4" /> Detach
                      </Button>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </Card>
      )}
    </div>
  )
}

// ── Security tab: the server's security groups (LIST_SECURITY_GROUPS action) + add/remove by NAME
// (clusterAction ADD/REMOVE_SECURITY_GROUP read firstStr(data, "name", ...)). ──
function SecurityTab({
  pid, resourceId, scope,
}: { pid: string; resourceId: string; scope?: CloudScope }) {
  const qc = useQueryClient()
  const [addName, setAddName] = useState("")

  const attached = useQuery({
    queryKey: ["server-action", pid, resourceId, "LIST_SECURITY_GROUPS"],
    queryFn: () => actionFetch<CloudResource[]>(pid, resourceId, scope, "LIST_SECURITY_GROUPS"),
    enabled: !!pid && !!resourceId && !!scope,
    select: (d) => d?.result ?? [],
  })
  const available = useCloudList(pid, "SECURITY_GROUP")

  const invalidate = () =>
    setTimeout(
      () => void qc.invalidateQueries({ queryKey: ["server-action", pid, resourceId, "LIST_SECURITY_GROUPS"] }),
      1500,
    )

  const add = useMutation({
    mutationFn: (name: string) => actionFetch(pid, resourceId, scope, "ADD_SECURITY_GROUP", { name }),
    onSuccess: () => {
      toast.success("Security group added")
      setAddName("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const remove = useMutation({
    mutationFn: (name: string) => actionFetch(pid, resourceId, scope, "REMOVE_SECURITY_GROUP", { name }),
    onSuccess: () => {
      toast.success("Security group removed")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const sgName = (r: CloudResource) => (r.data?.securityGroup?.name as string) ?? r.name ?? r.id
  const attachedNames = new Set((attached.data ?? []).map(sgName))
  const addable = (available.data ?? []).filter((sg) => !attachedNames.has(sgName(sg)))

  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <Select value={addName} onValueChange={setAddName}>
          <SelectTrigger className="w-72">
            <SelectValue placeholder={addable.length ? "Select a security group" : "No security groups to add"} />
          </SelectTrigger>
          <SelectContent>
            {addable.map((sg) => (
              <SelectItem key={sg.id} value={sgName(sg)}>
                {sgName(sg)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button size="sm" disabled={!addName || add.isPending} onClick={() => add.mutate(addName)}>
          <Plus className="size-4" /> Add group
        </Button>
      </div>

      {attached.isLoading ? (
        <Skeleton className="h-40" />
      ) : !attached.data?.length ? (
        <p className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
          No security groups attached to this server.
        </p>
      ) : (
        <Card className="overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead className="w-24" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {attached.data.map((sg) => (
                <TableRow key={sg.id}>
                  <TableCell className="font-medium">{sgName(sg)}</TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="sm"
                      disabled={remove.isPending}
                      onClick={() => remove.mutate(sgName(sg))}
                    >
                      <X className="size-4" /> Remove
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}
    </div>
  )
}

// ── Volumes tab: VOLUME cache list filtered to this server's attachments; ATTACH/DETACH act on the
// VOLUME resource with data{serverId: <server externalId>} (providers/write.go TypeVolume). ──
function volumeAttachedTo(v: CloudResource, ext: string): boolean {
  const cacheAtts = (v.data?.attachments as Array<Record<string, any>>) ?? []
  if (cacheAtts.some((a) => a?.serverId === ext || a?.server_id === ext)) return true
  const volAtts = (v.data?.volume?.attachments as Array<Record<string, any>>) ?? []
  return volAtts.some((a) => a?.server_id === ext || a?.serverId === ext)
}
function volumeIsFree(v: CloudResource): boolean {
  const cacheAtts = (v.data?.attachments as Array<Record<string, any>>) ?? []
  const volAtts = (v.data?.volume?.attachments as Array<Record<string, any>>) ?? []
  return cacheAtts.length === 0 && volAtts.length === 0
}

function VolumesTab({ pid, ext, scope }: { pid: string; ext: string; scope?: CloudScope }) {
  const qc = useQueryClient()
  const volumes = useCloudList(pid, "VOLUME")
  const [attachId, setAttachId] = useState("")

  const invalidate = () =>
    setTimeout(() => void qc.invalidateQueries({ queryKey: ["cloud", pid, "VOLUME"] }), 1500)

  const volAct = useMutation({
    mutationFn: (p: { volumeId: string; action: "ATTACH" | "DETACH" }) =>
      actionFetch(pid, p.volumeId, scope, p.action, { serverId: ext }),
    onSuccess: (_d, p) => {
      toast.success(p.action === "ATTACH" ? "Volume attach requested" : "Volume detach requested")
      setAttachId("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const volName = (v: CloudResource) => (v.data?.volume?.name as string) || v.name || v.id
  const attachedVols = (volumes.data ?? []).filter((v) => volumeAttachedTo(v, ext))
  const freeVols = (volumes.data ?? []).filter(volumeIsFree)

  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <Select value={attachId} onValueChange={setAttachId}>
          <SelectTrigger className="w-72">
            <SelectValue placeholder={freeVols.length ? "Select an unattached volume" : "No unattached volumes"} />
          </SelectTrigger>
          <SelectContent>
            {freeVols.map((v) => (
              <SelectItem key={v.id} value={v.id}>
                {volName(v)}
                {v.data?.volume?.size ? ` · ${v.data.volume.size} GB` : ""}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button
          size="sm"
          disabled={!attachId || volAct.isPending}
          onClick={() => volAct.mutate({ volumeId: attachId, action: "ATTACH" })}
        >
          <HardDrive className="size-4" /> Attach volume
        </Button>
      </div>

      {volumes.isLoading ? (
        <Skeleton className="h-40" />
      ) : !attachedVols.length ? (
        <p className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
          No volumes attached to this server.
        </p>
      ) : (
        <Card className="overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Size</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="w-24" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {attachedVols.map((v) => (
                <TableRow key={v.id}>
                  <TableCell className="font-medium">{volName(v)}</TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {v.data?.volume?.size ? `${v.data.volume.size} GB` : "—"}
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={(v.data?.volume?.status as string) ?? v.status} />
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="sm"
                      disabled={volAct.isPending}
                      onClick={() => volAct.mutate({ volumeId: v.id, action: "DETACH" })}
                    >
                      <X className="size-4" /> Detach
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}
    </div>
  )
}

// ── Events tab: LIST_EVENTS → [{date, action, message, requestId, userId}] (cloud_writes.go). ──
type ServerEvent = { date?: string; action?: string; message?: string; requestId?: string; userId?: string }

function EventsTab({ pid, resourceId, scope }: { pid: string; resourceId: string; scope?: CloudScope }) {
  const events = useQuery({
    queryKey: ["server-action", pid, resourceId, "LIST_EVENTS"],
    queryFn: () => actionFetch<ServerEvent[]>(pid, resourceId, scope, "LIST_EVENTS"),
    enabled: !!pid && !!resourceId && !!scope,
    select: (d) => d?.result ?? [],
  })

  if (events.isLoading) return <Skeleton className="h-40" />
  if (!events.data?.length) {
    return (
      <p className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
        No events recorded for this server.
      </p>
    )
  }
  return (
    <Card className="overflow-hidden py-0">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Time</TableHead>
            <TableHead>Action</TableHead>
            <TableHead>Message</TableHead>
            <TableHead>User</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {events.data.map((e, i) => (
            <TableRow key={`${e.requestId ?? i}`}>
              <TableCell className="whitespace-nowrap text-sm text-muted-foreground">{fmtDateTime(e.date)}</TableCell>
              <TableCell className="font-medium">{e.action ?? "—"}</TableCell>
              <TableCell className="text-sm text-muted-foreground">{e.message || "—"}</TableCell>
              <TableCell className="font-mono text-xs">{e.userId ?? "—"}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Card>
  )
}

// ── Public IP: a second NIC on an operator-designated public network with one address from its pool.
// It reaches the guest at the next boot (rebootRequired). ──
function PublicIpCard({
  pid, resourceId, server, scope,
}: { pid: string; resourceId: string; server: CloudResource; scope?: CloudScope }) {
  const qc = useQueryClient()
  const networks = useQuery({
    queryKey: ["public-networks", pid, scope?.serviceId],
    queryFn: () => apiFetch<Array<{ id: string; name?: string }>>(`/project/${pid}/public-networks`, { cloud: scope }),
    enabled: !!pid && !!scope,
  })
  const [networkId, setNetworkId] = useState("")
  const [reboot, setReboot] = useState(false)
  const addrs = (server.data?.server?.addresses ?? {}) as Record<string, Array<{ addr?: string }>>
  const publicIps = (addrs.public ?? []).map((a) => a.addr).filter(Boolean) as string[]
  const change = useMutation({
    mutationFn: (p: { action: string; networkId?: string }) =>
      actionFetch(pid, resourceId, scope, p.action, p.networkId ? { networkId: p.networkId } : {}),
    onSuccess: (_d, p) => {
      toast.success(p.action === "ATTACH_PUBLIC_NETWORK" ? "Public IP attached" : "Public IP detached")
      setReboot(true)
      void qc.invalidateQueries({ queryKey: ["cloud-resource", pid, resourceId] })
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "FLOATING_IP"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const rows = networks.data ?? []
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <Globe className="size-4" /> Public IP
        </CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {publicIps.length ? (
          <div className="flex flex-wrap items-center gap-3">
            <span className="font-mono">{publicIps.join(", ")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={change.isPending}
              onClick={() => change.mutate({ action: "DETACH_PUBLIC_NETWORK" })}
            >
              <X className="size-4" /> Detach
            </Button>
          </div>
        ) : networks.isLoading ? (
          <Skeleton className="h-9 w-72" />
        ) : !rows.length ? (
          <p className="text-muted-foreground">No public network is available to this project.</p>
        ) : (
          <div className="flex flex-wrap items-center gap-2">
            <Select value={networkId} onValueChange={setNetworkId}>
              <SelectTrigger className="w-72" aria-label="Public network">
                <SelectValue placeholder="Select a public network" />
              </SelectTrigger>
              <SelectContent>
                {rows.map((n) => (
                  <SelectItem key={n.id} value={n.id}>
                    {n.name ?? n.id}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button
              size="sm"
              disabled={!networkId || change.isPending}
              onClick={() => change.mutate({ action: "ATTACH_PUBLIC_NETWORK", networkId })}
            >
              <Plus className="size-4" /> Attach public IP
            </Button>
          </div>
        )}
        {reboot && (
          <p className="text-muted-foreground">
            Reboot the server for the guest to pick up the change. The public NIC takes the default route.
          </p>
        )}
      </CardContent>
    </Card>
  )
}

// ── Snapshots tab: the VM's disk snapshots. ZFS rolls back to the newest one only; a disk on storage
// without snapshots refuses (409 with the reason). ──
type DiskSnapshot = { name: string; description?: string; createdAt?: number; latest?: boolean }

function DiskSnapshotsTab({ pid, resourceId, scope }: { pid: string; resourceId: string; scope?: CloudScope }) {
  const qc = useQueryClient()
  const key = ["server-action", pid, resourceId, "LIST_SNAPSHOTS"]
  const snaps = useQuery({
    queryKey: key,
    queryFn: () => actionFetch<DiskSnapshot[]>(pid, resourceId, scope, "LIST_SNAPSHOTS"),
    enabled: !!pid && !!resourceId && !!scope,
    select: (d) => d?.result ?? [],
  })
  const [open, setOpen] = useState(false)
  const [snapName, setSnapName] = useState("")
  const [pending, setPending] = useState<{ action: string; name: string } | null>(null)
  const run = useMutation({
    mutationFn: (p: { action: string; name: string }) => actionFetch(pid, resourceId, scope, p.action, { name: p.name }),
    onSuccess: (_d, p) => {
      toast.success(
        p.action === "CREATE_SNAPSHOT" ? `Snapshot ${p.name} created`
          : p.action === "ROLLBACK_SNAPSHOT" ? `Rolled back to ${p.name}` : `Snapshot ${p.name} deleted`,
      )
      setOpen(false)
      setSnapName("")
      setPending(null)
      void qc.invalidateQueries({ queryKey: key })
      void qc.invalidateQueries({ queryKey: ["cloud-resource", pid, resourceId] })
    },
    onError: (e: Error) => {
      setPending(null)
      toast.error(e.message)
    },
  })
  const rows = snaps.data ?? []
  return (
    <div className="grid gap-4">
      <div>
        <Button size="sm" onClick={() => setOpen(true)}>
          <Camera className="size-4" /> Create snapshot
        </Button>
      </div>
      {snaps.isLoading ? (
        <Skeleton className="h-40" />
      ) : !rows.length ? (
        <p className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
          No snapshots of this server yet.
        </p>
      ) : (
        <Card className="overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="w-48" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((sn) => (
                <TableRow key={sn.name}>
                  <TableCell className="font-medium">
                    {sn.name}
                    {sn.latest ? <span className="ml-2 text-xs text-muted-foreground">newest</span> : null}
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {sn.createdAt ? timeAgo(new Date(sn.createdAt * 1000).toISOString()) : "—"}
                  </TableCell>
                  <TableCell className="text-right">
                    <Button variant="ghost" size="sm" onClick={() => setPending({ action: "ROLLBACK_SNAPSHOT", name: sn.name })}>
                      <History className="size-4" /> Roll back
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`Delete snapshot ${sn.name}`}
                      onClick={() => setPending({ action: "DELETE_SNAPSHOT", name: sn.name })}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create snapshot</DialogTitle>
            <DialogDescription>Saves the state of this server's disks. It protects against mistakes, not disk or node failure.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="disk-snap-name">Name</Label>
            <Input id="disk-snap-name" value={snapName} onChange={(e) => setSnapName(e.target.value)} placeholder="before-upgrade" />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button
              disabled={!snapName.trim() || run.isPending}
              onClick={() => run.mutate({ action: "CREATE_SNAPSHOT", name: snapName.trim() })}
            >
              {run.isPending ? "Creating…" : "Create snapshot"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <Dialog open={!!pending} onOpenChange={(o) => !o && setPending(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{pending?.action === "ROLLBACK_SNAPSHOT" ? "Roll back server" : "Delete snapshot"}</DialogTitle>
            <DialogDescription>
              {pending?.action === "ROLLBACK_SNAPSHOT"
                ? `The server returns to ${pending?.name}; everything written to its disks since is lost. A running server restarts.`
                : `Delete snapshot ${pending?.name}?`}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPending(null)}>
              Cancel
            </Button>
            <Button variant="destructive" disabled={run.isPending} onClick={() => pending && run.mutate(pending)}>
              {run.isPending ? "Working…" : pending?.action === "ROLLBACK_SNAPSHOT" ? "Roll back" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// ── Images tab: IMAGE list scoped by dataAssociatedTo=<server externalId> (listImages) + create
// image = POST /cloud {type:IMAGE, data:{name, serverId:<cache id>, imageType:snapshot}}. ──
function SnapshotsTab({
  pid, resourceId, ext, scope,
}: { pid: string; resourceId: string; ext: string; scope?: CloudScope }) {
  const qc = useQueryClient()
  const snaps = useCloudList(pid, "IMAGE", `&dataAssociatedTo=${ext}`)
  const [open, setOpen] = useState(false)
  const [snapName, setSnapName] = useState("")

  const create = useMutation({
    mutationFn: () =>
      apiFetch<CloudResource>(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: {
          type: "IMAGE",
          data: { name: snapName.trim(), serverId: resourceId, imageType: "snapshot" },
        },
      }),
    onSuccess: () => {
      toast.success(`Image "${snapName.trim()}" is being created`)
      setOpen(false)
      setSnapName("")
      setTimeout(() => void qc.invalidateQueries({ queryKey: ["cloud", pid, "IMAGE"] }), 1500)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const rows = (snaps.data ?? []).filter((im) => {
    const iu = im.data?.image?.instance_uuid as string | undefined
    return !iu || iu === ext
  })

  return (
    <div className="grid gap-4">
      <div>
        <Button size="sm" onClick={() => setOpen(true)}>
          <Camera className="size-4" /> Create image
        </Button>
      </div>

      {snaps.isLoading ? (
        <Skeleton className="h-40" />
      ) : !rows.length ? (
        <p className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
          No images made from this server yet.
        </p>
      ) : (
        <Card className="overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Size</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((im) => {
                const img = (im.data?.image ?? {}) as Record<string, any>
                const size = img.size as number | undefined
                return (
                  <TableRow key={im.id}>
                    <TableCell className="font-medium">{(img.name as string) ?? im.id}</TableCell>
                    <TableCell>
                      <StatusBadge status={img.status as string} />
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {size ? `${(size / 1073741824).toFixed(2)} GB` : "—"}
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {timeAgo(im.info?.createdAt ?? im.createdAt ?? (img.created_at as string))}
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </Card>
      )}

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create image</DialogTitle>
            <DialogDescription>Create a template image from this server's current disk.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="snap-name">Image name</Label>
            <Input id="snap-name" value={snapName} onChange={(e) => setSnapName(e.target.value)} placeholder="my-image" />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!snapName.trim() || create.isPending} onClick={() => create.mutate()}>
              <Camera className="size-4" /> Create image
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function ConsoleLog({ pid, resourceId }: { pid: string; resourceId: string }) {
  const scope = useCloudScope(pid)
  const logs = useMutation({
    mutationFn: () => actionFetch<string>(pid, resourceId, scope, "SHOW_CONSOLE_OUTPUT"),
  })
  return (
    <Card>
      <CardContent className="pt-6">
        <Button variant="outline" size="sm" onClick={() => logs.mutate()} disabled={logs.isPending}>
          {logs.isPending ? "Fetching…" : "Fetch console log"}
        </Button>
        {logs.data?.result ? (
          // Terminal look via tokens: inverted foreground/background reads as a
          // dark console in light mode and a deep panel in dark mode.
          <pre className="mt-4 max-h-96 overflow-auto rounded-lg bg-foreground/95 p-4 font-mono text-xs leading-relaxed text-background">
            {String(logs.data.result)}
          </pre>
        ) : null}
      </CardContent>
    </Card>
  )
}
