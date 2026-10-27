// Cloud resource endpoints: generic list/create/delete + the action verbs.
import { on } from "../router"
import { db } from "../db"
import {
  availabilityZones, bucketSettings, dnsRecordsets, flavorCategories,
  flavors, imageGrouping, lbListeners, lbMonitors, lbPools, publicImages,
  quotaUsage, s3Credentials, stackEvents, stackResources, volumeTypes,
} from "../fixtures/cloud"
import { locations, publicNetworks } from "../fixtures/platform"

const ok = (result: unknown = null) => ({ data: { result } })

const dbPorts: Record<string, number> = { postgresql: 5432, mysql: 3306, mariadb: 3306, valkey: 6379, ferretdb: 27017, opensearch: 9200, kafka: 9094 }

// --- Listing ----------------------------------------------------------------

on("GET /project/:pid/resource-types", () => ({ data: locations }))
on("GET /project/:pid/public-networks", () => ({ data: publicNetworks }))
on("GET /project/:pid/quota-usage", ({ params, opts }) => {
  const projectQuota = params.pid === "prj-staging"
    ? {
        ...quotaUsage,
        gpu: {
          limits: { "nvidia-a10": 1, "*": 3 },
          usage: { "nvidia-a10": 1, "nvidia-l40s": 2 },
          usageAvailable: true,
        },
      }
    : quotaUsage
  return { data: { ...projectQuota, region: opts.cloud?.region ?? projectQuota.region } }
})
on("GET /project/:pid/gpu-capacity", ({ opts }) => ({
  data: {
    visible: true,
    region: opts.cloud?.region ?? "RegionOne",
    // A superset of the models the project uses/limits, so the merged Quota tiles show the full
    // region catalog: nvidia-a10 has a project limit (quota tile + region-free note); the rest
    // have no limit (region availability tiles), incl. a sold-out model.
    capacity: [
      { model: "nvidia-a10", available: 3, total: 4 },
      { model: "nvidia-a100-80gb", available: 7, total: 16 },
      { model: "nvidia-l40s", available: 0, total: 24 },
    ],
    warnings: [],
  },
}))

on("POST /project/:pid/resource", ({ query }) => {
  const type = query.get("type")
  let items = db.cloud.filter((r) => r.type === type)
  const deviceId = query.get("deviceId")
  if (deviceId) items = items.filter((r) => r.data?.port?.device_id === deviceId)
  const associatedTo = query.get("dataAssociatedTo")
  if (associatedTo) items = items.filter((r) => r.data?.image?.instance_uuid === associatedTo)
  return { data: items }
})

on("GET /project/:pid/cloud/:resourceId", ({ params }) => ({
  data: db.cloud.find((r) => r.id === params.resourceId),
}))

// --- Create / delete --------------------------------------------------------

on("POST /project/:pid/cloud", ({ params, opts }) => {
  const body = opts.body as { type: string; data?: Record<string, any> }
  const id = db.nextId("res-new")
  const name = body.data?.name ?? body.data?.bucketName ?? id
  const kindKey: Record<string, string> = {
    SERVER: "server", SERVER_GROUP: "serverGroup", KEYPAIR: "keypair", IMAGE: "image",
    VOLUME: "volume", VOLUME_SNAPSHOT: "volumeSnapshot", NETWORK: "network", SUBNET: "subnet",
    PORT: "port", ROUTER: "router", SECURITY_GROUP: "securityGroup", LOAD_BALANCER: "loadBalancer",
    DNS_ZONE: "zone", STACK: "stack", BARBICAN_SECRET: "secret", SHARE: "share",
  }
  const key = kindKey[body.type]
  const resource = {
    id,
    type: body.type,
    name,
    status: "BUILD",
    externalId: `ext-${id}`,
    projectId: params.pid,
    createdAt: new Date().toISOString(),
    data: {
      ...(body.data ?? {}),
      ...(key ? { [key]: { id: `os-${id}`, status: "BUILD", ...(body.data ?? {}) } } : {}),
    },
  }
  // Database clusters cache the snake_case sync shape under data.database (the request body is
  // camelCase); the endpoint stays "" so the list demos the "Endpoint pending" polling state.
  if (body.type === "DATABASE_CLUSTER") {
    const d = body.data ?? {}
    resource.data = {
      database: {
        id: `std-${id}`, name, engine: d.engine, version: d.version, replicas: d.replicas ?? 1,
        cpu: d.cpu, memory_gib: d.memoryGiB, storage_gib: d.storageGiB,
        network_id: d.networkId, subnet_id: d.subnetId, allowed_cidrs: d.allowedCidrs ?? [],
        status: "PROGRESSING", sync_status: "Progressing", chart_version: "0.1.0",
        created_at: new Date().toISOString(), endpoint: "", port: dbPorts[d.engine] ?? 5432,
      },
    }
  }
  db.cloud.push(resource)
  return { data: resource }
})

