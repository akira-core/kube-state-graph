# Spec Delta

## ADDED Requirements

### Requirement: Bounded request line

The server SHALL set `http.Server.MaxHeaderBytes` explicitly to 256 KiB, so the request line and headers of one request are bounded independently of the Go default. The bound SHALL admit the largest request every graph endpoint's parser and root caps accept at the default byte budget.

#### Scenario: The largest legal request is admitted

- **WHEN** a client sends a `/v1/storage-graph` request carrying 100 distinct 253-byte values for each of `cluster`, `namespace`, `az` and `env` and a root set that fills the root chunk cap at the default byte budget
- **THEN** the server reads the request and answers it with a status other than `431 Request Header Fields Too Large`

#### Scenario: An oversized request line is refused before parsing

- **WHEN** a client sends a request whose request line exceeds the bound
- **THEN** the server answers `431 Request Header Fields Too Large` and no handler runs

### Requirement: Build admission control

The server SHALL bound the number of `/v1/*` requests in flight with `--max-inflight-builds` / `KSG_MAX_INFLIGHT_BUILDS` (default `32`, `0` disables), applied after API-key authentication. A request arriving at the bound SHALL be answered immediately with `503 Service Unavailable`, `reason: "overloaded"` and `Retry-After: 1`, and SHALL issue no upstream query. Routes outside `/v1` SHALL never be subject to the bound.

#### Scenario: A request past the bound is shed

- **WHEN** the server runs with `--max-inflight-builds=2`, two graph requests are still building, and a third arrives
- **THEN** the third receives 503 with `reason: "overloaded"` and `Retry-After: 1` at once, and issues no upstream query
- **AND** once one of the two completes, the next request is admitted

#### Scenario: Authentication runs first

- **WHEN** API keys are configured and the bound is reached
- **THEN** a request without a valid `X-API-Key` still receives 401, and only an authenticated request can receive 503 `overloaded`

#### Scenario: Operational routes are never limited

- **WHEN** the bound is reached
- **THEN** `GET /livez`, `GET /readyz`, `GET /metrics`, `GET /docs`, `GET /openapi.json` and `GET /openapi.yaml` are served as if no graph request were in flight

#### Scenario: Zero disables the bound

- **WHEN** the server runs with `--max-inflight-builds=0`
- **THEN** no request is ever answered `overloaded`

#### Scenario: Admission self-metrics

- **WHEN** a client scrapes `GET /metrics`
- **THEN** the exposition includes `kube_state_graph_inflight_builds` (gauge, no labels: the `/v1/*` requests currently admitted) and `kube_state_graph_admission_rejected_total` (counter, no labels: requests answered `overloaded`)
- **AND** the gauge returns to its previous value when an admitted request finishes, also when its handler panics

### Requirement: Optional window cap

The server SHALL accept `--max-window` / `KSG_MAX_WINDOW` (a duration, default `0` = no cap; a negative value is a startup error). When it is non-zero, a request to `/v1/graph`, `/v1/storage-graph` or `/v1/k8s-graph` whose caller-supplied `end - start` exceeds it SHALL be rejected `400 Bad Request` with `reason: "invalid_range"`, after every other parameter check and before any upstream query. A window equal to the cap SHALL be accepted.

#### Scenario: A window past the cap

- **WHEN** the server runs with `--max-window=24h` and a client sends `/v1/graph` with a 25-hour window
- **THEN** the server returns 400 with `reason: "invalid_range"` and issues no upstream query

#### Scenario: Every graph endpoint applies it

- **WHEN** the same over-cap window is sent to `/v1/storage-graph` or `/v1/k8s-graph` with an otherwise valid request
- **THEN** each returns 400 with `reason: "invalid_range"`

#### Scenario: Other parameter errors win

- **WHEN** an over-cap window is sent to `/v1/storage-graph` with no root
- **THEN** the server returns 400 with `reason: "missing_root"`

#### Scenario: The cap reads the caller's window

- **WHEN** a window equal to the cap is sent
- **THEN** it is accepted, and end alignment, which preserves the window length, cannot push it over the cap

### Requirement: The build deadline bounds every stage of a graph request

The `--build-timeout` deadline SHALL bound the whole graph request — build, projection, serialisation and JSON encoding — and the server SHALL check the request's context before each of the last three. A passed deadline SHALL be answered `504` `timeout` (counted in `kube_state_graph_build_rejected_total{reason="timeout"}`), a disconnected client `499` `canceled`.

