# Design

## Context

Every upstream query passes through `promql.guard`, one per backend store. The guard checks the query-result cache first, then coalesces misses with `singleflight`, then takes the store's FIFO slot, and finally calls `*promql.Client`. The fan-out (`fanoutQuerier.issue`) merges the per-backend parts with `mergeVectors`. `Router.ProbeAll` serves `/readyz` and bypasses the guard. `backendsfile.Reloader` hot-swaps the table.

This change hardens those pieces without moving a response body. Each decision below names the alternative it rejected.

## Decisions

### D1. Cache and flight keys are a SHA-256 digest (C7)

`cacheKey{store, digest [32]byte, ts, queryBytes}`. `queryBytes` is a pure function of the digest's input, so it never splits keys. It rides on the key so `put` can charge it. In-flight coalescing uses the same key.

- *Rejected: keep the string key and refuse to cache queries over 16 KiB.* A refusal still leaves a long query's string resident in the singleflight map for the length of the call, and it adds a threshold to tune.
- *Rejected: a 64-bit hash.* A collision would serve one query's result for another. SHA-256 makes that infeasible, and it is stdlib.

The cost is one SHA-256 per guarded call, about 7 µs for a 16 KiB query. That is negligible next to the upstream round trip.

### D2. Entry cost charges query length; the gauge stays series (C7)

`cost = max(1, len(vec)) + queryBytes/1024`. The resident key is now 32 bytes. The query-length charge bounds how many entries a burst of long, distinct, empty-result queries can occupy, which is the adversarial shape.

The cache tracks `size` (budget) and `series` (gauge) separately, so `kube_state_graph_query_cache_series` keeps meaning what it says.

- *Rejected: a separate byte budget and flag.* It is a second knob for an operator to size, and the series budget is already documented as the memory control.

### D3. `put` sweeps expired entries from the LRU tail (C7)

Before it evicts anything, `put` walks from the least-recently-used end and releases expired entries. It stops at the first live entry. Expiry is not counted as an eviction, which stays a budget-pressure signal.

An expired entry nearer the front, one that was read after insertion, is released by its next `get` or by eviction.

- *Rejected: a background sweeper goroutine.* An embedder would inherit a goroutine with a lifecycle to manage.
- *Rejected: a full scan on every `put`.* It is O(n) under the lock.

### D4. Coalesced fetch outlives its leader (P-3, option C)

The shared call runs under `context.WithCancelCause(context.WithoutCancel(leaderCtx))`. It keeps the leader's values, such as the trace parent, and drops its cancellation.

Each waiter that joins does two things:

- It increments a refcount.
- It extends the flight's deadline to the latest deadline among the waiters. A `time.AfterFunc` is re-armed for the new deadline. A waiter with no deadline makes the flight unbounded.

The shared context reports that deadline through `Deadline()`, so the client's evaluation timeout (D5) sees it.

When a waiter's own context ends, it leaves and returns its own context error. When the last waiter leaves, the flight is removed from the map and cancelled, so a later caller starts afresh. The deadline timer also removes the flight under the lock and checks for a re-arm first, so a caller never joins a flight that is already doomed.

A waiter that receives a context error from a flight cut by its deadline has a deadline no later than the flight's own. It therefore waits for its own context to end and returns that error. This keeps the 504 mapping, where returning the shared `context.Canceled` would have produced a 499.

There is no retry loop. The "retries once as leader" text is removed from the rule and the spec.

- *Rejected: keep leader-bound and only fix the text (option A).*
- *Rejected: retry while the caller lives (option B).* Both still turn one client's disconnect into a failure for every other request sharing the query.

### D5. Per-query `timeout=` from the context deadline (P-4, option b)

`Client.Instant` sends the Prometheus API `timeout` parameter when its context has a deadline. The value is the remaining time minus a 500 ms margin, truncated to whole seconds. Below 1 s the parameter is omitted, and the context deadline governs.

Whole seconds are used because Prometheus's duration parser rejects fractional seconds (`14.75s`). VictoriaMetrics caps the value at `-search.maxQueryDuration`.

- *Rejected: cap a build at half a store's slots (option a).* It adds a quota that idles slots when only one request is running. Cross-request fairness belongs to admission control (C3), not the store guard.
- *Rejected: a fixed operator-configured timeout.* It cannot track `--build-timeout` or an embedder's deadline.

### D6. Readiness probe is `count(up)`, coalesced with a 1 s TTL (P-1, options B + C)

Both consumers only test success and non-emptiness:

- `ProbeAll` and the `/readyz` fallback test success only.
- The retention probe tests `len(vec) > 0`.

`count(up)` preserves both, because `count` of an empty vector is an empty vector.

`Router.ProbeAll` coalesces concurrent calls with `singleflight`. The shared probe runs under `WithoutCancel` of the first caller, bounded by that caller's deadline, and every caller selects on its own context. A completed probe's outcome, success or `ProbeError`, is reused for 1 s, unless the shared probe's own deadline ended it: a timeout is not reused.

