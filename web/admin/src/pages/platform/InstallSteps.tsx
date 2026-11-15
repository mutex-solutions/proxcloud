// InstallSteps.tsx — the four steps of the Managed services page before a run: the settings (a draft installation),
// the Proxmox identities and their two token secrets, the checks, and the review with Install. The settings come
// first because the script and the tokens belong to a saved installation (the script takes its two stores).
import { useState } from "react"
import { Link } from "react-router-dom"
import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { cn } from "@/lib/utils"
import { CheckRows, CopyBlock, ErrorNote, LINK, StepCard, errMsg, fetchText, useClusterInfo } from "@/pages/setup/common"
import {
  CAPI_TOKEN, CSI_TOKEN, GROUPS, PLATFORM_KEY, GROUP_USE, TOKEN_SECRET, checkAction, emptyForm, errProblems, formToSpec, labProfile, specToForm, validate,
  type BOM, type Check, type Form, type Installation, type MachineForm, type Plan, type Problem,
} from "./model"

const base = (id: string) => `/admin/platform/installations/${id}`

// ── Step 1: Settings ──────────────────────────────────────────────────────────

function Field({ id, label, hint, problems, className, ...props }: React.ComponentProps<typeof Input> & {
  id: string; label: string; hint?: string; problems: Problem[]
}) {
  const problem = problems.find((p) => p.field === id)
  const describedBy = [hint ? `${id}-hint` : "", problem ? `${id}-error` : ""].filter(Boolean).join(" ") || undefined
  return (
    <div className="grid content-start gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} className={cn("font-mono", className)} autoComplete="off" aria-invalid={problem ? true : undefined} aria-describedby={describedBy} {...props} />
      {hint ? <p id={`${id}-hint`} className="text-xs text-muted-foreground">{hint}</p> : null}
      {problem ? <p id={`${id}-error`} className="text-xs text-destructive">{problem.message}</p> : null}
    </div>
  )
}

function Sizes({ id, what, value, onChange, problems, disabled }: {
  id: string; what: string; value: MachineForm; onChange: (m: MachineForm) => void; problems: Problem[]; disabled?: boolean
}) {
  const set = (k: keyof MachineForm) => (e: React.ChangeEvent<HTMLInputElement>) => onChange({ ...value, [k]: e.target.value })
  const problem = problems.find((p) => p.field === `${id}-cores`)
  const input = (k: keyof MachineForm, suffix: string, label: string) => (
    <div className="grid gap-1.5">
      <Label htmlFor={`${id}-${suffix}`}>{label}</Label>
      <Input id={`${id}-${suffix}`} inputMode="numeric" value={value[k]} onChange={set(k)} disabled={disabled} aria-label={`${what}: ${label}`}
        aria-invalid={problem ? true : undefined} aria-describedby={problem ? `${id}-error` : undefined} />
    </div>
  )
  return (
    <div className="grid gap-1.5">
      <div className="grid grid-cols-3 gap-3">
        {input("cores", "cores", "Cores")}
        {input("memoryGiB", "memory", "Memory (GiB)")}
        {input("diskGiB", "disk", "Disk (GiB)")}
      </div>
      {problem ? <p id={`${id}-error`} className="text-xs text-destructive">{problem.message}</p> : null}
    </div>
  )
}

