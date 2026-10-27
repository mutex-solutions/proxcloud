import { useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import {
  CheckCircle2, Download, FolderKanban, Gauge, Gift, Layers, PauseCircle, PlayCircle, Plus, Receipt,
  RefreshCw, ShieldCheck, Trash2, Undo2, Wallet, XCircle,
} from "lucide-react"
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
import { Card, CardContent } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { useAdminList, useTabParam } from "@/lib/hooks"
import { fmtDate, fmtDateTime, fmtMoney, timeAgo } from "@/lib/format"
import { cn } from "@/lib/utils"

// GET /admin/billing-profile/{id} → BillingSummary (profile + computed financials).
// The summary DROPS a few raw-doc fields (taxConfiguration / projectProvisioningQuota / bank / iban /
// identityValidationId) — those come from the raw doc via GET /admin/billing-profile/search.
type Summary = Record<string, any>
type Doc = Record<string, any>

function Row({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4 border-b border-dashed py-1.5 last:border-0">
      <dt className="text-muted-foreground">{k}</dt>
      <dd className="text-right">{v}</dd>
    </div>
  )
}

// Stream a receipt PDF: read the blob, name it from Content-Disposition, click a temp <a download>.
async function downloadResponse(resp: Response, fallback: string) {
  const blob = await resp.blob()
  const cd = resp.headers.get("content-disposition")
  const m = cd && (/filename\*=(?:UTF-8'')?"?([^";]+)"?/i.exec(cd) || /filename="?([^";]+)"?/i.exec(cd))
  const filename = m ? decodeURIComponent(m[1]) : fallback
  const url = URL.createObjectURL(blob)
  const a = document.createElement("a")
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

function ErrorPanel({ error }: { error: unknown }) {
  return (
    <div className="rounded-lg border bg-muted/40 p-4 text-sm text-muted-foreground">
      {(error as Error)?.message ?? "Something went wrong."}
    </div>
  )
}

/** Money stat: muted when zero (nothing to act on), destructive when negative (debt). */
function Stat({ label, amount, currency }: { label: string; amount: unknown; currency?: string }) {
  const n = Number(amount ?? 0)
  return (
    <Card className="gap-1 py-5">
      <p className="px-5 text-sm font-medium text-muted-foreground">{label}</p>
      <p
        className={cn(
          "px-5 font-mono text-xl font-semibold tabular-nums",
          n === 0 && "text-muted-foreground",
          n < 0 && "text-destructive-text",
        )}
      >
        {fmtMoney(n, currency)}
      </p>
    </Card>
  )
}

// ─── Status actions (POST /admin/billing-profile/{id}/action/{ACTIVE|SUSPENDED}) ─────────────
// Supported transitions (billingProfileUpdateStatus): NEW→ACTIVE, ACTIVE→SUSPENDED, SUSPENDED→ACTIVE.

type PendingAction = { target: "ACTIVE" | "SUSPENDED"; verb: string } | null

// ─── Page ─────────────────────────────────────────────────────────────────────────────────────

export default function BillingProfileDetailPage() {
  const { id = "" } = useParams()
  const qc = useQueryClient()
  const [tab, setTab] = useTabParam("dashboard")
  const [pending, setPending] = useState<PendingAction>(null)

  const { data: bp, isLoading, isError, error } = useQuery({
    queryKey: ["admin-bp", id],
    queryFn: () => apiFetch<Summary>(`/admin/billing-profile/${id}`),
    enabled: !!id,
  })

  // Raw profile doc (summary drops taxConfiguration/quota/bank/iban/identityValidationId).
  // GET /admin/billing-profile/search does an exact-match filter on raw fields — scope by
  // organizationId (else email) and pick our row. ponytail: no by-id raw endpoint exists.
  const rawQ = useQuery({
    queryKey: ["admin-bp-raw", id],
    enabled: !!bp && !!(bp.organizationId || bp.email),
    queryFn: async () => {
      const param = bp!.organizationId
        ? `organizationId=${encodeURIComponent(bp!.organizationId)}`
        : `email=${encodeURIComponent(bp!.email)}`
      const rows = await apiFetch<Doc[]>(`/admin/billing-profile/search?${param}`)
      return rows.find((r) => (r.id ?? r._id) === id) ?? null
    },
  })
  const raw = rawQ.data ?? null

  const act = useMutation({
    mutationFn: (target: string) =>
      apiFetch(`/admin/billing-profile/${id}/action/${target}`, { method: "POST" }),
    onSuccess: (_d, target) => {
      toast.success(`Profile is now ${target}`)
      void qc.invalidateQueries({ queryKey: ["admin-bp", id] })
      void qc.invalidateQueries({ queryKey: ["admin-bp-raw", id] })
      void qc.invalidateQueries({ queryKey: ["admin-list", "/admin/billing-profile"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const crumbs = (crumbLabel: string) => (
    <Breadcrumb>
      <BreadcrumbList>
        <BreadcrumbItem>
          <BreadcrumbLink asChild>
            <Link to="/clients/billing-profiles">Billing profiles</Link>
          </BreadcrumbLink>
        </BreadcrumbItem>
        <BreadcrumbSeparator />
        <BreadcrumbItem>
          <BreadcrumbPage>{crumbLabel}</BreadcrumbPage>
        </BreadcrumbItem>
      </BreadcrumbList>
    </Breadcrumb>
  )

  if (isLoading) {
    return (
      <>
        <PageHeader title="Billing profile" eyebrow="Clients" breadcrumb={crumbs(id)} />
        <Skeleton className="h-72" />
      </>
    )
  }
  if (isError || !bp) {
    return (
      <>
        <PageHeader title="Billing profile" eyebrow="Clients" breadcrumb={crumbs(id)} />
        <ErrorPanel error={error ?? new Error("Billing profile not found")} />
      </>
    )
  }

  const name =
    bp.fullName || [bp.firstName, bp.lastName].filter(Boolean).join(" ") || bp.companyName || bp.email || id
  const status = (bp.status as string) ?? ""
  const subtitle = [bp.email, bp.companyName && bp.companyName !== name ? bp.companyName : null]
    .filter(Boolean)
    .join(" · ")

  return (
    <>
      <PageHeader
        title={name}
        eyebrow="Clients"
        breadcrumb={crumbs(name)}
        description={subtitle}
        actions={
          <>
            {status === "NEW" && (
              <Button size="sm" onClick={() => setPending({ target: "ACTIVE", verb: "activate" })}>
                <CheckCircle2 className="size-4" /> Activate
              </Button>
            )}
            {status === "ACTIVE" && (
              <Button
                variant="outline"
                size="sm"
                onClick={() => setPending({ target: "SUSPENDED", verb: "suspend" })}
              >
                <PauseCircle className="size-4" /> Suspend
              </Button>
            )}
            {status === "SUSPENDED" && (
              <Button size="sm" onClick={() => setPending({ target: "ACTIVE", verb: "resume" })}>
                <PlayCircle className="size-4" /> Resume
              </Button>
            )}
          </>
        }
      />

      <div className="mb-4 flex items-center gap-3">
        <StatusBadge status={status} />
        <span className="font-mono text-xs text-muted-foreground">{id}</span>
      </div>

      <div className="mb-6 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Stat label="Balance" amount={bp.balance} currency={bp.currency} />
        <Stat label="Account credit" amount={bp.accountCredit} currency={bp.currency} />
        <Stat label="Promotional credit" amount={bp.promotionalCredit} currency={bp.currency} />
        <Stat label="This month usage" amount={bp.currentMonthUsage} currency={bp.currency} />
      </div>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList className="h-auto flex-wrap">
          <TabsTrigger value="dashboard">Dashboard</TabsTrigger>
          <TabsTrigger value="address">Address</TabsTrigger>
          <TabsTrigger value="projects">Projects</TabsTrigger>
          <TabsTrigger value="credits">Account credits</TabsTrigger>
          <TabsTrigger value="suspend">Suspend</TabsTrigger>
          <TabsTrigger value="bills">Bills</TabsTrigger>
          <TabsTrigger value="priceplans">Price plans</TabsTrigger>
          <TabsTrigger value="promo">Promotional credits</TabsTrigger>
          <TabsTrigger value="tax">Tax</TabsTrigger>
          <TabsTrigger value="transactions">Transactions</TabsTrigger>
          <TabsTrigger value="validation">Validation</TabsTrigger>
          <TabsTrigger value="quota">Quota</TabsTrigger>
        </TabsList>

        <TabsContent value="dashboard" className="mt-4">
          <DashboardTab bpId={id} currency={bp.currency} />
        </TabsContent>
        <TabsContent value="address" className="mt-4">
          {rawQ.isLoading ? (
            <Skeleton className="h-64" />
          ) : (
            <AddressTab key={raw ? "raw" : "summary"} bpId={id} bp={bp} raw={raw} />
          )}
        </TabsContent>
        <TabsContent value="projects" className="mt-4">
          <ProjectsTab bpId={id} />
        </TabsContent>
        <TabsContent value="credits" className="mt-4">
          <CreditsTab bpId={id} currency={bp.currency} />
        </TabsContent>
        <TabsContent value="suspend" className="mt-4">
          <SuspendTab bpId={id} status={status} raw={raw} rawLoading={rawQ.isLoading} />
        </TabsContent>
        <TabsContent value="bills" className="mt-4">
          <BillsTab bpId={id} />
        </TabsContent>
        <TabsContent value="priceplans" className="mt-4">
          <PricePlansTab bpId={id} bp={bp} />
        </TabsContent>
        <TabsContent value="promo" className="mt-4">
          <PromoCreditsTab bpId={id} currency={bp.currency} />
        </TabsContent>
        <TabsContent value="tax" className="mt-4">
          {rawQ.isLoading ? <Skeleton className="h-48" /> : <TaxTab key={raw ? "raw" : "summary"} bpId={id} raw={raw} />}
        </TabsContent>
        <TabsContent value="transactions" className="mt-4">
          <TransactionsTab bpId={id} />
        </TabsContent>
        <TabsContent value="validation" className="mt-4">
          <ValidationTab bpId={id} bp={bp} />
        </TabsContent>
        <TabsContent value="quota" className="mt-4">
          {rawQ.isLoading ? <Skeleton className="h-48" /> : <QuotaTab key={raw ? "raw" : "summary"} bpId={id} raw={raw} />}
        </TabsContent>
      </Tabs>

      <Dialog open={!!pending} onOpenChange={(o) => !o && setPending(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {pending ? `${pending.verb.charAt(0).toUpperCase() + pending.verb.slice(1)} billing profile` : ""}
            </DialogTitle>
            <DialogDescription>
              {pending?.target === "SUSPENDED"
                ? `Suspends ${name} — its projects are disabled and running servers paused until it is resumed.`
                : pending?.verb === "resume"
                  ? `Resumes ${name} — its projects are re-enabled and paused servers restarted.`
                  : `Activates ${name} — the profile can accrue usage and be billed.`}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPending(null)}>
              Cancel
            </Button>
            <Button
              variant={pending?.target === "SUSPENDED" ? "destructive" : "default"}
              disabled={act.isPending}
              onClick={() => {
                if (pending) act.mutate(pending.target)
                setPending(null)
              }}
            >
              {pending ? `${pending.verb.charAt(0).toUpperCase() + pending.verb.slice(1)} profile` : "Confirm"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// ─── Dashboard — GET /admin/billing-profile/{id}/cost-info + GET /admin/billing-profile/financial/{id} ─

function DashboardTab({ bpId, currency }: { bpId: string; currency?: string }) {
  const costQ = useQuery({
    queryKey: ["admin-bp-costinfo", bpId],
    queryFn: () => apiFetch<Doc>(`/admin/billing-profile/${bpId}/cost-info`),
  })
  const finQ = useQuery({
    queryKey: ["admin-bp-financial", bpId],
    queryFn: () => apiFetch<Doc>(`/admin/billing-profile/financial/${bpId}`),
  })

  if (costQ.isLoading || finQ.isLoading) return <Skeleton className="h-64" />
  if (costQ.isError) return <ErrorPanel error={costQ.error} />
  if (finQ.isError) return <ErrorPanel error={finQ.error} />

  const cost = costQ.data ?? {}
  const fin = finQ.data ?? {}
  const info = (cost.billingProfileCostInfo ?? {}) as Doc
  const byType = (info.currentMonthCostsByType ?? {}) as Record<string, any>
  const lastByType = (info.lastMonthCostsByType ?? {}) as Record<string, any>
  const top = (info.topResourcePrices ?? []) as Doc[]
  const typeKeys = Array.from(new Set([...Object.keys(byType), ...Object.keys(lastByType)])).sort()

  return (
    <div className="grid gap-4">
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Stat label="Due amount" amount={cost.dueAmount} currency={currency} />
        <Stat label="Current month" amount={cost.currentMonthCosts} currency={currency} />
        <Stat label="Last month" amount={cost.lastMonthCosts} currency={currency} />
        <Stat label="Forecasted month end" amount={cost.forecastedMonthEndCosts} currency={currency} />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardContent className="p-5">
            <h3 className="mb-3 font-medium">Financial summary</h3>
            <dl className="text-sm">
              <Row k="Currency" v={fin.currency ?? currency ?? "—"} />
              <Row k="Total credit" v={fmtMoney(fin.totalCredit, fin.currency ?? currency)} />
              <Row k="Total promotional credit" v={fmtMoney(fin.totalPromotionalCredit, fin.currency ?? currency)} />
              <Row k="Current month usage" v={fmtMoney(fin.currentMonthUsage, fin.currency ?? currency)} />
              <Row k="Successful bill transactions" v={String(fin.totalSuccessfulBillTransactions ?? 0)} />
              <Row k="Successful deposits" v={String(fin.totalSuccessfulAddFundsTransactions ?? 0)} />
              <Row k="Transactions (30 days)" v={String(fin.numberOfTransactionsLastMonth ?? 0)} />
            </dl>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="p-5">
            <h3 className="mb-3 font-medium">Costs by type</h3>
            {typeKeys.length === 0 ? (
              <p className="text-sm text-muted-foreground">No costs this month yet.</p>
            ) : (
              <dl className="text-sm">
                {typeKeys.map((k) => (
                  <Row
                    k={k}
                    key={k}
                    v={`${fmtMoney(byType[k] ?? 0, currency)} (last: ${fmtMoney(lastByType[k] ?? 0, currency)})`}
                  />
                ))}
              </dl>
            )}
          </CardContent>
        </Card>
      </div>

      {/* min-w-0: the table's min-content must not widen the tab grid (mobile reflow). */}
      <section className="min-w-0">
        <h3 className="text-eyebrow mb-2">Top cost generators</h3>
        {top.length === 0 ? (
          <p className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">
            No billed resources this month.
          </p>
        ) : (
          <Card className="overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Resource</TableHead>
                  <TableHead>Type</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="text-right">Current cost</TableHead>
                  <TableHead className="text-right">Forecast</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {top.map((t, i) => {
                  const res = (t.resource ?? {}) as Doc
                  return (
                    <TableRow key={res.id ?? i}>
                      <TableCell className="text-sm">{res.name || res.id || "—"}</TableCell>
                      <TableCell className="text-sm text-muted-foreground">{res.type ?? "—"}</TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {res.createdAt ? timeAgo(res.createdAt) : "—"}
                      </TableCell>
                      <TableCell className="text-right font-mono text-sm tabular-nums">
                        {fmtMoney(t.currentCost, currency)}
                      </TableCell>
                      <TableCell className="text-right font-mono text-sm tabular-nums">
                        {fmtMoney(t.forecastedCost, currency)}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </Card>
        )}
      </section>
    </div>
  )
}

// ─── Address — the profile fields, editable via PUT /admin/billing-profile/{id} ────────────────
// The PUT applies exactly the ~17 editable fields (billingProfileUpdateReq): firstName lastName
// company companyName vatCode bank iban taxPayer phone zipCode address city county country email
// currency (+ pricePlanConfig, managed on the Price plans tab). bank/iban come from the raw doc.

function AddressTab({ bpId, bp, raw }: { bpId: string; bp: Summary; raw: Doc | null }) {
  const qc = useQueryClient()
  const [f, setF] = useState<Record<string, any>>({
    firstName: bp.firstName ?? "",
    lastName: bp.lastName ?? "",
    email: bp.email ?? "",
    phone: bp.phone ?? "",
    address: bp.address ?? "",
    city: bp.city ?? "",
    county: bp.county ?? "",
    country: bp.country ?? "",
    zipCode: bp.zipCode ?? "",
    currency: bp.currency ?? "",
    company: !!bp.company,
    companyName: bp.companyName ?? "",
    vatCode: bp.vatCode ?? "",
    taxPayer: !!bp.taxPayer,
    bank: raw?.bank ?? "",
    iban: raw?.iban ?? "",
  })
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setF((s) => ({ ...s, [k]: e.target.value }))

  const save = useMutation({
    mutationFn: () => apiFetch(`/admin/billing-profile/${bpId}`, { method: "PUT", body: f }),
    onSuccess: () => {
      toast.success("Billing profile saved")
      void qc.invalidateQueries({ queryKey: ["admin-bp", bpId] })
      void qc.invalidateQueries({ queryKey: ["admin-bp-raw", bpId] })
      void qc.invalidateQueries({ queryKey: ["admin-list", "/admin/billing-profile"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // autoComplete off: the operator edits the CLIENT's details — their own
  // browser-autofilled address/IBAN here would be silently wrong data.
  const text = (k: string, label: string, placeholder = "") => (
    <div className="grid gap-1.5">
      <Label htmlFor={`bp-${k}`}>{label}</Label>
      <Input id={`bp-${k}`} value={f[k]} placeholder={placeholder} autoComplete="off" onChange={set(k)} />
    </div>
  )

  return (
    <div className="grid gap-4">
      <Card>
        <CardContent className="p-5">
          <h3 className="mb-4 font-medium">Contact</h3>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {text("firstName", "First name")}
            {text("lastName", "Last name")}
            {text("email", "Email")}
            {text("phone", "Phone", "+15551234567")}
            {text("address", "Address")}
            {text("city", "City")}
            {text("county", "County / state")}
            {text("country", "Country", "US")}
            {text("zipCode", "Zip code")}
            {text("currency", "Currency", "USD")}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardContent className="p-5">
          <h3 className="mb-4 font-medium">Company and banking</h3>
          <div className="mb-4 flex flex-wrap items-center gap-8">
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={f.company} onCheckedChange={(v) => setF((s) => ({ ...s, company: v }))} />
              Company
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={f.taxPayer} onCheckedChange={(v) => setF((s) => ({ ...s, taxPayer: v }))} />
              Tax payer
            </label>
          </div>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {text("companyName", "Company name")}
            {text("vatCode", "VAT code")}
            {text("bank", "Bank")}
            {text("iban", "IBAN")}
          </div>
        </CardContent>
      </Card>

      <div className="flex justify-end">
        <Button disabled={save.isPending} onClick={() => save.mutate()}>
          {save.isPending ? "Saving…" : "Save changes"}
        </Button>
      </div>

      <dl className="grid gap-x-8 text-sm sm:grid-cols-2">
        <Row k="Created" v={fmtDateTime(bp.createdAt)} />
        <Row k="Activated" v={fmtDateTime(bp.activatedAt)} />
      </dl>
    </div>
  )
}

// ─── Projects — GET /admin/project/{billingProfileId}/billing-profile → {data:[raw project docs]} ─

function ProjectsTab({ bpId }: { bpId: string }) {
  const navigate = useNavigate()
  const { data, isLoading, isError, error } = useAdminList<Doc>(`/admin/project/${bpId}/billing-profile`)
  const rows = data?.data ?? []

  if (isLoading) return <Skeleton className="h-48" />
  if (isError) return <ErrorPanel error={error} />
  if (!rows.length)
    return <EmptyState icon={FolderKanban} title="No projects" hint="No projects bill against this profile." />

  return (
    <Card className="overflow-hidden py-0">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Project</TableHead>
            <TableHead>Organization</TableHead>
            <TableHead>Created</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((p, i) => {
            const pid = p.id ?? p._id ?? String(i)
            const hasId = Boolean(p.id ?? p._id)
            return (
              <TableRow
                key={pid}
                className={hasId ? "cursor-pointer" : undefined}
                onClick={() => hasId && navigate(`/clients/projects/${pid}`)}
              >
                <TableCell>
                  {hasId ? (
                    <Link
                      to={`/clients/projects/${pid}`}
                      className="inline-block py-1 font-medium hover:underline"
                      onClick={(e) => e.stopPropagation()}
                    >
                      {p.name ?? "—"}
                    </Link>
                  ) : (
                    <span className="font-medium">{p.name ?? "—"}</span>
                  )}
                </TableCell>
                <TableCell>
                  <StatusBadge status={p.status ?? "—"} />
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{pid}</TableCell>
                <TableCell>
                  {p.organizationId ? (
                    <Link
                      to={`/clients/organizations/${p.organizationId}`}
                      className="inline-block py-1 font-mono text-xs text-muted-foreground hover:underline"
                      onClick={(e) => e.stopPropagation()}
                    >
                      {p.organizationId}
                    </Link>
                  ) : (
                    <span className="font-mono text-xs text-muted-foreground">—</span>
                  )}
                </TableCell>
                <TableCell className="text-sm text-muted-foreground">{timeAgo(p.createdAt)}</TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </Card>
  )
}

// ─── Credits tab — GET /admin/account-credit?billingProfileId= (bare {data:[…]}) +
//     grant: POST /admin/account-credit/{billingProfileId} {amount} ────────────────────────────

function CreditsTab({ bpId, currency }: { bpId: string; currency?: string }) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [amount, setAmount] = useState("")

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["admin-bp-credits", bpId],
    queryFn: () => apiFetch<Doc[]>(`/admin/account-credit?billingProfileId=${bpId}`),
  })

  const grant = useMutation({
    mutationFn: (amt: number) =>
      apiFetch(`/admin/account-credit/${bpId}`, { method: "POST", body: { amount: amt } }),
    onSuccess: () => {
      toast.success("Credit granted")
      setOpen(false)
      setAmount("")
      void qc.invalidateQueries({ queryKey: ["admin-bp-credits", bpId] })
      void qc.invalidateQueries({ queryKey: ["admin-bp", bpId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const rows = data ?? []
  const amt = parseFloat(amount)

  return (
    <>
      <div className="mb-3 flex justify-end">
        <Button size="sm" onClick={() => setOpen(true)}>
          <Plus className="size-4" /> Grant credit
        </Button>
      </div>
      {isLoading ? (
        <Skeleton className="h-48" />
      ) : isError ? (
        <ErrorPanel error={error} />
      ) : !rows.length ? (
        <EmptyState icon={Wallet} title="No account credits" hint="Grant a credit to top up this profile." />
      ) : (
        <Card className="overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Credit</TableHead>
                <TableHead className="text-right">Amount</TableHead>
                <TableHead className="text-right">Initial</TableHead>
                <TableHead>Currency</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((c) => (
                <TableRow key={c.id}>
                  <TableCell className="font-mono text-xs">{c.id}</TableCell>
                  <TableCell className="text-right font-mono text-sm tabular-nums">
                    {fmtMoney(c.amount, c.currency ?? currency)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-sm tabular-nums">
                    {fmtMoney(c.initialAmount, c.currency ?? currency)}
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">{c.currency ?? currency ?? "—"}</TableCell>
                  <TableCell className="text-sm text-muted-foreground">{timeAgo(c.createdAt)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Grant account credit</DialogTitle>
            <DialogDescription>
              Adds a spendable credit to this billing profile in the platform base currency.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2 py-2">
            <Label htmlFor="credit-amount">Amount</Label>
            <Input
              id="credit-amount"
              type="number"
              min="0"
              step="0.01"
              placeholder="10.00"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!(amt > 0) || grant.isPending} onClick={() => grant.mutate(amt)}>
              {grant.isPending ? "Granting…" : "Grant credit"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// ─── Suspend — profile status + dunning processes (GET /admin/suspensions/{bpId}) ──────────────
// Status transitions live in the header (Activate / Suspend / Resume). SuspensionProcess JSON keys
// are Go-capitalized (no json tags) → read both casings.

function SuspendTab({
  bpId,
  status,
  raw,
  rawLoading,
}: {
  bpId: string
  status: string
  raw: Doc | null
  rawLoading: boolean
}) {
  const { data, isLoading, isError, error } = useAdminList<Doc>(`/admin/suspensions/${bpId}`)
  const rows = data?.data ?? []

  return (
    <div className="grid gap-4">
      <Card>
        <CardContent className="flex items-center justify-between p-5">
          <div>
            <h3 className="font-medium">Suspension status</h3>
            <p className="mt-1 text-sm text-muted-foreground">
              Use the Activate / Suspend / Resume buttons in the page header to change the profile status.
            </p>
          </div>
          <StatusBadge status={status} />
        </CardContent>
      </Card>

      {rawLoading ? (
        <Skeleton className="h-64" />
      ) : (
        <AutoSuspensionOverrideCard key={raw ? "raw" : "summary"} bpId={bpId} raw={raw} />
      )}

      <section>
        <h3 className="text-eyebrow mb-2">Suspension processes</h3>
        {isLoading ? (
          <Skeleton className="h-40" />
        ) : isError ? (
          <ErrorPanel error={error} />
        ) : !rows.length ? (
          <p className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">
            No suspension processes — this profile has never entered dunning.
          </p>
        ) : (
          <Card className="overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Process</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead>Updated</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((p, i) => {
                  const pid = p.id ?? p.ID ?? String(i)
                  return (
                    <TableRow key={pid}>
                      <TableCell className="font-mono text-xs">{pid}</TableCell>
                      <TableCell>
                        <StatusBadge status={p.status ?? p.Status ?? "—"} />
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {fmtDateTime(p.createdAt ?? p.CreatedAt)}
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {fmtDateTime(p.updatedAt ?? p.UpdatedAt)}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </Card>
        )}
      </section>
    </div>
  )
}

// ─── Automatic suspension override — PUT /admin/billing-profile/automatic-suspension/{id} ──────
// Body (billingProfileAutomaticSuspension → automaticSuspensionConfigReq):
//   { overwriteSuspension: bool,
//     suspensionConfiguration: null | {                       // BillingAutomaticSuspensionConfig
//       enabled: bool,
//       type: "BALANCE" | "DUE_DATE",
//       suspendedAt: { balance?: number, days: int },         // SuspensionLimit (balance for BALANCE, days for DUE_DATE)
//       notifications: [ { balance?: number, days: int } ],   // warning thresholds before suspension
//     } }
// overwriteSuspension=false + suspensionConfiguration=null ⇒ clear the override (inherit the platform default).

type NotifRow = { balance: string; days: string }

function AutoSuspensionOverrideCard({ bpId, raw }: { bpId: string; raw: Doc | null }) {
  const qc = useQueryClient()
  const cfg = (raw?.suspensionConfiguration ?? {}) as Doc
  const susp = (cfg.suspendedAt ?? {}) as Doc

  const [override, setOverride] = useState<boolean>(!!raw?.overwriteSuspension)
  const [enabled, setEnabled] = useState<boolean>(!!cfg.enabled)
  const [type, setType] = useState<"BALANCE" | "DUE_DATE">(cfg.type === "DUE_DATE" ? "DUE_DATE" : "BALANCE")
  const [balance, setBalance] = useState<string>(susp.balance != null ? String(susp.balance) : "")
  const [days, setDays] = useState<string>(susp.days != null ? String(susp.days) : "")
  const [notifs, setNotifs] = useState<NotifRow[]>(
    Array.isArray(cfg.notifications)
      ? (cfg.notifications as Doc[]).map((n) => ({
          balance: n?.balance != null ? String(n.balance) : "",
          days: n?.days != null ? String(n.days) : "",
        }))
      : [],
  )

  const isBalance = type === "BALANCE"

  const buildConfig = () => ({
    enabled,
    type,
    suspendedAt: isBalance ? { balance: Number(balance) || 0 } : { days: parseInt(days, 10) || 0 },
    notifications: notifs.map((n) =>
      isBalance ? { balance: Number(n.balance) || 0 } : { days: parseInt(n.days, 10) || 0 },
    ),
  })

  const save = useMutation({
    mutationFn: (body: { overwriteSuspension: boolean; suspensionConfiguration: Doc | null }) =>
      apiFetch(`/admin/billing-profile/automatic-suspension/${bpId}`, { method: "PUT", body }),
    onSuccess: (_d, body) => {
      toast.success(body.overwriteSuspension ? "Suspension override saved" : "Override cleared — using platform default")
      void qc.invalidateQueries({ queryKey: ["admin-bp-raw", bpId] })
      void qc.invalidateQueries({ queryKey: ["admin-bp", bpId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const clear = () => {
    setOverride(false)
    save.mutate({ overwriteSuspension: false, suspensionConfiguration: null })
  }

  const addNotif = () => setNotifs((s) => [...s, { balance: "", days: "" }])
  const removeNotif = (i: number) => setNotifs((s) => s.filter((_, idx) => idx !== i))
  const setNotif = (i: number, k: keyof NotifRow, v: string) =>
    setNotifs((s) => s.map((n, idx) => (idx === i ? { ...n, [k]: v } : n)))

  return (
    <Card>
      <CardContent className="grid gap-5 p-5">
        <div>
          <h3 className="flex items-center gap-2 font-medium">
            <PauseCircle className="size-4" /> Automatic suspension override
          </h3>
          <p className="mt-1 text-sm text-muted-foreground">
            Overrides the platform suspension configuration for <strong>this billing profile only</strong>.
          </p>
        </div>

        <label className="flex items-center gap-2 text-sm">
          <Switch checked={override} onCheckedChange={setOverride} />
          Override the platform suspension configuration
        </label>

        <fieldset disabled={!override} className="grid gap-5 disabled:opacity-50">
          <label className="flex items-center gap-2 text-sm">
            <Switch checked={enabled} onCheckedChange={setEnabled} disabled={!override} />
            Automatic suspension enabled
          </label>

          <div className="grid gap-2">
            <Label>Suspend by</Label>
            <Select value={type} onValueChange={(v) => setType(v as "BALANCE" | "DUE_DATE")} disabled={!override}>
              <SelectTrigger className="max-w-xs" aria-label="Suspend by">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="BALANCE">Balance</SelectItem>
                <SelectItem value="DUE_DATE">Due date</SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div className="grid max-w-xs gap-2">
            <Label htmlFor="susp-threshold">
              {isBalance ? "Suspend at balance (or below)" : "Suspend after days overdue"}
            </Label>
            {isBalance ? (
              <Input
                id="susp-threshold"
                type="number"
                step="0.01"
                placeholder="-100.00"
                value={balance}
                disabled={!override}
                onChange={(e) => setBalance(e.target.value)}
              />
            ) : (
              <Input
                id="susp-threshold"
                type="number"
                min="0"
                step="1"
                placeholder="7"
                value={days}
                disabled={!override}
                onChange={(e) => setDays(e.target.value)}
              />
            )}
          </div>

          <div className="grid gap-2">
            <div className="flex items-center justify-between">
              <Label>Warning thresholds before suspension</Label>
              <Button type="button" variant="outline" size="sm" disabled={!override} onClick={addNotif}>
                <Plus className="size-4" /> Add threshold
              </Button>
            </div>
            {notifs.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No warning thresholds — the customer is only notified at suspension time.
              </p>
            ) : (
              <div className="grid gap-2">
                {notifs.map((n, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <Input
                      type="number"
                      step={isBalance ? "0.01" : "1"}
                      min={isBalance ? undefined : "0"}
                      className="max-w-xs"
                      placeholder={isBalance ? "Balance, e.g. -50.00" : "Days overdue, e.g. 3"}
                      aria-label={`Warning threshold ${i + 1}`}
                      value={isBalance ? n.balance : n.days}
                      disabled={!override}
                      onChange={(e) => setNotif(i, isBalance ? "balance" : "days", e.target.value)}
                    />
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`Remove warning threshold ${i + 1}`}
                      disabled={!override}
                      onClick={() => removeNotif(i)}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </div>
                ))}
              </div>
            )}
          </div>
        </fieldset>

        <div className="flex justify-end gap-2">
          <Button
            variant="outline"
            disabled={save.isPending || (!override && !raw?.overwriteSuspension)}
            onClick={clear}
          >
            Clear override (use platform default)
          </Button>
          <Button
            disabled={save.isPending}
            onClick={() =>
              save.mutate(
                override
                  ? { overwriteSuspension: true, suspensionConfiguration: buildConfig() }
                  : { overwriteSuspension: false, suspensionConfiguration: null },
              )
            }
          >
            Save override
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

// ─── Bills tab — GET /admin/bill/{billingProfileId}/billing-profile → BillFinancialOverview list ─

function BillsTab({ bpId }: { bpId: string }) {
  const { data, isLoading, isError, error } = useAdminList<Doc>(`/admin/bill/${bpId}/billing-profile`)
  const rows = data?.data ?? []

  if (isLoading) return <Skeleton className="h-48" />
  if (isError) return <ErrorPanel error={error} />
  if (!rows.length) return <EmptyState icon={Receipt} title="No bills" hint="Bills accrue as usage is charged." />

  return (
    <Card className="overflow-hidden py-0">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Bill</TableHead>
            <TableHead>Status</TableHead>
            <TableHead className="text-right">Net</TableHead>
            <TableHead className="text-right">Gross</TableHead>
            <TableHead className="text-right">Unpaid</TableHead>
            <TableHead>Due</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((b) => (
            <TableRow key={b.id}>
              <TableCell className="font-mono text-xs">{b.id}</TableCell>
              <TableCell>
                <StatusBadge status={b.status} />
              </TableCell>
              <TableCell className="text-right font-mono text-sm tabular-nums">
                {fmtMoney(b.totalAmount, b.currency)}
              </TableCell>
              <TableCell className="text-right font-mono text-sm tabular-nums">
                {fmtMoney(b.totalInvoiceAmount, b.invoiceCurrency ?? b.currency)}
              </TableCell>
              <TableCell
                className={cn(
                  "text-right font-mono text-sm tabular-nums",
                  Number(b.unpaidAmount ?? 0) > 0 ? "text-destructive-text" : "text-muted-foreground",
                )}
              >
                {fmtMoney(b.unpaidAmount, b.invoiceCurrency ?? b.currency)}
              </TableCell>
              <TableCell className="text-sm text-muted-foreground">{fmtDate(b.dueAt)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Card>
  )
}

// ─── Price plans — the profile's scoped plans (pricePlanConfig.pricePlanIds), assign/remove via
//     PUT /admin/billing-profile/{id} {pricePlanConfig}; catalog from GET /admin/price-plan ──────

function PricePlansTab({ bpId, bp }: { bpId: string; bp: Summary }) {
  const qc = useQueryClient()
  const [assignOpen, setAssignOpen] = useState(false)
  const [pick, setPick] = useState("")
  const [removeId, setRemoveId] = useState<string | null>(null)

  const cfg = (bp.pricePlanConfig ?? {}) as Doc
  const assigned: string[] = cfg.pricePlanIds ?? []
  const includePublic = !!cfg.includePublicPricePlans

  const plansQ = useAdminList<Doc>(`/admin/price-plan`)
  const plans = plansQ.data?.data ?? []
  const planId = (p: Doc) => p.id ?? p._id
  const byId = new Map(plans.map((p) => [planId(p), p]))
  const unassigned = plans.filter((p) => !assigned.includes(planId(p)))
  // Public plans apply to every profile (they're what an un-scoped profile bills under), so surface
  // them read-only when included — otherwise the tab looks empty even though pricing is active.
  const publicPlans = plans.filter((p) => (p.accessMode as string) === "PUBLIC" && p.enabled !== false)

  const savePlans = useMutation({
    mutationFn: (pricePlanIds: string[]) =>
      apiFetch(`/admin/billing-profile/${bpId}`, {
        method: "PUT",
        body: { pricePlanConfig: { pricePlanIds, includePublicPricePlans: includePublic } },
      }),
    onSuccess: () => {
      toast.success("Price plans updated")
      setAssignOpen(false)
      setPick("")
      setRemoveId(null)
      void qc.invalidateQueries({ queryKey: ["admin-bp", bpId] })
      void qc.invalidateQueries({ queryKey: ["admin-bp-raw", bpId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <>
      <div className="mb-3 flex items-center justify-between">
        <p className="text-sm text-muted-foreground">
          {includePublic ? "Public price plans are included for this profile." : "Only the assigned plans apply."}
        </p>
        <Button size="sm" onClick={() => setAssignOpen(true)}>
          <Plus className="size-4" /> Assign price plan
        </Button>
      </div>

      {plansQ.isLoading ? (
        <Skeleton className="h-40" />
      ) : plansQ.isError ? (
        <ErrorPanel error={plansQ.error} />
      ) : (
        <div className="space-y-6">
          {includePublic && publicPlans.length > 0 ? (
            <div>
              <p className="text-eyebrow mb-2">
                Included public plans (apply to every profile)
              </p>
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Plan</TableHead>
                      <TableHead>Name</TableHead>
                      <TableHead>Access</TableHead>
                      <TableHead>Enabled</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {publicPlans.map((p) => (
                      <TableRow key={planId(p)}>
                        <TableCell className="font-mono text-xs">{planId(p)}</TableCell>
                        <TableCell className="text-sm">{p.name ?? "—"}</TableCell>
                        <TableCell className="text-sm text-muted-foreground">{p.accessMode ?? "—"}</TableCell>
                        <TableCell className="text-sm text-muted-foreground">{p.enabled ? "Yes" : "No"}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </Card>
            </div>
          ) : null}

          <div>
            <p className="text-eyebrow mb-2">
              Scoped plans (this profile only)
            </p>
            {!assigned.length ? (
              <EmptyState
                icon={Layers}
                title="No scoped price plans"
                hint="Assign a plan to scope this profile's pricing beyond the public plans."
              />
            ) : (
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Plan</TableHead>
                      <TableHead>Name</TableHead>
                      <TableHead>Access</TableHead>
                      <TableHead>Enabled</TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {assigned.map((pid) => {
                      const p = byId.get(pid)
                      return (
                        <TableRow key={pid}>
                          <TableCell className="font-mono text-xs">{pid}</TableCell>
                          <TableCell className="text-sm">{p?.name ?? "(unknown plan)"}</TableCell>
                          <TableCell className="text-sm text-muted-foreground">{p?.accessMode ?? "—"}</TableCell>
                          <TableCell className="text-sm text-muted-foreground">
                            {p ? (p.enabled ? "Yes" : "No") : "—"}
                          </TableCell>
                          <TableCell className="text-right">
                            <Button variant="outline" size="sm" onClick={() => setRemoveId(pid)}>
                              <Trash2 className="size-4" /> Remove
                            </Button>
                          </TableCell>
                        </TableRow>
                      )
                    })}
                  </TableBody>
                </Table>
              </Card>
            )}
          </div>
        </div>
      )}

      <Dialog open={assignOpen} onOpenChange={setAssignOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Assign price plan</DialogTitle>
            <DialogDescription>Scope an additional price plan to this billing profile.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-2 py-2">
            <Label>Price plan</Label>
            <Select value={pick} onValueChange={setPick}>
              <SelectTrigger aria-label="Price plan">
                <SelectValue placeholder={unassigned.length ? "Pick a plan" : "No unassigned plans"} />
              </SelectTrigger>
              <SelectContent>
                {unassigned.map((p) => (
                  <SelectItem key={planId(p)} value={planId(p)}>
                    {p.name ?? planId(p)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAssignOpen(false)}>
              Cancel
            </Button>
            <Button
              disabled={!pick || savePlans.isPending}
              onClick={() => savePlans.mutate([...assigned, pick])}
            >
              {savePlans.isPending ? "Assigning…" : "Assign"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!removeId} onOpenChange={(o) => !o && setRemoveId(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Remove price plan</DialogTitle>
            <DialogDescription>
              The plan stops applying to this profile's future charges. The plan itself is not deleted.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRemoveId(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={savePlans.isPending}
              onClick={() => removeId && savePlans.mutate(assigned.filter((x) => x !== removeId))}
            >
              {savePlans.isPending ? "Removing…" : "Remove"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// ─── Promotional credits — GET /admin/promotional-credits/billing-profile/{bpId} +
//     grant: POST /admin/promotional-credits {amount, daysValidity, billingProfileId} ────────────

function PromoCreditsTab({ bpId, currency }: { bpId: string; currency?: string }) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [amount, setAmount] = useState("")
  const [days, setDays] = useState("30")

  const { data, isLoading, isError, error } = useAdminList<Doc>(
    `/admin/promotional-credits/billing-profile/${bpId}`,
  )
  const rows = data?.data ?? []

  const grant = useMutation({
    mutationFn: () =>
      apiFetch(`/admin/promotional-credits`, {
        method: "POST",
        body: { amount: parseFloat(amount), daysValidity: parseInt(days, 10), billingProfileId: bpId },
      }),
    onSuccess: () => {
      toast.success("Promotional credit granted")
      setOpen(false)
      setAmount("")
      void qc.invalidateQueries({ queryKey: ["admin-list", `/admin/promotional-credits/billing-profile/${bpId}`] })
      void qc.invalidateQueries({ queryKey: ["admin-bp", bpId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const amt = parseFloat(amount)
  const d = parseInt(days, 10)

  return (
    <>
      <div className="mb-3 flex justify-end">
        <Button size="sm" onClick={() => setOpen(true)}>
          <Plus className="size-4" /> Grant promotional credit
        </Button>
      </div>
      {isLoading ? (
        <Skeleton className="h-48" />
      ) : isError ? (
        <ErrorPanel error={error} />
      ) : !rows.length ? (
        <EmptyState icon={Gift} title="No promotional credits" hint="Grant a time-limited promotional credit." />
      ) : (
        <Card className="overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Credit</TableHead>
                <TableHead className="text-right">Remaining</TableHead>
                <TableHead className="text-right">Initial</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((c, i) => {
                const cid = c.id ?? c._id ?? String(i)
                return (
                  <TableRow key={cid}>
                    <TableCell className="font-mono text-xs">{cid}</TableCell>
                    <TableCell className="text-right font-mono text-sm tabular-nums">
                      {fmtMoney(c.remainingAmount, currency)}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm tabular-nums">
                      {fmtMoney(c.initialAmount, currency)}
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">{fmtDate(c.expirationDate)}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">{timeAgo(c.createdAt)}</TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </Card>
      )}

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Grant promotional credit</DialogTitle>
            <DialogDescription>
              Promotional credits are consumed before account credits and expire after the validity period.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-2">
            <div className="grid gap-2">
              <Label htmlFor="promo-amount">Amount</Label>
              <Input
                id="promo-amount"
                type="number"
                min="0"
                step="0.01"
                placeholder="25.00"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="promo-days">Days valid</Label>
              <Input
                id="promo-days"
                type="number"
                min="1"
                step="1"
                value={days}
                onChange={(e) => setDays(e.target.value)}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!(amt > 0) || !(d > 0) || grant.isPending} onClick={() => grant.mutate()}>
              {grant.isPending ? "Granting…" : "Grant credit"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// ─── Tax — taxConfiguration {disableAutomaticTaxCalculation, taxRuleId} via
//     PUT /admin/billing-profile/tax-configuration/{id}; rates from GET /admin/tax ───────────────

const TAX_NONE = "__none__"

function TaxTab({ bpId, raw }: { bpId: string; raw: Doc | null }) {
  const qc = useQueryClient()
  const cfg = (raw?.taxConfiguration ?? {}) as Doc
  const [disabled, setDisabled] = useState(!!cfg.disableAutomaticTaxCalculation)
  const [ruleId, setRuleId] = useState<string>(cfg.taxRuleId ?? TAX_NONE)

  const ratesQ = useAdminList<Doc>(`/admin/tax`)
  const rates = ratesQ.data?.data ?? []

  const save = useMutation({
    mutationFn: () =>
      apiFetch(`/admin/billing-profile/tax-configuration/${bpId}`, {
        method: "PUT",
        body: {
          disableAutomaticTaxCalculation: disabled,
          ...(ruleId !== TAX_NONE ? { taxRuleId: ruleId } : {}),
        },
      }),
    onSuccess: () => {
      toast.success("Tax configuration saved")
      void qc.invalidateQueries({ queryKey: ["admin-bp-raw", bpId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <Card>
      <CardContent className="grid max-w-xl gap-5 p-5">
        <div>
          <h3 className="font-medium">Tax configuration</h3>
          <p className="mt-1 text-sm text-muted-foreground">
            By default tax is computed automatically from the profile country. Disable it to pin a specific tax
            rule (or no tax) for this profile.
          </p>
        </div>
        <label className="flex items-center gap-2 text-sm">
          <Switch checked={disabled} onCheckedChange={setDisabled} />
          Disable automatic tax calculation
        </label>
        <div className="grid gap-2">
          <Label>Tax rule</Label>
          {ratesQ.isError ? (
            <ErrorPanel error={ratesQ.error} />
          ) : (
            <Select value={ruleId} onValueChange={setRuleId} disabled={!disabled}>
              <SelectTrigger aria-label="Tax rule">
                <SelectValue placeholder="No tax rule" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={TAX_NONE}>No tax rule</SelectItem>
                {rates.map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    {t.name ?? t.id} {t.country ? `(${t.country})` : ""}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          {!raw && (
            <p className="text-xs text-muted-foreground">
              Current configuration could not be loaded — saving overwrites it.
            </p>
          )}
        </div>
        <div className="flex justify-end">
          <Button disabled={save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? "Saving…" : "Save tax configuration"}
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

// ─── Transactions tab — the two by-billing-profile reads + refund on SUCCESS deposits + sync ────
//   GET /admin/account-credit-transactions/{bp}/billing-profile
//   GET /admin/collect-transactions/{bp}/billing-profile
//   POST /admin/account-credit-transactions/refund/{txnId}
//   GET /admin/account-credit-transactions/{txnId}/sync (re-drives the gateway status)

function TransactionsTab({ bpId }: { bpId: string }) {
  const qc = useQueryClient()
  const [refundId, setRefundId] = useState<string | null>(null)

  const credits = useAdminList<Doc>(`/admin/account-credit-transactions/${bpId}/billing-profile`)
  const collects = useAdminList<Doc>(`/admin/collect-transactions/${bpId}/billing-profile`)

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ["admin-list", `/admin/account-credit-transactions/${bpId}/billing-profile`] })
    void qc.invalidateQueries({ queryKey: ["admin-bp-credits", bpId] })
    void qc.invalidateQueries({ queryKey: ["admin-bp", bpId] })
  }

  const refund = useMutation({
    mutationFn: (txnId: string) =>
      apiFetch(`/admin/account-credit-transactions/refund/${txnId}`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Transaction refunded")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const sync = useMutation({
    mutationFn: (txnId: string) => apiFetch(`/admin/account-credit-transactions/${txnId}/sync`),
    onSuccess: () => {
      toast.success("Transaction synced with the gateway")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const download = useMutation({
    mutationFn: async (txnId: string) => {
      const resp = await apiFetch<Response>(`/admin/collect-transactions/download/${txnId}`, { raw: true })
      if (!resp.ok) throw new Error((await resp.text()) || `Download failed (${resp.status})`)
      await downloadResponse(resp, `${txnId}.pdf`)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <div className="grid gap-6">
      <section>
        <h3 className="text-eyebrow mb-2">Account credit transactions</h3>
        {credits.isLoading ? (
          <Skeleton className="h-32" />
        ) : credits.isError ? (
          <ErrorPanel error={credits.error} />
        ) : !credits.data?.data?.length ? (
          <p className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">No deposit transactions.</p>
        ) : (
          <Card className="overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Date</TableHead>
                  <TableHead>External ID</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Amount</TableHead>
                  <TableHead className="text-right">Gross</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {credits.data.data.map((t) => (
                  <TableRow key={t.id}>
                    <TableCell className="text-sm text-muted-foreground">{fmtDateTime(t.createdAt)}</TableCell>
                    <TableCell className="font-mono text-xs">{t.externalId ?? t.id}</TableCell>
                    <TableCell>
                      <StatusBadge status={t.status} />
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm tabular-nums">
                      {fmtMoney(t.amount, t.currency)}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm tabular-nums">
                      {fmtMoney(t.grossAmount, t.currency)}
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-2">
                        {t.status === "PENDING" && (
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={sync.isPending}
                            onClick={() => sync.mutate(t.id)}
                          >
                            <RefreshCw className="size-4" /> Sync
                          </Button>
                        )}
                        {t.status === "SUCCESS" && (
                          <Button variant="outline" size="sm" onClick={() => setRefundId(t.id)}>
                            <Undo2 className="size-4" /> Refund
                          </Button>
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        )}
      </section>

      <section>
        <h3 className="text-eyebrow mb-2">Collect transactions</h3>
        {collects.isLoading ? (
          <Skeleton className="h-32" />
        ) : collects.isError ? (
          <ErrorPanel error={collects.error} />
        ) : !collects.data?.data?.length ? (
          <p className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">No collect transactions.</p>
        ) : (
          <Card className="overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Date</TableHead>
                  <TableHead>External ID</TableHead>
                  <TableHead>Bill</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Amount</TableHead>
                  <TableHead className="text-right">Gross</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {collects.data.data.map((t) => (
                  <TableRow key={t.id}>
                    <TableCell className="text-sm text-muted-foreground">{fmtDateTime(t.createdAt)}</TableCell>
                    <TableCell className="font-mono text-xs">{t.externalId ?? t.id}</TableCell>
                    <TableCell className="font-mono text-xs">{t.billId ?? "—"}</TableCell>
                    <TableCell>
                      <StatusBadge status={t.status} />
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm tabular-nums">
                      {fmtMoney(t.amount, t.currency)}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm tabular-nums">
                      {fmtMoney(t.grossAmount, t.currency)}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={download.isPending}
                        onClick={() => download.mutate(t.id)}
                      >
                        <Download className="size-4" /> Download
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        )}
      </section>

      <Dialog open={!!refundId} onOpenChange={(o) => !o && setRefundId(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Refund transaction</DialogTitle>
            <DialogDescription>
              This refunds the payment at the gateway and voids the deposited account credit. It cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRefundId(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={refund.isPending}
              onClick={() => {
                if (refundId) refund.mutate(refundId)
                setRefundId(null)
              }}
            >
              Refund
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// ─── Validation — profile validationStatus + verifications + the pending validation's
//     APPROVE/REJECT (POST /admin/billing-profile/validations/{validationId}/status/{status}) ────

function ValidationTab({ bpId, bp }: { bpId: string; bp: Summary }) {
  const qc = useQueryClient()
  const [decide, setDecide] = useState<"APPROVED" | "REJECTED" | null>(null)

  // GET /admin/billing-profile/validations → PENDING validations joined with their profile.
  const pendingQ = useAdminList<Doc>(`/admin/billing-profile/validations`)
  const mine = (pendingQ.data?.data ?? []).find((v) => v.billingProfileId === bpId) ?? null

  const verifications = (bp.verifications ?? []) as Doc[]

  const setStatus = useMutation({
    mutationFn: (status: "APPROVED" | "REJECTED") =>
      apiFetch(`/admin/billing-profile/validations/${mine!.id ?? mine!._id}/status/${status}`, { method: "POST" }),
    onSuccess: (_d, status) => {
      toast.success(status === "APPROVED" ? "Validation approved — profile activated" : "Validation rejected")
      void qc.invalidateQueries({ queryKey: ["admin-list", "/admin/billing-profile/validations"] })
      void qc.invalidateQueries({ queryKey: ["admin-bp", bpId] })
      void qc.invalidateQueries({ queryKey: ["admin-bp-raw", bpId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <div className="grid gap-4">
      <Card>
        <CardContent className="p-5">
          <div className="flex items-center justify-between gap-4">
            <div>
              <h3 className="font-medium">Identity validation</h3>
              <p className="mt-1 text-sm text-muted-foreground">
                {mine
                  ? "A validation request is pending review. Approving it activates the billing profile."
                  : bp.validationStatus
                    ? `Validation status: ${bp.validationStatus}`
                    : "No validation request on this profile."}
              </p>
            </div>
            {pendingQ.isLoading ? (
              <Skeleton className="h-8 w-40" />
            ) : mine ? (
              <div className="flex gap-2">
                <Button size="sm" onClick={() => setDecide("APPROVED")}>
                  <CheckCircle2 className="size-4" /> Approve
                </Button>
                <Button variant="outline" size="sm" onClick={() => setDecide("REJECTED")}>
                  <XCircle className="size-4" /> Reject
                </Button>
              </div>
            ) : (
              <StatusBadge status={bp.validationStatus ?? "NONE"} />
            )}
          </div>
        </CardContent>
      </Card>

      <section>
        <h3 className="text-eyebrow mb-2">Verifications</h3>
        {!verifications.length ? (
          <EmptyState icon={ShieldCheck} title="No verifications" hint="Verification entries appear here." />
        ) : (
          <Card className="overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Key</TableHead>
                  <TableHead>Provider</TableHead>
                  <TableHead>Verified</TableHead>
                  <TableHead>Verified at</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {verifications.map((v, i) => (
                  <TableRow key={i}>
                    <TableCell className="text-sm">{v.key ?? "—"}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">{v.provider ?? "—"}</TableCell>
                    <TableCell className="text-sm">{v.verified ? "Yes" : "No"}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {v.verifiedAt ? fmtDateTime(v.verifiedAt) : "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        )}
      </section>

      <Dialog open={!!decide} onOpenChange={(o) => !o && setDecide(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{decide === "APPROVED" ? "Approve validation" : "Reject validation"}</DialogTitle>
            <DialogDescription>
              {decide === "APPROVED"
                ? "Approving marks the validation constraint complete and activates the billing profile."
                : "Rejecting keeps the profile unvalidated. The customer can submit a new request."}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDecide(null)}>
              Cancel
            </Button>
            <Button
              variant={decide === "REJECTED" ? "destructive" : "default"}
              disabled={setStatus.isPending}
              onClick={() => {
                if (decide) setStatus.mutate(decide)
                setDecide(null)
              }}
            >
              {decide === "APPROVED" ? "Approve" : "Reject"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// ─── Quota — projectProvisioningQuota {enabled, limit} via
//     PUT /admin/billing-profile/project-provisioning-quota/{id} ─────────────────────────────────

function QuotaTab({ bpId, raw }: { bpId: string; raw: Doc | null }) {
  const qc = useQueryClient()
  const cur = (raw?.projectProvisioningQuota ?? {}) as Doc
  const [enabled, setEnabled] = useState(!!cur.enabled)
  const [limit, setLimit] = useState(String(cur.limit ?? 0))

  const save = useMutation({
    mutationFn: () =>
      apiFetch(`/admin/billing-profile/project-provisioning-quota/${bpId}`, {
        method: "PUT",
        body: { enabled, limit: parseInt(limit, 10) || 0 },
      }),
    onSuccess: () => {
      toast.success("Provisioning quota saved")
      void qc.invalidateQueries({ queryKey: ["admin-bp-raw", bpId] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <Card>
      <CardContent className="grid max-w-xl gap-5 p-5">
        <div>
          <h3 className="flex items-center gap-2 font-medium">
            <Gauge className="size-4" /> Project provisioning quota
          </h3>
          <p className="mt-1 text-sm text-muted-foreground">
            Overrides the platform-wide quota: caps how many projects this profile can provision.
          </p>
        </div>
        <label className="flex items-center gap-2 text-sm">
          <Switch checked={enabled} onCheckedChange={setEnabled} />
          Enforce quota for this profile
        </label>
        <div className="grid gap-2">
          <Label htmlFor="quota-limit">Project limit</Label>
          <Input
            id="quota-limit"
            type="number"
            min="0"
            step="1"
            value={limit}
            disabled={!enabled}
            onChange={(e) => setLimit(e.target.value)}
          />
        </div>
        {!raw && (
          <p className="text-xs text-muted-foreground">
            Current quota could not be loaded — saving overwrites it.
          </p>
        )}
        <div className="flex justify-end">
          <Button disabled={save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? "Saving…" : "Save quota"}
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}