`Engine.Probe` and the retention probe are unchanged apart from the rendered query. The retention probe stays per build and live.

- *Rejected: route probes through the query-result cache.* That would contradict "probes bypass the cache", and the 60 s TTL is far too long for readiness.

### D7. Response size cap at the transport (#9)

The cap is a `RoundTripper` between basic-auth and the base transport.

- A response whose declared `Content-Length` exceeds the cap is refused before its body is read.
- Otherwise the body is wrapped. Once the cap is reached, the next byte read returns `ErrResponseTooLarge`, so a body is never silently truncated into a JSON error.

The default is 512 MiB, about ten times the largest leg seen at the 100k-series cache budget. Operators get `--upstream-max-response-bytes` / `KSG_UPSTREAM_MAX_RESPONSE_BYTES`, where `0` disables the cap. `promql.WithMaxResponseBytes(n)`, where `n <= 0` disables, reaches the client through `DefaultClientFactory(m, opts...)`.

- *Rejected: `io.LimitReader`.* It truncates silently.
- *Rejected: rely on the upstream's `-search.maxResponseSeries`.* That limit is not set by default and is not universal across compatible stores.

### D8. Basic auth requires the same scheme and host (#10)

`basicAuthTransport` records the scheme and the host of the configured URL, and attaches credentials only when both match. An `http`→`https` upgrade to the same host therefore also goes without credentials.

- *Rejected: allow upgrades.* That adds a rule for a case no supported store produces.
- *Rejected: disable redirects.* That breaks proxies that legitimately redirect.

### D9. `escapeLiteral` escapes control characters (#11, #13)

One package-level `strings.Replacer` escapes:

- `\` and `"`;
- `\n`, `\r` and `\t` by name;
- every other byte in 0x00–0x1F, and 0x7F, as `\xHH`.

All of these are escapes that both PromQL and MetricsQL string literals accept. A fast path returns the input unchanged when it holds none of them.

- *Rejected: `strconv.Quote`.* It also rewrites non-ASCII and invalid UTF-8 (`\u…`, `\x…`), which would change rendered queries for valid label values and move the render baseline.

### D10. Reloader digest from the parsed bytes (#7)

- `backendsfile.ReadWithContent(path, lookup)` returns the table and the bytes it was parsed from.
- `ReloaderOptions.Content` seeds the digest from those bytes, and the server passes them.
- An embedder that sets no `Content` leaves the digest zero, so the first tick re-parses and swaps once. Clients are reused by identity, so the swap is cheap.

- *Rejected: keep reading the file again in `NewReloader`.* A change between the startup read and that read would be taken as already live, and never applied.

### D11. Stale limiter series removed on swap (O3)

`observability.Metrics.SetBackends` already resets the per-backend failure counter. It now also deletes the in-flight and slot-wait series of any backend the new table no longer names, and records the live set.

`AddInflight` and `ObserveSlotWait` ignore names outside the live set. Without that, the decrement of a query still in flight on a removed backend would recreate its series at −1. A read-write mutex orders the check and the write against the swap. Before the first `SetBackends` call every name is accepted.

- *Rejected: `Reset()` the vectors.* It would zero the in-flight count of kept backends that have queries in flight, and their decrements would then drive the series negative.

### D12. Bounded statement in logs and spans (C1 item 4)

The `prometheus.query` span and the client's Debug and Error query logs carry:

- the statement, truncated to 2048 bytes on a UTF-8 rune boundary;
- the full byte length;
- the first 12 hex digits of its SHA-256.

The digest lets an operator correlate a truncated statement with a cache entry, or with an upstream log, without the full text.

- *Rejected: drop the statement entirely.* It is the most useful field when debugging a short query, which is the common case.

### D13. Merge fast path (C5)

When exactly one part is non-empty, `mergeVectors` returns it as-is. That covers every single-backend query and every zone-restricted fan-out. It is the guard's fresh slice header, so returning it shares nothing new.

It allocates only to drop a nil sample, and never filters in place, because cached vectors share their backing arrays. The empty result stays `model.Vector{}`.

With several parts, `FastFingerprint`, which does not allocate, proposes a duplicate and `Metric.Equal` confirms it. Colliding distinct series both survive.

- *Rejected: limit the fast path to `len(parts) == 1`.* An empty sibling part, such as an empty zone, is equally duplicate-free.
- *Rejected: `Fingerprint`.* It allocates and sorts per series, and equality confirmation makes the weaker hash safe.

Within one part, duplicate label sets are no longer collapsed. An instant vector from one store never carries duplicates.

## Risks / Trade-offs

- **The `timeout` parameter is new on the wire to upstream.** A store that rejects it would fail every query that carries a deadline. Prometheus and VictoriaMetrics both accept it. `internal/integration` exercises VictoriaMetrics.
- **A flight with no-deadline waiters is unbounded.** Every server path carries `--build-timeout`. An embedder calling with `context.Background()` gets the same unbounded behaviour it had before.
- **`/readyz` can be up to 1 s stale.** A kubelet probes every 10 s by default.
