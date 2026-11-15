// TemplatesStep.tsx — step 5 of the Platform setup page. Done as soon as the provider has a published OS image,
// however it got there (a site that built its templates with images/build.py sees no sub-steps). Otherwise it walks
// the template update path of /admin/template-updates (docs/template-updates.md): the flag, the GitHub token, the
// smoke-test network, the imports, and one explicit click that publishes every template whose smoke test passed.
import { useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { useAdminGet } from "@/lib/hooks"
import { STATE, type Placement, type Status, type TemplateState, type Templates, type VMLine } from "@/pages/system/TemplateUpdatesPage"
import { CopyBlock, ErrorNote, LINK, StateBadge, StepCard, errMsg, useClusterInfo, useInvalidateSetup, type SetupStep } from "./common"

const BASE = "/admin/template-updates"
const short = (d: string) => d.replace("sha256:", "").slice(0, 12)
type Outcome = { label: string; ok: boolean; message: string }

// Sub runs calls one after the other (each import and each publish is its own audited request) and keeps the result
// of every call, so one refusal does not hide the others.
async function runEach<T>(items: T[], label: (t: T) => string, call: (t: T) => Promise<unknown>): Promise<Outcome[]> {
  const out: Outcome[] = []
  for (const it of items) {
    try {
      await call(it)
      out.push({ label: label(it), ok: true, message: "done" })
    } catch (e) {
      out.push({ label: label(it), ok: false, message: errMsg(e) })
    }
  }
  return out
}

function Outcomes({ items }: { items?: Outcome[] }) {
  if (!items?.length) return null
  return (
    <ul className="grid gap-1 text-sm" aria-label="Results">
      {items.map((o) => (
        <li key={o.label} className={o.ok ? "" : "text-destructive"}>{o.label}: {o.message}</li>
      ))}
    </ul>
  )
}

function SubStep({ n, title, done, children }: { n: number; title: string; done?: boolean; children: React.ReactNode }) {
  return (
    <section className="grid gap-3 border-t pt-4" aria-label={title}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-medium">5.{n} {title}</h3>
        <StateBadge state={done === undefined ? "unknown" : done ? "done" : "todo"} />
      </div>
      {children}
    </section>
  )
}

export function TemplatesStep({ step, serviceId }: { step?: SetupStep; serviceId: string }) {
  return (
    <StepCard
      n={5}
      title="OS templates"
      step={step}
      description="Tenants create servers from published templates. Done once the provider publishes one OS image, however it was built."
    >
      {!serviceId ? (
        <p className="text-sm text-muted-foreground">Save a Proxmox provider first (step 2).</p>
      ) : step?.state === "todo" ? (
        <TemplateSubSteps serviceId={serviceId} />
      ) : step?.state !== "done" ? (
        <p className="text-sm text-muted-foreground">Only the cluster knows its templates: this step shows what to do once the cluster has answered (Check again).</p>
      ) : null}
    </StepCard>
  )
}

function TemplateSubSteps({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient()
  const invalidate = useInvalidateSetup()
  const status = useQuery({ queryKey: ["tu-status"], queryFn: () => apiFetch<Status>(`${BASE}/status`), retry: false })
  const flag = status.data?.vmUpdatesEnabled
  const tokenOK = !!status.data?.configured && !status.data?.error
  const templates = useQuery({
    queryKey: ["tu-templates"],
    queryFn: () => apiFetch<Templates>(`${BASE}/templates`),
    refetchInterval: (q) => (q.state.data?.items?.some((t) => ["importing", "staged", "smoke_running"].includes(t.state)) ? 5000 : false),
  })
  const mine = (templates.data?.items ?? []).filter((t) => t.serviceId === serviceId && t.variant === "os")
  const passed = mine.filter((t) => t.actions.publish)
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["tu-templates"] })
    invalidate(serviceId)
  }
  const publish = useMutation({
    mutationFn: () => runEach(passed, (t) => `${t.slug} ${t.tag} on ${t.node}`, (t: TemplateState) =>
      apiFetch(`${BASE}/templates/${encodeURIComponent(t.id)}/publish`, { method: "POST", body: { confirmDigest: t.digest } })),
    onSettled: refresh,
  })

  return (
    <div className="grid gap-4">
      <ErrorNote title="Template updates could not be read" error={status.error} />

      <SubStep n={1} title="Template updates turned on" done={status.data ? flag : undefined}>
        {status.data && !flag ? (
          <div className="grid gap-2 text-sm">
            <p>
              The API reads the flag when it starts, so the console cannot turn it on. On a Compose install run the
              installer again with it; elsewhere set <code>PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true</code> and restart the API.
            </p>
            <CopyBlock text="./proxcloud-up.sh install --template-updates" label="installer command" />
          </div>
        ) : null}
      </SubStep>

      <SubStep n={2} title="GitHub token" done={status.data ? tokenOK : undefined}>
        {tokenOK ? (
          <p className="text-sm text-muted-foreground">Connected as {status.data?.login}.</p>
        ) : status.data ? (
          <GitHubToken status={status.data} onSaved={() => { void qc.invalidateQueries({ queryKey: ["tu-status"] }); invalidate() }} />
        ) : null}
      </SubStep>

      <SmokeNetwork serviceId={serviceId} />

      <SubStep n={4} title="Import" done={templates.data ? mine.length > 0 : undefined}>
        {flag && tokenOK ? <ImportForm serviceId={serviceId} onQueued={refresh} /> : (
          <p className="text-sm text-muted-foreground">Needs template updates turned on and a GitHub token.</p>
        )}
        {templates.isLoading ? <Skeleton className="h-16" /> : <ImportedTable items={mine} />}
        <ErrorNote title="The imported templates could not be read" error={templates.error} />
      </SubStep>

      <SubStep n={5} title="Publish" done={templates.data ? mine.some((t) => t.state.startsWith("published")) : undefined}>
        <p className="text-sm text-muted-foreground">
          Publishing puts a template in the catalog: new servers can use it, and the newest published version becomes
          the default. Nothing is published without this click. It publishes each template whose smoke test passed, one
          request each.
        </p>
        <div>
          <Button onClick={() => publish.mutate()} disabled={!passed.length || publish.isPending}>
            {publish.isPending ? "Publishing…" : `Publish all that passed (${passed.length})`}
          </Button>
        </div>
        <Outcomes items={publish.data} />
      </SubStep>
    </div>
  )
}