// The Proxmox nodes a group's machines go to: a checkbox per node when the cluster answered, else their names as text.
function NodePicker({ id, group, nodes, value, onChange, problems, disabled }: {
  id: string; group: string; nodes: string[]; value: string[]; onChange: (v: string[]) => void; problems: Problem[]; disabled?: boolean
}) {
  const problem = problems.find((p) => p.field === id)
  const error = problem ? <p id={`${id}-error`} className="text-xs text-destructive">{problem.message}</p> : null
  if (!nodes.length) {
    return (
      <div className="grid gap-1.5">
        <Label htmlFor={id}>Nodes</Label>
        <Input id={id} className="font-mono" value={value.join(", ")} disabled={disabled} placeholder="pve1, pve2" aria-label={`Group ${group}: nodes`}
          aria-invalid={problem ? true : undefined} aria-describedby={problem ? `${id}-error` : undefined}
          onChange={(e) => onChange(e.target.value.split(/[\s,]+/).filter(Boolean))} />
        {error}
      </div>
    )
  }
  return (
    <fieldset id={id} className="grid gap-1.5" aria-describedby={problem ? `${id}-error` : undefined}>
      <legend className="mb-1.5 text-sm font-medium">Nodes</legend>
      <div className="flex flex-wrap gap-x-4 gap-y-2">
        {nodes.map((n) => (
          <span key={n} className="flex items-center gap-2">
            <Checkbox id={`${id}-${n}`} checked={value.includes(n)} disabled={disabled} aria-label={`Group ${group}: node ${n}`}
              onCheckedChange={(on) => onChange(on ? [...value, n] : value.filter((v) => v !== n))} />
            <Label htmlFor={`${id}-${n}`} className="font-mono font-normal">{n}</Label>
          </span>
        ))}
      </div>
      {error}
    </fieldset>
  )
}

const H3 = ({ children }: { children: React.ReactNode }) => <h3 className="border-b pb-1 text-sm font-semibold">{children}</h3>

