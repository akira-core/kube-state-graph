# upstream-backend-routing Specification

## Purpose

Dispatches every upstream PromQL call to one or more VictoriaMetrics installations
selected from a reloadable routing table, so an estate whose metrics are split by
availability zone and by metric family (NetApp Harvest in its own installation)
can be served as one graph by one process.

## Requirements

### Requirement: Declarative upstream backend table

The server SHALL accept a routing table, written in YAML or JSON in a file whose path is configured by `--backends-file` / `KSG_BACKENDS_FILE`, declaring a list of backends that each carry `name`, `url`, `families`, an optional `zones`, and optional `usernameEnv` / `passwordEnv`. The file SHALL NOT carry a credential value in any field.

#### Scenario: Backend entry fields

- **WHEN** the server reads a backend entry
- **THEN** `name` is a non-empty identifier, unique across the table, and is the backend's identity in logs, metrics, and every ordering rule of this capability
- **AND** `url` is a Prometheus-compatible query endpoint, parseable as an absolute HTTP or HTTPS URL
- **AND** `families` is a non-empty set of query families this backend serves, drawn from the fixed set defined by "Query family classification"
- **AND** `zones` is an optional set of `az` values whose series this backend holds; an omitted or empty set means **every zone** (a catch-all backend)
- **AND** `usernameEnv` / `passwordEnv` optionally name environment variables holding this backend's basic-auth credentials, per "Per-backend credentials sourced from the environment"

#### Scenario: Valid table accepted

- **WHEN** the server starts with a table declaring backend `zone-a` (`families: [ksm, kubelet, servicegraph, probe]`, `zones: [zone-a]`) and backend `netapp-a` (`families: [harvest]`, `zones: [zone-a]`)
- **THEN** startup succeeds, an Info log names both backends and their families, and an Info states that no backend serves `alerts` or `appmeta`

#### Scenario: JSON and YAML forms are equivalent

- **WHEN** two servers start, one with the table written as YAML and one with the byte-equivalent JSON
- **THEN** both resolve identical backends for every query

#### Scenario: Credential value in the file rejected

- **WHEN** a backend entry carries a literal `password` or `username` field
- **THEN** validation fails with an error stating that credentials are sourced from the environment only, and the error does not echo the value

### Requirement: Routing table validation

Validation SHALL reject a table that is empty, declares a duplicate `name`, declares an unparseable or non-HTTP(S) `url`, declares an unknown family, declares an empty `families` set, or leaves any one of the **required** query families (`ksm`, `kubelet`, `harvest`, `servicegraph`, `probe`) served by **no** backend. A rejected table SHALL NOT be applied.

#### Scenario: Malformed table rejected

- **WHEN** the table is empty, or a backend declares an unparseable or non-HTTP(S) `url`, an unknown family, or an empty `families` set
- **THEN** validation fails and the table is not applied

#### Scenario: Duplicate backend name rejected

- **WHEN** the table declares two backends both named `zone-a`
- **THEN** validation fails with an error naming `zone-a`, and the process exits non-zero before binding the listener

#### Scenario: Family left unserved rejected

- **WHEN** the table declares backends covering `ksm`, `kubelet`, `servicegraph` and `probe` but no backend declaring `harvest`
- **THEN** validation fails with an error naming the `harvest` family

### Requirement: Optional alerts and application-metadata families

The `alerts` and `appmeta` families SHALL be **optional**: a table serving either on no backend SHALL be accepted. With `alerts` unserved the server SHALL issue no `ALERTS` query and log one Info stating that the alert overlay is disabled; with `appmeta` unserved it SHALL issue no application-metadata query (see the `app-metadata-source` capability).

#### Scenario: Optional family left unserved accepted

- **WHEN** the table declares backends covering all five required families and none declaring `alerts` or `appmeta`
- **THEN** validation succeeds

#### Scenario: Unserved optional families issue no query

- **WHEN** no backend serves `alerts` and no backend serves `appmeta`
- **THEN** no `ALERTS` query is issued and one Info log states that the alert overlay is disabled
- **AND** no application-metadata query is issued

#### Scenario: Alerts served by a dedicated backend

- **WHEN** the table additionally declares backend `vmalert-a` (`families: [alerts]`, `zones: [zone-a]`)
- **THEN** startup succeeds and every `ALERTS` query for `az=zone-a` is issued only to `vmalert-a`

#### Scenario: Application metadata served by a dedicated backend

