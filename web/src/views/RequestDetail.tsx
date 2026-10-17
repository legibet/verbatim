import type { ReactNode } from "react"
import {
  isRequestRow,
  parseAttempts,
  parsePrices,
  parseRouting,
  safeParse,
  useApi,
  type Attempt,
  type Prices,
  type RequestRow,
  type Routing,
} from "@/api"
import { CopyButton, Dash, ErrorLine, Mark, pinTone, requestState } from "@/common"
import { Badge } from "@/components/ui/badge"
import { SheetTitle } from "@/components/ui/sheet"
import { Skeleton } from "@/components/ui/skeleton"
import { fmtDateTime, fmtDuration, fmtInt, fmtPrice } from "@/format"
import { cn } from "@/lib/utils"

export function RequestDetail({ id }: { id: number }) {
  const { data: r, error, reload } = useApi(`requests/${id}`, isRequestRow)
  if (error)
    return (
      <div className="px-6 pt-14">
        <SheetTitle className="sr-only">Request {id}</SheetTitle>
        <ErrorLine error={error} onRetry={reload} />
      </div>
    )
  if (!r || r.id !== id)
    return (
      <div className="space-y-3 px-6 pt-6">
        <SheetTitle className="sr-only">Request {id}</SheetTitle>
        <Skeleton className="h-4 w-40" />
        <Skeleton className="h-6 w-72" />
        <Skeleton className="h-4 w-96" />
      </div>
    )

  const state = requestState(r)
  const routing = parseRouting(r.routing_json)
  const attempts = parseAttempts(r.attempt_log)

  return (
    <div className="flex h-full flex-col">
      <header className="shrink-0 border-b px-6 pt-5 pb-4">
        <div className="flex items-center gap-3 pr-10 text-muted-foreground">
          <Mark tone={state.tone} className="text-foreground">
            <span className="num font-medium">{r.status}</span>
            <span className="text-muted-foreground">
              {r.outcome.replaceAll("_", " ")}
              {r.attempts > 1 && `, after ${r.attempts} attempts`}
            </span>
          </Mark>
          <span>
            {r.protocol} {r.operation}
            {r.stream && " · stream"}
          </span>
        </div>
        <SheetTitle className="mt-2 text-[17px] leading-snug font-semibold tracking-[-0.012em]">
          {r.client_model || "(no model)"}
          {r.provider && (
            <span className="font-normal text-muted-foreground">
              {" "}
              → {r.provider}
              {r.upstream_model && r.upstream_model !== r.client_model && ` / ${r.upstream_model}`}
            </span>
          )}
        </SheetTitle>
        <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-muted-foreground">
          <span className="inline-flex items-center gap-0.5">
            <span className="num text-foreground">{r.request_id}</span>
            <CopyButton text={r.request_id} label="Copy request ID" />
          </span>
          <span className="num">{fmtDateTime(r.ts)}</span>
        </div>
      </header>

      <div className="min-h-0 flex-1 space-y-7 overflow-y-auto overscroll-contain px-6 pt-5 pb-10">
        <dl className="grid grid-cols-[repeat(auto-fill,minmax(140px,1fr))] gap-x-6 gap-y-3">
          <Fact label="Upstream request ID">{r.upstream_request_id ?? <Dash />}</Fact>
          <Fact label="Route">{r.route ?? <Dash />}</Fact>
          <Fact label="Attempts">
            <span className={cn(r.attempts > 1 && "text-warn-ink")}>{r.attempts}</span>
          </Fact>
          <Fact label="In failed attempts">{r.attempts_ms > 0 ? fmtDuration(r.attempts_ms) : <Dash />}</Fact>
          <Fact label="Time to first byte">{r.ttfb_ms === null ? <Dash /> : fmtDuration(r.ttfb_ms)}</Fact>
          <Fact label="Total">{fmtDuration(r.total_ms)}</Fact>
          <Fact label="Affinity key">
            {r.affinity_key ? <span title={r.affinity_key}>{r.affinity_key.slice(0, 12)}</span> : <Dash />}
          </Fact>
        </dl>

        {r.error && (
          <Section title="Error">
            <Pre className="text-err-ink">{r.error}</Pre>
          </Section>
        )}

        <Section title="Routing">
          {routing ? <RoutingView r={r} routing={routing} attempts={attempts} /> : <Muted>No routing recorded.</Muted>}
        </Section>

        <Section title="Attempts">
          {attempts.length === 0 ? (
            <Muted>No upstream attempt was made.</Muted>
          ) : (
            <ol className="divide-y divide-rule rounded-lg border">
              {attempts.map((a, i) => (
                <li key={i} className="px-3.5 py-2.5">
                  <div className="flex items-baseline gap-3">
                    <span className="num w-4 shrink-0 text-faint">{i + 1}</span>
                    <span className="min-w-0 flex-1">
                      {a.provider}
                      <span className="text-muted-foreground"> / {a.upstream_model}</span>
                    </span>
                    <AttemptResult a={a} />
                    <span className="num w-16 shrink-0 text-right text-muted-foreground">{fmtInt(a.ms)} ms</span>
                  </div>
                  {a.error && <Pre className="mt-2 ml-7 text-err-ink">{a.error}</Pre>}
                  {a.excerpt && <Pre className="mt-2 ml-7">{a.excerpt}</Pre>}
                </li>
              ))}
            </ol>
          )}
        </Section>

        <Section title="Usage" aside="as reported by the provider">
          {r.usage_json ? <Pre>{JSON.stringify(safeParse(r.usage_json) ?? r.usage_json, null, 2)}</Pre> : <Muted>No usage was recorded.</Muted>}
        </Section>

        <Section title="Cost" aside="configured list prices, per 1M tokens">
          <CostView r={r} prices={parsePrices(r.price_json)} />
        </Section>
      </div>
    </div>
  )
}

