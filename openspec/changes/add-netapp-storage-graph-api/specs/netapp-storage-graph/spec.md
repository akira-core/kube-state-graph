## ADDED Requirements

### Requirement: NetApp SVM entity

The builder SHALL resolve, for each joined claim, the SVM its FlexVol lives in — the `svm` label of the matched `volume_labels` series, picked exactly as the PVC `svm` label already is (scoped to the picked aggregate's ONTAP cluster; lexically-smallest on conflict) — and SHALL be able to materialise it as a `type="netapp-svm"` graph node:

- `id` SHALL be `netapp/<ontap-cluster>/svm/<svm>` (SVM names are unique within an ONTAP cluster).
- `name` SHALL be `<svm>`.
- `labels` SHALL be exactly `{ontap_cluster: "<ontap-cluster>"}` — no `cluster` key; the same `clusters[]` / `?cluster=` exclusion as the aggregate and controller.

An SVM SHALL NOT carry `ipaddress`, `owner`, `application`, `containers`, `ready_status`, `health`, `usage`, `hardware` or `perf`. It SHALL be materialised only by the storage-flow graph (`storage-graph-api`) — as a tier of a retained claim's path or as a root — and NEVER by `GET /v1/graph`, whose body stays byte-identical. A claim whose matched series carries an empty `svm` SHALL produce no SVM node and SHALL contribute no storage-flow path at all — the tier chain is fixed and an `aggr → pvc` shortcut is not permitted; such a claim is counted like a topology miss for that graph.

#### Scenario: SVM identity and labels

- **WHEN** a claim joins a `volume_labels` series with `cluster="ontap-prod"`, `svm="svm_shop"` and the storage-flow graph retains its path
- **THEN** the body contains `{id:"netapp/ontap-prod/svm/svm_shop", name:"svm_shop", type:"netapp-svm", labels:{ontap_cluster:"ontap-prod"}}`

#### Scenario: SVM spans aggregates

- **WHEN** two claims in `svm_shop` join aggregates `aggr1` and `aggr2` on the same ONTAP cluster
- **THEN** exactly one `netapp/ontap-prod/svm/svm_shop` node is materialised, with an `aggr-svm` edge arriving from each aggregate

#### Scenario: Not emitted by the pod graph

- **WHEN** the same estate is served by `GET /v1/graph`
- **THEN** the body contains no `type="netapp-svm"` node and no `storage-flow` edge

## MODIFIED Requirements

### Requirement: NetApp node entity and health

The builder SHALL materialise each ONTAP controller referenced by at least one emitted `netapp-aggr` node (its `labels.node`) — or, in the storage-flow graph, selected as a root — as a `type="netapp-node"` graph node:

- `id` SHALL be `netapp/<ontap-cluster>/<node>`.
- `name` SHALL be `<node>` (the controller name).
- `labels` SHALL be exactly `{ontap_cluster: "<ontap-cluster>"}` — no `cluster` key; the same `clusters[]` / `?cluster=` exclusion as the aggregate.

The node's health SHALL derive from the OPTIONAL Harvest series `node_new_status` (fixed, case-sensitive labels: `cluster` — the ONTAP cluster, `node`; sample value `1` = the controller is healthy, any other value = not healthy), matched on `(ontap-cluster, node)`:

- the matched sample is `1` → `data.health = "online"`
- the matched sample is not `1` → `data.health = "degraded"`
- no matched series (or the metric absent entirely) → the `health` attribute is **omitted**.

Absence of data SHALL stay distinct from a reported unhealthy state — the builder SHALL NOT default a missing metric to `"degraded"` (the `ready_status` absent-vs-Unknown precedent). On duplicate series for one `(ontap-cluster, node)` the builder SHALL derive deterministically (any non-`1` sample → `"degraded"`, order-free). Failure of the `node_new_status` query SHALL degrade gracefully (health omitted on every node) and SHALL NOT fail the build.

**Hardware attribute.** The node SHALL additionally carry a typed, nullable `data.hardware` object resolved from the OPTIONAL Harvest info series `node_labels` (labels `cluster`, `node`, and any of `model`, `serial`, `version`, `vendor`, `location`; sample value ignored), matched on `(ontap-cluster, node)`: `{ model, serial, version, vendor, location }`, each field taken verbatim from the like-named label and **omitted when the label is empty or absent**; the whole object SHALL be omitted when no field resolves or no series matches. On duplicate series the lexically-smallest non-empty value per field wins. The attribute SHALL NEVER be placed inside `labels`.

**Performance attribute.** The node SHALL additionally carry a typed, nullable `data.perf` object resolved from four OPTIONAL Harvest `system_node` counters, each matched on `(ontap-cluster, node)` and read **verbatim** (no `rate()` — the Harvest values are already per-second / percent figures): `cpu_busy_pct` from `node_cpu_busy`, `total_ops` from `node_total_ops`, `total_latency_us` from `node_total_latency`, `total_bytes_per_sec` from `node_total_data`. Each field is independently optional (omitted when its series is absent or its query failed); the object is omitted when no field resolves. Values are JSON numbers rounded to 6 significant digits. The builder SHALL NOT derive `health` — nor any other verdict — from these counters: thresholds are model- and estate-specific and belong in the operator's alert rules, whose verdicts reach the node through the `alert-overlay` capability. Each of the five new legs (`node_labels` plus the four counters) is OPTIONAL and degrades log-and-continue.

Both attributes SHALL be resolved at build time onto the graph and therefore appear identically on `GET /v1/graph`, `GET /v1/storage-graph`, and every in-process engine call. A NetApp node SHALL be materialised ONLY via aggregate reference or as a storage-flow root — never wholesale from Harvest series presence — and SHALL NOT carry `ipaddress`, `owner`, `application`, `containers`, or `ready_status`. It acts as the **compound parent** of its aggregates (graph-api "Cytoscape compound node grouping"); in `GET /v1/graph` it is the target of no edge, while in the storage-flow graph it is the source of `storage-flow` edges of tier `node-aggr`.

#### Scenario: Node identity, labels, and health

- **WHEN** an emitted aggregate carries `labels.node="ontap-prod-01"` in ONTAP cluster `ontap-prod` and `node_new_status{cluster="ontap-prod", node="ontap-prod-01"}` has value `1`
- **THEN** the graph contains a node with `id="netapp/ontap-prod/ontap-prod-01"`, `name="ontap-prod-01"`, `type="netapp-node"`, `labels={ontap_cluster:"ontap-prod"}`, and `data.health="online"`

#### Scenario: Unhealthy controller

- **WHEN** `node_new_status` for a materialised controller has value `0`
- **THEN** that node carries `data.health="degraded"`

#### Scenario: Absent node-status metric omits the attribute

- **WHEN** no `node_new_status` series matches a materialised NetApp node (or the metric is absent from the window)
- **THEN** that node's `data` has no `health` key — absence is never conflated with `"degraded"`

#### Scenario: Controller not referenced by any aggregate is not materialised

- **WHEN** `node_new_status` reports a controller that owns no emitted aggregate and is not a storage-flow root
- **THEN** no `netapp-node` node is materialised for it

#### Scenario: Hardware attribute resolved

- **WHEN** `node_labels{cluster="ontap-prod", node="ontap-prod-01", model="AFF-A400", serial="721234000123", version="9.14.1", vendor="NetApp", location=""}` is present
- **THEN** the node carries `data.hardware={model:"AFF-A400", serial:"721234000123", version:"9.14.1", vendor:"NetApp"}` with no `location` key, and `labels` stays exactly `{ontap_cluster:"ontap-prod"}`

#### Scenario: Hardware absent

- **WHEN** no `node_labels` series matches a materialised controller
- **THEN** that node's `data` has no `hardware` key

#### Scenario: Performance counters resolved verbatim

- **WHEN** `node_cpu_busy=72.5`, `node_total_ops=18500`, `node_total_latency=830` and `node_total_data=1.2e9` match a materialised controller
- **THEN** the node carries `data.perf={cpu_busy_pct:72.5, total_ops:18500, total_latency_us:830, total_bytes_per_sec:1.2e9}` and its `data.health` is unchanged by those values

#### Scenario: Partial performance counters

- **WHEN** only `node_cpu_busy` matches a controller
- **THEN** the node carries `data.perf={cpu_busy_pct:<value>}` and no other `perf` field

#### Scenario: High CPU does not degrade health

- **WHEN** `node_cpu_busy=99` and `node_new_status=1` match a controller
- **THEN** the node carries `data.health="online"`

#### Scenario: Attributes present on both endpoints

- **WHEN** a controller resolves `hardware` and `perf` and is retained by both `GET /v1/graph` and `GET /v1/storage-graph`
- **THEN** both bodies carry identical `data.hardware` and `data.perf` on that node

### Requirement: Harvest legs under request-scoped selectors

Every NetApp Harvest query the builder issues — `volume_labels`, the six `qos_*` workload families, the two `qos_policy_fixed_max_throughput_*` families, `aggr_new_status`, `aggr_space_used`, `aggr_space_total`, `node_new_status`, `node_labels`, `node_cpu_busy`, `node_total_ops`, `node_total_latency`, and `node_total_data` — SHALL carry **no request-scoped matcher of any kind**: not `az`, not `env`, and never `cluster` or `namespace`. Harvest's `cluster` label is the **ONTAP** cluster name and never a Kubernetes cluster, so a Kubernetes `cluster` value pushed into it would match nothing; Harvest carries no `namespace` at all; and the `az` dimension reaches Harvest through backend selection alone (below), so a Harvest series need not carry the configured `az` / `env` labels.

The `qos_*` families carry one selector and no other: the `volume` alternation of the scoped-read requirement. That alternation is **derived from upstream data, not from the request**: its values are FlexVol names the volume-label family already returned. It is nonetheless *influenced* by the request, because the claims whose tokens produced those names are themselves loaded under the request's selectors. This is the "narrowed by reference" principle of this capability realised at the query layer rather than only in the parse: a `cluster`, `namespace`, `az` or `env` filter reaches the QoS read solely through the claims it loads, never as a matcher on a Harvest label. Every other Harvest query carries no selector at all.

These queries constitute the `harvest` query family of the `upstream-backend-routing` capability, so they MAY be served by a different upstream installation from the kube-state-metrics and kubelet legs. The family is **zone-routed**: a request's `az` values select which `harvest` backends are asked — those whose `zones` intersect the request, plus any catch-all — under the same rule as the `ksm` and `kubelet` families. Unlike those families, the selected zone is NOT additionally rendered as a matcher: for Harvest the zone boundary is the store, not a label on the series. The `env` dimension has no routing counterpart and SHALL have no effect on the Harvest legs whatsoever. Routing changes **which** installation answers a Harvest query; it changes neither the query string, the three-hop join, nor the per-hop degradation below.

Within a filtered build the storage chain is therefore narrowed **by reference**: an aggregate and its owning controller materialise only when a **loaded** claim's derived token matches a `volume_labels` series (or, in the storage-flow graph, when selected as a root), so a `cluster`, `namespace`, or `env` filter reaches the NetApp graph solely through the claims it loads, and an `az` filter reaches it through the claims it loads plus the backends it selects. A filer shared across clusters, zones, or environments is one node set, reached from whichever loaded claims match it. Under a catch-all `harvest` backend, or under any `env` value, the volume-label read is the whole estate and the narrowing is by reference alone; a FlexVol name carried by volumes in two zones or environments resolves to the lexically-smallest `(ontap_cluster, aggr)` — the same collision rule an unfiltered build already applies, since an unfiltered build reads every zone.

#### Scenario: Cluster and namespace filters never reach Harvest

- **WHEN** a build runs with `cluster={cluster-alpha}` and `namespace={shop}` and no `az` / `env` value
- **THEN** no Harvest query carries a `cluster` or `namespace` matcher; the `volume_labels`, aggregate, controller, hardware, performance and policy queries are issued exactly as in an unfiltered build; the QoS queries restrict `volume` to the FlexVol names matched by the loaded `cluster-alpha` / `shop` claims alone; and the aggregates in the response are exactly those those claims matched

#### Scenario: Zone filter reaches Harvest

- **WHEN** two backends serve `harvest`, declaring `zones: [zone-a]` and `zones: [zone-b]`, and a build runs with `az={zone-a}`
- **THEN** every Harvest query — including `node_labels` and the four `system_node` counters — is issued only to the `zone-a` backend, carrying no `az` matcher; a `volume_labels` series held by the `zone-b` backend is not loaded even if a loaded claim's derived token would match it

#### Scenario: Catch-all Harvest backend under a zone filter

- **WHEN** one backend serves `harvest` with no `zones` declared and a build runs with `az={zone-a}`
- **THEN** every Harvest query is issued to that backend carrying no `az` matcher, every zone's `volume_labels` series is loaded, and the aggregates in the response are exactly those matched by the loaded `zone-a` claims

#### Scenario: Environment filter does not reach Harvest

- **WHEN** a build runs with `env={prod}` against a single `harvest` backend holding series stamped `env="prod"` and `env="dev"`
- **THEN** no Harvest query carries an `env` matcher, both environments' series are loaded, and a loaded `prod` claim whose derived token matches a `volume_labels` series stamped `env="dev"` still receives its `pvc-to-netapp-aggr` edge

#### Scenario: Shared filer reached by reference from either filtered cluster

- **WHEN** claims in `cluster-alpha` and `cluster-beta` both match `netapp/ontap-prod/aggr/aggr1` and a build runs with `cluster={cluster-alpha}`
- **THEN** `netapp/ontap-prod/aggr/aggr1` and its owning `netapp-node` are materialised with a `pvc-to-netapp-aggr` edge from the `cluster-alpha` claim only; a `cluster={cluster-beta}` build materialises the same two nodes from the `cluster-beta` claim

#### Scenario: Harvest lacking the environment label under an env filter

- **WHEN** the kube-state-metrics series carry `az="zone-a"`, the Harvest series carry no `az` and no `env` label at all, and a build runs with `az={zone-a}` or `env={prod}`
- **THEN** every Harvest leg returns its rows, the `pvc-to-netapp-aggr` edges are drawn for the loaded claims that match, and the selector-coverage Warn never names a Harvest family

#### Scenario: Harvest served by its own upstream

- **WHEN** the routing table declares one backend serving `harvest` at `http://vm-netapp.example:8428` and another serving every other family at `http://vm-k8s.example:8428`
- **THEN** every Harvest query — including each chunk of the scoped QoS read and the five node legs — is issued only to `http://vm-netapp.example:8428`, the kube-state-metrics and kubelet queries only to `http://vm-k8s.example:8428`, and the resulting `pvc-to-netapp-aggr` edges join claims read from one upstream to volumes read from the other

#### Scenario: Unfiltered build merges Harvest across backends

- **WHEN** two backends serve `harvest` for different zones and a build runs with no `az` value
- **THEN** every Harvest query is issued to both and the results are merged, so aggregates from both zones can match their claims in one graph
