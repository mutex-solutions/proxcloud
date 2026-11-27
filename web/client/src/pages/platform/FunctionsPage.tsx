// Functions: a container image behind a stable internal HTTPS URL that scales to zero when idle
// and starts again on the next request. Backend: /project/{id}/functions on a sandbox location
// whose service carries the `functions` block. Images only, internal endpoints only: nothing here
// builds an image, serves a function on the internet, or gives it a hostname of its own or a GPU.
import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Copy, Info, MoreHorizontal, Pause, Play, Plus, RefreshCw, Rocket, ScrollText, SquareFunction, Trash2, Undo2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { registryHost, serverHost, timeAgo } from "@/lib/format"
import { useProjectId, useProjectServices } from "@/lib/hooks"
import type { CloudFunction, FunctionDeployment, FunctionLimits, ProjectService } from "@/lib/types"

// What the API applies where the block does not state a limit.
const DEFAULT_LIMITS: FunctionLimits = {
  domain: "", minInstances: 0, maxMinInstances: 0, idleSeconds: 300, minIdleSeconds: 60, maxIdleSeconds: 3600,
  maxCpuMillis: 2000, maxMemoryMi: 4096, maxFunctions: 20, maxInstances: 5, defaultConcurrency: 50, maxConcurrency: 1000,
}
// The limits of the location that runs functions, or undefined where none does.
const functionLimits = (services?: ProjectService[]): FunctionLimits | undefined => {
  const block = services?.find((s) => s.functions)?.functions
  return block ? { ...DEFAULT_LIMITS, ...(block === true ? {} : block) } : undefined
}

const CPU_SIZES = [250, 500, 1000, 2000]
// The sizes the API takes under a limit: the fractions, then whole vCPUs.
const cpuSizes = (max: number) => [...CPU_SIZES, ...Array.from({ length: 62 }, (_, i) => (i + 3) * 1000)].filter((c) => c <= max)
// A name starts with a letter: a host that starts with a digit is a sandbox's.
const NAME_RE = /^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/
const MAX_NAME = 40
// The platform's machines pull the image from the host it names: a DNS name, never an address or localhost.
const imageHostOk = (image: string) => {
  const host = registryHost(image).replace(/:\d+$/, "")
  return host !== "localhost" && !/^\d+(\.\d+){3}$/.test(host) && !host.includes("[") && !host.includes(":")
}

const sizeOf = (f: { cpuMillis: number; memoryMi: number }) => `${f.cpuMillis / 1000} vCPU · ${f.memoryMi} MiB`

async function copy(label: string, value: string) {
  try {
    await navigator.clipboard.writeText(value)
    toast.success(`${label} copied`)
  } catch {
    toast.error(`Couldn't copy ${label}: select and copy it manually`)
  }
}

// "KEY=value" lines ↔ the env map. undefined = a line that is not KEY=value.
function parseEnv(text: string): Record<string, string> | undefined {
  const env: Record<string, string> = {}
  for (const line of text.split("\n")) {
    if (!line.trim()) continue
    const i = line.indexOf("=")
    const key = line.slice(0, i).trim()
    if (i < 1 || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) return undefined
    env[key] = line.slice(i + 1)
  }
  return env
}
const envText = (env?: Record<string, string>) => Object.entries(env ?? {}).map(([k, v]) => `${k}=${v}`).join("\n")

// Secrets are write-only: the API returns their names, never a value. SecretChanges is what the
// next save sends and the API merges: a string sets or replaces that name, null removes it.
// A typed value lives in the state of the dialog that holds it and nowhere else: it is sent from
// there (never as the variables of a mutation, which the mutation cache keeps) and is gone when
// the save succeeds or the dialog closes.
type SecretChanges = Record<string, string | null>
const secretsAfter = (names: string[] = [], changes: SecretChanges) =>
  [...new Set([...names, ...Object.keys(changes)])].filter((n) => changes[n] !== null).sort()
// The name and value being typed, before "Add secret" makes them a change. The dialog holds them
// so that it can refuse to save while they are pending: a save sends the changes only.
type SecretDraft = { name: string; value: string }
const NO_DRAFT: SecretDraft = { name: "", value: "" }
const drafted = (d: SecretDraft) => !!(d.name || d.value)
const secretClash = (names: string[], env?: Record<string, string>) => {
  const n = names.find((x) => env && x in env)
  return n && `${n} is both an environment variable and a secret`
}

