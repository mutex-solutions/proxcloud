// Tiny method+path matcher for the mock API. Handlers are registered as
// "METHOD /path/:param" patterns and receive extracted params, the query
// string, and the request opts (body etc.). Responses are API envelopes.
import type { Envelope, ReqOpts } from "@/lib/api"

export type MockRequest = {
  params: Record<string, string>
  query: URLSearchParams
  opts: ReqOpts
  method: string
  path: string
}

export type RouteHandler = (req: MockRequest) => Envelope<unknown> | Promise<Envelope<unknown>>

type Route = {
  method: string
  segments: string[] // ":name" segments capture
  handler: RouteHandler
}

const routes: Route[] = []

export function on(pattern: string, handler: RouteHandler) {
  const [method, path] = pattern.split(" ", 2)
  routes.push({ method: method.toUpperCase(), segments: path.split("/").filter(Boolean), handler })
}

// Simulated network latency so loading states render like the real app.
const LATENCY_MS = 120
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

export async function dispatch(path: string, opts: ReqOpts): Promise<Envelope<unknown>> {
  const method = (opts.method ?? (opts.body !== undefined || opts.form || opts.rawBody !== undefined ? "POST" : "GET")).toUpperCase()
  const [pathname, search = ""] = path.split("?", 2)
  const segs = pathname.split("/").filter(Boolean)

  for (const r of routes) {
    if (r.method !== method || r.segments.length !== segs.length) continue
    const params: Record<string, string> = {}
    let ok = true
    for (let i = 0; i < segs.length; i++) {
      const pat = r.segments[i]
      if (pat.startsWith(":")) params[pat.slice(1)] = decodeURIComponent(segs[i])
      else if (pat !== segs[i]) {
        ok = false
        break
      }
    }
    if (!ok) continue
    await sleep(LATENCY_MS)
    return r.handler({ params, query: new URLSearchParams(search), opts, method, path: pathname })
  }

  // Unhandled endpoint: warn loudly (so gaps are found during dev) and return
  // an empty envelope — pages render their empty states instead of crashing.
  console.warn(`[mock] no handler for ${method} ${pathname} — returning empty data`)
  await sleep(LATENCY_MS)
  return { data: undefined }
}
