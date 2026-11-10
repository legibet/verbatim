# Web UI

A UI embedded in the binary at `/ui/`. It reads the request log and the gateway's in-memory state. It has two kinds of action: a price sync (same as `verbatim pricing sync`) plus a model-list fetch, and a configuration reload (same as `SIGHUP`). None of them changes the configuration file or a logged row. The configuration file stays the only source of truth: no settings, no key management, no provider editing. That line is what keeps verbatim from growing into a management system.

## Stack and build

`web/` is a Vite + React + TypeScript + Tailwind + shadcn/ui project. `pnpm build` writes to `internal/ui/dist`, which is committed and embedded with `go:embed`, so a Go build never needs Node. Charts use uPlot.

```sh
cd web
pnpm install
pnpm dev     # http://localhost:5173/ui/, proxies /ui/api to $VERBATIM_URL (default http://127.0.0.1:8080)
pnpm build   # then commit internal/ui/dist
```

When `server.api_key` is set, the UI asks for it once and sends it as a bearer token; `/ui/api` accepts only `Authorization: Bearer`, and the static assets are public. Live views poll; there is no push channel.

## Style

An operations tool: restrained but finished, not a product dashboard and not a bare admin template. Deliberate typography and consistent spacing; dense tables with tabular monospace numerals and right-aligned numbers; status and outcome shown as small colored marks with text; a neutral palette with color reserved for state (ok, failover, error, open breaker); light and dark following the system; no hero sections or decoration. Details open in place (a drawer or expanded row), not on separate pages.

## Views

- **Requests.** One row per request: time, protocol and operation, client model and where it went, status and outcome, stream flag, time to first byte and total, token columns, cost, attempts, pin state. Filters for time range, protocol, provider, route, status class, outcome, model substring, errors only. A live mode appends rows by polling.
- **Request detail**, opened from a row. The routing explanation (route, candidates, order, ownership, pin state, breaker skips), the attempt timeline with error excerpts, the raw usage object, and the cost breakdown: each token column times its stored unit price, with the total and the price key.
- **Usage.** Totals for the range and a breakdown by model, provider, route, or day, with a daily chart of cost and tokens. Every aggregate states how many requests had no price or no usage, and the view lists the model names that still have no price.
- **Models.** The names `/v1/models` lists, in its order: per name, the protocols that list it and the route serving each, the first-tier providers, and the text price (a range when targets differ). A row expands to the targets per protocol with priority, upstream model, and price, looked up as a request's would be (text and image prices for an `images` target). Only targets whose endpoint lists their upstream model are shown, so an exact-name route that no endpoint lists has none. Below the table: pattern routes that list no name (they still route what they match) and endpoints whose last model-list fetch failed, with the error on hover. Actions: Sync prices, Fetch model lists.
- **Providers.** Live state per endpoint: breaker (closed, open until when, half-open), consecutive counted failures, committed / failover / `upstream_error_event` counts over the last hour, active pins, model-list age or fetch error, dropped log rows. Below it, the served configuration, redacted, with Reload config.

## API

Under `/ui/api/`: `GET requests` (filters, cursor pagination, `after=` for the live tail), `GET requests/{id}` (the numeric row id), `GET usage`, `GET providers`, `GET models`, `POST models/refresh` (fetches every list, then answers like `GET models`), `GET config`, `POST pricing/sync`, `POST config/reload` (returns the new file's warnings, or 422 with the reason it was refused).

`GET config` masks `server.api_key`, every provider `key`, and every `headers` value, and strips userinfo from `base_url`s. The API rejects cross-origin browser requests other than `GET`, `HEAD`, and `OPTIONS` (`http.CrossOriginProtection`), so another site cannot trigger a sync or reload on a loopback server without `server.api_key`.
