# Usage log and pricing

What verbatim records about each request, how token usage is read from responses, and how a cost is put on it.

## Request log

One SQLite database (`log.path`, WAL mode) with tables `requests` and `owned_responses` ([affinity.md](affinity.md#response-ownership)). A row is written after the response completes or fails, off the request path, through a bounded channel to a single writer goroutine. If the channel is full the row is dropped and a `log_dropped` counter shown in the UI goes up; logging never delays a request. The channel is drained on shutdown. Requests rejected by client authentication are not logged, so an unauthenticated client cannot fill the database. Rows older than `log.retention` are deleted hourly. Request and response bodies are never stored.

A row records, beyond the obvious request fields:

- **How it was routed**: matched route, candidates, computed order, ownership target, pin state (`none`, `hit`, `reserved`, `moved`), and breaker skips, as `routing_json`; every attempt with its status or error, duration, and error excerpt, as `attempt_log`.
- **Where it went**: `provider` and `upstream_model` of the committed target, or of the last attempted one; skipped targets never appear there.
- **What the client got**: `status` (499 when the client left before anything was sent) and `outcome`: `completed`, `upstream_error_event` (error event in a `200` stream), `truncated` (stream cut), `client_cancelled`, or `gateway_error`.
- **Timing**: `attempts_ms` spent in failed attempts, `ttfb_ms` to the committed headers (including `attempts_ms`), `total_ms` to the last byte.
- **Usage**: normalized token columns plus the last usage object received, unmodified, in `usage_json`. For an Anthropic stream that is usually the final `message_delta`, so the columns can hold counts from `message_start` that `usage_json` lacks.
- **Cost**: `cost_usd`, plus `price_key` and the unit prices used (`price_json`), so history stays stable when the price table changes. `price_key` is set whenever lookup finds an entry, even when that entry cannot price the request and `cost_usd` stays null.

`count_tokens` rows carry `input_tokens`. `count_tokens` and `search` rows never count in usage aggregates or cost.

## Usage extraction

The tee ([forwarding.md](forwarding.md#the-response)) keeps the last usage object it sees in the committed response. In streams it comes from:

- `chat`: the last chunk with a non-null `usage` (it may have `choices: []` and precede `[DONE]`).
- `responses`: the terminal event (`response.completed`, `response.incomplete`, or `response.failed`).
- `anthropic`: `message_start` merged with `message_delta`; delta values are cumulative, so later ones replace earlier ones.
- `images`: `image_generation.completed` or `image_edit.completed`.

Normalization into the row:

- `input_tokens` is always uncached input. Anthropic reports it that way. Chat Completions and Responses report a prompt count that includes cached and cache-written tokens, so verbatim subtracts `cached_tokens` and `cache_write_tokens`, but only when the provider reports the details object at all; without it, `input_tokens` holds the full prompt count and the cache columns are null. The DeepSeek dialect's top-level `prompt_cache_hit_tokens` counts as cache reads too.
- `cache_write_1h_tokens` is the 1-hour-TTL subset of cache writes, which only Anthropic has. OpenAI's cache lifetime is 30 minutes with no 1-hour variant.
- `reasoning_tokens` is recorded as reported and is part of `output_tokens`.
- Image requests also record `input_image_tokens` and `output_image_tokens`, subsets of input and output. Some relays report only totals. When input details are missing, a generation (text prompt only) gets 0 image input tokens, and an edit counts all its input as image input, since its images outweigh its prompt. That overstates an edit's cost slightly (for gpt-image-2, 0.3% for 20 prompt tokens next to 256 image tokens).

Apart from those two image rules, a missing field is null, never zero, and nothing is estimated.

Unverified: with `n` above 1 in an image stream, the last completed event is kept, as everywhere else, but whether that event reports the whole request or one image is undocumented and has not been observed.

## Pricing

Pricing exists only to put a cost on the log. It is configured list prices per token, image tokens included. It cannot know what a relay actually charges, and it does not price hosted tools, per-picture image billing, or service tiers. The UI labels costs that way and shows, for every aggregate, how many requests had no price.

### Sources

`verbatim pricing sync` downloads each URL in `pricing.sources` into `pricing.cache`, named by the URL's last path element (two sources with the same file name are an error). The expected shape is the llm-metadata / models.dev provider file: `models.<id>.cost` with `input`, `output`, `cache_read`, `cache_write`, and optionally `reasoning`, `tiers`, and `currency`, plus provider-level `billing.cacheWrite1h`, the 1-hour cache write price as a multiple of `input`. Models that are not in USD, lack an `input` price, lack both `output` and `image_output`, or have a non-context tier type are skipped with a warning. When two sources define one model, the later source wins and `check` warns.

`pricing.overrides` is a local file in the same shape, merged over the synced data. An override replaces a model's whole `cost`; the source's `billing.cacheWrite1h` still applies unless the overrides file sets its own. The provider files have no image prices, so image models are priced here, with `image_input` and `image_output` (and `output` may be omitted for a model without text output):

```json
{"models": {"gpt-image-2": {"cost": {"input": 5, "image_input": 8, "image_output": 30}}}}
```

The running server reloads the price table when one of the cache files named by `pricing.sources`, or the overrides file, changes, checked at most once a minute. The UI's Sync prices does what `pricing sync` does, inside the server, and installs the new table at once; one sync runs at a time, for at most a minute. A cost is computed when its row is written, so a sync prices new requests only.

### Lookup

Prices are looked up by the **client's model name**, so a relay name that differs from the official one is priced by the name the user chose to expose:

1. Exact match of the client model name.
2. Its `pricing.aliases` entry, exact match.
3. The upstream model name, then its alias.
4. Otherwise no price: `cost_usd` is null and the row is still written.

There is no prefix or fuzzy matching. A dated model name the table does not know needs an alias or an override. Aggregates count as unpriced the rows that have usage but no cost. The unpriced-models list shows which client names in the range the current table still cannot price: a request logged before its price was added keeps no cost and still counts as unpriced, but its name leaves the list.

### Cost

Per million tokens:

```
cost = input_tokens                                 * input
     + cache_read_tokens                            * cache_read
     + (cache_write_tokens - cache_write_1h_tokens) * cache_write
     + cache_write_1h_tokens                        * input * cacheWrite1h
     + output_tokens                                * output
```

A null `input_tokens` or `output_tokens` leaves the request unpriced; null cache columns count as zero. Missing `cache_write` prices cache writes at `input`; missing `cacheWrite1h` prices 1-hour writes at `cache_write`. Cache reads with no `cache_read` price leave the request unpriced. Reasoning is priced as output. With `tiers`, the context count is `input_tokens + cache_read_tokens + cache_write_tokens`, the highest tier whose bound is below it applies, and a price missing from a tier inherits the base `cost`.

For image requests (which report no cache tokens), image tokens are priced at `image_input` and `image_output` and the rest at text prices:

```
cost = (input_tokens  - input_image_tokens)  * input
     + input_image_tokens                    * image_input
     + (output_tokens - output_image_tokens) * output
     + output_image_tokens                   * image_output
```

An image request is priced only by an entry with an image price. A token count with no price for it leaves the request unpriced: image tokens under an entry without that image price, text output under a model without a text output price, or a null `output_image_tokens`, unless the model has no text output price (gpt-image-2), in which case all output is image output. Image tokens above the reported total leave it unpriced too.
