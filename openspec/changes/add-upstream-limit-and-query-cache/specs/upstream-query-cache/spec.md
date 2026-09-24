# Spec Delta

## Purpose

Bounds repeated upstream load by caching successful upstream PromQL query results in process, so identical queries issued by concurrent or closely repeated graph builds are answered once per backend store.

## ADDED Requirements

### Requirement: Query-result cache key

The server SHALL cache the result of a successful upstream instant query under a key composed of the backend store the query was issued to (its URL and credential identity), the exact rendered query string, and the exact evaluation instant. Two queries SHALL share an entry only when all three components are equal. The query NAME (used for self-metrics and spans) SHALL NOT be part of the key. The key SHALL never contain, and a log line or metric describing the cache SHALL never expose, a credential value.

The cache SHALL sit below routing: a query fanned out to several backends is cached per backend, and the fan-out merge SHALL run over cached and fresh per-backend results identically, so the merged vector is the same whether each part came from the cache or the network.

#### Scenario: Identical query at the same instant hits

- **WHEN** two builds issue the same rendered query to the same backend at the same evaluation instant within the TTL
- **THEN** the second build receives the cached result and no second upstream request is sent

#### Scenario: Different evaluation instant misses

- **WHEN** two builds issue the same rendered query to the same backend at evaluation instants one second apart
- **THEN** each is sent upstream and cached separately

#### Scenario: Same query to two backends is cached per backend

- **WHEN** a query fans out to backends `zone-a` and `zone-b`
- **THEN** each backend's result is cached under its own key and a later identical query that routes only to `zone-a` reuses only `zone-a`'s entry

### Requirement: Cache bounds and eviction

The cache SHALL be bounded by a total resident-series budget (`--query-cache-max-series` / `KSG_QUERY_CACHE_MAX_SERIES`, default 100000) and SHALL evict least-recently-used entries until an insertion fits. A single result larger than the whole budget SHALL NOT be cached and SHALL still be returned to its caller. Every entry SHALL expire after `--query-cache-ttl` / `KSG_QUERY_CACHE_TTL` (default 60s) from insertion; an expired entry SHALL NOT be served. A budget of `0` SHALL disable the cache entirely, restoring byte-for-byte the uncached behaviour.

#### Scenario: LRU eviction under budget pressure

- **WHEN** the budget is 1000 series, entries A (600 series) and B (300 series) are resident, A was read more recently than B, and a new 300-series result C is inserted
- **THEN** B is evicted, A and C remain resident

#### Scenario: Oversized result is not cached

- **WHEN** a single query returns more series than the whole budget
- **THEN** the caller receives the full result and nothing is inserted or evicted

#### Scenario: Expired entry is refetched

- **WHEN** an entry is older than the TTL
- **THEN** the next identical query is sent upstream and the fresh result replaces the entry

#### Scenario: Cache disabled

- **WHEN** the server starts with `--query-cache-max-series=0`
- **THEN** every query is sent upstream and no cache metric other than the zero-valued series changes

### Requirement: Only successful results are cached

A query that failed — transport error, upstream error status, unexpected result type, or cancelled context — SHALL NOT be cached, and a later identical query SHALL be sent upstream. An empty vector is a successful result and SHALL be cached.

#### Scenario: Upstream 503 is not cached

- **WHEN** a backend answers a query with `503` and the same query is issued again
- **THEN** the second query is sent upstream rather than served a cached error

#### Scenario: Empty result is cached

- **WHEN** a query returns an empty vector
- **THEN** an identical query within the TTL is served from the cache

### Requirement: Concurrent misses are coalesced

When several callers miss on the same key at the same time, the server SHALL send exactly one upstream query for that key and deliver its outcome to every waiting caller. A waiting caller whose own context ends SHALL stop waiting and return its context error without cancelling the shared upstream query for the other waiters; the shared query SHALL be bounded by the context of the caller that started it.

#### Scenario: Two simultaneous builds share one upstream query

- **WHEN** two builds issue the same rendered query to the same backend at the same instant while neither result is cached
- **THEN** exactly one upstream request is sent and both builds receive its result

#### Scenario: A waiter times out independently

- **WHEN** a second caller waits on an in-flight shared query and its own build deadline expires first
- **THEN** the second caller returns a deadline error and the first caller still receives the upstream result

### Requirement: Cached results are immutable to readers

A result served from the cache SHALL be observationally identical to a freshly fetched one, and no reader SHALL be able to alter what a later reader of the same entry observes. Response bodies built from cached results SHALL be byte-identical to those built from fresh results for the same upstream data.

#### Scenario: Golden body unchanged with cache on

- **WHEN** the same `/v1/graph` request is issued twice with the cache enabled and upstream data unchanged
- **THEN** both response bodies are byte-identical and equal to the body produced with the cache disabled

### Requirement: Probes bypass the cache

The readiness probe and the outside-retention `up{}` probe SHALL always reach upstream and SHALL neither read nor populate the cache.

#### Scenario: Readiness reflects live upstream state

- **WHEN** a backend becomes unreachable after a successful probe was issued
- **THEN** the next readiness probe fails, not served from a cached success

### Requirement: Cache observability

The server SHALL expose self-metrics for the cache: counters of hits, misses, coalesced waits and evictions, and a gauge of resident series. These SHALL be new metric names; no existing self-metric SHALL gain a label. A hit SHALL NOT record an upstream query-duration or query-failure observation, since no upstream query was issued.

#### Scenario: Hit counted without an upstream observation

- **WHEN** a query is served from the cache
- **THEN** the hit counter increments and `kube_state_graph_upstream_query_duration_seconds` records no new observation for it
