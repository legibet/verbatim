import { useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import uPlot from "uplot"
import "uplot/dist/uPlot.min.css"
import { isConfigView, isObject, isPricingSync, isUsageView, postJSON, useApi, type Aggregate, type PricingSync, type UsageView } from "@/api"
import { Dash, ErrorLine, FilterSelect, Mark, RangePicker, SectionTitle } from "@/common"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableFooter, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { fmtInt, fmtUSD, query, rangeParams, type TimeRange } from "@/format"
import { cn } from "@/lib/utils"

type GroupBy = "model" | "provider" | "route" | "day"
const GROUPS: { id: GroupBy; label: string }[] = [
  { id: "model", label: "Model" },
  { id: "provider", label: "Provider" },
  { id: "route", label: "Route" },
  { id: "day", label: "Day" },
]

export function Usage() {
  const [range, setRange] = useState<TimeRange>({ id: "7d", from: "", to: "" })
  const [by, setBy] = useState<GroupBy>("model")
  const [protocol, setProtocol] = useState("")
  const [provider, setProvider] = useState("")
  const params = useMemo(() => rangeParams(range), [range])
  const { data, error, loading, reload } = useApi(`usage${query({ ...params, by, protocol, provider })}`, isUsageView)
  const config = useApi("config", isConfigView)
  const providerNames = config.data && isObject(config.data.providers) ? Object.keys(config.data.providers).sort() : []

  return (
    <div className="space-y-6 pt-4 pb-10">
      <div className="flex flex-wrap items-center gap-2">
        <RangePicker value={range} onChange={setRange} presets={["24h", "7d", "30d", "all", "custom"]} />
        <FilterSelect
          label="Protocol"
          value={protocol}
          onChange={setProtocol}
          options={["chat", "responses", "anthropic", "gemini", "images"]}
        />
        <FilterSelect label="Provider" value={provider} onChange={setProvider} options={providerNames} />
        <div className="ml-auto flex items-center gap-2">
          <span className="text-muted-foreground" id="group-by-label">
            Group by
          </span>
          <div role="group" aria-labelledby="group-by-label" className="inline-flex h-8 items-center rounded-sm border border-input bg-surface p-0.5">
            {GROUPS.map((g) => (
              <button
                key={g.id}
                type="button"
                aria-pressed={by === g.id}
                onClick={() => setBy(g.id)}
                className="inline-flex h-full cursor-pointer items-center rounded-xs px-2.5 text-[12.5px] whitespace-nowrap text-muted-foreground outline-none transition-[scale] duration-150 active:scale-[0.97] focus-visible:ring-2 focus-visible:ring-ring/60 aria-pressed:bg-background aria-pressed:font-medium aria-pressed:text-foreground aria-pressed:shadow-[0_0_0_1px_var(--border),0_1px_2px_rgb(0_0_0/0.06)] [@media(hover:hover)]:hover:text-foreground"
              >
                {g.label}
              </button>
            ))}
          </div>
        </div>
      </div>

      {error ? (
        <ErrorLine error={error} onRetry={reload} />
      ) : !data ? (
        <Skeleton className={cn("h-20 w-full", !loading && "hidden")} />
      ) : (
        <>
          <Totals t={data.total} />

          <div className="grid gap-6 md:grid-cols-2">
            <DayChart
              title="Cost per day"
              unit="USD, UTC days"
              days={data.daily}
              range={params}
              value={costOf}
              fmt={fmtCost}
            />
            <DayChart
              title="Tokens per day"
              unit="all kinds, UTC days"
              days={data.daily}
              range={params}
              value={tokensOf}
              fmt={fmtInt}
            />
          </div>

          <section>
            <SectionTitle aside={`${data.rows.length} ${data.rows.length === 1 ? "group" : "groups"}`}>
              By {by}
            </SectionTitle>
            <Breakdown rows={data.rows} total={data.total} by={by} />
          </section>

          <Unpriced models={data.unpriced_models} onSynced={reload} />
        </>
      )}
    </div>
  )
}

// Unpriced lists the client model names that still have no price and offers
// a price sync: the server downloads pricing.sources as `verbatim pricing
// sync` does. The new prices apply to new requests; logged requests keep
// their cost.
function Unpriced({ models, onSynced }: { models: UsageView["unpriced_models"]; onSynced: () => void }) {
  const [syncing, setSyncing] = useState(false)
  const [result, setResult] = useState<PricingSync | Error | null>(null)
  const sync = async () => {
    setSyncing(true)
    try {
      setResult(await postJSON("pricing/sync", isPricingSync))
      onSynced()
    } catch (e) {
      setResult(e instanceof Error ? e : new Error(String(e)))
    } finally {
      setSyncing(false)
    }
  }

  return (
    <section>
      <SectionTitle
        aside={
          <Button variant="outline" size="sm" disabled={syncing} onClick={() => void sync()}>
            {syncing ? "Syncing prices" : "Sync prices"}
          </Button>
        }
      >
        Unpriced models
      </SectionTitle>
      {result && (
        <div role="status" className="mb-3 space-y-1">
          {result instanceof Error ? (
            <Mark tone="err" className="whitespace-normal text-err-ink">
              Sync failed: {result.message}
            </Mark>
          ) : (
            <>
              <p>
                <Mark tone="ok">{fmtInt(result.models)} models in the price table.</Mark>{" "}
                <span className="text-muted-foreground">New requests are priced with it; logged requests keep their cost.</span>
              </p>
              {result.warnings.length > 0 && (
                <details>
                  <summary className="w-fit cursor-pointer text-muted-foreground select-none">
                    {fmtInt(result.warnings.length)} {result.warnings.length === 1 ? "warning" : "warnings"}
                  </summary>
                  <ul className="mt-1 max-w-[100ch] space-y-0.5 font-mono text-[12px] text-muted-foreground">
                    {result.warnings.map((w, i) => (
                      <li key={i}>{w}</li>
                    ))}
                  </ul>
                </details>
              )}
            </>
          )}
        </div>
      )}
      {models.length === 0 ? (
        <p className="text-muted-foreground">Every model with usage in this range has a price.</p>
      ) : (
        <div className="space-y-2">
          <p className="max-w-[72ch] text-muted-foreground">
            These client model names had usage in this range and still have no price. Sync prices if a source has added one, map the
            name to a priced name in <code className="font-mono text-foreground">pricing.aliases</code>, or price it in{" "}
            <code className="font-mono text-foreground">pricing.overrides</code>.
          </p>
          <ul className="w-fit min-w-80 divide-y divide-rule rounded-lg border">
            {models.map((m) => (
              <li key={m.model} className="flex items-baseline justify-between gap-8 px-3.5 py-2">
                <span className="font-medium">{m.model || "(no model)"}</span>
                <span className="num text-muted-foreground">
                  {fmtInt(m.count)} {m.count === 1 ? "request" : "requests"}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

// Chart accessors live at module scope so the chart effect sees stable functions.
const costOf = (a: Aggregate) => a.cost_usd
const tokensOf = (a: Aggregate) => a.input_tokens + a.cache_read_tokens + a.cache_write_tokens + a.output_tokens
const fmtCost = (v: number) => "$" + fmtUSD(v)

function Totals({ t }: { t: Aggregate }) {
  const cells: { label: string; value: ReactNode; tone?: string }[] = [
    { label: "Requests", value: fmtInt(t.requests) },
    { label: "Errors", value: fmtInt(t.errors), tone: t.errors > 0 ? "text-err-ink" : undefined },
    { label: "Input", value: fmtInt(t.input_tokens) },
    { label: "Cache read", value: fmtInt(t.cache_read_tokens) },
    { label: "Cache write", value: fmtInt(t.cache_write_tokens) },
    { label: "Output", value: fmtInt(t.output_tokens) },
    { label: "Reasoning", value: fmtInt(t.reasoning_tokens) },
    { label: "Cost", value: `$${fmtUSD(t.cost_usd)}` },
  ]
  return (
    <div>
      <div className="grid grid-cols-4 overflow-hidden rounded-lg border lg:grid-cols-8">
        {cells.map((c, i) => (
          <div
            key={c.label}
            className={cn(
              "border-rule px-4 py-3",
              i % 4 !== 0 && "border-l",
              i >= 4 && "border-t lg:border-t-0",
              i === 4 && "lg:border-l",
            )}
          >
            <div className="text-[11.5px] text-muted-foreground">{c.label}</div>
            <div className={cn("num mt-1 text-[18px] leading-tight tracking-[-0.01em]", c.tone)}>{c.value}</div>
          </div>
        ))}
      </div>
      <p className="mt-2 text-muted-foreground">
        <Coverage a={t} /> Cost covers priced requests only; count_tokens and search requests are excluded.
      </p>
    </div>
  )
}

function Coverage({ a }: { a: Aggregate }) {
  return (
    <span className={cn(a.unpriced > 0 && "text-warn-ink")}>
      {fmtInt(a.unpriced)} unpriced, {fmtInt(a.no_usage)} without usage.
    </span>
  )
}

function Breakdown({ rows, total, by }: { rows: Aggregate[]; total: Aggregate; by: GroupBy }) {
  const cols: { label: string; get: (a: Aggregate) => ReactNode }[] = [
    { label: "Requests", get: (a) => fmtInt(a.requests) },
    { label: "Errors", get: (a) => (a.errors ? <span className="text-err-ink">{fmtInt(a.errors)}</span> : <Dash />) },
    { label: "Input", get: (a) => fmtInt(a.input_tokens) },
    { label: "Cache read", get: (a) => fmtInt(a.cache_read_tokens) },
    { label: "Cache write", get: (a) => fmtInt(a.cache_write_tokens) },
    { label: "Output", get: (a) => fmtInt(a.output_tokens) },
    { label: "Reasoning", get: (a) => fmtInt(a.reasoning_tokens) },
    { label: "Cost USD", get: (a) => (a.requests - a.unpriced - a.no_usage > 0 ? fmtUSD(a.cost_usd) : <Dash />) },
    { label: "Unpriced", get: (a) => (a.unpriced ? <span className="text-warn-ink">{fmtInt(a.unpriced)}</span> : <Dash />) },
    { label: "No usage", get: (a) => (a.no_usage ? fmtInt(a.no_usage) : <Dash />) },
  ]
  if (rows.length === 0) return <p className="text-muted-foreground">No requests in this range.</p>
  return (
    <div className="overflow-x-auto rounded-lg border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-full">{by[0].toUpperCase() + by.slice(1)}</TableHead>
            {cols.map((c) => (
              <TableHead key={c.label} className="text-right">
                {c.label}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((a) => (
            <TableRow key={a.key}>
              <TableCell className={cn("font-medium", by === "day" && "num font-normal")}>
                {a.key || <span className="font-normal text-muted-foreground">(none)</span>}
              </TableCell>
              {cols.map((c) => (
                <TableCell key={c.label} className="num text-right">
                  {c.get(a)}
                </TableCell>
              ))}
            </TableRow>
          ))}
        </TableBody>
        {rows.length > 1 && (
          <TableFooter>
            <TableRow>
              <TableCell>Total</TableCell>
              {cols.map((c) => (
                <TableCell key={c.label} className="num text-right">
                  {c.get(total)}
                </TableCell>
              ))}
            </TableRow>
          </TableFooter>
        )}
      </Table>
    </div>
  )
}

// dayRange lists every UTC day from the range start (or the first day with
// data) to today, so quiet days show as gaps rather than disappearing.
function dayRange(daily: Aggregate[], range: Record<string, string>): string[] {
  const day = (ms: number) => new Date(ms).toISOString().slice(0, 10)
  const keys = daily.map((d) => d.key).sort()
  const start = range.since ? day(Number(range.since)) : (keys[0] ?? day(Date.now()))
  const end = range.until ? day(Number(range.until) - 1) : day(Date.now())
  const out: string[] = []
  for (let t = Date.parse(start); day(t) <= end; t += 86_400_000) out.push(day(t))
  return out
}

function useDark(): boolean {
  const mq = useMemo(() => window.matchMedia("(prefers-color-scheme: dark)"), [])
  const [dark, setDark] = useState(mq.matches)
  useEffect(() => {
    const on = () => setDark(mq.matches)
    mq.addEventListener("change", on)
    return () => mq.removeEventListener("change", on)
  }, [mq])
  return dark
}

function DayChart({
  title,
  unit,
  days,
  range,
  value,
  fmt,
}: {
  title: string
  unit: string
  days: Aggregate[]
  range: Record<string, string>
  value: (a: Aggregate) => number
  fmt: (v: number) => string
}) {
  const el = useRef<HTMLDivElement>(null)
  const dark = useDark()
  const [hover, setHover] = useState<number | null>(null)

  const keys = useMemo(() => dayRange(days, range), [days, range])
  const values = useMemo(() => {
    const byKey = new Map(days.map((d) => [d.key, value(d)]))
    return keys.map((k) => byKey.get(k) ?? 0)
  }, [keys, days, value])
  const total = values.reduce((s, v) => s + v, 0)

  useEffect(() => {
    const node = el.current
    if (!node) return
    const css = getComputedStyle(document.documentElement)
    const v = (name: string) => css.getPropertyValue(name).trim()
    const axis = {
      stroke: v("--muted-foreground"),
      grid: { stroke: v("--rule"), width: 1 },
      ticks: { show: false },
      font: `11px ${v("--font-mono") || "monospace"}`,
    }
    const xs = keys.map((k) => Date.parse(k) / 1000)
    const plot = new uPlot(
      {
        width: node.clientWidth,
        height: 160,
        padding: [8, 4, 0, 0],
        legend: { show: false },
        // Bars are UTC days, so ticks must be placed in UTC too.
        tzDate: (ts) => uPlot.tzDate(new Date(ts * 1000), "Etc/UTC"),
        cursor: { y: false, points: { show: false }, drag: { x: false, y: false } },
        scales: { x: { time: true, range: (_u, min, max) => [min - 43_200, max + 43_200] }, y: { range: (_u, _min, max) => [0, max > 0 ? max * 1.1 : 1] } },
        axes: [
          { ...axis, size: 30, grid: { show: false }, values: (_u, ticks) => ticks.map((t) => new Date(t * 1000).toISOString().slice(5, 10)) },
          { ...axis, size: 64, values: (_u, ticks) => ticks.map((t) => fmt(t)) },
        ],
        series: [
          {},
          {
            fill: v("--faint"),
            stroke: v("--faint"),
            width: 0,
            paths: uPlot.paths.bars!({ size: [0.7, 28], gap: 1 }),
            points: { show: false },
          },
        ],
        hooks: { setCursor: [(u) => setHover(u.cursor.idx ?? null)] },
      },
      [xs, values],
      node,
    )
    const ro = new ResizeObserver(() => plot.setSize({ width: node.clientWidth, height: 160 }))
    ro.observe(node)
    return () => {
      ro.disconnect()
      plot.destroy()
    }
  }, [keys, values, fmt, dark])

  return (
    <section className="rounded-lg border px-4 pt-3 pb-2">
      <div className="mb-1 flex items-baseline justify-between gap-4">
        <h2 className="text-[13px] font-semibold">
          {title} <span className="font-normal text-muted-foreground">{unit}</span>
        </h2>
        <span className="num whitespace-nowrap text-muted-foreground">
          {hover !== null && keys[hover] ? (
            <>
              {keys[hover]} <span className="text-foreground">{fmt(values[hover])}</span>
            </>
          ) : (
            <>
              total <span className="text-foreground">{fmt(total)}</span>
            </>
          )}
        </span>
      </div>
      <div ref={el} className="w-full" />
    </section>
  )
}
