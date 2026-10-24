# graph-api Specification

## Purpose
Define the versioned HTTP surface of the graph API: the time-ranged `GET /v1/graph` endpoint, its filter parameters and default projection, the Cytoscape.js wire shape of every node, edge and typed attribute, and the operational endpoints (health, self-metrics, OpenAPI, docs) and cross-cutting rules (authentication, timeouts, determinism) the server applies.

## Requirements

### Requirement: Versioned route prefix

The HTTP API SHALL expose every endpoint under the `/v1/` route prefix and SHALL include `apiVersion: "v1"` as a top-level field in every JSON response body.

#### Scenario: Body carries apiVersion

- **WHEN** a client sends `GET /v1/graph?start=...&end=...`
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

The server SHALL derive the upstream evaluation window from the caller's `start` and `end` after enforcing `end > start`, evaluating upstream PromQL with `<window> = end - start` at the (possibly aligned) `end`. End alignment SHALL be a pure function of `(start, end, grid)` that runs after validation and preserves the window length, and the response body SHALL NOT echo `start`, `end`, or any derived timestamp, aligned or not.

#### Scenario: End-alignment grid configuration

- **WHEN** the end-alignment grid (`--end-align` / `KSG_END_ALIGN`, default `30s`) is non-zero
- **THEN** the server floors `end` to the largest multiple of the grid (measured from the Unix epoch) that is not after it
- **AND** shifts `start` earlier by the same amount, so the window length `end - start` is unchanged

#### Scenario: Zero grid passes timestamps through

- **WHEN** the grid is `0`
- **THEN** `start` and `end` are passed through verbatim

#### Scenario: Alignment runs after validation

- **WHEN** a request is valid before alignment
- **THEN** it stays valid after alignment, and every validation error is reported against the caller's own values

#### Scenario: No window cap or future-time guard

- **WHEN** a client sends an arbitrarily long window or an `end` in the future
- **THEN** the server applies no window cap and no future-time guard
- **AND** operators relying on bounded query cost configure upstream VictoriaMetrics search limits (e.g. `-search.maxQueryDuration`, `-search.maxPointsPerTimeseries`)

#### Scenario: Caller timestamps drive PromQL

- **WHEN** the server runs with `--end-align=0` and a client sends `GET /v1/graph?start=2026-05-02T12:04:17Z&end=2026-05-02T12:19:30Z`
- **THEN** the upstream PromQL is evaluated with `<window> = end - start` and `<end> = 2026-05-02T12:19:30Z`, and the response body contains only `apiVersion`, `clusters`, and `elements`

#### Scenario: End floored to the grid, window length kept

- **WHEN** the server runs with `--end-align=30s` and a client sends `GET /v1/graph?start=2026-05-02T12:04:17Z&end=2026-05-02T12:19:47Z`
- **THEN** the upstream PromQL is evaluated at `<end> = 2026-05-02T12:19:30Z` with `<window> = 15m30s`

#### Scenario: Requests within one grid step render identical queries

- **WHEN** the server runs with `--end-align=30s` and two clients send the same parameters except `end=12:19:31Z` and `end=12:19:59Z` with equal window lengths
- **THEN** both builds render byte-identical upstream queries evaluated at `12:19:30Z`, and with unchanged upstream data return byte-identical bodies

#### Scenario: Validation uses the caller's values

