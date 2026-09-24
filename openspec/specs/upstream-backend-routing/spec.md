# upstream-backend-routing Specification

## Purpose

Dispatches every upstream PromQL call to one or more VictoriaMetrics installations
selected from a reloadable routing table, so an estate whose metrics are split by
availability zone and by metric family (NetApp Harvest in its own installation)
can be served as one graph by one process.

## Requirements

### Requirement: Declarative upstream backend table

The server SHALL accept a routing table declared in a file whose path is configured by `--backends-file` / `KSG_BACKENDS_FILE`. The file SHALL be accepted in either YAML or JSON form and SHALL declare a list of backends, each carrying:

- `name` — a non-empty identifier, unique across the table. It is the backend's identity in logs, metrics, and every ordering rule below.
- `url` — a Prometheus-compatible query endpoint, parseable as an absolute HTTP or HTTPS URL.
- `families` — a non-empty set of query families this backend serves, drawn from the fixed set defined by "Query family classification".
- `zones` — an optional set of `az` values whose series this backend holds. An omitted or empty set means **every zone** (a catch-all backend).
- `usernameEnv` / `passwordEnv` — optional names of environment variables holding this backend's basic-auth credentials, per "Per-backend credentials sourced from the environment".

The file SHALL NOT carry a credential value in any field. Validation SHALL reject a table that: is empty; declares a duplicate `name`; declares an unparseable or non-HTTP(S) `url`; declares an unknown family; declares an empty `families` set; or leaves any one of the **required** query families (`ksm`, `kubelet`, `harvest`, `servicegraph`, `probe`) served by **no** backend. The `alerts` family is **optional**: a table serving it on no backend SHALL be accepted, and the server SHALL then issue no `ALERTS` query and log one Info stating that the alert overlay is disabled. A rejected table SHALL NOT be applied.

#### Scenario: Valid table accepted

- **WHEN** the server starts with a table declaring backend `zone-a` (`families: [ksm, kubelet, servicegraph, probe]`, `zones: [zone-a]`) and backend `netapp-a` (`families: [harvest]`, `zones: [zone-a]`)
- **THEN** startup succeeds, an Info log names both backends and their families, and a second Info states that no backend serves `alerts`

#### Scenario: Alerts served by a dedicated backend

- **WHEN** the table additionally declares backend `vmalert-a` (`families: [alerts]`, `zones: [zone-a]`)
- **THEN** startup succeeds and every `ALERTS` query for `az=zone-a` is issued only to `vmalert-a`

#### Scenario: JSON and YAML forms are equivalent

- **WHEN** two servers start, one with the table written as YAML and one with the byte-equivalent JSON
- **THEN** both resolve identical backends for every query

#### Scenario: Duplicate backend name rejected

- **WHEN** the table declares two backends both named `zone-a`
- **THEN** validation fails with an error naming `zone-a`, and the process exits non-zero before binding the listener

#### Scenario: Family left unserved rejected

- **WHEN** the table declares backends covering `ksm`, `kubelet`, `servicegraph` and `probe` but no backend declaring `harvest`
- **THEN** validation fails with an error naming the `harvest` family

#### Scenario: Optional family left unserved accepted

- **WHEN** the table declares backends covering all five required families and none declaring `alerts`
- **THEN** validation succeeds

#### Scenario: Credential value in the file rejected

- **WHEN** a backend entry carries a literal `password` or `username` field
- **THEN** validation fails with an error stating that credentials are sourced from the environment only, and the error does not echo the value

### Requirement: Query family classification

Every upstream query the server issues SHALL belong to exactly one of six fixed families, and the mapping SHALL be a hardcoded contract with no configuration surface:

