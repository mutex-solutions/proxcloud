import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { Cloud, Plus } from "lucide-react"
import { useNavigate } from "react-router-dom"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { apiFetch } from "@/lib/api"
import { useAdminList } from "@/lib/hooks"
import {
  KamajiProviderForm,
  emptyKamajiForm,
  kamajiFormToBody,
  kamajiFormValid,
  type KamajiFormState,
} from "./kamajiProvider"
import {
  DbaasProviderForm,
  emptyDbaasForm,
  dbaasFormToBody,
  dbaasFormValid,
  type DbaasFormState,
} from "./dbaasProvider"
import {
  ProxmoxProviderForm,
  emptyProxmoxForm,
  proxmoxFormToBody,
  proxmoxFormValid,
  type ProxmoxFormState,
} from "./proxmoxProvider"

const LIST_PATH = "/admin/service"

export type CloudProvider = {
  id: string
  name?: string
  type?: string
  status?: string
  config?: {
    apiUrl?: string
    provider?: string
    regions?: Record<string, unknown>
    services?: Record<string, Record<string, boolean>>
    auth?: Record<string, unknown>
    // ceph-s3 provider fields (config.provider === "ceph-s3")
    s3Endpoint?: string
    adminApiUrl?: string
    s3WebsiteEndpoint?: string
    region?: string
    uidPrefix?: string
    defaultQuotaGiB?: number
  }
}

// Region chips: providers key config.regions; ceph-s3/proxmox store a single config.region.
function providerRegions(p: CloudProvider): string[] {
  const keyed = Object.keys(p.config?.regions ?? {})
  if (keyed.length) return keyed
  return p.config?.region ? [p.config.region] : []
}


// ── truenas create form ──────────────────────────────────────────────────────
type TrueNASFormState = {
  name: string; apiUrl: string; username: string; apiKey: string; fingerprint: string
  parentDataset: string; mountHost: string; snatSources: string; region: string; defaultQuotaGiB: string
}

const emptyTrueNASForm: TrueNASFormState = {
  name: "", apiUrl: "", username: "proxcloud", apiKey: "", fingerprint: "",
  parentDataset: "", mountHost: "", snatSources: "", region: "pve", defaultQuotaGiB: "",
}

const trueNASFormValid = (f: TrueNASFormState) =>
  [f.name, f.apiUrl, f.username, f.apiKey, f.parentDataset, f.region].every((v) => v.trim() !== "") &&
  f.apiUrl.trim().startsWith("https://") &&
  cephQuotaValid(f.defaultQuotaGiB)

export const parseSNATSources = (v: string) => v.split(",").map((a) => a.trim()).filter(Boolean)

export const SNAT_SOURCES_HELP =
  "Leave empty unless a NAT sits between tenants and this NAS, so that the NAS sees the NAT's address instead of the " +
  "tenant's. Each address listed here goes on every export of this provider, Kubernetes RWX datasets included, and " +
  "stays there through sync. The cost: the NAS can no longer tell tenants apart. Every tenant behind that NAT can " +
  "mount every NFS share of every project on this provider, and only the SMB password still protects an SMB share. " +
  "Single addresses only, no ranges."

// File shares only, so the shares service is enabled directly. The API key goes to the secret and never comes back.
function trueNASFormToBody(f: TrueNASFormState) {
  const region = f.region.trim()
  return {
    name: f.name.trim(),
    type: "CLOUD",
    status: "PUBLIC",
    config: {
      provider: "truenas",
      apiUrl: f.apiUrl.trim().replace(/\/+$/, ""),
      username: f.username.trim(),
      parentDataset: f.parentDataset.trim(),
      region,
      regions: { [region]: { displayName: region } },
      ...(f.fingerprint.trim() ? { fingerprint: f.fingerprint.trim() } : {}),
      ...(f.mountHost.trim() ? { mountHost: f.mountHost.trim() } : {}),
      ...(parseSNATSources(f.snatSources).length ? { snatSources: parseSNATSources(f.snatSources) } : {}),
      ...(f.defaultQuotaGiB.trim() ? { defaultQuotaGiB: Number(f.defaultQuotaGiB) } : {}),
      services: { shares: { [region]: true } },
    },
    secret: { apiKey: f.apiKey.trim() },
  }
}

