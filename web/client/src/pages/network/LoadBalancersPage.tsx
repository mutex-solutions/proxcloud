import { useEffect, useMemo, useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { ArrowLeft, Plus, RefreshCw, Scale, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { apiFetch, type CloudScope } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudList, useCloudScope, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

interface Member { serverId: string; port: number; weight?: number; address?: string; missing?: boolean }
interface Pool { name: string; protocol: string; algorithm: string; members: Member[] }
interface Listener { port: number; pool: string }
interface LB {
  id: string; name: string; status: string; statusMessage?: string; serverId?: string; networkId: string
  publicIp?: string; privateIp?: string; listeners?: Listener[] | null; pools?: Pool[] | null
}
interface Health { pool: string; serverId: string; port: number; status: string }

const lbOf = (r: CloudResource) => (r.data?.loadbalancer ?? {}) as LB
const BUSY = new Set(["BUILD"])

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

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(() => [
    {
      id: "name",
      accessorFn: (r) => lbOf(r).name,
      header: sortableHeader("Name"),
      cell: ({ row, getValue }) => (
        <Link className="inline-block py-1 font-medium hover:underline" to={`/p/${pid}/load-balancers/${row.original.id}`} onClick={(e) => e.stopPropagation()}>
          {getValue()}
        </Link>
      ),
    },
    { id: "status", accessorFn: (r) => lbOf(r).status, header: sortableHeader("Status"), cell: ({ getValue }) => <StatusBadge status={getValue()} /> },
    { id: "ip", accessorFn: (r) => lbOf(r).publicIp ?? "", header: "Public address", cell: ({ getValue }) => <span className="font-mono text-sm">{getValue() || "—"}</span> },
    { id: "listeners", accessorFn: (r) => (lbOf(r).listeners ?? []).map((l) => l.port).join(", "), header: "Listeners", cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "None"}</span> },
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
            <Button size="sm" onClick={() => setOpen(true)}><Plus className="size-4" /> Create load balancer</Button>
          </>
        }
      />
      {!list.isLoading && !list.error && !rows.length ? (
        <EmptyState
          icon={Scale}
          title="No load balancers yet"
          hint="Create one, then add listeners and the servers behind them."
          action={<Button onClick={() => setOpen(true)}><Plus className="size-4" /> Create load balancer</Button>}
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
  const create = useMutation({
    mutationFn: () => apiFetch<CloudResource>(`/project/${pid}/cloud`, {
      method: "POST", cloud: scope,
      body: { type: "LOAD_BALANCER", data: { name, networkId, publicNetworkId, flavorId } },
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
            It runs on its own small VM, billed like any server with its disk and public address, and counted in your quota.
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
    refetchInterval: (q) => (BUSY.has(lbOf((q.state.data ?? {}) as CloudResource).status) ? 10000 : false),
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

  const save = useMutation({
    mutationFn: () => action<CloudResource>(pid, resourceId, scope, "UPDATE_CONFIG", { listeners, pools }),
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
  const statusOf = (pool: string, m: Member) =>
    health.data?.find((h) => h.pool === pool && h.serverId === m.serverId && h.port === m.port)?.status
  const ready = !!lb?.privateIp

  return (
    <>
      <PageHeader
        title={lb?.name ?? "Load balancer"}
        eyebrow="Load balancer"
        description={lb?.publicIp ? `Public address ${lb.publicIp}, private address ${lb.privateIp}` : undefined}
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => navigate(`/p/${pid}/load-balancers`)}><ArrowLeft className="size-4" /> All</Button>
            <Button variant="outline" size="sm" onClick={() => setConfirmDelete(true)}><Trash2 className="size-4" /> Delete</Button>
          </>
        }
      />
      <div className="grid gap-4">
        <Card>
          <CardContent className="flex flex-wrap items-center gap-3 pt-6 text-sm">
            <StatusBadge status={lb?.status} />
            {lb?.status === "BUILD" && <span className="text-muted-foreground">Its VM is booting and installing HAProxy, usually a few minutes.</span>}
            {lb?.statusMessage && <span className="text-muted-foreground">{lb.statusMessage}</span>}
          </CardContent>
        </Card>

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
                    <Label>Protocol</Label>
                    <Select value={p.protocol} onValueChange={(v) => setPool(i, { protocol: v })}>
                      <SelectTrigger className="w-28" aria-label="Protocol"><SelectValue /></SelectTrigger>
                      <SelectContent><SelectItem value="http">HTTP</SelectItem><SelectItem value="tcp">TCP</SelectItem></SelectContent>
                    </Select>
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
                  <Button variant="ghost" size="sm" onClick={() => edit(() => {
                    setPools(pools.filter((_, j) => j !== i))
                    setListeners(listeners.filter((l) => l.pool !== p.name))
                  })}>Remove pool</Button>
                </div>
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
            <Button variant="outline" size="sm" disabled={!pools.length} onClick={() => edit(() => setListeners([...listeners, { port: 80, pool: pools[0].name }]))}>
              <Plus className="size-4" /> Add listener
            </Button>
          </CardHeader>
          <CardContent className="grid gap-3">
            {!listeners.length && <p className="text-sm text-muted-foreground">A listener opens a port on the public address.</p>}
            {listeners.map((l, i) => (
              <div key={i} className="flex flex-wrap items-center gap-2">
                <Input className="w-24" type="number" aria-label="Listener port" value={l.port} onChange={(e) => edit(() => setListeners(listeners.map((x, j) => (j === i ? { ...x, port: Number(e.target.value) } : x))))} />
                <span className="text-sm text-muted-foreground">forwards to</span>
                <Select value={l.pool} onValueChange={(v) => edit(() => setListeners(listeners.map((x, j) => (j === i ? { ...x, pool: v } : x))))}>
                  <SelectTrigger className="w-44" aria-label="Pool"><SelectValue /></SelectTrigger>
                  <SelectContent>{pools.map((p) => <SelectItem key={p.name} value={p.name}>{p.name}</SelectItem>)}</SelectContent>
                </Select>
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
      </div>

      <Dialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete load balancer</DialogTitle>
            <DialogDescription>
              Delete "{lb?.name}"? Its VM goes and its public address returns to the pool. The servers behind it stay.
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