- `ksm` — every `kube_*` kube-state-metrics series (pod, node, PVC, Service, EndpointSlice, owner, and controller-annotation families).
- `kubelet` — `kubelet_volume_stats_used_bytes` and `kubelet_volume_stats_capacity_bytes`.
- `harvest` — every NetApp Harvest series: `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, `node_total_data`.
- `servicegraph` — the three `traces_service_graph_*` series.
- `probe` — the `up{}` probe.
- `alerts` — the `ALERTS` series (the `alert-overlay` capability). This family is zone-routable (`az` selects its backends, and `az` / `env` / `namespace` are rendered as matchers on it); it is the only family a valid table may leave unserved.

A query with no declared family SHALL be a build-time failure of the repository's own test suite, not a runtime default: the classification table SHALL be exhaustive over the query set by construction.

A **caller-declared** family is the one exception to derivation from the query name, and it is confined to the embedder-facing label query of the `metrics-label-query` capability: because that query names an arbitrary metric, no classification table can decide which store holds it, so the caller declares the family per call. A declared family SHALL be validated against the same six names, and SHALL then be dispatched through the **unchanged** rules of this capability — the same backend selection by zone, the same identical query string per backend, the same de-duplicating merge, and the same fail-closed behaviour on a backend error. A caller-declared family SHALL NOT introduce a second dispatch policy, SHALL NOT widen the family set, and SHALL NOT make the family of a server-issued query configurable.

#### Scenario: Every query is classified

- **WHEN** the repository's test suite runs
- **THEN** a test enumerates every declared query and fails if any one of them has no family entry

#### Scenario: Harvest separable from kube-state-metrics

- **WHEN** the table declares one backend serving `ksm`, `kubelet`, `servicegraph` and `probe` at one URL and another serving `harvest` at a different URL
- **THEN** every `kube_*` and `kubelet_*` query is sent only to the first URL and every Harvest query only to the second

#### Scenario: Alerts separable from kube-state-metrics

- **WHEN** the table declares one backend serving `ksm` at one URL and another serving `alerts` at a different URL
- **THEN** the `ALERTS` query is sent only to the second URL and never to the first

#### Scenario: Caller-declared family dispatches through the same rules

- **WHEN** a consumer issues a label query naming an arbitrary metric and declaring family `harvest` with zone `zone-b`, against a table whose Harvest backends are split by zone
- **THEN** the query reaches exactly the Harvest backends the zone rule selects for a server-issued Harvest query under `az=zone-b`, and a backend error fails the call naming that backend

#### Scenario: Server-issued queries keep their derived family

- **WHEN** the graph build issues its own queries
- **THEN** each one's family is still read from the hardcoded classification table, and no caller value can override it

### Requirement: Backend selection by requested availability zone

For a query of family `F` under a request whose `az` dimension carries the value set `A`, the set of backends the query is issued to SHALL be derived as follows:

1. Candidates are the backends whose `families` contains `F`.
2. If `F` is a **zone-routed** family (`ksm`, `kubelet`, `harvest`), candidates are further restricted to those whose `zones` set is empty (catch-all) **or** intersects `A`. When `A` is empty, no restriction is applied — every candidate is selected.
3. If `F` is not zone-routed (`servicegraph`, `probe`), the `zones` field SHALL be ignored entirely and every candidate is selected regardless of `A`. Narrowing these families by zone would drop edges the loaded topology still needs.

Backend selection SHALL be composed **with**, never instead of, the request-scoped PromQL matchers: an `az` value that selects a backend is still rendered as a label matcher on every query that accepts it. The `harvest` family is zone-routed but accepts NO `az` matcher: for it, backend selection is the only effect the `az` dimension has, and the query string issued to the selected backends is the unfiltered one (see the `netapp-storage-graph` capability). Zone-routability is therefore a property of the family, declared alongside the matcher table and pinned by the same exhaustiveness test, not inferred from whether the family renders an `az` matcher.

When step 2 yields an empty candidate set — a requested zone that no backend declares — the query SHALL return an empty result rather than an error, and the build SHALL log a Warn naming the family and the unmatched zone values. An empty result under an active selector is a legitimate empty graph, not a retention miss.

#### Scenario: Zone selects a single backend

- **WHEN** backends `zone-a` (`zones: [zone-a]`) and `zone-b` (`zones: [zone-b]`) both serve `ksm`, and a request carries `az=zone-a`
- **THEN** every `ksm` query is issued only to `zone-a`, and the issued query string additionally carries the `az="zone-a"` matcher

#### Scenario: Absent zone fans out to every backend

- **WHEN** the same table serves a request carrying no `az` parameter
- **THEN** every `ksm` query is issued to both `zone-a` and `zone-b`

#### Scenario: Multiple zones select the covering subset

- **WHEN** backends `zone-a`, `zone-b` and `zone-c` each declare their own zone, and a request carries `az=zone-a&az=zone-c`
- **THEN** every `ksm` query is issued to `zone-a` and `zone-c` only, carrying the matcher `az=~"zone-a|zone-c"`

#### Scenario: Catch-all backend always selected

- **WHEN** a backend declares no `zones` and a request carries `az=zone-a`
- **THEN** that backend is selected alongside any backend declaring `zone-a`

#### Scenario: Service-graph and probe ignore zones

- **WHEN** a request carries `az=zone-a` and two backends declaring `zones: [zone-a]` and `zones: [zone-b]` both serve `servicegraph`
- **THEN** the three `traces_service_graph_*` queries are issued to BOTH backends, exactly as they would be for a request carrying no `az`

#### Scenario: Harvest routed by zone like kube-state-metrics

- **WHEN** backends `netapp-a` (`families: [harvest]`, `zones: [zone-a]`) and `netapp-b` (`families: [harvest]`, `zones: [zone-b]`) are declared and a request carries `az=zone-b`
- **THEN** every Harvest query is issued only to `netapp-b`, as the bare unfiltered query string — no `az` matcher is rendered

#### Scenario: Unmatched zone yields an empty result, not an error

- **WHEN** a request carries `az=zone-z` and no backend declares `zone-z` or is a catch-all
- **THEN** the `ksm` queries return no rows, the response is 200 with an empty element list, and a Warn log names the family and `zone-z`

### Requirement: Deterministic fan-out merge

When a query is issued to more than one backend, the results SHALL be merged into a single result set by concatenating each backend's returned series in ascending backend-`name` order.

A series whose label set is byte-identical to one already contributed by an earlier backend in that order SHALL be **dropped**, so a series present in two backends contributes exactly once. The surviving copy is the one from the lexically-smallest backend name. When the dropped copy carries a different sample value from the kept one, the server SHALL log the collision at Debug and count it; it SHALL NOT fail the build.

De-duplication is required for correctness, not merely tidiness: values that are summed across contributing series — notably service-graph request rates and error numerators — would otherwise be multiplied by the number of backends holding the series.

The merged result SHALL be a pure function of the value sets returned by the selected backends, so two requests differing only in parameter order or in backend response arrival order produce byte-identical response bodies.

#### Scenario: Disjoint backends concatenate

- **WHEN** backend `zone-a` returns two series and backend `zone-b` returns three, with no label set in common
- **THEN** the merged result carries all five series, `zone-a`'s first

#### Scenario: Duplicate series contributes once

- **WHEN** backends `zone-a` and `zone-b` both return a series with an identical label set
- **THEN** the merged result carries exactly one copy of it, the one returned by `zone-a`

#### Scenario: Duplicate service-graph series does not double a rate

- **WHEN** the same `traces_service_graph_request_total` series is returned by two backends serving the `servicegraph` family
- **THEN** the resulting edge's `data.metrics.rate` is the single-backend value, not twice it

#### Scenario: Response order independent of backend latency

- **WHEN** the same request is served twice and the two backends respond in opposite orders
- **THEN** both responses are byte-identical

### Requirement: Backend failure fails the query it was issued for

When a query is issued to several backends and any one of them returns an error, the query SHALL fail with an error naming the failing backend. A partial result SHALL NOT be silently returned in its place: a missing zone removes pods, and the connectivity prune then removes the nodes, claims, and edges that hung off them, so a degraded fan-out would render as a smaller but plausible graph with no signal.

Legs the builder already treats as optional (those that log-and-continue on query error) SHALL keep that behaviour — a backend failure on such a leg degrades that leg only, exactly as an upstream error does today.

#### Scenario: Required leg fails when one backend errors

- **WHEN** a request fans a `kube_pod_info` query out to two backends and one refuses the connection
- **THEN** the build fails and the returned error names the failing backend

#### Scenario: Optional leg degrades when one backend errors

- **WHEN** a request fans a `kubelet_volume_stats_used_bytes` query out to two backends and one returns an error
- **THEN** the build completes without kubelet usage for that leg and logs the failure naming the backend

#### Scenario: Failing backend never appears as a partial graph

- **WHEN** the backend holding `zone-b` is unreachable and a request carries no `az`
- **THEN** the request fails rather than returning a graph containing only `zone-a`

### Requirement: Hot reload of the routing table

When a routing-table file is configured and `--backends-reload-interval` is positive, the server SHALL re-read the file on that interval and, when its parsed content differs from the live table, validate it and **atomically** replace the live table. A reload interval of zero SHALL disable reloading; the table read at startup then serves for the process lifetime.

A file that fails to read, parse, or validate SHALL be rejected **wholesale**: the previously live table SHALL keep serving unchanged, the failure SHALL be logged at Error naming the reason, and a reload-failure counter SHALL be incremented. A partially applied table SHALL never be observable.

A build in flight SHALL observe one consistent table for its whole duration — a reload SHALL NOT change which backends a single build's queries are dispatched to part-way through.

The reload SHALL apply to backend membership, URLs, families, zones, and credential-variable names alike. Connections held for a backend that the new table no longer declares SHALL be released.

#### Scenario: New backend picked up without restart

- **WHEN** the mounted file is updated to add a backend for `zone-c` and the reload interval elapses
- **THEN** the next request carrying `az=zone-c` is dispatched to it, with no process restart

#### Scenario: Invalid file leaves the live table serving

- **WHEN** the mounted file is replaced with unparseable content and the reload interval elapses
- **THEN** requests continue to be dispatched by the previous table, an Error log names the parse failure, and the reload-failure counter increments

#### Scenario: Reload does not disturb an in-flight build

- **WHEN** a reload swaps the table while a `/v1/graph` build is running
- **THEN** every query of that build is dispatched by the table that was live when the build started

#### Scenario: Reload disabled

- **WHEN** the server starts with a reload interval of zero and the file is subsequently changed
- **THEN** the live table is unchanged and no reload is attempted

#### Scenario: Retired backend's connections released

- **WHEN** a reload removes a backend from the table
- **THEN** the idle connections held for that backend's URL are closed

### Requirement: Per-backend credentials sourced from the environment

A backend MAY name the environment variables holding its HTTP Basic Auth pair via `usernameEnv` and `passwordEnv`. The **values** SHALL be read from the process environment; they SHALL NOT appear in the routing file, in any CLI flag, in any log line, trace span attribute, metric label, error message, or HTTP response body.

Validation SHALL reject a backend that names exactly one of the two variables, and SHALL reject a backend naming a variable that is unset or empty in the process environment — a silently unauthenticated upstream call is a worse outcome than a failed load.

A backend naming neither variable SHALL fall back to the global `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` pair when that pair is configured, and SHALL otherwise issue unauthenticated requests. Credentials SHALL be attached only to requests addressed to that backend's own host, so a cross-host redirect carries no `Authorization` header.

Rotating a credential **value** requires a process restart; only the variable **names** are reloadable.

#### Scenario: Per-backend credentials applied

- **WHEN** backend `zone-a` declares `usernameEnv: KSG_PROM_USERNAME_A` / `passwordEnv: KSG_PROM_PASSWORD_A` and both variables are set
- **THEN** every request to `zone-a`'s URL carries `Authorization: Basic` for that pair, and requests to other backends do not

#### Scenario: Global pair used as fallback

- **WHEN** a backend names neither variable and `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` are set
- **THEN** requests to that backend carry the global credentials

#### Scenario: Half-declared pair rejected

- **WHEN** a backend declares `usernameEnv` but no `passwordEnv`
- **THEN** validation fails with an error naming the backend and both fields, without echoing any value

#### Scenario: Named variable unset rejected

- **WHEN** a backend declares `usernameEnv: KSG_PROM_USERNAME_A` and that variable is unset in the process environment
- **THEN** validation fails with an error naming the backend and the variable

#### Scenario: Credentials never logged

- **WHEN** the server runs at `debug` level with per-backend credentials configured and a backend query fails
- **THEN** no log line, span attribute, or error string contains either credential value, while the backend name and variable names may appear

### Requirement: Single-backend compatibility mode

When no routing-table file is configured, the server SHALL behave as a table declaring exactly one backend: named `default`, addressed at `--prom-url`, serving **all six** families (the five required plus `alerts`), with no `zones` (a catch-all). Every query SHALL then be issued to exactly one destination, and every rendered query string, merge result, and serialised response body SHALL be byte-identical to the same deployment before backend routing existed, apart from the `data.status` keys the graph-api "Node `status` attribute" requirement adds on every build — the added `ALERTS` leg contributes nothing to the body when the store holds no `ALERTS` series.

When both a routing-table file and `--prom-url` are configured, the file SHALL take precedence and a Warn SHALL be logged stating that `--prom-url` is ignored.

#### Scenario: No table configured behaves as today

- **WHEN** the server starts with `--prom-url=http://vm.example:8428` and no `--backends-file`
- **THEN** every upstream query — including `ALERTS` — is sent to `http://vm.example:8428` and the served response bodies match the pre-change golden files byte for byte when that store holds no `ALERTS` series

