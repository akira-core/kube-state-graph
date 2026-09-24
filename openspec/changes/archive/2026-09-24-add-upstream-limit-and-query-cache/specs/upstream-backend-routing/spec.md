## ADDED Requirements

### Requirement: Per-backend-store concurrency limit

The server SHALL bound the number of upstream queries in flight to each backend store (identified by URL and credential identity, so two routing-table backends naming the same store share one bound) to `--upstream-max-concurrency` / `KSG_UPSTREAM_MAX_CONCURRENCY` (default 32). The bound SHALL be process-wide — shared by every concurrent build, storage build, and label query — and SHALL survive a routing-table reload for every store whose identity the reload kept. A value of `0` SHALL disable the bound.

A query that finds its store saturated SHALL wait for a slot until its own context ends. A wait ended by the build deadline SHALL surface through the existing timeout mapping (`504`, `reason: "timeout"`); a wait ended by client cancellation SHALL surface through the existing cancellation handling. Waiting SHALL introduce no new error reason. Slots SHALL be granted to waiters of one store in arrival order.

The limit SHALL apply per backend: a fan-out query acquires one slot on each store it reaches, independently, and a saturated store SHALL NOT delay queries to a different store. A cache hit or a coalesced wait SHALL NOT hold a slot.

The readiness probe and the outside-retention `up{}` probe SHALL bypass the limit.

#### Scenario: Excess queries wait rather than overflow upstream

- **WHEN** the limit is 32 and three concurrent builds together issue 120 queries to one store
- **THEN** at no instant are more than 32 of those queries in flight to that store, and every query completes once slots free up

#### Scenario: Deadline expires while waiting

- **WHEN** a build's deadline expires while one of its queries is still waiting for a slot
- **THEN** the build fails with `504` and `reason: "timeout"`

#### Scenario: One saturated store does not stall another

- **WHEN** store `zone-a` is saturated and a build issues a query routed only to `zone-b`
- **THEN** the `zone-b` query is issued without waiting on `zone-a`

#### Scenario: Two backends naming one store share its bound

- **WHEN** backends `ksm` and `harvest` in the routing table name the same URL and credentials and the limit is 32
- **THEN** the queries to both together never exceed 32 in flight

#### Scenario: Readiness is not queued behind builds

- **WHEN** a store is saturated by builds
- **THEN** the readiness probe to that store is issued immediately

#### Scenario: Limit disabled

- **WHEN** the server starts with `--upstream-max-concurrency=0`
- **THEN** queries are issued with no in-process bound, as before this requirement

### Requirement: Concurrency limit observability

The server SHALL expose, per backend store, a gauge of queries currently in flight and a histogram of the time a query waited for a slot, labelled by the backend name the query was routed through. These SHALL be new metric names; the existing `kube_state_graph_upstream_query_*` metrics SHALL keep their label sets. The emitted client span for a query SHALL carry the slot wait duration as an attribute when the query waited.

#### Scenario: Waiting is visible

- **WHEN** queries wait for slots on backend `zone-a`
- **THEN** the slot-wait histogram labelled `zone-a` records non-zero observations

### Requirement: Embedded engine enables limit, cache and alignment by default

A Go module embedding the engine through its public constructors SHALL get the per-backend-store concurrency limit, the query-result cache (`upstream-query-cache` capability) and end-time alignment ("Time-window passthrough" in the `graph-api` capability) with the same default values as the server, without supplying any option:

- constructing a router over a routing table with no options SHALL apply the default concurrency limit and cache;
- constructing the convenience engine SHALL apply the default end-time alignment, and when it is handed a plain querier rather than a router it SHALL wrap that querier in the same limit and cache;
- the default values SHALL be exported, and the server's own defaults SHALL be those exported values.

Each feature SHALL be disableable independently through an explicit option. When the engine is handed a router, the router's own limit and cache configuration SHALL govern and the router's zone routing SHALL remain intact. The low-level build constructor SHALL remain unguarded and unaligned.

#### Scenario: Embedder with a single URL gets the defaults

- **WHEN** an external module builds a single-backend table, constructs a router with no options, and builds a graph through the convenience engine with zero-valued engine options
- **THEN** its queries are bounded at the default limit per store, repeated identical queries are served from the cache, and `end` is aligned to the default grid

#### Scenario: Embedder passes a plain client

- **WHEN** an external module constructs the convenience engine directly over a single-store client with zero-valued engine options
- **THEN** the default limit and cache apply to that client

#### Scenario: Embedder opts out

- **WHEN** an external module disables the limit, the cache and alignment through their explicit options
- **THEN** its builds issue every query upstream with no in-process bound and evaluate at the caller's verbatim `end`

#### Scenario: Server and library defaults agree

- **WHEN** the server starts with no concurrency, cache or alignment flags
- **THEN** its effective limit, cache budget, TTL and grid equal the exported library defaults