// ── powerdns create form ─────────────────────────────────────────────────────
type PowerDNSFormState = { name: string; apiUrl: string; apiKey: string; nameservers: string; region: string }
const emptyPowerDNSForm: PowerDNSFormState = { name: "", apiUrl: "", apiKey: "", nameservers: "", region: "pve" }
const powerDNSFormValid = (f: PowerDNSFormState) =>
  [f.name, f.apiUrl, f.apiKey, f.nameservers, f.region].every((v) => v.trim() !== "")

// DNS zones only, so the dns service is enabled directly. The API key goes to the secret and never comes back.
function powerDNSFormToBody(f: PowerDNSFormState) {
  const region = f.region.trim()
  return {
    name: f.name.trim(),
    type: "CLOUD",
    status: "PUBLIC",
    config: {
      provider: "powerdns",
      apiUrl: f.apiUrl.trim().replace(/\/+$/, ""),
      nameservers: f.nameservers.split(",").map((n) => n.trim()).filter(Boolean),
      region,
      regions: { [region]: { displayName: region } },
      services: { dns: { [region]: true } },
    },
    secret: { apiKey: f.apiKey.trim() },
  }
}

function PowerDNSProviderForm({ form, setForm }: { form: PowerDNSFormState; setForm: (f: PowerDNSFormState) => void }) {
  const field = (key: keyof PowerDNSFormState, label: string, placeholder = "", type = "text") => (
    <div className="grid gap-2">
      <Label htmlFor={`pd-${key}`}>{label}</Label>
      <Input id={`pd-${key}`} type={type} value={form[key]} onChange={(e) => setForm({ ...form, [key]: e.target.value })} placeholder={placeholder} />
    </div>
  )
  return (
    <div className="grid gap-4">
      {field("name", "Display name", "DNS")}
      {field("apiUrl", "API URL", "https://dns.example.com:8081")}
      {field("apiKey", "API key", "", "password")}
      {field("nameservers", "Name servers of new zones, comma separated", "ns1.example.com, ns2.example.com")}
      {field("region", "Region", "pve")}
    </div>
  )
}

function TrueNASProviderForm({ form, setForm }: { form: TrueNASFormState; setForm: (f: TrueNASFormState) => void }) {
  const field = (key: keyof TrueNASFormState, label: string, placeholder = "", type = "text") => (
    <div className="grid gap-2">
      <Label htmlFor={`tn-${key}`}>{label}</Label>
      <Input id={`tn-${key}`} type={type} value={form[key]} onChange={(e) => setForm({ ...form, [key]: e.target.value })} placeholder={placeholder} />
    </div>
  )
  return (
    <div className="grid gap-4">
      {field("name", "Display name", "TrueNAS")}
      {field("apiUrl", "URL (HTTPS only)", "https://nas.example.com")}
      {field("username", "API key owner", "proxcloud")}
      {field("apiKey", "API key", "", "password")}
      {field("fingerprint", "Certificate fingerprint (self-signed)", "AA:BB:…")}
      {field("parentDataset", "Parent dataset", "tank/shares")}
      {field("mountHost", "Address clients mount from (optional)", "defaults to the URL's host")}
      <div className="grid gap-2">
        {field("snatSources", "NAT source addresses, comma separated (optional, weakens isolation)", "none")}
        <p className="text-sm text-muted-foreground">{SNAT_SOURCES_HELP}</p>
      </div>
      {field("region", "Region", "pve")}
      {field("defaultQuotaGiB", "Quota per project, GiB (optional)")}
    </div>
  )
}

