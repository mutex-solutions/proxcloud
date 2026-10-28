import { useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, Copy, PlugZap, RefreshCw, Trash2 } from "lucide-react"
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
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Checkbox } from "@/components/ui/checkbox"
import { Textarea } from "@/components/ui/textarea"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch, ApiError } from "@/lib/api"
import { config } from "@/lib/config"
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
const asArr = (v: unknown): any[] => (Array.isArray(v) ? v : [])
const str = (v: unknown) => (typeof v === "string" ? v : "")
const errMsg = (e: unknown) => (e instanceof Error ? e.message : String(e))

// The 8 named feature components (old-admin Features tab).
const FEATURE_COMPONENTS = [
  "volume-backup",
  "volume-snapshot",
  "image-download",
  "instance-metrics",
  "volume-backed-reinstallation",
  "driver-handles-share-servers",
  "port-security-disable-public",
  "port-security-disable-private",
]
const CONSOLE_TYPES = ["NOVNC", "SPICE_HTML5", "SPICE", "WEBMKS", "SERIAL"]
// Common OpenStack quota keys the Manage-Quota tab exposes (unknown stored keys are preserved on save).
const QUOTA_KEYS = [
  "instances",
  "cores",
  "ram",
  "volumes",
  "gigabytes",
  "snapshots",
  "networks",
  "subnets",
  "ports",
  "routers",
  "floatingips",
  "security_groups",
  "security_group_rules",
  "key_pairs",
]
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

// CopyField — a read-only value + a copy button. The clipboard write can reject (insecure
// context / denied permission), so a rejected copy toasts an error instead of silently doing nothing.
function CopyField({ label, value }: { label?: string; value: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      toast.error("Copy failed — select and copy manually")
    }
  }
  return (
    <div className="grid gap-1">
      {label ? <Label className="text-xs text-muted-foreground">{label}</Label> : null}
      <div className="flex items-center gap-2">
        <Input
          readOnly
          value={value}
          className="font-mono text-xs"
          onFocus={(e) => e.currentTarget.select()}
          aria-label={label ? `Notifier URI for ${label}` : "Notifier URI"}
        />
        <Button type="button" variant="outline" size="icon" onClick={copy} aria-label={label ? `Copy notifier URI for ${label}` : "Copy notifier URI"}>
          {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
        </Button>
      </div>
    </div>
  )
}

// NotifierUriSection — the read-only OpenStack "Notifier URI" per region, ready to paste into the
// cloud's ceilometer event publisher. URL shape = {apiUrl}/notifications/{serviceId}/{region}
// (config.apiUrl already ends in /api/v1). No configured regions → show the {region} template.
function NotifierUriSection({ id, provider }: TabProps) {
  const regions = Object.keys(asObj(provider.config?.regions)).sort()
  const shown = regions.length ? regions : ["{region}"]
  const [secret, setSecret] = useState("")
  const save = useEsSave(id, () => setSecret(""))
  return (
    <div className="mt-6 space-y-3 border-t pt-4">
      <div>
        <div className="text-eyebrow">OpenStack notifier URI</div>
        <p className="text-xs text-muted-foreground">
          Point the cloud's ceilometer event publisher at these URLs to push resource lifecycle events for live
          dashboard updates. Optional — the periodic sync reconciles the cache without it. When a notification secret
          is set below, the webhook is rejected unless the caller sends it as the{" "}
          <span className="font-mono">X-ProxCloud-Notification-Secret</span> header.
        </p>
      </div>
      {shown.map((region) => (
        <CopyField
          key={region}
          label={regions.length ? region : undefined}
          value={`${config.apiUrl}/notifications/${id}/${region}`}
        />
      ))}
      <div className="grid gap-1 pt-1">
        <Label htmlFor="notif-secret" className="text-xs text-muted-foreground">Notification secret (leave blank to keep current)</Label>
        <div className="flex items-center gap-2">
          <Input
            id="notif-secret"
            type="password"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            autoComplete="off"
            placeholder="Shared secret ceilometer must present"
            className="font-mono text-xs"
          />
          <Button
            type="button"
            variant="outline"
            disabled={!secret.trim() || save.isPending}
            onClick={() => save.mutate({ path: `/admin/service/${id}/update`, body: { secret: { notificationSecret: secret.trim() } } })}
          >
            {save.isPending ? "Saving…" : "Save secret"}
          </Button>
        </div>
      </div>
    </div>
  )
}

type TabProps = { id: string; provider: CloudProvider }

