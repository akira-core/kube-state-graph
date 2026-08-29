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

The three series SHALL be read **in full for every request**: no request-scoped selector dimension (`az`, `env`, `cluster`, `namespace`) is ever rendered into a service-graph query. The trace-side `cluster` label is frequently missing or disagrees with topology, and the namespace labels describe only the caller's own view, so narrowing here would drop edges the loaded topology still needs; instead the loaded topology decides which series survive (see "Filtered-build admission and out-of-scope endpoints"). The only selectors on these queries remain the fixed, request-invariant sentinel and `edge_relation!="link"` matchers.

Edges SHALL be derived by computing `rate(...[<window>]) @ <end>` over each counter. The `client` and `server` string labels are consumed by the connection-string endpoint resolution rule (see "Connection-string endpoint resolution") and the missing pod-UID human-label fallback, and are otherwise ignored for pod-resolved endpoints. Before any endpoint resolution runs, the reader SHALL exclude virtual sentinel peers at the query layer (see "Virtual sentinel endpoint exclusion (user / unknown)"); excluded series never reach the resolution stages below.

#### Scenario: Edge produced from non-zero rate

- **WHEN** for the requested window `rate(traces_service_graph_request_total{...})` is greater than zero for a series whose client side and server-pod-UID resolve via topology
- **THEN** the reader emits one `pod-calls-pod` edge whose `source` is `<cluster>/<client_k8s_pod_uid>` and `target` is `<resolved-cluster>/<server_k8s_pod_uid>`, where `<resolved-cluster>` is the topology-side cluster of the matched server pod

#### Scenario: Zero-rate series is dropped

- **WHEN** `rate(traces_service_graph_request_total{...})` evaluates to exactly zero for a series in the window
- **THEN** no edge is emitted for that series

#### Scenario: Service-graph queries ignore request filters

- **WHEN** a build runs with `namespace={shop}` and `cluster={cluster-alpha}`
- **THEN** the three service-graph queries are issued exactly as for an unfiltered build, and the series for callers and servers outside `shop` / `cluster-alpha` reach the resolver (to be admitted or dropped by the filtered-build rule)

### Requirement: Virtual sentinel endpoint exclusion (user / unknown)

The reader SHALL exclude any `traces_service_graph_request_total` series whose `client`
label is exactly `"user"` or exactly `"unknown"`. These are **virtual peers** emitted by
the service-graph producer (the OpenTelemetry / Alloy / Tempo `servicegraph` connector) for
endpoints it cannot pair to an instrumented span — an uninstrumented caller surfaces as
`client="user"`, an unresolved peer as `"unknown"` — and they carry no pod UID and
represent no pod, service, or declared external dependency the API should surface.

The **client-side** exclusion SHALL be applied **at the PromQL query layer** via an
anchored negative label matcher on the series selector — `client!~"user|unknown"` — so a
series with either sentinel value on its `client` label is never returned by upstream
VictoriaMetrics and never reaches endpoint resolution.

The **server-side** exclusion is narrower: the query-layer matcher SHALL only exclude
`server` values exactly equal to `"user"` (`server!~"user"`). A series with `server`
exactly `"unknown"` SHALL reach Go-side resolution — it is no longer excluded at the query
layer — but the reader SHALL still treat it as unresolvable and drop it (no node, no edge)
UNLESS the "Unknown-server peer-label enrichment" requirement below applies. The narrower
server-side matcher exists solely to let that requirement's peer-label enrichment observe
the series' other labels (`client_server_address`, `client_network_peer_address`,
`client_net_peer_name`); it SHALL NOT be read as a general relaxation of the sentinel
rule — every `server="unknown"` case outside that requirement's narrow trigger condition
SHALL produce exactly the same outward result (dropped, no node, no edge) as the prior
query-layer exclusion.

Matching semantics:

- **Exact, fully anchored**: the PromQL `!~` regex is anchored to the entire label value,
  so only a label whose *whole* value is `user` or `unknown` is excluded (client side) or
  `user` (server side). A connection-string value such as `"http://user/path"` is NOT
  excluded (its value is not exactly `user`) and proceeds to connection-string resolution
  unchanged.
- **Case-sensitive**: `User`, `UNKNOWN`, and other case variants are NOT excluded.
- **Fixed set, no knob**: the sentinel set `{"user", "unknown"}` (client) / `{"user"}`
  (server) is compiled in. There is NO configuration surface (env var / flag / config
  field) to change either.

This exclusion is distinct from — and SHALL NOT affect — the `cluster="unknown"` bucketing
applied to series missing a `cluster` external label (a different label on a different
dimension): the sentinel matchers are evaluated ONLY against the `client` and `server`
endpoint labels.

Because the client-side-excluded series never arrive, no endpoint resolution runs for
them: no pod, synthesised-pod, `service`, or `external` node is materialised for a `user` /
`unknown` client sentinel, and no edge touching such a peer is emitted. A `server="unknown"`
series that reaches Go and does not satisfy the peer-label enrichment trigger is dropped in
Go with the identical observable effect (no node, no edge) — see the "Unknown-server
peer-label enrichment" requirement for the one case that resolves instead.

The same sentinel matchers (client: `user|unknown`; server: `user`) SHALL be applied,
byte-identically, to the selectors of the numeric service-graph series
(`traces_service_graph_request_failed_total`,
`traces_service_graph_request_server_seconds_bucket`) read by the "RED source series and
selector consistency" requirement, so the edge population stays consistent across metric
families. Those selectors carry one additional request-invariant matcher of their own
(the span-link exclusion); that does not weaken this rule, which fixes only the sentinel
fragment.

#### Scenario: Series with client `user` is excluded at the query layer

- **WHEN** upstream holds a `traces_service_graph_request_total` series with `client="user"`, `server="checkout"`, `server_k8s_pod_uid="abc"`
- **THEN** the service-graph query selector includes `client!~"user|unknown"`, VictoriaMetrics does not return the series, and the graph contains no edge for it and no node named `user`

#### Scenario: Series with server `unknown` and no usable peer label is still dropped

- **WHEN** upstream holds a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a real topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and the series carries none of `client_server_address`, `client_network_peer_address`, or `client_net_peer_name`
- **THEN** the series is no longer excluded at the query layer (`server!~"user"` only), but Go-side resolution drops it per the "Unknown-server peer-label enrichment" requirement's no-match case: the graph contains no edge for it and no node named `unknown`, `external/unknown`, or otherwise — identical to the outcome under the prior `server!~"user|unknown"` exclusion

#### Scenario: Series with server `unknown` and an unresolved client is still dropped