- **WHEN** the table additionally declares backend `meta-a` (`families: [appmeta]`, `zones: [zone-a]`) and the application-metadata metric is configured
- **THEN** startup succeeds and every application-metadata query for `az=zone-a` is issued only to `meta-a`

### Requirement: Query family classification

Every upstream query the server issues SHALL belong to exactly one of seven fixed families through a hardcoded mapping with no configuration surface. The classification table SHALL be exhaustive over the query set by construction: a query with no declared family SHALL be a build-time failure of the repository's own test suite, not a runtime default.

#### Scenario: Family membership

- **WHEN** a server-issued query is classified
- **THEN** `ksm` holds every `kube_*` kube-state-metrics series (pod, node, PVC, Service, EndpointSlice, owner, and controller-annotation families)
- **AND** `kubelet` holds `kubelet_volume_stats_used_bytes` and `kubelet_volume_stats_capacity_bytes`
- **AND** `harvest` holds every NetApp Harvest series: `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, `node_total_data`
- **AND** `servicegraph` holds the three `traces_service_graph_*` series, and `probe` the `up{}` probe
- **AND** `alerts` holds the `ALERTS` series (the `alert-overlay` capability); it is zone-routable (`az` selects its backends, and `az` / `env` / `namespace` are rendered as matchers on it) and a valid table may leave it unserved
- **AND** `appmeta` holds the operator-named application-metadata metric (the `app-metadata-source` capability), classified under its logical query name `app_metadata` whatever metric name is configured; it is zone-routable, with `az` / `env` / `cluster` / `namespace` rendered as matchers, and a valid table may leave it unserved

#### Scenario: Every query is classified

- **WHEN** the repository's test suite runs
- **THEN** a test enumerates every declared query and fails if any one of them has no family entry

#### Scenario: Harvest separable from kube-state-metrics

- **WHEN** the table declares one backend serving `ksm`, `kubelet`, `servicegraph` and `probe` at one URL and another serving `harvest` at a different URL
- **THEN** every `kube_*` and `kubelet_*` query is sent only to the first URL and every Harvest query only to the second

#### Scenario: Alerts separable from kube-state-metrics

- **WHEN** the table declares one backend serving `ksm` at one URL and another serving `alerts` at a different URL
- **THEN** the `ALERTS` query is sent only to the second URL and never to the first

#### Scenario: Application metadata separable from kube-state-metrics

- **WHEN** the table declares one backend serving `ksm` at one URL and another serving `appmeta` at a different URL, and the metric is configured as `argocd_app_pod_info`
- **THEN** the query reading `argocd_app_pod_info` is sent only to the second URL and never to the first
- **AND** configuring the application-metadata metric's NAME does not change its family, which stays a hardcoded contract

### Requirement: Caller-declared family for the label query

A **caller-declared** family SHALL be the only exception to deriving the family from the query name, confined to the embedder-facing label query of the `metrics-label-query` capability. It SHALL be validated against the same seven names and dispatched through the **unchanged** rules of this capability; it SHALL NOT introduce a second dispatch policy, widen the family set, or make the family of a server-issued query configurable.

#### Scenario: Caller-declared family dispatches through the same rules

- **WHEN** a consumer issues a label query naming an arbitrary metric and declaring family `harvest` with zone `zone-b`, against a table whose Harvest backends are split by zone
- **THEN** the query reaches exactly the Harvest backends the zone rule selects for a server-issued Harvest query under `az=zone-b`, and a backend error fails the call naming that backend
- **AND** the same backend selection by zone, the same identical query string per backend, the same de-duplicating merge, and the same fail-closed behaviour on a backend error apply
- **AND** the caller declares the family because the label query names an arbitrary metric, so no classification table can decide which store holds it

#### Scenario: Server-issued queries keep their derived family

- **WHEN** the graph build issues its own queries
- **THEN** each one's family is still read from the hardcoded classification table, and no caller value can override it

### Requirement: Backend selection by requested availability zone

For a query of family `F` under a request whose `az` dimension carries the value set `A`, the query SHALL be issued to the backends whose `families` contains `F`, restricted, when `F` is a **zone-routed** family (`ksm`, `kubelet`, `harvest`, `alerts`, `appmeta`) and `A` is non-empty, to those whose `zones` set is empty (catch-all) **or** intersects `A`. For a family that is not zone-routed (`servicegraph`, `probe`), the `zones` field SHALL be ignored entirely.

#### Scenario: Zone selects a single backend

- **WHEN** backends `zone-a` (`zones: [zone-a]`) and `zone-b` (`zones: [zone-b]`) both serve `ksm`, and a request carries `az=zone-a`
- **THEN** every `ksm` query is issued only to `zone-a`, and the issued query string additionally carries the `az="zone-a"` matcher

#### Scenario: Absent zone fans out to every backend

- **WHEN** the same table serves a request carrying no `az` parameter
- **THEN** every `ksm` query is issued to both `zone-a` and `zone-b`, as an empty `A` applies no restriction

#### Scenario: Multiple zones select the covering subset

- **WHEN** backends `zone-a`, `zone-b` and `zone-c` each declare their own zone, and a request carries `az=zone-a&az=zone-c`
- **THEN** every `ksm` query is issued to `zone-a` and `zone-c` only, carrying the matcher `az=~"zone-a|zone-c"`

#### Scenario: Catch-all backend always selected

- **WHEN** a backend declares no `zones` and a request carries `az=zone-a`
- **THEN** that backend is selected alongside any backend declaring `zone-a`

#### Scenario: Service-graph and probe ignore zones

- **WHEN** a request carries `az=zone-a` and two backends declaring `zones: [zone-a]` and `zones: [zone-b]` both serve `servicegraph`
- **THEN** the three `traces_service_graph_*` queries are issued to BOTH backends, exactly as they would be for a request carrying no `az`
- **AND** every candidate serving `servicegraph` or `probe` is selected regardless of `A`, because narrowing these families by zone would drop edges the loaded topology still needs

### Requirement: Zone routing composes with request matchers

Backend selection SHALL be composed **with**, never instead of, the request-scoped PromQL matchers: an `az` value that selects a backend SHALL still be rendered as a label matcher on every query that accepts it. The `harvest` family is zone-routed but SHALL accept NO `az` matcher, so for it backend selection is the only effect of the `az` dimension.

#### Scenario: Harvest routed by zone like kube-state-metrics

- **WHEN** backends `netapp-a` (`families: [harvest]`, `zones: [zone-a]`) and `netapp-b` (`families: [harvest]`, `zones: [zone-b]`) are declared and a request carries `az=zone-b`
- **THEN** every Harvest query is issued only to `netapp-b`, as the bare unfiltered query string — no `az` matcher is rendered (see the `netapp-storage-graph` capability)

#### Scenario: Zone-routability is declared per family

- **WHEN** the server decides whether a family is zone-routed
- **THEN** it reads a property of the family declared alongside the matcher table and pinned by the same exhaustiveness test
- **AND** it never infers zone-routability from whether the family renders an `az` matcher

### Requirement: Unmatched zone yields an empty result

When zone restriction leaves no candidate backend — a requested zone that no backend declares — the query SHALL return an empty result rather than an error, and the build SHALL log a Warn naming the family and the unmatched zone values. An empty result under an active selector SHALL be treated as a legitimate empty graph, not a retention miss.

#### Scenario: Unmatched zone yields an empty result, not an error

- **WHEN** a request carries `az=zone-z` and no backend declares `zone-z` or is a catch-all
- **THEN** the `ksm` queries return no rows, the response is 200 with an empty element list, and a Warn log names the family and `zone-z`
- **AND** the empty graph is not classified as an outside-retention miss

### Requirement: Deterministic fan-out merge

When a query is issued to more than one backend, the results SHALL be merged into a single result set by concatenating each backend's returned series in ascending backend-`name` order. The merged result SHALL be a pure function of the value sets returned by the selected backends, so two requests differing only in parameter order or in backend response arrival order produce byte-identical response bodies.

#### Scenario: Disjoint backends concatenate

- **WHEN** backend `zone-a` returns two series and backend `zone-b` returns three, with no label set in common
- **THEN** the merged result carries all five series, `zone-a`'s first

#### Scenario: Response order independent of backend latency

- **WHEN** the same request is served twice and the two backends respond in opposite orders
- **THEN** both responses are byte-identical

### Requirement: Fan-out merge de-duplicates identical series

A series whose label set is byte-identical to one already contributed by an earlier backend in merge order SHALL be **dropped**, so a series present in two backends contributes exactly once and the surviving copy is the one from the lexically-smallest backend name. A dropped copy carrying a different sample value SHALL be logged at Debug and counted, and SHALL NOT fail the build.

#### Scenario: Duplicate series contributes once

- **WHEN** backends `zone-a` and `zone-b` both return a series with an identical label set
- **THEN** the merged result carries exactly one copy of it, the one returned by `zone-a`

#### Scenario: Duplicate service-graph series does not double a rate

- **WHEN** the same `traces_service_graph_request_total` series is returned by two backends serving the `servicegraph` family
- **THEN** the resulting edge's `data.metrics.rate` is the single-backend value, not twice it
- **AND** de-duplication is required for correctness, since values summed across contributing series — notably service-graph request rates and error numerators — would otherwise be multiplied by the number of backends holding the series

#### Scenario: Colliding sample values

- **WHEN** the dropped copy of a duplicate series carries a different sample value from the kept one
- **THEN** the collision is logged at Debug and counted, and the build does not fail

### Requirement: Backend failure fails the query it was issued for

When a query is issued to several backends and any one of them returns an error, the query SHALL fail with an error naming the failing backend; a partial result SHALL NOT be silently returned in its place. Legs the builder already treats as optional (those that log-and-continue on query error) SHALL keep that behaviour: a backend failure on such a leg degrades that leg only, exactly as any upstream error does.

#### Scenario: Required leg fails when one backend errors

- **WHEN** a request fans a `kube_pod_info` query out to two backends and one refuses the connection
- **THEN** the build fails and the returned error names the failing backend

#### Scenario: Optional leg degrades when one backend errors

- **WHEN** a request fans a `kubelet_volume_stats_used_bytes` query out to two backends and one returns an error
- **THEN** the build completes without kubelet usage for that leg and logs the failure naming the backend

#### Scenario: Failing backend never appears as a partial graph

- **WHEN** the backend holding `zone-b` is unreachable and a request carries no `az`
- **THEN** the request fails rather than returning a graph containing only `zone-a`
- **AND** this matters because a missing zone removes pods and the connectivity prune then removes the nodes, claims, and edges that hung off them, so a degraded fan-out would render as a smaller but plausible graph with no signal

### Requirement: Hot reload of the routing table

When a routing-table file is configured and `--backends-reload-interval` is positive, the server SHALL re-read the file on that interval and, when its parsed content differs from the live table, validate it and **atomically** replace the live table. A file that fails to read, parse, or validate SHALL be rejected **wholesale**, and a partially applied table SHALL never be observable.

#### Scenario: New backend picked up without restart

- **WHEN** the mounted file is updated to add a backend for `zone-c` and the reload interval elapses
- **THEN** the next request carrying `az=zone-c` is dispatched to it, with no process restart

#### Scenario: Invalid file leaves the live table serving

- **WHEN** the mounted file is replaced with unparseable content and the reload interval elapses
- **THEN** requests continue to be dispatched by the previous table, an Error log names the parse failure, and the reload-failure counter increments

#### Scenario: Any rejected reload keeps the live table

- **WHEN** a reloaded file fails to read, parse, or validate
- **THEN** the previous table keeps serving unchanged, an Error log names the reason, and the reload-failure counter increments

#### Scenario: Reload disabled

- **WHEN** the server starts with a reload interval of zero and the file is subsequently changed
- **THEN** the live table is unchanged and no reload is attempted
- **AND** the table read at startup serves for the process lifetime

#### Scenario: Reload covers every backend field

- **WHEN** an accepted reload changes backend membership, URLs, families, zones, or credential-variable names
- **THEN** each change takes effect in the new live table

#### Scenario: Retired backend's connections released

- **WHEN** a reload removes a backend from the table
- **THEN** the idle connections held for that backend's URL are closed

### Requirement: In-flight builds observe one routing table

A build in flight SHALL observe one consistent routing table for its whole duration; a reload SHALL NOT change which backends a single build's queries are dispatched to part-way through.

#### Scenario: Reload does not disturb an in-flight build

- **WHEN** a reload swaps the table while a `/v1/graph` build is running
- **THEN** every query of that build is dispatched by the table that was live when the build started

### Requirement: Per-backend credentials sourced from the environment

A backend MAY name the environment variables holding its HTTP Basic Auth pair via `usernameEnv` and `passwordEnv`. The **values** SHALL be read from the process environment and SHALL NOT appear in the routing file, in any CLI flag, log line, trace span attribute, metric label, error message, or HTTP response body. Credentials SHALL be attached only to requests addressed to that backend's own host.

#### Scenario: Per-backend credentials applied

- **WHEN** backend `zone-a` declares `usernameEnv: KSG_PROM_USERNAME_A` / `passwordEnv: KSG_PROM_PASSWORD_A` and both variables are set
- **THEN** every request to `zone-a`'s URL carries `Authorization: Basic` for that pair, and requests to other backends do not

#### Scenario: Cross-host redirect carries no credentials

- **WHEN** a request to a backend with credentials is redirected to a different host
- **THEN** the redirected request carries no `Authorization` header

#### Scenario: Credential rotation

- **WHEN** an operator rotates a credential **value** in the environment
- **THEN** the new value takes effect only after a process restart, while the variable **names** are reloadable

#### Scenario: Credentials never logged

- **WHEN** the server runs at `debug` level with per-backend credentials configured and a backend query fails
- **THEN** no log line, span attribute, or error string contains either credential value, while the backend name and variable names may appear

### Requirement: Credential variable declarations are validated

Validation SHALL reject a backend that names exactly one of `usernameEnv` / `passwordEnv`, and SHALL reject a backend naming a variable that is unset or empty in the process environment, because a silently unauthenticated upstream call is a worse outcome than a failed load.

#### Scenario: Half-declared pair rejected

- **WHEN** a backend declares `usernameEnv` but no `passwordEnv`
- **THEN** validation fails with an error naming the backend and both fields, without echoing any value

#### Scenario: Named variable unset rejected

- **WHEN** a backend declares `usernameEnv: KSG_PROM_USERNAME_A` and that variable is unset in the process environment
- **THEN** validation fails with an error naming the backend and the variable
- **AND** a variable that is set but empty is rejected the same way

### Requirement: Global credential fallback

A backend naming neither credential variable SHALL fall back to the global `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` pair when that pair is configured, and SHALL otherwise issue unauthenticated requests.

#### Scenario: Global pair used as fallback

- **WHEN** a backend names neither variable and `KSG_PROM_USERNAME` / `KSG_PROM_PASSWORD` are set
- **THEN** requests to that backend carry the global credentials

#### Scenario: No credentials anywhere

- **WHEN** a backend names neither variable and the global pair is not configured
- **THEN** requests to that backend are unauthenticated

### Requirement: Single-backend compatibility mode

When no routing-table file is configured, the server SHALL behave as a table declaring exactly one backend: named `default`, addressed at `--prom-url`, serving **all seven** families (the five required plus `alerts` and `appmeta`), with no `zones` (a catch-all). When both a routing-table file and `--prom-url` are configured, the file SHALL take precedence and a Warn SHALL be logged stating that `--prom-url` is ignored.

#### Scenario: Implicit table output matches the pre-routing deployment

- **WHEN** no routing-table file is configured
- **THEN** every query is issued to exactly one destination, and every rendered query string, merge result, and serialised response body is byte-identical to the same deployment before backend routing existed, apart from the `data.status` keys the graph-api "Node `status` attribute" requirement adds on every build
- **AND** the added `ALERTS` leg contributes nothing to the body when the store holds no `ALERTS` series, and no application-metadata query is issued unless its metric is configured

#### Scenario: No table configured behaves as today

- **WHEN** the server starts with `--prom-url=http://vm.example:8428` and no `--backends-file`
- **THEN** every upstream query — including `ALERTS` — is sent to `http://vm.example:8428` and the served response bodies match the pre-change golden files byte for byte when that store holds no `ALERTS` series

