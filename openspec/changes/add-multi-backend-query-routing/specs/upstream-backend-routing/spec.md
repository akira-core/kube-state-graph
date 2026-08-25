## Purpose

Dispatches every upstream PromQL call to one or more VictoriaMetrics installations
selected from a reloadable routing table, so an estate whose metrics are split by
availability zone and by metric family (NetApp Harvest in its own installation)
can be served as one graph by one process.

## ADDED Requirements

### Requirement: Declarative upstream backend table

The server SHALL accept a routing table declared in a file whose path is configured by `--backends-file` / `KSG_BACKENDS_FILE`. The file SHALL be accepted in either YAML or JSON form and SHALL declare a list of backends, each carrying:

- `name` — a non-empty identifier, unique across the table. It is the backend's identity in logs, metrics, and every ordering rule below.
- `url` — a Prometheus-compatible query endpoint, parseable as an absolute HTTP or HTTPS URL.
- `families` — a non-empty set of query families this backend serves, drawn from the fixed set defined by "Query family classification".
- `zones` — an optional set of `az` values whose series this backend holds. An omitted or empty set means **every zone** (a catch-all backend).
- `usernameEnv` / `passwordEnv` — optional names of environment variables holding this backend's basic-auth credentials, per "Per-backend credentials sourced from the environment".

The file SHALL NOT carry a credential value in any field. Validation SHALL reject a table that: is empty; declares a duplicate `name`; declares an unparseable or non-HTTP(S) `url`; declares an unknown family; declares an empty `families` set; or leaves any one of the defined query families served by **no** backend. A rejected table SHALL NOT be applied.

#### Scenario: Valid table accepted

- **WHEN** the server starts with a table declaring backend `zone-a` (`families: [ksm, kubelet, servicegraph, probe]`, `zones: [zone-a]`) and backend `netapp-a` (`families: [harvest]`, `zones: [zone-a]`)
- **THEN** startup succeeds and an Info log names both backends and their families

#### Scenario: JSON and YAML forms are equivalent

- **WHEN** two servers start, one with the table written as YAML and one with the byte-equivalent JSON
- **THEN** both resolve identical backends for every query

#### Scenario: Duplicate backend name rejected

- **WHEN** the table declares two backends both named `zone-a`
- **THEN** validation fails with an error naming `zone-a`, and the process exits non-zero before binding the listener

#### Scenario: Family left unserved rejected

- **WHEN** the table declares backends covering `ksm`, `kubelet`, `servicegraph` and `probe` but no backend declaring `harvest`
- **THEN** validation fails with an error naming the `harvest` family

#### Scenario: Credential value in the file rejected

- **WHEN** a backend entry carries a literal `password` or `username` field
- **THEN** validation fails with an error stating that credentials are sourced from the environment only, and the error does not echo the value

### Requirement: Query family classification

Every upstream query the server issues SHALL belong to exactly one of five fixed families, and the mapping SHALL be a hardcoded contract with no configuration surface:

- `ksm` — every `kube_*` kube-state-metrics series (pod, node, PVC, Service, EndpointSlice, owner, and controller-annotation families).
- `kubelet` — `kubelet_volume_stats_used_bytes` and `kubelet_volume_stats_capacity_bytes`.
- `harvest` — every NetApp Harvest series: `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`.
- `servicegraph` — the three `traces_service_graph_*` series.
- `probe` — the `up{}` probe.

A query with no declared family SHALL be a build-time failure of the repository's own test suite, not a runtime default: the classification table SHALL be exhaustive over the query set by construction.

#### Scenario: Every query is classified

- **WHEN** the repository's test suite runs
- **THEN** a test enumerates every declared query and fails if any one of them has no family entry

#### Scenario: Harvest separable from kube-state-metrics

- **WHEN** the table declares one backend serving `ksm`, `kubelet`, `servicegraph` and `probe` at one URL and another serving `harvest` at a different URL
- **THEN** every `kube_*` and `kubelet_*` query is sent only to the first URL and every Harvest query only to the second

### Requirement: Backend selection by requested availability zone

For a query of family `F` under a request whose `az` dimension carries the value set `A`, the set of backends the query is issued to SHALL be derived as follows:

1. Candidates are the backends whose `families` contains `F`.
2. If `F` is a family whose queries accept the `az` dimension (`ksm`, `kubelet`, `harvest`), candidates are further restricted to those whose `zones` set is empty (catch-all) **or** intersects `A`. When `A` is empty, no restriction is applied — every candidate is selected.
3. If `F` is a family whose queries accept **no** request dimension (`servicegraph`, `probe`), the `zones` field SHALL be ignored entirely and every candidate is selected regardless of `A`. Narrowing these families by zone would drop edges the loaded topology still needs.

Backend selection SHALL be composed **with**, never instead of, the request-scoped PromQL matchers: an `az` value that selects a backend is still rendered as a label matcher on every query that accepts it.

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
- **THEN** every Harvest query is issued only to `netapp-b`, carrying the `az="zone-b"` matcher

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

When no routing-table file is configured, the server SHALL behave as a table declaring exactly one backend: named `default`, addressed at `--prom-url`, serving **all five** families, with no `zones` (a catch-all). Every query SHALL then be issued to exactly one destination, and every rendered query string, merge result, and serialised response body SHALL be byte-identical to the same deployment before backend routing existed.

When both a routing-table file and `--prom-url` are configured, the file SHALL take precedence and a Warn SHALL be logged stating that `--prom-url` is ignored.

#### Scenario: No table configured behaves as today

- **WHEN** the server starts with `--prom-url=http://vm.example:8428` and no `--backends-file`
- **THEN** every upstream query is sent to `http://vm.example:8428` and the served response bodies match the pre-change golden files byte for byte

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
