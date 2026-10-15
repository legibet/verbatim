import { useEffect, useState } from "react"
import { isConfigView, isProvidersView, useApi, type ProviderView } from "@/api"
import { Dash, ErrorLine, Mark, SectionTitle } from "@/common"
import { Badge } from "@/components/ui/badge"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { fmtAge, fmtClock, fmtInt } from "@/format"
import { cn } from "@/lib/utils"

export function Providers() {
  const { data, error, reload } = useApi("providers", isProvidersView, 5000)
  const now = useNow(data?.providers.some((p) => p.breaker.open_until) ?? false)

  if (error) return <ErrorLine error={error} onRetry={reload} />
  if (!data)
    return (
      <div className="space-y-3 pt-6">
        <Skeleton className="h-4 w-80" />
        <Skeleton className="h-40 w-full" />
      </div>
    )

  return (
    <div className="space-y-8 pt-5 pb-10">
      <section>
        <div className="mb-3 flex flex-wrap items-baseline gap-x-5 gap-y-1">
          <span>
            <span className="num">{fmtInt(data.pins_total)}</span> live {data.pins_total === 1 ? "pin" : "pins"}
          </span>
          <span className={cn(data.log_dropped > 0 && "text-err-ink")}>
            {data.log_dropped > 0 ? (
              <Mark tone="err">
                <span className="num">{fmtInt(data.log_dropped)}</span> log rows dropped
              </Mark>
            ) : (
              <>
                <span className="num">0</span> log rows dropped
              </>
            )}
          </span>
          <span className="text-muted-foreground">Counts cover the last hour. Refreshes every 5 s.</span>
        </div>
        <div className="overflow-x-auto rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Provider</TableHead>
                <TableHead>Breaker</TableHead>
                <TableHead className="text-right">Failures</TableHead>
                <TableHead className="text-right">Pins</TableHead>
                <TableHead className="text-right">Model list age</TableHead>
                <TableHead className="text-right">Committed</TableHead>
                <TableHead className="text-right">Failover</TableHead>
                <TableHead className="text-right">Error events</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.providers.map((p) => (
                <TableRow key={p.name}>
                  <TableCell>
                    <span className="inline-flex items-center gap-2">
                      <span className="font-medium">{p.name}</span>
                      <Badge>{p.type}</Badge>
                    </span>
                    <div className="font-mono text-[11.5px] text-muted-foreground">{p.base_url}</div>
                  </TableCell>
                  <TableCell>
                    <Breaker b={p.breaker} now={now} />
                  </TableCell>
                  <TableCell className={cn("num text-right", p.breaker.failures > 0 && "text-warn-ink")}>
                    {p.breaker.failures || <Dash />}
                  </TableCell>
                  <TableCell className="num text-right">{p.pins || <Dash />}</TableCell>
                  <TableCell className="num text-right">
                    {p.models_age_s === null ? <span className="text-faint">never fetched</span> : fmtAge(p.models_age_s)}
                  </TableCell>
                  <TableCell className="num text-right">{p.committed ? fmtInt(p.committed) : <Dash />}</TableCell>
                  <TableCell className={cn("num text-right", p.failover > 0 && "text-warn-ink")}>
                    {p.failover ? fmtInt(p.failover) : <Dash />}
                  </TableCell>
                  <TableCell className={cn("num text-right", p.error_events > 0 && "text-err-ink")}>
                    {p.error_events ? fmtInt(p.error_events) : <Dash />}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </section>

      <ConfigSection />
    </div>
  )
}

function Breaker({ b, now }: { b: ProviderView["breaker"]; now: number }) {
  if (b.state === "open") {
    const until = b.open_until ? new Date(b.open_until) : null
    const left = until ? Math.max(0, Math.ceil((until.getTime() - now) / 1000)) : null
    return (
      <Mark tone="err">
        open
        {until && (
          <span className="num text-muted-foreground">
            until {fmtClock(until)} · {left}s
          </span>
        )}
      </Mark>
    )
  }
  if (b.state === "half_open") return <Mark tone="warn">half-open</Mark>
  return <Mark tone="ok">closed</Mark>
}

// useNow ticks every second while a countdown is visible.
function useNow(ticking: boolean): number {
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    if (!ticking) return
    const t = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(t)
  }, [ticking])
  return ticking ? now : Date.now()
}

function ConfigSection() {
  const { data, error } = useApi("config", isConfigView)
  return (
    <section>
      <details className="group">
        <summary className="flex w-fit cursor-pointer list-none items-baseline gap-2 outline-none select-none focus-visible:underline [&::-webkit-details-marker]:hidden">
          <span aria-hidden className="inline-block w-3 text-muted-foreground group-open:rotate-90">
            ›
          </span>
          <SectionTitle>Loaded configuration</SectionTitle>
          <span className="text-muted-foreground">read-only; keys, header values, and URL credentials are redacted</span>
        </summary>
        <div className="mt-1 ml-5">
          {error ? (
            <ErrorLine error={error} />
          ) : (
            <pre className="overflow-x-auto rounded-lg border bg-surface px-4 py-3 font-mono text-[12px] leading-relaxed">
              {data ? JSON.stringify(data, null, 2) : "Loading"}
            </pre>
          )}
        </div>
      </details>
    </section>
  )
}