#### Scenario: Table overrides prom-url

- **WHEN** the server starts with both `--prom-url` and `--backends-file` set
- **THEN** the file's backends serve every query, and a Warn log states that `--prom-url` is ignored

### Requirement: Multi-backend readiness and retention probes

The readiness probe SHALL probe **every** backend declared in the live table and SHALL report ready only when all of them answer successfully. Its failure body SHALL name the backends that did not answer.

#### Scenario: One unreachable backend makes the server not ready

- **WHEN** two backends are declared and one refuses connections
- **THEN** the readiness probe returns not-ready and its body names the refusing backend

### Requirement: Retention probe requires every probe backend

The outside-retention classification's `up{}` probe SHALL regard the upstream as healthy only when every backend serving the `probe` family answers. When any of them fails to answer, the classification SHALL be skipped, so an empty graph is reported as an empty graph, never as a retention miss.

#### Scenario: Retention classification skipped when a backend is down

- **WHEN** a build loads no topology and one backend serving the `probe` family is unreachable
- **THEN** the response is an empty graph rather than an outside-retention error

### Requirement: Routing observability

The server SHALL expose self-metrics describing the live routing table and per-backend query outcomes — at minimum a gauge of the number of backends in the live table, a counter of routing-table reload attempts labelled by result, and a counter of upstream query failures labelled by backend. The client span for every upstream query SHALL carry the backend name as an attribute.

