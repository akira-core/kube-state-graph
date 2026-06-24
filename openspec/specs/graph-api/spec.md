# graph-api Specification

## Purpose
TBD - created by archiving change add-k8s-pod-graph-api. Update Purpose after archive.
## Requirements
### Requirement: Versioned route prefix

The HTTP API SHALL expose every endpoint under the `/v1/` route prefix and SHALL include `apiVersion: "v1"` as a top-level field in every JSON response body.

#### Scenario: Body carries apiVersion

- **WHEN** a client sends `GET /v1/clusters`
- **THEN** the server returns 200 with a JSON body whose top-level object contains `"apiVersion": "v1"`

#### Scenario: Unversioned route is not served

- **WHEN** a client sends `GET /graph?start=...&end=...`
- **THEN** the server returns 404 Not Found

### Requirement: Time-ranged graph endpoint

The server SHALL expose `GET /v1/graph` that returns a multi-cluster pod / node / PVC graph for a caller-specified `[start, end]` time range. Both `start` and `end` SHALL be required query parameters in either RFC 3339 form or Unix-seconds integer form.

#### Scenario: Successful graph request with absolute timestamps

- **WHEN** a client sends `GET /v1/graph?start=2026-05-01T12:00:00Z&end=2026-05-01T12:05:00Z`
- **THEN** the server returns 200 with a Cytoscape.js JSON body containing exactly `apiVersion`, `clusters`, and `elements` (with `elements.nodes` and `elements.edges`)

#### Scenario: Missing start parameter

- **WHEN** a client sends `GET /v1/graph?end=2026-05-01T12:05:00Z`
- **THEN** the server returns 400 Bad Request with `reason: "missing_start"`

#### Scenario: Missing end parameter

- **WHEN** a client sends `GET /v1/graph?start=2026-05-01T12:00:00Z`
- **THEN** the server returns 400 Bad Request with `reason: "missing_end"`

#### Scenario: end is not after start

- **WHEN** a client sends `GET /v1/graph?start=2026-05-01T12:05:00Z&end=2026-05-01T12:00:00Z`
- **THEN** the server returns 400 Bad Request with `reason: "invalid_range"`

### Requirement: Time-window passthrough

The server SHALL pass caller-supplied `start` and `end` through to upstream PromQL verbatim, after enforcing `end > start`. There is no server-side bucketing, alignment, grid, window cap, or future-time guard; the response body SHALL NOT echo `start`, `end`, or any derived timestamp. Operators relying on bounded query cost SHALL configure upstream VictoriaMetrics search limits (e.g. `-search.maxQueryDuration`, `-search.maxPointsPerTimeseries`).

#### Scenario: Caller timestamps drive PromQL

- **WHEN** a client sends `GET /v1/graph?start=2026-05-02T12:04:17Z&end=2026-05-02T12:19:30Z`
- **THEN** the upstream PromQL is evaluated with `<window> = end - start` and `<end> = 2026-05-02T12:19:30Z`, and the response body contains only `apiVersion`, `clusters`, and `elements`

### Requirement: Cytoscape.js response shape

`GET /v1/graph` SHALL return a JSON document in Cytoscape.js shape: `{ apiVersion, clusters, elements: { nodes, edges } }`. The body SHALL NOT contain time-varying or echo-of-input fields, so identical inputs against the same upstream state produce byte-identical bodies.

Each **node** SHALL be `{ data: { id, name, type, labels } }`:
- `id` SHALL be a cluster-scoped composite for pods / K8s nodes / PVCs / services (pods: `<cluster>/<pod-uid>`; nodes: `<cluster>/<node-name>`; PVCs: `<cluster>/<namespace>/<claim>`; services: `<cluster>/<namespace>/<service>`). For external nodes (unresolvable `"://"` connection-string endpoints or missing-UID human-label fallback), `id` SHALL be `external/<label-value>` (no cluster prefix).
- `name` SHALL be the human-readable pod / node / PVC / service name. For external nodes, `name` SHALL be the verbatim `client` or `server` label value from the source service-graph series.
- `type` SHALL be one of the strings `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`. The Cytoscape serialiser additionally synthesises `"cluster"` and `"storageclass"` group nodes for compound nesting (see "Cytoscape compound node grouping").
- `data` MAY carry an optional `parent` field (`omitempty`) referencing the `id` of the node's Cytoscape compound container — see "Cytoscape compound node grouping".
- `labels` SHALL be a JSON object whose values are strings only (`map[string]string`). For pod / K8s node / PVC / service nodes it SHALL include at minimum a `cluster` entry; for pods, PVCs, and services it SHALL also include a `namespace` entry; for pods it SHALL include `node` (the cluster-scoped node ID), and SHALL include `pod_ip` and `host_ip` whenever the upstream `kube_pod_info` series carried them; for K8s nodes it SHALL include `external_ip` when the upstream provided one. **For external nodes**, `labels` SHALL be an empty object `{}` (no `cluster` key).

Each **edge** SHALL be `{ data: { id, type, source, target, labels } }`:
- `id` SHALL be a UUID, RFC 4122 compliant, encoded as a lowercase canonical string.
- `type` SHALL be one of the registered edge types from `/v1/edge-types`.
- `source` and `target` SHALL each match the `id` of a node present in the same response's `elements.nodes`.
- `labels` SHALL be a JSON object whose values are strings only (`map[string]string`). The exact key set per edge type is defined by the `pod-service-graph` and `cluster-topology-source` capabilities.

Implementations SHALL NOT encode booleans or numbers as strings inside `labels`. Non-string-typed data (numeric metrics, boolean flags) is deferred to a future typed struct field on `data` and is NOT part of the v1 contract.

#### Scenario: Pod node payload

- **WHEN** the response contains a pod node
- **THEN** its `data.type` equals `"pod"`, its `data.id` matches `<cluster>/<pod-uid>`, its `data.name` equals the pod's metadata name, and `data.labels.cluster` matches the cluster prefix in the ID

