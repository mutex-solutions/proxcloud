import { NavLink } from "react-router-dom"
import { TriangleAlert } from "lucide-react"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import type { ProjectInitResult } from "@/lib/hooks"

type Refusal = Extract<ProjectInitResult, { ok: false }>

// What each /init refusal reason means for the user, and where they can act on it (#216).
function copy(r: Refusal, pid: string): { title: string; action?: { to: string; label: string } } {
  switch (r.reason) {
    case "billing_profile_inactive":
      return { title: "This project is not active yet", action: { to: `/p/${pid}/billing/funds`, label: "Set up billing" } }
    case "billing_profile_suspended":
      return { title: "This project is suspended", action: { to: `/p/${pid}/billing/funds`, label: "Add funds" } }
    case "billing_profile_missing":
      return { title: "This project has no billing profile", action: { to: `/p/${pid}/org/billing`, label: "Organization billing" } }
    case "disabled_by_operator":
      return { title: "This project was suspended by your operator" }
    case "scheduled_for_deletion":
      return { title: "This project is scheduled for deletion", action: { to: `/p/${pid}/org/projects`, label: "Manage projects" } }
    case "deletion_in_progress":
    case "deleted":
      return { title: "This project is being deleted", action: { to: `/p/${pid}/org/projects`, label: "Other projects" } }
    default:
      return { title: "This project is not active" }
  }
}

// Shown above every page of a project that POST /project/{id}/init could not bring up, with the
// server's explanation, so the user is not left with pages that quietly fail.
export function ProjectStatusNotice({ pid, init }: { pid: string; init?: ProjectInitResult }) {
  if (!init || init.ok) return null
  const { title, action } = copy(init, pid)
  return (
    <Alert variant="destructive" className="mb-6" data-testid="project-status-notice" data-reason={init.reason}>
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription>
        <p>{init.message}</p>
        {action && (
          <Button asChild size="sm" variant="outline" className="mt-2">
            <NavLink to={action.to}>{action.label}</NavLink>
          </Button>
        )}
      </AlertDescription>
    </Alert>
  )
}