#### Scenario: A request that completes every stage

- **WHEN** a graph request finishes every stage before its deadline
- **THEN** its status and body are exactly those it had before this requirement

#### Scenario: A client that leaves during the build

- **WHEN** the client disconnects while the build is running and the build then returns a graph
- **THEN** the projection, serialisation and encoding are not run, and the request is logged with status 499

#### Scenario: A build that finishes at its deadline

- **WHEN** the build returns a graph after the `--build-timeout` deadline has passed
- **THEN** the projection is not run and the response is 504 with `reason: "timeout"`

### Requirement: Non-finite upstream values are dropped

A Harvest or kubelet sample whose value is NaN or ±Inf SHALL be skipped by its reader, as if the series were absent, exactly as the RED and alert readers already skip them, so a non-finite value can never reach the JSON encoder.

#### Scenario: A NaN usage sample

- **WHEN** `kubelet_volume_stats_used_bytes` for a claim is NaN and its capacity is finite
- **THEN** the response is 200, the claim's `data.usage` carries `capacity_bytes` and no `used_bytes`

#### Scenario: A non-finite sample does not shadow a finite one

- **WHEN** two series of one aggregate's `aggr_space_used` return NaN and `100`
- **THEN** `data.usage.used_bytes` is `100`, whatever order the series arrive in

### Requirement: Bounded request ID

The server SHALL accept an inbound `X-Request-ID` only when it is 1 to 128 bytes long and every byte is in `[A-Za-z0-9._:=+/@-]`. Any other value SHALL be replaced by a generated id. The accepted or generated id SHALL be the one echoed in the `X-Request-ID` response header and written to the access log.

#### Scenario: A well-formed id is kept

- **WHEN** a client sends `X-Request-ID: 3f1c-9a/b.7:x=y+z@1` (128 bytes or fewer)
- **THEN** the response header and the access log carry exactly that value

#### Scenario: An oversized or malformed id is replaced

- **WHEN** a client sends an `X-Request-ID` of 129 bytes, or one containing a space, a quote or a non-ASCII byte
- **THEN** the response header and the access log carry a generated id instead, and the inbound value appears in neither

### Requirement: Rejected raw values are echoed only when bounded

A 400 message naming a malformed `start`, `end` or `prune` value SHALL repeat the raw value only when it passes the selector-value checks (at most 253 bytes, valid UTF-8, no control character); otherwise the message SHALL name the parameter's expected form without the value. The `reason` codes are unchanged.

#### Scenario: A short malformed timestamp is echoed

- **WHEN** a client sends `start=yesterday`
- **THEN** the response is 400 with `reason: "invalid_start"` and the message `timestamp must be RFC 3339 or Unix seconds: "yesterday"`, exactly as before

#### Scenario: An oversized malformed value is not echoed

- **WHEN** a client sends a 300-byte `end` or `prune` value that does not parse
- **THEN** the response is 400 with `reason: "invalid_end"` or `"invalid_scope"` respectively, and the message does not contain the value

## MODIFIED Requirements

### Requirement: Time-window passthrough

The server SHALL derive the upstream evaluation window from the caller's `start` and `end` after enforcing `end > start`, evaluating upstream PromQL with `<window> = end - start` at the (possibly aligned) `end`. End alignment SHALL be a pure function of `(start, end, grid)` that runs after validation and preserves the window length, and the response body SHALL NOT echo `start`, `end`, or any derived timestamp, aligned or not.

#### Scenario: End-alignment grid configuration

- **WHEN** the end-alignment grid (`--end-align` / `KSG_END_ALIGN`, default `30s`) is non-zero
- **THEN** the server floors `end` to the largest multiple of the grid (measured from the Unix epoch) that is not after it
- **AND** shifts `start` earlier by the same amount, so the window length `end - start` is unchanged

#### Scenario: Zero grid passes timestamps through

- **WHEN** the grid is `0`
- **THEN** `start` and `end` are passed through verbatim

#### Scenario: Alignment runs after validation

- **WHEN** a request is valid before alignment
- **THEN** it stays valid after alignment, and every validation error is reported against the caller's own values

#### Scenario: No window cap or future-time guard

