// InstallSteps.tsx — the four steps of the Managed services page before a run: the settings (a draft installation),
// the Proxmox identities and their two token secrets, the checks, and the review with Install. The settings come
// first because the script and the tokens belong to a saved installation (the script takes its two stores).
import { useEffect, useState } from "react"
import { Link } from "react-router-dom"
import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"
import { cn } from "@/lib/utils"
import { CheckRows, CopyBlock, ErrorNote, LINK, StepCard, errMsg, fetchText, useClusterInfo } from "@/pages/setup/common"
import {
  CAPI_TOKEN, CSI_TOKEN, ENROLL_PREFIX, GROUPS, PLATFORM_KEY, GROUP_USE, PRESETS, SHARED_GIVES_UP, TOKEN_SECRET, applyPreset, attachDefault, bomBuildsThree, bomOffersSandboxes, checkAction, enrollProblem, emptyForm, errCode, errProblems, formToSpec, gib, isConflict, plannedMemory, sandboxRoom, specToForm, validate,
  type BOM, type Check, type Form, type Installation, type MachineForm, type Plan, type Preset, type Problem, type SandboxForm, type SiteNode, type TenantProbe, type Upgrade, type UpgradePart,
} from "./model"
import { AttachChoice } from "./Tenants"

const base = (id: string) => `/admin/platform/installations/${id}`
const SITE_KEY = ["platform-site-changes"]

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

// The nodes of the provider with their memory in MiB, as the connection test reads it: free is what Proxmox reports as
// not in use now (0 and 0 for a node that did not report it).
function useSiteNodes(serviceId: string): SiteNode[] {
  const cluster = useClusterInfo(serviceId)
  return (cluster.data?.nodes ?? []).map((n) => ({ node: n.node, maxMiB: Math.floor((n.maxmem ?? 0) / 2 ** 20), freeMiB: Math.floor(Math.max(0, (n.maxmem ?? 0) - (n.mem ?? 0)) / 2 ** 20) }))
}

const H3 = ({ children }: { children: React.ReactNode }) => <h3 className="border-b pb-1 text-sm font-semibold">{children}</h3>

// The Sandboxes block of the settings: off, a node group of the platform cluster, or a cluster of its own. built is
// what a run made (status.sandbox): from then on the placement only goes to Off, and the range and the proxy address
// stay. Shown only for a release whose BOM has the sandbox chart, or when the stored settings already hold a block.
function SandboxSettings({ value, onChange, nodes, problems, canManage, built, schedulerOff }: {
  value: SandboxForm; onChange: (v: SandboxForm) => void; nodes: string[]; problems: Problem[]; canManage: boolean
  built?: NonNullable<Installation["status"]["sandbox"]>; schedulerOff: boolean
}) {
  const on = value.placement === "shared" || value.placement === "separate"
  const set = (next: Partial<SandboxForm>) => onChange({ ...value, ...next })
  const text = (k: "nodeRange" | "proxyAddress" | "domain" | "dns") => (e: React.ChangeEvent<HTMLInputElement>) => set({ [k]: e.target.value })
  const choice = (placement: SandboxForm["placement"], title: string, body: string) => (
    <label className="flex items-start gap-2 text-sm">
      <input type="radio" name="pf-sandbox-placement" className="mt-1" disabled={!canManage || (!!built && placement !== "off" && placement !== built.placement)}
        checked={value.placement === placement || (placement === "off" && value.placement === "")} onChange={() => set({ placement, accept: false })} />
      <span><span className="font-medium">{title}.</span> {body}</span>
    </label>
  )
  const machines = (id: string, group: string, key: "machines" | "system") => {
    const g = value[key]
    const setGroup = (next: Partial<typeof g>) => set({ [key]: { ...g, ...next } })
    return (
      <div className="grid gap-3 rounded-lg border p-3" role="group" aria-label={`Group ${group}`}>
        <p className="text-sm">
          <span className="font-mono font-medium">{group}</span>{" "}
          <span className="text-muted-foreground">{key === "machines" ? "runs the sandboxes, and nothing else" : "runs sandbox-proxy and the sandbox controller, never on a machine that runs a sandbox"}</span>
        </p>
        <NodePicker id={`${id}-nodes`} group={group} nodes={nodes} value={g.nodes} onChange={(v) => setGroup({ nodes: v })} problems={problems} disabled={!canManage} />
        <div className="grid gap-3 sm:grid-cols-[8rem_1fr]">
          <Field id={`${id}-count`} label="Machines" aria-label={`Group ${group}: machines`} inputMode="numeric" value={g.count} onChange={(e) => setGroup({ count: e.target.value })} disabled={!canManage} problems={problems} />
          <Sizes id={id} what={`Group ${group}`} value={g} onChange={(m) => setGroup(m)} problems={problems} disabled={!canManage} />
        </div>
        {key === "machines" && sandboxRoom(g.cores) ? <p className="text-xs text-muted-foreground">{sandboxRoom(g.cores)}</p> : null}
      </div>
    )
  }
  const acceptProblem = problems.find((p) => p.field === "sbxAccept")
  return (
    <>
      <H3>Sandboxes</H3>
      <p className="text-sm text-muted-foreground">
        Optional. Short-lived, isolated environments for tenants (gVisor), with the E2B-compatible API. The install builds the machines and registers a sandbox
        provider that stays disabled: you set it live on its page under Cloud providers once its release gates hold.
      </p>
      <fieldset className="grid content-start gap-1.5">
        <legend className="mb-1.5 text-sm font-medium">Where sandboxes run</legend>
        {choice("off", "Off", built ? "The next apply deletes the sandbox machines and with them every running sandbox. Disable the sandbox provider first: the save is refused while it is live." : "No sandbox machine is built.")}
        {choice("shared", "Shared node group", "A node group of the platform cluster, beside every tenant's control plane and database. For a site with one operator and no outside tenants, or too little memory for a second control plane. Read what it gives up below.")}
        {choice("separate", "Separate sandbox cluster", "A cluster of its own, pc-sandbox: its control plane runs as pods on the kamaji group (about 0.6 GiB), with a system machine for sandbox-proxy. An upgrade that replaces machines is refused while it exists: switch sandboxes off, upgrade, then on again.")}
      </fieldset>
      {built ? <p className="text-xs text-muted-foreground">A run built placement {built.placement}: it changes only to Off, and the range and the proxy address stay as they are.</p> : null}
      {on ? (
        <>
          {machines("sbx", "sandbox", "machines")}
          {value.placement === "separate" ? machines("sbxsys", "sandbox-system", "system") : null}
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id="sbxRange" label="Sandbox range" value={value.nodeRange} onChange={text("nodeRange")} disabled={!canManage || !!built} problems={problems} placeholder="10.90.0.64/28"
              hint="Addresses of the sandbox machines: one network inside the platform network, as a CIDR, apart from the node range and the load balancer range. Each sandbox machine's firewall pins it to its address here." />
            <Field id="sbxProxy" label="Proxy address" value={value.proxyAddress} onChange={text("proxyAddress")} disabled={!canManage || !!built} problems={problems} placeholder={value.placement === "shared" ? "10.90.0.151" : "10.90.0.90"}
              hint={value.placement === "shared" ? "Where sandbox-proxy answers: one free address of the load balancer range." : "Where sandbox-proxy answers: a free address of the platform network outside every range; the sandbox cluster announces it."} />
            <Field id="sbxDomain" label="Sandbox domain" value={value.domain} onChange={text("domain")} disabled={!canManage} problems={problems} placeholder="sbx.example.org"
              hint="A sandbox answers at https://<port>-<id>.<domain>: point *.<domain> at the proxy address in your DNS." />
            <Field id="sbxDns" label="DNS servers of the sandbox machines (optional)" value={value.dns} onChange={text("dns")} disabled={!canManage} problems={problems}
              hint="Empty: the platform network's. Up to three." />
          </div>
          <p className="text-sm text-muted-foreground">
            Certificate: the install makes a self-signed certificate for *.{value.domain.trim() || "<domain>"} and keeps its key sealed. Clients add the certificate to their trust store; it is offered for download once the platform is ready.
            A certificate of your own is not stored by this release.
          </p>
          <p className="text-sm text-muted-foreground" aria-label="Sandboxes and the job scheduler">
            Sandboxes need the job scheduler: it fences each sandbox machine, keeps watching its firewall and ends sandboxes that are gone, and the provider cannot be set live without it.
            Switching the scheduler on also starts charging usage. The Platform setup defaults add no price rule for sandbox minutes: add one to the price plan (or one at 0), the provider is refused live without it.
            {schedulerOff ? " The scheduler is off on this ProxCloud now." : ""}
          </p>
        </>
      ) : null}
      {value.placement === "shared" ? (
        <Alert role="note" aria-label="What a shared node group gives up">
          <AlertTitle>What a shared node group gives up</AlertTitle>
          <AlertDescription>
            <ul className="list-disc pl-5">{SHARED_GIVES_UP.map((line) => <li key={line}>{line}</li>)}</ul>
            <span className="mt-3 flex items-start gap-2">
              <Checkbox id="sbxAccept" className="mt-0.5" checked={value.accept} disabled={!canManage} aria-invalid={acceptProblem ? true : undefined}
                aria-describedby={acceptProblem ? "sbxAccept-error" : undefined} onCheckedChange={(v) => set({ accept: v === true })} />
              <Label htmlFor="sbxAccept" className="font-normal">I have read this, and sandboxes on this site may run on a shared node group.</Label>
            </span>
            {acceptProblem ? <p id="sbxAccept-error" className="mt-1 text-xs text-destructive">{acceptProblem.message}</p> : null}
          </AlertDescription>
        </Alert>
      ) : null}
    </>
  )
}

