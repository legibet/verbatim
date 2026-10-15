// Number, duration, and time formatting shared by the views.

const intFmt = new Intl.NumberFormat("en-US")

export function fmtInt(n: number): string {
  return intFmt.format(n)
}

// fmtUSD keeps four decimals below one dollar so that per-request costs stay
// aligned and distinguishable; larger amounts use cents.
export function fmtUSD(n: number): string {
  if (n === 0) return "0.0000"
  if (Math.abs(n) < 0.0001) return "<0.0001"
  return Math.abs(n) < 1 ? n.toFixed(4) : n.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 })
}

// fmtPrice formats a per-1M-token unit price.
export function fmtPrice(n: number): string {
  return n.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 4 })
}

// fmtDuration is for prose and detail panes; table columns show plain ms.
export function fmtDuration(ms: number): string {
  if (ms < 1000) return `${ms} ms`
  // Thresholds sit where rounding would carry into the next unit (9.995 s, 59.95 s).
  if (ms < 9_995) return `${(ms / 1000).toFixed(2)} s`
  if (ms < 59_950) return `${(ms / 1000).toFixed(1)} s`
  const total = Math.round(ms / 1000)
  const m = Math.floor(total / 60)
  const s = total % 60
  return `${m} m ${String(s).padStart(2, "0")} s`
}

export function fmtAge(seconds: number): string {
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`
  return `${Math.floor(seconds / 86_400)}d ${Math.floor((seconds % 86_400) / 3600)}h`
}

const pad = (n: number, w = 2) => String(n).padStart(w, "0")

export function fmtClock(ts: string | Date): string {
  const d = new Date(ts)
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

export function fmtDate(ts: string | Date): string {
  const d = new Date(ts)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

// fmtDateTime is the full local timestamp with milliseconds and UTC offset.
export function fmtDateTime(ts: string | Date): string {
  const d = new Date(ts)
  const off = -d.getTimezoneOffset()
  const sign = off >= 0 ? "+" : "-"
  const zone = `UTC${sign}${pad(Math.floor(Math.abs(off) / 60))}:${pad(Math.abs(off) % 60)}`
  return `${fmtDate(d)} ${fmtClock(d)}.${pad(d.getMilliseconds(), 3)} ${zone}`
}

// Time range presets shared by Requests and Usage. "custom" uses explicit
// local datetime bounds.
export const RANGES = [
  { id: "15m", label: "15 min", ms: 15 * 60_000 },
  { id: "1h", label: "1 hour", ms: 3_600_000 },
  { id: "24h", label: "24 hours", ms: 86_400_000 },
  { id: "7d", label: "7 days", ms: 7 * 86_400_000 },
  { id: "30d", label: "30 days", ms: 30 * 86_400_000 },
  { id: "all", label: "All time", ms: 0 },
  { id: "custom", label: "Custom", ms: 0 },
] as const

export type RangeId = (typeof RANGES)[number]["id"]

export type TimeRange = { id: RangeId; from: string; to: string } // from/to: datetime-local values

// rangeParams turns a range into since/until query parameters (unix ms),
// evaluated at call time.
export function rangeParams(r: TimeRange, now = Date.now()): Record<string, string> {
  if (r.id === "custom") {
    const p: Record<string, string> = {}
    if (r.from) p.since = String(new Date(r.from).getTime())
    if (r.to) p.until = String(new Date(r.to).getTime())
    return p
  }
  const preset = RANGES.find((x) => x.id === r.id)
  return preset && preset.ms > 0 ? { since: String(now - preset.ms) } : {}
}

export function query(params: Record<string, string | undefined>): string {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v) q.set(k, v)
  const s = q.toString()
  return s ? `?${s}` : ""
}
