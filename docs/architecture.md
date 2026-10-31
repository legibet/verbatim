# Architecture

verbatim is an HTTP gateway between LLM API clients and LLM API providers. It picks an upstream for each request, fails over before a response is committed, keeps a session on one upstream, and logs usage and cost. It never converts between API formats.

## Principles

Every change is checked against these.

1. **Zero format conversion.** A request is forwarded in the protocol it arrived in, to an endpoint that speaks that protocol natively. There is no internal message representation.
2. **Pass through by default.** verbatim changes the authentication header, the model name, the destination URL, and whatever static overrides a provider declares. Everything else (unknown fields, dialect extensions, extra headers, query parameters, stream events) is forwarded byte for byte. An upstream response, success or error, reaches the client unchanged.
3. **No hidden behavior.** Every change to a request is declared in the configuration file. There are no heuristics on hostnames, error text, or model names. Nothing is retried after a response has been committed to the client.
4. **Deterministic per target.** For a given configuration, client request, and chosen target, the forwarded request is a pure function of those three. Which target is chosen can vary; session affinity keeps a conversation on one. This is what Anthropic prompt caching and preserved thinking need: preserved thinking binds thinking-block signatures to the exact request prefix (`system`, `tools`, earlier `messages`) and rejects a later turn when that prefix changes.
5. **Small.** One Go binary, one YAML file, one SQLite database, an embedded web UI.

## Why zero conversion works

- Three of the four major protocols carry opaque reasoning state that the client must echo back unchanged: Anthropic `signature` and `redacted_thinking.data`, Responses `encrypted_content`, Gemini `thoughtSignature`. Chat Completions has no standard field for it, and provider dialects each add their own (`reasoning_content`, OpenRouter `reasoning_details`). verbatim forwards these untouched, but no gateway can move such state from one provider to another.
- Anthropic preserved thinking rejects a request when anything before a thinking block has changed. A converter that merges system messages, generates tool-call ids, or reorders blocks breaks every multi-turn conversation.
- As of October 2026 nearly every major provider serves Anthropic Messages and Responses natively (DeepSeek, Moonshot, Zhipu, MiniMax, Alibaba, OpenRouter, xAI, vLLM, SGLang, llama.cpp, Ollama). Relays that expose only Chat Completions stay reachable by Chat Completions clients.

The accepted cost: a client that speaks protocol P can only use endpoints that speak P. When none is configured for the requested model, verbatim answers with an error in P's native format.

## Non-goals

- Format conversion of any kind.
- Per-user accounts, multiple client keys, quotas, billing, or a management UI. The web UI changes no configuration.
- Request rewriting beyond provider overrides ([forwarding.md](forwarding.md)).
- Load-aware or cost-aware scheduling. Routing uses static priority and weight.
- Responses resource operations (`GET /v1/responses/{id}`, cancel, delete, input items, `background` polling, `/responses/compact`) and the Responses WebSocket transport. They return 404. Response ownership ([affinity.md](affinity.md)) is the basis for adding them.
- A Gemini `generateContent` entry point. Gemini models work through Google's OpenAI-compatible API as a `chat` endpoint.

## Request lifecycle

```
client request
  → protocol and operation from the path (404 if unknown)
  → client authentication (401)
  → read body, extract model name
  → first route whose pattern matches the model and that has a target serving
    this protocol (404 no_route)
  → candidates = that route's targets for this protocol
  → previous_response_id / conversation: the owning target is the only candidate
  → affinity: look up or reserve a pin
  → attempt order: priority tiers, balance within a tier, pinned target first
  → for each candidate:
       breaker open at dispatch time: skip
       send; no response headers or a failover status: record, feed the breaker
         if the signal counts, next candidate
       otherwise: commit status and headers to the client, move the pin to this
         target, stream the body, stop
  → all failed: return the last attempt's upstream response unchanged, or 502
    if it got none
  → write the log row
```

Failover is decided on the upstream status line and headers, before anything reaches the client. A client disconnect cancels the loop; it is not a provider failure and starts no further attempt. Routing, failover, and the breaker are in [routing.md](routing.md); pins and ownership in [affinity.md](affinity.md).

## Packages

```
cmd/verbatim/          CLI commands
internal/config/       YAML schema, env expansion, validation
internal/protocol/     per protocol: paths and operations, model get/set, prefix for
                       hashing, usage parser, native error body
internal/route/        route matching, attempt order, circuit breaker
internal/affinity/     key rules, pin store
internal/proxy/        handler, attempt loop, usage tee, log rows
internal/store/        SQLite schema, writer, retention, queries for the CLI and UI
internal/pricing/      price sources, overrides, aliases, lookup, cost
internal/models/       provider model-list cache and the /v1/models listing
internal/ui/           /ui/api handlers and the embedded build
web/                   UI source, built into internal/ui/dist
```

