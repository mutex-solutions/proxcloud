// OIDC auth for the admin console — Keycloak master realm, client
// `proxcloud-admin`. Any master-realm user is auto-provisioned SUPER_ADMIN by
// the API; /admin/me returns role + granted permissions.
import { AuthProvider, useAuth as useOidcAuth, type AuthProviderProps } from "react-oidc-context"
import { WebStorageStateStore } from "oidc-client-ts"
import { useQuery } from "@tanstack/react-query"
import { config } from "./config"
import { apiFetch, setTokenProvider } from "./api"
import { MOCK_ENABLED, mockAuthState } from "@/mocks/enabled"

const oidcConfig: AuthProviderProps = {
  authority: config.authIssuer,
  client_id: config.authClientId,
  redirect_uri: window.location.origin + "/",
  post_logout_redirect_uri: window.location.origin + "/",
  scope: config.authScope,
  userStore: new WebStorageStateStore({ store: window.localStorage }),
  // PKCE S256 needs crypto.subtle, which the browser only exposes in a SECURE
  // CONTEXT (https, or http on localhost). Served over plain http from a LAN IP
  // there is no crypto.subtle and signinRedirect() fails silently — live-caught
  // on http://<lan-ip>:8082. Fall back to a non-PKCE code flow there so a
  // dev/demo deployment still logs in; https and localhost keep full PKCE.
  disablePKCE: !window.isSecureContext,
  onSigninCallback: () => {
    window.history.replaceState({}, document.title, window.location.pathname)
  },
}

if (!window.isSecureContext) {
  console.warn(
    "proxcloud: insecure context (plain http on a non-localhost origin) — PKCE is disabled. " +
      "Serve the console over https for production.",
  )
}

export function ProxCloudAuthProvider({ children }: { children: React.ReactNode }) {
  if (MOCK_ENABLED) return <>{children}</>
  return <AuthProvider {...oidcConfig}>{children}</AuthProvider>
}

// In mock mode there is no OIDC provider; useAuth resolves to a canned
// always-authenticated master-realm session (see src/mocks/).
export const useAuth: typeof useOidcAuth = MOCK_ENABLED
  ? (() => mockAuthState as unknown as ReturnType<typeof useOidcAuth>)
  : useOidcAuth

export function AuthBridge({ children }: { children: React.ReactNode }) {
  const auth = useAuth()
  setTokenProvider(() => auth.user?.access_token)
  return <>{children}</>
}

export type AdminMe = {
  sub?: string
  email?: string
  firstName?: string
  lastName?: string
  role?: string
  permissions?: string[]
}

// Permission match supports the wildcard grants /admin/me can carry
// (admin:* or admin:<area>:*), mirroring the API's ExpandPatterns.
export function hasPermission(granted: string[] | undefined, want?: string): boolean {
  if (!want) return true
  if (!granted) return true // still loading: do not flash-hide
  if (granted.includes(want) || granted.includes("admin:*")) return true
  const area = want.split(":").slice(0, 2).join(":")
  return granted.includes(`${area}:*`)
}

export function useAdminMe() {
  const auth = useAuth()
  return useQuery({
    queryKey: ["admin-me"],
    queryFn: () => apiFetch<AdminMe>("/admin/me"),
    enabled: auth.isAuthenticated,
  })
}
