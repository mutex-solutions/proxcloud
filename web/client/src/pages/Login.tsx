import { useEffect, useState } from "react"
import { BookOpen } from "lucide-react"
import { useAuth } from "@/lib/auth"
import { apiFetch } from "@/lib/api"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

// Public landing: one job — sign in. Split layout after the design-system product
// logins: a full-bleed brand visual on the left (space photography over the
// pixel-mosaic texture), a centered sign-in column on the right. Fully
// token-driven; works in both themes.
export function LoginPage() {
  const auth = useAuth()
  const [email, setEmail] = useState("")
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    document.title = "ProxCloud Console"
  }, [])

  // Email first: an address in a domain with company SSO goes straight to that organization's
  // identity provider, everyone else to the normal sign-in page. A failed lookup is not fatal.
  const continueWithEmail = async () => {
    const address = email.trim()
    if (!address) return void auth.signinRedirect()
    setBusy(true)
    const found = await apiFetch<{ found: boolean; alias?: string }>("/auth/sso/discover", {
      method: "POST",
      body: { email: address },
    }).catch(() => ({ found: false, alias: undefined }))
    await auth.signinRedirect({
      extraQueryParams: found.found && found.alias ? { kc_idp_hint: found.alias, login_hint: address } : { login_hint: address },
    })
  }

  return (
    <main className="flex min-h-screen w-full gap-3 bg-background p-3 text-foreground md:gap-5 md:p-5">
      {/* Brand visual — dropped below lg */}
      <div className="relative hidden overflow-hidden rounded-2xl border lg:block lg:w-[55%] xl:w-[58%]">
        <div aria-hidden className="h-full w-full bg-linear-to-br from-primary/40 via-background to-background" />
        <div className="absolute bottom-5 left-5 flex items-center gap-2 rounded-full bg-black/40 px-3 py-1.5 backdrop-blur">
          <span className="status-dot status-dot-ok" />
          <span className="font-mono text-[11px] uppercase tracking-[0.05em] text-white/80">
            All regions operational
          </span>
        </div>
      </div>

      {/* Sign-in column */}
      <div className="flex flex-1 items-center justify-center px-4 py-8">
        <div className="flex w-full max-w-md flex-col items-center gap-10 text-center">
          <div className="flex items-center gap-2.5">
            <img src="/brand/logo.svg" alt="ProxCloud" className="h-6 w-auto" />
            <span className="text-eyebrow rounded border px-1.5 py-0.5">console</span>
          </div>

          <div className="flex flex-col gap-4">
            <h1 className="font-display text-4xl font-medium tracking-tight md:text-5xl">
              ProxCloud<span className="text-primary">.</span>
            </h1>
            <p className="text-lg text-muted-foreground md:text-xl">
              Compute, networking, storage and billing for your cloud — in one console.
            </p>
          </div>

          <div className="flex w-full flex-col gap-3">
            <form
              className="flex flex-col gap-3 text-left"
              onSubmit={(e) => {
                e.preventDefault()
                void continueWithEmail()
              }}
            >
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="login-email">Work email</Label>
                <Input
                  id="login-email"
                  type="email"
                  autoComplete="email"
                  placeholder="you@example.com"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                />
              </div>
              <Button type="submit" size="lg" className="w-full" disabled={auth.isLoading || busy}>
                {auth.isLoading ? "Checking session…" : "Continue"}
              </Button>
            </form>
            <Button variant="ghost" size="lg" className="w-full" disabled={auth.isLoading} onClick={() => void auth.signinRedirect()}>
              Sign in with a password
            </Button>
            <Button asChild variant="outline" size="lg" className="w-full">
              <a href="/docs" target="_blank" rel="noopener noreferrer">
                <BookOpen className="size-5" /> View docs
              </a>
            </Button>
            <p className="mt-1 text-xs text-muted-foreground">
              New here? Create your account from the sign-in page.
            </p>
          </div>
        </div>
      </div>
    </main>
  )
}
