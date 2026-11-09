// Managed Databases (dbaas provider) — list/create/manage database clusters. The cloud scope
// for cluster CRUD is a DBAAS location (the create wizard offers a picker when the project has
// more than one); the engine/version catalog comes from the dbaas service DTO. The database
// runs on platform-owned infrastructure and its endpoint is a MetalLB address in a services
// subnet reachable over the project's normal egress — no network is picked. Actions map to Go
// cloud_dbaas.go: create (optionally restoring from a backup) / delete + GET_CONNECTION_INFO /
// RESIZE / RESIZE_STORAGE / SCALE_REPLICAS / RESTART / RESET_PASSWORD / SET_ALLOWED_CIDRS /
// UPGRADE / SET_AUTOSCALE / SET_SSO / SET_CUSTOM_DOMAIN / MANAGE_ACCESS / RESET_USER_PASSWORD /
// GET_LOGS / SET_PARAMETERS / SET_READ_ENDPOINT / SET_BACKUP / CREATE_BACKUP / LIST_BACKUPS /
// SET_INDEX_POLICIES.
import { useCallback, useEffect, useMemo, useState } from "react"
import { useNavigate, useParams, useSearchParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import {
  ArrowLeft, Check, Copy, DatabaseZap, Eye, EyeOff, Loader2, MoreHorizontal, Plus, RefreshCw, Settings2, Trash2,
} from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { apiFetch, type CloudScope } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useLocations, useProjectId, useProjectServices } from "@/lib/hooks"
import type { CloudResource, DatabaseEngineOffer, DatabaseLimits, Location } from "@/lib/types"

type Db = Record<string, any>

// The engine display catalog — keys mirror the backend's dbaas engine slugs (the offered set
// per provider comes from the service DTO's databaseEngines; unknown keys render as-is).
const ENGINES: Record<string, { label: string; hint?: string }> = {
  postgresql: { label: "PostgreSQL" },
  mysql: { label: "MySQL" },
  mariadb: { label: "MariaDB" },
  valkey: { label: "Valkey" },
  ferretdb: { label: "FerretDB", hint: "Mongo-compatible" },
  opensearch: { label: "OpenSearch", hint: "Search & analytics · HTTPS :9200" },
  kafka: { label: "Kafka", hint: "Event streaming (Strimzi) · SASL :9094" },
}
const engineLabel = (e: string) => ENGINES[e]?.label ?? e

// Per-action engine deny-sets — mirrors the FALSE entries of dbaas.Capabilities server-side
// (internal/cloud/dbaas/engines.go); actions absent here are engine-agnostic. Unknown engines
// show everything and let the server refuse.
const HIDDEN_ACTIONS: Record<string, Set<string>> = {
  UPGRADE: new Set(["valkey"]),
  RESTART: new Set(["mysql", "valkey", "opensearch", "kafka"]),
  RESIZE_STORAGE: new Set(["valkey", "opensearch"]),
  RESET_PASSWORD: new Set(["valkey", "opensearch"]),
  // postgresql/mariadb/opensearch have a values-shaped user model in their pinned operator;
  // valkey manages ACL users on its own CR (users only — no logical databases). mysql's
  // ps-operator has no user/database CRDs at all.
  MANAGE_ACCESS: new Set(["mysql", "ferretdb", "kafka"]),
  // Caches and streams are not what customers restore, and neither pinned operator has an
  // object-store backup CR worth the surface.
  BACKUP: new Set(["valkey", "opensearch", "kafka"]),
  // Recovery into a NEW database: postgresql/ferretdb/mariadb bootstrap from the object store,
  // mysql applies a Restore of a named backup to the fresh cluster. Mirrors the RESTORE
  // capability server-side.
  RESTORE: new Set(["valkey", "opensearch", "kafka"]),
  // Only these three have a reader separate from the writer. Mirrors
  // dbaas.ReadEndpointEngines — without it the button offered an action the server refuses.
  SET_READ_ENDPOINT: new Set(["valkey", "ferretdb", "opensearch", "kafka"]),
}
const supports = (engine: string, action: string) => !HIDDEN_ACTIONS[action]?.has(engine)

// Engines whose access surface is USERS ONLY: opensearch binds roles instead of databases,
// valkey is one keyspace with ACL users. Everything else manages logical databases too.
const hasLogicalDatabases = (engine: string) => engine !== "opensearch" && engine !== "valkey"
const accessLabel = (engine: string) =>
  engine === "opensearch" ? "Users & roles" : engine === "valkey" ? "Users" : "Databases & users"

// Size presets (cpu / memory GiB) — no flavors here, the DB runs on platform-owned nodes.
const SIZE_PRESETS = [
  { key: "S", label: "Small", cpu: 1, memoryGiB: 2 },
  { key: "M", label: "Medium", cpu: 2, memoryGiB: 8 },
  { key: "L", label: "Large", cpu: 4, memoryGiB: 16 },
] as const

function database(r: CloudResource): Db {
  return (r.data?.database as Db) ?? {}
}

// The endpoint stays "" until the LB ingress IP is assigned — READY implies it exists, so the
// list polls while anything is not READY yet (unlike kubernetes there is no separate busy
// notion: a DEGRADED cluster keeps its endpoint and stops the polling via its next READY flip).
// UNKNOWN is settled too — the operator cannot report the state, which is not in-flight work,
// so it must neither poll forever nor show a permanent spinner (it renders a neutral badge).
function dbSettled(d: Db): boolean {
  const s = String(d.status ?? "")
  return s === "READY" || s === "DEGRADED" || s === "SUSPENDED" || s === "UNKNOWN"
}

function endpointOf(d: Db): string {
  const host = String(d.endpoint ?? "")
  if (!host) return ""
  return d.port ? `${host}:${d.port}` : host
}

function computeOf(d: Db): string {
  const parts = [
    d.cpu != null ? `${d.cpu} vCPU` : "",
    d.memory_gib != null ? `${d.memory_gib} GiB` : "",
  ].filter(Boolean)
  const n = Number(d.replicas) || 0
  return parts.join(" · ") + (parts.length && n > 1 ? ` × ${n}` : "")
}

function diskOf(d: Db): string {
  return d.storage_gib != null ? `${d.storage_gib} GiB disk` : ""
}

function sizeOf(d: Db): string {
  return [computeOf(d), diskOf(d)].filter(Boolean).join(" · ")
}

// Stable key for a location picker — the API array order is not stable, so never key by index.
const locKeyOf = (l: Location) => `${l.serviceId ?? ""}::${l.region ?? ""}`

// Dotted-numeric version compare, mirrors the server's upgrade-path check — "10.11" < "11.4",
// "9.6" < "10". Missing segments count as 0.
function versionGt(a: string, b: string): boolean {
  const as = a.split(".").map(Number)
  const bs = b.split(".").map(Number)
  for (let i = 0; i < Math.max(as.length, bs.length); i++) {
    const x = as[i] ?? 0
    const y = bs[i] ?? 0
    if (x !== y) return x > y
  }
  return false
}

async function copyText(value: string) {
  try {
    await navigator.clipboard.writeText(value)
    toast.success("Copied")
  } catch {
    toast.error("Copy failed — select and copy manually")
  }
}

// useDatabasesData is the shared data layer of the database LIST page and the per-cluster
// DETAIL page: dbaas locations/scopes, the (live read-through) cluster list query, the
// engine catalog lookups, and the optimistic cache patch.
function useDatabasesData(pid: string) {
  const qc = useQueryClient()
  const locations = useLocations(pid)
  const services = useProjectServices(pid)

  const dLocs = useMemo(
    () => (locations.data ?? []).filter((l) => l.provider === "dbaas" && l.serviceId && l.region),
    [locations.data],
  )
  const dLoc = dLocs[0]
  const dScope: CloudScope | undefined =
    dLoc?.serviceId && dLoc?.region ? { serviceId: dLoc.serviceId, region: dLoc.region } : undefined

  // The dbaas service DTO — per service, so per location — carries the curated engine
  // catalog plus the size limits and StorageClass allowlist the create form needs.
  const serviceFor = useCallback(
    (serviceId?: string) => services.data?.find((s) => s.id === serviceId),
    [services.data],
  )
  const enginesFor = useCallback(
    (serviceId?: string): Record<string, DatabaseEngineOffer> => serviceFor(serviceId)?.databaseEngines ?? {},
    [serviceFor],
  )
  // The provider's pinned platform (chart) version — informational on the detail page.
  const platformVersionFor = useCallback(
    (serviceId?: string) => String(services.data?.find((s) => s.id === serviceId)?.databasePlatformVersion ?? ""),
    [services.data],
  )

  // A cached row records the dbaas service it lives on; rows synced before those fields
  // existed fall back to the first location (mirrors the kubernetes page).
  const rowServiceId = useCallback(
    (r: CloudResource) => r.serviceId || dLoc?.serviceId,
    [dLoc?.serviceId],
  )
  const rowScope = useCallback(
    (r: CloudResource): CloudScope | undefined => {
      const serviceId = r.serviceId || dLoc?.serviceId
      const region = r.region || dLocs.find((l) => l.serviceId === serviceId)?.region
      return serviceId && region ? { serviceId, region } : undefined
    },
    [dLoc?.serviceId, dLocs],
  )

  const clusters = useQuery({
    queryKey: ["cloud", pid, "DATABASE_CLUSTER"],
    queryFn: () =>
      apiFetch<CloudResource[]>(`/project/${pid}/resource?type=DATABASE_CLUSTER`, { method: "POST", cloud: dScope }),
    enabled: !!pid && !!dScope,
    refetchInterval: (query) => ((query.state.data ?? []).some((r) => !dbSettled(database(r))) ? 15000 : false),
  })

  const invalidate = useCallback(
    () => void qc.invalidateQueries({ queryKey: ["cloud", pid, "DATABASE_CLUSTER"] }),
    [qc, pid],
  )
  // Optimistic patch of a row's data.database after a successful action — the list query is the
  // single source both pages read; the next (live read-through) refetch overwrites with truth.
  const patchDatabase = useCallback(
    (id: string, patch: Record<string, any>) => {
      qc.setQueryData<CloudResource[]>(["cloud", pid, "DATABASE_CLUSTER"], (rows) =>
        rows?.map((r) =>
          r.id === id ? { ...r, data: { ...r.data, database: { ...(r.data?.database ?? {}), ...patch } } } : r,
        ),
      )
    },
    [qc, pid],
  )

  return { dLocs, dScope, clusters, serviceFor, enginesFor, platformVersionFor, rowServiceId, rowScope, invalidate, patchDatabase }
}

