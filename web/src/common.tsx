import { useId, useState, type ReactNode } from "react"
import { CheckIcon, CopyIcon } from "lucide-react"
import { AuthError, getKey, setKey, type RequestRow } from "@/api"
import { RANGES, type RangeId, type TimeRange } from "@/format"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { cn } from "@/lib/utils"

// Small pieces shared by the views: state marks, null dashes, the key
// prompt, and the filter controls.

export type Tone = "ok" | "warn" | "err" | "info" | "none"

const toneBg: Record<Tone, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  err: "bg-err",
  info: "bg-info",
  none: "bg-faint",
}

// Mark is the one place color carries meaning: a small square before a label.
export function Mark({ tone, children, className }: { tone: Tone; children?: ReactNode; className?: string }) {
  return (
    <span className={cn("inline-flex items-baseline gap-1.5 whitespace-nowrap", className)}>
      <span aria-hidden className={cn("size-2 shrink-0 self-center rounded-[2px]", toneBg[tone])} />
      {children}
    </span>
  )
}

export function Dash() {
  return <span className="text-faint">–</span>
}

// requestState classifies a row for its status mark: an error status or a
// non-completed outcome is an error; a success after failed attempts is a
// failover; a client that left is neutral.
export function requestState(r: RequestRow): { tone: Tone; note: string | null } {
  if (r.outcome === "client_cancelled") return { tone: "none", note: "client left" }
  const note: Record<string, string> = {
    upstream_error_event: "error event",
    truncated: "truncated",
    gateway_error: "gateway error",
  }
  if (r.status >= 400 || r.outcome !== "completed") return { tone: "err", note: note[r.outcome] ?? null }
  if (r.attempts > 1) return { tone: "warn", note: "failover" }
  return { tone: "ok", note: null }
}

export const pinTone = (state: string): Tone => (state === "moved" ? "info" : "none")

export function ErrorLine({ error, onRetry }: { error: Error; onRetry?: () => void }) {
  if (error instanceof AuthError) return <KeyPrompt />
  return (
    <div className="flex items-center gap-3 py-6 text-err-ink">
      <Mark tone="err">Could not load: {error.message}</Mark>
      {onRetry && (
        <Button variant="outline" size="sm" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  )
}

// KeyPrompt asks for server.api_key in place, stores it, and every view
// refetches through the key version.
export function KeyPrompt() {
  const id = useId()
  const [value, setValue] = useState("")
  const rejected = getKey() !== null
  return (
    <form
      className="flex max-w-xl flex-wrap items-center gap-x-3 gap-y-2 py-6"
      onSubmit={(e) => {
        e.preventDefault()
        if (value.trim()) setKey(value.trim())
      }}
    >
      <label htmlFor={id} className="basis-full text-muted-foreground">
        {rejected
          ? "The stored API key was rejected. Enter server.api_key again."
          : "This gateway requires its API key (server.api_key) to show data."}
      </label>
      <Input
        id={id}
        type="password"
        autoComplete="current-password"
        autoFocus
        className="w-72"
        placeholder="API key"
        value={value}
        onChange={(e) => setValue(e.target.value)}
      />
      <Button type="submit" disabled={!value.trim()}>
        Use key
      </Button>
    </form>
  )
}

export function CopyButton({ text, label }: { text: string; label: string }) {
  const [done, setDone] = useState(false)
  return (
    <Button
      variant="ghost"
      size="icon-sm"
      aria-label={label}
      title={label}
      onClick={() => {
        void navigator.clipboard?.writeText(text).then(() => {
          setDone(true)
          window.setTimeout(() => setDone(false), 1400)
        })
      }}
    >
      {done ? <CheckIcon className="text-ok-ink" /> : <CopyIcon />}
    </Button>
  )
}

// FilterSelect is a labeled select whose first option means "no filter".
export function FilterSelect({
  label,
  value,
  onChange,
  options,
  anyLabel = "Any",
  className,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  options: readonly (string | { value: string; label: string })[]
  anyLabel?: string
  className?: string
}) {
  const active = value !== ""
  return (
    <Select value={value || "__any"} onValueChange={(v) => onChange(v === "__any" ? "" : v)}>
      <SelectTrigger aria-label={label} className={cn("gap-1.5", active && "border-ring/60", className)}>
        <span className="text-muted-foreground">{label}</span>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="__any">{anyLabel}</SelectItem>
        {options.map((o) => {
          const { value: v, label: l } = typeof o === "string" ? { value: o, label: o } : o
          return (
            <SelectItem key={v} value={v}>
              {l}
            </SelectItem>
          )
        })}
      </SelectContent>
    </Select>
  )
}

export function RangePicker({
  value,
  onChange,
  presets = RANGES.map((r) => r.id),
}: {
  value: TimeRange
  onChange: (r: TimeRange) => void
  presets?: readonly RangeId[]
}) {
  return (
    <>
      <Select value={value.id} onValueChange={(id) => onChange({ ...value, id: id as RangeId })}>
        <SelectTrigger aria-label="Time range" className="gap-1.5">
          <span className="text-muted-foreground">Range</span>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {RANGES.filter((r) => presets.includes(r.id)).map((r) => (
            <SelectItem key={r.id} value={r.id}>
              {r.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {value.id === "custom" && (
        <span className="inline-flex items-center gap-1.5">
          <Input
            type="datetime-local"
            aria-label="From"
            className="num w-auto"
            value={value.from}
            onChange={(e) => onChange({ ...value, from: e.target.value })}
          />
          <span className="text-muted-foreground">to</span>
          <Input
            type="datetime-local"
            aria-label="To"
            className="num w-auto"
            value={value.to}
            onChange={(e) => onChange({ ...value, to: e.target.value })}
          />
        </span>
      )}
    </>
  )
}

export function ToggleButton({
  pressed,
  onChange,
  children,
}: {
  pressed: boolean
  onChange: (v: boolean) => void
  children: ReactNode
}) {
  return (
    <Button
      variant="outline"
      aria-pressed={pressed}
      onClick={() => onChange(!pressed)}
      className="aria-pressed:border-foreground/40 aria-pressed:bg-muted"
    >
      {children}
    </Button>
  )
}

export function SectionTitle({ children, aside }: { children: ReactNode; aside?: ReactNode }) {
  return (
    <div className="mb-2.5 flex items-baseline justify-between gap-4">
      <h2 className="text-[13px] font-semibold tracking-[-0.003em]">{children}</h2>
      {aside && <div className="text-muted-foreground">{aside}</div>}
    </div>
  )
}
