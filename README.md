# verbatim

A small, self-hosted gateway between LLM API clients and LLM API providers. It routes requests to upstreams, retries on failure, keeps a session on the same upstream, records usage and cost, and does nothing else. It never converts between API formats.

One Go binary, one YAML file, one SQLite database, an embedded read-only web UI.

## Principles

- **Zero format conversion.** A Chat Completions request goes to a provider that speaks Chat Completions; a Messages request to one that speaks Messages; a Responses request to one that speaks Responses. There is no internal message representation. Opaque reasoning state (thinking signatures, encrypted reasoning) passes through untouched because everything passes through untouched.
- **Pass through by default.** verbatim changes three things in a request: the auth header, the model name, and the destination URL. Everything else is forwarded byte for byte, and an upstream response, success or error, is returned unchanged.
- **No hidden behavior.** Every change is declared in the configuration file. No heuristics on hostnames, error text, or model names.
- **Deterministic per target.** The same request to the same target is always the same bytes. Session affinity keeps an agent conversation on one upstream, which is what prompt caching and Anthropic preserved thinking need.

The design document, `docs/design.md`, is the behavior contract.

## What it does

- Entry points: `POST /v1/chat/completions`, `POST /v1/responses`, `POST /v1/messages` and `/v1/messages/count_tokens`; `GET /v1/models` assembled from the configuration and the providers' own lists.
- Routing: ordered routes with glob patterns; targets with priority tiers and weights, weighted or round-robin balancing; a tier is exhausted before the next one is tried.
- Failover on connection failures and on 408/409/425/429/5xx/529 (and 401/403/404, with a warning), decided before anything is sent to the client. Everything else, including every 400, is returned unchanged.
- Circuit breaker per provider, with a single half-open probe.
- Session affinity: the session key comes from request fields you name in the configuration (Claude Code's `metadata.user_id`, Codex's `session_id` header, ...), with a prefix hash as fallback; the pin is reserved before the first attempt so parallel first requests of a session land together. Responses `previous_response_id` is routed to the upstream that produced the id.
- Per-provider static request overrides for relay quirks (delete a field, set a literal).
- Request log in SQLite: how each request was routed, every attempt, tokens (input, cache read, cache write, output, reasoning) as the provider reported them, and a cost from configured list prices.
- CLI for the log and usage; a web UI at `/ui/` for requests, request detail, usage, and live provider state.

Not in version 1: the Gemini entry point (the protocol adapter exists, the entry point is not served), Responses resource operations (`GET /v1/responses/{id}` and friends return 404), per-user keys, quotas, billing.

## Install

Requires Go 1.27. The web UI is prebuilt and committed, so a Go build needs no Node.

```sh
go install ./cmd/verbatim
```

or `go build -o verbatim ./cmd/verbatim`.

## Quick start

1. Copy `verbatim.example.yaml` to `./verbatim.yaml` (or `~/.verbatim/verbatim.yaml`) and edit it. The example is annotated and covers every section.
2. Export the keys it references (`${OPENAI_KEY}` and so on) and run `verbatim check`.
3. `verbatim serve`, then point a client at it:

```sh
export VERBATIM_KEY=...
verbatim serve

# Chat Completions
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $VERBATIM_KEY" \
  -d '{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}'

# Claude Code
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 ANTHROPIC_API_KEY=$VERBATIM_KEY claude
```

The client's model name picks the route; the client's protocol picks which of the route's targets are eligible.

## Configuration

See `verbatim.example.yaml`. The sections:

| Section | What it configures |
|---|---|
| `server` | Listen address, client API key, body and concurrency limits, upstream timeouts |
| `providers` | One upstream endpoint per entry: `type`, `base_url`, `key`, optional `auth`, `headers`, `overrides` |
| `routes` | Ordered list of `model` glob → `targets` (`provider`, optional upstream `model`, `priority`, `weight`) |
| `affinity` | Session key rules per protocol, fallback, TTL, `prefer` or `strict` mode |
| `breaker` | Failure threshold and cooldown |
| `models` | Model-list cache TTL and fetch timeout |
| `log` | SQLite path, retention, error excerpt size |
| `pricing` | Price sources to sync, local overrides, name aliases |

`${VAR}` in any value is expanded from the environment; an unset variable is an error. Unknown keys are errors. `verbatim check` lists every problem it finds.

## CLI

```
verbatim [serve] [-c config.yaml] [-v]   run the gateway (default command)
verbatim check   [-c config.yaml]        validate the configuration and exit
verbatim usage   [--since 7d] [--by model|provider|route|day] [--protocol p] [--json]
verbatim log     [--tail] [--since 1h] [--status 5xx] [--provider p] [--errors] [--json]
verbatim log     <request_id>            one request in full: routing, attempts, usage, price
verbatim pricing sync                    download pricing.sources into pricing.cache
verbatim pricing show <model>            the price entry for a client model name
verbatim status                          live provider state from the running server
```

`usage` and `log` read the SQLite file directly and work while the server runs. The read-only commands do not need the provider keys in the environment.

## Web UI

`http://<listen>/ui/`. Read-only: it shows the request log and the server's live state (breakers, pins, model-list age) and edits nothing. If `server.api_key` is set, the UI asks for it once. Views: Requests (filters, live tail, detail drawer with the routing explanation, attempt timeline, raw usage, cost breakdown), Usage (totals, daily chart, breakdown by model / provider / route / day, unpriced models), Providers.

## Pricing

Costs are configured list prices, nothing more: they cannot know what a relay actually charges. `verbatim pricing sync` fetches llm-metadata / models.dev provider files; a request is priced by the client's model name, exact match only, then by `pricing.aliases`. Requests with no price are counted and listed as such in the UI and in `verbatim usage`.

## Development

```sh
go test -race ./...
cd web && pnpm install && pnpm dev     # UI dev server; proxies /ui/api to $VERBATIM_URL (default http://127.0.0.1:8080)
cd web && pnpm build                   # writes internal/ui/dist; commit the result
```

Dependencies are deliberately few: `gjson`/`sjson` for point edits of JSON bodies, `modernc.org/sqlite` (pure Go), `yaml.v3`, and the standard library.
