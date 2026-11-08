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
  // The sandbox registry (docs/managed-sandbox.md §6): where custom templates go, and whether built-in templates follow
  // the prepull DaemonSet to a new digest.
  templateRegistry: string
  imageAdoption: string
  // Custom template builds and the pruning of their images (docs/managed-sandbox.md §7). Keys are write-only.
  registryCa: string
  registryPrune: string
  registryGcUser: string
  registryGcPassword: string
  buildEndpoint: string
  buildPodEndpoint: string
  buildBucket: string
  buildRegion: string
  buildAccessKey: string
  buildSecretKey: string
  storedSandbox: Record<string, any>
  egressMbps: string
  abuseJSON: string
  rotationJSON: string
  rotationKubeconfig: string
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
  templateRegistry: "",
  imageAdoption: "off",
  registryCa: "",
  registryPrune: "off",
  registryGcUser: "",
  registryGcPassword: "",
  buildEndpoint: "",
  buildPodEndpoint: "",
  buildBucket: "",
  buildRegion: "",
  buildAccessKey: "",
  buildSecretKey: "",
  storedSandbox: {},
  egressMbps: "100",
  abuseJSON: '{"action":"alert","cpuPercent":90,"windowSeconds":900,"trafficBytes":65536,"notifyEmails":[]}',
  rotationJSON: '{"enabled":false,"intervalHours":168,"namespace":"pc-sandbox","cluster":"pc-sandbox","machineDeployments":[]}',
  rotationKubeconfig: "",
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

const jsonBlock = (s: string) => { try { const v = JSON.parse(s); return v && !Array.isArray(v) && typeof v === "object" ? v : null } catch { return null } }
const num = (s: string) => (s.trim() === "" ? undefined : Number(s))
const okNum = (s: string) => s.trim() === "" || (Number.isInteger(Number(s)) && Number(s) >= 0)

export const sandboxFormValid = (f: SandboxFormState, requireKubeconfig = true) =>
  !!f.name.trim() && jsonBlock(f.abuseJSON) !== null && jsonBlock(f.rotationJSON) !== null && okNum(f.egressMbps) &&
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
      egressMbps: num(f.egressMbps),
      abuse: jsonBlock(f.abuseJSON),
      rotation: jsonBlock(f.rotationJSON),
      proxmoxServiceId: f.proxmoxServiceId.trim(),
      templates: mergeTemplates(f.storedSandbox.templates, parseTemplates(f.templates) ?? {}),
      envd: f.envd,
      domain,
      templateRegistry: f.templateRegistry.trim(),
      imageAdoption: f.imageAdoption === "off" ? "" : f.imageAdoption,
      registryCa: f.registryCa.trim(),
      registryPrune: f.registryPrune === "off" ? "" : f.registryPrune,
      buildStorage: f.buildBucket.trim()
        ? { endpoint: f.buildEndpoint.trim(), podEndpoint: f.buildPodEndpoint.trim(), bucket: f.buildBucket.trim(), region: f.buildRegion.trim() }
        : {},
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

// sandboxSecret is what the form has for the secret: only filled fields, since the API merges non-blank values into the
// stored secret and a blank field means "unchanged".
export function sandboxSecret(f: SandboxFormState): Record<string, string> {
  const all: Record<string, string> = {
    kubeconfig: f.kubeconfig.trim(),
    rotationKubeconfig: f.rotationKubeconfig.trim(),
    registryGcUser: f.registryGcUser.trim(),
    registryGcPassword: f.registryGcPassword,
    buildStorageAccessKey: f.buildAccessKey.trim(),
    buildStorageSecretKey: f.buildSecretKey,
  }
  return Object.fromEntries(Object.entries(all).filter(([, v]) => v !== ""))
}

