# verbatim

A self-hosted gateway between LLM API clients and LLM API providers. It routes each request to an upstream that speaks the client's API, fails over when an upstream errors, keeps a session on one upstream, and records usage and cost.

verbatim never converts between API formats. A request is forwarded as the client sent it, with only the API key, model name, and URL changed, so reasoning state such as thinking signatures and encrypted reasoning survives. The trade-off: a client can only reach providers that natively speak its API.

One Go binary, one YAML file, one SQLite database, an embedded web UI.

## Features

- Entry points for Chat Completions, Responses, Anthropic Messages (with `count_tokens`), the OpenAI Images API, and Codex's web search. `GET /v1/models` lists the models your routes serve.
- Ordered routes with glob model patterns, priority tiers, and weighted or round-robin balancing.
- Failover on connection errors, timeouts, 429, 500/502/503/504/529, and 401/403/404, decided before anything reaches the client. Other errors, including every 400, are returned unchanged. A circuit breaker per provider endpoint.
- Session affinity keyed by request fields you name (Claude Code's `metadata.user_id`, Codex's `session-id`), so prompt caches and thinking signatures stay valid. Responses `previous_response_id` goes to the upstream that created it.
- Static per-provider request overrides for services with quirks.
- A request log with routing decisions, every attempt, token usage as reported, and cost from list prices. Browse it from the CLI or the web UI.

Not supported: Gemini's native API (use Google's OpenAI-compatible endpoint as a `chat` endpoint), Responses resource operations such as `GET /v1/responses/{id}`, multiple client keys, quotas.

## Install

Download an archive for your platform from [Releases](https://github.com/legibet/verbatim/releases) and put `verbatim` on your `PATH`. Each archive also has the example configuration.

Or build it with Go 1.27 or later. 

```sh
go install github.com/legibet/verbatim/cmd/verbatim@latest
```

## Quick start

1. Copy [`verbatim.example.yaml`](verbatim.example.yaml) to `./verbatim.yaml` or `~/.verbatim/verbatim.yaml` and edit it. Every field is annotated.
2. Export the keys it references (`${OPENAI_KEY}` and so on) and run `verbatim check`. `verbatim check --probe` also sends each endpoint an empty request to test its `base_url` and key.
3. Start the server and point a client at it:

```sh
export VERBATIM_KEY=...
verbatim serve

curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $VERBATIM_KEY" -H "Content-Type: application/json" \
  -d '{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}'

ANTHROPIC_BASE_URL=http://127.0.0.1:8080 ANTHROPIC_API_KEY=$VERBATIM_KEY claude
```

Codex, in `~/.codex/config.toml`:

```toml
model_provider = "verbatim"

[model_providers.verbatim]
name = "verbatim"
base_url = "http://127.0.0.1:8080/v1"
env_key = "VERBATIM_KEY"
wire_api = "responses"
# Web search for models Codex runs without hosted tools. Works when the
# routed upstream serves /alpha/search.
supports_standalone_web_search = true
# Codex offers its image tool to a custom provider only when this header is
# set (any value). Route gpt-image-2 to a provider with an images endpoint.
http_headers = { "x-openai-actor-authorization" = "verbatim" }

# Remote compaction. The compaction item is readable only by the upstream that
# produced it, which session affinity keeps. An upstream without remote
# compaction (DeepSeek) makes Codex report an error; leave this out to compact locally.
[model_providers.verbatim.capabilities]
remote_compaction = "v2"
```

## Configuration

The client's model name picks the route, and the client's API picks which of the route's targets can serve it.

| Section | Configures |
|---|---|
| `server` | Listen address, client API key, body and concurrency limits, upstream timeouts |
| `providers` | Upstream services: key, headers, overrides, and one `base_url` per API they speak |
| `routes` | Model patterns, each with targets: provider, upstream model name, priority, weight |
| `affinity` | Which request fields identify a session, pin lifetime, `prefer` or `strict` mode |
| `breaker` | Failure threshold and cooldown |
| `models` | Model-list cache TTL and fetch timeout |
| `log` | SQLite path, retention, error excerpt size |
| `pricing` | Price sources, local overrides, model name aliases |

`${VAR}` in any value is read from the environment; an unset variable or an unknown key is an error.

To reload a running server, send `SIGHUP` (`pkill -HUP verbatim`) or use Reload config in the web UI. A file with errors is refused and the old configuration keeps serving. Changes to `server.listen`, `log.path`, `log.retention`, and `affinity.ttl` need a restart.

## CLI

```
verbatim [serve] [-c config.yaml] [-v]       run the gateway (default command)
verbatim check   [-c config.yaml] [--probe]  validate the configuration; --probe also tries each endpoint
verbatim usage   [--since 7d] [--by model|provider|route|day] [--protocol p] [--json]
verbatim log     [--tail] [--since 1h] [--status 5xx] [--provider p] [--errors] [--json]
verbatim log     <request_id>                one request in full: routing, attempts, usage, price
verbatim pricing sync                        download pricing.sources into pricing.cache
verbatim pricing show <model>                the price entry for a client model name
verbatim status                              live endpoint state from the running server
verbatim version                             the version this binary was built from
```

`usage` and `log` read the database directly and work while the server runs. Only `serve` and `check` need the provider keys in the environment.

## Web UI

Open `http://127.0.0.1:8080/ui/` (or whatever `server.listen` is). Views: Requests (with a live tail and a detail drawer showing routing, attempts, and cost), Usage, Models, and Providers (breakers, pins, model lists). The UI never edits the configuration. Its only actions are syncing prices, fetching model lists, and reloading the configuration file.

## Pricing

Costs come from list prices, not from what a provider or relay actually bills. `verbatim pricing sync` downloads the [llm-metadata](https://github.com/basellm/llm-metadata) / models.dev price files listed in `pricing.sources`. Prices are matched by the client's model name, exactly, then through `pricing.aliases`. Image models such as `gpt-image-2` need prices in the overrides file (see the example config). The UI and `verbatim usage` list the models that still have no price.

## Development

```sh
go test -race ./...
cd web && pnpm install && pnpm dev    # UI dev server against a running verbatim
cd web && pnpm build                  # writes internal/ui/dist; commit it
```

Design docs are in [`docs/`](docs/); start with [`docs/architecture.md`](docs/architecture.md).

## License

[MIT](LICENSE)
