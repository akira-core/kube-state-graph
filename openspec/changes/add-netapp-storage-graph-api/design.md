## Context

See `proposal.md` — Why. What shapes the approach:

- **One build pipeline, one fan-out.** `Builder.Build` = `ReadTopology`
  (31 / 37 parallel PromQL legs) → `ReadServiceGraph` → `assemble` →
  `graph.NewGraph` → `graph.Project` → `cytoscape.Serialise`. The storage
  join (`resolveNetAppStorage`, hops A/B/C) already runs inside
  `ReadTopology` and produces per-claim `pvc-to-netapp-aggr` edges carrying
  `Edge.IO`, the aggregate / controller nodes, and `svmByPVC`.
- **Projection is a pure function of the built graph** (`graph.Project(g,
  Scope)`); the build is a pure function of `(window, end, Selector)`. Both
  properties are load-bearing (determinism, goldens, future caching) and are
  kept for the new endpoint.
- **Every upstream call is routed by `(family, az)`** through the immutable
  `promql.Table`; a family unserved by the table is a validation error
  today. `queryDims` / `queryFamily` are exhaustive over the `Query` set by
  test.
- **The sealed `GraphNode` interface** carries typed attributes
  (`IPAddress`, `Owner`, `Application`, …); the serialiser reads them
  through the interface, never by type switch.
- **Selector-level filters apply at the query layer**; `az` / `env` are
  repeatable on `/v1/graph`. A filtered build never issues the `up{}`
  probe and returns an empty 200 for an empty estate.

## Goals / Non-Goals

**Goals:**

- A second build entry point that reuses `ReadTopology` unchanged and adds
  no service-graph read; the storage-flow body is derived from data the
  build already loads (plus the new Harvest node legs).
- Weights that conserve in the **projected** body under every root /
  filter combination, computed order-free.
- Zero byte change to `/v1/graph` goldens whose fixtures carry none of the
  new series.
- `alerts` as the first optional family without touching the five-family
  coverage rule or any existing backends file.

**Non-Goals:**

- Caching, singleflight, or sharing a built graph between the two
  endpoints.
- SVM-level Harvest attributes (`svm_labels`, SVM health / usage) — the SVM
  node is an identity only in this change.
- Alerts on `service`, `external` or `netapp-svm` nodes.
- Any derived health verdict from performance counters (see proposal).
- A `/v1/graph` SVM node or `storage-flow` edge.

## Decisions

### D1. Separate build path, shared topology read

`Builder.BuildStorage(ctx, window, end, sel)` runs `ReadTopology` with the
same selector, **skips `ReadServiceGraph` entirely**, and calls a new pure
`assembleStorageFlow(topology)` instead of `assemble`. It returns an
ordinary `*graph.Graph` (nodes: pods, K8s nodes, PVCs, NetApp nodes /
aggregates / SVMs; edges: `storage-flow` only).

*Why not derive the flow inside `graph.Project` from the `/v1/graph`
build?* Three reasons: the `/v1/graph` build pays the three
`traces_service_graph_*` legs — the most expensive fan-out leg — for a body
that uses none of it; the ordinary graph holds no SVM node, so the
projection would have to *invent* nodes, breaking "projection is pure over
the built graph"; and the tier edges need their own stable UUIDv5 ids,
which belong to the builder. *Why not a third `Project` mode over a
storage-only graph built by `Build`?* Same first reason. The topology read
is byte-for-byte shared, so the two builds cannot disagree on what a claim
is.

The build stays root-agnostic: like `/v1/graph`, only `Selector` reaches
the queries; roots are a projection concern (D4). This keeps
`BuildStorage` a pure function of `(window, end, Selector)` — the same key
a future cache would use.

### D2. The whole Harvest inventory is materialised in the storage build

`resolveNetAppStorage` today materialises aggregates and controllers
**only via the PVC join**. The storage build needs flowless roots, so it
additionally materialises every NetApp entity the Harvest read names:

- `Topology.NetAppInventory` — sets collected in the existing single pass
  over `volume_labels` (every `(oc, node, aggr, svm)` triple) plus the node
  keys of `node_labels` / `node_new_status` / the four counters and the
  aggregate keys of `aggr_*`. Owner of an aggregate = lexically-smallest
  non-empty `node` among its volumes (the existing `pickOwner` rule).
- `assembleStorageFlow` emits one `NetAppNode` per inventory controller
  (health / hardware / perf attached), one `NetAppAggrNode` per inventory
  aggregate (owner, health, usage), one `NetAppSVMNode` per inventory SVM.
