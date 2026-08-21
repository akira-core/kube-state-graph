## MODIFIED Requirements

### Requirement: Versioned route prefix

The HTTP API SHALL expose every endpoint under the `/v1/` route prefix and SHALL include `apiVersion: "v1"` as a top-level field in every JSON response body.

#### Scenario: Body carries apiVersion

- **WHEN** a client sends `GET /v1/edge-types`
- **THEN** the server returns 200 with a JSON body whose top-level object contains `"apiVersion": "v1"`

#### Scenario: Unversioned route is not served

- **WHEN** a client sends `GET /graph?start=...&end=...`
- **THEN** the server returns 404 Not Found

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

## ADDED Requirements

### Requirement: Availability-zone and environment selector filters

`GET /v1/graph` SHALL accept the optional, repeatable parameters `az` and `env`. Each SHALL be rendered as an upstream label matcher on every topology query of the build (kube-state-metrics, kubelet, NetApp Harvest) and on no service-graph query; the `up{}` probe SHALL never carry them. The upstream label each parameter binds to is the operator-configured key of the `cluster-topology-source` capability ("Configurable `az` / `env` label keys"), defaulting to `az` and `env`; the request parameter names themselves are fixed.

The two filters narrow **at the source**: a series that lacks the configured label does not match an equality matcher and is therefore absent from the build. The operator SHALL ensure every topology family stamps both labels; a family that does not vanishes from every `az` / `env`-filtered request, and because the default projection keeps only connectivity-connected workload, a topology family missing the label yields an empty graph for that filter rather than a partial one. The response `clusters` list, derived from the built graph's node `cluster` labels, SHALL therefore list only the clusters with data in the requested zone / environment.

#### Scenario: Environment filter selects one environment's clusters

- **WHEN** the upstream holds `cluster-prod-1` (all series `env="prod"`) and `cluster-dev-1` (all series `env="dev"`) and a client sends `?env=prod`
- **THEN** every topology query carries `env="prod"`, the response contains only `cluster-prod-1` workload and infrastructure, and `clusters` is `["cluster-prod-1"]`

#### Scenario: Zone and environment are AND-combined

- **WHEN** `cluster-a` carries `az="zone-a",env="prod"`, `cluster-b` carries `az="zone-b",env="prod"`, and a client sends `?az=zone-a&env=prod`
- **THEN** the response contains `cluster-a` only

#### Scenario: Configured key is used in the matcher

- **WHEN** the server runs with `KSG_AZ_LABEL=topology_zone` and a client sends `?az=zone-a`
- **THEN** the rendered matcher is `topology_zone="zone-a"`, and the request parameter is still named `az`

#### Scenario: Family lacking the label vanishes under the filter

- **WHEN** the kube-state-metrics and kubelet series carry `env="prod"` but the Harvest series carry no `env` label, and a client sends `?env=prod`
- **THEN** the response contains the prod pods, nodes, and claims but no `netapp-aggr` / `netapp-node` nodes and no `pvc-to-netapp-aggr` edges (the Harvest legs returned nothing), and the build does not fail

## REMOVED Requirements

### Requirement: Partial-graph traversal

**Reason**: The `root` / `depth` / `direction` traversal existed as an on-demand escape hatch from the default connectivity prune and as a way to bound an otherwise unbounded full-estate response. Both jobs are now done at the source — selector-level `cluster` / `namespace` / `az` / `env` filters bound the build, and `prune=false` lifts the prune — so the BFS, its depth cap, and the root-anchored prune exception are removed to simplify the request surface.

**Migration**: Replace `?root=<id>&depth=<n>&direction=<d>` with the selector-level filters that describe the neighbourhood of interest (`?cluster=…&namespace=…`, optionally `&prune=false`). A client that still sends the three parameters receives the unanchored view without error.

### Requirement: Cluster discovery endpoint

**Reason**: `/v1/clusters` was the one upstream read outside the build, backed by its own discovery query and fixed one-hour lookback; keeping it would require a parallel copy of the new selector contract for a list the graph body already carries.

**Migration**: Read the `clusters` field of any `GET /v1/graph` response — it is the sorted set of clusters with data in the requested window and scope (`?prune=false` lists every loaded cluster, including those with no connectivity-connected workload). No replacement endpoint is provided.
