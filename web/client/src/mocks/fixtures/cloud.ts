// Cloud resource seeds — one realistic set per resource type. The provider's
// object lives under data.<kind> (mirrors the API's CloudResource).
import type { CloudResource, ProjectSandboxTemplate } from "@/lib/types"
import { PID } from "./platform"

const counters: Record<string, number> = {}
const rid = (p: string) => {
  counters[p] = (counters[p] ?? 0) + 1
  return `res-${p}-${String(counters[p]).padStart(3, "0")}`
}

function res(type: string, name: string, data: Record<string, any>, extra: Partial<CloudResource> = {}): CloudResource {
  const id = rid(type.toLowerCase().replaceAll("_", "-"))
  return {
    id,
    type,
    name,
    status: "ACTIVE",
    externalId: `ext-${id}`,
    projectId: PID,
    createdAt: "2026-06-12T09:30:00Z",
    info: { createdAt: "2026-06-12T09:30:00Z", updatedAt: "2026-07-01T14:00:00Z" },
    data,
    ...extra,
  }
}

const flavorSmall = { id: "flv-small", name: "m1.small", vcpus: 1, ram: 2048, disk: 20 }
const flavorMedium = { id: "flv-medium", name: "m1.medium", vcpus: 2, ram: 4096, disk: 40 }
const flavorLarge = { id: "flv-large", name: "m1.large", vcpus: 4, ram: 8192, disk: 80 }
const flavorGpu = {
  id: "flv-gpu-a10",
  name: "g1.a10",
  vcpus: 8,
  ram: 32768,
  disk: 200,
  extra_specs: { "pci_passthrough:alias": "nvidia-a10:1" },
}

export const quotaUsage = {
  serviceId: "svc-proxmox-1",
  region: "RegionOne",
  compute: {
    instances: { used: 4, reserved: 0, limit: 8 },
    cores: { used: 16, reserved: 0, limit: 32 },
    ramMb: { used: 49_152, reserved: 0, limit: 65_536 },
  },
  storage: {
    volumes: { used: 3, reserved: 0, limit: 10 },
    gigabytes: { used: 240, reserved: 0, limit: 1_000 },
    snapshots: { used: 1, reserved: 0, limit: 10 },
    perVolumeGigabytes: { used: 0, reserved: 0, limit: 500 },
    volumeTypes: {
      standard: {
        volumes: { used: 1, reserved: 0, limit: 8 },
        gigabytes: { used: 100, reserved: 0, limit: 800 },
      },
      "high-iops": {
        volumes: { used: 2, reserved: 0, limit: 5 },
        gigabytes: { used: 90, reserved: 0, limit: 100 },
      },
    },
  },
  // At the limit, and only one of the five is a server's: the rest are a balancer's and a cluster's.
  network: {
    networks: { used: 2, reserved: 0, limit: 10 },
    floatingIps: { used: 5, reserved: 0, limit: 5 },
    addresses: [
      { ip: "203.0.113.10", networkId: "vmbr1", kind: "service", name: "default/web", resourceId: "lb-8fecab82de11", cluster: "pc-c-618d4578", clusterName: "prod" },
      { ip: "203.0.113.11", networkId: "vmbr1", kind: "service", name: "default/dns", resourceId: "lb-8fecab82de11", cluster: "pc-c-618d4578", clusterName: "prod" },
      { ip: "203.0.113.12", networkId: "vmbr1", kind: "kept", resourceId: "lb-8fecab82de11", cluster: "pc-c-618d4578", clusterName: "prod" },
      { ip: "203.0.113.14", networkId: "vmbr1", kind: "loadBalancer", name: "shop", resourceId: "lb-fcbd5b7d328f" },
      { ip: "203.0.113.20", networkId: "vmbr1", kind: "server", name: "bastion", resourceId: "130" },
    ],
  },
  gpu: {
    limits: { "nvidia-a10": 1 },
    usage: { "nvidia-a10": 1, "nvidia-l40s": 2 },
    usageAvailable: true,
  },
  warnings: [],
}

export const flavors = [flavorSmall, flavorMedium, flavorLarge, flavorGpu].map((f) => ({
  externalId: f.id,
  data: f,
}))

