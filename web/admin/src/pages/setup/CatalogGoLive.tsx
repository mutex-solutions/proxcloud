// CatalogGoLive.tsx — steps 6 to 8 of the Platform setup page. Step 6 previews the install defaults
// (GET /admin/setup/defaults) and applies the chosen items; the price plan is created enabled, so the operator confirms
// the rates first and the apply carries confirmRates. Step 7 turns on the attach policy, runs one provider sync and
// attaches the projects created meanwhile. Step 8 only says what comes next.
import { useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { CopyBlock, ErrorNote, LINK, StateBadge, StepCard, errMsg, useInvalidateSetup, type SetupStatus, type SetupStep } from "./common"

type Doc = Record<string, any>
type DefaultsItem = { id: string; state: string; message: string; preview?: any }
type Defaults = { currency: string; items: DefaultsItem[] }
const ITEM_TITLES: Record<string, string> = { flavors: "Flavors", pricePlan: "Public price plan", quota: "Default project quota", region: "Region row" }
const APPLY_STATES: Record<string, string> = { created: "done", skipped: "done", blocked: "blocked" }

// ── Step 6: Catalog and pricing ───────────────────────────────────────────────

function tierText(t: Doc) {
  return t.from !== undefined || t.to !== undefined ? `${t.from ?? 0} to ${t.to ?? "more"}: ${t.value}` : String(t.value)
}

function RateTable({ rules, currency }: { rules: Doc[]; currency: string }) {
  return (
    <div>
      <Table aria-label="Rates">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Rule</TableHead>
            <TableHead>Resource</TableHead>
            <TableHead>Rate ({currency} per unit and {rules[0]?.timeUnit ?? "hour"})</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rules.map((r) => (
            <TableRow key={r.name}>
              <TableCell className="text-sm whitespace-normal">{r.name}</TableCell>
              <TableCell className="font-mono text-xs whitespace-normal">
                {r.resourceType}
                {(r.filters ?? []).map((f: Doc) => <div key={f.attributeName} className="text-muted-foreground">{f.attributeName} {f.operator} {String(f.value)}</div>)}
              </TableCell>
              <TableCell className="font-mono text-xs whitespace-normal">
                {(r.prices ?? []).map((p: Doc) => <div key={p.attributeName}>{p.attributeName}: {(p.tiers ?? []).map(tierText).join("; ")}</div>)}
                {r.timeUnit && r.timeUnit !== "hour" ? <div className="text-muted-foreground">per {r.timeUnit}</div> : null}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function Preview({ item, currency }: { item: DefaultsItem; currency: string }) {
  const p = item.preview
  if (!p) return null
  if (item.id === "flavors") {
    return <p className="font-mono text-xs text-muted-foreground">{(p as Doc[]).map((f) => `${f.slug} (${f.vcpus} vCPU, ${f.ramMb} MB, ${f.diskGb} GB)`).join(", ")}</p>
  }
  if (item.id === "pricePlan") {
    return (
      <div className="grid grid-cols-1 gap-2">
        <p className="text-sm">Plan <b>{p.plan?.name}</b>, {p.plan?.accessMode}, created enabled, {p.rules?.length ?? 0} rules, amounts in {p.currency ?? currency}:</p>
        <RateTable rules={p.rules ?? []} currency={p.currency ?? currency} />
      </div>
    )
  }
  if (item.id === "quota") {
    return <p className="font-mono text-xs text-muted-foreground">{Object.entries(p as Doc).map(([k, v]) => `${k}: ${typeof v === "object" ? JSON.stringify(v) : v}`).join(", ")}</p>
  }
  return <p className="font-mono text-xs text-muted-foreground">{p.region ? `region ${p.region}` : JSON.stringify(p)}</p>
}

export function CatalogStep({ step, serviceId }: { step?: SetupStep; serviceId: string }) {
  const qc = useQueryClient()
  const invalidate = useInvalidateSetup()
  const defaults = useQuery({
    queryKey: ["setup-defaults", serviceId],
    queryFn: () => apiFetch<Defaults>(`/admin/setup/defaults${serviceId ? `?serviceId=${encodeURIComponent(serviceId)}` : ""}`),
    retry: false,
  })
  const unpriced = useQuery({
    queryKey: ["setup-unpriced", serviceId],
    queryFn: () => apiFetch<{ id: string; name?: string; region?: string; reason?: string }[]>(`/admin/service/${serviceId}/unpriced-flavors`),
    enabled: !!serviceId,
    retry: false,
  })
  const todo = (defaults.data?.items ?? []).filter((i) => i.state === "todo").map((i) => i.id)
  const [unpicked, setUnpicked] = useState<string[]>([])
  const [ratesOK, setRatesOK] = useState(false)
  const items = todo.filter((id) => !unpicked.includes(id))
  const withPlan = items.includes("pricePlan")
  const apply = useMutation({
    mutationFn: () => apiFetch<{ items: DefaultsItem[] }>("/admin/setup/defaults", {
      method: "POST",
      body: { serviceId, items, confirmRates: withPlan && ratesOK },
    }),
    onSettled: () => {
      setRatesOK(false)
      void qc.invalidateQueries({ queryKey: ["setup-defaults"] })
      void qc.invalidateQueries({ queryKey: ["setup-unpriced"] })
      invalidate(serviceId)
    },
  })

  return (
    <StepCard
      n={6}
      title="Catalog and pricing"
      step={step}
      description="The flavors tenants pick, the public rate card, the default project quota and the region row. Shown first, applied only when you confirm; nothing that exists is changed."
    >
      {defaults.isLoading ? <Skeleton className="h-32" /> : null}
      <ErrorNote title="The defaults could not be read" error={defaults.error} />
      {defaults.data ? (
        <ul className="grid grid-cols-1 gap-3" aria-label="Defaults">
          {defaults.data.items.map((it) => (
            <li key={it.id} className="grid grid-cols-1 gap-2 rounded-lg border p-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                {it.state === "todo" ? (
                  <label className="flex items-center gap-2 text-sm font-medium">
                    <Checkbox checked={!unpicked.includes(it.id)} onCheckedChange={(on) => setUnpicked(on === true ? unpicked.filter((x) => x !== it.id) : [...unpicked, it.id])} />
                    Apply: {ITEM_TITLES[it.id] ?? it.id}
                  </label>
                ) : (
                  <span className="text-sm font-medium">{ITEM_TITLES[it.id] ?? it.id}</span>
                )}
                <StateBadge state={it.state} />
              </div>
              <p className="text-sm text-muted-foreground">{it.message}</p>
              {it.state === "todo" ? <Preview item={it} currency={defaults.data!.currency} /> : null}
            </li>
          ))}
        </ul>
      ) : null}

      {withPlan ? (
        <label className="flex items-start gap-2 rounded-lg border border-amber-500/40 p-3 text-sm">
          <Checkbox className="mt-0.5" checked={ratesOK} onCheckedChange={(on) => setRatesOK(on === true)} />
          <span>
            I reviewed these rates. The plan is created enabled: every resource it matches is billed at them in{" "}
            {defaults.data?.currency} from the next charge run. Change them later under System, Price plans.
          </span>
        </label>
      ) : null}
      {defaults.data ? (
        <div className="flex flex-wrap items-center gap-3">
          <Button onClick={() => apply.mutate()} disabled={!items.length || (withPlan && !ratesOK) || apply.isPending}>
            {apply.isPending ? "Applying…" : `Apply ${items.length} item${items.length === 1 ? "" : "s"}`}
          </Button>
          {!todo.length ? <span className="text-sm text-muted-foreground">Nothing to add.</span> : null}
        </div>
      ) : null}
      <ErrorNote title="The apply was refused" error={apply.error} />
      {apply.data ? (
        <ul className="grid gap-1 text-sm" aria-label="Applied">
          {apply.data.items.map((it) => (
            <li key={it.id} className="flex flex-wrap items-center gap-2">
              <StateBadge state={APPLY_STATES[it.state]} /> <span className="font-medium">{ITEM_TITLES[it.id] ?? it.id}</span> {it.state}: {it.message}
            </li>
          ))}
        </ul>
      ) : null}

      {unpriced.data?.length ? (
        <Alert>
          <AlertTitle>Flavors no price rule matches are billed as zero</AlertTitle>
          <AlertDescription>
            <ul className="list-disc pl-5">
              {unpriced.data.map((f) => <li key={`${f.region}/${f.id}`}>{f.name ?? f.id}{f.region ? ` in ${f.region}` : ""}: {f.reason}</li>)}
            </ul>
            <p>Add a rule for them under <Link to="/system/price-plans" className="underline underline-offset-4">Price plans</Link>.</p>
          </AlertDescription>
        </Alert>
      ) : null}
      <ErrorNote title="The unpriced flavors could not be read" error={unpriced.error} />

      <div className="grid gap-1 border-t pt-4 text-sm">
        <p className="font-medium">Recommended, not required</p>
        <Link to="/system/billing-configuration" className={LINK}>Business name, address and country that invoices carry (Billing configuration)</Link>
        <Link to="/system/integrations" className={LINK}>A mail integration, so bills and reminders are sent (Integrations)</Link>
      </div>
    </StepCard>
  )
}

// ── Step 7: Go live ───────────────────────────────────────────────────────────

const PREREQS = [["connect", "2. Connect"], ["network", "3. Network"], ["storage", "4. Storage"], ["templates", "5. OS templates"]] as const
const passes = (state?: string) => state === "done" || state === "attention"
type Outcome = { label: string; ok: boolean; message: string }

export function GoLiveStep({ status, serviceId }: { status?: SetupStatus; serviceId: string }) {
  const invalidate = useInvalidateSetup()
  const stepOf = (id: string) => status?.steps.find((s) => s.id === id)
  const step = stepOf("golive")
  const catalog = stepOf("catalog")
  const [skipCatalog, setSkipCatalog] = useState(false)
  const scheduler = step?.items.find((i) => i.id === "scheduler")
  // The API answers blocked or unknown when a prerequisite is open or was not read (without live=1, templates).
  const unread = step?.state === "unknown" || PREREQS.some(([id]) => stepOf(id)?.state === "unknown")
  const open = PREREQS.filter(([id]) => !passes(stepOf(id)?.state) && stepOf(id)?.state !== "unknown").map(([, title]) => title)
  const ready = !!serviceId && !!step && !open.length && !unread && step.state !== "blocked" && (passes(catalog?.state) || skipCatalog)

  const go = useMutation({
    mutationFn: async (): Promise<Outcome[]> => {
      const base = `/admin/service/${serviceId}`
      const out: Outcome[] = []
      await apiFetch(`${base}/attach-policy`, { method: "PUT", body: { attachToNewProjects: true } })
      out.push({ label: "Attach new projects", ok: true, message: "on" })
      try {
        await apiFetch("/admin/job/external-service/sync", { method: "POST" })
        out.push({ label: "Provider sync", ok: true, message: "ran once" })
      } catch (e) {
        out.push({ label: "Provider sync", ok: false, message: errMsg(e) })
      }
      try {
        const scope = { mode: "all" }
        const preview = await apiFetch<{ toAttach?: number }>(`${base}/bulk-attach/preview`, { method: "POST", body: { scope } })
        if (preview?.toAttach) {
          const run = await apiFetch<{ id?: string }>(`${base}/bulk-attach`, { method: "POST", body: { scope } })
          out.push({ label: "Existing projects", ok: true, message: `${preview.toAttach} queued for attaching (run ${run?.id ?? "started"}; progress on the provider page, Projects tab)` })
        } else {
          out.push({ label: "Existing projects", ok: true, message: "none to attach" })
        }
      } catch (e) {
        out.push({ label: "Existing projects", ok: false, message: errMsg(e) })
      }
      return out
    },
    onSettled: () => invalidate(serviceId),
  })

  return (
    <StepCard
      n={7}
      title="Go live"
      step={step}
      description="Turns on the attach policy, so new projects are provisioned on this provider, runs one provider sync, and attaches the projects created meanwhile."
    >
      <ul className="grid gap-1 text-sm" aria-label="Prerequisites">
        {PREREQS.map(([id, title]) => (
          <li key={id} className="flex items-center justify-between gap-2"><span>{title}</span><StateBadge state={stepOf(id)?.state} /></li>
        ))}
        <li className="flex items-center justify-between gap-2"><span>6. Catalog and pricing</span><StateBadge state={catalog?.state} /></li>
      </ul>
      {catalog && !passes(catalog.state) ? (
        <label className="flex items-start gap-2 text-sm">
          <Checkbox className="mt-0.5" checked={skipCatalog} onCheckedChange={(on) => setSkipCatalog(on === true)} />
          <span>Go live without a complete catalog: usage that no price rule matches is billed as zero.</span>
        </label>
      ) : null}
      {scheduler?.state === "attention" ? (
        <Alert>
          <AlertTitle>The job scheduler is off</AlertTitle>
          <AlertDescription>
            <p>
              Nothing syncs on a timer and no charge job runs, so usage is not billed on a timer. Go live runs one sync now;
              a later sync is up to you. Turn the scheduler on once the rates are the ones you want invoiced. It does not
              block going live.
            </p>
            <CopyBlock text="./proxcloud-up.sh install --scheduler" label="scheduler command" />
          </AlertDescription>
        </Alert>
      ) : null}
      <div className="flex flex-wrap items-center gap-3">
        <Button onClick={() => go.mutate()} disabled={!ready || go.isPending}>{go.isPending ? "Going live…" : "Go live"}</Button>
        {!ready ? (
          <span className="text-sm text-muted-foreground">
            {!serviceId ? "Save a provider first."
              : open.length ? `Finish first: ${open.join(", ")}.`
              : unread ? "Some steps could not be checked: use Check again."
              : !passes(catalog?.state) && !skipCatalog ? "Finish the catalog, or go live without it." : ""}
          </span>
        ) : null}
      </div>
      <ErrorNote title="Go live stopped" error={go.error} />
      {go.data ? (
        <ul className="grid gap-1 text-sm" aria-label="Go live results">
          {go.data.map((o) => <li key={o.label} className={o.ok ? "" : "text-destructive"}>{o.label}: {o.message}</li>)}
        </ul>
      ) : null}
      <div className="grid gap-1 border-t pt-4 text-sm">
        <p className="font-medium">Then the first tenant, on the customer console</p>
        <ol className="list-decimal pl-5 text-muted-foreground">
          <li>Sign up, create an organization and a project, fill the billing details.</li>
          <li>
            Activate the billing profile (<Link to="/clients/billing-profiles" className="underline underline-offset-4">Client area, Billing profiles</Link>),
            or turn on auto-activation (<Link to="/system/billing-configuration" className="underline underline-offset-4">Billing configuration</Link>).
          </li>
          <li>Create a network, then a server from a template.</li>
        </ol>
      </div>
    </StepCard>
  )
}

// ── Step 8: Next ──────────────────────────────────────────────────────────────

export function NextStep() {
  return (
    <StepCard n={8} title="Next: managed services" description="Not part of this setup. Managed Kubernetes, databases and sandboxes come with the platform lifecycle work.">
      <div className="grid gap-2 text-sm text-muted-foreground" aria-disabled="true">
        <p>What it adds:</p>
        <ul className="list-disc pl-5">
          <li>the seed cluster the managed services run on;</li>
          <li>more Proxmox identities: ProxCloudKubeMint, <code>capmox@pve</code> and <code>csi@pve</code>;</li>
          <li>price rules and quota for <code>kubernetes_cluster</code>, <code>database_cluster</code> and <code>sandbox</code>.</li>
        </ul>
        <a
          className={LINK}
          href="https://github.com/mutex-solutions/proxcloud/blob/main/docs/adr/0009-managed-services-platform-lifecycle.md"
          target="_blank"
          rel="noreferrer"
        >
          ADR 0009: managed-services platform lifecycle
        </a>
        <div><Button disabled variant="outline">Enable managed services (not available yet)</Button></div>
      </div>
    </StepCard>
  )
}

