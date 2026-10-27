import { useCallback } from "react"
import { useSearchParams } from "react-router-dom"
import { useQuery } from "@tanstack/react-query"
import { apiFetch, apiFetchEnvelope, type Envelope } from "./api"

// Deep-linkable detail-page tabs: ?tab=<value> round-trips through the URL so
// operators can share/bookmark a specific tab. The default tab keeps a clean URL.
export function useTabParam(defaultTab: string): [string, (tab: string) => void] {
  const [params, setParams] = useSearchParams()
  const tab = params.get("tab") ?? defaultTab
  const setTab = useCallback(
    (next: string) => {
      setParams(
        (prev) => {
          const p = new URLSearchParams(prev)
          if (next === defaultTab) p.delete("tab")
          else p.set("tab", next)
          return p
        },
        { replace: true },
      )
    },
    [defaultTab, setParams],
  )
  return [tab, setTab]
}

// Generic paged admin list — most /admin/* collections share the
// { data: [...], paging } envelope.
export function useAdminList<T = Record<string, any>>(path: string, enabled = true) {
  return useQuery({
    queryKey: ["admin-list", path],
    queryFn: () => apiFetchEnvelope<T[]>(path) as Promise<Envelope<T[]>>,
    enabled,
  })
}

export function useAdminGet<T = Record<string, any>>(path: string, enabled = true) {
  return useQuery({
    queryKey: ["admin-get", path],
    queryFn: () => apiFetch<T>(path),
    enabled,
  })
}

export type AdminStats = {
  users?: number
  projects?: number
  cloudResources?: number
  transactions?: number
  cloudProviderConfigured?: boolean
  billingConfigured?: boolean
  brandingConfigured?: boolean
  mailGatewayConfigured?: boolean
  pricePlanConfigured?: boolean
  insights?: {
    bills?: Array<{ year: number; month: number; total?: Record<string, number> }>
    payments?: Array<{ year: number; month: number; total?: Record<string, number> }>
    newUsers?: Array<{ year: number; month: number; day: number; count: number }>
    newBillingProfiles?: Array<{ year: number; month: number; day: number; count: number }>
  }
  [k: string]: unknown
}

export function useAdminStats() {
  return useQuery({
    queryKey: ["admin-stats"],
    queryFn: () => apiFetch<AdminStats>("/admin/stats"),
  })
}
