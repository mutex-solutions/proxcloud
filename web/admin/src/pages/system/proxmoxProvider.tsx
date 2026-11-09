// proxmoxProvider.tsx — the shape and the form of a `proxmox` provider, shared by the
// Add-provider dialog and the provider detail page so the two can never drift.
//
// config.provider === "proxmox": one Proxmox VE cluster, driven with ONE admin API token;
// tenancy is enforced platform-side (project pool + DB ownership rows). The token secret is
// write-only — the API strips secrets on read, so edits leave it blank to keep the stored one.

import { useState } from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"

export type ProxmoxFormState = {
  name: string
  region: string
  apiUrl: string
  tokenId: string
  tokenSecret: string
  fingerprint: string
  // Failover: other nodes or a VIP, comma or newline separated, with the cluster CA they all chain to
  apiUrls: string
  caCert: string
  insecureTls: boolean
  poolPrefix: string
  // config.network.isolation / .evpnController — the tenant network isolation model.
  isolation: PVEIsolation
  evpnController: string
  // config.network.blockedCidrs: extra networks tenants must not reach, one CIDR per line.
  blockedCidrs: string
  // Stored config carried through so Save never wipes keys this form does not own
  // (network, storageProfiles, templates, gpus, overcommit, metrics — later phases' tabs).
  storedConfig: Record<string, any>
}

// "shared": one SDN zone, cluster firewall rules DROP cross-tenant traffic (default, simplest).
// "evpn-per-project": every project gets its own EVPN zone (VRF) on top of the same DROP rules.
export type PVEIsolation = "shared" | "evpn-per-project"

export const emptyProxmoxForm: ProxmoxFormState = {
  name: "",
  region: "pve",
  apiUrl: "",
  tokenId: "",
  tokenSecret: "",
  fingerprint: "",
  apiUrls: "",
  caCert: "",
  insecureTls: false,
  poolPrefix: "pc-",
  isolation: "shared",
  evpnController: "",
  blockedCidrs: "",
  storedConfig: {},
}

// proxmoxNetworkConfig merges the form-owned isolation keys into the stored config.network
// (the update route replaces top-level config keys wholesale, so the rest must ride along).
export function proxmoxNetworkConfig(f: ProxmoxFormState) {
  const network: Record<string, any> = { ...((f.storedConfig?.network as Record<string, any>) ?? {}), isolation: f.isolation }
  if (f.isolation === "evpn-per-project" && f.evpnController.trim()) network.evpnController = f.evpnController.trim()
  else delete network.evpnController
  const blocked = f.blockedCidrs.split(/[\s,]+/).filter(Boolean)
  if (blocked.length) network.blockedCidrs = blocked
  else delete network.blockedCidrs
  return network
}

export const proxmoxFormValid = (f: ProxmoxFormState, requireSecret = true) => {
  const required = [f.name, f.region, f.apiUrl, f.tokenId]
  if (requireSecret) required.push(f.tokenSecret)
  return required.every((v) => v.trim() !== "")
}

// Keys this form owns; everything else under config passes through untouched.
const FORM_OWNED_KEYS = ["provider", "region", "regions", "services", "apiUrl", "apiUrls", "caCert", "tokenId", "fingerprint", "insecureTls", "poolPrefix"]

export function proxmoxFormToBody(f: ProxmoxFormState) {
  const region = f.region.trim()
  const passthrough = { ...(f.storedConfig ?? {}) }
  for (const k of FORM_OWNED_KEYS) delete passthrough[k]
  const enabled = { [region]: true }
  return {
    name: f.name.trim(),
    type: "CLOUD",
    status: "PUBLIC",
    config: {
      ...passthrough,
      provider: "proxmox",
      region,
      regions: { [region]: { name: region, country: "", displayName: region } },
      // The families a Proxmox cluster serves — enabled directly, no discovery round-trip.
      services: { compute: enabled, network: enabled, volumev3: enabled, image: enabled },
      apiUrl: f.apiUrl.trim().replace(/\/+$/, ""),
      tokenId: f.tokenId.trim(),
      fingerprint: f.fingerprint.trim(),
      apiUrls: f.apiUrls.split(/[\s,]+/).map((u) => u.trim().replace(/\/+$/, "")).filter(Boolean),
      caCert: f.caCert.trim(),
      insecureTls: f.insecureTls,
      poolPrefix: f.poolPrefix.trim() || "pc-",
      network: proxmoxNetworkConfig(f),
      metrics: (f.storedConfig?.metrics as Record<string, any>) ?? { source: "pve" },
    },
    // Secret merge skips blank values server-side, so an empty tokenSecret keeps the stored one.
    secret: f.tokenSecret.trim() ? { tokenSecret: f.tokenSecret.trim() } : {},
  }
}