- **WHEN** upstream holds a series with `client="admin"`, `client_k8s_pod_uid=""` (the client side does NOT resolve to a real topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and the series carries a `client_network_peer_address` label
- **THEN** the peer-label enrichment trigger requires a resolved client-side pod, which this series lacks, so the series is dropped exactly as under the prior exclusion — the presence of a peer label does not by itself cause resolution

#### Scenario: Both endpoints are sentinels

- **WHEN** a series has `client="user"` and `server="unknown"`
- **THEN** the series is excluded at the query layer by the client-side matcher (`client!~"user|unknown"`) regardless of the server-side matcher or any peer label, and no edge is emitted

#### Scenario: Connection-string value containing `user` is not excluded

- **WHEN** a series has `server="http://user/api"`, `server_k8s_pod_uid=""` (the value contains, but is not equal to, `user`)
- **THEN** the series is NOT excluded (the matcher is fully anchored), and connection-string endpoint resolution proceeds normally for that endpoint

#### Scenario: `cluster="unknown"` bucketing is unaffected

- **WHEN** a series is missing its `cluster` external label and is bucketed to `cluster="unknown"`, while its `client` and `server` labels are real service names with resolvable pod UIDs
- **THEN** the series is NOT excluded by the sentinel matchers (they match only `client` / `server`, never `cluster`), and the edge is emitted under `cluster="unknown"` exactly as before

#### Scenario: Numeric series carry the identical sentinel fragment

- **WHEN** the reader issues the `traces_service_graph_request_failed_total` and `traces_service_graph_request_server_seconds_bucket` queries
- **THEN** each selector contains the same `client!~"user|unknown",server!~"user"` fragment as the request-total selector, so no sentinel peer contributes a measurement to any edge

### Requirement: Edge cluster label

For every emitted `pod-calls-pod` or `pod-calls-service` edge whose **client side resolves to a pod**, the reader SHALL set `labels.cluster` to a cluster **identity** (`cluster-topology-source`, "Cluster identity composed from zone and environment labels"): the client pod's own `labels.cluster` when the client resolved to a topology pod, otherwise the series' `cluster` label resolved through the identity ladder (composed from the series' own zone/environment labels when it carries both, adopted when the raw name is unambiguous in the build, verbatim otherwise). The label represents the cluster that originated the RPC. When the **client side resolves to a non-pod node** (service or external), the reader SHALL omit the `cluster` key from the edge's `labels` (non-pod endpoints are not cluster-scoped). The reader SHALL NOT emit `client_cluster` or `server_cluster` keys on edge `labels` (server-side cluster is derivable from `target` node's `labels.cluster`). The reader SHALL NOT encode a `cross_cluster` boolean inside `labels` (booleans are deferred to a future typed field); cross-cluster status is derived by comparing the resolved source and target nodes' `labels.cluster` values.

#### Scenario: Intra-cluster RPC

- **WHEN** the reader processes a series with `cluster="cluster-alpha"` whose `client_k8s_pod_uid` and `server_k8s_pod_uid` both resolve to pods in `cluster-alpha`
- **THEN** the emitted edge has `labels.cluster: "cluster-alpha"`, the `target` node's `labels.cluster` is also `"cluster-alpha"`, and the edge contains no `client_cluster`, `server_cluster`, or `cross_cluster` key

#### Scenario: Cross-cluster RPC

- **WHEN** the reader processes a series with `cluster="cluster-alpha"` whose `client_k8s_pod_uid` resolves to a pod in `cluster-alpha` and whose `server_k8s_pod_uid` resolves via the global UID index to a pod in `cluster-beta`
- **THEN** the emitted edge has `labels.cluster: "cluster-alpha"`, `source: "cluster-alpha/<client-uid>"`, `target: "cluster-beta/<server-uid>"`, and the cross-cluster status is detectable by comparing the source and target node `labels.cluster` values

#### Scenario: Edge label carries the client pod's identity

- **WHEN** the reader processes a series with `cluster="c1"` and no zone/environment labels whose `client_k8s_pod_uid` resolves to the topology pod `us-dev-c1/<client-uid>`
- **THEN** the emitted edge has `labels.cluster: "us-dev-c1"` and `source: "us-dev-c1/<client-uid>"`; the raw `c1` appears on no element

#### Scenario: Synthesised client resolves the trace label through the ladder

- **WHEN** the reader processes a series with `cluster="c1"` whose `client_k8s_pod_uid` is unknown to topology (a synthesised client pod), and the build's only identity for raw `c1` is `us-dev-c1`
- **THEN** the synthesised pod is `us-dev-c1/<client-uid>` with `labels.cluster: "us-dev-c1"` and the edge carries `labels.cluster: "us-dev-c1"`; when the build holds both `us-dev-c1` and `eu-prod-c1`, the synthesised pod and the edge carry the verbatim `c1` instead and the series is counted as unresolved

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

In an **unfiltered** build (no request-scoped selector dimension active), when a service-graph series references a **non-empty** pod-UID endpoint that does not appear in the topology produced for the same window, the reader SHALL synthesise a pod node and SHALL NOT drop the edge. (Empty pod UIDs are handled by the "Missing pod-UID human-label fallback" requirement above, not by this rule.) In a **filtered** build no pod is ever synthesised; the same situation is governed by "Filtered-build admission and out-of-scope endpoints".

For the **client** side, the synthesised pod uses the metric's `cluster` label as its cluster value: `id="<cluster>/<client_k8s_pod_uid>"`, `labels.cluster=<cluster>`.

For the **server** side, when the global pod-UID index has no entry for `server_k8s_pod_uid`, the synthesised pod has `cluster` unknown: `id="/<server_k8s_pod_uid>"`, `labels.cluster=""`. (The metric does not carry a `server_cluster` label under Option A; the trace pipeline only knows the source cluster.)

In both cases, `name="<pod-uid>"`, `type="pod"`, and `labels` SHALL contain `namespace` when the metric provided `client_k8s_namespace_name` / `server_k8s_namespace_name`. The reader SHALL NOT add a boolean `ghost` flag to `labels`; consumers detect synthesised endpoints by the absence of richer labels (e.g., missing `node` for pods, or empty `labels.cluster` for unknown-cluster server pods).

#### Scenario: Server pod missing from topology

- **WHEN** an unfiltered build sees a service-graph series with `cluster="cluster-alpha"` and `server_k8s_pod_uid="missing-uid"` but no pod with `uid="missing-uid"` exists in the topology global pod-UID index
- **THEN** the resulting graph contains a synthesised pod node with `id: "/missing-uid"`, `name: "missing-uid"`, `type: "pod"`, `labels.cluster: ""` (server-side cluster is unknown), no `labels.ghost` key, and the edge is emitted with this node as `target`

#### Scenario: No synthesised pod in a filtered build

- **WHEN** a build with `namespace={shop}` sees the same series, the client pod being a loaded `shop` pod and `missing-uid` not loaded
- **THEN** no pod node is synthesised; the server side resolves to `external/<server label>` under the filtered-build rule

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

### Requirement: Unknown-server peer-label enrichment

The reader SHALL attempt to resolve the server side of a
`traces_service_graph_request_total` series from three additional client-recorded
peer-address labels — `client_server_address`, `client_network_peer_address`, and
`client_net_peer_name` — instead of unconditionally dropping the endpoint, when and only
when `client_k8s_pod_uid` resolves to a **real topology pod** (not a synthesised pod), the
server side has no resolvable pod (`server_k8s_pod_uid` empty, or non-empty but absent from
the global pod-UID index), AND the raw `server` label is exactly `"unknown"`. This is a
narrow, additive carve-out from the sentinel-exclusion outcome described in "Virtual
sentinel endpoint exclusion (user / unknown)"; it SHALL NOT apply when the client side is
unresolved or when the server UID itself resolves to a topology pod.

The three labels are distinct OpenTelemetry attributes, not three spellings of one:
`client_server_address` is the stable `server.address` (the logical destination as the
caller addressed it — a name, an IP, or a UDS name); `client_network_peer_address` is the
stable `network.peer.address` (the socket-level peer address, by convention an IP);
`client_net_peer_name` is the deprecated `net.peer.name`, superseded by `server.address`.

The reader SHALL NOT read `client_network_peer_port`. The stable OpenTelemetry network
conventions carry the peer port as its own attribute, but a port participates in neither
peer identification (Service resolution keys on DNS name or `ClusterIP`) nor node naming.
Its omission is deliberate, not an oversight.

Resolution order, evaluated only under the trigger condition above. The first non-empty
label wins outright; values are never merged across labels, and the reader SHALL NOT fall
through to a lower-precedence label when a higher-precedence one is non-empty but fails to
classify:

1. If `client_server_address` is non-empty, use its value as the peer address.
2. Otherwise, if `client_network_peer_address` is non-empty, use its value as the peer
   address.
3. Otherwise, if `client_net_peer_name` is non-empty, use its value as the peer address.
4. Otherwise (all three empty or absent), the reader SHALL drop the endpoint — no node, no
   edge — identical to the outcome when this requirement does not apply.

The order ranks the labels by what the classification stages below can resolve, not by
recency of spelling. Classification is strong on names — the Kubernetes Service-DNS grammar
and the bare-short-name form both yield a `(namespace, service)` pair that resolves to a
Service node with its family-wide fan-out — and weak on IP literals, whose `ClusterIP`
lookup is deliberately restricted to the anchor cluster and whose Pod-IP lookup resolves
only an unambiguous holder within the caller's cluster family, so a loopback address, a
NodePort or load-balancer address, or any off-cluster IP resolves to nothing and falls to
`external`. `server.address` is the
name-valued stable attribute and therefore leads; `network.peer.address` is the IP-valued
stable attribute and follows; the deprecated `net.peer.name`, superseded by
`server.address`, trails.

When a peer address is obtained (step 1, 2, or 3), the reader SHALL classify it through the
following stages, in order:

- **(a) Bracket truncation.** If the value contains a `[` at any index **greater than 0**,
  truncate the value at the **first** such `[`, discarding that character and everything
  after it. Some instrumentations append a bracketed connection or session identifier to
  the authority (e.g. `mongo.com:27017[-181]`), which is not part of the network address
  and prevents the host/port split in stage (b) from succeeding. A value with no `[`, or
  whose only `[` is at index 0, is used unchanged — a leading `[` is the IPv6 bracket form
  (`[2001:db8::1]:8080`), which the host/port split in stage (b) already handles correctly
  by removing the brackets, so truncating it would destroy a resolvable address. This
  truncation SHALL be applied uniformly to whichever of the three labels supplied the
  value — the reader does NOT track which label a value came from; the index-0 condition is
  a property of the value's shape, not of its source.
- **(b) Port strip.** Strip an optional trailing `:<port>` suffix (best-effort host/port
  split; a value with no splittable port is used unchanged). The split-out port is NOT
  discarded: it is carried to the Istio route-resolution step below as that step's
  highest-precedence listener-port hint. A bracketed IPv6 authority
  that reaches this stage intact SHALL yield the IPv6 address without its brackets.
- **(c) Service-DNS grammar.** Apply the same Kubernetes Service-DNS grammar used by
  connection-string resolution (2-label `<service>.<namespace>`, or 3-label headless
  `<pod>.<service>.<namespace>` with the leading pod-hostname dropped, `.svc[.<cluster-domain>]`
  suffix stripped) to the resulting host.
- **(d) Bare short name.** When stage (c) does not match AND the host is non-empty,
  contains no `.`, and is not an IP literal, the reader SHALL treat it as a **bare short
  Service name resolved in the client pod's own namespace** — `(service=host,
  namespace=<client_k8s_namespace_name>)`. This is one grammar extension beyond
  connection-string resolution's own classification, and it applies ONLY within this
  requirement's trigger condition. The stage performs **no** DNS-1123 character or length
  validation: any dot-free non-IP-literal string is accepted as a candidate Service name,
  and an unmatchable one is filtered out by the Service lookup rather than by the grammar.
  A consequence: a value that bracket truncation or the port strip reduces to a dot-free
  label reaches this stage, so a peer such as `mongo:27017[-181]` resolves to a Service
  named `mongo` in the client pod's own namespace if one exists — the same trade this stage
  already makes for the un-bracketed `mongo:27017`.
- **(e) IP literal.** When stages (c) and (d) both fail to match AND the host is a valid IP
  literal (IPv4 or IPv6, per `net.ParseIP`), the reader SHALL look it up as a Kubernetes
  Service `ClusterIP` **within the already-resolved client pod's own (anchor) cluster
  only** — never any other cluster, including a family sibling. A match yields the
  `(namespace, service)` of the Service whose `ClusterIP` equals the host in that cluster.
  This is a second grammar extension beyond connection-string resolution's own
  classification, scoped ONLY to this requirement's trigger condition, and it is evaluated
  after, and independently of, stage (d) (an IP literal never satisfies stage (d), since
  stage (d) explicitly excludes IP literals).
- **(f) Pod IP.** When stage (e) matched no Service `ClusterIP` AND the host is a valid IP
  literal, the reader SHALL look it up as a **Pod IP** against the loaded topology's pods,
  scoped to the **caller's cluster family** (the same family key connection-string
  resolution uses). This covers a caller that dialled another pod's address directly,
  bypassing any Service — including across a cluster boundary, which is ordinary traffic
  wherever clusters share a flat routable network. Selection is:
  1. If the **anchor cluster itself** holds a pod at that address, that pod — always, even
     when family siblings also hold it.
  2. Otherwise, if **exactly one** cluster in the family holds it, that cluster's pod.
  3. Otherwise (two or more family clusters hold it), the address is **ambiguous**: the
     reader SHALL resolve no pod and SHALL fall through to stage (g), with the distinct
     external-fallback reason `unknown_server_peer_pod_ip_ambiguous`. There SHALL be no
     tie-break across clusters.

  A cluster outside the anchor cluster's family SHALL never be a candidate. Being the
  family's only holder is itself the evidence that those clusters' pod CIDRs do not overlap
  at that address, which is why no service-mesh precondition is required: cross-cluster
  pod-to-pod reachability is a network-layer property, and a sidecar is neither necessary
  (a flat network needs none) nor sufficient (in a multi-network mesh the caller's sidecar
  is handed the east-west gateway address, never a remote Pod IP). Stage (e) is evaluated
  first and wins unconditionally — a Service `ClusterIP` always beats a Pod IP — and a pod
  that reports no address is never a candidate. This stage is evaluated BEFORE the Istio
  route-resolution step below, so in-cluster (or in-family) pod traffic is never handed to
  the route engine.
- **(g) Unresolvable.** Any other shape (multi-label non-`.svc` FQDN, an IP literal that matched
  neither an anchor-cluster Service `ClusterIP` nor an unambiguous family Pod IP, any other value no earlier stage
  matched) is **unresolvable** at this step. Note that a bracketed IPv6 authority in its
  plain (non-IPv4-embedded) form that stages (a) and (b) cannot reduce to a parseable host
  does NOT reach this stage: being dot-free and not an IP literal, it is matched by stage
  (d) as a candidate Service name and is filtered out by the subsequent Service lookup
  instead. An IPv6 authority with an embedded IPv4 suffix (e.g. `[::ffff:10.0.0.5]:8080[-1]`)
  contains dots, so it fails stage (d)'s dot-free test and DOES reach this stage.

When stage (c), (d), or (e) yields a `(namespace, service)` pair, the reader SHALL
resolve it via the same same-cluster Service-node resolution used by connection-string
resolution ("Connection-string endpoint resolution", steps 3–4), anchored on the
**already-resolved client pod's own cluster** (no anchor-recovery ambiguity, since the
client side is guaranteed to be a real topology pod under this requirement's trigger
condition): AT MOST ONE service node in that cluster, materialised iff the anchor cluster
itself holds the addressed Service, with the same cross-cluster `service-selects-pod`
fan-out over every same-family cluster holding the Service. This applies identically
whether the `(namespace, service)` pair was obtained via DNS-grammar classification (stage
c), bare-short-name classification (stage d), or IP-literal `ClusterIP` matching (stage e):
once identified, an IP-literal match is resolved exactly like any other classified
Service address — including the family-wide `service-selects-pod` fan-out — only the
*identification* step (stage e itself) is restricted to the anchor cluster. Resolution is
likewise identical regardless of which of the three labels supplied the peer address.

When stage (f) yields a topology **pod**, the reader SHALL resolve the endpoint directly to
that pod. It SHALL NOT materialise a service node and SHALL NOT emit any
`service-selects-pod` edge, so the generic target-driven rule makes the resulting edge
`pod-calls-pod` — which MAY therefore cross clusters.

If two or more Services within the SAME anchor cluster share the identical `ClusterIP`
value (a data anomaly Kubernetes itself prevents in a healthy cluster, but the reader
stays defensive), stage (e) SHALL deterministically select the Service with the
lexically-smaller `(namespace, service)` pair.

If two or more pods within the SAME cluster report the identical `pod_ip` — which happens
legitimately for `hostNetwork` pods, all of which report their node's address, and
transiently on address reuse within the window — stage (f) SHALL deterministically select
the pod with the lexically-smallest cluster-scoped id, so an intra-cluster duplicate never
makes the family look ambiguous.

When classification is unresolvable (stage g), OR the anchor cluster does not hold the
addressed Service, the reader SHALL — **before** falling back to an external node —
consult the Istio route-resolution engine as specified in "Istio route resolution of global
FQDN peers" (subject to that requirement's own preconditions: in particular, an endpoint
carrying no destination IPs is NEVER consulted and falls external directly). This
route-resolution step SHALL be reached from EVERY path that would otherwise produce an
external node under this requirement, without exception. In particular, a global or ingress
FQDN such as `api.example.com` is a 3-label host and is therefore *successfully* classified
by stage (c) — as service `example` in namespace `com` — before failing the anchor-cluster
membership test, so it reaches route resolution via the "anchor cluster does not hold the
addressed Service" path, NOT via the unresolvable path. An implementation that consults the
engine only on stage-(g) unresolvability does NOT satisfy this requirement.

When route resolution is disabled, or does not yield a destination, the reader SHALL fall
back to an **external** node from the RAW peer-address
value — the label's value exactly as read from the series, with NEITHER the bracket
truncation of stage (a) NOR the port strip of stage (b) applied:

- `id`     = `external/<raw_peer_address_value>`
- `name`   = `<raw_peer_address_value>` (verbatim — no normalisation, no trimming)
- `type`   = `"external"`
- `labels` = `{}` (empty map — no `cluster` key)

This raw-value convention is identical for all three peer-address labels. A consequence,
accepted deliberately: one host dialed under several distinct bracketed identifiers
materialises one external node per identifier.

The resulting edge follows the existing generic rules unchanged: `type` is
`pod-calls-service` when the resolved target is a service node, otherwise `pod-calls-pod`;
`labels.cluster` is present (the client pod's cluster) because the client side is a
resolved pod. No new node type and no new edge type are introduced by this requirement.

#### Scenario: `client_network_peer_address` resolves to an in-cluster Service

- **WHEN** a series has `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`, namespace `shop`), `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="payments.payments-ns.svc.cluster.local"`, no `client_server_address` and no `client_net_peer_name`, and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with backing pods
- **THEN** the resulting edge has `type: "pod-calls-service"`, `target: "cluster-alpha/payments-ns/payments"`, `labels.cluster: "cluster-alpha"`; the target service node materialises with its usual `service-selects-pod` fan-out to its backing pods

#### Scenario: `client_server_address` outranks both other peer labels

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, and all three labels are non-empty — `client_server_address="payments.payments-ns.svc.cluster.local"`, `client_network_peer_address="10.4.4.4"`, `client_net_peer_name="legacy.other-ns.svc.cluster.local"` — with the `payments` and `legacy` Services present in `cluster-alpha` and a `cluster-alpha` Service holding `cluster_ip="10.4.4.4"`
- **THEN** the endpoint resolves from `client_server_address` alone, targeting `cluster-alpha/payments-ns/payments`; neither the `ClusterIP`-matching Service nor `legacy` contributes a node or an edge

#### Scenario: `client_network_peer_address` outranks `client_net_peer_name`

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, no `client_server_address`, `client_network_peer_address="payments.payments-ns.svc.cluster.local"`, and `client_net_peer_name="legacy.other-ns.svc.cluster.local"`, with both addressed Services present in `cluster-alpha`
- **THEN** the endpoint resolves from `client_network_peer_address`, targeting `cluster-alpha/payments-ns/payments`; `legacy` contributes no node and no edge

#### Scenario: A non-classifying higher-precedence label does not fall through

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="payments.partner.example"` (a multi-label host that is not a `.svc` name), and `client_network_peer_address="payments.payments-ns.svc.cluster.local"` (which WOULD resolve in `cluster-alpha`)
- **THEN** the endpoint resolves from `client_server_address` only and falls back to `external/payments.partner.example` — the first non-empty label wins outright, and a failure to classify does NOT defer to a lower-precedence label

#### Scenario: Bracketed connection identifier is truncated before classification

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="payments.payments-ns.svc.cluster.local:27017[-181]"` (no `client_server_address`), and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha`
- **THEN** the value is truncated at the first `[` to `payments.payments-ns.svc.cluster.local:27017`, the `:27017` port suffix is then stripped, and the endpoint resolves to `cluster-alpha/payments-ns/payments` with `type: "pod-calls-service"`

#### Scenario: Bracketed value that does not resolve keeps its raw form as the external node

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, and `client_network_peer_address="mongo.com:27017[-181]"` (classification sees host `mongo.com`, which addresses no Service in `cluster-alpha`)
- **THEN** the endpoint falls back to an external node with `id: "external/mongo.com:27017[-181]"` and `name: "mongo.com:27017[-181]"` — the raw value verbatim, with neither the bracket suffix nor the port removed — and the edge has `type: "pod-calls-pod"`

#### Scenario: Two bracketed identifiers on one host produce two external nodes

- **WHEN** two series both resolve their client side to pods in `cluster-alpha` with `server="unknown"`, carrying `client_network_peer_address="mongo.com:27017[-181]"` and `client_network_peer_address="mongo.com:27017[-182]"` respectively, and `mongo.com` addresses no Service in `cluster-alpha`
- **THEN** two distinct external nodes are materialised — `external/mongo.com:27017[-181]` and `external/mongo.com:27017[-182]` — each the target of its own edge; the raw-value naming convention is not deduplicated by host

#### Scenario: Bracketed IP-literal peer address resolves to its ClusterIP-matching Service

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="172.20.10.5:27017[-9]"`, and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with `cluster_ip="172.20.10.5"`
- **THEN** the bracket suffix and the port are stripped in that order, the resulting host `172.20.10.5` matches by `ClusterIP`, and the endpoint resolves to `cluster-alpha/payments-ns/payments`

#### Scenario: Bracketed IPv6 authority is NOT truncated and still resolves by ClusterIP

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="[fd00:10:96::a]:8080"`, and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with `cluster_ip="fd00:10:96::a"`
- **THEN** the leading `[` is at index 0, so bracket truncation does NOT apply; the host/port split removes the brackets and the port, yielding `fd00:10:96::a`, which matches by `ClusterIP` and resolves to `cluster-alpha/payments-ns/payments` — identical to the behaviour before bracket truncation existed

#### Scenario: Bracketed IPv6 authority carrying a trailing identifier stays external

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, and `client_network_peer_address="[fd00:10:96::a]:8080[-181]"`
- **THEN** the first `[` is at index 0 so truncation does not apply, the host/port split then fails on the trailing `[`, the value reaches the bare-short-name stage (it is dot-free and not an IP literal) and is matched there as a candidate Service name, the Service lookup finds nothing, and the endpoint falls back to `external/[fd00:10:96::a]:8080[-181]` with the raw value verbatim

#### Scenario: Bracket truncation applies to the other peer labels too

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, no `client_server_address` and no `client_network_peer_address`, and `client_net_peer_name="payments.payments-ns.svc.cluster.local:27017[-181]"`, with a `payments` service in namespace `payments-ns` in `cluster-alpha`
- **THEN** the same truncation applies and the endpoint resolves to `cluster-alpha/payments-ns/payments` — the normalisation is a property of the peer-address value, not of which label carried it

#### Scenario: Truncation reduces a value to a bare short name and it resolves in the client's namespace

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, namespace `shop`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_address="payments:8080[-181]"`, and `cluster-alpha` holds a Service named `payments` in namespace `shop`
- **THEN** the value truncates to `payments:8080`, the port strip yields `payments`, the bare-short-name stage resolves it to `(namespace="shop", service="payments")`, and the endpoint targets `cluster-alpha/shop/payments` — the same trade the bare-short-name stage already makes for the un-bracketed value `payments:8080`, now reachable by one more value shape. Absent such a Service the endpoint falls back to `external/payments:8080[-181]` with the raw value verbatim

#### Scenario: `client_server_address` resolves to an in-cluster Service

- **WHEN** a series has `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`, namespace `shop`), `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="payments.payments-ns.svc.cluster.local:8080"`, and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with backing pods
- **THEN** the port suffix `:8080` is stripped before classification, the resulting edge has `type: "pod-calls-service"`, `target: "cluster-alpha/payments-ns/payments"`, `labels.cluster: "cluster-alpha"`, and the target service node materialises with its usual `service-selects-pod` fan-out to its backing pods

#### Scenario: `client_net_peer_name` used only when both higher-precedence labels are absent

- **WHEN** a series has `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a pod in `cluster-alpha`), `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address=""` and `client_network_peer_address=""` (both absent), and `client_net_peer_name="payments.payments-ns.svc.cluster.local"`
- **THEN** the endpoint resolves from `client_net_peer_name`, targeting `cluster-alpha/payments-ns/payments` exactly as if a higher-precedence label had carried the same host

#### Scenario: Bare short Service name resolves in the client pod's own namespace

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, namespace `shop`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="payments"` (a bare single-label name, no `.svc` suffix), and topology has a `payments` service in namespace `shop` (the client's own namespace) in `cluster-alpha`
- **THEN** the reader treats `payments` as addressing `(namespace="shop", service="payments")` and resolves it to `cluster-alpha/shop/payments`, exactly as the 2-label `.svc` form would

#### Scenario: Bare IP-literal peer address resolves to its ClusterIP-matching Service

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="172.20.10.5"` (no `:port`, no DNS name), and topology has a `payments` service in namespace `payments-ns` in `cluster-alpha` with `cluster_ip="172.20.10.5"`
- **THEN** the resulting edge has `type: "pod-calls-service"`, `target: "cluster-alpha/payments-ns/payments"`, exactly as if the peer address had been the Service's `.svc` DNS name

#### Scenario: IP-literal peer address with a port suffix is stripped before matching

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="172.20.10.5:8080"`, and topology has a service in `cluster-alpha` with `cluster_ip="172.20.10.5"`
- **THEN** the `:8080` suffix is stripped before IP-literal classification, and the endpoint resolves to that service node

#### Scenario: IP-literal peer address present only in a family-sibling cluster — external, not cross-resolved

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="172.20.10.5"`, `cluster-alpha` holds NO service with `cluster_ip="172.20.10.5"`, but a same-family sibling cluster `cluster-alpha-2` DOES hold a service with that `ClusterIP`
- **THEN** the endpoint falls back to `external/172.20.10.5` — the IP-literal lookup (stage e) is scoped to the anchor cluster only and does NOT consult family siblings, unlike the subsequent `service-selects-pod` fan-out that would apply once a Service is identified

#### Scenario: IP-literal peer address absent from the anchor cluster's Service set — external

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="192.0.2.55"`, and no Service in `cluster-alpha` has that `ClusterIP`
- **THEN** the endpoint falls back to `external/192.0.2.55`

#### Scenario: External peer address becomes an external node

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, and `client_net_peer_name="payments.partner.example"` (a multi-label host that is not a `.svc` name and not a bare short name)
- **THEN** the resulting edge has `type: "pod-calls-pod"`, `target: "external/payments.partner.example"`; the target node has `type: "external"`, `name: "payments.partner.example"`, `labels={}`

#### Scenario: Anchor cluster lacks the addressed Service — external, not dropped

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="web.shop.svc.cluster.local"`, and `cluster-alpha` does NOT hold a `web` service in namespace `shop` (a family sibling holding it does not count, per the existing same-cluster rule)
- **THEN** the endpoint falls back to `external/web.shop.svc.cluster.local` rather than resolving to a service node in a different cluster, and rather than being dropped

#### Scenario: No peer label present — dropped

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a real topology pod, `server="unknown"`, `server_k8s_pod_uid=""`, and all of `client_server_address`, `client_network_peer_address`, and `client_net_peer_name` are empty or absent
- **THEN** the endpoint is dropped: no node and no edge are produced for it

#### Scenario: `client_network_peer_port` alone does not trigger enrichment

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a real topology pod, `server="unknown"`, `server_k8s_pod_uid=""`, `client_network_peer_port="27017"`, and all three peer-address labels are empty or absent
- **THEN** the endpoint is dropped: the port label is not read, is never treated as a peer address, and produces no node and no edge

#### Scenario: Client side not a resolved real pod — enrichment does not apply

- **WHEN** a series has `client="admin"`, `client_k8s_pod_uid=""` (client does not resolve to a topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and `client_network_peer_address="payments.payments-ns.svc.cluster.local"` is present
- **THEN** the trigger condition (client resolved to a real pod) is not met, so this requirement does not apply and the endpoint is dropped per the sentinel-exclusion requirement, even though a peer label is present

#### Scenario: Server UID present but unresolved — enrichment still applies

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a real topology pod, `server="unknown"`, `server_k8s_pod_uid="stale-uid"` (non-empty but absent from the global pod-UID index), and `client_network_peer_address="payments.payments-ns.svc.cluster.local"` resolves to an in-cluster Service
- **THEN** the reader resolves via peer-label enrichment (target: the service node) rather than synthesising a pod node for `stale-uid` — a non-empty but unresolvable server UID does not take priority over this requirement when `server` is literally `"unknown"`

#### Scenario: Duplicate ClusterIP within the anchor cluster resolves deterministically

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="172.20.10.5"`, and (as a data anomaly) `cluster-alpha` holds two Services with `cluster_ip="172.20.10.5"` — `(namespace="ops", service="zeta")` and `(namespace="ops", service="alpha")`
- **THEN** the endpoint resolves to `(namespace="ops", service="alpha")` — the lexically-smaller `(namespace, service)` pair — deterministically and identically across rebuilds of the same upstream data

#### Scenario: Global FQDN reaches route resolution via the anchor-lacks-service path

- **WHEN** a `server="unknown"` series has a client resolving to a real topology pod and
  `client_net_peer_name="api.example.com"`
- **THEN** DNS-grammar classification (step 2) succeeds, yielding `(namespace="com",
  service="example")`
- **AND** the anchor cluster does not hold that Service, so same-cluster Service-node
  resolution misses
- **AND** the reader SHALL consult the route-resolution engine before emitting any external
  node

#### Scenario: Route resolution disabled falls back to external unchanged

- **WHEN** route resolution is not configured
- **AND** a `server="unknown"` endpoint's peer address is unresolvable by steps 2–4, or the
  anchor cluster does not hold the classified Service
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>` with `labels = {}`,
  byte-for-byte identical to the behaviour before route resolution existed

#### Scenario: In-cluster classification is unaffected by route resolution

- **WHEN** a `server="unknown"` endpoint's peer address classifies to a `(namespace,
  service)` pair the anchor cluster DOES hold — via `.svc` DNS, bare short name, or
  ClusterIP literal
- **THEN** the reader SHALL resolve it to that service node exactly as before
- **AND** the route-resolution engine SHALL NOT be consulted

#### Scenario: IP-literal peer address matches a pod's own IP in the anchor cluster

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="10.244.1.9"`, no Service in `cluster-alpha` has `cluster_ip="10.244.1.9"`, and a topology pod with UID `def` in `cluster-alpha` has `pod_ip="10.244.1.9"`
- **THEN** the resulting edge has `type: "pod-calls-pod"`, `target: "cluster-alpha/def"`, `labels.cluster: "cluster-alpha"`; no service node is materialised and no `service-selects-pod` edge is emitted for this resolution

#### Scenario: Pod-IP peer address with a port suffix is stripped before matching

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_net_peer_name="10.244.1.9:8080"`, and a topology pod with UID `def` in `cluster-alpha` has `pod_ip="10.244.1.9"`
- **THEN** the `:8080` suffix is stripped before IP-literal classification, and the endpoint resolves to `cluster-alpha/def`

#### Scenario: Service ClusterIP takes priority over a colliding Pod IP

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="10.96.0.7"`, `cluster-alpha` holds a `payments` service in namespace `shop` with `cluster_ip="10.96.0.7"`, AND a pod in `cluster-alpha` also reports `pod_ip="10.96.0.7"`
- **THEN** the endpoint resolves to the service node `cluster-alpha/shop/payments` with `type: "pod-calls-service"` — the Service `ClusterIP` step is evaluated first and the Pod-IP step is never reached

#### Scenario: Pod IP held only by a family sibling resolves across the cluster boundary

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `prod-1`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="10.244.1.9"`, no Service or pod in `prod-1` carries that address, and exactly one same-family sibling cluster `prod-2` has a pod (UID `sib`) with `pod_ip="10.244.1.9"`
- **THEN** the resulting edge has `type: "pod-calls-pod"`, `target: "prod-2/sib"`, `labels.cluster: "prod-1"` (the client side, unchanged); no external node is produced, no service node is materialised, and no `service-selects-pod` edge is emitted

#### Scenario: Anchor cluster wins over a family sibling holding the same Pod IP

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `prod-1`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="10.244.1.9"`, a pod in `prod-1` (UID `own`) has `pod_ip="10.244.1.9"`, AND a pod in the same-family sibling `prod-2` also has `pod_ip="10.244.1.9"`
- **THEN** the endpoint resolves to `prod-1/own` — the anchor cluster's own holder is always preferred, regardless of how many family siblings carry the address

#### Scenario: Two family siblings hold the Pod IP — external, no tie-break

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `prod-1`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="10.244.1.9"`, no pod in `prod-1` carries that address, and BOTH same-family siblings `prod-2` and `prod-3` have a pod with `pod_ip="10.244.1.9"`
- **THEN** the endpoint falls back to `external/10.244.1.9` and NO `pod-calls-pod` edge targets either sibling pod — two holders is direct evidence that the family's pod CIDRs overlap at this address, so the reader degrades rather than tie-breaking

#### Scenario: Pod IP present only outside the anchor cluster's family — external

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `prod-1` (family `prod-0`), `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="10.244.1.9"`, and the only pod carrying that address lives in `staging-1` (family `staging-0`)
- **THEN** the endpoint falls back to `external/10.244.1.9` — a cluster outside the anchor cluster's family is never a candidate

#### Scenario: Pod without a known IP is never a Pod-IP candidate

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="10.244.1.9"`, and every pod in `cluster-alpha`'s family has an empty or absent `pod_ip`
- **THEN** the endpoint falls back to `external/10.244.1.9`

#### Scenario: Duplicate Pod IP within one cluster resolves deterministically

- **WHEN** a series has `client_k8s_pod_uid="abc"` resolving to a pod in `cluster-alpha`, `server="unknown"`, `server_k8s_pod_uid=""`, `client_server_address="10.244.1.9"`, and two pods in `cluster-alpha` — node ids `cluster-alpha/zzz` and `cluster-alpha/aaa` — both report `pod_ip="10.244.1.9"` (for example two `hostNetwork` pods on the same node)
- **THEN** the endpoint resolves to `cluster-alpha/aaa` — the lexically-smallest node id — deterministically, independently of the order in which the pods were loaded and identically across rebuilds of the same upstream data; the intra-cluster duplicate does NOT make the cluster an ambiguous candidate, since ambiguity is counted per cluster, not per pod

### Requirement: RED edge metrics on trace-derived pod / service edges

The reader SHALL attach a typed, nullable numeric metrics object to an emitted edge **iff all four** of the following hold:

1. the edge was produced from at least one `traces_service_graph_request_total` series (it is **trace-derived**, not synthesised from topology or from a route-resolution outcome); AND
2. **both** resolved endpoints name a `type="pod"` node (a real topology pod or a synthesised pod) or a `type="service"` node; AND
3. the edge is NOT the route-hit ingress chain's caller → ingress-service entry hop; AND
4. the edge has at least one **in-scope contributing series** (defined below) and their summed rate is a finite value greater than zero.

**How an endpoint was identified SHALL NOT affect eligibility.** A non-empty `client_k8s_pod_uid` / `server_k8s_pod_uid`, a `"://"` connection string resolved to an in-cluster Service, a `server="unknown"` peer address classified as a Service DNS name, a peer address matched against a Service `ClusterIP`, a peer address matched against a family Pod IP, and an Istio route-engine resolution to a backend Service all satisfy condition 2 equally. The edge `type` SHALL likewise not be a condition: both `pod-calls-pod` and `pod-calls-service` edges MAY carry the object, since the type is a consequence of what the target resolved to.

A **contributing series** is a `traces_service_graph_request_total` sample that resolved onto the edge's `(source, target)` pair. A contributing series is **in scope** iff it does NOT carry the dimension `edge_relation="link"`. A series carrying that dimension describes a virtual edge the producer materialised from a **span link** rather than from a paired client/server span — the interaction it records traverses a queue or a database and its two spans belong to different trace contexts — so it SHALL contribute to no rate, no error numerator, and no duration bucket. The edge itself SHALL still be emitted; only its measurement is suppressed. The label name and the value `link` are a fixed, case-sensitive contract with NO configuration surface.

Where an edge's contributing series are a mix of in-scope and link series, the reader SHALL measure it over the **in-scope subset only**. Where they are ALL link series, the in-scope set is empty, condition 4 fails, and the edge SHALL carry no metrics object at all.

The object SHALL carry the following fields:

- `rate` (number, REQUIRED whenever the object is present) — requests per second over the request window, as `rate(traces_service_graph_request_total[<window>])` summed over the edge's **in-scope contributing series** (defined below). It is strictly greater than zero by construction, because zero-rate series never produce an edge.
- `error_rate` (number, OPTIONAL) — the failed fraction in the closed interval `[0, 1]`, computed as the summed `rate(traces_service_graph_request_failed_total[<window>])` over the SAME in-scope contributing series divided by `rate`. The value SHALL be clamped to `[0, 1]`. A value of `0` SHALL mean "the failure counter was read successfully and reported no failures", never "the failure counter could not be read".
- `p90_server_ms` (number, OPTIONAL) — the 90th percentile of the **server-observed** request duration for this edge, expressed in **milliseconds**.

The quantile is `0.90` and the observation side is **server**, matching the definition used by Grafana's documented service-graph queries so the two tools measure the same thing. The values are NOT expected to equal Grafana's numerically, because Grafana aggregates by service name while this API aggregates by pod pair.

The reader SHALL NOT attach the metrics object to any other edge. In particular, an edge SHALL carry no metrics object when it is:

- an edge with an `external` node on either side (an unresolvable `"://"` connection string, the missing-UID human-label fallback, or an unresolved peer address);
- a `service-selects-pod` edge (synthesised fan-out; no series names the individual backing pod);
- the synthesised ingress-chain hop from an ingress-gateway pod to the backend service (no contributing series exists);
- the route-hit ingress chain's caller → ingress-service entry hop. That edge and the retained caller → backend edge are two projections of the SAME observed call, so measuring both would make one request contribute twice to any sum taken over the chain. The measurement SHALL be carried by the caller → backend edge, which names the actual destination;
- a topology-derived edge (`pod-mounts-pvc`, `pod-to-node`, `pvc-to-netapp-aggr`).

Numeric values SHALL NOT appear anywhere in an edge's `labels` map, which remains strictly `map[string]string`. Attaching the metrics object SHALL NOT change an edge's `id`, `type`, `source`, `target`, or `labels`.

#### Scenario: Both endpoints are UID-resolved topology pods

- **WHEN** a `traces_service_graph_request_total` series with a non-zero rate has a non-empty `client_k8s_pod_uid` and a non-empty `server_k8s_pod_uid` that both resolve to topology pods
- **THEN** the emitted `pod-calls-pod` edge carries a metrics object whose `rate` equals the series' rate value

#### Scenario: Server UID is non-empty but unknown to topology (synthesised pod)

- **WHEN** a series' `server_k8s_pod_uid` is non-empty but absent from the topology pod-UID index, so the target materialises as a synthesised pod node
- **THEN** the emitted `pod-calls-pod` edge still carries a metrics object — the endpoint is UID-resolved and the target is a `type="pod"` node

#### Scenario: Peer-resolved Pod-IP target carries metrics

- **WHEN** a series has `server="unknown"` and an empty `server_k8s_pod_uid`, and the peer-label enrichment resolves its peer address to a family Pod IP, producing a `pod-calls-pod` edge whose target is a real topology pod
- **THEN** that edge carries a metrics object — condition 2 is satisfied by the resolved node type, regardless of how the endpoint was identified

#### Scenario: Peer address resolved to a Service ClusterIP

- **WHEN** a series has `server="unknown"` and an empty `server_k8s_pod_uid`, and the peer-label enrichment matches its peer address against a Service `ClusterIP` in the client pod's own cluster, producing a `pod-calls-service` edge
- **THEN** that edge carries a metrics object, and the `service-selects-pod` edges fanned out from the same service node carry none

#### Scenario: Connection-string endpoint resolved to a service

- **WHEN** a series' server side is a `"://"` connection string that resolves to a `type="service"` node and a `pod-calls-service` edge is emitted
- **THEN** that edge carries a metrics object whose `rate` is the summed rate of the series that resolved onto it

#### Scenario: Endpoint fell back to an external node

- **WHEN** a series' server side resolved to an `external` node (missing UID with a non-`"://"` label, an unresolvable connection string, or an unresolved peer address)
- **THEN** the emitted edge carries NO metrics object

#### Scenario: Synthesised service-selects-pod fan-out edge

- **WHEN** a service node fans out `service-selects-pod` edges to its backing pods
- **THEN** none of those edges carries a metrics object

#### Scenario: Synthesised route-hit ingress-chain edge

- **WHEN** the route engine produces the ingress chain and a synthesised gateway-pod → backend-service `pod-calls-service` edge is emitted
- **THEN** that edge carries NO metrics object

#### Scenario: Route-hit chain measures the backend edge, not the entry hop

- **WHEN** the route engine resolves a peer to a backend Service and the parse emits both the caller → ingress-service entry hop and the retained caller → backend-service edge from the same contributing series
- **THEN** the caller → backend-service edge carries the metrics object and the caller → ingress-service entry hop carries none, so the observed call's rate appears exactly once across the chain

#### Scenario: Span-link series suppress measurement without dropping the edge

- **WHEN** every contributing series of a pod-to-pod pair carries `edge_relation="link"`
- **THEN** the edge is still emitted with its usual `id`, `type`, `source`, `target`, and `labels`, and it carries NO metrics object

#### Scenario: Mixed link and non-link series measure the non-link subset

- **WHEN** an edge receives one contributing series carrying `edge_relation="link"` with rate `4` and one series without that dimension with rate `1`
- **THEN** the edge's `rate` is `1`, and its `error_rate` and `p90_server_ms` are likewise derived only from the non-link series

#### Scenario: Topology-derived edges never carry metrics

- **WHEN** the response contains a `pod-mounts-pvc`, `pod-to-node`, or `pvc-to-netapp-aggr` edge
- **THEN** that edge carries NO metrics object

#### Scenario: Numeric values never enter edge labels

- **WHEN** any edge is emitted, with or without a metrics object
- **THEN** its `labels` map contains no `rate`, `error_rate`, or `p90_server_ms` key, and every `labels` value is a string

### Requirement: RED source series and selector consistency

In addition to `traces_service_graph_request_total`, the reader SHALL read two further service-graph series for the same window and the same evaluation instant:

- `traces_service_graph_request_failed_total` — the Errors counter, read at the same label granularity as the total counter so it can be joined to the exact same series.
- `traces_service_graph_request_server_seconds_bucket` — the Duration classic histogram, also read at the same label granularity as the total counter, additionally carrying the `le` bucket boundary. The reader SHALL NOT aggregate it upstream: no low-cardinality label subset identifies an edge once endpoints may be resolved from peer addresses and connection strings, so a group-by would merge the latency distributions of unrelated edges that happen to share the retained labels.

The reader SHALL apply the SAME virtual-sentinel exclusion fragment to both new selectors as it applies to `traces_service_graph_request_total` (see "Virtual sentinel endpoint exclusion (user / unknown)"), so that the three series always describe the same edge population.

Both new selectors SHALL additionally exclude series carrying `edge_relation="link"`, mirroring the out-of-scope rule in the attachment requirement above. The `traces_service_graph_request_total` selector SHALL NOT carry that matcher — the span-link edge must still be emitted. Because a negative label matcher retains series on which the label is absent, the exclusion is inert for a producer that does not configure the dimension.

Both matchers are fixed, request-invariant metric-selection contracts — the same class as the sentinel exclusion and the node-condition selector, NOT caller-supplied filters — and SHALL NOT vary per request.

The queried series population is a **superset** of the attachment population, because the attachment rule's endpoint-node-type and chain-position conditions have no label-level equivalent. The reader SHALL guarantee the direction that matters instead: **every** query-layer exclusion applied to the two new selectors SHALL also be enforced during resolution, so a qualifying edge can never have its failure or duration series filtered away upstream and then be reported as `error_rate: 0` or as lacking a histogram. Series returned for a pair that turns out ineligible SHALL simply go unused.

The configurable upstream metric-name prefix SHALL NOT be applied to either new series (they belong to the trace-pipeline exporter family, not to kube-state-metrics). The reader SHALL NOT read `traces_service_graph_request_client_seconds_*`, nor the `_sum` / `_count` companions of the server histogram.

#### Scenario: Sentinel peers are excluded from the RED series too

- **WHEN** the reader issues the failure-counter and duration-histogram queries
- **THEN** both selectors carry the identical sentinel-exclusion matcher fragment used by the request-total selector, so a sentinel peer contributes to none of the three

#### Scenario: Span-link series never reach the RED join

- **WHEN** a service-graph series carries `edge_relation="link"`
- **THEN** it is excluded from the failure-counter and duration-histogram results at the query layer, and the reader independently marks it out of scope during resolution, so the two layers agree

#### Scenario: Series without a pod UID still reach the RED join

- **WHEN** a service-graph series has an empty `server_k8s_pod_uid` and a `server="unknown"` peer address that resolves to a pod or a service
- **THEN** its failure and duration series are returned by both new queries and joined to the resulting edge, because neither selector filters on the pod-UID labels

#### Scenario: Duration histogram is not aggregated upstream

- **WHEN** the reader issues the duration-histogram query
- **THEN** the query carries no `sum by (...)` aggregation, and each returned series retains the full dimension set of the corresponding request-total series plus `le`

#### Scenario: Metric prefix is not applied to the RED series

- **WHEN** an operator configures a non-empty upstream metric-name prefix
- **THEN** the failure-counter and duration-histogram queries still address the unprefixed `traces_service_graph_request_failed_total` and `traces_service_graph_request_server_seconds_bucket` names

### Requirement: RED join and deterministic aggregation

Several upstream series legitimately resolve to a single edge (most commonly when a dimension the edge identity does not carry, such as `connection_type`, differs, or when two series carry different `cluster` labels that resolve to the same client pod). The reader SHALL aggregate over the edge's **in-scope contributing series** (those not carrying `edge_relation="link"`) and no other:

- `rate` SHALL be the SUM of the in-scope contributing series' rates. An out-of-scope series SHALL NOT be counted, so that the Rate denominator, the Errors numerator, and the Duration buckets are all derived from one identical series set.
- Both companion vectors SHALL be joined to an in-scope contributing series by that series' **exact label identity** — all labels except the metric name, and for the duration histogram except `le` as well. The reader SHALL record that mapping while resolving the request-total vector and SHALL NOT re-derive an edge's endpoints from labels in a second pass.
- Where several in-scope contributing series map to one edge, their matched bucket counts SHALL be summed per `le` boundary before any quantile is taken.
- `p90_server_ms` SHALL be computed from the **summed** bucket set, using the classic cumulative-bucket convention with linear interpolation inside the bucket that contains the 90th percentile (the same algorithm as a PromQL `histogram_quantile(0.9, ...)`), then converted from seconds to milliseconds. The reader SHALL NOT compute per-series quantiles and then average or otherwise combine them.

Every attached value SHALL be a pure function of the upstream data and SHALL NOT depend on the arrival order of series within a query result. To that end the reader SHALL make its summation order-independent and SHALL round each attached value before serialisation, so that two builds over identical upstream data produce byte-identical response bodies (see the `graph-api` "Deterministic response body" requirement).

Rounding SHALL be to a fixed number of **significant digits**, applied identically to all three fields, and SHALL NOT be to a fixed number of decimal places. A non-zero value SHALL NEVER round to `0`: `rate` is a per-second value whose magnitude scales inversely with the caller's window, and `error_rate` can be legitimately tiny on a high-traffic edge, so an absolute precision floor would contradict this capability's guarantee that an emitted `rate` is strictly greater than zero, and would collide with the defined meaning of `error_rate == 0` ("read successfully, no failures").

#### Scenario: Two series collapse into one edge

- **WHEN** two `traces_service_graph_request_total` series differing only in `connection_type`, both carrying both pod UIDs, resolve to the same pod-to-pod edge with rates `2` and `3`
- **THEN** the edge's `rate` is `5`

#### Scenario: UID-resolved and peer-resolved series collapse into one measured edge

- **WHEN** an edge receives contributions from one series carrying both pod UIDs (rate `4`) and one `server="unknown"` series with an empty `server_k8s_pod_uid` whose peer address resolves to the same target pod (rate `1`)
- **THEN** the edge's `rate` is `5` — both series are in scope, since eligibility depends on the resolved node type and not on how the endpoint was identified

#### Scenario: p90 joins by full series identity minus `le`

- **WHEN** two in-scope contributing series differing only in `connection_type` resolve to one edge, and each has its own set of duration-histogram series
- **THEN** each histogram series is joined to its own contributing series by the label set it shares with it, and the edge's `p90_server_ms` is computed from the per-`le` sum of both bucket sets

#### Scenario: Error rate uses the matching series set

- **WHEN** two in-scope series resolve to one edge with total rates `2` and `3`, and only the first has a matching failure series with rate `1`
- **THEN** the edge's `error_rate` is `0.2`

#### Scenario: p90 is computed from summed buckets

- **WHEN** two in-scope contributing series map to one edge and each has its own duration histogram
- **THEN** the edge's `p90_server_ms` is derived from the per-`le` sum of both bucket sets, not from combining two per-series quantiles

#### Scenario: Attached values are order-independent

- **WHEN** the same upstream data is returned twice with the contributing series in different orders
- **THEN** the two responses are byte-identical, including every attached numeric value

#### Scenario: A very small non-zero value never rounds to zero

- **WHEN** an edge's window is long enough (or its failure count rare enough) that `rate` or `error_rate` evaluates to a value far below one part in a million — for example a single request over a 30-day window
- **THEN** the emitted value is a non-zero JSON number carrying the configured number of significant digits, and is not `0`

#### Scenario: Quantile above the highest finite bucket

- **WHEN** the 90th percentile falls into the `+Inf` bucket
- **THEN** `p90_server_ms` is the highest finite `le` boundary converted to milliseconds

### Requirement: RED graceful degradation

Neither new query SHALL be able to fail a build. The reader SHALL degrade as follows and SHALL continue to emit the graph:

- The failure-counter query returns an error, or the metric does not exist upstream: the reader SHALL OMIT `error_rate` from every edge's metrics object rather than reporting `0`, so an absent measurement is never presented as a measured absence of errors.
- The failure-counter query succeeds but has no series matching a given edge: that edge's `error_rate` SHALL be `0`.
- The duration-histogram query returns an error, the metric does not exist upstream, or its series carry no classic `le` bucket boundaries (for example because the producer emits native histograms, which a store may expose as `vmrange` buckets): the reader SHALL OMIT `p90_server_ms` from every affected edge's metrics object.
- Both new queries fail or return nothing: qualifying pod-to-pod edges SHALL still carry a metrics object containing only `rate`.
- The existing `traces_service_graph_request_total` query retains its current behaviour: an error there still fails the build.

A **non-finite upstream sample SHALL NOT reach the response**. Upstream exposition formats accept `NaN` and `+Inf`, and JSON has no representation for either, so a single poisoned sample would otherwise make the whole response body unencodable and turn one bad series into a failed request for the entire graph. The reader SHALL therefore: skip non-finite samples when accumulating; omit the metrics object entirely when an edge's aggregated `rate` is not a finite value greater than zero; and omit `p90_server_ms` when the computed quantile is not finite. Degrading one edge is always preferred to failing the request.

The reader SHALL additionally detect, **for each companion vector independently**, the case where it was read successfully but **joined to nothing**: a non-empty result none of whose series matched any edge. For the failure counter that would make every edge report `error_rate: 0`, indistinguishable from a measured absence of failures; for the duration histogram it would omit `p90_server_ms` everywhere, indistinguishable from a producer that emits no histogram. The reader SHALL surface each signature as aggregated operator evidence naming its own distinct reason, so a label-set divergence between the request counter and either companion is diagnosable rather than silently reading clean.

Each degradation SHALL be surfaced as aggregated operator evidence in the logs, naming the query and the reason, not per edge, and SHALL NOT alter any node, edge, `id`, `type`, `source`, `target`, or `labels`.

#### Scenario: Failure counter absent upstream

- **WHEN** `traces_service_graph_request_failed_total` does not exist in the upstream store
- **THEN** the build succeeds and qualifying edges carry a metrics object with `rate` (and `p90_server_ms` when available) but no `error_rate` key

#### Scenario: Histogram disabled at the producer

- **WHEN** `traces_service_graph_request_server_seconds_bucket` returns no series
- **THEN** the build succeeds and qualifying edges carry a metrics object with `rate` and `error_rate` but no `p90_server_ms` key

#### Scenario: Histogram carries no classic bucket boundaries

- **WHEN** the duration-histogram query returns series that carry no `le` label
- **THEN** the build succeeds, `p90_server_ms` is omitted from every edge, and the reason is logged as aggregated evidence distinguishable from "metric absent"

#### Scenario: Both RED queries error

- **WHEN** both the failure-counter and the duration-histogram queries return an upstream error
- **THEN** the build still succeeds, the response is unchanged apart from qualifying edges carrying a metrics object with only `rate`, and the failures are logged as aggregated evidence

#### Scenario: Non-finite rate degrades one edge, not the request

- **WHEN** an otherwise qualifying edge's contributing series carry a `+Inf` or `NaN` value
- **THEN** that edge carries no metrics object, every other edge is unaffected, and the response body remains JSON-encodable

#### Scenario: Failure counter read successfully but joined nothing

- **WHEN** the failure-counter query returns a non-empty result and none of its series matches any edge's contributing series identity
- **THEN** the build succeeds and the condition is logged once as aggregated evidence under its own reason, so the resulting `error_rate: 0` on every edge is diagnosable as a join failure rather than read as a measured absence of failures

#### Scenario: Duration histogram read successfully but joined nothing

- **WHEN** the duration-histogram query returns a non-empty result and none of its series matches any edge's contributing series identity minus `le`
- **THEN** the build succeeds, `p90_server_ms` is absent everywhere, and the condition is logged once as aggregated evidence under its own reason, distinguishable from "metric absent" and from "no usable `le` boundaries"

#### Scenario: Request-total query error still fails the build

- **WHEN** the `traces_service_graph_request_total` query returns an upstream error
- **THEN** the build fails as it does today — the graceful degradation applies only to the two new queries

### Requirement: Span-link logical edge relation marking

The reader SHALL read the `edge_relation` label of every
`traces_service_graph_request_total` series. A series whose value is exactly
`"link"` (a span-link-derived logical edge: client = producer pod, server =
consumer pod, joined across trace IDs through a broker) SHALL resolve both
endpoints through the ordinary resolution ladder unchanged, and the resulting
edge SHALL carry `labels.relation = "link"`. Any other `edge_relation` value
(absent, empty, or an unrecognised string) SHALL be ignored — the series is
processed exactly as before this requirement, and its edge carries no
`relation` key.

For each side of a `"link"` series that resolved to a **real topology pod**,
the reader SHALL additionally derive that side's broker (via) node ID from the
side's own client-recorded peer-address labels — the client side from
`client_server_address` / `client_net_peer_name` (with `client_dns_answers` /
`client_server_port` as route-resolution dimensions), the server side from the
mirrored `server_server_address` / `server_net_peer_name` (with
`server_dns_answers` / `server_server_port`) — using the SAME classification
chain as the unknown-server peer-label enrichment (bracket truncation and
port split, Kubernetes `.svc` DNS grammar, bare short name in the pod's own
namespace, anchor-cluster ClusterIP, family Pod IP, prefetched route-engine
index, raw-value external fallback), anchored on that side's own pod cluster.
The pair `(that side's pod, derived broker node ID)` SHALL be recorded as a
**transport** pair. A side with no peer-address value, or a side that did not
resolve to a real topology pod, contributes no transport pair; the other side
is unaffected.

Via derivation SHALL be **lookup-only**: it MUST NOT materialise any service,
external, or synthesised node, MUST NOT emit any `service-selects-pod` or
route-chain edge, and MUST NOT log route-engine external-fallback reasons.
For a route-index entry whose outcome is a routed hit, the derived ID is the
BACKEND service ID only (never the ingress entry-point), with no `role`
marking and no chain synthesis; every other index outcome, a missing entry,
absent destination IPs, or an unconfigured engine derives the external node ID
of the raw peer-address value — identical to the ID the materialising path
would have produced for the same series.

At edge-build time, an emitted `pod-calls-pod` or `pod-calls-service` edge
whose `(source, target)` pair was recorded as a link pair SHALL carry
`labels.relation = "link"`; otherwise, if recorded as a transport pair, it
SHALL carry `labels.relation = "transport"`; otherwise no `relation` key.
`link` SHALL take precedence over `transport` for the same pair, and over any
plain series that aggregated into the same edge. `service-selects-pod` edges
and synthesized route-chain edges SHALL never carry a `relation` key. A
transport pair with no matching emitted edge is a no-op (the reader MAY log
one aggregated debug line); the reader MUST NOT synthesise an edge for it.
Marking SHALL be a pure function of the accumulated pair sets — independent of
sample arrival order — and MUST NOT alter edge identity (the UUIDv5 input
remains `type|source|target`).

A `"link"` series whose raw `server` label is exactly `"unknown"` AND whose
server side has no resolvable topology pod recovered no consumer: it SHALL
contribute **no relation markers at all** — its pair is recorded as neither
link (the resolved target is the broker, not the consumer) nor transport, and
no per-side via pair is recorded from it. Its server side still resolves
through the unknown-server peer-label enrichment exactly as an ordinary
`server="unknown"` series would, so the producer → broker edge it produces is
byte-identical to the enrichment's ordinary outcome (an unmarked network
edge). Rationale: the frontend contract is "transport = the network hop
backing a rendered logical edge"; a transport marker with no accompanying
link edge would demote a real network dependency to a dashed line that backs
nothing. Consequently, transport pairs SHALL be recorded ONLY by series that
also record their link pair — a transport-marked edge in the built graph
always coexists with at least one link-marked edge originating from the same
series set (projection filters MAY still exclude either edge from a filtered
view; marking itself never depends on the projection). A `"link"` series
whose server side degrades to a synthesised pod or to the missing-UID
external fallback keeps its **link** marking (the logical claim stands even
when an endpoint is degraded).

