import { useState } from "react"
import { Link, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Pencil, Plus, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { StatusBadge } from "@/components/status-badge"
import {
  Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { useCloudList, useCloudResource, useCloudScope, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"
import { networkName, networkStatus } from "./NetworksPage"

function NetworkBreadcrumb({ pid, name }: { pid: string; name?: string }) {
  return (
    <Breadcrumb>
      <BreadcrumbList>
        <BreadcrumbItem>
          <BreadcrumbLink asChild>
            <Link to={`/p/${pid}/networks`}>Networks</Link>
          </BreadcrumbLink>
        </BreadcrumbItem>
        {name ? (
          <>
            <BreadcrumbSeparator />
            <BreadcrumbItem>
              <BreadcrumbPage>{name}</BreadcrumbPage>
            </BreadcrumbItem>
          </>
        ) : null}
      </BreadcrumbList>
    </Breadcrumb>
  )
}

export default function NetworkDetailPage() {
  const pid = useProjectId()
  const { resourceId = "" } = useParams()
  const { data: network, isLoading, error } = useCloudResource(pid, resourceId)

  if (isLoading || (!network && !error)) {
    return (
      <>
        <PageHeader title="Network" eyebrow="Network" breadcrumb={<NetworkBreadcrumb pid={pid} />} />
        <div className="grid gap-3">
          <Skeleton className="h-9 w-64" />
          <Skeleton className="h-64" />
        </div>
      </>
    )
  }
  if (error || !network) {
    return (
      <>
        <PageHeader title="Network" eyebrow="Network" breadcrumb={<NetworkBreadcrumb pid={pid} />} />
        <div className="rounded-xl border bg-card p-6 text-sm text-muted-foreground">
          {(error as Error | null)?.message ?? "Network not found."}
        </div>
      </>
    )
  }

  const net = (network.data?.network ?? {}) as Record<string, unknown>
  const subnets = (net.subnets as string[] | undefined) ?? []

  return (
    <>
      <PageHeader
        title={networkName(network)}
        eyebrow="Network"
        description={`${subnets.length} ${subnets.length === 1 ? "subnet" : "subnets"} — servers, subnets and settings for this network.`}
        breadcrumb={<NetworkBreadcrumb pid={pid} name={networkName(network)} />}
      />

      <div className="mb-4 flex items-center gap-3">
        <StatusBadge status={networkStatus(network)} />
        <span className="font-mono text-xs text-muted-foreground">{network.externalId}</span>
      </div>

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="servers">Servers</TabsTrigger>
          <TabsTrigger value="subnets">Subnets</TabsTrigger>
        </TabsList>

        <TabsContent value="overview" className="mt-4">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Details</CardTitle>
            </CardHeader>
            <CardContent>
              <dl className="grid gap-x-8 gap-y-3 text-sm sm:grid-cols-2">
                <Row k="ID" v={<span className="font-mono text-xs">{(net.id as string) ?? network.externalId ?? "—"}</span>} />
                <Row k="Status" v={<StatusBadge status={networkStatus(network)} />} />
                <Row k="MTU" v={net.mtu != null ? String(net.mtu) : "—"} />
                <Row k="Shared" v={net.shared ? "Yes" : "No"} />
                <Row k="External" v={net["router:external"] ? "Yes" : "No"} />
                <Row k="Admin state up" v={net.admin_state_up === false ? "No" : "Yes"} />
                <Row k="Subnets" v={String(subnets.length)} />
              </dl>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="servers" className="mt-4">
          <NetworkServers pid={pid} resourceId={resourceId} />
        </TabsContent>

        <TabsContent value="subnets" className="mt-4">
          <SubnetsTab pid={pid} networkExtId={network.externalId ?? ""} />
        </TabsContent>
      </Tabs>
    </>
  )
}

// Servers attached to this network — cloud action GET_SERVERS → data.result: CloudResource[].
function NetworkServers({ pid, resourceId }: { pid: string; resourceId: string }) {
  const scope = useCloudScope(pid)
  const { data, isLoading, error } = useQuery({
    queryKey: ["network-servers", pid, resourceId],
    queryFn: () =>
      apiFetch<{ result?: CloudResource[] }>(`/project/${pid}/cloud/${resourceId}/action`, {
        method: "POST",
        body: { action: "GET_SERVERS" },
        cloud: scope,
      }),
    enabled: !!pid && !!resourceId && !!scope,
  })

  if (isLoading) return <Skeleton className="h-40" />
  if (error) {
    return (
      <div className="rounded-xl border bg-card p-6 text-sm text-muted-foreground">{(error as Error).message}</div>
    )
  }
  const servers = data?.result ?? []
  return (
    <Card>
      <CardContent className="pt-6">
        {servers.length ? (
          <ul className="grid gap-2 text-sm">
            {servers.map((s) => (
              <li key={s.id} className="flex items-center justify-between border-b pb-2 last:border-0">
                <Link className="inline-block py-1 font-medium hover:underline" to={`/p/${pid}/servers/${s.id}`}>
                  {(s.data?.server?.name as string) ?? s.id}
                </Link>
                <StatusBadge status={(s.data?.server?.status as string) ?? s.status} />
              </li>
            ))}
          </ul>
        ) : (
          <p className="py-4 text-center text-sm text-muted-foreground">No servers attached to this network.</p>
        )}
      </CardContent>
    </Card>
  )
}

// SubnetsTab lists this network's subnets (cached SUBNET resources filtered by network_id) with
// add / edit / delete. A network commonly carries more than one subnet.
function SubnetsTab({ pid, networkExtId }: { pid: string; networkExtId: string }) {
  const scope = useCloudScope(pid)
  const qc = useQueryClient()
  const { data, isLoading, error } = useCloudList(pid, "SUBNET")
  const rows = (data ?? []).filter((r) => (r.data?.subnet?.network_id as string) === networkExtId)

  const [createOpen, setCreateOpen] = useState(false)
  const [editing, setEditing] = useState<CloudResource | null>(null)
  const [toDelete, setToDelete] = useState<CloudResource | null>(null)

  // shared form state
  const [name, setName] = useState("")
  const [cidr, setCidr] = useState("10.0.0.0/24")
  const [dhcp, setDhcp] = useState(true)
  const [gatewayIp, setGatewayIp] = useState("")
  const [dns, setDns] = useState("8.8.8.8, 1.1.1.1")

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["cloud", pid, "SUBNET"] })
  const dnsList = () => dns.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean)

  const openCreate = () => {
    setName("")
    setCidr("10.0.0.0/24")
    setDhcp(true)
    setGatewayIp("")
    setDns("8.8.8.8, 1.1.1.1")
    setCreateOpen(true)
  }
  const openEdit = (r: CloudResource) => {
    const s = (r.data?.subnet ?? {}) as Record<string, unknown>
    setName((s.name as string) ?? "")
    setCidr((s.cidr as string) ?? "")
    setDhcp(s.enable_dhcp !== false)
    setGatewayIp((s.gateway_ip as string) ?? "")
    setDns(((s.dns_nameservers as string[]) ?? []).join(", "))
    setEditing(r)
  }

  const create = useMutation({
    mutationFn: () =>
      apiFetch(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: {
          type: "SUBNET",
          data: {
            networkId: networkExtId,
            name,
            cidr,
            enableDhcp: dhcp,
            gateway: true,
            ...(gatewayIp.trim() ? { customGatewayIp: true, gatewayIp: gatewayIp.trim() } : {}),
            ...(dnsList().length ? { dnsNameServers: dnsList() } : {}),
          },
        },
      }),
    onSuccess: () => {
      toast.success("Subnet created")
      setCreateOpen(false)
      setTimeout(invalidate, 1200)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const update = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}/action`, {
        method: "POST",
        cloud: scope,
        body: {
          action: "UPDATE",
          data: { name, enableDhcp: dhcp, gatewayIp: gatewayIp.trim(), dnsNameServers: dnsList() },
        },
      }),
    onSuccess: () => {
      toast.success("Subnet updated")
      setEditing(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const del = useMutation({
    mutationFn: (r: CloudResource) => apiFetch(`/project/${pid}/cloud/${r.id}`, { method: "DELETE", cloud: scope }),
    onSuccess: () => {
      toast.success("Subnet deletion requested")
      setToDelete(null)
      setTimeout(invalidate, 1200)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <>
      <div className="mb-3 flex items-center justify-between gap-2">
        <span className="text-eyebrow">Subnets</span>
        <Button size="sm" onClick={openCreate}>
          <Plus className="size-4" /> Add subnet
        </Button>
      </div>

      {isLoading ? (
        <Skeleton className="h-40" />
      ) : error ? (
        <div className="rounded-xl border bg-card p-6 text-sm text-muted-foreground">{(error as Error).message}</div>
      ) : !rows.length ? (
        <div className="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">
          No subnets on this network yet.
        </div>
      ) : (
        <div className="overflow-hidden rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Name</TableHead>
                <TableHead>CIDR</TableHead>
                <TableHead>Gateway</TableHead>
                <TableHead>DHCP</TableHead>
                <TableHead>DNS</TableHead>
                <TableHead className="w-20" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => {
                const s = (r.data?.subnet ?? {}) as Record<string, unknown>
                return (
                  <TableRow key={r.id}>
                    <TableCell className="font-medium">{(s.name as string) || "—"}</TableCell>
                    <TableCell className="font-mono text-xs">{(s.cidr as string) ?? "—"}</TableCell>
                    <TableCell className="font-mono text-xs">{(s.gateway_ip as string) || "—"}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">{s.enable_dhcp !== false ? "Yes" : "No"}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {((s.dns_nameservers as string[]) ?? []).join(", ") || "—"}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button variant="ghost" size="icon-sm" onClick={() => openEdit(r)} aria-label="Edit subnet">
                        <Pencil className="size-4 text-muted-foreground" />
                      </Button>
                      <Button variant="ghost" size="icon-sm" onClick={() => setToDelete(r)} aria-label="Delete subnet">
                        <Trash2 className="size-4 text-muted-foreground" />
                      </Button>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </div>
      )}

      {/* Create / edit share one form */}
      <Dialog open={createOpen || !!editing} onOpenChange={(o) => !o && (setCreateOpen(false), setEditing(null))}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editing ? "Edit subnet" : "Add subnet"}</DialogTitle>
            <DialogDescription>
              {editing ? "CIDR can't change after creation." : "Add a subnet to this network."}
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-2">
            <div className="grid gap-2">
              <Label htmlFor="sn-name">Name</Label>
              <Input id="sn-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="my-subnet" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="sn-cidr">CIDR</Label>
              <Input
                id="sn-cidr"
                className="font-mono"
                value={cidr}
                onChange={(e) => setCidr(e.target.value)}
                disabled={!!editing}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="sn-gw">Gateway IP</Label>
              <Input
                id="sn-gw"
                className="font-mono"
                value={gatewayIp}
                onChange={(e) => setGatewayIp(e.target.value)}
                placeholder="Auto (first usable)"
              />
              <p className="text-xs text-muted-foreground">Set to a VM's IP to route via a self-hosted router.</p>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="sn-dns">DNS servers</Label>
              <Input
                id="sn-dns"
                className="font-mono"
                value={dns}
                onChange={(e) => setDns(e.target.value)}
                placeholder="8.8.8.8, 1.1.1.1"
              />
            </div>
            <label className="flex cursor-pointer items-center gap-2 text-sm">
              <Checkbox checked={dhcp} onCheckedChange={(v) => setDhcp(v === true)} />
              Enable DHCP
            </label>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => (editing ? setEditing(null) : setCreateOpen(false))}>
              Cancel
            </Button>
            {editing ? (
              <Button onClick={() => update.mutate(editing)} disabled={update.isPending}>
                {update.isPending ? "Saving…" : "Save changes"}
              </Button>
            ) : (
              <Button onClick={() => create.mutate()} disabled={!cidr || create.isPending}>
                {create.isPending ? "Creating…" : "Add subnet"}
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete subnet</DialogTitle>
            <DialogDescription>
              Delete subnet "{(toDelete?.data?.subnet?.name as string) || (toDelete?.data?.subnet?.cidr as string) || ""}"?
              Detach any router interface / ports first.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={() => toDelete && del.mutate(toDelete)} disabled={del.isPending}>
              {del.isPending ? "Deleting…" : "Delete subnet"}
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
