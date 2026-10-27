import { NavLink, Outlet, useLocation, useNavigate, useParams } from "react-router-dom"
import {
  Boxes, BarChart3, BookOpen, Camera, Check, ChevronsUpDown, CreditCard, Database, DatabaseZap, FileClock, FolderKanban, HardDrive,
  Image, KeyRound, LayoutDashboard, LogOut, Monitor, Moon, Network, Receipt,
  ExternalLink, Gift, PiggyBank, Search as SearchIcon, Server, Settings, Shield, Sun, UserCircle, Users, Wallet,
} from "lucide-react"
import { useEffect, useState } from "react"
import { useAuth } from "@/lib/auth"
import { useFeatures, useProjects, useProjectInit, useUIMenu } from "@/lib/hooks"
import { useTheme, type ThemePref } from "@/lib/theme"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel,
  DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Kbd } from "@/components/ui/kbd"
import {
  Sidebar, SidebarContent, SidebarGroup, SidebarGroupContent, SidebarGroupLabel,
  SidebarHeader, SidebarInset, SidebarMenu, SidebarMenuButton, SidebarMenuItem,
  SidebarProvider, SidebarTrigger,
} from "@/components/ui/sidebar"
import { cn } from "@/lib/utils"
import SearchModal from "@/components/search-modal"

type Item = {
  to: string
  label: string
  icon: React.ComponentType<{ className?: string }>
  // gating: hidden unless the /init menu carries the service enabled, and/or
  // the /features flag is present. No gate = always visible.
  service?: string
  feature?: string
}
type Group = { label: string; items: Item[] }

function navGroups(pid: string): Group[] {
  const p = `/p/${pid}`
  return [
    {
      label: "Overview",
      items: [{ to: `${p}/dashboard`, label: "Dashboard", icon: LayoutDashboard }],
    },
    {
      label: "Compute",
      items: [
        { to: `${p}/servers`, label: "Servers", icon: Server, service: "compute" },
        { to: `${p}/keypairs`, label: "Key pairs", icon: KeyRound, service: "compute" },
        { to: `${p}/images`, label: "Images", icon: Image, service: "image" },
      ],
    },
    {
      label: "Storage",
      items: [
        { to: `${p}/volumes`, label: "Volumes", icon: HardDrive, service: "volumev3" },
        { to: `${p}/snapshots`, label: "Snapshots", icon: Camera, service: "volumev3" },
        { to: `${p}/object-storage`, label: "Object storage", icon: Database, service: "object-store" },
        { to: `${p}/s3-keys`, label: "S3 access keys", icon: KeyRound, service: "object-store" },
      ],
    },
    {
      label: "Network",
      items: [
        { to: `${p}/networks`, label: "Networks", icon: Network, service: "network" },
        { to: `${p}/security-groups`, label: "Security groups", icon: Shield, service: "network" },
      ],
    },
    {
      label: "Platform",
      items: [
        { to: `${p}/kubernetes`, label: "Kubernetes", icon: Boxes, service: "kubernetes" },
        // DatabaseZap: plain Database is taken by object storage above.
        { to: `${p}/databases`, label: "Databases", icon: DatabaseZap, service: "database" },
      ],
    },
    {
      label: "Billing",
      items: [
        { to: `${p}/billing/funds`, label: "Funds", icon: Wallet, feature: "billing" },
        { to: `${p}/billing/cards`, label: "Cards", icon: CreditCard, feature: "billing" },
        { to: `${p}/billing/history`, label: "Billing history", icon: Receipt, feature: "billing" },
        { to: `${p}/billing/savings`, label: "Savings plans", icon: PiggyBank, feature: "billing" },
        { to: `${p}/billing/credits`, label: "Promotional credits", icon: Gift, feature: "billing" },
      ],
    },
    {
      label: "Organization",
      items: [
        { to: `${p}/org/billing`, label: "Billing", icon: BarChart3 },
        { to: `${p}/org/members`, label: "Members", icon: Users },
        { to: `${p}/org/projects`, label: "Projects", icon: Boxes },
        { to: `${p}/org/audit`, label: "Audit log", icon: FileClock },
        { to: `${p}/org/roles`, label: "Roles", icon: Shield },
        { to: `${p}/org/settings`, label: "Settings", icon: Settings },
      ],
    },
  ]
}