#### Scenario: Backend gauge reflects the live table

- **WHEN** a reload changes the table from two backends to three
- **THEN** the backend-count gauge reads 3 after the reload

#### Scenario: Reload result counted

- **WHEN** one reload succeeds and a later one is rejected as invalid
- **THEN** the reload counter carries one increment for the success result and one for the failure result

#### Scenario: Span names the backend

- **WHEN** a query is issued to backend `zone-b` with tracing enabled
- **THEN** the emitted client span carries an attribute identifying `zone-b`

### Requirement: Existing upstream query metrics keep their labels

The existing `kube_state_graph_upstream_query_duration_seconds` and `kube_state_graph_upstream_query_failures_total` metrics SHALL keep their current label sets, because a new label on an existing self-metric is a contract change; per-backend detail SHALL be carried by the new routing metrics instead.

#### Scenario: Existing query metrics keep their labels

- **WHEN** a client scrapes `/metrics` with several backends configured
- **THEN** `kube_state_graph_upstream_query_duration_seconds` and `kube_state_graph_upstream_query_failures_total` carry exactly the labels they carried before backend routing existed

### Requirement: Embeddable routing configuration surface

The routing file's schema, its parse, its credential resolution, and its hot-reload loop SHALL live in an importable package outside `internal/`, so a Go module embedding the graph engine configures routing with the same code the server runs. `internal/` MAY keep wrappers over that package, but SHALL NOT be the only place a routing table can be produced from a file or the reload behaviour is implemented.

