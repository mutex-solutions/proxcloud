// Sandboxes: isolated environments (gVisor) for running code, started in seconds and deleted at
// their time limit. Backend: SANDBOX resources of a `sandbox` provider (docs/managed-sandbox.md §3).
import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Clock, Cpu, FileText, MoreHorizontal, Plus, RefreshCw, SquareTerminal, Trash2 } from "lucide-react"
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch, type CloudScope } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useLocations, useProjectId, useProjectServices } from "@/lib/hooks"
import type { CloudResource, SandboxExecResult, SandboxLimits, SandboxTemplate } from "@/lib/types"
import { ApiKeysTab } from "./SandboxApiKeys"
import { TemplatesTab } from "./SandboxTemplates"

const TYPE = "SANDBOX"
const NO_GPU = "none"

function sbName(r: CloudResource): string {
  return (r.data?.name as string) || r.externalId || r.id
}

// "in 42 min" / "in 3 h" until the controller deletes it at shutdownTime.
function expiresIn(iso?: string): string {
  if (!iso) return "—"
  const ms = new Date(iso).getTime() - Date.now()
  if (ms <= 0) return "expiring"
  const min = Math.round(ms / 60000)
  return min < 90 ? `in ${min} min` : `in ${Math.round(min / 60)} h`
}

function useSandboxData(pid: string) {
  const locations = useLocations(pid)
  const services = useProjectServices(pid)
  const loc = (locations.data ?? []).find((l) => l.provider === "sandbox" && l.serviceId && l.region)
  const scope: CloudScope | undefined = loc?.serviceId && loc?.region ? { serviceId: loc.serviceId, region: loc.region } : undefined
  const svc = services.data?.find((s) => s.id === loc?.serviceId)
  return {
    scope,
    loading: locations.isLoading || services.isLoading,
    templates: svc?.sandboxTemplates ?? [],
    gpuModels: svc?.sandboxGpuModels ?? [],
    limits: (svc?.sandboxLimits ?? {}) as SandboxLimits,
    e2b: !!svc?.sandboxE2B,
  }
}

