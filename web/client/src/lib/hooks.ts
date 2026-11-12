import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query"
import { useParams } from "react-router-dom"
import { ApiError, apiFetch, apiFetchEnvelope, type CloudScope } from "./api"
import type {
  BillingSummary, CloudResource, CostInfo, GpuCapacityUsage, Location, Project, ProjectQuotaUsage,
  ProjectService,
} from "./types"
import { isObjectProvider } from "./types"

export function useProjects() {
  return useQuery({
    queryKey: ["projects"],
    queryFn: () => apiFetch<Project[]>("/project"),
  })
}

export function useProject(pid?: string) {
  const projects = useProjects()
  return { ...projects, project: projects.data?.find((p) => p.id === pid) }
}

// Current project id from the /p/:pid/* route.
export function useProjectId(): string {
  const { pid } = useParams()
  return pid ?? ""
}

// Locations (one per attached cloud service × region) — the cloud-call scope
// (x-service-id / x-region-id headers) comes from here.
export function useLocations(pid: string) {
  return useQuery({
    queryKey: ["locations", pid],
    queryFn: () => apiFetch<Location[]>(`/project/${pid}/resource-types`),
    enabled: !!pid,
  })
}

export function useCloudScope(pid: string): CloudScope | undefined {
  const { data } = useLocations(pid)
  // Generic compute/network/storage pages need a compute (Proxmox) scope. A project can
  // also attach an object store (buckets only), Kamaji (managed Kubernetes control planes) or
  // DBaaS (managed databases), and any may appear first — skip them all, mirroring the
  // backend's resolveServiceID.
  const loc =
    data?.find((candidate) => !isObjectProvider(candidate.provider) && candidate.provider !== "kamaji" && candidate.provider !== "dbaas" && candidate.provider !== "sandbox") ?? data?.[0]
  if (!loc?.serviceId || !loc?.region) return undefined
  return { serviceId: loc.serviceId, region: loc.region }
}

