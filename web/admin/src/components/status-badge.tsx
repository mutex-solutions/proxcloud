import { statusKind } from "@/lib/format"
import { cn } from "@/lib/utils"

// Literal class map: Tailwind v4 only emits utilities it can see literally,
// so a template like `status-dot-${kind}` silently drops every variant that
// never appears verbatim in source.
const DOT_CLASS: Record<string, string> = {
  ok: "status-dot-ok",
  warn: "status-dot-warn",
  error: "status-dot-error",
  muted: "status-dot-muted",
}

export function StatusBadge({ status, className }: { status?: string; className?: string }) {
  const kind = statusKind(status)
  const label = (status ?? "unknown").toLowerCase().replace(/_/g, " ")
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-sm", className)}>
      <span className={cn("status-dot", DOT_CLASS[kind] ?? DOT_CLASS.muted)} />
      {/* Sentence case (design-system convention) — neutral label carries the state;
          the dot is the redundant color signal. */}
      <span className="first-letter:uppercase">{label}</span>
    </span>
  )
}
