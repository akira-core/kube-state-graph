# graph-api Specification

## Purpose
TBD - created by archiving change add-k8s-pod-graph-api. Update Purpose after archive.

## Requirements

### Requirement: Versioned route prefix

The HTTP API SHALL expose every endpoint under the `/v1/` route prefix and SHALL include `apiVersion: "v1"` as a top-level field in every JSON response body.

#### Scenario: Body carries apiVersion

- **WHEN** a client sends `GET /v1/edge-types`
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
- **THEN** its `data.labels` still contains only string values and no `rate`, `error_rate`, `p90_server_ms`, `read_ops`, `write_ops`, `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`, `write_bytes_per_sec`, `max_iops`, or `max_bytes_per_sec` key

### Requirement: Filter parameters

`GET /v1/graph` SHALL accept the optional filter parameters `cluster`, `namespace`, `az`, `env`, `edge_type` (each repeatable) and `prune` (single-valued, `true` | `false`, default `true`). The request surface is exactly `start`, `end`, `cluster`, `namespace`, `az`, `env`, `edge_type`, `prune`; every parameter except `start` / `end` is optional. Multiple values for the same parameter SHALL be OR-combined; different parameters SHALL be AND-combined. An unknown filter **value** (a cluster, namespace, zone, or environment with no data) SHALL NOT cause an error — it yields an empty result. An unknown filter **parameter** (including the withdrawn `name`, `root`, `depth`, `direction`) SHALL be ignored without error.

Filters fall into two classes:

- **Selector-level filters** — `cluster`, `namespace`, `az`, `env` — SHALL be rendered into the upstream PromQL queries of the build as label matchers, so the graph is narrowed by VictoriaMetrics before any sample is read. Which matcher reaches which series is the hardcoded contract of the `cluster-topology-source` capability ("Request-scoped upstream selectors"); the service-graph series are deliberately read unfiltered (`pod-service-graph`). A request with no selector-level filter SHALL issue exactly the queries it issues today and produce a byte-identical body.
- **Projection-level filters** — `cluster` and `namespace` (applied again over the built graph as defence in depth), `edge_type`, and `prune` — SHALL be applied at response time as a projection over the freshly built graph.

Empty filters SHALL return the **connectivity-connected subgraph** of the full multi-cluster graph for the time window (the default connectivity prune — see "Default projection prunes connectivity-disconnected workload"); it is NOT the full topology inventory. `prune=false` returns the inventory instead.

