## Context

This repository delivers exactly one component: the **graph API server**. Everything else — a centralised VictoriaMetrics, the per-cluster scrape pipelines that feed it (`kube-state-metrics`, vmagent / Prometheus, the customised service-graph metrics source) — is treated as an external dependency and is only present in this repo as test scaffolding (the testcontainers-go integration suite under `internal/integration/`).

Data flow that the API server assumes is already in place:

```
cluster A: kube-state-metrics ──┐
           service-graph source ┤
                                 │  (vmagent / Prometheus
cluster B: kube-state-metrics ──┤   with external_labels:
           service-graph source ┤   { cluster: "<name>" })
                                 │
       ...                       ├──► centralised VictoriaMetrics ◄── Graph API server (this repo)
                                 │                                     (Prometheus HTTP API client)
cluster N: kube-state-metrics ──┤
           service-graph source ─┘
```

- Each cluster's scrape pipeline applies a `cluster=<name>` external label uniformly to `kube-state-metrics` and service-graph metrics before remote-writing into a single shared VictoriaMetrics.
- `kube-state-metrics` exports `kube_pod_info{cluster=...,uid=...}`, `kube_node_info{cluster=...,node=...}`, `kube_node_status_addresses{cluster=...}`, `kube_pod_spec_volumes_persistentvolumeclaims_info{cluster=...}`, `kube_node_labels{cluster=...}`, etc.
- A separate (out-of-repo) service-graph producer (typically Tempo's metrics-generator running per source cluster) emits pod-UID-labelled metrics carrying a single `cluster` external label representing the trace source cluster — the cluster originating the RPC. The remote (server-side) cluster is **not** stamped on the metric; cross-cluster status is recovered at build time by joining the server pod UID against the global topology pod-UID index:
  - `traces_service_graph_request_total{cluster, client_k8s_pod_uid, server_k8s_pod_uid, client_k8s_namespace_name, server_k8s_namespace_name, connection_type}`.
- The API server reads everything it needs from VictoriaMetrics through the Prometheus HTTP API, **on demand per request**, scoped to a caller-specified time range. It never talks to the Kubernetes API server in any cluster, never scrapes `kube-state-metrics` directly, and never connects to the service-graph producer.

The integration-test suite in this repo (`internal/integration/`, a `testcontainers-go` VictoriaMetrics container fed hand-crafted multi-cluster series via `POST /api/v1/import/prometheus`) exists only to give CI and developers a reproducible target. It deliberately does **not** stand up real per-cluster scrape pipelines — that work belongs to deployment, not to this repo.

Constraints on the API server:

- Go 1.25+ standard library `log/slog` for logging (toolchain pinned to `go1.26.2`).
- Gin for HTTP routing.
- `github.com/prometheus/client_golang/api` and `.../api/v1` for outbound queries.
- `golang.org/x/sync/errgroup` and `.../semaphore` for parallel fan-out and concurrency capping.
- No Kubernetes client-go, no informers, no direct VictoriaMetrics SDK.
- Single configurable upstream URL (the centralised VictoriaMetrics Prometheus-compatible endpoint).

## Goals / Non-Goals

**Goals:**
- Ship a Go (Gin) HTTP server that returns a unified nodes-and-edges JSON document for one or more Kubernetes clusters in a caller-specified time range `[start, end]`, computed from VictoriaMetrics on demand.
- Expose **cross-cluster** RPC edges (`pod-calls-pod` where the source and target pods resolve to different clusters via the global pod-UID index) as first-class graph elements.
- Build the graph by issuing PromQL queries with `@` timestamp modifiers and range-aware functions (`last_over_time`, `rate`) against centralised VictoriaMetrics, and joining the result sets in memory across all clusters in scope.
- Each request runs a fresh upstream fan-out — there is no in-process result cache and no singleflight. A horizontally scalable cache mechanism is deferred to a future iteration (see "Future cache mechanism" below).
- Use the `(cluster, pod-uid)` composite as the stable identity for pod nodes and the join key for pod-pod edges; node and PVC IDs are similarly cluster-scoped.
- Expose Cytoscape.js-shaped JSON as the response of `GET /v1/graph`.
- Provide cluster discovery (`GET /v1/clusters`) sourced live from VictoriaMetrics, plus a static edge-type catalogue (`GET /v1/edge-types`).
- Provide a container-integration test suite (`internal/integration/`, a testcontainers-go VictoriaMetrics container seeded with hand-crafted multi-cluster `kube_*` and `traces_service_graph_*` series) that proves the API server returns a non-empty, well-formed multi-cluster graph including a cross-cluster edge.

**Non-Goals:**
- Implementing the customised service-graph collector (Alloy / OTLP collector). The integration suite seeds the contract metrics directly into VictoriaMetrics.
- Operating, configuring, or hardening `kube-state-metrics` or VictoriaMetrics. They are dependencies, not deliverables.
- Talking to the Kubernetes API directly in any cluster. All cluster facts are read via metrics.
- Authorisation, multi-tenant isolation, or TLS termination on the HTTP API (assume reverse proxy handles it). Per-cluster RBAC is also out of scope — every reachable cluster is equally readable through this server. v1 ships static **API-key authentication** only (single shared secret tier, no per-caller scoping); see D24.
- Ingesting traces. Trace-derived metrics are produced upstream; the API server only reads the resulting metric series.
- Real-time streaming or WebSocket APIs.
- An in-process result cache. v1 deliberately ships **no** server-side build cache and **no** singleflight; every request runs a fresh upstream fan-out.
- A distributed / shared cache (Redis, memcached) or background materialiser. These are explicitly deferred — a future iteration will add a horizontally scalable cache mechanism for distributed deployment; the design space is captured under "Future cache mechanism".
- A graph database (Neo4j, Memgraph, ArangoDB) for partial / traversal queries. In-memory adjacency suffices for v1.
- VictoriaMetrics multi-tenant (vmcluster `accountID:projectID`) routing. Single-tenant centralised VM with `cluster` external labels is the v1 isolation model; multi-tenancy is a v1.1 escape hatch.
- Standing up real per-cluster scrape pipelines in the integration-test suite.

## Decisions

### D1. Single upstream: centralised VictoriaMetrics via Prometheus API

The server takes one upstream URL (`--prom-url`, default `http://localhost:8428`) pointing at the centralised VictoriaMetrics' Prometheus-compatible endpoint. All inputs (kube-state-metrics series and service-graph series, from any cluster) are queried from this single backend.

Multi-cluster discrimination is by **label**: every series carries `cluster=<name>` — for both topology (`kube_*`) and service-graph (`traces_service_graph_*`) metrics. Service-graph metrics carry only the trace-source cluster as `cluster`; the remote (server-side) cluster is recovered at build time by joining the server pod UID against the global topology pod-UID index. The API server never knows about per-cluster URLs.

- Why: matches the centralised-observability deployment topology these systems already use; collapses N readers into one client; lets one PromQL query cover all clusters in a single round-trip.
- Alternatives considered:
  - One upstream per cluster, fanned out by the API server (rejected — duplicates connection plumbing and breaks cross-cluster edge resolution, since an edge's two ends would land in two separate query results).
  - VictoriaMetrics multi-tenant (`accountID:projectID` per cluster) (rejected — requires vmcluster, heavier ops, and breaks single-PromQL cross-cluster edges; v1.1 escape hatch).
  - Direct Kubernetes API access via client-go informers (rejected — informers know only the *current* state of clusters they watch, cannot answer historical time-range queries, and re-introduce N watch streams plus per-cluster RBAC).

### D2. On-demand time-ranged build, no server-side snapshot, no result cache

Every request to `GET /v1/graph?start=...&end=...` triggers a fresh build of the multi-cluster graph for the supplied window:

1. Resolve and validate `start` / `end` (only `end > start`; D5).
2. Run all required PromQL queries against centralised VictoriaMetrics in parallel via `errgroup.WithContext` under a per-build `context.WithTimeout(ctx, --build-timeout)`. On `context.DeadlineExceeded`, abort and return `504 Gateway Timeout` with `reason: "timeout"`. Concurrency limiting is delegated to horizontal scaling (HPA) and Pod resource limits — there is no in-process semaphore.
3. Join the result sets across clusters in memory and produce the global multi-cluster `Graph`.
4. Apply filters (`cluster`, `namespace`, `edge_type`, `name`) and traversal pruning (`root`, `depth`, `direction`) over the freshly built `Graph` as a projection, then serialise to Cytoscape.js JSON and return.

There is no in-process result cache, no singleflight, no background `Snapshotter`, no `atomic.Pointer[Graph]`, no fixed refresh interval, and no `POST /admin/refresh`.

- Why: keeps the v1 implementation small and lets a future iteration choose a cache mechanism appropriate for distributed deployment (Redis, materialised-view tier, graph DB) without unwinding an in-process cache assumption first. The upstream cost remains O(requests) until that future iteration lands.
- Alternatives considered:
  - In-process Ristretto + singleflight (the previous design — moved out of v1; revisit when distributed deployment is on the table).
  - Periodic snapshot (rejected — incompatible with time-travel queries; staleness window forces a worst-case freshness penalty even when no caller needs it).
  - Background materialiser writing to a shared store (deferred — captured under "Future cache mechanism").

### D3. Pod, node, and PVC identity is cluster-scoped

`Graph` IDs:

- Pod nodes: `(cluster, pod-uid)`. Serialised IDs use the form `<cluster>/<pod-uid>`.
- K8s nodes: `(cluster, node-name)`. Serialised IDs use the form `<cluster>/<node-name>`.
- PVC nodes: `(cluster, namespace, pvc-name)`. Serialised IDs use the form `<cluster>/<namespace>/<pvc-name>`.

Edge endpoints reference these composite IDs.

- Why: pod UIDs are UUIDv4 and globally unique in practice, but mixing them with cluster names anyway is essentially free, makes IDs self-describing in logs and JSON, and avoids any contract relying on UUIDv4 collision avoidance. Node names and PVC names are *not* globally unique across clusters — node names like `worker-0` collide trivially — so cluster scoping is mandatory there.
- Alternatives considered:
  - Pod UID alone (rejected — works for pods but inconsistent with nodes/PVCs which require cluster scoping; mixing styles invites bugs).
  - `cluster.namespace.name` triple for pods (rejected — collides on pod restarts; service-graph metrics still reference the old UID for a window).

### D4. Edge types

Edges fall into typed categories:

- `pod-mounts-pvc` (intra-cluster only): derived from joining `kube_pod_spec_volumes_persistentvolumeclaims_info` with the node hosting the pod, within a single cluster.
- `pod-calls-pod` (intra-cluster **or cross-cluster**): from `rate(traces_service_graph_request_total[<window>]) @ <end>` with non-zero rate, when the target does NOT resolve to an in-cluster service node. The client side joins on `(cluster, client_k8s_pod_uid)`. The server side joins via the **global pod-UID index** built from topology — `server_k8s_pod_uid` alone resolves to a single pod across all loaded clusters, since K8s pod UIDs are unique cross-cluster in practice. The edge carries `labels.cluster` set to the client-side cluster (omitted when the client is an external endpoint); cross-cluster status is derived by comparing the resolved source-node `labels.cluster` and target-node `labels.cluster` on the consumer side (no boolean flag in `labels` per D9's strict-string rule).
- `pod-calls-service` (intra-cluster only): from the same `rate(traces_service_graph_request_total[...])` series when a `"://"` connection-string label resolves to an in-cluster service node (D29). The edge `type` is `pod-calls-service` instead of `pod-calls-pod`. Always intra-cluster. The edge carries `labels.cluster` set to the client-side cluster when the client is a pod; omitted when the client is non-pod (`external`).

Each edge carries `type`, `source`, `target`, plus type-specific `attrs` (see D9 for serialised JSON shape).

- Why: lets consumers filter by edge type and mirrors Tempo's `serviceGraph` shape conceptually; exposes cross-cluster traffic as a first-class concept rather than a secondary annotation.
- Alternative: untyped edges with a free-form attributes map (rejected — harder to validate and render).
- New edge types are additive only; existing `type` strings are never repurposed (see D14).

### D5. Time-range passthrough

`start` and `end` are mandatory query parameters in either RFC 3339 or Unix seconds form. The only server-side validation is `end > start`. Beyond that check, the timestamps are passed through to upstream PromQL verbatim (`<window> = end - start`, `<end>` is the caller-supplied `end`). There is no server-side bucketing, alignment, grid, max-window cap, or future-time guard.

- Why no window cap: bounded query cost is delegated to upstream VictoriaMetrics search limits (`-search.maxQueryDuration`, `-search.maxPointsPerTimeseries`, `-search.maxSamplesPerQuery`). Duplicating these in KSG adds a configuration knob with weak business value and a confusing layered failure mode.
- Why no skew guard: NTP drift is a deployment concern; future-time queries against PromQL return empty results, which the caller sees as an empty graph. A dedicated KSG-side check provided marginal diagnostic value at the cost of a config knob.
- Why pass-through: with no cache, alignment provides no hit-rate benefit; `last_over_time` / `rate` lookbacks span minutes, so sub-second `@end` drift is not load-bearing for upstream evaluation. Removing alignment also removes a Go package, a `Window` struct in handler signatures, and several rounds of doc/spec coordination.
- Bounded query cost is delegated entirely to upstream VictoriaMetrics search limits.
- Alternatives considered:
  - 60 s `floor`/`ceil` grid (removed in this change — was originally a cache-key bucket; no longer needed without an in-process cache).
  - Per-class TTL ladder (deferred — would only matter once a server-side cache returns; revisit in the future cache mechanism).

### D6. Response shape and deterministic body; no in-process result cache

v1 ships **no** in-process result cache, **no** singleflight, and **no** `/admin/cache` route. Each request runs a fresh upstream fan-out and recomputes the response body. There is no server-side build cache, and the server does not emit any HTTP cache validator (no `ETag`, no `Last-Modified`) — caching is intentionally a future-iteration concern (see "Future cache mechanism" below).

**Deterministic body.** The serialiser must produce byte-identical output for the same `(window, filters, upstream-data)`. This is load-bearing for golden tests and for any downstream consumer that diffs responses. The serialiser guarantees determinism by:

- Sorting `view.Nodes` and `view.Edges` (`graph.SortNodes` / `graph.SortEdges`) before encoding.
- Sorting `Graph.ClusterNames()`.
- Relying on `encoding/json` map-key sorting for `labels map[string]string`.
- Sorting / deduping `IPAddress` slices at construction (today they are emitted in a single-element form; the slice contract is "stable but not semantically ranked").
- Keeping body shape fixed at `{apiVersion, clusters, elements}` for graph routes. No time-varying or echo-of-input fields are serialised; observability moves to logs/metrics.

**Response cache headers.** Routes whose content is stable and long-lived emit explicit `Cache-Control` (`/v1/edge-types`: 3600 s, `/openapi.{yaml,json}`: 3600 s, `/docs/assets/*`: 86400 s, `/docs`: 300 s) so that browsers and reverse proxies may cache them client-side. `/v1/graph` and `/v1/clusters` emit no `Cache-Control` — every freshly built body is treated as authoritative.

**No singleflight.** Concurrent identical requests each run their own upstream fan-out. At dev / pre-distributed-deployment traffic this is acceptable. Cluster-wide deduplication is part of the future cache mechanism, not v1.

**Future cache mechanism.** Out of scope for v1 but explicitly anticipated. The likely shape (subject to a separate change) is one of:

- **Background materialiser + shared store** — a leader-elected worker pulls VM on a fixed cadence, writes the graph to a shared store (Redis Cluster, graph DB, or columnar archive). API replicas become stateless readers with pushdown filtering. Suits 1 M+ nodes / 10 M+ edges; bounds memory per replica.
- **Per-replica L1 + shared L2 (Redis)** — Ristretto reappears in front of a network-shared encoded `*Graph`. Cheaper to add than a materialiser, but does not solve heap pressure at million-node scale.
- **Graph DB as the materialised store** — only justifiable once in-memory `*Graph` ceases to fit; trades pointer-walk traversal for indexed Cypher with disk-backed working set.

Whichever shape ships will need to revisit D5 (time-class TTL ladder), D11 (cache-key hashing), D12 (cache metrics), and D14 (cache contract). v1 deliberately leaves these holes empty rather than committing to an implementation that may not match the chosen distributed shape.

### D7. Filtering, cluster scoping, and partial-graph traversal

`GET /v1/graph` accepts (in addition to mandatory `start` / `end`):

- `?cluster=<name>` — repeatable; restricts the response to nodes whose `cluster` is in the set. Cross-cluster edges with one end inside the set and one end outside are **kept** (the remote endpoint resolves correctly because the freshly built `*Graph` holds all clusters loaded for the window); the remote endpoint node is also kept (with its own `labels.cluster`). Cross-cluster status is conveyed by comparing the source-node and target-node `labels.cluster` — consumers derive the boolean from the two strings (the edge itself carries only `labels.cluster` = trace-source / client-side cluster). Setting `cluster` to an unknown value is not an error — it simply yields an empty result for that name.
- `?namespace=<ns>` — repeatable; restricts pod / PVC nodes whose `namespace` is in the set. A namespace value matches across clusters; combine with `?cluster=` to scope to a single cluster's namespace. K8s `node` nodes carry no `namespace` label and no edge linking them to a namespaced entity, so a namespace-narrowed view drops them entirely — it contains only namespaced entities.
- `?edge_type=<type>` — repeatable; restricts to those edge types only. If a requested type has no edges in the current `Graph`, that type is silently skipped (no error, just empty).
- `?name=<value>` — repeatable; matches `n.Name()` by exact equality across **every** node type (`PodNode`, `K8sNode`, `PVCNode`, `ExternalNode`). Use it to anchor the view on any single node — a pod, a host node, a PVC, or an external endpoint — without the caller having to encode the type. Names are not globally unique (a pod and a K8s node can share a name; a PVC name can repeat across namespaces); all matches are returned. Combine with `?cluster=` / `?namespace=` to disambiguate. A `name` match on a pod does **not** pull in the pod's host K8s node — there is no edge linking them (the pod→node relationship is compound nesting via `labels.node` only); to render the host node, anchor on it directly.
- `?root=<id>&depth=<n>&direction=in|out|both` — partial-graph traversal: BFS from the given composite ID (`<cluster>/<pod-uid>` or `<cluster>/<node-name>`), bounded by `depth` (default 2, max 6). BFS follows edges only; a pod root does **not** reach its host K8s node (no edge links them — the pod→node relationship is compound nesting via `labels.node` only). A K8s `node` is an edgeless graph node, so anchoring a root on it yields only the node itself (plus the node's own depth-0 entry).

Filtering is applied **at response time over the freshly built `*Graph` value**, not by re-querying upstream. PromQL queries always fetch the full window across every cluster present in upstream VictoriaMetrics; the in-memory `*Graph` is the shared base from which all filtered views are projected.

- Why: filter+serialise is microseconds for typical v1 graph sizes (≤ 5 k pods × ≤ 10 clusters); pushing filters to PromQL would re-evaluate per filter combination at upstream cost. When the future cache mechanism lands, the same projection-over-graph contract preserves filter shareability across cache entries.
- Empty filter ⇒ full multi-cluster graph for the time range.
- Filters compose with AND across types and OR within a type.
- Traversal first prunes by `root`/`depth`/`direction`, then `cluster` / `namespace` / `edge_type` / `name` filters apply over the traversal result.
- Edge re-add rule (unified across all filters): an edge survives when at least one resolved endpoint is in scope after node filtering. When exactly one endpoint is in scope, the missing endpoint is re-added from `g.NodesByID` provided it passes the non-cluster filters (namespace). This covers cross-cluster `pod-calls-pod` edges where only `cluster` narrows scope (the partner pod lives in an out-of-scope cluster), `pod-mounts-pvc` edges incident on an in-scope pod, and name-anchored views that need to render incident edges of the named node together with their partners. There is no name-specific suppression: anchoring on a named node intentionally surfaces the edges that connect it to its neighbourhood, otherwise the rendered graph would have dangling edge endpoints.
- The `pod_uid` filter parameter was considered and rejected: pod UIDs are opaque internal identifiers callers cannot obtain without first making a `/v1/graph` call. Callers scope by `cluster` + `name` instead, accepting that names are not globally unique.
- Alternatives considered:
  - PromQL label-selector narrowing per request (rejected — VictoriaMetrics scans the index regardless; label selectors trim the network payload but not upstream evaluation cost. The full multi-cluster graph is small enough at v1 scale to build once and project per request).
  - A graph database for traversal queries (rejected — operationally heavy for a workload in-memory adjacency handles in microseconds, see D16).

### D8. Producer contract and integration-test producer

The API server depends on a **metric contract**, not on any specific producer. Contract:

```
# Topology (per cluster)
kube_pod_info{cluster, namespace, pod, uid, node, ...}
kube_node_info{cluster, node, ...}
kube_node_status_addresses{cluster, node, type="ExternalIP", address=...}
kube_pod_spec_volumes_persistentvolumeclaims_info{cluster, namespace, pod, volume, claim_name, ...}
kube_node_labels{cluster, node, label_*=...}

# Service graph (single source cluster per series; cross-cluster recovered at build time via UID index)
traces_service_graph_request_total{
  client, server,
  cluster,                        # single trace-source cluster (client side)
  client_k8s_pod_uid, server_k8s_pod_uid,
  client_k8s_namespace_name, server_k8s_namespace_name,
  connection_type="virtual_node|messaging_system|database"
}
traces_service_graph_request_failed_total{ ...same labels... }
traces_service_graph_request_server_seconds_bucket{ ...same labels..., le="..." }
```

The `cluster` external label is applied by each cluster's scrape pipeline (`vmagent` / Prometheus `external_labels`) — for both `kube-state-metrics` series and service-graph series. Service-graph metrics are produced per source cluster by Tempo's metrics-generator (or an equivalent `servicegraph` connector); the producer only knows the cluster it runs in and stamps that as `cluster`. The remote (server-side) cluster is **not** stamped — recovery of cross-cluster targets happens in the API server by joining `server_k8s_pod_uid` against the global topology pod-UID index. The producer-side instrumentation requirement reduces to: emit `cluster` (typically already done as an external label) and pod-UID dimensions on each side.

**Integration-test fixture ingestion — direct exposition format:**

Integration tests in `internal/integration/` use [`testcontainers-go`](https://golang.testcontainers.org/) to start a real VictoriaMetrics container per suite, then push hand-crafted multi-cluster series via VictoriaMetrics' `POST /api/v1/import/prometheus` endpoint (Prometheus text exposition format). No separate fixture binary, no YAML, no `/metrics` endpoint, no SIGHUP reload — the test itself owns the series content and timestamps. Each test seeds:

- `kube_pod_info` / `kube_node_info` / `kube_node_status_addresses` series for several synthetic clusters (e.g., `cluster-alpha`, `cluster-beta`).
- `traces_service_graph_request_total` series including at least one **cross-cluster** edge: a series with `cluster="cluster-alpha"` whose `server_k8s_pod_uid` matches a pod whose `kube_pod_info` entry lives in `cluster-beta`, so the test asserts cross-cluster handling via UID-index resolution.

Service-graph counters are ingested as two monotonic samples (`t0` and `t1 = t0 + 60s`) so `rate(...[w]) @ t1` recovers a non-zero per-second rate. Tests use a fixed-time anchor (`fixedNow = 2026-05-01T12:00:00Z`) to keep time-bucket alignment deterministic — see D20.

- Why direct ingestion: the API server is the unit under test. Synthesising the metric contract directly in Go keeps tests focused on join / build / HTTP behaviour, makes multi-cluster scenarios trivial (just emit different `cluster` values), and avoids dragging in collector + tracing dependencies, fixture programs, YAML schemas, and reload protocols.
- Tests MUST emit the exact label set the production contract specifies, so swapping in real producers in deployment is a configuration change, not a code change.
- The integration suite (`internal/integration/`) is the sole verification path for both the topology and the service-graph code paths: it seeds `kube_*` and `traces_service_graph_*` series directly into the VictoriaMetrics container, so multi-cluster / cross-cluster scenarios are exercised end-to-end against real PromQL.

**Rejected: standalone fixtures binary (`cmd/vm-fixtures/`) + YAML config** — earlier sketch; superseded by direct in-test exposition ingestion. The binary added build complexity, deployment surface, and a YAML schema for no test-discrimination benefit. Tests can author exact series inline in Go.
**Rejected: real per-cluster `kube-state-metrics` scrape pipelines** — heavy setup cost, exhausts laptop resources, and validates the same metric contract that direct ingestion already covers.
**Rejected: synthetic OTLP trace generator + collector** — full pipeline exists in production but is upstream of this server; doubles the integration-test surface for no benefit.
**Rejected: `telemetrygen`** — emits standalone spans without parent/child propagation, so the `servicegraph` connector cannot pair them and no edge metrics result.
**Rejected: OpenTelemetry Demo (`otel-demo`)** — boots ~15 services and a heavy chart; too much for a per-PR integration test.

### D9. Output format: Cytoscape.js JSON

**Node and edge schema (canonical):**

| Object | Field | Type | Source / Notes |
|---|---|---|---|
| Node | `id` | string | Cluster-scoped composite. Pods: `<cluster>/<pod-uid>`. Nodes: `<cluster>/<node-name>`. PVCs: `<cluster>/<namespace>/<claim>`. **Services** (resolved from a `://` connection string, D29): `<cluster>/<namespace>/<service>`. **External** (unresolvable `://` connection string or missing-UID non-URL label): `external/<label-value>` (no cluster). |
| Node | `name` | string | Pod name / node name / PVC claim name. For external nodes, the verbatim `client` or `server` label value (e.g., `http://api.example.com`). Used as the display label in the rendered graph. |
| Node | `type` | string | One of `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`. |
| Node | `ipaddress` | `[]string` (`omitempty`) | Observed IP addresses for the node. Present on `type="pod"` (`kube_pod_info.pod_ip`), `type="node"` (K8s node `ExternalIP` from `kube_node_status_addresses`), and `type="service"` (`kube_service_info.cluster_ip`, omitted when `cluster_ip="None"` for headless services). Omitted for `type="pvc"`, `type="external"`, and omitted on pod / node / service entries when the source metric does not surface an IP. The slice order is stable but not semantically ranked. |
| Node | `labels` | `map[string]string` | String-only key/value bag. Pod / node / PVC nodes always include `cluster`, `namespace` (pods/PVCs), `node` (pods, cluster-scoped node ID). K8s pod / node labels are flattened in verbatim. IP addresses are **not** carried in `labels` — see the `ipaddress` row above. **Service nodes** (D29) carry `cluster` and `namespace`. **External nodes** carry an empty `labels` map (`{}`) — no `cluster`. New keys are additive. |
| Edge | `id` | string | UUIDv5 derived from a fixed namespace UUID and the canonical tuple `(type, source, target)`. Stable across builds for the same edge; format compliant with RFC 4122. |
| Edge | `type` | string | One of the registered edge types in `/v1/edge-types` (e.g., `"pod-mounts-pvc"`, `"pod-calls-pod"`, `"pod-calls-service"`, `"service-selects-pod"`). |
| Edge | `source` | string | Source node `id`. Always references a node present in the same response. |
| Edge | `target` | string | Target node `id`. Always references a node present in the same response. |
| Edge | `labels` | `map[string]string` | String-only key/value bag. For `pod-calls-pod` and `pod-calls-service`: `cluster` (the trace source cluster, i.e. the client-side pod's cluster — omitted when the client is a non-pod endpoint, i.e. `service` or `external`). For `pod-mounts-pvc`: `claim_name`, `storage_class`. For `service-selects-pod` (D29): optional `namespace`, none required. New keys are additive. |

**Strictly string-typed values.** `labels` is `map[string]string` for both nodes and edges. Non-string-typed data (numeric edge metrics such as `rate`, `p99_ms`, `error_rate`; boolean flags such as `cross_cluster` or `ghost`) is **deferred to a future typed struct field** on node/edge data. v1 does not encode booleans as `"true"`/`"false"` strings inside `labels`; consumers derive cross-cluster status for `pod-calls-pod` edges by comparing the edge's resolved source-node `labels.cluster` with the target-node `labels.cluster` (both nodes are guaranteed present in the same response).

The primary `GET /v1/graph` response is **Cytoscape.js**-shaped JSON:

```json
{
  "apiVersion": "v1",
  "clusters": ["cluster-alpha", "cluster-beta"],
  "elements": {
    "nodes": [
      { "data": { "id": "cluster-alpha/abc-123",
                  "name": "checkout-7c9d-x2p4",
                  "type": "pod",
                  "labels": { "cluster": "cluster-alpha", "namespace": "shop",
                              "node": "cluster-alpha/worker-0",
                              "app": "checkout", "version": "1.4.2" } } },
      { "data": { "id": "cluster-alpha/worker-0",
                  "name": "worker-0",
                  "type": "node",
                  "labels": { "cluster": "cluster-alpha",
                              "external_ip": "203.0.113.10",
                              "kubernetes.io/arch": "amd64",
                              "topology.kubernetes.io/zone": "us-east-1a" } } }
    ],
    "edges": [
      { "data": { "id": "5e7b8d6a-2c1f-5b3a-9b14-3a3f0a9e2c11",
                  "type": "pod-calls-pod",
                  "source": "cluster-alpha/abc-123",
                  "target": "cluster-beta/def-456",
                  "labels": { "cluster": "cluster-alpha" } } }
    ]
  }
}
```

- Why: a single canonical schema (`id`, `name`, `type`, `labels`) drives the serialiser; any future field addition lives in `labels` and is therefore non-breaking.
- Why UUIDv5 for edge `id`: deterministic (golden tests stay stable; same edge → same ID across rebuilds), RFC 4122 compliant, and decoupled from the human-readable `(source, target, type)` triple so renaming convention later does not change IDs already exposed.
- Alternatives considered:
  - `kind`/`label`/`attrs` field names (rejected — divergent from user-requested schema).
  - Random UUIDv4 for edges (rejected — breaks golden-test determinism; same edge would get a different ID every build).
  - GraphQL (rejected — adds dependency surface for v1 with no clear caller).

### D10. Logging via `log/slog`, JSON handler

`slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: ...}))` set as default logger; level configurable. Every HTTP request emits one structured log line with method, path, status, duration, request ID, and applied `cluster` filter values.

One additional log line per build: `slog.Info("graph built", "duration_ms", ..., "clusters", ..., "nodes", ..., "edges", ..., "cross_cluster_edges", ..., "queries", ..., "failures", ..., "start", ..., "end", ...)`.

- Why: required by the user; standard library only.
- Alternative: zap / zerolog (rejected — extra dependency, not requested).

### D11. Implementation tactics

These are mandatory for the v1 implementation:

- **Sealed graph node types**: Go interface `GraphNode` with concrete `PodNode`, `NodeNode`, `PVCNode`. Each implementation surfaces the canonical fields (`ID()`, `Name()`, `Type()`, `Labels()`) consumed by the serialisers in D9. The `cluster` value lives inside `Labels()["cluster"]` rather than as a separate first-class field on the wire.
- **Pure join layer**: `Build(topology Topology, edges []ServiceGraphEdge) *Graph` is a pure function over typed Go structs and produces the full, unfiltered multi-cluster graph for the time window. All HTTP- and Prometheus-free unit tests target this function. Cross-cluster edges are produced when the resolved source-pod cluster differs from the resolved target-pod cluster (server-side cluster comes from the topology pod-UID index lookup, not from a metric label).
- **Pure projection layer**: `Project(g *Graph, scope Scope) GraphView` applies cluster / namespace / node / edge_type filters and traversal over an immutable `*Graph` and returns a read-only view. No allocations of new node/edge structs, just slices of pointers.
- **Query registry**: PromQL strings as named constants in one file, parameterised on `<window>` and `<end>`. Paired with a parser that maps Prometheus `model.Vector` results into typed Go structs.
- **One PromQL instant query per metric family**, evaluated at the bucketed `end` with `last_over_time` / `rate` over the window. Queries do **not** include filter-derived selectors. Parse Vector client-side.
- **Parallel upstream fan-out** via `errgroup.WithContext`. Wall-clock latency = O(slowest query), not O(sum of queries).
- **Per-build context timeout**: the graph endpoint (`/v1/graph`) wraps the build in `context.WithTimeout(ctx, --build-timeout)` (default `15s`). On `context.DeadlineExceeded`, the build is aborted, the failure counter increments, and the request returns `504 Gateway Timeout` with `reason: "timeout"`.
- **Per-request timeout for non-graph endpoints**: `/v1/clusters` (live discovery query) and `/readyz` (`up{}` probe) wrap their upstream call in `context.WithTimeout(ctx, --api-timeout)` (default `5s`). On `context.DeadlineExceeded`, the request returns `504 Gateway Timeout` with `reason: "timeout"`. Endpoints with no upstream call (`/v1/edge-types`, `/livez`, `/metrics`, `/openapi.*`, `/docs*`) are not subject to this timeout.
- **No in-process concurrency cap.** Concurrency limiting is delegated to horizontal scaling (HPA) and Pod resource limits. The previous `golang.org/x/sync/semaphore`-based per-instance cap and the `503 capacity` error reason were removed: HPA reacts to CPU / latency signals at instance granularity and is the operator's primary lever; an in-process semaphore added a config knob whose tuning required the same load data HPA already uses, while making per-instance load shedding less observable than queue-time-based metrics.
- **Adjacency maps**: forward and reverse `map[NodeID][]*Edge` built once inside `Build()`; reused for traversal pruning during `Project()`. Built on the immutable `*Graph` so concurrent projections within the same request share them safely.

### D12. Self-metrics and operability

The server exposes its own `/metrics` endpoint (Prometheus exposition) with at least:

- `kube_state_graph_build_duration_seconds` (histogram — wall-clock build time per request).
- `kube_state_graph_project_duration_seconds` (histogram — filter + traversal pruning).
- `kube_state_graph_serialise_duration_seconds{format}` (histogram — JSON encode time).
- `kube_state_graph_build_rejected_total{reason="timeout"}` (counter).
- `kube_state_graph_graph_node_count{cluster,kind}` (gauge — last build only, observational; bounded by configured cluster count).
- `kube_state_graph_graph_edge_count{type,cross_cluster}` (gauge — `cross_cluster` ∈ `{"true","false"}`).
- `kube_state_graph_clusters_observed` (gauge — unique `cluster` values seen in the last build).
- `kube_state_graph_upstream_query_duration_seconds{query}` (histogram).
- `kube_state_graph_upstream_query_failures_total{query}` (counter).
- `kube_state_graph_http_requests_total{path,status}` (counter).

Health endpoints:

- `GET /livez` — always 200 while the process is up.
- `GET /readyz` — 200 iff a cheap upstream probe (`up{}` instant query, wrapped in `context.WithTimeout(ctx, --api-timeout)`) succeeds. `503 Service Unavailable` if the probe fails (semantically: not ready to serve traffic — the standard Kubernetes liveness/readiness convention).

Operator endpoints: none in v1. Diagnostics rely on `kube_state_graph_*` metrics and structured request logs.

### D13. Testing layers

The test stack has five layers, all CI-runnable; each MUST exist before this change is archived:

| Layer | CI? | Scope | Tool |
|------|-----|------|------|
| Unit | yes | Pure join / parse / project functions on hand-crafted multi-cluster `model.Vector` and `model.Matrix` inputs (intra-cluster, cross-cluster, and mixed) | `go test` |
| Component | yes | Build pipeline end-to-end against an `httptest.Server` mocking the Prometheus query API; covers per-build timeout, parameter validation, and serialiser output | `go test` |
| Golden | yes | Canned scenarios (single-cluster, two-cluster with cross-cluster edge, three-cluster with traversal pruning) → `/v1/graph`, `/v1/clusters`, `/v1/edge-types` JSON compared to checked-in `.golden.json` | `go test` |
| Property | yes | Random topology + edge inputs across N synthetic clusters + random filters → invariants (no orphan edges, no duplicate IDs, every endpoint resolves, filtered ⊆ unfiltered, traversal stays within `depth`, cross-cluster edges have distinct cluster endpoints) | `testing/quick` or `gopter` |
| **Container integration** (capability `container-integration`) | yes | Per-package VictoriaMetrics container started via testcontainers-go; series injected via VM's `/api/v1/import/prometheus`; in-process API server pointed at the container; assertions over real PromQL evaluation | `go test` + Docker |

All five layers run on every PR via `go test ./...` — see D20 for testcontainer rationale and D21 for static analysis / vulnerability scanning policy.

- Why: integration alone leaves logic regressions undetectable in PR feedback; mock-only component tests miss real PromQL semantics. The split puts every behavioural assertion in the CI path against real PromQL — the unit / component / golden / property layers for fast, dependency-free feedback, and the container-integration layer for real PromQL evaluation and deterministic time semantics.

### D14. Versioning

- All HTTP routes are prefixed `/v1/`. v2 can coexist on the same binary if the JSON shape ever breaks.
- The body carries `apiVersion: "v1"` so off-the-wire consumers can detect breaks.
- New edge types and new `attrs` fields are additive only; removed fields are a v2 break.
- `connection_type` values from the producer contract are mapped to a stable internal enum so a producer-side rename does not propagate into the API contract.
- `cluster` label values pass through as opaque strings; renaming a cluster upstream is a caller-visible change, not an API break.

### D15. Edge-type discovery API

`GET /v1/edge-types` returns the static catalogue of edge types this server can produce. No upstream calls; not parameterised by time range, cluster, or filters.

```json
{
  "apiVersion": "v1",
  "edge_types": [
    {
      "type": "pod-mounts-pvc",
      "description": "Pod mounts a PVC bound on the pod's host node. Always intra-cluster.",
      "source_type": "pod",
      "target_type": "pvc",
      "directed": true,
      "may_cross_cluster": false,
      "labels": [
        { "name": "claim_name",    "value_type": "string" },
        { "name": "storage_class", "value_type": "string" }
      ]
    },
    {
      "type": "pod-calls-pod",
      "description": "Pod-UID-resolved RPC edge from service-graph metrics where the target is a pod or an external node. May cross clusters when the resolved source and target pods live in different clusters (recovered from the topology pod-UID index since the metric only carries the trace-source cluster). Endpoints may be 'external' nodes when a '://' label does not resolve to an in-cluster service (D29) or via the missing-UID human-label fallback (D27).",
      "source_type": ["pod", "external"],
      "target_type": ["pod", "external"],
      "directed": true,
      "may_cross_cluster": true,
      "labels": [
        { "name": "cluster", "value_type": "string" }
      ]
    },
    {
      "type": "pod-calls-service",
      "description": "RPC edge from service-graph metrics where the target resolves to an in-cluster service node via connection-string resolution (D29). The '://' label is parsed, the host is matched to a Kubernetes service, and this edge type is emitted instead of pod-calls-pod. Always intra-cluster. Source may be a pod or external node; target is always a service node.",
      "source_type": ["pod", "external"],
      "target_type": "service",
      "directed": true,
      "may_cross_cluster": false,
      "labels": [
        { "name": "cluster", "value_type": "string" }
      ]
    },
    {
      "type": "service-selects-pod",
      "description": "Service node selects a backing pod, materialised on demand when a '://' connection string resolves to an in-cluster Service (D29). Joined from kube_endpointslice_endpoints to topology pods. Always intra-cluster.",
      "source_type": "service",
      "target_type": "pod",
      "directed": true,
      "may_cross_cluster": false,
      "labels": [
        { "name": "namespace", "value_type": "string" }
      ]
    }
  ]
}
```

- Source: a single in-code registry shared with the graph builder. Adding a new edge type updates both atomically.
- Caching: response carries `Cache-Control: public, max-age=3600`.
- Behaviour with `/v1/graph?edge_type=`: callers may pass any subset of `type` values from this endpoint. If a requested type has no edges in the current `Graph`, the response simply contains zero edges of that type — no error, no warning.

### D16. No graph database, no client-go informer for v1

Both options were considered and rejected for v1:

- **Graph DB (Neo4j / Memgraph / ArangoDB):** ~1 GB memory baseline, ops burden (backups, upgrades, auth, monitoring), sync complexity, ms-scale query latency. Traversal queries described in D7 are answered in microseconds by in-memory adjacency at v1 scale.
- **client-go informer for topology:** informers expose only the *current* cluster state and cannot answer historical time-range queries — the API's contract. Multi-cluster makes this worse: would need N watch streams plus per-cluster RBAC.

**Revisit triggers** (any of these promotes one to v1.1+):

- Total cluster size > 100 k objects across all clusters in scope.
- Time-travel queries beyond TSDB retention window become required.
- Cross-region cluster federation with isolation requirements (consider VictoriaMetrics multi-tenant routing instead).
- Free-form Cypher / Gremlin from operators.
- Sub-second freshness for "live" windows becomes a UI requirement.

### D17. Multi-cluster routing, discovery, and cross-cluster edges

**Routing.** The graph endpoint accepts multi-cluster scope as a query parameter, not a path segment:

- `GET /v1/graph?start=...&end=...&cluster=<name>&cluster=<name>...`

`cluster` is repeatable; absent ⇒ all clusters in the freshly built graph. Path-based per-cluster URLs were considered and rejected: cross-cluster edges naturally span more than one cluster, so a single-cluster path implies a scope smaller than the data — leading either to lossy responses (drop cross-cluster edges) or surprising responses (include endpoints outside the path). Query-param multi-select avoids this entirely.

**Discovery.** `GET /v1/clusters` returns the list of clusters that have data in centralised VictoriaMetrics, derived live from `group by (cluster) (last_over_time(kube_node_info[1h]))`. The lookback is fixed at `1h` (sufficient to absorb transient KSM scrape gaps; not configurable). Each request hits VictoriaMetrics directly — there is no in-process discovery cache in v1.

**Cross-cluster edges.** `pod-calls-pod` edges where the resolved source and target pods live in different clusters are emitted as ordinary edges with both endpoint nodes present in the freshly built graph (since each build holds the global multi-cluster graph). When a request scopes to a subset of clusters, cross-cluster edges that touch the selected set are kept along with both endpoint nodes — the remote node's `labels.cluster` makes the cross-cluster context obvious to renderers. The edge carries `labels.cluster` set to the trace-source cluster (i.e. the client-side pod's cluster); consumers detect cross-cluster status by comparing the source-node and target-node `labels.cluster` (a boolean shortcut field is deferred to the future typed struct described in D9).

**Cluster name handling.** Cluster names pass through as opaque strings. The server does no canonicalisation, no case-folding, and no length validation beyond the total URL length the HTTP stack already enforces. An unknown cluster name in `?cluster=` simply yields no nodes for that name — not an error.

### D19. Bounded upstream cost

Bounded query cost (per-query duration, samples, points) is delegated entirely to upstream VictoriaMetrics search limits — KSG does not duplicate them. Operators running large fleets SHALL configure `-search.maxQueryDuration`, `-search.maxPointsPerTimeseries`, and `-search.maxSamplesPerQuery` on VM and rely on `502 Bad Gateway` (with `reason: "upstream"`, mapped from VM 5xx) for overflow signalling. Per-cluster scope narrowing is a caller-side concern via the `?cluster=` query parameter on `/v1/graph`; the server itself loads every cluster present in upstream VM on each build.

### D20. Container integration via testcontainers-go

The CI integration layer uses **testcontainers-go** (`github.com/testcontainers/testcontainers-go`) to spin up a real VictoriaMetrics container from inside `go test`. Tests run in `internal/integration/`.

Architecture:

```
go test ./internal/integration/
  │
  ├─ TestMain (per package)
  │     └─ start vmsingle container (image pinned, e.g. victoriametrics/victoria-metrics:v1.107.0)
  │
  ├─ test helper: ingest(t, exposition string)
  │     POST <vm.URL>/api/v1/import/prometheus
  │
  └─ each test:
        ├─ ingest synthetic kube_* + traces_service_graph_* exposition with absolute timestamps
        ├─ wait for VM to acknowledge data (poll up{} or count(kube_pod_info) until non-empty, ≤ 10 s)
        ├─ start the API server in-process: srv := api.New(cfg, ...).Handler() + httptest.NewServer
        └─ exercise /v1/* endpoints, assert HTTP shape / headers / body content
```

Decisions:

- **One container per package**, not per test: bootstrapping VM costs ~5–10 s, far more than each test. Tests inside a package use unique series-label discriminators (e.g., a `test=<TestName>` label) so they never collide.
- **Direct injection via `/api/v1/import/prometheus`**, not a scrape stub: keeps the test process self-contained (no second container, no scrape interval to tune), and the API server only sees series in VM regardless of how they got there. The Prometheus exposition format is hand-written by the test and supports per-sample timestamps.
- **In-process API server** (`api.New(...).Handler()` against `httptest.NewServer`): no third container; tests can introspect server state, share types, and avoid Docker round-trips. The production container image (`deploy/docker/server.Dockerfile`) is built and published separately, not exercised by this layer.
- **Absolute timestamps in fixtures**: tests use fixed timestamps (e.g., `time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)`) and pass the same window to the API. This makes time-window alignment fully deterministic — a class of bugs the httptest mock layer cannot expose.
- **VM image is pinned** by tag in test code; no `:latest`. Image is pre-pulled in CI to remove first-run noise.
- **Docker socket required**: the integration-test job runs on `ubuntu-latest` GitHub Actions runners (Docker socket native). macOS / Windows runners are out of scope for this layer.
- **Body determinism**: the integration suite asserts that two consecutive `/v1/graph` requests for identical inputs return byte-identical response bodies, since v1 has no result cache and any non-determinism in the build/serialise path would surface here.

What the container suite deliberately does **not** cover (out of scope for this repo, left to deployment):

- Kubernetes Service / Deployment / ConfigMap wiring.
- Real scrape pipeline (vmagent → VM).

- Why a container layer: it gives us real PromQL evaluation, deterministic time semantics, and parallel-safe per-package tests against a real VictoriaMetrics, which a mock-only layer cannot.
- Alternatives considered:
  - Run the API server as a third container (rejected — no benefit over in-process for the assertions this layer makes; the production container image is built and published separately).
  - Inject series via a scrape stub container (rejected — adds a container, a scrape interval, and a startup race for no behavioural benefit since the API only ever reads from VM).

### D21. Static analysis and vulnerability scanning

Two CI gates beyond `go test`:

**1. `golangci-lint` — curated linter set.** A repository-level `.golangci.yml` enables the following linters (alphabetical):

- Correctness: `errcheck`, `gosimple`, `govet`, `ineffassign`, `staticcheck`, `unused`, `gocritic`, `exhaustive`.
- Modern Go idioms: `copyloopvar`, `intrange`, `revive`.
- Error handling: `errorlint`, `nilerr`.
- Security: `gosec`.
- Complexity: `gocyclo`, `gocognit`, `funlen`.
- Performance: `prealloc`, `bodyclose`, `unconvert`.
- Style: `misspell`, `gofmt`, `goimports`.
- Dead code / duplication: `dupl`, `unparam`.
- Magic numbers: `mnd`.

Complexity caps:
- `gocyclo`: cyclomatic complexity ≤ 15 per function.
- `gocognit`: cognitive complexity ≤ 20 per function.
- `funlen`: ≤ 100 lines / ≤ 50 statements per function.

Test files are exempted from `errcheck` and the strictest complexity / duplication rules (table-driven tests legitimately repeat structure).

**2. `govulncheck` — dependency vulnerability scanner.** A separate CI step runs `golang.org/x/vuln/cmd/govulncheck@latest ./...` on every PR. Detected vulnerabilities MUST be either fixed (dependency bump) or triaged with an explicit suppression comment + linked tracking issue before merge.

CI integration sketch (workflow snippet):

```yaml
jobs:
  lint:
    steps:
      - uses: golangci/golangci-lint-action@v8
        with: { args: --timeout=5m }
  vuln:
    steps:
      - run: |
          go install golang.org/x/vuln/cmd/govulncheck@latest
          govulncheck ./...
  test:
    steps:
      - run: go test ./... -count=1 -race -shuffle=on
```

`lint`, `vuln`, and `test` run as parallel jobs (no `needs` edges) so PR feedback latency = max, not sum.

- Why: linter set covers the trending Go quality dimensions (complexity, security, error handling, modern idioms) without enabling everything (`golangci-lint` ships ~100 linters; many overlap or fight each other). The set above is intentionally curated to maximise signal and minimise false positives.
- Why `govulncheck` separately from golangci-lint: govulncheck reads call-graph reachability, not just static patterns, and is the official Go security-team tool. Keeping it as its own CI job makes failure mode obvious in the GitHub UI.
- Alternatives considered:
  - Enable every golangci-lint linter (rejected — high false-positive rate, lint fatigue, churn from linter authors).
  - Use `gosec` only without `golangci-lint` (rejected — narrower coverage, redundant tooling).
  - Run `govulncheck` only on tagged releases (rejected — a vulnerable dependency lands the day it's introduced, not on release; per-PR is the only useful cadence).

### D22. OpenAPI generation and offline-capable Scalar UI

**Generation: `swaggo/swag` v2.** Handler functions in `internal/api/handlers.go` carry annotation comments (`// @Summary`, `// @Description`, `// @Tags`, `// @Param`, `// @Success`, `// @Failure`, `// @Router`, `// @Header`); the `cmd/kube-state-graph/main.go` entry point carries the document-level annotations (`// @title`, `// @version v1`, `// @license.name Apache 2.0`, `// @BasePath /v1`). Running `swag init -g cmd/kube-state-graph/main.go --output docs --parseDependency --parseInternal` regenerates `docs/swagger.json`, `docs/swagger.yaml`, and `docs/docs.go`. Generated files are checked in.

**OpenAPI version: 3.0.** Stable in swag v2; swag v2's 3.1 mode is still maturing and 3.0 covers everything we need (`additionalProperties: { type: string }` for the strict `map[string]string` `labels` invariant from D9, error-envelope component, etc.). Bump to 3.1 once swag v2's 3.1 path has shipped GA.

**Routes for spec + UI**:

| Route | What | Cache |
|------|------|------|
| `GET /openapi.yaml` | Generated YAML, served from embedded `docs/swagger.yaml` | `Cache-Control: public, max-age=3600` |
| `GET /openapi.json` | Generated JSON, served from embedded `docs/swagger.json` | same |
| `GET /docs` | HTML page that renders the spec via the Scalar API Reference viewer | `Cache-Control: public, max-age=300` |
| `GET /docs/assets/*` | Static Scalar JS / CSS bundle, served from embedded assets | `Cache-Control: public, max-age=86400, immutable` |

**Scalar UI is vendored in the binary**, not loaded from a CDN. The Scalar `@scalar/api-reference` standalone bundle (currently ~600 KB minified+gzipped) is checked in under `internal/api/static/scalar/` and embedded via `embed.FS`. The HTML at `/docs` references `/docs/assets/scalar.js` (relative path), so the page renders correctly behind reverse proxies, in air-gapped clusters, on isolated VPNs, and on developer laptops without internet — no exception cases.

The vendored bundle version is pinned (e.g., `@scalar/api-reference@1.x.y`) and refreshed via a `make refresh-docs-ui` script that re-downloads the pinned version, validates the SHA-256, and updates the embedded files. The script's expected SHA is committed alongside.

**Drift gate**: a `make check-docs` target re-runs `swag init` and exits non-zero if the working tree changes. The same step runs in CI; PRs that touch `internal/api/*.go` without regenerating `docs/swagger.{json,yaml,go}` fail.

**Route ↔ spec contract test** (Go-side): the test parses `docs/swagger.json` via `kin-openapi`, walks Gin's `engine.Routes()` after `Server.Handler()`, and asserts bidirectional `(method, path)` set-equality modulo a small allowlist for infrastructure paths (`/livez`, `/readyz`, `/metrics`, `/openapi.yaml`, `/openapi.json`, `/docs/*`). Any divergence — handler added without an annotation, annotation pointing at a removed route — fails the test.

- Why swag v2: lowest churn for an existing Gin codebase. Annotations live next to the handlers that implement the documented behaviour. Generated artefacts double as input to the drift gate and to the contract test.
- Why Scalar over Swagger UI: better default UX, smaller payload, native dark-mode, modern aesthetic. Drop-in replacement — both consume the same OpenAPI 3.0 spec.
- Why vendoring the UI bundle: deployment topology assumes restricted-network environments (Kubernetes operators, internal tools). A `/docs` route that requires reaching `cdn.jsdelivr.net` is silently broken in those environments. Vendoring guarantees the route works wherever the binary runs.
- Alternatives considered:
  - Hand-maintained `docs/openapi.yaml` (rejected — drift risk; swag v2 reduces effort while preserving control via annotations).
  - Huma (rejected — full framework refactor, see D20-style trade-off).
  - CDN-loaded UI (rejected — air-gap-incompatible).
  - Swagger UI bundled instead of Scalar (rejected — heavier bundle, dated UX; spec consumers still get the same JSON/YAML).

### D24. API-key authentication (header `X-API-Key`)

**Header.** Callers present a single key in `X-API-Key: <key>`. `Authorization: Bearer` was considered and rejected: `X-API-Key` is unambiguous (no scheme parsing), simpler for ops to set on Grafana datasources and `curl`, and avoids implying OAuth-style scope.

**Key sources.** Two flags, file takes precedence:

- `--api-keys-file <path>` / `KSG_API_KEYS_FILE` — one key per line, blank lines and `#` comments tolerated. Designed for Kubernetes `Secret` volume mounts. Re-read every `--api-keys-reload-interval` (default `30s`, `0` disables) so a `kubectl apply` on the Secret rotates keys without a Pod restart.
- `--api-keys` / `KSG_API_KEYS` — comma-separated literal. Dev / one-shot use only.

When neither is set the keyset is **empty** and the middleware is a no-op (auth disabled). The server logs a warning at boot in that case so the operator notices an unintended dev posture in production.

**Protected vs open routes.**

| Open (no key) | Protected (`X-API-Key` required) |
|---|---|
| `/livez`, `/readyz` (kubelet probes) | `/v1/graph`, `/v1/clusters`, `/v1/edge-types` |
| `/metrics` (Prometheus scrape; gate via NetworkPolicy or a separate listen address in production) | |
| `/openapi.yaml`, `/openapi.json`, `/docs`, `/docs/assets/*` (UI must load) | |

**Validation.** `crypto/subtle.ConstantTimeCompare` per stored key, with a same-length filler comparison for stored keys whose length differs from the presented value. The full key set is iterated on every call so neither match latency nor early exit leaks the key count or the matching position.

**Reload semantics.** File reload is implemented via an `atomic.Pointer` swap on the underlying slice. In-flight requests use whichever pointer they captured; no locking. Combined latency for a Kubernetes `Secret` rotation is `kubelet sync (~60s)` + `--api-keys-reload-interval (30s default)` ≈ ~90s worst case.

**Failure mode.** Missing or invalid key → `401 Unauthorized` with `{"error":{"reason":"unauthorized","message":"…"}}`. The middleware also increments `kube_state_graph_auth_rejected_total{reason="missing|invalid"}`.

**Docs.** OpenAPI 3.0 declares `securitySchemes.ApiKeyAuth` (`in: header`, `name: X-API-Key`); every protected handler carries `@Security ApiKeyAuth` + `@Failure 401`. The Scalar UI surfaces an "Authentication" control so callers can paste a key and try requests live.

- Why static keys (not JWT / OIDC): the operator's expected deployment posture is "behind a reverse proxy with caller-side auth" plus a coarse server-side gate. Static keys cover the gate without dragging in an OIDC stack. Per-caller scoping is a follow-up if real deployments need it.
- Why no `/admin/keys` API: keys live in the K8s `Secret`; the rotation procedure is a `kubectl apply`, not an HTTP call. No code path can leak keys via the API.
- Logging: never log the presented key value. Logs include `auth=ok|disabled|denied` only.
- Alternatives considered:
  - `Authorization: Bearer` (rejected — X-API-Key chosen for simplicity, see Header note).
  - mTLS (deferred — operationally heavier; reverse proxy is the recommended TLS layer).
  - OAuth2 / OIDC (deferred — too heavy for v1's gate-only posture).

### D23. Test framework: testify across the repository

**Adoption scope**: every test file under `internal/`, `tests/`, and `cmd/` uses `github.com/stretchr/testify/{assert,require,suite}`. No test file mixes stdlib `t.Errorf` / `t.Fatal` patterns with testify in the same suite (one style per file).

**Migration cadence**: a single dedicated PR converts all 57 existing tests in one pass. Mechanical refactor; no behaviour changes. Smaller PRs deferred — the larger one-shot diff is easier to review than ten micro-PRs each with the same shape.

**Patterns**:

- **`require`** for "if this fails, the rest of the test is meaningless" — fixture setup, container start, JSON unmarshal of the response under test.
- **`assert`** for individual checks within a test — encourages the test to surface multiple failures per run.
- **`suite.Suite`** for the testcontainers integration package only. `SetupSuite` starts the VictoriaMetrics container; `TearDownSuite` stops it; `SetupTest` resets fixtures; tests are methods on the suite struct. Stdlib unit / golden / property tests stay function-shaped.
- **`assert.JSONEq`** for wire-shape comparisons in golden tests so byte-for-byte diff isn't required.
- **`assert.Eventually`** for the VM-readiness poll in integration tests.

**`testifylint`** is added to the curated `golangci-lint` set (D21) and configured with `enable-all: true`. It catches the common testify misuses (`assert.True(t, a == b)` → `assert.Equal`, missing `t.Helper()` in helpers, `assert` calls inside goroutines without `t.Cleanup`, etc.).

- Why testify across the whole repo, not just integration: a single style is easier for contributors to read and grep. The mass-migration cost is one focused PR; the long-tail cost of mixed styles is forever.
- Why one PR rather than per-package: the diff is mechanical, the cognitive load of reviewing it is the same across packages; bundling reduces churn windows where new code lands in the old style and needs re-translation.
- Alternatives considered:
  - Stay on stdlib (rejected — integration tests with testcontainers benefit materially from `suite.Suite` and `require`; the rest of the repo is along for consistency).
  - Adopt `gotest.tools/v3` instead (rejected — testify is the dominant Go ecosystem choice and what `testifylint` polices; staying with the stream).

### D25. OpenTelemetry tracing and logging

**Why now**: D10 (`log/slog`, JSON handler) covers operator logs but ships no distributed-tracing surface. Once KSG sits behind an Alloy / OTel Collector and a centralised VictoriaMetrics, operators need per-request spans (which cluster, which PromQL leg was slow?) and trace-correlated logs (a single `trace_id` joining HTTP access, build pipeline, PromQL fan-out, projection, serialisation). The same OTel pipeline that already collects `traces_service_graph_*` from the workloads is the natural sink for KSG's own self-traces and self-logs.

**Stack**:

- `go.opentelemetry.io/otel` SDK + `sdktrace` + `sdklog`.
- `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` + `otlptracehttp` (HTTP/protobuf).
- `go.opentelemetry.io/otel/exporters/otlp/otlplogs/otlploggrpc` + `otlploghttp`.
- `go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin` for inbound HTTP spans.
- `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` for the outbound PromQL client transport.
- `go.opentelemetry.io/contrib/bridges/otelslog` for the slog → OTLP-logs bridge.
- `go.opentelemetry.io/otel/semconv/v1.27.0` for HTTP / RPC / DB attribute keys.

**Configuration surface**: OTel-standard environment variables only. No new CLI flags. No new `KSG_*` variables. Reading list:

- `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_INSECURE` and their per-signal `_TRACES_` / `_LOGS_` variants.
- `OTEL_SERVICE_NAME` (default `kube-state-graph`), `OTEL_RESOURCE_ATTRIBUTES`.
- `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG`.

The SDK's stock env-var loaders are used (`otlptracegrpc.WithEnv`-style configs) rather than re-implementing parsing. When `OTEL_EXPORTER_OTLP_ENDPOINT` and both per-signal endpoint variables are unset the binary installs `noop.NewTracerProvider()` and a no-op slog handler bridge. This is the **default**; v1 deployments without an OTel collector incur zero export overhead and zero new background goroutines.

**Init sequence** (in `cmd/kube-state-graph/main.go`):

1. Parse flags.
2. Build resource: `resource.New(ctx, resource.WithFromEnv(), resource.WithProcess(), resource.WithHost(), resource.WithTelemetrySDK(), resource.WithAttributes(serviceName, serviceVersion, serviceInstanceID))`.
3. If endpoint configured → build OTLP trace exporter, batch span processor, sampler from env, install global `TracerProvider`. Else → install `noop.NewTracerProvider()`.
4. Same for logs (OTLP log exporter + batch log processor + global `LoggerProvider`, or no-op).
5. Set global propagator: `propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})`.
6. Build `*slog.Logger` with `slog.New(slogmulti.Fanout(stderrHandler, otelslog.NewHandler("kube-state-graph")))` (or equivalent multi-handler). Without OTLP enabled, the otelslog handler short-circuits via the global no-op `LoggerProvider`.
7. Defer `tracerProvider.Shutdown(shutdownCtx)` and `loggerProvider.Shutdown(shutdownCtx)` after the existing HTTP-server graceful shutdown so in-flight exports flush within the existing grace deadline.

**Span topology**:

```
GET /v1/graph                               (otelgin server span)
└─ kube-state-graph.build                   (Builder.Build)
   ├─ prometheus.query (kube_pod_info)      (errgroup leg 1)
   ├─ prometheus.query (kube_node_info)     (errgroup leg 2)
   ├─ prometheus.query (kube_node_status_addresses)
   ├─ prometheus.query (kube_pod_spec_volumes_persistentvolumeclaims_info)
   ├─ prometheus.query (kube_node_labels)
   └─ prometheus.query (traces_service_graph_request_total)
└─ kube-state-graph.project                 (filter / cluster scope / traversal)
└─ kube-state-graph.serialise               (Cytoscape)
```

`prometheus.query` spans are siblings under `kube-state-graph.build` (driven by `errgroup`); they all carry `db.system=prometheus`, `db.statement=<rendered PromQL>`, `kube_state_graph.query_name=<one of the constants>`, and `server.address` / `server.port` derived from `--prom-url`. The PromQL HTTP client is wrapped with `otelhttp.NewTransport(...)` so the `traceparent` header is injected automatically and an additional client-side HTTP span is recorded per upstream call.

**Attribute set**:

| Span | Required attributes |
|------|---------------------|
| Server (otelgin) | `http.request.method`, `http.route`, `url.scheme`, `url.path`, `server.address`, `server.port`, `client.address`, `user_agent.original`, `http.response.status_code` |
| `kube-state-graph.build` | `kube_state_graph.window_seconds`, `kube_state_graph.end_unix`, on success `kube_state_graph.cluster_count`, `graph.node.count`, `graph.edge.count` |
| `prometheus.query` | `db.system=prometheus`, `db.statement`, `kube_state_graph.query_name`, `server.address`, `server.port` |
| `kube-state-graph.project` | `graph.node.count`, `graph.edge.count` (post-filter), `kube_state_graph.filter.cluster`, `kube_state_graph.filter.namespace`, `kube_state_graph.filter.edge_type` |
| `kube-state-graph.serialise` | `kube_state_graph.serialiser` (`cytoscape`), `graph.node.count`, `graph.edge.count` |

`db.statement` carries the raw PromQL — operators with strict policy on logging query strings can opt out by setting `OTEL_TRACES_SAMPLER=always_off` (kills tracing globally) or by stripping the attribute at the Collector via a processor. Document the trade-off; do not redact in-binary because the readable PromQL is the highest-value debugging signal in a slow-build trace.

**Error recording**: any `error` returned to the build pipeline calls `span.RecordError(err)` and `span.SetStatus(codes.Error, reason.String())`, where `reason` is the existing `build.Reason`. The error-mapping helper in `internal/api/errors.go` (`mapBuildError`) is the single place this is wired so HTTP status, response `reason`, log line, and span status stay in lockstep.

**slog bridge**: `otelslog.NewHandler("kube-state-graph")` wraps the existing JSON / text handler in a multi-handler. A logger obtained from `slog.New(...)` is stashed on `*gin.Context` via the otelgin middleware so handlers call `slog.LogAttrs(ctx, ...)` (or the package-level `slog.InfoContext(ctx, ...)`) and receive `trace_id` / `span_id` automatically — both in the local stderr line and in the OTLP log record. The local handler is configured with `HandlerOptions{ ReplaceAttr: ... }` so the `trace.SpanContextFromContext` IDs appear in stderr output even when the otelslog bridge is no-op (i.e. tracing disabled but a span context exists in tests).

**Non-traced routes**: `otelgin` is installed on the `/v1/*` and `/debug/*` route groups only. `/livez`, `/readyz`, `/metrics`, `/openapi.yaml`, `/openapi.json`, `/docs`, `/docs/assets/*` are mounted on a separate router group without the middleware. Rationale: kubelet probes hit `/livez` once a second per Pod; a single 50-replica deployment would emit 50 spans/s of pure noise. `/metrics` similarly produces one span per scrape per Prometheus replica. Documentation routes are served without auth and are not interesting in a trace.

**Sampling**: default sampler is `parentbased_alwayson` (the OTel SDK default). Operators control rate via `OTEL_TRACES_SAMPLER=parentbased_traceidratio` + `OTEL_TRACES_SAMPLER_ARG=0.05` etc. Because v1 has no in-process result cache and `/v1/graph` is the cost-dominant endpoint, head-based ratio sampling is sufficient; tail sampling is delegated to the Collector if needed.

**Secrets handling**: API-key validation in `auth.KeySet.Validate` MUST NOT log or attribute the presented key. Specifically: the otelgin middleware is configured with `otelgin.WithFilter(...)` to suppress `Authorization` and `X-API-Key` headers from the auto-attribute set, and the auth middleware's slog calls do not include the key value. This rule is enforced by an integration test that fails if any exported span attribute or log record contains the literal sentinel test key.

**Shutdown semantics**: the existing graceful shutdown sequence is:

```
1. SIGTERM received
2. http.Server.Shutdown(ctx with grace deadline)  — drains in-flight requests
3. tracerProvider.Shutdown(same ctx)
4. loggerProvider.Shutdown(same ctx)
5. exit
```

If `tracerProvider.Shutdown` or `loggerProvider.Shutdown` returns context-deadline-exceeded, the local stderr handler logs `otlp shutdown timed out` and the process exits with a non-zero status. The exporter SHALL NOT extend the grace period — operators rely on K8s `terminationGracePeriodSeconds` matching `--shutdown-grace-period`.

### D26. Configurable upstream metric-name prefix

Some deployments expose kube-state-metrics-shaped series under an organisational prefix — a fork of kube-state-metrics, a custom collector that re-publishes the same series, or a multi-tenant pipeline that namespaces metrics per tenant (`o11y_kube_pod_info`, `acme_kube_node_info`, etc.). To support those without forking KSG, the API server prepends a single configurable prefix to every kube-state-metrics-shaped series name it queries.

**Configuration**: `KSG_METRIC_PREFIX` env var or `--metric-prefix` flag (flag wins). Default is empty string — bit-identical to current behaviour. Validated against the Prometheus metric-name charset `^[a-zA-Z_:][a-zA-Z0-9_:]*$` when non-empty; invalid value fails startup. The trailing underscore is the operator's responsibility (`KSG_METRIC_PREFIX=o11y_` not `o11y`); the server does not inject one, so callers can produce both `o11y_kube_pod_info` and `o11ykube_pod_info` shapes if a real exporter genuinely needs the latter.

**Scope**: applied to every `kube_*` series the topology reader consumes — `kube_pod_info`, `kube_node_info`, `kube_node_status_addresses`, `kube_pod_spec_volumes_persistentvolumeclaims_info`, `kube_node_labels`, plus the three service-resolution series added by D29 (`kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`) — plus the `kube_node_info`-backed cluster-discovery query. NOT applied to `traces_service_graph_request_total` (Alloy/Tempo-family — different exporter, will not share a kube-state-metrics prefix) or to the Prometheus-native `up{}` readiness probe.

**Label-name contract**: only the metric *name* is prefixed. The label set the reader expects (`cluster`, `namespace`, `pod`, `uid`, `node`, `pod_ip`, `host_ip`, `persistentvolumeclaim`, `claim_name`, `volume`, `label_*`, `address`, `type`, plus the D29 service-resolution labels `service`, `cluster_ip`, `endpointslice`, `hostname`, `targetref_kind`, `targetref_name`, `targetref_namespace`, `label_kubernetes_io_service_name`) is the kube-state-metrics contract and is NOT configurable. A custom exporter that wants to be consumed by KSG MUST mimic both the metric-name suffix (`kube_pod_info` etc., after stripping the prefix) AND the label set.

**Implementation**: `promql.Renderer{Prefix string}` with a `Render(q Query, window) string` method. `build.Builder` and `api.Server` each hold a `Renderer` constructed from `config.Config.MetricPrefix` at startup; the package-level `promql.Render` is preserved as a zero-prefix convenience for tests that do not care about the prefix axis. Threading the renderer through constructors (rather than re-reading `Config.MetricPrefix` at every callsite) localises the wiring and keeps the existing `promql.Render(q, window)` signature available for pure unit tests.

**Why a single prefix instead of per-metric overrides**:

- Real custom exporters that re-publish KSM-shaped series consistently use one organisational prefix; per-metric naming divergence is uncommon enough to defer.
- Single env var keeps the operator workflow simple and the Secret-mount surface flat.
- If a real exporter ships with mixed names (e.g. `myorg_pods` + `myorg_pvcs`), a follow-up change can promote `Renderer` to a per-query name map without breaking the v1 contract — additive expansion.

**Why prefix is additive (not full rename)**:

- The 80 % case is "wrap stock series under an org prefix". Forcing operators to spell out the full name for every metric (`--metric-pod-info=o11y_kube_pod_info`) is verbose and error-prone for the common case.
- The label-name contract is unchanged regardless, so a prefix-only knob accurately matches the supported deformation.

**Alternatives considered**:

- **Per-metric name overrides (4 flags)**: rejected for v1 — no concrete user, deferred to a follow-up if a real exporter forces non-prefix naming. Easy to add additively later.
- **Full YAML mapping config (names + label keys)**: rejected — over-engineered. Label-name divergence is not a known requirement; if it ever lands, a separate config artifact is justified, not a flag.
- **Stripped-then-templated (`{prefix}kube_pod_info` template strings)**: rejected — same effect as additive prefix, more code, fewer affordances for tests.
- **Apply the prefix to the service-graph metric too**: rejected — different exporter family (Alloy/Tempo), realistic operators won't unify the prefix across both. A separate `KSG_SERVICE_GRAPH_METRIC` knob can ship in a follow-up change if a real deployment ever surfaces.

- Why no bespoke `--otlp-*` flags: every operator who already runs an OTel-instrumented service is configured by the standard env vars; introducing a parallel CLI surface forks the operator workflow for no benefit.
- Why no in-process result cache implications: D6 still holds — the build always runs. Tracing only adds visibility; it does not change response-body determinism (resource attributes are not in the response body).
- Why log to *both* stderr and OTLP: the binary must remain useful in `kubectl logs` even when the OTel collector is down. Fan-out keeps the stderr stream intact.
- Why bound shutdown by the existing grace period rather than a separate `--otlp-shutdown-timeout`: K8s lifecycle is governed by a single `terminationGracePeriodSeconds`; introducing a second knob forces operators to keep two values in sync.
- Alternatives considered:
  - **Replace `log/slog` with a dedicated OTel logger** (rejected — `slog` is stdlib, the project already standardised on it in D10, and the bridge is one line of init).
  - **Add an exporter selection flag** (rejected — `OTEL_EXPORTER_OTLP_PROTOCOL` already covers gRPC vs HTTP/protobuf).
  - **Trace `/livez` / `/readyz` and rely on Collector filtering** (rejected — cheap to skip at the source; saves Collector ingestion budget; matches industry guidance).
  - **Run a parallel async pipeline that captures the build trace into a debug endpoint** (rejected — duplicates OTel functionality; ops teams already have a Collector + Tempo / Jaeger).

### D27. Missing pod-UID human-label fallback

Service-graph series occasionally arrive without `client_k8s_pod_uid` or `server_k8s_pod_uid` populated. Common producers:

- **Beyla** auto-instrumentation that cannot resolve the `k8s.pod.uid` resource attribute for one side of a span (e.g., the calling process runs outside Kubernetes, or the resource-detection processor in Alloy missed it for that span batch).
- **Application-level OpenTelemetry SDKs** whose authors did not configure the K8s resource detector and consequently emit spans without `k8s.pod.uid`.
- **Mixed in-cluster / out-of-cluster traffic** where one peer is a CLI tool, a kubectl `port-forward`, or an off-cluster admin process with no pod identity at all.

In v1 prior to this decision, such series were dropped entirely — `resolveClientEndpoint` and `resolveServerEndpoint` short-circuit on empty UID. The dependency disappeared from the graph even though the human-readable `client`/`server` labels still named the endpoint clearly.

The chosen behaviour: when the pod UID for an endpoint is empty but the corresponding `client` (or `server`) string label is non-empty, the endpoint is promoted to an external node:

- `id` = `external/<label_value>` (no cluster prefix — the endpoint is not a pod)
- `name` = `<label_value>` (verbatim)
- `type` = `"external"`
- `labels` = `{}` (empty — no `cluster` key, no `pattern` key)

Per-endpoint resolution order (revised by D29):

1. **Connection-string resolution** (D29; only when UID is empty AND the human label contains `"://"`): resolves to a `service` node (plus on-demand `service-selects-pod` fan-out edges; edge type `pod-calls-service`) or — on miss — an `external` node (`id=external/<label>`, `labels={}`; edge type `pod-calls-pod`). Never a pod.
2. Pod-UID lookup against topology / synthesised-pod fallback (only when UID is non-empty).
3. Missing-UID human-label fallback (this decision; only when UID is empty AND human label is non-empty AND the label does **not** contain `"://"`; produces external node — `id=external/<label>`, `labels={}`).
4. Drop (both UID and label empty).

Consequence of the new ordering: a `"://"`-containing label with an empty UID **always** goes through Stage 1 and **never** reaches the missing-UID fallback. Both unresolvable `"://"` connection strings and non-URL missing-UID labels produce `external/<label>` nodes — there is one `external` category for all non-pod, non-service endpoints.

Decision points:

- **Always on, no knob.** Adding a config gate to revert to "drop edge" was considered and rejected — the prior drop behaviour was silent data loss, and operators already need an actionable signal when a producer regresses. A visible external node is strictly more useful than silence; if the resulting noise becomes a real problem for any deployment, a follow-up change can introduce a gate.
- **Both sides, symmetric.** The same rule fires independently for client and server endpoints. A series with both UIDs missing and both labels present produces an `external→external` edge — each endpoint is resolved independently.
- **Unified `external/<label>` ID scheme.** The fallback writes into the `external/...` namespace and a shared dedupe map with the unresolvable connection-string path (D29). Both unresolvable `"://"` connection strings and non-URL missing-UID human labels produce `external/<label>` nodes — there is one `external` category for all non-pod, non-service endpoints.
- **Edge `labels.cluster` rule unchanged.** When the client side is a non-pod (`external` via connection-string resolution OR this fallback), the edge `labels.cluster` is omitted. When the client side is still a pod and only the server fell back, the edge keeps `labels.cluster=<trace-source cluster>`.
- **Drop only when both UID AND label are empty.** With no identity at all there is no node to emit. The edge is dropped silently (consistent with the v1 contract for fully-unidentified endpoints).
- **No marker label on inferred externals.** Considered (`labels.source="missing_pod_uid"`) but rejected for v1 — the `external/<label>` ID space and the empty `labels` payload are sufficient for v1. A future revision MAY add it if richer producer-health observability becomes a priority.

Alternatives considered:

- **Drop edge on missing UID (status quo)**: rejected — silent data loss; operators routinely lose dependencies under known Beyla / Alloy resource-attribute regressions, and have no in-API signal to chase the producer.
- **Synthesise a pod node from the human label**: rejected — invents a pod identity that does not exist (no cluster, no UID, no namespace). The endpoint is not a pod; modelling it as one violates the assumption that pod IDs are `<cluster>/<uid>` and confuses traversal / filtering downstream.
- **Gate behind a new env var (e.g., `KSG_FALLBACK_MISSING_UID`)**: rejected for v1 — adds an opt-in knob with no clearly preferable default (off = silent loss, on = visible). Simpler to make the more useful behaviour unconditional and revisit if real noise emerges.
- **Maintain a second node type to distinguish declared third-party endpoints from producer-regression inferred endpoints**: rejected — both unresolvable connection strings and non-URL missing-UID labels are unified under `external`. A sudden growth of `type=external` nodes remains the actionable signal that a producer dropped `k8s.pod.uid`.
- **Surface a fix-the-producer error response instead**: rejected — the API server is a faithful relay, not a producer validator. Operators learn about the producer regression by seeing many `external/*` nodes appear in the graph; a single response-shape signal would be both noisy (per-request) and weaker (no per-endpoint visibility).

### D28. Top-level `ipaddress` attribute on pod and K8s-node entries

Pods and K8s nodes carry IP addresses out-of-band of the `labels map[string]string` bag, on a typed top-level attribute `ipaddress: []string` (`omitempty`) on the serialised node `data`. `PodNode.IPAddressValue` carries `kube_pod_info.pod_ip` (when present); `K8sNode.IPAddressValue` carries the K8s node's `ExternalIP` from `kube_node_status_addresses{type="ExternalIP"}` (when present). `PVCNode.IPAddress()` and `ExternalNode.IPAddress()` always return `nil`. The serialiser writes the slice when non-empty and omits the field otherwise.

The legacy `labels.pod_ip` / `labels.host_ip` / `labels.external_ip` keys are removed. `host_ip` (the node's IP, attached to pod samples) is dropped outright — that information lives on the node entry instead, surfaced via `K8sNode.IPAddress()`.

- Why a typed sibling field instead of a `map` key: `labels` is a string bag for typological metadata (cluster, namespace, node-id, flattened K8s labels). IPs are a typed structural attribute that has natural multi-value semantics (dual-stack, IPv4 + IPv6) and is rendered by UI clients independently of labels. Promoting it out of `labels` keeps the typological bag focused and frees consumers from string-encoded list parsing.
- Why a `[]string` instead of a single scalar: dual-stack and multi-NIC hosts surface more than one address per entity; a slice today (single element in practice for v1) avoids a breaking shape change when those scenarios become common.
- Why split pod-IP vs host-IP between the pod and the node entry: the same datum (the node's external IP) appearing on every pod under the node was redundant and invited stale-vs-fresh confusion when scrapes raced. The pod entry now carries only the pod's own IP; the node entry is the single source of truth for the node's address.
- Body-determinism (D6): `ipaddress` slices are constructed deterministically from upstream samples (newest non-empty `pod_ip` wins for pods, the `ExternalIP` row wins for nodes); when multi-element slices land, they must be sorted at construction so `encoding/json` produces byte-identical output.

Alternatives considered:

- **Keep IPs inside `labels`**: rejected — fights the typed-vs-string-bag separation and forces every consumer to know the `pod_ip` / `host_ip` / `external_ip` key set out of band.
- **Per-type schema variance (`pod.address` vs `node.address`)**: rejected — breaks the sealed-interface uniformity that lets the serialiser iterate `GraphNode` without type switches.
- **Numeric / structured IP form (`{family, value}`)**: rejected for v1 — the JSON consumer base treats IPs as opaque strings; encoding family would force a schema change once IPv6 lands and adds zero value to today's renderers.

### D29. Connection-string endpoint resolution, service nodes, and `external` fallback

Service-graph dependencies whose remote end is not a pod frequently carry a connection-string value in the `client` / `server` label — e.g. `mongodb://mongo-0.mongo.db.svc.cluster.local:27017`, `https://payments.partner.example/api`. The server detects these via hardcoded `"://"` detection (no configurable knob) plus a resolution pipeline that recognises an in-cluster Kubernetes Service named by such a string and models it as a first-class graph node — fanning out `service-selects-pod` edges to all of its backing pods — falling back to an `external` node when the string does not resolve in topology. **Every** recognised in-cluster `"://"` reference resolves to the Service, including the headless per-pod form `<pod>.<service>.<namespace>`: the pod-hostname is dropped and the address is treated as its parent Service. There is no per-pod resolution — a `"://"` endpoint is never a pod. Unresolvable `"://"` connection strings and non-URL missing-UID labels are both represented as `external` nodes.

**Hardcoded `"://"` detection.** The substring `"://"` is the only discriminator, and it is evaluated **only** against the service-graph `client` / `server` label values, per endpoint (client and server evaluated independently). When an endpoint's pod UID is **empty** AND its label contains `"://"`, connection-string resolution (Stage 0, below) runs *before* the missing-UID fallback (D27). When the UID is non-empty, normal pod-UID resolution applies unchanged — connection strings appear only when the UID is empty.

**Connection-string resolution algorithm.** Given a `"://"`-bearing label value:

1. Parse the label as a URL and take the **host** (strip scheme, userinfo, port, path, query). No host ⇒ unresolvable.
2. Match the host against Kubernetes DNS grammar. Strip an optional trailing `.svc.<cluster-domain>` (e.g. `.svc.cluster.local`); also accept the shorter `<...>.svc` and bare `<a>.<b>` forms. Count the dotted labels of the service-relative part and reduce **both** forms to the addressed `(service, namespace)`:
   - **2 labels** `<service>.<namespace>` → the addressed service (a regular ClusterIP service, or a headless service's service-level name).
   - **3 labels** `<pod-hostname>.<service>.<namespace>` (a headless per-pod DNS name) → the leading **pod-hostname is dropped**; this resolves to the same `<service>.<namespace>`. A headless per-pod address and the bare service address are treated identically — there is no per-pod resolution path.
   - any other label count → unresolvable.
3. Resolve `(cluster, namespace, service)` against the topology index `ServicesByNameNS` (built from `kube_service_info`). **HIT** → the endpoint resolves to a **service node**: `id="<cluster>/<namespace>/<service>"`, `type="service"`, `labels={cluster, namespace}`, `ipaddress=[cluster_ip]` when `cluster_ip != "None"` (omitted for a headless service's `cluster_ip="None"`). The service-graph reader **also** materialises, on demand and deduped, one `service-selects-pod` edge from this service node to **each** backing pod found in the topology index `EndpointsByService` (built from `kube_endpointslice_endpoints` joined to topology pods by `(namespace, targetref_name) → pod UID`). A known service with zero backing endpoints still materialises the service node, with no fan-out edges. **MISS** (service absent from topology) → unresolvable.
4. **Cluster determination**: the lookup is scoped to the trace-source `cluster` label (client side), because `.svc.cluster.local` is in-cluster DNS — the target lives in the same Kubernetes cluster as the caller. The reader buckets a missing `cluster` label to `"unknown"` (`bucketCluster`), so the lookup is always cluster-scoped; a connection string whose service is absent from that cluster's topology is **unresolvable**.

**Unresolvable `"://"` labels.** A `"://"` label that is not a parseable Kubernetes `.svc` name, OR whose service is absent from the trace cluster's topology, falls back to an **external node**: `id="external/<label>"`, `name="<label>"` (verbatim), `type="external"`, `labels={}` (empty). This keeps truly-external URLs (e.g. `https://payments.partner.example/api`) and unknown in-cluster names visible in the graph. All non-pod, non-service endpoints are `external`.

**Per-endpoint resolution order (updated, supersedes D27's ordering):**

1. Connection-string resolution (hardcoded `"://"`; only when UID empty AND label contains `"://"`) → service node (+ `service-selects-pod` edges; edge type `pod-calls-service`) OR (on miss) `external/<label>` with `labels={}` (edge type `pod-calls-pod`). Never a pod.
2. Pod-UID resolution against topology / synth-pod fallback (only when UID non-empty).
3. Missing-UID human-label fallback (D27) → `external/<label>`, `labels={}` (only when UID empty AND label non-empty AND label does **not** contain `"://"`).
4. Drop (both UID and label empty).

A `"://"`-bearing label with an empty UID therefore always flows through Stage 1 and never reaches the missing-UID fallback. Both unresolvable connection strings and non-URL missing-UID labels produce `external/<label>` — unified under one `external` node type and shared dedupe map. The edge `labels.cluster` rule (D9) is unchanged — present only when the **client** side resolves to a pod. A client `"://"` label always resolves to a service or external node (never a pod), so such an edge always **omits** `cluster`, consistent with every other non-pod client side. Service nodes are not pods.

**New graph types.**

- `NodeType "service"`; `ServiceNode` struct (`IDValue`, `NameValue`, `LabelsValue`, `IPAddressValue []string`). `IPAddress()` returns `[cluster_ip]` (nil when `"None"` / absent). `ServiceID(cluster, namespace, service) = "<cluster>/<namespace>/<service>"`.
- `EdgeType "service-selects-pod"` (directed service → pod, `may_cross_cluster=false`, always intra-cluster; labels: optional `namespace`, none required). Registered in `graph.EdgeTypes` and listed by `/v1/edge-types`.
- A call whose target resolves to a service node is typed `pod-calls-service` (a separate edge type, `target_type` exactly `["service"]`, always intra-cluster). `pod-calls-pod` `source_type` is `["pod", "service", "external"]` and its `target_type` is `["pod", "external"]` — service targets move to `pod-calls-service`.

**Topology reader additions.** The cluster-topology source newly consumes `kube_service_info{cluster, namespace, service, cluster_ip, ...}`, `kube_endpointslice_endpoints{cluster, namespace, endpointslice, address, targetref_kind, targetref_name, targetref_namespace, ...}`, and `kube_endpointslice_labels{cluster, namespace, endpointslice, label_kubernetes_io_service_name, ...}` (used to join slice → service name). The reader builds **indexes only**: `ServicesByNameNS` and `EndpointsByService` (service → `[]pod` resolved against topology pods by `targetref_name`). The endpoint `hostname` label is no longer consumed — the removed per-pod headless resolution was its only reader, so it drops out of the KSM contract. Service nodes and `service-selects-pod` edges are **materialised on demand** by the service-graph reader, for referenced services only — they are **not** emitted wholesale, to avoid graph bloat from services nothing calls.

> **Implementation-time verify**: confirm against a live KSM that the slice → service join label is `label_kubernetes_io_service_name` on `kube_endpointslice_labels`, joined by `(cluster, namespace, endpointslice)`, and that `kube_endpointslice_endpoints` carries the `targetref_*` labels. Adjust the index builders if the real label names differ.

**Determinism (D6).** Service nodes and `service-selects-pod` edges go through `graph.SortNodes` / `graph.SortEdges`; on-demand materialisation is deterministic for the same upstream data (the same referenced services produce the same node + edge set); `ipaddress` / `labels` follow the existing sorting rules. The response body shape `{apiVersion, clusters, elements}` is unchanged.

**Metric-prefix scope (D26).** `KSG_METRIC_PREFIX` extends to the three new KSM-shaped series (`kube_service_info`, `kube_endpointslice_endpoints`, `kube_endpointslice_labels`). It is NOT applied to `traces_service_graph_*` or `up{}`.

**KSM requirements (tracked in `tasks.md`).** Full connection-string resolution requires KSM to export services + endpointslices (`--resources`, `--metric-allowlist`) and the scrape ServiceAccount to hold `list`/`watch` on services and endpointslices.

Why:

- A `"://"`-shaped client/server value covers effectively 100 % of the real "non-pod endpoint" cases the substring knob was ever configured for, so a fixed discriminator removes a config knob with no loss of coverage, no escaping pitfalls, and no operator-tuning burden — and it is pre-GA, so there are no users to migrate.
- Recognising an in-cluster Service named by `<service>.<namespace>.svc.<domain>` lets the graph show the real Service → backing-pod fan-out instead of collapsing every in-cluster DB / cache / API call into an opaque `external` blob. Headless `<pod>.<service>.<namespace>` records (StatefulSets) resolve to the **same** Service node and fan out to all of its backing pods — one uniform shape for every in-cluster `"://"` reference, not a separate per-pod path.
- On-demand materialisation (referenced services only) keeps the graph bounded — a cluster with thousands of unreferenced services adds nothing until a `pod-calls-pod` edge actually names one.

Alternatives considered:

- **Keep a configurable substring knob and layer service resolution on top** (rejected — two overlapping detection mechanisms for the same `"://"` shape; a configurable substring earned its keep only when nothing parsed the value, and now something does).
- **Emit all services + endpointslices wholesale as nodes/edges** (rejected — graph bloat; the vast majority of services are never called by a traced pod in a given window, so they would be inert clutter).
- **Resolve services by parsing the URL host with a regex instead of the DNS-grammar label-count rule** (rejected — the 2-label vs 3-label distinction is exactly the ClusterIP-vs-headless-pod distinction; a regex hides that semantic and is harder to reason about across the `.svc` / `.svc.<domain>` / bare forms).
- **Resolve a headless `<pod>.<service>.<namespace>` to the specific addressed pod** (rejected — an earlier revision did this via an endpointslice-`hostname` lookup with a `PodsByNameNS` (pod-name == hostname) fallback. It added two topology surfaces (`PodsByNameNS`, the endpoint `hostname` label) and a separate resolution branch, and let a client-side headless string carry `cluster` — yet it still only ever pinned the edge to one replica DNS-derived name, not the trace's actual target pod. Collapsing both DNS forms to the Service + fan-out is uniform, drops the extra indexes, and matches how a service-graph consumer reasons about a dependency: at the Service level, not the replica level).
- **Require KSM services/endpointslices to be present** (rejected — absence degrades gracefully: every `"://"` label simply falls to `external/<label>`, so the feature is purely additive on the topology surface).

### D30. Exclude virtual sentinel endpoints (`user` / `unknown`) at the query layer

The OTel / Alloy / Tempo `servicegraph` connector emits **virtual nodes** for peers it cannot pair to an instrumented span: an uninstrumented caller (browser, external client, `curl`) surfaces as `client="user"`, and an unresolved peer surfaces as `"unknown"`. These virtual peers carry no `k8s.pod.uid`; they model no pod, service, or declared external dependency the graph should surface. Left in, they collapse all uninstrumented ingress into a single `user` blob node (plus `unknown` noise) that clutters every graph and never resolves to anything actionable.

**Decision.** Drop any `traces_service_graph_request_total` series whose `client` OR `server` label is exactly `"user"` or `"unknown"`, and do it **at the PromQL query layer** rather than in the Go resolver. The service-graph selector gains two anchored negative matchers:

```promql
rate(traces_service_graph_request_total{client!~"user|unknown",server!~"user|unknown"}[<window>])
```

- **Anchored, exact, case-sensitive.** PromQL / MetricsQL `!~` is a fully-anchored RE2 match, so `client!~"user|unknown"` excludes a series only when the *entire* `client` value is `user` or `unknown`. A connection-string label like `http://user/path` is **not** excluded (it is not equal to `user`), so D29 resolution is unaffected; `User` / `UNKNOWN` are not excluded (RE2 is case-sensitive). This matches the design intent exactly — two literal sentinels, nothing fuzzier.
- **Both sides, OR semantics.** The two matchers are ANDed in the selector, so a series survives only if `client` is non-sentinel AND `server` is non-sentinel — i.e. a series is dropped when **either** endpoint is a sentinel. Symmetric: `user` is client-only in practice, but matching both sides is harmless and future-proofs against connector changes.
- **Fixed, no knob.** The sentinel set `{user, unknown}` is compiled in, consistent with D29's no operator-tunable matching. An operator who genuinely wants to *see* uninstrumented ingress is not served by v1; if a real deployment needs it, a follow-up can add an opt-in, exactly as D27 left that door open for its own fallback.

**Why the query layer, not the resolver.** The series never leave VictoriaMetrics, so the reader does zero work for them — no fetch, no parse, no node materialisation, no edge dedupe. This is strictly cheaper than fetching then dropping in `parseServiceGraph`, and it needs no new Go branch: only the `QServiceGraphTotal` arm of `promql.Renderer.Render` changes. It is also impossible, by the connector's contract, for a sentinel `client` / `server` value to co-occur with a populated `*_k8s_pod_uid` — the virtual node exists *precisely because* there was no instrumented (hence no pod-identified) peer — so dropping the whole series can never discard a real pod-resolved edge. A workload whose OTel `service.name` is *literally* `user` or `unknown` would be excluded, but that naming is pathological and these strings are de-facto reserved by the connector; the trade-off is accepted.

**Relation to the "no filters pushed to PromQL" rule.** The architecture applies *caller-supplied* filters (`cluster`, `namespace`, `edge_type`, `name`, traversal) at projection time, never as PromQL pushdown — because per-request filter combinations would re-evaluate upstream per combination and break the projection-over-graph contract a future cache relies on (D2 / D7). The sentinel matchers are **not** caller-supplied and **do not vary per request**: they are a fixed part of the service-graph metric-selection contract, identical for every build, so they neither multiply upstream evaluations nor reduce cache shareability. They are therefore consistent with that rule — a fixed selector refinement, not a request filter — and this carve-out is documented so the distinction is explicit.

**Self-metric / span stability (D25 / D26).** The `Query` constant `QServiceGraphTotal` keeps its bare value (`traces_service_graph_request_total`), so the `query_name` self-metric label and the `kube_state_graph.query_name` span attribute are unchanged; only the rendered `db.statement` PromQL grows the matchers. The metric-name prefix (D26) still does not apply to this metric.

**Determinism (D6).** Excluding series upstream removes nodes / edges uniformly for the same upstream data; `SortNodes` / `SortEdges` and the `{apiVersion, clusters, elements}` body shape are unchanged. Self-metric `graph_node_count` / `graph_edge_count` are simply lower (no `user` / `unknown` virtual nodes and their edges).

**Forward note.** When the deferred numeric service-graph metrics (`traces_service_graph_request_failed_total`, `traces_service_graph_request_server_seconds_bucket`) are queried in a later revision, their selectors MUST carry the same `client` / `server` sentinel matchers so the edge set stays consistent across metric families.

Why / Alternatives considered:

- **Drop in the Go resolver after fetch** (rejected — fetches and parses series only to discard them; needs a new branch in endpoint resolution; the query-layer matcher is cheaper and localised to one `Render` arm).
- **Empty-UID-gated drop in Go** (skip only when the sentinel label has an empty UID) (rejected — adds resolver complexity to guard a case the connector contract already precludes: a sentinel label never carries a pod UID, so the whole-series drop is safe and simpler).
- **Keep `user` as a first-class "internet / end-user" node** (rejected for v1 — operators asked to suppress it; it resolves to nothing actionable and clutters every graph. A future opt-in can reintroduce it if a real deployment wants ingress visibility).
- **Make the sentinel set configurable** (rejected — consistent with D29's no-knob stance; two literals cover the connector's emitted sentinels, and a regex / list knob re-introduces exactly the tuning burden D29 removed).
- **Case-insensitive / substring match** (rejected — anchored exact match avoids false positives against legitimate names like a `user-service`, and the connector emits the lowercase literals only).

### D31. Cytoscape compound node grouping (`cluster > node > pod`), presentation-only

The Cytoscape `/v1/graph` serialiser groups nodes into compound (parent / child) containers via Cytoscape's `data.parent` field, so a renderer draws pods inside their K8s node and nodes / services / PVCs inside their cluster. This is a **presentation-only** concern: it lives entirely in `serialiseCytoscape` and touches neither the core `*Graph`, the sealed `GraphNode` types, `graph.Project`, nor the property tests.

**Containment hierarchy** — a single strict tree, because Cytoscape allows each node exactly one parent:

- `cluster > node > pod` — a synthetic `type="cluster"` group node contains the K8s nodes; each K8s node contains the pods scheduled on it. A K8s `node` node is an **edgeless** graph node that exists purely as a compound container (it still carries its `external_ip` on the `ipaddress` attribute); the pod→node relationship is expressed by this nesting and nothing else.
- `cluster > service`, `cluster > pvc` — services and PVCs are cluster-level **siblings** of K8s nodes. They are NOT compound parents of pods: a Kubernetes Service spans multiple nodes and a pod can back multiple Services, so service→pod containment is many-to-many and cannot map to a single-parent tree. The pod ↔ service / pod ↔ pvc relationships stay as edges (`pod-calls-service` / `service-selects-pod` / `pod-mounts-pvc`).
- `external` endpoints have no cluster identity → no parent (top-level).

**Synthetic cluster group nodes.** Cytoscape's `data.parent` must reference a node present in `elements.nodes`, so the serialiser emits one `{ data: { id: "cluster/<name>", name: "<name>", type: "cluster", labels: {} } }` group node per distinct `labels.cluster` value observed on an emitted node — no `parent`, no `ipaddress`. They are synthesised in the serialiser only (they are not `GraphNode`s). Emitted first, sorted by cluster name, for body determinism (D6).

**`data.parent` assignment** (a new `parent` field on the Cytoscape node `data`, `omitempty`):

- `pod` → its K8s node id, taken from the pod's `labels.node` (the cluster-scoped node id), when that node is present in the response; else its cluster group `cluster/<cluster>`; else (unknown / empty cluster) no parent.
- `node` / `service` / `pvc` → `cluster/<labels.cluster>`.
- `external` / `cluster` → no parent.

The pod→node parent derives from the pod's own `labels.node` attribute (a contract field in `graph-api`), so it survives edge-type projection.

**The pod→node relationship is compound nesting only — there is no edge for it.** Because `cluster > node > pod` nesting already expresses "pod runs on node", there is no `pod-runs-on-node` edge anywhere: not in the builder output, not in `graph.EdgeTypes`, not listed by `/v1/edge-types`, and not in any serialiser. The K8s `node` node is therefore edgeless, and the nesting (sourced from `labels.node`) is the sole on-the-wire representation of the pod→node relationship. A consequence (see D7): a `name` filter or a `root` traversal anchored on a pod does not pull in its host K8s node. A `?namespace=` filter is the **exception**: although a K8s node carries no namespace label of its own (and no incident edge), it is retained iff some pod that survived the namespace filter is scheduled on it (`labels.node`) — so the `cluster > node > pod` nesting is preserved for nodes hosting the filtered pods, while nodes hosting none of them drop. This *host-of-in-scope-pod* rule is a **projection-layer** rule living in `graph.Project`/`filterNodes` (`k8sNodePassesFilters`) — distinct from the presentation-only nesting above; it is the only place K8s-node admission is namespace-aware (`cluster` / `name` filters still apply to the node's own labels). The relationship edges (`pod-mounts-pvc`, `service-selects-pod`, `pod-to-service`, `pod-calls-pod`) are NOT containment in this tree and stay as serialised edges.

**Determinism (D6).** Cluster group nodes are emitted in sorted order ahead of the real nodes (themselves already sorted by `SortNodes` under projection); `parent` is a pure function of node attributes; the body shape stays `{apiVersion, clusters, elements}`. Byte-identical output for identical `(window, filters, upstream-data)`.

Why:

- Compound grouping is the single highest-value readability win for the Cytoscape view — operators see the cluster / node / pod hierarchy at a glance instead of a flat mesh — and Cytoscape renders it natively from `data.parent`.
- Keeping it presentation-only avoids polluting the load-bearing `graph.Project` and property invariants with edgeless group nodes (a `ClusterNode` in the core graph would never be reached by root-anchored BFS and would dangle).
- Sourcing the pod→node parent from `labels.node` (a contract attribute) keeps nesting correct under any `?edge_type=` projection, and it is the only representation of the pod→node relationship — no edge duplicates it.

Alternatives considered:

- **`ClusterNode` as a sealed `GraphNode` in the core graph** (rejected — edgeless group nodes break root-anchored BFS in `graph.Project` and force special-casing in traversal and property invariants, for no gain over synthesising them in the serialiser that needs them).
- **`cluster > node > service > pod` (service nested under node)** (rejected — a Service spans nodes and a pod backs multiple Services; forcing it into the tree fragments service identity per node and breaks the single `pod-to-service` target + the on-demand service dedupe of D29).
- **Model pod→node as an edge instead of (or in addition to) compound nesting** (rejected — the edge and the nesting double-represent the same fact; an edge from a pod to its enclosing node box is visual noise, and an edge type that no serialiser emits is dead weight in the registry / projection. Compound nesting via `labels.node` is the single, sufficient representation; the host node renders as the pod's container, not as an edge target).

### D32. Reusable graph engine as a public `pkg/`; embeddable by other modules

The graph-building stack — node / edge types, the multi-cluster `Build`, the `Project` projection, the PromQL `Querier` abstraction + `Renderer`, the `Clock`, and the Cytoscape serialiser — is promoted from `internal/` to a public `pkg/` tree so that **other Go modules can import the exact same graph engine in-process** instead of calling `GET /v1/graph` over HTTP and re-deserialising the JSON. `kube-state-graph` itself is refactored to consume its own `pkg/` (single source of truth — not a fork, not a vendored copy); the server in `internal/api` becomes a thin HTTP / auth shell over the `pkg/` engine.

The first consumer is **`graph-api-gateway`** (a separate module, `github.com/akira-core/graph-api-gateway`), which today calls ksg over HTTP, decodes the body into its own `CytoscapeGraph` structs, then merges a second **switch (network-topology) graph** by IP (its own design D9). It replaces that HTTP-plus-decode hop with the embedded `pkg/` engine: it builds the kube graph in-process and obtains the identical Cytoscape DTO with zero JSON round-trip, keeping its IP-reconcile + merge pipeline (and the external switch backend) unchanged.

**Package layout** (all under the existing `github.com/akira-core/kube-state-graph` module — same module, made importable by lifting out of `internal/`):

| pkg | Origin | Public surface |
|---|---|---|
| `pkg/graph` | `internal/graph` | `Graph`, sealed `GraphNode` + the six node types, `Edge`, `Project`, `Scope` / `NewScope`, `View`, `SortNodes` / `SortEdges`, `EdgeTypes` |
| `pkg/build` | `internal/build` | `Builder`, `New`, `Build`, the topology / service-graph readers |
| `pkg/promql` | `internal/promql` | `Querier` interface, `Renderer` + `Query` constants + sentinel selector, the HTTP `Client` + `New` (consumers build a `Querier` straight from a VM URL) |
| `pkg/clock` | `internal/clock` | `Clock`, `System`, `Fake` |
| `pkg/cytoscape` | **new** — lifted from `internal/api/serialise.go` | the `Body` / node / edge DTO types and `Serialise(g, view) Body` |
| `pkg/kubegraph` | **new** — convenience facade | `Engine`, `New(q promql.Querier, opts Options)`, `BuildFromValues(ctx, url.Values) (cytoscape.Body, error)`, plus the lower-level `Build(ctx, window, end) (*graph.Graph, error)` |

**The `kubegraph.Engine` facade** is the "easy button": it folds parse → `Build` → `Project` → `Serialise` into one call so a consumer needs no knowledge of the internal stages. The request parsing that today lives in the gin handler (`start` / `end` validation → `(window, end)`; `cluster` / `namespace` / `edge_type` / `name` / `root` / `depth` / `direction` → `graph.Scope`) is **pulled out of gin** and down into the facade, operating on `url.Values` (not `*gin.Context`). ksg's own handler then calls the same facade, so the request contract is defined once and **cannot drift** between the server and an embedded consumer. `Options` is a small `{ MetricPrefix string; Clock clock.Clock; Metrics … }` value — the server's full `config.Config` (listen addr, API keys, log level) is **not** the engine's input.

**Preserved contracts across the module boundary.** Lifting the code out of `internal/` does not relax any load-bearing rule:

- The `GraphNode` seal (D11) stays — `isGraphNode()` remains unexported, so an external module **cannot** mint its own node types. This is deliberate and is precisely why the gateway merges its switch graph at the **Cytoscape DTO layer** (D9 / D31), not by constructing `GraphNode`s: the switch nodes never need to be `GraphNode`s, so the seal costs the consumer nothing.
- Deterministic output (D6), the `{apiVersion, clusters, elements}` body shape (D9), the cluster-scoped IDs (D3), and the UUIDv5 edge IDs hold byte-for-byte — the refactor is a **pure relocation**, so the existing golden / property / component tests move with the packages and must stay green; the server's external wire output is unchanged.
- Metrics and OTLP tracing are **injectable with a no-op default**, so an embedding module does not inherit ksg's `kube_state_graph_*` self-metrics or register them in its own Prometheus registry. ksg passes its real `observability.Metrics`; the gateway passes a no-op. Spans stay no-op until an OTLP endpoint is configured (D25), so the consumer pays nothing unless it opts in.

Why:

- The gateway's stated goal is to drop the serialise → HTTP → deserialise round-trip while reusing the *identical* graph logic; a shared in-process library is the only way to get byte-identical behaviour without forking. Promoting the already HTTP-decoupled stack (D11's pure `Build` / `Project`) to `pkg/` is nearly mechanical because the seam already exists.
- Keeping it in the **same module** (just `pkg/` instead of `internal/`) avoids a third repository and release-coordination overhead for what is, today, a two-project setup maintained together.

Alternatives considered:

- **Separate shared repo / module** (e.g. `github.com/akira-core/graph-builder`) imported by both ksg and the gateway (rejected for now — most decoupled, but adds a third repo, an independent release cadence, and version-bump choreography a co-located two-project setup does not need. The `pkg/` boundary is clean enough that extracting it to its own module later, if a third consumer appears, is a non-breaking move).
- **Literal copy / vendor of `pkg/` into the gateway** (rejected — the framing was "same logic", and a copy drifts: every bug fix and contract change would have to be applied twice, defeating the single-source-of-truth goal).
- **Consumer operates on the native `*graph.Graph` end-to-end** instead of the Cytoscape DTO (rejected — the switch graph arrives from an external HTTP backend as Cytoscape JSON, so it must be decoded into *some* DTO regardless; converting it into native `graph` types would require an escape hatch through the `GraphNode` seal (D11) plus an extra conversion — i.e. **more** work than merging at the DTO layer the gateway already uses).

### D33. Self-loop UID guard — a colliding pod UID never blocks `"://"` service resolution

D29 Stage 0 (connection-string resolution) runs **only when the endpoint's pod UID is empty** — a non-empty `client_k8s_pod_uid` / `server_k8s_pod_uid` short-circuits straight to pod-UID resolution (D27 step 2), so the `"://"` label on that side is never read. This is correct when the UID genuinely identifies the peer's pod. It fails for a class of **service-graph exporter defect**: some `servicegraph` producers, when they cannot identify a remote peer (the real remote is only a `"://"` connection string), stamp the **caller's own** pod UID onto *both* sides — `client_k8s_pod_uid == server_k8s_pod_uid` (non-empty, equal) — while the actual target lives solely in the `"://"` label. Under D29's precedence the `"://"` side then collapses onto the caller's own pod: a **self-loop `pod-calls-pod` edge** is emitted and **no service node materialises**. The user-visible symptom is "the `"://"` mapping to a service failed" even though `kube_service_info`, endpointslices, and the connection string are all present and correct.

**Decision.** In `parseServiceGraph`, before resolving either endpoint, apply a narrow guard: when `client_k8s_pod_uid` and `server_k8s_pod_uid` are **both non-empty and equal**, treat the UID on any side whose label contains `"://"` as **bogus** and clear it (set to `""`) for that side only. The cleared side then follows the normal resolution order and reaches D29 Stage 0 (service / external); the **other** side keeps the shared UID and resolves to its real pod. The result for the canonical case (real caller → `"://"` remote, both UIDs = caller's) is the intended `pod-calls-service` edge plus `service-selects-pod` fan-out, instead of a self-loop `pod-calls-pod`.

```go
// D33 self-loop UID guard (pkg/build/servicegraph.go). The parse loop calls
// clientUID, serverUID = normalizeSelfLoopUIDs(clientUID, serverUID, clientLabel, serverLabel)
// before resolving either side; isConnString is the single "://" discriminator
// shared with resolveEmptyUID's Stage-0 routing so the two cannot drift.
func normalizeSelfLoopUIDs(clientUID, serverUID, clientLabel, serverLabel string) (string, string) {
    if clientUID == "" || clientUID != serverUID {
        return clientUID, serverUID
    }
    if isConnString(clientLabel) {
        clientUID = ""
    }
    if isConnString(serverLabel) {
        serverUID = ""
    }
    return clientUID, serverUID
}
```

**Scope — deliberately narrow.** The guard keys on the **conjunction** of (a) a non-empty UID collision AND (b) a `"://"` label on the cleared side. It does NOT fire when:

- the two UIDs differ (the normal case — a real caller calling a real distinct pod; `"://"` with a populated UID still correctly takes pod-UID resolution, per `TestParseServiceGraph_UIDPresentSkipsConnStringResolution`);
- the UIDs collide but neither label is a `"://"` string (a legitimate in-process self-call stays a `pod-calls-pod` self-loop — `TestParseServiceGraph_SelfLoopUID_NoConnString_StaysPodSelfLoop`).

A legitimate self-call *through* a service (`pod → http://my-svc.ns/ → same pod`) also satisfies (a)+(b) and is rerouted to `pod-calls-service` + fan-out — which is the more faithful representation of a call that physically traversed the Service, so the reroute is correct there too, not merely tolerable.

**Why clear the UID rather than reorder precedence globally.** Flipping D29 so a `"://"` label *always* wins over a populated UID would change resolution for every instrumented peer and break the contract that a populated UID identifies a pod (D27 step 2). The collision `clientUID == serverUID` is the specific, observable fingerprint of the exporter defect — gating on it leaves all healthy series untouched and confines the behavioural change to exactly the malformed shape.

**Per-endpoint resolution order (refines D29).** The guard is a **pre-resolution normalisation** of the UID inputs, evaluated once per series before Stage 0 / Stage 2. It changes only which branch each side enters; the four-step order (connection-string → pod-UID → missing-UID fallback → drop) and every node/edge contract are otherwise unchanged. The edge `labels.cluster` rule (D9) still derives from whether the **resolved** client side is a pod: a client `"://"` side cleared by this guard resolves to a service / external node, so the edge omits `cluster` — consistent with every other non-pod client side.

**Determinism (D6).** The guard is a pure function of the series' two UID labels and the two string labels; identical upstream data yields identical clearing, so `SortNodes` / `SortEdges` and the `{apiVersion, clusters, elements}` body shape are unaffected. No new node or edge type is introduced — the output reuses the existing `service` / `external` nodes and `pod-calls-service` / `service-selects-pod` / `pod-calls-pod` edges, so no golden **shape** changes (the wire form is already covered by the `with-service` golden); coverage of the guard itself lives in the `pkg/build` unit tests (`TestParseServiceGraph_SelfLoopUID_*`) and the `internal/integration` end-to-end test (`TestConnStringSelfLoopUIDResolvesToServiceNode`).

Why / Alternatives considered:

- **Do nothing — fix the exporter instead** (the genuinely correct root-cause fix is to stop the producer copying the caller's UID onto the unresolved peer; rejected as the *only* remedy because ksg is deployed against producers operators do not always control, and silent self-loops are a confusing, hard-to-attribute symptom. The guard is a defensive complement to, not a replacement for, fixing the producer).
- **Reorder D29 so `"://"` always beats a populated UID** (rejected — over-broad; changes healthy instrumented series and violates the populated-UID-means-pod contract. See "Why clear the UID" above).
- **Drop the self-loop edge entirely when UIDs collide** (rejected — discards a real dependency the `"://"` label fully describes; resolving it to the service is strictly more informative than dropping).
- **Gate behind a config knob** (rejected — consistent with D29 / D30's no-knob stance; the collision fingerprint is specific enough that the guard is safe to apply unconditionally, and an always-on defensive normalisation needs no operator tuning).

### D34. Pod controller-owner attribute (`owner` = `{kind, name}`) with ReplicaSet skipped to Deployment

**Decision.** Enrich every `type="pod"` node with its controller owner as a typed, nullable top-level attribute — `data.owner = {kind, name}` (serialised with `omitempty`, omitted entirely when the pod has no controller owner) — sourced from kube-state-metrics' `kube_pod_owner` series. The owner lives on the typed attribute, **never inside `labels`** (which stay strict typological metadata) — the same precedent as `ipaddress` (D28 / D32). It is surfaced through the sealed-interface method `graph.GraphNode.Owner() *graph.Owner` (nil for non-pods and ownerless pods), so the serialiser emits `data.owner` without a type switch. The intermediate **ReplicaSet is skipped**: when a pod's controller owner is a `ReplicaSet`, the reader resolves one level up via `kube_replicaset_owner` to the owning `Deployment` and records the Deployment as the owner. This adds **no new node or edge type** — the owner is purely a typed attribute on the existing pod node.

**Source series.** Both are kube-state-metrics defaults emitted without any `--metric-labels-allowlist` (unlike the D29 endpointslice→service join, which needs an explicit allowlist):

- `kube_pod_owner{cluster, namespace, pod, owner_kind, owner_name, owner_is_controller}` — the pod's owner references. A pod can have multiple; the **controller** is the one with `owner_is_controller="true"`.
- `kube_replicaset_owner{cluster, namespace, replicaset, owner_kind, owner_name}` — the ReplicaSet's owner (typically a Deployment).

Both are added to the topology `ReadTopology` errgroup fan-out (raising it from 8 to 10 parallel queries) and to the `KSG_METRIC_PREFIX` set, with bare `Query` constants (`QPodOwner`, `QReplicaSetOwner`) so `query_name` self-metric / span dimensions stay stable across prefixes (consistent with D26 / the prefix renderer). They are **OPTIONAL**: absence yields a valid topology with no `data.owner` on any pod and never fails a build (mirrors the `kube_node_labels` / service-index degradation in the topology spec).

**Resolution algorithm** (pure function of the loaded series — determinism preserved, D6):

1. For each pod `(cluster, namespace, pod)`, select the `kube_pod_owner` sample with `owner_is_controller="true"`. If several report `true`, pick the lexically-smallest `(owner_kind, owner_name)` so the emitted entity is stable across upstream result ordering.
2. If the selected `owner_kind == "ReplicaSet"`, look up `kube_replicaset_owner` by `(cluster, namespace, replicaset=owner_name)`. If a `Deployment` owner exists, set `owner={kind:"Deployment", name:<deployment>}`. If no such series exists (a bare ReplicaSet), keep `owner={kind:"ReplicaSet", name:<replicaset>}`.
3. Any other `owner_kind` (`DaemonSet`, `StatefulSet`, `Job`, …) is surfaced verbatim with no further lookup.
4. A pod with no controller owner has a nil `owner` — `data.owner` is omitted entirely (never an empty object), keeping the serialised body deterministic (absent ≠ empty).

**Why skip only the ReplicaSet, and only one level.** The ReplicaSet is an implementation detail of Deployment rollouts — its name carries a pod-template hash (`checkout-7f9c`) that churns on every deploy, so surfacing it as the owner would make the label unstable and analytically useless. The Deployment is the unit operators reason about. The symmetric `Job→CronJob` hop is intentionally **out of scope** for this change (Open Questions) to keep the join to a single, well-understood case; `kube_replicaset_owner` is the only second-hop series consumed.

**Wire-shape choice.** A single nested `owner` object (`{kind, name}`) rather than two flat top-level fields (`owner_kind` + `owner_name`) — a nested object is genuinely nullable (one attribute present-or-absent), expresses the kind-and-name-as-a-pair invariant (they are always set together), mirrors the existing nullable top-level attributes (`parent`, `ipaddress`), and leaves room to add owner fields later without proliferating top-level keys. It is deliberately NOT a `labels` entry: typed structured data belongs on a typed attribute, consistent with the IP-addresses-on-`ipaddress`-never-`labels` rule (D28), keeping `labels` a flat `map[string]string` of typological metadata.

**Alternatives considered.**

- **Put owner in `labels` as `owner_kind` / `owner_name`** (rejected — `labels` is strict typological metadata; structured derived data (like IPs) lives on typed attributes. Flat label keys also risk colliding with a real K8s pod label and force string-typing. This was the initial implementation and was migrated to the typed `owner` attribute).
- **Two flat top-level fields `owner_kind` / `owner_name`** (rejected — does not express the both-or-neither invariant as cleanly as one nullable object, and proliferates top-level keys; the nested object reads as a single "nullable attribute").
- **Emit owner as a separate `Deployment` / `ReplicaSet` node + `pod-owned-by` edge** (rejected — the requirement is a node-info attribute, not a topology relationship; a node/edge would balloon the graph and duplicate what `data.owner` already expresses on the pod. Reconsider only if owners need to become first-class filterable/traversable entities).
- **Surface the raw ReplicaSet without skipping** (rejected — unstable hash-suffixed name; not what operators reason about).
- **Resolve arbitrary owner chains generically** (rejected for v1 — only `ReplicaSet→Deployment` is consumed; `Job→CronJob` and deeper custom-controller chains are deferred to keep the join bounded and the series set small).

## Risks / Trade-offs

- [Per-request build cost] → Every `/v1/graph` request runs a fresh upstream PromQL fan-out (target ≤ 3 s for ≤ 5 k pods aggregated across clusters in scope). With no in-process result cache, upstream load scales linearly with HTTP traffic; `--build-timeout` bounds tail latency (`504 timeout`); concurrency control is delegated to HPA + Pod resource limits (no in-process semaphore). A future cache mechanism is expected to absorb this cost.
- [Pod UID churn on restart pollutes long lookback windows] → For windows where `last_over_time(kube_pod_info)` returns multiple UIDs for the same `(cluster, namespace, name)` tuple within the window, keep ONLY the latest UID and discard the prior. There is no reliable way to link a deleted pod's UID to its replacement once kubelet stops reporting the deleted UID (the `kube-state-metrics` series simply stops; the controller assigns a fresh UUID for the new pod with no back-reference). The earlier idea of emitting a `pod-replaced-by` synthetic edge was rejected for this reason — it would have implied an identity mapping that the source data does not support. Document in the spec.
- [Service-graph metrics absent or sparse] → Topology-only graph is still valid; missing service-graph series produce zero `pod-calls-pod` edges instead of a build failure.
- [PromQL fan-out large with many clusters] → Per-query cost (duration, samples, points) is bounded by upstream VictoriaMetrics search limits; KSG surfaces VM 5xx as `502 Bad Gateway` with `reason: "upstream"`.
- [Inconsistent `cluster` external label across scrape pipelines] → Series missing the `cluster` label are bucketed under `cluster="unknown"` and surfaced via `kube_state_graph_clusters_observed`; document that operators must set the label uniformly.
- [Cross-cluster edge with one endpoint missing topology data] → If the producer emits a `traces_service_graph_request_total` series whose `client_k8s_pod_uid` or `server_k8s_pod_uid` does not appear in any cluster's `kube_pod_info` for the window, the missing endpoint is rendered as a synthetic ghost pod node (`attrs.ghost=true`) carrying only its `cluster` and `pod_uid`, instead of dropping the edge.
- [`kube-state-metrics` retention in VictoriaMetrics shorter than requested window] → `last_over_time` returns empty; respond `400 Bad Request` with `reason: "outside retention"` when zero topology rows are returned for a window covered by upstream `up{}` data.
- [Integration-suite fixtures diverge from real producers] → Pin the metric names, label set, and cluster-label discipline the `internal/integration/` suite seeds to D8, so swapping in real producers is a configuration change rather than a code change.
- [No auth on the API] → Document that the service is intended to sit behind a reverse proxy.
- [No result cache → upstream load scales with traffic] → Accepted for v1 in pre-distributed-deployment dev. Future cache mechanism (Redis L2, materialiser tier, or graph DB) is the planned mitigation; the design space is intentionally left open so the chosen shape matches the eventual deployment topology.
- [Multi-cluster cardinality on self-metrics] → `cluster` label appears only on observational gauges (`graph_node_count`, `graph_edge_count`); document expected `cluster` cardinality range (≤ 20 in v1) and recommend dropping the label at the scrape layer if it grows beyond budget.
- [OTLP collector outage stalls slog bridge or trace export] → Both exporters use bounded `BatchSpanProcessor` / `BatchLogProcessor` queues; on persistent collector failure, the SDK drops the oldest batches and surfaces the failure via the SDK's internal error handler (logged through stderr, not through the bridge to avoid feedback loops). Local stderr logs remain unaffected.
- [Trace span explosion on debug endpoints] → `/debug/*` routes are traced; document that operators should avoid scripting curl loops over them in production. Mitigation is at the Collector via tail sampling.
- [`db.statement` attribute leaks tenant info via PromQL label matchers] → Document; operators with stricter policy disable tracing or strip the attribute at the Collector.
- [Missing-UID fallback floods graph with `external/*` nodes on producer regression] → A Beyla / Alloy regression that strips `k8s.pod.uid` resource attributes will now surface as a wave of inferred `external/<client>` and `external/<server>` nodes instead of silently dropped edges. This is intentional — the alternative was silent data loss — so a sudden growth of `type="external"` node count is a clean signal to investigate the trace pipeline rather than the API. See D27.
- [Service fan-out cardinality from on-demand `service-selects-pod` materialisation] → A hot ClusterIP service named by a `"://"` connection string materialises one `service-selects-pod` edge to **every** backing pod in `EndpointsByService` on demand (D29). A service fronting hundreds of pods adds hundreds of edges to a single build. Bounded in practice because only *referenced* services materialise (a service nothing calls adds nothing), and per-query upstream cost is still governed by VictoriaMetrics search limits (D19); operators with very-wide services should scope requests with `?cluster=` / `?namespace=` and rely on `--build-timeout`. See D29.
- [Expanded KSM resource / RBAC surface for service resolution] → Full connection-string resolution (D29) requires `kube_service_info`, `kube_endpointslice_endpoints`, and `kube_endpointslice_labels`, which means KSM must export services + endpointslices (`--resources`, `--metric-allowlist`) and the scrape ServiceAccount must hold `list`/`watch` on services and endpointslices. Absence degrades gracefully: every `"://"` label simply falls back to an `external/<label>` node, so the feature is additive on the topology surface and never fails a build. See D29.
- [Self-loop UID guard masks a producer defect instead of fixing it] → The D33 guard reroutes a `"://"` peer to its service node when an exporter stamps the caller's own pod UID on both sides (`client_k8s_pod_uid == server_k8s_pod_uid`). This makes the graph correct but hides the underlying producer bug — operators see a healthy `pod-calls-service` edge and may never learn their `servicegraph` exporter is mis-stamping UIDs. The guard is intentionally a defensive complement, not a substitute, for fixing the producer. A genuine self-call *through* a Service is also rerouted from a `pod-calls-pod` self-loop to `pod-calls-service` + fan-out — accepted as the more faithful representation. See D33.
- [Sentinel exclusion hides uninstrumented ingress] → Dropping `client` / `server ∈ {user, unknown}` series at the query layer (D30) removes the connector's virtual `user` node, so uninstrumented end-user / external ingress traffic is no longer visible as a `pod-calls-pod` edge. This is intentional — the node resolves to nothing actionable and clutters every graph — but operators who want ingress visibility lose it with no knob in v1; a follow-up opt-in can reintroduce it. A workload whose OTel `service.name` is literally `user` or `unknown` is also excluded — accepted as pathological, since those strings are de-facto reserved by the servicegraph connector. Distinct from the `cluster="unknown"` bucketing (a different label, unaffected). See D30.
- [Owner labels add two upstream queries and depend on KSM pod/replicaset owner series] → D34 raises the topology fan-out from 8 to 10 parallel PromQL queries per build. Both `kube_pod_owner` and `kube_replicaset_owner` are KSM defaults (no allowlist), so the common case needs no extra KSM config, but a deployment that has trimmed KSM `--resources` to exclude `pods` owner refs or `replicasets` will silently get no owner labels (graceful degradation, never a build failure). The two extra queries are subject to the same per-call timeout and VictoriaMetrics search limits (D19) as the existing topology legs. See D34.

## Migration Plan

Greenfield repository — no migration. Rollback is `git revert` of the merge commit. The JSON contract is versioned via a top-level `apiVersion: "v1"` field so consumers can detect breaking changes.

## Open Questions

- Final list of edge types beyond the core set (e.g., `pod-shares-node`, `pod-shares-namespace`) — resolve during spec drafting; whichever ship in v1 must appear in both `Build()` and the static `/v1/edge-types` registry. v1 ships exactly four: `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`.
- Alignment-grid policy across DST or leap seconds — likely "always UTC, no DST adjustment", confirm during spec.
- Shape of the future cache mechanism for distributed deployment (Redis L2 vs background materialiser vs graph DB). Tracked as a separate change once the deployment topology firms up.
- Whether `/v1/edge-types` should ever support time-window filtering — defer to v1.1.
- Whether the controller-owner resolution (D34) should also collapse the `Job → CronJob` hop (and other intermediate controllers) the way it skips `ReplicaSet → Deployment` — deferred; v1 consumes only `kube_replicaset_owner` and skips the ReplicaSet alone. Revisit if operators want CronJob-level grouping for `Job`-owned pods.
- Whether `/v1/clusters` should also report per-cluster pod / node counts in its response, or keep it minimal (names + first-seen / last-seen) — defer to spec.
- ~~Fake-fixtures program shape: continuous Deployment with steady-state metrics vs YAML-driven snapshot replayer~~ — resolved: no fixtures program. Integration tests (`internal/integration/`) ingest series directly via `POST /api/v1/import/prometheus` to a `testcontainers-go` VictoriaMetrics container.
