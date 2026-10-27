import { useEffect, useState, type ReactNode } from "react"
import { useNavigate } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { BookOpen, Mail, RefreshCw } from "lucide-react"
import { apiFetch } from "@/lib/api"
import { useAuth } from "@/lib/auth"
import { fmtDateTime } from "@/lib/format"
import { useProjects } from "@/lib/hooks"
import type { Organization, Project } from "@/lib/types"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"

// "/" — route to the first project; if the user is in an organization but has no
// project yet (e.g. added to an org, awaiting a project assignment), show a waiting
// state; otherwise walk a brand-new user through organization + project creation.
type Invite = { token: string; projectId: string; projectName?: string; expiresAt?: string }

// Brand frame shared by every "/" state. This page lives outside AppShell (it is
// the landing that decides where to send you), so it opens like Login: design-system
// logo + console chip over the warm background, one focused card.
function BrandShell({ children }: { children: ReactNode }) {
  return (
    <main className="flex min-h-screen items-center justify-center bg-background px-4 py-10 text-foreground">
      <div className="flex w-full max-w-md flex-col gap-8">
        <div className="flex items-center justify-center gap-2.5">
          <img src="/brand/logo.svg" alt="ProxCloud" className="h-6 w-auto" />
          <span className="text-eyebrow rounded border px-1.5 py-0.5">console</span>
        </div>
        {children}
        <SignedInFooter />
      </div>
    </main>
  )
}

// Every pre-project state ("waiting for a project", "invite sent to a different
// email", …) has "wrong account" as its most likely cause — give it an exit.
function SignedInFooter() {
  const auth = useAuth()
  const email = auth.user?.profile.email
  return (
    <p className="text-center text-xs text-muted-foreground">
      {email ? (
        <>
          Signed in as <span className="font-medium text-foreground">{email}</span>
          {" · "}
        </>
      ) : null}
      <button
        type="button"
        className="underline underline-offset-4 transition-colors hover:text-foreground"
        onClick={() => void auth.signoutRedirect()}
      >
        Sign out
      </button>
    </p>
  )
}

// Card opening: eyebrow, display-face title, description, the horizon line.
function BrandCardHeader({
  eyebrow,
  title,
  description,
}: {
  eyebrow: string
  title: string
  description?: ReactNode
}) {
  return (
    <CardHeader>
      <div className="text-eyebrow">{eyebrow}</div>
      <CardTitle className="font-display text-2xl font-semibold tracking-tight">{title}</CardTitle>
      {description ? <CardDescription>{description}</CardDescription> : null}
      <div className="horizon mt-2" />
    </CardHeader>
  )
}

function LoadingCard() {
  return (
    <BrandShell>
      <Card>
        <CardHeader>
          <Skeleton className="h-3 w-20" />
          <Skeleton className="h-7 w-48" />
          <Skeleton className="h-4 w-full" />
        </CardHeader>
        <CardContent className="space-y-3">
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
        </CardContent>
      </Card>
    </BrandShell>
  )
}

export function HomePage() {
  const { data: projects, isLoading } = useProjects()
  const { data: orgs, isLoading: orgsLoading } = useQuery({
    queryKey: ["organizations"],
    queryFn: () => apiFetch<Organization[]>("/organizations"),
  })
  const { data: invites, isLoading: invLoading } = useQuery({
    queryKey: ["my-invites"],
    queryFn: () => apiFetch<Invite[]>("/project-invites/mine"),
  })
  const navigate = useNavigate()

  useEffect(() => {
    if (projects && projects.length > 0) navigate(`/p/${projects[0].id}/dashboard`, { replace: true })
  }, [projects, navigate])

  if (isLoading || orgsLoading || invLoading) return <LoadingCard />
  if (projects && projects.length > 0) return null
  // Pending invitations (logged in directly without the email link) → let them accept here.
  if (invites && invites.length > 0) return <PendingInvites invites={invites} />
  // Member of an organization but no project yet → NOT the create-org onboarding.
  if (orgs && orgs.length > 0) return <NoProjectYet orgs={orgs} />
  return <Onboarding />
}

