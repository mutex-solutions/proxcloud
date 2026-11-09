import { useEffect, useMemo, useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Building2, CircuitBoard, CreditCard, Pause, Play, Plus, RefreshCw, Server, Trash2, Users } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { ApiError, apiFetch } from "@/lib/api"
import { fmtDate, fmtDateTime } from "@/lib/format"
import { useAdminGet, useAdminList, useTabParam } from "@/lib/hooks"

// GET /admin/project/{id} (handler.go rawByID "project") — the project doc shaped (_id→id).
type ProjectDoc = {
  id?: string
  name?: string
  status?: string
  disabledByOperator?: boolean
  organizationId?: string
  billingProfileId?: string
  memberships?: Array<{ sub?: string; role?: string }>
  // proxmox bindings carry provider/region/pool; ceph-s3 bindings carry provider/region/rgwUid.
  services?: Array<{ serviceId?: string; provider?: string; region?: string; pool?: string; rgwUid?: string }>
  // Per provider id: a provisioning attempt that failed and is retried when the project is next opened.
  provisionFailures?: Record<string, { message?: string; at?: string }>
  // absent/null = all public networks allowed; array = allow-list of public network ids.
  publicNetworkIds?: string[] | null
  // false/absent = the client can't choose an external network (server auto-picks); true = client picks.
  publicNetworksVisible?: boolean
  // false/absent = the client dashboard does not show the region GPU capacity; true = it does.
  gpuCapacityVisible?: boolean
  // Admin-managed per-project quota; gpu = {model alias (or "*") → device limit}, plus the
  // Proxmox resource keys (RESOURCE_QUOTA_KEYS). Other stored keys are preserved on save.
  quota?: { gpu?: Record<string, number> } & Record<string, unknown>
  createdAt?: string
}

// GET /admin/service/{id}/gpu-info (cloudadmin.go gpuInfo) — per-region GPU capacity.
type GpuRegionCapacity = { region: string; gpus: Array<{ name: string; total: number; inUse: number }> }

// GET /admin/project/{id}/gpu-usage — the cache snapshot used by the GPU quota gate.
type GPUUsageResponse = {
  usage: Record<string, number>
  usageAvailable: boolean
}

// GET /admin/cloud-resource/public-networks/{externalServiceId} (cloudadmin.go publicNetworks) —
// the provider's router:external networks.
type PublicNetwork = {
  id?: string
  name?: string
}

// GET /admin/project/{id}/members (projectmut.go projectMembers) — shaped user docs.
type MemberUser = {
  id?: string
  sub?: string
  email?: string
  firstName?: string
  lastName?: string
}

// GET /admin/cloud-resource/project/{id} → cloud.CloudResource.
type CloudResource = {
  id?: string
  type?: string
  externalId?: string
  region?: string
  data?: Record<string, unknown>
  createdAt?: string
}

// GET /admin/billing-profile (clientarea_reads.go billingProfileAdminList) — for the change dialog.
type BillingProfile = {
  id?: string
  name?: string
  email?: string
  status?: string
  currency?: string
}

// GET /admin/user (handler.go listRaw "users") — for the add-member picker.
type AdminUser = {
  id?: string
  sub?: string
  email?: string
  firstName?: string
  lastName?: string
}

function dataField(cr: CloudResource, key: "name" | "status"): string | undefined {
  for (const v of Object.values(cr.data ?? {})) {
    if (v && typeof v === "object") {
      const s = (v as Record<string, unknown>)[key]
      if (typeof s === "string" && s) return s
    }
  }
  return undefined
}

function normalizeGPUModel(value: string): string {
  const trimmed = value.trim()
  return trimmed === "*" ? "*" : trimmed.toLowerCase().replaceAll("_", "-")
}

// The resource keys the Proxmox create paths enforce (externalservice.PVEQuotaKeys), then the sandbox keys
// (externalservice.SandboxQuotaKeys): a blank sandbox key falls back to the sandbox provider's default, never unlimited.
const RESOURCE_QUOTA_KEYS: Array<{ key: string; label: string; blank?: string }> = [
  { key: "servers", label: "Servers" },
  { key: "vcpus", label: "vCPUs" },
  { key: "ramMb", label: "RAM (MiB)" },
  { key: "diskGb", label: "Disk (GiB)" },
  { key: "networks", label: "Networks" },
  { key: "floatingIps", label: "Public IPs" },
  { key: "sandboxes", label: "Sandboxes at once", blank: "Provider default (5)" },
  { key: "sandboxVcpus", label: "Sandbox vCPUs", blank: "Provider default (16)" },
  { key: "sandboxRamMb", label: "Sandbox RAM (MiB)", blank: "Provider default (32768)" },
  { key: "sandboxGpus", label: "Sandbox GPUs", blank: "Provider default (0)" },
]

function parseGPUQuotaLimit(value: string): number | undefined {
  if (!/^\d+$/.test(value.trim())) return undefined
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) ? parsed : undefined
}

function Field({ label, value, mono }: { label: string; value?: React.ReactNode; mono?: boolean }) {
  return (
    <div>
      <p className="text-eyebrow mb-1">{label}</p>
      <p className={mono ? "font-mono text-xs" : "text-sm"}>{value || "—"}</p>
    </div>
  )
}

function ErrorPanel({ error }: { error: unknown }) {
  return (
    <div className="rounded-lg border bg-muted/40 p-6 text-sm text-muted-foreground">
      {error instanceof Error ? error.message : "Something went wrong"}
    </div>
  )
}

