# verbatim

verbatim is a self-hosted, pass-through LLM API gateway. It forwards each request as-is to upstreams that natively speak the same protocol, replacing only the URL, API key, and model name. Thinking signatures, encrypted reasoning, and prompt caches are kept intact.

- Supports OpenAI Chat Completions, Responses, Images, and Anthropic Messages.
- Routes by model name (with glob patterns), priority tiers, and weighted or round-robin balancing.
- Fails over automatically on upstream errors or timeouts, with a circuit breaker per endpoint.
- Pins sessions to the same upstream by request field or prefix hash. Responses `previous_response_id` goes to the upstream that created it.
- Supports static per-provider header and JSON body overrides.
- Logs routing, attempts, token usage, and cost in SQLite, with a CLI and an embedded web UI.

## Install

Download a release from [Releases](https://github.com/legibet/verbatim/releases) and put `verbatim` on your `PATH`. Each archive includes [`verbatim.example.yaml`](verbatim.example.yaml).

Or build from source with Go 1.27 or later:

```sh
go install github.com/legibet/verbatim/cmd/verbatim@latest
```

## Quick start

1. Copy [`verbatim.example.yaml`](verbatim.example.yaml) to `./verbatim.yaml` or `~/.verbatim/verbatim.yaml` and edit it.
2. Export your API keys and run `verbatim check` to validate the configuration (add `--probe` to also verify upstream connectivity and API keys).
3. Start the server:

```sh
export VERBATIM_KEY=...
verbatim serve

curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $VERBATIM_KEY" -H "Content-Type: application/json" \
  -d '{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}'
```

## Configuration

All configuration lives in a single YAML file (see [`verbatim.example.yaml`](verbatim.example.yaml) for a fully annotated reference).

## CLI

```
verbatim [serve] [-c config.yaml] [-v]       run the gateway (default command)
verbatim check   [-c config.yaml] [--probe]  validate configuration; --probe also tests each endpoint
verbatim usage   [--since 7d] [--by model|provider|route|day] [--protocol p] [--json]
verbatim log     [--tail] [--since 1h] [--status 5xx] [--provider p] [--errors] [--json]
verbatim log     <request_id>                inspect one request: routing, attempts, usage, cost
verbatim pricing sync                        download pricing.sources into pricing.cache
verbatim pricing show <model>                show the price entry for a client model name
verbatim status                              live endpoint state from the running server
verbatim version                             print the build version
```

## Web UI

Open `http://127.0.0.1:8080/ui/` (or your `server.listen` address) to view the live request log, per-request routing and attempt details, usage aggregates, models, and provider status (circuit breakers and active session pins). The Web UI supports syncing prices, refreshing model lists, and reloading the configuration, and it never modifies the configuration file.
## Development

```sh
go test -race ./...
cd web && pnpm install && pnpm dev    # UI dev server against a running verbatim
cd web && pnpm build                  # writes internal/ui/dist; commit it
```

Design docs are in [`docs/`](docs/); start with [`docs/architecture.md`](docs/architecture.md).

## License

[MIT](LICENSE)
