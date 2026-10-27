import { useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Building2, FolderKanban, KeyRound, Server, Trash2, UserCog } from "lucide-react"
import { toast } from "sonner"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
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
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { ApiError, apiFetch } from "@/lib/api"
import { fmtDate, fmtDateTime } from "@/lib/format"
import { useAdminGet, useAdminList } from "@/lib/hooks"

// GET /admin/user/{id} (user.go userGet) → the user.User domain JSON.
type UserDetail = {
  id?: string
  sub?: string
  email?: string
  firstName?: string
  lastName?: string
  identities?: Array<{ sub?: string; issuer?: string }>
  createdAt?: string
}

// GET /admin/cloud-resource/user/{userId} → cloud.CloudResource: name/status live under
// data.<typeKey> (data.server.name, data.network.name, …).
type CloudResource = {
  id?: string
  type?: string
  externalId?: string
  region?: string
  data?: Record<string, unknown>
  createdAt?: string
}

// GET /admin/user-management/credentials?sub= (usermanagement.go userCredentialAdminDto).
type Credential = {
  id?: string
  sub?: string
  type?: string
  password?: { configured: boolean }
  totp?: { verified: boolean; deviceName?: string }
  createdAt?: string
}

// GET /admin/organizations/by-member/{sub} — RAW org docs (handler.go orgsByMember: no shaping,
// `_id` marshals as the hex string).
type RawOrg = {
  _id?: string
  id?: string
  name?: string
  billingProfileId?: string
  createdAt?: string
}

// GET /admin/project/by-user?sub= — RAW project docs (handler.go projectsByUser).
type RawProject = {
  _id?: string
  id?: string
  name?: string
  status?: string
  organizationId?: string
  createdAt?: string
}

const docId = (d: { _id?: string; id?: string }) => d.id ?? d._id

function dataField(cr: CloudResource, key: "name" | "status"): string | undefined {
  for (const v of Object.values(cr.data ?? {})) {
    if (v && typeof v === "object") {
      const s = (v as Record<string, unknown>)[key]
      if (typeof s === "string" && s) return s
    }
  }
  return undefined
}

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

