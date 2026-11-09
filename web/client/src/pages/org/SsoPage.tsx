import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Check, Copy, Plus, ShieldCheck, Trash2, X } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { ApiError, apiFetch } from "@/lib/api"
import { useProjectId } from "@/lib/hooks"
import { useOrg } from "./MembersPage"
import type { OrgRole } from "./RolesPage"

type DomainClaim = {
  domain: string
  verified: boolean
  verifiedAt?: string
  recordName: string
  recordValue: string
}

type SsoConfig = {
  configured: boolean
  alias: string
  redirectUri: string
  spMetadataUrl: string
  protocol?: "oidc" | "saml"
  discoveryUrl?: string
  clientId?: string
  hasSecret: boolean
  scopes?: string
  providerName?: string
  defaultRole?: string
  groupsClaim?: string
  groupRoleMap: Record<string, string>
  autoProvision: boolean
  trustEmail: boolean
  enabled: boolean
  enforceSso: boolean
  breakGlassSubs: string[]
  operatorDisabled: boolean
  domains: DomainClaim[]
}

type Member = { sub: string; role: string; email?: string; firstName?: string; lastName?: string }

// Only roles the server would accept: anything able to manage members or roles is refused.
function grantable(r: OrgRole) {
  if (r.builtIn) return r.name === "MEMBER"
  return !r.expandedPermissions.some((p) => p === "organization:manage_members" || p === "organization:manage_roles")
}