// ── ceph-s3 create form ──────────────────────────────────────────────────────
type CephFormState = {
  name: string
  s3Endpoint: string
  adminApiUrl: string
  s3WebsiteEndpoint: string
  region: string
  uidPrefix: string
  defaultQuotaGiB: string
  adminAccessKey: string
  adminSecretKey: string
}

const emptyCephForm: CephFormState = {
  name: "",
  s3Endpoint: "",
  adminApiUrl: "",
  s3WebsiteEndpoint: "",
  region: "us-east-1",
  uidPrefix: "",
  defaultQuotaGiB: "",
  adminAccessKey: "",
  adminSecretKey: "",
}

// defaultQuotaGiB is an integer server-side (config.defaultQuotaGiB) — empty means "no quota".
const cephQuotaValid = (v: string) => v.trim() === "" || (Number.isInteger(Number(v)) && Number(v) > 0)

const cephFormValid = (f: CephFormState) =>
  [f.name, f.s3Endpoint, f.adminApiUrl, f.region, f.adminAccessKey, f.adminSecretKey].every((v) => v.trim() !== "") &&
  cephQuotaValid(f.defaultQuotaGiB)

// cephFormToBody builds the ceph-s3 ExternalService document (see docs/cloud-integration.md
// "Provider config"): the S3 + Admin Ops endpoints drive everything, and the
// secret carries the RGW admin keys (caps users=*;buckets=*;usage=*). object-store is the ONLY
// service a ceph-s3 provider serves, so it is enabled here directly — no discover round-trip exists.
function cephFormToBody(f: CephFormState, provider: "ceph-s3" | "rustfs" = "ceph-s3") {
  const region = f.region.trim()
  const trimURL = (s: string) => s.trim().replace(/\/+$/, "")
  return {
    name: f.name.trim(),
    type: "CLOUD",
    status: "PUBLIC",
    config: {
      provider,
      s3Endpoint: trimURL(f.s3Endpoint),
      adminApiUrl: trimURL(f.adminApiUrl),
      ...(f.s3WebsiteEndpoint.trim() ? { s3WebsiteEndpoint: trimURL(f.s3WebsiteEndpoint) } : {}),
      region,
      ...(f.uidPrefix.trim() ? { uidPrefix: f.uidPrefix.trim() } : {}),
      ...(f.defaultQuotaGiB.trim() ? { defaultQuotaGiB: Number(f.defaultQuotaGiB) } : {}),
      services: { "object-store": { [region]: true } },
    },
    // Trim BOTH keys — a pasted secret with a trailing newline would validate but break SigV4.
    secret: { adminAccessKey: f.adminAccessKey.trim(), adminSecretKey: f.adminSecretKey.trim() },
  }
}

