## ADDED Requirements

### Requirement: RED edge metrics on UID-resolved pod-to-pod edges

The reader SHALL attach a typed, nullable numeric metrics object to an emitted edge **iff all three** of the following hold:

1. the edge `type` is `pod-calls-pod`; AND
2. the edge was produced from at least one `traces_service_graph_request_total` series (it is **trace-derived**, not synthesised from topology or from a route-resolution outcome); AND
3. **both** endpoints are **UID-resolved** — the contributing series carried a non-empty `client_k8s_pod_uid` AND a non-empty `server_k8s_pod_uid`, and each resolved to a `type="pod"` node (a real topology pod or a synthesised pod).

An endpoint that is **peer-resolved** — identified only through the "Unknown-server peer-label enrichment" ladder, including the Pod-IP branch that resolves straight to a topology pod — SHALL NOT satisfy condition 3, even though it yields a `type="pod"` node and a `pod-calls-pod` edge. The peer-address ladder runs only because the producer could not pair a server span; that endpoint's own side emitted no trace, so the RED series contain no measurement for it.

The object SHALL carry the following fields:

- `rate` (number, REQUIRED whenever the object is present) — requests per second over the request window, as `rate(traces_service_graph_request_total[<window>])` summed over the edge's **in-scope contributing series** (defined below). It is strictly greater than zero by construction, because zero-rate series never produce an edge.
- `error_rate` (number, OPTIONAL) — the failed fraction in the closed interval `[0, 1]`, computed as the summed `rate(traces_service_graph_request_failed_total[<window>])` over the SAME in-scope contributing series divided by `rate`. The value SHALL be clamped to `[0, 1]`. A value of `0` SHALL mean "the failure counter was read successfully and reported no failures", never "the failure counter could not be read".
- `p90_server_ms` (number, OPTIONAL) — the 90th percentile of the **server-observed** request duration for this edge, expressed in **milliseconds**.

The quantile is `0.90` and the observation side is **server**, matching the definition used by Grafana's documented service-graph queries so the two tools measure the same thing. The values are NOT expected to equal Grafana's numerically, because Grafana aggregates by service name while this API aggregates by pod pair.

The reader SHALL NOT attach the metrics object to any other edge. In particular, an edge SHALL carry no metrics object when it is:

- a `pod-calls-service` edge (target is a materialised service node, not a pod — whether from connection-string resolution, from route resolution, or from the synthesised ingress-chain hop);
- a `service-selects-pod` edge (synthesised fan-out; no series names the individual backing pod);
- a `pod-calls-pod` edge with an `external` endpoint on either side;
- a `pod-calls-pod` edge whose endpoint was peer-resolved rather than UID-resolved;
- a topology-derived edge (`pod-mounts-pvc`, `pod-to-node`, `pvc-to-storageclass`).

Numeric values SHALL NOT appear anywhere in an edge's `labels` map, which remains strictly `map[string]string`. Attaching the metrics object SHALL NOT change an edge's `id`, `type`, `source`, `target`, or `labels`.

#### Scenario: Both endpoints are UID-resolved topology pods

- **WHEN** a `traces_service_graph_request_total` series with a non-zero rate has a non-empty `client_k8s_pod_uid` and a non-empty `server_k8s_pod_uid` that both resolve to topology pods
- **THEN** the emitted `pod-calls-pod` edge carries a metrics object whose `rate` equals the series' rate value

#### Scenario: Server UID is non-empty but unknown to topology (synthesised pod)

- **WHEN** a series' `server_k8s_pod_uid` is non-empty but absent from the topology pod-UID index, so the target materialises as a synthesised pod node
- **THEN** the emitted `pod-calls-pod` edge still carries a metrics object — the endpoint is UID-resolved and the target is a `type="pod"` node

#### Scenario: Peer-resolved Pod-IP target carries no metrics

- **WHEN** a series has `server="unknown"` and an empty `server_k8s_pod_uid`, and the peer-label enrichment resolves its peer address to a family Pod IP, producing a `pod-calls-pod` edge whose target is a real topology pod
- **THEN** that edge carries NO metrics object — the target is peer-resolved, not UID-resolved

#### Scenario: Endpoint fell back to an external node

- **WHEN** a series' server side resolved to an `external` node (missing UID with a non-`"://"` label, an unresolvable connection string, or an unresolved peer address)
- **THEN** the emitted `pod-calls-pod` edge carries NO metrics object

#### Scenario: Connection-string endpoint resolved to a service

- **WHEN** a series' server side resolved to a `type="service"` node and a `pod-calls-service` edge is emitted
- **THEN** that edge carries NO metrics object