on("DELETE /project/:pid/cloud/:resourceId", ({ params }) => {
  db.cloud = db.cloud.filter((r) => r.id !== params.resourceId)
  return { data: {} }
})

// --- Collection-level actions ------------------------------------------------

on("POST /project/:pid/cloud/action", ({ opts }) => {
  const action = (opts.body as { action?: string })?.action
  switch (action) {
    case "LIST_FLAVORS": return ok(flavors)
    case "PUBLIC_IMAGES": return ok(publicImages)
    case "LIST_AVAILABILITY_ZONES": return ok(availabilityZones)
    case "LIST_VOLUME_TYPES": return ok(volumeTypes)
    case "LIST_TYPES": return ok(volumeTypes)
    default:
      console.warn(`[mock] unknown collection action ${action}`)
      return ok([])
  }
})

// --- Per-resource actions ----------------------------------------------------

// Server detail fixtures: instance action log (LIST_EVENTS) + nova console output.
const serverEvents = [
  { date: "2026-07-10T08:12:00Z", action: "reboot", message: "", requestId: "req-6f2a1c", userId: "usr-dev-001" },
  { date: "2026-06-28T14:03:00Z", action: "stop", message: "", requestId: "req-b81d90", userId: "usr-dev-001" },
  { date: "2026-06-12T16:30:00Z", action: "create", message: "", requestId: "req-3c47ae", userId: "usr-dev-001" },
]

const consoleOutput = [
  "[    0.000000] Linux version 6.8.0-45-generic (buildd@lcy02) (gcc 13.2.0)",
  "[    0.004521] Command line: BOOT_IMAGE=/boot/vmlinuz root=LABEL=cloudimg-rootfs ro console=ttyS0",
  "[    1.284712] systemd[1]: Detected virtualization kvm.",
  "[    2.031870] cloud-init[812]: Cloud-init v. 24.1 running 'init' at Thu, 12 Jun 2026 16:30:41 +0000.",
  "[    2.845003] cloud-init[812]: ci-info: | eth0  | True | 10.0.0.11 | 255.255.255.0 | global |",
  "[    4.190338] cloud-init[1043]: Cloud-init v. 24.1 finished. Datasource DataSourceOpenStack. Up 4.18 seconds",
  "",
  "Ubuntu 24.04 LTS web-01 ttyS0",
  "",
  "web-01 login:",
].join("\n")

