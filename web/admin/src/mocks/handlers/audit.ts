// Audit log — cursor-paged (limit + after marker over the event id).
import type { Envelope, Paging } from "@/lib/api"
import { on } from "../router"
import { db } from "../db"

on("GET /admin/audit", ({ query }) => {
  const limit = Math.max(1, parseInt(query.get("limit") ?? "50", 10) || 50)
  const after = query.get("after")
  const rows = db.auditEvents
  let start = 0
  if (after) {
    const idx = rows.findIndex((r) => r.event.id === after)
    if (idx >= 0) start = idx + 1
  }
  const page = rows.slice(start, start + limit)
  const last = page[page.length - 1]
  const nextMarker = start + limit < rows.length && last ? (last.event.id as string) : undefined
  // Cursor paging rides in the envelope's paging slot (nextMarker/prevMarker).
  return { data: page, paging: { limit, nextMarker } as unknown as Paging } satisfies Envelope<unknown>
})