// ── 1. Connection (identity + read-only auth + test) ─────────────────────────
type OpenstackAuthResponse = {
  services?: unknown[]
  projects?: unknown[]
  domains?: unknown[]
  roles?: string[]
  selectedProjectName?: string
}
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
    { key: "storageProfiles", label: "Storage profiles", hint: '{"std":{"storages":{"*":"local-zfs"},"shared":false,"snapshots":true,"thin":true,"liveMigrate":false,"replication":false}}' },
    { key: "templates", label: "OS templates", hint: '{"ubuntu-24":{"vmid":9000,"node":"pve1","name":"Ubuntu 24.04","osDistro":"ubuntu","minDiskGb":10}}' },
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
  const qc = useQueryClient()
  const auth = asObj(provider.config?.auth)
  const [result, setResult] = useState<OpenstackAuthResponse | null>(null)
  const test = useMutation({
    // ?externalServiceId + empty body → keystoneAuth uses the stored creds.
    mutationFn: () =>
      apiFetch<OpenstackAuthResponse>(`/admin/service/openstack/auth?externalServiceId=${encodeURIComponent(id)}`, {
        method: "POST",
        body: {},
      }),
    onSuccess: (r) => {
      setResult(r)
      toast.success(`Connection OK — ${r.services?.length ?? 0} services, ${r.projects?.length ?? 0} projects`)
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  // Discover regions + services from the live keystone catalog and persist them onto the provider —
  // this is what makes a UI-created provider usable (client menu + Location dropdown, Services tab).
  const sync = useMutation({
    mutationFn: () => apiFetch(`/admin/service/${id}/discover`, { method: "POST" }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin-get", `/admin/service/${id}`] })
      toast.success("Synced services & regions from the cloud")
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  // kamaji: no Keystone either, and unlike ceph its whole delivery plane (chart pin, ArgoCD
  // AppProject, DNS zone, version→image matrix) is config an operator has to keep current — so this
  // tab is the editable form rather than a read-only card.
  if (provider.config?.provider === "kamaji") {
    return <KamajiConnectionTab id={id} provider={provider} />
  }
  // dbaas: same delivery-config-heavy shape as kamaji — the tab IS the editable form.
  if (provider.config?.provider === "dbaas") {
    return <DbaasConnectionTab id={id} provider={provider} />
  }
  // proxmox: no Keystone — the editable connection form with its own Test button,
  // plus the resource-config blocks (zones/storage/templates/GPUs).
  if (provider.config?.provider === "proxmox") {
    return (
      <div className="grid gap-6">
        <ProxmoxConnectionTab id={id} provider={provider} />
        <ProxmoxConfigCard id={id} provider={provider} />
      </div>
    )
  }
  // ceph-s3: no Keystone — show the S3/Admin Ops config instead of the identity card. There is no
  // backend test endpoint for RGW (the connection is exercised by bootstrap/sync), so no Test button.
  if (provider.config?.provider === "ceph-s3") {
    const c = asObj(provider.config)
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-eyebrow">Ceph RGW (S3) connection</CardTitle>
        </CardHeader>
        <CardContent>
          <Row label="S3 endpoint" value={str(c.s3Endpoint)} />
          <Row label="Admin Ops URL" value={str(c.adminApiUrl)} />
          <Row label="Website endpoint" value={str(c.s3WebsiteEndpoint)} />
          <Row label="Region (zonegroup)" value={str(c.region)} />
          <Row label="RGW uid prefix" value={str(c.uidPrefix)} />
          <Row label="Default quota (GiB)" value={c.defaultQuotaGiB != null ? String(c.defaultQuotaGiB) : ""} />
          <p className="mt-4 text-sm text-muted-foreground">
            Projects are provisioned as dedicated RGW users on entry (or via the project's "attach external
            service" action). The admin keys are stored encrypted and never returned by the API.
          </p>
        </CardContent>
      </Card>
    )
  }
  // kamaji: no Keystone either — the provider IS a Kamaji management cluster reached over a
  // kubeconfig, and clusters are delivered as ArgoCD Applications of the pinned chart. Show the
  // read-only delivery config; the keystone Test-connection/Sync buttons would only 4xx here.
  if (provider.config?.provider === "kamaji") {
    const c = asObj(provider.config)
    const argo = asObj(c.argocd)
    const cluster = asObj(c.cluster)
    const versions = Object.keys(asObj(cluster.versions)).sort()
    const flavors = asArr(cluster.flavors).map(String).filter(Boolean)
    const regions = Object.keys(asObj(c.regions)).sort()
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-eyebrow">Kamaji management cluster (delivery)</CardTitle>
        </CardHeader>
        <CardContent>
          <Row label="Kubeconfig" value="configured" />
          <Row label="Regions" value={regions.join(", ")} />
          <div className="text-eyebrow mt-6">ArgoCD delivery</div>
          <Row label="Namespace" value={str(argo.namespace) || "argocd"} />
          <Row label="AppProject (guardrail)" value={str(argo.project) || "proxcloud-k8s"} />
          <Row label="Chart OCI repo" value={str(argo.chartRepo)} />
          <Row label="Chart name" value={str(argo.chartName) || "openstack-kamaji-cluster"} />
          <Row label="Chart version (pinned)" value={str(argo.chartVersion)} />
          <div className="text-eyebrow mt-6">Cluster defaults</div>
          <Row label="Kamaji DataStore" value={str(cluster.dataStoreName)} />
          <Row label="Floating network ID (API LB)" value={str(cluster.floatingNetworkId)} />
          <Row label="External network ID (workers)" value={str(cluster.externalNetworkId)} />
          <Row label="DNS zone" value={str(cluster.dnsZone)} />
          <Row label="Curated Kubernetes versions" value={versions.length ? String(versions.length) : ""} />
          <Row label="Flavor allowlist" value={flavors.length ? `${flavors.length} flavor id(s)` : "all tenant flavors"} />
          <p className="mt-4 text-sm text-muted-foreground">
            The management-cluster kubeconfig is stored encrypted and never returned by the API. Clusters are
            delivered as ArgoCD Applications of the pinned chart; worker nodes run in each customer's own
            OpenStack tenant.
          </p>
        </CardContent>
      </Card>
    )
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Keystone connection</CardTitle>
      </CardHeader>
      <CardContent>
        <Row label="Identity URL" value={provider.config?.identityUrl} />
        <Row label="Auth type" value={str(auth.adminAuthType)} />
        <Row label="Admin username" value={str(auth.adminUsername)} />
        <Row label="Admin project" value={str(auth.adminProjectName) || str(auth.adminProjectId)} />
        <Row label="Admin domain" value={str(auth.adminDomainName) || str(auth.adminDomainId)} />
        <div className="mt-4 flex flex-wrap items-center gap-3">
          <Button onClick={() => test.mutate()} disabled={test.isPending}>
            <PlugZap className="size-4" />
            {test.isPending ? "Testing…" : "Test connection"}
          </Button>
          <Button variant="outline" onClick={() => sync.mutate()} disabled={sync.isPending}>
            <RefreshCw className="size-4" />
            {sync.isPending ? "Syncing…" : "Sync services & regions"}
          </Button>
          {result ? (
            <span className="text-sm text-muted-foreground">
              {result.services?.length ?? 0} services · {result.projects?.length ?? 0} projects
              {result.selectedProjectName ? ` · scoped to ${result.selectedProjectName}` : ""}
            </span>
          ) : null}
        </div>
        <NotifierUriSection id={id} provider={provider} />
      </CardContent>
    </Card>
  )
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
          // /configuration sets name, status and config.openstackReseller — pass the current reseller so we never null it.
          onClick={() =>
            save.mutate({
              path: `/admin/service/${id}/configuration`,
              body: { name, status, config: { openstackReseller: asObj((provider.config as Obj)?.openstackReseller) } },
            })
          }
          disabled={save.isPending}
        >
          {save.isPending ? "Saving…" : "Save configuration"}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── 4. Provisioning (config.provisioning.projectRoles) ───────────────────────
function ProvisioningTab({ id, provider }: TabProps) {
  const prov = asObj((provider.config as Obj)?.provisioning)
  const [roles, setRoles] = useState(asArr(prov.projectRoles).map(String).join("\n"))
  const save = useEsSave(id)
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Provisioning roles</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <Note>Roles granted to a project on provisioning. One role name per line.</Note>
        <Textarea
          aria-label="Provisioning roles, one per line"
          rows={6}
          value={roles}
          onChange={(e) => setRoles(e.target.value)}
          placeholder="member&#10;_member_"
          className="font-mono text-sm"
        />
        <Button
          // PUT /service/{id}/update merges config; keep the rest of provisioning (e.g. quota) intact.
          onClick={() =>
            save.mutate({
              path: `/admin/service/${id}/update`,
              body: { config: { provisioning: { ...prov, projectRoles: roles.split("\n").map((r) => r.trim()).filter(Boolean) } } },
            })
          }
          disabled={save.isPending}
        >
          {save.isPending ? "Saving…" : "Save provisioning"}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── 5. Features (8 component toggles + console type) ──────────────────────────
function FeaturesTab({ id, provider }: TabProps) {
  const features = asObj((provider.config as Obj)?.features)
  const compMap: Record<string, boolean> = {}
  for (const c of asArr(features.components)) if (asObj(c).name) compMap[str(asObj(c).name)] = asObj(c).enabled === true
  const [comps, setComps] = useState<Record<string, boolean>>(() => {
    const seed: Record<string, boolean> = {}
    for (const n of FEATURE_COMPONENTS) seed[n] = compMap[n] === true
    return seed
  })
  const storedConsole = asArr(features.enabledConsoleTypes).map(String)[0] || asArr((provider.config as Obj)?.enabledConsoleTypes).map(String)[0] || "NOVNC"
  const [consoleType, setConsoleType] = useState(storedConsole)
  const save = useEsSave(id)

  const submit = () => {
    // Merge over the stored features (keep volumeTypes/publicNetworks/shareProtocols the other tabs manage),
    // then overwrite the known component set (preserving any unknown stored components).
    const merged: Record<string, boolean> = { ...compMap }
    for (const n of FEATURE_COMPONENTS) merged[n] = comps[n]
    const components = Object.entries(merged).map(([name, enabled]) => ({ name, enabled }))
    save.mutate({ path: `/admin/service/${id}/features`, body: { ...features, components, enabledConsoleTypes: [consoleType] } })
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Feature components</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="divide-y">
          {FEATURE_COMPONENTS.map((n) => (
            <div key={n} className="flex items-center justify-between py-2">
              <span className="font-mono text-xs">{n}</span>
              <Switch checked={comps[n]} onCheckedChange={(on) => setComps((c) => ({ ...c, [n]: on }))} aria-label={n} />
            </div>
          ))}
        </div>
        <div className="grid max-w-md gap-2">
          <Label htmlFor="es-console">Console type</Label>
          <Select value={consoleType} onValueChange={setConsoleType}>
            <SelectTrigger id="es-console" className="w-full sm:w-56">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {CONSOLE_TYPES.map((c) => (
                <SelectItem key={c} value={c}>
                  {c}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <Button onClick={submit} disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Save features"}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── 6. Quota (config.provisioning.quota) ─────────────────────────────────────
function QuotaTab({ id, provider }: TabProps) {
  const quota = asObj(asObj((provider.config as Obj)?.provisioning).quota)
  const isPve = provider.config?.provider === "proxmox"
  const keys = isPve ? PVE_QUOTA_KEYS : QUOTA_KEYS
  // PVE "gpu" is the any-model limit quota.gpu["*"]; every other key is a flat number.
  const read = (k: string) => (isPve && k === "gpu" ? asObj(quota.gpu)["*"] : quota[k])
  const [vals, setVals] = useState<Record<string, string>>(() => {
    const seed: Record<string, string> = {}
    for (const k of keys) seed[k] = read(k) != null ? String(read(k)) : ""
    return seed
  })
  const save = useEsSave(id)
  const submit = () => {
    // /quota replaces config.provisioning.quota wholesale → merge over the stored quota (keeps placementQuotas etc).
    // A blank field removes the key (unlimited).
    const next: Obj = { ...quota }
    for (const k of keys) {
      const v = vals[k].trim()
      let parsed: number | undefined
      if (v !== "") {
        parsed = Number(v)
        // NaN is dropped by JSON serialization — the key would silently keep its old value.
        if (!Number.isFinite(parsed) || (isPve && (parsed < 0 || !Number.isInteger(parsed)))) {
          toast.error(`Invalid quota value for "${k}"`)
          return
        }
      }
      if (isPve && k === "gpu") {
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
        {isPve ? (
          <p className="text-sm text-muted-foreground">
            Copied onto each new project and enforced for projects without their own quota. Blank = unlimited;
            ramMb in MiB, diskGb in GiB, gpu = devices of any model.
          </p>
        ) : null}
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

// ── 6b. GPU (placement capacity + unpriced-flavor guard) ─────────────────────
type GpuRegionCapacity = { region: string; gpus: Array<{ name: string; total: number; inUse: number }> }
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
              No GPU devices found in placement (expects nova PCI-in-placement resource providers
              carrying the CUSTOM_PCI_GPU trait).
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

// ── 7. VHI-ostor (config.vhiOstorConfig + write-only secret auth) ─────────────
function VhiOstorTab({ id, provider }: TabProps) {
  const cfg = asObj((provider.config as Obj)?.vhiOstorConfig)
  const [text, setText] = useState(JSON.stringify(cfg, null, 2))
  const [accessKey, setAccessKey] = useState("")
  const [secretKey, setSecretKey] = useState("")
  const save = useEsSave(id)
  const submit = () => {
    let parsed: unknown
    try {
      parsed = text.trim() ? JSON.parse(text) : {}
    } catch {
      toast.error("VHI-ostor config is not valid JSON")
      return
    }
    const body: Obj = { vhiOstorConfig: parsed }
    if (accessKey || secretKey) body.vhiOstorAuth = { accessKey, secretKey }
    save.mutate({ path: `/admin/service/${id}/vhi-ostor`, body })
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">VHI object storage (Ostor)</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid gap-2">
          <Label htmlFor="ostor-json">Ostor config (JSON)</Label>
          <Textarea id="ostor-json" rows={8} value={text} onChange={(e) => setText(e.target.value)} className="font-mono text-xs" />
        </div>
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
          <div className="grid gap-1">
            <Label htmlFor="ostor-ak" className="text-xs">Access key (leave blank to keep)</Label>
            <Input id="ostor-ak" className="font-mono" value={accessKey} onChange={(e) => setAccessKey(e.target.value)} autoComplete="off" />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="ostor-sk" className="text-xs">Secret key (leave blank to keep)</Label>
            <Input id="ostor-sk" type="password" className="font-mono" value={secretKey} onChange={(e) => setSecretKey(e.target.value)} autoComplete="off" />
          </div>
        </div>
        <Button onClick={submit} disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Save Ostor settings"}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── 8. Advanced (gnocchi granularity + delete provider) ──────────────────────
function AdvancedTab({ id, provider }: TabProps) {
  const navigate = useNavigate()
  const [gran, setGran] = useState(String((provider.config as Obj)?.gnocchiGranularity ?? 300))
  const save = useEsSave(id)
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
      <Card>
        <CardHeader>
          <CardTitle className="text-eyebrow">Advanced settings</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid max-w-xs gap-2">
            <Label htmlFor="gran">Gnocchi granularity (seconds)</Label>
            <Input id="gran" type="number" value={gran} onChange={(e) => setGran(e.target.value)} />
          </div>
          <Button
            onClick={() => {
              const parsed = Number(gran)
              if (!Number.isInteger(parsed) || parsed <= 0) {
                toast.error("Granularity must be a positive integer")
                return
              }
              save.mutate({ path: `/admin/service/${id}/gnocchi-granularity`, body: { granularity: parsed } })
            }}
            disabled={save.isPending}
          >
            {save.isPending ? "Saving…" : "Save granularity"}
          </Button>
        </CardContent>
      </Card>
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

// ── 8b. Metrics (usage source for traffic billing: gnocchi | prometheus | none) ──
type MetricsTestResp = { ok: boolean; error?: string; trafficSeries?: number; monthStartSeries?: number; warnings?: string[] }
function MetricsTab({ id, provider }: TabProps) {
  const stored = asObj(asObj((provider.config as Obj)?.metrics))
  const prom = asObj(stored.prometheus)
  const [source, setSource] = useState(String(stored.source ?? "gnocchi"))
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
    // typed-but-hidden values on a gnocchi/none save (invisible side effects on secrets).
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
            Where the hourly traffic-billing job reads per-instance network usage from. Gnocchi (Ceilometer) is the
            default; a Prometheus-compatible endpoint (Prometheus, Mimir, VictoriaMetrics, Thanos) can be used instead;
            &quot;None&quot; disables usage ingestion for this provider. Gnocchi granularity stays under Advanced.
          </p>
          <div className="grid max-w-xs gap-2">
            <Label htmlFor="mx-source">Source</Label>
            <Select value={source} onValueChange={setSource}>
              <SelectTrigger id="mx-source">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="gnocchi">Gnocchi (Ceilometer, default)</SelectItem>
                <SelectItem value="prometheus">Prometheus-compatible</SelectItem>
                <SelectItem value="none">None (disable usage metrics)</SelectItem>
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
                    <SelectItem value="libvirt-exporter">libvirt-exporter (kolla prometheus-libvirt-exporter)</SelectItem>
                    <SelectItem value="ceilometer-pushgateway">ceilometer-pushgateway (bare names, resource_id)</SelectItem>
                    <SelectItem value="ceilometer-exporter">ceilometer-exporter / sg-core (ceilometer_ prefix)</SelectItem>
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
                  placeholder='{"X-Scope-OrgID": "openstack"}'
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

// ── 9. Volume types (live GET + per-type enable/display-name per region) ──────
type VolumeTypesResp = { region: string; volumeTypes: string[] }[]
type VtState = Record<string, Record<string, { enabled: boolean; displayName: string }>>
function VolumeTypesTab({ id, provider }: TabProps) {
  const saved = asObj(asObj((provider.config as Obj)?.features).volumeTypes) // { region: [{name,displayName,enabled}] }
  const live = useQuery({
    queryKey: ["es-live", id, "volume-types"],
    queryFn: () => apiFetch<VolumeTypesResp>(`/admin/service/${id}/volume/types`),
  })
  const [state, setState] = useState<VtState>({})
  const save = useEsSave(id)

  const rows = live.data ?? []
  const cell = (region: string, name: string) => {
    const stored = asArr(saved[region]).find((t) => asObj(t).name === name)
    const cur = state[region]?.[name]
    return {
      enabled: cur ? cur.enabled : asObj(stored).enabled === true,
      displayName: cur ? cur.displayName : str(asObj(stored).displayName),
    }
  }
  const set = (region: string, name: string, patch: Partial<{ enabled: boolean; displayName: string }>) =>
    setState((s) => ({ ...s, [region]: { ...(s[region] ?? {}), [name]: { ...cell(region, name), ...patch } } }))

  const submit = () => {
    // ponytail: OpenStackVolumeType is sent as {name, displayName, enabled} keyed by region; adjust if the
    // client model carries more fields. Only live (available) types are written.
    const body: Record<string, any[]> = {}
    for (const { region, volumeTypes } of rows) body[region] = volumeTypes.map((name) => ({ name, ...cell(region, name) }))
    save.mutate({ path: `/admin/service/${id}/volume/types`, body })
  }

  if (live.isLoading) return <Skeleton className="h-40" />
  if (live.error) return <Note>{errMsg(live.error)}</Note>
  if (rows.length === 0) return <Note>No volume types reported by the cloud.</Note>
  return (
    <div className="space-y-4">
      {rows.map(({ region, volumeTypes }) => (
        <Card key={region} className="overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{region} — type</TableHead>
                <TableHead>Display name</TableHead>
                <TableHead className="w-24">Enabled</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {volumeTypes.map((name) => {
                const c = cell(region, name)
                return (
                  <TableRow key={name}>
                    <TableCell className="font-mono text-xs">{name}</TableCell>
                    <TableCell>
                      <Input
                        value={c.displayName}
                        onChange={(e) => set(region, name, { displayName: e.target.value })}
                        aria-label={`Display name for ${name}`}
                      />
                    </TableCell>
                    <TableCell>
                      <Switch
                        checked={c.enabled}
                        onCheckedChange={(on) => set(region, name, { enabled: on })}
                        aria-label={`Enable ${name}`}
                      />
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </Card>
      ))}
      <Button onClick={submit} disabled={save.isPending}>
        {save.isPending ? "Saving…" : "Save volume types"}
      </Button>
    </div>
  )
}

// ── 10. Placement (VHI placement-quotas; GET is an empty seam → doc/empty) ────
function PlacementTab({ id, provider }: TabProps) {
  const stored = asObj(asObj((provider.config as Obj)?.provisioning).quota).placementQuotas
  const [text, setText] = useState(JSON.stringify(stored ?? [], null, 2))
  const save = useEsSave(id)
  const submit = () => {
    let parsed: unknown
    try {
      parsed = text.trim() ? JSON.parse(text) : []
    } catch {
      toast.error("Placement quotas is not valid JSON")
      return
    }
    save.mutate({ path: `/admin/service/${id}/vhi/placement-quotas`, body: parsed })
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">VHI placement quotas</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* ponytail: the live GET is an empty stub (VHI-only) → edit the stored value as raw JSON. */}
        <Note>VHI-specific placement quotas. Edit the stored value as JSON.</Note>
        <Textarea
          aria-label="Placement quotas JSON"
          rows={8}
          value={text}
          onChange={(e) => setText(e.target.value)}
          className="font-mono text-xs"
        />
        <Button onClick={submit} disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Save placement quotas"}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── 11. Reseller (config.openstackReseller) ──────────────────────────────────
function ResellerTab({ id, provider }: TabProps) {
  const r = asObj((provider.config as Obj)?.openstackReseller)
  const [enabled, setEnabled] = useState(r.enabled === true)
  const [org, setOrg] = useState(str(r.organizationId))
  const [domain, setDomain] = useState(str(r.domain))
  const save = useEsSave(id)
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Reseller</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex items-center justify-between border-b py-2">
          <Label htmlFor="rs-enabled" className="text-sm font-normal">Enabled</Label>
          <Switch id="rs-enabled" checked={enabled} onCheckedChange={setEnabled} />
        </div>
        <div className="grid max-w-md gap-2">
          <Label htmlFor="rs-org">Organization ID</Label>
          <Input id="rs-org" className="font-mono" value={org} onChange={(e) => setOrg(e.target.value)} />
        </div>
        <div className="grid max-w-md gap-2">
          <Label htmlFor="rs-domain">Domain</Label>
          <Input id="rs-domain" value={domain} onChange={(e) => setDomain(e.target.value)} />
        </div>
        <Button
          onClick={() => save.mutate({ path: `/admin/service/${id}/reseller`, body: { ...r, enabled, organizationId: org, domain } })}
          disabled={save.isPending}
        >
          {save.isPending ? "Saving…" : "Save reseller"}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── 12. Availability zones (live GET + per-zone enable/display-name) ──────────
type AzResp = Record<string, Record<string, string[]>> // serviceType → region → [zones]
function AvailabilityZonesTab({ id, provider }: TabProps) {
  // stored config.availabilityZones: accept either a list [{name,displayName,enabled}] or a name-keyed map.
  const savedRaw = (provider.config as Obj)?.availabilityZones
  const savedMap: Record<string, { displayName: string; enabled: boolean }> = {}
  for (const e of asArr(savedRaw)) if (asObj(e).name) savedMap[str(asObj(e).name)] = { displayName: str(asObj(e).displayName), enabled: asObj(e).enabled === true }
  if (!Array.isArray(savedRaw)) for (const [name, v] of Object.entries(asObj(savedRaw))) savedMap[name] = { displayName: str(asObj(v).displayName), enabled: asObj(v).enabled === true }

  const live = useQuery({
    queryKey: ["es-live", id, "availability-zones"],
    queryFn: () => apiFetch<AzResp>(`/admin/service/${id}/availability-zones`),
  })
  const [state, setState] = useState<Record<string, { enabled: boolean; displayName: string }>>({})
  const save = useEsSave(id)

  const names = new Set<string>(Object.keys(savedMap))
  for (const byRegion of Object.values(live.data ?? {})) for (const zones of Object.values(byRegion)) for (const z of zones) names.add(z)
  const zoneNames = [...names].sort()

  const cell = (name: string) => state[name] ?? savedMap[name] ?? { enabled: false, displayName: "" }
  const set = (name: string, patch: Partial<{ enabled: boolean; displayName: string }>) =>
    setState((s) => ({ ...s, [name]: { ...cell(name), ...patch } }))

  const submit = () => {
    // ponytail: sent as a list [{name, displayName, enabled}]; switch to a name-keyed map if the client reads one.
    const body = zoneNames.map((name) => ({ name, ...cell(name) }))
    save.mutate({ path: `/admin/service/${id}/availability-zones`, body })
  }

  if (live.isLoading) return <Skeleton className="h-40" />
  if (zoneNames.length === 0) return <Note>{live.error ? errMsg(live.error) : "No availability zones reported by the cloud."}</Note>
  return (
    <div className="space-y-4">
      <Card className="overflow-hidden py-0">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Zone</TableHead>
              <TableHead>Display name</TableHead>
              <TableHead className="w-24">Enabled</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {zoneNames.map((name) => {
              const c = cell(name)
              return (
                <TableRow key={name}>
                  <TableCell className="font-mono text-xs">{name}</TableCell>
                  <TableCell>
                    <Input
                      value={c.displayName}
                      onChange={(e) => set(name, { displayName: e.target.value })}
                      aria-label={`Display name for ${name}`}
                    />
                  </TableCell>
                  <TableCell>
                    <Switch
                      checked={c.enabled}
                      onCheckedChange={(on) => set(name, { enabled: on })}
                      aria-label={`Enable ${name}`}
                    />
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </Card>
      <Button onClick={submit} disabled={save.isPending}>
        {save.isPending ? "Saving…" : "Save availability zones"}
      </Button>
    </div>
  )
}

// ── 13. Project imports (live keystone project list + bulk-import) ────────────
type ImportProject = { project: { id: string; name: string }; proxcloudProjectId?: string; users?: unknown[] }
function ProjectImportsTab({ id }: { id: string }) {
  const qc = useQueryClient()
  const q = useQuery({
    queryKey: ["es-live", id, "project-import"],
    queryFn: () => apiFetch<ImportProject[]>(`/admin/project-import/${id}`),
    retry: false,
  })
  const [sel, setSel] = useState<Record<string, boolean>>({})
  const seam = q.error instanceof ApiError && q.error.code === 501
  const rows = q.data ?? []
  const importable = rows.filter((r) => !r.proxcloudProjectId)
  const chosen = importable.filter((r) => sel[r.project.id])

  const doImport = useMutation({
    mutationFn: () =>
      apiFetch(`/admin/project-import/bulk-import/${id}`, {
        method: "POST",
        // proxcloudProjectId blank → the server inserts a new linked project for each.
        body: chosen.map((r) => ({ proxcloudProjectId: "", project: r.project, users: [] })),
      }),
    onSuccess: () => {
      setSel({})
      qc.invalidateQueries({ queryKey: ["es-live", id, "project-import"] })
      toast.success(`Imported ${chosen.length} project(s)`)
    },
    onError: (e) => toast.error(errMsg(e)),
  })

  return (
    <Card className="overflow-hidden">
      <CardHeader>
        <CardTitle className="text-eyebrow">Project imports</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {q.isLoading ? (
          <Skeleton className="h-24" />
        ) : seam ? (
          <Note>
            Listing importable OpenStack projects requires a live Keystone connection, which is not available on this
            deployment.
          </Note>
        ) : q.error ? (
          <Note>{errMsg(q.error)}</Note>
        ) : rows.length === 0 ? (
          <Note>No OpenStack projects were found on this provider.</Note>
        ) : (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-10">
                    <Checkbox
                      checked={chosen.length > 0 && chosen.length === importable.length}
                      onCheckedChange={(v) =>
                        setSel(v ? Object.fromEntries(importable.map((r) => [r.project.id, true])) : {})
                      }
                      aria-label="Select all importable"
                    />
                  </TableHead>
                  <TableHead>Project</TableHead>
                  <TableHead>OpenStack ID</TableHead>
                  <TableHead className="w-28">Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((r) => {
                  const linked = !!r.proxcloudProjectId
                  return (
                    <TableRow key={r.project.id}>
                      <TableCell>
                        <Checkbox
                          checked={!!sel[r.project.id]}
                          disabled={linked}
                          onCheckedChange={(v) => setSel((s) => ({ ...s, [r.project.id]: !!v }))}
                          aria-label={`Select ${r.project.name}`}
                        />
                      </TableCell>
                      <TableCell className="font-medium">{r.project.name}</TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">{r.project.id}</TableCell>
                      <TableCell>
                        <StatusBadge status={linked ? "IMPORTED" : "IMPORTABLE"} />
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
            <Button onClick={() => doImport.mutate()} disabled={chosen.length === 0 || doImport.isPending}>
              {doImport.isPending ? "Importing…" : `Import ${chosen.length || ""} selected`}
            </Button>
          </>
        )}
      </CardContent>
    </Card>
  )
}

// ── 14. Share protocols (live GET defaults + enable toggle) ───────────────────
type ShareProtocol = { name: string; displayName: string; enabled: boolean }
function ShareProtocolsTab({ id }: { id: string }) {
  const live = useQuery({
    queryKey: ["es-live", id, "share-protocols"],
    queryFn: () => apiFetch<ShareProtocol[]>(`/admin/service/${id}/share/protocols`),
  })
  const [state, setState] = useState<Record<string, boolean>>({})
  const save = useEsSave(id)
  const rows = live.data ?? []
  const enabledOf = (p: ShareProtocol) => (p.name in state ? state[p.name] : p.enabled)
  const submit = () =>
    save.mutate({
      path: `/admin/service/${id}/share/protocols`,
      body: rows.map((p) => ({ name: p.name, displayName: p.displayName, enabled: enabledOf(p) })),
    })

  if (live.isLoading) return <Skeleton className="h-40" />
  if (live.error) return <Note>{errMsg(live.error)}</Note>
  if (rows.length === 0) return <Note>No share protocols available.</Note>
  return (
    <div className="space-y-4">
      <Card className="overflow-hidden py-0">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Protocol</TableHead>
              <TableHead className="w-24">Enabled</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((p) => (
              <TableRow key={p.name}>
                <TableCell>
                  <span className="font-medium">{p.displayName}</span>{" "}
                  <span className="font-mono text-xs text-muted-foreground">({p.name})</span>
                </TableCell>
                <TableCell>
                  <Switch
                    checked={enabledOf(p)}
                    onCheckedChange={(on) => setState((s) => ({ ...s, [p.name]: on }))}
                    aria-label={`Enable ${p.displayName}`}
                  />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Card>
      <Button onClick={submit} disabled={save.isPending}>
        {save.isPending ? "Saving…" : "Save share protocols"}
      </Button>
    </div>
  )
}

// ── page ─────────────────────────────────────────────────────────────────────
const TAB_DEFS = [
  { v: "connection", label: "Connection" },
  { v: "services", label: "Services" },
  { v: "configuration", label: "Configuration" },
  { v: "provisioning", label: "Provisioning" },
  { v: "features", label: "Features" },
  { v: "quota", label: "Quota" },
  { v: "gpu", label: "GPU" },
  { v: "vhi-ostor", label: "VHI-ostor" },
  { v: "metrics", label: "Metrics" },
  { v: "advanced", label: "Advanced" },
  { v: "volume-types", label: "Volume types" },
  { v: "placement", label: "Placement" },
  { v: "reseller", label: "Reseller" },
  { v: "availability-zones", label: "Availability zones" },
  { v: "project-imports", label: "Project imports" },
  { v: "share-protocols", label: "Share protocols" },
]

// A ceph-s3 provider serves ONLY object-store over S3/Admin Ops — every other tab is Keystone/OpenStack
// machinery (quota, features, volume types, AZs, …) with nothing to configure on RGW.
const CEPH_TAB_KEYS = new Set(["connection", "services", "configuration"])

// A kamaji provider serves ONLY managed Kubernetes off its management cluster — same trimmed set
// (connection = kubeconfig/chart pin, services = the kubernetes toggle, configuration = raw doc).
const KAMAJI_TAB_KEYS = CEPH_TAB_KEYS

// A dbaas provider serves ONLY managed databases off its DB cluster — same trimmed set again
// (connection = kubeconfig/chart pin/engine catalog, services = the database toggle).
const DBAAS_TAB_KEYS = CEPH_TAB_KEYS

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
  const tabs =
    p.config?.provider === "ceph-s3"
      ? TAB_DEFS.filter((t) => CEPH_TAB_KEYS.has(t.v))
      : p.config?.provider === "kamaji"
        ? TAB_DEFS.filter((t) => KAMAJI_TAB_KEYS.has(t.v))
        : p.config?.provider === "dbaas"
          ? TAB_DEFS.filter((t) => DBAAS_TAB_KEYS.has(t.v))
          : TAB_DEFS

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
        <TabsContent value="services" className="mt-4">
          <ServicesTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="configuration" className="mt-4">
          <ConfigurationTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="provisioning" className="mt-4">
          <ProvisioningTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="features" className="mt-4">
          <FeaturesTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="quota" className="mt-4">
          <QuotaTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="gpu" className="mt-4">
          <GpuTab id={id} />
        </TabsContent>
        <TabsContent value="vhi-ostor" className="mt-4">
          <VhiOstorTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="metrics" className="mt-4">
          <MetricsTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="advanced" className="mt-4">
          <AdvancedTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="volume-types" className="mt-4">
          <VolumeTypesTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="placement" className="mt-4">
          <PlacementTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="reseller" className="mt-4">
          <ResellerTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="availability-zones" className="mt-4">
          <AvailabilityZonesTab id={id} provider={p} />
        </TabsContent>
        <TabsContent value="project-imports" className="mt-4">
          <ProjectImportsTab id={id} />
        </TabsContent>
        <TabsContent value="share-protocols" className="mt-4">
          <ShareProtocolsTab id={id} />
        </TabsContent>
      </Tabs>
    </>
  )
}
