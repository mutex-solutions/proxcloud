// kamajiProvider.tsx — the shape and the form of a `kamaji` Managed Kubernetes provider, shared by
// the Add-provider dialog and the provider detail page so the two can never drift.
//
// config.provider === "kamaji": customer clusters are ArgoCD Applications of the pinned kamaji
// cluster chart, applied to the provider's Kamaji MANAGEMENT cluster; the secret is a kubeconfig for
// a proxcloud-scoped service account there. Worker VMs, and everything they cost, land in the
// project's own compute service — so a project needs BOTH this service and a Proxmox one.
// TODO(P7): CAPMOX worker provisioning — cluster create is pending on Proxmox until it lands.

import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"

export type KamajiFormState = {
  name: string
  region: string
  kubeconfig: string
  chartRepo: string
  chartName: string
  chartVersion: string
  argoNamespace: string
  argoProject: string
  dataStoreName: string
  dnsZone: string
  rootVolumeGiB: string
  allowedCidrs: string // comma-separated
  versions: string // one "1.35.4=<node-image-id>" (or "1.35.4@variant=<id>") per line
  flavors: string // optional allowlist, one flavor id per line (empty = all flavors)
}

export const emptyKamajiForm: KamajiFormState = {
  name: "",
  region: "az1",
  kubeconfig: "",
  chartRepo: "ghcr.io/mutex-solutions/charts",
  chartName: "",
  chartVersion: "",
  argoNamespace: "argocd",
  argoProject: "proxcloud-k8s",
  dataStoreName: "default",
  dnsZone: "",
  rootVolumeGiB: "120",
  allowedCidrs: "",
  versions: "",
  flavors: "",
}

// splitLines is the shared reader for the one-per-line textarea fields.
const splitLines = (s: string) => s.split("\n").map((v) => v.trim()).filter(Boolean)

// parseVersions turns the versions textarea into the config.cluster {versions, imageVariants}
// maps. Line grammar: `1.35.4=<image-id>` (the version's default image) or
// `1.35.4@nvidia=<image-id>` (a named variant of that version — GPU builds etc.).
// null = malformed: bad line shape, a non-numeric version (the upgrade path parses
// major.minor[.patch] — a suffix like "1.35.4-nvidia" would brick upgrades, hence the variant
// syntax), a bad variant name, or a variant whose version has no default line.
export type ParsedVersions = {
  versions: Record<string, string>
  variants: Record<string, Record<string, string>> // variant → version → image id
}
const VERSION_RE = /^\d+\.\d+(\.\d+)?$/
const VARIANT_RE = /^[a-z0-9][a-z0-9-]*$/
export function parseVersions(raw: string): ParsedVersions | null {
  const versions: Record<string, string> = {}
  // Null-prototype: a variant named like an Object.prototype property ("constructor") must land
  // in the map, not resolve to the inherited value and silently vanish from the config.
  const variants: Record<string, Record<string, string>> = Object.create(null)
  for (const line of raw.split("\n")) {
    const t = line.trim()
    if (!t) continue
    const eq = t.indexOf("=")
    if (eq <= 0 || eq === t.length - 1) return null
    const key = t.slice(0, eq).trim()
    const img = t.slice(eq + 1).trim()
    const at = key.indexOf("@")
    if (at < 0) {
      if (!VERSION_RE.test(key)) return null
      versions[key] = img
    } else {
      const version = key.slice(0, at).trim()
      const variant = key.slice(at + 1).trim()
      if (!VERSION_RE.test(version) || !VARIANT_RE.test(variant)) return null
      ;(variants[variant] ??= {})[version] = img
    }
  }
  // A variant of an unlisted version would be unreachable (versions gate the offering).
  for (const byVersion of Object.values(variants)) {
    for (const version of Object.keys(byVersion)) {
      if (!versions[version]) return null
    }
  }
  return { versions, variants }
}

const splitCsv = (s: string) => s.split(",").map((v) => v.trim()).filter(Boolean)

export const kamajiFormValid = (f: KamajiFormState, requireKubeconfig = true) => {
  const parsed = parseVersions(f.versions)
  const required = [f.name, f.region, f.chartRepo, f.chartName, f.chartVersion]
  if (requireKubeconfig) required.push(f.kubeconfig)
  return (
    required.every((v) => v.trim() !== "") &&
    Number(f.rootVolumeGiB) >= 1 &&
    parsed !== null &&
    Object.keys(parsed.versions).length > 0
  )
}

