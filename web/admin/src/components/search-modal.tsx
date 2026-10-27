import { useNavigate } from "react-router-dom"
import type { LucideIcon } from "lucide-react"
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command"
import { Kbd, KbdGroup } from "@/components/ui/kbd"

// Admin has no backend search endpoint (the console's GET /search/{pid} is a
// customer-cloud feature) — the ⌘K palette is a page jumper over the same
// permission-gated nav groups the sidebar renders, so it never shows a page
// the operator can't open. Matching is cmdk's own fuzzy filter.
export type PaletteGroup = {
  label: string
  items: Array<{ to: string; label: string; icon: LucideIcon | React.ComponentType<{ className?: string }> }>
}

export default function SearchModal({
  groups, open, onOpenChange,
}: {
  groups: PaletteGroup[]
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const nav = useNavigate()

  const go = (to: string) => {
    nav(to)
    onOpenChange(false)
  }

  return (
    <CommandDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Go to page"
      description="Jump to an admin page"
      showCloseButton={false}
      className="glass-surface top-[20%] translate-y-0 sm:max-w-xl"
      commandProps={{ className: "bg-transparent" }}
    >
      <CommandInput placeholder="Go to page…" aria-label="Go to page" />
      <CommandList className="max-h-[60vh]">
        <CommandEmpty className="py-8 text-center text-sm text-muted-foreground">
          No matches.
        </CommandEmpty>
        {groups.map((g) => (
          <CommandGroup key={g.label}>
            <p className="text-eyebrow px-2 py-1.5" aria-hidden>
              {g.label}
            </p>
            {g.items.map((it) => (
              <CommandItem
                key={it.to}
                value={`${g.label} ${it.label}`}
                onSelect={() => go(it.to)}
                className="gap-3 py-2"
              >
                <it.icon className="size-4 shrink-0 text-muted-foreground" />
                <span className="truncate font-medium">{it.label}</span>
              </CommandItem>
            ))}
          </CommandGroup>
        ))}
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
