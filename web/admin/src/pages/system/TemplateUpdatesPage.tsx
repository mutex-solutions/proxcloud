// TemplateUpdatesPage — newer VM and sandbox templates published on GitHub Packages (ghcr.io).
// Backed by /admin/template-updates (docs/template-updates.md). Every GitHub call uses the token of
// the GitHub integration; without it the page asks for one instead of listing anonymously.

import { useMemo, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { ApiError, apiFetch } from "@/lib/api"
import { hasPermission, useAdminMe } from "@/lib/auth"
import { fmtDateTime, timeAgo } from "@/lib/format"
import { useTabParam } from "@/lib/hooks"

type RateLimit = { limit: number; remaining: number; used: number; reset: string; resource?: string }
type Status = {
  configured: boolean
  integrationId?: string
  login?: string
  scopes?: string[]
  rateLimit?: RateLimit
  error?: string
  vmUpdatesEnabled: boolean
  sandboxMirrorEnabled: boolean
}
type Skipped = { tag: string; digest: string; reason: string }
type VerifiedVM = {
  package: string
  tag: string
  digest: string
  createdAt: string
  diskDigest: string
  diskBytes: number
  manifestSha256: string
  manifest: { build_version: string; source_commit: string; disk: { virtual_size_bytes: number } }
}
type InstalledVM = { serviceId: string; serviceName: string; node: string; vmid: number; version: string; sourceDigest?: string; published: boolean }
type VMLine = {
  line: string
  package: string
  os: string
  variant: "os" | "kubernetes"
  kubernetesVersion?: string
  latest?: VerifiedVM
  skipped?: Skipped[]
  installed: InstalledVM[]
  updateAvailable: boolean
}
type SandboxCandidate = { name: string; package: string; tag: string; digest: string; createdAt: string; reference: string }
type PinnedSandbox = { serviceId: string; serviceName: string; template: string; image: string; digest: string }
type SandboxRow = { name: string; latest?: SandboxCandidate; skipped?: Skipped[]; pinned: PinnedSandbox[]; updateAvailable: boolean }
type Discovery = {
  vm: VMLine[]
  sandbox: SandboxRow[]
  checkedAt: string
  rateLimit?: RateLimit
  providers: { id: string; name: string; error?: string }[]
}
type Receipt = {
  id: string
  payload: { package: string; tag: string; digest: string; serviceId: string; node: string; importStorage: string; diskStorage: string; publish: boolean }
  requestedBy: string
  createdAt: string
  updatedAt: string
  step: string
  vmid?: number
  code?: string
  message?: string
  warnings?: string[]
}
type History = { items: Receipt[]; jobs: Record<string, { state: string; attempts: number; maxAttempts: number; lastError?: string }> }
type Placement = { configuredImportStorage: string; nodes: { node: string; importStorages: string[]; diskStorages: string[] }[] }
type SandboxPin = { id: string; template: string; registry: string; tag: string; digest: string; ref: string; action: string; previous?: string; serviceId: string; actor: string; at: string; mirrorJob: string }
type SandboxReceipt = {
  id: string
  payload: { action: "mirror" | "rollback"; serviceId: string; name: string; registry: string; tag: string; digest: string }
  requestedBy: string
  createdAt: string
  updatedAt: string
  step: string
  code?: string
  message?: string
  warnings?: string[]
}
type SandboxImageState = {
  serviceId: string
  serviceName: string
  name: string
  prepull: string[]
  pin?: SandboxPin
  pulled: number
  total: number
  templates: { template: string; image: string; switched: boolean }[]
  imagesYaml?: string
  error?: string
}
type SandboxState = { enabled: boolean; items: SandboxReceipt[]; jobs: History["jobs"]; pins: SandboxPin[]; states: SandboxImageState[] }
type Provider = { id: string; name: string; config?: { provider?: string }; status?: string }

const BASE = "/admin/template-updates"
const short = (d?: string) => (d ? d.replace("sha256:", "").slice(0, 12) : "")
const gib = (b: number) => `${(b / 2 ** 30).toFixed(b < 2 ** 30 ? 2 : 1)} GiB`

function RateLimitNote({ rl }: { rl?: RateLimit }) {
  if (!rl) return null
  return (
    <span className="text-xs text-muted-foreground">
      GitHub API: {rl.remaining}/{rl.limit} left, resets {fmtDateTime(rl.reset)}
    </span>
  )
}

export default function TemplateUpdatesPage() {
  const qc = useQueryClient()
  const { data: me } = useAdminMe()
  const canManage = hasPermission(me?.permissions, "admin:service:manage")
  const [tab, setTab] = useTabParam("vm")
  const [refresh, setRefresh] = useState(0)

  const status = useQuery({ queryKey: ["tu-status"], queryFn: () => apiFetch<Status>(`${BASE}/status`) })
  const configured = status.data?.configured && !status.data?.error
  const disc = useQuery({
    queryKey: ["tu-discovery", refresh],
    queryFn: () => apiFetch<Discovery>(refresh ? `${BASE}?refresh=1` : BASE),
    enabled: !!configured,
    retry: false,
  })
  const history = useQuery({
    queryKey: ["tu-history"],
    queryFn: () => apiFetch<History>(`${BASE}/history`),
    refetchInterval: (q) => (q.state.data?.items?.some((r) => !["succeeded", "failed"].includes(r.step)) ? 5000 : false),
  })

  const [target, setTarget] = useState<VMLine | null>(null)
  const sbxEnabled = !!status.data?.sandboxMirrorEnabled
  const sbx = useQuery({
    queryKey: ["tu-sandbox"],
    queryFn: () => apiFetch<SandboxState>(`${BASE}/sandbox/state`),
    enabled: !!configured,
    refetchInterval: (q) =>
      q.state.data?.items?.some((r) => !["succeeded", "failed"].includes(r.step)) ||
      q.state.data?.states?.some((s) => s.total > 0 && (s.pulled < s.total || s.templates.some((t) => !t.switched)))
        ? 5000
        : false,
  })
  const [sbxTarget, setSbxTarget] = useState<{ row: SandboxRow; serviceId: string } | null>(null)
  const [sbxRollback, setSbxRollback] = useState<SandboxImageState | null>(null)

  return (
    <>
      <PageHeader
        title="Template updates"
        description="Newer VM templates and sandbox images published on GitHub Packages, compared with what this site runs."
        actions={
          <Button variant="outline" disabled={!configured || disc.isFetching} onClick={() => setRefresh((n) => n + 1)}>
            {disc.isFetching ? "Checking…" : "Check now"}
          </Button>
        }
      />

      <GitHubCard status={status.data} loading={status.isLoading} />

      {configured && (
        <Tabs value={tab} onValueChange={setTab} className="mt-4">
          <TabsList>
            <TabsTrigger value="vm">VM templates</TabsTrigger>
            <TabsTrigger value="sandbox">Sandbox templates</TabsTrigger>
            <TabsTrigger value="history">Jobs</TabsTrigger>
          </TabsList>
          {disc.error && (
            <Alert variant="destructive" className="mt-4">
              <AlertTitle>Could not check GitHub Packages</AlertTitle>
              <AlertDescription>{disc.error instanceof Error ? disc.error.message : String(disc.error)}</AlertDescription>
            </Alert>
          )}
          {disc.data?.providers.filter((p) => p.error).map((p) => (
            <Alert key={p.id} className="mt-4">
              <AlertTitle>{p.name}: installed state unknown</AlertTitle>
              <AlertDescription>{p.error} — its column shows nothing until the cluster can be read.</AlertDescription>
            </Alert>
          ))}
          <TabsContent value="vm">
            <VMTab rows={disc.data?.vm} loading={disc.isLoading} canManage={canManage} enabled={!!status.data?.vmUpdatesEnabled} onUpdate={setTarget} />
          </TabsContent>
          <TabsContent value="sandbox">
            <SandboxTab rows={disc.data?.sandbox} loading={disc.isLoading} state={sbx.data} enabled={sbxEnabled} canManage={canManage}
              onUpdate={(row, serviceId) => setSbxTarget({ row, serviceId })} onRollback={setSbxRollback} />
          </TabsContent>
          <TabsContent value="history">
            <HistoryTab data={history.data} loading={history.isLoading} />
          </TabsContent>
          {disc.data && (
            <p className="mt-2 text-xs text-muted-foreground">
              Checked {timeAgo(disc.data.checkedAt)}. <RateLimitNote rl={disc.data.rateLimit} />
            </p>
          )}
        </Tabs>
      )}

      {sbxTarget && (
        <SandboxUpdateDialog row={sbxTarget.row} serviceId={sbxTarget.serviceId} onClose={() => setSbxTarget(null)}
          onQueued={() => { setSbxTarget(null); void qc.invalidateQueries({ queryKey: ["tu-sandbox"] }) }} />
      )}
      {sbxRollback && (
        <SandboxRollbackDialog state={sbxRollback} onClose={() => setSbxRollback(null)}
          onQueued={() => { setSbxRollback(null); void qc.invalidateQueries({ queryKey: ["tu-sandbox"] }) }} />
      )}
      {target && (
        <VMUpdateDialog
          line={target}
          onClose={() => setTarget(null)}
          onQueued={() => {
            setTarget(null)
            setTab("history")
            void qc.invalidateQueries({ queryKey: ["tu-history"] })
          }}
        />
      )}
    </>
  )
}

function GitHubCard({ status, loading }: { status?: Status; loading: boolean }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">GitHub integration</CardTitle>
      </CardHeader>
      <CardContent className="space-y-2 text-sm">
        {loading ? (
          <span className="text-muted-foreground">Checking…</span>
        ) : !status?.configured ? (
          <Alert>
            <AlertTitle>Configure the GitHub integration</AlertTitle>
            <AlertDescription>
              Add a personal access token (classic) with <code>read:packages</code> under{" "}
              <Link to="/system/integrations" className="underline">System → Integrations → GitHub</Link>. Every GitHub
              call uses it, so checks are not rate-limited as anonymous; nothing is listed without it.
            </AlertDescription>
          </Alert>
        ) : status.error ? (
          <Alert variant="destructive">
            <AlertTitle>The GitHub token does not work</AlertTitle>
            <AlertDescription>
              {status.error}. Replace it under <Link to="/system/integrations" className="underline">Integrations</Link>.
            </AlertDescription>
          </Alert>
        ) : (
          <div className="flex flex-wrap items-center gap-3">
            <Badge variant="outline">Connected as {status.login}</Badge>
            {status.scopes?.map((s) => <Badge key={s} variant="secondary">{s}</Badge>)}
            <RateLimitNote rl={status.rateLimit} />
          </div>
        )}
        {status && !status.vmUpdatesEnabled && (
          <p className="text-xs text-muted-foreground">
            VM template imports are not enabled on this site (PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED); discovery still works.
          </p>
        )}
        {status && !status.sandboxMirrorEnabled && (
          <p className="text-xs text-muted-foreground">
            Sandbox image updates from the portal are not enabled on this site (PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED); use images.yaml and make upgrade.
          </p>
        )}
      </CardContent>
    </Card>
  )
}