#### Scenario: Table overrides prom-url

- **WHEN** the server starts with both `--prom-url` and `--backends-file` set
- **THEN** the file's backends serve every query, and a Warn log states that `--prom-url` is ignored

### Requirement: Multi-backend readiness and retention probes

The readiness probe SHALL probe **every** backend declared in the live table and SHALL report ready only when all of them answer successfully. Its failure body SHALL name the backends that did not answer.

The outside-retention classification's `up{}` probe SHALL likewise regard the upstream as healthy only when every backend serving the `probe` family answers. When any of them fails to answer, the classification SHALL be skipped — an empty graph is then reported as an empty graph, never as a retention miss.

#### Scenario: One unreachable backend makes the server not ready

- **WHEN** two backends are declared and one refuses connections
- **THEN** the readiness probe returns not-ready and its body names the refusing backend

#### Scenario: Retention classification skipped when a backend is down

- **WHEN** a build loads no topology and one backend serving the `probe` family is unreachable
- **THEN** the response is an empty graph rather than an outside-retention error

### Requirement: Routing observability

The server SHALL expose self-metrics describing the live routing table and per-backend query outcomes, at minimum: a gauge of the number of backends in the live table, a counter of routing-table reload attempts labelled by result, and a counter of upstream query failures labelled by backend.

