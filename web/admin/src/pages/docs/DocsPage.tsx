// Public documentation viewer (/docs/*). Markdown-driven: pages live in
// src/docs/content/**/*.md, the sidebar comes from src/docs/manifest.ts.
// Editing docs = editing markdown; no code changes needed for new pages
// beyond a manifest entry.
import { useEffect } from "react"
import { Link, useLocation, useParams } from "react-router-dom"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { ArrowLeft, ArrowRight, BookOpen, Check, ChevronDown, Monitor, Moon, PanelsTopLeft, Sun } from "lucide-react"
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

// Slugified heading ids so sections deep-link (#building-a-plan); scroll-mt
// keeps the target clear of the sticky header.
function headingText(node: React.ReactNode): string {
  if (typeof node === "string" || typeof node === "number") return String(node)
  if (Array.isArray(node)) return node.map(headingText).join("")
  if (node && typeof node === "object" && "props" in node) {
    return headingText((node as { props: { children?: React.ReactNode } }).props.children)
  }
  return ""
}
function headingId(children: React.ReactNode): string {
  return headingText(children)
    .toLowerCase()
    .replace(/[^a-z0-9\s-]/g, "")
    .trim()
    .replace(/\s+/g, "-")
}

const THEME_OPTIONS: Array<{ value: ThemePref; label: string; icon: React.ComponentType<{ className?: string }> }> = [
  { value: "light", label: "Light", icon: Sun },
  { value: "dark", label: "Dark", icon: Moon },
  { value: "system", label: "System", icon: Monitor },
]

// Docs carry their own chrome (no AdminShell), so they get their own theme
// menu — same lib/theme store the console uses, so the preference follows you.
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

// Section list shared by the desktop sidebar and the mobile disclosure.
function SectionNav({ slug }: { slug: string }) {
  return (
    <>
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
    </>
  )
}

// Reading-order pager: the manifest flattened, with each page's section title
// kept for the prev/next footer eyebrows.
const flatPages = sections.flatMap((s) => s.pages.map((p) => ({ ...p, section: s.title })))

export default function DocsPage() {
  const params = useParams()
  const { hash } = useLocation()
  const slug = params["*"] && params["*"] !== "" ? params["*"].replace(/\/$/, "") : defaultSlug
  const md = contentFor(slug)
  const section = sections.find((s) => s.pages.some((p) => p.slug === slug))
  const page = section?.pages.find((p) => p.slug === slug)
  const flatIndex = flatPages.findIndex((p) => p.slug === slug)
  const prev = flatIndex > 0 ? flatPages[flatIndex - 1] : undefined
  const next = flatIndex >= 0 && flatIndex < flatPages.length - 1 ? flatPages[flatIndex + 1] : undefined

  useEffect(() => {
    document.title = `${page?.title ?? slug} — ${docsTitle}`
    // Honor a heading deep link (#building-a-plan); otherwise start at the top.
    const target = hash ? document.getElementById(hash.slice(1)) : null
    if (target) target.scrollIntoView()
    else window.scrollTo(0, 0)
  }, [slug, page?.title, hash])

  return (
    // Theme class lives on <html> (lib/theme + the pre-paint script); this
    // wrapper only paints the tokens.
    <div className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-10 flex h-[var(--navbar-height)] items-center gap-2.5 border-b bg-background/80 px-4 backdrop-blur md:px-6">
        <Link to="/docs" className="flex items-center gap-2.5">
          <span className="font-display text-lg font-semibold tracking-tight">
            ProxCloud<span className="text-primary">.</span>
          </span>
          <span className="text-eyebrow rounded border px-1.5 py-0.5">admin docs</span>
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
          <nav
            aria-label="Docs sections"
            className="sticky top-[calc(var(--navbar-height)+2rem)] space-y-6 text-sm"
          >
            <SectionNav slug={slug} />
          </nav>
        </aside>
        <main className="min-w-0 flex-1 pb-16">
          {/* Mobile substitute for the hidden sidebar: same nav, disclosed. */}
          <details className="group mb-6 rounded-lg border md:hidden">
            <summary className="flex cursor-pointer list-none items-center justify-between px-3 py-2 text-sm font-medium [&::-webkit-details-marker]:hidden">
              Browse docs
              <ChevronDown className="size-4 text-muted-foreground transition-transform group-open:rotate-180" aria-hidden="true" />
            </summary>
            <nav aria-label="Docs sections" className="space-y-6 border-t px-3 py-3 text-sm">
              <SectionNav slug={slug} />
            </nav>
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
                  h2: ({ children, ...p }) => (
                    <h2
                      id={headingId(children)}
                      className="mb-3 mt-10 scroll-mt-[calc(var(--navbar-height)+1rem)] border-b pb-2 font-display text-2xl font-semibold tracking-tight"
                      {...p}
                    >
                      {children}
                    </h2>
                  ),
                  h3: ({ children, ...p }) => (
                    <h3
                      id={headingId(children)}
                      className="mb-2 mt-6 scroll-mt-[calc(var(--navbar-height)+1rem)] font-display text-xl font-semibold"
                      {...p}
                    >
                      {children}
                    </h3>
                  ),
                  p: (p) => <p className="my-3 leading-7" {...p} />,
                  ul: (p) => <ul className="my-3 list-disc space-y-1 pl-6" {...p} />,
                  ol: (p) => <ol className="my-3 list-decimal space-y-1 pl-6" {...p} />,
                  // Internal /docs links stay client-side; external ones open a tab.
                  a: ({ href, ...p }) =>
                    href?.startsWith("/docs") ? (
                      <Link to={href} className="font-medium text-primary-text underline underline-offset-4" {...p} />
                    ) : (
                      <a
                        href={href}
                        className="font-medium text-primary-text underline underline-offset-4"
                        {...(href?.startsWith("http") ? { target: "_blank", rel: "noreferrer" } : {})}
                        {...p}
                      />
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
              {(prev || next) && (
                <nav aria-label="Docs pager" className="mt-12 grid gap-3 border-t pt-6 sm:grid-cols-2">
                  {prev ? (
                    <Link
                      to={`/docs/${prev.slug}`}
                      className="group rounded-lg border p-4 transition-colors hover:border-primary/40 hover:bg-muted/40"
                    >
                      <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                        <ArrowLeft className="size-3.5 transition-transform group-hover:-translate-x-0.5" aria-hidden="true" />
                        Previous · {prev.section}
                      </span>
                      <span className="mt-1 block font-medium">{prev.title}</span>
                    </Link>
                  ) : (
                    <span className="hidden sm:block" />
                  )}
                  {next ? (
                    <Link
                      to={`/docs/${next.slug}`}
                      className="group rounded-lg border p-4 text-right transition-colors hover:border-primary/40 hover:bg-muted/40"
                    >
                      <span className="flex items-center justify-end gap-1.5 text-xs text-muted-foreground">
                        Next · {next.section}
                        <ArrowRight className="size-3.5 transition-transform group-hover:translate-x-0.5" aria-hidden="true" />
                      </span>
                      <span className="mt-1 block font-medium">{next.title}</span>
                    </Link>
                  ) : null}
                </nav>
              )}
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