// Applies the admin-driven gates: /init menu.items (service keys, admin's cloud
// provider Services toggles) + /features flags. While either query is loading,
// nothing is filtered out (avoids a nav flash); once loaded, a service absent
// or disabled hides its items, an absent feature hides feature-gated items.
function useGatedNavGroups(pid: string): Group[] {
  const { data: init } = useUIMenu(pid)
  const { data: features } = useFeatures()
  const items = init?.menu?.items
  const featureSet = features ? new Set(features) : undefined
  // Admin Custom Menu items (newMenuItem:true) render as a trailing "More" group.
  const more: Group[] = []
  if (items) {
    const customs = Object.entries(items)
      .filter(([, v]) => (v as Record<string, unknown>)?.newMenuItem === true && (v as Record<string, unknown>)?.enabled === true)
      .sort((a, b) => Number((a[1] as Record<string, unknown>).order ?? 0) - Number((b[1] as Record<string, unknown>).order ?? 0))
    if (customs.length) {
      more.push({
        label: "More",
        items: customs.map(([slug, v]) => ({
          to: `/p/${pid}/more/${slug}`,
          label: ((v as Record<string, unknown>).displayName as string) ?? slug,
          icon: ExternalLink,
        })),
      })
    }
  }
  return navGroups(pid)
    .map((g) => ({
      ...g,
      items: g.items.filter((it) => {
        if (it.service && items && !(items[it.service]?.enabled === true)) return false
        if (it.feature && featureSet && !featureSet.has(it.feature)) return false
        return true
      }),
    }))
    .filter((g) => g.items.length > 0)
    .concat(more)
}

const THEME_OPTIONS: Array<{ value: ThemePref; label: string; icon: React.ComponentType<{ className?: string }> }> = [
  { value: "light", label: "Light", icon: Sun },
  { value: "dark", label: "Dark", icon: Moon },
  { value: "system", label: "System", icon: Monitor },
]