- **WHEN** the server runs with `--max-window` unset or `0` and a client sends an arbitrarily long window, or any client sends an `end` in the future
- **THEN** the server applies no window cap and no future-time guard
- **AND** operators relying on bounded query cost configure upstream VictoriaMetrics search limits (e.g. `-search.maxQueryDuration`, `-search.maxSamplesPerQuery`, `-search.maxPointsPerTimeseries`) as a deployment precondition, and may additionally set `--max-window` ("Optional window cap")

#### Scenario: Caller timestamps drive PromQL

- **WHEN** the server runs with `--end-align=0` and a client sends `GET /v1/graph?start=2026-05-02T12:04:17Z&end=2026-05-02T12:19:30Z`
- **THEN** the upstream PromQL is evaluated with `<window> = end - start` and `<end> = 2026-05-02T12:19:30Z`, and the response body contains only `apiVersion`, `clusters`, and `elements`

#### Scenario: End floored to the grid, window length kept

- **WHEN** the server runs with `--end-align=30s` and a client sends `GET /v1/graph?start=2026-05-02T12:04:17Z&end=2026-05-02T12:19:47Z`
- **THEN** the upstream PromQL is evaluated at `<end> = 2026-05-02T12:19:30Z` with `<window> = 15m30s`

#### Scenario: Requests within one grid step render identical queries

- **WHEN** the server runs with `--end-align=30s` and two clients send the same parameters except `end=12:19:31Z` and `end=12:19:59Z` with equal window lengths
- **THEN** both builds render byte-identical upstream queries evaluated at `12:19:30Z`, and with unchanged upstream data return byte-identical bodies

#### Scenario: Validation uses the caller's values

- **WHEN** the server runs with `--end-align=30s` and a client sends `start=12:19:40Z&end=12:19:50Z`
- **THEN** the request is valid (the caller's `end > start`), and is evaluated over a 10s window ending at `12:19:30Z`

### Requirement: Selector values are validated before rendering

Selector-level values and `prune` SHALL be validated before any upstream query is rendered, and an invalid value SHALL be rejected with `400 Bad Request` and `reason: "invalid_scope"`. Each of `cluster`, `namespace`, `az` and `env` SHALL carry at most 100 DISTINCT non-empty values (`promql.MaxAlternationValues`, the upstream's exact-lookup limit for one alternation) on every graph endpoint; a repeated identical value and an empty value do not count.

#### Scenario: Over-long value rejected

- **WHEN** a selector-level value is longer than 253 bytes
- **THEN** the server returns `400 Bad Request` with `reason: "invalid_scope"`

#### Scenario: Control character rejected

- **WHEN** a selector-level value contains a control character (including newline)
- **THEN** the server returns `400 Bad Request` with `reason: "invalid_scope"`

#### Scenario: Non-boolean prune rejected

- **WHEN** the `prune` value is not exactly `true` / `false`
- **THEN** the server returns `400 Bad Request` with `reason: "invalid_scope"`

#### Scenario: One hundred distinct values are accepted

- **WHEN** a client sends 100 distinct `namespace` values, or 101 values of which only 100 are distinct
- **THEN** the request is accepted

#### Scenario: More than one hundred distinct values are rejected before any query

- **WHEN** a client sends 101 distinct values of `cluster`, `namespace`, `az` or `env` to `/v1/graph`, `/v1/storage-graph` or `/v1/k8s-graph`
- **THEN** the server returns `400 Bad Request` with `reason: "invalid_scope"` and a message naming the parameter and the limit and repeating no value, and issues no upstream query

#### Scenario: Required parameters are checked first

- **WHEN** a `/v1/storage-graph` request carries 101 distinct `namespace` values and no `az`
- **THEN** the server returns 400 with `reason: "missing_az"`

### Requirement: Structured request logging

Every served HTTP request SHALL emit exactly one structured log line via `log/slog` JSON handler containing at least `method`, `path`, `status`, `duration_ms`, `request_id`, and applied `cluster` filter values. The `clusters` field SHALL carry at most the first 10 `cluster` values in request order, each cut to at most 64 bytes, and the line SHALL carry `clusters_truncated: true` when any value or count was cut.

#### Scenario: Request log line

- **WHEN** the server serves a request
- **THEN** stdout receives a JSON object with the listed fields and a top-level `level` field set to `INFO` for non-error responses

#### Scenario: Many cluster values are truncated in the log

- **WHEN** a request carries 50 `cluster` values, one of them 200 bytes long
- **THEN** the log line's `clusters` holds the first 10 values, none longer than 64 bytes, and `clusters_truncated` is `true`
