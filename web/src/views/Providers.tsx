import { useEffect, useState } from "react"
import { isConfigReload, isConfigView, isProvidersView, postJSON, useApi, type ConfigReload, type EndpointView } from "@/api"
import { Dash, ErrorLine, Mark, SectionTitle } from "@/common"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { fmtAge, fmtClock, fmtInt } from "@/format"
import { cn } from "@/lib/utils"

export function Providers() {
  const { data, error, reload } = useApi("providers", isProvidersView, 5000)
  const now = useNow(data?.endpoints.some((e) => e.breaker.open_until) ?? false)

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
          <span className="text-muted-foreground">One row per provider endpoint. Counts cover the last hour. Refreshes every 5 s.</span>
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
              {data.endpoints.map((e) => (
                <TableRow key={`${e.provider}.${e.protocol}`}>
                  <TableCell>
                    <span className="inline-flex items-center gap-2">
                      <span className="font-medium">{e.provider}</span>
                      <Badge>{e.protocol}</Badge>
                    </span>
                    <div className="font-mono text-[11.5px] text-muted-foreground">{e.base_url}</div>
                  </TableCell>
                  <TableCell>
                    <Breaker b={e.breaker} now={now} />
                  </TableCell>
                  <TableCell className={cn("num text-right", e.breaker.failures > 0 && "text-warn-ink")}>
                    {e.breaker.failures || <Dash />}
                  </TableCell>
                  <TableCell className="num text-right">{e.pins || <Dash />}</TableCell>
                  <TableCell className="num text-right">
                    {e.models_age_s === null ? <span className="text-faint">never fetched</span> : fmtAge(e.models_age_s)}
                  </TableCell>
                  <TableCell className="num text-right">{e.committed ? fmtInt(e.committed) : <Dash />}</TableCell>
                  <TableCell className={cn("num text-right", e.failover > 0 && "text-warn-ink")}>
                    {e.failover ? fmtInt(e.failover) : <Dash />}
                  </TableCell>
                  <TableCell className={cn("num text-right", e.error_events > 0 && "text-err-ink")}>
                    {e.error_events ? fmtInt(e.error_events) : <Dash />}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </section>

      <ConfigSection onReloaded={reload} />
    </div>
  )
}

function Breaker({ b, now }: { b: EndpointView["breaker"]; now: number }) {
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

// ConfigSection shows the configuration being served and reloads it from
// the file, as SIGHUP does.
function ConfigSection({ onReloaded }: { onReloaded: () => void }) {
  // Polled like the providers table, so a reload through SIGHUP shows too.
  const { data, error, reload } = useApi("config", isConfigView, 5000)
  const [reloading, setReloading] = useState(false)
  const [result, setResult] = useState<ConfigReload | Error | null>(null)
  const reloadConfig = async () => {
    setReloading(true)
    try {
      setResult(await postJSON("config/reload", isConfigReload))
      reload()
      onReloaded()
    } catch (e) {
      setResult(e instanceof Error ? e : new Error(String(e)))
    } finally {
      setReloading(false)
    }
  }

  return (
    <section>
      <SectionTitle
        aside={
          <Button variant="outline" size="sm" disabled={reloading} onClick={() => void reloadConfig()}>
            {reloading ? "Reloading config" : "Reload config"}
          </Button>
        }
      >
        Configuration
      </SectionTitle>
      <p className="mb-3 max-w-[72ch] text-muted-foreground">
        Reload reads the configuration file again, as SIGHUP does. Running requests finish under the previous
        configuration; pins are kept, and circuit breakers and model lists start afresh.
      </p>
      {result && (
        <div role="status" className="mb-3 space-y-1">
          {result instanceof Error ? (
            <>
              <Mark tone="err" className="text-err-ink">
                Reload failed; the previous configuration is still served.
              </Mark>
              <pre className="max-w-[100ch] font-mono text-[12px] whitespace-pre-wrap text-err-ink">{result.message}</pre>
            </>
          ) : (
            <>
              <Mark tone="ok">Configuration reloaded.</Mark>
              {result.warnings.length > 0 && (
                <details>
                  <summary className="w-fit cursor-pointer text-muted-foreground select-none">
                    {result.warnings.length} {result.warnings.length === 1 ? "warning" : "warnings"}
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
      <details className="group">
        <summary className="flex w-fit cursor-pointer list-none items-baseline gap-2 outline-none select-none focus-visible:underline [&::-webkit-details-marker]:hidden">
          <span aria-hidden className="inline-block w-3 text-muted-foreground group-open:rotate-90">
            ›
          </span>
          <span>Served configuration</span>
          <span className="text-muted-foreground">keys, header values, and URL credentials are redacted</span>
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