#### Scenario: Synthesised service-selects-pod fan-out edge

- **WHEN** a service node fans out `service-selects-pod` edges to its backing pods
- **THEN** none of those edges carries a metrics object

#### Scenario: Synthesised route-hit ingress-chain edge

- **WHEN** the route engine produces the ingress chain and a synthesised gateway-pod → backend-service `pod-calls-service` edge is emitted
- **THEN** that edge carries NO metrics object

#### Scenario: Topology-derived edges never carry metrics

- **WHEN** the response contains a `pod-mounts-pvc`, `pod-to-node`, or `pvc-to-storageclass` edge
- **THEN** that edge carries NO metrics object

#### Scenario: Numeric values never enter edge labels

- **WHEN** any edge is emitted, with or without a metrics object
- **THEN** its `labels` map contains no `rate`, `error_rate`, or `p90_server_ms` key, and every `labels` value is a string

### Requirement: RED source series and selector consistency

In addition to `traces_service_graph_request_total`, the reader SHALL read two further service-graph series for the same window and the same evaluation instant:

- `traces_service_graph_request_failed_total` — the Errors counter, read at the same label granularity as the total counter so it can be joined to the exact same series.
- `traces_service_graph_request_server_seconds_bucket` — the Duration classic histogram, pre-aggregated upstream by the pod-pair identity `(cluster, client_k8s_pod_uid, server_k8s_pod_uid)` plus the `le` bucket boundary, so per-bucket cardinality is collapsed before it crosses the wire.

The reader SHALL apply the SAME virtual-sentinel exclusion fragment to both new selectors as it applies to `traces_service_graph_request_total` (see "Virtual sentinel endpoint exclusion (user / unknown)"), so that the three series always describe the same edge population.

Both new selectors SHALL additionally require a non-empty `client_k8s_pod_uid` and a non-empty `server_k8s_pod_uid`. This is a fixed, request-invariant metric-selection contract — the same class as the sentinel exclusion and the node-condition selector, NOT a caller-supplied filter — and it makes the queried series population **exactly equal** to the attachment rule above, so no qualifying edge can be silently uncovered and no uncovered edge can be silently reported as measured. It SHALL NOT vary per request.

The configurable upstream metric-name prefix SHALL NOT be applied to either new series (they belong to the trace-pipeline exporter family, not to kube-state-metrics). The reader SHALL NOT read `traces_service_graph_request_client_seconds_*`, nor the `_sum` / `_count` companions of the server histogram.

#### Scenario: Sentinel peers are excluded from the RED series too

- **WHEN** the reader issues the failure-counter and duration-histogram queries
- **THEN** both selectors carry the identical sentinel-exclusion matcher fragment used by the request-total selector, so a sentinel peer contributes to none of the three

#### Scenario: Series without both pod UIDs never reach the RED join

- **WHEN** a service-graph series has an empty `client_k8s_pod_uid` or an empty `server_k8s_pod_uid`
- **THEN** it is excluded from the failure-counter and duration-histogram results at the query layer, matching the attachment rule that gives such an edge no metrics object

#### Scenario: Metric prefix is not applied to the RED series

- **WHEN** an operator configures a non-empty upstream metric-name prefix
- **THEN** the failure-counter and duration-histogram queries still address the unprefixed `traces_service_graph_request_failed_total` and `traces_service_graph_request_server_seconds_bucket` names

### Requirement: RED join and deterministic aggregation

Several upstream series legitimately resolve to a single edge (most commonly when a dimension the edge identity does not carry, such as `connection_type`, differs, or when two series carry different `cluster` labels that resolve to the same client pod). The **in-scope contributing series** of an edge are those contributing series that carry a non-empty `client_k8s_pod_uid` and a non-empty `server_k8s_pod_uid`. The reader SHALL aggregate over that set and no other:

- `rate` SHALL be the SUM of the in-scope contributing series' rates. A contributing series outside the RED selector's scope SHALL NOT be counted, so that the Rate denominator, the Errors numerator, and the Duration buckets are all derived from one identical series set.
- The failure counter SHALL be joined to an in-scope contributing series by that series' **exact label identity** (all labels except the metric name).
- The duration histogram SHALL be joined to an edge by the `(cluster, client_k8s_pod_uid, server_k8s_pod_uid)` identity recorded for that edge while its contributing series were resolved; where several such identities map to one edge, their bucket sets SHALL be summed per `le` boundary.
- `p90_server_ms` SHALL be computed from the **summed** bucket set, using the classic cumulative-bucket convention with linear interpolation inside the bucket that contains the 90th percentile (the same algorithm as a PromQL `histogram_quantile(0.9, ...)`), then converted from seconds to milliseconds. The reader SHALL NOT compute per-series quantiles and then average or otherwise combine them.

