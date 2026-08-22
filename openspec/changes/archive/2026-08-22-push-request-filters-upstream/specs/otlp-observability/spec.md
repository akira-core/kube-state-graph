## MODIFIED Requirements

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
