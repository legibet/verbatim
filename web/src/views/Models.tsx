import { Fragment, useState, type ReactNode } from "react"
import {
  isModelsView,
  isPricingSync,
  postJSON,
  useApi,
  type ModelTarget,
  type ModelView,
  type ModelsView,
  type PricingSync,
  type TargetPrice,
} from "@/api"
import { Dash, ErrorLine, Mark } from "@/common"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { fmtInt, fmtPrice } from "@/format"
import { cn } from "@/lib/utils"

// Models lists the names /v1/models returns, with the route that serves
// each, its providers, and its price. A row opens to every target per
// protocol. Its actions fetch every model list and sync the price table.
export function Models() {
  const { data, error, reload } = useApi("models", isModelsView, 30_000)
  const [filter, setFilter] = useState("")
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set())
  const [refreshing, setRefreshing] = useState(false)
  const [refreshError, setRefreshError] = useState<Error | null>(null)

  const refresh = async () => {
    setRefreshing(true)
    setRefreshError(null)
    try {
      await postJSON("models/refresh", isModelsView)
      reload()
    } catch (e) {
      setRefreshError(e instanceof Error ? e : new Error(String(e)))
    } finally {
      setRefreshing(false)
    }
  }
  // A price sync downloads pricing.sources as `verbatim pricing sync` does;
  // new requests are priced with the new table, logged ones keep their cost.
  const [syncing, setSyncing] = useState(false)
  const [synced, setSynced] = useState<PricingSync | Error | null>(null)
  const sync = async () => {
    setSyncing(true)
    try {
      setSynced(await postJSON("pricing/sync", isPricingSync))
      reload()
    } catch (e) {
      setSynced(e instanceof Error ? e : new Error(String(e)))
    } finally {
      setSyncing(false)
    }
  }
  const toggle = (name: string) =>
    setOpen((s) => {
      const next = new Set(s)
      if (!next.delete(name)) next.add(name)
      return next
    })

  if (error) return <ErrorLine error={error} onRetry={reload} />
  if (!data)
    return (
      <div className="space-y-3 pt-6">
        <Skeleton className="h-4 w-80" />
        <Skeleton className="h-40 w-full" />
      </div>
    )

  const q = filter.trim().toLowerCase()
  const shown = q
    ? data.models.filter((m) => m.name.toLowerCase().includes(q) || providersOf(m).all.some((p) => p.toLowerCase().includes(q)))
    : data.models

  return (
    <div className="space-y-4 pt-4 pb-10">
      <div className="flex flex-wrap items-center gap-2">
        <Input
          aria-label="Filter by model or provider"
          placeholder="Model or provider"
          className="w-56"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        <div className="ml-auto flex items-center gap-3">
          <span className="num text-muted-foreground">
            {q ? `${shown.length} of ${data.models.length}` : data.models.length}{" "}
            {data.models.length === 1 ? "model" : "models"}
          </span>
          <Button variant="outline" disabled={syncing} onClick={() => void sync()}>
            {syncing ? "Syncing prices" : "Sync prices"}
          </Button>
          <Button variant="outline" disabled={refreshing} onClick={() => void refresh()}>
            {refreshing ? "Fetching model lists" : "Fetch model lists"}
          </Button>
        </div>
      </div>

      {refreshError && (
        <Mark tone="err" className="whitespace-normal text-err-ink">
          Fetch failed: {refreshError.message}
        </Mark>
      )}
      {synced && <SyncResult result={synced} />}

      {shown.length === 0 ? (
        <p className="py-4 text-muted-foreground">{q ? "No model matches the filter." : "No model names are listed."}</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Model</TableHead>
                <TableHead>Route</TableHead>
                <TableHead>Providers</TableHead>
                <TableHead className="w-full">Protocols</TableHead>
                <PriceHeads />
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((m) => (
                <ModelRows key={m.name} m={m} open={open.has(m.name)} onToggle={() => toggle(m.name)} />
              ))}
            </TableBody>
          </Table>
        </div>
      )}
      <Footnotes data={data} />
    </div>
  )
}

