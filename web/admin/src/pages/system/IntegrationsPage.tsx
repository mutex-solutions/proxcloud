import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { Activity, MoreHorizontal, Pencil, Plug, Settings2, Trash2 } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { fmtDate } from "@/lib/format"
import { useAdminList } from "@/lib/hooks"

// The Go create/update body is { thirdParty, name?, config{}, secret{} } — see
// internal/platform/admin/thirdpartyintegration.go. `config`/`secret` are free-form objects; the
// secret is write-only (never returned) and is replaced only when every field is non-blank
// (isNeededToUpdateSecret). Field keys per vendor come from each vendor's config/secret schema.

type CatalogEntry = { name: string; categories: string[]; installed: boolean }
type Integration = {
  id: string
  name?: string
  description?: string
  thirdParty?: string
  config?: Record<string, unknown>
  createdAt?: string
}

const LIST_PATH = "/admin/integrations"
const STATS_PATH = "/admin/integrations/stats"

type FieldType = "text" | "number" | "password" | "textarea" | "switch"
type Field = { key: string; label: string; type: FieldType; placeholder?: string }
type VendorSchema = { config: Field[]; secret: Field[] }

// Per-vendor field schemas (keys match the Go create/update reads exactly). Any vendor without an
// entry falls back to raw config/secret JSON textareas so all 4 catalog entries are configurable.
const SCHEMAS: Record<string, VendorSchema> = {
  Stripe: {
    config: [
      { key: "publicKey", label: "Publishable key", type: "text", placeholder: "pk_…" },
      { key: "minDeposit", label: "Minimum deposit", type: "number", placeholder: "10" },
      { key: "sandbox", label: "Sandbox mode", type: "switch" },
      { key: "scan", label: "Scan for anomalies", type: "switch" },
      { key: "callback", label: "Use callback flow", type: "switch" },
    ],
    secret: [{ key: "privateKey", label: "Secret key", type: "password", placeholder: "sk_…" }],
  },
  SMTP: {
    config: [
      { key: "domain", label: "SMTP host", type: "text", placeholder: "smtp.example.com" },
      { key: "port", label: "Port", type: "number", placeholder: "587" },
      { key: "username", label: "Username", type: "text" },
      { key: "fromName", label: "From name", type: "text" },
      { key: "fromEmail", label: "From email", type: "text", placeholder: "no-reply@example.com" },
      { key: "starttls", label: "Use STARTTLS", type: "switch" },
      { key: "noAuth", label: "No authentication", type: "switch" },
    ],
    secret: [{ key: "password", label: "Password", type: "password" }],
  },
  BankTransfer: {
    config: [
      { key: "minDeposit", label: "Minimum deposit", type: "number" },
      { key: "bankTransferInstructions", label: "Instructions / account details", type: "textarea" },
    ],
    secret: [],
  },
}

// Category filter tabs → catalog category keys.
const CATS = [
  { label: "Payment", key: "Payment" },
  { label: "Invoice", key: "Invoice" },
  { label: "Mail", key: "Mail" },
] as const

type EditTarget = { vendor: string; installed: Integration | null }