on("POST /project/:pid/cloud/:resourceId/action", ({ params, opts }) => {
  const { action, data } = (opts.body ?? {}) as { action?: string; data?: Record<string, any> }
  const r = db.cloud.find((x) => x.id === params.resourceId)

  const setServerStatus = (status: string) => {
    if (!r) return
    r.status = status
    if (r.data?.server) r.data.server.status = status
  }

  // Managed databases first — their verbs (RESIZE) collide with the server ones, and the page
  // reads data.database, not the row status.
  if (r?.type === "DATABASE_CLUSTER") {
    const d = ((r.data ??= {}).database ??= {}) as Record<string, any>
    switch (action) {
      case "GET_CONNECTION_INFO": {
        const host = d.endpoint || "10.0.0.50"
        const port = d.port ?? 5432
        return ok({
          host, port, dbname: "app", username: "app", password: "mock-db-password-1",
          uri: `${d.engine ?? "postgresql"}://app:mock-db-password-1@${host}:${port}/app`,
          engine: d.engine,
        })
      }
      case "RESET_PASSWORD": return ok({ password: "mock-new-password-2" })
      case "RESIZE": d.cpu = data?.cpu; d.memory_gib = data?.memoryGiB; return ok()
      case "UPGRADE": d.version = data?.version; d.status = "PROGRESSING"; return ok("UPGRADING")
      case "RESIZE_STORAGE": d.storage_gib = data?.storageGiB; return ok()
      case "SCALE_REPLICAS": d.replicas = data?.replicas; return ok()
      case "SET_ALLOWED_CIDRS": d.allowed_cidrs = data?.allowedCidrs ?? []; return ok()
      case "SET_SSO": d.status = "UPDATING"; return ok("UPDATING")
      case "SET_AUTOSCALE":
        d.autoscale_enabled = data?.enabled ? 1 : 0
        d.autoscale_max_cpu = data?.maxCpu ?? 0
        d.autoscale_max_memory_gib = data?.maxMemoryGiB ?? 0
        d.autoscale_max_storage_gib = data?.maxStorageGiB ?? 0
        d.status = "UPDATING"
        return ok("UPDATING")
      case "RESTART": return ok()
    }
  }

  switch (action) {
    // Compute lifecycle
    case "START": setServerStatus("ACTIVE"); return ok()
    case "STOP": setServerStatus("SHUTOFF"); return ok()
    case "SOFTREBOOT": setServerStatus("ACTIVE"); return ok()
    case "RESCUE": setServerStatus("RESCUE"); return ok("mock-rescue-password")
    case "UNRESCUE": setServerStatus("ACTIVE"); return ok()
    case "RESIZE": setServerStatus("VERIFY_RESIZE"); return ok()
    case "CONFIRMRESIZE": case "REVERTRESIZE": setServerStatus("ACTIVE"); return ok()
    case "REBUILD": setServerStatus("ACTIVE"); return ok()
    case "SET_PASSWORD": case "RENAME": return ok()
    case "REMOTECONTROL": return ok({ url: "about:blank" })
    case "ATTACH": case "DETACH": return ok()
    case "ATTACH_PORT": case "DETACH_PORT": return ok()
    case "LIST_SECURITY_GROUPS": return ok(db.cloud.filter((x) => x.type === "SECURITY_GROUP"))
    case "ADD_SECURITY_GROUP": case "REMOVE_SECURITY_GROUP": return ok()
    case "LIST_EVENTS": return ok(serverEvents)
    case "SHOW_CONSOLE_OUTPUT": return ok(consoleOutput)

    // Volumes
    case "RETYPE": case "EXTEND": return ok()

    // Shares
    case "EXTEND_SHARE": case "SHRINK_SHARE": return ok()
    case "LIST_ACCESS":
      // Manila access rules come back verbatim (snake_case) — one sample rule so the manage sheet has data.
      return ok([{ id: "rule-1", access_type: "ip", access_to: "10.0.0.0/24", access_level: "rw", state: "active" }])
    case "GRANT_ACCESS": case "REVOKE_ACCESS": return ok()

    // Buckets
    case "LIST_OBJECTS": {
      const folder = data?.folderName
      return ok(folder ? db.bucketObjects.filter((o) => !o.directory && o.name.startsWith(folder)) : db.bucketObjects)
    }
    case "IS_BUCKET_PUBLIC": return ok(false)
    case "MAKE_BUCKET_PUBLIC": case "MAKE_BUCKET_PRIVATE": return ok()
    case "CREATE_FOLDER":
      db.bucketObjects.push({ name: `${data?.folderName ?? "folder"}/`, displayName: data?.folderName ?? "folder", directory: true })
      return ok()
    case "DELETE_OBJECT":
      db.bucketObjects = db.bucketObjects.filter((o) => o.name !== data?.objectName)
      return ok()
    case "DOWNLOAD": return ok({ url: "about:blank" })
    case "GET_SETTINGS": return ok(bucketSettings)
    case "ENABLE_WEBSITE": return ok({ enabled: true, indexDocument: "index.html" })
    case "DISABLE_WEBSITE": return ok({ enabled: false })

    // Networks / routers / ports / security groups
    case "GET_SERVERS": return ok(db.cloud.filter((x) => x.type === "SERVER"))
    case "UPDATE": return ok()
    case "ADD_INTERFACE": case "DELETE_INTERFACE":
    case "ADD_EXTERNAL_GATEWAY": case "DELETE_EXTERNAL_GATEWAY": return ok()
    case "ASSIGN": case "UNASSIGN": return ok()
    case "LIST_RULES": return ok(r?.data?.securityGroup?.security_group_rules ?? [])
    case "ADD_RULE": case "DELETE_RULE": return ok()

    // Load balancers
    case "GET_LISTENERS": return ok(lbListeners)
    case "GET_POOLS": return ok(lbPools)
    case "GET_MONITORS": return ok(lbMonitors)
    case "CREATE_LISTENER": case "DELETE_LISTENER": case "CREATE_POOL": case "DELETE_POOL":
    case "ADD_MEMBER": case "DELETE_MEMBER": case "ADD_MONITOR": case "DELETE_MONITOR": return ok()

    // DNS
    case "GET_RECORDSETS": return ok(dnsRecordsets)
    case "CREATE_RECORDSET": case "DELETE_RECORDSET": return ok()

    // Stacks
    case "SUSPEND_STACK": case "RESUME_STACK": return ok()
    case "GET_TEMPLATE": return ok({ template: "heat_template_version: 2021-04-16\nresources: {}\n" })
    case "LIST_STACK_EVENTS": return ok(stackEvents)
    case "LIST_RESOURCES": return ok(stackResources)

    default:
      console.warn(`[mock] unknown resource action ${action}`)
      return ok(null)
  }
})