#### Scenario: An embedder parses the operator's routing file

- **WHEN** an external module calls the exported reader with the path of a routing file
- **THEN** it receives the identical validated table the server would build from that file, subject to every validation and credential rule of this capability
- **AND** a file the server would reject is rejected with the same error

#### Scenario: An embedder builds the implicit table without a file

- **WHEN** an external module has a single upstream endpoint and no routing file
- **THEN** an exported helper produces the single-backend compatibility table — one catch-all backend named `default` serving all seven families — without touching the filesystem

#### Scenario: An embedder hot-reloads without re-implementing the loop

- **WHEN** an external module arms the exported reload loop against a path and a positive interval
- **THEN** the file is re-read on that interval, an unchanged file is not re-parsed, a file that fails to read/parse/validate leaves the previous table serving, and an accepted file is swapped atomically — the behaviour this capability already requires of the server

#### Scenario: The embedder inherits no telemetry it did not ask for

- **WHEN** an external module arms the reload loop supplying neither a logger nor a metrics recorder
- **THEN** the loop reloads as specified while emitting no log lines and recording no self-metrics

#### Scenario: An embedder wires routing into the graph engine

- **WHEN** an external module constructs the engine facade from a router over a two-backend table
- **THEN** the build dispatches through the routing table, and the request's `az` values select backends exactly as they do in the server

