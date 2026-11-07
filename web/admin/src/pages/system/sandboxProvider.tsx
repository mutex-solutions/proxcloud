// sandboxProvider.tsx — the shape and the form of a `sandbox` provider, shared by the Add-provider dialog and the
// provider detail page. config.provider === "sandbox": customer sandboxes run as gVisor pods on the platform's sandbox
// cluster; the secret is a kubeconfig for the proxcloud service account there (deploy/sandbox-cluster/rbac.yaml).
// The workers belong to a Proxmox provider's sandbox pool (config.sandbox.proxmoxServiceId), whose sync fences them.
// The provider goes live (any status but DISABLED) only while its three release gates hold (docs/managed-sandbox.md §3).
import { useMutation, useQuery } from "@tanstack/react-query"
import { CheckCircle2, ShieldCheck, XCircle } from "lucide-react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch, apiFetchEnvelope } from "@/lib/api"

export type SandboxFormState = {
  name: string
  status: string
  region: string
  kubeconfig: string
  proxmoxServiceId: string
  // One template per line: `name=image cpu memoryGiB [description…]`
  templates: string
  // The sandbox domain sandbox-proxy serves (<port>-<id>.<domain>): published ports (SB4) and, with envd, the E2B API.
  domain: string
  // envd in every sandbox, which the E2B-compatible API at /e2b needs (with a domain).
  envd: boolean
  maxCpu: string
  maxMemoryGiB: string
  maxTimeoutSeconds: string
  defaultTimeoutSeconds: string
  maxConcurrent: string
  // The per-project default quota (D10); a project's own quota overrides it key by key.
  quotaSandboxes: string
  quotaVcpus: string
  quotaRamMb: string
  quotaGpus: string
  storedSandbox: Record<string, any>
}

export const emptySandboxForm: SandboxFormState = {
  name: "",
  status: "DISABLED",
  region: "",
  kubeconfig: "",
  proxmoxServiceId: "",
  templates: "",
  domain: "",
  envd: false,
  maxCpu: "",
  maxMemoryGiB: "",
  maxTimeoutSeconds: "",
  defaultTimeoutSeconds: "",
  maxConcurrent: "",
  quotaSandboxes: "",
  quotaVcpus: "",
  quotaRamMb: "",
  quotaGpus: "",
  storedSandbox: {},
}

type TemplateOffer = { image: string; cpu: number; memoryGiB: number; description?: string }

// parseTemplates reads `name=image cpu memoryGiB [description…]` lines; null when a line is malformed.
export function parseTemplates(raw: string): Record<string, TemplateOffer> | null {
  const out: Record<string, TemplateOffer> = {}
  for (const line of raw.split("\n").map((l) => l.trim()).filter(Boolean)) {
    const m = /^([a-z0-9][a-z0-9-]*)=(\S+)\s+(\d+)\s+(\d+)(?:\s+(.*))?$/.exec(line)
    if (!m) return null
    out[m[1]] = { image: m[2], cpu: Number(m[3]), memoryGiB: Number(m[4]), ...(m[5] ? { description: m[5] } : {}) }
  }
  return Object.keys(out).length ? out : null
}

const num = (s: string) => (s.trim() === "" ? undefined : Number(s))
const okNum = (s: string) => s.trim() === "" || (Number.isInteger(Number(s)) && Number(s) >= 0)

export const sandboxFormValid = (f: SandboxFormState, requireKubeconfig = true) =>
  !!f.name.trim() &&
  !!f.region.trim() &&
  !!f.proxmoxServiceId.trim() &&
  (!requireKubeconfig || !!f.kubeconfig.trim()) &&
  parseTemplates(f.templates) !== null &&
  [f.maxCpu, f.maxMemoryGiB, f.maxTimeoutSeconds, f.defaultTimeoutSeconds, f.maxConcurrent, f.quotaSandboxes, f.quotaVcpus, f.quotaRamMb, f.quotaGpus].every(okNum)

function compact(o: Record<string, number | undefined>) {
  return Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined))
}

// mergeTemplates keeps what the one-line form cannot show (startCmd, readyCmd, user, workdir, startAsRoot, gpu) on a
// template still listed under the same name, so editing the list never strips code-interpreter's start command.
function mergeTemplates(stored: Record<string, any> | undefined, parsed: Record<string, TemplateOffer>) {
  const out: Record<string, any> = {}
  for (const [name, t] of Object.entries(parsed)) {
    const { description: _drop, ...keep } = (stored?.[name] ?? {}) as Record<string, any>
    out[name] = { ...keep, ...t }
  }
  return out
}