// --- Misc cloud endpoints -----------------------------------------------------

on("PUT /project/:pid/cloud/:resourceId/metadata", () => ({ data: {} }))
on("POST /project/:pid/image/:imageId/upload", () => ({ data: {} }))
on("POST /project/:pid/cloud/:resourceId/upload-bucket-file", ({ query }) => {
  const name = query.get("objectName") ?? "upload.bin"
  db.bucketObjects.push({ name, displayName: name.split("/").pop() ?? name, sizeInBytes: 1024, mimeType: "application/octet-stream", directory: false, lastModified: new Date().toISOString() })
  return { data: {} }
})

// --- S3 credentials / keys -----------------------------------------------------

on("GET /project/:pid/s3-credentials", () => ({ data: s3Credentials }))
on("POST /project/:pid/s3-credentials/rotate", () => ({ data: {} }))
on("GET /project/:pid/s3-keys", () => ({ data: db.s3Keys }))
on("POST /project/:pid/s3-keys", ({ opts }) => {
  const key = {
    id: db.nextId("s3k"),
    name: (opts.body as { name?: string })?.name ?? "new-key",
    rgwUid: "prj-aurora$new",
    accessKey: `MOCK${Math.floor(Math.random() * 1e10).toString(36).toUpperCase()}`,
    secretKey: "mockSecretKeyGenerated0000000000000000",
    createdAt: new Date().toISOString(),
  }
  db.s3Keys.push(key)
  return { data: key }
})
on("POST /project/:pid/s3-keys/:id/rotate", () => ({ data: {} }))
on("DELETE /project/:pid/s3-keys/:id", ({ params }) => {
  db.s3Keys = db.s3Keys.filter((k) => k.id !== params.id)
  return { data: {} }
})

// --- Catalog ---------------------------------------------------------------------

on("GET /flavor-categories", () => ({ data: flavorCategories }))
on("GET /groups/images", () => ({ data: imageGrouping }))
