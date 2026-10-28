// Cloud resource seeds — one realistic set per resource type. The provider's
// object lives under data.<kind> (mirrors the API's CloudResource).
import type { CloudResource } from "@/lib/types"
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
  const networkPrivate = res("NETWORK", "net-private", {
    network: { id: "net-priv-1", name: "net-private", status: "ACTIVE", shared: false, "router:external": false, subnets: ["sub-priv-1"] },
  }, { externalId: "net-priv-1" })
  const networkPublic = res("NETWORK", "public", {
    network: { id: "net-public-ext", name: "public", status: "ACTIVE", shared: true, "router:external": true, subnets: [] },
  }, { externalId: "net-public-ext" })

  return [
    ...servers,

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

    res("SECURITY_GROUP", "web-ingress", {
      securityGroup: {
        id: "sg-1",
        name: "web-ingress",
        description: "HTTP/HTTPS from anywhere",
        security_group_rules: [
          { id: "sgr-1", direction: "ingress", ethertype: "IPv4", protocol: "tcp", port_range_min: 80, port_range_max: 80, remote_ip_prefix: "0.0.0.0/0" },
          { id: "sgr-2", direction: "ingress", ethertype: "IPv4", protocol: "tcp", port_range_min: 443, port_range_max: 443, remote_ip_prefix: "0.0.0.0/0" },
        ],
      },
    }),

    res("DATABASE_CLUSTER", "orders-db", {
      database: {
        id: "std-1a2b3c4d", name: "orders-db", engine: "postgresql", version: "17", replicas: 3,
        cpu: 2, memory_gib: 8, storage_gib: 50, storage_class: "proxmox-csi",
        allowed_cidrs: ["10.0.0.0/24"],
        status: "READY", sync_status: "Synced", chart_version: "0.1.0",
        created_at: "2026-07-20T09:00:00Z", endpoint: "10.0.0.50", port: 5432,
      },
    }, { status: "READY", externalId: "std-1a2b3c4d", serviceId: "svc-dbaas-1", region: "RegionOne" }),
  ]
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
