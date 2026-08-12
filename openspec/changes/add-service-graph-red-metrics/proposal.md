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
- **RED is attached to trace-derived, UID-resolved pod↔pod edges ONLY.** An edge
  receives `data.metrics` iff it is a `pod-calls-pod` edge produced from at least one
  `traces_service_graph_*` series **and both endpoints were resolved from a non-empty
  `client_k8s_pod_uid` / `server_k8s_pod_uid`**. Every other edge carries no `metrics`
  key at all:
  - synthesised / derived edges — `service-selects-pod` (D29 fan-out), `pod-to-node`,
    `pvc-to-storageclass`, `pod-mounts-pvc`, and the route-hit ingress-chain
    `pod-calls-service` hop;
  - `pod-calls-service` edges (target is a materialised service node, not a pod);
  - `pod-calls-pod` edges with an `external` endpoint;
  - `pod-calls-pod` edges whose target was **peer-resolved** rather than UID-resolved
    — i.e. the `server="unknown"` peer-address ladder resolving straight to a Pod IP.

  Rationale for the last exclusion: the peer-address ladder only runs *because* the
  `servicegraph` connector could not pair a server span. A peer-resolved endpoint is
  by definition an endpoint whose own side emitted no trace, so the RED series carry
  no measurement for it — the connector never observed it. Attaching numbers there
  would be attributing a half-observed call as if it were fully measured. When both
  pods do emit traces the pairing succeeds, the UID is populated, and the edge gets
  RED through the ordinary path.
- **Grafana/Tempo definitional parity for Duration.** `p90_server_ms` uses the same
  metric, observation side, and quantile as Grafana's documented service-graph
  queries: `histogram_quantile(0.9, ...traces_service_graph_request_server_seconds_bucket...)`.
  The values are not numerically identical to Grafana's, because Grafana aggregates by
  service name (`client`, `server`) while this API aggregates by pod pair — the parity
  is in the definition, not the number.
- **Two new PromQL queries** in `ReadServiceGraph`, run in parallel with the existing
  one: `traces_service_graph_request_failed_total` (raw, joined by series identity)
  and `traces_service_graph_request_server_seconds_bucket` (upstream-aggregated by the
  pod-pair identity + `le`, to keep histogram cardinality off the wire). Both reuse
  the D30 sentinel selector fragment, as `promql.serviceGraphSentinelSelector`'s own
  doc comment already mandates, plus a request-invariant
  `client_k8s_pod_uid!="",server_k8s_pod_uid!=""` pair that makes the query population
  exactly equal to the attachment rule above.
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
  with requirements that define RED derivation, the UID-resolved-pod-pair attachment
  rule, the series→edge join, deterministic aggregation, and graceful degradation.
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
  `serviceGraphPodPairSelector` fragment.
- `pkg/build/servicegraph.go` — `ReadServiceGraph` fans out three queries under an
  errgroup; `parseWithResolver` records the series→pair join keys and attaches the
  aggregated RED after the resolution loop.
- `pkg/build/histogram.go` (new) — classic-bucket quantile, pure + unit-tested.
- `pkg/cytoscape/cytoscape.go` — `EdgeData.Metrics *EdgeMetricsDTO`.
- `internal/api/testdata/golden/*.json` — regenerated (`-update`).
- `docs/swagger.{json,yaml}` — regenerated (`make docs`).
- `README.md` — "Service-graph metric" section gains the two new metrics and the RED
  attachment rule; "Edge → metric mapping" table unchanged.
- `CLAUDE.md` — a compact glossary block (trace-derived / synthesised edge,
  UID-resolved / peer-resolved endpoint, contributing series, RED scope) ahead of the
  service-graph rules, plus the retirement of the "numeric edge metrics deferred"
  statement.
- Self-metrics: two new `query` / `query_name` dimension values.
- Upstream load: +2 PromQL queries per `/v1/graph` request. No new dependency, no new
  flag, no new node type, no new edge type.
