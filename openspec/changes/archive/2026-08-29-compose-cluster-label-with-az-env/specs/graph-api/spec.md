## MODIFIED Requirements

### Requirement: Cytoscape.js response shape

`GET /v1/graph` SHALL return a JSON document in Cytoscape.js shape: `{ apiVersion, clusters, elements: { nodes, edges } }`. The body SHALL NOT contain time-varying or echo-of-input fields, so identical inputs against the same upstream state produce byte-identical bodies. The top-level `clusters` array SHALL list **Kubernetes** cluster identities only — the `<az>-<env>-<cluster>` identity of the `cluster-topology-source` capability ("Cluster identity composed from zone and environment labels"), or the raw name for a cluster that composed none — and an ONTAP cluster name (the `ontap_cluster` label of `netapp-aggr` / `netapp-node` nodes) SHALL NEVER appear in it. An identity is NOT a valid `?cluster=` value (see "Filter parameters"); a client that wants to narrow to one listed cluster sends its three components as `az`, `env` and `cluster`.

Each **node** SHALL be `{ data: { id, name, type, labels } }`:
- `id` SHALL be a cluster-scoped composite for pods / K8s nodes / PVCs / services (pods: `<cluster>/<pod-uid>`; nodes: `<cluster>/<node-name>`; PVCs: `<cluster>/<namespace>/<claim>`; services: `<cluster>/<namespace>/<service>`), where `<cluster>` is the cluster identity. For NetApp aggregates, `id` SHALL be `netapp/<ontap-cluster>/aggr/<aggr>`; for NetApp nodes, `netapp/<ontap-cluster>/<node>` (neither carries a Kubernetes cluster prefix). For external nodes (unresolvable `"://"` connection-string endpoints or missing-UID human-label fallback), `id` SHALL be `external/<label-value>` (no cluster prefix).
- `name` SHALL be the human-readable pod / node / PVC / service name; for NetApp aggregates, the ONTAP aggregate name; for NetApp nodes, the ONTAP controller name. For external nodes, `name` SHALL be the verbatim `client` or `server` label value from the source service-graph series.
- `type` SHALL be one of the strings `"pod"`, `"node"`, `"pvc"`, `"service"`, `"external"`, `"netapp-aggr"`, `"netapp-node"`. The Cytoscape serialiser additionally synthesises `"cluster"`, `"storage-cluster"`, `"namespace"`, `"application"`, and `"controller"` group nodes for compound nesting (see "Cytoscape compound node grouping").
- `data` MAY carry an optional `parent` field (`omitempty`) referencing the `id` of the node's Cytoscape compound container — see "Cytoscape compound node grouping".
- `labels` SHALL be a JSON object whose values are strings only (`map[string]string`). For pod / K8s node / PVC / service nodes it SHALL include at minimum a `cluster` entry carrying the cluster identity — the same value as the `id` prefix; for pods, PVCs, and services it SHALL also include a `namespace` entry; for pods it SHALL include `node` (the cluster-scoped node ID, identity-prefixed), and SHALL include `pod_ip` and `host_ip` whenever the upstream `kube_pod_info` series carried them; for K8s nodes it SHALL include `external_ip` when the upstream provided one. **For NetApp aggregates**, `labels` SHALL be exactly `{ontap_cluster, node}` (the owning controller); **for NetApp nodes**, exactly `{ontap_cluster}` — deliberately no `cluster` key on either. **For external nodes**, `labels` SHALL be an empty object `{}` (no `cluster` key).

Each **edge** SHALL be `{ data: { id, type, source, target, labels } }`:
- `id` SHALL be a UUID, RFC 4122 compliant, encoded as a lowercase canonical string.
- `type` SHALL be one of the registered edge types from `/v1/edge-types`.
- `source` and `target` SHALL each match the `id` of a node present in the same response's `elements.nodes`.
- `labels` SHALL be a JSON object whose values are strings only (`map[string]string`). The exact key set per edge type is defined by the `pod-service-graph`, `cluster-topology-source`, and `netapp-storage-graph` capabilities. Wherever an edge carries a `cluster` key its value is a cluster identity, never a raw name that appears on no node of the body.
- `data` MAY carry an optional `metrics` object (`omitempty`) holding the edge's measurements — see "Edge `metrics` attribute".

Implementations SHALL NOT encode booleans or numbers as strings inside `labels`. Boolean flags remain deferred to a future typed field and are NOT part of the v1 contract. Numeric measurements are carried exclusively on the typed `data.metrics` object defined below — never inside `labels`.

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

### Requirement: Filter parameters

`GET /v1/graph` SHALL accept the optional filter parameters `cluster`, `namespace`, `az`, `env`, `edge_type` (each repeatable) and `prune` (single-valued, `true` | `false`, default `true`). The request surface is exactly `start`, `end`, `cluster`, `namespace`, `az`, `env`, `edge_type`, `prune`; every parameter except `start` / `end` is optional. Multiple values for the same parameter SHALL be OR-combined; different parameters SHALL be AND-combined. An unknown filter **value** (a cluster, namespace, zone, or environment with no data) SHALL NOT cause an error — it yields an empty result. An unknown filter **parameter** (including the withdrawn `name`, `root`, `depth`, `direction`) SHALL be ignored without error.

The `cluster` value is the **raw** Kubernetes cluster name — the value of the upstream `cluster` label — not the composed identity the response lists in `clusters[]`. A raw name selects every cluster identity whose raw component equals it, across zones and environments; the triple `az` + `env` + `cluster` pins exactly one identity, because those three request dimensions are the identity's three components.

Filters fall into two classes:

