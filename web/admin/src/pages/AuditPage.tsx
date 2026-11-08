// Design-system escape hatch — DOCUMENTED EXCEPTION to the DataTable rule: the audit
// trail is cursor-paged by the server (paging.nextMarker), so rows arrive in
// server order and client sorting/filtering would lie about the dataset. It
// stays a bare styled <Table> on a card surface with an explicit "Load more"
// cursor walk; only the row presentation is design-system (mono timestamps, actor
// chips, StatusBadge outcomes, mono action verbs).
import { useState } from "react"
import { useInfiniteQuery, useQuery } from "@tanstack/react-query"
import { ScrollText } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { EmptyState } from "@/components/empty-state"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetchEnvelope } from "@/lib/api"
import { fmtDateTime } from "@/lib/format"
import { cn } from "@/lib/utils"

type PropertyChange = { field?: string; oldValue?: unknown; newValue?: unknown }

// GET /admin/audit rows are hydrated AuditEventDto { event, organization?, project?, user? }.
type AuditRow = {
  event: {
    id?: string
    timestamp?: string
    requestInterface?: string
    eventContext?: string
    action?: string
    resourceType?: string
    resourceId?: string
    resourceDisplayName?: string
    resourceMetadata?: Record<string, unknown>
    changes?: PropertyChange[]
    actor?: { type?: string; id?: string; displayName?: string }
    outcome?: string
  }
  organization?: { id?: string; name?: string }
  project?: { id?: string; name?: string }
  user?: { id?: string; sub?: string; email?: string; firstName?: string; lastName?: string }
}

type CursorPaging = { limit?: number; nextMarker?: string; prevMarker?: string }

const PAGE_SIZE = 50

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
function identityChip(seed: string) {
  let h = 0
  for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) | 0
  return IDENTITY_CHIP[Math.abs(h) % IDENTITY_CHIP.length]
}

/** Initial-avatar chip + display name; the initial's hue is decorative
 * (per-actor identity color), the adjacent name carries the information. */
