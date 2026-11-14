## MODIFIED Requirements

### Requirement: Query-result cache key

The server SHALL cache the result of a successful upstream instant query under a key composed of the backend store the query was issued to (its URL and credential identity), the SHA-256 digest of the exact rendered query string, and the exact evaluation instant. Two queries SHALL share an entry only when all three components are equal, and the query NAME SHALL NOT be part of the key. The key SHALL NOT retain the rendered query string itself, so a resident entry's key is fixed-size however long its query was. Concurrent misses SHALL be coalesced under the same key.

#### Scenario: Identical query at the same instant hits

- **WHEN** two builds issue the same rendered query to the same backend at the same evaluation instant within the TTL
- **THEN** the second build receives the cached result and no second upstream request is sent

#### Scenario: Different evaluation instant misses

- **WHEN** two builds issue the same rendered query to the same backend at evaluation instants one second apart
- **THEN** each is sent upstream and cached separately

#### Scenario: Query name is not part of the key

- **WHEN** two queries with different query NAMES (used for self-metrics and spans) render the same query string to the same backend at the same instant
- **THEN** they share one cache entry

#### Scenario: Different queries never share an entry

- **WHEN** two different rendered queries are issued to the same backend at the same instant
- **THEN** each is cached under its own digest and neither is served the other's result

#### Scenario: Long query keeps a fixed-size key

- **WHEN** a 16 KiB rendered query's result is cached
- **THEN** the resident key holds its 32-byte digest, not the 16 KiB string

#### Scenario: No credential value is exposed

- **WHEN** the cache key is composed for a backend that carries credentials
- **THEN** the key carries the credential identity, never a credential value
- **AND** no log line or metric describing the cache exposes a credential value

### Requirement: Cache bounds and eviction

The cache SHALL be bounded by a total budget (`--query-cache-max-series` / `KSG_QUERY_CACHE_MAX_SERIES`, default 100000) in which an entry costs the larger of one and its series count, plus one unit per whole KiB of its rendered query. Before an insertion evicts any live entry, the cache SHALL release expired entries from its least-recently-used end, and SHALL then evict least-recently-used entries until the insertion fits. A single result costing more than the whole budget SHALL NOT be cached and SHALL still be returned to its caller. A budget of `0` SHALL disable the cache entirely, restoring byte-for-byte the uncached behaviour. The resident-series gauge SHALL report resident series, not budget units.

#### Scenario: LRU eviction under budget pressure

- **WHEN** the budget is 1000 units, entries A (600 series) and B (300 series) with short queries are resident, A was read more recently than B, and a new 300-series result C is inserted
- **THEN** B is evicted, A and C remain resident

#### Scenario: Long query charged for its length

- **WHEN** the budget is 100 units and empty results of distinct 16 KiB queries are inserted
- **THEN** each costs 17 units, at most five stay resident, and the resident-series gauge reads 0

#### Scenario: Oversized result is not cached

- **WHEN** a single query returns more series than the whole budget
- **THEN** the caller receives the full result and nothing is inserted or evicted

#### Scenario: Cache disabled

- **WHEN** the server starts with `--query-cache-max-series=0`
- **THEN** every query is sent upstream and no cache metric other than the zero-valued series changes

### Requirement: Cache entry expiry

Every entry SHALL expire after `--query-cache-ttl` / `KSG_QUERY_CACHE_TTL` (default 60s) from insertion, and an expired entry SHALL NOT be served. An expired entry SHALL be released when it is next looked up, or when an insertion finds it at the least-recently-used end, whichever is first. Its release SHALL NOT be counted as an eviction.

#### Scenario: Expired entry is refetched

- **WHEN** an entry is older than the TTL
- **THEN** the next identical query is sent upstream and the fresh result replaces the entry

#### Scenario: A burst of one-off queries does not outlive its TTL

- **WHEN** a burst of distinct queries is cached, none of them is read again, the TTL passes, and one new result is inserted
- **THEN** the burst's entries are released by that insertion and the eviction counter does not move

### Requirement: Concurrent misses are coalesced

When several callers miss on the same key at the same time, the server SHALL send exactly one upstream query for that key and deliver its outcome to every waiting caller. The shared upstream query SHALL NOT be bound to the cancellation of any single caller. It SHALL run until it completes, until every waiting caller has stopped waiting, or until the latest deadline among its waiters passes, whichever is first. A waiter with no deadline SHALL leave the shared query with no deadline. A waiting caller whose own context ends SHALL stop waiting and return its own context error, and SHALL NOT end the shared query for the other waiters.

#### Scenario: Two simultaneous builds share one upstream query

- **WHEN** two builds issue the same rendered query to the same backend at the same instant while neither result is cached
- **THEN** exactly one upstream request is sent and both builds receive its result

#### Scenario: A waiter times out independently

- **WHEN** a second caller waits on an in-flight shared query and its own build deadline expires first
- **THEN** the second caller returns a deadline error and the first caller still receives the upstream result

#### Scenario: The starting caller's cancellation does not fail the others

- **WHEN** the caller that started a shared query is cancelled while a second caller with a live context is waiting on it
- **THEN** the shared query keeps running, the second caller receives its result, and no second upstream request is sent

#### Scenario: Abandoned shared query is cancelled

- **WHEN** every caller waiting on a shared query has stopped waiting
- **THEN** the shared upstream query is cancelled and a later identical miss starts a new one

#### Scenario: The latest waiter deadline bounds the shared query

- **WHEN** a shared query was started by a caller with a 1 s deadline and a second caller with a 10 s deadline joins it
- **THEN** the shared query is bounded by the 10 s deadline, not the 1 s one

### Requirement: Probes bypass the cache

The readiness probe and the outside-retention `up{}` probe SHALL always reach upstream and SHALL neither read nor populate the query-result cache. Concurrent readiness probes SHALL be coalesced into one probe of each backend, and a completed readiness probe's outcome MAY be reused for at most 1 second, except an outcome cut short by the shared probe's own deadline, which SHALL NOT be reused. The outside-retention probe SHALL NOT reuse any earlier outcome.

#### Scenario: Readiness reflects live upstream state

- **WHEN** a backend becomes unreachable after a successful probe was issued
- **THEN** a readiness probe issued more than 1 second later fails, not served from a cached success

#### Scenario: Concurrent readiness probes share one upstream probe

- **WHEN** many readiness probes arrive at the same time
- **THEN** each backend receives one probe query and every caller receives its outcome
