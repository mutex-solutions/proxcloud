import { useState } from "react"
import { Link, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Plus, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import {
  Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { useCloudResource, useCloudScope, useProjectId } from "@/lib/hooks"

// Raw neutron rule shape (LIST_RULES returns the group's security_group_rules verbatim).
type SgRule = {
  id?: string
  direction?: string
  ethertype?: string
  protocol?: string | null
  port_range_min?: number | null
  port_range_max?: number | null
  remote_ip_prefix?: string | null
  remote_group_id?: string | null
}

function portRange(rule: SgRule): string {
  if (rule.port_range_min == null && rule.port_range_max == null) return "any"
  if (rule.port_range_min === rule.port_range_max) return String(rule.port_range_min)
  return `${rule.port_range_min ?? "*"} – ${rule.port_range_max ?? "*"}`
}

function SgBreadcrumb({ pid, name }: { pid: string; name?: string }) {
  return (
    <Breadcrumb>
      <BreadcrumbList>
        <BreadcrumbItem>
          <BreadcrumbLink asChild>
            <Link to={`/p/${pid}/security-groups`}>Security groups</Link>
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

export default function SecurityGroupDetailPage() {
  const pid = useProjectId()
  const { resourceId = "" } = useParams()
  const scope = useCloudScope(pid)
  const qc = useQueryClient()
  const { data: group, isLoading } = useCloudResource(pid, resourceId)

  const rules = useQuery({
    queryKey: ["sg-rules", pid, resourceId],
    queryFn: () =>
      apiFetch<{ result?: SgRule[] }>(`/project/${pid}/cloud/${resourceId}/action`, {
        method: "POST",
        body: { action: "LIST_RULES" },
        cloud: scope,
      }),
    enabled: !!pid && !!resourceId && !!scope,
  })

  const [addOpen, setAddOpen] = useState(false)
  const [direction, setDirection] = useState("ingress")
  const [etherType, setEtherType] = useState("IPv4")
  const [protocol, setProtocol] = useState("tcp")
  const [portMin, setPortMin] = useState("")
  const [portMax, setPortMax] = useState("")
  const [remoteIpPrefix, setRemoteIpPrefix] = useState("")
  const [ruleToDelete, setRuleToDelete] = useState<SgRule | null>(null)

  const refreshRules = () => {
    void qc.invalidateQueries({ queryKey: ["sg-rules", pid, resourceId] })
    void qc.invalidateQueries({ queryKey: ["cloud-resource", pid, resourceId] })
  }

  const addRule = useMutation({
    mutationFn: () => {
      const data: Record<string, unknown> = { direction, etherType }
      if (protocol.trim()) data.protocol = protocol.trim()
      if (portMin.trim()) data.portRangeMin = Number(portMin)
      if (portMax.trim()) data.portRangeMax = Number(portMax)
      if (remoteIpPrefix.trim()) data.remoteIpPrefix = remoteIpPrefix.trim()
      return apiFetch(`/project/${pid}/cloud/${resourceId}/action`, {
        method: "POST",
        body: { action: "ADD_RULE", data },
        cloud: scope,
      })
    },
    onSuccess: () => {
      toast.success("Rule added")
      setAddOpen(false)
      setPortMin("")
      setPortMax("")
      setRemoteIpPrefix("")
      refreshRules()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const deleteRule = useMutation({
    mutationFn: (ruleId: string) =>
      apiFetch(`/project/${pid}/cloud/${resourceId}/action`, {
        method: "POST",
        body: { action: "DELETE_RULE", data: { ruleId } },
        cloud: scope,
      }),
    onSuccess: () => {
      toast.success("Rule deleted")
      setRuleToDelete(null)
      refreshRules()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  if (isLoading || !group) {
    return (
      <>
        <PageHeader title="Security group" eyebrow="Network" breadcrumb={<SgBreadcrumb pid={pid} />} />
        <div className="grid gap-3">
          <Skeleton className="h-9 w-64" />
          <Skeleton className="h-64" />
        </div>
      </>
    )
  }

  const sg = group.data?.securityGroup ?? {}
  const name = (sg.name as string) ?? group.name ?? group.id

  return (
    <>
      <PageHeader
        title={name}
        eyebrow="Network"
        description={(sg.description as string) || "Security group rules."}
        breadcrumb={<SgBreadcrumb pid={pid} name={name} />}
        actions={
          <Button size="sm" onClick={() => setAddOpen(true)}>
            <Plus className="size-4" /> Add rule
          </Button>
        }
      />

      <div className="mb-4 flex items-center gap-3">
        <StatusBadge status={group.status} />
        <span className="font-mono text-xs text-muted-foreground">{group.externalId}</span>
      </div>

      <div className="mb-3 text-eyebrow">Rules</div>
      {rules.isLoading ? (
        <Skeleton className="h-40" />
      ) : rules.isError ? (
        <div className="rounded-xl border bg-card p-6 text-sm text-muted-foreground">
          {(rules.error as Error).message}
        </div>
      ) : !rules.data?.result?.length ? (
        <div className="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">
          No rules in this group yet — add one to allow traffic.
        </div>
      ) : (
        <div className="overflow-hidden rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Direction</TableHead>
                <TableHead>Ether type</TableHead>
                <TableHead>Protocol</TableHead>
                <TableHead>Port range</TableHead>
                <TableHead>Remote</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rules.data.result.map((rule, i) => (
                <TableRow key={rule.id ?? i}>
                  <TableCell>
                    <Badge variant={rule.direction === "ingress" ? "secondary" : "outline"} className="capitalize">
                      {rule.direction ?? "—"}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-sm">{rule.ethertype ?? "—"}</TableCell>
                  <TableCell className="text-sm uppercase">{rule.protocol ?? "any"}</TableCell>
                  <TableCell className="font-mono text-sm">{portRange(rule)}</TableCell>
                  <TableCell className="font-mono text-sm">
                    {rule.remote_ip_prefix ?? rule.remote_group_id ?? "any"}
                  </TableCell>
                  <TableCell className="text-right">
                    <Button variant="ghost" size="icon-sm" onClick={() => setRuleToDelete(rule)} aria-label="Delete rule">
                      <Trash2 className="size-4 text-muted-foreground" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add rule</DialogTitle>
            <DialogDescription>Allow traffic matching this rule.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-2">
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="grid gap-2">
                <Label>Direction</Label>
                <Select value={direction} onValueChange={setDirection}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ingress">Ingress</SelectItem>
                    <SelectItem value="egress">Egress</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="grid gap-2">
                <Label>Ether type</Label>
                <Select value={etherType} onValueChange={setEtherType}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="IPv4">IPv4</SelectItem>
                    <SelectItem value="IPv6">IPv6</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
              <div className="grid gap-2">
                <Label htmlFor="rule-proto">Protocol</Label>
                <Input
                  id="rule-proto"
                  value={protocol}
                  onChange={(e) => setProtocol(e.target.value)}
                  placeholder="tcp / udp / icmp"
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="rule-port-min">Port from</Label>
                <Input
                  id="rule-port-min"
                  type="number"
                  value={portMin}
                  onChange={(e) => setPortMin(e.target.value)}
                  placeholder="22"
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="rule-port-max">Port to</Label>
                <Input
                  id="rule-port-max"
                  type="number"
                  value={portMax}
                  onChange={(e) => setPortMax(e.target.value)}
                  placeholder="22"
                />
              </div>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="rule-cidr">Remote IP prefix (CIDR)</Label>
              <Input
                id="rule-cidr"
                className="font-mono"
                value={remoteIpPrefix}
                onChange={(e) => setRemoteIpPrefix(e.target.value)}
                placeholder="0.0.0.0/0"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => addRule.mutate()} disabled={addRule.isPending}>
              {addRule.isPending ? "Adding…" : "Add rule"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!ruleToDelete} onOpenChange={(o) => !o && setRuleToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete rule</DialogTitle>
            <DialogDescription>
              Delete this {ruleToDelete?.direction ?? ""} rule ({ruleToDelete?.protocol ?? "any"},{" "}
              {ruleToDelete ? portRange(ruleToDelete) : ""})? This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRuleToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => ruleToDelete?.id && deleteRule.mutate(ruleToDelete.id)}
              disabled={deleteRule.isPending}
            >
              {deleteRule.isPending ? "Deleting…" : "Delete rule"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
