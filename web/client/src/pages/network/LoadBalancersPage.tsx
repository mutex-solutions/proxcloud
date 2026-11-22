import { useEffect, useMemo, useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { ArrowLeft, Plus, RefreshCcw, RefreshCw, Scale, Trash2, TriangleAlert } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch, type CloudScope } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudList, useCloudScope, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

// node: a worker of a Kubernetes cluster, named by its address alone.
interface Member { serverId: string; port: number; weight?: number; address?: string; missing?: boolean; node?: boolean }
// interval and timeout in seconds, rise and fall in checks; absent = the load balancer's own (3 s, 2 and 2).
interface HealthCheck { type: string; port?: number; path?: string; interval?: number; timeout?: number; rise?: number; fall?: number }
// protocol: what the members are spoken to in. proxyVersion 1 or 2 (absent = 2). The timeouts are milliseconds.
interface Pool {
  name: string; protocol: string; algorithm: string; members: Member[]; healthCheck?: HealthCheck | null
  proxyProtocol?: boolean; proxyVersion?: number; sessionPersistence?: { type: string; timeout?: number } | null
  timeoutMemberConnect?: number; timeoutMemberData?: number
}
// protocol: what clients are spoken to in; absent on a listener made before it existed, which has its pool's.
// vip: the one address a cluster Service's listener binds.
interface Listener {
  port: number; protocol?: string; pool: string; sourceRanges?: string[] | null; vip?: string
  connectionLimit?: number; timeoutClientData?: number
}
// One VM of a load balancer with high availability, as the last apply saw it.
interface Appliance { serverId: string; node?: string; privateIp?: string; role?: string; holdsVip?: boolean; state?: string; reason?: string }
// A Kubernetes Service on a cluster's balancer, as the cluster's controller last sent it.
interface ClusterService { uid: string; namespace?: string; name?: string; vip?: string; state: string; reason?: string; message?: string }
export interface LB {
  id: string; name: string; status: string; statusMessage?: string; serverId?: string; networkId: string
  publicIp?: string; privateIp?: string; listeners?: Listener[] | null; pools?: Pool[] | null
  ha?: boolean; appliances?: Appliance[] | null; replacing?: string
  // The balancer of a Kubernetes cluster's Services: made and changed by the cluster, deleted with it.
  cluster?: { id: string; services?: ClusterService[] | null }
}
export interface Health { pool: string; serverId: string; address?: string; port: number; status: string }

const lbOf = (r: CloudResource) => (r.data?.loadbalancer ?? {}) as LB

// The appliances of a pair: where each runs and which one holds the addresses, and what one node means for them.
export function ApplianceList({ appliances }: { appliances: Appliance[] }) {
  const nodes = new Set(appliances.map((a) => a.node).filter(Boolean))
  return (
    <>
      {!appliances.length && <p className="text-muted-foreground">The two appliances are being built.</p>}
      {appliances.map((a) => (
        <div key={a.serverId} className="flex flex-wrap items-center gap-3">
          <span className="font-mono">VM {a.serverId}</span>
          <span className="text-muted-foreground">node {a.node || "—"}</span>
          <StatusBadge status={a.state ?? "unknown"} />
          {a.holdsVip ? <span className="font-medium">holds the address</span> : <span className="text-muted-foreground">standby</span>}
          {a.reason && <span className="text-muted-foreground">{a.reason}</span>}
        </div>
      ))}
      {appliances.length > 1 && nodes.size === 1 && (
        <p className="text-muted-foreground" data-testid="lb-one-node">
          Both appliances run on node {[...nodes][0]}: the load balancer survives an appliance failure, not the loss of the node.
        </p>
      )}
    </>
  )
}

