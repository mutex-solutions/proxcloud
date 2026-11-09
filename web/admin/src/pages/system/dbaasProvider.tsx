// dbaasProvider.tsx — the shape and the form of a `dbaas` Managed Database provider, shared by
// the Add-provider dialog and the provider detail page so the two can never drift.
//
// config.provider === "dbaas": customer databases are ArgoCD Applications of the pinned
// `database-cluster` chart, applied to an ops-pre-built Kubernetes cluster that runs the DB
// operator suite; the secret is a kubeconfig for a proxcloud-scoped service account there. The
// database endpoint is a LoadBalancer Service on that cluster whose address MetalLB hands out of
// the provider's address pool (a services subnet tenants reach over their normal egress),
// restricted by source ranges. Backups go to any S3 endpoint (RustFS, Ceph RGW, …).

import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"

export type DbaasFormState = {
  name: string
  region: string
  kubeconfig: string
  chartRepo: string
  chartVersion: string
  argoNamespace: string
  argoProject: string
  // MetalLB address pool every tenant-facing database LoadBalancer draws its address from.
  lbAddressPool: string
  // Topology key the replicas of one database spread across (hard anti-affinity). Empty = the
  // chart default, topology.kubernetes.io/zone; a one-node site sets e.g. kubernetes.io/hostname.
  zoneKey: string
  // S3 object store every database's backups land in. Customers never choose it — they only
  // turn backups on. Empty bucket/endpoint = the whole backup surface stays hidden.
  backupEndpoint: string
  backupBucket: string
  backupRegion: string
  backupPrefix: string
  backupPathStyle: boolean
  backupCaSecret: string
  // Write-only, like the kubeconfig: blank on edit keeps the stored value.
  backupAccessKey: string
  backupSecretKey: string
  storageClasses: string // comma-separated
  engines: string // one "postgresql=16,17,18" line per engine
  valkeyEnabled: boolean // valkey is pre-GA — its line is rejected unless explicitly enabled
  // The stored config.database object as loaded from the service. The engines textarea only
  // round-trips versions, and the update route replaces top-level config keys wholesale — so
  // everything the form does not own (limits, per-engine default/replicas/beta, future keys)
  // must be carried here and merged back on save, or a plain Save would silently wipe it.
  storedDatabase: Record<string, any>
}

export const emptyDbaasForm: DbaasFormState = {
  name: "",
  region: "az1",
  kubeconfig: "",
  chartRepo: "ghcr.io/mutex-solutions/charts",
  chartVersion: "",
  argoNamespace: "argocd",
  argoProject: "proxcloud-dbaas",
  lbAddressPool: "",
  zoneKey: "",
  backupEndpoint: "",
  backupBucket: "",
  backupRegion: "",
  backupPrefix: "",
  backupPathStyle: true,
  backupCaSecret: "",
  backupAccessKey: "",
  backupSecretKey: "",
  storageClasses: "",
  engines: "",
  valkeyEnabled: false,
  storedDatabase: {},
}

export const DBAAS_ENGINES = ["postgresql", "mysql", "mariadb", "valkey", "ferretdb", "opensearch", "kafka"] as const

const splitCsv = (s: string) => s.split(",").map((v) => v.trim()).filter(Boolean)

