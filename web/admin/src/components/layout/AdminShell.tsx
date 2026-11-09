import { NavLink, Outlet, useLocation } from "react-router-dom"
import {
  Banknote, Blocks, BookOpen, Building2, Check, CheckSquare, Cloud, CreditCard, FileClock, FileText,
  FolderKanban, Gauge, KeyRound, LayoutDashboard, LogOut, Mail, Menu as MenuIcon,
  Monitor, Moon, Percent, PiggyBank, Receipt, Search as SearchIcon, Server, Settings2, Shield, Sun, Tag, Users, Wallet,
} from "lucide-react"
import { useEffect, useState } from "react"
import { useAdminMe, useAuth } from "@/lib/auth"
import { useAdminGet } from "@/lib/hooks"
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

// Mirrors the OLD admin's structure: Dashboard · Client Area (users → cloud
// resources, in the old sidebar order) · System (the old Settings areas).
// Every item carries the old app's requiredPermission and is hidden unless
// /admin/me grants it; Validations additionally needs the billing
// configuration's autoActivationFlow to enable billing-profile validation.
type Item = {
  to: string
  label: string
  icon: React.ComponentType<{ className?: string }>
  permission?: string
  special?: "validations"
}
type Group = { label: string; items: Item[] }

const groups: Group[] = [
  {
    label: "Overview",
    items: [{ to: "/dashboard", label: "Dashboard", icon: LayoutDashboard }],
  },
  {
    label: "Client area",
    items: [
      { to: "/clients/users", label: "Users", icon: Users, permission: "admin:user:read" },
      { to: "/clients/projects", label: "Projects", icon: FolderKanban, permission: "admin:project:read" },
      { to: "/clients/organizations", label: "Organizations", icon: Building2, permission: "admin:organization:read" },
      { to: "/clients/billing-profiles", label: "Billing profiles", icon: Wallet, permission: "admin:billing_profile:read" },
      { to: "/clients/validations", label: "Validations", icon: CheckSquare, permission: "admin:billing_profile:read", special: "validations" },
      { to: "/clients/bills", label: "Customer bills", icon: Receipt, permission: "admin:bill:read" },
      { to: "/clients/cloud-resources", label: "Cloud resources", icon: Server, permission: "admin:cloud_resource:read" },
      { to: "/clients/transactions", label: "Transactions", icon: CreditCard, permission: "admin:transaction:read" },
      { to: "/clients/bank-transfers", label: "Bank transfers", icon: Banknote, permission: "admin:transaction:read" },
    ],
  },
  {
    label: "System",
    items: [
      { to: "/system/configuration", label: "Platform", icon: FileText, permission: "admin:platform_config:read" },
      { to: "/system/price-plans", label: "Price plans", icon: Tag, permission: "admin:price_plan:read" },
      { to: "/system/billing-configuration", label: "Billing configuration", icon: Gauge, permission: "admin:billing_config:read" },
      { to: "/system/menu", label: "Custom menu", icon: MenuIcon, permission: "admin:menu:manage" },
      { to: "/system/taxes", label: "Taxes", icon: Percent, permission: "admin:tax:read" },
      { to: "/system/templates", label: "Message templates", icon: Mail, permission: "admin:message_template:read" },
      { to: "/system/catalog", label: "Instances", icon: Blocks, permission: "admin:flavor_category:manage" },
      { to: "/system/pve-flavors", label: "Flavors (Proxmox)", icon: Blocks, permission: "admin:service:read" },
      { to: "/system/cloud-providers", label: "Cloud providers", icon: Cloud, permission: "admin:service:read" },
      { to: "/system/savings-plans", label: "Savings plans", icon: PiggyBank, permission: "admin:savings_plan:read" },
      { to: "/system/promotions", label: "Promotion codes", icon: Gauge, permission: "admin:promotional_credit:manage" },
      { to: "/system/integrations", label: "Integrations", icon: Settings2, permission: "admin:integration:read" },
      { to: "/system/roles", label: "Admin permissions", icon: Shield, permission: "admin:permission:read" },
      { to: "/system/hmac-keys", label: "API keys", icon: KeyRound, permission: "admin:hmac_key:manage" },
      { to: "/system/sso", label: "Organization SSO", icon: Shield, permission: "admin:sso:manage" },
      { to: "/audit", label: "Audit log", icon: FileClock, permission: "admin:audit:read" },
    ],
  },
]

