# Routing and failover

How a request picks its upstream: providers and endpoints, routes and targets, the attempt order, failover, the circuit breaker, and the `/v1/models` listing. Session pins, which put one target first, are in [affinity.md](affinity.md). Field-level reference is [`verbatim.example.yaml`](../verbatim.example.yaml).

## Providers and endpoints

A provider is one upstream service (a vendor's API, a relay) with one key, one set of `headers`, one list of `overrides`, and an `endpoints` map from protocol (`chat`, `responses`, `anthropic`, `images`) to `base_url`. A service that speaks several protocols is one provider with several endpoints. Two uses of one service that need different keys or overrides are two providers (YAML merge keys make this cheap).

An endpoint is named `provider.protocol` (`openrouter.anthropic`), so provider names cannot contain `.`. Breakers, model lists, and the Providers view counters are kept per endpoint: a relay's image channel failing does not stop its chat channel.

`auth` and `header_timeout` belong to the endpoint, because their defaults differ by protocol. `auth` defaults to `bearer` for `chat`, `responses`, and `images` and to `x-api-key` for `anthropic`. A non-streamed image is answered only once it is drawn, so an `images` endpoint usually needs a `header_timeout` of minutes, while a chat endpoint that silent should fail over.

## Routes and targets

`routes` is an ordered list. A request takes the first route whose `model` pattern matches the client's model name **and** that has at least one target serving the request's protocol. A route that matches the name but cannot serve the protocol is skipped, so a later route can still take the request. No qualifying route is a 404 `no_route`.

Patterns are globs whose only wildcard is `*`, matched case-sensitively against the whole name; `*` also matches `/`, so `anthropic/*` works for OpenRouter-style names. Exact names go first and `"*"` last. A route's pattern is its identity and must be unique.

A target names a provider and serves every protocol that provider has an endpoint for, or names one endpoint (`relay.responses`) and serves only that protocol. Naming the endpoint is how a route uses part of a service; a route that lists a whole provider also takes requests of protocols a later route was meant to handle. Within a protocol a target is identified by (provider, upstream model), and two identical targets in one route are an error.

A target's `model` is the upstream name, defaulting to the client's name. It may contain one `*` when the route pattern has exactly one, and that `*` becomes what the pattern's `*` matched: under `gpt-*`, `model: openai/gpt-*` sends `gpt-6.1-sol` as `openai/gpt-6.1-sol`. Names that differ in any other way get an exact-name route with the upstream name written out. There is no provider-level name mapping and no regex rewriting.

## Attempt order

From the matched route's targets for this protocol:

1. If a Responses continuation names an owning target ([affinity.md](affinity.md#response-ownership)), that target is the whole list.
2. Otherwise group by `priority`, ascending. Within a tier, `balance: weighted` (default) draws without replacement by `weight`, and `round_robin` rotates from a counter per (route, protocol, tier), ignoring weights. Concatenate the tiers.
3. A pinned target among the candidates moves to the front.
4. Try the list in order, each target at most once. The breaker is checked at dispatch time, not when the list is built; a target whose endpoint breaker is open is skipped and the skip is recorded. If every candidate is skipped, the request fails at once with 502 `all_upstreams_open`. There is no bypass: the half-open probe is what gives a recovering endpoint its chance.

A tier is exhausted before the next is touched. (new-api, by contrast, steps down one tier per retry and ends up retrying the last tier repeatedly.)

## Failover

Before anything is committed to the client, these end the attempt and move to the next candidate:

- No response headers: connection, TLS, or DNS failure, or the endpoint's `header_timeout`.
- 408, 409, 425, 429, 500, 502, 503, 504, 529.
- 401, 403: key invalid, exhausted, or not entitled to the model. Logged as a warning, since an operator has to act.
- 404: the provider does not serve this model or operation. Logged as a warning.

Every other status is returned to the client unchanged and ends the loop: every 400 (malformed requests, Anthropic signature errors, spend limits), 402, 413, 422. Retrying the same body elsewhere would fail the same way or hide the real error. The client owns its history and is the only party that can repair it.

Never a failover: anything after the status and headers are committed, and error events inside a `200` stream (forwarded, logged as outcome `upstream_error_event`, and not fed to the breaker).

A target whose request cannot be built (the model rewrite or an override fails on this body) counts as a failed attempt without touching the breaker, and the loop moves on.

**`failover_on`** adds statuses that fail over from one provider, for every operation of every endpoint it has. It exists for services that report a missing model with a status that also means a bad request: Alibaba Bailian answers an unknown model with 400 where others use 404. Only the status is matched, never the error text, so a genuinely malformed request also fails over and then fails at the remaining candidates. A listed status behaves like a 404: it moves the pin in `prefer` mode and does not feed the breaker.

The body of an abandoned attempt is read (bounded by `server.idle_timeout` and 1 MiB) so it can be returned if that attempt turns out to be the last, and its first `log.error_excerpt_bytes` go into the log.

## Circuit breaker

One breaker per endpoint. Only signals that the endpoint as a whole is unhealthy count: no response headers, 429, 500, 502, 503, 504, 529. A 401, 403, 404, or `failover_on` status fails over this request without counting, because it is as likely to concern one model, one key's entitlement, or one operation (Ollama serves Messages but not `count_tokens`; most relays do not serve `search`). `count_tokens` and `search` never count, whatever their status.

`breaker.failure_threshold` consecutive counted failures open the breaker for `breaker.cooldown`. After the cooldown exactly one request is admitted (half-open). A committed primary response closes the breaker and resets the counter; a counted failure reopens it for another cooldown. Any other end of the probe (a non-counted status, a client cancel, an auxiliary operation) leaves the breaker open with its cooldown elapsed, so the next request probes again.

## Exhaustion

When every candidate has failed or been skipped:

- If the last attempted target got an upstream response, that response goes to the client unchanged. A single-provider route whose key is rejected shows the client the provider's own 401, not a synthetic 502. The body was buffered up to 1 MiB; a longer one is cut there and logged as `truncated`.
- Otherwise verbatim answers 502 in the native error format, listing each candidate as `provider: error` (skipped ones as `breaker open`).

Either way the response carries `x-verbatim-attempts: <n>`.

## Model listing

Clients fill their model pickers from `GET /v1/models`, so the listing has to be real. It is assembled per route, in configuration order:

- An exact-name route lists its own `model`.
- A pattern route with `discover: true` (the default) lists the client names behind the ids its targets' endpoints report. For a target without `model`, an id matching the pattern is the name. For a target whose `model` has a `*`, an id matching that `model` maps back through the `*`: under `gpt-*` with `model: openai/gpt-*`, the listed `openai/gpt-7` becomes `gpt-7`. Targets with a `*`-less `model` are skipped, since such a name says nothing about which client names lead to it. Each name is then kept only if routing would actually send it to this route; otherwise an earlier route owns it.
- A pattern route with `discover: false` lists nothing.

Names are deduplicated, first occurrence wins. Each entry carries the union of the fields OpenAI and Anthropic clients read, so either parser accepts the response. A listed name is not a promise that every protocol can serve it.

Only endpoints that a discovering route reaches through a target whose `model` is absent or has a `*` are fetched. Each list comes from `{base_url}` plus the protocol's model-list suffix, following next-page cursors (`has_more`, `last_id` → `after_id`) up to 10 pages. Lists are cached per endpoint for `models.cache_ttl`, fetched in the background at startup and after a reload, and refreshed when stale. A failed fetch keeps the previous list and retries every minute (or every `cache_ttl`, if shorter); it warns when an endpoint starts failing and logs once when it recovers, so an endpoint without a model list (DeepSeek's Anthropic endpoint returns 404) warns once per start. `/v1/models` reads the cache and never waits on a refresh, except during the first round after startup or a reload, bounded by `models.timeout`, so an early client does not get an empty list. `/v1/models` requires the client key like any other `/v1` path.

## Open questions

- The defaults for `header_timeout`, `idle_timeout`, `failure_threshold`, and `cooldown` are placeholders to be tuned against real agent traffic. Long prompts with large caches can take over a minute to first byte.
- Whether 404 should fail over by default. It helps with relays that lack a model and is wrong when a provider returns 404 for a malformed path. Current choice: it fails over, with a warning, and never feeds the breaker.