- **Selector-level filters** — `cluster`, `namespace`, `az`, `env` — SHALL be rendered into the upstream PromQL queries of the build as label matchers, so the graph is narrowed by VictoriaMetrics before any sample is read. Which matcher reaches which series is the hardcoded contract of the `cluster-topology-source` capability ("Request-scoped upstream selectors"); the service-graph series are deliberately read unfiltered (`pod-service-graph`). A request with no selector-level filter SHALL issue exactly the queries it issues today and produce a byte-identical body.
- **Projection-level filters** — `cluster` and `namespace` (applied again over the built graph as defence in depth), `edge_type`, and `prune` — SHALL be applied at response time as a projection over the freshly built graph. The projection-level `cluster` check compares the request's raw values against the **raw-name component** of each element's cluster identity (recovered from the built graph's identity table; an identity absent from the table compares as itself), so the projection admits exactly what the upstream matcher admitted.

Empty filters SHALL return the **connectivity-connected subgraph** of the full multi-cluster graph for the time window (the default connectivity prune — see "Default projection prunes connectivity-disconnected workload"); it is NOT the full topology inventory. `prune=false` returns the inventory instead.

Selector-level values SHALL be validated before rendering: a value longer than 253 bytes, containing a control character (including newline), or — for `prune` — not exactly `true` / `false`, SHALL be rejected with `400 Bad Request` and `reason: "invalid_scope"`. A single value SHALL render an exact matcher (`<key>="<value>"`, with `"` and `\` escaped); several values for one parameter SHALL render one fully-anchored alternation (`<key>=~"<v1>|<v2>"`) over the sorted, de-duplicated, regex-quoted values. The request value `cluster=unknown` SHALL render `cluster=~"unknown|"` so that both spellings of the bucket — a series carrying no `cluster` label and one whose label is literally `unknown` — remain addressable, matching what the projection filter accepts.

**Edge retention rule (unified across all filters).** An edge SHALL be retained when at least one resolved endpoint is in scope after node filtering. When exactly one endpoint is in scope, the missing endpoint SHALL be re-added from the freshly built graph's node index provided it passes the non-cluster filters (namespace check; types without a namespace label — `node`, `external`, and the NetApp types `netapp-aggr` / `netapp-node` (which carry neither a namespace nor a `cluster` label) — pass through). This single rule is edge-type-agnostic and covers non-pod endpoints incident on in-scope pods — including the topology `pod-to-node` edge, the `pvc-to-netapp-aggr` edge, and the `external` partners that a filtered build produces for out-of-scope peers — and, in an **unfiltered** build, cross-cluster edges whose partner endpoint lies outside a projection-level `cluster` set.

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

### Requirement: Cross-cluster edge representation

A cross-cluster edge (`pod-calls-pod`, `pod-calls-service`, or `service-selects-pod` whose source-node cluster **identity** differs from its target-node cluster identity) SHALL be emitted with **both real endpoint nodes** only when both clusters are **loaded** by the build — every build without a `cluster` filter, or a build whose `cluster` filter lists both clusters' raw names. Consumers detect cross-cluster status by comparing the `labels.cluster` of the edge's resolved source and target nodes — not from edge labels. Two clusters sharing a raw name under different zones or environments are distinct identities and an edge between them IS cross-cluster. A `pod-calls-pod` edge carries `labels.cluster` (the client pod's cluster identity, present iff the client side resolved to a pod — `pod-service-graph` "Edge cluster label"); a `service-selects-pod` edge carries no `cluster` key (its source is a service node, which is cluster-scoped via its own `labels.cluster`).

When a `cluster` filter excludes one side, that side's topology is not loaded; the peer then follows the `pod-service-graph` capability's filtered-build rule and is rendered as an `external/<label>` node (carrying no `cluster`), and the edge is no longer cross-cluster — it is an edge to an external. The family-wide `service-selects-pod` fan-out of a local service node reaches only backing pods in **loaded** clusters. The former rule that a `?cluster=` projection keeps the out-of-scope partner as a real pod node is withdrawn.

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

### Requirement: Availability-zone and environment selector filters

`GET /v1/graph` SHALL accept the optional, repeatable parameters `az` and `env`. Each SHALL be rendered as an upstream label matcher on every kube-state-metrics and kubelet query of the build, on **no** NetApp Harvest query, and on no service-graph query; the `up{}` probe SHALL never carry them. `az` additionally selects which `harvest` backends are asked (the `upstream-backend-routing` capability's zone rule); that selection is the only effect `az` has on the Harvest legs, and `env` has none. The upstream label each parameter binds to is the operator-configured key of the `cluster-topology-source` capability ("Configurable `az` / `env` label keys"), defaulting to `az` and `env`; the request parameter names themselves are fixed.

The same two labels are the zone and environment components of every cluster's identity (`cluster-topology-source`, "Cluster identity composed from zone and environment labels"): a cluster whose series carry both is listed in `clusters[]` and prefixed on its ids as `<az>-<env>-<cluster>`, and the filter triple `az` + `env` + `cluster` therefore addresses exactly one listed identity.

The two filters narrow **at the source**: a series that lacks the configured label does not match an equality matcher and is therefore absent from the build. The operator SHALL ensure every kube-state-metrics and kubelet family stamps both labels; a family that does not vanishes from every `az` / `env`-filtered request, and because the default projection keeps only connectivity-connected workload, a topology family missing the label yields an empty graph for that filter rather than a partial one — and, unfiltered, such a family resolves its cluster through the identity ladder rather than composing it. The Harvest families are exempt: they carry no matcher, so they need no label. The response `clusters` list, derived from the built graph's node `cluster` labels, SHALL therefore list only the cluster identities with data in the requested zone / environment.

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