export const flavorCategories = [
  {
    id: "fc-general",
    name: "General purpose",
    orderNumber: 1,
    flavors: [{ flavorName: "m1.small" }, { flavorName: "m1.medium" }, { flavorName: "m1.large" }],
  },
  { id: "fc-gpu", name: "GPU accelerated", orderNumber: 2, flavors: [{ flavorName: "g1.a10" }] },
]

export const publicImages = [
  { id: "img-ubuntu-2404", name: "Ubuntu 24.04", os_distro: "ubuntu", os_version: "24.04", size: 613_000_000, status: "active", visibility: "public" },
  { id: "img-ubuntu-2204", name: "Ubuntu 22.04", os_distro: "ubuntu", os_version: "22.04", size: 601_000_000, status: "active", visibility: "public" },
  { id: "img-debian-13", name: "Debian 13", os_distro: "debian", os_version: "13", size: 590_000_000, status: "active", visibility: "public" },
]

export const imageGrouping = {
  imageCategories: [{ id: "cat-linux", name: "Linux" }],
  imageGroups: [
    {
      id: "ig-ubuntu",
      name: "Ubuntu",
      categoryId: "cat-linux",
      enabled: true,
      orderNumber: 1,
      images: [
        { name: "Ubuntu 24.04", version: "24.04", orderNumber: 1 },
        { name: "Ubuntu 22.04", version: "22.04", orderNumber: 2 },
      ],
    },
    {
      id: "ig-debian",
      name: "Debian",
      categoryId: "cat-linux",
      enabled: true,
      orderNumber: 2,
      images: [{ name: "Debian 13", version: "13", orderNumber: 1 }],
    },
  ],
}

export const volumeTypes = [
  { id: "standard", name: "standard", displayName: "SSD" },
  { id: "high-iops", name: "high-iops", displayName: "High IOPS SSD" },
]

function server(name: string, status: string, flavor: any, addr: string) {
  return res(
    "SERVER",
    name,
    {
      server: {
        id: `vm-${name}`,
        name,
        status,
        key_name: "ops-key",
        addresses: { "net-private": [{ addr, version: 4 }] },
        flavor: {
          original_name: flavor.name,
          vcpus: flavor.vcpus,
          ram: flavor.ram,
          disk: flavor.disk,
          extra_specs: flavor.extra_specs ?? {},
        },
        created: "2026-06-12T09:30:00Z",
      },
    },
    { status },
  )
}

