// Design-system escape hatch — DOCUMENTED EXCEPTION to the DataTable rule: the audit
// trail is cursor-paged by the server (paging.nextMarker), so rows arrive in
// server order and client sorting/filtering would lie about the dataset. It
// stays a bare styled <Table> on a card surface with an explicit "Load more"
// cursor walk; only the row presentation is design-system (mono timestamps, actor
// chips, StatusBadge outcomes, mono action verbs).
import { useState } from "react"
import { useInfiniteQuery } from "@tanstack/react-query"
import { toast } from "sonner"
import { Download, ScrollText } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch, apiFetchEnvelope } from "@/lib/api"
import { fmtDateTime } from "@/lib/format"
import { useProjectId } from "@/lib/hooks"
import { cn } from "@/lib/utils"
import { useOrg } from "./MembersPage"

type AuditEvent = {
  id?: string
  timestamp?: string
  action?: string
  resourceType?: string
  resourceId?: string
  resourceDisplayName?: string
  outcome?: string
  actor?: { id?: string; displayName?: string; type?: string }
}

const LIMIT = 50

// WORKAROUND (escalated): Tailwind v4 only emits @utility classes whose
// literal names appear in scanned source, and StatusBadge composes
// `status-dot-${kind}` dynamically — so the error/muted dot flavors were
// never generated and FAILED outcomes rendered dot-less app-wide. Keep the
// full set visible to the scanner until a proper safelist (@source inline in
// index.css) or literal map in status-badge.tsx lands:
// status-dot-ok status-dot-warn status-dot-error status-dot-muted

// Identity hues for actor chips — fixed per-actor assignment via a stable
// hash (design-system identity-* tokens; full literals so Tailwind emits them).
const IDENTITY_CHIP = [
  "bg-identity-1/15 text-identity-1",
  "bg-identity-2/15 text-identity-2",
  "bg-identity-3/15 text-identity-3",
  "bg-identity-4/15 text-identity-4",
  "bg-identity-5/15 text-identity-5",
  "bg-identity-6/15 text-identity-6",
  "bg-identity-7/15 text-identity-7",
  "bg-identity-8/15 text-identity-8",
  "bg-identity-9/15 text-identity-9",
]
export function identityChip(seed: string) {
  let h = 0
  for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) | 0
  return IDENTITY_CHIP[Math.abs(h) % IDENTITY_CHIP.length]
}

/** Initial-avatar chip + display name; the initial's hue is decorative
 * (per-actor identity color), the adjacent name carries the information. */
export function ActorChip({ id, name }: { id?: string; name?: string }) {
  const label = name ?? id
  if (!label) return <span className="text-sm text-muted-foreground">—</span>
  return (
    <span className="inline-flex items-center gap-2">
      <span
        aria-hidden
        className={cn(
          "flex size-5 shrink-0 items-center justify-center rounded-full font-display text-[10px] font-semibold",
          identityChip(id ?? label),
        )}
      >
        {label.charAt(0).toUpperCase()}
      </span>
      <span className="text-sm">{label}</span>
    </span>
  )
}

export default function AuditPage() {
  const pid = useProjectId()
  const { org, isLoading: orgLoading } = useOrg(pid)

  // Cursor-paged: GET /organizations/{orgId}/audit?limit=&after= → paging.nextMarker.
  const { data, isLoading, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: ["org-audit", org?.id],
    queryFn: ({ pageParam }) =>
      apiFetchEnvelope<AuditEvent[]>(
        `/organizations/${org?.id}/audit?limit=${LIMIT}${pageParam ? `&after=${encodeURIComponent(pageParam)}` : ""}`,
      ),
    initialPageParam: "",
    getNextPageParam: (last) => (last.paging as { nextMarker?: string } | undefined)?.nextMarker ?? undefined,
    enabled: !!org?.id,
  })

  const events = data?.pages.flatMap((p) => p.data ?? []) ?? []

  // GET /organizations/{id}/audit/export → text/csv attachment (UTF-8 BOM, capped 10000 events).
  const [exporting, setExporting] = useState(false)
  const exportCsv = async () => {
    if (!org?.id) return
    setExporting(true)
    try {
      const resp = await apiFetch<Response>(`/organizations/${org.id}/audit/export`, { raw: true })
      if (!resp.ok) throw new Error(`Export failed (${resp.status})`)
      const blob = await resp.blob()
      const url = URL.createObjectURL(blob)
      const a = document.createElement("a")
      a.href = url
      a.download = "audit-events.csv"
      a.click()
      URL.revokeObjectURL(url)
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setExporting(false)
    }
  }

  return (
    <>
      <PageHeader
        title="Audit log"
        eyebrow="Organization"
        description={org?.name ? `Activity in the ${org.name} organization.` : "Organization activity."}
        actions={
          <Button size="sm" variant="outline" onClick={() => void exportCsv()} disabled={!org || exporting}>
            <Download className="size-4" /> {exporting ? "Exporting…" : "Export CSV"}
          </Button>
        }
      />

      {orgLoading || isLoading ? (
        <Card className="gap-0 overflow-hidden py-0">
          <div className="space-y-3 p-4">
            {Array.from({ length: 6 }).map((_, i) => (
              <Skeleton key={i} className="h-8" />
            ))}
          </div>
        </Card>
      ) : error ? (
        <div className="rounded-md border bg-muted/40 p-4 text-sm text-muted-foreground">
          {(error as Error).message}
        </div>
      ) : !events.length ? (
        <EmptyState icon={ScrollText} title="No audit events" hint="Actions on this organization will show up here." />
      ) : (
        <>
          <Card className="overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Time</TableHead>
                  <TableHead>Actor</TableHead>
                  <TableHead>Action</TableHead>
                  <TableHead>Resource</TableHead>
                  <TableHead>Outcome</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {events.map((e, i) => (
                  <TableRow key={e.id ?? i}>
                    <TableCell className="whitespace-nowrap font-mono text-xs tabular-nums text-muted-foreground">
                      {fmtDateTime(e.timestamp)}
                    </TableCell>
                    <TableCell>
                      <ActorChip id={e.actor?.id} name={e.actor?.displayName} />
                    </TableCell>
                    <TableCell>
                      <span className="font-mono text-xs">{e.action ?? "—"}</span>
                    </TableCell>
                    <TableCell className="text-sm">
                      {e.resourceType ?? "—"}
                      {e.resourceDisplayName || e.resourceId ? (
                        <span className="ml-2 font-mono text-xs text-muted-foreground">
                          {e.resourceDisplayName ?? e.resourceId}
                        </span>
                      ) : null}
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={e.outcome} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
          <div className="mt-4 flex flex-col items-center gap-2">
            {hasNextPage ? (
              <Button variant="outline" onClick={() => void fetchNextPage()} disabled={isFetchingNextPage}>
                {isFetchingNextPage ? "Loading…" : "Load more"}
              </Button>
            ) : null}
            <p className="font-mono text-xs text-muted-foreground">
              {events.length} event{events.length === 1 ? "" : "s"} loaded{hasNextPage ? "" : " · end of log"}
            </p>
          </div>
        </>
      )}
    </>
  )
}