export function SettingsStep({ installation, serviceId, canManage, onDirty }: {
  installation?: Installation; serviceId: string; canManage: boolean; onDirty: (dirty: boolean) => void
}) {
  const qc = useQueryClient()
  const cluster = useClusterInfo(serviceId)
  const stored = installation ? specToForm(installation.name, installation.spec) : emptyForm
  const [form, setFormState] = useState<Form>(stored)
  const [shown, setShown] = useState(false) // problems are shown after the first Save
  const setForm = (f: Form) => {
    setFormState(f)
    onDirty(JSON.stringify(f) !== JSON.stringify(stored))
  }
  const set = (k: keyof Form) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => setForm({ ...form, [k]: e.target.value })
  const installed = !!installation?.status.bom
  const fixed = installed || !canManage // an input the API would refuse to change
  const all = validate(form)
  const problems = shown ? all : []
  const nodes = (cluster.data?.nodes ?? []).map((n) => n.node)

  const save = useMutation({
    mutationFn: () => installation
      ? apiFetch<Installation>(base(installation.id), { method: "PUT", body: { name: form.name, spec: formToSpec(form, installation.spec), rev: installation.rev } })
      : apiFetch<Installation>("/admin/platform/installations", { method: "POST", body: { name: form.name, serviceId, layout: "shared", spec: formToSpec(form) } }),
    onSuccess: () => {
      toast.success("Settings saved")
      onDirty(false)
      return qc.invalidateQueries({ queryKey: PLATFORM_KEY })
    },
  })
  const refused = errProblems(save.error)

  return (
    <StepCard n={1} title="Settings" description="What gets built and where. Saving keeps a draft: nothing is created on Proxmox until you press Install.">
      {installed ? (
        <p className="text-sm text-muted-foreground">
          The name, the network, the seed's node and address and the API address were fixed by the first successful install. To change
          them, remove the installation and install again.
        </p>
      ) : null}
      <datalist id="pf-nodes">{nodes.map((n) => <option key={n} value={n} />)}</datalist>
      <datalist id="pf-stores">{(cluster.data?.storages ?? []).map((s) => <option key={s.storage} value={s.storage} />)}</datalist>
      <datalist id="pf-bridges">{[...new Set((cluster.data?.bridges ?? []).filter((b) => !b.management).map((b) => b.iface))].map((b) => <option key={b} value={b} />)}</datalist>

      <H3>Layout</H3>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="name" label="Name" value={form.name} onChange={set("name")} disabled={fixed} problems={problems} placeholder="lab"
          hint="The two providers are named after it: “lab Kubernetes” and “lab Databases”." />
        <fieldset className="grid content-start gap-1.5">
          <legend className="mb-1.5 text-sm font-medium">Layout</legend>
          <label className="flex items-center gap-2 text-sm"><input type="radio" name="pf-layout" checked readOnly /> Shared: one cluster runs Kubernetes control planes and databases</label>
          <label className="flex items-center gap-2 text-sm text-muted-foreground"><input type="radio" name="pf-layout" disabled /> Separate clusters (in a later release)</label>
        </fieldset>
      </div>

      <H3>Node groups</H3>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">Three groups of worker machines. The machines of a group are spread over the nodes you choose.</p>
        <Button variant="outline" size="sm" disabled={!canManage} onClick={() => setForm(labProfile(form))}>Use the lab profile</Button>
      </div>
      {GROUPS.map((g) => {
        const gf = form.groups[g]
        const setGroup = (next: Partial<typeof gf>) => setForm({ ...form, groups: { ...form.groups, [g]: { ...gf, ...next } } })
        return (
          <div key={g} className="grid gap-3 rounded-lg border p-3" role="group" aria-label={`Group ${g}`}>
            <p className="text-sm"><span className="font-mono font-medium">{g}</span> <span className="text-muted-foreground">runs {GROUP_USE[g]}</span></p>
            <NodePicker id={`${g}-nodes`} group={g} nodes={nodes} value={gf.nodes} onChange={(v) => setGroup({ nodes: v })} problems={problems} disabled={!canManage} />
            <div className="grid gap-3 sm:grid-cols-[8rem_1fr]">
              <Field id={`${g}-count`} label="Machines" aria-label={`Group ${g}: machines`} inputMode="numeric" value={gf.count} onChange={(e) => setGroup({ count: e.target.value })} disabled={!canManage} problems={problems} />
              <Sizes id={g} what={`Group ${g}`} value={gf} onChange={(m) => setGroup(m)} problems={problems} disabled={!canManage} />
            </div>
          </div>
        )
      })}

      <H3>Platform network</H3>
      <p className="text-sm text-muted-foreground">
        A network of its own for the seed and the platform cluster, untagged on one bridge. Tenants must not reach the seed, the API
        address or the node range; they do reach the load balancer range.
      </p>
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <Field id="bridge" label="Bridge" list="pf-bridges" value={form.bridge} onChange={set("bridge")} disabled={fixed} problems={problems} placeholder="vmbr1" />
        <Field id="prefix" label="Network" value={form.prefix} onChange={set("prefix")} disabled={fixed} problems={problems} placeholder="10.90.0.0/24" />
        <Field id="gateway" label="Gateway" value={form.gateway} onChange={set("gateway")} disabled={fixed} problems={problems} placeholder="10.90.0.1" />
        <Field id="dns" label="DNS servers" value={form.dns} onChange={set("dns")} disabled={fixed} problems={problems} placeholder="10.90.0.1" hint="One to three, separated by commas." />
        <Field id="mtu" label="MTU (optional)" inputMode="numeric" value={form.mtu} onChange={set("mtu")} disabled={fixed} problems={problems} placeholder="1500" />
        <Field id="endpoint" label="API address" value={form.endpoint} onChange={set("endpoint")} disabled={fixed} problems={problems} placeholder="10.90.0.10"
          hint="A free address the platform cluster's API answers on." />
        <Field id="nodeRange" label="Node range" value={form.nodeRange} onChange={set("nodeRange")} disabled={!canManage} problems={problems} placeholder="10.90.0.20-10.90.0.60"
          hint="Addresses for the machines, with one spare per group and node for updates." />
        <Field id="metallbRange" label="Load balancer range" value={form.metallbRange} onChange={set("metallbRange")} disabled={!canManage} problems={problems} placeholder="10.90.0.100-10.90.0.150"
          hint="Addresses tenants reach their Kubernetes APIs and databases on." />
        <Field id="pveApiUrl" label="Proxmox API URL" value={form.pveApiUrl} onChange={set("pveApiUrl")} disabled={!canManage} problems={problems} placeholder="https://10.90.0.2:8006"
          hint="As machines on this network reach it." />
      </div>

      <H3>Seed</H3>
      <p className="text-sm text-muted-foreground">One small VM that builds the platform cluster and keeps it up to date.</p>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="seedNode" label="Seed node" list="pf-nodes" value={form.seedNode} onChange={set("seedNode")} disabled={fixed} problems={problems} placeholder="pve1" />
        <Field id="seedAddress" label="Seed address" value={form.seedAddress} onChange={set("seedAddress")} disabled={fixed} problems={problems} placeholder="10.90.0.5" />
        <Sizes id="seed" what="Seed" value={form.seed} onChange={(m) => setForm({ ...form, seed: m })} problems={problems} disabled={!canManage} />
        <Field id="apiSources" label="Seed API sources" value={form.apiSources} onChange={set("apiSources")} disabled={!canManage} problems={problems} placeholder="192.0.2.10/32"
          hint="Networks that may reach the seed's API: at least the address ProxCloud reaches it from." />
        <div className="grid content-start gap-1.5 sm:col-span-2">
          <Label htmlFor="sshKeys">SSH public keys (optional)</Label>
          <Textarea id="sshKeys" className="font-mono text-xs" rows={2} value={form.sshKeys} onChange={set("sshKeys")} disabled={!canManage}
            aria-invalid={problems.some((p) => p.field === "sshKeys") || undefined} aria-describedby="sshKeys-hint" />
          <p id="sshKeys-hint" className="text-xs text-muted-foreground">
            For emergencies only, one per line. With a key, the seed also accepts SSH from the API sources. ProxCloud never uses SSH.
          </p>
          {problems.filter((p) => p.field === "sshKeys").map((p) => <p key={p.message} className="text-xs text-destructive">{p.message}</p>)}
        </div>
      </div>

      <H3>Control plane</H3>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="cpNode" label="Control plane node" list="pf-nodes" value={form.cpNode} onChange={set("cpNode")} disabled={!canManage} problems={problems} placeholder="pve1"
          hint="One machine runs the platform cluster's control plane." />
        <Sizes id="cp" what="Control plane" value={form.cp} onChange={(m) => setForm({ ...form, cp: m })} problems={problems} disabled={!canManage} />
      </div>

      <H3>Storage</H3>
      <div className="grid gap-4 sm:grid-cols-3">
        <Field id="boot" label="Boot store" list="pf-stores" value={form.boot} onChange={set("boot")} disabled={!canManage} problems={problems} placeholder="local-zfs"
          hint="Disks of every machine, and the volumes of the platform's own components." />
        <Field id="iso" label="ISO store" list="pf-stores" value={form.iso} onChange={set("iso")} disabled={!canManage} problems={problems} placeholder="local"
          hint="Local to each node. Start-up data of the machines goes here." />
        <Field id="import" label="Import store (optional)" list="pf-stores" value={form.import} onChange={set("import")} disabled={!canManage} problems={problems}
          hint="Where the two images are downloaded. Empty: the provider's import store." />
      </div>

      {shown && all.length ? (
        <Alert variant="destructive" role="alert">
          <AlertTitle>{all.length === 1 ? "One setting needs correcting" : `${all.length} settings need correcting`}</AlertTitle>
          <AlertDescription><ul className="list-disc pl-5">{all.map((p) => <li key={p.field + p.message}>{p.message}</li>)}</ul></AlertDescription>
        </Alert>
      ) : null}
      {save.error ? (
        <Alert variant="destructive" role="alert">
          <AlertTitle>The settings were not saved</AlertTitle>
          <AlertDescription>
            <p>{errMsg(save.error)}</p>
            {refused.length ? <ul className="list-disc pl-5">{refused.map((p) => <li key={p}>{p}</li>)}</ul> : null}
          </AlertDescription>
        </Alert>
      ) : null}
      <div>
        <Button
          disabled={!canManage || save.isPending || !serviceId}
          onClick={() => {
            setShown(true)
            if (!all.length) save.mutate()
          }}
        >
          {save.isPending ? "Saving…" : installation ? "Save settings" : "Save draft"}
        </Button>
      </div>
    </StepCard>
  )
}

