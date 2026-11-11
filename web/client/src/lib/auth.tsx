// OIDC auth (Keycloak, authorization code + PKCE) via react-oidc-context.
// After the first sign-in the platform user is initialized with
// POST /user (form field id_token) — idempotent get-or-create.
import { useEffect, useRef, useState } from "react"
import { AuthProvider, useAuth as useOidcAuth, type AuthProviderProps } from "react-oidc-context"
import { WebStorageStateStore } from "oidc-client-ts"
import { config } from "./config"
import { apiFetchEnvelope, setSsoRefusalHandler, setTokenProvider } from "./api"
import { Button } from "@/components/ui/button"
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
    // strip ?code&state from the URL after the redirect completes
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
// always-authenticated session (see src/mocks/).
export const useAuth: typeof useOidcAuth = MOCK_ENABLED
  ? (() => mockAuthState as unknown as ReturnType<typeof useOidcAuth>)
  : useOidcAuth

const SSO_REFUSALS: Record<string, string> = {
  sso_required: "Your organization requires single sign-on. Sign out, then sign in with your work email.",
  sso_domain_not_allowed: "Your identity provider cannot sign in this email address. Ask your administrator to verify the domain.",
  sso_idp_mismatch: "This account belongs to a different identity provider. Sign in through your organization's provider.",
  sso_idp_disabled: "Your organization's single sign-on is turned off. Contact your administrator.",
  sso_operator_refused: "Single sign-on accounts cannot use this console.",
}

// Wires the bearer token into the API client + runs the one-time user init. The first render waits
// for the init: on a first SSO sign-in it is what adds the person to their organization.
export function AuthBridge({ children }: { children: React.ReactNode }) {
  const auth = useAuth()
  const started = useRef(false)
  const sub = auth.user?.profile?.sub
  const [initDone, setInitDone] = useState(() => MOCK_ENABLED || sessionStorage.getItem("proxcloud.userInit") === sub)
  const [refusal, setRefusal] = useState("")

  setTokenProvider(() => auth.user?.access_token)
  setSsoRefusalHandler(setRefusal)

  useEffect(() => {
    if (!auth.isAuthenticated || started.current) return
    const idToken = auth.user?.id_token
    if (!idToken || sessionStorage.getItem("proxcloud.userInit") === auth.user?.profile?.sub) {
      setInitDone(true)
      return
    }
    started.current = true
    apiFetchEnvelope("/user", { method: "POST", form: { id_token: idToken } })
      .then(() => sessionStorage.setItem("proxcloud.userInit", auth.user?.profile?.sub ?? ""))
      .catch((e) => console.warn("user init failed", e))
      .finally(() => setInitDone(true))
  }, [auth.isAuthenticated, auth.user])

  if (refusal) {
    return (
      <main className="flex min-h-screen items-center justify-center bg-background p-6 text-foreground">
        <div className="flex max-w-md flex-col items-center gap-5 text-center" role="alert">
          <h1 className="font-display text-2xl font-medium">Single sign-on required</h1>
          <p className="text-muted-foreground">{SSO_REFUSALS[refusal] ?? "Your session is not allowed by your organization's sign-in rules."}</p>
          <Button
            onClick={() => {
              sessionStorage.removeItem("proxcloud.userInit")
              void auth.signoutRedirect()
            }}
          >
            Sign out
          </Button>
        </div>
      </main>
    )
  }
  if (auth.isAuthenticated && !initDone) {
    return <div className="flex min-h-screen items-center justify-center text-muted-foreground">Signing in…</div>
  }
  return <>{children}</>
}
