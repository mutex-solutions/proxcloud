import { useMemo, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { MoreHorizontal, Network, Plus, RefreshCw, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { LoadMore } from "@/components/load-more"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudCursorList, useCloudScope, useProject, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

export function networkName(r: CloudResource): string {
  return (r.data?.networkName as string) || (r.data?.network?.name as string) || r.name || r.id
}
export function networkStatus(r: CloudResource): string | undefined {
  return (r.data?.network?.status as string) ?? r.status
}
// A private (own) network — not a shared or router:external cloud network. When the external-network
// picker is hidden (publicNetworksVisible=false), the Networks page + create-server network step show
// only these; external/shared pools stay in the FIP/router pickers.
export function isPrivateNetwork(r: CloudResource): boolean {
  const net = (r.data?.network ?? {}) as Record<string, unknown>
  return !net.shared && !net["router:external"]
}

export default function NetworksPage() {
  const pid = useProjectId()
  const scope = useCloudScope(pid)
  const navigate = useNavigate()
  const qc = useQueryClient()
  const {
    rows: data, isLoading, refetch, isFetching, error, hasNextPage, fetchNextPage, isFetchingNextPage,
  } = useCloudCursorList(pid, "NETWORK")
  // Hidden external picker → show only the project's own private networks (drop shared/external infra).
  const netsVisible = useProject(pid).project?.publicNetworksVisible === true
  const rows = useMemo(
    () => (netsVisible ? data : data.filter(isPrivateNetwork)),
    [data, netsVisible],
  )
  const [createOpen, setCreateOpen] = useState(false)
  const [toDelete, setToDelete] = useState<CloudResource | null>(null)

  // create form
  const [name, setName] = useState("")
  const [withSubnet, setWithSubnet] = useState(true)
  const [cidr, setCidr] = useState("10.0.0.0/24")
  const [dns, setDns] = useState("8.8.8.8, 1.1.1.1")
  const [gatewayIp, setGatewayIp] = useState("") // blank = auto (first usable); set to a VM IP for a self-hosted router

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["cloud", pid, "NETWORK"] })

  const create = useMutation({
    mutationFn: () => {
      const dnsList = dns.split(/[\s,]+/).map((s) => s.trim()).filter(Boolean)
      const gw = gatewayIp.trim()
      return apiFetch(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: {
          type: "NETWORK",
          data: withSubnet
            ? {
                name,
                defaultSubnet: true,
                cidr,
                enableDhcp: true,
                // A custom gateway points the subnet at a self-hosted router VM (e.g. pfSense);
                // blank uses neutron's auto gateway.
                gateway: true,
                ...(gw ? { customGatewayIp: true, gatewayIp: gw } : {}),
                ...(dnsList.length ? { dnsNameServers: dnsList } : {}),
              }
            : { name },
        },
      })
    },
    onSuccess: () => {
      toast.success(`Network "${name}" created`)
      setCreateOpen(false)
      setName("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const del = useMutation({
    mutationFn: (r: CloudResource) =>
      apiFetch(`/project/${pid}/cloud/${r.id}`, { method: "DELETE", cloud: scope }),
    onSuccess: () => {
      toast.success("Network deletion requested")
      setToDelete(null)
      setTimeout(invalidate, 1500)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => networkName(r),
        header: sortableHeader("Name"),
        cell: ({ row, getValue }) => (
          <Link
            className="inline-block py-1 font-medium hover:underline"
            to={`/p/${pid}/networks/${row.original.id}`}
            onClick={(e) => e.stopPropagation()}
          >
            {getValue()}
          </Link>
        ),
      },
      {
        id: "status",
        accessorFn: (r) => networkStatus(r) ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "subnets",
        accessorFn: (r) => ((r.data?.network?.subnets as string[] | undefined) ?? []).length,
        header: sortableHeader("Subnets"),
        cell: ({ getValue }) => (
          <span className="text-sm tabular-nums text-muted-foreground">{getValue()}</span>
        ),
      },
      {
        id: "flags",
        accessorFn: (r) => {
          const net = (r.data?.network ?? {}) as Record<string, unknown>
          return [net.shared ? "shared" : "", net["router:external"] ? "external" : ""].filter(Boolean).join(" ")
        },
        header: "Flags",
        enableSorting: false,
        cell: ({ row }) => {
          const net = (row.original.data?.network ?? {}) as Record<string, unknown>
          return (
            <div className="flex gap-1">
              {net.shared ? <Badge variant="secondary">Shared</Badge> : null}
              {net["router:external"] ? <Badge variant="secondary">External</Badge> : null}
              {!net.shared && !net["router:external"] ? (
                <span className="text-sm text-muted-foreground">—</span>
              ) : null}
            </div>
          )
        },
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
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${networkName(r)}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => navigate(`/p/${pid}/networks/${r.id}`)}>
                    View details
                  </DropdownMenuItem>
                  <DropdownMenuItem variant="destructive" onClick={() => setToDelete(r)}>
                    <Trash2 className="size-4" /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // pid is stable per mount; setters/navigate are stable.
    [pid, navigate],
  )

  return (
    <>
      <PageHeader
        title="Networks"
        eyebrow="Network"
        description="Private networks in this project."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="Refresh">
              <RefreshCw className={isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create network
            </Button>
          </>
        }
      />

      {!isLoading && !error && !rows.length ? (
        <EmptyState
          icon={Network}
          title="No networks yet"
          hint="Create a private network to connect your servers."
          action={
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="size-4" /> Create network
            </Button>
          }
        />
      ) : (
        <>
          <DataTable
            columns={columns}
            data={rows}
            isLoading={isLoading}
            error={error as Error | null}
            pagination={false}
            onRowClick={(r) => navigate(`/p/${pid}/networks/${r.id}`)}
          />
          <LoadMore
            hasNextPage={hasNextPage}
            isFetching={isFetchingNextPage}
            onClick={() => void fetchNextPage()}
            count={rows.length}
            noun="network"
          />
        </>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create network</DialogTitle>
            <DialogDescription>A private network, optionally with an initial subnet.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-2">
            <div className="grid gap-2">
              <Label htmlFor="net-name">Name</Label>
              <Input id="net-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="my-network" />
            </div>
            <div className="flex items-center gap-2">
              <Checkbox id="net-subnet" checked={withSubnet} onCheckedChange={(v) => setWithSubnet(v === true)} />
              <Label htmlFor="net-subnet">Create a subnet</Label>
            </div>
            {withSubnet ? (
              <>
                <div className="grid gap-2">
                  <Label htmlFor="net-cidr">Subnet CIDR</Label>
                  <Input id="net-cidr" className="font-mono" value={cidr} onChange={(e) => setCidr(e.target.value)} />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="net-dns">DNS servers</Label>
                  <Input
                    id="net-dns"
                    className="font-mono"
                    value={dns}
                    onChange={(e) => setDns(e.target.value)}
                    placeholder="8.8.8.8, 1.1.1.1"
                  />
                  <p className="text-xs text-muted-foreground">Comma/space separated. Leave blank for none.</p>
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="net-gw">Gateway IP</Label>
                  <Input
                    id="net-gw"
                    className="font-mono"
                    value={gatewayIp}
                    onChange={(e) => setGatewayIp(e.target.value)}
                    placeholder="Auto (first usable)"
                  />
                  <p className="text-xs text-muted-foreground">
                    Set to a VM's IP to route this subnet through a self-hosted router (e.g. pfSense/OPNsense).
                  </p>
                </div>
              </>
            ) : null}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => create.mutate()} disabled={!name || (withSubnet && !cidr) || create.isPending}>
              {create.isPending ? "Creating…" : "Create network"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete network</DialogTitle>
            <DialogDescription>
              Delete network "{toDelete ? networkName(toDelete) : ""}"? Its subnets are deleted with it. This
              cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={() => toDelete && del.mutate(toDelete)} disabled={del.isPending}>
              {del.isPending ? "Deleting…" : "Delete network"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