// ── Step 2: Proxmox identities ────────────────────────────────────────────────

export function IdentitiesStep({ installation, tokenId, canManage }: { installation: Installation; tokenId?: string; canManage: boolean }) {
  const qc = useQueryClient()
  const { boot, iso, import: imp } = installation.spec.storage
  const qs = new URLSearchParams({ managedServices: "true", bootStorage: boot, isoStorage: iso })
  if (tokenId) qs.set("tokenId", tokenId)
  if (imp) qs.set("importStorage", imp)
  const script = useQuery({
    queryKey: ["platform-script", qs.toString()],
    queryFn: () => fetchText(`/admin/setup/proxmox-script?${qs}`),
    placeholderData: keepPreviousData,
    retry: false,
  })
  // The secrets live in these two inputs only until they are sent: no answer carries them and nothing reads them back.
  const [capi, setCapi] = useState("")
  const [csi, setCsi] = useState("")
  const bad = (v: string) => v.trim() !== "" && !TOKEN_SECRET.test(v.trim())
  const save = useMutation({
    mutationFn: () => apiFetch(`${base(installation.id)}/credentials`, {
      method: "PUT",
      body: {
        ...(capi.trim() ? { capi: { tokenId: CAPI_TOKEN, secret: capi.trim() } } : {}),
        ...(csi.trim() ? { csi: { tokenId: CSI_TOKEN, secret: csi.trim() } } : {}),
      },
    }),
    onSuccess: () => {
      setCapi("")
      setCsi("")
      toast.success("Token secrets stored")
      return qc.invalidateQueries({ queryKey: PLATFORM_KEY })
    },
  })
  const secret = (id: string, token: string, value: string, set: (v: string) => void, stored: boolean) => (
    <div className="grid content-start gap-1.5">
      <Label htmlFor={id}>Secret of <span className="font-mono">{token}</span></Label>
      <Input id={id} type="password" className="font-mono" autoComplete="off" value={value} onChange={(e) => set(e.target.value)} disabled={!canManage}
        placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" aria-invalid={bad(value) || undefined} aria-describedby={`${id}-state`} />
      <p id={`${id}-state`} className="text-xs text-muted-foreground">
        {bad(value) ? <span className="text-destructive">Not a Proxmox token secret: it looks like xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx. </span> : null}
        {stored ? "Stored. It is never shown again; paste a new one to replace it." : "Not stored yet."}
      </p>
    </div>
  )
  return (
    <StepCard n={2} title="Proxmox identities" description="Done by hand: ProxCloud holds no root password for Proxmox and asks for none.">
      <p className="text-sm text-muted-foreground">
        Run this as root on one node. It adds two users with one token each (<code>{CAPI_TOKEN}</code> builds the machines,{" "}
        <code>{CSI_TOKEN}</code> manages their volumes), the pool <code>proxcloud-seed</code> and their permissions, and can be run again.
        It is made for the boot store <code>{boot}</code> and the ISO store <code>{iso}</code>: after changing either in step 1, run it again.
        It prints each token secret once. GPU permissions stay with the script of Platform setup.
      </p>
      {script.isLoading ? <Skeleton className="h-40" /> : null}
      <ErrorNote title="The script could not be generated" error={script.error} />
      {script.data && !script.error ? <CopyBlock text={script.data} label="identities script" /> : null}
      <div className="grid gap-4 sm:grid-cols-2">
        {secret("capi-secret", CAPI_TOKEN, capi, setCapi, !!installation.credentials?.capi)}
        {secret("csi-secret", CSI_TOKEN, csi, setCsi, !!installation.credentials?.csi)}
      </div>
      <ErrorNote title="The secrets were not stored" error={save.error} />
      <div>
        <Button disabled={!canManage || save.isPending || (!capi.trim() && !csi.trim()) || bad(capi) || bad(csi)} onClick={() => save.mutate()}>
          {save.isPending ? "Storing…" : "Store the secrets"}
        </Button>
      </div>
    </StepCard>
  )
}