export function proxmoxFormFromService(svc: { name?: string; config?: Record<string, any> }): ProxmoxFormState {
  const cfg = svc.config ?? {}
  const regions = Object.keys((cfg.regions as Record<string, unknown>) ?? {})
  return {
    ...emptyProxmoxForm,
    name: svc.name ?? "",
    region: String(cfg.region ?? regions[0] ?? "pve"),
    apiUrl: String(cfg.apiUrl ?? ""),
    tokenId: String(cfg.tokenId ?? ""),
    tokenSecret: "", // never returned by the API; blank = keep
    fingerprint: String(cfg.fingerprint ?? ""),
    apiUrls: ((cfg.apiUrls as string[]) ?? []).join("\n"),
    caCert: String(cfg.caCert ?? ""),
    insecureTls: !!cfg.insecureTls,
    poolPrefix: String(cfg.poolPrefix ?? "pc-"),
    isolation: cfg.network?.isolation === "evpn-per-project" ? "evpn-per-project" : "shared",
    evpnController: String(cfg.network?.evpnController ?? ""),
    blockedCidrs: ((cfg.network?.blockedCidrs as string[]) ?? []).join("\n"),
    storedConfig: cfg,
  }
}

type PVEAuthResponse = {
  version?: string
  nodes?: { node: string; status: string }[]
  storages?: { storage: string; type: string }[]
  sdnZones?: { zone: string; type: string }[]
  pciMappings?: { id: string }[]
  warnings?: string[]
}