function ThemeMenu() {
  const { pref, dark, setPref } = useTheme()
  const Icon = dark ? Moon : Sun
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon" aria-label="Theme">
          <Icon className="size-4" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {THEME_OPTIONS.map((opt) => (
          <DropdownMenuItem
            key={opt.value}
            onClick={(e) => setPref(opt.value, { x: e.clientX, y: e.clientY })}
          >
            <opt.icon className="mr-2 size-4" /> {opt.label}
            {pref === opt.value && <Check className="ml-auto size-4" />}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

export function AppShell() {
  const { pid = "" } = useParams()
  const auth = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const { data: projects } = useProjects()
  const current = projects?.find((p) => p.id === pid)
  const gatedGroups = useGatedNavGroups(pid)
  const [searchOpen, setSearchOpen] = useState(false)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault()
        setSearchOpen((o) => !o)
      }
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [])
  useProjectInit(pid)

  const email = auth.user?.profile.email
  const initial = (email ?? "?").slice(0, 1).toUpperCase()

  return (
    <SidebarProvider style={{ "--sidebar-width": "16rem" } as React.CSSProperties}>
      <Sidebar collapsible="icon">
        <SidebarHeader>
          {/* Wordmark + console chip; collapses to the dot mark on the icon rail. */}
          <div className="flex h-9 items-center gap-2 px-2 group-data-[collapsible=icon]:justify-center group-data-[collapsible=icon]:px-0">
            <NavLink to={pid ? `/p/${pid}/dashboard` : "/"} className="flex items-center gap-2">
              <span className="font-display text-lg font-semibold tracking-tight text-sidebar-foreground group-data-[collapsible=icon]:hidden">
                ProxCloud<span className="text-primary">.</span>
              </span>
              <span className="hidden size-5 items-center justify-center rounded-sm bg-primary font-display text-sm font-bold text-primary-foreground group-data-[collapsible=icon]:flex">
                S
              </span>
            </NavLink>
            <span className="text-eyebrow rounded border border-sidebar-border px-1.5 py-0.5 group-data-[collapsible=icon]:hidden">
              console
            </span>
          </div>

          {/* Project switcher */}
          <SidebarMenu>
            <SidebarMenuItem>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <SidebarMenuButton tooltip={current?.name ?? "Select project"} className="border border-sidebar-border bg-sidebar-accent/60">
                    <Boxes className="size-4" />
                    <span className="truncate">{current?.name ?? "Select project"}</span>
                    <ChevronsUpDown className="ml-auto size-4 opacity-60" />
                  </SidebarMenuButton>
                </DropdownMenuTrigger>
                <DropdownMenuContent className="w-56" align="start">
                  <DropdownMenuLabel className="text-eyebrow">Projects</DropdownMenuLabel>
                  {(projects ?? []).map((p) => (
                    <DropdownMenuItem key={p.id} onClick={() => navigate(`/p/${p.id}/dashboard`)}>
                      <span className="truncate">{p.name}</span>
                      {p.id === pid && <Check className="ml-auto size-4" />}
                    </DropdownMenuItem>
                  ))}
                  <DropdownMenuSeparator />
                  <DropdownMenuItem onClick={() => navigate(`/p/${pid}/org/projects`)}>
                    <FolderKanban className="mr-2 size-4" /> All projects
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarHeader>

        <SidebarContent>
          {gatedGroups.map((g) => (
            <SidebarGroup key={g.label}>
              <SidebarGroupLabel className="text-eyebrow">{g.label}</SidebarGroupLabel>
              <SidebarGroupContent>
                <SidebarMenu>
                  {g.items.map((it) => (
                    <SidebarMenuItem key={it.to}>
                      <SidebarMenuButton
                        asChild
                        tooltip={it.label}
                        isActive={location.pathname.startsWith(it.to)}
                      >
                        <NavLink to={it.to}>
                          <it.icon className="size-4" />
                          <span>{it.label}</span>
                        </NavLink>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                  ))}
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          ))}
        </SidebarContent>
      </Sidebar>

      <SidebarInset>
        <header className="sticky top-0 z-20 flex h-[var(--navbar-height)] items-center gap-2 border-b bg-background/80 px-4 backdrop-blur md:px-6">
          <SidebarTrigger />

          {/* Search: full trigger ≥md, icon-only below. */}
          <Button
            variant="outline"
            size="sm"
            className="hidden w-56 justify-between text-muted-foreground md:flex"
            onClick={() => setSearchOpen(true)}
          >
            <span className="inline-flex items-center gap-2"><SearchIcon className="size-4" /> Search…</span>
            <Kbd>⌘K</Kbd>
          </Button>
          <Button
            variant="ghost"
            size="icon"
            className="md:hidden"
            aria-label="Search"
            onClick={() => setSearchOpen(true)}
          >
            <SearchIcon className="size-4" />
          </Button>

          <div className="ml-auto flex items-center gap-1.5">
            <Button variant="ghost" size="sm" asChild>
              <NavLink to="/docs">
                <BookOpen className="size-4" />
                <span className="hidden md:inline">Docs</span>
              </NavLink>
            </Button>
            <ThemeMenu />
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline" size="sm" className="gap-2">
                  <span className={cn("flex size-5 items-center justify-center rounded-full bg-secondary text-[11px] font-semibold")}>
                    {initial}
                  </span>
                  <span className="hidden max-w-44 truncate lg:inline">{email ?? "Account"}</span>
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-60">
                {/* Identity first (the email people recognize); the opaque OIDC
                    subject stays visible but demoted to fine print. */}
                <DropdownMenuLabel className="flex flex-col gap-0.5">
                  <span className="truncate font-medium">{email ?? "Account"}</span>
                  <span className="truncate font-mono text-[11px] font-normal text-muted-foreground">
                    {auth.user?.profile.sub}
                  </span>
                </DropdownMenuLabel>
                <DropdownMenuSeparator />
                <DropdownMenuItem onClick={() => navigate(`/p/${pid}/account`)}>
                  <UserCircle className="mr-2 size-4" /> Account settings
                </DropdownMenuItem>
                <DropdownMenuItem onClick={() => void auth.signoutRedirect()}>
                  <LogOut className="mr-2 size-4" /> Sign out
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </header>
        <SearchModal pid={pid} open={searchOpen} onOpenChange={setSearchOpen} />
        <main className="flex-1 px-4 py-6 md:px-6">
          <div className="mx-auto w-full max-w-6xl">
            <Outlet />
          </div>
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}