// A cluster balancer's Services: each one's address, its ports and how many of the cluster's workers answer its health
// check (with externalTrafficPolicy Local, only the workers that run one of its pods).
export function ClusterServices({ lb, health }: { lb: LB; health?: Health[] }) {
  const services = lb.cluster?.services ?? []
  if (!services.length) return <p className="text-sm text-muted-foreground">No Service of type LoadBalancer is on this load balancer.</p>
  return (
    <Table>
      <TableHeader>
        <TableRow className="hover:bg-transparent">
          <TableHead>Service</TableHead>
          <TableHead>Address</TableHead>
          <TableHead>Ports</TableHead>
          <TableHead>Workers</TableHead>
          <TableHead>State</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {services.map((s) => {
          const listeners = (lb.listeners ?? []).filter((l) => s.vip && l.vip === s.vip)
          const members = listeners.flatMap((l) => ((lb.pools ?? []).find((p) => p.name === l.pool)?.members ?? []).map((m) => ({ ...m, pool: l.pool })))
          const workers = [...new Set(members.map((m) => m.address))]
          const up = workers.filter((a) => members.some((m) => m.address === a &&
            health?.find((h) => h.pool === m.pool && h.address === m.address && h.port === m.port)?.status?.startsWith("UP"))).length
          return (
            <TableRow key={s.uid}>
              <TableCell className="font-mono text-xs">{s.namespace}/{s.name}</TableCell>
              <TableCell className="font-mono text-sm">{s.vip || "—"}</TableCell>
              <TableCell className="text-sm">{listeners.map((l) => l.port).join(", ") || "—"}</TableCell>
              <TableCell className="text-sm">{!workers.length ? "—" : health ? `${up} of ${workers.length} up` : `${workers.length}`}</TableCell>
              <TableCell className="max-w-sm whitespace-normal text-sm">
                <StatusBadge status={s.state} />
                {s.reason && <span className="ml-2 text-muted-foreground">{s.reason}: {s.message}</span>}
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
const BUSY = new Set(["BUILD"])
// The listener protocols a load balancer serves today.
const PROTOCOLS = [{ value: "tcp", label: "TCP" }, { value: "http", label: "HTTP" }]
const CHECKS = [
  { value: "tcp", label: "TCP connection" }, { value: "http", label: "HTTP GET" }, { value: "https", label: "HTTPS GET" },
  { value: "tls-hello", label: "TLS hello" }, { value: "none", label: "None" },
]
// A number field that may be empty: empty is "the load balancer's own", sent as no field at all. The API takes whole
// numbers only, and 1.1 s is not 1100 ms before it is rounded.
const numberOf = (v: string, scale = 1) => {
  const n = Math.round(Number(v) * scale)
  return n > 0 ? n : undefined
}

function serverName(s: CloudResource) {
  return (s.data?.server?.name as string) ?? s.externalId ?? s.id
}

function action<T>(pid: string, id: string, scope: CloudScope | undefined, name: string, data?: object) {
  return apiFetch<T>(`/project/${pid}/cloud/${id}/action`, { method: "POST", body: { action: name, data: data ?? {} }, cloud: scope })
}

export default function LoadBalancersPage() {
  const pid = useProjectId()
  const scope = useCloudScope(pid)
  const navigate = useNavigate()
  const qc = useQueryClient()
  const list = useCloudList(pid, "LOAD_BALANCER", "", 15000)
  const rows = useMemo(() => (list.data ?? []) as CloudResource[], [list.data])
  const [open, setOpen] = useState(false)
  // What this site lacks for a load balancer (no public network for the project, no address left, no appliance
  // image). Only the operator can change these, so the page says so here instead of failing at create.
  const warnings = useQuery({
    queryKey: ["lb-warnings", pid, scope?.serviceId],
    queryFn: () =>
      apiFetch<{ result?: string[] }>(`/project/${pid}/cloud/action`, {
        method: "POST", cloud: scope, body: { action: "LIST_LOAD_BALANCER_WARNINGS" },
      }),
    enabled: !!pid && !!scope,
    select: (d) => (Array.isArray(d?.result) ? d.result.filter((w): w is string => typeof w === "string") : []),
  })
  const blocked = (warnings.data?.length ?? 0) > 0

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(() => [
    {
      id: "name",
      accessorFn: (r) => lbOf(r).name,
      header: sortableHeader("Name"),
      cell: ({ row, getValue }) => (
        <>
          <Link className="inline-block py-1 font-medium hover:underline" to={`/p/${pid}/load-balancers/${row.original.id}`} onClick={(e) => e.stopPropagation()}>
            {getValue()}
          </Link>
          {lbOf(row.original).cluster && <span className="ml-2 text-xs text-muted-foreground">Kubernetes cluster {lbOf(row.original).cluster?.id}</span>}
        </>
      ),
    },
    { id: "status", accessorFn: (r) => lbOf(r).status, header: sortableHeader("Status"), cell: ({ getValue }) => <StatusBadge status={getValue()} /> },
    { id: "ip", accessorFn: (r) => lbOf(r).publicIp ?? "", header: "Public address", cell: ({ getValue }) => <span className="font-mono text-sm">{getValue() || "—"}</span> },
    { id: "listeners", accessorFn: (r) => (lbOf(r).listeners ?? []).map((l) => l.port).join(", "), header: "Listeners", cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "None"}</span> },
    { id: "ha", accessorFn: (r) => (lbOf(r).ha ? "Two appliances" : "One VM"), header: "Runs on", cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue()}</span> },
    { id: "created", accessorFn: (r) => r.createdAt ?? "", header: sortableHeader("Created"), cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span> },
  ], [pid])

  return (
    <>
      <PageHeader
        title="Load balancers"
        eyebrow="Network"
        description="A public address that spreads traffic across servers on one of your networks."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void list.refetch()} disabled={list.isFetching} aria-label="Refresh">
              <RefreshCw className={list.isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setOpen(true)} disabled={blocked}><Plus className="size-4" /> Create load balancer</Button>
          </>
        }
      />
      {blocked && (
        <Alert className="mb-4" data-testid="lb-site-warning">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>A load balancer cannot be created on this site yet</AlertTitle>
          <AlertDescription>
            <ul className="list-disc pl-4">
              {warnings.data?.map((w) => <li key={w}>{w}</li>)}
            </ul>
          </AlertDescription>
        </Alert>
      )}
      {!list.isLoading && !list.error && !rows.length ? (
        <EmptyState
          icon={Scale}
          title="No load balancers yet"
          hint="Create one, then add listeners and the servers behind them."
          action={<Button onClick={() => setOpen(true)} disabled={blocked}><Plus className="size-4" /> Create load balancer</Button>}
        />
      ) : (
        <DataTable columns={columns} data={rows} isLoading={list.isLoading} error={list.error as Error | null} pagination={false} onRowClick={(r) => navigate(`/p/${pid}/load-balancers/${r.id}`)} />
      )}
      <CreateDialog
        open={open}
        onOpenChange={setOpen}
        pid={pid}
        scope={scope}
        onCreated={(r) => {
          void qc.invalidateQueries({ queryKey: ["cloud", pid, "LOAD_BALANCER"] })
          navigate(`/p/${pid}/load-balancers/${r.id}`)
        }}
      />
    </>
  )
}

function CreateDialog({ open, onOpenChange, pid, scope, onCreated }: {
  open: boolean; onOpenChange: (o: boolean) => void; pid: string; scope?: CloudScope; onCreated: (r: CloudResource) => void
}) {
  const networks = useCloudList(pid, "NETWORK")
  const publicNets = useQuery({
    queryKey: ["public-networks", pid, scope?.serviceId],
    queryFn: () => apiFetch<Array<{ id: string; name?: string }>>(`/project/${pid}/public-networks`, { cloud: scope }),
    enabled: open && !!scope,
  })
  const flavors = useQuery({
    queryKey: ["lb-flavors", pid, scope?.serviceId],
    queryFn: async () => {
      const r = await apiFetch<{ result?: Array<{ data?: { id: string; name?: string } }> }>(`/project/${pid}/cloud/action`, {
        method: "POST", body: { type: "SERVER", action: "LIST_FLAVORS" }, cloud: scope,
      })
      return (r.result ?? []).flatMap((f) => (f.data ? [f.data] : []))
    },
    enabled: open && !!scope,
  })
  const [name, setName] = useState("")
  const [networkId, setNetworkId] = useState("")
  const [publicNetworkId, setPublicNetworkId] = useState("")
  const [flavorId, setFlavorId] = useState("")
  const [ha, setHa] = useState(true)
  const create = useMutation({
    mutationFn: () => apiFetch<CloudResource>(`/project/${pid}/cloud`, {
      method: "POST", cloud: scope,
      body: { type: "LOAD_BALANCER", data: { name, networkId, publicNetworkId, flavorId, ha } },
    }),
    onSuccess: (r) => {
      toast.success(`Load balancer "${name}" is being created`)
      onOpenChange(false)
      setName("")
      onCreated(r)
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const nameOk = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/.test(name)
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create load balancer</DialogTitle>
          <DialogDescription>
            It runs on its own small VMs, billed like any server with its disk, plus its public address, and counted in your quota.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 py-2">
          <div className="grid gap-2">
            <Label htmlFor="lb-name">Name</Label>
            <Input id="lb-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="web" />
            {name && !nameOk && <p className="text-xs text-destructive">Use lowercase letters, digits and dashes, at most 32 characters.</p>}
          </div>
          <div className="grid gap-2">
            <Label>Network</Label>
            <Select value={networkId} onValueChange={setNetworkId}>
              <SelectTrigger aria-label="Network"><SelectValue placeholder="The network of your servers" /></SelectTrigger>
              <SelectContent>
                {((networks.data ?? []) as CloudResource[]).map((n) => (
                  <SelectItem key={n.id} value={n.id}>{(n.data?.networkName as string) || (n.data?.network?.name as string) || n.id}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label>Public network</Label>
            <Select value={publicNetworkId} onValueChange={setPublicNetworkId}>
              <SelectTrigger aria-label="Public network"><SelectValue placeholder={publicNets.data?.length === 0 ? "None available to this project" : "Select a public network"} /></SelectTrigger>
              <SelectContent>
                {(publicNets.data ?? []).map((n) => <SelectItem key={n.id} value={n.id}>{n.name ?? n.id}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label>Size</Label>
            <Select value={flavorId} onValueChange={setFlavorId}>
              <SelectTrigger aria-label="Size"><SelectValue placeholder="Select a flavor" /></SelectTrigger>
              <SelectContent>
                {(flavors.data ?? []).map((f) => <SelectItem key={f.id} value={f.id}>{f.name ?? f.id}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          <div className="flex items-start justify-between gap-4 rounded-md border p-3">
            <div className="grid gap-1">
              <Label htmlFor="lb-ha">High availability</Label>
              <p className="text-xs text-muted-foreground" data-testid="lb-ha-cost">
                {ha
                  ? "Two appliance VMs share the address: when one fails, the other takes over within seconds. Both are billed as servers of this size, with their disks."
                  : "One appliance VM, billed as a server of this size with its disk. When it fails, the address is down until it is back."}
              </p>
            </div>
            <Switch id="lb-ha" checked={ha} onCheckedChange={setHa} />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button onClick={() => create.mutate()} disabled={!nameOk || !networkId || !publicNetworkId || !flavorId || create.isPending}>
            {create.isPending ? "Creating…" : "Create load balancer"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function LoadBalancerDetailPage() {
  const pid = useProjectId()
  const scope = useCloudScope(pid)
  const { resourceId = "" } = useParams()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const row = useQuery({
    queryKey: ["cloud-resource", pid, resourceId],
    queryFn: () => apiFetch<CloudResource>(`/project/${pid}/cloud/${resourceId}`, { cloud: scope }),
    enabled: !!scope,
    refetchInterval: (q) => {
      const cur = lbOf((q.state.data ?? {}) as CloudResource)
      return BUSY.has(cur.status) ? 10000 : cur.ha ? 15000 : false
    },
  })
  const lb = row.data ? lbOf(row.data) : undefined
  const health = useQuery({
    queryKey: ["lb-health", pid, resourceId],
    queryFn: async () => (await action<{ result?: Health[] }>(pid, resourceId, scope, "GET_HEALTH")).result ?? [],
    enabled: lb?.status === "ACTIVE",
    refetchInterval: 10000,
    retry: false,
  })
  const servers = useCloudList(pid, "SERVER")
  // Only what can be a member: the balancer's own VM never can, nor can another balancer's.
  const candidates = useMemo(
    () => ((servers.data ?? []) as CloudResource[]).filter((s) => !s.data?.server?.loadBalancerId),
    [servers.data],
  )
  const [listeners, setListeners] = useState<Listener[]>([])
  const [pools, setPools] = useState<Pool[]>([])
  const [dirty, setDirty] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [confirmReplace, setConfirmReplace] = useState(false)
  useEffect(() => {
    if (lb && !dirty) {
      setListeners(lb.listeners ?? [])
      setPools(lb.pools ?? [])
    }
  }, [lb, dirty])
  const edit = (fn: () => void) => { fn(); setDirty(true) }
  const setPool = (i: number, patch: Partial<Pool>) => edit(() => setPools(pools.map((p, j) => (j === i ? { ...p, ...patch } : p))))
  const setMember = (i: number, k: number, patch: Partial<Member>) =>
    setPool(i, { members: pools[i].members.map((m, j) => (j === k ? { ...m, ...patch } : m)) })
  // A TCP check on the member's own port with no figure of its own is the default, sent as no check at all.
  const setCheck = (i: number, patch: Partial<HealthCheck>) => {
    let next: HealthCheck = { ...(pools[i].healthCheck ?? { type: "tcp" }), ...patch }
    if (next.type === "none") next = { type: "none" }
    if (next.type !== "http" && next.type !== "https") delete next.path
    for (const k of ["port", "interval", "timeout", "rise", "fall"] as const) if (!next[k]) delete next[k]
    setPool(i, { healthCheck: next.type === "tcp" && Object.keys(next).length === 1 ? null : next })
  }
  const setListener = (i: number, patch: Partial<Listener>) => edit(() => setListeners(listeners.map((x, j) => (j === i ? { ...x, ...patch } : x))))
  const protocolOf = (l: Listener) => l.protocol ?? pools.find((p) => p.name === l.pool)?.protocol ?? "http"
  // A pool speaks one protocol to its servers, the one its listeners need: the pool and its other listeners follow.
  const setProtocol = (i: number, protocol: string) => edit(() => {
    const pool = listeners[i].pool
    setListeners(listeners.map((x, j) => (j === i || x.pool === pool ? { ...x, protocol } : x)))
    setPools(pools.map((p) => (p.name === pool ? { ...p, protocol } : p)))
  })

  const save = useMutation({
    mutationFn: () => action<CloudResource>(pid, resourceId, scope, "UPDATE_CONFIG", {
      listeners: listeners.map((l) => ({ ...l, sourceRanges: (l.sourceRanges ?? []).filter(Boolean) })),
      pools,
    }),
    onSuccess: () => {
      toast.success("Configuration saved")
      setDirty(false)
      void qc.invalidateQueries({ queryKey: ["cloud-resource", pid, resourceId] })
      void qc.invalidateQueries({ queryKey: ["lb-health", pid, resourceId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const del = useMutation({
    mutationFn: () => apiFetch(`/project/${pid}/cloud/${resourceId}`, { method: "DELETE", cloud: scope }),
    onSuccess: () => {
      toast.success("Load balancer deleted")
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "LOAD_BALANCER"] })
      navigate(`/p/${pid}/load-balancers`)
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const replace = useMutation({
    mutationFn: () => action<CloudResource>(pid, resourceId, scope, "REPLACE_APPLIANCES"),
    onSuccess: () => {
      toast.success("Replacing the appliances, the standby first")
      setConfirmReplace(false)
      void qc.invalidateQueries({ queryKey: ["cloud-resource", pid, resourceId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const appliances = lb?.appliances ?? []
  const replacing = !!lb?.replacing
  // A cluster's balancer: its Services make it and the cluster's delete removes it, so nothing here edits it.
  const managed = lb?.cluster
  const statusOf = (pool: string, m: Member) =>
    health.data?.find((h) => h.pool === pool && h.serverId === m.serverId && h.port === m.port)?.status
  const ready = !!lb?.privateIp

  return (
    <>
      <PageHeader
        title={lb?.name ?? "Load balancer"}
        eyebrow="Load balancer"
        description={lb?.publicIp ? (lb.ha ? `Public address ${lb.publicIp}, on two appliances` : `Public address ${lb.publicIp}, private address ${lb.privateIp}`) : undefined}
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => navigate(`/p/${pid}/load-balancers`)}><ArrowLeft className="size-4" /> All</Button>
            {!managed && <Button variant="outline" size="sm" onClick={() => setConfirmDelete(true)}><Trash2 className="size-4" /> Delete</Button>}
          </>
        }
      />
      <div className="grid gap-4">
        <Card>
          <CardContent className="flex flex-wrap items-center gap-3 pt-6 text-sm">
            <StatusBadge status={lb?.status} />
            {lb?.status === "BUILD" && <span className="text-muted-foreground">Its VM is booting and installing HAProxy, usually a few minutes.</span>}
            {lb?.statusMessage && <span className="text-muted-foreground" data-testid="lb-status-message">{lb.statusMessage}</span>}
          </CardContent>
        </Card>

        {lb?.ha && (
          <Card data-testid="lb-appliances">
            <CardHeader className="flex flex-row items-center justify-between">
              <CardTitle className="text-base">Appliances</CardTitle>
              <Button variant="outline" size="sm" disabled={replacing || lb.status === "BUILD"} onClick={() => setConfirmReplace(true)}>
                <RefreshCcw className="size-4" /> Replace appliances
              </Button>
            </CardHeader>
            <CardContent className="grid gap-2 text-sm">
              <ApplianceList appliances={appliances} />
              {replacing && <p data-testid="lb-replacing">Replacing the appliances, one at a time, the standby first. The address keeps answering; open connections are cut at the switch.</p>}
            </CardContent>
          </Card>
        )}

        {managed && (
          <Card data-testid="lb-cluster">
            <CardHeader>
              <CardTitle className="text-base">Services of Kubernetes cluster {managed.id}</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-3 text-sm">
              <p className="text-muted-foreground">
                The Services of type LoadBalancer of{" "}
                <Link className="font-medium text-foreground hover:underline" to={`/p/${pid}/kubernetes/${managed.id}`}>cluster {managed.id}</Link>{" "}
                make this load balancer: change them, not this page. It is deleted with the cluster.
              </p>
              <ClusterServices lb={lb} health={health.data} />
            </CardContent>
          </Card>
        )}

        {!managed && <>
        <Card>
          <CardHeader className="flex flex-row items-center justify-between">
            <CardTitle className="text-base">Pools</CardTitle>
            <Button variant="outline" size="sm" onClick={() => edit(() => setPools([...pools, { name: `pool-${pools.length + 1}`, protocol: "http", algorithm: "roundrobin", members: [] }]))}>
              <Plus className="size-4" /> Add pool
            </Button>
          </CardHeader>
          <CardContent className="grid gap-6">
            {!pools.length && <p className="text-sm text-muted-foreground">A pool is the set of servers a listener forwards to.</p>}
            {pools.map((p, i) => (
              <div key={i} className="grid gap-3 rounded-md border p-3">
                <div className="flex flex-wrap items-end gap-2">
                  <div className="grid gap-1">
                    <Label htmlFor={`pool-${i}`}>Name</Label>
                    <Input id={`pool-${i}`} className="w-44" value={p.name} onChange={(e) => {
                      const next = e.target.value
                      edit(() => {
                        setListeners(listeners.map((l) => (l.pool === p.name ? { ...l, pool: next } : l)))
                        setPools(pools.map((x, j) => (j === i ? { ...x, name: next } : x)))
                      })
                    }} />
                  </div>
                  <div className="grid gap-1">
                    <Label>Algorithm</Label>
                    <Select value={p.algorithm} onValueChange={(v) => setPool(i, { algorithm: v })}>
                      <SelectTrigger className="w-40" aria-label="Algorithm"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        <SelectItem value="roundrobin">Round robin</SelectItem>
                        <SelectItem value="leastconn">Least connections</SelectItem>
                        <SelectItem value="source">Source address</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="grid gap-1">
                    <Label>Session persistence</Label>
                    <Select value={p.sessionPersistence ? "source_ip" : "none"} onValueChange={(v) => setPool(i, { sessionPersistence: v === "source_ip" ? { type: "source_ip" } : null })}>
                      <SelectTrigger className="w-40" aria-label="Session persistence"><SelectValue /></SelectTrigger>
                      <SelectContent><SelectItem value="none">None</SelectItem><SelectItem value="source_ip">Source address</SelectItem></SelectContent>
                    </Select>
                  </div>
                  {p.sessionPersistence && (
                    <div className="grid gap-1">
                      <Label htmlFor={`persist-${i}`}>Kept for (s)</Label>
                      <Input id={`persist-${i}`} className="w-28" type="number" min={1} max={86400} placeholder="10800" value={p.sessionPersistence.timeout ?? ""}
                        onChange={(e) => setPool(i, { sessionPersistence: { type: "source_ip", timeout: numberOf(e.target.value) } })} />
                    </div>
                  )}
                  <div className="grid gap-1">
                    <Label>PROXY protocol</Label>
                    <Select value={!p.proxyProtocol ? "off" : p.proxyVersion === 1 ? "1" : "2"}
                      onValueChange={(v) => setPool(i, { proxyProtocol: v !== "off", proxyVersion: v === "off" ? undefined : Number(v) })}>
                      <SelectTrigger className="w-24" aria-label="PROXY protocol"><SelectValue /></SelectTrigger>
                      <SelectContent><SelectItem value="off">Off</SelectItem><SelectItem value="1">v1</SelectItem><SelectItem value="2">v2</SelectItem></SelectContent>
                    </Select>
                  </div>
                  <Button variant="ghost" size="sm" onClick={() => edit(() => {
                    setPools(pools.filter((_, j) => j !== i))
                    setListeners(listeners.filter((l) => l.pool !== p.name))
                  })}>Remove pool</Button>
                </div>
                <div className="flex flex-wrap items-end gap-2">
                  <div className="grid gap-1">
                    <Label>Health check</Label>
                    <Select value={p.healthCheck?.type ?? "tcp"} onValueChange={(v) => setCheck(i, { type: v })}>
                      <SelectTrigger className="w-40" aria-label="Health check"><SelectValue /></SelectTrigger>
                      <SelectContent>{CHECKS.map((c) => <SelectItem key={c.value} value={c.value}>{c.label}</SelectItem>)}</SelectContent>
                    </Select>
                  </div>
                  {(p.healthCheck?.type === "http" || p.healthCheck?.type === "https") && (
                    <div className="grid gap-1">
                      <Label htmlFor={`check-path-${i}`}>Path</Label>
                      <Input id={`check-path-${i}`} className="w-36" placeholder="/" value={p.healthCheck.path ?? ""} onChange={(e) => setCheck(i, { path: e.target.value })} />
                    </div>
                  )}
                  {p.healthCheck?.type !== "none" && <>
                    <div className="grid gap-1">
                      <Label htmlFor={`check-port-${i}`}>Check port</Label>
                      <Input id={`check-port-${i}`} className="w-28" type="number" placeholder="member's" value={p.healthCheck?.port ?? ""} onChange={(e) => setCheck(i, { port: numberOf(e.target.value) })} />
                    </div>
                    {([["interval", "Every (s)", "3", 300], ["timeout", "Timeout (s)", "interval", 300], ["rise", "Up after", "2", 10], ["fall", "Down after", "2", 10]] as const).map(([key, label, own, most]) => (
                      <div key={key} className="grid gap-1">
                        <Label htmlFor={`check-${key}-${i}`}>{label}</Label>
                        <Input id={`check-${key}-${i}`} className="w-24" type="number" min={1} max={most} placeholder={own} value={p.healthCheck?.[key] ?? ""} onChange={(e) => setCheck(i, { [key]: numberOf(e.target.value) })} />
                      </div>
                    ))}
                  </>}
                  <div className="grid gap-1">
                    <Label htmlFor={`connect-${i}`}>Connect timeout (s)</Label>
                    <Input id={`connect-${i}`} className="w-32" type="number" min={1} max={86400} placeholder="2" value={p.timeoutMemberConnect ? p.timeoutMemberConnect / 1000 : ""}
                      onChange={(e) => setPool(i, { timeoutMemberConnect: numberOf(e.target.value, 1000) })} />
                  </div>
                  <div className="grid gap-1">
                    <Label htmlFor={`server-idle-${i}`}>Server idle timeout (s)</Label>
                    <Input id={`server-idle-${i}`} className="w-36" type="number" min={1} max={86400} placeholder="60" value={p.timeoutMemberData ? p.timeoutMemberData / 1000 : ""}
                      onChange={(e) => setPool(i, { timeoutMemberData: numberOf(e.target.value, 1000) })} />
                  </div>
                </div>
                <p className="text-xs text-muted-foreground" data-testid={`pool-protocol-${i}`}>
                  Its servers are spoken to in {p.protocol === "tcp" ? "TCP" : "HTTP"}, as its listeners say. An empty figure is the load balancer's own.
                </p>
                {p.members.length > 0 && <p className="text-xs text-muted-foreground">Server, port and weight</p>}
                {p.members.map((m, k) => (
                  <div key={k} className="flex flex-wrap items-center gap-2">
                    <Select value={m.serverId} onValueChange={(v) => setMember(i, k, { serverId: v })}>
                      <SelectTrigger className="w-56" aria-label="Server"><SelectValue placeholder="Server" /></SelectTrigger>
                      <SelectContent>
                        {candidates.map((s) => <SelectItem key={s.id} value={s.externalId ?? s.id}>{serverName(s)}</SelectItem>)}
                      </SelectContent>
                    </Select>
                    <Input className="w-24" type="number" aria-label="Port" value={m.port} onChange={(e) => setMember(i, k, { port: Number(e.target.value) })} />
                    <Input className="w-20" type="number" aria-label="Weight" value={m.weight ?? 1} onChange={(e) => setMember(i, k, { weight: Number(e.target.value) })} />
                    <span className="font-mono text-sm text-muted-foreground">{m.address}</span>
                    {m.missing ? <StatusBadge status="gone" /> : statusOf(p.name, m) ? <StatusBadge status={statusOf(p.name, m)} /> : null}
                    <Button variant="ghost" size="sm" onClick={() => setPool(i, { members: p.members.filter((_, j) => j !== k) })}>Remove</Button>
                  </div>
                ))}
                <div>
                  <Button variant="outline" size="sm" onClick={() => setPool(i, { members: [...p.members, { serverId: "", port: 80, weight: 1 }] })}>
                    <Plus className="size-4" /> Add server
                  </Button>
                </div>
              </div>
            ))}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex flex-row items-center justify-between">
            <CardTitle className="text-base">Listeners</CardTitle>
            <Button variant="outline" size="sm" disabled={!pools.length} onClick={() => edit(() => setListeners([...listeners, { port: 80, protocol: pools[0].protocol, pool: pools[0].name }]))}>
              <Plus className="size-4" /> Add listener
            </Button>
          </CardHeader>
          <CardContent className="grid gap-3">
            {!listeners.length && <p className="text-sm text-muted-foreground">A listener opens a port on the public address.</p>}
            {listeners.map((l, i) => (
              <div key={i} className="flex flex-wrap items-center gap-2">
                <Select value={protocolOf(l)} onValueChange={(v) => setProtocol(i, v)}>
                  <SelectTrigger className="w-24" aria-label="Listener protocol"><SelectValue /></SelectTrigger>
                  <SelectContent>{PROTOCOLS.map((x) => <SelectItem key={x.value} value={x.value}>{x.label}</SelectItem>)}</SelectContent>
                </Select>
                <Input className="w-24" type="number" aria-label="Listener port" value={l.port} onChange={(e) => setListener(i, { port: Number(e.target.value) })} />
                <span className="text-sm text-muted-foreground">forwards to</span>
                <Select value={l.pool} onValueChange={(v) => setListener(i, { pool: v, protocol: pools.find((p) => p.name === v)?.protocol })}>
                  <SelectTrigger className="w-44" aria-label="Pool"><SelectValue /></SelectTrigger>
                  <SelectContent>{pools.map((p) => <SelectItem key={p.name} value={p.name}>{p.name}</SelectItem>)}</SelectContent>
                </Select>
                <span className="text-sm text-muted-foreground">from</span>
                <Input className="w-72" aria-label="Allowed sources" placeholder="Any address, or networks such as 203.0.113.0/24"
                  value={(l.sourceRanges ?? []).join(", ")}
                  onChange={(e) => setListener(i, { sourceRanges: e.target.value.split(/[\s,]+/) })} />
                <span className="text-sm text-muted-foreground">connections</span>
                <Input className="w-28" type="number" min={1} max={20000} aria-label="Connection limit" placeholder="No limit" value={l.connectionLimit ?? ""}
                  onChange={(e) => setListener(i, { connectionLimit: numberOf(e.target.value) })} />
                <span className="text-sm text-muted-foreground">idle timeout (s)</span>
                <Input className="w-24" type="number" min={1} max={86400} aria-label="Client idle timeout in seconds" placeholder="60" value={l.timeoutClientData ? l.timeoutClientData / 1000 : ""}
                  onChange={(e) => setListener(i, { timeoutClientData: numberOf(e.target.value, 1000) })} />
                <Button variant="ghost" size="sm" onClick={() => edit(() => setListeners(listeners.filter((_, j) => j !== i)))}>Remove</Button>
              </div>
            ))}
          </CardContent>
        </Card>

        <div className="flex items-center gap-3">
          <Button onClick={() => save.mutate()} disabled={!dirty || !ready || save.isPending}>
            {save.isPending ? "Saving…" : "Save configuration"}
          </Button>
          <p className="text-sm text-muted-foreground">Security groups of the servers behind it must let the load balancer in. The default group does.</p>
        </div>
        </>}
      </div>

      <Dialog open={confirmReplace} onOpenChange={setConfirmReplace}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Replace appliances</DialogTitle>
            <DialogDescription>
              Each appliance of "{lb?.name}" is built again from the current image, the standby first. Traffic switches once, in about a second, and open connections are cut then. The new VMs are billed as the old ones were.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmReplace(false)}>Cancel</Button>
            <Button onClick={() => replace.mutate()} disabled={replace.isPending}>{replace.isPending ? "Starting…" : "Replace appliances"}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete load balancer</DialogTitle>
            <DialogDescription>
              Delete "{lb?.name}"? {lb?.ha ? "Its two VMs go" : "Its VM goes"} and its public address returns to the pool. The servers behind it stay.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmDelete(false)}>Cancel</Button>
            <Button variant="destructive" onClick={() => del.mutate()} disabled={del.isPending}>{del.isPending ? "Deleting…" : "Delete"}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
