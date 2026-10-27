import { useEffect, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Plus, Trash2, TriangleAlert } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { useAdminGet, useAdminList } from "@/lib/hooks"

// The read (GET /admin/billing/configuration/current) returns the FULL stored document.
// Save is PUT /admin/billing/configuration/{id}, which REPLACES exactly these mutable fields
// (an omitted field is set to null): name, address, company, baseCurrency, mailGatewayId,
// invoiceGatewayId, settings, defaultConfiguration, promotionCodesEnabled, provisioningSettings,
// autoActivationFlow, suspensionConfiguration, savingsContractNotificationConfig.
// So every save sends the full merged doc: everything read + the edits.

const CURRENT_PATH = "/admin/billing/configuration/current"

type SuspensionLimit = { balance?: number; days?: number }

type BillingConfig = {
  id?: string
  name?: string
  address?: { country?: string; city?: string; address?: string } & Record<string, unknown>
  company?: { vatId?: string; businessName?: string } & Record<string, unknown>
  baseCurrency?: string
  mailGatewayId?: string
  invoiceGatewayId?: string
  settings?: { timeUnitLimits?: Record<string, number> } & Record<string, unknown>
  defaultConfiguration?: boolean
  promotionCodesEnabled?: boolean
  provisioningSettings?: { promotionals?: Array<{ amount?: number; daysValidity?: number }> } & Record<string, unknown>
  autoActivationFlow?: {
    autoActivationEnabled?: boolean
    kyc?: string
    paymentMethod?: string
    paymentMethodCard?: string
    paymentMethodDeposit?: string
    minimumDepositAmount?: number
    billingProfileValidation?: string
  } & Record<string, unknown>
  suspensionConfiguration?: {
    enabled?: boolean
    type?: string
    suspendedAt?: SuspensionLimit
    notifications?: SuspensionLimit[]
  } & Record<string, unknown>
  savingsContractNotificationConfig?: {
    sendExpiryNotification?: boolean
    reminderDaysBeforeExpiry?: number[]
  } & Record<string, unknown>
  [k: string]: unknown
}

type Country = { name: string; cca2: string; cca3?: string; ccn3?: number }
type Currency = { country?: string; currency_name?: string; currency_code: string; numeric_code?: string }
type Integration = { id: string; name?: string; thirdParty?: string; [k: string]: unknown }

// Enum values are wire values; labels are sentence-cased for display.
const CONSTRAINTS = [
  { value: "DISABLED", label: "Disabled" },
  { value: "REQUIRED", label: "Required" },
  { value: "ALTERNATIVE", label: "Alternative" },
]
const NONE = "__none__"

type LimitRow = { balance: string; days: string }
type PromoRow = { amount: string; daysValidity: string }

type Form = {
  // business details
  name: string
  businessName: string
  vatId: string
  country: string // alpha-2 code, "" = unset
  city: string
  street: string
  baseCurrency: string
  mailGatewayId: string
  invoiceGatewayId: string
  defaultConfiguration: boolean
  // activation
  autoActivationEnabled: boolean
  paymentMethod: string
  paymentMethodCard: string
  paymentMethodDeposit: string
  billingProfileValidation: string
  minimumDepositAmount: string
  promotionals: PromoRow[]
  // settings
  promotionCodesEnabled: boolean
  suspEnabled: boolean
  suspType: string
  suspendedAtBalance: string
  suspendedAtDays: string
  notifications: LimitRow[]
  tuMinute: string
  tuHour: string
  tuMonth: string
  sendExpiryNotification: boolean
  reminderDays: string // comma separated
}

const numStr = (v: unknown) => (v === undefined || v === null ? "" : String(v))

