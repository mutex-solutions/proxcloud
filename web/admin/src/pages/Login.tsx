import { useEffect } from "react"
import { BookOpen } from "lucide-react"
import { useAuth } from "@/lib/auth"
import { Button } from "@/components/ui/button"

// Public landing: one job — sign in. Same split layout as the customer
// console's login, differentiated for the operations plane: the left visual is
// the design-system pixel-mosaic hero pattern over a dark textured panel (the console
// gets the space photograph), and the chip reads "admin". Fully token-driven;
// works in both themes (the panel is locally forced dark via the .dark class
// so its tokens re-resolve).
export function LoginPage() {
  const auth = useAuth()

  useEffect(() => {
    document.title = "ProxCloud Admin"
  }, [])

  return (
    <main className="flex min-h-screen w-full gap-3 bg-background p-3 text-foreground md:gap-5 md:p-5">
      {/* Brand visual — dropped below lg */}
      <div className="dark relative hidden overflow-hidden rounded-2xl border bg-background bg-texture-subtle text-foreground lg:block lg:w-[55%] xl:w-[58%]">
        <div className="absolute bottom-5 left-5 flex items-center gap-2 rounded-full bg-black/40 px-3 py-1.5 backdrop-blur">
          <span className="status-dot status-dot-ok" />
          <span className="font-mono text-[11px] uppercase tracking-[0.05em] text-white/80">
            Control plane online
          </span>
        </div>
      </div>

      {/* Sign-in column */}
      <div className="flex flex-1 items-center justify-center px-4 py-8">
        <div className="flex w-full max-w-md flex-col items-center gap-10 text-center">
          <div className="flex items-center gap-2.5">
            <img src="/brand/logo.svg" alt="ProxCloud" className="h-6 w-auto" />
            <span className="text-eyebrow rounded border px-1.5 py-0.5">admin</span>
          </div>

          <div className="flex flex-col gap-4">
            <h1 className="font-display text-4xl font-medium tracking-tight md:text-5xl">
              ProxCloud operations<span className="text-primary">.</span>
            </h1>
            <p className="text-lg text-muted-foreground md:text-xl">
              Customers, billing, catalog and cloud providers — the ProxCloud control plane.
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
              Operator access only — sign in with your admin account.
            </p>
          </div>
        </div>
      </div>
    </main>
  )
}
