import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Copy, Mail, MoreHorizontal, Trash2, UserPlus, Users } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
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
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { fmtDateTime } from "@/lib/format"
import { useProject, useProjectId } from "@/lib/hooks"
import type { Organization } from "@/lib/types"

type Member = { sub: string; firstName?: string; lastName?: string; email?: string; role?: string }

// GET /project/{id}/members → project.ProjectUser.
type ProjectMember = { userId?: string; sub: string; firstName?: string; lastName?: string; email?: string; role?: string }

type OrgRole = { id: string; name: string; builtIn: boolean }

// GET /project-invites/project/{pid} → the pending invitations the caller may see: all of them for a project
// admin, their own otherwise. Each carries its link: mailConfigured says only whether a mail was handed to a
// gateway, not that it arrived.
type PendingInvite = { id: string; email: string; expiresAt?: string; mailConfigured?: boolean; inviteUrl?: string }
type PendingInvites = { mailConfigured: boolean; invites: PendingInvite[] }

const NO_MAIL =
  "This site sends no email. Copy the invitation link and send it to your teammate yourself. They can also sign in with the invited address: the invitation is then waiting on their start page."

function CopyLink({ url }: { url: string }) {
  return (
    <Button
      variant="outline"
      size="sm"
      onClick={async () => {
        // navigator.clipboard is absent on a plain-http site and writeText can be refused: show the link instead.
        try {
          await navigator.clipboard.writeText(url)
          toast.success("Invitation link copied")
        } catch {
          toast.error(`Couldn't copy the link. Select and copy it: ${url}`, { duration: 30000 })
        }
      }}
    >
      <Copy className="size-4" /> Copy link
    </Button>
  )
}

// Resolve the org that owns this project (fall back to the first org the user belongs to).
export function useOrg(pid: string) {
  const { project } = useProject(pid)
  const orgs = useQuery({
    queryKey: ["organizations"],
    queryFn: () => apiFetch<Organization[]>("/organizations"),
  })
  const org = orgs.data?.find((o) => o.id === project?.organizationId) ?? orgs.data?.[0]
  return { org, isLoading: orgs.isLoading, error: orgs.error }
}

function memberName(m: { firstName?: string; lastName?: string }) {
  return [m.firstName, m.lastName].filter(Boolean).join(" ") || "—"
}