// PriceHeads are the price column headers; prices are USD per 1M tokens.
function PriceHeads({ as: Head = TableHead }: { as?: typeof TableHead | "th" }) {
  return (
    <>
      {["Input", "Output", "Cache read"].map((l) => (
        <Head key={l} className="text-right!">
          {l} <span className="font-normal text-faint">$/M</span>
        </Head>
      ))}
    </>
  )
}

function SyncResult({ result }: { result: PricingSync | Error }) {
  if (result instanceof Error)
    return (
      <Mark tone="err" className="whitespace-normal text-err-ink">
        Sync failed: {result.message}
      </Mark>
    )
  return (
    <div role="status" className="space-y-1">
      <Mark tone="ok">{fmtInt(result.models)} models priced.</Mark>
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
    </div>
  )
}

// Footnotes say what the table cannot show: the routes that list no name,
// and the endpoints whose model list cannot be fetched, with the error on
// hover.
function Footnotes({ data }: { data: ModelsView }) {
  const { unlisted, failing } = data
  if (unlisted.length === 0 && failing.length === 0) return null
  return (
    <div className="space-y-1 text-muted-foreground">
      {unlisted.length > 0 && (
        <p>
          {unlisted.map((r, i) => (
            <Fragment key={r}>
              {i > 0 && ", "}
              <code className="font-mono text-foreground">{r}</code>
            </Fragment>
          ))}{" "}
          also {unlisted.length === 1 ? "routes" : "route"} models not listed here.
        </p>
      )}
      {failing.length > 0 && (
        <p>
          <Mark tone="warn">
            <span>
              No model list from{" "}
              {failing.map((f, i) => (
                <Fragment key={f.endpoint}>
                  {i > 0 && ", "}
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <button
                        type="button"
                        className="cursor-default text-foreground underline decoration-faint decoration-dotted underline-offset-3 outline-none focus-visible:decoration-foreground"
                      >
                        {f.endpoint}
                      </button>
                    </TooltipTrigger>
                    <TooltipContent className="max-w-[60ch] font-mono break-all">{f.error}</TooltipContent>
                  </Tooltip>
                </Fragment>
              ))}
            </span>
          </Mark>
        </p>
      )}
    </div>
  )
}

function ModelRows({ m, open, onToggle }: { m: ModelView; open: boolean; onToggle: () => void }) {
  const providers = providersOf(m)
  const priced = m.protocols.flatMap((p) => p.targets.flatMap((t) => (t.price ? [t.price] : [])))
  // The main row shows text prices; image prices are in the open row.
  return (
    <>
      <TableRow data-state={open ? "selected" : undefined} className="cursor-pointer" onClick={onToggle}>
        <TableCell>
          <button
            type="button"
            aria-expanded={open}
            className="inline-flex cursor-pointer items-baseline gap-2 font-medium outline-none focus-visible:underline"
            onClick={(e) => {
              e.stopPropagation()
              onToggle()
            }}
          >
            <span aria-hidden className={cn("inline-block w-2.5 text-muted-foreground", open && "rotate-90")}>
              ›
            </span>
            {m.name}
          </button>
        </TableCell>
        <TableCell className="num text-muted-foreground">{m.routes.join(", ")}</TableCell>
        <TableCell>
          {providers.primary.join(", ")}
          {providers.backup > 0 && <span className="text-muted-foreground"> +{providers.backup}</span>}
        </TableCell>
        <TableCell className="whitespace-normal">
          <span className="flex flex-wrap gap-1">
            {m.protocols.map((p) => (
              <Badge key={p.protocol}>{p.protocol}</Badge>
            ))}
          </span>
        </TableCell>
        {priced.length === 0 ? (
          <TableCell colSpan={3} className="text-right text-warn-ink">
            unpriced
          </TableCell>
        ) : (
          <>
            <TableCell className="num text-right">{priceRange(priced, (p) => p.input)}</TableCell>
            <TableCell className="num text-right">{priceRange(priced, (p) => p.output)}</TableCell>
            <TableCell className="num text-right">{priceRange(priced, (p) => p.cache_read)}</TableCell>
          </>
        )}
      </TableRow>
      {open && (
        <TableRow className="[@media(hover:hover)]:hover:[&>td]:bg-transparent">
          <TableCell colSpan={7} className="py-3 pl-8.5 whitespace-normal">
            <Targets m={m} />
          </TableCell>
        </TableRow>
      )}
    </>
  )
}

