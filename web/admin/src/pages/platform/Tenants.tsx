// Tenants.tsx — the parts of the Managed services page that make a green install usable by a tenant
// (internal/platformlife/tenants.go): the settings' choice of who gets the two providers, and on the Ready card the one
// button that brings the install's node image to published and offers its Kubernetes version.
import { useEffect, useRef, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { apiFetch } from "@/lib/api"
import { ErrorNote } from "@/pages/setup/common"
import { NODE_STATE, type PublishAllReport } from "@/pages/system/TemplateUpdatesPage"
import { ATTACH_LABEL, attachDefault, type Attach, type BOM, type Installation } from "./model"

const ATTACH_HINT: Record<Attach, string> = {
  all: "Every project gets managed Kubernetes and databases in its menu when the install ends. The projects that exist are attached by one bulk attach, which you can follow on each provider's Projects tab.",
  new: "Projects created after the install get both. The projects that exist keep their menu until you attach them on a provider's Projects tab.",
  none: "The two providers are registered and held back, as before v0.9.0. No tenant sees them until you attach them.",
}

// The choice of who gets the two providers. value "" = not chosen yet: the default for the site is shown as chosen.
// A site that already has projects is live with tenants, so nothing changes for them unless the operator says so.
export function AttachChoice({ value, projects, disabled, onChange }: {
  value: Attach | ""; projects?: number; disabled: boolean; onChange: (v: Attach) => void
}) {
  const chosen = value || attachDefault(projects)
  return (
    <fieldset className="grid content-start gap-1.5">
      <legend className="mb-1.5 text-sm font-medium">Make managed Kubernetes and databases available to</legend>
      {(Object.keys(ATTACH_LABEL) as Attach[]).map((a) => (
        <label key={a} className="flex items-start gap-2 text-sm">
          <input type="radio" name="pf-attach" className="mt-1" checked={chosen === a} disabled={disabled} onChange={() => onChange(a)} />
          <span>{ATTACH_LABEL[a]}<span className="block text-xs text-muted-foreground">{ATTACH_HINT[a]}</span></span>
        </label>
      ))}
      <p className="text-xs text-muted-foreground">
        {value ? null : projects === undefined
          ? "The projects of this site could not be counted, so “New projects only” is preselected. "
          : projects > 0
            ? `This site has ${projects} ${projects === 1 ? "project" : "projects"}, so “New projects only” is preselected. `
            : "This site has no project yet, so “All projects” is preselected. "}
        Can change later: a changed choice is applied once, by the next Install or “Apply this release”. A run never detaches a project.
      </p>
    </fieldset>
  )
}

const POLL_MS = 15_000
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

// The Kubernetes provider offers no version: say which node lacks a published node image, and bring the install's own
// image there with one button. The install imports it staged and never publishes it: Template updates publishes only a
// copy whose smoke test passed, and the tests would clone beside the build. So the button runs "Publish on all nodes"
// for the BOM's image (one smoke test or import at a time) and then asks the API to offer the version. It stops on the
// first step that fails and says where; nothing is withdrawn and no version the provider names is overwritten.
export function KubernetesVersion({ installation, canManage }: { installation: Installation; canManage: boolean }) {
  const qc = useQueryClient()
  const sha = installation.status.bom?.sha256
  const bom = useQuery({
    queryKey: ["platform-bom", sha],
    queryFn: () => apiFetch<BOM>(`/admin/platform/bom?sha256=${sha}`),
    enabled: !!sha,
    retry: false,
  })
  const image = bom.data?.kubernetes.nodeImage
  const version = bom.data?.kubernetes.version
  const path = `/admin/template-updates/publish-all`
  const read = () => apiFetch<PublishAllReport>(`${path}?serviceId=${encodeURIComponent(installation.serviceId)}&digest=${encodeURIComponent(image!.digest)}`)
  const key = ["platform-node-image", installation.serviceId, image?.digest]
  const report = useQuery({ queryKey: key, queryFn: read, enabled: !!image, retry: false })
  const [step, setStep] = useState("")
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => { alive.current = false } // a page that is left stops asking; the jobs already queued finish
  }, [])

  const run = useMutation({
    mutationFn: async () => {
      let r = await read()
      while (alive.current) {
        qc.setQueryData(key, r)
        if (r.next === "advance") {
          setStep("Starting the next step…")
          const before = JSON.stringify(r.nodes)
          r = await apiFetch<PublishAllReport>(path, {
            method: "POST", body: { serviceId: installation.serviceId, package: image!.package, digest: image!.digest, confirmDigest: image!.digest },
          })
          if (r.error) throw new Error(`${r.error.message} (${r.error.code})`)
          // An advance that moved nothing would be asked again for ever: stop and show the nodes.
          if (r.next === "advance" && JSON.stringify(r.nodes) === before) break
        } else if (r.next === "wait") {
          setStep("A smoke test or an import is running. This page asks again every 15 seconds; a smoke test takes a few minutes.")
          await sleep(POLL_MS)
          r = await read()
        } else break
      }
      qc.setQueryData(key, r)
      return apiFetch<{ version: string; line: string; offered: boolean }>(`/admin/platform/installations/${installation.id}/kubernetes-version`, { method: "POST", body: {} })
    },
    onSuccess: (r) => {
      toast.success(r.offered ? `Tenants can now create clusters with Kubernetes ${r.version}` : `The provider already names Kubernetes ${r.version}: nothing was changed`)
      return qc.invalidateQueries({ queryKey: ["admin-list", "/admin/service"] })
    },
    onSettled: () => setStep(""),
  })

  const nodes = report.data?.nodes ?? []
  return (
    <div className="grid gap-2" role="group" aria-label="Kubernetes version for tenants">
      <p>
        The install imported the node image of Kubernetes {version ?? "its release"} and did not publish it for tenants: a copy is published
        only after its smoke test passed, and the tests do not run beside the install.
      </p>
      {nodes.length ? (
        <ul aria-label="Node image per node" className="grid gap-1">
          {nodes.map((n) => (
            <li key={n.node} className="flex flex-col gap-0.5 sm:flex-row sm:gap-3">
              <span className="font-mono sm:w-40">{n.node}{n.vmid ? ` · VM ${n.vmid}` : ""}</span>
              <span className="min-w-0 flex-1">
                {NODE_STATE[n.state]?.label ?? n.state}
                {n.message ? <span className="text-muted-foreground">: {n.message}</span> : null}
              </span>
            </li>
          ))}
        </ul>
      ) : null}
      <ErrorNote title="The node image's state could not be read" error={bom.error ?? report.error} />
      <div className="flex flex-wrap items-center gap-3">
        <Button disabled={!canManage || !image || run.isPending} onClick={() => run.mutate()}>
          {run.isPending ? "Working…" : `Test, publish and offer Kubernetes ${version ?? ""}`.trim()}
        </Button>
        <Link to="/system/template-updates" className="underline underline-offset-4">Or do each step in Template updates</Link>
      </div>
      <p className="text-muted-foreground">
        Smoke-tests the image on each node of the provider, one at a time, publishes the copies that pass and then adds the version to
        this provider. Keep this page open: leaving it pauses, and the button goes on from where it stopped. The smoke test needs the
        provider's smoke-test network (<span className="font-mono">config.templateSmoke</span>).
      </p>
      {step ? <p role="status">{step}</p> : null}
      <ErrorNote title="It stopped" error={run.error} />
    </div>
  )
}
