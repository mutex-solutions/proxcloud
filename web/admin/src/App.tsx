import { Suspense, lazy } from "react"
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom"
import { useAuth } from "@/lib/auth"
import { AdminShell } from "@/components/layout/AdminShell"
import { LoginPage } from "@/pages/Login"
import { DashboardPage } from "@/pages/Dashboard"

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
              <AdminShell />
            </Protected>
          }
        >
          <Route index element={<Navigate to="dashboard" replace />} />
          <Route path="dashboard" element={<DashboardPage />} />

          {/* Client area */}
          <Route path="clients/users" element={lazyPage(() => import("@/pages/clients/UsersPage"))} />
          <Route path="clients/users/:id" element={lazyPage(() => import("@/pages/clients/UserDetailPage"))} />
          <Route path="clients/organizations" element={lazyPage(() => import("@/pages/clients/OrganizationsPage"))} />
          <Route path="clients/organizations/:id" element={lazyPage(() => import("@/pages/clients/OrganizationDetailPage"))} />
          <Route path="clients/billing-profiles" element={lazyPage(() => import("@/pages/clients/BillingProfilesPage"))} />
          <Route path="clients/billing-profiles/:id" element={lazyPage(() => import("@/pages/clients/BillingProfileDetailPage"))} />
          <Route path="clients/projects" element={lazyPage(() => import("@/pages/clients/ProjectsPage"))} />
          <Route path="clients/projects/:id" element={lazyPage(() => import("@/pages/clients/ProjectDetailPage"))} />
          <Route path="clients/bills" element={lazyPage(() => import("@/pages/clients/BillsPage"))} />
          <Route path="clients/transactions" element={lazyPage(() => import("@/pages/clients/TransactionsPage"))} />
          <Route path="clients/account-credits" element={lazyPage(() => import("@/pages/clients/AccountCreditsPage"))} />
          <Route path="clients/bank-transfers" element={lazyPage(() => import("@/pages/clients/BankTransfersPage"))} />
          <Route path="clients/validations" element={lazyPage(() => import("@/pages/clients/ValidationsPage"))} />
          <Route path="clients/cloud-resources" element={lazyPage(() => import("@/pages/clients/CloudResourcesPage"))} />
          <Route path="clients/cloud-resources/:id" element={lazyPage(() => import("@/pages/clients/CloudResourceDetailPage"))} />

          {/* Billing setup */}
          <Route path="system/price-plans" element={lazyPage(() => import("@/pages/system/PricePlansPage"))} />
          <Route path="system/price-plans/:id" element={lazyPage(() => import("@/pages/system/PricePlanDetailPage"))} />
          <Route path="system/taxes" element={lazyPage(() => import("@/pages/system/TaxesPage"))} />
          <Route path="system/savings-plans" element={lazyPage(() => import("@/pages/system/SavingsPlansPage"))} />
          <Route path="system/promotions" element={lazyPage(() => import("@/pages/system/PromotionsPage"))} />

          {/* Platform */}
          <Route path="system/cloud-providers" element={lazyPage(() => import("@/pages/system/CloudProvidersPage"))} />
          <Route path="system/cloud-providers/:id" element={lazyPage(() => import("@/pages/system/CloudProviderDetailPage"))} />
          <Route path="system/template-updates" element={lazyPage(() => import("@/pages/system/TemplateUpdatesPage"))} />
          <Route path="system/catalog" element={lazyPage(() => import("@/pages/system/CatalogPage"))} />
          <Route path="system/pve-flavors" element={lazyPage(() => import("@/pages/system/PVEFlavorsPage"))} />
          <Route path="system/templates" element={lazyPage(() => import("@/pages/system/TemplatesPage"))} />
          <Route path="system/integrations" element={lazyPage(() => import("@/pages/system/IntegrationsPage"))} />
          <Route path="system/configuration" element={lazyPage(() => import("@/pages/system/ConfigurationPage"))} />
          <Route path="system/billing-configuration" element={lazyPage(() => import("@/pages/system/BillingConfigurationPage"))} />
          <Route path="system/menu" element={lazyPage(() => import("@/pages/system/MenuPage"))} />
          <Route path="system/roles" element={lazyPage(() => import("@/pages/system/RolesPage"))} />
          <Route path="system/hmac-keys" element={lazyPage(() => import("@/pages/system/HmacKeysPage"))} />
          <Route path="system/sso" element={lazyPage(() => import("@/pages/system/SsoPage"))} />
          <Route path="audit" element={lazyPage(() => import("@/pages/AuditPage"))} />
        </Route>
        {/* Public documentation (no auth) — markdown-driven, see src/docs/. */}
        <Route path="/docs" element={lazyPage(() => import("@/pages/docs/DocsPage"))} />
        <Route path="/docs/*" element={lazyPage(() => import("@/pages/docs/DocsPage"))} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  )
}