export default function ProjectDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const qc = useQueryClient()

  const projectPath = `/admin/project/${id}`
  const [tab, setTab] = useTabParam("overview")
  const { data: project, isLoading, error } = useAdminGet<ProjectDoc>(projectPath, !!id)
  // GET /admin/project/{id}/resources/counts (projectmut.go projectResourceCounts) — {TYPE: n, TOTAL: n}.
  const counts = useAdminGet<Record<string, number>>(`${projectPath}/resources/counts`, !!id)
  const members = useAdminList<MemberUser>(`${projectPath}/members`, !!id)
  const resources = useAdminList<CloudResource>(`/admin/cloud-resource/project/${id}`, !!id)
  const gpuUsage = useAdminGet<GPUUsageResponse>(`${projectPath}/gpu-usage`, !!id && tab === "quota")

  const [statusConfirm, setStatusConfirm] = useState<"ENABLED" | "DISABLED" | null>(null)
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [confirmNow, setConfirmNow] = useState(false)
  const [bpOpen, setBpOpen] = useState(false)
  const [bpChoice, setBpChoice] = useState("")
  const [orgOpen, setOrgOpen] = useState(false)
  const [orgChoice, setOrgChoice] = useState("")
  const [addMemberOpen, setAddMemberOpen] = useState(false)
  const [memberChoice, setMemberChoice] = useState("")
  const [memberRole, setMemberRole] = useState("MEMBER")
  const [memberToRemove, setMemberToRemove] = useState<MemberUser | null>(null)
  const [attachOpen, setAttachOpen] = useState(false)
  const [attachChoice, setAttachChoice] = useState("")

  const bps = useAdminList<BillingProfile>("/admin/billing-profile", bpOpen)
  const orgsList = useAdminList<{ id: string; name?: string }>("/admin/organizations", orgOpen)
  const users = useAdminList<AdminUser>("/admin/user", addMemberOpen)
  const providers = useAdminList<{ id: string; name?: string; status?: string; config?: { provider?: string } }>(
    "/admin/service",
    attachOpen,
  )

  const externalServiceId = project?.services?.find((s) => s.serviceId)?.serviceId ?? ""
  const publicNets = useAdminList<PublicNetwork>(
    `/admin/cloud-resource/public-networks/${externalServiceId}`,
    !!externalServiceId,
  )
  const [allPublicNets, setAllPublicNets] = useState(true)
  const [publicNetChoice, setPublicNetChoice] = useState<string[]>([])
  const [publicNetsVisible, setPublicNetsVisible] = useState(false)
  useEffect(() => {
    setAllPublicNets(!project?.publicNetworkIds)
    setPublicNetChoice(project?.publicNetworkIds ?? [])
    setPublicNetsVisible(project?.publicNetworksVisible === true)
  }, [project?.publicNetworkIds, project?.publicNetworksVisible])

  const enabled = (project?.status ?? "").toUpperCase() === "ENABLED"

  // GPU quota tab state: rows derive from project.quota.gpu; region capacity is offered
  // as a convenience only, while the free-text input accepts the actual GPU model alias.
  const gpuInfo = useAdminList<GpuRegionCapacity>(
    `/admin/service/${externalServiceId}/gpu-info`,
    !!externalServiceId && tab === "quota",
  )
  const [quotaRows, setQuotaRows] = useState<Array<{ model: string; limit: string }>>([])
  const [resourceQuota, setResourceQuota] = useState<Record<string, string>>({})
  useEffect(() => {
    const next: Record<string, string> = {}
    for (const { key } of RESOURCE_QUOTA_KEYS) {
      const v = project?.quota?.[key]
      next[key] = v != null ? String(v) : ""
    }
    setResourceQuota(next)
  }, [project?.quota])
  const resourceQuotaValid = RESOURCE_QUOTA_KEYS.every(
    ({ key }) => !(resourceQuota[key] ?? "").trim() || parseGPUQuotaLimit(resourceQuota[key]) !== undefined,
  )
  const [quotaModel, setQuotaModel] = useState("")
  const [quotaLimit, setQuotaLimit] = useState("")
  useEffect(() => {
    const canonical = new Map<string, { limit: string; source: string; exact: boolean }>()
    for (const [model, limit] of Object.entries(project?.quota?.gpu ?? {})) {
      const normalized = normalizeGPUModel(model)
      const source = model.trim()
      const exact = source === normalized
      const previous = canonical.get(normalized)
      if (normalized && (!previous || exact || (!previous.exact && source < previous.source))) {
        canonical.set(normalized, { limit: String(limit), source, exact })
      }
    }
    setQuotaRows([...canonical].map(([model, value]) => ({ model, limit: value.limit })))
  }, [project?.quota])
  const rawGPUUsage = gpuUsage.data?.usage
  const normalizedGPUUsage = useMemo(() => {
    const result: Record<string, number> = {}
    for (const [model, used] of Object.entries(rawGPUUsage ?? {})) {
      const normalized = normalizeGPUModel(model)
      if (normalized && Number.isFinite(used) && used >= 0) result[normalized] = used
    }
    return result
  }, [rawGPUUsage])
  const [gpuCapVisible, setGpuCapVisible] = useState(false)
  useEffect(() => {
    setGpuCapVisible(project?.gpuCapacityVisible === true)
  }, [project?.gpuCapacityVisible])
  const gpuModelOptions = [
    "*",
    ...new Set([
      ...(gpuInfo.data?.data ?? []).flatMap((r) => r.gpus.map((g) => normalizeGPUModel(g.name))),
      ...Object.keys(normalizedGPUUsage),
    ]),
  ]
    .filter((model) => model && !quotaRows.some((row) => normalizeGPUModel(row.model) === model))
    .sort((a, b) => (a === "*" ? -1 : b === "*" ? 1 : a.localeCompare(b)))

  const gpuStatusRows = useMemo(() => {
    const exactModels = new Set<string>()
    const exactLimits = new Map<string, number>()
    let fallbackConfigured = false
    let fallbackLimit: number | undefined
    for (const row of quotaRows) {
      const model = normalizeGPUModel(row.model)
      const limit = parseGPUQuotaLimit(row.limit)
      if (model === "*") {
        fallbackConfigured = true
        fallbackLimit = limit
      } else if (model) {
        exactModels.add(model)
        if (limit !== undefined) exactLimits.set(model, limit)
      }
    }

    // Region GPU capacity (gpu-info), aggregated per canonical model across the
    // service's regions — so the rows can list the full region catalog and a model with no
    // project limit shows the region's free/total instead of a bare "Unlimited".
    const capacityByModel = new Map<string, { total: number; inUse: number }>()
    for (const region of gpuInfo.data?.data ?? []) {
      for (const gpu of region.gpus ?? []) {
        const model = normalizeGPUModel(gpu.name)
        if (!model) continue
        const previous = capacityByModel.get(model) ?? { total: 0, inUse: 0 }
        capacityByModel.set(model, { total: previous.total + gpu.total, inUse: previous.inUse + gpu.inUse })
      }
    }

    const models = [...new Set([...capacityByModel.keys(), ...exactModels, ...Object.keys(normalizedGPUUsage)])]
      .sort((a, b) => a.localeCompare(b))
    const rows = models.map((model) => {
      const hasExact = exactModels.has(model)
      const invalid = hasExact ? !exactLimits.has(model) : fallbackConfigured && fallbackLimit === undefined
      const cap = capacityByModel.get(model)
      const capacity = cap ? { total: cap.total, available: Math.max(0, cap.total - cap.inUse) } : undefined
      return {
        model,
        used: normalizedGPUUsage[model] ?? 0,
        limit: hasExact ? exactLimits.get(model) : fallbackLimit,
        invalid,
        capacity,
        source: invalid
          ? "Invalid draft"
          : hasExact
            ? "Exact model limit"
            : fallbackLimit !== undefined
              ? "Fallback limit (*)"
              : capacity
                ? "Region availability · no limit"
                : "Unlimited",
      }
    })
    // The "*" catch-all only makes sense when NOT already listing the full region catalog.
    if (fallbackConfigured && capacityByModel.size === 0 && !models.some((model) => !exactModels.has(model))) {
      rows.push({
        model: "*",
        used: 0,
        limit: fallbackLimit,
        invalid: fallbackLimit === undefined,
        capacity: undefined,
        source: fallbackLimit === undefined ? "Invalid draft" : "Per unlisted model",
      })
    }
    return rows
  }, [normalizedGPUUsage, quotaRows, gpuInfo.data])

  const gpuUsageAvailable = gpuUsage.data?.usageAvailable === true
  const totalGPUUsed = Object.values(normalizedGPUUsage).reduce((total, used) => total + used, 0)
  const normalizedQuotaModel = normalizeGPUModel(quotaModel)
  const newQuotaLimit = parseGPUQuotaLimit(quotaLimit)
  const duplicateQuotaModel = quotaRows.some((row) => normalizeGPUModel(row.model) === normalizedQuotaModel)
  const canAddQuota = !!normalizedQuotaModel && newQuotaLimit !== undefined && !duplicateQuotaModel
  const quotaDraftValid = quotaRows.every(
    (row) => !!normalizeGPUModel(row.model) && parseGPUQuotaLimit(row.limit) !== undefined,
  )

  const invalidateProject = () => {
    qc.invalidateQueries({ queryKey: ["admin-get", projectPath] })
    qc.invalidateQueries({ queryKey: ["admin-list", `${projectPath}/members`] })
    qc.invalidateQueries({ queryKey: ["admin-list", "/admin/project"] })
  }

  // POST /admin/project/{id}/{ENABLED|DISABLED} (projectmut.go projectUpdateStatus).
  const updateStatus = useMutation({
    mutationFn: (status: "ENABLED" | "DISABLED") => apiFetch(`${projectPath}/${status}`, { method: "POST" }),
    onSuccess: (_d, status) => {
      toast.success(status === "ENABLED" ? "Project enabled" : "Project disabled")
      setStatusConfirm(null)
      invalidateProject()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // GET /admin/project/{id}/external-service/{esid} (projectmut.go projectAddExternalService):
  // bootstraps the project onto that provider — a PVE pool for proxmox, an RGW user + stored S3
  // credential for ceph-s3, a namespace for kamaji/dbaas. Idempotent, so a re-attach is safe.
  const attachService = useMutation({
    mutationFn: (esID: string) => apiFetch(`${projectPath}/external-service/${esID}`),
    onSuccess: () => {
      toast.success("Cloud provider attached")
      setAttachOpen(false)
      setAttachChoice("")
      invalidateProject()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // POST /admin/project/{id}/sync (projectmut.go projectSync).
  const syncProject = useMutation({
    mutationFn: () => apiFetch(`${projectPath}/sync`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Project synced")
      qc.invalidateQueries({ queryKey: ["admin-list", `/admin/cloud-resource/project/${id}`] })
      qc.invalidateQueries({ queryKey: ["admin-get", `${projectPath}/resources/counts`] })
      qc.invalidateQueries({ queryKey: ["admin-get", `${projectPath}/gpu-usage`] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // DELETE /admin/project/{id} (projectmut.go projectScheduleDeletion — cloud pre-check is a 501
  // seam on this deployment).
  // DELETE …/now flags the project for the deletion job's next sweep instead of the grace period.
  const deleteProject = useMutation({
    mutationFn: (now: boolean) => apiFetch(now ? `${projectPath}/now` : projectPath, { method: "DELETE" }),
    onSuccess: (_d, now) => {
      toast.success(now ? "Project deletion started" : "Project deletion scheduled")
      navigate("/clients/projects")
    },
    onError: (e: Error) => {
      setDeleteOpen(false)
      setConfirmNow(false)
      if (e instanceof ApiError && e.status === 501) {
        toast.info("Not available: project deletion is not supported on this deployment.")
      } else {
        toast.error(e.message)
      }
    },
  })

  // PUT /admin/project/{id} (projectmut.go projectUpdate) — overwrites name/billingProfileId/
  // organizationId together, so resend the current name + organizationId alongside the new bp.
  const changeBp = useMutation({
    mutationFn: () =>
      apiFetch(projectPath, {
        method: "PUT",
        body: {
          name: project?.name ?? "",
          organizationId: project?.organizationId ?? "",
          billingProfileId: bpChoice,
        },
      }),
    onSuccess: () => {
      toast.success("Billing profile updated")
      setBpOpen(false)
      setBpChoice("")
      invalidateProject()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // PUT /admin/project/{id} — assign/reassign the project's organization. Overwrites
  // name/billingProfileId/organizationId together, so resend the current name + bp.
  // Lets an operator adopt an imported (org-less) project into an organization.
  const changeOrg = useMutation({
    mutationFn: () =>
      apiFetch(projectPath, {
        method: "PUT",
        body: {
          name: project?.name ?? "",
          organizationId: orgChoice,
          billingProfileId: project?.billingProfileId ?? "",
        },
      }),
    onSuccess: () => {
      toast.success("Organization updated")
      setOrgOpen(false)
      setOrgChoice("")
      invalidateProject()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // PUT /admin/project/{id}/quota (projectquota.go) — replaces the whole quota doc: resource keys +
  // {gpu:{model→limit}} (other stored keys preserved). Enforcement is the cloud create/resize gate.
  // An empty object clears the quota; a Proxmox project then uses its provider's default quota.
  const saveQuota = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = { ...(project?.quota ?? {}) }
      delete body.gpu
      for (const { key, label } of RESOURCE_QUOTA_KEYS) {
        const raw = (resourceQuota[key] ?? "").trim()
        if (!raw) {
          delete body[key]
          continue
        }
        const limit = parseGPUQuotaLimit(raw)
        if (limit === undefined) throw new Error(`${label} limit must be a non-negative whole number`)
        body[key] = limit
      }
      const gpu: Record<string, number> = {}
      for (const row of quotaRows) {
        const model = normalizeGPUModel(row.model)
        const limit = parseGPUQuotaLimit(row.limit)
        if (!model || limit === undefined) throw new Error("GPU limits must be non-negative whole numbers")
        gpu[model] = limit
      }
      if (Object.keys(gpu).length) body.gpu = gpu
      return apiFetch(`${projectPath}/quota`, { method: "PUT", body })
    },
    onSuccess: () => {
      toast.success("Project quota saved")
      invalidateProject()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // PUT /admin/project/{id}/gpu-capacity-visible — toggles whether the client dashboard shows the
  // region GPU capacity for this project. Saved immediately on toggle.
  const saveGpuCapVisible = useMutation({
    mutationFn: (visible: boolean) =>
      apiFetch(`${projectPath}/gpu-capacity-visible`, { method: "PUT", body: { gpuCapacityVisible: visible } }),
    onSuccess: () => {
      toast.success("GPU capacity visibility updated")
      invalidateProject()
    },
    onError: (e: Error) => {
      setGpuCapVisible((v) => !v) // revert optimistic toggle
      toast.error(e.message)
    },
  })

  // POST /admin/projects/manage {userId, projectId, role} (projectmanager.go). The membership is
  // PERSISTED before the cloud-access grant, which 501s on this deployment — treat 501 as applied.
  const addMember = useMutation({
    mutationFn: () =>
      apiFetch("/admin/projects/manage", {
        method: "POST",
        body: { userId: memberChoice, projectId: id, role: memberRole },
      }),
    onSuccess: () => {
      toast.success("Member added")
      setAddMemberOpen(false)
      setMemberChoice("")
      invalidateProject()
    },
    onError: (e: Error) => {
      if (e instanceof ApiError && e.status === 501) {
        toast.success("Member added (cloud access grant not available on this deployment)")
        setAddMemberOpen(false)
        setMemberChoice("")
        invalidateProject()
      } else {
        toast.error(e.message)
      }
    },
  })

  // POST /admin/projects/manage/remove {projectId, sub} — same persisted-then-501 semantics.
  const removeMember = useMutation({
    mutationFn: (sub: string) =>
      apiFetch("/admin/projects/manage/remove", { method: "POST", body: { projectId: id, sub } }),
    onSuccess: () => {
      toast.success("Member removed")
      setMemberToRemove(null)
      invalidateProject()
    },
    onError: (e: Error) => {
      if (e instanceof ApiError && e.status === 501) {
        toast.success("Member removed (cloud access revoke not available on this deployment)")
        setMemberToRemove(null)
        invalidateProject()
      } else {
        toast.error(e.message)
      }
    },
  })

  // PUT /admin/project/{id}/public-networks — null resets to the default (all external networks),
  // an array restricts the project to that allow-list.
  const savePublicNets = useMutation({
    mutationFn: () =>
      apiFetch(`${projectPath}/public-networks`, {
        method: "PUT",
        body: {
          publicNetworkIds: allPublicNets ? null : publicNetChoice,
          publicNetworksVisible: publicNetsVisible,
        },
      }),
    onSuccess: () => {
      toast.success("Public networks updated")
      invalidateProject()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const memberRoleOf = (sub?: string) =>
    project?.memberships?.find((m) => m.sub === sub)?.role ?? "MEMBER"

  const countEntries = Object.entries(counts.data ?? {}).filter(([k, v]) => k !== "TOTAL" && (v ?? 0) > 0)

  return (
    <>
      <PageHeader
        title={project?.name ?? (isLoading ? "Loading…" : "Project")}
        eyebrow="Clients"
        description="Client project detail."
        breadcrumb={
          <Breadcrumb>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink asChild>
                  <Link to="/clients/projects">Projects</Link>
                </BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{project?.name ?? id}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => syncProject.mutate()} disabled={syncProject.isPending}>
              <RefreshCw className={syncProject.isPending ? "size-4 animate-spin" : "size-4"} /> Sync
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={!project || !["ENABLED", "DISABLED"].includes((project.status ?? "").toUpperCase())}
              onClick={() => setStatusConfirm(enabled ? "DISABLED" : "ENABLED")}
            >
              {enabled ? <Pause className="size-4" /> : <Play className="size-4" />}
              {enabled ? "Disable" : "Enable"}
            </Button>
            {/* Held only by billing: a resume would lift it, unless an operator pins it. */}
            {(project?.status ?? "").toUpperCase() === "DISABLED" && !project?.disabledByOperator && (
              <Button variant="outline" size="sm" onClick={() => setStatusConfirm("DISABLED")}>
                <Pause className="size-4" /> Keep disabled
              </Button>
            )}
            <Button variant="destructive" size="sm" disabled={!project} onClick={() => setDeleteOpen(true)}>
              <Trash2 className="size-4" /> Delete project
            </Button>
          </>
        }
      />

      {isLoading ? (
        <Skeleton className="h-64" />
      ) : error ? (
        <ErrorPanel error={error} />
      ) : (
        <Tabs value={tab} onValueChange={setTab}>
          {/* Labels carry live counts — on a narrow viewport the four triggers
              overflow, so the wrapper scrolls horizontally instead of forcing
              document-level scroll (WCAG reflow). */}
          <div className="-mx-1 overflow-x-auto px-1 pb-1">
            <TabsList className="w-max">
              <TabsTrigger value="overview">Overview</TabsTrigger>
              <TabsTrigger value="members">
                Members{members.data?.data ? ` (${members.data.data.length})` : ""}
              </TabsTrigger>
              <TabsTrigger value="resources">
                Cloud resources{counts.data?.TOTAL != null ? ` (${counts.data.TOTAL})` : ""}
              </TabsTrigger>
              <TabsTrigger value="quota">Quota</TabsTrigger>
            </TabsList>
          </div>

          <TabsContent value="overview" className="mt-4 space-y-6">
            <Card>
              <CardHeader>
                <CardTitle className="text-base">Project</CardTitle>
              </CardHeader>
              <CardContent className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                <Field label="Name" value={project?.name} />
                <div>
                  <p className="text-eyebrow mb-1">Status</p>
                  <StatusBadge status={project?.status} />
                </div>
                <Field label="ID" value={project?.id} mono />
                <div>
                  <p className="text-eyebrow mb-1">Organization</p>
                  <div className="flex items-center gap-2 text-sm">
                    {project?.organizationId ? (
                      <Link
                        to={`/clients/organizations/${project.organizationId}`}
                        className="font-mono text-xs underline-offset-2 hover:underline"
                      >
                        {project.organizationId}
                      </Link>
                    ) : (
                      <span className="text-muted-foreground">None (imported / unassigned)</span>
                    )}
                    <Button variant="outline" size="sm" onClick={() => setOrgOpen(true)}>
                      <Building2 className="size-4" /> {project?.organizationId ? "Change" : "Assign"}
                    </Button>
                  </div>
                </div>
                <div>
                  <p className="text-eyebrow mb-1">Billing profile</p>
                  <div className="flex items-center gap-2 text-sm">
                    {project?.billingProfileId ? (
                      <Link
                        to={`/clients/billing-profiles/${project.billingProfileId}`}
                        className="font-mono text-xs underline-offset-2 hover:underline"
                      >
                        {project.billingProfileId}
                      </Link>
                    ) : (
                      <span className="text-muted-foreground">Inherited from organization</span>
                    )}
                    <Button variant="outline" size="sm" onClick={() => setBpOpen(true)}>
                      <CreditCard className="size-4" /> Change
                    </Button>
                  </div>
                </div>
                <Field label="Created" value={fmtDateTime(project?.createdAt)} />
              </CardContent>
            </Card>

            <Card>
              <CardHeader className="flex flex-row items-center justify-between">
                <CardTitle className="text-base">Cloud services</CardTitle>
                <Button variant="outline" size="sm" onClick={() => setAttachOpen(true)}>
                  <Plus className="size-4" /> Attach provider
                </Button>
              </CardHeader>
              <CardContent>
                {Object.entries(project?.provisionFailures ?? {}).map(([id, f]) => (
                  <p key={id} role="alert" className="mb-2 text-sm text-destructive">
                    Not provisioned on <span className="font-mono text-xs">{id}</span>: {f.message}
                  </p>
                ))}
                {(project?.services ?? []).length === 0 ? (
                  <p className="text-sm text-muted-foreground">No external service attached.</p>
                ) : (
                  <div className="space-y-2">
                    {(project?.services ?? []).map((s, i) => (
                      <div key={i} className="flex flex-wrap items-center gap-x-6 gap-y-1 text-sm">
                        <span>
                          <span className="text-xs text-muted-foreground">Service: </span>
                          <span className="font-mono text-xs">{s.serviceId ?? "—"}</span>
                        </span>
                        {s.rgwUid ? (
                          <span>
                            <span className="text-xs text-muted-foreground">RGW user: </span>
                            <span className="font-mono text-xs">{s.rgwUid}</span>
                          </span>
                        ) : s.pool ? (
                          <span>
                            <span className="text-xs text-muted-foreground">PVE pool: </span>
                            <span className="font-mono text-xs">{s.pool}</span>
                          </span>
                        ) : null}
                      </div>
                    ))}
                  </div>
                )}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="text-base">Public networks</CardTitle>
              </CardHeader>
              <CardContent>
                {!externalServiceId ? (
                  <p className="text-sm text-muted-foreground">No cloud service attached.</p>
                ) : (
                  <div className="space-y-4">
                    <div className="flex items-center gap-2">
                      <Switch
                        id="public-nets-visible"
                        checked={publicNetsVisible}
                        onCheckedChange={setPublicNetsVisible}
                      />
                      <label htmlFor="public-nets-visible" className="text-sm">
                        Let users pick the external network
                        <span className="ml-1 text-xs text-muted-foreground">
                          (off = the server auto-assigns it)
                        </span>
                      </label>
                    </div>
                    <div className="flex items-center gap-2">
                      <Switch id="all-public-networks" checked={allPublicNets} onCheckedChange={setAllPublicNets} />
                      <label htmlFor="all-public-networks" className="text-sm">
                        All public networks (default)
                      </label>
                    </div>
                    {!allPublicNets &&
                      (publicNets.isLoading ? (
                        <Skeleton className="h-10" />
                      ) : publicNets.error ? (
                        <p className="text-sm text-muted-foreground">{(publicNets.error as Error).message}</p>
                      ) : (publicNets.data?.data ?? []).length === 0 ? (
                        <p className="text-sm text-muted-foreground">No public networks available.</p>
                      ) : (
                        <div className="space-y-2">
                          {(publicNets.data?.data ?? []).map((n) => {
                            const nid = n.id
                            if (!nid) return null
                            return (
                              <label key={nid} className="flex items-center gap-2 text-sm">
                                <Checkbox
                                  checked={publicNetChoice.includes(nid)}
                                  onCheckedChange={(on) =>
                                    setPublicNetChoice((prev) =>
                                      on === true ? [...prev, nid] : prev.filter((x) => x !== nid),
                                    )
                                  }
                                />
                                <span>{n.name || nid}</span>
                                <span className="font-mono text-xs text-muted-foreground">{nid}</span>
                              </label>
                            )
                          })}
                        </div>
                      ))}
                    <Button size="sm" disabled={savePublicNets.isPending} onClick={() => savePublicNets.mutate()}>
                      {savePublicNets.isPending ? "Saving…" : "Save"}
                    </Button>
                  </div>
                )}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="text-base">Resource counts</CardTitle>
              </CardHeader>
              <CardContent>
                {counts.isLoading ? (
                  <Skeleton className="h-10" />
                ) : counts.error ? (
                  <p className="text-sm text-muted-foreground">{(counts.error as Error).message}</p>
                ) : (
                  <div className="flex flex-wrap gap-3">
                    <div className="rounded-md border px-3 py-1.5 text-sm">
                      <span className="text-xs text-muted-foreground">TOTAL </span>
                      <span className="font-medium tabular-nums">{counts.data?.TOTAL ?? 0}</span>
                    </div>
                    {countEntries.map(([type, n]) => (
                      <div key={type} className="rounded-md border px-3 py-1.5 text-sm">
                        <span className="font-mono text-xs text-muted-foreground">{type} </span>
                        <span className="font-medium tabular-nums">{n}</span>
                      </div>
                    ))}
                  </div>
                )}
              </CardContent>
            </Card>
          </TabsContent>

          <TabsContent value="members" className="mt-4">
            <div className="mb-3 flex justify-end">
              <Button size="sm" onClick={() => setAddMemberOpen(true)}>
                <Plus className="size-4" /> Add member
              </Button>
            </div>
            {members.isLoading ? (
              <Skeleton className="h-32" />
            ) : members.error ? (
              <ErrorPanel error={members.error} />
            ) : (members.data?.data ?? []).length === 0 ? (
              <EmptyState icon={Users} title="No members" hint="Add a user to this project." />
            ) : (
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Member</TableHead>
                      <TableHead>Sub</TableHead>
                      <TableHead>Role</TableHead>
                      <TableHead className="w-10" />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(members.data?.data ?? []).map((m, i) => {
                      const role = memberRoleOf(m.sub)
                      return (
                        <TableRow key={m.sub ?? i}>
                          <TableCell>
                            <span className="font-medium">
                              {[m.firstName, m.lastName].filter(Boolean).join(" ") || m.email || "—"}
                            </span>
                            {m.email && (m.firstName || m.lastName) ? (
                              <span className="ml-2 text-xs text-muted-foreground">{m.email}</span>
                            ) : null}
                          </TableCell>
                          <TableCell className="font-mono text-xs text-muted-foreground">{m.sub ?? "—"}</TableCell>
                          <TableCell className="capitalize">{role.toLowerCase()}</TableCell>
                          <TableCell>
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              aria-label={`Remove ${m.email ?? m.sub ?? "member"}`}
                              disabled={role === "OWNER"}
                              onClick={() => setMemberToRemove(m)}
                            >
                              <Trash2 className="size-4 text-muted-foreground" />
                            </Button>
                          </TableCell>
                        </TableRow>
                      )
                    })}
                  </TableBody>
                </Table>
              </Card>
            )}
          </TabsContent>

          <TabsContent value="resources" className="mt-4">
            {resources.isLoading ? (
              <Skeleton className="h-32" />
            ) : resources.error ? (
              <ErrorPanel error={resources.error} />
            ) : (resources.data?.data ?? []).length === 0 ? (
              <EmptyState icon={Server} title="No cloud resources" hint="This project has no cached cloud resources." />
            ) : (
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Type</TableHead>
                      <TableHead>Name</TableHead>
                      <TableHead>Region</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>External ID</TableHead>
                      <TableHead>Created</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(resources.data?.data ?? []).map((cr) => (
                      <TableRow
                        key={cr.id ?? cr.externalId}
                        className={cr.id ? "cursor-pointer" : undefined}
                        onClick={() => cr.id && navigate(`/clients/cloud-resources/${cr.id}`)}
                      >
                        <TableCell className="font-mono text-xs">{cr.type ?? "—"}</TableCell>
                        <TableCell>
                          {cr.id ? (
                            <Link
                              to={`/clients/cloud-resources/${cr.id}`}
                              className="inline-block py-1 font-medium hover:underline"
                              onClick={(e) => e.stopPropagation()}
                            >
                              {dataField(cr, "name") ?? cr.externalId ?? "—"}
                            </Link>
                          ) : (
                            <span className="font-medium">{dataField(cr, "name") ?? "—"}</span>
                          )}
                        </TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">{cr.region ?? "—"}</TableCell>
                        <TableCell>
                          <StatusBadge status={dataField(cr, "status")} />
                        </TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">
                          {cr.externalId ?? "—"}
                        </TableCell>
                        <TableCell className="text-muted-foreground">{fmtDate(cr.createdAt)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </Card>
            )}
          </TabsContent>

          <TabsContent value="quota" className="mt-4 space-y-4">
            <Card>
              <CardHeader>
                <h2 className="text-base font-semibold">Resource quota</h2>
                <p className="mt-1 text-sm text-muted-foreground">
                  Enforced when servers, volumes, networks and public IPs are created or resized on Proxmox. Blank =
                  unlimited. A project with no quota at all uses its provider&apos;s default quota. The sandbox keys are
                  checked on every sandbox create against what is running; blank there means the sandbox
                  provider&apos;s default.
                </p>
              </CardHeader>
              <CardContent className="space-y-4">
                <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
                  {RESOURCE_QUOTA_KEYS.map(({ key, label, blank }) => {
                    const raw = resourceQuota[key] ?? ""
                    const invalid = !!raw.trim() && parseGPUQuotaLimit(raw) === undefined
                    return (
                      <div key={key} className="grid gap-1">
                        <Label className="text-xs" htmlFor={`rq-${key}`}>
                          {label}
                        </Label>
                        <Input
                          id={`rq-${key}`}
                          type="number"
                          min={0}
                          step={1}
                          placeholder={blank ?? "Unlimited"}
                          aria-invalid={invalid}
                          aria-describedby={invalid ? `rq-${key}-error` : undefined}
                          value={raw}
                          onChange={(e) => setResourceQuota((q) => ({ ...q, [key]: e.target.value }))}
                        />
                        {invalid ? (
                          <p id={`rq-${key}-error`} className="text-xs text-destructive">
                            Whole number required.
                          </p>
                        ) : null}
                      </div>
                    )
                  })}
                </div>
                <Button
                  onClick={() => saveQuota.mutate()}
                  disabled={saveQuota.isPending || !quotaDraftValid || !resourceQuotaValid}
                >
                  {saveQuota.isPending ? "Saving…" : "Save resource quota"}
                </Button>
              </CardContent>
            </Card>
            <Card>
              <CardHeader className="flex flex-row items-start justify-between gap-3">
                <div>
                  <h2 className="text-base font-semibold">GPU quota</h2>
                  <p className="mt-1 text-sm text-muted-foreground">Project-wide usage snapshot and limits.</p>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => gpuUsage.refetch()}
                  disabled={gpuUsage.isFetching}
                >
                  <RefreshCw className={gpuUsage.isFetching ? "size-4 animate-spin" : "size-4"} />
                  Refresh usage
                </Button>
              </CardHeader>
              <CardContent className="space-y-4">
                <div className="flex items-start justify-between gap-4 rounded-lg border p-3">
                  <div>
                    <p className="text-sm font-medium">Show GPU capacity to this project</p>
                    <p className="mt-0.5 text-xs text-muted-foreground">
                      When on, the client dashboard shows the region's available / total GPUs per model. Off by default.
                    </p>
                  </div>
                  <Switch
                    aria-label="Show GPU capacity to this project"
                    checked={gpuCapVisible}
                    disabled={saveGpuCapVisible.isPending}
                    onCheckedChange={(v) => {
                      const visible = v === true
                      setGpuCapVisible(visible)
                      saveGpuCapVisible.mutate(visible)
                    }}
                  />
                </div>
                <p className="text-sm text-muted-foreground">
                  Per-model GPU device limits, enforced when servers are created or resized through
                  ProxCloud. No entry = unlimited; "*" applies to any model without its own row.
                </p>

                <div className="grid gap-3 sm:grid-cols-3">
                  <div className="rounded-xl border bg-muted/20 p-4">
                    <div className="flex items-center gap-2 text-sm text-muted-foreground">
                      <CircuitBoard className="size-4" aria-hidden="true" /> GPU devices in use
                    </div>
                    <p className="mt-2 font-mono text-2xl font-semibold tabular-nums">
                      <span data-testid="gpu-used-total">
                        {gpuUsage.isLoading ? "…" : gpuUsageAvailable ? totalGPUUsed : "—"}
                      </span>
                    </p>
                  </div>
                  <div className="rounded-xl border bg-muted/20 p-4">
                    <p className="text-sm text-muted-foreground">Models in use</p>
                    <p className="mt-2 font-mono text-2xl font-semibold tabular-nums">
                      {gpuUsage.isLoading
                        ? "…"
                        : gpuUsageAvailable
                          ? Object.values(normalizedGPUUsage).filter((used) => used > 0).length
                          : "—"}
                    </p>
                  </div>
                  <div className="rounded-xl border bg-muted/20 p-4">
                    <p className="text-sm text-muted-foreground">Configured limits</p>
                    <p className="mt-2 font-mono text-2xl font-semibold tabular-nums">{quotaRows.length}</p>
                  </div>
                </div>

                {gpuUsage.error ? (
                  <p className="rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive" role="alert">
                    Could not load GPU usage: {gpuUsage.error instanceof Error ? gpuUsage.error.message : "Unknown error"}
                  </p>
                ) : !gpuUsage.isLoading && gpuUsage.data && !gpuUsageAvailable ? (
                  <p className="rounded-lg border bg-muted/40 p-3 text-sm text-muted-foreground" role="status">
                    GPU usage is currently unavailable. Try syncing the project; configured limits remain in place.
                  </p>
                ) : null}

                <div className="space-y-2">
                  <h3 className="text-sm font-medium">Usage by model</h3>
                  {gpuUsage.isLoading ? (
                    <Skeleton className="h-24 w-full" />
                  ) : gpuStatusRows.length === 0 ? (
                    <p className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">
                      No GPU usage or limits for this project yet.
                    </p>
                  ) : (
                    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
                      {gpuStatusRows.map((row) => {
                        const used = gpuUsageAvailable && row.model !== "*" ? row.used : undefined
                        const remaining = row.limit === undefined || used === undefined
                          ? undefined
                          : Math.max(row.limit - used, 0)
                        const exhausted = row.limit !== undefined && used !== undefined && used >= row.limit
                        return (
                          <div
                            key={row.model}
                            data-testid={`gpu-status-${row.model}`}
                            className="min-w-0 rounded-xl border p-4"
                          >
                            <p className="break-words font-mono text-xs font-medium">
                              {row.model === "*" ? "Other GPU models (*)" : row.model}
                            </p>
                            <div className="mt-3 grid grid-cols-2 gap-3 text-sm">
                              <div>
                                <p className="text-xs text-muted-foreground">Used</p>
                                <p className="mt-1 font-mono font-semibold tabular-nums">
                                  {!gpuUsageAvailable ? "Unavailable" : used ?? "—"}
                                </p>
                              </div>
                              <div>
                                <p className="text-xs text-muted-foreground">
                                  {row.limit === undefined && row.capacity ? "Region free" : "Limit"}
                                </p>
                                <p className="mt-1 font-mono font-semibold tabular-nums">
                                  {row.invalid
                                    ? "Invalid draft"
                                    : row.limit ?? (row.capacity ? `${row.capacity.available} / ${row.capacity.total}` : "Unlimited")}
                                </p>
                              </div>
                            </div>
                            <p className={exhausted ? "mt-3 text-xs text-destructive" : "mt-3 text-xs text-muted-foreground"}>
                              {row.invalid
                                ? "Enter a non-negative whole number before saving."
                                : remaining === undefined
                                ? row.source
                                : exhausted
                                  ? `${used! - row.limit! > 0 ? `${used! - row.limit!} over quota` : "Quota exhausted"} · ${row.source}`
                                  : `${remaining} remaining · ${row.source}`}
                            </p>
                          </div>
                        )
                      })}
                    </div>
                  )}
                </div>

                <div className="border-t pt-4">
                  <h3 className="text-sm font-medium">Configure limits</h3>
                  <p className="mt-1 text-xs text-muted-foreground">
                    Use the GPU model alias from the provider&apos;s GPU mappings (e.g. <code>nvidia-4090</code>). Aliases are
                    saved in lowercase with dashes.
                  </p>
                </div>
                {quotaRows.length === 0 ? (
                  <p className="text-sm text-muted-foreground">No GPU limits configured.</p>
                ) : (
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>GPU model</TableHead>
                        <TableHead className="w-32">Limit</TableHead>
                        <TableHead />
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {quotaRows.map((row, i) => (
                        <TableRow key={row.model}>
                          <TableCell className="font-mono text-xs">{row.model}</TableCell>
                          <TableCell>
                            <Input
                              type="number"
                              min={0}
                              step={1}
                              className="w-24"
                              aria-label={`Limit for ${row.model}`}
                              aria-invalid={parseGPUQuotaLimit(row.limit) === undefined}
                              aria-describedby={parseGPUQuotaLimit(row.limit) === undefined ? `gpu-limit-error-${i}` : undefined}
                              value={row.limit}
                              onChange={(e) =>
                                setQuotaRows((rows) => rows.map((r, idx) => (idx === i ? { ...r, limit: e.target.value } : r)))
                              }
                            />
                            {parseGPUQuotaLimit(row.limit) === undefined ? (
                              <p id={`gpu-limit-error-${i}`} className="mt-1 text-xs text-destructive">
                                Whole number required.
                              </p>
                            ) : null}
                          </TableCell>
                          <TableCell className="text-right">
                            <Button
                              variant="ghost"
                              size="sm"
                              aria-label={`Remove limit for ${row.model}`}
                              onClick={() => setQuotaRows((rows) => rows.filter((_, idx) => idx !== i))}
                            >
                              Remove
                            </Button>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                )}
                <div className="flex flex-wrap items-end gap-3">
                  <div className="min-w-56 flex-1 space-y-1.5 sm:max-w-80">
                    <p className="text-eyebrow">GPU model</p>
                    <Input
                      list="project-gpu-model-options"
                      aria-label="GPU model"
                      placeholder="e.g. nvidia-a100-80gb or *"
                      value={quotaModel}
                      onChange={(e) => setQuotaModel(e.target.value)}
                      onBlur={() => setQuotaModel((model) => normalizeGPUModel(model))}
                    />
                    <datalist id="project-gpu-model-options">
                      {gpuModelOptions.map((model) => <option key={model} value={model} />)}
                    </datalist>
                  </div>
                  <div className="space-y-1.5">
                    <p className="text-eyebrow">Limit</p>
                    <Input
                      type="number"
                      min={0}
                      step={1}
                      className="w-24"
                      aria-label="Limit"
                      value={quotaLimit}
                      onChange={(e) => setQuotaLimit(e.target.value)}
                    />
                  </div>
                  <Button
                    variant="outline"
                    disabled={!canAddQuota}
                    onClick={() => {
                      if (!canAddQuota || newQuotaLimit === undefined) return
                      setQuotaRows((rows) => [...rows, { model: normalizedQuotaModel, limit: String(newQuotaLimit) }])
                      setQuotaModel("")
                      setQuotaLimit("")
                    }}
                  >
                    Add limit
                  </Button>
                  <Button
                    onClick={() => saveQuota.mutate()}
                    disabled={saveQuota.isPending || !quotaDraftValid || !resourceQuotaValid}
                  >
                    {saveQuota.isPending ? "Saving…" : "Save quota"}
                  </Button>
                </div>
                {duplicateQuotaModel && normalizedQuotaModel ? (
                  <p className="text-xs text-destructive" role="alert">This GPU model already has a limit.</p>
                ) : quotaLimit.trim() && newQuotaLimit === undefined ? (
                  <p className="text-xs text-destructive" role="alert">Limit must be a non-negative whole number.</p>
                ) : null}
              </CardContent>
            </Card>
          </TabsContent>
        </Tabs>
      )}

      {/* Status confirm */}
      <Dialog open={!!statusConfirm} onOpenChange={(o) => !o && setStatusConfirm(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{statusConfirm === "DISABLED" ? "Disable project" : "Enable project"}</DialogTitle>
            <DialogDescription>
              {statusConfirm === "DISABLED"
                ? enabled
                  ? "Pauses every server in the project before disabling it."
                  : "Keeps the project disabled when its billing profile resumes or activates. Only an operator can then enable it."
                : "Unpauses the project's servers and re-enables it."}{" "}
              Project: <span className="font-medium">{project?.name}</span>
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setStatusConfirm(null)}>
              Cancel
            </Button>
            <Button
              variant={statusConfirm === "DISABLED" ? "destructive" : "default"}
              disabled={updateStatus.isPending}
              onClick={() => statusConfirm && updateStatus.mutate(statusConfirm)}
            >
              {updateStatus.isPending ? "Working…" : statusConfirm === "DISABLED" ? "Disable" : "Enable"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete confirm */}
      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete project</DialogTitle>
            <DialogDescription>
              Deletes {project?.name ?? "this project"} with its cloud resources. Schedule keeps it for the grace
              period, during which the deletion can be cancelled; Delete now hands it to the deletion job at once. This
              cannot be undone once the deletion runs.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteOpen(false)}>
              Cancel
            </Button>
            <Button variant="outline" disabled={deleteProject.isPending} onClick={() => deleteProject.mutate(false)}>
              Schedule deletion
            </Button>
            <Button variant="destructive" disabled={deleteProject.isPending} onClick={() => setConfirmNow(true)}>
              Delete now…
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete now: one more confirm, since nothing can stop it once sent */}
      <Dialog open={confirmNow} onOpenChange={setConfirmNow}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {project?.name ?? "this project"} now?</DialogTitle>
            <DialogDescription>
              The deletion job tears down every server, volume, network and other cloud resource of this project within
              about a minute. There is no grace period and the deletion cannot be cancelled.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmNow(false)}>
              Back
            </Button>
            <Button variant="destructive" disabled={deleteProject.isPending} onClick={() => deleteProject.mutate(true)}>
              {deleteProject.isPending ? "Deleting…" : "Delete now"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Change billing profile */}
      <Dialog open={bpOpen} onOpenChange={setBpOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Change billing profile</DialogTitle>
            <DialogDescription>
              Assigns a billing profile to this project. Charges accrue against the selected profile.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Select value={bpChoice} onValueChange={setBpChoice}>
              <SelectTrigger className="w-full" aria-label="Billing profile">
                <SelectValue placeholder={bps.isLoading ? "Loading billing profiles…" : "Pick a billing profile"} />
              </SelectTrigger>
              <SelectContent>
                {(bps.data?.data ?? []).map((bp) =>
                  bp.id ? (
                    <SelectItem key={bp.id} value={bp.id}>
                      {[bp.name || bp.email || bp.id, bp.currency].filter(Boolean).join(" · ")}
                    </SelectItem>
                  ) : null,
                )}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setBpOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!bpChoice || changeBp.isPending} onClick={() => changeBp.mutate()}>
              {changeBp.isPending ? "Saving…" : "Assign billing profile"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={attachOpen}
        onOpenChange={(o) => {
          setAttachOpen(o)
          if (!o) setAttachChoice("")
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Attach cloud provider</DialogTitle>
            <DialogDescription>
              Provisions this project on the provider: a PVE pool for Proxmox, a dedicated RGW user for Ceph S3,
              a namespace for Managed Kubernetes / Databases. Projects created after a provider was added pick it up on their own — this is for the ones
              that already existed.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Select value={attachChoice} onValueChange={setAttachChoice}>
              <SelectTrigger className="w-full" aria-label="Cloud provider">
                <SelectValue placeholder={providers.isLoading ? "Loading providers…" : "Pick a provider"} />
              </SelectTrigger>
              <SelectContent>
                {(providers.data?.data ?? [])
                  .filter((p) => p.id && !(project?.services ?? []).some((s) => s.serviceId === p.id))
                  .map((p) => (
                    <SelectItem key={p.id} value={p.id}>
                      {p.config?.provider ? `${p.name ?? p.id} · ${p.config.provider}` : p.name ?? p.id}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAttachOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!attachChoice || attachService.isPending} onClick={() => attachService.mutate(attachChoice)}>
              {attachService.isPending ? "Attaching…" : "Attach provider"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={orgOpen} onOpenChange={setOrgOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Change organization</DialogTitle>
            <DialogDescription>
              Assigns this project to an organization. Useful for imported projects that arrived without one.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Select value={orgChoice} onValueChange={setOrgChoice}>
              <SelectTrigger className="w-full" aria-label="Organization">
                <SelectValue placeholder={orgsList.isLoading ? "Loading organizations…" : "Pick an organization"} />
              </SelectTrigger>
              <SelectContent>
                {(orgsList.data?.data ?? []).map((o) =>
                  o.id ? (
                    <SelectItem key={o.id} value={o.id}>
                      {o.name ? `${o.name} · ${o.id}` : o.id}
                    </SelectItem>
                  ) : null,
                )}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOrgOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!orgChoice || changeOrg.isPending} onClick={() => changeOrg.mutate()}>
              {changeOrg.isPending ? "Saving…" : "Assign organization"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Add member */}
      <Dialog open={addMemberOpen} onOpenChange={setAddMemberOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add member</DialogTitle>
            <DialogDescription>Adds an existing user to {project?.name ?? "this project"}.</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <Select value={memberChoice} onValueChange={setMemberChoice}>
              <SelectTrigger className="w-full" aria-label="User">
                <SelectValue placeholder={users.isLoading ? "Loading users…" : "Pick a user"} />
              </SelectTrigger>
              <SelectContent>
                {(users.data?.data ?? []).map((u) =>
                  u.id ? (
                    <SelectItem key={u.id} value={u.id}>
                      {u.email ?? [u.firstName, u.lastName].filter(Boolean).join(" ") ?? u.id}
                    </SelectItem>
                  ) : null,
                )}
              </SelectContent>
            </Select>
            <Select value={memberRole} onValueChange={setMemberRole}>
              <SelectTrigger className="w-full" aria-label="Role">
                <SelectValue placeholder="Role" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="MEMBER">Member</SelectItem>
                <SelectItem value="OWNER">Owner</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddMemberOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!memberChoice || addMember.isPending} onClick={() => addMember.mutate()}>
              {addMember.isPending ? "Adding…" : "Add member"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Remove member confirm */}
      <Dialog open={!!memberToRemove} onOpenChange={(o) => !o && setMemberToRemove(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Remove member</DialogTitle>
            <DialogDescription>
              Removes {memberToRemove?.email ?? memberToRemove?.sub ?? "this user"} from{" "}
              {project?.name ?? "the project"}.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setMemberToRemove(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={removeMember.isPending}
              onClick={() => memberToRemove?.sub && removeMember.mutate(memberToRemove.sub)}
            >
              {removeMember.isPending ? "Removing…" : "Remove member"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