// ── Step 3: Check ─────────────────────────────────────────────────────────────

export function CheckStep({ installation, canManage, dirty }: { installation: Installation; canManage: boolean; dirty: boolean }) {
  const check = useMutation({
    mutationFn: () => apiFetch<{ ok: boolean; checks: Check[] }>(`${base(installation.id)}/preflight`, { method: "POST", body: {} }),
  })
  const failed = (check.data?.checks ?? []).filter((c) => c.status === "fail")
  return (
    <StepCard n={3} title="Check" description="Reads Proxmox with the stored tokens and changes nothing. Install runs the same checks first.">
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="outline" disabled={!canManage || check.isPending || dirty} onClick={() => check.mutate()}>
          {check.isPending ? "Checking…" : check.data ? "Check again" : "Check"}
        </Button>
        {dirty ? <span className="text-sm text-muted-foreground">Save the settings first: the check reads what is saved.</span> : null}
      </div>
      <ErrorNote title="The check did not run" error={check.error} />
      {check.data ? (
        <p className="text-sm" role="status">
          {check.data.ok ? "Every check passed." : `${failed.length} of ${check.data.checks.length} checks failed.`}
        </p>
      ) : null}
      <CheckRows checks={check.data?.checks} />
      {failed.length ? (
        <div className="grid gap-2">
          <h3 className="text-sm font-semibold">What to do</h3>
          <ul aria-label="What to do for each failed check" className="grid gap-2 text-sm">
            {failed.map((c) => (
              <li key={c.id}><span className="mr-2 font-mono text-xs text-muted-foreground">{c.id}</span>{checkAction(c.id)}</li>
            ))}
          </ul>
        </div>
      ) : null}
    </StepCard>
  )
}

