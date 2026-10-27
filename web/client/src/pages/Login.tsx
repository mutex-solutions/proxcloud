import { useEffect } from "react"
import { BookOpen } from "lucide-react"
import { useAuth } from "@/lib/auth"
import { Button } from "@/components/ui/button"

// Public landing: one job — sign in. Split layout after the design-system product
// logins: a full-bleed brand visual on the left (space photography over the
// pixel-mosaic texture), a centered sign-in column on the right. Fully
// token-driven; works in both themes.
export function LoginPage() {
  const auth = useAuth()

  useEffect(() => {
    document.title = "ProxCloud Console"
  }, [])

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
            <Button
              size="lg"
              className="w-full"
              disabled={auth.isLoading}
              onClick={() => void auth.signinRedirect()}
            >
              {auth.isLoading ? "Checking session…" : "Sign in"}
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
