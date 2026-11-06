import { useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Trash2 } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { StatusBadge } from "@/components/status-badge"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { useAdminGet } from "@/lib/hooks"
import type { CloudProvider } from "./CloudProvidersPage"
import {
  KamajiProviderForm,
  kamajiConfigBlocks,
  kamajiFormFromService,
  kamajiFormValid,
  type KamajiFormState,
} from "./kamajiProvider"
import {
  ProxmoxProviderForm,
  proxmoxNetworkConfig,
  proxmoxFormFromService,
  proxmoxFormValid,
  type ProxmoxFormState,
} from "./proxmoxProvider"
import {
  DbaasProviderForm,
  dbaasConfigBlocks,
  dbaasFormFromService,
  dbaasFormValid,
  type DbaasFormState,
} from "./dbaasProvider"

// ── helpers ─────────────────────────────────────────────────────────────────
type Obj = Record<string, any>
const asObj = (v: unknown): Obj => (v && typeof v === "object" && !Array.isArray(v) ? (v as Obj) : {})
const str = (v: unknown) => (typeof v === "string" ? v : "")
const errMsg = (e: unknown) => (e instanceof Error ? e.message : String(e))

// The console types the PVE console endpoint serves (VNC ticket + serial terminal).
// Proxmox providers: the keys the PVE create paths enforce (externalservice.PVEQuotaKeys), seeded onto
// each new project; "gpu" edits quota.gpu["*"] (any model).
const PVE_QUOTA_KEYS = ["servers", "vcpus", "ramMb", "diskGb", "networks", "floatingIps", "gpu"]

// useEsSave — the shared PUT/DELETE mutation for every provider tab. On success it invalidates the
// provider doc (so all tabs re-read the merged config) plus any per-tab live query for this provider.
function useEsSave(id: string, onDone?: () => void) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { path: string; body?: unknown; method?: string }) =>
      apiFetch(v.path, { method: v.method ?? "PUT", body: v.body }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin-get", `/admin/service/${id}`] })
      qc.invalidateQueries({ queryKey: ["es-live", id] })
      toast.success("Saved")
      onDone?.()
    },
    onError: (e) => toast.error(errMsg(e)),
  })
}

function Note({ children }: { children: React.ReactNode }) {
  return <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">{children}</div>
}
function Row({ label, value }: { label: string; value?: string }) {
  return (
    <div className="flex items-center justify-between gap-4 border-b py-2 text-sm last:border-b-0">
      <span className="text-muted-foreground">{label}</span>
      <span className="font-mono text-xs">{value || "—"}</span>
    </div>
  )
}

type TabProps = { id: string; provider: CloudProvider }

// ── 1. Connection (per-provider connection form) ─────────────────────────────
// KamajiConnectionTab edits a Managed Kubernetes provider in place. PUT /admin/service/{id}
// replaces top-level config keys wholesale and leaves the stored secret alone when the body omits
// one — so `argocd` and `cluster` are always sent complete, and `secret` only when the operator
// pastes a new kubeconfig.
function KamajiConnectionTab({ id, provider }: TabProps) {
  const [form, setForm] = useState<KamajiFormState>(() => kamajiFormFromService(provider))
  const save = useEsSave(id)
  const submit = () => {
    const blocks = kamajiConfigBlocks(form)
    save.mutate({
      path: `/admin/service/${id}`,
      body: {
        name: form.name.trim(),
        config: blocks,
        ...(form.kubeconfig.trim() ? { secret: { kubeconfig: form.kubeconfig.trim() } } : {}),
      },
    })
  }
  return (
    <>
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Managed Kubernetes (Kamaji) connection</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        <KamajiProviderForm form={form} setForm={setForm} mode="edit" />
        <Note>
          Changing the chart version here only affects clusters created from now on — an existing
          cluster keeps the version it was pinned to until it is explicitly moved. Removing a
          Kubernetes version stops it being offered; clusters already on it keep running.
        </Note>
        <div className="flex justify-end">
          <Button onClick={submit} disabled={!kamajiFormValid(form, false) || save.isPending}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </div>
      </CardContent>
    </Card>
    <KamajiClustersCard id={id} />
    </>
  )
}