// kamajiConfigBlocks are the two config sub-objects proxcloud reads. The admin update route replaces
// top-level config keys wholesale, so each block must always be sent complete.
export function kamajiConfigBlocks(f: KamajiFormState) {
  return {
    argocd: {
      namespace: f.argoNamespace.trim() || "argocd",
      project: f.argoProject.trim() || "default",
      chartRepo: f.chartRepo.trim(),
      chartName: f.chartName.trim(),
      chartVersion: f.chartVersion.trim(),
    },
    cluster: {
      ...(f.dataStoreName.trim() ? { dataStoreName: f.dataStoreName.trim() } : {}),
      ...(f.dnsZone.trim() ? { dnsZone: f.dnsZone.trim() } : {}),
      ...(Number(f.rootVolumeGiB) > 0 ? { rootVolumeGiB: Number(f.rootVolumeGiB) } : {}),
      ...(splitCsv(f.allowedCidrs).length ? { allowedCidrs: splitCsv(f.allowedCidrs) } : {}),
      // Omitted entirely when empty — an absent key means "offer all flavors".
      ...(splitLines(f.flavors).length ? { flavors: splitLines(f.flavors) } : {}),
      versions: parseVersions(f.versions)?.versions ?? {},
      // Sent complete on every save (the update route replaces whole blocks) — including the
      // empty {} so deleting the last variant line actually removes the stored variants.
      imageVariants: parseVersions(f.versions)?.variants ?? {},
    },
  }
}

export function kamajiFormToBody(f: KamajiFormState) {
  const region = f.region.trim()
  return {
    name: f.name.trim(),
    type: "CLOUD",
    status: "PUBLIC",
    config: {
      provider: "kamaji",
      regions: { [region]: { name: region, country: "", displayName: region } },
      // kubernetes is the ONLY service a kamaji provider serves — enabled directly, no discovery.
      services: { kubernetes: { [region]: true } },
      ...kamajiConfigBlocks(f),
    },
    secret: { kubeconfig: f.kubeconfig.trim() },
  }
}

// kamajiFormFromService fills the form from a stored provider so the detail page can edit it. The
// kubeconfig is never returned by the API (secrets are stripped) — it stays blank and is only sent
// when the operator types a new one.
export function kamajiFormFromService(svc: {
  name?: string
  config?: Record<string, any>
}): KamajiFormState {
  const cfg = svc.config ?? {}
  const argo = (cfg.argocd as Record<string, any>) ?? {}
  const cluster = (cfg.cluster as Record<string, any>) ?? {}
  const regions = Object.keys((cfg.regions as Record<string, unknown>) ?? {})
  const versions = (cluster.versions as Record<string, string>) ?? {}
  const imageVariants = (cluster.imageVariants as Record<string, Record<string, string>>) ?? {}
  const versionLines = [
    ...Object.entries(versions).map(([v, img]) => `${v}=${img}`),
    ...Object.entries(imageVariants).flatMap(([variant, byVersion]) =>
      Object.entries(byVersion).map(([v, img]) => `${v}@${variant}=${img}`)),
  ]
  return {
    ...emptyKamajiForm,
    name: svc.name ?? "",
    region: String(cfg.region ?? regions[0] ?? ""),
    kubeconfig: "",
    chartRepo: String(argo.chartRepo ?? emptyKamajiForm.chartRepo),
    chartName: String(argo.chartName ?? ""),
    chartVersion: String(argo.chartVersion ?? ""),
    argoNamespace: String(argo.namespace ?? "argocd"),
    argoProject: String(argo.project ?? "default"),
    dataStoreName: String(cluster.dataStoreName ?? ""),
    dnsZone: String(cluster.dnsZone ?? ""),
    rootVolumeGiB: String(cluster.rootVolumeGiB ?? emptyKamajiForm.rootVolumeGiB),
    allowedCidrs: ((cluster.allowedCidrs as string[]) ?? []).join(","),
    versions: versionLines.join("\n"),
    flavors: ((cluster.flavors as string[]) ?? []).join("\n"),
  }
}