The prescan SHALL collect route keys for both sides' peer labels of `"link"`
series (each side only when its own pod resolved, anchored on that pod's
cluster, subject to the same skip chain as the unknown-server collection:
in-cluster-resolvable and IP-less endpoints are not collected), deduplicated
with the keys ordinary `server="unknown"` series derive — a broker FQDN shared
by link and non-link series in one anchor cluster SHALL cost at most ONE
route-engine store read per build.

#### Scenario: Link series between two resolved pods

- **WHEN** a series with `edge_relation="link"` carries client and server pod
  UIDs that both resolve to topology pods
- **THEN** one `pod-calls-pod` edge is emitted from producer to consumer with
  `labels.relation = "link"`

#### Scenario: Link wins over a plain series for the same pair

- **WHEN** a plain series and an `edge_relation="link"` series resolve to the
  same `(source, target)` pair, in either ingestion order
- **THEN** the single aggregated edge carries `labels.relation = "link"`

#### Scenario: Transport marking of the existing broker edge

- **WHEN** a `"link"` series' client side resolves to a real topology pod and
  its `client_server_address` classifies to a Service the pod's own cluster
  holds, AND a plain series already produces the pod → that-Service edge
- **THEN** that `pod-calls-service` edge carries `labels.relation =
  "transport"`, and the link edge itself stays `"link"`