// DbaasConnectionTab edits a Managed Database provider in place — same wholesale-config,
// blank-kubeconfig-keeps semantics as the kamaji tab above.
function DbaasConnectionTab({ id, provider }: TabProps) {
  const [form, setForm] = useState<DbaasFormState>(() => dbaasFormFromService(provider))
  const save = useEsSave(id)
  const submit = () => {
    save.mutate({
      path: `/admin/service/${id}`,
      body: {
        name: form.name.trim(),
        config: dbaasConfigBlocks(form),
        ...(form.kubeconfig.trim() ? { secret: { kubeconfig: form.kubeconfig.trim() } } : {}),
      },
    })
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Managed Databases (DBaaS) connection</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        <DbaasProviderForm form={form} setForm={setForm} mode="edit" />
        <Note>
          Changing the chart version here only affects databases created from now on — an existing
          database keeps the version it was pinned to. Removing an engine or version stops it being
          offered; databases already on it keep running.
        </Note>
        <div className="flex justify-end">
          <Button onClick={submit} disabled={!dbaasFormValid(form, false) || save.isPending}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

// ProxmoxConfigCard — the provider's resource-shaping config blocks (network zones,
// storage profiles, template catalog, GPU mappings) as validated raw-JSON editors.
// A dedicated form per block is an upgrade path; the JSON editor is complete and
// honest today (the doc shapes are documented in externalservice/proxmox.go).
function ProxmoxConfigCard({ id, provider }: TabProps) {
  const cfg = asObj(provider.config)
  const blocks: { key: string; label: string; hint: string }[] = [
    { key: "network", label: "Network", hint: '{"defaultZone":"overlay","zones":{"overlay":{"kind":"evpn","peers":["10.0.0.1"],"exitNodes":["pve1"]}},"cidrPool":"10.100.0.0/16","isolation":"shared","publicNetworks":[{"id":"vmbr1","name":"Internet","cidr":"203.0.113.0/24","gateway":"203.0.113.1","ranges":["203.0.113.10-203.0.113.200"]}]}' },
    { key: "storageProfiles", label: "Storage profiles", hint: '{"std":{"label":"Local NVMe (ZFS)","storages":{"*":"local-zfs"},"snapshots":true,"thin":true,"replication":true},"capacity":{"label":"Capacity HDD (NFS)","storages":{"*":"nas-capacity"},"shared":true,"liveMigrate":true}}' },
    { key: "templates", label: "OS templates (optional: tagged templates are discovered; vmid + node pins one, the rest overlays)", hint: '{"ubuntu-24.04":{"name":"Ubuntu 24.04","minDiskGb":10}}' },
    { key: "gpus", label: "GPU mappings", hint: '{"<pci-mapping-id>":{"alias":"nvidia-4090"}}' },
  ]
  const [drafts, setDrafts] = useState<Record<string, string>>(() => {
    const out: Record<string, string> = {}
    for (const b of blocks) out[b.key] = JSON.stringify(cfg[b.key] ?? {}, null, 2)
    return out
  })
  const save = useEsSave(id)
  const submit = (key: string) => {
    let parsed: unknown
    try {
      parsed = JSON.parse(drafts[key] || "{}")
    } catch {
      toast.error("Not valid JSON")
      return
    }
    save.mutate({ path: `/admin/service/${id}`, body: { config: { [key]: parsed } } })
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Proxmox resources</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-6">
        {blocks.map((b) => (
          <div key={b.key} className="grid gap-2">
            <Label className="font-medium">{b.label} <span className="font-mono text-xs text-muted-foreground">config.{b.key}</span></Label>
            <Textarea
              className="min-h-28 font-mono text-xs"
              value={drafts[b.key]}
              onChange={(e) => setDrafts({ ...drafts, [b.key]: e.target.value })}
            />
            {b.key === "storageProfiles" ? <CapacityProfileNotes cfg={cfg} /> : null}
            <div className="flex items-center justify-between">
              <p className="truncate pr-4 font-mono text-[10px] text-muted-foreground" title={b.hint}>{b.hint}</p>
              <Button size="sm" variant="outline" onClick={() => submit(b.key)} disabled={save.isPending}>Save {b.label.toLowerCase()}</Button>
            </div>
          </div>
        ))}
      </CardContent>
    </Card>
  )
}

// CapacityProfileNotes flags the saved storage profiles whose storages are capacity class (the
// lowest class of a profile's storages wins, the Storage tab override over the detected class):
// the API refuses boot disks, databases and snapshots on them (plan S18).
function CapacityProfileNotes({ cfg }: { cfg: Record<string, any> }) {
  const detected = asObj(asObj(cfg.capabilities).storages) as Record<string, { class?: string }>
  const overrides = asObj(cfg.storageClasses) as Record<string, { class?: string }>
  const classOf = (s: string) => overrides[s]?.class || detected[s]?.class
  const capacity = Object.entries(asObj(cfg.storageProfiles) as Record<string, { label?: string; storages?: Record<string, string> }>)
    .filter(([, p]) => Object.values(p.storages ?? {}).some((s) => classOf(s) === "capacity"))
    .map(([slug, p]) => (p.label ? `${p.label} (${slug})` : slug))
  if (capacity.length === 0) return null
  return (
    <p className="text-xs text-amber-600 dark:text-amber-400">
      HDD — not for databases: {capacity.join(", ")}. Capacity-class profiles never take boot disks, managed databases
      or snapshots; tenants see this warning when they pick them. Set an optional "label", and "bootDisk": false or
      "dbaas": false to close other profiles too.
    </p>
  )
}

// ProxmoxConnectionTab edits a Proxmox VE provider in place. The update route overlays
// top-level config keys, so only the connection-owned keys are sent — the storageProfiles /
// templates / gpus blocks (their own tabs, later phases) survive a Save. config.network is
// sent whole: the stored block with the form's isolation keys merged in.
function ProxmoxConnectionTab({ id, provider }: TabProps) {
  const [form, setForm] = useState<ProxmoxFormState>(() => proxmoxFormFromService(provider))
  const save = useEsSave(id)
  const submit = () => {
    save.mutate({
      path: `/admin/service/${id}`,
      body: {
        name: form.name.trim(),
        config: {
          apiUrl: form.apiUrl.trim().replace(/\/+$/, ""),
          tokenId: form.tokenId.trim(),
          fingerprint: form.fingerprint.trim(),
          insecureTls: form.insecureTls,
          poolPrefix: form.poolPrefix.trim() || "pc-",
          // Merge into the CURRENT stored network (not the mount-time copy) so a Network-JSON
          // save made since this tab mounted is not reverted.
          network: proxmoxNetworkConfig({ ...form, storedConfig: asObj(provider.config) }),
        },
        ...(form.tokenSecret.trim() ? { secret: { tokenSecret: form.tokenSecret.trim() } } : {}),
      },
    })
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Proxmox VE connection</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        <ProxmoxProviderForm form={form} setForm={setForm} serviceId={id} mode="edit" />
        <div className="flex justify-end">
          <Button onClick={submit} disabled={!proxmoxFormValid(form, false) || save.isPending}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

// KamajiClustersCard — every proxcloud-managed cluster's pinned chart version, with explicit
// re-pin controls. Clusters keep their pin when the provider's version moves (by design); this
// card is the operator override, one cluster or all at once.
function KamajiClustersCard({ id }: { id: string }) {
  const qc = useQueryClient()
  const q = useAdminGet<{
    providerChartVersion?: string
    clusters?: { id: string; name?: string; projectId?: string; chartVersion?: string }[]
  }>(`/admin/service/${id}/k8s-clusters`)
  const pin = q.data?.providerChartVersion ?? ""
  const clusters = q.data?.clusters ?? []
  const behind = clusters.filter((c) => pin && c.chartVersion !== pin)
  const invalidate = () => void qc.invalidateQueries({ queryKey: ["admin-get", `/admin/service/${id}/k8s-clusters`] })

  const bumpOne = useMutation({
    mutationFn: (clusterId: string) =>
      apiFetch(`/admin/service/${id}/k8s-clusters/${clusterId}/bump-chart`, { method: "POST" }),
    onSuccess: () => {
      toast.success(`Cluster re-pinned to ${pin}`)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const bumpAll = useMutation({
    mutationFn: () =>
      apiFetch<{ bumped?: number; errors?: Record<string, string> }>(
        `/admin/service/${id}/k8s-clusters/bump-chart`, { method: "POST" },
      ),
    onSuccess: (d) => {
      const failed = Object.keys(d?.errors ?? {}).length
      if (failed) toast.warning(`${d?.bumped ?? 0} re-pinned, ${failed} failed`)
      else toast.success(`${d?.bumped ?? 0} cluster(s) re-pinned to ${pin}`)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between space-y-0">
        <CardTitle className="text-eyebrow">Managed clusters — platform (chart) versions</CardTitle>
        <Button
          size="sm"
          variant="outline"
          onClick={() => bumpAll.mutate()}
          disabled={!pin || behind.length === 0 || bumpAll.isPending}
        >
          {bumpAll.isPending ? "Bumping…" : `Bump all to ${pin || "—"}`}
        </Button>
      </CardHeader>
      <CardContent className="grid gap-3">
        <p className="text-sm text-muted-foreground">
          Provider pin: <span className="font-mono">{pin || "—"}</span>. Re-pinning re-renders the
          chart for that cluster — platform components restart with a rolling update, and when the
          node template changed between chart versions the nodes rotate too (surge-first).
          Customers can also apply this themselves from the cluster page.
        </p>
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>Cluster</TableHead>
              <TableHead>Project</TableHead>
              <TableHead>Chart version</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {q.isLoading ? (
              <TableRow><TableCell colSpan={4} className="text-center text-sm text-muted-foreground">Loading…</TableCell></TableRow>
            ) : clusters.length === 0 ? (
              <TableRow><TableCell colSpan={4} className="text-center text-sm text-muted-foreground">No managed clusters.</TableCell></TableRow>
            ) : (
              clusters.map((c) => (
                <TableRow key={c.id}>
                  <TableCell>
                    <span className="font-medium">{c.name || c.id}</span>{" "}
                    <span className="font-mono text-xs text-muted-foreground">{c.id}</span>
                  </TableCell>
                  <TableCell className="font-mono text-xs">{c.projectId || "—"}</TableCell>
                  <TableCell className="font-mono text-sm">
                    {c.chartVersion || "—"}
                    {pin && c.chartVersion === pin ? <span className="ml-2 text-xs text-muted-foreground">current</span> : null}
                  </TableCell>
                  <TableCell className="text-right">
                    {pin && c.chartVersion !== pin ? (
                      <Button size="sm" variant="outline" onClick={() => bumpOne.mutate(c.id)} disabled={bumpOne.isPending}>
                        Bump to {pin}
                      </Button>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  )
}

function ConnectionTab({ id, provider }: TabProps) {
  // kamaji: the delivery plane (chart pin, ArgoCD AppProject, DNS zone, version→image matrix) is
  // config an operator keeps current — so this tab is the editable form, not a read-only card.
  if (provider.config?.provider === "kamaji") {
    return <KamajiConnectionTab id={id} provider={provider} />
  }
  // dbaas: same delivery-config-heavy shape as kamaji — the tab IS the editable form.
  if (provider.config?.provider === "dbaas") {
    return <DbaasConnectionTab id={id} provider={provider} />
  }
  // proxmox: the editable connection form with its own Test button, plus the
  // resource-config blocks (zones/storage/templates/GPUs).
  if (provider.config?.provider === "proxmox") {
    return (
      <div className="grid gap-6">
        <ProxmoxConnectionTab id={id} provider={provider} />
        <ProxmoxConfigCard id={id} provider={provider} />
      </div>
    )
  }
  // ceph-s3: show the S3/Admin Ops config. There is no backend test endpoint for RGW (the
  // connection is exercised by bootstrap/sync), so no Test button.
  if (provider.config?.provider === "ceph-s3" || provider.config?.provider === "rustfs") {
    const c = asObj(provider.config)
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-eyebrow">{provider.config?.provider === "rustfs" ? "RustFS (S3) connection" : "Ceph RGW (S3) connection"}</CardTitle>
        </CardHeader>
        <CardContent>
          <Row label="S3 endpoint" value={str(c.s3Endpoint)} />
          <Row label="Admin URL" value={str(c.adminApiUrl)} />
          <Row label="Website endpoint" value={str(c.s3WebsiteEndpoint)} />
          <Row label="Region (zonegroup)" value={str(c.region)} />
          <Row label="RGW uid prefix" value={str(c.uidPrefix)} />
          <Row label="Default quota (GiB)" value={c.defaultQuotaGiB != null ? String(c.defaultQuotaGiB) : ""} />
          <p className="mt-4 text-sm text-muted-foreground">
            Projects are provisioned as dedicated object-store users on entry (or via the project's "attach external
            service" action). The admin keys are stored encrypted and never returned by the API.
          </p>
        </CardContent>
      </Card>
    )
  }
  return <Note>Unknown provider type "{str(provider.config?.provider)}" — no connection settings to show.</Note>
}

// ── 2. Services (per-region service toggle grid; Save sends the FULL map) ─────
type ServicesMap = Record<string, Record<string, boolean>>
function ServicesTab({ id, provider }: TabProps) {
  const stored: ServicesMap = (provider.config?.services as ServicesMap) ?? {}
  const [edits, setEdits] = useState<ServicesMap | null>(null)
  const services = edits ?? stored
  const save = useEsSave(id, () => setEdits(null))

  const regionSet = new Set<string>(Object.keys(provider.config?.regions ?? {}))
  for (const perRegion of Object.values(services)) for (const r of Object.keys(perRegion ?? {})) regionSet.add(r)
  const regions = [...regionSet].sort()

  const toggle = (svc: string, region: string, on: boolean) => {
    const next: ServicesMap = Object.fromEntries(Object.entries(services).map(([k, v]) => [k, { ...(v ?? {}) }]))
    next[svc] = { ...(next[svc] ?? {}), [region]: on }
    setEdits(next)
  }
  return (
    <div className="space-y-4">
      {Object.keys(services).length === 0 ? (
        <Note>No services configured on this provider.</Note>
      ) : (
        <Card className="overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Service</TableHead>
                {regions.map((r) => (
                  <TableHead key={r}>{r}</TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {Object.keys(services)
                .sort()
                .map((svc) => (
                  <TableRow key={svc}>
                    <TableCell className="font-mono text-xs">{svc}</TableCell>
                    {regions.map((r) => (
                      <TableCell key={r}>
                        <Switch
                          checked={services[svc]?.[r] === true}
                          onCheckedChange={(on) => toggle(svc, r, on)}
                          aria-label={`${svc} in ${r}`}
                        />
                      </TableCell>
                    ))}
                  </TableRow>
                ))}
            </TableBody>
          </Table>
        </Card>
      )}
      <div className="flex items-center gap-3">
        <Button
          // PUT /service/{id} merges config.services wholesale → must send the FULL map.
          onClick={() => edits && save.mutate({ path: `/admin/service/${id}?provisioning=false`, body: { config: { services: edits } } })}
          disabled={!edits || save.isPending}
        >
          {save.isPending ? "Saving…" : "Save services"}
        </Button>
        {edits ? <span className="text-sm text-muted-foreground">Unsaved changes</span> : null}
      </div>
    </div>
  )
}

// ── 3. Configuration (name + status; /configuration only reads these) ─────────
function ConfigurationTab({ id, provider }: TabProps) {
  const [name, setName] = useState(provider.name ?? "")
  const [status, setStatus] = useState(provider.status ?? "PUBLIC")
  const save = useEsSave(id)
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">General configuration</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid max-w-md gap-2">
          <Label htmlFor="es-name">Display name</Label>
          <Input id="es-name" value={name} onChange={(e) => setName(e.target.value)} />
        </div>
        <div className="grid max-w-md gap-2">
          <Label htmlFor="es-status">Status</Label>
          <Select value={status} onValueChange={setStatus}>
            <SelectTrigger id="es-status" className="w-full sm:w-56">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="PUBLIC">Public</SelectItem>
              <SelectItem value="PRIVATE">Private</SelectItem>
              <SelectItem value="DISABLED">Disabled</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <Button
          // /configuration sets name and status only (the rest of the stored doc is preserved).
          onClick={() => save.mutate({ path: `/admin/service/${id}/configuration`, body: { name, status } })}
          disabled={save.isPending}
        >
          {save.isPending ? "Saving…" : "Save configuration"}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── 6. Quota (config.provisioning.quota) ─────────────────────────────────────
function QuotaTab({ id, provider }: TabProps) {
  const quota = asObj(asObj((provider.config as Obj)?.provisioning).quota)
  const keys = PVE_QUOTA_KEYS
  // "gpu" is the any-model limit quota.gpu["*"]; every other key is a flat number.
  const read = (k: string) => (k === "gpu" ? asObj(quota.gpu)["*"] : quota[k])
  const [vals, setVals] = useState<Record<string, string>>(() => {
    const seed: Record<string, string> = {}
    for (const k of keys) seed[k] = read(k) != null ? String(read(k)) : ""
    return seed
  })
  const save = useEsSave(id)
  const submit = () => {
    // /quota replaces config.provisioning.quota wholesale → merge over the stored quota (keeps per-model gpu limits etc).
    // A blank field removes the key (unlimited).
    const next: Obj = { ...quota }
    for (const k of keys) {
      const v = vals[k].trim()
      let parsed: number | undefined
      if (v !== "") {
        parsed = Number(v)
        // NaN is dropped by JSON serialization — the key would silently keep its old value.
        if (!Number.isFinite(parsed) || parsed < 0 || !Number.isInteger(parsed)) {
          toast.error(`Invalid quota value for "${k}"`)
          return
        }
      }
      if (k === "gpu") {
        const gpu: Obj = { ...asObj(quota.gpu) }
        if (parsed === undefined) delete gpu["*"]
        else gpu["*"] = parsed
        if (Object.keys(gpu).length) next.gpu = gpu
        else delete next.gpu
      } else if (parsed === undefined) {
        delete next[k]
      } else {
        next[k] = parsed
      }
    }
    save.mutate({ path: `/admin/service/${id}/quota`, body: next })
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Default quota</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-sm text-muted-foreground">
          Copied onto each new project and enforced for projects without their own quota. Blank = unlimited;
          ramMb in MiB, diskGb in GiB, gpu = devices of any model.
        </p>
        <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
          {keys.map((k) => (
            <div key={k} className="grid gap-1">
              <Label className="text-xs" htmlFor={`q-${k}`}>
                {k}
              </Label>
              <Input
                id={`q-${k}`}
                type="number"
                value={vals[k]}
                onChange={(e) => setVals((s) => ({ ...s, [k]: e.target.value }))}
              />
            </div>
          ))}
        </div>
        <Button onClick={submit} disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Save quota"}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── 6b. GPU (PVE resource-mapping capacity + unpriced-flavor guard) ─────────────────────
type GpuRegionCapacity = { region: string; gpus: Array<{ name: string; total: number; inUse: number }> }
// ── Storage (detected capabilities + per-storage class override) ─────────────
// config.capabilities is written by detection (services sync / Re-detect / profile save);
// config.storageClasses.<id> holds the admin override (class, wins) and the installer's fio
// suggestion (suggestedClass, shown only — never applied on its own).
type StorageCap = {
  type?: string
  tier?: string
  class?: string
  content?: string[]
  nodes?: Record<string, { active?: boolean }>
} & Partial<Record<(typeof CAP_FLAGS)[number][0], boolean>>
const CAP_FLAGS = [
  ["shared", "shared"],
  ["snapshots", "snapshots"],
  ["thin", "thin"],
  ["liveMigrate", "live migrate"],
  ["replication", "replication"],
] as const
const STORAGE_CLASSES = ["performance", "standard", "capacity"]

function StorageTab({ id, provider }: TabProps) {
  const qc = useQueryClient()
  const cfg = asObj(provider.config)
  const caps = asObj(cfg.capabilities)
  const storages = asObj(caps.storages) as Record<string, StorageCap>
  const classes = asObj(cfg.storageClasses) as Record<string, { class?: string; suggestedClass?: string }>
  const save = useEsSave(id)
  const detect = useMutation({
    mutationFn: () => apiFetch(`/admin/service/${id}/storage/detect`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Storage capabilities re-detected")
      void qc.invalidateQueries({ queryKey: ["admin-get", `/admin/service/${id}`] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const setClass = (storage: string, v: string) =>
    save.mutate({ path: `/admin/service/${id}/storage-classes`, body: { [storage]: v === "auto" ? "" : v } })
  const ids = Object.keys(storages).sort()
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between space-y-0">
        <CardTitle className="text-eyebrow">Storage capabilities</CardTitle>
        <Button size="sm" variant="outline" onClick={() => detect.mutate()} disabled={detect.isPending}>
          {detect.isPending ? "Detecting…" : "Re-detect capabilities"}
        </Button>
      </CardHeader>
      <CardContent className="grid gap-3">
        <p className="text-sm text-muted-foreground">
          Detected from the cluster{str(caps.detectedAt) ? ` at ${new Date(str(caps.detectedAt)).toLocaleString()}` : ""}; re-run
          every sync cycle. Storage profiles may only claim flags their storage has. The class override wins over the
          detected class; the installer benchmark only suggests one.
        </p>
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Storage</TableHead>
                <TableHead>Tier</TableHead>
                <TableHead>Capabilities</TableHead>
                <TableHead>Active nodes</TableHead>
                <TableHead>Detected</TableHead>
                <TableHead>Suggested</TableHead>
                <TableHead>Class</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {ids.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={7} className="text-center text-sm text-muted-foreground">
                    Not detected yet. Use Re-detect capabilities.
                  </TableCell>
                </TableRow>
              ) : (
                ids.map((s) => {
                  const c = storages[s]
                  const active = Object.entries(c.nodes ?? {}).filter(([, n]) => n.active).map(([n]) => n).sort()
                  return (
                    <TableRow key={s}>
                      <TableCell>
                        <span className="font-medium">{s}</span>{" "}
                        <span className="font-mono text-xs text-muted-foreground">{c.type}</span>
                        <div className="font-mono text-[10px] text-muted-foreground">{(c.content ?? []).join(", ")}</div>
                      </TableCell>
                      <TableCell className="text-sm">{c.tier || "—"}</TableCell>
                      <TableCell>
                        <div className="flex flex-wrap gap-1">
                          {CAP_FLAGS.filter(([k]) => c[k]).map(([k, label]) => (
                            <Badge key={k} variant="secondary">{label}</Badge>
                          ))}
                        </div>
                      </TableCell>
                      <TableCell className="font-mono text-xs">{active.length ? active.join(", ") : "none"}</TableCell>
                      <TableCell className="text-sm">{c.class || "—"}</TableCell>
                      <TableCell className="text-sm">{classes[s]?.suggestedClass || "—"}</TableCell>
                      <TableCell>
                        <Select value={classes[s]?.class || "auto"} onValueChange={(v) => setClass(s, v)} disabled={save.isPending}>
                          <SelectTrigger className="h-8 w-40" aria-label={`Class for ${s}`}>
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            <SelectItem value="auto">Detected ({c.class || "—"})</SelectItem>
                            {STORAGE_CLASSES.map((k) => (
                              <SelectItem key={k} value={k}>{k}</SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      </TableCell>
                    </TableRow>
                  )
                })
              )}
            </TableBody>
          </Table>
        </div>
      </CardContent>
    </Card>
  )
}

type UnpricedFlavor = { region: string; id: string; name: string; gpuModel?: string; gpuCount?: number; reason?: string }

function GpuTab({ id }: { id: string }) {
  const capQ = useAdminGet<GpuRegionCapacity[]>(`/admin/service/${id}/gpu-info`)
  const unpricedQ = useAdminGet<UnpricedFlavor[]>(`/admin/service/${id}/unpriced-flavors`)
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-eyebrow">GPU capacity</CardTitle>
        </CardHeader>
        <CardContent>
          {capQ.isLoading ? (
            <Skeleton className="h-24" />
          ) : (capQ.data ?? []).flatMap((r) => r.gpus).length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No GPU devices found (expects PVE PCI resource mappings declared in the provider's
              GPU mappings config).
            </p>
          ) : (
            <div className="space-y-6">
              {(capQ.data ?? []).map((r) => (
                <div key={r.region} className="space-y-2">
                  <p className="text-eyebrow">{r.region}</p>
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>GPU model</TableHead>
                        <TableHead className="text-right">In use</TableHead>
                        <TableHead className="text-right">Free</TableHead>
                        <TableHead className="text-right">Total</TableHead>
                        <TableHead className="w-48">Usage</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {r.gpus.map((g) => (
                        <TableRow key={g.name}>
                          <TableCell className="font-mono text-xs">{g.name}</TableCell>
                          <TableCell className="text-right tabular-nums">{g.inUse}</TableCell>
                          <TableCell className="text-right tabular-nums">{g.total - g.inUse}</TableCell>
                          <TableCell className="text-right tabular-nums">{g.total}</TableCell>
                          <TableCell>
                            <div className="h-2 w-full rounded bg-muted">
                              <div
                                className="h-2 rounded bg-primary"
                                style={{ width: `${g.total ? Math.round((g.inUse / g.total) * 100) : 0}%` }}
                              />
                            </div>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle className="text-eyebrow">Unpriced flavors</CardTitle>
        </CardHeader>
        <CardContent>
          {unpricedQ.isLoading ? (
            <Skeleton className="h-16" />
          ) : (unpricedQ.data ?? []).length === 0 ? (
            <p className="text-sm text-muted-foreground">Every live flavor matches at least one enabled public price rule.</p>
          ) : (
            <div className="space-y-3">
              <p className="text-sm text-destructive">
                These flavors have a pricing gap — "no rule" bills the whole server ZERO,
                "no gpu rule" bills the GPU devices ZERO (only CPU/RAM rules match).
              </p>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Flavor</TableHead>
                    <TableHead>Region</TableHead>
                    <TableHead>GPU</TableHead>
                    <TableHead>Gap</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(unpricedQ.data ?? []).map((f) => (
                    <TableRow key={`${f.region}-${f.id}`}>
                      <TableCell>{f.name}</TableCell>
                      <TableCell>{f.region}</TableCell>
                      <TableCell className="font-mono text-xs">
                        {f.gpuModel ? `${f.gpuModel} × ${f.gpuCount}` : "—"}
                      </TableCell>
                      <TableCell className="text-xs text-destructive">{f.reason ?? "no rule"}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

// ── 8. Advanced (delete provider) ───────────────────────────────────────────────
function AdvancedTab({ id }: { id: string }) {
  const navigate = useNavigate()
  const [confirm, setConfirm] = useState(false)
  const del = useMutation({
    mutationFn: () => apiFetch(`/admin/service/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Cloud provider deleted")
      navigate("/system/cloud-providers")
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  return (
    <div className="space-y-4">
      <Card className="border-destructive/40">
        <CardHeader>
          <CardTitle className="text-eyebrow text-destructive">Danger zone</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-sm text-muted-foreground">
            Delete this cloud provider. Fails if any project, user or cloud resource still uses it.
          </p>
          {confirm ? (
            <div className="flex items-center gap-3">
              <Button variant="destructive" onClick={() => del.mutate()} disabled={del.isPending}>
                <Trash2 className="size-4" />
                {del.isPending ? "Deleting…" : "Confirm delete"}
              </Button>
              <Button variant="outline" onClick={() => setConfirm(false)}>
                Cancel
              </Button>
            </div>
          ) : (
            <Button variant="destructive" onClick={() => setConfirm(true)}>
              <Trash2 className="size-4" />
              Delete cloud provider
            </Button>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

// ── 8b. Metrics (usage source for traffic billing: pve | prometheus | none) ──────────
type MetricsTestResp = { ok: boolean; error?: string; trafficSeries?: number; monthStartSeries?: number; warnings?: string[] }
function MetricsTab({ id, provider }: TabProps) {
  const stored = asObj(asObj((provider.config as Obj)?.metrics))
  const prom = asObj(stored.prometheus)
  const [source, setSource] = useState(String(stored.source ?? (asObj(provider.config as Obj).provider === "proxmox" ? "pve" : "none")))
  const [url, setUrl] = useState(String(prom.url ?? ""))
  const [schema, setSchema] = useState(String(prom.schema ?? "libvirt-exporter"))
  const [headersText, setHeadersText] = useState(JSON.stringify(asObj(prom.headers), null, 2))
  const [basicUser, setBasicUser] = useState(String(prom.basicUser ?? ""))
  const [basicPassword, setBasicPassword] = useState("")
  const [bearerToken, setBearerToken] = useState("")
  const [insecureTls, setInsecureTls] = useState(prom.insecureTls === true)
  const [caCert, setCaCert] = useState(String(prom.caCert ?? ""))
  const [timeoutSeconds, setTimeoutSeconds] = useState(String(prom.timeoutSeconds ?? ""))
  const save = useEsSave(id, () => {
    setBasicPassword("")
    setBearerToken("")
  })
  const test = useMutation({
    mutationFn: () => apiFetch<MetricsTestResp>(`/admin/service/${id}/metrics-test`, { method: "POST" }),
    onError: (e) => toast.error(errMsg(e)),
  })
  const submit = () => {
    const body: Obj = { source }
    // Only send the prometheus block when prometheus is the selected source: the backend
    // merges config, so a source-only toggle must not re-validate (or clobber) the stored
    // connection config — a stale URL would otherwise block switching away.
    if (source === "prometheus") {
      let headers: Obj = {}
      if (headersText.trim() !== "" && headersText.trim() !== "{}") {
        let parsed: unknown
        try {
          parsed = JSON.parse(headersText)
        } catch {
          toast.error("Headers must be a JSON object, e.g. {\"X-Scope-OrgID\": \"tenant\"}")
          return
        }
        if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
          toast.error("Headers must be a JSON object, e.g. {\"X-Scope-OrgID\": \"tenant\"}")
          return
        }
        for (const v of Object.values(parsed)) {
          if (typeof v !== "string") {
            toast.error("Header values must all be strings")
            return
          }
        }
        headers = parsed as Obj
      }
      const timeout = timeoutSeconds.trim() === "" ? 0 : Number(timeoutSeconds)
      if (!Number.isInteger(timeout) || timeout < 0) {
        toast.error("Timeout must be a whole number of seconds (0 = default)")
        return
      }
      body.prometheus = {
        url,
        schema,
        headers,
        basicUser,
        insecureTls,
        caCert,
        timeoutSeconds: timeout,
      }
    }
    // Credential fields are only visible for the prometheus source — never apply leftover
    // typed-but-hidden values on a none save (invisible side effects on secrets).
    if (source === "prometheus" && (basicPassword || bearerToken)) {
      body.prometheusAuth = { basicPassword, bearerToken }
    }
    save.mutate({ path: `/admin/service/${id}/metrics-config`, body })
  }
  const r = test.data
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-eyebrow">Usage metrics source</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Where the hourly traffic-billing job reads per-instance network usage from: a Prometheus-compatible
            endpoint (Prometheus, Mimir, VictoriaMetrics, Thanos), or &quot;None&quot; (the default) to disable usage
            ingestion for this provider.
          </p>
          <div className="grid max-w-xs gap-2">
            <Label htmlFor="mx-source">Source</Label>
            <Select value={source} onValueChange={setSource}>
              <SelectTrigger id="mx-source">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="pve">Proxmox NIC counters (public NICs, Proxmox default)</SelectItem>
                <SelectItem value="none">None (disable usage metrics)</SelectItem>
                <SelectItem value="prometheus">Prometheus-compatible</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {source === "prometheus" && (
            <>
              <div className="grid gap-2">
                <Label htmlFor="mx-url">Endpoint URL (base up to /api/v1)</Label>
                <Input
                  id="mx-url"
                  className="font-mono"
                  value={url}
                  onChange={(e) => setUrl(e.target.value)}
                  placeholder="https://mimir.example/prometheus"
                />
                <p className="text-xs text-muted-foreground">
                  Mimir: append <code>/prometheus</code>. VictoriaMetrics cluster:{" "}
                  <code>/select/&lt;tenant&gt;/prometheus</code>. Vanilla Prometheus/Thanos: plain host:port.
                </p>
              </div>
              <div className="grid max-w-md gap-2">
                <Label htmlFor="mx-schema">Metric schema</Label>
                <Select value={schema} onValueChange={setSchema}>
                  <SelectTrigger id="mx-schema">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {/* TODO(P5): prometheus-pve-exporter schema — libvirt-exporter is a legacy layout that
                        cannot match a PVE VM yet. */}
                    <SelectItem value="libvirt-exporter">libvirt-exporter (legacy layout — no Proxmox schema yet)</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="mx-headers">Extra headers (JSON object; e.g. Mimir tenant)</Label>
                <Textarea
                  id="mx-headers"
                  rows={3}
                  value={headersText}
                  onChange={(e) => setHeadersText(e.target.value)}
                  className="font-mono text-xs"
                  placeholder='{"X-Scope-OrgID": "tenant-a"}'
                />
              </div>
              <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
                <div className="grid gap-1">
                  <Label htmlFor="mx-user" className="text-xs">Basic auth user</Label>
                  <Input id="mx-user" value={basicUser} onChange={(e) => setBasicUser(e.target.value)} autoComplete="off" />
                </div>
                <div className="grid gap-1">
                  <Label htmlFor="mx-pass" className="text-xs">Basic auth password (blank keeps, &quot;-&quot; clears)</Label>
                  <Input
                    id="mx-pass"
                    type="password"
                    value={basicPassword}
                    onChange={(e) => setBasicPassword(e.target.value)}
                    autoComplete="off"
                  />
                </div>
                <div className="grid gap-1">
                  <Label htmlFor="mx-token" className="text-xs">Bearer token (blank keeps, &quot;-&quot; clears)</Label>
                  <Input
                    id="mx-token"
                    type="password"
                    value={bearerToken}
                    onChange={(e) => setBearerToken(e.target.value)}
                    autoComplete="off"
                  />
                </div>
              </div>
              <div className="flex items-center gap-3">
                <Switch id="mx-insecure" checked={insecureTls} onCheckedChange={setInsecureTls} />
                <Label htmlFor="mx-insecure">Skip TLS verification (insecure)</Label>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="mx-ca">Custom CA bundle (PEM, optional)</Label>
                <Textarea
                  id="mx-ca"
                  rows={3}
                  value={caCert}
                  onChange={(e) => setCaCert(e.target.value)}
                  className="font-mono text-xs"
                />
              </div>
              <div className="grid max-w-xs gap-2">
                <Label htmlFor="mx-timeout">Query timeout (seconds, 0 = default 30)</Label>
                <Input id="mx-timeout" type="number" value={timeoutSeconds} onChange={(e) => setTimeoutSeconds(e.target.value)} />
              </div>
            </>
          )}
          <div className="flex flex-wrap items-center gap-3">
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? "Saving…" : "Save metrics config"}
            </Button>
            {source === "prometheus" && (
              <Button
                variant="outline"
                onClick={() => test.mutate()}
                disabled={test.isPending || stored.source !== "prometheus"}
              >
                {test.isPending ? "Testing…" : "Test connection"}
              </Button>
            )}
            {source === "prometheus" && stored.source !== "prometheus" && (
              <span className="text-xs text-muted-foreground">Save the config first — the probe tests the stored settings.</span>
            )}
          </div>
          {r && (
            <div className="space-y-1 text-sm">
              {r.ok ? (
                <p>
                  Connected — {r.trafficSeries ?? 0} traffic series (last hour), {r.monthStartSeries ?? 0} at month
                  start.
                </p>
              ) : (
                <p className="text-destructive">Probe failed: {r.error}</p>
              )}
              {(r.warnings ?? []).map((w) => (
                <p key={w} className="text-destructive">
                  ⚠ {w}
                </p>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

// ── page ─────────────────────────────────────────────────────────────────────
const TAB_DEFS = [
  { v: "connection", label: "Connection" },
  { v: "storage", label: "Storage" },
  { v: "services", label: "Services" },
  { v: "configuration", label: "Configuration" },
  { v: "quota", label: "Quota" },
  { v: "gpu", label: "GPU" },
  { v: "metrics", label: "Metrics" },
  { v: "advanced", label: "Advanced" },
]

// A Proxmox VE provider gets every tab (network/storage-profile/template/GPU config blocks live on
// Connection; Storage shows the detected capabilities).
const PROXMOX_TAB_KEYS = new Set(TAB_DEFS.map((t) => t.v))

// ceph-s3, kamaji and dbaas serve one service each (object store / managed Kubernetes / managed
// databases) — quota, features, GPUs and metrics have nothing to configure there, so they get the
// trimmed set (connection = endpoint/kubeconfig/chart pin, services = the toggle, configuration).
const BASE_TAB_KEYS = new Set(["connection", "services", "configuration"])

const crumbs = (label: string) => (
  <Breadcrumb>
    <BreadcrumbList>
      <BreadcrumbItem>
        <BreadcrumbLink asChild>
          <Link to="/system/cloud-providers">Cloud providers</Link>
        </BreadcrumbLink>
      </BreadcrumbItem>
      <BreadcrumbSeparator />
      <BreadcrumbItem>
        <BreadcrumbPage>{label}</BreadcrumbPage>
      </BreadcrumbItem>
    </BreadcrumbList>
  </Breadcrumb>
)

export default function CloudProviderDetailPage() {
  const { id = "" } = useParams()
  const { data, isLoading, error } = useAdminGet<CloudProvider>(`/admin/service/${id}`)

  if (isLoading) {
    return (
      <>
        <PageHeader title="Cloud provider" eyebrow="System" breadcrumb={crumbs(id)} />
        <Skeleton className="h-64" />
      </>
    )
  }
  if (error || !data) {
    return (
      <>
        <PageHeader title="Cloud provider" eyebrow="System" breadcrumb={crumbs(id)} />
        <Note>{(error as Error | undefined)?.message ?? "Failed to load cloud provider."}</Note>
      </>
    )
  }
  const p = data
  const tabKeys = p.config?.provider === "proxmox" ? PROXMOX_TAB_KEYS : BASE_TAB_KEYS
  const tabs = TAB_DEFS.filter((t) => tabKeys.has(t.v))

  return (
    <>
      <PageHeader
        title={p.name ?? id}
        eyebrow="System"
        breadcrumb={crumbs(p.name ?? id)}
        description={`${p.type ?? "CLOUD"} provider ${id}`}
        actions={<StatusBadge status={p.status} />}
      />
      <Tabs defaultValue="connection">
        <div className="-mx-1 overflow-x-auto px-1 pb-1">
          <TabsList className="w-max">
            {tabs.map((t) => (
              <TabsTrigger key={t.v} value={t.v}>
                {t.label}
              </TabsTrigger>
            ))}
          </TabsList>
        </div>

        <TabsContent value="connection" className="mt-4">
          <ConnectionTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="storage" className="mt-4">
          <StorageTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="services" className="mt-4">
          <ServicesTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="configuration" className="mt-4">
          <ConfigurationTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="quota" className="mt-4">
          <QuotaTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="gpu" className="mt-4">
          <GpuTab id={id} />
        </TabsContent>
        <TabsContent value="metrics" className="mt-4">
          <MetricsTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="advanced" className="mt-4">
          <AdvancedTab id={id} />
        </TabsContent>
      </Tabs>
    </>
  )
}
