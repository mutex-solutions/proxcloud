import type { ReactNode } from "react"
import { useNavigate, useParams, useSearchParams } from "react-router-dom"
import { useMutation, useQuery } from "@tanstack/react-query"
import { toast } from "sonner"
import { CheckCircle2, MailX } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { useAuth } from "@/lib/auth"
import { fmtDateTime } from "@/lib/format"

// GET /project-invites/{token} → the invite doc for the CALLER's email + token,
// or {} when there is no matching (or an expired) invite.
type Invite = {
  email?: string
  projectId?: string
  projectName?: string
  expiresAt?: string
}

// Deep-link brand moment: this page renders outside AppShell (email links land
// here before the user has any project context), so it opens like Login — the
// Brand logo + console chip over the warm background, one focused card.
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

// "Sent to a different email address" is this page's most common dead end —
// show which account is signed in and give it an exit.
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

export default function JoinProjectPage() {
  const { token: tokenParam = "" } = useParams()
  const [searchParams] = useSearchParams()
  // email deep-link = /join-project?invite-token=…; direct route = /join/:token
  const token = tokenParam || searchParams.get("invite-token") || ""
  const navigate = useNavigate()

  const { data: invite, isLoading, error } = useQuery({
    queryKey: ["project-invite", token],
    queryFn: () => apiFetch<Invite>(`/project-invites/${encodeURIComponent(token)}`),
    enabled: !!token,
  })

  const accept = useMutation({
    mutationFn: () => apiFetch(`/project-invites/accept/${encodeURIComponent(token)}`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Invite accepted — welcome to the project")
      navigate("/")
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const decline = useMutation({
    mutationFn: () => apiFetch(`/project-invites/decline/${encodeURIComponent(token)}`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Invite declined")
      navigate("/")
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const valid = !!invite?.projectId

  if (isLoading) {
    return (
      <BrandShell>
        <Card>
          <CardHeader>
            <Skeleton className="h-3 w-20" />
            <Skeleton className="h-7 w-44" />
            <Skeleton className="h-4 w-full" />
          </CardHeader>
          <CardContent className="space-y-3">
            <Skeleton className="h-28 w-full" />
            <Skeleton className="h-9 w-full" />
          </CardContent>
        </Card>
      </BrandShell>
    )
  }

  if (error) {
    return (
      <BrandShell>
        <Card>
          <BrandCardHeader
            eyebrow="Invitation"
            title="Couldn't load the invitation"
            description="Something went wrong while looking up this invite."
          />
          <CardContent className="space-y-4">
            <p className="rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
              {(error as Error).message}
            </p>
            <Button variant="outline" className="w-full" onClick={() => navigate("/")}>
              Go to console
            </Button>
          </CardContent>
        </Card>
      </BrandShell>
    )
  }

  if (!valid) {
    return (
      <BrandShell>
        <Card>
          <BrandCardHeader
            eyebrow="Invitation"
            title="Invite not found"
            description="This invitation is invalid, has expired, or was sent to a different email address than the account you are signed in with."
          />
          <CardContent className="flex flex-col items-center gap-6 pt-2">
            <div className="flex size-12 items-center justify-center rounded-full border bg-muted/50">
              <MailX className="size-6 text-muted-foreground" strokeWidth={1.5} />
            </div>
            <Button className="w-full" onClick={() => navigate("/")}>
              Go to console
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
          eyebrow="Invitation"
          title="Join project"
          description="You have been invited to join a project. Accepting adds you to the project and its organization."
        />
        <CardContent>
          <dl className="divide-y rounded-lg border text-sm">
            <div className="flex items-center justify-between gap-4 px-4 py-2.5">
              <dt className="text-muted-foreground">Invited email</dt>
              <dd className="truncate font-medium">{invite?.email ?? "—"}</dd>
            </div>
            <div className="flex items-center justify-between gap-4 px-4 py-2.5">
              <dt className="text-muted-foreground">Project</dt>
              <dd className="truncate">
                {invite?.projectName ? (
                  <span className="font-medium">{invite.projectName}</span>
                ) : (
                  <span className="font-mono text-xs">{invite?.projectId}</span>
                )}
              </dd>
            </div>
            <div className="flex items-center justify-between gap-4 px-4 py-2.5">
              <dt className="text-muted-foreground">Expires</dt>
              <dd>{fmtDateTime(invite?.expiresAt)}</dd>
            </div>
          </dl>
          <div className="mt-6 grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Button
              variant="outline"
              onClick={() => decline.mutate()}
              disabled={decline.isPending || accept.isPending}
            >
              {decline.isPending ? "Declining…" : "Decline"}
            </Button>
            <Button onClick={() => accept.mutate()} disabled={accept.isPending || decline.isPending}>
              <CheckCircle2 className="size-4" />
              {accept.isPending ? "Accepting…" : "Accept invite"}
            </Button>
          </div>
        </CardContent>
      </Card>
    </BrandShell>
  )
}