export default function UserDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const qc = useQueryClient()

  const { data: user, isLoading, error } = useAdminGet<UserDetail>(`/admin/user/${id}`, !!id)
  const sub = user?.sub ?? ""
  const resources = useAdminList<CloudResource>(`/admin/cloud-resource/user/${id}`, !!id)
  const orgs = useAdminList<RawOrg>(`/admin/organizations/by-member/${encodeURIComponent(sub)}`, !!sub)
  const projects = useAdminList<RawProject>(`/admin/project/by-user?sub=${encodeURIComponent(sub)}`, !!sub)
  const credsPath = `/admin/user-management/credentials?sub=${encodeURIComponent(sub)}`
  const creds = useAdminList<Credential>(credsPath, !!sub)

  const [resetOpen, setResetOpen] = useState(false)
  const [newPassword, setNewPassword] = useState("")
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [credToDelete, setCredToDelete] = useState<Credential | null>(null)

  // PUT /admin/user-management/password?sub= body {newPassword} (usermanagement.go).
  const resetPassword = useMutation({
    mutationFn: () =>
      apiFetch(`/admin/user-management/password?sub=${encodeURIComponent(sub)}`, {
        method: "PUT",
        body: { newPassword },
      }),
    onSuccess: () => {
      toast.success("Password reset")
      setResetOpen(false)
      setNewPassword("")
      qc.invalidateQueries({ queryKey: ["admin-list", credsPath] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // DELETE /admin/user-management/credentials/{credentialId}?sub= (usermanagement.go).
  const deleteCredential = useMutation({
    mutationFn: (credentialId: string) =>
      apiFetch(`/admin/user-management/credentials/${credentialId}?sub=${encodeURIComponent(sub)}`, {
        method: "DELETE",
      }),
    onSuccess: () => {
      toast.success("Credential deleted")
      setCredToDelete(null)
      qc.invalidateQueries({ queryKey: ["admin-list", credsPath] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // POST /admin/user/{id}/impersonate (user.go userImpersonate) — 501 seam on this deployment:
  // this service is a pure OIDC resource server and does not mint local OAuth2 tokens.
  const impersonate = useMutation({
    mutationFn: () => apiFetch<{ url?: string }>(`/admin/user/${id}/impersonate`, { method: "POST" }),
    onSuccess: (d) => {
      if (d?.url) {
        navigator.clipboard?.writeText(d.url)
        toast.success("Impersonation URL copied")
      } else {
        toast.success("Impersonation started")
      }
    },
    onError: (e: Error) => {
      if (e instanceof ApiError && e.status === 501) {
        toast.info("Not available: impersonation is not supported on this deployment.")
      } else {
        toast.error(e.message)
      }
    },
  })

  // DELETE /admin/user/{id} (user.go userDelete).
  const deleteUser = useMutation({
    mutationFn: () => apiFetch(`/admin/user/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("User deleted")
      navigate("/clients/users")
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <>
      <PageHeader
        title={user?.email ?? (isLoading ? "Loading…" : "User")}
        eyebrow="Clients"
        description="Client account detail."
        breadcrumb={
          <Breadcrumb>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink asChild>
                  <Link to="/clients/users">Users</Link>
                </BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{user?.email ?? id}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => impersonate.mutate()}
              disabled={!user || impersonate.isPending}
            >
              <UserCog className="size-4" /> Impersonate
            </Button>
            <Button variant="outline" size="sm" onClick={() => setResetOpen(true)} disabled={!sub}>
              <KeyRound className="size-4" /> Reset password
            </Button>
            <Button variant="destructive" size="sm" onClick={() => setDeleteOpen(true)} disabled={!user}>
              <Trash2 className="size-4" /> Delete user
            </Button>
          </>
        }
      />

      {isLoading ? (
        <Skeleton className="h-40" />
      ) : error ? (
        <ErrorPanel error={error} />
      ) : (
        <div className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Profile</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
              <Field label="Email" value={user?.email} />
              <Field label="First name" value={user?.firstName} />
              <Field label="Last name" value={user?.lastName} />
              <Field label="Sub" value={user?.sub} mono />
              <Field label="Created" value={fmtDateTime(user?.createdAt)} />
              <div>
                <p className="text-eyebrow mb-1">Identity issuers</p>
                <div className="flex flex-wrap gap-1.5">
                  {(user?.identities ?? []).length === 0 ? (
                    <span className="text-sm">—</span>
                  ) : (
                    (user?.identities ?? []).map((idn, i) => (
                      <Badge
                        key={i}
                        variant="secondary"
                        className="max-w-full whitespace-normal break-all font-mono text-xs"
                      >
                        {idn.issuer || "unknown"}
                      </Badge>
                    ))
                  )}
                </div>
              </div>
            </CardContent>
          </Card>

          <div>
            <h2 className="text-eyebrow mb-3">Organizations</h2>
            {orgs.isLoading && sub ? (
              <Skeleton className="h-24" />
            ) : orgs.error ? (
              <ErrorPanel error={orgs.error} />
            ) : (orgs.data?.data ?? []).length === 0 ? (
              <EmptyState icon={Building2} title="No organizations" hint="This user belongs to no organization." />
            ) : (
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Name</TableHead>
                      <TableHead>ID</TableHead>
                      <TableHead>Billing profile</TableHead>
                      <TableHead>Created</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(orgs.data?.data ?? []).map((o) => (
                      <TableRow
                        key={docId(o)}
                        className="cursor-pointer"
                        onClick={() => docId(o) && navigate(`/clients/organizations/${docId(o)}`)}
                      >
                        <TableCell>
                          {docId(o) ? (
                            <Link
                              to={`/clients/organizations/${docId(o)}`}
                              className="inline-block py-1 font-medium hover:underline"
                              onClick={(e) => e.stopPropagation()}
                            >
                              {o.name ?? "—"}
                            </Link>
                          ) : (
                            <span className="font-medium">{o.name ?? "—"}</span>
                          )}
                        </TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">{docId(o) ?? "—"}</TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">
                          {o.billingProfileId ?? "—"}
                        </TableCell>
                        <TableCell className="text-muted-foreground">{fmtDate(o.createdAt)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </Card>
            )}
          </div>

          <div>
            <h2 className="text-eyebrow mb-3">Projects</h2>
            {projects.isLoading && sub ? (
              <Skeleton className="h-24" />
            ) : projects.error ? (
              <ErrorPanel error={projects.error} />
            ) : (projects.data?.data ?? []).length === 0 ? (
              <EmptyState icon={FolderKanban} title="No projects" hint="This user is a member of no project." />
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
          </div>

          <div>
            <h2 className="text-eyebrow mb-3">Cloud resources</h2>
            {resources.isLoading ? (
              <Skeleton className="h-32" />
            ) : resources.error ? (
              <ErrorPanel error={resources.error} />
            ) : (resources.data?.data ?? []).length === 0 ? (
              <EmptyState icon={Server} title="No cloud resources" hint="This user owns no cached cloud resources." />
            ) : (
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Type</TableHead>
                      <TableHead>Name</TableHead>
                      <TableHead>Region</TableHead>
                      <TableHead>Status</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(resources.data?.data ?? []).map((cr) => (
                      <TableRow
                        key={cr.id ?? cr.externalId}
                        className={cr.id ? "cursor-pointer" : undefined}
                        onClick={() => cr.id && navigate(`/clients/cloud-resources/${cr.id}`)}
                      >
                        <TableCell className="font-mono text-xs">{cr.type ?? "—"}</TableCell>
                        <TableCell>
                          {cr.id ? (
                            <Link
                              to={`/clients/cloud-resources/${cr.id}`}
                              className="inline-block py-1 font-medium hover:underline"
                              onClick={(e) => e.stopPropagation()}
                            >
                              {dataField(cr, "name") ?? cr.externalId ?? "—"}
                            </Link>
                          ) : (
                            <span className="font-medium">{dataField(cr, "name") ?? cr.externalId ?? "—"}</span>
                          )}
                        </TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">{cr.region ?? "—"}</TableCell>
                        <TableCell>
                          <StatusBadge status={dataField(cr, "status")} />
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </Card>
            )}
          </div>

          <div>
            <h2 className="text-eyebrow mb-3">Credentials</h2>
            {creds.isLoading && sub ? (
              <Skeleton className="h-24" />
            ) : creds.error ? (
              <ErrorPanel error={creds.error} />
            ) : (creds.data?.data ?? []).length === 0 ? (
              <EmptyState
                icon={KeyRound}
                title="No credentials"
                hint="No stored credentials for this user. Reset the password to create one."
              />
            ) : (
              <Card className="overflow-hidden py-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Type</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead>Created</TableHead>
                      <TableHead className="w-10" />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(creds.data?.data ?? []).map((c, i) => (
                      <TableRow key={c.id ?? i}>
                        <TableCell className="font-mono text-xs">{c.type ?? "—"}</TableCell>
                        <TableCell>
                          {c.password ? (
                            <Badge variant={c.password.configured ? "secondary" : "outline"}>
                              {c.password.configured ? "configured" : "not configured"}
                            </Badge>
                          ) : c.totp ? (
                            <Badge variant={c.totp.verified ? "secondary" : "outline"}>
                              {c.totp.verified ? "verified" : "unverified"}
                              {c.totp.deviceName ? ` · ${c.totp.deviceName}` : ""}
                            </Badge>
                          ) : (
                            "—"
                          )}
                        </TableCell>
                        <TableCell className="text-muted-foreground">{fmtDateTime(c.createdAt)}</TableCell>
                        <TableCell>
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label={`Delete ${c.type ?? ""} credential`}
                            disabled={!c.id}
                            onClick={() => setCredToDelete(c)}
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
          </div>
        </div>
      )}

      {/* Reset password */}
      <Dialog open={resetOpen} onOpenChange={setResetOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Reset password</DialogTitle>
            <DialogDescription>Replaces every stored password credential for {user?.email}.</DialogDescription>
          </DialogHeader>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault()
              resetPassword.mutate()
            }}
          >
            <div className="grid gap-2">
              <Label htmlFor="new-password">New password</Label>
              <Input
                id="new-password"
                type="password"
                autoComplete="new-password"
                required
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
              />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setResetOpen(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={resetPassword.isPending || !newPassword}>
                {resetPassword.isPending ? "Resetting…" : "Reset password"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Delete credential confirm */}
      <Dialog open={!!credToDelete} onOpenChange={(o) => !o && setCredToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete credential</DialogTitle>
            <DialogDescription>
              This removes the {credToDelete?.type?.toLowerCase() ?? ""} credential for {user?.email}. The user may no
              longer be able to sign in with it.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCredToDelete(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={deleteCredential.isPending}
              onClick={() => credToDelete?.id && deleteCredential.mutate(credToDelete.id)}
            >
              {deleteCredential.isPending ? "Deleting…" : "Delete credential"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete confirm */}
      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete user</DialogTitle>
            <DialogDescription>
              This permanently deletes {user?.email ?? "this user"}. Users still attached to projects cannot be
              deleted.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteOpen(false)}>
              Cancel
            </Button>
            <Button variant="destructive" disabled={deleteUser.isPending} onClick={() => deleteUser.mutate()}>
              {deleteUser.isPending ? "Deleting…" : "Delete user"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
