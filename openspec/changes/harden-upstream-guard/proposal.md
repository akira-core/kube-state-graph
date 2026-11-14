# Proposal

## Why

A performance and security audit of the upstream query layer (`pkg/promql`) found that it trusts its inputs and its upstream more than it needs to:

- **The query-result cache holds the full rendered query as its key.** An empty result costs one series unit, so about 4000 requests with ~16 KiB queries can pin ~1.6 GiB, and expired entries stay resident until budget pressure evicts them.
- **The shared upstream call of a coalesced miss runs on the leader's context.** If the leader's request is cancelled, every waiter fails with it. The rule and spec say the waiter "retries once as leader", but the retry can join another caller's flight, so the text does not match the code.
- **A slow query holds its store slot until the build deadline.** The upstream keeps evaluating after the build has given up, because it never learns the deadline.
- **The unauthenticated `/readyz` sends a bare `up` to every probe backend on every call.** The query is not coalesced, and the response size grows with the number of scrape targets.
- **Smaller gaps:**
  - The upstream response body has no size bound.
  - Basic auth is re-attached after a same-host `https`→`http` redirect.
  - `escapeLiteral` relies on the request parser to reject control characters.
  - The routing-file reloader seeds its change digest from a second read of the file, not from the bytes the live table was parsed from.
  - Per-backend in-flight and slot-wait series outlive a reload that removes their backend.
  - Every failed query logs, and every span carries, the full rendered query. With an over-long request matcher that output can be unbounded.
- **The fan-out merge hashes every series of every query.** This includes single-backend deployments and cache hits: about 100 ms and 25 MB per 100k-series leg.

## What Changes

- **Cache key is a digest.** The query-result cache and the miss-coalescing key use the SHA-256 digest of the rendered query, plus the store id and the evaluation instant. The cost of an entry adds one unit per KiB of rendered query. `put` releases expired entries from the least-recently-used end before it evicts live ones. `kube_state_graph_query_cache_series` keeps reporting resident series.
- **Coalesced misses outlive their leader.** The shared upstream call runs under `context.WithoutCancel` of the caller that started it. Its deadline is the latest deadline among its waiters, and none if any waiter has none. It is cancelled when the last waiter leaves. A waiter whose own context ends returns its own context error, as before. There is no retry loop.
- **Evaluation timeout follows the deadline.** Every upstream instant query whose context carries a deadline sends `timeout=<remaining − 500ms, whole seconds>`, so the store stops evaluating, and the slot frees, before the build gives up. No slot quota is added. Cross-request fairness is left to admission control.
- **The readiness probe is `count(up)`.** It answers one series per backend instead of one per scrape target. The emptiness semantics are unchanged: an `up`-less store answers an empty vector either way. `Router.ProbeAll` coalesces concurrent probes, and a completed probe's outcome is reused for 1 s, unless the probe was cut short by its own deadline. Neither the probe nor the retention `up{}` probe ever touches the query-result cache.
- **Response size is bounded.** A new `--upstream-max-response-bytes` / `KSG_UPSTREAM_MAX_RESPONSE_BYTES` (default 512 MiB, `0` disables) bounds every upstream response body. Exceeding it is an upstream query error, the same as any other: required legs fail closed and optional legs degrade. The library default is the same, through `promql.DefaultMaxResponseBytes` and `promql.WithMaxResponseBytes`.
- **Credentials stay on the configured scheme.** Basic-auth credentials are attached only to requests with the configured scheme and host, so an `https`→`http` redirect to the same host never carries them in clear.
- **`escapeLiteral` escapes control characters itself.** It escapes `\n`, `\r` and `\t` by name and every other C0 / DEL byte as `\xHH`, so it no longer relies on the parser. The escaper is one package-level `strings.Replacer`.
- **The reloader is seeded from the parsed bytes.** `backendsfile.ReadWithContent` returns the bytes a table was parsed from. `ReloaderOptions.Content` seeds the reloader's digest from them. With no `Content`, the first tick re-parses and swaps once.
- **Stale per-backend gauges are removed.** A routing-table swap deletes the `kube_state_graph_upstream_inflight` and `..._upstream_slot_wait_seconds` series of backends the new table no longer names. It also ignores later observations for those names, so a query still in flight on a removed backend cannot resurrect its series.
- **Logs and spans carry a bounded statement.** The `prometheus.query` span and the client's query logs carry:
  - `db.statement` / `query`, truncated to 2 KiB on a UTF-8 boundary;
  - the full length, as `kube_state_graph.query_bytes` / `query_bytes`;
  - a 12-hex-digit SHA-256 prefix, as `kube_state_graph.query_sha256` / `query_sha256`.
- **The single-backend merge is a fast path.** When exactly one part carries samples, the fan-out merge returns it without hashing. It allocates only to drop a nil sample, and never filters a shared cached vector in place. With several parts, a hash match is confirmed with `Metric.Equal`.

## Capabilities

### Modified Capabilities

- `upstream-query-cache`: digest key, query-length cost, expired-entry sweep, coalescing that outlives its leader, and probe coalescing.
- `upstream-backend-routing`:
  - `count(up)` probe;
  - credentials limited to the configured scheme;
  - reloader seeded from the parsed bytes;
  - stale per-backend limiter series removed on reload;
  - per-query evaluation timeout;
  - response-size bound.
- `otlp-observability`: bounded `db.statement` plus length and digest attributes.
- `cluster-topology-source`: the probe query under request selectors is `count(up)`, and literal escaping covers control characters itself.

## Impact

- **Code:**
  - `pkg/promql`: `client.go`, `guard.go`, `querycache.go`, `fanout.go`, `router.go`, `selector.go`, `queries.go`, `backendsfile/`;
  - `pkg/kubegraph/engine.go`, where `Probe` now renders its query;
  - `internal/config`: new flag and env var, and `ReadBackendsFile` returns content;
  - `internal/observability/adapters.go`;
  - `cmd/kube-state-graph`.
- **Wire:** No response body changes, and no golden moves. `pkg/promql/testdata/render-baseline.txt` changes its `up` line to `count(up)`.
- **Dependencies:** No new dependencies. `crypto/sha256` and `context.WithoutCancel` are stdlib, available on Go 1.27.1.
- **Embedders:**
  - `promql.DefaultClientFactory` takes optional `...Option`; this is source-compatible.
  - `ReloaderOptions` gains `Content`. A caller that does not set it gets one extra swap on the first tick instead of a possibly wrong digest.
