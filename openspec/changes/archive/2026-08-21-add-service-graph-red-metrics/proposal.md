## Why

The `traces_service_graph_*` family produced by the OTel-collector `servicegraph`
connector (and the compatible Tempo metrics-generator / Grafana Alloy connectors)
already carries the full RED triple — `request_total` (Rate),
`request_failed_total` (Errors) and `request_server_seconds_bucket` (Duration) — on
**exactly the same label set** the reader already consumes for `pod-calls-pod` edge
resolution. Today the reader queries only the first of the three and throws the
numbers away entirely: an edge tells the caller *that* pod A calls pod B, never *how
much*, *how badly*, or *how slowly*. That makes the graph unusable for the two
questions operators actually bring to a dependency graph — "which dependency is hot?"
and "which dependency is failing?" — and forces them back to Grafana with a
hand-written PromQL query per edge.

The v1 spec deliberately deferred this (`Requirement: Numeric metrics deferred from
v1`) because `Edge.Labels` is strictly `map[string]string` and there was no typed
place to put a float. This change adds that typed place and fills it from the two
metrics the promoted spec already declares the reader SHALL consume.

## What Changes

- **New typed edge attribute `graph.EdgeMetrics`** (`Rate`, `ErrorRate`,
  `P90ServerMs`), carried on `graph.Edge` as a nullable `Metrics *EdgeMetrics` and
  serialised as the `omitempty` `data.metrics` object on the Cytoscape edge DTO.
  Numeric values NEVER enter `labels` — same precedent as node `ipaddress` / `owner`
  / `ready_status`.
- **RED is attached to trace-derived edges whose two endpoints both resolved to a pod
  or a service.** An edge receives `data.metrics` iff it was produced from at least one
  `traces_service_graph_request_total` series **and both resolved endpoints name a
  `type="pod"` node (real topology pod or synthesised pod) or a `type="service"` node**.
  How the endpoint was identified does not matter: a pod UID, a `"://"` connection
  string resolved to a Service, a `server="unknown"` peer address resolved to a
  Service `ClusterIP`, a peer address resolved straight to a Pod IP, and an Istio
  route-engine resolution to a backend Service all qualify. Every other edge carries
  no `metrics` key at all:
  - any edge with an `external` endpoint on either side — the external node collapses
    all traffic behind one label string and its id is not cluster-scoped, so the
    number would not name a measurable dependency;
  - synthesised edges, which have no contributing series to measure —
    `service-selects-pod` (D29 fan-out), `pod-to-node`, `pvc-to-storageclass`,
    `pod-mounts-pvc`, and the route-hit ingress-chain's gateway-pod → backend-service
    hop;
  - the route-hit ingress chain's **caller → ingress-service entry hop**. It is
    trace-derived and both its endpoints are pod/service, but it and the retained
    caller → backend edge are two projections of the **same** call: measuring both
    would make one request contribute twice to any sum over the chain. The backend
    edge — the one naming the actual destination — carries the measurement.
- **A contributing series carrying `edge_relation="link"` measures nothing.** That
  dimension marks a virtual edge the connector materialised from a **span link**, so
  the "call" it describes actually crossed a queue or a database and the two spans
  belong to different trace contexts. The edge is still emitted — it is a real
  dependency — but the series is **out of scope** for RED: it contributes to no rate,
  no error numerator, and no duration bucket. An edge whose contributing series are
  *all* link series therefore carries no `metrics` object at all, while a mixed edge is
  measured over its non-link series only. The label name is a fixed, case-sensitive
  contract (same class as the D30 sentinel values) — there is no knob.
- **Grafana/Tempo definitional parity for Duration.** `p90_server_ms` uses the same
  metric, observation side, and quantile as Grafana's documented service-graph
  queries: `histogram_quantile(0.9, ...traces_service_graph_request_server_seconds_bucket...)`.
  The values are not numerically identical to Grafana's, because Grafana aggregates by
  service name (`client`, `server`) while this API aggregates by pod pair — the parity
  is in the definition, not the number.
