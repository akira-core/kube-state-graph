## MODIFIED Requirements

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
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `true` (cross-cluster service resolution via cluster-family fan-out may resolve a `"://"` endpoint to a service node in a different cluster of the caller's family), its `source_type` is an array containing `"pod"` and `"external"`, its `target_type` is `"service"` (or `["service"]`), and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (omitted when the client side is non-pod)

#### Scenario: service-selects-pod catalogue entry

- **WHEN** a client inspects the catalogue entry for `service-selects-pod`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false`, its `source_type` is `["service"]` (or `"service"`), and its `target_type` is `["pod"]` (or `"pod"`)

### Requirement: Cross-cluster edge representation

When the freshly built graph contains a `pod-calls-pod` or `pod-calls-service` edge whose source-node cluster differs from its target-node cluster, the API SHALL emit it as a single edge carrying `labels.cluster` (the trace source / client-side cluster, present iff the client side resolved to a pod) and SHALL include both endpoint nodes in the response `elements.nodes` whenever the projection scope includes either endpoint's cluster. Consumers detect cross-cluster status by comparing the `labels.cluster` of the edge's resolved source and target nodes — not from edge labels. These rules apply identically to `pod-calls-pod` edges (server-side pod resolved via the global pod-UID index) and `pod-calls-service` edges (target service node resolved via cluster-family fan-out in connection-string resolution).

#### Scenario: Cross-cluster edge with both clusters in scope

- **WHEN** a client requests `?cluster=cluster-alpha&cluster=cluster-beta` for a window containing a cross-cluster edge whose client pod is in `cluster-alpha` and server pod is in `cluster-beta`
- **THEN** the response contains both endpoint pod nodes and one edge with `labels.cluster: "cluster-alpha"`, where the source node's `labels.cluster` is `"cluster-alpha"` and the target node's `labels.cluster` is `"cluster-beta"`

#### Scenario: Cross-cluster edge with one cluster in scope

- **WHEN** a client requests `?cluster=cluster-alpha` and a cross-cluster edge exists from a pod in `cluster-alpha` to a pod in `cluster-beta`
- **THEN** the response contains the `cluster-alpha` endpoint, the `cluster-beta` endpoint (so the edge resolves), and the edge with `labels.cluster: "cluster-alpha"`; the cross-cluster status is detected by comparing the two endpoint nodes' `labels.cluster` values

#### Scenario: Cross-cluster pod-calls-service edge from cluster-family fan-out

- **WHEN** a pod in cluster `prod-1` emits a `"://"` connection-string endpoint whose `(service, namespace)` is held ONLY by cluster `prod-2` within the `prod-#` family (absent from `prod-1` and every other family member), so cluster-family fan-out resolves it to exactly the `prod-2` service node, and a client requests a projection scope that includes `prod-1` or `prod-2` (or both)
- **THEN** the response contains exactly one `pod-calls-service` edge from the `prod-1` pod node to the `prod-2/<namespace>/<service>` service node, both endpoint nodes are present in `elements.nodes`, the edge carries `labels.cluster: "prod-1"` (the client side is a pod), and cross-cluster status is derived by comparing the source node's `labels.cluster` (`"prod-1"`) with the target node's `labels.cluster` (`"prod-2"`) — not from any edge label

### Requirement: Filter parameters

`GET /v1/graph` SHALL accept the optional, repeatable filter parameters `cluster`, `namespace`, `edge_type`, `name`. Filters SHALL be applied at response time as a projection over the freshly built graph. Empty filter SHALL return the full multi-cluster graph for the time window. Multiple values for the same parameter SHALL be OR-combined; different parameters SHALL be AND-combined. An unknown filter value SHALL NOT cause an error.

The `name` parameter SHALL match `n.Name()` by exact string equality across **every** node type (`PodNode`, `K8sNode`, `PVCNode`, `ServiceNode`, `ExternalNode`) — a single `?name=` value matches a pod, a K8s node, a PVC, a service, or an external node with the same name. Names are not globally unique (pods and K8s nodes can share a name; PVCs and services can repeat across namespaces); all matches SHALL be returned.

**Edge retention rule (unified across all filters).** An edge SHALL be retained when at least one resolved endpoint is in scope after node filtering. When exactly one endpoint is in scope, the missing endpoint SHALL be re-added from the freshly built graph's node index provided it passes the non-cluster filters (namespace check; types without a namespace label pass through). This single rule is edge-type-agnostic and covers (a) anchoring on a named node and visualising its incident edges with their partner endpoints, and (b) cross-cluster `pod-calls-pod` and `pod-calls-service` edges where only `cluster` narrows scope and the partner endpoint — a pod, or a service node resolved via cluster-family fan-out — lives outside the in-scope cluster set.

#### Scenario: Cluster filter narrows result

- **WHEN** the freshly built graph contains pods in `cluster-alpha` and `cluster-beta` and a client sends `?cluster=cluster-alpha`
- **THEN** the response contains pod nodes only for `cluster-alpha`, plus any cross-cluster edge endpoints (pod or service nodes) in `cluster-beta` that participate in an edge to `cluster-alpha`

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
