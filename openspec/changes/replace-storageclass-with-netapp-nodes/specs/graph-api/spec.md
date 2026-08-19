## MODIFIED Requirements

### Requirement: Cytoscape.js response shape

`GET /v1/graph` SHALL return a JSON document in Cytoscape.js shape: `{ apiVersion, clusters, elements: { nodes, edges } }`. The body SHALL NOT contain time-varying or echo-of-input fields, so identical inputs against the same upstream state produce byte-identical bodies. The top-level `clusters` array SHALL list **Kubernetes** cluster names only — an ONTAP cluster name (the `ontap_cluster` label of `netapp-aggr` / `netapp-node` nodes) SHALL NEVER appear in it.

Each **node** SHALL be `{ data: { id, name, type, labels } }`:
- `id` SHALL be a cluster-scoped composite for pods / K8s nodes / PVCs / services (pods: `<cluster>/<pod-uid>`; nodes: `<cluster>/<node-name>`; PVCs: `<cluster>/<namespace>/<claim>`; services: `<cluster>/<namespace>/<service>`). For NetApp aggregates, `id` SHALL be `netapp/<ontap-cluster>/aggr/<aggr>`; for NetApp nodes, `netapp/<ontap-cluster>/<node>` (neither carries a Kubernetes cluster prefix). For external nodes (unresolvable `"://"` connection-string endpoints or missing-UID human-label fallback), `id` SHALL be `external/<label-value>` (no cluster prefix).
- `name` SHALL be the human-readable pod / node / PVC / service name; for NetApp aggregates, the ONTAP aggregate name; for NetApp nodes, the ONTAP controller name. For external nodes, `name` SHALL be the verbatim `client` or `server` label value from the source service-graph series.
- `type` SHALL be one of the strings `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, `"netapp-aggr"`, `"netapp-node"`. The Cytoscape serialiser additionally synthesises `"cluster"`, `"storage-cluster"`, `"namespace"`, `"application"`, and `"controller"` group nodes for compound nesting (see "Cytoscape compound node grouping").
- `data` MAY carry an optional `parent` field (`omitempty`) referencing the `id` of the node's Cytoscape compound container — see "Cytoscape compound node grouping".
- `labels` SHALL be a JSON object whose values are strings only (`map[string]string`). For pod / K8s node / PVC / service nodes it SHALL include at minimum a `cluster` entry; for pods, PVCs, and services it SHALL also include a `namespace` entry; for pods it SHALL include `node` (the cluster-scoped node ID), and SHALL include `pod_ip` and `host_ip` whenever the upstream `kube_pod_info` series carried them; for K8s nodes it SHALL include `external_ip` when the upstream provided one. **For NetApp aggregates**, `labels` SHALL be exactly `{ontap_cluster, node}` (the owning controller); **for NetApp nodes**, exactly `{ontap_cluster}` — deliberately no `cluster` key on either. **For external nodes**, `labels` SHALL be an empty object `{}` (no `cluster` key).

Each **edge** SHALL be `{ data: { id, type, source, target, labels } }`:
- `id` SHALL be a UUID, RFC 4122 compliant, encoded as a lowercase canonical string.
- `type` SHALL be one of the registered edge types from `/v1/edge-types`.
- `source` and `target` SHALL each match the `id` of a node present in the same response's `elements.nodes`.
- `labels` SHALL be a JSON object whose values are strings only (`map[string]string`). The exact key set per edge type is defined by the `pod-service-graph`, `cluster-topology-source`, and `netapp-storage-graph` capabilities.
- `data` MAY carry an optional `metrics` object (`omitempty`) holding the edge's measurements — see "Edge `metrics` attribute".

Implementations SHALL NOT encode booleans or numbers as strings inside `labels`. Boolean flags remain deferred to a future typed field and are NOT part of the v1 contract. Numeric measurements are carried exclusively on the typed `data.metrics` object defined below — never inside `labels`.

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
- **THEN** the former prohibition this scenario named is lifted: the PVC node's `data.storageclass` equals the resolved name (see "PVC `storageclass` and `usage` attributes"), its `labels` still has no `storageclass` key, and no `type="storageclass"` node or `pvc-to-storageclass` edge exists anywhere in the response

#### Scenario: ONTAP cluster names never appear in clusters[]

- **WHEN** the response contains a `netapp-aggr` or `netapp-node` node with `labels.ontap_cluster="ontap-prod"`
- **THEN** the top-level `clusters` array does not contain `"ontap-prod"` (it lists Kubernetes cluster names only)

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

#### Scenario: Edge labels never carry numbers

- **WHEN** the response contains an edge that carries a `data.metrics` object
- **THEN** its `data.labels` still contains only string values and no `rate`, `error_rate`, `p90_server_ms`, `read_ops`, `write_ops`, `read_latency_us`, or `write_latency_us` key

### Requirement: Edge `metrics` attribute

An edge's `data` MAY carry an optional `metrics` object (`omitempty`) holding the edge's measurements for the requested window. The key SHALL be **absent entirely** — never `null`, never an empty object — on every edge that has no measurements. The object is a **union of two disjoint families**, and a single edge SHALL only ever carry fields from ONE family (the family is determined by the edge's provenance, so cross-family mixing is structurally impossible):

- **RED family** — on trace-derived call edges, presence rule defined by the `pod-service-graph` capability (in short: only trace-derived edges whose `source` and `target` both resolved to a pod or a service node, excluding the ingress chain's entry hop and any measurement derived from span-link series):
  - `rate` (number, REQUIRED **within this family**, strictly greater than zero) — requests per second over the window.
  - `error_rate` (number, OPTIONAL, absence semantics) — the failed fraction in `[0, 1]`. Absent when the upstream failure counter could not be read; `0` when it was read and reported no failures.
  - `p90_server_ms` (number, OPTIONAL, absent when unavailable) — the 90th percentile server-observed request duration in milliseconds. The quantile and observation side match Grafana's documented service-graph queries by definition; the values are not expected to equal Grafana's numerically, because Grafana aggregates by service name while this API aggregates by pod pair.
- **I/O family** — on `pvc-to-netapp-aggr` edges only, presence rule defined by the `netapp-storage-graph` capability (each field present iff its own Harvest family matched):
  - `read_ops`, `write_ops` (numbers, OPTIONAL) — read/write requests per second, verbatim from Harvest.
  - `read_latency_us`, `write_latency_us` (numbers, OPTIONAL) — average read/write latency in microseconds, verbatim from Harvest.

At the schema level every field of the union is therefore optional — a consequence the OpenAPI schema reflects by moving `rate` from required to optional. The RED invariant is preserved intact: a RED-family `metrics` object always carries a positive `rate`.

All values SHALL be JSON numbers, never strings. Each value SHALL be rounded to a fixed number of **significant digits** — not decimal places — so that the "Deterministic response body" requirement continues to hold byte-for-byte while a non-zero value can never be rendered as `0`. Consequently a value MAY appear in JSON exponent form (for example `3.86e-7`), which is legal JSON; consumers MUST NOT assume a fixed-decimal rendering, and MUST treat `0` as semantically distinct from a very small non-zero value. The presence or absence of `metrics` SHALL NOT affect the edge's `id`, `type`, `source`, `target`, or `labels`, and SHALL NOT affect node or edge ordering.

#### Scenario: Pod-to-pod edge carries RED metrics

- **WHEN** the response contains a `pod-calls-pod` edge whose `source` and `target` are both pod nodes and whose upstream series carried request, failure, and duration data
- **THEN** its `data.metrics` is an object with numeric `rate`, `error_rate`, and `p90_server_ms` fields and no I/O-family field

#### Scenario: Pod-to-service edge carries RED metrics

- **WHEN** the response contains a `pod-calls-service` edge produced by a contributing service-graph series (a connection string, a peer address matched to a Service, or a route-engine resolution to a backend Service)
- **THEN** its `data.metrics` is present and follows the same shape as a pod-to-pod edge's

#### Scenario: Storage edge carries I/O metrics only

- **WHEN** the response contains a `pvc-to-netapp-aggr` edge whose joined Harvest families all matched
- **THEN** its `data.metrics` is an object with numeric `read_ops`, `write_ops`, `read_latency_us`, and `write_latency_us` fields, and none of `rate`, `error_rate`, or `p90_server_ms`

#### Scenario: Edge without measurements omits the key

- **WHEN** the response contains a `service-selects-pod`, `pod-to-node`, or `pod-mounts-pvc` edge, or any call edge with an `external` endpoint
- **THEN** its `data` object has no `metrics` key at all (not `null`, not `{}`)

#### Scenario: Partial measurements omit only the missing fields

- **WHEN** a qualifying pod-to-pod edge has request data but the upstream duration histogram is unavailable
- **THEN** its `data.metrics` contains `rate` and `error_rate` but no `p90_server_ms` key

#### Scenario: Metrics are JSON numbers

- **WHEN** any edge carries `data.metrics`
- **THEN** every value inside it is a JSON number, not a string

#### Scenario: Very small values render in exponent form and survive a round-trip

- **WHEN** an edge's `rate` is small enough that its rendering falls below the JSON encoder's fixed-notation threshold
- **THEN** the value is emitted as a JSON number in exponent form, it is not `0`, and parsing the body and re-serialising it reproduces the identical number

#### Scenario: Metrics do not perturb determinism

- **WHEN** two builds run over identical upstream data
- **THEN** both response bodies are byte-identical, including every `data.metrics` value

### Requirement: Filter parameters

`GET /v1/graph` SHALL accept the optional, repeatable filter parameters `cluster`, `namespace`, `edge_type`, `name`. Filters SHALL be applied at response time as a projection over the freshly built graph. Empty filter SHALL return the **connectivity-connected subgraph** of the full multi-cluster graph for the time window (the default connectivity prune — see the "Default projection prunes connectivity-disconnected workload" requirement below); it is NOT the full topology inventory. Multiple values for the same parameter SHALL be OR-combined; different parameters SHALL be AND-combined. An unknown filter value SHALL NOT cause an error.

The `name` parameter SHALL match `n.Name()` by exact string equality across **every** node type (`PodNode`, `K8sNode`, `PVCNode`, `ServiceNode`, `ExternalNode`, `NetAppAggrNode`, `NetAppNode`) — a single `?name=` value matches a pod, a K8s node, a PVC, a service, an external node, a NetApp aggregate (whose `Name()` is the ONTAP aggregate name), or a NetApp node (whose `Name()` is the ONTAP controller name) with the same name. Names are not globally unique (pods and K8s nodes can share a name; PVCs and services can repeat across namespaces; ONTAP aggregate and controller names can repeat across ONTAP clusters); all matches SHALL be returned.

**Edge retention rule (unified across all filters).** An edge SHALL be retained when at least one resolved endpoint is in scope after node filtering. When exactly one endpoint is in scope, the missing endpoint SHALL be re-added from the freshly built graph's node index provided it passes the non-cluster filters (namespace check; types without a namespace label — `node`, `external`, and the NetApp types `netapp-aggr` / `netapp-node` (which carry neither a namespace nor a `cluster` label) — pass through). This single rule is edge-type-agnostic and covers (a) anchoring on a named node and visualising its incident edges with their partner endpoints — including the topology `pod-to-node` edge and the `pvc-to-netapp-aggr` edge — and (b) cross-cluster edges where only `cluster` narrows scope and the partner endpoint lives outside the in-scope cluster set.

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
- **THEN** the response contains the `frontend` pod node and any K8s-node, PVC, NetApp, or external-endpoint nodes that are edge endpoints of `frontend`, but NOT the `backend` pod node

#### Scenario: Name filter matches a K8s node

- **WHEN** the freshly built graph contains a K8s node named `worker-1` in `cluster-alpha` and a client sends `?name=worker-1`
- **THEN** the response contains the `worker-1` K8s-node node and, via the `pod-to-node` edges incident to it, the pods scheduled on it (re-added as the missing edge endpoints per the unified edge-retention rule)

#### Scenario: Name filter matches a PVC

- **WHEN** the freshly built graph contains a PVC named `checkout-data` in `cluster-alpha/shop` and a client sends `?name=checkout-data`
- **THEN** the response contains the `checkout-data` PVC node, any pod nodes that mount it via `pod-mounts-pvc`, and its NetApp aggregate via `pvc-to-netapp-aggr` when the claim joined

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

### Requirement: Default projection prunes connectivity-disconnected workload

`GET /v1/graph` SHALL, on every request that does NOT carry a `name` filter or a traversal root (`root`), return only the workload that participates in the connectivity graph. A `pod` node SHALL be retained iff it is an endpoint of at least one connectivity edge (`pod-calls-pod`, `pod-calls-service`, or `service-selects-pod`). A `pvc` node SHALL be retained iff at least one of the pods that mount it (`pod-mounts-pvc`) is itself retained by that rule; consequently a PVC with no mounting pod at all SHALL be dropped. A `node` (K8s host) and a `netapp-aggr` SHALL be retained iff referenced by a retained element (a pod scheduled on the node, a PVC joined to the aggregate via `pvc-to-netapp-aggr`), and a `netapp-node` iff referenced by a retained `netapp-aggr` (its `labels.node`) — the existing reference-driven infra-admission rule, now operating (transitively for the NetApp chain) over the connectivity-pruned pod/PVC set. `service` and `external` nodes are connectivity-born (only ever materialised as edge endpoints) and SHALL NOT be pruned by this rule. The prune SHALL be a pure, scope-independent function of the freshly built graph, applied uniformly for the no-filter, `cluster`, and `namespace` request shapes, and SHALL NOT resurrect a pruned pod/PVC through the edge-retention partner re-add. The full multi-cluster graph SHALL still be built upstream; the prune is a projection concern only.

`GET /v1/graph` SHALL suppress the prune under two on-demand escape hatches: an explicit `name` filter SHALL surface a matched element even when it is connectivity-disconnected, and a `root`-anchored traversal SHALL return its reachable set verbatim regardless of connectivity.

#### Scenario: Edgeless pod and its dependents are pruned from the default view

- **WHEN** the freshly built graph contains a pod `idle` that is on no connectivity edge (only a `pod-to-node` edge to host `worker-9` and a `pod-mounts-pvc` edge to PVC `idle-data`, where `worker-9` and `idle-data` are referenced by nothing else) and a client sends no filters
- **THEN** the response omits the `idle` pod, the `worker-9` node, the `idle-data` PVC, any NetApp aggregate serving only `idle-data`, and any NetApp node referenced only by such aggregates

#### Scenario: Connectivity-connected workload is retained with its infra

- **WHEN** a pod `web` is an endpoint of a `pod-calls-pod` edge, is scheduled on `worker-0`, and mounts PVC `web-data` whose claim joined aggregate `netapp/ontap-prod/aggr/aggr1` owned by controller `ontap-prod-01`, and a client sends no filters
- **THEN** the response contains `web`, `worker-0`, `web-data`, `netapp/ontap-prod/aggr/aggr1`, and `netapp/ontap-prod/ontap-prod-01`

#### Scenario: Name filter surfaces an otherwise-pruned edgeless pod

- **WHEN** the freshly built graph contains a connectivity-disconnected pod `idle` and a client sends `?name=idle`
- **THEN** the response contains the `idle` pod node (the prune is suppressed by the explicit name filter)

#### Scenario: Namespace filter still prunes edgeless workload

- **WHEN** a namespace `shop` contains both a connectivity-connected pod `web` and an edgeless pod `idle`, and a client sends `?namespace=shop`
- **THEN** the response contains `web` but omits `idle`

### Requirement: Edge-type discovery endpoint

The server SHALL expose `GET /v1/edge-types` that returns the static catalogue of edge types this server can produce. The response SHALL list at least `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, and `pvc-to-netapp-aggr`. Each catalogue entry SHALL describe `source_type` (one of `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, `"netapp-aggr"`, `"netapp-node"`, **or a JSON array of such strings** when more than one is permitted), `target_type` (same form as `source_type`), `directed`, `may_cross_cluster`, and a `labels` array enumerating the keys this edge type can emit on edge `labels`. The `pod-calls-pod` and `pod-calls-service` entries SHALL enumerate a `relation` label (`value_type: "string"`; emitted values `"link"` / `"transport"`, absent on ordinary edges); the `service-selects-pod` entry SHALL NOT. The endpoint SHALL NOT issue any upstream calls and SHALL NOT depend on time-range or cluster parameters. The response SHALL include a long `Cache-Control: public, max-age=3600` header.

#### Scenario: Static catalogue

- **WHEN** a client sends `GET /v1/edge-types`
- **THEN** the response body contains an `edge_types` array including objects whose `type` values include `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, and `pvc-to-netapp-aggr`, and no `pvc-to-storageclass` entry

#### Scenario: pod-calls-pod marked may_cross_cluster

- **WHEN** a client inspects the catalogue entry for `pod-calls-pod`
- **THEN** its `may_cross_cluster` field is `true`, its `source_type` and `target_type` are arrays containing `"pod"` and `"external"`, and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (representing the trace source cluster; cross-cluster status is detected by comparing the source/target nodes' `labels.cluster` rather than from edge labels) and an entry whose `name` is `relation` with `value_type: "string"` (the span-link relation marker — `"link"` for a logical producer→consumer edge, `"transport"` for a pod→broker network hop, absent otherwise)

#### Scenario: pod-calls-service catalogue entry

- **WHEN** a client inspects the catalogue entry for `pod-calls-service`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `true` (a `"://"` connection string resolves to a service node in the caller's OWN cluster, but the Istio route-resolution engine anchors on the selected ingress cluster, which may be a family sibling of the caller's), its `source_type` is an array containing `"pod"` and `"external"`, its `target_type` is `"service"` (or `["service"]`), and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (omitted when the client side is non-pod) and an entry whose `name` is `relation` with `value_type: "string"` (same semantics as on `pod-calls-pod`)

#### Scenario: service-selects-pod catalogue entry

- **WHEN** a client inspects the catalogue entry for `service-selects-pod`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `true` (a local service node fans out to backing pods across same-family clusters holding the same-named Service, so the edge may connect a service to a pod in a different cluster of the caller's family), its `source_type` is `["service"]` (or `"service"`), its `target_type` is `["pod"]` (or `"pod"`), and its `labels` array does NOT enumerate a `relation` entry (a shared fan-out edge is never relation-marked)

#### Scenario: pod-to-node catalogue entry

- **WHEN** a client inspects the catalogue entry for `pod-to-node`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (a pod and its scheduled node are always in the same cluster), its `source_type` is `["pod"]` (or `"pod"`), and its `target_type` is `["node"]` (or `"node"`)

#### Scenario: pvc-to-storageclass catalogue entry

- **WHEN** a client inspects the catalogue for a `pvc-to-storageclass` entry
- **THEN** no such entry exists — the edge type is removed from the registry and replaced by `pvc-to-netapp-aggr`

#### Scenario: pvc-to-netapp-aggr catalogue entry

- **WHEN** a client inspects the catalogue entry for `pvc-to-netapp-aggr`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (the target NetApp aggregate belongs to no Kubernetes cluster, so the Kubernetes cross-cluster notion does not apply), its `source_type` is `["pvc"]` (or `"pvc"`), its `target_type` is `["netapp-aggr"]` (or `"netapp-aggr"`), and its `labels` array is empty

### Requirement: Cytoscape compound node grouping

`GET /v1/graph` SHALL express a workload compound hierarchy `cluster > namespace > application > controller > pod` (with **skip-absent-levels**), plus `cluster > namespace > { service, pvc }`, `cluster > node`, and the storage chain `storage-cluster > netapp-node > netapp-aggr`, via an optional `data.parent` field. The `cluster`, `storage-cluster`, `namespace`, `application`, and `controller` group nodes are synthesised by the Cytoscape serialiser; this is a presentation concern that SHALL NOT affect the core graph, projection, or traversal.

In the storage chain, the middle tier is NOT a synthesised group: the **real** `type="netapp-node"` node acts as the compound parent of its `netapp-aggr` nodes. This is a deliberate, explicitly-scoped break from the "relationships are edges, compound parents are synthesised groups" rule (the rule that removed pod-under-node nesting): it applies to the `netapp-node > netapp-aggr` tier ONLY, and no other real node type SHALL acquire compound children.

**Cluster group node.** For each distinct `labels.cluster` value present on an emitted node, the serialiser SHALL emit one `{ data: { id: "cluster/<cluster>", name: "<cluster>", type: "cluster", labels: {} } }` with no `parent` and no `ipaddress`.

**Storage-cluster group node.** For each distinct `labels.ontap_cluster` value present on an emitted `netapp-aggr` or `netapp-node` node, the serialiser SHALL emit one `{ data: { id: "storage-cluster/<ontap-cluster>", name: "<ontap-cluster>", type: "storage-cluster", labels: {} } }` with no `parent` and no `ipaddress`, so the NetApp node set nests under its own filer rather than floating parentless like `external` nodes. A storage-cluster group is NOT a Kubernetes cluster group; its name never appears in `clusters[]`.

**Synthesised workload group nodes.** Derived from each emitted real node's own attributes: `namespace` and `application` groups from any `type="pod"`, `type="service"`, or `type="pvc"` node (via its `labels.namespace` and its resolved `data.application`); `controller` groups from `type="pod"` nodes only (via the pod's resolved `data.owner` `{kind, name}`). Each group node's `id` encodes its full ancestry path, so it has exactly one parent (the tree is unambiguous by construction and no `data.parent` can dangle):

- **namespace** — `{ id: "<cluster>/namespace/<ns>", name: "<ns>", type: "namespace", labels: {}, parent: "cluster/<cluster>" }`
- **application** (emitted for any pod, service, or PVC with a resolved Application) — `{ id: "<cluster>/namespace/<ns>/application/<app>", name: "<app>", type: "application", labels: {}, parent: "<cluster>/namespace/<ns>" }`
- **controller** (emitted only for pods with a resolved owner) — `{ id: "<cluster>/namespace/<ns>/application/<app>/controller/<kind>/<name>", ... , parent: <the application group id> }` when the pod also has a resolved Application, otherwise `{ id: "<cluster>/namespace/<ns>/controller/<kind>/<name>", ... , parent: "<cluster>/namespace/<ns>" }`. `name` is `<name>`, `type` is `"controller"`, `labels` is `{}`.

All synthesised group nodes carry `labels: {}` and no `ipaddress`, and SHALL be emitted in tier order (cluster, then storage-cluster, then namespace, then application, then controller), each tier ordered by `id`, before the non-group nodes, so the body stays byte-deterministic.

**`data.parent` assignment** (skip-absent-levels):

- `type="pod"` → its controller group id when the pod has a resolved owner; else its application group id when it has a resolved Application; else its namespace group id `<cluster>/namespace/<ns>`. (Every pod with a namespace always has at least the namespace group.)
- `type="service"`, `type="pvc"` → its application group id `<cluster>/namespace/<ns>/application/<app>` when it has a resolved Application; else its namespace group id `<cluster>/namespace/<ns>` (skip-absent-levels). Services and PVCs SHALL NOT nest under a `controller` group.
- `type="node"` → `cluster/<labels.cluster>`.
- `type="netapp-node"` → `storage-cluster/<labels.ontap_cluster>`.
- `type="netapp-aggr"` → the **real** NetApp node id `netapp/<labels.ontap_cluster>/<labels.node>` (the aggregate's current owner; an HA takeover moves the parent, never the aggregate's id).
- `type="external"` → omitted (no cluster identity).

The `parent` field SHALL use `omitempty` semantics. The pod→node and pvc→aggregate relationships SHALL be expressed as the edges `pod-to-node` and `pvc-to-netapp-aggr`, **not** as compound nesting; K8s `node` and `netapp-aggr` nodes therefore carry edges and act as infrastructure-level nodes and edge endpoints (the `netapp-node` is a compound parent and the target of no edge). Services and PVCs SHALL NOT be compound parents of pods.

This requirement **supersedes** the prior `storageclass` grouping and node behaviour: there is no `type="storageclass"` node (real or synthesised) and no `pvc-to-storageclass` edge in any form; the claim's StorageClass name survives only as the PVC's own `data.storageclass` attribute.

#### Scenario: Cluster group node synthesised

- **WHEN** the graph contains any node with `labels.cluster="cluster-alpha"`
- **THEN** the Cytoscape response contains a node `{ data: { id: "cluster/cluster-alpha", name: "cluster-alpha", type: "cluster", labels: {} } }` with no `parent` field

#### Scenario: Storage-cluster group node synthesised

- **WHEN** the response contains a `netapp-node` node with `labels.ontap_cluster="ontap-prod"`
- **THEN** the response contains a node `{ data: { id: "storage-cluster/ontap-prod", name: "ontap-prod", type: "storage-cluster", labels: {} } }` with no `parent` field, and the `netapp-node` node's `data.parent` equals `storage-cluster/ontap-prod`

#### Scenario: Aggregate nests under its real owning node

- **WHEN** the response contains a `netapp-aggr` node with `labels={ontap_cluster:"ontap-prod", node:"ontap-prod-01"}`
- **THEN** its `data.parent` equals `netapp/ontap-prod/ontap-prod-01` — the id of the **real** `netapp-node` node, which is present in the response and itself carries `data.parent="storage-cluster/ontap-prod"`

#### Scenario: Full workload hierarchy nesting

- **WHEN** a pod in `cluster-alpha` namespace `shop` carries `data.application="checkout"` and `data.owner={kind:"Deployment", name:"checkout"}`
- **THEN** the response contains group nodes with ids `cluster-alpha/namespace/shop` (parent `cluster/cluster-alpha`), `cluster-alpha/namespace/shop/application/checkout` (parent `cluster-alpha/namespace/shop`), and `cluster-alpha/namespace/shop/application/checkout/controller/Deployment/checkout` (parent `cluster-alpha/namespace/shop/application/checkout`), and the pod's `data.parent` equals `cluster-alpha/namespace/shop/application/checkout/controller/Deployment/checkout`

#### Scenario: Skip-absent-levels — controller but no application

- **WHEN** a pod in `cluster-alpha` namespace `shop` carries `data.owner={kind:"DaemonSet", name:"fluentd"}` and has no resolved Application
- **THEN** the pod's `data.parent` equals `cluster-alpha/namespace/shop/controller/DaemonSet/fluentd`, whose `data.parent` is `cluster-alpha/namespace/shop`; no `application` group node is synthesised for it

#### Scenario: Skip-absent-levels — neither application nor controller

- **WHEN** a pod in `cluster-alpha` namespace `shop` has neither a resolved Application nor a resolved owner
- **THEN** the pod's `data.parent` equals `cluster-alpha/namespace/shop` (the namespace group)

#### Scenario: Service and PVC nested under namespace when no Application

- **WHEN** the response contains a `type="service"` node and a `type="pvc"` node in `cluster-alpha` namespace `shop`, neither with a resolved ArgoCD Application
- **THEN** each carries `data.parent="cluster-alpha/namespace/shop"`, and neither is the `parent` of any pod

#### Scenario: Service and PVC nested under application when resolved

- **WHEN** the response contains a `type="service"` node and a `type="pvc"` node in `cluster-alpha` namespace `shop`, each with `data.application="checkout"`
- **THEN** each carries `data.parent="cluster-alpha/namespace/shop/application/checkout"`, the `cluster-alpha/namespace/shop/application/checkout` application group node (parent `cluster-alpha/namespace/shop`) is synthesised, and neither the service nor the PVC nests under a `controller` group

#### Scenario: Application group synthesised from a service/PVC even with no pod in it

- **WHEN** the only node carrying `data.application="checkout"` in `cluster-alpha` namespace `shop` is a `type="service"` node (no pod resolves that Application)
- **THEN** the response still contains the `cluster-alpha/namespace/shop/application/checkout` application group node with `parent="cluster-alpha/namespace/shop"`, and no `controller` group is synthesised under it

#### Scenario: Node and StorageClass nested under cluster

- **WHEN** the response contains a `type="node"` node in `cluster-alpha`
- **THEN** it carries `data.parent="cluster/cluster-alpha"`; no `type="storageclass"` node exists to nest (the type is removed — NetApp nodes nest under their `storage-cluster` group instead)

#### Scenario: pod→node and pvc→storageclass are edges, not nesting

- **WHEN** the response contains a scheduled pod and a PVC with a resolved StorageClass name
- **THEN** the pod→node relationship appears as a `pod-to-node` edge (not via `data.parent`), while the former pvc→storageclass relationship no longer exists in any form — no edge, no node, no nesting; the name survives only as the PVC's `data.storageclass` attribute

#### Scenario: pod→node and pvc→netapp-aggr are edges, not nesting

- **WHEN** the response contains a scheduled pod and a PVC whose claim joined a NetApp aggregate
- **THEN** the pod→node relationship appears as a `pod-to-node` edge (not via `data.parent`) and the pvc→aggregate relationship appears as a `pvc-to-netapp-aggr` edge (not via `data.parent`); the K8s `node` and the `netapp-aggr` are NOT compound parents of the pod / PVC — the only real-node compound parenting is the `netapp-node > netapp-aggr` tier

#### Scenario: external nodes have no parent

- **WHEN** the response contains a `type="external"` node
- **THEN** that node's `data` has no `parent` field, and no cluster group node is synthesised for an endpoint carrying no `cluster` label

### Requirement: Namespace-filter retention of cluster-scoped infra nodes

`GET /v1/graph` projection SHALL treat `type="node"`, `type="netapp-aggr"`, and `type="netapp-node"` nodes as infrastructure nodes that carry no `namespace` label, and SHALL admit such a node to a response **iff it is referenced by an in-scope element** — a `type="node"` node when some in-scope pod is scheduled on it (its `labels.node`), a `type="netapp-aggr"` node when some in-scope PVC is joined to it via a `pvc-to-netapp-aggr` edge, and a `type="netapp-node"` node when some admitted `netapp-aggr` names it as owner (its `labels.node`) — a **transitive** reference chain PVC → aggregate → controller — on **every** request shape (no filter, `?cluster=`, `?namespace=`). The default (no-filter) response therefore lists only the host nodes of pods that are in the graph, the aggregates serving in-scope PVCs, and the controllers owning those aggregates; it SHALL NOT carry an orphan K8s node that hosts no pod, an aggregate serving no in-scope PVC, or a controller owning no admitted aggregate.

The cluster filter applies to `type="node"` exactly as to other node types (the node's own `labels.cluster`). The NetApp types carry NO `cluster` label, so a `?cluster=` filter SHALL NEVER admit or exclude them directly — their admission is purely reference-driven, which means a filer shared by two Kubernetes clusters appears in a `?cluster=` view of either cluster (via that cluster's in-scope PVCs).

The **one exception** is an explicit `?name=` filter: a `?name=<value>` request SHALL admit a `type="node"`, `type="netapp-aggr"`, or `type="netapp-node"` node whose `Name()` equals `<value>` **even when it is referenced by no in-scope element** (an empty / `NotReady` K8s node stays directly queryable, and an aggregate or controller whose every referencing element is out of scope stays directly queryable). When a `?name=` filter is active and does not name a given infra node, that node SHALL NOT be admitted by this rule; if it is instead the host of a named pod (or the aggregate serving a named PVC) it re-enters the response as that edge's re-added partner under the unified edge-retention rule, not by this admission rule — and a `netapp-aggr` admitted either way SHALL pull in its owning `netapp-node` (the compound parent must exist in the response).

This retention is a node-admission rule of the projection over the freshly built `*Graph`; the build SHALL still load every node (the full-topology graph is built unchanged), so the pruning SHALL NOT alter the core graph, push any filter to PromQL, or change the determinism of the response. A **consequence** of this rule is that a podless K8s node's `ready_status` / `ipaddress` is absent from the default view and is obtained with `?name=`; there is no exception that keeps an unhealthy (`NotReady` / `Unknown`) podless node — or a `degraded` aggregate/controller serving no in-scope PVC — in the default view.

#### Scenario: Default view drops a podless node

- **WHEN** the built graph has a node `cluster-alpha/worker-9` on which no pod is scheduled and a client sends `GET /v1/graph` with no filter
- **THEN** the response does not contain `cluster-alpha/worker-9`

#### Scenario: Default view keeps a node hosting an in-graph pod

- **WHEN** a pod is scheduled on node `cluster-alpha/worker-0` and a client sends `GET /v1/graph` with no filter
- **THEN** the response contains `cluster-alpha/worker-0` and the `pod-to-node` edge from the pod to it

#### Scenario: Default view drops a PVC-less StorageClass

- **WHEN** a client sends `GET /v1/graph` with any filter shape
- **THEN** the response never contains a `type="storageclass"` node — the type is removed; the analogous reference-driven admission now governs the `netapp-aggr` / `netapp-node` chain

#### Scenario: StorageClass retained when a filtered-in PVC references it

- **WHEN** the graph has a PVC in namespace `shop` whose claim joined aggregate `netapp/ontap-prod/aggr/aggr1` owned by `ontap-prod-01`, and a client sends `?namespace=shop`
- **THEN** the retention this scenario formerly gave the StorageClass now applies to the NetApp chain: the response contains the `shop` PVC, the `netapp/ontap-prod/aggr/aggr1` node, its `pvc-to-netapp-aggr` edge, and the owning `netapp/ontap-prod/ontap-prod-01` node (the aggregate's compound parent)

#### Scenario: Name filter on an unused StorageClass surfaces it

- **WHEN** a client sends `?name=gp3` where `gp3` was formerly a StorageClass name and no other node type carries that name
- **THEN** the response is 200 with empty `elements.nodes` and `elements.edges` — StorageClass nodes no longer exist to surface; the `?name=` escape hatch applies to `node`, `netapp-aggr`, and `netapp-node` infra nodes instead

#### Scenario: Shared filer visible from either cluster's filtered view

- **WHEN** PVCs in `cluster-alpha` and `cluster-beta` both join `netapp/ontap-prod/aggr/aggr1` and a client sends `?cluster=cluster-alpha`
- **THEN** the response contains `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` (referenced by `cluster-alpha`'s in-scope PVC), and a `?cluster=cluster-beta` request equally contains them

#### Scenario: Cluster filter keeps only referenced infra nodes

- **WHEN** `?cluster=cluster-alpha` is sent and `cluster-alpha` has a node `worker-0` hosting a pod and a node `worker-1` hosting nothing
- **THEN** the response contains `cluster-alpha/worker-0` and not `cluster-alpha/worker-1`

#### Scenario: Name filter surfaces an unreferenced infra node

- **WHEN** node `cluster-alpha/worker-9` hosts no pod and a client sends `?name=worker-9`
- **THEN** the response contains `cluster-alpha/worker-9` (with its `ready_status` / `ipaddress` when resolved), admitted by the explicit name match despite being referenced by no in-scope pod

#### Scenario: Name filter surfaces a NetApp aggregate directly

- **WHEN** aggregate `netapp/ontap-prod/aggr/aggr1` serves no in-scope PVC under the active projection and a client sends `?name=aggr1`
- **THEN** the response contains `netapp/ontap-prod/aggr/aggr1` (with its `health` / `usage` attributes when resolved) and its owning `netapp-node` (the compound parent)

#### Scenario: Name filter surfaces a NetApp node directly

- **WHEN** NetApp node `netapp/ontap-prod/ontap-prod-01` owns no admitted aggregate under the active projection and a client sends `?name=ontap-prod-01`
- **THEN** the response contains `netapp/ontap-prod/ontap-prod-01` with its `health` attribute when resolved

#### Scenario: K8s node retained when a filtered-in pod is scheduled on it

- **WHEN** a pod in namespace `shop` is scheduled on node `cluster-alpha/worker-0` and a client sends `?namespace=shop`
- **THEN** the response contains node `cluster-alpha/worker-0` and the `pod-to-node` edge from the pod to it

#### Scenario: Podless NotReady node is hidden by default (no health exception)

- **WHEN** node `cluster-alpha/worker-broken` hosts no pod and its `ready_status` is `NotReady` and a client sends `GET /v1/graph` with no filter
- **THEN** the response does not contain `cluster-alpha/worker-broken` (it is obtained with `?name=worker-broken`)

### Requirement: PVC `volumename` and `svm` labels

A `type="pvc"` node's `data.labels` SHALL additively carry two further string entries whenever they resolve:

- `volumename` — the name of the PersistentVolume bound to the claim (from the `volumename` label of `kube_persistentvolumeclaim_info`, per the `cluster-topology-source` capability).
- `svm` — the NetApp ONTAP SVM serving the claim (from the `svm` label of the Harvest volume series matched by the `netapp-storage-graph` capability's PV-name join — the removed Trident custom-resource chain is no longer the source, with the label's shape unchanged).

Both are plain `labels` entries (strict `map[string]string`) — there SHALL be NO `data.volumename` or `data.svm` typed field on the PVC node. Each key SHALL be **absent** when its value is unresolved; an empty-string value SHALL never be emitted. `svm` SHALL never be present without `volumename`. The `volumename` key is distinct from the existing `volume` key (the pod-spec volume name); both MAY appear on the same node.

The additions are additive to the v1 wire contract and MUST NOT disturb the deterministic-body guarantee: for identical upstream data the label set is byte-identical across rebuilds, and responses built from upstreams without the Harvest series are unchanged except for `volumename` appearing wherever `kube_persistentvolumeclaim_info` carries it.

#### Scenario: PVC node with a fully-resolved NetApp chain

- **WHEN** the response contains a PVC node whose PV name and SVM resolved (e.g. PV `pvc-9f3a` on SVM `svm-prod`)
- **THEN** its `data.labels.volumename` equals `"pvc-9f3a"`, its `data.labels.svm` equals `"svm-prod"`, and its `data` has no `volumename` or `svm` field outside `labels`

#### Scenario: PVC node with an unresolved chain omits the keys

- **WHEN** the response contains a PVC node whose claim reported no `volumename` (or whose Harvest join did not resolve)
- **THEN** its `data.labels` has no `volumename` key (or carries `volumename` but no `svm` key, respectively); neither key is ever present with an empty-string value

#### Scenario: volume and volumename are distinct keys

- **WHEN** the response contains a PVC node that derives a pod-spec volume name `data` and a bound PV name `pvc-9f3a`
- **THEN** `data.labels.volume` equals `"data"` and `data.labels.volumename` equals `"pvc-9f3a"` — the two keys coexist and carry different values

### Requirement: Pod `application` and `containers` attributes

Every `data` object for a `type="pod"`, `type="service"`, or `type="pvc"` node SHALL be able to expose an `application` attribute, and every `type="pod"` node SHALL additionally be able to expose a `containers` attribute, all with `omitempty` semantics and all **outside `labels`** (which stays a strict `map[string]string`):

- `application` — a `string`, the node's ArgoCD Application name as resolved by the
  `cluster-topology-source` capability: for `type="pod"` from the `argocd_tracking_id`
  label on `kube_pod_owner`, and for `type="service"` / `type="pvc"` from the
  `annotation_argocd_argoproj_io_tracking_id` label on `kube_service_annotations` /
  `kube_persistentvolumeclaim_annotations` (see "Service and PVC ArgoCD Application
  resolution"). Emitted only when the node has a resolved Application; omitted entirely
  otherwise (never an empty string). This attribute is **complementary** to the
  synthesised `type="application"` group node (which is derived from this same value —
  see "Cytoscape compound node grouping"); an existing consumer reading
  `data.application` on a pod is unaffected (additive on services and PVCs).
- `containers` — an array of objects `[{ name: string, image: string }]`, one per
  container, as resolved by the `cluster-topology-source` capability and ordered
  deterministically by `(name, image)`. Emitted only on `type="pod"` nodes and only
  when the pod has at least one resolved container; omitted entirely otherwise (never
  an empty array).

The `application` attribute SHALL appear only on `type="pod"`, `type="service"`, and
`type="pvc"` nodes. The `containers` attribute SHALL appear only on `type="pod"`
nodes. `type="node"`, `type="external"`, `type="netapp-aggr"`, `type="netapp-node"`,
and the synthesised `type="cluster"` / `type="storage-cluster"` / `type="namespace"`
/ `type="application"` / `type="controller"` group nodes SHALL NOT emit
`application` or `containers`. The
attributes SHALL NOT appear inside `labels`, and SHALL NOT be encoded as numbers or
booleans. Because both are `omitempty`, a node with neither a resolved Application nor
container info produces a `data` object byte-identical to the pre-change shape.

#### Scenario: Pod node carries application when resolved

- **WHEN** the response contains a pod node whose `kube_pod_owner` series carried an `argocd_tracking_id` resolving to Application `checkout`
- **THEN** the corresponding `type="pod"` node carries `data.application: "checkout"` and `data.labels` contains no `argocd_tracking_id` / `application` key

#### Scenario: Service node carries application when resolved

- **WHEN** the response contains a service node whose `kube_service_annotations` series carried `annotation_argocd_argoproj_io_tracking_id` resolving to Application `checkout`
- **THEN** the corresponding `type="service"` node carries `data.application: "checkout"` and `data.labels` contains no `annotation_argocd_argoproj_io_tracking_id` / `application` key

#### Scenario: PVC node carries application when resolved

- **WHEN** the response contains a PVC node whose `kube_persistentvolumeclaim_annotations` series carried `annotation_argocd_argoproj_io_tracking_id` resolving to Application `mongo`
- **THEN** the corresponding `type="pvc"` node carries `data.application: "mongo"` and `data.labels` contains no `annotation_argocd_argoproj_io_tracking_id` / `application` key

#### Scenario: PVC node carries inherited application from a mounting pod

- **WHEN** the response contains a PVC node that has no own `annotation_argocd_argoproj_io_tracking_id` annotation but is mounted (via a `pod-mounts-pvc` edge) by a pod resolving ArgoCD Application `checkout` (see cluster-topology-source "PVC ArgoCD Application inheritance from mounting pod")
- **THEN** the corresponding `type="pvc"` node carries `data.application: "checkout"` — indistinguishable from an annotation-sourced value — `data.labels` contains no `application` / tracking-id key, and the PVC nests under the `<cluster>/namespace/<ns>/application/checkout` compound group

#### Scenario: Pod node carries containers when resolved

- **WHEN** the response contains a pod node whose `kube_pod_container_info` series listed containers `app` (`reg/app:1.2`) and `sidecar` (`reg/proxy:0.9`)
- **THEN** the corresponding `type="pod"` node carries `data.containers: [{"name":"app","image":"reg/app:1.2"},{"name":"sidecar","image":"reg/proxy:0.9"}]` ordered by `(name, image)` and `data.labels` contains no container key

#### Scenario: Pod node omits application and containers when unresolved

- **WHEN** the response contains a pod node with no resolved ArgoCD Application and no container info
- **THEN** the corresponding `type="pod"` node's `data` object includes neither an `application` field nor a `containers` field

#### Scenario: Service and PVC omit application when unresolved

- **WHEN** the response contains a service node and a PVC node with no resolved ArgoCD Application
- **THEN** neither node's `data` object includes an `application` field, and neither includes a `containers` field

#### Scenario: Node, external, and storageclass never carry application or containers

- **WHEN** the response contains nodes of `type="node"`, `type="external"`, `type="netapp-aggr"`, or `type="netapp-node"` (the `storageclass` type this scenario formerly named is removed)
- **THEN** those node `data` objects include neither an `application` field nor a `containers` field

#### Scenario: Deterministic body with new attributes

- **WHEN** the same pod (same Application and container set) is produced by two consecutive builds for the same time bucket
- **THEN** the pod node's `data.application` and `data.containers` are byte-identical between the two builds, with `data.containers` ordered by `(name, image)`

## ADDED Requirements

### Requirement: NetApp aggregate and node payloads

`GET /v1/graph` SHALL emit each NetApp aggregate and each NetApp node (materialised by the `netapp-storage-graph` capability) as real first-class nodes `{ data: { id, name, type, labels } }`:

**Aggregate** (`type="netapp-aggr"`):
- `id` SHALL be `netapp/<ontap-cluster>/aggr/<aggr>` (aggregate names are cluster-wide unique within an ONTAP cluster; the id excludes the owning node so an HA takeover never changes it).
- `name` SHALL equal the ONTAP aggregate name (`<aggr>`).
- `labels` SHALL be a strict `map[string]string` containing exactly `ontap_cluster` and `node` (the current owning controller, which drives `data.parent`) — deliberately no `cluster` key.
- `data` MAY additionally carry, with `omitempty` semantics and **outside `labels`**:
  - `health` — a `string`, exactly one of `"online"` or `"degraded"`, per the `netapp-storage-graph` capability's per-aggregate `aggr_new_status` read. Omitted entirely when the aggregate has no status data — absence of data is distinct from a reported unhealthy state and the two SHALL NOT be conflated.
  - `usage` — an object `{used_bytes, capacity_bytes}` of JSON numbers (bytes) from `aggr_space_used` / `aggr_space_total` — the **same shape** as the PVC `usage` attribute. Present iff at least one field resolved; an unresolved field omitted; values never strings.

**Controller** (`type="netapp-node"`):
- `id` SHALL be `netapp/<ontap-cluster>/<node>` (ONTAP controller names are not globally unique across ONTAP clusters).
- `name` SHALL equal the ONTAP controller name (`<node>`).
- `labels` SHALL be a strict `map[string]string` containing exactly an `ontap_cluster` entry — deliberately no `cluster` key.
- `data` MAY additionally carry `health` (`omitempty`, outside `labels`) — a `string`, exactly one of `"online"` or `"degraded"`, from `node_new_status` per the `netapp-storage-graph` capability; omitted when the controller has no status data, never a defaulted `"degraded"`.

Because neither type carries a `cluster` label, both stay out of `clusters[]` and out of the `?cluster=` domain. Neither SHALL emit `ipaddress`, `owner`, `application`, `containers`, `ready_status`, `provisioner`, or `parameters`.

#### Scenario: NetApp aggregate payload

- **WHEN** the response contains the aggregate `aggr1` of ONTAP cluster `ontap-prod`, owned by controller `ontap-prod-01`, online, with 700 GB used of 1 TB
- **THEN** the response contains `{ data: { id: "netapp/ontap-prod/aggr/aggr1", name: "aggr1", type: "netapp-aggr", labels: { ontap_cluster: "ontap-prod", node: "ontap-prod-01" }, health: "online", usage: { used_bytes: 700000000000, capacity_bytes: 1000000000000 }, parent: "netapp/ontap-prod/ontap-prod-01" } }` with no `ipaddress` field and no `cluster` label

#### Scenario: NetApp node payload

- **WHEN** the response contains a NetApp node for controller `ontap-prod-01` in ONTAP cluster `ontap-prod` whose `node_new_status` is `1`
- **THEN** the response contains `{ data: { id: "netapp/ontap-prod/ontap-prod-01", name: "ontap-prod-01", type: "netapp-node", labels: { ontap_cluster: "ontap-prod" }, health: "online", parent: "storage-cluster/ontap-prod" } }` with no `ipaddress` field and no `cluster` label

#### Scenario: NetApp payloads omit health when no status data

- **WHEN** a NetApp aggregate has no matching `aggr_new_status` series and a NetApp node has no matching `node_new_status` series
- **THEN** neither `data` has a `health` field — never `health: ""` and never a defaulted `"degraded"`

### Requirement: PVC `storageclass` and `usage` attributes

A `type="pvc"` node's `data` MAY carry two typed attributes, both with `omitempty` semantics, both **outside `labels`**, and neither encoded as a string map entry:

- `storageclass` — a `string`, the claim's StorageClass name as resolved by the `cluster-topology-source` capability ("PVC StorageClass resolution"). Emitted only when the name resolved non-empty; omitted entirely otherwise (never an empty string). This **supersedes** the previous rule that no `data.storageclass` field exists — the StorageClass node that rule pointed to is removed, and the name is retained on the claim itself as the operator's discriminator for the `netapp-storage-graph` join-coverage signal.
- `usage` — an object `{ used_bytes, capacity_bytes }` of JSON numbers (bytes), from the kubelet volume-stats series per the `cluster-topology-source` capability. The object SHALL be present iff at least one field resolved; an unresolved field SHALL be omitted from the object; values SHALL never be strings.

Both attributes SHALL appear only on `type="pvc"` nodes. Neither SHALL appear inside `labels`. Because both are `omitempty`, a PVC with neither resolved produces a `data` object byte-identical to the pre-change shape (modulo the removed edge/node).

#### Scenario: PVC node carries storageclass and usage

- **WHEN** the response contains a PVC node whose claim resolved StorageClass `netapp-nas`, `used_bytes` 5368709120, and `capacity_bytes` 10737418240
- **THEN** the corresponding `type="pvc"` node carries `data.storageclass: "netapp-nas"` and `data.usage: {"used_bytes": 5368709120, "capacity_bytes": 10737418240}`, and `data.labels` contains no `storageclass`, `used_bytes`, or `capacity_bytes` key

#### Scenario: PVC node omits unresolved attributes

- **WHEN** the response contains a PVC node with no matching `kube_persistentvolumeclaim_info` series and no kubelet volume-stats series
- **THEN** its `data` has neither a `storageclass` nor a `usage` field

#### Scenario: Partial usage keeps only the resolved field

- **WHEN** a PVC resolved `capacity_bytes` but no `used_bytes`
- **THEN** its `data.usage` equals `{"capacity_bytes": <value>}` with no `used_bytes` key

#### Scenario: Usage values are JSON numbers

- **WHEN** any PVC node carries `data.usage`
- **THEN** every value inside it is a JSON number, not a string

## REMOVED Requirements

### Requirement: StorageClass node payload

**Reason**: The `type="storageclass"` node is removed — the storage half of the graph re-anchors on the physical ONTAP controller (`netapp-node`) instead of the Kubernetes provisioning policy, and the `data.provisioner` / `data.parameters` attributes are dropped with the `kube_storageclass_info` query that fed them.

**Migration**: Read the claim's own `data.storageclass` for the policy name (see "PVC `storageclass` and `usage` attributes"). The physical backend surfaces via the `pvc-to-netapp-aggr` edge, the `netapp-aggr` / `netapp-node` payloads, and the PVC's `svm` / `volumename` labels; provisioner and parameter values are no longer served by this API.