function formFromDoc(cfg: BillingConfig): Form {
  const flow = cfg.autoActivationFlow ?? {}
  const susp = cfg.suspensionConfiguration ?? {}
  const tu = cfg.settings?.timeUnitLimits ?? {}
  const savings = cfg.savingsContractNotificationConfig ?? {}
  return {
    name: cfg.name ?? "",
    businessName: cfg.company?.businessName ?? "",
    vatId: cfg.company?.vatId ?? "",
    country: cfg.address?.country ?? "",
    city: cfg.address?.city ?? "",
    street: cfg.address?.address ?? "",
    baseCurrency: cfg.baseCurrency ?? "",
    mailGatewayId: cfg.mailGatewayId ?? "",
    invoiceGatewayId: cfg.invoiceGatewayId ?? "",
    defaultConfiguration: cfg.defaultConfiguration === true,
    autoActivationEnabled: flow.autoActivationEnabled === true,
    paymentMethod: flow.paymentMethod ?? "DISABLED",
    paymentMethodCard: flow.paymentMethodCard ?? "DISABLED",
    paymentMethodDeposit: flow.paymentMethodDeposit ?? "DISABLED",
    billingProfileValidation: flow.billingProfileValidation ?? "DISABLED",
    minimumDepositAmount: numStr(flow.minimumDepositAmount),
    promotionals: (cfg.provisioningSettings?.promotionals ?? []).map((p) => ({
      amount: numStr(p.amount),
      daysValidity: numStr(p.daysValidity),
    })),
    promotionCodesEnabled: cfg.promotionCodesEnabled === true,
    suspEnabled: susp.enabled === true,
    suspType: susp.type ?? "BALANCE",
    suspendedAtBalance: numStr(susp.suspendedAt?.balance),
    suspendedAtDays: numStr(susp.suspendedAt?.days),
    notifications: (susp.notifications ?? []).map((n) => ({ balance: numStr(n.balance), days: numStr(n.days) })),
    tuMinute: numStr(tu.minute),
    tuHour: numStr(tu.hour),
    tuMonth: numStr(tu.month),
    sendExpiryNotification: savings.sendExpiryNotification === true,
    reminderDays: (savings.reminderDaysBeforeExpiry ?? []).join(", "),
  }
}

const toNum = (s: string): number | undefined => {
  const n = parseFloat(s)
  return Number.isFinite(n) ? n : undefined
}

const toInt = (s: string): number | undefined => {
  const n = parseInt(s, 10)
  return Number.isFinite(n) ? n : undefined
}

function limit(row: LimitRow): SuspensionLimit | undefined {
  const balance = toNum(row.balance)
  const days = toInt(row.days)
  if (balance === undefined && days === undefined) return undefined
  const out: SuspensionLimit = {}
  if (balance !== undefined) out.balance = balance
  if (days !== undefined) out.days = days
  return out
}