// Permission match supports the wildcard grants /admin/me can carry
// (admin:* or admin:<area>:*), mirroring the API's ExpandPatterns.
function hasPermission(granted: string[] | undefined, want?: string): boolean {
  if (!want) return true
  if (!granted) return true // still loading — don't flash-hide
  if (granted.includes(want) || granted.includes("admin:*")) return true
  const area = want.split(":").slice(0, 2).join(":")
  return granted.includes(`${area}:*`)
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

export function AdminShell() {
  const auth = useAuth()
  const location = useLocation()
  const { data: me } = useAdminMe()
  // Validations visibility follows the old app: hidden unless the billing
  // configuration enables billing-profile validation in its activation flow.
  const { data: billingCfg } = useAdminGet<Record<string, any>>("/admin/billing/configuration/current")
  const validationMode = billingCfg?.autoActivationFlow?.billingProfileValidation as string | undefined
  const validationsVisible = validationMode === "REQUIRED" || validationMode === "ALTERNATIVE"

  const visibleGroups = groups
    .map((g) => ({
      ...g,
      items: g.items.filter((it) => {
        if (!hasPermission(me?.permissions, it.permission)) return false
        if (it.special === "validations" && !validationsVisible) return false
        return true
      }),
    }))
    .filter((g) => g.items.length > 0)

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

  const email = me?.email ?? auth.user?.profile.preferred_username
  const initial = (email ?? "?").slice(0, 1).toUpperCase()

  return (
    <SidebarProvider style={{ "--sidebar-width": "16rem" } as React.CSSProperties}>
      <Sidebar collapsible="icon">
        <SidebarHeader>
          {/* Wordmark + admin chip; collapses to the dot mark on the icon rail. */}
          <div className="flex h-9 items-center gap-2 px-2 group-data-[collapsible=icon]:justify-center group-data-[collapsible=icon]:px-0">
            <NavLink to="/dashboard" className="flex items-center gap-2">
              <span className="font-display text-lg font-semibold tracking-tight text-sidebar-foreground group-data-[collapsible=icon]:hidden">
                ProxCloud<span className="text-primary">.</span>
              </span>
              <span className="hidden size-5 items-center justify-center rounded-sm bg-primary font-display text-sm font-bold text-primary-foreground group-data-[collapsible=icon]:flex">
                S
              </span>
            </NavLink>
            <span className="text-eyebrow rounded border border-sidebar-border px-1.5 py-0.5 group-data-[collapsible=icon]:hidden">
              admin
            </span>
          </div>
        </SidebarHeader>

        <SidebarContent>
          {visibleGroups.map((g) => (
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

          {/* Page jump: full trigger ≥md, icon-only below. */}
          <Button
            variant="outline"
            size="sm"
            className="hidden w-56 justify-between text-muted-foreground md:flex"
            onClick={() => setSearchOpen(true)}
          >
            <span className="inline-flex items-center gap-2"><SearchIcon className="size-4" /> Go to page…</span>
            <Kbd>⌘K</Kbd>
          </Button>
          <Button
            variant="ghost"
            size="icon"
            className="md:hidden"
            aria-label="Go to page"
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
                  <span className="hidden max-w-44 truncate lg:inline">{me?.role ?? email ?? "Admin"}</span>
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuLabel className="font-mono text-xs">{email}</DropdownMenuLabel>
                {me?.role && (
                  <DropdownMenuLabel className="pt-0 text-eyebrow">{me.role}</DropdownMenuLabel>
                )}
                <DropdownMenuSeparator />
                <DropdownMenuItem onClick={() => void auth.signoutRedirect()}>
                  <LogOut className="mr-2 size-4" /> Sign out
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </header>
        <SearchModal groups={visibleGroups} open={searchOpen} onOpenChange={setSearchOpen} />
        <main className="flex-1 px-4 py-6 md:px-6">
          <div className="mx-auto w-full max-w-6xl">
            <Outlet />
          </div>
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}