#### Scenario: Link beats transport on pair collision

- **WHEN** the same `(source, target)` pair is recorded both as a link pair
  and as a transport pair within one build
- **THEN** the emitted edge carries `labels.relation = "link"`

#### Scenario: server="unknown" link series contributes no markers

- **WHEN** a `"link"` series has `server="unknown"`, no resolvable server pod,
  and a client that resolved to a real topology pod with a classifiable peer
  address
- **THEN** the emitted producer → broker edge carries NO `relation` key —
  byte-identical to the edge an ordinary `server="unknown"` series produces,
  including when a plain series merges into the same pair — and the series
  records neither a link pair nor any transport pair

#### Scenario: A transport-marked edge always accompanies a link-marked edge

- **WHEN** any build's emitted edge carries `labels.relation = "transport"`
- **THEN** the same built graph contains at least one edge carrying
  `labels.relation = "link"` recorded by the same series set (a transport
  marker can only originate from a series that also recorded its link pair)

#### Scenario: Degraded server endpoints keep the link marking

- **WHEN** a `"link"` series' server side degrades to a synthesised pod
  (unknown UID) or to the missing-UID external fallback (non-`"unknown"`
  label)
- **THEN** the emitted edge still carries `labels.relation = "link"`

#### Scenario: Per-side independence of via marking