// buildBody merges the read doc with the form. Nested blocks absent from the stored doc are only
// created when the user actually changed them (creating e.g. an autoActivationFlow of defaults
// would silently switch on the "auto-activation configured" behavior platform-wide).
function buildBody(cfg: BillingConfig, form: Form, initial: Form): Record<string, unknown> {
  const dirty = (...keys: (keyof Form)[]) =>
    keys.some((k) => JSON.stringify(form[k]) !== JSON.stringify(initial[k]))

  const body: Record<string, unknown> = { ...cfg }
  delete body.id

  body.name = form.name.trim() || undefined
  body.baseCurrency = form.baseCurrency || undefined
  body.mailGatewayId = form.mailGatewayId || undefined
  body.invoiceGatewayId = form.invoiceGatewayId || undefined
  body.defaultConfiguration = form.defaultConfiguration
  body.promotionCodesEnabled = form.promotionCodesEnabled

  const address: Record<string, unknown> = { ...(cfg.address ?? {}) }
  delete address.country
  delete address.city
  delete address.address
  if (form.country) address.country = form.country
  if (form.city.trim()) address.city = form.city.trim()
  if (form.street.trim()) address.address = form.street.trim()
  body.address = Object.keys(address).length ? address : undefined

  const company: Record<string, unknown> = { ...(cfg.company ?? {}) }
  delete company.businessName
  delete company.vatId
  if (form.businessName.trim()) company.businessName = form.businessName.trim()
  if (form.vatId.trim()) company.vatId = form.vatId.trim()
  body.company = Object.keys(company).length ? company : undefined

  // settings.timeUnitLimits — merge into whatever else lives under settings.
  const tu: Record<string, number> = {}
  const mi = toInt(form.tuMinute)
  const ho = toInt(form.tuHour)
  const mo = toInt(form.tuMonth)
  if (mi !== undefined) tu.minute = mi
  if (ho !== undefined) tu.hour = ho
  if (mo !== undefined) tu.month = mo
  if (cfg.settings || Object.keys(tu).length) {
    body.settings = { ...(cfg.settings ?? {}), timeUnitLimits: Object.keys(tu).length ? tu : undefined }
  } else {
    body.settings = undefined
  }

  if (
    cfg.autoActivationFlow ||
    dirty(
      "autoActivationEnabled",
      "paymentMethod",
      "paymentMethodCard",
      "paymentMethodDeposit",
      "billingProfileValidation",
      "minimumDepositAmount",
    )
  ) {
    // kyc is not surfaced (no KYC integration ships) — the spread keeps any stored value.
    body.autoActivationFlow = {
      ...(cfg.autoActivationFlow ?? {}),
      autoActivationEnabled: form.autoActivationEnabled,
      paymentMethod: form.paymentMethod,
      paymentMethodCard: form.paymentMethodCard,
      paymentMethodDeposit: form.paymentMethodDeposit,
      billingProfileValidation: form.billingProfileValidation,
      minimumDepositAmount: toNum(form.minimumDepositAmount),
    }
  }

  if (cfg.suspensionConfiguration || dirty("suspEnabled", "suspType", "suspendedAtBalance", "suspendedAtDays", "notifications")) {
    body.suspensionConfiguration = {
      ...(cfg.suspensionConfiguration ?? {}),
      enabled: form.suspEnabled,
      type: form.suspType,
      suspendedAt: limit({ balance: form.suspendedAtBalance, days: form.suspendedAtDays }),
      notifications: form.notifications.map(limit).filter((l): l is SuspensionLimit => l !== undefined),
    }
  }

  if (cfg.savingsContractNotificationConfig || dirty("sendExpiryNotification", "reminderDays")) {
    const days = form.reminderDays
      .split(/[,\s]+/)
      .map((s) => parseInt(s, 10))
      .filter((n) => Number.isFinite(n) && n >= 0)
    body.savingsContractNotificationConfig = {
      ...(cfg.savingsContractNotificationConfig ?? {}),
      sendExpiryNotification: form.sendExpiryNotification,
      reminderDaysBeforeExpiry: days,
    }
  }

  if (cfg.provisioningSettings || dirty("promotionals")) {
    body.provisioningSettings = {
      ...(cfg.provisioningSettings ?? {}),
      promotionals: form.promotionals
        .map((p) => ({ amount: toNum(p.amount), daysValidity: toInt(p.daysValidity) }))
        .filter((p) => p.amount !== undefined || p.daysValidity !== undefined),
    }
  }

  return body
}

function Field({ label, children, id }: { label: string; children: React.ReactNode; id?: string }) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  )
}

