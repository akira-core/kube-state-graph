## MODIFIED Requirements

### Requirement: Self-metrics endpoint

The server SHALL expose `GET /metrics` in Prometheus exposition format including at least: `kube_state_graph_build_duration_seconds`, `kube_state_graph_project_duration_seconds`, `kube_state_graph_serialise_duration_seconds`, `kube_state_graph_build_rejected_total`, `kube_state_graph_graph_node_count`, `kube_state_graph_graph_edge_count`, `kube_state_graph_clusters_observed`, `kube_state_graph_upstream_query_duration_seconds`, `kube_state_graph_upstream_query_failures_total`, `kube_state_graph_http_requests_total`, `kube_state_graph_auth_rejected_total`, `kube_state_graph_upstream_backends`, `kube_state_graph_backend_config_reload_total`, `kube_state_graph_backend_query_failures_total`, and `kube_state_graph_upstream_query_result_series`.

`kube_state_graph_upstream_query_duration_seconds` and `kube_state_graph_upstream_query_failures_total` SHALL keep exactly the label sets they carried before backend routing existed: adding a label to an existing self-metric is a contract change, so per-backend detail is carried by `kube_state_graph_backend_query_failures_total` instead.

`kube_state_graph_upstream_query_result_series` SHALL be a histogram labelled by `query` only, observing the number of series each successful upstream query returned — one observation per issued query, so a scoped read split into several chunks contributes one observation per chunk under the bare family name, and a query fanned out to several backends contributes one per backend. Its buckets SHALL be the powers of two from 1024 to 1048576, so an operator can read how close a leg sits to an upstream series limit (VictoriaMetrics' memory-derived `-search.maxUniqueTimeseries`) before that limit rejects the query. A failed query SHALL contribute no observation.

#### Scenario: Metrics exposition

- **WHEN** a client sends `GET /metrics`
- **THEN** the response is 200 in `text/plain; version=0.0.4` exposition format and includes all metric names listed above

#### Scenario: cluster label on observational gauges

- **WHEN** a build has produced a multi-cluster graph
- **THEN** `kube_state_graph_graph_node_count` series include a `cluster` label and `kube_state_graph_graph_edge_count` series include a `cross_cluster` label

#### Scenario: Backend metrics carry the backend label

- **WHEN** a query to backend `zone-b` fails and a client scrapes `/metrics`
- **THEN** `kube_state_graph_backend_query_failures_total` carries a series labelled with `zone-b`, and `kube_state_graph_upstream_query_failures_total` carries no backend label

#### Scenario: Backend gauge present with no routing table

- **WHEN** the server runs with only `--prom-url` configured
- **THEN** `kube_state_graph_upstream_backends` reads 1

#### Scenario: Result-series histogram after a build

- **WHEN** a `/v1/graph` build has completed and a client scrapes `/metrics`
- **THEN** `kube_state_graph_upstream_query_result_series_count{query="kube_pod_info"}` is at least 1, its `_bucket` series carry `le` values `1024`, `2048`, … `1048576` and `+Inf`, and no series of the metric carries a `backend` label