Every attached value SHALL be a pure function of the upstream data and SHALL NOT depend on the arrival order of series within a query result. To that end the reader SHALL make its summation order-independent and SHALL round each attached value before serialisation, so that two builds over identical upstream data produce byte-identical response bodies (see the `graph-api` "Deterministic response body" requirement).

Rounding SHALL be to a fixed number of **significant digits**, applied identically to all three fields, and SHALL NOT be to a fixed number of decimal places. A non-zero value SHALL NEVER round to `0`: `rate` is a per-second value whose magnitude scales inversely with the caller's window, and `error_rate` can be legitimately tiny on a high-traffic edge, so an absolute precision floor would contradict this capability's guarantee that an emitted `rate` is strictly greater than zero, and would collide with the defined meaning of `error_rate == 0` ("read successfully, no failures").

#### Scenario: Two series collapse into one edge

- **WHEN** two `traces_service_graph_request_total` series differing only in `connection_type`, both carrying both pod UIDs, resolve to the same pod-to-pod edge with rates `2` and `3`
- **THEN** the edge's `rate` is `5`

#### Scenario: Out-of-scope contributing series are excluded from rate

- **WHEN** an edge receives contributions from one series carrying both pod UIDs (rate `4`) and one `server="unknown"` series with an empty `server_k8s_pod_uid` whose peer address resolves to the same target pod (rate `1`)
- **THEN** the edge carries no metrics object at all, because condition 3 of the attachment rule is evaluated per contributing series and the edge's identity is not wholly UID-resolved

#### Scenario: Error rate uses the matching series set

- **WHEN** two in-scope series resolve to one edge with total rates `2` and `3`, and only the first has a matching failure series with rate `1`
- **THEN** the edge's `error_rate` is `0.2`

#### Scenario: p90 is computed from summed buckets

- **WHEN** two pod-pair identities map to one edge and each contributes its own duration histogram
- **THEN** the edge's `p90_server_ms` is derived from the per-`le` sum of both bucket sets, not from combining two per-identity quantiles

#### Scenario: Attached values are order-independent

- **WHEN** the same upstream data is returned twice with the contributing series in different orders
- **THEN** the two responses are byte-identical, including every attached numeric value

#### Scenario: A very small non-zero value never rounds to zero

- **WHEN** an edge's window is long enough (or its failure count rare enough) that `rate` or `error_rate` evaluates to a value far below one part in a million — for example a single request over a 30-day window
- **THEN** the emitted value is a non-zero JSON number carrying the configured number of significant digits, and is not `0`

#### Scenario: Quantile above the highest finite bucket

- **WHEN** the 90th percentile falls into the `+Inf` bucket
- **THEN** `p90_server_ms` is the highest finite `le` boundary converted to milliseconds

### Requirement: RED graceful degradation

Neither new query SHALL be able to fail a build. The reader SHALL degrade as follows and SHALL continue to emit the graph:

- The failure-counter query returns an error, or the metric does not exist upstream: the reader SHALL OMIT `error_rate` from every edge's metrics object rather than reporting `0`, so an absent measurement is never presented as a measured absence of errors.
- The failure-counter query succeeds but has no series matching a given edge: that edge's `error_rate` SHALL be `0`.
- The duration-histogram query returns an error, the metric does not exist upstream, or its series carry no classic `le` bucket boundaries (for example because the producer emits native histograms, which a store may expose as `vmrange` buckets): the reader SHALL OMIT `p90_server_ms` from every affected edge's metrics object.
- Both new queries fail or return nothing: qualifying pod-to-pod edges SHALL still carry a metrics object containing only `rate`.
- The existing `traces_service_graph_request_total` query retains its current behaviour: an error there still fails the build.

A **non-finite upstream sample SHALL NOT reach the response**. Upstream exposition formats accept `NaN` and `+Inf`, and JSON has no representation for either, so a single poisoned sample would otherwise make the whole response body unencodable and turn one bad series into a failed request for the entire graph. The reader SHALL therefore: skip non-finite samples when accumulating; omit the metrics object entirely when an edge's aggregated `rate` is not a finite value greater than zero; and omit `p90_server_ms` when the computed quantile is not finite. Degrading one edge is always preferred to failing the request.

The reader SHALL additionally detect the case where the failure counter was read successfully but **joined to nothing**: if the failure result is non-empty yet no series matched any edge, every edge would report `error_rate: 0`, which is indistinguishable from a measured absence of failures. The reader SHALL surface that signature as aggregated operator evidence naming the distinct reason, so a label-set divergence between the request and failure counters is diagnosable rather than silently reading clean.

