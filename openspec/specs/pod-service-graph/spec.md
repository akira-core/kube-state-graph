# pod-service-graph Specification

## Purpose
TBD - created by archiving change add-k8s-pod-graph-api. Update Purpose after archive.
## Requirements
### Requirement: Pod-UID-resolved edge source

The pod-service-graph reader SHALL build edges from service-graph metrics scraped into centralised VictoriaMetrics. The reader SHALL consume at minimum the following series, joined by pod UID:

- `traces_service_graph_request_total{client, server, cluster, client_k8s_pod_uid, server_k8s_pod_uid, client_k8s_namespace_name, server_k8s_namespace_name, connection_type}`
- `traces_service_graph_request_failed_total{ ...same labels... }`
- `traces_service_graph_request_server_seconds_bucket{ ...same labels..., le }`

Each series carries exactly one `cluster` external label, applied by the trace pipeline that produced it (typically Tempo's metrics-generator running in a single source cluster). The reader SHALL treat that `cluster` value as the **client-side cluster** — the cluster originating the call — and SHALL resolve the client pod via `(cluster, client_k8s_pod_uid)`. The server-side pod SHALL be resolved by looking up `server_k8s_pod_uid` against a global pod-UID index built from topology (Kubernetes pod UIDs are unique across clusters in practice). When the server UID matches a topology pod, the resolved pod's own `cluster` value provides the server-side cluster for the edge `target` ID.

Edges SHALL be derived by computing `rate(...[<window>]) @ <end>` over each counter. The `client` and `server` string labels are consumed by the connection-string endpoint resolution rule (see "Connection-string endpoint resolution") and the missing pod-UID human-label fallback, and are otherwise ignored for pod-resolved endpoints. Before any endpoint resolution runs, the reader SHALL exclude virtual sentinel peers at the query layer (see "Virtual sentinel endpoint exclusion (user / unknown)"); excluded series never reach the resolution stages below.

#### Scenario: Edge produced from non-zero rate

- **WHEN** for the requested window `rate(traces_service_graph_request_total{...})` is greater than zero for a series whose client side and server-pod-UID resolve via topology
- **THEN** the reader emits one `pod-calls-pod` edge whose `source` is `<cluster>/<client_k8s_pod_uid>` and `target` is `<resolved-cluster>/<server_k8s_pod_uid>`, where `<resolved-cluster>` is the topology-side cluster of the matched server pod

#### Scenario: Zero-rate series is dropped

- **WHEN** `rate(traces_service_graph_request_total{...})` evaluates to exactly zero for a series in the window
- **THEN** no edge is emitted for that series

### Requirement: Virtual sentinel endpoint exclusion (user / unknown)

The reader SHALL exclude any `traces_service_graph_request_total` series whose `client` label OR `server` label is exactly `"user"` or exactly `"unknown"`. These are **virtual peers** emitted by the service-graph producer (the OpenTelemetry / Alloy / Tempo `servicegraph` connector) for endpoints it cannot pair to an instrumented span — an uninstrumented caller surfaces as `client="user"`, an unresolved peer as `"unknown"` — and they carry no pod UID and represent no pod, service, or declared external dependency the API should surface.

The exclusion SHALL be applied **at the PromQL query layer** via anchored negative label matchers on the series selector — `client!~"user|unknown"` and `server!~"user|unknown"` — so the excluded series are never returned by upstream VictoriaMetrics and never reach endpoint resolution. The reader SHALL NOT fetch-then-drop these series in Go.

Matching semantics:

- **Exact, fully anchored**: the PromQL `!~` regex is anchored to the entire label value, so only a label whose *whole* value is `user` or `unknown` is excluded. A connection-string value such as `"http://user/path"` is NOT excluded (its value is not exactly `user`) and proceeds to connection-string resolution unchanged.
- **Case-sensitive**: `User`, `UNKNOWN`, and other case variants are NOT excluded.
- **Both sides, independently**: a series is excluded when EITHER `client` OR `server` equals a sentinel value.
- **Fixed set, no knob**: the sentinel set `{"user", "unknown"}` is compiled in. There is NO configuration surface (env var / flag / config field) to change it.

This exclusion is distinct from — and SHALL NOT affect — the `cluster="unknown"` bucketing applied to series missing a `cluster` external label (a different label on a different dimension): the sentinel matchers are evaluated ONLY against the `client` and `server` endpoint labels.

Because the excluded series never arrive, no endpoint resolution runs for them: no pod, synthesised-pod, `service`, or `external` node is materialised for a `user` / `unknown` sentinel peer, and no edge touching such a peer is emitted.

When the deferred numeric service-graph metrics (`traces_service_graph_request_failed_total`, `traces_service_graph_request_server_seconds_bucket`) are queried in a future spec revision, the same `client` / `server` sentinel matchers SHALL be applied to their selectors so the edge set stays consistent across metric families.

#### Scenario: Series with client `user` is excluded at the query layer

- **WHEN** upstream holds a `traces_service_graph_request_total` series with `client="user"`, `server="checkout"`, `server_k8s_pod_uid="abc"`
- **THEN** the service-graph query selector includes `client!~"user|unknown"`, VictoriaMetrics does not return the series, and the graph contains no edge for it and no node named `user`

#### Scenario: Series with server `unknown` is excluded

- **WHEN** upstream holds a series with `client="checkout"`, `client_k8s_pod_uid="abc"`, `server="unknown"`, `server_k8s_pod_uid=""`
- **THEN** the series is excluded by `server!~"user|unknown"`, and the graph contains no edge for it and no node named `unknown`

#### Scenario: Both endpoints are sentinels

- **WHEN** a series has `client="user"` and `server="unknown"`
- **THEN** the series is excluded (either matcher alone suffices) and no edge is emitted

#### Scenario: Connection-string value containing `user` is not excluded

- **WHEN** a series has `server="http://user/api"`, `server_k8s_pod_uid=""` (the value contains, but is not equal to, `user`)
- **THEN** the series is NOT excluded (the matcher is fully anchored), and connection-string endpoint resolution proceeds normally for that endpoint

#### Scenario: `cluster="unknown"` bucketing is unaffected

- **WHEN** a series is missing its `cluster` external label and is bucketed to `cluster="unknown"`, while its `client` and `server` labels are real service names with resolvable pod UIDs
- **THEN** the series is NOT excluded by the sentinel matchers (they match only `client` / `server`, never `cluster`), and the edge is emitted under `cluster="unknown"` exactly as before

### Requirement: Edge cluster label

For every emitted `pod-calls-pod` or `pod-calls-service` edge whose **client side resolves to a pod**, the reader SHALL set `labels.cluster = <client-pod-cluster>` from the metric's `cluster` label. The label represents the cluster that originated the RPC. When the **client side resolves to a non-pod node** (service or external), the reader SHALL omit the `cluster` key from the edge's `labels` (non-pod endpoints are not cluster-scoped). The reader SHALL NOT emit `client_cluster` or `server_cluster` keys on edge `labels` (server-side cluster is derivable from `target` node's `labels.cluster`). The reader SHALL NOT encode a `cross_cluster` boolean inside `labels` (booleans are deferred to a future typed field); cross-cluster status is derived by comparing the resolved source and target nodes' `labels.cluster` values.

#### Scenario: Intra-cluster RPC

- **WHEN** the reader processes a series with `cluster="cluster-alpha"` whose `client_k8s_pod_uid` and `server_k8s_pod_uid` both resolve to pods in `cluster-alpha`
- **THEN** the emitted edge has `labels.cluster: "cluster-alpha"`, the `target` node's `labels.cluster` is also `"cluster-alpha"`, and the edge contains no `client_cluster`, `server_cluster`, or `cross_cluster` key

#### Scenario: Cross-cluster RPC

- **WHEN** the reader processes a series with `cluster="cluster-alpha"` whose `client_k8s_pod_uid` resolves to a pod in `cluster-alpha` and whose `server_k8s_pod_uid` resolves via the global UID index to a pod in `cluster-beta`
- **THEN** the emitted edge has `labels.cluster: "cluster-alpha"`, `source: "cluster-alpha/<client-uid>"`, `target: "cluster-beta/<server-uid>"`, and the cross-cluster status is detectable by comparing the source and target node `labels.cluster` values

### Requirement: Numeric metrics deferred from v1

The reader SHALL NOT attach numeric edge metrics (`rate`, `p99_ms`, `error_rate`, etc.) to the v1 edge `labels` map, because `labels` is strictly `map[string]string` per the `graph-api` capability. Numeric metrics are deferred to a future typed struct field added in a later spec revision; v1 edges expose only string labels.

#### Scenario: No numeric labels emitted in v1

- **WHEN** the reader emits any `pod-calls-pod` or `pod-calls-service` edge in v1
- **THEN** the edge's `labels` map contains no `rate`, no `error_rate`, and no `p99_ms` key

#### Scenario: Histogram absence does not produce labels

- **WHEN** `traces_service_graph_request_server_seconds_bucket` is present for a series
- **THEN** the reader still emits no `p99_ms` key on the edge's `labels` map (numeric metrics are out of scope for v1)

### Requirement: Empty / sparse data tolerance

The reader SHALL treat an empty or sparse service-graph dataset as a valid input. When all service-graph queries return zero series, the build SHALL still complete successfully with zero `pod-calls-pod` edges, and no error SHALL be emitted.

#### Scenario: Window with no service-graph data

- **WHEN** centralised VictoriaMetrics has no `traces_service_graph_*` series in the requested window but topology queries return data
- **THEN** the build completes with a graph containing topology nodes, zero call edges (`pod-calls-pod` or `pod-calls-service`), and a 200 response

### Requirement: Connection-string endpoint resolution

When a service-graph series carries a connection string for an endpoint (an external dependency addressed by URL), that endpoint's pod UID is empty and the `client` / `server` label holds the connection string verbatim (e.g. `"mongodb://mongo-0.mongo.db.svc.cluster.local:27017"` or `"https://payments.partner.example/api"`). The reader SHALL detect connection strings by a hardcoded `"://"` substring check evaluated independently against the `client` and `server` label values. There is NO configurable knob for this detection: the reader SHALL NOT read any substring or pattern from configuration.

For each endpoint, the reader SHALL run **connection-string resolution** (Stage 0) when BOTH of the following hold:

1. the endpoint's pod UID (`client_k8s_pod_uid` or `server_k8s_pod_uid`) is empty or absent, AND
2. the corresponding label (`client` or `server`) contains the substring `"://"`.

When the pod UID is non-empty, normal pod-UID resolution applies unchanged and connection-string resolution is NOT run (connection strings only appear when the UID is empty).

Connection-string resolution proceeds as follows:

1. Parse the label as a URL and take the host (strip scheme, userinfo, port, and any path/query). If there is no host, the label is **unresolvable**.
2. Match the host against the Kubernetes DNS grammar. Strip an optional trailing `.svc.<cluster-domain>` suffix (e.g. `.svc.cluster.local`); also accept the shorter `<...>.svc` and the bare `<a>.<b>` forms. Count the dotted labels of the service-relative part and reduce BOTH forms to the addressed `(service, namespace)`:
   - 2 labels — `<service>.<namespace>` — the addressed service (regular ClusterIP service, or a headless service's service-level name).
   - 3 labels — `<pod-hostname>.<service>.<namespace>` — a headless per-pod DNS name; the reader SHALL DROP the leading `<pod-hostname>` and resolve the remaining `<service>.<namespace>`. A headless per-pod address and the bare service address resolve identically — there is NO per-pod resolution.
   - any other label count — **unresolvable**.
3. **Single local-cluster service-node resolution.** Resolve the addressed `(namespace, service)` against the topology index `ServicesByNameNS` (built from `kube_service_info`), scoped to the anchor cluster's family (step 4). The endpoint SHALL resolve to **AT MOST ONE** service node, located in the **anchor cluster itself**, and ONLY when the anchor cluster holds the addressed Service — i.e. there is a family candidate cluster (step 4) whose cluster equals the anchor cluster. When NO candidate equals the anchor cluster (the anchor cluster does not deploy the Service object), the label is **unresolvable** (→ external). This single anchor-membership test uniformly covers: an anchor whose own cluster lacks the Service (a family sibling holding it is NOT sufficient — a same-named local Service is a service-mesh precondition for the call to route at all); an `"unknown"`, empty, or bogus anchor that holds no matching Service in its own family; AND it PRESERVES the fully-unlabelled single-cluster case, because `clusterFamilyKey("unknown") = "unknown"` is a family-of-one and an `"unknown"`-bucketed Service makes `"unknown"` a legitimate holder.

   For the selected anchor candidate the reader SHALL materialise EXACTLY ONE **service** node: `id="<anchor>/<namespace>/<service>"`, `type="service"`, `labels={ cluster: anchor, namespace }`, `ipaddress=[cluster_ip]` from the ANCHOR cluster's OWN `ServiceObs` when `cluster_ip != "None"` (omitted for headless services where `cluster_ip="None"`). The reader SHALL NOT materialise a service node in any cluster other than the anchor.

   **Cross-cluster `service-selects-pod` fan-out.** From that single anchor-cluster service node, the reader SHALL materialise — on demand and deduplicated by `(service-node ID, pod ID)` — one `service-selects-pod` edge to EACH backing pod found in the `EndpointsByService` entry of **EVERY family candidate cluster that holds the same-named Service** — the UNION of `EndpointsByService[{c, namespace, service}]` over all candidate clusters `c` from step 4 (each a same-family cluster holding the Service object, built from `kube_endpointslice_endpoints` joined to topology pods by `(namespace, targetref_name) → pod UID`). These `service-selects-pod` edges MAY cross cluster boundaries: the local service node in the anchor cluster selecting a backing pod that runs in a family-sibling cluster. This reflects service-mesh endpoint aggregation (Istio multi-primary, Cilium Cluster Mesh), where the same-named Service exists in every family cluster but each cluster's kube-state-metrics observes only its OWN EndpointSlices, so the cross-cluster endpoint set is reconstructed by unioning over the family. A family sibling that holds the Service object but has zero backing pods simply contributes no edges; the local service node still materialises. There is **NO endpoint-backed pruning**: materialisation is gated on Service-object presence, never on endpoint presence — a known service with zero backing endpoints anywhere still materialises its (single, local) service node with no fan-out edges, an operator signal.
4. **Cluster-family scope (no cross-family fallback).** The **cluster-family key** of a cluster name SHALL be computed by replacing every maximal run of ASCII digits (`[0-9]+`) in the name with a single `0` sentinel character. Two clusters are in the same **family** if and only if their family keys are byte-equal. Examples: `prod-03` and `prod-12` both normalise to `prod-0` and are in the same family; `staging-1` (key `staging-0`) is NOT in `prod-1`'s family (key `prod-0`); clusters named bare digit runs such as `1` and `2` all normalise to `0` and form one family; a digit-free name normalises to itself, so its family contains only identically-named clusters. The sentinel SHALL be a digit so the mapping is collision-free without escaping: every `0` in a key originates from a digit run, and a non-digit byte can never equal the sentinel (a non-digit sentinel would collide with cluster names literally containing it). The key function SHALL be a hardcoded pure string function: there is NO configuration surface (flag / env var / config field) to alter it. The **family anchor** for a lookup SHALL be the client side's authoritative cluster: the UID-recovered client-pod cluster when the series' client side resolved to a topology pod (the trace `cluster` label is frequently missing or disagrees with topology), and otherwise the series' trace-source `cluster` label (bucketed to `"unknown"` when missing). The edge `labels.cluster` value is NOT affected by anchor recovery — it stays the raw trace label per the edge-cluster-label requirement. The **candidate clusters** SHALL be all clusters loaded in the build's topology whose family key equals the anchor's family key AND which hold the addressed `(namespace, service)` in `ServicesByNameNS`, iterated in sorted order. The candidate set serves two roles: (a) the anchor cluster, when present among them, is the single cluster whose service node materialises (step 3); (b) all candidates' `EndpointsByService` entries are unioned for the cross-cluster `service-selects-pod` fan-out (step 3). There is **NO unknown-family fallback and NO cross-family resolution**: an anchor whose own family holds no matching Service — including an unanchorable `"unknown"`/empty/bogus anchor — resolves to `external/<label>`. The same-cluster rule forbids attributing a call to a Service in a cluster the caller does not belong to; an unlabelled (`"unknown"`-bucketed) Service is reachable ONLY by an `"unknown"`-anchored caller (its own family-of-one), never unioned into a real cluster's fan-out (`"unknown" ≠ "prod-0"`). Family scoping happens in-memory at the resolution layer: there is NO PromQL query change and NO new flag or environment knob (the "no filters pushed to PromQL" contract is preserved).

The reader SHALL emit one `pod-calls-service` edge per `(resolved source node, resolved service-node ID)` pair. Because each `"://"` side now resolves to AT MOST ONE service node — in its own anchor cluster — a single upstream series yields at most one `pod-calls-service` edge per resolved source; when BOTH sides of a series are `"://"` labels resolving to service nodes, the reader SHALL emit the single (1×1) edge between them. A non-`"://"` side resolves to a single node ID exactly as before. A `pod-calls-service` edge connects its source (a pod, or — in the both-`"://"` and external-source cases — a service or external node) to a service node in the SAME (anchor) cluster, so `pod-calls-service` edges are ALWAYS intra-cluster; the cross-cluster relationship in connection-string resolution is carried by `service-selects-pod` fan-out edges, which MAY be cross-cluster. Determinism SHALL be preserved: candidate clusters are iterated in sorted order, the anchor-membership test and the endpoint union are order-free over the sorted candidate set, the existing `(source, target)` edge dedupe applies to each emitted edge (and `service-selects-pod` edges dedupe by `(service-node ID, pod ID)`), edge IDs remain deterministic UUIDv5 values over `<type>|<source>|<target>`, and the response body stays byte-identical for identical upstream data.

When the `"://"` label is **unresolvable** — the host is not a parseable Kubernetes `.svc` name, OR the anchor cluster does not itself hold the addressed Service in its own family (no candidate cluster equals the anchor) — the reader SHALL fall back to an **external** node:

- `id`     = `external/<label_value>`
- `name`   = `<label_value>` (verbatim — no normalisation, no trimming)
- `type`   = `"external"`
- `labels` = `{}` (empty map — no `cluster` key)

This keeps truly-external URLs (e.g. `https://payments.partner.example/api`), unknown in-cluster names, and connection strings whose own (caller) cluster does not deploy the addressed Service visible. All non-pod, non-service endpoints use the `external` node type — whether they arise from an unresolvable `"://"` connection string or from the missing pod-UID human-label fallback.

The decision is per endpoint: a single series MAY produce edges with a pod source and service or external targets, an external source and a pod target, two pods, or any mix. The edge `type` is `pod-calls-service` when the target resolves to a service node, otherwise `pod-calls-pod`; `pod-calls-service` edges are ALWAYS intra-cluster (the service node lives in the caller's own cluster — cross-cluster status, derived by comparing the resolved source and target node `labels.cluster` values, is therefore always intra). The edge `labels.cluster` rule for the client side applies: present when the client side resolves to a pod (from a non-empty pod UID), omitted when the client side resolves to a service or external node — including ANY `"://"` connection string, which never resolves to a pod.

#### Scenario: Headless connection string resolves to its service node and fans out to backing pods

- **WHEN** the upstream contains a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`), `server="mongodb://mongo-0.mongo.db.svc.cluster.local:27017"`, `server_k8s_pod_uid=""`, `cluster="cluster-alpha"`, and topology has a headless `mongo` service in namespace `db` whose `EndpointsByService` entry maps to a backing pod `cluster-alpha/pod-mongo-0-uid`
- **THEN** the leading pod-hostname `mongo-0` is dropped; the resulting `pod-calls-service` edge has `source: "cluster-alpha/abc"`, `target: "cluster-alpha/db/mongo"` (a `type="service"` node, NOT a specific pod), and `labels.cluster: "cluster-alpha"` (the client side is a pod); and the graph ALSO contains a `service-selects-pod` edge from `cluster-alpha/db/mongo` to `cluster-alpha/pod-mongo-0-uid`

#### Scenario: ClusterIP service connection string resolves to a service node with backing-pod edges

- **WHEN** the upstream contains a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`), `server="https://payments.payments-ns.svc.cluster.local/api"`, `server_k8s_pod_uid=""`, `cluster="cluster-alpha"`, and topology has a ClusterIP `payments` service in namespace `payments-ns` with `cluster_ip="10.0.0.5"` whose `EndpointsByService` entry maps to two backing pods `cluster-alpha/p1` and `cluster-alpha/p2`
- **THEN** the resulting `pod-calls-service` edge has `target: "cluster-alpha/payments-ns/payments"`; the target node has `type: "service"`, `name="payments"` (or service identity per the graph-api capability), `labels={ cluster: "cluster-alpha", namespace: "payments-ns" }`, and `ipaddress: ["10.0.0.5"]`; and the graph ALSO contains two `service-selects-pod` edges from `cluster-alpha/payments-ns/payments` to `cluster-alpha/p1` and `cluster-alpha/p2` respectively; the original edge has `labels.cluster: "cluster-alpha"` (the client side is a pod)

#### Scenario: Unresolvable external URL becomes an external node

- **WHEN** the upstream contains a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`), `server="https://payments.partner.example/api"`, `server_k8s_pod_uid=""`, `cluster="cluster-alpha"`, and the host `payments.partner.example` is not a parseable Kubernetes `.svc` name (no service or pod in topology)
- **THEN** the resulting `pod-calls-pod` edge has `target: "external/https://payments.partner.example/api"`; the target node has `type: "external"`, `name: "https://payments.partner.example/api"`, `labels={}` (empty — no `cluster` key); and the edge has `labels.cluster: "cluster-alpha"` (the client side is a pod)

#### Scenario: "://" label with empty UID is always handled by connection-string resolution

- **WHEN** a series has an endpoint whose pod UID is empty and whose `client` / `server` label contains `"://"` (whether or not it resolves)
- **THEN** that endpoint is resolved by connection-string resolution (at most one service node in the anchor cluster, or — when the anchor cluster does not hold the addressed Service — an `external/<label>` node) and the missing pod-UID human-label fallback is NEVER consulted for it

#### Scenario: Service in two family clusters resolves to ONE local node with cross-cluster fan-out

- **WHEN** clusters `prod-1` and `prod-2` are loaded (both family key `prod-0`), EACH holds a `payments` service in namespace `payments-ns` with its own backing pods (`prod-1/p1a` and `prod-2/p2a`), and the upstream contains a series with `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `prod-1`), `server="http://payments.payments-ns.svc.cluster.local"`, `server_k8s_pod_uid=""`
- **THEN** the reader emits EXACTLY ONE `pod-calls-service` edge — `prod-1/abc → prod-1/payments-ns/payments` — and materialises ONLY the `prod-1` service node (the anchor cluster); NO `prod-2/payments-ns/payments` service node is materialised and NO `pod-calls-service` edge targets `prod-2`; the `pod-calls-service` edge carries `labels.cluster: "prod-1"` and is intra-cluster; the single `prod-1/payments-ns/payments` service node fans out `service-selects-pod` edges to BOTH `prod-1/p1a` (intra-cluster) AND `prod-2/p2a` (cross-cluster — detectable by comparing source node `labels.cluster="prod-1"` with target pod node `labels.cluster="prod-2"`)

#### Scenario: Anchor cluster lacks the Service — external (a family sibling holding it is not enough)

- **WHEN** clusters `prod-1` and `prod-2` are loaded (family `prod-0`), ONLY `prod-2` holds a `web` service in namespace `shop` (with backing pods), `prod-1` does NOT deploy that Service object, and a series has `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (a `prod-1` pod), `server="http://web.shop.svc.cluster.local"`, `server_k8s_pod_uid=""`
- **THEN** the anchor cluster `prod-1` is not among the candidates holding the Service, so the endpoint is unresolvable: the edge has `type: "pod-calls-pod"`, `target: "external/http://web.shop.svc.cluster.local"`; the target node has `type: "external"`, `labels={}`; NO `prod-2/shop/web` service node is materialised by this resolution and NO `service-selects-pod` fan-out is emitted; the edge carries `labels.cluster: "prod-1"` (the client side is a pod)

#### Scenario: Anchor holds the Service but the whole family has zero endpoints — local node still materialises

- **WHEN** clusters `prod-1` and `prod-2` are loaded (family `prod-0`), BOTH hold a `nats` service in namespace `messaging`, NEITHER has any backing pod in `EndpointsByService` (e.g. the `kubernetes.io/service-name` endpointslice label is not allowlisted, or no backends are ready anywhere), and a series has `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (a `prod-1` pod), `server="nats://nats.messaging.svc:4222"`, `server_k8s_pod_uid=""`
- **THEN** exactly ONE `pod-calls-service` edge is emitted targeting `prod-1/messaging/nats` (the anchor's local node); the `prod-1/messaging/nats` service node materialises with ZERO `service-selects-pod` edges; no `prod-2` service node is materialised

#### Scenario: Same service in an out-of-family cluster is not unioned into the fan-out

- **WHEN** clusters `prod-1` (family key `prod-0`) and `staging-1` (family key `staging-0`) are loaded, BOTH hold a `payments` service in namespace `payments-ns` with their own backing pods, and a series has `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (a `prod-1` pod), `server="http://payments.payments-ns.svc"`, `server_k8s_pod_uid=""`
- **THEN** only `prod-1` is a candidate cluster (`staging-0` ≠ `prod-0`); exactly ONE `pod-calls-service` edge is emitted targeting `prod-1/payments-ns/payments`; its `service-selects-pod` fan-out reaches `prod-1`'s backing pods only — no `staging-1` pod is unioned and no `staging-1/payments-ns/payments` service node is materialised

#### Scenario: No family cluster holds the service — external fallback, no cross-family escape

- **WHEN** clusters `prod-1` and `prod-2` are loaded (family `prod-0`), NEITHER holds a `my-nats` service in namespace `messaging`, an out-of-family cluster (e.g. `staging-1`) DOES hold it, and a series has `cluster="prod-1"`, `client_k8s_pod_uid="abc"` (a `prod-1` pod), `server="nats://my-nats.messaging.svc:4222"`, `server_k8s_pod_uid=""`
- **THEN** the anchor family `prod-0` holds no matching Service and there is NO cross-family fallback, so the endpoint falls back to an external node: the edge has `type: "pod-calls-pod"`, `target: "external/nats://my-nats.messaging.svc:4222"`; the target node has `type: "external"`, `labels={}`; and the edge has `labels.cluster: "prod-1"` (the client side is a pod)

#### Scenario: Both sides are "://" labels — single intra-cluster edge in the anchor cluster

- **WHEN** a series has `client="http://frontend.web.svc"` and `server="http://payments.payments-ns.svc"`, BOTH pod UIDs empty, `cluster="prod-1"`, and both `(web, frontend)` and `(payments-ns, payments)` exist in BOTH family clusters `prod-1` and `prod-2`
- **THEN** the anchor for both sides is the trace cluster `prod-1` (neither side is a pod); the client side resolves to the single node `prod-1/web/frontend` and the server side to `prod-1/payments-ns/payments`; the reader emits exactly ONE `pod-calls-service` edge `prod-1/web/frontend → prod-1/payments-ns/payments` (intra-cluster), with no `cluster` key in `labels` (the client side resolved to a non-pod node); each service node additionally fans out its own cross-cluster `service-selects-pod` edges to the union of `prod-1` and `prod-2` backing pods

#### Scenario: Missing cluster label recovers the anchor from the UID-resolved client pod

- **WHEN** a series missing its `cluster` external label (bucketed to `cluster="unknown"`) has `client_k8s_pod_uid="abc"` resolving via the global UID index to a topology pod in `prod-1`, `server="http://payments.payments-ns.svc.cluster.local"`, `server_k8s_pod_uid=""`, and the `payments` service exists in family clusters `prod-1` and `prod-2`
- **THEN** the anchor is the recovered client-pod cluster `prod-1` (NOT the `"unknown"` bucket); the endpoint resolves to the single `prod-1/payments-ns/payments` service node with a cross-cluster `service-selects-pod` fan-out to both `prod-1` and `prod-2` backing pods; exactly ONE `pod-calls-service` edge is emitted and its `labels.cluster` is `"unknown"` (the raw trace label, unaffected by anchor recovery)

#### Scenario: Unanchorable series (non-pod client, no usable cluster) resolves to external

- **WHEN** a series is bucketed to `cluster="unknown"`, its client side does NOT resolve to a topology pod (e.g. `client="admin"` with an empty client UID), it has `server="http://payments.payments-ns.svc.cluster.local"`, `server_k8s_pod_uid=""`, and the `(payments-ns, payments)` service is held only by loaded clusters `prod-1` and `prod-2` (none of which is `"unknown"`)
- **THEN** the anchor is `"unknown"`, whose own family-of-one holds no matching Service (no `"unknown"`-bucketed `payments` entry), and there is NO cross-family fallback; the endpoint falls back to `external/http://payments.payments-ns.svc.cluster.local` (`type="external"`, `labels={}`), and the edge carries no `cluster` key (the client side is non-pod)

#### Scenario: Fully-unlabelled deployment keeps its pseudo-cluster resolution

- **WHEN** EVERY topology series lacks the `cluster` label (single-cluster install without an external cluster label: pods, services, and endpointslices are all bucketed to `"unknown"`), and an equally unlabelled series with a UID-resolved client pod (in `"unknown"`) addresses a `"://"` name held by the `"unknown"`-bucketed topology
- **THEN** the anchor is `"unknown"`, which IS a holder of the `"unknown"`-bucketed Service in its own family-of-one, so the endpoint resolves to the single `unknown/<namespace>/<service>` service node with its `service-selects-pod` fan-out over the `"unknown"`-bucketed backing pods — connection-string resolution is not regressed for label-less deployments

#### Scenario: Bogus-label anchor with only an unlabelled holder stays external

- **WHEN** the addressed `(namespace, service)` is known ONLY from `"unknown"`-bucketed `kube_service_info` series, and a series carries a bogus trace label (`cluster="legacy-7"`, family `legacy-0` matching no loaded cluster) with a `"://"` endpoint addressing it
- **THEN** the anchor `legacy-7`'s family `legacy-0` holds no matching Service (the `"unknown"`-bucketed entries are in family `"unknown"`, never `"legacy-0"`), so the endpoint falls back to `external/<label>` — real traffic is never attributed to a pseudo-cluster built from two pieces of non-identity

### Requirement: Missing pod-UID human-label fallback

When a service-graph series lacks a pod UID for an endpoint (`client_k8s_pod_uid` or `server_k8s_pod_uid` is empty or absent) AND the corresponding human-readable label (`client` or `server`) is non-empty AND that label does NOT contain the substring `"://"`, the reader SHALL promote that endpoint to an **external** node derived from the human label, instead of dropping the edge. (A label containing `"://"` with an empty UID is handled by connection-string resolution, not this fallback.)

This fallback fires AFTER connection-string resolution (the hardcoded `"://"` check) and BEFORE the synthesised-pod fallback. It is unconditionally on (no knob) and SHALL apply symmetrically to client and server sides.

For the affected endpoint, the reader SHALL produce a node with:

- `id`     = `external/<label_value>`  (no cluster prefix — the endpoint is not a pod and has no cluster identity)
- `name`   = `<label_value>` (verbatim — no normalisation, no trimming)
- `type`   = `"external"`
- `labels` = `{}` (empty map — no `cluster` key)

Both unresolvable `"://"` connection strings (from connection-string resolution) and NON-URL missing-UID human labels (from this fallback) produce `external/<label>` nodes sharing the same dedupe map and `id` namespace.

The edge `labels.cluster` rule is unchanged: present (set to the metric's `cluster` label) when the **client** side resolves to a pod; omitted when the client side is non-pod — whether the client became `service` via connection-string resolution or `external` via this missing-UID fallback or the unresolvable connection-string path.

When BOTH the pod UID AND the human label are empty for an endpoint, the reader SHALL drop the edge (no identity remains to construct any node).

The per-endpoint resolution order is:

1. Connection-string resolution (hardcoded `"://"` check; only when UID is empty AND label contains `"://"`) → at most ONE service node, in the caller's OWN (anchor) cluster, and only when that cluster holds the addressed `(namespace, service)` in `ServicesByNameNS` (anchored on the UID-recovered client-pod cluster when available, else the trace label) — each such service node fanning out a cross-cluster `service-selects-pod` union over the backing pods of every same-family cluster holding the Service. There is NO per-family service-node fan-out, NO unknown-family / cross-family fallback, and NO endpoint-backed pruning. When the anchor cluster does not hold the addressed Service (a sibling holding it is not enough), the endpoint resolves to an `external/<label>` node with `labels={}` (edge type `pod-calls-pod`). Never a pod; the resulting `pod-calls-service` edge is intra-cluster.
2. Pod-UID resolution against topology / synth-pod fallback (only when UID is non-empty).
3. Missing-UID human-label fallback (this requirement; only when UID is empty AND label is non-empty AND label does NOT contain `"://"`).
4. Drop (both UID and label empty).

A series with a **wholly empty side** (its pod UID AND its human label both empty) SHALL be dropped BEFORE any resolution runs for EITHER side: no edge is emitted and no node (service, external, or synthesised pod) is materialised for either endpoint — the other side's `"://"` label must not leak an orphan service/external subgraph for an edge that cannot exist.

#### Scenario: Client UID missing, client label promoted to external

- **WHEN** a service-graph series has `client="admin"`, `cluster="cluster-alpha"`, `server="rest-api"`, `server_k8s_pod_uid="abc"` (resolving to a pod with `cluster="cluster-alpha"`), and `client_k8s_pod_uid` is absent (empty string)
- **THEN** the resulting edge has `type: "pod-calls-pod"`, `source: "external/admin"`, `target: "cluster-alpha/abc"`; the source node has `id: "external/admin"`, `name: "admin"`, `type: "external"`, no `cluster` key under its `labels`; and the **edge** `labels` map contains no `cluster` key (client side is external)

#### Scenario: Server UID missing, server label promoted to external

- **WHEN** a service-graph series has `client="checkout"`, `cluster="cluster-alpha"`, `client_k8s_pod_uid="abc"` (resolving to a pod), `server="payments"`, and `server_k8s_pod_uid` is absent
- **THEN** the resulting edge has `target: "external/payments"`; the target node has `id: "external/payments"`, `name: "payments"`, `type: "external"`, no `cluster` key under its `labels`; and the edge has `labels.cluster: "cluster-alpha"` (the client side is still a pod)

#### Scenario: Both UIDs missing, both human labels present

- **WHEN** a series has `client="admin"`, `server="payments"`, `cluster="cluster-alpha"`, and both `client_k8s_pod_uid` and `server_k8s_pod_uid` are absent
- **THEN** the resulting edge has `source: "external/admin"`, `target: "external/payments"`, edge `type: "pod-calls-pod"`, and the edge `labels` map contains no `cluster` key (client side is external)

#### Scenario: Both UID and human label empty — edge dropped

- **WHEN** a series has `client_k8s_pod_uid=""` AND `client=""` (or symmetrically empty server pair)
- **THEN** no edge is emitted for that series and no node is synthesised for that endpoint

#### Scenario: Wholly empty side drops the series before the other side materialises

- **WHEN** a series has `client=""` AND `client_k8s_pod_uid=""` while `server="nats://nats.messaging.svc:4222"` with `server_k8s_pod_uid=""` (or the symmetric server-empty case with a `"://"` client)
- **THEN** the series is dropped before resolution: no edge is emitted AND no service node, no `service-selects-pod` fan-out edge, and no external node is materialised from the non-empty side's label

#### Scenario: Connection-string resolution wins over missing-UID fallback

- **WHEN** a series has `client="https://api.example.com"` with `client_k8s_pod_uid` also empty (the label contains `"://"` but the host does not resolve to any in-cluster service or pod)
- **THEN** the client side resolves via connection-string resolution to `external/https://api.example.com` (`type="external"`, `labels={}`); the missing-UID fallback is NOT consulted (the label contains `"://"`, so connection-string resolution already produced the external node)

#### Scenario: UID present — fallback does not fire

- **WHEN** a series has `client="checkout"` with `client_k8s_pod_uid="abc"`
- **THEN** the client side resolves via pod-UID lookup (with the synth-pod fallback on topology miss); the missing-UID fallback is NOT consulted (UID is non-empty)

#### Scenario: Unresolvable connection-string and non-URL missing-UID endpoints both become external nodes

- **WHEN** series A has `client="https://api.example.com"`, `client_k8s_pod_uid=""` (label contains `"://"`, host unresolvable) and series B has `client="stray-caller"`, `client_k8s_pod_uid=""` (NON-URL label; UID empty so the fallback fires)
- **THEN** series A's client resolves to `id="external/https://api.example.com"` (`type="external"`, `labels={}`) via connection-string resolution and series B's client resolves to `id="external/stray-caller"` (`type="external"`, `labels={}`) via the missing-UID fallback. Both nodes appear in the same response as `type="external"` nodes.

### Requirement: Synthesised pod node fallback

When a service-graph series references a **non-empty** pod-UID endpoint that does not appear in the topology produced for the same window, the reader SHALL synthesise a pod node and SHALL NOT drop the edge. (Empty pod UIDs are handled by the "Missing pod-UID human-label fallback" requirement above, not by this rule.)

For the **client** side, the synthesised pod uses the metric's `cluster` label as its cluster value: `id="<cluster>/<client_k8s_pod_uid>"`, `labels.cluster=<cluster>`.

For the **server** side, when the global pod-UID index has no entry for `server_k8s_pod_uid`, the synthesised pod has `cluster` unknown: `id="/<server_k8s_pod_uid>"`, `labels.cluster=""`. (The metric does not carry a `server_cluster` label under Option A; the trace pipeline only knows the source cluster.)

In both cases, `name="<pod-uid>"`, `type="pod"`, and `labels` SHALL contain `namespace` when the metric provided `client_k8s_namespace_name` / `server_k8s_namespace_name`. The reader SHALL NOT add a boolean `ghost` flag to `labels`; consumers detect synthesised endpoints by the absence of richer labels (e.g., missing `node` for pods, or empty `labels.cluster` for unknown-cluster server pods).

#### Scenario: Server pod missing from topology

- **WHEN** a service-graph series has `cluster="cluster-alpha"` and `server_k8s_pod_uid="missing-uid"` but no pod with `uid="missing-uid"` exists in the topology global pod-UID index
- **THEN** the resulting graph contains a synthesised pod node with `id: "/missing-uid"`, `name: "missing-uid"`, `type: "pod"`, `labels.cluster: ""` (server-side cluster is unknown), no `labels.ghost` key, and the edge is emitted with this node as `target`

### Requirement: Edge identity is a deterministic UUID

Edge IDs SHALL be RFC 4122 UUIDs in canonical lowercase string form. They SHALL be deterministic UUIDv5 values derived from a fixed namespace UUID compiled into the binary and the canonical input string `"<type>|<source>|<target>"` (where `<source>` and `<target>` are the cluster-scoped node IDs). Two builds that produce the same logical edge SHALL produce byte-identical edge IDs.

#### Scenario: Stable edge ID across rebuilds

- **WHEN** the same input series produces an edge in two consecutive builds for the same time bucket
- **THEN** the edge `id` is byte-identical between the two builds

#### Scenario: Edge ID is RFC 4122 compliant

- **WHEN** the reader emits any edge
- **THEN** the edge `id` matches the regex `^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$` (UUIDv5 in lowercase canonical form)

#### Scenario: Different edges produce different IDs

- **WHEN** two distinct logical edges differ only in `type`, or only in `source`, or only in `target`
- **THEN** their `id` values are not equal

