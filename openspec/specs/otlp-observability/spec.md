# otlp-observability Specification

## Purpose
TBD - created by archiving change add-k8s-pod-graph-api. Update Purpose after archive.
## Requirements
### Requirement: OpenTelemetry SDK initialisation from standard environment variables

The server SHALL initialise the OpenTelemetry Go SDK at startup using **only** the OTel-standard environment variables: `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_EXPORTER_OTLP_TRACES_*`, `OTEL_EXPORTER_OTLP_LOGS_*`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`, and `OTEL_TRACES_SAMPLER_ARG`. The server SHALL NOT introduce bespoke `--otlp-*` CLI flags, and SHALL NOT read the OTLP endpoint from any non-OTel-prefixed source.

When `OTEL_EXPORTER_OTLP_ENDPOINT` (or its per-signal `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` override) is unset, the SDK SHALL install a no-op `TracerProvider` and a no-op `LoggerProvider` so that no exporters, batchers, or background goroutines are created.

#### Scenario: Telemetry disabled by default

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_ENDPOINT` unset and no per-signal endpoint variable set
- **THEN** the global `TracerProvider` is `noop.NewTracerProvider()`, the slog OTLP bridge writes to a no-op logger, and no OTLP connection is opened

#### Scenario: Telemetry enabled by environment

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317`, `OTEL_SERVICE_NAME=kube-state-graph`, and `OTEL_RESOURCE_ATTRIBUTES=deployment.environment=prod`
- **THEN** the SDK installs an OTLP gRPC trace exporter and an OTLP gRPC log exporter targeting `otel-collector:4317`, and the resource carries `service.name=kube-state-graph` and `deployment.environment=prod`

#### Scenario: HTTP/protobuf protocol selection

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` and `OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.com:4318`
- **THEN** the SDK installs OTLP HTTP/protobuf exporters for traces and logs against the configured endpoint

#### Scenario: Sampler configuration

- **WHEN** the server starts with `OTEL_TRACES_SAMPLER=parentbased_traceidratio` and `OTEL_TRACES_SAMPLER_ARG=0.1`
- **THEN** the configured `TracerProvider` uses a parent-based trace-ID-ratio sampler with ratio `0.1`

### Requirement: HTTP request tracing via otelgin

The server SHALL install the `otelgin` middleware on every `/v1/*` and `/debug/*` route group so that each authenticated request produces an inbound server span whose name is the matched Gin route template (e.g. `GET /v1/graph`, `GET /v1/edge-types`).

The middleware SHALL extract the W3C `traceparent` and `tracestate` headers from inbound requests using the global propagator (`propagation.TraceContext{}` + `propagation.Baggage{}`) so that callers' trace context becomes the parent of the server span.

The middleware SHALL NOT be installed on `/livez`, `/readyz`, `/metrics`, `/openapi.yaml`, `/openapi.json`, or `/docs` so health probes and documentation requests do not generate spans.

Each request span SHALL carry semantic-convention HTTP attributes (`http.request.method`, `http.route`, `url.scheme`, `url.path`, `server.address`, `server.port`, `client.address`, `user_agent.original`, `http.response.status_code`).

When a handler returns a non-2xx status, the middleware SHALL set the span status to `Error` with the configured `build.Reason` string as the description, and SHALL NOT record the request body.

#### Scenario: Inbound request creates a server span

- **WHEN** a client sends `GET /v1/graph?start=...&end=...` with a valid API key and no inbound `traceparent`
- **THEN** the server emits one server span named `GET /v1/graph` with attributes including `http.request.method=GET`, `http.route=/v1/graph`, and `http.response.status_code=200`

#### Scenario: Inbound traceparent becomes the parent context

- **WHEN** a client sends `GET /v1/graph?...` with header `traceparent: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01`
- **THEN** the resulting server span's `trace_id` equals `0af7651916cd43dd8448eb211c80319c` and its parent span ID equals `b7ad6b7169203331`

