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

### Requirement: Availability-zone and environment selector filters

`GET /v1/graph` SHALL accept the optional, repeatable parameters `az` and `env`. Each SHALL be rendered as an upstream label matcher on every kube-state-metrics and kubelet query of the build, on **no** NetApp Harvest query, and on no service-graph query; the `up{}` probe SHALL never carry them. `az` additionally selects which `harvest` backends are asked (the `upstream-backend-routing` capability's zone rule); that selection is the only effect `az` has on the Harvest legs, and `env` has none. The upstream label each parameter binds to is the operator-configured key of the `cluster-topology-source` capability ("Configurable `az` / `env` label keys"), defaulting to `az` and `env`; the request parameter names themselves are fixed.

The two filters narrow **at the source**: a series that lacks the configured label does not match an equality matcher and is therefore absent from the build. The operator SHALL ensure every kube-state-metrics and kubelet family stamps both labels; a family that does not vanishes from every `az` / `env`-filtered request, and because the default projection keeps only connectivity-connected workload, a topology family missing the label yields an empty graph for that filter rather than a partial one. The Harvest families are exempt: they carry no matcher, so they need no label. The response `clusters` list, derived from the built graph's node `cluster` labels, SHALL therefore list only the clusters with data in the requested zone / environment.

#### Scenario: Environment filter selects one environment's clusters

- **WHEN** the upstream holds `cluster-prod-1` (all series `env="prod"`) and `cluster-dev-1` (all series `env="dev"`) and a client sends `?env=prod`
- **THEN** every kube-state-metrics and kubelet query carries `env="prod"`, the response contains only `cluster-prod-1` workload and infrastructure, and `clusters` is `["cluster-prod-1"]`

#### Scenario: Zone and environment are AND-combined

- **WHEN** `cluster-a` carries `az="zone-a",env="prod"`, `cluster-b` carries `az="zone-b",env="prod"`, and a client sends `?az=zone-a&env=prod`
- **THEN** the response contains `cluster-a` only

#### Scenario: Configured key is used in the matcher

- **WHEN** the server runs with `KSG_AZ_LABEL=topology_zone` and a client sends `?az=zone-a`
- **THEN** the rendered matcher is `topology_zone="zone-a"`, and the request parameter is still named `az`

#### Scenario: Family lacking the label vanishes under the filter

- **WHEN** the kube-state-metrics series carry `env="prod"` but the kubelet series carry no `env` label, and a client sends `?env=prod`
- **THEN** the response contains the prod pods, nodes, and claims but no claim carries kubelet usage (the kubelet legs returned nothing), and the build does not fail

#### Scenario: Harvest lacking the label still joins under the filter

- **WHEN** the kube-state-metrics and kubelet series carry `env="prod"`, the Harvest series carry no `env` label, and a client sends `?env=prod`
- **THEN** the Harvest legs are issued without an `env` matcher and return their rows, so the prod claims that join a `volume_labels` series receive their `netapp-aggr` / `netapp-node` nodes and `pvc-to-netapp-aggr` edges