The existing `kube_state_graph_upstream_query_duration_seconds` and `kube_state_graph_upstream_query_failures_total` metrics SHALL keep their current label sets — a new label on an existing self-metric is a contract change — so per-backend detail is carried by the new metrics instead.

Every upstream query SHALL be traceable to the backend it was issued to: the client span for a query SHALL carry the backend name as an attribute.

#### Scenario: Backend gauge reflects the live table

- **WHEN** a reload changes the table from two backends to three
- **THEN** the backend-count gauge reads 3 after the reload

#### Scenario: Reload result counted

- **WHEN** one reload succeeds and a later one is rejected as invalid
- **THEN** the reload counter carries one increment for the success result and one for the failure result

#### Scenario: Existing query metrics keep their labels

- **WHEN** a client scrapes `/metrics` with several backends configured
- **THEN** `kube_state_graph_upstream_query_duration_seconds` and `kube_state_graph_upstream_query_failures_total` carry exactly the labels they carried before backend routing existed

#### Scenario: Span names the backend

- **WHEN** a query is issued to backend `zone-b` with tracing enabled
- **THEN** the emitted client span carries an attribute identifying `zone-b`

### Requirement: Embeddable routing configuration surface

The routing file's schema, its parse, its credential resolution, and its hot-reload loop SHALL live in an importable package outside `internal/`, so a Go module embedding the graph engine configures routing with the same code the server runs. `internal/` MAY keep wrappers over that package, but SHALL NOT be the only place a routing table can be produced from a file, and SHALL NOT be the only place the reload behaviour is implemented.

