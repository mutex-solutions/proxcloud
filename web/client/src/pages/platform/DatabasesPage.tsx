// Managed Databases (dbaas provider) — list/create/manage database clusters. The cloud scope
// for cluster CRUD is a DBAAS location (the create wizard offers a picker when the project has
// more than one); the engine/version catalog comes from the dbaas service DTO. The database
// runs on platform-owned infrastructure — only the tenant network for the private endpoint is
// the customer's. Actions map to Go cloud_dbaas.go: create/delete + GET_CONNECTION_INFO /
// RESIZE / RESIZE_STORAGE / SCALE_REPLICAS / RESTART / RESET_PASSWORD / SET_ALLOWED_CIDRS /
// UPGRADE / SET_AUTOSCALE.
import { useCallback, useMemo, useState } from "react"
import { useNavigate, useParams } from "react-router-dom"
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
import { Button } from "@/components/ui/button"
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
import { Switch } from "@/components/ui/switch"
import { apiFetch, type CloudScope } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudList, useLocations, useProjectId, useProjectServices } from "@/lib/hooks"
import type { CloudResource, DatabaseEngineOffer, DatabaseLimits, Location } from "@/lib/types"
import { isPrivateNetwork, networkName } from "../network/NetworksPage"

type Db = Record<string, any>
// A pickable cluster network: one of the project's own VPCs plus its subnets. Unlike the
// Kubernetes wizard there is NO dedicated-network fallback — the database endpoint is an
// internal LB in a tenant subnet, so a network+subnet pick is required.
type NetworkOption = {
  id: string
  name: string
  subnets: { id: string; label: string }[]
}

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
  UPGRADE: new Set(["valkey", "ferretdb"]),
  RESTART: new Set(["mysql", "valkey", "opensearch", "kafka"]),
  RESIZE_STORAGE: new Set(["valkey", "opensearch"]),
  RESET_PASSWORD: new Set(["valkey", "opensearch"]),
}
const supports = (engine: string, action: string) => !HIDDEN_ACTIONS[action]?.has(engine)

// Size presets (cpu / memory GiB) — no flavors here, the DB runs on platform-owned nodes.
const SIZE_PRESETS = [
  { key: "S", label: "Small", cpu: 1, memoryGiB: 2 },
  { key: "M", label: "Medium", cpu: 2, memoryGiB: 8 },
  { key: "L", label: "Large", cpu: 4, memoryGiB: 16 },
] as const

function database(r: CloudResource): Db {
  return (r.data?.database as Db) ?? {}
}

// The endpoint stays "" until the Octavia VIP is programmed — READY implies it exists, so the
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