function Section({ title, aside, children }: { title: string; aside?: string; children: ReactNode }) {
  return (
    <section>
      <h3 className="mb-2.5 flex items-baseline gap-2 text-[13px] font-semibold">
        {title}
        {aside && <span className="text-[12px] font-normal text-muted-foreground">{aside}</span>}
      </h3>
      {children}
    </section>
  )
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-[11.5px] text-muted-foreground">{label}</dt>
      <dd className="num mt-0.5 truncate">{children}</dd>
    </div>
  )
}

function Muted({ children }: { children: ReactNode }) {
  return <p className="text-muted-foreground">{children}</p>
}

function Pre({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <pre
      className={cn(
        "max-h-72 overflow-auto rounded-sm bg-surface px-3 py-2.5 font-mono text-[12px] leading-relaxed break-all whitespace-pre-wrap",
        className,
      )}
    >
      {children}
    </pre>
  )
}

function AttemptResult({ a }: { a: Attempt }) {
  if (a.error) return <Mark tone="err">no response</Mark>
  if (a.status === undefined) return <Dash />
  return (
    <Mark tone={a.status < 400 ? "ok" : "err"}>
      <span className="num">{a.status}</span>
    </Mark>
  )
}

const same = (a: { provider: string; upstream_model: string }, b: { provider: string; upstream_model: string }) =>
  a.provider === b.provider && a.upstream_model === b.upstream_model