export default function DatabasesPage() {
  const pid = useProjectId()
  const navigate = useNavigate()
  const { dLocs, dScope, clusters, serviceFor, enginesFor, invalidate } = useDatabasesData(pid)
  const { data, isLoading, isError, error, refetch, isFetching } = clusters

  const [createLocKey, setCreateLocKey] = useState("")
  const createLoc = dLocs.find((l) => locKeyOf(l) === createLocKey) ?? dLocs[0]
  const createScope: CloudScope | undefined =
    createLoc?.serviceId && createLoc?.region ? { serviceId: createLoc.serviceId, region: createLoc.region } : undefined
  const createEngines = useMemo(() => enginesFor(createLoc?.serviceId), [enginesFor, createLoc?.serviceId])
  // Restore sources: this project's databases that actually have backups to read. Filtered by
  // engine inside the dialog, since the engine is picked there.
  const restoreSources = useMemo(
    () =>
      (data ?? [])
        .map((r: CloudResource) => ({ r, d: database(r) }))
        .filter(({ d }: { d: Db }) => (d.backup as { enabled?: boolean } | undefined)?.enabled === true)
        .map(({ r, d }: { r: CloudResource; d: Db }) => ({
          id: (r.externalId ?? r.id) as string,
          // The action route keys on the RESOURCE id while restoreFrom names the database id.
          resourceId: r.id,
          name: (d.name as string) || r.externalId || r.id,
          engine: String(d.engine ?? ""),
          // Recovery lays a physical copy on a fresh data directory, so the new database has to
          // be the SAME major version — the form seeds it from here when restoring.
          version: String(d.version ?? ""),
        })),
    [data],
  )
  const createSvc = serviceFor(createLoc?.serviceId)

  const [createOpen, setCreateOpen] = useState(false)
  const [toDelete, setToDelete] = useState<CloudResource | null>(null)
  // Restore is a CREATE: the engines recover into a fresh instance, never in place. The detail
  // page's per-backup Restore button therefore links here with the source pinned, and the form
  // opens on the same path a customer would have taken by hand. Params are consumed once so a
  // reload does not reopen a half-filled form.
  const [listParams, setListParams] = useSearchParams()
  const restoreParam = listParams.get("restore") ?? ""
  const [initialRestore, setInitialRestore] = useState<InitialRestore | null>(null)
  useEffect(() => {
    if (!restoreParam) return
    setInitialRestore({ sourceId: restoreParam, backupName: listParams.get("backup") ?? "" })
    setCreateOpen(true)
    const next = new URLSearchParams(listParams)
    for (const k of ["restore", "backup"]) next.delete(k)
    setListParams(next, { replace: true })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [restoreParam])

  const del = useMutation({
    mutationFn: (r: CloudResource) => apiFetch(`/project/${pid}/cloud/${r.id}`, { method: "DELETE", cloud: dScope }),
    onSuccess: () => {
      toast.success("Database deletion requested")
      setToDelete(null)
      setTimeout(invalidate, 1500)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => (database(r).name as string) || r.externalId || r.id,
        header: sortableHeader("Name"),
        cell: ({ row, getValue }) => (
          <button
            className="inline-block py-1 font-medium hover:underline"
            onClick={(e) => {
              e.stopPropagation()
              navigate(row.original.id)
            }}
          >
            {getValue()}
          </button>
        ),
      },
      {
        id: "engine",
        accessorFn: (r) => `${database(r).engine ?? ""} ${database(r).version ?? ""}`,
        header: sortableHeader("Engine"),
        cell: ({ row }) => {
          const d = database(row.original)
          return (
            <span className="whitespace-nowrap">
              {engineLabel(String(d.engine ?? "")) || "—"}{" "}
              <span className="font-mono text-xs text-muted-foreground">{String(d.version ?? "")}</span>
            </span>
          )
        },
      },
      {
        id: "status",
        accessorFn: (r) => (database(r).status as string) ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue() || undefined} />,
      },
      {
        id: "endpoint",
        accessorFn: (r) => endpointOf(database(r)),
        header: "Endpoint",
        cell: ({ row, getValue }) => {
          const ep = getValue() as string
          if (ep) return <span className="font-mono text-xs">{ep}</span>
          return dbSettled(database(row.original)) ? (
            <span className="text-xs text-muted-foreground">—</span>
          ) : (
            <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Loader2 className="size-3 animate-spin" /> Endpoint pending
            </span>
          )
        },
      },
      {
        id: "size",
        accessorFn: (r) => Number(database(r).cpu) || 0,
        header: sortableHeader("Size"),
        cell: ({ row }) => <span className="text-sm whitespace-nowrap">{sizeOf(database(row.original)) || "—"}</span>,
      },
      {
        id: "created",
        accessorFn: (r) => (database(r).created_at as string) ?? r.createdAt ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => (
          <div className="text-right" onClick={(e) => e.stopPropagation()}>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon-sm" aria-label="Database actions">
                  <MoreHorizontal className="size-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onClick={() => navigate(row.original.id)}>
                  <Settings2 className="size-4" /> Manage
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem variant="destructive" onClick={() => setToDelete(row.original)}>
                  <Trash2 className="size-4" /> Delete
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        ),
      },
    ],
    [navigate],
  )

  return (
    <>
      <PageHeader
        title="Databases"
        eyebrow="Platform"
        description="Managed databases — provisioned and operated for you, reachable over a private endpoint."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh">
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)} disabled={!dScope}>
              <Plus className="size-4" /> Create database
            </Button>
          </>
        }
      />

      {!isLoading && !isError && !data?.length ? (
        <EmptyState
          icon={DatabaseZap}
          title="No databases yet"
          hint="Create a managed database — pick an engine and a size; the platform runs it for you."
          action={
            <Button onClick={() => setCreateOpen(true)} disabled={!dScope}>
              <Plus className="size-4" /> Create database
            </Button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={data}
          isLoading={isLoading}
          error={isError ? (error as Error) : null}
          searchPlaceholder="Search databases…"
          onRowClick={(r) => navigate(r.id)}
        />
      )}

      {createOpen && (
        <DatabaseFormDialog
          pid={pid}
          scope={createScope}
          restoreSources={restoreSources}
          initialRestore={initialRestore}
          engines={createEngines}
          limits={createSvc?.databaseLimits}
          storageClasses={createSvc?.databaseStorageClasses}
          locations={dLocs}
          locKey={createLoc ? locKeyOf(createLoc) : ""}
          onLocKey={setCreateLocKey}
          onClose={() => {
            setCreateOpen(false)
            setInitialRestore(null)
          }}
          onSubmit={async (body) => {
            if (!createScope) throw new Error("Select a location first")
            await apiFetch(`/project/${pid}/cloud`, {
              method: "POST",
              body: { type: "DATABASE_CLUSTER", data: body },
              cloud: createScope,
            })
            toast.success(`Database "${body.name}" is being created`)
            setCreateOpen(false)
            invalidate()
          }}
        />
      )}

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete database</DialogTitle>
            <DialogDescription>
              Delete "{toDelete ? (database(toDelete).name as string) || toDelete.externalId : ""}"? All data in the
              database is lost. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => toDelete && del.mutate(toDelete)} disabled={del.isPending}>
              {del.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// ── database detail page (/p/:pid/databases/:resourceId) ─────────────────────
export function DatabaseClusterDetailPage() {
  const pid = useProjectId()
  const navigate = useNavigate()
  const { resourceId = "" } = useParams()
  const { clusters, serviceFor, enginesFor, platformVersionFor, rowServiceId, rowScope, invalidate, patchDatabase } =
    useDatabasesData(pid)
  const resource = (clusters.data ?? []).find((r) => r.id === resourceId)
  const [deleteOpen, setDeleteOpen] = useState(false)

  const del = useMutation({
    mutationFn: () =>
      apiFetch(`/project/${pid}/cloud/${resourceId}`, { method: "DELETE", cloud: resource ? rowScope(resource) : undefined }),
    onSuccess: () => {
      toast.success("Database deletion requested")
      setTimeout(invalidate, 1500)
      navigate(`/p/${pid}/databases`)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const name = resource ? (database(resource).name as string) || resource.externalId || resource.id : ""

  return (
    <>
      <PageHeader
        title={name || "Database"}
        eyebrow="Managed database"
        description={resource ? endpointOf(database(resource)) || "endpoint pending…" : undefined}
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => navigate(`/p/${pid}/databases`)}>
              <ArrowLeft className="size-4" /> All databases
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void clusters.refetch()}
              disabled={clusters.isFetching}
              aria-label="Refresh"
            >
              <RefreshCw className={clusters.isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
          </>
        }
      />

      {clusters.isLoading ? (
        <div className="py-20 text-center text-muted-foreground">Loading…</div>
      ) : !resource ? (
        <EmptyState
          icon={DatabaseZap}
          title="Database not found"
          hint="It may have been deleted, or the link is stale."
          action={
            <Button variant="outline" onClick={() => navigate(`/p/${pid}/databases`)}>
              <ArrowLeft className="size-4" /> Back to databases
            </Button>
          }
        />
      ) : (
        <DatabaseDetail
          pid={pid}
          scope={rowScope(resource)}
          resource={resource}
          engines={enginesFor(rowServiceId(resource))}
          limits={serviceFor(rowServiceId(resource))?.databaseLimits}
          platformVersion={platformVersionFor(rowServiceId(resource))}
          backupConfigured={!!serviceFor(rowServiceId(resource))?.databaseBackupConfigured}
          paramCatalog={serviceFor(rowServiceId(resource))?.databaseParameters}
          onDeleted={() => setDeleteOpen(true)}
          onPatch={(patch) => patchDatabase(resource.id, patch)}
        />
      )}

      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete database</DialogTitle>
            <DialogDescription>
              Deletes “{name}” and all data stored in it. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteOpen(false)}>Cancel</Button>
            <Button variant="destructive" onClick={() => del.mutate()} disabled={del.isPending}>
              {del.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// ── create form ──────────────────────────────────────────────────────────────
// Radix Select items cannot carry an empty value — this sentinel is the "no storageClass key
// in the create body" pick (the chart then uses the DB cluster's default StorageClass).
const CLUSTER_DEFAULT_STORAGE = "__cluster-default__"

function DatabaseFormDialog({
  engines, limits, storageClasses, restoreSources, initialRestore, pid, scope, locations, locKey, onLocKey, onClose, onSubmit,
}: {
  engines: Record<string, DatabaseEngineOffer>
  limits?: DatabaseLimits
  storageClasses?: string[]
  // Databases already in this project that a new one could be recovered from — same engine,
  // backups on. The page owns the list, so the dialog never re-fetches it.
  restoreSources: { id: string; name: string; engine: string; resourceId: string; version?: string }[]
  // Set when the form was opened from a specific backup (detail page → Restore).
  initialRestore?: InitialRestore | null
  // Needed only to list a source's backups when the engine restores by name (mysql).
  pid: string
  scope: CloudScope | undefined
  locations: Location[]
  locKey: string
  onLocKey: (key: string) => void
  onClose: () => void
  onSubmit: (body: Record<string, any>) => Promise<void>
}) {
  const engineKeys = Object.keys(engines).filter((e) => (engines[e]?.versions ?? []).length > 0)
  const [name, setName] = useState("")
  // Derived, not effect-reset: switching location swaps the catalog, and a stale pick that is
  // no longer offered falls back to the first offered engine/version.
  // Opened from a backup row: the engine and version are the SOURCE's, not the catalog's first
  // entry. Without this the form came up on whatever engine sorts first, the source did not match
  // it, and the whole restore block stayed hidden — a Restore button that silently restored
  // nothing. Version matters just as much: a physical copy only replays on the same major.
  const restoreSource = restoreSources.find((c) => c.id === initialRestore?.sourceId)
  // Derived, not a useState seed: the source list is loaded by the page and can arrive AFTER this
  // dialog mounts, and a state initialiser only ever runs once — the restore would then open on
  // the wrong engine with no way back to the right one.
  const [engineSel, setEngineSel] = useState("")
  const engine = engineKeys.includes(engineSel)
    ? engineSel
    : restoreSource?.engine && engineKeys.includes(restoreSource.engine)
      ? restoreSource.engine
      : engineKeys[0] ?? ""
  const offer = engines[engine] ?? {}
  const versions = (offer.versions ?? []).filter(Boolean)
  const [versionSel, setVersionSel] = useState("")
  const version = versions.includes(versionSel)
    ? versionSel
    : restoreSource?.version && versions.includes(restoreSource.version)
      ? restoreSource.version
      : offer.default && versions.includes(offer.default)
        ? offer.default
        : versions[0] ?? ""
  const allowedReplicas = offer.replicas?.length ? offer.replicas : [1, 3]
  const [replicasSel, setReplicasSel] = useState("")
  const replicas = allowedReplicas.includes(Number(replicasSel)) ? Number(replicasSel) : allowedReplicas[0]
  // Databases in this project that could be recovered from: same engine, backups on.
  // Computed by the page, which already has the list — the dialog must not re-fetch it.
  // OpenSearch only: deploy Dashboards (own private endpoint). Ignored for other engines.
  const [dashboards, setDashboards] = useState(false)
  // Restore-from-backup. A recovered database is a NEW one: these engines take physical
  // backups, which can only be laid on a fresh data directory, so the damaged database stays
  // untouched until the customer is satisfied with the replacement.
  const [restoreFrom, setRestoreFrom] = useState(initialRestore?.sourceId ?? "")
  const [restoreTime, setRestoreTime] = useState("")
  const restoreCandidates = restoreSources.filter((c) => c.engine === engine)
  // mysql restores by NAMING a backup object; the other engines find their own base backup.
  const needsBackupPick = engine === "mysql"
  const [restoreBackup, setRestoreBackup] = useState(initialRestore?.backupName ?? "")
  const sourceBackups = useMutation({
    mutationFn: (sourceResourceId: string) =>
      apiFetch<{ result?: { name: string; phase?: string; finishedAt?: string }[] }>(
        `/project/${pid}/cloud/${sourceResourceId}/action`,
        { method: "POST", body: { action: "LIST_BACKUPS", data: {} }, cloud: scope },
      ),
    onError: (e: Error) => toast.error(e.message),
  })
  const backupChoices = sourceBackups.data?.result ?? []
  // Dashboards OIDC, offered up front so SSO does not need a second trip through the actions
  // menu. Same fields (and same server-side rule) as the post-create SSO dialog.
  const [ssoConnectUrl, setSsoConnectUrl] = useState("")
  const [ssoClientId, setSsoClientId] = useState("")
  const [ssoRedirect, setSsoRedirect] = useState("")

  const [preset, setPreset] = useState<string>("S")
  const [cpu, setCpu] = useState(String(SIZE_PRESETS[0].cpu))
  const [memory, setMemory] = useState(String(SIZE_PRESETS[0].memoryGiB))
  const [storage, setStorage] = useState("20")
  const [storageClass, setStorageClass] = useState("") // "" = cluster default (no key sent)
  const [allowedCidrs, setAllowedCidrs] = useState("")
  const [pending, setPending] = useState(false)

  const pickPreset = (key: string) => {
    setPreset(key)
    const p = SIZE_PRESETS.find((x) => x.key === key)
    if (p) {
      setCpu(String(p.cpu))
      setMemory(String(p.memoryGiB))
    }
  }

  // Provider size limits (service DTO databaseLimits) — 0/absent means unbounded.
  const maxCpu = limits?.maxCpu || 0
  const maxMemory = limits?.maxMemoryGiB || 0
  const maxStorage = limits?.maxStorageGiB || 0
  const cpuOk = Number(cpu) >= 1 && (!maxCpu || Number(cpu) <= maxCpu)
  const memoryOk = Number(memory) >= 1 && (!maxMemory || Number(memory) <= maxMemory)
  const storageOk = Number(storage) >= 1 && (!maxStorage || Number(storage) <= maxStorage)
  const restoreOk = !restoreFrom || !needsBackupPick || !!restoreBackup
  const valid = !!name.trim() && !!engine && !!version && cpuOk && memoryOk && storageOk && restoreOk

  const submit = async () => {
    setPending(true)
    try {
      await onSubmit({
        name: name.trim(),
        engine,
        version,
        replicas,
        cpu: Number(cpu),
        memoryGiB: Number(memory),
        storageGiB: Number(storage),
        ...(storageClass ? { storageClass } : {}),
        ...(allowedCidrs.trim()
          ? { allowedCidrs: allowedCidrs.split(",").map((c) => c.trim()).filter(Boolean) }
          : {}),
        // The server refuses a beta engine without the explicit acknowledgement.
        ...(offer.beta ? { beta: true } : {}),
        ...(restoreFrom
          ? {
              restoreFrom: {
                sourceDatabaseId: restoreFrom,
                ...(restoreTime ? { targetTime: new Date(restoreTime).toISOString() } : {}),
                ...(restoreBackup ? { backupName: restoreBackup } : {}),
              },
            }
          : {}),
        ...(engine === "opensearch" && dashboards ? { dashboards: true } : {}),
        ...(engine === "opensearch" && dashboards && ssoConnectUrl.trim() && ssoClientId.trim()
          ? {
              sso: {
                connectUrl: ssoConnectUrl.trim(),
                clientId: ssoClientId.trim(),
                ...(ssoRedirect.trim() ? { baseRedirectUrl: ssoRedirect.trim() } : {}),
              },
            }
          : {}),
      })
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Create database</DialogTitle>
          <DialogDescription>
            The database is provisioned and operated by the platform and reachable over a
            private endpoint.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 py-2">
          {locations.length > 1 && (
            <div className="grid gap-2">
              <Label>Location</Label>
              <Select
                value={locKey}
                onValueChange={(k) => {
                  if (k === locKey) return
                  onLocKey(k)
                  // Offered engines/versions/storage classes can differ per location — clear
                  // stale picks.
                  setEngineSel("")
                  setVersionSel("")
                  setStorageClass("")
                }}
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {locations.map((l) => (
                    <SelectItem key={locKeyOf(l)} value={locKeyOf(l)}>
                      {l.displayName || l.region}
                      {l.serviceName ? ` — ${l.serviceName}` : ""}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}

          <div className="grid gap-2">
            <Label htmlFor="db-name">Name</Label>
            <Input id="db-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="orders-db" />
          </div>

          <div className="grid gap-2">
            <Label>Engine</Label>
            {engineKeys.length === 0 ? (
              <p className="text-sm text-muted-foreground">No engines offered at this location.</p>
            ) : (
              <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
                {engineKeys.map((e) => (
                  <button
                    key={e}
                    type="button"
                    className={`rounded-lg border p-3 text-left text-sm transition-colors ${
                      e === engine ? "border-primary bg-primary/5" : "hover:bg-muted/50"
                    }`}
                    onClick={() => {
                      setEngineSel(e)
                      setVersionSel("")
                    }}
                  >
                    <span className="flex items-center gap-1.5 font-medium">
                      {engineLabel(e)}
                      {engines[e]?.beta ? <Badge variant="outline">Beta</Badge> : null}
                    </span>
                    {ENGINES[e]?.hint ? (
                      <span className="text-xs text-muted-foreground">{ENGINES[e].hint}</span>
                    ) : null}
                  </button>
                ))}
              </div>
            )}
            {offer.beta ? (
              <p className="text-xs text-muted-foreground">
                {engineLabel(engine)} is offered in beta — not covered by the platform SLA yet.
              </p>
            ) : null}
          </div>

          {supports(engine, "RESTORE") && restoreCandidates.length > 0 && (
            <div className="grid gap-2 rounded-lg border p-3">
              <Label>Restore from a backup (optional)</Label>
              <p className="text-xs text-muted-foreground">
                Recovers another {engineLabel(engine)} database into this new one. The source is
                left running and untouched.
              </p>
              <Select
                value={restoreFrom || "none"}
                onValueChange={(v) => {
                  const id = v === "none" ? "" : v
                  setRestoreFrom(id)
                  setRestoreBackup("")
                  const picked = restoreCandidates.find((c) => c.id === id)
                  if (id && needsBackupPick && picked) sourceBackups.mutate(picked.resourceId)
                }}
              >
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">Start empty</SelectItem>
                  {restoreCandidates.map((c) => (
                    <SelectItem key={c.id} value={c.id}>{c.name}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {restoreFrom && needsBackupPick ? (
                <div className="grid gap-1">
                  <Label className="text-xs">Backup to restore</Label>
                  <Select value={restoreBackup} onValueChange={setRestoreBackup}>
                    <SelectTrigger>
                      <SelectValue placeholder={sourceBackups.isPending ? "Loading…" : "Pick a backup"} />
                    </SelectTrigger>
                    <SelectContent>
                      {backupChoices.map((b) => (
                        <SelectItem key={b.name} value={b.name}>
                          {b.name} {b.finishedAt ? `· ${timeAgo(b.finishedAt)}` : ""}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  {backupChoices.length === 0 && !sourceBackups.isPending ? (
                    <p className="text-xs text-muted-foreground">
                      This database has no completed backups yet.
                    </p>
                  ) : null}
                </div>
              ) : null}
              {restoreFrom ? (
                <div className="grid gap-1">
                  <Label htmlFor="rs-time" className="text-xs">Recover to (optional)</Label>
                  <Input id="rs-time" type="datetime-local" value={restoreTime} onChange={(e) => setRestoreTime(e.target.value)} />
                  <p className="text-xs text-muted-foreground">
                    Leave empty to recover everything the archive holds — the backup plus every
                    change made after it. Set a time only to deliberately go back to that instant.
                  </p>
                </div>
              ) : null}
            </div>
          )}

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label>Version</Label>
              <Select value={version} onValueChange={setVersionSel}>
                <SelectTrigger>
                  <SelectValue placeholder={versions.length ? "Pick a version" : "No versions offered"} />
                </SelectTrigger>
                <SelectContent>
                  {versions.map((v) => (
                    <SelectItem key={v} value={v}>{v}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label>Replicas</Label>
              <Select value={String(replicas)} onValueChange={setReplicasSel}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {allowedReplicas.map((n) => (
                    <SelectItem key={n} value={String(n)}>
                      {n === 1 ? "1 — single node" : `${n} — high availability`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          {engine === "opensearch" ? (
            <div className="flex items-center justify-between rounded-lg border p-3">
              <div>
                <Label htmlFor="db-dashboards" className="text-sm font-medium">OpenSearch Dashboards</Label>
                <div className="text-xs text-muted-foreground">
                  Deploys Dashboards with its own private endpoint.
                </div>
              </div>
              <Switch id="db-dashboards" checked={dashboards} onCheckedChange={setDashboards} />
            </div>
          ) : null}

          {engine === "opensearch" && dashboards ? (
            <div className="grid gap-3 rounded-lg border p-3">
              <div className="text-sm font-medium">Dashboards single sign-on (optional)</div>
              <div className="grid gap-2">
                <Label htmlFor="db-sso-connect">OIDC discovery URL</Label>
                <Input id="db-sso-connect" value={ssoConnectUrl} onChange={(e) => setSsoConnectUrl(e.target.value)} placeholder="https://idp.example.com/realms/main/.well-known/openid-configuration" />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="db-sso-client">Client ID</Label>
                <Input id="db-sso-client" value={ssoClientId} onChange={(e) => setSsoClientId(e.target.value)} placeholder="opensearch-dashboards" />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="db-sso-redirect">Base redirect URL</Label>
                <Input id="db-sso-redirect" value={ssoRedirect} onChange={(e) => setSsoRedirect(e.target.value)} placeholder="https://dashboards.example.com" />
              </div>
              <p className="text-xs text-muted-foreground">
                Register a PUBLIC OIDC client (PKCE) — no client secret is stored. Leave blank to
                configure SSO later.
              </p>
            </div>
          ) : null}

          <div className="grid gap-2">
            <Label>Size</Label>
            <div className="grid grid-cols-4 gap-2">
              {SIZE_PRESETS.map((p) => (
                <Button
                  key={p.key}
                  type="button"
                  variant={preset === p.key ? "default" : "outline"}
                  onClick={() => pickPreset(p.key)}
                >
                  {p.label}
                  <span className="text-xs opacity-70">{p.cpu}c/{p.memoryGiB}G</span>
                </Button>
              ))}
              <Button
                type="button"
                variant={preset === "custom" ? "default" : "outline"}
                onClick={() => setPreset("custom")}
              >
                Custom
              </Button>
            </div>
            {preset === "custom" && (
              <div className="grid grid-cols-2 gap-3">
                <div className="grid gap-2">
                  <Label htmlFor="db-cpu">vCPU</Label>
                  <Input id="db-cpu" type="number" min={1} max={maxCpu || undefined} value={cpu} onChange={(e) => setCpu(e.target.value)} />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="db-mem">Memory (GiB)</Label>
                  <Input id="db-mem" type="number" min={1} max={maxMemory || undefined} value={memory} onChange={(e) => setMemory(e.target.value)} />
                </div>
              </div>
            )}
            {maxCpu > 0 && Number(cpu) > maxCpu ? (
              <p className="text-xs text-destructive">At most {maxCpu} vCPU per database at this location.</p>
            ) : null}
            {maxMemory > 0 && Number(memory) > maxMemory ? (
              <p className="text-xs text-destructive">At most {maxMemory} GiB memory per database at this location.</p>
            ) : null}
          </div>

          <div className="grid gap-2">
            <Label htmlFor="db-storage">Storage (GiB)</Label>
            <Input id="db-storage" type="number" min={1} max={maxStorage || undefined} value={storage} onChange={(e) => setStorage(e.target.value)} />
            {maxStorage > 0 && Number(storage) > maxStorage ? (
              <p className="text-xs text-destructive">At most {maxStorage} GiB storage per database at this location.</p>
            ) : null}
            <p className="text-xs text-muted-foreground">Storage can be grown later, never shrunk.</p>
          </div>

          {(storageClasses?.length ?? 0) > 0 && (
            <div className="grid gap-2">
              <Label htmlFor="db-storageclass">Storage class</Label>
              <Select
                value={storageClass || CLUSTER_DEFAULT_STORAGE}
                onValueChange={(v) => setStorageClass(v === CLUSTER_DEFAULT_STORAGE ? "" : v)}
              >
                <SelectTrigger id="db-storageclass">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={CLUSTER_DEFAULT_STORAGE}>Cluster default</SelectItem>
                  {(storageClasses ?? []).map((sc) => (
                    <SelectItem key={sc} value={sc}>{sc}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                The storage tier the database volume is provisioned on — it cannot be changed later.
              </p>
            </div>
          )}

          <div className="grid gap-2">
            <Label htmlFor="db-cidrs">Allowed CIDRs (optional, comma-separated)</Label>
            <Input
              id="db-cidrs"
              className="font-mono"
              value={allowedCidrs}
              onChange={(e) => setAllowedCidrs(e.target.value)}
              placeholder="192.0.2.0/24, 198.51.100.7/32"
            />
            <p className="text-xs text-muted-foreground">
              Restricts which source addresses may reach the endpoint. Empty = no restriction.
            </p>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void submit()} disabled={!valid || pending}>
            {pending ? "Working…" : "Create database"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ── detail body (the manage surface, rendered by the detail page) ─────────────
type ConnectionInfo = {
  host?: string
  port?: number
  dbname?: string
  username?: string
  password?: string
  uri?: string
  engine?: string
}

function DatabaseDetail({
  pid, scope, resource, engines, limits, platformVersion, backupConfigured, paramCatalog, onDeleted, onPatch,
}: {
  pid: string
  scope: CloudScope | undefined
  resource: CloudResource
  engines: Record<string, DatabaseEngineOffer>
  limits?: DatabaseLimits
  platformVersion: string
  // False when the location has no backup object store — the backup surface stays hidden
  // rather than offering a toggle that would write nowhere.
  backupConfigured: boolean
  // Per-engine tunable catalog from the service DTO — the configuration form renders from
  // this, so it can never offer a setting the server allowlist would reject.
  paramCatalog?: Record<string, unknown[]>
  onDeleted: () => void
  // Optimistically applies a partial data.database patch to the cached row — called after
  // every successful mutating action so the sheet reflects the request immediately.
  onPatch: (patch: Record<string, any>) => void
}) {
  const d = database(resource)
  const name = (d.name as string) || resource.externalId || resource.id
  const engine = String(d.engine ?? "")
  const endpoint = endpointOf(d)
  const busy = !dbSettled(d)
  const allowedReplicas = engines[engine]?.replicas?.length ? engines[engine].replicas! : [1, 3]

  const [resizeOpen, setResizeOpen] = useState(false)
  const [storageOpen, setStorageOpen] = useState(false)
  const [upgradeOpen, setUpgradeOpen] = useState(false)
  const [replicasOpen, setReplicasOpen] = useState(false)
  const [restartOpen, setRestartOpen] = useState(false)
  const [resetOpen, setResetOpen] = useState(false)
  const [cidrsOpen, setCidrsOpen] = useState(false)
  const [ssoOpen, setSsoOpen] = useState(false)
  const [domainOpen, setDomainOpen] = useState(false)
  const [autoscaleOpen, setAutoscaleOpen] = useState(false)
  const [accessOpen, setAccessOpen] = useState(false)
  const [backupOpen, setBackupOpen] = useState(false)
  const navigate = useNavigate()
  // Read live off the DB cluster, never from the cache: this list is what a customer picks a
  // recovery point from, and a stale answer there is worse than a slow one.
  const backupRuns = useMutation({
    mutationFn: () =>
      apiFetch<{ result?: BackupRun[] }>(`/project/${pid}/cloud/${resource.id}/action`, {
        method: "POST",
        body: { action: "LIST_BACKUPS", data: {} },
        cloud: scope,
      }),
    onError: (e: Error) => toast.error(e.message),
  })
  const runs = (backupRuns.data?.result ?? []).slice(0, 10) as BackupRun[]
  const [paramsOpen, setParamsOpen] = useState(false)
  const backupsOn = (d.backup as BackupState | undefined)?.enabled === true
  const readEndpointOn = d.read_endpoint === true
  const [readOpen, setReadOpen] = useState(false)
  // The tunable catalog comes from the SERVER so the form can never offer a setting the
  // allowlist would reject.
  const paramDefs = (paramCatalog?.[engine] ?? []) as ParamDef[]
  const [policiesOpen, setPoliciesOpen] = useState(false)
  // MANAGE_ACCESS returns a password per NEWLY declared user, exactly once.
  const [newCredentials, setNewCredentials] = useState<Record<string, string> | null>(null)
  // RESET_PASSWORD's result is shown exactly once and never stored anywhere else.
  const [newPassword, setNewPassword] = useState<string | null>(null)

  const act = (action: string, data?: Record<string, any>) =>
    apiFetch<{ result?: any }>(`/project/${pid}/cloud/${resource.id}/action`, {
      method: "POST",
      body: { action, data: data ?? {} },
      cloud: scope,
    })

  // Connection info is fetched on demand and kept only in this mutation's local state —
  // it carries the live password and must never land in the query cache.
  const conn = useMutation({
    mutationFn: () => act("GET_CONNECTION_INFO"),
    onError: (e: Error) => toast.error(e.message),
  })
  const connInfo = (conn.data?.result ?? null) as ConnectionInfo | null

  const restart = useMutation({
    mutationFn: () => act("RESTART"),
    onSuccess: () => {
      onPatch({ status: "PROGRESSING" })
      toast.success("Restart started")
      setRestartOpen(false)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const resetPassword = useMutation({
    mutationFn: () => act("RESET_PASSWORD"),
    onSuccess: (res) => {
      const pw = res?.result?.password as string | undefined
      setResetOpen(false)
      if (!pw) {
        toast.error("The new password was not returned — try again")
        return
      }
      conn.reset() // any previously fetched connection info now shows a stale password
      setNewPassword(pw)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <>
      <div className="space-y-6">
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <div className="rounded-lg border bg-card p-3">
            <div className="text-xs text-muted-foreground">Status</div>
            <div className="mt-1">
              <StatusBadge status={(d.status as string) || undefined} />
            </div>
          </div>
          <div className="rounded-lg border bg-card p-3">
            <div className="text-xs text-muted-foreground">Engine</div>
            <div className="text-sm">
              {engineLabel(engine)} <span className="text-muted-foreground">{String(d.version ?? "")}</span>
            </div>
          </div>
          <div className="rounded-lg border bg-card p-3">
            <div className="text-xs text-muted-foreground">Size</div>
            <div className="text-sm">{sizeOf(d) || "\u2014"}</div>
          </div>
          <div className="rounded-lg border bg-card p-3">
            <div className="text-xs text-muted-foreground">Sync</div>
            <div className="text-sm">{(d.sync_status as string) || "\u2014"}</div>
          </div>
        </div>

        {busy && (
          <div className="flex items-center gap-2 rounded-xl border bg-card p-4">
            <Loader2 className="size-4 animate-spin text-muted-foreground" />
            <span className="text-sm">
              Working on it {"\u2014"} this page refreshes itself. The endpoint appears once the
              load balancer is programmed (a few minutes on first create).
            </span>
          </div>
        )}

        {connInfo && <ConnectionInfoCard info={connInfo} onClose={() => conn.reset()} />}

        {/* One tab per concern, each carrying BOTH the current state and the controls that
            change it. The flat button row this replaces made every action look equally likely
            and told the customer nothing about what was already configured. */}
        <Tabs
          defaultValue="overview"
          onValueChange={(v) => {
            if (v === "backups" && !backupRuns.data && !backupRuns.isPending) backupRuns.mutate()
          }}
        >
          <div className="-mx-1 overflow-x-auto px-1 pb-1">
            <TabsList className="w-max">
              <TabsTrigger value="overview">Overview</TabsTrigger>
              <TabsTrigger value="connect">Connection</TabsTrigger>
              {supports(engine, "MANAGE_ACCESS") && (
                <TabsTrigger value="access">{accessLabel(engine)}</TabsTrigger>
              )}
              {supports(engine, "BACKUP") && backupConfigured && <TabsTrigger value="backups">Backups</TabsTrigger>}
              {paramDefs.length > 0 && <TabsTrigger value="config">Configuration</TabsTrigger>}
              <TabsTrigger value="logs">Logs</TabsTrigger>
            </TabsList>
          </div>

          <TabsContent value="overview" className="mt-4 space-y-4">
            <Card>
              <CardHeader><CardTitle className="text-base">Details</CardTitle></CardHeader>
              <CardContent>
                <dl className="grid gap-x-8 gap-y-3 text-sm sm:grid-cols-2">
                  <DbRow k="Instances" v={String(d.replicas ?? 1)} />
                  <DbRow k="Compute" v={computeOf(d) || "\u2014"} />
                  <DbRow k="Storage" v={diskOf(d) || "\u2014"} />
                  <DbRow k="Storage class" v={(d.storage_class as string) || "cluster default"} mono />
                  <DbRow k="Location" v={resource.region || "\u2014"} />
                  <DbRow k="Platform" v={`${(d.chart_version as string) || "\u2014"}${platformVersion && platformVersion !== d.chart_version ? ` (pin ${platformVersion})` : ""}`} mono />
                </dl>
              </CardContent>
            </Card>

            <Card>
              <CardHeader><CardTitle className="text-base">Size and lifecycle</CardTitle></CardHeader>
              <CardContent className="flex flex-wrap gap-2">
                <Button size="sm" variant="outline" onClick={() => setResizeOpen(true)}>Resize</Button>
                {supports(engine, "RESIZE_STORAGE") && (
                  <Button size="sm" variant="outline" onClick={() => setStorageOpen(true)}>Resize storage</Button>
                )}
                <Button size="sm" variant="outline" onClick={() => setReplicasOpen(true)}>Scale replicas</Button>
                <Button size="sm" variant="outline" onClick={() => setAutoscaleOpen(true)}>Autoscale</Button>
                {supports(engine, "UPGRADE") && (
                  <Button size="sm" variant="outline" onClick={() => setUpgradeOpen(true)}>Upgrade version</Button>
                )}
                {supports(engine, "RESTART") && (
                  <Button size="sm" variant="outline" onClick={() => setRestartOpen(true)}>Restart</Button>
                )}
              </CardContent>
            </Card>

            <Card>
              <CardHeader><CardTitle className="text-base">Danger zone</CardTitle></CardHeader>
              <CardContent className="flex flex-wrap items-center gap-3">
                <Button size="sm" variant="destructive" onClick={onDeleted}>
                  <Trash2 className="size-4" /> Delete database
                </Button>
                <span className="text-xs text-muted-foreground">
                  Deletes the instance and its storage. Backups already taken are kept until their
                  retention expires.
                </span>
              </CardContent>
            </Card>
          </TabsContent>

          <TabsContent value="connect" className="mt-4 space-y-4">
            <Card>
              <CardHeader><CardTitle className="text-base">Endpoint</CardTitle></CardHeader>
              <CardContent className="grid gap-3">
                <div className="flex flex-wrap items-center gap-2 text-sm">
                  <span className="text-muted-foreground">Read/write:</span>
                  {endpoint ? (
                    <>
                      <span className="font-mono text-xs">{endpoint}</span>
                      <Button variant="ghost" size="icon-sm" aria-label="Copy endpoint" onClick={() => void copyText(endpoint)}>
                        <Copy className="size-3.5" />
                      </Button>
                    </>
                  ) : (
                    <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                      <Loader2 className="size-3 animate-spin" /> Pending {"\u2014"} appears once the load balancer is programmed
                    </span>
                  )}
                </div>
                {supports(engine, "SET_READ_ENDPOINT") && (
                  <div className="flex flex-wrap items-center gap-2 text-sm">
                    <span className="text-muted-foreground">Read-only:</span>
                    {readEndpointOn ? (
                      <>
                        <span className="font-mono text-xs">
                          {(d.read_endpoint_host as string)
                            ? `${d.read_endpoint_host}:${engine === "postgresql" ? d.port : 3307}`
                            : "pending"}
                        </span>
                        {(d.read_endpoint_host as string) ? (
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label="Copy read endpoint"
                            onClick={() => void copyText(`${d.read_endpoint_host}:${engine === "postgresql" ? d.port : 3307}`)}
                          >
                            <Copy className="size-3.5" />
                          </Button>
                        ) : null}
                      </>
                    ) : (
                      <span className="text-xs text-muted-foreground">Not enabled</span>
                    )}
                    {Number(d.replicas) > 1 ? (
                      <Button size="sm" variant="outline" onClick={() => setReadOpen(true)}>
                        {readEndpointOn ? "Remove" : "Add"}
                      </Button>
                    ) : (
                      <span className="text-xs text-muted-foreground">(needs more than one instance)</span>
                    )}
                  </div>
                )}
                <dl className="grid gap-x-8 gap-y-3 text-sm sm:grid-cols-2">
                  <DbRow k="Allowed CIDRs" v={((d.allowed_cidrs as string[]) ?? []).join(", ") || "no restriction"} mono />
                </dl>
                <div className="flex flex-wrap gap-2">
                  <Button size="sm" onClick={() => conn.mutate()} disabled={conn.isPending || !!connInfo}>
                    {conn.isPending ? "Fetching\u2026" : "Show connection info"}
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => setCidrsOpen(true)}>Allowed CIDRs</Button>
                  {supports(engine, "RESET_PASSWORD") && (
                    <Button size="sm" variant="outline" onClick={() => setResetOpen(true)}>Reset password</Button>
                  )}
                  {engine === "opensearch" && (
                    <>
                      <Button size="sm" variant="outline" onClick={() => setSsoOpen(true)}>Configure SSO</Button>
                      <Button size="sm" variant="outline" onClick={() => setDomainOpen(true)}>Custom domain</Button>
                      <Button size="sm" variant="outline" onClick={() => setPoliciesOpen(true)}>Index policies</Button>
                    </>
                  )}
                </div>
              </CardContent>
            </Card>
          </TabsContent>

          {supports(engine, "MANAGE_ACCESS") && (
            <TabsContent value="access" className="mt-4">
              <Card>
                <CardHeader><CardTitle className="text-base">{accessLabel(engine)}</CardTitle></CardHeader>
                <CardContent className="grid gap-3">
                  {hasLogicalDatabases(engine) && (
                    <div className="grid gap-1">
                      <div className="text-xs text-muted-foreground">Databases</div>
                      {((d.databases as AccessDatabase[]) ?? []).length === 0 ? (
                        <p className="text-sm text-muted-foreground">None yet.</p>
                      ) : (
                        <div className="flex flex-wrap gap-2">
                          {((d.databases as AccessDatabase[]) ?? []).map((x) => (
                            <Badge key={x.name} variant="outline" className="font-mono">{x.name}</Badge>
                          ))}
                        </div>
                      )}
                    </div>
                  )}
                  <div className="grid gap-1">
                    <div className="text-xs text-muted-foreground">Users</div>
                    {((d.users as AccessUser[]) ?? []).length === 0 ? (
                      <p className="text-sm text-muted-foreground">None yet.</p>
                    ) : (
                      <div className="flex flex-wrap gap-2">
                        {((d.users as AccessUser[]) ?? []).map((u) => (
                          <Badge key={u.name} variant="outline" className="font-mono">{u.login || u.name}</Badge>
                        ))}
                      </div>
                    )}
                  </div>
                  <div>
                    <Button size="sm" onClick={() => setAccessOpen(true)}>Manage</Button>
                  </div>
                </CardContent>
              </Card>
            </TabsContent>
          )}

          {supports(engine, "BACKUP") && backupConfigured && (
            <TabsContent value="backups" className="mt-4">
              <Card>
                <CardHeader><CardTitle className="text-base">Backups</CardTitle></CardHeader>
                <CardContent className="grid gap-3">
                  <dl className="grid gap-x-8 gap-y-3 text-sm sm:grid-cols-2">
                    <DbRow k="Scheduled backups" v={backupScheduleLabel((d.backup as BackupState) ?? {})} />
                    <DbRow k="Retention" v={(d.backup as BackupState)?.retentionDays ? `${(d.backup as BackupState).retentionDays} days` : "kept forever"} />
                  </dl>
                  <p className="text-xs text-muted-foreground">
                    This database archives continuously to the platform object store, so you can
                    back up at any time and recover to a point in time. The schedule only adds
                    unattended full backups on top.
                  </p>
                  <div className="flex flex-wrap gap-2">
                    <Button
                      size="sm"
                      onClick={() =>
                        act("CREATE_BACKUP")
                          .then(() => toast.success("Backup started"))
                          .then(() => backupRuns.mutate())
                          .catch((e: Error) => toast.error(e.message))
                      }
                    >
                      Back up now
                    </Button>
                    <Button size="sm" variant="outline" onClick={() => backupRuns.mutate()} disabled={backupRuns.isPending}>
                      {backupRuns.isPending ? "Loading…" : "Refresh"}
                    </Button>
                    <Button size="sm" variant="outline" onClick={() => setBackupOpen(true)}>
                      Schedule
                    </Button>
                  </div>
                  {/* The history lives HERE rather than behind a dialog: the reason to look at a
                      backup list is almost always to pick one to recover from, so the list and the
                      Restore action belong on the same surface. */}
                  <div className="grid gap-1">
                    <div className="text-xs text-muted-foreground">Backups</div>
                    {runs.length === 0 ? (
                      <p className="text-sm text-muted-foreground">
                        {backupRuns.isPending
                          ? "Loading…"
                          : "No backups yet — the first one runs on the next schedule, or start one now."}
                      </p>
                    ) : (
                      runs.map((b) => (
                        <div key={b.name} className="flex items-center justify-between gap-3 rounded-lg border p-2 text-xs">
                          <span className="truncate font-mono">{b.name}</span>
                          <span className="flex shrink-0 items-center gap-3">
                            <span className="text-muted-foreground">
                              {timeAgo(b.finishedAt || b.startedAt || b.createdAt)}
                            </span>
                            <StatusBadge status={b.phase} />
                            {supports(engine, "RESTORE") ? (
                              <Button
                                size="sm"
                                variant="outline"
                                // Only a finished copy can be recovered from; a running or failed
                                // one would send the customer into a create that cannot succeed.
                                disabled={String(b.phase ?? "").toLowerCase() !== "completed"}
                                onClick={() => {
                                  const q = new URLSearchParams({ restore: String(resource.externalId ?? resource.id), backup: b.name })
                                  navigate(`/p/${pid}/databases?${q.toString()}`)
                                }}
                              >
                                Restore
                              </Button>
                            ) : null}
                          </span>
                        </div>
                      ))
                    )}
                  </div>
                  {supports(engine, "RESTORE") ? (
                    <p className="text-xs text-muted-foreground">
                      Restoring recovers into a NEW database — this one keeps running and is never
                      written to. The recovery replays the archive to its end, so it includes
                      changes made after the backup you pick; the form lets you stop at an earlier
                      point in time instead.
                    </p>
                  ) : null}
                </CardContent>
              </Card>
            </TabsContent>
          )}

          {paramDefs.length > 0 && (
            <TabsContent value="config" className="mt-4">
              <Card>
                <CardHeader><CardTitle className="text-base">Runtime configuration</CardTitle></CardHeader>
                <CardContent className="grid gap-3">
                  {Object.keys((d.parameters as Record<string, string>) ?? {}).length === 0 ? (
                    <p className="text-sm text-muted-foreground">
                      Everything is on the engine defaults.
                    </p>
                  ) : (
                    <dl className="grid gap-x-8 gap-y-2 text-sm sm:grid-cols-2">
                      {Object.entries((d.parameters as Record<string, string>) ?? {}).map(([k, v]) => (
                        <DbRow key={k} k={k} v={String(v)} mono />
                      ))}
                    </dl>
                  )}
                  <div>
                    <Button size="sm" onClick={() => setParamsOpen(true)}>Edit configuration</Button>
                  </div>
                </CardContent>
              </Card>
            </TabsContent>
          )}

          <TabsContent value="logs" className="mt-4">
            <LogsPanel pid={pid} scope={scope} resourceId={resource.id} />
          </TabsContent>
        </Tabs>
      </div>

      {resizeOpen && (
        <ResizeDialog
          cpu={String(d.cpu ?? "")}
          memory={String(d.memory_gib ?? "")}
          onClose={() => setResizeOpen(false)}
          onSubmit={async (cpu, memoryGiB) => {
            await act("RESIZE", { cpu, memoryGiB })
            onPatch({ cpu, memory_gib: memoryGiB, status: "PROGRESSING" })
            toast.success("Resize started")
            setResizeOpen(false)
          }}
        />
      )}

      {storageOpen && (
        <ResizeStorageDialog
          current={Number(d.storage_gib) || 1}
          onClose={() => setStorageOpen(false)}
          onSubmit={async (storageGiB) => {
            await act("RESIZE_STORAGE", { storageGiB })
            onPatch({ storage_gib: storageGiB, status: "PROGRESSING" })
            toast.success("Storage resize started")
            setStorageOpen(false)
          }}
        />
      )}

      {upgradeOpen && (
        <UpgradeVersionDialog
          engine={engine}
          current={String(d.version ?? "")}
          offered={(engines[engine]?.versions ?? []).filter(Boolean)}
          backupsOn={backupsOn}
          onClose={() => setUpgradeOpen(false)}
          onSubmit={async (version, backupFirst) => {
            await act("UPGRADE", { version, backupFirst })
            onPatch({ version, status: "PROGRESSING" })
            toast.success(backupFirst ? "Backup and upgrade started" : "Upgrade started")
            setUpgradeOpen(false)
          }}
        />
      )}

      {replicasOpen && (
        <ScaleReplicasDialog
          current={Number(d.replicas) || 1}
          allowed={allowedReplicas}
          onClose={() => setReplicasOpen(false)}
          onSubmit={async (replicas) => {
            await act("SCALE_REPLICAS", { replicas })
            onPatch({ replicas, status: "PROGRESSING" })
            toast.success("Replica change started")
            setReplicasOpen(false)
          }}
        />
      )}

      {autoscaleOpen && (
        <AutoscaleDialog
          engine={engine}
          d={d}
          limits={limits}
          onClose={() => setAutoscaleOpen(false)}
          onSubmit={async (data) => {
            await act("SET_AUTOSCALE", data)
            onPatch({
              status: "UPDATING",
              autoscale_enabled: data.enabled ? 1 : 0,
              autoscale_max_cpu: data.maxCpu,
              autoscale_max_memory_gib: data.maxMemoryGiB,
              autoscale_max_storage_gib: data.maxStorageGiB,
            })
            toast.success(data.enabled ? "Autoscale enabled" : "Autoscale disabled")
            setAutoscaleOpen(false)
          }}
        />
      )}

      {accessOpen && (
        <AccessDialog
          engine={engine}
          databases={((d.databases as AccessDatabase[]) ?? [])}
          users={((d.users as AccessUser[]) ?? [])}
          roles={((d.os_roles as AccessRole[]) ?? [])}
          onClose={() => setAccessOpen(false)}
          backupsOn={backupsOn}
          onSubmit={async (body) => {
            const res = await act("MANAGE_ACCESS", body)
            onPatch({ databases: body.databases, users: body.users, os_roles: body.roles, sync_status: "OutOfSync" })
            const creds = (res as { credentials?: Record<string, string> }).credentials
            if (creds && Object.keys(creds).length > 0) setNewCredentials(creds)
            toast.success("Databases and users updated")
            setAccessOpen(false)
          }}
          onRotate={async (username) => {
            const res = await act("RESET_USER_PASSWORD", { username })
            const r = res as { username?: string; password?: string }
            if (r.password) setNewCredentials({ [r.username ?? username]: r.password })
            toast.success(`Password rotated for ${username}`)
          }}
        />
      )}

      {backupOpen && (
        <BackupDialog
          pid={pid}
          scope={scope}
          resourceId={resource.id}
          backup={(d.backup as BackupState) ?? {}}
          onClose={() => setBackupOpen(false)}
          onSubmit={async (body) => {
            await act("SET_BACKUP", body)
            onPatch({ backup: { ...(d.backup as BackupState), ...body }, sync_status: "OutOfSync" })
            toast.success(body.enabled ? "Backups updated" : "Backups disabled")
            setBackupOpen(false)
          }}
          onRunNow={async () => {
            await act("CREATE_BACKUP")
            onPatch({ sync_status: "OutOfSync" })
            toast.success("Backup started")
          }}
        />
      )}

      {paramsOpen && (
        <ParametersDialog
          defs={paramDefs}
          current={(d.parameters as Record<string, string>) ?? {}}
          onClose={() => setParamsOpen(false)}
          onSubmit={async (params) => {
            const res = await act("SET_PARAMETERS", { parameters: params })
            onPatch({ parameters: params, sync_status: "OutOfSync" })
            toast.success(
              (res as { restarts?: boolean }).restarts
                ? "Configuration applied — the database will restart"
                : "Configuration applied",
            )
            setParamsOpen(false)
          }}
        />
      )}

      {readOpen && (
        <Dialog open onOpenChange={(o) => !o && setReadOpen(false)}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{readEndpointOn ? "Disable read endpoint" : "Add read endpoint"}</DialogTitle>
              <DialogDescription>
                {readEndpointOn
                  ? "Removes the read-only endpoint. Anything still pointed at it loses its connection."
                  : engine === "postgresql"
                    ? "Adds a second endpoint that serves replicas only, so reads keep off the primary. It runs on its own load balancer, which is billed separately."
                    : "Adds a read-only endpoint on port 3307 of the address you already have. It reuses the same load balancer, so it costs nothing extra."}
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="outline" onClick={() => setReadOpen(false)}>Cancel</Button>
              <Button
                variant={readEndpointOn ? "destructive" : "default"}
                onClick={() =>
                  act("SET_READ_ENDPOINT", { enabled: !readEndpointOn })
                    .then(() => {
                      onPatch({ read_endpoint: !readEndpointOn, sync_status: "OutOfSync" })
                      toast.success(readEndpointOn ? "Read endpoint removed" : "Read endpoint added")
                      setReadOpen(false)
                    })
                    .catch((e: Error) => toast.error(e.message))
                }
              >
                {readEndpointOn ? "Remove" : "Add"}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}

      {policiesOpen && (
        <IndexPoliciesDialog
          policies={((d.index_policies as IndexPolicy[]) ?? [])}
          onClose={() => setPoliciesOpen(false)}
          onSubmit={async (policies) => {
            await act("SET_INDEX_POLICIES", { policies })
            onPatch({ index_policies: policies, sync_status: "OutOfSync" })
            toast.success("Index policies updated")
            setPoliciesOpen(false)
          }}
        />
      )}

      {newCredentials && (
        <NewCredentialsDialog creds={newCredentials} onClose={() => setNewCredentials(null)} />
      )}

      {ssoOpen && (
        <SsoDialog
          onClose={() => setSsoOpen(false)}
          onSubmit={async (data) => {
            await act("SET_SSO", data)
            onPatch({ status: "UPDATING" })
            toast.success("SSO configuration update started")
            setSsoOpen(false)
          }}
        />
      )}

      {domainOpen && (
        <CustomDomainDialog
          onClose={() => setDomainOpen(false)}
          onSubmit={async (data) => {
            await act("SET_CUSTOM_DOMAIN", data)
            onPatch({ status: "UPDATING" })
            toast.success(data.domain ? "Custom domain update started" : "Custom domain removed")
            setDomainOpen(false)
          }}
        />
      )}

      {cidrsOpen && (
        <CidrsDialog
          initial={((d.allowed_cidrs as string[]) ?? []).join(", ")}
          onClose={() => setCidrsOpen(false)}
          onSubmit={async (allowedCidrs) => {
            await act("SET_ALLOWED_CIDRS", { allowedCidrs })
            onPatch({ allowed_cidrs: allowedCidrs })
            toast.success("Allowed CIDRs updated")
            setCidrsOpen(false)
          }}
        />
      )}

      {/* Restart — brief interruption, so confirm */}
      <Dialog open={restartOpen} onOpenChange={setRestartOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Restart database</DialogTitle>
            <DialogDescription>
              Restarts “{name}”. Connections are dropped; with a single replica the database is
              briefly unavailable.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRestartOpen(false)}>Cancel</Button>
            <Button onClick={() => restart.mutate()} disabled={restart.isPending}>
              {restart.isPending ? "Restarting…" : "Restart"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Reset password — invalidates the current credentials, so confirm */}
      <Dialog open={resetOpen} onOpenChange={setResetOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Reset password</DialogTitle>
            <DialogDescription>
              Generates a new password for the database user. Everything using the current
              password stops authenticating — the new one is shown exactly once.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setResetOpen(false)}>Cancel</Button>
            <Button variant="destructive" onClick={() => resetPassword.mutate()} disabled={resetPassword.isPending}>
              {resetPassword.isPending ? "Resetting…" : "Reset password"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* The show-once result of RESET_PASSWORD */}
      <Dialog open={newPassword !== null} onOpenChange={(o) => !o && setNewPassword(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>New password</DialogTitle>
            <DialogDescription>
              Copy it now — it is not stored and cannot be shown again.
            </DialogDescription>
          </DialogHeader>
          <div className="flex items-center gap-2 py-2">
            <Input readOnly className="font-mono text-xs" value={newPassword ?? ""} onFocus={(e) => e.currentTarget.select()} />
            <Button variant="outline" size="icon" aria-label="Copy password" onClick={() => void copyText(newPassword ?? "")}>
              <Copy className="size-4" />
            </Button>
          </div>
          <DialogFooter>
            <Button onClick={() => setNewPassword(null)}>Done</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// ── connection info (masked password + copy; values live only in mutation state) ──
function ConnectionInfoCard({ info, onClose }: { info: ConnectionInfo; onClose: () => void }) {
  const [reveal, setReveal] = useState(false)
  const rows: Array<{ label: string; value: string; mono?: boolean }> = [
    { label: "Host", value: String(info.host ?? ""), mono: true },
    { label: "Port", value: info.port != null ? String(info.port) : "", mono: true },
    { label: "Database", value: String(info.dbname ?? ""), mono: true },
    { label: "Username", value: String(info.username ?? ""), mono: true },
  ]
  return (
    <div className="grid gap-2 rounded-xl border bg-card p-4">
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium">Connection info</span>
        <Button variant="ghost" size="sm" onClick={onClose}>Hide</Button>
      </div>
      {rows.filter((r) => r.value).map((r) => (
        <div key={r.label} className="flex items-center justify-between gap-4 border-b py-1.5 text-sm last:border-b-0">
          <span className="text-muted-foreground">{r.label}</span>
          <span className="flex items-center gap-1">
            <span className={r.mono ? "font-mono text-xs" : ""}>{r.value}</span>
            <Button variant="ghost" size="icon-sm" aria-label={`Copy ${r.label}`} onClick={() => void copyText(r.value)}>
              <Copy className="size-3.5" />
            </Button>
          </span>
        </div>
      ))}
      {info.password ? (
        <div className="flex items-center justify-between gap-4 border-b py-1.5 text-sm last:border-b-0">
          <span className="text-muted-foreground">Password</span>
          <span className="flex items-center gap-1">
            <span className="font-mono text-xs">{reveal ? info.password : "••••••••••••"}</span>
            <Button variant="ghost" size="icon-sm" aria-label={reveal ? "Hide password" : "Reveal password"} onClick={() => setReveal(!reveal)}>
              {reveal ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
            </Button>
            <Button variant="ghost" size="icon-sm" aria-label="Copy password" onClick={() => void copyText(info.password ?? "")}>
              <Copy className="size-3.5" />
            </Button>
          </span>
        </div>
      ) : null}
      {info.uri ? (
        <Button variant="outline" size="sm" className="justify-start" onClick={() => void copyText(info.uri ?? "")}>
          <Check className="size-4" /> Copy connection URI
        </Button>
      ) : null}
    </div>
  )
}

// ── action dialogs ───────────────────────────────────────────────────────────
function ResizeDialog({
  cpu: initialCpu, memory: initialMemory, onClose, onSubmit,
}: {
  cpu: string
  memory: string
  onClose: () => void
  onSubmit: (cpu: number, memoryGiB: number) => Promise<void>
}) {
  const [cpu, setCpu] = useState(initialCpu)
  const [memory, setMemory] = useState(initialMemory)
  const [pending, setPending] = useState(false)
  const valid = Number(cpu) >= 1 && Number(memory) >= 1
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Resize database</DialogTitle>
          <DialogDescription>
            Changes the CPU/memory of every replica. Instances restart with a rolling update;
            with a single replica the database is briefly unavailable.
          </DialogDescription>
        </DialogHeader>
        <div className="grid grid-cols-2 gap-3 py-2">
          <div className="grid gap-2">
            <Label htmlFor="rs-cpu">vCPU</Label>
            <Input id="rs-cpu" type="number" min={1} value={cpu} onChange={(e) => setCpu(e.target.value)} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="rs-mem">Memory (GiB)</Label>
            <Input id="rs-mem" type="number" min={1} value={memory} onChange={(e) => setMemory(e.target.value)} />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            onClick={() => {
              setPending(true)
              onSubmit(Number(cpu), Number(memory))
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
            disabled={!valid || pending}
          >
            {pending ? "Applying…" : "Resize"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ResizeStorageDialog({
  current, onClose, onSubmit,
}: {
  current: number
  onClose: () => void
  onSubmit: (storageGiB: number) => Promise<void>
}) {
  const [storage, setStorage] = useState(String(current))
  const [pending, setPending] = useState(false)
  // Grow-only is STRICT server-side — a value equal to current is always rejected.
  const valid = Number(storage) > current
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Resize storage</DialogTitle>
          <DialogDescription>
            Grows the database volume online. Storage can only grow — shrinking is not possible.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-2 py-2">
          <Label htmlFor="rs-storage">Storage (GiB, current: {current})</Label>
          <Input id="rs-storage" type="number" min={current + 1} value={storage} onChange={(e) => setStorage(e.target.value)} />
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            onClick={() => {
              setPending(true)
              onSubmit(Number(storage))
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
            disabled={!valid || pending}
          >
            {pending ? "Applying…" : "Resize storage"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function UpgradeVersionDialog({
  engine, current, offered, backupsOn, onClose, onSubmit,
}: {
  engine: string
  current: string
  offered: string[]
  backupsOn: boolean
  onClose: () => void
  onSubmit: (version: string, backupFirst: boolean) => Promise<void>
}) {
  // Only strictly newer catalog versions are valid targets — the server rejects anything else.
  const targets = offered.filter((v) => versionGt(v, current))
  const [sel, setSel] = useState("")
  const version = targets.includes(sel) ? sel : targets[0] ?? ""
  const [pending, setPending] = useState(false)
  // Default ON for the one action that cannot be undone.
  const [backupFirst, setBackupFirst] = useState(backupsOn)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Upgrade version</DialogTitle>
          <DialogDescription>
            Upgrades {engineLabel(engine)} from version {current} to a newer offered version.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-2 py-2">
          <Label>Target version (current: {current})</Label>
          {targets.length === 0 ? (
            <p className="text-sm text-muted-foreground">Already on the newest offered version.</p>
          ) : (
            <Select value={version} onValueChange={setSel}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {targets.map((v) => (
                  <SelectItem key={v} value={v}>{v}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          <p className="text-xs text-muted-foreground">
            The database pods roll onto the new engine image; expect a brief failover. The
            endpoint does not change. Downgrades are not possible.
          </p>
        </div>
        {backupsOn && (
          <BackupFirstToggle on={backupFirst} setOn={setBackupFirst} what="the upgrade" />
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            onClick={() => {
              setPending(true)
              onSubmit(version, backupFirst && backupsOn)
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
            disabled={!version || pending}
          >
            {pending ? "Upgrading…" : "Upgrade"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ScaleReplicasDialog({
  current, allowed, onClose, onSubmit,
}: {
  current: number
  allowed: number[]
  onClose: () => void
  onSubmit: (replicas: number) => Promise<void>
}) {
  const [sel, setSel] = useState(String(current))
  const [pending, setPending] = useState(false)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Scale replicas</DialogTitle>
          <DialogDescription>
            Changes the number of database instances. Scaling to 1 removes the standby replicas —
            the data itself is kept.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-2 py-2">
          <Label>Replicas (current: {current})</Label>
          <Select value={sel} onValueChange={setSel}>
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {allowed.map((n) => (
                <SelectItem key={n} value={String(n)}>
                  {n === 1 ? "1 — single node" : `${n} — high availability`}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            onClick={() => {
              setPending(true)
              onSubmit(Number(sel))
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
            disabled={Number(sel) === current || pending}
          >
            {pending ? "Applying…" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function CidrsDialog({
  initial, onClose, onSubmit,
}: {
  initial: string
  onClose: () => void
  onSubmit: (allowedCidrs: string[]) => Promise<void>
}) {
  const [cidrs, setCidrs] = useState(initial)
  const [pending, setPending] = useState(false)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Allowed CIDRs</DialogTitle>
          <DialogDescription>
            Source ranges allowed to reach the endpoint, comma-separated. Empty = no restriction.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-2 py-2">
          <Input
            className="font-mono"
            value={cidrs}
            onChange={(e) => setCidrs(e.target.value)}
            placeholder="192.0.2.0/24, 198.51.100.7/32"
            aria-label="Allowed CIDRs"
          />
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            onClick={() => {
              setPending(true)
              onSubmit(cidrs.split(",").map((c) => c.trim()).filter(Boolean))
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
            disabled={pending}
          >
            {pending ? "Applying…" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// All engines — opt-in vertical autoscale (SET_AUTOSCALE) with customer-set ceilings.
// Ceilings sit in [current size, provider databaseLimits]; the disk leg only exists where
// RESIZE_STORAGE does (valkey/opensearch hide it), and 0/empty storage = disk leg off.
function AutoscaleDialog({
  engine, d, limits, onClose, onSubmit,
}: {
  engine: string
  d: Db
  limits?: DatabaseLimits
  onClose: () => void
  onSubmit: (data: { enabled: boolean; maxCpu: number; maxMemoryGiB: number; maxStorageGiB: number }) => Promise<void>
}) {
  const curCpu = Number(d.cpu) || 1
  const curMemory = Number(d.memory_gib) || 1
  const curStorage = Number(d.storage_gib) || 0
  const capCpu = limits?.maxCpu || 0
  const capMemory = limits?.maxMemoryGiB || 0
  const capStorage = limits?.maxStorageGiB || 0
  const hasDisk = supports(engine, "RESIZE_STORAGE")

  const [enabled, setEnabled] = useState(Number(d.autoscale_enabled) === 1)
  const [maxCpu, setMaxCpu] = useState(String(Number(d.autoscale_max_cpu) || curCpu))
  const [maxMemory, setMaxMemory] = useState(String(Number(d.autoscale_max_memory_gib) || curMemory))
  const [maxStorage, setMaxStorage] = useState(Number(d.autoscale_max_storage_gib) ? String(d.autoscale_max_storage_gib) : "")
  const [pending, setPending] = useState(false)

  const cpuOk = Number(maxCpu) >= curCpu && (!capCpu || Number(maxCpu) <= capCpu)
  const memoryOk = Number(maxMemory) >= curMemory && (!capMemory || Number(maxMemory) <= capMemory)
  const storageN = Number(maxStorage) || 0 // 0/empty = disk leg off
  const storageOk = !hasDisk || storageN === 0 || (storageN >= curStorage && (!capStorage || storageN <= capStorage))
  const valid = !enabled || (cpuOk && memoryOk && storageOk)

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Autoscale</DialogTitle>
          <DialogDescription>
            Scales up automatically within your limits (VPA-guided, never scales down; storage
            grows at 80% full). Billed at the size actually scaled to, plus a small hourly fee
            while enabled.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 py-2">
          <div className="flex items-center justify-between">
            <Label htmlFor="as-enabled">Enable autoscale</Label>
            <Switch id="as-enabled" checked={enabled} onCheckedChange={setEnabled} />
          </div>
          {enabled && (
            <>
              <div className="grid grid-cols-2 gap-3">
                <div className="grid gap-2">
                  <Label htmlFor="as-cpu">Max vCPU (current: {curCpu})</Label>
                  <Input
                    id="as-cpu" type="number" min={curCpu} max={capCpu || undefined}
                    value={maxCpu} onChange={(e) => setMaxCpu(e.target.value)}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="as-mem">Max RAM (GiB, current: {curMemory})</Label>
                  <Input
                    id="as-mem" type="number" min={curMemory} max={capMemory || undefined}
                    value={maxMemory} onChange={(e) => setMaxMemory(e.target.value)}
                  />
                </div>
              </div>
              {!cpuOk && (
                <p className="text-xs text-destructive">
                  Max vCPU must be at least the current {curCpu}{capCpu ? ` and at most ${capCpu}` : ""}.
                </p>
              )}
              {!memoryOk && (
                <p className="text-xs text-destructive">
                  Max RAM must be at least the current {curMemory} GiB{capMemory ? ` and at most ${capMemory} GiB` : ""}.
                </p>
              )}
              {hasDisk && (
                <div className="grid gap-2">
                  <Label htmlFor="as-storage">Max storage (GiB, current: {curStorage})</Label>
                  <Input
                    id="as-storage" type="number" min={0} max={capStorage || undefined}
                    value={maxStorage} onChange={(e) => setMaxStorage(e.target.value)}
                    placeholder="0 — storage autoscale off"
                  />
                  {!storageOk ? (
                    <p className="text-xs text-destructive">
                      Max storage must be 0 (off) or at least the current {curStorage} GiB
                      {capStorage ? ` and at most ${capStorage} GiB` : ""}.
                    </p>
                  ) : (
                    <p className="text-xs text-muted-foreground">Empty or 0 leaves storage autoscale off.</p>
                  )}
                </div>
              )}
            </>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            onClick={() => {
              setPending(true)
              onSubmit({
                enabled,
                maxCpu: Number(maxCpu) || 0,
                maxMemoryGiB: Number(maxMemory) || 0,
                maxStorageGiB: hasDisk ? storageN : 0,
              })
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
            disabled={!valid || pending}
          >
            {pending ? "Applying…" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

type AccessDatabase = { name: string; owner?: string }
type AccessRole = { name: string; indexPatterns?: string[]; actions?: string[] }
type IndexPolicy = {
  name: string
  indexPatterns?: string[]
  deleteAfterDays?: number
  rolloverGiB?: number
  rolloverDays?: number
}

// OpenSearch ACTION GROUPS a custom role may grant — mirrors osActionGroups server-side.
// Action groups, never raw action strings: "indices:admin/*" is a management grant wearing an
// index permission costume.
const OPENSEARCH_ACTIONS = ["read", "search", "write", "crud", "create_index", "delete", "manage"]
type AccessUser = { name: string; login?: string; databases?: string[]; roles?: string[] }

// Built-in OpenSearch roles a customer may bind — mirrors the server-side allowlist in
// internal/cloud/dbaas/config.go openSearchRoles. Kept short: these are the ones that mean
// something without a custom role definition.
const OPENSEARCH_ROLES = [
  "all_access", "readall", "readall_and_monitor", "kibana_user", "kibana_read_only",
  "alerting_read_access", "alerting_full_access", "index_management_full_access",
]

// AccessDialog edits the WHOLE desired access list — the request is declarative, so anything
// removed here is removed on the database. Names are validated client-side with the same rule
// the server enforces (it is a SQL-safety gate there, not a nicety, so the message matches).
const IDENT_RE = /^[a-z][a-z0-9_]{0,30}$/

function AccessDialog({
  engine, databases, users, roles, backupsOn, onClose, onSubmit, onRotate,
}: {
  engine: string
  databases: AccessDatabase[]
  users: AccessUser[]
  roles: AccessRole[]
  onClose: () => void
  backupsOn: boolean
  onSubmit: (body: { databases: AccessDatabase[]; users: AccessUser[]; roles: AccessRole[]; backupFirst?: boolean }) => Promise<void>
  onRotate: (username: string) => Promise<void>
}) {
  const isSearch = engine === "opensearch"
  // valkey: ACL users only — one keyspace, nothing to grant per database and no roles.
  const usersOnly = engine === "valkey"
  const [dbs, setDbs] = useState<AccessDatabase[]>(databases)
  const [us, setUs] = useState<AccessUser[]>(users)
  const [rs, setRs] = useState<AccessRole[]>(roles)
  const [newDb, setNewDb] = useState("")
  const [newUser, setNewUser] = useState("")
  const [newRole, setNewRole] = useState("")
  const [newRolePatterns, setNewRolePatterns] = useState("")
  const [newRoleActions, setNewRoleActions] = useState<string[]>(["read"])
  const [pending, setPending] = useState(false)
  const [backupFirst, setBackupFirst] = useState(true)

  const existing = new Set(users.map((u) => u.name))
  // Removing a database DROPS it — that is the only shape of this change that loses data, so
  // the safety backup is offered exactly then rather than on every rename.
  const removesData = databases.some((d) => !dbs.find((x) => x.name === d.name))
  const addDb = () => {
    const name = newDb.trim()
    if (!IDENT_RE.test(name)) return toast.error("Use lower-case letters, digits and underscores; start with a letter")
    if (dbs.some((d) => d.name === name)) return toast.error(`${name} already exists`)
    setDbs([...dbs, { name }])
    setNewDb("")
  }
  const addUser = () => {
    const name = newUser.trim()
    if (!IDENT_RE.test(name)) return toast.error("Use lower-case letters, digits and underscores; start with a letter")
    if (us.some((u) => u.name === name)) return toast.error(`${name} already exists`)
    setUs([...us, { name, databases: [], roles: [] }])
    setNewUser("")
  }
  const addRole = () => {
    const name = newRole.trim()
    const patterns = newRolePatterns.split(",").map((v) => v.trim()).filter(Boolean)
    if (!IDENT_RE.test(name)) return toast.error("Use lower-case letters, digits and underscores; start with a letter")
    if (OPENSEARCH_ROLES.includes(name)) return toast.error(`${name} is a built-in role name`)
    if (rs.some((r) => r.name === name)) return toast.error(`${name} already exists`)
    if (patterns.length === 0) return toast.error("At least one index pattern is required")
    if (newRoleActions.length === 0) return toast.error("At least one permission is required")
    setRs([...rs, { name, indexPatterns: patterns, actions: newRoleActions }])
    setNewRole("")
    setNewRolePatterns("")
  }
  const toggle = (user: AccessUser, key: "databases" | "roles", value: string) => {
    const list = user[key] ?? []
    const next = list.includes(value) ? list.filter((v) => v !== value) : [...list, value]
    setUs(us.map((u) => (u.name === user.name ? { ...u, [key]: next } : u)))
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{accessLabel(engine)}</DialogTitle>
          <DialogDescription>
            {isSearch
              ? "Logins on this OpenSearch cluster and the built-in roles bound to them. New users get a password once, shown after you apply."
              : usersOnly
                ? "Additional accounts that can authenticate to this instance. New users get a password once, shown after you apply."
                : "Logical databases inside this instance and the logins that may use them. New users get a password once, shown after you apply."}
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-5 py-2">
          {!isSearch && !usersOnly && (
            <div className="grid gap-2">
              <Label>Databases</Label>
              {dbs.length === 0 ? (
                <p className="text-xs text-muted-foreground">No databases yet.</p>
              ) : (
                dbs.map((db) => (
                  <div key={db.name} className="flex items-center justify-between rounded-lg border p-2">
                    <div className="font-mono text-sm">{db.name}</div>
                    <div className="flex items-center gap-2">
                      <span className="text-xs text-muted-foreground">
                        owner {db.owner || "app"}
                      </span>
                      <Button size="sm" variant="ghost" onClick={() => setDbs(dbs.filter((d) => d.name !== db.name))}>
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </div>
                ))
              )}
              <div className="flex gap-2">
                <Input value={newDb} onChange={(e) => setNewDb(e.target.value)} placeholder="orders" className="font-mono" />
                <Button variant="outline" onClick={addDb}>Add database</Button>
              </div>
            </div>
          )}

          {isSearch && (
            <div className="grid gap-2">
              <Label>Custom roles</Label>
              {rs.length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  None. Built-in roles cover the common cases; add one to scope access to your own
                  index patterns.
                </p>
              ) : (
                rs.map((r) => (
                  <div key={r.name} className="flex items-center justify-between rounded-lg border p-2">
                    <div>
                      <div className="font-mono text-sm">{r.name}</div>
                      <div className="text-xs text-muted-foreground">
                        {(r.indexPatterns ?? []).join(", ")} · {(r.actions ?? []).join(", ")}
                      </div>
                    </div>
                    <Button size="sm" variant="ghost" onClick={() => setRs(rs.filter((x) => x.name !== r.name))}>
                      <Trash2 className="size-4" />
                    </Button>
                  </div>
                ))
              )}
              <div className="grid gap-2 rounded-lg border p-3">
                <div className="grid gap-2 sm:grid-cols-2">
                  <Input value={newRole} onChange={(e) => setNewRole(e.target.value)} placeholder="logs_reader" className="font-mono" />
                  <Input value={newRolePatterns} onChange={(e) => setNewRolePatterns(e.target.value)} placeholder="logs-*, app-*" className="font-mono" />
                </div>
                <div className="flex flex-wrap gap-2">
                  {OPENSEARCH_ACTIONS.map((a) => (
                    <Button
                      key={a}
                      size="sm"
                      variant={newRoleActions.includes(a) ? "default" : "outline"}
                      onClick={() =>
                        setNewRoleActions(
                          newRoleActions.includes(a)
                            ? newRoleActions.filter((v) => v !== a)
                            : [...newRoleActions, a],
                        )
                      }
                    >
                      {a}
                    </Button>
                  ))}
                </div>
                <Button variant="outline" onClick={addRole}>Add role</Button>
              </div>
            </div>
          )}

          <div className="grid gap-2">
            <Label>Users</Label>
            {us.length === 0 ? (
              <p className="text-xs text-muted-foreground">No users yet.</p>
            ) : (
              us.map((u) => (
                <div key={u.name} className="grid gap-2 rounded-lg border p-3">
                  <div className="flex items-center justify-between">
                    <div>
                      <div className="font-mono text-sm">{u.name}</div>
                      {u.login && u.login !== u.name ? (
                        <div className="text-xs text-muted-foreground">
                          logs in as <span className="font-mono">{u.login}</span>
                        </div>
                      ) : null}
                    </div>
                    <div className="flex items-center gap-2">
                      {existing.has(u.name) ? (
                        <Button size="sm" variant="outline" onClick={() => onRotate(u.name).catch((e: Error) => toast.error(e.message))}>
                          Rotate password
                        </Button>
                      ) : (
                        <span className="text-xs text-muted-foreground">new</span>
                      )}
                      <Button size="sm" variant="ghost" onClick={() => setUs(us.filter((x) => x.name !== u.name))}>
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </div>
                  {!usersOnly && (
                    <div className="flex flex-wrap gap-2">
                      {(isSearch ? [...OPENSEARCH_ROLES, ...rs.map((r) => r.name)] : dbs.map((d) => d.name)).map((value) => {
                        const key = isSearch ? "roles" : "databases"
                        const on = (u[key] ?? []).includes(value)
                        return (
                          <Button
                            key={value}
                            size="sm"
                            variant={on ? "default" : "outline"}
                            onClick={() => toggle(u, key, value)}
                          >
                            {value}
                          </Button>
                        )
                      })}
                      {!isSearch && dbs.length === 0 ? (
                        <span className="text-xs text-muted-foreground">Add a database to grant access.</span>
                      ) : null}
                    </div>
                  )}
                </div>
              ))
            )}
            <div className="flex gap-2">
              <Input value={newUser} onChange={(e) => setNewUser(e.target.value)} placeholder="alice" className="font-mono" />
              <Button variant="outline" onClick={addUser}>Add user</Button>
            </div>
          </div>

          {backupsOn && removesData ? (
            <BackupFirstToggle on={backupFirst} setOn={setBackupFirst} what="the change" />
          ) : null}

          <p className="text-xs text-muted-foreground">
            {usersOnly
              ? "Removing a user here removes it on the instance. Passwords are stored only on the database cluster; rotate to get a new one."
              : "Removing an entry here removes it on the database — for a database that also drops its data. Passwords are stored only on the database cluster; rotate to get a new one."}
          </p>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            disabled={pending}
            onClick={() => {
              setPending(true)
              const payload = {
                databases: isSearch || usersOnly ? [] : dbs,
                roles: isSearch ? rs : [],
                ...(backupsOn && removesData && backupFirst ? { backupFirst: true } : {}),
                users: us.map((u) => ({
                  name: u.name,
                  // valkey users carry neither grants nor roles — the server rejects both.
                  ...(usersOnly ? {} : isSearch ? { roles: u.roles ?? [] } : { databases: u.databases ?? [] }),
                })),
              }
              onSubmit(payload)
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
          >
            {pending ? "Applying…" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// Shown once per newly created (or rotated) user — the only time these passwords exist
// outside the database cluster. Never written to the query cache.
function NewCredentialsDialog({ creds, onClose }: { creds: Record<string, string>; onClose: () => void }) {
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New credentials</DialogTitle>
          <DialogDescription>
            Copy these now — they are shown once and are not stored anywhere you can read them
            back. Rotating a user issues a new password.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-2 py-2">
          {Object.entries(creds).map(([user, password]) => (
            <div key={user} className="grid gap-1 rounded-lg border p-3">
              <div className="text-xs text-muted-foreground">{user}</div>
              <div className="flex gap-2">
                <Input readOnly className="font-mono text-xs" value={password} onFocus={(e) => e.currentTarget.select()} />
                <Button variant="outline" size="icon" aria-label={`Copy password for ${user}`} onClick={() => void copyText(password)}>
                  <Copy className="size-4" />
                </Button>
              </div>
            </div>
          ))}
        </div>
        <DialogFooter>
          <Button onClick={onClose}>Done</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

type ParamDef = {
  name: string
  kind: string
  help?: string
  min?: string
  max?: string
  enum?: string[]
  restart?: boolean
}
type LogEntry = { pod: string; container?: string; log?: string; error?: string }

type BackupState = { enabled?: boolean; schedule?: string; retentionDays?: number }

// The opt-in safety backup offered before the two actions that can lose data. Deliberately NOT
// a promise that the change waits for the backup: gating on completion would let one stuck
// backup block every later change to the database. Continuous archiving is the real guarantee —
// recovery lands on the second before the change either way; the extra base backup just
// shortens the replay.
function BackupFirstToggle({
  on, setOn, what,
}: {
  on: boolean
  setOn: (v: boolean) => void
  what: string
}) {
  return (
    <div className="flex items-center justify-between rounded-lg border p-3">
      <div>
        <Label htmlFor="bf-toggle" className="text-sm font-medium">Take a backup first</Label>
        <div className="text-xs text-muted-foreground">
          Starts a backup alongside {what}. Recovery to any moment before this already works
          while backups are on; this just makes it faster.
        </div>
      </div>
      <Switch id="bf-toggle" checked={on} onCheckedChange={setOn} />
    </div>
  )
}
// One row of backup history, as ListBackups normalises it across the three operators.
type BackupRun = {
  name: string
  phase?: string
  createdAt?: string
  startedAt?: string
  finishedAt?: string
  error?: string
}

// What a "Restore this backup" click carries into the create form.
//
// Deliberately NOT a recovery time. Prefilling the backup's finish time looks right and loses
// data twice over: <input type="datetime-local"> has minute precision, so the instant is rounded
// DOWN — below the backup itself — and even at full precision it would throw away every write
// made after that backup, which continuous archiving still holds. Recovering with no target
// replays the archive to its end, i.e. the backup AND everything after it, which is what someone
// clicking Restore on a database that is still running wants. A specific point in time stays
// available as an explicit choice in the form.
//
// backupName is for mysql, the one engine that recovers by naming a backup object.
type InitialRestore = { sourceId: string; backupName?: string }

// Common schedules, so nobody has to know cron. The value is the 5-FIELD form the server
// stores; CNPG's six-field arity is the chart's problem, never the customer's.
//
// Weekly is the DEFAULT and it is listed first for that reason. Each run is a full copy of the
// database, so a daily schedule at the same retention is seven times the object store; the log
// ships continuously either way, so recovery still reaches any second in the window.
const BACKUP_SCHEDULES = [
  { value: "0 2 * * 0", label: "Weekly (Sunday 02:00)" },
  { value: "0 2 * * *", label: "Daily at 02:00" },
  { value: "0 * * * *", label: "Hourly" },
  { value: "", label: "On demand only" },
]

// Backups: posture + history. The object store is operator config — nothing here names a
// bucket, and the customer cannot point backups anywhere else.
function BackupDialog({
  pid, scope, resourceId, backup, onClose, onSubmit, onRunNow,
}: {
  pid: string
  scope: CloudScope | undefined
  resourceId: string
  backup: BackupState
  onClose: () => void
  onSubmit: (body: { enabled: boolean; schedule: string; retentionDays: number }) => Promise<void>
  onRunNow: () => Promise<void>
}) {
  // The store is always wired; this switch is the SCHEDULE, so it starts from whether one exists.
  const [enabled, setEnabled] = useState(!!backup.schedule)
  const [schedule, setSchedule] = useState(backup.schedule || "0 2 * * 0")
  const [retention, setRetention] = useState(String(backup.retentionDays ?? 30))
  const [pending, setPending] = useState(false)

  // The list is read live off the database cluster, not from the cache: someone reading it is
  // usually deciding what to restore, and a stale answer there is worse than a slow one.
  const list = useMutation({
    mutationFn: () =>
      apiFetch<{ result?: BackupRun[] }>(`/project/${pid}/cloud/${resourceId}/action`, {
        method: "POST",
        body: { action: "LIST_BACKUPS", data: {} },
        cloud: scope,
      }),
    onError: (e: Error) => toast.error(e.message),
  })
  const runs = (list.data?.result ?? []) as BackupRun[]
  useEffect(() => {
    list.mutate()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Backups</DialogTitle>
          <DialogDescription>
            This database already archives continuously to the platform object store — on-demand
            backups and point-in-time recovery work without any of this. What you set here is the
            unattended schedule and how long backups are kept.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          <div className="flex items-center justify-between rounded-lg border p-3">
            <div>
              <Label htmlFor="bk-enabled" className="text-sm font-medium">Scheduled backups</Label>
              <div className="text-xs text-muted-foreground">
                Turning this off only stops the unattended runs. Continuous archiving keeps going,
                so you can still back up on demand and recover to a point in time.
              </div>
            </div>
            <Switch id="bk-enabled" checked={enabled} onCheckedChange={setEnabled} />
          </div>

          {enabled && (
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="grid gap-2">
                <Label>Schedule</Label>
                <Select value={schedule} onValueChange={setSchedule}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {BACKUP_SCHEDULES.map((s) => (
                      <SelectItem key={s.value || "ondemand"} value={s.value}>{s.label}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-xs text-muted-foreground">
                  Each run is a full copy, so a more frequent schedule costs proportionally more
                  storage. Recovery reaches any point in time either way.
                </p>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="bk-ret">Keep for (days)</Label>
                <Input id="bk-ret" value={retention} onChange={(e) => setRetention(e.target.value)} inputMode="numeric" placeholder="30" />
                <p className="text-xs text-muted-foreground">0 keeps backups forever.</p>
              </div>
            </div>
          )}

          {(
            <div className="grid gap-2">
              <div className="flex items-center justify-between">
                <Label>Recent backups</Label>
                <div className="flex gap-2">
                  <Button size="sm" variant="outline" onClick={() => list.mutate()} disabled={list.isPending}>
                    {list.isPending ? "Loading…" : "Refresh"}
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => onRunNow().then(() => list.mutate()).catch((e: Error) => toast.error(e.message))}
                  >
                    Back up now
                  </Button>
                </div>
              </div>
              {runs.length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  {list.isPending ? "Loading…" : "No backups yet — the first one runs on the next schedule, or start one now."}
                </p>
              ) : (
                <div className="grid gap-1">
                  {runs.slice(0, 10).map((b) => (
                    <div key={b.name} className="flex items-center justify-between rounded-lg border p-2 text-xs">
                      <span className="font-mono">{b.name}</span>
                      <span className="flex items-center gap-3">
                        <span className="text-muted-foreground">{timeAgo(b.finishedAt || b.startedAt || b.createdAt)}</span>
                        <StatusBadge status={b.phase} />
                      </span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            disabled={pending}
            onClick={() => {
              const days = Number(retention)
              if (!Number.isInteger(days) || days < 0 || days > 3650) {
                return toast.error("Keep for must be 0–3650 days")
              }
              setPending(true)
              onSubmit({ enabled: true, schedule: enabled ? schedule : "", retentionDays: days })
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
          >
            {pending ? "Applying…" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// One definition row, matching the server detail page's layout.
function DbRow({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="grid gap-0.5">
      <dt className="text-xs text-muted-foreground">{k}</dt>
      <dd className={mono ? "font-mono text-xs break-all" : "text-sm"}>{v}</dd>
    </div>
  )
}

// Human wording for the stored 5-field cron, so the summary does not make the customer read
// cron to find out when their backups run.
function backupScheduleLabel(b: BackupState): string {
  if (!b.schedule) return "On demand only"
  const found = BACKUP_SCHEDULES.find((s) => s.value === b.schedule)
  return found ? found.label : b.schedule
}

// The database's own log, read on demand from the engine pods' stdout and stored nowhere —
// the same posture as connection info. A PANEL rather than a dialog: reading a log is what
// someone does while looking at everything else, not a modal errand.
function LogsPanel({
  pid, scope, resourceId,
}: {
  pid: string
  scope: CloudScope | undefined
  resourceId: string
}) {
  const [lines, setLines] = useState("200")
  const logs = useMutation({
    mutationFn: (n: number) =>
      apiFetch<{ result?: LogEntry[] }>(`/project/${pid}/cloud/${resourceId}/action`, {
        method: "POST",
        body: { action: "GET_LOGS", data: { lines: n } },
        cloud: scope,
      }),
    onError: (e: Error) => toast.error(e.message),
  })
  const entries = logs.data?.result ?? []
  useEffect(() => {
    logs.mutate(200)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <Card>
      <CardHeader><CardTitle className="text-base">Logs</CardTitle></CardHeader>
      <CardContent>
        <div className="grid gap-3">
          <p className="text-xs text-muted-foreground">
            The database engine's own log, newest lines last. One block per instance.
          </p>
          <div className="flex items-center gap-2">
            <Label htmlFor="lg-lines" className="text-xs">Lines</Label>
            <Input id="lg-lines" className="w-28" value={lines} onChange={(e) => setLines(e.target.value)} inputMode="numeric" />
            <Button size="sm" variant="outline" onClick={() => logs.mutate(Number(lines) || 200)} disabled={logs.isPending}>
              {logs.isPending ? "Loading…" : "Refresh"}
            </Button>
          </div>
          {entries.length === 0 && !logs.isPending ? (
            <p className="text-xs text-muted-foreground">No log output yet.</p>
          ) : null}
          {entries.map((e) => (
            <div key={e.pod} className="grid gap-1">
              <div className="font-mono text-xs text-muted-foreground">{e.pod}</div>
              <pre className="max-h-96 overflow-auto rounded-lg border bg-muted/40 p-3 text-xs leading-relaxed whitespace-pre-wrap">
                {e.error ? `— ${e.error}` : e.log || "(empty)"}
              </pre>
            </div>
          ))}
        </div>
      </CardContent>
    </Card>
  )
}

// Runtime configuration — the managed-database equivalent of an RDS parameter group. The
// catalog comes from the server, so the form can never offer a setting the allowlist rejects,
// and each row says up front whether changing it restarts the database.
function ParametersDialog({
  defs, current, onClose, onSubmit,
}: {
  defs: ParamDef[]
  current: Record<string, string>
  onClose: () => void
  onSubmit: (params: Record<string, string>) => Promise<void>
}) {
  const [vals, setVals] = useState<Record<string, string>>(current)
  const [pending, setPending] = useState(false)
  const set = (name: string, v: string) => setVals({ ...vals, [name]: v })
  const clear = (name: string) => {
    const next = { ...vals }
    delete next[name]
    setVals(next)
  }
  const willRestart = defs.some((d) => d.restart && (vals[d.name] ?? "") !== (current[d.name] ?? ""))

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Configuration</DialogTitle>
          <DialogDescription>
            Engine settings for this database. Anything left empty uses the engine default.
            Settings marked <span className="font-medium">restart</span> roll the instances when
            they change; the rest apply in place.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3 py-2">
          {defs.map((d) => (
            <div key={d.name} className="grid gap-1 rounded-lg border p-3">
              <div className="flex items-center justify-between gap-3">
                <Label htmlFor={`p-${d.name}`} className="font-mono text-sm">{d.name}</Label>
                <div className="flex items-center gap-2">
                  {d.restart ? <Badge variant="outline">restart</Badge> : null}
                  {vals[d.name] !== undefined ? (
                    <Button size="sm" variant="ghost" onClick={() => clear(d.name)}>Reset</Button>
                  ) : null}
                </div>
              </div>
              {d.enum ? (
                <Select value={vals[d.name] ?? ""} onValueChange={(v) => set(d.name, v)}>
                  <SelectTrigger id={`p-${d.name}`}><SelectValue placeholder="engine default" /></SelectTrigger>
                  <SelectContent>
                    {d.enum.map((v) => <SelectItem key={v} value={v}>{v}</SelectItem>)}
                  </SelectContent>
                </Select>
              ) : (
                <Input
                  id={`p-${d.name}`}
                  className="font-mono"
                  value={vals[d.name] ?? ""}
                  onChange={(e) => set(d.name, e.target.value)}
                  placeholder={d.min && d.max ? `engine default · ${d.min}–${d.max}` : "engine default"}
                />
              )}
              {d.help ? <p className="text-xs text-muted-foreground">{d.help}</p> : null}
            </div>
          ))}
        </div>
        <DialogFooter>
          {willRestart ? (
            <span className="mr-auto text-xs text-muted-foreground">
              These changes restart the database.
            </span>
          ) : null}
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            disabled={pending}
            onClick={() => {
              setPending(true)
              const clean: Record<string, string> = {}
              for (const [k, v] of Object.entries(vals)) if (v.trim() !== "") clean[k] = v.trim()
              onSubmit(clean)
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
          >
            {pending ? "Applying…" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// OpenSearch only — index retention (ISM), narrowed to the shape customers ask for. The whole
// list is sent every time, so removing a row removes the policy.
function IndexPoliciesDialog({
  policies, onClose, onSubmit,
}: {
  policies: IndexPolicy[]
  onClose: () => void
  onSubmit: (policies: IndexPolicy[]) => Promise<void>
}) {
  const [ps, setPs] = useState<IndexPolicy[]>(policies)
  const [name, setName] = useState("")
  const [patterns, setPatterns] = useState("")
  const [keepDays, setKeepDays] = useState("30")
  const [rolloverGiB, setRolloverGiB] = useState("")
  const [rolloverDays, setRolloverDays] = useState("")
  const [pending, setPending] = useState(false)

  const add = () => {
    const n = name.trim()
    const pats = patterns.split(",").map((v) => v.trim()).filter(Boolean)
    const keep = Number(keepDays)
    const rgb = Number(rolloverGiB) || 0
    const rdays = Number(rolloverDays) || 0
    if (!IDENT_RE.test(n)) return toast.error("Use lower-case letters, digits and underscores; start with a letter")
    if (ps.some((p) => p.name === n)) return toast.error(`${n} already exists`)
    if (pats.length === 0) return toast.error("At least one index pattern is required")
    if (!Number.isInteger(keep) || keep < 1 || keep > 3650) return toast.error("Keep for must be 1–3650 days")
    if (rdays && rdays >= keep) return toast.error("Roll over age must be less than the keep period")
    setPs([...ps, {
      name: n,
      indexPatterns: pats,
      deleteAfterDays: keep,
      ...(rgb ? { rolloverGiB: rgb } : {}),
      ...(rdays ? { rolloverDays: rdays } : {}),
    }])
    setName("")
    setPatterns("")
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Index policies</DialogTitle>
          <DialogDescription>
            Retention for indices matching a pattern: keep them for a period, then delete them.
            Optionally roll the write index over first, so the deletion falls on whole indices
            instead of documents. Applies to existing indices as well as new ones.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          {ps.length === 0 ? (
            <p className="text-xs text-muted-foreground">No policies yet — indices are kept forever.</p>
          ) : (
            ps.map((p) => (
              <div key={p.name} className="flex items-center justify-between rounded-lg border p-3">
                <div>
                  <div className="font-mono text-sm">{p.name}</div>
                  <div className="text-xs text-muted-foreground">
                    {(p.indexPatterns ?? []).join(", ")} · keep {p.deleteAfterDays}d
                    {p.rolloverGiB ? ` · roll at ${p.rolloverGiB} GiB` : ""}
                    {p.rolloverDays ? ` · roll at ${p.rolloverDays}d` : ""}
                  </div>
                </div>
                <Button size="sm" variant="ghost" onClick={() => setPs(ps.filter((x) => x.name !== p.name))}>
                  <Trash2 className="size-4" />
                </Button>
              </div>
            ))
          )}

          <div className="grid gap-2 rounded-lg border p-3">
            <div className="grid gap-2 sm:grid-cols-2">
              <div className="grid gap-1">
                <Label htmlFor="ip-name" className="text-xs">Name</Label>
                <Input id="ip-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="logs30" className="font-mono" />
              </div>
              <div className="grid gap-1">
                <Label htmlFor="ip-pat" className="text-xs">Index patterns</Label>
                <Input id="ip-pat" value={patterns} onChange={(e) => setPatterns(e.target.value)} placeholder="logs-*, app-*" className="font-mono" />
              </div>
            </div>
            <div className="grid gap-2 sm:grid-cols-3">
              <div className="grid gap-1">
                <Label htmlFor="ip-keep" className="text-xs">Keep for (days)</Label>
                <Input id="ip-keep" value={keepDays} onChange={(e) => setKeepDays(e.target.value)} inputMode="numeric" />
              </div>
              <div className="grid gap-1">
                <Label htmlFor="ip-rgb" className="text-xs">Roll over at (GiB)</Label>
                <Input id="ip-rgb" value={rolloverGiB} onChange={(e) => setRolloverGiB(e.target.value)} inputMode="numeric" placeholder="optional" />
              </div>
              <div className="grid gap-1">
                <Label htmlFor="ip-rd" className="text-xs">Roll over at (days)</Label>
                <Input id="ip-rd" value={rolloverDays} onChange={(e) => setRolloverDays(e.target.value)} inputMode="numeric" placeholder="optional" />
              </div>
            </div>
            <Button variant="outline" onClick={add}>Add policy</Button>
          </div>

          <p className="text-xs text-muted-foreground">
            Deletion is permanent and runs unattended — check the patterns before applying.
          </p>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            disabled={pending}
            onClick={() => {
              setPending(true)
              onSubmit(ps)
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
          >
            {pending ? "Applying…" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// OpenSearch only — bring-your-own domain + certificate (SET_CUSTOM_DOMAIN). No ACME: the
// customer certifies their own name and points DNS at the endpoint themselves. Submitting an
// empty domain removes the custom domain.
function CustomDomainDialog({
  onClose, onSubmit,
}: {
  onClose: () => void
  onSubmit: (data: { domain: string; certPem?: string; keyPem?: string; caPem?: string }) => Promise<void>
}) {
  const [domain, setDomain] = useState("")
  const [certPem, setCertPem] = useState("")
  const [keyPem, setKeyPem] = useState("")
  const [caPem, setCaPem] = useState("")
  const [pending, setPending] = useState(false)
  const removing = domain.trim() === ""
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Custom domain</DialogTitle>
          <DialogDescription>
            Serve the OpenSearch API and Dashboards on your own domain with your own TLS
            certificate. Point your DNS at the endpoint (an A record to the endpoint IP). Submit with an empty domain to remove.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 py-2">
          <div className="grid gap-2">
            <Label htmlFor="cd-domain">Domain</Label>
            <Input id="cd-domain" className="font-mono" value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="search.example.com" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cd-cert">Certificate (PEM, full chain)</Label>
            <Textarea id="cd-cert" className="min-h-24 font-mono text-xs" value={certPem} onChange={(e) => setCertPem(e.target.value)} placeholder={"-----BEGIN CERTIFICATE-----"} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cd-key">Private key (PEM)</Label>
            <Textarea id="cd-key" className="min-h-24 font-mono text-xs" value={keyPem} onChange={(e) => setKeyPem(e.target.value)} placeholder={"-----BEGIN PRIVATE KEY-----"} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cd-ca">CA chain (PEM, optional)</Label>
            <Textarea id="cd-ca" className="min-h-16 font-mono text-xs" value={caPem} onChange={(e) => setCaPem(e.target.value)} placeholder={"-----BEGIN CERTIFICATE-----"} />
          </div>
          <p className="text-xs text-muted-foreground">
            The key is stored only on the database cluster, never in the platform. Rotate by
            submitting the same domain with a new certificate.
          </p>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            variant={removing ? "destructive" : "default"}
            disabled={pending || (!removing && (!certPem.trim() || !keyPem.trim()))}
            onClick={() => {
              setPending(true)
              const data = removing
                ? { domain: "" }
                : { domain: domain.trim(), certPem: certPem.trim(), keyPem: keyPem.trim(), ...(caPem.trim() ? { caPem: caPem.trim() } : {}) }
              onSubmit(data)
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
          >
            {pending ? "Applying…" : removing ? "Remove domain" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// OpenSearch only — writes the Dashboards OIDC SSO block (SET_SSO). Submitting all fields
// empty disables SSO; the server validates the rest.
function SsoDialog({
  onClose, onSubmit,
}: {
  onClose: () => void
  onSubmit: (data: {
    connectUrl: string; clientId: string; scope: string; baseRedirectUrl: string; logoutUrl: string
  }) => Promise<void>
}) {
  const [connectUrl, setConnectUrl] = useState("")
  const [clientId, setClientId] = useState("")
  const [scope, setScope] = useState("openid profile email")
  const [baseRedirectUrl, setBaseRedirectUrl] = useState("")
  const [logoutUrl, setLogoutUrl] = useState("")
  const [pending, setPending] = useState(false)
  const fields = [
    { id: "sso-connect", label: "OIDC discovery URL", value: connectUrl, set: setConnectUrl, placeholder: "https://idp.example.com/realms/main/.well-known/openid-configuration" },
    { id: "sso-client", label: "Client ID", value: clientId, set: setClientId, placeholder: "opensearch-dashboards" },
    { id: "sso-scope", label: "Scope", value: scope, set: setScope, placeholder: "openid profile email" },
    { id: "sso-redirect", label: "Base redirect URL", value: baseRedirectUrl, set: setBaseRedirectUrl, placeholder: "https://dashboards.example.com" },
    { id: "sso-logout", label: "Logout URL", value: logoutUrl, set: setLogoutUrl, placeholder: "https://idp.example.com/logout" },
  ]
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Configure SSO</DialogTitle>
          <DialogDescription>
            OpenID Connect single sign-on for OpenSearch Dashboards. Submit with all fields empty
            to disable SSO.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 py-2">
          {fields.map((f) => (
            <div key={f.id} className="grid gap-2">
              <Label htmlFor={f.id}>{f.label}</Label>
              <Input id={f.id} value={f.value} onChange={(e) => f.set(e.target.value)} placeholder={f.placeholder} />
            </div>
          ))}
          <p className="text-xs text-muted-foreground">
            Register a PUBLIC OIDC client (PKCE) in your identity provider — no client secret is
            stored.
          </p>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            onClick={() => {
              setPending(true)
              onSubmit({ connectUrl: connectUrl.trim(), clientId: clientId.trim(), scope: scope.trim(), baseRedirectUrl: baseRedirectUrl.trim(), logoutUrl: logoutUrl.trim() })
                .catch((e: Error) => toast.error(e.message))
                .finally(() => setPending(false))
            }}
            disabled={pending}
          >
            {pending ? "Applying…" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