function CephProviderForm({ form, setForm, canTest }: { form: CephFormState; setForm: (f: CephFormState) => void; canTest: boolean }) {
  // The answer belongs to the values it tested, so it goes when they change.
  const [tested, setTested] = useState<{ form: CephFormState; error?: string } | null>(null)
  const [testing, setTesting] = useState(false)
  const result = tested?.form === form ? tested : null
  const test = async () => {
    setTesting(true)
    const { config, secret } = cephFormToBody(form)
    try {
      await apiFetch("/admin/service/object-store/test", {
        method: "POST",
        body: { s3Endpoint: config.s3Endpoint, adminApiUrl: config.adminApiUrl, region: config.region, ...secret },
      })
      setTested({ form })
    } catch (e) {
      setTested({ form, error: e instanceof Error ? e.message : String(e) })
    } finally {
      setTesting(false)
    }
  }
  return (
    <div className="grid gap-4">
      <div className="grid gap-2">
        <Label htmlFor="cs-name">Display name</Label>
        <Input id="cs-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="ceph-s3" />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="cs-s3">S3 endpoint</Label>
        <Input id="cs-s3" className="font-mono" value={form.s3Endpoint} onChange={(e) => setForm({ ...form, s3Endpoint: e.target.value })} placeholder="https://s3.example.com" />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="cs-admin">Admin Ops URL</Label>
        <Input id="cs-admin" className="font-mono" value={form.adminApiUrl} onChange={(e) => setForm({ ...form, adminApiUrl: e.target.value })} placeholder="https://s3.example.com/admin" />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="cs-web">Website endpoint (optional)</Label>
        <Input id="cs-web" className="font-mono" value={form.s3WebsiteEndpoint} onChange={(e) => setForm({ ...form, s3WebsiteEndpoint: e.target.value })} placeholder="https://s3-website.example.com" />
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="cs-region">Region (RGW zonegroup)</Label>
          <Input id="cs-region" value={form.region} onChange={(e) => setForm({ ...form, region: e.target.value })} placeholder="us-east-1" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="cs-prefix">RGW uid prefix (optional)</Label>
          <Input id="cs-prefix" value={form.uidPrefix} onChange={(e) => setForm({ ...form, uidPrefix: e.target.value })} placeholder="prod_" />
        </div>
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="cs-key">Admin access key</Label>
          <Input id="cs-key" className="font-mono" value={form.adminAccessKey} onChange={(e) => setForm({ ...form, adminAccessKey: e.target.value })} autoComplete="off" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="cs-secret">Admin secret key</Label>
          <Input
            id="cs-secret"
            type="password"
            value={form.adminSecretKey}
            onChange={(e) => setForm({ ...form, adminSecretKey: e.target.value })}
            autoComplete="new-password"
          />
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="cs-quota">Default per-project quota (GiB, optional)</Label>
        <Input id="cs-quota" value={form.defaultQuotaGiB} onChange={(e) => setForm({ ...form, defaultQuotaGiB: e.target.value })} placeholder="100" />
      </div>
      <p className="text-xs text-muted-foreground">
        The admin keys belong to an RGW admin user with caps <code>users=*;buckets=*;usage=*</code>.
      </p>
      {canTest ? (
        <div className="flex items-center gap-3">
          <Button type="button" variant="outline" onClick={() => void test()} disabled={testing || !cephFormValid({ ...form, name: form.name || "-" })}>
            {testing ? "Testing…" : "Test connection"}
          </Button>
          {result && !result.error ? <span className="text-xs text-emerald-600">Connection OK</span> : null}
          {result?.error ? <span className="text-xs text-destructive">{result.error}</span> : null}
        </div>
      ) : null}
    </div>
  )
}