- **WHEN** the server runs with `--end-align=30s` and a client sends `start=12:19:40Z&end=12:19:50Z`
- **THEN** the request is valid (the caller's `end > start`), and is evaluated over a 10s window ending at `12:19:30Z`

### Requirement: Cytoscape.js response shape

`GET /v1/graph` SHALL return a JSON document in Cytoscape.js shape: `{ apiVersion, clusters, elements: { nodes, edges } }`. The body SHALL NOT contain time-varying or echo-of-input fields, so identical inputs against the same upstream state produce byte-identical bodies.

#### Scenario: Pod node payload

- **WHEN** the response contains a pod node
- **THEN** its `data.type` equals `"pod"`, its `data.id` matches `<cluster>/<pod-uid>`, its `data.name` equals the pod's metadata name, and `data.labels.cluster` matches the cluster prefix in the ID

#### Scenario: Composed identity on every cluster-scoped element

- **WHEN** the build composed `zone-a-prod-cluster-alpha` for the raw cluster `cluster-alpha` and the response contains a pod, a K8s node, a PVC and a service of it
- **THEN** each `data.id` begins `zone-a-prod-cluster-alpha/`, each `data.labels.cluster` equals `zone-a-prod-cluster-alpha`, the pod's `data.labels.node` is `zone-a-prod-cluster-alpha/<node-name>`, the cluster group node is `{ id: "cluster/zone-a-prod-cluster-alpha", name: "zone-a-prod-cluster-alpha", type: "cluster" }`, the namespace groups beneath it are `zone-a-prod-cluster-alpha/namespace/<ns>`, and `clusters` is `["zone-a-prod-cluster-alpha"]` — the string `cluster-alpha` appears nowhere in the body

#### Scenario: Same raw name in two zones renders two clusters

- **WHEN** the build composed `us-dev-c1` and `eu-prod-c1` from the raw name `c1`
- **THEN** the response contains two cluster group nodes `cluster/us-dev-c1` and `cluster/eu-prod-c1`, `clusters` is `["eu-prod-c1","us-dev-c1"]`, and no node or edge carries `labels.cluster: "c1"`

#### Scenario: Unstamped cluster renders unchanged

- **WHEN** no series of `cluster-beta` carries both a zone and an environment label
- **THEN** every `cluster-beta` id, label and group is byte-identical to the body produced before cluster identities existed

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

#### Scenario: storage-flow never appears in a graph body

- **WHEN** a client sends any `GET /v1/graph` request
- **THEN** no edge in the response has `data.type` equal to `"storage-flow"`

### Requirement: Top-level `clusters` array lists Kubernetes cluster identities

The top-level `clusters` array SHALL list **Kubernetes** cluster identities only, and an ONTAP cluster name SHALL NEVER appear in it. An identity SHALL NOT be treated as a valid `?cluster=` value.

#### Scenario: Identity or raw name per Kubernetes cluster

- **WHEN** the body contains elements of a Kubernetes cluster
- **THEN** `clusters` lists that cluster's `<az>-<env>-<cluster>` identity of the `cluster-topology-source` capability ("Cluster identity composed from zone and environment labels"), or the raw name for a cluster that composed none

#### Scenario: ONTAP cluster label excluded

- **WHEN** the body contains `netapp-aggr` / `netapp-node` nodes
- **THEN** no ONTAP cluster name (the `ontap_cluster` label of those nodes) appears in `clusters`

#### Scenario: Narrowing to a listed cluster

- **WHEN** a client wants to narrow to one listed cluster
- **THEN** it sends the identity's three components as `az`, `env` and `cluster`, because an identity is NOT a valid `?cluster=` value (see "Filter parameters")

### Requirement: Graph node payload

Each **node** SHALL be `{ data: { id, name, type, labels } }`, and its `data` MAY carry an optional `parent` field (`omitempty`) referencing the `id` of the node's Cytoscape compound container (see "Cytoscape compound node grouping").

#### Scenario: Node type values

- **WHEN** the response contains any node
- **THEN** its `type` is one of the strings `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, `"netapp-aggr"`, `"netapp-node"`
- **AND** the Cytoscape serialiser additionally synthesises `"cluster"`, `"storage-cluster"`, `"namespace"`, `"application"`, and `"controller"` group nodes for compound nesting (see "Cytoscape compound node grouping")

#### Scenario: Cluster-scoped node ids

- **WHEN** the node is a pod, K8s node, PVC or service
- **THEN** its `id` is a cluster-scoped composite whose `<cluster>` is the cluster identity — pods: `<cluster>/<pod-uid>`; nodes: `<cluster>/<node-name>`; PVCs: `<cluster>/<namespace>/<claim>`; services: `<cluster>/<namespace>/<service>`

#### Scenario: NetApp node ids carry no Kubernetes cluster prefix

- **WHEN** the node is a NetApp aggregate or a NetApp node
- **THEN** an aggregate's `id` is `netapp/<ontap-cluster>/aggr/<aggr>` and a NetApp node's `id` is `netapp/<ontap-cluster>/<node>`, neither carrying a Kubernetes cluster prefix

#### Scenario: External node ids

- **WHEN** the node is an external node (an unresolvable `"://"` connection-string endpoint or the missing-UID human-label fallback)
- **THEN** its `id` is `external/<label-value>` (no cluster prefix)

#### Scenario: Node names

- **WHEN** the response contains any node
- **THEN** `name` is the human-readable pod / node / PVC / service name; for NetApp aggregates, the ONTAP aggregate name; for NetApp nodes, the ONTAP controller name
- **AND** for external nodes, `name` is the verbatim `client` or `server` label value from the source service-graph series

#### Scenario: Kubernetes node labels

- **WHEN** the node is a pod, K8s node, PVC or service
- **THEN** `labels` includes at minimum a `cluster` entry carrying the cluster identity — the same value as the `id` prefix
- **AND** pods, PVCs and services also include a `namespace` entry
- **AND** pods include `node` (the cluster-scoped node ID, identity-prefixed), and include `pod_ip` and `host_ip` whenever the upstream `kube_pod_info` series carried them
- **AND** K8s nodes include `external_ip` when the upstream provided one

#### Scenario: NetApp and external node labels

- **WHEN** the node is a NetApp aggregate, a NetApp node or an external node
- **THEN** a NetApp aggregate's `labels` is exactly `{ontap_cluster, node}` (the owning controller) and a NetApp node's exactly `{ontap_cluster}` — deliberately no `cluster` key on either
- **AND** an external node's `labels` is an empty object `{}` (no `cluster` key)

### Requirement: Graph edge payload

Each **edge** SHALL be `{ data: { id, type, source, target, labels } }`. Its `id` SHALL be an RFC 4122 compliant UUID encoded as a lowercase canonical string, and `source` and `target` SHALL each match the `id` of a node present in the same response's `elements.nodes`.

#### Scenario: Edge type set is fixed

- **WHEN** the response contains any edge
- **THEN** its `type` is one of the edge types this endpoint produces — `pod-mounts-pvc`, `pod-calls-pod`, `pod-calls-service`, `service-selects-pod`, `pod-to-node`, `pvc-to-netapp-aggr` — each defined by the `pod-service-graph`, `cluster-topology-source`, and `netapp-storage-graph` capabilities
- **AND** `storage-flow` is produced by `GET /v1/storage-graph` alone
- **AND** the set is a fixed contract of this specification; there is no discovery endpoint for it

#### Scenario: Edge label keys

- **WHEN** the response contains any edge
- **THEN** the exact `labels` key set per edge type is the one defined by the `pod-service-graph`, `cluster-topology-source`, and `netapp-storage-graph` capabilities
- **AND** wherever an edge carries a `cluster` key its value is a cluster identity, never a raw name that appears on no node of the body

#### Scenario: Optional edge metrics

- **WHEN** an edge has measurements
- **THEN** its `data` MAY carry an optional `metrics` object (`omitempty`) holding them — see "Edge `metrics` attribute"

### Requirement: Labels carry strings only

`labels` on every node and edge SHALL be a JSON object whose values are strings only (`map[string]string`), and implementations SHALL NOT encode booleans or numbers as strings inside `labels`.

#### Scenario: Numeric measurements are typed

- **WHEN** an element carries a numeric measurement
- **THEN** it is carried exclusively on the typed `data.metrics` object — never inside `labels`

#### Scenario: Boolean flags are not part of v1

- **WHEN** an element would carry a boolean flag
- **THEN** no such flag is emitted: boolean flags remain deferred to a future typed field and are NOT part of the v1 contract

### Requirement: Filter parameters

`GET /v1/graph` SHALL accept exactly the parameters `start`, `end`, `cluster`, `namespace`, `az`, `env` and `prune`, where every parameter except `start` / `end` is optional, `cluster` / `namespace` / `az` / `env` are each repeatable, and `prune` is single-valued (`true` | `false`, default `true`). Neither an unknown filter value nor an unknown parameter SHALL cause an error.

#### Scenario: Values combine OR within a parameter and AND across parameters

- **WHEN** a request carries several values for one parameter and values for several parameters
- **THEN** multiple values for the same parameter are OR-combined and different parameters are AND-combined

#### Scenario: Unknown filter value yields an empty result

- **WHEN** a filter **value** names a cluster, namespace, zone, or environment with no data
- **THEN** the request does not fail; it yields an empty result

#### Scenario: Unknown or withdrawn parameter is ignored

- **WHEN** a request carries an unknown filter **parameter** — including the withdrawn `name`, `root`, `depth`, `direction` and `edge_type` — whatever value it carries
- **THEN** the parameter is ignored without error: a withdrawn parameter never narrows the body and never rejects the request

#### Scenario: Empty filters return the connectivity-connected subgraph

- **WHEN** a request carries no filter
- **THEN** the response is the **connectivity-connected subgraph** of the full multi-cluster graph for the time window (the default connectivity prune — see "Default projection prunes connectivity-disconnected workload"), NOT the full topology inventory
- **AND** `prune=false` returns the inventory instead

#### Scenario: Cluster filter narrows result

- **WHEN** the upstream holds pods in `cluster-alpha` and `cluster-beta` and a client sends `?cluster=cluster-alpha`
- **THEN** every topology query carrying a `cluster` label is issued with `cluster="cluster-alpha"`, the response contains pod nodes only for `cluster-alpha`, and any `cluster-beta` peer of a `cluster-alpha` pod appears as an `external/<label>` node (not as a `cluster-beta` pod node — see "Cross-cluster edge representation")

#### Scenario: Raw cluster filter selects every zone's cluster of that name

- **WHEN** the upstream holds `c1` under `az="us",env="dev"` and `c1` under `az="eu",env="prod"` and a client sends `?cluster=c1`
- **THEN** the queries are issued with `cluster="c1"`, the response contains both `us-dev-c1` and `eu-prod-c1` workload, and `clusters` is `["eu-prod-c1","us-dev-c1"]`

#### Scenario: Zone, environment and cluster pin one identity

- **WHEN** the same upstream and a client sends `?az=us&env=dev&cluster=c1`
- **THEN** the queries are issued with `az="us",env="dev",cluster="c1"`, the response contains only `us-dev-c1` workload, and `clusters` is `["us-dev-c1"]`

#### Scenario: A listed identity is not a cluster filter value

- **WHEN** a client reads `clusters: ["us-dev-c1"]` from one response and sends `?cluster=us-dev-c1`
- **THEN** the queries are issued with `cluster="us-dev-c1"`, match no series, and the response is 200 with empty `elements` and an empty `clusters` list

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
- **THEN** the response is 200 with `elements.edges: []` and no error — because the default connectivity prune leaves nothing to draw, not because the parameter filtered anything; the body is identical to the same request without `edge_type`

#### Scenario: Withdrawn edge_type parameter is ignored

- **WHEN** a client sends `?edge_type=pod-calls-pod` for a window whose built graph holds `pod-calls-pod`, `pod-to-node` and `pod-mounts-pvc` edges
- **THEN** the response is 200 and byte-identical to the response for the same request without `edge_type` — every edge type the projection would otherwise emit is present, none is filtered out

#### Scenario: Unregistered edge_type value is not an error

- **WHEN** a client sends `?edge_type=pod-calls-pods` (a value no edge type has ever carried)
- **THEN** the response is 200 with the default view, not 400 `invalid_scope` — the parameter is unknown to the server and its value is never inspected

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

- **WHEN** a client sends `?name=frontend&root=cluster-alpha/abc&depth=1&edge_type=pod-calls-pod`
- **THEN** the server ignores the four parameters and returns the unanchored view for the remaining parameters (200), not a 400

#### Scenario: No selector-level filter issues today's queries

- **WHEN** a client sends `GET /v1/graph?start=...&end=...` with no other parameters
- **THEN** every upstream query is byte-identical to the query issued before request-scoped selectors existed, and the response body is byte-identical to the pre-change body for the same upstream data

### Requirement: Raw cluster filter value

The `cluster` value SHALL be the **raw** Kubernetes cluster name — the value of the upstream `cluster` label — not the composed identity the response lists in `clusters[]`.

#### Scenario: A raw name spans zones and environments

- **WHEN** a request carries `cluster=<raw>`
- **THEN** it selects every cluster identity whose raw component equals `<raw>`, across zones and environments

#### Scenario: Zone, environment and cluster triple pins one identity

- **WHEN** a request carries `az` + `env` + `cluster`
- **THEN** the triple pins exactly one identity, because those three request dimensions are the identity's three components

### Requirement: Selector-level filters render upstream label matchers

The selector-level filters `cluster`, `namespace`, `az`, `env` SHALL be rendered into the upstream PromQL queries of the build as label matchers, so the graph is narrowed by VictoriaMetrics before any sample is read. A request with no selector-level filter SHALL issue exactly the queries it issues today and produce a byte-identical body.

#### Scenario: Matcher reach is a fixed contract

- **WHEN** a selector-level filter is rendered
- **THEN** which matcher reaches which series is the hardcoded contract of the `cluster-topology-source` capability ("Request-scoped upstream selectors")
- **AND** the service-graph series are deliberately read unfiltered (`pod-service-graph`)

#### Scenario: Single value renders an exact matcher

- **WHEN** a parameter carries a single value
- **THEN** it renders an exact matcher `<key>="<value>"`, with `"` and `\` escaped

#### Scenario: Several values render one anchored alternation

- **WHEN** a parameter carries several values
- **THEN** they render one fully-anchored alternation (`<key>=~"<v1>|<v2>"`) over the sorted, de-duplicated, regex-quoted values

#### Scenario: cluster=unknown addresses both bucket spellings

- **WHEN** a request carries `cluster=unknown`
- **THEN** the server renders `cluster=~"unknown|"`, so both spellings of the bucket — a series carrying no `cluster` label and one whose label is literally `unknown` — remain addressable, matching what the projection filter accepts

### Requirement: Selector values are validated before rendering

Selector-level values and `prune` SHALL be validated before any upstream query is rendered, and an invalid value SHALL be rejected with `400 Bad Request` and `reason: "invalid_scope"`.

#### Scenario: Over-long value rejected

- **WHEN** a selector-level value is longer than 253 bytes
- **THEN** the server returns `400 Bad Request` with `reason: "invalid_scope"`

#### Scenario: Control character rejected

- **WHEN** a selector-level value contains a control character (including newline)
- **THEN** the server returns `400 Bad Request` with `reason: "invalid_scope"`

#### Scenario: Non-boolean prune rejected

- **WHEN** the `prune` value is not exactly `true` / `false`
- **THEN** the server returns `400 Bad Request` with `reason: "invalid_scope"`

### Requirement: Projection-level filters

The projection-level filters — `cluster` and `namespace` (applied again over the built graph as defence in depth) and `prune` — SHALL be applied at response time as a projection over the freshly built graph, and there SHALL be no projection over edge type.

#### Scenario: Projection cluster check compares the raw-name component

- **WHEN** the projection-level `cluster` check runs
- **THEN** it compares the request's raw values against the **raw-name component** of each element's cluster identity, recovered from the built graph's identity table (an identity absent from the table compares as itself)
- **AND** the projection therefore admits exactly what the upstream matcher admitted

#### Scenario: Every surviving edge is emitted regardless of type

- **WHEN** node filtering completes
- **THEN** every edge of the built graph whose endpoints survive node filtering is emitted, and a client wanting fewer edge types drops them by `data.type`

### Requirement: Edge retention rule

**Edge retention rule (unified across all filters).** An edge SHALL be retained when at least one resolved endpoint is in scope after node filtering. When exactly one endpoint is in scope, the missing endpoint SHALL be re-added from the freshly built graph's node index provided it passes the non-cluster filters.

#### Scenario: Types without a namespace label pass the namespace check

- **WHEN** the missing endpoint is re-added and it has no namespace label
- **THEN** it passes the namespace check — `node`, `external`, and the NetApp types `netapp-aggr` / `netapp-node` (which carry neither a namespace nor a `cluster` label) pass through

#### Scenario: Rule is edge-type-agnostic

- **WHEN** a non-pod endpoint is incident on an in-scope pod
- **THEN** the single rule retains it — including the topology `pod-to-node` edge, the `pvc-to-netapp-aggr` edge, and the `external` partners that a filtered build produces for out-of-scope peers

#### Scenario: Unfiltered cross-cluster partner re-added

- **WHEN** an **unfiltered** build holds a cross-cluster edge whose partner endpoint lies outside a projection-level `cluster` set
- **THEN** the edge is retained and the partner re-added under the same rule

### Requirement: Default projection prunes connectivity-disconnected workload

`GET /v1/graph` SHALL, on every request whose `prune` parameter is absent or `true`, return only the workload that participates in the connectivity graph. The prune SHALL be a pure function of the built graph, applied uniformly for every selector-level filter shape, and SHALL NOT resurrect a pruned pod/PVC through the edge-retention partner re-add.

#### Scenario: Pod retention

- **WHEN** the prune runs
- **THEN** a `pod` node is retained iff it is an endpoint of at least one connectivity edge (`pod-calls-pod`, `pod-calls-service`, or `service-selects-pod`)

#### Scenario: PVC retention

- **WHEN** the prune runs
- **THEN** a `pvc` node is retained iff at least one of the pods that mount it (`pod-mounts-pvc`) is itself retained
- **AND** a PVC with no mounting pod at all is dropped

#### Scenario: Reference-driven infra admission

- **WHEN** the prune runs over the connectivity-pruned pod/PVC set
- **THEN** a `node` (K8s host) and a `netapp-aggr` are retained iff referenced by a retained element (a pod scheduled on the node, a PVC joined to the aggregate via `pvc-to-netapp-aggr`)
- **AND** a `netapp-node` is retained iff referenced by a retained `netapp-aggr` (its `labels.node`), applying the rule transitively for the NetApp chain

#### Scenario: Connectivity-born nodes are not pruned

- **WHEN** the body holds `service` or `external` nodes
- **THEN** they are not pruned by this rule, because they are connectivity-born (only ever materialised as edge endpoints)

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
- **THEN** `web` is retained (because the service-graph series are read in full, its edge to the `external/<payments-peer-label>` partner a filtered build materialises is a connectivity edge) and the response contains `web`, that `external` node, and the edge

### Requirement: prune=false disables the connectivity prune

`prune=false` SHALL turn the prune off: every loaded pod SHALL be emitted together with its `pod-to-node`, `pod-mounts-pvc`, and `pvc-to-netapp-aggr` chain regardless of traffic, and every loaded PVC SHALL be emitted whether or not it is mounted. `prune=false` SHALL be the only escape hatch.

#### Scenario: Withdrawn escape hatches do not disable the prune

- **WHEN** a request carries the former `name` / `root` escape hatches without `prune=false`
- **THEN** the prune still applies, because those escape hatches are withdrawn

#### Scenario: Unreferenced infrastructure under prune=false

- **WHEN** a request carries `prune=false`
- **THEN** its effect on unreferenced infrastructure nodes is the one specified in "Namespace-filter retention of cluster-scoped infra nodes"

### Requirement: Cross-cluster edge representation

A cross-cluster edge (`pod-calls-pod`, `pod-calls-service`, or `service-selects-pod` whose source-node cluster **identity** differs from its target-node cluster identity) SHALL be emitted with **both real endpoint nodes** only when both clusters are **loaded** by the build — every build without a `cluster` filter, or a build whose `cluster` filter lists both clusters' raw names.

#### Scenario: Consumers detect cross-cluster status from the endpoint nodes

- **WHEN** a consumer needs to know whether an edge is cross-cluster
- **THEN** it compares the `labels.cluster` of the edge's resolved source and target nodes — not edge labels
- **AND** two clusters sharing a raw name under different zones or environments are distinct identities, so an edge between them IS cross-cluster

#### Scenario: Edge cluster labels by type

- **WHEN** the body carries a `pod-calls-pod` or `service-selects-pod` edge
- **THEN** a `pod-calls-pod` edge carries `labels.cluster` (the client pod's cluster identity, present iff the client side resolved to a pod — `pod-service-graph` "Edge cluster label")
- **AND** a `service-selects-pod` edge carries no `cluster` key (its source is a service node, which is cluster-scoped via its own `labels.cluster`)

#### Scenario: Excluded side becomes an external

- **WHEN** a `cluster` filter excludes one side of a call
- **THEN** that side's topology is not loaded and the peer follows the `pod-service-graph` capability's filtered-build rule, rendered as an `external/<label>` node (carrying no `cluster`)
- **AND** the edge is no longer cross-cluster — it is an edge to an external; the former rule that a `?cluster=` projection keeps the out-of-scope partner as a real pod node is withdrawn

#### Scenario: Service fan-out reaches loaded clusters only

- **WHEN** a local service node fans out `service-selects-pod` family-wide
- **THEN** the fan-out reaches only backing pods in **loaded** clusters

#### Scenario: Cross-cluster edge with both clusters in scope

- **WHEN** a client requests `?cluster=cluster-alpha&cluster=cluster-beta` for a window containing a cross-cluster `pod-calls-pod` edge whose client pod is in `cluster-alpha` and server pod is in `cluster-beta`
- **THEN** the response contains both endpoint pod nodes and one edge with `labels.cluster: "cluster-alpha"`, where the source node's `labels.cluster` is `"cluster-alpha"` and the target node's `labels.cluster` is `"cluster-beta"`

#### Scenario: Same raw name across zones is cross-cluster

- **WHEN** an unfiltered build holds `us-dev-c1` and `eu-prod-c1` and the service graph records a call from a `us-dev-c1` pod to an `eu-prod-c1` pod
- **THEN** the response contains both pod nodes, the edge carries `labels.cluster: "us-dev-c1"`, the source node's `labels.cluster` is `"us-dev-c1"` and the target node's is `"eu-prod-c1"`, and the edge counts as cross-cluster in the build's edge-count gauge

#### Scenario: Cross-cluster edge with one cluster in scope

- **WHEN** a client requests `?cluster=cluster-alpha` and the service-graph series records a call from a pod in `cluster-alpha` to a pod in `cluster-beta` whose `server` label is `cart`
- **THEN** the response contains the `cluster-alpha` endpoint, an `external/cart` node with `labels={}`, and one `pod-calls-pod` edge from the pod to `external/cart` with `labels.cluster: "cluster-alpha"`; no `cluster-beta` pod node is present

#### Scenario: Cross-cluster service-selects-pod edge from the local service node's endpoint union

- **WHEN** clusters `prod-1` and `prod-2` (family `prod-0`) both hold a `payments` service in namespace `payments-ns`, a pod in `prod-1` emits a `"://"` connection string addressing it, and a client requests `?cluster=prod-1&cluster=prod-2`
- **THEN** the response contains the single `pod-calls-service` edge from the `prod-1` pod to the `prod-1/payments-ns/payments` service node plus `service-selects-pod` edges to the backing pods of **both** clusters (the `prod-2` targets being cross-cluster, detected by comparing the endpoint nodes' `labels.cluster`); whereas a `?cluster=prod-1` request yields the same service node with `service-selects-pod` edges to `prod-1`'s backing pods only

### Requirement: Deterministic response body

For identical input — same `(aligned window, filters, upstream-data)` — the server SHALL produce a byte-identical response body across rebuilds, whether each upstream result was fetched or served from the in-process query-result cache. The server SHALL NOT emit any HTTP cache validator (no `ETag`, no `Last-Modified`).

#### Scenario: Cache scope

- **WHEN** the in-process cache is consulted
- **THEN** it operates on individual upstream query results (see the `upstream-query-cache` capability), never on built graphs or response bodies
- **AND** within one aligned window and selector set the projection-level filters remain a pure function of the built graph

#### Scenario: Serialiser sorting

- **WHEN** the serialiser renders a body
- **THEN** it sorts `view.Nodes` and `view.Edges`, sorts `Graph.ClusterNames()`, and sorts `IPAddress` slices at construction
- **AND** keeps the response body shape fixed at `{apiVersion, clusters, elements}` for graph routes (no time-of-build or echo-of-input fields)

#### Scenario: Selectors are pure functions of the parameter values

- **WHEN** upstream selectors are rendered
- **THEN** every rendered upstream selector is a pure function of the sorted, de-duplicated parameter values

#### Scenario: Cache-Control headers

- **WHEN** a client sends `GET /openapi.yaml`, `GET /openapi.json`, or `GET /docs`
- **THEN** the response carries an explicit `Cache-Control` header
- **AND** `GET /v1/graph` does not emit a `Cache-Control` header

#### Scenario: Body byte-identical across repeated requests

- **WHEN** a client sends two consecutive `GET /v1/graph` requests with identical query parameters and the upstream data has not changed between them
- **THEN** both response bodies are byte-identical, whether the second request's upstream results came from the cache or from an independent upstream fan-out

#### Scenario: Parameter order does not change the body

- **WHEN** a client sends `?az=b&az=a&namespace=y&namespace=x` and then `?namespace=x&namespace=y&az=a&az=b` for the same window and upstream data
- **THEN** both requests render identical upstream selectors and return byte-identical bodies

### Requirement: Node `ipaddress` attribute

Every `data` object for a node in the Cytoscape response SHALL expose a top-level `ipaddress` field of type `string[]` with `omitempty` semantics, sourced per node type. The legacy `labels.pod_ip`, `labels.host_ip`, `labels.external_ip` and `labels.internal_ip` keys SHALL NOT appear on any node entry.

#### Scenario: Pod source

- **WHEN** a `type="pod"` node is serialised
- **THEN** it carries the pod's IP from `kube_pod_info.pod_ip` (single-element slice) when the source metric surfaces it, and omits the field otherwise

#### Scenario: K8s node source and precedence

- **WHEN** a `type="node"` node is serialised
- **THEN** it carries the K8s node's `ExternalIP` from `kube_node_status_addresses` (single-element slice) when present, falling back to the node's `InternalIP` (single-element slice) when no ExternalIP row exists, and omits the field only when neither address type is present
- **AND** an ExternalIP always wins over an InternalIP regardless of upstream sample order

#### Scenario: Service source

- **WHEN** a `type="service"` node is serialised
- **THEN** it carries the service's `cluster_ip` from `kube_service_info` (single-element slice) when `cluster_ip` is not `"None"`, and omits the field for headless services (`cluster_ip="None"`) or when the metric does not surface it

#### Scenario: Legacy label keys replaced

- **WHEN** any node entry is serialised
- **THEN** `labels.pod_ip`, `labels.host_ip`, and `labels.external_ip` are absent — replaced by the typed `ipaddress` attribute and the node entry respectively
- **AND** no `labels.internal_ip` key appears either — the InternalIP fallback surfaces only via `ipaddress`

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

Every `data` object for a `type="node"` node SHALL expose a top-level `ready_status` field of type `string` with `omitempty` semantics, carrying the node's Kubernetes Ready-condition status derived from `kube_node_status_condition{condition="Ready"}` (see cluster-topology-source Requirement: K8s node Ready-status attribute). It SHALL be a typed attribute, never inside `labels`.

#### Scenario: Allowed values

- **WHEN** a node carries `ready_status`
- **THEN** the value is exactly one of `"Ready"`, `"NotReady"`, or `"Unknown"`

#### Scenario: Omitted without Ready-condition data

- **WHEN** the source metric is absent, the node has no `condition="Ready"` series, or no status row is active
- **THEN** the field is omitted entirely — the node carries no `ready_status` key, NOT `ready_status: ""` and NOT `ready_status: "Unknown"`

#### Scenario: Unknown only for the genuine Kubernetes state

- **WHEN** the literal `"Unknown"` is emitted
- **THEN** it is only for the genuine Kubernetes state where the Ready condition's `status` label is `unknown` (kubelet not reporting); it is never a stand-in for missing data

#### Scenario: Not a label

- **WHEN** a node carries a Ready status
- **THEN** it does not appear inside `labels` (which remain a strict `map[string]string` of typological metadata) — the same precedent as `ipaddress` and `owner`

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

When the server is started with at least one API key configured (via `--api-keys-file` or `--api-keys`), every request to `/v1/*` and `/debug/*` SHALL carry an `X-API-Key: <key>` header, and a request whose header is missing or names a key outside the configured set SHALL receive `401 Unauthorized` with reason `unauthorized`.

#### Scenario: Missing-header message

- **WHEN** a request to a protected route carries no `X-API-Key` header
- **THEN** the `401` body carries a JSON message indicating the missing header

#### Scenario: No keys configured disables the middleware

- **WHEN** no keys are configured (both flags empty)
- **THEN** the middleware is a no-op: every route behaves as if auth were not configured
- **AND** the server logs a warning at boot identifying that auth is disabled

#### Scenario: Exempt routes

- **WHEN** a request targets `/livez`, `/readyz`, `/metrics`, `/openapi.yaml`, `/openapi.json`, or `/docs`
- **THEN** it is exempt from authentication regardless of configuration

#### Scenario: Constant-time full-set comparison

- **WHEN** a presented key is compared
- **THEN** the comparison is constant-time and iterates the full configured key set on every request, so neither match latency nor early exit reveals the matching position

#### Scenario: Rejection counters

- **WHEN** a request is rejected
- **THEN** the server increments `kube_state_graph_auth_rejected_total{reason="missing"}` for a request without the header and `kube_state_graph_auth_rejected_total{reason="invalid"}` for one whose header value is unknown

#### Scenario: Key-file reload cadence

- **WHEN** `--api-keys-file` is set and `--api-keys-reload-interval` is positive
- **THEN** the server re-reads the file on the configured cadence and atomically swaps the active key set

#### Scenario: Missing header is rejected

- **WHEN** the server is started with `--api-keys=k1` and a client sends `GET /v1/graph?start=...&end=...` with no `X-API-Key`
- **THEN** the response is `401 Unauthorized` with body `{"error":{"reason":"unauthorized", ...}}`

#### Scenario: Wrong key is rejected

- **WHEN** the server is started with `--api-keys=k1` and a client sends `X-API-Key: wrong`
- **THEN** the response is `401 Unauthorized` with reason `unauthorized`

#### Scenario: Valid key is accepted

- **WHEN** the server is started with `--api-keys=k1,k2` and a client sends `X-API-Key: k2` to `GET /v1/graph?start=...&end=...`
- **THEN** the response is `200 OK` with a Cytoscape.js graph body

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

The server SHALL expose `GET /livez` that returns 200 while the process is running, and `GET /readyz` that returns 200 only when a 1-second `up{}` probe against **every** backend in the live routing table succeeds, and 503 otherwise with a JSON body carrying a `reason` field that names the backends that did not answer.

#### Scenario: No routing table probes the implicit backend

- **WHEN** a deployment has no routing table configured
- **THEN** the single implicit `default` backend is the only one probed, so the observable behaviour is unchanged

#### Scenario: Probes share one budget

- **WHEN** several backends are probed
- **THEN** the probes are issued concurrently and share the single 1-second budget, so readiness latency does not grow with the number of backends

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

The server SHALL expose `GET /metrics` in Prometheus exposition format including at least the self-metrics listed in this requirement's scenarios. Adding a label to an existing self-metric is a contract change.

#### Scenario: Required metric names

- **WHEN** a client scrapes `GET /metrics`
- **THEN** the exposition includes `kube_state_graph_build_duration_seconds`, `kube_state_graph_project_duration_seconds`, `kube_state_graph_serialise_duration_seconds`, `kube_state_graph_build_rejected_total`, `kube_state_graph_graph_node_count`, `kube_state_graph_graph_edge_count`, `kube_state_graph_clusters_observed`, `kube_state_graph_upstream_query_duration_seconds`, `kube_state_graph_upstream_query_failures_total`, `kube_state_graph_http_requests_total`, `kube_state_graph_auth_rejected_total`, `kube_state_graph_upstream_backends`, `kube_state_graph_backend_config_reload_total`, `kube_state_graph_backend_query_failures_total`, and `kube_state_graph_upstream_query_result_series`

#### Scenario: Upstream query metrics keep their pre-routing label sets

- **WHEN** backend routing is in use
- **THEN** `kube_state_graph_upstream_query_duration_seconds` and `kube_state_graph_upstream_query_failures_total` keep exactly the label sets they carried before backend routing existed
- **AND** per-backend detail is carried by `kube_state_graph_backend_query_failures_total` instead

#### Scenario: Result-series histogram observations

- **WHEN** an upstream query succeeds
- **THEN** `kube_state_graph_upstream_query_result_series`, a histogram labelled by `query` only, observes the number of series it returned — one observation per issued query, so a scoped read split into several chunks contributes one observation per chunk under the bare family name, and a query fanned out to several backends contributes one per backend
- **AND** a failed query contributes no observation

#### Scenario: Result-series histogram buckets

- **WHEN** the histogram is exposed
- **THEN** its buckets are the powers of two from 1024 to 1048576, so an operator can read how close a leg sits to an upstream series limit (VictoriaMetrics' memory-derived `-search.maxUniqueTimeseries`) before that limit rejects the query

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

#### Scenario: Result-series histogram after a build

- **WHEN** a `/v1/graph` build has completed and a client scrapes `/metrics`
- **THEN** `kube_state_graph_upstream_query_result_series_count{query="kube_pod_info"}` is at least 1, its `_bucket` series carry `le` values `1024`, `2048`, … `1048576` and `+Inf`, and no series of the metric carries a `backend` label

### Requirement: Per-build timeout (graph endpoints)

For `GET /v1/graph`, the server SHALL apply a configurable per-build `context.WithTimeout` derived from `--build-timeout` (default 15 seconds), and on `context.DeadlineExceeded` SHALL abort the build and respond `504 Gateway Timeout` with `reason: "timeout"`.

#### Scenario: Timeout is counted

- **WHEN** a build hits `context.DeadlineExceeded`
- **THEN** the `kube_state_graph_build_rejected_total{reason="timeout"}` counter is incremented
- **AND** the status follows RFC 9110 §15.6.5: the gateway did not receive a timely response from an upstream server it needed to access in order to complete the request

#### Scenario: Upstream stalls beyond build timeout

- **WHEN** centralised VictoriaMetrics fails to respond to a `/v1/graph` build within `--build-timeout`
- **THEN** the request returns 504 with `reason: "timeout"`

### Requirement: Per-request timeout (non-graph endpoints)

For non-graph endpoints that perform upstream calls (`GET /readyz` `up{}` probe), the server SHALL apply a `context.WithTimeout` derived from `--api-timeout` (default 5 seconds) to the upstream call, and on `context.DeadlineExceeded` SHALL respond `504 Gateway Timeout` with `reason: "timeout"`.

#### Scenario: Same timeout bounds the retention probe

- **WHEN** a build issues its `up{}` retention probe
- **THEN** the probe is bounded by the same `--api-timeout`

#### Scenario: Endpoints without upstream calls are not subject to it

- **WHEN** a client sends `GET /livez`, `GET /metrics`, `GET /openapi.*`, or `GET /docs*`
- **THEN** the request is not subject to this timeout, because those endpoints do not perform upstream calls

#### Scenario: Readiness probe stalls beyond api timeout

- **WHEN** centralised VictoriaMetrics fails to respond to the `/readyz` `up{}` probe within `--api-timeout`
- **THEN** the request returns 504 with `reason: "timeout"`

#### Scenario: Cluster discovery stalls beyond api timeout

- **WHEN** a client sends `GET /v1/clusters` while centralised VictoriaMetrics is unresponsive
- **THEN** the request returns 404 Not Found immediately — the endpoint is removed, no upstream call is made, and the api timeout does not apply

#### Scenario: Edge-type catalogue request while upstream is unresponsive

- **WHEN** a client sends `GET /v1/edge-types` while centralised VictoriaMetrics is unresponsive
- **THEN** the request returns 404 Not Found immediately with the standard error body — the endpoint is removed, no upstream call is made, and the api timeout does not apply

### Requirement: Outside-retention error

When a build carrying **no selector-level filter** finds zero pods and zero nodes for the requested window but the upstream VictoriaMetrics is reachable (a parallel `up{}` probe succeeds), the server SHALL respond `400 Bad Request` with `reason: "outside_retention"`.

#### Scenario: Filtered build with zero rows means nothing in scope

- **WHEN** any selector-level filter (`cluster`, `namespace`, `az`, `env`) is active and the build finds zero rows
- **THEN** the classification does not run and no `up{}` probe is issued for it
- **AND** the server responds `200` with empty `elements.nodes`, empty `elements.edges`, and an empty `clusters` list

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

The server SHALL serve the Scalar API Reference UI at `GET /docs`, rendering the same-origin OpenAPI spec from `/openapi.json` via `Scalar.createApiReference`, and the `/docs` response SHALL set `Content-Security-Policy`, `X-Frame-Options: DENY`, and `X-Content-Type-Options: nosniff` headers.

#### Scenario: Pinned, integrity-checked CDN bundle

- **WHEN** the `/docs` page loads the Scalar standalone bundle
- **THEN** it is loaded from the jsDelivr CDN, pinned to an exact version and carrying a Subresource Integrity (`integrity=`) hash, so a mutated CDN artifact cannot execute
- **AND** there is no vendored `/docs/assets/*` route

#### Scenario: /docs renders the Scalar UI from the pinned CDN bundle

- **WHEN** a client sends `GET /docs`
- **THEN** the response is 200, `Content-Type: text/html`, and the page loads the version-pinned `@scalar/api-reference` standalone bundle from `cdn.jsdelivr.net` with an `integrity` hash, then calls `Scalar.createApiReference` against the same-origin `/openapi.json`, and the response carries `Content-Security-Policy`, `X-Frame-Options`, and `X-Content-Type-Options` headers

### Requirement: Route ↔ spec drift guard

The repository SHALL guard against handler annotations drifting from the generated OpenAPI spec: the `make check-docs` CI job SHALL regenerate the swag output from source annotations and fail on any diff against the committed `docs/`.

#### Scenario: Scope of the guard

- **WHEN** an added, removed, or edited `@Router` / `@Summary` annotation is not reflected in `docs/`
- **THEN** CI fails
- **AND** no Go test diffs the embedded spec against the live Gin route table (descoped to avoid adding a `kin-openapi` dependency); `make check-docs` covers annotation↔source drift

#### Scenario: Handler annotation edited without regenerating docs

- **WHEN** a contributor adds, removes, or edits a `// @Router` / `// @Summary` annotation and does not run `make docs`
- **THEN** the `check-docs` CI job fails with a `git diff` showing the stale `docs/swagger.{json,yaml}`

### Requirement: Cytoscape compound node grouping

`GET /v1/graph` and `GET /v1/storage-graph` SHALL express, via an optional `data.parent` field, a workload compound hierarchy `cluster > namespace > application > controller > pod` (with **skip-absent-levels**), plus `cluster > namespace > { service, pvc }`, `cluster > node`, and the storage chain `storage-cluster > netapp-node > netapp-aggr` and `storage-cluster > netapp-svm`. Grouping is a presentation concern and SHALL NOT affect the core graph, projection, or traversal.

#### Scenario: Group nodes are synthesised by the serialiser

- **WHEN** a body carries compound groups
- **THEN** the `cluster`, `storage-cluster`, `namespace`, `application`, and `controller` group nodes are synthesised by the Cytoscape serialiser

#### Scenario: Prior storageclass grouping superseded

- **WHEN** any body is serialised
- **THEN** this requirement **supersedes** the prior `storageclass` grouping and node behaviour: there is no `type="storageclass"` node (real or synthesised) and no `pvc-to-storageclass` edge in any form
- **AND** the claim's StorageClass name survives only as the PVC's own `data.storageclass` attribute

#### Scenario: Cluster group node synthesised

- **WHEN** the graph contains any node with `labels.cluster="cluster-alpha"`
- **THEN** the Cytoscape response contains a node `{ data: { id: "cluster/cluster-alpha", name: "cluster-alpha", type: "cluster", labels: {} } }` with no `parent` field

#### Scenario: Storage-cluster group node synthesised

- **WHEN** the response contains a `netapp-node` node with `labels.ontap_cluster="ontap-prod"`
- **THEN** the response contains a node `{ data: { id: "storage-cluster/ontap-prod", name: "ontap-prod", type: "storage-cluster", labels: {} } }` with no `parent` field, and the `netapp-node` node's `data.parent` equals `storage-cluster/ontap-prod`

#### Scenario: Aggregate nests under its real owning node

- **WHEN** the response contains a `netapp-aggr` node with `labels={ontap_cluster:"ontap-prod", node:"ontap-prod-01"}`
- **THEN** its `data.parent` equals `netapp/ontap-prod/ontap-prod-01` — the id of the **real** `netapp-node` node, which is present in the response and itself carries `data.parent="storage-cluster/ontap-prod"`

#### Scenario: SVM nests under the storage cluster

- **WHEN** a storage-graph response contains a `netapp-svm` node with `labels.ontap_cluster="ontap-prod"`
- **THEN** its `data.parent` equals `storage-cluster/ontap-prod`, the storage-cluster group is present even if no `netapp-node` is, and no node names the SVM as its `parent`

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

### Requirement: Real NetApp node is the only real compound parent

In the storage chain the middle tier SHALL NOT be a synthesised group: the **real** `type="netapp-node"` node SHALL act as the compound parent of its `netapp-aggr` nodes, and no other real node type SHALL acquire compound children.

#### Scenario: Scoped break from the synthesised-parent rule

- **WHEN** compound parents are assigned
- **THEN** the real-node parenting is a deliberate, explicitly-scoped break from the "relationships are edges, compound parents are synthesised groups" rule (the rule that removed pod-under-node nesting), applying to the `netapp-node > netapp-aggr` tier ONLY

#### Scenario: SVM sits beside controllers

- **WHEN** a body carries a `netapp-svm`
- **THEN** because an SVM spans aggregates and controllers, `netapp-svm` nests directly under its `storage-cluster` group and is never a compound child of a controller nor a compound parent of anything

### Requirement: Cluster and storage-cluster group nodes

The serialiser SHALL emit one cluster group node for each distinct `labels.cluster` value present on an emitted node, and one storage-cluster group node for each distinct `labels.ontap_cluster` value present on an emitted `netapp-aggr`, `netapp-node` or `netapp-svm` node, each with no `parent` and no `ipaddress`.

#### Scenario: Cluster group shape

- **WHEN** an emitted node carries `labels.cluster`
- **THEN** the serialiser emits `{ data: { id: "cluster/<cluster>", name: "<cluster>", type: "cluster", labels: {} } }`

#### Scenario: Storage-cluster group shape

- **WHEN** an emitted NetApp node carries `labels.ontap_cluster`
- **THEN** the serialiser emits `{ data: { id: "storage-cluster/<ontap-cluster>", name: "<ontap-cluster>", type: "storage-cluster", labels: {} } }`, so the NetApp node set nests under its own filer rather than floating parentless like `external` nodes
- **AND** a storage-cluster group is NOT a Kubernetes cluster group; its name never appears in `clusters[]`

### Requirement: Synthesised workload group nodes

The serialiser SHALL derive `namespace`, `application` and `controller` group nodes from each emitted real node's own attributes, and each group node's `id` SHALL encode its full ancestry path, so it has exactly one parent (the tree is unambiguous by construction and no `data.parent` can dangle).

#### Scenario: Group sources

- **WHEN** groups are derived
- **THEN** `namespace` and `application` groups come from any `type="pod"`, `type="service"`, or `type="pvc"` node (via its `labels.namespace` and its resolved `data.application`)
- **AND** `controller` groups come from `type="pod"` nodes only (via the pod's resolved `data.owner` `{kind, name}`)

#### Scenario: Namespace group shape

- **WHEN** a namespace group is emitted
- **THEN** it is `{ id: "<cluster>/namespace/<ns>", name: "<ns>", type: "namespace", labels: {}, parent: "cluster/<cluster>" }`

#### Scenario: Application group shape

- **WHEN** any pod, service, or PVC has a resolved Application
- **THEN** an application group `{ id: "<cluster>/namespace/<ns>/application/<app>", name: "<app>", type: "application", labels: {}, parent: "<cluster>/namespace/<ns>" }` is emitted

#### Scenario: Controller group shape

- **WHEN** a pod has a resolved owner
- **THEN** a controller group is emitted as `{ id: "<cluster>/namespace/<ns>/application/<app>/controller/<kind>/<name>", ... , parent: <the application group id> }` when the pod also has a resolved Application, otherwise as `{ id: "<cluster>/namespace/<ns>/controller/<kind>/<name>", ... , parent: "<cluster>/namespace/<ns>" }`
- **AND** its `name` is `<name>`, `type` is `"controller"`, and `labels` is `{}`

### Requirement: Group node emission order

All synthesised group nodes SHALL carry `labels: {}` and no `ipaddress`, and SHALL be emitted in tier order (cluster, then storage-cluster, then namespace, then application, then controller), each tier ordered by `id`, before the non-group nodes, so the body stays byte-deterministic.

#### Scenario: Groups precede real nodes in tier order

- **WHEN** a body carries group nodes of several tiers and real nodes
- **THEN** every cluster group precedes every storage-cluster group, which precede namespace, application and controller groups in that order, each tier sorted by `id`, and all groups precede the first non-group node

### Requirement: Compound `data.parent` assignment

Each real node's `data.parent` SHALL be assigned by node type with skip-absent-levels, using `omitempty` semantics.

#### Scenario: Pod parent

- **WHEN** a `type="pod"` node is serialised
- **THEN** its parent is its controller group id when the pod has a resolved owner; else its application group id when it has a resolved Application; else its namespace group id `<cluster>/namespace/<ns>`
- **AND** every pod with a namespace always has at least the namespace group

#### Scenario: Service and PVC parent

- **WHEN** a `type="service"` or `type="pvc"` node is serialised
- **THEN** its parent is its application group id `<cluster>/namespace/<ns>/application/<app>` when it has a resolved Application; else its namespace group id `<cluster>/namespace/<ns>` (skip-absent-levels)
- **AND** services and PVCs never nest under a `controller` group

#### Scenario: K8s node parent

- **WHEN** a `type="node"` node is serialised
- **THEN** its parent is `cluster/<labels.cluster>`

#### Scenario: NetApp parents

- **WHEN** a NetApp node is serialised
- **THEN** a `type="netapp-node"` node's parent is `storage-cluster/<labels.ontap_cluster>`
- **AND** a `type="netapp-aggr"` node's parent is the **real** NetApp node id `netapp/<labels.ontap_cluster>/<labels.node>` (the aggregate's current owner; an HA takeover moves the parent, never the aggregate's id)
- **AND** a `type="netapp-svm"` node's parent is `storage-cluster/<labels.ontap_cluster>`

#### Scenario: External node parent omitted

- **WHEN** a `type="external"` node is serialised
- **THEN** `parent` is omitted (no cluster identity)

### Requirement: Infrastructure relationships are edges, not nesting

In `GET /v1/graph` the pod→node and pvc→aggregate relationships SHALL be expressed as the edges `pod-to-node` and `pvc-to-netapp-aggr`, and in `GET /v1/storage-graph` every tier relationship as a `storage-flow` edge — **not** as compound nesting. Services and PVCs SHALL NOT be compound parents of pods.

#### Scenario: Infrastructure nodes are edge endpoints

- **WHEN** a body carries K8s `node`, `netapp-aggr` and `netapp-svm` nodes
- **THEN** they carry edges and act as infrastructure-level nodes and edge endpoints
- **AND** the `netapp-node` is a compound parent and, in `/v1/graph`, the target of no edge

### Requirement: Pod `application` and `containers` attributes

Every `data` object for a `type="pod"`, `type="service"`, or `type="pvc"` node SHALL be able to expose an `application` attribute, and every `type="pod"` node SHALL additionally be able to expose a `containers` attribute, all with `omitempty` semantics and all **outside `labels`** (which stays a strict `map[string]string`). The attributes SHALL NOT be encoded as numbers or booleans.

#### Scenario: Application is the resolved ArgoCD Application name

- **WHEN** a node carries `application`
- **THEN** it is a `string`, the node's ArgoCD Application name as resolved by the `cluster-topology-source` capability from the `annotation_argocd_argoproj_io_tracking_id` label that kube-state-metrics derives from the `argocd.argoproj.io/tracking-id` annotation
- **AND** wherever the annotation is the source, all three node types derive the value from the same `<app>:<group>/<kind>:<ns>/<name>` parse

#### Scenario: Pod application source order

- **WHEN** a `type="pod"` node's Application is resolved
- **THEN** it comes from the `app-metadata-source` capability's metric when it names a non-empty Application for the pod (verbatim, see "Pod ArgoCD Application attribute")
- **AND** otherwise from the annotation series of the pod's **controller** (`kube_deployment_annotations`, `kube_statefulset_annotations`, `kube_daemonset_annotations`, `kube_replicaset_annotations`, `kube_job_annotations`, `kube_cronjob_annotations` — see "Pod ArgoCD Application attribute")

#### Scenario: Service and PVC application source

- **WHEN** a `type="service"` / `type="pvc"` node's Application is resolved
- **THEN** it comes from `kube_service_annotations` / `kube_persistentvolumeclaim_annotations` (see "Service and PVC ArgoCD Application resolution")

#### Scenario: Application emitted only when resolved

- **WHEN** a node has no resolved Application
- **THEN** `application` is omitted entirely (never an empty string)

#### Scenario: Application attribute complements the group node

- **WHEN** a pod carries `data.application`
- **THEN** the attribute is **complementary** to the synthesised `type="application"` group node, which is derived from this same value (see "Cytoscape compound node grouping")
- **AND** an existing consumer reading `data.application` on a pod is unaffected in shape, though a pod whose Application previously came from a non-standard pod-level `argocd_tracking_id` label now resolves it from the controller instead

#### Scenario: Containers shape

- **WHEN** a `type="pod"` node has at least one resolved container
- **THEN** `containers` is an array of objects `[{ name: string, image: string }]`, one per container, as resolved by the `cluster-topology-source` capability and ordered deterministically by `(name, image)`
- **AND** a pod with no resolved container omits the field entirely (never an empty array)

#### Scenario: Attributes restricted to their node types

- **WHEN** the body carries other node types
- **THEN** `application` appears only on `type="pod"`, `type="service"`, and `type="pvc"` nodes, and `containers` only on `type="pod"` nodes
- **AND** `type="node"`, `type="external"`, `type="netapp-aggr"`, `type="netapp-node"`, and the synthesised `type="cluster"` / `type="storage-cluster"` / `type="namespace"` / `type="application"` / `type="controller"` group nodes emit neither `application` nor `containers`

#### Scenario: Unresolved node keeps its pre-change shape

- **WHEN** a node has neither a resolved Application nor container info
- **THEN** because both attributes are `omitempty`, its `data` object is byte-identical to the pre-change shape

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

#### Scenario: Pod node carries the metric's application

- **WHEN** the application-metadata metric names Application `shop-v2` for a pod whose Deployment's tracking-id resolves to `checkout`
- **THEN** the corresponding `type="pod"` node carries `data.application: "shop-v2"` and nests under the `<cluster>/namespace/<ns>/application/shop-v2` group

### Requirement: Namespace-filter retention of cluster-scoped infra nodes

`GET /v1/graph` projection SHALL treat `type="node"`, `type="netapp-aggr"`, and `type="netapp-node"` nodes as infrastructure nodes that carry no `namespace` label, and SHALL admit such a node to a response **iff it is referenced by an in-scope element**, through a **transitive** reference chain PVC → aggregate → controller, on every request shape.

#### Scenario: Reference rule per infrastructure type

- **WHEN** the projection admits infrastructure
- **THEN** a `type="node"` node is admitted when some in-scope pod is scheduled on it (its `labels.node`), a `type="netapp-aggr"` node when some in-scope PVC is joined to it via a `pvc-to-netapp-aggr` edge, and a `type="netapp-node"` node when some admitted `netapp-aggr` names it as owner (its `labels.node`)

#### Scenario: Default response carries no orphan infrastructure

- **WHEN** a client sends a default request
- **THEN** the response lists only the host nodes of pods that are in the graph, the aggregates serving in-scope PVCs, and the controllers owning those aggregates
- **AND** it carries no orphan K8s node that hosts no pod, no aggregate serving no in-scope PVC, and no controller owning no admitted aggregate

#### Scenario: Cluster filter on K8s nodes applies twice

- **WHEN** a `cluster` filter is active
- **THEN** it applies to `type="node"` at the source (the `kube_node_*` series are cluster-filtered) and again at projection (the node's own `labels.cluster`)

#### Scenario: NetApp admission is purely reference-driven

- **WHEN** a `?cluster=` or `?namespace=` filter is active
- **THEN** it never admits or excludes the NetApp types directly, because they carry NO Kubernetes `cluster` label and their Harvest series receive no `cluster` or `namespace` matcher
- **AND** a filer shared by two Kubernetes clusters therefore appears in a `?cluster=` view of either cluster (via that cluster's in-scope PVCs)

#### Scenario: prune=false lifts the reference requirement only where no filter could exclude

- **WHEN** a request carries `prune=false`
- **THEN** a `type="node"` node is admitted unreferenced when no `namespace` filter is active (a `cluster` / `az` / `env` filter has already been applied to its own series)
- **AND** a `type="netapp-aggr"` or `type="netapp-node"` node is admitted unreferenced when neither a `cluster` nor a `namespace` filter is active (an `az` / `env` filter has already been applied to its own series)
- **AND** under a `namespace` filter, `prune=false` therefore still yields only the namespace's referenced infrastructure

#### Scenario: Unreferenced aggregate pulls in its controller

- **WHEN** an unreferenced `netapp-aggr` is admitted under `prune=false`
- **THEN** it pulls in its owning `netapp-node`, because the compound parent must exist in the response
- **AND** the former `?name=` exception is withdrawn with the parameter

#### Scenario: No health exception for podless or unreferenced infrastructure

- **WHEN** a K8s node is podless, or an aggregate / controller serves no in-scope PVC
- **THEN** as a **consequence** of this rule, the podless node's `ready_status` / `ipaddress` is absent from the default view and is obtained with `?prune=false` (optionally combined with `cluster`, `az`, `env`)
- **AND** there is no exception that keeps an unhealthy (`NotReady` / `Unknown`) podless node — or a `degraded` aggregate/controller serving no in-scope PVC — in the default view

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

A `type="pvc"` node's `data.labels` SHALL additively carry the plain string entries `volumename` and `svm` whenever they resolve, with NO `data.volumename` or `data.svm` typed field on the PVC node. Each key SHALL be **absent** when its value is unresolved, an empty-string value SHALL never be emitted, and `svm` SHALL never be present without `volumename`.

#### Scenario: volumename source

- **WHEN** a claim's bound PersistentVolume is known
- **THEN** `volumename` is the name of the PersistentVolume bound to the claim (from the `volumename` label of `kube_persistentvolumeclaim_info`, per the `cluster-topology-source` capability)
- **AND** the `volumename` key is distinct from the existing `volume` key (the pod-spec volume name); both MAY appear on the same node

#### Scenario: svm source

- **WHEN** a claim's serving SVM is known
- **THEN** `svm` is the NetApp ONTAP SVM serving the claim, from the `svm` label of the Harvest `volume_labels` series matched by the `netapp-storage-graph` capability's PV-name join
- **AND** the removed Trident custom-resource chain is no longer the source, with the label's shape unchanged

#### Scenario: Additive and deterministic

- **WHEN** the same upstream data is built twice
- **THEN** the label set is byte-identical across rebuilds, the additions being additive to the v1 wire contract
- **AND** responses built from upstreams without the Harvest series are unchanged except for `volumename` appearing wherever `kube_persistentvolumeclaim_info` carries it

#### Scenario: PVC node with a fully-resolved NetApp chain

- **WHEN** the response contains a PVC node whose PV name and SVM resolved (e.g. PV `pvc-9f3a` on SVM `svm-prod`)
- **THEN** its `data.labels.volumename` equals `"pvc-9f3a"`, its `data.labels.svm` equals `"svm-prod"`, and its `data` has no `volumename` or `svm` field outside `labels`

#### Scenario: PVC node with an unresolved chain omits the keys

- **WHEN** the response contains a PVC node whose claim reported no `volumename` (or whose Harvest join did not resolve)
- **THEN** its `data.labels` has no `volumename` key (or carries `volumename` but no `svm` key, respectively); neither key is ever present with an empty-string value

#### Scenario: volume and volumename are distinct keys

- **WHEN** the response contains a PVC node that derives a pod-spec volume name `data` and a bound PV name `pvc-9f3a`
- **THEN** `data.labels.volume` equals `"data"` and `data.labels.volumename` equals `"pvc-9f3a"` — the two keys coexist and carry different values

### Requirement: PVC `aggr` label

A `type="pvc"` node's `data.labels` SHALL additively carry an `aggr` entry whenever the claim's volume resolved a single containing aggregate, valued with the **node id** of that `netapp-aggr` node (`netapp/<ontap_cluster>/aggr/<aggr>`), not the bare aggregate name. It follows the rules of "PVC `volumename` and `svm` labels": a plain `labels` entry with no typed `data.aggr` field, **absent** when unresolved, and never an empty string.

#### Scenario: Node id because aggregate names are cluster-local

- **WHEN** the label is emitted
- **THEN** it carries the node id because aggregate names are unique only within an ONTAP cluster, and the label must name exactly one node

#### Scenario: Label, node and edge agree

- **WHEN** a `GET /v1/graph` response carries the label
- **THEN** it names the aggregate the claim's `pvc-to-netapp-aggr` edge is drawn to, so the response also carries that node and that edge, and the three can never disagree
- **AND** `GET /v1/storage-graph`, which draws no such edge, carries the same label under the `storage-graph-api` capability

#### Scenario: Absent when no aggregate resolved

- **WHEN** the claim resolved no aggregate — a FlexGroup volume spanning aggregates, a claim whose volume matched no `volume_labels` series, or an upstream without the Harvest series
- **THEN** `aggr` is absent
- **AND** `aggr` is never present without `volumename` (the join is rooted at the PV name)

#### Scenario: Independent of svm

- **WHEN** a joined series carries an empty `svm` label, or a FlexGroup series resolves `svm`
- **THEN** the empty-`svm` series still resolves the aggregate, and the FlexGroup series resolves `svm` with no aggregate

#### Scenario: Additive and deterministic

- **WHEN** the same upstream data is built twice
- **THEN** the label set is byte-identical across rebuilds, the addition being additive to the v1 wire contract

#### Scenario: A resolved claim names its aggregate node

- **WHEN** the response contains a PVC node whose volume resolved to aggregate `aggr1` on ONTAP cluster `ontap-prod`, served by SVM `svm_shop`
- **THEN** its `data.labels.aggr` equals `"netapp/ontap-prod/aggr/aggr1"`, its `data.labels.svm` equals `"svm_shop"`, the response contains the `netapp/ontap-prod/aggr/aggr1` node, and the PVC's `pvc-to-netapp-aggr` edge targets that node

#### Scenario: The same aggregate name on two filers

- **WHEN** two PVC nodes resolve to aggregates both named `aggr1`, one on ONTAP cluster `ontap-a` and one on `ontap-b`
- **THEN** their `data.labels.aggr` values are `"netapp/ontap-a/aggr/aggr1"` and `"netapp/ontap-b/aggr/aggr1"` respectively

#### Scenario: A FlexGroup claim carries svm but no aggr

- **WHEN** the response contains a PVC node whose matched `volume_labels` series carries `svm="svm_big"` and an empty `aggr`
- **THEN** its `data.labels.svm` equals `"svm_big"` and its `data.labels` has no `aggr` key

#### Scenario: No Harvest series, no aggr

- **WHEN** the upstream carries no `volume_labels` series for the window
- **THEN** no PVC node's `data.labels` has an `aggr` key

### Requirement: Edge `metrics` attribute

An edge's `data` MAY carry an optional `metrics` object (`omitempty`) holding the edge's measurements for the requested window, and the key SHALL be **absent entirely** — never `null`, never an empty object — on every edge that has no measurements. The object is a **union of two disjoint families** (RED and I/O), and a single edge SHALL only ever carry fields from ONE family.

#### Scenario: Family determined by provenance

- **WHEN** an edge carries `data.metrics`
- **THEN** its family is determined by the edge's provenance, so cross-family mixing is structurally impossible

#### Scenario: Every union field optional at the schema level

- **WHEN** the OpenAPI schema describes `metrics`
- **THEN** every field of the union is optional — a consequence the OpenAPI schema reflects by moving `rate` from required to optional
- **AND** the RED invariant is preserved intact: a RED-family `metrics` object always carries a positive `rate`

#### Scenario: Metrics never perturb identity or ordering

- **WHEN** an edge gains or loses `metrics`
- **THEN** its `id`, `type`, `source`, `target`, and `labels` are unaffected, and node and edge ordering is unaffected

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

### Requirement: RED metrics family

The **RED family** SHALL appear only on trace-derived call edges, under the presence rule defined by the `pod-service-graph` capability (in short: only trace-derived edges whose `source` and `target` both resolved to a pod or a service node, excluding the ingress chain's entry hop and any measurement derived from span-link series).

#### Scenario: rate

- **WHEN** an edge carries a RED-family object
- **THEN** it carries `rate` (number, REQUIRED **within this family**, strictly greater than zero) — requests per second over the window

#### Scenario: error_rate

- **WHEN** the RED object is rendered
- **THEN** `error_rate` (number, OPTIONAL, absence semantics) is the failed fraction in `[0, 1]`: absent when the upstream failure counter could not be read; `0` when it was read and reported no failures

#### Scenario: p90_server_ms

- **WHEN** the RED object is rendered
- **THEN** `p90_server_ms` (number, OPTIONAL, absent when unavailable) is the 90th percentile server-observed request duration in milliseconds
- **AND** the quantile and observation side match Grafana's documented service-graph queries by definition, but the values are not expected to equal Grafana's numerically, because Grafana aggregates by service name while this API aggregates by pod pair

### Requirement: I/O metrics family

The **I/O family** SHALL appear on `pvc-to-netapp-aggr` edges only, under the presence rule defined by the `netapp-storage-graph` capability: each field present iff its own Harvest family matched, the six measurements coming from the Harvest QoS workload families and the two ceilings from the QoS fixed-policy families.

#### Scenario: Measurement fields

- **WHEN** an edge carries an I/O-family object
- **THEN** `read_ops`, `write_ops` (numbers, OPTIONAL) are read/write requests per second, verbatim from Harvest
- **AND** `read_latency_us`, `write_latency_us` (numbers, OPTIONAL) are average read/write latency in microseconds, verbatim from Harvest
- **AND** `read_bytes_per_sec`, `write_bytes_per_sec` (numbers, OPTIONAL) are read/write throughput in bytes per second, verbatim from Harvest

#### Scenario: Ceiling fields

- **WHEN** the volume declares a QoS throughput ceiling
- **THEN** `max_iops`, `max_bytes_per_sec` (numbers, OPTIONAL) carry it: `max_iops` verbatim from Harvest, `max_bytes_per_sec` converted from the policy's megabytes-per-second figure so that it carries the same unit as the measured throughput fields and the two compare directly

#### Scenario: Absent ceiling

- **WHEN** a ceiling field is absent
- **THEN** absence means *no declared ceiling* — it is never rendered as `0` or as an "unlimited" sentinel
- **AND** neither ceiling field can appear unless at least one measurement field does

### Requirement: Metric value encoding

All `metrics` values SHALL be JSON numbers, never strings, each rounded to a fixed number of **significant digits** — not decimal places — so that the "Deterministic response body" requirement continues to hold byte-for-byte while a non-zero value can never be rendered as `0`.

#### Scenario: Exponent form is legal

- **WHEN** a value is very small
- **THEN** it MAY appear in JSON exponent form (for example `3.86e-7`), which is legal JSON
- **AND** consumers MUST NOT assume a fixed-decimal rendering, and MUST treat `0` as semantically distinct from a very small non-zero value

### Requirement: NetApp aggregate and node payloads

`GET /v1/graph` SHALL emit each NetApp aggregate and each NetApp node (materialised by the `netapp-storage-graph` capability) as real first-class nodes `{ data: { id, name, type, labels } }`. Neither type SHALL carry a `cluster` label, so both stay out of `clusters[]` and out of the `?cluster=` domain, and neither SHALL emit `ipaddress`, `owner`, `application`, `containers`, `ready_status`, `provisioner`, or `parameters`.

#### Scenario: Aggregate identity

- **WHEN** a `type="netapp-aggr"` node is emitted
- **THEN** its `id` is `netapp/<ontap-cluster>/aggr/<aggr>` (aggregate names are cluster-wide unique within an ONTAP cluster; the id excludes the owning node so an HA takeover never changes it) and its `name` equals the ONTAP aggregate name (`<aggr>`)
- **AND** its `labels` is a strict `map[string]string` containing exactly `ontap_cluster` and `node` (the current owning controller, which drives `data.parent`) — deliberately no `cluster` key

#### Scenario: Aggregate health

- **WHEN** an aggregate is emitted
- **THEN** its `data` MAY carry, with `omitempty` semantics and **outside `labels`**, `health` — a `string`, exactly one of `"online"` or `"degraded"`, per the `netapp-storage-graph` capability's per-aggregate `aggr_new_status` read
- **AND** `health` is omitted entirely when the aggregate has no status data — absence of data is distinct from a reported unhealthy state and the two are never conflated

#### Scenario: Aggregate usage

- **WHEN** an aggregate's space series resolve
- **THEN** its `data` MAY carry `usage` — an object `{used_bytes, capacity_bytes}` of JSON numbers (bytes) from `aggr_space_used` / `aggr_space_total`, the **same shape** as the PVC `usage` attribute
- **AND** `usage` is present iff at least one field resolved, an unresolved field is omitted, and values are never strings

#### Scenario: Controller identity

- **WHEN** a `type="netapp-node"` node is emitted
- **THEN** its `id` is `netapp/<ontap-cluster>/<node>` (ONTAP controller names are not globally unique across ONTAP clusters) and its `name` equals the ONTAP controller name (`<node>`)
- **AND** its `labels` is a strict `map[string]string` containing exactly an `ontap_cluster` entry — deliberately no `cluster` key

#### Scenario: Controller health

- **WHEN** a controller is emitted
- **THEN** its `data` MAY carry `health` (`omitempty`, outside `labels`) — a `string`, exactly one of `"online"` or `"degraded"`, from `node_new_status` per the `netapp-storage-graph` capability
- **AND** `health` is omitted when the controller has no status data, never a defaulted `"degraded"`

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

A `type="pvc"` node's `data` MAY carry two typed attributes, `storageclass` and `usage`, both with `omitempty` semantics, both **outside `labels`**, and neither encoded as a string map entry. Both attributes SHALL appear only on `type="pvc"` nodes.

#### Scenario: storageclass value

- **WHEN** the claim's StorageClass name resolved non-empty, as resolved by the `cluster-topology-source` capability ("PVC StorageClass resolution")
- **THEN** `storageclass` is that `string`; otherwise it is omitted entirely (never an empty string)

#### Scenario: storageclass supersedes the former prohibition

- **WHEN** a PVC carries `data.storageclass`
- **THEN** this **supersedes** the previous rule that no `data.storageclass` field exists — the StorageClass node that rule pointed to is removed
- **AND** the name is retained on the claim itself as the operator's discriminator for the `netapp-storage-graph` join-coverage signal

#### Scenario: usage value

- **WHEN** kubelet volume-stats series resolve for the claim
- **THEN** `usage` is an object `{ used_bytes, capacity_bytes }` of JSON numbers (bytes), from the kubelet volume-stats series per the `cluster-topology-source` capability
- **AND** the object is present iff at least one field resolved, an unresolved field is omitted from the object, and values are never strings

#### Scenario: Unresolved PVC keeps its pre-change shape

- **WHEN** a PVC has neither attribute resolved
- **THEN** because both are `omitempty`, its `data` object is byte-identical to the pre-change shape (modulo the removed edge/node)

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

`GET /v1/graph` SHALL accept the optional, repeatable parameters `az` and `env`, each rendered as an upstream label matcher on every kube-state-metrics and kubelet query of the build, on **no** NetApp Harvest query, and on no service-graph query; the `up{}` probe SHALL never carry them. The two filters SHALL narrow **at the source**.

#### Scenario: az routes Harvest backends

- **WHEN** a request carries `az`
- **THEN** `az` additionally selects which `harvest` backends are asked (the `upstream-backend-routing` capability's zone rule); that selection is the only effect `az` has on the Harvest legs, and `env` has none

#### Scenario: Configured label keys

- **WHEN** the parameters are rendered
- **THEN** the upstream label each parameter binds to is the operator-configured key of the `cluster-topology-source` capability ("Configurable `az` / `env` label keys"), defaulting to `az` and `env`
- **AND** the request parameter names themselves are fixed

#### Scenario: Labels are the identity components

- **WHEN** a cluster's series carry both labels
- **THEN** the same two labels are the zone and environment components of the cluster's identity (`cluster-topology-source`, "Cluster identity composed from zone and environment labels"): the cluster is listed in `clusters[]` and prefixed on its ids as `<az>-<env>-<cluster>`
- **AND** the filter triple `az` + `env` + `cluster` therefore addresses exactly one listed identity

#### Scenario: Series lacking the label is absent from the build

- **WHEN** a filter is active and a series lacks the configured label
- **THEN** the series does not match an equality matcher and is therefore absent from the build
- **AND** the response `clusters` list, derived from the built graph's node `cluster` labels, therefore lists only the cluster identities with data in the requested zone / environment

#### Scenario: Operator stamps every Kubernetes family

- **WHEN** an operator deploys the server
- **THEN** the operator SHALL ensure every kube-state-metrics and kubelet family stamps both labels
- **AND** a family that does not vanishes from every `az` / `env`-filtered request; because the default projection keeps only connectivity-connected workload, a topology family missing the label yields an empty graph for that filter rather than a partial one — and, unfiltered, such a family resolves its cluster through the identity ladder rather than composing it
- **AND** the Harvest families are exempt: they carry no matcher, so they need no label

#### Scenario: Environment filter selects one environment's clusters

- **WHEN** the upstream holds `cluster-prod-1` (all series `env="prod"`) and `cluster-dev-1` (all series `env="dev"`) and a client sends `?env=prod`
- **THEN** every kube-state-metrics and kubelet query carries `env="prod"`, the response contains only `cluster-prod-1` workload and infrastructure, and `clusters` is `["cluster-prod-1"]`

#### Scenario: Zone and environment are AND-combined

- **WHEN** `cluster-a` carries `az="zone-a",env="prod"`, `cluster-b` carries `az="zone-b",env="prod"`, and a client sends `?az=zone-a&env=prod`
- **THEN** the response contains `cluster-a` only

#### Scenario: Filtered response lists the composed identity

- **WHEN** `c1` carries `az="us",env="dev"` on every series and a client sends `?az=us&env=dev`
- **THEN** the response's `clusters` is `["us-dev-c1"]`, every id is prefixed `us-dev-c1/`, and the cluster group node is `cluster/us-dev-c1`

#### Scenario: Configured key is used in the matcher

- **WHEN** the server runs with `KSG_AZ_LABEL=topology_zone` and a client sends `?az=zone-a`
- **THEN** the rendered matcher is `topology_zone="zone-a"`, and the request parameter is still named `az`

#### Scenario: Family lacking the label vanishes under the filter

- **WHEN** the kube-state-metrics series carry `env="prod"` but the kubelet series carry no `env` label, and a client sends `?env=prod`
- **THEN** the response contains the prod pods, nodes, and claims but no claim carries kubelet usage (the kubelet legs returned nothing), and the build does not fail

#### Scenario: Harvest lacking the label still joins under the filter

- **WHEN** the kube-state-metrics and kubelet series carry `env="prod"`, the Harvest series carry no `env` label, and a client sends `?env=prod`
- **THEN** the Harvest legs are issued without an `env` matcher and return their rows, so the prod claims that join a `volume_labels` series receive their `netapp-aggr` / `netapp-node` nodes and `pvc-to-netapp-aggr` edges

### Requirement: Node `alerts` attribute

Each `type="pod"`, `type="node"`, `type="pvc"`, `type="netapp-node"` and `type="netapp-aggr"` node MAY carry a typed `data.alerts` array of `{ name, state, severity }` objects — the active alerts the `alert-overlay` capability matched to it — which SHALL be serialised with `omitempty` semantics and **never inside `labels`** (the `ipaddress` / `owner` / `ready_status` precedent).

#### Scenario: Entry order and de-duplication

- **WHEN** a node carries alerts
- **THEN** the array is sorted by `name` then `severity` and de-duplicated on that pair, and `severity` is omitted from an entry when empty

#### Scenario: Kinds that never carry alerts

- **WHEN** a body contains `service`, `external`, `netapp-svm` or synthesised group nodes
- **THEN** none of them carries `alerts`

#### Scenario: Both graph endpoints

- **WHEN** a node is served by `GET /v1/graph` or `GET /v1/storage-graph`
- **THEN** the attribute appears on either alike, since it is resolved onto the built graph

#### Scenario: Alerted pod serialised

- **WHEN** the build attached alerts `KubePodCrashLooping` (`warning`) and `HighMemory` (`critical`) to a pod
- **THEN** the pod's `data.alerts` equals `[{"name":"HighMemory","state":"firing","severity":"critical"},{"name":"KubePodCrashLooping","state":"firing","severity":"warning"}]` and `labels` carries no alert key

#### Scenario: Unalerted node has no key

- **WHEN** a node matched no alert
- **THEN** its `data` object has no `alerts` key, so a body with no alerts differs from the pre-change golden only by the `status` keys the "Node `status` attribute" requirement adds

### Requirement: Node `status` attribute

Each `type="pod"`, `type="node"`, `type="pvc"`, `type="netapp-node"` and `type="netapp-aggr"` node SHALL carry a typed `data.status` string — exactly one of `"normal"`, `"warning"`, `"critical"` — **always present** on those five kinds and **never inside `labels`**. The value SHALL be folded at build time, worst-wins, from the node's OWN attributes and nothing else: the highest rank reached, else `normal`.

#### Scenario: Kinds without a health signal carry no key

- **WHEN** a body carries `service`, `external`, `netapp-svm` or synthesised group nodes
- **THEN** they carry no `status`, so a consumer can tell "judged, nothing wrong" (`normal`) from "this kind carries no health signal" (no key)

#### Scenario: Alert severity ranks

- **WHEN** a node carries `data.alerts`
- **THEN** an entry whose `severity` (compared case-insensitively) is `critical` ranks critical; `warning`, an empty or absent `severity`, or any unrecognised value ranks warning; `info` and `none` rank nothing

#### Scenario: Health and readiness ranks

- **WHEN** a node carries `data.health` or `data.ready_status`
- **THEN** `data.health` `degraded` ranks critical; `data.ready_status` `NotReady` ranks critical and `Unknown` ranks warning
- **AND** every other value, and every absent attribute, ranks nothing

#### Scenario: perf does not influence status

- **WHEN** a node carries `data.perf`
- **THEN** `data.perf` does not influence `status`: a threshold over a performance counter is an alert rule's job and reaches `status` through `data.alerts`

#### Scenario: Fold is local and pure

- **WHEN** the fold runs
- **THEN** it is a pure function of the node's own attributes (order-free over the alert set) and never reads another node, so status does not propagate along edges or compound parents
- **AND** roll-up onto compound groups (a collapsed namespace showing its worst pod) is NOT performed by the server

#### Scenario: Identical across entry points

- **WHEN** the same node is produced by `GET /v1/graph`, `GET /v1/storage-graph` or an in-process engine call
- **THEN** the attribute appears identically on every one

#### Scenario: Worst severity wins

- **WHEN** a pod carries alerts with severities `warning` and `critical`
- **THEN** its `data.status` equals `"critical"`

#### Scenario: Warning only

- **WHEN** a pod's only alert carries `severity="warning"`
- **THEN** its `data.status` equals `"warning"`

#### Scenario: Unrecognised or missing severity is a warning

- **WHEN** one PVC's only alert carries `severity="page"` and another PVC's only alert carries no `severity`
- **THEN** both carry `data.status="warning"`

#### Scenario: Informational alerts do not tint

- **WHEN** a pod's only alerts carry `severity="info"` and `severity="none"`
- **THEN** its `data.status` equals `"normal"`

#### Scenario: Unalerted node is explicitly normal

- **WHEN** a pod matched no alert and its `data` has no `alerts` key
- **THEN** its `data.status` equals `"normal"` and the key is present

#### Scenario: Degraded controller

- **WHEN** a `netapp-node` carries `data.health="degraded"` and no alert
- **THEN** its `data.status` equals `"critical"`

#### Scenario: Status does not propagate

- **WHEN** a `netapp-aggr` carries `data.health="degraded"` and its owning `netapp-node` carries `data.health="online"`, neither with an alert
- **THEN** the aggregate's `data.status` equals `"critical"` and the controller's equals `"normal"`

#### Scenario: Kubernetes node readiness

- **WHEN** one `type="node"` carries `data.ready_status="NotReady"`, a second `"Unknown"`, and a third has no `ready_status`, none with an alert
- **THEN** their `data.status` values are `"critical"`, `"warning"` and `"normal"` respectively

#### Scenario: High CPU alone is normal

- **WHEN** a `netapp-node` carries `data.perf.cpu_busy_pct=99`, `data.health="online"` and no alert
- **THEN** its `data.status` equals `"normal"`

#### Scenario: Kinds without a signal carry no key

- **WHEN** a body contains `service`, `external`, `netapp-svm` and synthesised group nodes
- **THEN** none of their `data` objects has a `status` key

#### Scenario: Present on both endpoints

- **WHEN** a node is retained by both `GET /v1/graph` and `GET /v1/storage-graph`
- **THEN** both bodies carry the identical `data.status` on it

### Requirement: NetApp node `hardware` and `perf` attributes

Each `type="netapp-node"` node MAY carry two typed, nullable objects, `data.hardware` and `data.perf`, both `omitempty` and never inside `labels`, per the `netapp-storage-graph` "NetApp node entity and health" requirement. No other node type SHALL carry either, and both SHALL appear on `GET /v1/graph` and `GET /v1/storage-graph` alike.

#### Scenario: hardware shape

- **WHEN** the Harvest `node_labels` info series resolves for a controller
- **THEN** `data.hardware = { model, serial, version, vendor, location }` (strings, each omitted when unresolved)

#### Scenario: perf shape

- **WHEN** the Harvest `system_node` counters resolve for a controller
- **THEN** `data.perf = { cpu_busy_pct, total_ops, total_latency_us, total_bytes_per_sec }` (JSON numbers rounded to 6 significant digits, each omitted when unresolved)

#### Scenario: perf never drives health or status

- **WHEN** a controller carries `perf`
- **THEN** `data.health` on the same node remains the `node_new_status`-reported value and is not derived from `perf`
- **AND** `data.status` (see "Node `status` attribute") does not read `perf`

#### Scenario: Hardware and perf serialised

- **WHEN** a controller resolves `model="AFF-A400"`, `version="9.14.1"`, `cpu_busy_pct=72.5` and `total_ops=18500`
- **THEN** its `data.hardware` equals `{"model":"AFF-A400","version":"9.14.1"}` and its `data.perf` equals `{"cpu_busy_pct":72.5,"total_ops":18500}`

#### Scenario: Absent attributes omitted

- **WHEN** neither `node_labels` nor any `system_node` counter matches a controller
- **THEN** its `data` has neither a `hardware` nor a `perf` key

### Requirement: PVC `qos` attribute

A `type="pvc"` node's `data` MAY carry a typed `qos` object — the declared QoS throughput ceiling of the policy group governing the claim's volume, as resolved by the `netapp-storage-graph` capability ("QoS fixed-policy throughput ceilings") — serialised with `omitempty`, **outside `labels`**, and never encoded as a string map entry. The attribute SHALL appear only on `type="pvc"` nodes.

#### Scenario: Object fields

- **WHEN** a PVC carries `qos`
- **THEN** `policy_group` — a `string`, the policy group the ceiling was keyed on — is always present when the object is present, never empty
- **AND** `max_iops` — a JSON number, requests per second — is present iff the policy group's `qos_policy_fixed_max_throughput_iops` resolved
- **AND** `max_bytes_per_sec` — a JSON number, bytes per second (the policy's MB/s figure × 1048576, the unit of the edge's `read_bytes_per_sec`) — is present iff the policy group's `qos_policy_fixed_max_throughput_mbps` resolved

#### Scenario: Present iff a ceiling resolved

- **WHEN** neither `max_iops` nor `max_bytes_per_sec` resolved — no `svm`, no policy group on any in-scope workload, no fixed-policy series for the triple, no Harvest series in the window
- **THEN** the claim carries no `qos` key at all, never an empty object, a `0`, or an "unlimited" sentinel

#### Scenario: Presence independent of measurements and edges

- **WHEN** a claim's ceiling resolved but it has no I/O measurement or no `pvc-to-netapp-aggr` edge
- **THEN** it still carries `qos` — including a FlexGroup claim (SVM resolved, no aggregate) and a claim whose edge carries no measurement

#### Scenario: Copy of the edge ceiling, not a replacement

- **WHEN** the claim's `pvc-to-netapp-aggr` edge carries ceiling fields in `data.metrics`
- **THEN** the edge's `max_iops` / `max_bytes_per_sec` keep their own presence rule, and whenever both the node and the edge carry a field the two values are equal

#### Scenario: Rounding

- **WHEN** the attribute is serialised
- **THEN** both numbers are rounded to 6 significant digits exactly as edge metrics are, and MAY appear in exponent form

#### Scenario: Additive and deterministic

- **WHEN** a PVC has no resolved ceiling, or the same upstream data is built twice
- **THEN** the PVC's `data` object is byte-identical to the pre-change shape, the attribute being additive to the v1 wire contract
- **AND** for identical upstream data the attribute is byte-identical across rebuilds

#### Scenario: PVC node carries its ceiling

- **WHEN** the response contains a PVC node whose volume is governed by policy group `gold-tier` with `max_throughput_iops = 5000` and `max_throughput_mbps = 250`
- **THEN** the node carries `data.qos: {"policy_group": "gold-tier", "max_iops": 5000, "max_bytes_per_sec": 262144000}`, and `data.labels` contains no `policy_group`, `max_iops` or `max_bytes_per_sec` key

#### Scenario: Node and edge agree

- **WHEN** a PVC node carries `data.qos.max_iops` and its `pvc-to-netapp-aggr` edge carries `data.metrics.max_iops`
- **THEN** the two values are equal, and likewise for `max_bytes_per_sec`

#### Scenario: Unresolved ceiling omits the attribute

- **WHEN** the response contains a PVC node whose volume is in no policy group, or that joined no Harvest series
- **THEN** its `data` has no `qos` key

#### Scenario: Partial ceiling keeps only the resolved field

- **WHEN** a PVC's policy group resolved `max_throughput_iops` but no `max_throughput_mbps` series
- **THEN** its `data.qos` equals `{"policy_group": <name>, "max_iops": <value>}` with no `max_bytes_per_sec` key

#### Scenario: FlexGroup PVC carries a ceiling without an edge

- **WHEN** a PVC's volume resolved an SVM but no aggregate, and its policy group's ceiling resolved
- **THEN** the PVC node carries `data.qos` and the response contains no `pvc-to-netapp-aggr` edge from it

#### Scenario: Only PVC nodes carry qos

- **WHEN** the response contains nodes of any type other than `pvc`
- **THEN** none of them carries a `qos` field

### Requirement: Namespace and application group metadata

When the `app-metadata-source` capability resolves a namespace owner for a `namespace` group's `(cluster, namespace)`, that group SHALL carry `data.namespace_owner`; when it resolves a deployer for an `application` group's `(cluster, namespace, application)`, that group SHALL carry `data.deployed_by`. Both are `omitempty` strings outside `labels`, which stay `{}`; no other node type carries either.

#### Scenario: Namespace group carries its owner

- **WHEN** the metric resolves owner `team-a` for namespace `shop` in cluster `us-dev-c1` and the response contains that namespace group
- **THEN** the node `us-dev-c1/namespace/shop` carries `data.namespace_owner: "team-a"` and `data.labels` is `{}`

#### Scenario: Application group carries its deployer

- **WHEN** the metric resolves deployer `alice` for Application `checkout` in namespace `shop` of cluster `us-dev-c1`
- **THEN** the node `us-dev-c1/namespace/shop/application/checkout` carries `data.deployed_by: "alice"`

#### Scenario: Group exists only when the view needs it

- **WHEN** the metric resolves an owner for namespace `idle` but the projected view holds no pod, service or PVC in `idle`
- **THEN** the response contains no `idle` namespace group

#### Scenario: Unresolved values are omitted

- **WHEN** a namespace group or application group has no resolved owner or deployer, or the metric is unset
- **THEN** its `data` object carries no `namespace_owner` or `deployed_by` key, and the body is byte-identical to the pre-change shape

#### Scenario: Application group from the controller chain still gets its deployer

- **WHEN** every pod of Application `checkout` resolved it through the controller chain and a metric row carries `app="checkout"`, `user="alice"` in the same cluster and namespace
- **THEN** the `checkout` application group carries `data.deployed_by: "alice"`

#### Scenario: Real nodes never carry the group fields

- **WHEN** the response contains pod, service, PVC, K8s node, external or NetApp nodes
- **THEN** none of their `data` objects carries `namespace_owner` or `deployed_by`
