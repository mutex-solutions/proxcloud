// common.tsx — what the steps of the Platform setup page share: the status answer of GET /admin/setup/status
// (docs/api.md "Platform setup"), the state labels, the item and check rows, and a copyable command block.
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Copy } from "lucide-react"
import { toast } from "sonner"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { apiFetch } from "@/lib/api"
import { cn } from "@/lib/utils"

export type SetupState = "done" | "attention" | "todo" | "blocked" | "unknown"
export type SetupItem = { id: string; state: SetupState; message: string }
export type SetupStep = { id: string; state: SetupState; summary: string; items: SetupItem[] }
// seedUrl: where a URL seed is fetched from (config.metadata.url, else the API's own base URL; "" = none).
export type SetupStatus = { serviceId: string; live: boolean; seedUrl?: string; steps: SetupStep[] }

// One row of POST /admin/service/proxmox/auth?checks=all.
export type PVECheck = { id: string; status: "ok" | "warn" | "fail"; message: string }
export type NodeBridge = { node: string; iface: string; type: string; cidr?: string; management: boolean }
export type PVEAuthResult = {
  version?: string
  nodes?: { node: string; status: string }[]
  storages?: { storage: string; type: string; content?: string }[]
  warnings?: string[]
  checks?: PVECheck[]
  bridges?: NodeBridge[]
  presentedFingerprint?: string
}

// A text link in the page's foreground color (the brand color fails contrast as text).
export const LINK = "justify-self-start text-sm underline underline-offset-4"

export const errMsg = (e: unknown) => (e instanceof Error ? e.message : String(e))

// The steps of the page in order. prepare has no state of its own: the API reports the others. managed is optional and
// comes after go live, which does not wait for it.
export const SETUP_STEPS = [
  { id: "prepare", title: "Prepare Proxmox" },
  { id: "connect", title: "Connect" },
  { id: "network", title: "Network" },
  { id: "storage", title: "Storage" },
  { id: "templates", title: "OS templates" },
  { id: "catalog", title: "Catalog and pricing" },
  { id: "golive", title: "Go live" },
  { id: "managed", title: "Managed services (optional)" },
] as const

// Literal classes so Tailwind emits them. Anything the API did not call done is never shown as done: an unknown or
// missing state reads "Unknown".
const STATE_VIEW: Record<string, { label: string; dot: string }> = {
  done: { label: "Done", dot: "status-dot-ok" },
  attention: { label: "Needs attention", dot: "status-dot-warn" },
  todo: { label: "To do", dot: "status-dot-muted" },
  blocked: { label: "Blocked", dot: "status-dot-muted" },
  unknown: { label: "Unknown", dot: "status-dot-muted" },
}
const CHECK_VIEW: Record<string, { label: string; dot: string }> = {
  ok: { label: "OK", dot: "status-dot-ok" },
  warn: { label: "Warning", dot: "status-dot-warn" },
  fail: { label: "Failed", dot: "status-dot-error" },
}
export const stateView = (state?: string) => STATE_VIEW[state ?? ""] ?? STATE_VIEW.unknown

export function StateBadge({ state, className }: { state?: string; className?: string }) {
  const v = stateView(state)
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-sm", className)}>
      <span className={cn("status-dot", v.dot)} aria-hidden="true" />
      <span className="text-muted-foreground">{v.label}</span>
    </span>
  )
}

function Row({ dot, label, title, message }: { dot: string; label: string; title?: string; message: string }) {
  return (
    <li className="flex flex-col gap-1 border-b py-2 text-sm last:border-b-0 sm:flex-row sm:gap-3">
      <span className="flex shrink-0 items-center gap-1.5 self-start pt-0.5 sm:w-36">
        <span className={cn("status-dot", dot)} aria-hidden="true" />
        <span className="text-muted-foreground">{label}</span>
      </span>
      <span className="min-w-0 flex-1 break-words whitespace-pre-line">
        {title ? <span className="mr-2 font-mono text-xs text-muted-foreground">{title}</span> : null}
        {message}
      </span>
    </li>
  )
}

export function ItemRows({ items }: { items?: SetupItem[] }) {
  if (!items?.length) return null
  return (
    <ul aria-label="Checks of this step">
      {items.map((it) => {
        const v = stateView(it.state)
        return <Row key={it.id} dot={v.dot} label={v.label} title={it.id} message={it.message} />
      })}
    </ul>
  )
}

export function CheckRows({ checks }: { checks?: PVECheck[] }) {
  if (!checks?.length) return null
  return (
    <ul aria-label="Connection checks">
      {checks.map((c) => {
        const v = CHECK_VIEW[c.status] ?? { label: c.status, dot: "status-dot-muted" }
        return <Row key={c.id} dot={v.dot} label={v.label} title={c.id} message={c.message} />
      })}
    </ul>
  )
}