- **WHEN** a `"link"` series' client side does not resolve to a real topology
  pod but its server side does, and the server side carries
  `server_server_address`
- **THEN** no client-side transport pair is recorded, and the server-side
  pair `(consumer pod, broker node)` is still recorded as transport

#### Scenario: Via lookup never materialises

- **WHEN** a build's ONLY series are `"link"` series (no plain network
  series), with peer-address labels that would classify to Services,
  externals, or route-engine destinations
- **THEN** the response contains no service node, no external node, no
  `service-selects-pod` edge, and no route-chain edge attributable to via
  derivation — the unmatched transport pairs are marker-only

#### Scenario: Fan-out edges are never marked

- **WHEN** a broker Service node materialises via a plain series and fans out
  `service-selects-pod` edges, while link/transport marking touches edges into
  that Service
- **THEN** no `service-selects-pod` edge carries a `relation` key

#### Scenario: Self-loop link series

- **WHEN** a `"link"` series carries the same resolvable pod UID on both
  sides with no `"://"` label (the D33 guard does not fire)
- **THEN** one self-loop `pod-calls-pod` edge is emitted with
  `labels.relation = "link"`

#### Scenario: Non-link edge_relation values are ignored

- **WHEN** a series carries `edge_relation="database"` (or any value other
  than `"link"`)
- **THEN** it is processed as an ordinary series and its edge carries no
  `relation` key

#### Scenario: Shared broker FQDN resolves through one route-engine read

- **WHEN** several series in one anchor cluster — link series (either side)
  and ordinary `server="unknown"` series — carry the same unclassifiable
  broker FQDN with the same destination IPs and port
- **THEN** the route-resolution engine is consulted exactly once for that key,
  and every dependent edge/marker derives from the same prefetched answer

### Requirement: Ingress route-path marking

The two shapes emitted by a routed global-FQDN hit ("Full ingress chain for routed global-FQDN
hits") — the ingress chain and the direct `caller pod → backend service` edge — SHALL be
distinguishable in the emitted graph without traversal, so a consumer can render or toggle the
gateway path independently of the direct dependency.

**Ingress node marker.** A `service` node materialised as an ingress entry point SHALL carry a
`role` key on its `labels`, with exactly one of two values:

- `ingress-gateway` — the entry hop of a routed hit's chain; gateway pods and a synthesized
  `pod-calls-service` hop to the backend exist behind it;
- `ingress-lb` — the destination of the "Ingress LB Service fallback" (no routed backend behind
  it).

The key SHALL be absent (not empty-valued) on every `service` node that is not an ingress entry
point, and the node SHALL remain `type="service"` — no new node type is introduced. The
synthesized `ingress gateway pod → backend service` hop SHALL remain typed `pod-calls-service`
(no dedicated edge type).

**Determinism.** Marking SHALL be monotone and order-free. When one Service is materialised as
both a chain entry hop and an LB-fallback destination within a single build, the resulting value
SHALL be `ingress-gateway` regardless of the order in which the upstream series are resolved
(`ingress-gateway` overwrites; `ingress-lb` is written only into an unset value). Marking SHALL be
idempotent under repeated series that share one route key, and SHALL never clear or downgrade a
value.

**Degrade invariance.** Marking SHALL occur only after an ingress `service` node has been
successfully materialised. Every chain-precondition degrade of the parent requirement
materialises no ingress node and SHALL therefore produce no marker, leaving the degraded output
identical to the pre-existing direct-edge shape. The direct `caller pod → backend service` edge
SHALL be emitted unchanged — it carries no additional label. No resolution behaviour,
precondition, external fallback, external-fallback reason, engine outcome, upstream query, or
store read is altered by this requirement, and output with route resolution disabled SHALL be
unchanged.

#### Scenario: Chained routed hit marks its ingress node

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint resolves to a routed hit
  whose chain preconditions all hold
- **THEN** the ingress `service` node SHALL carry `labels.role = "ingress-gateway"` (and keep
  `type="service"`)
- **AND** each synthesized edge from an ingress gateway pod to the backend `service` node SHALL
  have type `pod-calls-service`, carrying the ingress cluster as its `cluster` label
- **AND** the `caller pod → ingress service` edge, the `caller pod → backend service` direct edge,
  and both `service-selects-pod` fan-outs SHALL be emitted exactly as before, the direct edge
  carrying no additional label

#### Scenario: Ingress LB Service fallback marks its node distinguishably

- **WHEN** the Istio pipeline misses with no Gateway and the destination IPs resolve to a unique
  ingress LB Service ("Ingress LB Service fallback")
- **THEN** the materialised `service` node SHALL carry `labels.role = "ingress-lb"`

#### Scenario: One Service resolved by both paths is marked deterministically

- **WHEN** within a single build one endpoint resolves a chained hit whose entry hop is a given
  Service, and another endpoint's resolution falls back to that same Service as its ingress LB
  Service
- **THEN** that single `service` node SHALL carry `labels.role = "ingress-gateway"` irrespective
  of the order in which the two endpoints are resolved

#### Scenario: Degraded chain emits no marker

- **WHEN** a routed hit's chain degrades for any precondition reason (no unique ingress identity,
  ingress identity equal to the backend identity, selected cluster's topology lacking the ingress
  Service, or the ingress Service having no backing pod in the selected cluster)