// The page mounts this form anew (by key) when the stored settings changed, so it always starts from what is stored.
// locked: a run that ended without being settled still holds the installation, and the API refuses a save until then.
// conflict: the last Save was refused because someone else had saved first; the page keeps that across the remount.
export function SettingsStep({ installation, serviceId, canManage, locked, conflict, onConflict, onDirty }: {
  installation?: Installation; serviceId: string; canManage: boolean; locked: boolean
  conflict: boolean; onConflict: (conflict: boolean) => void; onDirty: (dirty: boolean) => void
}) {
  const qc = useQueryClient()
  const cluster = useClusterInfo(serviceId)
  const stored = installation ? specToForm(installation.name, installation.spec) : emptyForm
  const [form, setFormState] = useState<Form>(stored)
  const [shown, setShown] = useState(false) // problems are shown after the first Save
  useEffect(() => onDirty(false), [onDirty]) // a form that just mounted shows what is stored
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
  const siteNodes = useSiteNodes(serviceId)
  // The release an install or apply runs with: the installed one for a platform that stays on it, else this ProxCloud's.
  const bomSha = installation?.status.bom?.sha256 ?? ""
  const bom = useQuery({ queryKey: ["platform-bom", bomSha], queryFn: () => apiFetch<BOM>(`/admin/platform/bom${bomSha ? `?sha256=${bomSha}` : ""}`), retry: false, staleTime: 300_000 })
  const three = bomBuildsThree(bom.data)
  const [preset, setPreset] = useState<Preset>("custom")
  const [notes, setNotes] = useState<string[]>([])
  const pick = (p: Preset) => {
    const filled = applyPreset(p, form, { nodes: siteNodes, three })
    setPreset(p)
    setNotes(filled.notes)
    setForm(filled.form)
  }
  // A control plane a run built with three machines stays three; a 3 that was saved and never applied goes back.
  const wasThree = (installation?.status.clusters?.[0]?.controlPlaneReplicas ?? 1) > 1
  // A new installation's attach choice defaults by whether the site already has projects (undefined: not counted).
  const projectList = useAdminList("/admin/project", !installation)
  const projects = projectList.data?.data?.length
  const attach = form.attach || attachDefault(projects)
  const spec = (prev?: Installation["spec"]) => formToSpec({ ...form, attach }, prev)

  const save = useMutation({
    mutationFn: () => installation
      ? apiFetch<Installation>(base(installation.id), { method: "PUT", body: { name: form.name, spec: spec(installation.spec), rev: installation.rev } })
      : apiFetch<Installation>("/admin/platform/installations", { method: "POST", body: { name: form.name, serviceId, layout: "shared", spec: spec() } }),
    onSuccess: (saved) => {
      toast.success("Settings saved")
      qc.setQueryData(SITE_KEY, saved.siteChanges ?? null)
      onDirty(false)
      onConflict(false)
      return qc.invalidateQueries({ queryKey: PLATFORM_KEY })
    },
    // Refused because the page's copy is behind: read the installation again, which shows the other save or the run.
    onError: (e) => {
      if (!isConflict(e)) return
      if (errCode(e) === "rev_conflict") onConflict(true)
      void qc.invalidateQueries({ queryKey: PLATFORM_KEY })
    },
  })
  const refused = errProblems(save.error)
  // What the last save changed on the provider, so that nobody has to (blocked networks, the tenant zone). Kept in
  // the query cache, not in state: the page mounts this step anew after every save.
  const site = useQuery<Installation["siteChanges"] | null>({ queryKey: SITE_KEY, queryFn: () => null, enabled: false, staleTime: Infinity }).data

  return (
    <StepCard n={1} title="Settings" description="What gets built and where. Saving keeps a draft: nothing is created on Proxmox until you press Install.">
      {installed ? (
        <p className="text-sm text-muted-foreground">
          The name, the network, the seed's node and address, the API address and the control plane's node were fixed by the first
          successful install. To change them, remove the installation and install again.
        </p>
      ) : null}
      <datalist id="pf-nodes">{nodes.map((n) => <option key={n} value={n} />)}</datalist>
      <datalist id="pf-stores">{(cluster.data?.storages ?? []).map((s) => <option key={s.storage} value={s.storage} />)}</datalist>
      <datalist id="pf-bridges">{[...new Set((cluster.data?.bridges ?? []).filter((b) => !b.management).map((b) => b.iface))].map((b) => <option key={b} value={b} />)}</datalist>

      <fieldset className="grid gap-2" aria-describedby="pf-preset-hint">
        <legend className="mb-1.5 text-sm font-semibold">Start from</legend>
        <div className="grid gap-2 sm:grid-cols-3">
          {PRESETS.map((p) => (
            <label key={p.id} className={cn("flex items-start gap-2 rounded-lg border p-3 text-sm", preset === p.id && "border-foreground")}>
              <input type="radio" name="pf-preset" className="mt-1" checked={preset === p.id} disabled={fixed} onChange={() => pick(p.id)} />
              <span><span className="font-medium">{p.title}</span><span className="block text-muted-foreground">{p.text}</span></span>
            </label>
          ))}
        </div>
        <p id="pf-preset-hint" className="text-xs text-muted-foreground">
          {installed
            ? "A preset is a start for a new installation. This platform is installed, and a preset would resize and replace its machines, so change the fields below one by one."
            : "A choice fills in the nodes, counts and sizes below and nothing else. Every field stays yours to change, and what is saved is the fields, not the choice."}
        </p>
        {notes.length ? <ul aria-label="What the preset changed" className="list-disc pl-5 text-sm">{notes.map((n) => <li key={n}>{n}</li>)}</ul> : null}
      </fieldset>

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

      <H3>Tenants</H3>
      <AttachChoice value={form.attach} projects={projects} disabled={!canManage} onChange={(attach) => setForm({ ...form, attach })} />

      <H3>Access</H3>
      <p className="text-sm text-muted-foreground">
        Optional, and it can be set, changed or cleared later. With both fields filled, the ArgoCD of the platform cluster (every tenant cluster and
        database) answers at https://&lt;address&gt; after the next apply, with a self-signed certificate. Its admin can change everything on that cluster.
        The seed's ArgoCD never gets an address.
      </p>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="argoAddress" label="ArgoCD address (optional)" value={form.argoAddress} onChange={set("argoAddress")} disabled={!canManage} problems={problems} placeholder="10.90.0.150"
          hint="One free address of the load balancer range. Tenants' load balancers draw from the same range." />
        <Field id="argoSources" label="Operator networks" value={form.argoSources} onChange={set("argoSources")} disabled={!canManage} problems={problems} placeholder="192.0.2.0/24"
          hint="Only these networks are let in, separated by commas. Where tenant traffic leaves under a Proxmox node's address (SNAT), a network that contains that address lets every tenant VM reach the login page: the Check warns." />
      </div>

      <H3>Node groups</H3>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">Three groups of worker machines. The machines of a group are spread over the nodes you choose. Counts, sizes and nodes can change later.</p>
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
        A network of its own for the seed and the platform cluster. Tenants must not reach the seed, the API
        address or the node range; they do reach the load balancer range.
      </p>
      <fieldset className="grid content-start gap-1.5" aria-describedby={problems.some((p) => p.field === "networkMode") ? "networkMode-error" : undefined}>
        <legend className="mb-1.5 text-sm font-medium">Where the network comes from</legend>
        <label className="flex items-start gap-2 text-sm">
          <input type="radio" name="pf-network-mode" className="mt-1" checked={form.networkMode === "bridge"} disabled={fixed} onChange={() => setForm({ ...form, networkMode: "bridge", bridge: "" })} />
          <span>An existing bridge. It exists on every node used, and your router routes the network. A VLAN tag is optional: with one, no bridge or vnet of its own is needed for the VLAN.</span>
        </label>
        <label className="flex items-start gap-2 text-sm">
          <input type="radio" name="pf-network-mode" className="mt-1" checked={form.networkMode === "routed"} disabled={fixed} onChange={() => setForm({ ...form, networkMode: "routed", vlan: "", bridge: "" })} />
          <span>Routed by the node (one Proxmox node only). ProxCloud makes the network as an SDN vnet whose gateway the node holds, with SNAT. Tenant traffic to the platform then never leaves the node. Everything outside the node needs a route to this network through the node's address. Not verified on a lab.</span>
        </label>
        {problems.filter((p) => p.field === "networkMode").map((p) => <p key={p.message} id="networkMode-error" className="text-xs text-destructive">{p.message}</p>)}
      </fieldset>
      {/* The name means another thing in each mode (a bridge that exists, a vnet that must not), so a switch empties it:
          a bridge's name carried into "routed" would ask Proxmox for a vnet named like a live interface. */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {form.networkMode === "routed" ? (
          <Field id="bridge" label="Vnet name" value={form.bridge} onChange={set("bridge")} disabled={fixed} problems={problems} placeholder="pcplat0"
            hint="A name Proxmox does not have yet: the install makes the vnet." />
        ) : (
          <>
            <Field id="bridge" label="Bridge" list="pf-bridges" value={form.bridge} onChange={set("bridge")} disabled={fixed} problems={problems} placeholder="vmbr1" />
            <Field id="vlan" label="VLAN tag (optional)" inputMode="numeric" value={form.vlan} onChange={set("vlan")} disabled={fixed} problems={problems} placeholder="140"
              hint="1 to 4094. Every machine's NIC on the bridge gets this tag." />
          </>
        )}
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
            aria-invalid={problems.some((p) => p.field === "sshKeys") || undefined} aria-describedby={problems.some((p) => p.field === "sshKeys") ? "sshKeys-hint sshKeys-error" : "sshKeys-hint"} />
          <p id="sshKeys-hint" className="text-xs text-muted-foreground">
            For emergencies only, one per line. With a key, the seed also accepts SSH from the API sources. ProxCloud never uses SSH.
          </p>
          {problems.some((p) => p.field === "sshKeys") ? (
            <p id="sshKeys-error" className="text-xs text-destructive">{problems.filter((p) => p.field === "sshKeys").map((p) => p.message).join(" ")}</p>
          ) : null}
        </div>
      </div>

      <H3>Control plane</H3>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="cpNode" label="Control plane node" list="pf-nodes" value={form.cpNode} onChange={set("cpNode")} disabled={fixed} problems={problems} placeholder="pve1"
          hint="Every control plane machine of the platform cluster runs on this node." />
        <Sizes id="cp" what="Control plane" value={form.cp} onChange={(m) => setForm({ ...form, cp: m })} problems={problems} disabled={!canManage} />
        <div className="grid content-start gap-1.5 sm:col-span-2">
          <Label htmlFor="cpReplicas">Control plane machines</Label>
          <select id="cpReplicas" className="h-9 w-40 rounded-md border bg-background px-2 text-sm" value={form.cpReplicas} disabled={!canManage} aria-describedby="cpReplicas-hint"
            onChange={(e) => setForm({ ...form, cpReplicas: e.target.value })}>
            <option value="1" disabled={wasThree}>1</option>
            <option value="3" disabled={!three && form.cpReplicas !== "3"}>3, on this one node</option>
          </select>
          <p id="cpReplicas-hint" className="text-xs text-muted-foreground">
            This is the platform cluster's own control plane, not a tenant's. Three machines keep its API up when one of their VMs is lost or replaced during an update.
            They share one Proxmox node and one store, so they do not survive the loss of that node or of its disk, and three etcd members write more to that store than one.
            The node needs memory for one machine more than the count: an update makes the new machine before it removes an old one, and a machine that does not fit is refused for good.
            From 1 to 3 can be done later, with Apply; once an apply has built three, back to 1 cannot. Machines on several nodes are not built in this release. Not verified on a lab.
            {bom.data && !three ? " This release builds one: three need a release whose cluster chart is 0.1.14 or newer." : ""}
            {wasThree ? " A run built this platform with three, so it stays at three." : ""}
          </p>
        </div>
      </div>

      {bomOffersSandboxes(bom.data) || form.sandbox.placement !== "" ? (
        <SandboxSettings value={form.sandbox} onChange={(sandbox) => setForm({ ...form, sandbox })} nodes={nodes} problems={problems} canManage={canManage}
          built={installation?.status.sandbox} schedulerOff={!!installation?.schedulerNote} />
      ) : null}

      <H3>Storage</H3>
      <div className="grid gap-4 sm:grid-cols-3">
        <Field id="boot" label="Boot store" list="pf-stores" value={form.boot} onChange={set("boot")} disabled={!canManage} problems={problems} placeholder="local-zfs"
          hint="Disks of every machine, and the volumes of the platform's own components." />
        <Field id="iso" label="ISO store" list="pf-stores" value={form.iso} onChange={set("iso")} disabled={!canManage} problems={problems} placeholder="local"
          hint="Local to each node. Start-up data of the machines goes here." />
        <Field id="import" label="Import store (optional)" list="pf-stores" value={form.import} onChange={set("import")} disabled={!canManage} problems={problems}
          hint="Where the two images are downloaded, not the boot store. Empty: the provider's import store." />
      </div>

      {shown && all.length ? (
        <Alert variant="destructive" role="alert">
          <AlertTitle>{all.length === 1 ? "One setting needs correcting" : `${all.length} settings need correcting`}</AlertTitle>
          <AlertDescription><ul className="list-disc pl-5">{all.map((p) => <li key={p.field + p.message}>{p.message}</li>)}</ul></AlertDescription>
        </Alert>
      ) : null}
      {conflict ? (
        <Alert>
          <AlertTitle>Someone else changed these settings</AlertTitle>
          <AlertDescription>Your change was not saved. The settings were read again and show what is stored now: enter your change once more, then save.</AlertDescription>
        </Alert>
      ) : null}
      {save.error && errCode(save.error) !== "rev_conflict" ? (
        <Alert variant="destructive" role="alert">
          <AlertTitle>The settings were not saved</AlertTitle>
          <AlertDescription>
            <p>{errMsg(save.error)}</p>
            {refused.length ? <ul className="list-disc pl-5">{refused.map((p) => <li key={p}>{p}</li>)}</ul> : null}
          </AlertDescription>
        </Alert>
      ) : null}
      {site ? (
        <Alert role="status">
          <AlertTitle>The save also changed the provider</AlertTitle>
          <AlertDescription>
            <ul className="list-disc pl-5">
              {site.blockedCidrs?.length ? <li>Added to its blocked networks, so that tenants cannot reach the seed, the API address and the node range: <span className="font-mono">{site.blockedCidrs.join(", ")}</span>. The next provider sync writes the firewall rules (step 3 has the button).</li> : null}
              {site.blockedCidrsRemoved?.length ? <li>Taken off its blocked networks, because the settings no longer need them (an earlier save had added them): <span className="font-mono">{site.blockedCidrsRemoved.join(", ")}</span>. If a provider sync already wrote firewall rules for them, the next sync rewrites the rules on a provider with a project; on one without, the Check row <span className="font-mono">isolation</span> names the rules to delete by hand.</li> : null}
              {site.zoneWithoutDhcp ? <li>The default tenant zone <span className="font-mono">{site.zoneWithoutDhcp}</span> does not exist in Proxmox yet and is now set to be created without DHCP, which tenant Kubernetes clusters need.</li> : null}
              {site.problem ? <li>{site.problem}</li> : null}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}
      <div className="flex flex-wrap items-center gap-3">
        <Button
          disabled={!canManage || save.isPending || !serviceId || locked}
          onClick={() => {
            setShown(true)
            if (!all.length) save.mutate()
          }}
        >
          {save.isPending ? "Saving…" : installation ? "Save settings" : "Save draft"}
        </Button>
        {locked ? <span className="text-sm text-muted-foreground">The run above ended without finishing. Press Retry or remove the installation first; settings can be saved after that.</span> : null}
      </div>
    </StepCard>
  )
}

// ── Step 2: Proxmox identities ────────────────────────────────────────────────

export function IdentitiesStep({ installation, tokenId, canManage }: { installation: Installation; tokenId?: string; canManage: boolean }) {
  const qc = useQueryClient()
  const { boot, iso } = installation.spec.storage
  const held = { capi: !!installation.credentials?.capi, csi: !!installation.credentials?.csi }
  const qs = new URLSearchParams({ installation: installation.id })
  if (tokenId) qs.set("tokenId", tokenId)
  // The command is made for this installation's stores and for the secrets already stored: both are part of the key.
  const script = useQuery({
    queryKey: ["platform-script", qs.toString(), boot, iso, installation.spec.storage.import, held.capi, held.csi],
    queryFn: () => fetchText(`/admin/setup/proxmox-script?${qs}`),
    placeholderData: keepPreviousData,
    retry: false,
  })
  // The secrets live in these inputs only until they are sent: no answer carries them and nothing reads them back.
  const [line, setLine] = useState("")
  const [capi, setCapi] = useState("")
  const [csi, setCsi] = useState("")
  const lineProblem = line.trim() ? enrollProblem(line, installation.id) : null
  const bad = (v: string) => v.trim() !== "" && !TOKEN_SECRET.test(v.trim())
  const save = useMutation({
    mutationFn: (body: Record<string, unknown>) => apiFetch(`${base(installation.id)}/credentials`, { method: "PUT", body }),
    onSuccess: () => {
      setLine("")
      setCapi("")
      setCsi("")
      toast.success("Token secrets stored")
      return qc.invalidateQueries({ queryKey: PLATFORM_KEY })
    },
  })
  const secret = (id: string, token: string, value: string, set: (v: string) => void) => (
    <div className="grid content-start gap-1.5">
      <Label htmlFor={id}>Secret of <span className="font-mono">{token}</span></Label>
      <Input id={id} type="password" className="font-mono" autoComplete="new-password" data-1p-ignore data-lpignore="true" spellCheck={false} value={value} onChange={(e) => set(e.target.value)} disabled={!canManage}
        placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" aria-invalid={bad(value) || undefined} aria-describedby={`${id}-state`} />
      <p id={`${id}-state`} className="text-xs text-muted-foreground">
        {bad(value) ? <span className="text-destructive">Not a Proxmox token secret: it looks like xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.</span> : null}
      </p>
    </div>
  )
  return (
    <StepCard n={2} title="Proxmox identities" description="One command as root on one node: ProxCloud holds no root password for Proxmox and asks for none.">
      <p className="text-sm text-muted-foreground">
        Paste this into a root shell on one node. It adds two users with one token each (<code>{CAPI_TOKEN}</code> builds the machines,{" "}
        <code>{CSI_TOKEN}</code> manages their volumes), the pool <code>proxcloud-seed</code> and their permissions, and can be run again.
        It is made for the boot store <code>{boot}</code> and the ISO store <code>{iso}</code>: after changing either in step 1, run it again.
        It prints no secret. Its last line starts with <code>{ENROLL_PREFIX}</code> and carries the secrets of the tokens it made: paste that line below.
        When one of the two tokens exists already and is not this installation's, the command stops before it changes anything and prints
        the command that removes the token, for you to run once you know nothing else uses it. GPU permissions stay with the script of Platform setup.
      </p>
      {script.isLoading ? <Skeleton className="h-40" /> : null}
      <ErrorNote title="The command could not be generated" error={script.error} />
      {script.data && !script.error ? <CopyBlock text={script.data} label="root command" /> : null}
      <ul aria-label="Stored token secrets" className="text-sm">
        {([[CAPI_TOKEN, held.capi], [CSI_TOKEN, held.csi]] as const).map(([token, stored]) => (
          <li key={token}><span className="font-mono">{token}</span>: {stored ? "stored. It is never shown again." : "not stored yet."}</li>
        ))}
      </ul>
      <div className="grid gap-1.5">
        <Label htmlFor="enroll-line">The PC-ENROLL line the command printed</Label>
        <Input id="enroll-line" type="password" className="font-mono" autoComplete="new-password" data-1p-ignore data-lpignore="true" spellCheck={false} value={line}
          onChange={(e) => setLine(e.target.value)} disabled={!canManage} placeholder={`${ENROLL_PREFIX}…`} aria-invalid={!!lineProblem || undefined} aria-describedby="enroll-line-state" />
        <p id="enroll-line-state" className="text-xs text-muted-foreground">
          {lineProblem ? <span className="text-destructive">{lineProblem}</span> : "The line is a password: it is sent once and not kept on this page."}
        </p>
      </div>
      <ErrorNote title="The secrets were not stored" error={save.error} />
      <div>
        <Button disabled={!canManage || save.isPending || !line.trim() || !!lineProblem} onClick={() => save.mutate({ enroll: line.replace(/\s+/g, "") })}>
          {save.isPending ? "Storing…" : "Store the secrets"}
        </Button>
      </div>
      <details className="text-sm">
        <summary className="cursor-pointer text-muted-foreground">Enter the two secrets by hand instead</summary>
        <div className="mt-3 grid gap-3">
          <p className="text-muted-foreground">For a token made another way (<code>pveum user token add</code> prints its secret once).</p>
          <div className="grid gap-4 sm:grid-cols-2">
            {secret("capi-secret", CAPI_TOKEN, capi, setCapi)}
            {secret("csi-secret", CSI_TOKEN, csi, setCsi)}
          </div>
          <div>
            <Button variant="outline" disabled={!canManage || save.isPending || (!capi.trim() && !csi.trim()) || bad(capi) || bad(csi)}
              onClick={() => save.mutate({
                ...(capi.trim() ? { capi: { tokenId: CAPI_TOKEN, secret: capi.trim() } } : {}),
                ...(csi.trim() ? { csi: { tokenId: CSI_TOKEN, secret: csi.trim() } } : {}),
              })}>
              Store these secrets
            </Button>
          </div>
        </div>
      </details>
    </StepCard>
  )
}

// ── Step 3: Check ─────────────────────────────────────────────────────────────

const PROBE_RESULT = { open: "opened", refused: "answered (refused)", none: "no answer" } as const

// "Test the tenant path": the one check that opens a connection, and the only one that writes to Proxmox (a throwaway
// network and VM, removed again). It runs when the button is pressed, never with Check, and only on an installed platform.
function TenantPathTest({ installation, canManage }: { installation: Installation; canManage: boolean }) {
  const qc = useQueryClient()
  const key = [...PLATFORM_KEY, "tenant-path-probe", installation.id]
  const c = installation.spec.clusters[0]
  const nodes = [...new Set([c.controlPlane.node, ...c.nodeGroups.flatMap((g) => g.nodes)])].sort()
  const [node, setNode] = useState(c.controlPlane.node)
  const probe = useQuery({
    queryKey: key,
    queryFn: () => apiFetch<TenantProbe>(`${base(installation.id)}/tenant-path-probe`).catch((e) => { if (errCode(e) === "no_probe") return null; throw e }),
    refetchInterval: (q) => (q.state.data?.state === "running" ? 3000 : false),
    retry: false,
  })
  const start = useMutation({
    mutationFn: () => apiFetch<TenantProbe>(`${base(installation.id)}/tenant-path-probe`, { method: "POST", body: { node } }),
    onSuccess: (p) => qc.setQueryData(key, p),
  })
  const p = probe.data
  const running = p?.state === "running"
  return (
    <section aria-label="Test of the tenant path" className="grid gap-2 rounded-lg border p-3 text-sm">
      <h3 className="font-semibold">Test the tenant path</h3>
      <p className="text-muted-foreground">
        The checks above read the configuration. This test opens a connection: it makes a small network in the tenant zone and a small VM on the
        chosen node, connects from it to what listens in the load balancer range, and removes both again. It costs one VM clone and two SDN applies,
        takes a few minutes, and needs a tenant cluster or database to connect to and, published on the node, an Ubuntu or Debian image that Template
        updates imported for this provider. The VM is cloned as a tenant's server is, never from a template someone else put on the cluster.
      </p>
      <div className="flex flex-wrap items-end gap-3">
        <div className="grid gap-1.5">
          <Label htmlFor="probe-node">Node to test from</Label>
          <select id="probe-node" className="h-9 rounded-md border bg-background px-2 font-mono text-sm" value={node} onChange={(e) => setNode(e.target.value)} disabled={!canManage || running || start.isPending}>
            {nodes.map((n) => <option key={n} value={n}>{n}</option>)}
          </select>
        </div>
        <Button variant="outline" disabled={!canManage || running || start.isPending} onClick={() => start.mutate()}>
          {running || start.isPending ? "Testing…" : "Test the tenant path"}
        </Button>
      </div>
      <ErrorNote title="The test did not start" error={start.error} />
      <ErrorNote title="The last test could not be read" error={probe.error} />
      {p ? (
        <div className="grid gap-1.5" role="status">
          <p>
            <span className="mr-2 font-mono text-xs text-muted-foreground">{p.state}</span>
            {running ? `Running on ${p.node}${p.image ? `, from the image ${p.image}` : ""}: ${p.step || "starting"}.` : p.message}
          </p>
          {!running && p.targets?.length ? (
            <ul aria-label="Connections of the last test" className="list-disc pl-5 text-muted-foreground">
              {p.targets.map((t) => (
                <li key={`${t.address}:${t.port}`}>
                  <span className="font-mono">{t.address}:{t.port}</span> {t.result ? PROBE_RESULT[t.result] : "not tried"}
                  {t.expect === "closed" ? " (the platform's own API address: tenants must not reach it)" : ""}
                </li>
              ))}
            </ul>
          ) : null}
          {p.left?.length ? (
            <Alert variant="destructive"><AlertTitle>The cleanup left something behind</AlertTitle><AlertDescription><ul className="list-disc pl-5">{p.left.map((l) => <li key={l}>{l}</li>)}</ul></AlertDescription></Alert>
          ) : null}
          {!running && p.notes?.length ? (
            <ul aria-label="What Proxmox may keep" className="list-disc pl-5 text-muted-foreground">{p.notes.map((n) => <li key={n}>{n}</li>)}</ul>
          ) : null}
        </div>
      ) : null}
    </section>
  )
}

export function CheckStep({ installation, canManage, dirty }: { installation: Installation; canManage: boolean; dirty: boolean }) {
  const check = useMutation({
    mutationFn: () => apiFetch<{ ok: boolean; checks: Check[] }>(`${base(installation.id)}/preflight`, { method: "POST", body: {} }),
  })
  // The isolation rules are written by a provider sync; nothing else on this page starts one (Go live does, once).
  const sync = useMutation({
    mutationFn: () => apiFetch("/admin/job/external-service/sync", { method: "POST" }),
    onSuccess: () => check.mutate(),
  })
  const failed = (check.data?.checks ?? []).filter((c) => c.status === "fail")
  const warned = (check.data?.checks ?? []).filter((c) => c.status === "warn").length
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
          {check.data.ok ? (warned ? `No check failed. ${warned} ${warned === 1 ? "warning does" : "warnings do"} not stop the install: read ${warned === 1 ? "it" : "them"} below.` : "Every check passed.") : `${failed.length} of ${check.data.checks.length} checks failed.`}
        </p>
      ) : null}
      <CheckRows checks={check.data?.checks} />
      {installation.status.bom ? <TenantPathTest installation={installation} canManage={canManage} /> : null}
      {failed.length ? (
        <div className="grid gap-2">
          <h3 className="text-sm font-semibold">What to do</h3>
          <ul aria-label="What to do for each failed check" className="grid gap-2 text-sm">
            {failed.map((c) => (
              <li key={c.id} className="grid gap-1.5">
                <span><span className="mr-2 font-mono text-xs text-muted-foreground">{c.id}</span>{checkAction(c.id)}</span>
                {c.id === "isolation" ? (
                  <span className="flex flex-wrap items-center gap-3">
                    <Link to={`/system/cloud-providers/${installation.serviceId}`} className="underline underline-offset-4">Open the provider's page</Link>
                    <Button variant="outline" size="sm" disabled={!canManage || sync.isPending || check.isPending} onClick={() => sync.mutate()}>
                      {sync.isPending ? "Syncing…" : "Run a provider sync, then check again"}
                    </Button>
                  </span>
                ) : null}
              </li>
            ))}
          </ul>
          <ErrorNote title="The provider sync did not run" error={sync.error} />
        </div>
      ) : null}
    </StepCard>
  )
}

