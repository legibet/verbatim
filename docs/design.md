# verbatim: Design

Status: draft for implementation, revision 2. Date: 2026-10-15.

verbatim is a small, self-hosted gateway that sits between LLM API clients and LLM API providers. It routes requests to upstream providers, retries on failure, keeps a session on the same upstream, records usage, and does nothing else. It never converts between API formats.

## 1. Principles

1. **Zero format conversion.** A request that arrives in a protocol is forwarded in that protocol, to a provider that natively speaks it. verbatim does not translate Chat Completions to Responses, Anthropic Messages to anything, or anything to Anthropic Messages. There is no internal message representation.
2. **Pass through by default.** The data plane changes exactly three things in a request: the authentication header, the model name, and the destination URL. Everything else, including unknown fields, dialect extensions, extra headers, query parameters, and stream events, is forwarded byte for byte. An upstream response, success or error, is returned to the client unchanged, with its status code, headers, and body.
3. **No hidden behavior.** Every change verbatim makes to a request is declared in the configuration file. There are no heuristics based on hostnames, error text, or model names beyond what the configuration states. verbatim never retries after it has committed a response to the client.
4. **Deterministic per target.** For a given configuration, client request, and chosen target, the forwarded request is a pure function of those three. Which target is chosen may vary by selection and failover; session affinity exists so that an agent conversation keeps hitting the same target. This matters for Anthropic prompt caching and for Anthropic preserved thinking, which binds thinking-block signatures to the exact request prefix (`system`, `tools`, earlier `messages`) and rejects later turns when the prefix changes.
5. **Small.** One Go binary, one YAML file, one SQLite database, and an embedded read-only web UI.

## 2. Non-goals

- Format conversion of any kind, including Chat Completions to or from Responses.
- Per-user accounts, multiple client keys with separate quotas, billing, or a management UI. The embedded web UI (section 14) is read-only.
- Prompt or response rewriting beyond the static overrides in section 9.
- Load-aware or cost-aware scheduling. Routing uses static priority and weight only.
- Responses resource operations other than creating a response: `GET /v1/responses/{id}`, cancel, delete, input item listing, `background: true` polling, compaction, and the Responses WebSocket transport. These are rejected with 404 in version 1 (section 4.4). The response ownership table (section 7.4) is the foundation for adding them later.
- Version 1 does not ship the Gemini entry point. The design includes it so that nothing has to change when it is added.

## 3. Why zero conversion is viable

Research notes are in `.agents/` and are not part of this document. The findings that justify the design:

