# verbatim

A self-hosted LLM API gateway in Go: routes client requests to upstream providers, fails over, keeps sessions on one upstream, logs usage and cost. It never converts between API formats. User-facing overview: [README.md](README.md).

## Docs

The docs under `docs/` are the behavior contract. Before changing an area, read its doc; a behavior change updates that doc in the same commit.

- [docs/architecture.md](docs/architecture.md): principles, non-goals, request lifecycle, package map, in-memory vs SQLite state, config loading and reload, CLI. Read first when adding any behavior.
- [docs/forwarding.md](docs/forwarding.md): protocols and paths, what verbatim changes in a request, provider overrides, response streaming and the usage tee, verbatim's own error codes.
- [docs/routing.md](docs/routing.md): providers and endpoints, routes and targets, attempt order, failover statuses, circuit breaker, `/v1/models`.
- [docs/affinity.md](docs/affinity.md): session keys, pins, `prefer`/`strict`, Responses response ownership.
- [docs/usage-and-pricing.md](docs/usage-and-pricing.md): request log rows, usage extraction and token normalization, price lookup and cost.
- [docs/web-ui.md](docs/web-ui.md): UI scope, stack, style, views, `/ui/api`.

## Rules

- The principles in architecture.md are hard constraints. A request body is forwarded as sent except for the model name and declared overrides; nothing is retried after the response is committed; every behavior is visible in the configuration. A feature that needs to break one of these is a design question for the user, not something to work around.
- Edit forwarded bodies with gjson/sjson point edits. Keep protocol-specific JSON paths inside `internal/protocol`.
- A configuration change also updates `verbatim.example.yaml`, the annotated reference for every field, and the validation in `internal/config`.
- Prefer the standard library; ask before adding a Go or npm dependency.
- Test against `httptest` fake upstreams. Failover, affinity, and usage parsing carry the risk, so changes there come with tests.
- `golangci-lint run` must pass. Fix what it reports; a `//nolint:<linter> // <reason>` is only for a false positive.
- A change under `web/` ends with `pnpm build` and the rebuilt `internal/ui/dist` committed alongside it; the Go binary embeds that directory.

## Commands

```sh
go test -race ./...
go vet ./...
golangci-lint run ./... # config in .golangci.yml; also checks gofmt and goimports
cd web && pnpm build    # type-checks, then writes internal/ui/dist
cd web && pnpm dev      # UI dev server; needs a running verbatim at $VERBATIM_URL (default http://127.0.0.1:8080)
```

## Releasing

Versions are semver tags, `v0.x` until the configuration format is stable. To release, write notes for users (what changed, and any configuration change they must make) and push them as an annotated tag:

```sh
git tag -a v0.2.0 -F notes.md && git push origin v0.2.0
```

The Release workflow tests, builds an archive per platform, and publishes a GitHub release with the tag message as its notes. CI runs on every push and pull request, including a check that `internal/ui/dist` matches a fresh build of `web/`.