// ── Step 4: Review and install ────────────────────────────────────────────────

const short = (digest?: string) => (digest ? digest.replace(/^sha256:/, "").slice(0, 12) : "–")

// The move of an installed platform to this ProxCloud's release, as the plan gives it: allowed with every change and
// what it does, refused with each reason, or started and not finished. Starting it asks for the typed confirmation.
// With `replaces` it is the move offered in place of an unfinished upgrade toward that release.
// Starts an apply of an upgrade: targetBom "" continues the one in progress; part "seed" is the first of the two
// actions of an upgrade that replaces machines.
export type UpgradeStart = (targetBom: string, confirm: string, seedBackupTaken: boolean, part?: "" | "seed") => void

function UpgradePlan({ installation, upgrade, replaces, canManage, dirty, pending, onUpgrade }: {
  installation: Installation; upgrade: Upgrade; replaces?: string; canManage: boolean; dirty: boolean; pending: boolean; onUpgrade: UpgradeStart
}) {
  const id = replaces ? "pf-upgrade-instead" : "pf-upgrade"
  // What is being confirmed: the part ("" = the whole apply, or the platform part) and its phrase. null: nothing.
  const [asking, setAsking] = useState<{ part: "" | "seed"; confirm: string } | null>(null)
  const [typed, setTyped] = useState("")
  const { from, to } = upgrade
  const parts = upgrade.parts ?? []
  const twoPart = parts.length > 0 // the move replaces the platform cluster's machines: two actions, not one button
  const offered = upgrade.allowed && !upgrade.inProgress
  // The one reason the operator can answer: no backup of the seed VM that ProxCloud can see.
  const SEED_BACKUP = "seed_backup_missing"
  const needsBackup = !upgrade.inProgress && [...upgrade.blocking, ...parts.flatMap((p) => p.blocking)].some((b) => b.startsWith(SEED_BACKUP)) && (offered || twoPart)
  const [backupTaken, setBackupTaken] = useState(false)
  // An upgrade in progress is continued without a target: the record names it.
  const start = (part: "" | "seed", confirm: string) => onUpgrade(upgrade.inProgress ? "" : to.sha256, confirm, needsBackup && backupTaken, part)
  const noWayBack = upgrade.changes.find((c) => c.name === "roll")?.impact
  const units = upgrade.changes.filter((c) => c.name.startsWith("roll/"))
  const deleteFirst = (unit: string) => !!upgrade.changes.find((c) => c.name === "roll-vm/" + unit.slice(5))?.to?.includes("deleted first")
  const changes = twoPart ? upgrade.changes.filter((c) => c.name !== "roll" && !c.name.startsWith("roll/")) : upgrade.changes
  const partRow = (p: UpgradePart, n: number) => {
    const seed = p.id === "seed"
    const reasons = p.blocking.filter((b) => !b.startsWith(SEED_BACKUP))
    const label = seed ? "Seed and node image" : "Platform cluster"
    return (
      <li key={p.id} className="grid gap-1.5 rounded-md border p-2" data-part={p.id}>
        <p>
          <span className="font-semibold">{n}. {label}{p.done ? " (done)" : ""}.</span>{" "}
          {seed
            ? "Imports the release's node image on every node the platform cluster uses, replaces k3s on the seed when the release names another, and moves the seed's own components. It replaces no machine and touches no tenant cluster or database."
            : "Replaces every machine of the platform cluster with one of the new Kubernetes version: the control plane first, then one node group and node at a time, as the table below says. Everything else of the release moves in the same run."}
        </p>
        {reasons.length ? (
          <ul aria-label={`What ${label} waits for`} className="list-disc pl-5">{reasons.map((b) => <li key={b}>{b}</li>)}</ul>
        ) : null}
        {p.done ? null : (
          <div>
            <Button variant="outline" disabled={!canManage || pending || dirty || !p.allowed || (needsBackup && !backupTaken)}
              onClick={() => { if (p.confirm) { setTyped(""); setAsking({ part: seed ? "seed" : "", confirm: p.confirm }) } else start(seed ? "seed" : "", "") }}>
              {seed ? "Seed and node image" : p.confirm || !upgrade.inProgress ? "Replace the machines" : "Resume the replacement"}
            </Button>
          </div>
        )}
      </li>
    )
  }
  return (
    <section aria-labelledby={id} className="grid gap-2 rounded-md border p-3 text-sm">
      <h3 id={id} className="font-semibold">
        {twoPart ? `Kubernetes upgrade to release ${to.release}` : upgrade.inProgress ? `The upgrade to release ${to.release} has not finished` : offered ? `Upgrade to release ${to.release}${replaces ? " instead" : ""}` : replaces ? `Release ${to.release} cannot take the place of the unfinished upgrade` : `The platform stays on release ${from.release}`}
      </h3>
      {twoPart ? (
        <>
          {noWayBack ? <p role="note" className="font-semibold">{noWayBack}</p> : null}
          <p>
            Release <span className="font-mono">{to.release}</span> names another Kubernetes version or node image than the platform cluster runs, so its machines are replaced.
            It takes two actions, in this order.{upgrade.inProgress ? ` The upgrade has started: the record names release ${from.release} until the second action has succeeded, and nothing goes back to it.` : ` Apply this release does not upgrade: it runs with the installed release ${from.release}.`}
          </p>
          <ol aria-label="The two actions of the Kubernetes upgrade" className="grid gap-2">{parts.map((p, i) => partRow(p, i + 1))}</ol>
          {units.length ? (
            <div className="overflow-x-auto">
              <table className="w-full text-left text-xs">
                <caption className="sr-only">The machines the upgrade replaces, in order, and what stops meanwhile</caption>
                <thead><tr className="border-b"><th scope="col" className="py-1 pr-3">Unit</th><th scope="col" className="pr-3">From</th><th scope="col" className="pr-3">To</th><th scope="col" className="pr-3">How</th><th scope="col">What stops meanwhile</th></tr></thead>
                <tbody>
                  {units.map((u, i) => (
                    <tr key={u.name} className="border-b align-top last:border-0">
                      <td className="py-1 pr-3 font-mono">{i + 1}. {u.name.slice(5)}</td>
                      <td className="pr-3 font-mono break-all">{u.from}</td>
                      <td className="pr-3 font-mono break-all">{u.to}</td>
                      <td className="pr-3">{deleteFirst(u.name) ? "old machine deleted first" : "one more machine first"}</td>
                      <td>{u.impact}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </>
      ) : upgrade.inProgress ? (
        <p>
          A run toward release <span className="font-mono">{to.release}</span> started and has not succeeded yet. Apply this release continues to it. Nothing goes back
          to release <span className="font-mono">{from.release}</span>, which the record names until a run has succeeded.
        </p>
      ) : replaces ? (
        <>
          <p>
            This ProxCloud carries the platform release <span className="font-mono">{to.release}</span>{offered ? " and can move the platform to it in place of" : ". It does not move the platform to it in place of"} the
            unfinished upgrade to release <span className="font-mono">{replaces}</span>{offered ? ": use it when that upgrade cannot finish." : "."} Apply this release and Retry continue to release <span className="font-mono">{replaces}</span>.
          </p>
          {offered ? null : <ul aria-label="Why the upgrade is refused" className="list-disc pl-5">{upgrade.blocking.map((b) => <li key={b}>{b}</li>)}</ul>}
        </>
      ) : offered ? (
        <p>
          This ProxCloud carries the platform release <span className="font-mono">{to.release}</span> and can move the platform to it. Tenant clusters and databases keep running.
          Apply this release does not upgrade: it runs with the installed release <span className="font-mono">{from.release}</span>.
        </p>
      ) : (
        <>
          <p>
            This ProxCloud carries the platform release <span className="font-mono">{to.release}</span> and does not move this platform to it. Apply this release runs with the
            installed release <span className="font-mono">{from.release}</span>, whose contents are listed above: it repairs what is missing, applies changed sizes and counts, and renews the credentials.
          </p>
          <ul aria-label="Why the upgrade is refused" className="list-disc pl-5">{upgrade.blocking.map((b) => <li key={b}>{b}</li>)}</ul>
        </>
      )}
      {changes.length ? (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs">
            <caption className="sr-only">What differs between release {from.release} and release {to.release}</caption>
            <thead><tr className="border-b"><th scope="col" className="py-1 pr-3">Entry</th><th scope="col" className="pr-3">From</th><th scope="col" className="pr-3">To</th><th scope="col" className="pr-3">Kind</th><th scope="col">What the run does</th></tr></thead>
            <tbody>
              {changes.map((ch) => (
                <tr key={(ch.cluster ?? "") + ch.name} className="border-b align-top last:border-0">
                  <td className="py-1 pr-3 font-mono">{ch.cluster ? `${ch.cluster === "seed" ? "seed" : "platform cluster"}: ` : ""}{ch.name}</td>
                  <td className="pr-3 font-mono break-all">{ch.from || "–"}</td>
                  <td className="pr-3 font-mono break-all">{ch.to || "–"}</td>
                  <td className="pr-3">{ch.class}{ch.stage ? ` (stage ${ch.stage}${ch.wave !== undefined ? `, wave ${ch.wave}` : ""})` : ""}</td>
                  <td>{ch.blocking ?? ch.impact}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {(offered || twoPart) && upgrade.seedBackup ? (
        <div className="grid gap-2">
          <p><span className="font-semibold">Seed backup{upgrade.seedBackup.status === "ok" ? "" : " (needed before this upgrade)"}:</span> {upgrade.seedBackup.message}</p>
          {needsBackup ? (
            <div className="flex items-start gap-2">
              <Checkbox id={id + "-backup"} checked={backupTaken} disabled={!canManage} onCheckedChange={(v) => setBackupTaken(v === true)} className="mt-0.5" />
              <Label htmlFor={id + "-backup"} className="leading-snug font-normal">I checked in Proxmox: seed VM {installation.status?.seed?.vmid} has a backup made after it was created and younger than 24 hours</Label>
            </div>
          ) : null}
        </div>
      ) : null}
      {offered && !twoPart ? (
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="outline" disabled={!canManage || pending || dirty || (needsBackup && !backupTaken)} onClick={() => { setTyped(""); setAsking({ part: "", confirm: upgrade.confirm ?? "" }) }}>Upgrade to release {to.release}</Button>
          <Link to="/docs/platform-admin/managed-services" className={LINK}>What an upgrade does</Link>
        </div>
      ) : null}
      {twoPart ? <Link to="/docs/platform-admin/managed-services" className={LINK}>What a Kubernetes upgrade does, and what it cannot undo</Link> : null}
      <Dialog open={!!asking} onOpenChange={(o) => !o && setAsking(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Upgrade “{installation.name}” to release {to.release}</DialogTitle>
            <DialogDescription>
              {twoPart && asking?.part === "" && noWayBack ? noWayBack + " " : ""}
              The run applies the changes of the plan in the order of its stages and waits after each step until the platform's components and the tenants that
              were healthy before are healthy again. If it fails, Retry continues to release {to.release}: the platform does not go back to release {from.release}.
              Take a backup of the seed VM and a dump of the portal database first; the guide's section “Credentials and the seed backup” has the commands.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-1.5">
            <Label htmlFor={id + "-confirm"}>Type <span className="font-mono">{asking?.confirm}</span> to confirm</Label>
            <Input id={id + "-confirm"} className="font-mono" autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAsking(null)}>Not now</Button>
            <Button disabled={!asking || typed !== asking.confirm || pending} onClick={() => { const a = asking!; setAsking(null); start(a.part, typed) }}>{pending ? "Starting…" : "Upgrade"}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  )
}

export function ReviewStep({ installation, canManage, dirty, onInstall, onUpgrade, installing, error }: {
  installation: Installation; canManage: boolean; dirty: boolean; onInstall: () => void; onUpgrade: UpgradeStart; installing: boolean; error: unknown
}) {
  // Keyed on the last run too: a run changes what is installed, and the API leaves `rev` alone then.
  const plan = useQuery({ queryKey: [...PLATFORM_KEY, "plan", installation.id, installation.rev, installation.lastRunId], queryFn: () => apiFetch<Plan>(`${base(installation.id)}/plan`), retry: false })
  // The contents an apply runs with (the plan's target): the installed release for a platform that stays on an earlier
  // one, else this ProxCloud's own.
  const target = plan.data?.target.sha256 ?? ""
  const bom = useQuery({ queryKey: ["platform-bom", target], queryFn: () => apiFetch<BOM>(`/admin/platform/bom${target ? `?sha256=${target}` : ""}`), enabled: !plan.isPending, retry: false, staleTime: 300_000 })
  const s = installation.spec
  const c = s.clusters[0]
  const workers = c.nodeGroups.reduce((n, g) => n + g.count, 0)
  const controlPlanes = c.controlPlane.replicas ?? 1
  // Cores and memory per Proxmox node: the seed, the control plane machines, and each group's machines spread over
  // its nodes; and the same with the spare machines of a rolling update.
  const load = plannedMemory(specToForm(installation.name, s))
  const siteNodes = useSiteNodes(installation.serviceId)
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
          <li>{controlPlanes === 1 ? "1 control plane machine" : `${controlPlanes} control plane machines on ${c.controlPlane.node}`} and {workers} worker machines in the pool <code>proxcloud-system</code>;</li>
          <li>the providers “{installation.name} Kubernetes” and “{installation.name} Databases”, held back from new projects until you attach them.</li>
        </ul>
      </div>
      <div className="grid gap-1 text-sm">
        <h3 className="font-semibold">What it takes on each node</h3>
        <ul aria-label="Cores and memory per node" className="list-disc pl-5">
          {Object.entries(load).sort().map(([node, l]) => {
            const at = siteNodes.find((n) => n.node === node)
            const read = !!at && at.maxMiB > 0
            return (
              <li key={node}>
                <span className="font-mono">{node}</span>: {l.cores} cores and {gib(l.run)} of memory; {gib(l.need)} with the one more machine an update of the control plane makes, {gib(l.spare)} with one more of each group as well.{" "}
                {!read ? "The node's memory was not read." : installed ? `The node has ${gib(at.maxMiB)}, ${gib(at.freeMiB)} of it free now, with this platform's machines running.`
                  : `The node has ${gib(at.freeMiB)} free of ${gib(at.maxMiB)}.`}
                {read && !installed && l.need > at.freeMiB ? <span className="font-medium text-destructive"> That is not enough.</span>
                  : read && !installed && l.spare > at.freeMiB ? " That holds the install; a later change that replaces a group's machines needs the larger figure." : null}
              </li>
            )
          })}
        </ul>
        <p className="text-muted-foreground">
          Free is what Proxmox reports as not in use now. The Check's row <span className="font-mono">capacity</span> decides: it counts the configured memory of every VM
          and container on the node, running or not, as the machines are counted when they are cloned. A rolling update makes one spare machine for the control plane and one for
          each group on each of its nodes before it removes the old ones. The row fails without room for the control plane's, and warns without room for the groups'.
        </p>
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
                  {ch.impact ? <span className="block text-muted-foreground">{ch.impact}</span> : null}
                </li>
              ))}
            </ul>
          ) : <p className="text-muted-foreground">No component changes version.</p>}
        </div>
      ) : null}
      {plan.data?.upgrade ? (
        <UpgradePlan installation={installation} upgrade={plan.data.upgrade} canManage={canManage} dirty={dirty} pending={installing} onUpgrade={onUpgrade} />
      ) : null}
      {plan.data?.upgrade && plan.data.replacement ? (
        <UpgradePlan installation={installation} upgrade={plan.data.replacement} replaces={plan.data.upgrade.to.release} canManage={canManage} dirty={dirty} pending={installing} onUpgrade={onUpgrade} />
      ) : null}
      {blocking.length ? (
        <Alert variant="destructive">
          <AlertTitle>This release cannot be applied to what is installed</AlertTitle>
          <AlertDescription><ul className="list-disc pl-5">{blocking.map((b) => <li key={b}>{b}</li>)}</ul></AlertDescription>
        </Alert>
      ) : null}
      {errCode(error) === "bom_installed" ? ( // the upgrade finished after this page read the plan (another session's run)
        <Alert>
          <AlertTitle>The platform is already on this release</AlertTitle>
          <AlertDescription>There is nothing to upgrade, and no run was started.</AlertDescription>
        </Alert>
      ) : <ErrorNote title="The run did not start" error={error} />}
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