export function ProxmoxProviderForm({
  form, setForm, serviceId, mode = "create",
}: {
  form: ProxmoxFormState
  setForm: (f: ProxmoxFormState) => void
  // On the detail page: lets Test reuse the stored secret when the field is blank.
  serviceId?: string
  mode?: "create" | "edit"
}) {
  const edit = mode === "edit"
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<string | null>(null)
  const [testError, setTestError] = useState<string | null>(null)
  const [testWarnings, setTestWarnings] = useState<string[]>([])

  const test = async () => {
    setTesting(true)
    setTestResult(null)
    setTestError(null)
    setTestWarnings([])
    try {
      const q = serviceId && !form.tokenSecret.trim() ? `?externalServiceId=${encodeURIComponent(serviceId)}` : ""
      const res = await apiFetch<PVEAuthResponse>(`/admin/service/proxmox/auth${q}`, {
        method: "POST",
        body: {
          apiUrl: form.apiUrl.trim(),
          tokenId: form.tokenId.trim(),
          tokenSecret: form.tokenSecret.trim(),
          fingerprint: form.fingerprint.trim(),
          caCert: form.caCert.trim(),
          insecureTls: form.insecureTls,
        },
      })
      setTestWarnings(res.warnings ?? [])
      const nodes = res.nodes?.length ?? 0
      const storages = res.storages?.length ?? 0
      const gpus = res.pciMappings?.length ?? 0
      setTestResult(
        `Proxmox VE ${res.version ?? "?"} — ${nodes} node${nodes === 1 ? "" : "s"}, ${storages} storage${storages === 1 ? "" : "s"}${gpus ? `, ${gpus} PCI mapping${gpus === 1 ? "" : "s"}` : ""}`,
      )
    } catch (e) {
      setTestError(e instanceof Error ? e.message : String(e))
    } finally {
      setTesting(false)
    }
  }

  return (
    <div className="grid gap-4">
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="pve-name">Display name</Label>
          <Input id="pve-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Proxmox Lab" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="pve-region">Region</Label>
          <Input id="pve-region" value={form.region} onChange={(e) => setForm({ ...form, region: e.target.value })} placeholder="pve" disabled={edit} />
          <p className="text-xs text-muted-foreground">One Proxmox cluster = one region.</p>
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="pve-url">API URL</Label>
        <Input id="pve-url" className="font-mono" value={form.apiUrl} onChange={(e) => setForm({ ...form, apiUrl: e.target.value })} placeholder="https://pve1.lab:8006" autoComplete="off" />
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="pve-tokenid">API token ID</Label>
          <Input id="pve-tokenid" className="font-mono" value={form.tokenId} onChange={(e) => setForm({ ...form, tokenId: e.target.value })} placeholder="proxcloud@pve!platform" autoComplete="off" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="pve-secret">Token secret{edit ? " (blank keeps the stored one)" : ""}</Label>
          <Input id="pve-secret" className="font-mono" type="password" value={form.tokenSecret} onChange={(e) => setForm({ ...form, tokenSecret: e.target.value })} autoComplete="new-password" />
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="pve-urls">Other API URLs for failover (other nodes or a VIP, optional)</Label>
        <Textarea id="pve-urls" className="font-mono" rows={2} value={form.apiUrls} onChange={(e) => setForm({ ...form, apiUrls: e.target.value })} placeholder="https://pve2.example:8006" />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="pve-ca">Cluster CA (PEM, /etc/pve/pve-root-ca.pem; needed with several URLs)</Label>
        <Textarea id="pve-ca" className="font-mono" rows={3} value={form.caCert} onChange={(e) => setForm({ ...form, caCert: e.target.value })} placeholder="-----BEGIN CERTIFICATE-----" />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="pve-fp">TLS certificate fingerprint (SHA-256, optional)</Label>
        <Input id="pve-fp" className="font-mono" value={form.fingerprint} onChange={(e) => setForm({ ...form, fingerprint: e.target.value })} placeholder="AA:BB:CC:…" autoComplete="off" />
        <p className="text-xs text-muted-foreground">
          Pin a self-signed cluster cert (<code>pvenode cert info</code> prints it). Empty = system CA.
        </p>
      </div>
      <div className="flex items-center justify-between rounded-lg border p-3">
        <div>
          <Label htmlFor="pve-insecure" className="text-sm font-medium">Skip TLS verification</Label>
          <div className="text-xs text-muted-foreground">Last resort — prefer the fingerprint pin.</div>
        </div>
        <Switch id="pve-insecure" checked={form.insecureTls} onCheckedChange={(on) => setForm({ ...form, insecureTls: on })} />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="pve-pool">Pool prefix</Label>
        <Input id="pve-pool" className="font-mono" value={form.poolPrefix} onChange={(e) => setForm({ ...form, poolPrefix: e.target.value })} placeholder="pc-" />
        <p className="text-xs text-muted-foreground">
          Every project gets one PVE pool (<code>{form.poolPrefix || "pc-"}&lt;projectId&gt;</code>) — the tenancy boundary.
        </p>
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="pve-isolation">Tenant network isolation</Label>
          <Select value={form.isolation} onValueChange={(v) => setForm({ ...form, isolation: v as PVEIsolation })}>
            <SelectTrigger id="pve-isolation">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="shared">Shared zone (default)</SelectItem>
              <SelectItem value="evpn-per-project">EVPN zone per project</SelectItem>
            </SelectContent>
          </Select>
          <p className="text-xs text-muted-foreground">
            Both drop cross-tenant traffic with cluster firewall rules inside the network <code>cidrPool</code> (required).
            Needs the nftables <code>proxmox-firewall</code> on every node.
          </p>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="pve-blocked">Networks tenants must not reach</Label>
          <Textarea id="pve-blocked" className="font-mono" rows={2} value={form.blockedCidrs} onChange={(e) => setForm({ ...form, blockedCidrs: e.target.value })} placeholder="192.0.2.0/24" autoComplete="off" />
          <p className="text-xs text-muted-foreground">
            One CIDR per line, on top of the nodes&apos; management networks, which are always blocked. List storage and office networks a server could otherwise reach through an exit node.
          </p>
        </div>
        {form.isolation === "evpn-per-project" ? (
          <div className="grid gap-2">
            <Label htmlFor="pve-evpnctl">EVPN controller</Label>
            <Input id="pve-evpnctl" className="font-mono" value={form.evpnController} onChange={(e) => setForm({ ...form, evpnController: e.target.value })} placeholder="proxcloud" autoComplete="off" />
            <p className="text-xs text-muted-foreground">
              Shared by every project zone. Blank = the default zone&apos;s controller; zone settings (ASN, peers, exit nodes) come from the default zone.
            </p>
          </div>
        ) : null}
      </div>
      <div className="flex items-center gap-3">
        <Button type="button" variant="outline" onClick={() => void test()} disabled={testing || !form.apiUrl.trim() || !form.tokenId.trim() || (!form.tokenSecret.trim() && !serviceId)}>
          {testing ? "Testing…" : "Test connection"}
        </Button>
        {testResult ? <span className="text-xs text-emerald-600">{testResult}</span> : null}
        {testError ? <span className="text-xs text-destructive">{testError}</span> : null}
      </div>
      {testWarnings.length ? (
        <ul className="list-disc space-y-1 pl-5 text-xs text-amber-600">
          {testWarnings.map((w) => <li key={w}>{w}</li>)}
        </ul>
      ) : null}
      <p className="text-xs text-muted-foreground">
        Use a privilege-separated token with the ProxCloudPlatform role (docs/proxmox-permissions.md);
        Test connection lists any privilege it lacks. Networking zones,
        storage profiles, templates and GPU mappings are configured on the provider detail page after creation.
      </p>
    </div>
  )
}