function SecretsEditor({
  id, names = [], setAt, changes, onChange, draft, onDraft, error, disabled,
}: {
  id: string
  names?: string[]
  setAt?: Record<string, string>
  changes: SecretChanges
  onChange: (c: SecretChanges) => void
  draft: SecretDraft
  onDraft: (d: SecretDraft) => void
  error?: string
  disabled?: boolean
}) {
  const { name, value } = draft
  const without = (n: string) => Object.fromEntries(Object.entries(changes).filter(([k]) => k !== n))
  const all = [...new Set([...names, ...Object.keys(changes)])].sort()
  const known = all.includes(name)
  const nameOk = /^[A-Za-z_][A-Za-z0-9_]*$/.test(name) && name !== "PORT"
  return (
    <div className="grid gap-2">
      <Label htmlFor={`${id}-name`}>Secrets</Label>
      {all.length > 0 && (
        <ul className="grid gap-1" aria-label="Secrets">
          {all.map((n) => {
            const stored = names.includes(n)
            const state = changes[n] === null ? "will be removed" : n in changes ? (stored ? "will be replaced" : "new") : setAt?.[n] ? `set ${timeAgo(setAt[n])}` : ""
            return (
              <li key={n} className="flex items-center gap-2 rounded-md border px-3 py-1.5 text-sm">
                <span className={changes[n] === null ? "font-mono text-xs line-through" : "font-mono text-xs"}>{n}</span>
                <span className="flex-1 text-xs text-muted-foreground">{state}</span>
                {changes[n] === null ? (
                  <Button variant="ghost" size="sm" disabled={disabled} onClick={() => onChange(without(n))} aria-label={`Keep ${n}`}>Keep</Button>
                ) : (
                  <>
                    <Button variant="ghost" size="sm" disabled={disabled} onClick={() => { onDraft({ ...draft, name: n }); document.getElementById(`${id}-value`)?.focus() }} aria-label={`Replace ${n}`}>Replace</Button>
                    <Button variant="ghost" size="sm" disabled={disabled} onClick={() => onChange(stored ? { ...changes, [n]: null } : without(n))} aria-label={`Remove ${n}`}>Remove</Button>
                  </>
                )}
              </li>
            )
          })}
        </ul>
      )}
      <div className="grid grid-cols-[1fr_1fr_auto] gap-2">
        <Input id={`${id}-name`} className="font-mono text-sm" value={name} onChange={(e) => onDraft({ ...draft, name: e.target.value })} placeholder="NAME" aria-label="Secret name" autoComplete="off" disabled={disabled} />
        {/* Never pre-filled: a stored value cannot be read back, and a typed one is cleared once added. */}
        <Input id={`${id}-value`} type="password" autoComplete="new-password" value={value} onChange={(e) => onDraft({ ...draft, value: e.target.value })} placeholder="Value" aria-label="Secret value" disabled={disabled} />
        <Button
          variant="outline"
          disabled={disabled || !nameOk || !value}
          onClick={() => { onChange({ ...changes, [name]: value }); onDraft(NO_DRAFT) }}
        >
          {known ? "Replace secret" : "Add secret"}
        </Button>
      </div>
      {drafted(draft) && (
        <p className="text-xs" role="status">
          Not added yet: choose {known ? "Replace secret" : "Add secret"}, or clear the name and value, to continue.
        </p>
      )}
      <p className={error ? "text-xs text-destructive" : "text-xs text-muted-foreground"} role={error ? "alert" : undefined}>
        {error ?? "Given to the function as environment variables. A value cannot be viewed again after it is saved, only replaced or removed. A rollback uses the current secrets, not the ones of the earlier deployment. The function itself can read them, and so can print them: keep them out of its output."}
      </p>
    </div>
  )
}

// Credentials of the private registry an image is pulled from. The password is write-only: the
// API returns the server and the username, never the password. RegistryChange is what the next
// save does: undefined keeps what is stored (or sets none), null removes it, an object sets or
// replaces it, with the password every time.
type RegistryChange = { server: string; username: string; password: string } | null | undefined
const NO_REGISTRY = { server: "", username: "", password: "" }
const registryOk = (c: RegistryChange) => !c || (!!c.username.trim() && !!c.password)
// What a save sends as registryAuth. An empty server is left to the API: the image's registry.
const registryBody = (c: NonNullable<RegistryChange>) => ({ ...(c.server.trim() ? { server: c.server.trim() } : {}), username: c.username.trim(), password: c.password })

function RegistryEditor({
  image, stored, change, onChange,
}: {
  image: string
  stored?: CloudFunction["registryAuth"]
  change: RegistryChange
  onChange: (c: RegistryChange) => void
}) {
  const host = registryHost(image)
  const c = change ?? NO_REGISTRY
  // Without stored credentials, clearing every field means none; with them, only Keep goes back.
  const set = (patch: Partial<typeof NO_REGISTRY>) => {
    const next = { ...c, ...patch }
    onChange(!stored && !next.server && !next.username && !next.password ? undefined : next)
  }
  return (
    <fieldset className="grid gap-2">
      <legend className="mb-2 text-sm font-medium">Private registry</legend>
      {stored && !change ? (
        <div className="flex items-center gap-2 rounded-md border px-3 py-1.5 text-sm">
          <span className={change === null ? "min-w-0 truncate font-mono text-xs line-through" : "min-w-0 truncate font-mono text-xs"}>{stored.server} · {stored.username}</span>
          <span className="flex-1 text-xs text-muted-foreground">{change === null ? "will be removed" : stored.setAt ? `set ${timeAgo(stored.setAt)}` : ""}</span>
          {change === null ? (
            <Button variant="ghost" size="sm" onClick={() => onChange(undefined)} aria-label="Keep registry credentials">Keep</Button>
          ) : (
            <>
              {/* The stored server only while the image is still in that registry: for another one it would be refused. */}
              <Button variant="ghost" size="sm" onClick={() => onChange({ server: serverHost(stored.server) === host ? stored.server : "", username: stored.username, password: "" })} aria-label="Replace registry credentials">Replace</Button>
              <Button variant="ghost" size="sm" onClick={() => onChange(null)} aria-label="Remove registry credentials">Remove</Button>
            </>
          )}
        </div>
      ) : (
        <div className="grid grid-cols-[1fr_1fr_1fr_auto] gap-2">
          <Input className="font-mono text-sm" value={c.server} onChange={(e) => set({ server: e.target.value })} placeholder={host === "docker.io" ? "https://index.docker.io/v1/" : host} aria-label="Registry server" autoComplete="off" />
          <Input value={c.username} onChange={(e) => set({ username: e.target.value })} placeholder="Username" aria-label="Registry username" autoComplete="off" />
          {/* Never pre-filled: a stored password cannot be read back. */}
          <Input type="password" autoComplete="new-password" value={c.password} onChange={(e) => set({ password: e.target.value })} placeholder="Password" aria-label="Registry password" />
          {stored && <Button variant="ghost" onClick={() => onChange(undefined)} aria-label="Keep registry credentials">Keep</Button>}
        </div>
      )}
      <p className={registryOk(change) ? "text-xs text-muted-foreground" : "text-xs text-destructive"} role={registryOk(change) ? undefined : "alert"}>
        {registryOk(change)
          ? "Only for an image the registry does not serve without a login. The platform does not verify them, and a deployment that works does not prove them: a public image can deploy also with wrong credentials. Leave the server empty to use the registry of the image. The password cannot be viewed again after it is saved, only replaced or removed. A rollback uses the current credentials."
          : stored ? "Enter the username and the password: a stored password cannot be read back, so it is needed every time. Or choose Keep." : "Enter the username and the password, or clear all three fields."}
      </p>
    </fieldset>
  )
}