export default function SandboxesPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const { scope, loading, templates, gpuModels, limits, e2b } = useSandboxData(pid)
  const list = useQuery({
    queryKey: ["cloud", pid, TYPE],
    queryFn: () => apiFetch<CloudResource[]>(`/project/${pid}/resource?type=${TYPE}`, { method: "POST", cloud: scope }),
    enabled: !!pid && !!scope,
    // Sandboxes go PENDING → RUNNING in seconds and disappear at their time limit.
    refetchInterval: 5000,
    // An ended sandbox keeps a DELETED row for a few minutes for billing: not shown.
    select: (rows) => rows.filter((r) => r.data?.status !== "DELETED" && r.data?.kind !== "build"),
  })
  const invalidate = () => void qc.invalidateQueries({ queryKey: ["cloud", pid, TYPE] })

  const [createOpen, setCreateOpen] = useState(false)
  const [execFor, setExecFor] = useState<CloudResource | null>(null)
  const [logsFor, setLogsFor] = useState<CloudResource | null>(null)
  const [toDelete, setToDelete] = useState<CloudResource | null>(null)

  const del = useMutation({
    mutationFn: (id: string) => apiFetch(`/project/${pid}/cloud/${id}`, { method: "DELETE", cloud: scope }),
    onSuccess: () => {
      toast.success("Sandbox deleted")
      setToDelete(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  // SET_TIMEOUT sets the end to now + timeoutSeconds, so extending adds an hour to what is left
  // (capped at the location's maximum) instead of replacing it.
  const extend = useMutation({
    mutationFn: (r: CloudResource) => {
      const end = Date.parse((r.data?.shutdownTime as string) ?? "")
      const left = Number.isFinite(end) ? Math.max(0, Math.floor((end - Date.now()) / 1000)) : 0
      const max = limits.maxTimeoutSeconds ?? 86400
      const timeoutSeconds = Math.min(left + 3600, max)
      return apiFetch(`/project/${pid}/cloud/${r.id}/action`, {
        method: "POST",
        body: { action: "SET_TIMEOUT", data: { timeoutSeconds } },
        cloud: scope,
      }).then(() => timeoutSeconds === max && left + 3600 > max)
    },
    onSuccess: (capped) => {
      toast.success(capped ? "Sandbox extended to the maximum time limit" : "Sandbox extended by one hour")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudResource, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (r) => sbName(r),
        header: sortableHeader("Name"),
        cell: ({ row, getValue }) => (
          <div className="grid">
            <span className="flex items-center gap-2 font-medium">
              <SquareTerminal className="size-3.5 text-muted-foreground" aria-hidden="true" />
              {getValue()}
            </span>
            <span className="font-mono text-xs text-muted-foreground">{row.original.externalId}</span>
          </div>
        ),
      },
      {
        id: "template",
        accessorFn: (r) => (r.data?.template as string) ?? "",
        header: sortableHeader("Template"),
        cell: ({ getValue }) => <Badge variant="secondary">{getValue()}</Badge>,
      },
      {
        id: "size",
        accessorFn: (r) => Number(r.data?.cpu ?? 0),
        header: sortableHeader("Size"),
        cell: ({ row }) => {
          const d = row.original.data ?? {}
          return (
            <span className="text-sm">
              {String(d.cpu)} vCPU · {String(d.memoryGiB)} GiB · egress {String(d.egressBandwidth || "legacy: recreate to cap")}
              {Number(d.gpuCount) > 0 && (
                <Badge className="ml-2" variant="outline">
                  {String(d.gpuCount)}× {String(d.gpuModel)}
                </Badge>
              )}
            </span>
          )
        },
      },
      {
        id: "status",
        accessorFn: (r) => (r.data?.status as string) ?? "",
        header: sortableHeader("Status"),
        cell: ({ getValue }) => <StatusBadge status={getValue()} />,
      },
      {
        id: "expires",
        accessorFn: (r) => (r.data?.shutdownTime as string) ?? "",
        header: sortableHeader("Ends"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{expiresIn(getValue())}</span>,
      },
      {
        id: "started",
        accessorFn: (r) => (r.data?.startedAt as string) ?? r.createdAt ?? "",
        header: sortableHeader("Started"),
        cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{timeAgo(getValue())}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const r = row.original
          const running = r.data?.status === "RUNNING"
          return (
            <div className="text-right">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${sbName(r)}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem disabled={!running} onClick={() => setExecFor(r)}>
                    <SquareTerminal className="size-4" /> Run command
                  </DropdownMenuItem>
                  <DropdownMenuItem disabled={!running} onClick={() => setLogsFor(r)}>
                    <FileText className="size-4" /> Logs
                  </DropdownMenuItem>
                  <DropdownMenuItem disabled={extend.isPending} onClick={() => extend.mutate(r)}>
                    <Clock className="size-4" /> Extend by 1 hour
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem variant="destructive" onClick={() => setToDelete(r)}>
                    <Trash2 className="size-4" /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    [extend],
  )

  if (!loading && !scope) {
    return (
      <>
        <PageHeader title="Sandboxes" eyebrow="Platform" />
        <EmptyState icon={SquareTerminal} title="Sandboxes are not enabled" hint="This project is not attached to a sandbox location. Ask your administrator to enable sandboxes." />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Sandboxes"
        eyebrow="Platform"
        description="Isolated environments for running code. They start in seconds, are billed per minute while running, and are deleted when they reach their time limit."
        actions={
          <>
            <Button variant="outline" onClick={() => list.refetch()} disabled={list.isFetching} aria-label="Refresh">
              <RefreshCw className={list.isFetching ? "animate-spin" : ""} aria-hidden="true" />
            </Button>
            <Button onClick={() => setCreateOpen(true)} disabled={templates.length === 0}>
              <Plus aria-hidden="true" /> New sandbox
            </Button>
          </>
        }
      />
      <Tabs defaultValue="sandboxes">
        <TabsList>
          <TabsTrigger value="sandboxes">Sandboxes</TabsTrigger>
          {e2b && <TabsTrigger value="templates">Templates</TabsTrigger>}
          <TabsTrigger value="api-keys">API keys</TabsTrigger>
        </TabsList>
        <TabsContent value="sandboxes" className="grid gap-4">
          {list.data && list.data.length === 0 ? (
            <EmptyState
              icon={Cpu}
              title="No sandboxes"
              hint="Start one to run code in an isolated environment."
              action={<Button onClick={() => setCreateOpen(true)}><Plus aria-hidden="true" /> New sandbox</Button>}
            />
          ) : (
            <DataTable columns={columns} data={list.data ?? []} isLoading={list.isLoading} error={list.error as Error | null} />
          )}
        </TabsContent>
        {e2b && (
          <TabsContent value="templates">
            <TemplatesTab pid={pid} />
          </TabsContent>
        )}
        <TabsContent value="api-keys">
          <ApiKeysTab pid={pid} e2b={e2b} />
        </TabsContent>
      </Tabs>

      <CreateSandboxDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        pid={pid}
        scope={scope}
        templates={templates}
        gpuModels={gpuModels}
        limits={limits}
        onCreated={invalidate}
      />
      <ExecDialog pid={pid} scope={scope} sandbox={execFor} onClose={() => setExecFor(null)} />
      <LogsDialog pid={pid} scope={scope} sandbox={logsFor} onClose={() => setLogsFor(null)} />

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete sandbox?</DialogTitle>
            <DialogDescription>
              {toDelete ? `"${sbName(toDelete)}" and every file in it are removed now. Billing stops at once.` : ""}
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

function CreateSandboxDialog({
  open, onOpenChange, pid, scope, templates, gpuModels, limits, onCreated,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  pid: string
  scope?: CloudScope
  templates: SandboxTemplate[]
  gpuModels: string[]
  limits: SandboxLimits
  onCreated: () => void
}) {
  const [name, setName] = useState("")
  const [template, setTemplate] = useState("")
  const [cpu, setCpu] = useState("")
  const [mem, setMem] = useState("")
  const [gpu, setGpu] = useState(NO_GPU)
  const [hours, setHours] = useState("1")
  const tpl = templates.find((t) => t.name === template)
  const maxHours = Math.max(1, Math.floor((limits.maxTimeoutSeconds ?? 86400) / 3600))
  const cpuN = cpu === "" ? (tpl?.cpu ?? 1) : Number(cpu)
  const memN = mem === "" ? (tpl?.memoryGiB ?? 1) : Number(mem)
  const hoursN = Number(hours)
  const valid =
    !!tpl &&
    Number.isInteger(cpuN) && cpuN >= 1 && cpuN <= (limits.maxCpu ?? 8) &&
    Number.isInteger(memN) && memN >= 1 && memN <= (limits.maxMemoryGiB ?? 32) &&
    hoursN > 0 && hoursN <= maxHours

  const reset = () => {
    setName(""); setTemplate(""); setCpu(""); setMem(""); setGpu(NO_GPU); setHours("1")
  }
  const create = useMutation({
    mutationFn: () =>
      apiFetch(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: {
          type: TYPE,
          data: {
            template,
            ...(name.trim() ? { name: name.trim() } : {}),
            cpu: cpuN,
            memoryGiB: memN,
            timeoutSeconds: Math.round(hoursN * 3600),
            ...(gpu !== NO_GPU && !tpl?.gpu ? { gpu: { model: gpu, count: 1 } } : {}),
          },
        },
      }),
    onSuccess: () => {
      toast.success("Sandbox starting")
      reset()
      onOpenChange(false)
      onCreated()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <Dialog open={open} onOpenChange={(o) => { onOpenChange(o); if (!o) reset() }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New sandbox</DialogTitle>
          <DialogDescription>Billed per minute while running. Deleted automatically when the time limit is reached; extend it any time.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="sb-template">Template</Label>
            <Select value={template} onValueChange={(v) => { setTemplate(v); setCpu(""); setMem(""); setGpu(NO_GPU) }}>
              <SelectTrigger id="sb-template"><SelectValue placeholder="Choose an environment" /></SelectTrigger>
              <SelectContent>
                {templates.map((t) => (
                  <SelectItem key={t.name} value={t.name}>
                    {t.name}{t.description ? ` — ${t.description}` : ""}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {tpl && <p className="font-mono text-xs text-muted-foreground">{tpl.image}</p>}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="sb-name">Name (optional)</Label>
            <Input id="sb-name" value={name} maxLength={63} onChange={(e) => setName(e.target.value)} placeholder="agent-run-1" />
          </div>
          <div className="grid grid-cols-3 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="sb-cpu">vCPU</Label>
              <Input id="sb-cpu" inputMode="numeric" value={cpu} onChange={(e) => setCpu(e.target.value)} placeholder={String(tpl?.cpu ?? 1)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="sb-mem">Memory (GiB)</Label>
              <Input id="sb-mem" inputMode="numeric" value={mem} onChange={(e) => setMem(e.target.value)} placeholder={String(tpl?.memoryGiB ?? 1)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="sb-hours">Time limit (h)</Label>
              <Input id="sb-hours" inputMode="decimal" value={hours} onChange={(e) => setHours(e.target.value)} />
            </div>
          </div>
          {gpuModels.length > 0 && !tpl?.gpu && (
            <div className="grid gap-2">
              <Label htmlFor="sb-gpu">GPU</Label>
              <Select value={gpu} onValueChange={setGpu}>
                <SelectTrigger id="sb-gpu"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value={NO_GPU}>No GPU</SelectItem>
                  {gpuModels.map((g) => <SelectItem key={g} value={g}>{g}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
          )}
          <p className="text-xs text-muted-foreground">
            Limits: up to {limits.maxCpu ?? 8} vCPU and {limits.maxMemoryGiB ?? 32} GiB per sandbox, {maxHours} h. Your
            project&apos;s sandbox quota caps what all its sandboxes hold together.
          </p>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button disabled={!valid || create.isPending} onClick={() => create.mutate()}>
            {create.isPending ? "Starting…" : "Start sandbox"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ExecDialog({
  pid, scope, sandbox, onClose,
}: {
  pid: string
  scope?: CloudScope
  sandbox: CloudResource | null
  onClose: () => void
}) {
  const [cmd, setCmd] = useState("")
  const [result, setResult] = useState<SandboxExecResult | null>(null)
  const run = useMutation({
    mutationFn: () =>
      apiFetch<{ result: SandboxExecResult }>(`/project/${pid}/cloud/${sandbox!.id}/action`, {
        method: "POST",
        cloud: scope,
        body: { action: "EXEC", data: { cmd, timeoutSeconds: 120 } },
      }),
    onSuccess: (r) => setResult(r.result),
    onError: (e: Error) => toast.error(e.message),
  })
  const close = () => { setCmd(""); setResult(null); onClose() }
  return (
    <Dialog open={!!sandbox} onOpenChange={(o) => !o && close()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>Run a command</DialogTitle>
          <DialogDescription>
            In {sandbox ? sbName(sandbox) : ""}. Runs with <code>sh -c</code> and waits up to 2 minutes; output is capped at 1 MiB.
          </DialogDescription>
        </DialogHeader>
        <Textarea
          className="min-h-20 font-mono text-sm"
          value={cmd}
          onChange={(e) => setCmd(e.target.value)}
          placeholder={"python3 -c 'print(6 * 7)'"}
          aria-label="Command"
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && cmd.trim()) run.mutate()
          }}
        />
        {result && (
          <div className="grid gap-2" aria-live="polite">
            <div className="flex items-center gap-2 text-sm">
              Exit code
              <Badge variant={result.exitCode === 0 ? "secondary" : "destructive"}>{result.exitCode}</Badge>
              {result.truncated && <Badge variant="outline">output truncated</Badge>}
            </div>
            {result.stdout && (
              <pre className="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap">{result.stdout}</pre>
            )}
            {result.stderr && (
              <pre className="max-h-40 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap text-destructive">{result.stderr}</pre>
            )}
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={close}>Close</Button>
          <Button disabled={!cmd.trim() || run.isPending} onClick={() => run.mutate()}>
            <SquareTerminal aria-hidden="true" /> {run.isPending ? "Running…" : "Run"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function LogsDialog({
  pid, scope, sandbox, onClose,
}: {
  pid: string
  scope?: CloudScope
  sandbox: CloudResource | null
  onClose: () => void
}) {
  const logs = useQuery({
    queryKey: ["sandbox-logs", pid, sandbox?.id],
    queryFn: () =>
      apiFetch<{ result: { logs: string } }>(`/project/${pid}/cloud/${sandbox!.id}/action`, {
        method: "POST",
        cloud: scope,
        body: { action: "GET_LOGS", data: { tailLines: 500 } },
      }),
    enabled: !!sandbox,
  })
  return (
    <Dialog open={!!sandbox} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-3xl">
        <DialogHeader>
          <DialogTitle>Logs</DialogTitle>
          <DialogDescription>The newest 500 lines of {sandbox ? sbName(sandbox) : ""}.</DialogDescription>
        </DialogHeader>
        <pre className="max-h-96 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap" aria-live="polite">
          {logs.isLoading ? "Loading…" : logs.error ? (logs.error as Error).message : logs.data?.result.logs || "(no output yet)"}
        </pre>
        <DialogFooter>
          <Button variant="outline" onClick={() => logs.refetch()} disabled={logs.isFetching}>
            <RefreshCw className={logs.isFetching ? "animate-spin" : ""} aria-hidden="true" /> Refresh
          </Button>
          <Button variant="outline" onClick={onClose}>Close</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
