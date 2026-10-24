# pod-service-graph Specification

## Purpose

Turn the trace pipeline's service-graph metrics (`traces_service_graph_*`) into call edges between pods, Services and external endpoints. Covers how each endpoint is resolved, the RED measurements an edge carries, span-link relation marking, and the optional Istio route resolution of global FQDN peers.

## Requirements

### Requirement: Pod-UID-resolved edge source

The reader SHALL build call edges from the service-graph series in centralised VictoriaMetrics, joined by pod UID, and SHALL derive each edge by computing `rate(...[<window>]) @ <end>` over each counter. The client pod SHALL be resolved via `(cluster, client_k8s_pod_uid)` and the server pod by looking up `server_k8s_pod_uid` in a global pod-UID index built from topology.

#### Scenario: Consumed series and labels

- **WHEN** the reader builds the service graph
- **THEN** it consumes at minimum `traces_service_graph_request_total{client, server, cluster, client_k8s_pod_uid, server_k8s_pod_uid, client_k8s_namespace_name, server_k8s_namespace_name, connection_type}`, `traces_service_graph_request_failed_total{ ...same labels... }` and `traces_service_graph_request_server_seconds_bucket{ ...same labels..., le }`

#### Scenario: Trace cluster label is the client-side cluster

- **WHEN** a series carries its single `cluster` external label, applied by the trace pipeline that produced it (typically Tempo's metrics-generator running in a single source cluster)
- **THEN** the reader treats that value as the client-side cluster, the cluster originating the call, and resolves the client pod via `(cluster, client_k8s_pod_uid)`

#### Scenario: Server-side cluster comes from the matched topology pod

- **WHEN** `server_k8s_pod_uid` matches a pod in the global pod-UID index (Kubernetes pod UIDs are unique across clusters in practice)
- **THEN** the matched pod's own `cluster` value provides the server-side cluster for the edge `target` ID

#### Scenario: Human labels are ignored for pod-resolved endpoints

- **WHEN** an endpoint resolves by pod UID
- **THEN** its `client` / `server` string label is ignored; those labels are consumed only by "Connection-string endpoint resolution" and the missing pod-UID human-label fallback

#### Scenario: Sentinel peers are excluded before resolution

- **WHEN** a series names a virtual sentinel peer
- **THEN** it is excluded at the query layer per "Virtual sentinel endpoint exclusion (user / unknown)" and never reaches any endpoint-resolution stage

#### Scenario: Edge produced from non-zero rate

- **WHEN** for the requested window `rate(traces_service_graph_request_total{...})` is greater than zero for a series whose client side and server-pod-UID resolve via topology
- **THEN** the reader emits one `pod-calls-pod` edge whose `source` is `<cluster>/<client_k8s_pod_uid>` and `target` is `<resolved-cluster>/<server_k8s_pod_uid>`, where `<resolved-cluster>` is the topology-side cluster of the matched server pod

#### Scenario: Zero-rate series is dropped

- **WHEN** `rate(traces_service_graph_request_total{...})` evaluates to exactly zero for a series in the window
- **THEN** no edge is emitted for that series

### Requirement: Service-graph queries carry no request-scoped selector

The reader SHALL read the three service-graph series in full for every request and SHALL NOT render any request-scoped selector dimension (`az`, `env`, `cluster`, `namespace`) into a service-graph query. The only selectors on these queries SHALL be the fixed, request-invariant sentinel and `edge_relation!="link"` matchers.

#### Scenario: Service-graph queries ignore request filters

- **WHEN** a build runs with `namespace={shop}` and `cluster={cluster-alpha}`
- **THEN** the three service-graph queries are issued exactly as for an unfiltered build, and the series for callers and servers outside `shop` / `cluster-alpha` reach the resolver (to be admitted or dropped by the filtered-build rule)

#### Scenario: Loaded topology decides which series survive

- **WHEN** a filtered build reads the service graph
- **THEN** series are narrowed in memory by "Filtered-build admission and out-of-scope endpoints", never upstream
- **AND** the reason is that the trace-side `cluster` label is frequently missing or disagrees with topology, and the namespace labels describe only the caller's own view, so narrowing at the query would drop edges the loaded topology still needs

### Requirement: Virtual sentinel endpoint exclusion (user / unknown)

The reader SHALL exclude, at the PromQL query layer, every `traces_service_graph_request_total` series whose `client` label is exactly `"user"` or exactly `"unknown"` (`client!~"user|unknown"`) or whose `server` label is exactly `"user"` (`server!~"user"`). The sentinel sets SHALL be compiled in and matched fully anchored and case-sensitively against the `client` and `server` labels only.

#### Scenario: Sentinels are virtual peers that represent nothing

- **WHEN** the service-graph producer (the OpenTelemetry / Alloy / Tempo `servicegraph` connector) cannot pair an endpoint to an instrumented span and emits an uninstrumented caller as `client="user"` or an unresolved peer as `"unknown"`
- **THEN** the series carries no pod UID and represents no pod, service, or declared external dependency the API should surface
- **AND** because a client-side-excluded series never arrives, no pod, synthesised-pod, `service`, or `external` node is materialised for it and no edge touching such a peer is emitted

#### Scenario: `server="unknown"` reaches Go-side resolution

- **WHEN** a series has `server` exactly `"unknown"`
- **THEN** the query-layer matcher does not exclude it and it reaches Go-side resolution, where it is treated as unresolvable and dropped (no node, no edge) UNLESS "Unknown-server peer-label enrichment" applies
- **AND** the narrower server-side matcher exists solely to let that enrichment observe the series' `client_server_address`, `client_network_peer_address` and `client_net_peer_name` labels; it is not a general relaxation, and every `server="unknown"` case outside the enrichment trigger produces exactly the outcome a query-layer exclusion would (dropped, no node, no edge)

#### Scenario: Matching is exact and fully anchored

- **WHEN** a `client` or `server` label merely contains a sentinel, such as the connection-string value `"http://user/path"`
- **THEN** it is not excluded, because the PromQL `!~` regex is anchored to the entire label value, and it proceeds to connection-string resolution unchanged

#### Scenario: Matching is case-sensitive

- **WHEN** a `client` label is `User`, `UNKNOWN`, or another case variant of a sentinel
- **THEN** the series is not excluded

#### Scenario: The sentinel set has no configuration surface

- **WHEN** an operator wants a different sentinel set
- **THEN** there is no env var, flag, or config field to change it; the set is fixed at `{"user", "unknown"}` (client) and `{"user"}` (server)

#### Scenario: RED selectors carry an extra matcher without weakening the rule

- **WHEN** the reader issues the `traces_service_graph_request_failed_total` and `traces_service_graph_request_server_seconds_bucket` queries read by "RED source series and selector consistency"
- **THEN** each carries the sentinel fragment (client: `user|unknown`; server: `user`) byte-identically, so the edge population stays consistent across metric families
- **AND** each also carries one request-invariant matcher of its own (the span-link exclusion), which does not alter the sentinel fragment this rule fixes

#### Scenario: Series with client `user` is excluded at the query layer

- **WHEN** upstream holds a `traces_service_graph_request_total` series with `client="user"`, `server="checkout"`, `server_k8s_pod_uid="abc"`
- **THEN** the service-graph query selector includes `client!~"user|unknown"`, VictoriaMetrics does not return the series, and the graph contains no edge for it and no node named `user`

#### Scenario: Series with server `unknown` and no usable peer label is still dropped

- **WHEN** upstream holds a series with `client="checkout"`, `client_k8s_pod_uid="abc"` (resolving to a real topology pod), `server="unknown"`, `server_k8s_pod_uid=""`, and the series carries none of `client_server_address`, `client_network_peer_address`, or `client_net_peer_name`
- **THEN** the series is not excluded at the query layer (`server!~"user"` only), but Go-side resolution drops it per the "Unknown-server peer-label enrichment" requirement's no-match case: the graph contains no edge for it and no node named `unknown`, `external/unknown`, or otherwise — the same outcome a `server!~"user|unknown"` query-layer exclusion would give

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

For every emitted `pod-calls-pod` or `pod-calls-service` edge whose client side resolves to a pod, the reader SHALL set `labels.cluster` to the cluster identity that originated the RPC. When the client side resolves to a non-pod node (service or external), the reader SHALL omit the `cluster` key. The reader SHALL NOT emit `client_cluster`, `server_cluster` or `cross_cluster` keys on edge `labels`.

#### Scenario: Identity source for the edge cluster label

- **WHEN** the client side resolves to a pod
- **THEN** `labels.cluster` is a cluster identity (`cluster-topology-source`, "Cluster identity composed from zone and environment labels"): the client pod's own `labels.cluster` when the client resolved to a topology pod
- **AND** otherwise the series' `cluster` label resolved through the identity ladder: composed from the series' own zone/environment labels when it carries both, adopted when the raw name is unambiguous in the build, verbatim otherwise

#### Scenario: Non-pod client omits the cluster key

- **WHEN** the client side resolves to a service or external node
- **THEN** the edge `labels` carry no `cluster` key, because non-pod endpoints are not cluster-scoped

#### Scenario: Cross-cluster status is derived, never labelled

- **WHEN** a consumer needs the server-side cluster or whether an edge crosses clusters
- **THEN** it reads the `target` node's `labels.cluster` and compares the resolved source and target nodes' `labels.cluster` values; no `cross_cluster` boolean is encoded inside `labels` (booleans are deferred to a future typed field)

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

The reader SHALL run connection-string resolution for an endpoint when BOTH its pod UID (`client_k8s_pod_uid` or `server_k8s_pod_uid`) is empty or absent AND its `client` / `server` label contains the substring `"://"`. The endpoint SHALL resolve to at most one service node in the anchor cluster, otherwise to an external node.

#### Scenario: A connection string carries the URL verbatim

- **WHEN** a service-graph series addresses an endpoint that is an external dependency addressed by URL
- **THEN** that endpoint's pod UID is empty and its `client` / `server` label holds the connection string verbatim, e.g. `"mongodb://mongo-0.mongo.db.svc.cluster.local:27017"` or `"https://payments.partner.example/api"`

#### Scenario: A non-empty pod UID skips connection-string resolution

- **WHEN** an endpoint's pod UID is non-empty
- **THEN** normal pod-UID resolution applies unchanged and connection-string resolution is NOT run (connection strings only appear when the UID is empty)

#### Scenario: Detection reads no configuration

- **WHEN** the reader detects connection strings
- **THEN** it uses only a hardcoded `"://"` substring check, evaluated independently against the `client` and `server` label values, and reads no substring or pattern from configuration (there is no configurable knob)

#### Scenario: Resolution is decided per endpoint

- **WHEN** a single series has endpoints of different kinds
- **THEN** it MAY produce edges with a pod source and service or external targets, an external source and a pod target, two pods, or any mix
- **AND** the edge `type` is `pod-calls-service` when the target resolves to a service node, otherwise `pod-calls-pod`

#### Scenario: Edge cluster label follows the client side

- **WHEN** an edge is emitted from a series involving a connection string
- **THEN** `labels.cluster` is present when the client side resolves to a pod (from a non-empty pod UID) and omitted when the client side resolves to a service or external node, including ANY `"://"` connection string, which never resolves to a pod

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

### Requirement: Connection-string host parsing and Kubernetes DNS grammar

The reader SHALL parse a connection string as a URL, take its host, and reduce a Kubernetes Service DNS name to the addressed `(service, namespace)`. A label with no host, or a host of any other shape, SHALL be unresolvable.

#### Scenario: Host extraction

- **WHEN** the reader parses a `"://"` label
- **THEN** it takes the URL host, stripping scheme, userinfo, port, and any path/query
- **AND** a label with no host is unresolvable

#### Scenario: Service DNS suffix forms

- **WHEN** the host is matched against the Kubernetes DNS grammar
- **THEN** an optional trailing `.svc.<cluster-domain>` suffix (e.g. `.svc.cluster.local`) is stripped, and the shorter `<...>.svc` and the bare `<a>.<b>` forms are also accepted
- **AND** the dotted labels of the service-relative part are counted and both forms reduce to the addressed `(service, namespace)`

#### Scenario: Two labels address the service

- **WHEN** the service-relative part has 2 labels, `<service>.<namespace>`
- **THEN** it addresses that service (a regular ClusterIP service, or a headless service's service-level name)

#### Scenario: Three labels are a headless per-pod name

- **WHEN** the service-relative part has 3 labels, `<pod-hostname>.<service>.<namespace>`
- **THEN** the reader drops the leading `<pod-hostname>` and resolves the remaining `<service>.<namespace>`, so a headless per-pod address and the bare service address resolve identically; there is NO per-pod resolution

#### Scenario: Any other label count is unresolvable

- **WHEN** the service-relative part has neither 2 nor 3 labels
- **THEN** the label is unresolvable

### Requirement: Single local-cluster service node for a connection string

The reader SHALL resolve the addressed `(namespace, service)` against the topology index `ServicesByNameNS` (built from `kube_service_info`) and SHALL materialise AT MOST ONE service node, located in the anchor cluster itself, and only when the anchor cluster holds the addressed Service. Otherwise the label SHALL be unresolvable. No service node SHALL be materialised in any other cluster.

#### Scenario: The anchor cluster must itself hold the Service

- **WHEN** no family candidate cluster equals the anchor cluster (the anchor cluster does not deploy the Service object)
- **THEN** the label is unresolvable and falls back to an external node
- **AND** a family sibling holding the Service is NOT sufficient, because a same-named local Service is a service-mesh precondition for the call to route at all

#### Scenario: One anchor-membership test covers unanchorable callers

- **WHEN** the anchor is `"unknown"`, empty, or bogus and holds no matching Service in its own family
- **THEN** the same anchor-membership test makes the label unresolvable

#### Scenario: Fully-unlabelled single-cluster case is preserved

- **WHEN** the caller and the Service are both bucketed to `"unknown"`
- **THEN** `clusterFamilyKey("unknown") = "unknown"` is a family-of-one and the `"unknown"`-bucketed Service makes `"unknown"` a legitimate holder, so the Service resolves

#### Scenario: Shape of the materialised service node

- **WHEN** the anchor candidate holds the addressed Service
- **THEN** the reader materialises EXACTLY ONE service node with `id="<anchor>/<namespace>/<service>"`, `type="service"`, `labels={ cluster: anchor, namespace }`
- **AND** `ipaddress=[cluster_ip]` taken from the ANCHOR cluster's OWN `ServiceObs` when `cluster_ip != "None"`, omitted for a headless service where `cluster_ip="None"`

### Requirement: Cross-cluster service-selects-pod fan-out from a connection-string service

From the single anchor-cluster service node, the reader SHALL materialise, on demand and deduplicated by `(service-node ID, pod ID)`, one `service-selects-pod` edge to each backing pod in the union of `EndpointsByService[{c, namespace, service}]` over every family candidate cluster `c` that holds the same-named Service. Materialisation SHALL be gated on Service-object presence, never on endpoint presence.

#### Scenario: Endpoint source

- **WHEN** the reader builds the fan-out
- **THEN** each `EndpointsByService` entry comes from `kube_endpointslice_endpoints` joined to topology pods by `(namespace, targetref_name) → pod UID`

#### Scenario: Fan-out edges may cross clusters

- **WHEN** a family sibling holding the same-named Service has backing pods
- **THEN** the local service node in the anchor cluster selects those pods, producing `service-selects-pod` edges that cross the cluster boundary
- **AND** this reflects service-mesh endpoint aggregation (Istio multi-primary, Cilium Cluster Mesh), where the same-named Service exists in every family cluster but each cluster's kube-state-metrics observes only its OWN EndpointSlices, so the cross-cluster endpoint set is reconstructed by unioning over the family

#### Scenario: A sibling with no backing pods contributes nothing

- **WHEN** a family sibling holds the Service object but has zero backing pods
- **THEN** it contributes no edges and the local service node still materialises

#### Scenario: No endpoint-backed pruning

- **WHEN** a known service has zero backing endpoints anywhere in the family
- **THEN** its single local service node still materialises with no fan-out edges, an operator signal

### Requirement: Cluster-family key and connection-string anchor

The reader SHALL compute a cluster's family key by replacing every maximal run of ASCII digits (`[0-9]+`) in its name with a single `0` sentinel; two clusters SHALL be in the same family iff their keys are byte-equal. The candidate clusters for a lookup SHALL be the loaded clusters in the anchor's family that hold the addressed `(namespace, service)`, with NO unknown-family fallback and NO cross-family resolution.

#### Scenario: Family key examples

- **WHEN** cluster names are normalised
- **THEN** `prod-03` and `prod-12` both become `prod-0` and share a family; `staging-1` (key `staging-0`) is NOT in `prod-1`'s family (key `prod-0`); bare digit-run names such as `1` and `2` all become `0` and form one family; a digit-free name normalises to itself, so its family contains only identically-named clusters

#### Scenario: The sentinel is a digit

- **WHEN** the key function substitutes a digit run
- **THEN** it writes the digit `0`, so the mapping is collision-free without escaping: every `0` in a key originates from a digit run and a non-digit byte can never equal the sentinel (a non-digit sentinel would collide with cluster names literally containing it)

#### Scenario: The key function has no configuration surface

- **WHEN** an operator wants a different family rule
- **THEN** there is no flag, env var, or config field; the key function is a hardcoded pure string function

#### Scenario: The anchor is the client side's authoritative cluster

- **WHEN** the series' client side resolved to a topology pod
- **THEN** the family anchor is the UID-recovered client-pod cluster, because the trace `cluster` label is frequently missing or disagrees with topology
- **AND** otherwise the anchor is the series' trace-source `cluster` label, bucketed to `"unknown"` when missing
- **AND** the edge `labels.cluster` value is set by the "Edge cluster label" requirement, not by anchor recovery: the client pod's cluster identity when the client resolved to a topology pod, else the series' `cluster` label resolved through the identity ladder

#### Scenario: Candidate clusters and their two roles

- **WHEN** the reader collects candidates for an anchor
- **THEN** they are all clusters loaded in the build's topology whose family key equals the anchor's AND which hold the addressed `(namespace, service)` in `ServicesByNameNS`, iterated in sorted order
- **AND** the anchor cluster, when among them, is the single cluster whose service node materialises, while all candidates' `EndpointsByService` entries are unioned for the cross-cluster `service-selects-pod` fan-out

#### Scenario: No cross-family escape for unlabelled Services

- **WHEN** an anchor's own family holds no matching Service, including an unanchorable `"unknown"`/empty/bogus anchor
- **THEN** the endpoint resolves to `external/<label>`, because a call is never attributed to a Service in a cluster the caller does not belong to
- **AND** an unlabelled (`"unknown"`-bucketed) Service is reachable ONLY by an `"unknown"`-anchored caller (its own family-of-one), never unioned into a real cluster's fan-out (`"unknown" ≠ "prod-0"`)

#### Scenario: Family scoping is in-memory

- **WHEN** family scoping is applied
- **THEN** it happens at the resolution layer with NO PromQL query change and NO new flag or environment knob, preserving the "no filters pushed to PromQL" contract

### Requirement: Connection-string edge emission and determinism

The reader SHALL emit one `pod-calls-service` edge per `(resolved source node, resolved service-node ID)` pair, so a single series yields at most one `pod-calls-service` edge per resolved source, and every such edge SHALL be intra-cluster. Connection-string resolution SHALL be deterministic, keeping the response body byte-identical for identical upstream data.

#### Scenario: Both sides are connection strings

- **WHEN** BOTH sides of a series are `"://"` labels resolving to service nodes
- **THEN** the reader emits the single (1×1) edge between them
- **AND** a non-`"://"` side resolves to a single node ID exactly as for any other series

#### Scenario: pod-calls-service edges are always intra-cluster

- **WHEN** a `pod-calls-service` edge is emitted from connection-string resolution
- **THEN** it connects its source (a pod, or in the both-`"://"` and external-source cases a service or external node) to a service node in the SAME (anchor) cluster, so its derived cross-cluster status is always intra
- **AND** the cross-cluster relationship is carried only by the `service-selects-pod` fan-out edges, which MAY be cross-cluster

#### Scenario: Determinism of resolution and emission

- **WHEN** the same upstream data is resolved twice
- **THEN** candidate clusters are iterated in sorted order, the anchor-membership test and the endpoint union are order-free over the sorted candidate set, the `(source, target)` edge dedupe applies to each emitted edge, `service-selects-pod` edges dedupe by `(service-node ID, pod ID)`, and edge IDs remain deterministic UUIDv5 values over `<type>|<source>|<target>`

### Requirement: Unresolvable connection string falls back to an external node

When a `"://"` label is unresolvable, because its host is not a parseable Kubernetes `.svc` name or because the anchor cluster does not itself hold the addressed Service in its own family, the reader SHALL fall back to an external node named by the verbatim label value.

#### Scenario: Shape of the external node

- **WHEN** a `"://"` label is unresolvable
- **THEN** the endpoint becomes a node with `id` = `external/<label_value>`, `name` = `<label_value>` (verbatim, no normalisation, no trimming), `type` = `"external"`, and `labels` = `{}` (empty map, no `cluster` key)

#### Scenario: External endpoints stay visible

- **WHEN** a connection string is a truly-external URL (e.g. `https://payments.partner.example/api`), an unknown in-cluster name, or names a Service the caller's own cluster does not deploy
- **THEN** it stays visible as an `external` node
- **AND** every non-pod, non-service endpoint uses the `external` node type, whether it arises from an unresolvable `"://"` connection string or from the missing pod-UID human-label fallback

### Requirement: Missing pod-UID human-label fallback

When an endpoint's pod UID (`client_k8s_pod_uid` or `server_k8s_pod_uid`) is empty or absent AND its human-readable label (`client` or `server`) is non-empty AND that label does NOT contain the substring `"://"`, the reader SHALL promote the endpoint to an external node derived from the human label instead of dropping the edge. The fallback SHALL be unconditionally on (no knob) and SHALL apply symmetrically to client and server sides.

#### Scenario: Shape of the fallback node

- **WHEN** the fallback fires for an endpoint
- **THEN** the node has `id` = `external/<label_value>` (no cluster prefix: the endpoint is not a pod and has no cluster identity), `name` = `<label_value>` (verbatim, no normalisation, no trimming), `type` = `"external"`, and `labels` = `{}` (empty map, no `cluster` key)

#### Scenario: Connection-string labels are not handled here

- **WHEN** an endpoint has an empty UID and a label containing `"://"`
- **THEN** connection-string resolution handles it and this fallback does not

#### Scenario: One external namespace for both external paths

- **WHEN** unresolvable `"://"` connection strings and NON-URL missing-UID human labels both produce external nodes
- **THEN** both are `external/<label>` nodes sharing the same dedupe map and `id` namespace

#### Scenario: Edge cluster label is unchanged by the fallback

- **WHEN** an edge has a fallback endpoint
- **THEN** `labels.cluster` is present (set to the metric's `cluster` label) when the client side resolves to a pod, and omitted when the client side is non-pod, whether it became `service` via connection-string resolution or `external` via this fallback or the unresolvable connection-string path

#### Scenario: Empty UID and empty label drop the edge

- **WHEN** both the pod UID AND the human label are empty for an endpoint
- **THEN** the reader drops the edge, since no identity remains to construct any node

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

### Requirement: Per-endpoint resolution order

The reader SHALL resolve each endpoint through these stages in order: (1) connection-string resolution, (2) pod-UID resolution against topology with the synthesised-pod fallback, (3) the missing-UID human-label fallback, (4) drop. The human-label fallback SHALL fire after connection-string resolution and before the synthesised-pod fallback. A series with a wholly empty side SHALL be dropped before any stage runs for either side.

#### Scenario: Stage 1, connection-string resolution

- **WHEN** an endpoint's UID is empty AND its label contains `"://"` (the hardcoded check)
- **THEN** it resolves to at most ONE service node, in the caller's OWN (anchor) cluster, and only when that cluster holds the addressed `(namespace, service)` in `ServicesByNameNS`, anchored on the UID-recovered client-pod cluster when available, else the trace label
- **AND** each such service node fans out a cross-cluster `service-selects-pod` union over the backing pods of every same-family cluster holding the Service; there is NO per-family service-node fan-out, NO unknown-family / cross-family fallback, and NO endpoint-backed pruning
- **AND** when the anchor cluster does not hold the addressed Service (a sibling holding it is not enough), the endpoint resolves to an `external/<label>` node with `labels={}` (edge type `pod-calls-pod`); it is never a pod, and the resulting `pod-calls-service` edge is intra-cluster

#### Scenario: Stage 2, pod-UID resolution

- **WHEN** an endpoint's UID is non-empty
- **THEN** it resolves against topology, with the synth-pod fallback on a topology miss

#### Scenario: Stage 3, human-label fallback

- **WHEN** an endpoint's UID is empty AND its label is non-empty AND the label does NOT contain `"://"`
- **THEN** it resolves through the missing-UID human-label fallback

#### Scenario: Stage 4, drop

- **WHEN** both the endpoint's UID and its label are empty
- **THEN** the endpoint is dropped

#### Scenario: A wholly empty side drops the series before resolution

- **WHEN** a series has a wholly empty side (its pod UID AND its human label both empty)
- **THEN** the series is dropped BEFORE any resolution runs for EITHER side: no edge is emitted and no node (service, external, or synthesised pod) is materialised for either endpoint
- **AND** the other side's `"://"` label therefore cannot leak an orphan service/external subgraph for an edge that cannot exist

### Requirement: Synthesised pod node fallback

In an **unfiltered** build (no request-scoped selector dimension active), when a service-graph series references a **non-empty** pod-UID endpoint that does not appear in the topology produced for the same window, the reader SHALL synthesise a pod node and SHALL NOT drop the edge. In a **filtered** build no pod SHALL ever be synthesised.

#### Scenario: Scope of the synthesised-pod rule

- **WHEN** an endpoint's pod UID is empty, or the build is filtered
- **THEN** this rule does not apply: empty pod UIDs are handled by "Missing pod-UID human-label fallback", and a filtered build is governed by "Filtered-build admission and out-of-scope endpoints"

#### Scenario: Synthesised client pod

- **WHEN** the client side's non-empty UID is not in topology
- **THEN** the synthesised pod uses the metric's `cluster` label as its cluster value: `id="<cluster>/<client_k8s_pod_uid>"`, `labels.cluster=<cluster>`

#### Scenario: Synthesised server pod has an unknown cluster

- **WHEN** the global pod-UID index has no entry for `server_k8s_pod_uid`
- **THEN** the synthesised pod has `id="/<server_k8s_pod_uid>"` and `labels.cluster=""`, because the metric carries no `server_cluster` label and the trace pipeline only knows the source cluster

#### Scenario: Common attributes of a synthesised pod

- **WHEN** either side is synthesised
- **THEN** the node has `name="<pod-uid>"` and `type="pod"`, and its `labels` contain `namespace` when the metric provided `client_k8s_namespace_name` / `server_k8s_namespace_name`

#### Scenario: No ghost flag

- **WHEN** a pod is synthesised
- **THEN** no boolean `ghost` flag is added to `labels`; consumers detect synthesised endpoints by the absence of richer labels (e.g. a missing `node` for pods, or an empty `labels.cluster` for unknown-cluster server pods)

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

The reader SHALL attempt to resolve the server side of a `traces_service_graph_request_total` series from the client-recorded peer-address labels `client_server_address`, `client_network_peer_address` and `client_net_peer_name`, rather than dropping it, when and only when `client_k8s_pod_uid` resolves to a real topology pod (not a synthesised pod), the server side has no resolvable pod, AND the raw `server` label is exactly `"unknown"`.

#### Scenario: Trigger condition in detail

- **WHEN** the reader evaluates a `server="unknown"` series
- **THEN** "no resolvable server pod" means `server_k8s_pod_uid` is empty, or non-empty but absent from the global pod-UID index
- **AND** the enrichment is a narrow, additive carve-out from the outcome of "Virtual sentinel endpoint exclusion (user / unknown)" and does NOT apply when the client side is unresolved or when the server UID itself resolves to a topology pod

#### Scenario: The three labels are distinct OpenTelemetry attributes

- **WHEN** the reader reads the peer-address labels
- **THEN** it treats `client_server_address` as the stable `server.address` (the logical destination as the caller addressed it: a name, an IP, or a UDS name), `client_network_peer_address` as the stable `network.peer.address` (the socket-level peer address, by convention an IP), and `client_net_peer_name` as the deprecated `net.peer.name`, superseded by `server.address`

#### Scenario: The peer port label is deliberately not read

- **WHEN** a series carries `client_network_peer_port`
- **THEN** the reader does not read it: a port participates in neither peer identification (Service resolution keys on DNS name or `ClusterIP`) nor node naming, and its omission is deliberate

#### Scenario: Resulting edge follows the generic rules

- **WHEN** the enrichment resolves an endpoint
- **THEN** the edge `type` is `pod-calls-service` when the resolved target is a service node, otherwise `pod-calls-pod`, and `labels.cluster` is present (the client pod's cluster) because the client side is a resolved pod
- **AND** no new node type and no new edge type are introduced

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
- **THEN** DNS-grammar classification (stage c) succeeds, yielding `(namespace="com",
  service="example")`
- **AND** the anchor cluster does not hold that Service, so same-cluster Service-node
  resolution misses
- **AND** the reader SHALL consult the route-resolution engine before emitting any external
  node

#### Scenario: Route resolution disabled falls back to external unchanged

- **WHEN** route resolution is not configured
- **AND** a `server="unknown"` endpoint's peer address is unresolvable by the classification stages, or the
  anchor cluster does not hold the classified Service
- **THEN** the reader SHALL emit `external/<raw_peer_address_value>` with `labels = {}`,
  byte-for-byte identical to a build in which the route engine does not exist

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

### Requirement: Peer-address label precedence

The reader SHALL take the peer address from the first non-empty label among `client_server_address`, then `client_network_peer_address`, then `client_net_peer_name`. Values SHALL NOT be merged across labels, and the reader SHALL NOT fall through to a lower-precedence label when a higher-precedence one is non-empty but fails to classify. When all three are empty or absent, the endpoint SHALL be dropped.

#### Scenario: All three labels empty

- **WHEN** a triggering series carries none of the three labels, or only empty values
- **THEN** the endpoint is dropped (no node, no edge), identical to the outcome when the enrichment does not apply

#### Scenario: Why names lead and IP literals follow

- **WHEN** the precedence is chosen
- **THEN** it ranks the labels by what classification can resolve, not by recency of spelling: names are strong (the Kubernetes Service-DNS grammar and the bare-short-name form both yield a `(namespace, service)` pair that resolves to a Service node with its family-wide fan-out), while IP literals are weak (the `ClusterIP` lookup is restricted to the anchor cluster and the Pod-IP lookup resolves only an unambiguous holder in the caller's cluster family, so a loopback address, a NodePort or load-balancer address, or any off-cluster IP falls to `external`)
- **AND** therefore the name-valued stable `server.address` leads, the IP-valued stable `network.peer.address` follows, and the deprecated `net.peer.name` trails

### Requirement: Peer-address bracket truncation and port strip

Before classification, the reader SHALL truncate a peer-address value at its first `[` when that `[` is at an index greater than 0, discarding it and everything after it, and SHALL then strip an optional trailing `:<port>` suffix. A value with no such `[`, or with no splittable port, SHALL be used unchanged.

#### Scenario: Bracketed identifiers are not part of the address

- **WHEN** an instrumentation appends a bracketed connection or session identifier to the authority, e.g. `mongo.com:27017[-181]`
- **THEN** the suffix is truncated, because it is not part of the network address and prevents the host/port split from succeeding

#### Scenario: A leading bracket is the IPv6 form and is kept

- **WHEN** a value's only `[` is at index 0, as in the IPv6 bracket form `[2001:db8::1]:8080`
- **THEN** it is not truncated, because the host/port split already removes the brackets correctly and truncating would destroy a resolvable address
- **AND** a bracketed IPv6 authority that reaches the port strip intact yields the IPv6 address without its brackets

#### Scenario: Truncation is a property of the value

- **WHEN** any of the three peer-address labels supplies the value
- **THEN** the same truncation applies; the reader does NOT track which label a value came from

#### Scenario: The stripped port is kept as a hint

- **WHEN** the port strip splits out a port
- **THEN** the port is NOT discarded but carried to Istio route resolution as its highest-precedence listener-port hint

### Requirement: Peer-address classification stages

The reader SHALL classify the reduced host through these stages in order: (c) the Kubernetes Service-DNS grammar, (d) a bare short Service name in the client pod's own namespace, (e) a Service `ClusterIP` in the anchor cluster, (f) a Pod IP in the caller's cluster family, and otherwise (g) unresolvable. Stages (d), (e) and (f) SHALL apply only within the unknown-server enrichment's trigger condition.

#### Scenario: Stage (c), Service-DNS grammar

- **WHEN** the host is classified
- **THEN** the grammar of connection-string resolution applies: 2-label `<service>.<namespace>`, or 3-label headless `<pod>.<service>.<namespace>` with the leading pod-hostname dropped, and the `.svc[.<cluster-domain>]` suffix stripped

#### Scenario: Stage (d), bare short name

- **WHEN** stage (c) does not match AND the host is non-empty, contains no `.`, and is not an IP literal
- **THEN** it is a bare short Service name resolved in the client pod's own namespace, `(service=host, namespace=<client_k8s_namespace_name>)`, a grammar extension beyond connection-string resolution
- **AND** no DNS-1123 character or length validation is performed: any dot-free non-IP-literal string is a candidate, and an unmatchable one is filtered out by the Service lookup

#### Scenario: Stage (d) also sees values reduced by truncation

- **WHEN** bracket truncation or the port strip reduces a value to a dot-free label, e.g. `mongo:27017[-181]`
- **THEN** it reaches stage (d) and resolves to a Service named `mongo` in the client pod's own namespace if one exists, the same trade the stage makes for the un-bracketed `mongo:27017`

#### Scenario: Stage (e), anchor-cluster ClusterIP

- **WHEN** stages (c) and (d) fail AND the host is a valid IP literal (IPv4 or IPv6, per `net.ParseIP`)
- **THEN** it is looked up as a Service `ClusterIP` within the already-resolved client pod's own (anchor) cluster only, never any other cluster including a family sibling, and a match yields that Service's `(namespace, service)`
- **AND** this second grammar extension is evaluated after, and independently of, stage (d), which never matches an IP literal

#### Scenario: Duplicate ClusterIP is resolved deterministically

- **WHEN** two or more Services in the same anchor cluster share one `ClusterIP` (an anomaly Kubernetes prevents in a healthy cluster)
- **THEN** stage (e) selects the Service with the lexically-smaller `(namespace, service)` pair

#### Scenario: Stage (g), unresolvable

- **WHEN** no earlier stage matched, such as a multi-label non-`.svc` FQDN or an IP literal matching neither an anchor-cluster Service `ClusterIP` nor an unambiguous family Pod IP
- **THEN** the value is unresolvable at this step

#### Scenario: Which bracketed IPv6 authorities reach stage (g)

- **WHEN** a plain (non-IPv4-embedded) bracketed IPv6 authority cannot be reduced to a parseable host by truncation and the port strip
- **THEN** it does NOT reach stage (g): being dot-free and not an IP literal, stage (d) takes it as a candidate Service name and the Service lookup filters it out
- **AND** an IPv6 authority with an embedded IPv4 suffix, e.g. `[::ffff:10.0.0.5]:8080[-1]`, contains dots, fails stage (d), and DOES reach stage (g)

### Requirement: Pod-IP peer resolution

When stage (e) matched no Service `ClusterIP` and the host is a valid IP literal, the reader SHALL look it up as a Pod IP among the loaded pods of the caller's cluster family: a holder in the anchor cluster itself always wins, otherwise the family's single holding cluster. When two or more family clusters hold it, the reader SHALL resolve no pod, with no tie-break. A matched pod SHALL become the endpoint directly.

#### Scenario: Direct pod dialling is covered

- **WHEN** a caller dialled another pod's address directly, bypassing any Service, including across a cluster boundary where clusters share a flat routable network
- **THEN** the Pod-IP stage resolves that pod

#### Scenario: Ambiguous Pod IP

- **WHEN** two or more family clusters hold the address
- **THEN** no pod is resolved and the value falls through to stage (g) with the distinct external-fallback reason `unknown_server_peer_pod_ip_ambiguous`

#### Scenario: Only the caller's family is searched

- **WHEN** a cluster outside the anchor cluster's family holds the address
- **THEN** it is never a candidate
- **AND** being the family's only holder is itself the evidence that those clusters' pod CIDRs do not overlap at that address, so no service-mesh precondition is required: cross-cluster pod-to-pod reachability is a network-layer property, and a sidecar is neither necessary (a flat network needs none) nor sufficient (in a multi-network mesh the caller's sidecar is handed the east-west gateway address, never a remote Pod IP)

#### Scenario: ClusterIP wins and address-less pods never match

- **WHEN** an address could match both a Service `ClusterIP` and a Pod IP, or a pod reports no address
- **THEN** stage (e) is evaluated first and wins unconditionally, and a pod that reports no address is never a candidate

#### Scenario: Pod IP is resolved before route resolution

- **WHEN** the Pod-IP stage runs
- **THEN** it runs BEFORE Istio route resolution, so in-cluster (or in-family) pod traffic is never handed to the route engine

#### Scenario: Duplicate Pod IP within one cluster

- **WHEN** two or more pods in the SAME cluster report the identical `pod_ip` (legitimately for `hostNetwork` pods, which report their node's address, and transiently on address reuse within the window)
- **THEN** the pod with the lexically-smallest cluster-scoped id is selected, so an intra-cluster duplicate never makes the family look ambiguous

#### Scenario: A resolved pod yields a pod-calls-pod edge

- **WHEN** stage (f) yields a topology pod
- **THEN** the endpoint resolves directly to that pod, no service node is materialised and no `service-selects-pod` edge is emitted, so the generic target-driven rule makes the edge `pod-calls-pod`, which MAY cross clusters

### Requirement: Classified peer Service resolution

When stage (c), (d), or (e) yields a `(namespace, service)` pair, the reader SHALL resolve it through the same same-cluster Service-node resolution as connection strings, anchored on the already-resolved client pod's own cluster: AT MOST ONE service node, materialised iff the anchor cluster itself holds the addressed Service, with the same cross-cluster `service-selects-pod` fan-out over every same-family cluster holding it.

#### Scenario: Resolution is independent of how the Service was identified

- **WHEN** the pair came from DNS-grammar classification (stage c), bare-short-name classification (stage d), or IP-literal `ClusterIP` matching (stage e), from any of the three labels
- **THEN** it is resolved identically, including the family-wide `service-selects-pod` fan-out; only the identification step of stage (e) is restricted to the anchor cluster
- **AND** there is no anchor-recovery ambiguity, since the client side is guaranteed to be a real topology pod

### Requirement: Unknown-server route resolution before external fallback

When classification is unresolvable (stage g), OR the anchor cluster does not hold the addressed Service, the reader SHALL consult the Istio route-resolution engine of "Istio route resolution of global FQDN peers" before falling back to an external node, subject to that requirement's own preconditions. This step SHALL be reached from EVERY path that would otherwise produce an external node under the unknown-server enrichment.

#### Scenario: An endpoint without destination IPs skips the engine

- **WHEN** the endpoint carries no destination IPs
- **THEN** the engine is NEVER consulted and the endpoint falls external directly

#### Scenario: A global FQDN reaches the engine through the anchor-miss path

- **WHEN** the peer address is a global or ingress FQDN such as `api.example.com`
- **THEN** stage (c) classifies the 3-label host successfully, as service `example` in namespace `com`, and it then fails the anchor-cluster membership test, so it reaches route resolution through the "anchor cluster does not hold the addressed Service" path, NOT the unresolvable path
- **AND** an implementation that consults the engine only on stage-(g) unresolvability does NOT satisfy this requirement

### Requirement: Unknown-server external fallback uses the raw peer value

When route resolution is disabled, or does not yield a destination, the reader SHALL fall back to an external node built from the RAW peer-address value, exactly as read from the series, with NEITHER the bracket truncation NOR the port strip applied.

#### Scenario: Shape of the external node

- **WHEN** the endpoint falls back
- **THEN** the node has `id` = `external/<raw_peer_address_value>`, `name` = `<raw_peer_address_value>` (verbatim, no normalisation, no trimming), `type` = `"external"`, and `labels` = `{}` (empty map, no `cluster` key)

#### Scenario: One external node per raw value

- **WHEN** one host is dialled under several distinct bracketed identifiers, from any of the three peer-address labels
- **THEN** one external node is materialised per identifier, a consequence accepted deliberately

### Requirement: RED edge metrics on trace-derived pod / service edges

The reader SHALL attach a typed, nullable numeric metrics object to an emitted edge **iff all four** hold: (1) the edge is trace-derived from at least one `traces_service_graph_request_total` series; (2) **both** resolved endpoints are `type="pod"` or `type="service"` nodes; (3) the edge is NOT the route-hit ingress chain's caller → ingress-service entry hop; (4) its in-scope contributing series have a finite summed rate greater than zero.

#### Scenario: Meaning of trace-derived and of a pod endpoint

- **WHEN** eligibility is evaluated
- **THEN** a trace-derived edge is one produced from service-graph series, not synthesised from topology or from a route-resolution outcome
- **AND** a `type="pod"` endpoint may be a real topology pod or a synthesised pod

#### Scenario: How an endpoint was identified does not affect eligibility

- **WHEN** an endpoint was identified by a non-empty `client_k8s_pod_uid` / `server_k8s_pod_uid`, a `"://"` connection string resolved to an in-cluster Service, a `server="unknown"` peer address classified as a Service DNS name, a peer address matched against a Service `ClusterIP` or against a family Pod IP, or an Istio route-engine resolution to a backend Service
- **THEN** each satisfies condition 2 equally
- **AND** the edge `type` is not a condition: both `pod-calls-pod` and `pod-calls-service` edges MAY carry the object, since the type is a consequence of what the target resolved to

#### Scenario: Edges that never carry the metrics object

- **WHEN** an edge has an `external` node on either side (an unresolvable `"://"` connection string, the missing-UID human-label fallback, or an unresolved peer address), or is a `service-selects-pod` edge (synthesised fan-out; no series names the individual backing pod), or is the synthesised ingress-chain hop from an ingress-gateway pod to the backend service (no contributing series exists), or is a topology-derived edge (`pod-mounts-pvc`, `pod-to-node`, `pvc-to-netapp-aggr`)
- **THEN** it carries no metrics object

#### Scenario: The chain's entry hop is never measured

- **WHEN** a route-hit ingress chain emits the caller → ingress-service entry hop beside the retained caller → backend edge
- **THEN** only the caller → backend edge, which names the actual destination, carries the measurement, because the two are projections of the SAME observed call and measuring both would make one request count twice in any sum taken over the chain

#### Scenario: Attaching metrics leaves the edge otherwise unchanged

- **WHEN** a metrics object is attached
- **THEN** no numeric value appears anywhere in the edge's `labels` map, which remains strictly `map[string]string`, and the edge's `id`, `type`, `source`, `target`, and `labels` are unchanged

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

### Requirement: RED metrics object fields

The metrics object SHALL carry `rate` (REQUIRED whenever the object is present) and the OPTIONAL `error_rate` and `p90_server_ms`, each computed only over the edge's in-scope contributing series.

#### Scenario: rate

- **WHEN** the object is present
- **THEN** `rate` is requests per second over the request window, `rate(traces_service_graph_request_total[<window>])` summed over the in-scope contributing series, and it is strictly greater than zero by construction because zero-rate series never produce an edge

#### Scenario: error_rate

- **WHEN** the failure counter was read
- **THEN** `error_rate` is the summed `rate(traces_service_graph_request_failed_total[<window>])` over the SAME in-scope contributing series divided by `rate`, clamped to the closed interval `[0, 1]`
- **AND** a value of `0` means "the failure counter was read successfully and reported no failures", never "the failure counter could not be read"

#### Scenario: p90_server_ms

- **WHEN** a duration histogram is available for the edge
- **THEN** `p90_server_ms` is the 90th percentile of the **server-observed** request duration, in **milliseconds**
- **AND** the quantile `0.90` and the server observation side match Grafana's documented service-graph queries so the two tools measure the same thing, although the values are not expected to be numerically equal, because Grafana aggregates by service name while this API aggregates by pod pair

### Requirement: Span-link series are out of measurement scope

A contributing series, a `traces_service_graph_request_total` sample that resolved onto the edge's `(source, target)` pair, SHALL be in scope iff it does NOT carry the dimension `edge_relation="link"`. An out-of-scope series SHALL contribute to no rate, no error numerator, and no duration bucket, while the edge itself SHALL still be emitted.

#### Scenario: What a link series describes

- **WHEN** a series carries `edge_relation="link"`
- **THEN** it describes a virtual edge the producer materialised from a **span link** rather than from a paired client/server span: the interaction traverses a queue or a database and its two spans belong to different trace contexts, so only its measurement is suppressed

#### Scenario: The link dimension is a fixed contract

- **WHEN** the reader matches the dimension
- **THEN** the label name and the value `link` are a fixed, case-sensitive contract with NO configuration surface

#### Scenario: Mixed and all-link contributions

- **WHEN** an edge's contributing series mix in-scope and link series
- **THEN** it is measured over the in-scope subset only
- **AND** when they are ALL link series the in-scope set is empty, condition 4 of the attachment rule fails, and the edge carries no metrics object at all

### Requirement: RED source series and selector consistency

In addition to `traces_service_graph_request_total`, the reader SHALL read `traces_service_graph_request_failed_total` and `traces_service_graph_request_server_seconds_bucket` for the same window and evaluation instant, at the same label granularity as the total counter. Both selectors SHALL carry the request-total selector's sentinel-exclusion fragment and SHALL additionally exclude series carrying `edge_relation="link"`.

#### Scenario: Granularity of the two companion series

- **WHEN** the reader reads the companion series
- **THEN** the Errors counter is read at the total counter's label granularity so it joins to the exact same series, and the Duration classic histogram is read at that granularity too, additionally carrying the `le` bucket boundary

#### Scenario: Why the histogram is never aggregated upstream

- **WHEN** the duration-histogram query is built
- **THEN** it carries no upstream aggregation, because no low-cardinality label subset identifies an edge once endpoints may be resolved from peer addresses and connection strings, and a group-by would merge the latency distributions of unrelated edges that happen to share the retained labels

#### Scenario: The request-total selector keeps link series

- **WHEN** the reader builds the `traces_service_graph_request_total` selector
- **THEN** it carries no `edge_relation` matcher, because the span-link edge must still be emitted
- **AND** because a negative label matcher retains series on which the label is absent, the exclusion on the companion selectors is inert for a producer that does not configure the dimension

#### Scenario: Both matchers are fixed metric-selection contracts

- **WHEN** any request is served
- **THEN** the sentinel and span-link matchers are identical, being request-invariant metric-selection contracts of the same class as the sentinel exclusion and the node-condition selector, NOT caller-supplied filters

#### Scenario: Query-layer exclusions are also enforced during resolution

- **WHEN** the companion series are joined
- **THEN** the queried population is a **superset** of the attachment population, because the attachment rule's endpoint-node-type and chain-position conditions have no label-level equivalent, and series returned for a pair that turns out ineligible go unused
- **AND** every query-layer exclusion applied to the two companion selectors is also enforced during resolution, so a qualifying edge never has its failure or duration series filtered away upstream and then reported as `error_rate: 0` or as lacking a histogram

#### Scenario: Series and prefixes that are never read

- **WHEN** the reader reads the RED series
- **THEN** both companion series are queried at their bare names, like every series the server reads (there is no metric-name prefix; they belong to the trace-pipeline exporter family, not to kube-state-metrics)
- **AND** the reader does not read `traces_service_graph_request_client_seconds_*`, nor the `_sum` / `_count` companions of the server histogram

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

#### Scenario: RED series are queried at their bare names

- **WHEN** the reader issues the failure-counter and duration-histogram queries
- **THEN** they address `traces_service_graph_request_failed_total` and `traces_service_graph_request_server_seconds_bucket` by their bare names; no configuration renames or prefixes them

### Requirement: RED join and deterministic aggregation

The reader SHALL aggregate an edge's metrics over its **in-scope contributing series** (those not carrying `edge_relation="link"`) and no other, joining each companion series to a contributing series by exact label identity. Every attached value SHALL be a pure function of the upstream data, independent of the arrival order of series within a query result.

#### Scenario: Why several series map to one edge

- **WHEN** a dimension the edge identity does not carry, such as `connection_type`, differs between series, or two series carry different `cluster` labels that resolve to the same client pod
- **THEN** those upstream series legitimately resolve to a single edge and are aggregated

#### Scenario: rate sums only the in-scope series

- **WHEN** an edge's `rate` is computed
- **THEN** it is the SUM of the in-scope contributing series' rates, and an out-of-scope series is not counted, so the Rate denominator, the Errors numerator, and the Duration buckets all derive from one identical series set

#### Scenario: Join by exact label identity

- **WHEN** a companion vector is joined
- **THEN** each of its series joins to an in-scope contributing series by that series' exact label identity: all labels except the metric name, and for the duration histogram except `le` as well
- **AND** the reader records that mapping while resolving the request-total vector and does not re-derive an edge's endpoints from labels in a second pass

#### Scenario: Quantile algorithm

- **WHEN** `p90_server_ms` is computed
- **THEN** the matched bucket counts of all in-scope contributing series are summed per `le` boundary first, and the 90th percentile is taken from the **summed** bucket set using the classic cumulative-bucket convention with linear interpolation inside the bucket containing it (the same algorithm as a PromQL `histogram_quantile(0.9, ...)`), then converted from seconds to milliseconds
- **AND** per-series quantiles are never computed and then averaged or otherwise combined

#### Scenario: Deterministic summation and rounding

- **WHEN** two builds run over identical upstream data
- **THEN** order-independent summation and rounding each attached value before serialisation make the response bodies byte-identical (see the `graph-api` "Deterministic response body" requirement)

#### Scenario: Rounding is to significant digits

- **WHEN** an attached value is rounded
- **THEN** it is rounded to a fixed number of **significant digits**, identically for all three fields, never to a fixed number of decimal places, and a non-zero value NEVER rounds to `0`
- **AND** this matters because `rate` is a per-second value whose magnitude scales inversely with the caller's window and `error_rate` can be legitimately tiny on a high-traffic edge, so an absolute precision floor would contradict the guarantee that an emitted `rate` is strictly greater than zero and collide with the meaning of `error_rate == 0` ("read successfully, no failures")

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

Neither companion RED query SHALL be able to fail a build: the reader SHALL degrade the affected field, continue to emit the graph, and SHALL NOT alter any node, edge, `id`, `type`, `source`, `target`, or `labels`. Each degradation SHALL be surfaced as aggregated operator evidence in the logs, naming the query and the reason, not per edge. An error in the `traces_service_graph_request_total` query SHALL still fail the build.

#### Scenario: Failure-counter degradation

- **WHEN** the failure-counter query returns an error, or the metric does not exist upstream
- **THEN** `error_rate` is OMITTED from every edge's metrics object rather than reported as `0`, so an absent measurement is never presented as a measured absence of errors
- **AND** when the query succeeds but has no series matching a given edge, that edge's `error_rate` is `0`

#### Scenario: Duration-histogram degradation

- **WHEN** the duration-histogram query returns an error, the metric does not exist upstream, or its series carry no classic `le` bucket boundaries (for example because the producer emits native histograms, which a store may expose as `vmrange` buckets)
- **THEN** `p90_server_ms` is OMITTED from every affected edge's metrics object

#### Scenario: Both companion queries fail or return nothing

- **WHEN** both companion queries fail or return nothing
- **THEN** qualifying pod-to-pod edges still carry a metrics object containing only `rate`

#### Scenario: Non-finite samples never reach the response

- **WHEN** an upstream sample is `NaN` or `+Inf`, which exposition formats accept but JSON cannot represent
- **THEN** the reader skips non-finite samples when accumulating, omits the metrics object entirely when an edge's aggregated `rate` is not a finite value greater than zero, and omits `p90_server_ms` when the computed quantile is not finite
- **AND** degrading one edge is always preferred to one poisoned sample making the whole body unencodable and failing the request

#### Scenario: A companion read successfully but joined to nothing

- **WHEN** either companion vector, considered independently, is a non-empty result none of whose series matched any edge
- **THEN** the reader surfaces that signature as aggregated operator evidence under its own distinct reason, so a label-set divergence between the request counter and that companion is diagnosable
- **AND** this matters because for the failure counter it would otherwise read as `error_rate: 0` on every edge, and for the histogram as a producer that emits no histogram

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

The reader SHALL read the `edge_relation` label of every `traces_service_graph_request_total` series. A series whose value is exactly `"link"` SHALL resolve both endpoints through the ordinary resolution ladder unchanged, and its edge SHALL carry `labels.relation = "link"`. Any other `edge_relation` value (absent, empty, or an unrecognised string) SHALL be ignored, and its edge SHALL carry no `relation` key.

#### Scenario: What a link series represents

- **WHEN** a series carries `edge_relation="link"`
- **THEN** it is a span-link-derived logical edge whose client is the producer pod and whose server is the consumer pod, joined across trace IDs through a broker

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
  sides with no `"://"` label (the self-loop UID guard does not fire)
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

### Requirement: Span-link transport pairs

For each side of a `"link"` series that resolved to a **real topology pod**, the reader SHALL derive that side's broker (via) node ID from the side's own peer-address labels, through the unknown-server classification chain anchored on that side's own pod cluster, and SHALL record `(that side's pod, derived broker node ID)` as a **transport** pair.

#### Scenario: Peer-address labels per side

- **WHEN** a via node is derived
- **THEN** the client side reads `client_server_address` / `client_net_peer_name`, with `client_dns_answers` / `client_server_port` as route-resolution dimensions
- **AND** the server side reads the mirrored `server_server_address` / `server_net_peer_name`, with `server_dns_answers` / `server_server_port`

#### Scenario: The same classification chain as the unknown-server enrichment

- **WHEN** a side's peer address is classified
- **THEN** it passes bracket truncation and port split, the Kubernetes `.svc` DNS grammar, the bare short name in the pod's own namespace, the anchor-cluster ClusterIP, the family Pod IP, the prefetched route-engine index, and the raw-value external fallback

#### Scenario: Sides are independent

- **WHEN** a side has no peer-address value, or did not resolve to a real topology pod
- **THEN** that side contributes no transport pair, and the other side is unaffected

### Requirement: Via derivation is lookup-only

Via derivation SHALL be lookup-only: it MUST NOT materialise any service, external, or synthesised node, MUST NOT emit any `service-selects-pod` or route-chain edge, and MUST NOT log route-engine external-fallback reasons.

#### Scenario: A routed hit derives the backend only

- **WHEN** the route-index entry for a side is a routed hit
- **THEN** the derived ID is the BACKEND service ID only, never the ingress entry-point, with no `role` marking and no chain synthesis

#### Scenario: Every other outcome derives the raw external ID

- **WHEN** the route-index outcome is anything else, the entry is missing, the destination IPs are absent, or the engine is unconfigured
- **THEN** the derived ID is the external node ID of the raw peer-address value, identical to the ID the materialising path would have produced for the same series

### Requirement: Relation marking precedence

At edge-build time, an emitted `pod-calls-pod` or `pod-calls-service` edge whose `(source, target)` pair was recorded as a link pair SHALL carry `labels.relation = "link"`; otherwise, if recorded as a transport pair, `labels.relation = "transport"`; otherwise no `relation` key. `link` SHALL take precedence over `transport` for the same pair and over any plain series aggregated into the same edge.

#### Scenario: Edges that are never marked

- **WHEN** an edge is a `service-selects-pod` edge or a synthesized route-chain edge
- **THEN** it never carries a `relation` key

#### Scenario: An unmatched transport pair is a no-op

- **WHEN** a transport pair has no matching emitted edge
- **THEN** nothing is emitted for it (the reader MAY log one aggregated debug line) and the reader MUST NOT synthesise an edge for it

#### Scenario: Marking is order-free and identity-preserving

- **WHEN** marking is applied
- **THEN** it is a pure function of the accumulated pair sets, independent of sample arrival order, and it MUST NOT alter edge identity (the UUIDv5 input remains `type|source|target`)

### Requirement: Unknown-server link series contribute no markers

A `"link"` series whose raw `server` label is exactly `"unknown"` AND whose server side has no resolvable topology pod recovered no consumer, and SHALL contribute **no relation markers at all**: no link pair, no transport pair, and no per-side via pair. Transport pairs SHALL be recorded ONLY by series that also record their link pair.

#### Scenario: The producer-to-broker edge stays an ordinary network edge

- **WHEN** such a series is resolved
- **THEN** its pair is not recorded as link, because the resolved target is the broker and not the consumer, and its server side still resolves through the unknown-server peer-label enrichment, so the producer → broker edge is byte-identical to the enrichment's ordinary, unmarked outcome

#### Scenario: Why a transport marker needs a link edge

- **WHEN** the frontend renders `transport`
- **THEN** it reads it as the network hop backing a rendered logical edge, so a transport marker with no accompanying link edge would demote a real network dependency to a dashed line that backs nothing
- **AND** a transport-marked edge in the built graph therefore always coexists with a link-marked edge from the same series set; projection filters MAY still exclude either edge from a filtered view, and marking never depends on the projection

#### Scenario: A degraded server endpoint keeps the link marking

- **WHEN** a `"link"` series' server side degrades to a synthesised pod or to the missing-UID external fallback
- **THEN** it keeps its **link** marking, since the logical claim stands even when an endpoint is degraded

### Requirement: Span-link route keys share the route prescan

The prescan SHALL collect route keys for both sides' peer labels of `"link"` series, each side only when its own pod resolved and anchored on that pod's cluster, deduplicated with the keys ordinary `server="unknown"` series derive. A broker FQDN shared by link and non-link series in one anchor cluster SHALL cost at most ONE route-engine store read per build.

#### Scenario: Same skip chain as the unknown-server collection

- **WHEN** a link-series side is in-cluster-resolvable or carries no destination IPs
- **THEN** no route key is collected for it, exactly as for an unknown-server endpoint

### Requirement: Ingress route-path marking

A `service` node materialised as an ingress entry point SHALL carry a `role` key on its `labels` with exactly one of the values `ingress-gateway` or `ingress-lb`, so the two shapes a routed global-FQDN hit ("Full ingress chain for routed global-FQDN hits") emits, the ingress chain and the direct `caller pod → backend service` edge, are distinguishable without traversal. Every other `service` node SHALL carry no `role` key.

#### Scenario: Meaning of the two role values

- **WHEN** an ingress entry point is marked
- **THEN** `ingress-gateway` marks the entry hop of a routed hit's chain, with gateway pods and a synthesized `pod-calls-service` hop to the backend behind it, and `ingress-lb` marks the destination of the "Ingress LB Service fallback", with no routed backend behind it
- **AND** a consumer can therefore render or toggle the gateway path independently of the direct dependency

#### Scenario: No new node or edge type

- **WHEN** a node is marked or left unmarked
- **THEN** the key is absent (not empty-valued) on a non-entry-point node, the marked node remains `type="service"`, and the synthesized `ingress gateway pod → backend service` hop remains typed `pod-calls-service` (no dedicated edge type)

#### Scenario: Marking is monotone and order-free

- **WHEN** series are resolved in any order, including repeated series sharing one route key
- **THEN** `ingress-gateway` overwrites, `ingress-lb` is written only into an unset value, marking is idempotent, and a value is never cleared or downgraded

#### Scenario: Marking changes nothing else

- **WHEN** marking runs
- **THEN** it occurs only after an ingress `service` node has been successfully materialised, so every chain-precondition degrade produces no marker and leaves the pre-existing direct-edge shape
- **AND** the direct `caller pod → backend service` edge carries no additional label, no resolution behaviour, precondition, external fallback, external-fallback reason, engine outcome, upstream query, or store read is altered, and output with route resolution disabled is unchanged

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

When route resolution is enabled (`KSG_ROUTE_STORE_DSN` / `--route-store-dsn` non-empty), the server SHALL accept optional ClickHouse native auth credentials from `KSG_ROUTE_STORE_USERNAME` and `KSG_ROUTE_STORE_PASSWORD` only, set together or both left unset, and MUST NOT register CLI flags for them. Credential values MUST NEVER appear in logs, spans, metric labels, or error messages.

#### Scenario: Env credentials override DSN userinfo

- **WHEN** both env vars are set and the DSN also embeds userinfo
- **THEN** the dial uses the env-var credentials, overriding the userinfo embedded in the DSN

#### Scenario: Startup logs only whether auth is configured

- **WHEN** the server starts with route resolution enabled
- **THEN** it MAY log only a boolean indicating whether route-store auth is configured, never a credential value

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

The configuration state the route-resolution engine translates SHALL name each resource version exactly once, regardless of how many destination IPs the request carries. A multi-IP request SHALL therefore produce the same outcome a single-IP request against the same configuration produces.

#### Scenario: Per-IP loads are unioned with deduplication

- **WHEN** the request carries several destination IPs, so the engine loads the selected ingress cluster's configuration once per IP and unions the results, and two IPs are served by the same ingress Service (a dual-stack Service publishing an IPv4 and an IPv6 address, or several addresses of one load balancer) and return overlapping rows
- **THEN** the union deduplicates by resource identity (cluster, namespace, name, and version start)
- **AND** the per-gateway translate input contains at most one configuration entry per `(kind, namespace, name)` and at most one registry entry per backend Service

#### Scenario: A duplicated resource is not an error

- **WHEN** a resource is returned by more than one per-IP load
- **THEN** it does not surface as a translation error and does not cause the endpoint to fall back to an external node

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

The engine SHALL resolve a VirtualService route's `destination.host` to a backend Service identity using the same rules istiod itself applies, so the destination the engine reports is the destination a real mesh would route to: a dot-free host SHALL be resolved within the owning VirtualService's namespace, and a host containing a dot SHALL NOT be expanded.

#### Scenario: A short name resolves to the in-cluster FQDN

- **WHEN** a `destination.host` contains no dot
- **THEN** it resolves within the owning VirtualService's namespace to the in-cluster Service FQDN `<name>.<namespace>.svc.<cluster domain>`
- **AND** that identity is used consistently for the backend Service lookup, the translation registry, and the parse of the resulting Envoy cluster string

#### Scenario: A dotted host is already qualified

- **WHEN** a `destination.host` contains a dot but is not a full in-cluster Service FQDN (for example `checkout.shop` or `checkout.shop.svc`)
- **THEN** it names no Service in the registry, no Service identity is inferred from it, and the endpoint falls back to an external node as for any unresolvable destination

#### Scenario: Envoy cluster strings parse only a well-formed FQDN

- **WHEN** the engine parses an Envoy cluster string's host
- **THEN** it is a Service identity only when it is a well-formed `<name>.<namespace>.svc.<cluster domain>` with exactly two leading labels, and any other shape is unresolvable rather than parsed by prefix

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

The IP-to-Gateway 3-hop SHALL be a pure function of the configuration state at the resolution instant, independent of the order in which the store returns rows: an ambiguous ingress Service identity SHALL degrade, several matching ingress Deployments SHALL contribute the union of their pod labels, and a candidate Gateway SHALL be identified by `(namespace, name)` throughout.

#### Scenario: Hop 1 degrades on an ambiguous ingress Service

- **WHEN** more than one distinct Service identity carrying the destination IP is live at the instant (Hop 1, IP to ingress Service)
- **THEN** the hop produces no candidate gateways rather than selecting one, matching the ambiguity rule the ingress LB Service identity dedup applies to the same situation
- **AND** a single identity with several versions is not an ambiguity, since version liveness resolves it before this rule applies

#### Scenario: Hop 2 takes the union of matching Deployments

- **WHEN** more than one ingress Deployment satisfies the ingress Service selector (Hop 2, selector to workload labels), the normal state during a revision-based canary gateway upgrade
- **THEN** the hop takes the **union** of their pod labels, matching the label union the store query itself computes, and does not select one Deployment

#### Scenario: Gateway identity includes the namespace

- **WHEN** a candidate Gateway's configuration and bound VirtualServices are rebuilt for translation
- **THEN** it is still identified by `(namespace, name)`, and a same-named Gateway in another namespace present in the loaded rows is not selectable and contributes none of its bound VirtualServices

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

Route resolution SHALL NOT let its per-build cost grow without bound on the request path. Within one build the deduplicated route keys SHALL be resolved with bounded concurrency, the number of keys resolved SHALL be capped, and a truncation SHALL be logged with the number of dropped keys, never silently.

#### Scenario: Dropped keys keep the external fallback

- **WHEN** the cap drops keys
- **THEN** their endpoints fall back to the pre-existing external-node behaviour, exactly as an unanswered key does when the build deadline fires

#### Scenario: Concurrency does not change the index

- **WHEN** keys are resolved concurrently
- **THEN** each key's answer is independent of every other key's, so the resolved index's contents are unchanged, and any per-build memoisation shared across concurrent resolutions is safe for concurrent use

#### Scenario: Independent keys resolve concurrently

- **GIVEN** several distinct route keys collected from one service-graph vector
- **WHEN** the reader resolves them
- **THEN** the resulting index is identical to the index a strictly serial resolution produces

#### Scenario: Truncation is reported

- **GIVEN** a build whose collected route keys exceed the cap
- **WHEN** the reader resolves them
- **THEN** the number of dropped keys is logged and the corresponding endpoints fall back to external nodes

### Requirement: Resolution errors carry no routing outcome

When route resolution fails with an error (a store read failure, a translation failure, or a matcher failure), the engine SHALL NOT also report a routing outcome that is meaningful to the caller, and in particular SHALL NOT report the outcome that gates the ingress LB Service fallback, so an infrastructure failure is never mistaken for "no Istio Gateway serves this host".

#### Scenario: Matcher results are recovered completely or not at all

- **WHEN** the matcher's per-query results are recovered
- **THEN** a result is recovered for every query posed, or the parse reports an error; a partially recovered result set is never returned

#### Scenario: Store failure is not reported as a missing gateway

- **GIVEN** the route store fails a read
- **WHEN** the engine resolves an endpoint
- **THEN** the failure is reported as an error with no routing outcome, and the endpoint falls back to an external node

#### Scenario: Incomplete matcher output errors

- **GIVEN** the matcher's output yields fewer results than the number of queries posed
- **WHEN** the engine parses that output
- **THEN** the parse reports an error rather than returning a result for any query

### Requirement: Filtered-build admission and out-of-scope endpoints

When any request-scoped selector dimension (`az`, `env`, `cluster`, `namespace`) is active, the reader SHALL admit a service-graph series only if **at least one** of its endpoints resolves to loaded topology, SHALL resolve every other endpoint of an admitted series to an external node, and SHALL NOT synthesise a pod node for any endpoint. None of these rules SHALL apply to an unfiltered build.

#### Scenario: Which series the rules apply to

- **WHEN** a filtered build runs, so the topology is a subset of the estate while the service-graph series are complete
- **THEN** the rules apply to every series that survives the sentinel exclusion and the wholly-empty-side drop, and the result is a pure function of the series set and the loaded topology (order-free, deterministic)

#### Scenario: Meaning of loaded topology and of a dropped series

- **WHEN** admission is decided
- **THEN** an endpoint reaches loaded topology if it is a pod present in the topology, or a Service present in the loaded service index (via connection-string resolution, the unknown-server peer ladder, or route resolution)
- **AND** a series whose endpoints both fail to reach loaded topology is dropped before any node or edge is materialised, whatever the unresolved sides would otherwise have become, which keeps the out-of-scope estate from rendering as a web of `external` nodes and bounds the output to the in-scope workload's direct neighbourhood

#### Scenario: Forms of an out-of-scope endpoint

- **WHEN** an endpoint of an admitted series does not reach loaded topology
- **THEN** it resolves to `external/<raw label>` with `labels={}`: a non-empty pod UID that is not loaded falls to that side's `client` / `server` human label exactly as the missing-UID human-label fallback does (`external/cart`), a `"://"` connection string whose Service is not loaded keeps the verbatim label, and a peer address that matches no loaded Service `ClusterIP` or Pod IP keeps the raw peer value
- **AND** a side whose UID is not loaded AND whose human label is empty is dropped together with its series, as a wholly empty side is

#### Scenario: Two-phase materialisation

- **WHEN** a series is resolved and later dropped
- **THEN** it leaves no node or edge behind, because service nodes, `service-selects-pod` fan-out edges, route-chain edges, and external nodes are materialised only for admitted series

#### Scenario: Existing conventions are unchanged

- **WHEN** a filtered build emits external endpoints
- **THEN** an edge whose client side is `external` carries no `labels.cluster`, and an edge with an `external` endpoint never carries `metrics`
- **AND** the unknown-server peer-label enrichment still requires the client to be a loaded topology pod (an out-of-scope client with `server="unknown"` is dropped), same-named out-of-scope peers collapse into one external node, and an out-of-scope caller of a loaded pod or Service appears as an inbound external

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

The reader SHALL support an OPTIONAL Istio route-resolution engine that resolves a global / ingress FQDN peer to the Kubernetes Service the Istio Gateway and VirtualService configuration routed it to **at a single instant**, the END of the request's own time window. The engine SHALL be consulted ONLY from "Unknown-server peer-label enrichment", and SHALL be disabled, the default, when its route-store connection string is empty.

#### Scenario: One instant, one outcome

- **WHEN** the engine resolves an endpoint
- **THEN** it evaluates the configuration **as of that one instant** and does not resolve per-version over the window: exactly one configuration state is consulted, exactly one outcome is produced, and the request window's start plays no part in route resolution

#### Scenario: A disabled engine changes nothing

- **WHEN** the route-store connection string is empty
- **THEN** the engine is **disabled** and the reader's behaviour is byte-for-byte identical to its behaviour without the engine

#### Scenario: Resolution is prefetched outside the parse

- **WHEN** a build uses route resolution
- **THEN** resolution is performed outside the pure service-graph parse and its results are supplied to the parse as a prefetched index, so the emitted graph remains a deterministic function of the upstream data and the resolved destinations

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

### Requirement: Route-resolution inputs

For one candidate endpoint the reader SHALL supply the engine with the caller cluster, the host (the port-stripped peer address), the path, the listener port, the destination IPs taken from the `client_dns_answers` dimension, and the resolution instant. The destination IPs SHALL be a **precondition**: an endpoint carrying no parseable destination IP SHALL NOT be submitted to the engine.

#### Scenario: The caller cluster only derives a family and breaks ties

- **WHEN** the caller cluster is supplied
- **THEN** it is the already-resolved client pod's own cluster, used ONLY to derive the cluster-family key and to break candidate ties in ingress-cluster selection; it never by itself scopes any route-store query

#### Scenario: The path is fixed

- **WHEN** the path is supplied
- **THEN** it is always `"/"`, because the service-graph metric carries no HTTP path or route dimension

#### Scenario: Missing destination IPs skip the engine with their own reason

- **WHEN** an endpoint carries no parseable destination IP
- **THEN** no store read occurs, the endpoint falls back to the external node directly, and the reader records a distinct diagnostic reason for the skip, separate from every engine outcome

#### Scenario: The resolution instant is fixed

- **WHEN** the resolution instant is supplied
- **THEN** it is the END of the build's own time window, not configurable, and the same instant the service-graph samples are evaluated at

#### Scenario: Listener-port precedence

- **WHEN** the listener port is derived
- **THEN** it is the `:<port>` suffix split from the peer-address value when present, otherwise the OPTIONAL `client_server_port` or `client_net_peer_port` dimension when present, otherwise the default **443**

### Requirement: Ingress-cluster selection

The destination IPs SHALL select the ingress cluster, the caller cluster contributing only its family key and a tie-break. Each destination IP SHALL be selected independently and all selections MUST agree on one cluster. Candidate sets and store snapshots SHALL NEVER be unioned across clusters, and every later step SHALL operate on the selected cluster only.

#### Scenario: Per-IP candidate sets

- **WHEN** the engine selects for one destination IP
- **THEN** it probes the store for the candidate set `G`, the clusters whose ingress Service carrying that IP was live at the resolution instant, and derives `F`, the subset of `G` in the caller's cluster family (the same digit-run-collapsing family rule used by `service-selects-pod` fan-out)

#### Scenario: Per-IP selection rules

- **WHEN** the candidate sets are known
- **THEN** exactly one family candidate selects that cluster; several family candidates select the caller's own cluster if it is among them, otherwise **ambiguous**; no family candidate and exactly one global candidate selects that cluster; no family candidate and several global candidates select the caller's own cluster if it is among them, otherwise **ambiguous**; no candidate at all is **no ingress**

#### Scenario: Combining several destination IPs

- **WHEN** the request carries several destination IPs
- **THEN** every IP yielding "no ingress" degrades as **no ingress**, and any other disagreement (an ambiguous IP, disagreeing selections, or a mix of "no ingress" and a selected cluster) degrades as **ambiguous**

#### Scenario: Everything after selection is single-cluster

- **WHEN** a cluster is selected
- **THEN** snapshot load, gateway narrowing, host disambiguation, translation, and route matching operate on that single cluster only
- **AND** the engine narrows the candidate Gateways to those reachable from the destination IPs within it

### Requirement: Route-store access is scoped and read-only

Every route-store *snapshot* query SHALL be scoped to the selected ingress cluster; the ONLY cross-cluster store read SHALL be the ingress probe, and its result SHALL be deterministic. The reader SHALL be strictly read-only against the store and SHALL validate the expected schema at startup, failing fast on drift.

#### Scenario: The ingress probe

- **WHEN** the engine reads across clusters
- **THEN** the read answers only "which clusters had an ingress Service with this IP at the resolution instant", and its result is deduplicated and ordered

#### Scenario: No writes and no silent schema drift

- **WHEN** the reader starts or queries the store
- **THEN** it creates no schema and writes nothing, and a schema that does not match the expected one fails startup rather than silently returning empty results

### Requirement: Route-store shape tolerance

The reader SHALL NOT treat a stale open row and its closing rewrite as two live versions, SHALL tolerate unknown fields in stored resource specs rather than failing the query, and SHALL bind a VirtualService gateway reference in the bare `<name>` form exactly as the qualified `<namespace>/<name>` form.

#### Scenario: Version-close rewrites are deduplicated at query time

- **WHEN** the exporter's version-close operation REWRITES the previous open row, so until background merges collapse them a stale open row and its closing rewrite coexist
- **THEN** the reader deduplicates them at query time, so results do not depend on merge timing

#### Scenario: Specs follow the cluster's CRD version

- **WHEN** a stored resource spec, the API server's JSON verbatim, carries fields from a different CRD version
- **THEN** the reader tolerates the unknown spec fields

#### Scenario: Bare gateway names are namespace shorthand

- **WHEN** a VirtualService binds its gateway by the bare `<name>` form
- **THEN** it is Istio shorthand for a gateway in the VirtualService's own namespace and binds exactly as the qualified form does

### Requirement: As-of version selection

Every resource version the engine consults (ingress Service, ingress Deployment, Gateway, VirtualService, backend Service) SHALL be the version **live at the resolution instant**: its validity interval starts at or before that instant and ends strictly after it.

#### Scenario: Ended and future versions do not participate

- **WHEN** a version ended at or before the instant, or begins after it
- **THEN** it does not participate in resolution, translation, or ingress identification

#### Scenario: Duplicate-row discipline precedes the liveness test

- **WHEN** liveness is evaluated
- **THEN** the duplicate-row discipline of "Route-store shape tolerance" has already been applied, so a stale open row whose closing rewrite has not yet merged can never present itself as the live version

### Requirement: Routed hit resolution

A routed hit SHALL yield a destination `(cluster, namespace, service)` whose cluster is the engine-selected ingress cluster. The reader SHALL resolve it through the SAME same-cluster Service-node resolution every other path uses, anchored on the **selected ingress cluster**, and SHALL discard the destination's port and DestinationRule subset.

#### Scenario: One service node with the family fan-out

- **WHEN** a hit is resolved
- **THEN** AT MOST ONE service node materialises, iff the selected cluster holds the Service in topology, with the same cross-cluster `service-selects-pod` fan-out over its family

#### Scenario: The resulting edge may cross clusters

- **WHEN** the selected cluster differs from the caller's
- **THEN** the resulting `pod-calls-service` edge crosses clusters, and the edge-type registry declares `may_cross_cluster: true` for `pod-calls-service`
- **AND** connection-string-resolved edges remain intra-cluster by construction, and per-edge cross-cluster status is still derived by comparing the resolved endpoints' clusters

#### Scenario: No new graph vocabulary

- **WHEN** a hit is emitted
- **THEN** **No new node type, no new edge type, no new node attribute, and no new `labels` key** are introduced

### Requirement: Route-resolution degradation

Every route-resolution failure SHALL fall back to the external node specified in "Unknown-server peer-label enrichment", and route resolution SHALL NEVER fail a build. The reader SHALL record distinct diagnostic reasons for "no listener on the derived port", "no candidate ingress cluster", and "ambiguous ingress cluster" outcomes.

#### Scenario: Failures that fall back

- **WHEN** the engine is disabled, the endpoint carries no destination IPs, the store errors, no candidate ingress cluster exists for the IPs, the candidate set is ambiguous, no Gateway serves the host, no route matched, no listener exists on the derived port, or resolution times out
- **THEN** the endpoint falls back to the external node

#### Scenario: A mis-derived port is diagnosable

- **WHEN** the outcome is "no listener on the derived port"
- **THEN** its reason is separate from "no route matched", so a mis-derived port is diagnosable

### Requirement: Full ingress chain for routed global-FQDN hits

When the Istio route-resolution engine produces a routed hit, it SHALL additionally attempt to recover the identity of the **ingress LB Service** the destination IP set uniquely maps to inside the already-selected ingress cluster's loaded snapshot. When an identity is recovered and every chain precondition holds, the reader SHALL emit the full chain instead of the direct `caller pod → backend service` edge.

#### Scenario: Identity recovery rule

- **WHEN** the engine recovers the ingress identity
- **THEN** it uses the same as-of identity-dedup rule as the "Ingress LB Service fallback" requirement: per-IP distinct `(namespace, name)` sets; a same-IP collision or disagreeing singletons give no identity; any IP with zero rows gives no identity; all singletons agreeing give that identity

#### Scenario: Recovery never costs the hit

- **WHEN** the identity is ambiguous or absent
- **THEN** recovery reads the store no further and never demotes the hit: the destination simply carries no ingress identity and the reader emits the pre-existing direct shape

#### Scenario: Shape of the chain

- **WHEN** the chain is emitted
- **THEN** it consists of a `service` node for the ingress LB Service in the selected ingress cluster, with `service-selects-pod` edges to **the selected cluster's own backing pods only** (locked-cluster fan-out, NOT the family-wide union, because an LB IP is a per-cluster address and a family sibling's same-named Service is not behind it)
- **AND** one `pod-calls-service` edge from the client pod to the ingress `service` node, which MAY cross clusters exactly as the direct edge it replaces
- **AND** one synthesized `pod-calls-service` edge from EACH of those locked-cluster ingress backing pods to the backend `service` node, carrying the ingress cluster as the edge's `cluster` label (the client side is a pod in that cluster)
- **AND** the backend `service` node and its family-wide `service-selects-pod` fan-out, unchanged from the pre-existing hit resolution

#### Scenario: Chain emission rules

- **WHEN** the chain is emitted
- **THEN** the direct `caller pod → backend service` edge is NOT emitted alongside it
- **AND** a synthesized ingress-pod→backend edge whose `(source pod, target service)` pair already exists as a trace-derived edge is skipped and the traced edge wins, since the two would otherwise share one deterministic edge ID
- **AND** synthesized-edge emission is deterministic (order-free over the endpoint set; idempotent under repeated series sharing one route key), and **No new node type, no new edge type, no new node attribute, and no new `labels` key** are introduced

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

### Requirement: Ingress chain preconditions

The chain SHALL be emitted only when ALL hold: (1) the destination carries a recovered ingress identity; (2) that identity differs from the backend destination's `(namespace, service)` in the selected cluster; (3) the selected ingress cluster holds the ingress Service in topology; (4) that Service has at least one backing pod in the selected cluster's endpoints. Each failure SHALL degrade to the pre-existing direct shape.

#### Scenario: Why the identity and backing-pod preconditions exist

- **WHEN** precondition 2 or 4 fails
- **THEN** the direct shape is kept, because a destination that IS the ingress entry point needs no chain, and an empty middle would disconnect the caller from the backend

#### Scenario: Degradation leaves no trace

- **WHEN** a chain precondition fails
- **THEN** the degrade is observable via a debug-level diagnostic only: it emits no external node, does not count toward external-fallback reasons, and leaves no partially-materialised ingress node or edge
- **AND** route resolution still NEVER fails a build

#### Scenario: A backend topology miss keeps the external path

- **WHEN** the selected cluster does not hold the **backend** Service in topology
- **THEN** the existing "selected cluster lacks the Service" external path applies and the ingress Service is never materialised

### Requirement: Locked-cluster fan-out for the LB fallback

The "Ingress LB Service fallback" resolution SHALL fan out `service-selects-pod` edges over the selected ingress cluster's own backing pods only, superseding its family-wide fan-out, and SHALL emit no synthesized `pod-calls-service` edges.

#### Scenario: Why the LB fallback is locked to one cluster

- **WHEN** the fallback resolves an ingress LB Service
- **THEN** its fan-out covers only the owning cluster's endpoints, because the fallback answers "which ingress LB Service owns this IP" and the pods behind that IP are the owning cluster's endpoints
- **AND** no synthesized pod-calls-service edge is emitted, since there is no routed backend behind the LB entry point

### Requirement: Ingress LB Service fallback for unresolved global FQDN peers

When the route-resolution engine has selected an ingress cluster, loaded its snapshot at the resolution instant, and the Gateway + VirtualService pipeline missed at gateway resolution (the "no gateway serves the host" outcome), the engine SHALL, before returning that miss, attempt one fallback: resolve the destination IP set to a **unique ingress LB Service** inside the selected cluster's already-loaded snapshot, without reading the store again.

#### Scenario: The trigger is the signature of a non-Istio ingress

- **WHEN** the pipeline's miss is "no gateway serves the host"
- **THEN** it is the signature of a non-Istio ingress such as nginx, whose Hop 3 finds no Gateway CR, and the fallback runs

#### Scenario: When the fallback does not run

- **WHEN** the pipeline produced a hit, or missed at a deeper outcome ("no listener on the derived port", "no server for host", "no route matched"), or ingress-cluster selection itself failed
- **THEN** the fallback does not run: a routed `hit` always wins; after a deeper miss an Istio Gateway DID serve the host and its diagnostic reason MUST NOT be masked by an LB-entry-point edge; and the "no candidate ingress cluster" and "ambiguous ingress cluster" outcomes return before any snapshot is loaded

#### Scenario: Candidate set as of the resolution instant

- **WHEN** candidates are collected for a destination IP
- **THEN** they are every ingress Service version in the loaded (selected-cluster) snapshot that is **live at the resolution instant** and whose ingress IPs (external or load-balancer) contain that IP, deduplicated to distinct `(namespace, name)` identities
- **AND** an identity replaced BEFORE the resolution instant does not count; only owners live at that instant do

#### Scenario: nginx ingress resolves to its LB Service

- **WHEN** route resolution is enabled and a `server="unknown"` endpoint's destination IP selects
  an ingress cluster whose snapshot holds exactly one ingress LB Service carrying that IP, with
  no Istio Gateway CR reachable from the IP (the pipeline misses at gateway resolution)
- **THEN** the reader SHALL emit a `service` node for that `(selected cluster, namespace,
  service)`
- **AND** a `pod-calls-service` edge from the client pod to it
- **AND** the `service-selects-pod` fan-out to the selected cluster's own backing pods only (the
  ingress controller pods; see "Locked-cluster fan-out for the LB fallback")
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

### Requirement: Ingress LB Service uniqueness rule

The fallback SHALL compute the identity set `S_ip` independently per destination IP, order-free over IPs: any `|S_ip| > 1` SHALL degrade as **ambiguous ingress Service**; otherwise any `|S_ip| == 0` SHALL yield nothing; otherwise all (singleton) candidates MUST be the same identity, which becomes the fallback destination, and disagreeing identities SHALL degrade as **ambiguous ingress Service**. No lexicographic or recency tie-break SHALL be applied.

#### Scenario: An empty set returns the pipeline miss unchanged

- **WHEN** the fallback yields nothing
- **THEN** the engine returns the Istio pipeline's own miss, with outcome and diagnostic reason byte-for-byte the ones the pipeline would have returned without the fallback

#### Scenario: Ambiguity degrades rather than guesses

- **WHEN** the outcome is **ambiguous ingress Service**
- **THEN** the endpoint falls back to the external node with its own diagnostic reason, distinct from every existing engine reason

### Requirement: Ingress LB Service fallback resolution

A fallback destination SHALL yield `(cluster, namespace, service)` in the engine-selected ingress cluster, reported under a distinct engine outcome separate from `hit`, and the reader SHALL resolve it through the SAME same-cluster Service-node resolution as a routed hit, ignoring the request's host, path, and derived listener port. Route resolution SHALL still NEVER fail a build.

#### Scenario: Resolution of the fallback destination

- **WHEN** a fallback destination is resolved
- **THEN** it is anchored on the selected ingress cluster: AT MOST ONE service node iff that cluster holds the Service in topology (a topology miss follows the existing "selected cluster lacks the Service" external path), the same cross-cluster `service-selects-pod` fan-out over its family (narrowed by "Locked-cluster fan-out for the LB fallback"), and a `pod-calls-service` edge that MAY cross clusters
- **AND** the destination's port and subset are absent/discarded

#### Scenario: Deliberately coarse semantics

- **WHEN** the fallback answers
- **THEN** it answers only "which ingress LB Service owns this IP", and **No new node type, no new edge type, no new node attribute, and no new `labels` key** are introduced