// What a customer should know before relying on a function: who reaches it, who pays, what a
// tag, the certificate, a cookie and a secret mean here.
function Notes({ limits }: { limits: FunctionLimits }) {
  const parent = limits.domain || "the platform's function domain"
  return (
    <section className="mt-8 grid gap-3" aria-labelledby="fn-notes">
      <h2 id="fn-notes" className="text-sm font-medium">Before you rely on a function</h2>
      <dl className="grid gap-3 text-sm md:grid-cols-2">
        {[
          ["Who can call it", "The URL is internal: it answers on the platform's network, not on the public internet. There is no login in front of it. Whoever can reach that address and knows the URL can call the function, and the six characters after the name are not a secret. If callers must prove who they are, check a header or a token in the function itself. On most installations a sandbox or another function cannot call it."],
          ["Callers cost you", "Every request keeps the function awake, and under load it runs up to its maximum instances. Whoever sends the requests, this project pays for the instances that run. A new function runs one instance at most: raise the maximum only when you need it."],
          ["What is billed", "Each instance, per minute, while it runs: every running period is rounded up to whole minutes. That includes the idle wait before the function stops, the start of an instance that went on to serve, and the larger of the two sizes while a deployment replaces one with the other. Nothing is billed while the function is idle or suspended. A start that never becomes ready (the image cannot be pulled, there is no room, the program crashes at start) is stopped within about two minutes and bills nothing."],
          ["Image tags", "A tag can move. A function deployed by tag pulls the tag again whenever an instance starts, so a later instance can run another image than the first one. Deploy by digest (repository@sha256:…) to run exactly one image. A rollback always deploys the image that deployment ran, by its digest."],
          ["The certificate", "The URL is served with the platform's own certificate. Where your operator did not install a publicly trusted one, a client must trust the platform's certificate authority before it can call the function. Ask your operator for it."],
          ["Cookies", `Every function of every project is served under ${parent}. Never set a cookie with that parent as its Domain: every other function would receive it. Leave the Domain attribute out, so a cookie stays with your function's own host.`],
          ["Secrets and logs", "A function can read its secrets, so it can print them, and what it prints is in its logs. Every member who can view this project's resources can read the logs, read-only members too. On the platform's cluster a secret is kept as a plain Kubernetes Secret. Where functions of several projects run on the same machines, a break-out of the sandbox runtime on a machine could read the secrets of the functions running there."],
        ].map(([term, text]) => (
          <div key={term} className="grid content-start gap-1 rounded-md border p-3">
            <dt className="font-medium">{term}</dt>
            <dd className="text-xs text-muted-foreground">{text}</dd>
          </div>
        ))}
      </dl>
    </section>
  )
}

