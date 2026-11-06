import { Suspense, lazy } from "react"
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom"
import { useAuth } from "@/lib/auth"
import { AppShell } from "@/components/layout/AppShell"
import { LoginPage } from "@/pages/Login"
import { HomePage } from "@/pages/Home"
import { DashboardPage } from "@/pages/Dashboard"
import { ServersPage } from "@/pages/servers/ServersPage"
import { ServerDetailPage } from "@/pages/servers/ServerDetailPage"

// Lazy page groups (built out under src/pages/**).
const lazyPage = (loader: () => Promise<{ default: React.ComponentType }>) => {
  const C = lazy(loader)
  return (
    <Suspense fallback={<div className="py-20 text-center text-muted-foreground">Loading…</div>}>
      <C />
    </Suspense>
  )
}

function Protected({ children }: { children: React.ReactNode }) {
  const auth = useAuth()
  if (auth.isLoading) {
    return <div className="flex min-h-screen items-center justify-center text-muted-foreground">Checking session…</div>
  }
  if (!auth.isAuthenticated) return <LoginPage />
  return <>{children}</>
}

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route
          path="/"
          element={
            <Protected>
              <HomePage />
            </Protected>
          }
        />
        <Route
          path="/p/:pid"
          element={
            <Protected>
              <AppShell />
            </Protected>
          }
        >
          <Route index element={<Navigate to="dashboard" replace />} />
          <Route path="dashboard" element={<DashboardPage />} />

          {/* Compute */}
          <Route path="servers" element={<ServersPage />} />
          <Route path="servers/new" element={lazyPage(() => import("@/pages/servers/CreateServerPage"))} />
          <Route path="servers/:resourceId" element={<ServerDetailPage />} />
          <Route
            path="servers/:resourceId/console"
            element={lazyPage(() => import("@/pages/servers/ServerConsolePage"))}
          />
          <Route path="keypairs" element={lazyPage(() => import("@/pages/compute/KeypairsPage"))} />
          <Route path="images" element={lazyPage(() => import("@/pages/compute/ImagesPage"))} />

          {/* Storage */}
          <Route path="volumes" element={lazyPage(() => import("@/pages/storage/VolumesPage"))} />
          <Route path="snapshots" element={lazyPage(() => import("@/pages/storage/SnapshotsPage"))} />
          <Route path="object-storage" element={lazyPage(() => import("@/pages/storage/BucketsPage"))} />
          <Route path="object-storage/:resourceId" element={lazyPage(() => import("@/pages/storage/BucketExplorePage"))} />
          <Route path="s3-keys" element={lazyPage(() => import("@/pages/storage/S3KeysPage"))} />

          {/* Network */}
          <Route path="networks" element={lazyPage(() => import("@/pages/network/NetworksPage"))} />
          <Route path="networks/:resourceId" element={lazyPage(() => import("@/pages/network/NetworkDetailPage"))} />
          <Route path="security-groups" element={lazyPage(() => import("@/pages/network/SecurityGroupsPage"))} />
          <Route path="security-groups/:resourceId" element={lazyPage(() => import("@/pages/network/SecurityGroupDetailPage"))} />

          {/* Platform */}
          <Route path="kubernetes" element={lazyPage(() => import("@/pages/platform/KubernetesPage"))} />
          <Route path="kubernetes/:resourceId" element={lazyPage(() => import("@/pages/platform/KubernetesClusterDetailPage"))} />
          <Route path="databases" element={lazyPage(() => import("@/pages/platform/DatabasesPage"))} />
          <Route path="databases/:resourceId" element={lazyPage(() => import("@/pages/platform/DatabaseDetailPage"))} />

          {/* Billing */}
          <Route path="billing/savings" element={lazyPage(() => import("@/pages/billing/SavingsPage"))} />
          <Route path="billing/credits" element={lazyPage(() => import("@/pages/billing/CreditsPage"))} />
          <Route path="billing/funds" element={lazyPage(() => import("@/pages/billing/FundsPage"))} />
          <Route path="billing/cards" element={lazyPage(() => import("@/pages/billing/CardsPage"))} />
          <Route path="billing/history" element={lazyPage(() => import("@/pages/billing/HistoryPage"))} />
          <Route path="billing/history/bills/:billId" element={lazyPage(() => import("@/pages/billing/BillDetailPage"))} />

          {/* Custom menu ("More") */}
          <Route path="more/:slug" element={lazyPage(() => import("@/pages/MorePage"))} />

          {/* Organization */}
          <Route path="org/billing" element={lazyPage(() => import("@/pages/org/OrgBillingPage"))} />
          <Route path="org/members" element={lazyPage(() => import("@/pages/org/MembersPage"))} />
          <Route path="org/projects" element={lazyPage(() => import("@/pages/org/ProjectsPage"))} />
          <Route path="org/audit" element={lazyPage(() => import("@/pages/org/AuditPage"))} />
          <Route path="org/roles" element={lazyPage(() => import("@/pages/org/RolesPage"))} />
          <Route path="org/sso" element={lazyPage(() => import("@/pages/org/SsoPage"))} />
          <Route path="org/settings" element={lazyPage(() => import("@/pages/org/OrgSettingsPage"))} />
          <Route path="account" element={lazyPage(() => import("@/pages/account/AccountPage"))} />
        </Route>
        {/* Invite accept/decline (email deep-link: /join-project?invite-token=…) */}
        <Route
          path="/join-project"
          element={
            <Protected>
              {lazyPage(() => import("@/pages/JoinProjectPage"))}
            </Protected>
          }
        />
        <Route
          path="/join/:token"
          element={
            <Protected>
              {lazyPage(() => import("@/pages/JoinProjectPage"))}
            </Protected>
          }
        />
        {/* Public documentation (no auth) — markdown-driven, see src/docs/. */}
        <Route path="/docs" element={lazyPage(() => import("@/pages/docs/DocsPage"))} />
        <Route path="/docs/*" element={lazyPage(() => import("@/pages/docs/DocsPage"))} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  )
}