function GitHubToken({ status, onSaved }: { status: Status; onSaved: () => void }) {
  const [token, setToken] = useState("")
  const save = useMutation({
    mutationFn: () => {
      const body = { thirdParty: "GitHub", name: "GitHub", config: {}, secret: { token: token.trim() } }
      return status.integrationId
        ? apiFetch(`/admin/integrations/${status.integrationId}`, { method: "PUT", body })
        : apiFetch("/admin/integrations", { method: "POST", body })
    },
    onSuccess: () => {
      setToken("")
      toast.success("GitHub token saved")
      onSaved()
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  return (
    <div className="grid gap-2 text-sm">
      {status.error ? <p className="text-destructive">The saved token does not work: {status.error}</p> : null}
      <p className="text-muted-foreground">
        Image versions are listed through the GitHub Packages API, which has no anonymous access: a classic personal
        access token with <code>read:packages</code>. It is checked with GitHub, stored encrypted and never shown again
        (also under <Link to="/system/integrations" className="underline underline-offset-4">Integrations</Link>).
      </p>
      <div className="flex flex-wrap items-end gap-3">
        <div className="grid gap-2">
          <Label htmlFor="tu-token">Personal access token (classic)</Label>
          <Input id="tu-token" type="password" className="w-72 font-mono" value={token} onChange={(e) => setToken(e.target.value)} placeholder="ghp_…" autoComplete="new-password" />
        </div>
        <Button onClick={() => save.mutate()} disabled={!token.trim() || save.isPending}>{save.isPending ? "Saving…" : "Save token"}</Button>
      </div>
    </div>
  )
}

function SmokeNetwork({ serviceId }: { serviceId: string }) {
  const provider = useAdminGet<{ config?: Record<string, any> }>(`/admin/service/${serviceId}`)
  const stored = provider.data?.config?.templateSmoke
  return (
    <SubStep n={3} title="Smoke-test network" done={provider.data ? !!stored?.bridge : undefined}>
      <ErrorNote title="The provider could not be read" error={provider.error} />
      {provider.data ? <SmokeForm key={provider.dataUpdatedAt} serviceId={serviceId} stored={stored ?? {}} /> : null}
    </SubStep>
  )
}

function SmokeForm({ serviceId, stored }: { serviceId: string; stored: Record<string, any> }) {
  const invalidate = useInvalidateSetup()
  const cluster = useClusterInfo(serviceId)
  const [f, setF] = useState({
    bridge: String(stored.bridge ?? ""), vlan: stored.vlan ? String(stored.vlan) : "",
    ip: String(stored.ip ?? ""), gateway: String(stored.gateway ?? ""), nameserver: String(stored.nameserver ?? ""),
  })
  const save = useMutation({
    mutationFn: () => {
      const smoke: Record<string, unknown> = { bridge: f.bridge.trim() }
      if (Number(f.vlan) > 0) smoke.vlan = Number(f.vlan)
      for (const k of ["ip", "gateway", "nameserver"] as const) if (f[k].trim()) smoke[k] = f[k].trim()
      return apiFetch(`/admin/service/${serviceId}/update`, { method: "PUT", body: { config: { templateSmoke: smoke } } })
    },
    onSuccess: () => {
      toast.success("Smoke-test network saved")
      invalidate(serviceId)
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  const field = (k: keyof typeof f, label: string, placeholder: string, extra?: React.InputHTMLAttributes<HTMLInputElement>) => (
    <div className="grid gap-2">
      <Label htmlFor={`smoke-${k}`}>{label}</Label>
      <Input id={`smoke-${k}`} className="font-mono" value={f[k]} onChange={(e) => setF({ ...f, [k]: e.target.value })} placeholder={placeholder} {...extra} />
    </div>
  )
  return (
    <div className="grid gap-3">
      <p className="text-sm text-muted-foreground">
        Every imported template boots once as a throwaway VM on this network before it can be published. An untagged
        network on a bridge that carries a node's address is refused: tag it with a VLAN. A blank address means DHCP.
      </p>
      <div className="grid gap-4 sm:grid-cols-5">
        {field("bridge", "Bridge", "vmbr0", { list: "smoke-bridges" })}
        {field("vlan", "VLAN tag", "untagged", { type: "number", min: 0, max: 4094 })}
        {field("ip", "Address (CIDR)", "DHCP")}
        {field("gateway", "Gateway", "192.0.2.1")}
        {field("nameserver", "Nameserver", "192.0.2.53")}
      </div>
      <datalist id="smoke-bridges">{[...new Set((cluster.data?.bridges ?? []).map((b) => b.iface))].map((b) => <option key={b} value={b} />)}</datalist>
      <div>
        <Button variant="outline" onClick={() => save.mutate()} disabled={!f.bridge.trim() || save.isPending}>
          {save.isPending ? "Saving…" : "Save smoke-test network"}
        </Button>
      </div>
    </div>
  )
}

function ImportForm({ serviceId, onQueued }: { serviceId: string; onQueued: () => void }) {
  const disc = useQuery({ queryKey: ["tu-discovery", 0], queryFn: () => apiFetch<{ vm: VMLine[] }>(BASE), retry: false })
  const placement = useQuery({ queryKey: ["tu-placement", serviceId], queryFn: () => apiFetch<Placement>(`${BASE}/placement/${encodeURIComponent(serviceId)}`) })
  const lines = (disc.data?.vm ?? []).filter((l) => l.variant === "os" && l.latest && !l.latestWithdrawn)
  const nodeOpts = placement.data?.nodes ?? []
  const [picked, setPicked] = useState<string[]>([])
  const [nodes, setNodes] = useState<string[]>([])
  const [disk, setDisk] = useState("")
  const chosenNodes = nodeOpts.filter((n) => nodes.includes(n.node))
  // A template disk storage every chosen node has.
  const disks = chosenNodes.length ? chosenNodes.map((n) => n.diskStorages).reduce((a, b) => a.filter((s) => b.includes(s))) : []
  const importStoreOf = (n: Placement["nodes"][number]) =>
    n.importStorages.includes(placement.data?.configuredImportStorage ?? "") ? placement.data!.configuredImportStorage : n.importStorages[0]
  const jobs = lines.filter((l) => picked.includes(l.line)).flatMap((l) => chosenNodes.map((n) => ({ l, n })))
  const run = useMutation({
    mutationFn: () => runEach(jobs, ({ l, n }) => `${l.os} ${l.latest!.tag} on ${n.node}`, ({ l, n }) =>
      apiFetch(`${BASE}/vm`, {
        method: "POST",
        // The digest is the one listed here; picking it in this list is the confirmation the API asks for.
        body: { package: l.package, digest: l.latest!.digest, confirmDigest: l.latest!.digest, serviceId, node: n.node, importStorage: importStoreOf(n), diskStorage: disk },
      })),
    onSettled: onQueued,
  })
  const toggle = (list: string[], set: (v: string[]) => void, v: string) => set(list.includes(v) ? list.filter((x) => x !== v) : [...list, v])

  if (disc.isLoading || placement.isLoading) return <Skeleton className="h-24" />
  return (
    <div className="grid gap-3">
      <ErrorNote title="Published images could not be listed" error={disc.error} />
      <ErrorNote title="The nodes could not be read" error={placement.error} />
      <fieldset className="grid gap-2">
        <legend className="mb-1 text-sm font-medium">OS images (latest published version)</legend>
        {lines.length ? lines.map((l) => (
          <label key={l.line} className="flex items-center gap-2 text-sm">
            <Checkbox checked={picked.includes(l.line)} onCheckedChange={() => toggle(picked, setPicked, l.line)} />
            <span>{l.os}</span>
            <span className="font-mono text-xs text-muted-foreground">{l.latest!.tag} @{short(l.latest!.digest)}</span>
          </label>
        )) : <p className="text-sm text-muted-foreground">No published OS image was found.</p>}
      </fieldset>
      <fieldset className="grid gap-2">
        <legend className="mb-1 text-sm font-medium">Nodes</legend>
        <div className="flex flex-wrap gap-4">
          {nodeOpts.map((n) => (
            <label key={n.node} className="flex items-center gap-2 text-sm">
              <Checkbox checked={nodes.includes(n.node)} disabled={!n.importStorages.length} onCheckedChange={() => { toggle(nodes, setNodes, n.node); setDisk("") }} />
              {n.node}{n.importStorages.length ? "" : " (no import store)"}
            </label>
          ))}
        </div>
      </fieldset>
      <div className="grid gap-2 sm:w-72">
        <Label htmlFor="tu-disk">Template disk storage</Label>
        <Select value={disk} onValueChange={setDisk} disabled={!disks.length}>
          <SelectTrigger id="tu-disk"><SelectValue placeholder={chosenNodes.length ? "Pick a storage" : "Pick nodes first"} /></SelectTrigger>
          <SelectContent>{disks.map((d) => <SelectItem key={d} value={d}>{d}</SelectItem>)}</SelectContent>
        </Select>
      </div>
      <div>
        <Button onClick={() => run.mutate()} disabled={!jobs.length || !disk || run.isPending}>
          {run.isPending ? "Queuing…" : `Import ${jobs.length} template${jobs.length === 1 ? "" : "s"}`}
        </Button>
      </div>
      <p className="text-xs text-muted-foreground">
        Each import creates a new, staged template in pool <code>pc-templates</code> (tenants do not see it) and starts its smoke test when it finishes.
      </p>
      <Outcomes items={run.data} />
    </div>
  )
}

function ImportedTable({ items }: { items: TemplateState[] }) {
  if (!items.length) return <p className="text-sm text-muted-foreground">No template was imported on this provider yet.</p>
  return (
    <div className="overflow-x-auto">
      <Table aria-label="Imported templates">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Template</TableHead>
            <TableHead>Node</TableHead>
            <TableHead>State</TableHead>
            <TableHead>Smoke test</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((t) => {
            const st = STATE[t.state] ?? { label: t.state, variant: "outline" as const }
            return (
              <TableRow key={t.id}>
                <TableCell><span className="font-medium">{t.slug}</span> <span className="font-mono text-xs text-muted-foreground">{t.tag}</span></TableCell>
                <TableCell className="text-sm">{t.node}{t.vmid ? ` · VM ${t.vmid}` : ""}</TableCell>
                <TableCell><Badge variant={st.variant}>{st.label}</Badge></TableCell>
                <TableCell className="text-xs">{t.smoke?.result ?? t.smoke?.step ?? "not run"}{t.smoke?.code ? ` (${t.smoke.code})` : ""}</TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
      <Link to="/system/template-updates?tab=imported" className={`${LINK} mt-2 inline-block`}>Smoke-test again or withdraw on the Template updates page</Link>
    </div>
  )
}