// parseEngines turns the engines textarea into the config.database.engines map. Line grammar:
// `postgresql=16,17,18` with an optional replica-choice suffix `@1,3` (allowed instance counts
// 1..3; omitted = the server default of one instance). null = malformed (bad line shape,
// unknown engine, duplicate line, empty version list, replicas outside 1..3, or a valkey line
// while the beta switch is off).
export function parseEngines(raw: string, valkeyEnabled: boolean): Record<string, { versions: string[]; replicas?: number[] }> | null {
  const out: Record<string, { versions: string[]; replicas?: number[] }> = {}
  for (const line of raw.split("\n")) {
    const t = line.trim()
    if (!t) continue
    const eq = t.indexOf("=")
    if (eq <= 0 || eq === t.length - 1) return null
    const engine = t.slice(0, eq).trim()
    if (!(DBAAS_ENGINES as readonly string[]).includes(engine)) return null
    if (engine === "valkey" && !valkeyEnabled) return null
    if (out[engine]) return null
    const rest = t.slice(eq + 1)
    const at = rest.indexOf("@")
    const versions = splitCsv(at >= 0 ? rest.slice(0, at) : rest)
    if (versions.length === 0) return null
    let replicas: number[] | undefined
    if (at >= 0) {
      replicas = splitCsv(rest.slice(at + 1)).map(Number)
      if (replicas.length === 0 || replicas.some((n) => !Number.isInteger(n) || n < 1 || n > 3)) return null
      if (new Set(replicas).size !== replicas.length) return null
    }
    out[engine] = replicas ? { versions, replicas } : { versions }
  }
  return out
}

export const dbaasFormValid = (f: DbaasFormState, requireKubeconfig = true) => {
  const engines = parseEngines(f.engines, f.valkeyEnabled)
  const required = [f.name, f.region, f.chartRepo, f.chartVersion]
  if (requireKubeconfig) required.push(f.kubeconfig)
  return required.every((v) => v.trim() !== "") && engines !== null && Object.keys(engines).length > 0
}

// The database.* keys the form edits directly — everything else stored under config.database
// (limits, future keys) passes through dbaasConfigBlocks untouched.
const FORM_OWNED_DATABASE_KEYS = ["lbAddressPool", "storageClasses", "engines", "backup", "scheduling"]

// dbaasConfigBlocks are the two config sub-objects proxcloud reads. The admin update route
// replaces top-level config keys wholesale, so each block must always be sent complete —
// deep-merged over the stored config.database, because the textarea grammar only carries
// versions: per engine the parsed versions replace the stored ones, `default` is kept if
// still offered (else the first parsed version), and `replicas`/`beta`/any other stored
// per-engine keys survive. Engines removed from the textarea are removed.
export function dbaasConfigBlocks(f: DbaasFormState) {
  const stored = f.storedDatabase ?? {}
  const storedEngines = (stored.engines as Record<string, Record<string, any>>) ?? {}
  const engines: Record<string, any> = {}
  for (const [name, { versions, replicas }] of Object.entries(parseEngines(f.engines, f.valkeyEnabled) ?? {})) {
    const prev = storedEngines[name] ?? {}
    engines[name] = {
      ...prev,
      versions,
      // An explicit @suffix replaces the stored replica choices; without one they survive.
      ...(replicas ? { replicas } : {}),
      default: prev.default && versions.includes(prev.default) ? prev.default : versions[0],
      // valkey only parses with the beta switch on; the server refuses a beta engine without
      // the customer acknowledgement, so its offer must always carry the beta flag.
      ...(name === "valkey" ? { beta: true } : {}),
    }
  }
  const passthrough = { ...stored }
  for (const k of FORM_OWNED_DATABASE_KEYS) delete passthrough[k]
  return {
    argocd: {
      namespace: f.argoNamespace.trim() || "argocd",
      project: f.argoProject.trim() || "proxcloud-dbaas",
      chartRepo: f.chartRepo.trim(),
      chartName: "database-cluster",
      chartVersion: f.chartVersion.trim(),
    },
    database: {
      ...passthrough,
      lbAddressPool: f.lbAddressPool.trim(),
      backup: {
        endpoint: f.backupEndpoint.trim(),
        bucket: f.backupBucket.trim(),
        region: f.backupRegion.trim(),
        prefix: f.backupPrefix.trim(),
        pathStyle: f.backupPathStyle,
        caCertSecret: f.backupCaSecret.trim(),
      },
      // nodeSelector / tolerations (a dedicated database node pool) are not edited here — they
      // ride along from the stored document; only the zone spread key is the form's.
      scheduling: { ...((stored.scheduling as Record<string, any>) ?? {}), zoneKey: f.zoneKey.trim() },
      storageClasses: splitCsv(f.storageClasses),
      engines,
    },
  }
}