#### Scenario: Health probes are not traced

- **WHEN** a Kubernetes kubelet sends `GET /livez` or `GET /readyz`
- **THEN** the otelgin middleware does not run, no span is exported for the request, and the response status is unchanged

#### Scenario: Metrics endpoint is not traced

- **WHEN** Prometheus scrapes `GET /metrics`
- **THEN** no span is exported for the scrape, and the response is the standard Prometheus exposition

#### Scenario: Build error sets span status

- **WHEN** a `/v1/graph` request fails with `build.Reason = "upstream_unavailable"` mapping to HTTP 502
- **THEN** the server span's status is set to `Error` with description `"upstream_unavailable"` and `http.response.status_code=502`

#### Scenario: Removed route is not a traced route template

- **WHEN** a client sends `GET /v1/clusters`
- **THEN** Gin matches no route, the request receives `404`, and no span named `GET /v1/clusters` is exported

### Requirement: Build pipeline span instrumentation

`Builder.Build(ctx, ...)` SHALL open a child span named `kube-state-graph.build` whose parent is the inbound HTTP server span. The build span SHALL carry attributes `kube_state_graph.window_seconds`, `kube_state_graph.end_unix`, and on successful completion `kube_state_graph.cluster_count`, `graph.node.count`, and `graph.edge.count`.

Each parallel PromQL query in the topology and service-graph errgroups SHALL run inside its own child span named `prometheus.query` with attributes `db.system=prometheus`, `db.statement=<rendered PromQL string>`, and `kube_state_graph.query_name=<one of: kube_pod_info | kube_node_info | kube_node_status_addresses | kube_pod_spec_volumes_persistentvolumeclaims_info | kube_node_labels | traces_service_graph_request_total | …>`. The PromQL HTTP client SHALL inject W3C `traceparent` headers into outbound VictoriaMetrics requests using the global propagator.

Projection and serialisation SHALL each run inside their own child spans named `kube-state-graph.project` and `kube-state-graph.serialise`, carrying the post-projection `graph.node.count` and `graph.edge.count` and the serialiser format (`cytoscape`) as `kube_state_graph.serialiser`.

When any PromQL query, projection, or serialisation step fails, the corresponding span SHALL record the error via `span.RecordError(err)` and set the span status to `Error`.

#### Scenario: Build span hierarchy

- **WHEN** a `/v1/graph` request completes successfully
- **THEN** the trace contains exactly one `GET /v1/graph` server span; one `kube-state-graph.build` child of it; multiple `prometheus.query` grandchildren (one per fan-out leg), each carrying `db.system=prometheus` and a non-empty `db.statement`; one `kube-state-graph.project` child of the server span emitted after build returns; and one `kube-state-graph.serialise` child emitted after projection

#### Scenario: PromQL query span carries query name and statement

- **WHEN** the topology reader fans out the `kube_pod_info` query
- **THEN** a span named `prometheus.query` is emitted with `kube_state_graph.query_name="kube_pod_info"`, `db.system="prometheus"`, and `db.statement` equal to the rendered PromQL string passed to VictoriaMetrics

#### Scenario: PromQL outbound traceparent propagation

- **WHEN** the topology reader issues a PromQL HTTP request to VictoriaMetrics from inside a `prometheus.query` span
- **THEN** the outbound request carries a `traceparent` header whose trace ID and parent span ID match the active `prometheus.query` span context

#### Scenario: Failed PromQL query records error

- **WHEN** VictoriaMetrics returns HTTP 500 to a PromQL query
- **THEN** the corresponding `prometheus.query` span records the error and its status is `Error`; the parent `kube-state-graph.build` span also records the propagated error and is `Error`

### Requirement: Structured logging via slog OTLP bridge with trace correlation

The server SHALL build its `*slog.Logger` using `go.opentelemetry.io/contrib/bridges/otelslog` so every `slog` record is emitted twice: once to the configured stderr text/JSON handler (preserving the existing operator log stream) and once to the OTLP logs pipeline through the global `LoggerProvider`.

