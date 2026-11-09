import { useState } from "react"
import { Link, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Plus, Trash2 } from "lucide-react"
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
import { useCloudFeatures, useCloudResource, useCloudScope, useProjectId } from "@/lib/hooks"
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
  // Subnets are a separate resource only on providers that serve them (/init features).
  const features = useCloudFeatures(pid)
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
        description="Details and servers of this network."
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
          {features.has("subnets") && <TabsTrigger value="subnets">Subnets</TabsTrigger>}
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
                <Row k="Subnet" v={<span className="font-mono text-xs">{subnets.join(", ") || "—"}</span>} />
                <Row k="Gateway" v={<span className="font-mono text-xs">{(net.gateway as string) || "—"}</span>} />
                <Row k="Outbound internet (SNAT)" v={net.snat ? "Yes" : "No"} />
                <Row k="Router" v={<span className="text-xs">zone {(net.zone as string) || "—"}</span>} />
              </dl>
              <p className="mt-3 text-xs text-muted-foreground">
                The zone routes between all of this project's networks and subnets. Other projects cannot reach them.
              </p>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="servers" className="mt-4">
          <NetworkServers pid={pid} resourceId={resourceId} />
        </TabsContent>

        {features.has("subnets") && (
          <TabsContent value="subnets" className="mt-4">
            <SubnetsTab pid={pid} network={network} />
          </TabsContent>
        )}
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

// SubnetsTab: a network's subnets live on its own row (data.network.subnetDetails). Routing between
// them and between the project's networks is the zone's job; a tenant only decides on outbound SNAT.
type SubnetRow = { cidr: string; gateway?: string; snat?: boolean }

function SubnetsTab({ pid, network }: { pid: string; network: CloudResource }) {
  const scope = useCloudScope(pid)
  const qc = useQueryClient()
  const rows = ((network.data?.network?.subnetDetails ?? []) as SubnetRow[])
  const [addOpen, setAddOpen] = useState(false)
  const [cidr, setCidr] = useState("")
  const [toDelete, setToDelete] = useState<string | null>(null)
  const act = useMutation({
    mutationFn: (p: { action: string; data: Record<string, unknown> }) =>
      apiFetch(`/project/${pid}/cloud/${network.id}/action`, { method: "POST", cloud: scope, body: p }),
    onSuccess: () => {
      toast.success("Network updated")
      setAddOpen(false)
      setCidr("")
      setToDelete(null)
      void qc.invalidateQueries({ queryKey: ["cloud-resource", pid, network.id] })
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "NETWORK"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  return (
    <>
      <div className="mb-3 flex items-center justify-between gap-2">
        <span className="text-eyebrow">Subnets</span>
        <Button size="sm" onClick={() => setAddOpen(true)}>
          <Plus className="size-4" /> Add subnet
        </Button>
      </div>
      <div className="overflow-hidden rounded-xl border bg-card">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>CIDR</TableHead>
              <TableHead>Gateway</TableHead>
              <TableHead>Outbound internet (SNAT)</TableHead>
              <TableHead className="w-20" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.cidr}>
                <TableCell className="font-mono text-xs">{r.cidr}</TableCell>
                <TableCell className="font-mono text-xs">{r.gateway || "—"}</TableCell>
                <TableCell>
                  <Checkbox
                    aria-label={`Outbound internet for ${r.cidr}`}
                    checked={!!r.snat}
                    disabled={act.isPending}
                    onCheckedChange={(v) => act.mutate({ action: "SET_SNAT", data: { cidr: r.cidr, snat: v === true } })}
                  />
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    disabled={rows.length < 2}
                    onClick={() => setToDelete(r.cidr)}
                    aria-label={`Delete subnet ${r.cidr}`}
                  >
                    <Trash2 className="size-4 text-muted-foreground" />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add subnet</DialogTitle>
            <DialogDescription>Leave the CIDR empty to take the next free /24 of the provider's pool.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="sn-cidr">CIDR (optional)</Label>
            <Input id="sn-cidr" value={cidr} onChange={(e) => setCidr(e.target.value)} placeholder="auto" className="font-mono" />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>
              Cancel
            </Button>
            <Button
              disabled={act.isPending}
              onClick={() => act.mutate({ action: "ADD_SUBNET", data: { ...(cidr.trim() ? { cidr: cidr.trim() } : {}) } })}
            >
              {act.isPending ? "Adding…" : "Add subnet"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete subnet</DialogTitle>
            <DialogDescription>Delete subnet {toDelete}? It must have no servers on it.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={act.isPending}
              onClick={() => toDelete && act.mutate({ action: "REMOVE_SUBNET", data: { cidr: toDelete } })}
            >
              {act.isPending ? "Deleting…" : "Delete subnet"}
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