- **THEN** no `service` node SHALL carry a `role` label for that endpoint
- **AND** the emitted nodes and edges SHALL be identical to the pre-existing direct-edge shape

#### Scenario: Backend service node is never marked

- **WHEN** a routed hit resolves a backend `service` node that is not itself an ingress entry
  point
- **THEN** that node's `labels` SHALL NOT contain a `role` key

### Requirement: Optional env-only ClickHouse credentials for the route store

When route resolution is enabled (`KSG_ROUTE_STORE_DSN` / `--route-store-dsn` non-empty), the server SHALL accept optional ClickHouse native auth credentials from `KSG_ROUTE_STORE_USERNAME` and `KSG_ROUTE_STORE_PASSWORD` only. The server MUST NOT register CLI flags for these credentials. Both env vars MUST be set together or both left unset; a half-configured pair MUST fail startup with an error that names both env vars and MUST NOT echo their values. When both are set, dial SHALL use those credentials (overriding any userinfo embedded in the DSN). When both are unset, DSN-embedded credentials SHALL continue to work. Credential values MUST NEVER appear in logs, spans, metric labels, or error messages — startup MAY log only a boolean indicating whether route-store auth is configured.

#### Scenario: Env credentials dial successfully

- **WHEN** `KSG_ROUTE_STORE_DSN` is a credential-free ClickHouse URL and `KSG_ROUTE_STORE_USERNAME` / `KSG_ROUTE_STORE_PASSWORD` are both set to valid credentials for that server
- **THEN** the process starts and the route store connection is established

#### Scenario: Half-configured credentials rejected at startup

- **WHEN** exactly one of `KSG_ROUTE_STORE_USERNAME` or `KSG_ROUTE_STORE_PASSWORD` is set
- **THEN** startup fails with an error naming both env vars and not containing either configured value

#### Scenario: DSN-embedded credentials remain valid when env unset

- **WHEN** `KSG_ROUTE_STORE_DSN` embeds `user:pass` and both route-store auth env vars are unset
- **THEN** the route store dials using the DSN userinfo (backward compatible)

### Requirement: Route-resolution snapshot integrity

The configuration state the route-resolution engine translates SHALL name each resource
version exactly once, regardless of how many destination IPs the request carries.

When the request carries several destination IPs, the engine loads the selected ingress
cluster's configuration once per IP and unions the results. Two IPs served by the same
ingress Service — a dual-stack Service publishing an IPv4 and an IPv6 address, or several
addresses of one load balancer — return overlapping rows. The union SHALL deduplicate by
resource identity (cluster, namespace, name, and version start), and the per-gateway
translate input SHALL contain at most one configuration entry per `(kind, namespace, name)`
and at most one registry entry per backend Service.

A multi-IP request SHALL therefore produce the same outcome a single-IP request against
the same configuration produces. A duplicated resource SHALL NOT surface as a translation
error, and SHALL NOT cause the endpoint to fall back to an external node.

#### Scenario: Dual-stack ingress resolves

- **GIVEN** an ingress Service whose ingress addresses include both an IPv4 and an IPv6 address
- **AND** a Gateway and VirtualService serving the requested host
- **WHEN** the endpoint's destination IPs contain both addresses
- **THEN** the engine resolves the routed backend Service, identically to a request carrying either address alone

#### Scenario: Overlapping loads do not error

- **GIVEN** two destination IPs whose configuration loads return the same VirtualService version
- **WHEN** the engine builds the translate input for the selected gateway
- **THEN** that VirtualService appears exactly once and translation succeeds

### Requirement: Backend destination host resolution

The engine SHALL resolve a VirtualService route's `destination.host` to a backend Service
identity using the same rules istiod itself applies, so that the destination the engine
reports is the destination a real mesh would route to.

A `destination.host` containing no dot is a short name and SHALL be resolved within the
owning VirtualService's namespace to the in-cluster Service FQDN
(`<name>.<namespace>.svc.<cluster domain>`). The resolved identity SHALL be used
consistently for the backend Service lookup, the translation registry, and the parse of
the resulting Envoy cluster string.

A `destination.host` containing a dot SHALL NOT be expanded — istiod treats it as already
qualified. A dotted value that is not a full in-cluster Service FQDN (for example
`checkout.shop` or `checkout.shop.svc`) therefore names no Service in the registry; the
engine SHALL NOT infer a Service identity from it, and the endpoint SHALL fall back to an
external node as it does for any unresolvable destination.

An Envoy cluster string's host SHALL be parsed as a Service identity only when it is a
well-formed `<name>.<namespace>.svc.<cluster domain>` with exactly two leading labels; any
other shape SHALL be treated as unresolvable rather than parsed by prefix.

#### Scenario: Bare short destination host resolves

- **GIVEN** a VirtualService in namespace `shop` routing to `destination: {host: checkout}`
- **AND** a Service `checkout` in namespace `shop`
- **WHEN** the engine resolves a request routed by that VirtualService
- **THEN** the destination is the Service `checkout` in namespace `shop`

#### Scenario: Dotted relative destination host does not resolve

- **GIVEN** a VirtualService routing to `destination: {host: checkout.shop}`
- **WHEN** the engine resolves a request routed by that VirtualService
- **THEN** no Service identity is inferred and the endpoint falls back to an external node

#### Scenario: Per-pod headless host is not a Service identity

- **WHEN** an Envoy cluster string names the host `mysql-0.mysql.db.svc.cluster.local`
- **THEN** it is treated as unresolvable rather than parsed as the Service `mysql-0` in namespace `mysql`

### Requirement: Deterministic ingress 3-hop selection

The IP-to-Gateway 3-hop SHALL be a pure function of the configuration state at the
resolution instant, independent of the order in which the store returns rows.

**Hop 1 (IP to ingress Service).** When more than one distinct Service identity is live at
the instant carrying the destination IP, the hop SHALL degrade rather than select one:
no candidate gateways are produced, matching the ambiguity rule the ingress LB Service
identity dedup already applies to the same situation. A single identity with several
versions is not an ambiguity — version liveness resolves it before this rule applies.

**Hop 2 (ingress Service selector to workload labels).** When more than one ingress
Deployment satisfies the Service selector — the normal state during a revision-based
canary gateway upgrade — the hop SHALL take the **union** of their pod labels, matching
the label union the store query itself computes. It SHALL NOT select one Deployment.

**Gateway identity.** A candidate Gateway SHALL be identified by `(namespace, name)`
throughout resolution, including when its configuration and bound VirtualServices are
rebuilt for translation. A same-named Gateway in another namespace present in the loaded
rows SHALL NOT be selectable, and SHALL NOT contribute its bound VirtualServices to the
translated configuration.

#### Scenario: Same-named gateway in another namespace is never translated

- **GIVEN** live Gateways `istio-system/public-gw` and `nginx-ingress/public-gw` in the loaded rows
- **AND** the ingress Service selecting the candidate lives in `istio-system`
- **WHEN** the engine rebuilds the gateway's configuration for translation
- **THEN** only `istio-system/public-gw` and the VirtualServices bound to it are translated

#### Scenario: Canary gateway Deployment does not change the candidate set

- **GIVEN** two live ingress Deployments whose pod labels both satisfy the ingress Service selector
- **WHEN** the engine derives candidate Gateways
- **THEN** the candidate set is derived from the union of both Deployments' pod labels and does not depend on row order

#### Scenario: Ambiguous ingress Service identity degrades

- **GIVEN** two distinct Service identities live at the instant carrying the destination IP
- **WHEN** the engine runs the 3-hop for that IP
- **THEN** no candidate gateway is produced

### Requirement: Bounded route-resolution work per build

Route resolution SHALL NOT let its per-build cost grow without bound on the request path.

Within one build the deduplicated route keys SHALL be resolved with bounded concurrency,
and the number of keys resolved SHALL be capped. When the cap truncates the key set, the
reader SHALL log the number of dropped keys; truncation SHALL NOT be silent. Dropped keys
fall back to their pre-existing external-node behaviour, exactly as an unanswered key does
when the build deadline fires.

Concurrent resolution SHALL NOT change the resolved index's contents: each key's answer is
independent of every other key's. Any per-build memoisation shared across concurrent
resolutions SHALL be safe for concurrent use.

#### Scenario: Independent keys resolve concurrently

- **GIVEN** several distinct route keys collected from one service-graph vector
- **WHEN** the reader resolves them
- **THEN** the resulting index is identical to the index a strictly serial resolution produces

#### Scenario: Truncation is reported

- **GIVEN** a build whose collected route keys exceed the cap
- **WHEN** the reader resolves them
- **THEN** the number of dropped keys is logged and the corresponding endpoints fall back to external nodes

### Requirement: Resolution errors carry no routing outcome

When route resolution fails with an error — a store read failure, a translation failure,
or a matcher failure — the engine SHALL NOT also report a routing outcome that is
meaningful to the caller. In particular it SHALL NOT report the outcome that gates the
ingress LB Service fallback, so that an infrastructure failure can never be mistaken for
"no Istio Gateway serves this host".

The matcher's per-query results SHALL be recovered for every query posed or reported as an
error; a partially recovered result set SHALL NOT be returned.

#### Scenario: Store failure is not reported as a missing gateway

- **GIVEN** the route store fails a read
- **WHEN** the engine resolves an endpoint
- **THEN** the failure is reported as an error with no routing outcome, and the endpoint falls back to an external node

#### Scenario: Incomplete matcher output errors

- **GIVEN** the matcher's output yields fewer results than the number of queries posed
- **WHEN** the engine parses that output
- **THEN** the parse reports an error rather than returning a result for any query

### Requirement: Filtered-build admission and out-of-scope endpoints

When any request-scoped selector dimension (`az`, `env`, `cluster`, `namespace`) is active, the topology is a subset of the estate while the service-graph series are complete. The reader SHALL therefore apply the following rules to every series that survives the sentinel exclusion and the wholly-empty-side drop; none of them applies to an unfiltered build, whose output is unchanged.

1. **Admission.** A series SHALL be admitted only if **at least one** of its two endpoints resolves to **loaded topology** — a pod present in the topology, or a Service present in the loaded service index (via connection-string resolution, the unknown-server peer ladder, or route resolution). A series whose endpoints both fail to reach loaded topology SHALL be dropped before any node or edge is materialised — whatever the unresolved sides would otherwise have become. This keeps the out-of-scope estate from rendering as a web of `external` nodes and bounds the output to the in-scope workload's direct neighbourhood.
2. **Out-of-scope endpoint becomes external.** For an admitted series, an endpoint that does not reach loaded topology SHALL resolve to `external/<raw label>` with `labels={}`: a non-empty pod UID that is not loaded falls to that side's `client` / `server` human label exactly as the missing-UID human-label fallback does (`external/cart`); a `"://"` connection string whose Service is not loaded keeps the verbatim label; a peer address that matches no loaded Service `ClusterIP` or Pod IP keeps the raw peer value. A side whose UID is not loaded AND whose human label is empty SHALL be dropped together with its series, as a wholly empty side is today.
3. **No synthesised pods.** A filtered build SHALL NOT create a synthesised pod node for any endpoint.
4. **Two-phase materialisation.** Service nodes, `service-selects-pod` fan-out edges, route-chain edges, and external nodes SHALL be materialised only for admitted series; resolution of a series that is later dropped SHALL leave no node or edge behind.
5. **Unchanged conventions.** An edge with an `external` endpoint carries no `labels.cluster` when the external is the client side and never carries `metrics`; the unknown-server peer-label enrichment still requires the client to be a loaded topology pod (an out-of-scope client with `server="unknown"` is dropped, as today); same-named out-of-scope peers collapse into one external node; an out-of-scope caller of a loaded pod or Service appears as an inbound external.

The rules are a pure function of the series set and the loaded topology (order-free; deterministic).

#### Scenario: Cross-namespace call renders the peer as external

- **WHEN** a build with `namespace={shop}` sees a series from loaded pod `shop/checkout` (`client_k8s_pod_uid="c1"`) to `server="cart"`, `server_k8s_pod_uid="s1"`, where `s1` lives in namespace `payments` and is not loaded
- **THEN** the reader emits one `pod-calls-pod` edge from `<cluster>/c1` to `external/cart`; `external/cart` has `type: "external"`, `name: "cart"`, `labels: {}`; no pod node exists for `s1`

#### Scenario: Series between two out-of-scope pods is dropped

- **WHEN** a build with `namespace={shop}` sees a series whose client UID and server UID both belong to `payments` pods (neither loaded)
- **THEN** no edge and no node is produced for the series — in particular no `external/<client>` → `external/<server>` edge

#### Scenario: Inbound call from an out-of-scope caller

- **WHEN** a build with `namespace={shop}` sees a series from `client="frontend"`, `client_k8s_pod_uid="f1"` (a `web` namespace pod, not loaded) to loaded pod `shop/checkout`
- **THEN** the reader emits one `pod-calls-pod` edge from `external/frontend` to `<cluster>/<checkout-uid>` with no `labels.cluster` and no `metrics`

#### Scenario: Out-of-scope Service via connection string

- **WHEN** a build with `namespace={shop}` sees a series from a loaded `shop` pod whose `server` label is `http://cart.payments.svc.cluster.local:8080` with an empty server UID, and `payments/cart` is not in the loaded service index
- **THEN** the reader emits a `pod-calls-pod` edge to `external/http://cart.payments.svc.cluster.local:8080` with `labels={}`; no service node is materialised

#### Scenario: In-scope Service called by an out-of-scope pod is materialised with its fan-out

