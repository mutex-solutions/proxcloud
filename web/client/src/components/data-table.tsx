// The one table wrapper for list pages. Headless TanStack Table rendered
// through the design-system Table primitives: sorting, optional global
// search, pagination, responsive cards, skeleton loading under a real header,
// empty/error states, and keyboard-accessible row navigation. Detail-page
// mini-tables (<~15 static rows) should keep using the bare <Table> primitives.
//
// Contract for callers: `columns` MUST be referentially stable (module scope
// or useMemo) and accessors for nested cloud data use accessorFn, not
// accessorKey. All useReactTable usage stays inside this leaf component.
import { useEffect, useLayoutEffect, useMemo, useState } from "react"
import {
  flexRender,
  getCoreRowModel,
  getFilteredRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
  type Column,
  type ColumnDef,
  type SortingState,
  type Table as TanStackTable,
} from "@tanstack/react-table"
import {
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  ChevronLeft,
  ChevronRight,
  ChevronsLeft,
  ChevronsRight,
  Search,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { fmtMoney } from "@/lib/format"
import { cn } from "@/lib/utils"

const EMPTY: never[] = []
const DEFAULT_PAGE_SIZE = 10
const DEFAULT_SORT_VALUE = "__default__"
const DESKTOP_TABLE_QUERY = "(min-width: 1280px)"

type LabeledHeader = ((...args: any[]) => React.ReactNode) & { displayName?: string }

function useDesktopTableLayout(): boolean {
  const getMatch = () =>
    typeof window === "undefined" || typeof window.matchMedia !== "function"
      ? true
      : window.matchMedia(DESKTOP_TABLE_QUERY).matches
  const [matches, setMatches] = useState(getMatch)

  useEffect(() => {
    if (typeof window.matchMedia !== "function") return
    const query = window.matchMedia(DESKTOP_TABLE_QUERY)
    const onChange = () => setMatches(query.matches)
    query.addEventListener("change", onChange)
    onChange()
    return () => query.removeEventListener("change", onChange)
  }, [])

  return matches
}

function humanizeColumnId(id: string): string {
  const text = id
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/[_-]+/g, " ")
    .trim()
  return text ? text.charAt(0).toUpperCase() + text.slice(1) : "Field"
}

function columnLabel<TData>(column: Column<TData, unknown>): string {
  const header = column.columnDef.header
  if (typeof header === "string") return header
  if (typeof header === "function") {
    const label = (header as LabeledHeader).displayName
    if (label) return label
  }
  return humanizeColumnId(column.id)
}

function accessibleValue(value: unknown): string | undefined {
  if (typeof value !== "string" && typeof value !== "number") return undefined
  const label = String(value).trim().replace(/\s+/g, " ")
  if (!label) return undefined
  return label.length > 120 ? `${label.slice(0, 117)}...` : label
}

function accessibleRowName<TData>(
  row: TData,
  primaryValue: unknown,
  getRowLabel?: (row: TData) => string,
): string | undefined {
  const record = row as Record<string, unknown>
  const candidates = [
    getRowLabel?.(row),
    primaryValue,
    record.name,
    record.displayName,
    record.title,
    record.email,
    record.id,
    record.externalId,
  ]
  return candidates.map(accessibleValue).find(Boolean)
}

export interface DataTableProps<TData> {
  /** Must be referentially stable (module const or useMemo). */
  columns: ColumnDef<TData, any>[]
  /** Pass query.data directly; undefined is normalized to a stable []. */
  data: TData[] | undefined
  /** First-load only — background refetches keep rendered rows. */
  isLoading?: boolean
  error?: Error | null
  /** Shown (spanning all columns) when there are no rows. */
  empty?: React.ReactNode
  onRowClick?: (row: TData) => void
  /** Optional accessible name used by the row's explicit Open button. */
  getRowLabel?: (row: TData) => string
  getRowId?: (row: TData) => string
  /** Custom toolbar content; function form receives the table instance. */
  toolbar?: React.ReactNode | ((table: TanStackTable<TData>) => React.ReactNode)
  /** Renders a built-in search input bound to the global filter. */
  searchPlaceholder?: string
  initialSorting?: SortingState
  /** Client pagination is on by default; disable only for deliberately short lists. */
  pagination?: boolean
  pageSize?: number
  skeletonRows?: number
  className?: string
}

