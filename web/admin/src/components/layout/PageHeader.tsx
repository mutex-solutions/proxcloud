import type { ReactNode } from "react"

// Every page opens the same way: optional eyebrow (mono small-caps context
// label), optional breadcrumb slot for detail pages, the title in the display
// face, and the signature horizon line burning underneath.
export function PageHeader({
  title,
  description,
  eyebrow,
  breadcrumb,
  actions,
}: {
  title: string
  description?: string
  /** Small-caps mono context label above the title (e.g. "Compute"). */
  eyebrow?: string
  /** Breadcrumb node rendered above the title on detail pages. */
  breadcrumb?: ReactNode
  actions?: ReactNode
}) {
  return (
    <div className="mb-6">
      {breadcrumb ? <div className="mb-2">{breadcrumb}</div> : null}
      <div className="flex items-end justify-between gap-4 max-sm:flex-wrap">
        <div>
          {eyebrow ? <div className="text-eyebrow mb-1">{eyebrow}</div> : null}
          <h1 className="font-display text-2xl font-semibold tracking-tight">{title}</h1>
          {description ? <p className="mt-1 text-sm text-muted-foreground">{description}</p> : null}
        </div>
        {actions ? (
          <div className="flex shrink-0 items-center gap-2 max-sm:shrink max-sm:flex-wrap">
            {actions}
          </div>
        ) : null}
      </div>
      <div className="horizon mt-3 w-full max-w-[520px]" />
    </div>
  )
}
