// Platform setup: the rows of the connection test with ?checks=all and the node bridges the admin route adds.
type Doc = Record<string, any>

// A stored, trusted connection. template-pool is a warning, as on a cluster where nobody ran the setup script.
export const setupChecks: Doc[] = [
  { id: "connect", status: "ok", message: "Reached Proxmox VE 9.2.1." },
  { id: "token", status: "ok", message: "Proxmox accepts the token proxcloud@pve!platform." },
  { id: "firewall-datacenter", status: "ok", message: "The datacenter firewall is on." },
  { id: "firewall-nodes", status: "ok", message: "Every online node runs the nftables firewall." },
  { id: "privileges:/", status: "ok", message: "The token holds the ProxCloudPlatform privileges on /." },
  { id: "privileges:/pool", status: "ok", message: "The token holds the ProxCloudPlatform privileges on /pool." },
  { id: "version", status: "ok", message: "Proxmox VE 9.2.1 is 9.2 or later." },
  { id: "template-pool", status: "warn", message: "The pool pc-templates does not exist: run the setup script, or the first template import creates it." },
]

// An unsaved connection to a self-signed certificate that is not pinned yet: nothing after connect can run.
export const untrustedChecks: Doc[] = [
  { id: "connect", status: "fail", message: "Proxmox connection failed: tls: failed to verify certificate: x509: certificate signed by unknown authority" },
]
export const presentedFingerprint =
  "3A:9C:51:0E:7B:22:D4:8F:61:AA:05:C3:9E:47:12:B8:6D:F0:33:5A:C8:91:2E:7D:44:B6:0F:A1:58:E3:9B:C2"

export const nodeBridges: Doc[] = [
  { node: "pve1", iface: "vmbr0", type: "bridge", cidr: "192.0.2.11/24", management: true },
  { node: "pve1", iface: "vmbr1", type: "bridge", management: false },
  { node: "pve2", iface: "vmbr0", type: "bridge", cidr: "192.0.2.12/24", management: true },
  { node: "pve2", iface: "vmbr1", type: "bridge", management: false },
]

// What storage detection finds (config.capabilities.storages) and proposes. No store takes import content yet.
export const detectedStorages: Doc = {
  "local": { type: "dir", content: ["iso", "vztmpl", "backup"], shared: false, nodes: { pve1: { active: true }, pve2: { active: true } } },
  "local-zfs": { type: "zfspool", content: ["images", "rootdir"], shared: false, snapshots: true, thin: true, replication: true, nodes: { pve1: { active: true }, pve2: { active: true } } },
  "nas-capacity": { type: "nfs", content: ["images"], shared: true, liveMigrate: true, nodes: { pve1: { active: true }, pve2: { active: true } } },
}
export const proposedProfiles: Doc = {
  std: { storages: { "*": "local-zfs" }, snapshots: true, thin: true, replication: true },
  capacity: { storages: { "*": "nas-capacity" }, shared: true, liveMigrate: true, label: "Capacity HDD (NFS)" },
}

