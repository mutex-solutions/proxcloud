// Cloud resource endpoints: generic list/create/delete + the action verbs.
import { ApiError } from "@/lib/api"
import { on } from "../router"
import { db } from "../db"
import {
  bucketSettings, flavorCategories, flavors, imageGrouping, publicImages,
  quotaUsage, s3Credentials, volumeTypes,
} from "../fixtures/cloud"
import { locations, publicNetworks } from "../fixtures/platform"

const ok = (result: unknown = null) => ({ data: { result } })

const dbPorts: Record<string, number> = { postgresql: 5432, mysql: 3306, mariadb: 3306, valkey: 6379, ferretdb: 27017, opensearch: 9200, kafka: 9094 }

// --- Listing ----------------------------------------------------------------

on("GET /project/:pid/resource-types", () => ({ data: locations }))
// prj-staging is a site that serves less (handlers/platform.ts): its operator gave the project no public network.
on("GET /project/:pid/public-networks", ({ params }) => ({ data: params.pid === "prj-staging" ? [] : publicNetworks }))
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
    PORT: "port", ROUTER: "router", SECURITY_GROUP: "securityGroup",
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
      ...(key ? { [key]: { id: `vm-${id}`, status: "BUILD", ...(body.data ?? {}) } } : {}),
    },
  }
  // Database clusters cache the snake_case sync shape under data.database (the request body is
  // camelCase); the endpoint stays "" so the list demos the "Endpoint pending" polling state.
  if (body.type === "DATABASE_CLUSTER") {
    const d = body.data ?? {}
    resource.data = {
      database: {
        id: `pc-d-${id}`, name, engine: d.engine, version: d.version, replicas: d.replicas ?? 1,
        cpu: d.cpu, memory_gib: d.memoryGiB, storage_gib: d.storageGiB,
        allowed_cidrs: d.allowedCidrs ?? [],
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

on("POST /project/:pid/cloud/action", ({ params, opts }) => {
  const action = (opts.body as { action?: string })?.action
  switch (action) {
    // What the site lacks for a load balancer: nothing on prj-aurora, a public network and the image on prj-staging.
    case "LIST_LOAD_BALANCER_WARNINGS":
      return ok(params.pid === "prj-staging"
        ? [
            "No public network is available to this project, and a load balancer takes its public address from one. Ask your operator to add a public network to the provider and to allow it for this project.",
            "The image a load balancer runs on, debian-13, is not in this site's image catalog. Ask your operator to add it, or to name another Debian family image in the provider setting lbImage.",
          ]
        : [])
    case "LIST_FLAVORS": return ok(flavors)
    case "PUBLIC_IMAGES": return ok(publicImages)
    case "LIST_VOLUME_TYPES": return ok(volumeTypes)
    case "LIST_TYPES": return ok(volumeTypes)
    // The file server sits where this project's networks cannot reach it.
    case "LIST_SHARE_WARNINGS":
      return ok(["Servers on this project's private networks cannot reach the file server 192.0.2.100: it is on a network this site keeps tenant networks out of. A share made here can be mounted only from a server with a public IP that has a route to it. Ask your operator."])
    default:
      console.warn(`[mock] unknown collection action ${action}`)
      return ok([])
  }
})

// --- Per-resource actions ----------------------------------------------------

// Server detail fixtures: instance action log (LIST_EVENTS) + serial console output.
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
  "[    4.190338] cloud-init[1043]: Cloud-init v. 24.1 finished. Datasource DataSourceNoCloud. Up 4.18 seconds",
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

  // Managed Kubernetes: the last Ready node of a group is only replaced when confirmed.
  if (r?.type === "KUBERNETES_CLUSTER" && action === "REPLACE_MACHINE") {
    if (String(data?.machine ?? "").endsWith("-ready") && !data?.confirm) {
      throw new ApiError(409, 409, "This machine is the last Ready node of its node group: its pods have nowhere to run in the group until the new node is Ready. Confirm to replace it anyway.", { reason: "last_ready_node" })
    }
    return ok("REPLACING")
  }

  // A cluster's load balancer: the controller's last call and the balancer its Services built (null: no controller).
  if (r?.type === "KUBERNETES_CLUSTER" && action === "GET_LOAD_BALANCER") {
    if (!r.data?.cluster?.load_balancer) return ok(null)
    const lb = db.cloud.find((x) => x.type === "LOAD_BALANCER" && x.data?.loadbalancer?.cluster?.id === r.externalId)
    return ok({
      controllerSince: "2026-11-20T08:00:00Z", controllerLastSeen: new Date(Date.now() - 20000).toISOString(),
      ...(lb ? { balancerId: lb.id, balancer: lb.data?.loadbalancer } : {}),
    })
  }

  switch (action) {
    // Compute lifecycle
    case "START": setServerStatus("ACTIVE"); return ok()
    case "STOP": setServerStatus("SHUTOFF"); return ok()
    case "SOFTREBOOT": case "HARDREBOOT": setServerStatus("ACTIVE"); return ok()
    case "RESCUE": setServerStatus("RESCUE"); return ok("mock-rescue-password")
    case "UNRESCUE": setServerStatus("ACTIVE"); return ok()
    case "RESIZE": setServerStatus("VERIFY_RESIZE"); return ok()
    case "CONFIRMRESIZE": case "REVERTRESIZE": setServerStatus("ACTIVE"); return ok()
    case "REBUILD": setServerStatus("ACTIVE"); return ok()
    case "SET_PASSWORD": case "RENAME": return ok()
    case "REMOTECONTROL": return ok({ url: "about:blank" })
    case "SERIAL_CONSOLE": return ok({ url: "ws://localhost:5273/mock-serial" })
    // A cloud image: the display is the serial port. A spec restarts the server by setting __mockServerUptime.
    case "GET_CONSOLE_STATE":
      return ok({ status: "running", serialDisplay: true, uptime: (globalThis as { __mockServerUptime?: number }).__mockServerUptime ?? 86_400 })
    case "LIST_BACKUPS": return ok([])
    case "CREATE_BACKUP": return ok({ id: "nas-backup:backup/vzdump-qemu-101-mock.vma.zst" })
    case "ATTACH": case "DETACH": return ok()
    case "ATTACH_PORT": case "DETACH_PORT": return ok()
    case "LIST_SECURITY_GROUPS": return ok(db.cloud.filter((x) => x.type === "SECURITY_GROUP"))
    case "ADD_SECURITY_GROUP": case "REMOVE_SECURITY_GROUP": return ok()
    case "LIST_EVENTS": return ok(serverEvents)
    case "SHOW_CONSOLE_OUTPUT": return ok(consoleOutput)

    // Volumes
    case "RETYPE": case "EXTEND": return ok()

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

    // Load balancers: the members as the appliance holding the address sees them, and a replace that starts.
    case "GET_HEALTH":
      return { data: {
        // A pool checked over HTTP on a cluster's health port (policy Local) has one worker with a pod.
        result: (r?.data?.loadbalancer?.pools ?? []).flatMap((p: any) => (p.members ?? []).map((m: any, i: number) => ({
          pool: p.name, serverId: m.serverId, address: m.address, port: m.port,
          status: p.healthCheck?.type === "none" ? "no check" : m.node && p.healthCheck?.type === "http" && i > 0 ? "DOWN" : "UP",
        }))),
        appliances: r?.data?.loadbalancer?.appliances ?? [],
      } }
    case "REPLACE_APPLIANCES":
      if (r?.data?.loadbalancer) r.data.loadbalancer.replacing = new Date().toISOString()
      return { data: r }
    // The row then holds what was asked, as the API stores it.
    case "UPDATE_CONFIG":
      ;(globalThis as { __lbConfig?: unknown }).__lbConfig = data // what the page sent, for the e2e spec
      if (r?.data?.loadbalancer) Object.assign(r.data.loadbalancer, { listeners: data?.listeners ?? [], pools: data?.pools ?? [] })
      return { data: r }

    // Networks / routers / ports / security groups
    case "GET_SERVERS": return ok(db.cloud.filter((x) => x.type === "SERVER"))
    case "UPDATE": return ok()
    case "ADD_INTERFACE": case "DELETE_INTERFACE":
    case "ADD_EXTERNAL_GATEWAY": case "DELETE_EXTERNAL_GATEWAY": return ok()
    case "LIST_RULES": return ok(r?.data?.securityGroup?.security_group_rules ?? [])
    // Rules as Proxmox keeps them: a new rule lands first. UPDATE_RULE is the API's add-then-delete.
    case "ADD_RULE": case "UPDATE_RULE": case "DELETE_RULE": {
      const sg = r?.data?.securityGroup
      if (!sg) return ok()
      const rules = ((sg.security_group_rules ?? []) as Array<Record<string, unknown>>).filter((x) => action === "ADD_RULE" || x.id !== data?.ruleId)
      if (action !== "DELETE_RULE") {
        rules.unshift({
          id: db.nextId("sgr"), direction: data?.direction ?? "ingress", protocol: data?.protocol ?? "",
          port_range: [data?.portRangeMin, data?.portRangeMax].filter(Boolean).join(":"),
          remote_ip_prefix: data?.remoteIpPrefix ?? "", description: data?.description ?? "",
        })
      }
      sg.security_group_rules = rules
      return ok({})
    }

    default:
      console.warn(`[mock] unknown resource action ${action}`)
      return ok(null)
  }
})

// --- Misc cloud endpoints -----------------------------------------------------

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

// --- TLS certificates of a project's load balancers --------------------------
// What each certificate says, and never its key. prj-aurora holds one with months left and one that expires in days.
type MockCert = {
  id: string; name: string; createdAt: string; subject: string; issuer: string; dnsNames: string[]
  notBefore: string; notAfter: string; fingerprint: string; keyType: string; chainLength: number
}
const inDays = (n: number) => new Date(Date.now() + n * 86_400_000).toISOString()
const certificates: Record<string, MockCert[]> = {
  "prj-aurora": [
    { id: "c0ffee00000000000000000000000001", name: "shop-2027", createdAt: inDays(-20), subject: "CN=shop.example.org", issuer: "CN=Example Issuing CA",
      dnsNames: ["shop.example.org", "www.shop.example.org"], notBefore: inDays(-20), notAfter: inDays(240), fingerprint: "9f".repeat(32), keyType: "ECDSA P-256", chainLength: 2 },
    { id: "c0ffee00000000000000000000000002", name: "api-2026", createdAt: inDays(-78), subject: "CN=api.example.org", issuer: "CN=Example Issuing CA",
      dnsNames: ["api.example.org"], notBefore: inDays(-78), notAfter: inDays(12.5), fingerprint: "3c".repeat(32), keyType: "RSA 2048", chainLength: 2 },
  ],
}
const certUsedBy = (pid: string, id: string): string[] => db.cloud
  .filter((x) => x.type === "LOAD_BALANCER" && pid === "prj-aurora" && (x.data?.loadbalancer?.listeners ?? []).some((l: any) => (l.certificates ?? []).includes(id)))
  .map((x) => x.data?.loadbalancer?.name as string)
on("GET /project/:pid/certificates", ({ params }) => ({
  data: (certificates[params.pid] ?? []).map((c) => ({ ...c, usedBy: certUsedBy(params.pid, c.id) })),
}))
on("POST /project/:pid/certificates", ({ params, opts }) => {
  const body = (opts.body ?? {}) as { name?: string; certificate?: string; privateKey?: string }
  if (!/^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/.test(body.name ?? "")) throw new ApiError(400, 400, "Certificate names use lowercase letters, digits and dashes, at most 32 characters")
  if (!body.certificate?.includes("BEGIN CERTIFICATE")) throw new ApiError(400, 400, "The certificate is not PEM text: no BEGIN CERTIFICATE line was found")
  if (!body.privateKey?.includes("PRIVATE KEY")) throw new ApiError(400, 400, "The private key is not PEM text with one key in it")
  const list = (certificates[params.pid] ??= [])
  if (list.some((c) => c.name === body.name)) throw new ApiError(409, 409, `This project already has a certificate named ${body.name}`)
  const made: MockCert = {
    id: db.nextId("cert"), name: body.name!, createdAt: inDays(0), subject: "CN=new.example.org", issuer: "CN=Example Issuing CA",
    dnsNames: ["new.example.org"], notBefore: inDays(-1), notAfter: inDays(89), fingerprint: "7a".repeat(32), keyType: "RSA 2048", chainLength: 2,
  }
  list.unshift(made)
  return { data: { ...made, usedBy: [] } } // the answer of an upload holds what the certificate says, and no key
})
on("DELETE /project/:pid/certificates/:certId", ({ params }) => {
  const used = certUsedBy(params.pid, params.certId)
  if (used.length) throw new ApiError(409, 409, `This certificate is used by a listener of load balancer ${used.join(", ")}. Take it off the listener first`)
  certificates[params.pid] = (certificates[params.pid] ?? []).filter((c) => c.id !== params.certId)
  return { data: {} }
})
