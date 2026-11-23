// PlatformPage — /platform, "Managed services": installs and removes the platform cluster that tenant Kubernetes
// clusters and databases run on, through /api/v1/admin/platform (docs/api.md "Managed-services platform"). The page
// shows one installation, the one of the first Proxmox provider: the API keeps one per provider.
// ponytail: one installation on the page; add a picker when an install has two Proxmox providers with managed services.
import { useEffect, useRef, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { useAdminMe } from "@/lib/auth"
import { fmtDateTime } from "@/lib/format"
import { useAdminList } from "@/lib/hooks"
import { hasPermission } from "@/lib/permissions"
import { cn } from "@/lib/utils"
import { CopyBlock, ErrorNote, LINK, errMsg, setupStatusQuery } from "@/pages/setup/common"
import { CheckStep, IdentitiesStep, ReviewStep, SettingsStep } from "./InstallSteps"
import { ATTACH_LABEL, GATE_LABEL, PLATFORM_KEY, STAGE_TITLE, codeAction, errCode, isConflict, rolling, runActive, stageNumbers, type Installation, type Run, type SandboxGate, type Stage } from "./model"
import { KubernetesVersion } from "./Tenants"

const POLL_MS = 3000
const INSTALL_SWITCH = "./proxcloud-up.sh install --managed-services"
const base = (id: string) => `/admin/platform/installations/${id}`

// A state as a dot and a word: the word carries the meaning, the dot repeats it in colour.
const STATE: Record<string, { label: string; dot: string }> = {
  off: { label: "Off", dot: "status-dot-muted" },
  draft: { label: "Draft", dot: "status-dot-muted" },
  applying: { label: "Installing", dot: "status-dot-warn" },
  ready: { label: "Ready", dot: "status-dot-ok" },
  degraded: { label: "Needs attention", dot: "status-dot-warn" },
  failed: { label: "Failed", dot: "status-dot-error" },
  removing: { label: "Removing", dot: "status-dot-warn" },
  removed: { label: "Removed", dot: "status-dot-muted" },
  queued: { label: "Queued", dot: "status-dot-muted" },
  pending: { label: "Not started", dot: "status-dot-muted" },
  running: { label: "Running", dot: "status-dot-warn" },
  waiting: { label: "Waiting", dot: "status-dot-warn" },
  cancelling: { label: "Cancelling", dot: "status-dot-warn" },
  cancelled: { label: "Cancelled", dot: "status-dot-muted" },
  succeeded: { label: "Succeeded", dot: "status-dot-ok" },
  done: { label: "Done", dot: "status-dot-ok" },
  skipped: { label: "Skipped", dot: "status-dot-muted" },
  ok: { label: "OK", dot: "status-dot-ok" },
  warn: { label: "Warning", dot: "status-dot-warn" },
  fail: { label: "Failed", dot: "status-dot-error" },
}
function State({ state, className }: { state: string; className?: string }) {
  const v = STATE[state] ?? { label: state, dot: "status-dot-muted" }
  return (
    <span className={cn("inline-flex shrink-0 items-center gap-1.5 text-sm", className)}>
      <span className={cn("status-dot", v.dot)} aria-hidden="true" />
      <span className="text-muted-foreground">{v.label}</span>
    </span>
  )
}

// titleRef makes the title a place the page can move keyboard focus to (it is not in the tab order).
function Section({ title, state, description, titleRef, children }: {
  title: string; state?: string; description?: React.ReactNode; titleRef?: React.Ref<HTMLDivElement>; children: React.ReactNode
}) {
  return (
    <Card>
      <CardHeader className="border-b">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div ref={titleRef} tabIndex={titleRef ? -1 : undefined} className="rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring">
            <CardTitle className="text-base">{title}</CardTitle>
          </div>
          {state ? <State state={state} /> : null}
        </div>
        {description ? <CardDescription>{description}</CardDescription> : null}
      </CardHeader>
      <CardContent className="grid grid-cols-1 gap-4">{children}</CardContent>
    </Card>
  )
}

// ── Off ───────────────────────────────────────────────────────────────────────

function Disabled() {
  return (
    <Section title="Managed services are off on this install" state="off">
      <p className="text-sm">
        Turn them on where ProxCloud is installed, then open this page again. On the one-host install, run this on the host. It
        also turns on template updates, which managed services need to import their two images:
      </p>
      <CopyBlock text={INSTALL_SWITCH} label="install command" />
      <p className="text-sm text-muted-foreground">
        On another kind of install, set <code>PROXCLOUD_PLATFORM_ENABLED=true</code> and{" "}
        <code>PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true</code> for the API and restart it.
      </p>
      <Link to="/docs/platform-admin/managed-services" className={LINK}>What managed services add and what they need</Link>
    </Section>
  )
}

// ── Prerequisites ─────────────────────────────────────────────────────────────

type Pre = { id: string; state: "ok" | "warn" | "fail"; message: React.ReactNode }

function Prerequisites({ serviceId, providerName }: { serviceId: string; providerName?: string }) {
  const setup = useQuery({ ...setupStatusQuery(serviceId, true), enabled: !!serviceId })
  const tu = useQuery({
    queryKey: ["admin-get", "/admin/template-updates/status"],
    queryFn: () => apiFetch<{ configured?: boolean; vmUpdatesEnabled?: boolean }>("/admin/template-updates/status"),
    retry: false,
  })
  const connect = setup.data?.steps.find((s) => s.id === "connect")
  const scheduler = setup.data?.steps.find((s) => s.id === "golive")?.items.find((i) => i.id === "scheduler")
  const rows: Pre[] = [
    { id: "switch", state: "ok", message: "Managed services are on." },
    !serviceId
      ? { id: "provider", state: "fail", message: <>No Proxmox provider is saved. <Link to="/setup?tab=connect" className="underline underline-offset-4">Connect one in Platform setup</Link>.</> }
      : setup.isLoading
        ? { id: "provider", state: "warn", message: `Provider ${providerName ?? serviceId}: asking the cluster…` }
        : !connect
          ? { id: "provider", state: "warn", message: `Provider ${providerName ?? serviceId} is saved. Its connection checks could not be read here; see Platform setup.` }
          : connect.state === "done" || connect.state === "attention"
            ? { id: "provider", state: connect.state === "done" ? "ok" : "warn", message: `Provider ${providerName ?? serviceId}: ${connect.summary}` }
            : { id: "provider", state: "fail", message: <>Provider {providerName ?? serviceId}: {connect.summary}. <Link to="/setup?tab=connect" className="underline underline-offset-4">Open Platform setup</Link>.</> },
  ]
  if (tu.data) {
    rows.push(tu.data.vmUpdatesEnabled
      ? { id: "template-updates", state: "ok", message: "Template updates are on: the two images can be imported." }
      : { id: "template-updates", state: "fail", message: <>Template updates are off, so no run can start. Run <code>{INSTALL_SWITCH}</code> on the host.</> })
    rows.push(tu.data.configured
      ? { id: "github", state: "ok", message: "A GitHub token is saved: the images can be downloaded." }
      : { id: "github", state: "fail", message: <>No GitHub token is saved. <Link to="/system/integrations" className="underline underline-offset-4">Add one under Integrations</Link>.</> })
  }
  if (scheduler && scheduler.state !== "done") {
    rows.push({ id: "scheduler", state: "warn", message: "The job scheduler is off. Tenant clusters and databases then update their status only when a provider sync runs." })
  }
  return (
    <Section title="Before you start" description="What managed services need from this install. The Check step looks closer, at Proxmox itself.">
      <ul aria-label="Prerequisites">
        {rows.map((r) => (
          <li key={r.id} className="flex flex-col gap-1 border-b py-2 text-sm last:border-b-0 sm:flex-row sm:gap-3">
            <State state={r.state} className="self-start pt-0.5 sm:w-36" />
            <span className="min-w-0 flex-1">{r.message}</span>
          </li>
        ))}
      </ul>
    </Section>
  )
}

// ── Progress ──────────────────────────────────────────────────────────────────

const clock = (v?: string) => (v ? new Date(v).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "")

// Why a stage or a run failed and what to do. retrying: the stage is still trying, so the same is shown without alarm,
// while there is time to act before the stage's time limit.
function Failure({ code, message, kind, retrying }: { code?: string; message?: string; kind: Run["kind"]; retrying?: boolean }) {
  if (!code && !message) return null
  return (
    <div className={cn("grid gap-1 rounded-lg border p-3 text-sm", !retrying && "border-destructive/40")} role="group" aria-label={retrying ? "Trying again" : "Failure"}>
      {retrying ? <p className="font-medium">Trying again. It did not work yet:</p> : null}
      <p>{code ? <span className="mr-2 font-mono text-xs">{code}</span> : null}{message}</p>
      {code ? <p><span className="font-medium">{retrying ? "What you can do meanwhile: " : "What to do: "}</span>{codeAction(code, kind)}</p> : null}
    </div>
  )
}

const RUN_TITLE = { apply: "Installation run", remove: "Removal" }

// One sentence for screen readers that changes as the run moves: the stage at work, then how the run ended.
function announcement(run?: Run): string {
  if (!run) return ""
  if (!runActive(run)) {
    return `${RUN_TITLE[run.kind]}: ${(STATE[run.state]?.label ?? run.state).toLowerCase()}.${run.code ? ` ${codeAction(run.code, run.kind)}` : ""}`
  }
  const stages = run.stages ?? []
  const i = stages.findIndex((s) => s.state === "running" || s.state === "waiting")
  if (i < 0) return `${RUN_TITLE[run.kind]}: ${(STATE[run.state]?.label ?? run.state).toLowerCase()}.`
  const s = stages[i]
  const numbers = stageNumbers(stages)
  return `Stage ${numbers[i]} of ${numbers.filter((n) => !n.endsWith("a")).length}: ${STAGE_TITLE[s.id] ?? s.id}, ${s.state === "waiting" ? "trying again" : "running"}.`
}

function StageRow({ stage, n, kind }: { stage: Stage; n: string; kind: Run["kind"] }) {
  const times = [
    stage.startedAt ? `started ${clock(stage.startedAt)}` : "",
    stage.finishedAt ? `finished ${clock(stage.finishedAt)}` : stage.deadline ? `must finish by ${clock(stage.deadline)}` : "",
    stage.retries ? `${stage.retries} ${stage.retries === 1 ? "retry" : "retries"}` : "",
  ].filter(Boolean).join(", ")
  return (
    <li className="grid gap-1.5 border-b py-2.5 text-sm last:border-b-0" data-stage={stage.id}>
      <div className="flex flex-col gap-1 sm:flex-row sm:items-baseline sm:gap-3">
        <State state={stage.state} className="sm:w-32" />
        <span className="min-w-0 flex-1">
          <span className="font-medium">{n}. {STAGE_TITLE[stage.id] ?? stage.id}</span>
          {times ? <span className="ml-2 text-xs text-muted-foreground">{times}</span> : null}
        </span>
      </div>
      {stage.progress ? <p className="text-muted-foreground sm:pl-35">{stage.progress}</p> : null}
      {stage.receipt?.vmid ? <p className="text-muted-foreground sm:pl-35">VM {stage.receipt.vmid}{stage.receipt.name ? ` (${stage.receipt.name})` : ""}</p> : null}
      {stage.state === "failed" || (stage.state === "waiting" && stage.code) ? (
        <div className="sm:pl-35"><Failure code={stage.code} message={stage.message} kind={kind} retrying={stage.state === "waiting"} /></div>
      ) : null}
      {stage.items?.length ? (
        <details className="sm:pl-35" open={stage.state === "failed"}>
          <summary className="cursor-pointer text-xs underline underline-offset-4">{stage.items.length} {stage.items.length === 1 ? "item" : "items"}</summary>
          <ul className="mt-1 grid gap-1">
            {stage.items.map((it) => (
              <li key={it.id} className="flex flex-col gap-0.5 sm:flex-row sm:gap-3">
                <State state={it.state} className="sm:w-28" />
                <span className="min-w-0 flex-1 break-words"><span className="mr-2 font-mono text-xs text-muted-foreground">{it.id}</span>{it.message}</span>
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </li>
  )
}

// focus: this session started the run, so keyboard focus (lost with the pressed button) moves to the card's title.
function RunCard({ run, installation, canManage, dirty, focus, onRetry, retrying, retryError, onRemove }: {
  run: Run; installation: Installation; canManage: boolean; dirty: boolean; focus: boolean
  onRetry: () => void; retrying: boolean; retryError: unknown; onRemove: () => void
}) {
  const qc = useQueryClient()
  const title = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (focus) title.current?.focus()
  }, [focus, run.id])
  const cancel = useMutation({
    mutationFn: () => apiFetch<Run>(`${base(installation.id)}/runs/${run.id}/cancel`, { method: "POST", body: {} }),
    onSuccess: () => qc.invalidateQueries({ queryKey: PLATFORM_KEY }),
    onError: (e) => {
      toast.error(errMsg(e))
      if (isConflict(e)) void qc.invalidateQueries({ queryKey: PLATFORM_KEY }) // the run ended meanwhile
    },
  })
  const active = runActive(run)
  const remove = run.kind === "remove"
  const ended = run.state === "failed" || run.state === "cancelled"
  const seedCD = run.code === "job_dead" && installation.status.seed?.cdVolume
  // The run's own failure repeats its failed stage's; show it only when no stage carries it (job_dead, a cancel).
  const stageFailed = run.stages?.some((s) => s.state === "failed" && s.code === run.code)
  // While the platform cluster's machines are replaced a cancel is a pause: the next apply goes on at the same unit.
  const roll = rolling(run)
  return (
    <Section
      title={RUN_TITLE[run.kind]}
      titleRef={title}
      state={run.state}
      description={`Started ${fmtDateTime(run.startedAt ?? run.createdAt)}${run.requestedBy ? ` by ${run.requestedBy}` : ""}${run.finishedAt ? `, ended ${fmtDateTime(run.finishedAt)}` : ""}.`}
    >
      {run.fromBom && run.bom ? (
        <p className="text-sm">
          This run upgrades the platform from release <span className="font-mono">{run.fromBom.release}</span> to release <span className="font-mono">{run.bom.release}</span>.
          {run.part === "seed"
            ? " It is the first of two actions: it imports the node image and moves the seed, and replaces no machine. The record names release " + run.fromBom.release + " until the second action, Platform cluster in step 4, has succeeded."
            : ` The record names release ${run.fromBom.release} until the last stage is done${ended ? `; ${roll ? "Resume" : "Retry"} continues to release ${run.bom.release}` : ""}.`}
        </p>
      ) : null}
      {active ? <p className="text-sm text-muted-foreground">This page reads the run every few seconds. You can leave it: the run goes on.</p> : null}
      <ol aria-label="Stages">
        {(run.stages ?? []).map((s, i, all) => <StageRow key={s.id} stage={s} n={stageNumbers(all)[i]} kind={run.kind} />)}
      </ol>
      {ended && !stageFailed ? (
        <Failure
          kind={run.kind}
          code={run.code}
          message={run.message ?? (run.state !== "cancelled" ? undefined
            : remove ? "The removal was cancelled. What it had already deleted stays deleted; the stages above show how far it came."
              : roll ? "The replacement of the machines is paused. A machine that was being replaced is finished by Cluster API on its own; the units not reached run the old Kubernetes version, which is a supported state. Resume goes on at the next unit."
                : "The run was cancelled. Nothing was deleted.")}
        />
      ) : null}
      {seedCD ? <p className="text-sm">The seed's start-up disc is still on <span className="font-mono">{seedCD}</span>. Retry or Remove deletes it.</p> : null}
      {ended ? (
        <p className="text-sm text-muted-foreground">
          {remove
            ? "A removal that stops keeps what it already deleted. Remove again goes on from there."
            : "Nothing is deleted when a run fails. Retry starts a new run that looks at what exists and goes on from there."}
        </p>
      ) : null}
      <ErrorNote title="The run did not start" error={retryError} />
      <div className="flex flex-wrap items-center gap-3">
        {active ? (
          <>
            <Button variant="outline" disabled={!canManage || cancel.isPending || run.state === "cancelling"} onClick={() => cancel.mutate()}>
              {run.state === "cancelling" ? (roll ? "Pausing…" : "Cancelling…") : roll ? "Pause" : "Cancel the run"}
            </Button>
            <span className="text-sm text-muted-foreground">{roll ? "Pause stops the run between two looks: the machine being replaced is finished by Cluster API, and no further unit starts until Resume. Nothing goes back to the old version. " : ""}A cancel usually takes effect within about 15 seconds, and up to 5 minutes while a stage pauses after repeated errors; a task Proxmox has started finishes first.</span>
          </>
        ) : null}
        {ended && !remove ? <Button disabled={!canManage || retrying || dirty} onClick={onRetry}>{retrying ? "Starting…" : roll ? "Resume" : "Retry"}</Button> : null}
        {ended && !remove && dirty ? <span className="text-sm text-muted-foreground">Save the settings first: Retry runs with what is saved.</span> : null}
        {ended && remove ? <Button variant="destructive" disabled={!canManage} onClick={onRemove}>Remove again</Button> : null}
      </div>
    </Section>
  )
}

// ── Ready ─────────────────────────────────────────────────────────────────────

// A break-glass kubeconfig goes from the answer straight into a file: it is not kept in page state and never rendered.
const kubeconfigFile = (installation: Installation, which: "seed" | "platform") => `${installation.name}-${which}.kubeconfig`
function useKubeconfig(installation: Installation, which: "seed" | "platform") {
  return useMutation({
    mutationFn: async () => {
      const { kubeconfig, expiresAt } = await apiFetch<{ kubeconfig: string; expiresAt: string }>(`${base(installation.id)}/${which}-kubeconfig`)
      const url = URL.createObjectURL(new Blob([kubeconfig], { type: "application/yaml" }))
      const a = document.createElement("a")
      a.href = url
      a.download = kubeconfigFile(installation, which)
      a.click()
      URL.revokeObjectURL(url)
      return expiresAt
    },
    onSuccess: (expiresAt) => toast.success(`Downloaded. It works until ${fmtDateTime(expiresAt)}.`),
  })
}

// The commands that open one ArgoCD through a port-forward: the password of admin, then the forward. tls: the ArgoCD
// has an address (spec.access.argocd, the platform cluster's only), so it serves HTTPS inside the cluster too.
const argoPassword = (file: string) =>
  `export KUBECONFIG=./${file}\nkubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo`
const argoCommands = (file: string, port: number, tls = false) =>
  `${argoPassword(file)}\nkubectl -n argocd port-forward svc/argocd-server ${port}:${tls ? 443 : 80}`

type ProviderRow = { id: string; name: string; status?: string; config?: Record<string, any> }

// The sandbox provider of the installation: whether it is live, each release gate's answer as the API checks it now
// (read-only, POST …/sandbox/release-check), the self-signed certificate for clients, and where it is set live. The
// page never sets it live: that is a save on the provider's own page, which runs the same gates.
function SandboxProviderItem({ id, label, row, installation, canManage }: { id: string; label: string; row?: ProviderRow; installation: Installation; canManage: boolean }) {
  const to = `/system/cloud-providers/${id}`
  const gates = useMutation({ mutationFn: () => apiFetch<SandboxGate[]>(`/admin/service/${id}/sandbox/release-check`, { method: "POST" }) })
  const sb = installation.status.sandbox
  const live = !!row?.status && row.status !== "DISABLED"
  const failed = (gates.data ?? []).filter((g) => !g.ok)
  const saveCert = () => {
    const url = URL.createObjectURL(new Blob([sb?.certificate ?? ""], { type: "application/x-pem-file" }))
    const a = document.createElement("a")
    a.href = url
    a.download = `${installation.name}-sandbox-ca.crt`
    a.click()
    URL.revokeObjectURL(url)
  }
  return (
    <li className="grid gap-1" aria-label="Sandbox provider">
      <Link to={to} className="underline underline-offset-4">{label}</Link>
      {row ? (
        <span>
          <State state={live ? "ok" : "warn"} className="mr-2" />
          {live ? `Live (${row.status}): tenants with this provider can start sandboxes.` : "Disabled: no tenant can start a sandbox yet. A run never sets it live."}
          {sb?.placement === "shared" ? " Reduced isolation: sandboxes run on a shared node group of the platform cluster." : ""}
        </span>
      ) : null}
      {installation.schedulerNote ? <span><State state="warn" className="mr-2" />The job scheduler is off, so this provider cannot be set live: no sync would fence a new sandbox machine or watch its firewall.</span> : null}
      <span className="flex flex-wrap items-center gap-2">
        <Button variant="outline" size="sm" disabled={!canManage || gates.isPending} onClick={() => gates.mutate()}>{gates.isPending ? "Checking the gates…" : "Check the release gates"}</Button>
        {sb?.certificate ? <Button variant="outline" size="sm" onClick={saveCert}>Download the certificate</Button> : null}
      </span>
      <ErrorNote title="The gates could not be checked" error={gates.error} />
      {gates.data ? (
        <ul aria-label="Release gates" className="grid gap-1">
          {gates.data.map((g) => (
            <li key={g.name} className="flex flex-col sm:flex-row sm:gap-3">
              <span className="sm:w-64"><State state={g.ok ? "ok" : "fail"} className="mr-2" />{GATE_LABEL[g.name] ?? g.name}</span>
              <span className="min-w-0 flex-1 text-muted-foreground">{g.detail}</span>
            </li>
          ))}
        </ul>
      ) : null}
      {gates.data ? (
        <span role="status">
          {failed.length
            ? `${failed.length} of ${gates.data.length} gates do not hold. A new sandbox machine passes the VM firewall gate about 20 minutes after it joined, once a provider sync has fenced it.`
            : <>Every gate holds. Set the provider live on <Link to={to} className="underline underline-offset-4">its page</Link>: choose a status other than Disabled and save.</>}
        </span>
      ) : null}
      {sb?.certificate ? <span className="text-muted-foreground">The certificate is self-signed for the sandbox domain: a client that calls a sandbox adds it to its trust store (SSL_CERT_FILE, or NODE_EXTRA_CA_CERTS for the JavaScript SDK).</span> : null}
    </li>
  )
}

// One registered provider with what a tenant still cannot use of it. row is the provider as /admin/service lists it;
// without one (the list is being read, or the row is gone) only the link is shown.
function ProviderItem({ id, label, kind, row, installation, canManage }: {
  id: string; label: string; kind: "kubernetes" | "database"; row?: ProviderRow; installation: Installation; canManage: boolean
}) {
  const to = `/system/cloud-providers/${id}`
  const versions = Object.keys(row?.config?.cluster?.versions ?? {})
  const applied = installation.status.tenantsAttach
  return (
    <li className="grid gap-1">
      <Link to={to} className="underline underline-offset-4">{label}</Link>
      {row && kind === "kubernetes" ? (
        versions.length ? <span><State state="ok" className="mr-2" />Offers Kubernetes {versions.join(", ")}.</span> : (
          <>
            <span><State state="warn" className="mr-2" />Offers no Kubernetes version yet, so no tenant can create a cluster.</span>
            <KubernetesVersion installation={installation} canManage={canManage} />
          </>
        )
      ) : null}
      {row ? (
        row.config?.attachToNewProjects === false ? (
          <span>
            <State state="warn" className="mr-2" />
            New projects do not get this provider{applied === "all" || applied === "new" ? "" : ", and the install attached it to no project"}.{" "}
            Choose who gets it in step 1 below and press “Apply this release”, or{" "}
            <Link to={to} className="underline underline-offset-4">attach it on the provider's Projects tab</Link>.
          </span>
        ) : (
          <span>
            <State state="ok" className="mr-2" />New projects get this provider.{" "}
            {applied === "all" ? "The last run that applied the settings also started attaching every project that existed; its progress is on the provider's Projects tab."
              : "Projects that existed before are attached on its Projects tab."}
          </span>
        )
      ) : null}
    </li>
  )
}

// live is the seed's answer of this page load; without one (it is being read, or the read failed) the card falls back
// to what the last run recorded.
function Ready({ installation, live, loading, error, canManage, providers }: {
  installation: Installation; live?: Installation["live"]; loading: boolean; error: unknown; canManage: boolean; providers: ProviderRow[]
}) {
  const recorded = installation.status.components ?? []
  const p = installation.status.providers
  const cluster = installation.spec.clusters[0]
  const endpoint = installation.status.clusters?.[0]?.endpoint ?? cluster?.endpoint
  const endpoints = [
    ["Seed API", installation.status.seed?.apiServer],
    ["Platform cluster API", endpoint ? `https://${endpoint}:6443` : undefined],
    ["Load balancer range (MetalLB)", cluster?.metallbRange],
    ["Node range", cluster?.nodeRange],
  ]
  const download = useKubeconfig(installation, "platform")
  const expires = installation.status.clusters?.[0]?.argocdTokenExpiresAt
  const argo = installation.spec.access?.argocd, argoUrl = installation.status.argocdUrl
  const days = expires ? Math.floor((new Date(expires).getTime() - Date.now()) / 86_400_000) : undefined
  const vmid = installation.status.seed?.vmid
  const choice = installation.spec.tenants?.attach ?? "none"
  return (
    <Section
      title={`Managed services “${installation.name}”`}
      state={live?.phase ?? installation.status.phase}
      description={`Release ${installation.status.bom?.release ?? "unknown"}. The components below are read from the seed each time this page loads.`}
    >
      {live?.error ? (
        <Alert variant="destructive">
          <AlertTitle>The seed could not be read</AlertTitle>
          <AlertDescription>
            <p>{live.error}</p>
            <Link to="/docs/platform-admin/managed-services" className={LINK}>When the seed does not answer: what to check and how to recover</Link>
          </AlertDescription>
        </Alert>
      ) : null}
      <ErrorNote title="The live state could not be read" error={error} />
      {loading ? <Skeleton className="h-24" /> : !live ? (
        <div className="grid gap-1 text-sm">
          <p className="text-muted-foreground">What the last run installed. Whether it is healthy now is not known.</p>
          <ul aria-label="Components the last run installed" className="list-disc pl-5">
            {recorded.map((c) => <li key={c.cluster + c.name}><span className="font-mono">{c.name}</span> {c.version}, on the {c.cluster === "seed" ? "seed" : "platform cluster"}</li>)}
          </ul>
        </div>
      ) : live.components.length ? (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <caption className="sr-only">Components and their live state</caption>
            <thead><tr className="border-b text-xs text-muted-foreground"><th scope="col" className="py-1.5 pr-3 font-medium">Component</th><th scope="col" className="pr-3 font-medium">Runs on</th><th scope="col" className="pr-3 font-medium">In sync</th><th scope="col" className="font-medium">Health</th></tr></thead>
            <tbody>
              {live.components.map((c) => (
                <tr key={c.cluster + c.name} className="border-b last:border-0">
                  <td className="py-1.5 pr-3 font-mono">{c.name}</td>
                  <td className="pr-3">{c.cluster === "seed" ? "seed" : "platform cluster"}</td>
                  <td className="pr-3"><State state={c.sync === "Synced" ? "ok" : "warn"} className="mr-1.5" />{c.sync || "unknown"}</td>
                  <td><State state={c.health === "Healthy" ? "ok" : "warn"} className="mr-1.5" />{c.health || "unknown"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : !live.error ? <p className="text-sm">The seed lists no component of this installation.</p> : null}

      <div className="grid gap-1 text-sm">
        <h3 className="font-semibold">Endpoints</h3>
        <p className="text-muted-foreground">
          Addresses of the platform network. They answer only from where that network is reachable, and the seed's API only for the seed API sources of the settings.
        </p>
        <ul aria-label="Endpoints">
          {endpoints.map(([label, value]) => (
            <li key={label} className="flex flex-col border-b py-1.5 last:border-b-0 sm:flex-row sm:gap-3">
              <span className="sm:w-64">{label}</span>
              <span className="min-w-0 flex-1 font-mono break-all">{value ?? "not recorded"}</span>
            </li>
          ))}
        </ul>
      </div>

      <div className="grid gap-1 text-sm">
        <h3 className="font-semibold">Providers</h3>
        <p className="text-muted-foreground">
          What the install registered for tenants, and what a tenant can use of it now. The settings say who gets them: {ATTACH_LABEL[choice].toLowerCase()}
          {choice !== (installation.status.tenantsAttach ?? "none") ? ". That is not applied yet: press “Apply this release” below." : "."}
        </p>
        <ul aria-label="Providers" className="grid list-disc gap-3 pl-5">
          {p?.kubernetes ? <ProviderItem id={p.kubernetes} label={`${installation.name} Kubernetes`} kind="kubernetes" row={providers.find((s) => s.id === p.kubernetes)} installation={installation} canManage={canManage} /> : null}
          {p?.database ? <ProviderItem id={p.database} label={`${installation.name} Databases`} kind="database" row={providers.find((s) => s.id === p.database)} installation={installation} canManage={canManage} /> : null}
          {p?.sandbox && installation.status.sandbox ? <SandboxProviderItem id={p.sandbox} label={`${installation.name} Sandboxes`} row={providers.find((s) => s.id === p.sandbox)} installation={installation} canManage={canManage} /> : null}
          {!p?.kubernetes && !p?.database ? <li>No provider is recorded for this installation.</li> : null}
        </ul>
        {installation.schedulerNote ? <p aria-label="Job scheduler"><State state="warn" className="mr-2" />{installation.schedulerNote}</p> : null}
      </div>

      <div className="grid gap-1.5 text-sm">
        <h3 className="font-semibold">Platform cluster access</h3>
        <div><Button variant="outline" disabled={!canManage || download.isPending} onClick={() => download.mutate()}>{download.isPending ? "Asking the platform cluster…" : "Download platform kubeconfig"}</Button></div>
        <p className="text-muted-foreground">
          Works for one hour. With kubectl it reads the cluster, without its Secrets, and changes nothing; it also opens the cluster's
          ArgoCD below, whose admin can change everything on the cluster. So keep the file like an admin credential. The download is
          written to the audit log; the file is shown nowhere.
        </p>
        <ErrorNote title="No kubeconfig was issued" error={download.error} />
      </div>

      <div className="grid min-w-0 gap-2 text-sm">
        <h3 className="font-semibold">Open ArgoCD</h3>
        <p>
          There are two. The one on the platform cluster holds every tenant cluster and database; it gets an address when you set one under Access
          in step 1 and apply. The one on the seed holds the platform's own components and never gets an address: its admin controls Cluster API and
          every machine. Without an address an ArgoCD opens through a port-forward, from a machine with kubectl that reaches the API address above.
        </p>
        {argoUrl ? (
          <>
            <p>
              On the platform cluster: <a className="font-mono underline" href={argoUrl} target="_blank" rel="noreferrer" aria-label="Open the platform ArgoCD">{argoUrl}</a>,
              from {(installation.status.argocdSources ?? []).join(", ") || "the operator networks"} only. The certificate is ArgoCD's own and self-signed, so the browser warns once:
              an IP address gets no publicly trusted certificate.
            </p>
            <p>
              Sign in as <span className="font-mono">admin</span>. ProxCloud creates no ArgoCD account and shows no password: read it with the platform kubeconfig
              (one hour, written to the audit log):
            </p>
            <CopyBlock text={argoPassword(kubeconfigFile(installation, "platform"))} label="platform ArgoCD password command" />
            <p className="text-muted-foreground">
              What you change in ArgoCD by hand is undone by its next sync: the platform's Applications heal themselves. For an everyday look that changes
              nothing, use the component list above.
            </p>
          </>
        ) : (
          <>
            <p>
              On the platform cluster{argo ? `, until an apply has given it ${argo.address} (after that apply it serves HTTPS, so the forward goes to port 443)` : ""}. Use the platform kubeconfig; the first
              command prints the password of user <span className="font-mono">admin</span>, the second runs until you stop it. Then open <span className="font-mono">{argo ? "https" : "http"}://localhost:8080</span>:
            </p>
            <CopyBlock text={argoCommands(kubeconfigFile(installation, "platform"), 8080, !!argo)} label="platform ArgoCD commands" />
          </>
        )}
        <p>On the seed, the ArgoCD that holds the platform's own components. Use the seed kubeconfig from “Seed access and removal” below, then open <span className="font-mono">http://localhost:8081</span>:</p>
        <CopyBlock text={argoCommands(kubeconfigFile(installation, "seed"), 8081)} label="seed ArgoCD commands" />
        <p className="text-muted-foreground">
          Look, and sync again what is out of sync. What you change by hand on a component, the next “Apply this release” writes back.
        </p>
      </div>

      {expires ? (
        <p className="text-sm">
          <State state={days !== undefined && days < 30 ? "warn" : "ok"} className="mr-2" />
          The seed's access to the platform cluster is valid until {fmtDateTime(expires)}.
          {days !== undefined && days < 30 ? " Press “Apply this release” below to renew it." : " Every apply renews it when under 90 days are left."}
        </p>
      ) : null}

      <div className="grid gap-2 text-sm">
        <h3 className="font-semibold">Next steps</h3>
        <ol className="grid list-decimal grid-cols-1 gap-2 pl-5">
          <li className="min-w-0">
            Back up the seed VM. Run this as root on one node, with your backup store:
            <CopyBlock text={`pvesh create /cluster/backup --vmid ${vmid ?? "<seed VMID>"} --storage <backup store> --schedule daily --mode snapshot --compress zstd --prune-backups keep-daily=7`} label="backup command" />
            Use a backup store that is not the boot, ISO or import store of the settings, and that no API token other than your own can read: the backup holds the seed's credentials.
            This page cannot see whether a backup job exists.
          </li>
          <li>Add price rules for clusters and databases: usage that matches no rule is billed as zero.</li>
        </ol>
      </div>
      <Alert>
        <AlertTitle>What this release does not do</AlertTitle>
        <AlertDescription>
          <ul className="list-disc pl-5">
            <li>Tenant Kubernetes clusters get no block volumes on a one-host install.</li>
            <li>Tenant databases are not backed up.</li>
            <li>One shared cluster only: separate clusters for Kubernetes and databases come later.</li>
          </ul>
        </AlertDescription>
      </Alert>
    </Section>
  )
}

// ── Seed access and removal ───────────────────────────────────────────────────

function RemoveDialog({ installation, open, onClose, onRemove, pending, error }: {
  installation: Installation; open: boolean; onClose: () => void; onRemove: (force: boolean) => void; pending: boolean; error: unknown
}) {
  const [typed, setTyped] = useState("")
  const [force, setForce] = useState(false)
  const want = `remove ${installation.name}`
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Remove managed services “{installation.name}”</DialogTitle>
          <DialogDescription>
            Deletes the platform cluster, its volumes and the seed VM, and disables the two providers. It is refused while tenant
            clusters or databases still run on the platform. This cannot be undone.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="remove-confirm">Type <span className="font-mono">{want}</span> to confirm</Label>
            <Input id="remove-confirm" className="font-mono" autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />
          </div>
          <div className="flex items-start gap-2">
            <Checkbox id="remove-force" checked={force} onCheckedChange={(v) => setForce(v === true)} className="mt-0.5" />
            <Label htmlFor="remove-force" className="leading-snug font-normal">
              Go on even when the seed VM is gone or the platform cluster does not answer. ProxCloud then cannot see whether tenant
              clusters or databases still run there: any that do are deleted with the cluster's machines and volumes.
            </Label>
          </div>
          <ErrorNote title="The removal did not start" error={error} />
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Keep it</Button>
          <Button variant="destructive" disabled={typed !== want || pending} onClick={() => onRemove(force)}>{pending ? "Starting…" : "Remove"}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// active: a run is going on. locked: the API still holds the installation for a run (going on, or ended unsettled).
function SeedAccess({ installation, canManage, active, locked, onRemove }: {
  installation: Installation; canManage: boolean; active: boolean; locked: boolean; onRemove: () => void
}) {
  const qc = useQueryClient()
  const download = useKubeconfig(installation, "seed")
  const rotate = useMutation({
    mutationFn: () => apiFetch(`${base(installation.id)}/seed-credential/rotate`, { method: "POST", body: {} }),
    onSuccess: () => {
      toast.success("ProxCloud now uses a new seed credential; the old one no longer works")
      return qc.invalidateQueries({ queryKey: PLATFORM_KEY })
    },
    onError: (e) => {
      if (isConflict(e)) void qc.invalidateQueries({ queryKey: PLATFORM_KEY }) // a run started meanwhile: show it
    },
  })
  const seedReady = !!installation.status.seed?.readyAt
  const phase = installation.status.phase
  return (
    <Section title="Seed access and removal" description="For emergencies and for taking managed services away again.">
      {seedReady ? (
        <>
          <div className="grid gap-1.5 text-sm">
            <div><Button variant="outline" disabled={!canManage || download.isPending} onClick={() => download.mutate()}>{download.isPending ? "Asking the seed…" : "Download seed kubeconfig"}</Button></div>
            <p className="text-muted-foreground">Full access to the seed for one hour, for looking closer when a component fails. The download is written to the audit log; the file is shown nowhere.</p>
            <ErrorNote title="No kubeconfig was issued" error={download.error} />
          </div>
          <div className="grid gap-1.5 text-sm">
            <div><Button variant="outline" disabled={!canManage || locked || rotate.isPending} onClick={() => rotate.mutate()}>{rotate.isPending ? "Replacing…" : "Replace the seed credential"}</Button></div>
            <p className="text-muted-foreground">Gives ProxCloud a new credential for the seed and ends the old one. Not possible during a run.</p>
            <ErrorNote title="The credential was not replaced" error={rotate.error} />
          </div>
        </>
      ) : <p className="text-sm text-muted-foreground">The seed is not ready yet, so there is nothing to download or replace.</p>}
      {phase !== "draft" && phase !== "removed" ? (
        <div className="grid gap-1.5 text-sm">
          <div><Button variant="destructive" disabled={!canManage || active} onClick={onRemove}>Remove managed services</Button></div>
          <p className="text-muted-foreground">Asks you to type the installation's name. Not possible during a run: cancel it first.</p>
        </div>
      ) : null}
    </Section>
  )
}

// ── Page ──────────────────────────────────────────────────────────────────────

export default function PlatformPage() {
  const qc = useQueryClient()
  const { data: me } = useAdminMe()
  const canManage = hasPermission(me?.permissions, "admin:service:manage")
  const [dirty, setDirty] = useState(false)
  const [removing, setRemoving] = useState(false)
  const [conflict, setConflict] = useState(false) // the last Save lost against someone else's
  const [started, setStarted] = useState("") // the run this session started last

  const list = useQuery({
    queryKey: [...PLATFORM_KEY, "installations"],
    queryFn: () => apiFetch<Installation[]>("/admin/platform/installations"),
    retry: false,
  })
  const stored = list.data?.[0]
  const services = useAdminList<{ id: string; name: string; config?: Record<string, any> }>("/admin/service")
  const proxmox = (services.data?.data ?? []).filter((s) => s.config?.provider === "proxmox").sort((a, b) => a.name.localeCompare(b.name))
  const provider = stored ? proxmox.find((s) => s.id === stored.serviceId) : proxmox[0]
  const serviceId = stored?.serviceId ?? provider?.id ?? ""

  const phase = stored?.status.phase ?? ""
  const run = useQuery({
    queryKey: [...PLATFORM_KEY, "run", stored?.id, stored?.lastRunId],
    queryFn: () => apiFetch<Run>(`${base(stored!.id)}/runs/${stored!.lastRunId}`),
    enabled: !!stored?.lastRunId,
    retry: false,
    refetchInterval: (q) => (runActive(q.state.data) ? POLL_MS : false),
  })
  // The run says whether it is going on; the stored phase stands in until the run is read. A run whose job died is
  // answered as failed while the stored phase stays applying or removing until Retry or Remove settles it: the page
  // then offers both, and keeps locked what the API refuses until then (a settings save, a new seed credential).
  const held = phase === "applying" || phase === "removing"
  const active = run.data ? runActive(run.data) : held
  const locked = active || held
  // The installation and the providers are read again once when its run ends, for the phase and the status the run left.
  const runEnded = !!run.data && !runActive(run.data)
  const runId = run.data?.id
  useEffect(() => {
    if (!runEnded) return
    void qc.invalidateQueries({ queryKey: [...PLATFORM_KEY, "installations"] })
    void qc.invalidateQueries({ queryKey: ["admin-list", "/admin/service"] }) // the run registered or disabled the two providers
  }, [qc, runEnded, runId])

  // Keyed on the last run: the API changes `rev` only when the settings are saved, never with a run.
  const live = useQuery({
    queryKey: [...PLATFORM_KEY, "live", stored?.id, stored?.lastRunId],
    queryFn: () => apiFetch<Installation>(`${base(stored!.id)}?live=1`),
    enabled: phase === "ready",
    retry: false,
  })
  // A query that is off still holds its last answer: the live state counts only while the installation is ready.
  const liveState = phase === "ready" ? live.data?.live : undefined

  const start = useMutation({
    mutationFn: (body: { kind: "apply" | "remove"; confirm?: string; targetBom?: string; seedBackupTaken?: boolean; part?: "seed" }) => apiFetch<{ runId: string }>(`${base(stored!.id)}/runs`, { method: "POST", body }),
    onSuccess: (r) => {
      setRemoving(false)
      setStarted(r.runId)
      return qc.invalidateQueries({ queryKey: PLATFORM_KEY })
    },
    onError: (e) => {
      if (isConflict(e)) void qc.invalidateQueries({ queryKey: PLATFORM_KEY }) // another session's run: show it
    },
  })
  const apply = () => start.mutate({ kind: "apply" })
  const startError = start.variables?.kind === "apply" ? start.error : null

  const header = (
    <PageHeader
      title="Managed services"
      eyebrow="System"
      description="Kubernetes clusters and databases for tenants. They run on a platform cluster that this page builds on Proxmox, starting from one small VM called the seed."
      actions={stored ? <span data-platform-state={liveState?.phase ?? phase}><State state={liveState?.phase ?? phase} /></span> : undefined}
    />
  )
  if (list.isLoading) return <>{header}<Skeleton className="h-64" /></>
  if (errCode(list.error) === "platform_disabled") return <>{header}<Disabled /></>
  if (list.error) return <>{header}<ErrorNote title="Managed services could not be read" error={list.error} /></>

  const ready = phase === "ready"
  // The last run is shown until it succeeded: after that the Ready or the Removed card says what there is to say.
  const showRun = !!stored && !!run.data && !((ready || phase === "removed") && run.data.state === "succeeded")
  return (
    <>
      {header}
      <div className="grid gap-4">
        {!canManage ? (
          <Alert><AlertTitle>You can read this page</AlertTitle><AlertDescription>Changing anything needs the permission admin:service:manage.</AlertDescription></Alert>
        ) : null}
        <p className="sr-only" role="status" aria-atomic="true">{announcement(run.data)}</p>
        {ready && stored ? <Ready installation={stored} live={liveState} loading={live.isLoading} error={live.error} canManage={canManage} providers={services.data?.data ?? []} /> : null}
        {phase === "removed" && stored ? (
          <Section title={`Managed services “${stored.name}” were removed`} state="removed">
            <p className="text-sm">The two tokens are yours to end. Run this as root on one Proxmox node:</p>
            <CopyBlock text={"pveum user token remove capmox@pve capi\npveum user token remove csi@pve csi"} label="token removal commands" />
            <p className="text-sm text-muted-foreground">To install again, go through the steps below. Both token secrets must be pasted again.</p>
          </Section>
        ) : null}
        {showRun ? (
          <RunCard run={run.data!} installation={stored!} canManage={canManage} dirty={dirty} focus={run.data!.id === started} onRetry={apply} retrying={start.isPending} retryError={startError} onRemove={() => setRemoving(true)} />
        ) : null}
        <ErrorNote title="The run could not be read" error={run.error} />
        {!active ? (
          <>
            {!ready ? <Prerequisites serviceId={serviceId} providerName={provider?.name} /> : null}
            <SettingsStep key={`${stored?.id}:${stored?.rev}`} installation={stored} serviceId={serviceId} canManage={canManage} locked={held}
              conflict={conflict} onConflict={setConflict} onDirty={setDirty} />
            {stored ? (
              <>
                <IdentitiesStep installation={stored} tokenId={provider?.config?.tokenId} canManage={canManage} />
                <CheckStep installation={stored} canManage={canManage} dirty={dirty} />
                <ReviewStep installation={stored} canManage={canManage} dirty={dirty} onInstall={apply} onUpgrade={(targetBom, confirm, seedBackupTaken, part) => start.mutate({ kind: "apply", ...(targetBom ? { targetBom } : {}), ...(confirm ? { confirm } : {}), ...(seedBackupTaken ? { seedBackupTaken } : {}), ...(part ? { part } : {}) })} installing={start.isPending} error={showRun ? null : startError} />
              </>
            ) : (
              <p className="text-sm text-muted-foreground">Steps 2 to 4 (Proxmox identities, Check, Review and install) open once the settings are saved.</p>
            )}
          </>
        ) : null}
        {stored ? <SeedAccess installation={stored} canManage={canManage} active={active} locked={locked} onRemove={() => setRemoving(true)} /> : null}
        {stored ? (
          <RemoveDialog
            key={String(removing)}
            installation={stored}
            open={removing}
            onClose={() => setRemoving(false)}
            pending={start.isPending}
            error={start.variables?.kind === "remove" ? start.error : null}
            onRemove={(force) => start.mutate({ kind: "remove", ...(force ? { confirm: `remove ${stored.name}` } : {}) })}
          />
        ) : null}
      </div>
    </>
  )
}
