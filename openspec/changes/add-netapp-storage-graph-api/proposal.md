## Why

`/v1/graph` is workload-rooted: its default projection keeps only pods on a
connectivity edge and lets storage hang off them, and its selectors
(`cluster` / `namespace` / `az` / `env`) address Kubernetes only. A storage
operator asks the opposite question — "this ONTAP controller / aggregate / SVM
is degraded or saturated; which claims, pods and Kubernetes nodes sit on it,
and how much I/O is each pushing?" — and wants the answer as a **Sankey
diagram**: storage on the left, workload on the right, link width = I/O.
Today there is no endpoint that takes a NetApp component as input, no SVM
entity to start from, the storage relationships are not a flow (the
`netapp-node > netapp-aggr` tier is compound nesting, not an edge, and only
one hop carries a measurement), and the `netapp-node` carries no hardware
identity (model / serial / version) to match against the filer at hand.
Nor does any node say whether it is currently **alerting** — the operator
has to cross-reference the diagram with an alert list by hand.

## What Changes

- **New `GET /v1/storage-graph` endpoint** returning a **storage-flow DAG**
  shaped for Sankey rendering. Same `start` / `end` window, auth, timeout and
  error mapping as `/v1/graph`; same `{apiVersion, clusters, elements}`
  Cytoscape body shape.
  - **Input is `az`, `env`, and a root list — searchable from EITHER end.**
    `az` and `env` are **REQUIRED and single-valued** (400 when absent or
    repeated — unlike `/v1/graph`, where they are optional and repeatable):
    they pin the zone whose Harvest store answers (`az` is the Harvest
    routing key; Harvest carries no `env`) and narrow the claim / pod / node
    side to one estate, so a filer shared across zones or environments is
    never merged into one diagram. Roots come from either side of the flow:
    - **storage roots** — an ONTAP cluster, controller node, aggregate or
      SVM → the answer is every pod (and its PVC / node) whose traffic lands
      on that component: "what is on this filer?";
    - **workload roots** — a `namespace` + pod name (pod names are unique
      per namespace within a cluster; `cluster` disambiguates when the
      zone holds several) → the answer is the storage chain under that pod:
      "which NetApp node does this pod use?"; or a Kubernetes node → every
      pod scheduled on it and their storage chains;
    - **`node=<name>`** is one parameter matched against BOTH the NetApp
      controller name and the Kubernetes node name — a hit on either tier
      makes that node a root on its own side (both, if the names collide),
      so the operator searches a node without knowing which kind it is.
    Both kinds are repeatable and may be mixed; the parameter shape (one
    typed parameter per kind, or one `<kind>:<name>` list) and the mixed
    semantics (design default: paths must touch a root on EACH side that
    has any root given — intersection, not union) are finalised in design.
    An empty root list returns the whole storage estate the selected zone's
    claims reach. `cluster` / `namespace` remain available as further
    narrowing. The response orientation is always storage → workload,
    whichever end was searched from.
  - **Roots always show.** A selected component is materialised even when
    nothing flows through it — a degraded aggregate with no claim, or a pod
    mounting no NetApp-backed claim, is a valid, non-empty answer, unlike
    `/v1/graph`'s join-only materialisation. "Exists" means the upstream
    names it in the window: for a storage root at least one Harvest series
    (`volume_labels`, `node_labels`, `aggr_*`, `node_new_status`); for a
    workload root a `kube_pod_info` series in the selected zone. A root NO
    series names (a typo, a decommissioned filer, a deleted pod) is **not
    drawn** — the body is simply empty of it, the same faithful-emptiness
    rule as `/v1/graph`.
  - **Fixed tier chain**: `netapp-node → netapp-aggr → netapp-svm → pvc →
    pod → node` (Kubernetes node). Every joined claim owns exactly one
    `(node, aggr, svm)` triple, so each claim is one linear path; edges are
    oriented storage → workload so the body is consumable by a Sankey layout
    without client-side reversal.
  - **Every edge carries a flow weight.** Each tier edge's `data.metrics`
    holds the claim I/O summed over every claim flowing through it
    (`read_ops`, `write_ops`, `read_bytes_per_sec`, `write_bytes_per_sec`;
    latencies and the declared ceiling ride the claim-level edge only), so
    the totals conserve tier to tier and no link has zero width. Summation
    is ascending-order and 6-significant-digit rounded, as for RED metrics.
    A claim mounted by several pods (RWX) has no observable per-pod share:
    its weight is **split equally** across the mounting pods so the pod and
    node tiers still conserve, and the split edges are marked so a consumer
    can tell an attributed share from a measurement.
  - **Reachability projection**, not the connectivity prune: a node survives
    iff it lies on a complete `netapp-node → … → pod` path that passes
    through a root. An **unmounted claim is dropped** (no pod, no flow, no
    Sankey path) and so is any aggregate / SVM that reaches only unmounted
    claims; a pod whose claims are not NetApp-backed is dropped — except a
    root on either side, which always shows.
  - **Entity attributes and grouping unchanged**: NetApp health / usage,
    PVC usage, storageclass, K8s node `ipaddress` / `ready_status` / zone
    labels, pod owner / application / containers all appear as in
    `/v1/graph`, and so do the synthesised compound groups (`cluster >
    namespace > application > controller` over pods, `cluster > namespace >
    [application >] pvc`, `storage-cluster` over the NetApp tiers) via
    `data.parent`. **Namespace and ArgoCD Application are NOT Sankey tiers
    in the body**: they are optional, orthogonal groupings of the pod / PVC
    tier (a pod without an Application would make the chain ragged, and
    `pod → node` is a physical hop while `pod → application` is a logical
    one), so the front end derives an Application- or namespace-level
    Sankey by walking `parent` and summing the conserved weights — the same
    body serves every grouping choice.
