// Object storage: Swift and Ceph S3 run SIDE BY SIDE as two independent products over two disjoint
// bucket sets. Nothing is ever migrated between them, so the UI must always say which store a bucket
// lives on, and the create form must make the user pick one.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { apiFetch } from "./api"
import { useLocations } from "./hooks"
import type { CloudResource, Location } from "./types"

export type StorageBackend = "SWIFT" | "CEPH_S3"

export const BACKEND_LABEL: Record<StorageBackend, string> = {
  SWIFT: "Swift",
  CEPH_S3: "S3 (Ceph)",
}

/** Which object store a bucket lives on. Cached rows written before this field existed read as Swift. */
export function bucketBackend(r: CloudResource): StorageBackend {
  return (r.data?.storageBackend as StorageBackend) ?? "SWIFT"
}

export function isS3Location(loc: Location | undefined): boolean {
  return loc?.provider === "ceph-s3"
}

/** Locations that can hold a bucket, i.e. the object-store services this project is attached to. */
export function useBucketLocations(pid: string) {
  const { data, ...rest } = useLocations(pid)
  const locations = (data ?? []).filter((l) => l.resourceTypes?.includes("BUCKET"))
  return { locations, ...rest }
}

// --- per-bucket settings (ceph-s3 only) ---

export type BucketWebsite = {
  enabled: boolean
  indexDocument?: string
  errorDocument?: string
  url?: string
  /** true ⇒ every object in the bucket is readable by anyone on the internet. */
  publicObjects: boolean
}

export type BucketGrant = { uid: string; permission: "READ" | "READ_WRITE" | "FULL" }

export type LifecycleRule = {
  id: string
  prefix?: string
  enabled: boolean
  expirationDays?: number
  noncurrentVersionExpirationDays?: number
  abortIncompleteMultipartUploadDays?: number
}

export type CorsRule = {
  allowedMethods: string[]
  allowedOrigins: string[]
  allowedHeaders?: string[]
  exposeHeaders?: string[]
  maxAgeSeconds?: number
}

export type BucketSettings = {
  versioning: "Enabled" | "Suspended" | "Disabled"
  objectLock?: { enabled: boolean; mode?: string; days?: number }
  quota: { enabled: boolean; maxSizeBytes: number; maxObjects: number }
  lifecycle: LifecycleRule[]
  cors: CorsRule[]
  tags: Record<string, string>
  grants: BucketGrant[]
  policyJson?: string
  website?: BucketWebsite
  indexType?: string
  placementRule?: string
}

/** Cloud action against ONE bucket. The server resolves the backend from the resource itself. */
export function bucketAction<T = unknown>(
  pid: string,
  resourceId: string,
  action: string,
  data?: Record<string, unknown>,
) {
  return apiFetch<{ result?: T }>(`/project/${pid}/cloud/${resourceId}/action`, {
    method: "POST",
    body: { action, ...(data ? { data } : {}) },
  }).then((r) => r.result as T)
}

export function useBucketSettings(pid: string, resourceId: string, enabled = true) {
  return useQuery({
    queryKey: ["bucket-settings", pid, resourceId],
    queryFn: () => bucketAction<BucketSettings>(pid, resourceId, "GET_SETTINGS"),
    enabled: !!pid && !!resourceId && enabled,
  })
}

/** A settings-shaped payload always carries `versioning`; ENABLE_WEBSITE/DISABLE_WEBSITE do not. */
function isBucketSettings(v: unknown): v is BucketSettings {
  return !!v && typeof v === "object" && "versioning" in (v as Record<string, unknown>)
}

export function useBucketSettingsMutation(pid: string, resourceId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ action, data }: { action: string; data?: Record<string, unknown> }) =>
      bucketAction<unknown>(pid, resourceId, action, data),
    onSuccess: (result) => {
      // Most mutating actions return the freshly-read settings, so seed the cache instead of refetching.
      // The website actions return a BucketWebsite (or nothing) — seeding that would leave `quota`,
      // `grants`, `lifecycle` undefined and the dialog would render garbage. Refetch in that case.
      if (isBucketSettings(result)) qc.setQueryData(["bucket-settings", pid, resourceId], result)
      else void qc.invalidateQueries({ queryKey: ["bucket-settings", pid, resourceId] })
    },
  })
}

// --- project S3 credentials + extra keys ---

export type S3Credentials = {
  accessKey: string
  secretKey: string
  rgwUid: string
  s3Endpoint: string
  websiteEndpoint?: string
  region: string
  warning?: string
}

export type S3Key = {
  id: string
  name: string
  rgwUid: string
  accessKey: string
  secretKey?: string
  createdAt?: string
}

export function useS3Credentials(pid: string, enabled = true) {
  return useQuery({
    queryKey: ["s3-credentials", pid],
    queryFn: () => apiFetch<S3Credentials>(`/project/${pid}/s3-credentials`),
    enabled: !!pid && enabled,
    retry: false,
  })
}

export function useS3Keys(pid: string, enabled = true) {
  return useQuery({
    queryKey: ["s3-keys", pid],
    queryFn: () => apiFetch<S3Key[]>(`/project/${pid}/s3-keys`),
    enabled: !!pid && enabled,
    retry: false,
  })
}
