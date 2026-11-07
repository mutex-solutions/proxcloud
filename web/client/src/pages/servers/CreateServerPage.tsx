// Create-server wizard: Location → Image → Flavor → Storage → Network → Access → Name.
// Body: name / imageId (project template) / flavorId / networkInterfaces:[{uuid, fixedIp?}] /
// keyName / adminPass / userData / securityGroupNames / rootVolume:{sizeGiB,type} / dataVolumes:[{sizeGiB,type}].
import { useMemo, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Check, CircleAlert, HardDrive, Plus, Server, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { StatusBadge } from "@/components/status-badge"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch, type CloudScope } from "@/lib/api"
import {
  useCloudList, useFlavorCategories, useImageGroups, useLocations, useProject, useProjectGpuCapacity, useProjectId,
  useProjectQuota, useUIMenu,
} from "@/lib/hooks"
import type { FlavorCategory, ImageGrouping } from "@/lib/hooks"
import { gpuCapacityViolations, gpuFromFlavor, serverQuotaViolations, volumeBatchQuotaViolations } from "@/lib/quota"
import type { CloudResource, Location } from "@/lib/types"
import { isObjectProvider } from "@/lib/types"
import { cn } from "@/lib/utils"
import { isPrivateNetwork } from "../network/NetworksPage"

type Section<T> = { label: string; items: T[] }

// buildFlavorSections groups live flavors by the admin's flavor categories (category order), showing
// only categorized flavors. If no category matches any live flavor (none configured / all drifted),
// it falls back to one unlabeled section of every flavor so the picker is never empty.
function buildFlavorSections(live: Flavor[], cats: FlavorCategory[]): Section<Flavor>[] {
  const sections = [...cats]
    .sort((a, b) => (a.orderNumber ?? 0) - (b.orderNumber ?? 0))
    .map((c) => {
      const names = new Set((c.flavors ?? []).map((f) => f.flavorName).filter(Boolean))
      return { label: c.name, items: live.filter((f) => f.data?.name && names.has(f.data.name)) }
    })
    .filter((s) => s.items.length > 0)
  const curated = sections.reduce((n, s) => n + s.items.length, 0)
  return curated > 0 ? sections : [{ label: "", items: live }]
}

// buildImageSections groups the location's templates by the enabled image groups (matched by name),
// labeled "Category · Group". Same never-empty fallback as flavors.
function buildImageSections(live: TemplateImage[], grouping?: ImageGrouping): Section<TemplateImage>[] {
  const catName = new Map((grouping?.imageCategories ?? []).map((c) => [c.id, c.name]))
  const byName = new Map(live.map((im) => [String(im.name), im]))
  const seen = new Set<string>()
  const sections = [...(grouping?.imageGroups ?? [])]
    .filter((g) => g.enabled)
    .sort((a, b) => (a.orderNumber ?? 0) - (b.orderNumber ?? 0))
    .map((g) => {
      const items: TemplateImage[] = []
      for (const gi of g.images ?? []) {
        const im = byName.get(String(gi.name))
        if (im && !seen.has(String(im.id))) {
          seen.add(String(im.id))
          items.push(im)
        }
      }
      const cat = g.categoryId ? catName.get(g.categoryId) : ""
      return { label: cat ? `${cat} · ${g.name}` : g.name, items }
    })
    .filter((s) => s.items.length > 0)
  const curated = sections.reduce((n, s) => n + s.items.length, 0)
  return curated > 0 ? sections : [{ label: "", items: live }]
}

// Selectable option card: hairline border at rest, border-primary + ring when picked
// (no heavy fills — design-system keeps selection quiet).
function optionCardClass(selected: boolean): string {
  return cn(
    "rounded-lg border bg-card p-3 text-left transition-colors hover:border-primary/50",
    "focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring",
    selected && "border-primary ring-1 ring-primary",
  )
}

type TemplateImage = Record<string, any>
type Flavor = {
  externalId?: string
  data?: {
    id?: string
    name?: string
    vcpus?: number
    ram?: number
    disk?: number
    ephemeral?: number
    swap?: number
    extra_specs?: Record<string, unknown>
  }
}

type VolumeType = { id?: string; name: string; displayName?: string; extra_specs?: { bootDisk?: boolean } }
type DataVolumeDraft = { id: number; sizeGiB: number; type: string }

const gb = (bytes?: number) => (bytes ? (bytes / 1073741824).toFixed(2) : "0.00")

function imageMinimumDisk(image?: TemplateImage): number {
  if (!image) return 1
  const declared = Number(image.min_disk ?? image.minDisk ?? 0)
  const imageBytes = Math.max(Number(image.size ?? 0), Number(image.virtual_size ?? image.virtualSize ?? 0))
  const imageGiB = Number.isFinite(imageBytes) && imageBytes > 0 ? Math.ceil(imageBytes / 1073741824) : 0
  return Math.max(1, Number.isFinite(declared) ? declared : 0, imageGiB)
}

function rootDefaultDisk(flavor?: Flavor, image?: TemplateImage): number {
  const flavorDisk = Number(flavor?.data?.disk ?? 0)
  return Math.max(imageMinimumDisk(image), Number.isFinite(flavorDisk) ? flavorDisk : 0)
}