// Live quota usage for the selected compute service/region. The response can
// be partial (with warnings), so callers should render each metric defensively.
export function useProjectQuota(pid: string, scope: CloudScope | undefined) {
  return useQuery({
    queryKey: ["project-quota", pid, scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<ProjectQuotaUsage>(`/project/${pid}/quota-usage`, { cloud: scope }),
    enabled: !!pid && !!scope,
    staleTime: 30_000,
  })
}

// Region GPU capacity (available/total per model) — only fetched when the project has it enabled.
export function useProjectGpuCapacity(pid: string, scope: CloudScope | undefined, enabled: boolean) {
  return useQuery({
    queryKey: ["project-gpu-capacity", pid, scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<GpuCapacityUsage>(`/project/${pid}/gpu-capacity`, { cloud: scope }),
    enabled: enabled && !!pid && !!scope,
    staleTime: 30_000,
  })
}

// Cached cloud-resource list by type (POST /project/{pid}/resource?type=X).
export function useCloudList(pid: string, type: string, extraQuery = "", refetchInterval: number | false = false) {
  const scope = useCloudScope(pid)
  return useQuery({
    queryKey: ["cloud", pid, type, extraQuery],
    queryFn: () =>
      apiFetch<CloudResource[]>(`/project/${pid}/resource?type=${type}${extraQuery}`, {
        method: "POST",
        cloud: scope,
      }),
    enabled: !!pid && !!scope,
    refetchInterval,
  })
}

// Keyset-paged cloud-resource list (POST /project/{pid}/resource?type=X) — the BE-paged
// twin of useCloudList for the big fleet lists (servers, volumes, …). "Load more" via
// paging.nextMarker. Use useCloudList (full) for secondary lookup lists a page joins against.
export function useCloudCursorList(pid: string, type: string, extraQuery = "") {
  const scope = useCloudScope(pid)
  return useCursorList<CloudResource>(
    ["cloud", pid, type, extraQuery],
    `/project/${pid}/resource?type=${type}${extraQuery}`,
    { method: "POST", cloud: scope, enabled: !!pid && !!scope },
  )
}

export function useCloudResource(pid: string, resourceId?: string) {
  const scope = useCloudScope(pid)
  return useQuery({
    queryKey: ["cloud-resource", pid, resourceId],
    queryFn: () => apiFetch<CloudResource>(`/project/${pid}/cloud/${resourceId}`, { cloud: scope }),
    enabled: !!pid && !!resourceId && !!scope,
  })
}

export function useCostInfo(pid: string) {
  return useQuery({
    queryKey: ["cost-info", pid],
    queryFn: () => apiFetch<CostInfo>(`/project/${pid}/cost-info`),
    enabled: !!pid,
  })
}

// Org-wide billing overview (profile aggregate + per-project breakdown) for the org billing
// dashboard. `bp` = the org's billing-profile id (from useBillingSummary().id).
export type OrgCostInfo = {
  billingProfileCostInfo?: CostInfo
  projects?: Record<string, CostInfo>
  currency?: string
}
export function useOrgCostInfo(bp?: string) {
  return useQuery({
    queryKey: ["org-cost-info", bp],
    queryFn: () => apiFetch<OrgCostInfo>(`/bill/${bp}/cost-info`),
    enabled: !!bp,
  })
}

export function useBillingSummary(pid: string) {
  return useQuery({
    queryKey: ["billing-summary", pid],
    queryFn: () => apiFetch<BillingSummary>(`/project/${pid}/billing`),
    enabled: !!pid,
  })
}

// Curated flavor categories (admin-configured "hardware" groups). The create-server flavor picker
// shows only flavors that belong to a category, grouped by it — uncategorized flavors are hidden.
export type FlavorCategory = {
  id: string
  name: string
  orderNumber?: number
  bareMetal?: boolean
  flavors?: Array<{ flavorName?: string }>
}
export function useFlavorCategories() {
  return useQuery({
    queryKey: ["flavor-categories"],
    queryFn: () => apiFetch<FlavorCategory[]>("/flavor-categories"),
  })
}

// Curated image groups + categories. Each enabled group offers named images under a category;
// the create-server image picker shows only offered images (matched to the location's templates by name).
export type ImageGroup = {
  id: string
  name: string
  categoryId?: string
  enabled?: boolean
  orderNumber?: number
  images?: Array<{ name?: string; version?: string; orderNumber?: number }>
}
export type ImageGrouping = {
  imageGroups?: ImageGroup[]
  imageCategories?: Array<{ id: string; name: string }>
}
export function useImageGroups() {
  return useQuery({
    queryKey: ["image-groups"],
    queryFn: () => apiFetch<ImageGrouping>("/groups/images"),
  })
}

export function useProjectServices(pid: string) {
  return useQuery({
    queryKey: ["project-services", pid],
    queryFn: () => apiFetch<ProjectService[]>(`/project/${pid}/service`),
    enabled: !!pid,
  })
}

// UI menu gating — GET /init/{pid} returns menu.items keyed by service
// slug ({enabled}). The ADMIN toggles these per region on the cloud
// provider's Services tab; a disabled/absent service hides its client section.
export type UIMenu = { items?: Record<string, { enabled?: boolean; newMenuItem?: boolean }> }

// Optional actions a provider's backend serves (/init features, keyed by service id; see
// cloudFeatures in internal/platform/project/clientcloud.go). An action whose flag is
// absent is hidden — the backend would reject it.
export type CloudFeature =
  | "serverSnapshots"
  | "volumeRetype"
  | "serverRebuild"
  | "serverRescue"
  | "serverPassword"
  | "userData"
  | "createDataVolumes"
  | "ports"
  | "subnets"
  | "serverEvents"
  | "consoleLog"
  | "volumeSnapshots"
  | "serverDiskSnapshots"
  | "serverBackups"
  | "imageImport"
  | "spreadGroups"
  | "publicIp"

export function useUIMenu(pid: string) {
  return useQuery({
    queryKey: ["ui-menu", pid],
    queryFn: () =>
      apiFetch<{ id: string; menu?: UIMenu; features?: Record<string, CloudFeature[]> }>(`/init/${pid}`),
    enabled: !!pid,
    staleTime: 60_000,
  })
}

// The compute scope's feature flags. Empty while loading, so an unsupported action never
// flashes into view.
export function useCloudFeatures(pid: string): Set<CloudFeature> {
  const scope = useCloudScope(pid)
  const { data } = useUIMenu(pid)
  return new Set(scope ? (data?.features?.[scope.serviceId] ?? []) : [])
}

// Platform feature flags — GET /features (billing, search, ...).
export function useFeatures() {
  return useQuery({
    queryKey: ["features"],
    queryFn: () => apiFetch<string[]>("/features"),
    staleTime: 300_000,
  })
}

// Keyset (cursor) list — the BE-paged read for churny/append-only lists (ledgers,
// bills, cloud resources). Walks server pages via paging.nextMarker and flattens
// them into `rows` for a "Load more" footer. `path` is WITHOUT paging params; the
// hook appends ?limit=&after=. Newest-first (the server default). See AuditPage for
// the rationale that keyset lists don't offer client sort/filter over partial data.
export function useCursorList<T = unknown>(
  key: readonly unknown[],
  path: string,
  opts?: { enabled?: boolean; limit?: number; method?: string; cloud?: CloudScope },
) {
  const limit = opts?.limit ?? 50
  const q = useInfiniteQuery({
    queryKey: [...key, "cursor", limit],
    queryFn: ({ pageParam }) => {
      const sep = path.includes("?") ? "&" : "?"
      const url = `${path}${sep}limit=${limit}${pageParam ? `&after=${encodeURIComponent(pageParam as string)}` : ""}`
      return apiFetchEnvelope<T[]>(url, { method: opts?.method, cloud: opts?.cloud })
    },
    initialPageParam: "",
    getNextPageParam: (last) => (last.paging as { nextMarker?: string } | undefined)?.nextMarker ?? undefined,
    enabled: opts?.enabled ?? true,
  })
  const rows = q.data?.pages.flatMap((p) => p.data ?? []) ?? []
  return { ...q, rows }
}

// Why POST /project/{id}/init left a project down (internal/platform/project/status.go Reason*).
export type ProjectInitReason =
  | "disabled_by_operator"
  | "billing_profile_inactive"
  | "billing_profile_suspended"
  | "billing_profile_missing"
  | "scheduled_for_deletion"
  | "deletion_in_progress"
  | "deleted"
  | "not_enabled"

export type ProjectInitResult =
  | { ok: true }
  | { ok: false; status: string; reason: ProjectInitReason; billingProfileStatus?: string; message: string }

// The 409 /init answers for a project it could not bring up: errors.details {status, reason[, billingProfileStatus]}.
export function projectInitRefusal(e: unknown): Extract<ProjectInitResult, { ok: false }> | undefined {
  if (!(e instanceof ApiError) || e.status !== 409) return undefined
  const d = e.details as { status?: unknown; reason?: unknown; billingProfileStatus?: unknown } | undefined
  if (!d || typeof d.reason !== "string" || typeof d.status !== "string") return undefined
  return {
    ok: false,
    status: d.status,
    reason: d.reason as ProjectInitReason,
    billingProfileStatus: typeof d.billingProfileStatus === "string" ? d.billingProfileStatus : undefined,
    message: e.message,
  }
}

// Ensures the project is bootstrapped (project pool) on entry. A new project gets its services
// attached by this very call, and the pages asked for its locations in parallel and got none
// ("No region" until a reload): ask again once it is done. A project the call could not bring up
// (billing profile not active or suspended, an operator's disable, a deletion) resolves to
// { ok: false, reason } so the shell can say why instead of carrying on as if it worked; it is
// asked again when the window regains focus (e.g. after adding billing details in another tab).
export function useProjectInit(pid: string) {
  const qc = useQueryClient()
  return useQuery<ProjectInitResult>({
    queryKey: ["project-init", pid],
    queryFn: async () => {
      try {
        await apiFetchEnvelope(`/project/${pid}/init`, { method: "POST" })
      } catch (e) {
        const refusal = projectInitRefusal(e)
        if (!refusal) throw e
        return refusal
      }
      void qc.invalidateQueries({ queryKey: ["locations", pid] })
      void qc.invalidateQueries({ queryKey: ["ui-menu", pid] })
      void qc.invalidateQueries({ queryKey: ["projects"] })
      return { ok: true }
    },
    enabled: !!pid,
    staleTime: (q) => (q.state.data?.ok === false ? 0 : Infinity),
    refetchOnWindowFocus: (q) => q.state.data?.ok === false,
    retry: 1,
  })
}
