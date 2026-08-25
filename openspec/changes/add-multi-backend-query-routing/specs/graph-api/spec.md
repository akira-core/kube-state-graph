## MODIFIED Requirements

### Requirement: Health endpoints

The server SHALL expose `GET /livez` that returns 200 while the process is running, and `GET /readyz` that returns 200 only when a 1-second `up{}` probe against **every** backend in the live routing table succeeds. `GET /readyz` SHALL return 503 otherwise, with a JSON body carrying a `reason` field that names the backends that did not answer. In a deployment with no routing table configured, the single implicit `default` backend is the only one probed, so the observable behaviour is unchanged.

The probes SHALL be issued concurrently and SHALL share the single 1-second budget, so readiness latency does not grow with the number of backends.

#### Scenario: livez always healthy while running

- **WHEN** a client sends `GET /livez`
- **THEN** the response is 200 with body `"ok"` regardless of upstream state

#### Scenario: readyz fails when upstream unreachable

- **WHEN** the configured VictoriaMetrics URL refuses connections and a client sends `GET /readyz`
- **THEN** the response is 503 with a JSON body containing a `reason` field

#### Scenario: readyz fails when one of several backends is unreachable

- **WHEN** three backends are configured, two answer and one refuses connections, and a client sends `GET /readyz`
- **THEN** the response is 503 and the `reason` field names the refusing backend

#### Scenario: readyz succeeds when every backend answers

- **WHEN** every configured backend answers the probe within the budget
- **THEN** the response is 200

### Requirement: Self-metrics endpoint

The server SHALL expose `GET /metrics` in Prometheus exposition format including at least: `kube_state_graph_build_duration_seconds`, `kube_state_graph_project_duration_seconds`, `kube_state_graph_serialise_duration_seconds`, `kube_state_graph_build_rejected_total`, `kube_state_graph_graph_node_count`, `kube_state_graph_graph_edge_count`, `kube_state_graph_clusters_observed`, `kube_state_graph_upstream_query_duration_seconds`, `kube_state_graph_upstream_query_failures_total`, `kube_state_graph_http_requests_total`, `kube_state_graph_auth_rejected_total`, `kube_state_graph_upstream_backends`, `kube_state_graph_backend_config_reload_total`, and `kube_state_graph_backend_query_failures_total`.

`kube_state_graph_upstream_query_duration_seconds` and `kube_state_graph_upstream_query_failures_total` SHALL keep exactly the label sets they carried before backend routing existed: adding a label to an existing self-metric is a contract change, so per-backend detail is carried by `kube_state_graph_backend_query_failures_total` instead.

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
