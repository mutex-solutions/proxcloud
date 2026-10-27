import { useEffect, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { ImageIcon } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { useAdminGet } from "@/lib/hooks"

type PlatformConfig = {
  id: string | Record<string, unknown>
  name?: string
  language?: string
  branding?: { name?: string; color?: string; logo?: string; faviconUrl?: string } & Record<string, unknown>
  dateConfiguration?: { dateFormat?: string }
  projectProvisioningQuota?: { enabled: boolean; limit: number }
  organizationProvisioningQuota?: { enabled: boolean; limit: number }
  regions?: Array<{ serviceId: string; region: string; order: number }>
  [k: string]: unknown
}

type BillingConfig = {
  id?: string
  baseCurrency?: string
  promotionCodesEnabled?: boolean
  defaultConfiguration?: boolean
}

type ServiceItem = { id: string; name?: string; config?: { regions?: Record<string, unknown> } }
type Currency = { currency_code: string; currency_name: string }

const PLATFORM_PATH = "/admin/platform-configuration/current"
const BILLING_PATH = "/admin/billing/configuration/current"

type PlatformForm = {
  name: string
  brandingName: string
  brandingColor: string
  brandingLogo: string
  brandingFaviconUrl: string
  dateFormat: string
  quotaEnabled: boolean
  quotaLimit: string
  orgQuotaEnabled: boolean
  orgQuotaLimit: string
}

function platformConfigToForm(cfg: PlatformConfig): PlatformForm {
  return {
    name: cfg.name ?? "",
    brandingName: cfg.branding?.name ?? "",
    brandingColor: cfg.branding?.color ?? "",
    brandingLogo: cfg.branding?.logo ?? "",
    brandingFaviconUrl: cfg.branding?.faviconUrl ?? "",
    dateFormat: cfg.dateConfiguration?.dateFormat ?? "",
    quotaEnabled: cfg.projectProvisioningQuota?.enabled === true,
    quotaLimit: String(cfg.projectProvisioningQuota?.limit ?? 0),
    orgQuotaEnabled: cfg.organizationProvisioningQuota?.enabled === true,
    orgQuotaLimit: String(cfg.organizationProvisioningQuota?.limit ?? 0),
  }
}

function brandAssetUrlError(value: string): string | null {
  const candidate = value.trim()
  if (!candidate) return null
  if (candidate.startsWith("/") && !candidate.startsWith("//")) return null
  try {
    const url = new URL(candidate)
    if (url.protocol === "http:" || url.protocol === "https:") return null
  } catch {
    // Fall through to the user-facing validation message.
  }
  return "Enter an HTTP(S) URL or a root-relative path beginning with /."
}

function BrandAssetField({
  id,
  label,
  value,
  placeholder,
  widePreview = false,
  onChange,
}: {
  id: string
  label: string
  value: string
  placeholder: string
  widePreview?: boolean
  onChange: (value: string) => void
}) {
  const [failedPreview, setFailedPreview] = useState<string | null>(null)
  const previewUrl = value.trim()
  const urlError = brandAssetUrlError(value)
  const previewFailed = Boolean(previewUrl) && failedPreview === previewUrl
  const canPreview = Boolean(previewUrl) && !urlError && !previewFailed
  const hint = urlError
    ?? (previewFailed
      ? "Preview could not be loaded. Check that the asset is publicly reachable."
      : "Use an HTTPS URL or root-relative path. Leave empty to use the default asset.")

  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      <div className="flex min-w-0 items-center gap-3">
        <div
          className={`${widePreview ? "h-12 w-20" : "size-12"} flex shrink-0 items-center justify-center overflow-hidden rounded-lg border bg-muted/30`}
        >
          {canPreview ? (
            <img
              src={previewUrl}
              alt=""
              className={widePreview ? "max-h-10 max-w-16 object-contain" : "size-8 object-contain"}
              loading="lazy"
              decoding="async"
              referrerPolicy="no-referrer"
              onError={() => setFailedPreview(previewUrl)}
            />
          ) : (
            <ImageIcon className="size-5 text-muted-foreground/60" aria-hidden="true" />
          )}
        </div>
        <Input
          id={id}
          type="text"
          inputMode="url"
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          maxLength={2048}
          placeholder={placeholder}
          value={value}
          aria-invalid={urlError ? true : undefined}
          aria-describedby={`${id}-hint`}
          onChange={(event) => {
            setFailedPreview(null)
            onChange(event.target.value)
          }}
        />
      </div>
      <p
        id={`${id}-hint`}
        aria-live="polite"
        className={`text-xs ${urlError || previewFailed ? "text-destructive" : "text-muted-foreground"}`}
      >
        {hint}
      </p>
    </div>
  )
}

