## Why

In a large estate `GET /v1/storage-graph` fails with an upstream 422 —
VictoriaMetrics' `the number of matching timeseries exceeds <N>; either narrow
down the search or increase -search.maxUniqueTimeseries` — on
`kube_pod_container_info`, and the whole Sankey is unavailable. The endpoint
reuses the `/v1/graph` topology fan-out verbatim, so it reads the highest-
cardinality kube-state-metrics family in the estate (pods × containers × image
variants × pod churn inside the window) as a REQUIRED leg, for a `containers`
attribute the storage view never renders. The same fan-out also reads every pod
in the estate although the storage view keeps only pods that mount a claim,
and reads the service / endpointslice families although the body may not
contain a service node. The limit is memory-derived and applies to the series a
query SELECTS before any aggregation, so only narrower matchers or fewer legs
move it — raising the flag only moves the wall.

## What Changes

1. **`kube_pod_container_info` degrades on a query error** instead of failing the
   build, joining `kube_replicaset_annotations` / `kube_job_annotations` as the
   third log-and-continue kube-state-metrics leg. `data.containers` is a
   presentation attribute; losing it is subtractive. Caller cancellation still
   fails the request. Applies to both endpoints.
2. **The storage build gets its own read plan.** `/v1/storage-graph` no longer
   issues the five families it cannot draw: `kube_pod_container_info`,
   `kube_service_info`, `kube_endpointslice_endpoints`,
   `kube_endpointslice_labels`, `kube_service_annotations`. Four of the five are
   output-preserving (the storage body carries no service node and the
   `service-selects-pod` indexes feed only the service-graph reader).
   **BREAKING** (storage-graph-api): pod nodes in a storage-graph body no longer
   carry `data.containers`. The `/v1/graph` fan-out is unchanged.
3. **The storage build reads workload by reference.** `kube_pod_info` and
   `kube_pod_owner` become a second wave scoped to the pod names the claim-
   binding family returned plus the request's `pod=` roots, chunked
   deterministically like the QoS workload read; an empty scope issues no pod
   query. Pods that mount no claim — the overwhelming majority — are never
   fetched for this endpoint. Roots therefore have to reach the build:
   **BREAKING** (Go API): `Builder.BuildStorage` / `Engine.BuildStorage` take a
   `graph.StorageRoots` parameter. The HTTP surface and
   `Engine.BuildStorageFromValues` are unchanged.
4. **Pod-only roots narrow the upstream read.** When a `/v1/storage-graph`
   request carries at least one `pod=<ns>/<name>` root and no other root kind
   and no explicit `?namespace=`, the parser derives the `namespace` selector
   from the roots so every namespaced kube-state-metrics, kubelet and `ALERTS`
   query carries it. Output-preserving: with pod roots only, every retained
   path lies in the roots' namespaces by construction.
5. **Result-cardinality observability.** A new self-metric
   `kube_state_graph_upstream_query_result_series{query}` (histogram of series
   returned per upstream query) plus the largest leg named on the existing
   `graph built` / `storage graph built` log lines, so an operator sees a leg
   approaching the upstream cap before it turns into a 422.

No new node type, edge type, request parameter, flag, or dependency. `queryDims`
is unchanged: both data-derived scopes (item 3) and the derived namespace
(item 4) compose with — never replace — the existing request matchers.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `cluster-topology-source`: **Topology series consumed** — the query-error
  axis paragraph gains `kube_pod_container_info` as a third degrading leg, and
  the fan-out-per-build wording is qualified by endpoint (items 1, 2).
- `storage-graph-api`:
  - **Attributes and compound groups carry over** — `containers` leaves the list
    of attributes a storage-graph pod carries (item 2, BREAKING).
  - ADDED **Storage build reads only what it draws** — the five families the
    storage build does not issue, and the two it issues scoped by reference
    with the empty-scope / chunking / root rules (items 2, 3).
  - ADDED **Pod-only roots narrow the upstream read** — the derived `namespace`
    selector and its preconditions (item 4).
- `graph-api`: **Self-metrics endpoint** — the metric list gains
  `kube_state_graph_upstream_query_result_series` (item 5).

## Impact

- `pkg/build/topology.go` — `ReadTopology` becomes a thin wrapper over a
  plan-taking reader; the storage plan skips five legs and routes two through a
  scoped second wave (`pkg/build/podscope.go`, mirroring `qosscope.go`);
  `kube_pod_container_info` moves from `fetch` to `fetchOptional`.
- `pkg/build/build.go` — `BuildStorage` takes `graph.StorageRoots`; both build
  logs name the largest leg.
- `pkg/promql` — `RenderScoped` (request matchers + one data-derived
  alternation) generalising `RenderQoSVolumeScoped`; `SeriesMetrics` optional
  upgrade interface recorded from `Client.Instant`.
- `pkg/kubegraph/parse.go` — `ParseStorageValues` derives the namespace
  selector from pod-only roots; `engine.go` threads roots into the build.
- `internal/observability` — new histogram + adapter.
- Tests: fan-out pins (37/43 for `/v1/graph`; 32/38 for storage with a
  non-empty pod scope, 30/36 with an empty one), scoped-render and chunking
  pins, parser pins, component fixtures for the storage goldens, integration
  storage suite.
- Docs: `docs/upstream-metrics.md` (fan-out diagram, query-error matrix, a
  storage fan-out section), `docs/BREAKING.md`, `docs/upstream-backend-routing.md`
  metric table, `README.md`, `README.zh-tw.md`, `CLAUDE.md`.