export function sandboxConfigBlocks(f: SandboxFormState) {
  const region = f.region.trim()
  const domain = f.domain.trim()
  return {
    provider: "sandbox",
    region,
    regions: { [region]: {} },
    services: { sandbox: { [region]: true } },
    sandbox: {
      ...f.storedSandbox,
      proxmoxServiceId: f.proxmoxServiceId.trim(),
      templates: mergeTemplates(f.storedSandbox.templates, parseTemplates(f.templates) ?? {}),
      envd: f.envd,
      domain,
      limits: compact({
        maxCpu: num(f.maxCpu),
        maxMemoryGiB: num(f.maxMemoryGiB),
        maxTimeoutSeconds: num(f.maxTimeoutSeconds),
        defaultTimeoutSeconds: num(f.defaultTimeoutSeconds),
        maxConcurrent: num(f.maxConcurrent),
      }),
      projectQuota: compact({
        sandboxes: num(f.quotaSandboxes),
        sandboxVcpus: num(f.quotaVcpus),
        sandboxRamMb: num(f.quotaRamMb),
        sandboxGpus: num(f.quotaGpus),
      }),
    },
  }
}

export function sandboxFormToBody(f: SandboxFormState) {
  return {
    name: f.name.trim(),
    type: "CLOUD",
    status: f.status,
    config: sandboxConfigBlocks(f),
    secret: { kubeconfig: f.kubeconfig.trim() },
  }
}

export function sandboxFormFromService(svc: { name?: string; status?: string; config?: Record<string, any> }): SandboxFormState {
  const c = svc.config ?? {}
  const sb = (c.sandbox ?? {}) as Record<string, any>
  const l = (sb.limits ?? {}) as Record<string, any>
  const q = (sb.projectQuota ?? {}) as Record<string, any>
  const s = (v: unknown) => (v === undefined || v === null ? "" : String(v))
  const templates = Object.entries((sb.templates ?? {}) as Record<string, TemplateOffer>)
    .map(([name, t]) => `${name}=${t.image} ${t.cpu ?? 1} ${t.memoryGiB ?? 1}${t.description ? " " + t.description : ""}`)
    .join("\n")
  return {
    ...emptySandboxForm,
    name: svc.name ?? "",
    status: svc.status ?? "DISABLED",
    region: s(c.region),
    proxmoxServiceId: s(sb.proxmoxServiceId),
    templates,
    domain: s(sb.domain),
    envd: sb.envd === true,
    maxCpu: s(l.maxCpu),
    maxMemoryGiB: s(l.maxMemoryGiB),
    maxTimeoutSeconds: s(l.maxTimeoutSeconds),
    defaultTimeoutSeconds: s(l.defaultTimeoutSeconds),
    maxConcurrent: s(l.maxConcurrent),
    quotaSandboxes: s(q.sandboxes),
    quotaVcpus: s(q.sandboxVcpus),
    quotaRamMb: s(q.sandboxRamMb),
    quotaGpus: s(q.sandboxGpus),
    storedSandbox: sb,
  }
}

function Field({ id, label, value, onChange, placeholder, hint }: {
  id: string; label: string; value: string; onChange: (v: string) => void; placeholder?: string; hint?: string
}) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} className="font-mono" value={value} onChange={(e) => onChange(e.target.value)} placeholder={placeholder} autoComplete="off" />
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

