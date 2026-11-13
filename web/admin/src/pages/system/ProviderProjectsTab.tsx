// ProviderProjectsTab — which projects a cloud provider is attached to, for every provider kind:
//   • the attach policy: whether a NEW project is attached to this provider automatically (config.attachToNewProjects,
//     PUT /admin/service/{id}/attach-policy — only that key changes);
//   • "Attach to existing projects": a durable bulk attach over all projects, picked organizations or picked projects
//     (/admin/service/{id}/bulk-attach), with live progress, cancel, the failed projects and "Retry failed projects".
// Backend: internal/providerattach (docs/cloud-integration.md "Attaching a provider to existing projects").

import { useEffect, useMemo, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { hasPermission, useAdminMe } from "@/lib/auth"
import { fmtDateTime, timeAgo } from "@/lib/format"
import type { CloudProvider } from "./CloudProvidersPage"

type ScopeMode = "all" | "organizations" | "projects" | "retry"
type Scope = { mode: ScopeMode; organizationIds?: string[]; projectIds?: string[]; sourceRunId?: string }
type Counts = {
  total: number; pending: number; running: number; attached: number; skipped: number; failed: number; cancelled: number
  byCode?: Record<string, number>
}
type BulkRun = {
  id: string
  serviceId: string
  scope: Scope
  concurrency: number
  requestedBy: string
  status: string
  planned: boolean
  cancelRequested?: boolean
  cancelledBy?: string
  code?: string
  counts: Counts
  createdAt: string
  startedAt?: string
  finishedAt?: string
  jobState?: string
}
type RunList = { runs: BulkRun[]; active?: BulkRun | null; attachToNewProjects: boolean }
type Item = { id: string; projectId: string; projectName?: string; organizationId?: string; state: string; code?: string; attempts: number; updatedAt: string }
type Candidate = { id: string; name: string; organizationId?: string; organizationName?: string; status?: string; attached?: boolean; projects?: number }
type Preview = { inScope: number; alreadyAttached: number; deleting: number; toAttach: number }

const errMsg = (e: unknown) => (e instanceof Error ? e.message : String(e))
const TERMINAL = new Set(["succeeded", "completed_with_failures", "cancelled", "failed"])

// Fixed result codes → what the operator reads.
const CODE_TEXT: Record<string, string> = {
  attached: "Attached",
  already_attached: "Already attached",
  deleting: "Scheduled for deletion or being deleted",
  not_found: "Project no longer exists",
  provider_error: "The provider refused or failed (see the API log for this project)",
  timeout: "The provider did not answer in time",
  provider_missing: "The provider was deleted",
  provider_disabled: "The provider was disabled",
  attempts_exhausted: "Interrupted too many times",
  cancelled: "Cancelled before it was attached",
  lease_expired: "The job stopped responding",
  invalid_payload: "Invalid job",
  retryable: "Interrupted; retrying",
}

const STATUS_TEXT: Record<string, string> = {
  queued: "Queued",
  running: "Running",
  cancelling: "Cancelling",
  succeeded: "Done",
  completed_with_failures: "Done with failures",
  cancelled: "Cancelled",
  failed: "Failed",
}

function statusVariant(s: string): "default" | "secondary" | "destructive" | "outline" {
  if (s === "succeeded") return "default"
  if (s === "failed" || s === "completed_with_failures") return "destructive"
  if (s === "cancelled") return "outline"
  return "secondary"
}

function scopeText(s: Scope): string {
  switch (s.mode) {
    case "all":
      return "All projects"
    case "organizations":
      return `${s.organizationIds?.length ?? 0} organization(s)`
    case "projects":
      return `${s.projectIds?.length ?? 0} picked project(s)`
    case "retry":
      return "Failed projects of an earlier run"
  }
  return s.mode
}

export function ProviderProjectsTab({ id, provider }: { id: string; provider: CloudProvider }) {
  const qc = useQueryClient()
  const { data: me } = useAdminMe()
  const canManage = hasPermission(me?.permissions, "admin:service:manage")
  const base = `/admin/service/${id}/bulk-attach`
  const runs = useQuery({
    queryKey: ["bulk-attach", id],
    queryFn: () => apiFetch<RunList>(base),
    refetchInterval: (q) => (q.state.data?.active ? 3000 : false),
  })
  const policyOn = (provider.config as Record<string, unknown> | undefined)?.attachToNewProjects !== false
  const [dialog, setDialog] = useState(false)
  const [shown, setShown] = useState<string | null>(null)
  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["bulk-attach", id] })
    qc.invalidateQueries({ queryKey: ["bulk-attach-items", id] })
  }

  const policy = useMutation({
    mutationFn: (on: boolean) => apiFetch(`/admin/service/${id}/attach-policy`, { method: "PUT", body: { attachToNewProjects: on } }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin-get", `/admin/service/${id}`] })
      toast.success("Saved")
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  const cancel = useMutation({
    mutationFn: (runId: string) => apiFetch(`${base}/${runId}/cancel`, { method: "POST" }),
    onSuccess: () => {
      invalidate()
      toast.success("Cancelling: the projects in flight finish, the rest are not attached")
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  const retry = useMutation({
    mutationFn: (runId: string) => apiFetch<BulkRun>(`${base}/${runId}/retry`, { method: "POST" }),
    onSuccess: (run) => {
      invalidate()
      setShown(run.id)
      toast.success("Retrying the failed projects")
    },
    onError: (e) => toast.error(errMsg(e)),
  })

  const list = runs.data?.runs ?? []
  const active = runs.data?.active ?? null
  const current = list.find((r) => r.id === shown) ?? active ?? list[0]
  const disabled = provider.status === "DISABLED"

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-eyebrow">New projects</CardTitle>
          <CardDescription>
            Whether a project created from now on is attached to this provider when it is first opened.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <div className="flex items-start gap-3">
            <Switch
              id="attach-policy"
              checked={policyOn}
              disabled={!canManage || policy.isPending}
              onCheckedChange={(on) => policy.mutate(on)}
              aria-describedby="attach-policy-help"
            />
            <div className="grid gap-1">
              <Label htmlFor="attach-policy">Attach new projects automatically</Label>
              <p id="attach-policy-help" className="text-sm text-muted-foreground">
                {policyOn
                  ? "New projects are attached to this provider automatically."
                  : "New projects are not attached to this provider. Attach them below, or from a project's page."}{" "}
                Changing this does not attach or detach any existing project, and it does not change which provider a
                request uses.
              </p>
            </div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex flex-row items-start justify-between gap-4">
          <div className="grid gap-1.5">
            <CardTitle className="text-eyebrow">Attach to existing projects</CardTitle>
            <CardDescription>
              Attaches this provider to projects that already exist, in the background. Projects already attached are
              skipped, and so are projects scheduled for deletion or being deleted. Suspended (disabled) projects are
              attached and stay suspended.
            </CardDescription>
          </div>
          {canManage ? (
            <Button onClick={() => setDialog(true)} disabled={!!active || disabled}>
              Attach to existing projects
            </Button>
          ) : null}
        </CardHeader>
        <CardContent className="space-y-4">
          {disabled ? (
            <Alert>
              <AlertTitle>This provider is disabled</AlertTitle>
              <AlertDescription>Enable it on the Configuration tab before attaching projects to it.</AlertDescription>
            </Alert>
          ) : null}
          {runs.error ? (
            <Alert variant="destructive">
              <AlertDescription>{errMsg(runs.error)}</AlertDescription>
            </Alert>
          ) : null}
          {current ? (
            <RunCard
              serviceId={id}
              run={current}
              canManage={canManage}
              onCancel={() => cancel.mutate(current.id)}
              cancelling={cancel.isPending}
              onRetry={() => retry.mutate(current.id)}
              retrying={retry.isPending || !!active}
            />
          ) : runs.isLoading ? null : (
            <p className="text-sm text-muted-foreground">No bulk attach has run for this provider yet.</p>
          )}
          {list.length > 1 ? (
            <div className="grid gap-2">
              <span className="text-sm font-medium">History</span>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Started</TableHead>
                    <TableHead>Scope</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead className="text-right">Attached</TableHead>
                    <TableHead className="text-right">Failed</TableHead>
                    <TableHead />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {list.map((r) => (
                    <TableRow key={r.id} data-state={r.id === current?.id ? "selected" : undefined}>
                      <TableCell title={fmtDateTime(r.createdAt)}>{timeAgo(r.createdAt)}</TableCell>
                      <TableCell>{scopeText(r.scope)}</TableCell>
                      <TableCell>
                        <Badge variant={statusVariant(r.status)}>{STATUS_TEXT[r.status] ?? r.status}</Badge>
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{r.counts?.attached ?? 0}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.counts?.failed ?? 0}</TableCell>
                      <TableCell className="text-right">
                        <Button variant="ghost" size="sm" onClick={() => setShown(r.id)}>
                          Show
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          ) : null}
        </CardContent>
      </Card>

      {dialog ? (
        <BulkAttachDialog
          serviceId={id}
          providerName={provider.name ?? id}
          onClose={() => setDialog(false)}
          onStarted={(run) => {
            setDialog(false)
            setShown(run.id)
            invalidate()
          }}
        />
      ) : null}
    </div>
  )
}

function RunCard({
  serviceId, run, canManage, onCancel, cancelling, onRetry, retrying,
}: {
  serviceId: string
  run: BulkRun
  canManage: boolean
  onCancel: () => void
  cancelling: boolean
  onRetry: () => void
  retrying: boolean
}) {
  const c = run.counts ?? { total: 0, pending: 0, running: 0, attached: 0, skipped: 0, failed: 0, cancelled: 0 }
  const done = c.attached + c.skipped + c.failed + c.cancelled
  const pct = c.total > 0 ? Math.round((done * 100) / c.total) : TERMINAL.has(run.status) ? 100 : 0
  const finished = TERMINAL.has(run.status)
  const failedProjects = c.failed > 0 || (run.status === "failed" && (c.pending > 0 || c.running > 0))
  return (
    <div className="space-y-3 rounded-lg border p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <Badge variant={statusVariant(run.status)}>{STATUS_TEXT[run.status] ?? run.status}</Badge>
          <span>{scopeText(run.scope)}</span>
          <span className="text-muted-foreground">
            · started {timeAgo(run.createdAt)} by <span className="font-mono text-xs">{run.requestedBy}</span>
          </span>
        </div>
        <div className="flex gap-2">
          {canManage && !finished && run.status !== "cancelling" ? (
            <Button variant="outline" size="sm" onClick={onCancel} disabled={cancelling}>
              Cancel
            </Button>
          ) : null}
          {canManage && finished && failedProjects ? (
            <Button size="sm" onClick={onRetry} disabled={retrying}>
              Retry failed projects
            </Button>
          ) : null}
        </div>
      </div>
      {run.code ? (
        <p className="text-sm text-destructive">{CODE_TEXT[run.code] ?? run.code}</p>
      ) : null}
      <div>
        <div className="h-2 w-full overflow-hidden rounded bg-muted" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
          <div className="h-full bg-primary transition-all" style={{ width: `${pct}%` }} />
        </div>
        <p className="mt-1 text-xs text-muted-foreground">
          {run.planned || finished ? `${done} of ${c.total} projects processed` : "Listing the projects in scope…"}
        </p>
      </div>
      <div className="grid grid-cols-2 gap-2 text-sm sm:grid-cols-5">
        <Stat label="Attached" value={c.attached} />
        <Stat label="Skipped" value={c.skipped} hint={skippedHint(c.byCode)} />
        <Stat label="Failed" value={c.failed} danger={c.failed > 0} />
        <Stat label="Left" value={c.pending + c.running} />
        <Stat label="Cancelled" value={c.cancelled} />
      </div>
      {c.failed > 0 ? <FailedList serviceId={serviceId} runId={run.id} live={!finished} /> : null}
    </div>
  )
}

function skippedHint(byCode?: Record<string, number>): string | undefined {
  if (!byCode) return undefined
  const parts = ["already_attached", "deleting", "not_found"]
    .filter((k) => byCode[k])
    .map((k) => `${byCode[k]} ${CODE_TEXT[k].toLowerCase()}`)
  return parts.length ? parts.join(", ") : undefined
}

function Stat({ label, value, hint, danger }: { label: string; value: number; hint?: string; danger?: boolean }) {
  return (
    <div className="rounded-md border p-2" title={hint}>
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={`text-lg font-semibold tabular-nums ${danger ? "text-destructive" : ""}`}>{value}</div>
      {hint ? <div className="truncate text-xs text-muted-foreground">{hint}</div> : null}
    </div>
  )
}

function FailedList({ serviceId, runId, live }: { serviceId: string; runId: string; live: boolean }) {
  const items = useQuery({
    queryKey: ["bulk-attach-items", serviceId, runId, "failed"],
    queryFn: () => apiFetch<{ items: Item[]; total: number }>(`/admin/service/${serviceId}/bulk-attach/${runId}/items?state=failed&limit=200`),
    refetchInterval: live ? 5000 : false,
  })
  const rows = items.data?.items ?? []
  return (
    <div className="grid gap-2">
      <span className="text-sm font-medium">
        Failed projects{items.data && items.data.total > rows.length ? ` (first ${rows.length} of ${items.data.total})` : ""}
      </span>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Project</TableHead>
            <TableHead>Reason</TableHead>
            <TableHead className="text-right">Tries</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((it) => (
            <TableRow key={it.id}>
              <TableCell>
                <Link className="underline-offset-2 hover:underline" to={`/clients/projects/${it.projectId}`}>
                  {it.projectName || it.projectId}
                </Link>
              </TableCell>
              <TableCell className="text-sm">{CODE_TEXT[it.code ?? ""] ?? it.code}</TableCell>
              <TableCell className="text-right tabular-nums">{it.attempts}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

// ── start dialog ────────────────────────────────────────────────────────────────────────────────

function useDebounced<T>(v: T, ms = 250): T {
  const [d, setD] = useState(v)
  useEffect(() => {
    const t = setTimeout(() => setD(v), ms)
    return () => clearTimeout(t)
  }, [v, ms])
  return d
}

function BulkAttachDialog({
  serviceId, providerName, onClose, onStarted,
}: {
  serviceId: string
  providerName: string
  onClose: () => void
  onStarted: (run: BulkRun) => void
}) {
  const base = `/admin/service/${serviceId}/bulk-attach`
  const [mode, setMode] = useState<"all" | "organizations" | "projects">("all")
  const [picked, setPicked] = useState<Record<string, Candidate>>({})
  const [search, setSearch] = useState("")
  const [concurrency, setConcurrency] = useState("2")
  const [confirm, setConfirm] = useState(false)
  const q = useDebounced(search)
  const kind = mode === "organizations" ? "organizations" : "projects"
  const candidates = useQuery({
    queryKey: ["bulk-attach-candidates", serviceId, kind, q],
    queryFn: () => apiFetch<Candidate[]>(`${base}/candidates?kind=${kind}&search=${encodeURIComponent(q)}`),
    enabled: mode !== "all",
  })
  const ids = useMemo(() => Object.keys(picked).sort(), [picked])
  const scope: Scope = mode === "all" ? { mode } : mode === "organizations" ? { mode, organizationIds: ids } : { mode, projectIds: ids }
  const ready = mode === "all" || ids.length > 0
  const preview = useQuery({
    queryKey: ["bulk-attach-preview", serviceId, JSON.stringify(scope)],
    queryFn: () => apiFetch<Preview>(`${base}/preview`, { method: "POST", body: { scope } }),
    enabled: ready,
  })
  const start = useMutation({
    mutationFn: () => apiFetch<BulkRun>(base, { method: "POST", body: { scope, concurrency: Number(concurrency) } }),
    onSuccess: (run) => {
      toast.success("Bulk attach started")
      onStarted(run)
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  const switchMode = (m: typeof mode) => {
    setMode(m)
    setPicked({})
    setSearch("")
    setConfirm(false)
  }
  const toggle = (c: Candidate, on: boolean) =>
    setPicked((prev) => {
      const next = { ...prev }
      if (on) next[c.id] = c
      else delete next[c.id]
      return next
    })
  const p = preview.data
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>Attach {providerName} to existing projects</DialogTitle>
          <DialogDescription>
            Runs in the background: you can leave this page. Each project is provisioned on the provider (the same as
            attaching it from the project&apos;s page) and gets the binding; nothing else about the project changes.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="grid gap-2">
            <Label>Projects</Label>
            <div className="flex flex-wrap gap-2" role="radiogroup">
              {(["all", "organizations", "projects"] as const).map((m) => (
                <Button key={m} type="button" size="sm" variant={mode === m ? "default" : "outline"} role="radio" aria-checked={mode === m} onClick={() => switchMode(m)}>
                  {m === "all" ? "All projects" : m === "organizations" ? "Selected organizations" : "Selected projects"}
                </Button>
              ))}
            </div>
          </div>
          {mode !== "all" ? (
            <div className="grid gap-2">
              <Input
                placeholder={mode === "organizations" ? "Search organizations by name or id" : "Search projects by name or id"}
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                aria-label="Search"
              />
              <div className="max-h-64 overflow-y-auto rounded-md border">
                {candidates.isLoading ? (
                  <p className="p-3 text-sm text-muted-foreground">Searching…</p>
                ) : (candidates.data ?? []).length === 0 ? (
                  <p className="p-3 text-sm text-muted-foreground">Nothing matches.</p>
                ) : (
                  (candidates.data ?? []).map((c) => (
                    <label key={c.id} className="flex cursor-pointer items-center gap-3 border-b px-3 py-2 text-sm last:border-b-0 hover:bg-muted/50">
                      <Checkbox checked={!!picked[c.id]} onCheckedChange={(on) => toggle(c, on === true)} aria-label={c.name || c.id} />
                      <span className="flex-1 truncate">
                        {c.name || c.id}
                        <span className="ml-2 font-mono text-xs text-muted-foreground">{c.id}</span>
                      </span>
                      {mode === "projects" ? (
                        <span className="flex items-center gap-2 text-xs text-muted-foreground">
                          {c.organizationName || c.organizationId}
                          {c.attached ? <Badge variant="outline">attached</Badge> : null}
                          {c.status && c.status !== "ENABLED" ? <Badge variant="secondary">{c.status.toLowerCase()}</Badge> : null}
                        </span>
                      ) : (
                        <span className="text-xs text-muted-foreground">{c.projects ?? 0} project(s)</span>
                      )}
                    </label>
                  ))
                )}
              </div>
              {ids.length > 0 ? (
                <div className="flex flex-wrap gap-1">
                  {ids.map((pid) => (
                    <Badge key={pid} variant="secondary" className="cursor-pointer" onClick={() => toggle(picked[pid], false)} title="Remove">
                      {picked[pid].name || pid} ✕
                    </Badge>
                  ))}
                </div>
              ) : null}
            </div>
          ) : null}
          <div className="grid max-w-xs gap-2">
            <Label htmlFor="bulk-concurrency">Projects at a time</Label>
            <Select value={concurrency} onValueChange={setConcurrency}>
              <SelectTrigger id="bulk-concurrency">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {["1", "2", "3", "4"].map((n) => (
                  <SelectItem key={n} value={n}>
                    {n}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {ready && p ? (
            <Alert>
              <AlertTitle>{p.toAttach} project(s) will be attached</AlertTitle>
              <AlertDescription>
                {p.inScope} in scope · {p.alreadyAttached} already attached · {p.deleting} being deleted (skipped). The count
                is taken now; the job checks every project again when it reaches it.
              </AlertDescription>
            </Alert>
          ) : null}
          {preview.error ? (
            <Alert variant="destructive">
              <AlertDescription>{errMsg(preview.error)}</AlertDescription>
            </Alert>
          ) : null}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Close
          </Button>
          {confirm ? (
            <Button onClick={() => start.mutate()} disabled={!ready || start.isPending}>
              {start.isPending ? "Starting…" : `Confirm: attach ${p?.toAttach ?? ""} project(s)`}
            </Button>
          ) : (
            <Button onClick={() => setConfirm(true)} disabled={!ready || !p || p.toAttach === 0}>
              Attach
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
