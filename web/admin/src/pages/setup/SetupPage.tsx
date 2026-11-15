// SetupPage — /setup (and /onboarding): a new install from an empty portal to its first tenant, one tab per step.
// The state of every step comes from GET /admin/setup/status (docs/api.md "Platform setup"); the page shows the
// database answer at once and the live one (the cluster asked too) when it arrives. Before a provider is saved there
// is no stored state: the Connect step keeps its form in page state and shows the rows of the connection test.
import { useState } from "react"
import { Link, useSearchParams } from "react-router-dom"
import { keepPreviousData, useMutation, useQuery } from "@tanstack/react-query"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { useTabParam } from "@/lib/hooks"
import { cn } from "@/lib/utils"
import {
  ProxmoxProviderForm,
  emptyProxmoxForm,
  proxmoxFormToBody,
  proxmoxFormValid,
  type ProxmoxFormState,
} from "@/pages/system/proxmoxProvider"
import {
  CheckRows,
  CopyBlock,
  ErrorNote,
  SETUP_STEPS,
  LINK,
  StepCard,
  errMsg,
  fetchText,
  stateView,
  useInvalidateSetup,
  useSetupStatus,
  type PVEAuthResult,
  type SetupStep,
} from "./common"
import { NetworkStep, StorageStep } from "./NetworkStorage"
import { TemplatesStep } from "./TemplatesStep"

const DEFAULT_TOKEN_ID = "proxcloud@pve!platform"

// ── Step 1: Prepare Proxmox ───────────────────────────────────────────────────

function PrepareStep() {
  const [tokenId, setTokenId] = useState(DEFAULT_TOKEN_ID)
  const [gpu, setGpu] = useState(false)
  const [importStorage, setImportStorage] = useState("")
  const qs = new URLSearchParams({ tokenId: tokenId.trim() || DEFAULT_TOKEN_ID, gpu: String(gpu) })
  if (importStorage.trim()) qs.set("importStorage", importStorage.trim())
  const script = useQuery({
    queryKey: ["setup-script", qs.toString()],
    queryFn: () => fetchText(`/admin/setup/proxmox-script?${qs}`),
    placeholderData: keepPreviousData,
    retry: false,
  })
  return (
    <StepCard
      n={1}
      title="Prepare Proxmox"
      description="Done by hand on one Proxmox node: the portal holds no root credential and asks for none. The Connect step shows whether it worked."
    >
      <div className="grid gap-4 sm:grid-cols-3">
        <div className="grid gap-2">
          <Label htmlFor="setup-token-id">API token ID</Label>
          <Input id="setup-token-id" className="font-mono" value={tokenId} onChange={(e) => setTokenId(e.target.value)} autoComplete="off" />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="setup-import-store">Import store (optional)</Label>
          <Input id="setup-import-store" className="font-mono" value={importStorage} onChange={(e) => setImportStorage(e.target.value)} placeholder="nas-templates" autoComplete="off" />
        </div>
        <div className="flex items-center justify-between gap-3 rounded-lg border p-3">
          <Label htmlFor="setup-gpu" className="text-sm">The cluster has GPUs to map</Label>
          <Switch id="setup-gpu" checked={gpu} onCheckedChange={setGpu} />
        </div>
      </div>
      <p className="text-sm text-muted-foreground">
        Run this as root on one node (the cluster shares it). It creates the role, the pools <code>pc-templates</code> and{" "}
        <code>proxcloud-system</code>, the user, its token and the ACLs on both, and can be run again. The token secret is
        printed once: keep it for the next step. The last lines print what the token holds and the certificate fingerprint.
      </p>
      {script.isLoading ? <Skeleton className="h-40" /> : null}
      <ErrorNote title="The script could not be generated" error={script.error} />
      {script.data && !script.error ? <CopyBlock text={script.data} label="setup script" /> : null}
      <div className="grid gap-1 text-sm">
        <p className="font-medium">Then turn on both firewalls in the Proxmox UI (the script does not change traffic):</p>
        <ul className="list-disc pl-5 text-muted-foreground">
          <li>Datacenter → Firewall → Options → Firewall: Yes</li>
          <li>Each node → Firewall → Options → nftables: Yes (running guests pick it up when they restart)</li>
        </ul>
      </div>
    </StepCard>
  )
}

