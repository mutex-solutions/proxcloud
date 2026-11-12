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
            <SandboxTab rows={disc.data?.sandbox} loading={disc.isLoading} />
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