export default function ConfigurationPage() {
  const qc = useQueryClient()
  const platformQ = useAdminGet<PlatformConfig>(PLATFORM_PATH)
  const billingQ = useAdminGet<BillingConfig>(BILLING_PATH)
  const cfg = platformQ.data

  const [form, setForm] = useState<PlatformForm | null>(null)
  useEffect(() => {
    // cfg.id guard: an empty envelope ({} — nothing stored yet) unwraps to a truthy object.
    if (cfg?.id && !form) {
      setForm(platformConfigToForm(cfg))
    }
  }, [cfg, form])

  const save = useMutation({
    mutationFn: () => {
      if (!cfg || !form) throw new Error("Configuration not loaded")
      const brandingUrlError = brandAssetUrlError(form.brandingLogo)
        ?? brandAssetUrlError(form.brandingFaviconUrl)
      if (brandingUrlError) throw new Error(brandingUrlError)
      // PUT /{id} REPLACES the stored document — send back every field the read returned, merged
      // with the edits, or unedited fields would be lost.
      const id = typeof cfg.id === "string" ? cfg.id : String(cfg.id)
      const body: PlatformConfig = {
        ...cfg,
        name: form.name,
        branding: {
          ...(cfg.branding ?? {}),
          name: form.brandingName,
          color: form.brandingColor,
          logo: form.brandingLogo.trim(),
          faviconUrl: form.brandingFaviconUrl.trim(),
        },
        dateConfiguration: { ...(cfg.dateConfiguration ?? {}), dateFormat: form.dateFormat },
        projectProvisioningQuota: { enabled: form.quotaEnabled, limit: Number(form.quotaLimit) || 0 },
        organizationProvisioningQuota: { enabled: form.orgQuotaEnabled, limit: Number(form.orgQuotaLimit) || 0 },
      }
      return apiFetch<PlatformConfig>(`/admin/platform-configuration/${id}`, { method: "PUT", body })
    },
    onSuccess: (saved) => {
      // Hydrate from the PUT response before the background refetch. Resetting to null first can
      // race with stale cached data and make a successful branding change appear reverted.
      qc.setQueryData<PlatformConfig>(["admin-get", PLATFORM_PATH], saved)
      setForm(platformConfigToForm(saved))
      void qc.invalidateQueries({ queryKey: ["admin-get", PLATFORM_PATH] })
      toast.success("Platform configuration saved")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  // Regions display config ({serviceId, region, order} rows) — PUT /{id}/regions replaces the
  // whole list, so add/remove rebuild it and reindex order by position.
  const servicesQ = useAdminGet<ServiceItem[]>("/admin/service")
  const [regSvc, setRegSvc] = useState("")
  const [regName, setRegName] = useState("")
  const saveRegions = useMutation({
    mutationFn: (regions: Array<{ serviceId: string; region: string; order: number }>) =>
      apiFetch(`/admin/platform-configuration/${cfg?.id}/regions`, { method: "PUT", body: regions }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin-get", PLATFORM_PATH] })
      toast.success("Regions saved")
    },
    onError: (e) => toast.error((e as Error).message),
  })
  const regionOptions = Object.keys(servicesQ.data?.find((s) => s.id === regSvc)?.config?.regions ?? {}).sort()
  const reindex = (rows: Array<{ serviceId: string; region: string; order: number }>) =>
    rows.map((r, i) => ({ ...r, order: i + 1 }))
  const addRegion = () => {
    const cur = cfg?.regions ?? []
    if (cur.some((r) => r.serviceId === regSvc && r.region === regName)) {
      toast.error("Region already configured")
      return
    }
    saveRegions.mutate(reindex([...cur, { serviceId: regSvc, region: regName, order: cur.length + 1 }]))
    setRegName("")
  }

  // Billing config: a fresh install has none — offer create (base currency + promo toggle).
  // Editing an EXISTING config stays disabled (partial read + replace-all update would wipe fields).
  const currenciesQ = useAdminGet<Currency[]>("/admin/billing/configuration/currencies", !billingQ.data?.id)
  const [newCurrency, setNewCurrency] = useState("")
  const [newPromo, setNewPromo] = useState(false)
  const createBilling = useMutation({
    mutationFn: () =>
      apiFetch("/admin/billing/configuration", {
        method: "POST",
        body: { baseCurrency: newCurrency, promotionCodesEnabled: newPromo, defaultConfiguration: true },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin-get", BILLING_PATH] })
      toast.success("Billing configuration created")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const hasBrandAssetError = form
    ? Boolean(brandAssetUrlError(form.brandingLogo) || brandAssetUrlError(form.brandingFaviconUrl))
    : false

  return (
    <>
      <PageHeader
        title="Configuration"
        eyebrow="System"
        description="Platform branding, formats, quota and billing basics."
      />

      <div className="space-y-6">
        <Card>
          <CardHeader>
            <CardTitle className="text-eyebrow">Platform</CardTitle>
          </CardHeader>
          <CardContent>
            {platformQ.isLoading ? (
              <Skeleton className="h-48" />
            ) : platformQ.error ? (
              <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">
                {(platformQ.error as Error).message}
              </div>
            ) : !cfg?.id || !form ? (
              <p className="text-sm text-muted-foreground">No platform configuration found.</p>
            ) : (
              <div className="space-y-4">
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-1.5">
                    <Label htmlFor="pc-name">Configuration name</Label>
                    <Input id="pc-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="pc-brand">Brand name</Label>
                    <Input id="pc-brand" value={form.brandingName} onChange={(e) => setForm({ ...form, brandingName: e.target.value })} />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="pc-color">Brand color</Label>
                    <Input id="pc-color" placeholder="#4f46e5" value={form.brandingColor} onChange={(e) => setForm({ ...form, brandingColor: e.target.value })} />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="pc-date">Date format</Label>
                    <Input id="pc-date" placeholder="DD/MM/YYYY" value={form.dateFormat} onChange={(e) => setForm({ ...form, dateFormat: e.target.value })} />
                  </div>
                </div>

                <div className="grid gap-4 md:grid-cols-2">
                  <BrandAssetField
                    id="pc-logo"
                    label="Logo"
                    value={form.brandingLogo}
                    placeholder="https://example.com/logo.svg"
                    widePreview
                    onChange={(brandingLogo) => setForm({ ...form, brandingLogo })}
                  />
                  <BrandAssetField
                    id="pc-favicon"
                    label="Favicon"
                    value={form.brandingFaviconUrl}
                    placeholder="https://example.com/favicon.ico"
                    onChange={(brandingFaviconUrl) => setForm({ ...form, brandingFaviconUrl })}
                  />
                </div>

                <div className="space-y-3">
                  <div className="flex flex-wrap items-center gap-x-6 gap-y-3 rounded-lg border p-4">
                    <div className="flex items-center gap-2">
                      <Switch
                        id="pc-quota"
                        checked={form.quotaEnabled}
                        onCheckedChange={(v) => setForm({ ...form, quotaEnabled: v })}
                      />
                      <Label htmlFor="pc-quota" className="text-sm font-normal">Project provisioning quota</Label>
                    </div>
                    <div className="flex items-center gap-2">
                      <Label htmlFor="pc-limit" className="text-sm text-muted-foreground">Limit</Label>
                      <Input
                        id="pc-limit"
                        type="number"
                        min={0}
                        className="w-24"
                        value={form.quotaLimit}
                        disabled={!form.quotaEnabled}
                        onChange={(e) => setForm({ ...form, quotaLimit: e.target.value })}
                      />
                    </div>
                    <p className="text-xs text-muted-foreground">Max projects per organization. 0 = only operators create projects.</p>
                  </div>
                  <div className="flex flex-wrap items-center gap-x-6 gap-y-3 rounded-lg border p-4">
                    <div className="flex items-center gap-2">
                      <Switch
                        id="pc-org-quota"
                        checked={form.orgQuotaEnabled}
                        onCheckedChange={(v) => setForm({ ...form, orgQuotaEnabled: v })}
                      />
                      <Label htmlFor="pc-org-quota" className="text-sm font-normal">Organization provisioning quota</Label>
                    </div>
                    <div className="flex items-center gap-2">
                      <Label htmlFor="pc-org-limit" className="text-sm text-muted-foreground">Limit</Label>
                      <Input
                        id="pc-org-limit"
                        type="number"
                        min={0}
                        className="w-24"
                        value={form.orgQuotaLimit}
                        disabled={!form.orgQuotaEnabled}
                        onChange={(e) => setForm({ ...form, orgQuotaLimit: e.target.value })}
                      />
                    </div>
                    <p className="text-xs text-muted-foreground">Max organizations a user can own. 0 = only operators create organizations and assign members.</p>
                  </div>
                </div>

                <div className="space-y-3">
                  <div className="text-eyebrow mb-2">Regions</div>
                  {(cfg.regions ?? []).length === 0 ? (
                    <p className="text-sm text-muted-foreground">No regions configured.</p>
                  ) : (
                    <Card className="overflow-hidden py-0">
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>Service</TableHead>
                            <TableHead>Region</TableHead>
                            <TableHead>Order</TableHead>
                            <TableHead />
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {(cfg.regions ?? []).map((r, i) => (
                            <TableRow key={i}>
                              <TableCell className="font-mono text-xs">
                                {servicesQ.data?.find((s) => s.id === r.serviceId)?.name ?? r.serviceId}
                              </TableCell>
                              <TableCell>{r.region}</TableCell>
                              <TableCell className="tabular-nums">{r.order}</TableCell>
                              <TableCell className="text-right">
                                <Button
                                  variant="ghost"
                                  size="sm"
                                  disabled={saveRegions.isPending}
                                  onClick={() => saveRegions.mutate(reindex((cfg.regions ?? []).filter((_, idx) => idx !== i)))}
                                >
                                  Remove
                                </Button>
                              </TableCell>
                            </TableRow>
                          ))}
                        </TableBody>
                      </Table>
                    </Card>
                  )}
                  <div className="flex flex-wrap items-end gap-3">
                    <div className="w-full space-y-1.5 sm:w-auto">
                      <Label htmlFor="pc-reg-svc" className="text-xs text-muted-foreground">Cloud provider</Label>
                      <Select
                        value={regSvc}
                        onValueChange={(v) => {
                          setRegSvc(v)
                          setRegName("")
                        }}
                      >
                        <SelectTrigger id="pc-reg-svc" className="w-full sm:w-56">
                          <SelectValue placeholder="Select provider" />
                        </SelectTrigger>
                        <SelectContent>
                          {(servicesQ.data ?? []).map((s) => (
                            <SelectItem key={s.id} value={s.id}>
                              {s.name ?? s.id}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    </div>
                    <div className="w-full space-y-1.5 sm:w-auto">
                      <Label htmlFor="pc-reg-name" className="text-xs text-muted-foreground">Region</Label>
                      <Select value={regName} onValueChange={setRegName} disabled={!regSvc || regionOptions.length === 0}>
                        <SelectTrigger id="pc-reg-name" className="w-full sm:w-48">
                          <SelectValue placeholder={regSvc && regionOptions.length === 0 ? "No regions on provider" : "Select region"} />
                        </SelectTrigger>
                        <SelectContent>
                          {regionOptions.map((r) => (
                            <SelectItem key={r} value={r}>
                              {r}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    </div>
                    <Button variant="outline" onClick={addRegion} disabled={!regSvc || !regName || saveRegions.isPending}>
                      {saveRegions.isPending ? "Saving…" : "Add region"}
                    </Button>
                  </div>
                </div>

                <Button onClick={() => save.mutate()} disabled={save.isPending || hasBrandAssetError}>
                  {save.isPending ? "Saving…" : "Save platform configuration"}
                </Button>
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-eyebrow">Billing</CardTitle>
          </CardHeader>
          <CardContent>
            {billingQ.isLoading ? (
              <Skeleton className="h-24" />
            ) : billingQ.error ? (
              <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">
                {(billingQ.error as Error).message}
              </div>
            ) : !billingQ.data?.id ? (
              <div className="space-y-4">
                <p className="text-sm text-muted-foreground">
                  No billing configuration yet — pick a base currency to create the default one.
                </p>
                <div className="flex flex-wrap items-end gap-3">
                  <div className="w-full space-y-1.5 sm:w-auto">
                    <Label htmlFor="pc-currency" className="text-xs text-muted-foreground">Base currency</Label>
                    <Select value={newCurrency} onValueChange={setNewCurrency}>
                      <SelectTrigger id="pc-currency" className="w-full sm:w-72">
                        <SelectValue placeholder="Select currency" />
                      </SelectTrigger>
                      <SelectContent>
                        {(currenciesQ.data ?? []).map((c) => (
                          <SelectItem key={c.currency_code} value={c.currency_code}>
                            {c.currency_code} — {c.currency_name}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="flex items-center gap-2 pb-2">
                    <Switch id="pc-promo" checked={newPromo} onCheckedChange={setNewPromo} />
                    <Label htmlFor="pc-promo" className="text-sm font-normal">Promotion codes</Label>
                  </div>
                  <Button onClick={() => createBilling.mutate()} disabled={!newCurrency || createBilling.isPending}>
                    {createBilling.isPending ? "Creating…" : "Create billing configuration"}
                  </Button>
                </div>
              </div>
            ) : (
              <div className="space-y-3">
                <div className="flex items-center justify-between border-b py-2 text-sm">
                  <span className="text-muted-foreground">Base currency</span>
                  <span className="font-mono">{billingQ.data.baseCurrency ?? "—"}</span>
                </div>
                <div className="flex items-center justify-between py-2 text-sm">
                  <span className="text-muted-foreground">Promotion codes</span>
                  <StatusBadge status={billingQ.data.promotionCodesEnabled ? "ENABLED" : "DISABLED"} />
                </div>
                {/* ponytail: read-only by design — the read DTO only exposes 4 fields while
                    PUT /admin/billing/configuration/{id} overwrites ALL 13 mutable fields (an
                    omitted field is nulled), so saving from this thin read would wipe stored
                    settings/suspension config. Enable editing once a full-shape read exists. */}
                <p className="text-xs text-muted-foreground">
                  Editing is disabled: the API's billing-configuration read is partial and its update replaces
                  unspecified fields.
                </p>
              </div>
            )}
          </CardContent>
        </Card>
      </div>
    </>
  )
}
