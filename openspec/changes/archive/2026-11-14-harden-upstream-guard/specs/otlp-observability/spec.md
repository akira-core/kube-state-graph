## MODIFIED Requirements

### Requirement: PromQL query spans and outbound trace propagation

Each parallel PromQL query in the topology and service-graph errgroups SHALL run inside its own child span named `prometheus.query`, and the PromQL HTTP client SHALL inject W3C `traceparent` headers into outbound VictoriaMetrics requests using the global propagator. The span's `db.statement`, and the `query` field of the client's query logs, SHALL carry the rendered PromQL string truncated to at most 2048 bytes on a UTF-8 character boundary; the span SHALL also carry `kube_state_graph.query_bytes` (the full statement's byte length) and `kube_state_graph.query_sha256` (the first 12 hexadecimal digits of the full statement's SHA-256), and the logs the same two values as `query_bytes` and `query_sha256`, so an over-long statement cannot inflate a span or a log record while it can still be correlated.

#### Scenario: PromQL query span attributes

- **WHEN** any fan-out leg issues its PromQL query
- **THEN** its `prometheus.query` span carries `db.system=prometheus`, `db.statement=<rendered PromQL string, at most 2048 bytes>`, `kube_state_graph.query_bytes`, `kube_state_graph.query_sha256`, and `kube_state_graph.query_name=<one of: kube_pod_info | kube_node_info | kube_node_status_addresses | kube_pod_spec_volumes_persistentvolumeclaims_info | kube_node_labels | traces_service_graph_request_total | …>`

#### Scenario: PromQL query span carries query name and statement

- **WHEN** the topology reader fans out the `kube_pod_info` query
- **THEN** a span named `prometheus.query` is emitted with `kube_state_graph.query_name="kube_pod_info"`, `db.system="prometheus"`, and `db.statement` equal to the rendered PromQL string passed to VictoriaMetrics, which is shorter than 2048 bytes

#### Scenario: PromQL outbound traceparent propagation

- **WHEN** the topology reader issues a PromQL HTTP request to VictoriaMetrics from inside a `prometheus.query` span
- **THEN** the outbound request carries a `traceparent` header whose trace ID and parent span ID match the active `prometheus.query` span context

#### Scenario: Over-long statement is truncated and fingerprinted

- **WHEN** a query whose rendered string is 20,000 bytes fails upstream
- **THEN** its span's `db.statement` and its Error log's `query` are the first 2048 bytes (or fewer, ending on a character boundary), and both carry the length 20000 and the 12-digit SHA-256 prefix of the full string