function RoutingView({ r, routing, attempts }: { r: RequestRow; routing: Routing; attempts: Attempt[] }) {
  // The last attempt's response is what the client got, if its headers were
  // committed (ttfb_ms set) and verbatim did not answer itself.
  const last = attempts.at(-1)
  const committed =
    last && last.status !== undefined && r.ttfb_ms !== null && r.outcome !== "gateway_error" ? last : null

  type Line = { provider: string; upstream_model: string; pos: number | null; priority?: number; weight?: number }
  const lines: Line[] = routing.order.map((t, i) => {
    const c = routing.candidates.find((c) => same(c, t))
    return { ...t, pos: i + 1, priority: c?.priority, weight: c?.weight }
  })
  for (const c of routing.candidates) if (!lines.some((l) => same(l, c))) lines.push({ ...c, pos: null })

  const result = (l: Line): ReactNode => {
    if (committed && same(committed, l))
      return <Mark tone={committed.status! < 400 ? "ok" : "err"}>committed {committed.status}</Mark>
    const tried = attempts.filter((a) => same(a, l))
    if (tried.length > 0) {
      const a = tried[tried.length - 1]
      return <Mark tone="warn">failed {a.error ? "(no response)" : a.status}</Mark>
    }
    if (routing.skipped_open.includes(l.provider)) return <Mark tone="err">skipped, breaker open</Mark>
    return <span className="text-faint">not tried</span>
  }

  const pin = routing.pin
  return (
    <div className="space-y-3">
      <dl className="grid grid-cols-[auto_1fr] gap-x-5 gap-y-1.5">
        <dt className="text-muted-foreground">Route</dt>
        <dd className="num">{routing.route || <Dash />}</dd>
        <dt className="text-muted-foreground">Pin</dt>
        <dd>
          {pin.state === "none" ? (
            <span className="text-muted-foreground">none</span>
          ) : (
            <span className="inline-flex flex-wrap items-center gap-x-3">
              <Mark tone={pinTone(pin.state)}>{pin.state}</Mark>
              {pin.target && (
                <span>
                  {pin.state === "moved" && <span className="text-muted-foreground">from </span>}
                  {pin.target.provider}
                  <span className="text-muted-foreground"> / {pin.target.upstream_model}</span>
                </span>
              )}
              {pin.source && <span className="num text-muted-foreground">key from {pin.source}</span>}
            </span>
          )}
        </dd>
        {routing.owner && (
          <>
            <dt className="text-muted-foreground">Owner</dt>
            <dd>
              <span className="num">{routing.owner.id}</span>
              <span className="text-muted-foreground"> on </span>
              {routing.owner.provider}
              <span className="text-muted-foreground"> / {routing.owner.upstream_model}</span>
            </dd>
          </>
        )}
        {routing.skipped_open.length > 0 && (
          <>
            <dt className="text-muted-foreground">Breaker open</dt>
            <dd>{routing.skipped_open.join(", ")}</dd>
          </>
        )}
      </dl>
      {lines.length > 0 && (
        <table className="w-full border-separate border-spacing-0 overflow-hidden rounded-lg border text-[12.5px] [&_td]:h-8 [&_td]:border-t [&_td]:border-rule [&_td]:px-3 [&_th]:h-7 [&_th]:bg-surface [&_th]:px-3 [&_th]:text-left [&_th]:text-[11.5px] [&_th]:font-medium [&_th]:text-muted-foreground">
          <thead>
            <tr>
              <th className="w-8">#</th>
              <th>Provider / upstream model</th>
              <th className="text-right!">Priority</th>
              <th className="text-right!">Weight</th>
              <th>Result</th>
            </tr>
          </thead>
          <tbody>
            {lines.map((l) => (
              <tr key={`${l.provider}/${l.upstream_model}`}>
                <td className="num text-faint">{l.pos ?? "–"}</td>
                <td>
                  {l.provider}
                  <span className="text-muted-foreground"> / {l.upstream_model}</span>
                </td>
                <td className="num text-right">{l.priority ?? <Dash />}</td>
                <td className="num text-right">{l.weight ?? <Dash />}</td>
                <td>{result(l)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

const usd = (n: number) => `$${n.toFixed(6)}`

function CostView({ r, prices }: { r: RequestRow; prices: Prices | null }) {
  if (r.operation !== "create") return <Muted>{r.operation} requests are not priced.</Muted>
  if (r.cost_usd === null || prices === null) {
    if (r.usage_json === null) return <Muted>No usage was recorded, so there is no cost.</Muted>
    if (r.input_tokens === null || r.output_tokens === null)
      return <Muted>The usage is incomplete (no input or output count), so nothing was priced.</Muted>
    const names = [r.client_model, r.upstream_model].filter((n, i, a): n is string => !!n && a.indexOf(n) === i)
    return (
      <div className="space-y-1.5">
        <Mark tone="none" className="font-medium">
          No price
        </Mark>
        <p className="max-w-[60ch] text-muted-foreground">
          No price table entry matches {names.map((n, i) => <span key={n}>{i > 0 && " or "}<Code>{n}</Code></span>)}, directly or
          through an alias. Map <Code>{r.client_model}</Code> to a priced model in <Code>pricing.aliases</Code>, or price it in{" "}
          <Code>pricing.overrides</Code>.
        </p>
        <p className="max-w-[60ch] text-muted-foreground">
          A priced model without a cache read price also leaves requests with cache reads unpriced.
        </p>
      </div>
    )
  }

  const cw1h = r.cache_write_1h_tokens ?? 0
  const lines: { label: string; tokens: number | null; price: number | null; note?: string }[] = [
    { label: "Input", tokens: r.input_tokens, price: prices.input },
    { label: "Cache read", tokens: r.cache_read_tokens, price: prices.cache_read },
    {
      label: cw1h > 0 ? "Cache write, 5 min" : "Cache write",
      tokens: r.cache_write_tokens === null ? null : r.cache_write_tokens - cw1h,
      price: prices.cache_write,
    },
    ...(cw1h > 0 ? [{ label: "Cache write, 1 hour", tokens: cw1h, price: prices.cache_write_1h }] : []),
    {
      label: "Output",
      tokens: r.output_tokens,
      price: prices.output,
      note: r.reasoning_tokens ? `includes ${fmtInt(r.reasoning_tokens)} reasoning` : undefined,
    },
  ]

  return (
    <div className="space-y-2">
      <table className="w-full border-separate border-spacing-0 overflow-hidden rounded-lg border text-[12.5px] [&_td]:h-8 [&_td]:border-t [&_td]:border-rule [&_td]:px-3 [&_th]:h-7 [&_th]:bg-surface [&_th]:px-3 [&_th]:text-right [&_th]:text-[11.5px] [&_th]:font-medium [&_th]:text-muted-foreground">
        <thead>
          <tr>
            <th className="text-left!">Column</th>
            <th>Tokens</th>
            <th>× $ / 1M</th>
            <th>= USD</th>
          </tr>
        </thead>
        <tbody>
          {lines.map((l) => (
            <tr key={l.label}>
              <td>
                {l.label}
                {l.note && <span className="text-muted-foreground"> · {l.note}</span>}
              </td>
              <td className="num text-right">{l.tokens === null ? <Dash /> : fmtInt(l.tokens)}</td>
              <td className="num text-right text-muted-foreground">{l.price === null ? <Dash /> : fmtPrice(l.price)}</td>
              <td className="num text-right">
                {l.tokens === null || l.price === null ? <Dash /> : usd((l.tokens * l.price) / 1e6)}
              </td>
            </tr>
          ))}
          <tr className="font-medium">
            <td className="border-border!">Total</td>
            <td className="border-border!" />
            <td className="border-border!" />
            <td className="num border-border! text-right">{usd(r.cost_usd)}</td>
          </tr>
        </tbody>
      </table>
      <p className="flex flex-wrap items-center gap-x-4 gap-y-1 text-muted-foreground">
        <span>
          Price <Code>{r.price_key ?? "?"}</Code>
        </span>
        <span>
          Source <Badge>{prices.source}</Badge>
        </span>
        {prices.tier_size ? <span>Tier above {fmtInt(prices.tier_size)} context tokens</span> : null}
      </p>
    </div>
  )
}

function Code({ children }: { children: ReactNode }) {
  return <code className="rounded-xs bg-muted px-1 py-px font-mono text-[12px] text-foreground">{children}</code>
}