### Requirement: Query layer stays free of file I/O

The package holding the routing table and the router SHALL remain free of file I/O and of any configuration-file parser, so a module that builds its table in code inherits neither.

#### Scenario: Importing the query layer alone pulls in no parser

- **WHEN** a module imports the package holding the routing table and router but not the configuration package
- **THEN** its build graph gains no configuration-file parser and no file I/O from this capability

### Requirement: Per-backend-store concurrency limit

The server SHALL bound the number of upstream queries in flight to each backend store to `--upstream-max-concurrency` / `KSG_UPSTREAM_MAX_CONCURRENCY` (default 32), a store being identified by URL and credential identity. The bound SHALL be process-wide and SHALL survive a routing-table reload for every store whose identity the reload kept. A value of `0` SHALL disable the bound.

#### Scenario: Excess queries wait rather than overflow upstream

- **WHEN** the limit is 32 and three concurrent builds together issue 120 queries to one store
- **THEN** at no instant are more than 32 of those queries in flight to that store, and every query completes once slots free up

#### Scenario: Bound shared by every consumer

- **WHEN** concurrent builds, storage builds, and label queries reach the same store
- **THEN** they all draw from that store's one process-wide bound

#### Scenario: Bound survives a reload

- **WHEN** a routing-table reload keeps a store's URL and credential identity
- **THEN** that store's bound survives the reload

