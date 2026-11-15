// Request capture for specs against the mock app. The app calls src/mocks in-process, not over the network, so
// page.route sees nothing: recordCalls replaces the mock handler with one that records every call and passes it on
// to the fixture handlers. A "METHOD /path" key in overrides answers {data: value} instead; a key with the query string
// ("GET /path?a=1") wins over the one without, and a value {apiError: {status, message}} fails the call with that
// ApiError. Install it after the first page load (main.tsx installs the mock then); earlier calls are not recorded.
import type { Page } from "@playwright/test"

export type MockCall = { method: string; path: string; body?: any }

export async function recordCalls(page: Page, overrides: Record<string, unknown> = {}) {
  await page.evaluate(async (ov) => {
    const apiPath = "/src/lib/api.ts", routerPath = "/src/mocks/router.ts"
    const { ApiError, setMockHandler } = await import(/* @vite-ignore */ apiPath)
    const { dispatch } = await import(/* @vite-ignore */ routerPath)
    const w = window as unknown as { __calls: unknown[] }
    w.__calls = []
    setMockHandler((path: string, opts: { method?: string; body?: unknown }) => {
      const method = (opts.method ?? (opts.body !== undefined ? "POST" : "GET")).toUpperCase()
      w.__calls.push({ method, path, body: opts.body })
      const key = `${method} ${path}` in ov ? `${method} ${path}` : `${method} ${path.split("?")[0]}`
      if (!(key in ov)) return dispatch(path, opts)
      const fail = (ov[key] as { apiError?: { status: number; message: string } } | null)?.apiError
      return fail ? Promise.reject(new ApiError(fail.status, fail.status, fail.message)) : Promise.resolve({ data: ov[key] })
    })
  }, overrides)
}

// calls lists the recorded calls of a method whose path (with its query string) matches.
export async function calls(page: Page, method: string, path: string | RegExp): Promise<MockCall[]> {
  const all = await page.evaluate(() => (window as unknown as { __calls: MockCall[] }).__calls)
  return all.filter((c) => c.method === method && (typeof path === "string" ? c.path.split("?")[0] === path : path.test(c.path)))
}

// navigate changes the route inside the app, so recorded overrides stay installed (a page.goto reloads the mock).
export async function navigate(page: Page, url: string) {
  await page.evaluate((u) => {
    history.pushState({}, "", u)
    dispatchEvent(new PopStateEvent("popstate"))
  }, url)
}
