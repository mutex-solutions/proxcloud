import { useState } from "react"
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Copy, ExternalLink, KeyRound, Pencil, Plus, ScrollText, Terminal, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch, apiFetchEnvelope, apiFetchRaw } from "@/lib/api"
import { config } from "@/lib/config"
import { fmtDate, fmtDateTime } from "@/lib/format"
import { cn } from "@/lib/utils"
import { identityChip } from "../org/AuditPage"

// GET /account/details — RAW endpoint (AccountDetailsDTO, no envelope).
type AccountDetails = {
  id?: string
  sub: string
  createdAt?: string
  customInfo?: Record<string, unknown>
  firstName?: string
  lastName?: string
  email?: string
  language?: string
  gravatarUrl?: string
}

type AuditEvent = {
  id?: string
  timestamp?: string
  action?: string
  resourceType?: string
  resourceDisplayName?: string
  outcome?: string
  actor?: { displayName?: string; ipAddress?: string }
}

// GET /user/api-keys — a Personal Access Token (secret stripped; the plaintext token is only ever
// returned once, from the create response).
type ApiKey = {
  id: string
  description?: string
  createdAt?: string
  lastUsedAt?: string
}

const AUDIT_LIMIT = 25

export default function AccountPage() {
  const qc = useQueryClient()

  const { data: details, isLoading, error } = useQuery({
    queryKey: ["account-details"],
    queryFn: () => apiFetchRaw<AccountDetails>("/account/details"),
  })

  // Security log: GET /account/audit — cursor-paged CursorList envelope.
  // Same escape hatch as the org audit page: server-ordered cursor walk,
  // bare styled table, "Load more", no client sorting.
  const audit = useInfiniteQuery({
    queryKey: ["account-audit"],
    queryFn: ({ pageParam }) =>
      apiFetchEnvelope<AuditEvent[]>(
        `/account/audit?limit=${AUDIT_LIMIT}${pageParam ? `&after=${encodeURIComponent(pageParam)}` : ""}`,
      ),
    initialPageParam: "",
    getNextPageParam: (last) => (last.paging as { nextMarker?: string } | undefined)?.nextMarker ?? undefined,
  })
  const events = audit.data?.pages.flatMap((p) => p.data ?? []) ?? []

  // Edit name dialog — POST /account/name {firstName, lastName} (raw response).
  const [editOpen, setEditOpen] = useState(false)
  const [firstName, setFirstName] = useState("")
  const [lastName, setLastName] = useState("")
  const saveName = useMutation({
    mutationFn: () =>
      apiFetchRaw("/account/name", { method: "POST", body: { firstName: firstName.trim(), lastName: lastName.trim() } }),
    onSuccess: () => {
      toast.success("Name updated")
      setEditOpen(false)
      void qc.invalidateQueries({ queryKey: ["account-details"] })
      void qc.invalidateQueries({ queryKey: ["account-audit"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // Custom info — POST /user/custom-info/{key}?value=… / DELETE /user/custom-info/{key} (enveloped).
  const [addOpen, setAddOpen] = useState(false)
  const [ciKey, setCiKey] = useState("")
  const [ciValue, setCiValue] = useState("")
  const [ciDeleting, setCiDeleting] = useState<string | null>(null)
  const addInfo = useMutation({
    mutationFn: () =>
      apiFetch(`/user/custom-info/${encodeURIComponent(ciKey.trim())}?value=${encodeURIComponent(ciValue)}`, {
        method: "POST",
      }),
    onSuccess: () => {
      toast.success("Custom info saved")
      setAddOpen(false)
      setCiKey("")
      setCiValue("")
      void qc.invalidateQueries({ queryKey: ["account-details"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const deleteInfo = useMutation({
    mutationFn: (key: string) => apiFetch(`/user/custom-info/${encodeURIComponent(key)}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Custom info removed")
      setCiDeleting(null)
      void qc.invalidateQueries({ queryKey: ["account-details"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // API tokens (PATs) — GET/POST/DELETE /user/api-keys (enveloped). A token authenticates the
  // client API + the client MCP toolset (e.g. a Terraform module) as this user; the secret is
  // shown exactly once at create time.
  const tokens = useQuery({
    queryKey: ["api-keys"],
    queryFn: () => apiFetch<ApiKey[]>("/user/api-keys"),
  })
  const [tokenCreateOpen, setTokenCreateOpen] = useState(false)
  const [tokenDesc, setTokenDesc] = useState("")
  const [newToken, setNewToken] = useState<{ id: string; token: string } | null>(null)
  const [tokenDeleting, setTokenDeleting] = useState<ApiKey | null>(null)
  const createToken = useMutation({
    mutationFn: () =>
      apiFetch<{ id: string; token: string }>("/user/api-keys", {
        method: "POST",
        body: { description: tokenDesc.trim() },
      }),
    onSuccess: (res) => {
      setTokenCreateOpen(false)
      setTokenDesc("")
      setNewToken({ id: res.id, token: res.token })
      void qc.invalidateQueries({ queryKey: ["api-keys"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const deleteToken = useMutation({
    mutationFn: (t: ApiKey) => apiFetch(`/user/api-keys/${encodeURIComponent(t.id)}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Token revoked")
      setTokenDeleting(null)
      void qc.invalidateQueries({ queryKey: ["api-keys"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const copyToken = async () => {
    if (!newToken) return
    try {
      await navigator.clipboard.writeText(newToken.token)
      toast.success("Token copied to clipboard")
    } catch {
      toast.error("Couldn't copy — select the token and copy it manually")
    }
  }

  const customInfo = Object.entries(details?.customInfo ?? {})
  const fullName = [details?.firstName, details?.lastName].filter(Boolean).join(" ")
  const avatarInitial = (details?.firstName || details?.email || details?.sub || "?").charAt(0).toUpperCase()
  // config.authIssuer = <keycloak origin>/realms/clients → the Keycloak account console.
  const keycloakAccountUrl = `${config.authIssuer.replace(/\/$/, "")}/account`

  return (
    <>
      <PageHeader
        title="Account"
        eyebrow="Account"
        description="Your profile, security log and custom info."
      />

      {isLoading ? (
        <div className="space-y-6">
          <Skeleton className="h-44" />
          <Skeleton className="h-20" />
          <Skeleton className="h-32" />
        </div>
      ) : error ? (
        <div className="rounded-md border bg-muted/40 p-4 text-sm text-muted-foreground">
          {(error as Error).message}
        </div>
      ) : (
        <div className="space-y-6">
          <Card className="gap-0 overflow-hidden py-0">
            <div className="flex items-center justify-between border-b px-5 py-2.5">
              <span className="text-eyebrow">Profile</span>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => {
                  setFirstName(details?.firstName ?? "")
                  setLastName(details?.lastName ?? "")
                  setEditOpen(true)
                }}
              >
                <Pencil className="size-4" /> Edit name
              </Button>
            </div>
            <div className="flex items-start gap-5 p-5">
              {/* Avatar: gravatar when present, else an identity-hued initial chip. */}
              {details?.gravatarUrl ? (
                <img src={details.gravatarUrl} alt="" className="size-14 shrink-0 rounded-full border" />
              ) : (
                <div
                  aria-hidden
                  className={cn(
                    "flex size-14 shrink-0 items-center justify-center rounded-full border font-display text-xl font-semibold",
                    identityChip(details?.sub ?? "account"),
                  )}
                >
                  {avatarInitial}
                </div>
              )}
              <dl className="grid flex-1 gap-x-8 gap-y-3 text-sm sm:grid-cols-2">
                <div>
                  <dt className="text-muted-foreground">Name</dt>
                  <dd className="font-medium">{fullName || "—"}</dd>
                </div>
                <div>
                  <dt className="text-muted-foreground">Email</dt>
                  <dd className="font-medium">{details?.email ?? "—"}</dd>
                </div>
                <div>
                  <dt className="text-muted-foreground">Subject</dt>
                  <dd className="font-mono text-xs">{details?.sub}</dd>
                </div>
                <div>
                  <dt className="text-muted-foreground">Member since</dt>
                  <dd>{fmtDate(details?.createdAt)}</dd>
                </div>
              </dl>
            </div>
          </Card>

          <Card className="gap-0 overflow-hidden py-0">
            <div className="text-eyebrow border-b px-5 py-3">Password & two-factor auth</div>
            <div className="flex flex-wrap items-center justify-between gap-3 p-5">
              <p className="flex items-center gap-2 text-sm text-muted-foreground">
                <KeyRound className="size-4 shrink-0" strokeWidth={1.5} />
                Password and two-factor authentication are managed by the identity provider.
              </p>
              <Button size="sm" variant="outline" asChild>
                <a href={keycloakAccountUrl} target="_blank" rel="noreferrer">
                  Open account console <ExternalLink className="size-4" />
                </a>
              </Button>
            </div>
          </Card>

          <Card className="gap-0 overflow-hidden py-0">
            <div className="flex items-center justify-between border-b px-5 py-2.5">
              <span className="text-eyebrow">API tokens</span>
              <Button size="sm" variant="ghost" onClick={() => setTokenCreateOpen(true)}>
                <Plus className="size-4" /> Create token
              </Button>
            </div>
            <p className="px-5 pt-4 text-sm text-muted-foreground">
              Personal access tokens authenticate the API and the MCP endpoint as you — for scripts,
              CI, or a Terraform module — without an interactive login. Send as{" "}
              <span className="font-mono text-xs">Authorization: Bearer &lt;token&gt;</span>.
            </p>
            {tokens.isLoading ? (
              <Skeleton className="m-5 h-16" />
            ) : tokens.error ? (
              <p className="p-5 text-sm text-muted-foreground">{(tokens.error as Error).message}</p>
            ) : !tokens.data?.length ? (
              <p className="p-5 text-sm text-muted-foreground">
                No API tokens yet. Create one to authenticate without SSO — the secret is shown only once.
              </p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead>Description</TableHead>
                    <TableHead>Token ID</TableHead>
                    <TableHead>Created</TableHead>
                    <TableHead className="text-right">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {tokens.data.map((t) => (
                    <TableRow key={t.id}>
                      <TableCell className="font-medium">{t.description || "—"}</TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">{t.id}</TableCell>
                      <TableCell className="text-sm text-muted-foreground">{fmtDate(t.createdAt)}</TableCell>
                      <TableCell className="text-right">
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          aria-label={`Revoke token ${t.description || t.id}`}
                          onClick={() => setTokenDeleting(t)}
                        >
                          <Trash2 className="size-4" />
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </Card>

          <Card className="gap-0 overflow-hidden py-0">
            <div className="flex items-center justify-between border-b px-5 py-2.5">
              <span className="text-eyebrow">Custom info</span>
              <Button size="sm" variant="ghost" onClick={() => setAddOpen(true)}>
                <Plus className="size-4" /> Add entry
              </Button>
            </div>
            {!customInfo.length ? (
              <p className="p-5 text-sm text-muted-foreground">
                No custom info entries. Key/value pairs stored on your user record.
              </p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead>Key</TableHead>
                    <TableHead>Value</TableHead>
                    <TableHead className="text-right">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {customInfo.map(([k, v]) => (
                    <TableRow key={k}>
                      <TableCell className="font-mono text-xs">{k}</TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">{String(v)}</TableCell>
                      <TableCell className="text-right">
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          aria-label={`Delete custom info ${k}`}
                          onClick={() => setCiDeleting(k)}
                        >
                          <Trash2 className="size-4" />
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </Card>

          <div>
            <div className="mb-3">
              <h2 className="font-display text-lg font-semibold">Security log</h2>
              <p className="text-sm text-muted-foreground">Recent activity on your account.</p>
            </div>
            {audit.isLoading ? (
              <Skeleton className="h-40" />
            ) : audit.error ? (
              <div className="rounded-md border bg-muted/40 p-4 text-sm text-muted-foreground">
                {(audit.error as Error).message}
              </div>
            ) : !events.length ? (
              <EmptyState icon={ScrollText} title="No security events" hint="Account activity will show up here." />
            ) : (
              <>
                <Card className="overflow-hidden py-0">
                  <Table>
                    <TableHeader>
                      <TableRow className="hover:bg-transparent">
                        <TableHead>Time</TableHead>
                        <TableHead>Event</TableHead>
                        <TableHead>Outcome</TableHead>
                        <TableHead>IP address</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {events.map((e, i) => (
                        <TableRow key={e.id ?? i}>
                          <TableCell className="whitespace-nowrap font-mono text-xs tabular-nums text-muted-foreground">
                            {fmtDateTime(e.timestamp)}
                          </TableCell>
                          <TableCell>
                            <span className="font-mono text-xs">{e.action ?? "—"}</span>
                            {e.resourceType ? (
                              <span className="ml-2 text-xs text-muted-foreground">{e.resourceType}</span>
                            ) : null}
                          </TableCell>
                          <TableCell>
                            <StatusBadge status={e.outcome} />
                          </TableCell>
                          <TableCell className="font-mono text-xs text-muted-foreground">
                            {e.actor?.ipAddress ?? "—"}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </Card>
                <div className="mt-4 flex flex-col items-center gap-2">
                  {audit.hasNextPage ? (
                    <Button
                      variant="outline"
                      onClick={() => void audit.fetchNextPage()}
                      disabled={audit.isFetchingNextPage}
                    >
                      {audit.isFetchingNextPage ? "Loading…" : "Load more"}
                    </Button>
                  ) : null}
                  <p className="font-mono text-xs text-muted-foreground">
                    {events.length} event{events.length === 1 ? "" : "s"} loaded
                    {audit.hasNextPage ? "" : " · end of log"}
                  </p>
                </div>
              </>
            )}
          </div>
        </div>
      )}

      <Dialog open={editOpen} onOpenChange={setEditOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Edit name</DialogTitle>
          </DialogHeader>
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <Label className="mb-1.5 block" htmlFor="account-first-name">First name</Label>
              <Input
                id="account-first-name"
                autoComplete="given-name"
                value={firstName}
                onChange={(e) => setFirstName(e.target.value)}
                maxLength={100}
              />
            </div>
            <div>
              <Label className="mb-1.5 block" htmlFor="account-last-name">Last name</Label>
              <Input
                id="account-last-name"
                autoComplete="family-name"
                value={lastName}
                onChange={(e) => setLastName(e.target.value)}
                maxLength={100}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => saveName.mutate()} disabled={saveName.isPending}>
              {saveName.isPending ? "Saving…" : "Save name"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add custom info</DialogTitle>
            <DialogDescription>A key/value pair stored on your user record.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <Label className="mb-1.5 block" htmlFor="ci-key">Key</Label>
              <Input
                id="ci-key"
                autoComplete="off"
                className="font-mono"
                value={ciKey}
                onChange={(e) => setCiKey(e.target.value)}
                placeholder="lang"
              />
            </div>
            <div>
              <Label className="mb-1.5 block" htmlFor="ci-value">Value</Label>
              <Input
                id="ci-value"
                autoComplete="off"
                className="font-mono"
                value={ciValue}
                onChange={(e) => setCiValue(e.target.value)}
                placeholder="en-us"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => addInfo.mutate()} disabled={!ciKey.trim() || addInfo.isPending}>
              {addInfo.isPending ? "Saving…" : "Save entry"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!ciDeleting} onOpenChange={(o) => !o && setCiDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete custom info</DialogTitle>
            <DialogDescription>
              Remove the entry <span className="font-mono">{ciDeleting}</span> from your user record?
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCiDeleting(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => ciDeleting && deleteInfo.mutate(ciDeleting)}
              disabled={deleteInfo.isPending}
            >
              {deleteInfo.isPending ? "Deleting…" : "Delete entry"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={tokenCreateOpen} onOpenChange={setTokenCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create API token</DialogTitle>
            <DialogDescription>
              Authenticates the API and MCP as you. Give it a description so you can tell it apart later.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="token-desc">Description</Label>
            <Input
              id="token-desc"
              value={tokenDesc}
              onChange={(e) => setTokenDesc(e.target.value)}
              placeholder="terraform / laptop / ci"
              maxLength={200}
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setTokenCreateOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => createToken.mutate()} disabled={!tokenDesc.trim() || createToken.isPending}>
              {createToken.isPending ? "Creating…" : "Create token"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!newToken} onOpenChange={(o) => !o && setNewToken(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Save your API token</DialogTitle>
            <DialogDescription>
              This is the only time the token is shown — copy it now. Send it as{" "}
              <span className="font-mono text-xs">Authorization: Bearer &lt;token&gt;</span>.
            </DialogDescription>
          </DialogHeader>
          <pre className="max-h-32 overflow-auto rounded-lg border bg-muted/50 p-3 font-mono text-xs break-all whitespace-pre-wrap">
            {newToken?.token}
          </pre>
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <Terminal className="size-3.5 shrink-0" /> Works for both the REST API and the MCP endpoint.
          </p>
          <DialogFooter>
            <Button onClick={() => void copyToken()}>
              <Copy className="size-4" /> Copy token
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!tokenDeleting} onOpenChange={(o) => !o && setTokenDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke API token</DialogTitle>
            <DialogDescription>
              Revoke <span className="font-medium">{tokenDeleting?.description || tokenDeleting?.id}</span>?
              Anything using it will stop working immediately. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setTokenDeleting(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => tokenDeleting && deleteToken.mutate(tokenDeleting)}
              disabled={deleteToken.isPending}
            >
              <Trash2 className="size-4" /> {deleteToken.isPending ? "Revoking…" : "Revoke token"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