Whenever a log record is emitted from a `context.Context` carrying an active span (via `slog.LogAttrs(ctx, …)` or equivalent context-aware call), the resulting OTLP log record SHALL carry `trace_id` and `span_id` fields matching that active span, and the local stderr handler output SHALL include `trace_id` and `span_id` keys with the same values.

When tracing is disabled (`OTEL_EXPORTER_OTLP_ENDPOINT` unset), the slog bridge SHALL write only to the local stderr handler and SHALL NOT emit OTLP log records.

The server SHALL never log the value of any presented `X-API-Key` or any per-key secret retrieved from `--api-keys-file`. This rule applies equally to the local handler and the OTLP bridge.

#### Scenario: Log record correlated with active span

- **WHEN** a request handler calls `slog.InfoContext(ctx, "served graph", "clusters", n)` from within a `GET /v1/graph` server span
- **THEN** the OTLP log record carries `trace_id` equal to the server-span trace ID and `span_id` equal to the server-span span ID, and the stderr line includes the same `trace_id` and `span_id` keys

#### Scenario: Log emitted without an active span

- **WHEN** the server logs `slog.Info("starting", "addr", listenAddr)` at startup before any request span exists
- **THEN** the OTLP log record (when telemetry is enabled) is emitted without `trace_id` / `span_id` fields, and the stderr line omits them

#### Scenario: API key never appears in logs

- **WHEN** authentication fails for a request bearing `X-API-Key: hunter2`
- **THEN** neither the stderr log line nor the OTLP log record contains the literal string `hunter2` or any prefix / suffix of it longer than zero characters

### Requirement: Resource attributes and service identity

The OpenTelemetry resource SHALL be assembled by merging, in this order: detected process / runtime / host attributes from `sdk.NewResource(...)` defaults, the value of `OTEL_RESOURCE_ATTRIBUTES`, and finally explicit overrides for `service.name` (from `OTEL_SERVICE_NAME`, defaulting to `kube-state-graph`), `service.version` (the build's compiled-in version string), and `service.instance.id` (a UUIDv4 generated once at process start).

#### Scenario: Default service identity

- **WHEN** the server starts with `OTEL_EXPORTER_OTLP_ENDPOINT` set and `OTEL_SERVICE_NAME` unset
- **THEN** the exported resource carries `service.name=kube-state-graph`, `service.version=<build version>`, and a non-empty `service.instance.id`

#### Scenario: User-supplied resource attributes

- **WHEN** the server starts with `OTEL_RESOURCE_ATTRIBUTES=cluster=prod-eu1,team=platform`
- **THEN** every exported span and log record carries resource attributes `cluster=prod-eu1` and `team=platform` in addition to the defaults

### Requirement: Graceful shutdown flushes pending exports

On `SIGTERM` or `SIGINT`, the server SHALL invoke `TracerProvider.Shutdown(ctx)` and `LoggerProvider.Shutdown(ctx)` after the HTTP server has stopped accepting new connections and after in-flight requests have drained. The shutdown context SHALL share the existing server-shutdown grace deadline; the exporter SHALL NOT extend the grace period.

When shutdown of either provider returns an error, the server SHALL log the error via the local stderr handler and exit with a non-zero status code.

#### Scenario: Successful flush on SIGTERM

- **WHEN** the server receives `SIGTERM` while telemetry is enabled and a recently completed request span is still buffered
- **THEN** the buffered span and its child spans are exported to the configured OTLP endpoint before the process exits, the process exit code is `0`, and the total shutdown duration does not exceed the configured shutdown grace period

#### Scenario: Shutdown timeout does not block exit

- **WHEN** the OTLP collector is unreachable on `SIGTERM` and the shutdown grace period elapses with exports still pending
- **THEN** the process exits with a non-zero status code, the local stderr handler logs an `otlp shutdown timed out` error, and the process does not exceed the grace period waiting for the collector

