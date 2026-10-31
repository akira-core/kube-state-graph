# Proposal

## Why

A node's `data.alerts` entry says an alert was firing at some point inside the request window, but not when. A consumer cannot tell an alert that is firing right now from one that fired and resolved an hour ago, so it cannot order alerts by recency or show how stale an entry is. The `ALERTS` read already sees each series' samples in the window; only the timestamp is discarded.

## What Changes

- Each `data.alerts` entry gains an integer `time` field: the Unix epoch second of the last `firing` sample of that alert inside the request window ("last seen firing"). It is not the alert's activation time.
- The `ALERTS` read changes from `last_over_time(ALERTS{…}[w])` to the MetricsQL `tlast_over_time(ALERTS{…}[w])`, whose value is each series' last-sample timestamp. The selector, routing, family and degradation do not change, and the series selection is the same, so matching is unaffected.
- When the alerts store rejects the query as invalid (a plain Prometheus does not know `tlast_over_time`), the leg retries once with the portable `last_over_time` read. Alerts then still attach and still feed `data.status`; only `time` is absent. A dial or timeout error does not retry.
- When several `ALERTS` series collapse into one `(name, severity)` entry, the entry's `time` is the greatest of their timestamps. When two backends serving the `alerts` family hold the same series, the cross-backend merge keeps the copy with the greater value (the later timestamp) instead of the first backend's.
- `time` is omitted when it did not resolve: a value below one second, a non-finite value, or a value outside the request window (which is what the portable fallback's constant `1` is).
- `time` does not feed `data.status`; `severity` stays the only alert input to the fold.
- In-process: `graph.Alert` gains a `time.Time` field. The zero value means unresolved.
- Additive wire change on all three graph endpoints (`/v1/graph`, `/v1/storage-graph`, `/v1/k8s-graph`) and the in-process engine, since alerts are attached in the shared `freeze` step. Not breaking for HTTP consumers.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `alert-overlay`: the `ALERTS` read uses `tlast_over_time` instead of `last_over_time`; the node `alerts` entry shape gains `time`, and its merge rule for collapsed series is defined.
- `graph-api`: the "Node `alerts` attribute" entry shape gains `time` (epoch seconds, `omitempty`).

## Impact

- Code: `pkg/promql/queries.go` (render of `QAlerts`, the portable fallback render, the bad-query classifier), `pkg/promql/fanout.go` (max-value merge for the `alerts` family), `pkg/graph/node.go` (`Alert`, `SortAlerts` merge), `pkg/build/alerts.go` (read the sample value, window bound), `pkg/build/topology_read.go` (fallback retry), `pkg/cytoscape/cytoscape.go` (`AlertDTO`, `alertsDTO`).
- Tests and fixtures: `pkg/promql/alerts_test.go`, `pkg/promql/testdata/render-baseline.txt`, `pkg/build/alerts_test.go`, `pkg/build/testdata/storage_fanout.golden`, `internal/api/{storage_graph,k8s_graph}_test.go`, `pkg/kubegraph/storage_namespace_test.go`, the golden bodies carrying alerts, and the integration suite's `ALERTS` fixtures.
- Generated: `docs/swagger.{json,yaml}` (`AlertDTO` gains `time`) via `make docs`.
- Docs: `docs/upstream-metrics.md` (the `ALERTS` row).
- Upstream: `tlast_over_time` is MetricsQL. The backend already uses it for the optional `kube_pod_container_info` and application-metadata legs, and VictoriaMetrics is the documented upstream. A plain Prometheus `alerts` backend rejects it; the leg then falls back to the portable read, so alerts and their status contribution survive without `time`. Not breaking. The cost on such a backend is one rejected query per build, which counts in `kube_state_graph_upstream_query_failures_total{query="ALERTS"}`.
- No new dependency, flag, metric or route.
