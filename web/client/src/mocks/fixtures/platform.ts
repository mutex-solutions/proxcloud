// Platform-level seed data: projects, orgs, locations, menu, features.
import type { Location, Organization, Project } from "@/lib/types"
import { mockProfile } from "../enabled"

export const PID = "prj-aurora"
export const ORG_ID = "org-acme"
export const BP_ID = "bp-acme-1"

export const projects: Project[] = [
  {
    id: PID,
    name: "aurora-production",
    status: "ACTIVE",
    organizationId: ORG_ID,
    billingProfileId: BP_ID,
    memberships: [{ sub: mockProfile.sub, roles: ["OWNER"] }],
    services: [{ serviceId: "svc-proxmox-1" }, { serviceId: "svc-ceph-1" }, { serviceId: "svc-dbaas-1" }, { serviceId: "svc-sandbox-1" }],
    publicNetworksVisible: true,
    gpuCapacityVisible: true,
  },
  {
    id: "prj-staging",
    name: "aurora-staging",
    status: "ACTIVE",
    organizationId: ORG_ID,
    billingProfileId: BP_ID,
    memberships: [{ sub: mockProfile.sub, roles: ["OWNER"] }],
    services: [{ serviceId: "svc-proxmox-1" }],
  },
  // Seeded mid-deletion so the projects list renders the scheduled-for-deletion
  // treatment (and its "Cancel deletion" action) without needing a mutation.
  {
    id: "prj-icarus",
    name: "icarus-sandbox",
    status: "SCHEDULED_FOR_DELETION",
    organizationId: ORG_ID,
    billingProfileId: BP_ID,
    memberships: [{ sub: mockProfile.sub, roles: ["OWNER"] }],
    services: [{ serviceId: "svc-proxmox-1" }],
  },
]

export const organizations: Organization[] = [
  {
    id: ORG_ID,
    name: "Example Cloud",
    billingProfileId: BP_ID,
    members: [{ sub: mockProfile.sub, roles: ["OWNER"], email: mockProfile.email }],
  },
]

const COMPUTE_TYPES = [
  "SERVER", "SERVER_GROUP", "KEYPAIR", "IMAGE", "VOLUME", "VOLUME_SNAPSHOT",
  "NETWORK", "SUBNET", "PORT", "ROUTER", "SECURITY_GROUP",
]

export const locations: Location[] = [
  {
    serviceId: "svc-proxmox-1",
    region: "RegionOne",
    displayName: "Hanoi (RegionOne)",
    resourceTypes: COMPUTE_TYPES,
    provider: "proxmox",
    serviceName: "ProxCloud Compute",
  },
  {
    serviceId: "svc-proxmox-1",
    region: "RegionTwo",
    displayName: "Da Nang (RegionTwo)",
    resourceTypes: COMPUTE_TYPES,
    provider: "proxmox",
    serviceName: "ProxCloud Compute",
  },
  {
    serviceId: "svc-ceph-1",
    region: "RegionOne",
    displayName: "Hanoi Object Storage",
    resourceTypes: ["BUCKET"],
    provider: "ceph-s3",
    serviceName: "ProxCloud S3",
  },
  {
    serviceId: "svc-dbaas-1",
    region: "RegionOne",
    displayName: "Hanoi Databases",
    resourceTypes: ["DATABASE_CLUSTER"],
    provider: "dbaas",
    serviceName: "ProxCloud DBaaS",
  },
  {
    serviceId: "svc-sandbox-1",
    region: "RegionOne",
    displayName: "Hanoi Sandboxes",
    resourceTypes: ["SANDBOX"],
    provider: "sandbox",
    serviceName: "ProxCloud Sandboxes",
  },
]

// Sidebar service gating — keys are service slugs (AppShell.tsx).
export const uiMenu = {
  id: PID,
  menu: {
    items: {
      compute: { enabled: true },
      image: { enabled: true },
      volumev3: { enabled: true },
      "object-store": { enabled: true },
      network: { enabled: true },
      database: { enabled: true },
      sandbox: { enabled: true },
    },
  },
}

export const features = ["billing", "search"]

export const publicNetworks = [{ id: "net-public-ext", name: "public" }]