export function DataTable<TData>({
  columns,
  data,
  isLoading = false,
  error = null,
  empty,
  onRowClick,
  getRowLabel,
  getRowId,
  toolbar,
  searchPlaceholder,
  initialSorting = [],
  pagination = true,
  pageSize = DEFAULT_PAGE_SIZE,
  skeletonRows = 5,
  className,
}: DataTableProps<TData>) {
  const [sorting, setSorting] = useState<SortingState>(initialSorting)
  const [globalFilter, setGlobalFilter] = useState("")
  const isDesktopTable = useDesktopTableLayout()

  const rows = data ?? (EMPTY as TData[])
  const paginationEnabled = pagination && pageSize > 0
  const initialPageSize = pageSize > 0 ? pageSize : DEFAULT_PAGE_SIZE
  const table = useReactTable({
    data: rows,
    columns,
    state: { sorting, globalFilter },
    onSortingChange: setSorting,
    onGlobalFilterChange: setGlobalFilter,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    ...(paginationEnabled
      ? {
          getPaginationRowModel: getPaginationRowModel(),
          initialState: { pagination: { pageSize: initialPageSize } },
        }
      : {}),
    globalFilterFn: "includesString",
    // Stable row identity by default: never fall back to array indexes
    // (breaks React keys + row state on refetch/reorder).
    getRowId:
      getRowId ??
      ((row: TData, index: number) => {
        const r = row as { id?: unknown; externalId?: unknown }
        return String(r.id ?? r.externalId ?? index)
      }),
  })

  const visibleColumns = table.getVisibleLeafColumns().length || columns.length
  const renderedColumnCount = visibleColumns + (onRowClick ? 1 : 0)
  const modelRows = table.getRowModel().rows
  const sortableColumns = table.getAllLeafColumns().filter((column) => column.getCanSort())
  const hasToolbar =
    Boolean(toolbar) || Boolean(searchPlaceholder) || (!isDesktopTable && sortableColumns.length > 0)
  const currentSort = sorting[0]
  const filteredRowCount = table.getFilteredRowModel().rows.length
  const pageState = table.getState().pagination
  const pageCount = Math.max(table.getPageCount(), 1)
  const pageIndex = Math.min(pageState.pageIndex, pageCount - 1)
  const rangeStart = filteredRowCount === 0 ? 0 : pageIndex * pageState.pageSize + 1
  const rangeEnd = Math.min((pageIndex + 1) * pageState.pageSize, filteredRowCount)
  const showPagination =
    paginationEnabled &&
    !isLoading &&
    !error &&
    (pageCount > 1 || pageState.pageSize !== initialPageSize)
  const pageSizeOptions = useMemo(
    () => Array.from(new Set([10, 25, 50, initialPageSize])).sort((a, b) => a - b),
    [initialPageSize],
  )

  useLayoutEffect(() => {
    if (!paginationEnabled || pageState.pageIndex < pageCount) return
    table.setPageIndex(pageCount - 1)
  }, [paginationEnabled, pageCount, pageState.pageIndex, table])

  const ariaSort = (col: Column<TData, unknown>): React.AriaAttributes["aria-sort"] => {
    if (!col.getCanSort()) return undefined
    const dir = col.getIsSorted()
    return dir === "asc" ? "ascending" : dir === "desc" ? "descending" : "none"
  }

  const emptyMessage = globalFilter
    ? "No results match your search."
    : (empty ?? "Nothing here yet.")

  const renderMobileCards = () => {
    if (isLoading) {
      return Array.from({ length: skeletonRows }).map((_, i) => (
        <div key={`card-skeleton-${i}`} className="rounded-xl border bg-card p-4">
          <Skeleton className="h-5 w-2/5" />
          <div className="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        </div>
      ))
    }

    if (error) {
      return (
        <div role="alert" className="rounded-xl border border-destructive/30 bg-card px-4 py-10 text-center text-destructive">
          {error.message || "Something went wrong loading this list."}
        </div>
      )
    }

    if (modelRows.length === 0) {
      return (
        <div className="rounded-xl border bg-card px-4 py-10 text-center text-sm text-muted-foreground">
          {emptyMessage}
        </div>
      )
    }

    return modelRows.map((row) => {
      const cells = row.getVisibleCells()
      const actionCell = cells.find((cell) => cell.column.id === "actions")
      const contentCells = cells.filter((cell) => cell.column.id !== "actions")
      const primaryCell = contentCells[0]
      const detailCells = contentCells.slice(1)
      const openLabel = accessibleRowName(row.original, primaryCell?.getValue(), getRowLabel)

      return (
        <div
          key={row.id}
          data-slot="data-table-card"
          className={cn(
            "rounded-xl border bg-card p-4 shadow-xs transition-colors",
            onRowClick && "cursor-pointer hover:bg-muted/30",
          )}
          onClick={onRowClick ? () => onRowClick(row.original) : undefined}
        >
          <div className="flex min-w-0 items-start justify-between gap-3">
            <div className="min-w-0">
              <div className="text-eyebrow">
                {primaryCell ? columnLabel(primaryCell.column) : "Item"}
              </div>
              <div className="mt-1 min-w-0 break-words text-base [overflow-wrap:anywhere]">
                {primaryCell
                  ? flexRender(primaryCell.column.columnDef.cell, primaryCell.getContext())
                  : null}
              </div>
            </div>
            {actionCell || onRowClick ? (
              <div className="flex shrink-0 items-center gap-1" onClick={(event) => event.stopPropagation()}>
                {actionCell
                  ? flexRender(actionCell.column.columnDef.cell, actionCell.getContext())
                  : null}
                {onRowClick ? (
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon-sm"
                    aria-label={openLabel ? `Open ${openLabel}` : "Open row"}
                    title={openLabel ? `Open ${openLabel}` : "Open"}
                    onClick={() => onRowClick(row.original)}
                  >
                    <ChevronRight className="size-4" />
                  </Button>
                ) : null}
              </div>
            ) : null}
          </div>

          {detailCells.length > 0 ? (
            <dl className="mt-4 grid grid-cols-1 gap-x-4 gap-y-3 border-t pt-4 sm:grid-cols-2">
              {detailCells.map((cell) => (
                <div key={cell.id} className="min-w-0">
                  <dt className="text-eyebrow">{columnLabel(cell.column)}</dt>
                  <dd className="mt-1 min-w-0 break-words text-sm [overflow-wrap:anywhere] [&>*]:max-w-full">
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                  </dd>
                </div>
              ))}
            </dl>
          ) : null}
        </div>
      )
    })
  }

  return (
    <div data-slot="data-table" className={cn("min-w-0 flex flex-col gap-3", className)}>
      {hasToolbar && (
        <div className="flex flex-wrap items-center gap-2">
          {searchPlaceholder && (
            <div className="relative w-full max-w-xs">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={globalFilter}
                onChange={(e) => {
                  setGlobalFilter(e.target.value)
                  if (paginationEnabled) table.setPageIndex(0)
                }}
                placeholder={searchPlaceholder}
                className="pl-8"
                aria-label={searchPlaceholder}
              />
            </div>
          )}
          {typeof toolbar === "function" ? toolbar(table) : toolbar}

          {!isDesktopTable && sortableColumns.length > 0 ? (
            <div className="ml-auto flex items-center gap-2">
              <Select
                value={currentSort?.id ?? DEFAULT_SORT_VALUE}
                onValueChange={(value) =>
                  setSorting(value === DEFAULT_SORT_VALUE ? [] : [{ id: value, desc: false }])
                }
              >
                <SelectTrigger size="sm" className="max-w-44" aria-label="Sort rows by">
                  <SelectValue placeholder="Sort by" />
                </SelectTrigger>
                <SelectContent align="end">
                  <SelectItem value={DEFAULT_SORT_VALUE}>Default order</SelectItem>
                  {sortableColumns.map((column) => (
                    <SelectItem key={column.id} value={column.id}>
                      {columnLabel(column)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Button
                type="button"
                variant="outline"
                size="icon-sm"
                disabled={!currentSort}
                aria-label={currentSort?.desc ? "Sort ascending" : "Sort descending"}
                title={currentSort?.desc ? "Sort ascending" : "Sort descending"}
                onClick={() =>
                  currentSort && setSorting([{ id: currentSort.id, desc: !currentSort.desc }])
                }
              >
                {currentSort?.desc ? <ArrowDown className="size-4" /> : <ArrowUp className="size-4" />}
              </Button>
            </div>
          ) : null}
        </div>
      )}

      {/* Below xl the same sorted/filtered/paged rows become vertical cards, so
          every field remains readable without a horizontal-scroll interaction. */}
      {!isDesktopTable ? (
        <div className="grid gap-3 lg:grid-cols-2">{renderMobileCards()}</div>
      ) : null}

      {/* Desktop keeps native table semantics. Fixed layout + wrapping contains
          long IDs inside the available panel width; the primitive's scroll
          wrapper remains only as a last-resort safety net. */}
      {isDesktopTable ? (
        <div data-slot="data-table-desktop" className="overflow-hidden rounded-xl border bg-card">
        <Table className="table-fixed">
          <TableHeader>
            {table.getHeaderGroups().map((hg) => (
              <TableRow key={hg.id} className="hover:bg-transparent">
                {hg.headers.map((header) => (
                  <TableHead
                    key={header.id}
                    aria-sort={ariaSort(header.column)}
                    className={cn(
                      "px-3 text-xs whitespace-normal",
                      header.column.id === "actions" && "w-12 px-1",
                    )}
                  >
                    {header.isPlaceholder
                      ? null
                      : flexRender(header.column.columnDef.header, header.getContext())}
                  </TableHead>
                ))}
                {onRowClick ? (
                  <TableHead className="w-10 px-1">
                    <span className="sr-only">Open</span>
                  </TableHead>
                ) : null}
              </TableRow>
            ))}
          </TableHeader>
          <TableBody>
            {isLoading ? (
              Array.from({ length: skeletonRows }).map((_, i) => (
                <TableRow key={`skeleton-${i}`} className="hover:bg-transparent">
                  {Array.from({ length: renderedColumnCount }).map((_, j) => (
                    <TableCell key={j} className="px-3 whitespace-normal">
                      <Skeleton className="h-4 w-full max-w-32" />
                    </TableCell>
                  ))}
                </TableRow>
              ))
            ) : error ? (
              <TableRow className="hover:bg-transparent">
                <TableCell colSpan={renderedColumnCount} className="py-10 text-center whitespace-normal text-destructive">
                  <span role="alert">
                    {error.message || "Something went wrong loading this list."}
                  </span>
                </TableCell>
              </TableRow>
            ) : modelRows.length === 0 ? (
              <TableRow className="hover:bg-transparent">
                <TableCell colSpan={renderedColumnCount} className="py-10 text-center whitespace-normal text-muted-foreground">
                  {emptyMessage}
                </TableCell>
              </TableRow>
            ) : (
              // Row click is a pointer convenience only: making the <tr>
              // focusable/role=button nests interactive elements (axe serious).
              // The accessible path is an in-row link/menu or the Open button.
              modelRows.map((row) => (
                <TableRow
                  key={row.id}
                  data-state={row.getIsSelected() ? "selected" : undefined}
                  className={cn(onRowClick && "cursor-pointer")}
                  onClick={onRowClick ? () => onRowClick(row.original) : undefined}
                >
                  {row.getVisibleCells().map((cell) => (
                    <TableCell
                      key={cell.id}
                      className={cn(
                        "min-w-0 px-3 whitespace-normal break-words [overflow-wrap:anywhere]",
                        cell.column.id === "actions" && "w-12 px-1",
                      )}
                    >
                      <div className="min-w-0 max-w-full [overflow-wrap:anywhere] [&>*]:max-w-full">
                        {flexRender(cell.column.columnDef.cell, cell.getContext())}
                      </div>
                    </TableCell>
                  ))}
                  {onRowClick ? (
                    <TableCell className="w-10 px-1 text-right" onClick={(event) => event.stopPropagation()}>
                      <Button
                        type="button"
                        variant="ghost"
                        size="icon-sm"
                        aria-label={`Open ${
                          accessibleRowName(
                            row.original,
                            row.getVisibleCells().find((cell) => cell.column.id !== "actions")?.getValue(),
                            getRowLabel,
                          ) ?? "row"
                        }`}
                        title="Open"
                        onClick={() => onRowClick(row.original)}
                      >
                        <ChevronRight className="size-4" />
                      </Button>
                    </TableCell>
                  ) : null}
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
        </div>
      ) : null}

      {showPagination && (
        <div
          data-slot="data-table-pagination"
          className="flex flex-wrap items-center justify-between gap-3 rounded-lg border bg-card/60 px-3 py-2 text-sm text-muted-foreground"
        >
          <span className="tabular-nums" aria-live="polite">
            Showing <span className="font-medium text-foreground">{rangeStart}–{rangeEnd}</span> of{" "}
            <span className="font-medium text-foreground">{filteredRowCount}</span>
          </span>
          <div className="flex flex-wrap items-center gap-2">
            <span className="hidden sm:inline">Rows per page</span>
            <Select value={String(pageState.pageSize)} onValueChange={(value) => table.setPageSize(Number(value))}>
              <SelectTrigger size="sm" className="w-20" aria-label="Rows per page">
                <SelectValue />
              </SelectTrigger>
              <SelectContent align="end">
                {pageSizeOptions.map((size) => (
                  <SelectItem key={size} value={String(size)}>{size}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <span className="mx-1 whitespace-nowrap tabular-nums">
              Page {pageIndex + 1} of {pageCount}
            </span>
            <Button
              variant="outline"
              size="icon-sm"
              className="hidden sm:inline-flex"
              onClick={() => table.setPageIndex(0)}
              disabled={!table.getCanPreviousPage()}
              aria-label="First page"
              title="First page"
            >
              <ChevronsLeft className="size-4" />
            </Button>
            <Button
              variant="outline"
              size="icon-sm"
              onClick={() => table.previousPage()}
              disabled={!table.getCanPreviousPage()}
              aria-label="Previous page"
              title="Previous page"
            >
              <ChevronLeft className="size-4" />
            </Button>
            <Button
              variant="outline"
              size="icon-sm"
              onClick={() => table.nextPage()}
              disabled={!table.getCanNextPage()}
              aria-label="Next page"
              title="Next page"
            >
              <ChevronRight className="size-4" />
            </Button>
            <Button
              variant="outline"
              size="icon-sm"
              className="hidden sm:inline-flex"
              onClick={() => table.setPageIndex(Math.max(table.getPageCount() - 1, 0))}
              disabled={!table.getCanNextPage()}
              aria-label="Last page"
              title="Last page"
            >
              <ChevronsRight className="size-4" />
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}

/** Sortable column header: ghost button with a direction indicator. Use as
 * `header: sortableHeader("Name")` in a column def. */
export function sortableHeader<TData>(label: string) {
  function SortableHeader({ column }: { column: Column<TData, unknown> }) {
    const dir = column.getIsSorted()
    const Icon = dir === "asc" ? ArrowUp : dir === "desc" ? ArrowDown : ArrowUpDown
    return (
      <Button
        variant="ghost"
        size="sm"
        className="-ml-2 h-auto min-h-8 max-w-full gap-1.5 bg-transparent px-2 text-left whitespace-normal font-medium hover:bg-accent dark:bg-transparent data-[state=open]:bg-accent"
        onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}
      >
        {label}
        <Icon className={cn("size-3.5", dir ? "text-foreground" : "text-muted-foreground/70")} />
      </Button>
    )
  }
  SortableHeader.displayName = label
  return SortableHeader
}

/** Right-aligned sortable header — numeric/money columns. */
export function sortableRightHeader<TData>(label: string) {
  const Header = sortableHeader<TData>(label)
  function SortableRightHeader(ctx: { column: Column<TData, unknown> }) {
    return (
      <div className="flex justify-end">
        <Header {...ctx} />
      </div>
    )
  }
  SortableRightHeader.displayName = label
  return SortableRightHeader
}

/** Right-aligned mono money cell — the one way money renders in tables. */
export function MoneyCell({ value, currency }: { value: number | null | undefined; currency: string }) {
  return <div className="text-right font-mono tabular-nums">{fmtMoney(value ?? 0, currency)}</div>
}

/** Stable hook for column defs — thin alias so pages remember the rule. */
export function useColumns<TData>(factory: () => ColumnDef<TData, any>[], deps: React.DependencyList = []) {
  // eslint-disable-next-line react-hooks/exhaustive-deps
  return useMemo(factory, deps)
}