// Targets lists every candidate per protocol in attempt-tier order, as the
// gateway would route a request for this name.
function Targets({ m }: { m: ModelView }) {
  return (
    <table className="w-full max-w-5xl border-separate border-spacing-0 overflow-hidden rounded-lg border bg-background text-[12.5px] [&_td]:h-8 [&_td]:border-t [&_td]:border-rule [&_td]:px-3 [&_th]:h-7 [&_th]:bg-surface [&_th]:px-3 [&_th]:text-left [&_th]:text-[11.5px] [&_th]:font-medium [&_th]:whitespace-nowrap [&_th]:text-muted-foreground">
      <thead>
        <tr>
          <th>Protocol</th>
          <th className="text-right!">Priority</th>
          <th className="w-full">Provider / upstream model</th>
          <PriceHeads as="th" />
        </tr>
      </thead>
      <tbody>
        {m.protocols.map((p) =>
          p.targets.map((t, i) => (
            <tr key={`${p.protocol}/${t.provider}/${t.upstream_model}`}>
              {i === 0 && (
                <td rowSpan={p.targets.length} className="align-top leading-8">
                  {p.protocol}
                  {m.routes.length > 1 && <span className="num text-muted-foreground"> {p.route}</span>}
                </td>
              )}
              <td className="num text-right">{t.priority}</td>
              <td className="break-all">
                {t.provider}
                <span className="text-muted-foreground"> / {t.upstream_model}</span>
              </td>
              <TargetPrices t={t} images={p.protocol === "images"} />
            </tr>
          )),
        )}
      </tbody>
    </table>
  )
}

// TargetPrices fills the Input, Output, and Cache read columns. An images
// request's text and image tokens are priced apart, so an images target
// shows both pairs, input / output.
function TargetPrices({ t, images }: { t: ModelTarget; images: boolean }) {
  const p = t.price
  if (!p)
    return (
      <td colSpan={3} className="text-right text-warn-ink">
        unpriced
      </td>
    )
  const fmt = (v: number | null) => (v === null ? <Dash /> : fmtPrice(v))
  if (images)
    return (
      <td colSpan={3} className="num text-right">
        <span className="text-muted-foreground">text</span> {fmt(p.input)} / {fmt(p.output)}
        <span className="text-muted-foreground"> · image</span> {fmt(p.image_input)} / {fmt(p.image_output)}
      </td>
    )
  return (
    <>
      <td className="num text-right">{fmt(p.input)}</td>
      <td className="num text-right">{fmt(p.output)}</td>
      <td className="num text-right">{fmt(p.cache_read)}</td>
    </>
  )
}

// providersOf names the providers of the route's first priority tier, and
// counts the providers that only appear in later tiers.
function providersOf(m: ModelView): { primary: string[]; backup: number; all: string[] } {
  const targets = m.protocols.flatMap((p) => p.targets)
  const first = Math.min(...targets.map((t) => t.priority))
  const primary: string[] = []
  const all: string[] = []
  for (const t of targets) {
    if (t.priority === first && !primary.includes(t.provider)) primary.push(t.provider)
    if (!all.includes(t.provider)) all.push(t.provider)
  }
  return { primary, backup: all.filter((p) => !primary.includes(p)).length, all }
}

// priceRange shows one price, or the lowest and highest when the targets
// differ; a price no target has is a dash.
function priceRange(prices: TargetPrice[], get: (p: TargetPrice) => number | null): ReactNode {
  const vs = prices.map(get).filter((v): v is number => v !== null)
  if (vs.length === 0) return <Dash />
  const lo = Math.min(...vs)
  const hi = Math.max(...vs)
  return lo === hi ? fmtPrice(lo) : `${fmtPrice(lo)}–${fmtPrice(hi)}`
}
