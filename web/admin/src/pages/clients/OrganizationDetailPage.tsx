import { useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { FolderKanban, Pencil, Plus, Trash2, Users } from "lucide-react"
import { toast } from "sonner"
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
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { fmtDate, fmtDateTime } from "@/lib/format"
import { useAdminGet, useAdminList, useTabParam } from "@/lib/hooks"

// GET /admin/organizations/{id} (organization.go orgToDto) — shaped org doc + memberCount /
// projectCount + populated billingProfile.
type OrgDetail = {
  id?: string
  name?: string
  description?: string
  billingProfileId?: string
  memberCount?: number
  projectCount?: number
  billingProfile?: {
    id?: string
    name?: string
    email?: string
    status?: string
    currency?: string
  }
  createdAt?: string
}

// GET /admin/organizations/{id}/members (organization.go organizationMemberDto).
type OrgMember = {
  sub?: string
  firstName?: string
  lastName?: string
  email?: string
  role?: string
}

// GET /admin/project/by-organization?organizationId= — RAW project docs (handler.go
// projectsByOrganization: no shaping, `_id` marshals as the hex string).
type RawProject = {
  _id?: string
  id?: string
  name?: string
  status?: string
  createdAt?: string
}

// GET /admin/user (handler.go listRaw "users") — for the add-member picker. The add-member body
// takes the user's id (organization.go addOrganizationMemberReq {userId, role}).
type AdminUser = {
  id?: string
  sub?: string
  email?: string
  firstName?: string
  lastName?: string
}

const docId = (d: { _id?: string; id?: string }) => d.id ?? d._id

const MEMBER_ROLES = ["OWNER", "MEMBER"] as const

function Field({ label, value, mono }: { label: string; value?: React.ReactNode; mono?: boolean }) {
  return (
    <div>
      <p className="text-eyebrow mb-1">{label}</p>
      <p className={mono ? "font-mono text-xs" : "text-sm"}>{value || "—"}</p>
    </div>
  )
}

function ErrorPanel({ error }: { error: unknown }) {
  return (
    <div className="rounded-lg border bg-muted/40 p-6 text-sm text-muted-foreground">
      {error instanceof Error ? error.message : "Something went wrong"}
    </div>
  )
}

export default function OrganizationDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const qc = useQueryClient()

  const orgPath = `/admin/organizations/${id}`
  const [tab, setTab] = useTabParam("overview")
  const { data: org, isLoading, error } = useAdminGet<OrgDetail>(orgPath, !!id)
  const membersPath = `${orgPath}/members`
  const members = useAdminList<OrgMember>(membersPath, !!id)
  const projects = useAdminList<RawProject>(`/admin/project/by-organization?organizationId=${id}`, !!id)

  const [editOpen, setEditOpen] = useState(false)
  const [editForm, setEditForm] = useState({ name: "", description: "" })
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [addMemberOpen, setAddMemberOpen] = useState(false)
  const [memberChoice, setMemberChoice] = useState("")
  const [memberRole, setMemberRole] = useState("MEMBER")
  const [memberToRemove, setMemberToRemove] = useState<OrgMember | null>(null)
  const [roleChange, setRoleChange] = useState<{ member: OrgMember; role: string } | null>(null)

  const users = useAdminList<AdminUser>("/admin/user", addMemberOpen)

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["admin-get", orgPath] })
    qc.invalidateQueries({ queryKey: ["admin-list", membersPath] })
    qc.invalidateQueries({ queryKey: ["admin-list", "/admin/organizations"] })
  }

  // PUT /admin/organizations/{id} (organization.go organizationUpdate) — only non-null fields change.
  const updateOrg = useMutation({
    mutationFn: () =>
      apiFetch(orgPath, {
        method: "PUT",
        body: { name: editForm.name, description: editForm.description },
      }),
    onSuccess: () => {
      toast.success("Organization updated")
      setEditOpen(false)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // DELETE /admin/organizations/{id} — 400 when the org still has projects.
  const deleteOrg = useMutation({
    mutationFn: () => apiFetch(orgPath, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Organization deleted")
      navigate("/clients/organizations")
    },
    onError: (e: Error) => {
      setDeleteOpen(false)
      toast.error(e.message)
    },
  })

  // POST /admin/organizations/{id}/member {userId, role} (organization.go organizationAddMember).
  const addMember = useMutation({
    mutationFn: () =>
      apiFetch(`${orgPath}/member`, { method: "POST", body: { userId: memberChoice, role: memberRole } }),
    onSuccess: () => {
      toast.success("Member added")
      setAddMemberOpen(false)
      setMemberChoice("")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // DELETE /admin/organizations/{id}/member/{userSub} — owners cannot be removed (400).
  const removeMember = useMutation({
    mutationFn: (sub: string) => apiFetch(`${orgPath}/member/${encodeURIComponent(sub)}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Member removed")
      setMemberToRemove(null)
      invalidate()
    },
    onError: (e: Error) => {
      setMemberToRemove(null)
      toast.error(e.message)
    },
  })

  // PUT /admin/organizations/{id}/member/{userSub}/role {role} (organizationUpdateMemberRole).
  const changeRole = useMutation({
    mutationFn: ({ sub, role }: { sub: string; role: string }) =>
      apiFetch(`${orgPath}/member/${encodeURIComponent(sub)}/role`, { method: "PUT", body: { role } }),
    onSuccess: () => {
      toast.success("Member role updated")
      setRoleChange(null)
      invalidate()
    },
    onError: (e: Error) => {
      setRoleChange(null)
      toast.error(e.message)
    },
  })

  return (
    <>
      <PageHeader
        title={org?.name ?? (isLoading ? "Loading…" : "Organization")}
        eyebrow="Clients"
        description="Client organization detail."
        breadcrumb={
          <Breadcrumb>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink asChild>
                  <Link to="/clients/organizations">Organizations</Link>
                </BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{org?.name ?? id}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              disabled={!org}
              onClick={() => {
                setEditForm({ name: org?.name ?? "", description: org?.description ?? "" })
                setEditOpen(true)
              }}
            >
              <Pencil className="size-4" /> Edit
            </Button>
            <Button variant="destructive" size="sm" disabled={!org} onClick={() => setDeleteOpen(true)}>
              <Trash2 className="size-4" /> Delete organization
            </Button>
          </>
        }
      />

      {isLoading ? (
        <Skeleton className="h-64" />
      ) : error ? (
        <ErrorPanel error={error} />
      ) : (
        <Tabs value={tab} onValueChange={setTab}>
          <TabsList>
            <TabsTrigger value="overview">Overview</TabsTrigger>
            <TabsTrigger value="members">
              Members{org?.memberCount != null ? ` (${org.memberCount})` : ""}
            </TabsTrigger>
            <TabsTrigger value="projects">
              Projects{org?.projectCount != null ? ` (${org.projectCount})` : ""}
            </TabsTrigger>
          </TabsList>

          <TabsContent value="overview" className="mt-4 space-y-6">
            <Card>
              <CardHeader>
                <CardTitle className="text-base">Organization</CardTitle>
              </CardHeader>
              <CardContent className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                <Field label="Name" value={org?.name} />
                <Field label="Description" value={org?.description} />
                <Field label="ID" value={org?.id} mono />
                <Field label="Members" value={<span className="tabular-nums">{org?.memberCount ?? 0}</span>} />
                <Field label="Projects" value={<span className="tabular-nums">{org?.projectCount ?? 0}</span>} />
                <Field label="Created" value={fmtDateTime(org?.createdAt)} />
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="text-base">Billing profile</CardTitle>
              </CardHeader>
              <CardContent>
                {org?.billingProfileId ? (
                  <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
                    <div>
                      <p className="text-eyebrow mb-1">Profile</p>
                      <p className="text-sm">
                        <Link
                          to={`/clients/billing-profiles/${org.billingProfileId}`}
                          className="inline-block py-1 font-mono text-xs underline-offset-2 hover:underline"
                        >
                          {org.billingProfileId}
                        </Link>
                      </p>
                    </div>
                    <Field
                      label="Name"
                      value={org.billingProfile?.name ?? org.billingProfile?.email}
                    />
                    <div>
                      <p className="text-eyebrow mb-1">Status</p>
                      <StatusBadge status={org.billingProfile?.status} />
                    </div>
                    <Field label="Currency" value={org.billingProfile?.currency} />
                  </div>
                ) : (
                  <p className="text-sm text-muted-foreground">No billing profile linked to this organization.</p>
                )}
              </CardContent>
            </Card>
          </TabsContent>

          <TabsContent value="members" className="mt-4">
            <div className="mb-3 flex justify-end">
              <Button size="sm" onClick={() => setAddMemberOpen(true)}>
                <Plus className="size-4" /> Add member
              </Button>
            </div>
            {members.isLoading ? (
              <Skeleton className="h-32" />
            ) : members.error ? (
              <ErrorPanel error={members.error} />
            ) : (members.data?.data ?? []).length === 0 ? (
              <EmptyState icon={Users} title="No members" hint="Add a user to this organization." />
            ) : (
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Member</TableHead>
                      <TableHead>Sub</TableHead>
                      <TableHead className="w-36">Role</TableHead>
                      <TableHead className="w-10" />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(members.data?.data ?? []).map((m, i) => (
                      <TableRow key={m.sub ?? i}>
                        <TableCell>
                          <span className="font-medium">
                            {[m.firstName, m.lastName].filter(Boolean).join(" ") || m.email || "—"}
                          </span>
                          {m.email && (m.firstName || m.lastName) ? (
                            <span className="ml-2 text-xs text-muted-foreground">{m.email}</span>
                          ) : null}
                        </TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">{m.sub ?? "—"}</TableCell>
                        <TableCell>
                          <Select
                            value={(m.role ?? "MEMBER").toUpperCase()}
                            onValueChange={(role) => m.sub && setRoleChange({ member: m, role })}
                          >
                            <SelectTrigger
                              className="h-8 w-32"
                              aria-label={`Role for ${m.email ?? m.sub ?? "member"}`}
                            >
                              <SelectValue />
                            </SelectTrigger>
                            <SelectContent>
                              {MEMBER_ROLES.map((r) => (
                                <SelectItem key={r} value={r}>
                                  {r.charAt(0) + r.slice(1).toLowerCase()}
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                        </TableCell>
                        <TableCell>
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label={`Remove ${m.email ?? m.sub ?? "member"}`}
                            disabled={(m.role ?? "").toUpperCase() === "OWNER"}
                            onClick={() => setMemberToRemove(m)}
                          >
                            <Trash2 className="size-4 text-muted-foreground" />
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </Card>
            )}
          </TabsContent>

          <TabsContent value="projects" className="mt-4">
            {projects.isLoading ? (
              <Skeleton className="h-32" />
            ) : projects.error ? (
              <ErrorPanel error={projects.error} />
            ) : (projects.data?.data ?? []).length === 0 ? (
              <EmptyState icon={FolderKanban} title="No projects" hint="This organization has no projects." />
            ) : (
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Name</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>ID</TableHead>
                      <TableHead>Created</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(projects.data?.data ?? []).map((p) => (
                      <TableRow
                        key={docId(p)}
                        className="cursor-pointer"
                        onClick={() => docId(p) && navigate(`/clients/projects/${docId(p)}`)}
                      >
                        <TableCell>
                          {docId(p) ? (
                            <Link
                              to={`/clients/projects/${docId(p)}`}
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
                          <StatusBadge status={p.status} />
                        </TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">{docId(p) ?? "—"}</TableCell>
                        <TableCell className="text-muted-foreground">{fmtDate(p.createdAt)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </Card>
            )}
          </TabsContent>
        </Tabs>
      )}

      {/* Edit organization */}
      <Dialog open={editOpen} onOpenChange={setEditOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Edit organization</DialogTitle>
            <DialogDescription>Updates the organization's name and description.</DialogDescription>
          </DialogHeader>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault()
              updateOrg.mutate()
            }}
          >
            <div className="space-y-2">
              <Label htmlFor="edit-org-name">Name</Label>
              <Input
                id="edit-org-name"
                autoComplete="off"
                required
                value={editForm.name}
                onChange={(e) => setEditForm({ ...editForm, name: e.target.value })}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="edit-org-desc">Description</Label>
              <Input
                id="edit-org-desc"
                autoComplete="off"
                value={editForm.description}
                onChange={(e) => setEditForm({ ...editForm, description: e.target.value })}
              />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setEditOpen(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={updateOrg.isPending || !editForm.name}>
                {updateOrg.isPending ? "Saving…" : "Save changes"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Delete confirm */}
      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete organization</DialogTitle>
            <DialogDescription>
              This permanently deletes {org?.name ?? "this organization"} and its memberships. Organizations that
              still have projects cannot be deleted.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteOpen(false)}>
              Cancel
            </Button>
            <Button variant="destructive" disabled={deleteOrg.isPending} onClick={() => deleteOrg.mutate()}>
              {deleteOrg.isPending ? "Deleting…" : "Delete organization"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Add member */}
      <Dialog open={addMemberOpen} onOpenChange={setAddMemberOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add member</DialogTitle>
            <DialogDescription>Adds an existing user to {org?.name ?? "this organization"}.</DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="add-member-user">User</Label>
              <Select value={memberChoice} onValueChange={setMemberChoice}>
                <SelectTrigger id="add-member-user" className="w-full">
                  <SelectValue placeholder={users.isLoading ? "Loading users…" : "Pick a user"} />
                </SelectTrigger>
                <SelectContent>
                  {(users.data?.data ?? []).map((u) =>
                    u.id ? (
                      <SelectItem key={u.id} value={u.id}>
                        {u.email ?? [u.firstName, u.lastName].filter(Boolean).join(" ") ?? u.id}
                      </SelectItem>
                    ) : null,
                  )}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="add-member-role">Role</Label>
              <Select value={memberRole} onValueChange={setMemberRole}>
                <SelectTrigger id="add-member-role" className="w-full">
                  <SelectValue placeholder="Role" />
                </SelectTrigger>
                <SelectContent>
                  {MEMBER_ROLES.map((r) => (
                    <SelectItem key={r} value={r}>
                      {r.charAt(0) + r.slice(1).toLowerCase()}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddMemberOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!memberChoice || addMember.isPending} onClick={() => addMember.mutate()}>
              {addMember.isPending ? "Adding…" : "Add member"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Remove member confirm */}
      <Dialog open={!!memberToRemove} onOpenChange={(o) => !o && setMemberToRemove(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Remove member</DialogTitle>
            <DialogDescription>
              Removes {memberToRemove?.email ?? memberToRemove?.sub ?? "this user"} from{" "}
              {org?.name ?? "the organization"}.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setMemberToRemove(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={removeMember.isPending}
              onClick={() => memberToRemove?.sub && removeMember.mutate(memberToRemove.sub)}
            >
              {removeMember.isPending ? "Removing…" : "Remove member"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Change role confirm */}
      <Dialog open={!!roleChange} onOpenChange={(o) => !o && setRoleChange(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Change member role</DialogTitle>
            <DialogDescription>
              Sets {roleChange?.member.email ?? roleChange?.member.sub ?? "this member"}'s role to{" "}
              <span className="font-medium">{roleChange?.role.toLowerCase()}</span>.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRoleChange(null)}>
              Cancel
            </Button>
            <Button
              disabled={changeRole.isPending}
              onClick={() =>
                roleChange?.member.sub && changeRole.mutate({ sub: roleChange.member.sub, role: roleChange.role })
              }
            >
              {changeRole.isPending ? "Saving…" : "Change role"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