#### Scenario: Pod node payload includes pod_ip and host_ip when upstream emits them

- **WHEN** the response contains a pod node whose source `kube_pod_info` series carried `pod_ip` and `host_ip`
- **THEN** `data.labels.pod_ip` equals the upstream `pod_ip` value and `data.labels.host_ip` equals the upstream `host_ip` value

#### Scenario: K8s node payload

- **WHEN** the response contains a Kubernetes-node node
- **THEN** its `data.type` equals `"node"`, its `data.id` matches `<cluster>/<node-name>`, its `data.name` equals the node's metadata name, and `data.labels.external_ip` is present whenever the upstream metric provided one

#### Scenario: PVC node payload

- **WHEN** the response contains a PVC node
- **THEN** its `data.type` equals `"pvc"`, its `data.id` matches `<cluster>/<namespace>/<claim>`, its `data.name` equals the claim name, and `data.labels.namespace` equals the PVC namespace

#### Scenario: PVC node carries no storageclass attribute

- **WHEN** the response contains a PVC node whose StorageClass was resolved from `kube_persistentvolumeclaim_info`
- **THEN** the PVC node's `data` has no `storageclass` field and its `labels` has no `storageclass` key — the StorageClass surfaces only via the node's `data.parent` and the synthetic `type="storageclass"` group node

#### Scenario: Service node payload

- **WHEN** the response contains a service node (a connection-string endpoint that resolved to an in-cluster service via `kube_service_info`)
- **THEN** its `data.type` equals `"service"`, its `data.id` matches `<cluster>/<namespace>/<service>`, its `data.name` equals the service name, `data.labels.cluster` matches the cluster prefix in the ID, `data.labels.namespace` equals the service namespace, and `data.ipaddress` equals `[cluster_ip]` whenever the upstream `kube_service_info` `cluster_ip` value is not `"None"`

#### Scenario: External node payload (unresolvable connection-string endpoint)

- **WHEN** the response contains an external node produced by an unresolvable `"://"` connection-string endpoint (a `client` or `server` label containing `"://"` whose host did not resolve to an in-cluster service)
- **THEN** its `data.type` equals `"external"`, its `data.id` equals `external/<value>`, its `data.name` equals `<value>` (the verbatim service-graph `client` or `server` label), and `data.labels` equals `{}`

#### Scenario: External node payload (missing-UID fallback)

- **WHEN** the response contains an external node produced by the missing-UID human-label fallback (a service-graph series whose `client_k8s_pod_uid` or `server_k8s_pod_uid` was empty but the corresponding `client`/`server` label was populated and contained no `"://"`)
- **THEN** its `data.type` equals `"external"`, its `data.id` equals `external/<value>`, its `data.name` equals `<value>`, and `data.labels` equals `{}`

#### Scenario: Edge payload references existing nodes

- **WHEN** the response contains any edge
- **THEN** both `data.source` and `data.target` SHALL match the `data.id` of a node present in the same response's `elements.nodes`

#### Scenario: Edge id is a UUID

