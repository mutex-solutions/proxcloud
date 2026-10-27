// HTTP client for the ProxCloud API. Every response uses the platform envelope
// { data?, errors?: {code, message}, paging? } — this wrapper unwraps it and
// throws ApiError on errors so callers only ever see `data`.
import { config } from "./config"

export type Paging = { limit: number; offset: number; total: number }
export type Envelope<T> = {
  data?: T
  errors?: { code: number; message: string }
  paging?: Paging
  redirectUrl?: string
}

export class ApiError extends Error {
  status: number
  code: number
  constructor(status: number, code: number, message: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

let tokenProvider: () => string | undefined = () => undefined
export function setTokenProvider(fn: () => string | undefined) {
  tokenProvider = fn
}

export type CloudScope = { serviceId: string; region: string }

export type ReqOpts = {
  method?: string
  body?: unknown
  rawBody?: BodyInit
  form?: Record<string, string>
  cloud?: CloudScope
  headers?: Record<string, string>
  // when true, return the raw Response (downloads)
  raw?: boolean
}

// Mock mode (frontend dev without a backend): src/mocks installs a handler at
// startup when VITE_MOCK=1 and every request short-circuits through it. The
// handler may throw ApiError to simulate failures.
export type MockHandler = (path: string, opts: ReqOpts) => Promise<Envelope<unknown>>
let mockHandler: MockHandler | null = null
export function setMockHandler(fn: MockHandler) {
  mockHandler = fn
}

export async function apiFetch<T = unknown>(path: string, opts: ReqOpts = {}): Promise<T> {
  const res = await apiFetchEnvelope<T>(path, opts)
  return res.data as T
}

export async function apiFetchEnvelope<T = unknown>(path: string, opts: ReqOpts = {}): Promise<Envelope<T>> {
  if (mockHandler) return (await mockHandler(path, opts)) as Envelope<T>
  const headers: Record<string, string> = { ...(opts.headers ?? {}) }
  const token = tokenProvider()
  if (token) headers["Authorization"] = `Bearer ${token}`
  if (opts.cloud) {
    headers["x-service-id"] = opts.cloud.serviceId
    headers["x-region-id"] = opts.cloud.region
  }

  let body: BodyInit | undefined
  if (opts.form) {
    body = new URLSearchParams(opts.form)
  } else if (opts.rawBody !== undefined) {
    body = opts.rawBody
  } else if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json"
    body = JSON.stringify(opts.body)
  }

  const resp = await fetch(`${config.apiUrl}${path}`, {
    method: opts.method ?? (body !== undefined ? "POST" : "GET"),
    headers,
    body,
  })

  if (opts.raw) return { data: resp as unknown as T }

  const text = await resp.text()
  let env: Envelope<T> = {}
  try {
    env = text ? JSON.parse(text) : {}
  } catch {
    // non-envelope payload (raw endpoints like /account/details)
    if (!resp.ok) throw new ApiError(resp.status, resp.status, text || resp.statusText)
    return { data: undefined }
  }
  if (env.errors) throw new ApiError(resp.status, env.errors.code, env.errors.message)
  if (!resp.ok) throw new ApiError(resp.status, resp.status, resp.statusText)
  // raw (non-enveloped) JSON endpoints: treat the whole payload as data
  if (env.data === undefined && !("paging" in env)) return { data: env as unknown as T }
  return env
}

// Raw endpoints that return the object directly (no envelope), e.g. /account/details.
export async function apiFetchRaw<T = unknown>(path: string, opts: ReqOpts = {}): Promise<T> {
  if (mockHandler) return (await mockHandler(path, opts)).data as T
  const headers: Record<string, string> = { ...(opts.headers ?? {}) }
  const token = tokenProvider()
  if (token) headers["Authorization"] = `Bearer ${token}`
  let body: BodyInit | undefined
  if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json"
    body = JSON.stringify(opts.body)
  }
  const resp = await fetch(`${config.apiUrl}${path}`, {
    method: opts.method ?? (body !== undefined ? "POST" : "GET"),
    headers,
    body,
  })
  if (!resp.ok) throw new ApiError(resp.status, resp.status, await resp.text())
  return (await resp.json()) as T
}
