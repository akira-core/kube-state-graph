## MODIFIED Requirements

### Requirement: Cytoscape.js response shape

`GET /v1/graph` SHALL return a JSON document in Cytoscape.js shape: `{ apiVersion, clusters, elements: { nodes, edges } }`. The body SHALL NOT contain time-varying or echo-of-input fields, so identical inputs against the same upstream state produce byte-identical bodies.

Each **node** SHALL be `{ data: { id, name, type, labels } }`:
- `id` SHALL be a cluster-scoped composite for pods / K8s nodes / PVCs / services / StorageClasses (pods: `<cluster>/<pod-uid>`; nodes: `<cluster>/<node-name>`; PVCs: `<cluster>/<namespace>/<claim>`; services: `<cluster>/<namespace>/<service>`; StorageClasses: `<cluster>/storageclass/<name>`). For external nodes (unresolvable `"://"` connection-string endpoints or missing-UID human-label fallback), `id` SHALL be `external/<label-value>` (no cluster prefix).
- `name` SHALL be the human-readable pod / node / PVC / service / StorageClass name. For external nodes, `name` SHALL be the verbatim `client` or `server` label value from the source service-graph series.
- `type` SHALL be one of the strings `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, `"storageclass"`. The Cytoscape serialiser additionally synthesises `"cluster"`, `"namespace"`, `"application"`, and `"controller"` group nodes for compound nesting (see "Cytoscape compound node grouping").
- `data` MAY carry an optional `parent` field (`omitempty`) referencing the `id` of the node's Cytoscape compound container — see "Cytoscape compound node grouping".
- `labels` SHALL be a JSON object whose values are strings only (`map[string]string`). For pod / K8s node / PVC / service / StorageClass nodes it SHALL include at minimum a `cluster` entry; for pods, PVCs, and services it SHALL also include a `namespace` entry; for pods it SHALL include `node` (the cluster-scoped node ID), and SHALL include `pod_ip` and `host_ip` whenever the upstream `kube_pod_info` series carried them; for K8s nodes it SHALL include `external_ip` when the upstream provided one. **For external nodes**, `labels` SHALL be an empty object `{}` (no `cluster` key).

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
- **THEN** the PVC node's `data` has no `storageclass` field and its `labels` has no `storageclass` key — the StorageClass surfaces only via a `pvc-to-storageclass` edge to the real `type="storageclass"` node (not via `data.parent` and not as a synthesised group node)

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

The `name` parameter SHALL match `n.Name()` by exact string equality across **every** node type (`PodNode`, `K8sNode`, `PVCNode`, `ServiceNode`, `ExternalNode`, `StorageClassNode`) — a single `?name=` value matches a pod, a K8s node, a PVC, a service, an external node, or a StorageClass with the same name. Names are not globally unique (pods and K8s nodes can share a name; PVCs and services can repeat across namespaces; StorageClass names repeat across clusters); all matches SHALL be returned.

**Edge retention rule (unified across all filters).** An edge SHALL be retained when at least one resolved endpoint is in scope after node filtering. When exactly one endpoint is in scope, the missing endpoint SHALL be re-added from the freshly built graph's node index provided it passes the non-cluster filters (namespace check; types without a namespace label — `node`, `storageclass`, `external` — pass through). This single rule is edge-type-agnostic and covers (a) anchoring on a named node and visualising its incident edges with their partner endpoints — including the topology `pod-to-node` and `pvc-to-storageclass` edges — and (b) cross-cluster edges where only `cluster` narrows scope and the partner endpoint lives outside the in-scope cluster set.

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
- **THEN** the response contains the `frontend` pod node and any K8s-node, PVC, StorageClass, or external-endpoint nodes that are edge endpoints of `frontend`, but NOT the `backend` pod node

#### Scenario: Name filter matches a K8s node

- **WHEN** the freshly built graph contains a K8s node named `worker-1` in `cluster-alpha` and a client sends `?name=worker-1`
- **THEN** the response contains the `worker-1` K8s-node node and, via the `pod-to-node` edges incident to it, the pods scheduled on it (re-added as the missing edge endpoints per the unified edge-retention rule)

#### Scenario: Name filter matches a PVC

- **WHEN** the freshly built graph contains a PVC named `checkout-data` in `cluster-alpha/shop` and a client sends `?name=checkout-data`
- **THEN** the response contains the `checkout-data` PVC node, any pod nodes that mount it via `pod-mounts-pvc`, and its StorageClass node via `pvc-to-storageclass` when one is resolved

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

### Requirement: Edge-type discovery endpoint

The server SHALL expose `GET /v1/edge-types` that returns the static catalogue of edge types this server can produce. The response SHALL list at least `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, and `pvc-to-storageclass`. Each catalogue entry SHALL describe `source_type` (one of `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, `"storageclass"`, **or a JSON array of such strings** when more than one is permitted), `target_type` (same form as `source_type`), `directed`, `may_cross_cluster`, and a `labels` array enumerating the keys this edge type can emit on edge `labels`. The endpoint SHALL NOT issue any upstream calls and SHALL NOT depend on time-range or cluster parameters. The response SHALL include a long `Cache-Control: public, max-age=3600` header.

#### Scenario: Static catalogue

- **WHEN** a client sends `GET /v1/edge-types`
- **THEN** the response body contains an `edge_types` array including objects whose `type` values include `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, and `pvc-to-storageclass`

#### Scenario: pod-calls-pod marked may_cross_cluster

- **WHEN** a client inspects the catalogue entry for `pod-calls-pod`
- **THEN** its `may_cross_cluster` field is `true`, its `source_type` and `target_type` are arrays containing `"pod"` and `"external"`, and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (representing the trace source cluster; cross-cluster status is detected by comparing the source/target nodes' `labels.cluster` rather than from edge labels)

#### Scenario: pod-calls-service catalogue entry

- **WHEN** a client inspects the catalogue entry for `pod-calls-service`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (a `"://"` connection string resolves to a service node in the caller's OWN cluster, so a `pod-calls-service` edge always connects a source to a service node in the same cluster and is never cross-cluster), its `source_type` is an array containing `"pod"` and `"external"`, its `target_type` is `"service"` (or `["service"]`), and its `labels` array enumerates an entry whose `name` is `cluster` with `value_type: "string"` (omitted when the client side is non-pod)

#### Scenario: service-selects-pod catalogue entry

- **WHEN** a client inspects the catalogue entry for `service-selects-pod`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `true` (a local service node fans out to backing pods across same-family clusters holding the same-named Service, so the edge may connect a service to a pod in a different cluster of the caller's family), its `source_type` is `["service"]` (or `"service"`), and its `target_type` is `["pod"]` (or `"pod"`)

#### Scenario: pod-to-node catalogue entry

- **WHEN** a client inspects the catalogue entry for `pod-to-node`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (a pod and its scheduled node are always in the same cluster), its `source_type` is `["pod"]` (or `"pod"`), and its `target_type` is `["node"]` (or `"node"`)

#### Scenario: pvc-to-storageclass catalogue entry

- **WHEN** a client inspects the catalogue entry for `pvc-to-storageclass`
- **THEN** its `directed` field is `true`, its `may_cross_cluster` field is `false` (a PVC and its StorageClass are always in the same cluster), its `source_type` is `["pvc"]` (or `"pvc"`), and its `target_type` is `["storageclass"]` (or `"storageclass"`)

### Requirement: Cytoscape compound node grouping

`GET /v1/graph` SHALL express a workload compound hierarchy `cluster > namespace > application > controller > pod` (with **skip-absent-levels**), plus `cluster > namespace > { service, pvc }` and `cluster > { node, storageclass }`, via an optional `data.parent` field. The `cluster`, `namespace`, `application`, and `controller` group nodes are synthesised by the Cytoscape serialiser; this is a presentation concern that SHALL NOT affect the core graph, projection, or traversal. `storageclass` is a **real** graph node (see "StorageClass node payload"), not a synthesised group.

**Cluster group node.** For each distinct `labels.cluster` value present on an emitted node, the serialiser SHALL emit one `{ data: { id: "cluster/<cluster>", name: "<cluster>", type: "cluster", labels: {} } }` with no `parent` and no `ipaddress`.

**Synthesised workload group nodes.** Derived from each emitted real node's own attributes: `namespace` and `application` groups from any `type="pod"`, `type="service"`, or `type="pvc"` node (via its `labels.namespace` and its resolved `data.application`); `controller` groups from `type="pod"` nodes only (via the pod's resolved `data.owner` `{kind, name}`). Each group node's `id` encodes its full ancestry path, so it has exactly one parent (the tree is unambiguous by construction and no `data.parent` can dangle):

- **namespace** — `{ id: "<cluster>/namespace/<ns>", name: "<ns>", type: "namespace", labels: {}, parent: "cluster/<cluster>" }`
- **application** (emitted for any pod, service, or PVC with a resolved Application) — `{ id: "<cluster>/namespace/<ns>/application/<app>", name: "<app>", type: "application", labels: {}, parent: "<cluster>/namespace/<ns>" }`
- **controller** (emitted only for pods with a resolved owner) — `{ id: "<cluster>/namespace/<ns>/application/<app>/controller/<kind>/<name>", ... , parent: <the application group id> }` when the pod also has a resolved Application, otherwise `{ id: "<cluster>/namespace/<ns>/controller/<kind>/<name>", ... , parent: "<cluster>/namespace/<ns>" }`. `name` is `<name>`, `type` is `"controller"`, `labels` is `{}`.

All synthesised group nodes carry `labels: {}` and no `ipaddress`, and SHALL be emitted in tier order (cluster, then namespace, then application, then controller), each tier ordered by `id`, before the non-group nodes, so the body stays byte-deterministic.

**`data.parent` assignment** (skip-absent-levels):

- `type="pod"` → its controller group id when the pod has a resolved owner; else its application group id when it has a resolved Application; else its namespace group id `<cluster>/namespace/<ns>`. (Every pod with a namespace always has at least the namespace group.)
- `type="service"`, `type="pvc"` → its application group id `<cluster>/namespace/<ns>/application/<app>` when it has a resolved Application; else its namespace group id `<cluster>/namespace/<ns>` (skip-absent-levels). Services and PVCs SHALL NOT nest under a `controller` group.
- `type="node"`, `type="storageclass"` → `cluster/<labels.cluster>`.
- `type="external"` → omitted (no cluster identity).

The `parent` field SHALL use `omitempty` semantics. The pod→node and pvc→storageclass relationships SHALL be expressed as the edges `pod-to-node` and `pvc-to-storageclass`, **not** as compound nesting; K8s `node` and `storageclass` nodes therefore carry edges and act as cluster-level nodes and edge endpoints. Services and PVCs SHALL NOT be compound parents of pods.

This requirement **supersedes** the prior `cluster > node > pod` and `cluster > storageclass > pvc` compound-grouping behaviour (design D31 and the 2026-06-08 StorageClass-grouping change): pods no longer nest under their K8s node, PVCs no longer nest under a synthesised StorageClass group, and there is no synthesised `type="storageclass"` group node.

#### Scenario: Cluster group node synthesised

- **WHEN** the graph contains any node with `labels.cluster="cluster-alpha"`
- **THEN** the Cytoscape response contains a node `{ data: { id: "cluster/cluster-alpha", name: "cluster-alpha", type: "cluster", labels: {} } }` with no `parent` field

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

- **WHEN** the response contains a `type="node"` node and a `type="storageclass"` node in `cluster-alpha`
- **THEN** each carries `data.parent="cluster/cluster-alpha"`

#### Scenario: pod→node and pvc→storageclass are edges, not nesting

- **WHEN** the response contains a scheduled pod and a PVC with a resolved StorageClass
- **THEN** the pod→node relationship appears as a `pod-to-node` edge (not via `data.parent`) and the pvc→storageclass relationship appears as a `pvc-to-storageclass` edge (not via `data.parent`), and the K8s `node` / `storageclass` nodes are NOT compound parents of the pod / PVC

#### Scenario: external nodes have no parent

- **WHEN** the response contains a `type="external"` node
- **THEN** that node's `data` has no `parent` field, and no cluster group node is synthesised for an endpoint carrying no `cluster` label

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
nodes. `type="node"`, `type="external"`, `type="storageclass"`, and the synthesised
`type="cluster"` / `type="namespace"` / `type="application"` / `type="controller"`
group nodes SHALL NOT emit `application` or `containers`. The attributes SHALL NOT
appear inside `labels`, and SHALL NOT be encoded as numbers or booleans. Because both
are `omitempty`, a node with neither a resolved Application nor container info produces
a `data` object byte-identical to the pre-change shape.

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

- **WHEN** the response contains nodes of `type="node"`, `type="external"`, or `type="storageclass"`
- **THEN** those node `data` objects include neither an `application` field nor a `containers` field

#### Scenario: Deterministic body with new attributes

- **WHEN** the same pod (same Application and container set) is produced by two consecutive builds for the same time bucket
- **THEN** the pod node's `data.application` and `data.containers` are byte-identical between the two builds, with `data.containers` ordered by `(name, image)`

## ADDED Requirements

### Requirement: StorageClass node payload

`GET /v1/graph` SHALL emit each StorageClass as a real first-class node `{ data: { id, name, type, labels } }`:

- `type` SHALL equal `"storageclass"`.
- `id` SHALL be the cluster-scoped composite `<cluster>/storageclass/<name>` (StorageClass names are not globally unique across clusters).
- `name` SHALL equal the StorageClass name.
- `labels` SHALL be a strict `map[string]string` containing exactly a `cluster` entry; the StorageClass's provisioner and backing-storage values do NOT live in `labels`.

A StorageClass node MAY additionally carry two top-level typed `data` attributes with `omitempty` semantics, both **outside `labels`** and resolved by cluster-topology-source "StorageClass entity from kube_storageclass_info":

- `provisioner` — a `string`, the StorageClass provisioner from the native `kube_storageclass_info` `provisioner` label; emitted only when non-empty.
- `parameters` — an object `map[string]string` of the NetApp/Ceph backing-storage values with keys `pool`, `fs`, `cluster_id`, `selector` (each key present only when its source label resolved non-empty); emitted only when at least one key is present.

A StorageClass node SHALL NOT emit `ipaddress`, `owner`, `application`, `containers`, or `ready_status`.

#### Scenario: StorageClass node payload with provisioner and parameters

- **WHEN** `kube_storageclass_info{cluster="cluster-alpha", storageclass="netapp-nas", provisioner="csi.trident.netapp.io", storagePools="aggr1", fsType="nfs", ClusterID="ceph-uuid", selector="region=eu"}` is present and at least one in-scope PVC references it
- **THEN** the response contains a node `{ data: { id: "cluster-alpha/storageclass/netapp-nas", name: "netapp-nas", type: "storageclass", labels: { cluster: "cluster-alpha" }, provisioner: "csi.trident.netapp.io", parameters: { pool: "aggr1", fs: "nfs", cluster_id: "ceph-uuid", selector: "region=eu" } } }` with no `ipaddress` field

#### Scenario: StorageClass node omits unset provisioner and parameter keys

- **WHEN** a `kube_storageclass_info` series carries `storageclass` and `pool` only (no `provisioner`, and no `fs`/`fsType`/`fsName`, `ClusterID`, or `selector`)
- **THEN** the StorageClass node's `labels` is `{ cluster: ... }`, its `data` has no `provisioner` field, and its `data.parameters` is `{ pool: ... }` with no `fs`, `cluster_id`, or `selector` key

#### Scenario: Bare StorageClass node omits both typed attributes

- **WHEN** a StorageClass node is materialised only because a PVC references it (absent from `kube_storageclass_info`)
- **THEN** its `data` has neither a `provisioner` nor a `parameters` field, and its `labels` is `{ cluster: ... }`

### Requirement: Namespace-filter retention of cluster-scoped infra nodes

`GET /v1/graph` projection SHALL treat `type="node"` and `type="storageclass"` nodes as cluster-scoped infrastructure nodes that carry no `namespace` label. Under a `?namespace=` filter, such a node SHALL be retained **iff** it is referenced by an in-scope element: a `type="node"` node is retained when some in-scope pod is scheduled on it (its `labels.node`), and a `type="storageclass"` node is retained when some in-scope PVC resolves to it. Without a `?namespace=` filter these nodes are unaffected. This retention is a node-admission rule of the projection; it SHALL NOT alter the core graph or the determinism of the response.

#### Scenario: StorageClass retained when a filtered-in PVC references it

- **WHEN** the graph has a PVC in namespace `shop` resolving to StorageClass `cluster-alpha/storageclass/gp3`, the StorageClass node carries no namespace, and a client sends `?namespace=shop`
- **THEN** the response contains the `shop` PVC, the `cluster-alpha/storageclass/gp3` node, and the `pvc-to-storageclass` edge between them

#### Scenario: StorageClass dropped when no in-scope PVC references it

- **WHEN** a StorageClass `cluster-alpha/storageclass/unused` is referenced only by PVCs in namespace `db` and a client sends `?namespace=shop`
- **THEN** the response does not contain the `cluster-alpha/storageclass/unused` node

#### Scenario: K8s node retained when a filtered-in pod is scheduled on it

- **WHEN** a pod in namespace `shop` is scheduled on node `cluster-alpha/worker-0` and a client sends `?namespace=shop`
- **THEN** the response contains node `cluster-alpha/worker-0` and the `pod-to-node` edge from the pod to it