export function dbaasFormToBody(f: DbaasFormState) {
  const region = f.region.trim()
  return {
    name: f.name.trim(),
    type: "CLOUD",
    status: "PUBLIC",
    config: {
      provider: "dbaas",
      region,
      regions: { [region]: { name: region, country: "", displayName: region } },
      // database is the ONLY service a dbaas provider serves — enabled directly, no discovery.
      services: { database: { [region]: true } },
      ...dbaasConfigBlocks(f),
    },
    secret: {
      kubeconfig: f.kubeconfig.trim(),
      // Backup credentials are provider secrets like the kubeconfig: encrypted at rest and
      // stripped from admin reads. Absent keys simply mean "no object store yet".
      ...(f.backupAccessKey.trim() ? { backupAccessKey: f.backupAccessKey.trim() } : {}),
      ...(f.backupSecretKey.trim() ? { backupSecretKey: f.backupSecretKey.trim() } : {}),
    },
  }
}

// dbaasFormFromService fills the form from a stored provider so the detail page can edit it.
// The kubeconfig is never returned by the API (secrets are stripped) — it stays blank and is
// only sent when the operator pastes a new one.
export function dbaasFormFromService(svc: {
  name?: string
  config?: Record<string, any>
}): DbaasFormState {
  const cfg = svc.config ?? {}
  const argo = (cfg.argocd as Record<string, any>) ?? {}
  const database = (cfg.database as Record<string, any>) ?? {}
  const backup = (database.backup as Record<string, any>) ?? {}
  const regions = Object.keys((cfg.regions as Record<string, unknown>) ?? {})
  const engines = (database.engines as Record<string, { versions?: string[]; replicas?: number[] }>) ?? {}
  const engineLines = Object.entries(engines).map(
    ([e, o]) => `${e}=${(o?.versions ?? []).join(",")}${o?.replicas?.length ? `@${o.replicas.join(",")}` : ""}`,
  )
  return {
    ...emptyDbaasForm,
    name: svc.name ?? "",
    region: String(cfg.region ?? regions[0] ?? ""),
    kubeconfig: "",
    chartRepo: String(argo.chartRepo ?? emptyDbaasForm.chartRepo),
    chartVersion: String(argo.chartVersion ?? ""),
    argoNamespace: String(argo.namespace ?? "argocd"),
    argoProject: String(argo.project ?? "proxcloud-dbaas"),
    lbAddressPool: String(database.lbAddressPool ?? ""),
    zoneKey: String((database.scheduling as Record<string, any> | undefined)?.zoneKey ?? ""),
    backupEndpoint: String(backup.endpoint ?? ""),
    backupBucket: String(backup.bucket ?? ""),
    backupRegion: String(backup.region ?? ""),
    backupPrefix: String(backup.prefix ?? ""),
    backupPathStyle: backup.pathStyle !== false,
    backupCaSecret: String(backup.caCertSecret ?? ""),
    storageClasses: ((database.storageClasses as string[]) ?? []).join(","),
    engines: engineLines.join("\n"),
    valkeyEnabled: !!engines.valkey,
    storedDatabase: database,
  }
}