`internal/protocol` is the only package that knows protocol-specific JSON paths. It answers a fixed set of questions about a body (its model, whether it streams, its hash prefix, its continuation id, its usage) and contains no translation logic.

## Forwarding machinery

`internal/proxy` handles the whole exchange itself, without `httputil.ReverseProxy`.

- **Attempt loop.** Each candidate is sent with its endpoint's `http.Transport` directly. Transports have compression disabled, a `ResponseHeaderTimeout` from the endpoint's `header_timeout`, and follow no redirects (a 3xx is forwarded as is); endpoints with the same `header_timeout` share one. verbatim neither adds nor strips `Idempotency-Key`, so Go's retry on a dead connection follows what the client sent.
- **Commit.** The first response that does not fail over is committed: its status and headers are written (hop-by-hop headers and those named in `Connection` dropped, trailers announced and written after the body), then the body is copied in chunks with a flush after each write while the usage tee observes it. A cut upstream aborts the client connection (`http.ErrAbortHandler`).
- **Idle timer.** It runs only while a read from the upstream is waiting, so a slow client does not count against it. On expiry the upstream body is closed and the client connection aborted, logged as outcome `truncated`. It also bounds reading a failed attempt's body.
- **Client cancel.** If the client leaves before anything is committed, the loop stops, the row is logged as 499 `client_cancelled`, and a pin this request reserved is released.

Every data-plane response carries `x-verbatim-request-id`, the id `verbatim log <request_id>` looks up.

## State

- **Configuration** is an immutable snapshot. A reload swaps it; running requests keep the one they started with.
- **Pins, breakers, round-robin counters** live in memory under mutexes. Pin reservation and half-open admission are compare-and-set under those mutexes. A restart loses them.
- **Request log** lives in SQLite, written off the request path by one writer goroutine through a bounded channel ([usage-and-pricing.md](usage-and-pricing.md)). **Response ownership** is in the same database but written synchronously on the request path ([affinity.md](affinity.md#response-ownership)).
- **Model lists and the price table** are caches, refreshed in the background.

Shutdown waits 30 s for running requests, then closes connections, gives handlers up to 5 s to record their rows, and drains the log writer.

## Configuration

One YAML file; [`verbatim.example.yaml`](../verbatim.example.yaml) annotates every field. `${VAR}` in any string is expanded at load time, and an unset variable is an error, never an empty string. A leading `~/` in `log.path`, `pricing.cache`, and `pricing.overrides` is the home directory. Unknown keys, references to undefined providers, duplicate route patterns, and invalid overrides are errors. Validation runs in stages (keys, variables, and durations; YAML types; semantics), and `verbatim check` lists every problem of the first stage that fails. Unknown keys are found by walking the schema, because yaml.v3's `KnownFields` is not available on `Node.Decode`.

The running server reads the file again on `SIGHUP` or on the web UI's Reload config. The file is not watched, because an editor can save a half-finished change that still validates. A reload applies the whole file or nothing: a file that fails validation is refused, and so is one that changes `server.listen`, `log.path`, `log.retention`, or `affinity.ttl`, which need a restart. The previous configuration then keeps serving and the reason is logged and shown. After a successful reload, pins and the in-flight count are kept (a session stays on its target wherever the new routes still offer it); breakers start closed, upstream transports and round-robin counters are new, model lists are fetched again, and the price table is reloaded with the new pricing settings.

## CLI

`usage` and `log` read the SQLite file directly and work while the server runs. `status` calls the running server's `/ui/api/providers`, because breakers and pins live in its memory. Commands other than `serve` and `check` load the configuration leniently: an unset `${VAR}` becomes empty and validation is skipped, so they run without provider keys.

`check --probe` sends each endpoint an empty JSON request (`{}`) at the path of its main operation (`create`, or `generate` for `images`), with the headers and key the gateway would send. Any failure makes `check` exit 1. No reply means the host, port, or scheme is wrong; 401 or 403 means the key is rejected; a redirect, 404, or non-JSON reply means `base_url` does not lead to the API. Anything else (typically 400 for missing fields) counts as ok. An ok is not proof: an upstream that answers every path under a prefix hides an extra path segment, and one that does not check keys hides a wrong key.

## Dependencies

Kept short: `tidwall/gjson` and `tidwall/sjson` for point access to JSON bodies, `modernc.org/sqlite` (pure Go, so the binary stays static), `gopkg.in/yaml.v3`, and `klauspost/compress/zstd` and `andybalholm/brotli` as decoders for the usage tee. Everything else is the standard library: `log/slog`, `net/http` pattern routing, `flag` with hand-written subcommand dispatch, `database/sql` with hand-written SQL and `user_version` migrations, `go:embed` for the UI.