- **Two new PromQL queries** in `ReadServiceGraph`, run in parallel with the existing
  one: `traces_service_graph_request_failed_total` and
  `traces_service_graph_request_server_seconds_bucket`. Both are read at the **same raw
  label granularity as the total counter** (the histogram additionally carrying `le`)
  and both join to a contributing series by exact label identity, so the Rate
  denominator, the Errors numerator, and the Duration buckets always come from one
  identical series set. Both reuse the D30 sentinel selector fragment, as
  `promql.serviceGraphSentinelSelector`'s own doc comment already mandates, plus a
  request-invariant `edge_relation!="link"` matcher mirroring the out-of-scope rule
  above. The `_total` selector does **not** gain that matcher — the link edge must
  still be emitted, it just carries no numbers.
- **Both new queries are OPTIONAL and non-fatal.** A missing metric, an empty result,
  or a query error degrades to "no `metrics` on the edge" (or "no `p90_server_ms`
  inside `metrics`") and never fails the build.
- `p90_server_ms` is computed **in-process** from the summed classic `le` buckets,
  never via a PromQL `histogram_quantile`, because several upstream series
  legitimately collapse onto one edge and pre-computed quantiles cannot be
  re-aggregated.
- **Upstream metric contract documented in `README.md`**, in the existing
  "Service-graph metric" table format — metric name, what it is used for, labels read,
  and whether it is required — so an operator can configure their producer without
  reading Go source.
- **BREAKING: none.** The response body gains an additive `omitempty` field.

## Capabilities

### New Capabilities

None — this extends two existing capabilities.

### Modified Capabilities

- `pod-service-graph`: **replaces** `Requirement: Numeric metrics deferred from v1`
  with requirements that define RED derivation, the pod/service-endpoint attachment
  rule, the span-link out-of-scope rule, the series→edge join, deterministic
  aggregation, and graceful degradation.
  **Modifies** `Requirement: Virtual sentinel endpoint exclusion (user / unknown)`,
  whose forward note about "deferred numeric service-graph metrics … queried in a
  future spec revision" this change makes true and must therefore be rewritten in the
  present tense. `Requirement: Pod-UID-resolved edge source` needs **no** change: it
  already declares all three series as ones the reader SHALL consume "at minimum", and
  the Duration series it names (`..._server_seconds_bucket`) is exactly the one used.
- `graph-api`: the serialised edge object gains the additive, `omitempty`
  `data.metrics` attribute; `labels` remains strict `map[string]string` and MUST NOT
  gain `rate` / `error_rate` / `p90_server_ms` keys.

## Impact

- `pkg/graph/edge.go` — new `EdgeMetrics` type, `Edge.Metrics` field, immutable
  `WithMetrics` constructor; edge ID derivation unchanged (still
  `UUIDv5(type|source|target)`), so no edge ID churn.
- `pkg/promql/queries.go` — two new `Query` constants + render cases; the shared
  `serviceGraphSentinelSelector` gains two more call sites; a new
  `serviceGraphLinkExclusionSelector` fragment. The histogram render carries no
  upstream `sum by` aggregation.
- `pkg/build/servicegraph.go` — `ReadServiceGraph` fans out three queries under an
  errgroup; `parseWithResolver` records one `seriesKey → pairKey` join map serving both
  the failure and the bucket vectors, tracks endpoint node types and the route-chain
  entry hop for eligibility, and attaches the aggregated RED after the resolution loop.
- `pkg/build/histogram.go` (new) — classic-bucket quantile, pure + unit-tested.
- `pkg/cytoscape/cytoscape.go` — `EdgeData.Metrics *EdgeMetricsDTO`.
- `internal/api/testdata/golden/*.json` — regenerated (`-update`).
- `docs/swagger.{json,yaml}` — regenerated (`make docs`).
- `README.md` — "Service-graph metric" section gains the two new metrics and the RED
  attachment rule; "Edge → metric mapping" table unchanged.
- `CLAUDE.md` — a compact glossary block (trace-derived / synthesised edge,
  UID-resolved / peer-resolved endpoint, contributing series, in-scope series, RED
  scope) ahead of the service-graph rules, plus the retirement of the "numeric edge
  metrics deferred" statement.
- Self-metrics: two new `query` / `query_name` dimension values.
- Upstream load: +2 PromQL queries per `/v1/graph` request. No new dependency, no new
  flag, no new node type, no new edge type.