export default function MembersPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const { org, isLoading: orgLoading, error: orgError } = useOrg(pid)

  const { data: members, isLoading, error } = useQuery({
    queryKey: ["org-members", org?.id],
    queryFn: () => apiFetch<Member[]>(`/organizations/${org?.id}/members`),
    enabled: !!org?.id,
  })

  // Role options = built-ins + this org's custom roles (GET /organizations/{id}/roles).
  const { data: roles } = useQuery({
    queryKey: ["org-roles", org?.id],
    queryFn: () => apiFetch<OrgRole[]>(`/organizations/${org?.id}/roles`),
    enabled: !!org?.id,
  })
  const roleNames = useMemo(() => roles?.map((r) => r.name) ?? ["OWNER", "ADMIN", "MEMBER"], [roles])

  // Project members for THIS project.
  const projectMembers = useQuery({
    queryKey: ["project-members", pid],
    queryFn: () => apiFetch<ProjectMember[]>(`/project/${pid}/members`),
    enabled: !!pid,
  })

  // Pending invitations of THIS project. A caller who may not see the project's invitations gets none.
  const pending = useQuery({
    queryKey: ["project-invites", pid],
    queryFn: () => apiFetch<PendingInvites>(`/project-invites/project/${pid}`),
    enabled: !!pid,
  })

  const [inviteOpen, setInviteOpen] = useState(false)
  const [email, setEmail] = useState("")
  // The invitation just made on a site without mail: the dialog stays open to hand over its link.
  const [created, setCreated] = useState<PendingInvite | null>(null)
  const [revoking, setRevoking] = useState<PendingInvite | null>(null)
  const [addOpen, setAddOpen] = useState(false)
  const [addEmail, setAddEmail] = useState("")
  const [addRole, setAddRole] = useState("MEMBER")
  const [removing, setRemoving] = useState<Member | null>(null)
  const [pmAddOpen, setPmAddOpen] = useState(false)
  const [pmSub, setPmSub] = useState("")
  const [pmRole, setPmRole] = useState("MEMBER")
  const [pmRemoving, setPmRemoving] = useState<ProjectMember | null>(null)

  const invalidateOrg = () => void qc.invalidateQueries({ queryKey: ["org-members", org?.id] })
  const invalidateProject = () => void qc.invalidateQueries({ queryKey: ["project-members", pid] })

  const invite = useMutation({
    // POST /project-invites/invite {email, projectId} → 202 with the invitation (mail + audit).
    mutationFn: () =>
      apiFetch<PendingInvite | undefined>(`/project-invites/invite`, { method: "POST", body: { email: email.trim(), projectId: pid } }),
    onSuccess: (inv) => {
      void qc.invalidateQueries({ queryKey: ["project-invites", pid] })
      if (inv?.inviteUrl && !inv.mailConfigured) {
        setCreated(inv) // no mail went out: saying "sent" would be the old dead end
        return
      }
      toast.success(`Invitation sent to ${email.trim()}`)
      setInviteOpen(false)
      setEmail("")
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const revoke = useMutation({
    // DELETE /project-invites/project/{pid}/{inviteId}: the link stops working.
    mutationFn: (id: string) => apiFetch(`/project-invites/project/${pid}/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Invitation revoked")
      setRevoking(null)
      void qc.invalidateQueries({ queryKey: ["project-invites", pid] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const closeInvite = (open: boolean) => {
    setInviteOpen(open)
    if (!open) {
      setCreated(null)
      setEmail("")
    }
  }

  const addMember = useMutation({
    // POST /organizations/{id}/member {email, role} — the user must already exist;
    // omitted projectIds → the member is propagated onto all org projects.
    mutationFn: () =>
      apiFetch(`/organizations/${org?.id}/member`, {
        method: "POST",
        body: { email: addEmail.trim(), role: addRole },
      }),
    onSuccess: () => {
      toast.success(`${addEmail.trim()} added to the organization`)
      setAddOpen(false)
      setAddEmail("")
      invalidateOrg()
      invalidateProject()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const changeRole = useMutation({
    // PUT /organizations/{id}/member/{sub}/role {role}.
    mutationFn: ({ sub, role }: { sub: string; role: string }) =>
      apiFetch(`/organizations/${org?.id}/member/${sub}/role`, { method: "PUT", body: { role } }),
    onSuccess: () => {
      toast.success("Role updated")
      invalidateOrg()
    },
    onError: (e: Error) => {
      toast.error(e.message)
      invalidateOrg() // reset the select to the server value
    },
  })

  const remove = useMutation({
    mutationFn: (sub: string) => apiFetch(`/organizations/${org?.id}/member/${sub}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Member removed")
      setRemoving(null)
      invalidateOrg()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const addProjectMember = useMutation({
    // POST /project/{id}/members {userSub, role}.
    mutationFn: () =>
      apiFetch(`/project/${pid}/members`, { method: "POST", body: { userSub: pmSub, role: pmRole } }),
    onSuccess: () => {
      toast.success("Member added to project")
      setPmAddOpen(false)
      setPmSub("")
      invalidateProject()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const changeProjectRole = useMutation({
    // PUT /project/{id}/members/{sub}/role {role} — OWNER (project admin) or MEMBER.
    mutationFn: ({ sub, role }: { sub: string; role: string }) =>
      apiFetch(`/project/${pid}/members/${encodeURIComponent(sub)}/role`, { method: "PUT", body: { role } }),
    onSuccess: () => {
      toast.success("Project role updated")
      invalidateProject()
    },
    onError: (e: Error) => {
      toast.error(e.message)
      invalidateProject() // reset the select to the server value
    },
  })

  const removeProjectMember = useMutation({
    // DELETE /project/{id}/members?sub=… (sub passed as a query param).
    mutationFn: (sub: string) =>
      apiFetch(`/project/${pid}/members?sub=${encodeURIComponent(sub)}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Member removed from project")
      setPmRemoving(null)
      invalidateProject()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const err = (orgError ?? error) as Error | null
  // Org members not yet on this project — candidates for the project-member add.
  const projectSubs = new Set((projectMembers.data ?? []).map((m) => m.sub))
  const addCandidates = (members ?? []).filter((m) => !projectSubs.has(m.sub))

  const changeRoleMutate = changeRole.mutate
  const changeRolePending = changeRole.isPending
  const orgColumns = useMemo<ColumnDef<Member, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (m) => memberName(m),
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "email",
        accessorFn: (m) => m.email ?? "",
        header: sortableHeader("Email"),
        cell: ({ getValue }) => <span className="text-sm">{getValue() || "—"}</span>,
      },
      {
        id: "sub",
        accessorFn: (m) => m.sub,
        header: "Subject",
        cell: ({ getValue }) => (
          <span className="font-mono text-xs text-muted-foreground">{getValue()}</span>
        ),
      },
      {
        id: "role",
        accessorFn: (m) => m.role ?? "",
        header: sortableHeader("Role"),
        cell: ({ row }) => {
          const m = row.original
          return (
            <Select
              value={m.role ?? ""}
              onValueChange={(role) => changeRoleMutate({ sub: m.sub, role })}
              disabled={changeRolePending}
            >
              <SelectTrigger className="h-8 w-36" size="sm" aria-label={`Role for ${m.email ?? m.sub}`}>
                <SelectValue placeholder="Set role" />
              </SelectTrigger>
              <SelectContent>
                {roleNames.map((r) => (
                  <SelectItem key={r} value={r}>
                    {r}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )
        },
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const m = row.original
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${m.email ?? m.sub}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem variant="destructive" onClick={() => setRemoving(m)}>
                    <Trash2 className="size-4" /> Remove
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // mutate is referentially stable; setRemoving is a stable setter.
    [roleNames, changeRoleMutate, changeRolePending],
  )

  const changeProjectRoleMutate = changeProjectRole.mutate
  const changeProjectRolePending = changeProjectRole.isPending
  const projectColumns = useMemo<ColumnDef<ProjectMember, any>[]>(
    () => [
      {
        id: "name",
        accessorFn: (m) => memberName(m),
        header: sortableHeader("Name"),
        cell: ({ getValue }) => <span className="font-medium">{getValue()}</span>,
      },
      {
        id: "email",
        accessorFn: (m) => m.email ?? "",
        header: sortableHeader("Email"),
        cell: ({ getValue }) => <span className="text-sm">{getValue() || "—"}</span>,
      },
      {
        id: "role",
        accessorFn: (m) => m.role ?? "",
        header: sortableHeader("Role"),
        cell: ({ row }) => {
          const m = row.original
          return (
            <Select
              value={m.role ?? ""}
              onValueChange={(role) => changeProjectRoleMutate({ sub: m.sub, role })}
              disabled={changeProjectRolePending}
            >
              <SelectTrigger className="h-8 w-32" size="sm" aria-label={`Project role for ${m.email ?? m.sub}`}>
                <SelectValue placeholder="Set role" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="MEMBER">MEMBER</SelectItem>
                <SelectItem value="OWNER">OWNER</SelectItem>
              </SelectContent>
            </Select>
          )
        },
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const m = row.original
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${m.email ?? m.sub}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem variant="destructive" onClick={() => setPmRemoving(m)}>
                    <Trash2 className="size-4" /> Remove
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // mutate is referentially stable; setPmRemoving is a stable setter.
    [changeProjectRoleMutate, changeProjectRolePending],
  )

  return (
    <>
      <PageHeader
        title="Members"
        eyebrow="Organization"
        description={org?.name ? `People in the ${org.name} organization.` : "People in this organization."}
        actions={
          <>
            <Button size="sm" variant="outline" onClick={() => setInviteOpen(true)} disabled={!org}>
              <Mail className="size-4" /> Invite by email
            </Button>
            <Button size="sm" onClick={() => setAddOpen(true)} disabled={!org}>
              <UserPlus className="size-4" /> Add existing user
            </Button>
          </>
        }
      />

      {!orgLoading && !isLoading && !err && !members?.length ? (
        <EmptyState icon={Users} title="No members" hint="Invite teammates to collaborate on this project." />
      ) : (
        <DataTable
          columns={orgColumns}
          data={members}
          isLoading={orgLoading || isLoading}
          error={err}
          searchPlaceholder="Search members…"
          getRowId={(m) => m.sub}
        />
      )}

      {/* Project members — membership of THIS project only. */}
      <div className="mt-8">
        <div className="mb-3 flex items-end justify-between gap-4">
          <div>
            <h2 className="font-display text-lg font-semibold">Project members</h2>
            <p className="text-sm text-muted-foreground">People with access to this project.</p>
          </div>
          <Button
            size="sm"
            variant="outline"
            onClick={() => {
              setPmSub(addCandidates[0]?.sub ?? "")
              setPmRole("MEMBER")
              setPmAddOpen(true)
            }}
            disabled={!pid}
          >
            <UserPlus className="size-4" /> Add to project
          </Button>
        </div>
        {!projectMembers.isLoading && !projectMembers.error && !projectMembers.data?.length ? (
          <EmptyState icon={Users} title="No project members" hint="Add an organization member to this project." />
        ) : (
          <DataTable
            columns={projectColumns}
            data={projectMembers.data}
            isLoading={projectMembers.isLoading}
            error={projectMembers.error as Error | null}
            searchPlaceholder="Search members…"
            getRowId={(m) => m.sub}
          />
        )}
      </div>

      {/* Pending invitations of this project: what "Invite by email" made and nobody has accepted yet. */}
      <div className="mt-8" data-testid="pending-invites">
        <div className="mb-3">
          <h2 className="font-display text-lg font-semibold">Pending invitations</h2>
          <p className="text-sm text-muted-foreground">
            Invitations to this project that are not accepted yet. An invitation expires 24 hours after it is made.
          </p>
        </div>
        {pending.data && !pending.data.mailConfigured ? (
          <p className="mb-3 rounded-md border bg-muted/50 px-3 py-2 text-sm" data-testid="no-mail-notice">
            {NO_MAIL}
          </p>
        ) : null}
        {pending.isLoading ? null : pending.error ? (
          <p className="text-sm text-muted-foreground">{(pending.error as Error).message}</p>
        ) : !pending.data?.invites?.length ? (
          <p className="rounded-xl border border-dashed p-6 text-center text-sm text-muted-foreground">No pending invitations.</p>
        ) : (
          <div className="overflow-hidden rounded-xl border bg-card">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Email</TableHead>
                  <TableHead>Expires</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {pending.data.invites.map((inv) => (
                  <TableRow key={inv.id}>
                    <TableCell className="text-sm">{inv.email}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">{fmtDateTime(inv.expiresAt)}</TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-2">
                        {inv.inviteUrl ? <CopyLink url={inv.inviteUrl} /> : null}
                        <Button variant="outline" size="sm" onClick={() => setRevoking(inv)} aria-label={`Revoke the invitation of ${inv.email}`}>
                          <Trash2 className="size-4" /> Revoke
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>

      {/* Invite by email (project invite — works for people without an account yet). */}
      <Dialog open={inviteOpen} onOpenChange={closeInvite}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Invite by email</DialogTitle>
            <DialogDescription>
              Invite someone to join this project. They accept after signing in with this email address, which their
              account must have verified. The invite expires in 24 hours.
            </DialogDescription>
          </DialogHeader>
          {created ? (
            <div className="grid gap-3" data-testid="invite-link">
              <p className="text-sm">
                The invitation for <span className="font-medium">{created.email}</span> is made, but no email was sent.{" "}
                {NO_MAIL}
              </p>
              <div className="flex items-center gap-2 rounded-md bg-muted p-2 font-mono text-xs break-all">
                <span className="flex-1">{created.inviteUrl}</span>
              </div>
            </div>
          ) : (
            <div>
              <Label className="mb-1.5 block" htmlFor="invite-email">Email</Label>
              <Input
                id="invite-email"
                type="email"
                autoComplete="email"
                placeholder="teammate@example.com"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </div>
          )}
          <DialogFooter>
            {created ? (
              <>
                {created.inviteUrl ? <CopyLink url={created.inviteUrl} /> : null}
                <Button onClick={() => closeInvite(false)}>Done</Button>
              </>
            ) : (
              <>
                <Button variant="outline" onClick={() => closeInvite(false)}>
                  Cancel
                </Button>
                <Button onClick={() => invite.mutate()} disabled={!email.trim() || invite.isPending}>
                  {invite.isPending ? "Inviting…" : "Send invite"}
                </Button>
              </>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!revoking} onOpenChange={(o) => !o && setRevoking(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke invitation</DialogTitle>
            <DialogDescription>
              Revoke the invitation of {revoking?.email}? Its link stops working. You can invite them again.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRevoking(null)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={() => revoking && revoke.mutate(revoking.id)} disabled={revoke.isPending}>
              {revoke.isPending ? "Revoking…" : "Revoke invitation"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Add existing user directly to the organization (no email round-trip). */}
      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add existing user</DialogTitle>
            <DialogDescription>
              Add a user who already has an account to this organization and its projects.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div>
              <Label className="mb-1.5 block" htmlFor="add-member-email">Email</Label>
              <Input
                id="add-member-email"
                type="email"
                autoComplete="email"
                placeholder="teammate@example.com"
                value={addEmail}
                onChange={(e) => setAddEmail(e.target.value)}
              />
            </div>
            <div>
              <Label className="mb-1.5 block" htmlFor="add-member-role">Role</Label>
              <Select value={addRole} onValueChange={setAddRole}>
                <SelectTrigger id="add-member-role" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {roleNames.map((r) => (
                    <SelectItem key={r} value={r}>
                      {r}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => addMember.mutate()} disabled={!addEmail.trim() || addMember.isPending}>
              {addMember.isPending ? "Adding…" : "Add member"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!removing} onOpenChange={(o) => !o && setRemoving(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Remove member</DialogTitle>
            <DialogDescription>
              Remove {removing?.email ?? removing?.sub} from the organization? They lose access to its projects.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRemoving(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => removing && remove.mutate(removing.sub)}
              disabled={remove.isPending}
            >
              {remove.isPending ? "Removing…" : "Remove member"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Add an org member to this project. */}
      <Dialog open={pmAddOpen} onOpenChange={setPmAddOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add to project</DialogTitle>
            <DialogDescription>Give an organization member access to this project.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div>
              <Label className="mb-1.5 block" htmlFor="pm-member">Member</Label>
              {addCandidates.length ? (
                <Select value={pmSub} onValueChange={setPmSub}>
                  <SelectTrigger id="pm-member" className="w-full">
                    <SelectValue placeholder="Pick a member" />
                  </SelectTrigger>
                  <SelectContent>
                    {addCandidates.map((m) => (
                      <SelectItem key={m.sub} value={m.sub}>
                        {m.email ?? m.sub}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              ) : (
                <p className="text-sm text-muted-foreground">
                  Every organization member is already on this project.
                </p>
              )}
            </div>
            <div>
              <Label className="mb-1.5 block" htmlFor="pm-role">Role</Label>
              <Select value={pmRole} onValueChange={setPmRole}>
                <SelectTrigger id="pm-role" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="MEMBER">MEMBER</SelectItem>
                  <SelectItem value="OWNER">OWNER</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPmAddOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => addProjectMember.mutate()} disabled={!pmSub || addProjectMember.isPending}>
              {addProjectMember.isPending ? "Adding…" : "Add to project"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!pmRemoving} onOpenChange={(o) => !o && setPmRemoving(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Remove from project</DialogTitle>
            <DialogDescription>
              Remove {pmRemoving?.email ?? pmRemoving?.sub} from this project? They stay in the organization.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPmRemoving(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => pmRemoving && removeProjectMember.mutate(pmRemoving.sub)}
              disabled={removeProjectMember.isPending}
            >
              {removeProjectMember.isPending ? "Removing…" : "Remove from project"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