export function seedCloudResources(): CloudResource[] {
  const servers = [
    server("web-01", "ACTIVE", flavorMedium, "10.0.0.11"),
    server("web-02", "ACTIVE", flavorMedium, "10.0.0.12"),
    server("worker-01", "SHUTOFF", flavorLarge, "10.0.0.21"),
    server("gpu-trainer", "BUILD", flavorGpu, "10.0.0.31"),
  ]
  const webExt = servers[0].externalId!

  // externalId mirrors the real API (like volumes above): the cache row's
  // externalId IS the network id — subnet.network_id and router
  // external_gateway_info.network_id join against it.
  // A row as v0.8.0 and v0.8.1 wrote it at create: the one subnet's cidr, gateway and snat, and no subnetDetails.
  const networkPrivate = res("NETWORK", "net-private", {
    network: {
      id: "net-priv-1", name: "net-private", status: "ACTIVE", shared: false, "router:external": false, subnets: ["sub-priv-1"],
      cidr: "10.0.0.0/24", gateway: "10.0.0.1", snat: true,
    },
  }, { externalId: "net-priv-1" })
  const networkPublic = res("NETWORK", "public", {
    network: { id: "net-public-ext", name: "public", status: "ACTIVE", shared: true, "router:external": true, subnets: [] },
  }, { externalId: "net-public-ext" })

  // Managed Kubernetes: one healthy cluster, and one whose provisioning stopped half-way: a worker
  // that never joined, a machine the host had no room for, and a group still inside its bound.
  const minutesAgo = (n: number) => new Date(Date.now() - n * 60000).toISOString()
  const k8s = (id: string, name: string, c: Record<string, any>) =>
    res("KUBERNETES_CLUSTER", name, {
      cluster: {
        id, name, version: "1.35.10", chart_version: "0.1.13", sync_status: "Synced", endpoint: "10.20.0.5:6443",
        cp_status: "Ready", cp_replicas: 1, created_at: minutesAgo(90), storage_classes: ["proxmox-csi"], rwx: false, ...c,
      },
    }, { id: `res-${id}`, externalId: id, serviceId: "svc-k8s-1", region: "RegionOne" })
  const group = (name: string, count: number, ready: number, progress?: Record<string, any>) => ({
    name, flavor_id: "flv-medium", count, replicas: count, ready_replicas: ready, phase: ready >= count ? "Running" : "ScalingUp",
    ...(progress ? { progress } : {}),
  })

  return [
    ...servers,

    // A load balancer on two appliances, both on node pve1 (a one-node site): it survives an appliance failure, not
    // the loss of the node.
    res("LOAD_BALANCER", "web-lb", {
      loadbalancer: {
        id: "a1b2c3d4e5f6", name: "web-lb", status: "ACTIVE", ha: true, serverId: "301", networkId: "v1", publicNetworkId: "vmbr1",
        publicIp: "203.0.113.10", privateIp: "10.0.0.41", vips: ["203.0.113.10"],
        appliances: [
          { serverId: "301", node: "pve1", privateIp: "10.0.0.41", role: "BACKUP", state: "up" },
          { serverId: "302", node: "pve1", privateIp: "10.0.0.42", role: "MASTER", holdsVip: true, state: "up" },
        ],
        listeners: [{ port: 80, pool: "web", sourceRanges: ["198.51.100.0/24"] }],
        pools: [{
          name: "web", protocol: "http", algorithm: "roundrobin", healthCheck: { type: "http", path: "/healthz" },
          members: [{ serverId: webExt, port: 8080, weight: 1, address: "10.0.0.11" }],
        }],
      },
    }),

    // The balancer of prod's Services (a cluster in default mode): five Services with an address each (one ends TLS
    // with a Secret's certificate, one is UDP with a worker down, one has its health check turned off, one has a
    // port without a check that a third worker serves alone), one refused.
    // Made and changed by the cluster, so its page shows the Services and offers no edit or delete.
    res("LOAD_BALANCER", "k8s-5e1f0a2b3c4d", {
      loadbalancer: {
        id: "5e1f0a2b3c4d", name: "k8s-5e1f0a2b3c4d", status: "ACTIVE", ha: true, networkId: "v1", publicNetworkId: "vmbr1",
        publicIp: "203.0.113.21", vips: ["203.0.113.21", "203.0.113.22", "203.0.113.23", "203.0.113.24", "203.0.113.25"],
        appliances: [
          { serverId: "311", node: "pve2", privateIp: "10.20.0.41", role: "MASTER", holdsVip: true, state: "up" },
          { serverId: "312", node: "pve3", privateIp: "10.20.0.42", role: "BACKUP", state: "up" },
        ],
        cluster: {
          id: "pc-c-healthy",
          services: [
            { uid: "u-web", namespace: "shop", name: "web", vip: "203.0.113.21", state: "ready" },
            { uid: "u-gw", namespace: "nginx-gateway", name: "public-gateway-nginx", vip: "203.0.113.22", state: "ready" },
            { uid: "u-dns", namespace: "infra", name: "dns", vip: "203.0.113.23", state: "ready" },
            { uid: "u-log", namespace: "infra", name: "syslog", vip: "203.0.113.24", state: "ready" },
            { uid: "u-mail", namespace: "infra", name: "relay", vip: "203.0.113.25", state: "ready" },
            { uid: "u-sig", namespace: "infra", name: "sigtran", state: "refused", reason: "ProtocolNotSupported", message: "The load balancer serves TCP and UDP, and port 2905 is SCTP: no port of this Service is served" },
          ],
          kept: ["203.0.113.30", "203.0.113.31"],
        },
        listeners: [
          { port: 443, protocol: "https", pool: "s0-0", vip: "203.0.113.21", certificates: ["cert-shop"] },
          { port: 80, pool: "s1-0", vip: "203.0.113.22" },
          { port: 443, protocol: "udp", pool: "s1-1", vip: "203.0.113.22" },
          { port: 53, protocol: "udp", pool: "s2-0", vip: "203.0.113.23" },
          { port: 6514, pool: "s3-0", vip: "203.0.113.24" },
          { port: 25, pool: "s4-0", vip: "203.0.113.25" },
          { port: 587, pool: "s4-1", vip: "203.0.113.25" },
        ],
        pools: [
          { name: "s0-0", protocol: "http", algorithm: "roundrobin", members: ["10.20.0.11", "10.20.0.12", "10.20.0.13"].map((a) => ({ serverId: "", node: true, address: a, port: 31443, weight: 1 })) },
          { name: "s1-0", protocol: "tcp", algorithm: "roundrobin", healthCheck: { type: "http", port: 32100, path: "/" },
            members: ["10.20.0.11", "10.20.0.12", "10.20.0.13"].map((a) => ({ serverId: "", node: true, address: a, port: 30080, weight: 1 })) },
          // The gateway's UDP port (policy Local): checked on the Service's health port, as its TCP port is.
          { name: "s1-1", protocol: "udp", algorithm: "roundrobin", healthCheck: { type: "http", port: 32100, path: "/" },
            members: ["10.20.0.11", "10.20.0.12", "10.20.0.13"].map((a) => ({ serverId: "", node: true, address: a, port: 30443, weight: 1 })) },
          { name: "s2-0", protocol: "udp", algorithm: "roundrobin", healthCheck: { type: "udp-connect" },
            members: ["10.20.0.11", "10.20.0.12", "10.20.0.13"].map((a) => ({ serverId: "", node: true, address: a, port: 30053, weight: 1 })) },
          // proxcloud.io/lb-enable-health-monitor: "false": HAProxy checks no worker and sends to every one.
          { name: "s3-0", protocol: "tcp", algorithm: "roundrobin", healthCheck: { type: "none" },
            members: ["10.20.0.11", "10.20.0.12", "10.20.0.13"].map((a) => ({ serverId: "", node: true, address: a, port: 30514, weight: 1 })) },
          // A checked port on two workers and an unchecked one on three. The controller gives every port of a Service
          // the same workers: this one is here for the count over both kinds.
          { name: "s4-0", protocol: "tcp", algorithm: "roundrobin",
            members: ["10.20.0.11", "10.20.0.12"].map((a) => ({ serverId: "", node: true, address: a, port: 30025, weight: 1 })) },
          { name: "s4-1", protocol: "tcp", algorithm: "roundrobin", healthCheck: { type: "none" },
            members: ["10.20.0.11", "10.20.0.12", "10.20.0.13"].map((a) => ({ serverId: "", node: true, address: a, port: 30587, weight: 1 })) },
        ],
      },
    }),

    // Three workers placed on three hosts: scaling to two empties one host.
    k8s("pc-c-healthy", "prod", {
      status: "READY", load_balancer: "default", addons: { metricsServer: true, nginxGatewayFabric: true },
      // __mockLBControllerUpdate: an e2e spec's stand-in for a release that changed the controller's image alone.
      ...((globalThis as { __mockLBControllerUpdate?: boolean }).__mockLBControllerUpdate ? { lb_controller_update: true } : {}),
      node_groups: [{ ...group("workers", 3, 3), image_id: "pve1=9201,pve2=9202,pve3=9203", placement: { pve1: 1, pve2: 1, pve3: 1 } }],
    }),
    k8s("pc-c-stuck", "staging", {
      status: "PROGRESSING",
      node_groups: [
        // The machine has no row among the project's servers: the production case (no sync had listed it).
        group("workers", 2, 1, { code: "not_joined", since: minutesAgo(42), machines: 1, attention: true, machineNames: ["pc-c-stuck-workers-pve2-fghij-stuck"] }),
        group("batch", 3, 0, { code: "no_capacity", since: minutesAgo(8), machines: 2, attention: true }),
        group("build", 1, 0, { code: "creating_vm", since: minutesAgo(3), machines: 1, attention: false }),
        group("edge", 1, 0, { code: "not_joined", since: minutesAgo(4), machines: 1, attention: false }),
      ],
    }),
    k8s("pc-c-nocp", "sandbox", {
      status: "PROGRESSING", endpoint: undefined, cp_status: "Provisioning", created_at: minutesAgo(25),
      attention: { code: "control_plane_stalled", since: minutesAgo(25) },
      node_groups: [group("workers", 1, 0, { code: "waiting_control_plane", since: minutesAgo(25), machines: 1, attention: false })],
    }),

    res("SERVER_GROUP", "web-anti-affinity", {
      serverGroup: { name: "web-anti-affinity", policy: "anti-affinity", policies: ["anti-affinity"], members: [] },
    }),

    res("KEYPAIR", "ops-key", {
      keypair: { name: "ops-key", fingerprint: "SHA256:mQ4o8Xg1kx1yFvE0aH3T7Zz9r2Yc5NbKpDqWlUuVsoE" },
      publicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockMockMockMockMockMockMockMockMockMockMock dev@example.com",
    }),

    res("IMAGE", "web-01-snapshot", {
      image: { id: "img-snap-web01", name: "web-01-snapshot", status: "active", visibility: "private", size: 2_147_483_648, instance_uuid: webExt },
    }),

    res("VOLUME", "data-vol-01", {
      volume: { id: "vol-1", name: "data-vol-01", size: 100, status: "in-use", volume_type: "standard", attachments: [{ server_id: `vm-web-01`, device: "/dev/vdb" }] },
      sizeInGb: 100,
      attachments: [{ server_id: `vm-web-01`, device: "/dev/vdb" }],
      // externalId mirrors the real API: the cache row's externalId IS the volume id,
      // so cross-references (snapshot.volume_id → volume) resolve like production.
    }, { status: "IN-USE", externalId: "vol-1" }),
    res("VOLUME", "scratch-vol", {
      volume: { id: "vol-2", name: "scratch-vol", size: 50, status: "available", volume_type: "high-iops", attachments: [] },
      sizeInGb: 50,
      attachments: [],
    }, { status: "AVAILABLE", externalId: "vol-2" }),

    res("VOLUME_SNAPSHOT", "data-vol-01-snap", {
      volumeSnapshot: { id: "vsnap-1", name: "data-vol-01-snap", status: "available", size: 100, volume_id: "vol-1" },
      snapshot: { id: "vsnap-1", name: "data-vol-01-snap", status: "available", size: 100, volume_id: "vol-1" },
    }, { status: "AVAILABLE" }),

    res("BUCKET", "acme-assets", { bucketName: "acme-assets", objectCount: 42, sizeInGb: "12.4", storageBackend: "CEPH_S3" }, { serviceId: "svc-ceph-1" }),
    res("BUCKET", "acme-backups", { bucketName: "acme-backups", objectCount: 7, sizeInGb: "3.2", storageBackend: "CEPH_S3" }, { serviceId: "svc-ceph-1" }),

    networkPrivate,
    networkPublic,

    res("SUBNET", "sub-private-a", {
      subnet: { id: "sub-priv-1", name: "sub-private-a", cidr: "10.0.0.0/24", network_id: "net-priv-1", enable_dhcp: true, gateway_ip: "10.0.0.1", dns_nameservers: ["1.1.1.1", "8.8.8.8"], ip_version: 4 },
    }),

    res("PORT", "web-01-eth0", {
      port: { id: "port-1", name: "web-01-eth0", status: "ACTIVE", device_id: webExt, network_id: "net-priv-1", fixed_ips: [{ ip_address: "10.0.0.11", subnet_id: "sub-priv-1" }], port_security_enabled: true, allowed_address_pairs: [] },
    }),

    res("ROUTER", "edge-router", {
      router: { id: "rtr-1", name: "edge-router", status: "ACTIVE", external_gateway_info: { network_id: "net-public-ext" } },
      routerName: "edge-router",
    }),

    // The workers of cluster "prod" (pc-c-healthy) are behind the Proxmox firewall: their group is named after it.
    res("SECURITY_GROUP", "k8s-pc-c-healthy", {
      securityGroup: {
        id: "sg-k8s", name: "k8s-pc-c-healthy", description: "",
        security_group_rules: [
          { id: "sg-k8s-0-aaaa", direction: "ingress", protocol: "udp", port_range: "8472", remote_ip_prefix: "10.100.0.232/29", description: "workers: Cilium VXLAN (pod network)" },
        ],
      },
    }),

    res("SECURITY_GROUP", "web-ingress", {
      securityGroup: {
        id: "sg-1",
        name: "web-ingress",
        description: "HTTP/HTTPS from anywhere",
        security_group_rules: [
          { id: "sgr-1", direction: "ingress", protocol: "tcp", port_range: "80", remote_ip_prefix: "0.0.0.0/0", description: "" },
          { id: "sgr-2", direction: "ingress", protocol: "tcp", port_range: "443", remote_ip_prefix: "0.0.0.0/0", description: "HTTPS" },
        ],
      },
    }),

    res("DATABASE_CLUSTER", "orders-db", {
      database: {
        id: "pc-d-1a2b3c4d", name: "orders-db", engine: "postgresql", version: "17", replicas: 3,
        cpu: 2, memory_gib: 8, storage_gib: 50, storage_class: "proxmox-csi",
        allowed_cidrs: ["10.0.0.0/24"],
        status: "READY", sync_status: "Synced", chart_version: "0.1.0",
        created_at: "2026-07-20T09:00:00Z", endpoint: "10.0.0.50", port: 5432,
      },
    }, { status: "READY", externalId: "pc-d-1a2b3c4d", serviceId: "svc-dbaas-1", region: "RegionOne" }),
  ]
}

