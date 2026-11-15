// NetworkStorage.tsx — steps 3 and 4 of the Platform setup page: the provider's config.network as a form, and its
// storage (detection, the proposed profiles, the default, the import and backup stores, the NAS and TrueNAS items).
// Both save with PUT /admin/service/{id}, which replaces each top-level config key: the block is read fresh and sent
// whole, so keys this page does not show (public networks, isolation, other zones, profiles) are kept.
import { useState } from "react"
import { Link } from "react-router-dom"
import { useMutation } from "@tanstack/react-query"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { useAdminGet } from "@/lib/hooks"
import {
  CopyBlock, ErrorNote, LINK, StepCard, errMsg, readProvider, useClusterInfo, useInvalidateSetup, type SetupStep,
} from "./common"

type Doc = Record<string, any>
const list = (v: string) => v.split(/[\s,]+/).filter(Boolean)
const NONE = "-"

// The stored provider, or the step's loading, error and no-provider states.
function useStoredProvider(serviceId: string) {
  return useAdminGet<{ name?: string; config?: Doc }>(`/admin/service/${serviceId}`, !!serviceId)
}
function ProviderGate({ serviceId, q, children }: { serviceId: string; q: ReturnType<typeof useStoredProvider>; children: React.ReactNode }) {
  if (!serviceId) return <p className="text-sm text-muted-foreground">Save a Proxmox provider first (step 2).</p>
  if (q.isLoading) return <Skeleton className="h-40" />
  if (q.error || !q.data) return <ErrorNote title="The provider could not be read" error={q.error ?? new Error("not found")} />
  return <>{children}</>
}

// ── Step 3: Network ───────────────────────────────────────────────────────────

type Kind = "simple" | "vlan" | "evpn"

export function NetworkStep({ step, serviceId }: { step?: SetupStep; serviceId: string }) {
  const q = useStoredProvider(serviceId)
  return (
    <StepCard
      n={3}
      title="Network"
      step={step}
      description="The SDN zone tenant networks are created in, the range they are cut from, and what tenants must not reach. Proxmox gets the zone when the first tenant network is created."
    >
      <ProviderGate serviceId={serviceId} q={q}>
        <NetworkForm key={q.dataUpdatedAt} serviceId={serviceId} cfg={q.data?.config ?? {}} />
      </ProviderGate>
    </StepCard>
  )
}