export default function CloudProvidersPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const { data, isLoading, error } = useAdminList<CloudProvider>(LIST_PATH)
  const items = data?.data ?? []

  const [createOpen, setCreateOpen] = useState(false)
  const [kind, setKind] = useState<"ceph-s3" | "rustfs" | "truenas" | "powerdns" | "kamaji" | "dbaas" | "proxmox">("proxmox")
  const [powerDNSForm, setPowerDNSForm] = useState<PowerDNSFormState>(emptyPowerDNSForm)
  const [trueNASForm, setTrueNASForm] = useState<TrueNASFormState>(emptyTrueNASForm)
  const [cephForm, setCephForm] = useState<CephFormState>(emptyCephForm)
  const [kamajiForm, setKamajiForm] = useState<KamajiFormState>(emptyKamajiForm)
  const [dbaasForm, setDbaasForm] = useState<DbaasFormState>(emptyDbaasForm)
  const [proxmoxForm, setProxmoxForm] = useState<ProxmoxFormState>(emptyProxmoxForm)

  const create = useMutation({
    // POST /admin/service (externalServiceCreate). The operator finishes the Services/Features tabs on
    // the detail page (it has its own "Test connection") — so we go straight there on success.
    mutationFn: () =>
      apiFetch<CloudProvider>(LIST_PATH, {
        method: "POST",
        body:
          kind === "ceph-s3" || kind === "rustfs"
            ? cephFormToBody(cephForm, kind)
            : kind === "truenas"
              ? trueNASFormToBody(trueNASForm)
            : kind === "powerdns"
              ? powerDNSFormToBody(powerDNSForm)
            : kind === "kamaji"
              ? kamajiFormToBody(kamajiForm)
              : kind === "dbaas"
                ? dbaasFormToBody(dbaasForm)
                : proxmoxFormToBody(proxmoxForm),
      }),
    onSuccess: (created) => {
      toast.success("Cloud provider created")
      setCreateOpen(false)
      setCephForm(emptyCephForm)
      setKamajiForm(emptyKamajiForm)
      setDbaasForm(emptyDbaasForm)
      setProxmoxForm(emptyProxmoxForm)
      void qc.invalidateQueries({ queryKey: ["admin-list", LIST_PATH] })
      if (created?.id) navigate(`/system/cloud-providers/${created.id}`)
    },
    // Surface the API message (validation / in-use errors) so the operator sees why it did not persist.
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudProvider, any>[]>(
    () => [
      {
        id: "id",
        accessorFn: (p) => p.id,
        header: sortableHeader("ID"),
        cell: ({ getValue }) => <span className="font-mono text-xs">{getValue()}</span>,
      },
      {
        id: "name",
        accessorFn: (p) => p.name ?? "",
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue() || "—"}</span>,
      },
      {
        id: "type",
        accessorFn: (p) => p.type ?? "",
        header: sortableHeader("Type"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue() || "—"}</span>,
      },
      {
        id: "endpoint",
        accessorFn: (p) => p.config?.apiUrl ?? p.config?.s3Endpoint ?? "",
        header: "Endpoint",
        cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() || "—"}</span>,
      },
      {
        id: "regions",
        accessorFn: (p) => providerRegions(p).join(", "),
        header: "Regions",
        enableSorting: false,
        cell: ({ row }) => (
          <div className="flex flex-wrap gap-1">
            {providerRegions(row.original).map((r) => (
              <Badge key={r} variant="outline">{r}</Badge>
            ))}
          </div>
        ),
      },
      {
        id: "status",
        accessorFn: (p) => p.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ row }) => <StatusBadge status={row.original.status} />,
      },
    ],
    // useState setters are stable; helpers are module-scope.
    [],
  )

  const addBtn = (
    <Button size="sm" onClick={() => setCreateOpen(true)}>
      <Plus className="size-4" /> Add provider
    </Button>
  )

  return (
    <>
      <PageHeader
        title="Cloud providers"
        eyebrow="System"
        description="External services connected to the platform."
        actions={addBtn}
      />
      {!isLoading && !error && items.length === 0 ? (
        <EmptyState
          icon={Cloud}
          title="No cloud providers"
          hint="Connect a Proxmox VE cluster (or a Ceph S3 object store) so projects can provision resources."
          action={addBtn}
        />
      ) : (
        <DataTable
          columns={columns}
          data={items}
          isLoading={isLoading}
          error={error as Error | null}
          searchPlaceholder="Search providers…"
          getRowId={(p) => p.id}
          onRowClick={(p) => navigate(`/system/cloud-providers/${p.id}`)}
        />
      )}

      <Dialog
        open={createOpen}
        onOpenChange={(o) => {
          setCreateOpen(o)
          // Clear ALL forms on every close (Cancel, Esc, overlay) — the ceph form holds admin keys
          // and the kamaji/dbaas forms a kubeconfig; none must sit in state and re-appear on next open.
          if (!o) {
                  setCephForm(emptyCephForm)
            setKamajiForm(emptyKamajiForm)
            setDbaasForm(emptyDbaasForm)
            setProxmoxForm(emptyProxmoxForm)
            setPowerDNSForm(emptyPowerDNSForm)
            setKind("proxmox")
          }
        }}
      >
        <DialogContent className="max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Add cloud provider</DialogTitle>
            <DialogDescription>
              {kind === "ceph-s3"
                ? "Connect a Ceph RGW object store over its S3 and Admin Ops endpoints. Projects get a dedicated RGW user."
                : kind === "powerdns"
                  ? "Connect a PowerDNS Authoritative server for tenant DNS zones, over its HTTP API."
                : kind === "truenas"
                  ? "Connect a TrueNAS for NFS and SMB file shares, over its WebSocket API. The key's account needs the Sharing Administrator group; Deleting shares also needs DATASET_DELETE, SMB shares ACCOUNT_WRITE."
                : kind === "rustfs"
                  ? "Connect a RustFS object store. The admin URL is its private endpoint: the public one must block /rustfs/admin. Projects get a dedicated IAM user; the keys are the RustFS root keys."
                : kind === "kamaji"
                  ? "Connect a Kamaji management cluster for Managed Kubernetes. Clusters are delivered as ArgoCD Applications of the pinned chart; worker nodes run in each customer's project."
                  : kind === "dbaas"
                    ? "Connect a pre-built database Kubernetes cluster for Managed Databases. Databases are delivered as ArgoCD Applications of the pinned chart, reachable over an internal LB in each customer's tenant network."
                    : "Connect a Proxmox VE cluster with one admin API token. Projects get a dedicated pool; networking, storage profiles and GPU mappings are configured on the provider page after creation."}
            </DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-3 gap-2">
            <Button variant={kind === "proxmox" ? "default" : "outline"} onClick={() => setKind("proxmox")}>
              Proxmox VE
            </Button>
            <Button variant={kind === "ceph-s3" ? "default" : "outline"} onClick={() => setKind("ceph-s3")}>
              Ceph S3
            </Button>
            <Button variant={kind === "rustfs" ? "default" : "outline"} onClick={() => setKind("rustfs")}>
              RustFS
            </Button>
            <Button variant={kind === "truenas" ? "default" : "outline"} onClick={() => setKind("truenas")}>
              TrueNAS
            </Button>
            <Button variant={kind === "powerdns" ? "default" : "outline"} onClick={() => setKind("powerdns")}>
              PowerDNS
            </Button>
            <Button variant={kind === "kamaji" ? "default" : "outline"} onClick={() => setKind("kamaji")}>
              Kubernetes
            </Button>
            <Button variant={kind === "dbaas" ? "default" : "outline"} onClick={() => setKind("dbaas")}>
              Database (DBaaS)
            </Button>
          </div>
          {kind === "powerdns" ? (
            <PowerDNSProviderForm form={powerDNSForm} setForm={setPowerDNSForm} />
          ) : kind === "truenas" ? (
            <TrueNASProviderForm form={trueNASForm} setForm={setTrueNASForm} />
          ) : kind === "ceph-s3" || kind === "rustfs" ? (
            <CephProviderForm form={cephForm} setForm={setCephForm} canTest={kind === "ceph-s3"} />
          ) : kind === "kamaji" ? (
            <KamajiProviderForm form={kamajiForm} setForm={setKamajiForm} />
          ) : kind === "dbaas" ? (
            <DbaasProviderForm form={dbaasForm} setForm={setDbaasForm} />
          ) : (
            <ProxmoxProviderForm form={proxmoxForm} setForm={setProxmoxForm} />
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button
              onClick={() => create.mutate()}
              disabled={
                (kind === "powerdns"
                  ? !powerDNSFormValid(powerDNSForm)
                  : kind === "truenas"
                  ? !trueNASFormValid(trueNASForm)
                  : kind === "ceph-s3" || kind === "rustfs"
                  ? !cephFormValid(cephForm)
                  : kind === "kamaji"
                    ? !kamajiFormValid(kamajiForm)
                    : kind === "dbaas"
                      ? !dbaasFormValid(dbaasForm)
                      : !proxmoxFormValid(proxmoxForm)) ||
                create.isPending
              }
            >
              {create.isPending ? "Creating…" : "Create provider"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