#### Scenario: Two backends naming one store share its bound

- **WHEN** backends `ksm` and `harvest` in the routing table name the same URL and credentials and the limit is 32
- **THEN** the queries to both together never exceed 32 in flight

#### Scenario: Limit disabled

- **WHEN** the server starts with `--upstream-max-concurrency=0`
- **THEN** queries are issued with no in-process bound, as before this requirement

### Requirement: Waiting for a concurrency slot

A query that finds its store saturated SHALL wait for a slot until its own context ends, and slots SHALL be granted to the waiters of one store in arrival order. Waiting SHALL introduce no new error reason: a wait ended by the build deadline or by client cancellation SHALL surface through the existing timeout mapping or cancellation handling.

#### Scenario: Deadline expires while waiting

- **WHEN** a build's deadline expires while one of its queries is still waiting for a slot
- **THEN** the build fails with `504` and `reason: "timeout"`, through the existing timeout mapping

#### Scenario: Client cancels while waiting

- **WHEN** the client cancels a request while one of its queries is waiting for a slot
- **THEN** the wait ends and the request surfaces through the existing cancellation handling, with no new error reason

#### Scenario: Slots granted in arrival order

- **WHEN** several queries wait for a slot on the same store
- **THEN** slots are granted to them in the order they began waiting

### Requirement: Concurrency slots are per store and only for upstream calls

The limit SHALL apply per backend: a fan-out query SHALL acquire one slot on each store it reaches, independently, and a saturated store SHALL NOT delay queries to a different store. A cache hit or a coalesced wait SHALL NOT hold a slot, and the readiness probe and the outside-retention `up{}` probe SHALL bypass the limit.

#### Scenario: One saturated store does not stall another

- **WHEN** store `zone-a` is saturated and a build issues a query routed only to `zone-b`
- **THEN** the `zone-b` query is issued without waiting on `zone-a`

#### Scenario: Fan-out acquires one slot per store

- **WHEN** a query fans out to two stores
- **THEN** it acquires one slot on each store, independently of the other

#### Scenario: Cache hits hold no slot

- **WHEN** a query is answered from the query-result cache or by waiting on a coalesced identical miss
- **THEN** it holds no concurrency slot

#### Scenario: Readiness is not queued behind builds

- **WHEN** a store is saturated by builds
- **THEN** the readiness probe to that store is issued immediately
- **AND** the outside-retention `up{}` probe likewise bypasses the limit

### Requirement: Concurrency limit observability

The server SHALL expose, per backend store, a gauge of queries currently in flight and a histogram of the time a query waited for a slot, labelled by the backend name the query was routed through. These SHALL be new metric names; the existing `kube_state_graph_upstream_query_*` metrics SHALL keep their label sets. The emitted client span for a query SHALL carry the slot wait duration as an attribute when the query waited.

#### Scenario: Waiting is visible

- **WHEN** queries wait for slots on backend `zone-a`
- **THEN** the slot-wait histogram labelled `zone-a` records non-zero observations

### Requirement: Embedded engine enables limit, cache and alignment by default

A Go module embedding the engine through its public constructors SHALL get the per-backend-store concurrency limit, the query-result cache (`upstream-query-cache` capability) and end-time alignment ("Time-window passthrough" in the `graph-api` capability) with the server's default values, without supplying any option. Each feature SHALL be disableable independently through an explicit option, and the low-level build constructor SHALL remain unguarded and unaligned.

#### Scenario: Defaults per public constructor

- **WHEN** an embedder uses the public constructors without options
- **THEN** constructing a router over a routing table with no options applies the default concurrency limit and cache
- **AND** constructing the convenience engine applies the default end-time alignment, and when it is handed a plain querier rather than a router it wraps that querier in the same limit and cache
- **AND** the default values are exported, and the server's own defaults are those exported values

#### Scenario: Engine handed a router

- **WHEN** the convenience engine is constructed over a router
- **THEN** the router's own limit and cache configuration govern, and the router's zone routing remains intact

#### Scenario: Low-level build constructor stays unguarded

- **WHEN** an embedder builds through the low-level build constructor
- **THEN** its queries pass through no concurrency limit or cache and `end` is not aligned

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