- **New `pkg/` surface for embedders.** A storage-request parser beside
  `kubegraph.ParseValues`, a storage-flow projection entry point in
  `pkg/graph`, and `Engine.BuildStorageFromValues` so an in-process consumer
  obtains the exact `/v1/storage-graph` body with no HTTP hop — same
  contract-sharing rule as `/v1/graph`.
- **New `netapp-svm` node type** (`netapp/<ontap-cluster>/svm/<svm>`,
  `labels={ontap_cluster}`), resolved from the hop-A `volume_labels` join
  (today surfaced only as the PVC's `svm` label). Emitted by
  `/v1/storage-graph` only; `/v1/graph` is unchanged.
- **One `storage-flow` edge type** registered in `graph.EdgeTypes`
  (`may_cross_cluster: false`) with `labels.tier` naming the hop, so
  `/v1/edge-types` lists it and `?edge_type=` validates against it.
  `/v1/graph` never emits it.
- **NetApp controller hardware attribute.** A new OPTIONAL Harvest leg reads
  `node_labels` (info series; labels `cluster`, `node`, `model`, `serial`,
  `version`, `vendor`, `location`) and the `netapp-node` gains a typed,
  nullable attribute (working name `data.hardware`, at minimum `model`;
  field set finalised in design) — never inside `labels`, per the
  `ipaddress` / `owner` / `ready_status` precedent. Resolved at build time
  onto the graph, so it appears on **both** endpoints and every `pkg/`
  consumer. Absence degrades to an omitted attribute; the leg never fails a
  build.
- **NetApp controller performance attribute — raw figures, no derived
  health.** A second OPTIONAL Harvest leg set reads the `system_node`
  counters `node_cpu_busy` (percent), `node_total_ops`, `node_total_latency`
  (µs) and `node_total_data` (bytes/s), matched on `(ontap-cluster, node)`,
  and the `netapp-node` gains a typed, nullable `data.perf = {cpu_busy_pct,
  total_ops, total_latency_us, total_bytes_per_sec}` — each field independently
  optional, values verbatim (no `rate()`), on both endpoints. The backend
  deliberately does NOT turn these into a health verdict: thresholds are
  model- and estate-specific (an A400 at 70 % CPU is idle, a FAS2720 is
  not), latency is as often the workload's fault as the controller's, and
  `data.health` keeps its precise meaning — the ONTAP-reported
  `node_new_status`, where absence ≠ degraded. Threshold judgement belongs
  in the operator's alert rules, which reach the node through the alert
  overlay below.
- **Alert overlay from the `ALERTS` series.** A new OPTIONAL leg reads the
  Prometheus / vmalert `ALERTS` series over the window and attaches active
  alerts to the graph node their labels identify: `{cluster, namespace,
  pod}` → `type="pod"`, `{cluster, node}` → `type="node"`, `{cluster,
  namespace, persistentvolumeclaim}` → `type="pvc"`, and — so that
  operator-authored rules over the Harvest counters (`node_cpu_busy > 85`,
  `node_read_latency > 5000`, `aggr_space_used / aggr_space_total > 0.9`)
  land on the storage tiers — `{cluster, node}` → `type="netapp-node"` and
  `{cluster, aggr}` → `type="netapp-aggr"` when `cluster` names an ONTAP
  cluster the Harvest join knows. The `{cluster, node}` shape is shared by
  the Kubernetes and NetApp node kinds: an alert whose `cluster` resolves
  to a Kubernetes identity matches the K8s node, one whose `cluster` is a
  known ONTAP cluster matches the controller, and one satisfying both is
  counted ambiguous and matched to neither (label-set matching only — a
  Kubernetes `cluster` walks the identity ladder like every other series).
  Each matched node gains a typed, nullable `data.alerts` list (`[{name,
  state, severity}]`, sorted for determinism, omitted when empty) — never
  inside `labels`. This is how a NetApp controller's health judgement
  reaches the diagram: raw counters in `data.perf`, thresholds in alert
  rules, verdicts in `data.alerts`. Which `alertstate` values count
  (`firing` only vs. `pending` too) and the "active in window" rule are
  design decisions. Unmatched alerts (no such node, or a label set naming
  none of the three kinds) are counted and ignored. Alerts — like the
  hardware attribute — are resolved **at build time onto the graph**, not
  by the storage projection, so they reach EVERY consumer of a build:
  `GET /v1/graph`, `GET /v1/storage-graph`, `Engine.Build` /
  `Engine.BuildFromValues` / `Engine.BuildStorageFromValues`, and any
  embedder walking `graph.Graph` through `GraphNode.Alerts()`. The leg runs
  on every build (an unfiltered `/v1/graph` reads the whole estate's
  alerts), degrades log-and-continue, and absence of the series is silent.
- **New `alerts` query family in the data-source (backends) YAML.** The
  routing table's `families` set gains `alerts`, so the operator declares
  which store holds `ALERTS` (typically the vmalert-fed store, which need
  not be the one holding `kube_*`). It is the first **optional** family:
  a table serving it on no backend is valid — the leg is skipped and no
  node carries `data.alerts` — because requiring it would invalidate every
  existing backends file (the five-family coverage rule stays exhaustive
  for the five). The implicit single-backend table (`--prom-url`, no file)
  serves it. `ALERTS` takes `az`, `env` and `namespace` as request
  matchers (`az` also routes it) but **NOT `cluster`** — an alert
  expression does not reliably keep the `cluster` label, so a `?cluster=`
  request must not silently drop alerts. Matching therefore uses the
  alert's `cluster` label through the identity ladder when it is present,
  and otherwise matches on the remaining labels (`namespace` + `pod`,
  `node`, `namespace` + `persistentvolumeclaim`) against the loaded
  estate, resolving only when exactly one node matches (ambiguous → counted
  as unmatched). `cluster` is re-applied at projection like every other
  attribute-carrying node. Operator precondition: the alerting store MUST
  stamp the same `az` / `env` external labels as the KSM store, or its
  alerts vanish under the filter (documented, and covered by the existing
  `selector_family_empty` Warn).
- **OpenAPI / docs** regenerated for the new route and attributes;
  `docs/netapp-harvest-preconditions.md` gains the `node_labels` leg;
  `docs/upstream-backend-routing.md` gains the `alerts` family.

No breaking change: `/v1/graph` gains three additive, omitted-when-absent
attributes (`data.hardware`, `data.perf`, `data.alerts`); existing ids, edge ids and
every existing field are byte-unchanged; existing backends files stay valid.

## Capabilities

### New Capabilities

- `storage-graph-api`: the `GET /v1/storage-graph` endpoint — required
  single-valued `az` / `env`, storage-side and workload-side root selectors
  (including the dual-tier `node=` search) and their mixed semantics, root-always-materialised rule (when Harvest knows it), the fixed tier chain
  and edge orientation, the single `storage-flow` edge type, per-edge
  flow-weight semantics (conservation, equal RWX split), mounted-only
  reachability projection, compound-group / namespace / Application
  carry-over for front-end grouping, response shape, determinism, timeout / auth /
  error mapping inherited from `/v1/graph`, and the `pkg/` embedder surface
  that shares its parser.
- `alert-overlay`: consumption of the `ALERTS` series — the optional leg,
  its `az` / `env` / `namespace` (no `cluster`) matcher set, the
  active-in-window rule, label-set matching to pod / K8s node / PVC /
  NetApp node / NetApp aggregate nodes (cluster-aware when the label is
  present, unique-in-estate otherwise; K8s-vs-ONTAP disambiguation of the
  `{cluster, node}` shape), the sorted `data.alerts` attribute,
  unmatched-alert observability, and per-family degradation.

### Modified Capabilities

- `netapp-storage-graph`: (1) new **NetApp SVM entity** requirement
  (`netapp-svm` node identity, labels, resolution from the hop-A join);
  (2) the **NetApp node entity** requirement gains the `node_labels`-sourced
  hardware attribute and the `system_node`-counter `perf` attribute, and
  states that `health` stays the reported status — never derived from
  them; (3) the Harvest-leg list and join-coverage
  observability acknowledge the new leg.
- `graph-api`: (1) **Edge-type discovery endpoint** lists `storage-flow`; (2) **Versioned route prefix** / **Route ↔ spec drift guard**
  / OpenAPI cover `/v1/storage-graph`; (3) **Cytoscape compound node
  grouping** parents `netapp-svm` under `storage-cluster/<ontap-cluster>`
  (for the storage-graph body; `/v1/graph` emits no SVM); (4) the
  **Cytoscape.js response shape** — on BOTH endpoints — gains the
  `data.alerts` (pod / node / PVC / netapp-node / netapp-aggr),
  `data.hardware` and `data.perf` (`netapp-node`) node attributes, with a
  `/v1/graph` golden pinning each.
- `upstream-backend-routing`: the family set grows to six with `alerts`
  as the first optional family — table validation, the implicit
  single-backend table, `Family.AcceptsAZ()` and the `queryFamily`
  exhaustiveness rule all acknowledge it.

## Impact

- `pkg/graph`: `NodeTypeNetAppSVM`, `NetAppSVMNode`, `Hardware()` and
  `Perf()` accessors on the sealed `GraphNode` (nil for all but `NetAppNode`)
  and an `Alerts()` accessor (non-nil only on pod / K8s node / PVC /
  NetApp node / NetApp aggregate), the `storage-flow` entry in
  `EdgeTypes`, a storage `Scope` / projection.
- `pkg/build`: `netapp.go` resolves the SVM entity and reads `node_labels`;
  a storage-flow assembler builds the tier chain + summed weights from the
  existing per-claim I/O; `ReadTopology` fan-out grows by one optional
  Harvest leg (the 31 / 37 query-count pins move). Root-always requires the
  builder to materialise selected storage roots absent a join (workload
  roots are ordinary topology pods, kept by the projection).
- `pkg/promql`: `QNetAppNodeLabels` + four `QNetAppNode*` perf counters in
  the `harvest` family and `QAlerts`
  in the new `alerts` family (`queryDims` / `queryFamily` exhaustiveness
  tests), `Family` set + table validation (optional family), render
  baseline; `pkg/promql/backendsfile` accepts `alerts`.
- `pkg/build`: `resolveAlerts` in `topology.go` and the node perf read in
  `netapp.go` (six more optional legs in the `ReadTopology` fan-out
  overall — `node_labels`, four counters, `ALERTS`; query-count pins
  move).
- `pkg/cytoscape`: hardware, perf and alerts DTOs; `netapp-svm` parent rule;
  storage-graph goldens.
- `pkg/kubegraph`: storage-request parser + `BuildStorageFromValues`.
- `internal/api`: `handleStorageGraph`, swag annotations, `docs/`
  regenerated, route ↔ spec drift test, new goldens; the existing
  `with-netapp-storage-cytoscape.json` golden gains only `data.hardware`
  when the fixture supplies `node_labels`.
- `internal/integration`: `TestPVCNetAppHarvestJoin` fixture gains
  `node_labels`; new storage-graph end-to-end test (Sankey shape:
  conservation across tiers).
- Docs: `docs/netapp-harvest-preconditions.md`, `docs/upstream-metrics.md`
  (the `ALERTS` contract and its label preconditions),
  `docs/upstream-backend-routing.md`,
  `CLAUDE.md` (request surface, edge-type list, NetApp bullet).
- Downstream (out of this repo): the demo's `netapp-faker` should emit
  `node_labels`; the frontend's Sankey consumes this body. Neither blocks
  this change.
- No new dependencies.