- **WHEN** a build with `namespace={shop}` sees a series from an unloaded `web` pod (`client="frontend"`) whose `server` label is `http://api.shop.svc.cluster.local` and `shop/api` is loaded with backing pods in `shop`
- **THEN** the reader admits the series (the server reaches loaded topology), materialises `<cluster>/shop/api` with its `service-selects-pod` edges to the loaded backing pods, and emits a `pod-calls-service` edge from `external/frontend` to the service node

#### Scenario: Peer IP of an out-of-scope pod becomes an external IP node

- **WHEN** a build with `namespace={shop}` sees a series from a loaded `shop` pod with `server="unknown"`, empty server UID, and `client_network_peer_address="10.1.2.3"`, where `10.1.2.3` is the Pod IP of an unloaded `payments` pod
- **THEN** the endpoint resolves to `external/10.1.2.3` (the ClusterIP and Pod-IP lookups see only loaded topology) and the edge is emitted to it

#### Scenario: Dropped series leaves no trace

- **WHEN** a build with `cluster={cluster-alpha}` sees a series from an unloaded `cluster-beta` pod to `http://cart.payments.svc.cluster.local` where `payments/cart` is also not loaded
- **THEN** the series is dropped and the built graph contains neither an `external/cart…` node nor any edge for it

#### Scenario: Cross-cluster partner under a cluster filter

- **WHEN** a build with `cluster={cluster-alpha}` sees a series from a loaded `cluster-alpha` pod to `server="cart"`, `server_k8s_pod_uid="s9"` where `s9` is a `cluster-beta` pod
- **THEN** the reader emits a `pod-calls-pod` edge to `external/cart` with `labels.cluster: "cluster-alpha"`; no `cluster-beta` pod node is synthesised or loaded

#### Scenario: Unfiltered build keeps the synthesised-pod behaviour

- **WHEN** a build with no request-scoped dimension sees a series whose server UID is absent from topology
- **THEN** the existing "Synthesised pod node fallback" applies unchanged and a synthesised pod node is produced

### Requirement: Istio route resolution of global FQDN peers

The reader SHALL support an OPTIONAL Istio route-resolution engine that resolves a
global / ingress FQDN peer to the Kubernetes Service that the Istio Gateway and
VirtualService configuration routed it to **at a single instant** — the END of the
request's own time window. The engine is consulted ONLY from "Unknown-server peer-label
enrichment", at every point that would otherwise emit an external node.

The engine SHALL evaluate the configuration **as of that one instant** and SHALL NOT
resolve per-version over the window: exactly one configuration state is consulted, exactly
one outcome is produced, and the request window's start plays no part in route resolution.

The engine SHALL be configured by a route-store connection string. When that setting is
empty the engine is **disabled**, and the reader's behaviour SHALL be byte-for-byte
identical to its behaviour before this requirement existed. Disabled is the default.

**Inputs.** For one candidate endpoint the reader SHALL supply:

- the **caller cluster** — the already-resolved client pod's own cluster, used ONLY to
  derive the cluster-family key and to break candidate ties in ingress-cluster selection;
  it SHALL NOT by itself scope any route-store query;
- the **host** — the port-stripped peer address;
- the **path** — fixed to `"/"`. The service-graph metric carries no HTTP path or route
  dimension, so no per-request path is available;
- the **listener port** — derived as specified below;
- the **destination IPs** — from the `client_dns_answers` dimension. These are a
  **precondition**: when the endpoint carries no parseable destination IP, the engine
  SHALL NOT be consulted at all — no store read occurs, the endpoint falls back to the
  external node directly, and the reader SHALL record a distinct diagnostic reason for
  the skip (separate from every engine outcome);
- the **resolution instant** — the END of the build's own time window. It is fixed (not
  configurable) and is the same instant the service-graph samples are evaluated at.

**Listener-port derivation.** The port SHALL be derived by this precedence:

1. the `:<port>` suffix split from the peer-address value, when present;
2. otherwise the OPTIONAL `client_server_port` or `client_net_peer_port` dimension, when
   present;
3. otherwise the default **443**.

**Ingress-cluster selection.** The destination IPs select the ingress cluster; the caller
cluster contributes only its family key and a tie-break. For each destination IP the
engine SHALL probe the store for the candidate set `G` — the clusters whose ingress Service
carrying that IP was live at the resolution instant — and derive `F`, the subset of `G` in the
caller's cluster family (the same digit-run-collapsing family rule used by
`service-selects-pod` fan-out). Selection per IP:

1. exactly one family candidate → that cluster;
2. several family candidates → the caller's own cluster if it is among them, otherwise
   **ambiguous**;
3. no family candidate and exactly one global candidate → that cluster;
4. no family candidate and several global candidates → the caller's own cluster if it is
   among them, otherwise **ambiguous**;
5. no candidate at all → **no ingress**.

With several destination IPs, each IP SHALL be selected independently and all selections
MUST agree on one cluster; every IP yielding "no ingress" degrades as **no ingress**, and
any other combination — an ambiguous IP, disagreeing selections, or a mix of "no ingress"
and a selected cluster — degrades as **ambiguous**. Candidate sets and store snapshots SHALL
NEVER be unioned across clusters. Once a cluster is selected, every subsequent resolution
step — snapshot load, gateway narrowing, host disambiguation, translation, route matching —
SHALL operate on that single cluster only, and the engine SHALL narrow the candidate
Gateways to those reachable from the destination IPs within it.

**Store scoping.** Every route-store *snapshot* query SHALL be scoped to the selected
ingress cluster. The ONLY cross-cluster store read SHALL be the ingress probe that answers
"which clusters had an ingress Service with this IP at the resolution instant", and its result SHALL
be deterministic (deduplicated and ordered). The reader SHALL be strictly read-only
against the store: it SHALL NOT create schema and SHALL NOT write. The reader SHALL
validate the expected schema at startup and fail fast on drift, rather than silently
returning empty results.