The package holding the routing table and the router SHALL itself remain free of file I/O and of any configuration-file parser, so a module that builds its table in code inherits neither.

#### Scenario: An embedder parses the operator's routing file

- **WHEN** an external module calls the exported reader with the path of a routing file
- **THEN** it receives the identical validated table the server would build from that file, subject to every validation and credential rule of this capability
- **AND** a file the server would reject is rejected with the same error

#### Scenario: An embedder builds the implicit table without a file

- **WHEN** an external module has a single upstream endpoint and no routing file
- **THEN** an exported helper produces the single-backend compatibility table — one catch-all backend named `default` serving all five families — without touching the filesystem

#### Scenario: An embedder hot-reloads without re-implementing the loop

- **WHEN** an external module arms the exported reload loop against a path and a positive interval
- **THEN** the file is re-read on that interval, an unchanged file is not re-parsed, a file that fails to read/parse/validate leaves the previous table serving, and an accepted file is swapped atomically — the behaviour this capability already requires of the server

#### Scenario: The embedder inherits no telemetry it did not ask for

- **WHEN** an external module arms the reload loop supplying neither a logger nor a metrics recorder
- **THEN** the loop reloads as specified while emitting no log lines and recording no self-metrics

#### Scenario: An embedder wires routing into the graph engine

- **WHEN** an external module constructs the engine facade from a router over a two-backend table
- **THEN** the build dispatches through the routing table, and the request's `az` values select backends exactly as they do in the server

#### Scenario: Importing the query layer alone pulls in no parser

- **WHEN** a module imports the package holding the routing table and router but not the configuration package
- **THEN** its build graph gains no configuration-file parser and no file I/O from this capability

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
