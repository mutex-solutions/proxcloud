import { Button } from "@/components/ui/button"

// "Load more" footer for keyset-paged (cursor) lists — walks the server's
// paging.nextMarker (see useCursorList). Pair with a DataTable rendered with
// pagination={false}, or a bare Table. Hidden until there is at least one row.
export function LoadMore({
  hasNextPage,
  isFetching,
  onClick,
  count,
  noun,
}: {
  hasNextPage?: boolean
  isFetching: boolean
  onClick: () => void
  count: number
  noun: string
}) {
  if (count === 0) return null
  return (
    <div className="mt-4 flex flex-col items-center gap-2">
      {hasNextPage ? (
        <Button variant="outline" onClick={onClick} disabled={isFetching}>
          {isFetching ? "Loading…" : "Load more"}
        </Button>
      ) : null}
      <p className="font-mono text-xs text-muted-foreground">
        {count} {noun}
        {count === 1 ? "" : "s"} loaded{hasNextPage ? "" : " · end"}
      </p>
    </div>
  )
}