**Store-shape tolerance.** The store is written by an exporter whose version-close
operation REWRITES the previous open row; until background merges collapse them, a stale
open row and its closing rewrite coexist. The reader SHALL NOT treat such a pair as two
live versions (deduplicating at query time), so results do not depend on merge timing.
Stored resource specs are the API server's JSON verbatim, whose field set follows the
cluster's CRD version: the reader SHALL tolerate unknown spec fields rather than failing
the query. A VirtualService binding its gateway by the bare `<name>` form (Istio shorthand
for a gateway in the VirtualService's own namespace) SHALL bind exactly as the qualified
`<namespace>/<name>` form does.

**Version selection (as-of).** Every resource version the engine consults — ingress Service,
ingress Deployment, Gateway, VirtualService, backend Service — SHALL be the version **live at
the resolution instant**: its validity interval starts at or before that instant and ends
strictly after it. Versions that ended at or before the instant, and versions that begin after
it, SHALL NOT participate in resolution, translation, or ingress identification. The
duplicate-row discipline above SHALL be applied BEFORE the liveness test, so a stale open row
whose closing rewrite has not yet merged can never present itself as the live version.

**Resolution.** A hit yields a destination `(cluster, namespace, service)`, where the
cluster is the engine-selected ingress cluster. The reader SHALL resolve that triple
through the SAME same-cluster Service-node resolution every other path uses — anchored on
the **selected ingress cluster**, materialising AT MOST ONE service node iff that cluster
holds the Service in topology, with the same cross-cluster `service-selects-pod` fan-out
over its family. Because the selected cluster may differ from the caller's, the resulting
`pod-calls-service` edge MAY cross clusters, and the edge-type registry SHALL declare
`may_cross_cluster: true` for `pod-calls-service` (connection-string-resolved edges remain
intra-cluster by construction; per-edge cross-cluster status is still derived by comparing
the resolved endpoints' clusters). The destination's port and DestinationRule subset SHALL
be discarded. **No new node type, no new edge type, no new node attribute, and no new
`labels` key** are introduced.

**Degradation.** Every failure — engine disabled, endpoint carrying no destination IPs,
store error, no candidate ingress cluster for the IPs, an ambiguous candidate set, no
Gateway serving the host, no route matched, no listener on the derived port, or a
resolution timeout — SHALL fall back to the external node specified in "Unknown-server
peer-label enrichment". Route resolution SHALL NEVER fail a build. The reader SHALL record
distinct diagnostic reasons for a "no listener on the derived port" outcome (separate from
"no route matched", so a mis-derived port is diagnosable), for a "no candidate ingress
cluster" outcome, and for an "ambiguous ingress cluster" outcome.

**Determinism.** Route resolution SHALL be performed outside the pure service-graph parse
and its results supplied to the parse as a prefetched index, so the emitted graph remains a
deterministic function of the upstream data and the resolved destinations.

#### Scenario: Global FQDN resolves to its routed Service

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's peer address is
  `api.example.com` with a destination IP selecting the caller's own cluster as ingress,
  whose Istio Gateway and VirtualService route the host to Service `checkout` in namespace
  `shop` at the resolution instant
- **THEN** the reader SHALL emit a `service` node for `(selected cluster, shop, checkout)`
- **AND** a `pod-calls-service` edge from the client pod to it
- **AND** SHALL NOT emit `external/api.example.com`

#### Scenario: Listener port taken from the peer address

- **WHEN** the peer address is `api.example.com:8080`
- **THEN** the host SHALL be `api.example.com` and the derived listener port SHALL be `8080`

#### Scenario: Listener port defaults to 443

- **WHEN** the peer address carries no port and neither `client_server_port` nor
  `client_net_peer_port` is present
- **THEN** the derived listener port SHALL be `443`

#### Scenario: No listener on the derived port degrades to external

- **WHEN** the selected ingress cluster's Gateway serves the host but declares no routable
  HTTP listener on the derived port
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>`
- **AND** SHALL record a diagnostic reason distinct from the "no route matched" reason

#### Scenario: The server owning the host on the port is selected

- **WHEN** the selected ingress cluster's Gateway declares two TLS-terminated HTTPS servers
  on the derived port — one whose hosts match the peer FQDN and one whose hosts do not —
  in either declaration order
- **THEN** the reader SHALL resolve the request through the RouteConfiguration of the
  server whose hosts most-specifically match the peer FQDN (Istio exact/wildcard
  semantics, any `<ns>/` binding prefix stripped before matching)

#### Scenario: No server on the derived port serves the host

- **WHEN** the selected ingress cluster's Gateway declares servers on the derived port but
  none of their hosts match the peer FQDN
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>`
- **AND** SHALL record a diagnostic reason distinct from both the "no listener on the
  derived port" reason and the "no route matched" reason

#### Scenario: Store failure never fails the build

- **WHEN** the route store is unreachable or returns an error while resolving an endpoint
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>` for that endpoint
- **AND** the build SHALL complete successfully

#### Scenario: Destination IP narrows the candidate Gateways within the selected cluster

- **WHEN** `client_dns_answers` supplies a destination IP that, within the selected ingress
  cluster, reaches exactly one of two Gateways whose server hosts both match the peer FQDN
- **THEN** the reader SHALL resolve the host against that Gateway only

#### Scenario: No destination IPs means the engine is not consulted

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's peer address
  would fall external, but the series carries no parseable `client_dns_answers` IP
- **THEN** the engine SHALL NOT be consulted (no store read for this endpoint)
- **AND** the reader SHALL emit `external/<raw_peer_address_value>` exactly as when route
  resolution is disabled
- **AND** SHALL record a diagnostic reason distinct from every engine outcome

#### Scenario: Same-family ingress candidate wins over a cross-family one

- **WHEN** a destination IP's candidate ingress clusters are one cluster in the caller's
  family and one cluster outside it
- **THEN** the engine SHALL select the same-family cluster

#### Scenario: Caller breaks a same-family ingress-IP collision

- **WHEN** a destination IP's candidate ingress clusters include the caller's own cluster
  and a family sibling
- **THEN** the engine SHALL select the caller's own cluster

#### Scenario: Unresolvable ingress-cluster tie degrades to external

- **WHEN** a destination IP's candidate ingress clusters are two family siblings, neither
  of which is the caller's own cluster
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>`
- **AND** the reader SHALL record the "ambiguous ingress cluster" diagnostic reason

#### Scenario: No cluster serves the destination IP

- **WHEN** no cluster had an ingress Service carrying any of the destination IPs live at the
  resolution instant
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>`
- **AND** the reader SHALL record the "no candidate ingress cluster" diagnostic reason

#### Scenario: Disagreeing multi-IP selections degrade to external

- **WHEN** `client_dns_answers` supplies two IPs whose independent selections pick two
  different ingress clusters
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>` with the
  "ambiguous ingress cluster" diagnostic reason

#### Scenario: Cross-cluster ingress resolves to a Service in the sibling cluster

- **WHEN** the caller pod lives in cluster A and the destination IP selects ingress
  cluster B, whose Gateway and VirtualService route the host to Service `payments` in
  namespace `shop`, and B holds that Service in topology
- **THEN** the reader SHALL emit the `service` node `B/shop/payments`
- **AND** a `pod-calls-service` edge from the cluster-A client pod to it (a cross-cluster
  edge)
- **AND** SHALL NOT emit an external node for the peer

#### Scenario: A rewritten (closed) version does not double-count

- **WHEN** the store carries both a stale open row (far-future `valid_to`) and its closing
  rewrite (same version key, higher ingest sequence) for one Gateway version
- **THEN** the reader SHALL see exactly one live version per instant, regardless of
  whether the store's background merge has run

#### Scenario: Unknown spec fields do not fail the query

- **WHEN** a stored Gateway or VirtualService spec carries a field unknown to the reader's
  compiled Istio API version
- **THEN** the reader SHALL parse the known fields and resolve routing from them

#### Scenario: Bare gateway reference binds

- **WHEN** a VirtualService in the gateway's own namespace lists the gateway by bare name
  in `spec.gateways`
- **THEN** its routes SHALL bind to that gateway exactly as a qualified
  `<namespace>/<name>` reference would

#### Scenario: Only the configuration live at the resolution instant is used

- **WHEN** the selected ingress cluster's store holds one Gateway version that ended inside the
  request window and a newer version live at the window's end, and only the newer version serves
  the peer FQDN on the derived port
- **THEN** the engine SHALL resolve the request through the newer version
- **AND** SHALL NOT consult the version that ended before the resolution instant

#### Scenario: Configuration that stopped serving the host degrades to external

- **WHEN** a Gateway routed the peer FQDN earlier in the request window but the version live at
  the window's end no longer serves that host
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>` with the
  corresponding miss diagnostic reason

#### Scenario: Cross-namespace Gateway is not a candidate

- **WHEN** the selected ingress cluster's ingress Service and Deployment live in namespace
  `istio-system`, and the only Gateway whose selector matches the ingress Deployment's pod
  labels and whose server hosts serve the peer FQDN lives in namespace `team-b`
- **THEN** that Gateway SHALL NOT be a candidate and the pipeline SHALL miss with the
  "no Gateway serves the host" outcome
- **AND** the ingress LB Service fallback SHALL still apply per its own requirement

#### Scenario: Identical host patterns resolve to the lexically-smallest gateway

- **WHEN** two candidate Gateways in the ingress namespace declare the identical, equally
  specific server-host pattern matching the peer FQDN, in either declaration or storage
  order
- **THEN** the engine SHALL resolve the request through the Gateway with the
  lexically-smallest name

### Requirement: Full ingress chain for routed global-FQDN hits

When the Istio route-resolution engine produces a routed hit ("Istio route resolution of global
FQDN peers"), the engine SHALL additionally attempt to recover the identity of the **ingress LB
Service** the destination IP set uniquely maps to inside the already-selected ingress cluster's
loaded snapshot, using the same as-of identity-dedup rule as the "Ingress LB Service fallback"
requirement (per-IP distinct `(namespace, name)` sets; same-IP collision or disagreeing
singletons → no identity; any IP with zero rows → no identity; all singletons agreeing → that
identity). Identity recovery SHALL NOT read the store again and SHALL NEVER demote the hit: an
ambiguous or absent identity leaves the hit's destination without an ingress identity and the
reader emits the pre-existing direct shape.

**Chain emission.** When the hit's destination carries an ingress identity AND every chain
precondition below holds, the reader SHALL emit — instead of the direct
`caller pod → backend service` edge — the full chain:

- a `service` node for the ingress LB Service in the selected ingress cluster, with
  `service-selects-pod` edges to **the selected cluster's own backing pods only** (locked-cluster
  fan-out — NOT the family-wide union: an LB IP is a per-cluster address, so a family sibling's
  same-named Service is not behind it);
- one `pod-calls-service` edge from the client pod to the ingress `service` node (this edge MAY
  cross clusters, exactly as the direct edge it replaces);
- one synthesized `pod-calls-service` edge from EACH of those locked-cluster ingress backing pods
  to the backend `service` node, carrying the ingress cluster as the edge's `cluster` label (the
  client side is a pod in that cluster);
- the backend `service` node and its family-wide `service-selects-pod` fan-out, unchanged from
  the pre-existing hit resolution.

The direct `caller pod → backend service` edge SHALL NOT be emitted alongside the chain. A
synthesized ingress-pod→backend edge whose `(source pod, target service)` pair already exists as
a trace-derived edge SHALL be skipped — the traced edge wins (the two would otherwise share one
deterministic edge ID). Synthesized-edge emission SHALL be deterministic (order-free over the
endpoint set; idempotent under repeated series sharing one route key). **No new node type, no new
edge type, no new node attribute, and no new `labels` key** are introduced.

**Chain preconditions (each failure degrades to the pre-existing direct shape).** The chain SHALL
be emitted only when ALL hold:

1. the destination carries an ingress identity (recovered as above);
2. the ingress identity differs from the backend destination's `(namespace, service)` in the
   selected cluster (a destination that IS the ingress entry point keeps the direct shape);
3. the selected ingress cluster holds the ingress Service in topology;
4. that Service has at least one backing pod in the selected cluster's endpoints (an empty middle
   would disconnect the caller from the backend).

Degradation SHALL be observable via a debug-level diagnostic only — it SHALL NOT emit an external
node, SHALL NOT count toward external-fallback reasons, and SHALL leave no partially-materialised
ingress node or edge. A topology miss on the **backend** Service keeps the existing "selected
cluster lacks the Service" external path, with the ingress Service never materialised. Route
resolution SHALL still NEVER fail a build.

**Locked-cluster fan-out for the LB fallback.** The "Ingress LB Service fallback" requirement's
resolution SHALL likewise fan out `service-selects-pod` edges over the selected ingress cluster's
own backing pods only (superseding its family-wide fan-out): the fallback answers "which ingress
LB Service owns this IP", and the pods behind that IP are the owning cluster's endpoints. No
synthesized pod-calls-service edges are emitted on the fallback path (there is no routed backend
behind the LB entry point).

#### Scenario: Routed hit with recoverable ingress identity emits the full chain

- **WHEN** route resolution is enabled, a `server="unknown"` endpoint's destination IP selects an
  ingress cluster whose snapshot uniquely maps the IP set to one ingress LB Service, the pipeline
  routes `(host, path, port)` to a backend Service, and the selected cluster's topology holds
  both Services with at least one ingress backing pod
- **THEN** the reader SHALL emit `service` nodes for the ingress LB Service and the backend
  Service
- **AND** a `pod-calls-service` edge from the client pod to the ingress `service` node
- **AND** `service-selects-pod` edges from the ingress node to the selected cluster's own ingress
  backing pods only
- **AND** one synthesized `pod-calls-service` edge from each such ingress pod to the backend
  `service` node
- **AND** the family-wide `service-selects-pod` fan-out from the backend node
- **AND** SHALL NOT emit a direct `pod-calls-service` edge from the client pod to the backend
  node, and SHALL NOT emit an external node

#### Scenario: Ambiguous ingress identity keeps the hit as a direct edge

- **WHEN** the pipeline produces a routed hit but the destination IPs map to more than one
  ingress LB Service identity live at the resolution instant in the selected cluster (or some IP
  maps to none)
- **THEN** the reader SHALL resolve the hit exactly as without this change: a direct
  `pod-calls-service` edge from the client pod to the backend `service` node and the family-wide
  backend fan-out
- **AND** the engine SHALL still report the routed hit (never a miss)

#### Scenario: Ingress Service missing from topology degrades to the direct edge

- **WHEN** a routed hit carries an ingress identity but the selected ingress cluster does not
  hold that Service in topology
- **THEN** the reader SHALL emit the direct-edge shape
- **AND** SHALL NOT emit an ingress `service` node or an external node for the peer

#### Scenario: Ingress Service with no backing pods degrades to the direct edge

- **WHEN** a routed hit carries an ingress identity, the selected cluster holds the ingress
  Service in topology, but the Service has no backing pods in that cluster's endpoints
- **THEN** the reader SHALL emit the direct-edge shape
- **AND** SHALL NOT emit an ingress `service` node

#### Scenario: Destination equal to the ingress identity keeps the direct edge

- **WHEN** a routed hit's backend destination `(namespace, service)` equals the recovered ingress
  identity in the selected cluster
- **THEN** the reader SHALL emit the direct-edge shape with that single `service` node

#### Scenario: Backend missing from topology stays external, ingress never materialised

- **WHEN** a routed hit carries an ingress identity but the selected ingress cluster does not
  hold the BACKEND Service in topology
- **THEN** the endpoint SHALL fall back to the external node with the existing "selected cluster
  lacks the Service" diagnostic reason
- **AND** SHALL NOT emit an ingress `service` node or any `service-selects-pod` edge

#### Scenario: Traced edge wins over the synthesized edge

- **WHEN** the chain is emitted and the service graph also carries a trace-derived
  `pod-calls-service` edge from one of the ingress backing pods to the same backend `service`
  node
- **THEN** the response SHALL carry exactly one edge for that `(pod, service)` pair

#### Scenario: LB fallback fans out over the selected cluster only

- **WHEN** the ingress LB Service fallback resolves an ingress Service that a family sibling
  cluster also holds under the same `(namespace, name)` with its own backing pods
- **THEN** the reader SHALL emit `service-selects-pod` edges to the selected ingress cluster's
  backing pods only
- **AND** SHALL NOT emit edges to the sibling cluster's pods

#### Scenario: Chain ingress fan-out ignores family siblings while the backend keeps them

- **WHEN** the chain is emitted in a cluster family where a sibling cluster holds a same-named
  ingress Service (with its own pods) and the backend Service is also held by the sibling (with
  its own endpoints)
- **THEN** the ingress node's `service-selects-pod` edges and the synthesized
  ingress-pod→backend edges SHALL cover the selected cluster's ingress pods only
- **AND** the backend node's `service-selects-pod` fan-out SHALL still cover the family-wide
  endpoint union

### Requirement: Ingress LB Service fallback for unresolved global FQDN peers

When the Istio route-resolution engine ("Istio route resolution of global FQDN peers") has
selected an ingress cluster, loaded its snapshot at the resolution instant, and run the Gateway +
VirtualService pipeline WITHOUT producing a hit AND without progressing past gateway
resolution (the pipeline's miss is the "no gateway serves the host" outcome — the
signature of a non-Istio ingress such as nginx, whose Hop 3 finds no Gateway CR), the engine
SHALL — before returning that miss — attempt one fallback: resolve the destination IP set to a
**unique ingress LB Service** inside the already-selected ingress cluster's loaded snapshot.

The fallback SHALL NOT run when the pipeline produced a hit (a routed `hit` always wins), SHALL
NOT run when the miss is any deeper pipeline outcome ("no listener on the derived
port", "no server for host", "no route matched" — an Istio Gateway DID serve the host and its
diagnostic reason MUST NOT be masked by an LB-entry-point edge), and CANNOT run when
ingress-cluster selection itself failed (the "no candidate ingress cluster" and "ambiguous
ingress cluster" outcomes return before any snapshot is loaded).

**Candidate set (as-of the resolution instant).** For each destination IP, the candidates are
every ingress Service version in the loaded (selected-cluster) snapshot that is **live at the
resolution instant** and whose ingress IPs (external or load-balancer) contain that IP,
deduplicated to distinct `(namespace, name)` identities. An identity that was replaced BEFORE
the resolution instant therefore does not count — only owners live at that instant do. The
fallback SHALL NOT read the store again — it operates on the rows already loaded for the
pipeline.

**Uniqueness rule (order-free over IPs).** Per destination IP the identity set `S_ip` is
computed independently; then:

1. any `|S_ip| > 1` → the fallback degrades as **ambiguous ingress Service**;
2. otherwise any `|S_ip| == 0` → the fallback yields nothing and the engine SHALL return the
   Istio pipeline's own miss unchanged;
3. otherwise all (singleton) candidates MUST be the same identity → that identity is the
   fallback destination; disagreeing identities degrade as **ambiguous ingress Service**.

No lexicographic or recency tie-break SHALL be applied — ambiguity degrades rather than guesses.

**Resolution.** A fallback destination yields `(cluster, namespace, service)` with the cluster
being the engine-selected ingress cluster, reported under a distinct engine outcome (separate
from `hit`). The reader SHALL resolve it through the SAME same-cluster Service-node resolution
as a routed hit — anchored on the selected ingress cluster, AT MOST ONE service node iff that
cluster holds the Service in topology (a topology miss follows the existing
"selected cluster lacks the Service" external path), the same cross-cluster
`service-selects-pod` fan-out over its family, and a `pod-calls-service` edge that MAY cross
clusters. The destination's port and subset are absent/discarded. The fallback's semantics are
deliberately coarse — "which ingress LB Service owns this IP" — and SHALL ignore the request's
host, path, and derived listener port. **No new node type, no new edge type, no new node
attribute, and no new `labels` key** are introduced.

**Degradation.** The **ambiguous ingress Service** outcome SHALL fall back to the external node
with its own diagnostic reason, distinct from every existing engine reason. When the fallback
yields nothing (rule 2), the engine's outcome and diagnostic reason SHALL be byte-for-byte the
ones the pipeline would have returned without the fallback. Route resolution SHALL still NEVER
fail a build.

#### Scenario: nginx ingress resolves to its LB Service

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's destination IP selects
  an ingress cluster whose snapshot holds exactly one ingress LB Service carrying that IP, with
  no Istio Gateway CR reachable from the IP (the pipeline misses at gateway resolution)
- **THEN** the reader SHALL emit a `service` node for that `(selected cluster, namespace,
  service)`
- **AND** a `pod-calls-service` edge from the client pod to it
- **AND** the family-wide `service-selects-pod` fan-out to the Service's backing pods (the
  ingress controller pods)
- **AND** SHALL NOT emit an external node for the peer

#### Scenario: A deep Istio miss is not masked by the fallback

- **WHEN** the selected ingress cluster's Gateway serves the host but the pipeline misses at a
  stage past gateway resolution (no listener on the derived port, no server for the host, or no
  route matched), and the snapshot also holds an ingress LB Service carrying the destination IP
- **THEN** the engine SHALL return that deeper miss outcome and diagnostic reason unchanged
- **AND** SHALL NOT resolve the ingress LB Service

#### Scenario: A routed hit always beats the fallback

- **WHEN** the selected ingress cluster's Gateway and VirtualService route the host to a backend
  Service at the resolution instant, and the snapshot also holds an ingress LB Service carrying
  the destination IP
- **THEN** the reader SHALL resolve the routed backend Service (`hit`), not the LB Service

#### Scenario: Two LB Services on one IP degrade to external

- **WHEN** the pipeline misses and the selected cluster's snapshot holds two differently-named
  ingress LB Services, both live at the resolution instant, whose ingress IPs both contain the
  destination IP
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>`
- **AND** the reader SHALL record the "ambiguous ingress Service" diagnostic reason

#### Scenario: A superseded identity does not make the IP ambiguous

- **WHEN** the pipeline misses and one ingress LB Service identity carrying the destination IP
  ended BEFORE the resolution instant while a differently-named one carrying the same IP is live
  at it
- **THEN** the fallback SHALL resolve the identity live at the resolution instant
- **AND** SHALL NOT report the "ambiguous ingress Service" diagnostic reason

#### Scenario: Version churn of one identity still resolves

- **WHEN** the pipeline misses and the store holds several versions (rows) of the SAME
  `(namespace, name)` ingress LB Service carrying the destination IP, one of them live at the
  resolution instant
- **THEN** the fallback SHALL resolve that single identity

#### Scenario: No LB Service keeps the pipeline miss

- **WHEN** the pipeline misses and no ingress Service live at the resolution instant in the
  selected cluster carries the destination IP
- **THEN** the engine SHALL return the pipeline's own miss outcome and diagnostic reason,
  byte-for-byte as without the fallback

#### Scenario: Disagreeing per-IP identities degrade to external

- **WHEN** the pipeline misses and two destination IPs each resolve to exactly one ingress LB
  Service, but to different identities
- **THEN** the endpoint SHALL fall back to `external/<raw_peer_address_value>` with the
  "ambiguous ingress Service" diagnostic reason

#### Scenario: Fallback hit whose Service is absent from topology

- **WHEN** the fallback resolves an ingress LB Service but the selected cluster does not hold
  that Service in topology
- **THEN** the endpoint SHALL fall back to the external node with the existing "selected cluster
  lacks the Service" diagnostic reason