// Collection-level cloud action (POST /project/{pid}/cloud/action → {result}).
function useBulkAction<T>(pid: string, scope: CloudScope | undefined, action: string) {
  return useQuery({
    queryKey: ["bulk-action", pid, action, scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<{ result?: T[] }>(`/project/${pid}/cloud/action`, {
        method: "POST",
        body: { action },
        cloud: scope,
      }),
    enabled: !!pid && !!scope,
    select: (d) => d?.result ?? [],
  })
}

export default function CreateServerPage() {
  const pid = useProjectId()
  const navigate = useNavigate()
  const qc = useQueryClient()

  const locations = useLocations(pid)
  const [locKey, setLocKey] = useState<string>()
  const locs = (locations.data ?? []).filter(
    (location) =>
      location.serviceId &&
      location.region &&
      !isObjectProvider(location.provider) &&
      (!location.resourceTypes || location.resourceTypes.includes("SERVER")),
  )
  const locOf = (l: Location) => `${l.serviceId}/${l.region}`
  const selectedLoc = locs.find((l) => locOf(l) === locKey) ?? locs[0]
  const scope: CloudScope | undefined =
    selectedLoc?.serviceId && selectedLoc?.region
      ? { serviceId: selectedLoc.serviceId, region: selectedLoc.region }
      : undefined
  // The selected provider's optional create inputs (/init features). Without user-data the
  // password applies to the image's default account; without boot-time data volumes they are
  // added after create (Volumes page).
  const init = useUIMenu(pid)
  const features = new Set(scope ? (init.data?.features?.[scope.serviceId] ?? []) : [])
  const userDataSupported = features.has("userData")

  const images = useBulkAction<TemplateImage>(pid, scope, "PUBLIC_IMAGES")
  const flavors = useBulkAction<Flavor>(pid, scope, "LIST_FLAVORS")
  const volumeTypes = useBulkAction<VolumeType>(pid, scope, "LIST_VOLUME_TYPES")
  const projectQuota = useProjectQuota(pid, scope)
  const project = useProject(pid).project
  // Region GPU capacity — only served when the operator enabled it; feeds the region-availability
  // gate so a GPU flavor with no project limit can't be created into an exhausted region.
  const gpuCapacity = useProjectGpuCapacity(pid, scope, project?.gpuCapacityVisible === true)
  // Curated catalog: show only the flavors/images the admin grouped into categories (grouped by
  // category), not the raw live cloud lists. Falls back to showing everything if nothing matches.
  const flavorCats = useFlavorCategories()
  const imageGroups = useImageGroups()
  // Volume-backed servers cannot use flavors with local ephemeral/swap disk (the backend
  // rejects them at create) — hide them from the wizard only; the shared LIST_FLAVORS
  // list keeps them for the RESIZE picker of pre-existing image-backed servers.
  const bootableFlavors = useMemo(
    () => (flavors.data ?? []).filter((f) => !(f.data?.ephemeral ?? 0) && !(f.data?.swap ?? 0)),
    [flavors.data],
  )
  const flavorSections = useMemo(() => buildFlavorSections(bootableFlavors, flavorCats.data ?? []), [bootableFlavors, flavorCats.data])
  // The project's own images (made from a server) are cloned by their template id.
  const { data: ownImages } = useCloudList(pid, "IMAGE")
  const projectImages = useMemo<TemplateImage[]>(
    () =>
      (ownImages ?? [])
        .filter((r) => r.externalId)
        .map((r) => ({ ...(r.data?.image as Record<string, unknown>), id: r.externalId }) as TemplateImage),
    [ownImages],
  )
  const allImages = useMemo<TemplateImage[]>(() => [...(images.data ?? []), ...projectImages], [images.data, projectImages])
  // Curated groups only hold the catalog, so the project's images get a section of their own.
  const imageSections = useMemo(() => {
    const catalog = images.data?.length ? buildImageSections(images.data, imageGroups.data) : []
    return projectImages.length ? [...catalog, { label: "Your images", items: projectImages }] : catalog
  }, [images.data, imageGroups.data, projectImages])
  const networks = useQuery({
    queryKey: ["cloud", pid, "NETWORK", scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<CloudResource[]>(`/project/${pid}/resource?type=NETWORK`, { method: "POST", cloud: scope }),
    enabled: !!pid && !!scope,
  })
  const keypairs = useQuery({
    queryKey: ["cloud", pid, "KEYPAIR", scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<CloudResource[]>(`/project/${pid}/resource?type=KEYPAIR`, { method: "POST", cloud: scope }),
    enabled: !!pid && !!scope,
  })
  const secgroups = useQuery({
    queryKey: ["cloud", pid, "SECURITY_GROUP", scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<CloudResource[]>(`/project/${pid}/resource?type=SECURITY_GROUP`, { method: "POST", cloud: scope }),
    enabled: !!pid && !!scope,
  })
  // publicNetworksVisible=false → offer only own private networks (no shared/external infra).
  const netsVisible = project?.publicNetworksVisible === true
  const networkRows = netsVisible ? (networks.data ?? []) : (networks.data ?? []).filter(isPrivateNetwork)

  const [imageId, setImageId] = useState<string>()
  const [flavorId, setFlavorId] = useState<string>() // = flavor externalId
  const [rootSizeGiB, setRootSizeGiB] = useState(1)
  const [rootSizeTouched, setRootSizeTouched] = useState(false)
  const [rootVolumeType, setRootVolumeType] = useState("")
  const [dataVolumes, setDataVolumes] = useState<DataVolumeDraft[]>([])
  const [netIds, setNetIds] = useState<string[]>([])
  const [subnet, setSubnet] = useState("")
  const [spreadGroup, setSpreadGroup] = useState("")
  const [spreadStrict, setSpreadStrict] = useState(false)
  const [metadata, setMetadata] = useState<Record<string, string>>({})
  const [keyName, setKeyName] = useState("") // "" = no key pair
  const [loginMethod, setLoginMethod] = useState<"key" | "password">("key")
  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [userData, setUserData] = useState("")
  const [sgNames, setSgNames] = useState<string[]>([])
  const [name, setName] = useState("")

  // Admin-defined instance metadata options a tenant can fill in (reach the guest through the seed).
  const metadataOptions = useQuery({
    queryKey: ["instance-metadata-options", pid, scope?.serviceId, scope?.region],
    queryFn: () =>
      apiFetch<Array<{ key: string; displayName?: string; description?: string; userEditable?: boolean; options?: Array<{ value?: string; displayName?: string }> }>>(
        `/project/${pid}/instance-metadata-options`,
        { cloud: scope },
      ),
    enabled: !!pid && !!scope && features.has("userData"),
  })
  const editableMetadata = (metadataOptions.data ?? []).filter((o) => o.userEditable)
  const selectedImage = allImages.find((image) => String(image.id) === imageId)
  const selectedFlavor = bootableFlavors.find((flavor) => flavor.externalId === flavorId)
  const minimumRootSizeGiB = imageMinimumDisk(selectedImage)
  // A profile that refuses boot disks (HDD capacity storage) is not offered for the boot volume.
  const availableVolumeTypes = useMemo(
    () => (volumeTypes.data ?? []).filter((type) => type.extra_specs?.bootDisk !== false),
    [volumeTypes.data],
  )
  const soleVolumeType = availableVolumeTypes.length === 1 ? availableVolumeTypes[0] : undefined
  const availableVolumeTypeNames = useMemo(
    () => new Set(availableVolumeTypes.map((type) => type.name)),
    [availableVolumeTypes],
  )
  const resolveVolumeType = (type: string) =>
    soleVolumeType?.name || (availableVolumeTypeNames.has(type) ? type : "")
  const selectedRootVolumeType = resolveVolumeType(rootVolumeType)
  // A provider without boot-time data volumes never receives (or quota-checks) stale drafts.
  const resolvedDataVolumes = (features.has("createDataVolumes") ? dataVolumes : []).map((volume) => ({
    ...volume,
    type: resolveVolumeType(volume.type),
  }))
  const selectedGpu = gpuFromFlavor(selectedFlavor?.data?.extra_specs)
  const gpuCapacityVisible = project?.gpuCapacityVisible === true
  const computeQuotaViolations = serverQuotaViolations(projectQuota.data, selectedFlavor)
  const capacityViolations = gpuCapacityViolations(gpuCapacity.data, selectedFlavor)
  // Shown together under the flavor step — both are "this flavor won't fit" reasons.
  const flavorGateViolations = [...computeQuotaViolations, ...capacityViolations]
  const volumeRequests = selectedFlavor && selectedImage
    ? [
        { sizeGiB: rootSizeGiB, type: selectedRootVolumeType, label: "Root volume" },
        ...resolvedDataVolumes.map((volume, index) => ({
          sizeGiB: volume.sizeGiB,
          type: volume.type,
          label: `Data volume ${index + 1}`,
        })),
      ]
    : []
  const storageQuotaViolations = volumeBatchQuotaViolations(projectQuota.data, volumeRequests)
  const quotaViolations = [...flavorGateViolations, ...storageQuotaViolations]
  // A GPU flavor selected while region capacity is still loading must not slip past the gate.
  const capacityCheckPending = !!selectedGpu && gpuCapacityVisible && gpuCapacity.isLoading
  const computeQuotaCheckPending = (!!flavorId && projectQuota.isLoading) || capacityCheckPending
  const storageQuotaCheckPending = !!flavorId && (projectQuota.isLoading || volumeTypes.isLoading)
  const quotaCheckPending = computeQuotaCheckPending || storageQuotaCheckPending
  const quotaUnavailable =
    !!flavorId &&
    !projectQuota.isLoading &&
    (!!projectQuota.error ||
      !projectQuota.data?.compute ||
      (!!selectedGpu && projectQuota.data.gpu.usageAvailable === false))
  const storageQuotaUnavailable =
    !!flavorId && !projectQuota.isLoading && (!!projectQuota.error || !projectQuota.data?.storage)
  const rootSizeValid = Number.isInteger(rootSizeGiB) && rootSizeGiB >= minimumRootSizeGiB
  const dataVolumesValid = resolvedDataVolumes.every(
    (volume) =>
      Number.isInteger(volume.sizeGiB) &&
      volume.sizeGiB > 0 &&
      !!volume.type,
  )

  const keypairName = (r: CloudResource) => (r.data?.keypair?.name as string) ?? r.name ?? ""
  const sgName = (r: CloudResource) => (r.data?.securityGroup?.name as string) ?? r.name ?? ""

  const create = useMutation({
    mutationFn: async () => {
      // Refresh immediately before submit as well as checking on flavor change.
      // This narrows the race window; the cluster scheduler remains the final authority.
      const latestQuota = await projectQuota.refetch()
      const latestCapacity = gpuCapacityVisible ? (await gpuCapacity.refetch()).data : gpuCapacity.data
      const latestViolations = [
        ...serverQuotaViolations(latestQuota.data, selectedFlavor),
        ...volumeBatchQuotaViolations(latestQuota.data, volumeRequests),
        ...gpuCapacityViolations(latestCapacity, selectedFlavor),
      ]
      if (latestViolations.length > 0) {
        throw new Error(latestViolations.map((violation) => violation.message).join(" "))
      }
      // Password login with a username creates that user next to the image's default account. The API
      // hashes it, so the password itself never sits on the server's seed.
      const finalUserData = userDataSupported ? userData : ""
      return apiFetch<CloudResource>(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: {
          type: "SERVER",
          data: {
            name: name.trim(),
            imageId,
            flavorId,
            rootVolume: { sizeGiB: rootSizeGiB, type: selectedRootVolumeType },
            dataVolumes: resolvedDataVolumes.map((volume) => ({
              sizeGiB: volume.sizeGiB,
              type: volume.type,
            })),
            networkInterfaces: netIds.map((uuid) => ({ uuid, ...(subnet ? { subnet } : {}) })),
            ...(features.has("spreadGroups") && spreadGroup.trim()
              ? { spreadGroup: spreadGroup.trim(), spreadStrict }
              : {}),
            ...(Object.values(metadata).some(Boolean)
              ? { metadata: Object.fromEntries(Object.entries(metadata).filter(([, v]) => v)) }
              : {}),
            ...(loginMethod === "key" && keyName ? { keyName } : {}),
            ...(loginMethod === "password" && password
              ? { adminPass: password, ...(username.trim() ? { adminUser: username.trim() } : {}) }
              : {}),
            ...(finalUserData.trim() ? { userData: finalUserData } : {}),
            ...(sgNames.length ? { securityGroupNames: sgNames } : {}),
          },
        },
      })
    },
    onSuccess: () => {
      toast.success(`Server "${name.trim()}" is being created`)
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "SERVER"] })
      void qc.invalidateQueries({ queryKey: ["cloud", pid, "VOLUME"] })
      void qc.invalidateQueries({ queryKey: ["project-quota", pid] })
      navigate(`/p/${pid}/servers`)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const ready =
    !!scope && !!imageId && !!selectedFlavor && availableVolumeTypes.length > 0 &&
    !!selectedRootVolumeType && rootSizeValid && dataVolumesValid &&
    netIds.length > 0 && !!name.trim()

  // What still blocks Create — shown next to the disabled button so nobody
  // has to scroll back up hunting for the unfinished step.
  const missing = [
    !imageId && "an image",
    !selectedFlavor && "a flavor",
    availableVolumeTypes.length === 0 && "an enabled storage type",
    !selectedRootVolumeType && "a boot storage type",
    !rootSizeValid && `a root volume of at least ${minimumRootSizeGiB} GiB`,
    !dataVolumesValid && "valid data volume settings",
    netIds.length === 0 && "a network",
    !name.trim() && "a name",
  ].filter((x): x is string => !!x)

  if (locations.isLoading) {
    return (
      <>
        <PageHeader title="Create server" eyebrow="Compute" />
        <div className="grid gap-4">
          <Skeleton className="h-32" />
          <Skeleton className="h-32" />
          <Skeleton className="h-64" />
        </div>
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Create server"
        eyebrow="Compute"
        description="Pick a location, image, flavor and network, then name your server."
      />

      {locations.error ? (
        <p className="rounded-lg border border-destructive/40 bg-destructive/5 p-4 text-sm text-destructive">
          {(locations.error as Error).message}
        </p>
      ) : !locs.length ? (
        <p className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
          No locations are available in this project — attach a cloud service first.
        </p>
      ) : (
        <div className="grid gap-4">
          <Step n={1} title="Location">
            <div className="flex flex-wrap gap-2">
              {locs.map((l) => {
                const active = locOf(l) === (selectedLoc ? locOf(selectedLoc) : undefined)
                return (
                  <Button
                    key={locOf(l)}
                    type="button"
                    variant="outline"
                    size="sm"
                    aria-pressed={active}
                    className={cn(
                      active && "border-primary bg-transparent ring-1 ring-primary hover:bg-transparent",
                    )}
                    onClick={() => {
                      setLocKey(locOf(l))
                      setImageId(undefined)
                      setFlavorId(undefined)
                      setRootSizeGiB(1)
                      setRootSizeTouched(false)
                      setRootVolumeType("")
                      setDataVolumes([])
                      setNetIds([])
                      setKeyName("")
                      setSgNames([])
                    }}
                  >
                    {active ? <Check className="size-4 text-primary" /> : null}
                    {l.displayName || l.region} <span className="font-mono text-xs opacity-70">{l.region}</span>
                  </Button>
                )
              })}
            </div>
          </Step>

          <Step n={2} title="Image">
            {images.isLoading || imageGroups.isLoading ? (
              <Skeleton className="h-40" />
            ) : !allImages.length ? (
              <p className="text-sm text-muted-foreground">No images available.</p>
            ) : (
              <div className="grid max-h-96 gap-4 overflow-y-auto pr-1">
                {imageSections.map((sec) => (
                  <div key={sec.label || "__all__"} className="grid gap-2">
                    {sec.label ? <div className="text-eyebrow">{sec.label}</div> : null}
                    <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 xl:grid-cols-3">
                      {sec.items.map((im) => {
                        const selected = imageId === String(im.id)
                        return (
                          <button
                            key={String(im.id)}
                            type="button"
                            aria-pressed={selected}
                            className={optionCardClass(selected)}
                            onClick={() => {
                              setImageId(String(im.id))
                              if (selectedFlavor) {
                                setRootSizeGiB((current) =>
                                  rootSizeTouched
                                    ? Math.max(current, imageMinimumDisk(im))
                                    : rootDefaultDisk(selectedFlavor, im),
                                )
                              }
                            }}
                          >
                            <div className="flex items-start justify-between gap-2">
                              <span className="text-sm font-medium">{String(im.name ?? im.id)}</span>
                              {selected ? <Check className="size-4 shrink-0 text-primary" /> : null}
                            </div>
                            <div className="mt-1 text-xs text-muted-foreground">
                              {[im.os_distro, im.os_version].filter(Boolean).join(" ") || "—"}
                              {im.size ? ` · ${gb(im.size as number)} GB` : ""}
                            </div>
                            <div className="mt-2">
                              <StatusBadge status={im.status as string} className="text-xs" />
                            </div>
                          </button>
                        )
                      })}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </Step>

          <Step n={3} title="Flavor">
            {flavors.isLoading || flavorCats.isLoading ? (
              <Skeleton className="h-40" />
            ) : !flavors.data?.length ? (
              <p className="text-sm text-muted-foreground">No flavors available.</p>
            ) : (
              <div className="grid max-h-96 gap-4 overflow-y-auto pr-1">
                {flavorSections.map((sec) => (
                  <div key={sec.label || "__all__"} className="grid gap-2">
                    {sec.label ? <div className="text-eyebrow">{sec.label}</div> : null}
                    <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 xl:grid-cols-3">
                      {sec.items.map((f) => {
                        const selected = flavorId === f.externalId
                        const flavorQuotaViolations = [
                          ...serverQuotaViolations(projectQuota.data, f),
                          ...gpuCapacityViolations(gpuCapacity.data, f),
                        ]
                        return (
                          <button
                            key={f.externalId}
                            type="button"
                            aria-pressed={selected}
                            className={cn(
                              optionCardClass(selected),
                              flavorQuotaViolations.length > 0 && "border-destructive/50",
                            )}
                            onClick={() => {
                              setFlavorId(f.externalId)
                              setRootSizeGiB((current) =>
                                rootSizeTouched
                                  ? Math.max(current, imageMinimumDisk(selectedImage))
                                  : rootDefaultDisk(f, selectedImage),
                              )
                            }}
                          >
                            <div className="flex items-start justify-between gap-2">
                              <span className="text-sm font-medium">{f.data?.name ?? f.externalId}</span>
                              {selected ? <Check className="size-4 shrink-0 text-primary" /> : null}
                            </div>
                            <div className="mt-1 font-mono text-xs text-muted-foreground">
                              {f.data?.vcpus ?? "—"} vCPU · {f.data?.ram ? `${Math.round(f.data.ram / 1024)} GB` : "—"} RAM ·{" "}
                              {f.data?.disk != null ? `${f.data.disk} GB` : "—"} root volume
                            </div>
                            {flavorQuotaViolations.length > 0 ? (
                              <div className="mt-2 text-xs text-destructive">Exceeds project quota</div>
                            ) : null}
                          </button>
                        )
                      })}
                    </div>
                  </div>
                ))}
                {computeQuotaCheckPending ? (
                  <p className="text-sm text-muted-foreground">Checking this flavor against the current quota…</p>
                ) : flavorGateViolations.length > 0 ? (
                  <Alert variant="destructive">
                    <CircleAlert />
                    <AlertTitle>This flavor can't be created right now</AlertTitle>
                    <AlertDescription>
                      <ul className="list-disc space-y-1 pl-4">
                        {flavorGateViolations.map((violation) => (
                          <li key={`${violation.resource}-${violation.message}`}>{violation.message}</li>
                        ))}
                      </ul>
                    </AlertDescription>
                  </Alert>
                ) : quotaUnavailable ? (
                  <Alert>
                    <CircleAlert />
                    <AlertTitle>Live quota check is partly unavailable</AlertTitle>
                    <AlertDescription>
                      The API will still validate this request when you create the server.
                    </AlertDescription>
                  </Alert>
                ) : flavorId && projectQuota.data ? (
                  <p className="text-sm text-muted-foreground">This flavor fits the current quota snapshot.</p>
                ) : null}
              </div>
            )}
          </Step>

          <Step n={4} title="Storage">
            {volumeTypes.isLoading ? (
              <Skeleton className="h-32" />
            ) : volumeTypes.isError ? (
              <Alert variant="destructive">
                <CircleAlert />
                <AlertTitle>Storage types could not be loaded</AlertTitle>
                <AlertDescription className="flex flex-wrap items-center gap-3">
                  The block-storage catalog is temporarily unavailable.
                  <Button type="button" variant="outline" size="sm" onClick={() => void volumeTypes.refetch()}>
                    Try again
                  </Button>
                </AlertDescription>
              </Alert>
            ) : availableVolumeTypes.length === 0 ? (
              <Alert variant="destructive">
                <CircleAlert />
                <AlertTitle>No storage type is enabled</AlertTitle>
                <AlertDescription>
                  An administrator must enable at least one volume type for this region before a server can be created.
                </AlertDescription>
              </Alert>
            ) : (
              <div className="grid gap-5">
                <div className="grid gap-3 rounded-lg border p-4">
                  <div>
                    <div className="flex items-center gap-2 text-sm font-medium">
                      <HardDrive className="size-4" /> Boot volume
                    </div>
                    <p className="mt-1 text-xs text-muted-foreground">
                      The operating system boots from persistent block storage. It is deleted with the server.
                    </p>
                  </div>
                  <div className="grid gap-3 sm:grid-cols-2">
                    <div className="grid gap-2">
                      <Label htmlFor="root-volume-size">Size (GiB)</Label>
                      <Input
                        id="root-volume-size"
                        type="number"
                        min={minimumRootSizeGiB}
                        step={1}
                        value={rootSizeGiB}
                        disabled={!selectedFlavor || !selectedImage}
                        onChange={(event) => {
                          setRootSizeTouched(true)
                          setRootSizeGiB(Number(event.target.value))
                        }}
                      />
                      <p className={cn("text-xs text-muted-foreground", !rootSizeValid && "text-destructive")}>
                        Defaults from the flavor; minimum {minimumRootSizeGiB} GiB for the selected image.
                      </p>
                    </div>
                    <div className="grid gap-2">
                      <Label>Storage type</Label>
                      {soleVolumeType ? (
                        <div className="flex h-9 items-center gap-2 rounded-md border bg-muted/30 px-3 text-sm">
                          <HardDrive className="size-4 text-muted-foreground" />
                          <span>{soleVolumeType.displayName || soleVolumeType.name}</span>
                        </div>
                      ) : (
                        <Select value={selectedRootVolumeType} onValueChange={setRootVolumeType}>
                          <SelectTrigger className="w-full" aria-label="Boot volume storage type">
                            <SelectValue placeholder="Select a storage type" />
                          </SelectTrigger>
                          <SelectContent>
                            {availableVolumeTypes.map((type) => (
                              <SelectItem key={type.name} value={type.name}>
                                {type.displayName || type.name}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      )}
                    </div>
                  </div>
                </div>

                {features.has("createDataVolumes") && (
                  <div className="grid gap-3">
                    <div className="flex flex-wrap items-start justify-between gap-3">
                      <div>
                        <div className="text-sm font-medium">Additional data volumes</div>
                        <p className="mt-1 text-xs text-muted-foreground">
                          Optional blank volumes are attached at boot and preserved when the server is deleted.
                        </p>
                      </div>
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        disabled={dataVolumes.length >= 32}
                        onClick={() =>
                          setDataVolumes((volumes) => [
                            ...volumes,
                            {
                              id: Date.now() + volumes.length,
                              sizeGiB: 10,
                              type: soleVolumeType?.name || "",
                            },
                          ])
                        }
                      >
                        <Plus className="size-4" /> Add data volume
                      </Button>
                    </div>
                    {dataVolumes.map((volume, index) => (
                      <div
                        key={volume.id}
                        className="grid gap-3 rounded-lg border p-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto] sm:items-end"
                      >
                        <div className="grid gap-2">
                          <Label htmlFor={`data-volume-size-${volume.id}`}>Data volume {index + 1} size (GiB)</Label>
                          <Input
                            id={`data-volume-size-${volume.id}`}
                            type="number"
                            min={1}
                            step={1}
                            value={volume.sizeGiB}
                            onChange={(event) =>
                              setDataVolumes((volumes) =>
                                volumes.map((item) =>
                                  item.id === volume.id ? { ...item, sizeGiB: Number(event.target.value) } : item,
                                ),
                              )
                            }
                          />
                        </div>
                        <div className="grid gap-2">
                          <Label>Storage type</Label>
                          {soleVolumeType ? (
                            <div className="flex h-9 items-center gap-2 rounded-md border bg-muted/30 px-3 text-sm">
                              <HardDrive className="size-4 text-muted-foreground" />
                              <span>{soleVolumeType.displayName || soleVolumeType.name}</span>
                            </div>
                          ) : (
                            <Select
                              value={resolveVolumeType(volume.type)}
                              onValueChange={(value) =>
                                setDataVolumes((volumes) =>
                                  volumes.map((item) => (item.id === volume.id ? { ...item, type: value } : item)),
                                )
                              }
                            >
                              <SelectTrigger className="w-full" aria-label={`Data volume ${index + 1} storage type`}>
                                <SelectValue placeholder="Select a storage type" />
                              </SelectTrigger>
                              <SelectContent>
                                {availableVolumeTypes.map((type) => (
                                  <SelectItem key={type.name} value={type.name}>
                                    {type.displayName || type.name}
                                  </SelectItem>
                                ))}
                              </SelectContent>
                            </Select>
                          )}
                        </div>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon"
                          aria-label={`Remove data volume ${index + 1}`}
                          onClick={() => setDataVolumes((volumes) => volumes.filter((item) => item.id !== volume.id))}
                        >
                          <Trash2 className="size-4" />
                        </Button>
                      </div>
                    ))}
                  </div>
                )}

                {storageQuotaCheckPending ? (
                  <p className="text-sm text-muted-foreground">Checking the current storage quota…</p>
                ) : storageQuotaViolations.length > 0 ? (
                  <Alert variant="destructive">
                    <CircleAlert />
                    <AlertTitle>This storage selection exceeds the project quota</AlertTitle>
                    <AlertDescription>
                      <ul className="list-disc space-y-1 pl-4">
                        {storageQuotaViolations.map((violation) => (
                          <li key={`${violation.resource}-${violation.message}`}>{violation.message}</li>
                        ))}
                      </ul>
                    </AlertDescription>
                  </Alert>
                ) : storageQuotaUnavailable ? (
                  <Alert>
                    <CircleAlert />
                    <AlertTitle>Live storage quota is unavailable</AlertTitle>
                    <AlertDescription>The API and the cluster will still validate this request.</AlertDescription>
                  </Alert>
                ) : selectedFlavor && selectedImage && projectQuota.data ? (
                  <p className="text-sm text-muted-foreground">This storage selection fits the current quota snapshot.</p>
                ) : null}
              </div>
            )}
          </Step>

          <Step n={5} title="Network">
            {networks.isLoading ? (
              <Skeleton className="h-16" />
            ) : !networkRows.length ? (
              <p className="text-sm text-muted-foreground">
                No networks in this project — create one under Networking first.
              </p>
            ) : (
              <div className="grid gap-2">
                {networkRows.map((n) => {
                  const ext = n.externalId ?? ""
                  const checked = netIds.includes(ext)
                  return (
                    <div
                      key={n.id}
                      className={cn(
                        "flex flex-wrap items-center gap-3 rounded-lg border p-3 text-sm transition-colors",
                        checked && "border-primary ring-1 ring-primary",
                      )}
                    >
                      <label className="flex min-w-0 cursor-pointer items-center gap-3">
                        <Checkbox
                          checked={checked}
                          disabled={!ext}
                          onCheckedChange={(v) => {
                            setNetIds(v === true ? [ext] : [])
                            setSubnet("")
                          }}
                        />
                        <span className="font-medium">{(n.data?.network?.name as string) ?? n.name ?? n.id}</span>
                        <span className="truncate font-mono text-xs text-muted-foreground">{ext}</span>
                      </label>
                      {checked && ((n.data?.network?.subnets as string[] | undefined)?.length ?? 0) > 1 && (
                        <Select value={subnet} onValueChange={setSubnet}>
                          <SelectTrigger className="w-56" aria-label="Subnet">
                            <SelectValue placeholder="First subnet" />
                          </SelectTrigger>
                          <SelectContent>
                            {((n.data?.network?.subnets ?? []) as string[]).map((c) => (
                              <SelectItem key={c} value={c}>
                                {c}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      )}
                    </div>
                  )
                })}
              </div>
            )}
          </Step>

          <Step n={6} title="Access (optional)">
            <div className="grid gap-4">
              <div className="grid max-w-md gap-2">
                <Label>Login method</Label>
                <div className="inline-flex w-fit overflow-hidden rounded-md border">
                  <Button
                    type="button"
                    variant={loginMethod === "key" ? "default" : "ghost"}
                    size="sm"
                    className="rounded-none"
                    onClick={() => setLoginMethod("key")}
                  >
                    SSH key pair
                  </Button>
                  <Button
                    type="button"
                    variant={loginMethod === "password" ? "default" : "ghost"}
                    size="sm"
                    className="rounded-none"
                    onClick={() => setLoginMethod("password")}
                  >
                    Password
                  </Button>
                </div>
                {loginMethod === "key" ? (
                  keypairs.isLoading ? (
                    <Skeleton className="h-9" />
                  ) : !keypairs.data?.length ? (
                    <p className="text-sm text-muted-foreground">
                      No key pairs in this project — create one under Compute → Key pairs first.
                    </p>
                  ) : (
                    <Select
                      value={keyName || "__none__"}
                      onValueChange={(v) => setKeyName(v === "__none__" ? "" : v)}
                    >
                      <SelectTrigger className="w-full" aria-label="Key pair">
                        <SelectValue placeholder="No key pair" />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="__none__">No key pair</SelectItem>
                        {keypairs.data
                          .filter((k) => !!keypairName(k))
                          .map((k) => (
                            <SelectItem key={k.id} value={keypairName(k)}>
                              {keypairName(k)}
                            </SelectItem>
                          ))}
                      </SelectContent>
                    </Select>
                  )
                ) : (
                  <div className="grid gap-2">
                    {userDataSupported && (
                      <Input
                        value={username}
                        onChange={(e) => setUsername(e.target.value)}
                        placeholder="Username (optional, e.g. devuser)"
                        autoComplete="off"
                      />
                    )}
                    <Input
                      type="password"
                      value={password}
                      onChange={(e) => setPassword(e.target.value)}
                      placeholder="Password"
                      autoComplete="new-password"
                    />
                    <p className="text-xs text-muted-foreground">
                      {userDataSupported
                        ? "With a username, a sudo login user is created via cloud-init (works on any cloud-init image). Without one, the password"
                        : "The password"}{" "}
                      is set on the image's default account (e.g.{" "}
                      <span className="font-mono">ubuntu</span>/<span className="font-mono">root</span>) and needs an image that supports password login.
                    </p>
                  </div>
                )}
              </div>

              {userDataSupported && (
                <div className="grid gap-2">
                  <Label htmlFor="user-data">User data (cloud-init, optional)</Label>
                  <Textarea
                    id="user-data"
                    value={userData}
                    onChange={(e) => setUserData(e.target.value)}
                    placeholder={"#cloud-config\n# runs on first boot"}
                    rows={5}
                    className="font-mono text-xs"
                  />
                  <p className="text-xs text-muted-foreground">
                    Passed to cloud-init on first boot — e.g. create users, install packages, run scripts.
                  </p>
                </div>
              )}

              {features.has("spreadGroups") && (
                <div className="grid max-w-md gap-2">
                  <Label htmlFor="spread-group">Spread group (optional)</Label>
                  <Input
                    id="spread-group"
                    value={spreadGroup}
                    onChange={(e) => setSpreadGroup(e.target.value)}
                    placeholder="web"
                    autoComplete="off"
                  />
                  <p className="text-xs text-muted-foreground">
                    Servers of one group are placed on different nodes where possible, at create time only.
                  </p>
                  <label className="flex items-center gap-2 text-sm">
                    <Checkbox
                      checked={spreadStrict}
                      disabled={!spreadGroup.trim()}
                      onCheckedChange={(v) => setSpreadStrict(v === true)}
                    />
                    Refuse to create rather than share a node
                  </label>
                </div>
              )}

              {editableMetadata.length > 0 && (
                <div className="grid max-w-md gap-2">
                  <Label>Instance metadata (optional)</Label>
                  {editableMetadata.map((o) => (
                    <div key={o.key} className="grid gap-1">
                      <Label htmlFor={`md-${o.key}`} className="text-xs text-muted-foreground">
                        {o.displayName ?? o.key}
                      </Label>
                      {o.options?.length ? (
                        <Select
                          value={metadata[o.key] ?? ""}
                          onValueChange={(v) => setMetadata((m) => ({ ...m, [o.key]: v }))}
                        >
                          <SelectTrigger id={`md-${o.key}`}>
                            <SelectValue placeholder="Not set" />
                          </SelectTrigger>
                          <SelectContent>
                            {o.options.map((v) => (
                              <SelectItem key={v.value} value={v.value ?? ""}>
                                {v.displayName ?? v.value}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      ) : (
                        <Input
                          id={`md-${o.key}`}
                          value={metadata[o.key] ?? ""}
                          onChange={(e) => setMetadata((m) => ({ ...m, [o.key]: e.target.value }))}
                        />
                      )}
                    </div>
                  ))}
                </div>
              )}

              <div className="grid gap-2">
                <Label>Security groups</Label>
                {secgroups.isLoading ? (
                  <Skeleton className="h-9" />
                ) : !secgroups.data?.length ? (
                  <p className="text-sm text-muted-foreground">
                    No security groups in this project — the default group will apply.
                  </p>
                ) : (
                  <div className="grid gap-2">
                    {secgroups.data
                      .filter((sg) => !!sgName(sg))
                      .map((sg) => {
                        const n = sgName(sg)
                        const checked = sgNames.includes(n)
                        return (
                          <label
                            key={sg.id}
                            className={cn(
                              "flex cursor-pointer items-center gap-3 rounded-lg border p-3 text-sm transition-colors hover:border-primary/50",
                              checked && "border-primary ring-1 ring-primary",
                            )}
                          >
                            <Checkbox
                              checked={checked}
                              onCheckedChange={(v) =>
                                setSgNames((ns) => (v === true ? [...ns, n] : ns.filter((x) => x !== n)))
                              }
                            />
                            <span className="font-medium">{n}</span>
                            <span className="text-xs text-muted-foreground">
                              {(sg.data?.securityGroup?.description as string) || ""}
                            </span>
                          </label>
                        )
                      })}
                  </div>
                )}
              </div>
            </div>
          </Step>

          <Step n={7} title="Name">
            <div className="grid max-w-md gap-2">
              <Label htmlFor="server-name">Server name</Label>
              <Input
                id="server-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="my-server"
              />
            </div>
          </Step>

          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <Button
              onClick={() => create.mutate()}
              disabled={!ready || quotaCheckPending || quotaViolations.length > 0 || create.isPending}
            >
              <Server className="size-4" />
              {create.isPending ? "Creating…" : "Create server"}
            </Button>
            <Button variant="outline" asChild>
              <Link to={`/p/${pid}/servers`}>Cancel</Link>
            </Button>
            {missing.length > 0 ? (
              <p className="text-sm text-muted-foreground">
                Still needed:{" "}
                {missing.length > 1
                  ? `${missing.slice(0, -1).join(", ")} and ${missing[missing.length - 1]}`
                  : missing[0]}
                .
              </p>
            ) : null}
          </div>
        </div>
      )}
    </>
  )
}

function Step({ n, title, children }: { n: number; title: string; children: React.ReactNode }) {
  return (
    <Card className="gap-4">
      <CardHeader className="gap-1">
        <div className="text-eyebrow">Step {n}</div>
        <CardTitle className="text-base">{title}</CardTitle>
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  )
}
