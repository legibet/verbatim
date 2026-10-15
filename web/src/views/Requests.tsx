import { useEffect, useMemo, useRef, useState } from "react"
import {
  getJSON,
  isConfigView,
  isObject,
  isRequestsPage,
  parseRouting,
  useApi,
  useKeyVersion,
  type RequestRow,
} from "@/api"
import { Dash, ErrorLine, FilterSelect, Mark, RangePicker, ToggleButton, pinTone, requestState } from "@/common"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Sheet, SheetContent } from "@/components/ui/sheet"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { fmtClock, fmtDateTime, fmtInt, fmtUSD, query, rangeParams, type TimeRange } from "@/format"
import { cn } from "@/lib/utils"
import { RequestDetail } from "@/views/RequestDetail"

type Filters = {
  range: TimeRange
  protocol: string
  provider: string
  route: string
  status: string
  outcome: string
  model: string
  errors: boolean
}

const INITIAL: Filters = {
  range: { id: "24h", from: "", to: "" },
  protocol: "",
  provider: "",
  route: "",
  status: "",
  outcome: "",
  model: "",
  errors: false,
}

const PROTOCOLS = ["chat", "responses", "anthropic", "gemini"]
const STATUS_CLASSES = ["2xx", "3xx", "4xx", "5xx"]
const OUTCOMES = [
  { value: "completed", label: "completed" },
  { value: "upstream_error_event", label: "upstream error event" },
  { value: "truncated", label: "truncated" },
  { value: "client_cancelled", label: "client cancelled" },
  { value: "gateway_error", label: "gateway error" },
]

const PAGE = 100
const LIVE_MS = 2000