- **WHEN** the response contains any edge
- **THEN** `data.id` matches the regex `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

#### Scenario: Edge id is stable across rebuilds

- **WHEN** the same logical edge (same `type`, `source`, `target`) is produced by two consecutive builds for the same time bucket
- **THEN** `data.id` is byte-identical between the two builds

### Requirement: Filter parameters

`GET /v1/graph` SHALL accept the optional, repeatable filter parameters `cluster`, `namespace`, `edge_type`, `name`. Filters SHALL be applied at response time as a projection over the freshly built graph. Empty filter SHALL return the full multi-cluster graph for the time window. Multiple values for the same parameter SHALL be OR-combined; different parameters SHALL be AND-combined. An unknown filter value SHALL NOT cause an error.

The `name` parameter SHALL match `n.Name()` by exact string equality across **every** node type (`PodNode`, `K8sNode`, `PVCNode`, `ServiceNode`, `ExternalNode`) — a single `?name=` value matches a pod, a K8s node, a PVC, a service, or an external node with the same name. Names are not globally unique (pods and K8s nodes can share a name; PVCs and services can repeat across namespaces); all matches SHALL be returned.

**Edge retention rule (unified across all filters).** An edge SHALL be retained when at least one resolved endpoint is in scope after node filtering. When exactly one endpoint is in scope, the missing endpoint SHALL be re-added from the freshly built graph's node index provided it passes the non-cluster filters (namespace check; types without a namespace label pass through). This single rule is edge-type-agnostic and covers (a) anchoring on a named node and visualising its incident edges with their partner endpoints, and (b) cross-cluster edges where only `cluster` narrows scope and the partner endpoint lives outside the in-scope cluster set — a `pod-calls-pod` edge whose server pod was resolved via the global pod-UID index, or a `service-selects-pod` edge from a local service node to a backing pod that runs in a family-sibling cluster (`pod-calls-service` edges are always intra-cluster and never need cross-cluster re-hydration).

#### Scenario: Cluster filter narrows result

- **WHEN** the freshly built graph contains pods in `cluster-alpha` and `cluster-beta` and a client sends `?cluster=cluster-alpha`
- **THEN** the response contains pod nodes only for `cluster-alpha`, plus any cross-cluster edge endpoints (pod nodes reached by a `pod-calls-pod` or `service-selects-pod` edge) in `cluster-beta` that participate in an edge to `cluster-alpha`

#### Scenario: Namespace filter combined with cluster

- **WHEN** a client sends `?cluster=cluster-alpha&namespace=ns-x&namespace=ns-y`
- **THEN** the response contains pods whose cluster is `cluster-alpha` AND whose namespace is `ns-x` OR `ns-y`

#### Scenario: Edge-type filter with no matching edges

- **WHEN** a client sends `?edge_type=pod-calls-pod` and the time window contains no service-graph data
- **THEN** the response is 200 with `elements.edges: []` and no error

#### Scenario: Unknown cluster name

- **WHEN** a client sends `?cluster=does-not-exist`
- **THEN** the response is 200 with empty `elements.nodes` and `elements.edges`

#### Scenario: Name filter matches a pod

- **WHEN** the freshly built graph contains pods named `frontend` and `backend` in `cluster-alpha` and a client sends `?name=frontend`
- **THEN** the response contains the `frontend` pod node and any K8s-node, PVC, or external-endpoint nodes that are edge endpoints of `frontend`, but NOT the `backend` pod node

#### Scenario: Name filter matches a K8s node

- **WHEN** the freshly built graph contains a K8s node named `worker-1` in `cluster-alpha` and a client sends `?name=worker-1`
- **THEN** the response contains the `worker-1` K8s-node node; because K8s nodes carry no edges, no pod is pulled in by this match (the pod→node relationship is compound nesting via `labels.node`, not an edge)

#### Scenario: Name filter matches a PVC

- **WHEN** the freshly built graph contains a PVC named `checkout-data` in `cluster-alpha/shop` and a client sends `?name=checkout-data`
- **THEN** the response contains the `checkout-data` PVC node and any pod nodes that mount it via `pod-mounts-pvc`

#### Scenario: Name shared across types returns every match

- **WHEN** a pod and a K8s node both happen to be named `worker-1` and a client sends `?name=worker-1`
- **THEN** the response contains both the matching pod node AND the matching K8s-node node

#### Scenario: Name shared across clusters returns every match

- **WHEN** a pod named `api` exists in both `cluster-alpha` and `cluster-beta` and a client sends `?name=api`
- **THEN** the response contains both `cluster-alpha`'s `api` pod node and `cluster-beta`'s `api` pod node

#### Scenario: Name filter combined with cluster

- **WHEN** a pod named `api` exists in both `cluster-alpha` and `cluster-beta` and a client sends `?name=api&cluster=cluster-alpha`
- **THEN** the response contains only `cluster-alpha`'s `api` pod node

#### Scenario: Name filter retains incident edges with re-hydrated partner

- **WHEN** a `pod-calls-pod` edge crosses from `cluster-alpha/<uid-A>` (pod name `frontend`) to `cluster-beta/<uid-B>` (pod name `backend`) and a client sends `?name=frontend`
- **THEN** the response contains `cluster-alpha/<uid-A>` (the named match), `cluster-beta/<uid-B>` (re-added as the missing edge endpoint), and the cross-cluster edge

#### Scenario: Unknown name returns empty result

- **WHEN** a client sends `?name=does-not-exist`
- **THEN** the response is 200 with empty `elements.nodes` and `elements.edges`

### Requirement: Partial-graph traversal

`GET /v1/graph` SHALL accept `?root=<id>&depth=<n>&direction=in|out|both` for partial-graph traversal. `depth` SHALL default to 2 and SHALL NOT exceed 6. Traversal SHALL run a BFS on the freshly built graph's adjacency map, then any other filter parameters SHALL apply to the traversal result.

#### Scenario: Outgoing traversal at depth 1

- **WHEN** a client sends `?root=cluster-alpha/<pod-uid>&depth=1&direction=out`
- **THEN** the response contains the root node and every node reachable in one outgoing edge from it

#### Scenario: depth above maximum

- **WHEN** a client supplies `depth=10`
- **THEN** the server returns 400 Bad Request with `reason: "depth_too_large"`

#### Scenario: Unknown root id

- **WHEN** a client supplies a `root` value that does not match any node in the graph
- **THEN** the response is 200 with empty `elements.nodes` and `elements.edges`

### Requirement: Cluster discovery endpoint

The server SHALL expose `GET /v1/clusters` that returns the list of clusters with data in centralised VictoriaMetrics over a fixed 1-hour lookback. The response SHALL be derived live from a single `group by (cluster) (last_over_time(kube_node_info[1h]))` query on every request — there is no in-process discovery cache in v1.

#### Scenario: Live discovery

- **WHEN** centralised VictoriaMetrics holds `kube_node_info` series with `cluster="cluster-alpha"` and `cluster="cluster-beta"` in the last hour
- **THEN** `GET /v1/clusters` returns 200 with a `clusters` array containing both names

### Requirement: Edge-type discovery endpoint

The server SHALL expose `GET /v1/edge-types` that returns the static catalogue of edge types this server can produce. The response SHALL list at least `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, and `service-selects-pod`. Each catalogue entry SHALL describe `source_type` (one of `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, **or a JSON array of such strings** when more than one is permitted), `target_type` (same form as `source_type`), `directed`, `may_cross_cluster`, and a `labels` array enumerating the keys this edge type can emit on edge `labels`. The endpoint SHALL NOT issue any upstream calls and SHALL NOT depend on time-range or cluster parameters. The response SHALL include a long `Cache-Control: public, max-age=3600` header.

#### Scenario: Static catalogue

- **WHEN** a client sends `GET /v1/edge-types`
- **THEN** the response body contains an `edge_types` array including objects whose `type` values include `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, and `service-selects-pod`

#### Scenario: pod-calls-pod marked may_cross_cluster

- **WHEN** a client inspects the catalogue entry for `pod-calls-pod`
- **THEN** its `may_cross_cluster` field is `true`, its `source_type` and `target_type` are arrays containing `"pod"` and `"external"`, and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (representing the trace source cluster; cross-cluster status is detected by comparing the source/target nodes' `labels.cluster` rather than from edge labels)

#### Scenario: pod-calls-service catalogue entry

- **WHEN** a client inspects the catalogue entry for `pod-calls-service`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (a `"://"` connection string resolves to a service node in the caller's OWN cluster, so a `pod-calls-service` edge always connects a source to a service node in the same cluster and is never cross-cluster), its `source_type` is an array containing `"pod"` and `"external"`, its `target_type` is `"service"` (or `["service"]`), and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (omitted when the client side is non-pod)

#### Scenario: service-selects-pod catalogue entry

- **WHEN** a client inspects the catalogue entry for `service-selects-pod`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `true` (a local service node fans out to backing pods across same-family clusters holding the same-named Service, so the edge may connect a service to a pod in a different cluster of the caller's family), its `source_type` is `["service"]` (or `"service"`), and its `target_type` is `["pod"]` (or `"pod"`)

### Requirement: Cross-cluster edge representation

When the freshly built graph contains a `pod-calls-pod` or `service-selects-pod` edge whose source-node cluster differs from its target-node cluster, the API SHALL emit it as a single edge and SHALL include both endpoint nodes in the response `elements.nodes` whenever the projection scope includes either endpoint's cluster. Consumers detect cross-cluster status by comparing the `labels.cluster` of the edge's resolved source and target nodes — not from edge labels. A `pod-calls-pod` edge carries `labels.cluster` (the trace source / client-side cluster, present iff the client side resolved to a pod); a `service-selects-pod` edge carries no `cluster` key (its source is a service node, which is cluster-scoped via its own `labels.cluster`). A `pod-calls-service` edge is ALWAYS intra-cluster (the addressed service resolves to a node in the caller's own cluster), so it never participates in cross-cluster re-hydration. These rules apply to `pod-calls-pod` edges (server-side pod recovered via the global pod-UID index) and `service-selects-pod` edges (a local service node selecting a backing pod that runs in a family-sibling cluster, per connection-string resolution's cross-cluster endpoint union).

#### Scenario: Cross-cluster edge with both clusters in scope

- **WHEN** a client requests `?cluster=cluster-alpha&cluster=cluster-beta` for a window containing a cross-cluster `pod-calls-pod` edge whose client pod is in `cluster-alpha` and server pod is in `cluster-beta`
- **THEN** the response contains both endpoint pod nodes and one edge with `labels.cluster: "cluster-alpha"`, where the source node's `labels.cluster` is `"cluster-alpha"` and the target node's `labels.cluster` is `"cluster-beta"`

#### Scenario: Cross-cluster edge with one cluster in scope

- **WHEN** a client requests `?cluster=cluster-alpha` and a cross-cluster `pod-calls-pod` edge exists from a pod in `cluster-alpha` to a pod in `cluster-beta`
- **THEN** the response contains the `cluster-alpha` endpoint, the `cluster-beta` endpoint (so the edge resolves), and the edge with `labels.cluster: "cluster-alpha"`; the cross-cluster status is detected by comparing the two endpoint nodes' `labels.cluster` values

#### Scenario: Cross-cluster service-selects-pod edge from the local service node's endpoint union

- **WHEN** clusters `prod-1` and `prod-2` (family `prod-0`) both hold a `payments` service in namespace `payments-ns`, a pod in `prod-1` emits a `"://"` connection string addressing it (so resolution materialises ONE `prod-1/payments-ns/payments` service node whose `service-selects-pod` fan-out unions both clusters' backing pods), and a client requests a projection scope that includes `prod-1` or `prod-2` (or both)
- **THEN** the response contains the single `pod-calls-service` edge from the `prod-1` pod node to the `prod-1/payments-ns/payments` service node (intra-cluster), plus the `service-selects-pod` edges from that service node to each backing pod; a `service-selects-pod` edge whose target pod runs in `prod-2` is cross-cluster, with both endpoint nodes present in `elements.nodes`, and its cross-cluster status derived by comparing the source service node's `labels.cluster` (`"prod-1"`) with the target pod node's `labels.cluster` (`"prod-2"`) — not from any edge label

### Requirement: Deterministic response body

For identical input — same `(window, filters, traversal, upstream-data)` — the server SHALL produce a byte-identical response body across rebuilds. The server SHALL NOT emit any HTTP cache validator (no `ETag`, no `Last-Modified`): cacheability is intentionally a future-iteration concern and v1 has no in-process result cache.

The serialiser SHALL maintain determinism by sorting `view.Nodes` and `view.Edges`, sorting `Graph.ClusterNames()`, sorting `IPAddress` slices at construction, and keeping the response body shape fixed at `{apiVersion, clusters, elements}` for graph routes (no time-of-build or echo-of-input fields).

`GET /v1/edge-types`, `GET /openapi.yaml`, `GET /openapi.json`, and `GET /docs` SHALL carry an explicit `Cache-Control` header. `GET /v1/graph` and `GET /v1/clusters` SHALL NOT emit a `Cache-Control` header.

#### Scenario: Body byte-identical across repeated requests

- **WHEN** a client sends two consecutive `GET /v1/graph` requests with identical query parameters and the upstream data has not changed between them
- **THEN** both response bodies are byte-identical, even though each request triggered an independent upstream fan-out

### Requirement: Node `ipaddress` attribute

Every `data` object for a node in the Cytoscape response SHALL expose a top-level `ipaddress` field of type `string[]` with `omitempty` semantics:

- `type="pod"` nodes SHALL carry the pod's IP from `kube_pod_info.pod_ip` (single-element slice) when the source metric surfaces it, and omit the field otherwise.
- `type="node"` nodes SHALL carry the K8s node's `ExternalIP` from `kube_node_status_addresses` (single-element slice) when present, falling back to the node's `InternalIP` (single-element slice) when no ExternalIP row exists, and omit the field only when neither address type is present. An ExternalIP SHALL always win over an InternalIP regardless of upstream sample order.
- `type="service"` nodes SHALL carry the service's `cluster_ip` from `kube_service_info` (single-element slice) when `cluster_ip` is not `"None"`, and omit the field for headless services (`cluster_ip="None"`) or when the metric does not surface it.
- `type="pvc"` and `type="external"` nodes SHALL NOT emit the `ipaddress` field.

The legacy `labels.pod_ip`, `labels.host_ip`, and `labels.external_ip` keys SHALL NOT appear on any node entry — they are replaced by the typed `ipaddress` attribute and the node entry respectively. A `labels.internal_ip` key SHALL NOT appear either — the InternalIP fallback surfaces only via `ipaddress`.

#### Scenario: Pod entry carries pod IP on ipaddress

- **WHEN** `kube_pod_info` exposes `pod_ip="10.244.0.10"` for a pod
- **THEN** the corresponding `type="pod"` node carries `data.ipaddress: ["10.244.0.10"]` and neither `data.labels.pod_ip` nor `data.labels.host_ip` is present

#### Scenario: Node entry carries ExternalIP on ipaddress

- **WHEN** `kube_node_status_addresses{type="ExternalIP",address="203.0.113.10"}` is present for a K8s node
- **THEN** the corresponding `type="node"` entry carries `data.ipaddress: ["203.0.113.10"]` and `data.labels.external_ip` is not present

#### Scenario: Node entry falls back to InternalIP on ipaddress

- **WHEN** a K8s node has no `kube_node_status_addresses{type="ExternalIP"}` row but `kube_node_status_addresses{type="InternalIP",address="10.0.0.7"}` is present
- **THEN** the corresponding `type="node"` entry carries `data.ipaddress: ["10.0.0.7"]` and neither `data.labels.internal_ip` nor `data.labels.external_ip` is present

#### Scenario: ExternalIP preferred over InternalIP

- **WHEN** a K8s node has both an `ExternalIP` row (`address="203.0.113.10"`) and an `InternalIP` row (`address="10.0.0.7"`) in `kube_node_status_addresses`
- **THEN** the corresponding `type="node"` entry carries `data.ipaddress: ["203.0.113.10"]`

#### Scenario: Service entry carries cluster IP on ipaddress

- **WHEN** `kube_service_info` exposes `cluster_ip="10.96.0.42"` for a service that a connection-string endpoint resolved to
- **THEN** the corresponding `type="service"` node carries `data.ipaddress: ["10.96.0.42"]`

#### Scenario: Headless service omits ipaddress

- **WHEN** `kube_service_info` exposes `cluster_ip="None"` for a service that a connection-string endpoint resolved to
- **THEN** the corresponding `type="service"` node's `data` object does not include an `ipaddress` field

#### Scenario: ipaddress omitted when source metric does not surface it

- **WHEN** a pod's `kube_pod_info` series omits `pod_ip`, or a K8s node has neither an `ExternalIP` nor an `InternalIP` row in `kube_node_status_addresses`
- **THEN** the corresponding node's `data` object does not include an `ipaddress` field

#### Scenario: PVC and external nodes never carry ipaddress

- **WHEN** the response contains nodes of `type="pvc"` or `type="external"`
- **THEN** those node `data` objects do not include an `ipaddress` field

### Requirement: Node `ready_status` attribute

Every `data` object for a `type="node"` node in the Cytoscape response SHALL expose a top-level `ready_status` field of type `string` with `omitempty` semantics, carrying the node's Kubernetes Ready-condition status derived from `kube_node_status_condition{condition="Ready"}` (see cluster-topology-source Requirement: K8s node Ready-status attribute):

- The value SHALL be exactly one of `"Ready"`, `"NotReady"`, or `"Unknown"`.
- The field SHALL be omitted entirely when the source metric is absent, when the node has no `condition="Ready"` series, or when no status row is active — a node with no Ready-condition data carries no `ready_status` key, NOT `ready_status: ""` and NOT `ready_status: "Unknown"`.
- The literal `"Unknown"` SHALL appear only for the genuine Kubernetes state where the Ready condition's `status` label is `unknown` (kubelet not reporting); it is never a stand-in for missing data.
- `type="pod"`, `type="service"`, `type="pvc"`, and `type="external"` nodes SHALL NOT emit the `ready_status` field.

The Ready status SHALL NOT appear inside `labels` (which remain a strict `map[string]string` of typological metadata) — it is a typed attribute, the same precedent as `ipaddress` and `owner`.

#### Scenario: Ready node carries ready_status

- **WHEN** a K8s node's active `kube_node_status_condition{condition="Ready"}` series carries `status="true"`
- **THEN** the corresponding `type="node"` entry carries `data.ready_status: "Ready"` and no `ready_status` key in `data.labels`

#### Scenario: NotReady node carries ready_status

- **WHEN** a K8s node's active Ready-condition series carries `status="false"`
- **THEN** the corresponding `type="node"` entry carries `data.ready_status: "NotReady"`

#### Scenario: Unknown is distinct from omitted

- **WHEN** node A's active Ready-condition series carries `status="unknown"` and node B has no `kube_node_status_condition` series at all
- **THEN** node A's entry carries `data.ready_status: "Unknown"` while node B's `data` object does not include a `ready_status` field

#### Scenario: Non-node types never carry ready_status

- **WHEN** the response contains nodes of `type="pod"`, `type="service"`, `type="pvc"`, or `type="external"`
- **THEN** those node `data` objects do not include a `ready_status` field

### Requirement: API-key authentication on `/v1/*` and `/debug/*`

When the server is started with at least one API key configured (via `--api-keys-file` or `--api-keys`), every request to `/v1/*` and `/debug/*` SHALL carry an `X-API-Key: <key>` header. Requests without the header SHALL receive `401 Unauthorized` with reason `unauthorized` and a JSON message indicating the missing header. Requests with a header value that is not present in the configured key set SHALL receive `401 Unauthorized` with reason `unauthorized`.

When no keys are configured (both flags empty), the middleware SHALL be a no-op: every route SHALL behave as if auth were not configured. The server SHALL log a warning at boot identifying that auth is disabled.

The following routes SHALL be exempt from authentication regardless of configuration: `/livez`, `/readyz`, `/metrics`, `/openapi.yaml`, `/openapi.json`, and `/docs`.

Key comparison SHALL be constant-time and SHALL iterate the full configured key set on every request so neither match latency nor early exit reveals the matching position.

The server SHALL increment `kube_state_graph_auth_rejected_total{reason="missing"}` on requests without the header and `kube_state_graph_auth_rejected_total{reason="invalid"}` on requests whose header value is unknown.

When `--api-keys-file` is set and `--api-keys-reload-interval` is positive, the server SHALL re-read the file on the configured cadence and atomically swap the active key set. A key removed from the file SHALL be rejected on subsequent requests; a key added SHALL be accepted.

#### Scenario: Missing header is rejected

- **WHEN** the server is started with `--api-keys=k1` and a client sends `GET /v1/graph?start=...&end=...` with no `X-API-Key`
- **THEN** the response is `401 Unauthorized` with body `{"error":{"reason":"unauthorized", ...}}`

#### Scenario: Wrong key is rejected

- **WHEN** the server is started with `--api-keys=k1` and a client sends `X-API-Key: wrong`
- **THEN** the response is `401 Unauthorized` with reason `unauthorized`

#### Scenario: Valid key is accepted

- **WHEN** the server is started with `--api-keys=k1,k2` and a client sends `X-API-Key: k2` to `/v1/edge-types`
- **THEN** the response is `200 OK` with the edge-type catalogue

#### Scenario: Open paths bypass auth even when keys are configured

- **WHEN** the server is started with keys configured and a client sends `GET /livez` / `GET /metrics` / `GET /docs` with no header
- **THEN** the response is `200 OK` (open routes ignore auth)

#### Scenario: Auth disabled when no keys configured

- **WHEN** the server is started with neither `--api-keys-file` nor `--api-keys` set
- **THEN** every route, including `/v1/graph`, accepts requests with no `X-API-Key` header, and the server boot log emits a warning identifying disabled auth

#### Scenario: Hot reload picks up rotated keys

- **WHEN** the operator updates `--api-keys-file` content (e.g., a Kubernetes `Secret` rotation propagates) and `--api-keys-reload-interval` elapses
- **THEN** subsequent requests presenting a key newly added to the file are accepted, and subsequent requests presenting a key removed from the file are rejected, all without process restart

### Requirement: Health endpoints

The server SHALL expose `GET /livez` that returns 200 while the process is running, and `GET /readyz` that returns 200 only when a 1-second `up{}` probe against the centralised VictoriaMetrics succeeds. `GET /readyz` SHALL return 503 otherwise.

#### Scenario: livez always healthy while running

- **WHEN** a client sends `GET /livez`
- **THEN** the response is 200 with body `"ok"` regardless of upstream state

#### Scenario: readyz fails when upstream unreachable

- **WHEN** the configured VictoriaMetrics URL refuses connections and a client sends `GET /readyz`
- **THEN** the response is 503 with a JSON body containing a `reason` field

### Requirement: Self-metrics endpoint

The server SHALL expose `GET /metrics` in Prometheus exposition format including at least: `kube_state_graph_build_duration_seconds`, `kube_state_graph_project_duration_seconds`, `kube_state_graph_serialise_duration_seconds`, `kube_state_graph_build_rejected_total`, `kube_state_graph_graph_node_count`, `kube_state_graph_graph_edge_count`, `kube_state_graph_clusters_observed`, `kube_state_graph_upstream_query_duration_seconds`, `kube_state_graph_upstream_query_failures_total`, `kube_state_graph_http_requests_total`, and `kube_state_graph_auth_rejected_total`.

#### Scenario: Metrics exposition

- **WHEN** a client sends `GET /metrics`
- **THEN** the response is 200 in `text/plain; version=0.0.4` exposition format and includes all metric names listed above

#### Scenario: cluster label on observational gauges

- **WHEN** a build has produced a multi-cluster graph
- **THEN** `kube_state_graph_graph_node_count` series include a `cluster` label and `kube_state_graph_graph_edge_count` series include a `cross_cluster` label

### Requirement: Per-build timeout (graph endpoints)

For `GET /v1/graph`, the server SHALL apply a configurable per-build `context.WithTimeout` derived from `--build-timeout` (default 15 seconds). On `context.DeadlineExceeded`, the build SHALL be aborted, the `kube_state_graph_build_rejected_total{reason="timeout"}` counter SHALL be incremented, and the request SHALL receive `504 Gateway Timeout` with `reason: "timeout"` (RFC 9110 §15.6.5: gateway did not receive a timely response from an upstream server it needed to access in order to complete the request).

#### Scenario: Upstream stalls beyond build timeout

- **WHEN** centralised VictoriaMetrics fails to respond to a `/v1/graph` build within `--build-timeout`
- **THEN** the request returns 504 with `reason: "timeout"`

### Requirement: Per-request timeout (non-graph endpoints)

For non-graph endpoints that perform upstream calls (`GET /v1/clusters` discovery query, `GET /readyz` `up{}` probe), the server SHALL apply a `context.WithTimeout` derived from `--api-timeout` (default 5 seconds) to the upstream call. On `context.DeadlineExceeded`, the request SHALL receive `504 Gateway Timeout` with `reason: "timeout"`. Endpoints that do not perform upstream calls (`GET /v1/edge-types`, `GET /livez`, `GET /metrics`, `GET /openapi.*`, `GET /docs*`) are not subject to this timeout.

#### Scenario: Cluster discovery stalls beyond api timeout

- **WHEN** centralised VictoriaMetrics fails to respond to the `/v1/clusters` discovery query within `--api-timeout`
- **THEN** the request returns 504 with `reason: "timeout"`

### Requirement: Outside-retention error

When a topology query for the requested window returns zero rows but the upstream VictoriaMetrics is reachable (a parallel `up{}` probe succeeds), the server SHALL respond `400 Bad Request` with `reason: "outside_retention"`.

#### Scenario: Window beyond retention

- **WHEN** a client requests a window older than upstream `kube_pod_info` retention but `up{}` returns 1
- **THEN** the response is 400 with `reason: "outside_retention"`

### Requirement: Structured request logging

Every served HTTP request SHALL emit exactly one structured log line via `log/slog` JSON handler containing at least `method`, `path`, `status`, `duration_ms`, `request_id`, and applied `cluster` filter values.

#### Scenario: Request log line

- **WHEN** the server serves a request
- **THEN** stdout receives a JSON object with the listed fields and a top-level `level` field set to `INFO` for non-error responses

### Requirement: OpenAPI specification served by the API

The server SHALL serve the auto-generated OpenAPI 3.0 specification at two routes:

- `GET /openapi.yaml` SHALL return the YAML form with `Content-Type: application/yaml`.
- `GET /openapi.json` SHALL return the JSON form with `Content-Type: application/json`.

Both responses SHALL carry `Cache-Control: public, max-age=3600`. The spec SHALL be generated from handler annotations via `swaggo/swag` v2; the generated `docs/swagger.{json,yaml,go}` artefacts SHALL be checked into the repository.

#### Scenario: YAML spec served

- **WHEN** a client sends `GET /openapi.yaml`
- **THEN** the response is 200 with `Content-Type: application/yaml` and a body whose first non-empty line begins with `openapi:`

#### Scenario: JSON spec served

- **WHEN** a client sends `GET /openapi.json`
- **THEN** the response is 200 with `Content-Type: application/json` and a body whose top-level object contains an `"openapi"` key

### Requirement: Scalar API Reference UI served at /docs

The server SHALL serve the Scalar API Reference UI at `GET /docs`, rendering the same-origin OpenAPI spec from `/openapi.json` via `Scalar.createApiReference`. The Scalar standalone bundle is loaded from the jsDelivr CDN, pinned to an exact version and carrying a Subresource Integrity (`integrity=`) hash so a mutated CDN artifact cannot execute. The `/docs` response SHALL set `Content-Security-Policy`, `X-Frame-Options: DENY`, and `X-Content-Type-Options: nosniff` headers. There is no vendored `/docs/assets/*` route.

#### Scenario: /docs renders the Scalar UI from the pinned CDN bundle

- **WHEN** a client sends `GET /docs`
- **THEN** the response is 200, `Content-Type: text/html`, and the page loads the version-pinned `@scalar/api-reference` standalone bundle from `cdn.jsdelivr.net` with an `integrity` hash, then calls `Scalar.createApiReference` against the same-origin `/openapi.json`, and the response carries `Content-Security-Policy`, `X-Frame-Options`, and `X-Content-Type-Options` headers

### Requirement: Route ↔ spec drift guard

The repository SHALL guard against handler annotations drifting from the generated OpenAPI spec. The `make check-docs` CI job regenerates the swag output from source annotations and fails on any diff against the committed `docs/`, so an added, removed, or edited `@Router` / `@Summary` annotation that is not reflected in `docs/` fails CI. (A Go test that parses the embedded spec and diffs it against the live Gin route table was considered and descoped to avoid adding a `kin-openapi` dependency; `make check-docs` covers annotation↔source drift.)

#### Scenario: Handler annotation edited without regenerating docs

- **WHEN** a contributor adds, removes, or edits a `// @Router` / `// @Summary` annotation and does not run `make docs`
- **THEN** the `check-docs` CI job fails with a `git diff` showing the stale `docs/swagger.{json,yaml}`

### Requirement: Cytoscape compound node grouping

`GET /v1/graph` SHALL express the cluster / node / pod and cluster / storageclass / pvc hierarchies as Cytoscape compound nodes via an optional `data.parent` field, and SHALL synthesise one group node per cluster plus one group node per `(cluster, StorageClass)` pair backing an emitted PVC. This is a presentation concern of the Cytoscape serialiser; it SHALL NOT affect the core graph, projection, or traversal.

The serialiser SHALL emit, for each distinct `labels.cluster` value present on an emitted node, one synthetic group node `{ data: { id: "cluster/<cluster>", name: "<cluster>", type: "cluster", labels: {} } }` with no `parent` and no `ipaddress`. These group nodes SHALL be emitted before the other nodes, ordered by cluster name, so the body stays byte-deterministic.

The serialiser SHALL also emit, for each distinct `(cluster, StorageClass)` pair carried by an emitted `type="pvc"` node that has a non-empty resolved StorageClass, one synthetic group node `{ data: { id: "<cluster>/storageclass/<storageclass>", name: "<storageclass>", type: "storageclass", labels: {}, parent: "cluster/<cluster>" } }` with no `ipaddress`. The StorageClass group node carries no `labels` (the empty object `{}`, matching the cluster group node); its cluster identity is expressed solely by its `id` and `parent`. These StorageClass group nodes SHALL be emitted after the cluster group nodes and before the non-group nodes, ordered by `(cluster, storageclass)`, so the body stays byte-deterministic. No StorageClass group node SHALL be synthesised for a `(cluster, StorageClass)` pair that no emitted PVC references, and none SHALL be synthesised for a PVC whose resolved StorageClass is empty.

Each non-group node's `data.parent` SHALL be assigned as:

- `type="pod"` → the pod's K8s node id (its `labels.node` value) when that node is present in the same response; else `cluster/<labels.cluster>` when the pod has a non-empty cluster; else omitted.
- `type="node"`, `type="service"` → `cluster/<labels.cluster>`.
- `type="pvc"` → its StorageClass group id `<cluster>/storageclass/<storageclass>` when the PVC has a non-empty resolved StorageClass (in which case that StorageClass group node is also emitted); else `cluster/<labels.cluster>`.
- `type="storageclass"` → `cluster/<labels.cluster>`.
- `type="external"` → omitted (no cluster identity).

The `parent` field SHALL use `omitempty` semantics (absent when there is no parent). Services and PVCs SHALL NOT be compound parents of pods (a Service spans nodes and a pod may back multiple Services; those relationships remain edges). A StorageClass group node SHALL contain only PVCs — it SHALL NOT be the parent of any pod, K8s node, or service.

There is no `pod-runs-on-node` edge. The pod→node relationship SHALL be expressed solely by the `cluster > node > pod` compound nesting, derived from each pod's `labels.node`. K8s `node` nodes therefore carry no edges and act purely as compound containers. Relationship edges (`pod-mounts-pvc`, `service-selects-pod`, `pod-calls-pod`, `pod-calls-service`) SHALL be retained in the Cytoscape output.

#### Scenario: Cluster group node synthesised

- **WHEN** the graph contains any node with `labels.cluster="cluster-alpha"`
- **THEN** the Cytoscape response contains a node `{ data: { id: "cluster/cluster-alpha", name: "cluster-alpha", type: "cluster", labels: {} } }` with no `parent` field

#### Scenario: cluster > node > pod nesting

- **WHEN** a pod node carries `labels.node="cluster-alpha/worker-0"` and the K8s node `cluster-alpha/worker-0` is in the response
- **THEN** the pod's `data.parent` equals `"cluster-alpha/worker-0"` and the K8s node's `data.parent` equals `"cluster/cluster-alpha"`

#### Scenario: pod falls back to cluster parent when its node is not in scope

- **WHEN** a pod carries `labels.node="cluster-alpha/worker-0"` but that K8s node is not present in the response (e.g. filtered out)
- **THEN** the pod's `data.parent` equals `"cluster/cluster-alpha"`

#### Scenario: StorageClass group node synthesised and PVC nested (cluster > storageclass > pvc)

- **WHEN** the response contains a `type="pvc"` node in `cluster-alpha` whose resolved StorageClass is `gp3`
- **THEN** the response contains a group node `{ data: { id: "cluster-alpha/storageclass/gp3", name: "gp3", type: "storageclass", labels: {}, parent: "cluster/cluster-alpha" } }`, the PVC's `data.parent` equals `"cluster-alpha/storageclass/gp3"`, and that StorageClass group node's `data.parent` equals `"cluster/cluster-alpha"`

#### Scenario: PVC without StorageClass falls back to cluster parent

- **WHEN** the response contains a `type="pvc"` node in `cluster-alpha` whose resolved StorageClass is empty (metric absent or unmatched)
- **THEN** no `type="storageclass"` group node is synthesised on its behalf and the PVC's `data.parent` equals `"cluster/cluster-alpha"`

#### Scenario: service and PVC-without-StorageClass parented to cluster, not containing pods

- **WHEN** the response contains a `type="service"` node and a `type="pvc"` node with no resolved StorageClass in `cluster-alpha`
- **THEN** each has `data.parent="cluster/cluster-alpha"`, and neither is the `parent` of any pod

#### Scenario: external nodes have no parent

- **WHEN** the response contains a `type="external"` node
- **THEN** that node's `data` has no `parent` field, and no cluster group node is synthesised for an endpoint carrying no `cluster` label

### Requirement: Pod `application` and `containers` attributes

Every `data` object for a `type="pod"` node in the Cytoscape response SHALL be
able to expose two additional top-level attributes with `omitempty` semantics,
both **outside `labels`** (which stays a strict `map[string]string`):

- `application` — a `string`, the pod's ArgoCD Application name as resolved by the
  `cluster-topology-source` capability. Emitted only when the pod has a resolved
  Application; omitted entirely otherwise (never an empty string).
- `containers` — an array of objects `[{ name: string, image: string }]`, one per
  container, as resolved by the `cluster-topology-source` capability and ordered
  deterministically by `(name, image)`. Emitted only when the pod has at least one
  resolved container; omitted entirely otherwise (never an empty array).

These attributes SHALL appear only on `type="pod"` nodes. `type="node"`,
`type="pvc"`, `type="service"`, `type="external"`, and the synthesised
`type="cluster"` / `type="storageclass"` group nodes SHALL NOT emit `application`
or `containers`. The attributes SHALL NOT appear inside `labels`, and SHALL NOT be
encoded as numbers or booleans. Because both are `omitempty`, a pod with neither a
resolved Application nor container info produces a `data` object byte-identical to
the pre-change shape.

#### Scenario: Pod node carries application when resolved

- **WHEN** the response contains a pod node whose `kube_pod_owner` series carried an `argocd_tracking_id` resolving to Application `checkout`
- **THEN** the corresponding `type="pod"` node carries `data.application: "checkout"` and `data.labels` contains no `argocd_tracking_id` / `application` key

#### Scenario: Pod node carries containers when resolved

- **WHEN** the response contains a pod node whose `kube_pod_container_info` series listed containers `app` (`reg/app:1.2`) and `sidecar` (`reg/proxy:0.9`)
- **THEN** the corresponding `type="pod"` node carries `data.containers: [{"name":"app","image":"reg/app:1.2"},{"name":"sidecar","image":"reg/proxy:0.9"}]` ordered by `(name, image)` and `data.labels` contains no container key

#### Scenario: Pod node omits application and containers when unresolved

- **WHEN** the response contains a pod node with no resolved ArgoCD Application and no container info
- **THEN** the corresponding `type="pod"` node's `data` object includes neither an `application` field nor a `containers` field

#### Scenario: Non-pod nodes never carry application or containers

- **WHEN** the response contains nodes of `type="node"`, `type="pvc"`, `type="service"`, or `type="external"`
- **THEN** those node `data` objects include neither an `application` field nor a `containers` field

#### Scenario: Deterministic body with new attributes

- **WHEN** the same pod (same Application and container set) is produced by two consecutive builds for the same time bucket
- **THEN** the pod node's `data.application` and `data.containers` are byte-identical between the two builds, with `data.containers` ordered by `(name, image)`