// Private project-scoped fixtures. Distinct IDs and same primary name demonstrate
// that a URL's project is authoritative; no template belongs to every project.
export type MockSandboxTemplate = ProjectSandboxTemplate & { projectId: string }
export function seedSandboxTemplates(): MockSandboxTemplate[] {
  return [PID, "prj-staging"].map((projectId) => ({
    id: projectId === PID ? "tpl-web-app" : "tpl-staging-app", projectId,
    name: "web-app", kind: "template", mode: "build", aliases: ["web-app", "app-stable"],
    cpu: 2, memoryGiB: 4, status: "error", ready: true,
    createdAt: "2026-11-01T07:00:00Z", updatedAt: "2026-11-01T08:00:00Z",
    builds: [
      { buildID: "build-failed", status: "error", mode: "build", tags: [], reason: "Build step failed (exit 137, OOMKilled)", step: "kaniko", buildResources: { buildCpu: 4, buildMemoryGiB: 16, buildDiskGiB: 80 }, createdAt: "2026-11-01T07:59:00Z", finishedAt: "2026-11-01T08:00:00Z" },
      { buildID: "build-ready", status: "ready", mode: "build", tags: ["default"], image: `registry.example.com/templates/${projectId}/web-app@sha256:${"a".repeat(64)}`, createdAt: "2026-11-01T07:00:00Z", finishedAt: "2026-11-01T07:01:00Z" },
    ],
  }))
}