export function KamajiProviderForm({
  form, setForm, mode = "create",
}: {
  form: KamajiFormState
  setForm: (f: KamajiFormState) => void
  // "edit" hides the fields that cannot change after creation and makes the kubeconfig optional.
  mode?: "create" | "edit"
}) {
  const edit = mode === "edit"
  return (
    <div className="grid gap-4">
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="km-name">Display name</Label>
          <Input id="km-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Managed Kubernetes AZ1" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="km-region">Region</Label>
          <Input id="km-region" value={form.region} onChange={(e) => setForm({ ...form, region: e.target.value })} placeholder="az1" disabled={edit} />
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="km-kubeconfig">
          Management-cluster kubeconfig{edit ? " (leave blank to keep the current one)" : ""}
        </Label>
        <Textarea
          id="km-kubeconfig"
          className="min-h-28 font-mono text-xs"
          value={form.kubeconfig}
          onChange={(e) => setForm({ ...form, kubeconfig: e.target.value })}
          placeholder="apiVersion: v1&#10;kind: Config&#10;…"
          autoComplete="off"
        />
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="km-repo">Chart OCI repo</Label>
          <Input id="km-repo" className="font-mono" value={form.chartRepo} onChange={(e) => setForm({ ...form, chartRepo: e.target.value })} placeholder="ghcr.io/mutex-solutions/charts" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="km-chartname">Chart name</Label>
          <Input id="km-chartname" className="font-mono" value={form.chartName} onChange={(e) => setForm({ ...form, chartName: e.target.value })} placeholder="kamaji-cluster" />
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="km-chartver">Chart version (pinned)</Label>
        <Input id="km-chartver" className="font-mono" value={form.chartVersion} onChange={(e) => setForm({ ...form, chartVersion: e.target.value })} placeholder="x.y.z" />
        <p className="text-xs text-muted-foreground">
          The cluster chart version NEW clusters pin (existing clusters keep theirs). Published
          versions: the chart registry above, or the chart repo&apos;s release tags.
        </p>
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="km-argons">ArgoCD namespace</Label>
          <Input id="km-argons" value={form.argoNamespace} onChange={(e) => setForm({ ...form, argoNamespace: e.target.value })} placeholder="argocd" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="km-argoproj">ArgoCD AppProject</Label>
          <Input id="km-argoproj" value={form.argoProject} onChange={(e) => setForm({ ...form, argoProject: e.target.value })} placeholder="proxcloud-k8s" />
        </div>
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="km-datastore">Kamaji DataStore</Label>
          <Input id="km-datastore" value={form.dataStoreName} onChange={(e) => setForm({ ...form, dataStoreName: e.target.value })} placeholder="default" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="km-dns">DNS zone (optional)</Label>
          <Input id="km-dns" value={form.dnsZone} onChange={(e) => setForm({ ...form, dnsZone: e.target.value })} placeholder="k8s.example.com" />
        </div>
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="km-rootdisk">Default node disk (GiB)</Label>
          <Input id="km-rootdisk" type="number" min={1} value={form.rootVolumeGiB} onChange={(e) => setForm({ ...form, rootVolumeGiB: e.target.value })} />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="km-cidrs">Default API allowlist (CIDRs, comma-separated)</Label>
          <Input id="km-cidrs" className="font-mono" value={form.allowedCidrs} onChange={(e) => setForm({ ...form, allowedCidrs: e.target.value })} placeholder="10.0.0.0/8,203.0.113.4/32" />
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="km-flavors">Flavor allowlist (optional, one flavor id per line)</Label>
        {/* IDs, not names: the create wizard sends the flavor's id and the backend matches on it. */}
        <Textarea
          id="km-flavors"
          className="min-h-20 font-mono text-xs"
          value={form.flavors}
          onChange={(e) => setForm({ ...form, flavors: e.target.value })}
          placeholder="gp.small&#10;gpu.4090"
        />
        <p className="text-xs text-muted-foreground">
          Flavor <em>ids</em> from the Proxmox flavor catalog, not display names. Only these are
          offered for worker pools; empty = every flavor.
        </p>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="km-versions">Kubernetes versions (one “version=node-image-id” per line)</Label>
        <Textarea
          id="km-versions"
          className="min-h-20 font-mono text-xs"
          value={form.versions}
          onChange={(e) => setForm({ ...form, versions: e.target.value })}
          placeholder={"1.35.4=<template/image id>\n1.35.4@nvidia=<template/image id>"}
        />
        <p className="text-xs text-muted-foreground">
          <code>1.35.4=&lt;template/image id&gt;</code> sets the version's default node image. Add image
          <em> variants</em> of the same version — a GPU build, say — as
          <code> 1.35.4@nvidia=&lt;template/image id&gt;</code>; customers pick a variant per node group and
          upgrades keep each pool on its variant. A variant needs its version's default line too.
        </p>
      </div>
      <p className="text-xs text-muted-foreground">
        The kubeconfig belongs to a proxcloud service account on the Kamaji management cluster (ArgoCD +
        AppProject installed there). Only versions listed here are offered to customers.
      </p>
    </div>
  )
}
