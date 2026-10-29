# verbatim: Design

Status: draft for implementation, revision 2. Date: 2026-10-15.

verbatim is a small, self-hosted gateway that sits between LLM API clients and LLM API providers. It routes requests to upstream providers, retries on failure, keeps a session on the same upstream, records usage, and does nothing else. It never converts between API formats.

## 1. Principles

1. **Zero format conversion.** A request that arrives in a protocol is forwarded in that protocol, to a provider that natively speaks it. verbatim does not translate Chat Completions to Responses, Anthropic Messages to anything, or anything to Anthropic Messages. There is no internal message representation.
2. **Pass through by default.** The data plane changes exactly three things in a request: the authentication header, the model name, and the destination URL. Everything else, including unknown fields, dialect extensions, extra headers, query parameters, and stream events, is forwarded byte for byte. An upstream response, success or error, is returned to the client unchanged, with its status code, headers, and body.
3. **No hidden behavior.** Every change verbatim makes to a request is declared in the configuration file. There are no heuristics based on hostnames, error text, or model names beyond what the configuration states. verbatim never retries after it has committed a response to the client.
4. **Deterministic per target.** For a given configuration, client request, and chosen target, the forwarded request is a pure function of those three. Which target is chosen may vary by selection and failover; session affinity exists so that an agent conversation keeps hitting the same target. This matters for Anthropic prompt caching and for Anthropic preserved thinking, which binds thinking-block signatures to the exact request prefix (`system`, `tools`, earlier `messages`) and rejects later turns when the prefix changes.
5. **Small.** One Go binary, one YAML file, one SQLite database, and an embedded web UI.

## 2. Non-goals

- Format conversion of any kind, including Chat Completions to or from Responses.
- Per-user accounts, multiple client keys with separate quotas, billing, or a management UI. The embedded web UI (section 14) changes no configuration.
- Prompt or response rewriting beyond the static overrides in section 9.
- Load-aware or cost-aware scheduling. Routing uses static priority and weight only.
- Responses resource operations other than creating a response: `GET /v1/responses/{id}`, cancel, delete, input item listing, `background: true` polling, compaction, and the Responses WebSocket transport. These are rejected with 404 in version 1 (section 4.4). The response ownership table (section 7.4) is the foundation for adding them later.
- A Gemini entry point (`generateContent`). No client verbatim serves speaks it, and Gemini models are reachable through Google's OpenAI-compatible API as a `chat` endpoint (`https://generativelanguage.googleapis.com/v1beta/openai`).

## 3. Why zero conversion is viable

Research notes are in `.agents/` and are not part of this document. The findings that justify the design:

- Three of the four major LLM API protocols carry opaque reasoning state that the client must echo back unchanged: Anthropic `signature` and `redacted_thinking.data`, Responses `encrypted_content`, Gemini `thoughtSignature`. The standard Chat Completions schema has no field for this. Provider dialects add non-standard ones (`reasoning_content`, OpenRouter `reasoning_details`, Gemini's `extra_content.google.thought_signature`); verbatim forwards them untouched because it forwards everything untouched, but no gateway can move such state from one provider to another.
- Anthropic's preserved thinking (Claude Fable 5.1, Opus 5.5, Sonnet 5.5, Haiku 5.5) rejects a request when anything before a thinking block has changed in value. A converter that merges system messages, generates tool-call ids, or reorders blocks breaks every multi-turn conversation.
- As of October 2026, almost every major provider offers Anthropic Messages and Responses endpoints natively (DeepSeek, Moonshot, Zhipu, MiniMax, Alibaba, OpenRouter, xAI, vLLM, SGLang, llama.cpp, Ollama). The only upstreams without a native path are third-party relays that expose Chat Completions alone, and those are reachable by Chat Completions clients.

The consequence is accepted: a client that speaks protocol P can only use providers that speak P. When no such provider is configured for the requested model, verbatim returns an error in P's native error format.

## 4. Protocols and entry points

verbatim identifies the inbound protocol from the request path. It does not inspect the client's identity or user agent.

| Protocol id | Operation | Path | Model location |
|---|---|---|---|
| `chat` | `create` | `POST /v1/chat/completions` | body `model` |
| `responses` | `create` | `POST /v1/responses` | body `model` |
| `responses` | `search` | `POST /v1/alpha/search` | body `model` |
| `anthropic` | `create` | `POST /v1/messages` | body `model` |
| `anthropic` | `count_tokens` | `POST /v1/messages/count_tokens` | body `model` |
| `images` | `generate` | `POST /v1/images/generations` | body `model` |
| `images` | `edit` | `POST /v1/images/edits` | body `model`, JSON or a multipart form field |

`search` is Codex's standalone web search. Models that Codex runs without hosted tools search through a client-side `web.run` function, and Codex posts each call to `{base_url}/alpha/search` when the provider sets `supports_standalone_web_search`. It is a plain JSON request and response; the body carries the turn's `model` and the Codex session id as `id`, and the response reports no usage.

`images` is the OpenAI Images API. It is a protocol of its own, like `chat` and `responses`, because whether an upstream serves it is a fact about that upstream, independent of whether it serves chat or Responses at the same base URL. An image model is routed like any other model, by the body `model`; Codex's image tool, for example, sends `gpt-image-2` to `{base_url}/images/generations` and `/images/edits` as JSON. Edits from the OpenAI SDK are `multipart/form-data`, with the model in a form field. Both endpoints are synchronous: the image comes back in the response (as `b64_json`, often megabytes), or as server-sent events (`image_generation.partial_image`, `image_generation.completed`, and the `image_edit.*` equivalents) when the client sets `stream`. The response reports token usage, which section 12.3 prices.

The operation matters for affinity (section 7.3), failover (section 8), usage (section 11.3), and the protocol-specific suffix appended to the endpoint's `base_url`. `count_tokens` and `search` are auxiliary operations: they serve the create requests of a session, follow a pin but never create or move one, never feed the breaker, and are neither priced nor aggregated as usage. Every other operation (`create`, `generate`, `edit`) is priced, creates and moves pins, and feeds the breaker. The client's query string is preserved on the upstream URL. Claude Code, for example, sends `/v1/messages?beta=true`; the `?beta=true` reaches the provider.

### 4.1 Client authentication

If `server.api_key` is set, a request must present it in `Authorization: Bearer <key>` or `x-api-key: <key>`. Either match is sufficient; a request may carry both. If `server.api_key` is unset, all requests are accepted; the configuration validator prints a warning unless `server.listen` is a loopback address.

### 4.2 Request forwarding

For a forwarded request, verbatim:

1. Reads the full client body into memory. The body is needed for retries, for the model rewrite, and for the affinity key. Requests above `server.max_body_bytes` (default 32 MiB) are rejected with 413. `server.max_inflight` (default 64) bounds the number of concurrent requests and therefore total buffered memory; excess requests get 503 in native format.
2. For each attempt, starts from the original client body and produces the attempt's body: the model name is rewritten in place with a JSON point update (`sjson`-style), then the provider's static overrides (section 9) are applied as point updates. For a `multipart/form-data` body (`images` edits) the bytes of the `model` field's value are replaced and no other byte changes; the form is never re-encoded, so file parts, field order, and part headers reach the provider as sent. Deserializing the body into a struct is never done, so unknown fields survive.
3. Builds the upstream URL as the `base_url` of the provider's endpoint for this protocol + the operation's fixed suffix + the client's query string. `base_url` is the base URL the protocol's official SDK takes, which is what providers document and what users already put in `OPENAI_BASE_URL` or `ANTHROPIC_BASE_URL`; the suffix completes it the way that SDK does. For `chat`, `responses`, and `images` the SDK base URL carries the version (`https://api.deepseek.com/v1`) and the suffixes are `/chat/completions`, `/responses`, `/alpha/search`, `/images/generations`, `/images/edits`, and `/models`. For `anthropic` it does not (`https://api.anthropic.com`, `https://api.deepseek.com/anthropic`, `https://openrouter.ai/api`) and the suffixes are `/v1/messages`, `/v1/messages/count_tokens`, and `/v1/models`.
4. Copies client headers, except hop-by-hop headers (including those named by `Connection`), `Host`, `Content-Length`, and every client authentication carrier (`Authorization`, `x-api-key`). Applies the provider's `headers` map (section 6.2): a string value sets the header, `null` removes it. Then sets the provider's key in the authentication header that the endpoint's `auth` names; it always wins over `headers`. For `anthropic`, if the client did not send `anthropic-version`, verbatim sets `anthropic-version: 2023-06-01`, because the Anthropic API rejects requests without it. This is the only header verbatim adds on its own, and a provider's `headers` can override it.
5. Sends the request and streams the response back.

### 4.3 Response forwarding

Response status, headers (except hop-by-hop), trailers, and body are copied to the client unchanged. The transport is configured not to request or transparently decompress content encodings, so what the provider sends is what the client gets. Server-sent events are copied as they arrive, with a flush after every write. verbatim observes the response body through a bounded tee that extracts usage and the response id for the log (section 11.3); the tee never modifies what the client receives, and a parse failure in the tee never affects forwarding. When the upstream response carries `Content-Encoding` `gzip`, `deflate`, `br`, or `zstd`, the tee decompresses its own copy; the client still receives the compressed bytes. All four matter in practice: clients advertise them (Node's fetch sends `gzip, deflate, br, zstd`) and providers use them for streams (DeepSeek answers that header with `zstd`), and the tee is also where the response id that Responses ownership needs is read. Other encodings, and stacked encodings, are not inspected and yield null usage.

The tee reads the body as server-sent events when the response `Content-Type` is `text/event-stream`, as one JSON document otherwise, and by the request's `stream` flag when the response has no `Content-Type`; an upstream may answer a stream request with one JSON body, as magpie does for images. In its own copy the tee replaces the content of every JSON string longer than 4 KiB with nothing, so a body or an event carrying an image is parsed in bounded memory; usage, ids, and event types are short. A non-streamed body is parsed when it ends, and one that still exceeds 8 MiB yields null usage; a single event above 1 MiB is skipped.

The point of no return is when verbatim commits the upstream status and headers to the client. After that, nothing is retried: a cut stream is forwarded as a cut stream and the client connection is aborted rather than closed cleanly, so the client's SDK sees an error instead of a truncated success. An error event inside a `200` stream is forwarded as is.

verbatim does not add `stream_options.include_usage` to Chat Completions requests. If the upstream stream carries no usage, the log row records null token counts.

### 4.4 Other paths

| Path | Behavior |
|---|---|
| `GET /v1/models` | Served by verbatim from the configuration and provider discovery, section 6.5 |
| `GET /ui/`, `GET /ui/api/*` | Embedded web UI, section 14 |
| `GET /healthz` | 200 with `{"ok":true}` |
| `GET /v1/responses/{id}`, `POST /v1/responses/{id}/cancel`, `DELETE /v1/responses/{id}`, `GET /v1/responses/{id}/input_items`, `POST /v1/responses/compact`, WebSocket upgrades | 404 in Responses error format with code `unsupported_operation` |
| anything else | 404 with a plain JSON error |

## 5. Request lifecycle

```
client request
  → identify protocol and operation from path (404 if unknown)
  → authenticate client (401 if required and missing)
  → read body, extract model name
  → match route: first route whose pattern matches the model AND that has at least
    one target serving this protocol (404 native if none, section 6.3)
  → candidates = that route's targets filtered to this protocol
  → ownership: if the body carries previous_response_id (or conversation), look up
    the owning target; it is the only candidate (section 7.4)
  → affinity: look up or reserve a pin for this session (section 7)
  → build attempt order (section 6.4)
  → for each candidate:
       if its endpoint's breaker is open at dispatch time: skip, record
       build attempt body and URL; send
       if no response headers (connect error, header timeout) or a failover-eligible
         status (section 8.1): record attempt, feed the breaker, move the pin, continue
       else: commit status and headers to the client; forward body; stop
  → if every candidate failed:
       if the last attempt produced an upstream response: return it unchanged
       else: 502 in native error format listing the attempts
  → record outcome (completed, upstream_error_event, truncated, client_cancelled)
  → write log row (section 11)
```

Failover is decided on the upstream status line and headers, before anything is committed to the client. Client disconnection during the attempt loop cancels the loop; it does not count as a provider failure and does not trigger another attempt.

## 6. Configuration

One YAML file. `${ENV_VAR}` in any string value is expanded from the environment at load time; an unset variable is a validation error, never an empty string. A leading `~/` in a path is the user's home directory. The file is validated at startup; any reference to an undefined provider, duplicate route pattern, unknown field, or invalid override is a startup error. The running server reads it again on request (section 6.6).

### 6.1 Full example

```yaml
server:
  listen: 127.0.0.1:8080
  api_key: ${VERBATIM_KEY}          # optional on loopback; required elsewhere
  max_body_bytes: 33554432
  max_inflight: 64
  header_timeout: 60s               # upstream response headers
  idle_timeout: 120s                # upstream body inactivity, also used for error bodies

providers:
  openai:
    key: ${OPENAI_KEY}
    endpoints:
      responses: {base_url: https://api.openai.com/v1}
      chat: {base_url: https://api.openai.com/v1}
      images:
        base_url: https://api.openai.com/v1
        header_timeout: 5m           # a non-streamed image is answered once it is drawn
  moonshot:
    key: ${MOONSHOT_KEY}
    endpoints:
      anthropic: {base_url: https://api.moonshot.ai/anthropic}
      chat: {base_url: https://api.moonshot.ai/v1}
  openrouter:
    key: ${OPENROUTER_KEY}
    endpoints:
      chat: {base_url: https://openrouter.ai/api/v1}
      anthropic: {base_url: https://openrouter.ai/api}
  relay-a:
    key: ${RELAY_A_KEY}
    headers:
      x-title: verbatim
      openai-project: null           # drop a client header this relay rejects
    overrides:
      - delete: metadata
      - set: {path: temperature, value: 1}
    endpoints:
      chat: {base_url: https://relay.example/v1}
  local-llama:
    endpoints:
      chat: {base_url: http://127.0.0.1:8081/v1, auth: none}

routes:
  - model: kimi-k3
    targets:
      - provider: moonshot           # its anthropic and chat endpoints
        weight: 3                    # for chat, 3 of 4 requests go here, 1 to relay-a
      - provider: relay-a
        model: kimi-k3-0905
      - provider: openrouter.anthropic   # only OpenRouter's Messages endpoint
        model: moonshotai/kimi-k3
        priority: 2
  - model: "gpt-*"
    targets:
      - provider: openai             # responses, chat, and images (gpt-image-*)
      - provider: openrouter.chat    # when OpenAI's chat endpoint fails:
        model: "openai/gpt-*"        # gpt-6.1-sol is sent as openai/gpt-6.1-sol
        priority: 2
  - model: "*"
    discover: false
    targets:
      - provider: openrouter
      - provider: local-llama        # chat only, when OpenRouter's chat endpoint fails
        priority: 2

affinity:
  ttl: 1h
  mode: prefer
  rules:
    - match: {protocol: anthropic}
      key: [{body: metadata.user_id}, {header: x-session-affinity}]
    - match: {protocol: responses}
      key: [{header: session-id}, {body: prompt_cache_key}, {body: id}]
    - match: {protocol: chat}
      key: [{header: x-session-affinity}, {body: prompt_cache_key}, {body: user}]
  fallback: prefix_hash

breaker:
  failure_threshold: 3
  cooldown: 30s

models:
  cache_ttl: 1h
  timeout: 10s

log:
  path: ~/.verbatim/verbatim.db
  retention: 90d
  error_excerpt_bytes: 4096

pricing:
  sources:
    - https://basellm.github.io/llm-metadata/api/providers/anthropic.json
    - https://basellm.github.io/llm-metadata/api/providers/openai.json
    - https://basellm.github.io/llm-metadata/api/providers/moonshotai.json
  cache: ~/.verbatim/pricing/
  overrides: ~/.verbatim/pricing-overrides.json
  aliases:
    claude-opus-5-5-20260301: claude-opus-5-5
    kimi-k3: kimi-k3-0905
```

### 6.2 `providers`

A provider is one upstream service, such as a vendor's API or a relay. It has one key, one set of headers, and one list of overrides, and serves each protocol listed under `endpoints`. A service that offers several protocols is one provider with several endpoints; a provider with one protocol has one endpoint. When two uses of one service need different overrides or keys, they are two providers.

| Field | Required | Meaning |
|---|---|---|
| `key` | unless every endpoint has `auth: none` | Provider API key |
| `headers` | no | Map of header name to string (set) or `null` (remove), applied after client headers are copied and before the endpoint's `auth` |
| `overrides` | no | Ordered list of static request edits applied to every request the provider receives, section 9; not allowed on a provider with an `images` endpoint |
| `failover_on` | no | List of 4xx or 5xx statuses that fail over from this provider in addition to the built-in ones, section 8.1 |
| `endpoints` | yes | Map from protocol (`chat`, `responses`, `anthropic`, `images`) to the endpoint fields below; at least one |

Endpoint fields:

| Field | Required | Meaning |
|---|---|---|
| `base_url` | yes | The base URL the protocol's official SDK takes; the operation's fixed suffix is appended (section 4.2) |
| `auth` | no | `bearer` (`Authorization: Bearer <key>`), `x-api-key`, or `none`. Default by protocol: `chat`, `responses`, and `images` use `bearer`, `anthropic` uses `x-api-key`. It belongs to the endpoint because the default differs by protocol: one setting for the whole provider would replace the default of every other protocol it serves. |
| `header_timeout` | no | Wait for this endpoint's response headers; default `server.header_timeout`. A non-streamed image is answered only once it is drawn, so an `images` endpoint usually needs minutes, while a chat endpoint that long silent should fail over. Endpoints with the same value share one connection pool. |

An endpoint is named `provider.protocol` (`openrouter.anthropic`), so a provider name cannot contain `.`. Circuit breakers (section 8.3), model lists (section 6.5), and the counters of the Providers view (section 14) are kept per endpoint: one protocol of a service failing, as a relay's image channel often does alone, does not stop the others. The request log records the provider, and its `protocol` column tells the endpoint.

### 6.3 `routes`

An ordered list. A request matches the first route whose `model` pattern matches the client's model name **and** which has at least one target serving the inbound protocol. A route that matches the name but has no target for the protocol is skipped, so a later route can still serve the request. If no route qualifies, verbatim returns 404 in native format with code `no_route`.

Patterns are globs with `*` as the only wildcard, matched case-sensitively against the whole name; `*` matches any run of characters including `/`, so `anthropic/*` works for OpenRouter-style names (the standard library's `path.Match` stops at `/` and is not used). Put exact names first, patterns after, and `"*"` last if a catch-all is wanted. A route's `model` pattern is its identity and must be unique in the list.

Each route has a flat `targets` list; the route itself is protocol-agnostic. A target names a provider, and serves every protocol that provider has an endpoint for, or names one endpoint as `provider.protocol` and serves that protocol only. Naming the endpoint is how a route uses part of a service: a `gpt-*` route that should reach a relay only through Responses names `relay.responses`, and an earlier route listing a whole provider can take requests of protocols a later route was meant to serve. Within a protocol, a target is identified by the pair (provider, upstream model name); two targets of one route that send the same model to the same endpoint are a configuration error.

| Target field | Default | Meaning |
|---|---|---|
| `provider` | required | Provider name, or `provider.protocol` for one of its endpoints |
| `model` | client's model name | Upstream model name. Sent as written, except that a `*` is replaced with the text the route pattern's `*` matched. If absent, the client's name is forwarded unchanged. |
| `priority` | 1 | Lower number is tried first |
| `weight` | 1 | Relative share within a priority tier |

Route-level fields:

| Field | Default | Meaning |
|---|---|---|
| `model` | required | Glob pattern and route identity |
| `targets` | required | At least one |
| `balance` | `weighted` | `weighted` (random by weight within a tier) or `round_robin` (rotate within a tier, weights ignored) |
| `discover` | `true` | For pattern routes: populate the listing endpoints from the provider endpoints' own model lists, section 6.5. Ignored on exact-name routes. |

A `*` in a target's `model` covers services that put a fixed prefix or suffix around the names the client uses: under the route `gpt-*`, `model: openai/gpt-*` sends `gpt-6.1-sol` as `openai/gpt-6.1-sol`. The target `model` may contain at most one `*`, and only on a route whose pattern contains exactly one, so the `*` always stands for one well-defined piece of the name; anything else is a configuration error. Names that differ in another way (OpenRouter's `anthropic/claude-opus-5.5` for `claude-opus-5-5`) get an exact-name route with the upstream name written out. There is no provider-level model name mapping and no regular-expression rewriting.

### 6.4 Selection and attempt order

Given the candidates (the matched route's targets filtered to the inbound protocol):

1. If ownership (section 7.4) names a target, the order is that single target.
2. Otherwise, group by `priority`, ascending. Within each tier, order by `balance`: `weighted` draws without replacement with probability proportional to `weight`; `round_robin` rotates from a counter kept per (route, protocol, tier). Concatenate the tiers.
3. If affinity (section 7) yields a pinned target that is among the candidates, move it to the front.
4. Attempt the list in order. The breaker is checked at dispatch time for each attempt, not when the list is built; a target whose endpoint's breaker is open is skipped and recorded in `routing_json`. If every candidate is skipped, the request fails immediately with 502 `all_upstreams_open`; there is no bypass, because the half-open probe (section 8.3) is what gives a recovering provider its chance.

Each target is attempted at most once per request. A tier is exhausted before the next tier is touched. This differs from new-api, which steps down one priority tier per retry and ends up retrying the last tier repeatedly.

### 6.5 Model listing

Many clients populate their model picker from `/v1/models`, so the list has to be real. It is assembled per route, in configuration order:

- An exact-name route contributes its own `model`. Providers are not consulted.
- A pattern route with `discover: true` (the default) contributes the client names behind the model ids its targets' endpoints list. For a target without `model`, a listed id that matches the pattern is the name itself. For a target whose `model` has a `*`, a listed id that matches that `model` gives the name by putting what the `*` matched in place of the pattern's `*`: under `gpt-*` with `model: openai/gpt-*`, the listed `openai/gpt-7` becomes `gpt-7`. Targets with a `model` without `*` are skipped, because such a name says nothing about which client names lead to it. Each discovered name is then resolved through the routing rules of section 6.3 for that endpoint's protocol, and is kept only if routing would send it to this route; otherwise an earlier route owns the name and the entry is dropped.
- A pattern route with `discover: false` contributes nothing.

Names are deduplicated; the first occurrence wins. The response lists names only; it does not promise that every listed name works for every protocol.

Each endpoint's list comes from `GET {base_url}` + the protocol's model-list suffix (section 4.2). `anthropic` pages the list, and verbatim asks for the largest page it allows (`limit=1000`) and follows the next-page cursor up to 10 pages: `has_more` and `last_id` sent back as `after_id`, for `anthropic` and for OpenAI-compatible relays that page. Lists are cached per endpoint for `models.cache_ttl`, warmed in the background at startup and after a configuration reload, and refreshed in the background when stale. A fetch has a `models.timeout`; on failure the previous list is kept and the fetch is retried every minute (every `models.cache_ttl` if that is shorter). A warning is logged when an endpoint's fetches start failing, not on every retry, and an info line when one succeeds again, so an endpoint that has no model list (DeepSeek's and Bailian's Anthropic-compatible endpoints return 404) warns once per start or reload. The web UI's Models view can also fetch every list at once (section 14). The client-facing endpoints read the cache and never wait on a refresh; only a request that arrives while the first round of fetches is still running waits for that round (bounded by `models.timeout`), so a client that lists models right after startup or a reload does not get an empty list.

Response shape: one entry per name carrying the union of what OpenAI and Anthropic clients read, so either parser is satisfied and ignores the rest:

```json
{"object":"list","data":[{"id":"kimi-k3","object":"model","created":0,"owned_by":"verbatim",
                          "type":"model","display_name":"kimi-k3","created_at":"1970-01-01T00:00:00Z"}],
 "has_more":false,"first_id":"kimi-k3","last_id":"kimi-k3"}
```

### 6.6 Reloading

The running server reads the file again on `SIGHUP` or on the Providers view's Reload config action (section 14). The file is not watched: an editor can save a half-finished change that still validates.

A reload applies the whole file or nothing. A file that fails validation is refused, and so is one that changes `server.listen`, `log.path`, `log.retention`, or `affinity.ttl`, which only a restart applies; the previous configuration keeps serving and the reason is logged and shown. Otherwise new requests are served with the new configuration at once, and requests already running finish under the one they started with. Pins are kept, so a session stays on its target wherever the new routes still offer it (section 7.2). Circuit breakers start closed, model lists are fetched again, and the price table is reloaded from `pricing.cache` with the new pricing settings.

## 7. Session affinity

Agent sessions must stay on one upstream. Anthropic thinking signatures are validated by the issuer and, on current models, bound to the issuing account; prompt caches live on one upstream; Responses server-side state lives on one upstream. Affinity is applied before selection and overrides priority and weight.

verbatim pins its immediate upstream. If that upstream is itself a router (OpenRouter balances across backends), continuity below it is the upstream's business.

### 7.1 Key extraction

Which request field identifies a session is a property of the client, so it is configuration, not code. `affinity.rules` is an ordered list; the first rule whose `match` fits the request is used. `match` has optional `protocol` and `model` (glob). `key` is an ordered list of sources, and the first source that yields a non-empty string is the key:

- `{body: <gjson path>}`: a string or number at that path in the request body, taken as sent.
- `{header: <name>}`: a request header, case-insensitive.

verbatim ships no built-in rules. The example configuration carries the rules that match today's common clients, and they are ordinary configuration the user can change: Claude Code sends `metadata.user_id`; Codex sends a `session-id` header and `prompt_cache_key` with the same value, and its `/alpha/search` carries that value as `id`, so one rule keeps a search on the session's upstream; Pi and omp can send `x-session-affinity`; OpenAI SDK users often set `user` or `prompt_cache_key`. Where a client offers both a header and a body field, the header is listed first because it never participates in prompt caching or thinking-prefix checks. These values identify a client session, not necessarily one conversation; two conversations under one `user` share a pin, which costs nothing but balance.

If no rule matches or no source yields a value, `affinity.fallback` applies:

- `prefix_hash` (default): SHA-256 over the protocol id, the system prompt field, and the first user-role message, each taken as the exact bytes the client sent: `anthropic` uses `system` and the first `messages[]` entry with `role: user`; `chat` uses the first message with role `system` or `developer` and the first with role `user`; `responses` uses `instructions` and the first `input[]` item with `role: user` (or `input` itself when it is a string). If no user-role message exists the request gets no affinity. This is best-effort stickiness: two conversations with the same opening share a pin, and a client that rewrites its opening mid-conversation (context compaction, a changing system prompt) moves to a fresh key. Clients that need guarantees should send a session identifier and name it in a rule.
- `none`: no affinity for this request.

### 7.2 Pin identity and lifetime

The pin key is (protocol, affinity key, client model name). One session usually touches several models, and each must be pinned independently; the same key under two protocols has two independent pins because the candidate sets differ. The pin value is a target identity (provider, upstream model).

The pin is **reserved at selection time**, not after success: when a request finds no pin, it reserves one for the target it is about to attempt, atomically against concurrent requests for the same key, so two parallel first requests of a session (agent clients routinely fire several at start) land on the same target. If the attempt fails over, the pin moves to the target that eventually gets committed. If the request fails entirely and no request has yet committed a response under this pin, the reservation is released; a pin that has served a committed response stays.

Pins live in an in-process map with LRU eviction and `affinity.ttl` expiry, refreshed on every use. They survive a configuration reload (section 6.6) but not a restart; after a restart, the first request of a session re-pins by ordinary selection, and the client handles any resulting signature error (Claude Code strips earlier thinking and retries; other clients fail the turn). `affinity.ttl` should be at least as long as the longest pause within a working session.

### 7.3 Modes

- `prefer` (default): if the pinned target is skipped (open breaker) or fails, selection continues with the remaining candidates and the pin moves to whichever target gets committed. `affinity_moved` is set on the log row. The client may receive a signature or state error from the new target on this very request, and verbatim does not hide it.
- `strict`: if the pinned target is skipped or fails, verbatim returns that failure (the upstream response unchanged, or 502 native if there was none) and tries nothing else. An expired or evicted pin makes the session new, and `strict` then behaves like a first request.

`count_tokens` and `search` follow an existing pin and never create or move one. A search that fails over does not take the conversation away from the upstream holding its prompt cache.

### 7.4 Response ownership (Responses)

A Responses client may continue a conversation by reference instead of by replaying it: `previous_response_id`, or a `conversation` id. The referenced state exists only on the upstream that produced it, and a prefix hash or session key cannot know which one that was, because the id is new on every turn. verbatim therefore records every response id it sees, taken from the committed response body (`id` of the response object; for streams, from the first event that carries the response object), together with the target that produced it, in the SQLite table `owned_responses(id, provider, upstream_model, ts)`. When a request carries `previous_response_id` or `conversation`, the owner is the only candidate (section 6.4 step 1); if the id is unknown, verbatim returns 404 in Responses format with code `unknown_response_id` rather than guess. Ownership rows follow `log.retention`.

This is state, and it is the one place verbatim keeps state beyond a cache. It is unavoidable for any gateway that serves more than one Responses upstream.

## 8. Failover and circuit breaking

### 8.1 Failover eligibility

Before anything is committed to the client, the following end the current attempt and move to the next candidate:

- No response headers: connection, TLS, or DNS failure; the endpoint's `header_timeout` elapsed.
- Upstream status 408, 409, 425, 429, 500, 502, 503, 504, 529.
- Upstream status 401 or 403: the provider key is invalid, exhausted, or not entitled to this model. Logged at warning level because it needs an operator.
- Upstream status 404: the provider does not serve this model or operation. Logged at warning level.

Every other status is returned to the client unchanged and ends the attempt loop. This includes all 400s (malformed request, Anthropic thinking signature errors, spend limits) and 402, 413, 422: retrying the same body elsewhere would either fail identically or hide the real error. The client owns its history and is the only party that can repair it.

A provider's `failover_on` adds statuses that fail over from that provider, for every operation of every endpoint it has. It is for services that report a missing model with a status that also means a bad request: Alibaba Bailian answers a model it does not serve with 400, where other services use 404. Only the status is matched, never the error text, so every listed status from that provider fails over, a malformed request included; that request then fails at the remaining candidates and the last one's response is returned (section 8.4). A listed status is handled like a 404: it moves the pin in `prefer` mode and does not feed the breaker (section 8.3). The response itself is never changed.

The body of an abandoned attempt is read (bounded by `server.idle_timeout` and 1 MiB) so that it can be returned to the client if this turns out to be the last attempt (section 8.4), and its first `log.error_excerpt_bytes` are kept in `attempt_log`.

### 8.2 Never failover

- Anything after status and headers have been committed to the client.
- Error objects inside a `200` streamed body. They are forwarded, and recorded as outcome `upstream_error_event` (section 11.2), but they do not touch the breaker in version 1.
- Any `count_tokens` or `search` failure affecting the pin (section 7.3).

### 8.3 Circuit breaker

Per provider endpoint (section 6.2). Only signals that indicate the endpoint as a whole is unhealthy feed the breaker: no response headers, and the built-in failover statuses 429, 500, 502, 503, 504, 529. A 401, 403, or 404, or a status that fails over only through `failover_on`, permits failover for this request but does not count, because it is as likely to be about one model, one key entitlement, or one operation (Ollama serves Messages but not `count_tokens`; most relays do not serve `search`) as about the endpoint. `count_tokens` and `search` attempts never feed the breaker, whatever their status.

`breaker.failure_threshold` consecutive counted failures open the breaker for `breaker.cooldown`. While open, the endpoint is skipped at dispatch (section 6.4 step 4). After the cooldown exactly one request is admitted (half-open, admission is atomic); a committed response closes the breaker, a counted failure reopens it for another cooldown. Any committed response resets the consecutive counter.

### 8.4 Exhaustion

When every candidate has failed or been skipped:

- If the last attempt received an upstream response, that response is returned to the client **unchanged** (status, headers, body), consistent with principle 2. verbatim adds one header, `x-verbatim-attempts: <n>`, and the log row explains the sequence. A route with a single provider that returns 401 therefore shows the client a 401 with the provider's own message, not a synthetic 502.
- If no attempt received a response (all connection failures or timeouts, or every endpoint open), verbatim responds 502 in the inbound protocol's native error format (section 10) with a message listing each attempt as `provider: error`.

## 9. Request overrides

A provider may declare static edits applied to every request it receives, whatever the protocol or operation, as an ordered list of operations:

```yaml
overrides:
  - delete: metadata
  - delete: service_tier
  - set: {path: temperature, value: 1}
```

Paths are gjson/sjson paths into the request body. Operations run in the listed order, each attempt starting from the original client body, so one provider's edits never leak into another provider's attempt. Values are literals; there is no templating, no conditionals, and no access to the request. These edits are the only body modification verbatim performs besides the model name.

The validator rejects:

- `model` and `stream` at any depth of a `set` or `delete`, because they drive routing and the usage parser.
- Any path under `system`, `tools`, or `messages` on a provider with an `anthropic` endpoint, because editing them would change the preserved-thinking prefix. Overrides elsewhere in the body are static and therefore identical on every turn.
- Two operations whose paths overlap (one is a prefix of the other), to keep the result independent of ordering mistakes.
- Any override on a provider with an `images` endpoint: an image request may be a multipart form, which these JSON edits cannot address, and an override that applies to some requests of a provider and not others would be hidden behavior. Such a provider's image endpoint goes in a provider of its own.

Overrides exist for service quirks (a relay that rejects an unknown field, or demands a fixed parameter, or OpenRouter's `provider.quantizations`). They are not a conversion mechanism. Note that an override cannot be conditional on the request, so an edit that is only valid for streaming requests (such as `stream_options`) cannot be expressed; that is deliberate.

## 10. Errors produced by verbatim

Errors that verbatim itself generates use the inbound protocol's native shape so SDKs parse them:

| Protocol | Body |
|---|---|
| `chat`, `responses` | `{"error":{"message":"...","type":"verbatim_error","code":"<code>"}}` |
| `anthropic` | `{"type":"error","error":{"type":"api_error","message":"<code>: ..."}}` |

Codes and statuses: `unauthenticated` 401; `no_model` 400 (the body has no string `model`); `no_route` 404; `unknown_response_id` 404; `unsupported_operation` 404; `body_too_large` 413; `too_many_inflight` 503; `all_upstreams_failed` 502; `all_upstreams_open` 502; `internal_error` 500 (the ownership lookup itself failed). Every verbatim-generated error carries the header `x-verbatim-error: <code>`, so it can be told apart from a forwarded upstream error, which never carries it.

## 11. Logging and usage

### 11.1 Storage

One SQLite database (`log.path`), WAL mode, tables `requests` and `owned_responses`. Rows are written after the response completes (or fails), off the request path, through a bounded channel to a single writer goroutine. Requests rejected by client authentication (section 4.1) are not logged, so an unauthenticated client cannot fill the database. If the channel is full the row is dropped and a `log_dropped` counter is incremented and shown in the UI; the request itself is never delayed by logging. On shutdown the channel is drained before exit. Rows older than `log.retention` are deleted hourly.

### 11.2 Row schema

| Column | Type | Meaning |
|---|---|---|
| `id` | integer | autoincrement |
| `ts` | integer | request start, unix milliseconds |
| `request_id` | text | verbatim-generated, also returned as `x-verbatim-request-id` |
| `upstream_request_id` | text | `x-request-id` or `request-id` from the committed upstream response |
| `protocol` | text | `chat`, `responses`, `anthropic`, `images` |
| `operation` | text | `create`, `count_tokens`, `search`, `generate`, or `edit` |
| `client_model` | text | model name as the client sent it |
| `route` | text | matched route pattern |
| `provider` | text | provider of the committed response, or of the last attempt |
| `upstream_model` | text | model name sent upstream |
| `status` | integer | HTTP status returned to the client; 499 when the client left before anything was sent |
| `outcome` | text | `completed`, `upstream_error_event`, `truncated`, `client_cancelled`, `gateway_error` |
| `stream` | integer | 1 if the client requested streaming |
| `attempts` | integer | number of upstream attempts |
| `attempts_ms` | integer | time spent in failed attempts before the committed one |
| `ttfb_ms` | integer | request start to committed upstream headers (includes `attempts_ms`) |
| `total_ms` | integer | request start to last byte sent or abort |
| `routing_json` | text | matched route, candidates, computed order, ownership target, pin state (`none`, `hit`, `reserved`, `moved`), providers skipped by open breakers |
| `attempt_log` | text | JSON array of `{provider, upstream_model, status or error, ms, excerpt}` for every attempt |
| `affinity_key` | text | SHA-256 of the key, or null |
| `input_tokens` | integer | uncached input tokens, normalized (11.3) |
| `cache_read_tokens` | integer | |
| `cache_write_tokens` | integer | all cache writes |
| `cache_write_1h_tokens` | integer | the subset written with a 1-hour TTL (Anthropic only) |
| `output_tokens` | integer | includes reasoning tokens |
| `reasoning_tokens` | integer | subset of output, when reported |
| `input_image_tokens` | integer | image tokens within `input_tokens` (`images` only) |
| `output_image_tokens` | integer | image tokens within `output_tokens` (`images` only) |
| `usage_json` | text | the provider's usage object as received, unmodified |
| `cost_usd` | real | computed at write time; null when no price or when usage is incomplete |
| `price_key` | text | the price-table entry found; set also when the usage has tokens that entry does not price (`cost_usd` null) |
| `price_json` | text | the unit prices used, so history stays stable when the price table changes |
| `error` | text | up to `log.error_excerpt_bytes` of the final error body, or verbatim's own message |

Request and response bodies are not stored. `count_tokens` rows carry `input_tokens` from the response; `search` rows carry no usage. Both are excluded from usage aggregates and cost.

### 11.3 Usage extraction

The tee observes the committed response body and keeps the last usage object seen. Field locations per protocol:

| Protocol | Non-stream | Stream | Fields |
|---|---|---|---|
| `chat` | `usage` | the last chunk carrying a non-null `usage` (it may have `choices: []` and precedes `[DONE]`) | `prompt_tokens`, `completion_tokens`, `prompt_tokens_details.cached_tokens`, `prompt_tokens_details.cache_write_tokens`, `completion_tokens_details.reasoning_tokens`; DeepSeek dialect `prompt_cache_hit_tokens` |
| `responses` | `usage` | `response.completed`, `response.incomplete`, or `response.failed`, whichever terminal event arrives, `response.usage` | `input_tokens`, `output_tokens`, `input_tokens_details.cached_tokens`, `input_tokens_details.cache_write_tokens`, `output_tokens_details.reasoning_tokens` |
| `anthropic` | `usage` | `message_start` (`message.usage`) merged with `message_delta` (`usage`); delta values are cumulative, so later values replace earlier ones | `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `cache_creation.ephemeral_5m_input_tokens`, `cache_creation.ephemeral_1h_input_tokens` |
| `images` | `usage` | `image_generation.completed` or `image_edit.completed`, `usage` | `input_tokens`, `output_tokens`, `input_tokens_details.image_tokens`, `output_tokens_details.image_tokens` |
| `count_tokens` | top-level `input_tokens` | n/a | |

Normalization into the row:

- `anthropic`: `input_tokens` is already uncached. `cache_write_tokens` = `cache_creation_input_tokens`; `cache_write_1h_tokens` = `cache_creation.ephemeral_1h_input_tokens` when present, else null.
- `chat`, `responses`: the reported prompt count includes cached and cache-written tokens. `input_tokens` = prompt count minus `cached_tokens` minus `cache_write_tokens`, each treated as 0 only when the provider reports the details object at all; if the details object is absent, `input_tokens` holds the full prompt count and the cache columns are null. OpenAI reports `cache_write_tokens` for GPT-5.6 and later; its cache lifetime is 30 minutes and has no 1-hour variant, so `cache_write_1h_tokens` is null.
- `images`: `input_tokens` and `output_tokens` as reported (the Images API reports no cache tokens); `input_image_tokens` and `output_image_tokens` from the details objects. Some relays (magpie) report only the totals. When the input details are missing, a generations request, which takes only a text prompt, gets an `input_image_tokens` of 0, and an edit gets its whole `input_tokens`: an edit's input images outweigh its prompt, so all of its input is priced as image input, which overstates the cost by the prompt tokens times the difference between the image and text input prices (for gpt-image-2, 20 prompt tokens next to 256 image tokens cost 0.3% more). With `n` above 1 in a stream, the last completed event's usage is kept, as for every other protocol; whether that event reports the whole request or one image is not documented and has not been observed, because the relays tested return one image.
- `reasoning_tokens` is recorded as reported and is part of `output_tokens`.

Apart from those two image rules, missing fields are null, never zero, and nothing is estimated. The raw object is always kept in `usage_json`.

## 12. Pricing

Pricing exists only to put a cost figure on the usage log. It represents configured list prices per token, image tokens included; it cannot know what a relay actually charges, nor price hosted tools, images billed per picture, or service tiers. The UI labels it as such and shows, for every aggregate, how many requests had no price.

### 12.1 Source

`verbatim pricing sync` downloads each URL in `pricing.sources` into `pricing.cache`, named by the last element of the URL path (`anthropic.json`); two sources with the same file name are a configuration error. The expected shape is the llm-metadata / models.dev provider file: `models.<id>.cost` with `input`, `output`, `cache_read`, `cache_write`, optionally `reasoning`, `tiers`, and `currency`; and provider-level `billing.cacheWrite1h` giving the 1-hour cache write price as a multiple of `input`. A model with no `cost.currency` is USD. Models whose currency is not USD are skipped with a warning. When two sources define the same model id, the later source in the list wins and `check` warns.

`pricing.overrides` is a local JSON file in the same shape (`{"models": {"<name>": {"cost": {...}}}}`). It is merged over the synced data at load time; an override replaces the whole `cost` object of that model. A cost there may also have `image_input` and `image_output`, the image token prices of an image model, and may then omit `output` when the model has no text output. The provider files carry no image prices, so image models are priced through overrides, from the provider's price page:

```json
{"models": {"gpt-image-2": {"cost": {"input": 5, "cache_read": 1.25, "image_input": 8, "image_output": 30}}}}
```

The running server reloads the price table when any file under `pricing.cache` or the overrides file changes, checked at most once a minute. The Models view's Sync prices action (section 14) does what `verbatim pricing sync` does inside the running server and installs the new table at once. One sync runs at a time, for at most a minute; a second one asked for meanwhile is refused. A cost is computed when its row is written, so a sync prices new requests only; logged rows keep their cost.

### 12.2 Lookup

The price is looked up by the **client's model name** (`client_model`), as new-api does, so a relay name that differs from the official one is priced by the name the user chose to expose. Lookup is:

1. Exact match of the client model name in the price table.
2. Else, if `pricing.aliases` maps the client model name to another name, exact match of that name.
3. Else, exact match of the upstream model name, then its alias.
4. Else, no price: `cost_usd` is null and the row is still written.

There is no prefix, pattern, or fuzzy matching. A dated or versioned model name the price table does not know is priced only if the user adds an alias or an override; the UI's "unpriced models" list shows which names need one: the client model names of unpriced requests in the range that the current price table still cannot price, checked by these steps with the client and upstream names each request was logged with, and for an `images` request by an entry with an image price (section 12.3). A request logged before its price was added keeps no cost, and is still counted as unpriced in the aggregates, but its name leaves the list.

### 12.3 Cost

```
cost = input_tokens                                 * cost.input
     + cache_read_tokens                            * cost.cache_read
     + (cache_write_tokens - cache_write_1h_tokens) * cost.cache_write
     + cache_write_1h_tokens                        * cost.input * billing.cacheWrite1h
     + output_tokens                                * cost.output
```

all divided by 1,000,000, with null token columns contributing nothing and `cache_write_1h_tokens` treated as 0 when null. If `cost.cache_write` is absent, cache writes are priced at `cost.input`; if `billing.cacheWrite1h` is absent, 1-hour writes are priced at `cost.cache_write`. Reasoning tokens are priced as output. When `cost.tiers` is present, the tier is chosen by `input_tokens + cache_read_tokens + cache_write_tokens` against each tier's context bound, and a price missing from a tier inherits the base `cost`. `price_key` and `price_json` record what was used. A model without a text output price prices only requests with no text output.

Image tokens, which the images protocol reports within input and output, are priced at `cost.image_input` and `cost.image_output`, the rest at the text prices:

```
cost = (input_tokens  - input_image_tokens)  * cost.input
     + input_image_tokens                    * cost.image_input
     + (output_tokens - output_image_tokens) * cost.output
     + output_image_tokens                   * cost.image_output
```

plus the cache terms above. An `images` request is priced only by an entry with an image price, since its counts mix text and image tokens. For such an entry the image tokens must be known: a null `input_image_tokens` (only when even `input_tokens` is missing, section 11.3) leaves the request unpriced, and a null `output_image_tokens` does too, unless the model has no text output price (gpt-image-2 has none), when all output is image output. A token count with no price for it leaves the request unpriced as well: image tokens for an entry without that image price, or text output for a model without a text output price. Beyond the image input rule of section 11.3, nothing is estimated.

## 13. CLI

```
verbatim serve   [-c config.yaml]            run the gateway (default command)
verbatim check   [-c config.yaml] [--probe]  validate the configuration; --probe also tries each endpoint
verbatim usage   [--since 7d] [--by model|provider|route|day] [--protocol p] [--json]
verbatim log     [--tail] [--since 1h] [--status 5xx] [--provider p] [--errors] [--json]
verbatim log     <request_id>                full row including routing_json, attempt_log, usage_json
verbatim pricing sync
verbatim pricing show <model>
verbatim status                              breaker states, pin count, model-list ages (asks the running server)
```

`check --probe` also sends every endpoint an empty JSON request (`{}`) at the path of its main operation (`create`, or `generate` for `images`), with the headers and key the gateway would send, to check its `base_url` and key; an empty request generates nothing. No reply means the host, port, or scheme is wrong; 401 or 403 means the key is rejected; a redirect, 404, or a reply that is not JSON means `base_url` does not lead to the API. Anything else, typically 400 for the missing fields, counts as ok. An ok is not proof: an upstream that answers every path under a prefix (Alibaba Bailian's `/compatible-mode/v1/`) hides an extra path segment, and one that does not check keys hides a wrong key. Any failure makes `check` exit with 1. `serve` reads its configuration file again on `SIGHUP` (section 6.6). `usage` and `log` read the SQLite file directly and work while `serve` is running. `status` calls the running server's `/ui/api/providers`, because breakers and pins live in its memory. The CLI is the quick path; the web UI is the primary way to inspect traffic.

## 14. Web UI

A UI embedded in the binary and served at `/ui/`. It reads the same SQLite database and the gateway's in-memory state. It has two actions: a price sync, the same as `verbatim pricing sync`, and a configuration reload, the same as `SIGHUP`. Neither changes the configuration file or a logged row. The configuration file remains the only source of truth; the UI has no settings, no key management, no provider editing. This is what keeps verbatim from growing into a management system.

Stack: a separate `web/` project built with Vite, React, TypeScript, Tailwind, and shadcn/ui, compiled to static files and embedded with `go:embed`. Node is needed to build the UI, not to run the gateway; the built assets are committed so a Go build never requires Node. The gateway serves the built assets and a small JSON API under `/ui/api/`. If `server.api_key` is set, the UI asks for it once and sends it as a bearer token. Live views poll; there is no push channel. Charts use a small library (uPlot or Recharts); no charting framework beyond that.

Style direction: clean and restrained, but finished. An operations tool, not a product dashboard, and not a bare admin template either: deliberate typography, consistent spacing, and real attention to the details that make a dense page readable. Dense tables with tabular monospace numerals, right-aligned numbers, status and outcome shown as small colored marks with text, a neutral palette with color reserved for state (ok, failover, error, open breaker), light and dark following the system, no hero sections, no decoration for its own sake. Details are revealed in place (a drawer or expanded row) rather than on separate pages. UI work follows the project's `ui` skill (screenshot-grounded polish), and the exact look is settled during implementation against screenshots.

Views:

1. **Requests.** One row per request: time, protocol and operation, client model and the provider / upstream model it went to, status and outcome, stream flag, time to first byte and total time, token columns (input, cache read, cache write, output, reasoning), cost, attempt count, and pin state. Filters: time range, protocol, provider, route, status class, outcome, model substring, errors only. A live mode appends new rows by polling.
2. **Request detail.** Opened from a row. Shows the routing explanation from `routing_json` (matched route, candidates, computed order, ownership, pin state, breaker skips), the attempt timeline from `attempt_log` (provider, status or error, duration, error excerpt for each attempt), the raw usage object, and the cost breakdown: each token column multiplied by the unit price stored in `price_json`, with the total and the `price_key`.
3. **Usage.** Totals for the selected range and a breakdown by model, provider, route, or day, with a daily chart of cost and tokens. Every aggregate states how many requests in it had no price and how many had no usage, and lists the model names that still have no price (section 12.2).
4. **Models.** The names `/v1/models` lists (section 6.5), in its order: per name, the protocols whose model lists name it and the route that serves each, the providers of the first priority tier, and its text price per 1M tokens (a range when targets differ). A row opens to every candidate per protocol with its priority, upstream model, and price, looked up as a request's would be (section 12.2); an `images` candidate shows its text and image prices. Below the table, the view names the pattern routes that list no name, which still route the requests they match, and the endpoints whose last model-list fetch failed, with the error on hover. Its actions are Sync prices (section 12.1) and Fetch model lists, which fetches every list now.
5. **Providers.** Live state per provider endpoint: breaker closed, open until when, or half-open; consecutive counted failures; committed, failover, and `upstream_error_event` counts over the last hour; active pins; age of the cached model list, or that its fetch is failing, with the error; log rows dropped. Below it, the served configuration, redacted, with the Reload config action (section 6.6).

JSON API: `GET /ui/api/requests` (filters and cursor pagination), `GET /ui/api/requests/{id}`, `GET /ui/api/usage`, `GET /ui/api/providers`, `GET /ui/api/models`, `POST /ui/api/models/refresh` (fetches every model list, then answers as `GET /ui/api/models`), `GET /ui/api/config` (the loaded configuration for display, with `key` values, every `headers` value, and any userinfo or query in an endpoint's `base_url` redacted), `POST /ui/api/pricing/sync`, and `POST /ui/api/config/reload` (the reloaded file's warnings, or 422 with the reason it was refused). The API rejects cross-origin browser requests other than `GET`, `HEAD`, and `OPTIONS` (Go's `http.CrossOriginProtection`), so another site's page cannot start a sync or a reload on a loopback server without `server.api_key`.

## 15. Implementation

This section is guidance for the implementer, not a contract. Sections 4 through 13 define behavior; anything here may change if a cleaner way to get that behavior turns up.

Language: Go (1.27, the current release). Dependencies, kept deliberately short: `tidwall/gjson` and `tidwall/sjson` for point access to JSON, `modernc.org/sqlite` (pure Go, no cgo, keeps the single static binary), `gopkg.in/yaml.v3` with `KnownFields(true)` so unknown configuration keys are errors, `klauspost/compress/zstd` and `andybalholm/brotli` (decoders only, for the tee of section 4.3; the standard library has neither). Everything else is the standard library: `log/slog` for logging, `net/http` ServeMux method-and-pattern routing, `flag` with hand-written subcommand dispatch for the CLI (no cobra), `text/tabwriter` for CLI tables, `database/sql` with hand-written SQL and a `user_version`-based migration list, `go:embed` for the UI. Tests use `testing` and `net/http/httptest` with fake upstreams; the failover, affinity, and usage-parsing behavior is where the tests go.

Forwarding is built on `net/http/httputil.ReverseProxy`, which already handles hop-by-hop headers, headers named by `Connection`, trailers, `FlushInterval: -1` streaming, and client-cancellation propagation. The parts verbatim owns:

- **`Rewrite`**: protocol detection, body read and model extraction have already happened in the handler; `Rewrite` only sets the outbound URL, headers, and body for the first attempt.
- **`Transport`**: a `http.RoundTripper` that implements the attempt loop of section 5. For each candidate it builds the attempt body and URL, sends through the endpoint's `http.Transport` (one per distinct `header_timeout`), and either returns the response (committing it) or records the failure and continues. Because the loop lives in the round tripper, `ReverseProxy` never sees a failed attempt, and only the committed response is written to the client. The shared transport has `DisableCompression: true`, `ResponseHeaderTimeout` from the provider's `header_timeout`, and no redirect following (round trippers do not follow redirects; a 3xx is forwarded as is). Go's transport retries a request on a dead connection only when the request is idempotent or carries an `Idempotency-Key` header; verbatim neither adds nor strips that header, so retry semantics are exactly what the client asked for.
- **`ModifyResponse`**: wraps the body in the usage tee and adds `x-verbatim-request-id` and, on exhaustion, `x-verbatim-attempts`.
- **`ErrorHandler`**: produces the native-format 502 when the round tripper returns an error (no upstream response at all).
- **Idle timeout**: a body-inactivity timer around the upstream body, reset on every read; on expiry the upstream body is closed and the client connection aborted, recorded as outcome `truncated`.

The usage tee is an `io.Reader` wrapper that passes bytes through unchanged and feeds a copy, bounded to the last 64 KiB plus any partial SSE event, to a per-protocol incremental parser. The parser never blocks the read path and swallows its own errors.

```
cmd/verbatim/          main, CLI commands
internal/config/       YAML schema, env expansion, validation
internal/protocol/     one file per protocol: paths and operations, model get/set,
                       auth default, prefix for hashing, usage parser, native error body
internal/route/        route matching, selection order, circuit breaker
internal/affinity/     rules, key extraction, pin store, response ownership
internal/proxy/        handler, ReverseProxy wiring, attempt round tripper, usage tee
internal/store/        SQLite schema, writer, retention, queries for the CLI and UI
internal/pricing/      sync, overrides, aliases, lookup, cost, reload
internal/models/       provider model-list cache and listing assembly
internal/ui/           /ui/api handlers and embedded static assets
web/                   Vite + React UI source, built into internal/ui/dist
```

The `protocol` package is the only place that knows protocol-specific JSON paths. It answers a fixed set of questions and contains no translation logic:

```go
type Protocol interface {
    ID() ID
    Match(method, path string) (op Operation, ok bool)
    Suffix(op Operation) string                                        // path appended to base_url
    ModelsPath(after string) string                                    // one model-list page, appended to base_url
    ModelList(body []byte) (ids []string, next string)                 // one model-list page and the next cursor
    ModelFromBody(body []byte, contentType string) (string, error)     // contentType tells a multipart body
    SetModel(body []byte, contentType, model string) ([]byte, error)
    Stream(body []byte, contentType string) bool
    DefaultHeaders(h http.Header)                                      // anthropic-version only
    PrefixForHash(body []byte) (prefix []byte, ok bool)                // false: no user message
    ContinuationRef(body []byte) (id string, ok bool)                  // previous_response_id / conversation
    NewUsageParser(op Operation, stream bool) UsageParser              // incremental, returns Usage and response id
    ErrorBody(status int, code, msg string) []byte
}
```

Concurrency: one goroutine per request, standard library. The pin store, the breaker state, and the round-robin counters are guarded by mutexes; pin reservation and half-open admission are compare-and-set operations under those mutexes. Log writes go through a bounded channel to a single writer goroutine.

## 16. Version 1 scope

In:

- `chat`, `responses`, `anthropic`, `images` entry points, including `count_tokens` and Codex's `/v1/alpha/search`.
- Providers with configurable auth and headers, routes, selection, failover, breaker, affinity with pin reservation, response ownership, overrides, as specified.
- SQLite log with usage and cost; `serve`, `check`, `usage`, `log`, `pricing sync`, `pricing show`, `status`.
- Model listing endpoints with provider discovery.
- Configuration reload on `SIGHUP` and from the web UI.
- Embedded web UI with the views of section 14.

Out, for later:

- Responses resource operations and WebSocket transport (section 4.4).
- Breaker input from stream-level outcomes.
- Multiple client keys.

## 17. Open items

- Default values for `header_timeout`, `idle_timeout`, `failure_threshold`, and `cooldown` are placeholders to be tuned against real agent traffic. Long prompts with large caches can take over 60 seconds to first byte.
- Whether 404 should be failover-eligible by default. It is useful for relays that lack a model, and wrong if a provider returns 404 for a malformed path. The current choice is eligible, with a warning log, and never counted by the breaker.
- Whether `affinity.ttl` should default higher than one hour. A working session with a lunch break in it loses its pin at one hour.