// Accept a pending project invitation here (no email link needed). Accepting adds the user to
// the project + its organization, after which the projects query repopulates and routes them in.
function PendingInvites({ invites }: { invites: Invite[] }) {
  const qc = useQueryClient()
  const accept = useMutation({
    mutationFn: (token: string) => apiFetch(`/project-invites/accept/${token}`, { method: "POST" }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["projects"] })
      void qc.invalidateQueries({ queryKey: ["organizations"] })
      void qc.invalidateQueries({ queryKey: ["my-invites"] })
      toast.success("Invitation accepted")
    },
    onError: (e: Error) => toast.error(e.message),
  })
  return (
    <BrandShell>
      <Card>
        <BrandCardHeader
          eyebrow="Invitations"
          title="Pending invitations"
          description="You've been invited to join the following projects. Accepting adds you to the project and its organization."
        />
        <CardContent className="space-y-3">
          {invites.map((inv) => (
            <div key={inv.token} className="flex items-center justify-between gap-3 rounded-lg border p-3">
              <div className="flex min-w-0 items-center gap-3">
                <Mail className="size-4 shrink-0 text-muted-foreground" strokeWidth={1.5} />
                <div className="min-w-0">
                  <div className="truncate text-sm font-medium">{inv.projectName ?? "Project"}</div>
                  {inv.expiresAt ? (
                    <div className="text-xs text-muted-foreground">Expires {fmtDateTime(inv.expiresAt)}</div>
                  ) : null}
                </div>
              </div>
              <Button size="sm" disabled={accept.isPending} onClick={() => accept.mutate(inv.token)}>
                {accept.isPending ? "Accepting…" : "Accept"}
              </Button>
            </div>
          ))}
        </CardContent>
      </Card>
    </BrandShell>
  )
}

// The user belongs to an organization but has no project assigned. The client is
// project-scoped, so there is nothing to land on yet — tell them to contact their admin.
function NoProjectYet({ orgs }: { orgs: Organization[] }) {
  const names = orgs.map((o) => o.name).filter(Boolean).join(", ")
  return (
    <BrandShell>
      <Card>
        <BrandCardHeader
          eyebrow="Organization"
          title="Waiting for a project"
          description={
            <>
              You're a member of {names || "your organization"}, but no project has been assigned to
              you yet. Ask your organization admin to add you to a project.
            </>
          }
        />
        <CardContent>
          <Button variant="outline" className="w-full" onClick={() => window.location.reload()}>
            <RefreshCw className="size-4" /> Check again
          </Button>
        </CardContent>
      </Card>
    </BrandShell>
  )
}

function Onboarding() {
  const [orgName, setOrgName] = useState("")
  const [projectName, setProjectName] = useState("")
  const navigate = useNavigate()
  const qc = useQueryClient()

  // Operator-only mode gate: when the platform org quota locks self-service creation
  // (limit 0 + enabled), show a contact note instead of a form that would only 400.
  const selfService = useQuery({
    queryKey: ["org-self-service"],
    queryFn: () => apiFetch<{ canCreateOrganization?: boolean }>("/organizations/self-service"),
  })

  const create = useMutation({
    mutationFn: async () => {
      const org = await apiFetch<Organization>("/organizations", { method: "POST", body: { name: orgName } })
      const project = await apiFetch<Project>("/project", {
        method: "POST",
        body: { name: projectName, organizationId: org.id },
      })
      return project
    },
    onSuccess: (p) => {
      void qc.invalidateQueries({ queryKey: ["projects"] })
      navigate(`/p/${p.id}/dashboard`)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  if (selfService.isLoading) return <LoadingCard />
  if (selfService.data && selfService.data.canCreateOrganization === false) {
    return (
      <BrandShell>
        <Card>
          <BrandCardHeader
            eyebrow="Get started"
            title="Welcome to ProxCloud"
            description="Your account isn't part of any project yet. Organizations on this platform are created by the operator — please contact support or wait for a project invitation."
          />
          <CardContent>
            <Button asChild variant="outline" className="w-full">
              <a href="/docs" target="_blank" rel="noopener noreferrer">
                <BookOpen className="size-4" /> View docs
              </a>
            </Button>
          </CardContent>
        </Card>
      </BrandShell>
    )
  }

  return (
    <BrandShell>
      <Card>
        <BrandCardHeader
          eyebrow="Get started"
          title="Welcome to ProxCloud"
          description="Create your organization and first project to get started."
        />
        <CardContent>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault()
              if (orgName && projectName && !create.isPending) create.mutate()
            }}
          >
            <div className="space-y-2">
              <Label htmlFor="org">Organization name</Label>
              <Input id="org" value={orgName} onChange={(e) => setOrgName(e.target.value)} placeholder="Acme Inc" />
            </div>
            <div className="space-y-2">
              <Label htmlFor="proj">Project name</Label>
              <Input
                id="proj"
                value={projectName}
                onChange={(e) => setProjectName(e.target.value)}
                placeholder="production"
              />
            </div>
            <Button type="submit" className="w-full" disabled={!orgName || !projectName || create.isPending}>
              {create.isPending ? "Creating…" : "Create organization"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </BrandShell>
  )
}