function sizeOf(d: Db): string {
  const parts = [
    d.cpu != null ? `${d.cpu} vCPU` : "",
    d.memory_gib != null ? `${d.memory_gib} GiB` : "",
    d.storage_gib != null ? `${d.storage_gib} GiB disk` : "",
  ].filter(Boolean)
  const n = Number(d.replicas) || 0
  return parts.join(" · ") + (n > 1 ? ` × ${n}` : "")
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
  const createSvc = serviceFor(createLoc?.serviceId)

  // The project's own networks + subnets (cached NETWORK/SUBNET resources, default cloud scope =
  // the OpenStack service) feed the REQUIRED network picker — the database's private endpoint
  // lands on the chosen subnet. Only private VPCs.
  const networksQ = useCloudList(pid, "NETWORK")
  const subnetsQ = useCloudList(pid, "SUBNET")
  const networkOptions = useMemo<NetworkOption[]>(() => {
    const subsByNet = new Map<string, { id: string; label: string }[]>()
    for (const r of subnetsQ.data ?? []) {
      const s = (r.data?.subnet ?? {}) as Record<string, unknown>
      const netId = (s.network_id as string) ?? ""
      const id = (s.id as string) || r.externalId
      if (!netId || !id) continue
      const label = [s.name as string, s.cidr as string].filter(Boolean).join(" · ") || id
      subsByNet.set(netId, [...(subsByNet.get(netId) ?? []), { id, label }])
    }
    return (networksQ.data ?? [])
      .filter(isPrivateNetwork)
      .map((r): NetworkOption => {
        const id = ((r.data?.network as Record<string, unknown> | undefined)?.id as string) || r.externalId || ""
        return { id, name: networkName(r), subnets: subsByNet.get(id) ?? [] }
      })
      .filter((n) => !!n.id && n.subnets.length > 0) // a network with no subnet cannot host the endpoint
  }, [networksQ.data, subnetsQ.data])

  const [createOpen, setCreateOpen] = useState(false)
  const [toDelete, setToDelete] = useState<CloudResource | null>(null)

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
        description="Managed databases — provisioned and operated for you, reachable over a private endpoint in your network."
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
          hint="Create a managed database — pick an engine, a size and one of your networks; the platform runs it for you."
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
          engines={createEngines}
          limits={createSvc?.databaseLimits}
          storageClasses={createSvc?.databaseStorageClasses}
          networks={networkOptions}
          locations={dLocs}
          locKey={createLoc ? locKeyOf(createLoc) : ""}
          onLocKey={setCreateLocKey}
          onClose={() => setCreateOpen(false)}
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
  engines, limits, storageClasses, networks, locations, locKey, onLocKey, onClose, onSubmit,
}: {
  engines: Record<string, DatabaseEngineOffer>
  limits?: DatabaseLimits
  storageClasses?: string[]
  networks: NetworkOption[]
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
  const [engineSel, setEngineSel] = useState("")
  const engine = engineKeys.includes(engineSel) ? engineSel : engineKeys[0] ?? ""
  const offer = engines[engine] ?? {}
  const versions = (offer.versions ?? []).filter(Boolean)
  const [versionSel, setVersionSel] = useState("")
  const version = versions.includes(versionSel) ? versionSel : offer.default && versions.includes(offer.default) ? offer.default : versions[0] ?? ""
  const allowedReplicas = offer.replicas?.length ? offer.replicas : [1, 3]
  const [replicasSel, setReplicasSel] = useState("")
  const replicas = allowedReplicas.includes(Number(replicasSel)) ? Number(replicasSel) : allowedReplicas[0]

  const [preset, setPreset] = useState<string>("S")
  const [cpu, setCpu] = useState(String(SIZE_PRESETS[0].cpu))
  const [memory, setMemory] = useState(String(SIZE_PRESETS[0].memoryGiB))
  const [storage, setStorage] = useState("20")
  const [storageClass, setStorageClass] = useState("") // "" = cluster default (no key sent)
  const [allowedCidrs, setAllowedCidrs] = useState("")
  const [networkId, setNetworkId] = useState("")
  const [subnetId, setSubnetId] = useState("")
  const selectedNetwork = networks.find((n) => n.id === networkId)
  const [pending, setPending] = useState(false)

  const pickPreset = (key: string) => {
    setPreset(key)
    const p = SIZE_PRESETS.find((x) => x.key === key)
    if (p) {
      setCpu(String(p.cpu))
      setMemory(String(p.memoryGiB))
    }
  }

  // The network pick is REQUIRED (no dedicated-network fallback) — the endpoint is an internal
  // LB on the chosen subnet.
  const networkValid = !!networkId && !!subnetId && !!selectedNetwork?.subnets.some((s) => s.id === subnetId)
  // Provider size limits (service DTO databaseLimits) — 0/absent means unbounded.
  const maxCpu = limits?.maxCpu || 0
  const maxMemory = limits?.maxMemoryGiB || 0
  const maxStorage = limits?.maxStorageGiB || 0
  const cpuOk = Number(cpu) >= 1 && (!maxCpu || Number(cpu) <= maxCpu)
  const memoryOk = Number(memory) >= 1 && (!maxMemory || Number(memory) <= maxMemory)
  const storageOk = Number(storage) >= 1 && (!maxStorage || Number(storage) <= maxStorage)
  const valid = !!name.trim() && !!engine && !!version && networkValid && cpuOk && memoryOk && storageOk

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
        networkId,
        subnetId,
        ...(storageClass ? { storageClass } : {}),
        ...(allowedCidrs.trim()
          ? { allowedCidrs: allowedCidrs.split(",").map((c) => c.trim()).filter(Boolean) }
          : {}),
        // The server refuses a beta engine without the explicit acknowledgement.
        ...(offer.beta ? { beta: true } : {}),
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
            The database is provisioned and operated by the platform and reachable only over a
            private endpoint in the network you pick below.
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

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="grid gap-2">
              <Label htmlFor="db-network">Network</Label>
              <Select
                value={networkId || undefined}
                onValueChange={(v) => {
                  setNetworkId(v)
                  setSubnetId("") // a subnet from the previous network is meaningless here
                }}
              >
                <SelectTrigger id="db-network">
                  <SelectValue placeholder={networks.length ? "Pick a network" : "No networks with a subnet found"} />
                </SelectTrigger>
                <SelectContent>
                  {networks.map((n) => (
                    <SelectItem key={n.id} value={n.id}>{n.name}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                The database's private endpoint is created on this network — only workloads that
                can reach the subnet can connect.
              </p>
            </div>
            {selectedNetwork && (
              <div className="grid gap-2">
                <Label htmlFor="db-subnet">Subnet</Label>
                <Select value={subnetId} onValueChange={setSubnetId}>
                  <SelectTrigger id="db-subnet">
                    <SelectValue placeholder="Pick a subnet" />
                  </SelectTrigger>
                  <SelectContent>
                    {selectedNetwork.subnets.map((s) => (
                      <SelectItem key={s.id} value={s.id}>{s.label}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            )}
          </div>

          <div className="grid gap-2">
            <Label htmlFor="db-cidrs">Allowed CIDRs (optional, comma-separated)</Label>
            <Input
              id="db-cidrs"
              className="font-mono"
              value={allowedCidrs}
              onChange={(e) => setAllowedCidrs(e.target.value)}
              placeholder="10.0.0.0/24, 10.0.1.7/32"
            />
            <p className="text-xs text-muted-foreground">
              Restricts which source addresses may reach the endpoint. Empty = the whole network.
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
  pid, scope, resource, engines, limits, platformVersion, onDeleted, onPatch,
}: {
  pid: string
  scope: CloudScope | undefined
  resource: CloudResource
  engines: Record<string, DatabaseEngineOffer>
  limits?: DatabaseLimits
  platformVersion: string
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
  const [autoscaleOpen, setAutoscaleOpen] = useState(false)
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
            <div className="flex items-center gap-1.5">
              <StatusBadge status={(d.status as string) || undefined} />
              {Number(d.autoscale_enabled) === 1 && <Badge variant="outline">Autoscale</Badge>}
            </div>
          </div>
          <div className="rounded-lg border bg-card p-3">
            <div className="text-xs text-muted-foreground">Engine</div>
            <div className="text-sm">
              {engineLabel(engine) || "—"} <span className="font-mono text-xs">{String(d.version ?? "")}</span>
            </div>
          </div>
          <div className="rounded-lg border bg-card p-3">
            <div className="text-xs text-muted-foreground">Size</div>
            <div className="text-sm">{sizeOf(d) || "—"}</div>
          </div>
          <div className="rounded-lg border bg-card p-3">
            <div className="text-xs text-muted-foreground">Sync</div>
            <div className="text-sm">{(d.sync_status as string) || "—"}</div>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">Endpoint:</span>
          {endpoint ? (
            <>
              <span className="font-mono text-xs">{endpoint}</span>
              <Button variant="ghost" size="icon-sm" aria-label="Copy endpoint" onClick={() => void copyText(endpoint)}>
                <Copy className="size-3.5" />
              </Button>
            </>
          ) : (
            <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Loader2 className="size-3 animate-spin" /> Endpoint pending — appears once the load balancer is programmed
            </span>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          {resource.region ? (
            <>Location: <span className="font-medium">{resource.region}</span></>
          ) : null}
          {(d.network_id as string) ? (
            <>
              {resource.region ? " · " : ""}Network: <span className="font-mono">{d.network_id as string}</span>
              {(d.subnet_id as string) ? <> · subnet <span className="font-mono">{d.subnet_id as string}</span></> : null}
            </>
          ) : null}
          {(d.storage_class as string) ? <> · storage class <span className="font-mono">{d.storage_class as string}</span></> : null}
          {(d.chart_version as string) ? (
            <>
              {" "}· platform <span className="font-mono">{d.chart_version as string}</span>
              {platformVersion && platformVersion !== d.chart_version ? <> (provider pin {platformVersion})</> : null}
            </>
          ) : null}
        </p>

        <div className="flex flex-wrap gap-2">
          <Button size="sm" onClick={() => conn.mutate()} disabled={conn.isPending || !!connInfo}>
            {conn.isPending ? "Fetching…" : "Show connection info"}
          </Button>
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
          {supports(engine, "RESET_PASSWORD") && (
            <Button size="sm" variant="outline" onClick={() => setResetOpen(true)}>Reset password</Button>
          )}
          {engine === "opensearch" && (
            <Button size="sm" variant="outline" onClick={() => setSsoOpen(true)}>Configure SSO</Button>
          )}
          <Button size="sm" variant="outline" onClick={() => setCidrsOpen(true)}>Allowed CIDRs</Button>
          <Button size="sm" variant="destructive" onClick={onDeleted}>
            <Trash2 className="size-4" /> Delete
          </Button>
        </div>

        {busy && (
          <div className="flex items-center gap-2 rounded-xl border bg-card p-4">
            <Loader2 className="size-4 animate-spin text-muted-foreground" />
            <span className="text-sm">
              Working on it — this page refreshes itself. The endpoint appears once the internal
              load balancer is programmed (a few minutes on first create).
            </span>
          </div>
        )}

        {connInfo && <ConnectionInfoCard info={connInfo} onClose={() => conn.reset()} />}

        {((d.allowed_cidrs as string[]) ?? []).length > 0 && (
          <p className="text-xs text-muted-foreground">
            Allowed CIDRs:{" "}
            <span className="font-mono">{((d.allowed_cidrs as string[]) ?? []).join(", ")}</span>
          </p>
        )}
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
          onClose={() => setUpgradeOpen(false)}
          onSubmit={async (version) => {
            await act("UPGRADE", { version })
            onPatch({ version, status: "PROGRESSING" })
            toast.success("Upgrade started")
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
  engine, current, offered, onClose, onSubmit,
}: {
  engine: string
  current: string
  offered: string[]
  onClose: () => void
  onSubmit: (version: string) => Promise<void>
}) {
  // Only strictly newer catalog versions are valid targets — the server rejects anything else.
  const targets = offered.filter((v) => versionGt(v, current))
  const [sel, setSel] = useState("")
  const version = targets.includes(sel) ? sel : targets[0] ?? ""
  const [pending, setPending] = useState(false)
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
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button
            onClick={() => {
              setPending(true)
              onSubmit(version)
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
            Source ranges allowed to reach the endpoint, comma-separated. Empty = the whole network.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-2 py-2">
          <Input
            className="font-mono"
            value={cidrs}
            onChange={(e) => setCidrs(e.target.value)}
            placeholder="10.0.0.0/24, 10.0.1.7/32"
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
