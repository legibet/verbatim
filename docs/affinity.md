# Session affinity

An agent session has to stay on one upstream. Anthropic thinking signatures are validated by their issuer and, on current models, bound to the issuing account; prompt caches live on one upstream; Responses server-side state lives on one upstream. Affinity is applied before selection and overrides priority and weight ([routing.md](routing.md#attempt-order)).

verbatim pins its immediate upstream. When that upstream is itself a router (OpenRouter balances across backends), continuity below it is the upstream's business.

## Session keys

Which request field identifies a session is a property of the client, so it is configuration, and verbatim ships no built-in rules. `affinity.rules` is ordered; the first rule whose `match` (optional `protocol` and `model` glob) fits the request is used, and later rules are not tried even if its sources are all empty. Its `key` is a list of sources, and the first that yields a non-empty value is the key:

- `{body: <gjson path>}`: a string or number in the request body, as sent.
- `{header: <name>}`: a request header.

The example configuration carries rules for today's common clients. Claude Code sends `metadata.user_id`. Codex sends a `session-id` header and `prompt_cache_key` with the same value, and its `/alpha/search` carries that value as `id`, so one rule keeps a search on the session's upstream. Pi and omp can send `x-session-affinity`. OpenAI SDK users often set `user` or `prompt_cache_key`. When a client offers both a header and a body field, the header goes first, because it never takes part in prompt caching or thinking-prefix checks. These values identify a client session, not necessarily one conversation; two conversations under one `user` share a pin, which costs only balance.

Without a key, `affinity.fallback` applies:

- `prefix_hash` (default): SHA-256 over the protocol id, the system prompt, and the first user message, each as the exact bytes sent. For `anthropic` that is `system` and the first `messages[]` entry with `role: user`; for `chat` the first `system` or `developer` message and the first `user` message; for `responses` `instructions` and the first `input[]` user item (or `input` itself when it is a string). No user message means no affinity, and `images` requests have no prefix to hash, so they get affinity only from a rule. This is best effort: two conversations with the same opening share a pin, and a client that rewrites its opening mid-conversation (compaction, a changing system prompt) moves to a new key. Clients that need a guarantee should send a session id and have a rule name it.
- `none`: no affinity.

The log stores only the SHA-256 of the key, never the value.

## Pins

A pin is keyed by (protocol, affinity key, client model name): one session touches several models and each is pinned separately, and the same key under two protocols has two pins because the candidates differ. Its value is a target (provider, upstream model).

The pin is **reserved at selection time**, not after success. A request that finds no pin atomically reserves one for the target it is about to try (the first in attempt order that its breaker would admit, since the others are skipped at dispatch), so the parallel first requests that agent clients fire at startup land on the same target. If the attempt fails over, the pin moves to the target that is finally committed. If the request fails entirely and nothing has been committed under this pin yet, the reservation is released; a pin that has served a committed response stays.

Pins live in memory with LRU eviction and `affinity.ttl` expiry, refreshed on every use. They survive a configuration reload but not a restart. After a restart a session's next request re-pins by ordinary selection, and the client handles any resulting signature error (Claude Code strips earlier thinking and retries; other clients fail the turn).

## Modes

- `prefer` (default): if the pinned target is skipped (open breaker) or fails, selection continues with the remaining candidates and the pin moves to whichever target is committed. The log row records the move. The new target may answer this very request with a signature or state error, and verbatim does not hide it.
- `strict`: when an existing pin names a target among the current candidates, that target is the only one tried; if it is skipped or fails, verbatim returns that failure (the upstream response unchanged, or 502). This includes a pin another request has just reserved and not yet committed: concurrent first requests produce signatures and caches at the same moment, and if this one failed over in its own order it could commit a different target and split the session. If the reserving request fails over, this one fails with the pinned target's error, and the client's retry follows the pin to where the reserving request committed. A request with no pin (first request, or expired or evicted pin), or whose pinned target the routes no longer offer, fails over normally like in `prefer`.

`count_tokens` and `search` follow an existing pin and never create or move one, so a search that fails over does not pull the conversation away from the upstream holding its prompt cache.

## Response ownership

A Responses client can continue a conversation by reference instead of replaying it: `previous_response_id`, or a `conversation` id. That state exists only on the upstream that produced it, and no session key can identify it because the id is new on every turn. So verbatim records the id of every committed 2xx Responses `create` response (the response object's `id`; for streams, the first event carrying the response object) with the target that produced it, in the SQLite table `owned_responses`. The row is written synchronously, as soon as the id is seen and before those bytes reach the client, so a client cannot continue from an id verbatim has not yet recorded.

A request that carries `previous_response_id` or `conversation` has its owner as the only candidate, and pins are neither looked up nor moved. The owner need not still be a target of the matched route, but its provider must still have an endpoint for the protocol; otherwise, and for an unknown id, the answer is 404 `unknown_response_id` rather than a guess. The owner's breaker still applies. Ownership rows follow `log.retention`.

This is the one place verbatim keeps state beyond a cache. Any gateway that serves more than one Responses upstream needs it.

## Open questions

- Whether `affinity.ttl` should default higher than one hour. A working session with a lunch break loses its pin.
