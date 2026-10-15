import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react"

// Row and response types mirror internal/ui/ui.go and internal/store.

export type RequestRow = {
  id: number
  ts: string // RFC 3339
  request_id: string
  upstream_request_id: string | null
  protocol: string
  operation: string
  client_model: string
  route: string | null
  provider: string | null
  upstream_model: string | null
  status: number
  outcome: string
  stream: boolean
  attempts: number
  attempts_ms: number
  ttfb_ms: number | null
  total_ms: number
  routing_json: string
  attempt_log: string
  affinity_key: string | null
  input_tokens: number | null
  cache_read_tokens: number | null
  cache_write_tokens: number | null
  cache_write_1h_tokens: number | null
  output_tokens: number | null
  reasoning_tokens: number | null
  usage_json: string | null
  cost_usd: number | null
  price_key: string | null
  price_json: string | null
  error: string | null
}

export type RequestsPage = { rows: RequestRow[]; next_before: number | null }

export type Target = { provider: string; upstream_model: string }

export type Routing = {
  route: string
  candidates: (Target & { priority: number; weight: number })[]
  order: Target[]
  owner: (Target & { id: string }) | null
  pin: { state: string; source?: string; target: Target | null }
  skipped_open: string[]
}

export type Attempt = Target & { status?: number; error?: string; ms: number; excerpt?: string }

export type Prices = {
  source: string
  tier_size?: number
  input: number
  cache_read: number | null
  cache_write: number
  cache_write_1h: number
  output: number
}

export type Aggregate = {
  key: string
  requests: number
  errors: number
  input_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cache_write_1h_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cost_usd: number
  unpriced: number
  no_usage: number
}

export type UsageView = {
  rows: Aggregate[]
  total: Aggregate
  unpriced_models: { model: string; count: number }[]
  daily: Aggregate[]
}

export type ProviderView = {
  name: string
  type: string
  base_url: string
  breaker: { state: "closed" | "open" | "half_open"; failures: number; open_until: string | null }
  pins: number
  models_age_s: number | null
  committed: number
  failover: number
  error_events: number
}

export type ProvidersView = { providers: ProviderView[]; log_dropped: number; pins_total: number }

export type ConfigView = Record<string, unknown>

// JSON-parse boundary: guards check the shape the views rely on; the server
// is the source of the field types.

export function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v)
}

export const isRequestsPage = (v: unknown): v is RequestsPage => isObject(v) && Array.isArray(v.rows)
export const isRequestRow = (v: unknown): v is RequestRow =>
  isObject(v) && typeof v.id === "number" && typeof v.routing_json === "string"
export const isUsageView = (v: unknown): v is UsageView =>
  isObject(v) && Array.isArray(v.rows) && isObject(v.total) && Array.isArray(v.daily)
export const isProvidersView = (v: unknown): v is ProvidersView =>
  isObject(v) && Array.isArray(v.providers)
export const isConfigView = (v: unknown): v is ConfigView => isObject(v)

// Parsers for the JSON-string columns of a row (routing_json, attempt_log,
// price_json). A malformed column yields null or an empty list.
export function parseRouting(s: string): Routing | null {
  const v: unknown = safeParse(s)
  return isObject(v) && Array.isArray(v.order) && isObject(v.pin) ? (v as Routing) : null
}

export function parseAttempts(s: string): Attempt[] {
  const v: unknown = safeParse(s)
  return Array.isArray(v) ? v.filter((a): a is Attempt => isObject(a) && typeof a.provider === "string") : []
}

export function parsePrices(s: string | null): Prices | null {
  if (s === null) return null
  const v: unknown = safeParse(s)
  return isObject(v) && typeof v.input === "number" ? (v as Prices) : null
}

export function safeParse(s: string): unknown {
  try {
    return JSON.parse(s) as unknown
  } catch {
    return undefined
  }
}

// API key: kept in localStorage, sent as a bearer token. Views subscribe to
// key changes so that saving or forgetting a key refetches everything.

const KEY = "verbatim.key"
let keyVersion = 0
const listeners = new Set<() => void>()

// The key lives in memory; localStorage only carries it across reloads and
// may be unavailable (private mode, blocked storage).
let currentKey: string | null = (() => {
  try {
    return localStorage.getItem(KEY)
  } catch {
    return null
  }
})()

export function getKey(): string | null {
  return currentKey
}

export function setKey(key: string | null) {
  currentKey = key
  try {
    if (key === null) localStorage.removeItem(KEY)
    else localStorage.setItem(KEY, key)
  } catch {
    // persistence is optional
  }
  keyVersion++
  listeners.forEach((l) => l())
}

export function useKeyVersion(): number {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l)
      return () => listeners.delete(l)
    },
    () => keyVersion,
  )
}

export class AuthError extends Error {}

export async function getJSON<T>(path: string, guard: (v: unknown) => v is T, signal?: AbortSignal): Promise<T> {
  const key = getKey()
  const res = await fetch(`/ui/api/${path}`, {
    headers: key ? { Authorization: `Bearer ${key}` } : {},
    signal,
  })
  if (res.status === 401) throw new AuthError("unauthenticated")
  const body: unknown = await res.json().catch(() => undefined)
  if (!res.ok) {
    const msg = isObject(body) && typeof body.error === "string" ? body.error : res.statusText
    throw new Error(`${res.status}: ${msg}`)
  }
  if (!guard(body)) throw new Error(`unexpected response from /ui/api/${path}`)
  return body
}

// useApi fetches path (null: nothing) and refetches every pollMs when given.
// Data from the previous path stays visible while the next one loads.
export function useApi<T>(path: string | null, guard: (v: unknown) => v is T, pollMs?: number) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<Error | null>(null)
  const [loading, setLoading] = useState(false)
  const version = useKeyVersion()
  const guardRef = useRef(guard)
  const [tick, setTick] = useState(0)
  const reload = useCallback(() => setTick((t) => t + 1), [])

  useEffect(() => {
    if (path === null) return
    const ctl = new AbortController()
    let timer: number | undefined
    const run = async (first: boolean) => {
      if (first) setLoading(true)
      try {
        setData(await getJSON(path, guardRef.current, ctl.signal))
        setError(null)
      } catch (e) {
        if (ctl.signal.aborted) return
        setError(e instanceof Error ? e : new Error(String(e)))
      } finally {
        if (!ctl.signal.aborted) setLoading(false)
      }
      if (pollMs && !ctl.signal.aborted) timer = window.setTimeout(() => void run(false), pollMs)
    }
    void run(true)
    return () => {
      ctl.abort()
      window.clearTimeout(timer)
    }
  }, [path, pollMs, version, tick])

  return { data, error, loading, reload }
}
