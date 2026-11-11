# Forwarding

What verbatim sends to the chosen endpoint and how the response comes back. Which endpoint is chosen is in [routing.md](routing.md).

## Protocols and operations

The protocol and operation come from the request path. verbatim never looks at the client's identity or user agent.

| Protocol | Operation | Path |
|---|---|---|
| `chat` | `create` | `POST /v1/chat/completions` |
| `responses` | `create` | `POST /v1/responses` |
| `responses` | `search` | `POST /v1/alpha/search` |
| `anthropic` | `create` | `POST /v1/messages` |
| `anthropic` | `count_tokens` | `POST /v1/messages/count_tokens` |
| `images` | `generate` | `POST /v1/images/generations` |
| `images` | `edit` | `POST /v1/images/edits` |

The model is always the body's `model`; for a multipart image edit, the `model` form field.

`create`, `generate`, and `edit` are primary operations: they are priced, create and move pins, and feed the breaker. `count_tokens` and `search` are auxiliary: they serve a session's primary requests, follow an existing pin without creating or moving one, never feed the breaker, and are not priced or counted in usage.

`search` is Codex's standalone web search. Models that Codex runs without hosted tools search through a client-side `web.run` function, and Codex posts each call to `{base_url}/alpha/search` when the provider sets `supports_standalone_web_search`. The body carries the turn's `model` and the Codex session id as `id`; the response reports no usage.

`images` is the OpenAI Images API, a protocol of its own because whether an upstream serves it is independent of whether it serves chat at the same base URL. Edits from the OpenAI SDK are `multipart/form-data`; Codex's image tool sends JSON. Both endpoints answer synchronously, with the image in the response (`b64_json`, often megabytes) or as server-sent events when the client sets `stream`.

Other paths:

| Path | Behavior |
|---|---|
| `GET /v1/models` | Listing assembled from the configuration and provider model lists ([routing.md](routing.md#model-listing)) |
| `/ui/`, `/ui/api/*` | Web UI ([web-ui.md](web-ui.md)) |
| `GET /healthz` | 200 `{"ok":true}` |
| Responses resource operations, WebSocket upgrades | 404 in Responses format, code `unsupported_operation` |
| anything else | 404, plain JSON error |

## Client authentication

When `server.api_key` is set, a request must carry it in `Authorization: Bearer <key>` or `x-api-key`; either match is enough. When it is unset every request is accepted, and `check` warns unless `server.listen` is a loopback address.

## The upstream request

1. **Body.** The whole body is read into memory: retries, the model rewrite, and the affinity key all need it. `server.max_body_bytes` (default 32 MiB) bounds it (413), and `server.max_inflight` (default 64) bounds concurrent requests and so total buffered memory (503).
2. **Edits.** Each attempt starts from the original client body. When the target's upstream model differs from the client's, the model name is rewritten in place as a JSON point edit; then the provider's overrides run. A multipart body has only the bytes of its `model` field's value replaced; the form is never re-encoded. The body is never deserialized into a struct, so unknown fields survive.
3. **URL.** The endpoint's `base_url`, plus the operation's fixed suffix, plus the client's query string (Claude Code's `?beta=true` reaches the provider). `base_url` is what the protocol's official SDK takes, the value users already put in `OPENAI_BASE_URL` or `ANTHROPIC_BASE_URL`, and the suffix completes it the way that SDK does. For `chat`, `responses`, and `images` the base URL carries the version (`https://api.deepseek.com/v1`) and the suffixes are `/chat/completions`, `/responses`, `/alpha/search`, `/images/generations`, `/images/edits`, `/models`. For `anthropic` it does not (`https://api.anthropic.com`, `https://openrouter.ai/api`) and the suffixes are `/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`.
4. **Headers.** Client headers are copied, except hop-by-hop headers, `Host`, `Content-Length`, and every client credential (`Authorization`, `x-api-key`). The provider's `headers` map then applies: a string sets a header, `null` removes it. Last, the provider key goes into the header the endpoint's `auth` names, and it wins over `headers`. For `anthropic`, a missing `anthropic-version` is set to `2023-06-01` because the Anthropic API rejects requests without it; this is the only header verbatim adds on its own, and `headers` can override it. A request without `User-Agent` is sent with an empty one, so Go's default is not added. `headers` cannot set `Content-Length`, `Host`, `Transfer-Encoding`, or `Connection`.

## Overrides

A provider may declare static edits applied to every request it receives:

```yaml
overrides:
  - delete: metadata
  - set: {path: temperature, value: 1}
```

Paths are plain dotted paths (`\` escapes a dot, a leading `:` makes a segment an object key); gjson query and wildcard syntax (`| # @ * ?`) is rejected. A `set` value must be plain JSON. Operations run in order, each attempt starting from the original body, so one provider's edits never leak into another's attempt. Values are literals: no templating, no conditions, no access to the request. They exist for service quirks (a relay that rejects an unknown field, OpenRouter's `provider.quantizations`), not as a conversion mechanism. An edit valid only for some requests, such as `stream_options` for streaming ones, cannot be expressed, on purpose.

The validator rejects:

- `model` or `stream` at any depth: they drive routing and the usage parser.
- Any path under `system`, `tools`, or `messages` on a provider with an `anthropic` endpoint: that would change the preserved-thinking prefix. Static edits elsewhere are identical on every turn and do not.
- Two paths that overlap (one a prefix of the other), so the result does not depend on ordering mistakes.
- Any override on a provider with an `images` endpoint. An image request may be multipart, which JSON edits cannot address, and an override that applies to some of a provider's requests and not others would be hidden behavior. Put the image endpoint in a provider of its own.

## The response

Status, headers (except hop-by-hop), trailers, and body are copied unchanged. The transport neither requests nor transparently decodes content encodings, so the client gets exactly what the provider sent. Server-sent events are flushed as they arrive. verbatim does not add `stream_options.include_usage` to Chat Completions requests; a stream without usage logs null token counts.

**The point of no return** is committing the upstream status and headers to the client. After that nothing is retried. A cut stream is forwarded as a cut stream and the client connection is aborted rather than closed cleanly, so the client's SDK sees an error instead of a truncated success. An error event inside a `200` stream is forwarded as is.

**The usage tee** observes the body for the log: usage, and the response id that Responses ownership needs. It never modifies what the client receives, and its parse failures never affect forwarding. Its rules:

- It decompresses its own copy for `gzip`, `deflate`, `br`, and `zstd`. All four occur in practice: Node's fetch advertises them and DeepSeek streams in `zstd`. Other or stacked encodings yield null usage.
- It parses server-sent events when the response is `text/event-stream`, one JSON document otherwise, and follows the request's `stream` flag when there is no `Content-Type` (some relays answer a stream request with one JSON body).
- It blanks every JSON string longer than 4 KiB in its copy, so image payloads are parsed in bounded memory. A non-streamed body still above 8 MiB yields null usage; a single event above 1 MiB is skipped.

## Errors verbatim produces

Errors verbatim generates itself use the inbound protocol's native shape so SDKs parse them, and carry `x-verbatim-error: <code>`, which a forwarded upstream error never has.

| Protocol | Body |
|---|---|
| `chat`, `responses`, `images` | `{"error":{"message":"...","type":"verbatim_error","code":"<code>"}}` |
| `anthropic` | `{"type":"error","error":{"type":"api_error","message":"<code>: ..."}}` |

| Code | Status | When |
|---|---|---|
| `unauthenticated` | 401 | client key missing or wrong |
| `no_model` | 400 | body has no string `model` |
| `no_route` | 404 | no route serves this model and protocol |
| `unknown_response_id` | 404 | `previous_response_id` has no recorded owner, or the owner's provider no longer serves the protocol |
| `unsupported_operation` | 404 | Responses resource operations, WebSocket |
| `body_too_large` | 413 | above `server.max_body_bytes` |
| `too_many_inflight` | 503 | above `server.max_inflight` |
| `all_upstreams_failed` | 502 | every candidate failed and the last attempt got no upstream response |
| `all_upstreams_open` | 502 | every candidate skipped by an open breaker |
| `internal_error` | 500 | the ownership lookup failed |