- Three of the four protocols carry opaque reasoning state that the client must echo back unchanged: Anthropic `signature` and `redacted_thinking.data`, Responses `encrypted_content`, Gemini `thoughtSignature`. The standard Chat Completions schema has no field for this. Provider dialects add non-standard ones (`reasoning_content`, OpenRouter `reasoning_details`, Gemini's `extra_content.google.thought_signature`); verbatim forwards them untouched because it forwards everything untouched, but no gateway can move such state from one provider to another.
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
| `gemini` | `create` | `POST /v1beta/models/{model}:generateContent`, `:streamGenerateContent` | URL path |
| `gemini` | `count_tokens` | `POST /v1beta/models/{model}:countTokens` | URL path |

`search` is Codex's standalone web search. Models that Codex runs without hosted tools search through a client-side `web.run` function, and Codex posts each call to `{base_url}/alpha/search` when the provider sets `supports_standalone_web_search`. It is a plain JSON request and response; the body carries the turn's `model` and the Codex session id as `id`, and the response reports no usage.

The operation matters for affinity (section 7.3), failover (section 8), usage (section 11.3), and the protocol-specific suffix appended to the provider's `base_url`. Only `create` is priced, creates or moves pins, and feeds the breaker; `count_tokens` and `search` follow a pin, never feed the breaker, and are left out of usage aggregates. The client's query string is preserved on the upstream URL, minus the `key` parameter used by Gemini clients for authentication. Claude Code, for example, sends `/v1/messages?beta=true`; the `?beta=true` reaches the provider.

### 4.1 Client authentication

If `server.api_key` is set, a request must present it in one of `Authorization: Bearer <key>`, `x-api-key: <key>`, `x-goog-api-key: <key>`, or the `key` query parameter. Any one match is sufficient; a request may carry several. If `server.api_key` is unset, all requests are accepted; the configuration validator prints a warning unless `server.listen` is a loopback address.

### 4.2 Request forwarding

For a forwarded request, verbatim:

1. Reads the full client body into memory. The body is needed for retries, for the model rewrite, and for the affinity key. Requests above `server.max_body_bytes` (default 32 MiB) are rejected with 413. `server.max_inflight` (default 64) bounds the number of concurrent requests and therefore total buffered memory; excess requests get 503 in native format.
2. For each attempt, starts from the original client body and produces the attempt's body: the model name is rewritten in place with a JSON point update (`sjson`-style), then the provider's static overrides (section 9) are applied as point updates. For `gemini` the model is in the URL and the body step is skipped. Deserializing the body into a struct is never done, so unknown fields survive.
3. Builds the upstream URL as `provider.base_url` + the operation's fixed suffix + the client's query string. `base_url` is the base URL the protocol's official SDK takes, which is what providers document and what users already put in `OPENAI_BASE_URL` or `ANTHROPIC_BASE_URL`; the suffix completes it the way that SDK does. For `chat` and `responses` the SDK base URL carries the version (`https://api.deepseek.com/v1`) and the suffixes are `/chat/completions`, `/responses`, `/alpha/search`, and `/models`. For `anthropic` it does not (`https://api.anthropic.com`, `https://api.deepseek.com/anthropic`, `https://openrouter.ai/api`) and the suffixes are `/v1/messages`, `/v1/messages/count_tokens`, and `/v1/models`. For `gemini` it does not either (`https://generativelanguage.googleapis.com`) and the suffixes are `/v1beta/models/{model}:generateContent`, `:countTokens`, and `/v1beta/models`.
4. Copies client headers, except hop-by-hop headers (including those named by `Connection`), `Host`, `Content-Length`, and every client authentication carrier (`Authorization`, `x-api-key`, `x-goog-api-key`). Applies the provider's `headers` map (section 6.2): a string value sets the header, `null` removes it. Then sets the provider's authentication header per its `auth` setting; it always wins over `headers`. For `anthropic`, if the client did not send `anthropic-version`, verbatim sets `anthropic-version: 2023-06-01`, because the Anthropic API rejects requests without it. This is the only header verbatim adds on its own, and a provider's `headers` can override it.
5. Sends the request and streams the response back.

### 4.3 Response forwarding

Response status, headers (except hop-by-hop), trailers, and body are copied to the client unchanged. The transport is configured not to request or transparently decompress content encodings, so what the provider sends is what the client gets. Server-sent events are copied as they arrive, with a flush after every write. verbatim observes the response body through a bounded tee that extracts usage and the response id for the log (section 11.3); the tee never modifies what the client receives, and a parse failure in the tee never affects forwarding. When the upstream response carries `Content-Encoding` `gzip`, `deflate`, `br`, or `zstd`, the tee decompresses its own copy; the client still receives the compressed bytes. All four matter in practice: clients advertise them (Node's fetch sends `gzip, deflate, br, zstd`) and providers use them for streams (DeepSeek answers that header with `zstd`), and the tee is also where the response id that Responses ownership needs is read. Other encodings, and stacked encodings, are not inspected and yield null usage.

The point of no return is when verbatim commits the upstream status and headers to the client. After that, nothing is retried: a cut stream is forwarded as a cut stream and the client connection is aborted rather than closed cleanly, so the client's SDK sees an error instead of a truncated success. An error event inside a `200` stream is forwarded as is.

verbatim does not add `stream_options.include_usage` to Chat Completions requests. If the upstream stream carries no usage, the log row records null token counts.

### 4.4 Other paths

| Path | Behavior |
|---|---|
| `GET /v1/models`, `GET /v1beta/models` | Served by verbatim from the configuration and provider discovery, section 6.5 |
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
    one target whose provider speaks this protocol (404 native if none, section 6.3)
  → candidates = that route's targets filtered to this protocol
  → ownership: if the body carries previous_response_id (or conversation), look up
    the owning target; it is the only candidate (section 7.4)
  → affinity: look up or reserve a pin for this session (section 7)
  → build attempt order (section 6.4)
  → for each candidate:
       if its provider's breaker is open at dispatch time: skip, record
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

One YAML file. `${ENV_VAR}` in any string value is expanded from the environment at load time; an unset variable is a validation error, never an empty string. A leading `~/` in a path is the user's home directory. The file is validated at startup; any reference to an undefined provider, duplicate route pattern, unknown field, or invalid override is a startup error.

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
  openai-responses:
    type: responses
    base_url: https://api.openai.com/v1
    key: ${OPENAI_KEY}
  openai-chat:
    type: chat
    base_url: https://api.openai.com/v1
    key: ${OPENAI_KEY}
  moonshot-anthropic:
    type: anthropic
    base_url: https://api.moonshot.ai/anthropic
    key: ${MOONSHOT_KEY}
  moonshot-chat:
    type: chat
    base_url: https://api.moonshot.ai/v1
    key: ${MOONSHOT_KEY}
  openrouter-anthropic:
    type: anthropic
    base_url: https://openrouter.ai/api
    key: ${OPENROUTER_KEY}
    auth: bearer                     # send the key as Authorization: Bearer instead of x-api-key
  relay-a-chat:
    type: chat
    base_url: https://relay.example/v1
    key: ${RELAY_A_KEY}
    headers:
      x-title: verbatim
      openai-project: null           # drop a client header this relay rejects
    overrides:
      - delete: metadata
      - set: {path: temperature, value: 1}
  local-llama:
    type: chat
    base_url: http://127.0.0.1:8081/v1
    auth: none

routes:
  - model: kimi-k3
    targets:
      - provider: moonshot-anthropic
      - provider: moonshot-chat
        weight: 3
      - provider: relay-a-chat
        model: kimi-k3-0905
        weight: 1
      - provider: openrouter-anthropic
        model: moonshotai/kimi-k3
        priority: 2
  - model: "gpt-*"
    targets:
      - provider: openai-responses
      - provider: openai-chat
  - model: "*"
    discover: false
    targets:
      - provider: openrouter-anthropic
      - provider: local-llama

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
  cache_ttl: 10m
  timeout: 3s

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

A provider is one upstream endpoint speaking exactly one protocol. A relay that offers several protocols is written as several providers, one per protocol, usually sharing a key through the same environment variable.

| Field | Required | Meaning |
|---|---|---|
| `type` | yes | `chat`, `responses`, `anthropic`, or `gemini` |
| `base_url` | yes | The base URL the protocol's official SDK takes; the operation's fixed suffix is appended (section 4.2) |
| `key` | unless `auth: none` | Provider API key |
| `auth` | no | `bearer` (`Authorization: Bearer <key>`), `x-api-key`, `x-goog-api-key`, or `none`. Default by type: `chat` and `responses` use `bearer`, `anthropic` uses `x-api-key`, `gemini` uses `x-goog-api-key`. |
| `headers` | no | Map of header name to string (set) or `null` (remove), applied after client headers are copied and before `auth` |
| `overrides` | no | Ordered list of static request edits, section 9 |

### 6.3 `routes`

An ordered list. A request matches the first route whose `model` pattern matches the client's model name **and** which has at least one target whose provider speaks the inbound protocol. A route that matches the name but has no target for the protocol is skipped, so a later route can still serve the request. If no route qualifies, verbatim returns 404 in native format with code `no_route`.

Patterns are globs with `*` as the only wildcard, matched case-sensitively against the whole name; `*` matches any run of characters including `/`, so `anthropic/*` works for OpenRouter-style names (the standard library's `path.Match` stops at `/` and is not used). Put exact names first, patterns after, and `"*"` last if a catch-all is wanted. A route's `model` pattern is its identity and must be unique in the list.

Each route has a flat `targets` list. A target's protocol is the protocol of its provider; the route itself is protocol-agnostic. A target is identified by the pair (provider, upstream model name).

| Target field | Default | Meaning |
|---|---|---|
| `provider` | required | Provider name |
| `model` | client's model name | Upstream model name. Literal only. If absent, the client's name is forwarded unchanged. |
| `priority` | 1 | Lower number is tried first |
| `weight` | 1 | Relative share within a priority tier |

Route-level fields:

| Field | Default | Meaning |
|---|---|---|
| `model` | required | Glob pattern and route identity |
| `targets` | required | At least one |
| `balance` | `weighted` | `weighted` (random by weight within a tier) or `round_robin` (rotate within a tier, weights ignored) |
| `discover` | `true` | For pattern routes: populate the listing endpoints from the providers' own model lists, section 6.5. Ignored on exact-name routes. |

There is no provider-level model name mapping and no template substitution in `model`. If a relay prefixes every model name, each target states its full upstream name.

### 6.4 Selection and attempt order

Given the candidates (the matched route's targets filtered to the inbound protocol):

1. If ownership (section 7.4) names a target, the order is that single target.
2. Otherwise, group by `priority`, ascending. Within each tier, order by `balance`: `weighted` draws without replacement with probability proportional to `weight`; `round_robin` rotates from a counter kept per (route, protocol, tier). Concatenate the tiers.
3. If affinity (section 7) yields a pinned target that is among the candidates, move it to the front.
4. Attempt the list in order. The breaker is checked at dispatch time for each attempt, not when the list is built; a provider whose breaker is open is skipped and recorded in `routing_json`. If every candidate is skipped, the request fails immediately with 502 `all_upstreams_open`; there is no bypass, because the half-open probe (section 8.3) is what gives a recovering provider its chance.

Each target is attempted at most once per request. A tier is exhausted before the next tier is touched. This differs from new-api, which steps down one priority tier per retry and ends up retrying the last tier repeatedly.

### 6.5 Model listing

Many clients populate their model picker from `/v1/models`, so the list has to be real. It is assembled per route, in configuration order:

- An exact-name route contributes its own `model`. Providers are not consulted.
- A pattern route with `discover: true` (the default) contributes the provider-listed model ids, over its targets, that match the pattern. Targets that carry a `model` rewrite are skipped, because the names a provider lists are its upstream names and would not match what the client must send. Each discovered name is then resolved through the routing rules of section 6.3 for that target's protocol, and is kept only if routing would send it to this route; otherwise an earlier route owns the name and the entry is dropped.
- A pattern route with `discover: false` contributes nothing.

Names are deduplicated; the first occurrence wins. The response lists names only; it does not promise that every listed name works for every protocol.

Provider lists come from `GET {base_url}` + the protocol's model-list suffix (section 4.2) for every protocol type (for `gemini`, ids are returned with the `models/` prefix stripped). Anthropic-style paginated lists are followed through `has_more` up to 10 pages. Lists are cached per provider for `models.cache_ttl`, warmed in the background at startup, and refreshed in the background when stale. A fetch has a `models.timeout`; on failure the previous list is kept and a warning is logged. The client-facing endpoints read the cache and never wait on a refresh; only a request that arrives while the startup round of fetches is still running waits for that round (bounded by `models.timeout`), so a client that lists models right after startup does not get an empty list.

Response shape: one entry per name carrying the union of what OpenAI and Anthropic clients read, so either parser is satisfied and ignores the rest:

```json
{"object":"list","data":[{"id":"kimi-k3","object":"model","created":0,"owned_by":"verbatim",
                          "type":"model","display_name":"kimi-k3","created_at":"1970-01-01T00:00:00Z"}],
 "has_more":false,"first_id":"kimi-k3","last_id":"kimi-k3"}
```

The Gemini listing uses Gemini's shape (`{"models":[{"name":"models/<id>", ...}]}`).

## 7. Session affinity

Agent sessions must stay on one upstream. Anthropic thinking signatures are validated by the issuer and, on current models, bound to the issuing account; prompt caches live on one upstream; Responses server-side state lives on one upstream. Affinity is applied before selection and overrides priority and weight.

verbatim pins its immediate upstream. If that upstream is itself a router (OpenRouter balances across backends), continuity below it is the upstream's business.

### 7.1 Key extraction

Which request field identifies a session is a property of the client, so it is configuration, not code. `affinity.rules` is an ordered list; the first rule whose `match` fits the request is used. `match` has optional `protocol` and `model` (glob). `key` is an ordered list of sources, and the first source that yields a non-empty string is the key:

- `{body: <gjson path>}`: a string or number at that path in the request body, taken as sent.
- `{header: <name>}`: a request header, case-insensitive.

verbatim ships no built-in rules. The example configuration carries the rules that match today's common clients, and they are ordinary configuration the user can change: Claude Code sends `metadata.user_id`; Codex sends a `session-id` header and `prompt_cache_key` with the same value, and its `/alpha/search` carries that value as `id`, so one rule keeps a search on the session's upstream; Pi and omp can send `x-session-affinity`; OpenAI SDK users often set `user` or `prompt_cache_key`. Where a client offers both a header and a body field, the header is listed first because it never participates in prompt caching or thinking-prefix checks. These values identify a client session, not necessarily one conversation; two conversations under one `user` share a pin, which costs nothing but balance.

If no rule matches or no source yields a value, `affinity.fallback` applies:

- `prefix_hash` (default): SHA-256 over the protocol id, the system prompt field, and the first user-role message, each taken as the exact bytes the client sent: `anthropic` uses `system` and the first `messages[]` entry with `role: user`; `chat` uses the first message with role `system` or `developer` and the first with role `user`; `responses` uses `instructions` and the first `input[]` item with `role: user` (or `input` itself when it is a string); `gemini` uses `systemInstruction` and the first `contents[]` entry with `role: user`. If no user-role message exists the request gets no affinity. This is best-effort stickiness: two conversations with the same opening share a pin, and a client that rewrites its opening mid-conversation (context compaction, a changing system prompt) moves to a fresh key. Clients that need guarantees should send a session identifier and name it in a rule.
- `none`: no affinity for this request.

### 7.2 Pin identity and lifetime

The pin key is (protocol, affinity key, client model name). One session usually touches several models, and each must be pinned independently; the same key under two protocols has two independent pins because the candidate sets differ. The pin value is a target identity (provider, upstream model).

The pin is **reserved at selection time**, not after success: when a request finds no pin, it reserves one for the target it is about to attempt, atomically against concurrent requests for the same key, so two parallel first requests of a session (agent clients routinely fire several at start) land on the same target. If the attempt fails over, the pin moves to the target that eventually gets committed. If the request fails entirely and no request has yet committed a response under this pin, the reservation is released; a pin that has served a committed response stays.

Pins live in an in-process map with LRU eviction and `affinity.ttl` expiry, refreshed on every use. They do not survive a restart; after a restart, the first request of a session re-pins by ordinary selection, and the client handles any resulting signature error (Claude Code strips earlier thinking and retries; other clients fail the turn). `affinity.ttl` should be at least as long as the longest pause within a working session.

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

- No response headers: connection, TLS, or DNS failure; `server.header_timeout` elapsed.
- Upstream status 408, 409, 425, 429, 500, 502, 503, 504, 529.
- Upstream status 401 or 403: the provider key is invalid, exhausted, or not entitled to this model. Logged at warning level because it needs an operator.
- Upstream status 404: the provider does not serve this model or operation. Logged at warning level.

Every other status is returned to the client unchanged and ends the attempt loop. This includes all 400s (malformed request, Anthropic thinking signature errors, spend limits) and 402, 413, 422: retrying the same body elsewhere would either fail identically or hide the real error. The client owns its history and is the only party that can repair it.

The body of an abandoned attempt is read (bounded by `server.idle_timeout` and 1 MiB) so that it can be returned to the client if this turns out to be the last attempt (section 8.4), and its first `log.error_excerpt_bytes` are kept in `attempt_log`.

### 8.2 Never failover

- Anything after status and headers have been committed to the client.
- Error objects inside a `200` streamed body. They are forwarded, and recorded as outcome `upstream_error_event` (section 11.2), but they do not touch the breaker in version 1.
- Any `count_tokens` or `search` failure affecting the pin (section 7.3).

### 8.3 Circuit breaker

Per provider. Only signals that indicate the provider as a whole is unhealthy feed the breaker: no response headers, 5xx, 529, 429. A 401, 403, or 404 permits failover for this request but does not count, because it is as likely to be about one model, one key entitlement, or one operation (Ollama serves Messages but not `count_tokens`; most relays do not serve `search`) as about the provider. `count_tokens` and `search` attempts never feed the breaker, whatever their status.

`breaker.failure_threshold` consecutive counted failures open the breaker for `breaker.cooldown`. While open, the provider is skipped at dispatch (section 6.4 step 4). After the cooldown exactly one request is admitted (half-open, admission is atomic); a committed response closes the breaker, a counted failure reopens it for another cooldown. Any committed response resets the consecutive counter.

### 8.4 Exhaustion

When every candidate has failed or been skipped:

- If the last attempt received an upstream response, that response is returned to the client **unchanged** (status, headers, body), consistent with principle 2. verbatim adds one header, `x-verbatim-attempts: <n>`, and the log row explains the sequence. A route with a single provider that returns 401 therefore shows the client a 401 with the provider's own message, not a synthetic 502.
- If no attempt received a response (all connection failures or timeouts, or every provider open), verbatim responds 502 in the inbound protocol's native error format (section 10) with a message listing each attempt as `provider: error`.

## 9. Request overrides

A provider may declare static edits applied to every request it receives, whatever the operation, as an ordered list of operations:

```yaml
overrides:
  - delete: metadata
  - delete: service_tier
  - set: {path: temperature, value: 1}
```

Paths are gjson/sjson paths into the request body. Operations run in the listed order, each attempt starting from the original client body, so one provider's edits never leak into another provider's attempt. Values are literals; there is no templating, no conditionals, and no access to the request. These edits are the only body modification verbatim performs besides the model name.

The validator rejects:

- `model` and `stream` at any depth of a `set` or `delete`, because they drive routing and the usage parser.
- Any path under `system`, `tools`, or `messages` on providers of type `anthropic`, because editing them would change the preserved-thinking prefix. Overrides elsewhere in the body are static and therefore identical on every turn.
- Two operations whose paths overlap (one is a prefix of the other), to keep the result independent of ordering mistakes.

Overrides exist for relay quirks (a relay that rejects an unknown field, or demands a fixed parameter). They are not a conversion mechanism. Note that an override cannot be conditional on the request, so an edit that is only valid for streaming requests (such as `stream_options`) cannot be expressed; that is deliberate.

## 10. Errors produced by verbatim

Errors that verbatim itself generates use the inbound protocol's native shape so SDKs parse them:

| Protocol | Body |
|---|---|
| `chat`, `responses` | `{"error":{"message":"...","type":"verbatim_error","code":"<code>"}}` |
| `anthropic` | `{"type":"error","error":{"type":"api_error","message":"<code>: ..."}}` |
| `gemini` | `{"error":{"code":502,"message":"<code>: ...","status":"UNAVAILABLE"}}` |

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
| `protocol` | text | `chat`, `responses`, `anthropic`, `gemini` |
| `operation` | text | `create`, `count_tokens`, or `search` |
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
| `usage_json` | text | the provider's usage object as received, unmodified |
| `cost_usd` | real | computed at write time; null when no price or when usage is incomplete |
| `price_key` | text | the price-table entry used |
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
| `gemini` | `usageMetadata` | last chunk's `usageMetadata` | `promptTokenCount`, `candidatesTokenCount`, `cachedContentTokenCount`, `thoughtsTokenCount` |
| `count_tokens` | top-level `input_tokens` (Anthropic), `totalTokens` (Gemini) | n/a | |

Normalization into the row:

- `anthropic`: `input_tokens` is already uncached. `cache_write_tokens` = `cache_creation_input_tokens`; `cache_write_1h_tokens` = `cache_creation.ephemeral_1h_input_tokens` when present, else null.
- `chat`, `responses`: the reported prompt count includes cached and cache-written tokens. `input_tokens` = prompt count minus `cached_tokens` minus `cache_write_tokens`, each treated as 0 only when the provider reports the details object at all; if the details object is absent, `input_tokens` holds the full prompt count and the cache columns are null. OpenAI reports `cache_write_tokens` for GPT-5.6 and later; its cache lifetime is 30 minutes and has no 1-hour variant, so `cache_write_1h_tokens` is null.
- `gemini`: `input_tokens` = `promptTokenCount` minus `cachedContentTokenCount`; no cache writes are reported.
- `reasoning_tokens` is recorded as reported and is part of `output_tokens`.

Missing fields are null, never zero. Nothing is estimated. The raw object is always kept in `usage_json`.

## 12. Pricing

Pricing exists only to put a cost figure on the usage log. It represents configured list prices per token; it cannot know what a relay actually charges, nor price hosted tools, images, or service tiers. The UI labels it as such and shows, for every aggregate, how many requests had no price.

### 12.1 Source

`verbatim pricing sync` downloads each URL in `pricing.sources` into `pricing.cache`, named by the last element of the URL path (`anthropic.json`); two sources with the same file name are a configuration error. The expected shape is the llm-metadata / models.dev provider file: `models.<id>.cost` with `input`, `output`, `cache_read`, `cache_write`, optionally `reasoning`, `tiers`, and `currency`; and provider-level `billing.cacheWrite1h` giving the 1-hour cache write price as a multiple of `input`. A model with no `cost.currency` is USD. Models whose currency is not USD are skipped with a warning. When two sources define the same model id, the later source in the list wins and `check` warns.

`pricing.overrides` is a local JSON file in the same shape (`{"models": {"<name>": {"cost": {...}}}}`). It is merged over the synced data at load time; an override replaces the whole `cost` object of that model.

The running server reloads the price table when any file under `pricing.cache` or the overrides file changes, checked at most once a minute.

### 12.2 Lookup

The price is looked up by the **client's model name** (`client_model`), as new-api does, so a relay name that differs from the official one is priced by the name the user chose to expose. Lookup is:

1. Exact match of the client model name in the price table.
2. Else, if `pricing.aliases` maps the client model name to another name, exact match of that name.
3. Else, exact match of the upstream model name, then its alias.
4. Else, no price: `cost_usd` is null and the row is still written.

There is no prefix, pattern, or fuzzy matching. A dated or versioned model name the price table does not know is priced only if the user adds an alias or an override; the UI's "unpriced models" list shows which names need one.

### 12.3 Cost

```
cost = input_tokens                                 * cost.input
     + cache_read_tokens                            * cost.cache_read
     + (cache_write_tokens - cache_write_1h_tokens) * cost.cache_write
     + cache_write_1h_tokens                        * cost.input * billing.cacheWrite1h
     + output_tokens                                * cost.output
```

all divided by 1,000,000, with null token columns contributing nothing and `cache_write_1h_tokens` treated as 0 when null. If `cost.cache_write` is absent, cache writes are priced at `cost.input`; if `billing.cacheWrite1h` is absent, 1-hour writes are priced at `cost.cache_write`. Reasoning tokens are priced as output. When `cost.tiers` is present, the tier is chosen by `input_tokens + cache_read_tokens + cache_write_tokens` against each tier's context bound, and a price missing from a tier inherits the base `cost`. `price_key` and `price_json` record what was used.

## 13. CLI

```
verbatim serve   [-c config.yaml]            run the gateway (default command)
verbatim check   [-c config.yaml]            validate the configuration and exit
verbatim usage   [--since 7d] [--by model|provider|route|day] [--protocol p] [--json]
verbatim log     [--tail] [--since 1h] [--status 5xx] [--provider p] [--errors] [--json]
verbatim log     <request_id>                full row including routing_json, attempt_log, usage_json
verbatim pricing sync
verbatim pricing show <model>
verbatim status                              breaker states, pin count, model-list ages (asks the running server)
```

`usage` and `log` read the SQLite file directly and work while `serve` is running. `status` calls the running server's `/ui/api/providers`, because breakers and pins live in its memory. The CLI is the quick path; the web UI is the primary way to inspect traffic.

## 14. Web UI

A read-only UI embedded in the binary and served at `/ui/`. It reads the same SQLite database and the gateway's in-memory state, and it edits nothing. The configuration file remains the only source of truth; the UI has no settings, no key management, no provider editing. This is what keeps verbatim from growing into a management system.

Stack: a separate `web/` project built with Vite, React, TypeScript, Tailwind, and shadcn/ui, compiled to static files and embedded with `go:embed`. Node is needed to build the UI, not to run the gateway; the built assets are committed so a Go build never requires Node. The gateway serves the built assets and a small JSON API under `/ui/api/`. If `server.api_key` is set, the UI asks for it once and sends it as a bearer token. Live views poll; there is no push channel. Charts use a small library (uPlot or Recharts); no charting framework beyond that.

Style direction: clean and restrained, but finished. An operations tool, not a product dashboard, and not a bare admin template either: deliberate typography, consistent spacing, and real attention to the details that make a dense page readable. Dense tables with tabular monospace numerals, right-aligned numbers, status and outcome shown as small colored marks with text, a neutral palette with color reserved for state (ok, failover, error, open breaker), light and dark following the system, no hero sections, no decoration for its own sake. Details are revealed in place (a drawer or expanded row) rather than on separate pages. UI work follows the project's `ui` skill (screenshot-grounded polish), and the exact look is settled during implementation against screenshots.

Views:

1. **Requests.** One row per request: time, protocol and operation, client model and the provider / upstream model it went to, status and outcome, stream flag, time to first byte and total time, token columns (input, cache read, cache write, output, reasoning), cost, attempt count, and pin state. Filters: time range, protocol, provider, route, status class, outcome, model substring, errors only. A live mode appends new rows by polling.
2. **Request detail.** Opened from a row. Shows the routing explanation from `routing_json` (matched route, candidates, computed order, ownership, pin state, breaker skips), the attempt timeline from `attempt_log` (provider, status or error, duration, error excerpt for each attempt), the raw usage object, and the cost breakdown: each token column multiplied by the unit price stored in `price_json`, with the total and the `price_key`.
3. **Usage.** Totals for the selected range and a breakdown by model, provider, route, or day, with a daily chart of cost and tokens. Every aggregate states how many requests in it had no price and how many had no usage, and lists the unpriced model names.
4. **Providers.** Live state per provider: breaker closed, open until when, or half-open; consecutive counted failures; committed, failover, and `upstream_error_event` counts over the last hour; active pins; age of the cached model list; log rows dropped.

JSON API, all `GET`: `/ui/api/requests` (filters and cursor pagination), `/ui/api/requests/{id}`, `/ui/api/usage`, `/ui/api/providers`, `/ui/api/config` (the loaded configuration for display, with `key` values, every `headers` value, and any userinfo or query in `base_url` redacted).

## 15. Implementation

This section is guidance for the implementer, not a contract. Sections 4 through 13 define behavior; anything here may change if a cleaner way to get that behavior turns up.

Language: Go (1.27, the current release). Dependencies, kept deliberately short: `tidwall/gjson` and `tidwall/sjson` for point access to JSON, `modernc.org/sqlite` (pure Go, no cgo, keeps the single static binary), `gopkg.in/yaml.v3` with `KnownFields(true)` so unknown configuration keys are errors, `klauspost/compress/zstd` and `andybalholm/brotli` (decoders only, for the tee of section 4.3; the standard library has neither). Everything else is the standard library: `log/slog` for logging, `net/http` ServeMux method-and-pattern routing, `flag` with hand-written subcommand dispatch for the CLI (no cobra), `text/tabwriter` for CLI tables, `database/sql` with hand-written SQL and a `user_version`-based migration list, `go:embed` for the UI. Tests use `testing` and `net/http/httptest` with fake upstreams; the failover, affinity, and usage-parsing behavior is where the tests go.

Forwarding is built on `net/http/httputil.ReverseProxy`, which already handles hop-by-hop headers, headers named by `Connection`, trailers, `FlushInterval: -1` streaming, and client-cancellation propagation. The parts verbatim owns:

- **`Rewrite`**: protocol detection, body read and model extraction have already happened in the handler; `Rewrite` only sets the outbound URL, headers, and body for the first attempt.
- **`Transport`**: a `http.RoundTripper` that implements the attempt loop of section 5. For each candidate it builds the attempt body and URL, sends through a shared `http.Transport`, and either returns the response (committing it) or records the failure and continues. Because the loop lives in the round tripper, `ReverseProxy` never sees a failed attempt, and only the committed response is written to the client. The shared transport has `DisableCompression: true`, `ResponseHeaderTimeout` from `server.header_timeout`, and no redirect following (round trippers do not follow redirects; a 3xx is forwarded as is). Go's transport retries a request on a dead connection only when the request is idempotent or carries an `Idempotency-Key` header; verbatim neither adds nor strips that header, so retry semantics are exactly what the client asked for.
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
    ID() string
    Match(method, path string) (op Operation, model string, ok bool)   // model only for gemini
    Suffix(op Operation, model string) string                          // path appended to base_url
    ModelsSuffix() string                                              // model-list path appended to base_url
    ModelFromBody(body []byte) (string, error)
    SetModel(body []byte, model string) ([]byte, error)               // no-op for gemini
    DefaultAuth() AuthScheme
    DefaultHeaders(h http.Header)                                      // anthropic-version only
    PrefixForHash(body []byte) ([]byte, bool)                          // false: no user message
    ContinuationRef(body []byte) (id string, ok bool)                  // previous_response_id / conversation
    NewUsageParser(op Operation, stream bool) UsageParser              // incremental, returns Usage and response id
    ErrorBody(status int, code, msg string) []byte
}
```

Concurrency: one goroutine per request, standard library. The pin store, the breaker state, and the round-robin counters are guarded by mutexes; pin reservation and half-open admission are compare-and-set operations under those mutexes. Log writes go through a bounded channel to a single writer goroutine.

## 16. Version 1 scope

In:

- `chat`, `responses`, `anthropic` entry points, including `count_tokens` and Codex's `/v1/alpha/search`.
- Providers with configurable auth and headers, routes, selection, failover, breaker, affinity with pin reservation, response ownership, overrides, as specified.
- SQLite log with usage and cost; `serve`, `check`, `usage`, `log`, `pricing sync`, `pricing show`, `status`.
- Model listing endpoints with provider discovery.
- Embedded read-only web UI with the four views of section 14.

Out, for later:

- `gemini` entry point (interface in place, adapter not written).
- Responses resource operations and WebSocket transport (section 4.4).
- Breaker input from stream-level outcomes.
- Model name templates in targets.
- Multiple client keys.

## 17. Open items

- Default values for `header_timeout`, `idle_timeout`, `failure_threshold`, and `cooldown` are placeholders to be tuned against real agent traffic. Long prompts with large caches can take over 60 seconds to first byte.
- Whether 404 should be failover-eligible by default. It is useful for relays that lack a model, and wrong if a provider returns 404 for a malformed path. The current choice is eligible, with a warning log, and never counted by the breaker.
- Whether `affinity.ttl` should default higher than one hour. A working session with a lunch break in it loses its pin at one hour.
- The Gemini Interactions API, which Google now positions above `generateContent`, has not been examined. It may require a fifth protocol rather than an extension of `gemini`.
