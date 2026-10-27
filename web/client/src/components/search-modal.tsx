import { useEffect, useMemo, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { useNavigate } from "react-router-dom"
import { Archive, Box, Camera, FolderKanban, HardDrive, Key, Network, Receipt, Server, Shield, type LucideIcon } from "lucide-react"
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command"
import { Kbd, KbdGroup } from "@/components/ui/kbd"
import { apiFetch } from "@/lib/api"

// GET /search/{projectId} → {data:[{type, data:{name,…,id,region}}]} (Go clientcloud.go search).
// The set is prefilled — the FE filters client-side; there is no query param.
type SearchItem = {
  type: string
  data?: Record<string, any>
}

const TYPE_ICONS: Record<string, LucideIcon> = {
  SERVER: Server,
  NETWORK: Network,
  SUBNET: Network,
  SECURITY_GROUP: Shield,
  IMAGE: Camera,
  VOLUME: HardDrive,
  VOLUME_SNAPSHOT: Camera,
  KEYPAIR: Key,
  BUCKET: Archive,
  PROJECT: FolderKanban,
  BILL: Receipt,
}

// Everything without a dedicated detail route goes to its list page.
const LIST_PAGES: Record<string, string> = {
  VOLUME: "volumes",
  VOLUME_SNAPSHOT: "snapshots",
  BUCKET: "object-storage",
  SUBNET: "networks",
  KEYPAIR: "keypairs",
  IMAGE: "images",
}

// Group heading: "SECURITY_GROUP" → "security groups" (the eyebrow class
// handles the small-caps treatment).
function typeLabel(t: string): string {
  const label = t.replaceAll("_", " ").toLowerCase()
  return label.endsWith("s") ? label : `${label}s`
}

// Raw API enums ("SHUTOFF", "SCHEDULED_FOR_DELETION") read as debug output —
// sentence-case them for the row subtitle.
function humanStatus(v: unknown): string | undefined {
  if (typeof v !== "string" || !v) return undefined
  const s = v.replaceAll("_", " ").toLowerCase()
  return s.charAt(0).toUpperCase() + s.slice(1)
}

export default function SearchModal({
  pid, open, onOpenChange,
}: {
  pid: string
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const nav = useNavigate()
  const [raw, setRaw] = useState("")
  const [query, setQuery] = useState("")

  // Fetch once per open (staleTime keeps re-opens cheap); filtering is client-side.
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["search", pid],
    queryFn: () => apiFetch<SearchItem[]>(`/search/${pid}`),
    enabled: open && !!pid,
    staleTime: 30_000,
  })

  // Debounce the input 200ms before filtering.
  useEffect(() => {
    const t = setTimeout(() => setQuery(raw), 200)
    return () => clearTimeout(t)
  }, [raw])

  // Reset on every open.
  useEffect(() => {
    if (open) {
      setRaw("")
      setQuery("")
    }
  }, [open])

  const filtered = useMemo(() => {
    const list = data ?? []
    const needle = query.trim().toLowerCase()
    if (!needle) return list
    const tokens = needle.split(/\s+/)
    return list.filter((it) => {
      const hay = [it.type, ...Object.values(it.data ?? {})]
        .filter((v) => typeof v === "string" || typeof v === "number")
        .join(" ")
        .toLowerCase()
      return tokens.every((t) => hay.includes(t))
    })
  }, [data, query])

  const groups = useMemo(() => {
    const m = new Map<string, SearchItem[]>()
    for (const it of filtered) {
      const k = it.type || "OTHER"
      const arr = m.get(k)
      if (arr) arr.push(it)
      else m.set(k, [it])
    }
    return [...m.entries()]
  }, [filtered])

  const go = (item: SearchItem) => {
    const id = (item.data?.id as string) ?? ""
    switch (item.type) {
      case "SERVER":
        nav(`/p/${pid}/servers/${id}`)
        break
      case "NETWORK":
        nav(`/p/${pid}/networks/${id}`)
        break
      case "SECURITY_GROUP":
        nav(`/p/${pid}/security-groups/${id}`)
        break
      case "BILL":
        nav(`/p/${pid}/billing/history/bills/${id}`)
        break
      case "PROJECT":
        nav(`/p/${id}/dashboard`)
        break
      default:
        nav(`/p/${pid}/${LIST_PAGES[item.type] ?? "dashboard"}`)
    }
    onOpenChange(false)
  }

  return (
    <CommandDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Search this project"
      description="Search resources in this project"
      showCloseButton={false}
      className="glass-surface top-[20%] translate-y-0 sm:max-w-xl"
      // Matching is done here (debounced, token-AND over all data values), not by cmdk.
      commandProps={{ shouldFilter: false, className: "bg-transparent" }}
    >
      <CommandInput
        value={raw}
        onValueChange={setRaw}
        placeholder="Search servers, networks, volumes, bills…"
        aria-label="Search this project"
      />
      <CommandList className="max-h-[60vh]">
        {isLoading ? (
          <p className="py-8 text-center text-sm text-muted-foreground">Loading…</p>
        ) : isError ? (
          <p className="py-8 text-center text-sm text-muted-foreground">{(error as Error).message}</p>
        ) : (
          <>
            <CommandEmpty className="py-8 text-center text-sm text-muted-foreground">
              {query.trim() ? "No matches." : "Nothing to search yet."}
            </CommandEmpty>
            {groups.map(([type, items]) => {
              const Icon = TYPE_ICONS[type] ?? Box
              return (
                <CommandGroup key={type}>
                  <p className="text-eyebrow px-2 py-1.5" aria-hidden>
                    {typeLabel(type)}
                  </p>
                  {items.map((item, i) => {
                    const id = (item.data?.id as string) ?? ""
                    const name = (item.data?.name as string) || id || "—"
                    const sub = [humanStatus(item.data?.status), item.data?.ipv4, item.data?.flavor, item.data?.region]
                      .filter((v) => typeof v === "string" && v)
                      .join(" · ")
                    return (
                      <CommandItem
                        key={`${type}-${id || i}`}
                        value={`${type}-${id || `${name}-${i}`}`}
                        onSelect={() => go(item)}
                        className="gap-3 py-2"
                      >
                        <Icon className="size-4 shrink-0 text-muted-foreground" />
                        <span className="truncate font-medium">{name}</span>
                        {sub && (
                          <span className="ml-auto truncate text-xs text-muted-foreground">{sub}</span>
                        )}
                      </CommandItem>
                    )
                  })}
                </CommandGroup>
              )
            })}
          </>
        )}
      </CommandList>
      <div className="flex items-center gap-4 border-t px-3 py-2 text-xs text-muted-foreground">
        <KbdGroup>
          <Kbd>↑</Kbd>
          <Kbd>↓</Kbd>
          navigate
        </KbdGroup>
        <KbdGroup>
          <Kbd>↵</Kbd>
          open
        </KbdGroup>
        <KbdGroup className="ml-auto">
          <Kbd>esc</Kbd>
          close
        </KbdGroup>
      </div>
    </CommandDialog>
  )
}