// Template updates: the published OS images on GitHub Packages, where they can be imported, and the imports so far.
const digest = (c: string) => `sha256:${c.repeat(64)}`
const vmLine = (os: string, pkg: string, tag: string, c: string, variant = "os") => ({
  line: `${pkg}/${variant}`, package: pkg, os, variant, installed: [], updateAvailable: true,
  latest: { package: pkg, tag, digest: digest(c), createdAt: "2026-11-10T09:00:00Z", diskDigest: digest("e"), diskBytes: 1_200_000_000, manifestSha256: "f".repeat(64),
    manifest: { build_version: tag, source_commit: "3593f6f", disk: { virtual_size_bytes: 5_368_709_120 } } },
})
export const templateDiscovery: Doc = {
  vm: [
    vmLine("Ubuntu 24.04", "proxcloud-vm-ubuntu-2404", "ubuntu-2404-20261110", "a"),
    vmLine("Debian 13", "proxcloud-vm-debian-13", "debian-13-20261110", "b"),
    { ...vmLine("Ubuntu 24.04", "proxcloud-vm-k8s-1-37", "k8s-1.37.2-20261110", "c", "kubernetes"), kubernetesVersion: "1.37.2" },
  ],
  sandbox: [], checkedAt: "2026-11-15T08:00:00Z", providers: [{ id: "svc-proxmox-01", name: "Helium Proxmox" }],
}
export const templatePlacement: Doc = {
  configuredImportStorage: "nas-templates",
  nodes: [
    { node: "pve1", importStorages: ["nas-templates"], diskStorages: ["local-zfs", "nas-capacity"] },
    { node: "pve2", importStorages: ["nas-templates"], diskStorages: ["local-zfs"] },
  ],
}
const imported = (id: string, slug: string, tag: string, c: string, state: string, result?: string) => ({
  id, package: `proxcloud-vm-${slug}`, tag, digest: digest(c), os: slug, variant: "os", buildVersion: tag, slug, serviceId: "svc-proxmox-01",
  node: "pve1", vmid: 9201, state, importedAt: "2026-11-14T10:00:00Z", requestedBy: "admin@example.com", publishedOnLine: 0,
  smoke: result ? { jobId: `smoke-${id}`, step: "done", result, ...(result === "FAIL" ? { code: "cloud_init_failed" } : {}) } : undefined,
  actions: { smoke: true, publish: state === "ready_to_publish", withdraw: false, lastPublished: false },
})
export const importedTemplates: Doc[] = [
  imported("tpl-ubuntu", "ubuntu-2404", "ubuntu-2404-20261110", "a", "ready_to_publish", "PASS"),
  imported("tpl-debian", "debian-13", "debian-13-20261110", "b", "ready_to_publish", "PASS"),
  imported("tpl-rocky", "rocky-10", "rocky-10-20261110", "d", "smoke_failed", "FAIL"),
]

// The install defaults (deploy/seed), shortened to what the preview shows.
export const seedFlavors: Doc[] = [
  { slug: "s1-small", name: "Small (1 vCPU, 1 GB)", vcpus: 1, ramMb: 1024, diskGb: 10, active: true, orderNumber: 10 },
  { slug: "s2-medium", name: "Medium (2 vCPU, 2 GB)", vcpus: 2, ramMb: 2048, diskGb: 20, active: true, orderNumber: 20 },
  { slug: "s4-large", name: "Large (4 vCPU, 8 GB)", vcpus: 4, ramMb: 8192, diskGb: 40, active: true, orderNumber: 30 },
]
const rule = (name: string, resourceType: string, prices: Doc[], filters: Doc[] = []) => ({ name, timeUnit: "hour", resourceType, applyMethod: "ADD_TO_TOTAL", prices, filters, modifiers: [] })
export const seedPricePlan: Doc = {
  plan: { name: "Public rate card", enabled: true, accessMode: "PUBLIC", serviceProviders: [] },
  rules: [
    rule("Compute: CPU + RAM components", "instance", [{ attributeName: "vcpus", tiers: [{ value: "0.008" }] }, { attributeName: "ram_gb", tiers: [{ value: "0.004" }] }]),
    rule("GPU:nvidia-a100-80gb", "instance", [{ attributeName: "gpu_count", tiers: [{ value: "1.19" }] }], [{ attributeName: "gpu_model", operator: "eq", value: "nvidia-a100-80gb" }]),
    rule("Public egress: 1 TiB free, then $0.01/GB", "instance_traffic", [{ attributeName: "outgoing_public_traffic_mb", tiers: [{ from: "0", to: "1048576", value: "0" }, { from: "1048576", value: "0.00001" }] }]),
    rule("Block storage", "volume", [{ attributeName: "size", tiers: [{ value: "0.000137" }] }]),
    rule("Public IP", "floating_ip", [{ attributeName: "existence", tiers: [{ value: "0.005" }] }]),
  ],
}
export const seedQuota: Doc = { servers: 20, vcpus: 80, ramMb: 102400, diskGb: 2000, networks: 10, floatingIps: 5 }