export function Requests({ active, openId }: { active: boolean; openId: number | null }) {
  const [filters, setFilters] = useState(INITIAL)
  const [modelInput, setModelInput] = useState("")
  const [rows, setRows] = useState<RequestRow[]>([])
  const [nextBefore, setNextBefore] = useState<number | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const [live, setLive] = useState(false)
  const [arrived, setArrived] = useState<ReadonlySet<number>>(new Set())
  const [reloadTick, setReloadTick] = useState(0)
  const keyVersion = useKeyVersion()

  const config = useApi("config", isConfigView)
  const providerNames = config.data && isObject(config.data.providers) ? Object.keys(config.data.providers).sort() : []
  const routeNames = Array.isArray(config.data?.routes)
    ? config.data.routes.flatMap((r) => (isObject(r) && typeof r.model === "string" ? [r.model] : []))
    : []

  // The model substring filter applies after typing pauses.
  useEffect(() => {
    const t = window.setTimeout(() => setFilters((f) => (f.model === modelInput ? f : { ...f, model: modelInput })), 300)
    return () => window.clearTimeout(t)
  }, [modelInput])

  // The base query is fixed when the filters change: "since" is evaluated
  // once, and both paging and the live tail reuse it.
  const base = useMemo(
    () => ({
      ...rangeParams(filters.range),
      protocol: filters.protocol,
      provider: filters.provider,
      route: filters.route,
      status: filters.status,
      outcome: filters.outcome,
      model: filters.model,
      errors: filters.errors ? "1" : "",
    }),
    // reloadTick re-evaluates the time range on an explicit refresh.
    [filters, reloadTick],
  )
  const canLive = !("until" in base)

  // The in-flight "older" page, aborted whenever the query changes.
  const olderCtl = useRef<AbortController | null>(null)

  useEffect(() => {
    const ctl = new AbortController()
    setLoadingOlder(false)
    setLoading(true)
    getJSON(`requests${query({ ...base, limit: String(PAGE) })}`, isRequestsPage, ctl.signal)
      .then((page) => {
        setRows(page.rows)
        setNextBefore(page.next_before)
        setArrived(new Set())
        setError(null)
      })
      .catch((e: unknown) => {
        if (!ctl.signal.aborted) setError(e instanceof Error ? e : new Error(String(e)))
      })
      .finally(() => {
        if (!ctl.signal.aborted) setLoading(false)
      })
    // A query change or unmount also drops any "older" page still in flight.
    return () => {
      ctl.abort()
      olderCtl.current?.abort()
    }
  }, [base, keyVersion])

  // Coming back to this view reloads with a fresh time range.
  const wasActive = useRef(active)
  useEffect(() => {
    if (active && !wasActive.current) setReloadTick((n) => n + 1)
    wasActive.current = active
  }, [active])

  // Live tail: poll for rows newer than the newest one shown.
  const newest = rows[0]?.id ?? 0
  const newestRef = useRef(newest)
  newestRef.current = newest
  useEffect(() => {
    if (!live || !active || !canLive || loading || error) return
    const ctl = new AbortController()
    let timer: number | undefined
    const poll = async () => {
      if (newestRef.current === 0) {
        setReloadTick((n) => n + 1)
        return
      }
      try {
        const page = await getJSON(
          `requests${query({ ...base, after: String(newestRef.current), limit: "1000" })}`,
          isRequestsPage,
          ctl.signal,
        )
        if (page.rows.length > 0) {
          const fresh = [...page.rows].reverse() // the tail returns oldest first
          setRows((rs) => [...fresh.filter((r) => r.id > (rs[0]?.id ?? 0)), ...rs])
          setArrived(new Set(fresh.map((r) => r.id)))
        }
      } catch (e) {
        // An error (a rejected key included) stops the tail and shows in place of the table.
        if (!ctl.signal.aborted) setError(e instanceof Error ? e : new Error(String(e)))
        return
      }
      if (!ctl.signal.aborted) timer = window.setTimeout(() => void poll(), LIVE_MS)
    }
    timer = window.setTimeout(() => void poll(), LIVE_MS)
    return () => {
      ctl.abort()
      window.clearTimeout(timer)
    }
  }, [live, active, canLive, loading, error, base])

  const loadOlder = () => {
    if (nextBefore === null) return
    olderCtl.current?.abort()
    const ctl = new AbortController()
    olderCtl.current = ctl
    setLoadingOlder(true)
    getJSON(`requests${query({ ...base, before: String(nextBefore), limit: String(PAGE) })}`, isRequestsPage, ctl.signal)
      .then((page) => {
        if (ctl.signal.aborted) return
        setRows((rs) => [...rs, ...page.rows])
        setNextBefore(page.next_before)
      })
      .catch((e: unknown) => {
        if (!ctl.signal.aborted) setError(e instanceof Error ? e : new Error(String(e)))
      })
      .finally(() => {
        if (!ctl.signal.aborted) setLoadingOlder(false)
      })
  }

  // The drawer is opened through the hash, so remember which row to refocus on close.
  const lastOpenId = useRef(openId)
  if (openId !== null) lastOpenId.current = openId

  const set = <K extends keyof Filters>(k: K, v: Filters[K]) => setFilters((f) => ({ ...f, [k]: v }))
  const filtered =
    filters.protocol || filters.provider || filters.route || filters.status || filters.outcome || filters.model || filters.errors

  return (
    <div className="flex h-full flex-col pt-4 pb-4">
      <div className="flex shrink-0 flex-wrap items-center gap-2 pb-3">
        <RangePicker value={filters.range} onChange={(r) => set("range", r)} />
        <FilterSelect label="Protocol" value={filters.protocol} onChange={(v) => set("protocol", v)} options={PROTOCOLS} />
        <FilterSelect label="Provider" value={filters.provider} onChange={(v) => set("provider", v)} options={providerNames} />
        <FilterSelect label="Route" value={filters.route} onChange={(v) => set("route", v)} options={routeNames} />
        <FilterSelect label="Status" value={filters.status} onChange={(v) => set("status", v)} options={STATUS_CLASSES} />
        <FilterSelect label="Outcome" value={filters.outcome} onChange={(v) => set("outcome", v)} options={OUTCOMES} />
        <Input
          aria-label="Client model contains"
          placeholder="Model contains"
          className="w-40"
          value={modelInput}
          onChange={(e) => setModelInput(e.target.value)}
        />
        <ToggleButton pressed={filters.errors} onChange={(v) => set("errors", v)}>
          Errors only
        </ToggleButton>
        {filtered && (
          <Button
            variant="ghost"
            onClick={() => {
              setFilters({ ...INITIAL, range: filters.range })
              setModelInput("")
            }}
          >
            Clear
          </Button>
        )}
        <div className="ml-auto flex items-center gap-2">
          <Button variant="ghost" onClick={() => setReloadTick((n) => n + 1)}>
            Refresh
          </Button>
          <ToggleButton pressed={live && canLive} onChange={setLive}>
            <span
              aria-hidden
              className={cn("size-1.5 rounded-full", live && canLive ? "live-dot bg-ok" : "bg-faint")}
            />
            Live
          </ToggleButton>
        </div>
      </div>

      {error ? (
        <ErrorLine error={error} onRetry={() => setReloadTick((n) => n + 1)} />
      ) : (
        <div className="min-h-0 overflow-auto rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Time</TableHead>
                <TableHead>Protocol</TableHead>
                <TableHead>Client model</TableHead>
                <TableHead>Provider / upstream</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Stream</TableHead>
                <TableHead className="text-right">Att.</TableHead>
                <TableHead className="text-right">
                  TTFB <Unit>ms</Unit>
                </TableHead>
                <TableHead className="text-right">
                  Total <Unit>ms</Unit>
                </TableHead>
                <TableHead className="text-right">Input</TableHead>
                <TableHead className="text-right">Cache read</TableHead>
                <TableHead className="text-right">Cache write</TableHead>
                <TableHead className="text-right">Output</TableHead>
                <TableHead className="text-right">Reasoning</TableHead>
                <TableHead className="text-right">
                  Cost <Unit>USD</Unit>
                </TableHead>
                <TableHead>Pin</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading && rows.length === 0
                ? Array.from({ length: 8 }, (_, i) => (
                    <TableRow key={i}>
                      <TableCell colSpan={16}>
                        <Skeleton className="h-3.5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))
                : rows.map((r) => <Row key={r.id} r={r} selected={r.id === openId} arrived={arrived.has(r.id)} />)}
              {!loading && rows.length === 0 && (
                <TableRow>
                  <TableCell colSpan={16} className="h-24 text-center text-muted-foreground">
                    No requests in this range{filtered ? " match these filters" : ""}.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
          {rows.length > 0 && (
            <div className="sticky left-0 flex items-center gap-3 px-4 py-3 text-muted-foreground">
              <span>
                <span className="num">{fmtInt(rows.length)}</span> {rows.length === 1 ? "row" : "rows"}
              </span>
              {nextBefore !== null ? (
                <Button variant="outline" size="sm" onClick={loadOlder} disabled={loadingOlder}>
                  {loadingOlder ? "Loading" : "Older"}
                </Button>
              ) : (
                <span className="text-faint">no older rows</span>
              )}
            </div>
          )}
        </div>
      )}

      <Sheet
        open={openId !== null}
        modal={false}
        onOpenChange={(open) => {
          if (!open) location.hash = "#/requests"
        }}
      >
        <SheetContent
          aria-describedby={undefined}
          onInteractOutside={(e) => {
            // Clicking another row swaps the detail instead of closing it.
            if (e.target instanceof Element && e.target.closest("[data-request-row]")) e.preventDefault()
          }}
          onCloseAutoFocus={(e) => {
            e.preventDefault()
            document.querySelector<HTMLAnchorElement>(`a[href="#/requests/${lastOpenId.current}"]`)?.focus()
          }}
        >
          {openId !== null && <RequestDetail id={openId} />}
        </SheetContent>
      </Sheet>
    </div>
  )
}

function Unit({ children }: { children: string }) {
  return <span className="font-normal text-faint">{children}</span>
}

function N({ v }: { v: number | null }) {
  return v === null ? <Dash /> : <>{fmtInt(v)}</>
}

function Row({ r, selected, arrived }: { r: RequestRow; selected: boolean; arrived: boolean }) {
  const state = requestState(r)
  const pin = parseRouting(r.routing_json)?.pin.state ?? "none"
  const href = `#/requests/${r.id}`
  return (
    <TableRow
      data-request-row
      data-state={selected ? "selected" : undefined}
      className={cn("cursor-pointer", arrived && "row-arrived")}
      onClick={(e) => {
        if (!(e.target instanceof Element && e.target.closest("a"))) location.hash = href
      }}
    >
      <TableCell className="num">
        <Tooltip>
          <TooltipTrigger asChild>
            <a href={href} className="outline-none focus-visible:underline">
              {fmtClock(r.ts)}
            </a>
          </TooltipTrigger>
          <TooltipContent side="right" className="num">
            {fmtDateTime(r.ts)}
          </TooltipContent>
        </Tooltip>
      </TableCell>
      <TableCell>
        <span className="inline-flex items-center gap-1.5">
          {r.protocol}
          {r.operation === "count_tokens" && <Badge title="count_tokens">count</Badge>}
        </span>
      </TableCell>
      <TableCell className="font-medium">{r.client_model || <Dash />}</TableCell>
      <TableCell>
        {r.provider ?? <Dash />}
        {r.upstream_model && r.upstream_model !== r.client_model && (
          <span className="text-muted-foreground"> / {r.upstream_model}</span>
        )}
      </TableCell>
      <TableCell>
        <Mark tone={state.tone}>
          <span className="num">{r.status}</span>
          {state.note && <span className="text-muted-foreground">{state.note}</span>}
        </Mark>
      </TableCell>
      <TableCell className="text-muted-foreground">{r.stream ? "stream" : <Dash />}</TableCell>
      <TableCell className={cn("num text-right", r.attempts > 1 && "text-warn-ink")}>{r.attempts}</TableCell>
      <TableCell className="num text-right">
        <N v={r.ttfb_ms} />
      </TableCell>
      <TableCell className="num text-right">{fmtInt(r.total_ms)}</TableCell>
      <TableCell className="num text-right">
        <N v={r.input_tokens} />
      </TableCell>
      <TableCell className="num text-right">
        <N v={r.cache_read_tokens} />
      </TableCell>
      <TableCell className="num text-right">
        <N v={r.cache_write_tokens} />
      </TableCell>
      <TableCell className="num text-right">
        <N v={r.output_tokens} />
      </TableCell>
      <TableCell className="num text-right">
        <N v={r.reasoning_tokens} />
      </TableCell>
      <TableCell className="num text-right">{r.cost_usd === null ? <Dash /> : fmtUSD(r.cost_usd)}</TableCell>
      <TableCell>
        {pin === "none" ? <Dash /> : pin === "moved" ? <Mark tone={pinTone(pin)}>moved</Mark> : <span className="text-muted-foreground">{pin}</span>}
      </TableCell>
    </TableRow>
  )
}