Each degradation SHALL be surfaced as aggregated operator evidence in the logs, naming the query and the reason, not per edge, and SHALL NOT alter any node, edge, `id`, `type`, `source`, `target`, or `labels`.

#### Scenario: Failure counter absent upstream

- **WHEN** `traces_service_graph_request_failed_total` does not exist in the upstream store
- **THEN** the build succeeds and qualifying edges carry a metrics object with `rate` (and `p90_server_ms` when available) but no `error_rate` key

#### Scenario: Histogram disabled at the producer

- **WHEN** `traces_service_graph_request_server_seconds_bucket` returns no series
- **THEN** the build succeeds and qualifying edges carry a metrics object with `rate` and `error_rate` but no `p90_server_ms` key

#### Scenario: Histogram carries no classic bucket boundaries

- **WHEN** the duration-histogram query returns series that carry no `le` label
- **THEN** the build succeeds, `p90_server_ms` is omitted from every edge, and the reason is logged as aggregated evidence distinguishable from "metric absent"

#### Scenario: Both RED queries error

- **WHEN** both the failure-counter and the duration-histogram queries return an upstream error
- **THEN** the build still succeeds, the response is unchanged apart from qualifying edges carrying a metrics object with only `rate`, and the failures are logged as aggregated evidence

#### Scenario: Non-finite rate degrades one edge, not the request

- **WHEN** an otherwise qualifying edge's contributing series carry a `+Inf` or `NaN` value
- **THEN** that edge carries no metrics object, every other edge is unaffected, and the response body remains JSON-encodable

#### Scenario: Failure counter read successfully but joined nothing

- **WHEN** the failure-counter query returns a non-empty result and none of its series matches any edge's contributing series identity
- **THEN** the build succeeds and the condition is logged once as aggregated evidence under its own reason, so the resulting `error_rate: 0` on every edge is diagnosable as a join failure rather than read as a measured absence of failures

#### Scenario: Request-total query error still fails the build

- **WHEN** the `traces_service_graph_request_total` query returns an upstream error
- **THEN** the build fails as it does today — the graceful degradation applies only to the two new queries

## MODIFIED Requirements

### Requirement: Virtual sentinel endpoint exclusion (user / unknown)

The reader SHALL exclude any `traces_service_graph_request_total` series whose `client`
label is exactly `"user"` or exactly `"unknown"`. These are **virtual peers** emitted by
the service-graph producer (the OpenTelemetry / Alloy / Tempo `servicegraph` connector) for
endpoints it cannot pair to an instrumented span — an uninstrumented caller surfaces as
`client="user"`, an unresolved peer as `"unknown"` — and they carry no pod UID and
represent no pod, service, or declared external dependency the API should surface.

The **client-side** exclusion SHALL be applied **at the PromQL query layer** via an
anchored negative label matcher on the series selector — `client!~"user|unknown"` — so a
series with either sentinel value on its `client` label is never returned by upstream
VictoriaMetrics and never reaches endpoint resolution.

The **server-side** exclusion is narrower: the query-layer matcher SHALL only exclude
`server` values exactly equal to `"user"` (`server!~"user"`). A series with `server`
exactly `"unknown"` SHALL reach Go-side resolution — it is no longer excluded at the query
layer — but the reader SHALL still treat it as unresolvable and drop it (no node, no edge)
UNLESS the "Unknown-server peer-label enrichment" requirement below applies. The narrower
server-side matcher exists solely to let that requirement's peer-label enrichment observe
the series' other labels (`client_server_address`, `client_network_peer_address`,
`client_net_peer_name`); it SHALL NOT be read as a general relaxation of the sentinel
rule — every `server="unknown"` case outside that requirement's narrow trigger condition
SHALL produce exactly the same outward result (dropped, no node, no edge) as the prior
query-layer exclusion.

Matching semantics:

- **Exact, fully anchored**: the PromQL `!~` regex is anchored to the entire label value,
  so only a label whose *whole* value is `user` or `unknown` is excluded (client side) or
  `user` (server side). A connection-string value such as `"http://user/path"` is NOT
  excluded (its value is not exactly `user`) and proceeds to connection-string resolution
  unchanged.
- **Case-sensitive**: `User`, `UNKNOWN`, and other case variants are NOT excluded.
- **Fixed set, no knob**: the sentinel set `{"user", "unknown"}` (client) / `{"user"}`
  (server) is compiled in. There is NO configuration surface (env var / flag / config
  field) to change either.

