## MODIFIED Requirements

### Requirement: Time-window passthrough

The server SHALL derive the upstream evaluation window from caller-supplied `start` and `end`, after enforcing `end > start`. When the end-alignment grid (`--end-align` / `KSG_END_ALIGN`, default `30s`) is non-zero, the server SHALL floor `end` to the largest multiple of the grid (measured from the Unix epoch) that is not after it, and SHALL shift `start` earlier by the same amount, so the window length `end - start` is unchanged; upstream PromQL is then evaluated with `<window> = end - start` at the aligned `end`. When the grid is `0`, `start` and `end` SHALL be passed through verbatim. Alignment SHALL run after validation, so a request valid before alignment stays valid and every validation error is reported against the caller's own values. Alignment SHALL be a pure function of `(start, end, grid)`. There is no window cap and no future-time guard; the response body SHALL NOT echo `start`, `end`, or any derived timestamp, aligned or not. Operators relying on bounded query cost SHALL configure upstream VictoriaMetrics search limits (e.g. `-search.maxQueryDuration`, `-search.maxPointsPerTimeseries`).

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

### Requirement: Deterministic response body

For identical input — same `(aligned window, filters, upstream-data)` — the server SHALL produce a byte-identical response body across rebuilds, whether each upstream result was fetched or served from the in-process query-result cache. The server SHALL NOT emit any HTTP cache validator (no `ETag`, no `Last-Modified`). The in-process cache operates on individual upstream query results (see the `upstream-query-cache` capability), never on built graphs or response bodies; within one aligned window and selector set the projection-level filters remain a pure function of the built graph.

The serialiser SHALL maintain determinism by sorting `view.Nodes` and `view.Edges`, sorting `Graph.ClusterNames()`, sorting `IPAddress` slices at construction, and keeping the response body shape fixed at `{apiVersion, clusters, elements}` for graph routes (no time-of-build or echo-of-input fields). Every rendered upstream selector SHALL be a pure function of the sorted, de-duplicated parameter values.

`GET /openapi.yaml`, `GET /openapi.json`, and `GET /docs` SHALL carry an explicit `Cache-Control` header. `GET /v1/graph` SHALL NOT emit a `Cache-Control` header.

#### Scenario: Body byte-identical across repeated requests

- **WHEN** a client sends two consecutive `GET /v1/graph` requests with identical query parameters and the upstream data has not changed between them
- **THEN** both response bodies are byte-identical, whether the second request's upstream results came from the cache or from an independent upstream fan-out

#### Scenario: Parameter order does not change the body

- **WHEN** a client sends `?az=b&az=a&namespace=y&namespace=x` and then `?namespace=x&namespace=y&az=a&az=b` for the same window and upstream data
- **THEN** both requests render identical upstream selectors and return byte-identical bodies