export default function IntegrationsPage() {
  const qc = useQueryClient()
  const statsQ = useAdminList<CatalogEntry>(STATS_PATH)
  const listQ = useAdminList<Integration>(LIST_PATH)

  // The backend catalog carries intentional duplicates (Stripe is registered by both the
  // invoice and payment factories) — dedupe by name for display, merging categories.
  // Memoized so the DataTable columns (which look up categories) stay referentially stable.
  const catalog = useMemo(() => {
    const list: CatalogEntry[] = []
    for (const e of statsQ.data?.data ?? []) {
      const seen = list.find((c) => c.name === e.name)
      if (seen) {
        for (const c of e.categories ?? []) if (!seen.categories.includes(c)) seen.categories.push(c)
      } else {
        list.push({ name: e.name, categories: [...(e.categories ?? [])], installed: e.installed })
      }
    }
    return list
  }, [statsQ.data])

  const installed = listQ.data?.data ?? []
  const installedByVendor: Record<string, Integration> = {}
  for (const i of installed) if (i.thirdParty && !installedByVendor[i.thirdParty]) installedByVendor[i.thirdParty] = i
  const categoriesOf = (thirdParty?: string) =>
    catalog.find((c) => c.name === thirdParty)?.categories ?? []

  const [tab, setTab] = useState<string>("All")

  const [editing, setEditing] = useState<EditTarget | null>(null)
  const [deleting, setDeleting] = useState<Integration | null>(null)

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["admin-list", LIST_PATH] })
    qc.invalidateQueries({ queryKey: ["admin-list", STATS_PATH] })
  }

  const remove = useMutation({
    mutationFn: (id: string) => apiFetch(`${LIST_PATH}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      setDeleting(null)
      invalidate()
      toast.success("Integration removed")
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const health = useMutation({
    mutationFn: (id: string) => apiFetch(`${LIST_PATH}/healthcheck/${id}`, { method: "POST" }),
    onSuccess: () => toast.success("Health check passed"),
    onError: (e) => {
      const msg = (e as Error).message || ""
      toast.message(/implement|seam|501/i.test(msg) ? "Health check not available for this provider" : `Health check failed: ${msg}`)
    },
  })

  const columns = useMemo<ColumnDef<Integration, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (i) => i.name ?? "",
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue() || "—"}</span>,
      },
      {
        id: "provider",
        accessorFn: (i) => i.thirdParty ?? "",
        header: sortableHeader("Provider"),
        cell: ({ getValue }) => <span className="text-sm">{getValue() || "—"}</span>,
      },
      {
        id: "category",
        accessorFn: (i) => categoriesOf(i.thirdParty).join(" "),
        header: "Category",
        enableSorting: false,
        cell: ({ row }) => (
          <div className="flex flex-wrap gap-1">
            {categoriesOf(row.original.thirdParty).map((c) => (
              <Badge key={c} variant="outline">{c}</Badge>
            ))}
          </div>
        ),
      },
      {
        id: "created",
        accessorFn: (i) => i.createdAt ?? "",
        header: sortableHeader("Created"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{fmtDate(getValue())}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const i = row.original
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${i.name ?? i.thirdParty}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => health.mutate(i.id)} disabled={health.isPending}>
                    <Activity className="size-4" /> Run health check
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => setEditing({ vendor: i.thirdParty ?? i.name ?? "", installed: i })}>
                    <Pencil className="size-4" /> Edit
                  </DropdownMenuItem>
                  <DropdownMenuItem variant="destructive" onClick={() => setDeleting(i)}>
                    <Trash2 className="size-4" /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // categoriesOf closes over the memoized catalog; setters are stable.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [catalog, health.isPending],
  )

  return (
    <>
      <PageHeader
        title="Integrations"
        eyebrow="System"
        description="Payment, invoicing and mail providers. Click a provider to configure it."
      />

      {/* Installed */}
      <h2 className="text-eyebrow mb-3">Installed</h2>
      {!listQ.isLoading && !listQ.error && installed.length === 0 ? (
        <EmptyState icon={Plug} title="No integrations installed" hint="Pick a provider from the catalog below to configure it." />
      ) : (
        <DataTable
          columns={columns}
          data={installed}
          isLoading={listQ.isLoading}
          error={listQ.error as Error | null}
          getRowId={(i) => i.id}
        />
      )}

      {/* Catalog — real tab panels (a filter-only TabsList leaves triggers pointing
          at panels that don't exist, an axe aria-valid-attr-value violation). */}
      <Tabs value={tab} onValueChange={setTab}>
        <div className="mb-3 mt-8 flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-eyebrow">Catalog</h2>
          <div className="-mx-1 overflow-x-auto px-1 pb-1">
            <TabsList className="w-max">
              <TabsTrigger value="All">All</TabsTrigger>
              {CATS.map((c) => (
                <TabsTrigger key={c.key} value={c.label}>{c.label}</TabsTrigger>
              ))}
            </TabsList>
          </div>
        </div>

        {statsQ.isLoading ? (
          <Skeleton className="h-40" />
        ) : statsQ.error ? (
          <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">{(statsQ.error as Error).message}</div>
        ) : (
          ["All", ...CATS.map((c) => c.label)].map((label) => (
            <TabsContent key={label} value={label} className="space-y-6">
              {(label === "All" ? CATS : CATS.filter((c) => c.label === label)).map((cat) => {
                const vendors = catalog.filter((v) => v.categories.includes(cat.key))
                if (vendors.length === 0) return null
                return (
                  <section key={cat.key}>
                    <h3 className="text-eyebrow mb-2">{cat.label}</h3>
                    <div className="grid gap-3 md:grid-cols-3 lg:grid-cols-4">
                      {vendors.map((v) => {
                        const inst = installedByVendor[v.name]
                        const isInstalled = !!inst || v.installed
                        return (
                          <Card
                            key={cat.key + v.name}
                            role="button"
                            tabIndex={0}
                            onClick={() => setEditing({ vendor: v.name, installed: inst ?? null })}
                            onKeyDown={(e) => {
                              if (e.key === "Enter" || e.key === " ") { e.preventDefault(); setEditing({ vendor: v.name, installed: inst ?? null }) }
                            }}
                            className="cursor-pointer transition-colors hover:border-primary/50 hover:bg-muted/40"
                          >
                            <CardContent className="p-4">
                              <div className="flex items-start justify-between gap-2">
                                <p className="font-medium">{v.name}</p>
                                {isInstalled ? <Badge variant="secondary">Installed</Badge> : <Badge variant="outline">Available</Badge>}
                              </div>
                              <div className="mt-2 flex flex-wrap gap-1">
                                {v.categories.map((c) => (
                                  <Badge key={c} variant="secondary">{c}</Badge>
                                ))}
                              </div>
                              <div className="mt-3 flex items-center gap-1.5 text-xs text-muted-foreground">
                                <Settings2 className="size-3.5" /> {isInstalled ? "Configure" : "Set up"}
                              </div>
                            </CardContent>
                          </Card>
                        )
                      })}
                    </div>
                  </section>
                )
              })}
            </TabsContent>
          ))
        )}
      </Tabs>

      {editing && (
        <ConfigDialog
          key={editing.vendor + (editing.installed?.id ?? "new")}
          target={editing}
          onClose={() => setEditing(null)}
          onSaved={() => { setEditing(null); invalidate() }}
        />
      )}

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Remove integration</DialogTitle>
            <DialogDescription>
              Remove “{deleting?.name}” ({deleting?.thirdParty})? Flows depending on it will stop working.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.id)} disabled={remove.isPending}>
              {remove.isPending ? "Removing…" : "Remove"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

function ConfigDialog({ target, onClose, onSaved }: { target: EditTarget; onClose: () => void; onSaved: () => void }) {
  const { vendor, installed } = target
  const schema = SCHEMAS[vendor]
  const cfg = (installed?.config ?? {}) as Record<string, unknown>

  // config field state (strings for text/number, booleans for switches); secrets are always blank
  // (write-only — existing values are never returned and only replaced when re-entered).
  const [config, setConfig] = useState<Record<string, string | boolean>>(() => {
    const s: Record<string, string | boolean> = {}
    for (const f of schema?.config ?? []) {
      s[f.key] = f.type === "switch" ? Boolean(cfg[f.key]) : (cfg[f.key] != null ? String(cfg[f.key]) : "")
    }
    return s
  })
  const [secret, setSecret] = useState<Record<string, string>>(() => {
    const s: Record<string, string> = {}
    for (const f of schema?.secret ?? []) s[f.key] = ""
    return s
  })
  // generic fallback (no schema): raw config/secret JSON
  const [rawConfig, setRawConfig] = useState<string>(() =>
    installed?.config ? JSON.stringify(installed.config, null, 2) : "")
  const [rawSecret, setRawSecret] = useState<string>("")

  const setCfg = (k: string, v: string | boolean) => setConfig((p) => ({ ...p, [k]: v }))
  const setSec = (k: string, v: string) => setSecret((p) => ({ ...p, [k]: v }))

  const save = useMutation({
    mutationFn: () => {
      let configOut: Record<string, unknown> = {}
      let secretOut: Record<string, unknown> = {}

      if (schema) {
        for (const f of schema.config) {
          const v = config[f.key]
          if (f.type === "switch") configOut[f.key] = Boolean(v)
          else if (f.type === "number") { if (String(v).trim() !== "") configOut[f.key] = Number(v) }
          else if (String(v ?? "").trim() !== "") configOut[f.key] = v
        }
        for (const f of schema.secret) {
          const v = String(secret[f.key] ?? "").trim()
          if (v !== "") secretOut[f.key] = v // only send secret fields when non-blank
        }
      } else {
        if (rawConfig.trim()) {
          try { configOut = JSON.parse(rawConfig) } catch { throw new Error("Config is not valid JSON") }
        }
        if (rawSecret.trim()) {
          try { secretOut = JSON.parse(rawSecret) } catch { throw new Error("Secret is not valid JSON") }
        }
      }

      const body: Record<string, unknown> = {
        thirdParty: vendor,
        name: installed?.name ?? vendor,
        config: configOut,
      }
      if (Object.keys(secretOut).length) body.secret = secretOut

      return installed
        ? apiFetch(`${LIST_PATH}/${installed.id}`, { method: "PUT", body })
        : apiFetch(LIST_PATH, { method: "POST", body })
    },
    onSuccess: () => {
      toast.success(installed ? `${vendor} updated` : `${vendor} configured`)
      onSaved()
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{installed ? `Configure ${vendor}` : `Set up ${vendor}`}</DialogTitle>
          <DialogDescription>
            {schema
              ? "Secrets are stored encrypted and never shown again — leave them blank to keep the current value."
              : "No preset form for this provider — enter its config and secret as JSON. Secrets are write-only."}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3">
          {schema ? (
            <>
              {schema.config.map((f) => (
                <FieldInput key={f.key} field={f} value={config[f.key]} onChange={(v) => setCfg(f.key, v)} />
              ))}
              {schema.secret.map((f) => (
                <div key={f.key} className="space-y-1.5">
                  <Label htmlFor={`sec-${f.key}`}>{f.label}</Label>
                  <Input
                    id={`sec-${f.key}`}
                    type="password"
                    className="font-mono"
                    placeholder={installed ? "•••••• (unchanged)" : f.placeholder}
                    value={secret[f.key] ?? ""}
                    onChange={(e) => setSec(f.key, e.target.value)}
                  />
                </div>
              ))}
            </>
          ) : (
            <>
              <div className="space-y-1.5">
                <Label htmlFor="raw-config">Config (JSON)</Label>
                <Textarea
                  id="raw-config"
                  className="min-h-28 font-mono text-xs"
                  placeholder={'{\n  "key": "value"\n}'}
                  value={rawConfig}
                  onChange={(e) => setRawConfig(e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="raw-secret">Secret (JSON)</Label>
                <Textarea
                  id="raw-secret"
                  className="min-h-20 font-mono text-xs"
                  placeholder={installed ? "leave blank to keep current" : '{\n  "apiKey": "…"\n}'}
                  value={rawSecret}
                  onChange={(e) => setRawSecret(e.target.value)}
                />
              </div>
            </>
          )}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button onClick={() => save.mutate()} disabled={save.isPending}>
            {save.isPending ? "Saving…" : installed ? "Save changes" : "Install"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function FieldInput({ field, value, onChange }: { field: Field; value: string | boolean; onChange: (v: string | boolean) => void }) {
  if (field.type === "switch") {
    return (
      <div className="flex items-center justify-between rounded-md border px-3 py-2">
        <Label htmlFor={`cfg-${field.key}`} className="cursor-pointer">{field.label}</Label>
        <Switch id={`cfg-${field.key}`} checked={Boolean(value)} onCheckedChange={(v) => onChange(v)} />
      </div>
    )
  }
  if (field.type === "textarea") {
    return (
      <div className="space-y-1.5">
        <Label htmlFor={`cfg-${field.key}`}>{field.label}</Label>
        <Textarea
          id={`cfg-${field.key}`}
          className="min-h-24"
          placeholder={field.placeholder}
          value={String(value ?? "")}
          onChange={(e) => onChange(e.target.value)}
        />
      </div>
    )
  }
  return (
    <div className="space-y-1.5">
      <Label htmlFor={`cfg-${field.key}`}>{field.label}</Label>
      <Input
        id={`cfg-${field.key}`}
        type={field.type === "number" ? "number" : "text"}
        className={field.type === "text" ? "font-mono" : undefined}
        placeholder={field.placeholder}
        value={String(value ?? "")}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  )
}