// ── Step 4: Review and install ────────────────────────────────────────────────

const short = (digest?: string) => (digest ? digest.replace(/^sha256:/, "").slice(0, 12) : "–")

export function ReviewStep({ installation, canManage, dirty, onInstall, installing, error }: {
  installation: Installation; canManage: boolean; dirty: boolean; onInstall: () => void; installing: boolean; error: unknown
}) {
  const bom = useQuery({ queryKey: ["platform-bom"], queryFn: () => apiFetch<BOM>("/admin/platform/bom"), retry: false, staleTime: 300_000 })
  const plan = useQuery({ queryKey: [...PLATFORM_KEY, "plan", installation.id, installation.rev], queryFn: () => apiFetch<Plan>(`${base(installation.id)}/plan`), retry: false })
  const s = installation.spec
  const c = s.clusters[0]
  const workers = c.nodeGroups.reduce((n, g) => n + g.count, 0)
  // Cores and memory per Proxmox node: the seed, the control plane, and each group's machines spread over its nodes.
  const load: Record<string, { cores: number; mem: number }> = {}
  const add = (node: string, cores: number, mem: number) => {
    load[node] = { cores: (load[node]?.cores ?? 0) + cores, mem: (load[node]?.mem ?? 0) + mem }
  }
  add(s.seed.node, s.seed.cores, s.seed.memoryMiB)
  add(c.controlPlane.node, c.controlPlane.cores, c.controlPlane.memoryMiB)
  for (const g of c.nodeGroups) g.nodes.forEach((n, i) => {
    const count = Math.floor(g.count / g.nodes.length) + (i < g.count % g.nodes.length ? 1 : 0)
    add(n, count * g.cores, count * g.memoryMiB)
  })
  const blocking = plan.data?.blocking ?? []
  const installed = !!installation.status.bom
  return (
    <StepCard n={4} title="Review and install" description="What this release installs and what it creates on Proxmox.">
      <ErrorNote title="The release contents could not be read" error={bom.error} />
      {bom.data ? (
        <div className="grid gap-2 text-sm">
          <p>
            Release <span className="font-mono">{bom.data.release}</span> with Kubernetes <span className="font-mono">{bom.data.kubernetes.version}</span>.
            Two images are imported: the seed image <span className="font-mono">{bom.data.seed.image.package}@{short(bom.data.seed.image.digest)}</span> and
            the node image <span className="font-mono">{bom.data.kubernetes.nodeImage.package}@{short(bom.data.kubernetes.nodeImage.digest)}</span>.
          </p>
          <details>
            <summary className="cursor-pointer underline underline-offset-4">The {bom.data.components.length} components of this release</summary>
            <div className="mt-2 overflow-x-auto">
              <table className="w-full text-left text-xs">
                <thead><tr className="border-b"><th scope="col" className="py-1 pr-3">Component</th><th scope="col" className="pr-3">Version</th><th scope="col" className="pr-3">Runs on</th><th scope="col" className="pr-3">Group</th><th scope="col">Digest</th></tr></thead>
                <tbody className="font-mono">
                  {bom.data.components.map((k) => (
                    <tr key={k.cluster + k.name} className="border-b last:border-0">
                      <td className="py-1 pr-3">{k.name}</td><td className="pr-3">{k.chart.version}</td>
                      <td className="pr-3">{k.cluster === "seed" ? "seed" : "platform cluster"}</td><td className="pr-3">{k.group || "system"}</td><td>{short(k.chart.digest)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </details>
        </div>
      ) : null}
      <div className="grid gap-1 text-sm">
        <h3 className="font-semibold">What is created</h3>
        <ul className="list-disc pl-5">
          <li>1 seed VM on {s.seed.node}, in the pool <code>{s.seed.pool || "proxcloud-seed"}</code>;</li>
          <li>1 control plane and {workers} worker machines in the pool <code>proxcloud-system</code>;</li>
          <li>the providers “{installation.name} Kubernetes” and “{installation.name} Databases”, held back from new projects until you attach them.</li>
        </ul>
      </div>
      <div className="grid gap-1 text-sm">
        <h3 className="font-semibold">What it takes on each node</h3>
        <ul aria-label="Cores and memory per node" className="list-disc pl-5">
          {Object.entries(load).sort().map(([node, l]) => (
            <li key={node}><span className="font-mono">{node}</span>: {l.cores} cores and {l.mem / 1024} GiB of memory</li>
          ))}
        </ul>
        <p className="text-muted-foreground">Compare it with the free memory of each node in Proxmox: this page does not read it.</p>
      </div>
      {!installed ? (
        <p className="text-sm text-muted-foreground">
          After the first successful install these cannot change: the name, the network, the seed's node and address, and the API address.
        </p>
      ) : null}
      <ErrorNote title="The plan could not be read" error={plan.error} />
      {plan.data?.installed ? (
        <div className="grid gap-1 text-sm">
          <h3 className="font-semibold">Changes from the installed release {plan.data.installed.release}</h3>
          {plan.data.changes?.length ? (
            <ul className="list-disc pl-5">
              {plan.data.changes.map((ch) => (
                <li key={(ch.cluster ?? "") + ch.name}>
                  <span className="font-mono">{ch.name}</span>: {ch.from || "new"} → {ch.to || "removed"}{ch.blocking ? ` (blocked: ${ch.blocking})` : ""}
                </li>
              ))}
            </ul>
          ) : <p className="text-muted-foreground">No component changes version.</p>}
        </div>
      ) : null}
      {blocking.length ? (
        <Alert variant="destructive">
          <AlertTitle>This release cannot be applied to what is installed</AlertTitle>
          <AlertDescription><ul className="list-disc pl-5">{blocking.map((b) => <li key={b}>{b}</li>)}</ul></AlertDescription>
        </Alert>
      ) : null}
      <ErrorNote title="The run did not start" error={error} />
      <div className="flex flex-wrap items-center gap-3">
        <Button disabled={!canManage || installing || dirty || blocking.length > 0} onClick={onInstall}>
          {installing ? "Starting…" : installed ? "Apply this release" : "Install"}
        </Button>
        {dirty ? <span className="text-sm text-muted-foreground">Save the settings first.</span> : null}
        <Link to="/docs/platform-admin/managed-services" className={LINK}>What each stage does</Link>
      </div>
    </StepCard>
  )
}