export function DbaasProviderForm({
  form, setForm, mode = "create",
}: {
  form: DbaasFormState
  setForm: (f: DbaasFormState) => void
  // "edit" hides the fields that cannot change after creation and makes the kubeconfig optional.
  mode?: "create" | "edit"
}) {
  const edit = mode === "edit"
  return (
    <div className="grid gap-4">
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="db-name">Display name</Label>
          <Input id="db-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Managed Databases AZ1" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="db-region">Region</Label>
          <Input id="db-region" value={form.region} onChange={(e) => setForm({ ...form, region: e.target.value })} placeholder="az1" disabled={edit} />
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="db-kubeconfig">
          DB-cluster kubeconfig{edit ? " (leave blank to keep the current one)" : ""}
        </Label>
        <Textarea
          id="db-kubeconfig"
          className="min-h-28 font-mono text-xs"
          value={form.kubeconfig}
          onChange={(e) => setForm({ ...form, kubeconfig: e.target.value })}
          placeholder="apiVersion: v1&#10;kind: Config&#10;…"
          autoComplete="off"
        />
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="db-repo">Chart OCI repo</Label>
          <Input id="db-repo" className="font-mono" value={form.chartRepo} onChange={(e) => setForm({ ...form, chartRepo: e.target.value })} placeholder="ghcr.io/mutex-solutions/charts" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="db-chartver">Chart version (pinned)</Label>
          <Input id="db-chartver" className="font-mono" value={form.chartVersion} onChange={(e) => setForm({ ...form, chartVersion: e.target.value })} placeholder="x.y.z" />
          <p className="text-xs text-muted-foreground">
            The database-cluster chart version NEW databases pin (existing ones keep theirs).
          </p>
        </div>
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="db-argons">ArgoCD namespace</Label>
          <Input id="db-argons" value={form.argoNamespace} onChange={(e) => setForm({ ...form, argoNamespace: e.target.value })} placeholder="argocd" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="db-argoproj">ArgoCD AppProject</Label>
          <Input id="db-argoproj" value={form.argoProject} onChange={(e) => setForm({ ...form, argoProject: e.target.value })} placeholder="proxcloud-dbaas" />
        </div>
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="grid gap-2">
          <Label htmlFor="db-pool">LB address pool</Label>
          <Input id="db-pool" className="font-mono" value={form.lbAddressPool} onChange={(e) => setForm({ ...form, lbAddressPool: e.target.value })} placeholder="dbaas-services" autoComplete="off" />
          <p className="text-xs text-muted-foreground">
            The MetalLB IPAddressPool every database endpoint is allocated from — a services
            subnet tenants reach over their normal egress. Empty = any auto-assign pool. Kafka
            needs one address per broker plus the bootstrap.
          </p>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="db-zonekey">Replica spread key (optional)</Label>
          <Input id="db-zonekey" className="font-mono" value={form.zoneKey} onChange={(e) => setForm({ ...form, zoneKey: e.target.value })} placeholder="topology.kubernetes.io/zone" autoComplete="off" />
          <p className="text-xs text-muted-foreground">
            Replicas of one database must land on different values of this node label (a hard
            rule). A one-node or one-zone site sets <code>kubernetes.io/hostname</code>.
          </p>
        </div>
      </div>
      <div className="grid gap-3 rounded-lg border p-3">
        <div>
          <Label className="text-sm font-medium">Backup object store (optional)</Label>
          <p className="text-xs text-muted-foreground">
            Any S3 endpoint — plain <code>http://</code> works. Every database on this location
            backs up here; customers never see it, they only turn backups on and pick a schedule.
            Leave the bucket empty to keep backups off.
          </p>
        </div>
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div className="grid gap-2">
            <Label htmlFor="db-bkendpoint">S3 endpoint</Label>
            <Input id="db-bkendpoint" className="font-mono" value={form.backupEndpoint} onChange={(e) => setForm({ ...form, backupEndpoint: e.target.value })} placeholder="https://s3.example.com" autoComplete="off" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="db-bkbucket">Bucket</Label>
            <Input id="db-bkbucket" className="font-mono" value={form.backupBucket} onChange={(e) => setForm({ ...form, backupBucket: e.target.value })} placeholder="dbaas-backups" autoComplete="off" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="db-bkprefix">Prefix (optional)</Label>
            <Input id="db-bkprefix" className="font-mono" value={form.backupPrefix} onChange={(e) => setForm({ ...form, backupPrefix: e.target.value })} placeholder="az1" autoComplete="off" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="db-bkregion">Region (optional)</Label>
            <Input id="db-bkregion" className="font-mono" value={form.backupRegion} onChange={(e) => setForm({ ...form, backupRegion: e.target.value })} placeholder="us-east-1" autoComplete="off" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="db-bkak">Access key</Label>
            <Input id="db-bkak" type="password" className="font-mono" value={form.backupAccessKey} onChange={(e) => setForm({ ...form, backupAccessKey: e.target.value })} placeholder={edit ? "unchanged" : ""} autoComplete="off" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="db-bksk">Secret key</Label>
            <Input id="db-bksk" type="password" className="font-mono" value={form.backupSecretKey} onChange={(e) => setForm({ ...form, backupSecretKey: e.target.value })} placeholder={edit ? "unchanged" : ""} autoComplete="off" />
          </div>
          <div className="grid gap-2 sm:col-span-2">
            <Label htmlFor="db-bkca">CA Secret (optional)</Label>
            <Input id="db-bkca" className="font-mono" value={form.backupCaSecret} onChange={(e) => setForm({ ...form, backupCaSecret: e.target.value })} placeholder="s3-ca" autoComplete="off" />
            <p className="text-xs text-muted-foreground">
              Name of a Secret (key <code>ca.crt</code>) in each database namespace holding the
              endpoint's CA, for an endpoint behind a private CA.
            </p>
          </div>
        </div>
        <div className="flex items-center justify-between">
          <div>
            <Label htmlFor="db-bkpath" className="text-sm font-medium">Path-style addressing</Label>
            <div className="text-xs text-muted-foreground">
              Keep on for RustFS and Ceph RGW — a virtual-host request makes the gateway parse
              the bucket out of the hostname.
            </div>
          </div>
          <Switch id="db-bkpath" checked={form.backupPathStyle} onCheckedChange={(v) => setForm({ ...form, backupPathStyle: v })} />
        </div>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="db-storageclasses">StorageClasses (comma-separated, optional)</Label>
        <Input id="db-storageclasses" className="font-mono" value={form.storageClasses} onChange={(e) => setForm({ ...form, storageClasses: e.target.value })} placeholder="proxmox-csi" />
        <p className="text-xs text-muted-foreground">
          StorageClasses offered for database volumes; empty = the cluster default only. The
          capacity tier (<code>capacity</code>, <code>capacity-*</code>) is never offered to databases.
        </p>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="db-engines">Engines (one “engine=versions” line each)</Label>
        <Textarea
          id="db-engines"
          className="min-h-20 font-mono text-xs"
          value={form.engines}
          onChange={(e) => setForm({ ...form, engines: e.target.value })}
          placeholder={"postgresql=16,17,18@1,3\nmysql=8.4\nmariadb=11.4\nferretdb=2.5\nkafka=4.3.0@3"}
        />
        <p className="text-xs text-muted-foreground">
          One line per engine, versions comma-separated. Known engines:{" "}
          <code>{DBAAS_ENGINES.join(", ")}</code>. Only listed engines/versions are offered to
          customers; a <code>valkey</code> line needs the beta switch below. Optional{" "}
          <code>@1,3</code> suffix sets the allowed instance counts (1–3; default: a single
          instance), e.g. <code>postgresql=17@1,3</code>.
        </p>
      </div>
      <div className="flex items-center justify-between rounded-lg border p-3">
        <div>
          <Label htmlFor="db-valkey" className="text-sm font-medium">Offer Valkey (beta)</Label>
          <div className="text-xs text-muted-foreground">
            The valkey operator is pre-GA — customers see a Beta badge and must acknowledge it.
          </div>
        </div>
        <Switch id="db-valkey" checked={form.valkeyEnabled} onCheckedChange={(on) => setForm({ ...form, valkeyEnabled: on })} />
      </div>
      <p className="text-xs text-muted-foreground">
        The kubeconfig belongs to a proxcloud service account on the pre-built DB cluster (operators,
        ArgoCD and the AppProject installed there by ops). Databases are delivered as ArgoCD
        Applications of the pinned chart.
      </p>
    </div>
  )
}