export const bucketObjects = [
  { name: "images/", displayName: "images", directory: true },
  { name: "index.html", displayName: "index.html", sizeInBytes: 4096, mimeType: "text/html", directory: false, lastModified: "2026-07-01T10:00:00Z" },
  { name: "styles.css", displayName: "styles.css", sizeInBytes: 12_288, mimeType: "text/css", directory: false, lastModified: "2026-07-01T10:00:00Z" },
  { name: "backup-2026-07-01.tar.gz", displayName: "backup-2026-07-01.tar.gz", sizeInBytes: 104_857_600, mimeType: "application/gzip", directory: false, lastModified: "2026-07-01T02:00:00Z" },
]

export const bucketSettings = {
  versioning: "Disabled",
  objectLock: { enabled: false },
  quota: { enabled: false, maxSizeBytes: 0, maxObjects: 0 },
  lifecycle: [],
  cors: [],
  tags: {},
  grants: [],
  policyJson: "",
  website: { enabled: false },
}

export const s3Credentials = {
  accessKey: "MOCKACCESSKEY0001",
  secretKey: "mockSecretKey/0000000000000000000000001",
  rgwUid: "prj-aurora$default",
  s3Endpoint: "https://s3.example.com",
  websiteEndpoint: "https://web.s3.example.com",
  region: "RegionOne",
}

export const s3Keys = [
  { id: "s3k-1", name: "ci-uploader", rgwUid: "prj-aurora$ci", accessKey: "MOCKACCESSKEYCI01", createdAt: "2026-06-15T12:00:00Z" },
]