This exclusion is distinct from — and SHALL NOT affect — the `cluster="unknown"` bucketing
applied to series missing a `cluster` external label (a different label on a different
dimension): the sentinel matchers are evaluated ONLY against the `client` and `server`
endpoint labels.

Because the client-side-excluded series never arrive, no endpoint resolution runs for
them: no pod, synthesised-pod, `service`, or `external` node is materialised for a `user` /
`unknown` client sentinel, and no edge touching such a peer is emitted. A `server="unknown"`
series that reaches Go and does not satisfy the peer-label enrichment trigger is dropped in
Go with the identical observable effect (no node, no edge) — see the "Unknown-server
peer-label enrichment" requirement for the one case that resolves instead.

The same sentinel matchers (client: `user|unknown`; server: `user`) SHALL be applied,
byte-identically, to the selectors of the numeric service-graph series
(`traces_service_graph_request_failed_total`,
`traces_service_graph_request_server_seconds_bucket`) read by the "RED source series and
selector consistency" requirement, so the edge population stays consistent across metric
families. Those selectors carry additional request-invariant matchers of their own; that
does not weaken this rule, which fixes only the sentinel fragment.

#### Scenario: Series with client `user` is excluded at the query layer

- **WHEN** upstream holds a `traces_service_graph_request_total` series with `client="user"`, `server="checkout"`, `server_k8s_pod_uid="abc"`
- **THEN** the service-graph query selector includes `client!~"user|unknown"`, VictoriaMetrics does not return the series, and the graph contains no edge for it and no node named `user`

#### Scenario: Series with server `unknown` and no usable peer label is still dropped

- **WHEN** upstream holds a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a real topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and the series carries none of `client_server_address`, `client_network_peer_address`, or `client_net_peer_name`
- **THEN** the series is no longer excluded at the query layer (`server!~"user"` only), but Go-side resolution drops it per the "Unknown-server peer-label enrichment" requirement's no-match case: the graph contains no edge for it and no node named `unknown`, `external/unknown`, or otherwise — identical to the outcome under the prior `server!~"user|unknown"` exclusion

#### Scenario: Series with server `unknown` and an unresolved client is still dropped

- **WHEN** upstream holds a series with `client="admin"`, `client_k8s_pod_uid=""` (the client side does NOT resolve to a real topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and the series carries a `client_network_peer_address` label
- **THEN** the peer-label enrichment trigger requires a resolved client-side pod, which this series lacks, so the series is dropped exactly as under the prior exclusion — the presence of a peer label does not by itself cause resolution

#### Scenario: Both endpoints are sentinels

- **WHEN** a series has `client="user"` and `server="unknown"`
- **THEN** the series is excluded at the query layer by the client-side matcher (`client!~"user|unknown"`) regardless of the server-side matcher or any peer label, and no edge is emitted

#### Scenario: Connection-string value containing `user` is not excluded

- **WHEN** a series has `server="http://user/api"`, `server_k8s_pod_uid=""` (the value contains, but is not equal to, `user`)
- **THEN** the series is NOT excluded (the matcher is fully anchored), and connection-string endpoint resolution proceeds normally for that endpoint

#### Scenario: `cluster="unknown"` bucketing is unaffected

- **WHEN** a series is missing its `cluster` external label and is bucketed to `cluster="unknown"`, while its `client` and `server` labels are real service names with resolvable pod UIDs
- **THEN** the series is NOT excluded by the sentinel matchers (they match only `client` / `server`, never `cluster`), and the edge is emitted under `cluster="unknown"` exactly as before

#### Scenario: Numeric series carry the identical sentinel fragment

- **WHEN** the reader issues the `traces_service_graph_request_failed_total` and `traces_service_graph_request_server_seconds_bucket` queries
- **THEN** each selector contains the same `client!~"user|unknown",server!~"user"` fragment as the request-total selector, so no sentinel peer contributes a measurement to any edge

## REMOVED Requirements

### Requirement: Numeric metrics deferred from v1

**Reason**: Superseded by "RED edge metrics on UID-resolved pod-to-pod edges". The deferral existed only because `Edge.labels` is strictly `map[string]string` and there was no typed place to carry a float; this change adds that typed place, so numeric metrics are no longer deferred. The prohibition the requirement actually protected — no numbers inside `labels` — is preserved verbatim by the replacing requirement and by the `graph-api` edge-payload requirement.

**Migration**: Consumers that relied on edges carrying no numeric data continue to work unchanged: the new data lives on an additive, omitted-when-absent `data.metrics` object and `labels` is untouched. Consumers that want RED read `data.metrics.rate`, `data.metrics.error_rate`, and `data.metrics.p90_server_ms`, each of which may be absent.