export function SandboxProviderForm({ form, setForm, mode = "create" }: {
  form: SandboxFormState
  setForm: (f: SandboxFormState) => void
  mode?: "create" | "edit"
}) {
  const edit = mode === "edit"
  const providers = useQuery({
    queryKey: ["admin-list", "/admin/service"],
    queryFn: () => apiFetchEnvelope<Array<{ id: string; name?: string; config?: Record<string, any> }>>("/admin/service"),
  })
  const proxmox = (providers.data?.data ?? []).filter((p) => p.config?.provider === "proxmox")
  const set = (k: keyof SandboxFormState) => (v: string) => setForm({ ...form, [k]: v })
  return (
    <div className="grid gap-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="sb-name" label="Name" value={form.name} onChange={set("name")} placeholder="Sandboxes AZ1" />
        <Field id="sb-region" label="Region" value={form.region} onChange={set("region")} placeholder="az1" />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="sb-status">Status</Label>
        <Select value={form.status} onValueChange={set("status")}>
          <SelectTrigger id="sb-status"><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value="DISABLED">DISABLED (stored, not offered)</SelectItem>
            <SelectItem value="PUBLIC">PUBLIC (live)</SelectItem>
            <SelectItem value="PRIVATE">PRIVATE (live)</SelectItem>
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">
          A live status is refused unless the release gates hold: NodeRestriction on the sandbox cluster, the isolation
          rules for the sandbox network and the VM firewall on every worker.
        </p>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="sb-pve">Proxmox provider of the workers</Label>
        <Select value={form.proxmoxServiceId} onValueChange={set("proxmoxServiceId")}>
          <SelectTrigger id="sb-pve"><SelectValue placeholder="Choose the provider whose sandbox pool holds the workers" /></SelectTrigger>
          <SelectContent>
            {proxmox.map((p) => (
              <SelectItem key={p.id} value={p.id}>{p.name ?? p.id}{p.config?.sandbox?.pool ? ` (pool ${p.config.sandbox.pool})` : " (no sandbox pool)"}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">Its sync fences the workers; the release gates read them through it.</p>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="sb-kubeconfig">Kubeconfig</Label>
        <Textarea
          id="sb-kubeconfig"
          className="min-h-24 font-mono text-xs"
          value={form.kubeconfig}
          onChange={(e) => setForm({ ...form, kubeconfig: e.target.value })}
          placeholder={edit ? "unchanged (paste to replace)" : "kubeconfig of proxcloud-system/proxcloud on the sandbox cluster"}
          autoComplete="off"
        />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="sb-templates">Templates (one “name=image cpu memoryGiB description” line each)</Label>
        <Textarea
          id="sb-templates"
          className="min-h-20 font-mono text-xs"
          value={form.templates}
          onChange={(e) => setForm({ ...form, templates: e.target.value })}
          placeholder={"python=python:3.12-slim 1 2 Python 3.12\nnode=node:22-slim 1 2 Node.js 22"}
        />
        <p className="text-xs text-muted-foreground">Only these images are offered. Customers may raise CPU and memory up to the limits below.</p>
      </div>
      <Field
        id="sb-domain"
        label="Sandbox domain"
        value={form.domain}
        onChange={set("domain")}
        placeholder="example-sandbox.net"
        hint="The domain sandbox-proxy serves (installer sandbox.domain): published ports are reached at <port>-<id>.<domain>. Empty: no published ports."
      />
      <div className="grid gap-2">
        <div className="flex items-center gap-3">
          <Switch id="sb-envd" checked={form.envd} onCheckedChange={(on) => setForm({ ...form, envd: on })} />
          <Label htmlFor="sb-envd">Run envd (E2B-compatible API)</Label>
        </div>
        <p className="text-xs text-muted-foreground">
          Sandboxes run E2B's envd and the API at /e2b answers for this provider. Needs the sandbox domain and the node runtime installer stage 55 installs.
        </p>
      </div>
      <div className="grid gap-4 sm:grid-cols-3">
        <Field id="sb-maxcpu" label="Max vCPU per sandbox" value={form.maxCpu} onChange={set("maxCpu")} placeholder="8" />
        <Field id="sb-maxmem" label="Max GiB per sandbox" value={form.maxMemoryGiB} onChange={set("maxMemoryGiB")} placeholder="32" />
        <Field id="sb-maxconc" label="Max at once (cluster cap)" value={form.maxConcurrent} onChange={set("maxConcurrent")} placeholder="10" />
        <Field id="sb-maxto" label="Max time limit (s)" value={form.maxTimeoutSeconds} onChange={set("maxTimeoutSeconds")} placeholder="86400" />
        <Field id="sb-defto" label="Default time limit (s)" value={form.defaultTimeoutSeconds} onChange={set("defaultTimeoutSeconds")} placeholder="3600" />
      </div>
      <div className="grid gap-2">
        <div className="text-sm font-medium">Default project quota</div>
        <div className="grid gap-4 sm:grid-cols-4">
          <Field id="sb-qn" label="Sandboxes" value={form.quotaSandboxes} onChange={set("quotaSandboxes")} placeholder="5" />
          <Field id="sb-qc" label="vCPU" value={form.quotaVcpus} onChange={set("quotaVcpus")} placeholder="16" />
          <Field id="sb-qr" label="RAM (MB)" value={form.quotaRamMb} onChange={set("quotaRamMb")} placeholder="32768" />
          <Field id="sb-qg" label="GPUs" value={form.quotaGpus} onChange={set("quotaGpus")} placeholder="0" />
        </div>
        <p className="text-xs text-muted-foreground">
          What one project's sandboxes may hold together. Empty = the platform default (5 sandboxes, 16 vCPU, 32768 MB,
          0 GPU). A project's own quota (Projects → Quota) overrides it key by key.
        </p>
      </div>
    </div>
  )
}

type Gate = { name: string; ok: boolean; detail: string }

const GATE_LABEL: Record<string, string> = {
  noderestriction: "NodeRestriction on the sandbox cluster's API server",
  isolation: "Isolation rules for the sandbox network",
  fence: "VM firewall on every worker",
}

// SandboxReleaseGates runs the release check on the stored provider and shows each gate's answer.
export function SandboxReleaseGates({ id }: { id: string }) {
  const check = useMutation({
    mutationFn: () => apiFetch<Gate[]>(`/admin/service/${id}/sandbox/release-check`, { method: "POST" }),
    onError: (e: Error) => toast.error(e.message),
  })
  return (
    <div className="grid gap-3">
      <div className="flex items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          The provider goes live only while all three hold; each sandbox create checks them again (cached for a minute).
        </p>
        <Button variant="outline" onClick={() => check.mutate()} disabled={check.isPending}>
          <ShieldCheck className="size-4" /> {check.isPending ? "Checking…" : "Check release gates"}
        </Button>
      </div>
      {check.data && (
        <ul className="grid gap-2" aria-live="polite">
          {check.data.map((g) => (
            <li key={g.name} className="flex items-start gap-2 rounded-md border p-3 text-sm">
              {g.ok ? <CheckCircle2 className="mt-0.5 size-4 text-emerald-600" aria-label="holds" /> : <XCircle className="mt-0.5 size-4 text-destructive" aria-label="does not hold" />}
              <div className="grid gap-1">
                <span className="font-medium">{GATE_LABEL[g.name] ?? g.name}</span>
                <span className="break-all text-xs text-muted-foreground">{g.detail}</span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
