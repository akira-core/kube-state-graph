# Proposal

## Why

VictoriaMetrics answers `503 Service Unavailable` when its search queue
overflows (`-search.maxConcurrentRequests` exhausted for longer than
`-search.maxQueueDuration`). One `/v1/graph` build puts ~40–50 PromQL queries
in flight at once (37 unbounded first-wave topology legs plus a
`scopeConcurrency`-bounded QoS wave), a `/v1/storage-graph` build up to ~100,
and nothing bounds this ACROSS requests or replicas. A handful of concurrent
requests is enough to overflow the upstream queue, and every overflowed leg
fails the build as `502 upstream`. Identical requests (dashboards polling the
same window, several viewers of the same view) also re-issue byte-identical
queries, doubling upstream load for no new information.

## What Changes

- **Per-backend concurrency limit.** Every upstream PromQL query to one backend
  store acquires a slot from a process-wide semaphore owned by that store
  before the HTTP round-trip. A query that finds the store saturated WAITS for a
  slot until its context ends; an expired wait surfaces through the existing
  build-deadline path (`504 timeout`), never as a new error class. New flag
  `--upstream-max-concurrency` / `KSG_UPSTREAM_MAX_CONCURRENCY` (default **32**
  per backend store; `0` disables). Readiness and retention `up{}` probes
  bypass the limit.
- **In-process LRU query-result cache.** Successful upstream results are cached
  keyed by `(backend store, rendered query, evaluation time)`, bounded by a
  total-series budget (`--query-cache-max-series`, default **100000**; `0`
  disables) and a per-entry TTL (`--query-cache-ttl`, default **60s**).
  Concurrent misses on one key are coalesced into one upstream query. Errors
  are never cached. Probes bypass the cache.
- **Request end-time alignment.** New flag `--end-align` / `KSG_END_ALIGN`
  (default **30s**; `0` disables) floors a graph request's `end` down to the
  grid and shifts `start` by the same amount, so the window length is
  unchanged and requests issued within the same grid step render identical
  queries and share cache entries. Applies to `/v1/graph` and
  `/v1/storage-graph`. **BREAKING**: with the default on, upstream PromQL is no
  longer evaluated at the caller's verbatim `end`; a response may reflect data
  up to one grid step older than requested. Operators restore passthrough with
  `--end-align=0`.
- **Self-metrics** (new series only, existing label sets untouched): per-backend
  in-flight gauge and slot-wait histogram; cache hit / miss / coalesced /
  eviction counters and a resident-series gauge.
- **Library defaults match the server.** An embedder gets all three features
  with the same defaults without writing any option: `promql.NewRouter` with
  no `RouterOption` applies the default limit and cache;
  `kubegraph.New` / `NewRouted` apply the default alignment, and
  `kubegraph.New` wraps a plain (non-routing) `Querier` in the same guard so a
  `*promql.Client` passed directly is limited and cached too. The defaults are
  exported constants (`promql.DefaultMaxConcurrency`,
  `promql.DefaultQueryCacheMaxSeries`, `promql.DefaultQueryCacheTTL`,
  `kubegraph.DefaultEndAlign`) shared with the server's flag defaults. Each
  feature is disabled explicitly (`WithMaxConcurrency(0)`,
  `WithQueryCache(0, 0)`, `kubegraph.Options` field `< 0`). **BREAKING for
  embedders** (e.g. `graph-api-gateway`): after upgrading, their builds are
  aligned, cached and limited unless they opt out. `pkg/build.New` stays the
  unguarded low-level constructor.

## Capabilities

### New Capabilities
- `upstream-query-cache`: in-process LRU cache of upstream query results —
  key, bounds, TTL, miss coalescing, bypass rules, observability.

### Modified Capabilities
- `upstream-backend-routing`: adds a per-backend-store concurrency limit
  requirement and extends routing observability with in-flight / wait metrics.
- `graph-api`: "Time-window passthrough" is revised to allow opt-out end-time
  alignment; "Deterministic response body" no longer states that v1 has no
  in-process cache.
- `storage-graph-api`: the storage endpoint adopts the same end-time alignment.

## Impact

- Code: `pkg/promql` (new limiter + cache wrapper around per-store clients,
  `RouterOption`s on `NewRouter`, new optional metrics upgrade interfaces),
  exported `promql.Guard` and `Default*` constants), `pkg/kubegraph` (window
  alignment helper, four new `Options` fields defaulting on, plain-`Querier`
  wrapping in `New`),
  `internal/api` handlers (apply alignment), `internal/config` (four flags),
  `internal/observability` (new metrics + adapters), `cmd/` wiring.
- Dependencies: none new — LRU is a small in-house `container/list` structure;
  miss coalescing uses `golang.org/x/sync/singleflight` (module already a
  direct dependency).
- Docs: `CLAUDE.md` load-bearing rules ("No server-side result cache", "No
  time-window alignment", "no singleflight"), `docs/BREAKING.md`,
  `docs/upstream-backend-routing.md`.
- Operations: memory grows with the cache budget; operators sizing VM's
  `-search.maxConcurrentRequests` should set `--upstream-max-concurrency ×
  replicas` at or below it.
