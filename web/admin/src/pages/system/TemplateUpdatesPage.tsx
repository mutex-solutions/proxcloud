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
import { Checkbox } from "@/components/ui/checkbox"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
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
type InstalledVM = { serviceId: string; serviceName: string; node: string; vmid: number; version: string; sourceDigest?: string; published: boolean; withdrawn?: boolean }
type Withdrawal = { id: string; package: string; tag: string; reason: string; actor: string; at: string; vmid?: number }
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
  latestWithdrawn?: Withdrawal
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
  payload: { package: string; tag: string; digest: string; serviceId: string; node: string; importStorage: string; diskStorage: string }
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
type SmokeSummary = { jobId: string; step?: string; result?: "PASS" | "FAIL"; code?: string; message?: string; startedAt?: string; finishedAt?: string }
type TemplateState = {
  id: string
  package: string
  tag: string
  digest: string
  os: string
  variant: "os" | "kubernetes"
  kubernetesVersion?: string
  buildVersion: string
  slug: string
  serviceId: string
  node: string
  vmid?: number
  state: string
  smoke?: SmokeSummary
  withdrawal?: Withdrawal
  importedAt: string
  requestedBy: string
  publishedOnLine: number
  versionsMapLine?: string
  actions: { smoke: boolean; publish: boolean; withdraw: boolean; lastPublished: boolean; lastOnProviders?: string[]; nodesLosingLast?: string[] }
  warnings?: string[]
}
type SmokeReceipt = {
  id: string
  payload: { tag: string; digest: string; node: string; templateVmid: number }
  auto?: boolean
  requestedBy: string
  createdAt: string
  step: string
  vmid?: number
  result?: string
  code?: string
  message?: string
  startedAt?: string
  finishedAt?: string
  keyFingerprint?: string
  checks?: { osPrettyName?: string; cloudInitStatus?: string; rootFsBytes?: number; templateDiskBytes?: number; kubelet?: string }
  warnings?: string[]
}
type Templates = { items: TemplateState[]; smokes: SmokeReceipt[]; vmUpdatesEnabled: boolean }
type Placement = { configuredImportStorage: string; nodes: { node: string; importStorages: string[]; diskStorages: string[] }[] }
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

  const templates = useQuery({
    queryKey: ["tu-templates"],
    queryFn: () => apiFetch<Templates>(`${BASE}/templates`),
    refetchInterval: (q) => (q.state.data?.items?.some((t) => t.state === "smoke_running" || t.state === "importing") ? 5000 : false),
  })
  const [target, setTarget] = useState<VMLine | null>(null)
  const [action, setAction] = useState<{ kind: "smoke" | "publish" | "withdraw"; t: TemplateState } | null>(null)

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
            <TabsTrigger value="imported">Imported templates</TabsTrigger>
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
          <TabsContent value="imported">
            <ImportedTab data={templates.data} loading={templates.isLoading} canManage={canManage} onAction={(kind, t) => setAction({ kind, t })} />
          </TabsContent>
          <TabsContent value="sandbox">
            <SandboxTab rows={disc.data?.sandbox} loading={disc.isLoading} />
          </TabsContent>
          <TabsContent value="history">
            <HistoryTab data={history.data} loading={history.isLoading} />
            <SmokeHistory smokes={templates.data?.smokes} />
          </TabsContent>
          {disc.data && (
            <p className="mt-2 text-xs text-muted-foreground">
              Checked {timeAgo(disc.data.checkedAt)}. <RateLimitNote rl={disc.data.rateLimit} />
            </p>
          )}
        </Tabs>
      )}

      {target && (
        <VMUpdateDialog
          line={target}
          onClose={() => setTarget(null)}
          onQueued={() => {
            setTarget(null)
            setTab("history")
            void qc.invalidateQueries({ queryKey: ["tu-history"] })
            void qc.invalidateQueries({ queryKey: ["tu-templates"] })
          }}
        />
      )}

      {action && (
        <LifecycleDialog
          kind={action.kind}
          t={action.t}
          onClose={() => setAction(null)}
          onDone={() => {
            setAction(null)
            void qc.invalidateQueries({ queryKey: ["tu-templates"] })
            void qc.invalidateQueries({ queryKey: ["tu-discovery"] })
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
                    {r.latestWithdrawn && (
                      <div className="text-xs text-destructive" title={r.latestWithdrawn.reason}>
                        Withdrawn {fmtDateTime(r.latestWithdrawn.at)}: {r.latestWithdrawn.reason}
                      </div>
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
                          {i.sourceDigest ? ` · @${i.sourceDigest}` : ""}{i.withdrawn ? " (withdrawn)" : i.published ? "" : " (staged)"}
                        </div>
                      ))
                    )}
                  </TableCell>
                  <TableCell>
                    {r.latestWithdrawn ? <Badge variant="destructive">Withdrawn</Badge> : r.updateAvailable ? <Badge>Update available</Badge> : r.latest ? <Badge variant="secondary">Up to date</Badge> : null}
                  </TableCell>
                  <TableCell className="text-right">
                    {r.latest && (
                      <Button size="sm" variant={r.updateAvailable ? "default" : "outline"} disabled={!canManage || !enabled || !!r.latestWithdrawn}
                        title={r.latestWithdrawn ? "This digest was withdrawn and can never be imported again" : !enabled ? "Not enabled on this site" : !canManage ? "Needs admin:service:manage" : undefined}
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
          An import always creates a <b>new</b>, staged template VM in pool pc-templates; existing templates are never changed or deleted.
          Smoke-test, publish and withdraw it under <i>Imported templates</i>.
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
  const [confirm, setConfirm] = useState("")
  const nodeOpt = placement.data?.nodes.find((n) => n.node === node)

  const submit = useMutation({
    mutationFn: () =>
      apiFetch(`${BASE}/vm`, {
        method: "POST",
        body: { package: latest.package, digest: latest.digest, confirmDigest: confirm.trim(), serviceId, node, importStorage, diskStorage },
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
          <Alert>
            <AlertTitle>The import lands staged</AlertTitle>
            <AlertDescription>
              Tenants do not see it. A smoke test (boot a throwaway clone, guest agent, cloud-init, root disk growth
              {line.variant === "kubernetes" ? ", kubelet + containerd" : ""}) starts automatically when the import finishes;
              after it passes you can <b>Publish</b> it under <i>Imported templates</i>.
              {line.variant === "kubernetes"
                ? " A Kubernetes node image never joins the OS catalog: publishing marks it ready and shows the version-map line for the Kubernetes provider."
                : " Once published, the newest version is the default for new servers; older versions stay."}
            </AlertDescription>
          </Alert>
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

function SandboxTab({ rows, loading }: { rows?: SandboxRow[]; loading: boolean }) {
  return (
    <Card>
      <CardContent className="space-y-4 pt-6">
        <Alert>
          <AlertTitle>Sandbox image updates are not enabled from the portal yet</AlertTitle>
          <AlertDescription>
            The portal has no account that may write the sandbox registry's <code>library/</code> images or change the
            prepull DaemonSet. To roll out a newer image: put its pin line into <code>deploy/templates/images.yaml</code>{" "}
            and run <code>make upgrade</code> — stage 55 copies it into the sandbox registry and pre-pulls it on every node,
            then the adoption job switches templates once every node has it (the previous pin stays until then).
          </AlertDescription>
        </Alert>
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
              rows.map((r) => (
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
                  <TableCell>{r.updateAvailable ? <Badge>Update available</Badge> : r.latest && r.pinned.length ? <Badge variant="secondary">Up to date</Badge> : null}</TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
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

// ── imported templates: lifecycle ─────────────────────────────────────────────────────────────

const STATE: Record<string, { label: string; variant: "default" | "secondary" | "outline" | "destructive" }> = {
  importing: { label: "Importing", variant: "outline" },
  import_failed: { label: "Import failed", variant: "destructive" },
  staged: { label: "Staged", variant: "outline" },
  smoke_running: { label: "Smoke running", variant: "outline" },
  smoke_failed: { label: "Smoke failed", variant: "destructive" },
  ready_to_publish: { label: "Ready to publish", variant: "secondary" },
  published_default: { label: "Published (default)", variant: "default" },
  published_older: { label: "Published (older)", variant: "secondary" },
  withdrawn: { label: "Withdrawn", variant: "destructive" },
  withdraw_incomplete: { label: "Withdraw incomplete", variant: "destructive" },
  template_missing: { label: "Template missing", variant: "destructive" },
  unknown: { label: "Unknown (cluster unreadable)", variant: "outline" },
}

function ImportedTab({ data, loading, canManage, onAction }: {
  data?: Templates; loading: boolean; canManage: boolean; onAction: (kind: "smoke" | "publish" | "withdraw", t: TemplateState) => void
}) {
  const enabled = !!data?.vmUpdatesEnabled
  const why = !enabled ? "Not enabled on this site (PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED)" : !canManage ? "Needs admin:service:manage" : undefined
  return (
    <Card>
      <CardContent className="space-y-3 pt-6">
        <p className="text-xs text-muted-foreground">
          Lifecycle: <b>Staged</b> (hidden) → automatic <b>smoke test</b> → <b>Publish</b> (only after the smoke test passed) → the newest
          published version is the default for new servers; older versions stay. <b>Withdraw</b> takes a bad version out of the catalog (the
          previous published one takes over) on every provider that has a copy, keeps the templates for inspection and blocks its digest for good.
          <b>Withdraw incomplete</b> means a copy could not be changed yet (provider unreachable): withdraw it again to finish. Running servers are never affected.
        </p>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Template</TableHead>
              <TableHead>Where</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Smoke test</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">Loading…</TableCell></TableRow>
            ) : !data?.items.length ? (
              <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">No imported templates yet.</TableCell></TableRow>
            ) : (
              data.items.map((t) => {
                const st = STATE[t.state] ?? { label: t.state, variant: "outline" as const }
                return (
                  <TableRow key={t.id}>
                    <TableCell>
                      <div className="font-medium">{t.slug}</div>
                      <div className="font-mono text-xs">{t.tag}</div>
                      <div className="font-mono text-xs text-muted-foreground" title={t.digest}>@{short(t.digest)}</div>
                    </TableCell>
                    <TableCell className="text-xs">
                      <span className="font-mono">{t.serviceId}</span> · {t.node}{t.vmid ? ` · VM ${t.vmid}` : ""}
                      <div className="text-muted-foreground">imported {timeAgo(t.importedAt)} by {t.requestedBy}</div>
                    </TableCell>
                    <TableCell>
                      <Badge variant={st.variant}>{st.label}</Badge>
                      {t.withdrawal && (
                        <div className="mt-1 max-w-xs text-xs text-muted-foreground" title={t.withdrawal.reason}>
                          {fmtDateTime(t.withdrawal.at)} by {t.withdrawal.actor}: {t.withdrawal.reason}
                        </div>
                      )}
                      {t.versionsMapLine && (
                        <div className="mt-1 text-xs">
                          Kubernetes version map:{" "}
                          <Button size="sm" variant="link" className="h-auto p-0 font-mono text-xs" onClick={() => {
                            void navigator.clipboard?.writeText(t.versionsMapLine!)
                            toast.success("Version-map line copied")
                          }}>{t.versionsMapLine}</Button>
                        </div>
                      )}
                      {t.warnings?.map((w) => <div key={w} className="text-xs text-amber-600">{w}</div>)}
                    </TableCell>
                    <TableCell className="text-xs">
                      {!t.smoke ? (
                        <span className="text-muted-foreground">not run</span>
                      ) : !t.smoke.jobId ? (
                        <span className="text-amber-600" title={t.smoke.message}>not started: {t.smoke.code}</span>
                      ) : (
                        <>
                          <div>
                            {t.smoke.result === "PASS" ? <Badge variant="secondary">PASS</Badge> : t.smoke.result === "FAIL" ? <Badge variant="destructive">FAIL</Badge> : <Badge variant="outline">{t.smoke.step ?? "queued"}</Badge>}
                            {t.smoke.code ? <span className="ml-1 font-mono">{t.smoke.code}</span> : null}
                          </div>
                          {t.smoke.message && t.smoke.result === "FAIL" && <div className="max-w-xs text-muted-foreground">{t.smoke.message}</div>}
                          {t.smoke.finishedAt && <div className="text-muted-foreground">{fmtDateTime(t.smoke.finishedAt)}</div>}
                        </>
                      )}
                    </TableCell>
                    <TableCell className="space-x-1 whitespace-nowrap text-right">
                      <Button size="sm" variant="outline" disabled={!canManage || !t.actions.smoke} title={why} onClick={() => onAction("smoke", t)}>Run smoke</Button>
                      <Button size="sm" disabled={!canManage || !t.actions.publish} title={why ?? (!t.actions.publish ? "Publish needs a passing smoke test of this template" : undefined)} onClick={() => onAction("publish", t)}>Publish</Button>
                      <Button size="sm" variant="destructive" disabled={!canManage || !t.actions.withdraw} title={why} onClick={() => onAction("withdraw", t)}>{t.state === "withdraw_incomplete" ? "Finish withdraw…" : "Withdraw…"}</Button>
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

type ProviderOutcome = { serviceId: string; status: string; changed: number[]; remaining: number[]; nodesLosingLast?: string[]; versionsMapLine?: string; warnings?: string[] }
type LifecycleResult = { complete?: boolean; warnings?: string[]; providers?: ProviderOutcome[]; unchanged?: boolean }

function LifecycleDialog({ kind, t, onClose, onDone }: { kind: "smoke" | "publish" | "withdraw"; t: TemplateState; onClose: () => void; onDone: () => void }) {
  const [confirm, setConfirm] = useState("")
  const [reason, setReason] = useState("")
  const [force, setForce] = useState(false)
  const [confirmLast, setConfirmLast] = useState("")
  const [confirmNodeLoss, setConfirmNodeLoss] = useState(false)
  const [result, setResult] = useState<LifecycleResult | null>(null)
  const last = kind === "withdraw" && t.actions.lastPublished
  const nodeLoss = kind === "withdraw" ? t.actions.nodesLosingLast ?? [] : []
  const run = useMutation({
    mutationFn: () => {
      const path = `${BASE}/templates/${encodeURIComponent(t.id)}/${kind}`
      if (kind === "smoke") return apiFetch<LifecycleResult>(path, { method: "POST" })
      if (kind === "publish") return apiFetch<LifecycleResult>(path, { method: "POST", body: { confirmDigest: confirm.trim() } })
      return apiFetch<LifecycleResult>(path, {
        method: "POST",
        body: { reason: reason.trim(), confirmDigest: confirm.trim(), force: last && force, confirmLast: last && force ? confirmLast.trim() : "", confirmNodeLoss: nodeLoss.length > 0 && confirmNodeLoss },
      })
    },
    onSuccess: (r) => {
      if (kind === "withdraw") {
        if (r && r.complete === false) toast.warning("Withdrawn where possible — some copies are not withdrawn yet; withdraw again to finish")
        else toast.success("Withdrawn")
        // Keep the dialog open to show the per-provider outcome (and the Kubernetes version-map line).
        setResult(r ?? {})
        return
      }
      toast.success(kind === "smoke" ? "Smoke test queued" : "Published")
      onDone()
    },
    onError: (e) => toast.error(e instanceof ApiError || e instanceof Error ? e.message : String(e)),
  })
  const digestOK = kind === "smoke" || confirm.trim() === t.digest
  const reasonOK = kind !== "withdraw" || (reason.trim().length >= 8 && reason.trim().length <= 500)
  const lastOK = !last || (force && confirmLast.trim() === t.slug)
  const nodeLossOK = nodeLoss.length === 0 || confirmNodeLoss
  const title = kind === "smoke" ? "Run the smoke test" : kind === "publish" ? "Publish" : t.state === "withdraw_incomplete" ? "Finish the withdraw" : "Withdraw"
  if (result) {
    return (
      <Dialog open onOpenChange={(o) => !o && onDone()}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>{result.complete === false ? "Withdraw incomplete" : "Withdrawn"}: {t.tag}</DialogTitle>
            <DialogDescription>
              {result.complete === false
                ? "The digest is blocked everywhere, but some copies could not be changed yet. Withdraw again (from the row marked Withdraw incomplete) once their provider is reachable."
                : "Every copy of this artifact left the catalog. The templates are kept for inspection."}
            </DialogDescription>
          </DialogHeader>
          <Table>
            <TableHeader>
              <TableRow><TableHead>Provider</TableHead><TableHead>Result</TableHead><TableHead>Details</TableHead></TableRow>
            </TableHeader>
            <TableBody>
              {(result.providers ?? []).map((p) => (
                <TableRow key={p.serviceId}>
                  <TableCell className="font-mono text-xs">{p.serviceId}</TableCell>
                  <TableCell><Badge variant={p.status === "withdrawn" ? "secondary" : "destructive"}>{p.status}</Badge></TableCell>
                  <TableCell className="text-xs">
                    {p.changed.length > 0 && <div>changed VM {p.changed.join(", ")}</div>}
                    {p.remaining.length > 0 ? <div>still published on the line: VM {p.remaining.join(", ")}</div> : <div className="text-amber-600">no published version of {t.slug} left</div>}
                    {p.versionsMapLine && (
                      <Button size="sm" variant="link" className="h-auto p-0 font-mono text-xs" onClick={() => {
                        void navigator.clipboard?.writeText(p.versionsMapLine!)
                        toast.success("Version-map line copied")
                      }}>{p.versionsMapLine}</Button>
                    )}
                    {p.warnings?.map((w) => <div key={w} className="text-amber-600">{w}</div>)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <DialogFooter><Button onClick={onDone}>Close</Button></DialogFooter>
        </DialogContent>
      </Dialog>
    )
  }
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>{title}: {t.tag}</DialogTitle>
          <DialogDescription>
            {kind === "smoke" && "Clones the template into a throwaway VM in pool pc-templates on the provider's smoke network, boots it with a fresh cloud-init (an ephemeral SSH key, no password), checks the guest agent, cloud-init and the root disk, then destroys the VM."}
            {kind === "publish" && (t.variant === "kubernetes"
              ? "Marks this Kubernetes node image as published (pc-tu-k8s-public). It never joins the OS catalog: use the version-map line on the provider."
              : `Adds pc-public: the newest published ${t.slug} version on ${t.node} becomes the default for new servers. Older versions stay. Running servers are not affected.`)}
            {kind === "withdraw" && `Removes this artifact from the catalog on every provider that has a copy (the previous published ${t.slug} version takes over), keeps the template VMs for inspection, and blocks its digest so it can never be imported or published again. Running servers are not affected.`}
            {kind === "withdraw" && t.variant === "kubernetes" && " The Kubernetes provider's version map names template VMIDs explicitly: the result gives the line to put there instead."}
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3 text-sm">
          <dl className="grid grid-cols-[8rem_1fr] gap-x-3 gap-y-1">
            <dt className="text-muted-foreground">Digest</dt><dd className="break-all font-mono text-xs">{t.digest}</dd>
            <dt className="text-muted-foreground">Template</dt><dd>{t.serviceId} · {t.node}{t.vmid ? ` · VM ${t.vmid}` : ""}</dd>
          </dl>
          {kind === "withdraw" && (
            <div className="space-y-1">
              <Label htmlFor="tu-reason">Reason (required, recorded with the blocklist entry and in the audit log)</Label>
              <Textarea id="tu-reason" value={reason} onChange={(e) => setReason(e.target.value)} placeholder="e.g. no network after first boot on our hardware" />
            </div>
          )}
          {last && (
            <Alert variant="destructive">
              <AlertTitle>This is the only published version of {t.slug} on {t.actions.lastOnProviders?.join(", ") || "this provider"}</AlertTitle>
              <AlertDescription className="space-y-2">
                <p>After withdrawing it, no new server{t.variant === "kubernetes" ? " or Kubernetes worker" : ""} of {t.slug} can be created there until another version is published.</p>
                <div className="flex items-center gap-2">
                  <Checkbox id="tu-force" checked={force} onCheckedChange={(v) => setForce(v === true)} />
                  <Label htmlFor="tu-force">Withdraw anyway</Label>
                </div>
                {force && (
                  <Input className="font-mono text-xs" placeholder={`type ${t.slug} to confirm`} value={confirmLast} onChange={(e) => setConfirmLast(e.target.value)} />
                )}
              </AlertDescription>
            </Alert>
          )}
          {nodeLoss.length > 0 && (
            <Alert variant="destructive">
              <AlertTitle>Nodes losing their last copy of {t.slug}</AlertTitle>
              <AlertDescription className="space-y-2">
                <p>{nodeLoss.join(", ")} keep no published {t.slug} version: a new server placed on {nodeLoss.length === 1 ? "that node" : "those nodes"} cannot be created from it until another version is published there.</p>
                <div className="flex items-center gap-2">
                  <Checkbox id="tu-nodeloss" checked={confirmNodeLoss} onCheckedChange={(v) => setConfirmNodeLoss(v === true)} />
                  <Label htmlFor="tu-nodeloss">I understand</Label>
                </div>
              </AlertDescription>
            </Alert>
          )}
          {kind !== "smoke" && (
            <div className="space-y-1">
              <Label htmlFor="tu-lc-confirm">Type or paste the exact digest to confirm</Label>
              <Input id="tu-lc-confirm" className="font-mono text-xs" placeholder={t.digest} value={confirm} onChange={(e) => setConfirm(e.target.value)} />
            </div>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button variant={kind === "withdraw" ? "destructive" : "default"} disabled={!digestOK || !reasonOK || !lastOK || !nodeLossOK || run.isPending} onClick={() => run.mutate()}>
            {run.isPending ? "Working…" : title}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function SmokeHistory({ smokes }: { smokes?: SmokeReceipt[] }) {
  if (!smokes?.length) return null
  return (
    <Card className="mt-4">
      <CardHeader><CardTitle className="text-base">Smoke tests</CardTitle></CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Started</TableHead>
              <TableHead>Template</TableHead>
              <TableHead>Result</TableHead>
              <TableHead>Checks</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {smokes.map((s) => (
              <TableRow key={s.id}>
                <TableCell className="text-xs">{fmtDateTime(s.createdAt)}<div className="text-muted-foreground">{s.auto ? "automatic" : s.requestedBy}</div></TableCell>
                <TableCell className="text-xs"><div className="font-mono">{s.payload.tag}</div>{s.payload.node} · VM {s.payload.templateVmid}{s.vmid ? ` → test VM ${s.vmid}` : ""}</TableCell>
                <TableCell className="text-xs">
                  {s.result ? <Badge variant={s.result === "PASS" ? "secondary" : "destructive"}>{s.result}</Badge> : <Badge variant="outline">{s.step}</Badge>}
                  {s.code && <span className="ml-1 font-mono">{s.code}</span>}
                  {s.result === "FAIL" && s.message && <div className="text-muted-foreground">{s.message}</div>}
                  {s.warnings?.map((w) => <div key={w} className="text-amber-600">{w}</div>)}
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">
                  {s.checks?.osPrettyName && <div>{s.checks.osPrettyName}</div>}
                  {s.checks?.cloudInitStatus && <div>cloud-init {s.checks.cloudInitStatus}</div>}
                  {!!s.checks?.rootFsBytes && <div>root fs {gib(s.checks.rootFsBytes)} (template disk {gib(s.checks.templateDiskBytes ?? 0)})</div>}
                  {s.checks?.kubelet && <div>{s.checks.kubelet}</div>}
                  {s.keyFingerprint && <div className="font-mono" title="ephemeral SSH key (never stored)">{s.keyFingerprint.slice(0, 20)}…</div>}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  )
}
