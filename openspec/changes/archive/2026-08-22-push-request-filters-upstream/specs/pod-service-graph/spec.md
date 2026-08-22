## MODIFIED Requirements

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

## ADDED Requirements

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