- `assemble` (the `/v1/graph` path) ignores the inventory — join-only
  materialisation is unchanged there.

*Why materialise everything rather than pass roots into the build?* Passing
roots would make the build a function of the request, which the caching
key and the "selectors only reach queries" rule both forbid. Inventory
size is bounded by the filer (tens of aggregates, hundreds of SVMs), not
by the Kubernetes estate. Flowless entities cost nothing at projection —
they are dropped unless they are roots.

### D3. Flow unit = `(claim, mounting pod)`; weights are summed at projection

The build emits, per joined-and-mounted claim, the tier edges
`node-aggr`, `aggr-svm`, `svm-pvc`, `pvc-pod` (one per mounter), `pod-node`
(one per scheduled mounter), deduplicated by `(source, target)` (D6
insert-only sets), each via `graph.NewEdge(EdgeTypeStorageFlow, …)` with
`labels.tier` and — on a `pvc-pod` edge whose claim has n > 1 mounters —
`labels.attribution="split"`. **The build bakes exactly one weight: the
claim's own `IOMetrics` (all eight fields) on its `svm-pvc` edge.** Every
other edge is emitted weightless.

`graph.ProjectStorage` (D4) then computes weights over the **retained**
flow units: a unit's share is the claim's four flow figures divided by
`n` = the number of `pvc-pod` edges leaving that PVC in the *built* graph;
each retained edge's weight is the ascending-order (by claim id, then pod
id) sum of the shares of every retained unit passing through it. The
`svm-pvc` edge keeps the claim's latency and ceiling verbatim and gets its
flow figures re-summed like any other tier. Edges are returned as `WithIO`
copies; the built graph is never mutated.

*Why not bake summed weights at build time?* A `namespace` filter or a
`pod=` root removes flow units; weights baked over the full estate would
then fail to conserve in the body (a PVC showing 300 in and 100 out).
Summing over retained units is what makes the spec's conservation clause
true in every view, and it costs one pass over the retained edges. *Why
`n` from the built graph and not the view?* The share is a fact about the
claim (how many pods mount it), not about the request; a `pod=` root
shows that pod's honest 1/n, which the up-tier sums then carry. *Why equal
split?* Per-pod I/O is not observable from any series the build reads; an
equal split conserves and is labelled so a consumer can tell it from a
measurement.

An unmeasured claim (`IO == nil`) contributes no unit to any sum; an edge
whose every unit is unmeasured returns no `IO`, hence no `metrics` key.

### D4. `graph.ProjectStorage(g, StorageScope) View`

```
StorageScope{ Clusters, Namespaces map[string]struct{}; Roots StorageRoots }
StorageRoots{ ONTAPClusters, Nodes, Aggrs, SVMs map[string]struct{}
              Pods map[PodRef]struct{} }   // PodRef{Namespace, Name}
```

Algorithm, all over the built graph:

1. Index `storage-flow` edges by tier; build the flow units (D3) by
   walking each `svm-pvc` edge up (`aggr-svm`, `node-aggr` — absent for a
   FlexGroup claim) and down (`pvc-pod`, then that pod's `pod-node`).
2. Resolve roots to node-id sets **per side**: storage side = every
   `netapp-node` / `netapp-aggr` / `netapp-svm` whose `labels.ontap_cluster`
   ∈ `ONTAPClusters`, plus controllers named in `Nodes`, aggregates in
   `Aggrs`, SVMs in `SVMs`; workload side = pods whose
   `(labels.namespace, Name())` ∈ `Pods`, plus K8s nodes named in `Nodes`.
   Record separately whether each side was *requested* (any root
   parameter that could resolve to it was non-empty — `node=` counts for
   both sides).
3. A unit is retained iff `(!storageRequested || unit ∩ storageIDs ≠ ∅)
   && (!workloadRequested || unit ∩ workloadIDs ≠ ∅)`, and its pod / PVC /
   node pass the re-applied `cluster` / `namespace` filters (same
   `nodePassesFilters` helper as `Project`). A requested side that resolved
   to **nothing** therefore retains nothing — `?aggr=typo` is empty, not
   the estate.
4. Nodes = ∪ retained units' nodes ∪ resolved root ids ∪ the owning
   `netapp-node` of every root aggregate (so its `data.parent` exists).
   Edges = the retained units' edges, weighted per D3. `SortNodes` /
   `SortEdges`.

The existing `pullNetAppParents` is reused for step 4's parent rule. No
connectivity prune runs.

### D5. Request surface and parser

`kubegraph.ParseStorageValues(url.Values) (StorageRequest, error)` with
`StorageRequest{Start, End, Selector promql.Selector, Scope
graph.StorageScope}`. It shares `parseTimestamp` / `validateSelectorValues`
with `ParseValues`, then: `az` and `env` must be present exactly once
(`missing_az` / `missing_env`; a second value → `invalid_scope`); `pod`
values must split on exactly one `/` into two non-empty segments
(`invalid_scope`); `cluster` / `namespace` are optional and repeatable.
`Selector` is populated with single-element `AZ` / `Env` slices so the
existing render / routing code is untouched. `edge_type` / `prune` are
ignored. The handler and `Engine.BuildStorageFromValues` both call this
parser — the same drift guard as `/v1/graph`.

*Why `pod=<ns>/<name>` rather than `pod=` + `namespace=`?* `namespace` is
already a narrowing filter with OR semantics; overloading it to qualify a
root would make `?namespace=a&namespace=b&pod=x` ambiguous. One
self-contained value per root is unambiguous and repeatable.

### D6. Harvest node legs and the `perf` / `hardware` attributes

Five new `fetchOptional` legs in `ReadTopology`, all `FamilyHarvest`,
`dimsHarvest` (routed by `az`, no matcher), rendered as
`last_over_time(<name>[w])`: `node_labels`, `node_cpu_busy`,
`node_total_ops`, `node_total_latency`, `node_total_data`.
`resolveNetAppStorage` gains the five vectors and two indexes keyed
`(oc, node)` — `hardwareByNode` (per-field lexically-smallest non-empty
label) and `perfByNode` (verbatim floats; duplicate series → the
lexically-smallest value, matching `healthFromSamples`' order-free
stance) — and stamps `HardwareValue` / `PerfValue` on every `NetAppNode`
it materialises (join path and inventory path alike). `GraphNode` gains
`Hardware() *Hardware` and `Perf() *NodePerf`; all other types return nil.
The counter names follow Harvest's stock `system_node` template; the
first implementation task verifies them against the Harvest metric
catalogue before the query constants are committed.

Query-count pins move: 31 → 36 first-wave legs without a matched volume
(plus `ALERTS` = 37), 37 → 43 with one.

### D7. Alert overlay: one leg, one resolver, five node kinds

- **Query**: `QAlerts = "ALERTS"`, fixed selector `alertstate="firing"`
  rendered first, `queryDims[QAlerts] = dimAZ | dimEnv | dimNamespace`
  (a new `dimsAlerts` constant — deliberately not `dimsNamespaced`, which
  carries `dimCluster`), `queryFamily[QAlerts] = FamilyAlerts`.
  `Family.AcceptsAZ()` derives routability from `dimAZ` unchanged.
- **Resolver** `resolveAlerts(vec, idx alertIndex) map[nodeID][]Alert` in
  `pkg/build/alerts.go`, pure. `alertIndex` is built once from the
  assembled node set: pods by `(identity, ns, name)` and by `(ns, name)`;
  PVCs likewise; K8s nodes by `(identity, name)` and by `name`; controllers
  by `(oc, node)` and by `node`; aggregates by `(oc, aggr)` and by `aggr`.
  The Kubernetes `cluster` label walks the same `clusterResolver` ladder
  as every other series (`bucket(QAlerts, …)`), so its Warn aggregation is
  reused. Kind precedence and the `{cluster, node}` two-way test are exactly
  the spec's; ambiguous / unmatched counts feed two aggregated Warns.
- **Attachment**: `AlertsValue []Alert` fields on `PodNode`, `K8sNode`,
  `PVCNode`, `NetAppNode`, `NetAppAggrNode`, set in `Build` / `BuildStorage`
  after `assemble*` and before `graph.NewGraph` (the same "bake before
  freeze" point as PVC application inheritance). `GraphNode.Alerts()`
  returns nil elsewhere. Sorted and de-duplicated on `(name, severity)`.
- **Unserved family**: `Table.Select(FamilyAlerts, az)` may return no
  backend; the router answers an empty vector and logs at **Debug** (not
  the "zone no backend declares" Warn — being unserved is the documented
  normal state for this family). Validation logs one Info at load when no
  backend serves it. The `selector_family_empty` Warn excludes
  `FamilyAlerts` — an empty alert vector is the healthy estate.

*Why `firing` only?* A `pending` alert is a threshold crossed for less than
its `for:`; surfacing it would flap the diagram on every rebuild. *Why
`last_over_time` over the whole window rather than the end instant?* The
endpoint's contract is "what was true in `[start, end]`"; an alert that
fired and resolved inside the window is part of the answer, and it
matches how every other series is read.

### D8. `alerts` as the first optional family

`promql.Families` grows to six; `validateTable` keeps the "served by no
backend" error for the original five (`requiredFamilies`) and accepts an
unserved `alerts`. `SingleBackendTable` serves all six. `backendsfile`
accepts the new name with no schema change. *Why not require it?* Every
existing backends file would fail to load on upgrade — a BREAKING change
for a purely additive feature. *Why not a separate `--alerts-url` flag?*
The routing table already expresses "which store, for which zone, with
which credentials"; a second mechanism would duplicate reload, auth and
metrics.

### D9. Registry, serialiser, engine, handler

- `graph.EdgeTypeStorageFlow` + `NodeTypeNetAppSVM` + `NetAppSVMNode`
  (labels `{ontap_cluster}`, every attribute accessor nil / empty).
  Registry entry per the spec; `ValidEdgeType` accepts it, so
  `?edge_type=storage-flow` on `/v1/graph` is a 200 with no edges.
- `cytoscape.NodeData` gains `Hardware *HardwareDTO`, `Perf *PerfDTO`,
  `Alerts []AlertDTO`, all `omitempty`; `compoundParent` gains the
  `NodeTypeNetAppSVM → storage-cluster/<oc>` case; `storageClusterSeen` is
  already keyed on `labels.ontap_cluster`, so the SVM's group appears with
  no further change. `metricsDTO` already serialises `Edge.IO`.
- `kubegraph.Engine.BuildStorage` / `BuildStorageFromValues`;
  `internal/api.handleStorageGraph` mirrors `handleGraph` (same
  `runBuild`-style timeout wrapper, `mapBuildError`, spans, swag
  annotations; `make docs`).
- Because `az` / `env` are always active, `BuildStorage` is always a
  *filtered* build: no `up{}` probe, no `outside_retention`, empty estate
  = empty 200 — consistent with the existing filtered-build rule and with
  the spec's "unknown root is not drawn".

### D10. Containment and test layering

`pkg/build` still imports nothing from `internal/`; the resolver and the
flow assembler are pure and unit-tested with hand-built `Topology` /
`model.Vector` fixtures (`storageflow_test.go`, `alerts_test.go`).
`ProjectStorage` gets property tests in `pkg/graph` (conservation at every
interior node for random estates with random roots; retained ⊆ built; root
presence). Component tests drive `/v1/storage-graph` through
`newMockQuerier`; two new goldens (`storage-graph-cytoscape.json`, one
per root side) and `/v1/graph` goldens extended only where a fixture adds
`node_labels` / counters / `ALERTS`. Integration adds `TestStorageGraph`
(Sankey conservation end to end) and extends `TestPVCNetAppHarvestJoin`'s
fixture with `node_labels`.

## Risks / Trade-offs

- [Harvest counter names differ across Harvest versions / templates] →
  verified against the catalogue in the first task; each leg is
  `fetchOptional`, so a wrong name costs one `perf` field, never a build.
- [`ALERTS` cardinality on a large estate] → `firing` only, `namespace`
  narrowing, and one `last_over_time` — the same shape as every KSM leg;
  the family can be served by no backend to opt out entirely.
- [`{cluster, node}` alerts misattributed when a Kubernetes cluster and an
  ONTAP cluster share a raw name] → the two-way test attaches to neither
  and counts ambiguous; documented in `docs/upstream-metrics.md`.
- [Equal RWX split misrepresents a skewed workload] → labelled
  `attribution="split"`; a consumer can hide or hatch such links.
- [Inventory materialisation on a very large filer] → bounded by ONTAP
  object counts (hundreds), and only the storage build pays it.
- [Query-count pins and fan-out width grow by six] → the pins are
  deliberate; the six legs are all optional and run in the same errgroup.
- [Two parsers drift] → both live in `pkg/kubegraph` and share every
  helper; the storage parser's tests mirror `ParseValues`'.

## Migration Plan

Additive. Deploy the new binary; `/v1/graph` bodies are byte-identical
until the stores carry `node_labels`, the counters or `ALERTS`. Existing
backends files load unchanged (an Info notes the overlay is off). To
enable alerts, add `alerts` to a backend's `families` and reload. Rollback
is the previous binary; no data or config migration in either direction.
The demo repository's `netapp-faker` gains `node_labels` and the four
counters in a follow-up; its `verify.sh` may add a `/v1/storage-graph`
hop.

## Open Questions

- Whether the frontend wants `labels.tier` values exposed through
  `/v1/edge-types` descriptions beyond the enumeration (cosmetic; no
  contract change).