export function CopyBlock({ text, label }: { text: string; label: string }) {
  return (
    <div className="relative min-w-0">
      <pre className="max-h-96 overflow-auto rounded-lg border bg-muted/40 p-3 pr-12 font-mono text-xs whitespace-pre" aria-label={label} tabIndex={0}>
        {text}
      </pre>
      <Button
        size="icon"
        variant="ghost"
        className="absolute top-1 right-1"
        aria-label={`Copy ${label}`}
        onClick={() => {
          void navigator.clipboard?.writeText(text)
          toast.success("Copied")
        }}
      >
        <Copy className="size-4" />
      </Button>
    </div>
  )
}

export function ErrorNote({ title, error }: { title: string; error: unknown }) {
  if (!error) return null
  return (
    <Alert variant="destructive">
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription>{errMsg(error)}</AlertDescription>
    </Alert>
  )
}

// StepCard frames one step: its title and state, the API's summary and items, then the step's own content.
export function StepCard({ n, title, step, description, children }: {
  n: number
  title: string
  step?: SetupStep
  description?: React.ReactNode
  children?: React.ReactNode
}) {
  return (
    <Card>
      <CardHeader className="border-b">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle className="text-base">{n}. {title}</CardTitle>
          {step ? <StateBadge state={step.state} /> : null}
        </div>
        {description ? <CardDescription>{description}</CardDescription> : null}
        {step?.summary ? <p className="text-sm">{step.summary}</p> : null}
      </CardHeader>
      <CardContent className="grid grid-cols-1 gap-4">
        <ItemRows items={step?.items} />
        {children}
      </CardContent>
    </Card>
  )
}

// GET /admin/setup/status needs all four reads (defaultsReadPerms in internal/platform/admin/setupdefaults.go): the
// nav link and the Dashboard card are shown only with every one of them.
export const SETUP_READ_PERMS = ["admin:service:read", "admin:price_plan:read", "admin:platform_config:read", "admin:billing_config:read"]

// The status of the install. live=false reads the database only (the Dashboard card); live=true also asks the cluster.
export function setupStatusQuery(serviceId: string, live: boolean) {
  const q = new URLSearchParams()
  if (live) q.set("live", "1")
  if (serviceId) q.set("serviceId", serviceId)
  const qs = q.toString()
  return {
    queryKey: ["setup-status", live, serviceId],
    queryFn: () => apiFetch<SetupStatus>(`/admin/setup/status${qs ? `?${qs}` : ""}`),
    retry: false,
  }
}

// useSetupStatus shows the database answer at once and the live one when the cluster has answered. An answer whose
// last read failed is not current (react-query keeps the data of the read before), so it is not shown: a failed live
// read falls back to the database answer, and with both failed there is none.
export function useSetupStatus(serviceId: string) {
  const db = useQuery(setupStatusQuery(serviceId, false))
  const live = useQuery(setupStatusQuery(serviceId, true))
  const current = (q: typeof db) => (q.error ? undefined : q.data)
  return { data: current(live) ?? current(db), db, live }
}

// useClusterInfo reads the nodes and bridges of a saved provider: the connection test on its stored connection. The
// bridges come only for a caller with admin:service:manage; without them the forms take names as text.
export function useClusterInfo(serviceId: string) {
  return useQuery({
    queryKey: ["setup-cluster", serviceId],
    queryFn: () => apiFetch<PVEAuthResult>(`/admin/service/proxmox/auth?checks=all&externalServiceId=${encodeURIComponent(serviceId)}`, { method: "POST", body: {} }),
    enabled: !!serviceId,
    retry: false,
    staleTime: 60_000,
  })
}

// The provider document, read fresh before a save: the update route replaces each top-level config key, so a block is
// sent whole, built on what is stored now rather than on what the form was opened with.
export const readProvider = (serviceId: string) => apiFetch<{ name?: string; config?: Record<string, any> }>(`/admin/service/${serviceId}`)

export function useInvalidateSetup() {
  const qc = useQueryClient()
  return (serviceId?: string) => {
    void qc.invalidateQueries({ queryKey: ["setup-status"] })
    if (serviceId) void qc.invalidateQueries({ queryKey: ["admin-get", `/admin/service/${serviceId}`] })
  }
}

// fetchText reads a text/plain answer (the setup script); an error answer carries the usual envelope.
export async function fetchText(path: string): Promise<string> {
  const r = await apiFetch<Response>(path, { raw: true })
  const text = await r.text()
  if (!r.ok) {
    let msg = text
    try {
      msg = JSON.parse(text).errors?.message ?? text
    } catch {
      // not JSON: the text is the message
    }
    throw new Error(msg || r.statusText)
  }
  return text
}