export function sandboxFormToBody(f: SandboxFormState) {
  return {
    name: f.name.trim(),
    type: "CLOUD",
    status: f.status,
    config: sandboxConfigBlocks(f),
    secret: sandboxSecret(f),
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
    templateRegistry: s(sb.templateRegistry),
    imageAdoption: s(sb.imageAdoption) || "off",
    registryCa: s(sb.registryCa),
    registryPrune: s(sb.registryPrune) || "off",
    buildEndpoint: s(sb.buildStorage?.endpoint),
    buildPodEndpoint: s(sb.buildStorage?.podEndpoint),
    buildBucket: s(sb.buildStorage?.bucket),
    buildRegion: s(sb.buildStorage?.region),
    storedSandbox: sb,
    egressMbps: s(sb.egressMbps) || "100",
    abuseJSON: JSON.stringify(sb.abuse ?? JSON.parse(emptySandboxForm.abuseJSON), null, 2),
    rotationJSON: JSON.stringify(sb.rotation ?? JSON.parse(emptySandboxForm.rotationJSON), null, 2),
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
      <Field id="sb-egress" label="Default egress cap (Mbps)" value={form.egressMbps} onChange={set("egressMbps")} hint="100 Mbps by default. Curated template egressMbps overrides inherit this unless set. Existing pods keep their cap." />
      <Label htmlFor="sb-abuse">Sustained CPU policy (JSON)</Label>
      <Textarea id="sb-abuse" value={form.abuseJSON} onChange={(e) => set("abuseJSON")(e.target.value)} className="font-mono text-xs" />
      <p className="text-xs text-muted-foreground">Heuristic, not proof of mining. Alert by default; pause/kill opt-in applies only to native-only providers. E2B/envd automatic actions are refused because silent SDK exec activity cannot be proved. Legitimate unattended jobs can match. Configure notifyEmails and the mail gateway.</p>
      <Label htmlFor="sb-rotation">Worker rotation (JSON, default off)</Label>
      <Textarea id="sb-rotation" value={form.rotationJSON} onChange={(e) => set("rotationJSON")(e.target.value)} className="font-mono text-xs" />
      <Label htmlFor="sb-rotation-key">Separate management rotation kubeconfig</Label>
      <Textarea id="sb-rotation-key" value={form.rotationKubeconfig} onChange={(e) => set("rotationKubeconfig")(e.target.value)} placeholder="unchanged; exact named MD patch/read + Machine read only" autoComplete="off" />
      <p className="text-xs text-muted-foreground">Waits for an empty protected worker group; never evicts live sandboxes, snapshots or builds. System nodes excluded. GET /admin/service/:id/sandbox/rotation-plan is read-only.</p>
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
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="sb-registry" label="Template registry" value={form.templateRegistry} onChange={set("templateRegistry")}
          placeholder="192.0.2.226/t" hint="The sandbox registry's address with /t. Sandboxes pull images there with their project's own account." />
        <div className="grid gap-2">
          <Label htmlFor="sb-adopt">Built-in image updates</Label>
          <Select value={form.imageAdoption} onValueChange={set("imageAdoption")}>
            <SelectTrigger id="sb-adopt"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="off">Off</SelectItem>
              <SelectItem value="dry-run">Dry run (log only)</SelectItem>
              <SelectItem value="on">On</SelectItem>
            </SelectContent>
          </Select>
          <p className="text-xs text-muted-foreground">
            Every 5 minutes, a template with a digest-pinned image moves to the digest the prepull DaemonSet runs, once every node
            has pulled it. To pin an image by hand, set this to Off first.
          </p>
        </div>
      </div>
      <div className="grid gap-2">
        <div className="text-sm font-medium">Custom template builds</div>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="sb-bep" label="Build bucket endpoint" value={form.buildEndpoint} onChange={set("buildEndpoint")} placeholder="http://192.0.2.41:9000" />
          <Field id="sb-bpep" label="Endpoint the nodes reach (if different)" value={form.buildPodEndpoint} onChange={set("buildPodEndpoint")} placeholder="empty: the same" />
          <Field id="sb-bbk" label="Bucket" value={form.buildBucket} onChange={set("buildBucket")} placeholder="pc-sandbox-builds" />
          <Field id="sb-brg" label="Region" value={form.buildRegion} onChange={set("buildRegion")} placeholder="us-east-1" />
          <Field id="sb-bak" label="Access key" value={form.buildAccessKey} onChange={set("buildAccessKey")} placeholder={edit ? "unchanged" : ""} />
          <div className="grid gap-2">
            <Label htmlFor="sb-bsk">Secret key</Label>
            <Input id="sb-bsk" type="password" value={form.buildSecretKey} onChange={(e) => setForm({ ...form, buildSecretKey: e.target.value })} placeholder={edit ? "unchanged" : ""} autoComplete="off" />
          </div>
        </div>
        <p className="text-xs text-muted-foreground">
          Where the E2B SDK's build contexts go; build pods fetch them with presigned URLs, so the nodes must reach the
          endpoint (the Proxmox provider's sandbox fence opens it). Empty bucket: template builds are off. Needs the
          template registry.
        </p>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="sb-prune">Prune unused template images</Label>
          <Select value={form.registryPrune} onValueChange={set("registryPrune")}>
            <SelectTrigger id="sb-prune"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="off">Off</SelectItem>
              <SelectItem value="dry-run">Dry run (log only)</SelectItem>
              <SelectItem value="on">On</SelectItem>
            </SelectContent>
          </Select>
          <p className="text-xs text-muted-foreground">Hourly: template images nothing references, older than a day, with the registry's gc account.</p>
        </div>
        <div className="grid gap-4">
          <Field id="sb-gcu" label="Registry gc user" value={form.registryGcUser} onChange={set("registryGcUser")} placeholder={edit ? "unchanged" : "gc"} />
          <div className="grid gap-2">
            <Label htmlFor="sb-gcp">Registry gc password</Label>
            <Input id="sb-gcp" type="password" value={form.registryGcPassword} onChange={(e) => setForm({ ...form, registryGcPassword: e.target.value })} placeholder={edit ? "unchanged" : ""} autoComplete="off" />
          </div>
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="sb-ca">Registry CA (PEM)</Label>
        <Textarea id="sb-ca" className="min-h-16 font-mono text-xs" value={form.registryCa} onChange={(e) => setForm({ ...form, registryCa: e.target.value })}
          placeholder="-----BEGIN CERTIFICATE----- (empty: the registry has a publicly trusted certificate)" />
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