function ActorChip({ id, name }: { id?: string; name?: string }) {
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

function fmtValue(v: unknown): string {
  if (v === undefined || v === null || v === "") return "—"
  return typeof v === "string" ? v : JSON.stringify(v)
}

export default function AuditPage() {
  const [abuseOnly, setAbuseOnly] = useState(false)
  const [projectId, setProjectId] = useState("")
  const counts = useQuery({ queryKey: ["sandbox-abuse-count", projectId], enabled: abuseOnly && !!projectId,
    queryFn: () => apiFetchEnvelope<{ projectCount: number }>(`/admin/sandbox/abuse?projectId=${encodeURIComponent(projectId)}`) })
  const { data, isLoading, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: ["admin-audit", abuseOnly, projectId],
    initialPageParam: "",
    queryFn: ({ pageParam }) =>
      apiFetchEnvelope<AuditRow[]>(
        `/admin/audit?limit=${PAGE_SIZE}${abuseOnly ? "&resourceType=SANDBOX_ABUSE" : ""}${projectId ? `&projectId=${encodeURIComponent(projectId)}` : ""}${pageParam ? `&after=${encodeURIComponent(pageParam)}` : ""}`,
      ),
    getNextPageParam: (last) => (last.paging as CursorPaging | undefined)?.nextMarker ?? undefined,
  })

  const rows = data?.pages.flatMap((p) => p.data ?? []) ?? []
  const [detail, setDetail] = useState<AuditRow | null>(null)

  const actorOf = (r: AuditRow) =>
    r.event.actor?.displayName ?? r.user?.email ?? r.event.actor?.id ?? undefined

  return (
    <>
      <PageHeader
        title="Audit log"
        eyebrow="System"
        description="Every admin, client and system mutation, newest first."
      />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <Button variant={abuseOnly ? "default" : "outline"} onClick={() => setAbuseOnly(!abuseOnly)}>Sandbox abuse events</Button>
        <label className="text-sm">Project ID <input className="ml-2 rounded border p-2" value={projectId} onChange={(e) => setProjectId(e.target.value)} /></label>
        {abuseOnly && projectId && <span className="text-sm">Detections: {counts.data?.data?.projectCount ?? "…"}</span>}
      </div>
      {isLoading ? (
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
      ) : rows.length === 0 ? (
        <EmptyState icon={ScrollText} title="No audit events" hint="Mutations will show up here as they happen." />
      ) : (
        <>
          <Card className="overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Time</TableHead>
                  <TableHead>Actor</TableHead>
                  <TableHead>Area</TableHead>
                  <TableHead>Action</TableHead>
                  <TableHead>Resource</TableHead>
                  <TableHead>Outcome</TableHead>
                  <TableHead>Changes</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((r, i) => {
                  const e = r.event
                  const nChanges = e.changes?.length ?? 0
                  return (
                    <TableRow key={e.id ?? i}>
                      <TableCell className="whitespace-nowrap font-mono text-xs tabular-nums text-muted-foreground">
                        {fmtDateTime(e.timestamp)}
                      </TableCell>
                      <TableCell>
                        <ActorChip id={e.actor?.id} name={actorOf(r)} />
                      </TableCell>
                      <TableCell>
                        <Badge variant="secondary">{e.requestInterface ?? "—"}</Badge>
                      </TableCell>
                      <TableCell>
                        <span className="font-mono text-xs">{e.action ?? "—"}</span>
                      </TableCell>
                      <TableCell className="text-sm">
                        <span className="flex items-baseline gap-2">
                          <span className="whitespace-nowrap">{e.resourceType ?? "—"}</span>
                          {e.resourceDisplayName || e.resourceId ? (
                            <span
                              className="max-w-40 truncate font-mono text-xs text-muted-foreground 2xl:max-w-md"
                              title={e.resourceDisplayName ?? e.resourceId}
                            >
                              {e.resourceDisplayName ?? e.resourceId}
                            </span>
                          ) : null}
                        </span>
                      </TableCell>
                      <TableCell>
                        <StatusBadge status={e.outcome} />
                        {e.resourceType === "SANDBOX_ABUSE" && <p className="text-xs text-muted-foreground">{String(e.resourceMetadata?.status ?? e.resourceMetadata?.action ?? "alert")}</p>}
                      </TableCell>
                      <TableCell>
                        {nChanges > 0 ? (
                          <Button variant="ghost" size="sm" onClick={() => setDetail(r)}>
                            {nChanges} {nChanges === 1 ? "change" : "changes"}
                          </Button>
                        ) : (
                          <span className="text-muted-foreground">—</span>
                        )}
                      </TableCell>
                    </TableRow>
                  )
                })}
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
              {rows.length} event{rows.length === 1 ? "" : "s"} loaded{hasNextPage ? "" : " · end of log"}
            </p>
          </div>
        </>
      )}

      <Dialog open={!!detail} onOpenChange={(o) => !o && setDetail(null)}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle className="font-display">
              {detail?.event.action} — {detail?.event.resourceType}
            </DialogTitle>
            <DialogDescription>
              {fmtDateTime(detail?.event.timestamp)} by {detail ? (actorOf(detail) ?? "—") : ""}
              {detail?.organization?.name ? ` · ${detail.organization.name}` : ""}
            </DialogDescription>
          </DialogHeader>
          <div className="max-h-[60vh] overflow-y-auto rounded-md border">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Field</TableHead>
                  <TableHead>Old value</TableHead>
                  <TableHead>New value</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(detail?.event.changes ?? []).map((c, i) => (
                  <TableRow key={i}>
                    <TableCell className="font-mono text-xs">{c.field ?? "—"}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">{fmtValue(c.oldValue)}</TableCell>
                    <TableCell className="font-mono text-xs">{fmtValue(c.newValue)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}