Selector-level values SHALL be validated before rendering: a value longer than 253 bytes, containing a control character (including newline), or — for `prune` — not exactly `true` / `false`, SHALL be rejected with `400 Bad Request` and `reason: "invalid_scope"`. A single value SHALL render an exact matcher (`<key>="<value>"`, with `"` and `\` escaped); several values for one parameter SHALL render one fully-anchored alternation (`<key>=~"<v1>|<v2>"`) over the sorted, de-duplicated, regex-quoted values. The request value `cluster=unknown` SHALL render `cluster=~"unknown|"` so that both spellings of the bucket — a series carrying no `cluster` label and one whose label is literally `unknown` — remain addressable, matching what the projection filter accepts.

**Edge retention rule (unified across all filters).** An edge SHALL be retained when at least one resolved endpoint is in scope after node filtering. When exactly one endpoint is in scope, the missing endpoint SHALL be re-added from the freshly built graph's node index provided it passes the non-cluster filters (namespace check; types without a namespace label — `node`, `external`, and the NetApp types `netapp-aggr` / `netapp-node` (which carry neither a namespace nor a `cluster` label) — pass through). This single rule is edge-type-agnostic and covers non-pod endpoints incident on in-scope pods — including the topology `pod-to-node` edge, the `pvc-to-netapp-aggr` edge, and the `external` partners that a filtered build produces for out-of-scope peers — and, in an **unfiltered** build, cross-cluster edges whose partner endpoint lies outside a projection-level `cluster` set.

#### Scenario: Cluster filter narrows result

- **WHEN** the upstream holds pods in `cluster-alpha` and `cluster-beta` and a client sends `?cluster=cluster-alpha`
- **THEN** every topology query carrying a `cluster` label is issued with `cluster="cluster-alpha"`, the response contains pod nodes only for `cluster-alpha`, and any `cluster-beta` peer of a `cluster-alpha` pod appears as an `external/<label>` node (not as a `cluster-beta` pod node — see "Cross-cluster edge representation")

#### Scenario: Namespace filter combined with cluster

- **WHEN** a client sends `?cluster=cluster-alpha&namespace=ns-x&namespace=ns-y`
- **THEN** the pod- and claim-scoped topology queries are issued with `cluster="cluster-alpha",namespace=~"ns-x|ns-y"` and the response contains pods whose cluster is `cluster-alpha` AND whose namespace is `ns-x` OR `ns-y`

#### Scenario: Zone and environment filters

- **WHEN** a client sends `?az=zone-a&env=prod`
- **THEN** every topology query is issued with `<az-key>="zone-a",<env-key>="prod"` (the keys defaulting to `az` / `env`), the service-graph queries are issued unchanged, and the response contains only the workload and infrastructure whose series carry both labels

#### Scenario: Multi-valued zone filter renders one anchored alternation

- **WHEN** a client sends `?az=zone-b&az=zone-a&az=zone-a`
- **THEN** the rendered matcher is `<az-key>=~"zone-a|zone-b"` (sorted, de-duplicated, regex-quoted) and the result is the union of both zones

#### Scenario: Invalid selector value is rejected

- **WHEN** a client sends `?env=prod%0A` (a value containing a newline) or `?prune=maybe`
- **THEN** the server returns 400 Bad Request with `reason: "invalid_scope"` and issues no upstream query

#### Scenario: Edge-type filter with no matching edges

- **WHEN** a client sends `?edge_type=pod-calls-pod` and the time window contains no service-graph data
- **THEN** the response is 200 with `elements.edges: []` and no error

#### Scenario: Unknown cluster name

- **WHEN** a client sends `?cluster=does-not-exist`
- **THEN** the response is 200 with empty `elements.nodes`, empty `elements.edges`, and an empty `clusters` list

#### Scenario: Name filter matches a pod

- **WHEN** the freshly built graph contains pods named `frontend` and `backend` and a client sends `?name=frontend`
- **THEN** the `name` parameter is ignored (it is withdrawn) and the response is the default connectivity view, containing `backend` as well as `frontend`

#### Scenario: Name filter matches a K8s node

- **WHEN** a client sends `?name=worker-1`
- **THEN** the parameter is ignored; a podless node is surfaced with `?prune=false` (combined with `cluster` to bound the response), not by name

#### Scenario: Name filter matches a PVC

- **WHEN** a client sends `?name=checkout-data`
- **THEN** the parameter is ignored; an unmounted claim in namespace `shop` is surfaced with `?namespace=shop&prune=false`, not by name

#### Scenario: Name shared across types returns every match

- **WHEN** a pod and a K8s node both happen to be named `worker-1` and a client sends `?name=worker-1`
- **THEN** the parameter is ignored and the response is unaffected by the shared name

#### Scenario: Name shared across clusters returns every match

- **WHEN** a pod named `api` exists in both `cluster-alpha` and `cluster-beta` and a client sends `?name=api`
- **THEN** the parameter is ignored; a per-cluster view is obtained with `?cluster=cluster-alpha` or `?cluster=cluster-beta`

#### Scenario: Name filter combined with cluster

- **WHEN** a client sends `?name=api&cluster=cluster-alpha`
- **THEN** the `cluster` filter is applied at the source, the `name` parameter is ignored, and the response is identical to `?cluster=cluster-alpha`

#### Scenario: Name filter retains incident edges with re-hydrated partner

- **WHEN** a client sends `?name=frontend` for a window containing a cross-cluster `pod-calls-pod` edge
- **THEN** the parameter is ignored and the edge follows the "Cross-cluster edge representation" requirement for an unfiltered build (both real endpoints present)

#### Scenario: Unknown name returns empty result

- **WHEN** a client sends `?name=does-not-exist`
- **THEN** the parameter is ignored and the response is the full default connectivity view — NOT an empty result

#### Scenario: Withdrawn parameters are ignored

- **WHEN** a client sends `?name=frontend&root=cluster-alpha/abc&depth=1`
- **THEN** the server ignores the three parameters and returns the unanchored view for the remaining parameters (200), not a 400

#### Scenario: No selector-level filter issues today's queries

- **WHEN** a client sends `GET /v1/graph?start=...&end=...` with no other parameters
- **THEN** every upstream query is byte-identical to the query issued before request-scoped selectors existed, and the response body is byte-identical to the pre-change body for the same upstream data

### Requirement: Default projection prunes connectivity-disconnected workload

`GET /v1/graph` SHALL, on every request whose `prune` parameter is absent or `true`, return only the workload that participates in the connectivity graph. A `pod` node SHALL be retained iff it is an endpoint of at least one connectivity edge (`pod-calls-pod`, `pod-calls-service`, or `service-selects-pod`). A `pvc` node SHALL be retained iff at least one of the pods that mount it (`pod-mounts-pvc`) is itself retained by that rule; consequently a PVC with no mounting pod at all SHALL be dropped. A `node` (K8s host) and a `netapp-aggr` SHALL be retained iff referenced by a retained element (a pod scheduled on the node, a PVC joined to the aggregate via `pvc-to-netapp-aggr`), and a `netapp-node` iff referenced by a retained `netapp-aggr` (its `labels.node`) — the reference-driven infra-admission rule, operating (transitively for the NetApp chain) over the connectivity-pruned pod/PVC set. `service` and `external` nodes are connectivity-born (only ever materialised as edge endpoints) and SHALL NOT be pruned by this rule. The prune SHALL be a pure function of the built graph, applied uniformly for every selector-level filter shape, and SHALL NOT resurrect a pruned pod/PVC through the edge-retention partner re-add. Because the service-graph series are read in full, an in-scope pod whose only traffic goes to out-of-scope peers still sits on a connectivity edge (to the `external` partner a filtered build materialises for such a peer) and is therefore retained.

`prune=false` SHALL turn the prune off: every loaded pod is emitted together with its `pod-to-node`, `pod-mounts-pvc`, and `pvc-to-netapp-aggr` chain regardless of traffic, and every loaded PVC is emitted whether or not it is mounted. `prune=false` is the only escape hatch; the former `name` / `root` escape hatches are withdrawn. Its effect on unreferenced infrastructure nodes is specified in "Namespace-filter retention of cluster-scoped infra nodes".

#### Scenario: Edgeless pod and its dependents are pruned from the default view

- **WHEN** the freshly built graph contains a pod `idle` that is on no connectivity edge (only a `pod-to-node` edge to host `worker-9` and a `pod-mounts-pvc` edge to PVC `idle-data`, where `worker-9` and `idle-data` are referenced by nothing else) and a client sends no filters
- **THEN** the response omits the `idle` pod, the `worker-9` node, the `idle-data` PVC, any NetApp aggregate serving only `idle-data`, and any NetApp node referenced only by such aggregates

#### Scenario: Connectivity-connected workload is retained with its infra

- **WHEN** a pod `web` is an endpoint of a `pod-calls-pod` edge, is scheduled on `worker-0`, and mounts PVC `web-data` whose claim joined aggregate `netapp/ontap-prod/aggr/aggr1` owned by controller `ontap-prod-01`, and a client sends no filters
- **THEN** the response contains `web`, `worker-0`, `web-data`, `netapp/ontap-prod/aggr/aggr1`, and `netapp/ontap-prod/ontap-prod-01`

#### Scenario: prune=false surfaces an otherwise-pruned edgeless pod with its storage chain

- **WHEN** the freshly built graph contains a connectivity-disconnected pod `idle` scheduled on `worker-9` and mounting `idle-data` (joined to `netapp/ontap-prod/aggr/aggr2` owned by `ontap-prod-02`), and a client sends `?prune=false`
- **THEN** the response contains `idle`, `worker-9`, `idle-data`, `netapp/ontap-prod/aggr/aggr2`, `netapp/ontap-prod/ontap-prod-02`, and the `pod-to-node`, `pod-mounts-pvc`, and `pvc-to-netapp-aggr` edges between them

#### Scenario: Namespace filter still prunes edgeless workload

- **WHEN** a namespace `shop` contains both a connectivity-connected pod `web` and an edgeless pod `idle`, and a client sends `?namespace=shop`
- **THEN** the response contains `web` but omits `idle`

#### Scenario: Name filter surfaces an otherwise-pruned edgeless pod

- **WHEN** the freshly built graph contains a connectivity-disconnected pod `idle` and a client sends `?name=idle`
- **THEN** the withdrawn `name` parameter is ignored and `idle` stays pruned; it is surfaced with `?prune=false`

#### Scenario: Namespace storage topology with prune=false

- **WHEN** namespace `shop` contains pods `web` (connected) and `idle` (edgeless), each scheduled on a node and mounting a claim joined to a NetApp aggregate, and a client sends `?namespace=shop&prune=false`
- **THEN** the response contains both pods, both host nodes, both claims, every aggregate those claims join, and the owning controllers — and nothing from any other namespace except the `external` partners of `shop`'s traffic

#### Scenario: In-scope pod with only out-of-scope traffic survives the prune

- **WHEN** pod `web` in namespace `shop` calls only pods in namespace `payments`, and a client sends `?namespace=shop`
- **THEN** `web` is retained (its edge to the `external/<payments-peer-label>` partner is a connectivity edge) and the response contains `web`, that `external` node, and the edge

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

### Requirement: Cross-cluster edge representation

A cross-cluster edge (`pod-calls-pod`, `pod-calls-service`, or `service-selects-pod` whose source-node cluster differs from its target-node cluster) SHALL be emitted with **both real endpoint nodes** only when both clusters are **loaded** by the build — every build without a `cluster` filter, or a build whose `cluster` filter lists both clusters. Consumers detect cross-cluster status by comparing the `labels.cluster` of the edge's resolved source and target nodes — not from edge labels. A `pod-calls-pod` edge carries `labels.cluster` (the trace source / client-side cluster, present iff the client side resolved to a pod); a `service-selects-pod` edge carries no `cluster` key (its source is a service node, which is cluster-scoped via its own `labels.cluster`).

When a `cluster` filter excludes one side, that side's topology is not loaded; the peer then follows the `pod-service-graph` capability's filtered-build rule and is rendered as an `external/<label>` node (carrying no `cluster`), and the edge is no longer cross-cluster — it is an edge to an external. The family-wide `service-selects-pod` fan-out of a local service node reaches only backing pods in **loaded** clusters. The former rule that a `?cluster=` projection keeps the out-of-scope partner as a real pod node is withdrawn.

#### Scenario: Cross-cluster edge with both clusters in scope

- **WHEN** a client requests `?cluster=cluster-alpha&cluster=cluster-beta` for a window containing a cross-cluster `pod-calls-pod` edge whose client pod is in `cluster-alpha` and server pod is in `cluster-beta`
- **THEN** the response contains both endpoint pod nodes and one edge with `labels.cluster: "cluster-alpha"`, where the source node's `labels.cluster` is `"cluster-alpha"` and the target node's `labels.cluster` is `"cluster-beta"`

#### Scenario: Cross-cluster edge with one cluster in scope

- **WHEN** a client requests `?cluster=cluster-alpha` and the service-graph series records a call from a pod in `cluster-alpha` to a pod in `cluster-beta` whose `server` label is `cart`
- **THEN** the response contains the `cluster-alpha` endpoint, an `external/cart` node with `labels={}`, and one `pod-calls-pod` edge from the pod to `external/cart` with `labels.cluster: "cluster-alpha"`; no `cluster-beta` pod node is present

#### Scenario: Cross-cluster service-selects-pod edge from the local service node's endpoint union

- **WHEN** clusters `prod-1` and `prod-2` (family `prod-0`) both hold a `payments` service in namespace `payments-ns`, a pod in `prod-1` emits a `"://"` connection string addressing it, and a client requests `?cluster=prod-1&cluster=prod-2`
- **THEN** the response contains the single `pod-calls-service` edge from the `prod-1` pod to the `prod-1/payments-ns/payments` service node plus `service-selects-pod` edges to the backing pods of **both** clusters (the `prod-2` targets being cross-cluster, detected by comparing the endpoint nodes' `labels.cluster`); whereas a `?cluster=prod-1` request yields the same service node with `service-selects-pod` edges to `prod-1`'s backing pods only

### Requirement: Deterministic response body

For identical input — same `(window, filters, upstream-data)` — the server SHALL produce a byte-identical response body across rebuilds. The server SHALL NOT emit any HTTP cache validator (no `ETag`, no `Last-Modified`): cacheability is intentionally a future-iteration concern and v1 has no in-process result cache. A future cache is keyed by `(window, az, env, cluster-set, namespace-set)`; within one such key the projection-level filters remain a pure function of the built graph.

The serialiser SHALL maintain determinism by sorting `view.Nodes` and `view.Edges`, sorting `Graph.ClusterNames()`, sorting `IPAddress` slices at construction, and keeping the response body shape fixed at `{apiVersion, clusters, elements}` for graph routes (no time-of-build or echo-of-input fields). Every rendered upstream selector SHALL be a pure function of the sorted, de-duplicated parameter values.

`GET /v1/edge-types`, `GET /openapi.yaml`, `GET /openapi.json`, and `GET /docs` SHALL carry an explicit `Cache-Control` header. `GET /v1/graph` SHALL NOT emit a `Cache-Control` header.

#### Scenario: Body byte-identical across repeated requests

- **WHEN** a client sends two consecutive `GET /v1/graph` requests with identical query parameters and the upstream data has not changed between them
- **THEN** both response bodies are byte-identical, even though each request triggered an independent upstream fan-out

#### Scenario: Parameter order does not change the body

- **WHEN** a client sends `?az=b&az=a&namespace=y&namespace=x` and then `?namespace=x&namespace=y&az=a&az=b` for the same window and upstream data
- **THEN** both requests render identical upstream selectors and return byte-identical bodies

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

The server SHALL expose `GET /livez` that returns 200 while the process is running, and `GET /readyz` that returns 200 only when a 1-second `up{}` probe against **every** backend in the live routing table succeeds. `GET /readyz` SHALL return 503 otherwise, with a JSON body carrying a `reason` field that names the backends that did not answer. In a deployment with no routing table configured, the single implicit `default` backend is the only one probed, so the observable behaviour is unchanged.

The probes SHALL be issued concurrently and SHALL share the single 1-second budget, so readiness latency does not grow with the number of backends.

#### Scenario: livez always healthy while running

- **WHEN** a client sends `GET /livez`
- **THEN** the response is 200 with body `"ok"` regardless of upstream state

#### Scenario: readyz fails when upstream unreachable

- **WHEN** the configured VictoriaMetrics URL refuses connections and a client sends `GET /readyz`
- **THEN** the response is 503 with a JSON body containing a `reason` field

#### Scenario: readyz fails when one of several backends is unreachable

- **WHEN** three backends are configured, two answer and one refuses connections, and a client sends `GET /readyz`
- **THEN** the response is 503 and the `reason` field names the refusing backend

#### Scenario: readyz succeeds when every backend answers

- **WHEN** every configured backend answers the probe within the budget
- **THEN** the response is 200

### Requirement: Self-metrics endpoint

The server SHALL expose `GET /metrics` in Prometheus exposition format including at least: `kube_state_graph_build_duration_seconds`, `kube_state_graph_project_duration_seconds`, `kube_state_graph_serialise_duration_seconds`, `kube_state_graph_build_rejected_total`, `kube_state_graph_graph_node_count`, `kube_state_graph_graph_edge_count`, `kube_state_graph_clusters_observed`, `kube_state_graph_upstream_query_duration_seconds`, `kube_state_graph_upstream_query_failures_total`, `kube_state_graph_http_requests_total`, `kube_state_graph_auth_rejected_total`, `kube_state_graph_upstream_backends`, `kube_state_graph_backend_config_reload_total`, and `kube_state_graph_backend_query_failures_total`.

`kube_state_graph_upstream_query_duration_seconds` and `kube_state_graph_upstream_query_failures_total` SHALL keep exactly the label sets they carried before backend routing existed: adding a label to an existing self-metric is a contract change, so per-backend detail is carried by `kube_state_graph_backend_query_failures_total` instead.

#### Scenario: Metrics exposition

- **WHEN** a client sends `GET /metrics`
- **THEN** the response is 200 in `text/plain; version=0.0.4` exposition format and includes all metric names listed above

#### Scenario: cluster label on observational gauges

- **WHEN** a build has produced a multi-cluster graph
- **THEN** `kube_state_graph_graph_node_count` series include a `cluster` label and `kube_state_graph_graph_edge_count` series include a `cross_cluster` label

#### Scenario: Backend metrics carry the backend label

- **WHEN** a query to backend `zone-b` fails and a client scrapes `/metrics`
- **THEN** `kube_state_graph_backend_query_failures_total` carries a series labelled with `zone-b`, and `kube_state_graph_upstream_query_failures_total` carries no backend label

#### Scenario: Backend gauge present with no routing table

- **WHEN** the server runs with only `--prom-url` configured
- **THEN** `kube_state_graph_upstream_backends` reads 1

### Requirement: Per-build timeout (graph endpoints)

For `GET /v1/graph`, the server SHALL apply a configurable per-build `context.WithTimeout` derived from `--build-timeout` (default 15 seconds). On `context.DeadlineExceeded`, the build SHALL be aborted, the `kube_state_graph_build_rejected_total{reason="timeout"}` counter SHALL be incremented, and the request SHALL receive `504 Gateway Timeout` with `reason: "timeout"` (RFC 9110 §15.6.5: gateway did not receive a timely response from an upstream server it needed to access in order to complete the request).

#### Scenario: Upstream stalls beyond build timeout

- **WHEN** centralised VictoriaMetrics fails to respond to a `/v1/graph` build within `--build-timeout`
- **THEN** the request returns 504 with `reason: "timeout"`

### Requirement: Per-request timeout (non-graph endpoints)

For non-graph endpoints that perform upstream calls (`GET /readyz` `up{}` probe), the server SHALL apply a `context.WithTimeout` derived from `--api-timeout` (default 5 seconds) to the upstream call. On `context.DeadlineExceeded`, the request SHALL receive `504 Gateway Timeout` with `reason: "timeout"`. The same timeout bounds the build's `up{}` retention probe. Endpoints that do not perform upstream calls (`GET /v1/edge-types`, `GET /livez`, `GET /metrics`, `GET /openapi.*`, `GET /docs*`) are not subject to this timeout.

#### Scenario: Readiness probe stalls beyond api timeout

- **WHEN** centralised VictoriaMetrics fails to respond to the `/readyz` `up{}` probe within `--api-timeout`
- **THEN** the request returns 504 with `reason: "timeout"`

#### Scenario: Cluster discovery stalls beyond api timeout

- **WHEN** a client sends `GET /v1/clusters` while centralised VictoriaMetrics is unresponsive
- **THEN** the request returns 404 Not Found immediately — the endpoint is removed, no upstream call is made, and the api timeout does not apply

### Requirement: Outside-retention error

When a build carrying **no selector-level filter** finds zero pods and zero nodes for the requested window but the upstream VictoriaMetrics is reachable (a parallel `up{}` probe succeeds), the server SHALL respond `400 Bad Request` with `reason: "outside_retention"`. When any selector-level filter (`cluster`, `namespace`, `az`, `env`) is active, zero rows means "nothing in scope": the classification SHALL NOT run, no `up{}` probe SHALL be issued for it, and the server SHALL respond `200` with empty `elements.nodes`, empty `elements.edges`, and an empty `clusters` list.

#### Scenario: Window beyond retention

- **WHEN** a client requests a window older than upstream `kube_pod_info` retention with no filter, and `up{}` returns 1
- **THEN** the response is 400 with `reason: "outside_retention"`

#### Scenario: Filtered request with no matching data

- **WHEN** a client requests `?env=staging` for a window in which no series carries `env="staging"`, and `up{}` would return 1
- **THEN** the response is 200 with empty `elements.nodes`, empty `elements.edges`, and `clusters: []`; no retention probe is issued

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

### Requirement: Pod `application` and `containers` attributes

Every `data` object for a `type="pod"`, `type="service"`, or `type="pvc"` node SHALL be able to expose an `application` attribute, and every `type="pod"` node SHALL additionally be able to expose a `containers` attribute, all with `omitempty` semantics and all **outside `labels`** (which stays a strict `map[string]string`):

- `application` — a `string`, the node's ArgoCD Application name as resolved by the
  `cluster-topology-source` capability from the `annotation_argocd_argoproj_io_tracking_id`
  label that kube-state-metrics derives from the `argocd.argoproj.io/tracking-id`
  annotation: for `type="pod"` from the annotation series of the pod's **controller**
  (`kube_deployment_annotations`, `kube_statefulset_annotations`,
  `kube_daemonset_annotations`, `kube_replicaset_annotations`,
  `kube_job_annotations`, `kube_cronjob_annotations` — see "Pod ArgoCD Application
  attribute"), and for `type="service"` / `type="pvc"` from
  `kube_service_annotations` / `kube_persistentvolumeclaim_annotations` (see "Service
  and PVC ArgoCD Application resolution"). All three node types therefore derive the
  value from the same annotation and the same `<app>:<group>/<kind>:<ns>/<name>` parse.
  Emitted only when the node has a resolved Application; omitted entirely otherwise
  (never an empty string). This attribute is **complementary** to the synthesised
  `type="application"` group node (which is derived from this same value — see
  "Cytoscape compound node grouping"); an existing consumer reading `data.application`
  on a pod is unaffected in shape, though a pod whose Application previously came from
  a non-standard pod-level `argocd_tracking_id` label now resolves it from the
  controller instead.
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

- **WHEN** the response contains a pod node whose resolved controller is a Deployment carrying `annotation_argocd_argoproj_io_tracking_id` resolving to Application `checkout`
- **THEN** the corresponding `type="pod"` node carries `data.application: "checkout"` and `data.labels` contains no `annotation_argocd_argoproj_io_tracking_id` / `argocd_tracking_id` / `application` key

#### Scenario: Pod node omits application when its controller has no annotation

- **WHEN** the response contains a pod node whose resolved controller carries no `argocd.argoproj.io/tracking-id` annotation — including a pod whose own `kube_pod_owner` series carried a non-standard `argocd_tracking_id` label
- **THEN** the corresponding `type="pod"` node's `data` object includes no `application` field, and the pod nests under its `controller` group with no `application` group between it and its namespace

#### Scenario: Service node carries application when resolved

- **WHEN** the response contains a service node whose `kube_service_annotations` series carried `annotation_argocd_argoproj_io_tracking_id` resolving to Application `checkout`
- **THEN** the corresponding `type="service"` node carries `data.application: "checkout"` and `data.labels` contains no `annotation_argocd_argoproj_io_tracking_id` / `application` key

#### Scenario: PVC node carries application when resolved

- **WHEN** the response contains a PVC node whose `kube_persistentvolumeclaim_annotations` series carried `annotation_argocd_argoproj_io_tracking_id` resolving to Application `mongo`
- **THEN** the corresponding `type="pvc"` node carries `data.application: "mongo"` and `data.labels` contains no `annotation_argocd_argoproj_io_tracking_id` / `application` key

#### Scenario: PVC node carries inherited application from a mounting pod

- **WHEN** the response contains a PVC node that has no own `annotation_argocd_argoproj_io_tracking_id` annotation but is mounted (via a `pod-mounts-pvc` edge) by a pod whose controller resolves ArgoCD Application `checkout` (see cluster-topology-source "PVC ArgoCD Application inheritance from mounting pod")
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

### Requirement: Namespace-filter retention of cluster-scoped infra nodes

`GET /v1/graph` projection SHALL treat `type="node"`, `type="netapp-aggr"`, and `type="netapp-node"` nodes as infrastructure nodes that carry no `namespace` label, and SHALL admit such a node to a response **iff it is referenced by an in-scope element** — a `type="node"` node when some in-scope pod is scheduled on it (its `labels.node`), a `type="netapp-aggr"` node when some in-scope PVC is joined to it via a `pvc-to-netapp-aggr` edge, and a `type="netapp-node"` node when some admitted `netapp-aggr` names it as owner (its `labels.node`) — a **transitive** reference chain PVC → aggregate → controller — on every request shape. The default response therefore lists only the host nodes of pods that are in the graph, the aggregates serving in-scope PVCs, and the controllers owning those aggregates; it SHALL NOT carry an orphan K8s node that hosts no pod, an aggregate serving no in-scope PVC, or a controller owning no admitted aggregate.

The `cluster` filter applies to `type="node"` at the source (the `kube_node_*` series are cluster-filtered) and again at projection (the node's own `labels.cluster`). The NetApp types carry NO Kubernetes `cluster` label and their Harvest series receive no `cluster` or `namespace` matcher, so a `?cluster=` or `?namespace=` filter SHALL NEVER admit or exclude them directly — their admission is purely reference-driven, which means a filer shared by two Kubernetes clusters appears in a `?cluster=` view of either cluster (via that cluster's in-scope PVCs).

`prune=false` SHALL lift the reference requirement for an infrastructure node exactly when no active filter could have excluded that node by its own labels: a `type="node"` node is admitted unreferenced when no `namespace` filter is active (a `cluster` / `az` / `env` filter has already been applied to its own series); a `type="netapp-aggr"` or `type="netapp-node"` node is admitted unreferenced when neither a `cluster` nor a `namespace` filter is active (an `az` / `env` filter has already been applied to its own series). Under a `namespace` filter, `prune=false` therefore still yields only the namespace's referenced infrastructure. An unreferenced `netapp-aggr` admitted this way SHALL pull in its owning `netapp-node` (the compound parent must exist in the response). The former `?name=` exception is withdrawn with the parameter.

A **consequence** of this rule is that a podless K8s node's `ready_status` / `ipaddress` is absent from the default view and is obtained with `?prune=false` (optionally combined with `cluster`, `az`, `env`); there is no exception that keeps an unhealthy (`NotReady` / `Unknown`) podless node — or a `degraded` aggregate/controller serving no in-scope PVC — in the default view.

#### Scenario: Default view drops a podless node

- **WHEN** the built graph has a node `cluster-alpha/worker-9` on which no pod is scheduled and a client sends `GET /v1/graph` with no filter
- **THEN** the response does not contain `cluster-alpha/worker-9`

#### Scenario: Default view keeps a node hosting an in-graph pod

- **WHEN** a pod is scheduled on node `cluster-alpha/worker-0` and a client sends `GET /v1/graph` with no filter
- **THEN** the response contains `cluster-alpha/worker-0` and the `pod-to-node` edge from the pod to it

#### Scenario: StorageClass retained when a filtered-in PVC references it

- **WHEN** the graph has a PVC in namespace `shop` whose claim joined aggregate `netapp/ontap-prod/aggr/aggr1` owned by `ontap-prod-01`, and a client sends `?namespace=shop`
- **THEN** the response contains the `shop` PVC, the `netapp/ontap-prod/aggr/aggr1` node, its `pvc-to-netapp-aggr` edge, and the owning `netapp/ontap-prod/ontap-prod-01` node (the aggregate's compound parent)

#### Scenario: Default view drops a PVC-less StorageClass

- **WHEN** a client sends `GET /v1/graph` with any filter shape
- **THEN** the response never contains a `type="storageclass"` node — the type is removed; the reference-driven admission governs the `netapp-aggr` / `netapp-node` chain instead

#### Scenario: Name filter on an unused StorageClass surfaces it

- **WHEN** a client sends `?name=gp3` where `gp3` was formerly a StorageClass name
- **THEN** the withdrawn `name` parameter is ignored and the response is the default view; StorageClass nodes no longer exist, and unreferenced infrastructure is surfaced with `?prune=false`

#### Scenario: Name filter surfaces an unreferenced infra node

- **WHEN** node `cluster-alpha/worker-9` hosts no pod and a client sends `?name=worker-9`
- **THEN** the withdrawn `name` parameter is ignored and `cluster-alpha/worker-9` is not admitted; `?cluster=cluster-alpha&prune=false` admits it (with its `ready_status` / `ipaddress` when resolved)

#### Scenario: Name filter surfaces a NetApp aggregate directly

- **WHEN** aggregate `netapp/ontap-prod/aggr/aggr1` serves no in-scope PVC and a client sends `?name=aggr1`
- **THEN** the withdrawn `name` parameter is ignored and the aggregate is not admitted; `?prune=false` with no `cluster` / `namespace` filter admits it (with its `health` / `usage` when resolved) together with its owning `netapp-node`

#### Scenario: Name filter surfaces a NetApp node directly

- **WHEN** NetApp node `netapp/ontap-prod/ontap-prod-01` owns no admitted aggregate and a client sends `?name=ontap-prod-01`
- **THEN** the withdrawn `name` parameter is ignored and the node is not admitted; `?prune=false` with no `cluster` / `namespace` filter admits it with its `health` attribute when resolved

#### Scenario: Shared filer visible from either cluster's filtered view

- **WHEN** PVCs in `cluster-alpha` and `cluster-beta` both join `netapp/ontap-prod/aggr/aggr1` and a client sends `?cluster=cluster-alpha`
- **THEN** the response contains `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` (referenced by `cluster-alpha`'s in-scope PVC), and a `?cluster=cluster-beta` request equally contains them

#### Scenario: Cluster filter keeps only referenced infra nodes

- **WHEN** `?cluster=cluster-alpha` is sent and `cluster-alpha` has a node `worker-0` hosting a pod and a node `worker-1` hosting nothing
- **THEN** the response contains `cluster-alpha/worker-0` and not `cluster-alpha/worker-1`

#### Scenario: K8s node retained when a filtered-in pod is scheduled on it

- **WHEN** a pod in namespace `shop` is scheduled on node `cluster-alpha/worker-0` and a client sends `?namespace=shop`
- **THEN** the response contains node `cluster-alpha/worker-0` and the `pod-to-node` edge from the pod to it

#### Scenario: Podless NotReady node is hidden by default (no health exception)

- **WHEN** node `cluster-alpha/worker-broken` hosts no pod and its `ready_status` is `NotReady` and a client sends `GET /v1/graph` with no filter
- **THEN** the response does not contain `cluster-alpha/worker-broken` (it is obtained with `?prune=false`)

#### Scenario: prune=false alone is the full inventory

- **WHEN** node `cluster-alpha/worker-9` hosts no pod, aggregate `netapp/ontap-prod/aggr/spare` (owned by `ontap-prod-02`) serves no claim, and a client sends `?prune=false`
- **THEN** the response contains `cluster-alpha/worker-9` (with its `ready_status` / `ipaddress` when resolved), `netapp/ontap-prod/aggr/spare` (with its `health` / `usage` when resolved), and `netapp/ontap-prod/ontap-prod-02`

#### Scenario: prune=false under a cluster filter admits that cluster's podless nodes but not unreferenced aggregates

- **WHEN** a client sends `?cluster=cluster-alpha&prune=false`, `cluster-alpha/worker-9` hosts no pod, and `netapp/ontap-prod/aggr/spare` serves no claim
- **THEN** the response contains `cluster-alpha/worker-9` but not `netapp/ontap-prod/aggr/spare`

#### Scenario: prune=false under a namespace filter stays reference-driven

- **WHEN** a client sends `?namespace=shop&prune=false`, `cluster-alpha/worker-9` hosts no `shop` pod, and `netapp/ontap-prod/aggr/spare` serves no `shop` claim
- **THEN** the response contains neither `cluster-alpha/worker-9` nor `netapp/ontap-prod/aggr/spare`

### Requirement: PVC `volumename` and `svm` labels

A `type="pvc"` node's `data.labels` SHALL additively carry two further string entries whenever they resolve:

- `volumename` — the name of the PersistentVolume bound to the claim (from the `volumename` label of `kube_persistentvolumeclaim_info`, per the `cluster-topology-source` capability).
- `svm` — the NetApp ONTAP SVM serving the claim (from the `svm` label of the Harvest `volume_labels` series matched by the `netapp-storage-graph` capability's PV-name join — the removed Trident custom-resource chain is no longer the source, with the label's shape unchanged).

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

### Requirement: Edge `metrics` attribute

An edge's `data` MAY carry an optional `metrics` object (`omitempty`) holding the edge's measurements for the requested window. The key SHALL be **absent entirely** — never `null`, never an empty object — on every edge that has no measurements. The object is a **union of two disjoint families**, and a single edge SHALL only ever carry fields from ONE family (the family is determined by the edge's provenance, so cross-family mixing is structurally impossible):

- **RED family** — on trace-derived call edges, presence rule defined by the `pod-service-graph` capability (in short: only trace-derived edges whose `source` and `target` both resolved to a pod or a service node, excluding the ingress chain's entry hop and any measurement derived from span-link series):
  - `rate` (number, REQUIRED **within this family**, strictly greater than zero) — requests per second over the window.
  - `error_rate` (number, OPTIONAL, absence semantics) — the failed fraction in `[0, 1]`. Absent when the upstream failure counter could not be read; `0` when it was read and reported no failures.
  - `p90_server_ms` (number, OPTIONAL, absent when unavailable) — the 90th percentile server-observed request duration in milliseconds. The quantile and observation side match Grafana's documented service-graph queries by definition; the values are not expected to equal Grafana's numerically, because Grafana aggregates by service name while this API aggregates by pod pair.
- **I/O family** — on `pvc-to-netapp-aggr` edges only, presence rule defined by the `netapp-storage-graph` capability (each field present iff its own Harvest family matched; the six measurements come from the Harvest QoS workload families, the two ceilings from the QoS fixed-policy families):
  - `read_ops`, `write_ops` (numbers, OPTIONAL) — read/write requests per second, verbatim from Harvest.
  - `read_latency_us`, `write_latency_us` (numbers, OPTIONAL) — average read/write latency in microseconds, verbatim from Harvest.
  - `read_bytes_per_sec`, `write_bytes_per_sec` (numbers, OPTIONAL) — read/write throughput in bytes per second, verbatim from Harvest.
  - `max_iops`, `max_bytes_per_sec` (numbers, OPTIONAL) — the volume's declared QoS throughput ceiling: `max_iops` verbatim from Harvest, `max_bytes_per_sec` converted from the policy's megabytes-per-second figure so that it carries the same unit as the measured throughput fields and the two compare directly. Absence means *no declared ceiling* — it SHALL NEVER be rendered as `0` or as an "unlimited" sentinel — and neither ceiling field can appear unless at least one measurement field does.

At the schema level every field of the union is therefore optional — a consequence the OpenAPI schema reflects by moving `rate` from required to optional. The RED invariant is preserved intact: a RED-family `metrics` object always carries a positive `rate`.

All values SHALL be JSON numbers, never strings. Each value SHALL be rounded to a fixed number of **significant digits** — not decimal places — so that the "Deterministic response body" requirement continues to hold byte-for-byte while a non-zero value can never be rendered as `0`. Consequently a value MAY appear in JSON exponent form (for example `3.86e-7`), which is legal JSON; consumers MUST NOT assume a fixed-decimal rendering, and MUST treat `0` as semantically distinct from a very small non-zero value. The presence or absence of `metrics` SHALL NOT affect the edge's `id`, `type`, `source`, `target`, or `labels`, and SHALL NOT affect node or edge ordering.

#### Scenario: Pod-to-pod edge carries RED metrics

- **WHEN** the response contains a `pod-calls-pod` edge whose `source` and `target` are both pod nodes and whose upstream series carried request, failure, and duration data
- **THEN** its `data.metrics` is an object with numeric `rate`, `error_rate`, and `p90_server_ms` fields and no I/O-family field

#### Scenario: Pod-to-service edge carries RED metrics

- **WHEN** the response contains a `pod-calls-service` edge produced by a contributing service-graph series (a connection string, a peer address matched to a Service, or a route-engine resolution to a backend Service)
- **THEN** its `data.metrics` is present and follows the same shape as a pod-to-pod edge's

#### Scenario: Storage edge carries I/O metrics only

- **WHEN** the response contains a `pvc-to-netapp-aggr` edge whose joined Harvest families all matched and whose volume is governed by a fixed QoS policy
- **THEN** its `data.metrics` is an object with numeric `read_ops`, `write_ops`, `read_latency_us`, `write_latency_us`, `read_bytes_per_sec`, `write_bytes_per_sec`, `max_iops`, and `max_bytes_per_sec` fields, and none of `rate`, `error_rate`, or `p90_server_ms`

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

### Requirement: Availability-zone and environment selector filters

`GET /v1/graph` SHALL accept the optional, repeatable parameters `az` and `env`. Each SHALL be rendered as an upstream label matcher on every kube-state-metrics and kubelet query of the build, on **no** NetApp Harvest query, and on no service-graph query; the `up{}` probe SHALL never carry them. `az` additionally selects which `harvest` backends are asked (the `upstream-backend-routing` capability's zone rule); that selection is the only effect `az` has on the Harvest legs, and `env` has none. The upstream label each parameter binds to is the operator-configured key of the `cluster-topology-source` capability ("Configurable `az` / `env` label keys"), defaulting to `az` and `env`; the request parameter names themselves are fixed.

The two filters narrow **at the source**: a series that lacks the configured label does not match an equality matcher and is therefore absent from the build. The operator SHALL ensure every kube-state-metrics and kubelet family stamps both labels; a family that does not vanishes from every `az` / `env`-filtered request, and because the default projection keeps only connectivity-connected workload, a topology family missing the label yields an empty graph for that filter rather than a partial one. The Harvest families are exempt: they carry no matcher, so they need no label. The response `clusters` list, derived from the built graph's node `cluster` labels, SHALL therefore list only the clusters with data in the requested zone / environment.

#### Scenario: Environment filter selects one environment's clusters

- **WHEN** the upstream holds `cluster-prod-1` (all series `env="prod"`) and `cluster-dev-1` (all series `env="dev"`) and a client sends `?env=prod`
- **THEN** every kube-state-metrics and kubelet query carries `env="prod"`, the response contains only `cluster-prod-1` workload and infrastructure, and `clusters` is `["cluster-prod-1"]`

#### Scenario: Zone and environment are AND-combined

- **WHEN** `cluster-a` carries `az="zone-a",env="prod"`, `cluster-b` carries `az="zone-b",env="prod"`, and a client sends `?az=zone-a&env=prod`
- **THEN** the response contains `cluster-a` only

#### Scenario: Configured key is used in the matcher

- **WHEN** the server runs with `KSG_AZ_LABEL=topology_zone` and a client sends `?az=zone-a`
- **THEN** the rendered matcher is `topology_zone="zone-a"`, and the request parameter is still named `az`

#### Scenario: Family lacking the label vanishes under the filter

- **WHEN** the kube-state-metrics series carry `env="prod"` but the kubelet series carry no `env` label, and a client sends `?env=prod`
- **THEN** the response contains the prod pods, nodes, and claims but no claim carries kubelet usage (the kubelet legs returned nothing), and the build does not fail

#### Scenario: Harvest lacking the label still joins under the filter

- **WHEN** the kube-state-metrics and kubelet series carry `env="prod"`, the Harvest series carry no `env` label, and a client sends `?env=prod`
- **THEN** the Harvest legs are issued without an `env` matcher and return their rows, so the prod claims that join a `volume_labels` series receive their `netapp-aggr` / `netapp-node` nodes and `pvc-to-netapp-aggr` edges