export default function FunctionsPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const services = useProjectServices(pid)
  const limits = functionLimits(services.data)
  const list = useQuery({
    queryKey: ["functions", pid],
    queryFn: () => apiFetch<CloudFunction[]>(`/project/${pid}/functions`),
    enabled: !!pid && !!limits,
    // A rollout settles in seconds, and instances come and go with traffic.
    refetchInterval: 5000,
  })
  const invalidate = () => void qc.invalidateQueries({ queryKey: ["functions", pid] })

  // Dialogs hold the function's id, not the row, so they follow the polled list.
  const [createOpen, setCreateOpen] = useState(false)
  const [editId, setEditId] = useState<string | null>(null)
  const [detail, setDetail] = useState<{ id: string; tab: string } | null>(null)
  const [toDelete, setToDelete] = useState<CloudFunction | null>(null)
  const byId = (id?: string | null) => list.data?.find((f) => f.id === id)

  const action = useMutation({
    mutationFn: ({ f, action }: { f: CloudFunction; action: "suspend" | "resume" }) =>
      apiFetch(`/project/${pid}/functions/${f.id}/action`, { method: "POST", body: { action } }),
    onSuccess: (_d, { action }) => {
      toast.success(action === "suspend" ? "Function suspended" : "Function resumed")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const del = useMutation({
    mutationFn: (id: string) => apiFetch(`/project/${pid}/functions/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Function deleted")
      setToDelete(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudFunction, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (f) => f.name,
        header: sortableHeader("Name"),
        cell: ({ row, getValue }) => (
          <div className="grid">
            <span className="flex items-center gap-2 font-medium">
              <SquareFunction className="size-3.5 text-muted-foreground" aria-hidden="true" />
              {getValue()}
            </span>
            <span className="font-mono text-xs text-muted-foreground">{row.original.id}</span>
          </div>
        ),
      },
      {
        id: "url",
        accessorFn: (f) => f.url,
        header: sortableHeader("URL"),
        cell: ({ row, getValue }) => (
          <span className="flex items-center gap-1">
            <span className="font-mono text-xs">{getValue()}</span>
            <Button variant="ghost" size="icon-sm" onClick={() => void copy("URL", getValue())} aria-label={`Copy URL of ${row.original.name}`}>
              <Copy className="size-3.5" aria-hidden="true" />
            </Button>
          </span>
        ),
      },
      {
        id: "status",
        accessorFn: (f) => f.status,
        header: sortableHeader("Status"),
        cell: ({ row, getValue }) => (
          <span className="flex items-center gap-2">
            <StatusBadge status={getValue()} />
            {row.original.instances > 1 && <Badge variant="outline">{row.original.instances} instances</Badge>}
            {row.original.lastError && row.original.status !== "FAILED" && <Badge variant="outline">last deploy failed</Badge>}
          </span>
        ),
      },
      {
        id: "size",
        accessorFn: (f) => f.cpuMillis,
        header: sortableHeader("Size"),
        cell: ({ row }) => <span className="text-sm">{sizeOf(row.original)}</span>,
      },
      {
        id: "deployed",
        accessorFn: (f) => f.currentDeployment?.deployedAt ?? "",
        header: sortableHeader("Last deploy"),
        cell: ({ row, getValue }) => (
          <span className="text-sm text-muted-foreground">
            {row.original.currentDeployment ? `#${row.original.currentDeployment.seq} · ${timeAgo(getValue())}` : "—"}
          </span>
        ),
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const f = row.original
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${f.name}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => setDetail({ id: f.id, tab: "overview" })}>
                    <Info className="size-4" /> Details and deployments
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => setDetail({ id: f.id, tab: "logs" })}>
                    <ScrollText className="size-4" /> Logs
                  </DropdownMenuItem>
                  <DropdownMenuItem disabled={f.status === "DEPLOYING"} onClick={() => setEditId(f.id)}>
                    <Rocket className="size-4" /> Deploy new version
                  </DropdownMenuItem>
                  <DropdownMenuItem disabled={action.isPending} onClick={() => action.mutate({ f, action: f.suspended ? "resume" : "suspend" })}>
                    {f.suspended ? <><Play className="size-4" /> Resume</> : <><Pause className="size-4" /> Suspend</>}
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem variant="destructive" onClick={() => setToDelete(f)}>
                    <Trash2 className="size-4" /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    [action],
  )

  // The shell hides this page where no location runs functions; nothing to draw until the limits are known.
  if (!limits) return null

  const full = (list.data?.length ?? 0) >= limits.maxFunctions
  const editing = byId(editId)
  return (
    <>
      <PageHeader
        title="Functions"
        eyebrow="Platform"
        description="Run a container image behind a stable internal URL. A function stops when it is idle and starts again on the next request; it is billed per minute only while an instance runs."
        actions={
          <>
            <Button variant="outline" onClick={() => list.refetch()} disabled={list.isFetching} aria-label="Refresh">
              <RefreshCw className={list.isFetching ? "animate-spin" : ""} aria-hidden="true" />
            </Button>
            <Button onClick={() => setCreateOpen(true)} disabled={full}>
              <Plus aria-hidden="true" /> New function
            </Button>
          </>
        }
      />
      {list.data && list.data.length === 0 ? (
        <EmptyState
          icon={SquareFunction}
          title="No functions"
          hint="Deploy a container image that serves HTTP. It gets its own internal URL and costs nothing while idle."
          action={<Button onClick={() => setCreateOpen(true)}><Plus aria-hidden="true" /> New function</Button>}
        />
      ) : (
        <DataTable columns={columns} data={list.data ?? []} isLoading={list.isLoading} error={list.error as Error | null} />
      )}
      <Notes limits={limits} />

      {(createOpen || editing) && (
        // Mounted only while open, so each opening starts fresh: from the function's current
        // settings, or empty for a new one (not with the previous function's image, env and secrets).
        <FunctionDialog
          key={editing?.id ?? "new"}
          open
          onClose={() => { setCreateOpen(false); setEditId(null) }}
          pid={pid}
          limits={limits}
          fn={editing}
          onSaved={invalidate}
        />
      )}
      <DetailDialog
        // Keyed by the function shown: secret values typed for one are dropped when the dialog
        // closes or its function goes away (deleted elsewhere), never offered for the next one.
        key={byId(detail?.id)?.id ?? "none"}
        pid={pid}
        limits={limits}
        fn={byId(detail?.id)}
        tab={detail?.tab ?? "overview"}
        onTab={(tab) => detail && setDetail({ ...detail, tab })}
        onClose={() => setDetail(null)}
        onChanged={invalidate}
      />

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete function?</DialogTitle>
            <DialogDescription>
              {toDelete ? `"${toDelete.name}" stops now and ${toDelete.url} stops answering. Its deployment history is removed. The URL is not given out again.` : ""}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>Cancel</Button>
            <Button variant="destructive" disabled={del.isPending} onClick={() => toDelete && del.mutate(toDelete.id)}>
              {del.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// Create (no fn) or deploy a new version (fn). An update sends only what changed: image, size,
// port or environment roll out as a new deployment, scale settings apply without one.
function FunctionDialog({
  open, onClose, pid, limits, fn, onSaved,
}: {
  open: boolean
  onClose: () => void
  pid: string
  limits: FunctionLimits
  fn?: CloudFunction
  onSaved: () => void
}) {
  const [name, setName] = useState("")
  const [image, setImage] = useState(fn?.image ?? "")
  const [port, setPort] = useState(String(fn?.port ?? 8080))
  const [cpu, setCpu] = useState(String(fn?.cpuMillis ?? CPU_SIZES[0]))
  const [mem, setMem] = useState(String(fn?.memoryMi ?? 256))
  const [env, setEnv] = useState(envText(fn?.env))
  const [idle, setIdle] = useState(String(fn?.idleSeconds ?? limits.idleSeconds))
  const [keepOne, setKeepOne] = useState((fn?.minInstances ?? 0) > 0)
  // One instance unless asked otherwise: callers, not the owner, decide how many run under load.
  // A stored maximum above what the location allows now is shown as that cap, and saved as it.
  const [maxInst, setMaxInst] = useState(String(Math.min(fn?.maxInstances ?? 1, limits.maxInstances)))
  const [conc, setConc] = useState(String(fn?.concurrency ?? limits.defaultConcurrency))
  const [secrets, setSecrets] = useState<SecretChanges>({})
  const [draft, setDraft] = useState(NO_DRAFT)
  const [registry, setRegistry] = useState<RegistryChange>()

  const portN = Number(port), memN = Number(mem), idleN = Number(idle), maxN = Number(maxInst), concN = Number(conc)
  const envMap = parseEnv(env)
  const clash = secretClash(secretsAfter(fn?.secretNames, secrets), envMap)
  const nameOk = !!fn || NAME_RE.test(name)
  const img = image.trim()
  const imageOk = !!img && !/\s/.test(img) && imageHostOk(img)
  const valid =
    nameOk && imageOk &&
    Number.isInteger(portN) && portN >= 1 && portN <= 65535 &&
    Number.isInteger(memN) && memN >= 256 && memN <= limits.maxMemoryMi &&
    Number.isInteger(idleN) && idleN >= limits.minIdleSeconds && idleN <= limits.maxIdleSeconds &&
    Number.isInteger(maxN) && maxN >= 1 && maxN <= limits.maxInstances &&
    Number.isInteger(concN) && concN >= 1 && concN <= limits.maxConcurrency &&
    !!envMap && !("PORT" in envMap) && !clash && registryOk(registry)

  const save = useMutation({
    // No variables: the secrets and the registry password are read from this dialog's state, so
    // the mutation cache never holds them.
    mutationFn: async () => {
      const body: Record<string, unknown> = {
        image: img, port: portN, cpuMillis: Number(cpu), memoryMi: memN,
        env: envMap, idleSeconds: idleN, minInstances: keepOne ? 1 : 0, maxInstances: maxN, concurrency: concN,
      }
      const changed: Record<string, unknown> = Object.keys(secrets).length ? { secrets } : {}
      // null removes the stored credentials; left out, they are kept.
      if (registry !== undefined) changed.registryAuth = registry && registryBody(registry)
      if (!fn) return apiFetch(`/project/${pid}/functions`, { method: "POST", body: { name, ...body, ...changed } })
      const was: Record<string, unknown> = { ...fn, env: fn.env ?? {} }
      for (const k of Object.keys(body)) {
        if (JSON.stringify(body[k]) === JSON.stringify(was[k])) delete body[k]
      }
      return apiFetch(`/project/${pid}/functions/${fn.id}`, { method: "PATCH", body: { ...body, ...changed } })
    },
    onSuccess: () => {
      toast.success(fn ? "Function updated" : "Function deploying")
      onClose()
      onSaved()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{fn ? `Deploy new version of ${fn.name}` : "New function"}</DialogTitle>
          <DialogDescription>
            {fn
              ? "A change to the image, registry credentials, size, port, environment or secrets rolls out as a new deployment. If it does not become ready, the function stays on its current one."
              : "The image must serve HTTP on the port below. Billed per minute while an instance runs; nothing while the function is idle."}
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          {!fn && (
            <div className="grid gap-2">
              <Label htmlFor="fn-name">Name</Label>
              <Input id="fn-name" value={name} maxLength={MAX_NAME} onChange={(e) => setName(e.target.value)} placeholder="hello-api" aria-describedby="fn-name-hint" />
              <p id="fn-name-hint" className="text-xs text-muted-foreground">
                Lowercase letters, digits and "-", starting with a letter. It starts the URL (<span className="font-mono">{name && nameOk ? name : "name"}-xxxxxx{limits.domain && `.${limits.domain}`}</span>) and cannot be changed.
              </p>
            </div>
          )}
          <div className="grid gap-2">
            <Label htmlFor="fn-image">Image</Label>
            <Input id="fn-image" className="font-mono text-sm" value={image} onChange={(e) => setImage(e.target.value)} placeholder="registry.example.com/acme/hello@sha256:…" aria-describedby="fn-image-hint" aria-invalid={!!img && !imageOk} />
            <p id="fn-image-hint" className={!img || imageOk ? "text-xs text-muted-foreground" : "text-xs text-destructive"}>
              {!img || imageOk
                ? "Any image the platform's machines can pull. A tag can move: a later instance may then run another image than the first. Use a digest to run exactly one image."
                : "The registry must be named by a DNS name: not an IP address, not localhost, and no spaces in the reference."}
            </p>
            <RegistryEditor image={image} stored={fn?.registryAuth} change={registry} onChange={setRegistry} />
          </div>
          <div className="grid grid-cols-3 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="fn-cpu">CPU</Label>
              <Select value={cpu} onValueChange={setCpu}>
                <SelectTrigger id="fn-cpu"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {cpuSizes(limits.maxCpuMillis).map((c) => (
                    <SelectItem key={c} value={String(c)}>{c / 1000} vCPU</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="fn-mem">Memory (MiB)</Label>
              <Input id="fn-mem" inputMode="numeric" value={mem} onChange={(e) => setMem(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="fn-port">Port</Label>
              <Input id="fn-port" inputMode="numeric" value={port} onChange={(e) => setPort(e.target.value)} />
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="fn-env">Environment variables</Label>
            <Textarea id="fn-env" className="min-h-20 font-mono text-sm" value={env} onChange={(e) => setEnv(e.target.value)} placeholder={"LOG_LEVEL=info\nAPI_BASE=https://example.com"} aria-describedby="fn-env-hint" />
            <p id="fn-env-hint" className={envMap && !("PORT" in envMap) ? "text-xs text-muted-foreground" : "text-xs text-destructive"}>
              One NAME=value per line. PORT is set for you from the port above. Members who manage this project's resources can read these values: put anything confidential in a secret.
            </p>
          </div>
          <SecretsEditor id="fn-secret" names={fn?.secretNames} setAt={fn?.secretsSetAt} changes={secrets} onChange={setSecrets} draft={draft} onDraft={setDraft} error={clash} />
          <div className="grid gap-2">
            <Label htmlFor="fn-idle">Stop after idle (seconds)</Label>
            <Input id="fn-idle" inputMode="numeric" value={idle} onChange={(e) => setIdle(e.target.value)} aria-describedby="fn-idle-hint" />
            <p id="fn-idle-hint" className="text-xs text-muted-foreground">
              {limits.minIdleSeconds}–{limits.maxIdleSeconds}. The time without a request before the instance stops; this wait is billed.
            </p>
          </div>
          {limits.minInstances >= 1 ? (
            <p className="text-xs text-muted-foreground">This location keeps one instance of every function running, so functions here are billed continuously.</p>
          ) : limits.maxMinInstances >= 1 && (
            <div className="flex items-center justify-between gap-3">
              <div className="grid gap-1">
                <Label htmlFor="fn-keep">Keep one instance running</Label>
                <p className="text-xs text-muted-foreground">No cold start, but the function is billed continuously.</p>
              </div>
              <Switch id="fn-keep" checked={keepOne} onCheckedChange={setKeepOne} />
            </div>
          )}
          {limits.maxInstances > 1 && (
            <div className="grid grid-cols-2 gap-4">
              <div className="grid gap-2">
                <Label htmlFor="fn-max">Maximum instances</Label>
                <Input id="fn-max" inputMode="numeric" value={maxInst} onChange={(e) => setMaxInst(e.target.value)} aria-describedby="fn-scale-hint" />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="fn-conc">Requests per instance</Label>
                <Input id="fn-conc" inputMode="numeric" value={conc} onChange={(e) => setConc(e.target.value)} aria-describedby="fn-scale-hint" />
              </div>
              <p id="fn-scale-hint" className="col-span-2 text-xs text-muted-foreground">
                Maximum instances: 1–{limits.maxInstances}. Requests per instance: 1–{limits.maxConcurrency}. Under load the function runs one instance for every {Number.isInteger(concN) && concN > 0 ? concN : "so many"} requests in flight, up to the maximum, and steps back down when the load drops. Every running instance is billed, whoever sends the requests: anyone who can reach the URL can drive the function to its maximum. An instance is never refused a request: this is a target, not a limit.
              </p>
            </div>
          )}
          <p className="text-xs text-muted-foreground">
            Limits: up to {limits.maxCpuMillis / 1000} vCPU, {limits.maxMemoryMi} MiB; {limits.maxFunctions} functions per project.
          </p>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button disabled={!valid || drafted(draft) || save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? "Deploying…" : fn ? "Deploy" : "Create function"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[9rem_1fr] gap-2 text-sm">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </div>
  )
}

function DetailDialog({
  pid, limits, fn, tab, onTab, onClose, onChanged,
}: {
  pid: string
  limits: FunctionLimits
  fn?: CloudFunction
  tab: string
  onTab: (tab: string) => void
  onClose: () => void
  onChanged: () => void
}) {
  const qc = useQueryClient()
  const id = fn?.id
  // The single read carries what the list does not: what an unfinished rollout is waiting for.
  const live = useQuery({
    queryKey: ["functions", pid, id],
    queryFn: () => apiFetch<CloudFunction>(`/project/${pid}/functions/${id}`),
    enabled: !!id,
    refetchInterval: 3000,
  })
  const deployments = useQuery({
    queryKey: ["functions", pid, id, "deployments"],
    queryFn: () => apiFetch<FunctionDeployment[]>(`/project/${pid}/functions/${id}/deployments`),
    enabled: !!id && tab === "deployments",
    refetchInterval: 3000,
  })
  const logs = useQuery({
    queryKey: ["functions", pid, id, "logs"],
    queryFn: () => apiFetch<{ logs: string }>(`/project/${pid}/functions/${id}/logs?tail=200`),
    enabled: !!id && tab === "logs",
  })
  const f = live.data?.id === id ? live.data : fn
  // The stored credentials are for another registry than this deployment's image: they cannot
  // pull it, and the API refuses the rollback unless it says what becomes of them.
  const otherRegistry = (d: FunctionDeployment) => !!f?.registryAuth && serverHost(f.registryAuth.server) !== registryHost(d.image)
  const changed = () => {
    void qc.invalidateQueries({ queryKey: ["functions", pid, id] })
    onChanged()
  }
  const rollback = useMutation({
    mutationFn: (d: FunctionDeployment) =>
      apiFetch(`/project/${pid}/functions/${id}/rollback`, { method: "POST", body: { deploymentId: d.id, ...(otherRegistry(d) ? { registryAuth: null } : {}) } }),
    onSuccess: (_r, d) => {
      toast.success(`Rolling back to deployment #${d.seq}`)
      changed()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const busy = f?.status === "DEPLOYING"
  // Typed values are dropped with the dialog, which is keyed by its function.
  const [secrets, setSecrets] = useState<SecretChanges>({})
  const [draft, setDraft] = useState(NO_DRAFT)
  const clash = secretClash(secretsAfter(f?.secretNames, secrets), f?.env)
  const saveSecrets = useMutation({
    // No variables: the values are read from this dialog's state, so the mutation cache never holds them.
    mutationFn: () => apiFetch(`/project/${pid}/functions/${id}`, { method: "PATCH", body: { secrets } }),
    onSuccess: () => {
      toast.success("Secrets saved, deploying")
      setSecrets({})
      changed()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  // What applies now, not what is stored: the platform's floor and bounds override the
  // function's own settings (and may have changed since they were saved).
  const floor = f ? Math.max(limits.minInstances, Math.min(f.minInstances, limits.maxMinInstances)) : 0
  const idleFor = f ? Math.min(Math.max(f.idleSeconds, limits.minIdleSeconds), limits.maxIdleSeconds) : 0
  const maxFor = f ? Math.min(f.maxInstances, limits.maxInstances) : 1

  return (
    <Dialog open={!!fn} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>{f?.name}</DialogTitle>
          <DialogDescription className="font-mono text-xs">{f?.url}</DialogDescription>
        </DialogHeader>
        {f && (
          <Tabs value={tab} onValueChange={onTab}>
            <TabsList>
              <TabsTrigger value="overview">Overview</TabsTrigger>
              <TabsTrigger value="secrets">Secrets</TabsTrigger>
              <TabsTrigger value="deployments">Deployments</TabsTrigger>
              <TabsTrigger value="logs">Logs</TabsTrigger>
            </TabsList>
            <TabsContent value="overview">
              <dl className="grid gap-2">
                <Field label="Status">
                  <span className="flex flex-wrap items-center gap-2">
                    <StatusBadge status={f.status} />
                    {f.reason && <span className="text-muted-foreground">{f.reason}</span>}
                  </span>
                </Field>
                {f.lastError && (
                  <Field label="Last deploy failed"><span className="text-destructive" role="status">{f.lastError}</span></Field>
                )}
                <Field label="URL">
                  <span className="flex flex-wrap items-center gap-1">
                    <span className="font-mono text-xs">{f.url}</span>
                    <Button variant="ghost" size="icon-sm" onClick={() => void copy("URL", f.url)} aria-label="Copy the URL">
                      <Copy className="size-3.5" aria-hidden="true" />
                    </Button>
                  </span>
                  <span className="block text-xs text-muted-foreground">Internal, with no login in front of it: whoever reaches the address can call it.</span>
                </Field>
                <Field label="Instances">
                  {f.instances} running
                  {maxFor > 1 && <span className="text-muted-foreground"> · up to {maxFor}, one for every {f.concurrency} requests in flight</span>}
                </Field>
                <Field label="Image"><span className="font-mono text-xs">{f.image}</span></Field>
                <Field label="Private registry">
                  {f.registryAuth ? <><span className="font-mono text-xs">{f.registryAuth.server}</span> · {f.registryAuth.username}<span className="text-muted-foreground"> · password set {timeAgo(f.registryAuth.setAt)}</span></> : "—"}
                </Field>
                <Field label="Size">{sizeOf(f)}</Field>
                <Field label="Port">{f.port}</Field>
                <Field label="Stops after idle">{floor > 0 ? "Never: one instance is kept running" : `${idleFor} s`}</Field>
                <Field label="Environment">
                  {Object.keys(f.env ?? {}).length ? <span className="font-mono text-xs">{Object.keys(f.env!).join(", ")}</span> : "—"}
                </Field>
                <Field label="Secrets">
                  {f.secretNames?.length ? <span className="font-mono text-xs">{f.secretNames.join(", ")}</span> : "—"}
                </Field>
                <Field label="Current deployment">
                  {f.currentDeployment ? `#${f.currentDeployment.seq}, deployed ${timeAgo(f.currentDeployment.deployedAt)}` : "None yet"}
                </Field>
                <Field label="Last request">{timeAgo(f.lastActiveAt)}</Field>
              </dl>
            </TabsContent>
            <TabsContent value="secrets" className="grid gap-3">
              <SecretsEditor id="fn-detail-secret" names={f.secretNames} setAt={f.secretsSetAt} changes={secrets} onChange={setSecrets} draft={draft} onDraft={setDraft} error={clash} disabled={busy || saveSecrets.isPending} />
              <div className="flex items-center justify-between gap-2">
                <p className="text-xs text-muted-foreground">
                  {busy ? "A deployment is in progress. The list shows the secrets that are serving until it finishes." : "Saving rolls out a new deployment. If it does not become ready, the function keeps its current secrets."}
                </p>
                <Button disabled={busy || saveSecrets.isPending || !!clash || drafted(draft) || !Object.keys(secrets).length} onClick={() => saveSecrets.mutate()}>
                  {saveSecrets.isPending ? "Saving…" : "Save and deploy"}
                </Button>
              </div>
            </TabsContent>
            <TabsContent value="deployments" className="grid gap-2">
              {deployments.error ? (
                <p className="text-sm text-destructive" role="alert">{(deployments.error as Error).message}</p>
              ) : deployments.isLoading ? (
                <p className="text-sm text-muted-foreground">Loading…</p>
              ) : (
                <ul className="grid max-h-96 gap-2 overflow-auto" aria-label="Deployments">
                  {(deployments.data ?? []).map((d) => (
                    <li key={d.id} className="grid grid-cols-[1fr_auto] items-center gap-3 rounded-md border p-3">
                      <div className="grid min-w-0 gap-1">
                        <span className="flex flex-wrap items-center gap-2 text-sm font-medium">
                          #{d.seq}
                          <StatusBadge status={d.state} />
                          {d.rollbackOf ? <Badge variant="outline">rollback to #{d.rollbackOf}</Badge> : null}
                          {d.id === f.currentDeployment?.id && <Badge variant="secondary">serving</Badge>}
                        </span>
                        <span className="truncate font-mono text-xs text-muted-foreground" title={d.image}>{d.image}</span>
                        {d.imageDigest && !d.image.includes("@") && <span className="truncate font-mono text-xs text-muted-foreground" title={d.imageDigest}>ran {d.imageDigest.slice(0, 19)}…</span>}
                        <span className="text-xs text-muted-foreground">{sizeOf(d)} · port {d.port} · {timeAgo(d.createdAt)}</span>
                        {d.message && <span className="text-xs text-destructive">{d.message}</span>}
                        {d.state === "SUPERSEDED" && otherRegistry(d) && <span className="text-xs text-muted-foreground">In another registry than the stored credentials ({serverHost(f.registryAuth?.server ?? "")}): rolling back removes them. To pull this image with credentials of its own, use Deploy new version.</span>}
                      </div>
                      {d.state === "SUPERSEDED" && (
                        <Button variant="outline" size="sm" disabled={busy || rollback.isPending} onClick={() => rollback.mutate(d)} aria-label={`Roll back to deployment ${d.seq}${otherRegistry(d) ? " without registry credentials" : ""}`}>
                          <Undo2 aria-hidden="true" /> {otherRegistry(d) ? "Roll back without credentials" : "Roll back"}
                        </Button>
                      )}
                    </li>
                  ))}
                </ul>
              )}
              <p className="text-xs text-muted-foreground">The 10 newest deployments are kept. Rolling back deploys an earlier one again, as a new deployment, with its size, port and environment and the image it ran (by digest, also where it was deployed by tag), but the function's current secrets and registry credentials.</p>
            </TabsContent>
            <TabsContent value="logs" className="grid gap-2">
              <div className="flex items-center justify-between gap-2">
                <p className="text-xs text-muted-foreground">Newest 200 lines of the running instance. Every member who can view this project's resources can read them: a function should never print its secrets.</p>
                <Button variant="outline" size="sm" onClick={() => logs.refetch()} disabled={logs.isFetching}>
                  <RefreshCw className={logs.isFetching ? "animate-spin" : ""} aria-hidden="true" /> Refresh
                </Button>
              </div>
              {logs.error ? (
                <p className="text-sm text-destructive" role="alert">{(logs.error as Error).message}</p>
              ) : (
                <pre className="max-h-96 min-h-24 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap" aria-label="Function logs" tabIndex={0}>
                  {logs.isLoading ? "Loading…" : logs.data?.logs || "No log lines. An idle function has no running instance to read logs from."}
                </pre>
              )}
            </TabsContent>
          </Tabs>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Close</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