function ConstraintSelect({ id, value, onChange }: { id?: string; value: string; onChange: (v: string) => void }) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger id={id}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {CONSTRAINTS.map((c) => (
          <SelectItem key={c.value} value={c.value}>
            {c.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

export default function BillingConfigurationPage() {
  const qc = useQueryClient()
  const cfgQ = useAdminGet<BillingConfig>(CURRENT_PATH)
  const currenciesQ = useAdminList<Currency>("/admin/billing/configuration/currencies")
  const countriesQ = useAdminList<Country>("/admin/billing/configuration/countries")
  const integrationsQ = useAdminList<Integration>("/admin/integrations")

  const cfg = cfgQ.data
  const [form, setForm] = useState<Form | null>(null)
  const [initial, setInitial] = useState<Form | null>(null)
  useEffect(() => {
    if (cfg && !form) {
      const f = formFromDoc(cfg)
      setForm(f)
      setInitial(f)
    }
  }, [cfg, form])

  const set = (patch: Partial<Form>) => setForm((f) => (f ? { ...f, ...patch } : f))

  const save = useMutation({
    mutationFn: () => {
      if (!cfg?.id || !form || !initial) throw new Error("Configuration not loaded")
      return apiFetch(`/admin/billing/configuration/${cfg.id}`, {
        method: "PUT",
        body: buildBody(cfg, form, initial),
      })
    },
    onSuccess: () => {
      setForm(null) // re-derive from the fresh read
      setInitial(null)
      void qc.invalidateQueries({ queryKey: ["admin-get", CURRENT_PATH] })
      toast.success("Billing configuration saved")
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const currencies = currenciesQ.data?.data ?? []
  const countries = countriesQ.data?.data ?? []
  const integrations = integrationsQ.data?.data ?? []

  if (cfgQ.isLoading) {
    return (
      <>
        <PageHeader
          title="Billing configuration"
          eyebrow="System"
          description="Business details, activation flow and billing behavior."
        />
        <Skeleton className="h-96" />
      </>
    )
  }

  if (cfgQ.error) {
    return (
      <>
        <PageHeader
          title="Billing configuration"
          eyebrow="System"
          description="Business details, activation flow and billing behavior."
        />
        <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">{(cfgQ.error as Error).message}</div>
      </>
    )
  }

  if (!cfg?.id || !form) {
    return (
      <>
        <PageHeader
          title="Billing configuration"
          eyebrow="System"
          description="Business details, activation flow and billing behavior."
        />
        <p className="text-sm text-muted-foreground">No billing configuration found.</p>
      </>
    )
  }

  const gatewaySelect = (id: string, value: string, onChange: (v: string) => void) => (
    <Select value={value || NONE} onValueChange={(v) => onChange(v === NONE ? "" : v)}>
      <SelectTrigger id={id}>
        <SelectValue placeholder="None" />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={NONE}>None</SelectItem>
        {integrations.map((i) => (
          <SelectItem key={i.id} value={i.id}>
            {i.name || i.thirdParty || i.id}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )

  return (
    <>
      <PageHeader
        title="Billing configuration"
        eyebrow="System"
        description="Business details, activation flow and billing behavior."
        actions={
          <Button onClick={() => save.mutate()} disabled={save.isPending || !form.baseCurrency}>
            {save.isPending ? "Saving…" : "Save configuration"}
          </Button>
        }
      />

      <div className="space-y-6">
        <Alert>
          <TriangleAlert className="size-4" />
          <AlertTitle>Changes apply platform-wide.</AlertTitle>
          <AlertDescription>
            This configuration drives activation, billing and suspension for every billing profile.
            {!form.baseCurrency ? " A base currency is required before saving." : ""}
          </AlertDescription>
        </Alert>

        <Tabs defaultValue="business">
          <div className="-mx-1 overflow-x-auto px-1 pb-1">
            <TabsList className="w-max">
              <TabsTrigger value="business">Business details</TabsTrigger>
              <TabsTrigger value="activation">Activation</TabsTrigger>
              <TabsTrigger value="settings">Settings</TabsTrigger>
            </TabsList>
          </div>

          <TabsContent value="business" className="mt-4">
            <Card>
              <CardContent className="space-y-4 pt-6">
                <div className="grid gap-4 md:grid-cols-2">
                  <Field label="Configuration name" id="bc-name">
                    <Input id="bc-name" value={form.name} onChange={(e) => set({ name: e.target.value })} />
                  </Field>
                  <Field label="Base currency" id="bc-currency">
                    <Select value={form.baseCurrency || NONE} onValueChange={(v) => set({ baseCurrency: v === NONE ? "" : v })}>
                      <SelectTrigger id="bc-currency">
                        <SelectValue placeholder="Select currency" />
                      </SelectTrigger>
                      <SelectContent>
                        {currencies.map((c) => (
                          <SelectItem key={c.currency_code} value={c.currency_code}>
                            {c.currency_code}
                            {c.currency_name ? ` — ${c.currency_name}` : ""}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </Field>
                  <Field label="Business name" id="bc-business">
                    <Input id="bc-business" value={form.businessName} onChange={(e) => set({ businessName: e.target.value })} />
                  </Field>
                  <Field label="VAT id" id="bc-vat">
                    <Input id="bc-vat" value={form.vatId} onChange={(e) => set({ vatId: e.target.value })} />
                  </Field>
                  <Field label="Country" id="bc-country">
                    <Select value={form.country || NONE} onValueChange={(v) => set({ country: v === NONE ? "" : v })}>
                      <SelectTrigger id="bc-country">
                        <SelectValue placeholder="None" />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value={NONE}>None</SelectItem>
                        {countries.map((c) => (
                          <SelectItem key={c.cca2} value={c.cca2}>
                            {c.name} ({c.cca2})
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </Field>
                  <Field label="City" id="bc-city">
                    <Input id="bc-city" value={form.city} onChange={(e) => set({ city: e.target.value })} />
                  </Field>
                  <Field label="Address" id="bc-street">
                    <Input id="bc-street" value={form.street} onChange={(e) => set({ street: e.target.value })} />
                  </Field>
                </div>

                <div className="grid gap-4 md:grid-cols-2">
                  <Field label="Invoice gateway" id="bc-invoice-gw">{gatewaySelect("bc-invoice-gw", form.invoiceGatewayId, (v) => set({ invoiceGatewayId: v }))}</Field>
                  <Field label="Mail gateway" id="bc-mail-gw">{gatewaySelect("bc-mail-gw", form.mailGatewayId, (v) => set({ mailGatewayId: v }))}</Field>
                </div>

                <div className="flex items-center gap-2 rounded-lg border p-4">
                  <Switch id="bc-default" checked={form.defaultConfiguration} onCheckedChange={(v) => set({ defaultConfiguration: v })} />
                  <Label htmlFor="bc-default" className="text-sm font-normal">Default configuration</Label>
                </div>
              </CardContent>
            </Card>
          </TabsContent>

          <TabsContent value="activation" className="mt-4">
            <Card>
              <CardContent className="space-y-6 pt-6">
                <div className="flex items-center gap-2 rounded-lg border p-4">
                  <Switch id="bc-autoact" checked={form.autoActivationEnabled} onCheckedChange={(v) => set({ autoActivationEnabled: v })} />
                  <Label htmlFor="bc-autoact" className="text-sm font-normal">Auto-activation enabled</Label>
                </div>

                <div className="grid gap-4 md:grid-cols-2">
                  <Field label="Payment method" id="bc-pm">
                    <ConstraintSelect id="bc-pm" value={form.paymentMethod} onChange={(v) => set({ paymentMethod: v })} />
                  </Field>
                  <Field label="Payment method — card" id="bc-pm-card">
                    <ConstraintSelect id="bc-pm-card" value={form.paymentMethodCard} onChange={(v) => set({ paymentMethodCard: v })} />
                  </Field>
                  <Field label="Payment method — deposit" id="bc-pm-dep">
                    <ConstraintSelect id="bc-pm-dep" value={form.paymentMethodDeposit} onChange={(v) => set({ paymentMethodDeposit: v })} />
                  </Field>
                  <Field label="Billing profile validation" id="bc-bp-val">
                    <ConstraintSelect id="bc-bp-val" value={form.billingProfileValidation} onChange={(v) => set({ billingProfileValidation: v })} />
                  </Field>
                  <Field label="Minimum deposit amount" id="bc-mindep">
                    <Input
                      id="bc-mindep"
                      type="number"
                      min="0"
                      value={form.minimumDepositAmount}
                      onChange={(e) => set({ minimumDepositAmount: e.target.value })}
                    />
                  </Field>
                </div>

                <div className="space-y-2">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="text-eyebrow">Provisioning promotional credits</div>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => set({ promotionals: [...form.promotionals, { amount: "", daysValidity: "" }] })}
                    >
                      <Plus className="size-4" /> Add credit
                    </Button>
                  </div>
                  {!form.promotionals.length ? (
                    <p className="text-sm text-muted-foreground">No promotional credits granted at provisioning.</p>
                  ) : (
                    form.promotionals.map((p, i) => (
                      <div key={i} className="flex flex-wrap items-end gap-3">
                        <Field label="Amount">
                          <Input
                            type="number"
                            min="0"
                            className="w-32"
                            value={p.amount}
                            onChange={(e) =>
                              set({ promotionals: form.promotionals.map((r, j) => (j === i ? { ...r, amount: e.target.value } : r)) })
                            }
                          />
                        </Field>
                        <Field label="Days valid">
                          <Input
                            type="number"
                            min="0"
                            className="w-32"
                            value={p.daysValidity}
                            onChange={(e) =>
                              set({
                                promotionals: form.promotionals.map((r, j) => (j === i ? { ...r, daysValidity: e.target.value } : r)),
                              })
                            }
                          />
                        </Field>
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label="Remove promotional credit"
                          onClick={() => set({ promotionals: form.promotionals.filter((_, j) => j !== i) })}
                        >
                          <Trash2 className="size-4 text-muted-foreground" />
                        </Button>
                      </div>
                    ))
                  )}
                </div>
              </CardContent>
            </Card>
          </TabsContent>

          <TabsContent value="settings" className="mt-4">
            <Card>
              <CardContent className="space-y-6 pt-6">
                <div className="flex items-center gap-2 rounded-lg border p-4">
                  <Switch id="bc-promo" checked={form.promotionCodesEnabled} onCheckedChange={(v) => set({ promotionCodesEnabled: v })} />
                  <Label htmlFor="bc-promo" className="text-sm font-normal">Promotion codes enabled</Label>
                </div>

                <div className="space-y-4 rounded-lg border p-4">
                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <div className="flex items-center gap-2">
                      <Switch id="bc-susp" checked={form.suspEnabled} onCheckedChange={(v) => set({ suspEnabled: v })} />
                      <Label htmlFor="bc-susp" className="text-sm">Automatic suspension</Label>
                    </div>
                    <Select value={form.suspType} onValueChange={(v) => set({ suspType: v })}>
                      <SelectTrigger className="w-36" aria-label="Suspension type">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="BALANCE">Balance</SelectItem>
                        <SelectItem value="DUE_DATE">Due date</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    BALANCE limits use the balance field; DUE_DATE limits use days overdue.
                  </p>
                  <div className="flex flex-wrap items-end gap-3">
                    <Field label="Suspend at — balance">
                      <Input
                        type="number"
                        className="w-32"
                        value={form.suspendedAtBalance}
                        onChange={(e) => set({ suspendedAtBalance: e.target.value })}
                      />
                    </Field>
                    <Field label="Suspend at — days">
                      <Input
                        type="number"
                        className="w-32"
                        value={form.suspendedAtDays}
                        onChange={(e) => set({ suspendedAtDays: e.target.value })}
                      />
                    </Field>
                  </div>
                  <div className="space-y-2">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <div className="text-eyebrow">Notification limits</div>
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => set({ notifications: [...form.notifications, { balance: "", days: "" }] })}
                      >
                        <Plus className="size-4" /> Add limit
                      </Button>
                    </div>
                    {!form.notifications.length ? (
                      <p className="text-sm text-muted-foreground">No pre-suspension notifications configured.</p>
                    ) : (
                      form.notifications.map((n, i) => (
                        <div key={i} className="flex flex-wrap items-end gap-3">
                          <Field label="Balance">
                            <Input
                              type="number"
                              className="w-32"
                              value={n.balance}
                              onChange={(e) =>
                                set({ notifications: form.notifications.map((r, j) => (j === i ? { ...r, balance: e.target.value } : r)) })
                              }
                            />
                          </Field>
                          <Field label="Days">
                            <Input
                              type="number"
                              className="w-32"
                              value={n.days}
                              onChange={(e) =>
                                set({ notifications: form.notifications.map((r, j) => (j === i ? { ...r, days: e.target.value } : r)) })
                              }
                            />
                          </Field>
                          <Button
                            variant="ghost"
                            size="icon"
                            aria-label="Remove notification limit"
                            onClick={() => set({ notifications: form.notifications.filter((_, j) => j !== i) })}
                          >
                            <Trash2 className="size-4 text-muted-foreground" />
                          </Button>
                        </div>
                      ))
                    )}
                  </div>
                </div>

                <div className="space-y-3 rounded-lg border p-4">
                  <div className="text-eyebrow">Time unit limits</div>
                  <p className="text-xs text-muted-foreground">Units per month used when rating (defaults: minute 43200, hour 720, month 1).</p>
                  <div className="flex flex-wrap gap-3">
                    <Field label="Minute">
                      <Input type="number" className="w-32" value={form.tuMinute} onChange={(e) => set({ tuMinute: e.target.value })} />
                    </Field>
                    <Field label="Hour">
                      <Input type="number" className="w-32" value={form.tuHour} onChange={(e) => set({ tuHour: e.target.value })} />
                    </Field>
                    <Field label="Month">
                      <Input type="number" className="w-32" value={form.tuMonth} onChange={(e) => set({ tuMonth: e.target.value })} />
                    </Field>
                  </div>
                </div>

                <div className="space-y-3 rounded-lg border p-4">
                  <div className="flex items-center gap-2">
                    <Switch id="bc-expiry" checked={form.sendExpiryNotification} onCheckedChange={(v) => set({ sendExpiryNotification: v })} />
                    <Label htmlFor="bc-expiry" className="text-sm">Savings contract expiry notifications</Label>
                  </div>
                  <Field label="Reminder days before expiry (comma separated)" id="bc-reminders">
                    <Input
                      id="bc-reminders"
                      placeholder="30, 14, 7"
                      value={form.reminderDays}
                      onChange={(e) => set({ reminderDays: e.target.value })}
                    />
                  </Field>
                </div>
              </CardContent>
            </Card>
          </TabsContent>
        </Tabs>
      </div>
    </>
  )
}
