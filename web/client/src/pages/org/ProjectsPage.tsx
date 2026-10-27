import { useMemo, useState } from "react"
import { useNavigate } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { ArrowRightLeft, CreditCard, FolderKanban, MoreHorizontal, Pencil, Plus, Trash2, Undo2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { useProjectId, useProjects } from "@/lib/hooks"
import { cn } from "@/lib/utils"
import type { BillingSummary, Organization, Project } from "@/lib/types"
import { useOrg } from "./MembersPage"

export default function ProjectsPage() {
  const pid = useProjectId()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const { data: projects, isLoading, error } = useProjects()
  const { org } = useOrg(pid)

  // Every org the caller belongs to (same cache key as useOrg — no extra fetch),
  // so create/move can target any of them, not just the current project's org.
  const orgsQ = useQuery({
    queryKey: ["organizations"],
    queryFn: () => apiFetch<Organization[]>("/organizations"),
  })
  const orgs = orgsQ.data ?? []

  const [createOpen, setCreateOpen] = useState(false)
  const [newName, setNewName] = useState("")
  const [createOrg, setCreateOrg] = useState("")
  const [movingProj, setMovingProj] = useState<Project | null>(null)
  const [moveOrg, setMoveOrg] = useState("")
  const [renaming, setRenaming] = useState<Project | null>(null)
  const [renameName, setRenameName] = useState("")
  const [deleting, setDeleting] = useState<Project | null>(null)
  const [changingBilling, setChangingBilling] = useState<Project | null>(null)
  const [targetBp, setTargetBp] = useState("")

  // Billing profiles the caller can read (GET /billing-profile → billing.Summary list).
  const billingProfiles = useQuery({
    queryKey: ["billing-profiles"],
    queryFn: () => apiFetch<BillingSummary[]>("/billing-profile"),
    enabled: !!changingBilling,
  })

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["projects"] })

  // The org a new project lands in: the explicit dialog pick, else the current
  // project's org, else the first org the caller belongs to.
  const createOrgId = createOrg || org?.id || orgs[0]?.id || ""

  const create = useMutation({
    mutationFn: () =>
      apiFetch<Project>(`/project`, { method: "POST", body: { name: newName.trim(), organizationId: createOrgId } }),
    onSuccess: (p) => {
      toast.success(`Project ${p.name} created`)
      setCreateOpen(false)
      setNewName("")
      setCreateOrg("")
      invalidate()
      if (p?.id) navigate(`/p/${p.id}/dashboard`)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // PUT /project/{id}/organization — move a project to another org the caller belongs to.
  const move = useMutation({
    mutationFn: ({ p, orgId }: { p: Project; orgId: string }) =>
      apiFetch(`/project/${p.id}/organization`, { method: "PUT", body: { organizationId: orgId } }),
    onSuccess: () => {
      toast.success("Project moved")
      setMovingProj(null)
      setMoveOrg("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const rename = useMutation({
    mutationFn: (p: Project) =>
      apiFetch(`/project/${p.id}/rename`, { method: "POST", body: { name: renameName.trim() } }),
    onSuccess: () => {
      toast.success("Project renamed")
      setRenaming(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const remove = useMutation({
    // DELETE /project/{id} schedules deletion (cancellable for ~5 minutes via /cancel).
    mutationFn: (p: Project) => apiFetch(`/project/${p.id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Project scheduled for deletion — you can cancel it for the next 5 minutes")
      setDeleting(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const cancelDeletion = useMutation({
    mutationFn: (p: Project) => apiFetch(`/project/${p.id}/cancel`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Deletion cancelled")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // DELETE /project/{id}/now — immediate delete, no 5-minute grace window.
  const removeNow = useMutation({
    mutationFn: (p: Project) => apiFetch(`/project/${p.id}/now`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Project deleted")
      setDeleting(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // POST /project/{id}/billing/{bpId} — reassign the project's billing profile.
  const changeBilling = useMutation({
    mutationFn: ({ p, bpId }: { p: Project; bpId: string }) =>
      apiFetch(`/project/${p.id}/billing/${bpId}`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Billing profile changed")
      setChangingBilling(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const bpLabel = (bp: BillingSummary) =>
    `${bp.fullName || bp.id || "Unnamed"}${bp.currency ? ` · ${bp.currency}` : ""}${bp.status ? ` · ${bp.status}` : ""}`

  const cancelDeletionMutate = cancelDeletion.mutate
  const showMove = orgs.length > 1
  const columns = useMemo<ColumnDef<Project, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (p) => p.name,
        header: sortableHeader("Name"),
        cell: ({ row }) => {
          const p = row.original
          const scheduled = p.status === "SCHEDULED_FOR_DELETION"
          return (
            <span className={cn("font-medium", scheduled && "text-muted-foreground line-through decoration-muted-foreground/50")}>
              {p.name}
            </span>
          )
        },
      },
      {
        id: "status",
        accessorFn: (p) => p.status ?? "",
        header: sortableHeader("Status"),
        cell: ({ row }) => {
          const p = row.original
          // Scheduled-for-deletion gets the amber treatment: statusKind would
          // classify it "muted", which under-signals a project on a timer.
          return p.status === "SCHEDULED_FOR_DELETION" ? (
            <span className="inline-flex items-center gap-1.5 text-sm text-muted-foreground">
              <span className="status-dot status-dot-warn" />
              Scheduled for deletion
            </span>
          ) : (
            <StatusBadge status={p.status} />
          )
        },
      },
      {
        id: "id",
        accessorFn: (p) => p.id,
        header: "ID",
        cell: ({ getValue }) => <span className="font-mono text-xs text-muted-foreground">{getValue()}</span>,
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const p = row.original
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${p.name}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem
                    onClick={() => {
                      setRenaming(p)
                      setRenameName(p.name)
                    }}
                  >
                    <Pencil className="size-4" /> Rename
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    onClick={() => {
                      setTargetBp("")
                      setChangingBilling(p)
                    }}
                  >
                    <CreditCard className="size-4" /> Change billing profile
                  </DropdownMenuItem>
                  {showMove && (
                    <DropdownMenuItem
                      onClick={() => {
                        setMoveOrg("")
                        setMovingProj(p)
                      }}
                    >
                      <ArrowRightLeft className="size-4" /> Move to organization
                    </DropdownMenuItem>
                  )}
                  {p.status === "SCHEDULED_FOR_DELETION" ? (
                    <DropdownMenuItem onClick={() => cancelDeletionMutate(p)}>
                      <Undo2 className="size-4" /> Cancel deletion
                    </DropdownMenuItem>
                  ) : (
                    <DropdownMenuItem variant="destructive" onClick={() => setDeleting(p)}>
                      <Trash2 className="size-4" /> Delete
                    </DropdownMenuItem>
                  )}
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // mutate is referentially stable; the set* dialog openers are stable setters.
    [showMove, cancelDeletionMutate],
  )

  return (
    <>
      <PageHeader
        title="Projects"
        eyebrow="Organization"
        description="All projects in this organization."
        actions={
          <Button size="sm" onClick={() => setCreateOpen(true)} disabled={!orgs.length}>
            <Plus className="size-4" /> Create project
          </Button>
        }
      />

      {!isLoading && !error && !projects?.length ? (
        <EmptyState
          icon={FolderKanban}
          title="No projects yet"
          hint="Create a project to start provisioning cloud resources."
          action={
            <Button onClick={() => setCreateOpen(true)} disabled={!orgs.length}>
              <Plus className="size-4" /> Create project
            </Button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={projects}
          isLoading={isLoading}
          error={error as Error | null}
          searchPlaceholder="Search projects…"
          onRowClick={(p) => navigate(`/p/${p.id}/dashboard`)}
          getRowId={(p) => p.id}
        />
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create project</DialogTitle>
            <DialogDescription>A new project in {org?.name ?? "your organization"}.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div>
              <Label className="mb-1.5 block" htmlFor="project-name">Project name</Label>
              <Input
                id="project-name"
                autoComplete="off"
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder="my-project"
              />
            </div>
            {orgs.length > 1 && (
              <div>
                <Label className="mb-1.5 block">Organization</Label>
                <Select value={createOrgId} onValueChange={setCreateOrg}>
                  <SelectTrigger className="w-full">
                    <SelectValue placeholder="Pick an organization" />
                  </SelectTrigger>
                  <SelectContent>
                    {orgs.map((o) => (
                      <SelectItem key={o.id} value={o.id}>
                        {o.name ?? o.id}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => create.mutate()} disabled={!newName.trim() || !createOrgId || create.isPending}>
              {create.isPending ? "Creating…" : "Create project"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!renaming} onOpenChange={(o) => !o && setRenaming(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Rename project</DialogTitle>
          </DialogHeader>
          <div>
            <Label className="mb-1.5 block" htmlFor="project-rename">New name</Label>
            <Input
              id="project-rename"
              autoComplete="off"
              value={renameName}
              onChange={(e) => setRenameName(e.target.value)}
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRenaming(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => renaming && rename.mutate(renaming)}
              disabled={!renameName.trim() || rename.isPending}
            >
              {rename.isPending ? "Renaming…" : "Rename"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete project</DialogTitle>
            <DialogDescription>
              Delete {deleting?.name}? Scheduling gives you 5 minutes to cancel before its cloud resources are
              removed. "Delete now" skips the grace window and removes everything immediately.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>
              Cancel
            </Button>
            <Button
              variant="outline"
              className="border-destructive/40 text-destructive hover:text-destructive"
              onClick={() => deleting && removeNow.mutate(deleting)}
              disabled={removeNow.isPending || remove.isPending}
            >
              {removeNow.isPending ? "Deleting…" : "Delete now"}
            </Button>
            <Button
              variant="destructive"
              onClick={() => deleting && remove.mutate(deleting)}
              disabled={remove.isPending || removeNow.isPending}
            >
              {remove.isPending ? "Deleting…" : "Schedule deletion"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!changingBilling} onOpenChange={(o) => !o && setChangingBilling(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Change billing profile</DialogTitle>
            <DialogDescription>
              Pick the billing profile that {changingBilling?.name} should be charged against.
            </DialogDescription>
          </DialogHeader>
          <div>
            <Label className="mb-1.5 block">Billing profile</Label>
            {billingProfiles.isLoading ? (
              <Skeleton className="h-9" />
            ) : billingProfiles.error ? (
              <p className="text-sm text-muted-foreground">{(billingProfiles.error as Error).message}</p>
            ) : !billingProfiles.data?.length ? (
              <p className="text-sm text-muted-foreground">No billing profiles available.</p>
            ) : (
              <Select value={targetBp} onValueChange={setTargetBp}>
                <SelectTrigger className="w-full">
                  <SelectValue placeholder="Pick a billing profile" />
                </SelectTrigger>
                <SelectContent>
                  {billingProfiles.data.map((bp) => (
                    <SelectItem key={String(bp.id)} value={String(bp.id)}>
                      {bpLabel(bp)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setChangingBilling(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => changingBilling && changeBilling.mutate({ p: changingBilling, bpId: targetBp })}
              disabled={!targetBp || changeBilling.isPending}
            >
              {changeBilling.isPending ? "Changing…" : "Change billing profile"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!movingProj} onOpenChange={(o) => !o && setMovingProj(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Move to organization</DialogTitle>
            <DialogDescription>
              Move {movingProj?.name} to another organization you belong to. Its resources and billing profile move with it.
            </DialogDescription>
          </DialogHeader>
          <div>
            <Label className="mb-1.5 block">Target organization</Label>
            <Select value={moveOrg} onValueChange={setMoveOrg}>
              <SelectTrigger className="w-full">
                <SelectValue placeholder="Pick an organization" />
              </SelectTrigger>
              <SelectContent>
                {orgs
                  .filter((o) => o.id !== movingProj?.organizationId)
                  .map((o) => (
                    <SelectItem key={o.id} value={o.id}>
                      {o.name ?? o.id}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setMovingProj(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => movingProj && move.mutate({ p: movingProj, orgId: moveOrg })}
              disabled={!moveOrg || move.isPending}
            >
              {move.isPending ? "Moving…" : "Move project"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
