import { useParams } from "react-router-dom"
import { ExternalLink, Unplug } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { useAuth } from "@/lib/auth"
import { useBillingSummary, useProjectId, useUIMenu } from "@/lib/hooks"

// Renders an admin-configured Custom Menu item ("More" section). IFRAME
// renderMode embeds the (variable-substituted) URL full-page; other modes get
// a link. Substituted tokens match the old client exactly: {{project.id}},
// {{billingProfile.id}}, {{billingProfile.email}}, {{user.email}}.
export function substituteUrlVariables(
  url: string,
  vars: { projectId: string; billingProfileId?: string; billingProfileEmail?: string; userEmail?: string },
): string {
  return url
    .replaceAll("{{project.id}}", encodeURIComponent(vars.projectId))
    .replaceAll("{{billingProfile.id}}", encodeURIComponent(vars.billingProfileId ?? ""))
    .replaceAll("{{billingProfile.email}}", encodeURIComponent(vars.billingProfileEmail ?? ""))
    .replaceAll("{{user.email}}", encodeURIComponent(vars.userEmail ?? ""))
}

function NotConfigured() {
  return (
    <>
      <PageHeader title="Not available" eyebrow="More" description="This menu item is not configured." />
      <EmptyState
        icon={Unplug}
        title="Nothing to show here"
        hint="An administrator can configure custom menu items for this project in the admin console."
      />
    </>
  )
}

export default function MorePage() {
  const pid = useProjectId()
  const { slug = "" } = useParams()
  const auth = useAuth()
  const { data: init, isLoading } = useUIMenu(pid)
  const { data: summary } = useBillingSummary(pid)

  const item = init?.menu?.items?.[slug] as
    | { displayName?: string; url?: string; renderMode?: string }
    | undefined

  if (isLoading) {
    return (
      <div className="space-y-6">
        <div className="space-y-2">
          <Skeleton className="h-3 w-16" />
          <Skeleton className="h-8 w-56" />
        </div>
        <Skeleton className="h-[420px] w-full rounded-xl" />
      </div>
    )
  }

  if (!item?.url) return <NotConfigured />

  const url = substituteUrlVariables(item.url, {
    projectId: pid,
    billingProfileId: summary?.id as string | undefined,
    billingProfileEmail: summary?.email as string | undefined,
    userEmail: auth.user?.profile.email,
  })

  // The URL is operator-configured, but only ever render web URLs — never javascript: etc.
  if (!url.startsWith("http://") && !url.startsWith("https://")) return <NotConfigured />

  const title = item.displayName ?? slug

  if ((item.renderMode ?? "IFRAME").toUpperCase() !== "IFRAME") {
    return (
      <>
        <PageHeader title={title} eyebrow="More" description="This item opens in a new browser tab." />
        <Button asChild variant="outline">
          <a href={url} target="_blank" rel="noreferrer">
            <ExternalLink className="size-4" /> Open {title}
          </a>
        </Button>
      </>
    )
  }

  // Embedded page: header keeps the console context; the iframe fills the rest
  // of the viewport inside a hairline-bordered card surface.
  return (
    <div className="flex h-[calc(100vh-var(--navbar-height)-3rem)] min-h-[420px] flex-col">
      <PageHeader title={title} eyebrow="More" />
      <div className="min-h-0 flex-1 overflow-hidden rounded-xl border bg-card shadow-card">
        <iframe title={title} src={url} className="size-full border-0" />
      </div>
    </div>
  )
}