// ── Step 2: Connect ───────────────────────────────────────────────────────────

// The connection-test body for the form's unsaved values: the same mapping as the provider it would save.
function authBody(f: ProxmoxFormState) {
  const c = proxmoxFormToBody(f).config
  return {
    apiUrl: c.apiUrl, apiUrls: c.apiUrls, tokenId: c.tokenId, tokenSecret: f.tokenSecret.trim(),
    caCert: c.caCert, fingerprint: c.fingerprint, insecureTls: c.insecureTls,
  }
}

function NewProvider({ onCreated }: { onCreated: (id: string) => void }) {
  const [form, setForm] = useState<ProxmoxFormState>({ ...emptyProxmoxForm, name: "Proxmox", tokenId: DEFAULT_TOKEN_ID })
  const test = useMutation({
    mutationFn: () => apiFetch<PVEAuthResult>("/admin/service/proxmox/auth?checks=all", { method: "POST", body: authBody(form) }),
  })
  const create = useMutation({
    mutationFn: () => {
      const body = proxmoxFormToBody(form)
      // Held back from new projects until Go live: a project created meanwhile is not attached to a half-set-up provider.
      return apiFetch<{ id: string }>("/admin/service", { method: "POST", body: { ...body, config: { ...body.config, attachToNewProjects: false } } })
    },
    onSuccess: (svc) => {
      toast.success("Provider saved")
      if (svc?.id) onCreated(svc.id)
    },
    onError: (e) => toast.error(errMsg(e)),
  })
  const checks = test.data?.checks
  const failed = checks?.some((c) => c.status === "fail")
  const fp = test.data?.presentedFingerprint
  return (
    <>
      <ProxmoxProviderForm form={form} setForm={setForm} setup />
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="outline" onClick={() => test.mutate()} disabled={test.isPending || !proxmoxFormValid(form)}>
          {test.isPending ? "Checking…" : test.data ? "Re-check" : "Run the checks"}
        </Button>
        <Button onClick={() => create.mutate()} disabled={create.isPending || !proxmoxFormValid(form)}>
          {create.isPending ? "Saving…" : "Save provider"}
        </Button>
        {failed ? <span className="text-sm text-muted-foreground">A check failed: the provider can be saved, but this step stays open until it passes.</span> : null}
      </div>
      <ErrorNote title="The connection test did not run" error={test.error} />
      <CheckRows checks={checks} />
      {fp ? (
        <Alert>
          <AlertTitle>The server presented this certificate</AlertTitle>
          <AlertDescription>
            <p className="font-mono text-xs break-all">{fp}</p>
            <p>
              Compare it with <code>pvenode cert info</code> on the node before you pin it: pinning what the server presents
              without comparing it is trust on first use.
            </p>
            <Button size="sm" variant="outline" onClick={() => setForm({ ...form, fingerprint: fp })}>
              It matches: pin this fingerprint
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
    </>
  )
}

function ConnectStep({ step, serviceId, loading, onCreated, onRecheck, rechecking }: {
  step?: SetupStep
  serviceId: string
  loading: boolean
  onCreated: (id: string) => void
  onRecheck: () => void
  rechecking: boolean
}) {
  return (
    <StepCard
      n={2}
      title="Connect"
      step={step}
      description="The API URL, the token and how to trust the certificate. Save creates the provider, held back from new projects until Go live."
    >
      {loading ? (
        <Skeleton className="h-40" />
      ) : serviceId ? (
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="outline" onClick={onRecheck} disabled={rechecking}>{rechecking ? "Checking…" : "Re-check"}</Button>
          <Link to={`/system/cloud-providers/${serviceId}`} className={LINK}>Change the connection on the provider page</Link>
        </div>
      ) : (
        <NewProvider onCreated={onCreated} />
      )}
    </StepCard>
  )
}

// A step whose form this page does not have yet: its checks, and where it is set today.
function StatusOnlyStep({ n, title, step, serviceId }: { n: number; title: string; step?: SetupStep; serviceId: string }) {
  return (
    <StepCard n={n} title={title} step={step}>
      {serviceId ? (
        <Link to={`/system/cloud-providers/${serviceId}`} className={LINK}>Set it on the provider page</Link>
      ) : null}
    </StepCard>
  )
}

// ── Page ──────────────────────────────────────────────────────────────────────

export default function SetupPage() {
  const [params, setParams] = useSearchParams()
  const [tab, setTab] = useTabParam("prepare")
  const status = useSetupStatus(params.get("serviceId") ?? "")
  const invalidate = useInvalidateSetup()
  const serviceId = status.data?.serviceId ?? ""
  const stepOf = (id: string) => status.data?.steps.find((s) => s.id === id)
  const recheck = () => {
    void status.db.refetch()
    void status.live.refetch()
  }
  const onCreated = (id: string) => {
    setParams((prev) => {
      const p = new URLSearchParams(prev)
      p.set("serviceId", id)
      return p
    }, { replace: true })
    invalidate()
  }

  return (
    <>
      <PageHeader
        title="Platform setup"
        eyebrow="System"
        description="From a new install to the first tenant: connect Proxmox, set networks, storage and templates, price the catalog, then go live."
        actions={
          <Button variant="outline" onClick={recheck} disabled={status.live.isFetching}>
            {status.live.isFetching ? "Checking the cluster…" : "Check again"}
          </Button>
        }
      />
      {status.db.error && status.live.error ? (
        <div className="mb-4"><ErrorNote title="The setup status could not be read" error={status.live.error} /></div>
      ) : status.live.error ? (
        <Alert className="mb-4">
          <AlertTitle>The cluster was not asked</AlertTitle>
          <AlertDescription>{errMsg(status.live.error)}. The states below come from the database only; steps the cluster decides read Unknown.</AlertDescription>
        </Alert>
      ) : status.data && !status.data.live && serviceId ? (
        <p className="mb-4 text-sm text-muted-foreground">Asking the cluster… until it answers, the states come from the database only.</p>
      ) : null}

      <Tabs value={tab} onValueChange={setTab} orientation="vertical" className="gap-4 max-lg:flex-col">
        <TabsList className="h-fit w-full shrink-0 lg:w-60" aria-label="Setup steps">
          {SETUP_STEPS.map((s, i) => {
            const state = s.id === "prepare" || s.id === "next" ? undefined : stepOf(s.id)?.state
            const v = stateView(state)
            const label = s.id === "prepare" ? "manual" : s.id === "next" ? "later" : v.label
            return (
              <TabsTrigger key={s.id} value={s.id} className="justify-start gap-2">
                <span className={cn("status-dot", state ? v.dot : "status-dot-muted")} aria-hidden="true" />
                <span>{i + 1}. {s.title}</span>
                <span className="sr-only">({label})</span>
              </TabsTrigger>
            )
          })}
        </TabsList>
        <div className="min-w-0 flex-1">
          <TabsContent value="prepare"><PrepareStep /></TabsContent>
          <TabsContent value="connect">
            <ConnectStep
              step={stepOf("connect")}
              serviceId={serviceId}
              loading={!status.data /* unread status: never offer a second provider */}
              onCreated={onCreated}
              onRecheck={recheck}
              rechecking={status.live.isFetching}
            />
          </TabsContent>
          <TabsContent value="network"><NetworkStep step={stepOf("network")} serviceId={serviceId} /></TabsContent>
          <TabsContent value="storage"><StorageStep step={stepOf("storage")} serviceId={serviceId} /></TabsContent>
          <TabsContent value="templates"><TemplatesStep step={stepOf("templates")} serviceId={serviceId} /></TabsContent>
          <TabsContent value="catalog"><StatusOnlyStep n={6} title="Catalog and pricing" step={stepOf("catalog")} serviceId={serviceId} /></TabsContent>
          <TabsContent value="golive"><StatusOnlyStep n={7} title="Go live" step={stepOf("golive")} serviceId={serviceId} /></TabsContent>
          <TabsContent value="next"><StatusOnlyStep n={8} title="Next: managed services" serviceId="" /></TabsContent>
        </div>
      </Tabs>
    </>
  )
}
