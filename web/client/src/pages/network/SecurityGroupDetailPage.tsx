import { useState } from "react"
import { Link, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Pencil, Plus, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
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
import { ApiError, apiFetch } from "@/lib/api"
import { useCloudResource, useCloudScope, useProjectId } from "@/lib/hooks"

// Raw rule shape (LIST_RULES returns the group's security_group_rules verbatim).
type SgRule = {
  id?: string
  direction?: string
  protocol?: string | null
  port_range?: string | null // "22" or "8000:8100"
  remote_ip_prefix?: string | null
  remote_group_id?: string | null
  description?: string | null // the rule's comment on Proxmox
}

// The protocols the API takes (sgProtocols in internal/platform/project/cloud_writes_pve.go); true = it has ports.
const PROTOCOLS: Array<{ value: string; label: string; ports: boolean }> = [
  { value: "tcp", label: "TCP", ports: true },
  { value: "udp", label: "UDP", ports: true },
  { value: "sctp", label: "SCTP", ports: true },
  { value: "icmp", label: "ICMP", ports: false },
  { value: "gre", label: "GRE", ports: false },
  { value: "esp", label: "ESP", ports: false },
  { value: "ah", label: "AH", ports: false },
  { value: "any", label: "Any protocol", ports: false },
]

type RuleForm = { protocol: string; portMin: string; portMax: string; remoteIpPrefix: string; description: string }
const EMPTY_RULE: RuleForm = { protocol: "tcp", portMin: "", portMax: "", remoteIpPrefix: "", description: "" }

function formOf(rule: SgRule): RuleForm {
  const [portMin = "", portMax = ""] = (rule.port_range ?? "").split(":")
  return {
    protocol: rule.protocol || "any", portMin, portMax,
    remoteIpPrefix: rule.remote_ip_prefix ?? "", description: rule.description ?? "",
  }
}

function portRange(rule: SgRule): string {
  return rule.port_range ? rule.port_range.replace(":", " – ") : "any"
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

  // The rule dialog adds a rule, or edits the one in `editing`. It starts from the rule or from empty each time it
  // opens, so a cancelled or refused attempt leaves nothing behind.
  const [ruleOpen, setRuleOpen] = useState(false)
  const [editing, setEditing] = useState<SgRule | null>(null)
  const [form, setForm] = useState<RuleForm>(EMPTY_RULE)
  const [ruleToDelete, setRuleToDelete] = useState<SgRule | null>(null)
  const openRule = (rule: SgRule | null) => {
    setEditing(rule)
    setForm(rule ? formOf(rule) : EMPTY_RULE)
    setRuleOpen(true)
  }
  const hasPorts = PROTOCOLS.find((p) => p.value === form.protocol)?.ports ?? false
  // The console makes inbound rules only. A rule made through the API as egress keeps its direction when edited.
  const direction = editing?.direction === "egress" ? "egress" : "ingress"

  const refreshRules = () => {
    void qc.invalidateQueries({ queryKey: ["sg-rules", pid, resourceId] })
    void qc.invalidateQueries({ queryKey: ["cloud-resource", pid, resourceId] })
  }

  const saveRule = useMutation({
    mutationFn: () => {
      const data: Record<string, unknown> = { direction }
      if (form.protocol !== "any") data.protocol = form.protocol
      if (hasPorts && form.portMin.trim()) data.portRangeMin = Number(form.portMin)
      if (hasPorts && form.portMax.trim()) data.portRangeMax = Number(form.portMax)
      if (form.remoteIpPrefix.trim()) data.remoteIpPrefix = form.remoteIpPrefix.trim()
      if (form.description.trim()) data.description = form.description.trim()
      // An edit is one action: the API adds the new rule and then deletes the old one (UPDATE_RULE).
      if (editing?.id) data.ruleId = editing.id
      return apiFetch(`/project/${pid}/cloud/${resourceId}/action`, {
        method: "POST",
        body: { action: editing ? "UPDATE_RULE" : "ADD_RULE", data },
        cloud: scope,
      })
    },
    onSuccess: () => {
      toast.success(editing ? "Rule changed" : "Rule added")
      setRuleOpen(false)
      refreshRules()
    },
    onError: (e: Error) => {
      toast.error(e.message)
      // 409: the rules changed since this page loaded them (the answer says whether the new rule was added).
      if (e instanceof ApiError && e.status === 409) {
        setRuleOpen(false)
        refreshRules()
      }
    },
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
    onError: (e: Error) => {
      toast.error(e.message)
      // 409: the rule list changed since this page loaded it, so the id no longer names the rule. Show the current list.
      if (e instanceof ApiError && e.status === 409) {
        setRuleToDelete(null)
        refreshRules()
      }
    },
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
          <Button size="sm" onClick={() => openRule(null)}>
            <Plus className="size-4" /> Add rule
          </Button>
        }
      />

      <div className="mb-4 flex items-center gap-3">
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
                <TableHead>Protocol</TableHead>
                <TableHead>Port range</TableHead>
                <TableHead>Remote</TableHead>
                <TableHead>Description</TableHead>
                <TableHead className="w-24" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rules.data.result.map((rule, i) => (
                <TableRow key={rule.id ?? i}>
                  <TableCell className="text-sm capitalize">{rule.direction || "ingress"}</TableCell>
                  <TableCell className="text-sm uppercase">{rule.protocol || "any"}</TableCell>
                  <TableCell className="font-mono text-sm">{portRange(rule)}</TableCell>
                  <TableCell className="font-mono text-sm">
                    {rule.remote_ip_prefix || rule.remote_group_id || "any"}
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">{rule.description || "—"}</TableCell>
                  <TableCell className="text-right whitespace-nowrap">
                    <Button variant="ghost" size="icon-sm" onClick={() => openRule(rule)} aria-label="Edit rule" disabled={!rule.id}>
                      <Pencil className="size-4 text-muted-foreground" />
                    </Button>
                    <Button variant="ghost" size="icon-sm" onClick={() => setRuleToDelete(rule)} aria-label="Delete rule">
                      <Trash2 className="size-4 text-muted-foreground" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {rules.data.result.some((rule) => rule.direction === "egress") ? (
            <p className="border-t px-4 py-3 text-sm text-muted-foreground">
              Outbound traffic is always allowed. An egress rule is stored but restricts nothing.
            </p>
          ) : null}
        </div>
      )}

      <Dialog open={ruleOpen} onOpenChange={setRuleOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editing ? "Edit rule" : "Add rule"}</DialogTitle>
            <DialogDescription>
              {direction === "egress"
                ? "This rule was made through the API as an outbound rule. Outbound traffic is always allowed, so it restricts nothing."
                : "A rule allows inbound traffic that matches it. Rules only allow: there is no deny rule, and outbound traffic is always allowed."}
              {editing ? " Saving adds the changed rule and then removes the old one." : ""}
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-2">
            <div className="grid gap-1 text-sm">
              <span className="font-medium">Direction</span>
              <span className="text-muted-foreground" data-testid="rule-direction">
                {direction === "egress" ? "Egress (outbound)" : "Ingress (inbound) — the only direction a rule can filter"}
              </span>
            </div>
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
              <div className="grid gap-2">
                <Label htmlFor="rule-proto">Protocol</Label>
                <Select value={form.protocol} onValueChange={(protocol) => setForm({ ...form, protocol })}>
                  <SelectTrigger id="rule-proto" className="w-full" aria-label="Protocol">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {PROTOCOLS.map((p) => (
                      <SelectItem key={p.value} value={p.value}>
                        {p.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="rule-port-min">Port from</Label>
                <Input
                  id="rule-port-min"
                  type="number"
                  min={1}
                  max={65535}
                  value={hasPorts ? form.portMin : ""}
                  onChange={(e) => setForm({ ...form, portMin: e.target.value })}
                  placeholder={hasPorts ? "22" : "no ports"}
                  disabled={!hasPorts}
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="rule-port-max">Port to</Label>
                <Input
                  id="rule-port-max"
                  type="number"
                  min={1}
                  max={65535}
                  value={hasPorts ? form.portMax : ""}
                  onChange={(e) => setForm({ ...form, portMax: e.target.value })}
                  placeholder={hasPorts ? "22" : "no ports"}
                  disabled={!hasPorts}
                />
              </div>
            </div>
            <p className="-mt-2 text-xs text-muted-foreground">
              {hasPorts
                ? "Destination port on the server. Leave both empty for every port; fill only the first for one port."
                : "This protocol has no ports."}
            </p>
            <div className="grid gap-2">
              <Label htmlFor="rule-cidr">{direction === "egress" ? "Destination" : "Source"} (CIDR, empty = anywhere)</Label>
              <Input
                id="rule-cidr"
                className="font-mono"
                value={form.remoteIpPrefix}
                onChange={(e) => setForm({ ...form, remoteIpPrefix: e.target.value })}
                placeholder="203.0.113.0/24"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="rule-description">Description (optional)</Label>
              <Input
                id="rule-description"
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
                placeholder="SSH from the office"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRuleOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => saveRule.mutate()} disabled={saveRule.isPending}>
              {saveRule.isPending ? "Saving…" : editing ? "Save rule" : "Add rule"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!ruleToDelete} onOpenChange={(o) => !o && setRuleToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete rule</DialogTitle>
            <DialogDescription>
              Delete this {ruleToDelete?.direction || "ingress"} rule ({ruleToDelete?.protocol || "any"},{" "}
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