function VMTab({ rows, loading, canManage, enabled, onUpdate }: {
  rows?: VMLine[]; loading: boolean; canManage: boolean; enabled: boolean; onUpdate: (l: VMLine) => void
}) {
  return (
    <Card>
      <CardContent className="pt-6">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Template</TableHead>
              <TableHead>Latest published</TableHead>
              <TableHead>Installed on this site</TableHead>
              <TableHead>Status</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">Checking GitHub Packages…</TableCell></TableRow>
            ) : !rows?.length ? (
              <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">No published VM artifacts were found.</TableCell></TableRow>
            ) : (
              rows.map((r) => (
                <TableRow key={r.line}>
                  <TableCell>
                    <div className="font-medium">{r.os}{r.variant === "kubernetes" ? ` · Kubernetes ${r.kubernetesVersion}` : ""}</div>
                    <div className="font-mono text-xs text-muted-foreground">{r.package}</div>
                  </TableCell>
                  <TableCell>
                    {r.latest ? (
                      <>
                        <div className="font-mono text-xs">{r.latest.tag}</div>
                        <div className="font-mono text-xs text-muted-foreground" title={r.latest.digest}>@{short(r.latest.digest)} · {gib(r.latest.diskBytes)} · {timeAgo(r.latest.createdAt)}</div>
                      </>
                    ) : (
                      <span className="text-muted-foreground">none valid</span>
                    )}
                    {!!r.skipped?.length && (
                      <div className="text-xs text-amber-600" title={r.skipped.map((s) => `${s.tag}: ${s.reason}`).join("\n")}>
                        {r.skipped.length} newer tag(s) refused by contract checks
                      </div>
                    )}
                  </TableCell>
                  <TableCell className="text-xs">
                    {r.installed.length === 0 ? (
                      <span className="text-muted-foreground">—</span>
                    ) : (
                      r.installed.map((i) => (
                        <div key={`${i.serviceId}/${i.vmid}`}>
                          {i.serviceName} · {i.node} · VM {i.vmid} · {i.version || "unversioned"}
                          {i.sourceDigest ? ` · @${i.sourceDigest}` : ""}{i.published ? "" : " (staged)"}
                        </div>
                      ))
                    )}
                  </TableCell>
                  <TableCell>
                    {r.updateAvailable ? <Badge>Update available</Badge> : r.latest ? <Badge variant="secondary">Up to date</Badge> : null}
                  </TableCell>
                  <TableCell className="text-right">
                    {r.latest && (
                      <Button size="sm" variant={r.updateAvailable ? "default" : "outline"} disabled={!canManage || !enabled}
                        title={!enabled ? "Not enabled on this site" : !canManage ? "Needs admin:service:manage" : undefined}
                        onClick={() => onUpdate(r)}>
                        Import…
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
        <p className="mt-3 text-xs text-muted-foreground">
          An import always creates a <b>new</b> template VM in pool pc-templates; existing templates are never changed or deleted.
        </p>
      </CardContent>
    </Card>
  )
}

function VMUpdateDialog({ line, onClose, onQueued }: { line: VMLine; onClose: () => void; onQueued: () => void }) {
  const latest = line.latest!
  const providers = useQuery({ queryKey: ["tu-providers"], queryFn: () => apiFetch<Provider[]>("/admin/service") })
  const pve = useMemo(() => (providers.data ?? []).filter((p) => p.config?.provider === "proxmox" && p.status !== "DISABLED"), [providers.data])
  const [serviceId, setServiceId] = useState("")
  const placement = useQuery({
    queryKey: ["tu-placement", serviceId],
    queryFn: () => apiFetch<Placement>(`${BASE}/placement/${encodeURIComponent(serviceId)}`),
    enabled: !!serviceId,
  })
  const [node, setNode] = useState("")
  const [importStorage, setImportStorage] = useState("")
  const [diskStorage, setDiskStorage] = useState("")
  const [publish, setPublish] = useState(false)
  const [confirm, setConfirm] = useState("")
  const nodeOpt = placement.data?.nodes.find((n) => n.node === node)

  const submit = useMutation({
    mutationFn: () =>
      apiFetch(`${BASE}/vm`, {
        method: "POST",
        body: { package: latest.package, digest: latest.digest, confirmDigest: confirm.trim(), serviceId, node, importStorage, diskStorage, publish: line.variant === "os" && publish },
      }),
    onSuccess: () => {
      toast.success("Import queued")
      onQueued()
    },
    onError: (e) => toast.error(e instanceof ApiError || e instanceof Error ? e.message : String(e)),
  })
  const ready = serviceId && node && importStorage && diskStorage && confirm.trim() === latest.digest

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>Import {latest.tag}</DialogTitle>
          <DialogDescription>
            The API downloads the disk by digest, verifies it (manifest sha256, SHA256SUMS, and Proxmox's own sha256 check on
            upload) and imports it as a new template. Nothing existing is modified.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3 text-sm">
          <dl className="grid grid-cols-[10rem_1fr] gap-x-3 gap-y-1">
            <dt className="text-muted-foreground">Artifact</dt><dd className="font-mono text-xs break-all">ghcr.io/mutex-solutions/{latest.package}@{latest.digest}</dd>
            <dt className="text-muted-foreground">Disk sha256</dt><dd className="font-mono text-xs break-all">{latest.diskDigest.replace("sha256:", "")}</dd>
            <dt className="text-muted-foreground">Size</dt><dd>{gib(latest.diskBytes)} file, {gib(latest.manifest.disk.virtual_size_bytes)} virtual</dd>
            <dt className="text-muted-foreground">Source commit</dt><dd className="font-mono text-xs">{latest.manifest.source_commit}</dd>
          </dl>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1">
              <Label>Proxmox provider</Label>
              <Select value={serviceId} onValueChange={(v) => { setServiceId(v); setNode(""); setImportStorage(""); setDiskStorage("") }}>
                <SelectTrigger><SelectValue placeholder="Pick a provider" /></SelectTrigger>
                <SelectContent>{pve.map((p) => <SelectItem key={p.id} value={p.id}>{p.name}</SelectItem>)}</SelectContent>
              </Select>
            </div>
            <div className="space-y-1">
              <Label>Node</Label>
              <Select value={node} onValueChange={(v) => {
                setNode(v)
                const o = placement.data?.nodes.find((n) => n.node === v)
                setImportStorage(o?.importStorages[0] ?? "")
                setDiskStorage("")
              }} disabled={!placement.data}>
                <SelectTrigger><SelectValue placeholder={placement.isLoading ? "Loading…" : "Pick a node"} /></SelectTrigger>
                <SelectContent>{placement.data?.nodes.map((n) => <SelectItem key={n.node} value={n.node}>{n.node}</SelectItem>)}</SelectContent>
              </Select>
            </div>
            <div className="space-y-1">
              <Label>Import storage (import content)</Label>
              <Select value={importStorage} onValueChange={setImportStorage} disabled={!nodeOpt}>
                <SelectTrigger><SelectValue placeholder="Pick a storage" /></SelectTrigger>
                <SelectContent>{nodeOpt?.importStorages.map((s) => <SelectItem key={s} value={s}>{s}</SelectItem>)}</SelectContent>
              </Select>
            </div>
            <div className="space-y-1">
              <Label>Template disk storage (images content)</Label>
              <Select value={diskStorage} onValueChange={setDiskStorage} disabled={!nodeOpt}>
                <SelectTrigger><SelectValue placeholder="Pick a storage" /></SelectTrigger>
                <SelectContent>{nodeOpt?.diskStorages.map((s) => <SelectItem key={s} value={s}>{s}</SelectItem>)}</SelectContent>
              </Select>
            </div>
          </div>
          {placement.error && <p className="text-xs text-destructive">{String((placement.error as Error).message)}</p>}
          {nodeOpt && nodeOpt.importStorages.length === 0 && (
            <p className="text-xs text-destructive">This node has no active storage with import content: enable it on a storage first.</p>
          )}
          {line.variant === "os" ? (
            <div className="space-y-1">
            <div className="flex items-center gap-2">
              <Switch id="tu-publish" checked={publish} onCheckedChange={setPublish} />
              <Label htmlFor="tu-publish">Publish to the OS catalog right away (otherwise it is staged for review)</Label>
            </div>
            {publish && (
              <p className="text-xs text-amber-600">
                An import is not boot-tested before it is published (the pipeline build is). A published import also counts
                as a catalog version for <code>images/build.py --keep</code> pruning on that node. Staging it and booting a clone first
                is the safer default.
              </p>
            )}
            </div>
          ) : (
            <p className="text-xs text-muted-foreground">
              A Kubernetes node image is never a catalog entry: reference the new template's VMID in the Kubernetes provider's version map.
            </p>
          )}
          <div className="space-y-1">
            <Label htmlFor="tu-confirm">Type or paste the exact digest to confirm</Label>
            <Input id="tu-confirm" className="font-mono text-xs" placeholder={latest.digest} value={confirm} onChange={(e) => setConfirm(e.target.value)} />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button disabled={!ready || submit.isPending} onClick={() => submit.mutate()}>
            {submit.isPending ? "Queuing…" : "Import as new template"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

const SBX_STEP: Record<string, string> = {
  queued: "Queued", mirroring: "Mirroring", mirrored: "Verifying digest", verified: "Verifying digest",
  prepull: "Updating prepull", pinned: "Prepull updated", succeeded: "Prepull updated", failed: "Failed",
}

// sandboxPhase is the one-line progress of a built-in image on a provider: the job's step while it runs, then
// "waiting for prepull on N/M nodes" until every Ready node pulled the pinned digest, then "switched" once the adoption
// job moved the templates.
function sandboxPhase(st: SandboxImageState | undefined, job: SandboxReceipt | undefined): { label: string; tone: "default" | "secondary" | "destructive" | "outline" } {
  if (job && !["succeeded", "failed"].includes(job.step)) return { label: SBX_STEP[job.step] ?? job.step, tone: "outline" }
  if (job?.step === "failed") return { label: `Failed: ${job.code ?? "error"}`, tone: "destructive" }
  if (!st?.pin) return { label: "", tone: "outline" }
  if (st.error) return { label: st.error, tone: "destructive" }
  if (st.total > 0 && st.pulled < st.total) return { label: `Waiting for prepull on ${st.pulled}/${st.total} nodes`, tone: "outline" }
  if (st.templates.length > 0 && st.templates.some((t) => !t.switched)) return { label: `Prepulled on ${st.pulled}/${st.total} nodes, switching templates`, tone: "outline" }
  return { label: "Switched", tone: "secondary" }
}

function SandboxTab({ rows, loading, state, enabled, canManage, onUpdate, onRollback }: {
  rows?: SandboxRow[]; loading: boolean; state?: SandboxState; enabled: boolean; canManage: boolean
  onUpdate: (r: SandboxRow, serviceId: string) => void; onRollback: (s: SandboxImageState) => void
}) {
  const latestJob = (name: string, serviceId: string) =>
    state?.items.filter((r) => r.payload.name === name && r.payload.serviceId === serviceId).sort((a, b) => b.createdAt.localeCompare(a.createdAt))[0]
  const stateOf = (name: string, serviceId: string) => state?.states.find((s) => s.name === name && s.serviceId === serviceId)
  return (
    <Card>
      <CardContent className="space-y-4 pt-6">
        {enabled ? (
          <Alert>
            <AlertTitle>Updates are mirrored, pre-pulled, then switched</AlertTitle>
            <AlertDescription>
              <b>Update</b> copies the published image by digest into the sandbox registry (<code>library/</code>, same digest,
              verified), points the prepull DaemonSets at it, and the adoption job switches each template once every Ready node
              has pulled it — the previous image stays until then and is never deleted. <b>Rollback</b> points the prepull back at
              the previous pin. Commit the shown <code>images.yaml</code> line so the repository matches; <code>make upgrade</code>{" "}
              reads these pins and never renders an older image.
            </AlertDescription>
          </Alert>
        ) : (
          <Alert>
            <AlertTitle>Sandbox image updates from the portal are not enabled on this site</AlertTitle>
            <AlertDescription>
              To roll out a newer image: put its pin line into <code>deploy/templates/images.yaml</code>{" "}
              and run <code>make upgrade</code> — stage 55 copies it into the sandbox registry and pre-pulls it on every node,
              then the adoption job switches templates once every node has it (the previous pin stays until then). The operator
              can turn on portal updates with <code>PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED=true</code>.
            </AlertDescription>
          </Alert>
        )}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Template image</TableHead>
              <TableHead>Latest published</TableHead>
              <TableHead>Pinned on this site</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableRow><TableCell colSpan={4} className="text-center text-muted-foreground">Checking GitHub Packages…</TableCell></TableRow>
            ) : !rows?.length ? (
              <TableRow><TableCell colSpan={4} className="text-center text-muted-foreground">No sandbox images found.</TableCell></TableRow>
            ) : (
              rows.map((r) => {
                const providers = Array.from(new Map(r.pinned.map((p) => [p.serviceId, p.serviceName])).entries())
                return (
                  <TableRow key={r.name}>
                    <TableCell className="font-mono text-xs">sandbox-{r.name}</TableCell>
                    <TableCell>
                      {r.latest ? (
                        <>
                          <div className="font-mono text-xs">{r.latest.tag} @{short(r.latest.digest)}</div>
                          <div className="text-xs text-muted-foreground">{timeAgo(r.latest.createdAt)}</div>
                          <Button size="sm" variant="link" className="h-auto p-0 text-xs" onClick={() => {
                            void navigator.clipboard?.writeText(`  ${r.name}: ${r.latest!.reference}`)
                            toast.success("images.yaml pin line copied")
                          }}>Copy images.yaml pin</Button>
                        </>
                      ) : (
                        <span className="text-muted-foreground">not published</span>
                      )}
                    </TableCell>
                    <TableCell className="text-xs">
                      {r.pinned.length === 0 ? <span className="text-muted-foreground">not used by a sandbox provider</span> : r.pinned.map((p) => (
                        <div key={`${p.serviceId}/${p.template}`} title={p.image}>{p.serviceName} · {p.template} · @{short(p.digest) || "tag only"}</div>
                      ))}
                    </TableCell>
                    <TableCell className="space-y-2">
                      {r.updateAvailable ? <Badge>Update available</Badge> : r.latest && r.pinned.length ? <Badge variant="secondary">Up to date</Badge> : null}
                      {providers.map(([serviceId, serviceName]) => {
                        const st = stateOf(r.name, serviceId)
                        const job = latestJob(r.name, serviceId)
                        const phase = sandboxPhase(st, job)
                        const busy = !!job && !["succeeded", "failed"].includes(job.step)
                        return (
                          <div key={serviceId} className="space-y-1 text-xs">
                            {providers.length > 1 && <div className="text-muted-foreground">{serviceName}</div>}
                            {phase.label && <Badge variant={phase.tone}>{phase.label}</Badge>}
                            {st?.imagesYaml && (
                              <Button size="sm" variant="link" className="h-auto p-0 text-xs" title={st.imagesYaml} onClick={() => {
                                void navigator.clipboard?.writeText(st.imagesYaml!)
                                toast.success("images.yaml line of the portal's pin copied: commit it")
                              }}>Copy the images.yaml line to commit</Button>
                            )}
                            <div className="flex gap-2">
                              {r.latest && (
                                <Button size="sm" variant={r.updateAvailable ? "default" : "outline"} disabled={!enabled || !canManage || busy}
                                  title={!enabled ? "Not enabled on this site" : !canManage ? "Needs admin:service:manage" : busy ? "An update of this image is running" : undefined}
                                  onClick={() => onUpdate(r, serviceId)}>Update…</Button>
                              )}
                              {st?.pin?.previous && (
                                <Button size="sm" variant="outline" disabled={!enabled || !canManage || busy}
                                  title={!enabled ? "Not enabled on this site" : !canManage ? "Needs admin:service:manage" : undefined}
                                  onClick={() => onRollback(st)}>Rollback…</Button>
                              )}
                            </div>
                          </div>
                        )
                      })}
                    </TableCell>
                  </TableRow>
                )
              })
            )}
          </TableBody>
        </Table>
        {!!state?.items.length && (
          <div className="space-y-1">
            <div className="text-sm font-medium">Sandbox update jobs</div>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Requested</TableHead>
                  <TableHead>Image</TableHead>
                  <TableHead>Progress</TableHead>
                  <TableHead>Result</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {state.items.map((j) => {
                  const dj = state.jobs[j.id]
                  return (
                    <TableRow key={j.id}>
                      <TableCell className="text-xs">{fmtDateTime(j.createdAt)}<div className="text-muted-foreground">{j.requestedBy}</div></TableCell>
                      <TableCell className="font-mono text-xs">{j.payload.action} sandbox-{j.payload.name}:{j.payload.tag} @{short(j.payload.digest)}</TableCell>
                      <TableCell>
                        <Badge variant={j.step === "failed" ? "destructive" : j.step === "succeeded" ? "secondary" : "outline"}>{SBX_STEP[j.step] ?? j.step}</Badge>
                        {dj && dj.state !== "succeeded" && <div className="text-xs text-muted-foreground">job {dj.state}, attempt {dj.attempts}/{dj.maxAttempts}</div>}
                      </TableCell>
                      <TableCell className="text-xs">
                        {j.code ? <code>{j.code}</code> : null} {j.message}
                        {j.warnings?.map((w) => <div key={w} className="text-amber-600">{w}</div>)}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function SandboxUpdateDialog({ row, serviceId, onClose, onQueued }: { row: SandboxRow; serviceId: string; onClose: () => void; onQueued: () => void }) {
  const latest = row.latest!
  const [confirm, setConfirm] = useState("")
  const submit = useMutation({
    mutationFn: () => apiFetch(`${BASE}/sandbox`, { method: "POST", body: { serviceId, name: row.name, digest: latest.digest, confirmDigest: confirm.trim() } }),
    onSuccess: () => { toast.success("Mirror queued"); onQueued() },
    onError: (e) => toast.error(e instanceof ApiError || e instanceof Error ? e.message : String(e)),
  })
  const pinned = row.pinned.filter((p) => p.serviceId === serviceId)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>Update sandbox-{row.name} to {latest.tag}</DialogTitle>
          <DialogDescription>
            The API re-verifies the digest on GitHub Packages, copies the image by digest into the sandbox registry (the same
            digest, checked after the copy), then points the prepull DaemonSets at it. Templates switch once every Ready node has
            pulled it; until then sandboxes keep starting from the current image.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3 text-sm">
          <dl className="grid grid-cols-[8rem_1fr] gap-x-3 gap-y-1">
            <dt className="text-muted-foreground">Source</dt><dd className="font-mono text-xs break-all">ghcr.io/mutex-solutions/{latest.package}@{latest.digest}</dd>
            <dt className="text-muted-foreground">Tag</dt><dd className="font-mono text-xs">{latest.tag}</dd>
            <dt className="text-muted-foreground">Runs now</dt>
            <dd className="font-mono text-xs break-all">{pinned.length ? pinned.map((p) => <div key={p.template}>{p.template}: {p.image}</div>) : "—"}</dd>
          </dl>
          <div className="space-y-1">
            <Label htmlFor="sbx-confirm">Type or paste the exact digest to confirm</Label>
            <Input id="sbx-confirm" className="font-mono text-xs" placeholder={latest.digest} value={confirm} onChange={(e) => setConfirm(e.target.value)} />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button disabled={confirm.trim() !== latest.digest || submit.isPending} onClick={() => submit.mutate()}>
            {submit.isPending ? "Queuing…" : "Mirror and pre-pull"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function SandboxRollbackDialog({ state, onClose, onQueued }: { state: SandboxImageState; onClose: () => void; onQueued: () => void }) {
  const previous = state.pin?.previous ?? ""
  const digest = previous.includes("@") ? previous.slice(previous.indexOf("@") + 1) : ""
  const [confirm, setConfirm] = useState("")
  const submit = useMutation({
    mutationFn: () => apiFetch(`${BASE}/sandbox/rollback`, { method: "POST", body: { serviceId: state.serviceId, name: state.name, confirmDigest: confirm.trim() } }),
    onSuccess: () => { toast.success("Rollback queued"); onQueued() },
    onError: (e) => toast.error(e instanceof ApiError || e instanceof Error ? e.message : String(e)),
  })
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>Roll back sandbox-{state.name}</DialogTitle>
          <DialogDescription>
            Points the prepull DaemonSets back at the previous image (still in the sandbox registry: images are never deleted).
            Templates switch back once every Ready node has it.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3 text-sm">
          <dl className="grid grid-cols-[8rem_1fr] gap-x-3 gap-y-1">
            <dt className="text-muted-foreground">Now</dt><dd className="font-mono text-xs break-all">{state.pin?.ref}</dd>
            <dt className="text-muted-foreground">Back to</dt><dd className="font-mono text-xs break-all">{previous}</dd>
          </dl>
          <div className="space-y-1">
            <Label htmlFor="sbx-rb-confirm">Type or paste the digest to roll back to</Label>
            <Input id="sbx-rb-confirm" className="font-mono text-xs" placeholder={digest} value={confirm} onChange={(e) => setConfirm(e.target.value)} />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button variant="destructive" disabled={!digest || confirm.trim() !== digest || submit.isPending} onClick={() => submit.mutate()}>
            {submit.isPending ? "Queuing…" : "Roll back"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

const STEP_LABEL: Record<string, string> = {
  queued: "Queued", uploading: "Downloading + uploading", uploaded: "Verified upload", creating: "Creating VM",
  imported: "Importing disk", templated: "Finalizing", succeeded: "Done", failed: "Failed",
}

function HistoryTab({ data, loading }: { data?: History; loading: boolean }) {
  return (
    <Card>
      <CardContent className="pt-6">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Requested</TableHead>
              <TableHead>Artifact</TableHead>
              <TableHead>Target</TableHead>
              <TableHead>Progress</TableHead>
              <TableHead>Result</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">Loading…</TableCell></TableRow>
            ) : !data?.items.length ? (
              <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">No template update jobs yet.</TableCell></TableRow>
            ) : (
              data.items.map((r) => {
                const job = data.jobs[r.id]
                return (
                  <TableRow key={r.id}>
                    <TableCell className="text-xs">{fmtDateTime(r.createdAt)}<div className="text-muted-foreground">{r.requestedBy}</div></TableCell>
                    <TableCell><div className="font-mono text-xs">{r.payload.tag}</div><div className="font-mono text-xs text-muted-foreground">@{short(r.payload.digest)}</div></TableCell>
                    <TableCell className="text-xs">{r.payload.node} · {r.payload.diskStorage}{r.vmid ? ` · VM ${r.vmid}` : ""}</TableCell>
                    <TableCell>
                      <Badge variant={r.step === "failed" ? "destructive" : r.step === "succeeded" ? "secondary" : "outline"}>{STEP_LABEL[r.step] ?? r.step}</Badge>
                      {job && job.state !== "succeeded" && <div className="text-xs text-muted-foreground">job {job.state}, attempt {job.attempts}/{job.maxAttempts}</div>}
                    </TableCell>
                    <TableCell className="text-xs">
                      {r.message}
                      {r.warnings?.map((w) => <div key={w} className="text-amber-600">{w}</div>)}
                    </TableCell>
                  </TableRow>
                )
              })
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  )
}