function NetworkForm({ serviceId, cfg }: { serviceId: string; cfg: Doc }) {
  const invalidate = useInvalidateSetup()
  const cluster = useClusterInfo(serviceId)
  const stored: Doc = cfg.network ?? {}
  const storedZone: Doc = stored.zones?.[stored.defaultZone] ?? {}
  const nodes = (cluster.data?.nodes ?? []).map((n) => n.node).sort()
  const bridges = cluster.data?.bridges ?? []
  // EVPN peers default to the nodes' own addresses: the address of each node's management bridge.
  const nodeIPs = [...new Set(bridges.filter((b) => b.management && b.cidr).map((b) => b.cidr!.split("/")[0]))]
  const publicBridges = [...new Set(bridges.filter((b) => !b.management).map((b) => b.iface))].sort()

  const [kind, setKind] = useState<Kind>(storedZone.kind === "vlan" || storedZone.kind === "simple" ? storedZone.kind : "evpn")
  const [zoneId, setZoneId] = useState<string>(stored.defaultZone || "overlay")
  const [bridge, setBridge] = useState<string>(storedZone.bridge ?? "")
  const [vlanMin, setVlanMin] = useState<string>(String(storedZone.vlanMin ?? 100))
  const [vlanMax, setVlanMax] = useState<string>(String(storedZone.vlanMax ?? 199))
  const [peers, setPeers] = useState<string | null>(storedZone.peers ? storedZone.peers.join(", ") : null)
  const [exitNodes, setExitNodes] = useState<string[]>(storedZone.exitNodes ?? [])
  const [cidrPool, setCidrPool] = useState<string>(stored.cidrPool || "10.100.0.0/16")
  const [blocked, setBlocked] = useState<string>((stored.blockedCidrs ?? []).join("\n"))
  const [pub, setPub] = useState({ id: "", name: "Internet", cidr: "", gateway: "", ranges: "" })
  const peerText = peers ?? nodeIPs.join(", ")

  const save = useMutation({
    mutationFn: async () => {
      const fresh: Doc = (await readProvider(serviceId)).config?.network ?? {}
      const old: Doc = fresh.zones?.[zoneId] ?? {}
      const zone: Doc = kind === "simple" ? { kind }
        : kind === "vlan" ? { kind, bridge: bridge.trim(), vlanMin: Number(vlanMin), vlanMax: Number(vlanMax) }
        : { kind, controller: old.controller || "proxcloud", asn: old.asn || 65000, peers: list(peerText), exitNodes, vrfVxlan: old.vrfVxlan || 10000, mtu: old.mtu || 1450 }
      const network: Doc = {
        ...fresh,
        defaultZone: zoneId.trim(),
        zones: { ...(fresh.zones ?? {}), [zoneId.trim()]: { ...old, ...zone } },
        cidrPool: fresh.cidrPool || cidrPool.trim(), // set once: a stored range is never replaced from here
        blockedCidrs: list(blocked),
      }
      if (!network.blockedCidrs.length) delete network.blockedCidrs
      if (pub.id) {
        const added = { id: pub.id, name: pub.name.trim() || pub.id, cidr: pub.cidr.trim(), gateway: pub.gateway.trim(), ...(list(pub.ranges).length ? { ranges: list(pub.ranges) } : {}) }
        network.publicNetworks = [...((fresh.publicNetworks ?? []) as Doc[]).filter((p) => p.id !== pub.id), added]
      }
      return apiFetch(`/admin/service/${serviceId}`, { method: "PUT", body: { config: { network } } })
    },
    onSuccess: () => {
      toast.success("Network saved")
      invalidate(serviceId)
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  const zoneOK = /^[A-Za-z][A-Za-z0-9]{0,7}$/.test(zoneId.trim())
  const ready = zoneOK && cidrPool.trim() && (kind !== "vlan" || bridge.trim()) && (!pub.id || (pub.cidr.trim() && pub.gateway.trim()))

  return (
    <div className="grid gap-4">
      <div className="grid gap-4 sm:grid-cols-3">
        <div className="grid gap-2">
          <Label htmlFor="net-kind">Zone kind</Label>
          <Select value={kind} onValueChange={(v) => setKind(v as Kind)}>
            <SelectTrigger id="net-kind"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="evpn">EVPN (several nodes)</SelectItem>
              <SelectItem value="vlan">VLAN on a bridge</SelectItem>
              <SelectItem value="simple">Simple (one node)</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="net-zone">Zone ID</Label>
          <Input id="net-zone" className="font-mono" maxLength={8} value={zoneId} onChange={(e) => setZoneId(e.target.value)} aria-invalid={!zoneOK} aria-describedby="net-zone-help" />
          <p id="net-zone-help" className="text-xs text-muted-foreground">The Proxmox SDN zone ID: a letter, then letters or digits, 8 at most.</p>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="net-pool">Project range (cidrPool)</Label>
          <Input id="net-pool" className="font-mono" value={cidrPool} onChange={(e) => setCidrPool(e.target.value)} disabled={!!stored.cidrPool} aria-describedby="net-pool-help" />
          <p id="net-pool-help" className="text-xs text-muted-foreground">
            Set once: every tenant network is cut from it, so it cannot change here once saved. IPv4, /24 or larger.
          </p>
        </div>
      </div>

      {kind === "vlan" ? (
        <div className="grid gap-4 sm:grid-cols-3">
          <div className="grid gap-2">
            <Label htmlFor="net-bridge">Bridge</Label>
            <Input id="net-bridge" className="font-mono" list="net-bridges" value={bridge} onChange={(e) => setBridge(e.target.value)} placeholder="vmbr1" />
            <datalist id="net-bridges">{[...new Set(bridges.map((b) => b.iface))].map((b) => <option key={b} value={b} />)}</datalist>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="net-vlan-min">First VLAN tag</Label>
            <Input id="net-vlan-min" type="number" min={1} max={4094} value={vlanMin} onChange={(e) => setVlanMin(e.target.value)} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="net-vlan-max">Last VLAN tag</Label>
            <Input id="net-vlan-max" type="number" min={1} max={4094} value={vlanMax} onChange={(e) => setVlanMax(e.target.value)} />
          </div>
        </div>
      ) : null}

      {kind === "evpn" ? (
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="grid gap-2">
            <Label htmlFor="net-peers">EVPN peers (the nodes' addresses)</Label>
            <Input id="net-peers" className="font-mono" value={peerText} onChange={(e) => setPeers(e.target.value)} placeholder="192.0.2.11, 192.0.2.12" />
          </div>
          <fieldset className="grid gap-2">
            <legend className="mb-2 text-sm font-medium">Exit nodes (SNAT to the outside)</legend>
            {nodes.length ? (
              <div className="flex flex-wrap gap-4">
                {nodes.map((n) => (
                  <label key={n} className="flex items-center gap-2 text-sm">
                    <Checkbox checked={exitNodes.includes(n)} onCheckedChange={(on) => setExitNodes(on === true ? [...exitNodes, n] : exitNodes.filter((x) => x !== n))} />
                    {n}
                  </label>
                ))}
              </div>
            ) : (
              <Input aria-label="Exit nodes" className="font-mono" value={exitNodes.join(", ")} onChange={(e) => setExitNodes(list(e.target.value))} placeholder="pve1" />
            )}
          </fieldset>
        </div>
      ) : null}

      <div className="grid gap-2">
        <Label htmlFor="net-blocked">Networks tenants must not reach</Label>
        <Textarea id="net-blocked" className="font-mono" rows={2} value={blocked} onChange={(e) => setBlocked(e.target.value)} placeholder="192.0.2.0/24" />
        <p className="text-xs text-muted-foreground">One CIDR per line, on top of the nodes' management networks, which are always blocked: storage and office networks a server could reach through an exit node.</p>
      </div>

      <fieldset className="grid gap-3 rounded-lg border p-3">
        <legend className="px-1 text-sm font-medium">Public network (optional)</legend>
        {(stored.publicNetworks ?? []).length ? (
          <p className="text-sm text-muted-foreground">
            Kept as they are: {(stored.publicNetworks as Doc[]).map((p) => `${p.name ?? p.id} (${p.id}${p.cidr ? `, ${p.cidr}` : ""})`).join("; ")}.
          </p>
        ) : null}
        <div className="grid gap-4 sm:grid-cols-3">
          <div className="grid gap-2">
            <Label htmlFor="pub-bridge">Bridge</Label>
            <Select value={pub.id || NONE} onValueChange={(v) => setPub({ ...pub, id: v === NONE ? "" : v })} disabled={!publicBridges.length}>
              <SelectTrigger id="pub-bridge"><SelectValue placeholder="None" /></SelectTrigger>
              <SelectContent>
                <SelectItem value={NONE}>None</SelectItem>
                {publicBridges.map((b) => <SelectItem key={b} value={b}>{b}</SelectItem>)}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              {cluster.isLoading ? "Reading the bridges…" : publicBridges.length
                ? "Only bridges that carry no node address and no default gateway: never the management bridge."
                : "No bridge without a node address was found, or the bridges could not be read."}
            </p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="pub-name">Name</Label>
            <Input id="pub-name" value={pub.name} onChange={(e) => setPub({ ...pub, name: e.target.value })} disabled={!pub.id} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="pub-cidr">Range (CIDR)</Label>
            <Input id="pub-cidr" className="font-mono" value={pub.cidr} onChange={(e) => setPub({ ...pub, cidr: e.target.value })} placeholder="203.0.113.0/24" disabled={!pub.id} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="pub-gw">Gateway</Label>
            <Input id="pub-gw" className="font-mono" value={pub.gateway} onChange={(e) => setPub({ ...pub, gateway: e.target.value })} placeholder="203.0.113.1" disabled={!pub.id} />
          </div>
          <div className="grid gap-2 sm:col-span-2">
            <Label htmlFor="pub-ranges">Addresses to hand out (optional, default the whole range)</Label>
            <Input id="pub-ranges" className="font-mono" value={pub.ranges} onChange={(e) => setPub({ ...pub, ranges: e.target.value })} placeholder="203.0.113.10-203.0.113.200" disabled={!pub.id} />
          </div>
        </div>
      </fieldset>

      <div className="flex justify-end">
        <Button onClick={() => save.mutate()} disabled={!ready || save.isPending}>{save.isPending ? "Saving…" : "Save network"}</Button>
      </div>
    </div>
  )
}

// ── Step 4: Storage ───────────────────────────────────────────────────────────

type StorageCap = { type?: string; content?: string[]; nodes?: Record<string, { active?: boolean }>; shared?: boolean }
type Detected = { detectedAt?: string; storages?: Record<string, StorageCap>; proposedProfiles?: Record<string, Doc> }
const FLAGS = ["shared", "snapshots", "thin", "liveMigrate", "replication"]

export function StorageStep({ step, serviceId }: { step?: SetupStep; serviceId: string }) {
  const q = useStoredProvider(serviceId)
  return (
    <StepCard
      n={4}
      title="Storage"
      step={step}
      description="Where servers and volumes go: detect the cluster's storage, confirm the proposed profiles and pick the default, the import store and the backup store."
    >
      <ProviderGate serviceId={serviceId} q={q}>
        <StorageForm key={q.dataUpdatedAt} serviceId={serviceId} cfg={q.data?.config ?? {}} />
      </ProviderGate>
      <NASItems />
    </StepCard>
  )
}

function StorageForm({ serviceId, cfg }: { serviceId: string; cfg: Doc }) {
  const invalidate = useInvalidateSetup()
  const storedProfiles: Record<string, Doc> = cfg.storageProfiles ?? {}
  const [chosen, setChosen] = useState<string[]>([])
  const [defaultProfile, setDefaultProfile] = useState<string>(cfg.defaultStorageProfile ?? "")
  const [importStorage, setImportStorage] = useState<string>(cfg.importStorage ?? "")
  const [backupStorage, setBackupStorage] = useState<string>(cfg.backupStorage ?? "")
  const detect = useMutation({
    mutationFn: () => apiFetch<Detected>(`/admin/service/${serviceId}/storage/detect`, { method: "POST" }),
    onSuccess: (d) => {
      const fresh = Object.keys(d?.proposedProfiles ?? {}).filter((s) => !storedProfiles[s])
      setChosen(fresh)
      if (!defaultProfile) setDefaultProfile(Object.keys(storedProfiles)[0] ?? fresh[0] ?? "")
      // The status only: a fresh provider document would remount this form and drop the proposal.
      invalidate()
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  const proposed = detect.data?.proposedProfiles ?? {}
  const storages: Record<string, StorageCap> = detect.data?.storages ?? cfg.capabilities?.storages ?? {}
  const ids = Object.keys(storages).sort()
  const withContent = (c: string) => ids.filter((id) => storages[id].content?.includes(c))
  const profileSlugs = [...new Set([...Object.keys(storedProfiles), ...chosen])].sort()
  // No store takes import content: the command for one, run as root (the portal token may not change storage).
  const importCandidate = withContent("iso")[0] ?? ids[0]

  const save = useMutation({
    mutationFn: async () => {
      const fresh: Doc = (await readProvider(serviceId)).config?.storageProfiles ?? {}
      const added = Object.fromEntries(chosen.map((s) => [s, proposed[s]]))
      return apiFetch(`/admin/service/${serviceId}`, {
        method: "PUT",
        body: {
          config: {
            storageProfiles: { ...added, ...fresh }, // a saved profile wins over a proposal of the same slug
            defaultStorageProfile: defaultProfile,
            ...(importStorage ? { importStorage } : {}),
            ...(backupStorage ? { backupStorage } : {}),
          },
        },
      })
    },
    onSuccess: () => {
      toast.success("Storage saved")
      invalidate(serviceId)
    },
    onError: (e) => toast.error(errMsg(e)),
  })

  const storeSelect = (id: string, label: string, value: string, set: (v: string) => void, options: string[]) => (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      <Select value={value || NONE} onValueChange={(v) => set(v === NONE ? "" : v)}>
        <SelectTrigger id={id}><SelectValue /></SelectTrigger>
        <SelectContent>
          <SelectItem value={NONE}>Leave unchanged</SelectItem>
          {options.map((o) => <SelectItem key={o} value={o}>{o}</SelectItem>)}
        </SelectContent>
      </Select>
    </div>
  )

  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="outline" onClick={() => detect.mutate()} disabled={detect.isPending}>
          {detect.isPending ? "Detecting…" : ids.length ? "Detect again" : "Detect storage"}
        </Button>
        <span className="text-sm text-muted-foreground">
          {detect.data?.detectedAt || cfg.capabilities?.detectedAt ? `Detected ${new Date(detect.data?.detectedAt ?? cfg.capabilities.detectedAt).toLocaleString()}` : "Not detected yet."}
          {ids.length && !detect.data ? " Detect again to get proposed profiles." : ""}
        </span>
      </div>
      <ErrorNote title="Storage detection failed" error={detect.error} />

      {ids.length ? (
        <div>
          <Table aria-label="Detected storage">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Storage</TableHead>
                <TableHead>Content</TableHead>
                <TableHead>Active on</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {ids.map((id) => (
                <TableRow key={id}>
                  <TableCell><span className="font-medium">{id}</span> <span className="font-mono text-xs text-muted-foreground">{storages[id].type}</span></TableCell>
                  <TableCell className="font-mono text-xs">{(storages[id].content ?? []).join(", ")}</TableCell>
                  <TableCell className="font-mono text-xs">{Object.entries(storages[id].nodes ?? {}).filter(([, n]) => n.active).map(([n]) => n).join(", ") || "none"}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ) : null}

      {profileSlugs.length || Object.keys(proposed).length ? (
        <div>
          <Table aria-label="Storage profiles">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Add</TableHead>
                <TableHead>Profile</TableHead>
                <TableHead>Storage</TableHead>
                <TableHead>Capabilities</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {[...new Set([...Object.keys(storedProfiles), ...Object.keys(proposed)])].sort().map((slug) => {
                const saved = !!storedProfiles[slug]
                const p = storedProfiles[slug] ?? proposed[slug]
                return (
                  <TableRow key={slug}>
                    <TableCell>
                      {saved ? <span className="text-xs text-muted-foreground">saved</span> : (
                        <Checkbox aria-label={`Add profile ${slug}`} checked={chosen.includes(slug)} onCheckedChange={(on) => setChosen(on === true ? [...chosen, slug] : chosen.filter((s) => s !== slug))} />
                      )}
                    </TableCell>
                    <TableCell><span className="font-mono">{slug}</span>{p.label ? <span className="text-muted-foreground"> {p.label}</span> : null}</TableCell>
                    <TableCell className="font-mono text-xs">{Object.values(p.storages ?? {}).join(", ")}</TableCell>
                    <TableCell className="text-xs">{FLAGS.filter((f) => p[f]).join(", ") || "—"}</TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </div>
      ) : null}

      <div className="grid gap-4 sm:grid-cols-3">
        <div className="grid gap-2">
          <Label htmlFor="st-default">Default profile</Label>
          <Select value={defaultProfile || NONE} onValueChange={(v) => setDefaultProfile(v === NONE ? "" : v)} disabled={!profileSlugs.length}>
            <SelectTrigger id="st-default"><SelectValue placeholder="Pick a profile" /></SelectTrigger>
            <SelectContent>
              <SelectItem value={NONE}>None</SelectItem>
              {profileSlugs.map((s) => <SelectItem key={s} value={s}>{s}</SelectItem>)}
            </SelectContent>
          </Select>
        </div>
        {storeSelect("st-import", "Import store (import content)", importStorage, setImportStorage, withContent("import"))}
        {storeSelect("st-backup", "Backup store (backup content)", backupStorage, setBackupStorage, withContent("backup"))}
      </div>

      {ids.length && !withContent("import").length ? (
        <div className="grid gap-2 text-sm">
          <p>
            No storage takes <code>import</code> content, so templates and images cannot be imported. Add it as root on a
            node (the portal token may not change storage), then detect again:
          </p>
          <CopyBlock text={`pvesm set ${importCandidate} --content ${[...(storages[importCandidate]?.content ?? []), "import"].join(",")}`} label="pvesm set command" />
        </div>
      ) : null}

      <div className="flex justify-end">
        <Button onClick={() => save.mutate()} disabled={!defaultProfile || save.isPending}>{save.isPending ? "Saving…" : "Save storage"}</Button>
      </div>
    </div>
  )
}

// The two optional guided items: NAS stores in Proxmox (the stores installer stage 05 creates), and TrueNAS for
// tenant file shares.
function NASItems() {
  const [host, setHost] = useState("")
  const [base, setBase] = useState("/mnt/tank")
  const h = host.trim() || "<nas address>"
  const b = base.trim().replace(/\/+$/, "") || "/mnt/tank"
  const commands = [
    `pvesm add nfs nas-templates --server ${h} --export ${b}/templates --content iso,import,images --options vers=4.2,soft`,
    `pvesm add nfs nas-capacity --server ${h} --export ${b}/capacity --content images --options vers=4.2`,
    `pvesm add nfs nas-backup --server ${h} --export ${b}/backup --content backup --options vers=4.2`,
  ].join("\n")
  return (
    <div className="grid gap-4 border-t pt-4">
      <details className="grid gap-3">
        <summary className="cursor-pointer text-sm font-medium">Optional: NAS stores in Proxmox</summary>
        <div className="mt-3 grid gap-3">
          <p className="text-sm text-muted-foreground">
            Three NFS stores: templates and image import (soft-mounted, so a NAS outage does not hang a node), capacity
            disks, and server backups. Run as root on one node, then detect again and pick them above.
          </p>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="nas-host">NAS address</Label>
              <Input id="nas-host" className="font-mono" value={host} onChange={(e) => setHost(e.target.value)} placeholder="192.0.2.20" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="nas-base">Export path prefix</Label>
              <Input id="nas-base" className="font-mono" value={base} onChange={(e) => setBase(e.target.value)} />
            </div>
          </div>
          <CopyBlock text={commands} label="pvesm add commands" />
        </div>
      </details>
      <details>
        <summary className="cursor-pointer text-sm font-medium">Optional: TrueNAS for tenant file shares</summary>
        <div className="mt-3 grid gap-2 text-sm">
          <p className="text-muted-foreground">Tenants get NFS and SMB shares from a TrueNAS provider. Before you add it, have:</p>
          <ul className="list-disc pl-5 text-muted-foreground">
            <li>the TrueNAS reachable over HTTPS from the API, and its certificate fingerprint when it is self-signed;</li>
            <li>a user (for example <code>proxcloud</code>) in the Sharing Administrator group with an API key; deleting shares also needs DATASET_DELETE, SMB shares ACCOUNT_WRITE;</li>
            <li>a parent dataset for the shares, for example <code>tank/shares</code>;</li>
            <li>an address of the NAS that tenant networks can reach to mount from.</li>
          </ul>
          <Link to="/system/cloud-providers?add=truenas" className={LINK}>Add a TrueNAS provider</Link>
        </div>
      </details>
    </div>
  )
}
