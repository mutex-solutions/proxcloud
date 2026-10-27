// Public documentation viewer (/docs/*). Markdown-driven: pages live in
// src/docs/content/**/*.md, the sidebar comes from src/docs/manifest.ts.
// Editing docs = editing markdown; no code changes needed for new pages
// beyond a manifest entry.
import { useEffect } from "react"
import { Link, useParams } from "react-router-dom"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { BookOpen, Check, ChevronDown, Monitor, Moon, PanelsTopLeft, Sun } from "lucide-react"
import { docsTitle, sections, defaultSlug } from "@/docs/manifest"
import { useTheme, type ThemePref } from "@/lib/theme"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { EmptyState } from "@/components/empty-state"

// Eager raw import keeps lookup synchronous and the whole docs set is small text.
const files = import.meta.glob("../../docs/content/**/*.md", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>

function contentFor(slug: string): string | undefined {
  return files[`../../docs/content/${slug}.md`] ?? files[`../../docs/content/${slug}/index.md`]
}

const THEME_OPTIONS: Array<{ value: ThemePref; label: string; icon: React.ComponentType<{ className?: string }> }> = [
  { value: "light", label: "Light", icon: Sun },
  { value: "dark", label: "Dark", icon: Moon },
  { value: "system", label: "System", icon: Monitor },
]

// Docs carry their own chrome (no AppShell), so they get their own theme menu —
// same lib/theme store the console uses, so the preference follows you across.
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

// Section/page tree, shared by the desktop rail and the mobile disclosure.
function DocsNav({ slug }: { slug: string }) {
  return (
    <nav className="space-y-6 text-sm">
      {sections.map((s) => (
        <div key={s.title}>
          <div className="text-eyebrow mb-2">{s.title}</div>
          <ul className="space-y-0.5 border-l pl-3">
            {s.pages.map((p) => (
              <li key={p.slug}>
                <Link
                  to={`/docs/${p.slug}`}
                  aria-current={slug === p.slug ? "page" : undefined}
                  className={
                    slug === p.slug
                      ? "block rounded-sm px-2 py-1 font-medium text-primary-text"
                      : "block rounded-sm px-2 py-1 text-muted-foreground transition-colors hover:text-foreground"
                  }
                >
                  {p.title}
                </Link>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </nav>
  )
}

export default function DocsPage() {
  const params = useParams()
  const slug = params["*"] && params["*"] !== "" ? params["*"].replace(/\/$/, "") : defaultSlug
  const md = contentFor(slug)
  const section = sections.find((s) => s.pages.some((p) => p.slug === slug))
  const page = section?.pages.find((p) => p.slug === slug)

  useEffect(() => {
    document.title = `${page?.title ?? slug} — ${docsTitle}`
    window.scrollTo(0, 0)
  }, [slug, page?.title])

  return (
    // Theme class lives on <html> (lib/theme + the pre-paint script); this
    // wrapper only paints the tokens.
    <div className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-10 flex h-[var(--navbar-height)] items-center gap-2.5 border-b bg-background/80 px-4 backdrop-blur md:px-6">
        <Link to="/docs" className="flex items-center gap-2.5">
          <span className="font-display text-lg font-semibold tracking-tight">
            ProxCloud<span className="text-primary">.</span>
          </span>
          <span className="text-eyebrow rounded border px-1.5 py-0.5">docs</span>
        </Link>
        <div className="ml-auto flex items-center gap-1.5">
          <Button variant="ghost" size="sm" asChild>
            <Link to="/">
              <PanelsTopLeft className="size-4" />
              <span className="hidden md:inline">Console</span>
            </Link>
          </Button>
          <ThemeMenu />
        </div>
      </header>
      <div className="mx-auto flex w-full max-w-6xl gap-10 px-4 py-8 md:px-6">
        <aside className="hidden w-60 shrink-0 md:block">
          <div className="sticky top-[calc(var(--navbar-height)+2rem)]">
            <DocsNav slug={slug} />
          </div>
        </aside>
        <main className="min-w-0 max-w-3xl flex-1 pb-16">
          {/* Mobile: the rail is hidden, so docs need a way to move between
              pages. Keyed by slug so it re-collapses after navigating. */}
          <details key={slug} className="group mb-6 rounded-xl border bg-card shadow-sm md:hidden">
            <summary className="flex cursor-pointer list-none items-center justify-between gap-2 px-4 py-3 text-sm font-medium [&::-webkit-details-marker]:hidden">
              {page?.title ?? "Browse docs"}
              <ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" />
            </summary>
            <div className="border-t px-4 py-4">
              <DocsNav slug={slug} />
            </div>
          </details>
          {md ? (
            <>
              {section ? <div className="text-eyebrow mb-3">{section.title}</div> : null}
              <ReactMarkdown
                remarkPlugins={[remarkGfm]}
                components={{
                  h1: ({ children, ...p }) => (
                    <>
                      <h1 className="font-display text-3xl font-semibold tracking-tight" {...p}>
                        {children}
                      </h1>
                      <div className="horizon mb-6 mt-3 max-w-[420px]" />
                    </>
                  ),
                  h2: (p) => (
                    <h2 className="mb-3 mt-10 border-b pb-2 font-display text-2xl font-semibold tracking-tight" {...p} />
                  ),
                  h3: (p) => <h3 className="mb-2 mt-6 font-display text-xl font-semibold" {...p} />,
                  p: (p) => <p className="my-3 leading-7" {...p} />,
                  ul: (p) => <ul className="my-3 list-disc space-y-1 pl-6" {...p} />,
                  ol: (p) => <ol className="my-3 list-decimal space-y-1 pl-6" {...p} />,
                  a: (p) => (
                    <a className="font-medium text-primary-text underline underline-offset-4" {...p} />
                  ),
                  blockquote: (p) => (
                    <blockquote className="my-4 border-l-2 border-primary/50 pl-4 text-muted-foreground" {...p} />
                  ),
                  code: ({ className, children, ...rest }) =>
                    className ? (
                      <code className={`${className} block overflow-x-auto`} {...rest}>
                        {children}
                      </code>
                    ) : (
                      <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-[0.85em]" {...rest}>
                        {children}
                      </code>
                    ),
                  pre: (p) => (
                    <pre className="my-4 overflow-x-auto rounded-lg border bg-muted/60 p-4 font-mono text-sm" {...p} />
                  ),
                  table: (p) => (
                    <div className="my-4 overflow-x-auto rounded-lg border">
                      <table className="w-full border-collapse text-sm" {...p} />
                    </div>
                  ),
                  th: (p) => (
                    <th className="border-b bg-muted/50 px-3 py-2 text-left font-medium" {...p} />
                  ),
                  td: (p) => <td className="border-b px-3 py-2 align-top" {...p} />,
                  tr: (p) => <tr className="last:[&>td]:border-b-0" {...p} />,
                  img: (p) => <img className="my-4 max-w-full rounded-lg border" loading="lazy" {...p} />,
                  hr: () => <hr className="my-8" />,
                }}
              >
                {md}
              </ReactMarkdown>
            </>
          ) : (
            <EmptyState
              icon={BookOpen}
              title="Page not found"
              hint="This page may have moved. Browse the sidebar or head back to the docs home."
              action={
                <Button asChild variant="outline">
                  <Link to="/docs">Back to docs</Link>
                </Button>
              }
            />
          )}
        </main>
      </div>
    </div>
  )
}