function CopyRow({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div>
      <Label className="text-xs text-muted-foreground">{label}</Label>
      <div className="mt-1 flex items-center gap-2">
        <code className="min-w-0 flex-1 truncate rounded-md border bg-muted/50 px-2 py-1.5 font-mono text-xs">{value}</code>
        <Button
          variant="outline"
          size="icon"
          className="size-8 shrink-0"
          aria-label={`Copy ${label}`}
          onClick={() => {
            void navigator.clipboard?.writeText(value)
            setCopied(true)
            setTimeout(() => setCopied(false), 1500)
          }}
        >
          {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
        </Button>
      </div>
    </div>
  )
}

function ToggleRow({ id, label, hint, checked, onChange, disabled }: {
  id: string; label: string; hint: string; checked: boolean; onChange: (v: boolean) => void; disabled?: boolean
}) {
  return (
    <div className="flex items-start justify-between gap-4">
      <div>
        <Label htmlFor={id}>{label}</Label>
        <p className="mt-0.5 text-xs text-muted-foreground">{hint}</p>
      </div>
      <Switch id={id} checked={checked} onCheckedChange={onChange} disabled={disabled} />
    </div>
  )
}

function DomainsCard({ orgId, domains }: { orgId: string; domains: DomainClaim[] }) {
  const qc = useQueryClient()
  const [domain, setDomain] = useState("")
  const path = `/organizations/${orgId}/sso/domains`
  const refresh = () => qc.invalidateQueries({ queryKey: ["org-sso", orgId] })
  const claim = useMutation({
    mutationFn: () => apiFetch(path, { method: "POST", body: { domain: domain.trim() } }),
    onSuccess: () => { setDomain(""); refresh() },
    onError: (e) => toast.error((e as Error).message),
  })
  const verify = useMutation({
    mutationFn: (d: string) => apiFetch(`${path}/${encodeURIComponent(d)}/verify`, { method: "POST" }),
    onSuccess: () => { toast.success("Domain verified"); refresh() },
    onError: (e) => toast.error((e as Error).message),
  })
  const remove = useMutation({
    mutationFn: (d: string) => apiFetch(`${path}/${encodeURIComponent(d)}`, { method: "DELETE" }),
    onSuccess: refresh,
    onError: (e) => toast.error((e as Error).message),
  })
  return (
    <Card className="flex flex-col gap-4 p-5">
      <div>
        <h2 className="font-medium">Email domains</h2>
        <p className="text-sm text-muted-foreground">
          Your identity provider can only sign in addresses in domains you have proved you own. Publish the TXT record, then verify.
        </p>
      </div>
      {domains.map((d) => (
        <div key={d.domain} className="flex flex-col gap-3 rounded-md border p-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="flex items-center gap-2">
              <span className="font-mono text-sm">{d.domain}</span>
              <Badge variant={d.verified ? "default" : "outline"}>{d.verified ? "Verified" : "Pending"}</Badge>
            </div>
            <div className="flex items-center gap-2">
              {!d.verified && (
                <Button size="sm" variant="outline" disabled={verify.isPending} onClick={() => verify.mutate(d.domain)}>Verify</Button>
              )}
              <Button size="icon-sm" variant="ghost" aria-label={`Remove ${d.domain}`} onClick={() => remove.mutate(d.domain)}>
                <Trash2 className="size-4" />
              </Button>
            </div>
          </div>
          {!d.verified && (
            <div className="grid gap-3 md:grid-cols-2">
              <CopyRow label="TXT record name" value={d.recordName} />
              <CopyRow label="TXT record value" value={d.recordValue} />
            </div>
          )}
        </div>
      ))}
      <form
        className="flex items-center gap-2"
        onSubmit={(e) => { e.preventDefault(); if (domain.trim()) claim.mutate() }}
      >
        <Input aria-label="Domain to claim" placeholder="example.com" value={domain} onChange={(e) => setDomain(e.target.value)} />
        <Button type="submit" variant="outline" disabled={!domain.trim() || claim.isPending}>
          <Plus className="size-4" /> Add domain
        </Button>
      </form>
    </Card>
  )
}

function ProviderForm({ orgId, cfg }: { orgId: string; cfg: SsoConfig }) {
  const qc = useQueryClient()
  const [protocol, setProtocol] = useState<"oidc" | "saml">(cfg.protocol ?? "oidc")
  const [discoveryUrl, setDiscoveryUrl] = useState(cfg.discoveryUrl ?? "")
  const [clientId, setClientId] = useState(cfg.clientId ?? "")
  const [clientSecret, setClientSecret] = useState("")
  const [scopes, setScopes] = useState(cfg.scopes ?? "")
  const [providerName, setProviderName] = useState(cfg.providerName ?? "")
  const [defaultRole, setDefaultRole] = useState(cfg.defaultRole ?? "MEMBER")
  const [groupsClaim, setGroupsClaim] = useState(cfg.groupsClaim ?? "groups")
  const [groupRows, setGroupRows] = useState<[string, string][]>(Object.entries(cfg.groupRoleMap))
  const [autoProvision, setAutoProvision] = useState(cfg.configured ? cfg.autoProvision : true)
  const [trustEmail, setTrustEmail] = useState(cfg.configured ? cfg.trustEmail : true)
  const [enabled, setEnabled] = useState(cfg.configured ? cfg.enabled : true)
  const [enforceSso, setEnforceSso] = useState(cfg.enforceSso)
  const [breakGlass, setBreakGlass] = useState<string[]>(cfg.breakGlassSubs)

  const { data: roles } = useQuery({
    queryKey: ["org-roles", orgId],
    queryFn: () => apiFetch<OrgRole[]>(`/organizations/${orgId}/roles`),
  })
  const { data: members } = useQuery({
    queryKey: ["org-members", orgId],
    queryFn: () => apiFetch<Member[]>(`/organizations/${orgId}/members`),
  })
  const roleNames = useMemo(() => (roles ?? []).filter(grantable).map((r) => r.name), [roles])
  const owners = useMemo(() => (members ?? []).filter((m) => m.role === "OWNER"), [members])

  const refresh = () => qc.invalidateQueries({ queryKey: ["org-sso", orgId] })
  const save = useMutation({
    mutationFn: () =>
      apiFetch(`/organizations/${orgId}/sso`, {
        method: "PUT",
        body: {
          protocol, discoveryUrl, clientId, clientSecret, scopes, providerName, defaultRole, groupsClaim,
          groupRoleMap: Object.fromEntries(groupRows.filter(([g, r]) => g.trim() && r)),
          autoProvision, trustEmail, enabled, enforceSso, breakGlassSubs: breakGlass,
        },
      }),
    onSuccess: () => { setClientSecret(""); toast.success("Single sign-on saved"); refresh() },
    onError: (e) => toast.error((e as Error).message),
  })
  const test = useMutation({
    mutationFn: () =>
      apiFetch<{ ok: boolean; issuer?: string; error?: string }>(`/organizations/${orgId}/sso/test`, {
        method: "POST",
        body: { protocol, discoveryUrl },
      }),
    onSuccess: (r) => (r.ok ? toast.success(r.issuer ? `Reached ${r.issuer}` : "Metadata looks valid") : toast.error(r.error ?? "Test failed")),
    onError: (e) => toast.error((e as Error).message),
  })
  const remove = useMutation({
    mutationFn: () => apiFetch(`/organizations/${orgId}/sso`, { method: "DELETE" }),
    onSuccess: () => { toast.success("Single sign-on removed"); refresh() },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <>
      <Card className="flex flex-col gap-4 p-5">
        <div>
          <h2 className="font-medium">Identity provider</h2>
          <p className="text-sm text-muted-foreground">Register ProxCloud at your provider, then enter what it gives you.</p>
        </div>
        <div className="grid gap-3 md:grid-cols-2">
          <CopyRow label="Redirect URI (OIDC)" value={cfg.redirectUri} />
          <CopyRow label="Service provider metadata (SAML)" value={cfg.spMetadataUrl} />
        </div>
        <div className="grid gap-4 md:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="sso-protocol">Protocol</Label>
            <Select value={protocol} onValueChange={(v) => setProtocol(v as "oidc" | "saml")}>
              <SelectTrigger id="sso-protocol"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="oidc">OpenID Connect</SelectItem>
                <SelectItem value="saml">SAML 2.0</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="sso-name">Display name</Label>
            <Input id="sso-name" placeholder="Company SSO" value={providerName} onChange={(e) => setProviderName(e.target.value)} />
          </div>
          <div className="flex flex-col gap-1.5 md:col-span-2">
            <Label htmlFor="sso-url">{protocol === "oidc" ? "Discovery URL" : "Metadata URL"}</Label>
            <div className="flex items-center gap-2">
              <Input
                id="sso-url"
                placeholder={protocol === "oidc" ? "https://idp.example.com/.well-known/openid-configuration" : "https://idp.example.com/metadata.xml"}
                value={discoveryUrl}
                onChange={(e) => setDiscoveryUrl(e.target.value)}
              />
              <Button variant="outline" disabled={!discoveryUrl.trim() || test.isPending} onClick={() => test.mutate()}>Test</Button>
            </div>
          </div>
          {protocol === "oidc" && (
            <>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="sso-client">Client ID</Label>
                <Input id="sso-client" value={clientId} onChange={(e) => setClientId(e.target.value)} />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="sso-secret">Client secret</Label>
                <Input
                  id="sso-secret"
                  type="password"
                  autoComplete="off"
                  placeholder={cfg.hasSecret ? "Stored; leave blank to keep" : ""}
                  value={clientSecret}
                  onChange={(e) => setClientSecret(e.target.value)}
                />
              </div>
              <div className="flex flex-col gap-1.5 md:col-span-2">
                <Label htmlFor="sso-scopes">Scopes</Label>
                <Input id="sso-scopes" placeholder="openid email profile" value={scopes} onChange={(e) => setScopes(e.target.value)} />
              </div>
            </>
          )}
        </div>
        <ToggleRow
          id="sso-enabled"
          label="Enabled"
          hint={cfg.operatorDisabled ? "The platform operator turned this provider off." : "People with a verified email domain sign in through this provider."}
          checked={enabled}
          onChange={setEnabled}
          disabled={cfg.operatorDisabled}
        />
        <ToggleRow
          id="sso-trust"
          label="Trust email from the provider"
          hint="Accept the address the provider sends as verified. Turn off only if your deployment can send verification mail."
          checked={trustEmail}
          onChange={setTrustEmail}
        />
      </Card>

      <Card className="flex flex-col gap-4 p-5">
        <div>
          <h2 className="font-medium">Provisioning</h2>
          <p className="text-sm text-muted-foreground">
            SSO adds people to this organization on first sign-in. It never grants owner or admin, and it never changes the role of someone already here.
          </p>
        </div>
        <ToggleRow
          id="sso-auto"
          label="Add people on first sign-in"
          hint="When off, an administrator adds each person by hand."
          checked={autoProvision}
          onChange={setAutoProvision}
        />
        <div className="grid gap-4 md:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="sso-role">Default role</Label>
            <Select value={defaultRole} onValueChange={setDefaultRole}>
              <SelectTrigger id="sso-role"><SelectValue /></SelectTrigger>
              <SelectContent>
                {roleNames.map((r) => <SelectItem key={r} value={r}>{r}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="sso-claim">{protocol === "oidc" ? "Groups claim" : "Groups attribute"}</Label>
            <Input id="sso-claim" value={groupsClaim} onChange={(e) => setGroupsClaim(e.target.value)} />
          </div>
        </div>
        <div className="flex flex-col gap-2">
          <Label>Group to role</Label>
          {groupRows.map(([group, role], i) => (
            <div key={i} className="flex items-center gap-2">
              <Input
                aria-label="Group name"
                placeholder="Group name"
                value={group}
                onChange={(e) => setGroupRows(groupRows.map((row, j) => (j === i ? [e.target.value, row[1]] : row)))}
              />
              <Select value={role} onValueChange={(v) => setGroupRows(groupRows.map((row, j) => (j === i ? [row[0], v] : row)))}>
                <SelectTrigger className="w-48" aria-label="Role for this group"><SelectValue placeholder="Role" /></SelectTrigger>
                <SelectContent>
                  {roleNames.map((r) => <SelectItem key={r} value={r}>{r}</SelectItem>)}
                </SelectContent>
              </Select>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="Remove mapping"
                onClick={() => setGroupRows(groupRows.filter((_, j) => j !== i))}
              >
                <X className="size-4" />
              </Button>
            </div>
          ))}
          <div>
            <Button variant="outline" size="sm" onClick={() => setGroupRows([...groupRows, ["", ""]])}>
              <Plus className="size-4" /> Add mapping
            </Button>
          </div>
        </div>
      </Card>

      <Card className="flex flex-col gap-4 p-5">
        <div>
          <h2 className="font-medium">Enforcement</h2>
          <p className="text-sm text-muted-foreground">
            Require SSO for members and for everyone with a verified domain address. Passwords and API tokens stop working for them.
          </p>
        </div>
        <ToggleRow
          id="sso-enforce"
          label="Require single sign-on"
          hint="Needs an enabled provider, a verified domain and at least one break-glass owner."
          checked={enforceSso}
          onChange={setEnforceSso}
        />
        <div className="flex flex-col gap-2">
          <Label>Break-glass owners (at most two)</Label>
          <p className="text-xs text-muted-foreground">
            These owners keep password access in case your provider is down. Every use is written to the audit log.
          </p>
          {owners.map((m) => {
            const on = breakGlass.includes(m.sub)
            return (
              <label key={m.sub} className="flex items-center gap-2 text-sm">
                <Checkbox
                  checked={on}
                  disabled={!on && breakGlass.length >= 2}
                  onCheckedChange={(v) => setBreakGlass(v ? [...breakGlass, m.sub] : breakGlass.filter((s) => s !== m.sub))}
                />
                {m.email ?? m.sub}
              </label>
            )
          })}
        </div>
      </Card>

      <div className="flex items-center justify-between gap-2">
        <Button disabled={save.isPending || !discoveryUrl.trim()} onClick={() => save.mutate()}>Save</Button>
        {cfg.configured && (
          <Button variant="outline" disabled={remove.isPending} onClick={() => remove.mutate()}>
            <Trash2 className="size-4" /> Remove single sign-on
          </Button>
        )}
      </div>
    </>
  )
}

export default function SsoPage() {
  const pid = useProjectId()
  const { org, isLoading: orgLoading } = useOrg(pid)
  const { data: cfg, isLoading, error } = useQuery({
    queryKey: ["org-sso", org?.id],
    queryFn: () => apiFetch<SsoConfig>(`/organizations/${org?.id}/sso`),
    enabled: !!org?.id,
    retry: false,
  })
  const status = error instanceof ApiError ? error.status : 0

  return (
    <div className="flex flex-col gap-5">
      <PageHeader
        title="Single sign-on"
        eyebrow="Organization"
        description="Sign your people in through your own identity provider."
      />
      {(isLoading || orgLoading) && <Skeleton className="h-64 w-full" />}
      {status === 404 && (
        <EmptyState icon={ShieldCheck} title="Not available" hint="Single sign-on is not enabled on this platform." />
      )}
      {error && status !== 404 && (
        <Alert variant="destructive"><AlertDescription>{(error as Error).message}</AlertDescription></Alert>
      )}
      {cfg && org && (
        <>
          {cfg.operatorDisabled && (
            <Alert variant="destructive">
              <AlertDescription>The platform operator disabled this identity provider. Contact them to turn it back on.</AlertDescription>
            </Alert>
          )}
          <DomainsCard orgId={org.id} domains={cfg.domains} />
          <ProviderForm key={cfg.alias} orgId={org.id} cfg={cfg} />
        </>
      )}
    </div>
  )
}
